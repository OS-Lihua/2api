package router

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"fmt"
	"image"
	"image/png"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/pkg/capability"
	"github.com/QuantumNous/new-api/pkg/channelhealth"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/pkg/wsmanager"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	relaytypes "github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/gorilla/websocket"

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
	gormlogger "gorm.io/gorm/logger"
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
	previousHealth := service.ChannelHealth
	service.ChannelHealth = channelhealth.New(nil)
	t.Cleanup(func() { service.ChannelHealth = previousHealth })
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
	return textGatewayFixture{engine: engine, user: user, token: token, adminPAT: createWalletTestAccessToken(t, actor.Id, "request:read", "ledger:read", "channel:read"), userPAT: createWalletTestAccessToken(t, user.Id, "log:read"), offering: offering, channel: channel, sale: sale, purchase: purchase, fake: fake}
}

func assertTextGatewayLedger(t *testing.T, request model.GatewayRequest, expectedCost int64) {
	t.Helper()
	var entries []model.LedgerEntry
	require.NoError(t, model.DB.Where("request_id = ?", request.ID).Find(&entries).Error)
	var charged, revenue, payable, reserved, released int64
	var reservations, charges, revenues int
	for _, entry := range entries {
		assert.Equal(t, money.Currency("USD"), entry.Amount.Currency)
		switch entry.AccountType {
		case model.LedgerAccountUserWallet:
			switch entry.Type {
			case model.LedgerEntryPreConsume:
				reserved += entry.Amount.Nanos
				reservations++
			case model.LedgerEntryPreConsumeRelease:
				released += entry.Amount.Nanos
			case model.LedgerEntryCharge:
				charged += entry.Amount.Nanos
				charges++
			}
		case model.LedgerAccountPlatformRevenue:
			revenue += entry.Amount.Nanos
			revenues++
		case model.LedgerAccountProviderPayable:
			payable += entry.Amount.Nanos
			assert.NotEmpty(t, entry.AttemptId)
		}
	}
	assert.LessOrEqual(t, reservations, 1)
	assert.LessOrEqual(t, charges, 1)
	assert.LessOrEqual(t, revenues, 1)
	assert.Equal(t, -reserved, released)
	assert.Equal(t, -request.Receivable.Nanos, charged)
	assert.Equal(t, request.Receivable.Nanos, revenue)
	assert.Equal(t, expectedCost, payable)
	reconciliation, err := model.ReconcileWallets(100)
	require.NoError(t, err)
	assert.Empty(t, reconciliation.Mismatches)
}

func dialResponsesGateway(t *testing.T, fixture textGatewayFixture) *websocket.Conn {
	t.Helper()
	require.NoError(t, model.DB.Model(fixture.channel).Update("protocol", protocol.OpenAIResponses).Error)
	var provider model.Provider
	require.NoError(t, model.DB.First(&provider, fixture.channel.ProviderID).Error)
	provider.SetSetting(dto.ProviderSettings{ResponsesWebSocketEnabled: true})
	require.NoError(t, model.DB.Model(&provider).Update("setting", provider.Setting).Error)
	gateway := httptest.NewServer(fixture.engine)
	t.Cleanup(gateway.Close)
	client, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gateway.URL, "http")+"/v1/responses", http.Header{"Authorization": {"Bearer sk-" + fixture.token.Key}})
	if response != nil {
		require.NoError(t, response.Body.Close())
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestResponsesWSGatewayAccountingOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, terminal, source, status   string
		sale, cost                       int64
		credentials                      bool
		delta, cancel, disconnect, probe bool
	}{
		{name: "partial usage without delivery", terminal: `{"type":"response.failed","response":{"id":"active","status":"failed","error":{"code":"server_error"},"usage":{"input_tokens":1000}}}`, source: "none", status: "failed", cost: 1_000_000},
		{name: "valid base counters with invalid total", terminal: `{"type":"response.completed","response":{"id":"active","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":"invalid"}}}`, source: "upstream", status: "success", sale: 1_836_000, cost: 1_020_000},
		{name: "terminal function call without usage", terminal: `{"type":"response.completed","response":{"id":"active","status":"completed","output":[{"type":"function_call","name":"hello","arguments":""}]}}`, source: "estimated", status: "success", sale: 5400, cost: 3000},
		{name: "purchase probes use final wire and Provider header", terminal: `{"type":"response.completed","response":{"id":"active","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}`, source: "upstream", status: "success", sale: 1_836_000, cost: 510_000, probe: true},
		{name: "client credentials never become Provider authentication", terminal: `{"type":"response.completed","response":{"id":"active","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1000,"output_tokens":10}}}`, source: "upstream", status: "success", sale: 1_836_000, cost: 1_020_000, credentials: true},
		{name: "cancel retains complete reported usage", terminal: `{"type":"response.cancelled","response":{"id":"active","status":"cancelled","usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}`, source: "upstream", status: "cancelled", sale: 1_836_000, cost: 1_020_000, delta: true, cancel: true},
		{name: "disconnect settles generated output once", source: "estimated", status: "cancelled", sale: 5400, cost: 3000, delta: true, disconnect: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstreamDone := make(chan struct{})
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.credentials {
					assert.Equal(t, "Bearer provider-secret", r.Header.Get("Authorization"))
					assert.Empty(t, r.Header.Get("Sec-WebSocket-Protocol"))
				}
				peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if !assert.NoError(t, err) {
					return
				}
				defer peer.Close()
				defer close(upstreamDone)
				_, _, err = peer.ReadMessage()
				if !assert.NoError(t, err) {
					return
				}
				if tc.delta {
					if !assert.NoError(t, peer.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"active","status":"in_progress"}}`))) {
						return
					}
					if !assert.NoError(t, peer.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"hello"}`))) {
						return
					}
				}
				if tc.disconnect {
					_, _, _ = peer.ReadMessage()
					return
				}
				if tc.cancel {
					_, control, err := peer.ReadMessage()
					if !assert.NoError(t, err) {
						return
					}
					assert.JSONEq(t, `{"type":"response.cancel","response_id":"active"}`, string(control))
				}
				if !assert.NoError(t, peer.WriteMessage(websocket.TextMessage, []byte(tc.terminal))) {
					return
				}
				_, _, _ = peer.ReadMessage()
			})
			require.NoError(t, model.DB.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Update("model_name", "gpt-4o").Error)
			if tc.probe {
				price := *fixture.purchase
				price.ID = 0
				price.EffectiveAt = clock.Now().Unix() + 1
				price.Expression = `param("type") == "response.create" && header("x-provider-tier") == "purchase" ? tier("wire", p * 0.5 + c) : tier("wrong", p + c * 2)`
				require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("header_override", `{"X-Provider-Tier":"purchase"}`).Error)
				require.NoError(t, model.CreatePriceVersion(t.Context(), &price, fixture.sale.CreatedBy))
				fixture.fake.Advance(2 * time.Second)
			}
			if tc.credentials {
				require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("header_override", `{"Authorization":"{client_header:Authorization}","Sec-WebSocket-Protocol":"{client_header:Sec-WebSocket-Protocol}","*":""}`).Error)
			}
			client := dialResponsesGateway(t, fixture)
			require.NoError(t, client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gateway-alias","input":"hello","max_output_tokens":16}`)))
			require.NoError(t, client.SetReadDeadline(time.Now().Add(5*time.Second)))
			if tc.delta {
				for range 2 {
					_, _, err := client.ReadMessage()
					require.NoError(t, err)
				}
			}
			if tc.cancel {
				require.NoError(t, client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.cancel","response_id":"active"}`)))
			}
			if !tc.disconnect {
				_, raw, err := client.ReadMessage()
				require.NoError(t, err)
				assert.JSONEq(t, tc.terminal, string(raw))
			}
			require.NoError(t, client.Close())
			select {
			case <-upstreamDone:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream work survived client closure")
			}
			var records []model.GatewayRequest
			require.Eventually(t, func() bool {
				var err error
				records, _, err = model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
				if err != nil || len(records) != 1 || records[0].Status == "settlement_pending" {
					return false
				}
				_, count, err := model.ListGatewayAttempts(t.Context(), records[0].ID, 0, 10)
				return err == nil && count == 1
			}, 5*time.Second, 10*time.Millisecond)
			record := records[0]
			assert.Equal(t, tc.status, record.Status)
			assert.Equal(t, tc.source, record.UsageSource)
			assert.Equal(t, tc.sale, record.Receivable.Nanos)
			assertTextGatewayLedger(t, record, tc.cost)
			attempts, _, err := model.ListGatewayAttempts(t.Context(), record.ID, 0, 10)
			require.NoError(t, err)
			assert.Equal(t, tc.cost, attempts[0].Cost.Nanos)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, int64(1_000_000_000)-tc.sale, wallet.Balance.Nanos)
			_, err = model.RecoverGatewaySettlements(t.Context(), 100)
			require.NoError(t, err)
			assertTextGatewayLedger(t, record, tc.cost)
		})
	}
}

func newRealtimeGatewayServer(t *testing.T, engine *gin.Engine) *httptest.Server {
	t.Helper()
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(done); engine.ServeHTTP(w, r) }))
	t.Cleanup(func() {
		server.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Realtime gateway handler survived connection cleanup")
		}
	})
	return server
}

func dialRealtimeGateway(t *testing.T, fixture textGatewayFixture) *websocket.Conn {
	t.Helper()
	require.NoError(t, model.DB.Model(fixture.channel).Update("protocol", protocol.OpenAIRealtime).Error)
	require.NoError(t, model.DB.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Update("model_name", "gpt-4o").Error)
	gateway := newRealtimeGatewayServer(t, fixture.engine)
	client, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gateway.URL, "http")+"/v1/realtime?model=gateway-alias", http.Header{"Authorization": {"Bearer sk-" + fixture.token.Key}})
	if response != nil && response.Body != nil {
		require.NoError(t, response.Body.Close())
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.SetReadDeadline(time.Now().Add(5*time.Second)))
	return client
}

func TestRealtimeGatewayRejectsFixedPricingBeforeReservation(t *testing.T) {
	for _, owner := range []string{"sale", "purchase"} {
		t.Run(owner, func(t *testing.T) {
			var calls atomic.Int32
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			})
			require.NoError(t, model.DB.Model(fixture.channel).Update("protocol", protocol.OpenAIRealtime).Error)
			price := *fixture.sale
			if owner == "purchase" {
				price = *fixture.purchase
			}
			price.ID, price.EffectiveAt, price.Expression = 0, clock.Now().Unix()+1, `tier("unsupported", fixed(0))`
			require.NoError(t, model.CreatePriceVersion(t.Context(), &price, fixture.sale.CreatedBy))
			fixture.fake.Advance(2 * time.Second)
			server := newRealtimeGatewayServer(t, fixture.engine)
			client, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/realtime?model=gateway-alias", http.Header{"Authorization": {"Bearer sk-" + fixture.token.Key}})
			if client != nil {
				require.NoError(t, client.Close())
			}
			require.Error(t, err)
			require.NotNil(t, response)
			assert.GreaterOrEqual(t, response.StatusCode, http.StatusBadRequest)
			require.NoError(t, response.Body.Close())
			assert.Zero(t, calls.Load())
			var lifecycles, postings int64
			require.NoError(t, model.DB.Model(&model.GatewayBilling{}).Where("user_id = ?", fixture.user.Id).Count(&lifecycles).Error)
			require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("type = ?", model.LedgerEntryPreConsume).Count(&postings).Error)
			assert.Zero(t, lifecycles)
			assert.Zero(t, postings)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, int64(1_000_000_000), wallet.Balance.Nanos)
		})
	}
}

func TestRealtimeGatewayAutomaticVAD(t *testing.T) {
	var fixture textGatewayFixture
	upstreamDone := make(chan struct{})
	inputAudio := base64.StdEncoding.EncodeToString(make([]byte, 48000))
	outputAudio := base64.StdEncoding.EncodeToString(make([]byte, 9600))
	fixture = newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if !assert.NoError(t, err) {
			return
		}
		defer peer.Close()
		defer close(upstreamDone)
		if !assert.NoError(t, peer.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.created","session":{"model":"upstream-model","modalities":["text","audio"],"input_audio_format":"pcm16","output_audio_format":"pcm16","max_response_output_tokens":512,"turn_detection":{"type":"server_vad","create_response":true,"interrupt_response":true}}}`))) {
			return
		}
		_, configure, err := peer.ReadMessage()
		if !assert.NoError(t, err) {
			return
		}
		if !assert.Equal(t, "session.update", ojson.GetBytes(configure, "type").String()) {
			return
		}
		assert.Equal(t, "false", ojson.GetBytes(configure, "session.turn_detection.create_response").Raw)
		assert.Equal(t, "false", ojson.GetBytes(configure, "session.turn_detection.interrupt_response").Raw)
		if !assert.NoError(t, peer.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.updated","session":{"model":"upstream-model","modalities":["text","audio"],"input_audio_format":"pcm16","output_audio_format":"pcm16","max_response_output_tokens":512,"turn_detection":{"type":"server_vad","create_response":false,"interrupt_response":false}}}`))) {
			return
		}
		_, appendAudio, err := peer.ReadMessage()
		if !assert.NoError(t, err) {
			return
		}
		assert.Equal(t, "input_audio_buffer.append", ojson.GetBytes(appendAudio, "type").String())
		var billing model.GatewayBilling
		if !assert.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).First(&billing).Error) {
			return
		}
		assert.Equal(t, "reserved", billing.Phase)
		for _, raw := range []string{
			`{"type":"input_audio_buffer.speech_started","item_id":"voice-input","audio_start_ms":0}`,
			`{"type":"input_audio_buffer.speech_stopped","item_id":"voice-input","audio_end_ms":1000}`,
			`{"type":"input_audio_buffer.committed","item_id":"voice-input"}`,
		} {
			if !assert.NoError(t, peer.WriteMessage(websocket.TextMessage, []byte(raw))) {
				return
			}
		}
		_, create, err := peer.ReadMessage()
		if !assert.NoError(t, err) {
			return
		}
		assert.Equal(t, "response.create", ojson.GetBytes(create, "type").String())
		assert.Equal(t, "512", ojson.GetBytes(create, "response.max_output_tokens").Raw)
		for _, raw := range []string{
			`{"type":"response.created","response":{"id":"voice-response","status":"in_progress","conversation_id":"conversation"}}`,
			`{"type":"response.audio.delta","response_id":"voice-response","output_index":0,"delta":"` + outputAudio + `"}`,
			`{"type":"response.done","response":{"id":"voice-response","status":"completed","conversation_id":"conversation","usage":{"input_tokens":10,"output_tokens":4,"input_token_details":{"text_tokens":0,"audio_tokens":10,"cached_tokens":0},"output_token_details":{"text_tokens":0,"audio_tokens":4}}}}`,
		} {
			if !assert.NoError(t, peer.WriteMessage(websocket.TextMessage, []byte(raw))) {
				return
			}
		}
		_, _, _ = peer.ReadMessage()
	})
	var unified model.Model
	require.NoError(t, model.DB.First(&unified, fixture.channel.ModelID).Error)
	unified.Capabilities.InputModalities = []capability.Modality{capability.Text, capability.Audio}
	unified.Capabilities.OutputModalities = []capability.Modality{capability.Text, capability.Audio}
	require.NoError(t, model.DB.Save(&unified).Error)
	for _, change := range []struct {
		price      *model.PriceVersion
		expression string
	}{{fixture.sale, `tier("voice", ai * 3 + ao * 6)`}, {fixture.purchase, `tier("voice", ai * 2 + ao * 4)`}} {
		version := *change.price
		version.ID, version.EffectiveAt, version.Expression = 0, clock.Now().Unix()+1, change.expression
		require.NoError(t, model.CreatePriceVersion(t.Context(), &version, fixture.sale.CreatedBy))
	}
	fixture.fake.Advance(2 * time.Second)
	client := dialRealtimeGateway(t, fixture)
	for range 2 {
		_, metadata, err := client.ReadMessage()
		require.NoError(t, err)
		assert.Equal(t, "true", ojson.GetBytes(metadata, "session.turn_detection.create_response").Raw)
		assert.Equal(t, "true", ojson.GetBytes(metadata, "session.turn_detection.interrupt_response").Raw)
	}
	require.NoError(t, client.WriteMessage(websocket.TextMessage, []byte(`{"type":"input_audio_buffer.append","audio":"`+inputAudio+`"}`)))
	for {
		_, raw, err := client.ReadMessage()
		require.NoError(t, err)
		if ojson.GetBytes(raw, "type").String() == "error" {
			t.Fatalf("automatic response rejected: %s", ojson.GetBytes(raw, "error.code").String())
		}
		if ojson.GetBytes(raw, "type").String() == "response.done" {
			break
		}
	}
	require.NoError(t, client.Close())
	select {
	case <-upstreamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("VAD upstream survived client close")
	}
	var records []model.GatewayRequest
	require.Eventually(t, func() bool {
		var count int64
		var err error
		records, count, err = model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
		if err != nil || count != 1 || len(records) != 1 || records[0].Status == "settlement_pending" {
			return false
		}
		_, count, err = model.ListGatewayAttempts(t.Context(), records[0].ID, 0, 10)
		return err == nil && count == 1
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, "success", records[0].Status)
	assert.Equal(t, "upstream", records[0].UsageSource)
	assert.Equal(t, int64(10), records[0].Usage.AudioInputTokens)
	assert.Equal(t, int64(4), records[0].Usage.AudioOutputTokens)
	assert.Equal(t, int64(48600), records[0].Receivable.Nanos)
	assertTextGatewayLedger(t, records[0], 36000)
}

func TestRealtimeGatewayConcurrentResponses(t *testing.T) {
	upstreamDone := make(chan struct{})
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if !assert.NoError(t, err) {
			return
		}
		defer peer.Close()
		defer close(upstreamDone)
		if !assert.NoError(t, peer.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.created","session":{"modalities":["text"],"turn_detection":null}}`))) {
			return
		}
		_, first, err := peer.ReadMessage()
		if !assert.NoError(t, err) {
			return
		}
		assert.Equal(t, "response.create", ojson.GetBytes(first, "type").String())
		if !assert.NoError(t, peer.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"concurrent-first","status":"in_progress","conversation_id":null}}`))) {
			return
		}
		_, second, err := peer.ReadMessage()
		if !assert.NoError(t, err) {
			return
		}
		assert.Equal(t, "response.create", ojson.GetBytes(second, "type").String())
		if !assert.NoError(t, peer.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"concurrent-second","status":"in_progress","conversation_id":null}}`))) {
			return
		}
		_, metadata, err := peer.ReadMessage()
		if !assert.NoError(t, err) {
			return
		}
		assert.Equal(t, "session.update", ojson.GetBytes(metadata, "type").String())
		for _, raw := range []string{
			`{"type":"session.updated","session":{"modalities":["text"],"turn_detection":null}}`,
			`{"type":"response.text.delta","response_id":"concurrent-second","output_index":0,"delta":"world"}`,
			`{"type":"response.done","response":{"id":"concurrent-second","status":"completed","conversation_id":null,"usage":{"input_tokens":20,"output_tokens":2}}}`,
			`{"type":"response.text.delta","response_id":"concurrent-first","output_index":0,"delta":"hello"}`,
			`{"type":"response.done","response":{"id":"concurrent-first","status":"completed","conversation_id":null,"usage":{"input_tokens":10,"output_tokens":1}}}`,
		} {
			if !assert.NoError(t, peer.WriteMessage(websocket.TextMessage, []byte(raw))) {
				return
			}
		}
		_, _, _ = peer.ReadMessage()
	})
	client := dialRealtimeGateway(t, fixture)
	_, _, err := client.ReadMessage()
	require.NoError(t, err)
	for range 2 {
		require.NoError(t, client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","response":{"conversation":"none","input":[],"modalities":["text"],"max_output_tokens":16}}`)))
	}
	require.NoError(t, client.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.update","session":{"modalities":["text"]}}`)))
	completed := map[string]bool{}
	for len(completed) < 2 {
		_, raw, err := client.ReadMessage()
		require.NoError(t, err)
		if ojson.GetBytes(raw, "type").String() == "response.done" {
			completed[ojson.GetBytes(raw, "response.id").String()] = true
		}
	}
	require.NoError(t, client.Close())
	select {
	case <-upstreamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent upstream survived client close")
	}
	var records []model.GatewayRequest
	require.Eventually(t, func() bool {
		var count int64
		var err error
		records, count, err = model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
		return err == nil && count == 2 && records[0].Status != "settlement_pending" && records[1].Status != "settlement_pending"
	}, 5*time.Second, 10*time.Millisecond)
	var sale int64
	for _, record := range records {
		attempts, count, err := model.ListGatewayAttempts(t.Context(), record.ID, 0, 10)
		require.NoError(t, err)
		require.EqualValues(t, 1, count)
		assert.Equal(t, "success", record.Status)
		assert.Equal(t, fixture.channel.ID, attempts[0].ChannelID)
		cost := int64(12000)
		if record.Usage.PromptTokens == 20 {
			assert.Equal(t, int64(43200), record.Receivable.Nanos)
			cost = 24000
		} else {
			assert.Equal(t, int64(10), record.Usage.PromptTokens)
			assert.Equal(t, int64(21600), record.Receivable.Nanos)
		}
		assertTextGatewayLedger(t, record, cost)
		sale += record.Receivable.Nanos
	}
	assert.Equal(t, int64(64800), sale)
}

func TestRealtimeGatewaySettlesNativeResponses(t *testing.T) {
	var connections atomic.Int32
	upstreamDone := make(chan struct{})
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		connections.Add(1)
		assert.Equal(t, "/v1/realtime", r.URL.Path)
		assert.Equal(t, "upstream-model", r.URL.Query().Get("model"))
		assert.Equal(t, "Bearer provider-secret", r.Header.Get("Authorization"))
		peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if !assert.NoError(t, err) {
			return
		}
		defer peer.Close()
		defer close(upstreamDone)
		if !assert.NoError(t, peer.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.created","session":{"id":"native-session","model":"upstream-model","modalities":["text"],"turn_detection":null}}`))) {
			return
		}
		var previous []byte
		for i := range 2 {
			_, raw, err := peer.ReadMessage()
			if !assert.NoError(t, err) {
				return
			}
			assert.Equal(t, "response.create", ojson.GetBytes(raw, "type").String())
			if previous != nil {
				if !assert.NoError(t, peer.WriteMessage(websocket.TextMessage, previous)) {
					return
				}
			}
			id := fmt.Sprintf("native-%d", i)
			input, output := 1000, 10
			if i == 1 {
				input, output = 0, 0
			}
			for _, event := range []string{
				fmt.Sprintf(`{"type":"response.created","response":{"id":%q,"status":"in_progress","conversation_id":null}}`, id),
				fmt.Sprintf(`{"type":"response.text.delta","response_id":%q,"output_index":0,"delta":"hello"}`, id),
				fmt.Sprintf(`{"type":"response.done","response":{"id":%q,"status":"completed","conversation_id":null,"usage":{"input_tokens":%d,"output_tokens":%d,"total_tokens":%d,"input_token_details":{"cached_tokens":0,"audio_tokens":0},"output_token_details":{"audio_tokens":0}}}}`, id, input, output, input+output),
			} {
				if !assert.NoError(t, peer.WriteMessage(websocket.TextMessage, []byte(event))) {
					return
				}
				previous = []byte(event)
			}
		}
		_, _, _ = peer.ReadMessage()
	})
	require.NoError(t, model.DB.Model(fixture.channel).Update("protocol", protocol.OpenAIRealtime).Error)
	require.NoError(t, model.DB.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Update("model_name", "gpt-4o").Error)
	server := newRealtimeGatewayServer(t, fixture.engine)
	headers := http.Header{"Authorization": {"Bearer sk-" + fixture.token.Key}}
	client, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/realtime?model=gateway-alias", headers)
	if response != nil && response.Body != nil {
		require.NoError(t, response.Body.Close())
	}
	require.NoError(t, err)
	defer client.Close()
	require.NoError(t, client.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, metadata, err := client.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, "session.created", ojson.GetBytes(metadata, "type").String())
	for i := range 2 {
		require.NoError(t, client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","response":{"conversation":"none","input":[],"modalities":["text"],"max_output_tokens":16}}`)))
		for {
			_, raw, err := client.ReadMessage()
			require.NoError(t, err)
			if ojson.GetBytes(raw, "type").String() == "error" {
				t.Fatalf("native request rejected: code=%s message=%s", ojson.GetBytes(raw, "error.code").String(), ojson.GetBytes(raw, "error.message").String())
			}
			if ojson.GetBytes(raw, "type").String() == "response.done" && ojson.GetBytes(raw, "response.id").String() == fmt.Sprintf("native-%d", i) {
				break
			}
		}
	}
	require.NoError(t, client.Close())
	select {
	case <-upstreamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("native upstream survived client close")
	}
	var records []model.GatewayRequest
	require.Eventually(t, func() bool {
		var count int64
		var err error
		records, count, err = model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
		if err != nil || count != 2 {
			return false
		}
		for _, record := range records {
			_, count, err = model.ListGatewayAttempts(t.Context(), record.ID, 0, 10)
			if err != nil || count != 1 || record.Status == "settlement_pending" {
				return false
			}
		}
		return true
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, int32(1), connections.Load())
	var total int64
	for _, record := range records {
		assert.Equal(t, "success", record.Status)
		assert.Equal(t, "upstream", record.UsageSource)
		assert.Equal(t, "gateway-alias", record.RequestedName)
		cost := int64(0)
		if record.Usage.PromptTokens == 1000 {
			assert.Equal(t, int64(1_836_000), record.Receivable.Nanos)
			cost = 1_020_000
		} else {
			assert.Zero(t, record.Receivable.Nanos)
		}
		assertTextGatewayLedger(t, record, cost)
		total += record.Receivable.Nanos
	}
	assert.Equal(t, int64(1_836_000), total)
	wallet, err := model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	assert.Equal(t, int64(1_000_000_000)-total, wallet.Balance.Nanos)
}

func TestResponsesWSGatewayRejectsLegacyConnectionReuse(t *testing.T) {
	var moneyCalls, legacyCalls atomic.Int32
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		moneyCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	require.NoError(t, model.DB.AutoMigrate(&model.Ability{}, &model.Log{}))
	previousRatios := ratio_setting.ModelRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"gpt-4o":1}`))
	t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(previousRatios)) })
	require.NoError(t, model.DB.Model(fixture.user).Updates(map[string]any{"quota": 1_000_000, "setting": `{"billing_preference":"wallet_only"}`}).Error)
	require.NoError(t, model.DB.Model(fixture.token).Update("unlimited_quota", true).Error)
	legacyDone := make(chan struct{})
	legacyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if !assert.NoError(t, err) {
			return
		}
		defer peer.Close()
		defer close(legacyDone)
		for {
			_, _, err := peer.ReadMessage()
			if err != nil {
				return
			}
			index := legacyCalls.Add(1)
			if err := peer.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"legacy-%d","status":"completed","usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11}}}`, index))); err != nil {
				return
			}
		}
	}))
	t.Cleanup(legacyServer.Close)
	legacy := &model.Provider{Name: "Legacy WS Provider", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Key: "legacy-secret", BaseURL: &legacyServer.URL, Models: "gpt-4o", Group: "default"}
	legacy.SetSetting(dto.ProviderSettings{ResponsesWebSocketEnabled: true})
	require.NoError(t, model.DB.Create(legacy).Error)
	require.NoError(t, model.DB.Create(&model.Ability{ChannelId: legacy.Id, Model: "gpt-4o", Group: "default", Enabled: true}).Error)
	client := dialResponsesGateway(t, fixture)
	require.NoError(t, client.SetReadDeadline(time.Now().Add(5*time.Second)))
	require.NoError(t, client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-4o","input":"hello"}`)))
	_, raw, err := client.ReadMessage()
	require.NoError(t, err)
	require.Contains(t, string(raw), `"id":"legacy-1"`)
	require.NoError(t, client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gateway-alias","input":"hello"}`)))
	_, raw, err = client.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"status":409`)
	require.NoError(t, client.Close())
	select {
	case <-legacyDone:
	case <-time.After(5 * time.Second):
		t.Fatal("legacy Provider connection did not stop")
	}
	assert.Equal(t, int32(1), legacyCalls.Load())
	assert.Zero(t, moneyCalls.Load())
	_, count, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
	require.NoError(t, err)
	assert.Zero(t, count)
	wallet, err := model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	assert.Equal(t, int64(1_000_000_000), wallet.Balance.Nanos)
}

func TestResponsesWSGatewaySettlesLogicalRequests(t *testing.T) {
	var connections atomic.Int32
	received := make(chan string, 2)
	upstreamDone := make(chan struct{})
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if !assert.NoError(t, err) {
			return
		}
		connections.Add(1)
		defer ws.Close()
		defer close(upstreamDone)
		assert.Equal(t, "Bearer provider-secret", r.Header.Get("Authorization"))
		assert.Empty(t, r.Header.Get("Sec-WebSocket-Protocol"))
		for i := range 2 {
			_, raw, err := ws.ReadMessage()
			if !assert.NoError(t, err) {
				return
			}
			var event struct {
				Model    string `json:"model"`
				Previous string `json:"previous_response_id"`
			}
			if !assert.NoError(t, common.Unmarshal(raw, &event)) {
				return
			}
			received <- event.Model
			if i == 1 {
				assert.Equal(t, "response-0", event.Previous)
				_ = ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"response-0","status":"completed","usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}`))
			}
			usage := `"input_tokens":1000,"output_tokens":10,"total_tokens":1010`
			if i == 1 {
				usage = `"input_tokens":0,"output_tokens":0,"total_tokens":0`
			}
			terminal := fmt.Sprintf(`{"type":"response.completed","response":{"id":"response-%d","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{%s}}}`, i, usage)
			if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(terminal))) {
				return
			}
		}
		_, _, _ = ws.ReadMessage()
	})
	require.NoError(t, model.DB.Model(fixture.channel).Update("protocol", protocol.OpenAIResponses).Error)
	var provider model.Provider
	require.NoError(t, model.DB.First(&provider, fixture.channel.ProviderID).Error)
	provider.SetSetting(dto.ProviderSettings{ResponsesWebSocketEnabled: true})
	provider.Key = "provider-secret\nspare-secret"
	provider.ProviderInfo = model.ProviderInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModePolling}
	require.NoError(t, model.DB.Model(&provider).Select("key", "channel_info", "setting").Updates(&provider).Error)
	gateway := httptest.NewServer(fixture.engine)
	defer gateway.Close()
	client, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gateway.URL, "http")+"/v1/responses", http.Header{"Authorization": {"Bearer sk-" + fixture.token.Key}})
	if response != nil {
		defer response.Body.Close()
	}
	require.NoError(t, err, "wallet requests must not depend on retired token quota")
	defer client.Close()
	for i, name := range []string{"gateway-alias", "gateway-offering"} {
		previous := ""
		if i == 1 {
			previous = `,"previous_response_id":"response-0"`
		}
		require.NoError(t, client.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.create","model":%q,"input":"hello","max_output_tokens":16%s}`, name, previous))))
		require.NoError(t, client.SetReadDeadline(time.Now().Add(5*time.Second)))
		_, raw, err := client.ReadMessage()
		require.NoError(t, err)
		require.Contains(t, string(raw), fmt.Sprintf(`"id":"response-%d"`, i), "%s", raw)
		select {
		case modelName := <-received:
			assert.Equal(t, "upstream-model", modelName)
		case <-time.After(5 * time.Second):
			t.Fatal("Provider did not receive the logical request")
		}
		// Wait for the committed records before starting the next request.
		var records []model.GatewayRequest
		require.Eventually(t, func() bool {
			var err error
			records, _, err = model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
			if err != nil || len(records) != i+1 {
				return false
			}
			for _, record := range records {
				_, count, err := model.ListGatewayAttempts(t.Context(), record.ID, 0, 10)
				if err != nil || count != 1 || record.Status == "settlement_pending" {
					return false
				}
			}
			return true
		}, 5*time.Second, 10*time.Millisecond)
	}
	require.NoError(t, client.Close())
	select {
	case <-upstreamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream session did not stop")
	}
	assert.Equal(t, int32(1), connections.Load())
	records, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 2, total)
	for _, record := range records {
		expectedSale, expectedCost := int64(1_836_000), int64(1_020_000)
		if record.RequestedName == "gateway-offering" {
			expectedSale, expectedCost = 0, 0
		}
		assert.Equal(t, expectedSale, record.Receivable.Nanos)
		assert.Equal(t, "upstream", record.UsageSource)
		assertTextGatewayLedger(t, record, expectedCost)
		attempts, count, err := model.ListGatewayAttempts(t.Context(), record.ID, 0, 10)
		require.NoError(t, err)
		require.EqualValues(t, 1, count)
		assert.Equal(t, fixture.channel.ID, attempts[0].ChannelID)
		assert.Equal(t, expectedCost, attempts[0].Cost.Nanos)
		response := adminSecurityRequest(fixture.engine, fixture.adminPAT, http.MethodGet, "/api/admin/v1/requests/"+record.ID+"/attempts", "")
		assert.Equal(t, http.StatusOK, response.Code)
		assert.NotContains(t, response.Body.String(), "provider-secret")
		assert.NotContains(t, response.Body.String(), fixture.token.Key)
	}
	wallet, err := model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	assert.Equal(t, int64(998_164_000), wallet.Balance.Nanos)
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

func TestTextGatewayFailoverCompletesWhileDisableNoticeIsPending(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"Available Provider"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	})
	noticeStarted, releaseNotice, noticeFinished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(noticeStarted)
		<-releaseNotice
		w.WriteHeader(http.StatusNoContent)
		close(noticeFinished)
	}))
	defer server.Close()
	defer releaseOnce.Do(func() { close(releaseNotice) })
	previousDisable, previousNotifyLimit := common.AutomaticDisableChannelEnabled, constant.NotifyLimitCount
	common.AutomaticDisableChannelEnabled, constant.NotifyLimitCount = true, 100
	fetch := system_setting.GetFetchSetting()
	previousFetch, previousWorker := *fetch, system_setting.WorkerUrl
	fetch.EnableSSRFProtection, system_setting.WorkerUrl = false, ""
	t.Cleanup(func() {
		common.AutomaticDisableChannelEnabled, constant.NotifyLimitCount = previousDisable, previousNotifyLimit
		*fetch, system_setting.WorkerUrl = previousFetch, previousWorker
	})
	require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("auto_ban", 1).Error)
	settings, err := common.Marshal(dto.UserSetting{NotifyType: dto.NotifyTypeWebhook, WebhookUrl: server.URL, ChannelNotifyEnabled: common.GetPointer(true)})
	require.NoError(t, err)
	require.NoError(t, model.DB.Model(&model.User{}).Where("username = ?", "security-admin").Update("setting", string(settings)).Error)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
	}()
	select {
	case <-noticeStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("automatic disable did not dispatch its notification")
	}
	select {
	case response := <-done:
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), "Available Provider")
	case <-time.After(5 * time.Second):
		releaseOnce.Do(func() { close(releaseNotice) })
		<-done
		t.Fatal("notification delivery blocked failover")
	}
	releaseOnce.Do(func() { close(releaseNotice) })
	select {
	case <-noticeFinished:
	case <-time.After(10 * time.Second):
		t.Fatal("automatic disable notification did not finish")
	}
}

func TestTextGatewayAutomaticallyDisablesSelectedProviderKey(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rotate bool
		single bool
		quota  bool
	}{
		{"unchanged", false, false, false},
		{"rotated", true, false, false},
		{"removed slot", false, true, false},
		{"account quota", false, false, true},
		{"account quota rotated", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) { testGatewayAutomaticDisable(t, tc.rotate, tc.single, tc.quota) })
	}
}

func testGatewayAutomaticDisable(t *testing.T, rotated, single, quota bool) {
	var selectedProviderID int
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if rotated {
			if err := model.DB.Model(&model.Provider{}).Where("id = ?", selectedProviderID).Update("key", "replacement-secret").Error; err != nil {
				t.Error(err)
			}
		}
		if single {
			if err := model.DB.Model(&model.Provider{}).Where("id = ?", selectedProviderID).Updates(map[string]any{
				"key": strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "channel_info": model.ProviderInfo{},
			}).Error; err != nil {
				t.Error(err)
			}
		}
		if quota {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"quota exhausted","type":"insufficient_quota","code":"insufficient_quota"}}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	previous := common.AutomaticDisableChannelEnabled
	common.AutomaticDisableChannelEnabled = true
	t.Cleanup(func() { common.AutomaticDisableChannelEnabled = previous })
	// Duplicate secret values still identify distinct configured Key slots.
	provider := &model.Provider{Name: "Selected Provider", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
		Key: "slot-secret\nslot-secret", AutoBan: common.GetPointer(1), ProviderInfo: model.ProviderInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModePolling, MultiKeyPollingIndex: 1}}
	original, err := model.GetProviderById(fixture.channel.ProviderID, true)
	require.NoError(t, err)
	provider.BaseURL = original.BaseURL
	require.NoError(t, model.DB.Create(provider).Error)
	selectedProviderID = provider.Id
	require.NotEqual(t, fixture.channel.ID, provider.Id)
	require.NoError(t, model.DB.Model(fixture.channel).Update("provider_id", provider.Id).Error)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`))
	request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
	request.Header.Set("Content-Type", "application/json")
	fixture.engine.ServeHTTP(response, request)
	stored, err := model.GetProviderById(provider.Id, true)
	require.NoError(t, err)
	if quota && !rotated && !single {
		assert.Equal(t, common.ChannelStatusAutoDisabled, stored.Status)
	} else {
		assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
	}
	if rotated || single || quota {
		assert.Empty(t, stored.ProviderInfo.MultiKeyStatusList)
	} else {
		assert.Equal(t, common.ChannelStatusAutoDisabled, stored.ProviderInfo.MultiKeyStatusList[1])
	}
	assert.Zero(t, stored.ProviderInfo.MultiKeyStatusList[0])
	untouched, err := model.GetProviderById(original.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusEnabled, untouched.Status)
	logs, _, err := model.GetAuditLogs(model.AuditLogFilter{Category: model.AuditCategorySecurity}, 0, 100, common.RoleRootUser)
	require.NoError(t, err)
	encoded, err := common.Marshal(logs)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), "provider.auto_disable")
	assert.NotContains(t, string(encoded), "slot-secret")
	assert.NotContains(t, string(encoded), "replacement-secret")
	found := false
	for _, entry := range logs {
		if entry.Action == "provider.auto_disable" {
			found = true
			assert.Equal(t, !rotated && !single, entry.Success, "audit reports the actual Key transition outcome")
			rootInfo, err := common.Marshal(entry.Other.RootInfo)
			require.NoError(t, err)
			expectedScope := "key"
			if quota {
				expectedScope = "provider"
				assert.Contains(t, string(rootInfo), `"error_class":"account_quota"`)
			}
			assert.Contains(t, string(rootInfo), `"target_scope":"`+expectedScope+`"`)
		}
	}
	assert.True(t, found)
}

func TestTextGatewayReplacementKeySurvivesConcurrentAutoDisable(t *testing.T) {
	for _, cached := range []bool{false, true} {
		for _, quota := range []bool{false, true} {
			t.Run(fmt.Sprint("cache=", cached, "/quota=", quota), func(t *testing.T) {
				testGatewayReplacementKeySurvivesConcurrentAutoDisable(t, cached, quota)
			})
		}
	}
}

func testGatewayReplacementKeySurvivesConcurrentAutoDisable(t *testing.T, cached, quota bool) {
	arrived := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	var calls atomic.Int32
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if call := calls.Add(1); call > 1 {
			expectedKey := "spare-test-key"
			if call == 3 {
				expectedKey = "replacement-test-key"
			}
			assert.Equal(t, "Bearer "+expectedKey, r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"replacement","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
			return
		}
		assert.Equal(t, "Bearer old-test-key", r.Header.Get("Authorization"))
		close(arrived)
		select {
		case <-release:
			if quota {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":{"message":"quota exhausted","type":"insufficient_quota","code":"insufficient_quota"}}`)
			} else {
				w.WriteHeader(http.StatusUnauthorized)
			}
		case <-r.Context().Done():
		}
	})
	previous := common.AutomaticDisableChannelEnabled
	common.AutomaticDisableChannelEnabled = true
	t.Cleanup(func() { common.AutomaticDisableChannelEnabled = previous })
	provider, err := model.GetProviderById(fixture.channel.ProviderID, true)
	require.NoError(t, err)
	provider.Key = "old-test-key\nspare-test-key"
	provider.AutoBan = common.GetPointer(1)
	provider.ProviderInfo = model.ProviderInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModePolling}
	require.NoError(t, model.DB.Model(provider).Select("key", "auto_ban", "channel_info").Updates(provider).Error)
	common.MemoryCacheEnabled = cached
	model.CacheUpdateProvider(provider)
	model.InitProviderCache()
	var admin model.User
	require.NoError(t, model.DB.Where("username = ?", "security-admin").First(&admin).Error)
	require.NoError(t, model.DB.Model(&admin).Update("role", common.RoleRootUser).Error)
	adminPAT := createWalletTestAccessToken(t, admin.Id, "channel:read", "channel:write", "channel:sensitive_write", "channel:operate")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	gatewayDone := make(chan *httptest.ResponseRecorder, 1)
	defer func() {
		cancel()
		releaseOnce.Do(func() { close(release) })
		select {
		case <-gatewayDone:
		case <-time.After(5 * time.Second):
		}
	}()
	go func() {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)).WithContext(ctx)
		request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		fixture.engine.ServeHTTP(response, request)
		gatewayDone <- response
	}()
	select {
	case <-arrived:
	case <-ctx.Done():
		t.Fatal("gateway did not reach Provider")
	}
	lock := model.DB.WithContext(ctx).Begin()
	require.NoError(t, lock.Error)
	defer lock.Rollback()
	defer releaseOnce.Do(func() { close(release) })
	var lockedID, lockerPID int
	require.NoError(t, lock.Raw(`SELECT "id" FROM "providers" WHERE "id" = ? FOR UPDATE`, provider.Id).Scan(&lockedID).Error)
	require.Equal(t, provider.Id, lockedID)
	require.NoError(t, lock.Raw("SELECT pg_backend_pid()").Scan(&lockerPID).Error)
	adminDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		request := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/v1/providers/%d", provider.Id), strings.NewReader(`{"key":"replacement-test-key\nspare-test-key","key_mode":"replace"}`)).WithContext(ctx)
		request.Header.Set("Authorization", "Bearer "+adminPAT)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		fixture.engine.ServeHTTP(response, request)
		adminDone <- response
	}()
	// PostgreSQL lock waits establish order through the public management and
	// gateway requests. No production callback or wall-clock delay is involved.
	var adminPID int
	for adminPID == 0 && ctx.Err() == nil {
		select {
		case response := <-adminDone:
			t.Fatalf("management request completed before locking: HTTP %d %s", response.Code, response.Body.String())
		default:
		}
		require.NoError(t, model.DB.WithContext(ctx).Raw(`SELECT COALESCE(min(pid), 0) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND ? = ANY(pg_blocking_pids(pid))`, lockerPID).Scan(&adminPID).Error)
	}
	require.NotZero(t, adminPID, "management replacement must wait behind the external row lock")
	releaseOnce.Do(func() { close(release) })
	var statusPID int
	for statusPID == 0 && ctx.Err() == nil {
		require.NoError(t, model.DB.WithContext(ctx).Raw(`SELECT COALESCE(min(pid), 0) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> ? AND (? = ANY(pg_blocking_pids(pid)) OR ? = ANY(pg_blocking_pids(pid)))`, adminPID, lockerPID, adminPID).Scan(&statusPID).Error)
	}
	require.NotZero(t, statusPID, "automatic disable must wait behind the management replacement")
	require.NoError(t, lock.Commit().Error)
	select {
	case response := <-adminDone:
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.Contains(t, response.Body.String(), `"success":true`)
	case <-ctx.Done():
		t.Fatal("management replacement did not complete")
	}
	select {
	case response := <-gatewayDone:
		gatewayDone <- response
	case <-ctx.Done():
		t.Fatal("gateway did not complete")
	}
	keys := adminSecurityRequest(fixture.engine, adminPAT, http.MethodGet, fmt.Sprintf("/api/admin/v1/providers/%d/keys", provider.Id), "")
	require.Equal(t, http.StatusOK, keys.Code)
	assert.Contains(t, keys.Body.String(), `"auto_disabled_count":0`, "a failure for the old Key must not disable its replacement")
	read := adminSecurityRequest(fixture.engine, adminPAT, http.MethodGet, fmt.Sprintf("/api/admin/v1/providers/%d", provider.Id), "")
	require.Equal(t, http.StatusOK, read.Code)
	assert.Contains(t, read.Body.String(), `"status":1`, "an old account quota failure must not disable the replacement Provider")
	logs, _, err := model.GetAuditLogs(model.AuditLogFilter{Category: model.AuditCategorySecurity}, 0, 100, common.RoleRootUser)
	require.NoError(t, err)
	found := false
	for _, entry := range logs {
		if entry.Action == "provider.auto_disable" {
			found = true
			assert.False(t, entry.Success, "replacement committed before the status write; old Key transition cannot claim success")
		}
	}
	assert.True(t, found)
	// The cursor already advanced past slot zero for the failed request.
	// One full polling cycle must still visit the replacement.
	for range 2 {
		next := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
		assert.Equal(t, http.StatusOK, next.Code, next.Body.String())
	}
	assert.EqualValues(t, 3, calls.Load(), "replacement must remain selectable, including from the cache")
}

func TestTextGatewayFailedAutoDisablePreservesCachedProvider(t *testing.T) {
	for _, multi := range []bool{false, true} {
		t.Run(fmt.Sprint("multi=", multi), func(t *testing.T) {
			var calls atomic.Int32
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, "Bearer provider-secret", r.Header.Get("Authorization"))
				w.WriteHeader(http.StatusUnauthorized)
			})
			previous := common.AutomaticDisableChannelEnabled
			common.AutomaticDisableChannelEnabled = true
			t.Cleanup(func() { common.AutomaticDisableChannelEnabled = previous })
			provider, err := model.GetProviderById(fixture.channel.ProviderID, true)
			require.NoError(t, err)
			provider.AutoBan = common.GetPointer(1)
			provider.ProviderInfo = model.ProviderInfo{IsMultiKey: multi, MultiKeySize: 1, MultiKeyMode: constant.MultiKeyModePolling}
			require.NoError(t, model.DB.Model(provider).Select("auto_ban", "channel_info").Updates(provider).Error)
			// Exercise rollback of both Provider and legacy capability state. The
			// database rejects the capability write after the Provider update.
			require.NoError(t, model.DB.Create(&model.Ability{ChannelId: provider.Id, Group: "default", Model: "legacy-model", Enabled: true}).Error)
			require.NoError(t, model.DB.Exec(`CREATE FUNCTION reject_health_status() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'health status write rejected'; END $$`).Error)
			require.NoError(t, model.DB.Exec(`CREATE TRIGGER reject_health_status BEFORE UPDATE ON "abilities" FOR EACH ROW EXECUTE FUNCTION reject_health_status()`).Error)
			common.MemoryCacheEnabled = true
			model.InitProviderCache()
			for range 2 {
				response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
				assert.Equal(t, http.StatusUnauthorized, response.Code, response.Body.String())
			}
			assert.EqualValues(t, 2, calls.Load(), "a rolled-back transition must not remove the cached Provider or its only Key")
			var admin model.User
			require.NoError(t, model.DB.Where("username = ?", "security-admin").First(&admin).Error)
			adminPAT := createWalletTestAccessToken(t, admin.Id, "channel:operate")
			manual := adminSecurityRequest(fixture.engine, adminPAT, http.MethodPost, fmt.Sprintf("/api/admin/v1/providers/%d/disable", provider.Id), "")
			assert.Equal(t, http.StatusInternalServerError, manual.Code, manual.Body.String())
			assert.Contains(t, manual.Body.String(), `"success":false`)
			batch := adminSecurityRequest(fixture.engine, adminPAT, http.MethodPost, "/api/channel/status/batch", fmt.Sprintf(`{"ids":[%d],"status":2}`, provider.Id))
			assert.Equal(t, http.StatusInternalServerError, batch.Code, batch.Body.String())
			assert.Contains(t, batch.Body.String(), `"success":false`)
			operations, _, err := model.GetAuditLogs(model.AuditLogFilter{Category: model.AuditCategoryOperation}, 0, 100, common.RoleRootUser)
			require.NoError(t, err)
			foundManual, foundBatch := false, false
			for _, entry := range operations {
				if strings.Contains(entry.Content, "/providers/:id/disable") {
					foundManual = true
					assert.False(t, entry.Success)
					assert.Equal(t, http.StatusInternalServerError, entry.Status)
				}
				if strings.Contains(entry.Content, "/api/channel/status/batch") {
					foundBatch = true
					assert.False(t, entry.Success)
					assert.Equal(t, http.StatusInternalServerError, entry.Status)
				}
			}
			assert.True(t, foundManual)
			assert.True(t, foundBatch)
			stored, err := model.GetProviderById(provider.Id, true)
			require.NoError(t, err)
			assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
			assert.Empty(t, stored.ProviderInfo.MultiKeyStatusList)
			var ability model.Ability
			require.NoError(t, model.DB.Where("channel_id = ?", provider.Id).First(&ability).Error)
			assert.True(t, ability.Enabled)
			logs, _, err := model.GetAuditLogs(model.AuditLogFilter{Category: model.AuditCategorySecurity}, 0, 100, common.RoleRootUser)
			require.NoError(t, err)
			attempts := 0
			for _, entry := range logs {
				if entry.Action == "provider.auto_disable" {
					attempts++
					assert.False(t, entry.Success)
				}
			}
			assert.Equal(t, 2, attempts)
			encoded, err := common.Marshal(logs)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), provider.Key)
		})
	}
}

func TestTextGatewayManualDisableWinsOverOldFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		multi bool
		key   bool
	}{
		{"single Provider", false, false},
		{"multi Provider", true, false},
		{"multi Key", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var engine *gin.Engine
			var adminPAT, path string
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				response := adminSecurityRequest(engine, adminPAT, http.MethodPost, path, "")
				assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
				assert.Contains(t, response.Body.String(), `"success":true`)
				w.WriteHeader(http.StatusUnauthorized)
			})
			engine = fixture.engine
			previous := common.AutomaticDisableChannelEnabled
			common.AutomaticDisableChannelEnabled = true
			t.Cleanup(func() { common.AutomaticDisableChannelEnabled = previous })
			provider, err := model.GetProviderById(fixture.channel.ProviderID, true)
			require.NoError(t, err)
			provider.AutoBan = common.GetPointer(1)
			provider.ProviderInfo = model.ProviderInfo{IsMultiKey: tc.multi, MultiKeySize: 1, MultiKeyMode: constant.MultiKeyModePolling}
			require.NoError(t, model.DB.Model(provider).Select("auto_ban", "channel_info").Updates(provider).Error)
			var admin model.User
			require.NoError(t, model.DB.Where("username = ?", "security-admin").First(&admin).Error)
			adminPAT = createWalletTestAccessToken(t, admin.Id, "channel:read", "channel:operate")
			path = fmt.Sprintf("/api/admin/v1/providers/%d/disable", provider.Id)
			if tc.key {
				path = fmt.Sprintf("/api/admin/v1/providers/%d/keys/0/disable", provider.Id)
			}
			common.MemoryCacheEnabled = true
			model.InitProviderCache()
			response := adminSecurityRequest(engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
			assert.Equal(t, http.StatusUnauthorized, response.Code, response.Body.String())
			if tc.key {
				keys := adminSecurityRequest(engine, adminPAT, http.MethodGet, fmt.Sprintf("/api/admin/v1/providers/%d/keys", provider.Id), "")
				assert.Contains(t, keys.Body.String(), `"manual_disabled_count":1`)
				assert.Contains(t, keys.Body.String(), `"auto_disabled_count":0`)
			} else {
				read := adminSecurityRequest(engine, adminPAT, http.MethodGet, fmt.Sprintf("/api/admin/v1/providers/%d", provider.Id), "")
				assert.Contains(t, read.Body.String(), `"status":2`, "manual Provider disable must remain manual")
				repeat := adminSecurityRequest(engine, adminPAT, http.MethodPost, path, "")
				assert.Equal(t, http.StatusOK, repeat.Code, repeat.Body.String())
				assert.Contains(t, repeat.Body.String(), `"success":true`, "an idempotent manual operation remains successful")
			}
			logs, _, err := model.GetAuditLogs(model.AuditLogFilter{Category: model.AuditCategorySecurity}, 0, 100, common.RoleRootUser)
			require.NoError(t, err)
			found := false
			for _, entry := range logs {
				if entry.Action == "provider.auto_disable" {
					found = true
					assert.False(t, entry.Success)
				}
			}
			assert.True(t, found)
		})
	}
}

func TestTextGatewayAutoDisablePublishesOnlyTargetCache(t *testing.T) {
	var calls atomic.Int32
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if call == 1 {
			assert.Equal(t, "Bearer provider-secret", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		expected := "spare-secret"
		if call == 3 {
			expected = "unrelated-secret"
		}
		assert.Equal(t, "Bearer "+expected, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"cached","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	})
	previous := common.AutomaticDisableChannelEnabled
	common.AutomaticDisableChannelEnabled = true
	t.Cleanup(func() { common.AutomaticDisableChannelEnabled = previous })
	provider, err := model.GetProviderById(fixture.channel.ProviderID, true)
	require.NoError(t, err)
	provider.Key = "provider-secret\nspare-secret"
	provider.AutoBan = common.GetPointer(1)
	provider.ProviderInfo = model.ProviderInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModePolling}
	require.NoError(t, model.DB.Model(provider).Select("key", "auto_ban", "channel_info").Updates(provider).Error)
	unrelated := &model.Provider{Name: "Unrelated cached Provider", Key: "unrelated-secret", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, BaseURL: provider.BaseURL, Models: "legacy-model", Group: "default"}
	require.NoError(t, model.DB.Create(unrelated).Error)
	require.NoError(t, model.DB.Create(&model.Ability{ChannelId: unrelated.Id, Group: "default", Model: "legacy-model", Enabled: true}).Error)
	common.MemoryCacheEnabled = true
	model.InitProviderCache()
	// A database fault makes a global capability reload fail immediately after
	// the key transition commits. Only this test's isolated schema is changed.
	require.NoError(t, model.DB.Exec(`CREATE FUNCTION hide_health_abilities() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN ALTER TABLE "abilities" RENAME TO "health_abilities_unavailable"; RETURN NEW; END $$`).Error)
	require.NoError(t, model.DB.Exec(`CREATE TRIGGER hide_health_abilities AFTER UPDATE OF "channel_info" ON "providers" FOR EACH ROW EXECUTE FUNCTION hide_health_abilities()`).Error)
	require.NotPanics(t, func() {
		response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
		assert.Equal(t, http.StatusUnauthorized, response.Code, response.Body.String())
	})
	require.NoError(t, model.DB.Exec(`DROP TRIGGER hide_health_abilities ON "providers"`).Error)
	require.NoError(t, model.DB.Exec(`ALTER TABLE "health_abilities_unavailable" RENAME TO "abilities"`).Error)
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
	assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.NoError(t, model.DB.Model(fixture.channel).Update("provider_id", unrelated.Id).Error)
	response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
	assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.EqualValues(t, 3, calls.Load(), "target's spare Key and unrelated Provider must remain selectable")
}

func TestTextGatewayManualDisableRepairsStaleCache(t *testing.T) {
	for _, alreadyDisabled := range []bool{false, true} {
		t.Run(fmt.Sprint("already disabled=", alreadyDisabled), func(t *testing.T) {
			var calls atomic.Int32
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"stale","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
			})
			provider, err := model.GetProviderById(fixture.channel.ProviderID, true)
			require.NoError(t, err)
			provider.Key = "old-test-key\nspare-test-key"
			provider.ProviderInfo = model.ProviderInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModePolling}
			require.NoError(t, model.DB.Model(provider).Select("key", "channel_info").Updates(provider).Error)
			common.MemoryCacheEnabled = true
			model.InitProviderCache()
			// Model another node's committed state without refreshing this node.
			provider.ProviderInfo.MultiKeyStatusList = map[int]int{0: common.ChannelStatusAutoDisabled}
			if alreadyDisabled {
				provider.Status = common.ChannelStatusManuallyDisabled
			}
			require.NoError(t, model.DB.Model(provider).Select("status", "channel_info").Updates(provider).Error)
			var admin model.User
			require.NoError(t, model.DB.Where("username = ?", "security-admin").First(&admin).Error)
			adminPAT := createWalletTestAccessToken(t, admin.Id, "channel:operate")
			response := adminSecurityRequest(fixture.engine, adminPAT, http.MethodPost, fmt.Sprintf("/api/admin/v1/providers/%d/disable", provider.Id), "")
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), `"success":true`)
			response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
			assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
			assert.Zero(t, calls.Load(), "successful manual disable must immediately stop dispatch from a stale cache")
		})
	}
}

func TestTextGatewayRejectedAutoDisableRepairsStaleCache(t *testing.T) {
	for _, keyOnly := range []bool{false, true} {
		t.Run(fmt.Sprint("key=", keyOnly), func(t *testing.T) {
			var rejected, spare atomic.Int32
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer stale-selected-key" {
					rejected.Add(1)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				spare.Add(1)
				assert.Equal(t, "Bearer stale-spare-key", r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"spare","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
			})
			previous := common.AutomaticDisableChannelEnabled
			common.AutomaticDisableChannelEnabled = true
			t.Cleanup(func() { common.AutomaticDisableChannelEnabled = previous })
			provider, err := model.GetProviderById(fixture.channel.ProviderID, true)
			require.NoError(t, err)
			provider.Key = "stale-selected-key"
			provider.AutoBan = common.GetPointer(1)
			if keyOnly {
				provider.Key += "\nstale-spare-key"
				provider.ProviderInfo = model.ProviderInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModePolling}
			}
			require.NoError(t, model.DB.Model(provider).Select("key", "auto_ban", "channel_info").Updates(provider).Error)
			var admin model.User
			require.NoError(t, model.DB.Where("username = ?", "security-admin").First(&admin).Error)
			adminPAT := createWalletTestAccessToken(t, admin.Id, "channel:read", "channel:operate")
			common.MemoryCacheEnabled = true
			model.InitProviderCache()
			model.CacheUpdateProvider(provider)
			// A management node without a memory cache commits the manual state;
			// this gateway node retains its earlier enabled cache.
			common.MemoryCacheEnabled = false
			path := fmt.Sprintf("/api/admin/v1/providers/%d/disable", provider.Id)
			if keyOnly {
				path = fmt.Sprintf("/api/admin/v1/providers/%d/keys/0/disable", provider.Id)
			}
			manual := adminSecurityRequest(fixture.engine, adminPAT, http.MethodPost, path, "")
			require.Equal(t, http.StatusOK, manual.Code, manual.Body.String())
			require.Contains(t, manual.Body.String(), `"success":true`)
			common.MemoryCacheEnabled = true
			_ = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
			require.EqualValues(t, 1, rejected.Load(), "the first call must exercise the stale gateway cache")
			for range 2 {
				next := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
				expected := http.StatusServiceUnavailable
				if keyOnly {
					expected = http.StatusOK
				}
				assert.Equal(t, expected, next.Code, next.Body.String())
			}
			assert.EqualValues(t, 1, rejected.Load(), "observing committed manual state must stop further dispatch to the disabled credential")
			if keyOnly {
				assert.EqualValues(t, 2, spare.Load())
			}
		})
	}
}

func TestTextGatewayManagementEditPreservesCommittedKeyDisable(t *testing.T) {
	var rejected, spare atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer edited-selected-key" {
			if rejected.Add(1) == 1 {
				close(entered)
				<-release
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		spare.Add(1)
		assert.Equal(t, "Bearer edited-spare-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"edited","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	})
	previous := common.AutomaticDisableChannelEnabled
	common.AutomaticDisableChannelEnabled = true
	t.Cleanup(func() { common.AutomaticDisableChannelEnabled = previous })
	provider, err := model.GetProviderById(fixture.channel.ProviderID, true)
	require.NoError(t, err)
	provider.Key = "edited-selected-key\nedited-spare-key"
	provider.AutoBan = common.GetPointer(1)
	provider.ProviderInfo = model.ProviderInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModePolling}
	require.NoError(t, model.DB.Model(provider).Select("key", "auto_ban", "channel_info").Updates(provider).Error)
	var admin model.User
	require.NoError(t, model.DB.Where("username = ?", "security-admin").First(&admin).Error)
	adminPAT := createWalletTestAccessToken(t, admin.Id, "channel:read", "channel:write", "channel:operate")
	common.MemoryCacheEnabled = true
	model.InitProviderCache()
	model.CacheUpdateProvider(provider)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	gatewayDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)).WithContext(ctx)
		request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		fixture.engine.ServeHTTP(response, request)
		gatewayDone <- response
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("gateway did not reach Provider")
	}
	lock := model.DB.WithContext(ctx).Begin()
	require.NoError(t, lock.Error)
	defer lock.Rollback()
	var lockedID, lockerPID int
	require.NoError(t, lock.Raw(`SELECT "id" FROM "providers" WHERE "id" = ? FOR UPDATE`, provider.Id).Scan(&lockedID).Error)
	require.NoError(t, lock.Raw("SELECT pg_backend_pid()").Scan(&lockerPID).Error)
	releaseOnce.Do(func() { close(release) })
	var statusPID int
	for statusPID == 0 && ctx.Err() == nil {
		require.NoError(t, model.DB.WithContext(ctx).Raw(`SELECT COALESCE(min(pid), 0) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND ? = ANY(pg_blocking_pids(pid)) AND query LIKE 'UPDATE "providers"%'`, lockerPID).Scan(&statusPID).Error)
	}
	require.NotZero(t, statusPID, "automatic status update must enter the PostgreSQL lock queue first")
	adminDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		request := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/v1/providers/%d", provider.Id), strings.NewReader(`{"name":"Edited after selected failure"}`)).WithContext(ctx)
		request.Header.Set("Authorization", "Bearer "+adminPAT)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		fixture.engine.ServeHTTP(response, request)
		adminDone <- response
	}()
	var adminPID int
	for adminPID == 0 && ctx.Err() == nil {
		select {
		case response := <-adminDone:
			t.Fatalf("management edit completed before locking: HTTP %d %s", response.Code, response.Body.String())
		default:
		}
		require.NoError(t, model.DB.WithContext(ctx).Raw(`SELECT COALESCE(min(pid), 0) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> ? AND (? = ANY(pg_blocking_pids(pid)) OR ? = ANY(pg_blocking_pids(pid)))`, statusPID, lockerPID, statusPID).Scan(&adminPID).Error)
	}
	require.NotZero(t, adminPID, "management read or write must queue behind the automatic status update")
	require.NoError(t, lock.Commit().Error)
	select {
	case response := <-adminDone:
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.Contains(t, response.Body.String(), `"success":true`)
	case <-ctx.Done():
		t.Fatal("management edit did not complete")
	}
	select {
	case <-gatewayDone:
	case <-ctx.Done():
		t.Fatal("gateway did not complete")
	}
	keys := adminSecurityRequest(fixture.engine, adminPAT, http.MethodGet, fmt.Sprintf("/api/admin/v1/providers/%d/keys", provider.Id), "")
	require.Equal(t, http.StatusOK, keys.Code, keys.Body.String())
	assert.Contains(t, keys.Body.String(), `"auto_disabled_count":1`, "ordinary editing must preserve the earlier committed disable")
	read := adminSecurityRequest(fixture.engine, adminPAT, http.MethodGet, fmt.Sprintf("/api/admin/v1/providers/%d", provider.Id), "")
	assert.Contains(t, read.Body.String(), `"name":"Edited after selected failure"`)
	for range 2 {
		next := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
		assert.Equal(t, http.StatusOK, next.Code, next.Body.String())
	}
	assert.EqualValues(t, 1, rejected.Load())
	assert.EqualValues(t, 2, spare.Load())
}

func providerSecurityRequest(t *testing.T, engine http.Handler, session *service.AuthBundle, operation service.VerificationOperation, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	identity, err := service.ParseAccessToken(session.AccessToken)
	require.NoError(t, err)
	binding, err := service.BindVerificationOperation(operation)
	require.NoError(t, err)
	proof, _, err := service.IssueSecurityProof(identity, service.VerificationMethodPassword, binding)
	require.NoError(t, err)
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+session.AccessToken)
	request.Header.Set("X-Security-Proof", proof)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	return response
}

func TestProviderSensitiveEditRechecksPermissionAfterLock(t *testing.T) {
	for _, operation := range []string{"replace", "delete", "name", "disable", "provider", "batch", "tag"} {
		for _, lockTable := range []string{"providers", "abilities"} {
			for _, changeKind := range []string{"policy", "remote_policy", "pat_revoke", "pat_expire", "pat_scope", "user_disable", "user_demote", "session_revoke", "session_expire"} {
				if changeKind == "user_demote" {
					if operation != "name" || lockTable != "providers" {
						continue
					}
				} else if changeKind != "policy" && (operation != "replace" || lockTable != "providers") {
					continue
				}
				t.Run(operation+"/"+lockTable+"/"+changeKind, func(t *testing.T) {
					fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected upstream call") })
					provider, err := model.GetProviderById(fixture.channel.ProviderID, true)
					require.NoError(t, err)
					provider.Key = "permission-first-key\npermission-spare-key"
					provider.ProviderInfo = model.ProviderInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModePolling}
					provider.Tag = common.GetPointer("permission-lock-tag")
					require.NoError(t, model.DB.Model(provider).Select("key", "channel_info", "tag").Updates(provider).Error)
					require.NoError(t, model.DB.Create(&model.Ability{ChannelId: provider.Id, Group: "default", Model: "permission-lock-model", Enabled: true}).Error)
					var following *model.Provider
					if operation == "batch" {
						following = &model.Provider{Name: "Following Provider", Key: "following-permission-key", Status: common.ChannelStatusEnabled}
						require.NoError(t, model.DB.Create(following).Error)
					}
					admin := &model.User{Username: "locked-sensitive-editor", Role: common.RoleAdminUser, Status: common.UserStatusEnabled, Group: "default", AffCode: "lockedit"}
					require.NoError(t, model.DB.Create(admin).Error)
					permission := authz.ChannelSensitiveWrite
					if operation == "name" {
						permission = authz.ChannelWrite
					} else if operation == "disable" || operation == "provider" || operation == "batch" || operation == "tag" {
						permission = authz.ChannelOperate
					}
					require.NoError(t, authz.SetUserPermissions(admin.Id, authz.PermissionsMap{permission.Resource: {permission.Action: true}}))
					require.True(t, authz.Can(admin.Id, admin.Role, permission))
					pat := createWalletTestAccessToken(t, admin.Id, "channel:read", "channel:write", "channel:sensitive_write", "channel:operate")
					credential := pat
					var control *service.AuthBundle
					var personal model.UserAccessToken
					if changeKind != "policy" && changeKind != "remote_policy" {
						require.NoError(t, model.DB.AutoMigrate(&model.AuthFlow{}, &model.TwoFA{}, &model.PasskeyCredential{}, &model.UserOAuthBinding{}))
						previousPasswordLogin := common.PasswordLoginEnabled
						common.PasswordLoginEnabled = true
						t.Cleanup(func() { common.PasswordLoginEnabled = previousPasswordLogin })
						hash, err := common.Password2Hash("provider-test-password")
						require.NoError(t, err)
						admin.Password = hash
						require.NoError(t, model.DB.Model(admin).Select("Password").Updates(&model.User{Password: hash}).Error)
						control, err = service.CreateLoginSession(admin.Id, service.VerificationMethodPassword, "127.0.0.1", "provider-regression")
						require.NoError(t, err)
						require.NoError(t, model.DB.Where("token_hash = ?", model.AccessTokenFingerprint(pat)).First(&personal).Error)
						if strings.HasPrefix(changeKind, "session_") {
							credential = control.AccessToken
						}
					}
					ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
					defer cancel()
					lock := model.DB.WithContext(ctx).Begin()
					require.NoError(t, lock.Error)
					defer lock.Rollback()
					var lockedID, lockerPID int
					if lockTable == "providers" {
						require.NoError(t, lock.Raw(`SELECT "id" FROM "providers" WHERE "id" = ? FOR UPDATE`, provider.Id).Scan(&lockedID).Error)
					} else {
						require.NoError(t, lock.Raw(`SELECT "channel_id" FROM "abilities" WHERE "channel_id" = ? FOR UPDATE`, provider.Id).Scan(&lockedID).Error)
					}
					require.NoError(t, lock.Raw("SELECT pg_backend_pid()").Scan(&lockerPID).Error)
					done := make(chan *httptest.ResponseRecorder, 1)
					go func() {
						method, path, body := http.MethodPatch, fmt.Sprintf("/api/admin/v1/providers/%d", provider.Id), `{"key":"permission-replacement-key\npermission-spare-key","key_mode":"replace"}`
						if operation == "delete" {
							method, path, body = http.MethodDelete, fmt.Sprintf("/api/admin/v1/providers/%d/keys/0", provider.Id), ""
						} else if operation == "name" {
							body = `{"name":"Revoked edited Provider"}`
						} else if operation == "disable" {
							method, path, body = http.MethodPost, fmt.Sprintf("/api/admin/v1/providers/%d/keys/0/disable", provider.Id), ""
						} else if operation == "provider" {
							method, path, body = http.MethodPost, fmt.Sprintf("/api/admin/v1/providers/%d/disable", provider.Id), ""
						} else if operation == "batch" {
							method, path, body = http.MethodPost, "/api/channel/status/batch", fmt.Sprintf(`{"ids":[%d,%d],"status":2}`, provider.Id, following.Id)
						} else if operation == "tag" {
							method, path, body = http.MethodPost, "/api/channel/tag/disabled", `{"tag":"permission-lock-tag"}`
						}
						request := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
						request.Header.Set("Authorization", "Bearer "+credential)
						request.Header.Set("Content-Type", "application/json")
						response := httptest.NewRecorder()
						fixture.engine.ServeHTTP(response, request)
						done <- response
					}()
					var waitingPID int
					for waitingPID == 0 && ctx.Err() == nil {
						select {
						case response := <-done:
							t.Fatalf("sensitive request completed before locking: HTTP %d %s", response.Code, response.Body.String())
						default:
						}
						require.NoError(t, model.DB.WithContext(ctx).Raw(`SELECT COALESCE(min(pid), 0) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND ? = ANY(pg_blocking_pids(pid))`, lockerPID).Scan(&waitingPID).Error)
					}
					require.NotZero(t, waitingPID)
					switch changeKind {
					case "policy":
						require.NoError(t, authz.SetUserPermissions(admin.Id, authz.PermissionsMap{permission.Resource: {permission.Action: false}}))
						require.NoError(t, authz.ReloadPolicy())
						require.False(t, authz.Can(admin.Id, admin.Role, permission))
					case "remote_policy":
						require.NoError(t, model.DB.Transaction(func(tx *gorm.DB) error {
							return authz.SetUserPermissionsInTx(tx, admin.Id, authz.PermissionsMap{permission.Resource: {permission.Action: false}})
						}))
						require.True(t, authz.Can(admin.Id, admin.Role, permission), "the serving node must still have its old policy snapshot")
					case "pat_revoke", "pat_scope":
						var operation service.VerificationOperation
						method, body := http.MethodDelete, ""
						if changeKind == "pat_revoke" {
							context, err := common.Marshal(service.AccessTokenRevokeContext{TokenID: personal.Id})
							require.NoError(t, err)
							operation = service.VerificationOperation{Scope: service.VerificationScopeAccessTokenRevoke, Context: context}
						} else {
							scopes := []string{"profile:read"}
							context, err := common.Marshal(service.AccessTokenUpdateContext{TokenID: personal.Id, Scopes: scopes})
							require.NoError(t, err)
							operation = service.VerificationOperation{Scope: service.VerificationScopeAccessTokenUpdate, Context: context}
							method, body = http.MethodPatch, `{"name":"narrowed","scopes":["profile:read"]}`
						}
						response := providerSecurityRequest(t, fixture.engine, control, operation, method, fmt.Sprintf("/api/user/access_tokens/%d", personal.Id), body)
						require.Equal(t, http.StatusOK, response.Code, response.Body.String())
						require.Contains(t, response.Body.String(), `"success":true`)
					case "pat_expire":
						require.NoError(t, model.DB.Model(&personal).Update("expires_at", time.Now().Unix()-1).Error)
					case "session_revoke":
						response := adminSecurityRequest(fixture.engine, credential, http.MethodPost, "/api/user/auth/logout", "")
						require.Equal(t, http.StatusOK, response.Code, response.Body.String())
						require.Contains(t, response.Body.String(), `"success":true`)
					case "session_expire":
						identity, err := service.ParseAccessToken(credential)
						require.NoError(t, err)
						require.NoError(t, model.DB.Model(&model.UserSession{}).Where("sid = ?", identity.SessionID).Update("expires_at", time.Now().Unix()-1).Error)
					case "user_disable", "user_demote":
						root := &model.User{Username: "provider-authority-root", Password: admin.Password, Role: common.RoleRootUser, Status: common.UserStatusEnabled, Group: "default", AffCode: "authroot"}
						require.NoError(t, model.DB.Create(root).Error)
						rootSession, err := service.CreateLoginSession(root.Id, service.VerificationMethodPassword, "127.0.0.1", "provider-regression")
						require.NoError(t, err)
						action := strings.TrimPrefix(changeKind, "user_")
						context, err := common.Marshal(service.AdminUserManageContext{UserID: admin.Id, Action: action})
						require.NoError(t, err)
						operation := service.VerificationOperation{Scope: service.VerificationScopeAdminUserManage, Context: context}
						response := providerSecurityRequest(t, fixture.engine, rootSession, operation, http.MethodPost, "/api/user/manage", fmt.Sprintf(`{"id":%d,"action":"%s"}`, admin.Id, action))
						require.Equal(t, http.StatusOK, response.Code, response.Body.String())
						require.Contains(t, response.Body.String(), `"success":true`)
					}
					require.NoError(t, lock.Commit().Error)
					select {
					case response := <-done:
						assert.Contains(t, response.Body.String(), `"success":false`, "revoked permission must prevent the queued credential mutation")
					case <-ctx.Done():
						t.Fatal("sensitive request did not complete")
					}
					stored, err := model.GetProviderById(provider.Id, true)
					require.NoError(t, err)
					assert.Equal(t, provider.Key, stored.Key)
					assert.Equal(t, provider.Name, stored.Name)
					assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
					assert.Empty(t, stored.ProviderInfo.MultiKeyStatusList)
					assert.Equal(t, 2, stored.ProviderInfo.MultiKeySize)
					if following != nil {
						next, err := model.GetProviderById(following.Id, true)
						require.NoError(t, err)
						assert.Equal(t, common.ChannelStatusEnabled, next.Status, "revocation must stop the remaining batch targets")
					}
					logs, _, err := model.GetAuditLogs(model.AuditLogFilter{Category: model.AuditCategoryOperation}, 0, 100, common.RoleRootUser)
					require.NoError(t, err)
					found := false
					for _, entry := range logs {
						if entry.UserId == admin.Id && (strings.Contains(entry.Route, "/providers/") || strings.HasPrefix(entry.Action, "channel.")) {
							found = true
							assert.False(t, entry.Success)
						}
					}
					assert.True(t, found)
				})
			}
		}
	}
}

func TestProviderCredentialsStayOutOfDebugSQL(t *testing.T) {
	for _, operation := range []string{"status", "name", "polling", "invalid_setting", "invalid_other"} {
		t.Run(operation, func(t *testing.T) {
			const sentinel = "debug-provider-credential-sentinel"
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer "+sentinel, r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"debug","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
			})
			provider, err := model.GetProviderById(fixture.channel.ProviderID, true)
			require.NoError(t, err)
			provider.Key = sentinel
			if operation == "polling" {
				provider.Key += "\ndebug-spare-key"
				provider.ProviderInfo = model.ProviderInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModePolling}
			}
			if operation == "invalid_setting" {
				provider.Setting = common.GetPointer("{")
			} else if operation == "invalid_other" {
				provider.OtherSettings = "{"
			}
			require.NoError(t, model.DB.Model(provider).Select("key", "channel_info", "setting", "other_settings").Updates(provider).Error)
			var admin model.User
			require.NoError(t, model.DB.Where("username = ?", "security-admin").First(&admin).Error)
			adminPAT := createWalletTestAccessToken(t, admin.Id, "channel:read", "channel:write", "channel:operate")
			var output bytes.Buffer
			originalDB, originalDebug := model.DB, common.DebugEnabled
			common.DebugEnabled = true
			// Match production DEBUG: Info SQL with visible bound parameters.
			debugLogger := gormlogger.New(log.New(&output, "", 0), gormlogger.Config{LogLevel: gormlogger.Info, ParameterizedQueries: false})
			model.DB = model.DB.Session(&gorm.Session{Logger: debugLogger}).Debug()
			t.Cleanup(func() { model.DB, common.DebugEnabled = originalDB, originalDebug })
			method, path, body := http.MethodPost, fmt.Sprintf("/api/admin/v1/providers/%d/disable", provider.Id), ""
			token := adminPAT
			if operation == "name" {
				method, path, body = http.MethodPatch, fmt.Sprintf("/api/admin/v1/providers/%d", provider.Id), `{"name":"Debug-safe edited Provider"}`
			} else if operation == "invalid_setting" {
				method, path, body = http.MethodPut, "/api/channel/", fmt.Sprintf(`{"id":%d,"name":"Debug-safe edited Provider"}`, provider.Id)
			} else if operation == "polling" || operation == "invalid_other" {
				path, body, token = "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`, fixture.token.Key
			}
			var response *httptest.ResponseRecorder
			if operation == "invalid_setting" {
				done := make(chan *httptest.ResponseRecorder, 1)
				go func() { done <- adminSecurityRequest(fixture.engine, token, method, path, body) }()
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				for response == nil && ctx.Err() == nil {
					select {
					case response = <-done:
					default:
						var waitingPID int
						require.NoError(t, originalDB.WithContext(ctx).Raw(`SELECT COALESCE(min(pid), 0) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE 'UPDATE "providers"%'`).Scan(&waitingPID).Error)
						if waitingPID != 0 {
							assert.Fail(t, "reading invalid settings blocked on its own Provider transaction")
							var cancelled bool
							require.NoError(t, originalDB.Raw("SELECT pg_cancel_backend(?)", waitingPID).Scan(&cancelled).Error)
							require.True(t, cancelled)
						}
					}
				}
				require.NotNil(t, response)
			} else {
				response = adminSecurityRequest(fixture.engine, token, method, path, body)
			}
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			read := adminSecurityRequest(fixture.engine, adminPAT, http.MethodGet, fmt.Sprintf("/api/admin/v1/providers/%d", provider.Id), "")
			require.Equal(t, http.StatusOK, read.Code, read.Body.String())
			if operation == "status" {
				assert.Contains(t, read.Body.String(), `"status":2`)
			} else if operation == "name" || operation == "invalid_setting" {
				assert.Contains(t, read.Body.String(), `"name":"Debug-safe edited Provider"`)
			}
			assert.NotEmpty(t, output.String(), "ordinary diagnostics remain enabled")
			assert.NotContains(t, output.String(), sentinel, "sensitive Provider sessions must not log usable credentials")
			logs, _, err := model.GetAuditLogs(model.AuditLogFilter{}, 0, 100, common.RoleRootUser)
			require.NoError(t, err)
			encoded, err := common.Marshal(logs)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), sentinel)
		})
	}
}

func TestProviderBatchStatusFailureRecordsCommittedTargets(t *testing.T) {
	for _, operation := range []string{"batch", "tag"} {
		t.Run(operation, func(t *testing.T) {
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected Provider call") })
			firstID := fixture.channel.ProviderID
			failed := &model.Provider{Name: "Rejected status", Key: "failed-provider-secret", Status: common.ChannelStatusEnabled}
			require.NoError(t, model.DB.Create(failed).Error)
			for _, id := range []int{firstID, failed.Id} {
				require.NoError(t, model.DB.Create(&model.Ability{ChannelId: id, Group: "default", Model: "legacy-model", Enabled: true}).Error)
			}
			require.NoError(t, model.DB.Exec(fmt.Sprintf(`CREATE FUNCTION reject_health_batch_status() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW."channel_id" = %d THEN RAISE EXCEPTION 'batch status write rejected'; END IF; RETURN NEW; END $$`, failed.Id)).Error)
			require.NoError(t, model.DB.Exec(`CREATE TRIGGER reject_health_batch_status BEFORE UPDATE ON "abilities" FOR EACH ROW EXECUTE FUNCTION reject_health_batch_status()`).Error)
			var admin model.User
			require.NoError(t, model.DB.Where("username = ?", "security-admin").First(&admin).Error)
			adminPAT := createWalletTestAccessToken(t, admin.Id, "channel:read", "channel:operate")
			closed := make(chan string, 1)
			unregister := wsmanager.Register(firstID, wsmanager.KindResponses, func(reason string) { closed <- reason })
			defer unregister()
			path, body, action := "/api/channel/status/batch", fmt.Sprintf(`{"ids":[%d,%d],"status":2}`, firstID, failed.Id), "channel.status_update_batch"
			if operation == "tag" {
				require.NoError(t, model.DB.Model(&model.Provider{}).Where(`"id" IN ?`, []int{firstID, failed.Id}).Update("tag", "partial-disable-tag").Error)
				path, body, action = "/api/channel/tag/disabled", `{"tag":"partial-disable-tag"}`, "channel.tag_disable"
			}
			response := adminSecurityRequest(fixture.engine, adminPAT, http.MethodPost, path, body)
			assert.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
			if operation == "batch" {
				assert.Contains(t, response.Body.String(), `"data":1`)
			}
			select {
			case reason := <-closed:
				assert.Equal(t, service.ChannelDisabledCloseReason, reason)
			default:
				t.Error("a committed disable must close active connections even when a later target fails")
			}
			for id, expected := range map[int]int{firstID: common.ChannelStatusManuallyDisabled, failed.Id: common.ChannelStatusEnabled} {
				read := adminSecurityRequest(fixture.engine, adminPAT, http.MethodGet, fmt.Sprintf("/api/admin/v1/providers/%d", id), "")
				assert.Contains(t, read.Body.String(), fmt.Sprintf(`"status":%d`, expected))
			}
			logs, _, err := model.GetAuditLogs(model.AuditLogFilter{Category: model.AuditCategoryOperation}, 0, 100, common.RoleRootUser)
			require.NoError(t, err)
			found := false
			for _, entry := range logs {
				if entry.Action == action {
					found = true
					assert.False(t, entry.Success)
					require.NotNil(t, entry.Other.Op)
					params, err := common.Marshal(entry.Other.Op.Params)
					require.NoError(t, err)
					assert.Contains(t, string(params), fmt.Sprintf(`"committed_ids":[%d]`, firstID))
					assert.Contains(t, string(params), fmt.Sprintf(`"failed_id":%d`, failed.Id))
					assert.Contains(t, string(params), `"status":2`)
					assert.NotContains(t, string(params), failed.Key)
				}
			}
			assert.True(t, found, "partial failure must retain the actual committed and rejected targets")
		})
	}
}

func TestTextGatewayPollingSnapshotPreservesCommittedKeyDisable(t *testing.T) {
	var calls atomic.Int32
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			assert.Equal(t, "Bearer old-test-key", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		assert.Equal(t, "Bearer spare-test-key", r.Header.Get("Authorization"), "a polling snapshot must not restore the rejected Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"polling","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	})
	previous := common.AutomaticDisableChannelEnabled
	common.AutomaticDisableChannelEnabled = true
	t.Cleanup(func() { common.AutomaticDisableChannelEnabled = previous })
	provider, err := model.GetProviderById(fixture.channel.ProviderID, true)
	require.NoError(t, err)
	provider.Key = "old-test-key\nspare-test-key"
	provider.AutoBan = common.GetPointer(1)
	provider.ProviderInfo = model.ProviderInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModePolling}
	require.NoError(t, model.DB.Model(provider).Select("key", "auto_ban", "channel_info").Updates(provider).Error)
	// Another node can retain this independently loaded snapshot while the
	// gateway commits a Key disable. Exercise its public polling operation next.
	pollingSnapshot, err := model.GetProviderById(provider.Id, true)
	require.NoError(t, err)
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
	require.Equal(t, http.StatusUnauthorized, response.Code, response.Body.String())
	_, _, pollingErr := pollingSnapshot.GetNextEnabledKey()
	require.Nil(t, pollingErr)
	var admin model.User
	require.NoError(t, model.DB.Where("username = ?", "security-admin").First(&admin).Error)
	adminPAT := createWalletTestAccessToken(t, admin.Id, "channel:read", "channel:operate")
	keys := adminSecurityRequest(fixture.engine, adminPAT, http.MethodGet, fmt.Sprintf("/api/admin/v1/providers/%d/keys", provider.Id), "")
	require.Equal(t, http.StatusOK, keys.Code, keys.Body.String())
	assert.Contains(t, keys.Body.String(), `"auto_disabled_count":1`, "cursor persistence must preserve a newer committed Key status")
	response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
	assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.EqualValues(t, 2, calls.Load())
}

func TestTextGatewayManualDisableWinsOverLateCredentialRecovery(t *testing.T) {
	for _, keyOnly := range []bool{false, true} {
		t.Run(fmt.Sprint("key=", keyOnly), func(t *testing.T) {
			var calls atomic.Int32
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"late","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
			})
			provider, err := model.GetProviderById(fixture.channel.ProviderID, true)
			require.NoError(t, err)
			provider.Status = common.ChannelStatusAutoDisabled
			provider.ProviderInfo = model.ProviderInfo{IsMultiKey: keyOnly, MultiKeySize: 1, MultiKeyMode: constant.MultiKeyModePolling,
				MultiKeyStatusList: map[int]int{0: common.ChannelStatusAutoDisabled}}
			require.NoError(t, model.DB.Model(provider).Select("status", "channel_info").Updates(provider).Error)
			common.MemoryCacheEnabled = true
			model.CacheUpdateProvider(provider)
			model.InitProviderCache()
			var admin model.User
			require.NoError(t, model.DB.Where("username = ?", "security-admin").First(&admin).Error)
			adminPAT := createWalletTestAccessToken(t, admin.Id, "channel:read", "channel:operate")
			path := fmt.Sprintf("/api/admin/v1/providers/%d/disable", provider.Id)
			if keyOnly {
				path = fmt.Sprintf("/api/admin/v1/providers/%d/keys/0/disable", provider.Id)
			}
			response := adminSecurityRequest(fixture.engine, adminPAT, http.MethodPost, path, "")
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			// A previously started probe completes through the public recovery API
			// after the administrator has already committed the manual disable.
			assert.False(t, service.EnableChannel(provider.Id, provider.Key, provider.Name, 0), "a refused recovery must not be counted as a committed enable")
			if keyOnly {
				read := adminSecurityRequest(fixture.engine, adminPAT, http.MethodGet, fmt.Sprintf("/api/admin/v1/providers/%d/keys", provider.Id), "")
				assert.Contains(t, read.Body.String(), `"manual_disabled_count":1`)
			} else {
				read := adminSecurityRequest(fixture.engine, adminPAT, http.MethodGet, fmt.Sprintf("/api/admin/v1/providers/%d", provider.Id), "")
				assert.Contains(t, read.Body.String(), `"status":2`)
			}
			response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
			assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
			assert.Zero(t, calls.Load(), "late automatic recovery must not override manual intent")
		})
	}
}

func TestTextGatewaySingleKeyFailoverCapturesCurrentSlot(t *testing.T) {
	var calls atomic.Int32
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, "Bearer second-test-key", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusUnauthorized)
	})
	previous := common.AutomaticDisableChannelEnabled
	common.AutomaticDisableChannelEnabled = true
	t.Cleanup(func() { common.AutomaticDisableChannelEnabled = previous })
	first, err := model.GetProviderById(fixture.channel.ProviderID, true)
	require.NoError(t, err)
	first.Key = "first-test-key\nsecond-test-key"
	first.AutoBan = common.GetPointer(1)
	first.ProviderInfo = model.ProviderInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModePolling, MultiKeyPollingIndex: 1}
	require.NoError(t, model.DB.Model(first).Select("key", "auto_ban", "channel_info").Updates(first).Error)
	second := addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, "Bearer additional-secret", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusUnauthorized)
	})
	require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", second.ProviderID).Update("auto_ban", 1).Error)
	_ = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
	assert.EqualValues(t, 2, calls.Load())
	read := adminSecurityRequest(fixture.engine, fixture.adminPAT, http.MethodGet, fmt.Sprintf("/api/admin/v1/providers/%d", second.ProviderID), "")
	require.Equal(t, http.StatusOK, read.Code, read.Body.String())
	assert.Contains(t, read.Body.String(), `"status":3`, "the single-Key Provider must not inherit the previous Provider's slot index")
	logs, _, err := model.GetAuditLogs(model.AuditLogFilter{Category: model.AuditCategorySecurity}, 0, 100, common.RoleRootUser)
	require.NoError(t, err)
	found := false
	for _, entry := range logs {
		if entry.Action == "provider.auto_disable" {
			rootInfo, err := common.Marshal(entry.Other.RootInfo)
			require.NoError(t, err)
			if strings.Contains(string(rootInfo), fmt.Sprintf(`"provider_id":%d`, second.ProviderID)) {
				found = true
				assert.True(t, entry.Success)
				assert.Contains(t, string(rootInfo), `"key_index":0`)
				assert.Contains(t, string(rootInfo), `"target_scope":"key"`)
			}
		}
	}
	assert.True(t, found)
}

func TestChannelManagementReadsSurviveUnavailableRuntimeHealth(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("management reads must not contact the Provider")
	})
	require.NoError(t, model.DB.AutoMigrate(&model.ChannelProbeResult{}))
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	require.NoError(t, client.Close())
	service.ChannelHealth = channelhealth.New(client)
	for _, path := range []string{fmt.Sprintf("/api/admin/v1/channels/%d", fixture.channel.ID), "/api/admin/v1/channels"} {
		assert.Equal(t, http.StatusUnauthorized, adminSecurityRequest(fixture.engine, "", http.MethodGet, path, "").Code)
		assert.Equal(t, http.StatusForbidden, adminSecurityRequest(fixture.engine, fixture.userPAT, http.MethodGet, path, "").Code)
		response := adminSecurityRequest(fixture.engine, fixture.adminPAT, http.MethodGet, path, "")
		assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), `"provider_id":`)
		assert.NotContains(t, response.Body.String(), `"health":`)
		assert.NotContains(t, response.Body.String(), "provider-secret")
	}
}

func TestTextGatewayCircuitUsesProviderFailuresEvenAtAttemptLimit(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprint("redis=", shared), func(t *testing.T) { testGatewayCircuit(t, shared) })
	}
}

func testGatewayCircuit(t *testing.T, shared bool) {
	calls := 0
	recovered := false
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if recovered {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Recovered probe"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	if shared {
		server := miniredis.RunT(t)
		client := redis.NewClient(&redis.Options{Addr: server.Addr()})
		t.Cleanup(func() { require.NoError(t, client.Close()) })
		service.ChannelHealth = channelhealth.New(client)
	}
	t.Setenv("GATEWAY_MAX_ATTEMPTS", "1")
	for range 5 {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`))
		request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
		request.Header.Set("Content-Type", "application/json")
		fixture.engine.ServeHTTP(response, request)
		require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	}
	addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Healthy"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	})
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`))
	request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
	request.Header.Set("Content-Type", "application/json")
	fixture.engine.ServeHTTP(response, request)
	assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, 5, calls, "an actual Provider fault counts even when the attempt limit forbids switching")
	requests, _, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{}, 0, 100)
	require.NoError(t, err)
	opened := 0
	for _, record := range requests {
		attempts, _, err := model.ListGatewayAttempts(t.Context(), record.ID, 0, 100)
		require.NoError(t, err)
		for _, attempt := range attempts {
			if attempt.CircuitAction == "opened" {
				opened++
			}
		}
	}
	assert.Equal(t, 1, opened)
	path := fmt.Sprintf("/api/admin/v1/channels/%d", fixture.channel.ID)
	assert.Equal(t, 401, adminSecurityRequest(fixture.engine, "", http.MethodGet, path, "").Code)
	assert.Equal(t, 403, adminSecurityRequest(fixture.engine, fixture.userPAT, http.MethodGet, path, "").Code)
	details := adminSecurityRequest(fixture.engine, fixture.adminPAT, http.MethodGet, path, "")
	require.Equal(t, 200, details.Code)
	assert.Contains(t, details.Body.String(), `"health":{"circuit":"open"`)
	assert.Contains(t, details.Body.String(), `"failures":5`)
	assert.NotContains(t, details.Body.String(), "provider-secret")
	var stored model.Channel
	require.NoError(t, model.DB.First(&stored, fixture.channel.ID).Error)
	assert.Equal(t, model.ChannelEnabled, stored.Status, "circuit state is runtime only")
	recovered = true
	fixture.fake.Advance(time.Minute)
	probe := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
	require.Equal(t, 200, probe.Code, probe.Body.String())
	assert.Contains(t, probe.Body.String(), "Recovered probe", "expired circuits receive their probe despite poor recent health")
	assert.Equal(t, 6, calls)
	health, err := service.ChannelHealth.Snapshots(t.Context(), []int{fixture.channel.ID})
	require.NoError(t, err)
	assert.Equal(t, "closed", health[fixture.channel.ID].Circuit)
	assert.Zero(t, health[fixture.channel.ID].Failures)
}

func TestTextGatewayRateLimitCooldownHonorsHeader(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		delay        time.Duration
		sdk          bool
	}{
		{"long seconds", "172800", 48 * time.Hour, false}, {"long HTTP date", "date", 48 * time.Hour, false},
		{"zero", "0", 0, false}, {"elapsed HTTP date", "past", 0, false}, {"invalid sign", "+1", time.Minute, false}, {"missing", "", time.Minute, false},
		{"SDK long seconds", "172800", 48 * time.Hour, true}, {"SDK zero", "0", 0, true},
		{"SDK invalid private header", "private-sdk-header-secret", time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header := tc.header
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if header != "" {
					w.Header().Set("Retry-After", header)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"code":"rate_limit_exceeded"}}`))
			})
			if tc.header == "date" {
				header = fixture.fake.Now().Add(tc.delay).UTC().Format(http.TimeFormat)
			}
			if tc.header == "past" {
				header = fixture.fake.Now().Add(-time.Second).UTC().Format(http.TimeFormat)
			}
			if tc.sdk {
				require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Updates(map[string]any{"type": constant.ChannelTypeAws, "key": "fake|us-east-1"}).Error)
				require.NoError(t, model.DB.Model(fixture.channel).Update("upstream_model_name", "amazon.nova-lite-v1:0").Error)
				client := service.GetHttpClient()
				original := client.Transport
				t.Cleanup(func() { client.Transport = original })
				client.Transport = channelProbeRoundTripper(func(request *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 429, Header: http.Header{"Content-Type": {"application/json"}, "X-Amzn-Errortype": {"ThrottlingException"}, "Retry-After": {header}}, Body: io.NopCloser(strings.NewReader(`{"message":"rate limited"}`)), Request: request}, nil
				})
			}
			t.Setenv("GATEWAY_MAX_ATTEMPTS", "1")
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
			expectedStatus := http.StatusTooManyRequests
			if tc.sdk {
				expectedStatus = http.StatusBadGateway
			}
			require.Equal(t, expectedStatus, response.Code, response.Body.String())
			details := adminSecurityRequest(fixture.engine, fixture.adminPAT, http.MethodGet, fmt.Sprintf("/api/admin/v1/channels/%d", fixture.channel.ID), "")
			require.Equal(t, 200, details.Code, details.Body.String())
			var view struct {
				Data struct {
					Health channelhealth.Snapshot `json:"health"`
				} `json:"data"`
			}
			require.NoError(t, common.Unmarshal(details.Body.Bytes(), &view))
			assert.Equal(t, "open", view.Data.Health.Circuit)
			assert.Equal(t, fixture.fake.Now().Add(tc.delay).UnixMilli(), view.Data.Health.RetryAt)
			assert.Equal(t, 1, view.Data.Health.Samples)
			if strings.HasPrefix(tc.header, "private-") {
				assert.NotContains(t, response.Body.String(), tc.header)
				assert.NotContains(t, details.Body.String(), tc.header)
				attempts, _, err := model.ListGatewayAttempts(t.Context(), response.Header().Get(common.RequestIdKey), 0, 10)
				require.NoError(t, err)
				require.Len(t, attempts, 1)
				encoded, err := common.Marshal(attempts)
				require.NoError(t, err)
				assert.NotContains(t, string(encoded), tc.header)
			}
		})
	}
}

func TestTextGatewayHealthOrdersByCompletedBusinessLatency(t *testing.T) {
	var fixture textGatewayFixture
	firstCalls, secondCalls := 0, 0
	fixture = newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		firstCalls++
		fixture.fake.Advance(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Slow"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	})
	body := `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", body)
	require.Equal(t, 200, response.Code, response.Body.String())
	second := addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) {
		secondCalls++
		fixture.fake.Advance(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Fast"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	})
	for range 2 {
		response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", body)
		require.Equal(t, 200, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), "Fast")
	}
	assert.Equal(t, 1, firstCalls)
	assert.Equal(t, 2, secondCalls)
	health, err := service.ChannelHealth.Snapshots(t.Context(), []int{fixture.channel.ID, second.ID})
	require.NoError(t, err)
	assert.Equal(t, int64(200), health[fixture.channel.ID].FirstByteMillis)
	assert.Equal(t, int64(20), health[second.ID].FirstByteMillis)
	fixture.fake.Advance(5*time.Minute + time.Millisecond)
	health, err = service.ChannelHealth.Snapshots(t.Context(), []int{fixture.channel.ID, second.ID})
	require.NoError(t, err)
	assert.Zero(t, health[second.ID].Samples)
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
	assertTextGatewayLedger(t, *record, 27000)
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
		{"model 404", 404, "model_not_found", "model_not_found", 3}, {"Azure deployment 404", 404, "DeploymentNotFound", "model_not_found", 3}, {"other 404", 404, "unknown_path", "upstream_error", 1},
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
				if test.code == "model_not_found" || test.status == 429 || test.status >= 500 {
					_, _ = fmt.Fprintf(w, `{"error":{"type":"invalid_request_error","code":%q,"message":"private"}}`, test.code)
				} else {
					_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"message":"Bearer provider-secret https://private.invalid/?key=private"}}`, test.code)
				}
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

func TestTextGatewayNativeIdleTimeoutAfterBusinessOutput(t *testing.T) {
	for _, test := range []struct {
		name, data, key string
		provider        int
	}{
		{"Ollama", "{\"model\":\"upstream-model\",\"message\":{\"role\":\"assistant\",\"content\":\"Hello\"},\"done\":false}\n", "", constant.ChannelTypeOllama},
		{"Coze", "event: conversation.message.delta\ndata: {\"content\":\"Hello\",\"type\":\"answer\"}\n\n", "", constant.ChannelTypeCoze},
		{"Cohere", "{\"event_type\":\"text-generation\",\"text\":\"Hello\"}\n", "", constant.ChannelTypeCohere},
		{"Tencent", "data: {\"Choices\":[{\"Delta\":{\"Content\":\"Hello\"}}]}\n\n", "1|test-id|test-secret", constant.ChannelTypeTencent},
		{"Zhipu", "data:Hello\n", "test.secret", constant.ChannelTypeZhipu},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(test.data))
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			})
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", test.provider).Error)
			if test.key != "" {
				require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("key", test.key).Error)
			}
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
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gateway-alias","stream":true,"user":"idle-user","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)).WithContext(ctx)
			request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { fixture.engine.ServeHTTP(response, request); close(done) }()
			select {
			case <-reading:
			case <-done:
				t.Fatalf("stream ended before business delivery: %d %s", response.Code, response.Body.String())
			case <-time.After(10 * time.Second):
				cancel()
				<-done
				t.Fatal("Provider did not reach a business-frame read")
			}
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
			assert.NotContains(t, response.Body.String(), "[DONE]")
		})
	}
}

func TestTextGatewayCozeMetadataCompletionDoesNotEndFallbackStream(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: conversation.chat.completed\ndata: {\"usage\":{\"input_count\":10,\"output_count\":5,\"token_count\":15}}\n\n"))
	})
	require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", constant.ChannelTypeCoze).Error)
	addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Recovered\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	})
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","stream":true,"user":"preview-user","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
	require.Contains(t, response.Body.String(), "Recovered")
	assert.Equal(t, 1, strings.Count(response.Body.String(), "[DONE]"))
	assert.Less(t, strings.Index(response.Body.String(), "Recovered"), strings.Index(response.Body.String(), "[DONE]"))
}

func TestTextGatewayCozeRejectionsPreserveStatusAndProviderScope(t *testing.T) {
	for _, stage := range []string{"create", "poll"} {
		for _, status := range []int{400, 401, 403} {
			for _, format := range []string{"json", "text", "truncated"} {
				t.Run(fmt.Sprint(stage, "/", status, "/", format), func(t *testing.T) {
					fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						if stage == "poll" && r.Method == http.MethodPost {
							_, _ = w.Write([]byte(`{"code":0,"data":{"id":"chat","conversation_id":"conversation"}}`))
							return
						}
						if format == "truncated" {
							w.Header().Set("Content-Length", "100")
						}
						w.WriteHeader(status)
						if format != "json" {
							_, _ = w.Write([]byte("private-coze-secret"))
							if format == "truncated" {
								w.(http.Flusher).Flush()
								conn, _, err := w.(http.Hijacker).Hijack()
								if assert.NoError(t, err) {
									_ = conn.Close()
								}
							}
							return
						}
						_, _ = fmt.Fprintf(w, `{"code":%d,"msg":"Bearer private-coze-secret"}`, status)
					})
					require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", constant.ChannelTypeCoze).Error)
					sibling := *fixture.channel
					sibling.ID, sibling.UpstreamModelName = 0, "same-account-next"
					require.NoError(t, model.DB.Create(&sibling).Error)
					price := *fixture.purchase
					price.ID, price.OwnerID = 0, sibling.ID
					require.NoError(t, model.CreatePriceVersion(t.Context(), &price, fixture.sale.CreatedBy))
					recovered := 0
					addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) {
						recovered++
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Recovered"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
					})
					response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","user":"preview-user","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
					attempts, total, err := model.ListGatewayAttempts(t.Context(), response.Header().Get(common.RequestIdKey), 0, 10)
					require.NoError(t, err)
					if status == 400 {
						require.EqualValues(t, 1, total)
						assert.Zero(t, recovered)
						assert.Equal(t, "invalid_request", attempts[0].ErrorClass)
					} else {
						require.EqualValues(t, 2, total)
						assert.Equal(t, 1, recovered)
						for _, a := range attempts {
							if a.ChannelID == fixture.channel.ID {
								assert.Equal(t, "credentials", a.ErrorClass)
							}
						}
					}
					assert.NotContains(t, response.Body.String(), "private-coze-secret")
				})
			}
		}
	}
}

func TestTextGatewayBaiduTokenPreparationUsesSharedBudgetAndCancellation(t *testing.T) {
	for _, cancelDownstream := range []bool{false, true} {
		t.Run(fmt.Sprint("cancel=", cancelDownstream), func(t *testing.T) {
			entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
			defer close(release)
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if r.URL.Path == "/oauth/2.0/token" {
					close(entered)
					select {
					case <-r.Context().Done():
						close(cancelled)
					case <-release:
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"access_token":"fake-token","expires_in":7200}`))
					return
				}
				t.Error("Baidu model must not be sent after token preparation fails")
			})
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Updates(map[string]any{"type": constant.ChannelTypeBaidu, "key": "budget37-" + t.Name() + "|fake"}).Error)
			var provider model.Provider
			require.NoError(t, model.DB.First(&provider, fixture.channel.ProviderID).Error)
			endpoint, err := url.Parse(*provider.BaseURL)
			require.NoError(t, err)
			client := service.GetHttpClient()
			original := client.Transport
			t.Cleanup(func() { client.Transport = original })
			client.Transport = channelProbeRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "aip.baidubce.com" {
					return original.RoundTrip(r)
				}
				copyRequest := r.Clone(r.Context())
				copyURL := *r.URL
				copyURL.Scheme, copyURL.Host = endpoint.Scheme, endpoint.Host
				copyRequest.URL, copyRequest.Host = &copyURL, endpoint.Host
				return original.RoundTrip(copyRequest)
			})
			recoveries := 0
			addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) {
				recoveries++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Recovered"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
			})
			t.Setenv("GATEWAY_MAX_ATTEMPTS", "1")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)).WithContext(ctx)
			request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { fixture.engine.ServeHTTP(response, request); close(done) }()
			<-entered
			if cancelDownstream {
				cancel()
			} else {
				fixture.fake.Advance(30 * time.Second)
			}
			select {
			case <-cancelled:
			case <-time.After(10 * time.Second):
				cancel()
				release <- struct{}{}
				<-done
				t.Fatal("token preparation ignored cancellation or the shared budget")
			}
			<-done
			_, total, err := model.ListGatewayAttempts(t.Context(), response.Header().Get(common.RequestIdKey), 0, 10)
			require.NoError(t, err)
			if cancelDownstream {
				assert.Zero(t, recoveries)
				assert.Zero(t, total)
			} else {
				assert.Equal(t, 1, recoveries)
				assert.EqualValues(t, 1, total)
				assert.Contains(t, response.Body.String(), "Recovered")
			}
		})
	}
}

func TestTextGatewayBaiduTokenExpiryAndRejectionControls(t *testing.T) {
	for _, test := range []struct {
		name   string
		expiry int64
		reject string
		repeat bool
	}{
		{"expired cache refresh", 7200, "", true}, {"missing expiry", 0, "", false}, {"negative expiry", -1, "", false}, {"overflowing expiry", 1<<63 - 1, "", false}, {"invalid client", 7200, "invalid_client", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			tokens, models := 0, 0
			logPath := captureGatewayDebugLog(t)
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/oauth/2.0/token" {
					tokens++
					if test.reject != "" {
						w.WriteHeader(400)
						_, _ = fmt.Fprintf(w, `{"error":%q,"error_description":"private-token-secret"}`, test.reject)
					} else {
						_, _ = fmt.Fprintf(w, `{"access_token":"fake+token/=","expires_in":%d}`, test.expiry)
					}
					return
				}
				models++
				assert.Empty(t, r.Header.Get("Authorization"), "the resource request must not forward the OAuth client secret")
				assert.Equal(t, "fake+token/=", r.URL.Query().Get("access_token"))
				_, _ = w.Write([]byte(`{"result":"Hello","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
			})
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Updates(map[string]any{"type": constant.ChannelTypeBaidu, "key": "expiry37-" + t.Name() + "|fake"}).Error)
			if test.reject != "" {
				addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Recovered"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
				})
			}
			var provider model.Provider
			require.NoError(t, model.DB.First(&provider, fixture.channel.ProviderID).Error)
			endpoint, err := url.Parse(*provider.BaseURL)
			require.NoError(t, err)
			client := service.GetHttpClient()
			original := client.Transport
			t.Cleanup(func() { client.Transport = original })
			client.Transport = channelProbeRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "aip.baidubce.com" {
					return original.RoundTrip(r)
				}
				assert.Equal(t, "https", r.URL.Scheme)
				copyRequest := r.Clone(r.Context())
				copyURL := *r.URL
				copyURL.Scheme, copyURL.Host = endpoint.Scheme, endpoint.Host
				copyRequest.URL, copyRequest.Host = &copyURL, endpoint.Host
				return original.RoundTrip(copyRequest)
			})
			body := `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", body)
			if test.repeat {
				require.Equal(t, 200, response.Code, response.Body.String())
				fixture.fake.Advance(3 * time.Hour)
				response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", body)
				assert.Equal(t, 200, response.Code, response.Body.String())
				assert.Equal(t, 2, tokens)
				assert.Equal(t, 2, models)
			} else if test.reject != "" {
				assert.Equal(t, 1, tokens)
				assert.Zero(t, models)
				assert.Contains(t, response.Body.String(), "Recovered")
				logged, err := os.ReadFile(logPath)
				require.NoError(t, err)
				assert.Contains(t, string(logged), "category=credentials_rejected")
				assert.Contains(t, string(logged), fmt.Sprintf("provider=%d", fixture.channel.ProviderID))
				assert.Contains(t, string(logged), response.Header().Get(common.RequestIdKey))
				assert.NotContains(t, string(logged), "private-token-secret")
				assert.NotContains(t, string(logged), "expiry37-"+t.Name()+"|fake")
				_, total, err := model.ListGatewayAttempts(t.Context(), response.Header().Get(common.RequestIdKey), 0, 10)
				require.NoError(t, err)
				assert.EqualValues(t, 1, total)
			} else {
				assert.Equal(t, 1, tokens)
				assert.Zero(t, models)
				assert.GreaterOrEqual(t, response.Code, 400)
				wallet, err := model.GetUserWallet(fixture.user.Id)
				require.NoError(t, err)
				assert.EqualValues(t, 1_000_000_000, wallet.Balance.Nanos)
				_, total, err := model.ListGatewayAttempts(t.Context(), response.Header().Get(common.RequestIdKey), 0, 10)
				require.NoError(t, err)
				assert.Zero(t, total)
			}
			assert.NotContains(t, response.Body.String(), "private-token-secret")
			assert.NotContains(t, response.Body.String(), "fake+token/=")
		})
	}
}

// HTTP test leaves explicitly opt into the credential transport contract.
func (transport channelProbeRoundTripper) CloneWithVerifiedTLS() http.RoundTripper { return transport }

func TestBaiduTokenPreparationRejectsUntrustedTLS(t *testing.T) {
	t.Setenv("TLS_INSECURE_SKIP_VERIFY", "true")
	logPath := captureGatewayDebugLog(t)
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("the model cannot run after TLS verification failure")
	})
	require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Updates(map[string]any{"type": constant.ChannelTypeBaidu, "key": t.Name() + "|fake"}).Error)
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/capture", http.StatusTemporaryRedirect)
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"fake-token","expires_in":7200}`))
	}))
	defer server.Close()
	client := service.GetHttpClient()
	if client == nil {
		service.InitHttpClient()
		client = service.GetHttpClient()
	}
	original := client.Transport
	t.Cleanup(func() { client.Transport = original })
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	defer transport.CloseIdleConnections()
	client.Transport = transport
	provider := relay.GetProviderAdapter(constant.APITypeBaidu)
	preparer, ok := provider.(channel.ProviderAccountPreparer)
	require.True(t, ok)
	err := preparer.PrepareAccount(t.Context(), &channel.ProviderRequest{ApiKey: strings.Join([]string{"budget37-tls-verification", "fake"}, "|")}, func() (context.Context, error) { return t.Context(), nil })
	require.Error(t, err)
	var invalid *tls.CertificateVerificationError
	assert.ErrorAs(t, err, &invalid)
	untrusted, err := service.GetVerifiedHttpClientWithProxy("")
	require.NoError(t, err)
	_, err = untrusted.Get(server.URL)
	untrusted.CloseIdleConnections()
	var unknownAuthority x509.UnknownAuthorityError
	assert.ErrorAs(t, err, &unknownAuthority)
	legacy := relay.GetProviderAdapter(constant.APITypeBaidu)
	_, err = legacy.RequestURL(&channel.ProviderRequest{ApiKey: t.Name() + "-legacy|fake", ChannelBaseUrl: server.URL, UpstreamModelName: "ERNIE-Bot"})
	require.Error(t, err, "legacy cache misses must use the same verified token source")
	gatewayResponse := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
	assert.GreaterOrEqual(t, gatewayResponse.Code, 400)
	logged, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Contains(t, string(logged), "category=certificate_verification_failed")
	assert.Contains(t, string(logged), fmt.Sprintf("provider=%d", fixture.channel.ProviderID))
	assert.Contains(t, string(logged), gatewayResponse.Header().Get(common.RequestIdKey))
	assert.NotContains(t, string(logged), t.Name()+"|fake")
	assert.Zero(t, requests, "credentials must not reach an untrusted TLS endpoint")
	assert.True(t, transport.TLSClientConfig.InsecureSkipVerify, "verification must not mutate the shared relay transport")
	transport.TLSClientConfig.RootCAs = x509.NewCertPool()
	transport.TLSClientConfig.RootCAs.AddCert(server.Certificate())
	verified, err := service.GetVerifiedHttpClientWithProxy("")
	require.NoError(t, err)
	defer verified.CloseIdleConnections()
	response, err := verified.Get(server.URL)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, 1, requests, "a trusted certificate remains usable")
	redirected, err := verified.PostForm(server.URL+"/redirect", url.Values{"client_secret": {"fake"}})
	require.NoError(t, err)
	require.NoError(t, redirected.Body.Close())
	assert.Equal(t, http.StatusTemporaryRedirect, redirected.StatusCode)
	assert.Equal(t, 2, requests, "credential request bodies must not follow redirects")
}

func TestTextGatewayBaiduNativeRejectionsExcludeProvider(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, code := range []int{17, 110, 111} {
			for _, status := range []int{200, 400, 429} {
				if stream && status != 200 {
					continue
				}
				t.Run(fmt.Sprint("stream=", stream, "/code=", code, "/status=", status), func(t *testing.T) {
					calls := 0
					fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						if r.URL.Path == "/oauth/2.0/token" {
							_, _ = w.Write([]byte(`{"access_token":"fake-token","expires_in":7200}`))
							return
						}
						calls++
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = fmt.Fprintf(w, "data: {\"error_code\":%d,\"error_msg\":\"private-baidu-error\"}\n\n", code)
						} else {
							w.WriteHeader(status)
							_, _ = fmt.Fprintf(w, `{"error_code":%d,"error_msg":"private-baidu-error"}`, code)
						}
					})
					require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Updates(map[string]any{"type": constant.ChannelTypeBaidu, "key": t.Name() + "|fake"}).Error)
					sibling := *fixture.channel
					sibling.ID, sibling.UpstreamModelName = 0, "same-account-next"
					require.NoError(t, model.DB.Create(&sibling).Error)
					price := *fixture.purchase
					price.ID, price.OwnerID = 0, sibling.ID
					require.NoError(t, model.CreatePriceVersion(t.Context(), &price, fixture.sale.CreatedBy))
					var provider model.Provider
					require.NoError(t, model.DB.First(&provider, fixture.channel.ProviderID).Error)
					endpoint, err := url.Parse(*provider.BaseURL)
					require.NoError(t, err)
					client := service.GetHttpClient()
					original := client.Transport
					t.Cleanup(func() { client.Transport = original })
					client.Transport = channelProbeRoundTripper(func(r *http.Request) (*http.Response, error) {
						if r.URL.Host != "aip.baidubce.com" {
							return original.RoundTrip(r)
						}
						copyRequest := r.Clone(r.Context())
						copyURL := *r.URL
						copyURL.Scheme, copyURL.Host = endpoint.Scheme, endpoint.Host
						copyRequest.URL, copyRequest.Host = &copyURL, endpoint.Host
						return original.RoundTrip(copyRequest)
					})
					recovered := 0
					addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) {
						recovered++
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Recovered\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\ndata: [DONE]\n\n"))
							return
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Recovered"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
					})
					response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(`{"model":"gateway-alias","stream":%t,"messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`, stream))
					assert.Equal(t, 1, calls)
					assert.Equal(t, 1, recovered)
					assert.Contains(t, response.Body.String(), "Recovered")
					assert.NotContains(t, response.Body.String(), "private-baidu-error")
					attempts, total, err := model.ListGatewayAttempts(t.Context(), response.Header().Get(common.RequestIdKey), 0, 10)
					require.NoError(t, err)
					require.EqualValues(t, 2, total)
					for _, a := range attempts {
						if a.ChannelID == fixture.channel.ID {
							expected := "credentials"
							if code == 17 {
								expected = "account_quota"
							}
							assert.Equal(t, expected, a.ErrorClass)
						}
					}
				})
			}
		}
	}
}

func TestTextGatewayStreamModelRejectionUsesChannelScope(t *testing.T) {
	for _, business := range []bool{false, true} {
		for _, code := range []string{"model_not_found", "DeploymentNotFound"} {
			t.Run(fmt.Sprint(code, "/business=", business), func(t *testing.T) {
				calls := 0
				fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
					calls++
					w.Header().Set("Content-Type", "text/event-stream")
					if calls > 1 {
						_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Recovered\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\ndata: [DONE]\n\n"))
						return
					}
					if business {
						_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"First\"}}]}\n\n"))
					}
					_, _ = fmt.Fprintf(w, "data: {\"error\":{\"type\":\"invalid_request_error\",\"code\":%q,\"message\":\"private-model-error\"}}\n\n", code)
				})
				sibling := *fixture.channel
				sibling.ID, sibling.UpstreamModelName = 0, "same-account-next"
				require.NoError(t, model.DB.Create(&sibling).Error)
				price := *fixture.purchase
				price.ID, price.OwnerID = 0, sibling.ID
				require.NoError(t, model.CreatePriceVersion(t.Context(), &price, fixture.sale.CreatedBy))
				response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","stream":true,"messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
				attempts, total, err := model.ListGatewayAttempts(t.Context(), response.Header().Get(common.RequestIdKey), 0, 10)
				require.NoError(t, err)
				if business {
					assert.Equal(t, 1, calls)
					require.EqualValues(t, 1, total)
					assert.Contains(t, response.Body.String(), "First")
					assert.NotContains(t, response.Body.String(), "Recovered")
					assert.NotContains(t, response.Body.String(), "[DONE]")
				} else {
					assert.Equal(t, 2, calls)
					require.EqualValues(t, 2, total)
					assert.Contains(t, response.Body.String(), "Recovered")
					assert.Equal(t, 1, strings.Count(response.Body.String(), "[DONE]"))
				}
				for _, a := range attempts {
					if a.ChannelID == fixture.channel.ID {
						assert.Equal(t, "model_not_found", a.ErrorClass)
					}
				}
				assert.NotContains(t, response.Body.String(), "private-model-error")
			})
		}
	}
}

func TestTextGatewayFirstByteBudgetIsNotShortenedByIdleTimeout(t *testing.T) {
	previous := constant.StreamingTimeout
	constant.StreamingTimeout = 300
	t.Cleanup(func() { constant.StreamingTimeout = previous })
	release, reading, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			return
		case <-release:
		}
		_, _ = w.Write([]byte("{\"message\":{\"role\":\"assistant\",\"content\":\"Hello\"},\"done\":true,\"prompt_eval_count\":10,\"eval_count\":5}\n"))
	})
	constant.StreamingTimeout = 300
	require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", constant.ChannelTypeOllama).Error)
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", fixture.sale.CreatedBy).Update("role", common.RoleRootUser).Error)
	root := createWalletTestAccessToken(t, fixture.sale.CreatedBy, "option:write")
	for _, key := range []string{"GatewayTotalFirstByteSeconds", "GatewayAttemptFirstByteSeconds"} {
		updated := adminSecurityRequest(fixture.engine, root, http.MethodPut, "/api/option/", fmt.Sprintf(`{"key":%q,"value":600}`, key))
		require.Contains(t, updated.Body.String(), `"success":true`)
	}
	client := service.GetHttpClient()
	original := client.Transport
	t.Cleanup(func() { client.Transport = original })
	client.Transport = channelProbeRoundTripper(func(r *http.Request) (*http.Response, error) {
		response, err := original.RoundTrip(r)
		if err == nil {
			response.Body = &gatewayObservedStreamBody{ReadCloser: response.Body, reads: 1, readingAgain: reading, closed: closed}
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
	select {
	case <-reading:
	case <-done:
		t.Fatal("stream stopped before the first read")
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		t.Fatal("upstream read not reached")
	}
	fixture.fake.Advance(300 * time.Second)
	select {
	case <-closed:
		t.Error("idle timeout shortened the configured 600-second first-byte budget")
	default:
	}
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		t.Fatal("stream did not complete after the business frame was released")
	}
	assert.Contains(t, response.Body.String(), "Hello")
	assert.Equal(t, 1, strings.Count(response.Body.String(), "[DONE]"))
	record, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
	require.NoError(t, err)
	assert.Equal(t, "success", record.Status)
}

func TestTextGatewayPurchaseSnapshotStartsAfterAccountPreparation(t *testing.T) {
	for _, tc := range []struct {
		total       int
		preparation time.Duration
	}{{90, 10 * time.Second}, {10, 6 * time.Second}, {11, 6 * time.Second}} {
		t.Run(fmt.Sprint("total=", tc.total), func(t *testing.T) {
			modelCalls := 0
			var fixture textGatewayFixture
			fixture = newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/oauth/2.0/token" {
					fixture.fake.Advance(tc.preparation)
					_, _ = w.Write([]byte(`{"access_token":"fake-token","expires_in":7200}`))
					return
				}
				modelCalls++
				_, _ = w.Write([]byte(`{"result":"Hello","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
			})
			require.NoError(t, model.UpdateOption("GatewayTotalFirstByteSeconds", fmt.Sprint(tc.total)))
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Updates(map[string]any{"type": constant.ChannelTypeBaidu, "key": t.Name() + "|fake"}).Error)
			started := clock.Now()
			later := &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: `tier("tokens",p * 3 + c * 4)`, EffectiveAt: started.Add(5 * time.Second).Unix()}
			require.NoError(t, model.CreatePriceVersion(t.Context(), later, fixture.sale.CreatedBy))
			var provider model.Provider
			require.NoError(t, model.DB.First(&provider, fixture.channel.ProviderID).Error)
			endpoint, err := url.Parse(*provider.BaseURL)
			require.NoError(t, err)
			client := service.GetHttpClient()
			original := client.Transport
			t.Cleanup(func() { client.Transport = original })
			client.Transport = channelProbeRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "aip.baidubce.com" {
					return original.RoundTrip(r)
				}
				copyRequest := r.Clone(r.Context())
				copyURL := *r.URL
				copyURL.Scheme, copyURL.Host = endpoint.Scheme, endpoint.Host
				copyRequest.URL, copyRequest.Host = &copyURL, endpoint.Host
				return original.RoundTrip(copyRequest)
			})
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
			if tc.total == 10 {
				assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
				assert.Zero(t, modelCalls)
				_, total, err := model.ListGatewayAttempts(t.Context(), response.Header().Get(common.RequestIdKey), 0, 10)
				require.NoError(t, err)
				assert.Zero(t, total)
				record, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
				require.NoError(t, err)
				assert.Zero(t, record.Receivable.Nanos)
				return
			}
			require.Equal(t, 200, response.Code, response.Body.String())
			assert.Equal(t, 1, modelCalls)
			attempts, total, err := model.ListGatewayAttempts(t.Context(), response.Header().Get(common.RequestIdKey), 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.Equal(t, later.ID, attempts[0].PurchasePriceVersionID)
			assert.Equal(t, started.Add(tc.preparation).UnixMilli(), attempts[0].StartedAt)
			assert.EqualValues(t, 50_000, attempts[0].Cost.Nanos)
			record, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
			require.NoError(t, err)
			assert.EqualValues(t, 36_000, record.Receivable.Nanos)
		})
	}
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

func TestTextGatewayCozeBusinessOutputPreventsSwitch(t *testing.T) {
	calls := 0
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: conversation.message.delta\ndata: {\"content\":\"First\",\"type\":\"answer\"}\n\nevent: conversation.chat.completed\ndata: {\"usage\":{\"input_count\":10,\"output_count\":5,\"token_count\":15}}\n\n"))
	})
	require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", constant.ChannelTypeCoze).Error)
	addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Recovered\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	})
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","stream":true,"user":"preview-user","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
	assert.Equal(t, 1, calls)
	assert.Contains(t, response.Body.String(), "First")
	assert.NotContains(t, response.Body.String(), "Recovered")
	_, total, err := model.ListGatewayAttempts(t.Context(), response.Header().Get(common.RequestIdKey), 0, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
}

func TestTextGatewaySDKPreparationDoesNotSpendAttempt(t *testing.T) {
	for _, test := range []struct {
		name  string
		parts []string
	}{
		{"missing separator", []string{"fake"}},
		{"empty region", []string{"fake", ""}},
		{"empty access key", []string{"", "secret", "us-east-1"}},
		{"empty secret key", []string{"fake", "", "us-east-1"}},
		{"invalid region", []string{"fake", "bad region"}},
		{"invalid region label", []string{"fake", "us-east-1..invalid"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid SDK credentials must not be sent") })
			t.Setenv("GATEWAY_MAX_ATTEMPTS", "1")
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Updates(map[string]any{"type": constant.ChannelTypeAws, "key": strings.Join(test.parts, "|")}).Error)
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
		})
	}
}

func TestTextGatewaySendContextUsesSharedBudgetsInsteadOfRelayDeadline(t *testing.T) {
	for _, providerType := range []int{constant.ChannelTypeOpenAI, constant.ChannelTypeAws} {
		t.Run(fmt.Sprint(providerType), func(t *testing.T) {
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\ndata: [DONE]\n\n"))
			})
			if providerType == constant.ChannelTypeAws {
				require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Updates(map[string]any{"type": providerType, "key": strings.Join([]string{"fake", "us-east-1"}, "|")}).Error)
				require.NoError(t, model.DB.Model(fixture.channel).Update("upstream_model_name", "amazon.nova-lite-v1:0").Error)
			}
			previousRelayTimeout := common.RelayTimeout
			common.RelayTimeout = 30
			t.Cleanup(func() { common.RelayTimeout = previousRelayTimeout })
			client := service.GetHttpClient()
			original, timeout := client.Transport, client.Timeout
			client.Timeout = 30 * time.Second
			t.Cleanup(func() { client.Transport, client.Timeout = original, timeout })
			client.Transport = channelProbeRoundTripper(func(request *http.Request) (*http.Response, error) {
				_, absoluteDeadline := request.Context().Deadline()
				assert.False(t, absoluteDeadline, "a legacy total deadline must not outlive first business delivery")
				if providerType == constant.ChannelTypeAws {
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"output":{"message":{"content":[{"text":"Hello"}]}},"usage":{"inputTokens":10,"outputTokens":5,"totalTokens":15}}`)), Request: request}, nil
				}
				return original.RoundTrip(request)
			})
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","stream":true,"messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
			assert.Contains(t, response.Body.String(), "Hello")
			record, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, "success", record.Status)
		})
	}
}

func TestTextGatewaySDKCannotHideAdditionalSends(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("SDK must use its own transport") })
	t.Setenv("GATEWAY_MAX_ATTEMPTS", "1")
	require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Updates(map[string]any{"type": constant.ChannelTypeAws, "key": strings.Join([]string{"fake", "us-east-1"}, "|")}).Error)
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
	}{{400, 1, "ValidationException", "invalid_request"}, {401, 2, "UnrecognizedClientException", "credentials"}, {403, 2, "AccessDeniedException", "credentials"}, {404, 3, "ResourceNotFoundException", "model_not_found"}, {404, 1, "UnknownPath", "upstream_error"}, {429, 2, "ServiceQuotaExceededException", "account_quota"}} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("SDK must use its own transport") })
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Updates(map[string]any{"type": constant.ChannelTypeAws, "key": strings.Join([]string{"fake", "us-east-1"}, "|")}).Error)
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
			if tc.class == "model_not_found" {
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

func TestTextGatewayDifyUploadUsesSharedBudgetWithoutSpendingModelAttempt(t *testing.T) {
	for _, cancelDownstream := range []bool{false, true} {
		t.Run(fmt.Sprint("cancel=", cancelDownstream), func(t *testing.T) {
			entered, cancelled := make(chan struct{}), make(chan struct{})
			uploads, generations := 0, 0
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if strings.HasSuffix(r.URL.Path, "/files/upload") {
					uploads++
					close(entered)
					<-r.Context().Done()
					close(cancelled)
					return
				}
				generations++
				w.WriteHeader(503)
			})
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", constant.ChannelTypeDify).Error)
			var unified model.Model
			require.NoError(t, model.DB.First(&unified, fixture.channel.ModelID).Error)
			unified.Capabilities.InputModalities = append(unified.Capabilities.InputModalities, capability.Image)
			require.NoError(t, model.DB.Model(&unified).Select("capabilities").Updates(&unified).Error)
			healthy := addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Recovered"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
			})
			t.Setenv("GATEWAY_MAX_ATTEMPTS", "1")
			var picture bytes.Buffer
			require.NoError(t, png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 1, 1))))
			body := fmt.Sprintf(`{"model":"gateway-alias","messages":[{"role":"user","content":[{"type":"text","text":"Hello"},{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}}]}],"max_tokens":16}`, base64.StdEncoding.EncodeToString(picture.Bytes()))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
			request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { fixture.engine.ServeHTTP(response, request); close(done) }()
			<-entered
			if cancelDownstream {
				cancel()
			} else {
				fixture.fake.Advance(30 * time.Second)
			}
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				cancel()
				<-done
				t.Fatal("native upload ignored the shared first-byte budget")
			}
			<-cancelled
			assert.Equal(t, 1, uploads)
			assert.Zero(t, generations)
			attempts, total, err := model.ListGatewayAttempts(t.Context(), response.Header().Get(common.RequestIdKey), 0, 10)
			require.NoError(t, err)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			if cancelDownstream {
				assert.Zero(t, total)
				assert.NotContains(t, response.Body.String(), "Recovered")
				assert.EqualValues(t, 1_000_000_000, wallet.Balance.Nanos)
				record, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
				require.NoError(t, err)
				assert.Equal(t, "cancelled", record.Status)
			} else {
				require.EqualValues(t, 1, total)
				assert.Equal(t, healthy.ID, attempts[0].ChannelID)
				assert.Contains(t, response.Body.String(), "Recovered")
				assert.EqualValues(t, 999_964_000, wallet.Balance.Nanos)
			}
		})
	}
}

func TestTextGatewayDifyUploadRejectionsRespectSharedExclusions(t *testing.T) {
	for _, test := range []struct {
		status    int
		code      string
		uploads   int
		recovered bool
	}{
		{401, "", 1, true}, {403, "", 1, true}, {429, "insufficient_quota", 1, true},
		{503, "server_error", 2, true}, {400, "invalid_request_error", 1, false},
		{404, "unknown_path", 1, false}, {403, "content_filter", 1, false},
	} {
		t.Run(fmt.Sprint(test.status, "/", test.code), func(t *testing.T) {
			logPath := captureGatewayDebugLog(t)
			uploads := 0
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				assert.True(t, strings.HasSuffix(r.URL.Path, "/files/upload"))
				uploads++
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = fmt.Fprintf(w, `{"code":%q,"message":"Bearer private-upload-secret"}`, test.code)
			})
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", constant.ChannelTypeDify).Error)
			var unified model.Model
			require.NoError(t, model.DB.First(&unified, fixture.channel.ModelID).Error)
			unified.Capabilities.InputModalities = append(unified.Capabilities.InputModalities, capability.Image)
			require.NoError(t, model.DB.Model(&unified).Select("capabilities").Updates(&unified).Error)
			sibling := *fixture.channel
			sibling.ID, sibling.UpstreamModelName = 0, "same-account-next"
			require.NoError(t, model.DB.Create(&sibling).Error)
			price := *fixture.purchase
			price.ID, price.OwnerID = 0, sibling.ID
			require.NoError(t, model.CreatePriceVersion(t.Context(), &price, fixture.sale.CreatedBy))
			addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Recovered"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
			})
			t.Setenv("GATEWAY_MAX_ATTEMPTS", "1")
			var picture bytes.Buffer
			require.NoError(t, png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 1, 1))))
			body := fmt.Sprintf(`{"model":"gateway-alias","messages":[{"role":"user","content":[{"type":"text","text":"Hello"},{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}}]}],"max_tokens":16}`, base64.StdEncoding.EncodeToString(picture.Bytes()))
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", body)
			assert.Equal(t, test.uploads, uploads)
			_, total, err := model.ListGatewayAttempts(t.Context(), response.Header().Get(common.RequestIdKey), 0, 10)
			require.NoError(t, err)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			if test.recovered {
				assert.EqualValues(t, 1, total)
				assert.Contains(t, response.Body.String(), "Recovered")
				assert.EqualValues(t, 999_964_000, wallet.Balance.Nanos)
			} else {
				assert.Zero(t, total)
				assert.NotContains(t, response.Body.String(), "Recovered")
				assert.EqualValues(t, 1_000_000_000, wallet.Balance.Nanos)
			}
			logged, err := os.ReadFile(logPath)
			require.NoError(t, err)
			assert.NotContains(t, string(logged), "private-upload-secret")
			assert.NotContains(t, response.Body.String(), "private-upload-secret")
		})
	}
}

func TestTextGatewayDifyMetadataDoesNotBecomeBillableReasoning(t *testing.T) {
	for _, business := range []bool{false, true} {
		t.Run(fmt.Sprint("business=", business), func(t *testing.T) {
			previous := constant.DifyDebug
			constant.DifyDebug = true
			t.Cleanup(func() { constant.DifyDebug = previous })
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"event\":\"workflow_started\",\"data\":{\"workflow_id\":\"internal-workflow\"}}\n\ndata: {\"event\":\"node_started\",\"data\":{\"node_type\":\"internal-node\"}}\n\n"))
				if business {
					_, _ = w.Write([]byte("data: {\"event\":\"message\",\"answer\":\"Hello\"}\n\n"))
				}
				_, _ = w.Write([]byte("data: {\"event\":\"error\",\"status\":503,\"code\":\"overloaded\"}\n\n"))
			})
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", constant.ChannelTypeDify).Error)
			calls := 0
			addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Recovered\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
			})
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","stream":true,"messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
			assert.NotContains(t, response.Body.String(), "internal-workflow")
			assert.NotContains(t, response.Body.String(), "internal-node")
			assert.NotContains(t, response.Body.String(), "reasoning_content")
			if business {
				assert.Zero(t, calls)
				assert.Contains(t, response.Body.String(), "Hello")
				record, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
				require.NoError(t, err)
				assert.Equal(t, "partial", record.Status)
				assert.EqualValues(t, 1, record.Usage.CompletionTokens)
			} else {
				assert.Equal(t, 1, calls)
				assert.Contains(t, response.Body.String(), "Recovered")
			}
		})
	}
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

func captureGatewayDebugLog(t *testing.T) string {
	t.Helper()
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
	return logFile.Name()
}

type gatewayStreamReadFailure struct {
	io.ReadCloser
	read bool
}

func (body *gatewayStreamReadFailure) Read(p []byte) (int, error) {
	if body.read {
		return 0, fmt.Errorf("Bearer provider-secret https://private.invalid/?key=secret")
	}
	body.read = true
	n, err := body.ReadCloser.Read(p)
	if n > 0 {
		return n, nil
	}
	return n, err
}

func TestTextGatewayStreamReadErrorsNeverLogProviderSecrets(t *testing.T) {
	path := captureGatewayDebugLog(t)
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n"))
	})
	client := service.GetHttpClient()
	original := client.Transport
	t.Cleanup(func() { client.Transport = original })
	client.Transport = channelProbeRoundTripper(func(request *http.Request) (*http.Response, error) {
		response, err := original.RoundTrip(request)
		if err == nil {
			response.Body = &gatewayStreamReadFailure{ReadCloser: response.Body}
		}
		return response, err
	})
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","stream":true,"messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
	logged, err := os.ReadFile(path)
	require.NoError(t, err)
	for _, secret := range []string{"provider-secret", "private.invalid"} {
		assert.NotContains(t, response.Body.String(), secret)
		assert.NotContains(t, string(logged), secret)
	}
}

func TestTextGatewayStreamFailoverNeverLeaksErrorsOrEndsEarly(t *testing.T) {
	for _, frame := range []struct{ name, data string }{
		{"structured", `{"error":{"type":"server_error","code":123,"message":"Bearer provider-secret https://private.invalid/?key=secret"}}`},
		{"truncated", `{"error":{"message":"Bearer provider-secret https://private.invalid/?key=secret`},
	} {
		for _, business := range []bool{false, true} {
			t.Run(fmt.Sprint(frame.name, "/business=", business), func(t *testing.T) {
				logPath := captureGatewayDebugLog(t)
				fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte(": PING\n\ndata: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
					if business {
						_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"First\"}}]}\n\n"))
					}
					_, _ = fmt.Fprintf(w, "data: %s\n\n", frame.data)
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
				logged, err := os.ReadFile(logPath)
				require.NoError(t, err)
				assert.NotContains(t, string(logged), "provider-secret")
				assert.NotContains(t, string(logged), "private.invalid")
				if business {
					assert.Zero(t, secondCalls)
					assert.Contains(t, response.Body.String(), `"code":"upstream_error"`)
					assert.NotContains(t, response.Body.String(), "Recovered")
					assert.NotContains(t, response.Body.String(), "[DONE]")
				} else {
					assert.Equal(t, 1, secondCalls)
					assert.Contains(t, response.Body.String(), "Recovered")
					assert.Equal(t, 1, strings.Count(response.Body.String(), "[DONE]"))
					assert.Less(t, strings.Index(response.Body.String(), "Recovered"), strings.Index(response.Body.String(), "[DONE]"))
				}
			})
		}
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
	for _, test := range []struct {
		name, url, proxy, header string
		providerType             int
	}{
		{name: "parameters"},
		{name: "invalid escape", url: "http://%zz"},
		{name: "unsupported scheme", url: "ftp://upstream.invalid"},
		{name: "SDK HTTP fallback scheme", url: "ftp://upstream.invalid", providerType: constant.ChannelTypeVolcEngine},
		{name: "missing scheme", url: "upstream.invalid"},
		{name: "missing host", url: "http:///relative"},
		{name: "invalid proxy", proxy: "ftp://proxy.invalid"},
		{name: "invalid header name", header: `{"Bad Header":"value"}`},
		{name: "invalid header value", header: `{"X-Test":"value\r\nInjected: true"}`},
		{name: "invalid host override", header: `{"Host":"bad host"}`},
		{name: "invalid destination port", url: "http://127.0.0.1:65536"},
		{name: "zero destination port", url: "http://127.0.0.1:0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid prepared candidate must not be sent") })
			if test.providerType != 0 {
				require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", test.providerType).Error)
			}
			if test.url != "" {
				require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("base_url", test.url).Error)
			} else if test.proxy != "" {
				setting, err := common.Marshal(map[string]string{"proxy": test.proxy})
				require.NoError(t, err)
				require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("setting", string(setting)).Error)
			} else if test.header != "" {
				require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("header_override", test.header).Error)
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
	for _, frame := range []struct{ name, data string }{
		{"error envelope", "data: {\"error\":{\"code\":\"content_filter\",\"message\":\"private-secret\"}}\n\n"},
		{"finish reason", "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"content_filter\"}]}\n\n"},
	} {
		t.Run(frame.name, func(t *testing.T) {
			sent := make(chan struct{})
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(frame.data))
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
		})
	}
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

func TestTextGatewayNativeResponseAccounting(t *testing.T) {
	for _, test := range []struct {
		name, body, source string
		provider           int
		key, upstreamModel string
		stream             bool
		status, cozeStatus string
		httpStatus         int
		upstreamStatus     int
		prompt, completion int64
		charge, cost       int64
	}{
		{name: "reported", body: `{"response_id":"native","text":"Hello","finish_reason":"COMPLETE","meta":{"billed_units":{"input_tokens":10,"output_tokens":5}}}`, source: "upstream", prompt: 10, completion: 5, charge: 36000, cost: 20000},
		{name: "explicit zero", body: `{"text":"Hello","meta":{"billed_units":{"input_tokens":0,"output_tokens":0}}}`, source: "upstream"},
		{name: "Zhipu invalid total preserves reported failure cost", provider: constant.ChannelTypeZhipu, key: "test.secret", status: "failed", source: "none", httpStatus: http.StatusBadGateway, body: `{"success":false,"data":{"usage":{"prompt_tokens":0,"completion_tokens":1000,"total_tokens":-1}}}`, cost: 2000000},
		{name: "Zhipu overflowing total preserves valid counters", provider: constant.ChannelTypeZhipu, key: "test.secret", body: `{"success":true,"data":{"choices":[{"role":"assistant","content":"Hello"}],"usage":{"prompt_tokens":0,"completion_tokens":5,"total_tokens":2147483648}}}`, source: "upstream", completion: 5, charge: 18000, cost: 10000},
		{name: "OpenAI invalid total preserves valid counters", provider: constant.ChannelTypeOpenAI, body: `{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":5,"total_tokens":-1}}`, source: "upstream", completion: 5, charge: 18000, cost: 10000},
		{name: "missing input retains output", body: `{"text":"Hello","meta":{"billed_units":{"output_tokens":1000}}}`, source: "estimated", prompt: 9, completion: 1000, charge: 3616200, cost: 2009000},
		{name: "OpenAI missing input retains output beyond budget", provider: constant.ChannelTypeOpenAI, body: `{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"completion_tokens":1000}}`, source: "estimated", prompt: 9, completion: 1000, charge: 3616200, cost: 2009000},
		{name: "Cohere HTTP failure preserves cost", upstreamStatus: http.StatusServiceUnavailable, httpStatus: http.StatusServiceUnavailable, status: "failed", source: "none", body: `{"text":"Hello","meta":{"billed_units":{"input_tokens":10,"output_tokens":5}}}`, cost: 20000},
		{name: "Cohere interrupted stream", stream: true, status: "partial", body: "{\"text\":\"Hello\"}\n", source: "estimated", prompt: 9, completion: 1, charge: 19800, cost: 11000},
		{name: "Cohere failed terminal", stream: true, status: "partial", body: "{\"text\":\"Hello\"}\n{\"is_finished\":true,\"finish_reason\":\"ERROR\",\"response\":{\"meta\":{\"billed_units\":{\"input_tokens\":10,\"output_tokens\":5}}}}\n", source: "estimated", prompt: 10, completion: 1, charge: 21600, cost: 20000},
		{name: "Cohere invalid input retains output", body: `{"text":"Hello","meta":{"billed_units":{"input_tokens":-1,"output_tokens":5}}}`, source: "estimated", prompt: 9, completion: 5, charge: 34200, cost: 19000},
		{name: "Dify error reports cost", provider: constant.ChannelTypeDify, stream: true, status: "failed", source: "none", body: "data: {\"event\":\"error\",\"metadata\":{\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}}\n\n", cost: 20000},
		{name: "Dify error after output preserves counters", provider: constant.ChannelTypeDify, stream: true, status: "partial", source: "estimated", body: "data: {\"event\":\"message\",\"answer\":\"Hello\",\"metadata\":{\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":5}}}\n\ndata: {\"event\":\"error\"}\n\n", completion: 1, charge: 3600, cost: 10000},
		{name: "Ollama HTTP failure preserves cost", provider: constant.ChannelTypeOllama, upstreamStatus: http.StatusServiceUnavailable, httpStatus: http.StatusServiceUnavailable, status: "failed", source: "none", body: `{"message":{"role":"assistant","content":"Hello"},"prompt_eval_count":10,"eval_count":5}`, cost: 20000},
		{name: "Coze list failure preserves cost", provider: constant.ChannelTypeCoze, status: "failed", source: "none", httpStatus: http.StatusBadGateway, body: `{"code":1,"data":[],"msg":"Hello"}`, cost: 20000},
		{name: "Coze polling failure preserves cost", provider: constant.ChannelTypeCoze, status: "failed", cozeStatus: "failed", source: "none", httpStatus: http.StatusBadGateway, body: `{}`, cost: 20000},
		{name: "Zhipu failed terminal preserves counters", provider: constant.ChannelTypeZhipu, key: "test.secret", stream: true, status: "partial", body: "data:Hello\nmeta:{\"task_status\":\"FAIL\",\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n", source: "estimated", prompt: 10, completion: 1, charge: 21600, cost: 20000},
		{name: "Tencent interrupted stream", provider: constant.ChannelTypeTencent, key: "1|test-id|test-secret", stream: true, status: "partial", body: "data: {\"Choices\":[{\"Delta\":{\"Content\":\"Hello\"}}],\"Usage\":{\"PromptTokens\":0}}\n\n", source: "estimated", completion: 1, charge: 3600, cost: 2000},
		{name: "Cohere stream", stream: true, body: "{\"event_type\":\"text-generation\",\"text\":\"Hello\"}\n{\"is_finished\":true,\"finish_reason\":\"COMPLETE\",\"response\":{\"meta\":{\"billed_units\":{\"input_tokens\":10,\"output_tokens\":5}}}}\n", source: "upstream", prompt: 10, completion: 5, charge: 36000, cost: 20000},
		{name: "Baidu JSON", provider: constant.ChannelTypeBaidu, key: "test-key|test-secret", body: `{"result":"Hello","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, source: "upstream", prompt: 10, completion: 5, charge: 36000, cost: 20000},
		{name: "Baidu stream", provider: constant.ChannelTypeBaidu, key: "test-key|test-secret", stream: true, body: "data: {\"result\":\"Hello\",\"is_end\":true,\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n", source: "upstream", prompt: 10, completion: 5, charge: 36000, cost: 20000},
		{name: "PaLM JSON", provider: constant.ChannelTypePaLM, body: `{"candidates":[{"content":"Hello"}]}`, source: "estimated", prompt: 9, completion: 1, charge: 19800, cost: 11000},
		{name: "PaLM stream", provider: constant.ChannelTypePaLM, stream: true, body: `{"candidates":[{"content":"Hello"}]}`, source: "estimated", prompt: 9, completion: 1, charge: 19800, cost: 11000},
		{name: "Zhipu JSON", provider: constant.ChannelTypeZhipu, key: "test.secret", body: `{"success":true,"data":{"choices":[{"role":"assistant","content":"Hello"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}}`, source: "upstream", prompt: 10, completion: 5, charge: 36000, cost: 20000},
		{name: "Zhipu stream", provider: constant.ChannelTypeZhipu, key: "test.secret", stream: true, body: "data:Hello\nmeta:{\"task_status\":\"SUCCESS\",\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n", source: "upstream", prompt: 10, completion: 5, charge: 36000, cost: 20000},
		{name: "Cloudflare JSON", provider: constant.ChannelCloudflare, body: `{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, source: "upstream", prompt: 10, completion: 5, charge: 36000, cost: 20000},
		{name: "Cloudflare stream", provider: constant.ChannelCloudflare, stream: true, body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\ndata: [DONE]\n\n", source: "upstream", prompt: 10, completion: 5, charge: 36000, cost: 20000},
		{name: "Tencent TC3 JSON", provider: constant.ChannelTypeTencent, key: "1|test-id|test-secret", body: `{"Response":{"Choices":[{"Message":{"Role":"assistant","Content":"Hello"},"FinishReason":"stop"}],"Usage":{"PromptTokens":10,"CompletionTokens":5,"TotalTokens":15}}}`, source: "upstream", prompt: 10, completion: 5, charge: 36000, cost: 20000},
		{name: "Tencent TC3 stream", provider: constant.ChannelTypeTencent, key: "1|test-id|test-secret", stream: true, body: "data: {\"Choices\":[{\"Delta\":{\"Role\":\"assistant\",\"Content\":\"Hello\"},\"FinishReason\":\"stop\"}],\"Usage\":{\"PromptTokens\":10,\"CompletionTokens\":5,\"TotalTokens\":15}}\n\n", source: "upstream", prompt: 10, completion: 5, charge: 36000, cost: 20000},
		{name: "Tencent TokenHub JSON", provider: constant.ChannelTypeTencent, body: `{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, source: "upstream", prompt: 10, completion: 5, charge: 36000, cost: 20000},
		{name: "Coze JSON", provider: constant.ChannelTypeCoze, body: `{"code":0,"data":[{"type":"answer","role":"assistant","content":"Hello"}]}`, source: "upstream", prompt: 10, completion: 5, charge: 36000, cost: 20000},
		{name: "Coze stream", provider: constant.ChannelTypeCoze, stream: true, body: "event: conversation.message.delta\ndata: {\"content\":\"Hello\"}\n\nevent: conversation.chat.completed\ndata: {\"usage\":{\"input_count\":10,\"output_count\":5,\"token_count\":15}}\n\n", source: "upstream", prompt: 10, completion: 5, charge: 36000, cost: 20000},
		{name: "Nova missing usage", provider: constant.ChannelTypeAws, key: "test-access|test-secret|us-east-1", upstreamModel: "amazon.nova-lite-v1:0", body: `{"output":{"message":{"content":[{"text":"Hello"}]}},"stopReason":"end_turn"}`, source: "estimated", prompt: 9, completion: 1, charge: 19800, cost: 11000},
		{name: "Nova missing input", provider: constant.ChannelTypeAws, key: "test-access|test-secret|us-east-1", upstreamModel: "amazon.nova-lite-v1:0", body: `{"output":{"message":{"content":[{"text":"Hello"}]}},"usage":{"outputTokens":5},"stopReason":"end_turn"}`, source: "estimated", prompt: 9, completion: 5, charge: 34200, cost: 19000},
		{name: "Nova stream missing output", provider: constant.ChannelTypeAws, key: "test-access|test-secret|us-east-1", upstreamModel: "amazon.nova-lite-v1:0", stream: true, body: `{"output":{"message":{"content":[{"text":"Hello"}]}},"usage":{"inputTokens":0},"stopReason":"end_turn"}`, source: "estimated", completion: 1, charge: 3600, cost: 2000},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if test.stream && test.provider != constant.ChannelTypePaLM && test.provider != constant.ChannelTypeAws {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				if test.provider == constant.ChannelTypeCoze && !test.stream {
					switch r.URL.Path {
					case "/v3/chat":
						_, _ = w.Write([]byte(`{"code":0,"data":{"id":"chat","conversation_id":"conversation"}}`))
						return
					case "/v3/chat/retrieve":
						status := test.cozeStatus
						if status == "" {
							status = "completed"
						}
						_, _ = fmt.Fprintf(w, `{"code":0,"data":{"status":%q,"usage":{"input_count":10,"output_count":5,"token_count":15}}}`, status)
						return
					}
				}
				if test.upstreamStatus != 0 {
					w.WriteHeader(test.upstreamStatus)
				}
				_, _ = w.Write([]byte(test.body))
			})
			providerType := test.provider
			if providerType == 0 {
				providerType = constant.ChannelTypeCohere
			}
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", providerType).Error)
			if test.key != "" {
				require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("key", test.key).Error)
			}
			if test.upstreamModel != "" {
				require.NoError(t, model.DB.Model(fixture.channel).Update("upstream_model_name", test.upstreamModel).Error)
			}
			if providerType == constant.ChannelTypeAws || providerType == constant.ChannelTypeBaidu {
				var provider model.Provider
				require.NoError(t, model.DB.First(&provider, fixture.channel.ProviderID).Error)
				endpoint, err := url.Parse(*provider.BaseURL)
				require.NoError(t, err)
				client := service.GetHttpClient()
				transport := client.Transport
				t.Cleanup(func() { client.Transport = transport })
				client.Transport = channelProbeRoundTripper(func(request *http.Request) (*http.Response, error) {
					if request.URL.Host == "aip.baidubce.com" {
						return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"access_token":"native-test-token","expires_in":7200}`))}, nil
					}
					copyRequest := request.Clone(request.Context())
					copyURL := *request.URL
					copyURL.Scheme, copyURL.Host = endpoint.Scheme, endpoint.Host
					copyRequest.URL, copyRequest.Host = &copyURL, endpoint.Host
					return transport.RoundTrip(copyRequest)
				})
			}
			finished := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fixture.engine.ServeHTTP(w, r)
				close(finished)
			}))
			t.Cleanup(server.Close)
			limit := `,"max_tokens":16`
			if providerType == constant.ChannelTypePaLM {
				limit = ""
			}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"gateway-alias","stream":%t,"user":"gateway-user","messages":[{"role":"user","content":"Hello"}]%s}`, test.stream, limit)))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
			request.Header.Set("Content-Type", "application/json")
			response, err := server.Client().Do(request)
			require.NoError(t, err)
			body, err := io.ReadAll(response.Body)
			require.NoError(t, response.Body.Close())
			require.NoError(t, err)
			<-finished
			statusCode, status := test.httpStatus, test.status
			if statusCode == 0 {
				statusCode = http.StatusOK
			}
			if status == "" {
				status = "success"
			}
			require.Equal(t, statusCode, response.StatusCode, string(body))
			if status == "failed" {
				assert.NotContains(t, string(body), "Hello", "Provider failures must not be exposed as business output")
			} else {
				assert.Contains(t, string(body), "Hello")
			}
			record, err := model.GetGatewayRequest(t.Context(), response.Header.Get(common.RequestIdKey), fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, status, record.Status)
			assert.Equal(t, test.source, record.UsageSource)
			assert.Equal(t, test.prompt, record.Usage.PromptTokens)
			assert.Equal(t, test.completion, record.Usage.CompletionTokens)
			assert.Equal(t, test.charge, record.Receivable.Nanos)
			attempts, _, err := model.ListGatewayAttempts(t.Context(), record.ID, 0, 10)
			require.NoError(t, err)
			require.Len(t, attempts, 1)
			assert.Equal(t, test.cost, attempts[0].Cost.Nanos)
		})
	}
}

func TestTextGatewayCozeCompletedUsageSurvivesMessageFetchFailure(t *testing.T) {
	for _, failure := range []string{"connection", "cancellation"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v3/chat":
					_, _ = w.Write([]byte(`{"code":0,"data":{"id":"chat","conversation_id":"conversation"}}`))
				case "/v3/chat/retrieve":
					_, _ = w.Write([]byte(`{"code":0,"data":{"status":"completed","usage":{"input_count":10,"output_count":5,"token_count":15}}}`))
				case "/v3/chat/message/list":
					if failure == "cancellation" {
						cancel()
						<-r.Context().Done()
						return
					}
					connection, _, err := http.NewResponseController(w).Hijack()
					if assert.NoError(t, err) {
						assert.NoError(t, connection.Close())
					}
				default:
					t.Errorf("unexpected Coze endpoint %s", r.URL.Path)
				}
			})
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", constant.ChannelTypeCoze).Error)
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gateway-alias","user":"gateway-user","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)).WithContext(ctx)
			request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			fixture.engine.ServeHTTP(response, request)
			assert.NotContains(t, response.Body.String(), "Hello")
			record, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
			require.NoError(t, err)
			assert.Zero(t, record.Receivable.Nanos)
			attempts, _, err := model.ListGatewayAttempts(t.Context(), record.ID, 0, 10)
			require.NoError(t, err)
			require.Len(t, attempts, 1)
			assert.Equal(t, int64(10), attempts[0].Usage.PromptTokens)
			assert.Equal(t, int64(5), attempts[0].Usage.CompletionTokens)
			assert.Equal(t, int64(20000), attempts[0].Cost.Nanos)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, int64(1000000000), wallet.Balance.Nanos)
			reconciliation, err := model.ReconcileWallets(100)
			require.NoError(t, err)
			assert.Empty(t, reconciliation.Mismatches)
		})
	}
}

func TestTextGatewaySparkPreservesNativeCounterPresence(t *testing.T) {
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 60
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	for _, test := range []struct {
		name, counters     string
		before             string
		failed             bool
		prompt, completion int
	}{
		{name: "native failure reports usage", failed: true, counters: `,"usage":{"text":{"prompt_tokens":10,"completion_tokens":5}}`, prompt: 10, completion: 5},
		{name: "native failure retains prior usage", failed: true, before: `{"header":{"code":0},"payload":{"choices":{"status":1,"text":[{"role":"assistant","content":"Hello"}]},"usage":{"text":{"prompt_tokens":10,"completion_tokens":5}}}}` + "\n", prompt: 10, completion: 5},
		{name: "missing usage", prompt: 9, completion: 1},
		{name: "zero input missing output", counters: `,"usage":{"text":{"prompt_tokens":0}}`, completion: 1},
		{name: "missing input retains output", counters: `,"usage":{"text":{"completion_tokens":1000}}`, prompt: 9, completion: 1000},
		{name: "reported zero", counters: `,"usage":{"text":{"prompt_tokens":0,"completion_tokens":0}}`},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", test.name, stream), func(t *testing.T) {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
				info := &relaycommon.RelayInfo{RelayFormat: relaytypes.RelayFormatOpenAI, RelayMode: relayconstant.RelayModeChatCompletions, IsStream: stream, TokenizerModel: "gpt-4o-mini", ResponseFacts: &relaycommon.ResponseFacts{}, ProviderMeta: &relaycommon.ProviderMeta{ChannelType: constant.ChannelTypeXunfei, UpstreamModelName: "SparkDesk"}}
				info.SetEstimatePromptTokens(9)
				common.SetContextKey(c, constant.ContextKeyResponseFacts, info.ResponseFacts)
				adaptor, err := relay.GetChannelAdaptor(constant.APITypeXunfei, protocol.OpenAIChat)
				require.NoError(t, err)
				code := 0
				if test.failed {
					code = 10014
				}
				body := test.before + fmt.Sprintf(`{"header":{"code":%d},"payload":{"choices":{"status":2,"text":[{"role":"assistant","content":"Hello"}]}%s}}`+"\n", code, test.counters)
				response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
				result, apiErr := adaptor.DoResponse(c, response, info)
				if test.failed && !stream {
					require.NotNil(t, apiErr)
				} else {
					require.Nil(t, apiErr)
				}
				usage, ok := result.(*dto.Usage)
				require.True(t, ok, "reported Provider usage survives native errors")
				assert.Equal(t, test.prompt, usage.PromptTokens)
				assert.Equal(t, test.completion, usage.CompletionTokens)
				if test.failed && (!stream || test.before == "") {
					assert.NotContains(t, recorder.Body.String(), "Hello")
				} else {
					assert.Contains(t, recorder.Body.String(), "Hello")
				}
				if test.failed && stream {
					assert.False(t, info.StreamStatus.IsNormalEnd())
				}
			})
		}
	}
}

func TestTextGatewayDifyRemoteImageUsesNativeCategory(t *testing.T) {
	previous := constant.GetMediaToken
	constant.GetMediaToken = false
	t.Cleanup(func() { constant.GetMediaToken = previous })
	var sent struct {
		Files []map[string]any `json:"files"`
	}
	calls := 0
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.NoError(t, common.DecodeJson(r.Body, &sent))
		if len(sent.Files) != 1 || sent.Files[0]["transfer_method"] != "remote_url" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if _, misspelled := sent.Files[0]["transfer_mode"]; misspelled {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answer":"Hello","metadata":{"usage":{"prompt_tokens":10,"completion_tokens":1}}}`))
	})
	require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", constant.ChannelTypeDify).Error)
	caps := capability.Set{InputModalities: []capability.Modality{capability.Text, capability.Image}, OutputModalities: []capability.Modality{capability.Text}}
	require.NoError(t, model.DB.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Update("capabilities", caps).Error)
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":[{"type":"text","text":"Hello"},{"type":"image_url","image_url":{"url":"https://example.invalid/image.png"}}]}],"max_tokens":16}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, 1, calls)
	require.Len(t, sent.Files, 1)
	assert.Equal(t, "image", sent.Files[0]["type"])
	assert.Equal(t, "remote_url", sent.Files[0]["transfer_method"])
	assert.Equal(t, "https://example.invalid/image.png", sent.Files[0]["url"])
	assert.NotContains(t, sent.Files[0], "transfer_mode")
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
	assertTextGatewayLedger(t, requests[0], 0)
}

func TestTextGatewayJSONAndStreamUsage(t *testing.T) {
	for _, test := range []struct {
		name          string
		stream        bool
		body          string
		status        string
		source        string
		prompt        int64
		completion    int64
		receivable    int64
		cost          int64
		attemptSource string
	}{
		{name: "JSON reported zero", body: `{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`, status: "success", source: "upstream"},
		{name: "JSON missing usage", body: `{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}]}`, status: "success", source: "estimated", prompt: 9, completion: 1, receivable: 19800, cost: 11000},
		{name: "JSON empty usage object", body: `{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{}}`, status: "success", source: "estimated", prompt: 9, completion: 1, receivable: 19800, cost: 11000},
		{name: "SSE cumulative usage before trailing frames", stream: true, body: "data: {\"id\":\"stream\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\ndata: {\"choices\":[]}\n\ndata: {\"choices\":[]}\n\ndata: [DONE]\n\n", status: "success", source: "upstream", prompt: 10, completion: 5, receivable: 36000, cost: 20000},
		{name: "SSE reported zero", stream: true, body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0,\"total_tokens\":0}}\n\ndata: [DONE]\n\n", status: "success", source: "upstream"},
		{name: "SSE missing usage", stream: true, body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", status: "success", source: "estimated", prompt: 9, completion: 1, receivable: 19800, cost: 11000},
		{name: "JSON explicit zero input estimates only output", body: `{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":0}}`, status: "success", source: "estimated", completion: 1, receivable: 3600, cost: 2000},
		{name: "JSON explicit zero output estimates only input", body: `{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"completion_tokens":0}}`, status: "success", source: "estimated", prompt: 9, receivable: 16200, cost: 9000},
		{name: "JSON reasoning is generated output", body: `{"choices":[{"message":{"role":"assistant","reasoning_content":"Hello"},"finish_reason":"stop"}]}`, status: "success", source: "estimated", prompt: 9, completion: 1, receivable: 19800, cost: 11000},
		{name: "SSE interrupted reported tool uses complete local estimate", stream: true, body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_test\",\"type\":\"function\",\"function\":{\"name\":\"Hello\",\"arguments\":\"\"}}]}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":100}}\n\n", status: "partial", source: "estimated", attemptSource: "upstream", prompt: 10, completion: 8, receivable: 46800, cost: 210000},
		{name: "SSE interrupted tool keeps complete local estimate", stream: true, body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_test\",\"type\":\"function\",\"function\":{\"name\":\"Hello\",\"arguments\":\"\"}}]}}]}\n\n", status: "partial", source: "estimated", prompt: 9, completion: 8, receivable: 45000, cost: 25000},
		{name: "SSE interrupted explicit zero stays authoritative", stream: true, body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}],\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0,\"total_tokens\":0}}\n\n", status: "partial", source: "upstream"},
		{name: "SSE explicit upstream failure bills delivered output", stream: true, body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\ndata: {\"error\":{\"code\":\"overloaded\",\"type\":\"server_error\",\"status\":503}}\n\n", status: "partial", source: "estimated", attemptSource: "upstream", prompt: 10, completion: 1, receivable: 21600, cost: 20000},
		{name: "SSE interrupted reported usage bills delivered output", stream: true, body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n", status: "partial", source: "estimated", attemptSource: "upstream", prompt: 10, completion: 1, receivable: 21600, cost: 20000},
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
			attemptSource := test.attemptSource
			if attemptSource == "" {
				attemptSource = test.source
			}
			assert.Equal(t, attemptSource, attempts[0].UsageSource)
			assert.Equal(t, test.cost, attempts[0].Cost.Nanos)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, int64(1_000_000_000)-test.receivable, wallet.Balance.Nanos)
			assertTextGatewayLedger(t, requests[0], test.cost)
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
	for _, test := range []struct {
		name, body, source string
		expression         string
		provider           int
		charge, cost       int64
	}{
		{name: "OpenAI partial usage", provider: constant.ChannelTypeOpenAI, source: "estimated", charge: 19800, cost: 209000, body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}],\"usage\":{\"completion_tokens\":100}}\n\ndata: {\"choices\":[]}\n\n"},
		{name: "OpenAI missing usage", provider: constant.ChannelTypeOpenAI, source: "estimated", charge: 19800, cost: 11000, body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}]}\n\ndata: {\"choices\":[]}\n\n"},
		{name: "OpenAI fixed cancellation refunds", provider: constant.ChannelTypeOpenAI, expression: `tier("request",fixed(0.01))`, source: "upstream", cost: 20000, body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\ndata: {\"choices\":[]}\n\n"},
		{name: "OpenAI", provider: constant.ChannelTypeOpenAI, source: "upstream", charge: 36000, cost: 20000, body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\ndata: {\"choices\":[]}\n\n"},
		{name: "Cohere", provider: constant.ChannelTypeCohere, source: "estimated", charge: 19800, cost: 11000, body: "{\"text\":\"Hello\"}\n"},
		{name: "Dify", provider: constant.ChannelTypeDify, source: "upstream", charge: 36000, cost: 20000, body: "data: {\"event\":\"message\",\"answer\":\"Hello\",\"metadata\":{\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}}\n\n"},
	} {
		for _, output := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/output=%t", test.name, output), func(t *testing.T) {
				entered, cancelled := make(chan struct{}), make(chan struct{})
				fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					close(entered)
					if output {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = w.Write([]byte(test.body))
						w.(http.Flusher).Flush()
					}
					<-r.Context().Done()
					close(cancelled)
				})
				require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", test.provider).Error)
				if test.expression != "" {
					price := &model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: test.expression, EffectiveAt: clock.Now().Unix()}
					require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
				}
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
				health, err := service.ChannelHealth.Snapshots(t.Context(), []int{fixture.channel.ID})
				require.NoError(t, err)
				assert.Zero(t, health[fixture.channel.ID].Samples)
				assert.Zero(t, health[fixture.channel.ID].Failures)
				wallet, err := model.GetUserWallet(fixture.user.Id)
				require.NoError(t, err)
				if output {
					assertTextGatewayLedger(t, requests[0], test.cost)
				} else {
					assertTextGatewayLedger(t, requests[0], 0)
				}
				if output {
					assert.Equal(t, test.source, attempts[0].UsageSource)
					assert.Equal(t, test.charge, requests[0].Receivable.Nanos)
					assert.Equal(t, test.cost, attempts[0].Cost.Nanos)
					assert.Equal(t, int64(1000000000)-test.charge, wallet.Balance.Nanos)
				} else {
					assert.Zero(t, requests[0].Receivable.Nanos)
					assert.Zero(t, attempts[0].Cost.Nanos)
					assert.Equal(t, int64(1000000000), wallet.Balance.Nanos)
				}
			})
		}
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
		name, expression, body, status, source, errorClass string
		provider                                           int
		upstreamStatus                                     int
		stream                                             bool
		charge, cost                                       int64
	}{
		{name: "explicit free request", expression: `tier("request",fixed(0))`, body: `{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, status: "success", source: "upstream", cost: 20000},
		{name: "fixed successful request", expression: `tier("request",fixed(0.01))`, body: `{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, status: "success", source: "upstream", charge: 9000000, cost: 20000},
		{name: "fixed interrupted condition failure still refunds", expression: `(len == 9 || 1 / (len - 10) > 0) ? tier("a",fixed(0.01)) : tier("b",fixed(0.02))`, stream: true, body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n", status: "partial", source: "upstream", cost: 20000},
		{name: "fixed interrupted overflowing branch still refunds", expression: `len == 10 ? tier("overflow",fixed(10000000000)) : tier("request",fixed(0.01))`, stream: true, body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n", status: "partial", source: "upstream", cost: 20000},
		{name: "fixed interrupted stream refunds", expression: `tier("request",fixed(0.01))`, stream: true, body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}]}\n\n", status: "partial", source: "estimated", cost: 11000},
		{name: "fixed safety stream refunds", expression: `tier("request",fixed(0.01))`, stream: true, body: "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"content_filter\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\ndata: [DONE]\n\n", status: "partial", source: "upstream", errorClass: "content_safety", cost: 20000},
		{name: "fixed safety JSON refunds", expression: `tier("request",fixed(0.01))`, body: `{"choices":[{"message":{"content":"Hello"},"finish_reason":"content_filter"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, status: "partial", source: "upstream", errorClass: "content_safety", cost: 20000},
		{name: "fixed native interrupted stream refunds", provider: constant.ChannelTypeCohere, expression: `tier("request",fixed(0.01))`, stream: true, body: "{\"event_type\":\"text-generation\",\"text\":\"Hello\"}\n", status: "partial", source: "estimated", errorClass: "stream_interrupted", cost: 11000},
		{name: "fixed native safety JSON refunds", provider: constant.ChannelTypeCohere, expression: `tier("request",fixed(0.01))`, body: `{"text":"Hello","finish_reason":"ERROR_TOXIC","meta":{"billed_units":{"input_tokens":10,"output_tokens":5}}}`, status: "partial", source: "upstream", errorClass: "content_safety", cost: 20000},
		{name: "fixed native safety stream refunds", provider: constant.ChannelTypeCohere, expression: `tier("request",fixed(0.01))`, stream: true, body: "{\"event_type\":\"text-generation\",\"text\":\"Hello\"}\n{\"is_finished\":true,\"finish_reason\":\"ERROR_TOXIC\",\"response\":{\"meta\":{\"billed_units\":{\"input_tokens\":10,\"output_tokens\":5}}}}\n", status: "partial", source: "upstream", errorClass: "content_safety", cost: 20000},
		{name: "fixed Tencent safety stream refunds", provider: constant.ChannelTypeTencent, expression: `tier("request",fixed(0.01))`, stream: true, body: "data: {\"Choices\":[{\"Delta\":{\"Content\":\"Hello\"}}]}\n\ndata: {\"Choices\":[{\"Delta\":{},\"FinishReason\":\"content_filter\"}],\"Usage\":{\"PromptTokens\":10,\"CompletionTokens\":5,\"TotalTokens\":15}}\n\n", status: "partial", source: "upstream", errorClass: "content_safety", cost: 20000},
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
			if test.provider != 0 {
				require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", test.provider).Error)
				if test.provider == constant.ChannelTypeTencent {
					require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("key", strings.Join([]string{"1", "test-id", "test-secret"}, "|")).Error)
				}
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
			if test.errorClass != "" {
				assert.Equal(t, test.errorClass, attempts[0].ErrorClass)
				if test.stream {
					assert.NotContains(t, response.Body.String(), "[DONE]")
				}
			}
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, int64(1000000000)-test.charge, wallet.Balance.Nanos)
			assertTextGatewayLedger(t, request, test.cost)
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
	for _, query := range []string{"page=0", "page_size=-1", "page_size=101", "model_offering_id=0", "started_after=-1", "status=unknown"} {
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
	nilError bool
}

func (w gatewayPrefixOnlyWriter) Write(body []byte) (int, error) {
	n, _ := w.ResponseRecorder.Write(body[:min(8, len(body))])
	if w.nilError {
		return n, nil
	}
	return n, io.ErrClosedPipe
}

func TestTextGatewayMetadataPrefixIsNotDeliveredBusinessOutput(t *testing.T) {
	for _, test := range []struct {
		name     string
		provider int
		body     string
		stream   bool
		nilError bool
	}{
		{name: "OpenAI stream", provider: constant.ChannelTypeOpenAI, stream: true, body: "data: {\"id\":\"chat-test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\ndata: [DONE]\n\n"},
		{name: "OpenAI", provider: constant.ChannelTypeOpenAI, body: `{"id":"chat-test","object":"chat.completion","model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`},
		{name: "Ollama", provider: constant.ChannelTypeOllama, body: `{"model":"upstream-model","message":{"role":"assistant","content":"Hello"},"done":true,"prompt_eval_count":10,"eval_count":5}`},
		{name: "Dify", provider: constant.ChannelTypeDify, body: `{"answer":"Hello","conversation_id":"chat-test","metadata":{"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}}`},
		{name: "Dify short write without error", provider: constant.ChannelTypeDify, nilError: true, body: `{"answer":"Hello","conversation_id":"chat-test","metadata":{"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}}`},
		{name: "Cohere", provider: constant.ChannelTypeCohere, body: `{"text":"Hello","meta":{"billed_units":{"input_tokens":10,"output_tokens":5}}}`},
		{name: "Dify stream", provider: constant.ChannelTypeDify, stream: true, body: "data: {\"event\":\"message\",\"answer\":\"Hello\",\"metadata\":{\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}}\n\n"},
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
			fixture.engine.ServeHTTP(gatewayPrefixOnlyWriter{ResponseRecorder: recorder, nilError: test.nilError}, request)
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

func TestTextGatewayDifyUploadsAfterReservation(t *testing.T) {
	var imageData bytes.Buffer
	require.NoError(t, png.Encode(&imageData, image.NewRGBA(image.Rect(0, 0, 1, 1))))
	imageURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(imageData.Bytes())
	for _, test := range []struct {
		name                                          string
		insufficient, uploadFails, removed, cancelled bool
	}{
		{name: "insufficient wallet", insufficient: true},
		{name: "funded inline image"},
		{name: "upload failure", uploadFails: true},
		{name: "override removes inline image", removed: true},
		{name: "cancelled upload", cancelled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var fixture textGatewayFixture
			var phases []string
			var sent map[string]any
			uploads, chats := 0, 0
			uploadStarted, uploadCancelled, releaseUpload := make(chan struct{}), make(chan struct{}), make(chan struct{})
			defer close(releaseUpload)
			fixture = newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				var billing model.GatewayBilling
				if err := model.DB.Where("user_id = ?", fixture.user.Id).First(&billing).Error; err == nil {
					phases = append(phases, billing.Phase)
				} else {
					phases = append(phases, "missing reservation")
				}
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/files/upload" {
					uploads++
					if test.cancelled {
						_, _ = io.Copy(io.Discard, r.Body)
						close(uploadStarted)
						select {
						case <-r.Context().Done():
							close(uploadCancelled)
						case <-releaseUpload:
						}
						return
					}
					if test.uploadFails {
						w.WriteHeader(http.StatusServiceUnavailable)
						_, _ = w.Write([]byte(`{"error":{"message":"Provider diagnostic"}}`))
					} else {
						_, _ = w.Write([]byte(`{"id":"reserved-image"}`))
					}
					return
				}
				chats++
				require.NoError(t, common.DecodeJson(r.Body, &sent))
				if !test.removed {
					files, ok := sent["files"].([]any)
					if !ok || len(files) != 1 {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					file, ok := files[0].(map[string]any)
					_, misspelled := file["transfer_mode"]
					if !ok || misspelled || file["transfer_method"] != "local_file" {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
				}
				_, _ = w.Write([]byte(`{"answer":"Hello","metadata":{"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}}`))
			})
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", constant.ChannelTypeDify).Error)
			caps := capability.Set{InputModalities: []capability.Modality{capability.Text, capability.Image}, OutputModalities: []capability.Modality{capability.Text}}
			require.NoError(t, model.DB.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Update("capabilities", caps).Error)
			if test.insufficient {
				sale := &model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: `tier("request",fixed(2))`, EffectiveAt: clock.Now().Unix()}
				require.NoError(t, model.CreatePriceVersion(t.Context(), sale, fixture.sale.CreatedBy))
			}
			purchase := &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: `param("files.0.upload_file_id") == "reserved-image" ? tier("sent",fixed(0.003)) : tier("wrong",fixed(0.1))`, EffectiveAt: clock.Now().Unix()}
			require.NoError(t, model.CreatePriceVersion(t.Context(), purchase, fixture.purchase.CreatedBy))
			if test.removed {
				require.NoError(t, model.DB.Model(fixture.channel).Update("parameter_overrides", `{"files":[]}`).Error)
			}
			body := fmt.Sprintf(`{"model":"gateway-alias","messages":[{"role":"user","content":[{"type":"text","text":"Hello"},{"type":"image_url","image_url":{"url":%q}}]}],"max_tokens":16}`, imageURL)
			var response *httptest.ResponseRecorder
			if test.cancelled {
				waitContext, stop := context.WithTimeout(t.Context(), 10*time.Second)
				defer stop()
				requestContext, cancel := context.WithCancel(waitContext)
				defer cancel()
				request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(requestContext)
				request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
				request.Header.Set("Content-Type", "application/json")
				response = httptest.NewRecorder()
				finished := make(chan struct{})
				go func() {
					fixture.engine.ServeHTTP(response, request)
					close(finished)
				}()
				select {
				case <-uploadStarted:
				case <-waitContext.Done():
					require.FailNow(t, "Dify upload did not start")
				}
				cancel()
				select {
				case <-uploadCancelled:
				case <-waitContext.Done():
					require.FailNow(t, "Dify upload ignored request cancellation")
				}
				select {
				case <-finished:
				case <-waitContext.Done():
					require.FailNow(t, "cancelled gateway request did not finish")
				}
			} else {
				response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", body)
			}
			assert.NotContains(t, response.Body.String(), "Provider diagnostic")
			if test.insufficient {
				assert.Equal(t, http.StatusPaymentRequired, response.Code, response.Body.String())
				assert.Zero(t, uploads)
				assert.Zero(t, chats)
				return
			}
			if test.uploadFails || test.cancelled {
				if test.cancelled {
					assert.Empty(t, response.Body.String(), "a cancelled downstream receives no synthetic response")
				} else {
					require.Equal(t, http.StatusBadGateway, response.Code, response.Body.String())
				}
				assert.Zero(t, chats)
				assert.Equal(t, []string{"reserved"}, phases)
			} else {
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				assert.Equal(t, 1, chats)
				files := sent["files"].([]any)
				if test.removed {
					assert.Empty(t, files)
					assert.Equal(t, []string{"reserved"}, phases)
				} else {
					assert.Equal(t, []string{"reserved", "reserved"}, phases)
					require.Len(t, files, 1)
					assert.Equal(t, "reserved-image", files[0].(map[string]any)["upload_file_id"])
					assert.Equal(t, "local_file", files[0].(map[string]any)["transfer_method"])
					assert.NotContains(t, files[0], "transfer_mode")
					assert.NotContains(t, files[0], "url")
				}
			}
			if test.removed {
				assert.Zero(t, uploads)
			} else {
				assert.Equal(t, 1, uploads)
			}
			record, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
			require.NoError(t, err)
			attempts, _, err := model.ListGatewayAttempts(t.Context(), record.ID, 0, 10)
			require.NoError(t, err)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			if test.uploadFails || test.cancelled {
				assert.Empty(t, attempts, "failed preparation must not create a model Attempt")
				if test.cancelled {
					assert.Equal(t, "cancelled", record.Status)
				} else {
					assert.Equal(t, "failed", record.Status)
				}
				assert.Zero(t, record.Receivable.Nanos)
				assert.Equal(t, int64(1_000_000_000), wallet.Balance.Nanos)
			} else {
				require.Len(t, attempts, 1)
				assert.Equal(t, purchase.ID, attempts[0].PurchasePriceVersionID)
				assert.Equal(t, "1", attempts[0].AccountingFXRate)
				assert.Equal(t, "success", record.Status)
				assert.Equal(t, int64(36000), record.Receivable.Nanos)
				if test.removed {
					assert.Equal(t, int64(100_000_000), attempts[0].Cost.Nanos)
				} else {
					assert.Equal(t, int64(3_000_000), attempts[0].Cost.Nanos)
				}
				assert.Equal(t, int64(999_964_000), wallet.Balance.Nanos)
			}
		})
	}
}

func TestTextGatewayProcurementEvaluationCannotHoldFailedDeliveryFunds(t *testing.T) {
	calls := 0
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"overloaded"},"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":1}}`))
	})
	price := &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: `len == 9 ? tier("base",p + c * 2) : tier("invalid",p / (len - 10))`, EffectiveAt: clock.Now().Unix()}
	require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
	body := `{"model":"gateway-offering","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`
	failed := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", body)
	require.Equal(t, http.StatusServiceUnavailable, failed.Code, failed.Body.String())
	wallet, err := model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	assert.Equal(t, int64(1000000000), wallet.Balance.Nanos, "an unknown platform cost cannot consume the failed request's funds")
	resumed := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", body)
	assert.Equal(t, http.StatusOK, resumed.Code, resumed.Body.String())
	assert.Equal(t, 2, calls)
	id := failed.Header().Get(common.RequestIdKey)
	record, err := model.GetGatewayRequest(t.Context(), id, fixture.user.Id)
	require.NoError(t, err)
	assert.Zero(t, record.Receivable.Nanos)
	assert.False(t, record.SalePending, "a failed delivery has a known zero charge even when procurement is unresolved")
	adminAttempts := walletAPIRequest(t, fixture.engine, fixture.adminPAT, http.MethodGet, "/api/admin/v1/requests/"+id+"/attempts", "")
	require.True(t, adminAttempts.Success, adminAttempts.Message)
	adminAttempt := adminAttempts.Data["items"].([]any)[0].(map[string]any)
	assert.Equal(t, true, adminAttempt["cost_pending"])
	var entries []model.LedgerEntry
	require.NoError(t, model.DB.Where("request_id = ?", id).Find(&entries).Error)
	require.Len(t, entries, 2)
	var released int64
	for _, entry := range entries {
		released += entry.Amount.Nanos
	}
	assert.Zero(t, released)
}

func TestTextGatewayEvaluationFailureRetainsPendingAccounting(t *testing.T) {
	for _, test := range []struct {
		name, owner, expression string
		reservation             int64
	}{
		{"mixed pricing condition failure", model.PriceOwnerOffering, `(len == 9 || 1 / (len - 10) > 0) ? tier("request",fixed(0.01)) : tier("tokens",p + c)`, 9000000},
		{"sale overflow", model.PriceOwnerOffering, `(header("Content-Type") == "application/json" && len != 9) ? tier(header("Authorization"),p * (param("rate") ?? 1) + c * 10000000000000000) : tier("base",p * 2 + c * 4)`, 73800},
		{"sale evaluation", model.PriceOwnerOffering, `len == 9 ? tier("base",p * 2 + c * 4) : tier("invalid",p / (len - 10))`, 73800},
		{"purchase conversion overflow", model.PriceOwnerChannel, `len == 9 ? tier("base",p * 0.5 + c) : tier("expensive",p * 500000000000000)`, 73800},
		{"purchase overflow", model.PriceOwnerChannel, `len == 9 ? tier("base",p + c * 2) : tier(header("Authorization"),p * 1000000000000000 + c * 4)`, 73800},
		{"purchase evaluation", model.PriceOwnerChannel, `len == 9 ? tier("base",p + c * 2) : tier("invalid",p / (len - 10))`, 73800},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				if test.name == "purchase conversion overflow" && calls > 1 {
					_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":1}}`))
					return
				}
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":2,"cached_tokens_details":{"text_tokens":1,"image_tokens":1},"image_tokens":2},"completion_tokens_details":{"reasoning_tokens":1},"cost":{"credential":"quantitative-secret"},"usage_source":"quantitative-secret","usage_semantic":"quantitative-secret"}}`))
			})
			ownerID := fixture.offering.ID
			if test.owner == model.PriceOwnerChannel {
				ownerID = fixture.channel.ID
			}
			currency := money.Currency("USD")
			if test.name == "purchase conversion overflow" {
				currency = "EUR"
				_, err := model.SaveFXRate(t.Context(), "EUR", "USD", "2", "0.02", fixture.sale.CreatedBy)
				require.NoError(t, err)
			}
			price := &model.PriceVersion{OwnerType: test.owner, OwnerID: ownerID, Currency: currency, Expression: test.expression, EffectiveAt: clock.Now().Unix()}
			require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
			healthy := model.GatewayRequest{ID: "zz-healthy-pending", UserID: fixture.user.Id, SaleFXRate: "1", SaleCurrency: "USD", Receivable: money.Amount{Currency: "USD"}, Status: "failed"}
			zero := money.Amount{Currency: "USD"}
			require.NoError(t, model.ReserveGateway(&healthy, zero, zero))
			body := `{"model":"gateway-offering","messages":[{"role":"user","content":"Hello"}],"max_tokens":16,"rate":2}`
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", body)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			id := response.Header().Get(common.RequestIdKey)
			var lifecycle model.GatewayBilling
			require.NoError(t, model.DB.First(&lifecycle, "request_id = ?", id).Error)
			require.Equal(t, "evaluation_failed", lifecycle.Phase, "the reservation must never become a finalized replacement for an unknown actual amount")
			var stored map[string]any
			require.NoError(t, common.UnmarshalJsonStr(lifecycle.Settlement, &stored))
			saleUsage, ok := stored["sale_usage"].(map[string]any)
			require.True(t, ok, "unresolved pricing retains quantitative usage beyond the log summary")
			assert.Equal(t, map[string]any{"text_tokens": float64(1), "image_tokens": float64(1)}, saleUsage["prompt_tokens_details"].(map[string]any)["cached_tokens_details"])
			assert.Equal(t, float64(1), saleUsage["completion_tokens_details"].(map[string]any)["reasoning_tokens"])
			assert.NotContains(t, saleUsage, "cost")
			assert.NotContains(t, stored, "body")
			assert.NotContains(t, stored, "headers")
			assert.NotContains(t, lifecycle.Settlement, "quantitative-secret")
			assert.NotContains(t, lifecycle.Settlement, fixture.token.Key)
			assert.NotContains(t, lifecycle.Settlement, "provider-secret")
			if test.name == "sale overflow" {
				trace, ok := stored["sale_trace"].(map[string]any)
				require.True(t, ok)
				conditions, ok := trace["conditions"].([]any)
				require.True(t, ok)
				assert.Contains(t, conditions, map[string]any{"cond": `header("Content-Type") == "application/json"`, "matched": true})
				assert.Equal(t, "2500000000000001/50000", trace["unrounded_amount"], "the exact failed amount must retain the numeric rate without persisting the request")
			}
			view := walletAPIRequest(t, fixture.engine, fixture.adminPAT, http.MethodGet, "/api/admin/v1/requests?status=settlement_pending", "")
			require.True(t, view.Success, view.Message)
			items := view.Data["items"].([]any)
			require.Len(t, items, 1)
			item := items[0].(map[string]any)
			assert.Equal(t, id, item["id"])
			assert.Equal(t, "settlement_pending", item["status"])
			assert.Equal(t, "billing_evaluation_failed", item["error_code"])
			if test.owner == model.PriceOwnerOffering {
				assert.Equal(t, true, item["sale_pending"])
			} else {
				assert.NotEqual(t, true, item["sale_pending"])
			}
			adminAttempts := walletAPIRequest(t, fixture.engine, fixture.adminPAT, http.MethodGet, "/api/admin/v1/requests/"+id+"/attempts", "")
			require.True(t, adminAttempts.Success, adminAttempts.Message)
			adminAttempt := adminAttempts.Data["items"].([]any)[0].(map[string]any)
			if test.name == "purchase conversion overflow" {
				assert.NotEqual(t, true, adminAttempt["cost_pending"])
				assert.Equal(t, true, adminAttempt["converted_cost_pending"])
				assert.Equal(t, map[string]any{"nanos": "5000000000000000000", "currency": "EUR"}, adminAttempt["cost"])
			} else if test.owner == model.PriceOwnerChannel {
				assert.Equal(t, true, adminAttempt["cost_pending"])
			} else {
				assert.NotEqual(t, true, adminAttempt["cost_pending"])
			}
			selfRequests := walletAPIRequest(t, fixture.engine, fixture.userPAT, http.MethodGet, "/api/requests/self?status=settlement_pending", "")
			require.True(t, selfRequests.Success, selfRequests.Message)
			selfRequest := selfRequests.Data["items"].([]any)[0].(map[string]any)
			assert.Equal(t, item["sale_pending"], selfRequest["sale_pending"])
			assert.NotContains(t, selfRequest, "cost_pending")
			query := fmt.Sprintf("status=settlement_pending&token_id=%d&model_offering_id=%d&started_after=%d&started_before=%d", fixture.token.Id, fixture.offering.ID, clock.Now().UnixMilli(), clock.Now().UnixMilli())
			filtered := walletAPIRequest(t, fixture.engine, fixture.adminPAT, http.MethodGet, "/api/admin/v1/requests?"+query, "")
			require.True(t, filtered.Success, filtered.Message)
			assert.EqualValues(t, 1, filtered.Data["total"])
			for _, query := range []string{
				"status=success",
				fmt.Sprintf("token_id=%d", fixture.token.Id+1),
				fmt.Sprintf("model_offering_id=%d", fixture.offering.ID+1),
				fmt.Sprintf("started_after=%d", clock.Now().UnixMilli()+1),
				fmt.Sprintf("started_before=%d", clock.Now().UnixMilli()-1),
			} {
				filtered := walletAPIRequest(t, fixture.engine, fixture.adminPAT, http.MethodGet, "/api/admin/v1/requests?"+query, "")
				require.True(t, filtered.Success, filtered.Message)
				assert.Zero(t, filtered.Data["total"], query)
			}
			requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			require.Len(t, requests, 1)
			assert.Equal(t, int64(10), requests[0].Usage.PromptTokens)
			assert.Equal(t, int64(5), requests[0].Usage.CompletionTokens)
			attempts, total, err := model.ListGatewayAttempts(t.Context(), id, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.Equal(t, int64(10), attempts[0].Usage.PromptTokens)
			assert.Equal(t, "upstream", attempts[0].UsageSource)
			if test.owner == model.PriceOwnerOffering {
				assert.Equal(t, int64(20000), attempts[0].Cost.Nanos)
			}
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			deducted := test.reservation
			if test.owner == model.PriceOwnerChannel {
				deducted = 36000
			}
			assert.Equal(t, int64(1000000000)-deducted, wallet.Balance.Nanos)
			_, err = model.TopUpWallet(model.WalletTopUp{UserId: fixture.user.Id, Source: money.Amount{Nanos: 1000, Currency: "USD"}, Reference: "pending-credit"})
			require.NoError(t, err)
			blocked := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/chat/completions", body)
			if test.owner == model.PriceOwnerOffering {
				assert.Equal(t, http.StatusPaymentRequired, blocked.Code, blocked.Body.String())
				assert.Equal(t, 1, calls)
			} else {
				assert.Equal(t, http.StatusOK, blocked.Code, blocked.Body.String())
				assert.Equal(t, 2, calls, "platform cost reconciliation does not create a user debt")
			}
			walletView := walletAPIRequest(t, fixture.engine, fixture.adminPAT, http.MethodGet, fmt.Sprintf("/api/admin/v1/user-wallets/%d", fixture.user.Id), "")
			require.True(t, walletView.Success, walletView.Message)
			if test.owner == model.PriceOwnerOffering {
				assert.Equal(t, true, walletView.Data["requests_paused"])
			} else {
				assert.NotEqual(t, true, walletView.Data["requests_paused"])
			}
			self := walletAPIRequest(t, fixture.engine, fixture.userPAT, http.MethodGet, "/api/requests/self/"+id+"/attempts", "")
			require.True(t, self.Success, self.Message)
			selfAttempt := self.Data["items"].([]any)[0].(map[string]any)
			assert.NotContains(t, selfAttempt, "cost")
			assert.NotContains(t, selfAttempt, "cost_pending")
			assert.NotContains(t, selfAttempt, "converted_cost_pending")
			assert.NotContains(t, selfAttempt, "purchase_expression")
			other := createWalletTestAccessToken(t, fixture.sale.CreatedBy, "log:read")
			denied := adminSecurityRequest(fixture.engine, other, http.MethodGet, "/api/requests/self/"+id+"/attempts", "")
			assert.Equal(t, http.StatusNotFound, denied.Code, denied.Body.String())
			require.NoError(t, model.PrepareGatewaySettlement(model.GatewaySettlement{Request: healthy, SaleSource: zero}))
			completed, err := model.RecoverGatewaySettlements(t.Context(), 1)
			assert.Equal(t, 1, completed, "an older unresolved evaluation must not starve recoverable settlements")
			assert.ErrorIs(t, err, model.ErrGatewayEvaluationPending)
			for range 2 {
				completed, err := model.RecoverGatewaySettlements(t.Context(), 100)
				assert.Zero(t, completed)
				assert.Error(t, err, "unresolved evaluations must remain visible to the recovery operator")
			}
			count := 2
			if test.owner == model.PriceOwnerChannel {
				count = 3
			}
			firstPage := walletAPIRequest(t, fixture.engine, fixture.adminPAT, http.MethodGet, "/api/admin/v1/requests?page=1&page_size=1", "")
			require.True(t, firstPage.Success, firstPage.Message)
			assert.EqualValues(t, count, firstPage.Data["total"])
			require.Len(t, firstPage.Data["items"].([]any), 1)
			lastPage := walletAPIRequest(t, fixture.engine, fixture.adminPAT, http.MethodGet, fmt.Sprintf("/api/admin/v1/requests?page=%d&page_size=1", count), "")
			require.True(t, lastPage.Success, lastPage.Message)
			assert.EqualValues(t, count, lastPage.Data["total"])
			require.Len(t, lastPage.Data["items"].([]any), 1)
			assert.Equal(t, healthy.ID, lastPage.Data["items"].([]any)[0].(map[string]any)["id"])
			var postings []model.LedgerEntry
			require.NoError(t, model.DB.Where("request_id = ?", id).Order("id").Find(&postings).Error)
			if test.owner == model.PriceOwnerOffering {
				require.Len(t, postings, 2, "known Provider costs must be posted even while the sale needs reconciliation")
				assert.Equal(t, model.LedgerAccountProviderPayable, postings[1].AccountType)
				assert.Equal(t, int64(20000), postings[1].Amount.Nanos)
			} else if test.name == "purchase conversion overflow" {
				require.Len(t, postings, 5)
				assert.Equal(t, model.LedgerAccountProviderPayable, postings[4].AccountType)
				assert.Equal(t, money.Amount{Nanos: 5000000000000000000, Currency: "EUR"}, postings[4].Amount)
				assert.Equal(t, "2", postings[4].FxRate.Decimal.String())
			} else {
				require.Len(t, postings, 4)
			}
			assert.Equal(t, model.LedgerEntryPreConsume, postings[0].Type)
			reconciliation, err := model.ReconcileWallets(100)
			require.NoError(t, err)
			assert.Empty(t, reconciliation.Mismatches)
		})
	}
}
