package router

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newAdminTestServer(t *testing.T) (*gin.Engine, *model.User, string) {
	t.Helper()
	master, redis, cache, secret := common.IsMasterNode, common.RedisEnabled, common.MemoryCacheEnabled, common.SessionSecret
	t.Cleanup(func() {
		common.IsMasterNode, common.RedisEnabled, common.MemoryCacheEnabled, common.SessionSecret = master, redis, cache, secret
	})
	common.IsMasterNode, common.RedisEnabled, common.MemoryCacheEnabled = true, false, false
	common.SessionSecret = "admin-api-regression-session-secret"
	db := model.UseTestDatabases(t)
	require.NoError(t, i18n.Init())
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.UserSession{}, &model.CasbinRule{}, &model.AuthzRole{}, &model.UserAccessToken{}, &model.Option{}, &model.Model{}, &model.Vendor{}, &model.Provider{}, &model.Ability{}))
	require.NoError(t, model.MigrateAuditLogs())
	require.NoError(t, model.EnsureLegacyAccessTokenRetireAt(time.Now().Unix()))
	require.NoError(t, authz.Init(db))
	user := &model.User{Username: "security-admin", Password: "unused", Role: common.RoleAdminUser, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: "security"}
	require.NoError(t, db.Create(user).Error)
	token := createWalletTestAccessToken(t, user.Id, "model:read", "model:write", "channel:read", "channel:write", "channel:sensitive_write", "channel:operate", "ledger:read", "ledger:write")
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	SetApiRouter(engine)
	return engine, user, token
}

func adminSecurityRequest(engine http.Handler, token, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(common.RequestIdKey, "ignored-client-request-id")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	return recorder
}

func TestAdminAPIAuditsCompressedSuccess(t *testing.T) {
	for _, description := range []string{"normal description", strings.Repeat("long description ", 8192)} {
		t.Run(fmt.Sprintf("description bytes %d", len(description)), func(t *testing.T) {
			engine, _, token := newAdminTestServer(t)
			publisher := &model.Vendor{Name: "audit-publisher"}
			require.NoError(t, model.DB.Create(publisher).Error)
			body, err := common.Marshal(map[string]any{"name": "audit-model", "publisher_id": publisher.Id, "description": description})
			require.NoError(t, err)
			request := httptest.NewRequest(http.MethodPost, "/api/admin/v1/models", strings.NewReader(string(body)))
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Accept-Encoding", "gzip")
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)
			require.Equal(t, http.StatusOK, response.Code)
			require.Equal(t, "gzip", response.Header().Get("Content-Encoding"))
			for _, category := range []string{model.AuditCategoryOperation, model.AuditCategoryAccessToken} {
				logs, total, err := model.GetAuditLogs(model.AuditLogFilter{Category: category}, 0, 10, common.RoleRootUser)
				require.NoError(t, err)
				require.EqualValues(t, 1, total, "exactly one %s event", category)
				assert.True(t, logs[0].Success, category)
			}
		})
	}
}

func TestAdminAPIAuditsCompressedBusinessFailures(t *testing.T) {
	engine, admin, token := newAdminTestServer(t)
	require.NoError(t, authz.SetUserPermissions(admin.Id, authz.PermissionsMap{"ledger": {"read": true, "write": true}}))
	// The wallet handler rejects changes to an equal role with HTTP 200 and
	// success=false. Gzip must not hide the business outcome from the audit.
	request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/admin/v1/user-wallets/%d/adjustments", admin.Id), strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept-Encoding", "gzip")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "gzip", response.Header().Get("Content-Encoding"))
	logs, total, err := model.GetAuditLogs(model.AuditLogFilter{Category: model.AuditCategoryOperation}, 0, 10, common.RoleRootUser)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	assert.False(t, logs[0].Success)
}

func TestAdminAPIWriteAuditExcludesProviderSecrets(t *testing.T) {
	engine, admin, _ := newAdminTestServer(t)
	require.NoError(t, model.DB.Model(admin).Update("role", common.RoleRootUser).Error)
	root := createWalletTestAccessToken(t, admin.Id, "channel:read", "channel:write", "channel:sensitive_write", "channel:operate")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"transporterror-secret"}`))
	}))
	t.Cleanup(upstream.Close)
	response := adminSecurityRequest(engine, root, http.MethodPost, "/api/admin/v1/providers", `{"provider":{"type":1,"base_url":"`+upstream.URL+`","name":"audit fixture","models":"gpt-test","group":"default","key":"credential-one\ncredential-two","header_override":"{\"X-Api-Key\":\"header-secret\"}","other":"account-secret","settings":"{\"custom\":\"deployment-secret\"}"},"mode":"multi_to_single"}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var created struct {
		Success bool
		Data    struct {
			IDs []int `json:"ids"`
		}
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &created))
	require.True(t, created.Success)
	require.Len(t, created.Data.IDs, 1)
	path := fmt.Sprintf("/api/admin/v1/providers/%d", created.Data.IDs[0])
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/api/admin/v1/providers", "", 200}, {"GET", path, "", 200}, {"GET", path + "/keys", "", 200},
		{"PATCH", path, `{"key":"replacement-secret","header_override":"{\"Authorization\":\"Bearer new-header-secret\"}"}`, 200},
		{"POST", path + "/disable", "", 200}, {"POST", path + "/enable", "", 200},
		{"POST", path + "/keys/0/disable", "", 200}, {"POST", path + "/keys/0/enable", "", 200},
		{"POST", path + "/keys/disable", "", 200}, {"POST", path + "/keys/enable", "", 200},
		{"DELETE", path + "/keys/disabled", "", 400}, {"POST", path + "/balance", "", 502},
		{"PATCH", path, `{"key":"rejected-secret",`, 400},
		{"DELETE", path + "/keys/invalid-secret", "", 400},
		{"POST", path + "/key", "", 403}, {"GET", path + "/export", "", 403},
		{"DELETE", path, "", 200},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			reply := adminSecurityRequest(engine, root, tc.method, tc.path+"?token=query-secret", tc.body)
			assert.Equal(t, tc.status, reply.Code, reply.Body.String())
			for _, secret := range []string{"credential-one", "credential-two", "header-secret", "account-secret", "deployment-secret", "replacement-secret", "new-header-secret", "rejected-secret"} {
				assert.NotContains(t, reply.Body.String(), secret)
			}
		})
	}
	logs, total, err := model.GetAuditLogs(model.AuditLogFilter{Category: model.AuditCategoryOperation}, 0, 100, common.RoleRootUser)
	require.NoError(t, err)
	assert.EqualValues(t, 15, total, "exactly one operation event per write or refused read")
	encoded, err := common.Marshal(logs)
	require.NoError(t, err)
	for _, secret := range []string{root, "credential-one", "credential-two", "header-secret", "account-secret", "deployment-secret", "replacement-secret", "new-header-secret", "rejected-secret", "invalid-secret", "query-secret"} {
		assert.NotContains(t, string(encoded), secret)
	}
	var success, failure bool
	for _, entry := range logs {
		success = success || entry.Success
		failure = failure || !entry.Success
	}
	assert.True(t, success)
	assert.True(t, failure)
}

func TestAdminAPIBrowserSessionRequiresExplicitBearer(t *testing.T) {
	engine, admin, _ := newAdminTestServer(t)
	bundle, err := service.CreateLoginSession(admin.Id, "password", "127.0.0.1", "regression")
	require.NoError(t, err)
	for _, origin := range []string{"https://attacker.example", "http://example.com"} {
		request := httptest.NewRequest(http.MethodPost, "/api/admin/v1/models", strings.NewReader(`{}`))
		request.Header.Set("Origin", origin)
		request.AddCookie(&http.Cookie{Name: service.RefreshCookieName, Value: bundle.RefreshToken})
		request.AddCookie(&http.Cookie{Name: "session", Value: bundle.AccessToken})
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, request)
		assert.Equal(t, http.StatusUnauthorized, recorder.Code)
	}
	response := adminSecurityRequest(engine, bundle.AccessToken, http.MethodGet, "/api/admin/v1/models", "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	require.NoError(t, model.DB.Model(&model.UserSession{}).Where("sid = ?", bundle.Session.SID).Update("status", model.UserSessionStatusRevoked).Error)
	rejected := adminSecurityRequest(engine, bundle.AccessToken, http.MethodPost, "/api/admin/v1/models", `{}`)
	assert.Equal(t, http.StatusUnauthorized, rejected.Code, rejected.Body.String())
}

func TestAdminAPIEveryRouteRejectsInvalidAuthority(t *testing.T) {
	engine, admin, granted := newAdminTestServer(t)
	ordinary := &model.User{Username: "ordinary", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, AuthVersion: 1, AffCode: "ordinary"}
	require.NoError(t, model.DB.Create(ordinary).Error)
	ordinaryPAT := createWalletTestAccessToken(t, ordinary.Id, "model:write", "channel:write", "ledger:write")
	limited := createWalletTestAccessToken(t, admin.Id, "profile:read")
	expired := createWalletTestAccessToken(t, admin.Id, "model:write")
	require.NoError(t, model.DB.Model(&model.UserAccessToken{}).Where("token_hash = ?", model.AccessTokenFingerprint(expired)).Update("expires_at", time.Now().Unix()-1).Error)
	revoked := createWalletTestAccessToken(t, admin.Id, "model:write")
	require.NoError(t, model.DB.Where("token_hash = ?", model.AccessTokenFingerprint(revoked)).Delete(&model.UserAccessToken{}).Error)
	bundle, err := service.CreateLoginSession(admin.Id, "password", "127.0.0.1", "regression")
	require.NoError(t, err)
	// Denials apply to every existing resource, and to browser session access
	// tokens as well as scoped PATs. Fixtures never reach business handlers.
	require.NoError(t, authz.SetUserPermissions(admin.Id, authz.PermissionsMap{
		"model": {"read": false, "write": false}, "channel": {"read": false, "write": false, "operate": false, "sensitive_write": false}, "ledger": {"read": false, "write": false},
	}))
	cases := []struct {
		name, token string
		status      int
	}{
		{"anonymous", "", 401}, {"ordinary user", ordinaryPAT, 403}, {"expired PAT", expired, 401}, {"revoked PAT", revoked, 401},
		{"scope denied", limited, 403}, {"Casbin PAT denial", granted, 403}, {"Casbin session denial", bundle.AccessToken, 403},
	}
	for _, route := range engine.Routes() {
		if !strings.HasPrefix(route.Path, "/api/admin/v1/") {
			continue
		}
		path := strings.NewReplacer(":id", "1", ":user_id", "1", ":index", "0").Replace(route.Path)
		for _, tc := range cases {
			t.Run(route.Method+" "+route.Path+"/"+tc.name, func(t *testing.T) {
				response := adminSecurityRequest(engine, tc.token, route.Method, path, `{}`)
				assert.Equal(t, tc.status, response.Code, response.Body.String())
			})
		}
	}
	logs, _, err := model.GetAuditLogs(model.AuditLogFilter{Category: model.AuditCategoryOperation}, 0, 1000, common.RoleRootUser)
	require.NoError(t, err)
	for _, entry := range logs {
		assert.False(t, entry.Success)
	}
}

func TestAdminAPIUndeclaredRoutesFailClosed(t *testing.T) {
	engine, user, token := newAdminTestServer(t)
	// A future resource registered without the permission helper must not run,
	// even if a developer forgets to add resource-level AdminAuth.
	engine.POST("/api/admin/v1/future-resource", func(c *gin.Context) { c.Status(http.StatusCreated) })
	bundle, err := service.CreateLoginSession(user.Id, "password", "127.0.0.1", "regression")
	require.NoError(t, err)
	for _, credential := range []string{token, bundle.AccessToken, ""} {
		response := adminSecurityRequest(engine, credential, http.MethodPost, "/api/admin/v1/future-resource", `{}`)
		assert.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, response.Code)
	}
}

func TestAdminAPIModelsHonorCasbinDenials(t *testing.T) {
	engine, user, token := newAdminTestServer(t)
	// An explicit per-user denial must override the built-in admin grant.
	require.NoError(t, model.DB.Create(&model.CasbinRule{Ptype: "p", V0: authz.UserSubject(user.Id), V1: "model", V2: "write", V3: authz.EffectDeny}).Error)
	require.NoError(t, authz.ReloadPolicy())
	response := adminSecurityRequest(engine, token, http.MethodPost, "/api/admin/v1/models", `{}`)
	assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
}

func TestAdminAPIRejectedWritesAreAudited(t *testing.T) {
	engine, _, _ := newAdminTestServer(t)
	response := adminSecurityRequest(engine, "", http.MethodPost, "/api/admin/v1/models?key=query-secret", `{"key":"body-secret"}`)
	require.Equal(t, http.StatusUnauthorized, response.Code)
	logs, total, err := model.GetAuditLogs(model.AuditLogFilter{Category: model.AuditCategoryOperation}, 0, 20, common.RoleRootUser)
	require.NoError(t, err)
	require.EqualValues(t, 1, total, "authentication rejection must leave an operation audit")
	assert.False(t, logs[0].Success)
	assert.Equal(t, http.StatusUnauthorized, logs[0].Status)
	assert.Equal(t, "/api/admin/v1/models", logs[0].Route)
	encoded, err := common.Marshal(logs)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "query-secret")
	assert.NotContains(t, string(encoded), "body-secret")
}
