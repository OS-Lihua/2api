package router

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKeyPermissionsHiddenAndMissingCallsAreIndistinguishable(t *testing.T) {
	calls := 0
	f := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { calls++ })
	pat := createWalletTestAccessToken(t, f.user.Id, "api_key:read", "api_key:write")
	endpoint := fmt.Sprintf("/api/token/%d/permissions", f.token.Id)
	deny := adminSecurityRequest(f.engine, pat, http.MethodPatch, endpoint, `{"action":"deny_all","revision":0}`)
	require.Equal(t, http.StatusOK, deny.Code, deny.Body.String())
	var errorType, errorMessage string
	for i, name := range []string{"gateway-alias", "missing-offering"} {
		response := adminSecurityRequest(f.engine, f.token.Key, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"Hello"}]}`, name))
		assert.Equal(t, http.StatusNotFound, response.Code, "%s: %s", name, response.Body.String())
		var payload struct {
			Error struct {
				Code    string `json:"code"`
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
		assert.Equal(t, "model_not_found", payload.Error.Code, name)
		if i == 0 {
			errorType, errorMessage = payload.Error.Type, payload.Error.Message
		} else {
			assert.Equal(t, errorType, payload.Error.Type, name)
			assert.Equal(t, errorMessage, payload.Error.Message, name)
		}
	}
	assert.Zero(t, calls, "neither inaccessible nor nonexistent models may dispatch")
}

func TestKeyPermissionsLegacyAllowlistMigration(t *testing.T) {
	f := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	})
	other := model.ModelOffering{Name: "outside-legacy-list", Members: f.offering.Members}
	require.NoError(t, model.SaveModelOffering(t.Context(), &other, "save"))
	price := model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: other.ID, Currency: f.sale.Currency, Expression: f.sale.Expression, EffectiveAt: f.fake.Now().Unix() - 1}
	require.NoError(t, model.CreatePriceVersion(t.Context(), &price, f.sale.CreatedBy))
	require.NoError(t, model.SaveModelOffering(t.Context(), &other, "enable"))
	require.NoError(t, model.DB.Model(f.token).Updates(map[string]any{"model_limits_enabled": true, "model_limits": "gateway-alias,missing-legacy-name"}).Error)
	require.NoError(t, model.MigrateKeyPermissions(model.DB))
	require.NoError(t, model.MigrateKeyPermissions(model.DB))
	pat := createWalletTestAccessToken(t, f.user.Id, "api_key:read")
	permissions := adminSecurityRequest(f.engine, pat, http.MethodGet, fmt.Sprintf("/api/token/%d/permissions", f.token.Id), "")
	assert.Contains(t, permissions.Body.String(), `"default_allow":false`)
	assert.Contains(t, permissions.Body.String(), fmt.Sprintf(`"exception_ids":[%d]`, f.offering.ID))
	denied := adminSecurityRequest(f.engine, f.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"outside-legacy-list","messages":[{"role":"user","content":"Hello"}]}`)
	assert.Equal(t, 404, denied.Code, denied.Body.String())
	f.offering.Name = "renamed-legacy-offering"
	f.offering.Aliases = []string{}
	require.NoError(t, model.SaveModelOffering(t.Context(), f.offering, "save"))
	allowed := adminSecurityRequest(f.engine, f.token.Key, http.MethodGet, "/v1/models/renamed-legacy-offering", "")
	assert.Equal(t, 200, allowed.Code, allowed.Body.String())
	_, err := model.MutateKeyPermissions(t.Context(), f.token.Id, f.user.Id, 0, "allow_all", model.KeyPermissions{}, nil)
	require.NoError(t, err)
	require.NoError(t, model.MigrateKeyPermissions(model.DB))
	allowed = adminSecurityRequest(f.engine, f.token.Key, http.MethodGet, "/v1/models/outside-legacy-list", "")
	assert.Equal(t, 200, allowed.Code, allowed.Body.String())
}

func TestKeyPermissionsDiscoveryProtocols(t *testing.T) {
	f := newTextGatewayFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected dispatch") })
	for _, path := range []string{"/v1/models/", "/v1/models/gateway-alias/", "/v1beta/models/", "/v1beta/models/gateway-alias/"} {
		response := adminSecurityRequest(f.engine, f.token.Key, http.MethodGet, path+"?limit=7", "")
		assert.Equal(t, http.StatusMovedPermanently, response.Code, response.Body.String())
		assert.Equal(t, strings.TrimSuffix(path, "/")+"?limit=7", response.Header().Get("Location"))
	}
	for _, tc := range []struct{ path, header, field string }{{"/v1/models/gateway-alias", "x-api-key", "type"}, {"/v1beta/models/gateway-alias", "x-goog-api-key", "name"}} {
		request := httptest.NewRequest(http.MethodGet, tc.path, nil)
		request.Header.Set(tc.header, f.token.Key)
		request.Header.Set("anthropic-version", "2023-06-01")
		response := httptest.NewRecorder()
		f.engine.ServeHTTP(response, request)
		require.Equal(t, 200, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), "gateway-offering")
		assert.Contains(t, response.Header().Get("Cache-Control"), "no-store")
	}
	_, err := model.MutateKeyPermissions(t.Context(), f.token.Id, f.user.Id, 0, "deny_all", model.KeyPermissions{}, nil)
	require.NoError(t, err)
	for _, language := range []struct{ code, message string }{{"en", "Model not found"}, {"zh-CN", "模型不存在"}, {"zh-TW", "模型不存在"}} {
		request := httptest.NewRequest(http.MethodGet, "/v1/models/gateway-alias", nil)
		request.Header.Set("Authorization", "Bearer "+f.token.Key)
		request.Header.Set("Accept-Language", language.code)
		response := httptest.NewRecorder()
		f.engine.ServeHTTP(response, request)
		assert.Equal(t, 404, response.Code)
		assert.Contains(t, response.Body.String(), language.message)
	}
	for _, tc := range []struct{ path, header, expected string }{{"/v1/models/gateway-alias", "x-api-key", `"type":"not_found_error"`}, {"/v1beta/models/gateway-alias", "x-goog-api-key", `"status":"NOT_FOUND"`}} {
		request := httptest.NewRequest(http.MethodGet, tc.path, nil)
		request.Header.Set(tc.header, f.token.Key)
		request.Header.Set("anthropic-version", "2023-06-01")
		response := httptest.NewRecorder()
		f.engine.ServeHTTP(response, request)
		assert.Equal(t, 404, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), tc.expected)
		assert.NotContains(t, response.Body.String(), "gateway-offering")
	}
}

func TestKeyPermissionsConcurrentEditsAndCredentialRestrictions(t *testing.T) {
	f := newTextGatewayFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected dispatch") })
	pat := createWalletTestAccessToken(t, f.user.Id, "api_key:read", "api_key:write")
	endpoint := fmt.Sprintf("/api/token/%d/permissions", f.token.Id)
	var group sync.WaitGroup
	codes := make(chan int, 2)
	for range 2 {
		group.Go(func() {
			codes <- adminSecurityRequest(f.engine, pat, http.MethodPatch, endpoint, `{"action":"deny_all","revision":0}`).Code
		})
	}
	group.Wait()
	close(codes)
	results := []int{}
	for code := range codes {
		results = append(results, code)
	}
	slices.Sort(results)
	assert.Equal(t, []int{200, 409}, results)
	for _, change := range []struct {
		name  string
		value any
	}{{"status", common.TokenStatusDisabled}, {"status", common.TokenStatusExpired}, {"status", common.TokenStatusExhausted}, {"expired_time", int64(1)}, {"allow_ips", "203.0.113.0/24"}} {
		require.NoError(t, model.DB.Model(f.token).Updates(map[string]any{"status": common.TokenStatusEnabled, "expired_time": -1, "allow_ips": ""}).Error)
		require.NoError(t, model.DB.Model(f.token).Update(change.name, change.value).Error)
		for _, path := range []string{"/v1/models", "/v1/models/gateway-alias"} {
			response := adminSecurityRequest(f.engine, f.token.Key, http.MethodGet, path, "")
			assert.Contains(t, []int{401, 403}, response.Code, response.Body.String())
		}
		for _, name := range []string{"gateway-alias", "missing-offering"} {
			response := adminSecurityRequest(f.engine, f.token.Key, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"Hello"}]}`, name))
			assert.Contains(t, []int{401, 403}, response.Code, response.Body.String())
			assert.NotContains(t, response.Body.String(), "model_not_found")
		}
	}
	require.NoError(t, model.DB.Model(f.token).Updates(map[string]any{"status": common.TokenStatusEnabled, "expired_time": -1, "allow_ips": ""}).Error)
	require.NoError(t, model.DB.Model(f.user).Update("status", common.UserStatusDisabled).Error)
	for _, name := range []string{"gateway-alias", "missing-offering"} {
		response := adminSecurityRequest(f.engine, f.token.Key, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"Hello"}]}`, name))
		assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	}
	require.NoError(t, model.DB.Model(f.user).Update("status", common.UserStatusEnabled).Error)
	require.NoError(t, f.token.Delete())
	assert.Equal(t, 401, adminSecurityRequest(f.engine, f.token.Key, http.MethodGet, "/v1/models", "").Code)
	for _, name := range []string{"gateway-alias", "missing-offering"} {
		response := adminSecurityRequest(f.engine, f.token.Key, http.MethodPost, "/v1/chat/completions", fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"Hello"}]}`, name))
		assert.Equal(t, http.StatusUnauthorized, response.Code, response.Body.String())
	}
	for _, revoke := range []bool{false, true} {
		credential := createWalletTestAccessToken(t, f.user.Id, "api_key:write")
		if revoke {
			require.NoError(t, model.DB.Where("token_hash = ?", model.AccessTokenFingerprint(credential)).Delete(&model.UserAccessToken{}).Error)
		} else {
			require.NoError(t, model.DB.Model(&model.UserAccessToken{}).Where("token_hash = ?", model.AccessTokenFingerprint(credential)).Update("expires_at", f.fake.Now().Unix()-1).Error)
		}
		assert.Equal(t, 401, adminSecurityRequest(f.engine, credential, http.MethodPatch, endpoint, `{"action":"allow_all","revision":1}`).Code)
	}
}

func TestKeyPermissionsAdminAndSessionAuthorization(t *testing.T) {
	f := newTextGatewayFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected dispatch") })
	endpoint := fmt.Sprintf("/api/admin/v1/tokens/%d/permissions", f.token.Id)
	granted := createWalletTestAccessToken(t, f.sale.CreatedBy, "key_permission:read", "key_permission:write")
	readOnly := createWalletTestAccessToken(t, f.sale.CreatedBy, "key_permission:read")
	assert.Equal(t, 200, adminSecurityRequest(f.engine, granted, http.MethodGet, endpoint, "").Code)
	assert.Equal(t, 403, adminSecurityRequest(f.engine, readOnly, http.MethodPatch, endpoint, `{"action":"deny_all","revision":0}`).Code)
	require.NoError(t, authz.SetUserPermissions(f.sale.CreatedBy, authz.PermissionsMap{"key_permission": {"read": false, "write": false}}))
	assert.Equal(t, 403, adminSecurityRequest(f.engine, granted, http.MethodGet, endpoint, "").Code)
	assert.Equal(t, 403, adminSecurityRequest(f.engine, granted, http.MethodPatch, endpoint, `{"action":"deny_all","revision":0}`).Code)
	bundle, err := service.CreateLoginSession(f.user.Id, "password", "127.0.0.1", "key-permissions-test")
	require.NoError(t, err)
	self := fmt.Sprintf("/api/token/%d/permissions", f.token.Id)
	assert.Equal(t, 200, adminSecurityRequest(f.engine, bundle.AccessToken, http.MethodGet, self, "").Code)
	request := httptest.NewRequest(http.MethodPatch, self, strings.NewReader(`{"action":"deny_all","revision":0}`))
	request.Header.Set("Origin", "https://attacker.example")
	request.AddCookie(&http.Cookie{Name: service.RefreshCookieName, Value: bundle.RefreshToken})
	request.AddCookie(&http.Cookie{Name: "session", Value: bundle.AccessToken})
	recorder := httptest.NewRecorder()
	f.engine.ServeHTTP(recorder, request)
	assert.Equal(t, 401, recorder.Code)
	_, err = model.RevokeUserSession(f.user.Id, bundle.Session.SID, "key-permissions-test")
	require.NoError(t, err)
	assert.Equal(t, 401, adminSecurityRequest(f.engine, bundle.AccessToken, http.MethodPatch, self, `{"action":"deny_all","revision":0}`).Code)
}

func TestKeyPermissionsIdentityVisibilityAndDeletion(t *testing.T) {
	f := newTextGatewayFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected dispatch") })
	cache := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: cache.Addr()})
	oldEnabled, oldClient := common.RedisEnabled, common.RDB
	common.RedisEnabled, common.RDB = true, client
	t.Cleanup(func() { common.RedisEnabled, common.RDB = oldEnabled, oldClient; require.NoError(t, client.Close()) })
	pat := createWalletTestAccessToken(t, f.user.Id, "api_key:read", "api_key:write")
	endpoint := fmt.Sprintf("/api/token/%d/permissions", f.token.Id)
	response := adminSecurityRequest(f.engine, pat, http.MethodPut, endpoint, fmt.Sprintf(`{"default_allow":false,"exception_ids":[%d],"revision":0}`, f.offering.ID))
	require.Equal(t, 200, response.Code, response.Body.String())
	f.offering.Name = "namespace/renamed-offering"
	f.offering.Aliases = append(f.offering.Aliases, "namespace/trailing/")
	require.NoError(t, model.SaveModelOffering(t.Context(), f.offering, "save"))
	for _, name := range []string{"gateway-offering", "gateway-alias", "namespace/renamed-offering", "namespace%2Ftrailing%2F"} {
		detail := adminSecurityRequest(f.engine, f.token.Key, http.MethodGet, "/v1/models/"+name, "")
		require.Equal(t, 200, detail.Code, detail.Body.String())
		assert.Contains(t, detail.Body.String(), `"id":"namespace/renamed-offering"`)
	}
	for _, name := range []string{"namespace/renamed-offering", "namespace%2Ftrailing%2F"} {
		request := httptest.NewRequest(http.MethodGet, "/v1beta/models/"+name, nil)
		request.Header.Set("x-goog-api-key", f.token.Key)
		response := httptest.NewRecorder()
		f.engine.ServeHTTP(response, request)
		assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), `"name":"namespace/renamed-offering"`)
	}
	f.offering.Visibility = "groups"
	f.offering.Groups = []string{"default"}
	require.NoError(t, model.SaveModelOffering(t.Context(), f.offering, "save"))
	require.NoError(t, model.DB.Model(f.user).Update("group", "private").Error)
	denied := adminSecurityRequest(f.engine, f.token.Key, http.MethodGet, "/v1/models/gateway-alias", "")
	assert.Equal(t, 404, denied.Code, denied.Body.String())
	read := adminSecurityRequest(f.engine, pat, http.MethodGet, endpoint, "")
	assert.Contains(t, read.Body.String(), fmt.Sprintf(`"exception_ids":[%d]`, f.offering.ID))
	require.NoError(t, model.DB.Model(f.user).Update("group", "default").Error)
	restored := adminSecurityRequest(f.engine, f.token.Key, http.MethodGet, "/v1/models/gateway-alias", "")
	assert.Equal(t, 200, restored.Code, restored.Body.String())
	admin := createWalletTestAccessToken(t, f.sale.CreatedBy, "key_permission:read", "key_permission:write", "model:write")
	adminEndpoint := fmt.Sprintf("/api/admin/v1/tokens/%d/permissions", f.token.Id)
	response = adminSecurityRequest(f.engine, admin, http.MethodGet, adminEndpoint, "")
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.NotContains(t, response.Body.String(), f.token.Key)
	draft := &model.ModelOffering{Name: "deleted-offering", Members: f.offering.Members}
	require.NoError(t, model.SaveModelOffering(t.Context(), draft, "save"))
	response = adminSecurityRequest(f.engine, pat, http.MethodPut, endpoint, fmt.Sprintf(`{"default_allow":false,"exception_ids":[%d],"revision":1}`, draft.ID))
	require.Equal(t, 200, response.Code, response.Body.String())
	response = adminSecurityRequest(f.engine, admin, http.MethodDelete, fmt.Sprintf("/api/admin/v1/model-offerings/%d", draft.ID), "")
	require.Equal(t, 200, response.Code, response.Body.String())
	response = adminSecurityRequest(f.engine, pat, http.MethodGet, endpoint, "")
	assert.Contains(t, response.Body.String(), `"exception_ids":[]`)
}

func TestKeyPermissionsListDetailAndCall(t *testing.T) {
	calls := 0
	f := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	})
	// Model discovery is a wallet path, even with no legacy integer quota.
	list := adminSecurityRequest(f.engine, f.token.Key, http.MethodGet, "/v1/models", "")
	require.Equal(t, 200, list.Code, list.Body.String())
	assert.Contains(t, list.Body.String(), `"id":"gateway-offering"`)
	assert.NotContains(t, list.Body.String(), "gateway-alias")
	detail := adminSecurityRequest(f.engine, f.token.Key, http.MethodGet, "/v1/models/gateway-alias", "")
	require.Equal(t, 200, detail.Code, detail.Body.String())
	assert.Contains(t, detail.Body.String(), `"id":"gateway-offering"`)
	assert.Contains(t, detail.Body.String(), fmt.Sprintf(`"members":[{"model_id":%d,"tier":1}]`, f.channel.ModelID))
	pat := createWalletTestAccessToken(t, f.user.Id, "api_key:read", "api_key:write")
	endpoint := fmt.Sprintf("/api/token/%d/permissions", f.token.Id)
	deniedPolicy := adminSecurityRequest(f.engine, pat, http.MethodPatch, endpoint, `{"action":"deny_all","revision":0}`)
	require.Equal(t, 200, deniedPolicy.Code, deniedPolicy.Body.String())
	// PATCH replace must not turn the denied Offering into an allow exception.
	rejected := adminSecurityRequest(f.engine, pat, http.MethodPatch, endpoint, fmt.Sprintf(`{"action":"replace","default_allow":true,"exception_ids":[%d],"revision":1}`, f.offering.ID))
	assert.Equal(t, 400, rejected.Code, rejected.Body.String())
	list = adminSecurityRequest(f.engine, f.token.Key, http.MethodGet, "/v1/models", "")
	assert.Contains(t, list.Body.String(), `"data":[]`)
	denied := adminSecurityRequest(f.engine, f.token.Key, http.MethodGet, "/v1/models/gateway-alias", "")
	missing := adminSecurityRequest(f.engine, f.token.Key, http.MethodGet, "/v1/models/missing", "")
	assert.Equal(t, 404, denied.Code, denied.Body.String())
	assert.Equal(t, missing.Code, denied.Code)
	assert.JSONEq(t, missing.Body.String(), denied.Body.String())
	call := adminSecurityRequest(f.engine, f.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}]}`)
	assert.Equal(t, 404, call.Code, call.Body.String())
	assert.Contains(t, call.Body.String(), `"code":"model_not_found"`)
	assert.Zero(t, calls)
	opened := adminSecurityRequest(f.engine, pat, http.MethodPatch, endpoint, fmt.Sprintf(`{"action":"add","offering_ids":[%d],"revision":1}`, f.offering.ID))
	require.Equal(t, 200, opened.Code, opened.Body.String())
	call = adminSecurityRequest(f.engine, f.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}]}`)
	assert.Equal(t, 200, call.Code, call.Body.String())
	assert.Equal(t, 1, calls)
	// A default-allow Key uses exceptions to deny exactly these identities.
	opened = adminSecurityRequest(f.engine, pat, http.MethodPut, endpoint, fmt.Sprintf(`{"default_allow":true,"exception_ids":[%d],"revision":2}`, f.offering.ID))
	require.Equal(t, 200, opened.Code, opened.Body.String())
	for _, path := range []string{"/v1/models/gateway-offering", "/v1/models/gateway-alias"} {
		response := adminSecurityRequest(f.engine, f.token.Key, http.MethodGet, path, "")
		assert.Equal(t, 404, response.Code, response.Body.String())
	}
	list = adminSecurityRequest(f.engine, f.token.Key, http.MethodGet, "/v1/models", "")
	assert.Contains(t, list.Body.String(), `"data":[]`)
	call = adminSecurityRequest(f.engine, f.token.Key, http.MethodPost, "/v1/chat/completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}]}`)
	assert.Equal(t, 404, call.Code, call.Body.String())
	assert.Equal(t, 1, calls)
	records := adminSecurityRequest(f.engine, f.userPAT, http.MethodGet, "/api/requests/self", "")
	require.Equal(t, 200, records.Code, records.Body.String())
	var logged walletAPIResponse
	require.NoError(t, common.Unmarshal(records.Body.Bytes(), &logged))
	assert.EqualValues(t, 1, logged.Data["total"], "denied calls must not create billable request records")
}

func TestKeyPermissionsOperations(t *testing.T) {
	f := newTextGatewayFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected dispatch") })
	pat := createWalletTestAccessToken(t, f.user.Id, "api_key:read", "api_key:write")
	endpoint := fmt.Sprintf("/api/token/%d/permissions", f.token.Id)
	var revision int64
	for _, tc := range []struct {
		action string
		allow  bool
		ids    []int
	}{
		{"deny_all", false, []int{}}, {"add", false, []int{f.offering.ID}}, {"remove", false, []int{}}, {"allow_all", true, []int{}},
	} {
		t.Run(tc.action, func(t *testing.T) {
			response := adminSecurityRequest(f.engine, pat, http.MethodPatch, endpoint, fmt.Sprintf(`{"action":%q,"offering_ids":[%d],"revision":%d}`, tc.action, f.offering.ID, revision))
			require.Equal(t, 200, response.Code, response.Body.String())
			var payload struct {
				Success bool
				Data    model.KeyPermissions
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
			require.True(t, payload.Success)
			assert.Equal(t, tc.allow, payload.Data.DefaultAllow)
			assert.Equal(t, tc.ids, payload.Data.ExceptionIDs)
			assert.Equal(t, revision+1, payload.Data.Revision)
			revision = payload.Data.Revision
		})
	}
	replace := fmt.Sprintf(`{"default_allow":false,"exception_ids":[%d],"revision":%d}`, f.offering.ID, revision)
	response := adminSecurityRequest(f.engine, pat, http.MethodPut, endpoint, replace)
	require.Equal(t, 200, response.Code, response.Body.String())
	stale := adminSecurityRequest(f.engine, pat, http.MethodPut, endpoint, replace)
	assert.Equal(t, http.StatusConflict, stale.Code, stale.Body.String())
	readOnly := createWalletTestAccessToken(t, f.user.Id, "api_key:read")
	assert.Equal(t, 403, adminSecurityRequest(f.engine, readOnly, http.MethodPatch, endpoint, `{"action":"allow_all"}`).Code)
	entries, _, err := model.GetAuditLogs(model.AuditLogFilter{UserId: f.user.Id, Category: model.AuditCategorySecurity}, 0, 100, common.RoleRootUser)
	require.NoError(t, err)
	var success, failure bool
	for _, entry := range entries {
		if entry.Action == "token.permissions.update" {
			success = success || entry.Success
			failure = failure || !entry.Success
		}
	}
	assert.True(t, success, "successful permission changes must leave audit events")
	assert.True(t, failure, "rejected stale writes must leave audit events")
	encoded, err := common.Marshal(entries)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), pat)
	assert.NotContains(t, string(encoded), f.token.Key)
}

func TestKeyPermissionsDefaultAndOwnership(t *testing.T) {
	f := newTextGatewayFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("permission read must not dispatch") })
	pat := createWalletTestAccessToken(t, f.user.Id, "api_key:read", "api_key:write")
	endpoint := fmt.Sprintf("/api/token/%d/permissions", f.token.Id)
	response := adminSecurityRequest(f.engine, pat, http.MethodGet, endpoint, "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var payload struct {
		Success bool
		Data    struct {
			DefaultAllow bool  `json:"default_allow"`
			ExceptionIDs []int `json:"exception_ids"`
			Revision     int64 `json:"revision"`
		}
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
	require.True(t, payload.Success)
	assert.True(t, payload.Data.DefaultAllow)
	assert.Empty(t, payload.Data.ExceptionIDs)
	assert.NotContains(t, response.Body.String(), f.token.Key)
	other := model.User{Username: "other-key-owner", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, AuthVersion: 1, AffCode: "other-key-owner"}
	require.NoError(t, model.DB.Create(&other).Error)
	otherPAT := createWalletTestAccessToken(t, other.Id, "api_key:read", "api_key:write")
	denied := adminSecurityRequest(f.engine, otherPAT, http.MethodGet, endpoint+"?user_id="+fmt.Sprint(f.user.Id), "")
	missing := adminSecurityRequest(f.engine, otherPAT, http.MethodGet, "/api/token/999999/permissions", "")
	assert.Equal(t, http.StatusNotFound, denied.Code)
	assert.Equal(t, missing.Code, denied.Code)
	assert.JSONEq(t, missing.Body.String(), denied.Body.String())
	for _, method := range []string{http.MethodPut, http.MethodPatch} {
		body := fmt.Sprintf(`{"action":"deny_all","default_allow":false,"exception_ids":[],"revision":0,"user_id":%d,"token_id":%d}`, f.user.Id, f.token.Id)
		denied = adminSecurityRequest(f.engine, otherPAT, method, endpoint, body)
		missing = adminSecurityRequest(f.engine, otherPAT, method, "/api/token/999999/permissions", body)
		assert.Equal(t, 404, denied.Code, denied.Body.String())
		assert.JSONEq(t, missing.Body.String(), denied.Body.String())
	}
}
