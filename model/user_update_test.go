package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupUserUpdateTestState(t *testing.T) {
	t.Helper()
	truncateTables(t)
	require.NoError(t, DB.Exec("DELETE FROM users").Error)

	oldRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() {
		common.RedisEnabled = oldRedisEnabled
	})
}

func setupRegistrationRewardTest(t *testing.T, reward string) {
	t.Helper()
	UseTestDatabases(t)
	require.NoError(t, DB.AutoMigrate(&User{}, &Option{}, &FXRate{}, &ReferralOpening{}))
	require.NoError(t, migrateWalletLedger(DB))
	previous := config.GlobalConfig.Get("reward_setting")
	previousPayment := *operation_setting.GetPaymentSetting()
	common.OptionMapRWMutex.Lock()
	previousOptions := common.OptionMap
	common.OptionMap = map[string]string{}
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		if previous != nil {
			config.GlobalConfig.Register("reward_setting", previous)
		}
		*operation_setting.GetPaymentSetting() = previousPayment
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptions
		common.OptionMapRWMutex.Unlock()
	})
	operation_setting.GetPaymentSetting().ComplianceConfirmed = true
	operation_setting.GetPaymentSetting().ComplianceTermsVersion = operation_setting.CurrentComplianceTermsVersion
	require.NoError(t, UpdateOption("reward_setting.config", reward))
}

func TestUserRegistrationRewardsAreAtomicMoney(t *testing.T) {
	setupRegistrationRewardTest(t, `{"signup":{"nanos":"9007199254740993","currency":"USD"},"inviter":{"nanos":"300000001","currency":"USD"},"invitee":{"nanos":"200000001","currency":"USD"}}`)
	inviter := User{Username: "reward-inviter", Password: "unused", AffCode: "reward-inviter", Status: common.UserStatusEnabled}
	require.NoError(t, DB.Create(&inviter).Error)
	created := User{Username: "reward-new-user", Status: common.UserStatusEnabled}
	require.NoError(t, DB.Transaction(func(tx *gorm.DB) error { return created.InsertWithTx(tx, inviter.Id) }))
	wallet, err := GetUserWallet(created.Id)
	require.NoError(t, err)
	require.NotNil(t, wallet, "registration must credit the money wallet in its creation transaction")
	assert.Equal(t, money.Amount{Nanos: 9007199454740994, Currency: "USD"}, wallet.Balance)
	var persisted User
	require.NoError(t, DB.First(&persisted, created.Id).Error)
	assert.Zero(t, persisted.Quota, "registration must stop writing legacy int quota")
	assert.Equal(t, inviter.Id, persisted.InviterId)
	created.FinishInsert(inviter.Id)
	created.FinalizeOAuthUserCreation(inviter.Id)
	var entries int64
	require.NoError(t, DB.Model(&LedgerEntry{}).Count(&entries).Error)
	assert.EqualValues(t, 3, entries, "post-commit helpers must not award a second time")
}

func TestRegistrationRewardFailureRollsBackUserAndAllAwards(t *testing.T) {
	setupRegistrationRewardTest(t, `{"signup":{"nanos":"7","currency":"USD"},"inviter":{"nanos":"3","currency":"CNY"},"invitee":{"nanos":"5","currency":"USD"}}`)
	inviter := User{Username: "fx-inviter", Status: common.UserStatusEnabled}
	require.NoError(t, DB.Create(&inviter).Error)
	created := User{Username: "fx-invitee", Status: common.UserStatusEnabled}
	require.Error(t, created.Insert(inviter.Id), "missing accounting FX must roll back the whole registration")
	var count int64
	require.NoError(t, DB.Model(&User{}).Where("username = ?", created.Username).Count(&count).Error)
	assert.Zero(t, count)
	require.NoError(t, DB.Model(&LedgerEntry{}).Count(&count).Error)
	assert.Zero(t, count)
	require.NoError(t, DB.First(&inviter, inviter.Id).Error)
	assert.Zero(t, inviter.AffCount)

	require.NoError(t, DB.Create(&FXRate{FromCurrency: "CNY", ToCurrency: "USD", Rate: decimal.RequireFromString("0.5")}).Error)
	created.Id = 0
	rollback := errors.New("binding transaction failed")
	require.ErrorIs(t, DB.Transaction(func(tx *gorm.DB) error {
		if err := created.InsertWithTx(tx, inviter.Id); err != nil {
			return err
		}
		return rollback
	}), rollback)
	require.NoError(t, DB.Model(&LedgerEntry{}).Count(&count).Error)
	assert.Zero(t, count)
	created.Id = 0
	require.NoError(t, created.Insert(inviter.Id))
	rewards, err := GetReferralRewards(inviter.Id)
	require.NoError(t, err)
	assert.Equal(t, money.Amount{Nanos: 1, Currency: "USD"}, rewards.Available)
	assert.Equal(t, CheckinAwardTotal{Nanos: "1", Currency: "USD"}, rewards.Earned)
	assert.Equal(t, 1, rewards.Invited)
	var entry LedgerEntry
	require.NoError(t, DB.Where("account_type = ?", LedgerAccountUserReferral).First(&entry).Error)
	assert.Equal(t, money.Amount{Nanos: 3, Currency: "CNY"}, entry.SourceAmount)
	require.True(t, entry.FxRate.Valid)
	assert.Equal(t, "0.5", entry.FxRate.Decimal.String())
	wallet, err := GetUserWallet(created.Id)
	require.NoError(t, err)
	assert.Equal(t, money.Amount{Nanos: 12, Currency: "USD"}, wallet.Balance)

	_, err = PostLedgerEntries(LedgerPosting{Account: userReferralAccount(inviter.Id), Type: LedgerEntryTopUp, Amount: money.Amount{Nanos: math.MaxInt64 - 1, Currency: "USD"}})
	require.NoError(t, err)
	overflow := User{Username: "overflow-invitee", Status: common.UserStatusEnabled}
	require.ErrorIs(t, overflow.Insert(inviter.Id), money.ErrOverflow)
	require.NoError(t, DB.Model(&User{}).Where("username = ?", overflow.Username).Count(&count).Error)
	assert.Zero(t, count)
	require.NoError(t, DB.First(&inviter, inviter.Id).Error)
	assert.Equal(t, 1, inviter.AffCount)
}

func TestReferralRewardTransferRepaysDebtAndReplaysExactlyOnce(t *testing.T) {
	setupRegistrationRewardTest(t, `{"signup":{"nanos":"0","currency":"USD"},"inviter":{"nanos":"0","currency":"USD"},"invitee":{"nanos":"0","currency":"USD"}}`)
	user := User{Username: "referral-transfer", Status: common.UserStatusEnabled, Quota: 777}
	require.NoError(t, DB.Create(&user).Error)
	_, err := PostLedgerEntries(LedgerPosting{Account: userReferralAccount(user.Id), Type: LedgerEntryTopUp, Amount: money.Amount{Nanos: 9007199254740993, Currency: "USD"}})
	require.NoError(t, err)
	require.NoError(t, DB.Transaction(func(tx *gorm.DB) error {
		_, err := postLedgerEntries(tx, []LedgerPosting{{Account: UserReceivableAccount(user.Id), Type: LedgerEntryCharge, Amount: money.Amount{Nanos: 2, Currency: "USD"}, RequestId: "referral-debt"}})
		return err
	}))
	amount := money.Amount{Nanos: 9007199254740993, Currency: "USD"}
	entry, err := TransferReferralRewards(user.Id, amount, "lost-response")
	require.NoError(t, err)
	replayed, err := TransferReferralRewards(user.Id, amount, "lost-response")
	require.NoError(t, err)
	assert.Equal(t, entry.Id, replayed.Id)
	_, err = TransferReferralRewards(user.Id, money.Amount{Nanos: 1, Currency: "USD"}, "lost-response")
	require.Error(t, err, "an identity must not be reused for a changed amount")
	for _, invalid := range []money.Amount{{Nanos: 1, Currency: "USD"}, {Nanos: 1, Currency: "CNY"}, {Currency: "USD"}, {Nanos: -1, Currency: "USD"}} {
		_, err := TransferReferralRewards(user.Id, invalid, "another-transfer")
		require.Error(t, err)
	}
	_, err = ReverseLedgerEntry(entry.Id, 0, "cannot reverse one side")
	require.Error(t, err)
	rewards, err := GetReferralRewards(user.Id)
	require.NoError(t, err)
	assert.Equal(t, money.Amount{Currency: "USD"}, rewards.Available)
	assert.Equal(t, CheckinAwardTotal{Nanos: "9007199254740993", Currency: "USD"}, rewards.Earned)
	wallet, err := GetUserWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, money.Amount{Nanos: 9007199254740991, Currency: "USD"}, wallet.Balance)
	debt, err := GetUserOutstanding(user.Id)
	require.NoError(t, err)
	assert.Zero(t, debt.Nanos)
	require.NoError(t, DB.First(&user, user.Id).Error)
	assert.Equal(t, 777, user.Quota)
	var transfers int64
	require.NoError(t, DB.Model(&LedgerEntry{}).Where("type = ?", LedgerEntryTransfer).Count(&transfers).Error)
	assert.EqualValues(t, 2, transfers)
	reconciliation, err := ReconcileWallets(100)
	require.NoError(t, err)
	assert.Empty(t, reconciliation.Mismatches)
}

func TestReferralRewardConcurrentTransfersCannotDuplicateOrOverspend(t *testing.T) {
	for _, sameID := range []bool{true, false} {
		t.Run(strconv.FormatBool(sameID), func(t *testing.T) {
			setupRegistrationRewardTest(t, `{"signup":{"nanos":"0","currency":"USD"},"inviter":{"nanos":"0","currency":"USD"},"invitee":{"nanos":"0","currency":"USD"}}`)
			user := User{Username: "concurrent-referral", Status: common.UserStatusEnabled}
			require.NoError(t, DB.Create(&user).Error)
			_, err := PostLedgerEntries(LedgerPosting{Account: userReferralAccount(user.Id), Type: LedgerEntryTopUp, Amount: money.Amount{Nanos: 10, Currency: "USD"}})
			require.NoError(t, err)
			db, err := DB.DB()
			require.NoError(t, err)
			db.SetMaxOpenConns(2)
			t.Cleanup(func() { db.SetMaxOpenConns(1) })
			type result struct {
				entry *LedgerEntry
				err   error
			}
			results := make(chan result, 2)
			start := make(chan struct{})
			for i := range 2 {
				go func() {
					<-start
					id := "same"
					if !sameID {
						id = strconv.Itoa(i)
					}
					entry, err := TransferReferralRewards(user.Id, money.Amount{Nanos: 7, Currency: "USD"}, id)
					results <- result{entry: entry, err: err}
				}()
			}
			close(start)
			first, second := <-results, <-results
			if sameID {
				require.NoError(t, first.err)
				require.NoError(t, second.err)
				assert.Equal(t, first.entry.Id, second.entry.Id)
			} else {
				assert.NotEqual(t, first.err == nil, second.err == nil, "only one competing transfer can spend the balance")
			}
			rewards, err := GetReferralRewards(user.Id)
			require.NoError(t, err)
			assert.Equal(t, money.Amount{Nanos: 3, Currency: "USD"}, rewards.Available)
			wallet, err := GetUserWallet(user.Id)
			require.NoError(t, err)
			assert.Equal(t, money.Amount{Nanos: 7, Currency: "USD"}, wallet.Balance)
		})
	}
}

func TestReferralRewardsFollowWalletBeforeUserLockOrder(t *testing.T) {
	for _, operation := range []string{"transfer", "registration"} {
		t.Run(operation, func(t *testing.T) {
			setupRegistrationRewardTest(t, `{"signup":{"nanos":"0","currency":"USD"},"inviter":{"nanos":"1","currency":"USD"},"invitee":{"nanos":"0","currency":"USD"}}`)
			user := User{Username: "lock-order-inviter", Status: common.UserStatusEnabled}
			require.NoError(t, DB.Create(&user).Error)
			_, err := PostLedgerEntries(LedgerPosting{Account: userReferralAccount(user.Id), Type: LedgerEntryTopUp, Amount: money.Amount{Nanos: 10, Currency: "USD"}})
			require.NoError(t, err)
			checkout := DB.Begin()
			require.NoError(t, checkout.Error)
			defer checkout.Rollback()
			var wallet Wallet
			require.NoError(t, lockForUpdate(checkout).Where("account_type = ? AND owner_id = ?", LedgerAccountUserReferral, user.Id).Take(&wallet).Error)
			waiting := make(chan struct{})
			var once sync.Once
			require.NoError(t, DB.Callback().Query().Before("gorm:query").Register("test_reward_wallet_wait", func(tx *gorm.DB) {
				if tx.Statement.Table == "wallets" && tx.Statement.Clauses["FOR"].Name != "" {
					once.Do(func() { close(waiting) })
				}
			}))
			defer DB.Callback().Query().Remove("test_reward_wallet_wait")
			finished := make(chan error, 1)
			go func() {
				if operation == "transfer" {
					_, err := TransferReferralRewards(user.Id, money.Amount{Nanos: 1, Currency: "USD"}, "concurrent-checkout")
					finished <- err
					return
				}
				newUser := User{Username: "lock-order-invitee"}
				finished <- newUser.Insert(user.Id)
			}()
			select {
			case <-waiting:
			case <-time.After(5 * time.Second):
				t.Fatal("reward transaction did not reach the shared wallet lock")
			}
			require.NoError(t, checkout.Exec("SET LOCAL lock_timeout = '300ms'").Error)
			err = checkout.Model(&User{}).Where("id = ?", user.Id).UpdateColumn("request_count", gorm.Expr("request_count + 1")).Error
			assert.NoError(t, err, "an operation waiting for a wallet must not already hold the user row needed by checkout fulfillment")
			if err != nil {
				require.NoError(t, checkout.Rollback().Error)
			} else {
				require.NoError(t, checkout.Commit().Error)
			}
			select {
			case err := <-finished:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("reward transaction did not finish after the wallet lock was released")
			}
		})
	}
}

func TestMigrateUserRewardsPreservesUnboundedHistory(t *testing.T) {
	UseTestDatabases(t)
	require.NoError(t, DB.AutoMigrate(&User{}, &Option{}))
	require.NoError(t, migrateWalletLedger(DB))
	require.NoError(t, DB.Exec(`ALTER TABLE "users" ADD COLUMN "aff_quota" bigint DEFAULT 0, ADD COLUMN "aff_history" bigint DEFAULT 0`).Error)
	user := User{Username: "unbounded-referral", Status: common.UserStatusEnabled}
	require.NoError(t, DB.Create(&user).Error)
	require.NoError(t, DB.Model(&user).Updates(map[string]any{"aff_history": int64(math.MaxInt64)}).Error)
	for range 2 {
		require.NoError(t, migrateUserRewards(DB))
		rewards, err := GetReferralRewards(user.Id)
		require.NoError(t, err)
		assert.Zero(t, rewards.Available.Nanos)
		assert.Equal(t, CheckinAwardTotal{Nanos: "18446744073709551614000", Currency: "USD"}, rewards.Earned)
	}
}

func TestMigrateUserRewardsPreservesObligationsTwice(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(strconv.FormatBool(legacy), func(t *testing.T) {
			UseTestDatabases(t)
			require.NoError(t, DB.AutoMigrate(&User{}, &Option{}))
			require.NoError(t, migrateWalletLedger(DB))
			user := User{Username: "migration-referral", Status: common.UserStatusEnabled}
			require.NoError(t, DB.Create(&user).Error)
			if legacy {
				require.NoError(t, DB.Exec(`ALTER TABLE "users" ADD COLUMN "aff_quota" bigint DEFAULT 0, ADD COLUMN "aff_history" bigint DEFAULT 0`).Error)
				require.NoError(t, DB.Model(&user).Updates(map[string]any{"aff_quota": 500001, "aff_history": 1500002}).Error)
				require.NoError(t, DB.Create(&[]Option{{Key: "QuotaForNewUser", Value: "1"}, {Key: "QuotaForInviter", Value: "500001"}, {Key: "QuotaForInvitee", Value: "0"}}).Error)
			}
			for range 2 {
				require.NoError(t, migrateUserRewards(DB))
				rewards, err := GetReferralRewards(user.Id)
				require.NoError(t, err)
				if legacy {
					assert.Equal(t, money.Amount{Nanos: 1000002000, Currency: "USD"}, rewards.Available)
					assert.Equal(t, CheckinAwardTotal{Nanos: "3000004000", Currency: "USD"}, rewards.Earned)
					var option Option
					require.NoError(t, DB.First(&option, `"key" = ?`, "reward_setting.config").Error)
					var reward operation_setting.RewardConfig
					require.NoError(t, common.UnmarshalJsonStr(option.Value, &reward))
					assert.Equal(t, money.Amount{Nanos: 2000, Currency: "USD"}, reward.Signup)
					assert.Equal(t, money.Amount{Nanos: 1000002000, Currency: "USD"}, reward.Inviter)
				} else {
					assert.Zero(t, rewards.Available.Nanos)
				}
				assert.False(t, DB.Migrator().HasColumn("users", "aff_quota"))
				assert.False(t, DB.Migrator().HasColumn("users", "aff_history"))
				wallet, err := GetUserWallet(user.Id)
				require.NoError(t, err)
				assert.Nil(t, wallet, "pending rewards must not be credited as spendable wallet funds during migration")
			}
			if legacy {
				require.Error(t, DB.Model(&ReferralOpening{}).Where("user_id = ?", user.Id).Update("earned_nanos", 0).Error)
				_, err := TransferReferralRewards(user.Id, money.Amount{Nanos: 1000002000, Currency: "USD"}, "migrated-transfer")
				require.NoError(t, err)
				require.NoError(t, migrateUserRewards(DB))
				rewards, err := GetReferralRewards(user.Id)
				require.NoError(t, err)
				assert.Zero(t, rewards.Available.Nanos, "a repeated startup must not reinstate transferred opening funds")
				assert.Equal(t, "3000004000", rewards.Earned.Nanos)
			}
		})
	}
}

func createUserBindTestUser(t *testing.T) User {
	t.Helper()
	user := User{
		Username:    "bind-test-user",
		Password:    "unused-password-hash",
		Role:        common.RoleCommonUser,
		Status:      common.UserStatusEnabled,
		Group:       "default",
		AuthVersion: 1,
		AffCode:     "bind-test-aff-code",
	}
	require.NoError(t, DB.Create(&user).Error)
	return user
}

func TestUserUpdateDoesNotOverwriteConcurrentAccountingOrTokenChanges(t *testing.T) {
	UseTestDatabases(t)
	require.NoError(t, DB.AutoMigrate(&User{}))
	require.NoError(t, migrateWalletLedger(DB))

	user := User{
		Id:           1,
		Username:     "quota-race-user",
		Password:     "password",
		DisplayName:  "before",
		Status:       common.UserStatusEnabled,
		Quota:        1000,
		UsedQuota:    20,
		RequestCount: 3,
		AffCount:     2,
	}
	user.SetAccessToken(testAccessToken("old-token"))
	require.NoError(t, DB.Create(&user).Error)

	staleUser, err := GetUserById(user.Id, true)
	require.NoError(t, err)

	require.NoError(t, DB.Model(&User{}).Where("id = ?", user.Id).Updates(map[string]any{
		"quota":         gorm.Expr("quota - ?", 400),
		"used_quota":    gorm.Expr("used_quota + ?", 400),
		"request_count": gorm.Expr("request_count + ?", 1),
		"aff_count":     gorm.Expr("aff_count + ?", 1),
		"access_token":  testAccessToken("rotated-token"),
	}).Error)

	_, err = PostLedgerEntries(LedgerPosting{Account: UserWalletAccount(user.Id), Type: LedgerEntryTopUp, Amount: money.Amount{Nanos: 500, Currency: "USD"}})
	require.NoError(t, err)
	staleUser.DisplayName = "after"
	require.NoError(t, staleUser.Update(false))

	var got User
	require.NoError(t, DB.First(&got, user.Id).Error)
	assert.Equal(t, "after", got.DisplayName)
	assert.Equal(t, 600, got.Quota)
	assert.Equal(t, 420, got.UsedQuota)
	assert.Equal(t, 4, got.RequestCount)
	assert.Equal(t, 3, got.AffCount)
	wallet, err := GetUserWallet(user.Id)
	require.NoError(t, err)
	require.NotNil(t, wallet)
	assert.Equal(t, money.Amount{Nanos: 500, Currency: "USD"}, wallet.Balance)
	assert.Equal(t, testAccessToken("rotated-token"), got.GetAccessToken())
}

func TestUsageAccountingSupportsSignedDirectAndBatchDeltas(t *testing.T) {
	user, _ := subscriptionFundingFixture(t, "wallet_only", 2000, 0, false, false)
	require.NoError(t, DB.AutoMigrate(&Provider{}))
	require.NoError(t, DB.Model(&user).Updates(map[string]any{"used_quota": 1000, "request_count": 3}).Error)
	provider := Provider{Id: 17, Name: "usage-adjustment-provider", Key: "fixture", Status: common.ChannelStatusEnabled, UsedQuota: 1000}
	require.NoError(t, DB.Create(&provider).Error)
	request := GatewayRequest{ID: "signed-usage", UserID: user.Id, SaleFXRate: "1"}
	require.NoError(t, ReserveGateway(&request, usd(1000), usd(1000)))
	intent := subscriptionFundingIntent(request, 1000)
	intent.Attempts[0].Cost, intent.Attempts[0].ConvertedCost = usd(1000), usd(1000)
	require.NoError(t, PrepareGatewaySettlement(intent))
	_, err := SettleGateway(request.ID)
	require.NoError(t, err)

	for _, phase := range []struct {
		name     string
		deltas   []int64
		batched  bool
		consumed int64
	}{
		{name: "direct", deltas: []int64{-200, 50}, consumed: 850},
		{name: "batch", deltas: []int64{400, -100}, batched: true, consumed: 1150},
	} {
		var postings []LedgerPosting
		for i, delta := range phase.deltas {
			kind := LedgerEntryCharge
			if delta < 0 {
				kind = LedgerEntryRefund
			}
			prefix := phase.name + "-" + strconv.Itoa(i)
			correction := []LedgerPosting{
				{Account: UserWalletAccount(user.Id), Type: kind, Amount: usd(-delta), RequestId: request.ID, IdempotencyKey: prefix + "-wallet"},
				{Account: PlatformRevenueAccount("USD"), Type: kind, Amount: usd(delta), RequestId: request.ID, IdempotencyKey: prefix + "-revenue"},
				{Account: ProviderPayableAccount(provider.Id, "USD"), Type: kind, Amount: usd(delta), RequestId: request.ID, IdempotencyKey: prefix + "-payable"},
			}
			if !phase.batched {
				_, err = PostLedgerEntries(correction...)
				require.NoError(t, err)
			}
			postings = append(postings, correction...)
		}
		if phase.batched {
			_, err = PostLedgerEntries(postings...)
			require.NoError(t, err)
		}
		var entriesBefore, entriesAfter int64
		require.NoError(t, DB.Model(&LedgerEntry{}).Count(&entriesBefore).Error)
		_, err = PostLedgerEntries(postings...)
		require.ErrorIs(t, err, ErrDuplicateLedgerEntry)
		require.NoError(t, DB.Model(&LedgerEntry{}).Count(&entriesAfter).Error)
		assert.Equal(t, entriesBefore, entriesAfter, "replayed signed corrections cannot create another entry")

		wallet, err := GetUserWallet(user.Id)
		require.NoError(t, err)
		assert.Equal(t, usd(2000-phase.consumed), wallet.Balance)
		for _, account := range []LedgerAccount{PlatformRevenueAccount("USD"), ProviderPayableAccount(provider.Id, "USD")} {
			var balance Wallet
			require.NoError(t, DB.Where("account_type = ? AND owner_id = ? AND balance_currency = ?", account.Type, account.OwnerId, account.Currency).First(&balance).Error)
			assert.Equal(t, usd(phase.consumed), balance.Balance, "signed corrections must reach both revenue and original Provider payable")
		}
		summaries, err := GetUserWalletSummaries(t.Context(), []int{user.Id})
		require.NoError(t, err)
		require.Len(t, summaries, 1)
		assert.Equal(t, MoneyTotal{Nanos: strconv.FormatInt(phase.consumed, 10), Currency: "USD"}, summaries[0].Consumed)
		var savedUser User
		var savedProvider Provider
		require.NoError(t, DB.First(&savedUser, user.Id).Error)
		require.NoError(t, DB.First(&savedProvider, provider.Id).Error)
		assert.Equal(t, 1000, savedUser.UsedQuota, "historical usage is read-only, not a second accounting balance")
		assert.Equal(t, 3, savedUser.RequestCount, "corrections are not new requests")
		assert.EqualValues(t, 1000, savedProvider.UsedQuota)
		var originalCharge LedgerEntry
		require.NoError(t, DB.Where("wallet_id = ? AND request_id = ? AND type = ?", wallet.Id, request.ID, LedgerEntryCharge).Order("id").First(&originalCharge).Error)
		assert.Equal(t, usd(-1000), originalCharge.Amount, "corrections append history instead of rewriting the original charge")
	}
}

func TestRevokeUserAccessTokenOnlyClearsLegacyToken(t *testing.T) {
	UseTestDatabases(t)
	require.NoError(t, DB.AutoMigrate(&User{}))
	require.NoError(t, migrateWalletLedger(DB))

	createdAt := int64(100)
	user := User{
		Id:                   2,
		Username:             "token-revoke-user",
		Password:             "password",
		DisplayName:          "before",
		Status:               common.UserStatusEnabled,
		Quota:                1000,
		AccessTokenCreatedAt: &createdAt,
	}
	user.SetAccessToken(testAccessToken("legacy-token"))
	require.NoError(t, DB.Create(&user).Error)

	require.NoError(t, DB.Model(&User{}).Where("id = ?", user.Id).Updates(map[string]any{
		"quota":        gorm.Expr("quota + ?", 500),
		"display_name": "concurrent-update",
	}).Error)

	_, err := PostLedgerEntries(LedgerPosting{Account: UserWalletAccount(user.Id), Type: LedgerEntryTopUp, Amount: money.Amount{Nanos: 500, Currency: "USD"}})
	require.NoError(t, err)
	tokenRef, err := RevokeUserAccessToken(user.Id)
	require.NoError(t, err)
	assert.Equal(t, AccessTokenFingerprint(testAccessToken("legacy-token")), tokenRef)

	var got User
	require.NoError(t, DB.First(&got, user.Id).Error)
	assert.Empty(t, got.GetAccessToken())
	assert.Nil(t, got.AccessTokenCreatedAt)
	assert.Equal(t, "concurrent-update", got.DisplayName)
	assert.Equal(t, 1500, got.Quota)
	wallet, err := GetUserWallet(user.Id)
	require.NoError(t, err)
	require.NotNil(t, wallet)
	assert.Equal(t, money.Amount{Nanos: 500, Currency: "USD"}, wallet.Balance)
}

func TestRevokeUserAccessTokenRejectsSoftDeletedUser(t *testing.T) {
	setupUserUpdateTestState(t)

	user := User{
		Id:       3,
		Username: "deleted-token-revoke-user",
		Password: "password",
		Status:   common.UserStatusEnabled,
	}
	user.SetAccessToken(testAccessToken("old-token"))
	require.NoError(t, DB.Create(&user).Error)
	require.NoError(t, DB.Delete(&user).Error)

	_, err := RevokeUserAccessToken(user.Id)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)

	var got User
	require.NoError(t, DB.Unscoped().First(&got, user.Id).Error)
	assert.Equal(t, testAccessToken("old-token"), got.GetAccessToken())
}

func TestUpdateUserSettingOnlyUpdatesSetting(t *testing.T) {
	setupUserUpdateTestState(t)

	user := User{
		Id:           2,
		Username:     "setting-user",
		Password:     "password",
		Status:       common.UserStatusEnabled,
		Quota:        1000,
		UsedQuota:    20,
		RequestCount: 3,
	}
	require.NoError(t, DB.Create(&user).Error)

	require.NoError(t, DB.Model(&User{}).Where("id = ?", user.Id).Updates(map[string]any{
		"quota":         gorm.Expr("quota - ?", 250),
		"used_quota":    gorm.Expr("used_quota + ?", 250),
		"request_count": gorm.Expr("request_count + ?", 1),
	}).Error)

	require.NoError(t, UpdateUserSetting(user.Id, dto.UserSetting{Language: "zh"}))

	var got User
	require.NoError(t, DB.First(&got, user.Id).Error)
	assert.Equal(t, 750, got.Quota)
	assert.Equal(t, 270, got.UsedQuota)
	assert.Equal(t, 4, got.RequestCount)
	assert.Equal(t, "zh", got.GetSetting().Language)
}

func TestEnsureEmailAvailableRejectsExistingEmailCaseInsensitive(t *testing.T) {
	setupUserUpdateTestState(t)

	require.NoError(t, DB.Create(&User{
		Username: "existing",
		Password: "old-password",
		Email:    "Taken@Example.com",
		Status:   common.UserStatusEnabled,
	}).Error)

	err := EnsureEmailAvailable(" taken@example.COM ", 0)
	require.ErrorIs(t, err, ErrEmailAlreadyTaken)

	user, err := GetUniqueUserByEmail("TAKEN@example.com")
	require.NoError(t, err)
	assert.Equal(t, "existing", user.Username)

	require.NoError(t, EnsureEmailAvailable("taken@example.com", user.Id))
}

func TestInsertRejectsDuplicateEmailWithoutUniqueIndex(t *testing.T) {
	setupUserUpdateTestState(t)

	require.NoError(t, DB.Create(&User{
		Username: "existing",
		Password: "old-password",
		Email:    "taken@example.com",
		Status:   common.UserStatusEnabled,
	}).Error)

	user := &User{
		Username: "oauth-user",
		Email:    "TAKEN@example.com",
		Role:     common.RoleCommonUser,
		Status:   common.UserStatusEnabled,
	}

	err := user.Insert(0)
	require.ErrorIs(t, err, ErrEmailAlreadyTaken)

	var count int64
	require.NoError(t, DB.Model(&User{}).Where("username = ?", "oauth-user").Count(&count).Error)
	assert.Zero(t, count)
}

func TestInsertKeepsBlankPasswordForPasswordlessUser(t *testing.T) {
	setupUserUpdateTestState(t)

	user := &User{
		Username: "passwordless-user",
		Role:     common.RoleCommonUser,
		Status:   common.UserStatusEnabled,
	}

	require.NoError(t, user.Insert(0))

	var stored User
	require.NoError(t, DB.Where("username = ?", user.Username).First(&stored).Error)
	assert.Empty(t, stored.Password)
}

func TestUpdateUserBindColumnOnlyTouchesTheBindingColumn(t *testing.T) {
	truncateTables(t)

	user := createUserBindTestUser(t)
	require.NoError(t, DB.Model(&User{}).Where("id = ?", user.Id).Updates(map[string]any{
		"role":   common.RoleAdminUser,
		"status": common.UserStatusEnabled,
		"group":  "vip",
	}).Error)

	require.NoError(t, UpdateUserBindColumn(user.Id, "github_id", "gh-12345"))

	reloaded, err := GetUserById(user.Id, true)
	require.NoError(t, err)
	assert.Equal(t, "gh-12345", reloaded.GitHubId)
	assert.Equal(t, common.RoleAdminUser, reloaded.Role)
	assert.Equal(t, common.UserStatusEnabled, reloaded.Status)
	assert.Equal(t, "vip", reloaded.Group)
}

func TestUpdateUserBindColumnPreservesRestrictiveChange(t *testing.T) {
	truncateTables(t)

	user := createUserBindTestUser(t)
	require.NoError(t, DB.Model(&User{}).Where("id = ?", user.Id).
		Update("status", common.UserStatusDisabled).Error)
	require.NoError(t, UpdateUserBindColumn(user.Id, "wechat_id", "wx-open-id"))

	reloaded, err := GetUserById(user.Id, true)
	require.NoError(t, err)
	assert.Equal(t, "wx-open-id", reloaded.WeChatId)
	assert.Equal(t, common.UserStatusDisabled, reloaded.Status)
}

func TestUpdateUserBindColumnRejectsNonWhitelistedColumns(t *testing.T) {
	truncateTables(t)

	user := createUserBindTestUser(t)
	for _, column := range []string{"role", "status", "group", "quota", "username", "password", "id"} {
		assert.Error(t, UpdateUserBindColumn(user.Id, column, "1"), "column %s must be rejected", column)
	}
	assert.Error(t, UpdateUserBindColumn(user.Id, "github_id; DROP TABLE users", "x"))
	assert.Error(t, UpdateUserBindColumn(0, "github_id", "x"))
}

func TestValidateAndFillRejectsPasswordlessUser(t *testing.T) {
	setupUserUpdateTestState(t)

	require.NoError(t, DB.Create(&User{
		Username: "passwordless-user",
		Password: "",
		Status:   common.UserStatusEnabled,
	}).Error)

	loginUser := User{
		Username: "passwordless-user",
		Password: "NewPassword123",
	}
	err := loginUser.ValidateAndFill()
	require.ErrorIs(t, err, ErrInvalidCredentials)

	var stored User
	require.NoError(t, DB.Where("username = ?", "passwordless-user").First(&stored).Error)
	assert.Empty(t, stored.Password)
}

func TestResetUserPasswordByEmailRequiresSingleActiveMatch(t *testing.T) {
	setupUserUpdateTestState(t)

	require.NoError(t, DB.Create(&User{
		Username: "duplicate-1",
		Password: "old-1",
		Email:    "legacy@example.com",
		AffCode:  "dupe1",
		Status:   common.UserStatusEnabled,
	}).Error)
	require.NoError(t, DB.Create(&User{
		Username: "duplicate-2",
		Password: "old-2",
		Email:    "LEGACY@example.com",
		AffCode:  "dupe2",
		Status:   common.UserStatusEnabled,
	}).Error)

	err := ResetUserPasswordByEmail("legacy@example.com", "NewPassword123")
	require.ErrorIs(t, err, ErrEmailAmbiguous)

	var duplicates []User
	require.NoError(t, DB.Where("LOWER(email) = ?", "legacy@example.com").Order("username asc").Find(&duplicates).Error)
	require.Len(t, duplicates, 2)
	assert.Equal(t, "old-1", duplicates[0].Password)
	assert.Equal(t, "old-2", duplicates[1].Password)

	require.NoError(t, DB.Create(&User{
		Username: "unique",
		Password: "old",
		Email:    "unique@example.com",
		AffCode:  "unique",
		Status:   common.UserStatusEnabled,
	}).Error)

	require.NoError(t, ResetUserPasswordByEmail("UNIQUE@example.com", "NewPassword123"))

	var unique User
	require.NoError(t, DB.Where("username = ?", "unique").First(&unique).Error)
	assert.True(t, common.ValidatePasswordAndHash("NewPassword123", unique.Password))

	err = ResetUserPasswordByEmail("missing@example.com", "NewPassword123")
	require.True(t, errors.Is(err, ErrEmailNotFound))
}

func createAccessTokenTestUser(t *testing.T, username string) User {
	t.Helper()
	user := User{
		Username:    username,
		Password:    "unused-password-hash",
		Role:        common.RoleCommonUser,
		Status:      common.UserStatusEnabled,
		Group:       "default",
		AuthVersion: 1,
		AffCode:     username + "-aff",
	}
	require.NoError(t, DB.Create(&user).Error)
	return user
}

func createTestUserAccessToken(t *testing.T, userID int, raw string, expiresAt int64) UserAccessToken {
	t.Helper()
	token := UserAccessToken{Name: raw, TokenHash: AccessTokenFingerprint(raw), TokenHint: AccessTokenHint(raw), ExpiresAt: expiresAt}
	require.NoError(t, token.SetScopes([]string{"profile:read"}))
	require.NoError(t, CreateUserAccessToken(userID, &token, 20))
	return token
}

func TestLegacyAccessTokenRetireAtIsWrittenOnceAndReadOnly(t *testing.T) {
	require.NoError(t, DB.AutoMigrate(&Option{}))
	previous := legacyAccessTokenRetireAt.Load()
	originalMap := common.OptionMap
	common.OptionMap = map[string]string{}
	require.NoError(t, DB.Where(&Option{Key: legacyAccessTokenRetireAtKey}).Delete(&Option{}).Error)
	t.Cleanup(func() {
		legacyAccessTokenRetireAt.Store(previous)
		common.OptionMap = originalMap
		DB.Where(&Option{Key: legacyAccessTokenRetireAtKey}).Delete(&Option{})
	})

	const firstStart = int64(1_800_000_000)
	want := firstStart + 30*24*60*60
	require.NoError(t, EnsureLegacyAccessTokenRetireAt(firstStart))
	assert.Equal(t, want, LegacyAccessTokenRetireAt())

	require.NoError(t, EnsureLegacyAccessTokenRetireAt(firstStart+24*60*60))
	assert.Equal(t, want, LegacyAccessTokenRetireAt())
	var stored Option
	require.NoError(t, DB.Where(&Option{Key: legacyAccessTokenRetireAtKey}).First(&stored).Error)
	assert.Equal(t, strconv.FormatInt(want, 10), stored.Value)

	require.ErrorIs(t, UpdateOption(legacyAccessTokenRetireAtKey, "1"), errLegacyRetireAtReadOnly)
	require.ErrorIs(t, UpdateOptionsBulk(map[string]string{legacyAccessTokenRetireAtKey: "1"}), errLegacyRetireAtReadOnly)
	require.NoError(t, DB.Where(&Option{Key: legacyAccessTokenRetireAtKey}).First(&stored).Error)
	assert.Equal(t, strconv.FormatInt(want, 10), stored.Value)

	require.NoError(t, updateOptionMap(legacyAccessTokenRetireAtKey, stored.Value))
	assert.NotContains(t, common.OptionMap, legacyAccessTokenRetireAtKey)
}

func TestValidateAccessTokenStopsAtLegacyDeadline(t *testing.T) {
	setupUserUpdateTestState(t)
	previous := legacyAccessTokenRetireAt.Load()
	t.Cleanup(func() { legacyAccessTokenRetireAt.Store(previous) })

	user := createAccessTokenTestUser(t, "legacy-deadline-user")
	require.NoError(t, DB.Model(&User{}).Where("id = ?", user.Id).Update("access_token", "legacy-deadline-token").Error)

	legacyAccessTokenRetireAt.Store(time.Now().Add(time.Hour).Unix())
	found, err := ValidateAccessToken("legacy-deadline-token")
	require.NoError(t, err)
	require.NotNil(t, found)
	assert.Equal(t, user.Id, found.Id)

	var userQueries int
	const callbackName = "test:count_legacy_token_queries"
	require.NoError(t, DB.Callback().Query().Before("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table == "users" {
			userQueries++
		}
	}))
	t.Cleanup(func() { _ = DB.Callback().Query().Remove(callbackName) })

	for _, retireAt := range []int64{time.Now().Add(-time.Second).Unix(), 0} {
		legacyAccessTokenRetireAt.Store(retireAt)
		found, err = ValidateAccessToken("legacy-deadline-token")
		require.ErrorIs(t, err, ErrLegacyAccessTokenRetired)
		assert.Nil(t, found)
	}
	assert.Zero(t, userQueries)
}

func TestUserAccessTokenLimitAndOwnership(t *testing.T) {
	setupUserUpdateTestState(t)
	owner := createAccessTokenTestUser(t, "token-owner")
	other := createAccessTokenTestUser(t, "token-other")

	var first UserAccessToken
	for i := range 20 {
		token := createTestUserAccessToken(t, owner.Id, "nap_limit_"+strconv.Itoa(i), 0)
		if i == 0 {
			first = token
		}
	}
	extra := UserAccessToken{TokenHash: AccessTokenFingerprint("nap_limit_extra")}
	require.ErrorIs(t, CreateUserAccessToken(owner.Id, &extra, 20), ErrAccessTokenLimit)
	var count int64
	require.NoError(t, DB.Model(&UserAccessToken{}).Where("user_id = ?", owner.Id).Count(&count).Error)
	assert.EqualValues(t, 20, count)

	_, err := UpdateUserAccessToken(other.Id, first.Id, "stolen", []string{"profile:read"})
	require.ErrorIs(t, err, ErrAccessTokenNotFound)
	_, err = DeleteUserAccessToken(other.Id, first.Id)
	require.ErrorIs(t, err, ErrAccessTokenNotFound)
	stored, err := GetUserAccessToken(owner.Id, first.Id)
	require.NoError(t, err)
	assert.Equal(t, first.Name, stored.Name)

	assert.Equal(t, first.Scopes, stored.Scopes)

	renamed, err := UpdateUserAccessToken(owner.Id, first.Id, "renamed", nil)
	require.NoError(t, err)
	assert.Equal(t, "renamed", renamed.Name)
	assert.Equal(t, first.Scopes, renamed.Scopes, "a rename keeps the grant")
	regranted, err := UpdateUserAccessToken(owner.Id, first.Id, "regranted", []string{"usage:read"})
	require.NoError(t, err)
	assert.Equal(t, "regranted", regranted.Name)
	assert.Equal(t, []string{"usage:read"}, regranted.GetScopes())
	deleted, err := DeleteUserAccessToken(owner.Id, first.Id)
	require.NoError(t, err)
	assert.Equal(t, first.TokenHash, deleted.TokenHash)
	found, err := FindUserAccessTokenByHash(first.TokenHash)
	require.NoError(t, err)
	assert.Nil(t, found)
}

func TestDisablingUserKeepsAccessTokensAndDeletingRemovesThem(t *testing.T) {
	setupUserUpdateTestState(t)
	user := createAccessTokenTestUser(t, "token-lifecycle-user")
	createTestUserAccessToken(t, user.Id, "nap_lifecycle_one", 0)
	createTestUserAccessToken(t, user.Id, "nap_lifecycle_two", time.Now().Add(time.Hour).Unix())
	countTokens := func() int64 {
		var count int64
		require.NoError(t, DB.Model(&UserAccessToken{}).Where("user_id = ?", user.Id).Count(&count).Error)
		return count
	}

	user.Status = common.UserStatusDisabled
	require.NoError(t, user.Update(false))
	assert.EqualValues(t, 2, countTokens())

	revoked, err := user.Delete()
	require.NoError(t, err)
	assert.EqualValues(t, 2, revoked)
	assert.Zero(t, countTokens())

	hardDeleted := createAccessTokenTestUser(t, "token-hard-delete-user")
	createTestUserAccessToken(t, hardDeleted.Id, "nap_hard_delete", 0)
	revoked, err = hardDeleted.HardDelete()
	require.NoError(t, err)
	assert.EqualValues(t, 1, revoked)
	var remaining int64
	require.NoError(t, DB.Model(&UserAccessToken{}).Where("user_id = ?", hardDeleted.Id).Count(&remaining).Error)
	assert.Zero(t, remaining)
}

func TestValidateAuthSessionWithTxChecksAccessTokenIdentity(t *testing.T) {
	setupUserUpdateTestState(t)
	now := time.Now()

	tests := []struct {
		name    string
		prepare func(user *User, token *UserAccessToken, identity *AuthSessionIdentity)
		wantErr bool
	}{
		{name: "active token", prepare: func(*User, *UserAccessToken, *AuthSessionIdentity) {}},
		{name: "expired token", wantErr: true, prepare: func(_ *User, token *UserAccessToken, _ *AuthSessionIdentity) {
			require.NoError(t, DB.Model(token).Update("expires_at", now.Add(-time.Second).Unix()).Error)
		}},
		{name: "deleted token", wantErr: true, prepare: func(user *User, token *UserAccessToken, _ *AuthSessionIdentity) {
			_, err := DeleteUserAccessToken(user.Id, token.Id)
			require.NoError(t, err)
		}},
		{name: "another user's token", wantErr: true, prepare: func(_ *User, _ *UserAccessToken, identity *AuthSessionIdentity) {
			other := createAccessTokenTestUser(t, "token-identity-other")
			identity.UserID = other.Id
		}},
		{name: "stale auth version", wantErr: true, prepare: func(user *User, _ *UserAccessToken, _ *AuthSessionIdentity) {
			require.NoError(t, DB.Model(user).Update("auth_version", 2).Error)
		}},
		{name: "disabled user", wantErr: true, prepare: func(user *User, _ *UserAccessToken, _ *AuthSessionIdentity) {
			require.NoError(t, DB.Model(user).Update("status", common.UserStatusDisabled).Error)
		}},
		{name: "unexpected session version", wantErr: true, prepare: func(_ *User, _ *UserAccessToken, identity *AuthSessionIdentity) {
			identity.SessionVersion = 2
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, DB.Exec("DELETE FROM user_access_tokens").Error)
			require.NoError(t, DB.Exec("DELETE FROM users").Error)
			user := createAccessTokenTestUser(t, "token-identity-user")
			token := createTestUserAccessToken(t, user.Id, "nap_identity_token", now.Add(time.Hour).Unix())
			identity := AuthSessionIdentity{
				UserID: user.Id, SessionID: AccessTokenSessionID(token.Id),
				UserAuthVersion: 1, SessionVersion: AccessTokenSessionVersion,
			}
			tt.prepare(&user, &token, &identity)

			txErr := DB.Transaction(func(tx *gorm.DB) error { return ValidateAuthSessionWithTx(tx, identity) })
			plainErr := ValidateAccessTokenIdentity(identity)
			if !tt.wantErr {
				assert.NoError(t, txErr)
				assert.NoError(t, plainErr)
				return
			}
			assert.ErrorIs(t, txErr, ErrUserSessionInactive)
			assert.ErrorIs(t, plainErr, ErrUserSessionInactive)
		})
	}
}

// testAccessToken returns a 32-character legacy access token derived from
// label. users.access_token is char(32), and PostgreSQL pads shorter values.
func testAccessToken(label string) string {
	sum := sha256.Sum256([]byte(label))
	return hex.EncodeToString(sum[:])[:32]
}
