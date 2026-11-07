package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	providerKeyA = "sk-provider-secret-a"
	providerKeyB = "sk-provider-secret-b"
	providerKeyC = "sk-provider-secret-c"
)

type providerAPIReply struct {
	Success bool           `json:"success"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data"`
}

func newProviderAdminEngine(t *testing.T) *gin.Engine {
	t.Helper()
	previousMaster, previousCache, previousRedis := common.IsMasterNode, common.MemoryCacheEnabled, common.RedisEnabled
	t.Cleanup(func() {
		common.IsMasterNode, common.MemoryCacheEnabled, common.RedisEnabled = previousMaster, previousCache, previousRedis
	})
	common.IsMasterNode, common.MemoryCacheEnabled, common.RedisEnabled = false, false, false
	database := model.UseTestDatabases(t)
	model.LOG_DB = database
	common.SetLogDatabaseType(common.DatabaseTypePostgreSQL)
	require.NoError(t, database.AutoMigrate(&model.Provider{}, &model.Ability{}, &model.User{}, &model.Log{}, &model.AuditLog{}))
	root := &model.User{Username: "provider-admin-root", Role: common.RoleRootUser, Status: common.UserStatusEnabled}
	require.NoError(t, database.Create(root).Error)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	group := engine.Group("/api/admin/v1/providers", func(c *gin.Context) {
		c.Set("id", root.Id)
		c.Set("role", common.RoleRootUser)
	})
	group.GET("", ListProviders)
	group.POST("", CreateProvider)
	group.GET("/:id", GetProvider)
	group.PATCH("/:id", UpdateProvider)
	group.DELETE("/:id", DeleteProvider)
	group.POST("/:id/enable", EnableProvider)
	group.POST("/:id/disable", DisableProvider)
	group.POST("/:id/balance", RefreshProviderBalance)
	group.GET("/:id/keys", ListProviderKeys)
	group.POST("/:id/keys/enable", EnableAllProviderKeys)
	group.POST("/:id/keys/disable", DisableAllProviderKeys)
	group.DELETE("/:id/keys/disabled", DeleteDisabledProviderKeys)
	group.POST("/:id/keys/:index/enable", EnableProviderKey)
	group.POST("/:id/keys/:index/disable", DisableProviderKey)
	group.DELETE("/:id/keys/:index", DeleteProviderKey)
	return engine
}

// callProviderAPI sends a request and checks that no reply carries a
// credential, whatever the route.
func callProviderAPI(t *testing.T, engine *gin.Engine, method, path, body string) providerAPIReply {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, "/api/admin/v1/providers"+path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	for _, secret := range []string{"sk-provider", `"key":`, "key_preview"} {
		assert.NotContains(t, recorder.Body.String(), secret, "%s %s", method, path)
	}
	var reply providerAPIReply
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &reply))
	return reply
}

func storedProvider(t *testing.T, id int) *model.Provider {
	t.Helper()
	provider, err := model.GetProviderById(id, true)
	require.NoError(t, err)
	return provider
}

func TestProviderAdminAPIManagesProviderWithWriteOnlyKeys(t *testing.T) {
	engine := newProviderAdminEngine(t)

	created := callProviderAPI(t, engine, http.MethodPost, "", `{"mode":"multi_to_single","multi_key_mode":"polling","provider":{
		"name":"account one","type":1,"key":"`+providerKeyA+`\n`+providerKeyB+`\n`+providerKeyC+`",
		"models":"gpt-test","group":"default","base_url":"https://upstream.example"}}`)
	require.True(t, created.Success, created.Message)
	ids := created.Data["ids"].([]any)
	require.Len(t, ids, 1)
	id := int(ids[0].(float64))
	path := "/" + common.Interface2String(id)

	listed := callProviderAPI(t, engine, http.MethodGet, "?keyword=account&p=1&page_size=10", "")
	require.True(t, listed.Success, listed.Message)
	assert.EqualValues(t, 1, listed.Data["total"])
	item := listed.Data["items"].([]any)[0].(map[string]any)
	assert.Equal(t, "account one", item["name"])
	assert.Equal(t, true, item["channel_info"].(map[string]any)["is_multi_key"])

	updated := callProviderAPI(t, engine, http.MethodPatch, path, `{"name":"account renamed","key":""}`)
	require.True(t, updated.Success, updated.Message)
	assert.Equal(t, "account renamed", updated.Data["name"])
	assert.Equal(t, providerKeyA+"\n"+providerKeyB+"\n"+providerKeyC, storedProvider(t, id).Key, "an empty key keeps the stored credential")

	disabled := callProviderAPI(t, engine, http.MethodPost, path+"/disable", "")
	require.True(t, disabled.Success, disabled.Message)
	assert.EqualValues(t, common.ChannelStatusManuallyDisabled, disabled.Data["status"])
	enabled := callProviderAPI(t, engine, http.MethodPost, path+"/enable", "")
	require.True(t, enabled.Success, enabled.Message)
	assert.EqualValues(t, common.ChannelStatusEnabled, enabled.Data["status"])

	keyDisabled := callProviderAPI(t, engine, http.MethodPost, path+"/keys/1/disable", "")
	require.True(t, keyDisabled.Success, keyDisabled.Message)
	keys := callProviderAPI(t, engine, http.MethodGet, path+"/keys?p=1&page_size=10", "")
	require.True(t, keys.Success, keys.Message)
	assert.EqualValues(t, 3, keys.Data["total"])
	assert.EqualValues(t, 2, keys.Data["enabled_count"])
	assert.EqualValues(t, 1, keys.Data["manual_disabled_count"])
	second := keys.Data["keys"].([]any)[1].(map[string]any)
	assert.EqualValues(t, 1, second["index"])
	assert.EqualValues(t, common.ChannelStatusManuallyDisabled, second["status"])

	removed := callProviderAPI(t, engine, http.MethodDelete, path+"/keys/1", "")
	require.True(t, removed.Success, removed.Message)
	assert.Equal(t, providerKeyA+"\n"+providerKeyC, storedProvider(t, id).Key)

	appended := callProviderAPI(t, engine, http.MethodPatch, path, `{"key":"`+providerKeyB+`","key_mode":"append"}`)
	require.True(t, appended.Success, appended.Message)
	assert.Equal(t, providerKeyA+"\n"+providerKeyC+"\n"+providerKeyB, storedProvider(t, id).Key)

	deleted := callProviderAPI(t, engine, http.MethodDelete, path, "")
	require.True(t, deleted.Success, deleted.Message)
	missing := callProviderAPI(t, engine, http.MethodGet, path, "")
	assert.False(t, missing.Success)
}

func TestProviderAdminAPIRefreshesBalance(t *testing.T) {
	engine := newProviderAdminEngine(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/usage/token/":
			assert.Equal(t, "Bearer "+providerKeyA, r.Header.Get("Authorization"))
			_, _ = w.Write([]byte(`{"code":true,"data":{"total_available":1500000,"unlimited_quota":false}}`))
		case "/api/status":
			_, _ = w.Write([]byte(`{"success":true,"data":{"quota_per_unit":1000000}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	baseURL := server.URL + "/"
	provider := &model.Provider{Type: constant.ChannelTypeNewAPI, BaseURL: &baseURL, Key: providerKeyA, Name: "balance account"}
	require.NoError(t, model.DB.Create(provider).Error)

	refreshed := callProviderAPI(t, engine, http.MethodPost, "/"+common.Interface2String(provider.Id)+"/balance", "")
	require.True(t, refreshed.Success, refreshed.Message)
	assert.InDelta(t, 1.5, refreshed.Data["balance"], 1e-9)
	assert.InDelta(t, 1.5, storedProvider(t, provider.Id).Balance, 1e-9)
}

func TestProviderAdminAPIForwardsLegacyValidationErrors(t *testing.T) {
	engine := newProviderAdminEngine(t)
	provider := &model.Provider{Type: constant.ChannelTypeOpenAI, Key: providerKeyA, Name: "single key account", Models: "gpt-test", Group: "default"}
	require.NoError(t, provider.Insert())
	path := "/" + common.Interface2String(provider.Id)

	for _, tc := range []struct {
		name, method, path, body string
	}{
		{name: "create without provider", method: http.MethodPost, path: "", body: `{"mode":"single"}`},
		{name: "unsupported add mode", method: http.MethodPost, path: "", body: `{"mode":"copy","provider":{"name":"x","type":1,"key":"` + providerKeyB + `","models":"gpt-test","group":"default"}}`},
		{name: "status in update body", method: http.MethodPatch, path: path, body: `{"status":2}`},
		{name: "key actions on a single-key provider", method: http.MethodGet, path: path + "/keys", body: ""},
		{name: "invalid id", method: http.MethodGet, path: "/abc", body: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply := callProviderAPI(t, engine, tc.method, tc.path, tc.body)
			assert.False(t, reply.Success)
			assert.NotEmpty(t, reply.Message)
		})
	}
	assert.Equal(t, common.ChannelStatusEnabled, storedProvider(t, provider.Id).Status)
}
