package controller

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
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

type providerAdminTestEngine struct {
	*gin.Engine
	accessToken string
}

func newProviderAdminEngine(t *testing.T) *providerAdminTestEngine {
	t.Helper()
	previousMaster, previousCache, previousRedis := common.IsMasterNode, common.MemoryCacheEnabled, common.RedisEnabled
	previousSecret := common.SessionSecret
	common.SessionSecret = "provider-admin-session-fixture-secret"
	t.Cleanup(func() {
		common.SessionSecret = previousSecret
		common.IsMasterNode, common.MemoryCacheEnabled, common.RedisEnabled = previousMaster, previousCache, previousRedis
	})
	common.IsMasterNode, common.MemoryCacheEnabled, common.RedisEnabled = false, false, false
	database := model.UseTestDatabases(t)
	model.LOG_DB = database
	common.SetLogDatabaseType(common.DatabaseTypePostgreSQL)
	require.NoError(t, database.AutoMigrate(&model.Provider{}, &model.Ability{}, &model.User{}, &model.UserSession{}, &model.Log{}, &model.AuditLog{}))
	root := &model.User{Username: "provider-admin-root", Role: common.RoleRootUser, Status: common.UserStatusEnabled, AuthVersion: 1}
	require.NoError(t, database.Create(root).Error)
	session, err := service.CreateLoginSession(root.Id, service.VerificationMethodPassword, "127.0.0.1", "provider-admin-fixture")
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	group := engine.Group("/api/admin/v1/providers", middleware.AdminAuth())
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
	return &providerAdminTestEngine{Engine: engine, accessToken: session.AccessToken}
}

// callProviderAPI sends a request and checks that no reply carries a
// credential, whatever the route.
func callProviderAPI(t *testing.T, engine *providerAdminTestEngine, method, path, body string) providerAPIReply {
	t.Helper()
	return callProviderAPIStatus(t, engine, http.StatusOK, method, path, body)
}

func callProviderAPIStatus(t *testing.T, engine *providerAdminTestEngine, wantStatus int, method, path, body string, secrets ...string) providerAPIReply {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, "/api/admin/v1/providers"+path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+engine.accessToken)
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

func TestProviderAdminAPIRejectsRetiredRoutingFields(t *testing.T) {
	engine := newProviderAdminEngine(t)
	settings := ` {"tool_loss_policy":"legacy-unknown","aws_region":"us-east-1"} `
	provider := &model.Provider{Type: constant.ChannelTypeOpenAI, Name: "existing account", Key: providerKeyA, Models: "legacy-model", Group: "legacy-group", OtherSettings: settings}
	require.NoError(t, provider.Insert())
	path := "/" + common.Interface2String(provider.Id)
	before := storedProvider(t, provider.Id)

	cases := []struct {
		name   string
		fields map[string]any
	}{
		{"models", map[string]any{"models": "new-model"}},
		{"empty models", map[string]any{"models": ""}},
		{"null models", map[string]any{"models": nil}},
		{"case insensitive models", map[string]any{"MoDeLs": "new-model"}},
		{"unicode folded models", map[string]any{"modelſ": "new-model"}},
		{"unicode folded settings", map[string]any{"ſettings": `{"tool_loss_policy":"allow"}`}},
		{"model mapping", map[string]any{"model_mapping": `{"public":"upstream"}`}},
		{"group", map[string]any{"group": "new-group"}},
		{"priority", map[string]any{"priority": 100}},
		{"weight", map[string]any{"weight": 10}},
		{"legacy used quota", map[string]any{"used_quota": 900}},
		{"null legacy used quota", map[string]any{"used_quota": nil}},
		{"case folded legacy used quota", map[string]any{"Used_Quota": 900}},
		{"top level loss policy", map[string]any{"tool_loss_policy": "allow"}},
		{"nested loss policy", map[string]any{"settings": `{"tool_loss_policy":"allow","aws_region":"us-west-2"}`}},
		{"case insensitive loss policy", map[string]any{"Settings": `{"TOOL_LOSS_POLICY":"allow"}`}},
		{"null loss policy", map[string]any{"settings": `{"tool_loss_policy":null}`}},
		{"escaped loss policy", map[string]any{"settings": "{\"tool_loss_poli\\u0063y\":\"allow\"}"}},
	}
	for _, tc := range cases {
		for _, method := range []string{http.MethodPost, http.MethodPatch} {
			t.Run(tc.name+"/"+method, func(t *testing.T) {
				fields := map[string]any{"name": "rejected rename", "key": providerKeyB}
				for name, value := range tc.fields {
					fields[name] = value
				}
				endpoint := path
				var body any = fields
				if method == http.MethodPost {
					endpoint = ""
					fields["type"] = constant.ChannelTypeOpenAI
					body = map[string]any{"provider": fields}
				}
				encoded, err := common.Marshal(body)
				require.NoError(t, err)
				var providers, abilities, audits int64
				require.NoError(t, model.DB.Model(&model.Provider{}).Count(&providers).Error)
				require.NoError(t, model.DB.Model(&model.Ability{}).Count(&abilities).Error)
				require.NoError(t, model.LOG_DB.Model(&model.AuditLog{}).Where("success = ?", true).Count(&audits).Error)
				reply := callProviderAPIStatus(t, engine, http.StatusBadRequest, method, endpoint, string(encoded))
				assert.False(t, reply.Success)
				assert.Equal(t, before, storedProvider(t, provider.Id), "a rejected field must not rename or rotate the account")
				var after int64
				require.NoError(t, model.DB.Model(&model.Provider{}).Count(&after).Error)
				assert.Equal(t, providers, after)
				require.NoError(t, model.DB.Model(&model.Ability{}).Count(&after).Error)
				assert.Equal(t, abilities, after)
				require.NoError(t, model.LOG_DB.Model(&model.AuditLog{}).Where("success = ?", true).Count(&after).Error)
				assert.Equal(t, audits, after, "rejection remains audited without recording a successful mutation")
			})
		}
	}

	updated := callProviderAPI(t, engine, http.MethodPatch, path, `{"name":"renamed account","key":""}`)
	require.True(t, updated.Success, updated.Message)
	current := storedProvider(t, provider.Id)
	assert.Equal(t, "renamed account", current.Name)
	assert.Equal(t, before.Key, current.Key)
	assert.Equal(t, before.Models, current.Models)
	assert.Equal(t, before.Group, current.Group)
	assert.Equal(t, settings, current.OtherSettings, "ordinary updates preserve stored configuration without resubmitting retired fields")
}

func TestProviderSettingsRetainValidationWithRetiredLossPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, setting, other, wantError string
		providerType                    int
	}{
		{name: "unknown historical policy", other: `{"tool_loss_policy":"legacy-unknown","aws_region":"us-east-1"}`},
		{name: "historical policy with a non-string value", other: `{"tool_loss_policy":17,"openrouter_enterprise":true}`},
		{name: "invalid active field JSON type", other: `{"tool_loss_policy":17,"openrouter_enterprise":17}`, wantError: "cannot unmarshal"},
		{name: "invalid settings JSON", setting: `{`, wantError: "unexpected end"},
		{name: "invalid other settings JSON", other: `{`, wantError: "unexpected end"},
		{name: "proxy still checked", setting: `{"proxy":"ftp://localhost"}`, other: `{"tool_loss_policy":"legacy-unknown"}`, wantError: "invalid channel proxy"},
		{name: "transport still checked", setting: `{"http_protocol":"http1","http2_connection_shards":2}`, other: `{"tool_loss_policy":"legacy-unknown"}`, wantError: "http2_connection_shards"},
		{name: "advanced config still required", providerType: constant.ChannelTypeAdvancedCustom, other: `{"tool_loss_policy":"legacy-unknown"}`, wantError: "advanced_custom is required"},
		{name: "advanced config still checked", providerType: constant.ChannelTypeAdvancedCustom, other: `{"tool_loss_policy":"legacy-unknown","advanced_custom":{"advanced_routes":[{"incoming_path":"/v1/chat/completions","upstream_path":"/v1/chat/completions","converter":"invalid"}]}}`, wantError: "converter"},
		{name: "discovery route still required", providerType: constant.ChannelTypeAdvancedCustom, other: `{"tool_loss_policy":"legacy-unknown","upstream_model_update_check_enabled":true,"advanced_custom":{"advanced_routes":[{"incoming_path":"/v1/chat/completions","upstream_path":"/v1/chat/completions","converter":"none"}]}}`, wantError: "/v1/models"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := model.Provider{Type: tc.providerType, Setting: &tc.setting, OtherSettings: tc.other}
			err := provider.ValidateSettings()
			if tc.wantError == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantError)
			}
			assert.Equal(t, tc.setting, *provider.Setting)
			assert.Equal(t, tc.other, provider.OtherSettings)
		})
	}
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

func TestProviderAdminAPIRejectsStaleHiddenURLFallback(t *testing.T) {
	engine := newProviderAdminEngine(t)
	baseURL := "https://fixture-account:fixture@invalid["
	provider := &model.Provider{Type: constant.ChannelTypeOpenAI, Name: "saved endpoint", Key: providerKeyA, BaseURL: &baseURL}
	require.NoError(t, provider.Insert())
	path := "/" + common.Interface2String(provider.Id)
	stale := callProviderAPI(t, engine, http.MethodGet, path, "")
	endpoint := url.URL{Scheme: "https", Host: "replacement.example", User: url.UserPassword("new-account", "new-credential")}
	body, err := common.Marshal(map[string]any{"base_url": endpoint.String()})
	require.NoError(t, err)
	replaced := callProviderAPI(t, engine, http.MethodPatch, path, string(body))
	require.True(t, replaced.Success, replaced.Message)

	body, err = common.Marshal(map[string]any{"name": "stale rename", "base_url": stale.Data["base_url"]})
	require.NoError(t, err)
	rejected := callProviderAPIStatus(t, engine, http.StatusBadRequest, http.MethodPatch, path, string(body))
	assert.False(t, rejected.Success)
	current := storedProvider(t, provider.Id)
	assert.Equal(t, endpoint.String(), *current.BaseURL)
	assert.Equal(t, "saved endpoint", current.Name)
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

// Only fixtures that exercise historical routing may create these source columns.
func declareLegacyProviderRoutingColumns(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, db.Exec(`ALTER TABLE "providers" ADD COLUMN "models" text, ADD COLUMN "model_mapping" text,
		ADD COLUMN "group" varchar(64) DEFAULT 'default', ADD COLUMN "priority" bigint DEFAULT 0, ADD COLUMN "weight" bigint DEFAULT 0`).Error)
}

func seedLegacyProviderRouting(t *testing.T, db *gorm.DB, provider *model.Provider) {
	t.Helper()
	if provider.Group == "" {
		provider.Group = "default"
	}
	if provider.Priority == nil {
		provider.Priority = common.GetPointer(int64(0))
	}
	if provider.Weight == nil {
		provider.Weight = common.GetPointer(uint(0))
	}
	result := db.Exec(`UPDATE "providers" SET "models" = ?, "model_mapping" = ?, "group" = ?, "priority" = ?, "weight" = ? WHERE "id" = ?`,
		provider.Models, provider.ModelMapping, provider.Group, provider.Priority, provider.Weight, provider.Id)
	require.NoError(t, result.Error)
	require.EqualValues(t, 1, result.RowsAffected)
}

func TestProviderAdminAPIIgnoresLegacyAbilityWrites(t *testing.T) {
	for _, operation := range []string{"create", "batch_create", "direct_insert", "patch", "replace", "append", "disable", "enable", "key_disable", "key_enable", "keys_disable", "keys_enable", "key_delete", "disabled_delete", "delete", "batch_delete"} {
		t.Run(operation, func(t *testing.T) {
			engine := newProviderAdminEngine(t)
			provider := &model.Provider{Name: "legacy projection owner", Type: constant.ChannelTypeOpenAI,
				Key: providerKeyA + "\n" + providerKeyB + "\n" + providerKeyC, Status: common.ChannelStatusEnabled,
				Models: "legacy-model", Group: "legacy-group", ProviderInfo: model.ProviderInfo{
					IsMultiKey: true, MultiKeySize: 3, MultiKeyMode: constant.MultiKeyModePolling,
					MultiKeyStatusList: map[int]int{1: common.ChannelStatusManuallyDisabled}}}
			if operation == "enable" {
				provider.Status = common.ChannelStatusManuallyDisabled
			} else if operation == "disabled_delete" {
				provider.ProviderInfo.MultiKeyStatusList[1] = common.ChannelStatusAutoDisabled
			}
			require.NoError(t, model.DB.Create(provider).Error)
			declareLegacyProviderRoutingColumns(t, model.DB)
			seedLegacyProviderRouting(t, model.DB, provider)
			// Historical tools still construct source projections explicitly.
			require.NoError(t, provider.AddAbilities(nil))
			var before []model.Ability
			require.NoError(t, model.DB.Order(`"channel_id", "group", "model"`).Find(&before).Error)
			require.Len(t, before, 1)
			require.NoError(t, model.DB.Exec(`CREATE FUNCTION reject_legacy_projection_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'legacy projection is read only'; END $$`).Error)
			require.NoError(t, model.DB.Exec(`CREATE TRIGGER reject_legacy_projection_write BEFORE INSERT OR UPDATE OR DELETE ON "abilities" FOR EACH STATEMENT EXECUTE FUNCTION reject_legacy_projection_write()`).Error)
			path := "/" + common.Interface2String(provider.Id)
			method, body := http.MethodPatch, `{"name":"updated account","key":""}`
			wantKey, wantStatus, wantCount := provider.Key, provider.Status, int64(1)
			var direct bool
			switch operation {
			case "create", "batch_create":
				method, path = http.MethodPost, ""
				body = `{"provider":{"name":"new account","type":1,"key":"` + providerKeyA + `"}}`
				wantCount = 2
				if operation == "batch_create" {
					body = `{"mode":"batch","provider":{"name":"new accounts","type":1,"key":"` + providerKeyA + `\n` + providerKeyB + `"}}`
					wantCount = 3
				}
			case "direct_insert":
				direct, wantCount = true, 2
				inserted := &model.Provider{Name: "direct account", Type: constant.ChannelTypeOpenAI, Key: providerKeyA, Status: common.ChannelStatusEnabled}
				require.NoError(t, inserted.Insert())
				require.NotZero(t, inserted.Id)
			case "replace":
				body, wantKey = `{"key":"`+providerKeyC+`","key_mode":"replace"}`, providerKeyC
			case "append":
				body = `{"key":"sk-provider-added","key_mode":"append"}`
				wantKey += "\nsk-provider-added"
			case "disable", "enable":
				method, path, body = http.MethodPost, path+"/"+operation, ""
				wantStatus = common.ChannelStatusManuallyDisabled
				if operation == "enable" {
					wantStatus = common.ChannelStatusEnabled
				}
			case "key_disable", "key_enable":
				method, body = http.MethodPost, ""
				path += "/keys/0/disable"
				if operation == "key_enable" {
					path = "/" + common.Interface2String(provider.Id) + "/keys/1/enable"
				}
			case "keys_disable", "keys_enable":
				method, path, body = http.MethodPost, path+"/keys/"+strings.TrimPrefix(operation, "keys_"), ""
				if operation == "keys_disable" {
					wantStatus = common.ChannelStatusManuallyDisabled
				}
			case "key_delete", "disabled_delete":
				method, path, body = http.MethodDelete, path+"/keys/1", ""
				if operation == "disabled_delete" {
					path = "/" + common.Interface2String(provider.Id) + "/keys/disabled"
				}
				wantKey = providerKeyA + "\n" + providerKeyC
			case "delete":
				method, body, wantCount = http.MethodDelete, "", 0
			case "batch_delete":
				direct, wantCount = true, 0
				deleted, err := model.BatchDeleteProviders([]int{provider.Id})
				require.NoError(t, err)
				assert.EqualValues(t, 1, deleted)
			}
			if !direct {
				reply := callProviderAPI(t, engine, method, path, body)
				require.True(t, reply.Success, reply.Message)
				if operation == "create" || operation == "batch_create" {
					ids := reply.Data["ids"].([]any)
					require.Len(t, ids, int(wantCount)-1)
					for _, id := range ids {
						assert.NotZero(t, storedProvider(t, int(id.(float64))).Id)
					}
				}
			}
			var count int64
			require.NoError(t, model.DB.Model(&model.Provider{}).Count(&count).Error)
			assert.Equal(t, wantCount, count)
			if wantCount != 0 {
				current := storedProvider(t, provider.Id)
				assert.Equal(t, wantKey, current.Key)
				assert.Equal(t, wantStatus, current.Status)
				assert.Equal(t, provider.Models, current.Models)
				assert.Equal(t, provider.Group, current.Group)
				if operation == "patch" {
					assert.Equal(t, "updated account", current.Name)
				}
				if operation == "key_enable" || operation == "keys_enable" {
					assert.Empty(t, current.ProviderInfo.MultiKeyStatusList)
				}
				if operation == "key_disable" {
					assert.Equal(t, common.ChannelStatusManuallyDisabled, current.ProviderInfo.MultiKeyStatusList[0])
				}
			}
			var after []model.Ability
			require.NoError(t, model.DB.Order(`"channel_id", "group", "model"`).Find(&after).Error)
			assert.Equal(t, before, after, "account mutations must leave the historical projection untouched")
		})
	}
}

func TestProviderAdminAPICacheDoesNotDependOnLegacyAbilities(t *testing.T) {
	for _, failure := range []string{"read_forbidden", "table_unavailable"} {
		t.Run(failure, func(t *testing.T) {
			engine := newProviderAdminEngine(t)
			provider := &model.Provider{Name: "account without legacy projection", Type: constant.ChannelTypeOpenAI,
				Key: providerKeyA + "\n" + providerKeyB, Status: common.ChannelStatusEnabled, Group: "orphan-group", Models: "legacy-only-model",
				ProviderInfo: model.ProviderInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModePolling}}
			require.NoError(t, provider.Insert())
			original := model.DB
			transaction := original
			t.Cleanup(func() {
				if transaction != original {
					require.NoError(t, transaction.Rollback().Error)
				}
				model.DB = original
				common.MemoryCacheEnabled = false
			})
			var legacyReads atomic.Int32
			require.NoError(t, original.Callback().Query().Before("gorm:query").Register("forbid_account_ability_reads", func(tx *gorm.DB) {
				if tx.Statement.Table == "abilities" {
					legacyReads.Add(1)
					tx.AddError(errors.New("legacy ability reads forbidden"))
				}
			}))
			t.Cleanup(func() { require.NoError(t, original.Callback().Query().Remove("forbid_account_ability_reads")) })
			require.NoError(t, original.Exec(`CREATE FUNCTION reject_account_projection() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'legacy projection writes forbidden'; END $$`).Error)
			require.NoError(t, original.Exec(`CREATE TRIGGER reject_account_projection BEFORE INSERT OR UPDATE OR DELETE ON "abilities" FOR EACH STATEMENT EXECUTE FUNCTION reject_account_projection()`).Error)
			if failure == "table_unavailable" {
				transaction = original.Begin()
				require.NoError(t, transaction.Error)
				model.DB = transaction
				require.NoError(t, transaction.Exec(`ALTER TABLE "abilities" RENAME TO "unavailable_legacy_abilities"`).Error)
			}
			common.MemoryCacheEnabled = true
			require.NotPanics(t, model.InitProviderCache)
			cached, err := model.CacheGetProvider(provider.Id)
			require.NoError(t, err)
			key, slot, keyErr := cached.GetNextEnabledKey()
			require.Nil(t, keyErr)
			assert.Equal(t, providerKeyA, key)
			assert.Zero(t, slot)
			require.NotPanics(t, model.InitProviderCache)
			cached, err = model.CacheGetProvider(provider.Id)
			require.NoError(t, err)
			key, slot, keyErr = cached.GetNextEnabledKey()
			require.Nil(t, keyErr)
			assert.Equal(t, providerKeyB, key, "refresh must preserve the polling cursor")
			assert.Equal(t, 1, slot)
			// CRUD owns its own transactions. Restore this DDL transaction first;
			// the read/write rejection guards remain active for every API operation.
			if transaction != original {
				require.NoError(t, transaction.Rollback().Error)
				transaction, model.DB = original, original
			}
			path := "/" + common.Interface2String(provider.Id)
			for _, operation := range []struct{ method, path, body string }{
				{http.MethodPost, "", `{"provider":{"type":1,"name":"new cache-only account","key":"` + providerKeyC + `"}}`},
				{http.MethodPatch, path, `{"name":"renamed cache-only account"}`},
				{http.MethodPost, path + "/disable", ""},
				{http.MethodPost, path + "/enable", ""},
				{http.MethodPost, path + "/keys/0/disable", ""},
			} {
				reply := callProviderAPI(t, engine, operation.method, operation.path, operation.body)
				require.True(t, reply.Success, reply.Message)
			}
			cached, err = model.CacheGetProvider(provider.Id)
			require.NoError(t, err)
			assert.Equal(t, "renamed cache-only account", cached.Name)
			assert.Equal(t, common.ChannelStatusEnabled, cached.Status)
			assert.Equal(t, common.ChannelStatusManuallyDisabled, cached.ProviderInfo.MultiKeyStatusList[0])
			assert.Zero(t, legacyReads.Load(), "account refresh must never query the historical route projection")
		})
	}
}

func TestProviderAdminAPIAccountTransactionsRemainAtomic(t *testing.T) {
	for _, operation := range []string{"batch_insert", "batch_delete", "channel_reference"} {
		t.Run(operation, func(t *testing.T) {
			engine := newProviderAdminEngine(t)
			if operation == "channel_reference" {
				require.NoError(t, model.DB.AutoMigrate(&model.Model{}, &model.Channel{}))
				provider := &model.Provider{Name: "referenced account", Type: constant.ChannelTypeOpenAI, Key: providerKeyA}
				require.NoError(t, provider.Insert())
				unified := &model.Model{ModelName: "referenced model", Status: 1}
				require.NoError(t, model.DB.Create(unified).Error)
				channel := &model.Channel{ProviderID: provider.Id, ModelID: unified.Id, Protocol: "openai_chat", UpstreamModelName: "upstream", ParameterOverrides: "{}", Status: model.ChannelPending}
				require.NoError(t, model.DB.Create(channel).Error)
				reply := callProviderAPIStatus(t, engine, http.StatusBadRequest, http.MethodDelete, "/"+common.Interface2String(provider.Id), "")
				assert.False(t, reply.Success)
				assert.Equal(t, provider.Key, storedProvider(t, provider.Id).Key)
				var retained model.Channel
				require.NoError(t, model.DB.First(&retained, channel.ID).Error)
				assert.Equal(t, provider.Id, retained.ProviderID)
				return
			}
			rows := make([]model.Provider, 201)
			for i := range rows {
				rows[i] = model.Provider{Name: "transaction-account-" + common.Interface2String(i), Type: constant.ChannelTypeOpenAI, Key: providerKeyA}
			}
			rows[len(rows)-1].Name = "reject-last-account"
			if operation == "batch_insert" {
				require.NoError(t, model.DB.Exec(`CREATE FUNCTION reject_account_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW."name" = 'reject-last-account' THEN RAISE EXCEPTION 'account insert rejected'; END IF; RETURN NEW; END $$`).Error)
				require.NoError(t, model.DB.Exec(`CREATE TRIGGER reject_account_insert AFTER INSERT ON "providers" FOR EACH ROW EXECUTE FUNCTION reject_account_insert()`).Error)
				require.ErrorContains(t, model.BatchInsertProviders(rows), "account insert rejected")
				var count int64
				require.NoError(t, model.DB.Model(&model.Provider{}).Count(&count).Error)
				assert.Zero(t, count, "a later chunk failure must roll back every prior inserted account")
			} else {
				require.NoError(t, model.BatchInsertProviders(rows))
				ids := make([]int, len(rows))
				for i := range rows {
					require.NotZero(t, rows[i].Id)
					ids[i] = rows[i].Id
				}
				var before []model.Provider
				require.NoError(t, model.DB.Order("id").Find(&before).Error)
				require.NoError(t, model.DB.Exec(`CREATE FUNCTION reject_account_delete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF OLD."name" = 'reject-last-account' THEN RAISE EXCEPTION 'account delete rejected'; END IF; RETURN OLD; END $$`).Error)
				require.NoError(t, model.DB.Exec(`CREATE TRIGGER reject_account_delete AFTER DELETE ON "providers" FOR EACH ROW EXECUTE FUNCTION reject_account_delete()`).Error)
				count, err := model.BatchDeleteProviders(ids)
				require.ErrorContains(t, err, "account delete rejected")
				assert.Zero(t, count)
				var after []model.Provider
				require.NoError(t, model.DB.Order("id").Find(&after).Error)
				assert.Equal(t, before, after, "a second delete chunk failure must restore every account")
			}
		})
	}
}

func TestProviderAdminAPIManagesProviderWithWriteOnlyKeys(t *testing.T) {
	engine := newProviderAdminEngine(t)
	require.NoError(t, model.DB.Exec(`ALTER TABLE "providers" DROP COLUMN IF EXISTS "models", DROP COLUMN IF EXISTS "model_mapping",
		DROP COLUMN IF EXISTS "group", DROP COLUMN IF EXISTS "priority", DROP COLUMN IF EXISTS "weight"`).Error)

	created := callProviderAPI(t, engine, http.MethodPost, "", `{"mode":"multi_to_single","multi_key_mode":"polling","provider":{
		"name":"account one","type":1,"key":"`+providerKeyA+`\n`+providerKeyB+`\n`+providerKeyC+`",
		"base_url":"https://upstream.example"}}`)
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

func TestProviderAdminAPIEnablesKeysAfterAutomaticExhaustion(t *testing.T) {
	engine := newProviderAdminEngine(t)
	for _, endpoint := range []string{"/keys/0/enable", "/keys/enable"} {
		for _, manualDisable := range []bool{false, true} {
			t.Run(endpoint+"/manual="+common.Interface2String(manualDisable), func(t *testing.T) {
				provider := &model.Provider{Name: t.Name(), Type: constant.ChannelTypeOpenAI, Key: providerKeyA + "\n" + providerKeyB,
					Status: common.ChannelStatusEnabled, Models: "test-model", Group: "default",
					ProviderInfo: model.ProviderInfo{IsMultiKey: true, MultiKeySize: 2}}
				require.NoError(t, provider.Insert())
				for index, key := range []string{providerKeyA, providerKeyB} {
					changed, err := model.ApplyProviderStatus(provider.Id, model.ProviderStatusChange{
						Status: common.ChannelStatusAutoDisabled, UsingKey: key, KeyIndex: &index, Reason: "authentication rejected"})
					require.NoError(t, err)
					require.True(t, changed)
				}
				path := "/" + common.Interface2String(provider.Id)
				before := callProviderAPI(t, engine, http.MethodGet, path, "")
				require.EqualValues(t, common.ChannelStatusAutoDisabled, before.Data["status"])
				wantStatus := common.ChannelStatusEnabled
				if manualDisable {
					require.True(t, callProviderAPI(t, engine, http.MethodPost, path+"/disable", "").Success)
					wantStatus = common.ChannelStatusManuallyDisabled
				}
				require.True(t, callProviderAPI(t, engine, http.MethodPost, path+endpoint, "").Success)
				after := callProviderAPI(t, engine, http.MethodGet, path, "")
				assert.EqualValues(t, wantStatus, after.Data["status"])
				keys := callProviderAPI(t, engine, http.MethodGet, path+"/keys", "")
				assert.Greater(t, keys.Data["enabled_count"].(float64), float64(0))
			})
		}
	}
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
			request.Header.Set("Authorization", "Bearer "+engine.accessToken)
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
		{name: "unsupported add mode", method: http.MethodPost, path: "", body: `{"mode":"copy","provider":{"name":"x","type":1,"key":"` + providerKeyB + `"}}`},
		{name: "duplicate create envelope", method: http.MethodPost, path: "", body: `{"provider":{"models":"legacy"},"provider":{"name":"x","type":1,"key":"` + providerKeyB + `"}}`},
		{name: "trailing create object", method: http.MethodPost, path: "", body: `{"provider":{"name":"x","type":1,"key":"` + providerKeyB + `"}} {}`},
		{name: "null create object", method: http.MethodPost, path: "", body: `{"provider":null}`},
		{name: "duplicate update field", method: http.MethodPatch, path: path, body: `{"name":"first","name":"second"}`},
		{name: "trailing update object", method: http.MethodPatch, path: path, body: `{"name":"changed"} {}`},
		{name: "null update object", method: http.MethodPatch, path: path, body: `null`},
		{name: "non-object settings", method: http.MethodPatch, path: path, body: `{"settings":"[]"}`},
		{name: "duplicate settings field", method: http.MethodPatch, path: path, body: `{"settings":"{\"aws_region\":\"us-east-1\",\"aws_region\":\"us-west-2\"}"}`},
		{name: "trailing settings object", method: http.MethodPatch, path: path, body: `{"settings":"{} {}"}`},
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
	current := storedProvider(t, provider.Id)
	assert.Equal(t, common.ChannelStatusEnabled, current.Status)
	assert.Equal(t, "single key account", current.Name)
	assert.Equal(t, providerKeyA, current.Key)
	assert.Empty(t, current.OtherSettings)
}
