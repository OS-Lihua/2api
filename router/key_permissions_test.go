package router

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	pat := createWalletTestAccessToken(t, f.user.Id, "api_key:read", "api_key:write")
	endpoint := fmt.Sprintf("/api/token/%d/permissions", f.token.Id)
	deniedPolicy := adminSecurityRequest(f.engine, pat, http.MethodPatch, endpoint, `{"action":"deny_all","revision":0}`)
	require.Equal(t, 200, deniedPolicy.Code, deniedPolicy.Body.String())
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
}
