package router

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/capability"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// configuredChannelPrice models a configured price version, including zero.
type configuredChannelPrice struct{}

func (configuredChannelPrice) HasPurchasePrice(context.Context, *gorm.DB, int) (bool, error) {
	return true, nil
}

// Exercise the management boundary with actual PAT authentication and Casbin.
func TestSingleModelChannelAPI(t *testing.T) {
	model.SetChannelPurchasePriceChecker(nil)
	t.Cleanup(func() { model.SetChannelPurchasePriceChecker(nil) })
	previousMaster, previousRedis := common.IsMasterNode, common.RedisEnabled
	t.Cleanup(func() { common.IsMasterNode, common.RedisEnabled = previousMaster, previousRedis })
	common.IsMasterNode, common.RedisEnabled = true, false
	db := model.UseTestDatabases(t)
	require.NoError(t, i18n.Init())
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.UserSession{}, &model.CasbinRule{}, &model.AuthzRole{}, &model.UserAccessToken{}, &model.Option{}, &model.Provider{}, &model.Channel{}, &model.Ability{}, &model.TaskPlugin{}, &model.Model{}, &model.Vendor{}))
	require.NoError(t, model.MigrateAuditLogs())
	require.NoError(t, model.EnsureLegacyAccessTokenRetireAt(time.Now().Unix()))
	require.NoError(t, authz.Init(db))
	root := model.User{Username: "channel-root", Password: "placeholder", Role: common.RoleRootUser, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: "channel-root"}
	require.NoError(t, db.Create(&root).Error)
	token := createWalletTestAccessToken(t, root.Id, "channel:read", "channel:write", "channel:sensitive_write", "channel:operate", "model:write", "model:read")
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	SetApiRouter(engine)
	call := func(method, path, body string) (int, map[string]any) {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, request)
		result := map[string]any{}
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &result), recorder.Body.String())
		return recorder.Code, result
	}
	publisher := model.Vendor{Name: "Channel publisher", Status: 1}
	require.NoError(t, publisher.Insert())
	unified := model.Model{ModelName: "gpt-4o", VendorID: publisher.Id, Status: 1}
	require.NoError(t, unified.Insert())
	provider := model.Provider{Name: "Channel provider", Key: "secret", Status: common.ChannelStatusEnabled, Type: 1}
	require.NoError(t, db.Create(&provider).Error)
	body := fmt.Sprintf(`{"provider_id":%d,"model_id":%d,"protocol":"openai_chat","upstream_model_name":"openai/GPT-4o","rpm":60,"tpm":10000,"max_concurrency":2,"weight":10,"parameter_overrides":{"temperature":0,"private_marker":"channel-override-must-not-be-audited"}}`, provider.Id, unified.Id)
	status, created := call(http.MethodPost, "/api/admin/v1/channels", body)
	require.Equal(t, http.StatusOK, status, created)
	require.Equal(t, true, created["success"], created)
	data := created["data"].(map[string]any)
	assert.Equal(t, "pending", data["status"])
	assert.Equal(t, false, data["mapping_confirmed"])
	path := fmt.Sprintf("/api/admin/v1/channels/%v", data["id"])
	status, read := call(http.MethodGet, path, "")
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, data, read["data"])
	status, duplicate := call(http.MethodPost, "/api/admin/v1/channels", body)
	assert.Equal(t, http.StatusConflict, status, duplicate)
	status, _ = call(http.MethodPost, path+"/enable", "")
	assert.Equal(t, http.StatusBadRequest, status)
	status, confirmed := call(http.MethodPost, path+"/confirm", "")
	require.Equal(t, http.StatusOK, status, confirmed)
	assert.Equal(t, "disabled", confirmed["data"].(map[string]any)["status"])
	status, _ = call(http.MethodPost, path+"/enable", "")
	assert.Equal(t, http.StatusBadRequest, status, "no purchase price checker must deny activation")
	status, listed := call(http.MethodGet, fmt.Sprintf("/api/admin/v1/channels?provider_id=%d", provider.Id), "")
	require.Equal(t, http.StatusOK, status)
	assert.EqualValues(t, 1, listed["data"].(map[string]any)["total"])

	t.Run("capability narrowing and model edits stay consistent", func(t *testing.T) {
		unified.Capabilities = capability.Set{Features: []capability.Feature{capability.Streaming, capability.ToolCalling}, InputModalities: []capability.Modality{capability.Text, capability.Image}, OutputModalities: []capability.Modality{capability.Text}, Limits: capability.Limits{ContextLength: common.GetPointer(int64(32000)), MaxOutputTokens: common.GetPointer(int64(4096)), MaxImages: common.GetPointer(int64(8)), MaxDurationSeconds: common.GetPointer(int64(60)), EmbeddingDimensions: common.GetPointer(int64(3072))}}
		require.NoError(t, unified.UpdateDeclaration())
		for _, invalid := range []string{
			`{"features":["reasoning"]}`, `{"input_modalities":["audio"]}`, `{"output_modalities":["image"]}`,
			`{"limits":{"context_length":32001}}`, `{"limits":{"max_output_tokens":4097}}`, `{"limits":{"max_images":9}}`, `{"limits":{"max_duration_seconds":61}}`, `{"limits":{"embedding_dimensions":3073}}`,
			`{"features":["unknown"]}`, `{"limits":{"max_images":0}}`,
		} {
			status, result := call(http.MethodPatch, path, `{"capabilities":`+invalid+`}`)
			assert.Equal(t, http.StatusBadRequest, status, result)
		}
		narrowed := `{"features":["streaming"],"input_modalities":["text"],"output_modalities":["text"],"limits":{"context_length":16000,"max_output_tokens":2048,"max_images":4,"max_duration_seconds":30,"embedding_dimensions":1024}}`
		status, result := call(http.MethodPatch, path, `{"capabilities":`+narrowed+`}`)
		require.Equal(t, http.StatusOK, status, result)
		model.SetChannelPurchasePriceChecker(configuredChannelPrice{})
		status, result = call(http.MethodPost, path+"/enable", "")
		require.Equal(t, http.StatusOK, status, result)
		assert.Equal(t, "enabled", result["data"].(map[string]any)["status"])
		// A Model change may not leave an enabled Channel with unsupported features.
		modelBody := fmt.Sprintf(`{"name":"gpt-4o","publisher_id":%d,"capabilities":{"features":[],"input_modalities":["text"],"output_modalities":["text"],"limits":{"context_length":8000}}}`, publisher.Id)
		status, result = call(http.MethodPut, fmt.Sprintf("/api/admin/v1/models/%d", unified.Id), modelBody)
		assert.Equal(t, http.StatusBadRequest, status, result)
		status, result = call(http.MethodPatch, path, `{"model_id":999999}`)
		assert.Equal(t, http.StatusNotFound, status, result)
		status, result = call(http.MethodPatch, path, `{"upstream_model_name":"openai/gpt-4o-new"}`)
		require.Equal(t, http.StatusOK, status, result)
		assert.Equal(t, "pending", result["data"].(map[string]any)["status"])
		assert.Equal(t, false, result["data"].(map[string]any)["mapping_confirmed"])
		status, _ = call(http.MethodPost, path+"/enable", "")
		assert.Equal(t, http.StatusBadRequest, status)
		status, _ = call(http.MethodPost, path+"/confirm", "")
		require.Equal(t, http.StatusOK, status)
		require.NoError(t, db.Model(&provider).Update("status", common.ChannelStatusManuallyDisabled).Error)
		status, _ = call(http.MethodPost, path+"/enable", "")
		assert.Equal(t, http.StatusBadRequest, status)
		require.NoError(t, db.Model(&provider).Update("status", common.ChannelStatusEnabled).Error)
		status, _ = call(http.MethodPost, path+"/enable", "")
		require.Equal(t, http.StatusOK, status)
		status, result = call(http.MethodPost, path+"/disable", "")
		require.Equal(t, http.StatusOK, status, result)
		assert.Equal(t, "disabled", result["data"].(map[string]any)["status"])
	})
	t.Run("referenced models cannot become metadata matching rules", func(t *testing.T) {
		legacyBody := fmt.Sprintf(`{"id":%d,"model_name":"gpt-4o","vendor_id":%d,"name_rule":1,"status":1,"sync_official":1}`, unified.Id, publisher.Id)
		_, result := call(http.MethodPut, "/api/models/", legacyBody)
		assert.Equal(t, false, result["success"], result)
		status, _ := call(http.MethodGet, fmt.Sprintf("/api/admin/v1/models/%d", unified.Id), "")
		assert.Equal(t, http.StatusOK, status)
	})
	t.Run("referenced models cannot be soft deleted", func(t *testing.T) {
		status, result := call(http.MethodDelete, fmt.Sprintf("/api/admin/v1/models/%d", unified.Id), "")
		assert.Equal(t, http.StatusConflict, status, result)
		status, _ = call(http.MethodGet, fmt.Sprintf("/api/admin/v1/models/%d", unified.Id), "")
		assert.Equal(t, http.StatusOK, status)
	})
	t.Run("suggestions ignore only casing and slash namespaces", func(t *testing.T) {
		status, result := call(http.MethodGet, "/api/admin/v1/channels/suggest-models?upstream_model_name=tenant/OPENAI/GPT-4O", "")
		require.Equal(t, http.StatusOK, status, result)
		assert.EqualValues(t, unified.Id, result["data"].(map[string]any)["selected_model_id"])
		_, result = call(http.MethodGet, "/api/admin/v1/channels/suggest-models?upstream_model_name=gpt-4o-q4", "")
		assert.Empty(t, result["data"].(map[string]any)["items"])
		variant := model.Model{ModelName: "vendor/GPT-4O", VendorID: publisher.Id, Status: 1}
		require.NoError(t, variant.Insert())
		_, result = call(http.MethodGet, "/api/admin/v1/channels/suggest-models?upstream_model_name=gpt-4o", "")
		assert.Nil(t, result["data"].(map[string]any)["selected_model_id"])
		assert.Len(t, result["data"].(map[string]any)["items"], 2)
	})
	t.Run("reject invalid limits and protocols", func(t *testing.T) {
		for _, invalid := range []string{`{"rpm":-1}`, `{"tpm":-1}`, `{"max_concurrency":-1}`, `{"weight":-1}`, `{"rpm":9223372036854775808}`, `{"weight":1.5}`, `{"parameter_overrides":[]}`, `{"parameter_overrides":null}`, `{"protocol":"openai"}`, `{"protocol":"task:missing"}`, `{"capabilities":{"features":["unknown"]}}`} {
			status, result := call(http.MethodPatch, path, invalid)
			assert.Equal(t, http.StatusBadRequest, status, result)
		}
	})
	t.Run("concurrent patches preserve fields omitted by the later body", func(t *testing.T) {
		reader, writer := io.Pipe()
		defer reader.Close()
		defer writer.Close()
		request := httptest.NewRequest(http.MethodPatch, path, reader)
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		done := make(chan struct{})
		go func() { defer close(done); engine.ServeHTTP(recorder, request) }()
		prefixRead := make(chan error, 1)
		go func() { _, err := writer.Write([]byte(`{`)); prefixRead <- err }()
		select {
		case err := <-prefixRead:
			require.NoError(t, err)
		case <-done:
			t.Fatalf("PATCH returned before reading its body: %d %s", recorder.Code, recorder.Body.String())
		}
		status, result := call(http.MethodPatch, path, `{"tpm":77}`)
		require.Equal(t, http.StatusOK, status, result)
		_, err := writer.Write([]byte(`"weight":2}`))
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		<-done
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		status, result = call(http.MethodGet, path, "")
		require.Equal(t, http.StatusOK, status, result)
		assert.EqualValues(t, 77, result["data"].(map[string]any)["tpm"])
		assert.EqualValues(t, 2, result["data"].(map[string]any)["weight"])
	})
	t.Run("task binding needs permission and unavailable plugins can be disabled", func(t *testing.T) {
		previousRegistry := jsplugin.DefaultRegistry
		jsplugin.DefaultRegistry = jsplugin.NewRegistry()
		defer func() { jsplugin.DefaultRegistry = previousRegistry }()
		source, err := plugins.Source("kling")
		require.NoError(t, err)
		_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "kling"})
		require.NoError(t, err)
		taskBody := fmt.Sprintf(`{"provider_id":%d,"model_id":%d,"protocol":"task:kling","upstream_model_name":"task-model"}`, provider.Id, unified.Id)
		status, result := call(http.MethodPost, "/api/admin/v1/channels", taskBody)
		assert.Equal(t, http.StatusForbidden, status, result)
		original := token
		token = createWalletTestAccessToken(t, root.Id, "channel:read", "channel:write", "channel:operate", "task_plugin:bind")
		defer func() { token = original }()
		status, result = call(http.MethodPost, "/api/admin/v1/channels", taskBody)
		require.Equal(t, http.StatusOK, status, result)
		taskPath := fmt.Sprintf("/api/admin/v1/channels/%v", result["data"].(map[string]any)["id"])
		status, _ = call(http.MethodPost, taskPath+"/confirm", "")
		require.Equal(t, http.StatusOK, status)
		status, _ = call(http.MethodPost, taskPath+"/enable", "")
		require.Equal(t, http.StatusOK, status)
		jsplugin.DefaultRegistry.SetEnabled(false)
		status, _ = call(http.MethodPost, taskPath+"/enable", "")
		assert.Equal(t, http.StatusBadRequest, status)
		status, result = call(http.MethodPost, taskPath+"/disable", "")
		require.Equal(t, http.StatusOK, status, result)
		assert.Equal(t, "disabled", result["data"].(map[string]any)["status"])
		status, _ = call(http.MethodDelete, taskPath, "")
		require.Equal(t, http.StatusOK, status)
	})
	t.Run("PAT scopes and Casbin guard every action", func(t *testing.T) {
		original := token
		token = createWalletTestAccessToken(t, root.Id, "channel:read")
		for _, action := range []struct{ method, path, body string }{{http.MethodPost, "/api/admin/v1/channels", body}, {http.MethodPatch, path, `{"weight":2}`}, {http.MethodDelete, path, ""}, {http.MethodPost, path + "/confirm", ""}, {http.MethodPost, path + "/enable", ""}, {http.MethodPost, path + "/disable", ""}} {
			status, _ := call(action.method, action.path, action.body)
			assert.Equal(t, http.StatusForbidden, status, action)
		}
		customer := model.User{Username: "channel-customer", Password: "placeholder", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: "channel-customer"}
		require.NoError(t, db.Create(&customer).Error)
		token = createWalletTestAccessToken(t, customer.Id, "channel:read", "channel:write")
		status, _ := call(http.MethodGet, path, "")
		assert.Equal(t, http.StatusForbidden, status)
		token = "invalid"
		status, _ = call(http.MethodGet, path, "")
		assert.Equal(t, http.StatusUnauthorized, status)
		expired := createWalletTestAccessToken(t, root.Id, "channel:read")
		require.NoError(t, db.Model(&model.UserAccessToken{}).Where("token_hash = ?", model.AccessTokenFingerprint(expired)).Update("expires_at", time.Now().Unix()-1).Error)
		token = expired
		status, _ = call(http.MethodGet, path, "")
		assert.Equal(t, http.StatusUnauthorized, status)
		bundle, err := service.CreateLoginSession(root.Id, "password", "127.0.0.1", "channel-api-test")
		require.NoError(t, err)
		token = bundle.AccessToken
		status, _ = call(http.MethodGet, path, "")
		assert.Equal(t, http.StatusOK, status, "admin browser-session access token")
		_, err = model.RevokeUserSession(root.Id, bundle.Session.SID, "test-revocation")
		require.NoError(t, err)
		status, _ = call(http.MethodGet, path, "")
		assert.Equal(t, http.StatusUnauthorized, status, "revoked session cannot access Channel API")
		token = original
	})
	status, _ = call(http.MethodDelete, path, "")
	assert.Equal(t, http.StatusOK, status)
	status, _ = call(http.MethodGet, path, "")
	assert.Equal(t, http.StatusNotFound, status)
	audits, _, err := model.GetAuditLogs(model.AuditLogFilter{UserId: root.Id, Category: model.AuditCategoryOperation}, 0, 100, common.RoleRootUser)
	require.NoError(t, err)
	actions := map[string]bool{}
	for _, audit := range audits {
		actions[audit.Action] = true
		encoded, err := common.Marshal(audit)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), "channel-override-must-not-be-audited")
		assert.NotContains(t, string(encoded), token)
		assert.NotContains(t, string(encoded), provider.Key)
	}
	for _, action := range []string{"channel.create", "channel.update", "channel.confirm", "channel.enable", "channel.disable", "channel.delete"} {
		assert.True(t, actions[action], action)
	}
}
