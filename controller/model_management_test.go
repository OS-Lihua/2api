package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relaykit/dto"

	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func modelManagementDB(t *testing.T) *gorm.DB {
	t.Helper()
	previousMaster := common.IsMasterNode
	previousRedis, previousMemory := common.RedisEnabled, common.MemoryCacheEnabled
	previousOptions := common.OptionMap
	previousConfig := config.GlobalConfig.ExportAllConfigs()
	restoreRatios := []struct {
		value   string
		restore func(string) error
	}{
		{ratio_setting.ModelPrice2JSONString(), ratio_setting.UpdateModelPriceByJSONString},
		{ratio_setting.ModelRatio2JSONString(), ratio_setting.UpdateModelRatioByJSONString},
		{ratio_setting.CompletionRatio2JSONString(), ratio_setting.UpdateCompletionRatioByJSONString},
		{ratio_setting.CacheRatio2JSONString(), ratio_setting.UpdateCacheRatioByJSONString},
		{ratio_setting.CreateCacheRatio2JSONString(), ratio_setting.UpdateCreateCacheRatioByJSONString},
		{ratio_setting.ImageRatio2JSONString(), ratio_setting.UpdateImageRatioByJSONString},
		{ratio_setting.AudioRatio2JSONString(), ratio_setting.UpdateAudioRatioByJSONString},
		{ratio_setting.AudioCompletionRatio2JSONString(), ratio_setting.UpdateAudioCompletionRatioByJSONString},
	}
	common.IsMasterNode = false
	common.RedisEnabled, common.MemoryCacheEnabled = false, false
	common.OptionMap = map[string]string{}
	t.Cleanup(func() {
		for _, value := range restoreRatios {
			require.NoError(t, value.restore(value.value))
		}
		config.UpdateConfigFromMap(config.GlobalConfig.Get("billing_setting"), map[string]string{"billing_mode": previousConfig["billing_setting.billing_mode"], "billing_expr": previousConfig["billing_setting.billing_expr"], "plugin_billing_expr": previousConfig[billing_setting.PluginBillingExprOption]})
		common.OptionMap = previousOptions
		common.IsMasterNode = previousMaster
		common.RedisEnabled, common.MemoryCacheEnabled = previousRedis, previousMemory
	})
	// UseTestDatabases restores LOG_DB and the log database type.
	database := model.UseTestDatabases(t)
	model.LOG_DB = database
	common.SetLogDatabaseType(common.DatabaseTypePostgreSQL)
	require.NoError(t, database.AutoMigrate(&model.Model{}, &model.Vendor{}, &model.Provider{}, &model.Channel{}, &model.Ability{}, &model.Option{}, &model.User{}, &model.AuditLog{}, &model.ModelOffering{}, &model.ModelOfferingName{}, &model.ModelOfferingMember{}))
	require.NoError(t, model.MigratePricing(database))
	for _, value := range restoreRatios {
		require.NoError(t, value.restore("{}"))
	}
	config.UpdateConfigFromMap(config.GlobalConfig.Get("billing_setting"), map[string]string{"billing_mode": "{}", "billing_expr": "{}", "plugin_billing_expr": "{}"})
	return database
}

func modelManagementRequest(t *testing.T, handler gin.HandlerFunc, method, path string, body any, output any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := common.Marshal(body)
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(method, path, bytes.NewReader(encoded))
	context.Set("role", common.RoleRootUser)
	handler(context)
	if output != nil {
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), output), recorder.Body.String())
	}
	return recorder
}

func legacyPricingSource(t *testing.T, db *gorm.DB) *model.LegacyPricingConversionSource {
	t.Helper()
	source, err := model.LoadLegacyPricingConversionSource(db, jsplugin.DefaultRegistry.Generation())
	require.NoError(t, err)
	return source
}

// Snapshot the actual historical bytes, not a synthesized legacy pricing view.
func modelManagementOptions(t *testing.T, db *gorm.DB) []model.Option {
	t.Helper()
	var rows []model.Option
	require.NoError(t, db.Order(`"key"`).Find(&rows).Error)
	return rows
}

func modelManagementPrices(t *testing.T, db *gorm.DB) []model.PriceVersion {
	t.Helper()
	var rows []model.PriceVersion
	require.NoError(t, db.Order("id").Find(&rows).Error)
	return rows
}

func modelManagementPriceOwner(t *testing.T, db *gorm.DB, name string, kind protocol.Type) model.Channel {
	t.Helper()
	var member model.Model
	err := db.Where("model_name = ?", name).First(&member).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		member = model.Model{ModelName: name, Status: 1}
		require.NoError(t, db.Create(&member).Error)
	} else {
		require.NoError(t, err)
	}
	provider := model.Provider{Name: "Immutable price owner", Type: 1, Status: common.ChannelStatusEnabled}
	require.NoError(t, db.Create(&provider).Error)
	channel := model.Channel{ProviderID: provider.Id, ModelID: member.Id, Protocol: kind, UpstreamModelName: name, ParameterOverrides: "{}", Status: model.ChannelDisabled, MappingConfirmed: true}
	require.NoError(t, db.Create(&channel).Error)
	return channel
}

func modelManagementPriceActor(t *testing.T, db *gorm.DB) int {
	t.Helper()
	var user model.User
	err := db.Where("username = ?", "immutable-price-actor").First(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		user = model.User{Username: "immutable-price-actor", Status: common.UserStatusEnabled}
		require.NoError(t, db.Create(&user).Error)
	} else {
		require.NoError(t, err)
	}
	return user.Id
}

func modelManagementPrice(t *testing.T, db *gorm.DB, owner model.Channel, expression string, at int64) model.PriceVersion {
	t.Helper()
	price := model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: owner.ID, Currency: money.Currency("USD"), Expression: expression, EffectiveAt: at}
	require.NoError(t, model.CreatePriceVersion(context.Background(), &price, modelManagementPriceActor(t, db)))
	return price
}

func modelManagementSale(t *testing.T, db *gorm.DB, memberID int, name string) (model.ModelOffering, model.PriceVersion) {
	t.Helper()
	offering := model.ModelOffering{Name: name, Visibility: "all", RouteMethod: "cost", Members: []model.ModelOfferingMember{{ModelID: memberID, Tier: 1}}}
	require.NoError(t, model.SaveModelOffering(context.Background(), &offering, "save"))
	price := model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: offering.ID, Currency: money.Currency("USD"), Expression: `tier("request", fixed(0))`, EffectiveAt: 1}
	require.NoError(t, model.CreatePriceVersion(context.Background(), &price, modelManagementPriceActor(t, db)))
	require.NoError(t, model.SaveModelOffering(context.Background(), &offering, "enable"))
	return offering, price
}

func modelManagementSales(t *testing.T, db *gorm.DB) []SaleCatalogOffering {
	t.Helper()
	var result struct {
		Success bool
		Data    []SaleCatalogOffering
	}
	actor := modelManagementPriceActor(t, db)
	response := modelManagementRequest(t, func(c *gin.Context) {
		c.Set("id", actor)
		GetPricing(c)
	}, http.MethodGet, "/api/pricing", nil, &result)
	require.True(t, result.Success, response.Body.String())
	return result.Data
}

func TestModelPricingConversionDatabase(t *testing.T) {
	db := modelManagementDB(t)
	declareLegacyProviderRoutingColumns(t, db)
	priceAmount := func(t *testing.T, expression string, usage *dto.Usage, input billingexpr.RequestInput, actualImages ...int) string {
		t.Helper()
		price := &model.PriceVersion{Expression: expression, Currency: "USD"}
		amount, _, _, err := service.GatewayPrice(price, usage, input, time.Time{}, decimal.NewFromInt(1), nil, actualImages...)
		require.NoError(t, err)
		return amount.String()
	}
	// These are legacy conversion sources, not account API mutations.
	insertLegacyProvider := func(t *testing.T, provider *model.Provider) {
		t.Helper()
		require.NoError(t, provider.Insert())
		seedLegacyProviderRouting(t, db, provider)
		require.NoError(t, provider.AddAbilities(nil))
	}
	contract, err := os.ReadFile("../pkg/billingexpr/testdata/frontend_simulation.json")
	require.NoError(t, err)
	var fixtures []struct {
		Name, Expression string
		Conversion       *struct {
			ModelName string              `json:"model_name"`
			Pricing   model.PricingValues `json:"pricing"`
		}
	}
	require.NoError(t, common.Unmarshal(contract, &fixtures))
	for _, fixture := range fixtures {
		if fixture.Conversion == nil {
			continue
		}
		t.Run("frontend_contract/"+fixture.Name, func(t *testing.T) {
			converted, err := legacyPricingSource(t, db).Preview(fixture.Conversion.ModelName, fixture.Conversion.Pricing)
			require.NoError(t, err)
			assert.Equal(t, fixture.Expression, converted.Expression)
		})
	}
	// The preview must resolve the draft, even when the running process
	// still has a different saved output multiplier for that model.
	require.NoError(t, ratio_setting.UpdateCompletionRatioByJSONString(`{"conversion-defaults":99}`))
	for _, tc := range []struct {
		name               string
		draft              model.PricingValues
		expression, reason string
	}{
		{"conversion-defaults", model.PricingValues{"ModelRatio": float64(2)}, `tier("base", p * 4 + c * 4)`, ""},
		{"gpt-4-32k", model.PricingValues{"ModelRatio": 30.0, "CacheRatio": 1.0}, `tier("base", p * 60 + c * 120)`, ""},
		{"conversion-free-cache", model.PricingValues{"ModelRatio": 0.0, "CacheRatio": 0.0}, `tier("base", p * 0 + c * 0 + cr * 0)`, ""},
		{"claude-3-7-sonnet-20250219", model.PricingValues{"ModelRatio": float64(1.5), "CacheRatio": float64(0.1), "CreateCacheRatio": float64(1.25)}, `tier("base", p * 3 + c * 15 + cr * 0.3 + cc * 3.75 + cc1h * 6)`, ""},
		{"conversion-image-default", model.PricingValues{"ModelRatio": float64(2), "ImageRatio": float64(1)}, `tier("base", p * 4 + c * 4)`, ""},
		{"conversion-image-free", model.PricingValues{"ModelRatio": float64(2), "ImageRatio": float64(0)}, `tier("base", p * 4 + c * 4 + cr * 4 + img * 0)`, ""},
		{"deepseek-chat", model.PricingValues{"ModelRatio": float64(0.135), "CacheRatio": float64(0.25)}, `tier("base", p * 0.27 + c * 0.27 + cr * 0.0675)`, ""},
		{"gpt-4o-custom", model.PricingValues{"ModelRatio": float64(2)}, `tier("base", p * 4 + c * 16)`, ""},
		{"gemini-2.5-pro-custom", model.PricingValues{"ModelRatio": float64(2)}, `tier("base", p * 4 + c * 32)`, ""},
		{"vendor/claude-sonnet-4", model.PricingValues{"ModelRatio": float64(2), "CompletionRatio": float64(0)}, `tier("base", p * 4 + c * 0 + cr * 4 + cc * 5 + cc1h * 8)`, ""},
		{"conversion-custom", model.PricingValues{"ModelRatio": float64(2), "CompletionRatio": float64(3), "CacheRatio": float64(0), "CreateCacheRatio": float64(1.5), "ImageRatio": float64(2)}, `tier("base", p * 4 + c * 12 + cr * 0 + cc * 6 + img * 8)`, ""},
		{"deepseek-reasoner", model.PricingValues{"ModelRatio": 0.275, "CacheRatio": 0.25}, `tier("base", p * 0.55 + c * 0.55 + cr * 0.1375)`, ""},
		{"gpt-5.6-sol", model.PricingValues{"ModelRatio": float64(2), "CompletionRatio": float64(2)}, `tier("base", p * 4 + c * 8)`, ""},
		{"gpt-5.5", model.PricingValues{"ModelRatio": float64(2), "CompletionRatio": float64(2)}, `tier("base", p * 4 + c * 8)`, ""},
		{"gpt-6-astra", model.PricingValues{"ModelRatio": float64(2), "CompletionRatio": float64(2)}, `tier("base", p * 4 + c * 8)`, ""},
		{"conversion-free", model.PricingValues{"ModelPrice": float64(0)}, `tier("request", fixed(0))`, ""},
		{"conversion-fixed", model.PricingValues{"ModelPrice": float64(0.25), "ModelRatio": float64(7)}, `tier("request", fixed(0.25))`, ""},
		{"gpt-4o-2024-05-13", model.PricingValues{"ModelRatio": float64(2), "CompletionRatio": float64(99)}, `tier("base", p * 4 + c * 12)`, ""},
		{"gpt-image-2", model.PricingValues{"ModelPrice": float64(1)}, `tier("image", fixed(1)) * image_count`, ""},
		// Bailian image models are served by the alibaba task plugin, so their pricing is a task usage expression.
		{"qwen-image-3.0-pro", model.PricingValues{"ModelPrice": float64(1)}, "", "Task pricing must be converted manually using the task usage schema."},
		{"wan2.7-image-pro", model.PricingValues{"ModelPrice": float64(1)}, "", "Task pricing must be converted manually using the task usage schema."},
		{"video-summarizer", model.PricingValues{"ModelPrice": float64(1)}, `tier("request", fixed(1))`, ""},
		{"sora-transcript-helper", model.PricingValues{"ModelPrice": float64(1)}, `tier("request", fixed(1))`, ""},
		{"gpt-realtime", model.PricingValues{"ModelRatio": float64(1)}, "", "Realtime pricing must be converted manually."},
		{"conversion-audio", model.PricingValues{"ModelRatio": float64(1), "AudioRatio": float64(2)}, `tier("base", p * 2 + c * 2 + ai * 4 + ao * 4)`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := modelManagementOptions(t, db)
			source := legacyPricingSource(t, db)
			converted, err := source.Preview(tc.name, tc.draft)
			require.NoError(t, err)
			assert.Equal(t, tc.expression, converted.Expression)
			assert.Equal(t, tc.reason, converted.UnsupportedReason)
			if tc.name == "claude-3-7-sonnet-20250219" {
				// Both providers include images in input; Claude reports cache
				// separately and does not report an image token breakdown.
				for _, usage := range []dto.Usage{
					{PromptTokens: 1000, CompletionTokens: 100, UsageSemantic: "openai", PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 200, CachedCreationTokens: 50, ImageTokens: 300}},
					{PromptTokens: 750, CompletionTokens: 100, UsageSemantic: "anthropic", ClaudeCacheCreation5mTokens: 50, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 200, CachedCreationTokens: 50}},
				} {
					assert.Equal(t, "0.0039975 USD", priceAmount(t, converted.Expression, &usage, billingexpr.RequestInput{}), "image input must remain billed exactly once")
				}
				usage := dto.Usage{PromptTokens: 750, CompletionTokens: 100, UsageSemantic: "anthropic", ClaudeCacheCreation5mTokens: 30, ClaudeCacheCreation1hTokens: 20, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 200, CachedCreationTokens: 50}}
				assert.Equal(t, "0.0040425 USD", priceAmount(t, converted.Expression, &usage, billingexpr.RequestInput{}), "mixed TTLs preserve both legacy cache-write prices")
			}
			if tc.name == "deepseek-chat" {
				for _, usage := range []dto.Usage{
					{PromptTokens: 1000, CompletionTokens: 100, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 200}},
					{PromptTokens: 800, CompletionTokens: 100, UsageSemantic: "anthropic", PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 200}},
				} {
					assert.Equal(t, "0.0002565 USD", priceAmount(t, converted.Expression, &usage, billingexpr.RequestInput{}), "cache reads remain billed across both response formats")
				}
			}
			assert.Equal(t, before, modelManagementOptions(t, db), "captured preview must not write historical options")
			if tc.expression == "" {
				return
			}
			// Save the converted expression through the real immutable owner API,
			// not through a retired model-name Option writer.
			owner := modelManagementPriceOwner(t, db, tc.name, protocol.OpenAIChat)
			price := modelManagementPrice(t, db, owner, tc.expression, 1)
			current, err := model.CurrentPriceVersion(context.Background(), db, model.PriceOwnerChannel, owner.ID, 1)
			require.NoError(t, err)
			assert.Equal(t, price.ID, current.ID)
			assert.Equal(t, tc.expression, current.Expression)
			duplicate := model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: owner.ID, Currency: money.Currency("USD"), Expression: tc.expression, EffectiveAt: 1}
			require.Error(t, model.CreatePriceVersion(context.Background(), &duplicate, price.CreatedBy), "same owner/effective time cannot overwrite the accepted expression")
			assert.Equal(t, before, modelManagementOptions(t, db), "native saves never dual-write legacy options")
			repeated, err := source.Preview(tc.name, tc.draft)
			require.NoError(t, err)
			assert.Equal(t, converted, repeated)
			if _, fixed := tc.draft["ModelPrice"]; !fixed {
				require.NotEmpty(t, converted.Effective)
				configuredCompletion, _ := tc.draft["CompletionRatio"].(float64)
				var completion *float64
				if _, exists := tc.draft["CompletionRatio"]; exists {
					completion = &configuredCompletion
				}
				assert.Equal(t, ratio_setting.ResolveCompletionRatio(tc.name, completion).Ratio, converted.Effective["CompletionRatio"])
				for field, fallback := range map[string]float64{"CacheRatio": ratio_setting.DefaultCacheRatio, "CreateCacheRatio": ratio_setting.DefaultCreateCacheRatio, "ImageRatio": ratio_setting.DefaultImageRatio} {
					if configured, exists := tc.draft[field]; exists {
						assert.Equal(t, configured, converted.Effective[field])
					} else {
						assert.Equal(t, fallback, converted.Effective[field])
					}
				}
			}
		})
	}
	insertLegacyProvider(t, &model.Provider{Name: "Mapped image", Models: "conversion-alias", Type: 1, ModelMapping: common.GetPointer(`{"conversion-alias":"conversion-hop","conversion-hop":"gpt-image-2"}`)})
	preview, err := legacyPricingSource(t, db).Preview("conversion-alias", model.PricingValues{"ModelPrice": float64(1)})
	require.NoError(t, err)
	assert.Equal(t, `tier("image", fixed(1)) * image_count`, preview.Expression)
	assert.Empty(t, preview.UnsupportedReason)

	t.Run("cache_writes_follow_configured_prices_and_claude_names", func(t *testing.T) {
		for _, tc := range []struct {
			name       string
			configured *float64
			mode       model.CacheWriteMode
		}{
			{"unrecognized-model", nil, model.CacheWriteNone},
			{"gpt-5.6-terra", nil, model.CacheWriteNone},
			{"gpt-5.6-terra", common.GetPointer(1.25), model.CacheWriteOpenAITTL},
			{"custom-GPT-alias", common.GetPointer(0.0), model.CacheWriteOpenAITTL},
			{"vendor/claude-sonnet-4-6-high", nil, model.CacheWriteClaudeTTL},
			{"custom-CLAUDE-alias", common.GetPointer(0.0), model.CacheWriteClaudeTTL},
			{"deepseek-reasoner", common.GetPointer(0.0), model.CacheWriteStandard},
			{"custom-cache", common.GetPointer(1.5), model.CacheWriteStandard},
		} {
			t.Run(tc.name+"/"+string(tc.mode), func(t *testing.T) {
				draft := model.PricingValues{"ModelRatio": 2.0, "CompletionRatio": 2.0, "CacheRatio": 0.25, "billing_setting.billing_mode": "ratio"}
				if tc.configured != nil {
					draft["CreateCacheRatio"] = *tc.configured
				}
				converted, err := legacyPricingSource(t, db).Preview(tc.name, draft)
				require.NoError(t, err)
				require.NotEmpty(t, converted.Expression)
				assert.Equal(t, tc.mode, converted.CacheWriteMode)
				assert.Equal(t, float64(2), converted.Effective["ModelRatio"])
				assert.Equal(t, float64(0.25), converted.Effective["CacheRatio"])
				if tc.configured != nil {
					assert.Equal(t, *tc.configured, converted.Effective["CreateCacheRatio"])
				}
				assert.Equal(t, converted.CacheWriteMode, model.ResolveCacheWriteMode(tc.name, draft))
				vars := billingexpr.UsedVars(converted.Expression)
				assert.Equal(t, tc.mode != model.CacheWriteNone, vars["cc"])
				assert.Equal(t, tc.mode == model.CacheWriteClaudeTTL, vars["cc1h"])
				assert.Equal(t, tc.mode == model.CacheWriteOpenAITTL, vars["cc12h"])
				if tc.configured != nil && *tc.configured == 0 {
					cost, _, err := billingexpr.RunExpr(converted.Expression, billingexpr.TokenParams{CC: 100, CC1h: 100, CC12h: 100})
					require.NoError(t, err)
					assert.Zero(t, cost, "explicit zero cache prices must stay free")
				}
			})
		}
		// GPT names price the 12h write at twice the configured write price.
		openai, err := legacyPricingSource(t, db).Preview("gpt-5.6-terra", model.PricingValues{"ModelRatio": 5.0, "CompletionRatio": 5.0, "CacheRatio": 0.1, "CreateCacheRatio": 1.25})
		require.NoError(t, err)
		assert.Equal(t, model.CacheWriteOpenAITTL, openai.CacheWriteMode)
		assert.Equal(t, `tier("base", p * 10 + c * 50 + cr * 1 + cc * 12.5 + cc12h * 25)`, openai.Expression)
		// The billing name remains authoritative for cache terms.
		insertLegacyProvider(t, &model.Provider{Name: "Cache alias", Type: 1, Models: "opaque-cache-alias", ModelMapping: common.GetPointer(`{"opaque-cache-alias":"claude-sonnet-4-6"}`)})
		alias, err := legacyPricingSource(t, db).Preview("opaque-cache-alias", model.PricingValues{"ModelRatio": 2.0})
		require.NoError(t, err)
		assert.Equal(t, `tier("base", p * 4 + c * 4)`, alias.Expression)
	})

	t.Run("same_price_cache_reads_preserve_overlapping_categories", func(t *testing.T) {
		for _, tc := range []struct {
			field, term            string
			ratio                  float64
			details                dto.InputTokenDetails
			amount, unpricedAmount string
		}{
			{"CreateCacheRatio", "cc * 75", 1.25, dto.InputTokenDetails{CachedTokens: 800, CacheWriteTokens: 500}, "0.0975 USD", "0.0795 USD"},
			{"ImageRatio", "img * 120", 2, dto.InputTokenDetails{CachedTokens: 600, ImageTokens: 500}, "0.108 USD", "0.102 USD"},
		} {
			t.Run(tc.field, func(t *testing.T) {
				converted, err := legacyPricingSource(t, db).Preview("overlapping-cache", model.PricingValues{"ModelRatio": 30.0, "CompletionRatio": 2.0, "CacheRatio": 1.0, tc.field: tc.ratio})
				require.NoError(t, err)
				assert.Equal(t, `tier("base", p * 60 + c * 120 + cr * 60 + `+tc.term+`)`, converted.Expression)
				usage := dto.Usage{PromptTokens: 1000, CompletionTokens: 100, PromptTokensDetails: tc.details}
				assert.Equal(t, tc.amount, priceAmount(t, converted.Expression, &usage, billingexpr.RequestInput{}), "preserve the existing overlapping-category charge")
				unpriced := `tier("base", p * 60 + c * 120 + ` + tc.term + `)`
				assert.Equal(t, tc.unpricedAmount, priceAmount(t, unpriced, &usage, billingexpr.RequestInput{}), "existing expressions without cr retain their OpenAI normalization")
			})
		}
	})
	t.Run("media_prices_and_request_adjustments", func(t *testing.T) {
		for _, quantity := range []string{"129", "-1", "0.5", "18446744073709551615"} {
			t.Run(quantity, func(t *testing.T) {
				_, engine := newCanonicalRelayFixture(t, protocol.OpenAIImages, "z-image")
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"z-image","parameters":{"n":`+quantity+`}}`))
				request.Header.Set("Content-Type", "application/json")
				engine.ServeHTTP(recorder, request)
				assert.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
			})
		}
		conflicting, err := legacyPricingSource(t, db).Preview("gemini-2.5-flash-conflict", model.PricingValues{"ModelRatio": 0.15, "AudioRatio": 10.0})
		require.NoError(t, err)
		assert.Contains(t, conflicting.UnsupportedReason, "Gemini and OpenAI audio prices differ")
		aliName := "z-image-media-conversion"
		insertLegacyProvider(t, &model.Provider{Name: aliName, Models: aliName, Type: 17, Status: common.ChannelStatusEnabled})
		aliConversion, err := legacyPricingSource(t, db).Preview(aliName, model.PricingValues{"ModelPrice": 0.04})
		require.NoError(t, err)
		require.Empty(t, aliConversion.UnsupportedReason)
		// Ali image billing lives in the alibaba task plugin, where prompt_extend is a
		// usage fact; the legacy conversion no longer adds a per-channel request multiplier.
		assert.Equal(t, `tier("image", fixed(0.04)) * image_count`, aliConversion.Expression)
		assert.Empty(t, aliConversion.BillingDetails.RequestRules)
		imageUsage := &dto.Usage{}
		input := billingexpr.RequestInput{Body: []byte(`{"parameters":{"prompt_extend":true}}`)}
		assert.Equal(t, "0.12 USD", priceAmount(t, aliConversion.Expression, imageUsage, input, 3))
		for _, tc := range []struct {
			name    string
			pricing model.PricingValues
			usage   dto.Usage
			want    string
		}{
			{"gemini-2.5-flash", model.PricingValues{"ModelRatio": 0.15, "CompletionRatio": 2.0, "CacheRatio": 0.1}, dto.Usage{PromptTokens: 1000, CompletionTokens: 100, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 100, AudioTokens: 200}}, "0.000473 USD"},
			{"gemini-2.5-flash-zero", model.PricingValues{"ModelRatio": 0.0}, dto.Usage{PromptTokens: 1000, PromptTokensDetails: dto.InputTokenDetails{AudioTokens: 200}}, "0.0002 USD"},
			{"media-audio", model.PricingValues{"ModelRatio": 1.0, "CompletionRatio": 3.0, "AudioRatio": 2.0, "AudioCompletionRatio": 4.0}, dto.Usage{PromptTokens: 1000, CompletionTokens: 100, PromptTokensDetails: dto.InputTokenDetails{TextTokens: 800, AudioTokens: 200}, CompletionTokenDetails: dto.OutputTokenDetails{TextTokens: 50, AudioTokens: 50}}, "0.0035 USD"},
			{"media-free-audio", model.PricingValues{"ModelRatio": 1.0, "AudioRatio": 0.0}, dto.Usage{PromptTokens: 1000, CompletionTokens: 100, PromptTokensDetails: dto.InputTokenDetails{AudioTokens: 200}, CompletionTokenDetails: dto.OutputTokenDetails{AudioTokens: 100}}, "0.0016 USD"},
			{"media-audio-cache", model.PricingValues{"ModelRatio": 1.0, "CompletionRatio": 3.0, "AudioRatio": 2.0, "CacheRatio": 0.1}, dto.Usage{PromptTokens: 1000, CompletionTokens: 100, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 400, AudioTokens: 200}, CompletionTokenDetails: dto.OutputTokenDetails{AudioTokens: 50}}, "0.0029 USD"},
			{"media-text-cache", model.PricingValues{"ModelRatio": 1.0, "CompletionRatio": 3.0, "AudioRatio": 2.0, "CacheRatio": 0.1}, dto.Usage{PromptTokens: 1000, CompletionTokens: 100, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 400}}, "0.00188 USD"},
			{"gemini-2.5-flash-overlap", model.PricingValues{"ModelRatio": 1.0}, dto.Usage{PromptTokens: 1000, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 800, AudioTokens: 500}}, "0.0021 USD"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				converted, err := legacyPricingSource(t, db).Preview(tc.name, tc.pricing)
				require.NoError(t, err)
				require.Empty(t, converted.UnsupportedReason)
				require.NotNil(t, converted.BillingDetails.AudioInputPrice)
				assert.Equal(t, tc.want, priceAmount(t, converted.Expression, &tc.usage, billingexpr.RequestInput{}))
			})
		}
		converted, err := legacyPricingSource(t, db).Preview("dall-e-3", model.PricingValues{"ModelPrice": 0.04})
		require.NoError(t, err)
		require.Empty(t, converted.UnsupportedReason)
		assert.NotContains(t, converted.Expression, "256x256")
		assert.NotContains(t, converted.Expression, "512x512")
		smallImage, err := legacyPricingSource(t, db).Preview("dall-e-2", model.PricingValues{"ModelPrice": 0.02})
		require.NoError(t, err)
		require.Empty(t, smallImage.UnsupportedReason)
		assert.NotContains(t, smallImage.Expression, "1792")
		input = billingexpr.RequestInput{Body: []byte(`{"size":"256x256"}`)}
		assert.Equal(t, "0.024 USD", priceAmount(t, smallImage.Expression, imageUsage, input, 3))
		for _, tc := range []struct {
			size, quality string
			count         int
			amount        string
		}{
			{"1024x1024", "standard", 3, "0.12 USD"},
			{"1024x1024", "hd", 2, "0.16 USD"},
			{"1024x1792", "hd", 3, "0.36 USD"},
		} {
			body, err := common.Marshal(map[string]any{"size": tc.size, "quality": tc.quality})
			require.NoError(t, err)
			input := billingexpr.RequestInput{Body: body}
			assert.Equal(t, tc.amount, priceAmount(t, converted.Expression, imageUsage, input, tc.count))
		}
		for _, channelType := range []int{20, 58} {
			name := fmt.Sprintf("gemini-3.5-flash-media-%d", channelType)
			insertLegacyProvider(t, &model.Provider{Name: name, Models: name, Type: channelType})
			require.NoError(t, db.Create(&model.Model{ModelName: name, Endpoints: `{"custom-text":"/v1/custom"}`}).Error)
			converted, err := legacyPricingSource(t, db).Preview(name, model.PricingValues{"ModelRatio": 0.75, "CompletionRatio": 6.0, "CacheRatio": 0.1})
			require.NoError(t, err)
			assert.Empty(t, converted.UnsupportedReason)
			assert.Equal(t, `tier("base", p * 1.5 + c * 9 + cr * 0.15)`, converted.Expression)
			assert.Nil(t, converted.BillingDetails.AudioInputPrice)
		}
	})
	require.NoError(t, db.Create(&model.Model{ModelName: "conversion-metadata", Endpoints: `{"image-generation":"/v1/images/generations"}`}).Error)
	preview, err = legacyPricingSource(t, db).Preview("conversion-metadata", model.PricingValues{"ModelPrice": float64(1)})
	require.NoError(t, err)
	assert.Equal(t, `tier("image", fixed(1)) * image_count`, preview.Expression)
	assert.Empty(t, preview.UnsupportedReason)
	for _, tc := range []struct {
		name, rule string
		nameRule   int
		endpoint   constant.EndpointType
	}{
		{"catalog-image-prefix-opaque", "catalog-image-prefix-", model.NameRulePrefix, constant.EndpointTypeImageGeneration},
		{"opaque-catalog-image-suffix", "-catalog-image-suffix", model.NameRuleSuffix, constant.EndpointTypeImageGeneration},
		{"before-catalog-image-middle-after", "catalog-image-middle", model.NameRuleContains, constant.EndpointTypeImageGeneration},
		{"opaque-catalog-video", "opaque-catalog-video", model.NameRuleExact, constant.EndpointTypeOpenAIVideo},
	} {
		t.Run("endpoint_metadata/"+tc.name, func(t *testing.T) {
			endpoints, err := common.Marshal(map[string]string{string(tc.endpoint): "/v1/fixture"})
			require.NoError(t, err)
			require.NoError(t, db.Create(&model.Model{ModelName: tc.rule, NameRule: tc.nameRule, Endpoints: string(endpoints)}).Error)
			converted, err := legacyPricingSource(t, db).Preview(tc.name, model.PricingValues{"ModelPrice": 1.0})
			require.NoError(t, err)
			if tc.endpoint == constant.EndpointTypeOpenAIVideo {
				assert.Empty(t, converted.Expression)
				assert.Equal(t, "Video pricing must be converted manually.", converted.UnsupportedReason)
			} else {
				assert.Equal(t, `tier("image", fixed(1)) * image_count`, converted.Expression)
			}
		})
	}
	sora := &model.Provider{Name: "Opaque video route", Models: "opaque-video-route", Type: constant.ChannelTypeSora, Group: "a,b"}
	insertLegacyProvider(t, sora)
	video, err := legacyPricingSource(t, db).Preview("opaque-video-route", model.PricingValues{"ModelPrice": 1.0})
	require.NoError(t, err)
	assert.Equal(t, "Video pricing must be converted manually.", video.UnsupportedReason)
}

func TestModelMetadataOmitsLegacyPricing(t *testing.T) {
	db := modelManagementDB(t)
	publisher := model.Vendor{Name: "Metadata publisher", Status: 1}
	require.NoError(t, db.Create(&publisher).Error)
	metadata := model.Model{
		ModelName: "metadata-price-independent", VendorID: publisher.Id, Status: 1,
		Description: "Stored description", Tags: "stored-tag", Icon: "stored-icon",
		Endpoints: `{"openai":{"path":"/v1/chat/completions","method":"POST"}}`,
	}
	require.NoError(t, common.Unmarshal([]byte(`{"features":["streaming"],"input_modalities":["text"],"output_modalities":["text"]}`), &metadata.Capabilities))
	require.NoError(t, metadata.Insert())
	provider := model.Provider{Name: "Metadata route", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled}
	require.NoError(t, db.Create(&provider).Error)
	require.NoError(t, db.Create(&model.Ability{Model: metadata.ModelName, Group: "metadata-group", ChannelId: provider.Id, Enabled: true}).Error)

	engine := gin.New()
	engine.Use(func(c *gin.Context) { c.Set("role", common.RoleRootUser) })
	engine.GET("/api/models/", GetAllModelsMeta)
	engine.GET("/api/models/search", SearchModelsMeta)
	engine.GET("/api/models/:id", GetModelMeta)
	engine.POST("/api/models/", CreateModelMeta)
	engine.PUT("/api/models/", UpdateModelMeta)
	call := func(method, path string, body any) map[string]any {
		t.Helper()
		encoded, err := common.Marshal(body)
		require.NoError(t, err)
		request := httptest.NewRequest(method, path, bytes.NewReader(encoded))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, request)
		require.Equal(t, http.StatusOK, recorder.Code)
		var response struct {
			Success bool
			Data    map[string]any
		}
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
		require.True(t, response.Success, recorder.Body.String())
		return response.Data
	}

	// Metadata reads must not trigger the legacy pricing cache's Vendor scan.
	var pricingVendorReads atomic.Int64
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("metadata_pricing_probe", func(tx *gorm.DB) {
		if tx.Statement.Table == "vendors" {
			pricingVendorReads.Add(1)
		}
	}))
	t.Cleanup(func() { require.NoError(t, db.Callback().Query().Remove("metadata_pricing_probe")) })
	paths := []string{"/api/models/?page_size=1", "/api/models/search?keyword=price-independent&page_size=1", fmt.Sprintf("/api/models/%d", metadata.Id)}
	baseline := make(map[string]map[string]any)
	for _, pricing := range []struct{ ratio, price string }{
		{`{"metadata-price-independent":2}`, `{}`},
		{`{}`, `{"metadata-price-independent":123}`},
		{`{}`, `{}`},
	} {
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(pricing.ratio))
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(pricing.price))
		for _, path := range paths {
			pricingVendorReads.Store(0)
			data := call(http.MethodGet, path, nil)
			assert.Zero(t, pricingVendorReads.Load(), "metadata must not refresh legacy pricing: %s", path)
			row := data
			if items, ok := data["items"]; ok {
				require.Len(t, items.([]any), 1)
				assert.EqualValues(t, 1, data["total"])
				assert.EqualValues(t, 1, data["page_size"])
				row = items.([]any)[0].(map[string]any)
			}
			assert.NotContains(t, row, "quota_types", "retired billing fields must be absent, not zero-filled")
			assert.Equal(t, metadata.Description, row["description"])
			assert.Equal(t, metadata.Endpoints, row["endpoints"])
			assert.Equal(t, []any{"openai"}, row["supported_endpoints"])
			assert.Equal(t, []any{"streaming"}, row["capabilities"].(map[string]any)["features"])
			for _, field := range []string{"bound_channels", "enable_groups", "matched_models", "matched_count"} {
				assert.NotContains(t, row, field, "legacy route details must not escape the model:read boundary")
			}
			assert.EqualValues(t, 0, row["configured_channel_count"], "legacy abilities are not stable Channel relationships")
			assert.EqualValues(t, 0, row["enabled_channel_count"])
			if previous, exists := baseline[path]; exists {
				assert.Equal(t, previous, data, "legacy price changes must not change metadata: %s", path)
			} else {
				baseline[path] = data
			}
		}
	}

	// Existing clients may still send the retired, non-persisted field. It must
	// neither be echoed nor change any legacy pricing configuration.
	require.NoError(t, db.Create(&model.Option{Key: "ModelPrice", Value: `{"metadata-price-independent":0.125,"metadata-created":0}`}).Error)
	priceBefore := modelManagementOptions(t, db)
	updated := call(http.MethodPut, "/api/models/", map[string]any{
		"id": metadata.Id, "model_name": metadata.ModelName, "vendor_id": publisher.Id,
		"description": "Updated description", "status": 1, "endpoints": metadata.Endpoints,
		"quota_types": []int{1},
	})
	assert.NotContains(t, updated, "quota_types")
	assert.Equal(t, []any{"streaming"}, updated["capabilities"].(map[string]any)["features"])
	stored := call(http.MethodGet, fmt.Sprintf("/api/models/%d", metadata.Id), nil)
	assert.Equal(t, "Updated description", stored["description"])
	assert.Equal(t, metadata.Endpoints, stored["endpoints"])
	created := call(http.MethodPost, "/api/models/", map[string]any{"model_name": "metadata-created", "status": 1, "quota_types": []int{0}})
	assert.NotContains(t, created, "quota_types")
	assert.NotZero(t, created["id"])
	assert.Equal(t, priceBefore, modelManagementOptions(t, db))
	assert.Empty(t, modelManagementPrices(t, db), "metadata CRUD must not invent prices")
}

func TestModelMetadataNativeEndpoints(t *testing.T) {
	db := modelManagementDB(t)
	provider := model.Provider{Name: "Native declarations", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled}
	require.NoError(t, db.Create(&provider).Error)
	engine := gin.New()
	engine.Use(func(c *gin.Context) { c.Set("role", common.RoleRootUser) })
	engine.GET("/api/models/", GetAllModelsMeta)
	engine.GET("/api/models/search", SearchModelsMeta)
	engine.GET("/api/models/:id", GetModelMeta)
	engine.POST("/api/models/", CreateModelMeta)
	engine.PUT("/api/models/", UpdateModelMeta)
	engine.DELETE("/api/models/:id", DeleteModelMeta)
	engine.POST("/channels", SaveSingleModelChannel)
	engine.PUT("/channels/:id", SaveSingleModelChannel)
	engine.POST("/channels/:id/confirm", ConfirmSingleModelChannel)
	engine.POST("/channels/:id/disable", DisableSingleModelChannel)
	engine.DELETE("/channels/:id", DeleteSingleModelChannel)
	call := func(method, path string, body any) map[string]any {
		t.Helper()
		encoded, err := common.Marshal(body)
		require.NoError(t, err)
		request := httptest.NewRequest(method, path, bytes.NewReader(encoded))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, request)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		var result struct {
			Success bool
			Data    map[string]any
		}
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &result))
		require.True(t, result.Success, recorder.Body.String())
		return result.Data
	}
	read := func(id int, expected ...string) {
		t.Helper()
		row := call(http.MethodGet, fmt.Sprintf("/api/models/%d", id), nil)
		want := make([]any, 0, len(expected))
		slices.Sort(expected)
		for _, endpoint := range expected {
			want = append(want, endpoint)
		}
		assert.Equal(t, want, row["supported_endpoints"], "native declaration for Model ID %d", id)
	}

	// Every built-in has an explicit native surface; no fallback to OpenAI or
	// an Offering member is permitted. These enabled fixtures have no prices.
	expected := map[protocol.Type][]string{
		protocol.OpenAIChat:                  {"openai"},
		protocol.OpenAIResponses:             {"openai-response", "openai-response-compact"},
		protocol.OpenAIRealtime:              {"openai-realtime"},
		protocol.AnthropicMessages:           {"anthropic"},
		protocol.GeminiGenerate:              {"gemini"},
		protocol.OpenAIEmbeddings:            {"embeddings"},
		protocol.GeminiEmbed:                 {"gemini-embed"},
		protocol.OpenAIImages:                {"image-generation"},
		protocol.OpenAIAudioSpeech:           {"openai-audio-speech"},
		protocol.OpenAIAudioTranscription:    {"openai-audio-transcription"},
		protocol.Rerank:                      {"jina-rerank"},
		protocol.AlphaSearch:                 {"openai-alpha-search"},
		protocol.DeepSeekTools:               {"deepseek-tools-search", "deepseek-tools-fetch"},
		protocol.OpenAIModerations:           {"openai-moderations"},
		protocol.Type("task:native-fixture"): {"task:native-fixture"},
	}
	for _, kind := range append(protocol.Builtins(), protocol.Type("task:native-fixture")) {
		t.Run(string(kind), func(t *testing.T) {
			want, exists := expected[kind]
			require.True(t, exists, "new protocols require an explicit presentation contract")
			metadata := model.Model{ModelName: "native-" + string(kind), Status: 1}
			require.NoError(t, metadata.Insert())
			require.NoError(t, db.Create(&model.Channel{ProviderID: provider.Id, ModelID: metadata.Id, Protocol: kind, UpstreamModelName: "same-upstream-name", Status: model.ChannelEnabled, MappingConfirmed: true, ParameterOverrides: "{}"}).Error)
			read(metadata.Id, want...)
		})
	}

	metadata := model.Model{ModelName: "stable-native", Status: 1, Endpoints: `{"custom":"/custom","openai-response":{"path":"/custom/responses","method":"POST"}}`}
	require.NoError(t, metadata.Insert())
	pattern := model.Model{ModelName: "stable-", NameRule: model.NameRulePrefix, Status: 1, Endpoints: `["declared-only","declared-only"]`}
	require.NoError(t, pattern.Insert())
	// Poison the old name-keyed protocol cache with a route that does not own
	// any new Channel. Neither prefix rules nor stale abilities may contribute.
	require.NoError(t, db.Create(&model.Ability{Model: metadata.ModelName, ChannelId: provider.Id, Group: "default", Enabled: true}).Error)
	read(metadata.Id, "custom", "openai-response")
	read(pattern.Id, "declared-only")

	created := call(http.MethodPost, "/channels", map[string]any{"provider_id": provider.Id, "model_id": metadata.Id, "protocol": protocol.AnthropicMessages, "upstream_model_name": "mutable-upstream"})
	channelID := int(created["id"].(float64))
	channelPath := fmt.Sprintf("/channels/%d", channelID)
	read(metadata.Id, "custom", "openai-response") // pending
	call(http.MethodPost, channelPath+"/confirm", nil)
	read(metadata.Id, "custom", "openai-response") // confirmed, disabled
	// Administrative declarations must not load a purchase price or require an
	// Offering. Persist an enabled fixture without invoking the price gate.
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", channelID).Update("status", model.ChannelEnabled).Error)
	read(metadata.Id, "anthropic", "custom", "openai-response")
	read(pattern.Id, "declared-only")

	var priceReads, nativeReads atomic.Int64
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("native_metadata_price_probe", func(tx *gorm.DB) {
		if tx.Statement.Table == "price_versions" || tx.Statement.Table == "vendors" || tx.Statement.Table == "model_offerings" {
			priceReads.Add(1)
		}
		if tx.Statement.Table == "channels" {
			nativeReads.Add(1)
		}
	}))
	t.Cleanup(func() { require.NoError(t, db.Callback().Query().Remove("native_metadata_price_probe")) })
	for _, path := range []string{"/api/models/?page_size=100", "/api/models/search?keyword=stable-native&page_size=1"} {
		priceReads.Store(0)
		nativeReads.Store(0)
		data := call(http.MethodGet, path, nil)
		found := false
		for _, item := range data["items"].([]any) {
			row := item.(map[string]any)
			if int(row["id"].(float64)) == metadata.Id {
				found = true
				assert.Equal(t, []any{"anthropic", "custom", "openai-response"}, row["supported_endpoints"])
			}
		}
		require.True(t, found)
		assert.Zero(t, priceReads.Load())
		assert.EqualValues(t, 2, nativeReads.Load(), "one batch for connection counts and one for native endpoints, never one per Model")
	}
	priceReads.Store(0)
	read(metadata.Id, "anthropic", "custom", "openai-response")
	assert.Zero(t, priceReads.Load())

	call(http.MethodPut, channelPath, map[string]any{"protocol": protocol.OpenAIResponses})
	read(metadata.Id, "custom", "openai-response") // mapping edit resets confirmation
	call(http.MethodPost, channelPath+"/confirm", nil)
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", channelID).Update("status", model.ChannelEnabled).Error)
	read(metadata.Id, "custom", "openai-response", "openai-response-compact")
	call(http.MethodPost, channelPath+"/disable", nil)
	read(metadata.Id, "custom", "openai-response")
	require.Error(t, db.Model(&model.Channel{}).Where("id = ?", channelID).Updates(map[string]any{"status": model.ChannelEnabled, "mapping_confirmed": false}).Error)
	read(metadata.Id, "custom", "openai-response") // invalid enable cannot change the declaration
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", channelID).Update("status", model.ChannelEnabled).Error)
	for _, status := range []int{common.ChannelStatusManuallyDisabled, common.ChannelStatusAutoDisabled, common.ChannelStatusEnabled} {
		require.NoError(t, db.Model(&model.Provider{}).Where("id = ?", provider.Id).Update("status", status).Error)
		if status == common.ChannelStatusEnabled {
			read(metadata.Id, "custom", "openai-response", "openai-response-compact")
		} else {
			read(metadata.Id, "custom", "openai-response")
		}
	}

	// A rename keeps ID ownership; recreating the old name cannot inherit it.
	call(http.MethodPut, "/api/models/", map[string]any{"id": metadata.Id, "model_name": "renamed-native", "status": 0, "endpoints": `["array-custom","openai-response"]`})
	read(metadata.Id, "array-custom", "openai-response", "openai-response-compact")
	other := call(http.MethodPost, "/api/models/", map[string]any{"model_name": metadata.ModelName, "status": 1})
	otherID := int(other["id"].(float64))
	read(otherID)
	call(http.MethodPut, channelPath, map[string]any{"model_id": otherID})
	call(http.MethodPost, channelPath+"/confirm", nil)
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", channelID).Update("status", model.ChannelEnabled).Error)
	read(metadata.Id, "array-custom", "openai-response")
	read(otherID, "openai-response", "openai-response-compact")
	call(http.MethodDelete, channelPath, nil)
	read(otherID)
	// Hard-deleted Providers cannot leave native declarations behind. The FK
	// first prevents deleting an account while its Channels still exist.
	require.Error(t, db.Delete(&provider).Error)
	require.NoError(t, db.Where("provider_id = ?", provider.Id).Delete(&model.Channel{}).Error)
	require.NoError(t, db.Delete(&provider).Error)
	read(otherID)
	read(metadata.Id, "array-custom", "openai-response")
	call(http.MethodDelete, fmt.Sprintf("/api/models/%d", metadata.Id), nil)
	replacement := call(http.MethodPost, "/api/models/", map[string]any{"model_name": "renamed-native", "status": 1})
	replacementID := int(replacement["id"].(float64))
	require.NotEqual(t, metadata.Id, replacementID)
	read(replacementID)
}

func TestMetadataWritesDoNotReadLegacyPricing(t *testing.T) {
	db := modelManagementDB(t)
	var legacyReads atomic.Int64
	rejectLegacyRead := func(tx *gorm.DB) {
		if tx.Statement.Table == "abilities" {
			legacyReads.Add(1)
			tx.AddError(errors.New("legacy abilities are unavailable to metadata operations"))
		}
	}
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("metadata_without_legacy_pricing", rejectLegacyRead))
	require.NoError(t, db.Callback().Row().Before("gorm:row").Register("metadata_without_legacy_pricing", rejectLegacyRead))
	t.Cleanup(func() {
		require.NoError(t, db.Callback().Query().Remove("metadata_without_legacy_pricing"))
		require.NoError(t, db.Callback().Row().Remove("metadata_without_legacy_pricing"))
	})
	request := func(t *testing.T, handler gin.HandlerFunc, method, path string, id int, body any) int {
		t.Helper()
		before := legacyReads.Load()
		var response struct {
			Success bool
			Data    struct{ ID int }
		}
		recorder := modelManagementRequest(t, func(c *gin.Context) {
			if id > 0 {
				c.Params = gin.Params{{Key: "id", Value: strconv.Itoa(id)}}
			}
			handler(c)
		}, method, path, body, &response)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		require.True(t, response.Success, recorder.Body.String())
		assert.Equal(t, before, legacyReads.Load(), "%s %s must not depend on legacy abilities", method, path)
		return response.Data.ID
	}

	publisherID := request(t, CreateVendorMeta, http.MethodPost, "/api/vendors", 0, map[string]any{"name": "Independent metadata publisher"})
	require.Positive(t, publisherID)
	request(t, UpdateVendorMeta, http.MethodPut, "/api/vendors", 0, map[string]any{"id": publisherID, "name": "Renamed metadata publisher", "description": "Current publisher"})
	publisher, err := model.GetVendorByID(publisherID)
	require.NoError(t, err)
	assert.Equal(t, "Renamed metadata publisher", publisher.Name)
	assert.Equal(t, "Current publisher", publisher.Description)

	for _, tc := range []struct {
		name, path, nameKey, publisherKey string
		create, update, remove            gin.HandlerFunc
	}{
		{"metadata", "/api/models", "model_name", "vendor_id", CreateModelMeta, UpdateModelMeta, DeleteModelMeta},
		{"declarations", "/api/admin/v1/models", "name", "publisher_id", AdminCreateModel, AdminUpdateModel, AdminDeleteModel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{tc.nameKey: "independent-" + tc.name, tc.publisherKey: publisherID, "description": "Before edit", "status": 1, "sync_official": 1, "endpoints": `["openai"]`}
			id := request(t, tc.create, http.MethodPost, tc.path, 0, body)
			require.Positive(t, id)
			body["id"], body["description"] = id, "After edit"
			request(t, tc.update, http.MethodPut, tc.path+"/"+strconv.Itoa(id), id, body)
			stored, err := model.GetExactModel(id)
			require.NoError(t, err)
			assert.Equal(t, body[tc.nameKey], stored.ModelName)
			assert.Equal(t, publisherID, stored.VendorID)
			assert.Equal(t, "After edit", stored.Description)
			assert.Equal(t, `["openai"]`, stored.Endpoints)
			if tc.name == "metadata" {
				request(t, UpdateModelMeta, http.MethodPut, tc.path+"?status_only=true", id, map[string]any{"id": id, "status": 0})
				stored, err = model.GetExactModel(id)
				require.NoError(t, err)
				assert.Zero(t, stored.Status)
				assert.Equal(t, "After edit", stored.Description)
			}
			request(t, tc.remove, http.MethodDelete, tc.path+"/"+strconv.Itoa(id), id, nil)
			_, err = model.GetExactModel(id)
			assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
		})
	}

	t.Run("synchronization_and_publisher_assignment", func(t *testing.T) {
		before := legacyReads.Load()
		update := model.MetadataSyncUpdate{
			MetadataSyncSelection: model.MetadataSyncSelection{ModelName: "independent-synchronized", RecordVersion: model.MetadataRecordVersion(nil, nil, nil), Create: true},
			Values:                model.MetadataValues{Description: "Synchronized metadata", Status: 1, Endpoints: `["openai"]`},
		}
		result, err := model.ApplyMetadataSync([]model.MetadataSyncUpdate{update}, nil)
		require.NoError(t, err)
		assert.Equal(t, []string{update.ModelName}, result.CreatedModels)
		assert.Equal(t, before, legacyReads.Load(), "synchronization must not read legacy abilities")
		var stored model.Model
		require.NoError(t, db.Where("model_name = ?", update.ModelName).First(&stored).Error)
		assert.Equal(t, update.Values.Description, stored.Description)
		operation := model.VendorOperation{Action: "assign", ModelIDs: []int{stored.Id}, TargetVendorID: publisherID}
		preview, err := model.PreviewVendorOperation(operation)
		require.NoError(t, err)
		operation.ExpectedVersion = preview.Version
		request(t, ApplyVendorOperation, http.MethodPost, "/api/vendors/operations", 0, operation)
		assigned, err := model.GetExactModel(stored.Id)
		require.NoError(t, err)
		assert.Equal(t, publisherID, assigned.VendorID)
		assert.Equal(t, update.Values.Description, assigned.Description)
		request(t, BatchDeleteModelMeta, http.MethodPost, "/api/models/delete", 0, map[string]any{"model_ids": []int{stored.Id}})
		_, err = model.GetExactModel(stored.Id)
		assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
	})
	request(t, DeleteVendorMeta, http.MethodDelete, "/api/vendors/"+strconv.Itoa(publisherID), publisherID, nil)
	_, err = model.GetVendorByID(publisherID)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestMetadataDeletionPreservesNativeSalesAndHistoricalSources(t *testing.T) {
	db := modelManagementDB(t)
	record := model.Model{ModelName: "metadata-delete-cache", Description: "Before deletion", Status: 1}
	require.NoError(t, record.Insert())
	provider := model.Provider{Name: "Historical source", Type: 1, Status: common.ChannelStatusEnabled}
	require.NoError(t, db.Create(&provider).Error)
	ability := model.Ability{ChannelId: provider.Id, Model: record.ModelName, Group: "default", Enabled: true}
	require.NoError(t, db.Create(&ability).Error)
	require.NoError(t, db.Create(&model.Option{Key: "ModelPrice", Value: `{"metadata-delete-cache":0}`}).Error)
	owner := modelManagementPriceOwner(t, db, "metadata-sale-retained", protocol.OpenAIChat)
	modelManagementSale(t, db, owner.ModelID, "metadata-sale")
	sales, prices, options := modelManagementSales(t, db), modelManagementPrices(t, db), modelManagementOptions(t, db)
	require.Len(t, sales, 1)
	before, err := model.GetExactModel(record.Id)
	require.NoError(t, err)
	assert.Equal(t, record.Description, before.Description)
	result, err := model.DeleteModelMetadata([]int{record.Id})
	require.NoError(t, err)
	assert.Equal(t, 1, result.DeletedCount)
	_, err = model.GetExactModel(record.Id)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	var response struct {
		Success bool
		Data    struct {
			Items []model.Model
			Total int
		}
	}
	modelManagementRequest(t, SearchModelsMeta, http.MethodGet, "/api/models/search?keyword=metadata-delete-cache", nil, &response)
	require.True(t, response.Success)
	assert.Empty(t, response.Data.Items)
	assert.Zero(t, response.Data.Total, "deleted metadata must not reappear through historical Ability or Option data")
	assert.Equal(t, sales, modelManagementSales(t, db))
	assert.Equal(t, prices, modelManagementPrices(t, db))
	assert.Equal(t, options, modelManagementOptions(t, db))
	var preserved model.Ability
	require.NoError(t, db.First(&preserved).Error)
	assert.Equal(t, ability, preserved)
}

func TestModelManagementDatabase(t *testing.T) {
	_, err := jsplugin.DefaultRegistry.Register(`
export const meta = {apiVersion: 1, key: "model-management-task", name: "Management task fixture", version: "1.0.0", author: {name: "Test"}, models: ["matrix-task"], fetchMode: "per_task", usageSchema: {seconds: {type: "number", unit: "second"}}};
export function buildSubmitRequest() { return {}; }
export function parseSubmitResponse() { return {}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {}; }
`, jsplugin.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { jsplugin.DefaultRegistry.Unregister("model-management-task") })
	db := modelManagementDB(t)

	t.Run("square_states_follow_stable_channel_status", func(t *testing.T) {
		records := []model.Model{
			{ModelName: "square-visible", Status: 1},
			{ModelName: "square-hidden", Status: 0},
			{ModelName: "square-catalog", Status: 1},
			{ModelName: "square-hidden-catalog", Status: 0},
			{ModelName: "square-partial-", NameRule: model.NameRulePrefix, Status: 1},
			{ModelName: "square-partial-hidden", Status: 0},
			{ModelName: "square-hidden-", NameRule: model.NameRulePrefix, Status: 0},
			{ModelName: "square-hidden-override", Status: 1},
			{ModelName: "square-off-", NameRule: model.NameRulePrefix, Status: 0},
			{ModelName: "square-empty", NameRule: model.NameRulePrefix, Status: 1},
			{ModelName: "square-empty-hidden", NameRule: model.NameRulePrefix, Status: 0},
			{ModelName: "-ending", NameRule: model.NameRuleSuffix, Status: 1},
			{ModelName: "square-prefix-", NameRule: model.NameRulePrefix, Status: 0},
			{ModelName: "square-contains", NameRule: model.NameRuleContains, Status: 0},
		}
		ids := make([]int, 0, len(records))
		for i := range records {
			require.NoError(t, records[i].Insert())
			ids = append(ids, records[i].Id)
		}
		active := model.Provider{Name: "Square active", Type: 1, Key: "fixture", Group: "default", Status: common.ChannelStatusEnabled,
			Models: "square-visible,square-hidden,square-bare,square-partial-on,square-partial-hidden,square-hidden-child,square-hidden-override,square-prefix-ending,square-contains-ending"}
		inactive := model.Provider{Name: "Square inactive", Type: 1, Key: "fixture", Group: "default", Status: common.ChannelStatusManuallyDisabled,
			Models: "square-disabled,square-partial-off,square-off-child"}
		for _, channel := range []*model.Provider{&active, &inactive} {
			require.NoError(t, channel.Insert())
			require.NoError(t, channel.AddAbilities(nil))
		}
		connections := []model.Channel{
			{ProviderID: active.Id, ModelID: records[0].Id, Protocol: "openai_chat", UpstreamModelName: "visible", ParameterOverrides: "{}", Status: model.ChannelEnabled, MappingConfirmed: true},
			{ProviderID: active.Id, ModelID: records[1].Id, Protocol: "openai_chat", UpstreamModelName: "hidden", ParameterOverrides: "{}", Status: model.ChannelEnabled, MappingConfirmed: true},
			{ProviderID: inactive.Id, ModelID: records[2].Id, Protocol: "openai_chat", UpstreamModelName: "disabled", ParameterOverrides: "{}", Status: model.ChannelEnabled, MappingConfirmed: true},
			{ProviderID: active.Id, ModelID: records[7].Id, Protocol: "openai_chat", UpstreamModelName: "override", ParameterOverrides: "{}", Status: model.ChannelEnabled, MappingConfirmed: true},
		}
		require.NoError(t, db.Create(&connections).Error)
		t.Cleanup(func() {
			require.NoError(t, db.Where("model_id IN ?", ids).Delete(&model.Channel{}).Error)
			require.NoError(t, db.Where("channel_id IN ?", []int{active.Id, inactive.Id}).Delete(&model.Ability{}).Error)
			require.NoError(t, db.Where("id IN ?", []int{active.Id, inactive.Id}).Delete(&model.Provider{}).Error)
			require.NoError(t, db.Unscoped().Where("id IN ?", ids).Delete(&model.Model{}).Error)
		})
		var response struct {
			Success bool
			Data    struct{ Items []model.Model }
		}
		modelManagementRequest(t, SearchModelsMeta, "GET", "/api/models/search?include_channel_models=true&keyword=square-&page_size=100", nil, &response)
		require.True(t, response.Success)
		byName := make(map[string]model.Model)
		for _, row := range response.Data.Items {
			byName[row.ModelName] = row
		}
		expected := map[string]model.ModelSquareState{
			"square-visible":         model.ModelSquareVisible,
			"square-hidden":          model.ModelSquareHidden,
			"square-catalog":         model.ModelSquareUnavailable,
			"square-hidden-catalog":  model.ModelSquareHidden,
			"square-partial-":        model.ModelSquareUnavailable,
			"square-partial-hidden":  model.ModelSquareHidden,
			"square-hidden-":         model.ModelSquareHidden,
			"square-hidden-override": model.ModelSquareVisible,
			"square-off-":            model.ModelSquareHidden,
			"square-empty":           model.ModelSquareUnavailable,
			"square-empty-hidden":    model.ModelSquareHidden,
			"square-prefix-":         model.ModelSquareHidden,
			"square-contains":        model.ModelSquareHidden,
		}
		for name, state := range expected {
			row, exists := byName[name]
			require.True(t, exists, name)
			assert.Equal(t, state, row.SquareState, name)
		}
		assert.NotContains(t, byName, "square-bare")
		assert.NotContains(t, byName, "square-hidden-child")
		t.Run("filter_square_state_before_pagination", func(t *testing.T) {
			type filteredResponse struct {
				Success bool
				Data    struct {
					Items []model.Model
					Total int
				}
			}
			for _, state := range []model.ModelSquareState{model.ModelSquareVisible, model.ModelSquareUnavailable, model.ModelSquareHidden, model.ModelSquarePartial} {
				t.Run(string(state), func(t *testing.T) {
					expectedNames := []string{}
					for name, expectedState := range expected {
						if expectedState == state {
							expectedNames = append(expectedNames, name)
						}
					}
					actualNames := []string{}
					for page := 1; page <= (len(expectedNames)+1)/2; page++ {
						var result filteredResponse
						path := fmt.Sprintf("/api/models/search?include_channel_models=true&keyword=square-&square_state=%s&page_size=2&p=%d", state, page)
						modelManagementRequest(t, SearchModelsMeta, "GET", path, nil, &result)
						require.True(t, result.Success)
						assert.Equal(t, len(expectedNames), result.Data.Total)
						for _, row := range result.Data.Items {
							assert.Equal(t, state, row.SquareState)
							actualNames = append(actualNames, row.ModelName)
						}
					}
					assert.ElementsMatch(t, expectedNames, actualNames)
					var beyondLastPage filteredResponse
					modelManagementRequest(t, SearchModelsMeta, "GET", "/api/models/search?include_channel_models=true&keyword=square-&square_state="+string(state)+"&page_size=2&p=99", nil, &beyondLastPage)
					require.True(t, beyondLastPage.Success)
					assert.Equal(t, len(expectedNames), beyondLastPage.Data.Total)
					assert.Empty(t, beyondLastPage.Data.Items)
				})
			}
			for _, test := range []struct {
				name    string
				handler gin.HandlerFunc
				query   string
				names   []string
			}{
				{"list", GetAllModelsMeta, "?include_channel_models=true&square_state=visible", []string{"square-visible", "square-hidden-override"}},
				{"metadata_only", SearchModelsMeta, "?keyword=square-&square_state=visible", []string{"square-visible", "square-hidden-override"}},
				{"keyword", SearchModelsMeta, "?include_channel_models=true&keyword=square-partial&square_state=hidden", []string{"square-partial-hidden"}},
				{"policy", SearchModelsMeta, "?include_channel_models=true&keyword=square-&square_state=partial&status=disabled", []string{}},
				{"vendor_and_sync", SearchModelsMeta, "?include_channel_models=true&keyword=square-&square_state=partial&vendor=0&sync_official=no", []string{}},
				{"no_matches", SearchModelsMeta, "?include_channel_models=true&keyword=square-bare&square_state=hidden", []string{}},
			} {
				t.Run(test.name, func(t *testing.T) {
					var result filteredResponse
					modelManagementRequest(t, test.handler, "GET", "/api/models/"+test.query+"&page_size=100", nil, &result)
					require.True(t, result.Success)
					names := []string{}
					for _, row := range result.Data.Items {
						names = append(names, row.ModelName)
					}
					assert.Equal(t, len(test.names), result.Data.Total)
					assert.ElementsMatch(t, test.names, names)
				})
			}
			for _, handler := range []gin.HandlerFunc{GetAllModelsMeta, SearchModelsMeta} {
				var result filteredResponse
				recorder := modelManagementRequest(t, handler, "GET", "/api/models/?square_state=unknown", nil, &result)
				assert.Equal(t, http.StatusBadRequest, recorder.Code)
				assert.False(t, result.Success)
			}
			for _, query := range []string{"p=-1", "page_size=-1"} {
				recorder := modelManagementRequest(t, SearchModelsMeta, "GET", "/api/models/search?square_state=visible&"+query, nil, nil)
				assert.Equal(t, http.StatusBadRequest, recorder.Code)
			}
			var oversizedPage filteredResponse
			modelManagementRequest(t, SearchModelsMeta, "GET", "/api/models/search?square_state=visible&keyword=square-&p="+strconv.Itoa(int(^uint(0)>>1)), nil, &oversizedPage)
			require.True(t, oversizedPage.Success)
			assert.Equal(t, 2, oversizedPage.Data.Total)
			assert.Empty(t, oversizedPage.Data.Items)
		})
		var detail struct {
			Success bool
			Data    model.Model
		}
		modelManagementRequest(t, func(c *gin.Context) {
			c.Params = gin.Params{{Key: "id", Value: strconv.Itoa(records[4].Id)}}
			GetModelMeta(c)
		}, "GET", "/api/models/"+strconv.Itoa(records[4].Id), nil, &detail)
		require.True(t, detail.Success)
		assert.Equal(t, model.ModelSquareUnavailable, detail.Data.SquareState)
	})
	t.Run("channel_model_listing", func(t *testing.T) {
		exact := model.Model{ModelName: "listing-exact", Status: 1, SyncOfficial: 1}
		catalog := model.Model{ModelName: "listing-catalog", Status: 1}
		rule := model.Model{ModelName: "listing-rule-", NameRule: model.NameRulePrefix, Status: 1}
		for _, item := range []*model.Model{&exact, &catalog, &rule} {
			require.NoError(t, item.Insert())
		}
		active := model.Provider{Name: "Listing active", Type: 1, Key: "fixture", Models: "listing-exact, listing-new,listing-rule-child,listing-new, ,", Group: "default", Status: common.ChannelStatusEnabled}
		inactive := model.Provider{Name: "Listing inactive", Type: 1, Key: "fixture", Models: "listing-new,listing-disabled", Group: "default", Status: common.ChannelStatusManuallyDisabled}
		for _, channel := range []*model.Provider{&active, &inactive} {
			require.NoError(t, channel.Insert())
			require.NoError(t, channel.AddAbilities(nil))
		}
		connections := []model.Channel{
			{ProviderID: active.Id, ModelID: exact.Id, Protocol: "openai_chat", UpstreamModelName: "exact", ParameterOverrides: "{}", Status: model.ChannelEnabled, MappingConfirmed: true},
			{ProviderID: inactive.Id, ModelID: exact.Id, Protocol: "openai_chat", UpstreamModelName: "disabled", ParameterOverrides: "{}", Status: model.ChannelEnabled, MappingConfirmed: true},
		}
		require.NoError(t, db.Create(&connections).Error)
		t.Cleanup(func() {
			require.NoError(t, db.Where("model_id = ?", exact.Id).Delete(&model.Channel{}).Error)
			require.NoError(t, db.Where("channel_id IN ?", []int{active.Id, inactive.Id}).Delete(&model.Ability{}).Error)
			require.NoError(t, db.Where("id IN ?", []int{active.Id, inactive.Id}).Delete(&model.Provider{}).Error)
			require.NoError(t, db.Unscoped().Where("model_name LIKE ?", "listing-%").Delete(&model.Model{}).Error)
		})
		type listingResponse struct {
			Success bool
			Data    struct {
				Items []model.Model
				Total int
			}
		}
		var response listingResponse
		modelManagementRequest(t, SearchModelsMeta, "GET", "/api/models/search?include_channel_models=true&keyword=listing-", nil, &response)
		require.True(t, response.Success)
		require.Equal(t, 3, response.Data.Total)
		names := make([]string, 0)
		byName := make(map[string]model.Model)
		for _, item := range response.Data.Items {
			names = append(names, item.ModelName)
			byName[item.ModelName] = item
		}
		assert.Equal(t, []string{"listing-rule-", "listing-catalog", "listing-exact"}, names)
		assert.True(t, byName[exact.ModelName].HasMetadata)
		assert.NotContains(t, byName, "listing-new")
		assert.NotContains(t, byName, "listing-disabled")
		assert.NotContains(t, byName, "listing-rule-child")
		assert.Equal(t, 2, byName[exact.ModelName].ConfiguredChannelCount)
		assert.Equal(t, 1, byName[exact.ModelName].EnabledChannelCount)
		assert.Zero(t, byName[rule.ModelName].ConfiguredChannelCount)
		assert.Zero(t, byName[catalog.ModelName].ConfiguredChannelCount)
		for _, tc := range []struct {
			query string
			total int
			names []string
		}{
			{"&p=2&page_size=2", 3, []string{"listing-exact"}},
			{"&p=9&page_size=2", 3, []string{}},
			{"&status=enabled", 3, []string{"listing-rule-", "listing-catalog", "listing-exact"}},
			{"&sync_official=no", 2, []string{"listing-rule-", "listing-catalog"}},
			{"&vendor=0", 3, []string{"listing-rule-", "listing-catalog", "listing-exact"}},
			{"&vendor=999", 0, []string{}},
		} {
			var page listingResponse
			modelManagementRequest(t, SearchModelsMeta, "GET", "/api/models/search?include_channel_models=true&keyword=listing-"+tc.query, nil, &page)
			require.True(t, page.Success)
			assert.Equal(t, tc.total, page.Data.Total)
			actual := make([]string, 0)
			for _, item := range page.Data.Items {
				actual = append(actual, item.ModelName)
			}
			assert.Equal(t, tc.names, actual)
		}
		var legacy listingResponse
		modelManagementRequest(t, SearchModelsMeta, "GET", "/api/models/search?keyword=listing-", nil, &legacy)
		assert.Equal(t, 3, legacy.Data.Total)
		var count int64
		require.NoError(t, db.Model(&model.Model{}).Where("model_name LIKE ?", "listing-%").Count(&count).Error)
		assert.EqualValues(t, 3, count)
		// Historical price-only names do not create native metadata records.
		require.NoError(t, db.Create(&model.Option{Key: "ModelPrice", Value: `{"listing-new":0}`}).Error)
		priceOnly := modelManagementOptions(t, db)
		t.Cleanup(func() { require.NoError(t, db.Where(`"key" = ?`, "ModelPrice").Delete(&model.Option{}).Error) })
		require.NoError(t, db.Model(&model.Model{}).Where("model_name = ?", "listing-new").Count(&count).Error)
		assert.Zero(t, count)
		saleOwner := modelManagementPriceOwner(t, db, "sales-listed-member", protocol.OpenAIChat)
		offering, salePrice := modelManagementSale(t, db, saleOwner.ModelID, "sales-listed")
		sales := modelManagementSales(t, db)
		require.Len(t, sales, 1)
		assert.Equal(t, offering.ID, sales[0].ID)
		require.NotNil(t, sales[0].Price)
		assert.Equal(t, salePrice.ID, sales[0].Price.ID)
		assert.Equal(t, salePrice.Expression, sales[0].Price.Expression)
		require.Len(t, sales[0].Members, 1)
		assert.Equal(t, saleOwner.ModelID, sales[0].Members[0].ModelID)
		assert.NotEqual(t, catalog.ModelName, sales[0].Members[0].Name)
		assert.NotEqual(t, "listing-new", sales[0].Members[0].Name)
		priceHistory := modelManagementPrices(t, db)
		created := model.Model{ModelName: "listing-new", Status: 1}
		require.NoError(t, created.Insert())
		var after listingResponse
		modelManagementRequest(t, SearchModelsMeta, "GET", "/api/models/search?include_channel_models=true&keyword=listing-new", nil, &after)
		require.Len(t, after.Data.Items, 1)
		assert.Equal(t, created.Id, after.Data.Items[0].Id)
		require.NoError(t, created.Delete())
		after = listingResponse{}
		modelManagementRequest(t, SearchModelsMeta, "GET", "/api/models/search?include_channel_models=true&keyword=listing-new", nil, &after)
		require.Empty(t, after.Data.Items)
		assert.Zero(t, after.Data.Total)
		assert.Equal(t, priceOnly, modelManagementOptions(t, db), "metadata create/delete preserves even an explicit historical zero")
		assert.Equal(t, sales, modelManagementSales(t, db))
		assert.Equal(t, priceHistory, modelManagementPrices(t, db))
		require.NoError(t, db.Callback().Query().Before("gorm:query").Register("fail_listing_channels", func(tx *gorm.DB) {
			if tx.Statement.Table == "channels" {
				tx.AddError(errors.New("channel lookup failed"))
			}
		}))
		var failed listingResponse
		modelManagementRequest(t, GetAllModelsMeta, "GET", "/api/models/?include_channel_models=true", nil, &failed)
		require.NoError(t, db.Callback().Query().Remove("fail_listing_channels"))
		assert.False(t, failed.Success, "a channel lookup failure must not look like an empty configuration")

	})

	t.Run("immutable_prices_preserve_zero_and_reject_conflicting_versions", func(t *testing.T) {
		owner := modelManagementPriceOwner(t, db, "matrix-priced", protocol.OpenAIChat)
		other := modelManagementPriceOwner(t, db, "matrix-other", protocol.OpenAIChat)
		actor := modelManagementPriceActor(t, db)
		options := modelManagementOptions(t, db)
		free := modelManagementPrice(t, db, owner, `tier("request", fixed(0))`, 1)
		separate := modelManagementPrice(t, db, other, `tier("base", p * 2 + cc * 2.5)`, 1)
		paid := modelManagementPrice(t, db, owner, `tier("base", p * 2 + c * 8 + cr * 0 + cc * 2.5)`, 2)
		for _, expected := range []model.PriceVersion{free, paid, separate} {
			current, err := model.CurrentPriceVersion(context.Background(), db, expected.OwnerType, expected.OwnerID, expected.EffectiveAt)
			require.NoError(t, err)
			assert.Equal(t, expected.ID, current.ID)
			assert.Equal(t, expected.Expression, current.Expression)
		}
		loaded := modelManagementPrices(t, db)
		duplicate := model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: owner.ID, Currency: money.Currency("USD"), Expression: `tier("request", fixed(9))`, EffectiveAt: 2}
		require.ErrorContains(t, model.CreatePriceVersion(context.Background(), &duplicate, actor), "23505")
		invalid := model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: other.ID, Currency: money.Currency("USD"), Expression: `tier("request", fixed(-1))`, EffectiveAt: 3}
		require.Error(t, model.CreatePriceVersion(context.Background(), &invalid, actor))
		assert.Equal(t, loaded, modelManagementPrices(t, db))
		// An actual SQL error after INSERT must roll back the new history row.
		require.NoError(t, db.Callback().Create().After("gorm:create").Register("fail_pricing_matrix", func(tx *gorm.DB) {
			if tx.Statement.Table == "price_versions" {
				tx.AddError(tx.Session(&gorm.Session{NewDB: true}).Exec("SELECT 1/0").Error)
			}
		}))
		proposal := model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: other.ID, Currency: money.Currency("USD"), Expression: `tier("request", fixed(9))`, EffectiveAt: 3}
		err := model.CreatePriceVersion(context.Background(), &proposal, actor)
		require.NoError(t, db.Callback().Create().Remove("fail_pricing_matrix"))
		require.ErrorContains(t, err, "22012", "failure must come from PostgreSQL after the inserted price, not validation or query construction")
		assert.Equal(t, loaded, modelManagementPrices(t, db))
		// Concurrent appends at one effective instant have exactly one winner.
		var wg sync.WaitGroup
		results := make(chan error, 2)
		for _, expression := range []string{`tier("request", fixed(3))`, `tier("request", fixed(4))`} {
			wg.Go(func() {
				proposal := model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: other.ID, Currency: money.Currency("USD"), Expression: expression, EffectiveAt: 3}
				results <- model.CreatePriceVersion(context.Background(), &proposal, actor)
			})
		}
		wg.Wait()
		close(results)
		successes, conflicts := 0, 0
		for err := range results {
			if err == nil {
				successes++
			} else {
				require.ErrorContains(t, err, "23505")
				conflicts++
			}
		}
		assert.Equal(t, 1, successes)
		assert.Equal(t, 1, conflicts)
		winner, err := model.CurrentPriceVersion(context.Background(), db, model.PriceOwnerChannel, other.ID, 3)
		require.NoError(t, err)
		assert.Contains(t, []string{`tier("request", fixed(3))`, `tier("request", fixed(4))`}, winner.Expression)
		require.Error(t, db.Model(&model.PriceVersion{}).Where("id = ?", free.ID).Update("expression", `tier("request", fixed(7))`).Error)
		require.Error(t, db.Delete(&model.PriceVersion{}, free.ID).Error)
		require.Error(t, db.Exec("TRUNCATE price_versions").Error)
		assert.Equal(t, loaded, modelManagementPrices(t, db)[:len(loaded)], "all prior versions remain byte-for-byte unchanged")
		assert.Equal(t, options, modelManagementOptions(t, db))
	})
	t.Run("task_usage_and_offline_builtin_defaults", func(t *testing.T) {
		plugin, ok := jsplugin.DefaultRegistry.Generation().GetByModel("matrix-task")
		require.True(t, ok)
		schema, _ := plugin.Meta.UsageForModel("matrix-task")
		assert.Contains(t, schema, "seconds")
		owner := modelManagementPriceOwner(t, db, "matrix-task", protocol.Type("task:"+plugin.Meta.Key))
		expression := `tier("base", u("seconds") * 0.25)`
		price := modelManagementPrice(t, db, owner, expression, 1)
		before := modelManagementPrices(t, db)
		invalid := model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: owner.ID, Currency: money.Currency("USD"), Expression: `tier("base", u("undeclared") * 1)`, EffectiveAt: 2}
		require.Error(t, model.CreatePriceVersion(context.Background(), &invalid, price.CreatedBy))
		assert.Equal(t, before, modelManagementPrices(t, db))
		const name = "gpt-6-astra"
		builtin, exists := billing_setting.GetBuiltinBillingExpr(name)
		require.True(t, exists)
		declareLegacyProviderRoutingColumns(t, db)
		source := legacyPricingSource(t, db)
		options := modelManagementOptions(t, db)
		custom, err := source.Preview(name, model.PricingValues{"ModelPrice": float64(0)})
		require.NoError(t, err)
		assert.Equal(t, `tier("request", fixed(0))`, custom.Expression)
		// Choosing a builtin is now an explicit immutable owner version, not a
		// reset that deletes stored model-name prices or invents a conversion.
		builtinOwner := modelManagementPriceOwner(t, db, name, protocol.OpenAIChat)
		customPrice := modelManagementPrice(t, db, builtinOwner, custom.Expression, 1)
		builtinPrice := modelManagementPrice(t, db, builtinOwner, builtin, 2)
		for _, expected := range []model.PriceVersion{customPrice, builtinPrice} {
			current, err := model.CurrentPriceVersion(context.Background(), db, expected.OwnerType, expected.OwnerID, expected.EffectiveAt)
			require.NoError(t, err)
			assert.Equal(t, expected.ID, current.ID)
			assert.Equal(t, expected.Expression, current.Expression)
		}
		assert.Equal(t, options, modelManagementOptions(t, db))
		assert.Equal(t, before, modelManagementPrices(t, db)[:len(before)])
	})
	t.Run("concurrent_import_creates_one_record", func(t *testing.T) {
		update := model.MetadataSyncUpdate{MetadataSyncSelection: model.MetadataSyncSelection{ModelName: "matrix-concurrent-import", RecordVersion: model.MetadataRecordVersion(nil, nil, nil), Create: true}, Values: model.MetadataValues{Description: "Imported", Status: 1}}
		var wg sync.WaitGroup
		results := make(chan error, 2)
		for range 2 {
			wg.Go(func() {
				_, err := model.ApplyMetadataSync([]model.MetadataSyncUpdate{update}, nil)
				results <- err
			})
		}
		wg.Wait()
		close(results)
		successes, conflicts := 0, 0
		for err := range results {
			if err == nil {
				successes++
			} else if errors.Is(err, model.ErrMetadataSyncConflict) {
				conflicts++
			} else {
				require.NoError(t, err)
			}
		}
		assert.Equal(t, 1, successes)
		assert.Equal(t, 1, conflicts)
		var count int64
		require.NoError(t, db.Model(&model.Model{}).Where("model_name = ?", update.ModelName).Count(&count).Error)
		assert.EqualValues(t, 1, count)
	})
	t.Run("metadata_keeps_pricing_and_channel_identity", func(t *testing.T) {
		active := model.Provider{Name: "Active route", Type: 1, Status: common.ChannelStatusEnabled}
		disabled := model.Provider{Name: "Disabled route", Type: 1, Status: common.ChannelStatusManuallyDisabled}
		require.NoError(t, db.Create(&active).Error)
		require.NoError(t, db.Create(&disabled).Error)
		require.NoError(t, db.Create(&[]model.Ability{
			{Model: "matrix-hidden-unpriced", Group: "available", ChannelId: active.Id, Enabled: true},
			{Model: "matrix-hidden-unpriced", Group: "disabled", ChannelId: disabled.Id, Enabled: true},
			{Model: "matrix-hidden-unpriced", Group: "inactive", ChannelId: active.Id, Enabled: false},
		}).Error)
		exact := &model.Model{ModelName: "matrix-hidden-unpriced", Status: 0, SyncOfficial: 0}
		require.NoError(t, exact.Insert())
		rule := &model.Model{ModelName: "matrix-hidden-", NameRule: model.NameRulePrefix}
		connection := model.Channel{ProviderID: active.Id, ModelID: exact.Id, Protocol: "openai_chat", UpstreamModelName: "stable", ParameterOverrides: "{}", Status: model.ChannelEnabled, MappingConfirmed: true}
		require.NoError(t, db.Create(&connection).Error)
		require.NoError(t, enrichModels([]*model.Model{exact, rule}))
		assert.Empty(t, exact.EnableGroups)
		assert.Empty(t, exact.BoundChannels)
		assert.Empty(t, rule.MatchedModels)
		assert.Equal(t, 1, exact.ConfiguredChannelCount)
		assert.Equal(t, 1, exact.EnabledChannelCount)
		assert.Zero(t, rule.ConfiguredChannelCount)
		assert.Empty(t, exact.Endpoints, "inferred endpoints must not become stored configuration")
		require.NoError(t, db.Create(&model.Option{Key: "ModelPrice", Value: `{"matrix-hidden-unpriced":0}`}).Error)
		priceBefore := modelManagementOptions(t, db)
		nativeBefore := modelManagementPrices(t, db)
		exact.ModelName = "matrix-renamed"
		exact.Endpoints = `{"openai":{"path":"/v1/chat/completions","method":"POST"}}`
		response := modelManagementRequest(t, UpdateModelMeta, http.MethodPut, "/api/models/", exact, nil)
		assert.Contains(t, response.Body.String(), `"success":true`)
		var reloaded model.Model
		require.NoError(t, db.First(&reloaded, exact.Id).Error)
		require.NoError(t, enrichModels([]*model.Model{&reloaded}))
		assert.Equal(t, exact.Endpoints, reloaded.Endpoints)
		assert.Empty(t, reloaded.BoundChannels)
		assert.Equal(t, 1, reloaded.ConfiguredChannelCount)
		assert.Equal(t, 1, reloaded.EnabledChannelCount)
		assert.Equal(t, priceBefore, modelManagementOptions(t, db), "rename does not move a historical model-name price")
		assert.Equal(t, nativeBefore, modelManagementPrices(t, db))
		var stable model.Channel
		require.NoError(t, db.First(&stable, connection.ID).Error)
		assert.Equal(t, exact.Id, stable.ModelID)
		assert.Equal(t, "stable", stable.UpstreamModelName)
		require.ErrorIs(t, reloaded.Delete(), model.ErrModelHasChannels)
		require.NoError(t, db.Delete(&connection).Error)
		require.NoError(t, reloaded.Delete())
		assert.Equal(t, priceBefore, modelManagementOptions(t, db), "metadata deletion never resets historical prices")
		assert.Equal(t, nativeBefore, modelManagementPrices(t, db))
		var ability model.Ability
		require.NoError(t, db.Where("model = ? AND channel_id = ? AND enabled = ?", "matrix-hidden-unpriced", active.Id, true).First(&ability).Error)
	})
	t.Run("metadata_preview_selection_versions_and_transaction", func(t *testing.T) {
		local := &model.Model{ModelName: "matrix-existing", Description: "Local description", Tags: "keep", Status: 1, SyncOfficial: 1}
		require.NoError(t, local.Insert())
		blocked := &model.Model{ModelName: "matrix-blocked", Status: 1, SyncOfficial: 0}
		require.NoError(t, blocked.Insert())
		require.NoError(t, db.Create(&model.Ability{Model: "matrix-new", Group: "default", ChannelId: 1, Enabled: true}).Error)
		var revision atomic.Int32
		var failVendors atomic.Bool
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Contains(t, r.URL.Path, "/api/i18n/zh/newapi/")
			var payload any
			if strings.HasSuffix(r.URL.Path, "vendors.json") {
				if failVendors.Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				payload = []upstreamVendor{{Name: "Matrix vendor", Status: 1}}
			} else {
				payload = []upstreamModel{
					{ModelName: "matrix-existing", Description: fmt.Sprintf("Upstream %d", revision.Load()), VendorName: "Matrix vendor", Tags: "changed", Status: 0, Endpoints: []byte(`{"openai":{"path":"/v1/chat/completions","method":"POST"}}`)},
					{ModelName: "matrix-new", Description: "New model", VendorName: "Matrix vendor", Status: 0, Endpoints: []byte(`{"openai":"/v1/chat/completions"}`)},
					{ModelName: "matrix-blocked", Description: "Do not overwrite", Status: 1},
					{ModelName: "matrix-catalog", Description: "Catalog only", Status: 1},
				}
			}
			encoded, err := common.Marshal(payload)
			require.NoError(t, err)
			_, _ = w.Write(encoded)
		}))
		defer upstream.Close()
		t.Setenv("SYNC_UPSTREAM_BASE", upstream.URL)
		t.Setenv("SYNC_HTTP_RETRY", "1")
		var preview struct {
			Success bool
			Data    struct {
				Source     metadataSyncSource
				Candidates []metadataSyncCandidate
			}
		}
		response := modelManagementRequest(t, SyncUpstreamPreview, "GET", "/api/models/sync_upstream/preview?locale=zh", nil, &preview)
		require.Equal(t, http.StatusOK, response.Code)
		require.True(t, preview.Success, response.Body.String())
		byName := make(map[string]metadataSyncCandidate)
		for _, candidate := range preview.Data.Candidates {
			byName[candidate.ModelName] = candidate
		}
		assert.Equal(t, "site", byName["matrix-existing"].Scope)
		assert.Equal(t, "site", byName["matrix-blocked"].Scope)
		assert.Equal(t, "catalog", byName["matrix-new"].Scope, "an obsolete Ability does not make a catalog entry a configured Model")
		assert.Equal(t, "catalog", byName["matrix-catalog"].Scope)
		assert.Equal(t, "blocked", byName["matrix-blocked"].Kind)
		var count int64
		require.NoError(t, db.Model(&model.Vendor{}).Count(&count).Error)
		assert.Zero(t, count)
		body := map[string]any{"locale": "zh", "source_version": preview.Data.Source.Version, "selections": []model.MetadataSyncSelection{
			{ModelName: "matrix-existing", RecordVersion: byName["matrix-existing"].RecordVersion, Fields: []string{"description", "endpoints"}},
			{ModelName: "matrix-new", RecordVersion: byName["matrix-new"].RecordVersion, Create: true},
		}}
		beforePricing := modelManagementPrices(t, db)
		beforeOptions := modelManagementOptions(t, db)
		// A failed model insert must also undo the earlier metadata update
		// and the newly inserted supplier.
		require.NoError(t, db.Callback().Create().Before("gorm:create").Register("fail_metadata_matrix", func(tx *gorm.DB) {
			if tx.Statement.Table == "models" {
				tx.AddError(errors.New("injected metadata failure"))
			}
		}))
		response = modelManagementRequest(t, SyncUpstreamModels, "POST", "/api/models/sync_upstream", body, nil)
		assert.NotEqual(t, http.StatusOK, response.Code)
		require.NoError(t, db.Callback().Create().Remove("fail_metadata_matrix"))
		var persisted model.Model
		require.NoError(t, db.First(&persisted, local.Id).Error)
		assert.Equal(t, "Local description", persisted.Description)
		require.NoError(t, db.Model(&model.Vendor{}).Count(&count).Error)
		assert.Zero(t, count)
		var result struct {
			Success bool
			Data    model.MetadataSyncResult
		}
		response = modelManagementRequest(t, SyncUpstreamModels, "POST", "/api/models/sync_upstream", body, &result)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.True(t, result.Success)
		assert.Equal(t, []string{"matrix-new"}, result.Data.CreatedModels)
		assert.Equal(t, []string{"Matrix vendor"}, result.Data.CreatedVendors)
		require.NoError(t, db.First(&persisted, local.Id).Error)
		assert.Equal(t, "Upstream 0", persisted.Description)
		assert.Equal(t, "keep", persisted.Tags)
		assert.Equal(t, 1, persisted.Status)
		assert.Zero(t, persisted.VendorID)
		assert.JSONEq(t, `{"openai":{"path":"/v1/chat/completions","method":"POST"}}`, persisted.Endpoints)
		var created model.Model
		require.NoError(t, db.Where("model_name = ?", "matrix-new").First(&created).Error)
		assert.Equal(t, 1, created.SyncOfficial)
		assert.Zero(t, created.Status)
		assert.Equal(t, beforePricing, modelManagementPrices(t, db))
		assert.Equal(t, beforeOptions, modelManagementOptions(t, db))
		response = modelManagementRequest(t, SyncUpstreamModels, "POST", "/api/models/sync_upstream", body, nil)
		assert.Equal(t, http.StatusConflict, response.Code)
		response = modelManagementRequest(t, SyncUpstreamModels, "POST", "/api/models/sync_upstream", map[string]any{}, nil)
		assert.Equal(t, http.StatusBadRequest, response.Code)
		body["selections"] = []model.MetadataSyncSelection{{ModelName: "matrix-blocked", RecordVersion: byName["matrix-blocked"].RecordVersion, Fields: []string{"description"}}}
		response = modelManagementRequest(t, SyncUpstreamModels, "POST", "/api/models/sync_upstream", body, nil)
		assert.Equal(t, http.StatusBadRequest, response.Code)
		revision.Add(1)
		response = modelManagementRequest(t, SyncUpstreamModels, "POST", "/api/models/sync_upstream", body, nil)
		assert.Equal(t, http.StatusConflict, response.Code)
		failVendors.Store(true)
		modelManagementRequest(t, SyncUpstreamPreview, "GET", "/api/models/sync_upstream/preview?locale=zh", nil, &preview)
		assert.False(t, preview.Success)
	})
}

func TestMetadataSyncLocaleAndEndpointValidation(t *testing.T) {
	for _, locale := range []struct{ input, expected string }{{"zh", "zh"}, {"zh-CN", "zh"}, {"en", "en"}, {"ja", "ja"}} {
		t.Run(locale.input, func(t *testing.T) {
			normalized, ok := normalizeLocale(locale.input)
			assert.True(t, ok)
			assert.Equal(t, locale.expected, normalized)
		})
	}
	_, valid := normalizeLocale("invalid")
	assert.False(t, valid)
	for _, endpoint := range []string{`1`, `null`, `{"openai":false}`, `{"openai":{"path":"invalid"}}`} {
		assert.Error(t, model.ValidateModelEndpoints(endpoint))
	}
}

func TestVendorManagementDatabase(t *testing.T) {
	db := modelManagementDB(t)

	t.Run("pricing_reads_keep_default_brands_without_writing_vendors", func(t *testing.T) {
		channel := model.Provider{Name: "Vendor fixture", Type: 1, Status: common.ChannelStatusEnabled}
		require.NoError(t, db.Create(&channel).Error)
		require.NoError(t, db.Create(&model.Ability{Model: "gemini-vendor-fixture", Group: "default", ChannelId: channel.Id, Enabled: true}).Error)
		publishers, err := model.GetRankingModelPublishers([]string{"gemini-vendor-fixture"})
		require.NoError(t, err)
		require.Len(t, publishers, 1)
		assert.Equal(t, model.RankingModelPublisher{Name: "Google", Icon: "Gemini.Color"}, publishers["gemini-vendor-fixture"])
		var count int64
		require.NoError(t, db.Model(&model.Vendor{}).Count(&count).Error)
		assert.Zero(t, count)
		saved := model.Vendor{Name: "Google", Icon: "Gemini.Color"}
		require.NoError(t, saved.Insert())
		stored, err := model.GetVendorByID(saved.Id)
		require.NoError(t, err)
		assert.Equal(t, saved.Id, stored.Id)
		assert.Equal(t, "Google", stored.Name)
		withStored, err := model.GetRankingModelPublishers([]string{"gemini-vendor-fixture"})
		require.NoError(t, err)
		assert.Equal(t, publishers, withStored)
		require.NoError(t, saved.Delete())
		_, err = model.GetVendorByID(saved.Id)
		require.ErrorIs(t, err, gorm.ErrRecordNotFound)
		after, err := model.GetRankingModelPublishers([]string{"gemini-vendor-fixture"})
		require.NoError(t, err)
		assert.Equal(t, publishers, after)
		require.NoError(t, db.Model(&model.Vendor{}).Count(&count).Error)
		assert.Zero(t, count, "display defaults must not recreate a deleted publisher")
	})
	t.Run("metadata_ownership_preview_merge_delete_and_rollback", func(t *testing.T) {
		source := model.Vendor{Name: "  Vendor Source  ", Icon: "Gemini.Color"}
		target := model.Vendor{Name: "Vendor Target", Description: "Keep target", Icon: "OpenAI"}
		require.NoError(t, source.Insert())
		require.NoError(t, target.Insert())
		assert.Equal(t, "Vendor Source", source.Name)
		assert.Error(t, (&model.Vendor{Name: "vendor source"}).Insert())
		assert.Error(t, (&model.Vendor{Name: " "}).Insert())
		require.NoError(t, db.Model(&model.Vendor{}).Where("id = ?", source.Id).Update("status", 0).Error)
		loaded, err := model.GetVendorByID(source.Id)
		require.NoError(t, err)
		staleVersion := loaded.Version
		edit := model.Vendor{Id: source.Id, Name: source.Name, Description: "Updated source", Icon: source.Icon, Version: loaded.Version}
		require.NoError(t, edit.Update())
		updated, err := model.GetVendorByID(source.Id)
		require.NoError(t, err)
		assert.Equal(t, loaded.CreatedTime, updated.CreatedTime)
		assert.Zero(t, updated.Status)
		edit.Version = staleVersion
		assert.ErrorIs(t, edit.Update(), model.ErrVendorConflict)

		one := model.Model{ModelName: "vendor-model-one", VendorID: source.Id, Icon: "Custom", Description: "Preserve description", Status: 0, SyncOfficial: 0}
		rule := model.Model{ModelName: "vendor-rule-", VendorID: source.Id, NameRule: model.NameRulePrefix, Status: 1, SyncOfficial: 1}
		require.NoError(t, one.Insert())
		require.NoError(t, rule.Insert())
		owner := modelManagementPriceOwner(t, db, one.ModelName, protocol.OpenAIChat)
		modelManagementPrice(t, db, owner, `tier("request", fixed(0.25))`, 1)
		priceBefore := modelManagementPrices(t, db)
		optionBefore := modelManagementOptions(t, db)
		channel := model.Provider{Name: "Unchanged vendor channel", Type: 1, Status: common.ChannelStatusEnabled}
		require.NoError(t, db.Create(&channel).Error)
		ability := model.Ability{Model: one.ModelName, Group: "default", ChannelId: channel.Id, Enabled: true}
		require.NoError(t, db.Create(&ability).Error)
		linked, total, err := model.SearchVendors("", 0, 20, "linked")
		require.NoError(t, err)
		require.Len(t, linked, 1)
		assert.EqualValues(t, 1, total)
		assert.EqualValues(t, 2, linked[0].ModelCount)
		unlinked, _, err := model.SearchVendors("Vendor Target", 0, 20, "unlinked")
		require.NoError(t, err)
		require.Len(t, unlinked, 1)
		var references *model.VendorReferenceError
		err = model.DeleteVendors([]int{source.Id, target.Id})
		require.ErrorAs(t, err, &references)
		assert.EqualValues(t, 2, references.Counts[source.Id])
		_, err = model.GetVendorByID(target.Id)
		require.NoError(t, err, "bulk delete must not partially delete unreferenced vendors")
		var response struct {
			Success         bool
			Code            string
			ReferenceCounts map[int]int64 `json:"reference_counts"`
		}
		recorder := modelManagementRequest(t, PreviewVendorOperation, http.MethodPost, "/api/vendors/operations/preview", model.VendorOperation{Action: "delete", VendorIDs: []int{source.Id}}, &response)
		assert.Equal(t, http.StatusConflict, recorder.Code)
		assert.Equal(t, "VENDOR_REFERENCED", response.Code)
		assert.EqualValues(t, 2, response.ReferenceCounts[source.Id])

		disappearing := model.Vendor{Name: "Preview target"}
		require.NoError(t, disappearing.Insert())
		staleAssignment := model.VendorOperation{Action: "assign", ModelIDs: []int{one.Id}, TargetVendorID: disappearing.Id}
		stalePreview, err := model.PreviewVendorOperation(staleAssignment)
		require.NoError(t, err)
		staleAssignment.ExpectedVersion = stalePreview.Version
		require.NoError(t, disappearing.Delete())
		recorder = modelManagementRequest(t, ApplyVendorOperation, http.MethodPost, "/api/vendors/operations", staleAssignment, &response)
		assert.Equal(t, http.StatusConflict, recorder.Code)
		assert.Equal(t, "VENDOR_CONFLICT", response.Code)

		assign := model.VendorOperation{Action: "assign", ModelIDs: []int{one.Id}, TargetVendorID: target.Id}
		preview, err := model.PreviewVendorOperation(assign)
		require.NoError(t, err)
		require.Len(t, preview.Models, 1)
		assign.ExpectedVersion = preview.Version
		one.Description = "Updated in the same timestamp"
		require.NoError(t, db.Model(&model.Model{}).Where("id = ?", one.Id).Update("description", one.Description).Error)
		_, err = model.ApplyVendorOperation(assign)
		assert.ErrorIs(t, err, model.ErrVendorConflict)
		preview, err = model.PreviewVendorOperation(assign)
		require.NoError(t, err)
		assign.ExpectedVersion = preview.Version
		target.Description = "New target description"
		require.NoError(t, target.Update())
		_, err = model.ApplyVendorOperation(assign)
		assert.ErrorIs(t, err, model.ErrVendorConflict)
		preview, err = model.PreviewVendorOperation(assign)
		require.NoError(t, err)
		assign.ExpectedVersion = preview.Version
		result, err := model.ApplyVendorOperation(assign)
		require.NoError(t, err)
		assert.Equal(t, []int{one.Id}, result.UpdatedModels)
		var after model.Model
		after = model.Model{}
		require.NoError(t, db.First(&after, one.Id).Error)
		assert.Equal(t, target.Id, after.VendorID)
		assert.Equal(t, one.Description, after.Description)
		assert.Equal(t, one.Icon, after.Icon)
		assert.Equal(t, one.Status, after.Status)
		after = model.Model{}
		require.NoError(t, db.First(&after, rule.Id).Error)
		assert.Equal(t, source.Id, after.VendorID)
		assign.TargetVendorID = 0
		preview, err = model.PreviewVendorOperation(assign)
		require.NoError(t, err)
		assign.ExpectedVersion = preview.Version
		_, err = model.ApplyVendorOperation(assign)
		require.NoError(t, err)
		after = model.Model{}
		require.NoError(t, db.First(&after, one.Id).Error)
		assert.Zero(t, after.VendorID)
		after.VendorID = -1001
		assert.Error(t, after.Update(), "display-only vendors cannot become stored references")

		merge := model.VendorOperation{Action: "merge", VendorIDs: []int{source.Id}, TargetVendorID: target.Id}
		preview, err = model.PreviewVendorOperation(merge)
		require.NoError(t, err)
		merge.ExpectedVersion = preview.Version
		require.NoError(t, db.Callback().Delete().Before("gorm:delete").Register("vendor_delete_failure", func(tx *gorm.DB) {
			if tx.Statement.Table == "vendors" {
				tx.AddError(errors.New("injected vendor delete failure"))
			}
		}))
		_, err = model.ApplyVendorOperation(merge)
		require.Error(t, err)
		require.NoError(t, db.Callback().Delete().Remove("vendor_delete_failure"))
		after = model.Model{}
		require.NoError(t, db.First(&after, rule.Id).Error)
		assert.Equal(t, source.Id, after.VendorID, "ownership updates roll back when deletion fails")
		_, err = model.GetVendorByID(source.Id)
		require.NoError(t, err)
		_, err = model.ApplyVendorOperation(merge)
		require.NoError(t, err)
		_, err = model.GetVendorByID(source.Id)
		assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
		retained, err := model.GetVendorByID(target.Id)
		require.NoError(t, err)
		assert.Equal(t, target.Description, retained.Description)
		assert.Equal(t, "OpenAI", retained.Icon)
		assert.EqualValues(t, 1, retained.ModelCount)
		after = one
		after.VendorID = source.Id
		assert.Error(t, after.Update(), "deleted vendors cannot acquire new references")
		assert.Equal(t, priceBefore, modelManagementPrices(t, db), "assignment and merge preserve immutable owner prices")
		assert.Equal(t, optionBefore, modelManagementOptions(t, db), "assignment and merge preserve historical option bytes")
		var retainedChannel model.Provider
		require.NoError(t, db.First(&retainedChannel, channel.Id).Error)
		assert.Equal(t, channel.Name, retainedChannel.Name)
		assert.Equal(t, channel.Status, retainedChannel.Status)
		var retainedAbility model.Ability
		require.NoError(t, db.Where("model = ? AND channel_id = ?", one.ModelName, channel.Id).First(&retainedAbility).Error)
		assert.Equal(t, ability.Group, retainedAbility.Group)
		assert.Equal(t, ability.Enabled, retainedAbility.Enabled)
	})
	t.Run("concurrent_create_and_delete_never_orphan_model", func(t *testing.T) {
		vendor := model.Vendor{Name: "Concurrent owner"}
		require.NoError(t, vendor.Insert())
		var createErr, deleteErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			createErr = (&model.Model{ModelName: "concurrent-owned-model", VendorID: vendor.Id}).Insert()
		}()
		go func() { defer wg.Done(); deleteErr = vendor.Delete() }()
		wg.Wait()
		if createErr == nil {
			require.Error(t, deleteErr)
		} else {
			require.NoError(t, deleteErr)
		}
		var models []model.Model
		require.NoError(t, db.Where("model_name = ?", "concurrent-owned-model").Find(&models).Error)
		if len(models) != 0 {
			_, err := model.GetVendorByID(models[0].VendorID)
			require.NoError(t, err)
		}
	})
}

func TestModelDeletionDatabase(t *testing.T) {
	for _, rule := range []int{model.NameRuleExact, model.NameRulePrefix, model.NameRuleContains, model.NameRuleSuffix} {
		t.Run(fmt.Sprintf("rule_%d_atomic_metadata_only", rule), func(t *testing.T) {
			db := modelManagementDB(t)
			name := fmt.Sprintf("delete-rule-%d", rule)
			first := model.Model{ModelName: name, NameRule: rule, Status: 1}
			second := model.Model{ModelName: name + "-second", Status: 1}
			keep := model.Model{ModelName: name + "-keep", Status: 1}
			for _, record := range []*model.Model{&first, &second, &keep} {
				require.NoError(t, record.Insert())
			}
			mapping := `{"` + name + `":"upstream-name"}`
			priority, weight := int64(7), uint(9)
			providers := []model.Provider{
				{Name: "Enabled", Type: 1, Key: "fixture-key", Models: name + "," + keep.ModelName + "," + second.ModelName, Group: "default,vip", Status: common.ChannelStatusEnabled, ModelMapping: &mapping, Priority: &priority, Weight: &weight},
				{Name: "Disabled", Type: 1, Models: name + ",prefix-" + name, Group: "disabled-group", Status: common.ChannelStatusManuallyDisabled},
				{Name: "Last model", Type: 1, Models: name, Group: "last-model-group", Status: common.ChannelStatusEnabled},
				{Name: "Case-sensitive name", Type: 1, Models: strings.ToUpper(name), Group: "case-group", Status: common.ChannelStatusEnabled},
			}
			declareLegacyProviderRoutingColumns(t, db)
			for i := range providers {
				require.NoError(t, db.Create(&providers[i]).Error)
				// Explicit historical source, never a live account API mutation.
				seedLegacyProviderRouting(t, db, &providers[i])
				require.NoError(t, providers[i].AddAbilities(db))
			}
			archiveBefore, err := model.ArchiveLegacyAbilitySource(db)
			require.NoError(t, err)
			require.Positive(t, archiveBefore.RowCount)
			require.NoError(t, db.Migrator().DropTable(&model.Ability{}))
			common.MemoryCacheEnabled = true
			model.InitProviderCache()
			cachedBefore, err := model.CacheGetProvider(providers[0].Id)
			require.NoError(t, err)
			require.NotNil(t, cachedBefore)
			accountBefore := *cachedBefore

			// Stored historical prices are immutable inputs here, not prices to
			// delete or publish when metadata with a matching name disappears.
			legacyPrices := []struct {
				key     string
				value   any
				load    func(string) error
				current func() string
			}{
				{"ModelPrice", 0, ratio_setting.UpdateModelPriceByJSONString, ratio_setting.ModelPrice2JSONString},
				{"ModelRatio", 1, ratio_setting.UpdateModelRatioByJSONString, ratio_setting.ModelRatio2JSONString},
				{"CompletionRatio", 2, ratio_setting.UpdateCompletionRatioByJSONString, ratio_setting.CompletionRatio2JSONString},
				{"CacheRatio", 0.1, ratio_setting.UpdateCacheRatioByJSONString, ratio_setting.CacheRatio2JSONString},
				{"CreateCacheRatio", 1.25, ratio_setting.UpdateCreateCacheRatioByJSONString, ratio_setting.CreateCacheRatio2JSONString},
				{"ImageRatio", 3, ratio_setting.UpdateImageRatioByJSONString, ratio_setting.ImageRatio2JSONString},
				{"AudioRatio", 4, ratio_setting.UpdateAudioRatioByJSONString, ratio_setting.AudioRatio2JSONString},
				{"AudioCompletionRatio", 5, ratio_setting.UpdateAudioCompletionRatioByJSONString, ratio_setting.AudioCompletionRatio2JSONString},
				{"billing_setting.billing_mode", "tiered_expr", nil, nil},
				{"billing_setting.billing_expr", `tier("base", p * 2 + c * 4)`, nil, nil},
			}
			storedPrices := make(map[string]string, len(legacyPrices))
			runtimePrices := make(map[string]string, len(legacyPrices))
			for _, source := range legacyPrices {
				encoded, err := common.Marshal(map[string]any{name: source.value, second.ModelName: source.value, keep.ModelName: source.value})
				require.NoError(t, err)
				storedPrices[source.key] = string(encoded)
				require.NoError(t, db.Create(&model.Option{Key: source.key, Value: string(encoded)}).Error)
				common.OptionMap[source.key] = string(encoded)
				if source.load != nil {
					require.NoError(t, source.load(string(encoded)))
					runtimePrices[source.key] = source.current()
				}
			}
			config.UpdateConfigFromMap(config.GlobalConfig.Get("billing_setting"), map[string]string{
				"billing_mode": storedPrices["billing_setting.billing_mode"],
				"billing_expr": storedPrices["billing_setting.billing_expr"],
			})
			billingConfigBefore := config.GlobalConfig.ExportAllConfigs()
			assertSourcesUnchanged := func(t *testing.T) {
				t.Helper()
				for _, original := range providers {
					var after model.Provider
					require.NoError(t, db.First(&after, original.Id).Error)
					assert.Equal(t, original, after)
				}
				archiveAfter, err := model.VerifyLegacyAbilitySourceArchive(db)
				require.NoError(t, err)
				assert.Equal(t, archiveBefore, archiveAfter)
				assert.False(t, db.Migrator().HasTable(&model.Ability{}))
				for _, source := range legacyPrices {
					var stored model.Option
					require.NoError(t, db.Where("key = ?", source.key).First(&stored).Error)
					assert.Equal(t, storedPrices[source.key], stored.Value, source.key)
					assert.Equal(t, storedPrices[source.key], common.OptionMap[source.key], source.key)
					if source.current != nil {
						assert.Equal(t, runtimePrices[source.key], source.current(), source.key)
					}
				}
				billingConfigAfter := config.GlobalConfig.ExportAllConfigs()
				for _, key := range []string{"billing_setting.billing_mode", "billing_setting.billing_expr"} {
					assert.Equal(t, billingConfigBefore[key], billingConfigAfter[key], key)
				}
				cached, err := model.CacheGetProvider(providers[0].Id)
				require.NoError(t, err)
				require.NotNil(t, cached)
				assert.Equal(t, accountBefore, *cached)
			}

			for _, role := range []int{common.RoleAdminUser, common.RoleRootUser} {
				for _, flags := range []struct{ channels, pricing bool }{{true, false}, {false, true}, {true, true}} {
					for _, single := range []bool{false, true} {
						body := map[string]any{"model_ids": []int{first.Id, second.Id}, "remove_from_channels": flags.channels, "remove_pricing": flags.pricing}
						path := fmt.Sprintf("/api/models/delete?remove_from_channels=%t&remove_pricing=%t", flags.channels, flags.pricing)
						recorder := modelManagementRequest(t, func(c *gin.Context) {
							c.Set("role", role)
							if single {
								c.Params = gin.Params{{Key: "id", Value: strconv.Itoa(first.Id)}}
								DeleteModelMeta(c)
							} else {
								BatchDeleteModelMeta(c)
							}
						}, http.MethodPost, path, body, nil)
						assert.Equal(t, http.StatusGone, recorder.Code, recorder.Body.String())
						assert.Contains(t, recorder.Body.String(), `"success":false`)
						var remaining int64
						require.NoError(t, db.Model(&model.Model{}).Where("id IN ?", []int{first.Id, second.Id}).Count(&remaining).Error)
						assert.EqualValues(t, 2, remaining)
						assertSourcesUnchanged(t)
					}
				}
			}

			// A late failure after the delete has executed rolls the entire
			// metadata batch back; no partial count or source mutation escapes.
			require.NoError(t, db.Callback().Delete().After("gorm:delete").Register("fail_model_deletion", func(tx *gorm.DB) {
				if tx.Statement.Table == "models" && tx.RowsAffected == 2 {
					tx.AddError(errors.New("injected model deletion failure"))
				}
			}))
			result, err := model.DeleteModelMetadata([]int{first.Id, second.Id})
			require.EqualError(t, err, "injected model deletion failure")
			assert.Zero(t, result)
			require.NoError(t, db.Callback().Delete().Remove("fail_model_deletion"))
			var remaining int64
			require.NoError(t, db.Model(&model.Model{}).Where("id IN ?", []int{first.Id, second.Id}).Count(&remaining).Error)
			assert.EqualValues(t, 2, remaining)
			assertSourcesUnchanged(t)
			_, err = model.DeleteModelMetadata([]int{first.Id, 999999})
			require.Error(t, err, "a missing model aborts the whole batch")
			require.NoError(t, db.Model(&model.Model{}).Where("id IN ?", []int{first.Id, second.Id}).Count(&remaining).Error)
			assert.EqualValues(t, 2, remaining)

			channel := model.Channel{ProviderID: providers[0].Id, ModelID: first.Id, Protocol: protocol.OpenAIChat,
				UpstreamModelName: "upstream", ParameterOverrides: "{}", Weight: 1, Status: model.ChannelPending}
			require.NoError(t, db.Create(&channel).Error)
			_, err = model.DeleteModelMetadata([]int{first.Id, second.Id})
			require.ErrorIs(t, err, model.ErrModelHasChannels)
			require.NoError(t, db.Model(&model.Model{}).Where("id IN ?", []int{first.Id, second.Id}).Count(&remaining).Error)
			assert.EqualValues(t, 2, remaining)
			require.NoError(t, db.Delete(&channel).Error)
			require.NoError(t, db.AutoMigrate(&model.ModelOffering{}, &model.ModelOfferingMember{}))
			offering := model.ModelOffering{Name: "referenced", Visibility: "all", Groups: []string{}, RouteMethod: "weight",
				AcceptedConversionLosses: []string{}, Members: []model.ModelOfferingMember{{ModelID: second.Id, Tier: 1}}}
			require.NoError(t, db.Create(&offering).Error)
			_, err = model.DeleteModelMetadata([]int{first.Id, second.Id})
			require.ErrorIs(t, err, model.ErrModelOfferingMember)
			require.NoError(t, db.Model(&model.Model{}).Where("id IN ?", []int{first.Id, second.Id}).Count(&remaining).Error)
			assert.EqualValues(t, 2, remaining)
			assertSourcesUnchanged(t)
			require.NoError(t, db.Where("offering_id = ?", offering.ID).Delete(&model.ModelOfferingMember{}).Error)

			var response struct {
				Success bool
				Data    model.ModelDeleteResult
			}
			body := map[string]any{"model_ids": []int{first.Id, second.Id, first.Id}}
			recorder := modelManagementRequest(t, BatchDeleteModelMeta, http.MethodPost, "/api/models/delete", body, &response)
			require.True(t, response.Success, recorder.Body.String())
			assert.Equal(t, model.ModelDeleteResult{DeletedCount: 2}, response.Data)
			require.NoError(t, db.Model(&model.Model{}).Where("id IN ?", []int{first.Id, second.Id}).Count(&remaining).Error)
			assert.Zero(t, remaining)
			var retained model.Model
			require.NoError(t, db.First(&retained, keep.Id).Error)
			assert.Equal(t, keep.ModelName, retained.ModelName, "name rules never expand the selected IDs")
			assertSourcesUnchanged(t)
			_, err = model.DeleteModelMetadata([]int{first.Id})
			require.Error(t, err, "stale selections cannot delete newly created records")
			recorder = modelManagementRequest(t, func(c *gin.Context) {
				c.Params = gin.Params{{Key: "id", Value: strconv.Itoa(keep.Id)}}
				DeleteModelMeta(c)
			}, http.MethodDelete, "/api/models/"+strconv.Itoa(keep.Id), nil, &response)
			require.True(t, response.Success, recorder.Body.String())
			assert.Equal(t, model.ModelDeleteResult{DeletedCount: 1}, response.Data)
			assertSourcesUnchanged(t)
			for _, ids := range [][]int{nil, {0}, {-1}, make([]int, 1001)} {
				_, err := model.DeleteModelMetadata(ids)
				assert.Error(t, err)
			}
		})
	}
}

func TestSharedModelPluginPricingDatabase(t *testing.T) {
	const name = "shared-model::priced"
	const base = `tier("base", u("seconds") * 0.4)`
	const variant = `tier("beta", u("credits") * 2)`
	db := modelManagementDB(t)
	declareLegacyProviderRoutingColumns(t, db)
	for _, spec := range []struct{ key, field, unit string }{{"matrix-alpha", "seconds", "second"}, {"matrix-beta", "credits", "credit"}} {
		source := fmt.Sprintf(`
export const meta = {apiVersion:1,key:%q,name:%q,version:"1.0.0",author:{name:"Test"},models:[%q],fetchMode:"per_task",usageSchema:{%s:{type:"number",unit:%q}}};
export function buildSubmitRequest(){return {};}
export function parseSubmitResponse(){return {};}
export function buildQueryRequest(){return {};}
export function parseTaskResult(){return {};}
`, spec.key, spec.key, name, spec.field, spec.unit)
		_, err := jsplugin.DefaultRegistry.Register(source, jsplugin.Options{})
		require.NoError(t, err)
		t.Cleanup(func() { jsplugin.DefaultRegistry.Unregister(spec.key) })
		if spec.key == "matrix-alpha" {
			preview, err := legacyPricingSource(t, db).Preview(name, model.PricingValues{
				"billing_setting.billing_mode": "tiered_expr", "billing_setting.billing_expr": base,
			})
			require.NoError(t, err)
			assert.Equal(t, "This model already uses an expression.", preview.UnsupportedReason)
		}
	}
	baseJSON, err := common.Marshal(map[string]string{name: base})
	require.NoError(t, err)
	modeJSON, err := common.Marshal(map[string]string{name: "tiered_expr"})
	require.NoError(t, err)
	// Historical options are fixture input only, never an online pricing writer.
	require.NoError(t, db.Create(&[]model.Option{
		{Key: "ModelPrice", Value: `{"unrelated-model":0.75}`},
		{Key: "billing_setting.billing_expr", Value: string(baseJSON)},
		{Key: "billing_setting.billing_mode", Value: string(modeJSON)},
	}).Error)
	options := modelManagementOptions(t, db)
	source := legacyPricingSource(t, db)
	draft := model.PricingValues{"billing_setting.billing_mode": "tiered_expr", "billing_setting.billing_expr": base}
	_, err = source.Preview(name, draft)
	require.ErrorContains(t, err, "matrix-beta", "the base seconds expression cannot silently price a credits-only plugin")
	draft[billing_setting.PluginBillingExprOption] = map[string]any{"matrix-beta": variant}
	preview, err := source.Preview(name, draft)
	require.NoError(t, err)
	assert.Equal(t, "This model already uses an expression.", preview.UnsupportedReason)
	assert.Equal(t, options, modelManagementOptions(t, db), "offline validation never saves its draft")

	alpha := modelManagementPriceOwner(t, db, name, protocol.Type("task:matrix-alpha"))
	beta := modelManagementPriceOwner(t, db, name, protocol.Type("task:matrix-beta"))
	ordinary := modelManagementPriceOwner(t, db, name, protocol.OpenAIChat)
	assert.Equal(t, alpha.ModelID, beta.ModelID)
	assert.Equal(t, alpha.ModelID, ordinary.ModelID)
	alphaPrice := modelManagementPrice(t, db, alpha, base, 1)
	betaPrice := modelManagementPrice(t, db, beta, variant, 1)
	ordinaryPrice := modelManagementPrice(t, db, ordinary, `tier("image", fixed(0.04)) * image_count`, 1)
	prices := modelManagementPrices(t, db)
	invalid := model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: beta.ID, Currency: money.Currency("USD"), Expression: base, EffectiveAt: 2}
	require.Error(t, model.CreatePriceVersion(context.Background(), &invalid, betaPrice.CreatedBy))
	assert.Equal(t, prices, modelManagementPrices(t, db))
	betaFree := modelManagementPrice(t, db, beta, `tier("free", u("credits") * 0)`, 2)
	for _, expected := range []model.PriceVersion{alphaPrice, betaFree, ordinaryPrice} {
		current, err := model.CurrentPriceVersion(context.Background(), db, expected.OwnerType, expected.OwnerID, 2)
		require.NoError(t, err)
		assert.Equal(t, expected.ID, current.ID)
		assert.Equal(t, expected.Expression, current.Expression)
	}
	historical, err := model.CurrentPriceVersion(context.Background(), db, model.PriceOwnerChannel, beta.ID, 1)
	require.NoError(t, err)
	assert.Equal(t, betaPrice.ID, historical.ID, "changing one owner never overwrites its accepted history")
	for _, quote := range []struct {
		price model.PriceVersion
		input billingexpr.MoneyInput
		want  string
	}{
		{alphaPrice, billingexpr.MoneyInput{TaskUsage: true, Request: billingexpr.RequestInput{Usage: map[string]any{"seconds": 2}}}, "0.8 USD"},
		{*historical, billingexpr.MoneyInput{TaskUsage: true, Request: billingexpr.RequestInput{Usage: map[string]any{"credits": 2}}}, "4 USD"},
		{betaFree, billingexpr.MoneyInput{TaskUsage: true, Request: billingexpr.RequestInput{Usage: map[string]any{"credits": 2}}}, "0 USD"},
		{ordinaryPrice, billingexpr.MoneyInput{ImageCount: common.GetPointer(2)}, "0.08 USD"},
	} {
		amount, _, err := billingexpr.RunMoneyExpr(quote.price.Expression, quote.input, quote.price.Currency)
		require.NoError(t, err)
		assert.Equal(t, quote.want, amount.String())
	}
	prices = modelManagementPrices(t, db)
	require.NoError(t, model.MigratePricing(db))
	assert.Equal(t, prices, modelManagementPrices(t, db), "repeated schema migration preserves owner versions")
	assert.Equal(t, options, modelManagementOptions(t, db), "native owner prices do not dual-write model-name options")

	// Flat historical plugin keys retain the complete model name, including ::.
	flatJSON, err := common.Marshal(map[string]string{"matrix-beta::" + name: variant})
	require.NoError(t, err)
	require.NoError(t, db.Create(&model.Option{Key: billing_setting.PluginBillingExprOption, Value: string(flatJSON)}).Error)
	options = modelManagementOptions(t, db)
	source = legacyPricingSource(t, db)
	_, err = source.Preview(name, draft)
	require.NoError(t, err)
	unrelated, err := source.Preview("unrelated-model", model.PricingValues{"ModelPrice": float64(0.75)})
	require.NoError(t, err)
	assert.Equal(t, `tier("request", fixed(0.75))`, unrelated.Expression)
	assert.NotContains(t, billing_setting.GetPricingSyncData(map[string]any{}), "plugin_billing_expr")
	assert.NotContains(t, billing_setting.GetPricingSyncData(map[string]any{})["billing_expr"], "matrix-beta::"+name)
	for _, request := range []OptionUpdateRequest{
		{Key: billing_setting.PluginBillingExprOption, Value: `{}`},
		{Key: billing_setting.PluginBillingExprOption, Value: string(flatJSON)},
		{Key: "billing_setting.billing_expr", Value: string(baseJSON)},
		{Key: "ModelRatio", Value: `{"shared-model::priced":2}`},
	} {
		response := modelManagementRequest(t, UpdateOption, http.MethodPut, "/api/option/", request, nil)
		assert.Equal(t, http.StatusGone, response.Code)
		assert.Contains(t, response.Body.String(), `"success":false`)
		assert.Equal(t, options, modelManagementOptions(t, db))
		assert.Equal(t, prices, modelManagementPrices(t, db))
	}
	// Removing a required override remains invalid in captured offline input.
	_, err = source.Preview(name, model.PricingValues{"billing_setting.billing_mode": "tiered_expr", "billing_setting.billing_expr": base})
	require.ErrorContains(t, err, "matrix-beta")
	require.NoError(t, jsplugin.DefaultRegistry.Unregister("matrix-beta"))
	stopped := legacyPricingSource(t, db)
	_, err = stopped.Preview(name, draft)
	require.NoError(t, err, "unchanged unavailable-plugin evidence remains intact for offline migration")
	tamperedJSON, err := common.Marshal(map[string]string{"matrix-beta::" + name: "1"})
	require.NoError(t, err)
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{billing_setting.PluginBillingExprOption: string(tamperedJSON)}))
	draft[billing_setting.PluginBillingExprOption] = map[string]any{"matrix-beta": "1"}
	_, err = stopped.Preview(name, draft)
	require.ErrorContains(t, err, "does not declare this model", "runtime cache cannot replace captured database evidence")
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{billing_setting.PluginBillingExprOption: string(flatJSON)}))
	invalid = model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: beta.ID, Currency: money.Currency("USD"), Expression: variant, EffectiveAt: 3}
	require.Error(t, model.CreatePriceVersion(context.Background(), &invalid, betaPrice.CreatedBy))
	assert.Equal(t, prices, modelManagementPrices(t, db), "stopping a plugin cannot mutate or extend its price history")
	_, err = jsplugin.DefaultRegistry.Register(`
export const meta = {apiVersion:1,key:"matrix-beta",name:"Beta updated",version:"2.0.0",author:{name:"Test"},models:["replacement-model"],fetchMode:"per_task",usageSchema:{credits:{type:"number",unit:"credit"}}};
export function buildSubmitRequest(){return {};}
export function parseSubmitResponse(){return {};}
export function buildQueryRequest(){return {};}
export function parseTaskResult(){return {};}
`, jsplugin.Options{})
	require.NoError(t, err)
	require.NoError(t, jsplugin.DefaultRegistry.Unregister("matrix-alpha"))
	changed := legacyPricingSource(t, db)
	draft[billing_setting.PluginBillingExprOption] = map[string]any{"matrix-beta": variant}
	_, err = changed.Preview(name, draft)
	require.NoError(t, err)
	delete(draft, billing_setting.PluginBillingExprOption)
	_, err = changed.Preview(name, draft)
	require.NoError(t, err, "discarding only a stale offline override must not discard unrelated saved evidence")
	_, err = changed.Preview(name, model.PricingValues{})
	require.NoError(t, err, "reset is a non-persistent offline draft, not deletion of financial history")
	assert.Equal(t, options, modelManagementOptions(t, db))
	assert.Equal(t, prices, modelManagementPrices(t, db))
}
