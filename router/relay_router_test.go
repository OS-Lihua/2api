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
	"github.com/QuantumNous/new-api/service"
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
	t.Setenv("GATEWAY_MAX_ATTEMPTS", "3")
	t.Setenv("GATEWAY_TOTAL_FIRST_BYTE_SECONDS", "90")
	t.Setenv("GATEWAY_ATTEMPT_FIRST_BYTE_SECONDS", "30")
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
	return textGatewayFixture{engine: engine, user: user, token: token, adminPAT: createWalletTestAccessToken(t, actor.Id, "request:read", "ledger:read"), userPAT: createWalletTestAccessToken(t, user.Id, "log:read"), offering: offering, channel: channel, sale: sale, purchase: purchase, fake: fake}
}

func TestGatewayBudgetManagementOptions(t *testing.T) {
	engine, admin, _ := newAdminTestServer(t)
	t.Setenv("GATEWAY_MAX_ATTEMPTS", "3")
	t.Setenv("GATEWAY_TOTAL_FIRST_BYTE_SECONDS", "90")
	t.Setenv("GATEWAY_ATTEMPT_FIRST_BYTE_SECONDS", "30")
	common.OptionMapRWMutex.Lock()
	previous := common.OptionMap
	common.OptionMap = map[string]string{}
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previous
		common.OptionMapRWMutex.Unlock()
	})
	model.InitOptionMap()
	require.NoError(t, model.DB.Model(admin).Update("role", common.RoleRootUser).Error)
	root := createWalletTestAccessToken(t, admin.Id, "option:read", "option:write")
	readOnly := createWalletTestAccessToken(t, admin.Id, "option:read")
	for _, key := range []string{"GatewayMaxAttempts", "GatewayTotalFirstByteSeconds", "GatewayAttemptFirstByteSeconds"} {
		response := adminSecurityRequest(engine, root, http.MethodGet, "/api/option/", "")
		require.Equal(t, 200, response.Code)
		assert.Contains(t, response.Body.String(), `"key":"`+key+`"`)
	}
	for _, tc := range []struct{ key, value string }{
		{"GatewayMaxAttempts", "0"}, {"GatewayMaxAttempts", "6"}, {"GatewayMaxAttempts", "1.5"},
		{"GatewayTotalFirstByteSeconds", "4"}, {"GatewayTotalFirstByteSeconds", "601"},
		{"GatewayAttemptFirstByteSeconds", "0"}, {"GatewayAttemptFirstByteSeconds", "601"},
		{"GatewayAttemptFirstByteSeconds", "30s"}, {"GatewayMaxAttempts", "true"},
	} {
		response := adminSecurityRequest(engine, root, http.MethodPut, "/api/option/", fmt.Sprintf(`{"key":%q,"value":%q}`, tc.key, tc.value))
		assert.Contains(t, response.Body.String(), `"success":false`, "%s=%s", tc.key, tc.value)
	}
	assert.Equal(t, service.GatewayAttemptPolicy{MaxAttempts: 3, TotalFirstByte: 90 * time.Second, AttemptFirstByte: 30 * time.Second}, service.GatewayPolicy())
	body := `{"key":"GatewayMaxAttempts","value":1}`
	assert.Equal(t, 401, adminSecurityRequest(engine, "", http.MethodPut, "/api/option/", body).Code)
	assert.Equal(t, 403, adminSecurityRequest(engine, readOnly, http.MethodPut, "/api/option/", body).Code)
	for _, tc := range []struct {
		key   string
		value int
	}{{"GatewayMaxAttempts", 5}, {"GatewayTotalFirstByteSeconds", 61}, {"GatewayAttemptFirstByteSeconds", 12}} {
		response := adminSecurityRequest(engine, root, http.MethodPut, "/api/option/", fmt.Sprintf(`{"key":%q,"value":%d}`, tc.key, tc.value))
		require.Contains(t, response.Body.String(), `"success":true`, response.Body.String())
	}
	// Reload from PostgreSQL, with conflicting deployment defaults.
	t.Setenv("GATEWAY_MAX_ATTEMPTS", "2")
	model.InitOptionMap()
	assert.Equal(t, service.GatewayAttemptPolicy{MaxAttempts: 5, TotalFirstByte: 61 * time.Second, AttemptFirstByte: 12 * time.Second}, service.GatewayPolicy())
	response := adminSecurityRequest(engine, root, http.MethodGet, "/api/option/", "")
	assert.Contains(t, response.Body.String(), `"key":"GatewayMaxAttempts","value":"5"`)
	logs, _, err := model.GetAuditLogs(model.AuditLogFilter{Category: model.AuditCategoryOperation}, 0, 100, common.RoleRootUser)
	require.NoError(t, err)
	updates := 0
	for _, entry := range logs {
		if entry.Action == "option.update" && entry.Success {
			updates++
		}
	}
	assert.Equal(t, 3, updates)
	encoded, err := common.Marshal(logs)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), root)
	assert.NotContains(t, string(encoded), readOnly)
	// Role changes, expiry and revocation are checked at the real management boundary.
	require.NoError(t, model.DB.Model(admin).Update("role", common.RoleAdminUser).Error)
	assert.Equal(t, 403, adminSecurityRequest(engine, root, http.MethodPut, "/api/option/", body).Code)
	require.NoError(t, model.DB.Model(admin).Update("role", common.RoleRootUser).Error)
	require.NoError(t, model.DB.Model(&model.UserAccessToken{}).Where("user_id = ?", admin.Id).Update("expires_at", time.Now().Unix()-1).Error)
	assert.Equal(t, 401, adminSecurityRequest(engine, root, http.MethodPut, "/api/option/", body).Code)
	require.NoError(t, model.DB.Where("user_id = ?", admin.Id).Delete(&model.UserAccessToken{}).Error)
	assert.Equal(t, 401, adminSecurityRequest(engine, root, http.MethodGet, "/api/option/", "").Code)
	assert.Equal(t, service.GatewayAttemptPolicy{MaxAttempts: 5, TotalFirstByte: 61 * time.Second, AttemptFirstByte: 12 * time.Second}, service.GatewayPolicy())
}

func TestTextGatewayFailoverChargesOnlyDeliveredAttempt(t *testing.T) {
	var unavailable *gorm.DB
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"overloaded","message":"private provider-secret"},"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		model.LOG_DB = unavailable
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Recovered"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	t.Cleanup(server.Close)
	provider := &model.Provider{Name: "Recovery Provider", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Key: "recovery-secret", BaseURL: &server.URL}
	require.NoError(t, model.DB.Create(provider).Error)
	channel := &model.Channel{ProviderID: provider.Id, ModelID: fixture.channel.ModelID, Protocol: "openai_chat", UpstreamModelName: "recovery-model", ParameterOverrides: `{}`, MappingConfirmed: true, Status: model.ChannelEnabled, Weight: 1}
	require.NoError(t, model.DB.Create(channel).Error)
	purchase := &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: channel.ID, Currency: "USD", Expression: `tier("base", p + c * 2)`, EffectiveAt: clock.Now().Unix() - 1}
	require.NoError(t, model.CreatePriceVersion(t.Context(), purchase, fixture.sale.CreatedBy))
	logs := model.LOG_DB
	unavailable = unavailableGatewayDatabase(t)
	t.Cleanup(func() { model.LOG_DB = logs })
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
	model.LOG_DB = logs
	completed, err := model.RecoverGatewaySettlements(t.Context(), 100)
	require.NoError(t, err)
	assert.Equal(t, 1, completed)
	completed, err = model.RecoverGatewaySettlements(t.Context(), 100)
	require.NoError(t, err)
	assert.Zero(t, completed)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "Recovered")
	assert.NotContains(t, response.Body.String(), "secret")
	record, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
	require.NoError(t, err)
	assert.Equal(t, int64(36000), record.Receivable.Nanos)
	attempts, total, err := model.ListGatewayAttempts(t.Context(), record.ID, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 2, total)
	costs := map[int]int64{}
	for _, attempt := range attempts {
		costs[attempt.ChannelID] = attempt.Cost.Nanos
		if attempt.ChannelID == fixture.channel.ID {
			assert.Equal(t, "upstream_unavailable", attempt.ErrorClass)
			assert.Equal(t, "switch", attempt.FailoverDecision)
		}
	}
	assert.Equal(t, map[int]int64{fixture.channel.ID: 7000, channel.ID: 20000}, costs)
	wallet, err := model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	assert.Equal(t, int64(999964000), wallet.Balance.Nanos)
	entries, _, err := model.ListLedgerEntries(wallet.Id, 0, 20)
	require.NoError(t, err)
	assert.Len(t, entries, 4)
	var payables model.Wallet
	require.NoError(t, model.DB.Where("account_type = ? AND owner_id = ?", model.ProviderPayableAccount(provider.Id, "USD").Type, provider.Id).First(&payables).Error)
	assert.Equal(t, int64(20000), payables.Balance.Nanos)
	reconciliation, err := model.ReconcileWallets(100)
	require.NoError(t, err)
	assert.Empty(t, reconciliation.Mismatches)
}

func TestTextGatewayFailureClassificationAndProviderScope(t *testing.T) {
	for _, test := range []struct {
		name        string
		status      int
		code, class string
		sends       int
	}{
		{"connection", 0, "", "connection_failed", 3},
		{"401", 401, "", "credentials", 2}, {"403", 403, "", "credentials", 2},
		{"quota", 429, "insufficient_quota", "account_quota", 2}, {"balance", 402, "insufficient_balance", "account_quota", 2},
		{"429", 429, "", "rate_limited", 3}, {"500", 500, "", "upstream_unavailable", 3},
		{"502", 502, "", "upstream_unavailable", 3}, {"503", 503, "", "upstream_unavailable", 3},
		{"504", 504, "", "upstream_unavailable", 3}, {"529", 529, "", "upstream_unavailable", 3},
		{"model 404", 404, "model_not_found", "model_not_found", 3}, {"other 404", 404, "unknown_path", "upstream_error", 1},
		{"400", 400, "", "invalid_request", 1}, {"413", 413, "", "invalid_request", 1}, {"422", 422, "", "invalid_request", 1},
		{"context", 400, "context_length_exceeded", "context_length", 1}, {"safety", 403, "content_filter", "content_safety", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if test.status == 0 {
					connection, _, err := w.(http.Hijacker).Hijack()
					require.NoError(t, err)
					_ = connection.Close()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"message":"Bearer provider-secret https://private.invalid/?key=private"}}`, test.code)
			})
			// A second model path on the same account must be skipped only for
			// credentials/account quota. The final account always succeeds.
			for i := range 2 {
				providerID := fixture.channel.ProviderID
				if i == 1 {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls++
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Recovered"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
					}))
					t.Cleanup(server.Close)
					provider := &model.Provider{Name: "Healthy account", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Key: "other-secret", BaseURL: &server.URL}
					require.NoError(t, model.DB.Create(provider).Error)
					providerID = provider.Id
				}
				channel := &model.Channel{ProviderID: providerID, ModelID: fixture.channel.ModelID, Protocol: "openai_chat", UpstreamModelName: fmt.Sprintf("model-%d", i), ParameterOverrides: `{}`, MappingConfirmed: true, Status: model.ChannelEnabled, Weight: 1}
				require.NoError(t, model.DB.Create(channel).Error)
				price := &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: channel.ID, Currency: "USD", Expression: fixture.purchase.Expression, EffectiveAt: clock.Now().Unix() - 1}
				require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
			}
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
			assert.Equal(t, test.sends, calls)
			if test.sends > 1 {
				assert.Equal(t, 200, response.Code, response.Body.String())
			} else {
				assert.Equal(t, test.status, response.Code)
			}
			assert.NotContains(t, response.Body.String(), "secret")
			assert.NotContains(t, response.Body.String(), "private")
			record, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
			require.NoError(t, err)
			attempts, total, err := model.ListGatewayAttempts(t.Context(), record.ID, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, test.sends, total)
			for _, attempt := range attempts {
				if attempt.ChannelID == fixture.channel.ID {
					assert.Equal(t, test.class, attempt.ErrorClass)
				}
			}
			if test.sends == 1 {
				assert.Zero(t, record.Receivable.Nanos)
				wallet, err := model.GetUserWallet(fixture.user.Id)
				require.NoError(t, err)
				assert.Equal(t, int64(1_000_000_000), wallet.Balance.Nanos)
			}
		})
	}
}

func addTextGatewayChannel(t *testing.T, fixture textGatewayFixture, upstream http.HandlerFunc) *model.Channel {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	provider := &model.Provider{Name: "Additional Provider", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Key: "additional-secret", BaseURL: &server.URL}
	require.NoError(t, model.DB.Create(provider).Error)
	channel := &model.Channel{ProviderID: provider.Id, ModelID: fixture.channel.ModelID, Protocol: "openai_chat", UpstreamModelName: "additional-model", ParameterOverrides: `{}`, MappingConfirmed: true, Status: model.ChannelEnabled, Weight: 1}
	require.NoError(t, model.DB.Create(channel).Error)
	purchase := &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: channel.ID, Currency: "USD", Expression: fixture.purchase.Expression, EffectiveAt: clock.Now().Unix() - 1}
	require.NoError(t, model.CreatePriceVersion(t.Context(), purchase, fixture.sale.CreatedBy))
	return channel
}

type gatewayObservedStreamBody struct {
	io.ReadCloser
	reads        int
	readingAgain chan struct{}
	closed       chan struct{}
	closeOnce    sync.Once
}

func (body *gatewayObservedStreamBody) Read(p []byte) (int, error) {
	body.reads++
	if body.reads == 2 {
		close(body.readingAgain)
	}
	return body.ReadCloser.Read(p)
}

func (body *gatewayObservedStreamBody) Close() error {
	body.closeOnce.Do(func() { close(body.closed) })
	return body.ReadCloser.Close()
}

func TestTextGatewayOllamaIdleTimeoutAfterBusinessOutput(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte("{\"model\":\"upstream-model\",\"message\":{\"role\":\"assistant\",\"content\":\"Hello\"},\"done\":false}\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", constant.ChannelTypeOllama).Error)
	calls := 0
	addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(503) })
	reading, closed := make(chan struct{}), make(chan struct{})
	client := service.GetHttpClient()
	original := client.Transport
	t.Cleanup(func() { client.Transport = original })
	client.Transport = channelProbeRoundTripper(func(request *http.Request) (*http.Response, error) {
		response, err := original.RoundTrip(request)
		if err == nil {
			response.Body = &gatewayObservedStreamBody{ReadCloser: response.Body, readingAgain: reading, closed: closed}
		}
		return response, err
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gateway-alias","stream":true,"messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { fixture.engine.ServeHTTP(response, request); close(done) }()
	<-reading // The first business frame has been delivered; the next upstream read is idle.
	fixture.fake.Advance(59 * time.Second)
	select {
	case <-closed:
		t.Error("idle stream closed before its 60-second budget")
	default:
	}
	fixture.fake.Advance(time.Second)
	select {
	case <-closed:
	default:
		t.Error("idle timeout did not close the upstream body")
		cancel()
	}
	<-done
	assert.Zero(t, calls)
	assert.Contains(t, response.Body.String(), "Hello")
	assert.Contains(t, response.Body.String(), `"code":"upstream_error"`)
	record, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
	require.NoError(t, err)
	assert.Equal(t, "partial", record.Status)
}

func TestTextGatewayFailoverCannotExceedFrozenReservation(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		t.Run(fmt.Sprint("recovery=", recovery), func(t *testing.T) {
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
			_, err := model.PostLedgerEntries(model.LedgerPosting{Account: model.UserWalletAccount(fixture.user.Id), Type: model.LedgerEntryAdjustment, Amount: money.Amount{Nanos: -999_900_000, Currency: "USD"}, Remark: "controlled failover starting balance"})
			require.NoError(t, err)
			largerCalls := 0
			larger := addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) { largerCalls++; w.WriteHeader(503) })
			require.NoError(t, model.DB.Model(larger).Update("parameter_overrides", `{"n":128}`).Error)
			if recovery {
				addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Recovered"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
				})
			}
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
			assert.Zero(t, largerCalls, "a candidate cannot increase the frozen reservation after the first send")
			_, total, err := model.ListGatewayAttempts(t.Context(), response.Header().Get(common.RequestIdKey), 0, 10)
			require.NoError(t, err)
			wantAttempts, wantBalance := int64(1), int64(100_000)
			if recovery {
				wantAttempts, wantBalance = 2, 64_000
				assert.Contains(t, response.Body.String(), "Recovered")
			}
			assert.Equal(t, wantAttempts, total)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, wantBalance, wallet.Balance.Nanos)
			entries, _, err := model.ListLedgerEntries(wallet.Id, 0, 20)
			require.NoError(t, err)
			preconsumes := 0
			for _, entry := range entries {
				if entry.Type == model.LedgerEntryPreConsume {
					preconsumes++
				}
			}
			assert.Equal(t, 1, preconsumes)
		})
	}
}

func TestTextGatewayUntrackedNativeResponseIsRejectedBeforeSend(t *testing.T) {
	calls := 0
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: conversation.message.delta\ndata: {\"content\":\"First\"}\n\nevent: conversation.chat.completed\ndata: {\"usage\":{\"input_count\":10,\"output_count\":5,\"token_count\":15}}\n\n"))
	})
	require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", constant.ChannelTypeCoze).Error)
	addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Recovered\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	})
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","stream":true,"user":"preview-user","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
	assert.Zero(t, calls)
	assert.NotContains(t, response.Body.String(), "First")
	assert.Contains(t, response.Body.String(), "Recovered")
	_, total, err := model.ListGatewayAttempts(t.Context(), response.Header().Get(common.RequestIdKey), 0, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
}

func TestTextGatewaySDKPreparationDoesNotSpendAttempt(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid SDK credentials must not be sent") })
	t.Setenv("GATEWAY_MAX_ATTEMPTS", "1")
	require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", constant.ChannelTypeAws).Error)
	require.NoError(t, model.DB.Model(fixture.channel).Update("upstream_model_name", "amazon.nova-lite-v1:0").Error)
	addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Recovered"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	})
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
	assert.Contains(t, response.Body.String(), "Recovered")
	_, total, err := model.ListGatewayAttempts(t.Context(), response.Header().Get(common.RequestIdKey), 0, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
}

func TestTextGatewaySDKCannotHideAdditionalSends(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("SDK must use its own transport") })
	t.Setenv("GATEWAY_MAX_ATTEMPTS", "1")
	require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Updates(map[string]any{"type": constant.ChannelTypeAws, "key": "test-bedrock-token|us-east-1"}).Error)
	require.NoError(t, model.DB.Model(fixture.channel).Update("upstream_model_name", "amazon.nova-lite-v1:0").Error)
	calls := 0
	client := service.GetHttpClient()
	original := client.Transport
	t.Cleanup(func() { client.Transport = original })
	client.Transport = channelProbeRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 503, Header: http.Header{"Content-Type": {"application/json"}, "X-Amzn-Errortype": {"ServiceUnavailableException"}}, Body: io.NopCloser(strings.NewReader(`{"message":"unavailable"}`)), Request: request}, nil
	})
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
	assert.Equal(t, 1, calls, "the SDK cannot send outside the shared Attempt budget")
	_, total, err := model.ListGatewayAttempts(t.Context(), response.Header().Get(common.RequestIdKey), 0, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
	wallet, err := model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	assert.Equal(t, int64(1_000_000_000), wallet.Balance.Nanos)
}

func TestTextGatewaySDKRejectionsRetainStatusAndProviderScope(t *testing.T) {
	for _, tc := range []struct {
		status, sends     int
		nativeCode, class string
	}{{400, 1, "ValidationException", "invalid_request"}, {401, 2, "UnrecognizedClientException", "credentials"}, {403, 2, "AccessDeniedException", "credentials"}, {404, 3, "ResourceNotFoundException", "model_not_found"}, {429, 2, "ServiceQuotaExceededException", "account_quota"}} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("SDK must use its own transport") })
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Updates(map[string]any{"type": constant.ChannelTypeAws, "key": "test-bedrock-token|us-east-1"}).Error)
			require.NoError(t, model.DB.Model(fixture.channel).Update("upstream_model_name", "amazon.nova-lite-v1:0").Error)
			sibling := *fixture.channel
			sibling.ID, sibling.UpstreamModelName = 0, "amazon.nova-pro-v1:0"
			require.NoError(t, model.DB.Create(&sibling).Error)
			price := *fixture.purchase
			price.ID, price.OwnerID = 0, sibling.ID
			require.NoError(t, model.CreatePriceVersion(t.Context(), &price, fixture.sale.CreatedBy))
			recoveryCalls, sdkCalls := 0, 0
			addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) {
				recoveryCalls++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Recovered"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
			})
			client := service.GetHttpClient()
			original := client.Transport
			t.Cleanup(func() { client.Transport = original })
			client.Transport = channelProbeRoundTripper(func(request *http.Request) (*http.Response, error) {
				if !strings.Contains(request.URL.Host, "bedrock-runtime") {
					return original.RoundTrip(request)
				}
				sdkCalls++
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {"application/json"}, "X-Amzn-Errortype": {tc.nativeCode}}, Body: io.NopCloser(strings.NewReader(`{"message":"private-sdk-error"}`)), Request: request}, nil
			})
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
			if tc.status == 404 {
				assert.Equal(t, 2, sdkCalls, "a model failure excludes only its Channel")
			} else {
				assert.Equal(t, 1, sdkCalls)
			}
			if tc.sends > 1 {
				assert.Equal(t, 1, recoveryCalls)
			} else {
				assert.Zero(t, recoveryCalls)
			}
			assert.NotContains(t, response.Body.String(), "private-sdk-error")
			attempts, total, err := model.ListGatewayAttempts(t.Context(), response.Header().Get(common.RequestIdKey), 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, tc.sends, total)
			for _, attempt := range attempts {
				if attempt.ChannelID == fixture.channel.ID {
					assert.Equal(t, tc.class, attempt.ErrorClass)
				}
			}
		})
	}
}

func TestTextGatewayStreamInvalidRequestStopsBeforeSwitching(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"error\":{\"type\":\"invalid_request_error\",\"code\":\"invalid_value\",\"message\":\"provider-secret\"}}\n\n"))
	})
	calls := 0
	addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(503) })
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","stream":true,"messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
	assert.Zero(t, calls)
	assert.NotContains(t, response.Body.String(), "provider-secret")
	attempts, total, err := model.ListGatewayAttempts(t.Context(), response.Header().Get(common.RequestIdKey), 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	assert.Equal(t, "invalid_request", attempts[0].ErrorClass)
	assert.Equal(t, "stop", attempts[0].FailoverDecision)
}

func TestTextGatewayDifyStreamErrorsRespectFailureScope(t *testing.T) {
	for _, tc := range []struct {
		name, code, class string
		status            int
		sends             int
	}{
		{"invalid request", "invalid_param", "invalid_request", 400, 1},
		{"credentials", "invalid_api_key", "credentials", 401, 2},
		{"account quota", "insufficient_quota", "account_quota", 429, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failedCalls, recoveryCalls := 0, 0
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				failedCalls++
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"event\":\"error\",\"status\":%d,\"code\":%q,\"message\":\"provider-secret\"}\n\n", tc.status, tc.code)
			})
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", constant.ChannelTypeDify).Error)
			sibling := *fixture.channel
			sibling.ID, sibling.UpstreamModelName = 0, "another-dify-model"
			require.NoError(t, model.DB.Create(&sibling).Error)
			price := *fixture.purchase
			price.ID, price.OwnerID = 0, sibling.ID
			require.NoError(t, model.CreatePriceVersion(t.Context(), &price, fixture.sale.CreatedBy))
			addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) {
				recoveryCalls++
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Recovered\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
			})
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","stream":true,"messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
			assert.Equal(t, 1, failedCalls, "invalid requests stop; account failures exclude the Provider")
			assert.Equal(t, tc.sends-1, recoveryCalls)
			assert.NotContains(t, response.Body.String(), "provider-secret")
			attempts, total, err := model.ListGatewayAttempts(t.Context(), response.Header().Get(common.RequestIdKey), 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, tc.sends, total)
			for _, attempt := range attempts {
				if attempt.ChannelID == fixture.channel.ID {
					assert.Equal(t, tc.class, attempt.ErrorClass)
				}
			}
		})
	}
}

func TestTextGatewayFailoverClearsProviderOrganization(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "first-account", r.Header.Get("OpenAI-Organization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"code":"overloaded"}}`))
	})
	require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("open_ai_organization", "first-account").Error)
	addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("OpenAI-Organization"), "the next Provider must not inherit another account's organization")
		assert.Equal(t, "Bearer additional-secret", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Recovered"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	})
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "Recovered")
}

func TestTextGatewayStreamFailoverNeverLeaksErrorsOrEndsEarly(t *testing.T) {
	for _, business := range []bool{false, true} {
		t.Run(fmt.Sprint("business=", business), func(t *testing.T) {
			logFile, err := os.Create(filepath.Join(t.TempDir(), "debug.log"))
			require.NoError(t, err)
			common.LogWriterMu.Lock()
			previousWriter, previousErrorWriter, previousDebug := gin.DefaultWriter, gin.DefaultErrorWriter, common.DebugEnabled
			gin.DefaultWriter, gin.DefaultErrorWriter, common.DebugEnabled = logFile, logFile, true
			common.LogWriterMu.Unlock()
			t.Cleanup(func() {
				common.LogWriterMu.Lock()
				gin.DefaultWriter, gin.DefaultErrorWriter, common.DebugEnabled = previousWriter, previousErrorWriter, previousDebug
				common.LogWriterMu.Unlock()
				require.NoError(t, logFile.Close())
			})
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(": PING\n\ndata: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
				if business {
					_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"First\"}}]}\n\n"))
				}
				_, _ = w.Write([]byte("data: {\"error\":{\"type\":\"server_error\",\"code\":123,\"message\":\"Bearer provider-secret https://private.invalid/?key=secret\"}}\n\n"))
			})
			secondCalls := 0
			addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) {
				secondCalls++
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Recovered\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
			})
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","stream":true,"messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
			assert.NotContains(t, response.Body.String(), "secret")
			assert.NotContains(t, response.Body.String(), "private.invalid")
			logged, err := os.ReadFile(logFile.Name())
			require.NoError(t, err)
			assert.NotContains(t, string(logged), "provider-secret")
			assert.NotContains(t, string(logged), "private.invalid")
			if business {
				assert.Zero(t, secondCalls)
				assert.Contains(t, response.Body.String(), `"code":"upstream_error"`)
				assert.NotContains(t, response.Body.String(), "Recovered")
			} else {
				assert.Equal(t, 1, secondCalls)
				assert.Contains(t, response.Body.String(), "Recovered")
				assert.Equal(t, 1, strings.Count(response.Body.String(), "[DONE]"))
				assert.Less(t, strings.Index(response.Body.String(), "Recovered"), strings.Index(response.Body.String(), "[DONE]"))
			}
		})
	}
}

func TestTextGatewayFirstByteTimeoutCancelsUpstreamAndSwitches(t *testing.T) {
	entered, cancelled := make(chan struct{}), make(chan struct{})
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
		close(cancelled)
	})
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", fixture.sale.CreatedBy).Update("role", common.RoleRootUser).Error)
	root := createWalletTestAccessToken(t, fixture.sale.CreatedBy, "option:write")
	updated := adminSecurityRequest(fixture.engine, root, http.MethodPut, "/api/option/", `{"key":"GatewayAttemptFirstByteSeconds","value":12}`)
	require.Contains(t, updated.Body.String(), `"success":true`)
	addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Recovered"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`))
	request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { fixture.engine.ServeHTTP(response, request); close(done) }()
	<-entered
	fixture.fake.Advance(12 * time.Second)
	<-done
	<-cancelled
	require.Equal(t, 200, response.Code, response.Body.String())
	record, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
	require.NoError(t, err)
	attempts, total, err := model.ListGatewayAttempts(t.Context(), record.ID, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 2, total)
	for _, attempt := range attempts {
		if attempt.ChannelID == fixture.channel.ID {
			assert.Equal(t, "first_byte_timeout", attempt.ErrorClass)
			assert.Equal(t, "failed", attempt.Status)
			assert.Equal(t, "switch", attempt.FailoverDecision)
			assert.Zero(t, attempt.Cost.Nanos)
		}
	}
}

func TestTextGatewayTotalBudgetFiveSecondBoundaryAndAttemptLimit(t *testing.T) {
	for _, test := range []struct {
		name, total, limit string
		sends              int
	}{
		{"under five", "60", "5", 2}, {"exactly five", "61", "5", 3}, {"one attempt", "90", "1", 1}, {"five attempts", "300", "5", 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			var fake *clock.Fake
			calls := 0
			handler := func(w http.ResponseWriter, r *http.Request) {
				calls++
				fake.Advance(28 * time.Second)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(503)
				_, _ = w.Write([]byte(`{"error":{"code":"overloaded"},"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
			}
			fixture := newTextGatewayFixture(t, handler)
			require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", fixture.sale.CreatedBy).Update("role", common.RoleRootUser).Error)
			root := createWalletTestAccessToken(t, fixture.sale.CreatedBy, "option:write")
			for key, value := range map[string]string{"GatewayTotalFirstByteSeconds": test.total, "GatewayMaxAttempts": test.limit} {
				updated := adminSecurityRequest(fixture.engine, root, http.MethodPut, "/api/option/", fmt.Sprintf(`{"key":%q,"value":%q}`, key, value))
				require.Contains(t, updated.Body.String(), `"success":true`, updated.Body.String())
			}
			fake = fixture.fake
			for range 4 {
				addTextGatewayChannel(t, fixture, handler)
			}
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
			assert.Equal(t, test.sends, calls)
			record, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
			require.NoError(t, err)
			assert.Zero(t, record.Receivable.Nanos)
			attempts, total, err := model.ListGatewayAttempts(t.Context(), record.ID, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, test.sends, total)
			assert.Equal(t, "stop", attempts[len(attempts)-1].FailoverDecision)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, int64(1_000_000_000), wallet.Balance.Nanos)
			reconciliation, err := model.ReconcileWallets(100)
			require.NoError(t, err)
			assert.Empty(t, reconciliation.Mismatches)
		})
	}
}

func TestTextGatewayPreparationFailureDoesNotSpendAttempts(t *testing.T) {
	for _, badURL := range []bool{false, true} {
		t.Run(fmt.Sprint("bad URL=", badURL), func(t *testing.T) {
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid prepared candidate must not be sent") })
			if badURL {
				require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("base_url", "http://%zz").Error)
			} else {
				require.NoError(t, model.DB.Model(fixture.channel).Update("parameter_overrides", `{"n":129}`).Error)
			}
			t.Setenv("GATEWAY_MAX_ATTEMPTS", "1")
			healthy := addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Recovered"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
			})
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
			require.Equal(t, 200, response.Code, response.Body.String())
			record, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
			require.NoError(t, err)
			attempts, total, err := model.ListGatewayAttempts(t.Context(), record.ID, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.Equal(t, healthy.ID, attempts[0].ChannelID)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			entries, _, err := model.ListLedgerEntries(wallet.Id, 0, 20)
			require.NoError(t, err)
			assert.Len(t, entries, 4)
			assert.Equal(t, int64(999964000), wallet.Balance.Nanos)
		})
	}
}

func TestTextGatewaySafetyStreamStopsWithoutWaitingForProviderEOF(t *testing.T) {
	sent := make(chan struct{})
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"error\":{\"code\":\"content_filter\",\"message\":\"private-secret\"}}\n\n"))
		w.(http.Flusher).Flush()
		close(sent)
		<-r.Context().Done()
	})
	addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) { t.Error("safety rejection must not switch Channel") })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gateway-alias","stream":true,"messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { fixture.engine.ServeHTTP(response, request); close(done) }()
	<-sent
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		t.Fatal("the gateway waited for Provider EOF after a safety rejection")
	}
	assert.NotContains(t, response.Body.String(), "private-secret")
	record, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
	require.NoError(t, err)
	attempts, total, err := model.ListGatewayAttempts(t.Context(), record.ID, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	assert.Equal(t, "content_safety", attempts[0].ErrorClass)
	assert.Equal(t, "stop", attempts[0].FailoverDecision)
	assert.Zero(t, record.Receivable.Nanos)
}

func TestTextGatewayNativeModelNotFoundIsDistinctFromUnknownPath(t *testing.T) {
	for _, missingModel := range []bool{false, true} {
		t.Run(fmt.Sprint("model missing=", missingModel), func(t *testing.T) {
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(404)
				if missingModel {
					_, _ = w.Write([]byte(`{"error":"model 'upstream-model' not found"}`))
				} else {
					_, _ = w.Write([]byte(`{"error":"unknown endpoint"}`))
				}
			})
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", constant.ChannelTypeOllama).Error)
			addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Recovered"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
			})
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
			if missingModel {
				assert.Equal(t, 200, response.Code, response.Body.String())
			} else {
				assert.Equal(t, 404, response.Code)
			}
			record, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
			require.NoError(t, err)
			attempts, _, err := model.ListGatewayAttempts(t.Context(), record.ID, 0, 10)
			require.NoError(t, err)
			for _, attempt := range attempts {
				if attempt.ChannelID == fixture.channel.ID {
					if missingModel {
						assert.Equal(t, "model_not_found", attempt.ErrorClass)
					} else {
						assert.Equal(t, "upstream_error", attempt.ErrorClass)
					}
				}
			}
		})
	}
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
		name, override string
		provider       int
		limit          int
	}{
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
			caps := capability.Set{Features: []capability.Feature{capability.Streaming, capability.ToolCalling}, InputModalities: []capability.Modality{capability.Text}, OutputModalities: []capability.Modality{capability.Text}, Limits: capability.Limits{MaxOutputTokens: common.GetPointer(int64(32)), ContextLength: common.GetPointer(int64(32))}}
			require.NoError(t, model.DB.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Update("capabilities", caps).Error)
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", test.provider).Error)
			if test.override != "" {
				require.NoError(t, model.DB.Model(fixture.channel).Update("parameter_overrides", test.override).Error)
			}
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}]}`)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			if test.provider == constant.ChannelTypeOllama {
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
			if test.status == "partial" {
				assert.NotContains(t, response.Body.String(), "[DONE]", "an interrupted stream must end with an error instead of a success terminator")
				assert.Contains(t, response.Body.String(), `"code":"upstream_error"`)
			}
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
			addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) {
				t.Error("downstream cancellation must not dispatch another Provider")
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
		upstreamStatus                         int
		stream                                 bool
		charge, cost                           int64
	}{
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
			addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) {
				t.Error("downstream write failures must not dispatch another Provider")
			})
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
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Ability{}))
}
