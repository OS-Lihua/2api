package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/pkg/testdb"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	scope := &testdb.MainScope{}
	db := model.UseTestDatabases(scope)
	sqlDB, err := db.DB()
	if err != nil {
		panic("failed to get sql.DB: " + err.Error())
	}
	sqlDB.SetMaxOpenConns(1)

	common.RedisEnabled = false
	common.LogConsumeEnabled = true
	if err := i18n.Init(); err != nil {
		panic("failed to load locales: " + err.Error())
	}

	if err := db.AutoMigrate(
		&model.Task{},
		&model.User{},
		&model.Token{},
		&model.Log{},
		&model.Provider{},
		&model.Midjourney{},
		&model.TopUp{},
		&model.UserSubscription{},
		&model.LegacySubscriptionTaskObligation{},
		&model.LegacyTaskObligation{},
		&model.SystemTask{},
		&model.SystemTaskLock{},
	); err != nil {
		panic("failed to migrate: " + err.Error())
	}

	code := m.Run()
	scope.Close()
	os.Exit(code)
}

// ---------------------------------------------------------------------------
// Seed helpers
// ---------------------------------------------------------------------------

func truncate(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		model.DB.Exec("DELETE FROM tasks")
		model.DB.Exec("DELETE FROM users")
		model.DB.Exec("DELETE FROM tokens")
		model.ClearTestLogTable("logs")
		model.DB.Exec("DELETE FROM providers")
		model.DB.Exec("DELETE FROM midjourneys")
		model.DB.Exec("DELETE FROM top_ups")
		model.DB.Exec("DELETE FROM user_subscriptions")
		model.DB.Exec("DELETE FROM system_task_locks")
		model.DB.Exec("DELETE FROM system_tasks")
	})
}

func seedUser(t *testing.T, id int, quota int) {
	t.Helper()
	user := &model.User{Id: id, Username: "test_user", Quota: quota, Status: common.UserStatusEnabled}
	require.NoError(t, model.DB.Create(user).Error)
}

func seedToken(t *testing.T, id int, userId int, key string, remainQuota int) {
	t.Helper()
	token := &model.Token{
		Id:          id,
		UserId:      userId,
		Key:         key,
		Name:        "test_token",
		Status:      common.TokenStatusEnabled,
		RemainQuota: remainQuota,
		UsedQuota:   0,
	}
	require.NoError(t, model.DB.Create(token).Error)
}

func seedSubscriptionTaskMoney(t *testing.T, consumed int64) model.UserSubscription {
	t.Helper()
	model.UseTestDatabases(t)
	t.Setenv("GATEWAY_SETTLEMENT_JOURNAL_DIR", t.TempDir())
	require.NoError(t, model.DB.AutoMigrate(&model.User{}, &model.Token{}, &model.Provider{}, &model.Task{}, &model.Log{}, &model.Option{}))
	require.NoError(t, model.MigrateSubscriptionMoney(model.DB))
	require.NoError(t, model.MigrateGatewayBilling(model.DB))
	require.NoError(t, model.MigrateBalanceReminders(model.DB))
	require.NoError(t, model.MigrateGatewayRecords())
	seedUser(t, 77, 0)
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", 77).
		Update("setting", `{"billing_preference":"subscription_only"}`).Error)
	seedToken(t, 77, 77, "subscription-task-fixture", 8000)
	seedChannel(t, 77)
	_, err := model.TopUpWallet(model.WalletTopUp{UserId: 77, Source: money.Amount{Nanos: 8888, Currency: "USD"}, Reference: "unspent-task-wallet"})
	require.NoError(t, err)
	plan := model.SubscriptionPlan{
		Id: 7077, Title: "Task entitlement", Enabled: true, MoneyVersion: 1,
		Price: money.Amount{Currency: "USD"}, Allowance: money.Amount{Nanos: 100000, Currency: "USD"},
		DurationUnit: "day", DurationValue: 30, QuotaResetPeriod: "never",
	}
	require.NoError(t, model.DB.Create(&plan).Error)
	model.InvalidateSubscriptionPlanCache(plan.Id)
	t.Cleanup(func() { model.InvalidateSubscriptionPlanCache(plan.Id) })
	_, err = model.AdminBindSubscription(77, plan.Id, "task settlement fixture")
	require.NoError(t, err)
	var subscription model.UserSubscription
	require.NoError(t, model.DB.Where("user_id = ?", 77).Take(&subscription).Error)
	previous := model.GatewayRequest{ID: "previous-subscription-consumption", UserID: 77, SaleCurrency: "USD", SaleFXRate: "1", Status: "success"}
	amount := money.Amount{Nanos: consumed, Currency: "USD"}
	require.NoError(t, model.ReserveGateway(&previous, amount, amount))
	previous.Receivable = amount
	require.NoError(t, model.PrepareGatewaySettlement(model.GatewaySettlement{Request: previous, SaleSource: amount}))
	_, err = model.SettleGateway(previous.ID)
	require.NoError(t, err)
	return subscription
}

func seedChannel(t *testing.T, id int) {
	t.Helper()
	ch := &model.Provider{Id: id, Name: "test_channel", Key: "sk-test", Status: common.ChannelStatusEnabled}
	require.NoError(t, model.DB.Create(ch).Error)
}

func seedChargedAccounting(t *testing.T, userID, channelID, tokenID, quota, requestCount int) {
	t.Helper()
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", userID).Updates(map[string]any{
		"used_quota":    quota,
		"request_count": requestCount,
	}).Error)
	require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", channelID).
		Update("used_quota", quota).Error)
	if tokenID > 0 {
		require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", tokenID).
			Update("used_quota", quota).Error)
	}
}

func makeTask(userId, channelId, quota, tokenId int, billingSource string, subscriptionId int) *model.Task {
	return &model.Task{
		TaskID:    "task_" + time.Now().Format("150405.000"),
		UserId:    userId,
		ChannelId: channelId,
		Quota:     quota,
		Status:    model.TaskStatus(model.TaskStatusInProgress),
		Group:     "default",
		Data:      json.RawMessage(`{}`),
		CreatedAt: time.Now().Unix(),
		UpdatedAt: time.Now().Unix(),
		Properties: model.Properties{
			OriginModelName: "test-model",
		},
		PrivateData: model.TaskPrivateData{
			BillingSource:  billingSource,
			SubscriptionId: subscriptionId,
			TokenId:        tokenId,
			BillingContext: &model.TaskBillingContext{
				ModelPrice:      0.02,
				GroupRatio:      1.0,
				OriginModelName: "test-model",
			},
		},
	}
}

func TestPriceDataOtherRatiosFilterAndSnapshot(t *testing.T) {
	priceData := types.PriceData{}

	priceData.AddOtherRatio("zero", 0)
	priceData.AddOtherRatio("negative", -0.5)
	priceData.AddOtherRatio("nan", math.NaN())
	priceData.AddOtherRatio("inf", math.Inf(1))
	priceData.AddOtherRatio("one", 1)
	priceData.AddOtherRatio("positive", 2.5)

	ratios := priceData.OtherRatios()
	require.Len(t, ratios, 2)
	assert.Equal(t, 1.0, ratios["one"])
	assert.Equal(t, 2.5, ratios["positive"])
	assert.True(t, priceData.HasOtherRatio("one"))
	assert.False(t, priceData.HasOtherRatio("zero"))

	ratios["positive"] = 99
	ratios["new"] = 3
	nextSnapshot := priceData.OtherRatios()
	assert.Equal(t, 2.5, nextSnapshot["positive"])
	assert.NotContains(t, nextSnapshot, "new")
}

func TestPriceDataReplaceAndApplyOtherRatios(t *testing.T) {
	priceData := types.PriceData{}

	replaced := priceData.ReplaceOtherRatios(map[string]float64{
		"zero":     0,
		"negative": -3,
		"nan":      math.NaN(),
		"inf":      math.Inf(1),
		"one":      1,
		"duration": 2,
		"size":     1.5,
	})

	require.True(t, replaced)
	assert.Equal(t, 3.0, priceData.OtherRatioMultiplier())
	assert.Equal(t, 30.0, priceData.ApplyOtherRatiosToFloat(10))
	assert.Equal(t, 10.0, priceData.RemoveOtherRatiosFromFloat(30))
	assert.True(t, decimal.NewFromInt(30).Equal(priceData.ApplyOtherRatiosToDecimal(decimal.NewFromInt(10))))

	replaced = priceData.ReplaceOtherRatios(map[string]float64{
		"zero": 0,
		"nan":  math.NaN(),
	})

	require.False(t, replaced)
	assert.Nil(t, priceData.OtherRatios())
	assert.Equal(t, 1.0, priceData.OtherRatioMultiplier())
}

func TestTaskBillingOtherFiltersHistoricalOtherRatios(t *testing.T) {
	task := makeTask(1, 1, 100, 0, BillingSourceWallet, 0)
	task.PrivateData.BillingContext.OtherRatios = map[string]float64{
		"seconds":  2,
		"identity": 1,
		"zero":     0,
		"negative": -1,
		"nan":      math.NaN(),
		"inf":      math.Inf(1),
	}

	other := taskBillingOther(task).Snapshot()

	assert.Equal(t, 2.0, other["seconds"])
	assert.Equal(t, 1.0, other["identity"])
	assert.NotContains(t, other, "zero")
	assert.NotContains(t, other, "negative")
	assert.NotContains(t, other, "nan")
	assert.NotContains(t, other, "inf")
	assert.NotContains(t, other, "billing_mode")
	assert.NotContains(t, other, "expr_b64")
	assert.NotContains(t, other, "matched_tier")
	assert.NotContains(t, other, "usage_facts")
}

func TestTaskBillingOtherIncludesTieredSnapshotAndKeepsUsageFactsNested(t *testing.T) {
	task := makeTask(1, 1, 100, 0, BillingSourceWallet, 0)
	expression := `tier("720P", u("seconds") * 5)`
	task.PrivateData.BillingContext.TieredSnapshot = &billingexpr.BillingSnapshot{
		ExprString:    expression,
		EstimatedTier: "720P",
		UsageFacts: map[string]any{
			"resolution": "720P",
			"seconds":    5,
		},
	}

	other := taskBillingOther(task).Snapshot()

	assert.Equal(t, "tiered_expr", other["billing_mode"])
	assert.Equal(t, base64.StdEncoding.EncodeToString([]byte(expression)), other["expr_b64"])
	assert.Equal(t, "720P", other["matched_tier"])
	facts, ok := other["usage_facts"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, map[string]any{
		"resolution": "720P",
		"seconds":    5,
	}, facts)
	assert.NotContains(t, other, "resolution")
	assert.NotContains(t, other, "seconds")
}

func TestTaskBillingOtherOmitsEmptyUsageFacts(t *testing.T) {
	task := makeTask(1, 1, 100, 0, BillingSourceWallet, 0)
	expression := `tier("base", 1)`
	task.PrivateData.BillingContext.TieredSnapshot = &billingexpr.BillingSnapshot{
		ExprString:    expression,
		EstimatedTier: "base",
		UsageFacts:    map[string]any{},
	}

	other := taskBillingOther(task).Snapshot()

	assert.Equal(t, "tiered_expr", other["billing_mode"])
	assert.Equal(t, base64.StdEncoding.EncodeToString([]byte(expression)), other["expr_b64"])
	assert.Equal(t, "base", other["matched_tier"])
	assert.NotContains(t, other, "usage_facts")
}

func callLogTaskConsumption(t *testing.T, info *relaycommon.RelayInfo, task *model.Task) *model.Log {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	ctx.Set("token_name", "test_token")
	// Project and read immutable historical metadata without invoking a retired
	// consume-log producer or changing legacy account statistics.
	task.PrivateData.BillingContext.TieredSnapshot = info.TieredBillingSnapshot
	other := taskBillingOther(task)
	other.SetPublic("request_path", ctx.Request.URL.Path)
	parameters := "size: 2.00"
	if info.TieredBillingSnapshot != nil {
		parameters = "resolution: 720P, seconds: 5"
	}
	parts := []any{
		map[string]any{"key": "Action {{action}}", "params": map[string]any{"action": info.Action}},
		map[string]any{"key": "Billing parameters: {{params}}", "params": map[string]any{"params": parameters}},
	}
	other.SetPublic("content_parts", parts)
	fixture := model.Log{UserId: info.UserId, Type: model.LogTypeConsume, Quota: task.Quota,
		Content: "Action " + info.Action + ", Billing parameters: " + parameters, Other: other.JSONString()}
	require.NoError(t, model.LOG_DB.Create(&fixture).Error)
	logs, total, err := model.GetUserLogs(info.UserId, model.LogFilter{}, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Len(t, logs, 1)
	assert.Equal(t, 10000, getUserQuota(t, info.UserId))
	used, requests := getUserUsageAccounting(t, info.UserId)
	assert.Zero(t, used)
	assert.Zero(t, requests)
	assert.Zero(t, getChannelUsedQuota(t, task.ChannelId))
	return logs[0]
}

func TestLogTaskConsumptionIncludesTieredSnapshotUsageFacts(t *testing.T) {
	truncate(t)
	const userID, channelID = 40, 40
	seedUser(t, userID, 10_000)
	seedChannel(t, channelID)

	expression := `tier("720P", u("seconds") * 5)`
	task := makeTask(userID, channelID, 100, 0, BillingSourceWallet, 0)
	info := &relaycommon.RelayInfo{
		UserId:          userID,
		TokenId:         0,
		OriginModelName: "wan2.5-i2v-preview",
		UsingGroup:      "default",
		ProviderMeta:    &relaycommon.ProviderMeta{ChannelId: channelID},
		TaskRelayInfo:   &relaycommon.TaskRelayInfo{Action: "GENERATE"},
		PriceData: types.PriceData{
			ModelPrice:     0.02,
			Quota:          100,
			GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1},
		},
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			ExprString:    expression,
			EstimatedTier: "720P",
			UsageFacts: map[string]any{
				"resolution": "720P",
				"seconds":    5,
			},
		},
	}

	log := callLogTaskConsumption(t, info, task)

	var other map[string]any
	require.NoError(t, common.UnmarshalJsonStr(log.Other, &other))
	assert.Equal(t, "tiered_expr", other["billing_mode"])
	assert.Equal(t, base64.StdEncoding.EncodeToString([]byte(expression)), other["expr_b64"])
	assert.Equal(t, "720P", other["matched_tier"])
	facts, ok := other["usage_facts"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "720P", facts["resolution"])
	assert.Equal(t, float64(5), facts["seconds"])
	assert.NotContains(t, other, "resolution")
	assert.NotContains(t, other, "seconds")
	assert.Contains(t, log.Content, "Billing parameters: ")
	assert.Contains(t, log.Content, "resolution: 720P")
	assert.Contains(t, log.Content, "seconds: 5")
}

func TestLogTaskConsumptionWithoutSnapshotKeepsRatioMode(t *testing.T) {
	truncate(t)
	const userID, channelID = 41, 41
	seedUser(t, userID, 10_000)
	seedChannel(t, channelID)

	priceData := types.PriceData{
		ModelPrice:     0.02,
		Quota:          100,
		GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1},
	}
	priceData.AddOtherRatio("size", 2)
	task := makeTask(userID, channelID, 100, 0, BillingSourceWallet, 0)
	info := &relaycommon.RelayInfo{
		UserId:          userID,
		TokenId:         0,
		OriginModelName: "test-model",
		UsingGroup:      "default",
		ProviderMeta:    &relaycommon.ProviderMeta{ChannelId: channelID},
		TaskRelayInfo:   &relaycommon.TaskRelayInfo{Action: "GENERATE"},
		PriceData:       priceData,
	}

	log := callLogTaskConsumption(t, info, task)

	var other map[string]any
	require.NoError(t, common.UnmarshalJsonStr(log.Other, &other))
	assert.Equal(t, true, other["is_task"])
	assert.Equal(t, "/v1/videos", other["request_path"])
	assert.NotContains(t, other, "billing_mode")
	assert.NotContains(t, other, "expr_b64")
	assert.NotContains(t, other, "matched_tier")
	assert.NotContains(t, other, "usage_facts")
	assert.Equal(t, "Action GENERATE, Billing parameters: size: 2.00", log.Content)
	// The web console renders content_parts in the viewer's language; content
	// keeps the English text.
	assert.Equal(t, []any{
		map[string]any{"key": "Action {{action}}", "params": map[string]any{"action": "GENERATE"}},
		map[string]any{"key": "Billing parameters: {{params}}", "params": map[string]any{"params": "size: 2.00"}},
	}, other["content_parts"])
}

// Task logs distinguish jobs the client polls from requests whose HTTP call
// returned the deliverable itself, and flag results the gateway did not keep.
func TestLogTaskConsumptionMarksInlineResultsAndDiscardedArtifacts(t *testing.T) {
	for _, tc := range []struct {
		name               string
		status             model.TaskStatus
		discarded          bool
		pinnedProtocol     string
		wantSync, wantKept bool
	}{
		{"asynchronous job", model.TaskStatusNotStart, false, "", false, true},
		{"immediate result on a discarding route", model.TaskStatusSuccess, true, "", true, false},
		{"openai image request waits for an asynchronous upstream task", model.TaskStatusNotStart, false, jsplugin.ProtocolOpenAIImage, true, true},
		{"immediate failure", model.TaskStatusFailure, false, "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			truncate(t)
			const userID, channelID = 43, 43
			seedUser(t, userID, 10_000)
			seedChannel(t, channelID)
			task := makeTask(userID, channelID, 100, 0, BillingSourceWallet, 0)
			task.Status = tc.status
			task.PrivateData.ResultDiscarded = tc.discarded
			gin.SetMode(gin.TestMode)
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
			ctx.Set("token_name", "test_token")
			if tc.pinnedProtocol != "" {
				ctx.Set(jsplugin.ContextKeyPinnedEndpoint, jsplugin.PinnedEndpoint{Protocol: tc.pinnedProtocol})
			}
			projection := taskBillingOther(task)
			if taskDeliveredInline(ctx, task) {
				projection.SetPublic("task_sync", true)
			}
			other := projection.Snapshot()
			assert.Zero(t, countLogs(t))
			assert.Equal(t, 10000, getUserQuota(t, userID))
			assert.Equal(t, true, other["is_task"])
			if tc.wantSync {
				assert.Equal(t, true, other["task_sync"])
			} else {
				assert.NotContains(t, other, "task_sync")
			}
			if tc.wantKept {
				assert.NotContains(t, other, "result_discarded")
			} else {
				assert.Equal(t, true, other["result_discarded"])
			}
		})
	}
}

func TestTaskBillingOtherSeparatesPluginAndRootDiagnostics(t *testing.T) {
	task := makeTask(1, 1, 100, 0, BillingSourceWallet, 0)
	task.TaskID = "task_public"
	task.PrivateData.UpstreamTaskID = "upstream-private"
	task.PrivateData.NodeName = "node-a"
	task.PrivateData.Execution = &model.TaskExecutionSnapshot{
		TaskPlugin: &model.TaskPluginSnapshot{
			Key:     "document-parser",
			Name:    "Document Parser",
			Version: "1.2.3",
			Author: &model.TaskPluginAuthorSnapshot{
				Name: "Community Author",
				URL:  "https://plugins.example/author",
			},
			APIVersion: 1,
			Generation: 42,
		},
	}

	other := taskBillingOther(task).Snapshot()

	assert.Equal(t, "task_public", other["task_id"])
	adminInfo, ok := other["admin_info"].(map[string]any)
	require.True(t, ok)
	pluginInfo, ok := adminInfo["task_plugin"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "document-parser", pluginInfo["key"])
	assert.Equal(t, "1.2.3", pluginInfo["version"])
	assert.Equal(t, map[string]any{
		"name": "Community Author",
		"url":  "https://plugins.example/author",
	}, pluginInfo["author"])

	rootInfo, ok := other["root_info"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "upstream-private", rootInfo["upstream_task_id"])
	assert.Equal(t, "node-a", rootInfo["node_name"])
	runtimeInfo, ok := rootInfo["task_plugin"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, uint64(42), runtimeInfo["generation"])
	assert.NotContains(t, runtimeInfo, "author")
}

func TestTaskBillingContextPriceDataFiltersMultiplier(t *testing.T) {
	priceData := taskBillingContextPriceData(&model.TaskBillingContext{
		OtherRatios: map[string]float64{
			"seconds":  2,
			"size":     3,
			"identity": 1,
			"zero":     0,
			"negative": -1,
			"nan":      math.NaN(),
			"inf":      math.Inf(1),
		},
	})

	require.NotNil(t, priceData)
	assert.Equal(t, 6.0, priceData.OtherRatioMultiplier())
	assert.Equal(t, map[string]float64{
		"seconds":  2,
		"size":     3,
		"identity": 1,
	}, priceData.OtherRatios())
}

// ---------------------------------------------------------------------------
// Read-back helpers
// ---------------------------------------------------------------------------

func getUserQuota(t *testing.T, id int) int {
	t.Helper()
	var user model.User
	require.NoError(t, model.DB.Select("quota").Where("id = ?", id).First(&user).Error)
	return user.Quota
}

func getUserUsageAccounting(t *testing.T, id int) (int, int) {
	t.Helper()
	var user model.User
	require.NoError(t, model.DB.Select("used_quota", "request_count").Where("id = ?", id).First(&user).Error)
	return user.UsedQuota, user.RequestCount
}

func getChannelUsedQuota(t *testing.T, id int) int64 {
	t.Helper()
	var channel model.Provider
	require.NoError(t, model.DB.Select("used_quota").Where("id = ?", id).First(&channel).Error)
	return channel.UsedQuota
}

func getTokenRemainQuota(t *testing.T, id int) int {
	t.Helper()
	var token model.Token
	require.NoError(t, model.DB.Select("remain_quota").Where("id = ?", id).First(&token).Error)
	return token.RemainQuota
}

func getTokenUsedQuota(t *testing.T, id int) int {
	t.Helper()
	var token model.Token
	require.NoError(t, model.DB.Select("used_quota").Where("id = ?", id).First(&token).Error)
	return token.UsedQuota
}

func assertSubscriptionTaskSettlement(t *testing.T, reserved, actual int64, status model.TaskStatus) {
	t.Helper()
	subscription := seedSubscriptionTaskMoney(t, 50000-reserved)
	seedChargedAccounting(t, 77, 77, 77, 1234, 1)
	request := model.GatewayRequest{
		ID: "subscription-task-settlement", UserID: 77, TokenID: 77,
		SaleCurrency: "USD", SaleFXRate: "1", GroupRatio: "1",
		SaleExpression: `u("tokens") / 1000000000`, StartedAt: time.Now().UnixMilli(),
	}
	attempt := model.GatewayAttempt{
		ID: "subscription-task-attempt", RequestID: request.ID, ProviderID: 77,
		StartedAt: request.StartedAt, PurchaseExpression: `u("tokens") / 2000000000`,
		Cost: money.Amount{Currency: "USD"}, ConvertedCost: money.Amount{Currency: "USD"}, AccountingFXRate: "1",
	}
	task := model.Task{
		TaskID: "subscription-money-task", UserId: 77, ChannelId: 77, Platform: "sora", Status: model.TaskStatusInProgress,
		PrivateData: model.TaskPrivateData{TokenId: 77, Gateway: &model.TaskGatewaySnapshot{
			ProviderID: 77, Settlement: model.GatewaySettlement{Request: request, Attempt: &attempt},
			Facts: map[string]any{"tokens": reserved},
		}},
	}
	amount := money.Amount{Nanos: reserved, Currency: "USD"}
	require.NoError(t, model.ReserveGatewayTask(&request, amount, amount, &task))
	period, err := model.GetSubscriptionPeriodSummary(t.Context(), subscription.Id)
	require.NoError(t, err)
	assert.EqualValues(t, 50000-reserved, period.Consumed.Nanos)
	assert.Equal(t, amount, period.Reserved)
	assert.EqualValues(t, 50000, period.Available.Nanos)
	task.Status = status
	task.PrivateData.Gateway.Accepted = true
	task.PrivateData.Gateway.CompletionFacts = map[string]any{"tokens": actual}
	require.NoError(t, model.DB.Save(&task).Error)
	require.NoError(t, SettleGatewayTask(t.Context(), &task))
	var firstEntries []model.LedgerEntry
	require.NoError(t, model.DB.Order("id").Find(&firstEntries).Error)
	require.NoError(t, SettleGatewayTask(t.Context(), &task))
	var replayedEntries []model.LedgerEntry
	require.NoError(t, model.DB.Order("id").Find(&replayedEntries).Error)
	assert.Equal(t, firstEntries, replayedEntries, "terminal replay cannot refund or charge twice")
	period, err = model.GetSubscriptionPeriodSummary(t.Context(), subscription.Id)
	require.NoError(t, err)
	assert.Equal(t, subscription.CurrentPeriodID, period.PeriodID)
	assert.EqualValues(t, 50000-reserved+actual, period.Consumed.Nanos)
	assert.EqualValues(t, 50000+reserved-actual, period.Available.Nanos)
	assert.Equal(t, money.Amount{Currency: "USD"}, period.Reserved)
	wallet, err := model.GetUserWallet(77)
	require.NoError(t, err)
	assert.Equal(t, money.Amount{Nanos: 8888, Currency: "USD"}, wallet.Balance, "a subscription release must not credit the cash wallet")
	assert.Equal(t, 8000, getTokenRemainQuota(t, 77))
	assert.Equal(t, 1234, getTokenUsedQuota(t, 77))
	used, requests := getUserUsageAccounting(t, 77)
	assert.Equal(t, 1234, used)
	assert.Equal(t, 1, requests)
	assert.EqualValues(t, 1234, getChannelUsedQuota(t, 77))
	assert.Zero(t, getTaskQuota(t, task.ID), "money tasks never acquire a legacy quota refund marker")
	record, err := model.GetGatewayRequest(t.Context(), request.ID, 77)
	require.NoError(t, err)
	assert.Equal(t, request.Funding, record.Funding)
	assert.Equal(t, money.Amount{Nanos: actual, Currency: "USD"}, record.Receivable)
	assert.False(t, record.SalePending)
	wantStatus := "success"
	if status == model.TaskStatusFailure {
		wantStatus = "failed"
	}
	assert.Equal(t, wantStatus, record.Status)
	attempts, total, err := model.ListGatewayAttempts(t.Context(), request.ID, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Len(t, attempts, 1)
	assert.Equal(t, money.Amount{Nanos: actual / 2, Currency: "USD"}, attempts[0].Cost)
	assert.False(t, attempts[0].CostPending)
	assert.False(t, attempts[0].ConvertedCostPending)
	var postings []model.LedgerEntry
	require.NoError(t, model.DB.Where("request_id = ?", request.ID).Find(&postings).Error)
	var holds, releases, charges int
	for _, entry := range postings {
		switch entry.Type {
		case model.LedgerEntryPreConsume:
			holds++
			assert.Equal(t, -reserved, entry.Amount.Nanos)
		case model.LedgerEntryPreConsumeRelease:
			releases++
			assert.Equal(t, reserved, entry.Amount.Nanos)
		case model.LedgerEntryCharge:
			charges++
		}
	}
	assert.Equal(t, 1, holds)
	assert.Equal(t, 1, releases)
	if actual == 0 {
		assert.Zero(t, charges, "failed delivery must not recognize a charge or procurement from the estimate")
	} else {
		assert.Equal(t, 3, charges, "one entitlement debit, one revenue entry and one Provider cost")
	}
	reconciliation, err := model.ReconcileWallets(100)
	require.NoError(t, err)
	assert.Empty(t, reconciliation.Mismatches)
}

func seedCanonicalWalletTask(t *testing.T, reserved int64) *model.Task {
	t.Helper()
	return seedCanonicalWalletTaskWithPricing(t, reserved, `u("tokens") / 1000000000`, `u("tokens") / 2000000000`, map[string]any{"tokens": reserved})
}

func seedCanonicalWalletTaskWithPricing(t *testing.T, reserved int64, sale, purchase string, facts map[string]any, ratios ...string) *model.Task {
	t.Helper()
	seedSubscriptionTaskMoney(t, 0)
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", 77).Update("setting", `{"billing_preference":"wallet_only"}`).Error)
	seedChargedAccounting(t, 77, 77, 77, 1234, 1)
	groupRatio := "1"
	if len(ratios) != 0 {
		require.Len(t, ratios, 1)
		groupRatio = ratios[0]
	}
	request := model.GatewayRequest{ID: "wallet-task-settlement", UserID: 77, TokenID: 77,
		SaleCurrency: "USD", SaleFXRate: "1", GroupRatio: groupRatio, SaleExpression: sale, StartedAt: time.Now().UnixMilli()}
	attempt := model.GatewayAttempt{ID: "wallet-task-attempt", RequestID: request.ID, ProviderID: 77, StartedAt: request.StartedAt,
		PurchaseExpression: purchase, Cost: money.Amount{Currency: "USD"}, ConvertedCost: money.Amount{Currency: "USD"}, AccountingFXRate: "1"}
	task := &model.Task{TaskID: "wallet-money-task", UserId: 77, ChannelId: 77, Platform: "sora", Status: model.TaskStatusInProgress, Progress: "10%",
		PrivateData: model.TaskPrivateData{TokenId: 77, Gateway: &model.TaskGatewaySnapshot{ProviderID: 77,
			Settlement: model.GatewaySettlement{Request: request, Attempt: &attempt}, Facts: facts}}}
	amount := money.Amount{Nanos: reserved, Currency: "USD"}
	require.NoError(t, model.ReserveGatewayTask(&request, amount, amount, task))
	task.PrivateData.Gateway.Accepted = true
	require.NoError(t, model.DB.Save(task).Error)
	wallet, err := model.GetUserWallet(77)
	require.NoError(t, err)
	assert.Equal(t, money.Amount{Nanos: 8888 - reserved, Currency: "USD"}, wallet.Balance)
	assert.Equal(t, "wallet", request.Funding.Source)
	return task
}

func assertCanonicalWalletTaskAccounting(t *testing.T, task *model.Task, reserved, actual, cost int64) {
	t.Helper()
	wallet, err := model.GetUserWallet(77)
	require.NoError(t, err)
	assert.Equal(t, money.Amount{Nanos: 8888 - actual, Currency: "USD"}, wallet.Balance)
	assert.Equal(t, 8000, getTokenRemainQuota(t, 77))
	assert.Equal(t, 1234, getTokenUsedQuota(t, 77))
	used, requests := getUserUsageAccounting(t, 77)
	assert.Equal(t, 1234, used)
	assert.Equal(t, 1, requests)
	assert.EqualValues(t, 1234, getChannelUsedQuota(t, 77))
	assert.Zero(t, getTaskQuota(t, task.ID))
	requestID := task.PrivateData.Gateway.Settlement.Request.ID
	record, err := model.GetGatewayRequest(t.Context(), requestID, 77)
	require.NoError(t, err)
	assert.Equal(t, money.Amount{Nanos: actual, Currency: "USD"}, record.Receivable)
	assert.False(t, record.SalePending)
	assert.Equal(t, "wallet", record.Funding.Source)
	attempts, total, err := model.ListGatewayAttempts(t.Context(), requestID, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	assert.Equal(t, money.Amount{Nanos: cost, Currency: "USD"}, attempts[0].Cost)
	assert.False(t, attempts[0].CostPending)
	var entries []model.LedgerEntry
	require.NoError(t, model.DB.Where("request_id = ?", requestID).Order("id").Find(&entries).Error)
	holds, releases := 0, 0
	for _, entry := range entries {
		if entry.Type == model.LedgerEntryPreConsume {
			holds++
			assert.Equal(t, -reserved, entry.Amount.Nanos)
		}
		if entry.Type == model.LedgerEntryPreConsumeRelease {
			releases++
			assert.Equal(t, reserved, entry.Amount.Nanos)
		}
	}
	if reserved == 0 {
		assert.Zero(t, holds)
		assert.Zero(t, releases)
	} else {
		assert.Equal(t, 1, holds)
		assert.Equal(t, 1, releases)
	}
	require.NoError(t, SettleGatewayTask(t.Context(), task))
	var replay []model.LedgerEntry
	require.NoError(t, model.DB.Where("request_id = ?", requestID).Order("id").Find(&replay).Error)
	assert.Equal(t, entries, replay, "a replay cannot duplicate a refund, differential charge or procurement")
	reconciliation, err := model.ReconcileWallets(100)
	require.NoError(t, err)
	assert.Empty(t, reconciliation.Mismatches)
}

func assertCanonicalWalletTaskSettlement(t *testing.T, reserved, actual int64, status model.TaskStatus) {
	t.Helper()
	task := seedCanonicalWalletTask(t, reserved)
	simulatePollBilling(t.Context(), task, status, int(actual))
	require.NoError(t, SettleGatewayTask(t.Context(), task))
	var persisted model.Task
	require.NoError(t, model.DB.First(&persisted, task.ID).Error)
	assert.Equal(t, status, persisted.Status)
	assertCanonicalWalletTaskAccounting(t, task, reserved, actual, actual/2)
}

func getTaskQuota(t *testing.T, id int64) int {
	t.Helper()
	var task model.Task
	require.NoError(t, model.DB.Select("quota").Where("id = ?", id).First(&task).Error)
	return task.Quota
}

func getMidjourneyTask(t *testing.T, id int) model.Midjourney {
	t.Helper()
	var task model.Midjourney
	require.NoError(t, model.DB.First(&task, id).Error)
	return task
}

func getLastLog(t *testing.T) *model.Log {
	t.Helper()
	var log model.Log
	err := model.LOG_DB.Order("id desc").First(&log).Error
	if err != nil {
		return nil
	}
	return &log
}

func countLogs(t *testing.T) int64 {
	t.Helper()
	var count int64
	model.LOG_DB.Model(&model.Log{}).Count(&count)
	return count
}

// ===========================================================================
// Legacy Midjourney billing tests
// ===========================================================================

func TestLegacyMidjourneyRefundRequiresVerifiedMoney(t *testing.T) {
	for _, quota := range []int{0, 3000} {
		t.Run(fmt.Sprint(quota), func(t *testing.T) {
			seedSubscriptionTaskMoney(t, 0)
			require.NoError(t, model.DB.AutoMigrate(&model.Midjourney{}))
			seedChargedAccounting(t, 77, 77, 77, 1234, 1)
			task := &model.Midjourney{UserId: 77, ChannelId: 77, BillingChannelId: 77, TokenId: 77, MjId: "unverified-mj-refund", Quota: quota, Action: "IMAGINE"}
			require.NoError(t, task.Insert())
			var beforeUser model.User
			var beforeToken model.Token
			var beforeProvider model.Provider
			var beforeEntries []model.LedgerEntry
			require.NoError(t, model.DB.First(&beforeUser, 77).Error)
			require.NoError(t, model.DB.First(&beforeToken, 77).Error)
			require.NoError(t, model.DB.First(&beforeProvider, 77).Error)
			require.NoError(t, model.DB.Order("id").Find(&beforeEntries).Error)
			beforeTask := getMidjourneyTask(t, task.Id)
			for range 2 {
				assert.False(t, RefundMidjourneyQuota(t.Context(), task, "historical failure has no verified money receipt"))
			}
			var afterUser model.User
			var afterToken model.Token
			var afterProvider model.Provider
			var afterEntries []model.LedgerEntry
			require.NoError(t, model.DB.First(&afterUser, 77).Error)
			require.NoError(t, model.DB.First(&afterToken, 77).Error)
			require.NoError(t, model.DB.First(&afterProvider, 77).Error)
			require.NoError(t, model.DB.Order("id").Find(&afterEntries).Error)
			assert.Equal(t, beforeUser, afterUser)
			assert.Equal(t, beforeToken, afterToken)
			assert.Equal(t, beforeProvider, afterProvider)
			assert.Equal(t, beforeEntries, afterEntries)
			assert.Equal(t, beforeTask, getMidjourneyTask(t, task.Id))
			assert.Equal(t, quota, task.Quota)
			assert.Zero(t, countLogs(t), "an unverified obligation cannot produce a refund receipt")
		})
	}
}

func seedCanonicalMidjourneyTask(t *testing.T, reservation int64) *model.Task {
	t.Helper()
	task := seedCanonicalWalletTaskWithPricing(t, reservation, `u("operations") / 1000000000`, `u("operations") / 2000000000`, map[string]any{"operations": reservation})
	task.Platform = "midjourney"
	require.NoError(t, model.DB.Save(task).Error)
	return task
}

func TestPrepareMidjourneyTaskBillingKeepsUnbilledMarkerClear(t *testing.T) {
	task := seedCanonicalMidjourneyTask(t, 0)
	assert.Zero(t, task.Quota)
	var count int64
	require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("request_id = ?", "wallet-task-settlement").Count(&count).Error)
	assert.Zero(t, count, "a zero money reservation must not create a fake int marker or zero ledger entry")
	task.Status = model.TaskStatusFailure
	task.PrivateData.Gateway.CompletionFacts = map[string]any{"operations": 0}
	require.NoError(t, model.DB.Save(task).Error)
	require.NoError(t, SettleGatewayTask(t.Context(), task))
	assertCanonicalWalletTaskAccounting(t, task, 0, 0, 0)
}

func TestSettleMidjourneyTaskBillingRequiresPersistedTask(t *testing.T) {
	seedSubscriptionTaskMoney(t, 0)
	task := &model.Task{TaskID: "unpersisted-midjourney", UserId: 77, ChannelId: 77, Platform: "midjourney", Status: model.TaskStatusFailure,
		PrivateData: model.TaskPrivateData{Gateway: &model.TaskGatewaySnapshot{Settlement: model.GatewaySettlement{
			Request: model.GatewayRequest{ID: "no-durable-midjourney-reservation", UserID: 77},
		}}}}
	var before, after []model.LedgerEntry
	require.NoError(t, model.DB.Order("id").Find(&before).Error)
	require.Error(t, SettleGatewayTask(t.Context(), task))
	require.NoError(t, model.DB.Order("id").Find(&after).Error)
	assert.Equal(t, before, after)
	wallet, err := model.GetUserWallet(77)
	require.NoError(t, err)
	assert.Equal(t, money.Amount{Nanos: 8888, Currency: "USD"}, wallet.Balance)
	assert.Equal(t, 8000, getTokenRemainQuota(t, 77))
}

func TestMidjourneyRefundRestoresEveryAccountingElementOnBillingChannel(t *testing.T) {
	task := seedCanonicalMidjourneyTask(t, 3000)
	seedChannel(t, 78) // An unrelated current Provider must not receive this task's procurement.
	task.Status = model.TaskStatusFailure
	task.Progress = "100%"
	task.PrivateData.Gateway.CompletionFacts = map[string]any{"operations": 100}
	require.NoError(t, model.DB.Save(task).Error)
	require.NoError(t, SettleGatewayTask(t.Context(), task))
	assertCanonicalWalletTaskAccounting(t, task, 3000, 0, 50)
	var wrongProvider int64
	require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Joins(`JOIN "wallets" ON "wallets"."id" = "ledger_entries"."wallet_id"`).Where(`"ledger_entries"."request_id" = ? AND "ledger_entries"."account_type" = ? AND "wallets"."owner_id" = ?`, "wallet-task-settlement", model.LedgerAccountProviderPayable, 78).Count(&wrongProvider).Error)
	assert.Zero(t, wrongProvider)
	var before, after []model.LedgerEntry
	require.NoError(t, model.DB.Order("id").Find(&before).Error)
	errors := make(chan error, 8)
	for range 8 {
		go func() { errors <- SettleGatewayTask(t.Context(), task) }()
	}
	for range 8 {
		require.NoError(t, <-errors)
	}
	require.NoError(t, model.DB.Order("id").Find(&after).Error)
	assert.Equal(t, before, after, "concurrent repeated failure reports release the original hold only once")
	assert.Zero(t, countLogs(t), "canonical settlement must not emit an int refund receipt")
}

func TestSettleMidjourneyTaskBillingFundingFailureClearsMarkers(t *testing.T) {
	seedSubscriptionTaskMoney(t, 0)
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", 77).Update("setting", `{"billing_preference":"wallet_only"}`).Error)
	request := model.GatewayRequest{ID: "midjourney-reservation-failure", UserID: 77, TokenID: 77, SaleCurrency: "USD", SaleFXRate: "1", GroupRatio: "1"}
	attempt := &model.GatewayAttempt{ID: "midjourney-reservation-attempt", RequestID: request.ID, ProviderID: 77, PurchaseExpression: `u("operations") * 0`, Cost: money.Amount{Currency: "USD"}, ConvertedCost: money.Amount{Currency: "USD"}, AccountingFXRate: "1"}
	task := &model.Task{TaskID: "midjourney-reservation-task", UserId: 77, ChannelId: 77, Platform: "midjourney", Status: model.TaskStatusInProgress,
		PrivateData: model.TaskPrivateData{Gateway: &model.TaskGatewaySnapshot{ProviderID: 77, Settlement: model.GatewaySettlement{Request: request, Attempt: attempt}}}}
	amount := money.Amount{Nanos: 3000, Currency: "USD"}
	require.NoError(t, model.DB.Exec(`CREATE FUNCTION reject_midjourney_reserve() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW."request_id" = 'midjourney-reservation-failure' AND NEW."type" = 'pre_consume' THEN RAISE EXCEPTION 'reservation rejected'; END IF; RETURN NEW; END $$`).Error)
	require.NoError(t, model.DB.Exec(`CREATE TRIGGER reject_midjourney_reserve BEFORE INSERT ON "ledger_entries" FOR EACH ROW EXECUTE FUNCTION reject_midjourney_reserve()`).Error)
	var before, after []model.LedgerEntry
	require.NoError(t, model.DB.Order("id").Find(&before).Error)
	require.Error(t, model.ReserveGatewayTask(&request, amount, amount, task))
	require.NoError(t, model.DB.Order("id").Find(&after).Error)
	assert.Equal(t, before, after)
	var tasks, billings int64
	require.NoError(t, model.DB.Model(&model.Task{}).Where("task_id = ?", task.TaskID).Count(&tasks).Error)
	require.NoError(t, model.DB.Model(&model.GatewayBilling{}).Where("request_id = ?", request.ID).Count(&billings).Error)
	assert.Zero(t, tasks)
	assert.Zero(t, billings)
	wallet, err := model.GetUserWallet(77)
	require.NoError(t, err)
	assert.Equal(t, money.Amount{Nanos: 8888, Currency: "USD"}, wallet.Balance)
	assert.Equal(t, 8000, getTokenRemainQuota(t, 77))
	require.NoError(t, model.DB.Exec(`DROP TRIGGER reject_midjourney_reserve ON "ledger_entries"`).Error)
	require.NoError(t, model.ReserveGatewayTask(&request, amount, amount, task))
	task.Status = model.TaskStatusFailure
	task.PrivateData.Gateway.Accepted = true
	task.PrivateData.Gateway.CompletionFacts = map[string]any{"operations": 0}
	require.NoError(t, model.DB.Save(task).Error)
	require.NoError(t, SettleGatewayTask(t.Context(), task))
	require.NoError(t, SettleGatewayTask(t.Context(), task))
	wallet, err = model.GetUserWallet(77)
	require.NoError(t, err)
	assert.Equal(t, money.Amount{Nanos: 8888, Currency: "USD"}, wallet.Balance)
}

func TestSettleMidjourneyTaskBillingTokenFailureKeepsFundingRefundable(t *testing.T) {
	task := seedCanonicalMidjourneyTask(t, 3000)
	// Retired Token counters are not an accounting stage. Rejecting their writes
	// must not break settlement; a real procurement failure must roll back all money.
	failRowUpdates(t, "tokens", 77, "retired token quota writes forbidden")
	task.Status = model.TaskStatusFailure
	task.PrivateData.Gateway.CompletionFacts = map[string]any{"operations": 100}
	require.NoError(t, model.DB.Save(task).Error)
	require.NoError(t, model.DB.Exec(`CREATE FUNCTION reject_midjourney_cost() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW."request_id" = 'wallet-task-settlement' AND NEW."account_type" = 'provider_payable' THEN RAISE EXCEPTION 'procurement rejected'; END IF; RETURN NEW; END $$`).Error)
	require.NoError(t, model.DB.Exec(`CREATE TRIGGER reject_midjourney_cost BEFORE INSERT ON "ledger_entries" FOR EACH ROW EXECUTE FUNCTION reject_midjourney_cost()`).Error)
	var before, after []model.LedgerEntry
	require.NoError(t, model.DB.Order("id").Find(&before).Error)
	require.Error(t, SettleGatewayTask(t.Context(), task))
	require.NoError(t, model.DB.Order("id").Find(&after).Error)
	assert.Equal(t, before, after)
	wallet, err := model.GetUserWallet(77)
	require.NoError(t, err)
	assert.Equal(t, money.Amount{Nanos: 5888, Currency: "USD"}, wallet.Balance)
	var billing model.GatewayBilling
	require.NoError(t, model.DB.Take(&billing, "request_id = ?", "wallet-task-settlement").Error)
	assert.Equal(t, "settlement_pending", billing.Phase)
	assert.Equal(t, 8000, getTokenRemainQuota(t, 77))
	require.NoError(t, model.DB.Exec(`DROP TRIGGER reject_midjourney_cost ON "ledger_entries"`).Error)
	require.NoError(t, SettleGatewayTask(t.Context(), task))
	assertCanonicalWalletTaskAccounting(t, task, 3000, 0, 50)
}

func TestPrepareMidjourneyTaskBillingRejectsSubscriptionBeforeCharge(t *testing.T) {
	// The old int helper rejected subscriptions entirely. The actual contract is
	// now an exact original-period refund, with no wallet or Token fallback.
	assertSubscriptionTaskSettlement(t, 3000, 0, model.TaskStatusFailure)
}

func TestRefundMidjourneyQuotaUsesLegacyChannelFallbackWithoutTokenAdjustment(t *testing.T) {
	task := seedCanonicalMidjourneyTask(t, 3000)
	require.NoError(t, model.DB.Delete(&model.Token{}, 77).Error)
	task.Status = model.TaskStatusFailure
	task.PrivateData.Gateway.CompletionFacts = map[string]any{"operations": 100}
	require.NoError(t, model.DB.Save(task).Error)
	require.NoError(t, SettleGatewayTask(t.Context(), task))
	wallet, err := model.GetUserWallet(77)
	require.NoError(t, err)
	assert.Equal(t, money.Amount{Nanos: 8888, Currency: "USD"}, wallet.Balance)
	var before, after []model.LedgerEntry
	require.NoError(t, model.DB.Where("request_id = ?", "wallet-task-settlement").Order("id").Find(&before).Error)
	var releases, payable int
	for _, entry := range before {
		if entry.Type == model.LedgerEntryPreConsumeRelease {
			releases++
			assert.EqualValues(t, 3000, entry.Amount.Nanos)
		}
		if entry.AccountType == model.LedgerAccountProviderPayable {
			payable++
			var account model.Wallet
			require.NoError(t, model.DB.First(&account, entry.WalletId).Error)
			assert.Equal(t, 77, account.OwnerId)
			assert.EqualValues(t, 50, entry.Amount.Nanos)
		}
	}
	assert.Equal(t, 1, releases)
	assert.Equal(t, 1, payable)
	require.NoError(t, SettleGatewayTask(t.Context(), task))
	require.NoError(t, model.DB.Where("request_id = ?", "wallet-task-settlement").Order("id").Find(&after).Error)
	assert.Equal(t, before, after)
	assert.Zero(t, countLogs(t))
}

// ===========================================================================
// RefundTaskQuota tests
// ===========================================================================

func TestLegacyTaskIntBillingEntrypointsRetired(t *testing.T) {
	for _, source := range []string{"", BillingSourceWallet, BillingSourceSubscription, "unknown-source"} {
		for _, operation := range []string{"refund", "zero_refund", "increase", "decrease", "exact", "tokens"} {
			t.Run(source+"/"+operation, func(t *testing.T) {
				seedSubscriptionTaskMoney(t, 0)
				seedChargedAccounting(t, 77, 77, 77, 5000, 1)
				task := makeTask(77, 77, 5000, 77, source, 0)
				if operation == "zero_refund" {
					task.Quota = 0
				}
				task.Platform = "sora"
				task.PrivateData.Key = "must-not-be-logged-task-credential"
				require.NoError(t, model.DB.Create(task).Error)
				var beforeTask model.Task
				var beforeUser model.User
				var beforeToken model.Token
				var beforeProvider model.Provider
				var beforeLedger []model.LedgerEntry
				require.NoError(t, model.DB.First(&beforeTask, task.ID).Error)
				require.NoError(t, model.DB.First(&beforeUser, 77).Error)
				require.NoError(t, model.DB.First(&beforeToken, 77).Error)
				require.NoError(t, model.DB.First(&beforeProvider, 77).Error)
				require.NoError(t, model.DB.Order("id").Find(&beforeLedger).Error)
				for range 2 {
					switch operation {
					case "refund", "zero_refund":
						assert.False(t, RefundTaskQuota(t.Context(), task, "unverified historical failure"))
					case "increase":
						RecalculateTaskQuota(t.Context(), task, 6000, common.NewMessage("legacy adjustment"))
					case "decrease":
						RecalculateTaskQuota(t.Context(), task, 2000, common.NewMessage("legacy adjustment"))
					case "exact":
						RecalculateTaskQuota(t.Context(), task, task.Quota, common.NewMessage("legacy adjustment"))
					case "tokens":
						assert.False(t, RecalculateTaskQuotaByTokens(t.Context(), task, 8000))
					}
				}
				var afterTask model.Task
				var afterUser model.User
				var afterToken model.Token
				var afterProvider model.Provider
				var afterLedger []model.LedgerEntry
				require.NoError(t, model.DB.First(&afterTask, task.ID).Error)
				require.NoError(t, model.DB.First(&afterUser, 77).Error)
				require.NoError(t, model.DB.First(&afterToken, 77).Error)
				require.NoError(t, model.DB.First(&afterProvider, 77).Error)
				require.NoError(t, model.DB.Order("id").Find(&afterLedger).Error)
				assert.Equal(t, beforeTask, afterTask)
				assert.Equal(t, beforeTask.Quota, task.Quota)
				assert.Equal(t, beforeUser, afterUser)
				assert.Equal(t, beforeToken, afterToken)
				assert.Equal(t, beforeProvider, afterProvider)
				assert.Equal(t, beforeLedger, afterLedger)
				assert.Zero(t, countLogs(t), "retired entrypoints cannot claim a successful refund or charge")
			})
		}
	}
}

func TestRefundTaskQuota_Wallet(t *testing.T) {
	assertCanonicalWalletTaskSettlement(t, 3000, 0, model.TaskStatusFailure)
}

func TestRefundTaskQuota_Subscription(t *testing.T) {
	// The same failed-task refund now releases its exact original period hold,
	// rather than mutating retired subscription and Token quota counters.
	assertSubscriptionTaskSettlement(t, 2000, 0, model.TaskStatusFailure)
}

func TestRefundTaskQuota_ZeroQuota(t *testing.T) {
	assertCanonicalWalletTaskSettlement(t, 0, 0, model.TaskStatusFailure)
}

func TestRefundTaskQuota_NoToken(t *testing.T) {
	task := seedCanonicalWalletTask(t, 1500)
	require.NoError(t, model.DB.Delete(&model.Token{}, 77).Error)
	task.PrivateData.TokenId = 0
	simulatePollBilling(t.Context(), task, model.TaskStatusFailure, 0)
	require.NoError(t, SettleGatewayTask(t.Context(), task))
	wallet, err := model.GetUserWallet(77)
	require.NoError(t, err)
	assert.Equal(t, money.Amount{Nanos: 8888, Currency: "USD"}, wallet.Balance)
	var entries []model.LedgerEntry
	require.NoError(t, model.DB.Where("request_id = ?", "wallet-task-settlement").Find(&entries).Error)
	require.Len(t, entries, 2)
	assert.Equal(t, model.LedgerEntryPreConsume, entries[0].Type)
	assert.Equal(t, model.LedgerEntryPreConsumeRelease, entries[1].Type)
	assert.Zero(t, getTaskQuota(t, task.ID))
}

func TestRefundTaskQuota_FundingFailureKeepsAccountingAndPendingMarker(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, channelID, preConsumed = 5, 5, 1200
	seedUser(t, userID, 5000)
	seedChannel(t, channelID)
	seedChargedAccounting(t, userID, channelID, 0, preConsumed, 1)
	task := makeTask(userID, channelID, preConsumed, 0, BillingSourceSubscription, 9999)
	task.Status = model.TaskStatusFailure
	require.NoError(t, model.DB.Create(task).Error)

	assert.False(t, RefundTaskQuota(ctx, task, "subscription missing"))
	assert.Equal(t, 5000, getUserQuota(t, userID))
	assert.Equal(t, preConsumed, task.Quota)
	assert.Equal(t, preConsumed, getTaskQuota(t, task.ID))
	usedQuota, requestCount := getUserUsageAccounting(t, userID)
	assert.Equal(t, preConsumed, usedQuota)
	assert.Equal(t, 1, requestCount)
	assert.Equal(t, int64(preConsumed), getChannelUsedQuota(t, channelID))
	assert.Equal(t, int64(0), countLogs(t))
}

func TestCanonicalTaskSettlementFailureRollsBackAndRecovers(t *testing.T) {
	for _, point := range []string{"release", "procurement"} {
		t.Run(point, func(t *testing.T) {
			task := seedCanonicalWalletTask(t, 1200)
			task.Status = model.TaskStatusFailure
			task.Progress = "100%"
			task.PrivateData.Gateway.CompletionFacts = map[string]any{"tokens": 100}
			require.NoError(t, model.DB.Save(task).Error)
			condition := `NEW."type" = 'pre_consume_release'`
			if point == "procurement" {
				condition = `NEW."account_type" = 'provider_payable'`
			}
			require.NoError(t, model.DB.Exec(`CREATE FUNCTION reject_task_settlement() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW."request_id" = 'wallet-task-settlement' AND `+condition+` THEN RAISE EXCEPTION 'task settlement write rejected'; END IF; RETURN NEW; END $$`).Error)
			require.NoError(t, model.DB.Exec(`CREATE TRIGGER reject_task_settlement BEFORE INSERT ON "ledger_entries" FOR EACH ROW EXECUTE FUNCTION reject_task_settlement()`).Error)
			var before []model.LedgerEntry
			require.NoError(t, model.DB.Order("id").Find(&before).Error)
			require.Error(t, SettleGatewayTask(t.Context(), task))
			var after []model.LedgerEntry
			require.NoError(t, model.DB.Order("id").Find(&after).Error)
			assert.Equal(t, before, after)
			wallet, err := model.GetUserWallet(77)
			require.NoError(t, err)
			assert.Equal(t, money.Amount{Nanos: 7688, Currency: "USD"}, wallet.Balance)
			assert.Equal(t, 8000, getTokenRemainQuota(t, 77))
			assert.Equal(t, 1234, getTokenUsedQuota(t, 77))
			assert.Zero(t, countLogs(t))
			var billing model.GatewayBilling
			require.NoError(t, model.DB.First(&billing, "request_id = ?", "wallet-task-settlement").Error)
			assert.Equal(t, "settlement_pending", billing.Phase)
			require.NoError(t, model.DB.Exec(`DROP TRIGGER reject_task_settlement ON "ledger_entries"`).Error)
			require.NoError(t, SettleGatewayTask(t.Context(), task))
			assertCanonicalWalletTaskAccounting(t, task, 1200, 0, 50)
		})
	}
}

// ===========================================================================
// RecalculateTaskQuota tests
// ===========================================================================

func TestRecalculate_PositiveDelta(t *testing.T) {
	assertCanonicalWalletTaskSettlement(t, 2000, 3000, model.TaskStatusSuccess)
}

func TestRecalculate_NegativeDelta(t *testing.T) {
	assertCanonicalWalletTaskSettlement(t, 5000, 3000, model.TaskStatusSuccess)
}

func TestRecalculate_ZeroDelta(t *testing.T) {
	assertCanonicalWalletTaskSettlement(t, 3000, 3000, model.TaskStatusSuccess)
}

func TestRecalculate_ActualQuotaZero(t *testing.T) {
	assertCanonicalWalletTaskSettlement(t, 5000, 0, model.TaskStatusSuccess)
}

func TestRecalculate_RejectsNegativeActualQuota(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, preConsumed = 34, 5000
	const initQuota = 10000
	seedUser(t, userID, initQuota)
	task := makeTask(userID, 0, preConsumed, 0, BillingSourceWallet, 0)

	RecalculateTaskQuota(ctx, task, -1, common.NewMessage("invalid negative actual"))

	assert.Equal(t, initQuota, getUserQuota(t, userID))
	assert.Equal(t, preConsumed, task.Quota)
	assert.Equal(t, int64(0), countLogs(t))
}

func TestRecalculate_Subscription_NegativeDelta(t *testing.T) {
	// Completed usage reduces the frozen 5000-nano hold to a 2000-nano charge.
	assertSubscriptionTaskSettlement(t, 5000, 2000, model.TaskStatusSuccess)
}

func TestLegacySubscriptionTaskBillingPreservesUnverifiedObligations(t *testing.T) {
	for _, refund := range []bool{true, false} {
		t.Run(fmt.Sprintf("refund=%t", refund), func(t *testing.T) {
			subscription := seedSubscriptionTaskMoney(t, 50000)
			seedChargedAccounting(t, 77, 77, 77, 5000, 1)
			task := makeTask(77, 77, 5000, 77, BillingSourceSubscription, subscription.Id)
			task.Platform = "sora"
			require.NoError(t, model.DB.Create(task).Error)
			require.NoError(t, model.MigrateSubscriptionMoney(model.DB))
			var evidence model.LegacySubscriptionTaskObligation
			require.NoError(t, model.DB.Where("source_task_id = ?", task.ID).Take(&evidence).Error)
			require.Equal(t, "unverified", evidence.State)
			var before []model.LedgerEntry
			require.NoError(t, model.DB.Order("id").Find(&before).Error)
			var savedTask model.Task
			require.NoError(t, model.DB.First(&savedTask, task.ID).Error)
			for range 2 {
				if refund {
					assert.False(t, RefundTaskQuota(t.Context(), task, "legacy subscription task failed"))
				} else {
					RecalculateTaskQuota(t.Context(), task, 2000, common.NewMessage("legacy subscription over-charge"))
				}
			}
			var after []model.LedgerEntry
			require.NoError(t, model.DB.Order("id").Find(&after).Error)
			assert.Equal(t, before, after, "unverified quota cannot become a new refund or charge")
			var retained model.LegacySubscriptionTaskObligation
			require.NoError(t, model.DB.Where("source_task_id = ?", task.ID).Take(&retained).Error)
			assert.Equal(t, evidence, retained)
			var retainedTask model.Task
			require.NoError(t, model.DB.First(&retainedTask, task.ID).Error)
			assert.Equal(t, savedTask, retainedTask)
			assert.Equal(t, 5000, task.Quota, "failure cannot clear the pending legacy marker")
			period, err := model.GetSubscriptionPeriodSummary(t.Context(), subscription.Id)
			require.NoError(t, err)
			assert.EqualValues(t, 50000, period.Consumed.Nanos)
			assert.EqualValues(t, 50000, period.Available.Nanos)
			assert.Zero(t, period.Reserved.Nanos)
			wallet, err := model.GetUserWallet(77)
			require.NoError(t, err)
			assert.Equal(t, money.Amount{Nanos: 8888, Currency: "USD"}, wallet.Balance)
			assert.Equal(t, 8000, getTokenRemainQuota(t, 77))
			assert.Equal(t, 5000, getTokenUsedQuota(t, 77))
			used, requests := getUserUsageAccounting(t, 77)
			assert.Equal(t, 5000, used)
			assert.Equal(t, 1, requests)
			assert.EqualValues(t, 5000, getChannelUsedQuota(t, 77))
			var logs int64
			require.NoError(t, model.LOG_DB.Model(&model.Log{}).Count(&logs).Error)
			assert.Zero(t, logs, "an unverified legacy task cannot emit a successful refund log")
		})
	}
}

// ===========================================================================
// CAS + Billing integration tests
// Simulates the flow in updateVideoSingleTask (service/task_polling.go)
// ===========================================================================

// simulatePollBilling reproduces the CAS + billing logic from updateVideoSingleTask.
// It takes a persisted task (already in DB), applies the new status, and performs
// the conditional update + billing exactly as the polling loop does.
func simulatePollBilling(ctx context.Context, task *model.Task, newStatus model.TaskStatus, actualQuota int) {
	snap := task.Snapshot()
	task.Status = newStatus
	terminal := newStatus == model.TaskStatusSuccess || newStatus == model.TaskStatusFailure
	if terminal {
		task.Progress = "100%"
		task.FinishTime = 9999
		if task.PrivateData.Gateway != nil {
			task.PrivateData.Gateway.CompletionFacts = map[string]any{"tokens": actualQuota}
		}
	} else {
		task.Progress = "50%"
	}
	won, err := task.UpdateWithStatus(snap.Status)
	if err == nil && won && terminal && snap.Status != newStatus {
		finalizeTerminalTask(ctx, &mockAdaptor{}, task, &relaycommon.TaskInfo{Status: string(newStatus), UsageFacts: map[string]any{"tokens": actualQuota}})
	}
}

func TestCASGuardedRefund_Win(t *testing.T) {
	assertCanonicalWalletTaskSettlement(t, 4000, 0, model.TaskStatusFailure)
}

func TestCASGuardedRefund_Lose(t *testing.T) {
	task := seedCanonicalWalletTask(t, 4000)
	var stale model.Task
	require.NoError(t, model.DB.First(&stale, task.ID).Error)
	simulatePollBilling(t.Context(), task, model.TaskStatusFailure, 0)
	assertCanonicalWalletTaskAccounting(t, task, 4000, 0, 0)
	var before []model.LedgerEntry
	require.NoError(t, model.DB.Order("id").Find(&before).Error)
	simulatePollBilling(t.Context(), &stale, model.TaskStatusFailure, 0)
	var after []model.LedgerEntry
	require.NoError(t, model.DB.Order("id").Find(&after).Error)
	assert.Equal(t, before, after, "the real losing status CAS cannot refund twice")
	var persisted model.Task
	require.NoError(t, model.DB.First(&persisted, task.ID).Error)
	assert.EqualValues(t, model.TaskStatusFailure, persisted.Status)
	wallet, err := model.GetUserWallet(77)
	require.NoError(t, err)
	assert.Equal(t, money.Amount{Nanos: 8888, Currency: "USD"}, wallet.Balance)
}

func TestCASGuardedSettle_Win(t *testing.T) {
	assertCanonicalWalletTaskSettlement(t, 5000, 3000, model.TaskStatusSuccess)
}

func TestNonTerminalUpdate_NoBilling(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, channelID = 23, 23
	const initQuota, preConsumed = 10000, 3000

	seedUser(t, userID, initQuota)
	seedChannel(t, channelID)

	task := makeTask(userID, channelID, preConsumed, 0, BillingSourceWallet, 0)
	task.Status = model.TaskStatus(model.TaskStatusInProgress)
	task.Progress = "20%"
	require.NoError(t, model.DB.Create(task).Error)

	// Simulate a non-terminal poll update (still IN_PROGRESS, progress changed)
	simulatePollBilling(ctx, task, model.TaskStatus(model.TaskStatusInProgress), 0)

	// User quota should NOT change
	assert.Equal(t, initQuota, getUserQuota(t, userID))

	// No billing log
	assert.Equal(t, int64(0), countLogs(t))

	// Task progress should be updated in DB
	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.Equal(t, "50%", reloaded.Progress)
}

// ===========================================================================
// Mock adaptor for settleTaskBillingOnComplete tests
// ===========================================================================

type mockAdaptor struct {
	adjustReturn int
}

func (m *mockAdaptor) Init(_ *relaycommon.RelayInfo) {}
func (m *mockAdaptor) FetchTask(string, string, *model.Task, string) (*http.Response, error) {
	return nil, nil
}
func (m *mockAdaptor) ParseTaskResult(*model.Task, *http.Response, []byte) (*relaycommon.TaskInfo, error) {
	return nil, nil
}
func (m *mockAdaptor) AdjustBillingOnComplete(_ *model.Task, _ *relaycommon.TaskInfo) int {
	return m.adjustReturn
}

// ===========================================================================
// PerCallBilling tests — settleTaskBillingOnComplete
// ===========================================================================

func TestSettle_PerCallBilling_SkipsAdaptorAdjust(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 30, 30, 30
	const initQuota, preConsumed = 10000, 5000
	const tokenRemain = 8000

	seedUser(t, userID, initQuota)
	seedToken(t, tokenID, userID, "sk-percall-adaptor", tokenRemain)
	seedChannel(t, channelID)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	task.PrivateData.BillingContext.PerCallBilling = true

	adaptor := &mockAdaptor{adjustReturn: 2000}
	taskResult := &relaycommon.TaskInfo{Status: model.TaskStatusSuccess}

	settled := settleTaskBillingOnComplete(ctx, adaptor, task, taskResult)

	// Per-call: no adjustment despite adaptor returning 2000
	assert.False(t, settled)
	assert.Equal(t, initQuota, getUserQuota(t, userID))
	assert.Equal(t, tokenRemain, getTokenRemainQuota(t, tokenID))
	assert.Equal(t, preConsumed, task.Quota)
	assert.Equal(t, int64(0), countLogs(t))
}

func TestSettle_PerCallBilling_SkipsTotalTokens(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID, tokenID, channelID = 31, 31, 31
	const initQuota, preConsumed = 10000, 4000
	const tokenRemain = 7000

	seedUser(t, userID, initQuota)
	seedToken(t, tokenID, userID, "sk-percall-tokens", tokenRemain)
	seedChannel(t, channelID)

	task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
	task.PrivateData.BillingContext.PerCallBilling = true

	adaptor := &mockAdaptor{adjustReturn: 0}
	taskResult := &relaycommon.TaskInfo{Status: model.TaskStatusSuccess, TotalTokens: 9999}

	settled := settleTaskBillingOnComplete(ctx, adaptor, task, taskResult)

	// Per-call: no recalculation by tokens
	assert.False(t, settled)
	assert.Equal(t, initQuota, getUserQuota(t, userID))
	assert.Equal(t, tokenRemain, getTokenRemainQuota(t, tokenID))
	assert.Equal(t, preConsumed, task.Quota)
	assert.Equal(t, int64(0), countLogs(t))
}

func TestSettle_NonPerCallBilling_AppliesAdaptorAdjustment(t *testing.T) {
	// The replacement contract prices actual facts, not an adaptor's old int hint.
	task := seedCanonicalWalletTask(t, 5000)
	task.Status = model.TaskStatusSuccess
	task.Progress = "100%"
	task.PrivateData.Gateway.CompletionFacts = map[string]any{"tokens": 3000}
	require.NoError(t, model.DB.Save(task).Error)
	finalizeTerminalTask(t.Context(), &mockAdaptor{adjustReturn: 1}, task, &relaycommon.TaskInfo{Status: model.TaskStatusSuccess})
	assertCanonicalWalletTaskAccounting(t, task, 5000, 3000, 1500)
}

func TestSettle_TieredEvaluationFailureKeepsPreConsumedCharge(t *testing.T) {
	task := seedCanonicalWalletTaskWithPricing(t, 5000, `tier("broken",`, `0.000001`, nil)
	task.Status = model.TaskStatusSuccess
	task.Progress = "100%"
	task.PrivateData.Gateway.CompletionFacts = map[string]any{"tokens": 3}
	require.NoError(t, model.DB.Save(task).Error)
	require.Error(t, SettleGatewayTask(t.Context(), task))
	wallet, err := model.GetUserWallet(77)
	require.NoError(t, err)
	assert.Equal(t, money.Amount{Nanos: 3888, Currency: "USD"}, wallet.Balance)
	var billing model.GatewayBilling
	require.NoError(t, model.DB.First(&billing, "request_id = ?", "wallet-task-settlement").Error)
	assert.Equal(t, "evaluation_failed", billing.Phase)
	var intent model.GatewaySettlement
	require.NoError(t, common.UnmarshalJsonStr(billing.Settlement, &intent))
	assert.True(t, intent.SaleEvaluationFailed)
	require.NotNil(t, intent.Attempt)
	assert.Equal(t, money.Amount{Nanos: 1000, Currency: "USD"}, intent.Attempt.Cost)
	var before []model.LedgerEntry
	require.NoError(t, model.DB.Order("id").Find(&before).Error)
	assert.Error(t, SettleGatewayTask(t.Context(), task))
	var after []model.LedgerEntry
	require.NoError(t, model.DB.Order("id").Find(&after).Error)
	assert.Equal(t, before, after, "known procurement posts once while the unresolved sale retains its hold")
	assert.Zero(t, getTaskQuota(t, task.ID))
	assert.Equal(t, 8000, getTokenRemainQuota(t, 77))
}

func TestSettle_TieredFailureReturnsFalseForCallerRefund(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const userID = 37
	const initialQuota, preConsumed = 10_000, 25
	seedUser(t, userID, initialQuota)

	expression := `tier("base", u("seconds") + u("clips") * 10)`
	task := makeTask(userID, 0, preConsumed, 0, BillingSourceWallet, 0)
	task.Status = model.TaskStatusFailure
	task.PrivateData.BillingContext.TieredSnapshot = &billingexpr.BillingSnapshot{
		ExprString:       expression,
		ExprHash:         billingexpr.ExprHashString(expression),
		GroupRatio:       1,
		QuotaPerUnit:     1,
		ExprVersion:      1,
		TaskUsageBilling: true,
		UsageFacts:       map[string]any{"seconds": float64(5), "clips": float64(2)},
		EstimatedTier:    "base",
	}

	settled := settleTaskBillingOnComplete(
		ctx,
		&mockAdaptor{adjustReturn: 1},
		task,
		&relaycommon.TaskInfo{Status: model.TaskStatusFailure, UsageFacts: map[string]any{"seconds": float64(8)}},
	)

	assert.False(t, settled)
	assert.Equal(t, preConsumed, task.Quota)
	assert.Equal(t, map[string]any{"seconds": float64(5), "clips": float64(2)}, task.PrivateData.BillingContext.TieredSnapshot.UsageFacts)
	assert.Equal(t, "base", task.PrivateData.BillingContext.TieredSnapshot.EstimatedTier)
	assert.Equal(t, initialQuota, getUserQuota(t, userID))
	assert.Equal(t, int64(0), countLogs(t))
}

func assertCanonicalTieredTask(t *testing.T, expression string, submitted, completed, expected map[string]any, tier string, amount int64) {
	t.Helper()
	before, err := common.Marshal(submitted)
	require.NoError(t, err)
	task := seedCanonicalWalletTaskWithPricing(t, 50, expression, "0", submitted)
	task.Status = model.TaskStatusSuccess
	task.Progress = "100%"
	task.PrivateData.Gateway.CompletionFacts = completed
	require.NoError(t, model.DB.Save(task).Error)
	require.NoError(t, SettleGatewayTask(t.Context(), task))
	assertCanonicalWalletTaskAccounting(t, task, 50, amount, 0)
	record, err := model.GetGatewayRequest(t.Context(), "wallet-task-settlement", 77)
	require.NoError(t, err)
	assert.Equal(t, expected, record.Usage.TaskFacts)
	assert.Equal(t, expression, record.SaleExpression)
	var billing model.GatewayBilling
	require.NoError(t, model.DB.First(&billing, "request_id = ?", record.ID).Error)
	var intent model.GatewaySettlement
	require.NoError(t, common.UnmarshalJsonStr(billing.Settlement, &intent))
	require.NotNil(t, intent.SaleTrace)
	assert.Empty(t, intent.SaleTrace.MatchedTier, "durable traces must not retain computed strings")
	value, trace, err := billingexpr.RunMoneyExpr(record.SaleExpression, billingexpr.MoneyInput{TaskUsage: true, Request: billingexpr.RequestInput{Usage: record.Usage.TaskFacts}}, "USD")
	require.NoError(t, err)
	assert.Equal(t, tier, trace.MatchedTier)
	posted, err := value.Receivable()
	require.NoError(t, err)
	assert.Equal(t, record.Receivable, posted)
	after, err := common.Marshal(submitted)
	require.NoError(t, err)
	assert.Equal(t, before, after, "completion must not mutate frozen submission facts")
	assert.Zero(t, countLogs(t), "canonical accounting must not recreate an integer refund log")
}

func TestSettle_TieredSuccessStillRecomputes(t *testing.T) {
	assertCanonicalTieredTask(t, `tier("base", u("seconds") + u("clips") * 10) / 1000000000`,
		map[string]any{"seconds": float64(5), "clips": float64(2)}, map[string]any{"seconds": float64(8)},
		map[string]any{"seconds": float64(8), "clips": float64(2)}, "base", 28)
}

func TestSettle_TieredUsageFactsMergeCompletionOverSubmission(t *testing.T) {
	for _, tc := range []struct {
		name      string
		completed map[string]any
		amount    int64
		expected  map[string]any
	}{
		{"submission facts survive missing completion facts", nil, 25, map[string]any{"seconds": float64(5), "clips": float64(2)}},
		{"completion facts partially override submission facts", map[string]any{"seconds": float64(8)}, 28, map[string]any{"seconds": float64(8), "clips": float64(2)}},
		{"completion facts fully override submission facts", map[string]any{"seconds": float64(8), "clips": float64(3)}, 38, map[string]any{"seconds": float64(8), "clips": float64(3)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertCanonicalTieredTask(t, `tier("base", u("seconds") + u("clips") * 10) / 1000000000`,
				map[string]any{"seconds": float64(5), "clips": float64(2)}, tc.completed, tc.expected, "base", tc.amount)
		})
	}

}

func TestSettle_TieredSnapshotWriteBackUsesSettledFactsAndMatchedTier(t *testing.T) {
	assertCanonicalTieredTask(t, `(u("resolution") == "1080P" ? tier("1080P", u("seconds") * 10) : tier("720P", u("seconds") * 5)) / 1000000000`,
		map[string]any{"resolution": "720P", "seconds": float64(5)}, map[string]any{"resolution": "1080P"},
		map[string]any{"resolution": "1080P", "seconds": float64(5)}, "1080P", 50)
}

func TestSettle_TokenRecalcFallsBackToCompletionTokens(t *testing.T) {
	for _, tc := range []struct {
		name   string
		amount int64
	}{
		{"reported total tokens", 80}, {"reported completion-only tokens", 80},
	} {
		t.Run(tc.name, func(t *testing.T) { assertCanonicalWalletTaskSettlement(t, 50, tc.amount, model.TaskStatusSuccess) })
	}
	t.Run("missing counters remain unknown", func(t *testing.T) {
		task := seedCanonicalWalletTask(t, 50)
		task.Status = model.TaskStatusSuccess
		task.Progress = "100%"
		task.PrivateData.Gateway.CompletionUnknown = true
		require.NoError(t, model.DB.Save(task).Error)
		require.Error(t, SettleGatewayTask(t.Context(), task))
		wallet, err := model.GetUserWallet(77)
		require.NoError(t, err)
		assert.Equal(t, money.Amount{Nanos: 8838, Currency: "USD"}, wallet.Balance)
		assert.Zero(t, getTaskQuota(t, task.ID))
		assert.Equal(t, 8000, getTokenRemainQuota(t, 77))
	})
}

// failRowUpdates installs a database trigger that rejects every UPDATE of the
// row with the given id, so a test can force a write failure regardless of
// which SQL statement the code under test issues.
func failRowUpdates(t *testing.T, table string, id int, message string) {
	t.Helper()
	name := fmt.Sprintf("fail_%s_%d_update", table, id)
	require.NoError(t, model.DB.Exec(fmt.Sprintf(`
		CREATE FUNCTION %[1]s() RETURNS trigger AS $body$
		BEGIN
			IF OLD.id = %[2]d THEN
				RAISE EXCEPTION '%[3]s';
			END IF;
			RETURN NEW;
		END
		$body$ LANGUAGE plpgsql`, name, id, message)).Error)
	require.NoError(t, model.DB.Exec(fmt.Sprintf(
		`CREATE TRIGGER %[1]s BEFORE UPDATE ON %[2]s FOR EACH ROW EXECUTE FUNCTION %[1]s()`, name, table)).Error)
	t.Cleanup(func() {
		model.DB.Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON %s`, name, table))
		model.DB.Exec(fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, name))
	})
}
