package controller

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
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
	return callProviderAPIStatus(t, engine, http.StatusOK, method, path, body)
}

func callProviderAPIStatus(t *testing.T, engine *gin.Engine, wantStatus int, method, path, body string, secrets ...string) providerAPIReply {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, "/api/admin/v1/providers"+path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(recorder, request)
	require.Equal(t, wantStatus, recorder.Code, recorder.Body.String())
	for _, secret := range append(secrets, providerKeyA, "sk-provider", `"key":`, "key_preview") {
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

func TestProviderAdminAPIKeepsURLCredentialsWriteOnly(t *testing.T) {
	engine := newProviderAdminEngine(t)
	userSecret, passwordSecret := "url-user-fixture", "srlpzvnhm-ysxwzq"
	querySecret, fragmentSecret := "url-query-fixture", "url-fragment-fixture"
	baseURL := "https://" + userSecret + ":" + passwordSecret + "@upstream.example/v1?api_key=" + querySecret + "&region=west#" + fragmentSecret
	body, err := common.Marshal(map[string]any{"provider": map[string]any{
		"type": constant.ChannelTypeOpenAI, "name": "credential URL", "key": providerKeyA, "base_url": baseURL,
	}})
	require.NoError(t, err)
	created := callProviderAPI(t, engine, http.MethodPost, "", string(body))
	require.True(t, created.Success, created.Message)
	id := int(created.Data["ids"].([]any)[0].(float64))
	path := "/" + common.Interface2String(id)
	secrets := []string{userSecret, passwordSecret, querySecret, fragmentSecret}

	for _, request := range []struct{ method, endpoint, body string }{
		{http.MethodGet, path, ""},
		{http.MethodGet, "?keyword=credential", ""},
		{http.MethodPatch, path, `{"name":"credential URL renamed"}`},
		{http.MethodPost, path + "/disable", ""},
		{http.MethodPost, path + "/enable", ""},
	} {
		reply := callProviderAPIStatus(t, engine, http.StatusOK, request.method, request.endpoint, request.body, secrets...)
		require.True(t, reply.Success, reply.Message)
	}
	assert.Equal(t, baseURL, *storedProvider(t, id).BaseURL)
}

func TestProviderAdminAPIPreservesURLReadbackOnUpdate(t *testing.T) {
	for _, tc := range []struct {
		name, baseURL string
		hidden        bool
	}{
		{"userinfo", "https://fixture-account:fixture-value@upstream.example/v1", true},
		{"query", "https://upstream.example/v1?custom_auth=fixture-value", true},
		{"fragment", "https://upstream.example/v1#fixture-value", true},
		{"legacy malformed", "https://fixture-account:fixture-value@invalid[", true},
		{"public HTTP", "https://upstream.example/v1", false},
		{"public WebSocket", "wss://upstream.example/v1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := newProviderAdminEngine(t)
			baseURL := tc.baseURL
			provider := &model.Provider{Type: constant.ChannelTypeOpenAI, Name: "saved endpoint", Key: providerKeyA, BaseURL: &baseURL}
			require.NoError(t, provider.Insert())
			path := "/" + common.Interface2String(provider.Id)
			view := callProviderAPI(t, engine, http.MethodGet, path, "")
			if tc.hidden {
				assert.NotEqual(t, baseURL, view.Data["base_url"])
			} else {
				assert.Equal(t, baseURL, view.Data["base_url"])
			}
			body, err := common.Marshal(map[string]any{"name": "renamed endpoint", "base_url": view.Data["base_url"]})
			require.NoError(t, err)
			updated := callProviderAPI(t, engine, http.MethodPatch, path, string(body))
			require.True(t, updated.Success, updated.Message)
			assert.Equal(t, "renamed endpoint", updated.Data["name"])
			assert.Equal(t, baseURL, *storedProvider(t, provider.Id).BaseURL)
		})
	}
}

func TestProviderAdminAPIRequiresCompleteHiddenURLReplacement(t *testing.T) {
	engine := newProviderAdminEngine(t)
	baseURL := "https://fixture-account:hkgapogh-msirj@upstream.example/v1?api_key=fixture-value#fixture-fragment"
	provider := &model.Provider{Type: constant.ChannelTypeOpenAI, Name: "saved endpoint", Key: providerKeyA, BaseURL: &baseURL}
	require.NoError(t, provider.Insert())
	path := "/" + common.Interface2String(provider.Id)
	view := callProviderAPI(t, engine, http.MethodGet, path, "")
	partial := strings.Replace(view.Data["base_url"].(string), "upstream.example", "replacement.example", 1)
	body, err := common.Marshal(map[string]any{"base_url": partial})
	require.NoError(t, err)
	rejected := callProviderAPIStatus(t, engine, http.StatusBadRequest, http.MethodPatch, path, string(body))
	assert.False(t, rejected.Success)
	assert.Equal(t, baseURL, *storedProvider(t, provider.Id).BaseURL)

	replaced := callProviderAPI(t, engine, http.MethodPatch, path, `{"base_url":"https://replacement.example/v2"}`)
	require.True(t, replaced.Success, replaced.Message)
	assert.Equal(t, "https://replacement.example/v2", replaced.Data["base_url"])
	assert.Equal(t, "https://replacement.example/v2", *storedProvider(t, provider.Id).BaseURL)
	cleared := callProviderAPI(t, engine, http.MethodPatch, path, `{"base_url":""}`)
	require.True(t, cleared.Success, cleared.Message)
	assert.Empty(t, *storedProvider(t, provider.Id).BaseURL)
}

func TestProviderAdminAPISearchDoesNotProbeHiddenURLValues(t *testing.T) {
	for _, host := range []string{"upstream.example:8443", "[::1]:8443"} {
		t.Run(host, func(t *testing.T) {
			engine := newProviderAdminEngine(t)
			userSecret, passwordSecret := "url-search-user", "url-search-credential"
			querySecret, fragmentSecret := "query-value-opaque", "fragment-value-opaque"
			address := url.URL{Scheme: "https", Host: host, Path: "/v1", User: url.UserPassword(userSecret, passwordSecret), RawQuery: "api_key=" + querySecret, Fragment: fragmentSecret}
			baseURL := address.String()
			provider := &model.Provider{Type: constant.ChannelTypeOpenAI, Name: "private URL", Key: providerKeyA, BaseURL: &baseURL}
			require.NoError(t, provider.Insert())
			for _, secret := range []string{userSecret, passwordSecret, querySecret, fragmentSecret} {
				result := callProviderAPI(t, engine, http.MethodGet, "?keyword="+url.QueryEscape(secret), "")
				assert.EqualValues(t, 0, result.Data["total"])
				assert.Empty(t, result.Data["items"])
			}
			lookup := address
			lookup.User, lookup.RawQuery, lookup.Fragment = url.UserPassword("someone", "lookup-credential"), "api_key=another", "another"
			for _, keyword := range []string{address.Hostname(), host, lookup.String()} {
				result := callProviderAPI(t, engine, http.MethodGet, "?keyword="+url.QueryEscape(keyword), "")
				assert.EqualValues(t, 1, result.Data["total"])
			}
		})
	}
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
	missing := callProviderAPIStatus(t, engine, http.StatusNotFound, http.MethodGet, path, "")
	assert.False(t, missing.Success)
}

func TestProviderAdminAPIKeepsAccountSecretsWriteOnly(t *testing.T) {
	engine := newProviderAdminEngine(t)
	headers := `{"Authorization":"Bearer fixture-header-secret","x-api-key":"fixture-api-secret","X-Custom":"fixture-custom-secret"}`
	setting := `{"proxy":"http://user:fixture-proxy-secret@proxy.example:8080"}`
	provider := &model.Provider{Type: constant.ChannelTypeOpenAI, Name: "secret fixture", Key: providerKeyA,
		HeaderOverride: &headers, Setting: &setting, OtherInfo: `{"error":"fixture-info-secret"}`,
		OtherSettings: `{"custom":"fixture-settings-secret"}`, ParamOverride: common.GetPointer(`{"token":"fixture-param-secret"}`),
		Models: "gpt-test", Group: "default",
		ProviderInfo: model.ProviderInfo{IsMultiKey: true, MultiKeySize: 1, MultiKeyStatusList: map[int]int{0: common.ChannelStatusAutoDisabled}, MultiKeyDisabledReason: map[int]string{0: "fixture-reason-secret"}}}
	require.NoError(t, provider.Insert())
	path := "/" + common.Interface2String(provider.Id)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "", ""},
		{http.MethodGet, path, ""},
		{http.MethodGet, path + "/keys", ""},
		{http.MethodPatch, path, `{"name":"renamed"}`},
		{http.MethodPost, path + "/disable", ""},
		{http.MethodPost, path + "/enable", ""},
		{http.MethodPatch, path, `{"header_override":"{\"Authorization\":\"Bearer fixture-new-secret\"}"}`},
	} {
		t.Run(tc.method+tc.path+tc.body, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(tc.method, "/api/admin/v1/providers"+tc.path, strings.NewReader(tc.body))
			request.Header.Set("Content-Type", "application/json")
			engine.ServeHTTP(recorder, request)
			require.Equal(t, http.StatusOK, recorder.Code)
			for _, secret := range []string{providerKeyA, "fixture-header-secret", "fixture-api-secret", "fixture-custom-secret", "fixture-proxy-secret", "fixture-info-secret", "fixture-settings-secret", "fixture-param-secret", "fixture-new-secret", "fixture-reason-secret"} {
				assert.NotContains(t, recorder.Body.String(), secret)
			}
		})
		if tc.method == http.MethodPatch && tc.body == `{"name":"renamed"}` {
			assert.Equal(t, headers, *storedProvider(t, provider.Id).HeaderOverride, "omitting header overrides preserves existing credentials")
		}
	}
	assert.Equal(t, `{"Authorization":"Bearer fixture-new-secret"}`, *storedProvider(t, provider.Id).HeaderOverride)
}

func TestProviderAdminAPICreatesConfiguredAdapters(t *testing.T) {
	engine := newProviderAdminEngine(t)
	for _, tc := range []struct {
		name            string
		typeID          int
		other, settings string
	}{
		{"azure", constant.ChannelTypeAzure, "2025-04-01-preview", `{"azure_responses_version":"preview"}`},
		{"spark", constant.ChannelTypeXunfei, "v2.1", ""},
		{"cloudflare", constant.ChannelCloudflare, "account-id", ""},
		{"vertex", constant.ChannelTypeVertexAi, `{"default":"us-central1"}`, `{"vertex_key_type":"api_key","vertex_project_id":"test-project"}`},
		{"aws", constant.ChannelTypeAws, "", `{"aws_key_type":"api_key","aws_api_type":"mantle"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := common.Marshal(map[string]any{"provider": map[string]any{
				"name": tc.name, "type": tc.typeID, "key": providerKeyA, "base_url": "https://upstream.example", "other": tc.other, "settings": tc.settings,
			}})
			require.NoError(t, err)
			created := callProviderAPI(t, engine, http.MethodPost, "", string(body))
			require.True(t, created.Success, created.Message)
			id := int(created.Data["ids"].([]any)[0].(float64))
			assert.Equal(t, tc.other, storedProvider(t, id).Other)
			assert.Equal(t, tc.settings, storedProvider(t, id).OtherSettings)
			updated := callProviderAPI(t, engine, http.MethodPatch, "/"+common.Interface2String(id), `{"name":"renamed"}`)
			require.True(t, updated.Success, updated.Message)
			assert.Equal(t, tc.other, storedProvider(t, id).Other)
			assert.Equal(t, tc.settings, storedProvider(t, id).OtherSettings)
		})
	}
}

func TestProviderAdminAPIValidatesPartialAdapterUpdates(t *testing.T) {
	engine := newProviderAdminEngine(t)
	provider := &model.Provider{Type: constant.ChannelTypeVertexAi, Name: "vertex", Key: providerKeyA, Other: `{"default":"us-central1"}`, OtherSettings: `{"vertex_key_type":"api_key"}`}
	require.NoError(t, provider.Insert())
	path := "/" + common.Interface2String(provider.Id)
	for _, body := range []string{`{"settings":"{\"vertex_api_version\":\"invalid-version\"}"}`, `{"other":"not-json"}`} {
		reply := callProviderAPIStatus(t, engine, http.StatusBadRequest, http.MethodPatch, path, body)
		assert.False(t, reply.Success)
	}
	assert.Equal(t, provider.Other, storedProvider(t, provider.Id).Other)
	assert.Equal(t, provider.OtherSettings, storedProvider(t, provider.Id).OtherSettings)
}

func TestProviderAdminAPIHidesRawBalanceResponses(t *testing.T) {
	engine := newProviderAdminEngine(t)
	headerSecret, querySecret := "fixture-header-secret", "fixture-query-secret"
	upstreamBody, err := common.Marshal(gin.H{"token": providerKeyA, "custom_header": headerSecret, "query": querySecret})
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(upstreamBody)
	}))
	defer server.Close()
	baseURL := server.URL
	provider := &model.Provider{Type: constant.ChannelTypeAdvancedCustom, BaseURL: &baseURL, Key: providerKeyA, Name: "custom balance",
		OtherSettings: `{"advanced_custom":{"advanced_routes":[{"incoming_path":"/v1/dashboard/billing/credit_grants","upstream_path":"/balance","converter":"none"}]}}`}
	require.NoError(t, model.DB.Create(provider).Error)
	reply := callProviderAPIStatus(t, engine, http.StatusBadGateway, http.MethodPost, "/"+common.Interface2String(provider.Id)+"/balance", "", headerSecret, querySecret, "raw_response")
	assert.False(t, reply.Success)
	assert.Equal(t, "Failed to query balance", reply.Message)
}

func TestProviderAdminAPIHidesBalanceErrors(t *testing.T) {
	engine := newProviderAdminEngine(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":false,"message":"sk-provider-secret-a"}`))
	}))
	defer server.Close()
	baseURL := server.URL + "/"
	provider := &model.Provider{Type: constant.ChannelTypeNewAPI, BaseURL: &baseURL, Key: providerKeyA, Name: "failed balance"}
	require.NoError(t, model.DB.Create(provider).Error)
	reply := callProviderAPIStatus(t, engine, http.StatusBadGateway, http.MethodPost, "/"+common.Interface2String(provider.Id)+"/balance", "")
	assert.False(t, reply.Success)
	assert.Equal(t, "Failed to query balance", reply.Message)
}

func TestProviderAdminAPIHidesBalanceTransportErrors(t *testing.T) {
	engine := newProviderAdminEngine(t)
	headerSecret, querySecret := "fixture-header-secret", "fixture-query-secret"
	var upstreamCalled atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled.Store(true)
		assert.Equal(t, querySecret, r.URL.Query().Get("api_key"))
		assert.Equal(t, "Bearer "+providerKeyA, r.Header.Get("Authorization"))
		connection, _, err := w.(http.Hijacker).Hijack()
		if assert.NoError(t, err) {
			assert.NoError(t, connection.Close())
		}
	}))
	defer server.Close()
	// The legacy helper appends its path to this URL; keep a credential in the
	// query to exercise url.Error without contacting a real Provider.
	baseURL := server.URL + "/?api_key=" + querySecret + "&path="
	headers, err := common.Marshal(gin.H{"X-Api-Key": headerSecret})
	require.NoError(t, err)
	headerOverride := string(headers)
	provider := &model.Provider{Type: constant.ChannelTypeOpenAI, BaseURL: &baseURL, Key: providerKeyA,
		HeaderOverride: &headerOverride, Name: "disconnected balance"}
	require.NoError(t, model.DB.Create(provider).Error)
	reply := callProviderAPIStatus(t, engine, http.StatusBadGateway, http.MethodPost, "/"+common.Interface2String(provider.Id)+"/balance", "", headerSecret, querySecret, "raw_response")
	assert.False(t, reply.Success)
	assert.Equal(t, "Failed to query balance", reply.Message)
	assert.Empty(t, reply.Data)
	assert.True(t, upstreamCalled.Load(), "the balance request must reach the disconnecting upstream")
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

func TestProviderAdminAPIRejectsInvalidRequestsWithBadRequest(t *testing.T) {
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
		{name: "credential prefix in name", method: http.MethodPost, path: "", body: `{"mode":"batch","batch_add_set_key_prefix_2_name":true,"provider":{"name":"x","type":1,"key":"` + providerKeyB + `\\n` + providerKeyC + `"}}`},
		{name: "invalid id", method: http.MethodGet, path: "/abc", body: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply := callProviderAPIStatus(t, engine, http.StatusBadRequest, tc.method, tc.path, tc.body)
			assert.False(t, reply.Success)
			assert.NotEmpty(t, reply.Message)
		})
	}
	assert.Equal(t, common.ChannelStatusEnabled, storedProvider(t, provider.Id).Status)
}
