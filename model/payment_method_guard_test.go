package model

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"math"
	"strings"
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

func usePaymentCompletionDB(t *testing.T) {
	t.Helper()
	UseTestDatabases(t)
	require.NoError(t, DB.AutoMigrate(&User{}, &TopUp{}, &Log{}, &SubscriptionPlan{}, &SubscriptionOrder{}, &SubscriptionOrderMoney{}, &UserSubscription{}, &FXRate{}))
	require.NoError(t, migrateWalletLedger(DB))
	require.NoError(t, MigrateSubscriptionMoney(DB))
	previousCurrency := settlementCurrency
	settlementCurrency = "USD"
	t.Cleanup(func() { settlementCurrency = previousCurrency })
	require.NoError(t, MigrateBalanceReminders(DB))
}

func paymentWalletBalance(t *testing.T, userID int) money.Amount {
	t.Helper()
	wallet, err := GetUserWallet(userID)
	require.NoError(t, err)
	if wallet == nil {
		return money.Amount{Currency: SettlementCurrency()}
	}
	return wallet.Balance
}

func completePaymentForGuardTest(provider, tradeNo string, manual bool) error {
	if manual {
		return ManualCompleteTopUp(tradeNo, "127.0.0.1")
	}
	switch provider {
	case PaymentProviderEpay:
		_, err := RechargeEpay(tradeNo, "alipay", "127.0.0.1")
		return err
	case PaymentProviderStripe:
		return Recharge(tradeNo, "customer-id", "127.0.0.1")
	case PaymentProviderCreem:
		return RechargeCreem(tradeNo, "customer@example.com", "Customer", "127.0.0.1")
	case PaymentProviderWaffo:
		return RechargeWaffo(tradeNo, "127.0.0.1")
	default:
		return RechargeWaffoPancake(tradeNo)
	}
}

func insertUserForPaymentGuardTest(t *testing.T, id int, quota int) *User {
	t.Helper()
	user := &User{
		Id:       id,
		Username: "payment_guard_user",
		Status:   common.UserStatusEnabled,
		Quota:    quota,
	}
	require.NoError(t, DB.Create(user).Error)
	return user
}

func insertSubscriptionPlanForPaymentGuardTest(t *testing.T, id int) *SubscriptionPlan {
	t.Helper()
	plan := &SubscriptionPlan{
		Id:            id,
		Title:         "Guard Plan",
		Price:         usd(9_990_000_000),
		Allowance:     usd(2_000_000),
		MoneyVersion:  1,
		DurationUnit:  SubscriptionDurationMonth,
		DurationValue: 1,
		Enabled:       true,
	}
	require.NoError(t, DB.Create(plan).Error)
	return plan
}

func insertSubscriptionOrderForPaymentGuardTest(t *testing.T, tradeNo string, userID int, planID int, paymentProvider string) {
	t.Helper()
	order := &SubscriptionOrder{
		UserId:          userID,
		PlanId:          planID,
		Money:           9.99,
		TradeNo:         tradeNo,
		PaymentMethod:   paymentProvider,
		PaymentProvider: paymentProvider,
		Status:          common.TopUpStatusPending,
		CreateTime:      time.Now().Unix(),
	}
	require.NoError(t, order.Insert())
}

func insertTopUpForPaymentGuardTest(t *testing.T, tradeNo string, userID int, paymentProvider string) {
	t.Helper()
	topUp := &TopUp{
		UserId:          userID,
		Amount:          2,
		Money:           9.99,
		TradeNo:         tradeNo,
		PaymentMethod:   paymentProvider,
		PaymentProvider: paymentProvider,
		Status:          common.TopUpStatusPending,
		CreateTime:      time.Now().Unix(),
	}
	require.NoError(t, topUp.Insert())
}

func getTopUpStatusForPaymentGuardTest(t *testing.T, tradeNo string) string {
	t.Helper()
	topUp := GetTopUpByTradeNo(tradeNo)
	require.NotNil(t, topUp)
	return topUp.Status
}

func countUserSubscriptionsForPaymentGuardTest(t *testing.T, userID int) int64 {
	t.Helper()
	var count int64
	require.NoError(t, DB.Model(&UserSubscription{}).Where("user_id = ?", userID).Count(&count).Error)
	return count
}

func getUserQuotaForPaymentGuardTest(t *testing.T, userID int) int {
	t.Helper()
	var user User
	require.NoError(t, DB.Select("quota").Where("id = ?", userID).First(&user).Error)
	return user.Quota
}

func subscriptionPurchaseRevisionForTest(t *testing.T, plan *SubscriptionPlan) string {
	t.Helper()
	revision, err := SubscriptionPurchaseRevision(plan)
	require.NoError(t, err)
	return revision
}

func TestSubscriptionPurchaseRevisionCoversAcceptedTerms(t *testing.T) {
	plan := SubscriptionPlan{Id: 901, Title: "Confirmed", Price: usd(1_000_000_001), Allowance: usd(9_007_199_254_740_993), MoneyVersion: 1,
		DurationUnit: SubscriptionDurationHour, DurationValue: 2, CustomSeconds: 7, QuotaResetPeriod: SubscriptionResetCustom,
		QuotaResetCustomSeconds: 600, MaxPurchasePerUser: 2, UpgradeGroup: "paid", DowngradeGroup: "default"}
	revision := subscriptionPurchaseRevisionForTest(t, &plan)
	require.Regexp(t, `^v1:[0-9a-f]{64}$`, revision)
	assert.Equal(t, revision, subscriptionPurchaseRevisionForTest(t, &plan))
	for name, mutate := range map[string]func(*SubscriptionPlan){
		"identity":                           func(p *SubscriptionPlan) { p.Id++ },
		"title":                              func(p *SubscriptionPlan) { p.Title += " changed" },
		"price nanos":                        func(p *SubscriptionPlan) { p.Price.Nanos++ },
		"price currency":                     func(p *SubscriptionPlan) { p.Price.Currency = "EUR" },
		"allowance nanos above JS precision": func(p *SubscriptionPlan) { p.Allowance.Nanos++ },
		"allowance currency":                 func(p *SubscriptionPlan) { p.Allowance.Currency = "EUR" },
		"unlimited":                          func(p *SubscriptionPlan) { p.Unlimited = true },
		"duration unit":                      func(p *SubscriptionPlan) { p.DurationUnit = SubscriptionDurationDay },
		"duration count":                     func(p *SubscriptionPlan) { p.DurationValue++ },
		"custom duration":                    func(p *SubscriptionPlan) { p.CustomSeconds++ },
		"reset period":                       func(p *SubscriptionPlan) { p.QuotaResetPeriod = SubscriptionResetDaily },
		"custom reset":                       func(p *SubscriptionPlan) { p.QuotaResetCustomSeconds++ },
		"wallet overflow":                    func(p *SubscriptionPlan) { p.AllowWalletOverflow = common.GetPointer(false) },
		"purchase limit":                     func(p *SubscriptionPlan) { p.MaxPurchasePerUser++ },
		"upgrade group":                      func(p *SubscriptionPlan) { p.UpgradeGroup += "-changed" },
		"downgrade group":                    func(p *SubscriptionPlan) { p.DowngradeGroup += "-changed" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := plan
			mutate(&changed)
			assert.NotEqual(t, revision, subscriptionPurchaseRevisionForTest(t, &changed))
		})
	}
	presentation := plan
	presentation.CreatedAt, presentation.UpdatedAt = 1, 2
	presentation.SortOrder, presentation.Subtitle = 50, "display only"
	presentation.Enabled, presentation.AllowBalancePay = false, common.GetPointer(false)
	presentation.AllowWalletOverflow = common.GetPointer(true)
	presentation.StripePriceId, presentation.CreemProductId, presentation.WaffoPancakeProductId = "external-price", "external-product", "external-product"
	presentation.PriceAmount, presentation.TotalAmount, presentation.LegacyCurrency = 9999, 500000, "EUR"
	assert.Equal(t, revision, subscriptionPurchaseRevisionForTest(t, &presentation), "presentation, availability and archived columns cannot rewrite an accepted promise")
	_, err := SubscriptionPurchaseRevision(nil)
	assert.Error(t, err)
	presentation.MoneyVersion = 0
	_, err = SubscriptionPurchaseRevision(&presentation)
	assert.Error(t, err)
}

func TestSubscriptionBalanceUsesExactWallet(t *testing.T) {
	for _, tc := range []struct {
		name      string
		quota     int
		wallet    int64
		wantError bool
	}{
		{"legacy quota is not purchasing power", 500000, 0, true},
		{"exact wallet works with zero legacy quota", 0, 1_000_000_002, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usePaymentCompletionDB(t)
			user := insertUserForPaymentGuardTest(t, 901, tc.quota)
			plan := &SubscriptionPlan{Id: 901, Title: "Exact plan", Price: usd(1_000_000_001), Allowance: usd(5_000_000_000), MoneyVersion: 1, DurationUnit: SubscriptionDurationMonth, DurationValue: 1, Enabled: true}
			require.NoError(t, DB.Create(plan).Error)
			// A retained source column must never become a second funding pool.
			require.NoError(t, DB.Exec(`ALTER TABLE subscription_plans ADD COLUMN price_amount numeric(10,6)`).Error)
			require.NoError(t, DB.Exec(`UPDATE subscription_plans SET price_amount=1 WHERE id=?`, plan.Id).Error)
			if tc.wallet > 0 {
				_, err := PostLedgerEntries(LedgerPosting{Account: UserWalletAccount(user.Id), Type: LedgerEntryOpeningBalance, Amount: usd(tc.wallet)})
				require.NoError(t, err)
			}
			_, err := PurchaseSubscriptionWithBalance(context.Background(), user.Id, plan.Id, "ec0c4cc5-7187-46a2-a27c-b5d02a8e04cb", subscriptionPurchaseRevisionForTest(t, plan))
			if tc.wantError {
				assert.Error(t, err)
				assert.Zero(t, countUserSubscriptionsForPaymentGuardTest(t, user.Id))
				assert.Equal(t, usd(tc.wallet), paymentWalletBalance(t, user.Id))
			} else {
				assert.NoError(t, err)
				assert.Equal(t, int64(1), countUserSubscriptionsForPaymentGuardTest(t, user.Id))
				assert.Equal(t, usd(1), paymentWalletBalance(t, user.Id))
			}
			assert.Equal(t, tc.quota, getUserQuotaForPaymentGuardTest(t, user.Id))
			var revenues int64
			require.NoError(t, DB.Model(&LedgerEntry{}).Joins("JOIN wallets ON wallets.id = ledger_entries.wallet_id").Where("wallets.account_type = ?", LedgerAccountPlatformRevenue).Count(&revenues).Error)
			assert.Zero(t, revenues, "plan purchase must not recognize request revenue")
		})
	}
}

func TestRechargeWaffoPancake_RejectsMismatchedPaymentMethod(t *testing.T) {
	usePaymentCompletionDB(t)

	insertUserForPaymentGuardTest(t, 101, 0)
	insertTopUpForPaymentGuardTest(t, "waffo-pancake-guard", 101, PaymentProviderStripe)

	err := RechargeWaffoPancake("waffo-pancake-guard")
	require.Error(t, err)

	topUp := GetTopUpByTradeNo("waffo-pancake-guard")
	require.NotNil(t, topUp)
	assert.Equal(t, common.TopUpStatusPending, topUp.Status)
	assert.Equal(t, 0, getUserQuotaForPaymentGuardTest(t, 101))
}

func TestUpdatePendingTopUpStatus_RejectsMismatchedPaymentProvider(t *testing.T) {
	testCases := []struct {
		name                    string
		tradeNo                 string
		storedPaymentProvider   string
		expectedPaymentProvider string
		targetStatus            string
	}{
		{
			name:                    "stripe expire",
			tradeNo:                 "stripe-expire-guard",
			storedPaymentProvider:   PaymentProviderCreem,
			expectedPaymentProvider: PaymentProviderStripe,
			targetStatus:            common.TopUpStatusExpired,
		},
		{
			name:                    "waffo failed",
			tradeNo:                 "waffo-failed-guard",
			storedPaymentProvider:   PaymentProviderStripe,
			expectedPaymentProvider: PaymentProviderWaffo,
			targetStatus:            common.TopUpStatusFailed,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			usePaymentCompletionDB(t)
			insertUserForPaymentGuardTest(t, 150, 0)
			insertTopUpForPaymentGuardTest(t, tc.tradeNo, 150, tc.storedPaymentProvider)

			err := UpdatePendingTopUpStatus(tc.tradeNo, tc.expectedPaymentProvider, tc.targetStatus)
			require.ErrorIs(t, err, ErrPaymentMethodMismatch)
			assert.Equal(t, common.TopUpStatusPending, getTopUpStatusForPaymentGuardTest(t, tc.tradeNo))
		})
	}
}

func TestCompleteSubscriptionOrder_RejectsMismatchedPaymentProvider(t *testing.T) {
	usePaymentCompletionDB(t)

	insertUserForPaymentGuardTest(t, 202, 0)
	plan := insertSubscriptionPlanForPaymentGuardTest(t, 301)
	insertSubscriptionOrderForPaymentGuardTest(t, "sub-guard-order", 202, plan.Id, PaymentProviderStripe)

	err := CompleteSubscriptionOrder("sub-guard-order", `{"provider":"epay"}`, PaymentProviderEpay, "alipay")
	require.ErrorIs(t, err, ErrPaymentMethodMismatch)

	order := GetSubscriptionOrderByTradeNo("sub-guard-order")
	require.NotNil(t, order)
	assert.Equal(t, common.TopUpStatusPending, order.Status)
	assert.Zero(t, countUserSubscriptionsForPaymentGuardTest(t, 202))

	topUp := GetTopUpByTradeNo("sub-guard-order")
	assert.Nil(t, topUp)
}

func TestExpireSubscriptionOrder_RejectsMismatchedPaymentProvider(t *testing.T) {
	usePaymentCompletionDB(t)

	insertUserForPaymentGuardTest(t, 303, 0)
	plan := insertSubscriptionPlanForPaymentGuardTest(t, 401)
	insertSubscriptionOrderForPaymentGuardTest(t, "sub-expire-guard", 303, plan.Id, PaymentProviderStripe)

	err := ExpireSubscriptionOrder("sub-expire-guard", PaymentProviderCreem)
	require.ErrorIs(t, err, ErrPaymentMethodMismatch)

	order := GetSubscriptionOrderByTradeNo("sub-expire-guard")
	require.NotNil(t, order)
	assert.Equal(t, common.TopUpStatusPending, order.Status)
}

func createEpayTestOrder(t *testing.T, userId int, tradeNo string, provider string, status string) TopUp {
	t.Helper()
	topUp := TopUp{
		UserId:          userId,
		Amount:          2,
		Money:           10.0,
		TradeNo:         tradeNo,
		PaymentMethod:   "alipay",
		PaymentProvider: provider,
		CreateTime:      common.GetTimestamp(),
		Status:          status,
	}
	require.NoError(t, DB.Create(&topUp).Error)
	return topUp
}

func TestRechargeEpayCreditsWalletExactlyOnce(t *testing.T) {
	usePaymentCompletionDB(t)

	user := insertUserForPaymentGuardTest(t, 501, 0)
	order := createEpayTestOrder(t, user.Id, "EPAYTESTONCE", PaymentProviderEpay, common.TopUpStatusPending)

	alreadyDone, err := RechargeEpay(order.TradeNo, "alipay", "127.0.0.1")
	require.NoError(t, err)
	assert.False(t, alreadyDone)
	assert.Equal(t, usd(2_000_000_000), paymentWalletBalance(t, user.Id))
	assert.Zero(t, getUserQuotaForPaymentGuardTest(t, user.Id))

	reloaded := GetTopUpByTradeNo(order.TradeNo)
	require.NotNil(t, reloaded)
	assert.Equal(t, common.TopUpStatusSuccess, reloaded.Status)
	assert.NotZero(t, reloaded.CompleteTime)

	alreadyDone, err = RechargeEpay(order.TradeNo, "alipay", "127.0.0.1")
	require.NoError(t, err)
	assert.True(t, alreadyDone)
	assert.Equal(t, usd(2_000_000_000), paymentWalletBalance(t, user.Id))
	assert.Zero(t, getUserQuotaForPaymentGuardTest(t, user.Id))
}

func TestRechargeEpayPreservesLegacyQuotaAndCache(t *testing.T) {
	usePaymentCompletionDB(t)
	useUserCacheMiniRedis(t)

	user := insertUserForPaymentGuardTest(t, 502, 7)
	require.NoError(t, populateUserCache(*user))
	require.NoError(t, common.RDB.HSet(t.Context(), getUserCacheKey(user.Id), "Quota", 7).Err())
	order := createEpayTestOrder(t, user.Id, "EPAYTESTREDISSYNC", PaymentProviderEpay, common.TopUpStatusPending)

	alreadyDone, err := RechargeEpay(order.TradeNo, "alipay", "127.0.0.1")
	require.NoError(t, err)
	assert.False(t, alreadyDone)
	assert.Equal(t, 7, getUserQuotaForPaymentGuardTest(t, user.Id))
	cached, err := cacheGetUserBase(user.Id)
	require.NoError(t, err)
	assert.Equal(t, user.Id, cached.Id)
	prior, err := common.RDB.HGet(t.Context(), getUserCacheKey(user.Id), "Quota").Int()
	require.NoError(t, err)
	assert.Equal(t, 7, prior)
	assert.Equal(t, usd(2_000_000_000), paymentWalletBalance(t, user.Id))

	alreadyDone, err = RechargeEpay(order.TradeNo, "alipay", "127.0.0.1")
	require.NoError(t, err)
	assert.True(t, alreadyDone)
	cached, err = cacheGetUserBase(user.Id)
	require.NoError(t, err)
	assert.Equal(t, user.Id, cached.Id)
	prior, err = common.RDB.HGet(t.Context(), getUserCacheKey(user.Id), "Quota").Int()
	require.NoError(t, err)
	assert.Equal(t, 7, prior)
	assert.Equal(t, usd(2_000_000_000), paymentWalletBalance(t, user.Id))
}

func TestRechargeEpayUpdatesPaymentMethodToActual(t *testing.T) {
	usePaymentCompletionDB(t)

	user := insertUserForPaymentGuardTest(t, 503, 0)
	order := createEpayTestOrder(t, user.Id, "EPAYTESTMETHOD", PaymentProviderEpay, common.TopUpStatusPending)

	alreadyDone, err := RechargeEpay(order.TradeNo, "wxpay", "127.0.0.1")
	require.NoError(t, err)
	assert.False(t, alreadyDone)

	reloaded := GetTopUpByTradeNo(order.TradeNo)
	require.NotNil(t, reloaded)
	assert.Equal(t, "wxpay", reloaded.PaymentMethod)
	assert.Equal(t, usd(2_000_000_000), paymentWalletBalance(t, user.Id))
	assert.Zero(t, getUserQuotaForPaymentGuardTest(t, user.Id))
}

func TestRechargeEpayRejectsForeignAndNonPendingOrders(t *testing.T) {
	usePaymentCompletionDB(t)

	user := insertUserForPaymentGuardTest(t, 504, 7)

	t.Run("order from another payment provider", func(t *testing.T) {
		order := createEpayTestOrder(t, user.Id, "EPAYTESTSTRIPE", PaymentProviderStripe, common.TopUpStatusPending)
		_, err := RechargeEpay(order.TradeNo, "alipay", "127.0.0.1")
		assert.ErrorIs(t, err, ErrPaymentMethodMismatch)
		assert.Equal(t, 7, getUserQuotaForPaymentGuardTest(t, user.Id))
	})

	t.Run("order that is not pending", func(t *testing.T) {
		order := createEpayTestOrder(t, user.Id, "EPAYTESTEXPIRED", PaymentProviderEpay, common.TopUpStatusExpired)
		_, err := RechargeEpay(order.TradeNo, "alipay", "127.0.0.1")
		assert.ErrorIs(t, err, ErrTopUpStatusInvalid)
		assert.Equal(t, 7, getUserQuotaForPaymentGuardTest(t, user.Id))
	})

	t.Run("missing order", func(t *testing.T) {
		_, err := RechargeEpay("EPAYTESTMISSING", "alipay", "127.0.0.1")
		assert.ErrorIs(t, err, ErrTopUpNotFound)
	})
}

func TestRechargeEpayRejectsAmountOverflowBeforeCompletingOrder(t *testing.T) {
	usePaymentCompletionDB(t)
	user := insertUserForPaymentGuardTest(t, 505, 3)
	order := createEpayTestOrder(t, user.Id, "EPAYTESTOVERFLOW", PaymentProviderEpay, common.TopUpStatusPending)
	require.NoError(t, DB.Model(&order).Update("amount", int64(math.MaxInt64)).Error)

	_, err := RechargeEpay(order.TradeNo, "alipay", "127.0.0.1")
	require.Error(t, err)
	assert.Equal(t, 3, getUserQuotaForPaymentGuardTest(t, user.Id))
	assert.Equal(t, usd(0), paymentWalletBalance(t, user.Id))
	assert.Equal(t, common.TopUpStatusPending, getTopUpStatusForPaymentGuardTest(t, order.TradeNo))
}

func TestPaymentCompletionCreditsWalletOnceAcrossConcurrentCallbacks(t *testing.T) {
	cases := []struct {
		provider string
		amount   int64
		charged  float64
		credit   int64
	}{
		{PaymentProviderEpay, 2, 99, 2_000_000_000},
		{PaymentProviderStripe, 2, 2.123456789, 2_123_456_789},
		{PaymentProviderCreem, 500001, 99, 1_000_002_000},
		{PaymentProviderWaffo, 2, 99, 2_000_000_000},
		{PaymentProviderWaffoPancake, 2, 99, 2_000_000_000},
	}
	for _, tc := range cases {
		for _, manual := range []bool{false, true} {
			name := tc.provider
			if manual {
				name += " manual"
			}
			t.Run(name, func(t *testing.T) {
				usePaymentCompletionDB(t)
				user := insertUserForPaymentGuardTest(t, 601, 123)
				order := createEpayTestOrder(t, user.Id, "payment-reference", tc.provider, common.TopUpStatusPending)
				require.NoError(t, DB.Model(&order).Updates(map[string]any{"amount": tc.amount, "money": tc.charged}).Error)

				start := make(chan struct{})
				errors := make(chan error, 2)
				var workers sync.WaitGroup
				for range 2 {
					workers.Go(func() {
						<-start
						errors <- completePaymentForGuardTest(tc.provider, order.TradeNo, manual)
					})
				}
				close(start)
				workers.Wait()
				close(errors)
				successes := 0
				for err := range errors {
					if err == nil {
						successes++
					}
				}
				assert.Positive(t, successes)
				// Stripe and Creem historically reject a success replay; either
				// callback contract must leave exactly one committed credit.
				_ = completePaymentForGuardTest(tc.provider, order.TradeNo, manual)
				assert.Equal(t, usd(tc.credit), paymentWalletBalance(t, user.Id))
				assert.Equal(t, 123, getUserQuotaForPaymentGuardTest(t, user.Id))
				assert.Equal(t, common.TopUpStatusSuccess, getTopUpStatusForPaymentGuardTest(t, order.TradeNo))
				var entries []LedgerEntry
				require.NoError(t, DB.Find(&entries).Error)
				require.Len(t, entries, 1)
				assert.Equal(t, LedgerEntryTopUp, entries[0].Type)
				assert.Equal(t, usd(tc.credit), entries[0].SourceAmount)
				assert.NotNil(t, entries[0].IdempotencyKey)
				var updated User
				require.NoError(t, DB.First(&updated, user.Id).Error)
				if !manual && tc.provider == PaymentProviderStripe {
					assert.Equal(t, "customer-id", updated.StripeCustomer)
				}
				if !manual && tc.provider == PaymentProviderCreem {
					assert.Equal(t, "customer@example.com", updated.Email)
				}
			})
		}
	}
}

func TestPaymentCompletionRejectsInvalidAmountsAndPreservesOrder(t *testing.T) {
	for _, provider := range []string{PaymentProviderEpay, PaymentProviderStripe, PaymentProviderCreem, PaymentProviderWaffo, PaymentProviderWaffoPancake} {
		for _, manual := range []bool{false, true} {
			name := provider
			if manual {
				name += " manual"
			}
			t.Run(name, func(t *testing.T) {
				usePaymentCompletionDB(t)
				user := insertUserForPaymentGuardTest(t, 602, 42)
				for _, amount := range []int64{0, -1, math.MaxInt64} {
					order := createEpayTestOrder(t, user.Id, decimal.NewFromInt(amount).String(), provider, common.TopUpStatusPending)
					charged := float64(amount) // A fixture for the historical stored Stripe field.
					require.NoError(t, DB.Model(&order).Updates(map[string]any{"amount": amount, "money": charged}).Error)
					require.Error(t, completePaymentForGuardTest(provider, order.TradeNo, manual))
					stored := GetTopUpByTradeNo(order.TradeNo)
					require.NotNil(t, stored)
					assert.Equal(t, common.TopUpStatusPending, stored.Status)
					assert.Zero(t, stored.CompleteTime)
				}
				assert.Equal(t, usd(0), paymentWalletBalance(t, user.Id))
				assert.Equal(t, 42, getUserQuotaForPaymentGuardTest(t, user.Id))
				var entries int64
				require.NoError(t, DB.Model(&LedgerEntry{}).Count(&entries).Error)
				assert.Zero(t, entries)
				var updated User
				require.NoError(t, DB.First(&updated, user.Id).Error)
				assert.Empty(t, updated.StripeCustomer)
				assert.Empty(t, updated.Email)
			})
		}
	}
}

func TestPaymentCompletionRejectsInvalidStoredStripeMoney(t *testing.T) {
	usePaymentCompletionDB(t)
	user := insertUserForPaymentGuardTest(t, 607, 17)
	for i, charged := range []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1), 0.0000000001} {
		order := createEpayTestOrder(t, user.Id, decimal.NewFromInt(int64(i)).String(), PaymentProviderStripe, common.TopUpStatusPending)
		require.NoError(t, DB.Model(&order).Update("money", charged).Error)
		require.Error(t, Recharge(order.TradeNo, "customer-id", "127.0.0.1"))
		require.Error(t, ManualCompleteTopUp(order.TradeNo, "127.0.0.1"))
		assert.Equal(t, common.TopUpStatusPending, getTopUpStatusForPaymentGuardTest(t, order.TradeNo))
	}
	assert.Equal(t, usd(0), paymentWalletBalance(t, user.Id))
	assert.Equal(t, 17, getUserQuotaForPaymentGuardTest(t, user.Id))
	var entries int64
	require.NoError(t, DB.Model(&LedgerEntry{}).Count(&entries).Error)
	assert.Zero(t, entries)
}

func TestPaymentCompletionRollsBackWalletAndOrderWithCustomerMetadata(t *testing.T) {
	for _, provider := range []string{PaymentProviderStripe, PaymentProviderCreem} {
		t.Run(provider, func(t *testing.T) {
			usePaymentCompletionDB(t)
			user := insertUserForPaymentGuardTest(t, 608, 23)
			order := createEpayTestOrder(t, user.Id, "metadata-failure", provider, common.TopUpStatusPending)
			require.NoError(t, DB.Model(&order).Update("amount", 500000).Error)
			require.NoError(t, DB.Exec(`CREATE FUNCTION reject_payment_metadata() RETURNS trigger AS $$
				BEGIN RAISE EXCEPTION 'Customer metadata write rejected'; END;
				$$ LANGUAGE plpgsql`).Error)
			require.NoError(t, DB.Exec(`CREATE TRIGGER reject_payment_metadata BEFORE UPDATE OF stripe_customer, email ON users
				FOR EACH ROW EXECUTE FUNCTION reject_payment_metadata()`).Error)
			require.Error(t, completePaymentForGuardTest(provider, order.TradeNo, false))
			stored := GetTopUpByTradeNo(order.TradeNo)
			require.NotNil(t, stored)
			assert.Equal(t, common.TopUpStatusPending, stored.Status)
			assert.Zero(t, stored.CompleteTime)
			assert.Equal(t, usd(0), paymentWalletBalance(t, user.Id))
			assert.Equal(t, 23, getUserQuotaForPaymentGuardTest(t, user.Id))
			var entries int64
			require.NoError(t, DB.Model(&LedgerEntry{}).Count(&entries).Error)
			assert.Zero(t, entries)
		})
	}
}

func TestPaymentCompletionPaymentReferenceCannotBeCreditedAgain(t *testing.T) {
	usePaymentCompletionDB(t)
	user := insertUserForPaymentGuardTest(t, 609, 31)
	// Historical payment references can be longer than ledger references.
	tradeNo := "long-payment-" + strings.Repeat("x", 190)
	order := createEpayTestOrder(t, user.Id, tradeNo, PaymentProviderEpay, common.TopUpStatusPending)
	_, err := RechargeEpay(order.TradeNo, "alipay", "127.0.0.1")
	require.NoError(t, err)
	require.NoError(t, DB.Model(&order).Updates(map[string]any{"status": common.TopUpStatusPending, "complete_time": 0}).Error)
	require.ErrorIs(t, ManualCompleteTopUp(order.TradeNo, "127.0.0.1"), ErrDuplicateLedgerEntry)
	assert.Equal(t, common.TopUpStatusPending, getTopUpStatusForPaymentGuardTest(t, order.TradeNo))
	assert.Equal(t, usd(2_000_000_000), paymentWalletBalance(t, user.Id))
	assert.Equal(t, 31, getUserQuotaForPaymentGuardTest(t, user.Id))
	var entries int64
	require.NoError(t, DB.Model(&LedgerEntry{}).Count(&entries).Error)
	assert.Equal(t, int64(1), entries)
}

func TestPaymentCompletionKeepsGatewayBindingAndPendingGuards(t *testing.T) {
	for _, provider := range []string{PaymentProviderEpay, PaymentProviderStripe, PaymentProviderCreem, PaymentProviderWaffo, PaymentProviderWaffoPancake} {
		t.Run(provider, func(t *testing.T) {
			usePaymentCompletionDB(t)
			user := insertUserForPaymentGuardTest(t, 603, 7)
			foreign := PaymentProviderStripe
			if provider == foreign {
				foreign = PaymentProviderCreem
			}
			mismatch := createEpayTestOrder(t, user.Id, "foreign", foreign, common.TopUpStatusPending)
			require.Error(t, completePaymentForGuardTest(provider, mismatch.TradeNo, false))
			assert.Equal(t, common.TopUpStatusPending, getTopUpStatusForPaymentGuardTest(t, mismatch.TradeNo))
			for _, manual := range []bool{false, true} {
				order := createEpayTestOrder(t, user.Id, map[bool]string{false: "expired", true: "manual expired"}[manual], provider, common.TopUpStatusExpired)
				require.Error(t, completePaymentForGuardTest(provider, order.TradeNo, manual))
				assert.Equal(t, common.TopUpStatusExpired, getTopUpStatusForPaymentGuardTest(t, order.TradeNo))
			}
			assert.Equal(t, usd(0), paymentWalletBalance(t, user.Id))
			assert.Equal(t, 7, getUserQuotaForPaymentGuardTest(t, user.Id))
		})
	}
}

func TestPaymentCompletionDoesNotCreditHistoricalSuccessOrSubscriptions(t *testing.T) {
	usePaymentCompletionDB(t)
	user := insertUserForPaymentGuardTest(t, 604, 500000)
	for _, provider := range []string{PaymentProviderEpay, PaymentProviderStripe, PaymentProviderCreem, PaymentProviderWaffo, PaymentProviderWaffoPancake} {
		for _, amount := range []int64{0, 2} {
			order := createEpayTestOrder(t, user.Id, provider+decimal.NewFromInt(amount).String(), provider, common.TopUpStatusSuccess)
			require.NoError(t, DB.Model(&order).Update("amount", amount).Error)
			_ = completePaymentForGuardTest(provider, order.TradeNo, false)
			require.NoError(t, ManualCompleteTopUp(order.TradeNo, "127.0.0.1"))
		}
	}
	assert.Equal(t, usd(0), paymentWalletBalance(t, user.Id))
	assert.Equal(t, 500000, getUserQuotaForPaymentGuardTest(t, user.Id))
	var entries int64
	require.NoError(t, DB.Model(&LedgerEntry{}).Count(&entries).Error)
	assert.Zero(t, entries)
}

func TestPaymentCompletionUsesAccountingFXAndRepaysDebt(t *testing.T) {
	usePaymentCompletionDB(t)
	settlementCurrency = "CNY"
	user := insertUserForPaymentGuardTest(t, 605, 19)
	order := createEpayTestOrder(t, user.Id, "fx-payment", PaymentProviderEpay, common.TopUpStatusPending)
	_, err := RechargeEpay(order.TradeNo, "alipay", "127.0.0.1")
	require.Error(t, err, "a missing accounting quote must roll back the order")
	assert.Equal(t, common.TopUpStatusPending, getTopUpStatusForPaymentGuardTest(t, order.TradeNo))
	require.NoError(t, DB.Create(&FXRate{FromCurrency: "USD", ToCurrency: "CNY", Rate: decimal.RequireFromString("1.1234567899"), BufferRate: decimal.RequireFromString("0.5")}).Error)
	debt := money.Amount{Nanos: 3_000_000_000, Currency: "CNY"}
	require.NoError(t, DB.Transaction(func(tx *gorm.DB) error {
		_, err := postLedgerEntries(tx, []LedgerPosting{
			{Account: UserReceivableAccount(user.Id), Type: LedgerEntryCharge, Amount: debt, RequestId: "old-request"},
			{Account: PlatformRevenueAccount("CNY"), Type: LedgerEntryCharge, Amount: debt, RequestId: "old-request"},
		})
		return err
	}))

	for i := range 2 {
		if i == 1 {
			order = createEpayTestOrder(t, user.Id, "second-fx-payment", PaymentProviderEpay, common.TopUpStatusPending)
		}
		_, err = RechargeEpay(order.TradeNo, "alipay", "127.0.0.1")
		require.NoError(t, err)
		wallet, err := GetUserWallet(user.Id)
		require.NoError(t, err)
		require.NotNil(t, wallet)
		var receivable Wallet
		require.NoError(t, DB.Where("account_type = ? AND owner_id = ?", LedgerAccountUserReceivable, user.Id).First(&receivable).Error)
		if i == 0 {
			assert.Zero(t, wallet.Balance.Nanos)
			assert.Equal(t, int64(753_086_421), receivable.Balance.Nanos)
			assert.True(t, wallet.RequestsPaused)
		} else {
			assert.Equal(t, int64(1_493_827_158), wallet.Balance.Nanos)
			assert.Zero(t, receivable.Balance.Nanos)
			assert.False(t, wallet.RequestsPaused)
		}
	}
	var credits []LedgerEntry
	require.NoError(t, DB.Where("type = ?", LedgerEntryTopUp).Find(&credits).Error)
	require.Len(t, credits, 2)
	for _, credit := range credits {
		assert.Equal(t, money.Amount{Nanos: 2_246_913_579, Currency: "CNY"}, credit.Amount)
		assert.Equal(t, usd(2_000_000_000), credit.SourceAmount)
		require.True(t, credit.FxRate.Valid)
		assert.Equal(t, "1.1234567899", credit.FxRate.Decimal.String())
	}
	var repayments []LedgerEntry
	require.NoError(t, DB.Where("type = ?", LedgerEntryRepayment).Find(&repayments).Error)
	require.Len(t, repayments, 4)
	for _, repayment := range repayments {
		assert.Equal(t, "old-request", repayment.RequestId)
	}
	var revenue Wallet
	require.NoError(t, DB.Where("account_type = ?", LedgerAccountPlatformRevenue).First(&revenue).Error)
	assert.Equal(t, debt, revenue.Balance)
	assert.Equal(t, 19, getUserQuotaForPaymentGuardTest(t, user.Id))
	result, err := ReconcileWallets(100)
	require.NoError(t, err)
	assert.Empty(t, result.Mismatches)
}

func TestRechargeEpayEnforcesFinalWalletBalanceLimit(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact maximum", true: "one nano above maximum"}[overflow], func(t *testing.T) {
			usePaymentCompletionDB(t)
			user := insertUserForPaymentGuardTest(t, 506, 11)
			current := int64(math.MaxInt64 - 2_000_000_000)
			if overflow {
				current++
			}
			_, err := PostLedgerEntries(LedgerPosting{Account: UserWalletAccount(user.Id), Type: LedgerEntryOpeningBalance, Amount: usd(current)})
			require.NoError(t, err)
			order := createEpayTestOrder(t, user.Id, "EPAYTESTWALLETLIMIT", PaymentProviderEpay, common.TopUpStatusPending)

			_, err = RechargeEpay(order.TradeNo, "alipay", "127.0.0.1")
			wantBalance, wantStatus, wantEntries := int64(math.MaxInt64), common.TopUpStatusSuccess, int64(2)
			if overflow {
				require.ErrorIs(t, err, money.ErrOverflow)
				wantBalance, wantStatus, wantEntries = current, common.TopUpStatusPending, 1
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, usd(wantBalance), paymentWalletBalance(t, user.Id))
			assert.Equal(t, 11, getUserQuotaForPaymentGuardTest(t, user.Id))
			assert.Equal(t, wantStatus, getTopUpStatusForPaymentGuardTest(t, order.TradeNo))
			var count int64
			require.NoError(t, DB.Model(&LedgerEntry{}).Count(&count).Error)
			assert.Equal(t, wantEntries, count)
		})
	}
}

func TestSubscriptionBalancePurchaseReplayAndLimits(t *testing.T) {
	usePaymentCompletionDB(t)
	user := insertUserForPaymentGuardTest(t, 902, 777)
	plan := insertSubscriptionPlanForPaymentGuardTest(t, 902)
	require.NoError(t, DB.Model(plan).Update("price_nanos", 7).Error)
	_, err := PostLedgerEntries(LedgerPosting{Account: UserWalletAccount(user.Id), Type: LedgerEntryOpeningBalance, Amount: usd(21)})
	require.NoError(t, err)
	purchaseID := uuid.NewString()
	revision := subscriptionPurchaseRevisionForTest(t, plan)
	results := make(chan *SubscriptionPurchaseResult, 8)
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := PurchaseSubscriptionWithBalance(context.Background(), user.Id, plan.Id, purchaseID, revision)
			results <- result
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var original *SubscriptionPurchaseResult
	for result := range results {
		require.NotNil(t, result)
		if original == nil {
			original = result
		}
		assert.Equal(t, original.OrderID, result.OrderID)
		assert.Equal(t, original.Subscription.Id, result.Subscription.Id)
		assert.Equal(t, usd(7), result.ContractPrice)
		assert.Equal(t, usd(7), result.WalletCharge)
		assert.Equal(t, plan.Allowance, result.Period.Available)
	}
	assert.Equal(t, usd(14), paymentWalletBalance(t, user.Id))
	assert.EqualValues(t, 1, countUserSubscriptionsForPaymentGuardTest(t, user.Id))
	assert.Equal(t, 777, getUserQuotaForPaymentGuardTest(t, user.Id))
	other := insertSubscriptionPlanForPaymentGuardTest(t, 903)
	_, err = PurchaseSubscriptionWithBalance(t.Context(), user.Id, other.Id, purchaseID, revision)
	require.ErrorIs(t, err, ErrSubscriptionPurchaseConflict)
	require.NoError(t, DB.Model(plan).Updates(map[string]any{"price_nanos": 9, "enabled": false}).Error)
	replay, err := PurchaseSubscriptionWithBalance(t.Context(), user.Id, plan.Id, purchaseID, revision)
	require.NoError(t, err)
	assert.Equal(t, usd(7), replay.ContractPrice)
	_, err = PurchaseSubscriptionWithBalance(t.Context(), user.Id, plan.Id, uuid.NewString(), subscriptionPurchaseRevisionForTest(t, plan))
	require.Error(t, err)
	require.NoError(t, DB.Model(plan).Updates(map[string]any{"enabled": true, "max_purchase_per_user": 1}).Error)
	_, err = PurchaseSubscriptionWithBalance(t.Context(), user.Id, plan.Id, uuid.NewString(), subscriptionPurchaseRevisionForTest(t, plan))
	require.ErrorContains(t, err, "Purchase limit")
	assert.Equal(t, usd(14), paymentWalletBalance(t, user.Id))
}

func TestSubscriptionBalanceFreeAndAtomicFailures(t *testing.T) {
	for _, scenario := range []string{"free", "period failure", "order failure", "foreign currency", "wallet paused", "disabled user", "concurrent spend"} {
		t.Run(scenario, func(t *testing.T) {
			usePaymentCompletionDB(t)
			user := insertUserForPaymentGuardTest(t, 904, 999)
			plan := insertSubscriptionPlanForPaymentGuardTest(t, 904)
			require.NoError(t, DB.Model(plan).Update("price_nanos", 10).Error)
			if scenario != "free" {
				_, err := PostLedgerEntries(LedgerPosting{Account: UserWalletAccount(user.Id), Type: LedgerEntryOpeningBalance, Amount: usd(10)})
				require.NoError(t, err)
			}
			switch scenario {
			case "free":
				require.NoError(t, DB.Model(plan).Update("price_nanos", 0).Error)
			case "period failure", "order failure":
				table := "subscription_periods"
				if scenario == "order failure" {
					table = "subscription_orders"
				}
				require.NoError(t, DB.Exec(`CREATE FUNCTION purchase_test_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'forced purchase rollback'; END $$`).Error)
				require.NoError(t, DB.Exec(fmt.Sprintf(`CREATE TRIGGER purchase_test_fail BEFORE INSERT ON %s FOR EACH ROW EXECUTE FUNCTION purchase_test_fail()`, table)).Error)
			case "foreign currency":
				require.NoError(t, DB.Model(plan).Update("price_currency", "EUR").Error)
			case "wallet paused":
				require.NoError(t, DB.Model(&Wallet{}).Where("account_type = ? AND owner_id = ?", LedgerAccountUserWallet, user.Id).Update("requests_paused", true).Error)
			case "disabled user":
				require.NoError(t, DB.Model(user).Update("status", common.UserStatusDisabled).Error)
			}
			id := uuid.NewString()
			revision := subscriptionPurchaseRevisionForTest(t, plan)
			if scenario == "concurrent spend" {
				errs := make(chan error, 2)
				var wg sync.WaitGroup
				for range 2 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						_, err := PurchaseSubscriptionWithBalance(context.Background(), user.Id, plan.Id, uuid.NewString(), revision)
						errs <- err
					}()
				}
				wg.Wait()
				close(errs)
				successes := 0
				for err := range errs {
					if err == nil {
						successes++
					} else {
						assert.EqualError(t, err, "Insufficient balance")
					}
				}
				assert.Equal(t, 1, successes)
				assert.Equal(t, usd(0), paymentWalletBalance(t, user.Id))
				return
			}
			result, err := PurchaseSubscriptionWithBalance(t.Context(), user.Id, plan.Id, id, revision)
			if scenario == "free" {
				require.NoError(t, err)
				assert.Equal(t, usd(0), result.WalletCharge)
				again, err := PurchaseSubscriptionWithBalance(t.Context(), user.Id, plan.Id, id, revision)
				require.NoError(t, err)
				assert.Equal(t, result.OrderID, again.OrderID)
				assert.EqualValues(t, 1, countUserSubscriptionsForPaymentGuardTest(t, user.Id))
			} else {
				require.Error(t, err)
				assert.Zero(t, countUserSubscriptionsForPaymentGuardTest(t, user.Id))
				assert.Equal(t, usd(10), paymentWalletBalance(t, user.Id))
				var count int64
				require.NoError(t, DB.Model(&SubscriptionOrderMoney{}).Count(&count).Error)
				assert.Zero(t, count)
			}
			var charges int64
			require.NoError(t, DB.Model(&LedgerEntry{}).Where("type = ?", LedgerEntryCharge).Count(&charges).Error)
			assert.Zero(t, charges)
			assert.Equal(t, 999, getUserQuotaForPaymentGuardTest(t, user.Id))
		})
	}
}

func TestSubscriptionLifecyclePreservesFinancialEvidence(t *testing.T) {
	usePaymentCompletionDB(t)
	user := insertUserForPaymentGuardTest(t, 905, 500000)
	plan := insertSubscriptionPlanForPaymentGuardTest(t, 905)
	require.NoError(t, DB.Model(plan).Update("price_nanos", 0).Error)
	result, err := PurchaseSubscriptionWithBalance(t.Context(), user.Id, plan.Id, uuid.NewString(), subscriptionPurchaseRevisionForTest(t, plan))
	require.NoError(t, err)
	before := result.Period.PeriodID
	_, err = AdminDeleteUserSubscription(result.Subscription.Id)
	require.ErrorContains(t, err, "financial history")
	_, err = AdminInvalidateUserSubscription(result.Subscription.Id)
	require.NoError(t, err)
	period, err := GetSubscriptionPeriodSummary(t.Context(), result.Subscription.Id)
	require.NoError(t, err)
	assert.Equal(t, before, period.PeriodID)
	assert.Equal(t, plan.Allowance, period.Available)
	_, err = PreConsumeUserSubscription("legacy", user.Id, "m", 0, 1)
	require.ErrorIs(t, err, ErrLegacySubscriptionQuota)
	require.ErrorIs(t, RefundSubscriptionPreConsume("legacy"), ErrLegacySubscriptionQuota)
	require.ErrorIs(t, PostConsumeUserSubscriptionDelta(result.Subscription.Id, 1), ErrLegacySubscriptionQuota)
	require.NoError(t, DB.AutoMigrate(&SubscriptionPreConsumeRecord{}))
	record := SubscriptionPreConsumeRecord{RequestId: "old", UserId: user.Id, UserSubscriptionId: result.Subscription.Id, PreConsumed: 1, Status: "consumed", CreatedAt: 1, UpdatedAt: 1}
	require.NoError(t, DB.Create(&record).Error)
	removed, err := CleanupSubscriptionPreConsumeRecords(1)
	require.NoError(t, err)
	assert.Zero(t, removed)
	var archived SubscriptionPreConsumeRecord
	require.NoError(t, DB.First(&archived, record.Id).Error)
	assert.Equal(t, record, archived)
}

func TestSubscriptionExternalContractDoesNotClaimWalletCash(t *testing.T) {
	usePaymentCompletionDB(t)
	user := insertUserForPaymentGuardTest(t, 906, 654321)
	plan := insertSubscriptionPlanForPaymentGuardTest(t, 906)
	plan.DurationUnit, plan.CustomSeconds = SubscriptionDurationCustom, 7200
	plan.QuotaResetPeriod, plan.QuotaResetCustomSeconds = SubscriptionResetCustom, 600
	plan.AllowWalletOverflow = common.GetPointer(false)
	plan.UpgradeGroup, plan.DowngradeGroup = "vip", "after-vip"
	plan.MaxPurchasePerUser = 2
	require.NoError(t, DB.Save(plan).Error)
	originalPrice, originalAllowance := plan.Price, plan.Allowance
	order := SubscriptionOrder{UserId: user.Id, PlanId: plan.Id, TradeNo: "exact-external-subscription", PaymentMethod: PaymentMethodStripe, PaymentProvider: PaymentProviderStripe, Status: common.TopUpStatusPending}
	require.NoError(t, order.InsertWithContract(plan))
	contract, err := GetSubscriptionOrderMoney(order.TradeNo)
	require.NoError(t, err)
	assert.Equal(t, plan.Price, contract.ContractPrice)
	assert.Equal(t, usd(0), contract.WalletCharge)
	require.NoError(t, DB.Model(&SubscriptionPlan{}).Where("id = ?", plan.Id).Updates(map[string]any{
		"price_nanos": 1, "enabled": false, "allowance_nanos": 0, "unlimited": true,
		"custom_seconds": 17, "quota_reset_period": SubscriptionResetNever, "quota_reset_custom_seconds": 0,
		"allow_wallet_overflow": true, "upgrade_group": "changed", "downgrade_group": "changed-down", "max_purchase_per_user": 0,
	}).Error)
	require.NoError(t, CompleteSubscriptionOrder(order.TradeNo, "sensitive provider payload must not be retained", PaymentProviderStripe, ""))
	require.NoError(t, CompleteSubscriptionOrder(order.TradeNo, "", PaymentProviderStripe, ""))
	assert.EqualValues(t, 1, countUserSubscriptionsForPaymentGuardTest(t, user.Id))
	assert.Equal(t, usd(0), paymentWalletBalance(t, user.Id))
	assert.Equal(t, 654321, getUserQuotaForPaymentGuardTest(t, user.Id))
	contract, err = GetSubscriptionOrderMoney(order.TradeNo)
	require.NoError(t, err)
	assert.Equal(t, originalPrice, contract.ContractPrice)
	assert.Positive(t, contract.UserSubscriptionID)
	var granted UserSubscription
	require.NoError(t, DB.First(&granted, contract.UserSubscriptionID).Error)
	assert.Equal(t, originalAllowance, granted.Allowance)
	assert.False(t, granted.Unlimited)
	assert.EqualValues(t, 7200, granted.EndTime-granted.StartTime)
	assert.EqualValues(t, 600, granted.NextResetTime-granted.StartTime)
	assert.False(t, granted.AllowWalletOverflow)
	assert.Equal(t, "vip", granted.UpgradeGroup)
	assert.Equal(t, "after-vip", granted.DowngradeGroup)
	var stored SubscriptionOrder
	require.NoError(t, DB.Where("trade_no = ?", order.TradeNo).First(&stored).Error)
	assert.Empty(t, stored.ProviderPayload)
	var oldAmount, oldMoney *string
	require.NoError(t, DB.Raw("SELECT amount::text FROM top_ups WHERE trade_no = ?", order.TradeNo).Scan(&oldAmount).Error)
	require.NoError(t, DB.Raw("SELECT money::text FROM top_ups WHERE trade_no = ?", order.TradeNo).Scan(&oldMoney).Error)
	assert.Nil(t, oldAmount)
	assert.Nil(t, oldMoney)
	var revenues int64
	require.NoError(t, DB.Model(&LedgerEntry{}).Where("account_type = ?", LedgerAccountPlatformRevenue).Count(&revenues).Error)
	assert.Zero(t, revenues)
}

func TestSubscriptionGrantTermsRemainFrozenAcrossResets(t *testing.T) {
	for _, method := range []string{"balance", "external"} {
		t.Run(method, func(t *testing.T) {
			usePaymentCompletionDB(t)
			user := insertUserForPaymentGuardTest(t, 911, 500000)
			plan := insertSubscriptionPlanForPaymentGuardTest(t, 911)
			plan.DurationUnit, plan.CustomSeconds = SubscriptionDurationCustom, 7200
			plan.QuotaResetPeriod, plan.QuotaResetCustomSeconds = SubscriptionResetCustom, 600
			plan.AllowWalletOverflow = common.GetPointer(false)
			require.NoError(t, DB.Save(plan).Error)
			var subscriptionID int
			var tradeNo string
			if method == "balance" {
				_, err := PostLedgerEntries(LedgerPosting{Account: UserWalletAccount(user.Id), Type: LedgerEntryOpeningBalance, Amount: plan.Price})
				require.NoError(t, err)
				result, err := PurchaseSubscriptionWithBalance(t.Context(), user.Id, plan.Id, uuid.NewString(), subscriptionPurchaseRevisionForTest(t, plan))
				require.NoError(t, err)
				subscriptionID, tradeNo = result.Subscription.Id, result.OrderID
			} else {
				order := SubscriptionOrder{UserId: user.Id, PlanId: plan.Id, TradeNo: "frozen-reset", PaymentMethod: PaymentMethodStripe, PaymentProvider: PaymentProviderStripe, Status: common.TopUpStatusPending}
				require.NoError(t, order.InsertWithContract(plan))
				require.NoError(t, CompleteSubscriptionOrder(order.TradeNo, "", PaymentProviderStripe, ""))
				contract, err := GetSubscriptionOrderMoney(order.TradeNo)
				require.NoError(t, err)
				subscriptionID, tradeNo = contract.UserSubscriptionID, order.TradeNo
			}
			contract, err := GetSubscriptionOrderMoney(tradeNo)
			require.NoError(t, err)
			assert.Equal(t, 1, contract.GrantTermsVersion)
			assert.NotEmpty(t, contract.GrantTerms)
			require.NoError(t, DB.Model(&SubscriptionPlan{}).Where("id = ?", plan.Id).Updates(map[string]any{
				"allowance_nanos": 0, "unlimited": true, "quota_reset_period": SubscriptionResetNever,
				"quota_reset_custom_seconds": 0, "allow_wallet_overflow": true,
			}).Error)
			var current SubscriptionPlan
			require.NoError(t, DB.First(&current, plan.Id).Error)
			var start int64
			require.NoError(t, DB.Transaction(func(tx *gorm.DB) error {
				sub, err := lockSubscriptionLifecycleTx(tx, subscriptionID)
				if err != nil {
					return err
				}
				start = sub.StartTime
				return maybeResetUserSubscriptionWithPlanTx(tx, sub, &current, start+610)
			}))
			var reset UserSubscription
			require.NoError(t, DB.First(&reset, subscriptionID).Error)
			assert.Equal(t, start+1200, reset.NextResetTime)
			assert.Equal(t, plan.Allowance, reset.Allowance)
			assert.False(t, reset.Unlimited)
			assert.False(t, reset.AllowWalletOverflow)
			period, err := GetSubscriptionPeriodSummary(t.Context(), subscriptionID)
			require.NoError(t, err)
			assert.EqualValues(t, 2, period.Sequence)
			assert.Equal(t, plan.Allowance, period.Available)
			require.NoError(t, DB.Transaction(func(tx *gorm.DB) error {
				sub, err := lockSubscriptionLifecycleTx(tx, subscriptionID)
				if err != nil {
					return err
				}
				return resetUserSubscriptionTx(tx, sub, &current, start+1234, true)
			}))
			require.NoError(t, DB.First(&reset, subscriptionID).Error)
			assert.Equal(t, start+1834, reset.NextResetTime)
			period, err = GetSubscriptionPeriodSummary(t.Context(), subscriptionID)
			require.NoError(t, err)
			assert.EqualValues(t, 3, period.Sequence)
			assert.Equal(t, plan.Allowance, period.Available)
			after, err := GetSubscriptionOrderMoney(tradeNo)
			require.NoError(t, err)
			assert.Equal(t, contract, after)
			assert.Equal(t, 500000, getUserQuotaForPaymentGuardTest(t, user.Id))
		})
	}
}

func TestSubscriptionGrantTermsLegacyAndInvalidSnapshots(t *testing.T) {
	for _, variant := range []string{"legacy_no_contract", "legacy_v0", "unknown_version", "bad_json", "missing_fields", "stale_creation"} {
		t.Run(variant, func(t *testing.T) {
			usePaymentCompletionDB(t)
			user := insertUserForPaymentGuardTest(t, 912, 0)
			plan := insertSubscriptionPlanForPaymentGuardTest(t, 912)
			order := SubscriptionOrder{UserId: user.Id, PlanId: plan.Id, TradeNo: "grant-" + variant, PaymentMethod: PaymentMethodStripe, PaymentProvider: PaymentProviderStripe, Status: common.TopUpStatusPending}
			if variant == "stale_creation" {
				require.NoError(t, DB.Model(&SubscriptionPlan{}).Where("id = ?", plan.Id).Update("allowance_nanos", 3).Error)
				require.ErrorIs(t, order.InsertWithContract(plan), errSubscriptionGrantTerms)
				var count int64
				require.NoError(t, DB.Model(&SubscriptionOrder{}).Count(&count).Error)
				assert.Zero(t, count)
				return
			}
			require.NoError(t, order.Insert())
			if variant != "legacy_no_contract" {
				contract, err := newSubscriptionOrderMoney(plan, order.TradeNo, usd(0), 0)
				require.NoError(t, err)
				switch variant {
				case "legacy_v0":
					contract.GrantTermsVersion, contract.GrantTerms = 0, ""
				case "unknown_version":
					contract.GrantTermsVersion = 2
				case "bad_json":
					contract.GrantTerms = "{"
				case "missing_fields":
					contract.GrantTerms = `{}`
				}
				require.NoError(t, DB.Create(contract).Error)
			}
			require.NoError(t, DB.Model(&SubscriptionPlan{}).Where("id = ?", plan.Id).Updates(map[string]any{"allowance_nanos": 123, "enabled": false}).Error)
			err := CompleteSubscriptionOrder(order.TradeNo, "", PaymentProviderStripe, "")
			if strings.HasPrefix(variant, "legacy_") {
				require.NoError(t, err)
				var granted UserSubscription
				require.NoError(t, DB.Where("user_id = ?", user.Id).First(&granted).Error)
				assert.Equal(t, usd(123), granted.Allowance)
				if variant == "legacy_v0" {
					stored, err := GetSubscriptionOrderMoney(order.TradeNo)
					require.NoError(t, err)
					assert.Zero(t, stored.GrantTermsVersion)
					assert.Empty(t, stored.GrantTerms)
				} else {
					_, err := GetSubscriptionOrderMoney(order.TradeNo)
					require.ErrorIs(t, err, gorm.ErrRecordNotFound)
				}
				return
			}
			require.ErrorIs(t, err, errSubscriptionGrantTerms)
			assert.Zero(t, countUserSubscriptionsForPaymentGuardTest(t, user.Id))
			var stored SubscriptionOrder
			require.NoError(t, DB.First(&stored, order.Id).Error)
			assert.Equal(t, common.TopUpStatusPending, stored.Status)
			var count int64
			require.NoError(t, DB.Model(&LedgerEntry{}).Count(&count).Error)
			assert.Zero(t, count)
		})
	}
}
