package model

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"gorm.io/gorm"
)

// PricingValues contains captured legacy migration input keyed by old option
// names. Missing keys inherit legacy defaults; explicit zero remains free.
type PricingValues map[string]any

// Keep the retired keys recognizable so live option writers reject them.
var modelPricingOptionKeys = []string{
	"AudioCompletionRatio", "AudioRatio", "CacheRatio", "CompletionRatio",
	"CreateCacheRatio", "ImageRatio", "ModelPrice", "ModelRatio",
	"billing_setting.billing_expr", "billing_setting.billing_mode", billing_setting.PluginBillingExprOption,
}

func IsModelPricingOption(key string) bool {
	return slices.Contains(modelPricingOptionKeys, key)
}

func defaultPricingMaps() map[string]map[string]any {
	result := make(map[string]map[string]any, len(modelPricingOptionKeys))
	for _, key := range modelPricingOptionKeys {
		result[key] = make(map[string]any)
	}
	for key, values := range ratio_setting.GetDefaultPricingMaps() {
		for name, value := range values {
			result[key][name] = value
		}
	}
	return result
}

func readModelPricingMaps(db *gorm.DB) (map[string]map[string]any, map[string]bool, []string, error) {
	var rows []Option
	if err := db.Where(`"key" IN ?`, modelPricingOptionKeys).Find(&rows).Error; err != nil {
		return nil, nil, nil, err
	}
	values := defaultPricingMaps()
	existing := make(map[string]bool)
	counts := make(map[string]int)
	for _, row := range rows {
		var entries map[string]any
		if err := common.UnmarshalJsonStr(row.Value, &entries); err != nil {
			return nil, nil, nil, fmt.Errorf("%s: %w", row.Key, err)
		}
		if entries == nil {
			return nil, nil, nil, fmt.Errorf("%s must be a JSON object", row.Key)
		}
		values[row.Key] = entries
		existing[row.Key] = true
		counts[row.Key]++
	}
	var duplicated []string
	for _, key := range modelPricingOptionKeys {
		if counts[key] > 1 {
			duplicated = append(duplicated, key)
		}
	}
	return values, existing, duplicated, nil
}

func modelPricingValues(values map[string]map[string]any, name string) PricingValues {
	result := make(PricingValues)
	for _, key := range modelPricingOptionKeys {
		if key == billing_setting.PluginBillingExprOption {
			variants := make(map[string]any)
			for variant, expression := range values[key] {
				if plugin, model, ok := billing_setting.SplitPluginBillingExprKey(variant); ok && model == name {
					variants[plugin] = expression
				}
			}
			if len(variants) > 0 {
				result[key] = variants
			}
			continue
		}
		if value, exists := values[key][name]; exists {
			result[key] = value
		}
	}
	return result
}

// replaceModelPricing replaces one offline draft in private in-memory maps.
// Plugin keys retain their original flat representation; no storage is written.
func replaceModelPricing(values map[string]map[string]any, name string, draft PricingValues) {
	for _, key := range modelPricingOptionKeys {
		if key == billing_setting.PluginBillingExprOption {
			for variant := range values[key] {
				if _, model, ok := billing_setting.SplitPluginBillingExprKey(variant); ok && model == name {
					delete(values[key], variant)
				}
			}
			variants, _ := draft[key].(map[string]any)
			for plugin, expr := range variants {
				values[key][billing_setting.PluginBillingExprKey(plugin, name)] = expr
			}
			continue
		}
		delete(values[key], name)
		if value, exists := draft[key]; exists {
			values[key][name] = value
		}
	}
}

func effectiveModelPricingWithSelfUse(values map[string]map[string]any, name string, selfUse bool) PricingValues {
	result := modelPricingValues(values, name)
	// Legacy wildcard aliases are resolved by the same normalization as relay.
	alias := ratio_setting.FormatMatchingModelName(name)
	for _, key := range []string{"ModelPrice", "ModelRatio", "CompletionRatio", "AudioRatio", "AudioCompletionRatio"} {
		delete(result, key)
		if value, exists := values[key][alias]; exists {
			result[key] = value
		}
	}
	mode, _ := result["billing_setting.billing_mode"].(string)
	if mode == "" {
		_, hasPrice := result["ModelPrice"]
		_, hasRatio := result["ModelRatio"]
		if _, builtin := billing_setting.GetBuiltinBillingExpr(name); builtin && !hasPrice && !hasRatio {
			mode = "tiered_expr"
		}
	}
	if mode == "tiered_expr" {
		result["billing_setting.billing_mode"] = mode
		if _, exists := result["billing_setting.billing_expr"]; !exists {
			if expression, ok := billing_setting.GetBuiltinBillingExpr(name); ok {
				result["billing_setting.billing_expr"] = expression
			}
		}
		return result
	}
	if _, exists := result["ModelPrice"]; exists {
		return result
	}
	if _, exists := result["ModelRatio"]; !exists && selfUse {
		result["ModelRatio"] = float64(37.5)
	}
	// Completion ratios include engine-enforced model defaults. Expose their
	// effective value without persisting them into the editable configuration.
	var configuredCompletion *float64
	if ratio, exists := result["CompletionRatio"].(float64); exists {
		configuredCompletion = &ratio
	}
	result["CompletionRatio"] = ratio_setting.ResolveCompletionRatio(name, configuredCompletion).Ratio
	for key, fallback := range map[string]float64{
		"CacheRatio":       ratio_setting.DefaultCacheRatio,
		"CreateCacheRatio": ratio_setting.DefaultCreateCacheRatio,
		"ImageRatio":       ratio_setting.DefaultImageRatio,
	} {
		if _, exists := result[key]; !exists {
			result[key] = fallback
		}
	}
	return result
}

// Offline conversion validates only its captured pricing, plugin and alias facts.
func validateModelPricingWithAliases(name string, values, previous PricingValues, generation *jsplugin.RoutingGeneration, resolveAlias func(string) (TaskAliasTarget, bool)) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("model name is required")
	}
	previousVariants, _ := previous[billing_setting.PluginBillingExprOption].(map[string]any)
	variants := map[string]any{}
	if value, exists := values[billing_setting.PluginBillingExprOption]; exists {
		var ok bool
		variants, ok = value.(map[string]any)
		if !ok || variants == nil {
			return errors.New("plugin billing expressions must be a plugin-to-expression object")
		}
		for key, value := range variants {
			expression, ok := value.(string)
			if !ok || strings.TrimSpace(expression) == "" {
				return fmt.Errorf("model %s: plugin %s: billing expression is required", name, key)
			}
			plugin, exists := generation.Get(key)
			declared := exists && slices.Contains(plugin.Meta.Models, name)
			// An unchanged override stays saveable after its plugin disappears or
			// stops declaring the model. A sole provider also keeps it after
			// narrowing its usage profile; relay checks shared models against
			// each profile, so their prices must follow it.
			if previousVariants[key] == expression && (!declared || !generation.SharedModel(name)) {
				continue
			}
			if !declared {
				return fmt.Errorf("model %s: plugin %s does not declare this model", name, key)
			}
			schema, _ := plugin.Meta.UsageForModel(name)
			if err := billing_setting.SmokeTestTaskExpr(expression, schema); err != nil {
				return fmt.Errorf("model %s: plugin %s: %w", name, key, err)
			}
		}
	}
	for key, value := range values {
		if key == billing_setting.PluginBillingExprOption {
			continue
		}
		if !IsModelPricingOption(key) {
			return fmt.Errorf("unsupported pricing field: %s", key)
		}
		if key == "billing_setting.billing_mode" {
			if value != "ratio" && value != "tiered_expr" {
				return errors.New("invalid billing mode")
			}
			continue
		}
		if key == "billing_setting.billing_expr" {
			expression, ok := value.(string)
			if !ok || strings.TrimSpace(expression) == "" {
				return errors.New("billing expression is required")
			}
			// Even a model expression currently shadowed by every provider must
			// compile; only its schema-specific smoke tests can be skipped.
			if _, err := billingexpr.CompileFromCache(expression); err != nil {
				return fmt.Errorf("model %s: %w", name, err)
			}
			// An unchanged expression a sole provider has been charging with keeps
			// its price after the plugin narrows its usage profile; relay checks
			// shared models against each profile, so their prices must follow it.
			unchanged := previous[key] == expression
			var err error
			if plugins := generation.PluginsByModel(name); len(plugins) > 0 {
				for _, plugin := range plugins {
					_, overridden := variants[plugin.Meta.Key]
					_, wasOverridden := previousVariants[plugin.Meta.Key]
					if overridden || (unchanged && !wasOverridden && len(plugins) == 1) {
						continue
					}
					schema, _ := plugin.Meta.UsageForModel(name)
					if err = billing_setting.SmokeTestTaskExpr(expression, schema); err != nil {
						return fmt.Errorf("model %s: plugin %s: %w", name, plugin.Meta.Key, err)
					}
				}
			} else if target, resolved := resolveAlias(name); resolved {
				if plugin, ok := generation.Get(target.PluginKey); ok {
					if !unchanged || generation.SharedModel(target.Declared) {
						schema, _ := plugin.Meta.UsageForModel(target.Declared)
						err = billing_setting.SmokeTestTaskExpr(expression, schema)
					}
				} else {
					err = billing_setting.SmokeTestExpr(expression)
				}
			} else if !unchanged || len(billingexpr.UsedUsageKeys(expression)) == 0 {
				err = billing_setting.SmokeTestExpr(expression)
			}
			// With no remaining plugin, an unchanged stored usage expression has
			// no schema to test. Preserve it so removing stale overrides or saving
			// other model prices does not become impossible.
			if err != nil {
				return fmt.Errorf("model %s: %w", name, err)
			}
			continue
		}
		number, ok := value.(float64)
		if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 {
			return fmt.Errorf("%s must be a finite, non-negative number", key)
		}
	}
	if values["billing_setting.billing_mode"] == "tiered_expr" {
		if _, exists := values["billing_setting.billing_expr"]; !exists {
			if _, builtin := billing_setting.GetBuiltinBillingExpr(name); !builtin {
				return errors.New("billing expression is required")
			}
		}
	}
	return nil
}
