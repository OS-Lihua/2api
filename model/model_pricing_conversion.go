package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/pluginhost"
	"github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	hostreasoning "github.com/QuantumNous/new-api/setting/reasoning"
	"github.com/QuantumNous/new-api/types"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ModelPricingConversion describes an offline conversion preview. Persisting the
// result requires an immutable PriceVersion through the migration transaction.
type ModelPricingConversion struct {
	ModelPricingDescription
	Expression        string `json:"expression,omitempty"`
	UnsupportedReason string `json:"unsupported_reason,omitempty"`
}

// ModelPricingDescription is shared by previews, conversions and snapshot
// entries. It describes effective prices without adding persisted settings.
type ModelPricingDescription struct {
	Effective      PricingValues        `json:"effective,omitempty"`
	CacheWriteMode CacheWriteMode       `json:"cache_write_mode,omitempty"`
	BillingDetails LegacyBillingDetails `json:"billing_details"`
}

func legacyInputPricePerMillion(ratio float64) (decimal.Decimal, error) {
	if common.QuotaPerUnit <= 0 || math.IsInf(common.QuotaPerUnit, 0) || math.IsNaN(common.QuotaPerUnit) {
		return decimal.Zero, errors.New("invalid quota unit")
	}
	if ratio < 0 || math.IsInf(ratio, 0) || math.IsNaN(ratio) {
		return decimal.Zero, errors.New("input ratio must be finite and non-negative")
	}
	return decimal.NewFromFloat(ratio).Mul(decimal.NewFromInt(1_000_000)).Div(decimal.NewFromFloat(common.QuotaPerUnit)), nil
}

type LegacyPricingRule struct {
	Condition  string  `json:"condition"`
	Multiplier float64 `json:"multiplier"`
}

// LegacyBillingDetails is display-only. Absolute audio prices cannot always be
// represented by a ratio: Gemini audio can be billable with a zero text price.
type LegacyBillingDetails struct {
	AudioInputPrice        *float64            `json:"audio_input_price,omitempty"`
	AudioOutputPrice       *float64            `json:"audio_output_price,omitempty"`
	ImageCount             bool                `json:"image_count,omitempty"`
	RequestRules           []LegacyPricingRule `json:"request_rules,omitempty"`
	AudioTextBranches      bool                `json:"audio_text_branches,omitempty"`
	InvalidAudioPrice      bool                `json:"-"`
	ConflictingAudioPrices bool                `json:"-"`
}

func ResolveLegacyBillingDetails(name string, effective, configured PricingValues) LegacyBillingDetails {
	details := LegacyBillingDetails{}
	if effective["billing_setting.billing_mode"] == billing_setting.BillingModeTieredExpr {
		return details
	}
	if _, fixed := effective["ModelPrice"]; fixed {
		details.ImageCount = common.IsImageGenerationModel(name)
		details.RequestRules = legacyDallePricingRules(name)
		return details
	}
	ratio, priced := effective["ModelRatio"].(float64)
	if !priced {
		return details
	}
	base, err := legacyInputPricePerMillion(ratio)
	if err != nil {
		return details
	}
	if price := operation_setting.GetGeminiInputAudioPricePerMillionTokens(name); price > 0 {
		details.AudioInputPrice = &price
		// Native Gemini settles with its dedicated input price; compatible
		// chat enters the audio-ratio path when those ratios are configured.
		// Do not silently replace one price with the other during migration.
		audio, hasInput := effective["AudioRatio"].(float64)
		output, hasOutput := effective["AudioCompletionRatio"].(float64)
		if hasInput || hasOutput {
			if !hasInput {
				audio = 1
			}
			if !hasOutput {
				output = 1
			}
			audioPrice := base.Mul(decimal.NewFromFloat(audio))
			textOutput := base.Mul(decimal.NewFromFloat(effective["CompletionRatio"].(float64)))
			details.ConflictingAudioPrices = !audioPrice.Equal(decimal.NewFromFloat(price)) || !audioPrice.Mul(decimal.NewFromFloat(output)).Equal(textOutput)
		}
		return details
	}
	audioRatio, hasAudio := effective["AudioRatio"].(float64)
	audioCompletionRatio, hasAudioCompletion := effective["AudioCompletionRatio"].(float64)
	if !hasAudio && !hasAudioCompletion {
		return details
	}
	if !hasAudio {
		audioRatio = 1
	}
	if !hasAudioCompletion {
		audioCompletionRatio = 1
	}
	input := base.Mul(decimal.NewFromFloat(audioRatio))
	inPrice, outPrice := input.InexactFloat64(), input.Mul(decimal.NewFromFloat(audioCompletionRatio)).InexactFloat64()
	if math.IsInf(inPrice, 0) || math.IsInf(outPrice, 0) {
		details.InvalidAudioPrice = true
		return details
	}
	details.AudioInputPrice, details.AudioOutputPrice = &inPrice, &outPrice
	details.AudioTextBranches = effective["CacheRatio"] != float64(1) || effective["ImageRatio"] != float64(1) || ResolveCacheWriteMode(name, configured) != CacheWriteNone
	return details
}

type CacheWriteMode string

const (
	CacheWriteNone      CacheWriteMode = "none"
	CacheWriteStandard  CacheWriteMode = "standard"
	CacheWriteClaudeTTL CacheWriteMode = "claude_ttl"
	CacheWriteOpenAITTL CacheWriteMode = "openai_ttl"
)

// ResolveCacheWriteMode describes display and migration only. A generic engine
// fallback is not configured cache-write pricing. Only Claude names inherit the
// legacy dual-TTL rule; a configured write price on a GPT name also prices the
// OpenAI 12h write, and other models retain their configured write price.
func ResolveCacheWriteMode(name string, configured PricingValues) CacheWriteMode {
	if strings.Contains(strings.ToLower(name), "claude") {
		return CacheWriteClaudeTTL
	}
	if _, exists := configured["CreateCacheRatio"]; exists {
		if strings.Contains(strings.ToLower(name), "gpt") {
			return CacheWriteOpenAITTL
		}
		return CacheWriteStandard
	}
	return CacheWriteNone
}

// declaredEndpointNames resolves the endpoint types declared by one model
// metadata row. Model metadata accepts two shapes — the map form (custom paths)
// and the type-array form (declared protocols) — and conversion must honour
// both. Reading only the map form made array-form metadata look like a broken
// routing configuration and blocked an otherwise convertible model.
func declaredEndpointNames(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var value any
	if err := common.UnmarshalJsonStr(raw, &value); err != nil {
		return nil, err
	}
	// Normalize whitespace here: the callers match these names against the
	// endpoint switch, so a padded value such as "image-generation " would
	// silently fall through and drop the image-count settlement rule.
	switch endpoints := value.(type) {
	case []any:
		names := make([]string, 0, len(endpoints))
		for _, endpoint := range endpoints {
			text, ok := endpoint.(string)
			if !ok {
				return nil, errors.New("invalid endpoint type")
			}
			text = strings.TrimSpace(text)
			if text == "" {
				return nil, errors.New("invalid endpoint type")
			}
			names = append(names, text)
		}
		return names, nil
	case map[string]any:
		names := make([]string, 0, len(endpoints))
		for endpoint := range endpoints {
			endpoint = strings.TrimSpace(endpoint)
			if endpoint == "" {
				continue
			}
			names = append(names, endpoint)
		}
		return names, nil
	}
	return nil, errors.New("invalid endpoints")
}

// LegacyPricingConversionSource is an offline-only snapshot. Its private fields
// cannot be replaced by callers with an empty candidate set. It contains no
// credentials, request bodies, headers, URLs, or complete Provider settings.
// The caller owns the transaction/source snapshot; no conversion reads DB or
// rebuilds/publishes the serving process's Ability or task-alias caches.
type LegacyPricingConversionSource struct {
	providers  []legacyPricingProvider
	abilities  []legacyPricingAbility
	metadata   []Model
	pricing    map[string]map[string]any
	generation *jsplugin.RoutingGeneration
	aliases    map[string]TaskAliasTarget
	// Known declarations retain disabled/replaced identities for refusal only;
	// they never authorize a Channel or provide a price validation schema.
	declarations []*jsplugin.LoadedPlugin
	selfUse      bool
}

type legacyPricingProvider struct {
	ID                   int
	Type                 int
	Status               int
	Models               string
	ModelMapping         *string
	TaskPluginKey        string
	TaskExtendPluginKeys string
}

type legacyPricingAbility struct {
	Model     string
	ChannelID int
	Enabled   bool
}

// loadLegacyMigrationPricingSource reproduces the source deployment's plugin
// layer on an isolated registry. No runtime incumbent can stand in for source
// rows: unreadable, ambiguous, invalid or conflicting active sources fail closed.
func loadLegacyMigrationPricingSource(tx *gorm.DB) (*LegacyPricingConversionSource, error) {
	tx = tx.Session(&gorm.Session{Logger: logger.Discard})
	registry, err := plugins.NewFactoryRegistry()
	if err != nil {
		return nil, errors.New("legacy factory plugin declarations are invalid")
	}
	declarations := registry.Generation().Plugins()
	var options []Option
	if err := tx.Where(`"key" IN ?`, []string{"TaskPluginEnabled", setting.TaskPluginDisabledFactoryKeysKey, "SelfUseModeEnabled"}).Find(&options).Error; err != nil {
		return nil, err
	}
	enabled, selfUse := true, false
	var disabled []string
	seenOptions := map[string]bool{}
	for _, option := range options {
		if seenOptions[option.Key] {
			return nil, errors.New("ambiguous legacy plugin options")
		}
		seenOptions[option.Key] = true
		switch option.Key {
		case "TaskPluginEnabled":
			enabled, err = strconv.ParseBool(option.Value)
		case "SelfUseModeEnabled":
			selfUse, err = strconv.ParseBool(option.Value)
		case setting.TaskPluginDisabledFactoryKeysKey:
			err = common.UnmarshalJsonStr(option.Value, &disabled)
		}
		if err != nil {
			return nil, errors.New("invalid legacy plugin options")
		}
	}
	registry.SetDisabledFactoryKeys(disabled)
	var rows []TaskPlugin
	if err := tx.Select("key", "api_version", "version", "source", "source_hash", "enabled", "active").Where("active = ?", true).Order("key, version, id").Find(&rows).Error; err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var overrides []*jsplugin.LoadedPlugin
	for _, row := range rows {
		if seen[row.Key] {
			return nil, errors.New("ambiguous active legacy task plugin versions")
		}
		seen[row.Key] = true
		// Even a disabled active source retains its declared task identities
		// for refusal: they must not silently turn into per-million text prices.
		hash := sha256.Sum256([]byte(row.Source))
		if hex.EncodeToString(hash[:]) != row.SourceHash {
			return nil, errors.New("legacy task plugin source hash mismatch")
		}
		compiled, err := jsplugin.CompilePlugin(string(row.Source), jsplugin.Options{Key: row.Key, Version: row.Version})
		if err != nil || compiled.Meta.APIVersion != row.APIVersion || compiled.Meta.Key != row.Key || compiled.Meta.Version != row.Version {
			return nil, errors.New("invalid active legacy task plugin source")
		}
		declarations = append(declarations, compiled)
		// Normal publication excludes a disabled override; an independently
		// enabled factory can still supply this key.
		if row.Enabled {
			overrides = append(overrides, compiled)
		}
	}
	if err := registry.ReplaceOverrides(overrides); err != nil || len(registry.RoutingErrors()) != 0 || registry.LastRebuildError() != "" {
		return nil, errors.New("conflicting legacy task plugin declarations")
	}
	// The same host policy used by the live router must admit this isolated
	// source generation before it can define Channels or task price units.
	// There is no runtime incumbent, middleware, service, or global publication.
	hostPolicy := pluginhost.New(pluginhost.HostRoutes())
	if err := registry.SetGenerationPreparer(hostPolicy.PrepareOffline); err != nil || len(registry.RoutingErrors()) != 0 || registry.LastRebuildError() != "" {
		return nil, errors.New("legacy task plugin host admission failed")
	}
	registry.SetEnabled(enabled)
	source, err := LoadLegacyPricingConversionSource(tx, registry.Generation())
	if err != nil {
		return nil, err
	}
	source.declarations, source.selfUse = declarations, selfUse
	return source, nil
}

// LoadLegacyPricingConversionSource reads every legacy routing row, including
// disabled abilities. Enabled was not a filter in the historical safety check.
// A missing table/read failure is an error, never an empty-source fallback.
func LoadLegacyPricingConversionSource(tx *gorm.DB, generation *jsplugin.RoutingGeneration) (*LegacyPricingConversionSource, error) {
	if tx == nil || generation == nil {
		return nil, errors.New("legacy pricing conversion source is required")
	}
	source := &LegacyPricingConversionSource{generation: generation, aliases: map[string]TaskAliasTarget{}}
	// Invalid settings must fail without logging their potentially secret values.
	tx = tx.Session(&gorm.Session{Logger: logger.Discard})
	if err := tx.Table(providerTable).Select(`id, type, status, models, model_mapping,
		COALESCE(NULLIF(setting, '')::jsonb->>'task_plugin_key', '') AS task_plugin_key,
		COALESCE(NULLIF(setting, '')::jsonb->'task_extend_plugin_keys', '[]'::jsonb)::text AS task_extend_plugin_keys`).Order("id").Scan(&source.providers).Error; err != nil {
		return nil, err
	}
	if err := tx.Model(&Ability{}).Select("model", "channel_id", "enabled").Order("channel_id, model").Scan(&source.abilities).Error; err != nil {
		return nil, err
	}
	if err := tx.Model(&Model{}).Select("model_name", "name_rule", "endpoints").Where("endpoints <> ?", "").Find(&source.metadata).Error; err != nil {
		return nil, err
	}
	var err error
	var duplicated []string
	if source.pricing, _, duplicated, err = readModelPricingMaps(tx); err != nil {
		return nil, err
	}
	if len(duplicated) != 0 {
		return nil, errors.New("ambiguous legacy pricing options")
	}
	// Reproduce the legacy alias classification from the captured Provider rows
	// and immutable plugin declaration generation, not a process-global view.
	type aliasDraft struct {
		spellings []string
		byPlugin  map[string]map[string]struct{}
	}
	drafts := map[string]*aliasDraft{}
	for _, provider := range source.providers {
		if provider.Status != common.ChannelStatusEnabled || provider.ModelMapping == nil || *provider.ModelMapping == "" {
			continue
		}
		var mapping map[string]string
		if err := common.UnmarshalJsonStr(*provider.ModelMapping, &mapping); err != nil {
			continue // The per-model check below reports an unverifiable route.
		}
		for _, name := range strings.Split(provider.Models, ",") {
			if mapping[name] == "" {
				continue
			}
			if _, declared := generation.CanonicalModel(name); declared {
				continue
			}
			tail, cycle := followChannelModelMapping(mapping, name)
			if cycle {
				continue
			}
			declared, ok := generation.CanonicalModel(tail)
			if !ok {
				continue
			}
			plugin, ok := generation.GetByModel(declared)
			if !ok {
				continue
			}
			fold := jsplugin.ASCIIFold(name)
			if drafts[fold] == nil {
				drafts[fold] = &aliasDraft{byPlugin: map[string]map[string]struct{}{}}
			}
			draft := drafts[fold]
			draft.spellings = append(draft.spellings, name)
			if draft.byPlugin[plugin.Meta.Key] == nil {
				draft.byPlugin[plugin.Meta.Key] = map[string]struct{}{}
			}
			draft.byPlugin[plugin.Meta.Key][declared] = struct{}{}
		}
	}
	for fold, draft := range drafts {
		if len(draft.byPlugin) != 1 {
			continue
		}
		for key, targets := range draft.byPlugin {
			target := TaskAliasTarget{Alias: slices.Min(draft.spellings), PluginKey: key}
			if len(targets) == 1 {
				for declared := range targets {
					target.Declared = declared
				}
			}
			source.aliases[fold] = target
		}
	}
	return source, nil
}

// saleExpression preserves the executing plugin's historical override priority.
// One Offering can have only one immutable sales expression/unit contract: when
// legacy routes disagree, leave it disabled rather than choose an arbitrary one.
func (source *LegacyPricingConversionSource) saleExpression(name string, routes []legacyGatewayChannel) string {
	modelExpression := func(model string) (string, bool) {
		effective := effectiveModelPricingWithSelfUse(source.pricing, model, source.selfUse)
		if effective["billing_setting.billing_mode"] != billing_setting.BillingModeTieredExpr {
			return "", false
		}
		expression, _ := effective["billing_setting.billing_expr"].(string)
		return expression, true
	}
	var selected string
	var selectedTask, found bool
	for _, route := range routes {
		if route.name != name {
			continue
		}
		key, task := route.protocol.TaskPluginKey()
		expression := ""
		if task {
			value, exists := source.pricing[billing_setting.PluginBillingExprOption][billing_setting.PluginBillingExprKey(key, name)]
			if !exists && route.upstream != name {
				value, exists = source.pricing[billing_setting.PluginBillingExprOption][billing_setting.PluginBillingExprKey(key, route.upstream)]
			}
			if exists {
				expression, _ = value.(string)
			} else if client, configured := modelExpression(name); configured {
				expression = client
			} else if route.upstream != name {
				expression, _ = modelExpression(route.upstream)
			}
		} else if configured, exists := modelExpression(name); exists {
			expression = configured
		} else {
			converted, err := source.Preview(name, modelPricingValues(source.pricing, name))
			if err == nil && converted != nil && converted.UnsupportedReason == "" {
				expression = converted.Expression
			}
		}
		if strings.TrimSpace(expression) == "" || found && (selected != expression || selectedTask != task) {
			return ""
		}
		selected, selectedTask, found = expression, task, true
	}
	return selected
}

func (source *LegacyPricingConversionSource) resolveAlias(name string) (TaskAliasTarget, bool) {
	target, ok := source.aliases[jsplugin.ASCIIFold(name)]
	return target, ok
}

func (source *LegacyPricingConversionSource) Preview(name string, draft PricingValues) (*ModelPricingConversion, error) {
	if source == nil || source.generation == nil || source.pricing == nil {
		return nil, errors.New("legacy pricing conversion source is required")
	}
	if draft == nil {
		return nil, errors.New("pricing draft is required")
	}
	if err := validateModelPricingWithAliases(name, draft, modelPricingValues(source.pricing, name), source.generation, source.resolveAlias); err != nil {
		return nil, err
	}
	if draft["billing_setting.billing_mode"] == billing_setting.BillingModeTieredExpr {
		return &ModelPricingConversion{UnsupportedReason: "This model already uses an expression."}, nil
	}
	// The draft is the complete editable configuration: omitted fields inherit
	// relay defaults rather than silently retaining discarded saved values.
	legacyDraft := make(PricingValues, len(draft)+1)
	maps.Copy(legacyDraft, draft)
	legacyDraft["billing_setting.billing_mode"] = billing_setting.BillingModeRatio
	values := make(map[string]map[string]any, len(source.pricing))
	for key, entries := range source.pricing {
		values[key] = maps.Clone(entries)
	}
	replaceModelPricing(values, name, legacyDraft)
	effective := effectiveModelPricingWithSelfUse(values, name, source.selfUse)
	if err := validateModelPricingWithAliases(name, effective, modelPricingValues(source.pricing, name), source.generation, source.resolveAlias); err != nil {
		return nil, err
	}
	_, fixedPrice := effective["ModelPrice"]
	preview := &ModelPricingConversion{ModelPricingDescription: ModelPricingDescription{Effective: effective, BillingDetails: ResolveLegacyBillingDetails(name, effective, draft)}}
	if preview.BillingDetails.InvalidAudioPrice {
		return nil, errors.New("audio prices must be finite")
	}
	if preview.BillingDetails.ConflictingAudioPrices {
		return &ModelPricingConversion{UnsupportedReason: "Gemini and OpenAI audio prices differ for this model. Use separate billing model names to convert them."}, nil
	}

	generation := source.generation
	if _, task := generation.GetByModel(name); task {
		return &ModelPricingConversion{UnsupportedReason: "Task pricing must be converted manually using the task usage schema."}, nil
	}
	if _, task := source.resolveAlias(name); task {
		return &ModelPricingConversion{UnsupportedReason: "Task pricing must be converted manually using the task usage schema."}, nil
	}

	// Inspect non-secret routing metadata so aliases cannot disguise a special
	// settlement path as an ordinary text model.
	candidates := map[int]bool{}
	for _, ability := range source.abilities {
		if ability.Model == name {
			candidates[ability.ChannelID] = true
		}
	}
	names := []string{name}
	for _, channel := range source.providers {
		if !candidates[channel.ID] || !slices.Contains(strings.Split(channel.Models, ","), name) {
			continue
		}
		if channel.Type == constant.ChannelTypeTaskPlugin {
			return &ModelPricingConversion{UnsupportedReason: "Task pricing must be converted manually using the task usage schema."}, nil
		}
		if slices.Contains(common.GetEndpointTypesByChannelType(channel.Type, name), constant.EndpointTypeOpenAIVideo) {
			return &ModelPricingConversion{UnsupportedReason: "Video pricing must be converted manually."}, nil
		}
		if channel.Status == common.ChannelStatusEnabled && channel.Type == constant.ChannelTypeOpenRouter && strings.Contains(strings.ToLower(name), "claude") && !fixedPrice {
			defaultRatio, hasDefault := ratio_setting.GetDefaultModelRatioMap()[name]
			if hasDefault && effective["ModelRatio"] == defaultRatio && effective["CreateCacheRatio"] != float64(1) {
				return &ModelPricingConversion{UnsupportedReason: "This OpenRouter Claude price derives cache-write usage from upstream cost and must be converted manually."}, nil
			}
		}
		upstream := name
		if channel.ModelMapping != nil && *channel.ModelMapping != "" {
			var mapping map[string]string
			if err := common.UnmarshalJsonStr(*channel.ModelMapping, &mapping); err != nil {
				return &ModelPricingConversion{UnsupportedReason: "The model routing configuration could not be verified."}, nil
			}
			// Relay also resolves reasoning-suffixed names at any mapping hop.
			candidates := []string{name}
			for _, target := range mapping {
				candidates = append(candidates, target)
			}
			for _, candidate := range candidates {
				if mapping[candidate] == "" {
					mapping[candidate] = mapping[hostreasoning.BaseModelName(candidate)]
				}
			}
			var cycle bool
			upstream, cycle = followChannelModelMapping(mapping, name)
			if cycle {
				return &ModelPricingConversion{UnsupportedReason: "The model routing configuration could not be verified."}, nil
			}
			if upstream != name {
				names = append(names, upstream)
			}
		}
		// Type-60 task extensions are bindings, not generic text routes. Resolve
		// only their declared names from this generation; an unavailable binding
		// cannot safely establish ordinary text settlement.
		if channel.Type == constant.ChannelTypeNewAPI {
			var bindings []string
			if err := common.UnmarshalJsonStr(channel.TaskExtendPluginKeys, &bindings); err != nil {
				return &ModelPricingConversion{UnsupportedReason: "The model routing configuration could not be verified."}, nil
			}
			if channel.TaskPluginKey != "" {
				bindings = append(bindings, channel.TaskPluginKey)
			}
			for _, key := range bindings {
				plugin, ok := generation.Get(key)
				if !ok {
					return &ModelPricingConversion{UnsupportedReason: "The model routing configuration could not be verified."}, nil
				}
				if slices.ContainsFunc(plugin.Meta.Models, func(declared string) bool {
					return jsplugin.ASCIIFold(declared) == jsplugin.ASCIIFold(upstream)
				}) {
					return &ModelPricingConversion{UnsupportedReason: "Task pricing must be converted manually using the task usage schema."}, nil
				}
			}
		}
	}
	for _, modelName := range names {
		lower := strings.ToLower(modelName)
		if strings.Contains(lower, "realtime") {
			return &ModelPricingConversion{UnsupportedReason: "Realtime pricing must be converted manually."}, nil
		}
		if fixedPrice && ResolveLegacyBillingDetails(modelName, effective, draft).ImageCount {
			preview.BillingDetails.ImageCount = true
		}
	}
	for _, entry := range source.metadata {
		if entry.Endpoints == "" || !slices.ContainsFunc(names, entry.MatchesName) {
			continue
		}
		endpoints, err := declaredEndpointNames(entry.Endpoints)
		if err != nil {
			return &ModelPricingConversion{UnsupportedReason: "The model routing configuration could not be verified."}, nil
		}
		for _, endpoint := range endpoints {
			switch constant.EndpointType(endpoint) {
			case constant.EndpointTypeImageGeneration:
				if fixedPrice {
					preview.BillingDetails.ImageCount = true
				}
			case constant.EndpointTypeOpenAIVideo:
				return &ModelPricingConversion{UnsupportedReason: "Video pricing must be converted manually."}, nil
			case constant.EndpointTypeOpenAI, constant.EndpointTypeOpenAIResponse, constant.EndpointTypeAnthropic, constant.EndpointTypeGemini, constant.EndpointTypeEmbeddings, constant.EndpointTypeJinaRerank:
				// These endpoints use the ordinary token/fixed-request settlement.
			}
		}
	}
	var expression string
	if fixedPrice {
		price := effective["ModelPrice"].(float64)
		expression = `tier("request", fixed(` + decimal.NewFromFloat(price).String() + `))`
		if preview.BillingDetails.ImageCount {
			expression = `tier("image", fixed(` + decimal.NewFromFloat(price).String() + `)) * image_count`
			for _, rule := range preview.BillingDetails.RequestRules {
				expression += ` * (` + rule.Condition + ` ? ` + decimal.NewFromFloat(rule.Multiplier).String() + ` : 1)`
			}
		}
	} else {
		ratio, exists := effective["ModelRatio"].(float64)
		if !exists {
			return &ModelPricingConversion{UnsupportedReason: "Configure an input price before converting this model."}, nil
		}
		preview.CacheWriteMode = ResolveCacheWriteMode(name, draft)
		base, err := legacyInputPricePerMillion(ratio)
		if err != nil {
			return nil, err
		}
		// Cache reads can overlap other separately priced input categories.
		// Keep cr in that case: clamping the input remainder makes folding it
		// into p non-equivalent. A zero base price is equivalent either way.
		ordinaryAudio := preview.BillingDetails.AudioOutputPrice != nil
		if ordinaryAudio && preview.CacheWriteMode != CacheWriteNone {
			preview.BillingDetails.AudioTextBranches = true
		}
		mergeCacheRead := base.IsZero() || preview.CacheWriteMode == CacheWriteNone && effective["ImageRatio"].(float64) == 1 && (preview.BillingDetails.AudioInputPrice == nil || ordinaryAudio)
		var expressionBody strings.Builder
		expressionBody.WriteString(`tier("base", p * `)
		expressionBody.WriteString(base.String())
		// Retain explicit zero coefficients for included categories. Claude
		// names inherit the relay's 1h / 5m factor and GPT names its 12h / 30m
		// factor.
		for _, lane := range []struct {
			variable, key string
			multiplier    float64
		}{
			{"c", "CompletionRatio", 1},
			{"cr", "CacheRatio", 1},
			{"cc", "CreateCacheRatio", 1},
			{"cc1h", "CreateCacheRatio", types.ClaudeCacheCreation1hMultiplier},
			{"cc12h", "CreateCacheRatio", types.OpenAICacheCreation12hMultiplier},
			{"img", "ImageRatio", 1},
		} {
			if ordinaryAudio && !preview.BillingDetails.AudioTextBranches && lane.variable != "c" {
				continue
			}
			if lane.variable == "cc" && preview.CacheWriteMode == CacheWriteNone ||
				lane.variable == "cc1h" && preview.CacheWriteMode != CacheWriteClaudeTTL ||
				lane.variable == "cc12h" && preview.CacheWriteMode != CacheWriteOpenAITTL {
				continue
			}
			multiplier := effective[lane.key].(float64)
			// Same-price input categories stay in p. Keep distinct prices,
			// including explicit zero, as independently priced terms.
			if (lane.variable == "img" || lane.variable == "cr" && mergeCacheRead) && multiplier == 1 {
				continue
			}
			expressionBody.WriteString(" + " + lane.variable + " * ")
			expressionBody.WriteString(base.Mul(decimal.NewFromFloat(multiplier)).Mul(decimal.NewFromFloat(lane.multiplier)).String())
		}
		for _, lane := range []struct {
			variable string
			price    *float64
		}{
			{"ai", preview.BillingDetails.AudioInputPrice},
			{"ao", preview.BillingDetails.AudioOutputPrice},
		} {
			if lane.price != nil && !preview.BillingDetails.AudioTextBranches {
				expressionBody.WriteString(" + " + lane.variable + " * " + decimal.NewFromFloat(*lane.price).String())
			}
		}
		expressionBody.WriteString(")")
		expression = expressionBody.String()
		if ordinaryAudio && preview.BillingDetails.AudioTextBranches {
			completion := base.Mul(decimal.NewFromFloat(effective["CompletionRatio"].(float64)))
			// The audio settlement ignores cache/image ratios. Use the full
			// input total here because variables in the text-only branch still
			// participate in AST-based token normalization.
			expression = `(ai > 0 || ao > 0) ? tier("audio", max(len - ai, 0) * ` + base.String() + ` + c * ` + completion.String() + ` + ai * ` + decimal.NewFromFloat(*preview.BillingDetails.AudioInputPrice).String() + ` + ao * ` + decimal.NewFromFloat(*preview.BillingDetails.AudioOutputPrice).String() + `) : ` + expression
		}
	}
	if err := billing_setting.SmokeTestExpr(expression); err != nil {
		return nil, err
	}
	preview.Expression = expression
	return preview, nil
}
