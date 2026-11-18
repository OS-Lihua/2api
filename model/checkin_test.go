package model

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupCheckinFixture(t *testing.T, source money.Amount) (User, *clock.Fake) {
	t.Helper()
	UseTestDatabases(t)
	require.NoError(t, DB.AutoMigrate(&User{}, &Checkin{}, &Option{}, &FXRate{}))
	require.NoError(t, migrateWalletLedger(DB))
	previous := config.GlobalConfig.Get("checkin_setting")
	previousCurrency := settlementCurrency
	settlementCurrency = "USD"
	config.GlobalConfig.Register("checkin_setting", &operation_setting.CheckinSetting{
		Enabled: true, RewardRange: operation_setting.CheckinRewardRange{Min: source, Max: source},
	})
	common.OptionMapRWMutex.Lock()
	previousOptions := common.OptionMap
	common.OptionMap = make(map[string]string)
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		config.GlobalConfig.Register("checkin_setting", previous)
		settlementCurrency = previousCurrency
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptions
		common.OptionMapRWMutex.Unlock()
	})
	user := User{Username: "checkin-user", Password: "password", Status: common.UserStatusEnabled, Quota: 17, AuthVersion: 1, Group: "default"}
	require.NoError(t, DB.Create(&user).Error)
	fake := clock.UseFake(t, time.Date(2026, 11, 14, 23, 59, 59, 0, time.FixedZone("checkin-zone", 8*3600)))
	return user, fake
}

func TestCheckinCreditsWalletWithoutLegacyQuota(t *testing.T) {
	user, _ := setupCheckinFixture(t, usd(2_000_000))
	useUserCacheMiniRedis(t)
	require.NoError(t, populateUserCache(user))
	require.NoError(t, common.RDB.HSet(t.Context(), getUserCacheKey(user.Id), "Quota", 14).Err())
	cached, err := common.RDB.HGetAll(t.Context(), getUserCacheKey(user.Id)).Result()
	require.NoError(t, err)
	checkin, err := UserCheckin(user.Id)
	require.NoError(t, err)
	assert.Equal(t, usd(2_000_000), checkin.Amount)
	require.NoError(t, DB.First(&user, user.Id).Error)
	assert.Equal(t, 17, user.Quota)
	afterCache, err := common.RDB.HGetAll(t.Context(), getUserCacheKey(user.Id)).Result()
	require.NoError(t, err)
	assert.Equal(t, cached, afterCache)
	wallet, err := GetUserWallet(user.Id)
	require.NoError(t, err)
	require.NotNil(t, wallet)
	assert.Equal(t, checkin.Amount, wallet.Balance)
	var entry LedgerEntry
	require.NoError(t, DB.First(&entry).Error)
	assert.Equal(t, LedgerEntryTopUp, entry.Type)
	assert.Equal(t, checkin.Amount, entry.Amount)
	assert.Equal(t, checkin.Amount, entry.SourceAmount)
	assert.False(t, entry.FxRate.Valid)
	require.NotNil(t, entry.IdempotencyKey)
	assert.Equal(t, fmt.Sprintf("checkin:%d:2026-11-14", user.Id), *entry.IdempotencyKey)
}

func TestCheckinRejectsRetiredOptionsBeforePersistence(t *testing.T) {
	setupCheckinFixture(t, usd(1))
	const key = "checkin_setting.reward_range"
	valid := `{"min":{"nanos":"0","currency":"USD"},"max":{"nanos":"9223372036854775807","currency":"USD"}}`
	require.NoError(t, UpdateOption(key, valid))
	original := *operation_setting.GetCheckinSetting()
	for _, tc := range []struct{ name, key, value string }{
		{"retired minimum", "checkin_setting.min_quota", "1000"},
		{"retired maximum", "checkin_setting.max_quota", "10000"},
		{"unknown option", "checkin_setting.unknown", "1000"},
		{"whole object", "checkin_setting", `{}`},
		{"invalid enabled", "checkin_setting.enabled", "1"},
		{"unknown field", key, `{"min":{"nanos":"0","currency":"USD"},"max":{"nanos":"1","currency":"USD"},"quota":1}`},
		{"unknown amount field", key, `{"min":{"nanos":"0","currency":"USD","quota":1},"max":{"nanos":"1","currency":"USD"}}`},
		{"missing maximum", key, `{"min":{"nanos":"0","currency":"USD"}}`},
		{"missing nanos", key, `{"min":{"currency":"USD"},"max":{"nanos":"1","currency":"USD"}}`},
		{"null nanos", key, `{"min":{"nanos":null,"currency":"USD"},"max":{"nanos":"1","currency":"USD"}}`},
		{"string null nanos", key, `{"min":{"nanos":"null","currency":"USD"},"max":{"nanos":"1","currency":"USD"}}`},
		{"numeric nanos", key, `{"min":{"nanos":0,"currency":"USD"},"max":{"nanos":"1","currency":"USD"}}`},
		{"negative", key, `{"min":{"nanos":"-1","currency":"USD"},"max":{"nanos":"1","currency":"USD"}}`},
		{"reversed", key, `{"min":{"nanos":"2","currency":"USD"},"max":{"nanos":"1","currency":"USD"}}`},
		{"currency mismatch", key, `{"min":{"nanos":"0","currency":"CNY"},"max":{"nanos":"1","currency":"USD"}}`},
		{"invalid currency", key, `{"min":{"nanos":"0","currency":"usd"},"max":{"nanos":"1","currency":"usd"}}`},
		{"overflow", key, `{"min":{"nanos":"0","currency":"USD"},"max":{"nanos":"9223372036854775808","currency":"USD"}}`},
		{"fractional", key, `{"min":{"nanos":"0.1","currency":"USD"},"max":{"nanos":"1","currency":"USD"}}`},
		{"trailing JSON", key, valid + `{}`},
		{"null range", key, `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Error(t, UpdateOption(tc.key, tc.value))
			assert.Error(t, UpdateOptionsBulk(map[string]string{tc.key: tc.value, "checkin_setting.enabled": "false"}))
			assert.Error(t, updateOptionMap(tc.key, tc.value), "database reload must also reject invalid settings")
			var options []Option
			require.NoError(t, DB.Order("key").Find(&options).Error)
			assert.Equal(t, []Option{{Key: key, Value: valid}}, options)
			assert.Equal(t, original, *operation_setting.GetCheckinSetting())
			common.OptionMapRWMutex.RLock()
			assert.Equal(t, map[string]string{key: valid}, common.OptionMap)
			common.OptionMapRWMutex.RUnlock()
		})
	}
	replacement := `{"min":{"nanos":"9007199254740993","currency":"CNY"},"max":{"nanos":"9007199254740993","currency":"CNY"}}`
	require.NoError(t, UpdateOption(key, replacement))
	assert.Equal(t, int64(0), original.RewardRange.Min.Nanos, "previous snapshots stay immutable")
	assert.Equal(t, money.Amount{Nanos: 9007199254740993, Currency: "CNY"}, operation_setting.GetCheckinSetting().RewardRange.Min)
}

func TestCheckinInclusiveRangeAndDateBoundary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		min, max int64
		entropy  []byte
		want     int64
	}{
		{"minimum", 1, 2, []byte{0}, 1},
		{"maximum", 1, 2, []byte{1}, 2},
		{"full range maximum", 0, math.MaxInt64, bytes.Repeat([]byte{255}, 8), math.MaxInt64},
		{"full range zero", 0, math.MaxInt64, make([]byte, 8), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			amount, err := checkinReward(operation_setting.CheckinRewardRange{Min: usd(tc.min), Max: usd(tc.max)}, bytes.NewReader(tc.entropy))
			require.NoError(t, err)
			assert.Equal(t, usd(tc.want), amount)
		})
	}
	user, fake := setupCheckinFixture(t, usd(1))
	started := fake.Now()
	require.NoError(t, DB.Callback().Query().After("gorm:query").Register("test:checkin_midnight", func(tx *gorm.DB) {
		if tx.Statement.Table == "users" {
			fake.Advance(2 * time.Second)
		}
	}))
	t.Cleanup(func() { require.NoError(t, DB.Callback().Query().Remove("test:checkin_midnight")) })
	row, err := UserCheckin(user.Id)
	require.NoError(t, err)
	assert.Equal(t, "2026-11-14", row.CheckinDate)
	assert.Equal(t, started.Unix(), row.CreatedAt)
	fake.Set(time.Unix(0, 0).UTC())
	row, err = UserCheckin(user.Id)
	require.NoError(t, err)
	assert.Equal(t, "1970-01-01", row.CheckinDate)
	assert.Zero(t, row.CreatedAt, "GORM must retain the captured epoch instead of substituting wall time")
}

func TestCheckinConcurrentAndLedgerReplay(t *testing.T) {
	user, _ := setupCheckinFixture(t, usd(3))
	start := make(chan struct{})
	results := make([]error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Go(func() { <-start; _, results[i] = UserCheckin(user.Id) })
	}
	close(start)
	wg.Wait()
	successes := 0
	for _, err := range results {
		if err == nil {
			successes++
		}
	}
	assert.Equal(t, 1, successes)
	var rows int64
	require.NoError(t, DB.Model(&Checkin{}).Count(&rows).Error)
	assert.Equal(t, int64(1), rows)
	wallet, err := GetUserWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, usd(3), wallet.Balance)
	// Even if the calendar row is deleted, the immutable ledger key blocks replay.
	require.NoError(t, DB.Where("user_id = ?", user.Id).Delete(&Checkin{}).Error)
	_, err = UserCheckin(user.Id)
	require.Error(t, err)
	require.NoError(t, DB.Model(&Checkin{}).Count(&rows).Error)
	assert.Zero(t, rows)
	require.NoError(t, DB.Model(&LedgerEntry{}).Count(&rows).Error)
	assert.Equal(t, int64(1), rows)
}

func TestCheckinFailuresRollBackAllWrites(t *testing.T) {
	for _, failure := range []string{"checkin_insert", "ledger_insert", "wallet_update", "checkin_amount_update", "overflow", "fx_missing", "fx_rounds_zero", "fx_overflow", "missing_user", "deleted_user", "disabled"} {
		t.Run(failure, func(t *testing.T) {
			source := usd(2)
			if strings.HasPrefix(failure, "fx_") {
				source.Currency = "CNY"
			}
			user, _ := setupCheckinFixture(t, source)
			expectedEntries := int64(0)
			if failure == "overflow" {
				_, err := TopUpWallet(WalletTopUp{UserId: user.Id, Source: usd(math.MaxInt64 - 1)})
				require.NoError(t, err)
				expectedEntries = 1
			}
			if failure == "fx_rounds_zero" || failure == "fx_overflow" {
				rate := "0.1"
				if failure == "fx_overflow" {
					rate = "1000000000"
					setting := operation_setting.GetCheckinSetting()
					setting.RewardRange.Min.Nanos, setting.RewardRange.Max.Nanos = math.MaxInt64, math.MaxInt64
					config.GlobalConfig.Register("checkin_setting", setting)
				}
				require.NoError(t, DB.Create(&FXRate{FromCurrency: "CNY", ToCurrency: "USD", Rate: decimal.RequireFromString(rate), BufferRate: decimal.Zero}).Error)
			}
			if failure == "missing_user" {
				user.Id += 10000
			}
			if failure == "deleted_user" {
				require.NoError(t, DB.Delete(&user).Error)
			}
			if failure == "disabled" {
				setting := operation_setting.GetCheckinSetting()
				setting.Enabled = false
				config.GlobalConfig.Register("checkin_setting", setting)
			}
			injected := false
			require.NoError(t, DB.Callback().Create().After("gorm:create").Register("test:checkin_create_failure", func(tx *gorm.DB) {
				if (failure == "checkin_insert" && tx.Statement.Table == "checkins") || (failure == "ledger_insert" && tx.Statement.Table == "ledger_entries") {
					injected = true
					tx.AddError(errors.New("injected private write failure"))
				}
			}))
			require.NoError(t, DB.Callback().Update().After("gorm:update").Register("test:checkin_update_failure", func(tx *gorm.DB) {
				if (failure == "wallet_update" && tx.Statement.Table == "wallets") || (failure == "checkin_amount_update" && tx.Statement.Table == "checkins") {
					injected = true
					tx.AddError(errors.New("injected private write failure"))
				}
			}))
			t.Cleanup(func() {
				require.NoError(t, DB.Callback().Create().Remove("test:checkin_create_failure"))
				require.NoError(t, DB.Callback().Update().Remove("test:checkin_update_failure"))
			})
			_, err := UserCheckin(user.Id)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "private write failure")
			if strings.HasSuffix(failure, "insert") || strings.HasSuffix(failure, "update") {
				assert.True(t, injected)
			}
			var count int64
			require.NoError(t, DB.Model(&Checkin{}).Count(&count).Error)
			assert.Zero(t, count)
			require.NoError(t, DB.Model(&LedgerEntry{}).Count(&count).Error)
			assert.Equal(t, expectedEntries, count)
			wallet, err := GetUserWallet(user.Id)
			require.NoError(t, err)
			if expectedEntries == 0 {
				assert.Nil(t, wallet)
			} else {
				assert.Equal(t, usd(math.MaxInt64-1), wallet.Balance)
			}
		})
	}
}

func TestCheckinZeroRewardAndDeletedUser(t *testing.T) {
	user, _ := setupCheckinFixture(t, money.Amount{Currency: "CNY"})
	row, err := UserCheckin(user.Id)
	require.NoError(t, err)
	assert.Equal(t, usd(0), row.Amount, "zero needs no FX or zero ledger entry")
	var count int64
	require.NoError(t, DB.Model(&LedgerEntry{}).Count(&count).Error)
	assert.Zero(t, count)
	_, err = UserCheckin(user.Id)
	require.Error(t, err)
	_, err = UserCheckin(user.Id + 10000)
	require.Error(t, err)
	require.NoError(t, DB.Delete(&user).Error)
	_, err = UserCheckin(user.Id)
	require.Error(t, err)
}

func TestCheckinFXRepaysDebtAtomically(t *testing.T) {
	user, fake := setupCheckinFixture(t, money.Amount{Nanos: 3, Currency: "CNY"})
	require.NoError(t, DB.Create(&FXRate{FromCurrency: "CNY", ToCurrency: "USD", Rate: decimal.RequireFromString("0.5"), BufferRate: decimal.RequireFromString("0.5")}).Error)
	require.NoError(t, DB.Transaction(func(tx *gorm.DB) error {
		_, err := postLedgerEntries(tx, []LedgerPosting{
			{Account: UserReceivableAccount(user.Id), Type: LedgerEntryCharge, Amount: usd(2), RequestId: "checkin-debt"},
			{Account: PlatformRevenueAccount("USD"), Type: LedgerEntryCharge, Amount: usd(2), RequestId: "checkin-debt"},
		})
		return err
	}))
	injected := false
	require.NoError(t, DB.Callback().Create().After("gorm:create").Register("test:checkin_repayment_failure", func(tx *gorm.DB) {
		if entry, ok := tx.Statement.Dest.(*LedgerEntry); ok && entry.Type == LedgerEntryRepayment {
			injected = true
			tx.AddError(errors.New("repayment write failure"))
		}
	}))
	_, err := UserCheckin(user.Id)
	require.Error(t, err)
	assert.True(t, injected)
	require.NoError(t, DB.Callback().Create().Remove("test:checkin_repayment_failure"))
	var count int64
	require.NoError(t, DB.Model(&Checkin{}).Count(&count).Error)
	assert.Zero(t, count)
	require.NoError(t, DB.Model(&LedgerEntry{}).Count(&count).Error)
	assert.Equal(t, int64(2), count)
	for i := range 2 {
		if i == 1 {
			fake.Advance(24 * time.Hour)
			require.NoError(t, DB.Model(&FXRate{}).Where("from_currency = ? AND to_currency = ?", "CNY", "USD").Update("rate", decimal.NewFromInt(1)).Error)
		}
		row, err := UserCheckin(user.Id)
		require.NoError(t, err)
		wantCredit, wantDebt, wantSpendable := int64(1), int64(1), int64(0)
		if i == 1 {
			wantCredit, wantDebt, wantSpendable = 3, 0, 2
		}
		assert.Equal(t, usd(wantCredit), row.Amount)
		wallet, err := GetUserWallet(user.Id)
		require.NoError(t, err)
		assert.Equal(t, usd(wantSpendable), wallet.Balance)
		assert.Equal(t, i == 0, wallet.RequestsPaused)
		var debt Wallet
		require.NoError(t, DB.Where("account_type = ? AND owner_id = ?", LedgerAccountUserReceivable, user.Id).First(&debt).Error)
		assert.Equal(t, usd(wantDebt), debt.Balance)
	}
	var credits []LedgerEntry
	require.NoError(t, DB.Where("type = ?", LedgerEntryTopUp).Order("id").Find(&credits).Error)
	require.Len(t, credits, 2)
	for i, rate := range []string{"0.5", "1"} {
		assert.Equal(t, money.Amount{Nanos: 3, Currency: "CNY"}, credits[i].SourceAmount)
		require.True(t, credits[i].FxRate.Valid)
		assert.Equal(t, rate, credits[i].FxRate.Decimal.String())
	}
	var revenue Wallet
	require.NoError(t, DB.Where("account_type = ?", LedgerAccountPlatformRevenue).First(&revenue).Error)
	assert.Equal(t, usd(2), revenue.Balance)
	reconciliation, err := ReconcileWallets(100)
	require.NoError(t, err)
	assert.Empty(t, reconciliation.Mismatches)
}

func TestCheckinStatsPreserveCurrencyAndUnboundedTotals(t *testing.T) {
	user, _ := setupCheckinFixture(t, usd(1))
	for i, amount := range []money.Amount{usd(math.MaxInt64), usd(math.MaxInt64), usd(-1), {Nanos: 7, Currency: "CNY"}, {Nanos: -2, Currency: "CNY"}, {Currency: "EUR"}} {
		require.NoError(t, DB.Create(&Checkin{UserId: user.Id, CheckinDate: fmt.Sprintf("2026-11-%02d", i+1), Amount: amount}).Error)
	}
	stats, err := GetUserCheckinStats(user.Id, "2026-11")
	require.NoError(t, err)
	assert.Equal(t, []CheckinAwardTotal{{Nanos: "5", Currency: "CNY"}, {Nanos: "0", Currency: "EUR"}, {Nanos: "18446744073709551613", Currency: "USD"}}, stats["total_awarded"])
	assert.Equal(t, int64(6), stats["total_checkins"])
	assert.Equal(t, 6, stats["checkin_count"])
	assert.Equal(t, false, stats["checked_in_today"])
	encoded, err := common.Marshal(stats)
	require.NoError(t, err)
	for _, field := range []string{`"quota_awarded"`, `"total_quota"`, `"id"`, `"user_id"`} {
		assert.NotContains(t, string(encoded), field)
	}
	empty, err := GetUserCheckinStats(user.Id+1, "2026-11")
	require.NoError(t, err)
	assert.Equal(t, []CheckinAwardTotal{}, empty["total_awarded"])
	assert.Equal(t, []CheckinRecord{}, empty["records"])
	for _, stage := range []string{"records", "today", "count", "aggregate"} {
		t.Run(stage, func(t *testing.T) {
			failure := errors.New("stats query failed")
			require.NoError(t, DB.Callback().Query().After("gorm:query").Register("test:checkin_stats_failure", func(tx *gorm.DB) {
				sql := tx.Statement.SQL.String()
				if tx.Statement.Table == "checkins" && ((stage == "records" && strings.Contains(sql, "ORDER BY")) || (stage == "today" && strings.Contains(sql, "checkin_date =")) || (stage == "count" && strings.Contains(sql, "count(*)") && !strings.Contains(sql, "checkin_date"))) {
					tx.AddError(failure)
				}
			}))
			require.NoError(t, DB.Callback().Row().Before("gorm:row").Register("test:checkin_sum_failure", func(tx *gorm.DB) {
				if stage == "aggregate" && strings.Contains(strings.Join(tx.Statement.Selects, " "), "SUM(amount_nanos)") {
					tx.AddError(failure)
				}
			}))
			_, err := GetUserCheckinStats(user.Id, "2026-11")
			assert.ErrorIs(t, err, failure)
			require.NoError(t, DB.Callback().Query().Remove("test:checkin_stats_failure"))
			require.NoError(t, DB.Callback().Row().Remove("test:checkin_sum_failure"))
		})
	}
}

type legacyCheckinFixture struct {
	Id           int    `gorm:"primaryKey;autoIncrement"`
	UserId       int    `gorm:"not null;uniqueIndex:idx_user_checkin_date"`
	CheckinDate  string `gorm:"type:varchar(10);not null;uniqueIndex:idx_user_checkin_date"`
	QuotaAwarded int64  `gorm:"not null"`
	CreatedAt    int64  `gorm:"bigint"`
}

func (legacyCheckinFixture) TableName() string { return "checkins" }

func TestMigrateCheckinAmountsFreshAndLegacyTwice(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			UseTestDatabases(t)
			require.NoError(t, DB.AutoMigrate(&Option{}))
			rows := []legacyCheckinFixture{}
			if legacy {
				require.NoError(t, DB.AutoMigrate(&legacyCheckinFixture{}))
				for i, quota := range []int64{-math.MaxInt64 / 2000, -1, 0, 1, 500001, math.MaxInt64 / 2000} {
					rows = append(rows, legacyCheckinFixture{Id: i + 41, UserId: 71, CheckinDate: fmt.Sprintf("2023-11-%02d", i+1), QuotaAwarded: quota, CreatedAt: int64(1700000000 + i)})
				}
				require.NoError(t, DB.Create(&rows).Error)
				require.NoError(t, DB.Create(&[]Option{{Key: "checkin_setting.min_quota", Value: "0"}, {Key: "checkin_setting.max_quota", Value: "500001"}, {Key: "checkin_setting.enabled", Value: "true"}}).Error)
			}
			want := make([]Checkin, len(rows))
			for i, before := range rows {
				want[i] = Checkin{Id: before.Id, UserId: before.UserId, CheckinDate: before.CheckinDate, CreatedAt: before.CreatedAt, Amount: usd(before.QuotaAwarded * 2000)}
			}
			for attempt := range 2 {
				require.NoError(t, migrateCheckinAmounts(DB))
				if !legacy && attempt == 0 {
					row := Checkin{UserId: 71, CheckinDate: "2026-11-14", Amount: money.Amount{Nanos: 7, Currency: "CNY"}}
					require.NoError(t, DB.Create(&row).Error)
					want = append(want, row)
				}
				var stored []Checkin
				require.NoError(t, DB.Order("id").Find(&stored).Error)
				assert.Equal(t, want, stored)
				assert.False(t, DB.Migrator().HasColumn("checkins", "quota_awarded"))
				var oldCount int64
				require.NoError(t, DB.Model(&Option{}).Where(`"key" IN ?`, []string{"checkin_setting.min_quota", "checkin_setting.max_quota"}).Count(&oldCount).Error)
				assert.Zero(t, oldCount)
				if legacy {
					var saved Option
					require.NoError(t, DB.First(&saved, `"key" = ?`, "checkin_setting.reward_range").Error)
					var value operation_setting.CheckinRewardRange
					require.NoError(t, common.UnmarshalJsonStr(saved.Value, &value))
					assert.Equal(t, operation_setting.CheckinRewardRange{Min: usd(0), Max: usd(1_000_002_000)}, value)
				}
				assert.False(t, DB.Migrator().HasTable(&Wallet{}), "migration cannot create or credit wallets")
				assert.False(t, DB.Migrator().HasTable(&LedgerEntry{}))
			}
			if legacy {
				require.Error(t, DB.Create(&Checkin{UserId: 71, CheckinDate: rows[0].CheckinDate, Amount: usd(1)}).Error)
			}
			for _, column := range []string{"amount_nanos", "amount_currency"} {
				var nullable string
				require.NoError(t, DB.Raw(`SELECT is_nullable FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='checkins' AND column_name=?`, column).Scan(&nullable).Error)
				assert.Equal(t, "NO", nullable)
			}
		})
	}
}

func TestMigrateCheckinAmountsRollsBackHistoryAndOptions(t *testing.T) {
	for _, failure := range []string{"positive_overflow", "negative_overflow", "amount_conflict", "partial_amount", "range_conflict", "invalid_minimum", "negative_range", "reversed_range", "range_overflow", "range_write", "old_option_delete", "drop_column"} {
		t.Run(failure, func(t *testing.T) {
			UseTestDatabases(t)
			require.NoError(t, DB.AutoMigrate(&legacyCheckinFixture{}, &Option{}))
			rows := []legacyCheckinFixture{{Id: 41, UserId: 71, CheckinDate: "2023-11-01", QuotaAwarded: 1, CreatedAt: 1700000000}, {Id: 42, UserId: 71, CheckinDate: "2023-11-02", QuotaAwarded: 2, CreatedAt: 1700000001}}
			if failure == "positive_overflow" {
				rows[1].QuotaAwarded = math.MaxInt64/2000 + 1
			}
			if failure == "negative_overflow" {
				rows[1].QuotaAwarded = math.MinInt64/2000 - 1
			}
			require.NoError(t, DB.Create(&rows).Error)
			options := []Option{{Key: "checkin_setting.min_quota", Value: "1000"}, {Key: "checkin_setting.max_quota", Value: "10000"}}
			switch failure {
			case "range_conflict":
				options = append(options, Option{Key: "checkin_setting.reward_range", Value: `{"min":{"nanos":"1","currency":"CNY"},"max":{"nanos":"2","currency":"CNY"}}`})
			case "invalid_minimum":
				options[0].Value = "1.5"
			case "negative_range":
				options[0].Value = "-1"
			case "reversed_range":
				options[0].Value = "10001"
			case "range_overflow":
				options[1].Value = fmt.Sprint(math.MaxInt64/2000 + 1)
			}
			require.NoError(t, DB.Create(&options).Error)
			partial := failure == "amount_conflict" || failure == "partial_amount"
			if partial {
				require.NoError(t, DB.Exec(`ALTER TABLE "checkins" ADD COLUMN amount_nanos bigint, ADD COLUMN amount_currency char(3)`).Error)
				var currency any = "CNY"
				if failure == "partial_amount" {
					currency = nil
				}
				require.NoError(t, DB.Table("checkins").Where("id = ?", 42).Updates(map[string]any{"amount_nanos": 4000, "amount_currency": currency}).Error)
			}
			injected := false
			require.NoError(t, DB.Callback().Create().After("gorm:create").Register("test:range_write_failure", func(tx *gorm.DB) {
				if failure == "range_write" && tx.Statement.Table == "options" {
					injected = true
					tx.AddError(errors.New("range write failed"))
				}
			}))
			require.NoError(t, DB.Callback().Delete().After("gorm:delete").Register("test:range_delete_failure", func(tx *gorm.DB) {
				if failure == "old_option_delete" && tx.Statement.Table == "options" {
					injected = true
					tx.AddError(errors.New("range delete failed"))
				}
			}))
			if failure == "drop_column" {
				require.NoError(t, DB.Exec(`CREATE VIEW "checkin_legacy_dependency" AS SELECT "quota_awarded" FROM "checkins"`).Error)
			}
			for range 2 {
				require.Error(t, migrateCheckinAmounts(DB))
				assert.True(t, DB.Migrator().HasColumn("checkins", "quota_awarded"))
				assert.Equal(t, partial, DB.Migrator().HasColumn("checkins", "amount_nanos"))
				var after []legacyCheckinFixture
				require.NoError(t, DB.Order("id").Find(&after).Error)
				assert.Equal(t, rows, after)
				var afterOptions []Option
				require.NoError(t, DB.Order("key").Find(&afterOptions).Error)
				assert.ElementsMatch(t, options, afterOptions)
				if partial {
					var nanos *int64
					require.NoError(t, DB.Table("checkins").Select("amount_nanos").Where("id = ?", 41).Scan(&nanos).Error)
					assert.Nil(t, nanos, "earlier history conversions roll back")
				}
			}
			if failure == "range_write" || failure == "old_option_delete" {
				assert.True(t, injected)
			}
			require.NoError(t, DB.Callback().Create().Remove("test:range_write_failure"))
			require.NoError(t, DB.Callback().Delete().Remove("test:range_delete_failure"))
		})
	}
}
