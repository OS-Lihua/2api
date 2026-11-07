package router

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/pkg/capability"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/plugins"
	"github.com/gin-gonic/gin"
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
	return textGatewayFixture{engine: engine, user: user, token: token, adminPAT: createWalletTestAccessToken(t, actor.Id, "request:read"), userPAT: createWalletTestAccessToken(t, user.Id, "log:read"), offering: offering, channel: channel, sale: sale, purchase: purchase, fake: fake}
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
	assert.Equal(t, int64(1000000000), requests[0].Receivable.Nanos)
	assert.Equal(t, "wallet_shortfall", requests[0].ErrorCode)
	blocked := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", body)
	assert.Equal(t, 402, blocked.Code, blocked.Body.String())
	assert.Equal(t, 1, calls)
	_, err = model.TopUpWallet(model.WalletTopUp{UserId: fixture.user.Id, Source: money.Amount{Nanos: 100000000, Currency: "USD"}, Reference: "shortfall-recharge"})
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

func TestTextGatewayFreeFixedPricesAndFailureCosts(t *testing.T) {
	for _, test := range []struct {
		name, expression, body, status, source string
		upstreamStatus                         int
		stream                                 bool
		charge, cost                           int64
	}{
		{name: "explicit free request", expression: `tier("request",fixed(0))`, body: `{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, status: "success", source: "upstream", cost: 20000},
		{name: "fixed successful request", expression: `tier("request",fixed(0.01))`, body: `{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, status: "success", source: "upstream", charge: 9000000, cost: 20000},
		{name: "fixed interrupted stream refunds", expression: `tier("request",fixed(0.01))`, stream: true, body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}]}\n\n", status: "partial", source: "estimated", cost: 11000},
		{name: "HTTP failure without usage refunds", upstreamStatus: 503, body: `{"error":{"type":"provider_error","message":"provider-secret https://secret.invalid request-body-secret"}}`, status: "failed", source: "none"},
		{name: "HTTP failure retains reported purchase usage", upstreamStatus: 503, body: `{"error":{"type":"provider_error","message":"provider-secret"},"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, status: "failed", source: "upstream", cost: 20000},
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
		{name: "different protocol", change: func(t *testing.T, f *textGatewayFixture) {
			require.NoError(t, model.DB.Model(f.channel).Update("protocol", "openai_responses").Error)
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
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat-test","object":"chat.completion","model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gateway-offering","messages":[{"role":"user","content":"Hello"}],"max_tokens":32}`))
	request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
	request.Header.Set("Content-Type", "application/json")
	fixture.engine.ServeHTTP(gatewayPrefixOnlyWriter{recorder}, request)
	require.Len(t, recorder.Body.Bytes(), 8, "the downstream accepted only a short metadata prefix")
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
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Ability{}))
}
