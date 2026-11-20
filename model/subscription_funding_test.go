package model

import (
	"bytes"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/clock"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func subscriptionFundingFixture(t *testing.T, preference string, wallet, allowance int64, unlimited, overflow bool) (User, UserSubscription) {
	t.Helper()
	useWalletLedgerDB(t)
	t.Setenv("GATEWAY_SETTLEMENT_JOURNAL_DIR", t.TempDir())
	require.NoError(t, DB.AutoMigrate(&User{}))
	require.NoError(t, MigrateSubscriptionMoney(DB))
	require.NoError(t, MigrateGatewayBilling(DB))
	require.NoError(t, MigrateGatewayRecords())
	user := User{Username: "funding", Setting: fmt.Sprintf(`{"billing_preference":%q}`, preference)}
	require.NoError(t, DB.Create(&user).Error)
	if wallet > 0 {
		_, err := TopUpWallet(WalletTopUp{UserId: user.Id, Source: usd(wallet), Reference: "funding-credit"})
		require.NoError(t, err)
	}
	plan := SubscriptionPlan{Title: "Exact entitlement", Price: usd(7), Allowance: usd(allowance), Unlimited: unlimited, MoneyVersion: 1}
	require.NoError(t, DB.Create(&plan).Error)
	sub := UserSubscription{UserId: user.Id, PlanId: plan.Id, Allowance: plan.Allowance, Unlimited: unlimited, MoneyVersion: 1, Status: "active", StartTime: clock.Now().Unix() - 1, EndTime: clock.Now().Unix() + 3600, AllowWalletOverflow: overflow}
	require.NoError(t, DB.Create(&sub).Error)
	require.NoError(t, DB.Transaction(func(tx *gorm.DB) error {
		if _, err := lockSubscriptionFundingWallets(tx, []int{user.Id}); err != nil {
			return err
		}
		return createSubscriptionPeriodTx(tx, &sub, usd(0), "grant")
	}))
	return user, sub
}

func TestLegacyTaskArchivesPreserveOpaqueJSON(t *testing.T) {
	for _, archive := range []string{"ordinary", "subscription"} {
		t.Run(archive, func(t *testing.T) {
			user, sub := subscriptionFundingFixture(t, "wallet_only", 1_000_000_000, 500_000_000, false, false)
			quiet := DB.Session(&gorm.Session{Logger: logger.Discard})
			require.NoError(t, quiet.AutoMigrate(&Task{}))
			const canary = "opaque-task-private-value-must-not-leak"
			for i, kind := range []string{"ordinary", "subscription", "midjourney", "gateway"} {
				private := TaskPrivateData{BillingSource: "wallet", TokenId: 91, UpstreamTaskID: "original-upstream", Key: canary,
					PluginState: common.RawMessage(`{"n":1e200000}`),
					Execution:   &TaskExecutionSnapshot{RequestID: "original-request", TaskPlugin: &TaskPluginSnapshot{Key: "sora", Version: "1.2.3", Generation: math.MaxUint64}}}
				switch kind {
				case "subscription":
					private.BillingSource, private.SubscriptionId = "subscription", sub.Id
				case "midjourney":
					private.LegacyObligation = &TaskLegacyObligation{Version: "legacy_midjourney_obligations_v2", SourceID: 81}
				case "gateway":
					private.BillingSource, private.SubscriptionId = "subscription", sub.Id
					private.Gateway = &TaskGatewaySnapshot{}
				}
				encoded, err := common.Marshal(private)
				require.NoError(t, err)
				raw := strings.TrimSuffix(string(encoded), "}") + `,"unknown_future":{"n":1e200000}}`
				task := Task{ID: int64(901 + i), TaskID: kind, UserId: user.Id, ChannelId: 71, Platform: "sora", Quota: 9_007_199_254_740_993, Status: TaskStatusSuccess}
				require.NoError(t, quiet.Create(&task).Error)
				require.NoError(t, quiet.Exec(`UPDATE tasks SET private_data=?::json WHERE id=?`, raw, task.ID).Error)
				var decoded Task
				require.NoError(t, quiet.First(&decoded, task.ID).Error, "real Task.Scan accepts unknown fields and opaque RawMessage numbers")
				assert.Equal(t, private, decoded.PrivateData)
			}
			snapshot := func() map[string]string {
				t.Helper()
				result := map[string]string{}
				for _, table := range []string{"tasks", "wallets", "ledger_entries", "gateway_billings", "subscription_periods", "user_subscriptions"} {
					var digest string
					require.NoError(t, quiet.Raw(fmt.Sprintf(`SELECT md5(COALESCE(string_agg(row_to_json(t)::text,E'\n' ORDER BY row_to_json(t)::text),'')) FROM %q t`, table)).Scan(&digest).Error)
					result[table] = digest
				}
				return result
			}
			before := snapshot()
			var queryLog bytes.Buffer
			logged := DB.Session(&gorm.Session{Logger: logger.New(log.New(&queryLog, "", 0), logger.Config{LogLevel: logger.Info})})
			var previous string
			for range 2 {
				var err error
				if archive == "ordinary" {
					err = MigrateLegacyTaskObligations(logged)
				} else {
					err = MigrateSubscriptionMoney(logged)
				}
				require.NoError(t, err, "opaque values must not be converted through PostgreSQL jsonb numeric")
				var encoded []byte
				if archive == "ordinary" {
					var rows []LegacyTaskObligation
					require.NoError(t, quiet.Order("source_task_id").Find(&rows).Error)
					require.Len(t, rows, 1)
					assert.EqualValues(t, 901, rows[0].SourceTaskID)
					assert.Equal(t, "wallet", rows[0].BillingSource)
					assert.Equal(t, uint64(math.MaxUint64), rows[0].PluginGeneration)
					assert.Equal(t, "original-upstream", rows[0].UpstreamTaskID)
					assert.EqualValues(t, 9_007_199_254_740_993, rows[0].SourceQuota)
					encoded, err = common.Marshal(rows)
				} else {
					var rows []LegacySubscriptionTaskObligation
					require.NoError(t, quiet.Order("source_task_id").Find(&rows).Error)
					require.Len(t, rows, 1)
					assert.EqualValues(t, 902, rows[0].SourceTaskID)
					assert.Equal(t, sub.Id, rows[0].SubscriptionID)
					assert.Equal(t, user.Id, rows[0].UserID)
					assert.EqualValues(t, 9_007_199_254_740_993, rows[0].SourceQuota)
					encoded, err = common.Marshal(rows)
				}
				require.NoError(t, err)
				assert.False(t, bytes.Contains(encoded, []byte(canary)))
				assert.NotContains(t, string(encoded), "1e200000", "only identity scalars belong in evidence")
				if previous != "" {
					assert.Equal(t, previous, string(encoded), "replay preserves the first immutable evidence")
				}
				previous = string(encoded)
				assert.Equal(t, before, snapshot(), "source bytes, identities and all financial facts remain unchanged")
			}
			assert.False(t, strings.Contains(queryLog.String(), canary), "logs must not contain private source values")
		})
	}
}

func TestLegacyTaskObligationsPreserveSafeEvidence(t *testing.T) {
	user, _ := subscriptionFundingFixture(t, "wallet_only", 1_000_000_000, 500_000_000, false, false)
	require.NoError(t, DB.AutoMigrate(&Task{}))
	original := Task{TaskID: "historical-public-id", UserId: user.Id, ChannelId: 9001, Platform: "sora", Status: TaskStatusFailure,
		Quota: 9_007_199_254_740_993, CreatedAt: 11, UpdatedAt: 12, SubmitTime: 13, StartTime: 14, FinishTime: 15,
		PrivateData: TaskPrivateData{BillingSource: "wallet", TokenId: 9002, SubscriptionId: 9003, UpstreamTaskID: "original-upstream-id",
			Key: "credential-must-not-copy", ResultURL: "https://example.invalid/private-result",
			PluginState: common.RawMessage(`{"secret":"plugin-state-must-not-copy"}`),
			Execution: &TaskExecutionSnapshot{RequestID: "actual-request-id", RequestPath: "/request-path-must-not-copy",
				TaskPlugin: &TaskPluginSnapshot{Key: "sora", Name: "plugin-name-must-not-copy", Version: "1.2.3", Generation: math.MaxUint64}}},
		Data: common.RawMessage(`{"body":"body-must-not-copy"}`)}
	require.NoError(t, DB.Create(&original).Error)
	unknown := Task{TaskID: "unknown-source", UserId: 0, ChannelId: 0, Platform: "", Status: TaskStatusSuccess, Quota: -7,
		PrivateData: TaskPrivateData{BillingSource: "unrecognized-source", SubscriptionId: 9004}}
	require.NoError(t, DB.Create(&unknown).Error)
	empty := Task{Status: TaskStatusSuccess, Quota: 0}
	require.NoError(t, DB.Create(&empty).Error)
	batch := make([]Task, 250)
	for i := range batch {
		batch[i] = Task{TaskID: fmt.Sprintf("ordinary-%d", i), Status: TaskStatusInProgress, Quota: i}
	}
	require.NoError(t, DB.Create(&batch).Error)
	for _, data := range []TaskPrivateData{
		{Gateway: &TaskGatewaySnapshot{}},
		{BillingSource: "wallet", LegacyObligation: &TaskLegacyObligation{Version: "legacy_midjourney_obligations_v2"}},
		{BillingSource: "subscription"},
	} {
		task := Task{TaskID: "excluded", PrivateData: data}
		require.NoError(t, DB.Create(&task).Error)
	}
	archived := Task{TaskID: "already-subscription-evidence", PrivateData: TaskPrivateData{BillingSource: "unknown"}}
	require.NoError(t, DB.Create(&archived).Error)
	require.NoError(t, DB.Create(&LegacySubscriptionTaskObligation{SourceTaskID: archived.ID, State: "unverified"}).Error)
	var beforeTasks []Task
	var beforeWallets []Wallet
	var beforeEntries []LedgerEntry
	require.NoError(t, DB.Order("id").Find(&beforeTasks).Error)
	require.NoError(t, DB.Order("id").Find(&beforeWallets).Error)
	require.NoError(t, DB.Order("id").Find(&beforeEntries).Error)
	require.NoError(t, MigrateLegacyTaskObligations(DB))
	var rows []LegacyTaskObligation
	require.NoError(t, DB.Order("source_task_id").Find(&rows).Error)
	require.Len(t, rows, 253, "ordinary evidence must survive a 250-row batch boundary without classifying subscription or money tasks as wallet funding")
	assert.Equal(t, LegacyTaskObligation{
		SourceTaskID: original.ID, UserID: user.Id, ProviderID: 9001, TaskID: original.TaskID, UpstreamTaskID: "original-upstream-id",
		Platform: "sora", BillingSource: "wallet", TokenID: 9002, SubscriptionID: 9003, RequestID: "actual-request-id",
		PluginKey: "sora", PluginVersion: "1.2.3", PluginGeneration: math.MaxUint64, SourceQuota: 9_007_199_254_740_993,
		SourceStatus: string(TaskStatusFailure), SourceCreatedAt: 11, SourceUpdatedAt: 12,
		SourceSubmitTime: 13, SourceStartTime: 14, SourceFinishTime: 15, State: "unverified", CapturedAt: rows[0].CapturedAt,
	}, rows[0])
	assert.Positive(t, rows[0].CapturedAt)
	assert.Equal(t, "unrecognized-source", rows[1].BillingSource)
	assert.EqualValues(t, -7, rows[1].SourceQuota)
	assert.Equal(t, 9004, rows[1].SubscriptionID, "unknown funding is not relabeled even with a subscription ID")
	assert.Zero(t, rows[1].UserID)
	assert.Zero(t, rows[1].ProviderID)
	assert.Empty(t, rows[1].UpstreamTaskID, "do not invent an upstream identity from the public ID")
	assert.Empty(t, rows[2].RequestID, "missing request/plugin identities stay missing")
	assert.Empty(t, rows[2].PluginKey)
	assert.Zero(t, rows[2].PluginGeneration)
	assert.Zero(t, rows[2].SourceQuota)
	encoded, err := common.Marshal(rows)
	require.NoError(t, err)
	for _, secret := range []string{"credential-must-not-copy", "private-result", "plugin-state-must-not-copy", "request-path-must-not-copy", "plugin-name-must-not-copy", "body-must-not-copy"} {
		assert.NotContains(t, string(encoded), secret)
	}
	var afterTasks []Task
	require.NoError(t, DB.Order("id").Find(&afterTasks).Error)
	assert.Equal(t, beforeTasks, afterTasks, "capture does not rewrite the historical source")
	require.NoError(t, DB.Model(&Task{}).Where("id = ?", original.ID).Updates(map[string]any{
		"status": TaskStatusSuccess, "quota": 0, "created_at": 101, "updated_at": 102, "submit_time": 103, "start_time": 104, "finish_time": 105,
	}).Error)
	require.NoError(t, MigrateLegacyTaskObligations(DB))
	var repeated []LegacyTaskObligation
	require.NoError(t, DB.Order("source_task_id").Find(&repeated).Error)
	assert.Equal(t, rows, repeated, "polling may change source facts, never the first captured evidence")
	var afterWallets []Wallet
	var afterEntries []LedgerEntry
	require.NoError(t, DB.Order("id").Find(&afterWallets).Error)
	require.NoError(t, DB.Order("id").Find(&afterEntries).Error)
	assert.Equal(t, beforeWallets, afterWallets)
	assert.Equal(t, beforeEntries, afterEntries)
	var billings int64
	require.NoError(t, DB.Model(&GatewayBilling{}).Count(&billings).Error)
	assert.Zero(t, billings, "historical evidence is not a reconstructed Gateway settlement")
	require.Error(t, DB.Model(&LegacyTaskObligation{}).Where("source_task_id = ?", original.ID).Update("state", "unverified").Error)
	require.Error(t, DB.Where("source_task_id = ?", original.ID).Delete(&LegacyTaskObligation{}).Error)
	require.Error(t, DB.Exec(`TRUNCATE TABLE "legacy_task_obligations"`).Error)
	require.Error(t, DB.Create(&rows[0]).Error, "the source task identity remains unique")
}

func TestLegacyTaskObligationsRejectIdentityRebinding(t *testing.T) {
	changes := []struct {
		name    string
		mutate  func(*Task)
		archive bool
	}{
		{name: "owner", mutate: func(task *Task) { task.UserId++ }},
		{name: "provider", mutate: func(task *Task) { task.ChannelId++ }},
		{name: "public_task", mutate: func(task *Task) { task.TaskID += "-other" }},
		{name: "platform", mutate: func(task *Task) { task.Platform = "other" }},
		{name: "upstream", mutate: func(task *Task) { task.PrivateData.UpstreamTaskID += "-other" }},
		{name: "source", mutate: func(task *Task) { task.PrivateData.BillingSource = "other" }},
		{name: "token", mutate: func(task *Task) { task.PrivateData.TokenId++ }},
		{name: "subscription", mutate: func(task *Task) { task.PrivateData.SubscriptionId++ }},
		{name: "request", mutate: func(task *Task) { task.PrivateData.Execution.RequestID += "-other" }},
		{name: "plugin_key", mutate: func(task *Task) { task.PrivateData.Execution.TaskPlugin.Key = "other" }},
		{name: "plugin_version", mutate: func(task *Task) { task.PrivateData.Execution.TaskPlugin.Version = "other" }},
		{name: "plugin_generation", mutate: func(task *Task) { task.PrivateData.Execution.TaskPlugin.Generation++ }},
		{name: "remove_execution", mutate: func(task *Task) { task.PrivateData.Execution = nil }},
		{name: "remove_plugin", mutate: func(task *Task) { task.PrivateData.Execution.TaskPlugin = nil }},
		{name: "converted_gateway", mutate: func(task *Task) { task.PrivateData.Gateway = &TaskGatewaySnapshot{} }},
		{name: "converted_midjourney", mutate: func(task *Task) {
			task.PrivateData.LegacyObligation = &TaskLegacyObligation{Version: "legacy_midjourney_obligations_v2"}
		}},
		{name: "converted_subscription", mutate: func(task *Task) { task.PrivateData.BillingSource = "subscription" }},
		{name: "new_subscription_archive", archive: true},
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			UseTestDatabases(t)
			require.NoError(t, DB.AutoMigrate(&Task{}))
			original := Task{TaskID: "public", UserId: 11, ChannelId: 12, Platform: "sora", Quota: 17,
				PrivateData: TaskPrivateData{BillingSource: "wallet", TokenId: 13, SubscriptionId: 14, UpstreamTaskID: "upstream",
					Execution: &TaskExecutionSnapshot{RequestID: "request", TaskPlugin: &TaskPluginSnapshot{Key: "sora", Version: "1", Generation: 7}}}}
			require.NoError(t, DB.Create(&original).Error)
			require.NoError(t, MigrateLegacyTaskObligations(DB))
			var before LegacyTaskObligation
			require.NoError(t, DB.First(&before, "source_task_id = ?", original.ID).Error)
			changed := original
			execution := *original.PrivateData.Execution
			plugin := *execution.TaskPlugin
			execution.TaskPlugin = &plugin
			changed.PrivateData.Execution = &execution
			if change.archive {
				require.NoError(t, DB.AutoMigrate(&LegacySubscriptionTaskObligation{}))
				require.NoError(t, DB.Create(&LegacySubscriptionTaskObligation{SourceTaskID: original.ID, State: "unverified"}).Error)
			} else {
				change.mutate(&changed)
				require.NoError(t, DB.Model(&Task{}).Where("id = ?", original.ID).Updates(map[string]any{
					"user_id": changed.UserId, "channel_id": changed.ChannelId, "task_id": changed.TaskID,
					"platform": changed.Platform, "private_data": changed.PrivateData,
				}).Error)
			}
			err := MigrateLegacyTaskObligations(DB)
			require.ErrorContains(t, err, "identity changed")
			var after LegacyTaskObligation
			require.NoError(t, DB.First(&after, "source_task_id = ?", original.ID).Error)
			assert.Equal(t, before, after)
			if !change.archive {
				require.NoError(t, DB.Model(&Task{}).Where("id = ?", original.ID).Updates(map[string]any{
					"user_id": original.UserId, "channel_id": original.ChannelId, "task_id": original.TaskID,
					"platform": original.Platform, "private_data": original.PrivateData,
				}).Error)
				require.NoError(t, MigrateLegacyTaskObligations(DB))
			}
		})
	}
}

func TestLegacyTaskObligationsRollbackUnsafeSources(t *testing.T) {
	// Another test schema can already have committed the same migration.
	neighbor := UseTestDatabases(t)
	require.NoError(t, MigrateLegacyTaskObligations(neighbor))

	for _, tc := range []struct{ name, raw string }{
		{"invalid_json", `not-json-private-do-not-leak`},
		{"duplicate_source", `{"billing_source":"subscription","billing_source":"wallet"}`},
		{"duplicate_request", `{"execution":{"request_id":"first","request_id":"other"}}`},
		{"duplicate_plugin", `{"execution":{"task_plugin":{"key":"first","key":"other"}}}`},
		{"private_array", `["private-do-not-leak"]`},
		{"execution_string", `{"execution":"private-do-not-leak"}`},
		{"plugin_array", `{"execution":{"task_plugin":[]}}`},
		{"source_number", `{"billing_source":1}`},
		{"token_string", `{"token_id":"private-do-not-leak"}`},
		{"subscription_fraction", `{"subscription_id":1.25}`},
		{"upstream_boolean", `{"upstream_task_id":true}`},
		{"request_number", `{"execution":{"request_id":1}}`},
		{"plugin_key_number", `{"execution":{"task_plugin":{"key":1}}}`},
		{"plugin_version_object", `{"execution":{"task_plugin":{"version":{}}}}`},
		{"generation_overflow", `{"execution":{"task_plugin":{"generation":18446744073709551616}}}`},
		{"generation_negative", `{"execution":{"task_plugin":{"generation":-1}}}`},
		{"gateway_scalar", `{"gateway":"private-do-not-leak"}`},
		{"midjourney_scalar", `{"legacy_obligation":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			UseTestDatabases(t)
			require.NoError(t, DB.AutoMigrate(&Task{}))
			rows := make([]Task, 251)
			for i := range rows {
				rows[i] = Task{TaskID: fmt.Sprintf("candidate-%d", i), Quota: i}
			}
			require.NoError(t, DB.Create(&rows).Error)
			if tc.name == "invalid_json" {
				// Released/imported schemas may still hold JSON in a text column.
				require.NoError(t, DB.Exec(`ALTER TABLE tasks ALTER COLUMN private_data TYPE text USING private_data::text`).Error)
			}
			require.NoError(t, DB.Model(&Task{}).Where("id = ?", rows[250].ID).UpdateColumn("private_data", tc.raw).Error)
			err := MigrateLegacyTaskObligations(DB)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "private-do-not-leak")
			assert.False(t, DB.Migrator().HasTable(&LegacyTaskObligation{}), "schema and first batch must roll back with a bad later batch")
			var functions int64
			require.NoError(t, DB.Raw(`SELECT COUNT(*) FROM pg_proc
 WHERE proname = 'protect_legacy_task_obligations' AND pronamespace = current_schema()::regnamespace`).Scan(&functions).Error)
			assert.Zero(t, functions)
			var count int64
			require.NoError(t, DB.Model(&Task{}).Count(&count).Error)
			assert.EqualValues(t, 251, count)
		})
	}
	t.Run("sql_projection_failure", func(t *testing.T) {
		UseTestDatabases(t)
		require.NoError(t, DB.AutoMigrate(&Task{}))
		require.NoError(t, DB.Create(&Task{TaskID: "valid"}).Error)
		require.NoError(t, DB.Exec(`ALTER TABLE tasks RENAME COLUMN private_data TO old_private_data`).Error)
		require.ErrorContains(t, MigrateLegacyTaskObligations(DB), "cannot read")
		assert.False(t, DB.Migrator().HasTable(&LegacyTaskObligation{}))
		require.NoError(t, DB.Exec(`ALTER TABLE tasks RENAME COLUMN old_private_data TO private_data`).Error)
		require.NoError(t, MigrateLegacyTaskObligations(DB))
		var count int64
		require.NoError(t, DB.Model(&LegacyTaskObligation{}).Count(&count).Error)
		assert.EqualValues(t, 1, count)
	})
}

func TestLegacyTaskObligationsFreshAndOrphanEvidence(t *testing.T) {
	UseTestDatabases(t)
	require.Error(t, MigrateLegacyTaskObligations(nil))
	require.NoError(t, MigrateLegacyTaskObligations(DB))
	require.NoError(t, MigrateLegacyTaskObligations(DB))
	assert.False(t, DB.Migrator().HasTable(&Task{}))
	assert.False(t, DB.Migrator().HasTable(&Wallet{}))
	assert.False(t, DB.Migrator().HasTable(&LedgerEntry{}))
	require.NoError(t, DB.AutoMigrate(&Task{}))
	// Parent identity is historical evidence even if the corresponding User,
	// Provider, Token and Subscription tables or rows no longer exist.
	orphan := Task{ID: -1, TaskID: "orphan", UserId: 123, ChannelId: 456,
		PrivateData: TaskPrivateData{TokenId: 789, SubscriptionId: 999, BillingSource: "wallet"}}
	require.NoError(t, DB.Create(&orphan).Error)
	require.NoError(t, DB.Exec(`INSERT INTO tasks (id, task_id, user_id, channel_id, quota) VALUES (0, '', 0, 0, 0)`).Error)
	require.NoError(t, MigrateLegacyTaskObligations(DB))
	var rows []LegacyTaskObligation
	require.NoError(t, DB.Order("source_task_id").Find(&rows).Error)
	require.Len(t, rows, 2)
	assert.EqualValues(t, -1, rows[0].SourceTaskID)
	assert.Equal(t, 123, rows[0].UserID)
	assert.Equal(t, 456, rows[0].ProviderID)
	assert.Equal(t, 789, rows[0].TokenID)
	assert.Equal(t, 999, rows[0].SubscriptionID)
	assert.Empty(t, rows[0].RequestID)
	assert.Empty(t, rows[0].PluginKey)
	assert.EqualValues(t, 0, rows[1].SourceTaskID)
	require.NoError(t, DB.Where("id = ?", orphan.ID).Delete(&Task{}).Error)
	require.NoError(t, MigrateLegacyTaskObligations(DB))
	var retained []LegacyTaskObligation
	require.NoError(t, DB.Order("source_task_id").Find(&retained).Error)
	assert.Equal(t, rows, retained, "removing an operational task cannot delete its evidence")
	assert.False(t, DB.Migrator().HasTable(&Wallet{}))
	assert.False(t, DB.Migrator().HasTable(&LedgerEntry{}))
	if common.LogDatabaseType() == common.DatabaseTypeClickHouse {
		assert.False(t, LOG_DB.Migrator().HasTable(&LegacyTaskObligation{}), "evidence belongs exclusively to primary PostgreSQL")
	}
}

func TestLegacySubscriptionTaskObligationsPreserveUnknownEvidence(t *testing.T) {
	user, sub := subscriptionFundingFixture(t, "subscription_only", 1_000_000_000, 500_000_000, false, false)
	require.NoError(t, DB.AutoMigrate(&Task{}))
	task := Task{TaskID: "legacy-private-task", UserId: user.Id, ChannelId: 71, Platform: "sora", Status: TaskStatusSuccess,
		CreatedAt: 11, UpdatedAt: 12, Quota: 0, PrivateData: TaskPrivateData{BillingSource: "subscription", SubscriptionId: sub.Id,
			Key: "must-not-copy-credential", PluginState: common.RawMessage(`{"secret":"must-not-copy-plugin-state"}`)},
		Data: common.RawMessage(`{"credential":"must-not-copy-result-body"}`)}
	require.NoError(t, DB.Create(&task).Error)
	failed := task
	failed.ID, failed.TaskID, failed.Status, failed.Quota = 0, "failed-task", TaskStatusFailure, 9_007_199_254_740_993
	failed.PrivateData.Execution = &TaskExecutionSnapshot{RequestID: "proven-original-request"}
	require.NoError(t, DB.Create(&failed).Error)
	batch := make([]Task, 250)
	for i := range batch {
		batch[i] = task
		batch[i].ID, batch[i].TaskID, batch[i].Status, batch[i].Quota = 0, fmt.Sprintf("batch-%d", i), TaskStatusInProgress, i
	}
	require.NoError(t, DB.Create(&batch).Error)
	modern := task
	modern.ID, modern.TaskID, modern.PrivateData.Gateway = 0, "modern-task", &TaskGatewaySnapshot{}
	require.NoError(t, DB.Create(&modern).Error)
	walletTask := task
	walletTask.ID, walletTask.TaskID, walletTask.PrivateData.BillingSource = 0, "wallet-task", "wallet"
	require.NoError(t, DB.Create(&walletTask).Error)
	var beforeEntries int64
	require.NoError(t, DB.Model(&LedgerEntry{}).Count(&beforeEntries).Error)
	require.NoError(t, MigrateSubscriptionMoney(DB))
	require.True(t, DB.Migrator().HasTable("legacy_subscription_task_obligations"), "a legacy task without a preconsume row still carries unverified subscription evidence")
	var rows []LegacySubscriptionTaskObligation
	require.NoError(t, DB.Order("source_task_id").Find(&rows).Error)
	require.Len(t, rows, 252, "batch boundaries must not lose evidence or include modern/wallet funding")
	assert.Equal(t, task.ID, rows[0].SourceTaskID)
	assert.Equal(t, user.Id, rows[0].UserID)
	assert.Equal(t, sub.Id, rows[0].SubscriptionID)
	assert.Equal(t, 71, rows[0].ProviderID)
	assert.Equal(t, task.TaskID, rows[0].TaskID)
	assert.Equal(t, "sora", rows[0].Platform)
	assert.Empty(t, rows[0].RequestID, "the task ID is not a request ID")
	assert.Zero(t, rows[0].SourceQuota)
	assert.Equal(t, string(TaskStatusSuccess), rows[0].SourceStatus)
	assert.EqualValues(t, 11, rows[0].SourceCreatedAt)
	assert.EqualValues(t, 12, rows[0].SourceUpdatedAt)
	assert.Equal(t, "proven-original-request", rows[1].RequestID)
	assert.EqualValues(t, 9_007_199_254_740_993, rows[1].SourceQuota, "original integer evidence is not converted through float or money")
	assert.Equal(t, string(TaskStatusFailure), rows[1].SourceStatus)
	for _, row := range rows {
		assert.Equal(t, "unverified", row.State)
	}
	encoded, err := common.Marshal(rows)
	require.NoError(t, err)
	for _, secret := range []string{"must-not-copy-credential", "must-not-copy-plugin-state", "must-not-copy-result-body"} {
		assert.NotContains(t, string(encoded), secret)
	}
	var sourceAfter Task
	require.NoError(t, DB.First(&sourceAfter, task.ID).Error)
	assert.Equal(t, task, sourceAfter, "capture does not mutate or strip the historical source")
	require.NoError(t, DB.Model(&Task{}).Where("id = ?", task.ID).Updates(map[string]any{"status": TaskStatusFailure, "quota": 333, "updated_at": 999, "finish_time": 998}).Error)
	for range 2 {
		require.NoError(t, MigrateSubscriptionMoney(DB))
	}
	var repeated []LegacySubscriptionTaskObligation
	require.NoError(t, DB.Order("source_task_id").Find(&repeated).Error)
	assert.Equal(t, rows, repeated, "normal task transitions do not rewrite the original evidence")
	var afterEntries int64
	require.NoError(t, DB.Model(&LedgerEntry{}).Count(&afterEntries).Error)
	assert.Equal(t, beforeEntries, afterEntries, "capturing unknown evidence never creates financial postings")
	require.Error(t, DB.Model(&LegacySubscriptionTaskObligation{}).Where("source_task_id = ?", task.ID).Update("state", "settled").Error)
	require.Error(t, DB.Where("source_task_id = ?", task.ID).Delete(&LegacySubscriptionTaskObligation{}).Error)
	require.Error(t, DB.Exec(`TRUNCATE TABLE "legacy_subscription_task_obligations"`).Error)
}

func TestLegacySubscriptionTaskObligationsKeepDeletedParentsButRejectOwnerConflicts(t *testing.T) {
	for _, state := range []string{"deleted_parent", "matching_parent", "conflicting_owner"} {
		t.Run(state, func(t *testing.T) {
			useWalletLedgerDB(t)
			require.NoError(t, DB.AutoMigrate(&Task{}, &UserSubscription{}, &LegacySubscriptionTaskObligation{}))
			parent := UserSubscription{Id: 21, UserId: 11, MoneyVersion: 1, Allowance: usd(0)}
			if state == "conflicting_owner" {
				parent.UserId = 12
			}
			require.NoError(t, DB.Create(&parent).Error)
			if state == "deleted_parent" {
				require.NoError(t, DB.Delete(&parent).Error)
			}
			source := Task{UserId: 11, ChannelId: 71, TaskID: "original-task", Platform: "sora", Status: TaskStatusSuccess, Quota: 0,
				PrivateData: TaskPrivateData{BillingSource: "subscription", SubscriptionId: 21, Execution: &TaskExecutionSnapshot{RequestID: "original-request"}}}
			require.NoError(t, DB.Create(&source).Error)
			err := DB.Transaction(migrateLegacySubscriptionTaskObligations)
			if state == "conflicting_owner" {
				require.ErrorContains(t, err, "owner")
				var count int64
				require.NoError(t, DB.Model(&LegacySubscriptionTaskObligation{}).Count(&count).Error)
				assert.Zero(t, count)
				return
			}
			require.NoError(t, err)
			var original LegacySubscriptionTaskObligation
			require.NoError(t, DB.First(&original, "source_task_id = ?", source.ID).Error)
			assert.Equal(t, 11, original.UserID)
			assert.Equal(t, 21, original.SubscriptionID)
			assert.Equal(t, 71, original.ProviderID)
			assert.Equal(t, "original-task", original.TaskID)
			assert.Equal(t, "original-request", original.RequestID)
			assert.Equal(t, "unverified", original.State)
			assert.Zero(t, original.SourceQuota)
			for range 2 {
				require.NoError(t, DB.Transaction(migrateLegacySubscriptionTaskObligations))
			}
			var repeated []LegacySubscriptionTaskObligation
			require.NoError(t, DB.Find(&repeated).Error)
			assert.Equal(t, []LegacySubscriptionTaskObligation{original}, repeated)
			var ledgerCount int64
			require.NoError(t, DB.Model(&LedgerEntry{}).Count(&ledgerCount).Error)
			assert.Zero(t, ledgerCount, "orphan evidence cannot create refunds, grants or revenue")
			if state == "matching_parent" {
				require.NoError(t, DB.Model(&UserSubscription{}).Where("id = ?", parent.Id).UpdateColumn("user_id", 12).Error)
				require.ErrorContains(t, DB.Transaction(migrateLegacySubscriptionTaskObligations), "owner")
			}
		})
	}
}

func TestLegacySubscriptionTaskObligationsRejectIdentityRebinding(t *testing.T) {
	for _, field := range []string{"owner", "subscription", "provider", "task", "platform", "request", "billing_source", "gateway"} {
		t.Run(field, func(t *testing.T) {
			useWalletLedgerDB(t)
			require.NoError(t, DB.AutoMigrate(&Task{}, &UserSubscription{}, &LegacySubscriptionTaskObligation{}))
			source := Task{ID: 1000, UserId: 1, ChannelId: 71, TaskID: "original-task", Platform: "sora", Status: TaskStatusInProgress,
				PrivateData: TaskPrivateData{BillingSource: "subscription", SubscriptionId: 2, Execution: &TaskExecutionSnapshot{RequestID: "original-request"}}}
			require.NoError(t, DB.Create(&source).Error)
			require.NoError(t, DB.Transaction(migrateLegacySubscriptionTaskObligations))
			var original LegacySubscriptionTaskObligation
			require.NoError(t, DB.First(&original, "source_task_id = ?", source.ID).Error)
			// A complete first batch is inserted before the changed identity is
			// reached. Failure must roll that batch back, not leave partial capture.
			batch := make([]Task, 250)
			for i := range batch {
				batch[i] = Task{UserId: 1, ChannelId: 71, TaskID: fmt.Sprintf("new-%d", i), Platform: "sora",
					PrivateData: TaskPrivateData{BillingSource: "subscription", SubscriptionId: 2}}
			}
			require.NoError(t, DB.Create(&batch).Error)
			changes := map[string]any{}
			switch field {
			case "owner":
				changes["user_id"] = 7
			case "provider":
				changes["channel_id"] = 72
			case "task":
				changes["task_id"] = "different-task"
			case "platform":
				changes["platform"] = "another-platform"
			case "subscription":
				source.PrivateData.SubscriptionId = 3
			case "request":
				source.PrivateData.Execution.RequestID = "different-request"
			case "billing_source":
				source.PrivateData.BillingSource = "wallet"
			case "gateway":
				source.PrivateData.Gateway = &TaskGatewaySnapshot{}
			}
			if len(changes) == 0 {
				encoded, err := common.Marshal(source.PrivateData)
				require.NoError(t, err)
				changes["private_data"] = string(encoded)
			}
			require.NoError(t, DB.Model(&Task{}).Where("id = ?", source.ID).Updates(changes).Error)
			err := DB.Transaction(migrateLegacySubscriptionTaskObligations)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "task 1000")
			var retained []LegacySubscriptionTaskObligation
			require.NoError(t, DB.Find(&retained).Error)
			assert.Equal(t, []LegacySubscriptionTaskObligation{original}, retained)
		})
	}
}

func TestLegacySubscriptionTaskObligationsRejectMalformedEvidence(t *testing.T) {
	for _, document := range []string{
		`[]`,
		`{"billing_source":123,"key":"must-not-leak"}`,
		`{"billing_source":"subscription","subscription_id":"must-not-leak"}`,
		`{"billing_source":"subscription","subscription_id":2,"execution":[]}`,
		`{"billing_source":"subscription","subscription_id":2,"execution":{"request_id":{"key":"must-not-leak"}}}`,
		`{"billing_source":"subscription","subscription_id":2,"gateway":false}`,
	} {
		t.Run(fmt.Sprintf("case-%d", len(document)), func(t *testing.T) {
			useWalletLedgerDB(t)
			require.NoError(t, DB.AutoMigrate(&Task{}, &UserSubscription{}, &LegacySubscriptionTaskObligation{}))
			batch := make([]Task, 251)
			for i := range batch {
				batch[i] = Task{UserId: 1, ChannelId: 71, TaskID: fmt.Sprintf("source-%d", i), Platform: "sora",
					PrivateData: TaskPrivateData{BillingSource: "subscription", SubscriptionId: 2}}
			}
			require.NoError(t, DB.Create(&batch).Error)
			require.NoError(t, DB.Model(&Task{}).Where("id = ?", batch[250].ID).UpdateColumn("private_data", document).Error)
			err := DB.Transaction(migrateLegacySubscriptionTaskObligations)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "task 251")
			assert.NotContains(t, err.Error(), "must-not-leak")
			var count int64
			require.NoError(t, DB.Model(&LegacySubscriptionTaskObligation{}).Count(&count).Error)
			assert.Zero(t, count, "an invalid later batch rolls back all earlier captures")
		})
	}
}

func subscriptionFundingIntent(request GatewayRequest, sale int64) GatewaySettlement {
	request.Status, request.Receivable = "success", usd(sale)
	request.StartedAt, request.EndedAt = clock.Now().UnixMilli(), clock.Now().UnixMilli()
	return GatewaySettlement{Request: request, SaleSource: usd(sale), Attempts: []GatewayAttempt{{ID: request.ID + "-attempt", RequestID: request.ID, ProviderID: 17, Cost: usd(50_000_000), ConvertedCost: usd(50_000_000), AccountingFXRate: "1"}}}
}

func TestBalanceReminderActualConsumptionCrossings(t *testing.T) {
	for _, source := range []string{"wallet_only", "subscription_only"} {
		t.Run(source, func(t *testing.T) {
			user, sub := subscriptionFundingFixture(t, source, 1_500_000_000, 1_500_000_000, false, false)
			require.NoError(t, MigrateBalanceReminders(DB))
			threshold := usd(1_000_000_000)
			_, err := PatchUserNotificationSettings(t.Context(), user.Id, &BalanceReminderSettings{Enabled: true, Threshold: &threshold}, nil)
			require.NoError(t, err)
			for i, sale := range []int64{400_000_000, 200_000_000, 300_000_000, 1_200_000_000} {
				if i == 3 {
					if source == "wallet_only" {
						_, err = TopUpWallet(WalletTopUp{UserId: user.Id, Source: usd(1_500_000_000), Reference: "reminder-rearm"})
					} else {
						_, err = PostLedgerEntries(LedgerPosting{Account: LedgerAccount{Type: LedgerAccountSubscription, OwnerId: int(sub.CurrentPeriodID), Currency: "USD"}, Type: LedgerEntryAdjustment, Amount: usd(1_500_000_000), IdempotencyKey: "reminder-rearm"})
					}
					require.NoError(t, err)
				}
				request := GatewayRequest{ID: fmt.Sprintf("reminder-%d", i), UserID: user.Id, SaleFXRate: "1"}
				require.NoError(t, ReserveGateway(&request, usd(sale), usd(sale)))
				var before int64
				require.NoError(t, DB.Model(&balanceReminderEvent{}).Count(&before).Error)
				assert.EqualValues(t, []int{0, 0, 1, 1}[i], before, "holds do not consume")
				require.NoError(t, PrepareGatewaySettlement(subscriptionFundingIntent(request, sale)))
				_, err = SettleGateway(request.ID)
				require.NoError(t, err)
				_, err = SettleGateway(request.ID)
				require.NoError(t, err)
				var count int64
				require.NoError(t, DB.Model(&balanceReminderEvent{}).Count(&count).Error)
				assert.EqualValues(t, []int{0, 1, 1, 2}[i], count, "only a new crossing emits; replay does not")
			}
		})
	}
}

func TestBalanceReminderFinancialBoundaries(t *testing.T) {
	t.Run("unlimited-and-old-period", func(t *testing.T) {
		user, sub := subscriptionFundingFixture(t, "subscription_only", 0, 0, true, false)
		threshold := usd(1_000_000_000)
		_, err := PatchUserNotificationSettings(t.Context(), user.Id, &BalanceReminderSettings{Enabled: true, Threshold: &threshold}, nil)
		require.NoError(t, err)
		request := GatewayRequest{ID: "unlimited-reminder", UserID: user.Id, SaleFXRate: "1"}
		require.NoError(t, ReserveGateway(&request, usd(100), usd(100)))
		require.NoError(t, PrepareGatewaySettlement(subscriptionFundingIntent(request, 200)))
		_, err = SettleGateway(request.ID)
		require.NoError(t, err)
		var count int64
		require.NoError(t, DB.Model(&balanceReminderEvent{}).Count(&count).Error)
		assert.Zero(t, count)
		var charge LedgerEntry
		require.NoError(t, DB.Where("request_id = ? AND account_type = ? AND type = ?", request.ID, LedgerAccountSubscriptionUnlimited, LedgerEntryCharge).Take(&charge).Error)
		assert.Equal(t, usd(-200), charge.Amount)
		observation, err := observeBalanceReminder(DB, user.Id+1, LedgerAccount{Type: LedgerAccountSubscriptionUnlimited, OwnerId: int(sub.CurrentPeriodID), Currency: "USD"})
		require.ErrorIs(t, err, ErrLedgerInvalidEntry)
		assert.Nil(t, observation)
	})
	t.Run("old-finite-period", func(t *testing.T) {
		user, sub := subscriptionFundingFixture(t, "subscription_only", 0, 1_500_000_000, false, false)
		threshold := usd(1_000_000_000)
		_, err := PatchUserNotificationSettings(t.Context(), user.Id, &BalanceReminderSettings{Enabled: true, Threshold: &threshold}, nil)
		require.NoError(t, err)
		request := GatewayRequest{ID: "old-period-reminder", UserID: user.Id, SaleFXRate: "1"}
		require.NoError(t, ReserveGateway(&request, usd(600_000_000), usd(600_000_000)))
		oldPeriod := sub.CurrentPeriodID
		require.NoError(t, DB.Transaction(func(tx *gorm.DB) error {
			locked, err := lockSubscriptionLifecycleTx(tx, sub.Id)
			if err != nil {
				return err
			}
			if err := createSubscriptionPeriodTx(tx, locked, usd(0), "reset"); err != nil {
				return err
			}
			sub = *locked
			return nil
		}))
		require.NotEqual(t, oldPeriod, sub.CurrentPeriodID)
		require.NoError(t, PrepareGatewaySettlement(subscriptionFundingIntent(request, 600_000_000)))
		_, err = SettleGateway(request.ID)
		require.NoError(t, err)
		var count int64
		require.NoError(t, DB.Model(&balanceReminderEvent{}).Count(&count).Error)
		assert.Zero(t, count)
		var charge LedgerEntry
		require.NoError(t, DB.Where("request_id = ? AND account_type = ? AND type = ?", request.ID, LedgerAccountSubscription, LedgerEntryCharge).Take(&charge).Error)
		assert.Equal(t, usd(-600_000_000), charge.Amount)
	})
	t.Run("funding-pending-recovery", func(t *testing.T) {
		user, _ := subscriptionFundingFixture(t, "subscription_only", 0, 1_000_000_000, false, false)
		threshold := usd(500_000_000)
		_, err := PatchUserNotificationSettings(t.Context(), user.Id, &BalanceReminderSettings{Enabled: true, Threshold: &threshold}, nil)
		require.NoError(t, err)
		a := GatewayRequest{ID: "reminder-recovery-a", UserID: user.Id, SaleFXRate: "1"}
		b := GatewayRequest{ID: "reminder-recovery-b", UserID: user.Id, SaleFXRate: "1"}
		for _, request := range []*GatewayRequest{&a, &b} {
			require.NoError(t, ReserveGateway(request, usd(500_000_000), usd(500_000_000)))
		}
		require.NoError(t, PrepareGatewaySettlement(subscriptionFundingIntent(a, 700_000_000)))
		_, err = SettleGateway(a.ID)
		require.ErrorIs(t, err, ErrGatewayFundingPending)
		var count int64
		require.NoError(t, DB.Model(&balanceReminderEvent{}).Count(&count).Error)
		assert.Zero(t, count)
		require.NoError(t, PrepareGatewaySettlement(subscriptionFundingIntent(b, 200_000_000)))
		_, err = SettleGateway(b.ID)
		require.NoError(t, err)
		_, err = RecoverGatewaySettlements(t.Context(), 10)
		require.NoError(t, err)
		var events []balanceReminderEvent
		require.NoError(t, DB.Find(&events).Error)
		require.Len(t, events, 1)
		assert.Equal(t, "subscription", events[0].Scope)
		assert.Equal(t, "800000000", events[0].BeforeNanos.String())
		assert.Equal(t, "100000000", events[0].AfterNanos.String())
	})
	for _, mode := range []string{"evaluation-pending", "unknown-sale", "zero-sale", "disabled-user", "disabled-site"} {
		t.Run(mode, func(t *testing.T) {
			user, _ := subscriptionFundingFixture(t, "wallet_only", 1_500_000_000, 0, true, false)
			threshold := usd(1_000_000_000)
			_, err := PatchUserNotificationSettings(t.Context(), user.Id, &BalanceReminderSettings{Enabled: mode != "disabled-user", Threshold: &threshold}, nil)
			require.NoError(t, err)
			if mode == "disabled-site" {
				require.NoError(t, DB.Model(&Option{}).Where("key = ?", "balance_reminder_setting.config").UpdateColumn("value", `{"enabled":false,"threshold":{"nanos":"1000000000","currency":"USD"}}`).Error)
			}
			request := GatewayRequest{ID: "reminder-" + mode, UserID: user.Id, SaleFXRate: "1"}
			require.NoError(t, ReserveGateway(&request, usd(600_000_000), usd(600_000_000)))
			intent := subscriptionFundingIntent(request, 600_000_000)
			if mode == "evaluation-pending" {
				intent.EvaluationFailed = true
			}
			if mode == "unknown-sale" {
				intent.SaleEvaluationFailed = true
				intent.EvaluationFailed = true
				intent.Request.SalePending = true
				intent.Request.Receivable = usd(0)
				intent.SaleSource = usd(0)
			}
			if mode == "zero-sale" {
				intent.Request.Receivable = usd(0)
				intent.SaleSource = usd(0)
			}
			require.NoError(t, PrepareGatewaySettlement(intent))
			_, err = SettleGateway(request.ID)
			if mode == "evaluation-pending" || mode == "unknown-sale" {
				require.ErrorIs(t, err, ErrGatewayEvaluationPending)
			} else {
				require.NoError(t, err)
			}
			var count int64
			require.NoError(t, DB.Model(&balanceReminderEvent{}).Count(&count).Error)
			if mode == "evaluation-pending" {
				assert.EqualValues(t, 1, count)
			} else {
				assert.Zero(t, count)
			}
		})
	}
	t.Run("signed-shortfall-and-debt-repayment", func(t *testing.T) {
		user, _ := subscriptionFundingFixture(t, "wallet_only", 1_000_000_000, 0, true, false)
		threshold := usd(0)
		_, err := PatchUserNotificationSettings(t.Context(), user.Id, &BalanceReminderSettings{Enabled: true, Threshold: &threshold}, nil)
		require.NoError(t, err)
		request := GatewayRequest{ID: "reminder-debt", UserID: user.Id, SaleFXRate: "1"}
		require.NoError(t, ReserveGateway(&request, usd(500_000_000), usd(500_000_000)))
		require.NoError(t, PrepareGatewaySettlement(subscriptionFundingIntent(request, 2_000_000_000)))
		_, err = SettleGateway(request.ID)
		require.NoError(t, err)
		var events []balanceReminderEvent
		require.NoError(t, DB.Find(&events).Error)
		require.Len(t, events, 1)
		assert.Equal(t, "1000000000", events[0].BeforeNanos.String())
		assert.Equal(t, "-1000000000", events[0].AfterNanos.String())
		_, err = TopUpWallet(WalletTopUp{UserId: user.Id, Source: usd(500_000_000), Reference: "partial-repayment"})
		require.NoError(t, err)
		var count int64
		require.NoError(t, DB.Model(&balanceReminderEvent{}).Count(&count).Error)
		assert.EqualValues(t, 1, count)
	})
	t.Run("unbounded-derived-total", func(t *testing.T) {
		user, _ := subscriptionFundingFixture(t, "wallet_only", math.MaxInt64-1000, 0, true, false)
		threshold := usd(math.MaxInt64 - 100)
		_, err := PatchUserNotificationSettings(t.Context(), user.Id, &BalanceReminderSettings{Enabled: true, Threshold: &threshold}, nil)
		require.NoError(t, err)
		a := GatewayRequest{ID: "reminder-big-hold", UserID: user.Id, SaleFXRate: "1"}
		b := GatewayRequest{ID: "reminder-small-hold", UserID: user.Id, SaleFXRate: "1"}
		require.NoError(t, ReserveGateway(&a, usd(10000), usd(10000)))
		require.NoError(t, ReserveGateway(&b, usd(100), usd(100)))
		_, err = TopUpWallet(WalletTopUp{UserId: user.Id, Source: usd(10000), Reference: "topup-during-holds"})
		require.NoError(t, err)
		require.NoError(t, PrepareGatewaySettlement(subscriptionFundingIntent(b, 20000)))
		_, err = SettleGateway(b.ID)
		require.NoError(t, err)
		var event balanceReminderEvent
		require.NoError(t, DB.Take(&event).Error)
		assert.Equal(t, "9223372036854784807", event.BeforeNanos.String())
		assert.Equal(t, "9223372036854764807", event.AfterNanos.String())
	})
	t.Run("outbox-error-rolls-back-funds", func(t *testing.T) {
		user, _ := subscriptionFundingFixture(t, "wallet_only", 1_500_000_000, 0, true, false)
		threshold := usd(1_000_000_000)
		_, err := PatchUserNotificationSettings(t.Context(), user.Id, &BalanceReminderSettings{Enabled: true, Threshold: &threshold}, nil)
		require.NoError(t, err)
		request := GatewayRequest{ID: "reminder-rollback", UserID: user.Id, SaleFXRate: "1"}
		require.NoError(t, ReserveGateway(&request, usd(600_000_000), usd(600_000_000)))
		require.NoError(t, PrepareGatewaySettlement(subscriptionFundingIntent(request, 600_000_000)))
		require.NoError(t, DB.Exec(`CREATE FUNCTION reject_reminder() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'outbox unavailable'; END $$; CREATE TRIGGER reject_reminder BEFORE INSERT ON balance_reminder_events FOR EACH ROW EXECUTE FUNCTION reject_reminder()`).Error)
		_, err = SettleGateway(request.ID)
		require.Error(t, err)
		var charge int64
		require.NoError(t, DB.Model(&LedgerEntry{}).Where("request_id = ? AND type = ?", request.ID, LedgerEntryCharge).Count(&charge).Error)
		assert.Zero(t, charge)
		require.NoError(t, DB.Exec("DROP TRIGGER reject_reminder ON balance_reminder_events").Error)
		_, err = SettleGateway(request.ID)
		require.NoError(t, err)
		var count int64
		require.NoError(t, DB.Model(&balanceReminderEvent{}).Count(&count).Error)
		assert.EqualValues(t, 1, count)
	})
}

func TestSubscriptionFundingPreferencesChooseOneWholeSource(t *testing.T) {
	for _, tc := range []struct {
		name, preference    string
		wallet, allowance   int64
		unlimited, overflow bool
		source              string
	}{
		{"subscription_only", "subscription_only", 1_000_000_000, 500_000_000, false, true, "subscription"},
		{"strict_insufficient", "subscription_only", 1_000_000_000, 50_000_000, false, true, ""},
		{"subscription_first", "subscription_first", 1_000_000_000, 500_000_000, false, true, "subscription"},
		{"subscription_fallback", "subscription_first", 1_000_000_000, 50_000_000, false, true, "wallet"},
		{"overflow_disabled", "subscription_first", 1_000_000_000, 50_000_000, false, false, ""},
		{"wallet_first", "wallet_first", 1_000_000_000, 500_000_000, false, true, "wallet"},
		{"wallet_fallback", "wallet_first", 50_000_000, 500_000_000, false, false, "subscription"},
		{"wallet_only", "wallet_only", 1_000_000_000, 500_000_000, false, true, "wallet"},
		{"wallet_strict", "wallet_only", 50_000_000, 500_000_000, false, true, ""},
		{"unlimited", "subscription_only", 0, 0, true, false, "subscription"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user, sub := subscriptionFundingFixture(t, tc.preference, tc.wallet, tc.allowance, tc.unlimited, tc.overflow)
			request := GatewayRequest{ID: "source-choice", UserID: user.Id, SaleFXRate: "1"}
			err := ReserveGateway(&request, usd(100_000_000), usd(100_000_000))
			if tc.source == "" {
				require.ErrorIs(t, err, ErrInsufficientWalletBalance)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.source, request.Funding.Source)
			var reserveEntries int64
			require.NoError(t, DB.Model(&LedgerEntry{}).Where("request_id = ? AND type = ?", request.ID, LedgerEntryPreConsume).Count(&reserveEntries).Error)
			assert.EqualValues(t, 1, reserveEntries)
			require.NoError(t, PrepareGatewaySettlement(subscriptionFundingIntent(request, 70_000_000)))
			_, err = SettleGateway(request.ID)
			require.NoError(t, err)
			_, err = SettleGateway(request.ID)
			require.NoError(t, err)
			wallet, err := GetUserWallet(user.Id)
			require.NoError(t, err)
			wantWallet := tc.wallet
			if tc.source == "wallet" {
				wantWallet -= 70_000_000
			}
			assert.Equal(t, usd(wantWallet), wallet.Balance)
			period, err := GetSubscriptionPeriodSummary(t.Context(), sub.Id)
			require.NoError(t, err)
			wantUsed := int64(0)
			if tc.source == "subscription" {
				wantUsed = 70_000_000
			}
			assert.Equal(t, usd(wantUsed), period.Consumed)
			assert.Equal(t, usd(0), period.Reserved)
			require.NoError(t, PublishGatewayRecords(request.ID))
			record, err := GetGatewayRequest(t.Context(), request.ID, user.Id)
			require.NoError(t, err)
			assert.Equal(t, request.Funding, record.Funding)
			rows, total, err := MarginReport(t.Context(), MarginFilter{}, 0, 10)
			require.NoError(t, err)
			require.Equal(t, 1, total)
			require.Len(t, rows, 1)
			assert.Equal(t, int64(70_000_000), rows[0].Revenue.Nanos)
			assert.Equal(t, int64(70_000_000), rows[0].Collected.Nanos)
		})
	}
}

func TestSubscriptionFundingPeriodSurvivesResetAndExpiry(t *testing.T) {
	user, sub := subscriptionFundingFixture(t, "subscription_only", 1_000_000_000, 500_000_000, false, false)
	request := GatewayRequest{ID: "original-period", UserID: user.Id, SaleFXRate: "1"}
	require.NoError(t, ReserveGateway(&request, usd(100_000_000), usd(100_000_000)))
	original := sub.CurrentPeriodID
	require.NoError(t, DB.Transaction(func(tx *gorm.DB) error {
		if _, err := lockSubscriptionFundingWallets(tx, []int{user.Id}); err != nil {
			return err
		}
		return createSubscriptionPeriodTx(tx, &sub, usd(0), "reset")
	}))
	require.NotEqual(t, original, sub.CurrentPeriodID)
	require.NoError(t, DB.Model(&sub).Updates(map[string]any{"status": "expired", "end_time": clock.Now().Unix() - 1}).Error)
	require.NoError(t, PrepareGatewaySettlement(subscriptionFundingIntent(request, 60_000_000)))
	_, err := SettleGateway(request.ID)
	require.NoError(t, err)
	current, err := GetSubscriptionPeriodSummary(t.Context(), sub.Id)
	require.NoError(t, err)
	assert.Equal(t, usd(500_000_000), current.Available)
	assert.Equal(t, usd(0), current.Consumed)
	var period SubscriptionPeriod
	require.NoError(t, DB.First(&period, original).Error)
	var account Wallet
	require.NoError(t, DB.Where("account_type = ? AND owner_id = ?", period.account().Type, original).First(&account).Error)
	assert.Equal(t, usd(440_000_000), account.Balance)
	wallet, err := GetUserWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, usd(1_000_000_000), wallet.Balance)
	require.Error(t, DB.Model(&period).Update("unlimited", true).Error, "a frozen entitlement period cannot change its funding contract")
}

func TestSubscriptionFundingOverrunStaysPendingUntilOriginalPeriodCorrection(t *testing.T) {
	user, sub := subscriptionFundingFixture(t, "subscription_only", 1_000_000_000, 1_000_000_000, false, true)
	request := GatewayRequest{ID: "funding-overrun", UserID: user.Id, SaleFXRate: "1"}
	require.NoError(t, ReserveGateway(&request, usd(200_000_000), usd(200_000_000)))
	original := sub.CurrentPeriodID
	require.NoError(t, PrepareGatewaySettlement(subscriptionFundingIntent(request, 1_200_000_000)))
	result, err := SettleGateway(request.ID)
	require.ErrorIs(t, err, ErrGatewayFundingPending)
	assert.True(t, result.FundingPending)
	assert.False(t, result.SalePending)
	assert.Equal(t, usd(1_200_000_000), result.Receivable)
	summary, err := GetUserWalletSummaries(t.Context(), []int{user.Id})
	require.NoError(t, err)
	require.Len(t, summary, 1)
	assert.True(t, summary[0].RequestsPaused)
	assert.False(t, summary[0].SalePending)
	assert.Equal(t, "1200000000", summary[0].Consumed.Nanos)
	assert.True(t, summary[0].FundingPending)
	pendingRecord, err := GetGatewayRequest(t.Context(), request.ID, user.Id)
	require.NoError(t, err)
	assert.True(t, pendingRecord.FundingPending)
	assert.False(t, pendingRecord.SalePending)
	assert.Equal(t, "settlement_pending", pendingRecord.Status)
	report, err := DashboardFinance(t.Context(), DashboardFinanceFilter{UserID: user.Id, From: clock.Now().Add(-time.Hour).UnixMilli(), Until: clock.Now().Add(time.Hour).UnixMilli(), Bucket: "hour", Timezone: "UTC", Admin: true})
	require.NoError(t, err)
	assert.Equal(t, "1", report.Totals.FundingPendingCount)
	require.NotNil(t, report.Totals.Revenue.TotalAmount)
	assert.Equal(t, "1200000000", report.Totals.Revenue.KnownAmount.Nanos)
	assert.Nil(t, report.Totals.Collected.TotalAmount)
	assert.Equal(t, "0", report.Totals.Collected.KnownAmount.Nanos)
	assert.Equal(t, "1", report.Totals.Collected.PendingCount)
	require.NotNil(t, report.Totals.Margin)
	require.NotNil(t, report.Totals.Margin.TotalAmount)
	assert.Equal(t, "1150000000", report.Totals.Margin.KnownAmount.Nanos)
	pendingMargins, _, err := MarginReport(t.Context(), MarginFilter{}, 0, 10)
	require.NoError(t, err)
	require.Len(t, pendingMargins, 1)
	assert.Equal(t, 1, pendingMargins[0].FundingPending)
	assert.Zero(t, pendingMargins[0].SalePending)
	require.NoError(t, DB.Model(&User{}).Where("id = ?", user.Id).Update("setting", `{"billing_preference":"wallet_only"}`).Error)
	require.NoError(t, DB.Transaction(func(tx *gorm.DB) error {
		if _, err := lockSubscriptionFundingWallets(tx, []int{user.Id}); err != nil {
			return err
		}
		return createSubscriptionPeriodTx(tx, &sub, usd(0), "reset")
	}))
	_, err = SettleGateway(request.ID)
	require.ErrorIs(t, err, ErrGatewayFundingPending)
	var entries []LedgerEntry
	require.NoError(t, DB.Where("request_id = ?", request.ID).Order("id").Find(&entries).Error)
	require.Len(t, entries, 3, "one hold, full revenue and known procurement; no wallet debit or release")
	completed, err := RecoverGatewaySettlements(t.Context(), 10)
	require.ErrorIs(t, err, ErrGatewayFundingPending)
	assert.Zero(t, completed)
	var oldPeriod SubscriptionPeriod
	require.NoError(t, DB.First(&oldPeriod, original).Error)
	_, err = PostLedgerEntries(LedgerPosting{Account: oldPeriod.account(), Type: LedgerEntryAdjustment, Amount: usd(200_000_000), IdempotencyKey: "verified-original-correction"})
	require.NoError(t, err)
	result, err = SettleGateway(request.ID)
	require.NoError(t, err)
	assert.False(t, result.FundingPending)
	_, err = SettleGateway(request.ID)
	require.NoError(t, err)
	wallet, err := GetUserWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, usd(1_000_000_000), wallet.Balance)
	current, err := GetSubscriptionPeriodSummary(t.Context(), sub.Id)
	require.NoError(t, err)
	assert.Equal(t, usd(1_000_000_000), current.Available)
	require.NoError(t, PublishGatewayRecords(request.ID))
	rows, _, err := MarginReport(t.Context(), MarginFilter{}, 0, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, int64(1_200_000_000), rows[0].Revenue.Nanos)
	assert.Equal(t, int64(1_200_000_000), rows[0].Collected.Nanos)
	assert.Equal(t, int64(50_000_000), rows[0].Cost.Nanos)
}

func TestSubscriptionFundingRecoveryUsesReleasedOriginalHold(t *testing.T) {
	user, sub := subscriptionFundingFixture(t, "subscription_only", 1_000_000_000, 1_000_000_000, false, false)
	a := GatewayRequest{ID: "earlier-overrun", UserID: user.Id, SaleFXRate: "1"}
	b := GatewayRequest{ID: "later-release", UserID: user.Id, SaleFXRate: "1"}
	require.NoError(t, ReserveGateway(&a, usd(500_000_000), usd(500_000_000)))
	require.NoError(t, ReserveGateway(&b, usd(500_000_000), usd(500_000_000)))
	require.NoError(t, PrepareGatewaySettlement(subscriptionFundingIntent(a, 700_000_000)))
	_, err := SettleGateway(a.ID)
	require.ErrorIs(t, err, ErrGatewayFundingPending)
	require.NoError(t, PrepareGatewaySettlement(subscriptionFundingIntent(b, 200_000_000)))
	_, err = SettleGateway(b.ID)
	require.NoError(t, err)
	require.NoError(t, PublishGatewayRecords(b.ID))
	before, err := GetSubscriptionPeriodSummary(t.Context(), sub.Id)
	require.NoError(t, err)
	assert.Equal(t, usd(300_000_000), before.Available)
	assert.Equal(t, usd(500_000_000), before.Reserved)
	completed, err := RecoverGatewaySettlements(t.Context(), 1)
	require.NoError(t, err)
	assert.Equal(t, 1, completed)
	var row GatewayBilling
	require.NoError(t, DB.First(&row, "request_id = ?", a.ID).Error)
	assert.Equal(t, "settled", row.Phase)
	assert.True(t, row.Published)
	after, err := GetSubscriptionPeriodSummary(t.Context(), sub.Id)
	require.NoError(t, err)
	assert.Equal(t, usd(100_000_000), after.Available)
	assert.Equal(t, usd(900_000_000), after.Consumed)
	assert.Equal(t, usd(0), after.Reserved)
	wallet, err := GetUserWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, usd(1_000_000_000), wallet.Balance)
	record, err := GetGatewayRequest(t.Context(), a.ID, user.Id)
	require.NoError(t, err)
	assert.Equal(t, a.Funding, record.Funding)
	assert.False(t, record.FundingPending)
	summaries, err := GetUserWalletSummaries(t.Context(), []int{user.Id})
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	assert.False(t, summaries[0].RequestsPaused)
	for range 2 {
		completed, err = RecoverGatewaySettlements(t.Context(), 1)
		require.NoError(t, err)
		assert.Zero(t, completed)
	}
	for _, account := range []LedgerAccountType{LedgerAccountPlatformRevenue, LedgerAccountProviderPayable, LedgerAccountSubscription} {
		var count int64
		require.NoError(t, DB.Model(&LedgerEntry{}).Where("request_id IN ? AND account_type = ? AND type = ?", []string{a.ID, b.ID}, account, LedgerEntryCharge).Count(&count).Error)
		assert.EqualValues(t, 2, count)
	}
}

func TestSubscriptionFundingRecoveryDoesNotStarveEitherQueue(t *testing.T) {
	user, sub := subscriptionFundingFixture(t, "subscription_only", 1_000_000_000, 1_000_000_000, false, false)
	requests := []GatewayRequest{
		{ID: "00-unpriced", UserID: user.Id, SaleFXRate: "1"},
		{ID: "01-blocked", UserID: user.Id, SaleFXRate: "1"},
		{ID: "02-blocked", UserID: user.Id, SaleFXRate: "1"},
		{ID: "03-ready-later", UserID: user.Id, SaleFXRate: "1"},
		{ID: "04-normal-release", UserID: user.Id, SaleFXRate: "1"},
	}
	holds := []int64{0, 200_000_000, 100_000_000, 400_000_000, 300_000_000}
	for i := range requests {
		require.NoError(t, ReserveGateway(&requests[i], usd(holds[i]), usd(holds[i])))
	}
	for i := 1; i <= 3; i++ {
		sale := int64(2_000_000_000)
		if i == 3 {
			sale = 600_000_000
		}
		require.NoError(t, PrepareGatewaySettlement(subscriptionFundingIntent(requests[i], sale)))
		_, err := SettleGateway(requests[i].ID)
		require.ErrorIs(t, err, ErrGatewayFundingPending)
	}
	unknown := subscriptionFundingIntent(requests[0], 0)
	unknown.SaleEvaluationFailed, unknown.EvaluationFailed, unknown.Request.SalePending = true, true, true
	require.NoError(t, PrepareGatewaySettlement(unknown))
	_, err := SettleGateway(requests[0].ID)
	require.ErrorIs(t, err, ErrGatewayEvaluationPending)
	var unpricedBefore GatewayBilling
	require.NoError(t, DB.First(&unpricedBefore, "request_id = ?", requests[0].ID).Error)
	require.NoError(t, PrepareGatewaySettlement(subscriptionFundingIntent(requests[4], 100_000_000)))
	completed, err := RecoverGatewaySettlements(t.Context(), 0)
	require.NoError(t, err)
	assert.Zero(t, completed)
	for i := range 3 {
		completed, err = RecoverGatewaySettlements(t.Context(), 1)
		require.Error(t, err, "unknown and permanently insufficient records remain explicit")
		want := 0
		if i == 0 || i == 2 {
			want = 1
		}
		assert.Equal(t, want, completed, "normal work and the later eligible funding row each make bounded progress")
		var ready GatewayBilling
		require.NoError(t, DB.First(&ready, "request_id = ?", requests[3].ID).Error)
		if i < 2 {
			assert.Equal(t, "funding_pending", ready.Phase, "one funding candidate per call, not an unbounded scan")
		} else {
			assert.Equal(t, "settled", ready.Phase, "insufficient older funding rows must rotate behind later rows")
			assert.True(t, ready.Published)
		}
	}
	var before []LedgerEntry
	require.NoError(t, DB.Order("id").Find(&before).Error)
	var group sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		group.Go(func() { _, errs[i] = RecoverGatewaySettlements(t.Context(), 1) })
	}
	group.Wait()
	for _, err := range errs {
		require.Error(t, err)
	}
	var after []LedgerEntry
	require.NoError(t, DB.Order("id").Find(&after).Error)
	assert.Equal(t, before, after, "repeated concurrent recovery cannot charge or recognize costs twice")
	var unpricedAfter GatewayBilling
	require.NoError(t, DB.First(&unpricedAfter, "request_id = ?", requests[0].ID).Error)
	assert.Equal(t, unpricedBefore, unpricedAfter, "unknown sale is never retried or repriced")
	period, err := GetSubscriptionPeriodSummary(t.Context(), sub.Id)
	require.NoError(t, err)
	assert.Equal(t, sub.CurrentPeriodID, period.PeriodID)
	assert.Equal(t, usd(700_000_000), period.Consumed)
	assert.Equal(t, usd(300_000_000), period.Reserved)
	assert.Equal(t, usd(0), period.Available)
	wallet, err := GetUserWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, usd(1_000_000_000), wallet.Balance)
}

func TestSubscriptionFundingRecoveryMigrationAndConcurrentClaims(t *testing.T) {
	user, _ := subscriptionFundingFixture(t, "subscription_only", 1_000_000_000, 1_000_000_000, false, false)
	requests := []GatewayRequest{
		{ID: "claim-a", UserID: user.Id, SaleFXRate: "1"},
		{ID: "claim-b", UserID: user.Id, SaleFXRate: "1"},
		{ID: "claim-c", UserID: user.Id, SaleFXRate: "1"},
	}
	for i := range requests {
		require.NoError(t, ReserveGateway(&requests[i], usd(100_000_000), usd(100_000_000)))
	}
	for _, request := range requests {
		require.NoError(t, PrepareGatewaySettlement(subscriptionFundingIntent(request, 2_000_000_000)))
		_, err := SettleGateway(request.ID)
		require.ErrorIs(t, err, ErrGatewayFundingPending)
	}
	var before []GatewayBilling
	require.NoError(t, DB.Order("request_id").Find(&before).Error)
	for _, row := range before {
		assert.Zero(t, row.FundingRecoveryOrder)
	}
	// Upgrade a populated pre-column lifecycle twice, without rebuilding its
	// frozen intents or financial entries. Released-source gates run separately.
	require.NoError(t, DB.Exec(`ALTER TABLE "gateway_billings" DROP COLUMN "funding_recovery_order"`).Error)
	for range 2 {
		require.NoError(t, MigrateGatewayBilling(DB))
	}
	require.True(t, DB.Migrator().HasIndex(&GatewayBilling{}, "idx_gateway_funding_recovery"))
	var after []GatewayBilling
	require.NoError(t, DB.Order("request_id").Find(&after).Error)
	assert.Equal(t, before, after)
	fresh := GatewayBilling{RequestID: "new-default", UserID: user.Id, Reservation: usd(0), Phase: "reserved"}
	require.NoError(t, DB.Omit("FundingRecoveryOrder").Create(&fresh).Error)
	require.NoError(t, DB.First(&fresh, "request_id = ?", fresh.RequestID).Error)
	assert.Zero(t, fresh.FundingRecoveryOrder)

	require.NoError(t, DB.Exec(`CREATE FUNCTION wait_funding_rotation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW."request_id" = 'claim-a' AND NEW."funding_recovery_order" <> OLD."funding_recovery_order" THEN PERFORM pg_advisory_xact_lock(451046); END IF; RETURN NEW; END $$`).Error)
	require.NoError(t, DB.Exec(`CREATE TRIGGER wait_funding_rotation AFTER UPDATE OF "funding_recovery_order" ON "gateway_billings" FOR EACH ROW EXECUTE FUNCTION wait_funding_rotation()`).Error)
	barrier := DB.Begin()
	require.NoError(t, barrier.Error)
	defer barrier.Rollback()
	require.NoError(t, barrier.Exec(`SELECT pg_advisory_xact_lock(451046)`).Error)
	type claimResult struct {
		rows []GatewayBilling
		err  error
	}
	first := make(chan claimResult, 1)
	go func() {
		rows, err := claimGatewayFundingRecovery(t.Context(), 1)
		first <- claimResult{rows, err}
	}()
	require.Eventually(t, func() bool {
		var waiting int64
		err := DB.Raw(`SELECT COUNT(*) FROM "pg_locks" l JOIN "pg_stat_activity" a ON a."pid" = l."pid" WHERE a."datname" = current_database() AND l."locktype" = 'advisory' AND l."objid" = 451046 AND NOT l."granted"`).Scan(&waiting).Error
		return err == nil && waiting == 1
	}, 5*time.Second, 5*time.Millisecond, "first worker must hold the candidate row while waiting inside rotation")
	second, err := claimGatewayFundingRecovery(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, second, 1)
	assert.Equal(t, "claim-b", second[0].RequestID, "another worker skips the locked candidate")
	_, err = SettleGateway(second[0].RequestID)
	require.ErrorIs(t, err, ErrGatewayFundingPending, "rotation committed before settlement takes its own row and account locks")
	require.NoError(t, barrier.Commit().Error)
	var result claimResult
	select {
	case result = <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("funding rotation did not finish after its lock was released")
	}
	require.NoError(t, result.err)
	require.Len(t, result.rows, 1)
	assert.Equal(t, "claim-a", result.rows[0].RequestID)
	third, err := claimGatewayFundingRecovery(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, third, 1)
	assert.Equal(t, "claim-c", third[0].RequestID, "neither earlier blocked row monopolizes the queue")
	// A worker may crash after rotation; there is no lease or claimed state to
	// strand its intent. The same row becomes eligible again on the next round.
	retry, err := claimGatewayFundingRecovery(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, retry, 1)
	assert.Equal(t, "claim-a", retry[0].RequestID)
	require.NoError(t, DB.Model(&GatewayBilling{}).Where("request_id = ?", "claim-a").UpdateColumn("funding_recovery_order", int64(math.MaxInt64)).Error)
	var exhausted []GatewayBilling
	require.NoError(t, DB.Order("request_id").Find(&exhausted).Error)
	_, err = claimGatewayFundingRecovery(t.Context(), 1)
	require.ErrorContains(t, err, "recovery order exhausted")
	var unchanged []GatewayBilling
	require.NoError(t, DB.Order("request_id").Find(&unchanged).Error)
	assert.Equal(t, exhausted, unchanged, "overflow aborts the claim transaction without wrapping or losing a position")
	require.Error(t, DB.Model(&GatewayBilling{}).Where("request_id = ?", "claim-b").UpdateColumn("funding_recovery_order", -1).Error)
}

func TestSubscriptionFundingJournalAndConcurrency(t *testing.T) {
	user, sub := subscriptionFundingFixture(t, "subscription_only", 1_000_000_000, 1_000_000_000, false, false)
	requests := []GatewayRequest{{ID: "concurrent-a", UserID: user.Id, SaleFXRate: "1"}, {ID: "concurrent-b", UserID: user.Id, SaleFXRate: "1"}}
	errs := make([]error, 2)
	var group sync.WaitGroup
	for i := range requests {
		group.Go(func() { errs[i] = ReserveGateway(&requests[i], usd(700_000_000), usd(700_000_000)) })
	}
	group.Wait()
	winner := 0
	if errs[0] != nil {
		winner = 1
	}
	require.NoError(t, errs[winner])
	require.ErrorIs(t, errs[1-winner], ErrInsufficientWalletBalance)
	intent := subscriptionFundingIntent(requests[winner], 0)
	_, _, err := journalGatewaySettlement(intent)
	require.NoError(t, err)
	completed, err := RecoverGatewaySettlements(t.Context(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, completed)
	completed, err = RecoverGatewaySettlements(t.Context(), 10)
	require.NoError(t, err)
	assert.Zero(t, completed)
	period, err := GetSubscriptionPeriodSummary(t.Context(), sub.Id)
	require.NoError(t, err)
	assert.Equal(t, usd(1_000_000_000), period.Available)
	assert.Equal(t, usd(0), period.Reserved)
	var record GatewayBilling
	require.NoError(t, DB.First(&record, "request_id = ?", requests[winner].ID).Error)
	var decoded GatewayRequest
	require.NoError(t, common.UnmarshalJsonStr(record.RequestData, &decoded))
	assert.Equal(t, requests[winner].Funding, decoded.Funding)
}

func TestSubscriptionFundingLazyResetUsesNewPeriodWithoutChangingOldHold(t *testing.T) {
	user, sub := subscriptionFundingFixture(t, "subscription_only", 1_000_000_000, 100_000_000, false, false)
	old := GatewayRequest{ID: "old-period-hold", UserID: user.Id, SaleFXRate: "1"}
	require.NoError(t, ReserveGateway(&old, usd(100_000_000), usd(100_000_000)))
	original := sub.CurrentPeriodID
	now := clock.Now().Unix()
	require.NoError(t, DB.Model(&SubscriptionPlan{}).Where("id = ?", sub.PlanId).Updates(map[string]any{"quota_reset_period": "custom", "quota_reset_custom_seconds": 60}).Error)
	require.NoError(t, DB.Model(&sub).Updates(map[string]any{"start_time": now - 120, "last_reset_time": now - 120, "next_reset_time": now - 60}).Error)
	next := GatewayRequest{ID: "new-period-hold", UserID: user.Id, SaleFXRate: "1"}
	require.NoError(t, ReserveGateway(&next, usd(50_000_000), usd(50_000_000)), "due reset is applied under the same ordered funding transaction")
	require.NotEqual(t, original, next.Funding.PeriodID)
	period, err := GetSubscriptionPeriodSummary(t.Context(), sub.Id)
	require.NoError(t, err)
	assert.EqualValues(t, 2, period.Sequence)
	assert.Equal(t, usd(50_000_000), period.Available)
	assert.Equal(t, usd(50_000_000), period.Reserved)
	require.NoError(t, PrepareGatewaySettlement(subscriptionFundingIntent(old, 60_000_000)))
	_, err = SettleGateway(old.ID)
	require.NoError(t, err)
	period, err = GetSubscriptionPeriodSummary(t.Context(), sub.Id)
	require.NoError(t, err)
	assert.Equal(t, usd(50_000_000), period.Available, "old-period settlement cannot release into the new period")
}

func TestSubscriptionFundingTaskReservationIsAtomic(t *testing.T) {
	user, sub := subscriptionFundingFixture(t, "subscription_only", 1_000_000_000, 500_000_000, false, false)
	require.NoError(t, DB.AutoMigrate(&Task{}))
	require.NoError(t, DB.Exec(`CREATE FUNCTION reject_funding_task() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'task recovery identity rejected'; END $$`).Error)
	require.NoError(t, DB.Exec(`CREATE TRIGGER reject_funding_task BEFORE INSERT ON "tasks" FOR EACH ROW EXECUTE FUNCTION reject_funding_task()`).Error)
	request := GatewayRequest{ID: "task-atomic", UserID: user.Id, SaleFXRate: "1"}
	task := Task{UserId: user.Id, TaskID: "public-task", Platform: constant.TaskPlatform("funding"), PrivateData: TaskPrivateData{Gateway: &TaskGatewaySnapshot{}}}
	require.ErrorContains(t, ReserveGatewayTask(&request, usd(100_000_000), usd(100_000_000), &task), "task recovery identity rejected")
	var count int64
	require.NoError(t, DB.Model(&GatewayBilling{}).Where("request_id = ?", request.ID).Count(&count).Error)
	assert.Zero(t, count)
	require.NoError(t, DB.Model(&LedgerEntry{}).Where("request_id = ?", request.ID).Count(&count).Error)
	assert.Zero(t, count)
	period, err := GetSubscriptionPeriodSummary(t.Context(), sub.Id)
	require.NoError(t, err)
	assert.Equal(t, usd(500_000_000), period.Available)
	require.NoError(t, DB.Exec(`DROP TRIGGER reject_funding_task ON "tasks"`).Error)
	require.NoError(t, ReserveGatewayTask(&request, usd(100_000_000), usd(100_000_000), &task))
	var stored Task
	require.NoError(t, DB.First(&stored, task.ID).Error)
	require.NotNil(t, stored.PrivateData.Gateway)
	assert.Equal(t, request.Funding, stored.PrivateData.Gateway.Settlement.Request.Funding)
	assert.Empty(t, stored.PrivateData.Key)
	require.NoError(t, PublishPendingGatewayRecords(GatewaySettlement{Request: GatewayRequest{ID: request.ID, UserID: user.Id, Status: "accepted"}}))
	accepted, err := GetGatewayRequest(t.Context(), request.ID, user.Id)
	require.NoError(t, err)
	assert.Equal(t, request.Funding, accepted.Funding, "the pending outbox must retain the authoritative reservation source")
	require.Error(t, ReserveGatewayTask(&request, usd(100_000_000), usd(100_000_000), &task), "a repeated public request must not obtain another hold or recovery identity")
	require.NoError(t, DB.Model(&LedgerEntry{}).Where("request_id = ? AND type = ?", request.ID, LedgerEntryPreConsume).Count(&count).Error)
	assert.EqualValues(t, 1, count)
}

func TestSubscriptionFundingWaitsForWalletBeforeUser(t *testing.T) {
	user, _ := subscriptionFundingFixture(t, "subscription_only", 1_000_000_000, 500_000_000, false, false)
	checkout := DB.Begin()
	require.NoError(t, checkout.Error)
	defer checkout.Rollback()
	var wallet Wallet
	require.NoError(t, lockForUpdate(checkout).Where("account_type = ? AND owner_id = ?", LedgerAccountUserWallet, user.Id).Take(&wallet).Error)
	waiting := make(chan struct{})
	var once sync.Once
	require.NoError(t, DB.Callback().Query().Before("gorm:query").Register("test_subscription_wallet_wait", func(tx *gorm.DB) {
		if tx.Statement.Table == "wallets" && tx.Statement.Clauses["FOR"].Name != "" {
			once.Do(func() { close(waiting) })
		}
	}))
	defer DB.Callback().Query().Remove("test_subscription_wallet_wait")
	finished := make(chan error, 1)
	go func() {
		request := GatewayRequest{ID: "ordered-reserve", UserID: user.Id, SaleFXRate: "1"}
		finished <- ReserveGateway(&request, usd(100_000_000), usd(100_000_000))
	}()
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("reservation did not reach the shared wallet lock")
	}
	require.NoError(t, checkout.Exec("SET LOCAL lock_timeout = '300ms'").Error)
	err := checkout.Model(&User{}).Where("id = ?", user.Id).UpdateColumn("request_count", gorm.Expr("request_count + 1")).Error
	assert.NoError(t, err, "reservation must not hold the user row while waiting for an existing wallet")
	if err != nil {
		require.NoError(t, checkout.Rollback().Error)
	} else {
		require.NoError(t, checkout.Commit().Error)
	}
	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("reservation did not finish after the wallet lock was released")
	}
}

func TestSubscriptionFundingUnknownSaleAndExplicitFreeStayDistinct(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprintf("unknown=%t", unknown), func(t *testing.T) {
			user, sub := subscriptionFundingFixture(t, "subscription_only", 1_000_000_000, 500_000_000, false, false)
			request := GatewayRequest{ID: "unknown-or-free", UserID: user.Id, SaleFXRate: "1"}
			reservation := int64(0)
			if unknown {
				reservation = 100_000_000
			}
			require.NoError(t, ReserveGateway(&request, usd(reservation), usd(reservation)))
			intent := subscriptionFundingIntent(request, 0)
			intent.EvaluationFailed, intent.SaleEvaluationFailed, intent.Request.SalePending = unknown, unknown, unknown
			require.NoError(t, PrepareGatewaySettlement(intent))
			result, err := SettleGateway(request.ID)
			if unknown {
				require.ErrorIs(t, err, ErrGatewayEvaluationPending)
			} else {
				require.NoError(t, err)
			}
			assert.False(t, result.FundingPending)
			assert.Equal(t, unknown, result.SalePending)
			period, err := GetSubscriptionPeriodSummary(t.Context(), sub.Id)
			require.NoError(t, err)
			assert.Equal(t, usd(reservation), period.Reserved)
			assert.Equal(t, usd(500_000_000-reservation), period.Available)
			var entries []LedgerEntry
			require.NoError(t, DB.Where("request_id = ?", request.ID).Find(&entries).Error)
			expected := 1
			if unknown {
				expected++
			}
			require.Len(t, entries, expected, "only the actual procurement and, for unknown sale, original hold are posted")
			for _, entry := range entries {
				assert.NotZero(t, entry.Amount.Nanos)
			}
			next := GatewayRequest{ID: "next-request", UserID: user.Id, SaleFXRate: "1"}
			err = ReserveGateway(&next, usd(1), usd(1))
			if unknown {
				require.ErrorIs(t, err, ErrInsufficientWalletBalance)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestSubscriptionFundingNeverFallsBackAfterStrictSelection(t *testing.T) {
	useWalletLedgerDB(t)
	t.Setenv("GATEWAY_SETTLEMENT_JOURNAL_DIR", t.TempDir())
	require.NoError(t, DB.AutoMigrate(&User{}))
	require.NoError(t, MigrateSubscriptionMoney(DB))
	require.NoError(t, MigrateGatewayBilling(DB))
	user := User{Username: "subscription-only", Setting: `{"billing_preference":"subscription_only"}`}
	require.NoError(t, DB.Create(&user).Error)
	_, err := TopUpWallet(WalletTopUp{UserId: user.Id, Source: usd(1_000_000_000), Reference: "strict-funding-credit"})
	require.NoError(t, err)
	request := GatewayRequest{ID: "strict-without-subscription", UserID: user.Id, SaleFXRate: "1"}
	err = ReserveGateway(&request, usd(100_000_000), usd(100_000_000))
	require.Error(t, err, "subscription_only must not silently reserve the wallet")
	wallet, err := GetUserWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, usd(1_000_000_000), wallet.Balance)
	var rows int64
	require.NoError(t, DB.Model(&GatewayBilling{}).Where("request_id = ?", request.ID).Count(&rows).Error)
	assert.Zero(t, rows, "failed source selection must roll back the lifecycle")
}
