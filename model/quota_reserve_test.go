package model

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestGatewayReservationWithoutRedis(t *testing.T) {
	user, _ := subscriptionFundingFixture(t, "wallet_only", 100, 0, false, false)
	oldRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = oldRedisEnabled })

	request := GatewayRequest{ID: "reserve-without-cache", UserID: user.Id, SaleFXRate: "1"}
	require.NoError(t, ReserveGateway(&request, usd(60), usd(60)))
	wallet, err := GetUserWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, usd(40), wallet.Balance)
	second := GatewayRequest{ID: "reserve-insufficient", UserID: user.Id, SaleFXRate: "1"}
	require.ErrorIs(t, ReserveGateway(&second, usd(41), usd(41)), ErrInsufficientWalletBalance)
	wallet, err = GetUserWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, usd(40), wallet.Balance)

	require.NoError(t, PrepareGatewaySettlement(subscriptionFundingIntent(request, 25)))
	_, err = SettleGateway(request.ID)
	require.NoError(t, err)
	_, err = SettleGateway(request.ID)
	require.NoError(t, err)
	wallet, err = GetUserWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, usd(75), wallet.Balance, "only the actual charge remains after release and replay")
	var holds, charges int64
	require.NoError(t, DB.Model(&LedgerEntry{}).Where("wallet_id = ? AND type = ?", wallet.Id, LedgerEntryPreConsume).Count(&holds).Error)
	require.NoError(t, DB.Model(&LedgerEntry{}).Where("wallet_id = ? AND type = ?", wallet.Id, LedgerEntryCharge).Count(&charges).Error)
	assert.EqualValues(t, 1, holds)
	assert.EqualValues(t, 1, charges)
}

func TestGatewayReservationUsesOnlyLedgerBalanceWithLegacyCaches(t *testing.T) {
	for _, cacheState := range []string{"stale_high", "stale_zero", "unavailable"} {
		t.Run(cacheState, func(t *testing.T) {
			user, _ := subscriptionFundingFixture(t, "wallet_only", 10, 0, false, false)
			require.NoError(t, DB.AutoMigrate(&Token{}))
			server := useUserCacheMiniRedis(t)
			legacy := math.MaxInt32
			if cacheState == "stale_zero" {
				legacy = 0
			}
			user.Quota = legacy
			require.NoError(t, DB.Model(&user).UpdateColumn("quota", legacy).Error)
			require.NoError(t, populateUserCache(user))
			token := Token{UserId: user.Id, Key: "canonical-cache-fixture", Name: "canonical cache", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: legacy}
			require.NoError(t, token.Insert())
			_, err := GetTokenByKey(token.Key, true)
			require.NoError(t, err)
			if cacheState == "unavailable" {
				server.Close()
			}

			requests := []GatewayRequest{{ID: "cache-reservation-a", UserID: user.Id, TokenID: token.Id, SaleFXRate: "1"}, {ID: "cache-reservation-b", UserID: user.Id, TokenID: token.Id, SaleFXRate: "1"}}
			errs := make([]error, len(requests))
			var group sync.WaitGroup
			for i := range requests {
				group.Go(func() { errs[i] = ReserveGateway(&requests[i], usd(7), usd(7)) })
			}
			group.Wait()
			winner := 0
			if errs[0] != nil {
				winner = 1
			}
			require.NoError(t, errs[winner], "zero legacy quota or an unavailable cache must not block funded reservations")
			require.ErrorIs(t, errs[1-winner], ErrInsufficientWalletBalance, "stale legacy balances must not authorize a second reservation")
			wallet, err := GetUserWallet(user.Id)
			require.NoError(t, err)
			assert.Equal(t, usd(3), wallet.Balance)
			var lifecycles, holds int64
			require.NoError(t, DB.Model(&GatewayBilling{}).Count(&lifecycles).Error)
			require.NoError(t, DB.Model(&LedgerEntry{}).Where("type = ?", LedgerEntryPreConsume).Count(&holds).Error)
			assert.EqualValues(t, 1, lifecycles, "the rejected reservation must leave no billing row")
			assert.EqualValues(t, 1, holds)

			require.NoError(t, PrepareGatewaySettlement(subscriptionFundingIntent(requests[winner], 0)))
			_, err = SettleGateway(requests[winner].ID)
			require.NoError(t, err)
			_, err = SettleGateway(requests[winner].ID)
			require.NoError(t, err)
			wallet, err = GetUserWallet(user.Id)
			require.NoError(t, err)
			assert.Equal(t, usd(10), wallet.Balance, "no-output settlement and its replay release the hold once")
			var releases int64
			require.NoError(t, DB.Model(&LedgerEntry{}).Where("type = ?", LedgerEntryPreConsumeRelease).Count(&releases).Error)
			assert.EqualValues(t, 1, releases)
			var savedUser User
			var savedToken Token
			require.NoError(t, DB.First(&savedUser, user.Id).Error)
			require.NoError(t, DB.First(&savedToken, token.Id).Error)
			assert.Equal(t, legacy, savedUser.Quota)
			assert.Equal(t, legacy, savedToken.RemainQuota)
			assert.Zero(t, savedUser.UsedQuota)
			assert.Zero(t, savedToken.UsedQuota)
			if cacheState != "unavailable" {
				cachedUser, err := cacheGetUserBase(user.Id)
				require.NoError(t, err)
				cachedToken, err := cacheGetTokenByKey(token.Key)
				require.NoError(t, err)
				assert.Equal(t, legacy, cachedUser.Quota)
				assert.Equal(t, legacy, cachedToken.RemainQuota)
				assert.Zero(t, cachedToken.UsedQuota)
			}
		})
	}
}

func TestGatewayReservationAccumulatesLargeChargesExactly(t *testing.T) {
	const charge int64 = math.MaxInt64 / 3
	user, _ := subscriptionFundingFixture(t, "wallet_only", 2*charge+100, 0, false, false)
	for i := range 2 {
		request := GatewayRequest{ID: fmt.Sprintf("large-charge-%d", i), UserID: user.Id, SaleFXRate: "1"}
		require.NoError(t, ReserveGateway(&request, usd(charge), usd(charge)))
		require.NoError(t, PrepareGatewaySettlement(subscriptionFundingIntent(request, charge)))
		_, err := SettleGateway(request.ID)
		require.NoError(t, err)
		_, err = SettleGateway(request.ID)
		require.NoError(t, err)
	}
	wallet, err := GetUserWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, usd(100), wallet.Balance)
	var revenue Wallet
	require.NoError(t, DB.Where("account_type = ? AND owner_id = ? AND balance_currency = ?", LedgerAccountPlatformRevenue, 0, "USD").First(&revenue).Error)
	assert.Equal(t, usd(2*charge), revenue.Balance, "multiple large charges must not overflow or saturate an int accumulator")
	var charges int64
	require.NoError(t, DB.Model(&LedgerEntry{}).Where("wallet_id = ? AND type = ?", wallet.Id, LedgerEntryCharge).Count(&charges).Error)
	assert.EqualValues(t, 2, charges)
}

func TestLedgerBatchRejectsBothOverflowDirectionsAtomically(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, second int64
	}{
		{name: "positive", first: math.MaxInt64, second: 1},
		{name: "negative", first: math.MinInt64, second: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useWalletLedgerDB(t)
			account := PlatformRevenueAccount("USD")
			_, err := PostLedgerEntries(
				LedgerPosting{Account: account, Type: LedgerEntryAdjustment, Amount: usd(tc.first), IdempotencyKey: "overflow-first"},
				LedgerPosting{Account: account, Type: LedgerEntryAdjustment, Amount: usd(tc.second), IdempotencyKey: "overflow-second"},
			)
			require.ErrorIs(t, err, money.ErrOverflow, "overflow must reject, never clamp or wrap into an opposite-signed amount")
			var entries, wallets int64
			require.NoError(t, DB.Model(&LedgerEntry{}).Count(&entries).Error)
			require.NoError(t, DB.Model(&Wallet{}).Count(&wallets).Error)
			assert.Zero(t, entries, "the first posting must roll back with the second")
			assert.Zero(t, wallets)
			_, err = PostLedgerEntries(LedgerPosting{Account: account, Type: LedgerEntryAdjustment, Amount: usd(tc.second), IdempotencyKey: "overflow-first"})
			require.NoError(t, err, "rollback must not consume the original idempotency key")
			var wallet Wallet
			require.NoError(t, DB.Where("account_type = ? AND owner_id = ? AND balance_currency = ?", account.Type, account.OwnerId, account.Currency).First(&wallet).Error)
			assert.Equal(t, usd(tc.second), wallet.Balance)
		})
	}
}

func TestGatewayReservationRollsBackLedgerAndLeavesCachesUnchanged(t *testing.T) {
	user, _ := subscriptionFundingFixture(t, "wallet_only", 10, 0, false, false)
	require.NoError(t, DB.AutoMigrate(&Token{}))
	useUserCacheMiniRedis(t)
	user.Quota = 123
	require.NoError(t, DB.Model(&user).UpdateColumn("quota", user.Quota).Error)
	require.NoError(t, populateUserCache(user))
	token := Token{UserId: user.Id, Key: "rollback-cache-fixture", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 456}
	require.NoError(t, token.Insert())
	_, err := GetTokenByKey(token.Key, true)
	require.NoError(t, err)
	injected := errors.New("injected reservation snapshot failure")
	require.NoError(t, DB.Callback().Update().Before("gorm:update").Register("reserve_snapshot_failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "gateway_billings" {
			tx.AddError(injected)
		}
	}))
	t.Cleanup(func() { DB.Callback().Update().Remove("reserve_snapshot_failure") })
	request := GatewayRequest{ID: "rollback-reservation", UserID: user.Id, TokenID: token.Id, SaleFXRate: "1"}
	require.ErrorIs(t, ReserveGateway(&request, usd(7), usd(7)), injected)
	wallet, err := GetUserWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, usd(10), wallet.Balance)
	var lifecycles, holds int64
	require.NoError(t, DB.Model(&GatewayBilling{}).Count(&lifecycles).Error)
	require.NoError(t, DB.Model(&LedgerEntry{}).Where("type = ?", LedgerEntryPreConsume).Count(&holds).Error)
	assert.Zero(t, lifecycles)
	assert.Zero(t, holds)
	cachedUser, err := cacheGetUserBase(user.Id)
	require.NoError(t, err)
	cachedToken, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, 123, cachedUser.Quota)
	assert.Equal(t, 456, cachedToken.RemainQuota)
	assert.Zero(t, cachedToken.UsedQuota)
	require.NoError(t, DB.Callback().Update().Remove("reserve_snapshot_failure"))
	require.NoError(t, ReserveGateway(&request, usd(7), usd(7)), "the same request can retry after a fully rolled-back transaction")
	wallet, err = GetUserWallet(user.Id)
	require.NoError(t, err)
	assert.Equal(t, usd(3), wallet.Balance)
}

func TestTokenCacheFenceBlocksStaleCredentialSnapshot(t *testing.T) {
	truncateTables(t)
	server := useUserCacheMiniRedis(t)
	token := Token{UserId: 1, Key: "reserve-fence-fixture", Name: "cache fence", Status: common.TokenStatusEnabled, ExpiredTime: -1}
	require.NoError(t, token.Insert())
	loaded, err := GetTokenByKey(token.Key, true)
	require.NoError(t, err)
	stale := *loaded
	_, err = ValidateWalletToken(token.Key)
	require.NoError(t, err, "zero legacy quota is not a credential restriction")

	token.Status = common.TokenStatusDisabled
	require.NoError(t, token.SelectUpdate())
	code, err := cacheInitToken(stale)
	require.NoError(t, err)
	assert.Zero(t, code, "the pre-mutation snapshot must not be published while fenced")
	_, err = cacheGetTokenByKey(token.Key)
	require.Error(t, err)
	_, err = ValidateWalletToken(token.Key)
	require.ErrorIs(t, err, ErrTokenInvalid)

	server.FastForward(time.Duration(tokenCacheFenceSeconds+1) * time.Second)
	fresh, err := GetTokenByKey(token.Key, false)
	require.NoError(t, err)
	assert.Equal(t, common.TokenStatusDisabled, fresh.Status)
	_, err = cacheInitToken(*fresh)
	require.NoError(t, err)
	code, err = cacheInitToken(stale)
	require.NoError(t, err)
	assert.Equal(t, 2, code, "an older snapshot must not overwrite a valid current credential cache")
	cached, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, common.TokenStatusDisabled, cached.Status)
	_, err = ValidateWalletToken(token.Key)
	require.ErrorIs(t, err, ErrTokenInvalid, "the original credential restriction must survive cache rehydration")
}
