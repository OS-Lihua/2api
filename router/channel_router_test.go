package router

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registerLegacyChannelCompatibilityTestRoutes exercises retained internal batch
// transactions only. Production routers never register this test-only surface;
// the real /api/channel endpoints are covered by the retirement matrix below.
func registerLegacyChannelCompatibilityTestRoutes(engine *gin.Engine) {
	group := engine.Group("/api/test-only/legacy-channel", middleware.AccessTokenAudit(), middleware.AdminAuth())
	handlePermissionRoute(group, http.MethodPost, "/status/batch", authz.ChannelOperate, controller.BatchUpdateChannelStatus)
	handlePermissionRoute(group, http.MethodPost, "/tag/disabled", authz.ChannelOperate, controller.DisableTagChannels)
}

// The retired tag handler reads the old Provider projection. Only subtests
// that mount and exercise that handler need its historical columns.
func restoreLegacyProviderTagFixture(t *testing.T) {
	t.Helper()
	require.NoError(t, model.DB.Exec(`ALTER TABLE "providers"
		ADD COLUMN "models" text, ADD COLUMN "model_mapping" text,
		ADD COLUMN "group" text, ADD COLUMN "priority" bigint, ADD COLUMN "weight" bigint`).Error)
}

func TestLegacyChannelRetirementAuthorityAndIsolation(t *testing.T) {
	engine, admin, _ := newAdminTestServer(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.TopUp{}, &model.QuotaData{}))
	require.NoError(t, model.MigrateGatewayBilling(model.DB))
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(upstream.Close)
	provider := model.Provider{Name: "Retired provider", Type: 1, Status: common.ChannelStatusEnabled, Key: "retired-provider-secret", BaseURL: &upstream.URL, Models: "old-model", Group: "default"}
	require.NoError(t, model.DB.Create(&provider).Error)
	require.NoError(t, model.DB.Create(&model.Ability{ChannelId: provider.Id, Group: "default", Model: "old-model", Enabled: true}).Error)
	metadata := model.Model{ModelName: "native-model"}
	require.NoError(t, model.DB.Create(&metadata).Error)
	require.NoError(t, model.DB.Create(&model.Channel{ProviderID: provider.Id, ModelID: metadata.Id, Protocol: protocol.OpenAIChat, UpstreamModelName: "native-upstream", ParameterOverrides: "{}", Status: model.ChannelPending}).Error)
	_, err := model.TopUpWallet(model.WalletTopUp{UserId: admin.Id, Source: money.Amount{Nanos: 123456789, Currency: "USD"}, Reference: "retirement-fixture"})
	require.NoError(t, err)
	// Independent inventory freezes every former public permission; iterating only
	// the production table would miss a deleted route or an accidentally widened grant.
	inventory := map[authz.Permission][]string{
		authz.ChannelRead:           {"GET /priority/order", "GET /", "GET /search", "GET /models", "GET /default_base_urls", "GET /models_enabled", "GET /ops", "GET /:id", "GET /:id/vllm/status", "GET /:id/sglang/status", "GET /:id/codex/usage", "GET /:id/codex/usage/reset-credits", "GET /tag/models"},
		authz.ChannelWrite:          {"POST /priority/reorder", "PUT /", "PUT /tag", "POST /batch/tag", "POST /upstream_updates/apply", "POST /upstream_updates/apply_all"},
		authz.ChannelOperate:        {"GET /test", "GET /test/:id", "GET /update_balance", "GET /update_balance/:id", "POST /status/batch", "POST /:id/status", "POST /tag/disabled", "POST /tag/enabled", "POST /fix", "GET /fetch_models/:id", "POST /:id/codex/usage/reset", "POST /multi_key/manage", "POST /upstream_updates/detect", "POST /upstream_updates/detect_all"},
		authz.ChannelSensitiveWrite: {"POST /", "DELETE /disabled", "DELETE /:id", "POST /batch", "POST /fetch_models", "POST /:id/codex/refresh", "POST /ollama/pull", "POST /ollama/pull/stream", "DELETE /ollama/delete", "GET /ollama/version/:id", "POST /copy/:id"},
	}
	declared := map[string]authz.Permission{}
	for _, route := range channelPermissionRoutes {
		declared[route.method+" "+route.path] = route.permission
		assert.Equal(t, reflect.ValueOf(controller.RetiredLegacyChannelManagement).Pointer(), reflect.ValueOf(route.handler).Pointer())
	}
	expected := map[string]authz.Permission{}
	scoped := map[authz.Permission]string{}
	for permission, routes := range inventory {
		scoped[permission] = createWalletTestAccessToken(t, admin.Id, service.AccessTokenScopeOf(permission))
		for _, route := range routes {
			expected[route] = permission
		}
	}
	require.Equal(t, expected, declared)
	require.Len(t, expected, 44)
	wrong := createWalletTestAccessToken(t, admin.Id, "profile:read")
	expired := createWalletTestAccessToken(t, admin.Id, "channel:read", "channel:write", "channel:operate", "channel:sensitive_write")
	require.NoError(t, model.DB.Model(&model.UserAccessToken{}).Where("token_hash = ?", model.AccessTokenFingerprint(expired)).Update("expires_at", time.Now().Unix()-1).Error)
	revoked := createWalletTestAccessToken(t, admin.Id, "channel:read")
	require.NoError(t, model.DB.Where("token_hash = ?", model.AccessTokenFingerprint(revoked)).Delete(&model.UserAccessToken{}).Error)
	user := model.User{Username: "retirement-user", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", AffCode: "retired", AuthVersion: 1}
	require.NoError(t, model.DB.Create(&user).Error)
	ordinary := createWalletTestAccessToken(t, user.Id, "channel:read", "channel:write", "channel:operate", "channel:sensitive_write")
	tables := []string{"providers", "abilities", "channels", "models", "wallets", "ledger_entries", "gateway_billings", "top_ups", "quota_data"}
	before := legacyChannelRows(t, tables...)
	for route, permission := range expected {
		t.Run(route, func(t *testing.T) {
			// Overrides replace the resource's full action map. Establish this
			// case's exact server grant independently of randomized map order.
			require.NoError(t, authz.SetUserPermissions(admin.Id, authz.PermissionsMap{permission.Resource: {permission.Action: true}}))
			require.True(t, authz.Can(admin.Id, admin.Role, permission))
			method, relative, _ := strings.Cut(route, " ")
			path := "/api/channel" + strings.ReplaceAll(relative, ":id", fmt.Sprint(provider.Id))
			body := fmt.Sprintf(`{"id":%d,"ids":[%d],"status":2,"models":"bypass","group":"bypass","key":"request-secret"}`, provider.Id, provider.Id)
			for _, authority := range []struct {
				name, token string
				status      int
			}{
				{"anonymous", "", 401}, {"expired", expired, 401}, {"revoked", revoked, 401}, {"wrong_scope", wrong, 403}, {"ordinary_user", ordinary, 403}, {"exact_scope", scoped[permission], 410},
			} {
				t.Run(authority.name, func(t *testing.T) {
					response := adminSecurityRequest(engine, authority.token, method, path, body)
					require.Equal(t, authority.status, response.Code, response.Body.String())
					if authority.status == http.StatusGone {
						assert.Contains(t, response.Header().Get("Cache-Control"), "no-store")
						assert.Contains(t, response.Body.String(), `"code":"legacy_channel_management_retired"`)
						assert.Contains(t, response.Body.String(), `"success":false`)
					}
					assert.NotContains(t, response.Body.String(), provider.Key)
				})
			}
			// A matching PAT grant does not bypass current Casbin denial.
			require.NoError(t, authz.SetUserPermissions(admin.Id, authz.PermissionsMap{permission.Resource: {permission.Action: false}}))
			require.False(t, authz.Can(admin.Id, admin.Role, permission))
			response := adminSecurityRequest(engine, scoped[permission], method, path, body)
			assert.Equal(t, http.StatusForbidden, response.Code)
			require.NoError(t, authz.SetUserPermissions(admin.Id, authz.PermissionsMap{permission.Resource: {permission.Action: true}}))
			require.Equal(t, before, legacyChannelRows(t, tables...))
		})
	}
	assert.Zero(t, upstreamCalls.Load(), "no retired operation may contact a Provider")
	logs, _, err := model.GetAuditLogs(model.AuditLogFilter{Category: model.AuditCategoryAccessToken}, 0, 1000, common.RoleRootUser)
	require.NoError(t, err)
	for route := range expected {
		method, path, _ := strings.Cut(route, " ")
		gone, denied := 0, 0
		for _, entry := range logs {
			if entry.Method == method && entry.Route == "/api/channel"+path {
				assert.False(t, entry.Success)
				if entry.Status == 410 {
					gone++
				}
				if entry.Status == 403 {
					denied++
				}
			}
		}
		assert.Equal(t, 1, gone, route)
		assert.GreaterOrEqual(t, denied, 3, route)
	}
	encoded, err := common.Marshal(logs)
	require.NoError(t, err)
	for _, secret := range []string{provider.Key, "request-secret", wrong, expired, revoked, ordinary} {
		assert.NotContains(t, string(encoded), secret)
	}
	for _, token := range scoped {
		assert.NotContains(t, string(encoded), token)
	}
}

func TestLegacyChannelRetirementKeyProofBoundary(t *testing.T) {
	engine, admin, _ := newAdminTestServer(t)
	require.NoError(t, model.DB.AutoMigrate(&model.AuthFlow{}, &model.TwoFA{}, &model.PasskeyCredential{}))
	root := model.User{Username: "retirement-root", Role: common.RoleRootUser, Status: common.UserStatusEnabled, AuthVersion: 1, Group: "default", AffCode: "retroot"}
	require.NoError(t, model.DB.Create(&root).Error)
	require.NoError(t, model.DB.Create(&model.TwoFA{UserId: root.Id, IsEnabled: true, Secret: "non-usable-test-factor"}).Error)
	provider := model.Provider{Name: "Secret must stay hidden", Type: 1, Key: "never-return-this-provider-secret", Status: common.ChannelStatusEnabled}
	require.NoError(t, model.DB.Create(&provider).Error)
	token := createWalletTestAccessToken(t, root.Id, service.AccessTokenScopeOf(authz.ChannelSecretView))
	var personal model.UserAccessToken
	require.NoError(t, model.DB.Where("token_hash = ?", model.AccessTokenFingerprint(token)).First(&personal).Error)
	identity := service.AuthIdentity{UserID: root.Id, SessionID: model.AccessTokenSessionID(personal.Id), UserAuthVersion: root.AuthVersion, SessionVersion: model.AccessTokenSessionVersion}
	proofFor := func(id int) string {
		context, err := common.Marshal(service.ProviderKeyReadContext{ChannelID: id})
		require.NoError(t, err)
		binding, err := service.BindVerificationOperation(service.VerificationOperation{Scope: service.VerificationScopeChannelKeyRead, Context: context})
		require.NoError(t, err)
		proof, _, err := service.IssueSecurityProof(identity, service.VerificationMethodTwoFA, binding)
		require.NoError(t, err)
		return proof
	}
	request := func(credential, proof string, id int) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/channel/%d/key", id), nil)
		r.Header.Set("Authorization", "Bearer "+credential)
		r.Header.Set("X-Security-Proof", proof)
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, r)
		assert.NotContains(t, response.Body.String(), provider.Key)
		return response
	}
	before := legacyChannelRows(t, "providers", "abilities")
	proof := proofFor(provider.Id)
	for _, credential := range []string{
		createWalletTestAccessToken(t, root.Id, "channel:read"),
		createWalletTestAccessToken(t, admin.Id, service.AccessTokenScopeOf(authz.ChannelSecretView)),
	} {
		assert.Equal(t, http.StatusForbidden, request(credential, proof, provider.Id).Code)
	}
	assert.Equal(t, http.StatusUnauthorized, request("", proof, provider.Id).Code)
	missing := request(token, "", provider.Id)
	assert.Equal(t, http.StatusForbidden, missing.Code)
	assert.Contains(t, missing.Body.String(), "SECURITY_PROOF_REQUIRED")
	mismatch := request(token, proof, provider.Id+1)
	assert.Equal(t, http.StatusForbidden, mismatch.Code)
	assert.Contains(t, mismatch.Body.String(), "SECURITY_PROOF_CONTEXT_MISMATCH")
	retired := request(token, proof, provider.Id)
	require.Equal(t, http.StatusGone, retired.Code, retired.Body.String())
	assert.Contains(t, retired.Header().Get("Cache-Control"), "no-store")
	assert.Contains(t, retired.Body.String(), `"code":"legacy_channel_management_retired"`)
	var consumed int64
	require.NoError(t, model.DB.Model(&model.AuthFlow{}).Where("user_id = ? AND consumed_at IS NOT NULL", root.Id).Count(&consumed).Error)
	assert.EqualValues(t, 1, consumed, "410 must not restore a successfully consumed proof")
	replay := request(token, proof, provider.Id)
	assert.Equal(t, http.StatusForbidden, replay.Code)
	assert.Contains(t, replay.Body.String(), "SECURITY_PROOF_CONSUMED")
	expiredProof := proofFor(provider.Id)
	require.NoError(t, model.DB.Model(&model.AuthFlow{}).Where("user_id = ? AND consumed_at IS NULL", root.Id).Update("expires_at", time.Now().Add(-time.Minute)).Error)
	expired := request(token, expiredProof, provider.Id)
	assert.Equal(t, http.StatusForbidden, expired.Code)
	assert.Contains(t, expired.Body.String(), "SECURITY_PROOF_EXPIRED")
	require.NoError(t, model.DB.Model(&personal).Update("expires_at", time.Now().Unix()-1).Error)
	assert.Equal(t, http.StatusUnauthorized, request(token, proofFor(provider.Id), provider.Id).Code)
	assert.Equal(t, before, legacyChannelRows(t, "providers", "abilities"))
	logs, _, err := model.GetAuditLogs(model.AuditLogFilter{}, 0, 100, common.RoleRootUser)
	require.NoError(t, err)
	encoded, err := common.Marshal(logs)
	require.NoError(t, err)
	for _, secret := range []string{provider.Key, token, proof, expiredProof} {
		assert.NotContains(t, string(encoded), secret)
	}
	gone, rejected := false, false
	for _, entry := range logs {
		if entry.Route == "/api/channel/:id/key" {
			assert.False(t, entry.Success)
			gone = gone || entry.Status == 410
			rejected = rejected || entry.Status == 403
		}
	}
	assert.True(t, gone)
	assert.True(t, rejected)
}

func TestLegacyChannelRetirementKeyRateLimit(t *testing.T) {
	_, root, _ := newAdminTestServer(t)
	require.NoError(t, model.DB.Model(root).Update("role", common.RoleRootUser).Error)
	token := createWalletTestAccessToken(t, root.Id, service.AccessTokenScopeOf(authz.ChannelSecretView))
	enabled, count, duration := common.CriticalRateLimitEnable, common.CriticalRateLimitNum, common.CriticalRateLimitDuration
	common.CriticalRateLimitEnable, common.CriticalRateLimitNum, common.CriticalRateLimitDuration = true, 1, 60
	t.Cleanup(func() {
		common.CriticalRateLimitEnable, common.CriticalRateLimitNum, common.CriticalRateLimitDuration = enabled, count, duration
	})
	engine := gin.New()
	registerChannelRoutes(engine.Group("/api", middleware.AccessTokenAudit()))
	for index, status := range []int{http.StatusForbidden, http.StatusTooManyRequests} {
		request := httptest.NewRequest(http.MethodPost, "/api/channel/1/key", nil)
		request.RemoteAddr = "203.0.113.239:12345"
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		assert.Equal(t, status, response.Code, "attempt %d: %s", index, response.Body.String())
	}
}

func TestLegacyChannelRetirementRejectsRoutingWrites(t *testing.T) {
	engine, _, token := newAdminTestServer(t)
	provider := model.Provider{Name: "Retired routing fixture", Type: 1, Status: common.ChannelStatusEnabled, Key: "routing-fixture-secret", Models: "original-model", Group: "default"}
	require.NoError(t, model.DB.Create(&provider).Error)
	require.NoError(t, model.DB.Create(&model.Ability{ChannelId: provider.Id, Group: "default", Model: "original-model", Enabled: true}).Error)
	before := legacyChannelRows(t, "providers", "abilities")
	response := adminSecurityRequest(engine, token, http.MethodPut, "/api/channel/", fmt.Sprintf(`{"id":%d,"type":1,"name":"Retired routing fixture","models":"bypass-model","group":"bypass-group","priority":99,"weight":7}`, provider.Id))
	assert.Equal(t, http.StatusGone, response.Code, response.Body.String())
	assert.Contains(t, response.Header().Get("Cache-Control"), "no-store")
	assert.Equal(t, before, legacyChannelRows(t, "providers", "abilities"), "retired routing must not rewrite Providers or regenerate abilities")
}

func legacyChannelRows(t *testing.T, tables ...string) map[string]string {
	t.Helper()
	rows := make(map[string]string, len(tables))
	for _, table := range tables {
		var encoded string
		require.NoError(t, model.DB.Raw(`SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text), '[]'::jsonb)::text FROM "`+table+`" t`).Scan(&encoded).Error)
		rows[table] = encoded
	}
	return rows
}

func TestChannelDefaultBaseURLsRequireReadPermission(t *testing.T) {
	assertChannelRoutePermission(t, http.MethodGet, "/default_base_urls", authz.ChannelRead, controller.RetiredLegacyChannelManagement)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	registerChannelRoutes(engine.Group("/api"))
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/channel/default_base_urls", nil))
	assert.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func TestChannelStatusRoutesUseExpectedPermissions(t *testing.T) {
	assertChannelRoutePermission(t, http.MethodGet, "/priority/order", authz.ChannelRead, controller.RetiredLegacyChannelManagement)
	assertChannelRoutePermission(t, http.MethodPost, "/priority/reorder", authz.ChannelWrite, controller.RetiredLegacyChannelManagement)
	assertChannelRoutePermission(t, http.MethodGet, "/:id/vllm/status", authz.ChannelRead, controller.RetiredLegacyChannelManagement)
	assertChannelRoutePermission(t, http.MethodGet, "/:id/sglang/status", authz.ChannelRead, controller.RetiredLegacyChannelManagement)
	assertChannelRoutePermission(t, http.MethodPost, "/:id/status", authz.ChannelOperate, controller.RetiredLegacyChannelManagement)
	assertChannelRoutePermission(t, http.MethodPost, "/status/batch", authz.ChannelOperate, controller.RetiredLegacyChannelManagement)
	assertChannelRoutePermission(t, http.MethodPut, "/", authz.ChannelWrite, controller.RetiredLegacyChannelManagement)
}

func TestChannelDeleteRoutesUseSensitiveWritePermission(t *testing.T) {
	assertChannelRoutePermission(t, http.MethodDelete, "/:id", authz.ChannelSensitiveWrite, controller.RetiredLegacyChannelManagement)
	assertChannelRoutePermission(t, http.MethodPost, "/batch", authz.ChannelSensitiveWrite, controller.RetiredLegacyChannelManagement)
	assertChannelRoutePermission(t, http.MethodDelete, "/disabled", authz.ChannelSensitiveWrite, controller.RetiredLegacyChannelManagement)
	assertChannelRoutePermission(t, http.MethodPut, "/", authz.ChannelWrite, controller.RetiredLegacyChannelManagement)
	assertChannelRoutePermission(t, http.MethodPut, "/tag", authz.ChannelWrite, controller.RetiredLegacyChannelManagement)
	assertChannelRoutePermission(t, http.MethodPost, "/batch/tag", authz.ChannelWrite, controller.RetiredLegacyChannelManagement)
}

func TestChannelStatusRoutesRegisterWithoutConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	api := engine.Group("/api")

	require.NotPanics(t, func() {
		registerChannelRoutes(api)
	})
}

func assertChannelRoutePermission(t *testing.T, method string, path string, permission authz.Permission, handler any) {
	t.Helper()
	for _, route := range channelPermissionRoutes {
		if route.method == method && route.path == path {
			assert.Equal(t, permission, route.permission)
			assert.Equal(t, reflect.ValueOf(handler).Pointer(), reflect.ValueOf(route.handler).Pointer())
			return
		}
	}
	t.Fatalf("route %s %s not found", method, path)
}

func TestProviderAccountListWithoutLegacyRoutingColumns(t *testing.T) {
	engine, admin, _ := newAdminTestServer(t)
	require.NoError(t, authz.SetUserPermissions(admin.Id, authz.PermissionsMap{"channel": {"read": true}}))
	reader := createWalletTestAccessToken(t, admin.Id, "channel:read")
	wrongScope := createWalletTestAccessToken(t, admin.Id, "model:read")
	ordinary := model.User{Username: "provider-list-user", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: "provider-list-user"}
	require.NoError(t, model.DB.Create(&ordinary).Error)
	ordinaryToken := createWalletTestAccessToken(t, ordinary.Id, "channel:read")

	address := "https://url-user:url-password@gateway.example.test:8443/v1?access_key=url-query-secret#url-fragment-secret"
	setting := `{"credential":"adapter-secret"}`
	header := `{"Authorization":"header-secret"}`
	providers := []model.Provider{
		{Name: "shared account", Type: 1, Status: common.ChannelStatusEnabled, BaseURL: &address, Setting: &setting, HeaderOverride: &header,
			ProviderInfo: model.ProviderInfo{IsMultiKey: true, MultiKeySize: 1, MultiKeyDisabledReason: map[int]string{0: "upstream-secret"}}},
		{Name: "second account", Type: 3, Status: common.ChannelStatusManuallyDisabled},
		{Name: "shared account", Type: 1, Status: common.ChannelStatusAutoDisabled},
		{Name: "final account", Type: 3, Status: common.ChannelStatusEnabled},
	}
	require.NoError(t, model.DB.Exec(`ALTER TABLE "providers"
		ADD COLUMN IF NOT EXISTS "group" text, ADD COLUMN IF NOT EXISTS "priority" bigint`).Error)
	legacyGroups := []string{"vip", "default", "vip", "default"}
	for i := range providers {
		providers[i].Key = fmt.Sprintf("sk-provider-list-secret-%d", i)
		require.NoError(t, model.DB.Create(&providers[i]).Error)
		require.NoError(t, model.DB.Exec(`UPDATE "providers" SET "group" = ?, "priority" = ? WHERE "id" = ?`, legacyGroups[i], 100-i, providers[i].Id).Error)
	}
	path := "/api/admin/v1/providers"

	t.Run("legacy priorities do not change account pagination", func(t *testing.T) {
		for page, wantIDs := range [][]int{{providers[3].Id, providers[2].Id}, {providers[1].Id, providers[0].Id}} {
			response := adminSecurityRequest(engine, reader, http.MethodGet, fmt.Sprintf("%s?p=%d&page_size=2", path, page+1), "")
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var result struct {
				Success bool
				Data    struct {
					Items []struct{ ID int }
					Total int
				}
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
			require.True(t, result.Success)
			assert.Equal(t, 4, result.Data.Total)
			ids := make([]int, 0, len(result.Data.Items))
			for _, item := range result.Data.Items {
				ids = append(ids, item.ID)
			}
			assert.Equal(t, wantIDs, ids)
		}
	})

	// Account reads must also work after the entire old routing schema is gone.
	require.NoError(t, model.DB.Exec(`ALTER TABLE "providers" DROP COLUMN IF EXISTS "models", DROP COLUMN IF EXISTS "model_mapping",
		DROP COLUMN IF EXISTS "group", DROP COLUMN IF EXISTS "priority", DROP COLUMN IF EXISTS "weight"`).Error)
	for _, column := range []string{"models", "model_mapping", "group", "priority", "weight"} {
		require.False(t, model.DB.Migrator().HasColumn("providers", column))
	}
	for _, tc := range []struct {
		name     string
		query    string
		ids      []int
		total    int
		page     int
		pageSize int
	}{
		{"first page", "p=1&page_size=2", []int{providers[3].Id, providers[2].Id}, 4, 1, 2},
		{"second page", "p=2&page_size=2", []int{providers[1].Id, providers[0].Id}, 4, 2, 2},
		{"empty page", "p=3&page_size=2", []int{}, 4, 3, 2},
		{"offering choices", "p=1&page_size=100", []int{providers[3].Id, providers[2].Id, providers[1].Id, providers[0].Id}, 4, 1, 100},
		{"name", "keyword=" + url.QueryEscape(" shared account ") + "&page_size=2", []int{providers[2].Id, providers[0].Id}, 2, 1, 2},
		{"identifier", fmt.Sprintf("keyword=%d&page_size=2", providers[1].Id), []int{providers[1].Id}, 1, 1, 2},
		{"host", "keyword=gateway.example.test&page_size=2", []int{providers[0].Id}, 1, 1, 2},
		{"endpoint address", "keyword=" + url.QueryEscape("https://gateway.example.test:8443/v1") + "&page_size=2", []int{providers[0].Id}, 1, 1, 2},
		{"hidden address credential", "keyword=url-query-secret&page_size=2", []int{}, 0, 1, 2},
		{"type", "type=3&page_size=2", []int{providers[3].Id, providers[1].Id}, 2, 1, 2},
		{"enabled", "status=enabled&page_size=2", []int{providers[3].Id, providers[0].Id}, 2, 1, 2},
		{"both disabled states", "status=disabled&page_size=2", []int{providers[2].Id, providers[1].Id}, 2, 1, 2},
		{"combined filters", "keyword=account&type=3&status=1&page_size=2", []int{providers[3].Id}, 1, 1, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := adminSecurityRequest(engine, reader, http.MethodGet, path+"?"+tc.query, "")
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var result struct {
				Success bool
				Data    struct {
					Items    []map[string]any
					Total    int
					Page     int
					PageSize int `json:"page_size"`
				}
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
			require.True(t, result.Success)
			assert.Equal(t, tc.total, result.Data.Total)
			assert.Equal(t, tc.page, result.Data.Page)
			assert.Equal(t, tc.pageSize, result.Data.PageSize)
			require.NotNil(t, result.Data.Items)
			ids := make([]int, 0, len(result.Data.Items))
			for _, item := range result.Data.Items {
				id, ok := item["id"].(float64)
				require.True(t, ok)
				ids = append(ids, int(id))
				for _, field := range []string{"key", "key_preview", "group", "priority", "weight", "models", "model_mapping", "setting", "settings", "header_override", "param_override", "other_info", "other"} {
					assert.NotContains(t, item, field)
				}
				if int(id) == providers[0].Id {
					assert.Equal(t, "shared account", item["name"])
					assert.EqualValues(t, 1, item["type"])
					assert.EqualValues(t, common.ChannelStatusEnabled, item["status"])
					assert.Equal(t, "https://REDACTED:REDACTED@gateway.example.test:8443/v1?__redacted__=1#REDACTED", item["base_url"])
					info, ok := item["channel_info"].(map[string]any)
					require.True(t, ok)
					assert.Equal(t, true, info["is_multi_key"])
					assert.EqualValues(t, 1, info["multi_key_size"])
					assert.NotContains(t, info, "multi_key_disabled_reason")
				}
			}
			assert.Equal(t, tc.ids, ids)
			for _, secret := range []string{"sk-provider-list-secret", "url-user", "url-password", "url-query-secret", "url-fragment-secret", "adapter-secret", "header-secret", "upstream-secret"} {
				assert.NotContains(t, response.Body.String(), secret)
			}
		})
	}

	t.Run("existing authorization remains required", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, adminSecurityRequest(engine, "", http.MethodGet, path, "").Code)
		assert.Equal(t, http.StatusForbidden, adminSecurityRequest(engine, wrongScope, http.MethodGet, path, "").Code)
		assert.Equal(t, http.StatusForbidden, adminSecurityRequest(engine, ordinaryToken, http.MethodGet, path, "").Code)
		require.NoError(t, authz.SetUserPermissions(admin.Id, authz.PermissionsMap{"channel": {"read": false}}))
		assert.Equal(t, http.StatusForbidden, adminSecurityRequest(engine, reader, http.MethodGet, path, "").Code)
		require.NoError(t, authz.SetUserPermissions(admin.Id, authz.PermissionsMap{"channel": {"read": true}}))
	})

	t.Run("account reads remain audited in the configured log database", func(t *testing.T) {
		logs, _, err := model.GetAuditLogs(model.AuditLogFilter{Category: model.AuditCategoryAccessToken, UserId: admin.Id}, 0, 100, common.RoleRootUser)
		require.NoError(t, err)
		successes := 0
		for _, entry := range logs {
			if entry.Success {
				successes++
			}
		}
		assert.Equal(t, 15, successes)
	})

	t.Run("retired query controls are rejected before account queries", func(t *testing.T) {
		// An unavailable account table distinguishes early request rejection
		// from either silently ignoring the filter or attempting a list query.
		require.NoError(t, model.DB.Migrator().DropTable(&model.Provider{}))
		for _, query := range []string{"group=vip", "group=", "group", "group=all", "group=vip&group=default", "GROUP=vip", "%67roup=vip", "group=vip;status=enabled", "sort_by=priority", "sort_order=asc", "id_sort=true", "type=not-an-integer"} {
			response := adminSecurityRequest(engine, reader, http.MethodGet, path+"?"+query, "")
			require.Equal(t, http.StatusBadRequest, response.Code, query+": "+response.Body.String())
			var result struct {
				Success    bool
				MessageKey string `json:"message_key"`
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
			assert.False(t, result.Success)
			assert.Equal(t, "Invalid parameters", result.MessageKey)
		}
	})
}

func TestProviderRoutesRequireAdminAndKeepChannelPermissions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	require.NotPanics(t, func() {
		registerProviderRoutes(engine.Group("/api"))
	})
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/v1/providers", nil))
	assert.Equal(t, http.StatusUnauthorized, recorder.Code)

	permissions := map[string]authz.Permission{}
	for _, route := range providerPermissionRoutes {
		permissions[route.method+" "+route.path] = route.permission
	}
	assert.Equal(t, map[string]authz.Permission{
		"GET ":                          authz.ChannelRead,
		"POST ":                         authz.ChannelSensitiveWrite,
		"POST /import":                  authz.ChannelSensitiveWrite,
		"GET /:id":                      authz.ChannelRead,
		"PATCH /:id":                    authz.ChannelWrite,
		"DELETE /:id":                   authz.ChannelSensitiveWrite,
		"POST /:id/enable":              authz.ChannelOperate,
		"POST /:id/disable":             authz.ChannelOperate,
		"POST /:id/balance":             authz.ChannelOperate,
		"GET /:id/keys":                 authz.ChannelOperate,
		"POST /:id/keys/enable":         authz.ChannelOperate,
		"POST /:id/keys/disable":        authz.ChannelOperate,
		"DELETE /:id/keys/disabled":     authz.ChannelOperate,
		"POST /:id/keys/:index/enable":  authz.ChannelOperate,
		"POST /:id/keys/:index/disable": authz.ChannelOperate,
		"DELETE /:id/keys/:index":       authz.ChannelOperate,
	}, permissions)
}
