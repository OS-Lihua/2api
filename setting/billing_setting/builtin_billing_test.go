package billing_setting_test

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGPT6AstraBuiltinBilling(t *testing.T) {
	settings := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
	saved := *settings
	savedRatios, savedPrices := ratio_setting.ModelRatio2JSONString(), ratio_setting.ModelPrice2JSONString()
	savedOptions := common.OptionMap
	t.Cleanup(func() {
		*settings, common.OptionMap = saved, savedOptions
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(savedRatios))
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(savedPrices))
	})
	common.OptionMap = map[string]string{"billing_setting.billing_mode": `{}`, "billing_setting.billing_expr": `{}`}
	require.NoError(t, config.GlobalConfig.LoadFromDB(common.OptionMap))
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{}`))
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{}`))
	assert.Equal(t, billing_setting.BillingModeTieredExpr, billing_setting.GetBillingMode("gpt-6-astra"))
	expression, ok := billing_setting.GetBillingExpr("gpt-6-astra")
	require.True(t, ok)

	for _, tc := range []struct {
		name                                       string
		input, output, cached, written, written12h int
		request                                    string
		amount                                     string
	}{
		{"standard", 1000, 100, 0, 0, 0, `{}`, "0.015 USD"},
		{"client flex cannot discount standard pricing", 1000, 100, 0, 0, 0, `{"service_tier":"flex"}`, "0.015 USD"},
		{"cache at context boundary", 272000, 1000, 200000, 20000, 0, `{}`, "1.02 USD"},
		{"whole request above boundary", 272001, 1000, 200000, 20000, 0, `{}`, "2.01502 USD"},
		// The prompt caching guide example: 4000 of the 7000 writes are 12h.
		{"12h cache writes", 12000, 0, 4000, 7000, 4000, `{}`, "0.1515 USD"},
		{"12h cache writes above boundary", 300000, 0, 200000, 20000, 10000, `{}`, "2.75 USD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usage := &dto.Usage{
				PromptTokens: tc.input, CompletionTokens: tc.output,
				PromptTokensDetails: dto.InputTokenDetails{CachedTokens: tc.cached, CacheWriteTokens: tc.written,
					CacheWriteTokensDetails: &dto.CacheWriteTokenDetails{TTL30mTokens: tc.written - tc.written12h, TTL12hTokens: tc.written12h}},
			}
			price := &model.PriceVersion{Expression: expression, Currency: "USD"}
			input := billingexpr.RequestInput{Body: []byte(tc.request)}
			amount, _, _, err := service.GatewayPrice(price, usage, input, time.Time{}, decimal.NewFromInt(1), nil)
			require.NoError(t, err)
			assert.Equal(t, tc.amount, amount.String())
		})
	}

	t.Run("admin options omit legacy defaults without changing offline pricing", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		controller.GetOptions(ctx)
		var response struct {
			Success bool
			Data    []model.Option
		}
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
		require.True(t, response.Success)
		found := make(map[string]string, len(response.Data))
		for _, option := range response.Data {
			assert.NotContains(t, found, option.Key)
			found[option.Key] = option.Value
			assert.False(t, model.IsModelPricingOption(option.Key), "retired pricing key: %s", option.Key)
		}
		assert.NotContains(t, found, "CompletionRatioMeta")
		assert.Equal(t, map[string]string{"billing_setting.billing_mode": `{}`, "billing_setting.billing_expr": `{}`}, common.OptionMap)
		assert.Equal(t, billing_setting.BillingModeTieredExpr, billing_setting.GetBillingMode("gpt-6-astra"))
		actualExpression, exists := billing_setting.GetBillingExpr("gpt-6-astra")
		require.True(t, exists)
		assert.Equal(t, expression, actualExpression)
	})

	for _, tc := range []struct {
		name, mode, expr, ratios, prices, wantMode string
	}{
		{"custom expression overrides legacy price", "tiered_expr", "p * 7", `{"gpt-6-astra":8}`, `{}`, "tiered_expr"},
		{"explicit ratio mode", "ratio", "", `{}`, `{}`, "ratio"},
		{"existing free token price", "", "", `{"gpt-6-astra":0}`, `{}`, "ratio"},
		{"existing per-call price", "", "", `{}`, `{"gpt-6-astra":0.1}`, "ratio"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			*settings = billing_setting.BillingSetting{BillingMode: map[string]string{}, BillingExpr: map[string]string{}}
			if tc.mode != "" {
				settings.BillingMode["gpt-6-astra"] = tc.mode
			}
			if tc.expr != "" {
				settings.BillingExpr["gpt-6-astra"] = tc.expr
			}
			require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(tc.ratios))
			require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(tc.prices))
			assert.Equal(t, tc.wantMode, billing_setting.GetBillingMode("gpt-6-astra"))
			actual, ok := billing_setting.GetBillingExpr("gpt-6-astra")
			assert.Equal(t, tc.expr, actual)
			assert.Equal(t, tc.expr != "", ok)
		})
	}
}

func TestImageModelBuiltinPricesAndOverrides(t *testing.T) {
	settings := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
	saved := *settings
	savedRatios, savedPrices := ratio_setting.ModelRatio2JSONString(), ratio_setting.ModelPrice2JSONString()
	t.Cleanup(func() {
		*settings = saved
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(savedRatios))
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(savedPrices))
	})
	for _, name := range []string{"gpt-image-2", "gpt-image-2.5-sunburst", "gpt-image-2.5-flare"} {
		t.Run(name, func(t *testing.T) {
			*settings = billing_setting.BillingSetting{BillingMode: map[string]string{}, BillingExpr: map[string]string{}}
			require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{}`))
			require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{}`))
			expression, ok := billing_setting.GetBillingExpr(name)
			require.True(t, ok)
			usage := &dto.Usage{PromptTokens: 1000, CompletionTokens: 100, PromptTokensDetails: dto.InputTokenDetails{
				CachedTokens: 300, ImageTokens: 600, CachedTokensDetails: &dto.CachedTokenDetails{ImageTokens: common.GetPointer(200)},
			}}
			price := &model.PriceVersion{Expression: expression, Currency: "USD"}
			amount, _, _, err := service.GatewayPrice(price, usage, billingexpr.RequestInput{}, time.Time{}, decimal.NewFromInt(1), nil)
			require.NoError(t, err)
			assert.Equal(t, "0.008225 USD", amount.String())
			encoded, err := common.Marshal(map[string]float64{name: 0})
			require.NoError(t, err)
			require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(string(encoded)))
			assert.Equal(t, "ratio", billing_setting.GetBillingMode(name))
			require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{}`))
			require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(string(encoded)))
			assert.Equal(t, "ratio", billing_setting.GetBillingMode(name))
			settings.BillingMode[name] = "tiered_expr"
			settings.BillingExpr[name] = `tier("custom", p * 7)`
			actual, ok := billing_setting.GetBillingExpr(name)
			require.True(t, ok)
			assert.Equal(t, settings.BillingExpr[name], actual)
		})
	}
}

// DeepSeek prices are peak/off-peak expressions in USD per 1M tokens; peak is
// twice off-peak. Equivalent Chat, Responses, and Anthropic usage costs the
// same amount. Search and Fetch cost $0.75 per 1,000 requests.
func TestDeepSeekBuiltinBilling(t *testing.T) {
	settings := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
	saved := *settings
	savedRatios, savedPrices := ratio_setting.ModelRatio2JSONString(), ratio_setting.ModelPrice2JSONString()
	t.Cleanup(func() {
		*settings = saved
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(savedRatios))
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(savedPrices))
	})
	*settings = billing_setting.BillingSetting{BillingMode: map[string]string{}, BillingExpr: map[string]string{}}
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{}`))
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{}`))

	// 1000 cache-miss + 2000 cache-hit input tokens and 500 output tokens.
	chat := &dto.Usage{PromptTokens: 3000, CompletionTokens: 500, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 2000}}
	responses := &dto.Usage{}
	service.ApplyResponsesUsage(responses, &dto.Usage{InputTokens: 3000, OutputTokens: 500, InputTokensDetails: &dto.InputTokenDetails{CachedTokens: 2000}})
	anthropic := &dto.Usage{PromptTokens: 1000, CompletionTokens: 500, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 2000}, UsageSemantic: "anthropic"}
	for name, want := range map[string]map[string]string{
		// flash: 1000*0.15 + 2000*0.003 + 500*0.6 = $0.000456 off-peak.
		"deepseek-flash":               {"off_peak": "0.000456 USD", "peak": "0.000912 USD"},
		"deepseek-v4-flash":            {"off_peak": "0.000456 USD", "peak": "0.000912 USD"},
		"deepseek-v4-flash-vision-exp": {"off_peak": "0.000456 USD", "peak": "0.000912 USD"},
		// pro: 1000*0.66 + 2000*0.022 + 500*1.98 = $0.001694 off-peak.
		"deepseek-v4-pro": {"off_peak": "0.001694 USD", "peak": "0.003388 USD"},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, billing_setting.BillingModeTieredExpr, billing_setting.GetBillingMode(name))
			expression, ok := billing_setting.GetBillingExpr(name)
			require.True(t, ok)
			price := &model.PriceVersion{Expression: expression, Currency: "USD"}
			for name, usage := range map[string]*dto.Usage{"chat": chat, "responses": responses, "anthropic": anthropic} {
				for tier, hour := range map[string]int{"peak": 1, "off_peak": 0} {
					at := time.Date(2026, time.November, 23, hour, 0, 0, 0, time.UTC)
					amount, _, trace, err := service.GatewayPrice(price, usage, billingexpr.RequestInput{}, at, decimal.NewFromInt(1), nil)
					require.NoError(t, err)
					assert.Equal(t, tier, trace.MatchedTier, name)
					assert.Equal(t, want[tier], amount.String(), name)
				}
			}
		})
	}
	for _, name := range []string{"deepseek-search", "deepseek-fetch"} {
		expression, ok := billing_setting.GetBillingExpr(name)
		require.True(t, ok, name)
		price := &model.PriceVersion{Expression: expression, Currency: "USD"}
		amount, _, trace, err := service.GatewayPrice(price, &dto.Usage{}, billingexpr.RequestInput{}, time.Time{}, decimal.NewFromInt(1), nil)
		require.NoError(t, err)
		assert.Equal(t, billingexpr.BillingUnitRequest, trace.BillingUnit, name)
		assert.Equal(t, "0.00075 USD", amount.String(), name)
	}
}
