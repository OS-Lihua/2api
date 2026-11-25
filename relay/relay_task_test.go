package relay

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/money"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const legacyTaskFixtureUnitsPerUSD = 500_000

func setupRelayChannelDB(t *testing.T) *gorm.DB {
	t.Helper()
	previousDB := model.DB
	previousCache := common.MemoryCacheEnabled
	database := model.UseTestDatabases(t)
	sqlDB, err := database.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, database.AutoMigrate(&model.Provider{}))
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		model.DB = previousDB
		common.MemoryCacheEnabled = previousCache
		require.NoError(t, sqlDB.Close())
	})
	return database
}

func persistRealtimeTaskEvidence(t *testing.T, database *gorm.DB, task *model.Task) {
	t.Helper()
	require.NoError(t, database.AutoMigrate(&model.Task{}, &model.User{}, &model.LegacySubscriptionTaskObligation{}))
	var owners int64
	require.NoError(t, database.Model(&model.User{}).Where("id = ?", task.UserId).Count(&owners).Error)
	if owners == 0 {
		require.NoError(t, database.Create(&model.User{Id: task.UserId, Username: "realtime-owner", Quota: 100, Status: common.UserStatusEnabled}).Error)
	}
	adaptor := GetTaskAdaptor(constant.TaskPlatform(strconv.Itoa(constant.ChannelTypeGemini)))
	require.NotNil(t, adaptor)
	identity, ok := adaptor.(interface{ TaskPluginIdentity() (string, string) })
	require.True(t, ok)
	key, version := identity.TaskPluginIdentity()
	task.Platform = constant.TaskPlatform(key)
	task.PrivateData.Execution = &model.TaskExecutionSnapshot{RequestID: "original-" + task.TaskID, TaskPlugin: &model.TaskPluginSnapshot{Key: key, Version: version}}
	require.NoError(t, database.Create(task).Error)
	require.NoError(t, model.MigrateLegacyTaskObligations(database))
}

func TestRealtimeFetchAppliesChannelHeaderOverrides(t *testing.T) {
	database := setupRelayChannelDB(t)
	service.InitHttpClient()
	requests := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Clone(r.Context())
		_, _ = w.Write([]byte(`{"name":"operations/task-1","done":false}`))
	}))
	t.Cleanup(server.Close)
	headerOverride := `{"X-API-Key":"{api_key}"}`
	channel := &model.Provider{
		Name: "realtime-header-override", Type: constant.ChannelTypeGemini,
		Key: "channel-key\nsaved-key", BaseURL: &server.URL, HeaderOverride: &headerOverride, Status: common.ChannelStatusEnabled,
		ProviderInfo: model.ProviderInfo{IsMultiKey: true, MultiKeySize: 2},
	}
	require.NoError(t, database.Create(channel).Error)
	task := &model.Task{
		TaskID: "public-task", UserId: 7, ChannelId: channel.Id,
		Status: model.TaskStatusInProgress, Progress: "50%",
		PrivateData: model.TaskPrivateData{
			Key: "saved-key", UpstreamTaskID: base64.RawURLEncoding.EncodeToString([]byte("operations/task-1")),
		},
	}

	persistRealtimeTaskEvidence(t, database, task)
	tryRealtimeFetch(task, true)

	require.Len(t, requests, 1)
	request := <-requests
	assert.Equal(t, "/v1beta/operations/task-1", request.URL.Path)
	assert.Equal(t, "saved-key", request.Header.Get("X-API-Key"))
	assert.Equal(t, "saved-key", request.Header.Get("X-Goog-Api-Key"))

	t.Run("money task reads stay out of legacy realtime helper", func(t *testing.T) {
		moneyTask := *task
		moneyTask.PrivateData.Gateway = &model.TaskGatewaySnapshot{
			Accepted: true, ProviderID: channel.Id, ProviderType: constant.ChannelTypeGemini, ChannelID: 1,
			Settlement: model.GatewaySettlement{Attempt: &model.GatewayAttempt{ProviderID: channel.Id, ChannelID: 1}},
		}
		assert.Empty(t, tryRealtimeFetch(&moneyTask, true))
		assert.Empty(t, requests, "money reads use persisted facts even when an original Provider credential remains valid")
	})
	t.Run("revoked original slot cannot use another enabled slot", func(t *testing.T) {
		channel.ProviderInfo.MultiKeyStatusList = map[int]int{1: common.ChannelStatusManuallyDisabled}
		require.NoError(t, database.Model(&model.Provider{}).Where("id = ?", channel.Id).Update("channel_info", channel.ProviderInfo).Error)
		assert.Empty(t, tryRealtimeFetch(task, true))
		assert.Empty(t, requests)
		var evidence model.LegacyTaskObligation
		require.NoError(t, database.First(&evidence, "source_task_id = ?", task.ID).Error)
		assert.Equal(t, "unverified", evidence.State)
	})
}

func TestRealtimeFetchRefundsTaskItFindsFailed(t *testing.T) {
	database := setupRelayChannelDB(t)
	previousFactory, previousRedis := service.GetTaskAdaptorFunc, common.RedisEnabled
	common.RedisEnabled = false
	service.GetTaskAdaptorFunc = func(platform constant.TaskPlatform) service.TaskPollingAdaptor {
		if adaptor := GetTaskAdaptor(platform); adaptor != nil {
			return adaptor
		}
		return nil
	}
	t.Cleanup(func() { service.GetTaskAdaptorFunc = previousFactory; common.RedisEnabled = previousRedis })
	service.InitHttpClient()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"name":"operations/task-1","done":true,"error":{"code":3,"message":"blocked by safety"}}`))
	}))
	t.Cleanup(server.Close)
	provider := &model.Provider{Name: "realtime-refund", Type: constant.ChannelTypeGemini, Key: "channel-key", BaseURL: &server.URL, Status: common.ChannelStatusEnabled}
	require.NoError(t, database.Create(provider).Error)
	task := &model.Task{TaskID: "public-task", UserId: 7, ChannelId: provider.Id, Quota: 1000,
		Status: model.TaskStatusInProgress, Progress: "50%", PrivateData: model.TaskPrivateData{
			Key: provider.Key, UpstreamTaskID: base64.RawURLEncoding.EncodeToString([]byte("operations/task-1")),
		}}
	persistRealtimeTaskEvidence(t, database, task)
	var evidence model.LegacyTaskObligation
	require.NoError(t, database.First(&evidence, "source_task_id = ?", task.ID).Error)
	tryRealtimeFetch(task, true)
	var stored model.Task
	require.NoError(t, database.First(&stored, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), stored.Status)
	assert.Equal(t, "blocked by safety", stored.FailReason)
	assert.Equal(t, 1000, stored.Quota, "an unverified historical task is not refund proof")
	var afterEvidence model.LegacyTaskObligation
	require.NoError(t, database.First(&afterEvidence, "source_task_id = ?", task.ID).Error)
	assert.Equal(t, evidence, afterEvidence)

	t.Run("canonical accepted task releases its actual reservation once", func(t *testing.T) {
		// The outbox publishes through the log connection while holding its
		// primary transaction; the account-only fixture's one-slot pool cannot.
		sqlDB, err := database.DB()
		require.NoError(t, err)
		sqlDB.SetMaxOpenConns(4)
		t.Cleanup(func() { sqlDB.SetMaxOpenConns(1) })
		t.Setenv("GATEWAY_SETTLEMENT_JOURNAL_DIR", t.TempDir())
		require.NoError(t, model.MigrateSubscriptionMoney(database))
		require.NoError(t, model.MigrateGatewayBilling(database))
		require.NoError(t, model.MigrateGatewayRecords())
		require.NoError(t, database.Model(&model.User{}).Where("id = ?", 7).Update("setting", `{"billing_preference":"wallet_only"}`).Error)
		_, err = model.TopUpWallet(model.WalletTopUp{UserId: 7, Source: money.Amount{Nanos: 5000, Currency: "USD"}, Reference: "canonical-refund-fixture"})
		require.NoError(t, err)
		request := model.GatewayRequest{ID: "canonical-realtime-refund", UserID: 7, SaleCurrency: "USD", SaleFXRate: "1", GroupRatio: "1", SaleExpression: "0.000001", StartedAt: time.Now().UnixMilli()}
		attempt := model.GatewayAttempt{ID: "canonical-realtime-attempt", RequestID: request.ID, ProviderID: provider.Id, ChannelID: 99, StartedAt: request.StartedAt,
			PurchaseExpression: "0.0000002", Cost: money.Amount{Currency: "USD"}, ConvertedCost: money.Amount{Currency: "USD"}, AccountingFXRate: "1"}
		accepted := &model.Task{TaskID: "canonical-task", UserId: 7, ChannelId: provider.Id, Platform: task.Platform, Status: model.TaskStatusInProgress, Progress: "50%",
			PrivateData: model.TaskPrivateData{UpstreamTaskID: task.PrivateData.UpstreamTaskID, Execution: task.PrivateData.Execution, Gateway: &model.TaskGatewaySnapshot{
				ProviderID: provider.Id, ProviderType: provider.Type, ChannelID: 99, Settlement: model.GatewaySettlement{Request: request, Attempt: &attempt}, Facts: map[string]any{},
			}}}
		reserved := money.Amount{Nanos: 1000, Currency: "USD"}
		require.NoError(t, model.ReserveGatewayTask(&request, reserved, reserved, accepted))
		accepted.PrivateData.Gateway.Accepted = true
		require.NoError(t, database.Save(accepted).Error)
		wallet, err := model.GetUserWallet(7)
		require.NoError(t, err)
		assert.EqualValues(t, 4000, wallet.Balance.Nanos)
		before := calls.Load()
		assert.Empty(t, tryRealtimeFetch(accepted, true))
		assert.Equal(t, before, calls.Load(), "client reads cannot poll or refund a money task")
		require.NoError(t, service.UpdateGatewayTask(t.Context(), accepted))
		require.NoError(t, database.First(accepted, accepted.ID).Error)
		assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), accepted.Status)
		assert.Equal(t, "blocked by safety", accepted.FailReason)
		wallet, err = model.GetUserWallet(7)
		require.NoError(t, err)
		assert.EqualValues(t, 5000, wallet.Balance.Nanos)
		var entries, replay []model.LedgerEntry
		require.NoError(t, database.Where("request_id = ?", request.ID).Order("id").Find(&entries).Error)
		require.NoError(t, service.SettleGatewayTask(t.Context(), accepted))
		require.NoError(t, database.Where("request_id = ?", request.ID).Order("id").Find(&replay).Error)
		assert.Equal(t, entries, replay)
		var holds, releases, payables int
		for _, entry := range entries {
			if entry.Type == model.LedgerEntryPreConsume {
				holds++
				assert.Equal(t, money.Amount{Nanos: -1000, Currency: "USD"}, entry.Amount)
			}
			if entry.Type == model.LedgerEntryPreConsumeRelease {
				releases++
				assert.Equal(t, reserved, entry.Amount)
			}
			if entry.AccountType == model.LedgerAccountProviderPayable {
				payables++
				assert.EqualValues(t, 200, entry.Amount.Nanos)
			}
		}
		assert.Equal(t, 1, holds)
		assert.Equal(t, 1, releases)
		assert.Equal(t, 1, payables)
		projection, err := model.GetGatewayRequest(t.Context(), request.ID, 7)
		require.NoError(t, err)
		assert.Equal(t, money.Amount{Currency: "USD"}, projection.Receivable)
		assert.False(t, projection.SalePending)
		reconciled, err := model.ReconcileWallets(100)
		require.NoError(t, err)
		assert.Empty(t, reconciled.Mismatches)
	})
	var user model.User
	require.NoError(t, database.First(&user, 7).Error)
	assert.Equal(t, 100, user.Quota, "neither lifecycle revives the retired integer wallet")
}

func TestApplyChannelPinPreservesOriginTasksAndRetryMode(t *testing.T) {
	database := setupRelayChannelDB(t)
	channel := &model.Provider{Name: "origin-channel", Key: "sk-test", Status: common.ChannelStatusEnabled, Type: constant.ChannelTypeDoubaoVideo}
	require.NoError(t, database.Create(channel).Error)
	originTask := &model.Task{
		TaskID: "task-lock", ChannelId: channel.Id, Action: "text_to_video", Status: model.TaskStatusSuccess,
		PrivateData: model.TaskPrivateData{UpstreamTaskID: "upstream-task-lock"},
		Data:        []byte(`{"id":"upstream-task-lock"}`),
	}

	for _, tc := range []struct {
		name     string
		tokenPin bool
		apply    func(*gin.Context, *relaycommon.RelayInfo) *dto.TaskError
	}{
		{name: "origin affinity", apply: ApplyOriginTaskAffinity},
		{name: "same channel retry", apply: ApplyChannelPin},
		{name: "token pin suppresses channel lock", tokenPin: true, apply: ApplyChannelPin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			common.SetContextKey(c, constant.ContextKeyOriginTasks, []*model.Task{originTask})
			constraints := service.GetChannelConstraints(c)
			constraints.AddPin(dto.ProviderPin{ChannelId: channel.Id, Source: dto.PinSourceOriginTask, Rank: dto.PinRankOriginTask, RetryMode: dto.PinRetrySameChannel})
			if tc.tokenPin {
				constraints.AddPin(dto.ProviderPin{ChannelId: channel.Id, Source: dto.PinSourceToken, Rank: dto.PinRankToken, RetryMode: dto.PinRetrySingleAttempt})
			}
			info := &relaycommon.RelayInfo{TaskRelayInfo: &relaycommon.TaskRelayInfo{}}
			require.Nil(t, tc.apply(c, info))
			if tc.tokenPin {
				assert.Nil(t, info.LockedChannel)
			} else {
				locked, ok := info.LockedChannel.(*model.Provider)
				require.True(t, ok)
				require.NotNil(t, locked)
				assert.Equal(t, channel.Id, locked.Id)
			}
			require.Len(t, info.OriginTasks, 1)
			assert.Equal(t, "task-lock", info.OriginTasks[0].TaskID)
			assert.Equal(t, "upstream-task-lock", info.OriginTasks[0].UpstreamTaskID)
			assert.Equal(t, "text_to_video", info.OriginTasks[0].Action)
			assert.Equal(t, string(model.TaskStatusSuccess), info.OriginTasks[0].Status)
			assert.Equal(t, []byte(originTask.Data), info.OriginTasks[0].Data)
		})
	}
}

func TestTaskModel2DtoNormalizesLegacyAction(t *testing.T) {
	task := &model.Task{Action: "firstTailGenerate"}

	dtoTask := TaskModel2Dto(task)

	assert.Equal(t, constant.TaskActionFirstTailToVideo, dtoTask.Action)
	assert.Equal(t, "firstTailGenerate", task.Action)
}

const mappingOrderSubmitPlugin = `
export const meta = {apiVersion:1,key:"maporder",name:"Map Order",version:"1.0.0",author:{name:"Test"},models:["declared-model"],fetchMode:"per_task"};
export function buildSubmitRequest(ctx) {
  return {url: ctx.baseUrl+"/submit", method:"POST", body:{upstreamModel: ctx.upstreamModel, model: ctx.model}, action:"text_to_video"};
}
export function parseSubmitResponse(){return {taskId:"1"};}
export function buildQueryRequest(){return {url:"https://provider.example"};}
export function parseTaskResult(){return {status:"SUCCESS"};}
`

const mappingOrderRewritePlugin = `
export const meta = {apiVersion:1,key:"maporder-rw",name:"Map Order RW",version:"1.0.0",author:{name:"Test"},models:["declared-model"],fetchMode:"per_task"};
export function buildSubmitRequest(ctx) {
  return {url: ctx.baseUrl+"/submit", method:"POST", body:{upstreamModel: ctx.upstreamModel}, rewriteModel:"rewritten"};
}
export function parseSubmitResponse(){return {taskId:"1"};}
export function buildQueryRequest(){return {url:"https://provider.example"};}
export function parseTaskResult(){return {status:"SUCCESS"};}
`

func pinMappingOrderPlugin(t *testing.T, c *gin.Context, source string) {
	t.Helper()
	plugin, err := pluginruntime.NewRegistry().Register(source, pluginruntime.Options{})
	require.NoError(t, err)
	c.Set(pluginruntime.ContextKeyPinnedPlugin, pluginruntime.PinnedPlugin{Plugin: plugin})
}

func newTaskSubmitContext(t *testing.T, originalModel, mapping string) (*gin.Context, *relaycommon.RelayInfo) {
	t.Helper()
	require.NoError(t, i18n.Init())
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	common.SetContextKey(c, constant.ContextKeyOriginalModel, originalModel)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://provider.example")
	if mapping != "" {
		c.Set("model_mapping", mapping)
	}
	c.Set("task_request", map[string]any{"prompt": "p"})
	return c, &relaycommon.RelayInfo{TaskRelayInfo: &relaycommon.TaskRelayInfo{}, ProviderMeta: &relaycommon.ProviderMeta{}}
}

const billingFallbackPlugin = `
export const meta = {apiVersion:1,key:"bill-fallback",name:"Bill Fallback",version:"1.0.0",author:{name:"Test"},models:["declared-model"],fetchMode:"per_task"};
export function buildSubmitRequest(ctx) {
  return {url: ctx.baseUrl+"/submit", method:"POST", body:{upstreamModel: ctx.upstreamModel, model: ctx.model}, action:"text_to_video"};
}
export function parseSubmitResponse(){return {taskId:"1"};}
export function buildQueryRequest(){return {url:"https://provider.example"};}
export function parseTaskResult(){return {status:"SUCCESS"};}
`

func saveBillingConfig(t *testing.T) {
	t.Helper()
	saved := map[string]string{}
	require.NoError(t, config.GlobalConfig.SaveToDB(func(key, value string) error {
		saved[key] = value
		return nil
	}))
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(saved))
	})
}

func TestRelayTaskSubmitAliasBillingIdentityAndExprFallback(t *testing.T) {
	const aliasExpr = `tier("alias", 2)`
	const tailExpr = `tier("tail", 3)`
	const legacyExpr = `tier("legacy", u("old_units") * 2)`

	tests := []struct {
		name       string
		modes      map[string]string
		exprs      map[string]string
		wantTiered bool
		wantExpr   string
		source     string
	}{
		{
			name:       "alias own tiered wins",
			modes:      map[string]string{"alias-model": "tiered_expr", "declared-model": "tiered_expr"},
			exprs:      map[string]string{"alias-model": aliasExpr, "declared-model": tailExpr},
			wantTiered: true,
			wantExpr:   aliasExpr,
		},
		{
			name:       "fallback uses tail expr",
			modes:      map[string]string{"declared-model": "tiered_expr"},
			exprs:      map[string]string{"declared-model": tailExpr},
			wantTiered: true,
			wantExpr:   tailExpr,
		},
		{
			name:       "stored expression keeps running after schema narrows",
			modes:      map[string]string{"declared-model": "tiered_expr"},
			exprs:      map[string]string{"declared-model": legacyExpr},
			wantTiered: true,
			wantExpr:   legacyExpr,
			source: strings.Replace(billingFallbackPlugin, `fetchMode:"per_task"`, `fetchMode:"per_task", usageSchema:{old_units:{type:"number",unit:"count"}}, usageProfiles:[{models:["declared-model"],schema:{seconds:{type:"number",unit:"second"}}}]`, 1) + `
export function extractUsage(){return {old_units:2};}
`,
		},
		{
			name:       "neither tiered uses ordinary pricing",
			wantTiered: false,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			saveBillingConfig(t)
			if len(testCase.modes) > 0 {
				modeJSON, marshalErr := common.Marshal(testCase.modes)
				require.NoError(t, marshalErr)
				exprJSON, marshalErr := common.Marshal(testCase.exprs)
				require.NoError(t, marshalErr)
				require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
					"billing_setting.billing_mode": string(modeJSON),
					"billing_setting.billing_expr": string(exprJSON),
				}))
				if testCase.wantExpr == aliasExpr {
					require.Equal(t, billing_setting.BillingModeTieredExpr, billing_setting.GetBillingMode("alias-model"))
				} else {
					require.Equal(t, billing_setting.BillingModeRatio, billing_setting.GetBillingMode("alias-model"))
					require.Equal(t, billing_setting.BillingModeTieredExpr, billing_setting.GetBillingMode("declared-model"))
				}
			}

			// This is a historical pricing-source compatibility test, not a live
			// request. Current requests never consult these retired option tables.
			expression, found := billing_setting.ResolveTaskBillingExpr("bill-fallback", "alias-model", "declared-model")
			assert.Equal(t, testCase.wantTiered, found)
			info := &relaycommon.RelayInfo{OriginModelName: "alias-model", ProviderMeta: &relaycommon.ProviderMeta{UpstreamModelName: "declared-model"}}
			task := model.InitTask(constant.TaskPlatform("bill-fallback"), info)
			assert.Equal(t, "alias-model", task.Properties.OriginModelName)
			assert.Equal(t, "declared-model", task.Properties.UpstreamModelName)
			if !found {
				assert.Empty(t, expression)
				return
			}
			assert.Equal(t, testCase.wantExpr, expression)
			if testCase.source != "" {
				registry := pluginruntime.NewRegistry()
				plugin, err := registry.Register(testCase.source, pluginruntime.Options{})
				require.NoError(t, err)
				schema, _ := plugin.Meta.UsageForModels("declared-model", "alias-model")
				assert.Contains(t, schema, "seconds")
				assert.NotContains(t, schema, "old_units")
				assert.False(t, billing_setting.TaskExprCompatible(expression, schema))
			}
			// Preserve a persisted legacy expression even if the current plugin
			// profile has narrowed. This evaluator must not mutate any funds.
			snapshot := &billingexpr.BillingSnapshot{BillingMode: billing_setting.BillingModeTieredExpr, ModelName: "alias-model", ExprString: expression, ExprHash: billingexpr.ExprHashString(expression), GroupRatio: 1, QuotaPerUnit: legacyTaskFixtureUnitsPerUSD, ExprVersion: billingexpr.ExprVersion(expression), TaskUsageBilling: true, UsageFacts: map[string]any{"old_units": float64(2)}}
			assert.Equal(t, billingexpr.ExprHashString(testCase.wantExpr), snapshot.ExprHash)
			result, _, err := service.EvaluateTaskCompletionUsage(snapshot, map[string]any{"old_units": float64(2)})
			require.NoError(t, err)
			want := int(2 * legacyTaskFixtureUnitsPerUSD)
			if expression == tailExpr {
				want = int(3 * legacyTaskFixtureUnitsPerUSD)
			}
			if expression == legacyExpr {
				want = int(4 * legacyTaskFixtureUnitsPerUSD)
			}
			assert.Equal(t, want, result.ActualQuotaAfterGroup)
		})
	}
}

func TestSharedTaskBillingExpressionSelectionAndFrozenSettlement(t *testing.T) {
	const baseExpr = `tier("base", u("seconds") * 2)`
	const alphaExpr = `tier("alpha", u("seconds") * 3)`
	const betaExpr = `tier("beta", u("credits") * 5)`
	const aliasExpr = `tier("alias", u("credits") * 7)`
	for _, tc := range []struct {
		name, plugin, model, mapping, modelExpr, mode, wantExpr string
		variants                                                map[string]string
		wantPriceError, profiled                                bool
	}{
		{name: "executing plugin override", plugin: "billing-beta", model: "declared-model", modelExpr: baseExpr, mode: "tiered_expr", variants: map[string]string{"billing-alpha::declared-model": alphaExpr, "billing-beta::declared-model": betaExpr}, wantExpr: betaExpr},
		{name: "override ignores model mode", plugin: "billing-beta", model: "declared-model", modelExpr: baseExpr, mode: "ratio", variants: map[string]string{"billing-beta::declared-model": betaExpr}, wantExpr: betaExpr},
		{name: "model expression fallback", plugin: "billing-alpha", model: "declared-model", modelExpr: baseExpr, mode: "tiered_expr", wantExpr: baseExpr},
		{name: "alias override precedes mapped override", plugin: "billing-beta", model: "alias-model", mapping: `{"alias-model":"declared-model"}`, modelExpr: baseExpr, mode: "tiered_expr", variants: map[string]string{"billing-beta::declared-model": betaExpr, "billing-beta::alias-model": aliasExpr}, wantExpr: aliasExpr},
		{name: "mapped override precedes model fallback", plugin: "billing-beta", model: "alias-model", mapping: `{"alias-model":"declared-model"}`, modelExpr: baseExpr, mode: "tiered_expr", variants: map[string]string{"billing-beta::declared-model": betaExpr}, wantExpr: betaExpr},
		{name: "unconfigured plugin cannot use another schema", plugin: "billing-beta", model: "declared-model", modelExpr: baseExpr, mode: "tiered_expr", wantPriceError: true},
		{name: "missing usage in skipped branch remains incompatible", plugin: "billing-beta", model: "declared-model", modelExpr: `true ? tier("free", 0) : tier("missing", u("seconds"))`, mode: "tiered_expr", wantPriceError: true},
		{name: "fixed pricing is still rejected", plugin: "billing-beta", model: "declared-model", variants: map[string]string{"billing-beta::declared-model": `tier("fixed", fixed(1))`}, wantPriceError: true},
		{name: "endpoint mapping keeps the declared profile", plugin: "billing-beta", model: "declared-model", mapping: `{"declared-model":"ep-endpoint"}`, modelExpr: baseExpr, mode: "tiered_expr", variants: map[string]string{"billing-beta::declared-model": betaExpr}, wantExpr: betaExpr, profiled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saveBillingConfig(t)
			registry := pluginruntime.NewRegistry()
			for _, spec := range []struct{ key, field, unit string }{{"billing-alpha", "seconds", "second"}, {"billing-beta", "credits", "credit"}} {
				source := strings.ReplaceAll(billingFallbackPlugin, "bill-fallback", spec.key)
				schema := `usageSchema:{` + spec.field + `:{type:"number",unit:"` + spec.unit + `"}}`
				if tc.profiled && spec.key == "billing-beta" {
					// The endpoint ID is undeclared, so only the declared model's profile carries this schema.
					schema = `usageSchema:{seconds:{type:"number",unit:"second"}},usageProfiles:[{models:["declared-model"],schema:{` + spec.field + `:{type:"number",unit:"` + spec.unit + `"}}}]`
				}
				source = strings.Replace(source, `fetchMode:"per_task"`, `fetchMode:"per_task",`+schema, 1)
				source += `export function extractUsage(){return {` + spec.field + `:2};}`
				_, err := registry.Register(source, pluginruntime.Options{})
				require.NoError(t, err)
			}
			variants := tc.variants
			if variants == nil {
				variants = map[string]string{}
			}
			rawVariants, err := common.Marshal(variants)
			require.NoError(t, err)
			modes, err := common.Marshal(map[string]string{"declared-model": tc.mode})
			require.NoError(t, err)
			expressions, err := common.Marshal(map[string]string{"declared-model": tc.modelExpr})
			require.NoError(t, err)
			require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
				billing_setting.PluginBillingExprOption: string(rawVariants), "billing_setting.billing_mode": string(modes), "billing_setting.billing_expr": string(expressions),
			}))
			c, info := newTaskSubmitContext(t, tc.model, tc.mapping)
			info.OriginModelName, info.UpstreamModelName = tc.model, tc.model
			require.NoError(t, helper.ModelMappedHelper(c, info, nil))
			plugin, ok := registry.Generation().Get(tc.plugin)
			require.True(t, ok)
			expression, found := billing_setting.ResolveTaskBillingExpr(tc.plugin, tc.model, info.UpstreamModelName)
			schema, _ := plugin.Meta.UsageForModels(info.UpstreamModelName, tc.model)
			valid := found && !billingexpr.UsesFixedPricing(expression) && billing_setting.TaskExprCompatible(expression, schema)
			if tc.wantPriceError {
				assert.False(t, valid, "incompatible historical pricing sources cannot become an implicit free price")
				return
			}
			require.True(t, valid)
			assert.Equal(t, tc.wantExpr, expression)
			field := "seconds"
			if tc.plugin == "billing-beta" {
				field = "credits"
			}
			facts := map[string]any{field: float64(2)}
			cost, _, err := billingexpr.RunExprWithRequest(expression, billingexpr.TokenParams{}, billingexpr.RequestInput{Usage: facts})
			require.NoError(t, err)
			// A released task stored this complete snapshot. Replaying it must
			// remain independent of current options and plugin schemas.
			snapshot := &billingexpr.BillingSnapshot{BillingMode: billing_setting.BillingModeTieredExpr, ModelName: tc.model, ExprString: expression, ExprHash: billingexpr.ExprHashString(expression), GroupRatio: 1, EstimatedQuotaBeforeGroup: cost * legacyTaskFixtureUnitsPerUSD, EstimatedQuotaAfterGroup: common.QuotaRound(cost * legacyTaskFixtureUnitsPerUSD), QuotaPerUnit: legacyTaskFixtureUnitsPerUSD, ExprVersion: billingexpr.ExprVersion(expression), TaskUsageBilling: true, UsageFacts: facts}
			require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{billing_setting.PluginBillingExprOption: `{}`, "billing_setting.billing_expr": `{}`}))
			result, usage, err := service.EvaluateTaskCompletionUsage(snapshot, map[string]any{field: float64(4)})
			require.NoError(t, err)
			assert.Equal(t, float64(4), usage[field])
			assert.Equal(t, float64(2), snapshot.UsageFacts[field])
			assert.Equal(t, 2*snapshot.EstimatedQuotaAfterGroup, result.ActualQuotaAfterGroup)
			assert.Equal(t, tc.wantExpr, snapshot.ExprString)
		})
	}
}
