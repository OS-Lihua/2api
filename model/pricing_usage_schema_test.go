package model

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func pricingUsagePluginSource(version, usageSchema string) string {
	return fmt.Sprintf(`
export const meta = {
  apiVersion: 1, key: "pricing-usage-probe", name: "Pricing Usage Probe", version: %q, author: {name: "Test"},
  models: ["pricing-usage-model"], fetchMode: "per_task", usageSchema: %s
};
export function buildSubmitRequest() { return {}; }
export function parseSubmitResponse() { return {}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {}; }
`, version, usageSchema)
}

func addLegacyProviderMappingColumns(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, db.Exec(`ALTER TABLE providers ADD COLUMN models text, ADD COLUMN model_mapping text`).Error)
}

func setLegacyProviderMapping(t *testing.T, db *gorm.DB, id int, names, mapping string) {
	t.Helper()
	result := db.Exec(`UPDATE providers SET models = ?, model_mapping = ? WHERE id = ?`, names, mapping, id)
	require.NoError(t, result.Error)
	require.EqualValues(t, 1, result.RowsAffected)
}

func legacyUsagePricingFixture(t *testing.T) (*gorm.DB, *jsplugin.Registry) {
	t.Helper()
	db := UseTestDatabases(t)
	require.NoError(t, db.AutoMigrate(&Provider{}, &Ability{}, &Model{}, &Option{}))
	addLegacyProviderMappingColumns(t, db)
	previous := jsplugin.DefaultRegistry
	registry := jsplugin.NewRegistry()
	jsplugin.DefaultRegistry = registry
	t.Cleanup(func() { jsplugin.DefaultRegistry = previous })
	return db, registry
}

func insertLegacyUsageProvider(t *testing.T, db *gorm.DB, id int, names, mapping string) {
	t.Helper()
	provider := Provider{Id: id, Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled,
		Name: fmt.Sprintf("legacy-%d", id)}
	require.NoError(t, db.Create(&provider).Error)
	setLegacyProviderMapping(t, db, id, names, mapping)
	for name := range strings.SplitSeq(names, ",") {
		require.NoError(t, db.Create(&Ability{Group: "default", Model: name, ChannelId: id, Enabled: true}).Error)
	}
}

func setLegacyUsageOption(t *testing.T, db *gorm.DB, key string, value any) {
	t.Helper()
	encoded, err := common.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, db.Save(&Option{Key: key, Value: string(encoded)}).Error)
}

func captureLegacyUsagePricing(t *testing.T, db *gorm.DB, generation *jsplugin.RoutingGeneration) *LegacyPricingConversionSource {
	t.Helper()
	source, err := LoadLegacyPricingConversionSource(db, generation)
	require.NoError(t, err)
	return source
}

func TestPricingCarriesTaskUsageSchemaAndRefreshesWithPluginGeneration(t *testing.T) {
	db, registry := legacyUsagePricingFixture(t)
	const key = "pricing-usage-probe"
	initialSource := pricingUsagePluginSource("1.0.0", `{
 seconds:{type:"number",unit:"second",description:{en:"Video generation unit price",zh:"视频生成单价"}},
 action:{enum:["video"],enumLabels:{video:{en:"Generate video",zh:"生成视频"}}}
}`)
	_, err := registry.Register(initialSource, jsplugin.Options{})
	require.NoError(t, err)
	insertLegacyUsageProvider(t, db, 901, "pricing-usage-model", "{}")
	initial := captureLegacyUsagePricing(t, db, registry.Generation())
	plugin, ok := initial.generation.Get(key)
	require.True(t, ok)
	schema, _ := plugin.Meta.UsageForModels("pricing-usage-model")
	assert.Equal(t, "second", schema["seconds"].Unit)
	assert.Equal(t, "Video generation unit price", schema["seconds"].Description["en"])
	assert.Equal(t, "视频生成单价", schema["seconds"].Description["zh"])
	assert.Equal(t, []string{"video"}, schema["action"].Enum)
	assert.Equal(t, "Generate video", schema["action"].EnumLabels["video"]["en"])
	assert.Equal(t, "生成视频", schema["action"].EnumLabels["video"]["zh"])
	_, ok = initial.generation.GetByModel("ordinary-model")
	assert.False(t, ok, "ordinary pricing must not acquire a task schema")
	ordinary, err := initial.Preview("ordinary-model", PricingValues{"ModelPrice": float64(0.25)})
	require.NoError(t, err)
	assert.Empty(t, ordinary.UnsupportedReason)
	assert.Equal(t, `tier("request", fixed(0.25))`, ordinary.Expression)

	updatedSource := pricingUsagePluginSource("1.1.0", `{
 seconds:{type:"number",unit:"second",description:{en:"Video processing unit price",zh:"视频处理单价"}},
 clips:{type:"number",unit:"count",unitLabel:{en:"Clip",zh:"片段"},description:{en:"Clip generation unit price",zh:"片段生成单价"}}}`)
	_, err = registry.Register(updatedSource, jsplugin.Options{})
	require.NoError(t, err)
	updated := captureLegacyUsagePricing(t, db, registry.Generation())
	current, ok := updated.generation.Get(key)
	require.True(t, ok)
	currentSchema, _ := current.Meta.UsageForModels("pricing-usage-model")
	require.Len(t, currentSchema, 2)
	assert.Equal(t, "1.1.0", current.Meta.Version)
	assert.Equal(t, "Video processing unit price", currentSchema["seconds"].Description["en"])
	assert.Equal(t, "视频处理单价", currentSchema["seconds"].Description["zh"])
	assert.Equal(t, "count", currentSchema["clips"].Unit)
	assert.Equal(t, "Clip", currentSchema["clips"].UnitLabel["en"])
	assert.Equal(t, "片段", currentSchema["clips"].UnitLabel["zh"])
	old, ok := initial.generation.Get(key)
	require.True(t, ok)
	oldSchema, _ := old.Meta.UsageForModels("pricing-usage-model")
	assert.Equal(t, "1.0.0", old.Meta.Version)
	assert.Equal(t, schema, oldSchema, "a captured source never follows registry publication")
	assert.NotContains(t, oldSchema, "clips")
	_, err = initial.Preview("pricing-usage-model", PricingValues{"billing_setting.billing_expr": `u("clips")`})
	require.ErrorContains(t, err, "not declared")
	_, err = updated.Preview("pricing-usage-model", PricingValues{"billing_setting.billing_expr": `u("clips")`})
	require.NoError(t, err)
}

func TestPricingAliasCarriesPluginUsageSchemaAndTailExpr(t *testing.T) {
	db, registry := legacyUsagePricingFixture(t)
	const key = "pricing-usage-probe"
	_, err := registry.Register(pricingUsagePluginSource("1.0.0", `{
 seconds:{type:"number",unit:"second",description:{en:"Video generation unit price",zh:"视频生成单价"}}}`), jsplugin.Options{})
	require.NoError(t, err)
	insertLegacyUsageProvider(t, db, 910, "alias-model,pricing-usage-model", `{"alias-model":"pricing-usage-model"}`)
	setLegacyUsageOption(t, db, "billing_setting.billing_mode", map[string]string{"pricing-usage-model": "tiered_expr", "alias-own-expr": "tiered_expr"})
	setLegacyUsageOption(t, db, "billing_setting.billing_expr", map[string]string{"pricing-usage-model": `u("seconds")`, "alias-own-expr": `u("seconds") * 2`})
	initial := captureLegacyUsagePricing(t, db, registry.Generation())
	target, ok := initial.resolveAlias("alias-model")
	require.True(t, ok)
	assert.Equal(t, key, target.PluginKey)
	assert.Equal(t, "pricing-usage-model", target.Declared)
	plugin, ok := initial.generation.Get(target.PluginKey)
	require.True(t, ok)
	schema, _ := plugin.Meta.UsageForModels(target.Declared)
	assert.Equal(t, "second", schema["seconds"].Unit)
	assert.Equal(t, "Video generation unit price", schema["seconds"].Description["en"])
	for _, name := range []string{"alias-model", "pricing-usage-model"} {
		assert.Equal(t, `u("seconds")`, initial.saleExpression(name, []legacyGatewayChannel{{name: name, upstream: "pricing-usage-model", protocol: protocol.Type("task:" + key)}}))
		_, err := initial.Preview(name, PricingValues{"billing_setting.billing_expr": `u("seconds")`})
		require.NoError(t, err)
	}

	insertLegacyUsageProvider(t, db, 911, "alias-own-expr,pricing-usage-model", `{"alias-own-expr":"pricing-usage-model"}`)
	refreshed := captureLegacyUsagePricing(t, db, registry.Generation())
	own, ok := refreshed.resolveAlias("alias-own-expr")
	require.True(t, ok)
	assert.Equal(t, target.Declared, own.Declared)
	assert.Equal(t, `u("seconds") * 2`, refreshed.saleExpression("alias-own-expr", []legacyGatewayChannel{{name: "alias-own-expr", upstream: own.Declared, protocol: protocol.Type("task:" + own.PluginKey)}}))
	_, ok = initial.resolveAlias("alias-own-expr")
	assert.False(t, ok, "later Provider mappings cannot enter an existing source capture")
	setLegacyUsageOption(t, db, "billing_setting.billing_expr", map[string]string{"pricing-usage-model": `u("seconds") * 9`})
	assert.Equal(t, `u("seconds")`, initial.saleExpression("alias-model", []legacyGatewayChannel{{name: "alias-model", upstream: target.Declared, protocol: protocol.Type("task:" + key)}}))
}

func TestPricingUsageProfilesAndAliases(t *testing.T) {
	db, registry := legacyUsagePricingFixture(t)
	const key = "pricing-profiles"
	const pluginSource = `
export const meta = {
 apiVersion:1,key:"pricing-profiles",name:"Pricing Profiles",version:"1.0.0",author:{name:"Test"},
 models:["profile-image","profile-video"],fetchMode:"per_task",
 usageSchema:{fallback:{type:"number",unit:"count",description:"Task execution unit price"}},
 usageExamples:[{label:"default",facts:{fallback:1}}],
 usageProfiles:[
  {models:["profile-image"],schema:{image_count:{type:"number",unit:"count",description:"Image generation unit price"}}},
  {models:["profile-video"],schema:{seconds:{type:"number",unit:"second",description:"Video generation unit price"}},examples:[{label:"5 seconds",facts:{seconds:5}}]}
 ]
};
export function buildSubmitRequest(){return {};}
export function parseSubmitResponse(){return {};}
export function buildQueryRequest(){return {};}
export function parseTaskResult(){return {};}
`
	_, err := registry.Register(pluginSource, jsplugin.Options{})
	require.NoError(t, err)
	for index, mapping := range []string{
		`{"image-alias":"profile-image","video-alias":"profile-video","ambiguous-alias":"profile-image"}`,
		`{"ambiguous-alias":"profile-video"}`,
	} {
		insertLegacyUsageProvider(t, db, 920+index, "profile-image,profile-video,image-alias,video-alias,ambiguous-alias", mapping)
	}
	source := captureLegacyUsagePricing(t, db, registry.Generation())
	plugin, ok := source.generation.Get(key)
	require.True(t, ok)
	for _, test := range []struct{ name, field, unit, example string }{
		{"profile-image", "image_count", "count", ""},
		{"profile-video", "seconds", "second", "5 seconds"},
		{"image-alias", "image_count", "count", ""},
		{"video-alias", "seconds", "second", "5 seconds"},
		{"ambiguous-alias", "fallback", "count", "default"},
	} {
		t.Run(test.name, func(t *testing.T) {
			declared := test.name
			if target, ok := source.resolveAlias(test.name); ok {
				assert.Equal(t, key, target.PluginKey)
				declared = target.Declared
				if test.name == "ambiguous-alias" {
					assert.Empty(t, declared)
				}
			}
			schema, examples := plugin.Meta.UsageForModels(declared)
			require.Len(t, schema, 1)
			assert.Equal(t, "number", schema[test.field].Type)
			assert.Equal(t, test.unit, schema[test.field].Unit)
			if test.example == "" {
				assert.Empty(t, examples, "profile examples do not inherit default examples")
			} else {
				require.Len(t, examples, 1)
				assert.Equal(t, test.example, examples[0].Label)
				want := 5
				if test.field == "fallback" {
					want = 1
				}
				assert.EqualValues(t, want, examples[0].Facts[test.field])
			}
			_, err := source.Preview(test.name, PricingValues{"billing_setting.billing_expr": `u("` + test.field + `")`})
			require.NoError(t, err)
			_, err = source.Preview(test.name, PricingValues{"billing_setting.billing_expr": `u("missing")`})
			require.ErrorContains(t, err, "not declared")
		})
	}
	for name, field := range map[string]string{"profile-image": "seconds", "video-alias": "image_count", "ambiguous-alias": "seconds"} {
		_, err := source.Preview(name, PricingValues{"billing_setting.billing_expr": `u("` + field + `")`})
		require.ErrorContains(t, err, "not declared")
	}

	// A real owner stores the old expression before the plugin narrows its schema.
	require.NoError(t, db.AutoMigrate(&User{}, &Channel{}, &ModelOffering{}, &ModelOfferingMember{}))
	require.NoError(t, MigratePricing(db))
	actor := User{Username: "usage-price-owner", AffCode: "usage-price-owner"}
	require.NoError(t, db.Create(&actor).Error)
	member := Model{ModelName: "profile-image"}
	require.NoError(t, db.Create(&member).Error)
	channel := Channel{ProviderID: 920, ModelID: member.Id, Protocol: protocol.Type("task:" + key), UpstreamModelName: "profile-image", ParameterOverrides: "{}", Status: ChannelPending}
	require.NoError(t, db.Omit("Provider", "Model").Create(&channel).Error)
	oldPrice := PriceVersion{OwnerType: PriceOwnerChannel, OwnerID: channel.ID, Currency: "USD", Expression: `u("image_count")`, EffectiveAt: 1}
	require.NoError(t, CreatePriceVersion(t.Context(), &oldPrice, actor.Id))
	setLegacyUsageOption(t, db, "billing_setting.billing_expr", map[string]string{"profile-image": `u("image_count")`})
	setLegacyUsageOption(t, db, billing_setting.PluginBillingExprOption, map[string]string{key + "::profile-image": `u("image_count") * 2`})
	updated := strings.Replace(pluginSource, `version:"1.0.0"`, `version:"1.1.0"`, 1)
	updated = strings.Replace(updated, `image_count:{`, `images:{`, 1)
	_, err = registry.Register(updated, jsplugin.Options{})
	require.NoError(t, err)
	current := captureLegacyUsagePricing(t, db, registry.Generation())
	stored := PricingValues{"billing_setting.billing_expr": `u("image_count")`, billing_setting.PluginBillingExprOption: map[string]any{key: `u("image_count") * 2`}}
	_, err = current.Preview("profile-image", stored)
	require.NoError(t, err, "unchanged historical expressions remain inspectable")
	_, err = current.Preview("profile-image", PricingValues{"billing_setting.billing_expr": `u("image_count") * 3`})
	require.ErrorContains(t, err, "not declared")
	stored[billing_setting.PluginBillingExprOption] = map[string]any{key: `u("image_count") * 3`}
	_, err = current.Preview("profile-image", stored)
	require.ErrorContains(t, err, "not declared")
	oldSchema, _ := plugin.Meta.UsageForModels("profile-image")
	assert.Contains(t, oldSchema, "image_count")
	latest, ok := current.generation.Get(key)
	require.True(t, ok)
	newSchema, _ := latest.Meta.UsageForModels("profile-image")
	assert.Contains(t, newSchema, "images")
	assert.NotContains(t, newSchema, "image_count")
	videoSchema, _ := latest.Meta.UsageForModels("profile-video")
	assert.Equal(t, "second", videoSchema["seconds"].Unit)
	for _, expression := range []string{`u("image_count")`, `u("image_count") * 3`, `u("missing")`, `u("seconds")`} {
		proposal := PriceVersion{OwnerType: PriceOwnerChannel, OwnerID: channel.ID, Currency: "USD", Expression: expression, EffectiveAt: 2}
		require.ErrorContains(t, CreatePriceVersion(t.Context(), &proposal, actor.Id), "Invalid price expression", "a new PriceVersion cannot borrow the historical unchanged-expression exemption")
	}
	valid := PriceVersion{OwnerType: PriceOwnerChannel, OwnerID: channel.ID, Currency: "USD", Expression: `u("images")`, EffectiveAt: 2}
	require.NoError(t, CreatePriceVersion(t.Context(), &valid, actor.Id))
	var preserved PriceVersion
	require.NoError(t, db.First(&preserved, oldPrice.ID).Error)
	assert.Equal(t, `u("image_count")`, preserved.Expression)
	var count int64
	require.NoError(t, db.Model(&PriceVersion{}).Count(&count).Error)
	assert.EqualValues(t, 2, count, "rejected edits create no price history")
}

func TestPricingSharedPluginVariantsUseEachSchemaAndExpression(t *testing.T) {
	db, registry := legacyUsagePricingFixture(t)
	for _, spec := range []struct{ key, field, unit, label string }{
		{"pricing-alpha", "seconds", "second", "Video generation unit price"},
		{"pricing-beta", "credits", "credit", "Generation credit unit price"},
	} {
		pluginSource := strings.ReplaceAll(pricingUsagePluginSource("1.0.0", fmt.Sprintf(`{%s:{type:"number",unit:%q,description:%q}}`, spec.field, spec.unit, spec.label)), "pricing-usage-probe", spec.key)
		_, err := registry.Register(pluginSource, jsplugin.Options{})
		require.NoError(t, err)
	}
	insertLegacyUsageProvider(t, db, 930, "pricing-usage-model", "{}")
	const name = "pricing-usage-model"
	const expression = `tier("base", u("seconds") * 0.4)`
	const override = `tier("beta", u("credits") * 2)`
	setLegacyUsageOption(t, db, "billing_setting.billing_mode", map[string]string{name: "tiered_expr"})
	setLegacyUsageOption(t, db, "billing_setting.billing_expr", map[string]string{name: expression})
	setLegacyUsageOption(t, db, billing_setting.PluginBillingExprOption, map[string]string{})
	source := captureLegacyUsagePricing(t, db, registry.Generation())
	plugins := source.generation.PluginsByModel(name)
	require.Len(t, plugins, 2)
	for _, plugin := range plugins {
		schema, _ := plugin.Meta.UsageForModels(name)
		if plugin.Meta.Key == "pricing-alpha" {
			assert.Equal(t, "second", schema["seconds"].Unit)
			assert.True(t, billing_setting.TaskExprCompatible(expression, schema))
		} else {
			assert.Equal(t, "pricing-beta", plugin.Meta.Key)
			assert.Equal(t, "credit", schema["credits"].Unit)
			assert.False(t, billing_setting.TaskExprCompatible(expression, schema))
		}
	}
	draft := PricingValues{"billing_setting.billing_expr": expression}
	_, err := source.Preview(name, draft)
	require.ErrorContains(t, err, "plugin pricing-beta")
	draft[billing_setting.PluginBillingExprOption] = map[string]any{"pricing-beta": override}
	_, err = source.Preview(name, draft)
	require.NoError(t, err)
	for _, invalid := range []any{map[string]any{"missing": "1"}, map[string]any{"pricing-beta": float64(1)}, map[string]any{"pricing-beta": `u("seconds")`}, map[string]any(nil), []any{}, "invalid"} {
		draft[billing_setting.PluginBillingExprOption] = invalid
		_, err = source.Preview(name, draft)
		assert.Error(t, err)
	}
	_, err = source.Preview(name, PricingValues{
		"billing_setting.billing_expr":          "invalid expression(",
		billing_setting.PluginBillingExprOption: map[string]any{"pricing-alpha": expression, "pricing-beta": `u("credits")`},
	})
	require.ErrorContains(t, err, "compile")
	setLegacyUsageOption(t, db, billing_setting.PluginBillingExprOption, map[string]string{"pricing-beta::" + name: override})
	priced := captureLegacyUsagePricing(t, db, registry.Generation())
	alpha := legacyGatewayChannel{name: name, upstream: name, protocol: "task:pricing-alpha"}
	beta := legacyGatewayChannel{name: name, upstream: name, protocol: "task:pricing-beta"}
	assert.Equal(t, expression, priced.saleExpression(name, []legacyGatewayChannel{alpha}))
	assert.Equal(t, override, priced.saleExpression(name, []legacyGatewayChannel{beta}))
	assert.Empty(t, priced.saleExpression(name, []legacyGatewayChannel{alpha, beta}), "incompatible route prices cannot become one guessed sale contract")
	assert.Equal(t, expression, source.saleExpression(name, []legacyGatewayChannel{beta}), "existing capture retains its inherited expression for compatibility checking")

	setLegacyUsageOption(t, db, billing_setting.PluginBillingExprOption, map[string]string{"pricing-beta::" + name: "0"})
	free := captureLegacyUsagePricing(t, db, registry.Generation())
	_, err = free.Preview(name, PricingValues{"billing_setting.billing_expr": expression, billing_setting.PluginBillingExprOption: map[string]any{"pricing-beta": "0"}})
	require.NoError(t, err)
	assert.Equal(t, "0", free.saleExpression(name, []legacyGatewayChannel{beta}), "explicit zero is not a missing override")
	setLegacyUsageOption(t, db, "ModelPrice", map[string]float64{name: 0.25})
	setLegacyUsageOption(t, db, "billing_setting.billing_mode", map[string]string{name: "ratio"})
	setLegacyUsageOption(t, db, billing_setting.PluginBillingExprOption, map[string]string{"pricing-beta::" + name: override})
	mixed := captureLegacyUsagePricing(t, db, registry.Generation())
	assert.Equal(t, float64(0.25), mixed.pricing["ModelPrice"][name])
	assert.Empty(t, mixed.saleExpression(name, []legacyGatewayChannel{alpha}), "legacy per-call task prices require explicit conversion, not a guessed task expression")
	assert.Equal(t, override, mixed.saleExpression(name, []legacyGatewayChannel{beta}))
	setLegacyUsageOption(t, db, billing_setting.PluginBillingExprOption, map[string]string{})
	perCall := captureLegacyUsagePricing(t, db, registry.Generation())
	assert.Empty(t, modelPricingValues(perCall.pricing, name)[billing_setting.PluginBillingExprOption])
	preview, err := perCall.Preview(name, PricingValues{"ModelPrice": float64(0.25)})
	require.NoError(t, err)
	assert.Contains(t, preview.UnsupportedReason, "Task pricing must be converted manually")
	ordinary, err := perCall.Preview("ordinary-model", PricingValues{"ModelPrice": float64(0.25)})
	require.NoError(t, err)
	value, _, err := billingexpr.RunMoneyExpr(ordinary.Expression, billingexpr.MoneyInput{}, "USD")
	require.NoError(t, err)
	amount, err := value.Receivable()
	require.NoError(t, err)
	assert.EqualValues(t, 250000000, amount.Nanos)

	setLegacyUsageOption(t, db, billing_setting.PluginBillingExprOption, map[string]string{"pricing-beta::" + name: override})
	require.NoError(t, registry.Unregister("pricing-alpha"))
	single := captureLegacyUsagePricing(t, db, registry.Generation())
	remaining := single.generation.PluginsByModel(name)
	require.Len(t, remaining, 1)
	assert.Equal(t, "pricing-beta", remaining[0].Meta.Key)
	assert.Equal(t, override, single.saleExpression(name, []legacyGatewayChannel{beta}))
	assert.Len(t, source.generation.PluginsByModel(name), 2, "old captures retain both declarations")
	var persisted Option
	require.NoError(t, db.First(&persisted, "key = ?", billing_setting.PluginBillingExprOption).Error)
	var stored map[string]string
	require.NoError(t, common.UnmarshalJsonStr(persisted.Value, &stored))
	assert.Equal(t, map[string]string{"pricing-beta::" + name: override}, stored, "plugin removal never deletes another plugin's stored price")
}

// Diagnostics follow persisted owner identities and the selected immutable
// price, not old global prices, aliases or every model in a plugin catalog.
func TestModelPricingIssuesFollowPluginChanges(t *testing.T) {
	db := UseTestDatabases(t)
	require.NoError(t, db.AutoMigrate(&User{}, &Model{}, &Provider{}, &Channel{}, &ModelOffering{}, &ModelOfferingMember{}, &TaskPlugin{}))
	require.NoError(t, MigratePricing(db))
	actor := User{Username: "diagnostic", AffCode: "diagnostic"}
	require.NoError(t, db.Create(&actor).Error)
	const seconds = `{seconds:{type:"number",unit:"second",description:"Video generation unit price"}}`
	const credits = `{credits:{type:"number",unit:"credit",description:"Credit unit price"}}`
	pluginSource := func(key, version, models, schema string) string {
		source := strings.ReplaceAll(pricingUsagePluginSource(version, schema), "pricing-usage-probe", key)
		return strings.Replace(source, `["pricing-usage-model"]`, models, 1)
	}
	const declarations = `["priced","unpriced","free","future","dropped","mapped-cost","ownerless","shared","member-only"]`
	alphaSource := pluginSource("issue-alpha", "1.0.0", declarations, seconds)
	alphaSource = strings.Replace(alphaSource, "fetchMode:", `usageProfiles:[{models:["mapped-cost"],schema:`+credits+`}],fetchMode:`, 1)
	_, err := jsplugin.DefaultRegistry.Register(alphaSource, jsplugin.Options{})
	require.NoError(t, err)
	_, err = jsplugin.DefaultRegistry.Register(pluginSource("issue-beta", "1.0.0", `["shared"]`, credits), jsplugin.Options{})
	require.NoError(t, err)
	t.Cleanup(func() {
		jsplugin.DefaultRegistry.Unregister("issue-alpha")
		jsplugin.DefaultRegistry.Unregister("issue-beta")
	})
	addLegacyProviderMappingColumns(t, db)
	provider := Provider{Name: "private", Key: "fixture"}
	require.NoError(t, db.Create(&provider).Error)
	setLegacyProviderMapping(t, db, provider.Id, "ignored-old-model", `{"priced":"not-a-declaration"}`)
	price := func(ownerType string, id int, expression string, effective int64) PriceVersion {
		record := PriceVersion{OwnerType: ownerType, OwnerID: id, Currency: "USD", Expression: expression, EffectiveAt: effective, CreatedBy: actor.Id}
		if ownerType == PriceOwnerChannel {
			record.ChannelID = &id
		} else {
			record.OfferingID = &id
		}
		require.NoError(t, db.Omit("Creator", "Channel", "Offering").Create(&record).Error)
		return record
	}
	channel := func(key, client, upstream, expression string) Channel {
		member := Model{ModelName: client}
		require.NoError(t, db.Where("model_name = ?", client).FirstOrCreate(&member).Error)
		record := Channel{ProviderID: provider.Id, ModelID: member.Id, Protocol: protocol.Type("task:" + key), UpstreamModelName: upstream, ParameterOverrides: "{}", Status: ChannelPending}
		require.NoError(t, db.Omit("Provider", "Model").Create(&record).Error)
		if expression != "" {
			price(PriceOwnerChannel, record.ID, expression, 1)
		}
		return record
	}
	priced := channel("issue-alpha", "priced", "priced", `tier("base", u("seconds") * 0.1)`)
	unpriced := channel("issue-alpha", "unpriced", "unpriced", "")
	channel("issue-alpha", "free", "free", `tier("base", 0)`)
	future := channel("issue-alpha", "future", "future", "")
	price(PriceOwnerChannel, future.ID, `tier("base", 0)`, MaxPriceEffectiveAt)
	dropped := channel("issue-alpha", "dropped", "dropped", `tier("base", u("seconds"))`)
	mapped := channel("issue-alpha", "private-client-alias", "mapped-cost", `tier("base", u("seconds"))`)
	price(PriceOwnerChannel, mapped.ID, `tier("base", u("credits"))`, MaxPriceEffectiveAt)
	shared := channel("issue-alpha", "shared", "shared", `tier("base", u("seconds"))`)
	beta := channel("issue-beta", "shared", "shared", `tier("base", u("seconds"))`)
	originalProviderID := provider.Id
	outsideProvider := Provider{Name: "scope-excluded", Key: "fixture"}
	require.NoError(t, db.Create(&outsideProvider).Error)
	provider.Id = outsideProvider.Id
	outside := channel("issue-alpha", "shared", "mapped-cost", `tier("base", u("credits"))`)
	provider.Id = originalProviderID
	offering := func(name string, memberID int, scope OfferingChannelScope, expression string) ModelOffering {
		record := ModelOffering{Name: name, Visibility: "all", Groups: []string{}, RouteMethod: "cost", AcceptedConversionLosses: []string{}, ChannelScope: scope, Members: []ModelOfferingMember{{ModelID: memberID, Tier: 1}}}
		require.NoError(t, db.Omit("Members.Model").Create(&record).Error)
		if expression != "" {
			price(PriceOwnerOffering, record.ID, expression, 1)
		}
		return record
	}
	goodScope := OfferingChannelScope{IncludeProviderIDs: []int{provider.Id, outsideProvider.Id}, ExcludeChannelIDs: []int{outside.ID}}
	offering("mixed-plugin-compatible-alpha", shared.ModelID, goodScope, `tier("base", u("seconds"))`)
	unpricedOffering := offering("private-unpriced-sale", shared.ModelID, OfferingChannelScope{IncludeChannelIDs: []int{shared.ID}}, "")
	offering("scoped-out-alpha", shared.ModelID, OfferingChannelScope{IncludeChannelIDs: []int{beta.ID}}, `tier("base", u("credits"))`)
	member := Model{ModelName: "member-only"}
	require.NoError(t, db.Create(&member).Error)
	memberOnly := offering("persisted-member-without-channel", member.Id, OfferingChannelScope{}, "")
	read := func(key string, visibility TaskPluginPricingVisibility) []TaskPluginPricingIssue {
		var result *TaskPluginPricingDiagnostics
		err := db.Transaction(func(tx *gorm.DB) error {
			var err error
			result, err = GetTaskPluginPricingDiagnostics(context.Background(), tx, key, visibility)
			return err
		}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
		require.NoError(t, err)
		require.Equal(t, TaskPluginPricingReady, result.State)
		return result.Issues
	}
	both := TaskPluginPricingVisibility{Channels: true, Offerings: true}
	issues := read("issue-alpha", both)
	require.Len(t, issues, 5, "no meta-only owner, no other-plugin or out-of-scope error, exact zero valid")
	assert.Equal(t, []int{unpriced.ID, future.ID, mapped.ID, unpricedOffering.ID, memberOnly.ID}, []int{issues[0].OwnerID, issues[1].OwnerID, issues[2].OwnerID, issues[3].OwnerID, issues[4].OwnerID})
	assert.Equal(t, TaskPluginPricingIncompatible, issues[2].Reason, "upstream profile overrides client/default schema; future price cannot replace current")
	require.NotNil(t, issues[2].PriceVersionID)
	assert.Nil(t, issues[1].PriceVersionID, "future-only is unconfigured")
	for _, issue := range read("issue-alpha", TaskPluginPricingVisibility{Offerings: true}) {
		assert.Equal(t, PriceOwnerOffering, issue.OwnerType)
	}
	betaIssues := read("issue-beta", both)
	require.Len(t, betaIssues, 2, "its channel and the mixed-plugin sale are incompatible only for beta")
	for _, issue := range betaIssues {
		assert.Equal(t, TaskPluginPricingIncompatible, issue.Reason)
	}
	price(PriceOwnerChannel, beta.ID, `tier("base", u("credits") * 2)`, 2)
	assert.Len(t, read("issue-beta", TaskPluginPricingVisibility{Channels: true}), 0, "an immutable later current purchase price repairs only its own owner")

	changed := strings.Replace(alphaSource, `"dropped",`, "", 1)
	changed = strings.Replace(changed, `version: "1.0.0"`, `version: "1.1.0"`, 1)
	_, err = jsplugin.DefaultRegistry.Register(changed, jsplugin.Options{})
	require.NoError(t, err)
	issues = read("issue-alpha", both)
	require.True(t, slices.ContainsFunc(issues, func(issue TaskPluginPricingIssue) bool {
		return issue.OwnerID == dropped.ID && issue.OwnerType == PriceOwnerChannel && issue.Reason == TaskPluginPricingStale
	}))

	// Publishing a new usage profile during SQL reads cannot mix generations.
	changedAgain := strings.Replace(changed, seconds, credits, 1)
	changedAgain = strings.Replace(changedAgain, `version: "1.1.0"`, `version: "1.2.0"`, 1)
	registered := false
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("diagnostic_generation_change", func(tx *gorm.DB) {
		if registered || tx.Statement.Table != "models" {
			return
		}
		registered = true
		_, err := jsplugin.DefaultRegistry.Register(changedAgain, jsplugin.Options{})
		require.NoError(t, err)
	}))
	t.Cleanup(func() { db.Callback().Query().Remove("diagnostic_generation_change") })
	issues = read("issue-alpha", both)
	require.True(t, registered)
	assert.False(t, slices.ContainsFunc(issues, func(issue TaskPluginPricingIssue) bool {
		return issue.OwnerType == PriceOwnerChannel && issue.OwnerID == priced.ID
	}), "one diagnostic retains its captured generation")
	issues = read("issue-alpha", both)
	assert.True(t, slices.ContainsFunc(issues, func(issue TaskPluginPricingIssue) bool {
		return issue.OwnerType == PriceOwnerChannel && issue.OwnerID == priced.ID && issue.Reason == TaskPluginPricingIncompatible
	}))
	var immutable PriceVersion
	require.NoError(t, db.Where("owner_type=? AND owner_id=? AND effective_at=1", PriceOwnerChannel, priced.ID).First(&immutable).Error)
	assert.Equal(t, `tier("base", u("seconds") * 0.1)`, immutable.Expression, "diagnostics never reprice existing owners")
}
