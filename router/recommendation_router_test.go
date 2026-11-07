package router

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecommendationAdminAPI(t *testing.T) {
	previousMaster, previousRedis := common.IsMasterNode, common.RedisEnabled
	t.Cleanup(func() { common.IsMasterNode, common.RedisEnabled = previousMaster, previousRedis })
	common.IsMasterNode, common.RedisEnabled = true, false
	db := model.UseTestDatabases(t)
	require.NoError(t, i18n.Init())
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.UserSession{}, &model.CasbinRule{}, &model.AuthzRole{}, &model.UserAccessToken{}, &model.Option{}, &model.Vendor{}, &model.Model{}, &model.Provider{}, &model.Ability{}))
	require.NoError(t, model.MigrateAuditLogs())
	require.NoError(t, model.MigrateRecommendationRules(db))
	require.NoError(t, model.EnsureLegacyAccessTokenRetireAt(time.Now().Unix()))
	require.NoError(t, authz.Init(db))
	root := &model.User{Username: "recommendation-root", Role: common.RoleRootUser, Status: common.UserStatusEnabled, AuthVersion: 1, AffCode: "rec-root"}
	require.NoError(t, db.Create(root).Error)
	token := createWalletTestAccessToken(t, root.Id, "recommendation:read", "recommendation:write", "channel:sensitive_write", "channel:read", "model:write", "model:read", "audit:read")
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	SetApiRouter(engine)
	call := func(method, path, body string) map[string]any {
		if !strings.HasPrefix(path, "/api/") {
			path = "/api/admin/v1/" + path
		}
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, request)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		var response struct {
			Success bool           `json:"success"`
			Data    map[string]any `json:"data"`
		}
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
		require.True(t, response.Success)
		return response.Data
	}
	listed := call(http.MethodGet, "recommendation-rules?kind=provider", "")
	require.NotEmpty(t, listed["items"])
	matched := call(http.MethodGet, "recommendations/provider?url=https%3A%2F%2Fapi.deepseek.com%3A8443%2Fv1&type=1", "")
	items := matched["items"].([]any)
	require.Len(t, items, 1)
	assert.Equal(t, "DeepSeek", items[0].(map[string]any)["provider_name"])
	assert.EqualValues(t, 43, items[0].(map[string]any)["provider_type"])

	t.Run("host boundaries and official default", func(t *testing.T) {
		for _, tc := range []struct {
			address string
			kind    int
			name    string
		}{
			{"https://API.DEEPSEEK.COM.:8443/v1", 1, "DeepSeek"},
			{"", 43, "DeepSeek"}, {"", 1, "OpenAI"}, {"", 3, ""}, {"", 999, ""},
			{"https://tenant.openai.azure.com:443/v1", 1, "Azure OpenAI"},
			{"https://nested.tenant.openai.azure.com", 1, "Azure OpenAI"},
			{"https://openai.azure.com", 1, ""}, {"https://evilopenai.azure.com", 1, ""},
			{"https://tenant.openai.azure.com.evil.example", 1, ""},
			{"https://api.deepseek.com.evil.example", 43, ""}, {"https://evilapi.deepseek.com", 43, ""},
			{"https://api.deepseek.com@evil.example/v1", 43, ""}, {"https://evil.example/api.deepseek.com", 43, ""},
			{"https://.openai.azure.com", 1, ""}, {"https://a..openai.azure.com", 1, ""},
			{"not a URL", 43, ""}, {"file://api.deepseek.com", 43, ""},
		} {
			t.Run(tc.address+fmt.Sprint(tc.kind), func(t *testing.T) {
				data := call(http.MethodGet, "recommendations/provider?url="+url.QueryEscape(tc.address)+"&type="+fmt.Sprint(tc.kind), "")
				candidates := data["items"].([]any)
				if tc.name == "" {
					assert.Empty(t, candidates)
					return
				}
				require.Len(t, candidates, 1)
				assert.Equal(t, tc.name, candidates[0].(map[string]any)["provider_name"])
			})
		}
	})
	t.Run("rule edits and deletion survive restart", func(t *testing.T) {
		deepseek := items[0].(map[string]any)
		id := fmt.Sprint(deepseek["id"])
		call(http.MethodPut, "recommendation-rules/"+id, `{"kind":"provider","pattern":"api.deepseek.com","provider_name":"My DeepSeek","provider_type":43,"enabled":true}`)
		require.NoError(t, model.MigrateRecommendationRules(db))
		changed := call(http.MethodGet, "recommendations/provider?type=43", "")["items"].([]any)
		require.Len(t, changed, 1)
		assert.Equal(t, "My DeepSeek", changed[0].(map[string]any)["provider_name"])
		call(http.MethodDelete, "recommendation-rules/"+id, "")
		require.NoError(t, model.MigrateRecommendationRules(db))
		require.NoError(t, model.MigrateRecommendationRules(db))
		assert.Empty(t, call(http.MethodGet, "recommendations/provider?type=43", "")["items"])
		first := call(http.MethodPost, "recommendation-rules", `{"kind":"provider","pattern":"*.example.com","provider_name":"General","provider_type":1,"enabled":true}`)
		specific := call(http.MethodPost, "recommendation-rules", `{"kind":"provider","pattern":"api.example.com","provider_name":"Specific","provider_type":14,"enabled":true}`)
		override := call(http.MethodPost, "recommendation-rules", `{"kind":"provider","pattern":"*.example.com","provider_name":"Override","provider_type":24,"priority":10,"enabled":true}`)
		call(http.MethodPost, "recommendation-rules", `{"kind":"provider","pattern":"api.example.com","provider_name":"Disabled","provider_type":1,"priority":99,"enabled":false}`)
		sorted := call(http.MethodGet, "recommendations/provider?url=https://api.example.com", "")["items"].([]any)
		require.Len(t, sorted, 3)
		assert.Equal(t, override["id"], sorted[0].(map[string]any)["id"])
		assert.Equal(t, specific["id"], sorted[1].(map[string]any)["id"])
		assert.Equal(t, first["id"], sorted[2].(map[string]any)["id"])
	})
	t.Run("Publisher prefixes suggest existing identities without merging Models", func(t *testing.T) {
		assert.Empty(t, call(http.MethodGet, "recommendations/publisher?model_name=gpt-4o", "")["items"])
		publisher := &model.Vendor{Name: "OpenAI", Status: 1}
		require.NoError(t, db.Create(publisher).Error)
		for _, name := range []string{"gpt-4o", "GPT-4O-2024-08-06", "gpt-4o-q4_k_m"} {
			candidates := call(http.MethodGet, "recommendations/publisher?model_name="+name, "")["items"].([]any)
			require.Len(t, candidates, 1)
			assert.EqualValues(t, publisher.Id, candidates[0].(map[string]any)["publisher_id"])
			created := call(http.MethodPost, "models", fmt.Sprintf(`{"name":%q,"publisher_id":%d}`, name, publisher.Id))
			assert.Equal(t, name, created["name"])
		}
		assert.Empty(t, call(http.MethodGet, "recommendations/publisher?model_name=unknown", "")["items"])
		custom := &model.Vendor{Name: "Custom publisher", Status: 1}
		require.NoError(t, db.Create(custom).Error)
		rule := call(http.MethodPost, "recommendation-rules", fmt.Sprintf(`{"kind":"publisher","pattern":"gpt-4o-","publisher_id":%d,"enabled":true}`, custom.Id))
		candidates := call(http.MethodGet, "recommendations/publisher?model_name=gpt-4o-q8", "")["items"].([]any)
		require.Len(t, candidates, 2)
		assert.Equal(t, rule["id"], candidates[0].(map[string]any)["id"])
		assert.EqualValues(t, custom.Id, candidates[0].(map[string]any)["publisher_id"])
		// Manual choice wins over the recommendation; saving performs no inference.
		created := call(http.MethodPost, "models", fmt.Sprintf(`{"name":"gpt-4o-q8","publisher_id":%d}`, publisher.Id))
		assert.EqualValues(t, publisher.Id, created["publisher_id"])
		provider := call(http.MethodPost, "providers", `{"provider":{"name":"Manual account","type":1,"base_url":"https://api.anthropic.com","key":"synthetic-secret"}}`)
		providerID := provider["ids"].([]any)[0]
		saved := call(http.MethodGet, "providers/"+fmt.Sprint(providerID), "")
		assert.Equal(t, "Manual account", saved["name"])
		assert.EqualValues(t, 1, saved["type"])
	})
	t.Run("permissions and invalid requests", func(t *testing.T) {
		admin := &model.User{Username: "recommendation-admin", Role: common.RoleAdminUser, Status: common.UserStatusEnabled, AuthVersion: 1, AffCode: "rec-admin"}
		require.NoError(t, db.Create(admin).Error)
		adminToken := createWalletTestAccessToken(t, admin.Id, "recommendation:read", "recommendation:write")
		require.NoError(t, authz.SetUserPermissions(admin.Id, authz.PermissionsMap{authz.ResourceRecommendation: {authz.ActionRead: true, authz.ActionWrite: false}}))
		readOnly := createWalletTestAccessToken(t, root.Id, "recommendation:read")
		expired := createWalletTestAccessToken(t, root.Id, "recommendation:read")
		require.NoError(t, db.Model(&model.UserAccessToken{}).Where("token_hash = ?", model.AccessTokenFingerprint(expired)).Update("expires_at", time.Now().Unix()-1).Error)
		for _, tc := range []struct {
			token, method, path, body string
			status                    int
		}{
			{"", http.MethodGet, "recommendation-rules", "", 401},
			{expired, http.MethodGet, "recommendation-rules", "", 401},
			{readOnly, http.MethodPost, "recommendation-rules", `{}`, 403},
			{adminToken, http.MethodPost, "recommendation-rules", `{}`, 403},
			{adminToken, http.MethodPut, "recommendation-rules/1", `{}`, 403},
			{adminToken, http.MethodDelete, "recommendation-rules/1", "", 403},
			{token, http.MethodPost, "recommendation-rules", `{"kind":"provider","pattern":"*openai.com","provider_name":"Bad","provider_type":1}`, 400},
			{token, http.MethodPost, "recommendation-rules", `{"kind":"publisher","pattern":"gpt-","publisher_id":999999}`, 400},
			{token, http.MethodDelete, "recommendation-rules/999999", "", 404},
			{token, http.MethodGet, "recommendations/unknown", "", 400},
		} {
			request := httptest.NewRequest(tc.method, "/api/admin/v1/"+tc.path, strings.NewReader(tc.body))
			if tc.token != "" {
				request.Header.Set("Authorization", "Bearer "+tc.token)
			}
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, request)
			assert.Equal(t, tc.status, recorder.Code, recorder.Body.String())
		}
		audit := call(http.MethodGet, "/api/audit?p=1&page_size=100", "")
		encoded, err := common.Marshal(audit)
		require.NoError(t, err)
		assert.Contains(t, string(encoded), "recommendation_rule.save")
		assert.Contains(t, string(encoded), "recommendation_rule.delete")
		assert.NotContains(t, string(encoded), "synthetic-secret")
		assert.NotContains(t, string(encoded), token)
	})
}
