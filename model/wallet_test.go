package model

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// useWalletLedgerDB gives the test its own PostgreSQL schema with the wallet
// and ledger tables migrated the same way startup migrates them.
func useWalletLedgerDB(t *testing.T) {
	t.Helper()
	UseTestDatabases(t)
	require.NoError(t, migrateWalletLedger(DB))
	require.NoError(t, DB.AutoMigrate(&User{}, &Option{}))
	require.NoError(t, MigrateBalanceReminders(DB))
}

func usd(nanos int64) money.Amount {
	return money.Amount{Nanos: nanos, Currency: "USD"}
}

func mustRate(t *testing.T, from, to money.Currency, rate string) *money.Rate {
	t.Helper()
	r, err := money.NewRate(from, to, decimal.RequireFromString(rate))
	require.NoError(t, err)
	return &r
}

func TestBalanceReminderStrictSettingsAndAtomicPatch(t *testing.T) {
	useWalletLedgerDB(t)
	user := User{Username: "reminder-settings", Setting: `{"language":"fr","billing_preference":"wallet_only"}`}
	require.NoError(t, DB.Create(&user).Error)
	for _, body := range []string{`{}`, `null`, `{"enabled":false}`, `{"threshold":null}`, `{"enabled":null,"threshold":null}`, `{"enabled":true,"threshold":{"nanos":1,"currency":"USD"}}`, `{"enabled":true,"threshold":{"nanos":"-1","currency":"USD"}}`, `{"enabled":true,"threshold":{"nanos":"1","currency":"EUR"}}`, `{"enabled":true,"enabled":false,"threshold":null}`, `{"enabled":true,"threshold":null,"unknown":1}`} {
		var settings BalanceReminderSettings
		require.Error(t, common.UnmarshalJsonStr(body, &settings), body)
	}
	var settings BalanceReminderSettings
	require.NoError(t, common.UnmarshalJsonStr(`{"enabled":false,"threshold":null}`, &settings))
	assert.False(t, settings.Enabled)
	assert.Nil(t, settings.Threshold)
	zero := usd(0)
	view, err := PatchUserNotificationSettings(t.Context(), user.Id, &BalanceReminderSettings{Enabled: true, Threshold: &zero}, func(current *User) error {
		assert.Contains(t, current.Setting, "fr")
		current.Setting = `{"language":"en","billing_preference":"wallet_only"}`
		return nil
	})
	require.NoError(t, err)
	require.NotNil(t, view.Threshold)
	assert.Equal(t, zero, view.EffectiveThreshold)
	_, err = PatchUserNotificationSettings(t.Context(), user.Id, nil, func(current *User) error { current.Setting = `{"language":"ja"}`; return nil })
	require.NoError(t, err)
	view, err = GetUserBalanceReminder(t.Context(), user.Id)
	require.NoError(t, err)
	require.NotNil(t, view.Threshold)
	assert.Zero(t, view.Threshold.Nanos)
	_, err = PatchUserNotificationSettings(t.Context(), user.Id, &BalanceReminderSettings{Enabled: false}, func(current *User) error { current.Setting = `{"language":"de"}`; return errors.New("patch rejected") })
	require.Error(t, err)
	require.NoError(t, DB.First(&user, user.Id).Error)
	assert.Equal(t, `{"language":"ja"}`, user.Setting)
	view, err = GetUserBalanceReminder(t.Context(), user.Id)
	require.NoError(t, err)
	assert.True(t, view.Enabled)
	require.NotNil(t, view.Threshold)
	for _, value := range []string{`{}`, `{"enabled":true,"threshold":null}`, `{"enabled":false,"threshold":{"nanos":"1","currency":"EUR"}}`, `{"enabled":true,"threshold":{"nanos":1,"currency":"USD"}}`} {
		require.Error(t, UpdateOption(operation_setting.BalanceReminderConfigOption, value))
	}
	require.Error(t, UpdateOption("QuotaRemindThreshold", "1"))
	require.Error(t, UpdateOptionsBulk(map[string]string{"QuotaRemindEnabled": "false", operation_setting.BalanceReminderConfigOption: `{"enabled":true,"threshold":{"nanos":"0","currency":"USD"}}`}))
	site, err := GetBalanceReminderSiteSettings(t.Context())
	require.NoError(t, err)
	assert.Equal(t, usd(2_000_000), site.Threshold)
	_, err = GetUserBalanceReminder(t.Context(), user.Id+10000)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	require.NoError(t, DB.Callback().Create().Before("gorm:create").Register("reminder-pref-failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "user_balance_reminders" {
			tx.AddError(errors.New("preference storage failure"))
		}
	}))
	_, err = PatchUserNotificationSettings(t.Context(), user.Id, &BalanceReminderSettings{Enabled: false}, func(current *User) error { current.Setting = `{"language":"de"}`; return nil })
	require.Error(t, err)
	require.NoError(t, DB.Callback().Create().Remove("reminder-pref-failure"))
	require.NoError(t, DB.First(&user, user.Id).Error)
	assert.Equal(t, `{"language":"ja"}`, user.Setting, "preference failure rolls back transport settings too")
}

func TestBalanceReminderMigrationPreservesSourcesAndConfirmedSettings(t *testing.T) {
	UseTestDatabases(t)
	require.NoError(t, DB.AutoMigrate(&User{}, &Option{}))
	require.NoError(t, migrateWalletLedger(DB))
	users := []User{{Username: "legacy-inherit", AffCode: "one", Setting: `{"quota_warning_threshold":0,"language":"fr"}`}, {Username: "legacy-small", AffCode: "two", Setting: `{"quota_warning_enabled":false,"quota_warning_threshold":0.0001}`}, {Username: "legacy-large", AffCode: "three", Setting: `{"quota_warning_threshold":4503599627370.497}`}}
	require.NoError(t, DB.Create(&users).Error)
	require.NoError(t, DB.Create(&Option{Key: "QuotaRemindThreshold", Value: "1000"}).Error)
	require.NoError(t, MigrateBalanceReminders(DB))
	want := []*int64{nil, new(int64(1)), new(int64(9007199254740994))}
	for i, user := range users {
		view, err := GetUserBalanceReminder(t.Context(), user.Id)
		require.NoError(t, err)
		if want[i] == nil {
			assert.Nil(t, view.Threshold)
		} else {
			require.NotNil(t, view.Threshold)
			assert.Equal(t, *want[i], view.Threshold.Nanos)
		}
		assert.Equal(t, i != 1, view.Enabled)
		var after User
		require.NoError(t, DB.First(&after, user.Id).Error)
		assert.Equal(t, user.Setting, after.Setting)
	}
	zero := usd(0)
	_, err := PatchUserNotificationSettings(t.Context(), users[2].Id, &BalanceReminderSettings{Enabled: false, Threshold: &zero}, nil)
	require.NoError(t, err)
	require.NoError(t, MigrateBalanceReminders(DB))
	view, err := GetUserBalanceReminder(t.Context(), users[2].Id)
	require.NoError(t, err)
	assert.False(t, view.Enabled)
	assert.Equal(t, zero, *view.Threshold)
	var source Option
	require.NoError(t, DB.Where("key = ?", "QuotaRemindThreshold").Take(&source).Error)
	assert.Equal(t, "1000", source.Value)
	for _, entity := range []any{&LedgerEntry{}, &balanceReminderEvent{}} {
		var count int64
		require.NoError(t, DB.Model(entity).Count(&count).Error)
		assert.Zero(t, count)
	}
}

func TestBalanceReminderMigrationRejectsInvalidAndDistinguishesFreshCurrency(t *testing.T) {
	for _, tc := range []struct {
		name, setting, option, currency string
		ok                              bool
	}{
		{"fresh-eur", "", "", "EUR", true}, {"legacy-eur", "", "1000", "EUR", false}, {"user-eur", `{"quota_warning_threshold":1}`, "", "EUR", false}, {"negative", `{"quota_warning_threshold":-1}`, "", "USD", false}, {"overflow", "", "9223372036854775807", "USD", false}, {"invalid-bool", `{"quota_warning_enabled":"false"}`, "", "USD", false}, {"duplicate", `{"quota_warning_threshold":1,"quota_warning_threshold":2}`, "", "USD", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			UseTestDatabases(t)
			require.NoError(t, DB.AutoMigrate(&User{}, &Option{}))
			old := settlementCurrency
			settlementCurrency = money.Currency(tc.currency)
			t.Cleanup(func() { settlementCurrency = old })
			if tc.setting != "" {
				require.NoError(t, DB.Create(&User{Username: "legacy", Setting: tc.setting}).Error)
			}
			if tc.option != "" {
				require.NoError(t, DB.Create(&Option{Key: "QuotaRemindThreshold", Value: tc.option}).Error)
			}
			err := MigrateBalanceReminders(DB)
			if tc.ok {
				require.NoError(t, err)
				site, err := GetBalanceReminderSiteSettings(t.Context())
				require.NoError(t, err)
				assert.Equal(t, money.Amount{Nanos: 2_000_000, Currency: "EUR"}, site.Threshold)
				require.NoError(t, MigrateBalanceReminders(DB))
			} else {
				require.Error(t, err)
				assert.False(t, DB.Migrator().HasTable(&balanceReminderMigration{}))
				var count int64
				require.NoError(t, DB.Model(&Option{}).Where("key = ?", operation_setting.BalanceReminderConfigOption).Count(&count).Error)
				assert.Zero(t, count)
			}
		})
	}
}

func reminderDeliveryFixture(t *testing.T, events int) User {
	t.Helper()
	user, _ := subscriptionFundingFixture(t, "wallet_only", 1_500_000_000, 0, true, false)
	threshold := usd(1_000_000_000)
	_, err := PatchUserNotificationSettings(t.Context(), user.Id, &BalanceReminderSettings{Enabled: true, Threshold: &threshold}, nil)
	require.NoError(t, err)
	for i := 0; i < events; i++ {
		if i > 0 {
			_, err = TopUpWallet(WalletTopUp{UserId: user.Id, Source: usd(600_000_000), Reference: fmt.Sprintf("rearm-%d", i)})
			require.NoError(t, err)
		}
		request := GatewayRequest{ID: fmt.Sprintf("delivery-%d", i), UserID: user.Id, SaleFXRate: "1"}
		require.NoError(t, ReserveGateway(&request, usd(600_000_000), usd(600_000_000)))
		require.NoError(t, PrepareGatewaySettlement(subscriptionFundingIntent(request, 600_000_000)))
		_, err = SettleGateway(request.ID)
		require.NoError(t, err)
	}
	return user
}

func TestBalanceReminderClaimLeaseBudgetAndRecheck(t *testing.T) {
	t.Run("fixed-window", func(t *testing.T) {
		fake := clock.UseFake(t, time.Date(2026, 11, 17, 10, 59, 0, 0, time.UTC))
		user := reminderDeliveryFixture(t, 3)
		deliveries, err := ClaimBalanceReminders(t.Context(), "worker-one", 1, time.Minute, 12*time.Hour, 2)
		require.NoError(t, err)
		require.Len(t, deliveries, 1)
		first := deliveries[0]
		assert.Equal(t, "wallet", first.Scope)
		assert.Equal(t, "1500000000", first.Before.Nanos)
		assert.Equal(t, "900000000", first.After.Nanos)
		ok, err := CheckBalanceReminderDelivery(t.Context(), first.ID, first.ClaimToken)
		require.NoError(t, err)
		assert.True(t, ok)
		require.NoError(t, FinishBalanceReminderDelivery(t.Context(), first.ID, first.ClaimToken, BalanceReminderOutcome{Status: "retry", Code: BalanceReminderDeliveryFailed, RetryAt: clock.Now().Add(time.Minute)}))
		fake.Advance(2 * time.Minute)
		deliveries, err = ClaimBalanceReminders(t.Context(), "worker-after-hour", 1, time.Minute, 12*time.Hour, 2)
		require.NoError(t, err)
		require.Len(t, deliveries, 1)
		second := deliveries[0]
		require.NoError(t, FinishBalanceReminderDelivery(t.Context(), second.ID, second.ClaimToken, BalanceReminderOutcome{Status: "sent", Code: BalanceReminderDelivered}))
		deliveries, err = ClaimBalanceReminders(t.Context(), "other-process", 100, time.Minute, 12*time.Hour, 2)
		require.NoError(t, err)
		assert.Empty(t, deliveries)
		var budget balanceReminderWindow
		require.NoError(t, DB.Where("user_id = ?", user.Id).Take(&budget).Error)
		assert.Equal(t, 2, budget.Attempts)
		assert.Equal(t, time.Date(2026, 11, 17, 22, 59, 0, 0, time.UTC).UnixMilli(), budget.EndsAt)
		fake.Advance(12 * time.Hour)
		deliveries, err = ClaimBalanceReminders(t.Context(), "after-fixed-window", 100, time.Minute, 12*time.Hour, 2)
		require.NoError(t, err)
		assert.Len(t, deliveries, 2)
	})
	t.Run("lease-fencing", func(t *testing.T) {
		fake := clock.UseFake(t, time.Unix(2000, 0))
		reminderDeliveryFixture(t, 1)
		first, err := ClaimBalanceReminders(t.Context(), "old", 1, time.Second, time.Hour, 10)
		require.NoError(t, err)
		require.Len(t, first, 1)
		fake.Advance(2 * time.Second)
		second, err := ClaimBalanceReminders(t.Context(), "new", 1, time.Minute, time.Hour, 10)
		require.NoError(t, err)
		require.Len(t, second, 1)
		assert.NotEqual(t, first[0].ClaimToken, second[0].ClaimToken)
		require.ErrorIs(t, FinishBalanceReminderDelivery(t.Context(), first[0].ID, first[0].ClaimToken, BalanceReminderOutcome{Status: "sent", Code: BalanceReminderDelivered}), ErrBalanceReminderLeaseLost)
		ok, err := CheckBalanceReminderDelivery(t.Context(), first[0].ID, first[0].ClaimToken)
		require.NoError(t, err)
		assert.False(t, ok)
		require.NoError(t, FinishBalanceReminderDelivery(t.Context(), second[0].ID, second[0].ClaimToken, BalanceReminderOutcome{Status: "sent", Code: BalanceReminderDelivered}))
		require.Error(t, FinishBalanceReminderDelivery(t.Context(), second[0].ID, second[0].ClaimToken, BalanceReminderOutcome{Status: "retry", Code: "https://secret.invalid/token"}))
	})
	for _, reason := range []string{"recovered", "disabled", "sql-error"} {
		t.Run(reason, func(t *testing.T) {
			user := reminderDeliveryFixture(t, 1)
			events, err := ClaimBalanceReminders(t.Context(), "recheck", 1, time.Minute, time.Hour, 2)
			require.NoError(t, err)
			require.Len(t, events, 1)
			switch reason {
			case "recovered":
				_, err = TopUpWallet(WalletTopUp{UserId: user.Id, Source: usd(100_000_000), Reference: "restore"})
				require.NoError(t, err)
			case "disabled":
				_, err = PatchUserNotificationSettings(t.Context(), user.Id, &BalanceReminderSettings{Enabled: false}, nil)
				require.NoError(t, err)
			case "sql-error":
				require.NoError(t, DB.Exec("ALTER TABLE user_balance_reminders RENAME TO reminder_hidden").Error)
				defer func() {
					require.NoError(t, DB.Exec("ALTER TABLE reminder_hidden RENAME TO user_balance_reminders").Error)
				}()
			}
			ok, err := CheckBalanceReminderDelivery(t.Context(), events[0].ID, events[0].ClaimToken)
			if reason == "sql-error" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				var event balanceReminderEvent
				require.NoError(t, DB.First(&event, events[0].ID).Error)
				assert.Equal(t, "skipped", event.Status)
				assert.Equal(t, reason, event.Code)
			}
			assert.False(t, ok)
		})
	}
	t.Run("stale-period", func(t *testing.T) {
		user, sub := subscriptionFundingFixture(t, "subscription_only", 0, 1_500_000_000, false, false)
		threshold := usd(1_000_000_000)
		_, err := PatchUserNotificationSettings(t.Context(), user.Id, &BalanceReminderSettings{Enabled: true, Threshold: &threshold}, nil)
		require.NoError(t, err)
		request := GatewayRequest{ID: "reminder-delivery-old-period", UserID: user.Id, SaleFXRate: "1"}
		require.NoError(t, ReserveGateway(&request, usd(600_000_000), usd(600_000_000)))
		require.NoError(t, PrepareGatewaySettlement(subscriptionFundingIntent(request, 600_000_000)))
		_, err = SettleGateway(request.ID)
		require.NoError(t, err)
		deliveries, err := ClaimBalanceReminders(t.Context(), "before-reset", 1, time.Minute, time.Hour, 2)
		require.NoError(t, err)
		require.Len(t, deliveries, 1)
		require.NoError(t, DB.Transaction(func(tx *gorm.DB) error {
			locked, err := lockSubscriptionLifecycleTx(tx, sub.Id)
			if err != nil {
				return err
			}
			return createSubscriptionPeriodTx(tx, locked, usd(0), "reset")
		}))
		ok, err := CheckBalanceReminderDelivery(t.Context(), deliveries[0].ID, deliveries[0].ClaimToken)
		require.NoError(t, err)
		assert.False(t, ok)
		var event balanceReminderEvent
		require.NoError(t, DB.First(&event, deliveries[0].ID).Error)
		assert.Equal(t, "skipped", event.Status)
		assert.Equal(t, BalanceReminderStalePeriod, event.Code)
	})
	t.Run("concurrent-budget", func(t *testing.T) {
		reminderDeliveryFixture(t, 3)
		results := make(chan []BalanceReminderDelivery, 2)
		errs := make(chan error, 2)
		for _, name := range []string{"one", "two"} {
			go func() {
				rows, err := ClaimBalanceReminders(t.Context(), name, 2, time.Minute, time.Hour, 1)
				results <- rows
				errs <- err
			}()
		}
		total := len(<-results) + len(<-results)
		require.NoError(t, <-errs)
		require.NoError(t, <-errs)
		assert.Equal(t, 1, total)
	})
}

func TestBalanceReminderDuePredicate(t *testing.T) {
	clock.UseFake(t, time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC))
	reminderDeliveryFixture(t, 1)
	now := clock.Now().UnixMilli()
	for _, tc := range []struct {
		name, status string
		next, lease  int64
		due          bool
	}{
		{"pending-now", "pending", now, 0, true}, {"pending-future", "pending", now + 1, 0, false},
		{"retry-now", "retry", now, 0, true}, {"retry-future", "retry", now + 1, 0, false},
		{"lease-expired", "delivering", now + 1, now, true}, {"lease-live", "delivering", now - 1, now + 1, false},
		{"sent", "sent", now - 1, 0, false}, {"skipped", "skipped", now - 1, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, DB.Model(&balanceReminderEvent{}).Where("id > 0").Updates(map[string]any{"status": tc.status, "next_attempt_at": tc.next, "lease_until": tc.lease}).Error)
			var before, after balanceReminderEvent
			require.NoError(t, DB.Take(&before).Error)
			due, err := HasDueBalanceReminders(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tc.due, due)
			require.NoError(t, DB.Take(&after).Error)
			assert.Equal(t, before, after)
		})
	}
	var count int64
	require.NoError(t, DB.Model(&balanceReminderWindow{}).Count(&count).Error)
	assert.Zero(t, count)
	require.NoError(t, DB.Exec("ALTER TABLE balance_reminder_events RENAME TO unavailable_reminders").Error)
	due, err := HasDueBalanceReminders(t.Context())
	assert.Error(t, err)
	assert.False(t, due)
	require.NoError(t, DB.Exec("ALTER TABLE unavailable_reminders RENAME TO balance_reminder_events").Error)
}

func TestBalanceReminderPurchaseAndDurableFacts(t *testing.T) {
	user, _ := subscriptionFundingFixture(t, "wallet_only", 1_500_000_000, 0, true, false)
	require.NoError(t, DB.AutoMigrate(&SubscriptionOrder{}))
	require.NoError(t, migrateLOGDB())
	threshold := usd(1_000_000_000)
	_, err := PatchUserNotificationSettings(t.Context(), user.Id, &BalanceReminderSettings{Enabled: true, Threshold: &threshold}, nil)
	require.NoError(t, err)
	allowed := true
	plan := SubscriptionPlan{Title: "Reminder purchase", Price: usd(600_000_000), Allowance: usd(0), Unlimited: true, MoneyVersion: 1, Enabled: true, DurationUnit: "month", DurationValue: 1, AllowBalancePay: &allowed}
	require.NoError(t, DB.Create(&plan).Error)
	revision, err := SubscriptionPurchaseRevision(&plan)
	require.NoError(t, err)
	purchaseID := "9241639c-31c7-4220-943b-d7f99f0cb990"
	_, err = PurchaseSubscriptionWithBalance(t.Context(), user.Id, plan.Id, purchaseID, revision)
	require.NoError(t, err)
	_, err = PurchaseSubscriptionWithBalance(t.Context(), user.Id, plan.Id, purchaseID, revision)
	require.NoError(t, err)
	var events []balanceReminderEvent
	require.NoError(t, DB.Find(&events).Error)
	require.Len(t, events, 1)
	assert.Equal(t, "1500000000", events[0].BeforeNanos.String())
	assert.Equal(t, "900000000", events[0].AfterNanos.String())
	require.NoError(t, DB.Model(&SubscriptionPlan{}).Where("id = ?", plan.Id).UpdateColumn("price_nanos", 0).Error)
	require.NoError(t, DB.First(&plan, plan.Id).Error)
	revision, err = SubscriptionPurchaseRevision(&plan)
	require.NoError(t, err)
	_, err = PurchaseSubscriptionWithBalance(t.Context(), user.Id, plan.Id, "440e6be4-4e56-4b2b-9496-4e511cb328e3", revision)
	require.NoError(t, err)
	var count int64
	require.NoError(t, DB.Model(&balanceReminderEvent{}).Count(&count).Error)
	assert.EqualValues(t, 1, count, "free grant creates no consumption event")
	require.Error(t, DB.Model(&balanceReminderEvent{}).Where("id = ?", events[0].ID).UpdateColumn("user_id", user.Id+1).Error)
	require.Error(t, DB.Delete(&balanceReminderEvent{}, events[0].ID).Error)
	require.Error(t, DB.Exec("TRUNCATE balance_reminder_events").Error)
	require.NoError(t, MigrateBalanceReminders(DB))
	var retained balanceReminderEvent
	require.NoError(t, DB.First(&retained, events[0].ID).Error)
	assert.Equal(t, events[0], retained)
	if LOG_DB.Dialector.Name() == "clickhouse" {
		assert.False(t, LOG_DB.Migrator().HasTable(&balanceReminderEvent{}))
		assert.False(t, LOG_DB.Migrator().HasTable(&UserBalanceReminder{}))
	}
}

func TestBalanceReminderZeroBudgetAndThresholdEquality(t *testing.T) {
	t.Run("zero-budget", func(t *testing.T) {
		reminderDeliveryFixture(t, 1)
		deliveries, err := ClaimBalanceReminders(t.Context(), "paused-budget", 10, time.Minute, time.Hour, 0)
		require.NoError(t, err)
		assert.Empty(t, deliveries)
		var event balanceReminderEvent
		require.NoError(t, DB.Take(&event).Error)
		assert.Equal(t, "retry", event.Status)
		assert.Equal(t, BalanceReminderRateLimited, event.Code)
		assert.Zero(t, event.Attempts)
	})
	t.Run("equality", func(t *testing.T) {
		user, _ := subscriptionFundingFixture(t, "wallet_only", 1_500_000_000, 0, true, false)
		threshold := usd(1_000_000_000)
		_, err := PatchUserNotificationSettings(t.Context(), user.Id, &BalanceReminderSettings{Enabled: true, Threshold: &threshold}, nil)
		require.NoError(t, err)
		for i, amount := range []int64{500_000_000, 1} {
			request := GatewayRequest{ID: fmt.Sprintf("equality-%d", i), UserID: user.Id, SaleFXRate: "1"}
			require.NoError(t, ReserveGateway(&request, usd(amount), usd(amount)))
			require.NoError(t, PrepareGatewaySettlement(subscriptionFundingIntent(request, amount)))
			_, err = SettleGateway(request.ID)
			require.NoError(t, err)
			var count int64
			require.NoError(t, DB.Model(&balanceReminderEvent{}).Count(&count).Error)
			assert.EqualValues(t, i, count)
		}
	})
}

func TestTopUpWalletCreditsSettlementCurrency(t *testing.T) {
	useWalletLedgerDB(t)

	tests := []struct {
		name       string
		userID     int
		source     money.Amount
		rate       *money.Rate
		wantAmount money.Amount
		wantRate   string
	}{
		{
			name:       "settlement currency",
			userID:     1,
			source:     usd(10_000_000_000),
			wantAmount: usd(10_000_000_000),
		},
		{
			// 1 CNY × 0.1234567891 = 0.1234567891 USD; a credit rounds down.
			name:       "other currency at the given rate",
			userID:     2,
			source:     money.Amount{Nanos: 1_000_000_000, Currency: "CNY"},
			rate:       mustRate(t, "CNY", "USD", "0.1234567891"),
			wantAmount: usd(123_456_789),
			wantRate:   "0.1234567891",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry, err := TopUpWallet(WalletTopUp{UserId: tt.userID, Source: tt.source, Rate: tt.rate, Reference: tt.name, OperatorId: 7})
			require.NoError(t, err)

			assert.Equal(t, LedgerEntryTopUp, entry.Type)
			assert.Equal(t, tt.wantAmount, entry.Amount)
			assert.Equal(t, tt.wantAmount, entry.BalanceAfter)
			assert.Equal(t, tt.source, entry.SourceAmount)
			if tt.wantRate == "" {
				assert.False(t, entry.FxRate.Valid)
			} else {
				require.True(t, entry.FxRate.Valid)
				assert.Equal(t, tt.wantRate, entry.FxRate.Decimal.String())
			}
			assert.Equal(t, 7, entry.OperatorId)

			wallet, err := GetUserWallet(tt.userID)
			require.NoError(t, err)
			require.NotNil(t, wallet)
			assert.Equal(t, tt.wantAmount, wallet.Balance)

			entries, total, err := ListLedgerEntries(wallet.Id, 0, 10)
			require.NoError(t, err)
			assert.Equal(t, int64(1), total)
			require.Len(t, entries, 1)
			assert.Equal(t, entry.Id, entries[0].Id)
			assert.Equal(t, tt.source, entries[0].SourceAmount)
			assert.Equal(t, entry.FxRate.Decimal.String(), entries[0].FxRate.Decimal.String())
		})
	}
}

func TestTopUpWalletRejectsInvalidConversion(t *testing.T) {
	useWalletLedgerDB(t)

	tests := []struct {
		name    string
		source  money.Amount
		rate    *money.Rate
		wantErr error
	}{
		{name: "other currency without a rate", source: money.Amount{Nanos: 1, Currency: "CNY"}, wantErr: ErrLedgerRateRequired},
		{name: "rate from another currency", source: money.Amount{Nanos: 1, Currency: "CNY"}, rate: mustRate(t, "EUR", "USD", "1.1"), wantErr: money.ErrCurrencyMismatch},
		{name: "rate to another currency", source: money.Amount{Nanos: 1, Currency: "CNY"}, rate: mustRate(t, "CNY", "EUR", "0.1"), wantErr: money.ErrCurrencyMismatch},
		{name: "rate for the settlement currency", source: usd(1), rate: mustRate(t, "CNY", "USD", "0.1"), wantErr: money.ErrCurrencyMismatch},
		{name: "zero amount", source: usd(0), wantErr: ErrLedgerInvalidEntry},
		{name: "negative amount", source: usd(-1), wantErr: ErrLedgerInvalidEntry},
		{name: "rounds down to zero", source: money.Amount{Nanos: 1, Currency: "CNY"}, rate: mustRate(t, "CNY", "USD", "0.5"), wantErr: ErrLedgerInvalidEntry},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := TopUpWallet(WalletTopUp{UserId: 1, Source: tt.source, Rate: tt.rate})
			require.ErrorIs(t, err, tt.wantErr)
		})
	}

	wallet, err := GetUserWallet(1)
	require.NoError(t, err)
	assert.Nil(t, wallet, "a rejected top-up must not leave a wallet behind")
}

func TestPostLedgerEntriesRejectsFirstUserWalletInAnotherCurrency(t *testing.T) {
	useWalletLedgerDB(t)
	t.Setenv("SETTLEMENT_CURRENCY", "USD")
	originalCurrency := SettlementCurrency()
	t.Cleanup(func() { settlementCurrency = originalCurrency })
	require.NoError(t, InitSettlementCurrency())

	entries, err := PostLedgerEntries(LedgerPosting{
		Account: LedgerAccount{Type: LedgerAccountUserWallet, OwnerId: 1, Currency: "CNY"},
		Type:    LedgerEntryAdjustment,
		Amount:  money.Amount{Nanos: 100, Currency: "CNY"},
	})
	assert.ErrorIs(t, err, money.ErrCurrencyMismatch)
	assert.Empty(t, entries)
	wallet, err := GetUserWallet(1)
	require.NoError(t, err)
	assert.Nil(t, wallet, "a rejected first posting must not create a wallet")

	entry, err := TopUpWallet(WalletTopUp{UserId: 1, Source: usd(10)})
	require.NoError(t, err, "the rejected posting must not block a settlement-currency top-up")
	assert.Equal(t, usd(10), entry.BalanceAfter)
	wallet, err = GetUserWallet(1)
	require.NoError(t, err)
	require.NotNil(t, wallet)
	assert.Equal(t, usd(10), wallet.Balance)
	_, total, err := ListLedgerEntries(wallet.Id, 0, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
}

func TestPostLedgerEntriesEnforcesTypeAndSign(t *testing.T) {
	useWalletLedgerDB(t)
	_, err := TopUpWallet(WalletTopUp{UserId: 1, Source: usd(100)})
	require.NoError(t, err)

	user := UserWalletAccount(1)
	revenue := PlatformRevenueAccount("USD")
	payable := ProviderPayableAccount(3, "CNY")
	tests := []struct {
		name    string
		posting LedgerPosting
		wantErr error
	}{
		{name: "user pre-consume", posting: LedgerPosting{Account: user, Type: LedgerEntryPreConsume, Amount: usd(-10)}},
		{name: "user pre-consume release", posting: LedgerPosting{Account: user, Type: LedgerEntryPreConsumeRelease, Amount: usd(10)}},
		{name: "user charge", posting: LedgerPosting{Account: user, Type: LedgerEntryCharge, Amount: usd(-5)}},
		{name: "user refund", posting: LedgerPosting{Account: user, Type: LedgerEntryRefund, Amount: usd(5)}},
		{name: "user adjustment up", posting: LedgerPosting{Account: user, Type: LedgerEntryAdjustment, Amount: usd(1)}},
		{name: "user adjustment down", posting: LedgerPosting{Account: user, Type: LedgerEntryAdjustment, Amount: usd(-1)}},
		{name: "user opening balance", posting: LedgerPosting{Account: user, Type: LedgerEntryOpeningBalance, Amount: usd(1)}},
		{name: "revenue charge", posting: LedgerPosting{Account: revenue, Type: LedgerEntryCharge, Amount: usd(5)}},
		{name: "revenue refund", posting: LedgerPosting{Account: revenue, Type: LedgerEntryRefund, Amount: usd(-5)}},
		{name: "provider cost", posting: LedgerPosting{Account: payable, Type: LedgerEntryCharge, Amount: money.Amount{Nanos: 7, Currency: "CNY"}}},
		{name: "same provider cost in another currency", posting: LedgerPosting{Account: ProviderPayableAccount(3, "USD"), Type: LedgerEntryCharge, Amount: usd(2)}},

		{name: "user owner zero", posting: LedgerPosting{Account: UserWalletAccount(0), Type: LedgerEntryAdjustment, Amount: usd(1)}, wantErr: ErrLedgerInvalidEntry},
		{name: "negative user owner", posting: LedgerPosting{Account: UserWalletAccount(-1), Type: LedgerEntryAdjustment, Amount: usd(1)}, wantErr: ErrLedgerInvalidEntry},
		{name: "provider owner zero", posting: LedgerPosting{Account: ProviderPayableAccount(0, "USD"), Type: LedgerEntryCharge, Amount: usd(1)}, wantErr: ErrLedgerInvalidEntry},
		{name: "negative provider owner", posting: LedgerPosting{Account: ProviderPayableAccount(-1, "USD"), Type: LedgerEntryCharge, Amount: usd(1)}, wantErr: ErrLedgerInvalidEntry},
		{name: "nonzero revenue owner", posting: LedgerPosting{Account: LedgerAccount{Type: LedgerAccountPlatformRevenue, OwnerId: 1, Currency: "USD"}, Type: LedgerEntryCharge, Amount: usd(1)}, wantErr: ErrLedgerInvalidEntry},
		{name: "positive pre-consume", posting: LedgerPosting{Account: user, Type: LedgerEntryPreConsume, Amount: usd(10)}, wantErr: ErrLedgerInvalidEntry},
		{name: "negative release", posting: LedgerPosting{Account: user, Type: LedgerEntryPreConsumeRelease, Amount: usd(-10)}, wantErr: ErrLedgerInvalidEntry},
		{name: "positive charge", posting: LedgerPosting{Account: user, Type: LedgerEntryCharge, Amount: usd(5)}, wantErr: ErrLedgerInvalidEntry},
		{name: "negative refund", posting: LedgerPosting{Account: user, Type: LedgerEntryRefund, Amount: usd(-5)}, wantErr: ErrLedgerInvalidEntry},
		{name: "negative top-up", posting: LedgerPosting{Account: user, Type: LedgerEntryTopUp, Amount: usd(-5)}, wantErr: ErrLedgerInvalidEntry},
		{name: "negative opening balance", posting: LedgerPosting{Account: user, Type: LedgerEntryOpeningBalance, Amount: usd(-1)}, wantErr: ErrLedgerInvalidEntry},
		{name: "zero adjustment", posting: LedgerPosting{Account: user, Type: LedgerEntryAdjustment, Amount: usd(0)}, wantErr: ErrLedgerInvalidEntry},
		{name: "negative revenue charge", posting: LedgerPosting{Account: revenue, Type: LedgerEntryCharge, Amount: usd(-5)}, wantErr: ErrLedgerInvalidEntry},
		{name: "pre-consume on revenue", posting: LedgerPosting{Account: revenue, Type: LedgerEntryPreConsume, Amount: usd(-5)}, wantErr: ErrLedgerInvalidEntry},
		{name: "top-up on provider payable", posting: LedgerPosting{Account: payable, Type: LedgerEntryTopUp, Amount: money.Amount{Nanos: 7, Currency: "CNY"}}, wantErr: ErrLedgerInvalidEntry},
		{name: "unknown entry type", posting: LedgerPosting{Account: user, Type: "bonus", Amount: usd(1)}, wantErr: ErrLedgerInvalidEntry},
		{name: "unknown account type", posting: LedgerPosting{Account: LedgerAccount{Type: "escrow", Currency: "USD"}, Type: LedgerEntryAdjustment, Amount: usd(1)}, wantErr: ErrLedgerInvalidEntry},
		{name: "amount in another currency", posting: LedgerPosting{Account: user, Type: LedgerEntryAdjustment, Amount: money.Amount{Nanos: 1, Currency: "CNY"}}, wantErr: money.ErrCurrencyMismatch},
		{name: "user wallet in another currency", posting: LedgerPosting{Account: LedgerAccount{Type: LedgerAccountUserWallet, OwnerId: 1, Currency: "CNY"}, Type: LedgerEntryAdjustment, Amount: money.Amount{Nanos: 1, Currency: "CNY"}}, wantErr: money.ErrCurrencyMismatch},
		{name: "balance overflow", posting: LedgerPosting{Account: user, Type: LedgerEntryAdjustment, Amount: usd(math.MaxInt64)}, wantErr: money.ErrOverflow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries, err := PostLedgerEntries(tt.posting)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Len(t, entries, 1)
			assert.Equal(t, tt.posting.Amount, entries[0].Amount)
			assert.Equal(t, tt.posting.Type, entries[0].Type)
			assert.Equal(t, tt.posting.Account.Type, entries[0].AccountType)
		})
	}

	// 100 + (-10 + 10 - 5 + 5 + 1 - 1 + 1) = 101; rejected postings change nothing.
	wallet, err := GetUserWallet(1)
	require.NoError(t, err)
	assert.Equal(t, usd(101), wallet.Balance)
}

func TestPostLedgerEntriesKeepsUserWalletNonNegative(t *testing.T) {
	useWalletLedgerDB(t)
	_, err := TopUpWallet(WalletTopUp{UserId: 1, Source: usd(10)})
	require.NoError(t, err)
	user := UserWalletAccount(1)

	_, err = PostLedgerEntries(LedgerPosting{Account: user, Type: LedgerEntryPreConsume, Amount: usd(-11)})
	require.ErrorIs(t, err, ErrInsufficientWalletBalance)

	entries, err := PostLedgerEntries(LedgerPosting{Account: user, Type: LedgerEntryCharge, Amount: usd(-10)})
	require.NoError(t, err)
	assert.Equal(t, usd(0), entries[0].BalanceAfter)

	_, err = PostLedgerEntries(LedgerPosting{Account: user, Type: LedgerEntryAdjustment, Amount: usd(-1)})
	require.ErrorIs(t, err, ErrInsufficientWalletBalance)

	// Platform and Provider accounts may go negative, for example after a refund.
	entries, err = PostLedgerEntries(LedgerPosting{Account: PlatformRevenueAccount("USD"), Type: LedgerEntryRefund, Amount: usd(-3)})
	require.NoError(t, err)
	assert.Equal(t, usd(-3), entries[0].BalanceAfter)
}

func TestPostLedgerEntriesIsAllOrNothing(t *testing.T) {
	useWalletLedgerDB(t)
	_, err := TopUpWallet(WalletTopUp{UserId: 1, Source: usd(10)})
	require.NoError(t, err)

	// A request charge to the user and the matching revenue, then a second
	// charge the wallet cannot cover: the whole batch must roll back.
	_, err = PostLedgerEntries(
		LedgerPosting{Account: UserWalletAccount(1), Type: LedgerEntryCharge, Amount: usd(-6), RequestId: "req-1"},
		LedgerPosting{Account: PlatformRevenueAccount("USD"), Type: LedgerEntryCharge, Amount: usd(6), RequestId: "req-1"},
		LedgerPosting{Account: UserWalletAccount(1), Type: LedgerEntryCharge, Amount: usd(-6), RequestId: "req-2"},
	)
	require.ErrorIs(t, err, ErrInsufficientWalletBalance)

	wallet, err := GetUserWallet(1)
	require.NoError(t, err)
	assert.Equal(t, usd(10), wallet.Balance)
	_, total, err := ListLedgerEntries(wallet.Id, 0, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	var revenueWallets int64
	require.NoError(t, DB.Model(&Wallet{}).Where("account_type = ?", LedgerAccountPlatformRevenue).Count(&revenueWallets).Error)
	assert.Zero(t, revenueWallets)

	entries, err := PostLedgerEntries(
		LedgerPosting{Account: UserWalletAccount(1), Type: LedgerEntryCharge, Amount: usd(-6), RequestId: "req-1", AttemptId: "att-1"},
		LedgerPosting{Account: PlatformRevenueAccount("USD"), Type: LedgerEntryCharge, Amount: usd(6), RequestId: "req-1"},
	)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, usd(4), entries[0].BalanceAfter)
	assert.Equal(t, usd(6), entries[1].BalanceAfter)
	assert.Equal(t, "req-1", entries[0].RequestId)
	assert.Equal(t, "att-1", entries[0].AttemptId)
}

func TestLedgerIdempotencyKeyIsUnique(t *testing.T) {
	useWalletLedgerDB(t)
	first, err := TopUpWallet(WalletTopUp{UserId: 1, Source: usd(10), Reference: "bank-42"})
	require.NoError(t, err)

	_, err = TopUpWallet(WalletTopUp{UserId: 1, Source: usd(10), Reference: "bank-42"})
	require.ErrorIs(t, err, ErrDuplicateLedgerEntry)
	_, err = TopUpWallet(WalletTopUp{UserId: 2, Source: usd(10), Reference: "bank-42"})
	require.ErrorIs(t, err, ErrDuplicateLedgerEntry)

	wallet, err := GetUserWallet(1)
	require.NoError(t, err)
	assert.Equal(t, first.BalanceAfter, wallet.Balance)

	posting := LedgerPosting{Account: UserWalletAccount(1), Type: LedgerEntryPreConsume, Amount: usd(-1), RequestId: "req-1", IdempotencyKey: "pre_consume:req-1"}
	_, err = PostLedgerEntries(posting)
	require.NoError(t, err)
	_, err = PostLedgerEntries(posting)
	require.ErrorIs(t, err, ErrDuplicateLedgerEntry)
}

func TestLedgerEntriesAreAppendOnly(t *testing.T) {
	useWalletLedgerDB(t)
	entry, err := TopUpWallet(WalletTopUp{UserId: 1, Source: usd(10)})
	require.NoError(t, err)

	require.Error(t, DB.Model(&LedgerEntry{}).Where("id = ?", entry.Id).Update("amount_nanos", 20).Error)
	require.Error(t, DB.Where("id = ?", entry.Id).Delete(&LedgerEntry{}).Error)
	require.Error(t, DB.Exec("TRUNCATE ledger_entries CASCADE").Error)
	require.Error(t, DB.Exec(`INSERT INTO ledger_entries (wallet_id, account_type, type, amount_nanos, amount_currency, balance_after_nanos, balance_after_currency, source_nanos, source_currency, created_at)
		VALUES (?, 'user_wallet', 'adjustment', 0, 'USD', 10, 'USD', 0, 'USD', 0)`, entry.WalletId).Error, "a zero amount is rejected by the database")

	var stored LedgerEntry
	require.NoError(t, DB.First(&stored, entry.Id).Error)
	assert.Equal(t, usd(10), stored.Amount)
}

func TestReverseLedgerEntry(t *testing.T) {
	useWalletLedgerDB(t)
	topUp, err := TopUpWallet(WalletTopUp{
		UserId: 1,
		Source: money.Amount{Nanos: 10_000_000_000, Currency: "CNY"},
		Rate:   mustRate(t, "CNY", "USD", "0.14"),
	})
	require.NoError(t, err)
	require.Equal(t, usd(1_400_000_000), topUp.Amount)
	charges, err := PostLedgerEntries(LedgerPosting{Account: UserWalletAccount(1), Type: LedgerEntryCharge, Amount: usd(-400_000_000), RequestId: "req-1"})
	require.NoError(t, err)

	reversal, err := ReverseLedgerEntry(charges[0].Id, 9, "charged twice")
	require.NoError(t, err)
	assert.Equal(t, LedgerEntryCharge, reversal.Type)
	assert.Equal(t, usd(400_000_000), reversal.Amount)
	assert.Equal(t, usd(1_400_000_000), reversal.BalanceAfter)
	require.NotNil(t, reversal.ReversesEntryId)
	assert.Equal(t, charges[0].Id, *reversal.ReversesEntryId)
	assert.Equal(t, "req-1", reversal.RequestId)
	assert.Equal(t, 9, reversal.OperatorId)
	assert.Equal(t, "charged twice", reversal.Remark)

	_, err = ReverseLedgerEntry(charges[0].Id, 9, "again")
	require.ErrorIs(t, err, ErrLedgerEntryAlreadyReversed)
	_, err = ReverseLedgerEntry(reversal.Id, 9, "undo the undo")
	require.ErrorIs(t, err, ErrLedgerReversalNotReversible)
	_, err = ReverseLedgerEntry(999_999, 9, "missing")
	require.ErrorIs(t, err, ErrLedgerEntryNotFound)

	topUpReversal, err := ReverseLedgerEntry(topUp.Id, 9, "payment bounced")
	require.NoError(t, err)
	assert.Equal(t, usd(-1_400_000_000), topUpReversal.Amount)
	assert.Equal(t, money.Amount{Nanos: -10_000_000_000, Currency: "CNY"}, topUpReversal.SourceAmount)
	require.True(t, topUpReversal.FxRate.Valid)
	assert.Equal(t, "0.14", topUpReversal.FxRate.Decimal.String())
	assert.Equal(t, usd(0), topUpReversal.BalanceAfter)

	wallet, err := GetUserWallet(1)
	require.NoError(t, err)
	assert.Equal(t, usd(0), wallet.Balance)
}

func TestReverseLedgerEntryKeepsUserWalletNonNegative(t *testing.T) {
	useWalletLedgerDB(t)
	topUp, err := TopUpWallet(WalletTopUp{UserId: 1, Source: usd(10)})
	require.NoError(t, err)
	_, err = PostLedgerEntries(LedgerPosting{Account: UserWalletAccount(1), Type: LedgerEntryCharge, Amount: usd(-4)})
	require.NoError(t, err)

	_, err = ReverseLedgerEntry(topUp.Id, 9, "payment bounced after use")
	require.ErrorIs(t, err, ErrInsufficientWalletBalance)
}

func TestReconcileWalletsReportsBalanceThatDiffersFromEntries(t *testing.T) {
	useWalletLedgerDB(t)
	for userID := 1; userID <= 3; userID++ {
		_, err := TopUpWallet(WalletTopUp{UserId: userID, Source: usd(100)})
		require.NoError(t, err)
	}
	_, err := PostLedgerEntries(
		LedgerPosting{Account: UserWalletAccount(2), Type: LedgerEntryCharge, Amount: usd(-30)},
		LedgerPosting{Account: PlatformRevenueAccount("USD"), Type: LedgerEntryCharge, Amount: usd(30)},
	)
	require.NoError(t, err)

	result, err := ReconcileWallets(2)
	require.NoError(t, err)
	assert.Equal(t, int64(7), result.CheckedWallets)
	assert.Empty(t, result.Mismatches)

	// Simulate a balance written outside the ledger.
	wallet, err := GetUserWallet(2)
	require.NoError(t, err)
	require.NoError(t, DB.Model(&Wallet{}).Where("id = ?", wallet.Id).Update("balance_nanos", 75).Error)

	result, err = ReconcileWallets(2)
	require.NoError(t, err)
	assert.Equal(t, int64(7), result.CheckedWallets)
	require.Len(t, result.Mismatches, 1)
	mismatch := result.Mismatches[0]
	assert.Equal(t, wallet.Id, mismatch.WalletId)
	assert.Equal(t, LedgerAccountUserWallet, mismatch.AccountType)
	assert.Equal(t, 2, mismatch.OwnerId)
	assert.Equal(t, usd(75), mismatch.Balance)
	assert.Equal(t, "70", mismatch.EntryTotalNanos.String())
}

func TestConcurrentChargesKeepBalanceEqualToEntries(t *testing.T) {
	useWalletLedgerDB(t)
	_, err := TopUpWallet(WalletTopUp{UserId: 1, Source: usd(100)})
	require.NoError(t, err)

	// 40 requests each charge 3 against a balance of 100: exactly 33 fit.
	const requests = 40
	errs := make(chan error, requests)
	var wg sync.WaitGroup
	for i := range requests {
		wg.Go(func() {
			_, err := PostLedgerEntries(
				LedgerPosting{Account: UserWalletAccount(1), Type: LedgerEntryCharge, Amount: usd(-3), RequestId: fmt.Sprintf("req-%d", i)},
				LedgerPosting{Account: PlatformRevenueAccount("USD"), Type: LedgerEntryCharge, Amount: usd(3), RequestId: fmt.Sprintf("req-%d", i)},
			)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)

	succeeded := 0
	for err := range errs {
		if err == nil {
			succeeded++
			continue
		}
		require.ErrorIs(t, err, ErrInsufficientWalletBalance)
	}
	assert.Equal(t, 33, succeeded)

	wallet, err := GetUserWallet(1)
	require.NoError(t, err)
	assert.Equal(t, usd(1), wallet.Balance)
	var revenue Wallet
	require.NoError(t, DB.Where("account_type = ?", LedgerAccountPlatformRevenue).First(&revenue).Error)
	assert.Equal(t, usd(99), revenue.Balance)

	result, err := ReconcileWallets(100)
	require.NoError(t, err)
	assert.Equal(t, int64(3), result.CheckedWallets)
	assert.Empty(t, result.Mismatches)
}

func TestMigrateWalletLedgerIsIdempotent(t *testing.T) {
	useWalletLedgerDB(t)
	entry, err := TopUpWallet(WalletTopUp{UserId: 1, Source: usd(10), Reference: "bank-1"})
	require.NoError(t, err)

	require.NoError(t, migrateWalletLedger(DB))

	wallet, err := GetUserWallet(1)
	require.NoError(t, err)
	assert.Equal(t, usd(10), wallet.Balance)
	_, err = TopUpWallet(WalletTopUp{UserId: 1, Source: usd(10), Reference: "bank-1"})
	require.ErrorIs(t, err, ErrDuplicateLedgerEntry)
	require.Error(t, DB.Where("id = ?", entry.Id).Delete(&LedgerEntry{}).Error)
	require.Error(t, DB.Create(&Wallet{AccountType: LedgerAccountUserWallet, OwnerId: 1, Balance: money.Amount{Currency: "CNY"}}).Error, "a user has one wallet")
}
