package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"gorm.io/gorm"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func balanceReminderServiceFixture(t *testing.T, handler http.HandlerFunc) (model.User, *clock.Fake) {
	t.Helper()
	user, fake := balanceReminderRecipientFixture(t, handler)
	seedBalanceReminderCharge(t, user.Id, "reminder")
	var count int64
	require.NoError(t, model.DB.Table("balance_reminder_events").Count(&count).Error)
	require.EqualValues(t, 1, count, "actual charge must have committed one reminder with its ledger")
	return user, fake
}

func balanceReminderRecipientFixture(t *testing.T, handler http.HandlerFunc) (model.User, *clock.Fake) {
	t.Helper()
	fake := clock.UseFake(t, time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC))
	model.UseTestDatabases(t)
	t.Setenv("GATEWAY_SETTLEMENT_JOURNAL_DIR", t.TempDir())
	require.NoError(t, model.DB.AutoMigrate(&model.User{}, &model.Option{}))
	require.NoError(t, model.MigrateSubscriptionMoney(model.DB))
	require.NoError(t, model.MigrateGatewayBilling(model.DB))
	require.NoError(t, model.MigrateGatewayRecords())
	require.NoError(t, model.MigrateBalanceReminders(model.DB))
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	previousClient, previousWorker := httpClient, system_setting.WorkerUrl
	previousLimit, previousWindow := constant.NotifyLimitCount, constant.NotificationLimitDurationMinute
	fetch := system_setting.GetFetchSetting()
	previousFetch := *fetch
	t.Cleanup(func() {
		httpClient, system_setting.WorkerUrl = previousClient, previousWorker
		constant.NotifyLimitCount, constant.NotificationLimitDurationMinute = previousLimit, previousWindow
		*fetch = previousFetch
	})
	httpClient, system_setting.WorkerUrl = server.Client(), ""
	constant.NotifyLimitCount, constant.NotificationLimitDurationMinute = 100, 10
	fetch.EnableSSRFProtection = false
	settings, err := common.Marshal(dto.UserSetting{BillingPreference: "wallet_only", Language: "zhCN", NotifyType: dto.NotifyTypeWebhook, WebhookUrl: server.URL, WebhookSecret: "reminder-signature-canary"})
	require.NoError(t, err)
	user := model.User{Username: "reminder-recipient", Status: common.UserStatusEnabled, Setting: string(settings)}
	require.NoError(t, model.DB.Create(&user).Error)
	return user, fake
}

func seedBalanceReminderCharge(t *testing.T, userID int, reference string) {
	t.Helper()
	threshold := money.Amount{Nanos: 1_000_000_000, Currency: "USD"}
	_, err := model.PatchUserNotificationSettings(t.Context(), userID, &model.BalanceReminderSettings{Enabled: true, Threshold: &threshold}, nil)
	require.NoError(t, err)
	_, err = model.TopUpWallet(model.WalletTopUp{UserId: userID, Source: money.Amount{Nanos: 1_500_000_001, Currency: "USD"}, Reference: reference + "-opening"})
	require.NoError(t, err)
	request := model.GatewayRequest{ID: reference + "-real-charge", UserID: userID, SaleCurrency: "USD", SaleFXRate: "1", Status: "success"}
	charge := money.Amount{Nanos: 600_000_002, Currency: "USD"}
	require.NoError(t, model.ReserveGateway(&request, charge, charge))
	request.Receivable = charge
	require.NoError(t, model.PrepareGatewaySettlement(model.GatewaySettlement{Request: request, SaleSource: charge}))
	_, err = model.SettleGateway(request.ID)
	require.NoError(t, err)
}

func TestBalanceReminderDeliveryUsesDurableOutbox(t *testing.T) {
	t.Run("accepted-once-with-exact-saved-language", func(t *testing.T) {
		calls := 0
		var payload WebhookPayload
		user, _ := balanceReminderServiceFixture(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			assert.NotEmpty(t, r.Header.Get("X-Webhook-Signature"))
			assert.Empty(t, r.Header.Get("Authorization"))
			assert.NoError(t, common.DecodeJson(r.Body, &payload))
			w.WriteHeader(http.StatusNoContent)
		})
		var ledgerBefore int64
		require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Count(&ledgerBefore).Error)
		summary, err := RunBalanceReminders(t.Context(), "recipient-worker")
		require.NoError(t, err)
		assert.Equal(t, 1, summary.Accepted)
		assert.Equal(t, 1, calls)
		assert.Equal(t, "balance_reminder", payload.Type)
		assert.Contains(t, payload.Title, "钱包")
		assert.Contains(t, payload.Content, "0.899999999 USD")
		assert.Contains(t, payload.Content, "2026-11-17T12:00:00Z")
		assert.NotContains(t, payload.Content, "reminder-signature-canary")
		_, err = RunBalanceReminders(t.Context(), "after-restart")
		require.NoError(t, err)
		assert.Equal(t, 1, calls, "sent event is not redelivered after a fresh runner")
		var state struct {
			Status, Code string
			Attempts     int
		}
		require.NoError(t, model.DB.Table("balance_reminder_events").Take(&state).Error)
		assert.Equal(t, "sent", state.Status)
		assert.Equal(t, model.BalanceReminderDelivered, state.Code)
		assert.Equal(t, 1, state.Attempts)
		var ledgerAfter int64
		require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Count(&ledgerAfter).Error)
		assert.Equal(t, ledgerBefore, ledgerAfter)
		wallet, err := model.GetUserWallet(user.Id)
		require.NoError(t, err)
		assert.EqualValues(t, 899_999_999, wallet.Balance.Nanos)
	})
	t.Run("retry-persists-across-runners", func(t *testing.T) {
		calls := 0
		_, fake := balanceReminderServiceFixture(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, "provider-private-canary")
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
		summary, err := RunBalanceReminders(t.Context(), "first-process")
		require.NoError(t, err)
		assert.Equal(t, 1, summary.Retry)
		var state struct {
			Status, Code  string
			NextAttemptAt int64
		}
		require.NoError(t, model.DB.Table("balance_reminder_events").Take(&state).Error)
		assert.Equal(t, "retry", state.Status)
		assert.Equal(t, model.BalanceReminderDeliveryFailed, state.Code)
		assert.Greater(t, state.NextAttemptAt, clock.Now().UnixMilli())
		_, err = RunBalanceReminders(t.Context(), "too-early")
		require.NoError(t, err)
		assert.Equal(t, 1, calls)
		fake.Advance(time.Hour)
		summary, err = RunBalanceReminders(t.Context(), "next-process")
		require.NoError(t, err)
		assert.Equal(t, 1, summary.Accepted)
		assert.Equal(t, 2, calls)
	})
	t.Run("skipped-head-does-not-hide-next-recipient", func(t *testing.T) {
		calls := 0
		user, _ := balanceReminderServiceFixture(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusNoContent) })
		next := model.User{Username: "next-recipient", AffCode: "next-reminder", Status: common.UserStatusEnabled, Setting: user.Setting}
		require.NoError(t, model.DB.Create(&next).Error)
		seedBalanceReminderCharge(t, next.Id, "next")
		_, err := model.PatchUserNotificationSettings(t.Context(), user.Id, &model.BalanceReminderSettings{Enabled: false}, nil)
		require.NoError(t, err)
		summary, err := RunBalanceReminders(t.Context(), "drain-worker")
		require.NoError(t, err)
		assert.Equal(t, 1, summary.Accepted)
		assert.Equal(t, 1, calls)
		var skipped, sent int64
		require.NoError(t, model.DB.Table("balance_reminder_events").Where("user_id = ? AND status = ? AND code = ?", user.Id, "skipped", model.BalanceReminderDisabled).Count(&skipped).Error)
		require.NoError(t, model.DB.Table("balance_reminder_events").Where("user_id = ? AND status = ?", next.Id, "sent").Count(&sent).Error)
		assert.EqualValues(t, 1, skipped)
		assert.EqualValues(t, 1, sent)
	})
	t.Run("cancellation-retains-retry-and-stops-network", func(t *testing.T) {
		started, stopped := make(chan struct{}), make(chan struct{})
		_, _ = balanceReminderServiceFixture(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			close(started)
			<-r.Context().Done()
			close(stopped)
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		type outcome struct {
			summary BalanceReminderSummary
			err     error
		}
		finished := make(chan outcome, 1)
		go func() {
			summary, err := RunBalanceReminders(ctx, "cancelled-worker")
			finished <- outcome{summary, err}
		}()
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("reminder did not reach controlled transport")
		}
		cancel()
		select {
		case result := <-finished:
			assert.ErrorIs(t, result.err, context.Canceled)
			assert.Equal(t, 1, result.summary.Retry)
		case <-time.After(3 * time.Second):
			t.Fatal("reminder ignored caller cancellation")
		}
		select {
		case <-stopped:
		case <-time.After(3 * time.Second):
			t.Fatal("cancelled reminder left its HTTP request running")
		}
		var event struct {
			Status, Code  string
			NextAttemptAt int64
		}
		require.NoError(t, model.DB.Table("balance_reminder_events").Take(&event).Error)
		assert.Equal(t, "retry", event.Status)
		assert.Equal(t, model.BalanceReminderCancelled, event.Code)
		assert.Greater(t, event.NextAttemptAt, clock.Now().UnixMilli())
	})
	t.Run("system-task-owner-cancellation-and-history", func(t *testing.T) {
		calls := 0
		_, _ = balanceReminderServiceFixture(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusNoContent) })
		require.NoError(t, model.DB.AutoMigrate(&model.SystemTask{}, &model.SystemTaskLock{}))
		handler := BalanceReminderHandler{}
		require.True(t, handler.Enabled())
		task, err := model.CreateSystemTask(handler.Type(), nil, nil)
		require.NoError(t, err)
		task, acquired, err := model.ClaimSystemTask(task.ID, handler.Type(), "original-owner", common.GetTimestamp()+600)
		require.NoError(t, err)
		require.True(t, acquired)
		handler.Run(t.Context(), task, "not-the-owner")
		cancelled, cancel := context.WithCancel(t.Context())
		cancel()
		handler.Run(cancelled, task, "original-owner")
		assert.Zero(t, calls)
		current, err := model.GetSystemTaskByTaskID(task.TaskID)
		require.NoError(t, err)
		assert.Equal(t, model.SystemTaskStatusRunning, current.Status)
		handler.Run(t.Context(), task, "original-owner")
		assert.Equal(t, 1, calls)
		current, err = model.GetSystemTaskByTaskID(task.TaskID)
		require.NoError(t, err)
		assert.Equal(t, model.SystemTaskStatusSucceeded, current.Status)
		assert.Empty(t, current.Error)
		assert.Contains(t, current.Result, `"accepted":1`)
		assert.NotContains(t, current.Result, "reminder-signature-canary")
		assert.False(t, handler.Enabled())
		require.NoError(t, model.DB.Delete(&model.SystemTask{}, task.ID).Error)
		_, err = RunBalanceReminders(t.Context(), "after-history-cleanup")
		require.NoError(t, err)
		assert.Equal(t, 1, calls, "system-task history is not the deduplication record")
	})
	for _, state := range []string{"unconfigured", "rejected", "recovered", "disabled", "zero-budget"} {
		t.Run(state, func(t *testing.T) {
			calls := 0
			user, _ := balanceReminderServiceFixture(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusUnauthorized) })
			expectedStatus, expectedCode := "skipped", model.BalanceReminderNotConfigured
			switch state {
			case "unconfigured":
				_, err := model.PatchUserNotificationSettings(t.Context(), user.Id, nil, func(current *model.User) error {
					setting := current.GetSetting()
					setting.WebhookUrl = ""
					current.SetSetting(setting)
					return nil
				})
				require.NoError(t, err)
			case "rejected":
				expectedCode = model.BalanceReminderTransportRejected
			case "recovered":
				expectedCode = model.BalanceReminderRecovered
				_, err := model.TopUpWallet(model.WalletTopUp{UserId: user.Id, Source: money.Amount{Nanos: 200_000_000, Currency: "USD"}, Reference: "before-send-credit"})
				require.NoError(t, err)
			case "disabled":
				expectedCode = model.BalanceReminderDisabled
				_, err := model.PatchUserNotificationSettings(t.Context(), user.Id, &model.BalanceReminderSettings{Enabled: false}, nil)
				require.NoError(t, err)
			case "zero-budget":
				constant.NotifyLimitCount = 0
				expectedStatus, expectedCode = "retry", model.BalanceReminderRateLimited
			}
			_, err := RunBalanceReminders(t.Context(), "policy-worker")
			require.NoError(t, err)
			var event struct{ Status, Code string }
			require.NoError(t, model.DB.Table("balance_reminder_events").Take(&event).Error)
			assert.Equal(t, expectedStatus, event.Status)
			assert.Equal(t, expectedCode, event.Code)
			if state == "rejected" {
				assert.Equal(t, 1, calls)
			} else {
				assert.Zero(t, calls)
			}
		})
	}
}

func TestBalanceReminderFormatsUnboundedSnapshots(t *testing.T) {
	for _, test := range []struct{ nanos, formatted string }{
		{"9223372036854775808", "9223372036.854775808 USD"},
		{"-10000000000000000001", "-10000000000.000000001 USD"},
	} {
		for _, language := range []string{"en", "zhCN", "zhTW"} {
			t.Run(test.nanos+"/"+language, func(t *testing.T) {
				event := model.BalanceReminderDelivery{Scope: "subscription", After: model.MoneyTotal{Nanos: test.nanos, Currency: "USD"}, Threshold: money.Amount{Currency: "USD"}, CreatedAt: 1_794_916_800_000}
				notification, err := balanceReminderNotification(event, language)
				require.NoError(t, err)
				assert.Contains(t, notification.Content, test.formatted)
				assert.Contains(t, notification.Content, "0 USD")
				assert.NotContains(t, notification.Content, "{{")
				assert.Equal(t, i18n.Translate(language, i18n.MsgBalanceReminderSubscriptionTitle), notification.Title)
			})
		}
	}
}

func TestNotificationDeliveryUnconfiguredEmailIsNotRetry(t *testing.T) {
	previousServer, previousAccount, previousFrom := common.SMTPServer, common.SMTPAccount, common.SMTPFrom
	common.OptionMapRWMutex.Lock()
	previousOptions := common.OptionMap
	common.OptionMap = map[string]string{"EmailProvider": "smtp"}
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.SMTPServer, common.SMTPAccount, common.SMTPFrom = previousServer, previousAccount, previousFrom
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptions
		common.OptionMapRWMutex.Unlock()
	})
	common.SMTPServer, common.SMTPAccount, common.SMTPFrom = "", "", ""
	for _, provider := range []string{"smtp", "resend"} {
		t.Run(provider, func(t *testing.T) {
			common.OptionMapRWMutex.Lock()
			common.OptionMap["EmailProvider"] = provider
			common.OptionMapRWMutex.Unlock()
			result := DeliverUserNotification(t.Context(), "recipient@example.test", dto.UserSetting{}, dto.NewNotify("balance_reminder", "Balance", "Exact balance", nil))
			assert.Equal(t, NotificationDeliveryResult{NotificationUnconfigured, model.BalanceReminderNotConfigured}, result)
		})
	}
}

func TestNotificationDeliveryCarriesCancellationToTransport(t *testing.T) {
	previousClient, previousWorker := httpClient, system_setting.WorkerUrl
	previousAllowHTTP := system_setting.WorkerAllowHttpImageRequestEnabled
	previousLimit := constant.NotifyLimitCount
	fetch := system_setting.GetFetchSetting()
	previousFetch := *fetch
	t.Cleanup(func() {
		httpClient, system_setting.WorkerUrl = previousClient, previousWorker
		system_setting.WorkerAllowHttpImageRequestEnabled = previousAllowHTTP
		constant.NotifyLimitCount = previousLimit
		*fetch = previousFetch
	})
	fetch.EnableSSRFProtection = false
	constant.NotifyLimitCount = 0 // The durable caller already owns its attempt budget.
	for _, method := range []string{dto.NotifyTypeWebhook, dto.NotifyTypeBark, dto.NotifyTypeGotify} {
		for _, worker := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/worker=%t", method, worker), func(t *testing.T) {
				started, release := make(chan struct{}, 1), make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					started <- struct{}{}
					select {
					case <-r.Context().Done():
					case <-release:
					}
					w.WriteHeader(http.StatusNoContent)
				}))
				defer server.Close()
				httpClient, system_setting.WorkerUrl = server.Client(), ""
				system_setting.WorkerAllowHttpImageRequestEnabled = true
				if worker {
					system_setting.WorkerUrl = server.URL
				}
				settings := dto.UserSetting{NotifyType: method, WebhookUrl: server.URL, WebhookSecret: "notification-secret-canary", BarkUrl: server.URL, GotifyUrl: server.URL, GotifyToken: "notification-secret-canary"}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				finished := make(chan NotificationDeliveryResult, 1)
				go func() {
					finished <- DeliverUserNotification(ctx, "", settings, dto.NewNotify("balance_reminder", "Balance", "Below threshold", nil))
				}()
				select {
				case <-started:
				case <-time.After(time.Second):
					close(release)
					cancel()
					<-finished
					t.Fatal("notification did not reach the controlled transport")
				}
				cancel()
				select {
				case result := <-finished:
					assert.Equal(t, NotificationDeliveryResult{NotificationRetry, model.BalanceReminderCancelled}, result)
					close(release)
				case <-time.After(time.Second):
					close(release)
					<-finished
					t.Error("notification transport ignored caller cancellation")
				}
			})
		}
	}
}

// A low-balance reminder goes out once, on the charge that takes a wallet or
// subscription balance below the threshold. A balance already below it earns
// nothing more until a top-up lifts it back above the line. The site switch
// and the user's switch both turn reminders off; a user who never chose gets
// them.
func TestBalanceReminderFiresOncePerDrop(t *testing.T) {
	// These are exact USD nanos, not legacy quota. Preserve the original five
	// notification contracts through real funding, settlement and delivery.
	for _, source := range []string{"wallet", "subscription"} {
		for _, tc := range []struct {
			name        string
			siteEnabled bool
			setting     *model.BalanceReminderSettings
			start       int64
			changes     []int64
			want        []bool
		}{
			{name: "never chosen: one reminder per drop, again after replenishment", siteEnabled: true, start: 1_500_000_000, changes: []int64{-400_000_000, -200_000_000, -300_000_000, 1_500_000_000, -1_200_000_000}, want: []bool{false, true, false, true}},
			{name: "switched on with its own threshold", siteEnabled: true, setting: &model.BalanceReminderSettings{Enabled: true, Threshold: &money.Amount{Nanos: 5_000_000_000, Currency: "USD"}}, start: 6_000_000_000, changes: []int64{-500_000_000, -600_000_000, -2_000_000_000}, want: []bool{false, true, false}},
			{name: "exact threshold does not remind", siteEnabled: true, start: 1_200_000_000, changes: []int64{-200_000_000, -1}, want: []bool{false, true}},
			{name: "user switched off", siteEnabled: true, setting: &model.BalanceReminderSettings{Enabled: false}, start: 1_500_000_000, changes: []int64{-600_000_000}, want: []bool{false}},
			{name: "site switched off", setting: &model.BalanceReminderSettings{Enabled: true}, start: 1_500_000_000, changes: []int64{-600_000_000}, want: []bool{false}},
		} {
			t.Run(source+"/"+tc.name, func(t *testing.T) {
				var calls atomic.Int64
				user, fake := balanceReminderRecipientFixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusNoContent) })
				fake.Set(time.Unix(model.GetDBTimestamp(), 0))
				config, err := common.Marshal(operation_setting.BalanceReminderConfig{Enabled: tc.siteEnabled, Threshold: money.Amount{Nanos: 1_000_000_000, Currency: "USD"}})
				require.NoError(t, err)
				require.NoError(t, model.DB.Model(&model.Option{}).Where("key = ?", operation_setting.BalanceReminderConfigOption).Update("value", string(config)).Error)
				settings := user.GetSetting()
				settings.BillingPreference = source + "_only"
				user.SetSetting(settings)
				require.NoError(t, model.DB.Model(&user).Update("setting", user.Setting).Error)
				if tc.setting != nil {
					_, err = model.PatchUserNotificationSettings(t.Context(), user.Id, tc.setting, nil)
					require.NoError(t, err)
				}
				var periodID int64
				if source == "wallet" {
					_, err = model.TopUpWallet(model.WalletTopUp{UserId: user.Id, Source: money.Amount{Nanos: tc.start, Currency: "USD"}, Reference: "sequence-opening"})
					require.NoError(t, err)
				} else {
					plan := model.SubscriptionPlan{Title: "Reminder entitlement", Enabled: true, DurationUnit: "month", DurationValue: 1, Price: money.Amount{Currency: "USD"}, Allowance: money.Amount{Nanos: tc.start, Currency: "USD"}, MoneyVersion: 1}
					require.NoError(t, model.DB.Create(&plan).Error)
					require.NoError(t, model.DB.Transaction(func(tx *gorm.DB) error {
						sub, err := model.CreateUserSubscriptionFromPlanTx(tx, user.Id, &plan, "admin")
						if err == nil {
							periodID = sub.CurrentPeriodID
						}
						return err
					}))
				}
				got := make([]bool, 0, len(tc.want))
				for i, change := range tc.changes {
					before := calls.Load()
					if change > 0 {
						if source == "wallet" {
							_, err = model.TopUpWallet(model.WalletTopUp{UserId: user.Id, Source: money.Amount{Nanos: change, Currency: "USD"}, Reference: fmt.Sprintf("sequence-credit-%d", i)})
						} else {
							_, err = model.PostLedgerEntries(model.LedgerPosting{Account: model.LedgerAccount{Type: model.LedgerAccountSubscription, OwnerId: int(periodID), Currency: "USD"}, Type: model.LedgerEntryAdjustment, Amount: money.Amount{Nanos: change, Currency: "USD"}, IdempotencyKey: fmt.Sprintf("sequence-credit-%d", i)})
						}
						require.NoError(t, err)
					} else {
						charge := money.Amount{Nanos: -change, Currency: "USD"}
						request := model.GatewayRequest{ID: fmt.Sprintf("sequence-charge-%d", i), UserID: user.Id, SaleCurrency: "USD", SaleFXRate: "1", Status: "success"}
						require.NoError(t, model.ReserveGateway(&request, charge, charge))
						request.Receivable = charge
						require.NoError(t, model.PrepareGatewaySettlement(model.GatewaySettlement{Request: request, SaleSource: charge}))
						_, err = model.SettleGateway(request.ID)
						require.NoError(t, err)
						_, err = model.SettleGateway(request.ID)
						require.NoError(t, err)
					}
					_, err = RunBalanceReminders(t.Context(), "sequence-worker")
					require.NoError(t, err)
					if change < 0 {
						assert.LessOrEqual(t, calls.Load()-before, int64(1), "settlement replay cannot duplicate delivery")
						got = append(got, calls.Load() > before)
					} else {
						assert.Equal(t, before, calls.Load(), "replenishment does not send a charge notice")
					}
				}
				assert.Equal(t, tc.want, got)
			})
		}
	}
}

// Channel notices go to every enabled administrator whose channel switch is
// on, in that administrator's saved language. Until an administrator chooses,
// root receives them and other administrators do not; other users never do.
func TestChannelNoticesFollowEachAdministratorsSwitch(t *testing.T) {
	previousDB := model.DB
	previousCache, previousRedis := common.MemoryCacheEnabled, common.RedisEnabled
	previousNotifyLimit := constant.NotifyLimitCount
	previousClient, previousWorker := httpClient, system_setting.WorkerUrl
	fetch := system_setting.GetFetchSetting()
	previousFetch := *fetch
	t.Cleanup(func() {
		model.DB = previousDB
		common.MemoryCacheEnabled, common.RedisEnabled = previousCache, previousRedis
		constant.NotifyLimitCount = previousNotifyLimit
		httpClient, system_setting.WorkerUrl = previousClient, previousWorker
		*fetch = previousFetch
	})
	database := model.UseTestDatabases(t)
	sqlDB, err := database.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	require.NoError(t, database.AutoMigrate(&model.Provider{}, &model.Ability{}, &model.User{}))
	common.MemoryCacheEnabled, common.RedisEnabled = false, false
	constant.NotifyLimitCount = 100

	var mutex sync.Mutex
	received := map[string][]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload WebhookPayload
		if err := common.DecodeJson(r.Body, &payload); err == nil {
			mutex.Lock()
			recipient := strings.TrimPrefix(r.URL.Path, "/")
			received[recipient] = append(received[recipient], payload.Type+" "+payload.Title)
			mutex.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	httpClient, system_setting.WorkerUrl = server.Client(), ""
	fetch.EnableSSRFProtection = false

	on, off := true, false
	for _, user := range []struct {
		name     string
		role     int
		status   int
		receives *bool
		language string
	}{
		{"root-never-chose", common.RoleRootUser, common.UserStatusEnabled, nil, ""},
		{"root-turned-off", common.RoleRootUser, common.UserStatusEnabled, &off, ""},
		{"admin-never-chose", common.RoleAdminUser, common.UserStatusEnabled, nil, ""},
		{"admin-turned-on", common.RoleAdminUser, common.UserStatusEnabled, &on, "zhCN"},
		{"disabled-admin-turned-on", common.RoleAdminUser, common.UserStatusDisabled, &on, ""},
		{"user-with-switch-stored-on", common.RoleCommonUser, common.UserStatusEnabled, &on, ""},
	} {
		setting, err := common.Marshal(dto.UserSetting{
			NotifyType:           dto.NotifyTypeWebhook,
			WebhookUrl:           server.URL + "/" + user.name,
			ChannelNotifyEnabled: user.receives,
			Language:             user.language,
		})
		require.NoError(t, err)
		require.NoError(t, database.Create(&model.User{Username: user.name, Role: user.role, Status: user.status, Setting: string(setting), AffCode: user.name}).Error)
	}
	channel := &model.Provider{Name: "notice-review", Key: "fixture-key", Type: 1, Status: common.ChannelStatusEnabled, Group: "default", Models: "test-model"}
	require.NoError(t, channel.Insert())

	DisableChannel(types.ProviderError{ChannelId: channel.Id, ChannelName: channel.Name, AutoBan: true}, "upstream failed")
	EnableChannel(channel.Id, "", channel.Name)
	NotifyChannelWatchers(dto.NotifyTypeChannelTest, func(lang string) (string, string) {
		return i18n.Translate(lang, i18n.MsgChannelTestCompletedSubject), i18n.Translate(lang, i18n.MsgChannelTestCompletedContent)
	})

	disabledType := formatNotifyType(channel.Id, common.ChannelStatusAutoDisabled)
	enabledType := formatNotifyType(channel.Id, common.ChannelStatusEnabled)
	assert.Equal(t, map[string][]string{
		"root-never-chose": {
			fmt.Sprintf(`%s Channel "notice-review" (#%d) has been disabled`, disabledType, channel.Id),
			fmt.Sprintf(`%s Channel "notice-review" (#%d) has been enabled`, enabledType, channel.Id),
			"channel_test Channel test completed",
		},
		"admin-turned-on": {
			fmt.Sprintf("%s 通道「notice-review」（#%d）已被禁用", disabledType, channel.Id),
			fmt.Sprintf("%s 通道「notice-review」（#%d）已被启用", enabledType, channel.Id),
			"channel_test 通道测试完成",
		},
	}, received)
}
