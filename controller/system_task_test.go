package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/testdb"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLegacyProviderMaintenanceTasksAreRetired(t *testing.T) {
	require.NoError(t, i18n.Init())
	monitor := operation_setting.GetMonitorSetting()
	previousMonitor := *monitor
	previousRedis, previousMemory := common.RedisEnabled, common.MemoryCacheEnabled
	t.Cleanup(func() {
		*monitor = previousMonitor
		common.RedisEnabled, common.MemoryCacheEnabled = previousRedis, previousMemory
	})
	monitor.AutoTestChannelEnabled = true
	common.RedisEnabled, common.MemoryCacheEnabled = false, false
	t.Setenv("CHANNEL_UPSTREAM_MODEL_UPDATE_TASK_ENABLED", "true")

	for _, handler := range []service.ScheduledSystemTaskHandler{providerTestHandler{}, modelUpdateHandler{}} {
		t.Run(handler.Type(), func(t *testing.T) {
			db := setupModelListControllerTestDB(t)
			require.NoError(t, db.AutoMigrate(&model.SystemTask{}, &model.SystemTaskLock{}, &model.QuotaData{}))
			root := &model.User{Username: "maintenance-root", Role: common.RoleRootUser, Status: common.UserStatusEnabled}
			require.NoError(t, db.Create(root).Error)
			var upstreamCalls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				_, _ = w.Write([]byte(`{"data":[{"id":"legacy-model"},{"id":"unexpected-new-model"}]}`))
			}))
			t.Cleanup(upstream.Close)
			provider := newAdvancedCustomModelListChannel(upstream.URL, "maintenance-fixture-key", "/v1/models", nil)
			provider.Name, provider.Models = "maintenance provider", "legacy-model"
			settings := provider.GetOtherSettings()
			settings.UpstreamModelUpdateCheckEnabled = true
			settings.UpstreamModelUpdateAutoSyncEnabled = true
			provider.SetOtherSettings(settings)
			require.NoError(t, db.Create(provider).Error)
			declareLegacyProviderRoutingColumns(t, db)
			seedLegacyProviderRouting(t, db, provider)
			require.NoError(t, db.AutoMigrate(&model.Ability{}))
			require.NoError(t, provider.AddAbilities(db))
			var originalAbilities []model.Ability
			require.NoError(t, db.Order("channel_id, model").Find(&originalAbilities).Error)
			require.NotEmpty(t, originalAbilities)
			report, err := model.MigrateLegacyGateway(context.Background())
			require.NoError(t, err)
			require.True(t, report.Committed)
			require.False(t, db.Migrator().HasTable("abilities"))
			require.False(t, db.Migrator().HasColumn("providers", "models"))
			var originalProvider model.Provider
			require.NoError(t, db.First(&originalProvider, provider.Id).Error)
			assert.Equal(t, provider.Name, originalProvider.Name)
			assert.Equal(t, provider.Key, originalProvider.Key)
			originalSeal, err := model.VerifyLegacyAbilitySourceArchive(db)
			require.NoError(t, err)
			var archivedAbilities []model.Ability
			require.NoError(t, db.Table("legacy_ability_sources").Order("channel_id, model").Find(&archivedAbilities).Error)
			require.Equal(t, originalAbilities, archivedAbilities, "offline retirement preserves every original routing source fact")

			assert.False(t, handler.Enabled(), "retired jobs must not be scheduled even when old settings opt in")
			for _, tc := range []struct {
				name    string
				payload string
			}{
				{"scheduled", ""},
				{"manual", `{"manual":true,"mode":"scheduled_all","notify":true}`},
				{"malformed historical payload", `{not-json`},
			} {
				t.Run(tc.name, func(t *testing.T) {
					task, err := model.CreateSystemTask(handler.Type(), nil, map[string]any{"retained": true})
					require.NoError(t, err)
					require.NoError(t, db.Model(task).Update("payload", tc.payload).Error)
					claimed, acquired, err := model.ClaimSystemTask(task.ID, task.Type, "maintenance-runner", common.GetTimestamp()+60)
					require.NoError(t, err)
					require.True(t, acquired)
					if tc.name == "scheduled" {
						handler.Run(context.Background(), claimed, "other-runner")
						cancelled, cancel := context.WithCancel(context.Background())
						cancel()
						handler.Run(cancelled, claimed, "maintenance-runner")
						stillClaimed, err := model.GetSystemTaskByTaskID(task.TaskID)
						require.NoError(t, err)
						assert.Equal(t, model.SystemTaskStatusRunning, stillClaimed.Status)
						assert.Empty(t, stillClaimed.Error)
					}
					handler.Run(context.Background(), claimed, "maintenance-runner")
					stored, err := model.GetSystemTaskByTaskID(task.TaskID)
					require.NoError(t, err)
					assert.Equal(t, model.SystemTaskStatusFailed, stored.Status)
					assert.Equal(t, "Legacy Channel management has been retired. Use the Provider and Channel APIs instead.", stored.Error)
					assert.Equal(t, tc.payload, stored.Payload)
					assert.Equal(t, claimed.State, stored.State)
					assert.Equal(t, claimed.CreatedAt, stored.CreatedAt)
					assert.Nil(t, stored.ActiveKey)
					var locks int64
					require.NoError(t, db.Model(&model.SystemTaskLock{}).Where("task_id = ?", task.TaskID).Count(&locks).Error)
					assert.Zero(t, locks)
				})
			}
			var currentProvider model.Provider
			require.NoError(t, db.First(&currentProvider, provider.Id).Error)
			assert.Equal(t, originalProvider, currentProvider)
			var currentAbilities []model.Ability
			require.False(t, db.Migrator().HasTable("abilities"), "retired jobs cannot recreate the routing table")
			require.NoError(t, db.Table("legacy_ability_sources").Order("channel_id, model").Find(&currentAbilities).Error)
			assert.Equal(t, originalAbilities, currentAbilities)
			currentSeal, err := model.VerifyLegacyAbilitySourceArchive(db)
			require.NoError(t, err)
			assert.Equal(t, originalSeal, currentSeal, "retired jobs cannot change archived evidence or its seal")
			assert.Zero(t, upstreamCalls.Load())
			var quotaRows int64
			require.NoError(t, db.Model(&model.QuotaData{}).Count(&quotaRows).Error)
			assert.Zero(t, quotaRows)
		})
	}
}

func TestLegacyMidjourneyPollingTasksAreRetired(t *testing.T) {
	previousRedis, previousMemory := common.RedisEnabled, common.MemoryCacheEnabled
	previousUpdate, previousConsume := constant.UpdateTask, common.LogConsumeEnabled
	common.RedisEnabled, common.MemoryCacheEnabled = false, false
	common.LogConsumeEnabled = true
	t.Cleanup(func() {
		common.RedisEnabled, common.MemoryCacheEnabled = previousRedis, previousMemory
		constant.UpdateTask, common.LogConsumeEnabled = previousUpdate, previousConsume
	})
	if service.GetHttpClient() == nil {
		service.InitHttpClient()
	}

	for _, tc := range []struct {
		name      string
		payload   string
		update    bool
		wrongUser bool
		cancelled bool
		expired   bool
	}{
		{name: "scheduled", update: true},
		{name: "already queued while scheduling disabled"},
		{name: "manual", payload: `{"manual":true}`, update: true},
		{name: "malformed historical payload", payload: `{not-json`, update: true},
		{name: "other lease owner", update: true, wrongUser: true},
		{name: "cancelled lease context", update: true, cancelled: true},
		{name: "expired lease", update: true, expired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			constant.UpdateTask = tc.update
			db := model.UseTestDatabases(t)
			require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Provider{}, &model.Midjourney{}, &model.Task{}, &model.SystemTask{}, &model.SystemTaskLock{}))
			require.NoError(t, model.MigrateLogDB())
			user := model.User{Username: "retired-mj-user", Status: common.UserStatusEnabled, Quota: 1000, UsedQuota: 900}
			require.NoError(t, db.Create(&user).Error)
			token := model.Token{UserId: user.Id, Key: "retired-mj-token", Status: common.TokenStatusEnabled}
			require.NoError(t, db.Create(&token).Error)
			legacyQuota := testdb.LegacyKeyQuota{RemainQuota: 1100, UsedQuota: 900}
			testdb.SeedLegacyKeyQuota(t, db, token.Id, legacyQuota)
			var upstreamCalls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				assert.Equal(t, "/mj/task/list-by-condition", r.URL.Path)
				body, err := common.Marshal([]dto.MidjourneyDto{{MjId: "original-mj-id", Status: "FAILURE", Progress: "100%", FailReason: "fixture failure"}})
				assert.NoError(t, err)
				_, _ = w.Write(body)
			}))
			t.Cleanup(upstream.Close)
			provider := model.Provider{Name: "retired-mj-provider", Type: constant.ChannelTypeMidjourney, Key: "retired-mj-provider-key", Status: common.ChannelStatusEnabled, BaseURL: &upstream.URL, UsedQuota: 900}
			require.NoError(t, db.Create(&provider).Error)
			legacy := []model.Midjourney{
				{UserId: user.Id, ChannelId: provider.Id, BillingChannelId: provider.Id, TokenId: token.Id, MjId: "original-mj-id", Action: "IMAGINE", Status: "IN_PROGRESS", Progress: "10%", Quota: 250, SubmitTime: common.GetTimestamp() * 1000},
				{UserId: user.Id, ChannelId: provider.Id, Status: "SUBMITTED", Progress: "0%", Quota: 500},
			}
			require.NoError(t, db.Create(&legacy).Error)
			pending := model.Task{TaskID: "independent-task", Platform: "midjourney", UserId: user.Id, ChannelId: provider.Id, Status: model.TaskStatusSubmitted, Progress: "10%"}
			require.NoError(t, db.Create(&pending).Error)
			var originalPending model.Task
			require.NoError(t, db.First(&originalPending, pending.ID).Error)
			handler := midjourneyPollHandler{}
			assert.False(t, handler.Enabled(), "old settings cannot re-enable legacy polling")
			assert.Zero(t, handler.Interval())
			assert.Equal(t, tc.update, (asyncTaskPollHandler{}).Enabled(), "canonical task scheduling retains its existing switch")
			assert.Positive(t, (asyncTaskPollHandler{}).Interval())
			job, err := model.CreateSystemTask(handler.Type(), nil, map[string]any{"retained": true})
			require.NoError(t, err)
			require.NoError(t, db.Model(job).Update("payload", tc.payload).Error)
			claimed, acquired, err := model.ClaimSystemTask(job.ID, job.Type, "mj-original-runner", common.GetTimestamp()+60)
			require.NoError(t, err)
			require.True(t, acquired)
			if tc.expired {
				require.NoError(t, db.Model(&model.SystemTaskLock{}).Where("task_id = ?", job.TaskID).Update("locked_until", common.GetTimestamp()-1).Error)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelled {
				cancel()
			}
			runner := "mj-original-runner"
			if tc.wrongUser {
				runner = "other-runner"
			}
			handler.Run(ctx, claimed, runner)
			stored, err := model.GetSystemTaskByTaskID(job.TaskID)
			require.NoError(t, err)
			if tc.wrongUser || tc.cancelled || tc.expired {
				assert.Equal(t, model.SystemTaskStatusRunning, stored.Status)
				assert.Empty(t, stored.Error)
				assert.NotNil(t, stored.ActiveKey)
				if tc.expired {
					require.NoError(t, model.ExpireStaleSystemTaskLocks(common.GetTimestamp()))
				} else {
					handler.Run(context.Background(), claimed, "mj-original-runner")
				}
				stored, err = model.GetSystemTaskByTaskID(job.TaskID)
				require.NoError(t, err)
			}
			assert.Equal(t, model.SystemTaskStatusFailed, stored.Status)
			if tc.expired {
				assert.Equal(t, "task lease expired", stored.Error)
			} else {
				assert.Equal(t, "Legacy Midjourney polling has been retired. Migrate historical tasks before starting the server.", stored.Error)
			}
			assert.Equal(t, claimed.Payload, stored.Payload)
			assert.Equal(t, claimed.State, stored.State)
			assert.Equal(t, claimed.CreatedAt, stored.CreatedAt)
			assert.Nil(t, stored.ActiveKey)
			beforeReplay := *stored
			handler.Run(context.Background(), claimed, "mj-original-runner")
			stored, err = model.GetSystemTaskByTaskID(job.TaskID)
			require.NoError(t, err)
			assert.Equal(t, beforeReplay, *stored)
			var locks int64
			require.NoError(t, db.Model(&model.SystemTaskLock{}).Where("task_id = ?", job.TaskID).Count(&locks).Error)
			assert.Zero(t, locks)
			var currentUser model.User
			var currentToken model.Token
			var currentProvider model.Provider
			var currentPending model.Task
			var currentLegacy []model.Midjourney
			require.NoError(t, db.First(&currentUser, user.Id).Error)
			require.NoError(t, db.First(&currentToken, token.Id).Error)
			require.NoError(t, db.First(&currentProvider, provider.Id).Error)
			require.NoError(t, db.First(&currentPending, pending.ID).Error)
			require.NoError(t, db.Order("id").Find(&currentLegacy).Error)
			assert.Equal(t, user, currentUser)
			assert.Equal(t, token, currentToken)
			assert.Equal(t, legacyQuota, testdb.ReadLegacyKeyQuota(t, db, token.Id))
			assert.Equal(t, provider, currentProvider)
			assert.Equal(t, originalPending, currentPending)
			assert.Equal(t, legacy, currentLegacy)
			assert.Zero(t, upstreamCalls.Load(), "retired, cancelled and wrong-owner jobs cannot contact Providers")
			var logs int64
			require.NoError(t, model.LOG_DB.Model(&model.Log{}).Count(&logs).Error)
			assert.Zero(t, logs, "job retirement must not invent a historical refund")
		})
	}
}

func TestSystemTaskListFiltersAndPaginationResponse(t *testing.T) {
	db := model.UseTestDatabases(t)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	previousDB := model.DB
	t.Cleanup(func() {
		model.DB = previousDB
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.SystemTask{}))
	require.NoError(t, db.Create(&[]model.SystemTask{
		{TaskID: "older", Type: model.SystemTaskTypeModelUpdate, Status: model.SystemTaskStatusFailed},
		{TaskID: "newer", Type: model.SystemTaskTypeModelUpdate, Status: model.SystemTaskStatusFailed},
		{TaskID: "other-status", Type: model.SystemTaskTypeModelUpdate, Status: model.SystemTaskStatusSucceeded},
		{TaskID: "other-type", Type: model.SystemTaskTypeChannelTest, Status: model.SystemTaskStatusFailed},
	}).Error)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/system-task/list?scope=history&type=model_update&status=failed&offset=1&limit=1", nil)
	ListSystemTasks(c)
	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Success bool                       `json:"success"`
		Total   int64                      `json:"total"`
		Data    []model.SystemTaskResponse `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.True(t, response.Success)
	assert.EqualValues(t, 2, response.Total)
	require.Len(t, response.Data, 1)
	assert.Equal(t, "older", response.Data[0].TaskID)
}

func TestSystemTaskInvalidFiltersAreRejected(t *testing.T) {
	for _, tc := range []struct {
		query   string
		handler gin.HandlerFunc
	}{
		{"?scope=invalid", ListSystemTasks},
		{"?status=unknown", ListSystemTasks},
		{"?offset=-1", ListSystemTasks},
		{"?limit=invalid", ListSystemTasks},
		{"?status=unknown", DeleteSystemTaskHistory},
		{"?scope=invalid", DeleteSystemTaskHistory},
	} {
		t.Run(tc.query, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/"+tc.query, nil)
			tc.handler(c)
			assert.Equal(t, http.StatusBadRequest, recorder.Code)
			assert.JSONEq(t, `{"success":false,"message":"invalid system task filters"}`, recorder.Body.String())
		})
	}
}
