package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/QuantumNous/new-api/setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupTaskPluginControllerTest(t *testing.T) {
	t.Helper()
	originalDB := model.DB
	database := model.UseTestDatabases(t)
	require.NoError(t, database.AutoMigrate(&model.TaskPlugin{}, &model.Provider{}, &model.Ability{}, &model.Task{}, &model.Option{}))
	t.Cleanup(func() { model.DB = originalDB })
}

const lifecyclePluginSource = `
export const meta = {apiVersion: 1, key: "lifecycle-only", name: "Lifecycle", version: "1.0.0", author: {name: "Test"}, models: ["doc"], fetchMode: "per_task"};
export function buildSubmitRequest() { return {}; }
export function parseSubmitResponse() { return {}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {}; }
`

func taskPluginControllerTestSource(key, version string) string {
	return fmt.Sprintf(`
export const meta = {apiVersion: 1, key: %q, name: "Test", version: %q, author: {name: "Test"}, models: ["doc-1"], fetchMode: "per_task"};
export function buildSubmitRequest() { return {}; }
export function parseSubmitResponse() { return {}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {}; }
`, key, version)
}

func taskPluginControllerChannelSource(key, version string, channelType int) string {
	return fmt.Sprintf(`
export const meta = {apiVersion: 1, key: %q, name: "Test", version: %q, author: {name: "Test"}, channelTypes: [%d], models: ["doc-1"], fetchMode: "per_task"};
export function buildSubmitRequest() { return {}; }
export function parseSubmitResponse() { return {}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {}; }
`, key, version, channelType)
}

func cleanupTaskPluginControllerRuntime(t *testing.T, key string) {
	t.Helper()
	t.Cleanup(func() {
		jsplugin.DefaultRegistry.Unregister(key)
		taskPluginSyncState.Lock()
		delete(taskPluginSyncState.hashes, key)
		delete(taskPluginSyncState.errors, key)
		taskPluginSyncState.Unlock()
	})
}

func TestDeleteThirdPartyPluginReportsAssociatedChannelsAndInFlightTasks(t *testing.T) {
	setupTaskPluginControllerTest(t)
	loaded, err := jsplugin.DefaultRegistry.Register(lifecyclePluginSource, jsplugin.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { jsplugin.DefaultRegistry.Unregister("lifecycle-only") })
	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{Key: loaded.Meta.Key, APIVersion: 1, Version: "1", Source: lifecyclePluginSource, SourceHash: "hash", Enabled: true}))
	baseURL := "https://example.com"
	setting := `{"task_plugin_key":"lifecycle-only"}`
	channel := model.Provider{Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Name: "linked", Models: "doc", Group: "default", BaseURL: &baseURL, Setting: &setting}
	require.NoError(t, channel.Insert())
	require.NoError(t, model.DB.Create(&model.Task{Platform: "lifecycle-only", Status: model.TaskStatusInProgress}).Error)

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Params = gin.Params{{Key: "key", Value: "lifecycle-only"}, {Key: "version", Value: "1"}}
	context.Request = httptest.NewRequest(http.MethodDelete, "/api/plugin/task/lifecycle-only/versions/1", nil)
	DeleteTaskPluginVersion(context)

	assert.Contains(t, recorder.Body.String(), `"name":"linked"`)
	assert.Contains(t, recorder.Body.String(), `"in_flight_count":1`)
}

func TestDisableThirdPartyPluginSupportsCascadeAndForce(t *testing.T) {
	setupTaskPluginControllerTest(t)
	loaded, err := jsplugin.DefaultRegistry.Register(lifecyclePluginSource, jsplugin.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { jsplugin.DefaultRegistry.Unregister("lifecycle-only") })
	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{Key: loaded.Meta.Key, APIVersion: 1, Version: "1", Source: lifecyclePluginSource, SourceHash: "hash", Enabled: true}))
	baseURL := "https://example.com"
	setting := `{"task_plugin_key":"lifecycle-only"}`
	channel := model.Provider{Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Name: "linked", Models: "doc", Group: "default", BaseURL: &baseURL, Setting: &setting}
	require.NoError(t, channel.Insert())
	gatewaySetting := `{"task_extend_plugin_keys":["lifecycle-only","other"]}`
	gateway := model.Provider{Type: constant.ChannelTypeNewAPI, Status: common.ChannelStatusEnabled, Name: "gateway", Models: "doc,gpt", Group: "default", BaseURL: &baseURL, Setting: &gatewaySetting}
	require.NoError(t, gateway.Insert())
	require.NoError(t, model.DB.Create(&model.Task{Platform: "lifecycle-only", Status: model.TaskStatusSubmitted}).Error)

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Params = gin.Params{{Key: "key", Value: "lifecycle-only"}}
	context.Request = httptest.NewRequest(http.MethodPost, "/api/plugin/task/lifecycle-only/status?cascade=true&force=true", strings.NewReader(`{"enabled":false}`))
	context.Request.Header.Set("Content-Type", "application/json")
	SetTaskPluginStatus(context)

	assert.Contains(t, recorder.Body.String(), `"success":true`)
	assert.Contains(t, recorder.Body.String(), `"disabled_channels":1`)
	assert.Contains(t, recorder.Body.String(), `"unbound_channels":1`)
	updated, err := model.GetProviderById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusManuallyDisabled, updated.Status)
	updatedGateway, err := model.GetProviderById(gateway.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusEnabled, updatedGateway.Status, "a gateway channel keeps serving its other traffic")
	assert.Equal(t, []string{"other"}, updatedGateway.GetSetting().TaskExtendPluginKeys, "only the disabled plugin is unbound")
}

// klingFactoryVersion returns the version declared in the embedded kling factory
// manifest so tests do not hardcode a value that moves with every plugin release.
func klingFactoryVersion(t *testing.T) string {
	t.Helper()
	factorySource, err := plugins.Source("kling")
	require.NoError(t, err)
	match := regexp.MustCompile(`version:\s*"([^"]+)"`).FindStringSubmatch(factorySource)
	require.Len(t, match, 2, "kling factory manifest must declare a version")
	return match[1]
}

func setupTaskPluginFactoryDisableTest(t *testing.T) {
	t.Helper()
	setupTaskPluginControllerTest(t)
	originalMap := common.OptionMap
	common.OptionMapRWMutex.Lock()
	common.OptionMap = map[string]string{}
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		jsplugin.DefaultRegistry.SetDisabledFactoryKeys(nil)
		common.OptionMapRWMutex.Lock()
		common.OptionMap = originalMap
		common.OptionMapRWMutex.Unlock()
	})
}

func postTaskPluginStatus(t *testing.T, key, query, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Params = gin.Params{{Key: "key", Value: key}}
	path := "/api/plugin/task/" + key + "/status"
	if query != "" {
		path += "?" + query
	}
	context.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	context.Request.Header.Set("Content-Type", "application/json")
	SetTaskPluginStatus(context)
	return recorder
}

func listTaskPluginItem(t *testing.T, key string) taskPluginListItem {
	t.Helper()
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/plugin/task", nil)
	ListTaskPlugins(context)
	var response struct {
		Success bool                 `json:"success"`
		Data    []taskPluginListItem `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success)
	for _, item := range response.Data {
		if item.Meta.Key == key {
			return item
		}
	}
	t.Fatalf("task plugin %q not found", key)
	return taskPluginListItem{}
}

func taskPluginOptionsHasKey(t *testing.T, key string) bool {
	t.Helper()
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/task_plugin_options", nil)
	GetTaskPluginOptions(context)
	var response struct {
		Success bool `json:"success"`
		Data    []struct {
			Key string `json:"key"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success)
	for _, option := range response.Data {
		if option.Key == key {
			return true
		}
	}
	return false
}

func TestDisableFactoryPluginPersistsOptionAndHidesFromBindOptions(t *testing.T) {
	setupTaskPluginFactoryDisableTest(t)
	const key = "kling"

	recorder := postTaskPluginStatus(t, key, "", `{"enabled":false}`)
	assert.Contains(t, recorder.Body.String(), `"success":true`)
	assert.Equal(t, []string{key}, setting.GetTaskPluginDisabledFactoryKeys())
	var stored model.Option
	require.NoError(t, model.DB.Where("key = ?", setting.TaskPluginDisabledFactoryKeysKey).First(&stored).Error)
	assert.Equal(t, `["kling"]`, stored.Value)

	item := listTaskPluginItem(t, key)
	assert.Equal(t, "factory", item.Source)
	assert.False(t, item.Enabled)
	assert.Equal(t, "disabled", item.RuntimeStatus)
	assert.False(t, taskPluginOptionsHasKey(t, key))
	_, ok := jsplugin.DefaultRegistry.Get(key)
	assert.False(t, ok)

	recorder = postTaskPluginStatus(t, key, "", `{"enabled":true}`)
	assert.Contains(t, recorder.Body.String(), `"success":true`)
	assert.Empty(t, setting.GetTaskPluginDisabledFactoryKeys())
	item = listTaskPluginItem(t, key)
	assert.True(t, item.Enabled)
	assert.Equal(t, "registered", item.RuntimeStatus)
	assert.True(t, taskPluginOptionsHasKey(t, key))
	_, ok = jsplugin.DefaultRegistry.Get(key)
	assert.True(t, ok)
}

func TestDisableFactoryPluginRespectsInUseGuard(t *testing.T) {
	setupTaskPluginFactoryDisableTest(t)
	const key = "kling"
	baseURL := "https://example.com"
	channelSetting := `{"task_plugin_key":"kling"}`
	channel := model.Provider{Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Name: "linked-factory", Models: "doc", Group: "default", BaseURL: &baseURL, Setting: &channelSetting}
	require.NoError(t, channel.Insert())

	recorder := postTaskPluginStatus(t, key, "", `{"enabled":false}`)
	assert.Contains(t, recorder.Body.String(), `"success":false`)
	assert.Contains(t, recorder.Body.String(), "task plugin is still in use")
	assert.Empty(t, setting.GetTaskPluginDisabledFactoryKeys())
	_, ok := jsplugin.DefaultRegistry.Get(key)
	assert.True(t, ok)
}

func TestDisableFactoryOverrideRowSuppressesBothLayers(t *testing.T) {
	setupTaskPluginFactoryDisableTest(t)
	factorySource, err := plugins.Source("kling")
	require.NoError(t, err)
	factoryVersion := klingFactoryVersion(t)
	overrideSource := strings.Replace(factorySource, `version: "`+factoryVersion+`"`, `version: "`+factoryVersion+`-test-factory-status"`, 1)
	require.NotEqual(t, factorySource, overrideSource, "factory version marker must be found in kling source")
	loaded, err := jsplugin.DefaultRegistry.Register(overrideSource, jsplugin.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { jsplugin.DefaultRegistry.Unregister("kling") })
	plugin := model.TaskPlugin{
		Key: "kling", APIVersion: loaded.Meta.APIVersion, Version: loaded.Meta.Version,
		Source: model.LongText(overrideSource), SourceHash: "test-hash", Enabled: true,
	}
	require.NoError(t, model.SaveTaskPlugin(&plugin))
	require.NoError(t, syncTaskPluginsOnce())

	recorder := postTaskPluginStatus(t, "kling", "", `{"enabled":false}`)
	assert.Contains(t, recorder.Body.String(), `"success":true`)
	assert.Equal(t, []string{"kling"}, setting.GetTaskPluginDisabledFactoryKeys())
	row, err := model.GetTaskPluginVersion("kling", "")
	require.NoError(t, err)
	assert.False(t, row.Enabled)
	item := listTaskPluginItem(t, "kling")
	assert.Equal(t, "override_over_factory", item.Source)
	assert.False(t, item.Enabled)
	assert.Equal(t, "disabled", item.RuntimeStatus)
	assert.False(t, taskPluginOptionsHasKey(t, "kling"))
	_, ok := jsplugin.DefaultRegistry.Get("kling")
	assert.False(t, ok, "factory built-in must not keep serving after the plugin is switched off")

	recorder = postTaskPluginStatus(t, "kling", "", `{"enabled":true}`)
	assert.Contains(t, recorder.Body.String(), `"success":true`)
	assert.Empty(t, setting.GetTaskPluginDisabledFactoryKeys())
	row, err = model.GetTaskPluginVersion("kling", "")
	require.NoError(t, err)
	assert.True(t, row.Enabled)
	got, ok := jsplugin.DefaultRegistry.Get("kling")
	require.True(t, ok)
	assert.Equal(t, loaded.Meta.Version, got.Meta.Version)
	assert.Equal(t, "registered", listTaskPluginItem(t, "kling").RuntimeStatus)

	// Regression: with every kling layer off, a plugin whose model differs from
	// a built-in kling model only by case must upload without a routing conflict.
	recorder = postTaskPluginStatus(t, "kling", "", `{"enabled":false}`)
	require.Contains(t, recorder.Body.String(), `"success":true`)
	cleanupTaskPluginControllerRuntime(t, "kling-shadow")
	const shadowSource = `
export const meta = {apiVersion: 1, key: "kling-shadow", name: "Shadow", version: "1.0.0", author: {name: "Test"}, models: ["KLING-V1"], fetchMode: "per_task"};
export function buildSubmitRequest() { return {}; }
export function parseSubmitResponse() { return {}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {}; }
`
	body, err := common.Marshal(map[string]any{"source": shadowSource})
	require.NoError(t, err)
	uploadRecorder := httptest.NewRecorder()
	uploadContext, _ := gin.CreateTestContext(uploadRecorder)
	uploadContext.Request = httptest.NewRequest(http.MethodPost, "/api/plugin/task", strings.NewReader(string(body)))
	uploadContext.Request.Header.Set("Content-Type", "application/json")
	UploadTaskPlugin(uploadContext)
	assert.Contains(t, uploadRecorder.Body.String(), `"success":true`)
	_, ok = jsplugin.DefaultRegistry.Get("kling-shadow")
	assert.True(t, ok)
}

// The shipped alibaba plugin is larger than 64 KiB, so storing it through the
// model path the upload endpoint uses and reading it back unchanged proves the
// source and icon columns are not truncated.
func TestTaskPluginLargeSourceRoundTrips(t *testing.T) {
	database := openTaskTestDatabase(t, &model.TaskPlugin{})
	originalDB := model.DB
	model.DB = database
	t.Cleanup(func() { model.DB = originalDB })

	source, err := plugins.Source("alibaba")
	require.NoError(t, err)
	require.Greater(t, len(source), 65535, "fixture must exceed 64 KiB")
	icon := "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(strings.Repeat(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`, 2048)))
	require.Greater(t, len(icon), 65535, "icon fixture must exceed 64 KiB")

	plugin := model.TaskPlugin{Key: "alibaba", APIVersion: 1, Version: "roundtrip", Source: model.LongText(source), SourceHash: "hash", Enabled: true}
	require.NoError(t, model.SaveTaskPlugin(&plugin))
	// Re-saving the same version attaches the icon through the update path.
	withIcon := model.TaskPlugin{Key: "alibaba", APIVersion: 1, Version: "roundtrip", Source: model.LongText(source), SourceHash: "hash", Icon: model.LongText(icon), Enabled: true}
	require.NoError(t, model.SaveTaskPlugin(&withIcon))

	stored, err := model.GetTaskPluginVersion("alibaba", "roundtrip")
	require.NoError(t, err)
	assert.Equal(t, source, string(stored.Source))
	assert.Equal(t, icon, string(stored.Icon))

	// The 30-second sync poll must not pull these payloads; it reads hashes
	// and fetches source only for rows it has to recompile.
	snapshot, err := model.GetTaskPluginSyncSnapshot()
	require.NoError(t, err)
	require.Len(t, snapshot.Plugins, 1)
	assert.Equal(t, stored.Id, snapshot.Plugins[0].Id)
	assert.Equal(t, "hash", snapshot.Plugins[0].SourceHash)
	assert.Empty(t, snapshot.Plugins[0].Source)
	assert.Empty(t, snapshot.Plugins[0].Icon)
	loaded, err := model.GetTaskPluginSource(snapshot.Plugins[0].Id)
	require.NoError(t, err)
	assert.Equal(t, source, string(loaded))
}

func TestUploadTaskPluginAcceptsSourcesUpToEightMiB(t *testing.T) {
	setupTaskPluginControllerTest(t)
	cleanupTaskPluginControllerRuntime(t, "large-source")
	upload := func(source string) *httptest.ResponseRecorder {
		body, err := common.Marshal(map[string]any{"source": source})
		require.NoError(t, err)
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Request = httptest.NewRequest(http.MethodPost, "/api/plugin/task", bytes.NewReader(body))
		context.Request.Header.Set("Content-Type", "application/json")
		UploadTaskPlugin(context)
		return recorder
	}

	// A trailing comment pads a valid plugin past the former 1 MiB cap.
	accepted := taskPluginControllerTestSource("large-source", "1.0.0") + "// " + strings.Repeat("x", 2<<20) + "\n"
	recorder := upload(accepted)
	require.Contains(t, recorder.Body.String(), `"success":true`)
	stored, err := model.GetTaskPluginVersion("large-source", "1.0.0")
	require.NoError(t, err)
	assert.Equal(t, accepted, string(stored.Source))

	rejected := upload(accepted + strings.Repeat("x", 6<<20))
	assert.Contains(t, rejected.Body.String(), `"success":false`)
	assert.Contains(t, rejected.Body.String(), "plugin source exceeds 8 MiB")
	_, err = model.GetTaskPluginVersion("large-source", "2.0.0")
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestListTaskPluginsIncludesFactoryWithoutDatabaseRows(t *testing.T) {
	setupTaskPluginControllerTest(t)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/plugin/task", nil)

	ListTaskPlugins(context)

	var response struct {
		Success bool                 `json:"success"`
		Data    []taskPluginListItem `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success)
	var factoryItem *taskPluginListItem
	for i := range response.Data {
		if response.Data[i].Meta.Key == "kling" {
			factoryItem = &response.Data[i]
			break
		}
	}
	require.NotNil(t, factoryItem)
	assert.Equal(t, "factory", factoryItem.Source)
	assert.Equal(t, "registered", factoryItem.RuntimeStatus)
	assert.NotEmpty(t, factoryItem.SourceHash)
}

func TestMasterSwitchEmptiesOptionsAndKeepsList(t *testing.T) {
	setupTaskPluginControllerTest(t)
	originalEnabled := constant.TaskPluginEnabled
	jsplugin.DefaultRegistry.SetEnabled(false)
	t.Cleanup(func() {
		constant.TaskPluginEnabled = originalEnabled
		jsplugin.DefaultRegistry.SetEnabled(originalEnabled)
	})

	assert.False(t, taskPluginOptionsHasKey(t, "kling"))
	item := listTaskPluginItem(t, "kling")
	assert.Equal(t, "factory", item.Source)
	assert.Equal(t, "kling", item.Meta.Key)
}

func TestGetTaskPluginOptionsIncludesDescriptionUsageSchemaIconBaseURLAndChannelTypes(t *testing.T) {
	setupTaskPluginControllerTest(t)
	const key = "usage-options-probe"
	source := `
export const meta = {
  apiVersion: 1, key: "usage-options-probe", name: "Usage Options", version: "1.0.0", author: {name: "Test"},
  description: {en: "Video generation via the vendor API", zh: "通过厂商接口生成视频"},
  icon: "text:UO", baseUrl: "http://localhost:9000/",
  channelTypes: [1990, 1991],
  upstreams: ["vendor", "new_api"],
  models: ["usage-options-model"], fetchMode: "per_task",
  usageSchema: {seconds: {type: "number", unit: "second", description: "Video generation unit price"}},
  usageProfiles: [{models:["usage-options-model"],schema:{image_count:{type:"number",unit:"count"}}}]
};
export function buildSubmitRequest() { return {}; }
export function parseSubmitResponse() { return {}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {}; }
`
	_, err := jsplugin.DefaultRegistry.Register(source, jsplugin.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { jsplugin.DefaultRegistry.Unregister(key) })

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/task_plugin_options", nil)

	GetTaskPluginOptions(context)

	var response struct {
		Success bool `json:"success"`
		Data    []struct {
			Key           string                               `json:"key"`
			Description   jsplugin.LocalizedText               `json:"description"`
			Icon          string                               `json:"icon"`
			BaseURL       string                               `json:"baseUrl"`
			ChannelTypes  []int                                `json:"channelTypes"`
			Upstreams     []string                             `json:"upstreams"`
			UsageSchema   map[string]jsplugin.UsageFieldSchema `json:"usageSchema"`
			UsageProfiles []jsplugin.UsageProfile              `json:"usageProfiles"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success)
	for _, option := range response.Data {
		if option.Key != key {
			continue
		}
		assert.Equal(t, jsplugin.LocalizedText{"en": "Video generation via the vendor API", "zh": "通过厂商接口生成视频"}, option.Description)
		assert.Equal(t, "second", option.UsageSchema["seconds"].Unit)
		assert.Equal(t, "Video generation unit price", option.UsageSchema["seconds"].Description["en"])
		require.Len(t, option.UsageProfiles, 1)
		assert.Equal(t, []string{"usage-options-model"}, option.UsageProfiles[0].Models)
		assert.Equal(t, "count", option.UsageProfiles[0].Schema["image_count"].Unit)
		assert.Equal(t, "text:UO", option.Icon)
		assert.Equal(t, []int{1990, 1991}, option.ChannelTypes)
		assert.Equal(t, []string{"vendor", "new_api"}, option.Upstreams, "the New API channel form lists only plugins declaring new_api")
		assert.Equal(t, "http://localhost:9000", option.BaseURL, "the drawer prefills the normalized plugin default")
		return
	}
	t.Fatal("task plugin option not found")
}

func TestDeleteActiveOverrideFallsBackToFactoryAndDeletesRecord(t *testing.T) {
	setupTaskPluginControllerTest(t)
	factorySource, err := plugins.Source("kling")
	require.NoError(t, err)
	factoryVersion := klingFactoryVersion(t)
	overrideSource := strings.Replace(factorySource, `version: "`+factoryVersion+`"`, `version: "`+factoryVersion+`-test-override"`, 1)
	require.NotEqual(t, factorySource, overrideSource, "factory version marker must be found in kling source")
	loaded, err := jsplugin.DefaultRegistry.Register(overrideSource, jsplugin.Options{Key: "kling", Version: "test-override"})
	require.NoError(t, err)
	t.Cleanup(func() { jsplugin.DefaultRegistry.Unregister("kling") })
	plugin := model.TaskPlugin{
		Key: "kling", APIVersion: loaded.Meta.APIVersion, Version: loaded.Meta.Version,
		Source: model.LongText(overrideSource), SourceHash: "test-hash", Enabled: true,
	}
	require.NoError(t, model.SaveTaskPlugin(&plugin))
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Params = gin.Params{{Key: "key", Value: "kling"}, {Key: "version", Value: loaded.Meta.Version}}
	context.Request = httptest.NewRequest(http.MethodDelete, "/api/plugin/task/kling/versions/"+loaded.Meta.Version, nil)

	DeleteTaskPluginVersion(context)

	assert.Contains(t, recorder.Body.String(), `"success":true`)
	versions, err := model.ListTaskPluginVersions("kling")
	require.NoError(t, err)
	assert.Empty(t, versions)
	runtimePlugin, ok := jsplugin.DefaultRegistry.Get("kling")
	require.True(t, ok)
	assert.NotEqual(t, loaded.Meta.Version, runtimePlugin.Meta.Version)
}

func TestDeleteActiveTaskPluginPromotesEnabledVersionInRuntime(t *testing.T) {
	setupTaskPluginControllerTest(t)
	key := "delete-promote-probe"
	cleanupTaskPluginControllerRuntime(t, key)
	v1Source := taskPluginControllerTestSource(key, "1.0.0")
	v2Source := taskPluginControllerTestSource(key, "2.0.0")
	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{
		Key: key, APIVersion: 1, Version: "1.0.0", Source: model.LongText(v1Source), SourceHash: "hash-v1", Enabled: true,
	}))
	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{
		Key: key, APIVersion: 1, Version: "2.0.0", Source: model.LongText(v2Source), SourceHash: "hash-v2", Enabled: true,
	}))
	_, err := jsplugin.DefaultRegistry.Register(v1Source, jsplugin.Options{Key: key, Version: "1.0.0"})
	require.NoError(t, err)
	taskPluginSyncState.Lock()
	taskPluginSyncState.hashes[key] = "hash-v1"
	taskPluginSyncState.errors[key] = "stale compile error"
	taskPluginSyncState.Unlock()

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Params = gin.Params{{Key: "key", Value: key}, {Key: "version", Value: "1.0.0"}}
	context.Request = httptest.NewRequest(http.MethodDelete, "/api/plugin/task/"+key+"/versions/1.0.0", nil)
	DeleteTaskPluginVersion(context)

	assert.Contains(t, recorder.Body.String(), `"success":true`)
	active, err := model.GetTaskPluginVersion(key, "")
	require.NoError(t, err)
	assert.Equal(t, "2.0.0", active.Version)
	runtimePlugin, ok := jsplugin.DefaultRegistry.Get(key)
	require.True(t, ok)
	assert.Equal(t, "2.0.0", runtimePlugin.Meta.Version)
	taskPluginSyncState.Lock()
	syncedHash := taskPluginSyncState.hashes[key]
	_, hasSyncError := taskPluginSyncState.errors[key]
	taskPluginSyncState.Unlock()
	assert.Equal(t, "hash-v2", syncedHash)
	assert.False(t, hasSyncError)
}

func TestUploadTaskPluginRefreshesRuntimeSyncState(t *testing.T) {
	setupTaskPluginControllerTest(t)
	key := "upload-sync-probe"
	cleanupTaskPluginControllerRuntime(t, key)
	source := taskPluginControllerTestSource(key, "1.0.0")
	taskPluginSyncState.Lock()
	taskPluginSyncState.hashes[key] = "stale-hash"
	taskPluginSyncState.errors[key] = "stale compile error"
	taskPluginSyncState.Unlock()
	body, err := common.Marshal(map[string]any{"source": source})
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, "/api/plugin/task", strings.NewReader(string(body)))
	context.Request.Header.Set("Content-Type", "application/json")
	UploadTaskPlugin(context)

	assert.Contains(t, recorder.Body.String(), `"success":true`)
	stored, err := model.GetTaskPluginVersion(key, "")
	require.NoError(t, err)
	taskPluginSyncState.Lock()
	syncedHash := taskPluginSyncState.hashes[key]
	_, hasSyncError := taskPluginSyncState.errors[key]
	taskPluginSyncState.Unlock()
	assert.Equal(t, stored.SourceHash, syncedHash)
	assert.False(t, hasSyncError)
}

func TestActivateTaskPluginRefreshesRuntimeSyncState(t *testing.T) {
	setupTaskPluginControllerTest(t)
	key := "activate-sync-probe"
	cleanupTaskPluginControllerRuntime(t, key)
	v1Source := taskPluginControllerTestSource(key, "1.0.0")
	v2Source := taskPluginControllerTestSource(key, "2.0.0")
	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{
		Key: key, APIVersion: 1, Version: "1.0.0", Source: model.LongText(v1Source), SourceHash: "hash-v1", Enabled: true,
	}))
	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{
		Key: key, APIVersion: 1, Version: "2.0.0", Source: model.LongText(v2Source), SourceHash: "hash-v2", Enabled: true,
	}))
	_, err := jsplugin.DefaultRegistry.Register(v1Source, jsplugin.Options{Key: key, Version: "1.0.0"})
	require.NoError(t, err)
	taskPluginSyncState.Lock()
	taskPluginSyncState.hashes[key] = "hash-v1"
	taskPluginSyncState.errors[key] = "stale compile error"
	taskPluginSyncState.Unlock()

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Params = gin.Params{{Key: "key", Value: key}}
	context.Request = httptest.NewRequest(http.MethodPost, "/api/plugin/task/"+key+"/activate", strings.NewReader(`{"version":"2.0.0"}`))
	context.Request.Header.Set("Content-Type", "application/json")
	ActivateTaskPlugin(context)

	assert.Contains(t, recorder.Body.String(), `"success":true`)
	runtimePlugin, ok := jsplugin.DefaultRegistry.Get(key)
	require.True(t, ok)
	assert.Equal(t, "2.0.0", runtimePlugin.Meta.Version)
	taskPluginSyncState.Lock()
	syncedHash := taskPluginSyncState.hashes[key]
	_, hasSyncError := taskPluginSyncState.errors[key]
	taskPluginSyncState.Unlock()
	assert.Equal(t, "hash-v2", syncedHash)
	assert.False(t, hasSyncError)
}

func taskPluginDiagnosticIdentity(t *testing.T) (service.AuthIdentity, string) {
	t.Helper()
	previousRedis := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = previousRedis })
	require.NoError(t, model.DB.AutoMigrate(&model.User{}, &model.UserSession{}, &model.UserAccessToken{}, &model.CasbinRule{}, &model.AuthzRole{}, &model.Model{}, &model.Channel{}, &model.ModelOffering{}, &model.ModelOfferingMember{}))
	require.NoError(t, model.MigratePricing(model.DB))
	user := model.User{Username: "pricing-root", Role: common.RoleRootUser, Status: common.UserStatusEnabled, AuthVersion: 1, AffCode: "pricing-root"}
	require.NoError(t, model.DB.Create(&user).Error)
	return createAccessTokenTestSession(t, user.Id, "pricing-session")
}

func taskPluginDiagnosticContext(t *testing.T, method, path, body string, params gin.Params, identity service.AuthIdentity, credential string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Params = params
	c.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Authorization", "Bearer "+credential)
	c.Set("id", identity.UserID)
	c.Set("session_id", identity.SessionID)
	c.Set("auth_version", identity.UserAuthVersion)
	c.Set("session_version", identity.SessionVersion)
	return c, recorder
}

func taskPluginDiagnosticChannel(t *testing.T, key, name, expression string, actor int) model.Channel {
	t.Helper()
	member := model.Model{ModelName: name}
	require.NoError(t, model.DB.Where("model_name = ?", name).FirstOrCreate(&member).Error)
	provider := model.Provider{Name: "private-provider", Key: "fixture-only"}
	require.NoError(t, model.DB.Create(&provider).Error)
	channel := model.Channel{ProviderID: provider.Id, Protocol: protocol.Type("task:" + key), UpstreamModelName: name, ModelID: member.Id, ParameterOverrides: "{}", Status: model.ChannelPending}
	require.NoError(t, model.DB.Omit("Provider", "Model").Create(&channel).Error)
	if expression != "" {
		require.NoError(t, model.DB.Omit("Creator", "Channel", "Offering").Create(&model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: channel.ID, ChannelID: &channel.ID, Currency: "USD", Expression: expression, EffectiveAt: 1, CreatedBy: actor}).Error)
	}
	return channel
}

func TestTaskPluginPricingDiagnosticsNoOwnersAndAuthorization(t *testing.T) {
	setupTaskPluginControllerTest(t)
	previousRedis := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = previousRedis })
	require.NoError(t, model.DB.AutoMigrate(&model.User{}, &model.UserSession{}, &model.UserAccessToken{}, &model.CasbinRule{}, &model.AuthzRole{}, &model.Model{}, &model.Channel{}, &model.ModelOffering{}, &model.ModelOfferingMember{}, &model.PriceVersion{}))
	user := model.User{Username: "diagnostic-root", Role: common.RoleRootUser, Status: common.UserStatusEnabled, AuthVersion: 1, AffCode: "diagnostic-root"}
	require.NoError(t, model.DB.Create(&user).Error)
	identity, credential := createAccessTokenTestSession(t, user.Id, "diagnostic-session")
	for _, authorized := range []bool{false, true} {
		key := fmt.Sprintf("diagnostic-ownerless-%t", authorized)
		cleanupTaskPluginControllerRuntime(t, key)
		body, err := common.Marshal(map[string]any{"source": taskPluginControllerTestSource(key, "1.0.0")})
		require.NoError(t, err)
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/plugin/task", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		if authorized {
			c.Request.Header.Set("Authorization", "Bearer "+credential)
			c.Set("id", identity.UserID)
			c.Set("session_id", identity.SessionID)
			c.Set("auth_version", identity.UserAuthVersion)
			c.Set("session_version", identity.SessionVersion)
		}
		UploadTaskPlugin(c)
		var response struct {
			Success bool `json:"success"`
			Data    struct {
				Diagnostics struct {
					State  string `json:"state"`
					Issues []any  `json:"issues"`
				} `json:"pricing_diagnostics"`
			} `json:"data"`
		}
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
		require.True(t, response.Success, recorder.Body.String())
		want := "not_authorized"
		if authorized {
			want = "ready"
		}
		assert.Equal(t, want, response.Data.Diagnostics.State)
		assert.NotNil(t, response.Data.Diagnostics.Issues)
		assert.Empty(t, response.Data.Diagnostics.Issues, "plugin model declarations alone are not persisted owners")
		assert.NotContains(t, recorder.Body.String(), `"pricing_issues"`)
	}
}

// Every change that puts a version into service answers with the prices the
// serving plugin cannot charge, read after the registry sync.
func TestTaskPluginChangesReportPricingIssuesOfServingPlugin(t *testing.T) {
	setupTaskPluginControllerTest(t)
	identity, credential := taskPluginDiagnosticIdentity(t)
	key := "pricing-notice-probe"
	channel := taskPluginDiagnosticChannel(t, key, "doc-1", "", identity.UserID)
	cleanupTaskPluginControllerRuntime(t, key)
	upload := func(version string) string {
		source := taskPluginControllerTestSource(key, version)
		if version == "2.0.0" {
			source = strings.Replace(source, `["doc-1"]`, `["doc-2"]`, 1)
		}
		body, err := common.Marshal(map[string]any{"source": source})
		require.NoError(t, err)
		return string(body)
	}
	unpriced := []model.TaskPluginPricingIssue{{OwnerType: model.PriceOwnerChannel, OwnerID: channel.ID, OwnerName: "doc-1", PluginKey: key, PluginName: "Test", Reason: model.TaskPluginPricingUnconfigured}}
	stale := slices.Clone(unpriced)
	stale[0].Reason = model.TaskPluginPricingStale
	statusPath := "/api/plugin/task/" + key + "/status"
	for _, step := range []struct {
		name, method, path, body string
		params                   gin.Params
		handler                  gin.HandlerFunc
		want                     []model.TaskPluginPricingIssue
	}{
		{"first upload serves", http.MethodPost, "/api/plugin/task", upload("1.0.0"), nil, UploadTaskPlugin, unpriced},
		{"later upload waits for activation", http.MethodPost, "/api/plugin/task", upload("2.0.0"), nil, UploadTaskPlugin, nil},
		{"activation", http.MethodPost, "/api/plugin/task/" + key + "/activate", `{"version":"2.0.0"}`, gin.Params{{Key: "key", Value: key}}, ActivateTaskPlugin, stale},
		{"old version detail uses current serving diagnostics", http.MethodGet, "/api/plugin/task/" + key + "?version=1.0.0", "", gin.Params{{Key: "key", Value: key}}, GetTaskPlugin, stale},
		{"switching off", http.MethodPost, statusPath, `{"enabled":false}`, gin.Params{{Key: "key", Value: key}}, SetTaskPluginStatus, stale},
		{"switching on", http.MethodPost, statusPath, `{"enabled":true}`, gin.Params{{Key: "key", Value: key}}, SetTaskPluginStatus, stale},
		{"deleting the active version promotes another", http.MethodDelete, "/api/plugin/task/" + key + "/versions/2.0.0", "", gin.Params{{Key: "key", Value: key}, {Key: "version", Value: "2.0.0"}}, DeleteTaskPluginVersion, unpriced},
	} {
		context, recorder := taskPluginDiagnosticContext(t, step.method, step.path, step.body, step.params, identity, credential)
		step.handler(context)

		var response struct {
			Success bool   `json:"success"`
			Message string `json:"message"`
			Data    struct {
				Diagnostics *model.TaskPluginPricingDiagnostics `json:"pricing_diagnostics"`
			} `json:"data"`
		}
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response), step.name)
		require.True(t, response.Success, "%s: %s", step.name, response.Message)
		if step.name == "later upload waits for activation" {
			assert.Nil(t, response.Data.Diagnostics)
			continue
		}
		require.NotNil(t, response.Data.Diagnostics, step.name)
		assert.Equal(t, model.TaskPluginPricingReady, response.Data.Diagnostics.State, step.name)
		assert.Equal(t, append([]model.TaskPluginPricingIssue{}, step.want...), response.Data.Diagnostics.Issues, step.name)
	}
	active, err := model.GetTaskPluginVersion(key, "")
	require.NoError(t, err)
	assert.Equal(t, "1.0.0", active.Version)
}

// A plugin switched off keeps its saved prices for when it is switched back
// on, whether its override row is stored disabled or its built-in sits in the
// disabled set: prices for models its installed version declares are not
// issues, and none of its models counts as unpriced. Prices turn stale once the
// installed version stops declaring the model or the plugin is gone.
func TestSwitchedOffPluginPricesAreNotPricingIssues(t *testing.T) {
	setupTaskPluginFactoryDisableTest(t)
	identity, credential := taskPluginDiagnosticIdentity(t)
	key := "pricing-off-probe"
	cleanupTaskPluginControllerRuntime(t, key)
	body, err := common.Marshal(map[string]any{"source": taskPluginControllerTestSource(key, "1.0.0")})
	require.NoError(t, err)
	c, upload := taskPluginDiagnosticContext(t, http.MethodPost, "/api/plugin/task", string(body), nil, identity, credential)
	UploadTaskPlugin(c)
	require.Contains(t, upload.Body.String(), `"success":true`)
	var kling jsplugin.Meta
	for _, meta := range jsplugin.DefaultRegistry.Snapshot().Factory {
		if meta.Key == "kling" {
			kling = meta
		}
	}
	require.NotEmpty(t, kling.Models)
	valid := taskPluginDiagnosticChannel(t, key, "DoC-1", `tier("base", 0)`, identity.UserID)
	stale := taskPluginDiagnosticChannel(t, key, "retired-model", `tier("base", 0)`, identity.UserID)
	taskPluginDiagnosticChannel(t, "kling", kling.Models[0], `tier("base", 0)`, identity.UserID)
	staleFactory := taskPluginDiagnosticChannel(t, "kling", "kling-retired-model", `tier("base", 0)`, identity.UserID)
	if len(kling.Models) > 1 {
		taskPluginDiagnosticChannel(t, "kling", kling.Models[1], "", identity.UserID)
	}
	readIssues := func(plugin string) []model.TaskPluginPricingIssue {
		c, _ := taskPluginDiagnosticContext(t, http.MethodGet, "/api/plugin/task/"+plugin, "", nil, identity, credential)
		result := taskPluginPricingDiagnostics(c, plugin)
		require.Equal(t, model.TaskPluginPricingReady, result.State)
		return result.Issues
	}
	require.Len(t, readIssues(key), 1, "the current generation accepts the canonical mixed-case binding")
	if len(kling.Models) > 1 {
		require.True(t, slices.ContainsFunc(readIssues("kling"), func(issue model.TaskPluginPricingIssue) bool {
			return issue.Reason == model.TaskPluginPricingUnconfigured
		}), "only the persisted unpriced factory owner is reported")
	}
	for _, plugin := range []string{key, "kling"} {
		require.Contains(t, postTaskPluginStatus(t, plugin, "", `{"enabled":false}`).Body.String(), `"success":true`)
	}
	row, err := model.GetTaskPluginVersion(key, "")
	require.NoError(t, err)
	require.False(t, row.Enabled)
	require.Equal(t, []string{"kling"}, setting.GetTaskPluginDisabledFactoryKeys())
	_, serving := jsplugin.DefaultRegistry.Get("kling")
	require.False(t, serving)
	for plugin, owner := range map[string]model.Channel{key: stale, "kling": staleFactory} {
		issues := readIssues(plugin)
		require.Len(t, issues, 1)
		assert.Equal(t, owner.ID, issues[0].OwnerID)
		assert.Equal(t, model.TaskPluginPricingStale, issues[0].Reason)
		require.NotNil(t, issues[0].PriceVersionID)
	}
	c, remove := taskPluginDiagnosticContext(t, http.MethodDelete, "/api/plugin/task/"+key+"/versions/1.0.0", "", gin.Params{{Key: "key", Value: key}, {Key: "version", Value: "1.0.0"}}, identity, credential)
	DeleteTaskPluginVersion(c)
	require.Contains(t, remove.Body.String(), `"success":true`)
	issues := readIssues(key)
	require.Len(t, issues, 2)
	assert.Equal(t, []int{valid.ID, stale.ID}, []int{issues[0].OwnerID, issues[1].OwnerID})
	for _, issue := range issues {
		assert.Equal(t, model.TaskPluginPricingStale, issue.Reason)
	}
	var count int64
	require.NoError(t, model.DB.Model(&model.PriceVersion{}).Count(&count).Error)
	assert.EqualValues(t, 4, count, "disabling/deleting plugins never rewrites immutable prices")
}

func TestTaskPluginPricingDiagnosticsPermissionsAndReadFailure(t *testing.T) {
	setupTaskPluginControllerTest(t)
	identity, credential := taskPluginDiagnosticIdentity(t)
	require.NoError(t, i18n.Init())
	require.NoError(t, model.DB.AutoMigrate(&model.Log{}, &model.AuditLog{}))
	require.NoError(t, model.EnsureLegacyAccessTokenRetireAt(time.Now().Unix()))
	key := "private-owner-diagnostic"
	cleanupTaskPluginControllerRuntime(t, key)
	channel := taskPluginDiagnosticChannel(t, key, "doc-1", "", identity.UserID)
	require.NoError(t, model.DB.Model(&channel).Update("upstream_model_name", "private-vendor-endpoint").Error)
	offering := model.ModelOffering{Name: "private-sale-owner", Visibility: "all", Groups: []string{}, RouteMethod: "cost", AcceptedConversionLosses: []string{}, ChannelScope: model.OfferingChannelScope{}, Members: []model.ModelOfferingMember{{ModelID: channel.ModelID, Tier: 1}}}
	require.NoError(t, model.DB.Omit("Members.Model").Create(&offering).Error)
	var afterAuthentication func()
	router := gin.New()
	router.Use(middleware.RequestId(), middleware.AccessTokenAudit())
	routes := router.Group("/api/plugin/task", middleware.RootAuth(), func(c *gin.Context) {
		if afterAuthentication != nil {
			action := afterAuthentication
			afterAuthentication = nil
			action()
		}
		c.Next()
	})
	routes.POST("", UploadTaskPlugin)
	routes.GET("/:key", GetTaskPlugin)
	routes.POST("/:key/status", SetTaskPluginStatus)
	payload, err := common.Marshal(map[string]any{"source": taskPluginControllerTestSource(key, "1.0.0")})
	require.NoError(t, err)
	response := accessTokenRequest(router, http.MethodPost, "/api/plugin/task", credential, "", string(payload))
	require.Equal(t, http.StatusOK, response.Code)
	decode := func(response *httptest.ResponseRecorder) model.TaskPluginPricingDiagnostics {
		var envelope struct {
			Success bool `json:"success"`
			Data    struct {
				Diagnostics model.TaskPluginPricingDiagnostics `json:"pricing_diagnostics"`
			} `json:"data"`
		}
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &envelope))
		require.True(t, envelope.Success, response.Body.String())
		require.NotNil(t, envelope.Data.Diagnostics.Issues)
		return envelope.Data.Diagnostics
	}
	require.Len(t, decode(response).Issues, 2)
	for _, tc := range []struct {
		name   string
		scopes []string
		owner  string
	}{
		{"plugin-only", []string{"plugin:read"}, ""},
		{"pricing-without-owner", []string{"plugin:read", "pricing:read"}, ""},
		{"owner-without-pricing", []string{"plugin:read", "model:read", "channel:read"}, ""},
		{"sale-only", []string{"plugin:read", "pricing:read", "model:read"}, model.PriceOwnerOffering},
		{"purchase-only", []string{"plugin:read", "pricing:read", "channel:read"}, model.PriceOwnerChannel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := createScopedAccessToken(t, identity.UserID, time.Now().Unix()+3600, tc.scopes...)
			response := accessTokenRequest(router, http.MethodGet, "/api/plugin/task/"+key, raw, "", "")
			require.Equal(t, http.StatusOK, response.Code)
			result := decode(response)
			if tc.owner == "" {
				assert.Equal(t, model.TaskPluginPricingNotAuthorized, result.State)
				assert.Empty(t, result.Issues)
				assert.NotContains(t, response.Body.String(), "private-sale-owner")
				assert.NotContains(t, response.Body.String(), "private-vendor-endpoint")
				return
			}
			require.Equal(t, model.TaskPluginPricingReady, result.State)
			require.Len(t, result.Issues, 1)
			assert.Equal(t, tc.owner, result.Issues[0].OwnerType)
			if tc.owner == model.PriceOwnerOffering {
				assert.NotContains(t, response.Body.String(), "private-vendor-endpoint")
			} else {
				assert.NotContains(t, response.Body.String(), "private-sale-owner")
			}
		})
	}
	raw, _ := createScopedAccessToken(t, identity.UserID, time.Now().Unix()+3600, "plugin:write")
	response = accessTokenRequest(router, http.MethodPost, "/api/plugin/task/"+key+"/status", raw, "", `{"enabled":true}`)
	require.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, model.TaskPluginPricingNotAuthorized, decode(response).State, "mutation grant alone reveals no private owner")
	assert.NotContains(t, response.Body.String(), "private-sale-owner")
	raw, token := createScopedAccessToken(t, identity.UserID, time.Now().Unix()+3600, "plugin:read", "pricing:read", "channel:read")
	afterAuthentication = func() {
		require.NoError(t, token.SetScopes([]string{"plugin:read"}))
		require.NoError(t, model.DB.Model(token).Update("scopes", token.Scopes).Error)
	}
	response = accessTokenRequest(router, http.MethodGet, "/api/plugin/task/"+key, raw, "", "")
	require.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, model.TaskPluginPricingNotAuthorized, decode(response).State, "current PAT scopes, not middleware's earlier snapshot")

	failureKey := "diagnostic-read-failure"
	cleanupTaskPluginControllerRuntime(t, failureKey)
	taskPluginDiagnosticChannel(t, failureKey, "doc-1", "", identity.UserID)
	require.NoError(t, model.DB.Exec(`ALTER TABLE "price_versions" RENAME TO "diagnostic_private_failure"`).Error)
	payload, err = common.Marshal(map[string]any{"source": taskPluginControllerTestSource(failureKey, "1.0.0")})
	require.NoError(t, err)
	response = accessTokenRequest(router, http.MethodPost, "/api/plugin/task", credential, "", string(payload))
	require.Equal(t, http.StatusOK, response.Code)
	result := decode(response)
	assert.Equal(t, model.TaskPluginPricingUnavailable, result.State)
	assert.Empty(t, result.Issues)
	assert.NotContains(t, response.Body.String(), "diagnostic_private_failure")
	row, err := model.GetTaskPluginVersion(failureKey, "")
	require.NoError(t, err)
	assert.True(t, row.Active, "diagnostic SQL failure cannot roll back the saved mutation")
	require.NoError(t, model.DB.Exec(`ALTER TABLE "diagnostic_private_failure" RENAME TO "price_versions"`).Error)
	response = accessTokenRequest(router, http.MethodGet, "/api/plugin/task/"+failureKey, credential, "", "")
	assert.Equal(t, model.TaskPluginPricingReady, decode(response).State, "retry is a detail read, not another mutation")
	var rows int64
	require.NoError(t, model.DB.Model(&model.TaskPlugin{}).Where("key=?", failureKey).Count(&rows).Error)
	assert.EqualValues(t, 1, rows)

	// Recheck current role and policy even if middleware already authenticated
	// a root session. This ordinary transaction legitimately locks its credential.
	require.NoError(t, model.DB.Model(&model.User{}).Where("id=?", identity.UserID).Update("role", common.RoleAdminUser).Error)
	for _, resource := range []string{"pricing", "model"} {
		require.NoError(t, model.DB.Create(&model.CasbinRule{Ptype: "p", V0: authz.UserSubject(identity.UserID), V1: resource, V2: "read", V3: authz.EffectAllow}).Error)
	}
	c, _ := taskPluginDiagnosticContext(t, http.MethodGet, "/api/plugin/task/"+key, "", nil, identity, credential)
	result = *taskPluginPricingDiagnostics(c, key)
	require.Equal(t, model.TaskPluginPricingReady, result.State)
	require.Len(t, result.Issues, 1)
	assert.Equal(t, model.PriceOwnerOffering, result.Issues[0].OwnerType)
	require.NoError(t, model.DB.Model(&model.CasbinRule{}).Where("v0=? AND v1=?", authz.UserSubject(identity.UserID), "pricing").Update("v3", authz.EffectDeny).Error)
	assert.Equal(t, model.TaskPluginPricingNotAuthorized, taskPluginPricingDiagnostics(c, key).State, "permission revocation is authoritative without cache refresh")
	require.NoError(t, model.DB.Model(&model.User{}).Where("id=?", identity.UserID).Update("role", common.RoleRootUser).Error)
	require.NoError(t, model.DB.Model(&model.UserSession{}).Where("sid=?", identity.SessionID).Update("version", 2).Error)
	assert.Equal(t, model.TaskPluginPricingNotAuthorized, taskPluginPricingDiagnostics(c, key).State, "revoked session version cannot reveal prices")
}

func TestSyncTaskPluginsPublishesOneGenerationForWholeBatch(t *testing.T) {
	setupTaskPluginControllerTest(t)
	firstKey := "batch-sync-first"
	secondKey := "batch-sync-second"
	cleanupTaskPluginControllerRuntime(t, firstKey)
	cleanupTaskPluginControllerRuntime(t, secondKey)
	firstSource := taskPluginControllerTestSource(firstKey, "1.0.0")
	secondSource := taskPluginControllerTestSource(secondKey, "1.0.0")
	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{
		Key: firstKey, APIVersion: 1, Version: "1.0.0", Source: model.LongText(firstSource), SourceHash: "first-hash", Enabled: true,
	}))
	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{
		Key: secondKey, APIVersion: 1, Version: "1.0.0", Source: model.LongText(secondSource), SourceHash: "second-hash", Enabled: true,
	}))
	before := jsplugin.DefaultRegistry.Generation().Number

	SyncTaskPluginsOnce()

	assert.Equal(t, before+1, jsplugin.DefaultRegistry.Generation().Number)
	_, firstRegistered := jsplugin.DefaultRegistry.Get(firstKey)
	_, secondRegistered := jsplugin.DefaultRegistry.Get(secondKey)
	assert.True(t, firstRegistered)
	assert.True(t, secondRegistered)

	published := jsplugin.DefaultRegistry.Generation()
	SyncTaskPluginsOnce()
	assert.Same(t, published, jsplugin.DefaultRegistry.Generation())
}

func TestTaskPluginRuntimeExposesDatabaseRevisionAheadOfLocalGeneration(t *testing.T) {
	setupTaskPluginControllerTest(t)
	key := "runtime-revision-probe"
	cleanupTaskPluginControllerRuntime(t, key)
	taskPluginSyncState.Lock()
	previousRebuild := taskPluginSyncState.lastRebuild
	taskPluginSyncState.Unlock()
	t.Cleanup(func() {
		taskPluginSyncState.Lock()
		taskPluginSyncState.lastRebuild = previousRebuild
		taskPluginSyncState.Unlock()
	})

	v1Source := taskPluginControllerTestSource(key, "1.0.0")
	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{
		Key: key, APIVersion: 1, Version: "1.0.0",
		Source: model.LongText(v1Source), SourceHash: "runtime-v1", Enabled: true,
	}))
	require.NoError(t, syncTaskPluginsOnce())
	localGeneration := jsplugin.DefaultRegistry.Generation().Number
	taskPluginSyncState.Lock()
	syncedRevision := taskPluginSyncState.lastRebuild.DatabaseRevision
	taskPluginSyncState.Unlock()

	v2Source := taskPluginControllerTestSource(key, "2.0.0")
	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{
		Key: key, APIVersion: 1, Version: "2.0.0",
		Source: model.LongText(v2Source), SourceHash: "runtime-v2", Enabled: true,
	}))
	require.NoError(t, model.ActivateTaskPlugin(key, "2.0.0"))

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/plugin/task/runtime/status", nil)
	GetTaskPluginRuntime(context)

	var response struct {
		Success bool                    `json:"success"`
		Data    taskPluginRuntimeStatus `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success)
	assert.Equal(t, localGeneration, response.Data.CurrentGeneration)
	assert.NotZero(t, response.Data.GenerationPublishedAt)
	assert.NotEqual(t, syncedRevision, response.Data.DatabaseRevision)
	assert.Equal(t, "success", response.Data.LastRebuild.Status)
	assert.Equal(t, syncedRevision, response.Data.LastRebuild.DatabaseRevision)
	assert.Empty(t, response.Data.PluginErrors)

	active, ok := jsplugin.DefaultRegistry.Get(key)
	require.True(t, ok)
	assert.Equal(t, "1.0.0", active.Meta.Version)
}

func TestTaskPluginRuntimeReportsPluginLevelCompileErrors(t *testing.T) {
	setupTaskPluginControllerTest(t)
	key := "runtime-error-probe"
	cleanupTaskPluginControllerRuntime(t, key)
	taskPluginSyncState.Lock()
	previousRebuild := taskPluginSyncState.lastRebuild
	taskPluginSyncState.Unlock()
	t.Cleanup(func() {
		taskPluginSyncState.Lock()
		taskPluginSyncState.lastRebuild = previousRebuild
		taskPluginSyncState.Unlock()
	})
	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{
		Key: key, APIVersion: 1, Version: "1.0.0",
		Source: "export const meta = {", SourceHash: "broken-source", Enabled: true,
	}))
	require.NoError(t, syncTaskPluginsOnce())

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/plugin/task/runtime/status", nil)
	GetTaskPluginRuntime(context)

	var response struct {
		Success bool                    `json:"success"`
		Data    taskPluginRuntimeStatus `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success)
	assert.Equal(t, "partial", response.Data.LastRebuild.Status)
	assert.Equal(t, response.Data.DatabaseRevision, response.Data.LastRebuild.DatabaseRevision)
	assert.GreaterOrEqual(t, response.Data.LastRebuild.PluginErrorCount, 1)
	assert.NotEmpty(t, response.Data.PluginErrors[key])
	_, registered := jsplugin.DefaultRegistry.Get(key)
	assert.False(t, registered)
}

func TestTaskPluginRuntimeSurvivesDatabaseSyncFailure(t *testing.T) {
	setupTaskPluginControllerTest(t)
	key := "runtime-database-failure"
	cleanupTaskPluginControllerRuntime(t, key)
	taskPluginSyncState.Lock()
	previousRebuild := taskPluginSyncState.lastRebuild
	taskPluginSyncState.Unlock()
	t.Cleanup(func() {
		taskPluginSyncState.Lock()
		taskPluginSyncState.lastRebuild = previousRebuild
		taskPluginSyncState.Unlock()
	})

	source := taskPluginControllerTestSource(key, "1.0.0")
	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{
		Key: key, APIVersion: 1, Version: "1.0.0",
		Source: model.LongText(source), SourceHash: "runtime-database-v1", Enabled: true,
	}))
	require.NoError(t, syncTaskPluginsOnce())
	generation := jsplugin.DefaultRegistry.Generation().Number
	taskPluginSyncState.Lock()
	syncedRevision := taskPluginSyncState.lastRebuild.DatabaseRevision
	taskPluginSyncState.Unlock()

	sqlDatabase, err := model.DB.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDatabase.Close())
	require.Error(t, syncTaskPluginsOnce())

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/plugin/task/runtime/status", nil)
	GetTaskPluginRuntime(context)

	var response struct {
		Success bool                    `json:"success"`
		Data    taskPluginRuntimeStatus `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success)
	assert.Equal(t, generation, response.Data.CurrentGeneration)
	assert.Equal(t, syncedRevision, response.Data.DatabaseRevision)
	assert.Equal(t, "database snapshot unavailable", response.Data.DatabaseError)
	assert.Equal(t, "failed", response.Data.LastRebuild.Status)
	assert.Equal(t, syncedRevision, response.Data.LastRebuild.DatabaseRevision)
	assert.Contains(t, response.Data.LastRebuild.Error, "sync task plugins")
}

func TestSyncTaskPluginsCachesRejectedDesiredSourceWithoutLosingIncumbent(t *testing.T) {
	setupTaskPluginControllerTest(t)
	pluginKey := "sync-retained-plugin"
	ownerKey := "sync-retained-owner"
	cleanupTaskPluginControllerRuntime(t, pluginKey)
	cleanupTaskPluginControllerRuntime(t, ownerKey)
	v1Source := taskPluginControllerChannelSource(pluginKey, "1.0.0", 9001)
	v2Source := taskPluginControllerChannelSource(pluginKey, "2.0.0", 9002)
	ownerSource := taskPluginControllerChannelSource(ownerKey, "1.0.0", 9002)
	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{
		Key: pluginKey, APIVersion: 1, Version: "1.0.0", Source: model.LongText(v1Source), SourceHash: "retained-v1", Enabled: true,
	}))
	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{
		Key: ownerKey, APIVersion: 1, Version: "1.0.0", Source: model.LongText(ownerSource), SourceHash: "owner-v1", Enabled: true,
	}))
	require.NoError(t, syncTaskPluginsOnce())

	incumbent, ok := jsplugin.DefaultRegistry.Get(pluginKey)
	require.True(t, ok)
	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{
		Key: pluginKey, APIVersion: 1, Version: "2.0.0", Source: model.LongText(v2Source), SourceHash: "retained-v2", Enabled: true,
	}))
	require.NoError(t, model.ActivateTaskPlugin(pluginKey, "2.0.0"))

	require.NoError(t, syncTaskPluginsOnce())
	rejectedGeneration := jsplugin.DefaultRegistry.Generation()
	for range 2 {
		require.NoError(t, syncTaskPluginsOnce())
		active, found := jsplugin.DefaultRegistry.Get(pluginKey)
		require.True(t, found)
		assert.Same(t, incumbent, active)
		assert.Equal(t, "2.0.0", jsplugin.DefaultRegistry.OverridePlugins()[pluginKey].Meta.Version)
		assert.Same(t, incumbent, jsplugin.DefaultRegistry.ActiveOverridePlugins()[pluginKey])
		assert.Contains(t, jsplugin.DefaultRegistry.RoutingErrors()[pluginKey], "channelType 9002 conflicts")
		taskPluginSyncState.Lock()
		assert.Equal(t, "retained-v2", taskPluginSyncState.hashes[pluginKey])
		taskPluginSyncState.Unlock()
		assert.Same(t, rejectedGeneration, jsplugin.DefaultRegistry.Generation())
	}

	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{
		Key: ownerKey, APIVersion: 1, Version: "2.0.0",
		Source: model.LongText(taskPluginControllerChannelSource(ownerKey, "2.0.0", 9003)), SourceHash: "owner-v2", Enabled: true,
	}))
	require.NoError(t, model.ActivateTaskPlugin(ownerKey, "2.0.0"))
	require.NoError(t, syncTaskPluginsOnce())
	active, ok := jsplugin.DefaultRegistry.Get(pluginKey)
	require.True(t, ok)
	assert.Equal(t, "2.0.0", active.Meta.Version)
	assert.NotContains(t, jsplugin.DefaultRegistry.RoutingErrors(), pluginKey)
}

const dryRunPluginSource = `
export const meta = {apiVersion: 1, key: "dryrun-probe", name: "DryRun", version: "1.0.0", author: {name: "Test"}, models: ["doc-1"], fetchMode: "per_task"};
export function buildSubmitRequest(payload) {
	if (!payload || !payload.model) { throw new Error("model required"); }
	return {model: payload.model};
}
export function parseSubmitResponse() { return {}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {}; }
export const native = { info: function(ctx, task) { return "task:" + task.id; } };
`

func runTaskPluginDryRun(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Params = gin.Params{{Key: "key", Value: "dryrun-probe"}}
	context.Request = httptest.NewRequest(http.MethodPost, "/api/plugin/task/dryrun-probe/dryrun", strings.NewReader(body))
	context.Request.Header.Set("Content-Type", "application/json")
	DryRunTaskPlugin(context)
	return recorder
}

func TestDryRunTaskPluginExecutesHookAndRendererMember(t *testing.T) {
	setupTaskPluginControllerTest(t)
	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{Key: "dryrun-probe", APIVersion: 1, Version: "1.0.0", Source: dryRunPluginSource, SourceHash: "hash", Enabled: true}))

	hookRecorder := runTaskPluginDryRun(t, `{"hook":"buildSubmitRequest","args":[{"model":"doc-1"}]}`)
	assert.Contains(t, hookRecorder.Body.String(), `"success":true`)
	assert.Contains(t, hookRecorder.Body.String(), `"model":"doc-1"`)

	memberRecorder := runTaskPluginDryRun(t, `{"hook":"native","member":"info","args":[{}, {"id":"t-1"}]}`)
	assert.Contains(t, memberRecorder.Body.String(), `"success":true`)
	assert.Contains(t, memberRecorder.Body.String(), "task:t-1")
}

func TestDryRunTaskPluginReportsUnknownHook(t *testing.T) {
	setupTaskPluginControllerTest(t)
	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{Key: "dryrun-probe", APIVersion: 1, Version: "1.0.0", Source: dryRunPluginSource, SourceHash: "hash", Enabled: true}))

	recorder := runTaskPluginDryRun(t, `{"hook":"missingHook"}`)

	assert.Contains(t, recorder.Body.String(), `"success":false`)
	assert.Contains(t, recorder.Body.String(), `plugin export \"missingHook\" not found`)
}

func TestDryRunTaskPluginSurfacesBadArgumentErrors(t *testing.T) {
	setupTaskPluginControllerTest(t)
	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{Key: "dryrun-probe", APIVersion: 1, Version: "1.0.0", Source: dryRunPluginSource, SourceHash: "hash", Enabled: true}))

	malformedRecorder := runTaskPluginDryRun(t, `{"hook":"buildSubmitRequest","args":[{`)
	assert.Contains(t, malformedRecorder.Body.String(), `"success":false`)

	rejectedRecorder := runTaskPluginDryRun(t, `{"hook":"buildSubmitRequest","args":[{}]}`)
	assert.Contains(t, rejectedRecorder.Body.String(), `"success":false`)
	assert.Contains(t, rejectedRecorder.Body.String(), "model required")
}

func TestUploadTaskPluginPreflightConflict(t *testing.T) {
	setupTaskPluginControllerTest(t)
	enabledFalse := false
	tests := []struct {
		name        string
		key         string
		enabled     *bool
		force       bool
		wantSuccess bool
		wantError   string
	}{
		{
			name:      "enabled conflict rejected",
			key:       "preflight-reject",
			wantError: "channelType 50 conflicts",
		},
		{
			name:      "force does not bypass a conflict",
			key:       "preflight-force",
			force:     true,
			wantError: "channelType 50 conflicts",
		},
		{
			name:        "disabled skips preflight",
			key:         "preflight-disabled",
			enabled:     &enabledFalse,
			wantSuccess: true,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			cleanupTaskPluginControllerRuntime(t, testCase.key)
			source := taskPluginControllerChannelSource(testCase.key, "1.0.0", 50)
			payload := map[string]any{"source": source}
			if testCase.enabled != nil {
				payload["enabled"] = *testCase.enabled
			}
			if testCase.force {
				payload["force"] = true
			}
			body, err := common.Marshal(payload)
			require.NoError(t, err)
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			context.Request = httptest.NewRequest(http.MethodPost, "/api/plugin/task", strings.NewReader(string(body)))
			context.Request.Header.Set("Content-Type", "application/json")

			UploadTaskPlugin(context)

			if testCase.wantSuccess {
				assert.Contains(t, recorder.Body.String(), `"success":true`)
				stored, err := model.GetTaskPluginVersion(testCase.key, "")
				require.NoError(t, err)
				assert.False(t, stored.Enabled)
				return
			}
			assert.Contains(t, recorder.Body.String(), `"success":false`)
			assert.Contains(t, recorder.Body.String(), testCase.wantError)
			assert.Contains(t, recorder.Body.String(), "kling")
			var count int64
			require.NoError(t, model.DB.Model(&model.TaskPlugin{}).Where("key = ?", testCase.key).Count(&count).Error)
			assert.Zero(t, count)
		})
	}
}

func taskPluginControllerRouteSource(key, version, routePath string) string {
	return fmt.Sprintf(`
export const meta = {apiVersion: 1, key: %q, name: "Test", version: %q, author: {name: "Test"}, models: ["doc-1"], fetchMode: "per_task",
	routes: [{method: "POST", path: %q, type: "submit", decode: "createJob", render: "jobCreated"}]};
export const native = {
	createJob(ctx) { return {kind: "submit", model: ctx.body.value.model, requestBody: ctx.body.value}; },
	jobCreated(ctx, task) { return {id: task.task_id}; },
};
export function buildSubmitRequest() { return {}; }
export function parseSubmitResponse() { return {}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {}; }
`, key, version, routePath)
}

func postTaskPluginUpload(t *testing.T, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := common.Marshal(payload)
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, "/api/plugin/task", strings.NewReader(string(body)))
	context.Request.Header.Set("Content-Type", "application/json")
	UploadTaskPlugin(context)
	return recorder
}

func deleteTaskPluginVersionRequest(t *testing.T, key, version string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Params = gin.Params{{Key: "key", Value: key}, {Key: "version", Value: version}}
	context.Request = httptest.NewRequest(http.MethodDelete, "/api/plugin/task/"+key+"/versions/"+version, nil)
	DeleteTaskPluginVersion(context)
	return recorder
}

// The plugin list and detail name the declared routes the gateway leaves to
// the /{key} address, for the version that serves.
func TestTaskPluginListAndDetailReportSkippedRoutes(t *testing.T) {
	setupTaskPluginControllerTest(t)
	cleanupTaskPluginControllerRuntime(t, "skipped-probe")
	// Stands in for the router, which skips a third-party plugin's routes
	// under the host's namespaces.
	require.NoError(t, jsplugin.DefaultRegistry.SetGenerationPreparer(func(candidate, _ *jsplugin.RoutingGeneration) (jsplugin.PreparedRoutingGeneration, error) {
		var skipped []jsplugin.SkippedRoute
		if plugin, ok := candidate.Get("skipped-probe"); ok {
			skipped = append(skipped, jsplugin.SkippedRoute{PluginKey: "skipped-probe", Route: plugin.Meta.Routes[0], Reason: "host_namespace"})
		}
		return jsplugin.PreparedRoutingGeneration{Generation: candidate.WithSkippedRoutes(skipped)}, nil
	}))
	t.Cleanup(func() { require.NoError(t, jsplugin.DefaultRegistry.SetGenerationPreparer(nil)) })
	require.Contains(t, postTaskPluginUpload(t, map[string]any{"source": taskPluginControllerRouteSource("skipped-probe", "1.0.0", "/v1/videos/probe")}).Body.String(), `"success":true`)

	assert.Equal(t, []string{"POST /v1/videos/probe"}, listTaskPluginItem(t, "skipped-probe").SkippedRoutes)
	assert.Equal(t, []string{}, listTaskPluginItem(t, "doubao").SkippedRoutes)
	for key, want := range map[string][]string{"skipped-probe": {"POST /v1/videos/probe"}, "doubao": {}} {
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Params = gin.Params{{Key: "key", Value: key}}
		context.Request = httptest.NewRequest(http.MethodGet, "/api/plugin/task/"+key, nil)
		GetTaskPlugin(context)
		var response struct {
			Data taskPluginDetail `json:"data"`
		}
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
		assert.Equal(t, want, response.Data.SkippedRoutes, key)
	}
}

// With two enabled plugins whose routing conflicts, the restart order would
// pick the one that serves. Every path that puts a plugin into service refuses
// a conflict and changes nothing.
func TestTaskPluginSwitchOnPathsRejectRoutingConflicts(t *testing.T) {
	t.Run("switching on a stored plugin", func(t *testing.T) {
		setupTaskPluginControllerTest(t)
		cleanupTaskPluginControllerRuntime(t, "conflict-zeta")
		cleanupTaskPluginControllerRuntime(t, "conflict-alpha")
		require.Contains(t, postTaskPluginUpload(t, map[string]any{"source": taskPluginControllerChannelSource("conflict-zeta", "1.0.0", 990)}).Body.String(), `"success":true`)
		require.Contains(t, postTaskPluginUpload(t, map[string]any{"source": taskPluginControllerChannelSource("conflict-alpha", "1.0.0", 990), "enabled": false}).Body.String(), `"success":true`)

		recorder := postTaskPluginStatus(t, "conflict-alpha", "", `{"enabled":true}`)
		assert.Contains(t, recorder.Body.String(), `"success":false`)
		assert.Contains(t, recorder.Body.String(), "channelType 990 conflicts")
		assert.Contains(t, recorder.Body.String(), "conflict-zeta")
		stored, err := model.GetTaskPluginVersion("conflict-alpha", "")
		require.NoError(t, err)
		assert.False(t, stored.Enabled)

		require.Contains(t, postTaskPluginStatus(t, "conflict-zeta", "", `{"enabled":false}`).Body.String(), `"success":true`)
		assert.Contains(t, postTaskPluginStatus(t, "conflict-alpha", "", `{"enabled":true}`).Body.String(), `"success":true`)
		serving, ok := jsplugin.DefaultRegistry.GetByChannelType(990)
		require.True(t, ok)
		assert.Equal(t, "conflict-alpha", serving.Meta.Key)
	})

	t.Run("switching on a factory built-in", func(t *testing.T) {
		setupTaskPluginFactoryDisableTest(t)
		cleanupTaskPluginControllerRuntime(t, "kling-rival")
		require.Contains(t, postTaskPluginStatus(t, "kling", "", `{"enabled":false}`).Body.String(), `"success":true`)
		// With the built-in off, another plugin may take its channel type.
		require.Contains(t, postTaskPluginUpload(t, map[string]any{"source": taskPluginControllerChannelSource("kling-rival", "1.0.0", 50)}).Body.String(), `"success":true`)

		recorder := postTaskPluginStatus(t, "kling", "", `{"enabled":true}`)
		assert.Contains(t, recorder.Body.String(), `"success":false`)
		assert.Contains(t, recorder.Body.String(), "channelType 50 conflicts")
		assert.Equal(t, []string{"kling"}, setting.GetTaskPluginDisabledFactoryKeys())
		_, ok := jsplugin.DefaultRegistry.Get("kling")
		assert.False(t, ok)

		require.Contains(t, postTaskPluginStatus(t, "kling-rival", "", `{"enabled":false}`).Body.String(), `"success":true`)
		assert.Contains(t, postTaskPluginStatus(t, "kling", "", `{"enabled":true}`).Body.String(), `"success":true`)
		_, ok = jsplugin.DefaultRegistry.Get("kling")
		assert.True(t, ok)
	})

	t.Run("activating a version", func(t *testing.T) {
		setupTaskPluginControllerTest(t)
		cleanupTaskPluginControllerRuntime(t, "activate-conflict")
		cleanupTaskPluginControllerRuntime(t, "activate-rival")
		require.Contains(t, postTaskPluginUpload(t, map[string]any{"source": taskPluginControllerChannelSource("activate-rival", "1.0.0", 991)}).Body.String(), `"success":true`)
		require.Contains(t, postTaskPluginUpload(t, map[string]any{"source": taskPluginControllerTestSource("activate-conflict", "1.0.0")}).Body.String(), `"success":true`)
		require.Contains(t, postTaskPluginUpload(t, map[string]any{"source": taskPluginControllerChannelSource("activate-conflict", "2.0.0", 991), "enabled": false}).Body.String(), `"success":true`)

		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Params = gin.Params{{Key: "key", Value: "activate-conflict"}}
		context.Request = httptest.NewRequest(http.MethodPost, "/api/plugin/task/activate-conflict/activate", strings.NewReader(`{"version":"2.0.0"}`))
		context.Request.Header.Set("Content-Type", "application/json")
		ActivateTaskPlugin(context)

		assert.Contains(t, recorder.Body.String(), `"success":false`)
		assert.Contains(t, recorder.Body.String(), "activate-rival")
		active, err := model.GetTaskPluginVersion("activate-conflict", "")
		require.NoError(t, err)
		assert.Equal(t, "1.0.0", active.Version)
		runtimePlugin, ok := jsplugin.DefaultRegistry.Get("activate-conflict")
		require.True(t, ok)
		assert.Equal(t, "1.0.0", runtimePlugin.Meta.Version)
	})

	t.Run("deleting the active version promotes a conflicting one", func(t *testing.T) {
		setupTaskPluginControllerTest(t)
		cleanupTaskPluginControllerRuntime(t, "delete-conflict")
		cleanupTaskPluginControllerRuntime(t, "delete-rival")
		require.Contains(t, postTaskPluginUpload(t, map[string]any{"source": taskPluginControllerTestSource("delete-conflict", "1.0.0")}).Body.String(), `"success":true`)
		// Stored switched on before the rival existed, so its own check passed.
		require.Contains(t, postTaskPluginUpload(t, map[string]any{"source": taskPluginControllerChannelSource("delete-conflict", "2.0.0", 992)}).Body.String(), `"success":true`)
		require.Contains(t, postTaskPluginUpload(t, map[string]any{"source": taskPluginControllerChannelSource("delete-rival", "1.0.0", 992)}).Body.String(), `"success":true`)

		recorder := deleteTaskPluginVersionRequest(t, "delete-conflict", "1.0.0")
		assert.Contains(t, recorder.Body.String(), `"success":false`)
		assert.Contains(t, recorder.Body.String(), "delete-rival")
		active, err := model.GetTaskPluginVersion("delete-conflict", "")
		require.NoError(t, err)
		assert.Equal(t, "1.0.0", active.Version)

		// Deleting a version that promotes nothing is unaffected.
		assert.Contains(t, deleteTaskPluginVersionRequest(t, "delete-conflict", "2.0.0").Body.String(), `"success":true`)
		versions, err := model.ListTaskPluginVersions("delete-conflict")
		require.NoError(t, err)
		require.Len(t, versions, 1)
		assert.Equal(t, "1.0.0", versions[0].Version)
	})

	t.Run("deleting or switching off the active version hands back a conflicting built-in", func(t *testing.T) {
		setupTaskPluginFactoryDisableTest(t)
		cleanupTaskPluginControllerRuntime(t, "kling")
		cleanupTaskPluginControllerRuntime(t, "kling-rival")
		const overrideVersion = "9.9.9-no-channel-type"
		overrideSource := taskPluginControllerTestSource("kling", overrideVersion)
		require.Contains(t, postTaskPluginUpload(t, map[string]any{"source": overrideSource}).Body.String(), `"success":true`)
		// The override serving kling does not claim channel type 50, so the rival may.
		require.Contains(t, postTaskPluginUpload(t, map[string]any{"source": taskPluginControllerChannelSource("kling-rival", "1.0.0", 50)}).Body.String(), `"success":true`)

		recorder := deleteTaskPluginVersionRequest(t, "kling", overrideVersion)
		assert.Contains(t, recorder.Body.String(), `"success":false`)
		assert.Contains(t, recorder.Body.String(), "channelType 50 conflicts")
		recorder = postTaskPluginUpload(t, map[string]any{"source": overrideSource, "enabled": false})
		assert.Contains(t, recorder.Body.String(), `"success":false`)
		assert.Contains(t, recorder.Body.String(), "channelType 50 conflicts")

		active, err := model.GetTaskPluginVersion("kling", "")
		require.NoError(t, err)
		assert.True(t, active.Enabled)
		runtimePlugin, ok := jsplugin.DefaultRegistry.Get("kling")
		require.True(t, ok)
		assert.Equal(t, overrideVersion, runtimePlugin.Meta.Version)
	})
}

func TestUploadTaskPluginReportsUnknownMetaField(t *testing.T) {
	body, err := common.Marshal(map[string]any{
		"source": `export const meta = { futureField: true };`,
	})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/plugin/task", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Accept-Language", "zh-CN")
	UploadTaskPlugin(c)
	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Success       bool           `json:"success"`
		Message       string         `json:"message"`
		MessageKey    string         `json:"message_key"`
		MessageParams map[string]any `json:"message_params"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.False(t, response.Success)
	assert.Equal(t, `Plugin metadata contains an unknown field "futureField". If this plugin was downloaded from the official marketplace, it may require a newer version of new-api. Try updating new-api and installing the plugin again.`, response.Message)
	assert.Equal(t, `Plugin metadata contains an unknown field "{{field}}". If this plugin was downloaded from the official marketplace, it may require a newer version of new-api. Try updating new-api and installing the plugin again.`, response.MessageKey)
	assert.Equal(t, map[string]any{"field": "futureField"}, response.MessageParams)
}

func TestUploadTaskPluginRejectsMetaViolatingV1Schema(t *testing.T) {
	setupTaskPluginControllerTest(t)
	cases := []struct {
		name          string
		meta          string
		expectedError string
	}{
		{
			name:          "key with uppercase characters",
			meta:          `{apiVersion: 1, key: "Bad-Key", name: "Bad", version: "1.0.0", author: {name: "Test"}, models: ["doc-1"], fetchMode: "per_task"}`,
			expectedError: "plugin meta key must match",
		},
		{
			name:          "version that is not semver",
			meta:          `{apiVersion: 1, key: "bad-plugin", name: "Bad", version: "one", author: {name: "Test"}, models: ["doc-1"], fetchMode: "per_task"}`,
			expectedError: "plugin meta version must be semver",
		},
		{
			name:          "unsupported fetch mode",
			meta:          `{apiVersion: 1, key: "bad-plugin", name: "Bad", version: "1.0.0", author: {name: "Test"}, models: ["doc-1"], fetchMode: "sometimes"}`,
			expectedError: "plugin meta fetchMode must be per_task or batch",
		},
		{
			name:          "empty model list",
			meta:          `{apiVersion: 1, key: "bad-plugin", name: "Bad", version: "1.0.0", author: {name: "Test"}, models: [], fetchMode: "per_task"}`,
			expectedError: "plugin meta models must contain at least one model",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			source := "export const meta = " + testCase.meta + `;
export function buildSubmitRequest() { return {}; }
export function parseSubmitResponse() { return {}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {}; }
`
			body, err := common.Marshal(map[string]any{"source": source})
			require.NoError(t, err)
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			context.Request = httptest.NewRequest(http.MethodPost, "/api/plugin/task", strings.NewReader(string(body)))
			context.Request.Header.Set("Content-Type", "application/json")

			UploadTaskPlugin(context)

			assert.Contains(t, recorder.Body.String(), `"success":false`)
			assert.Contains(t, recorder.Body.String(), testCase.expectedError)
			var count int64
			require.NoError(t, model.DB.Model(&model.TaskPlugin{}).Count(&count).Error)
			assert.Zero(t, count)
		})
	}
}

func TestDeletePureFactoryPluginIsRejected(t *testing.T) {
	for _, query := range []string{"", "?force=true"} {
		t.Run(query, func(t *testing.T) {
			setupTaskPluginControllerTest(t)
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			context.Params = gin.Params{{Key: "key", Value: "kling"}, {Key: "version", Value: "1.0.0"}}
			context.Request = httptest.NewRequest(http.MethodDelete, "/api/plugin/task/kling/versions/1.0.0"+query, nil)

			DeleteTaskPluginVersion(context)

			assert.Contains(t, recorder.Body.String(), `"success":false`)
			assert.Contains(t, recorder.Body.String(), "factory plugins cannot be deleted")
			_, ok := jsplugin.DefaultRegistry.Get("kling")
			assert.True(t, ok)
		})
	}
}

func TestUploadTaskPluginSourceSha256(t *testing.T) {
	setupTaskPluginControllerTest(t)
	tests := []struct {
		name        string
		key         string
		withHash    bool
		hash        string
		wantSuccess bool
		wantError   string
	}{
		{
			name:        "matching hash succeeds",
			key:         "sha256-match",
			withHash:    true,
			wantSuccess: true,
		},
		{
			name:      "mismatching hash rejected",
			key:       "sha256-mismatch",
			withHash:  true,
			hash:      "deadbeef",
			wantError: "plugin source sha256 mismatch",
		},
		{
			name:        "absent field unchanged",
			key:         "sha256-absent",
			wantSuccess: true,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			cleanupTaskPluginControllerRuntime(t, testCase.key)
			source := taskPluginControllerTestSource(testCase.key, "1.0.0")
			payload := map[string]any{"source": source}
			if testCase.withHash {
				hash := testCase.hash
				if hash == "" {
					hash = "  " + strings.ToUpper(fmt.Sprintf("%x", sha256.Sum256([]byte(source)))) + "  "
				}
				payload["sourceSha256"] = hash
			}
			body, err := common.Marshal(payload)
			require.NoError(t, err)
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			context.Request = httptest.NewRequest(http.MethodPost, "/api/plugin/task", strings.NewReader(string(body)))
			context.Request.Header.Set("Content-Type", "application/json")

			UploadTaskPlugin(context)

			if testCase.wantSuccess {
				assert.Contains(t, recorder.Body.String(), `"success":true`)
				_, err = model.GetTaskPluginVersion(testCase.key, "")
				require.NoError(t, err)
				return
			}
			assert.Contains(t, recorder.Body.String(), `"success":false`)
			assert.Contains(t, recorder.Body.String(), testCase.wantError)
			var count int64
			require.NoError(t, model.DB.Model(&model.TaskPlugin{}).Where("key = ?", testCase.key).Count(&count).Error)
			assert.Zero(t, count)
		})
	}
}

func setupTaskPluginMarketplaceSourcesTest(t *testing.T) {
	t.Helper()
	setupTaskPluginControllerTest(t)
	originalMap := common.OptionMap
	common.OptionMapRWMutex.Lock()
	common.OptionMap = map[string]string{}
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		common.OptionMap = originalMap
		common.OptionMapRWMutex.Unlock()
	})
}

func TestGetTaskPluginMarketplaceSourcesDefaultWhenUnset(t *testing.T) {
	setupTaskPluginMarketplaceSourcesTest(t)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/plugin/task/marketplace/sources", nil)

	GetTaskPluginMarketplaceSources(context)

	var response struct {
		Success bool                                  `json:"success"`
		Data    []setting.TaskPluginMarketplaceSource `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success)
	require.Equal(t, []setting.TaskPluginMarketplaceSource{
		{Name: "Official", IndexURL: "https://www.newapi.ai/api/v1/plugins/index.json"},
		{Name: "GitHub", IndexURL: "https://raw.githubusercontent.com/QuantumNous/new-api-plugins/main/index.json"},
	}, response.Data)
	var count int64
	require.NoError(t, model.DB.Model(&model.Option{}).Where("key = ?", setting.TaskPluginMarketplaceSourcesKey).Count(&count).Error)
	assert.Zero(t, count)
}

func TestUpdateTaskPluginMarketplaceSourcesRoundTrip(t *testing.T) {
	setupTaskPluginMarketplaceSourcesTest(t)
	payload := []setting.TaskPluginMarketplaceSource{
		{Name: "Mirror", IndexURL: "https://example.com/plugins/index.json"},
		{Name: "Official", IndexURL: "https://www.newapi.ai/api/v1/plugins/index.json"},
	}
	body, err := common.Marshal(payload)
	require.NoError(t, err)
	putRecorder := httptest.NewRecorder()
	putContext, _ := gin.CreateTestContext(putRecorder)
	putContext.Request = httptest.NewRequest(http.MethodPut, "/api/plugin/task/marketplace/sources", strings.NewReader(string(body)))
	putContext.Request.Header.Set("Content-Type", "application/json")

	UpdateTaskPluginMarketplaceSources(putContext)

	assert.Contains(t, putRecorder.Body.String(), `"success":true`)

	getRecorder := httptest.NewRecorder()
	getContext, _ := gin.CreateTestContext(getRecorder)
	getContext.Request = httptest.NewRequest(http.MethodGet, "/api/plugin/task/marketplace/sources", nil)
	GetTaskPluginMarketplaceSources(getContext)

	var response struct {
		Success bool                                  `json:"success"`
		Data    []setting.TaskPluginMarketplaceSource `json:"data"`
	}
	require.NoError(t, common.Unmarshal(getRecorder.Body.Bytes(), &response))
	require.True(t, response.Success)
	assert.Equal(t, payload, response.Data)
}

func TestUpdateTaskPluginMarketplaceSourcesValidation(t *testing.T) {
	setupTaskPluginMarketplaceSourcesTest(t)
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "empty name",
			body:    `[{"name":"","index_url":"https://example.com/index.json"}]`,
			wantErr: "marketplace source name is required",
		},
		{
			name:    "invalid URL",
			body:    `[{"name":"Local","index_url":"not-a-url"}]`,
			wantErr: "marketplace source index_url must be an absolute http(s) URL",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			context.Request = httptest.NewRequest(http.MethodPut, "/api/plugin/task/marketplace/sources", strings.NewReader(testCase.body))
			context.Request.Header.Set("Content-Type", "application/json")

			UpdateTaskPluginMarketplaceSources(context)

			assert.Contains(t, recorder.Body.String(), `"success":false`)
			assert.Contains(t, recorder.Body.String(), testCase.wantErr)
			assert.Empty(t, common.OptionMap[setting.TaskPluginMarketplaceSourcesKey])
			var count int64
			require.NoError(t, model.DB.Model(&model.Option{}).Where("key = ?", setting.TaskPluginMarketplaceSourcesKey).Count(&count).Error)
			assert.Zero(t, count)
		})
	}
}

func TestTaskPluginDisplayOrderAndMetadata(t *testing.T) {
	setupTaskPluginControllerTest(t)
	const website = "https://example.com/plugins"
	keys := []string{"display-low", "display-zero", "display-beta", "display-alpha"}
	priorities := []int{-10, 0, 50, 50}
	for i, key := range keys {
		source := strings.Replace(taskPluginControllerTestSource(key, "1.0.0"), "apiVersion: 1,", fmt.Sprintf("apiVersion: 1, sortPriority: %d, website: %q,", priorities[i], website), 1)
		_, err := jsplugin.DefaultRegistry.Register(source, jsplugin.Options{})
		require.NoError(t, err)
		cleanupTaskPluginControllerRuntime(t, key)
		require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{Key: key, Version: "1.0.0", APIVersion: 1, Source: model.LongText(source), Enabled: true}))
	}
	for _, endpoint := range []string{"list", "options"} {
		t.Run(endpoint, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			context.Request = httptest.NewRequest(http.MethodGet, "/", nil)
			var metas []jsplugin.Meta
			if endpoint == "list" {
				ListTaskPlugins(context)
				var response struct {
					Success bool
					Data    []taskPluginListItem
				}
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
				require.True(t, response.Success)
				for _, item := range response.Data {
					metas = append(metas, item.Meta)
				}
			} else {
				GetTaskPluginOptions(context)
				var response struct {
					Success bool
					Data    []jsplugin.Meta
				}
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
				require.True(t, response.Success)
				metas = response.Data
			}
			var ordered []string
			for _, meta := range metas {
				if strings.HasPrefix(meta.Key, "display-") {
					ordered = append(ordered, meta.Key)
					assert.Equal(t, website, meta.Website)
				}
			}
			assert.Equal(t, []string{"display-alpha", "display-beta", "display-zero", "display-low"}, ordered)
		})
	}
}

func TestUploadTaskPluginStoresSidecarIconAndServesIt(t *testing.T) {
	setupTaskPluginControllerTest(t)
	const key = "icon-sidecar"
	source := taskPluginControllerTestSource(key, "1.0.0")
	t.Cleanup(func() { jsplugin.DefaultRegistry.Unregister(key) })
	svgIcon := "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 8 8"><circle cx="4" cy="4" r="4"/></svg>`))
	upload := func(icon string) *httptest.ResponseRecorder {
		body, err := common.Marshal(map[string]any{"source": source, "icon": icon})
		require.NoError(t, err)
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Request = httptest.NewRequest(http.MethodPost, "/api/plugin/task", bytes.NewReader(body))
		context.Request.Header.Set("Content-Type", "application/json")
		UploadTaskPlugin(context)
		return recorder
	}

	rejected := upload("data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>1</script></svg>`)))
	assert.Contains(t, rejected.Body.String(), `"success":false`)
	assert.Contains(t, rejected.Body.String(), "script")
	_, err := model.GetTaskPluginVersion(key, "1.0.0")
	require.ErrorIs(t, err, gorm.ErrRecordNotFound, "a rejected icon must not store the plugin")

	accepted := upload(svgIcon)
	require.Contains(t, accepted.Body.String(), `"success":true`)
	assert.Contains(t, accepted.Body.String(), `"has_icon":true`)
	assert.NotContains(t, accepted.Body.String(), "base64,", "icon bytes never travel inside detail JSON")
	stored, err := model.GetTaskPluginVersion(key, "1.0.0")
	require.NoError(t, err)
	assert.Equal(t, svgIcon, string(stored.Icon))

	item := listTaskPluginItem(t, key)
	assert.True(t, item.HasIcon)

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Params = gin.Params{{Key: "key", Value: key}}
	context.Request = httptest.NewRequest(http.MethodGet, "/api/plugin/task/"+key+"/icon", nil)
	GetTaskPluginIcon(context)
	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "image/svg+xml", recorder.Header().Get("Content-Type"))
	assert.Equal(t, "nosniff", recorder.Header().Get("X-Content-Type-Options"))
	assert.Contains(t, recorder.Body.String(), "<circle")

	missing := httptest.NewRecorder()
	context, _ = gin.CreateTestContext(missing)
	context.Params = gin.Params{{Key: "key", Value: "no-such-plugin"}}
	context.Request = httptest.NewRequest(http.MethodGet, "/api/plugin/task/no-such-plugin/icon", nil)
	GetTaskPluginIcon(context)
	assert.Equal(t, http.StatusNotFound, missing.Code)
}
