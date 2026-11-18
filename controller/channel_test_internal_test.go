package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetChannelDefaultBaseURLsUsesBuiltInDefaults(t *testing.T) {
	originalBaseURLs := constant.ChannelBaseURLs
	constant.ChannelBaseURLs = append([]string(nil), originalBaseURLs...)
	constant.ChannelBaseURLs[constant.ChannelTypeDeepSeek] = "https://deepseek.server.example"
	t.Cleanup(func() {
		constant.ChannelBaseURLs = originalBaseURLs
	})

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/channel/default_base_urls", nil)
	GetChannelDefaultBaseURLs(c)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Success bool           `json:"success"`
		Data    map[int]string `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success)
	assert.Equal(t, "https://deepseek.server.example", response.Data[constant.ChannelTypeDeepSeek])
	assert.Equal(t, "https://api.openai.com", response.Data[constant.ChannelTypeOpenAI])
	assert.NotContains(t, response.Data, constant.ChannelTypeAzure)
	assert.NotContains(t, response.Data, constant.ChannelTypeNewAPI)
	assert.NotContains(t, response.Data, constant.ChannelTypeTaskPlugin)
}

func TestValidateChannelProxy(t *testing.T) {
	tests := []struct {
		name    string
		proxy   string
		wantErr bool
	}{
		{name: "empty"},
		{name: "http", proxy: "http://proxy.example:8080"},
		{name: "https", proxy: "https://proxy.example:8443"},
		{name: "socks5", proxy: "socks5://proxy.example"},
		{name: "socks5h", proxy: "socks5h://proxy.example:1080/"},
		{name: "unsupported", proxy: "ftp://proxy.example", wantErr: true},
		{name: "path", proxy: "socks5://proxy.example:1080/path", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setting, err := common.Marshal(dto.ProviderSettings{Proxy: test.proxy})
			require.NoError(t, err)
			channel := &model.Provider{
				Type:    constant.ChannelTypeOpenAI,
				Setting: common.GetPointer(string(setting)),
			}

			err = validateChannel(channel, false)

			if test.wantErr {
				require.ErrorContains(t, err, "invalid channel proxy")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestValidateChannelRequiresNewAPIBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL *string
		wantErr bool
	}{
		{name: "missing", wantErr: true},
		{name: "blank", baseURL: common.GetPointer("  "), wantErr: true},
		{name: "configured", baseURL: common.GetPointer("https://new-api.example")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			channel := &model.Provider{
				Type:    constant.ChannelTypeNewAPI,
				BaseURL: test.baseURL,
			}

			err := validateChannel(channel, false)

			if test.wantErr {
				require.ErrorContains(t, err, "New API channel base URL cannot be empty")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestNewAPIChannelRegistration(t *testing.T) {
	apiType, ok := common.ChannelType2APIType(constant.ChannelTypeNewAPI)

	require.True(t, ok)
	assert.Equal(t, constant.APITypeNewAPI, apiType)
	assert.Equal(t, "New API", constant.GetChannelTypeName(constant.ChannelTypeNewAPI))
	require.Greater(t, len(constant.ChannelBaseURLs), constant.ChannelTypeNewAPI)
	assert.Empty(t, constant.ChannelBaseURLs[constant.ChannelTypeNewAPI])
}

func TestResponsesCompactChannelSupport(t *testing.T) {
	tests := []struct {
		name        string
		channelType int
		apiType     int
		want        bool
	}{
		{name: "OpenAI", channelType: constant.ChannelTypeOpenAI, apiType: constant.APITypeOpenAI, want: true},
		{name: "Azure", channelType: constant.ChannelTypeAzure, apiType: constant.APITypeOpenAI, want: true},
		{name: "Codex", channelType: constant.ChannelTypeCodex, apiType: constant.APITypeCodex, want: true},
		{name: "Advanced Custom", channelType: constant.ChannelTypeAdvancedCustom, apiType: constant.APITypeAdvancedCustom, want: true},
		{name: "Sub2API", channelType: constant.ChannelTypeSub2API, apiType: constant.APITypeSub2API, want: true},
		{name: "New API", channelType: constant.ChannelTypeNewAPI, apiType: constant.APITypeNewAPI, want: true},
		{name: "Anthropic", channelType: constant.ChannelTypeAnthropic, apiType: constant.APITypeAnthropic, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, common.SupportsResponsesCompact(test.channelType, test.apiType))
		})
	}
}

func TestMultiprotocolGatewayEndpointTypes(t *testing.T) {
	want := []constant.EndpointType{
		constant.EndpointTypeOpenAI,
		constant.EndpointTypeOpenAIResponse,
		constant.EndpointTypeOpenAIResponseCompact,
		constant.EndpointTypeAnthropic,
		constant.EndpointTypeGemini,
		constant.EndpointTypeOpenAIAlphaSearch,
	}

	assert.Equal(t, want, common.GetEndpointTypesByChannelType(constant.ChannelTypeNewAPI, "gpt-5"))
	assert.Equal(t, want, common.GetEndpointTypesByChannelType(constant.ChannelTypeSub2API, "gpt-5"))
}

func TestCopyChannelRejectsInvalidLegacyProxySettings(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	settingBytes, err := common.Marshal(dto.ProviderSettings{
		Proxy: "socks5://proxy.example/legacy-path",
	})
	require.NoError(t, err)
	setting := string(settingBytes)
	origin := &model.Provider{
		Type:    constant.ChannelTypeOpenAI,
		Name:    "legacy proxy channel",
		Key:     "test-key",
		Models:  "gpt-test",
		Group:   "default",
		Setting: &setting,
	}
	require.NoError(t, db.Create(origin).Error)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", origin.Id)}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/copy", nil)

	CopyChannel(ctx)

	assert.Contains(t, recorder.Body.String(), "invalid channel settings")
	var channelCount int64
	require.NoError(t, db.Model(&model.Provider{}).Count(&channelCount).Error)
	assert.Equal(t, int64(1), channelCount)
}

func TestDeleteChannelResetsProxyCacheWhenPreReadFails(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AuditLog{}))
	service.ResetProxyClientCache()
	t.Cleanup(service.ResetProxyClientCache)

	proxyURL := "http://proxy.example:8080"
	beforeDelete, err := service.GetHttpClientWithProxy(proxyURL)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "999999"}}
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/api/channel/999999", nil)

	DeleteChannel(ctx)

	assert.Contains(t, recorder.Body.String(), `"success":true`)
	afterDelete, err := service.GetHttpClientWithProxy(proxyURL)
	require.NoError(t, err)
	assert.NotSame(t, beforeDelete, afterDelete)
}

func TestDeleteChannelBatchReportsAndAuditsActualDeletedCount(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AuditLog{}))
	channel := &model.Provider{Name: "existing", Key: "test-key"}
	require.NoError(t, db.Create(channel).Error)

	requestBody, err := common.Marshal(ProviderBatch{Ids: []int{channel.Id, 999999}})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/api/channel/batch", bytes.NewReader(requestBody))
	ctx.Request.Header.Set("Content-Type", "application/json")

	DeleteChannelBatch(ctx)

	var response struct {
		Success bool  `json:"success"`
		Data    int64 `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.True(t, response.Success)
	assert.Equal(t, int64(1), response.Data)

	var auditLog model.AuditLog
	require.NoError(t, model.AuditLogQuery().First(&auditLog).Error)
	var auditData struct {
		Operation struct {
			Params map[string]any `json:"params"`
		} `json:"op"`
	}
	encodedAudit, err := common.Marshal(auditLog.Other)
	require.NoError(t, err)
	require.NoError(t, common.Unmarshal(encodedAudit, &auditData))
	assert.Equal(t, float64(1), auditData.Operation.Params["count"])
}

// Historic quota/expression metadata is readable evidence, not a live calculator.
func TestHistoricalChannelTestLogsRemainReadable(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.QuotaData{}))
	require.NoError(t, model.MigrateLogDB())
	expression := `param("stream") == true ? tier("stream", p * 3) : tier("base", p * 2)`
	for _, tc := range []struct {
		name  string
		quota int
		tier  string
	}{{"stream", 1500, "stream"}, {"base", 1000, "base"}} {
		t.Run(tc.name, func(t *testing.T) {
			other := common.MapToJsonStr(map[string]any{
				"billing_mode": "tiered_expr", "matched_tier": tc.tier,
				"request_rules": []map[string]any{{"cond": `param("service_tier") == "fast"`, "multiplier": 2, "matched": true}},
				"expr_b64":      base64.StdEncoding.EncodeToString([]byte(expression)),
				"cache_tokens":  12, "cache_write_tokens": 30, "cache_creation_5m_tokens": 10, "cache_creation_1h_tokens": 20,
				"admin_info": map[string]any{"channel_id": 99}, "root_info": map[string]any{"upstream_task_id": "retained-private-id"},
			})
			stored := model.Log{UserId: 2, Type: model.LogTypeConsume, CreatedAt: 3600, Content: "Historical channel test", ModelName: "retired-model", Quota: tc.quota, PromptTokens: 1000, RequestId: "historical-probe-" + tc.name, Other: other}
			require.NoError(t, model.LOG_DB.Create(&stored).Error)
			filter := model.LogFilter{Type: model.LogTypeConsume, RequestId: stored.RequestId, StartTimestamp: 3600, EndTimestamp: 3600}
			rows, total, err := model.GetUserLogs(2, filter, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			require.Len(t, rows, 1)
			assert.Equal(t, tc.quota, rows[0].Quota)
			fields, err := common.StrToMap(rows[0].Other)
			require.NoError(t, err)
			assert.Equal(t, "tiered_expr", fields["billing_mode"])
			assert.Equal(t, tc.tier, fields["matched_tier"])
			assert.Equal(t, base64.StdEncoding.EncodeToString([]byte(expression)), fields["expr_b64"])
			assert.Equal(t, float64(12), fields["cache_tokens"])
			assert.Equal(t, float64(30), fields["cache_write_tokens"])
			assert.Equal(t, float64(10), fields["cache_creation_5m_tokens"])
			assert.Equal(t, float64(20), fields["cache_creation_1h_tokens"])
			assert.Equal(t, []any{map[string]any{"cond": `param("service_tier") == "fast"`, "multiplier": float64(2), "matched": true}}, fields["request_rules"])
			assert.NotContains(t, fields, "admin_info")
			assert.NotContains(t, fields, "root_info")
			_, total, err = model.GetUserLogs(3, filter, 0, 10)
			require.NoError(t, err)
			assert.Zero(t, total)
			filter.StartTimestamp = 3601
			_, total, err = model.GetUserLogs(2, filter, 0, 10)
			require.NoError(t, err)
			assert.Zero(t, total)
			filter.StartTimestamp = 3600
			rows, total, err = model.GetAllLogs(filter, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			require.Len(t, rows, 1)
			model.FormatAdminLogs(rows)
			fields, err = common.StrToMap(rows[0].Other)
			require.NoError(t, err)
			assert.Contains(t, fields, "admin_info")
			assert.NotContains(t, fields, "root_info")
			var persisted model.Log
			require.NoError(t, model.LOG_DB.Where("request_id = ?", stored.RequestId).Take(&persisted).Error)
			assert.Equal(t, other, persisted.Other, "read shaping never rewrites historic pricing/cache facts")
		})
	}
	var statistics int64
	require.NoError(t, db.Model(&model.QuotaData{}).Count(&statistics).Error)
	assert.Zero(t, statistics, "reading historical logs never regenerates buffered statistics")
}

func TestRetiredChannelTestsPreserveQueuedHistory(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.SystemTask{}, &model.SystemTaskLock{}, &model.QuotaData{}))
	require.NoError(t, model.MigrateLogDB())
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { upstreamCalls.Add(1); w.WriteHeader(http.StatusOK) }))
	defer upstream.Close()
	for _, status := range []int{common.ChannelStatusEnabled, common.ChannelStatusAutoDisabled, common.ChannelStatusManuallyDisabled} {
		provider := model.Provider{Type: constant.ChannelTypeOpenAI, Name: fmt.Sprintf("retired-%d", status), Status: status, Key: "fixture-only", BaseURL: common.GetPointer(upstream.URL), Models: "retired-model", Group: "default"}
		require.NoError(t, db.Create(&provider).Error)
	}
	var originalProviders []model.Provider
	require.NoError(t, db.Order("id").Find(&originalProviders).Error)
	for _, mode := range []string{"scheduled_all", "passive_recovery", "auto_ban_only"} {
		t.Run(mode, func(t *testing.T) {
			existing, err := model.CreateSystemTask(model.SystemTaskTypeChannelTest, nil, map[string]any{"manual": true, "mode": mode, "notify": true, "concurrency": 2})
			require.NoError(t, err)
			for _, path := range []string{"/api/channel/test", "/api/channel/test/1"} {
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Set("id", 2)
				ctx.Request = httptest.NewRequest(http.MethodGet, path, nil)
				RetiredLegacyChannelManagement(ctx)
				require.Equal(t, http.StatusGone, recorder.Code)
				assert.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
				assert.Contains(t, recorder.Body.String(), "legacy_channel_management_retired")
				unchanged, err := model.GetSystemTaskByTaskID(existing.TaskID)
				require.NoError(t, err)
				assert.Equal(t, existing, unchanged, "retired HTTP must not enqueue, replace, or execute a queued job")
			}
			claimed, acquired, err := model.ClaimSystemTask(existing.ID, existing.Type, "retired-runner", common.GetTimestamp()+60)
			require.NoError(t, err)
			require.True(t, acquired)
			handler := providerTestHandler{}
			assert.False(t, handler.Enabled())
			cancelled, cancel := context.WithCancel(context.Background())
			cancel()
			handler.Run(cancelled, claimed, "retired-runner")
			running, err := model.GetSystemTaskByTaskID(existing.TaskID)
			require.NoError(t, err)
			assert.Equal(t, model.SystemTaskStatusRunning, running.Status)
			handler.Run(context.Background(), claimed, "retired-runner")
			finished, err := model.GetSystemTaskByTaskID(existing.TaskID)
			require.NoError(t, err)
			assert.Equal(t, model.SystemTaskStatusFailed, finished.Status)
			assert.Equal(t, existing.Payload, finished.Payload)
			assert.Equal(t, existing.State, finished.State)
			assert.Equal(t, "Legacy Channel management has been retired. Use the Provider and Channel APIs instead.", finished.Error)
			assert.Nil(t, finished.ActiveKey)
		})
	}
	var currentProviders []model.Provider
	require.NoError(t, db.Order("id").Find(&currentProviders).Error)
	assert.Equal(t, originalProviders, currentProviders)
	assert.Zero(t, upstreamCalls.Load())
	var statistics, logs, locks, tasks int64
	require.NoError(t, db.Model(&model.QuotaData{}).Count(&statistics).Error)
	assert.Zero(t, statistics)
	require.NoError(t, model.LOG_DB.Model(&model.Log{}).Count(&logs).Error)
	assert.Zero(t, logs)
	require.NoError(t, db.Model(&model.SystemTaskLock{}).Count(&locks).Error)
	assert.Zero(t, locks)
	require.NoError(t, db.Model(&model.SystemTask{}).Count(&tasks).Error)
	assert.EqualValues(t, 3, tasks)
}
