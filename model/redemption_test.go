package model

import (
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestSearchRedemptionsFiltersAndPaginates(t *testing.T) {
	require.NoError(t, DB.AutoMigrate(&Redemption{}))
	require.NoError(t, DB.Session(&gorm.Session{AllowGlobalUpdate: true}).Unscoped().Delete(&Redemption{}).Error)
	t.Cleanup(func() {
		require.NoError(t, DB.Session(&gorm.Session{AllowGlobalUpdate: true}).Unscoped().Delete(&Redemption{}).Error)
	})

	now := common.GetTimestamp()
	redemptions := []Redemption{
		{Id: 1, Name: "alpha-active", Key: "00000000000000000000000000000001", Status: common.RedemptionCodeStatusEnabled, ExpiredTime: 0},
		{Id: 2, Name: "alpha-future", Key: "00000000000000000000000000000002", Status: common.RedemptionCodeStatusEnabled, ExpiredTime: now + 3600},
		{Id: 3, Name: "alpha-expired", Key: "00000000000000000000000000000003", Status: common.RedemptionCodeStatusEnabled, ExpiredTime: now - 10},
		{Id: 4, Name: "beta-disabled", Key: "00000000000000000000000000000004", Status: common.RedemptionCodeStatusDisabled, ExpiredTime: 0},
		{Id: 5, Name: "beta-used", Key: "00000000000000000000000000000005", Status: common.RedemptionCodeStatusUsed, ExpiredTime: 0},
	}
	for i := range redemptions {
		redemptions[i].Amount = usd(1)
	}
	require.NoError(t, DB.Create(&redemptions).Error)

	tests := []struct {
		name      string
		keyword   string
		status    string
		startIdx  int
		num       int
		wantTotal int64
		wantIds   []int
	}{
		{
			name:      "no filters returns all rows",
			num:       10,
			wantTotal: 5,
			wantIds:   []int{5, 4, 3, 2, 1},
		},
		{
			name:      "keyword filters by name prefix",
			keyword:   "alpha",
			num:       10,
			wantTotal: 3,
			wantIds:   []int{3, 2, 1},
		},
		{
			name:      "enabled status excludes expired rows",
			status:    "1",
			num:       10,
			wantTotal: 2,
			wantIds:   []int{2, 1},
		},
		{
			name:      "expired status returns enabled expired rows",
			status:    "expired",
			num:       10,
			wantTotal: 1,
			wantIds:   []int{3},
		},
		{
			name:      "disabled status",
			status:    "2",
			num:       10,
			wantTotal: 1,
			wantIds:   []int{4},
		},
		{
			name:      "used status",
			status:    "3",
			num:       10,
			wantTotal: 1,
			wantIds:   []int{5},
		},
		{
			name:      "pagination keeps unpaged total",
			startIdx:  1,
			num:       2,
			wantTotal: 5,
			wantIds:   []int{4, 3},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, total, err := SearchRedemptions(tt.keyword, tt.status, tt.startIdx, tt.num)
			require.NoError(t, err)
			assert.Equal(t, tt.wantTotal, total)
			gotIds := make([]int, 0, len(rows))
			for _, row := range rows {
				gotIds = append(gotIds, row.Id)
			}
			assert.Equal(t, tt.wantIds, gotIds)
		})
	}
}

func setupRedeemFixture(t *testing.T, nanos int64) (userId int, key string) {
	t.Helper()
	UseTestDatabases(t)
	require.NoError(t, DB.AutoMigrate(&User{}, &Redemption{}, &Log{}, &FXRate{}))
	require.NoError(t, migrateWalletLedger(DB))
	previousCurrency := settlementCurrency
	settlementCurrency = "USD"
	t.Cleanup(func() { settlementCurrency = previousCurrency })

	user := &User{Username: "redeem-user", Password: "password", Status: common.UserStatusEnabled, Quota: 17}
	require.NoError(t, DB.Create(user).Error)

	key = "10000000000000000000000000000001"
	redemption := &Redemption{
		Name:        "redeem-test",
		Key:         key,
		Status:      common.RedemptionCodeStatusEnabled,
		Amount:      usd(nanos),
		CreatedTime: common.GetTimestamp(),
	}
	require.NoError(t, DB.Create(redemption).Error)
	return user.Id, key
}

func TestRedeemCreditsWalletExactlyOnce(t *testing.T) {
	userId, key := setupRedeemFixture(t, 1_000_000)

	credited, err := Redeem(key, userId)
	require.NoError(t, err)
	assert.Equal(t, usd(1_000_000), credited)
	wallet, err := GetUserWallet(userId)
	require.NoError(t, err)
	require.NotNil(t, wallet)
	assert.Equal(t, usd(1_000_000), wallet.Balance)

	var user User
	require.NoError(t, DB.First(&user, "id = ?", userId).Error)
	assert.Equal(t, 17, user.Quota)

	var redemption Redemption
	require.NoError(t, DB.First(&redemption, "name = ?", "redeem-test").Error)
	assert.Equal(t, common.RedemptionCodeStatusUsed, redemption.Status)
	assert.Equal(t, userId, redemption.UsedUserId)

	// Redeeming the same code again must fail and must not credit quota.
	_, err = Redeem(key, userId)
	require.Error(t, err)
	require.NoError(t, DB.First(&user, "id = ?", userId).Error)
	assert.Equal(t, 17, user.Quota)
}

func TestRedeemRejectsWalletOverflow(t *testing.T) {
	userId, key := setupRedeemFixture(t, 11)
	_, err := TopUpWallet(WalletTopUp{UserId: userId, Source: usd(math.MaxInt64 - 10)})
	require.NoError(t, err)

	_, err = Redeem(key, userId)
	require.ErrorIs(t, err, ErrRedeemFailed)

	var user User
	require.NoError(t, DB.First(&user, "id = ?", userId).Error)
	assert.Equal(t, 17, user.Quota)
	wallet, err := GetUserWallet(userId)
	require.NoError(t, err)
	require.NotNil(t, wallet)
	assert.Equal(t, usd(math.MaxInt64-10), wallet.Balance)

	var redemption Redemption
	require.NoError(t, DB.First(&redemption, "key = ?", key).Error)
	assert.Equal(t, common.RedemptionCodeStatusEnabled, redemption.Status)
}

func TestRedemptionRejectsInvalidAmounts(t *testing.T) {
	setupRedeemFixture(t, 1_000_000)
	for _, amount := range []money.Amount{usd(0), usd(-1), {Nanos: 1, Currency: "usd"}, {Nanos: 1, Currency: ""}} {
		redemption := &Redemption{Name: "invalid-amount", Key: "10000000000000000000000000000002", Amount: amount}
		require.ErrorIs(t, redemption.Insert(), ErrInvalidRedemptionAmount)
		require.ErrorIs(t, redemption.Update(), ErrInvalidRedemptionAmount)
	}
}

func TestRedeemRejectsUnavailableCreditsWithoutConsumingCode(t *testing.T) {
	cases := []struct {
		name    string
		updates map[string]any
		rate    string
		missing bool
	}{
		{name: "expired", updates: map[string]any{"expired_time": common.GetTimestamp() - 10}},
		{name: "disabled", updates: map[string]any{"status": common.RedemptionCodeStatusDisabled}},
		{name: "previously redeemed", updates: map[string]any{"redeemed_time": 1, "used_user_id": 99}},
		{name: "missing user", missing: true},
		{name: "zero amount", updates: map[string]any{"amount_nanos": 0}},
		{name: "negative amount", updates: map[string]any{"amount_nanos": -1}},
		{name: "invalid currency", updates: map[string]any{"amount_currency": "usd"}},
		{name: "missing FX", updates: map[string]any{"amount_currency": "CNY"}},
		{name: "FX overflow", updates: map[string]any{"amount_currency": "CNY", "amount_nanos": int64(math.MaxInt64)}, rate: "2"},
		{name: "subnano credit", updates: map[string]any{"amount_currency": "CNY", "amount_nanos": 1}, rate: "0.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			userID, key := setupRedeemFixture(t, 1_000_000)
			if len(tc.updates) > 0 {
				require.NoError(t, DB.Model(&Redemption{}).Where("key = ?", key).Updates(tc.updates).Error)
			}
			if tc.rate != "" {
				require.NoError(t, DB.Create(&FXRate{FromCurrency: "CNY", ToCurrency: "USD", Rate: decimal.RequireFromString(tc.rate), BufferRate: decimal.Zero}).Error)
			}
			var before, after Redemption
			require.NoError(t, DB.Where("key = ?", key).First(&before).Error)
			redeemer := userID
			if tc.missing {
				redeemer += 1000
			}
			credited, err := Redeem(key, redeemer)
			require.ErrorIs(t, err, ErrRedeemFailed)
			assert.Equal(t, money.Amount{}, credited)
			require.NoError(t, DB.First(&after, before.Id).Error)
			assert.Equal(t, before, after, "even a failure after the status flip must restore the voucher")
			var entries, wallets int64
			require.NoError(t, DB.Model(&LedgerEntry{}).Count(&entries).Error)
			require.NoError(t, DB.Model(&Wallet{}).Count(&wallets).Error)
			assert.Zero(t, entries)
			assert.Zero(t, wallets)
			var user User
			require.NoError(t, DB.First(&user, userID).Error)
			assert.Equal(t, 17, user.Quota)
		})
	}
}

func TestRedeemPreservesLegacyCacheAndCannotReplayResetStatus(t *testing.T) {
	userID, key := setupRedeemFixture(t, 1)
	useUserCacheMiniRedis(t)
	// This test checks cache preservation, not expiry. Redeem can sleep for up to three seconds.
	common.SyncFrequency = 60
	var user User
	require.NoError(t, DB.First(&user, userID).Error)
	require.NoError(t, populateUserCache(user))
	require.NoError(t, common.RDB.HSet(t.Context(), getUserCacheKey(userID), "Quota", 17).Err())
	credited, err := Redeem(key, userID)
	require.NoError(t, err)
	assert.Equal(t, usd(1), credited)
	cached, err := cacheGetUserBase(userID)
	require.NoError(t, err)
	assert.Equal(t, userID, cached.Id)
	prior, err := common.RDB.HGet(t.Context(), getUserCacheKey(userID), "Quota").Int()
	require.NoError(t, err)
	assert.Equal(t, 17, prior)
	require.NoError(t, DB.Model(&Redemption{}).Where("key = ?", key).
		Updates(map[string]any{"status": common.RedemptionCodeStatusEnabled, "redeemed_time": 0, "used_user_id": 0}).Error)
	_, err = Redeem(key, userID)
	require.ErrorIs(t, err, ErrRedeemFailed)
	wallet, err := GetUserWallet(userID)
	require.NoError(t, err)
	require.NotNil(t, wallet)
	assert.Equal(t, usd(1), wallet.Balance)
	var entries []LedgerEntry
	require.NoError(t, DB.Find(&entries).Error)
	require.Len(t, entries, 1)
	assert.Equal(t, "Redemption code redeemed", entries[0].Remark)
	assert.NotContains(t, entries[0].Remark, key)
	assert.NotNil(t, entries[0].IdempotencyKey)
}

func TestRedeemAllowsMaximumWalletAmount(t *testing.T) {
	userID, key := setupRedeemFixture(t, math.MaxInt64)
	credited, err := Redeem(key, userID)
	require.NoError(t, err)
	assert.Equal(t, usd(math.MaxInt64), credited)
	wallet, err := GetUserWallet(userID)
	require.NoError(t, err)
	require.NotNil(t, wallet)
	assert.Equal(t, credited, wallet.Balance)
}

func TestRedeemUsesOriginalCurrencyAndRepaysDebt(t *testing.T) {
	userID, key := setupRedeemFixture(t, 2_000_000_000)
	require.NoError(t, DB.Model(&Redemption{}).Where("key = ?", key).Update("amount_currency", "CNY").Error)
	require.NoError(t, DB.Create(&FXRate{FromCurrency: "CNY", ToCurrency: "USD", Rate: decimal.RequireFromString("0.5"), BufferRate: decimal.RequireFromString("0.5")}).Error)
	require.NoError(t, DB.Transaction(func(tx *gorm.DB) error {
		_, err := postLedgerEntries(tx, []LedgerPosting{
			{Account: UserReceivableAccount(userID), Type: LedgerEntryCharge, Amount: usd(1_500_000_000), RequestId: "unpaid-request"},
			{Account: PlatformRevenueAccount("USD"), Type: LedgerEntryCharge, Amount: usd(1_500_000_000), RequestId: "unpaid-request"},
		})
		return err
	}))
	for i := range 2 {
		if i == 1 {
			key = "10000000000000000000000000000002"
			require.NoError(t, (&Redemption{Name: "second-credit", Key: key, Amount: money.Amount{Nanos: 2_000_000_000, Currency: "CNY"}}).Insert())
		}
		credited, err := Redeem(key, userID)
		require.NoError(t, err)
		assert.Equal(t, usd(1_000_000_000), credited, "repayment does not change the credited amount returned to the user")
		wallet, err := GetUserWallet(userID)
		require.NoError(t, err)
		require.NotNil(t, wallet)
		var debt Wallet
		require.NoError(t, DB.Where("account_type = ? AND owner_id = ?", LedgerAccountUserReceivable, userID).First(&debt).Error)
		if i == 0 {
			assert.Zero(t, wallet.Balance.Nanos)
			assert.Equal(t, int64(500_000_000), debt.Balance.Nanos)
			assert.True(t, wallet.RequestsPaused)
		} else {
			assert.Equal(t, int64(500_000_000), wallet.Balance.Nanos)
			assert.Zero(t, debt.Balance.Nanos)
			assert.False(t, wallet.RequestsPaused)
		}
	}
	var credits []LedgerEntry
	require.NoError(t, DB.Where("type = ?", LedgerEntryTopUp).Find(&credits).Error)
	require.Len(t, credits, 2)
	for _, entry := range credits {
		assert.Equal(t, money.Amount{Nanos: 2_000_000_000, Currency: "CNY"}, entry.SourceAmount)
		assert.Equal(t, usd(1_000_000_000), entry.Amount)
		require.True(t, entry.FxRate.Valid)
		assert.Equal(t, "0.5", entry.FxRate.Decimal.String())
	}
	var revenue Wallet
	require.NoError(t, DB.Where("account_type = ?", LedgerAccountPlatformRevenue).First(&revenue).Error)
	assert.Equal(t, usd(1_500_000_000), revenue.Balance)
	result, err := ReconcileWallets(100)
	require.NoError(t, err)
	assert.Empty(t, result.Mismatches)
}

// Exactly one concurrent redemption may commit a wallet credit.
func TestRedeemConcurrentSingleSuccess(t *testing.T) {
	userId, key := setupRedeemFixture(t, 300)

	const goroutines = 5
	successes := make([]bool, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func(idx int) {
			defer wg.Done()
			if _, err := Redeem(key, userId); err == nil {
				successes[idx] = true
			}
		}(i)
	}
	wg.Wait()

	successCount := 0
	for _, ok := range successes {
		if ok {
			successCount++
		}
	}
	assert.Equal(t, 1, successCount, "exactly one concurrent redeem should succeed")

	var user User
	require.NoError(t, DB.First(&user, "id = ?", userId).Error)
	assert.Equal(t, 17, user.Quota, "legacy quota must remain unchanged")
	wallet, err := GetUserWallet(userId)
	require.NoError(t, err)
	require.NotNil(t, wallet)
	assert.Equal(t, usd(300), wallet.Balance)
	var entries int64
	require.NoError(t, DB.Model(&LedgerEntry{}).Count(&entries).Error)
	assert.Equal(t, int64(1), entries)
}

type legacyRedemptionFixture struct {
	Id           int
	UserId       int
	Key          string `gorm:"type:char(32);uniqueIndex:idx_redemptions_key"`
	Status       int    `gorm:"default:1"`
	Name         string `gorm:"index:idx_redemptions_name"`
	Quota        int64  `gorm:"default:100"`
	CreatedTime  int64
	RedeemedTime int64
	UsedUserId   int
	DeletedAt    gorm.DeletedAt `gorm:"index:idx_redemptions_deleted_at"`
	ExpiredTime  int64
}

func (legacyRedemptionFixture) TableName() string { return "redemptions" }

func TestMigrateRedemptionAmountsFreshTwice(t *testing.T) {
	UseTestDatabases(t)
	require.NoError(t, migrateRedemptionAmounts(DB))
	voucher := Redemption{Name: "fresh", Key: "10000000000000000000000000000001", Amount: money.Amount{Nanos: 7, Currency: "CNY"}}
	require.NoError(t, voucher.Insert())
	require.NoError(t, migrateRedemptionAmounts(DB))
	stored, err := GetRedemptionById(voucher.Id)
	require.NoError(t, err)
	assert.Equal(t, voucher, *stored)
	assert.False(t, DB.Migrator().HasColumn(&Redemption{}, "quota"))
}

func TestMigrateRedemptionAmountsPreservesLegacyVouchersTwice(t *testing.T) {
	UseTestDatabases(t)
	previousCurrency := settlementCurrency
	settlementCurrency = "CNY"
	t.Cleanup(func() { settlementCurrency = previousCurrency })
	require.NoError(t, DB.AutoMigrate(&legacyRedemptionFixture{}))
	rows := []legacyRedemptionFixture{
		{Id: 1, UserId: 71, Name: "unused", Key: "10000000000000000000000000000001", Quota: 1, CreatedTime: 100},
		{Id: 2, UserId: 72, Name: "disabled", Key: "10000000000000000000000000000002", Quota: 500001, Status: common.RedemptionCodeStatusDisabled, ExpiredTime: 200},
		{Id: 3, UserId: 73, Name: "used", Key: "10000000000000000000000000000003", Quota: 1000000, Status: common.RedemptionCodeStatusUsed, RedeemedTime: 300, UsedUserId: 81},
		{Id: 4, UserId: 74, Name: "deleted", Key: "10000000000000000000000000000004", Quota: 17, DeletedAt: gorm.DeletedAt{Time: time.Unix(400, 0), Valid: true}},
		{Id: 5, UserId: 75, Name: "maximum", Key: "10000000000000000000000000000005", Quota: math.MaxInt64 / 2000},
	}
	require.NoError(t, DB.Create(&rows).Error)
	for range 2 {
		require.NoError(t, migrateRedemptionAmounts(DB))
		var stored []Redemption
		require.NoError(t, DB.Unscoped().Order("id").Find(&stored).Error)
		require.Len(t, stored, len(rows))
		for i, before := range rows {
			after := stored[i]
			assert.Equal(t, before.Id, after.Id)
			assert.Equal(t, before.UserId, after.UserId)
			assert.Equal(t, before.Key, after.Key)
			assert.Equal(t, before.Name, after.Name)
			assert.Equal(t, before.Status, after.Status)
			assert.Equal(t, before.CreatedTime, after.CreatedTime)
			assert.Equal(t, before.RedeemedTime, after.RedeemedTime)
			assert.Equal(t, before.UsedUserId, after.UsedUserId)
			assert.Equal(t, before.DeletedAt.Valid, after.DeletedAt.Valid)
			assert.True(t, before.DeletedAt.Time.Equal(after.DeletedAt.Time), "soft-delete timestamp must be preserved")
			assert.Equal(t, before.ExpiredTime, after.ExpiredTime)
			assert.Equal(t, usd(before.Quota*2000), after.Amount, "migration retains USD without an FX quote")
		}
		assert.False(t, DB.Migrator().HasColumn(&Redemption{}, "quota"))
	}
	duplicate := Redemption{Id: 999, Name: "duplicate", Key: rows[0].Key, Amount: usd(1)}
	require.Error(t, duplicate.Insert(), "the original unique code constraint must survive migration")
}

func TestMigrateRedemptionAmountsRejectsUnsafeLegacyValuesAtomically(t *testing.T) {
	for _, quota := range []int64{0, -1, math.MaxInt64/2000 + 1} {
		t.Run(fmt.Sprint(quota), func(t *testing.T) {
			UseTestDatabases(t)
			require.NoError(t, DB.AutoMigrate(&legacyRedemptionFixture{}))
			row := legacyRedemptionFixture{Id: 1, UserId: 17, Name: "preserved", Key: "10000000000000000000000000000001", Quota: 1}
			require.NoError(t, DB.Create(&row).Error)
			require.NoError(t, DB.Model(&row).Update("quota", quota).Error)
			for range 2 {
				require.Error(t, migrateRedemptionAmounts(DB))
				assert.True(t, DB.Migrator().HasColumn("redemptions", "quota"))
				assert.False(t, DB.Migrator().HasColumn("redemptions", "amount_nanos"))
				var stored legacyRedemptionFixture
				require.NoError(t, DB.First(&stored, row.Id).Error)
				assert.Equal(t, quota, stored.Quota)
				assert.Equal(t, row.Key, stored.Key)
				assert.Equal(t, row.UserId, stored.UserId)
			}
		})
	}
}
