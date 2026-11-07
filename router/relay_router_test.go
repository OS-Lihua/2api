package router

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/pkg/capability"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/plugins"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestListModelsSupportsOpenAIAndGeminiAuthentication(t *testing.T) {
	setupRelayRouterTestDB(t)

	user := model.User{
		Username: "models-user",
		Status:   common.UserStatusEnabled,
		Group:    "default",
		Quota:    100,
	}
	require.NoError(t, model.DB.Create(&user).Error)
	require.NoError(t, model.DB.Create(&model.Token{
		UserId:         user.Id,
		Key:            "modelstestkey",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}).Error)

	engine := gin.New()
	SetRelayRouter(engine)

	tests := []struct {
		name           string
		path           string
		headerName     string
		expectedObject string
		expectedField  string
	}{
		{
			name:           "OpenAI bearer token",
			path:           "/v1/models",
			headerName:     "Authorization",
			expectedObject: "list",
			expectedField:  "data",
		},
		{
			name:          "Gemini API key header",
			path:          "/v1/models",
			headerName:    "x-goog-api-key",
			expectedField: "models",
		},
		{
			name:          "Gemini API key query",
			path:          "/v1/models?key=modelstestkey",
			expectedField: "models",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			if test.headerName != "" {
				value := "modelstestkey"
				if test.headerName == "Authorization" {
					value = "Bearer " + value
				}
				request.Header.Set(test.headerName, value)
			}

			engine.ServeHTTP(recorder, request)

			require.Equal(t, http.StatusOK, recorder.Code)
			var payload map[string]any
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
			assert.Contains(t, payload, test.expectedField)
			assert.NotContains(t, payload, "error")
			if test.expectedObject != "" {
				assert.Equal(t, test.expectedObject, payload["object"])
			}
		})
	}
}

func TestGeminiRelayHandsLongRunningPredictionToPluginRoute(t *testing.T) {
	setupRelayRouterTestDB(t)
	require.NoError(t, i18n.Init())
	user := model.User{Username: "veo-user", Status: common.UserStatusEnabled, Group: "default", Quota: 100}
	require.NoError(t, model.DB.Create(&user).Error)
	require.NoError(t, model.DB.Create(&model.Token{UserId: user.Id, Key: "veotestkey", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}).Error)

	engine := gin.New()
	SetRelayRouter(engine)
	source, err := plugins.Source("google")
	require.NoError(t, err)
	registry := jsplugin.NewRegistry()
	_, err = registry.RegisterFactory(source, jsplugin.Options{Key: "google"})
	require.NoError(t, err)
	// The plugin route authenticates as in production, then names itself.
	handlers := func(generation *jsplugin.RoutingGeneration, group jsplugin.RouteGroup) []gin.HandlerFunc {
		production := productionPluginRouteHandlers(generation, group)
		return []gin.HandlerFunc{production[0], production[1], production[2], func(c *gin.Context) {
			c.String(http.StatusOK, "plugin %s for user %d", group.Bindings[0].Plugin.Meta.Key, c.GetInt("id"))
		}}
	}
	require.NoError(t, registry.SetGenerationPreparer(newPluginGenerationBuilder(engine.Routes(), nil, handlers).prepare))
	require.Empty(t, registry.RoutingErrors())
	previousRegistry := defaultPluginDispatcher.registry
	defaultPluginDispatcher.registry = registry
	t.Cleanup(func() { defaultPluginDispatcher.registry = previousRegistry })
	engine.NoRoute(defaultPluginDispatcher.dispatch)

	for _, testCase := range []struct {
		method   string
		path     string
		toPlugin bool
	}{
		{method: http.MethodPost, path: "/v1beta/models/veo-3.1-generate-preview:predictLongRunning", toPlugin: true},
		{method: http.MethodGet, path: "/v1beta/models/veo-3.1-generate-preview/operations/task_sample", toPlugin: true},
		{method: http.MethodPost, path: "/v1beta/models/gemini-2.5-flash:generateContent"},
	} {
		t.Run(testCase.path, func(t *testing.T) {
			request := httptest.NewRequest(testCase.method, testCase.path, strings.NewReader(`{}`))
			request.Header.Set("x-goog-api-key", "veotestkey")
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, request)

			if testCase.toPlugin {
				assert.Equal(t, fmt.Sprintf("plugin google for user %d", user.Id), recorder.Body.String())
				return
			}
			// The relay looks for a Gemini channel serving the model; there is none.
			assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
			assert.Contains(t, recorder.Body.String(), `"code":"model_not_found"`)
		})
	}
}

type textGatewayFixture struct {
	engine   *gin.Engine
	user     *model.User
	token    *model.Token
	adminPAT string
	userPAT  string
	offering *model.ModelOffering
	channel  *model.Channel
	sale     *model.PriceVersion
	purchase *model.PriceVersion
	fake     *clock.Fake
}

func newTextGatewayFixture(t *testing.T, upstream http.HandlerFunc) textGatewayFixture {
	t.Helper()
	t.Setenv("GATEWAY_SETTLEMENT_JOURNAL_DIR", t.TempDir())
	previousLosses := model_setting.GetGlobalSettings().AcceptedConversionLosses
	model_setting.GetGlobalSettings().AcceptedConversionLosses = []string{}
	t.Cleanup(func() { model_setting.GetGlobalSettings().AcceptedConversionLosses = previousLosses })
	_, actor, _ := newAdminTestServer(t)
	fake := clock.UseFake(t, time.Unix(1794000000, 0))
	previousStreamTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 60
	t.Cleanup(func() { constant.StreamingTimeout = previousStreamTimeout })
	require.NoError(t, model.InitSettlementCurrency())
	db := model.DB
	require.NoError(t, db.AutoMigrate(&model.Token{}, &model.Channel{}, &model.ModelOffering{}, &model.ModelOfferingName{}, &model.ModelOfferingMember{}, &model.Wallet{}, &model.LedgerEntry{}))
	require.NoError(t, db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS "idx_wallets_account" ON "wallets" ("account_type", "owner_id", "balance_currency")`).Error)
	require.NoError(t, model.MigratePricing(db))
	require.NoError(t, model.MigrateGatewayBilling(db))
	require.NoError(t, model.MigrateLogDB())
	common.OptionMapRWMutex.Lock()
	previousOptions := common.OptionMap
	common.OptionMap = map[string]string{"GroupRatio": `{"default":0.9}`, "GroupGroupRatio": `{}`}
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptions
		common.OptionMapRWMutex.Unlock()
	})
	previousRatio := ratio_setting.GroupRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":0.9}`))
	t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(previousRatio)) })
	user := &model.User{Username: "gateway-user", Status: common.UserStatusEnabled, Group: "default", Role: common.RoleCommonUser, AuthVersion: 1, AffCode: "gateway"}
	require.NoError(t, db.Create(user).Error)
	token := &model.Token{UserId: user.Id, Key: "gatewaytestkey", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 0}
	require.NoError(t, db.Create(token).Error)
	_, err := model.TopUpWallet(model.WalletTopUp{UserId: user.Id, Source: money.Amount{Nanos: 1_000_000_000, Currency: "USD"}, Reference: "gateway-initial"})
	require.NoError(t, err)
	publisher := &model.Vendor{Name: "Gateway publisher", Status: 1}
	require.NoError(t, db.Create(publisher).Error)
	unified := &model.Model{ModelName: "gpt-4o-mini", VendorID: publisher.Id, Status: 1, NameRule: model.NameRuleExact, Capabilities: capability.Set{Features: []capability.Feature{capability.Streaming, capability.ToolCalling}, InputModalities: []capability.Modality{capability.Text}, OutputModalities: []capability.Modality{capability.Text}}}
	require.NoError(t, db.Create(unified).Error)
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	provider := &model.Provider{Name: "Gateway Provider", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Key: "provider-secret", BaseURL: &server.URL}
	require.NoError(t, db.Create(provider).Error)
	channel := &model.Channel{ProviderID: provider.Id, ModelID: unified.Id, Protocol: "openai_chat", UpstreamModelName: "upstream-model", ParameterOverrides: `{}`, MappingConfirmed: true, Status: model.ChannelEnabled, Weight: 1}
	require.NoError(t, db.Create(channel).Error)
	offering := &model.ModelOffering{Name: "gateway-offering", Aliases: []string{"gateway-alias"}, Members: []model.ModelOfferingMember{{ModelID: unified.Id, Tier: 1}}}
	require.NoError(t, model.SaveModelOffering(t.Context(), offering, "save"))
	sale := &model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: offering.ID, Currency: "USD", Expression: `tier("base", p * 2 + c * 4)`, EffectiveAt: clock.Now().Unix() - 1}
	purchase := &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: channel.ID, Currency: "USD", Expression: `tier("base", p + c * 2)`, EffectiveAt: clock.Now().Unix() - 1}
	require.NoError(t, model.CreatePriceVersion(t.Context(), sale, actor.Id))
	require.NoError(t, model.CreatePriceVersion(t.Context(), purchase, actor.Id))
	require.NoError(t, model.SaveModelOffering(t.Context(), offering, "enable"))
	service.InitHttpClient()
	engine := gin.New()
	engine.Use(middleware.RequestId())
	SetApiRouter(engine)
	SetRelayRouter(engine)
	SetTaskPluginProtocolRouter(engine)
	return textGatewayFixture{engine: engine, user: user, token: token, adminPAT: createWalletTestAccessToken(t, actor.Id, "request:read", "ledger:read"), userPAT: createWalletTestAccessToken(t, user.Id, "log:read"), offering: offering, channel: channel, sale: sale, purchase: purchase, fake: fake}
}

func TestTextGatewayPurchaseProbesUseEffectiveProviderRequest(t *testing.T) {
	var sent map[string]any
	var sentHeader string
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, common.DecodeJson(r.Body, &sent))
		sentHeader = r.Header.Get("X-Billing-Tier")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	})
	sale := &model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: `param("service_tier") == "standard" && header("x-billing-tier") == "client" ? tier("original",fixed(0.002)) : tier("unexpected",fixed(0.1))`, EffectiveAt: clock.Now().Unix()}
	purchase := &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: `param("service_tier") == "priority" && header("x-billing-tier") == "provider" ? tier("effective",fixed(0.003)) : tier("unexpected",fixed(0.1))`, EffectiveAt: clock.Now().Unix()}
	require.NoError(t, model.CreatePriceVersion(t.Context(), sale, fixture.sale.CreatedBy))
	require.NoError(t, model.CreatePriceVersion(t.Context(), purchase, fixture.sale.CreatedBy))
	require.NoError(t, model.DB.Model(fixture.channel).Update("parameter_overrides", `{"service_tier":"priority","operations":[{"mode":"set_header","path":"X-Billing-Tier","value":"provider"}]}`).Error)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gateway-alias","service_tier":"standard","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`))
	request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Billing-Tier", "client")
	response := httptest.NewRecorder()
	fixture.engine.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, "priority", sent["service_tier"])
	assert.Equal(t, "provider", sentHeader)
	record, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
	require.NoError(t, err)
	assert.Equal(t, int64(1_800_000), record.Receivable.Nanos)
	attempts, _, err := model.ListGatewayAttempts(t.Context(), record.ID, 0, 10)
	require.NoError(t, err)
	require.Len(t, attempts, 1)
	assert.Equal(t, int64(3_000_000), attempts[0].Cost.Nanos)
}

func TestTextGatewayNativeWireOutputAndUsage(t *testing.T) {
	for _, test := range []struct {
		name, body, contentType, status, source string
		provider                                int
		stream                                  bool
		truncated                               bool
		charge                                  int64
	}{
		{name: "Ollama missing output", provider: constant.ChannelTypeOllama, contentType: "application/json", body: `{"model":"upstream-model","message":{"role":"assistant","content":"Hello"},"done":true,"prompt_eval_count":10}`, status: "success", source: "estimated", charge: 21600},
		{name: "Ollama explicit zero input missing output", provider: constant.ChannelTypeOllama, contentType: "application/json", body: `{"model":"upstream-model","message":{"role":"assistant","content":"Hello"},"done":true,"prompt_eval_count":0}`, status: "success", source: "estimated", charge: 3600},
		{name: "Ollama missing input explicit zero output", provider: constant.ChannelTypeOllama, contentType: "application/json", body: `{"model":"upstream-model","message":{"role":"assistant","content":"Hello"},"done":true,"eval_count":0}`, status: "success", source: "estimated", charge: 16200},
		{name: "Ollama stream explicit zero input missing output", provider: constant.ChannelTypeOllama, stream: true, contentType: "application/x-ndjson", body: "{\"model\":\"upstream-model\",\"message\":{\"role\":\"assistant\",\"content\":\"Hello\"},\"done\":true,\"prompt_eval_count\":0}\n", status: "success", source: "estimated", charge: 3600},
		{name: "OpenAI explicit zero input missing output", provider: constant.ChannelTypeOpenAI, contentType: "application/json", body: `{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":0}}`, status: "success", source: "estimated", charge: 3600},
		{name: "OpenAI missing input explicit zero output", provider: constant.ChannelTypeOpenAI, contentType: "application/json", body: `{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"completion_tokens":0}}`, status: "success", source: "estimated", charge: 16200},
		{name: "Dify explicit zero input missing output", provider: constant.ChannelTypeDify, contentType: "application/json", body: `{"answer":"Hello","metadata":{"usage":{"prompt_tokens":0}}}`, status: "success", source: "estimated", charge: 3600},
		{name: "OpenAI stream explicit zero input missing output", provider: constant.ChannelTypeOpenAI, stream: true, contentType: "text/event-stream", body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":0}}\n\ndata: [DONE]\n\n", status: "success", source: "estimated", charge: 3600},
		{name: "OpenAI stream missing input explicit zero output", provider: constant.ChannelTypeOpenAI, stream: true, contentType: "text/event-stream", body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":0}}\n\ndata: [DONE]\n\n", status: "success", source: "estimated", charge: 16200},
		{name: "Dify stream explicit zero input missing output", provider: constant.ChannelTypeDify, stream: true, contentType: "text/event-stream", body: "data: {\"event\":\"message\",\"answer\":\"Hello\"}\n\ndata: {\"event\":\"message_end\",\"metadata\":{\"usage\":{\"prompt_tokens\":0}}}\n\n", status: "success", source: "estimated", charge: 3600},
		{name: "Ollama read failure after output preserves partial usage", provider: constant.ChannelTypeOllama, stream: true, truncated: true, contentType: "application/x-ndjson", body: "{\"model\":\"upstream-model\",\"message\":{\"role\":\"assistant\",\"content\":\"Hello\"},\"done\":false,\"prompt_eval_count\":0}\n", status: "partial", source: "estimated", charge: 3600},
		{name: "Ollama JSON", provider: constant.ChannelTypeOllama, contentType: "application/json", body: `{"model":"upstream-model","message":{"role":"assistant","content":"Hello"},"done":true,"prompt_eval_count":10,"eval_count":5}`, status: "success", source: "upstream", charge: 36000},
		{name: "Ollama JSON explicit zero", provider: constant.ChannelTypeOllama, contentType: "application/json", body: `{"model":"upstream-model","message":{"role":"assistant","content":"Hello"},"done":true,"prompt_eval_count":0,"eval_count":0}`, status: "success", source: "upstream"},
		{name: "Ollama JSON missing usage", provider: constant.ChannelTypeOllama, contentType: "application/json", body: `{"model":"upstream-model","message":{"role":"assistant","content":"Hello"},"done":true}`, status: "success", source: "estimated", charge: 19800},
		{name: "Ollama stream", provider: constant.ChannelTypeOllama, stream: true, contentType: "application/x-ndjson", body: "{\"model\":\"upstream-model\",\"message\":{\"role\":\"assistant\",\"content\":\"Hello\"},\"done\":false}\n{\"done\":true,\"prompt_eval_count\":10,\"eval_count\":5}\n", status: "success", source: "upstream", charge: 36000},
		{name: "Ollama interrupted stream", provider: constant.ChannelTypeOllama, stream: true, contentType: "application/x-ndjson", body: "{\"model\":\"upstream-model\",\"message\":{\"role\":\"assistant\",\"content\":\"Hello\"},\"done\":false}\n", status: "partial", source: "estimated", charge: 19800},
		{name: "Dify JSON", provider: constant.ChannelTypeDify, contentType: "application/json", body: `{"answer":"Hello","conversation_id":"conversation","metadata":{"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}}`, status: "success", source: "upstream", charge: 36000},
		{name: "Dify JSON explicit zero", provider: constant.ChannelTypeDify, contentType: "application/json", body: `{"answer":"Hello","conversation_id":"conversation","metadata":{"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}}`, status: "success", source: "upstream"},
		{name: "Dify stream", provider: constant.ChannelTypeDify, stream: true, contentType: "text/event-stream", body: "data: {\"event\":\"message\",\"answer\":\"Hello\"}\n\ndata: {\"event\":\"message_end\",\"metadata\":{\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}}\n\n", status: "success", source: "upstream", charge: 36000},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				if test.truncated {
					w.Header().Set("Content-Length", fmt.Sprint(len(test.body)+100))
				}
				_, _ = w.Write([]byte(test.body))
			})
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", test.provider).Error)
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(`{"model":"gateway-alias","stream":%t,"messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`, test.stream))
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), "Hello")
			record, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, test.status, record.Status)
			assert.Equal(t, test.source, record.UsageSource)
			assert.Equal(t, test.charge, record.Receivable.Nanos)
		})
	}
}

func TestTextGatewayForeignShortfallRecordsPostedSourceAmount(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	})
	common.OptionMapRWMutex.Lock()
	common.OptionMap["GroupRatio"] = `{"default":1}`
	common.OptionMapRWMutex.Unlock()
	_, err := model.TopUpWallet(model.WalletTopUp{UserId: fixture.user.Id, Source: money.Amount{Nanos: 4_000_000_000, Currency: "USD"}, Reference: "shortfall-funds"})
	require.NoError(t, err)
	_, err = model.SaveFXRate(t.Context(), "EUR", "USD", "2", "0", fixture.sale.CreatedBy)
	require.NoError(t, err)
	sale := &model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "EUR", Expression: `c > 1 ? tier("actual",fixed(10)) : tier("reserve",fixed(1))`, EffectiveAt: clock.Now().Unix()}
	require.NoError(t, model.CreatePriceVersion(t.Context(), sale, fixture.sale.CreatedBy))
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":1}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	id := response.Header().Get(common.RequestIdKey)
	wallet, err := model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	assert.Zero(t, wallet.Balance.Nanos)
	assert.True(t, wallet.RequestsPaused)
	var entries []model.LedgerEntry
	require.NoError(t, model.DB.Where("request_id = ? AND type = ? AND account_type IN ?", id, model.LedgerEntryCharge, []model.LedgerAccountType{model.LedgerAccountUserWallet, model.LedgerAccountPlatformRevenue}).Order("id").Find(&entries).Error)
	require.Len(t, entries, 2)
	assert.Equal(t, money.Amount{Nanos: -5_000_000_000, Currency: "USD"}, entries[0].Amount)
	assert.Equal(t, money.Amount{Nanos: -2_500_000_000, Currency: "EUR"}, entries[0].SourceAmount)
	assert.Equal(t, money.Amount{Nanos: 10_000_000_000, Currency: "EUR"}, entries[1].SourceAmount)
	assert.Equal(t, "2", entries[0].FxRate.Decimal.String())
	var lifecycle model.GatewayBilling
	require.NoError(t, model.DB.First(&lifecycle, "request_id = ?", id).Error)
	var finalized struct {
		ReceivableDue    money.Amount `json:"receivable_due"`
		SaleSource       money.Amount `json:"sale_source"`
		PostedSaleSource money.Amount `json:"posted_sale_source"`
	}
	require.NoError(t, common.UnmarshalJsonStr(lifecycle.Settlement, &finalized))
	assert.Equal(t, money.Amount{Nanos: 20_000_000_000, Currency: "USD"}, finalized.ReceivableDue)
	assert.Equal(t, money.Amount{Nanos: 10_000_000_000, Currency: "EUR"}, finalized.SaleSource)
	assert.Equal(t, money.Amount{Nanos: 2_500_000_000, Currency: "EUR"}, finalized.PostedSaleSource)
}

func TestTextGatewayHistoricalOutputNormalizesChoicesBeforeReservation(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"},{"index":1,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"},{"index":2,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"},{"index":3,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":40,"total_tokens":50}}`))
	})
	first := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"n":4,"max_tokens":10}`)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	fixture.fake.Advance(time.Second)
	second := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"n":4,"max_tokens":100}`)
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	var reserve model.LedgerEntry
	require.NoError(t, model.DB.Where("request_id = ? AND type = ?", second.Header().Get(common.RequestIdKey), model.LedgerEntryPreConsume).First(&reserve).Error)
	// Nine estimated input tokens and ten output tokens for each of four choices.
	assert.Equal(t, int64(-160200), reserve.Amount.Nanos)
	requests := adminSecurityRequest(fixture.engine, fixture.adminPAT, http.MethodGet, "/api/admin/v1/requests", "")
	require.Equal(t, http.StatusOK, requests.Code, requests.Body.String())
	var payload struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(requests.Body.Bytes(), &payload))
	require.Len(t, payload.Data.Items, 2)
	assert.EqualValues(t, 4, payload.Data.Items[0]["choice_count"])
}

func TestTextGatewayDefaultOutputLimitFitsRemainingContext(t *testing.T) {
	for _, test := range []struct {
		name, override, protocol string
		provider                 int
		limit                    int
	}{
		{name: "Responses remaining context", protocol: "openai_responses", provider: constant.ChannelTypeOpenAI, limit: 23},
		{name: "Messages remaining context", protocol: "anthropic_messages", provider: constant.ChannelTypeOpenAI, limit: 23},
		{name: "Gemini remaining context", protocol: "gemini_generate", provider: constant.ChannelTypeOpenAI, limit: 23},
		{name: "OpenAI original prompt", provider: constant.ChannelTypeOpenAI, limit: 23},
		{name: "OpenAI effective prompt", provider: constant.ChannelTypeOpenAI, override: `{"messages":[{"role":"user","content":"Hello Hello"}]}`, limit: 22},
		{name: "Ollama effective prompt", provider: constant.ChannelTypeOllama, override: `{"messages":[{"role":"user","content":"Hello Hello"}]}`, limit: 22},
		{name: "Dify native prompt", provider: constant.ChannelTypeDify},
	} {
		t.Run(test.name, func(t *testing.T) {
			var sent map[string]any
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, common.DecodeJson(r.Body, &sent))
				w.Header().Set("Content-Type", "application/json")
				switch test.provider {
				case constant.ChannelTypeOllama:
					_, _ = w.Write([]byte(`{"model":"upstream-model","message":{"role":"assistant","content":"Hello"},"done":true,"prompt_eval_count":10,"eval_count":1}`))
				case constant.ChannelTypeDify:
					_, _ = w.Write([]byte(`{"answer":"Hello","metadata":{"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}}`))
				default:
					_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":1,"total_tokens":10}}`))
				}
			})
			if test.protocol != "" {
				require.NoError(t, model.DB.Model(fixture.channel).Update("protocol", test.protocol).Error)
			}
			caps := capability.Set{Features: []capability.Feature{capability.Streaming, capability.ToolCalling}, InputModalities: []capability.Modality{capability.Text}, OutputModalities: []capability.Modality{capability.Text}, Limits: capability.Limits{MaxOutputTokens: common.GetPointer(int64(32)), ContextLength: common.GetPointer(int64(32))}}
			require.NoError(t, model.DB.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Update("capabilities", caps).Error)
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", test.provider).Error)
			if test.override != "" {
				require.NoError(t, model.DB.Model(fixture.channel).Update("parameter_overrides", test.override).Error)
			}
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}]}`)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			if test.protocol == "openai_responses" {
				assert.EqualValues(t, test.limit, sent["max_output_tokens"])
			} else if test.protocol == "anthropic_messages" {
				assert.EqualValues(t, test.limit, sent["max_tokens"])
			} else if test.protocol == "gemini_generate" {
				assert.EqualValues(t, test.limit, sent["generationConfig"].(map[string]any)["maxOutputTokens"])
			} else if test.provider == constant.ChannelTypeOllama {
				assert.EqualValues(t, test.limit, sent["options"].(map[string]any)["num_predict"])
			} else if test.provider == constant.ChannelTypeDify {
				assert.NotContains(t, sent, "max_completion_tokens")
				assert.Contains(t, sent["query"], "Hello")
			} else {
				assert.EqualValues(t, test.limit, sent["max_completion_tokens"])
			}
		})
	}
}

func TestTextGatewayNativeOverridesRespectMultipliersAndCapabilities(t *testing.T) {
	for _, test := range []struct {
		name, override string
		provider       int
	}{
		{name: "Ollama multiple choices", provider: constant.ChannelTypeOllama, override: `{"n":2}`},
		{name: "Dify multiple choices", provider: constant.ChannelTypeDify, override: `{"n":2}`},
		{name: "Ollama negative choice count", provider: constant.ChannelTypeOllama, override: `{"n":-1}`},
		{name: "Dify oversized choice count", provider: constant.ChannelTypeDify, override: `{"n":129}`},
		{name: "Ollama unlimited prediction", provider: constant.ChannelTypeOllama, override: `{"options":{"num_predict":-1}}`},
		{name: "Ollama prediction exceeds capability", provider: constant.ChannelTypeOllama, override: `{"options":{"num_predict":64}}`},
		{name: "Ollama reasoning exceeds capability", provider: constant.ChannelTypeOllama, override: `{"think":true}`},
		{name: "Ollama image exceeds capability", provider: constant.ChannelTypeOllama, override: `{"messages":[{"role":"user","content":"Hello","images":["image-data"]}]}`},
		{name: "Dify invalid limit", provider: constant.ChannelTypeDify, override: `{"max_tokens":18446744073686646784}`},
		{name: "Dify stream mode mismatch", provider: constant.ChannelTypeDify, override: `{"response_mode":"streaming"}`},
		{name: "Dify image exceeds capability", provider: constant.ChannelTypeDify, override: `{"files":[{"type":"image","transfer_method":"remote_url","url":"https://example.invalid/private-image"}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				if test.provider == constant.ChannelTypeOllama {
					_, _ = w.Write([]byte(`{"model":"upstream-model","message":{"role":"assistant","content":"Hello"},"done":true,"prompt_eval_count":10,"eval_count":5}`))
				} else {
					_, _ = w.Write([]byte(`{"answer":"Hello","metadata":{"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}}`))
				}
			})
			caps := capability.Set{InputModalities: []capability.Modality{capability.Text}, OutputModalities: []capability.Modality{capability.Text}, Limits: capability.Limits{MaxOutputTokens: common.GetPointer(int64(32))}}
			require.NoError(t, model.DB.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Update("capabilities", caps).Error)
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", test.provider).Error)
			require.NoError(t, model.DB.Model(fixture.channel).Update("parameter_overrides", test.override).Error)
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
			assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			assert.Zero(t, calls)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, int64(1_000_000_000), wallet.Balance.Nanos)
		})
	}
}

func TestTextGatewayCustomToolOnlyOutputIsBillable(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_custom","type":"custom","custom":{"name":"code_exec","input":"print(1)"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	})
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "call_custom")
	requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	assert.Equal(t, "success", requests[0].Status)
	assert.Equal(t, int64(36000), requests[0].Receivable.Nanos)
}

func TestTextGatewaySameGroupSpecialDiscountIsExact(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	})
	common.OptionMapRWMutex.Lock()
	common.OptionMap["GroupGroupRatio"] = `{"default":{"default":0.500000000000000001,"vip":0.01}}`
	common.OptionMapRWMutex.Unlock()
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
	require.Equal(t, 200, response.Code, response.Body.String())
	requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	assert.Equal(t, "0.500000000000000001", requests[0].GroupRatio)
	assert.Equal(t, int64(20001), requests[0].Receivable.Nanos)
}

func unavailableGatewayDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	closed, err := sql.Open("pgx", "host=127.0.0.1 port=1 dbname=unavailable sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, closed.Close())
	unavailable, err := gorm.Open(postgres.New(postgres.Config{Conn: closed}), &gorm.Config{DisableAutomaticPing: true})
	require.NoError(t, err)
	return unavailable
}

func TestTextGatewaySettlementRecoveryProcess(t *testing.T) {
	primaryDSN := os.Getenv("GATEWAY_RECOVERY_PRIMARY_DSN")
	if primaryDSN == "" {
		t.Skip("invoked by the process-restart regression")
	}
	primary, _, err := model.OpenDSN("GATEWAY_RECOVERY_PRIMARY_DSN", primaryDSN, false)
	require.NoError(t, err)
	logs, kind, err := model.OpenDSN("GATEWAY_RECOVERY_LOG_DSN", os.Getenv("GATEWAY_RECOVERY_LOG_DSN"), true)
	require.NoError(t, err)
	model.DB, model.LOG_DB = primary, logs
	common.SetLogDatabaseType(kind)
	require.NoError(t, model.InitSettlementCurrency())
	completed, err := model.RecoverGatewaySettlements(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, 1, completed)
}

func TestTextGatewayPrimaryPreparationFailureRecoversFinalIntent(t *testing.T) {
	for _, foreignShortfall := range []bool{false, true} {
		t.Run(fmt.Sprint("foreign shortfall=", foreignShortfall), func(t *testing.T) {
			closed, err := sql.Open("pgx", "host=127.0.0.1 port=1 dbname=unavailable sslmode=disable")
			require.NoError(t, err)
			require.NoError(t, closed.Close())
			unavailable, err := gorm.Open(postgres.New(postgres.Config{Conn: closed}), &gorm.Config{DisableAutomaticPing: true})
			require.NoError(t, err)
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				model.DB = unavailable
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
			})
			journalDir := filepath.Join(t.TempDir(), "private", "settlements")
			t.Setenv("GATEWAY_SETTLEMENT_JOURNAL_DIR", journalDir)
			if foreignShortfall {
				common.OptionMapRWMutex.Lock()
				common.OptionMap["GroupRatio"] = `{"default":1}`
				common.OptionMapRWMutex.Unlock()
				_, err := model.TopUpWallet(model.WalletTopUp{UserId: fixture.user.Id, Source: money.Amount{Nanos: 4_000_000_000, Currency: "USD"}, Reference: "restart-shortfall"})
				require.NoError(t, err)
				_, err = model.SaveFXRate(t.Context(), "EUR", "USD", "2", "0", fixture.sale.CreatedBy)
				require.NoError(t, err)
				sale := &model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "EUR", Expression: `c > 1 ? tier("actual",fixed(10)) : tier("reserve",fixed(1))`, EffectiveAt: clock.Now().Unix()}
				require.NoError(t, model.CreatePriceVersion(t.Context(), sale, fixture.sale.CreatedBy))
			}
			primary := model.DB
			t.Cleanup(func() { model.DB = primary })
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":1}`)
			model.DB = primary
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			id := response.Header().Get(common.RequestIdKey)
			journalPath := filepath.Join(journalDir, id+".json")
			body, err := os.ReadFile(journalPath)
			require.NoError(t, err)
			assert.NotContains(t, string(body), "provider-secret")
			assert.NotContains(t, string(body), fixture.token.Key)
			assert.NotContains(t, string(body), "Hello")
			fileInfo, err := os.Stat(journalPath)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0600), fileInfo.Mode().Perm())
			directoryInfo, err := os.Stat(journalDir)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0700), directoryInfo.Mode().Perm())
			var originalIntent model.GatewaySettlement
			require.NoError(t, common.Unmarshal(body, &originalIntent))
			for _, name := range []string{".preflight-abandoned", ".intent-abandoned"} {
				require.NoError(t, os.WriteFile(filepath.Join(journalDir, name), []byte("unfinished"), 0600))
			}

			// Reconnect a new process to this fixture's existing isolated databases.
			var schema string
			require.NoError(t, primary.Raw("SELECT current_schema()").Scan(&schema).Error)
			primaryURL, err := url.Parse(os.Getenv("TEST_POSTGRES_DSN"))
			require.NoError(t, err)
			query := primaryURL.Query()
			query.Set("search_path", schema)
			primaryURL.RawQuery = query.Encode()
			logDSN := primaryURL.String()
			if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
				var database string
				require.NoError(t, model.LOG_DB.Raw("SELECT currentDatabase()").Scan(&database).Error)
				logURL, err := url.Parse(os.Getenv("TEST_CLICKHOUSE_LOG_DSN"))
				require.NoError(t, err)
				logURL.Path = "/" + database
				logDSN = logURL.String()
			}
			executable, err := os.Executable()
			require.NoError(t, err)
			command := exec.CommandContext(t.Context(), executable, "-test.run=^TestTextGatewaySettlementRecoveryProcess$", "-test.count=1")
			command.Env = append(os.Environ(), "GATEWAY_RECOVERY_PRIMARY_DSN="+primaryURL.String(), "GATEWAY_RECOVERY_LOG_DSN="+logDSN)
			output, err := command.CombinedOutput()
			require.NoError(t, err, "%s", output)

			// A lost acknowledgement can redeliver the original intent after the
			// primary row already contains adjusted shortfall amounts.
			model.DB = unavailable
			err = model.PrepareGatewaySettlement(originalIntent)
			model.DB = primary
			require.Error(t, err)
			gate := make(chan struct{})
			operations := []func() error{
				func() error { _, err := model.RecoverGatewaySettlements(t.Context(), 1); return err },
				func() error { return model.PrepareGatewaySettlement(originalIntent) },
				func() error { _, err := model.SettleGateway(id); return err },
			}
			failures := make(chan error, len(operations))
			var group sync.WaitGroup
			for _, operation := range operations {
				group.Go(func() { <-gate; failures <- operation() })
			}
			close(gate)
			group.Wait()
			close(failures)
			for err := range failures {
				require.NoError(t, err)
			}
			completed, err := model.RecoverGatewaySettlements(t.Context(), 1)
			require.NoError(t, err)
			assert.Zero(t, completed)
			_, err = os.Stat(journalPath)
			assert.ErrorIs(t, err, os.ErrNotExist)
			requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			_, total, err = model.ListLedgerEntries(wallet.Id, 0, 20)
			require.NoError(t, err)
			var lifecycle model.GatewayBilling
			require.NoError(t, primary.First(&lifecycle, "request_id = ?", id).Error)
			var finalized model.GatewaySettlement
			require.NoError(t, common.UnmarshalJsonStr(lifecycle.Settlement, &finalized))
			assert.Equal(t, originalIntent.ReceivableDue, finalized.ReceivableDue)
			assert.Equal(t, originalIntent.SaleSource, finalized.SaleSource)
			if foreignShortfall {
				assert.Equal(t, int64(20_000_000_000), requests[0].Receivable.Nanos)
				assert.Equal(t, money.Amount{Nanos: 2_500_000_000, Currency: "EUR"}, finalized.PostedSaleSource)
				assert.True(t, wallet.RequestsPaused)
				assert.Zero(t, wallet.Balance.Nanos)
				assert.EqualValues(t, 5, total)
			} else {
				assert.Equal(t, int64(36000), requests[0].Receivable.Nanos)
				assert.Equal(t, int64(999964000), wallet.Balance.Nanos)
				assert.EqualValues(t, 4, total)
			}
		})
	}
}

func TestTextGatewayUnavailableJournalRejectsBeforeReservation(t *testing.T) {
	calls := 0
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { calls++ })
	blocked := filepath.Join(t.TempDir(), "blocked")
	require.NoError(t, os.WriteFile(blocked, []byte("not a directory"), 0600))
	t.Setenv("GATEWAY_SETTLEMENT_JOURNAL_DIR", blocked)
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
	assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	assert.Zero(t, calls)
	wallet, err := model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	assert.Equal(t, int64(1_000_000_000), wallet.Balance.Nanos)
	_, total, err := model.ListLedgerEntries(wallet.Id, 0, 20)
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
	var lifecycles int64
	require.NoError(t, model.DB.Model(&model.GatewayBilling{}).Count(&lifecycles).Error)
	assert.Zero(t, lifecycles)
}

func TestTextGatewayMissingUsageIgnoresOutputCeilings(t *testing.T) {
	for _, test := range []struct {
		name, body, override string
		prompt               int64
	}{
		{name: "client declaration", body: `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`, prompt: 9},
		{name: "injected default is not a declaration", body: `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}]}`, prompt: 9},
		{name: "Channel override is not a declaration", body: `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`, override: `{"max_tokens":64}`, prompt: 9},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}]}`))
			})
			caps := capability.Set{Features: []capability.Feature{capability.Streaming, capability.ToolCalling}, InputModalities: []capability.Modality{capability.Text}, OutputModalities: []capability.Modality{capability.Text}, Limits: capability.Limits{MaxOutputTokens: common.GetPointer(int64(100))}}
			require.NoError(t, model.DB.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Update("capabilities", caps).Error)
			if test.override != "" {
				require.NoError(t, model.DB.Model(fixture.channel).Update("parameter_overrides", test.override).Error)
			}
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", test.body)
			require.Equal(t, 200, response.Code, response.Body.String())
			requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.Equal(t, test.prompt, requests[0].Usage.PromptTokens)
			assert.Equal(t, "estimated", requests[0].UsageSource)
		})
	}
}

func TestTextGatewayResponsesToolSnapshotsDoNotDuplicateEstimatedOutput(t *testing.T) {
	calls := 0
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"custom1\",\"type\":\"custom_tool_call\",\"name\":\"exec\",\"input\":\"\"}}\n\ndata: {\"type\":\"response.custom_tool_call_input.delta\",\"item_id\":\"custom1\",\"output_index\":0,\"delta\":\"echo Hello\"}\n\n"))
		if calls == 2 {
			_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"custom1\",\"type\":\"custom_tool_call\",\"name\":\"exec\",\"input\":\"echo Hello\"}}\n\n"))
		}
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp\",\"status\":\"completed\",\"output\":[]}}\n\n"))
	})
	require.NoError(t, model.DB.Model(fixture.channel).Update("protocol", "openai_responses").Error)
	var first *model.GatewayRequest
	for range 2 {
		response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/responses", `{"model":"gateway-offering","input":"Hello","tools":[{"type":"custom","name":"exec"}],"max_output_tokens":16,"stream":true}`)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		record, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
		require.NoError(t, err)
		assert.Equal(t, "estimated", record.UsageSource)
		assert.Greater(t, record.Usage.CompletionTokens, int64(0))
		assert.Greater(t, record.Receivable.Nanos, int64(0))
		if first == nil {
			first = record
			continue
		}
		assert.Equal(t, first.Usage.CompletionTokens, record.Usage.CompletionTokens)
		assert.Equal(t, first.Receivable.Nanos, record.Receivable.Nanos)
	}
}

func TestTextGatewayNativeHostedToolOutputIsBillable(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"search\",\"type\":\"web_search_call\",\"status\":\"in_progress\",\"action\":{\"type\":\"search\",\"query\":\"Hello\"}}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n"))
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"id":"resp","status":"completed","object":"response","output":[{"id":"search","type":"web_search_call","status":"completed","action":{"type":"search","query":"Hello"}}],"usage":{"input_tokens":10,"output_tokens":5}}`))
				}
			})
			require.NoError(t, model.DB.Model(fixture.channel).Update("protocol", "openai_responses").Error)
			body := fmt.Sprintf(`{"model":"gateway-offering","input":"Hello","tools":[{"type":"web_search_preview"}],"max_output_tokens":16,"stream":%t}`, stream)
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/responses", body)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), "web_search_call")
			record, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, "success", record.Status)
			assert.Equal(t, int64(36000), record.Receivable.Nanos)
		})
	}
}

func TestTextGatewayConvertedStreamInterruptionDoesNotComplete(t *testing.T) {
	for _, endpoint := range []struct{ path, body, failure, forbidden string }{
		{"/v1/chat/completions", `{"model":"gateway-offering","messages":[{"role":"user","content":"Hello"}],"max_tokens":16,"stream":true}`, `"error"`, "[DONE]"},
		{"/v1/responses", `{"model":"gateway-offering","input":"Hello","max_output_tokens":16,"stream":true}`, "event: response.failed", "event: response.completed"},
		{"/v1/messages", `{"model":"gateway-offering","messages":[{"role":"user","content":"Hello"}],"max_tokens":16,"stream":true}`, "event: error", "event: message_stop"},
		{"/v1beta/models/gateway-offering:streamGenerateContent?alt=sse", `{"contents":[{"role":"user","parts":[{"text":"Hello"}]}],"generationConfig":{"maxOutputTokens":16}}`, `"UNAVAILABLE"`, `"finishReason":"STOP"`},
	} {
		t.Run(endpoint.path, func(t *testing.T) {
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"Hello\"}\n\n"))
			})
			require.NoError(t, model.DB.Model(fixture.channel).Update("protocol", "openai_responses").Error)
			price := &model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: `tier("request",fixed(0.01))`, EffectiveAt: clock.Now().Unix()}
			require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, endpoint.path, endpoint.body)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), "Hello")
			assert.Contains(t, response.Body.String(), endpoint.failure)
			assert.NotContains(t, response.Body.String(), endpoint.forbidden)
			record, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, "partial", record.Status)
			assert.Zero(t, record.Receivable.Nanos)
			attempts, _, err := model.ListGatewayAttempts(t.Context(), record.ID, 0, 10)
			require.NoError(t, err)
			require.Len(t, attempts, 1)
			assert.Equal(t, int64(11000), attempts[0].Cost.Nanos)
			assert.Equal(t, int64(9), attempts[0].Usage.PromptTokens)
			assert.Equal(t, int64(1), attempts[0].Usage.CompletionTokens)
		})
	}
}

func TestTextGatewayRejectsInvalidProviderSettingsWithoutMutation(t *testing.T) {
	for _, column := range []string{"setting", "settings"} {
		t.Run(column, func(t *testing.T) {
			calls := 0
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) })
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update(column, "{").Error)
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-offering","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
			assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
			assert.Zero(t, calls)
			provider, err := model.GetProviderById(fixture.channel.ProviderID, true)
			require.NoError(t, err)
			if column == "setting" {
				require.NotNil(t, provider.Setting)
				assert.Equal(t, "{", *provider.Setting)
			} else {
				assert.Equal(t, "{", provider.OtherSettings)
			}
			_, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
			require.NoError(t, err)
			assert.Zero(t, total)
		})
	}
}

func TestTextGatewayOpenRouterSystemCacheRequiresActualPreservation(t *testing.T) {
	for _, test := range []struct {
		upstream string
		status   int
	}{{"upstream-model", 400}, {"anthropic/claude-3-7-sonnet", 200}} {
		t.Run(test.upstream, func(t *testing.T) {
			calls := 0
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
			})
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", constant.ChannelTypeOpenRouter).Error)
			require.NoError(t, model.DB.Model(fixture.channel).Update("upstream_model_name", test.upstream).Error)
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/messages", `{"model":"gateway-offering","system":[{"type":"text","text":"System instructions","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
			require.Equal(t, test.status, response.Code, response.Body.String())
			if test.status == 400 {
				assert.Zero(t, calls)
				assert.Contains(t, response.Body.String(), "cache_control")
			} else {
				assert.Equal(t, 1, calls)
				assert.Empty(t, response.Header().Get("X-Gateway-Conversion-Loss"))
			}
		})
	}
}

func TestTextGatewayOpenRouterRetainsCacheBoundaries(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var sent map[string]any
		require.NoError(t, common.DecodeJson(r.Body, &sent))
		messages := sent["messages"].([]any)
		content := messages[0].(map[string]any)["content"].([]any)
		assert.Equal(t, map[string]any{"type": "ephemeral"}, content[0].(map[string]any)["cache_control"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	})
	require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", constant.ChannelTypeOpenRouter).Error)
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/messages", `{"model":"gateway-offering","messages":[{"role":"user","content":[{"type":"text","text":"Hello","cache_control":{"type":"ephemeral"}}]}],"max_tokens":16}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Empty(t, response.Header().Get("X-Gateway-Conversion-Loss"))
}

func TestGatewayRequestSnapshotKeepsOriginalAlias(t *testing.T) {
	for _, source := range []dto.Request{
		&dto.GeneralOpenAIRequest{Model: "public-alias"},
		&dto.OpenAIResponsesRequest{Model: "public-alias"},
		&dto.ClaudeRequest{Model: "public-alias"},
		&dto.GeminiChatRequest{CachedContent: "cachedContents/original"},
	} {
		t.Run(fmt.Sprintf("%T", source), func(t *testing.T) {
			snapshot, err := service.CloneGatewayRequest(source)
			require.NoError(t, err)
			snapshot.SetModelName("candidate-model")
			if gemini, ok := snapshot.(*dto.GeminiChatRequest); ok {
				gemini.CachedContent = "cachedContents/candidate"
			}
			switch source := source.(type) {
			case *dto.GeneralOpenAIRequest:
				assert.Equal(t, "public-alias", source.Model)
			case *dto.OpenAIResponsesRequest:
				assert.Equal(t, "public-alias", source.Model)
			case *dto.ClaudeRequest:
				assert.Equal(t, "public-alias", source.Model)
			case *dto.GeminiChatRequest:
				assert.Equal(t, "cachedContents/original", source.CachedContent)
			}
		})
	}
}

func TestTextGatewayGeminiResolvesColonAlias(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	})
	fixture.offering.Aliases = append(fixture.offering.Aliases, "gateway:alias")
	require.NoError(t, model.SaveModelOffering(t.Context(), fixture.offering, "save"))
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1beta/models/gateway:alias:generateContent", `{"contents":[{"role":"user","parts":[{"text":"Hello"}]}],"generationConfig":{"maxOutputTokens":16}}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	record, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
	require.NoError(t, err)
	assert.Equal(t, "gateway:alias", record.RequestedName)
	assert.Equal(t, int64(36000), record.Receivable.Nanos)
}

func TestTextGatewayClaudeConversionPreservesExplicitZeroScalars(t *testing.T) {
	for _, test := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"gateway-offering","messages":[{"role":"user","content":"Hello"}],"max_tokens":0,"temperature":0,"top_p":0}`},
		{"/v1/responses", `{"model":"gateway-offering","input":"Hello","max_output_tokens":0,"temperature":0,"top_p":0}`},
	} {
		t.Run(test.path, func(t *testing.T) {
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				var sent map[string]any
				require.NoError(t, common.DecodeJson(r.Body, &sent))
				assert.EqualValues(t, 0, sent["max_tokens"])
				assert.EqualValues(t, 0, sent["temperature"])
				assert.EqualValues(t, 0, sent["top_p"])
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"msg","type":"message","role":"assistant","content":[{"type":"text","text":"Hello"}],"stop_reason":"end_turn","usage":{"input_tokens":0,"output_tokens":0}}`))
			})
			require.NoError(t, model.DB.Model(fixture.channel).Update("protocol", "anthropic_messages").Error)
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, test.path, test.body)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		})
	}
}

func TestTextGatewayNativeCustomToolStreamCountsBusinessOutput(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"custom1\",\"type\":\"custom_tool_call\",\"call_id\":\"call1\",\"name\":\"exec\",\"input\":\"\"}}\n\ndata: {\"type\":\"response.custom_tool_call_input.delta\",\"item_id\":\"custom1\",\"output_index\":0,\"delta\":\"echo Hello\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n"))
	})
	require.NoError(t, model.DB.Model(fixture.channel).Update("protocol", "openai_responses").Error)
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/responses", `{"model":"gateway-offering","input":"Hello","tools":[{"type":"custom","name":"exec"}],"max_output_tokens":16,"stream":true}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "echo Hello")
	requests, _, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
	require.NoError(t, err)
	require.Len(t, requests, 1)
	assert.Equal(t, "success", requests[0].Status)
	assert.Equal(t, int64(36000), requests[0].Receivable.Nanos)
}

func TestTextGatewayGeminiPreservesRequestedChoiceCount(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var sent map[string]any
		require.NoError(t, common.DecodeJson(r.Body, &sent))
		assert.EqualValues(t, 2, sent["generationConfig"].(map[string]any)["candidateCount"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"Hello"}]},"finishReason":"STOP"},{"index":1,"content":{"role":"model","parts":[{"text":"Hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}`))
	})
	require.NoError(t, model.DB.Model(fixture.channel).Update("protocol", "gemini_generate").Error)
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-offering","messages":[{"role":"user","content":"Hello"}],"max_tokens":16,"n":2}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	requests, _, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
	require.NoError(t, err)
	require.Len(t, requests, 1)
	assert.Equal(t, 2, requests[0].ChoiceCount)
}

func TestTextGatewayStreamRequestsUpstreamUsageOnConvertedCopy(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var sent map[string]any
		require.NoError(t, common.DecodeJson(r.Body, &sent))
		options, ok := sent["stream_options"].(map[string]any)
		require.True(t, ok, "upstream usage must be requested on the actual wire DTO")
		assert.Equal(t, true, options["include_usage"])
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n"))
	})
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-offering","messages":[{"role":"user","content":"Hello"}],"max_tokens":16,"stream":true}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
}

func TestTextGatewayConvertedStreamPreservesUsagePresence(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello\",\"output_index\":0,\"content_index\":0}\n\ndata: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"input_tokens_details\":{\"cached_tokens\":3,\"audio_tokens\":2}}}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"input_tokens_details\":{\"cached_tokens\":0}}}}\n\n"))
	})
	require.NoError(t, model.DB.Model(fixture.channel).Update("protocol", "openai_responses").Error)
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-offering","messages":[{"role":"user","content":"Hello"}],"max_tokens":16,"stream":true}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	requests, _, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
	require.NoError(t, err)
	require.Len(t, requests, 1)
	assert.Equal(t, "upstream", requests[0].UsageSource)
	assert.Equal(t, int64(0), requests[0].Usage.CachedTokens)
	assert.Equal(t, int64(2), requests[0].Usage.AudioInputTokens)
}

func TestTextGatewayAllEntrancesEnforceCredentialAndOfferingChecks(t *testing.T) {
	for _, endpoint := range []struct{ name, path, body string }{
		{"chat", "/v1/chat/completions", `{"model":"gateway-offering","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`},
		{"responses", "/v1/responses", `{"model":"gateway-offering","input":"Hello","max_output_tokens":16}`},
		{"messages", "/v1/messages", `{"model":"gateway-offering","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`},
		{"gemini", "/v1beta/models/gateway-offering:generateContent", `{"contents":[{"role":"user","parts":[{"text":"Hello"}]}]}`},
	} {
		t.Run(endpoint.name, func(t *testing.T) {
			calls := 0
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) })
			for _, status := range []int{common.TokenStatusDisabled, common.TokenStatusEnabled} {
				expiry := int64(-1)
				if status == common.TokenStatusEnabled {
					expiry = 1
				}
				require.NoError(t, model.DB.Model(fixture.token).Updates(map[string]any{"status": status, "expired_time": expiry}).Error)
				response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, endpoint.path, endpoint.body)
				assert.Equal(t, http.StatusUnauthorized, response.Code, response.Body.String())
			}
			require.NoError(t, model.DB.Model(fixture.token).Updates(map[string]any{"status": common.TokenStatusEnabled, "expired_time": -1}).Error)
			fixture.offering.Visibility, fixture.offering.Groups = "groups", []string{"paid"}
			require.NoError(t, model.SaveModelOffering(t.Context(), fixture.offering, "save"))
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, endpoint.path, endpoint.body)
			assert.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), "model_not_found")
			assert.Zero(t, calls)
			_, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
			require.NoError(t, err)
			assert.Zero(t, total)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, int64(1_000_000_000), wallet.Balance.Nanos)
		})
	}
}

func TestTextGatewayProtocolPreference(t *testing.T) {
	for _, exact := range []bool{false, true} {
		t.Run(fmt.Sprintf("same-protocol=%t", exact), func(t *testing.T) {
			path := ""
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				path = r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/responses" {
					_, _ = w.Write([]byte(`{"id":"resp","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`))
				} else {
					_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
				}
			})
			require.NoError(t, model.DB.Model(fixture.channel).Update("protocol", "anthropic_messages").Error)
			good := *fixture.channel
			good.ID, good.Protocol = 0, "openai_responses"
			require.NoError(t, model.DB.Create(&good).Error)
			price := *fixture.purchase
			price.ID, price.OwnerID = 0, good.ID
			require.NoError(t, model.CreatePriceVersion(t.Context(), &price, fixture.purchase.CreatedBy))
			chosen := good.ID
			if exact {
				same := good
				same.ID, same.Protocol = 0, "openai_chat"
				require.NoError(t, model.DB.Create(&same).Error)
				price.ID, price.OwnerID = 0, same.ID
				require.NoError(t, model.CreatePriceVersion(t.Context(), &price, fixture.purchase.CreatedBy))
				chosen = same.ID
			}
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-offering","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			if exact {
				assert.Equal(t, "/v1/chat/completions", path)
			} else {
				assert.Equal(t, "/v1/responses", path)
			}
			requests, _, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
			require.NoError(t, err)
			require.Len(t, requests, 1)
			attempts, _, err := model.ListGatewayAttempts(t.Context(), requests[0].ID, 0, 10)
			require.NoError(t, err)
			require.Len(t, attempts, 1)
			assert.Equal(t, chosen, attempts[0].ChannelID)
		})
	}
}

func TestTextGatewayStreamConversionFacts(t *testing.T) {
	for _, downstream := range []struct{ name, path, body, frame string }{
		{"chat", "/v1/chat/completions", `{"model":"gateway-offering","messages":[{"role":"user","content":"Hello"}],"max_tokens":16,"stream":true}`, `"choices"`},
		{"responses", "/v1/responses", `{"model":"gateway-offering","input":"Hello","max_output_tokens":16,"stream":true}`, "event: response.output_text.delta"},
		{"messages", "/v1/messages", `{"model":"gateway-offering","messages":[{"role":"user","content":"Hello"}],"max_tokens":16,"stream":true}`, "event: content_block_delta"},
		{"gemini", "/v1beta/models/gateway-offering:streamGenerateContent?alt=sse", `{"contents":[{"role":"user","parts":[{"text":"Hello"}]}],"generationConfig":{"maxOutputTokens":16}}`, `"candidates"`},
	} {
		for _, upstream := range []struct{ protocol, events string }{
			{"openai_chat", "data: {\"error\":null,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\ndata: [DONE]\n\n"},
			{"openai_responses", "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"Hello\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp\",\"object\":\"response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"total_tokens\":15}}}\n\n"},
			{"anthropic_messages", "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":10}}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\ndata: {\"type\":\"message_stop\"}\n\n"},
			{"gemini_generate", "data: {\"candidates\":[{\"index\":0,\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Hello\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":5,\"totalTokenCount\":15}}\n\n"},
		} {
			t.Run(downstream.name+"/"+upstream.protocol, func(t *testing.T) {
				fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte(upstream.events))
				})
				require.NoError(t, model.DB.Model(fixture.channel).Update("protocol", upstream.protocol).Error)
				response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, downstream.path, downstream.body)
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				assert.Contains(t, response.Body.String(), downstream.frame)
				assert.Contains(t, response.Body.String(), "Hello")
				requests, _, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
				require.NoError(t, err)
				require.Len(t, requests, 1)
				assert.Equal(t, "success", requests[0].Status)
				assert.Equal(t, "upstream", requests[0].UsageSource)
				assert.Equal(t, int64(36000), requests[0].Receivable.Nanos)
				attempts, _, err := model.ListGatewayAttempts(t.Context(), requests[0].ID, 0, 10)
				require.NoError(t, err)
				require.Len(t, attempts, 1)
				assert.Equal(t, int64(20000), attempts[0].Cost.Nanos)
			})
		}
	}
}

func TestTextGatewayStrictConversionLosses(t *testing.T) {
	for _, test := range []struct {
		name, path, body, upstream, code, parameter string
		hard                                        bool
	}{
		{"blocked domains", "/v1/messages", `{"model":"gateway-offering","messages":[{"role":"user","content":"Hello"}],"max_tokens":16,"tools":[{"type":"web_search_20250305","name":"web_search","blocked_domains":["example.com"]}]}`, "openai_responses", "unsupported_blocked_domains", "blocked_domains", false},
		{"stateful response", "/v1/responses", `{"model":"gateway-offering","input":"Hello","max_output_tokens":16,"previous_response_id":"resp-private"}`, "openai_chat", "stateful_field_unsupported", "previous_response_id", true},
		{"cache control", "/v1/messages", `{"model":"gateway-offering","messages":[{"role":"user","content":[{"type":"text","text":"Hello","cache_control":{"type":"ephemeral"}}]}],"max_tokens":16}`, "openai_chat", "cache_control_dropped", "cache_control", false},
		{"Gemini cached content", "/v1beta/models/gateway-offering:generateContent", `{"contents":[{"role":"user","parts":[{"text":"Hello"}]}],"cachedContent":"cachedContents/example","generationConfig":{"maxOutputTokens":16}}`, "openai_chat", "cache_parameter_dropped", "cachedContent", false},
		{"Gemini response schema", "/v1beta/models/gateway-offering:generateContent", `{"contents":[{"role":"user","parts":[{"text":"Hello"}]}],"generationConfig":{"maxOutputTokens":16,"responseMimeType":"application/json","responseSchema":{"type":"OBJECT","properties":{"x":{"type":"STRING"}}}}}`, "openai_chat", "response_schema_dropped", "responseSchema", false},
		{"OpenAI cache options", "/v1/chat/completions", `{"model":"gateway-offering","messages":[{"role":"user","content":"Hello"}],"max_tokens":16,"prompt_cache_options":{"type":"ephemeral"}}`, "openai_responses", "cache_parameter_dropped", "prompt_cache_options", false},
		{"Gemini schema", "/v1beta/models/gateway-offering:generateContent", `{"contents":[{"role":"user","parts":[{"text":"Hello"}]}],"generationConfig":{"maxOutputTokens":16},"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"OBJECT","properties":{"x":{"type":"STRING"}},"propertyOrdering":["x"]}}]}]}`, "openai_chat", "gemini_schema_keyword_dropped", "parameters", false},
	} {
		for _, policy := range []string{"blocked", "offering", "global"} {
			allowed := policy != "blocked"
			t.Run(fmt.Sprintf("%s/%s", test.name, policy), func(t *testing.T) {
				calls := 0
				fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
					calls++
					w.Header().Set("Content-Type", "application/json")
					if test.upstream == "openai_responses" {
						_, _ = w.Write([]byte(`{"id":"resp","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`))
					} else {
						_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
					}
				})
				require.NoError(t, model.DB.Model(fixture.channel).Update("protocol", test.upstream).Error)
				caps := capability.Set{Features: []capability.Feature{capability.Streaming, capability.ToolCalling, capability.Caching, capability.StructuredOutput}, InputModalities: []capability.Modality{capability.Text}, OutputModalities: []capability.Modality{capability.Text}}
				require.NoError(t, model.DB.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Update("capabilities", caps).Error)
				if policy == "global" {
					require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", fixture.sale.CreatedBy).Update("role", common.RoleRootUser).Error)
					rootPAT := createWalletTestAccessToken(t, fixture.sale.CreatedBy, "option:write", "option:read")
					if test.name == "cache control" {
						for _, invalid := range []string{`{}`, `[1]`, `null`, `[""]`} {
							body, err := common.Marshal(map[string]string{"key": "global.accepted_conversion_losses", "value": invalid})
							require.NoError(t, err)
							invalidResponse := adminSecurityRequest(fixture.engine, rootPAT, http.MethodPut, "/api/option/", string(body))
							var result struct {
								Success bool `json:"success"`
							}
							require.NoError(t, common.Unmarshal(invalidResponse.Body.Bytes(), &result))
							assert.False(t, result.Success, invalidResponse.Body.String())
						}
					}
					codes, err := common.Marshal([]string{test.code})
					require.NoError(t, err)
					body, err := common.Marshal(map[string]string{"key": "global.accepted_conversion_losses", "value": string(codes)})
					require.NoError(t, err)
					saved := adminSecurityRequest(fixture.engine, rootPAT, http.MethodPut, "/api/option/", string(body))
					var result struct {
						Success bool `json:"success"`
					}
					require.NoError(t, common.Unmarshal(saved.Body.Bytes(), &result))
					require.True(t, result.Success, saved.Body.String())
					t.Cleanup(func() { require.NoError(t, model.UpdateOption("global.accepted_conversion_losses", "[]")) })
				} else if policy == "offering" {
					fixture.offering.AcceptedConversionLosses = []string{test.code}
					require.NoError(t, model.SaveModelOffering(t.Context(), fixture.offering, "save"))
				}
				response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, test.path, test.body)
				if !allowed || test.hard {
					assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
					assert.Contains(t, response.Body.String(), "unsupported_parameter")
					assert.Contains(t, response.Body.String(), test.parameter)
					var envelope map[string]any
					require.NoError(t, common.Unmarshal(response.Body.Bytes(), &envelope))
					protocolError, ok := envelope["error"].(map[string]any)
					require.True(t, ok, "expected protocol error envelope")
					switch {
					case test.path == "/v1/messages":
						assert.Equal(t, "error", envelope["type"])
						assert.Equal(t, "unsupported_parameter", protocolError["type"])
					case strings.HasPrefix(test.path, "/v1beta/"):
						assert.EqualValues(t, 400, protocolError["code"])
						assert.Equal(t, "INVALID_ARGUMENT", protocolError["status"])
					default:
						assert.Equal(t, "unsupported_parameter", protocolError["code"])
					}
					assert.NotContains(t, response.Body.String(), "provider-secret")
					assert.NotContains(t, response.Body.String(), "resp-private")
					assert.Zero(t, calls)
					_, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
					require.NoError(t, err)
					assert.Zero(t, total)
					wallet, err := model.GetUserWallet(fixture.user.Id)
					require.NoError(t, err)
					assert.Equal(t, int64(1_000_000_000), wallet.Balance.Nanos)
					return
				}
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				assert.Equal(t, 1, calls)
				assert.Equal(t, test.code, response.Header().Get("X-Gateway-Conversion-Loss"))
				requests, _, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
				require.NoError(t, err)
				require.Len(t, requests, 1)
				assert.Equal(t, int64(36000), requests[0].Receivable.Nanos)
				attempts, _, err := model.ListGatewayAttempts(t.Context(), requests[0].ID, 0, 10)
				require.NoError(t, err)
				require.Len(t, attempts, 1)
				assert.Equal(t, []string{test.code}, attempts[0].ConversionLosses)
				assert.Equal(t, int64(20000), attempts[0].Cost.Nanos)
			})
		}
	}
}

func TestTextGatewayFourDownstreamEntrances(t *testing.T) {
	for _, test := range []struct{ name, path, body, responseField string }{
		{"chat", "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`, "choices"},
		{"responses", "/v1/responses", `{"model":"gateway-alias","input":"Hello","max_output_tokens":16}`, "output"},
		{"messages", "/v1/messages", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`, "content"},
		{"gemini", "/v1beta/models/gateway-alias:generateContent", `{"contents":[{"role":"user","parts":[{"text":"Hello"}]}],"generationConfig":{"maxOutputTokens":16}}`, "candidates"},
	} {
		for _, upstream := range []struct{ protocol, body string }{
			{"openai_chat", `{"id":"chat-test","object":"chat.completion","model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`},
			{"openai_responses", `{"id":"resp-test","object":"response","status":"completed","error":null,"model":"upstream-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`},
			{"anthropic_messages", `{"id":"msg","type":"message","role":"assistant","model":"upstream-model","content":[{"type":"text","text":"Hello"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`},
			{"gemini_generate", `{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}`},
		} {
			t.Run(test.name+"/"+upstream.protocol, func(t *testing.T) {
				calls := 0
				fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
					calls++
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(upstream.body))
				})
				require.NoError(t, model.DB.Model(fixture.channel).Update("protocol", upstream.protocol).Error)
				response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, test.path, test.body)
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				assert.Equal(t, 1, calls)
				var payload map[string]any
				require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
				assert.Contains(t, payload, test.responseField)
				requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
				require.NoError(t, err)
				require.EqualValues(t, 1, total)
				assert.Equal(t, "gateway-alias", requests[0].RequestedName)
				assert.Equal(t, int64(36000), requests[0].Receivable.Nanos)
				attempts, _, err := model.ListGatewayAttempts(t.Context(), requests[0].ID, 0, 10)
				require.NoError(t, err)
				require.Len(t, attempts, 1)
				assert.Equal(t, int64(20000), attempts[0].Cost.Nanos)
			})
		}
	}
}

func TestTextGatewaySelectsExplicitResponsesProtocol(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/responses", r.URL.Path)
		var body map[string]any
		require.NoError(t, common.DecodeJson(r.Body, &body))
		assert.EqualValues(t, 0, body["temperature"])
		assert.Equal(t, false, body["parallel_tool_calls"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-test","object":"response","status":"completed","model":"upstream-model","output":[{"id":"msg","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello","annotations":[]}]}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`))
	})
	require.NoError(t, model.DB.Model(fixture.channel).Update("protocol", "openai_responses").Error)
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-offering","messages":[{"role":"user","content":"Hello"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{}}}}],"temperature":0,"parallel_tool_calls":false,"max_tokens":16}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "Hello")
	requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	assert.Equal(t, int64(36000), requests[0].Receivable.Nanos)
	attempts, _, err := model.ListGatewayAttempts(t.Context(), requests[0].ID, 0, 10)
	require.NoError(t, err)
	require.Len(t, attempts, 1)
	assert.Equal(t, int64(20000), attempts[0].Cost.Nanos)
}

func TestTextGatewayAliasBillsWalletAndRecordsAttempt(t *testing.T) {
	calls := 0
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, "/v1/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer provider-secret", r.Header.Get("Authorization"))
		var body map[string]any
		require.NoError(t, common.DecodeJson(r.Body, &body))
		assert.Equal(t, "upstream-model", body["model"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat-test","object":"chat.completion","model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	})
	otherProvider := &model.Provider{Name: "Other scope Provider", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Key: "unused"}
	require.NoError(t, model.DB.Create(otherProvider).Error)
	fixture.offering.ChannelScope.IncludeProviderIDs = []int{otherProvider.Id}
	fixture.offering.ChannelScope.IncludeChannelIDs = []int{fixture.channel.ID}
	require.NoError(t, model.SaveModelOffering(t.Context(), fixture.offering, "save"))
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, 1, calls)
	assert.NotEmpty(t, response.Header().Get(common.RequestIdKey))
	wallet, err := model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	assert.Equal(t, int64(999_964_000), wallet.Balance.Nanos)
	entries, _, err := model.ListLedgerEntries(wallet.Id, 0, 10)
	require.NoError(t, err)
	require.Len(t, entries, 4)
	assert.Equal(t, model.LedgerEntryCharge, entries[0].Type)
	assert.Equal(t, int64(-36_000), entries[0].Amount.Nanos)
	assert.Equal(t, response.Header().Get(common.RequestIdKey), entries[0].RequestId)
	requests := walletAPIRequest(t, fixture.engine, fixture.adminPAT, http.MethodGet, "/api/admin/v1/requests?page=1&page_size=10", "")
	require.True(t, requests.Success, requests.Message)
	require.EqualValues(t, 1, requests.Data["total"])
	request := requests.Data["items"].([]any)[0].(map[string]any)
	assert.Equal(t, response.Header().Get(common.RequestIdKey), request["id"])
	assert.Equal(t, "gateway-alias", request["requested_name"])
	assert.Equal(t, "gateway-offering", request["offering_name"])
	assert.Equal(t, "0.9", request["group_ratio"])
	assert.Equal(t, "upstream", request["usage_source"])
	assert.Equal(t, map[string]any{"nanos": "36000", "currency": "USD"}, request["receivable"])
	assert.EqualValues(t, 1794000000000, request["started_at"])
	attempts := walletAPIRequest(t, fixture.engine, fixture.adminPAT, http.MethodGet, "/api/admin/v1/requests/"+request["id"].(string)+"/attempts", "")
	require.True(t, attempts.Success, attempts.Message)
	require.EqualValues(t, 1, attempts.Data["total"])
	attempt := attempts.Data["items"].([]any)[0].(map[string]any)
	assert.EqualValues(t, fixture.channel.ID, attempt["channel_id"])
	assert.Equal(t, map[string]any{"nanos": "20000", "currency": "USD"}, attempt["cost"])
	assert.Contains(t, attempt, "route_diagnostics")
	reconciliation, err := model.ReconcileWallets(100)
	require.NoError(t, err)
	assert.Empty(t, reconciliation.Mismatches)
}

func TestTextGatewayNoOutputFailureReleasesReservation(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-offering","stream":true,"messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	assert.Equal(t, "failed", requests[0].Status)
	assert.Zero(t, requests[0].Receivable.Nanos)
	attempts, _, err := model.ListGatewayAttempts(t.Context(), requests[0].ID, 0, 10)
	require.NoError(t, err)
	require.Len(t, attempts, 1)
	assert.Zero(t, attempts[0].Cost.Nanos, "an upstream with no business output and no reported usage has no inferred procurement cost")
	assert.Equal(t, "none", attempts[0].UsageSource)
	wallet, err := model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	assert.Equal(t, int64(1_000_000_000), wallet.Balance.Nanos)
}

func TestTextGatewayJSONAndStreamUsage(t *testing.T) {
	for _, test := range []struct {
		name       string
		stream     bool
		body       string
		status     string
		source     string
		prompt     int64
		completion int64
		receivable int64
		cost       int64
	}{
		{name: "JSON reported zero", body: `{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`, status: "success", source: "upstream"},
		{name: "JSON missing usage", body: `{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}]}`, status: "success", source: "estimated", prompt: 9, completion: 1, receivable: 19800, cost: 11000},
		{name: "JSON empty usage object", body: `{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{}}`, status: "success", source: "estimated", prompt: 9, completion: 1, receivable: 19800, cost: 11000},
		{name: "SSE cumulative usage before trailing frames", stream: true, body: "data: {\"id\":\"stream\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\ndata: {\"choices\":[]}\n\ndata: {\"choices\":[]}\n\ndata: [DONE]\n\n", status: "success", source: "upstream", prompt: 10, completion: 5, receivable: 36000, cost: 20000},
		{name: "SSE reported zero", stream: true, body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0,\"total_tokens\":0}}\n\ndata: [DONE]\n\n", status: "success", source: "upstream"},
		{name: "SSE missing usage", stream: true, body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", status: "success", source: "estimated", prompt: 9, completion: 1, receivable: 19800, cost: 11000},
		{name: "SSE interrupted after output", stream: true, body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}]}\n\n", status: "partial", source: "estimated", prompt: 9, completion: 1, receivable: 19800, cost: 11000},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if test.stream {
					w.Header().Set("Content-Type", "text/event-stream")
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				_, _ = w.Write([]byte(test.body))
			})
			body := fmt.Sprintf(`{"model":"gateway-offering","stream":%t,"messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`, test.stream)
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", body)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), "Hello")
			requests, _, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
			require.NoError(t, err)
			require.Len(t, requests, 1)
			assert.Equal(t, test.status, requests[0].Status)
			assert.Equal(t, test.source, requests[0].UsageSource)
			assert.Equal(t, test.prompt, requests[0].Usage.PromptTokens)
			assert.Equal(t, test.completion, requests[0].Usage.CompletionTokens)
			assert.Equal(t, test.receivable, requests[0].Receivable.Nanos)
			attempts, _, err := model.ListGatewayAttempts(t.Context(), requests[0].ID, 0, 10)
			require.NoError(t, err)
			require.Len(t, attempts, 1)
			assert.Equal(t, test.source, attempts[0].UsageSource)
			assert.Equal(t, test.cost, attempts[0].Cost.Nanos)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, int64(1_000_000_000)-test.receivable, wallet.Balance.Nanos)
		})
	}
}

func TestTextGatewayFreezesPriceVersionsAndAccountingFX(t *testing.T) {
	var fixture textGatewayFixture
	fixture = newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		fixture.fake.Advance(time.Second)
		actor := fixture.sale.CreatedBy
		for _, price := range []*model.PriceVersion{
			{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "EUR", Expression: `tier("changed",fixed(0.5))`, EffectiveAt: clock.Now().Unix()},
			{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "JPY", Expression: `tier("changed",fixed(0.2))`, EffectiveAt: clock.Now().Unix()},
		} {
			require.NoError(t, model.CreatePriceVersion(t.Context(), price, actor))
		}
		_, err := model.SaveFXRate(t.Context(), "EUR", "USD", "4", "0.02", actor)
		require.NoError(t, err)
		_, err = model.SaveFXRate(t.Context(), "JPY", "USD", "0.4", "0.02", actor)
		require.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	})
	actor := fixture.sale.CreatedBy
	sale := &model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "EUR", Expression: fixture.sale.Expression, EffectiveAt: clock.Now().Unix()}
	purchase := &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "JPY", Expression: fixture.purchase.Expression, EffectiveAt: clock.Now().Unix()}
	require.NoError(t, model.CreatePriceVersion(t.Context(), sale, actor))
	require.NoError(t, model.CreatePriceVersion(t.Context(), purchase, actor))
	_, err := model.SaveFXRate(t.Context(), "EUR", "USD", "2", "0.02", actor)
	require.NoError(t, err)
	_, err = model.SaveFXRate(t.Context(), "JPY", "USD", "0.2", "0.02", actor)
	require.NoError(t, err)
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	requests, _, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
	require.NoError(t, err)
	require.Len(t, requests, 1)
	assert.Equal(t, sale.ID, requests[0].SalePriceVersionID)
	assert.Equal(t, sale.Expression, requests[0].SaleExpression)
	assert.Equal(t, "2", requests[0].SaleFXRate)
	assert.Equal(t, int64(72000), requests[0].Receivable.Nanos)
	attempts, _, err := model.ListGatewayAttempts(t.Context(), requests[0].ID, 0, 10)
	require.NoError(t, err)
	require.Len(t, attempts, 1)
	assert.Equal(t, purchase.ID, attempts[0].PurchasePriceVersionID)
	assert.Equal(t, purchase.Expression, attempts[0].PurchaseExpression)
	assert.Equal(t, "0.2", attempts[0].AccountingFXRate)
	assert.Equal(t, money.Amount{Nanos: 20000, Currency: "JPY"}, attempts[0].Cost)
	assert.Equal(t, money.Amount{Nanos: 4000, Currency: "USD"}, attempts[0].ConvertedCost)
	var entries []model.LedgerEntry
	require.NoError(t, model.DB.Where("request_id = ?", requests[0].ID).Order("id").Find(&entries).Error)
	require.Len(t, entries, 5)
	assert.Equal(t, "2", entries[2].FxRate.Decimal.String())
	assert.Equal(t, money.Amount{Nanos: -36000, Currency: "EUR"}, entries[2].SourceAmount)
	assert.Equal(t, "0.2", entries[4].FxRate.Decimal.String())
	assert.Equal(t, money.Amount{Nanos: 20000, Currency: "JPY"}, entries[4].Amount)
}

type cancelOnOutputWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w cancelOnOutputWriter) Write(body []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(body)
	if strings.Contains(string(body), "Hello") {
		w.cancel()
	}
	return n, err
}

func (w cancelOnOutputWriter) WriteString(body string) (int, error) {
	n, err := w.ResponseRecorder.WriteString(body)
	if strings.Contains(body, "Hello") {
		w.cancel()
	}
	return n, err
}

func TestTextGatewayCancellationReleasesOrChargesReportedUsage(t *testing.T) {
	for _, output := range []bool{false, true} {
		t.Run(fmt.Sprint("output=", output), func(t *testing.T) {
			entered, cancelled := make(chan struct{}), make(chan struct{})
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				close(entered)
				if output {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\ndata: {\"choices\":[]}\n\n"))
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
				close(cancelled)
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gateway-alias","stream":true,"messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)).WithContext(ctx)
			request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { fixture.engine.ServeHTTP(cancelOnOutputWriter{recorder, cancel}, request); close(done) }()
			<-entered
			if !output {
				cancel()
			}
			<-done
			<-cancelled
			requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.Equal(t, "cancelled", requests[0].Status)
			attempts, total, err := model.ListGatewayAttempts(t.Context(), requests[0].ID, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.Equal(t, "client_cancelled", attempts[0].ErrorClass)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			if output {
				assert.Equal(t, "upstream", attempts[0].UsageSource)
				assert.Equal(t, int64(36000), requests[0].Receivable.Nanos)
				assert.Equal(t, int64(20000), attempts[0].Cost.Nanos)
				assert.Equal(t, int64(999964000), wallet.Balance.Nanos)
			} else {
				assert.Zero(t, requests[0].Receivable.Nanos)
				assert.Zero(t, attempts[0].Cost.Nanos)
				assert.Equal(t, int64(1000000000), wallet.Balance.Nanos)
			}
		})
	}
}

func TestTextGatewayMissingAndFuturePricesNeverDispatch(t *testing.T) {
	for _, owner := range []string{model.PriceOwnerOffering, model.PriceOwnerChannel} {
		for _, future := range []bool{false, true} {
			t.Run(fmt.Sprint(owner, " future=", future), func(t *testing.T) {
				calls := 0
				fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { calls++ })
				name := "gateway-alias"
				ownerID := 0
				if owner == model.PriceOwnerOffering {
					offering := &model.ModelOffering{Name: "unpriced-offering", Members: []model.ModelOfferingMember{{ModelID: fixture.channel.ModelID, Tier: 1}}}
					require.NoError(t, model.SaveModelOffering(t.Context(), offering, "save"))
					// Simulate an enabled historical row whose current price is absent.
					require.NoError(t, model.DB.Model(offering).Update("enabled", true).Error)
					ownerID, name = offering.ID, offering.Name
				} else {
					require.NoError(t, model.DB.Model(fixture.channel).Update("status", model.ChannelDisabled).Error)
					channel := &model.Channel{ProviderID: fixture.channel.ProviderID, ModelID: fixture.channel.ModelID, Protocol: fixture.channel.Protocol, UpstreamModelName: "unpriced-upstream", ParameterOverrides: `{}`, MappingConfirmed: true, Status: model.ChannelEnabled, Weight: 1}
					require.NoError(t, model.DB.Create(channel).Error)
					ownerID = channel.ID
				}
				if future {
					price := &model.PriceVersion{OwnerType: owner, OwnerID: ownerID, Currency: "USD", Expression: `tier("future",p+c)`, EffectiveAt: clock.Now().Unix() + 60}
					require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
				}
				response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`, name))
				assert.Equal(t, 503, response.Code, response.Body.String())
				assert.Contains(t, response.Body.String(), "price_unavailable")
				assert.Zero(t, calls)
				wallet, err := model.GetUserWallet(fixture.user.Id)
				require.NoError(t, err)
				assert.Equal(t, int64(1000000000), wallet.Balance.Nanos)
			})
		}
	}
}

func TestTextGatewayExactConditionalCacheNormalization(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":4}}}`))
	})
	price := &model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: `0.123456789123456789 == 0.123456789123456788 ? tier("plain",p*2+c*4) : tier("cached",p*2+cr*0.5+c*4)`, EffectiveAt: clock.Now().Unix()}
	require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
	require.Equal(t, 200, response.Code, response.Body.String())
	requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	assert.Equal(t, int64(30600), requests[0].Receivable.Nanos)
	assert.Equal(t, int64(4), requests[0].Usage.CachedTokens)
}

func TestTextGatewayRecoveryPublishesOnceWithoutRecharging(t *testing.T) {
	for _, free := range []bool{false, true} {
		t.Run(fmt.Sprint("free=", free), func(t *testing.T) {
			closed, err := sql.Open("pgx", "host=127.0.0.1 port=1 dbname=unavailable sslmode=disable")
			require.NoError(t, err)
			require.NoError(t, closed.Close())
			unavailable, err := gorm.Open(postgres.New(postgres.Config{Conn: closed}), &gorm.Config{DisableAutomaticPing: true})
			require.NoError(t, err)
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				model.LOG_DB = unavailable
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
			})
			logs := model.LOG_DB
			t.Cleanup(func() { model.LOG_DB = logs })
			charge := int64(36000)
			if free {
				charge = 0
				price := &model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: `tier("request",fixed(0))`, EffectiveAt: clock.Now().Unix()}
				require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
			}
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
			model.LOG_DB = logs
			require.Equal(t, 200, response.Code, response.Body.String())
			id := response.Header().Get(common.RequestIdKey)
			_, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
			require.NoError(t, err)
			assert.Zero(t, total)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, int64(1000000000)-charge, wallet.Balance.Nanos)
			completed, err := model.RecoverGatewaySettlements(t.Context(), 100)
			require.NoError(t, err)
			assert.Equal(t, 1, completed)
			completed, err = model.RecoverGatewaySettlements(t.Context(), 100)
			require.NoError(t, err)
			assert.Zero(t, completed)
			request, err := model.SettleGateway(id)
			require.NoError(t, err)
			assert.Equal(t, charge, request.Receivable.Nanos)
			require.NoError(t, model.PublishGatewayRecords(id))
			_, total, err = model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
			require.NoError(t, err)
			assert.EqualValues(t, 1, total)
			_, total, err = model.ListGatewayAttempts(t.Context(), id, 0, 10)
			require.NoError(t, err)
			assert.EqualValues(t, 1, total)
			wallet, err = model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, int64(1000000000)-charge, wallet.Balance.Nanos)
			_, total, err = model.ListLedgerEntries(wallet.Id, 0, 10)
			require.NoError(t, err)
			if free {
				assert.EqualValues(t, 1, total)
			} else {
				assert.EqualValues(t, 4, total)
			}
			reconciliation, err := model.ReconcileWallets(100)
			require.NoError(t, err)
			assert.Empty(t, reconciliation.Mismatches)
		})
	}
}

func TestTextGatewayShortfallPausesUntilTopUp(t *testing.T) {
	calls := 0
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":250000,"completion_tokens":250000,"total_tokens":500000}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	})
	body := `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`
	first := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", body)
	require.Equal(t, 200, first.Code, first.Body.String())
	wallet, err := model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	assert.Zero(t, wallet.Balance.Nanos)
	assert.True(t, wallet.RequestsPaused)
	requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	assert.Equal(t, int64(1350000000), requests[0].Receivable.Nanos)
	assert.Equal(t, "wallet_shortfall", requests[0].ErrorCode)
	blocked := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", body)
	assert.Equal(t, 402, blocked.Code, blocked.Body.String())
	assert.Equal(t, 1, calls)
	_, err = model.TopUpWallet(model.WalletTopUp{UserId: fixture.user.Id, Source: money.Amount{Nanos: 450000000, Currency: "USD"}, Reference: "shortfall-recharge"})
	require.NoError(t, err)
	wallet, err = model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	assert.False(t, wallet.RequestsPaused)
	resumed := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", body)
	require.Equal(t, 200, resumed.Code, resumed.Body.String())
	assert.Equal(t, 2, calls)
	wallet, err = model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	assert.Equal(t, int64(99964000), wallet.Balance.Nanos)
	reconciliation, err := model.ReconcileWallets(100)
	require.NoError(t, err)
	assert.Empty(t, reconciliation.Mismatches)
}

func TestTextGatewayPartialUsageRetainsCacheDetails(t *testing.T) {
	for _, test := range []struct {
		name                 string
		stream, split, clear bool
		cache, charge, cost  int64
	}{
		{name: "JSON partial", cache: 100, charge: 45000, cost: 50000},
		{name: "SSE partial", stream: true, cache: 100, charge: 45000, cost: 50000},
		{name: "SSE split counters retain details", stream: true, split: true, cache: 100, charge: 45000, cost: 50000},
		{name: "SSE explicit zero clears details", stream: true, split: true, clear: true, charge: 180000, cost: 200000},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				body := `{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":100}}}`
				if test.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					body = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"prompt_tokens_details\":{\"cached_tokens\":100}}}\n\n"
					if test.split {
						body += "data: {\"choices\":[],\"usage\":{\"completion_tokens\":1}}\n\n"
					}
					if test.clear {
						body += "data: {\"choices\":[],\"usage\":{\"prompt_tokens_details\":{\"cached_tokens\":0}}}\n\n"
					}
					body += "data: [DONE]\n\n"
				}
				_, _ = w.Write([]byte(body))
			})
			for _, owner := range []struct {
				kind string
				id   int
			}{{model.PriceOwnerOffering, fixture.offering.ID}, {model.PriceOwnerChannel, fixture.channel.ID}} {
				price := &model.PriceVersion{OwnerType: owner.kind, OwnerID: owner.id, Currency: "USD", Expression: `tier("cache", p * 2 + cr * 0.5)`, EffectiveAt: clock.Now().Unix()}
				require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
			}
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(`{"model":"gateway-offering","stream":%t,"messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`, test.stream))
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			record, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
			require.NoError(t, err)
			source := "estimated"
			if test.split {
				source = "upstream"
			}
			assert.Equal(t, source, record.UsageSource)
			assert.Equal(t, int64(100), record.Usage.PromptTokens)
			assert.Equal(t, test.cache, record.Usage.CachedTokens)
			assert.Equal(t, test.charge, record.Receivable.Nanos)
			attempts, _, err := model.ListGatewayAttempts(t.Context(), record.ID, 0, 10)
			require.NoError(t, err)
			require.Len(t, attempts, 1)
			assert.Equal(t, test.cost, attempts[0].Cost.Nanos)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, int64(1000000000)-test.charge, wallet.Balance.Nanos)
		})
	}
}

func TestTextGatewayFreeFixedPricesAndFailureCosts(t *testing.T) {
	for _, test := range []struct {
		name, expression, body, status, source string
		protocol                               string
		upstreamStatus                         int
		stream                                 bool
		charge, cost                           int64
	}{
		{name: "Responses failed fixed request refunds", protocol: "openai_responses", expression: `tier("request",fixed(0.01))`, body: `{"id":"resp","object":"response","status":"failed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}],"usage":{"input_tokens":10,"output_tokens":5}}`, status: "partial", source: "upstream", cost: 20000},
		{name: "Responses incomplete fixed request refunds", protocol: "openai_responses", expression: `tier("request",fixed(0.01))`, body: `{"id":"resp","object":"response","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}],"usage":{"input_tokens":10,"output_tokens":5}}`, status: "partial", source: "upstream", cost: 20000},
		{name: "explicit free request", expression: `tier("request",fixed(0))`, body: `{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, status: "success", source: "upstream", cost: 20000},
		{name: "fixed successful request", expression: `tier("request",fixed(0.01))`, body: `{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, status: "success", source: "upstream", charge: 9000000, cost: 20000},
		{name: "fixed interrupted stream refunds", expression: `tier("request",fixed(0.01))`, stream: true, body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}]}\n\n", status: "partial", source: "estimated", cost: 11000},
		{name: "HTTP failure without usage refunds", upstreamStatus: 503, body: `{"error":{"type":"provider_error","message":"provider-secret https://secret.invalid request-body-secret"}}`, status: "failed", source: "none"},
		{name: "HTTP failure retains reported purchase usage", upstreamStatus: 503, body: `{"error":{"type":"provider_error","message":"provider-secret"},"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, status: "failed", source: "upstream", cost: 20000},
		{name: "HTTP failure retains partial reported purchase usage", upstreamStatus: 503, body: `{"error":{"type":"provider_error","message":"provider-secret"},"usage":{"prompt_tokens":10}}`, status: "failed", source: "estimated", cost: 10000},
		{name: "JSON error retains reported purchase usage", body: `{"error":{"type":"provider_error","message":"provider-secret"},"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, status: "failed", source: "upstream", cost: 20000},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				contentType := "application/json"
				if test.stream {
					contentType = "text/event-stream"
				}
				w.Header().Set("Content-Type", contentType)
				if test.upstreamStatus != 0 {
					w.WriteHeader(test.upstreamStatus)
				}
				_, _ = w.Write([]byte(test.body))
			})
			if test.protocol != "" {
				require.NoError(t, model.DB.Model(fixture.channel).Update("protocol", test.protocol).Error)
			}
			if test.expression != "" {
				price := &model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: test.expression, EffectiveAt: clock.Now().Unix()}
				require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
			}
			body := fmt.Sprintf(`{"model":"gateway-alias","stream":%t,"messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`, test.stream)
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", body)
			assert.Equal(t, 1, calls)
			if test.status == "failed" {
				assert.GreaterOrEqual(t, response.Code, 400, response.Body.String())
				assert.NotContains(t, response.Body.String(), "provider-secret")
			} else {
				assert.Equal(t, 200, response.Code, response.Body.String())
			}
			requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			request := requests[0]
			assert.Equal(t, test.status, request.Status)
			assert.Equal(t, test.charge, request.Receivable.Nanos)
			attempts, total, err := model.ListGatewayAttempts(t.Context(), request.ID, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.Equal(t, test.source, attempts[0].UsageSource)
			assert.Equal(t, test.cost, attempts[0].Cost.Nanos)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, int64(1000000000)-test.charge, wallet.Balance.Nanos)
			if test.name == "explicit free request" {
				entries, total, err := model.ListLedgerEntries(wallet.Id, 0, 10)
				require.NoError(t, err)
				require.EqualValues(t, 1, total)
				assert.NotZero(t, entries[0].Amount.Nanos)
			}
		})
	}
}

func TestTextGatewayAuthorizationCapabilityAndScopeFilters(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*testing.T, *textGatewayFixture)
		body   string
		status int
		code   string
	}{
		{name: "disabled Offering cannot fall through", change: func(t *testing.T, f *textGatewayFixture) {
			require.NoError(t, model.DB.Model(f.offering).Update("enabled", false).Error)
		}, status: 404, code: "model_not_found"},
		{name: "hidden Offering cannot fall through", change: func(t *testing.T, f *textGatewayFixture) {
			f.offering.Visibility = "groups"
			f.offering.Groups = []string{"vip"}
			require.NoError(t, model.SaveModelOffering(t.Context(), f.offering, "save"))
		}, status: 404, code: "model_not_found"},
		{name: "token model restriction", change: func(t *testing.T, f *textGatewayFixture) {
			require.NoError(t, model.DB.Model(f.token).Updates(map[string]any{"model_limits_enabled": true, "model_limits": "other-model"}).Error)
		}, status: 404, code: "model_not_found"},
		{name: "Provider exclusion", change: func(t *testing.T, f *textGatewayFixture) {
			f.offering.ChannelScope.ExcludeProviderIDs = []int{f.channel.ProviderID}
			require.NoError(t, model.SaveModelOffering(t.Context(), f.offering, "save"))
		}, status: 503, code: "channel_unavailable"},
		{name: "Channel exclusion wins over Provider include", change: func(t *testing.T, f *textGatewayFixture) {
			f.offering.ChannelScope.IncludeProviderIDs = []int{f.channel.ProviderID}
			f.offering.ChannelScope.ExcludeChannelIDs = []int{f.channel.ID}
			require.NoError(t, model.SaveModelOffering(t.Context(), f.offering, "save"))
		}, status: 503, code: "channel_unavailable"},
		{name: "deleted include stays restrictive", change: func(t *testing.T, f *textGatewayFixture) {
			require.NoError(t, model.DB.Exec(`UPDATE "model_offerings" SET "channel_scope" = '{"include_provider_ids":[999999],"exclude_provider_ids":[],"include_channel_ids":[],"exclude_channel_ids":[]}' WHERE "id" = ?`, f.offering.ID).Error)
		}, status: 503, code: "channel_unavailable"},
		{name: "narrowed streaming capability", change: func(t *testing.T, f *textGatewayFixture) {
			caps := &capability.Set{InputModalities: []capability.Modality{capability.Text}, OutputModalities: []capability.Modality{capability.Text}}
			require.NoError(t, model.DB.Model(f.channel).Update("capabilities", caps).Error)
		}, body: `{"model":"gateway-alias","stream":true,"messages":[{"role":"user","content":"Hello"}]}`, status: 400, code: "unsupported_parameter"},
		{name: "image capability", body: `{"model":"gateway-alias","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.invalid/secret.png"}}]}]}`, status: 400, code: "unsupported_parameter"},
		{name: "Provider disabled", change: func(t *testing.T, f *textGatewayFixture) {
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", f.channel.ProviderID).Update("status", common.ChannelStatusManuallyDisabled).Error)
		}, status: 503, code: "channel_unavailable"},
		{name: "Channel disabled", change: func(t *testing.T, f *textGatewayFixture) {
			require.NoError(t, model.DB.Model(f.channel).Update("status", model.ChannelDisabled).Error)
		}, status: 503, code: "channel_unavailable"},
		{name: "non-text protocol", change: func(t *testing.T, f *textGatewayFixture) {
			require.NoError(t, model.DB.Model(f.channel).Update("protocol", "openai_embeddings").Error)
		}, status: 503, code: "channel_unavailable"},
		{name: "disabled token", change: func(t *testing.T, f *textGatewayFixture) {
			require.NoError(t, model.DB.Model(f.token).Update("status", common.TokenStatusDisabled).Error)
		}, status: 401},
		{name: "exhausted token remains denied", change: func(t *testing.T, f *textGatewayFixture) {
			require.NoError(t, model.DB.Model(f.token).Update("status", common.TokenStatusExhausted).Error)
		}, status: 401},
		{name: "expired token", change: func(t *testing.T, f *textGatewayFixture) {
			require.NoError(t, model.DB.Model(f.token).Update("expired_time", int64(1)).Error)
		}, status: 401},
		{name: "unknown token", change: func(t *testing.T, f *textGatewayFixture) { f.token.Key = "invalidtoken" }, status: 401},
		{name: "IP restricted token", change: func(t *testing.T, f *textGatewayFixture) {
			require.NoError(t, model.DB.Model(f.token).Update("allow_ips", "203.0.113.0/24").Error)
		}, status: 403},
		{name: "disabled user", change: func(t *testing.T, f *textGatewayFixture) {
			require.NoError(t, model.DB.Model(f.user).Update("status", common.UserStatusDisabled).Error)
		}, status: 403},
		{name: "oversized choice count", body: `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"n":129}`, status: 400, code: "invalid_request"},
		{name: "oversized output limit", body: `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":18446744073709551615}`, status: 400, code: "invalid_request"},
		{name: "unsafe Channel override", change: func(t *testing.T, f *textGatewayFixture) {
			require.NoError(t, model.DB.Model(f.channel).Update("parameter_overrides", `{"n":129}`).Error)
		}, status: 400, code: "unsupported_parameter"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusInternalServerError) })
			if test.change != nil {
				test.change(t, &fixture)
			}
			body := test.body
			if body == "" {
				body = `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`
			}
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", body)
			assert.Equal(t, test.status, response.Code, response.Body.String())
			if test.code != "" {
				assert.Contains(t, response.Body.String(), `"code":"`+test.code+`"`)
			}
			assert.Zero(t, calls)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, int64(1_000_000_000), wallet.Balance.Nanos)
		})
	}
}

func TestTextGatewayRecordOwnershipAndPermissionScopes(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	})
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-offering","messages":[{"role":"user","content":"secret-request-body"}],"max_tokens":16}`)
	require.Equal(t, 200, response.Code, response.Body.String())
	id := response.Header().Get(common.RequestIdKey)
	other := &model.User{Username: "other-reader", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: "otherreader"}
	require.NoError(t, model.DB.Create(other).Error)
	otherPAT := createWalletTestAccessToken(t, other.Id, "log:read")
	self := adminSecurityRequest(fixture.engine, fixture.userPAT, http.MethodGet, "/api/requests/self?user_id="+fmt.Sprint(other.Id), "")
	require.Equal(t, 200, self.Code, self.Body.String())
	assert.Equal(t, "no-store", self.Header().Get("Cache-Control"))
	assert.NotContains(t, self.Body.String(), "sale_expression")
	assert.NotContains(t, self.Body.String(), "secret-request-body")
	var payload walletAPIResponse
	require.NoError(t, common.Unmarshal(self.Body.Bytes(), &payload))
	require.EqualValues(t, 1, payload.Data["total"])
	attempts := walletAPIRequest(t, fixture.engine, fixture.userPAT, http.MethodGet, "/api/requests/self/"+id+"/attempts", "")
	require.True(t, attempts.Success, attempts.Message)
	require.EqualValues(t, 1, attempts.Data["total"])
	attempt := attempts.Data["items"].([]any)[0].(map[string]any)
	for _, field := range []string{"provider_id", "channel_id", "model_id", "member_tier", "route_diagnostics", "cost", "converted_cost", "accounting_fx_rate", "purchase_expression", "purchase_price_version_id", "failover_decision", "failover_reason", "circuit_action"} {
		assert.NotContains(t, attempt, field)
	}
	cross := adminSecurityRequest(fixture.engine, otherPAT, http.MethodGet, "/api/requests/self/"+id+"/attempts", "")
	missing := adminSecurityRequest(fixture.engine, otherPAT, http.MethodGet, "/api/requests/self/missing/attempts", "")
	assert.Equal(t, 404, cross.Code)
	assert.Equal(t, missing.Code, cross.Code)
	assert.JSONEq(t, missing.Body.String(), cross.Body.String())
	assert.Equal(t, 403, adminSecurityRequest(fixture.engine, fixture.userPAT, http.MethodGet, "/api/admin/v1/requests", "").Code)
	readOnlyAdmin := createWalletTestAccessToken(t, fixture.sale.CreatedBy, "log:read")
	assert.Equal(t, 403, adminSecurityRequest(fixture.engine, readOnlyAdmin, http.MethodGet, "/api/admin/v1/requests", "").Code)
	admin := adminSecurityRequest(fixture.engine, fixture.adminPAT, http.MethodGet, "/api/admin/v1/requests/"+id+"/attempts", "")
	require.Equal(t, 200, admin.Code, admin.Body.String())
	assert.Contains(t, admin.Body.String(), "route_diagnostics")
	assert.NotContains(t, admin.Body.String(), "provider-secret")
	assert.NotContains(t, admin.Body.String(), "secret-request-body")
	assert.NotContains(t, admin.Body.String(), "http://127.0.0.1")
	for _, query := range []string{"page=0", "page_size=-1", "page_size=101", "model_offering_id=0", "started_after=-1", "status=settlement_pending"} {
		assert.Equal(t, 400, adminSecurityRequest(fixture.engine, fixture.adminPAT, http.MethodGet, "/api/admin/v1/requests?"+query, "").Code, query)
	}
}

func TestTextGatewayInsufficientReservationStopsBeforeProvider(t *testing.T) {
	calls := 0
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { calls++ })
	price := &model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: `tier("request",fixed(2))`, EffectiveAt: clock.Now().Unix()}
	require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-offering","messages":[{"role":"user","content":"Hello"}]}`)
	assert.Equal(t, http.StatusPaymentRequired, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "insufficient_balance")
	assert.Zero(t, calls)
	wallet, err := model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	assert.Equal(t, int64(1_000_000_000), wallet.Balance.Nanos)
	_, total, err := model.ListLedgerEntries(wallet.Id, 0, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
}

func TestTextGatewayUnknownOfferingRetainsLegacyTokenQuota(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unknown Offering must not use the new Channel") })
	body := `{"model":"unknown-legacy-model","messages":[{"role":"user","content":"Hello"}]}`
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", body)
	assert.Equal(t, 401, response.Code, response.Body.String())
	token, err := model.GetTokenById(fixture.token.Id)
	require.NoError(t, err)
	assert.Equal(t, common.TokenStatusExhausted, token.Status)
	require.NoError(t, model.DB.Model(fixture.token).Updates(map[string]any{"status": common.TokenStatusEnabled, "unlimited_quota": true}).Error)
	response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", body)
	assert.Equal(t, 503, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"code":"model_not_found"`)
	_, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
	require.NoError(t, err)
	assert.Zero(t, total)
}

func TestTextGatewayMissingUsageDoesNotChargeOutputLimitAsPrompt(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat-test","object":"chat.completion","model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}]}`))
	})
	ids := make([]string, 0, 2)
	debits := make([]int64, 0, 2)
	for _, limit := range []int{32, 1024} {
		before, err := model.GetUserWallet(fixture.user.Id)
		require.NoError(t, err)
		body := fmt.Sprintf(`{"model":"gateway-offering","messages":[{"role":"user","content":"Hello"}],"max_tokens":%d}`, limit)
		response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", body)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		ids = append(ids, response.Header().Get(common.RequestIdKey))
		after, err := model.GetUserWallet(fixture.user.Id)
		require.NoError(t, err)
		debits = append(debits, before.Balance.Nanos-after.Balance.Nanos)
	}
	requests := walletAPIRequest(t, fixture.engine, fixture.adminPAT, http.MethodGet, "/api/admin/v1/requests?page=1&page_size=10", "")
	require.True(t, requests.Success, requests.Message)
	require.EqualValues(t, 2, requests.Data["total"])
	records := make(map[string]map[string]any)
	for _, item := range requests.Data["items"].([]any) {
		record := item.(map[string]any)
		records[record["id"].(string)] = record
	}
	first, second := records[ids[0]], records[ids[1]]
	require.NotNil(t, first)
	require.NotNil(t, second)
	firstUsage, secondUsage := first["usage"].(map[string]any), second["usage"].(map[string]any)
	t.Logf("identical content at output limits 32/1024: prompt=%v/%v completion=%v/%v debits(nanos)=%v", firstUsage["prompt_tokens"], secondUsage["prompt_tokens"], firstUsage["completion_tokens"], secondUsage["completion_tokens"], debits)
	assert.Equal(t, "estimated", first["usage_source"])
	assert.Equal(t, firstUsage["completion_tokens"], secondUsage["completion_tokens"])
	assert.Equal(t, firstUsage["prompt_tokens"], secondUsage["prompt_tokens"], "an output limit cannot change the input token estimate for identical content")
	assert.Equal(t, first["receivable"], second["receivable"], "identical actual content and fixed prices must have the same receivable")
	assert.Equal(t, debits[0], debits[1], "the user's balance must not pay for an unused output budget")
}

func TestTextGatewayToolStreamWithoutUsageStillDeliversToolCall(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"tool-final\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\ndata: [DONE]\n\n"))
	})
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-offering","stream":true,"stream_options":{"include_usage":false},"messages":[{"role":"user","content":"Hello"}],"max_tokens":32}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "lookup")
	assert.NotContains(t, response.Body.String(), `"usage"`)
	record, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
	require.NoError(t, err)
	assert.Equal(t, "success", record.Status)
	assert.Equal(t, int64(36000), record.Receivable.Nanos)
	wallet, err := model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	assert.Equal(t, int64(999964000), wallet.Balance.Nanos)
}

func TestTextGatewayToolOnlyUsageCountsDeliveredArguments(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat-tool","object":"chat.completion","model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_test","type":"function","function":{"name":"lookup_weather","arguments":"{\"city\":\"Paris\"}"}}]},"finish_reason":"tool_calls"}]}`))
	})
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-offering","messages":[{"role":"user","content":"Hello"}],"max_tokens":32}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `lookup_weather`)
	requests := walletAPIRequest(t, fixture.engine, fixture.adminPAT, http.MethodGet, "/api/admin/v1/requests?page=1&page_size=10", "")
	require.True(t, requests.Success, requests.Message)
	require.EqualValues(t, 1, requests.Data["total"])
	record := requests.Data["items"].([]any)[0].(map[string]any)
	usage := record["usage"].(map[string]any)
	t.Logf("delivered tool call: status=%v usage_source=%v completion=%v receivable=%v", record["status"], record["usage_source"], usage["completion_tokens"], record["receivable"])
	assert.Equal(t, "success", record["status"])
	assert.Equal(t, "estimated", record["usage_source"])
	assert.Greater(t, usage["completion_tokens"].(float64), float64(0), "delivered tool name and arguments contribute real output tokens")
}

type gatewayPrefixOnlyWriter struct {
	*httptest.ResponseRecorder
}

func (w gatewayPrefixOnlyWriter) Write(body []byte) (int, error) {
	n, _ := w.ResponseRecorder.Write(body[:min(8, len(body))])
	return n, io.ErrClosedPipe
}

func TestTextGatewayMetadataPrefixIsNotDeliveredBusinessOutput(t *testing.T) {
	for _, test := range []struct {
		name     string
		provider int
		body     string
		stream   bool
	}{
		{name: "OpenAI stream", provider: constant.ChannelTypeOpenAI, stream: true, body: "data: {\"id\":\"chat-test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\ndata: [DONE]\n\n"},
		{name: "OpenAI", provider: constant.ChannelTypeOpenAI, body: `{"id":"chat-test","object":"chat.completion","model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`},
		{name: "Ollama", provider: constant.ChannelTypeOllama, body: `{"model":"upstream-model","message":{"role":"assistant","content":"Hello"},"done":true,"prompt_eval_count":10,"eval_count":5}`},
		{name: "Dify", provider: constant.ChannelTypeDify, body: `{"answer":"Hello","conversation_id":"chat-test","metadata":{"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if test.stream {
					w.Header().Set("Content-Type", "text/event-stream")
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				_, _ = w.Write([]byte(test.body))
			})
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", test.provider).Error)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"gateway-offering","stream":%t,"messages":[{"role":"user","content":"Hello"}],"max_tokens":32}`, test.stream)))
			request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
			request.Header.Set("Content-Type", "application/json")
			fixture.engine.ServeHTTP(gatewayPrefixOnlyWriter{recorder}, request)
			if test.stream {
				require.NotEmpty(t, recorder.Body.Bytes())
			} else {
				require.Len(t, recorder.Body.Bytes(), 8, "the downstream accepted only a short metadata prefix")
			}
			require.NotContains(t, recorder.Body.String(), "Hello", "the write failed before the first business content")
			requests := walletAPIRequest(t, fixture.engine, fixture.adminPAT, http.MethodGet, "/api/admin/v1/requests?page=1&page_size=10", "")
			require.True(t, requests.Success, requests.Message)
			require.EqualValues(t, 1, requests.Data["total"])
			record := requests.Data["items"].([]any)[0].(map[string]any)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			t.Logf("downstream bytes=%q status=%v receivable=%v wallet_nanos=%d", recorder.Body.String(), record["status"], record["receivable"], wallet.Balance.Nanos)
			assert.NotEqual(t, "success", record["status"], "a failed response write cannot be recorded as a successful delivery")
			assert.Equal(t, map[string]any{"nanos": "0", "currency": "USD"}, record["receivable"], "metadata alone is not billable business output")
			assert.Equal(t, int64(1_000_000_000), wallet.Balance.Nanos, "no business content was delivered, so release the full reservation")
			attempts := walletAPIRequest(t, fixture.engine, fixture.adminPAT, http.MethodGet, "/api/admin/v1/requests/"+record["id"].(string)+"/attempts", "")
			require.True(t, attempts.Success, attempts.Message)
			require.EqualValues(t, 1, attempts.Data["total"])
			attempt := attempts.Data["items"].([]any)[0].(map[string]any)
			assert.Equal(t, "downstream_write_failed", attempt["error_class"])
			assert.Equal(t, map[string]any{"nanos": "20000", "currency": "USD"}, attempt["cost"], "the platform still records the Provider's reported cost")
		})
	}
}

func TestTextGatewayShortfallPersistsAndTopUpCannotBypassDebt(t *testing.T) {
	calls := 0
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		completion := 1
		if calls == 2 {
			completion = 16
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"chat-test","object":"chat.completion","model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":%d,"total_tokens":%d}}`, completion, 10+completion)
	})
	body := `{"model":"gateway-offering","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`
	warmup := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", body)
	require.Equal(t, http.StatusOK, warmup.Code, warmup.Body.String())
	// Keep a second real reservation in flight while the request settles short.
	pending := model.GatewayRequest{ID: "debt-pending-release", UserID: fixture.user.Id, SaleFXRate: "1", SaleCurrency: "USD", Receivable: money.Amount{Nanos: 2_000, Currency: "USD"}, Status: "success"}
	reserve := money.Amount{Nanos: 1_000, Currency: "USD"}
	require.NoError(t, model.ReserveGateway(&pending, reserve, reserve))
	concurrent := model.GatewayRequest{ID: "debt-concurrent-release", UserID: fixture.user.Id, SaleFXRate: "1", SaleCurrency: "USD", Receivable: money.Amount{Currency: "USD"}, Status: "failed"}
	require.NoError(t, model.ReserveGateway(&concurrent, reserve, reserve))
	negative, err := model.PostLedgerEntries(model.LedgerPosting{Account: model.UserWalletAccount(fixture.user.Id), Type: model.LedgerEntryAdjustment, Amount: money.Amount{Nanos: -1_000, Currency: "USD"}, Remark: "reversible debit"})
	require.NoError(t, err)
	wallet, err := model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	_, err = model.PostLedgerEntries(model.LedgerPosting{Account: model.UserWalletAccount(fixture.user.Id), Type: model.LedgerEntryAdjustment, Amount: money.Amount{Nanos: 50_000 - wallet.Balance.Nanos, Currency: "USD"}, Remark: "controlled test starting balance"})
	require.NoError(t, err)
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", body)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	requests := walletAPIRequest(t, fixture.engine, fixture.adminPAT, http.MethodGet, "/api/admin/v1/requests?page=1&page_size=10", "")
	require.True(t, requests.Success, requests.Message)
	var record map[string]any
	for _, item := range requests.Data["items"].([]any) {
		candidate := item.(map[string]any)
		if candidate["id"] == response.Header().Get(common.RequestIdKey) {
			record = candidate
		}
	}
	require.NotNil(t, record)
	assert.Equal(t, map[string]any{"nanos": "75600", "currency": "USD"}, record["receivable"], "the full receivable must survive a 25600-nano shortfall")
	wallet, err = model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	require.Zero(t, wallet.Balance.Nanos)
	require.True(t, wallet.RequestsPaused)
	_, err = model.TopUpWallet(model.WalletTopUp{UserId: fixture.user.Id, Source: money.Amount{Nanos: 20_000, Currency: "USD"}, Reference: "partial-debt-payment"})
	require.NoError(t, err)
	wallet, err = model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	t.Logf("full receivable=75600 observed=%v; after 20000 topup for 25600 debt: balance=%d paused=%v", record["receivable"], wallet.Balance.Nanos, wallet.RequestsPaused)
	assert.Zero(t, wallet.Balance.Nanos, "the partial top-up must pay existing debt before becoming spendable")
	assert.True(t, wallet.RequestsPaused, "5600 nanos of debt remain after partial repayment")
	balanceView := walletAPIRequest(t, fixture.engine, fixture.adminPAT, http.MethodGet, fmt.Sprintf("/api/admin/v1/user-wallets/%d", fixture.user.Id), "")
	require.True(t, balanceView.Success, balanceView.Message)
	assert.Equal(t, "0", balanceView.Data["balance"])
	assert.Equal(t, "0.0000056", balanceView.Data["outstanding"])
	assert.Equal(t, true, balanceView.Data["requests_paused"])
	third := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-offering","messages":[{"role":"user","content":"Hello"}],"max_tokens":1}`)
	t.Logf("request with unpaid debt: status=%d upstream_calls=%d", third.Code, calls)
	assert.NotEqual(t, http.StatusOK, third.Code, "a partial top-up cannot authorize another billable request")
	assert.Equal(t, 2, calls, "existing debt must stop dispatch before another upstream call")

	_, err = model.TopUpWallet(model.WalletTopUp{UserId: fixture.user.Id, Source: money.Amount{Nanos: 20_000, Currency: "USD"}, Reference: "partial-debt-payment"})
	require.ErrorIs(t, err, model.ErrDuplicateLedgerEntry)
	for i, kind := range []model.LedgerEntryType{model.LedgerEntryAdjustment, model.LedgerEntryRefund, model.LedgerEntryOpeningBalance} {
		_, err = model.PostLedgerEntries(model.LedgerPosting{Account: model.UserWalletAccount(fixture.user.Id), Type: kind, Amount: reserve, Remark: "credit pays existing debt"})
		require.NoError(t, err)
		outstanding, err := model.GetUserOutstanding(fixture.user.Id)
		require.NoError(t, err)
		assert.Equal(t, int64(4_600-i*1_000), outstanding.Nanos)
		self := walletAPIRequest(t, fixture.engine, fixture.userPAT, http.MethodGet, "/api/requests/self", "")
		require.True(t, self.Success, self.Message)
		assert.Equal(t, map[string]any{"nanos": fmt.Sprint(4_600 - i*1_000), "currency": "USD"}, self.Data["outstanding"])
	}
	_, err = model.ReverseLedgerEntry(negative[0].Id, 0, "return the earlier debit")
	require.NoError(t, err)
	require.NoError(t, model.PrepareGatewaySettlement(model.GatewaySettlement{Request: pending, SaleSource: pending.Receivable}))
	changed := pending
	changed.Receivable = money.Amount{Nanos: 99_999, Currency: "USD"}
	require.NoError(t, model.PrepareGatewaySettlement(model.GatewaySettlement{Request: changed, SaleSource: changed.Receivable}))
	logDB := model.LOG_DB
	model.LOG_DB = unavailableGatewayDatabase(t)
	_, recoveryErr := model.RecoverGatewaySettlements(t.Context(), 100)
	model.LOG_DB = logDB
	require.Error(t, recoveryErr, "primary settlement succeeds even when log publication fails")
	outstanding, err := model.GetUserOutstanding(fixture.user.Id)
	require.NoError(t, err)
	assert.Equal(t, int64(2_600), outstanding.Nanos)
	// The older request retains a 600-nano claim; the concurrent settlement
	// records its complete new 2000-nano debt while its release pays old debt.
	beforeFinal, _, err := model.ListLedgerEntries(wallet.Id, 0, 100)
	require.NoError(t, err)
	for _, entry := range beforeFinal {
		if entry.Type == model.LedgerEntryRepayment {
			assert.NotEqual(t, pending.ID, entry.RequestId)
		}
	}
	// A batch cannot spend a new credit before satisfying older receivables.
	_, err = model.PostLedgerEntries(
		model.LedgerPosting{Account: model.UserWalletAccount(fixture.user.Id), Type: model.LedgerEntryAdjustment, Amount: money.Amount{Nanos: 1_000, Currency: "USD"}},
		model.LedgerPosting{Account: model.UserWalletAccount(fixture.user.Id), Type: model.LedgerEntryCharge, Amount: money.Amount{Nanos: -1_000, Currency: "USD"}},
	)
	assert.ErrorIs(t, err, model.ErrInsufficientWalletBalance)
	batch, err := model.PostLedgerEntries(
		model.LedgerPosting{Account: model.UserWalletAccount(fixture.user.Id), Type: model.LedgerEntryAdjustment, Amount: money.Amount{Nanos: 40, Currency: "USD"}},
		model.LedgerPosting{Account: model.UserWalletAccount(fixture.user.Id), Type: model.LedgerEntryRefund, Amount: money.Amount{Nanos: 60, Currency: "USD"}},
	)
	require.NoError(t, err)
	funding := make(map[string]int64)
	var allocations []int64
	for _, entry := range batch {
		if entry.AccountType == model.LedgerAccountUserWallet && entry.Amount.Nanos > 0 {
			funding[fmt.Sprintf("Receivable repayment funded by ledger entry %d", entry.Id)] = entry.Amount.Nanos
		}
		if entry.AccountType == model.LedgerAccountUserWallet && entry.Type == model.LedgerEntryRepayment {
			allocations = append(allocations, -entry.Amount.Nanos)
			assert.GreaterOrEqual(t, funding[entry.Remark], -entry.Amount.Nanos, "a repayment cannot exceed its funding credit")
		}
	}
	assert.Equal(t, []int64{40, 60}, allocations)
	require.NoError(t, model.PrepareGatewaySettlement(model.GatewaySettlement{Request: concurrent, SaleSource: concurrent.Receivable}))

	// Race a duplicate payment against a prepared settlement and recovery.
	rate, err := money.NewRate("EUR", "USD", decimal.NewFromInt(2))
	require.NoError(t, err)
	payment := model.WalletTopUp{UserId: fixture.user.Id, Source: money.Amount{Nanos: 20_000, Currency: "EUR"}, Rate: &rate, Reference: "final-debt-payment"}
	results := make(chan error, 3)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			_, err := model.TopUpWallet(payment)
			results <- err
		})
	}
	workers.Go(func() {
		_, err := model.RecoverGatewaySettlements(t.Context(), 100)
		results <- err
	})
	workers.Wait()
	close(results)
	duplicates := 0
	for err := range results {
		if err == model.ErrDuplicateLedgerEntry {
			duplicates++
		} else {
			require.NoError(t, err)
		}
	}
	assert.Equal(t, 1, duplicates)
	for _, id := range []string{response.Header().Get(common.RequestIdKey), pending.ID, concurrent.ID} {
		_, err = model.SettleGateway(id)
		require.NoError(t, err)
		require.NoError(t, model.PublishGatewayRecords(id))
	}
	wallet, err = model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	assert.Equal(t, int64(38_500), wallet.Balance.Nanos)
	assert.False(t, wallet.RequestsPaused)
	outstanding, err = model.GetUserOutstanding(fixture.user.Id)
	require.NoError(t, err)
	assert.Zero(t, outstanding.Nanos)
	entries, _, err := model.ListLedgerEntries(wallet.Id, 0, 100)
	require.NoError(t, err)
	var charged, repaid int64
	for _, entry := range entries {
		if entry.Type == model.LedgerEntryCharge && entry.RequestId == response.Header().Get(common.RequestIdKey) {
			charged -= entry.Amount.Nanos
		}
		if entry.Type == model.LedgerEntryRepayment {
			repaid -= entry.Amount.Nanos
		}
		if entry.Type == model.LedgerEntryTopUp && entry.SourceAmount.Currency == "EUR" {
			assert.Equal(t, int64(20_000), entry.SourceAmount.Nanos)
			assert.Equal(t, int64(40_000), entry.Amount.Nanos)
			assert.Equal(t, "2", entry.FxRate.Decimal.String())
		}
	}
	assert.Equal(t, int64(50_000), charged)
	assert.Equal(t, int64(27_600), repaid)
	var revenue model.Wallet
	require.NoError(t, model.DB.Where("account_type = ?", model.LedgerAccountPlatformRevenue).Take(&revenue).Error)
	assert.Equal(t, int64(99_200), revenue.Balance.Nanos, "repayment and recovery do not recognize revenue again")
	reconciliation, err := model.ReconcileWallets(100)
	require.NoError(t, err)
	assert.Empty(t, reconciliation.Mismatches)
	resumed := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-offering","messages":[{"role":"user","content":"Hello"}],"max_tokens":1}`)
	require.Equal(t, http.StatusOK, resumed.Code, resumed.Body.String())
	assert.Equal(t, 3, calls)
}

func setupRelayRouterTestDB(t *testing.T) {
	t.Helper()

	gin.SetMode(gin.TestMode)
	originalIsMasterNode := common.IsMasterNode
	originalRedisEnabled := common.RedisEnabled
	common.IsMasterNode = false
	common.RedisEnabled = false
	t.Cleanup(func() {
		common.IsMasterNode = originalIsMasterNode
		common.RedisEnabled = originalRedisEnabled
	})
	db := model.UseTestDatabases(t)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Ability{}, &model.ModelOffering{}, &model.ModelOfferingName{}, &model.ModelOfferingMember{}))
}
