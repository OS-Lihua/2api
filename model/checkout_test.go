package model

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/pkg/clock"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func useCheckoutDB(t *testing.T) {
	t.Helper()
	usePaymentCompletionDB(t)
	require.NoError(t, migrateCheckout(DB))
	previous := operation_setting.GetCheckoutConfig()
	payment := operation_setting.GetPaymentSetting()
	confirmed, terms := payment.ComplianceConfirmed, payment.ComplianceTermsVersion
	payment.ComplianceConfirmed, payment.ComplianceTermsVersion = true, operation_setting.CurrentComplianceTermsVersion
	t.Cleanup(func() {
		payment.ComplianceConfirmed, payment.ComplianceTermsVersion = confirmed, terms
		encoded, err := common.Marshal(previous)
		require.NoError(t, err)
		require.NoError(t, operation_setting.ApplyCheckoutOption(operation_setting.CheckoutConfigOption, string(encoded)))
	})
	require.NoError(t, operation_setting.ApplyCheckoutOption(operation_setting.CheckoutConfigOption, `{"providers":{"epay":{"enabled":true,"source_unit":{"nanos":"1000000000","currency":"USD"},"charge_unit":{"nanos":"7300000000","currency":"CNY"},"minimum":{"nanos":"1","currency":"USD"},"presets":[],"group_ratios":{},"discounts":[],"products":[]}}}`))
}

func TestCheckoutQuoteFreezesFXAndExactAmounts(t *testing.T) {
	useCheckoutDB(t)
	user := insertUserForPaymentGuardTest(t, 8101, common.MaxWalletQuota)
	settlementCurrency = "CNY"
	require.NoError(t, DB.Create(&FXRate{FromCurrency: "USD", ToCurrency: "CNY", Rate: decimal.RequireFromString("0.5"), BufferRate: decimal.RequireFromString("0.5")}).Error)
	source := money.Amount{Nanos: 9_007_199_254_740_993, Currency: "USD"}
	request := CheckoutQuoteRequest{Provider: PaymentProviderEpay, PaymentMethod: "alipay", Selection: CheckoutSelection{Kind: "amount", SourceAmount: &source}}
	order, err := CreateCheckoutQuote(context.Background(), user.Id, request, nil, "", "", "", "")
	require.NoError(t, err)
	assert.Equal(t, source, order.SourceAmount)
	assert.Equal(t, money.Amount{Nanos: 4_503_599_627_370_496, Currency: "CNY"}, order.CreditAmount)
	assert.Equal(t, "0.5", order.Quote().FX.Rate)
	assert.Equal(t, "CNY", string(order.ChargeAmount.Currency))
	require.NoError(t, DB.Model(&FXRate{}).Where("from_currency = ? AND to_currency = ?", "USD", "CNY").Update("rate", "0.8").Error)
	_, accepted, claimed, err := AcceptCheckoutQuote(context.Background(), user.Id, PaymentProviderEpay, order.TradeNo)
	require.NoError(t, err)
	require.True(t, claimed)
	assert.Equal(t, order.CreditAmount, accepted.CreditAmount)
	receipt := CheckoutPayment{TradeNo: order.TradeNo, PaidAmount: order.ChargeAmount, ProviderOrderID: "external-paid"}
	require.NoError(t, ConfirmCheckoutPayment(context.Background(), PaymentProviderEpay, receipt))
	require.NoError(t, CompleteCheckoutPayment(context.Background(), order.TradeNo))
	assert.Equal(t, order.CreditAmount, paymentWalletBalance(t, user.Id))
	assert.Equal(t, common.MaxWalletQuota, getUserQuotaForPaymentGuardTest(t, user.Id))
	var row struct {
		Amount *int64
		Money  *float64
	}
	require.NoError(t, DB.Model(&TopUp{}).Select("amount", "money").Where("trade_no = ?", order.TradeNo).Scan(&row).Error)
	assert.Nil(t, row.Amount)
	assert.Nil(t, row.Money)
	var entry LedgerEntry
	require.NoError(t, DB.Where("type = ?", LedgerEntryTopUp).First(&entry).Error)
	assert.Equal(t, source, entry.SourceAmount)
	assert.Equal(t, "0.5", entry.FxRate.Decimal.String())
}

func TestCheckoutQuoteOwnershipAndConcurrentPaymentReplay(t *testing.T) {
	useCheckoutDB(t)
	user := insertUserForPaymentGuardTest(t, 8102, 17)
	source := money.Amount{Nanos: 1_000_000_001, Currency: "USD"}
	order, err := CreateCheckoutQuote(context.Background(), user.Id, CheckoutQuoteRequest{Provider: PaymentProviderEpay, PaymentMethod: "alipay", Selection: CheckoutSelection{Kind: "amount", SourceAmount: &source}}, nil, "", "", "", "")
	require.NoError(t, err)
	_, _, _, err = AcceptCheckoutQuote(context.Background(), user.Id+1, PaymentProviderEpay, order.TradeNo)
	require.Error(t, err)
	_, _, _, err = AcceptCheckoutQuote(context.Background(), user.Id, PaymentProviderStripe, order.TradeNo)
	require.ErrorIs(t, err, ErrPaymentMethodMismatch)
	claims := make(chan bool, 8)
	failures := make(chan error, 8)
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			_, _, claimed, err := AcceptCheckoutQuote(context.Background(), user.Id, PaymentProviderEpay, order.TradeNo)
			claims <- claimed
			failures <- err
		})
	}
	workers.Wait()
	close(claims)
	close(failures)
	count := 0
	for claimed := range claims {
		if claimed {
			count++
		}
	}
	assert.Equal(t, 1, count)
	for err := range failures {
		require.NoError(t, err)
	}
	paymentFailures := make(chan error, 8)
	for range 8 {
		workers.Go(func() {
			err := ConfirmCheckoutPayment(context.Background(), PaymentProviderEpay, CheckoutPayment{TradeNo: order.TradeNo, PaidAmount: order.ChargeAmount, ProviderOrderID: "provider-order"})
			if err == nil {
				err = CompleteCheckoutPayment(context.Background(), order.TradeNo)
			}
			paymentFailures <- err
		})
	}
	workers.Wait()
	close(paymentFailures)
	for err := range paymentFailures {
		require.NoError(t, err)
	}
	require.NoError(t, CompleteCheckoutPayment(context.Background(), order.TradeNo))
	assert.Equal(t, source, paymentWalletBalance(t, user.Id))
	var entries int64
	require.NoError(t, DB.Model(&LedgerEntry{}).Where("type = ?", LedgerEntryTopUp).Count(&entries).Error)
	assert.EqualValues(t, 1, entries)
	var receipts int64
	require.NoError(t, DB.Model(&CheckoutPayment{}).Count(&receipts).Error)
	assert.EqualValues(t, 1, receipts)
	assert.Equal(t, 17, getUserQuotaForPaymentGuardTest(t, user.Id))
}

func TestCheckoutVerifiedPaymentSurvivesFulfillmentFailure(t *testing.T) {
	useCheckoutDB(t)
	user := insertUserForPaymentGuardTest(t, 8103, 9)
	source := money.Amount{Nanos: 1_000_000_000, Currency: "USD"}
	order, err := CreateCheckoutQuote(context.Background(), user.Id, CheckoutQuoteRequest{Provider: PaymentProviderEpay, PaymentMethod: "alipay", Selection: CheckoutSelection{Kind: "amount", SourceAmount: &source}}, nil, "", "", "", "")
	require.NoError(t, err)
	_, _, _, err = AcceptCheckoutQuote(context.Background(), user.Id, PaymentProviderEpay, order.TradeNo)
	require.NoError(t, err)
	wrong := order.ChargeAmount
	wrong.Nanos++
	require.ErrorIs(t, ConfirmCheckoutPayment(context.Background(), PaymentProviderEpay, CheckoutPayment{TradeNo: order.TradeNo, PaidAmount: wrong, ProviderOrderID: "wrong-order"}), ErrPaymentMethodMismatch)
	require.NoError(t, ConfirmCheckoutPayment(context.Background(), PaymentProviderEpay, CheckoutPayment{TradeNo: order.TradeNo, PaidAmount: order.ChargeAmount, ProviderOrderID: "verified-order"}))
	const callback = "checkout-final-order-failure"
	require.NoError(t, DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "top_ups" {
			tx.AddError(errors.New("forced completion failure"))
		}
	}))
	err = CompleteCheckoutPayment(context.Background(), order.TradeNo)
	require.Error(t, err)
	require.NoError(t, DB.Callback().Update().Remove(callback))
	var receipt CheckoutPayment
	require.NoError(t, DB.First(&receipt, "trade_no = ?", order.TradeNo).Error)
	assert.Equal(t, order.ChargeAmount, receipt.PaidAmount)
	assert.Zero(t, paymentWalletBalance(t, user.Id).Nanos)
	assert.Equal(t, common.TopUpStatusPending, GetTopUpByTradeNo(order.TradeNo).Status)
	completed, err := RecoverCheckoutPayments(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, completed)
	assert.Equal(t, source, paymentWalletBalance(t, user.Id))
	assert.Error(t, DB.Model(&CheckoutPayment{}).Where("trade_no = ?", order.TradeNo).Update("paid_nanos", 1).Error)
	assert.Error(t, DB.Model(&CheckoutOrder{}).Where("trade_no = ?", order.TradeNo).Update("source_nanos", 1).Error)
}

func TestCheckoutProviderWireAmountsRejectOverflow(t *testing.T) {
	for _, tc := range []struct {
		provider, input string
		currency        money.Currency
		expected        int64
	}{
		{PaymentProviderEpay, "29.999", "CNY", 30_000_000_000},
		{PaymentProviderWaffo, "1.000000001", "JPY", 2_000_000_000},
		{PaymentProviderStripe, "1.001", "USD", 1_010_000_000},
	} {
		value, err := money.ParseValue(tc.input, tc.currency)
		require.NoError(t, err)
		amount, err := QuantizeCheckoutCharge(value, tc.provider)
		require.NoError(t, err)
		assert.Equal(t, tc.expected, amount.Nanos)
	}
	_, err := QuantizeCheckoutCharge(money.Amount{Nanos: math.MaxInt64, Currency: "USD"}.Value(), PaymentProviderEpay)
	require.ErrorIs(t, err, money.ErrOverflow)
}

func TestCheckoutConfigurationRejectsInvalidBeforeMutation(t *testing.T) {
	UseTestDatabases(t)
	require.NoError(t, DB.AutoMigrate(&Option{}))
	const key = "checkout_setting.config"
	common.OptionMapRWMutex.Lock()
	if common.OptionMap == nil {
		common.OptionMap = make(map[string]string)
	}
	previous, existed := common.OptionMap[key]
	common.OptionMap[key] = `{"providers":{}}`
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		defer common.OptionMapRWMutex.Unlock()
		if existed {
			common.OptionMap[key] = previous
		} else {
			delete(common.OptionMap, key)
		}
	})
	for _, input := range []string{
		`{"providers":{"epay":{"enabled":true}}}`,
		`{"providers":{"stripe":{"enabled":true,"products":[{"product_id":"price_test","name":"Test","source_amount":{"nanos":1,"currency":"USD"}}]}}}`,
		`{"providers":{"creem":{"enabled":false,"products":[],"quota":1}}}`,
		`{"providers":{},"unexpected":true}`,
		`null`,
	} {
		t.Run(input, func(t *testing.T) {
			require.Error(t, UpdateOption(key, input))
			var count int64
			require.NoError(t, DB.Model(&Option{}).Where("key = ?", key).Count(&count).Error)
			assert.Zero(t, count)
			common.OptionMapRWMutex.RLock()
			assert.Equal(t, `{"providers":{}}`, common.OptionMap[key])
			common.OptionMapRWMutex.RUnlock()
		})
	}
}

func TestCheckoutManualCompletionRequiresVerifiedPayment(t *testing.T) {
	useCheckoutDB(t)
	user := insertUserForPaymentGuardTest(t, 8201, 19)
	source := money.Amount{Nanos: 1_000_000_001, Currency: "USD"}
	order, err := CreateCheckoutQuote(context.Background(), user.Id, CheckoutQuoteRequest{Provider: PaymentProviderEpay, PaymentMethod: "alipay", Selection: CheckoutSelection{Kind: "amount", SourceAmount: &source}}, nil, "", "", "", "")
	require.NoError(t, err)
	require.Error(t, ManualCompleteTopUp(order.TradeNo, ""))
	_, _, _, err = AcceptCheckoutQuote(context.Background(), user.Id, PaymentProviderEpay, order.TradeNo)
	require.NoError(t, err)
	require.EqualError(t, ManualCompleteTopUp(order.TradeNo, ""), "Payment has not been confirmed")
	assert.Equal(t, common.TopUpStatusPending, GetTopUpByTradeNo(order.TradeNo).Status)
	assert.Zero(t, paymentWalletBalance(t, user.Id).Nanos)
	require.NoError(t, ConfirmCheckoutPayment(context.Background(), PaymentProviderEpay, CheckoutPayment{TradeNo: order.TradeNo, PaidAmount: order.ChargeAmount, ProviderOrderID: "manual-verified"}))
	require.NoError(t, ManualCompleteTopUp(order.TradeNo, ""))
	require.NoError(t, ManualCompleteTopUp(order.TradeNo, ""))
	assert.Equal(t, source, paymentWalletBalance(t, user.Id))
	assert.Equal(t, 19, getUserQuotaForPaymentGuardTest(t, user.Id))
}

func TestCheckoutExpiryAndZeroUnixAcceptance(t *testing.T) {
	useCheckoutDB(t)
	fake := clock.UseFake(t, time.Unix(0, 0))
	user := insertUserForPaymentGuardTest(t, 8202, 0)
	source := money.Amount{Nanos: 1_000_000_000, Currency: "USD"}
	request := CheckoutQuoteRequest{Provider: PaymentProviderEpay, PaymentMethod: "alipay", Selection: CheckoutSelection{Kind: "amount", SourceAmount: &source}}
	expired, err := CreateCheckoutQuote(context.Background(), user.Id, request, nil, "", "", "", "")
	require.NoError(t, err)
	accepted, err := CreateCheckoutQuote(context.Background(), user.Id, request, nil, "", "", "", "")
	require.NoError(t, err)
	_, _, claimed, err := AcceptCheckoutQuote(context.Background(), user.Id, PaymentProviderEpay, accepted.TradeNo)
	require.NoError(t, err)
	require.True(t, claimed)
	fake.Advance(15 * time.Minute)
	_, _, _, err = AcceptCheckoutQuote(context.Background(), user.Id, PaymentProviderEpay, expired.TradeNo)
	require.Error(t, err)
	_, _, claimed, err = AcceptCheckoutQuote(context.Background(), user.Id, PaymentProviderEpay, accepted.TradeNo)
	require.NoError(t, err)
	require.False(t, claimed)
	require.NoError(t, ConfirmCheckoutPayment(context.Background(), PaymentProviderEpay, CheckoutPayment{TradeNo: accepted.TradeNo, PaidAmount: accepted.ChargeAmount, ProviderOrderID: "zero-unix-paid"}))
	require.NoError(t, CompleteCheckoutPayment(context.Background(), accepted.TradeNo))
	assert.Equal(t, source, paymentWalletBalance(t, user.Id))
	assert.Error(t, DB.Model(&CheckoutOrder{}).Where("trade_no = ?", accepted.TradeNo).Update("accepted_at", 1).Error)
}

func TestCheckoutHistoryPreservesUnknownLegacyMoneyAndPurpose(t *testing.T) {
	useCheckoutDB(t)
	user := insertUserForPaymentGuardTest(t, 8203, 0)
	for _, row := range []TopUp{
		{UserId: user.Id, TradeNo: "legacy-large", Amount: 9_007_199_254_740_993, Money: 1.25, PaymentProvider: PaymentProviderStripe, Status: common.TopUpStatusSuccess},
		{UserId: user.Id, TradeNo: "zero-wallet", Amount: 0, Money: math.NaN(), PaymentProvider: PaymentProviderEpay, Status: common.TopUpStatusPending},
		{UserId: user.Id, TradeNo: "real-subscription", Amount: 7, Money: 2, PaymentProvider: PaymentProviderStripe, Status: common.TopUpStatusPending},
	} {
		require.NoError(t, DB.Create(&row).Error)
	}
	require.NoError(t, DB.Create(&SubscriptionOrder{TradeNo: "real-subscription", UserId: user.Id, PlanId: 1, Status: common.TopUpStatusSuccess, PaymentProvider: PaymentProviderStripe}).Error)
	assert.ErrorIs(t, ManualCompleteTopUp("real-subscription", ""), ErrLedgerInvalidEntry)
	source := money.Amount{Nanos: 1_000_000_001, Currency: "USD"}
	exact, err := CreateCheckoutQuote(context.Background(), user.Id, CheckoutQuoteRequest{Provider: PaymentProviderEpay, PaymentMethod: "alipay", Selection: CheckoutSelection{Kind: "amount", SourceAmount: &source}}, nil, "", "", "", "")
	require.NoError(t, err)
	_, _, _, err = AcceptCheckoutQuote(context.Background(), user.Id, PaymentProviderEpay, exact.TradeNo)
	require.NoError(t, err)
	require.NoError(t, ConfirmCheckoutPayment(context.Background(), PaymentProviderEpay, CheckoutPayment{TradeNo: exact.TradeNo, PaidAmount: exact.ChargeAmount, ProviderOrderID: "history-paid"}))
	rows, total, err := GetAllTopUps(&common.PageInfo{Page: 1, PageSize: 20})
	require.NoError(t, err)
	assert.EqualValues(t, 4, total)
	byTrade := map[string]*TopUp{}
	for _, row := range rows {
		byTrade[row.TradeNo] = row
	}
	assert.Equal(t, "9007199254740993", *byTrade["legacy-large"].LegacyAmount)
	assert.Equal(t, "1.25", *byTrade["legacy-large"].LegacyMoney)
	assert.Nil(t, byTrade["legacy-large"].Checkout)
	assert.Equal(t, "legacy_topup", byTrade["zero-wallet"].Purpose)
	assert.Equal(t, "NaN", *byTrade["zero-wallet"].LegacyMoney)
	assert.Equal(t, "subscription_history", byTrade["real-subscription"].Purpose)
	require.NotNil(t, byTrade[exact.TradeNo].Checkout)
	assert.Equal(t, &exact.ChargeAmount, byTrade[exact.TradeNo].Checkout.PaidAmount)
	assert.True(t, byTrade[exact.TradeNo].Checkout.FulfillmentPending)
	assert.Nil(t, byTrade[exact.TradeNo].LegacyMoney)
	encoded, err := common.Marshal(rows)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"legacy_amount":"9007199254740993"`)
	assert.Contains(t, string(encoded), `"money":null`)
	for index, search := range []func() ([]*TopUp, int64, error){
		func() ([]*TopUp, int64, error) {
			return SearchAllTopUps("legacy-large", &common.PageInfo{Page: 1, PageSize: 20})
		},
		func() ([]*TopUp, int64, error) {
			return SearchUserTopUps(user.Id, "CHECKOUT-%", &common.PageInfo{Page: 1, PageSize: 20})
		},
	} {
		result, _, err := search()
		require.NoError(t, err)
		require.NotEmpty(t, result, "history query index=%d", index)
	}
}

func TestCheckoutMigrationPreservesLegacyAndRunsTwice(t *testing.T) {
	for _, tc := range []struct {
		name, display, legacyDisplay string
		imported                     bool
	}{
		{"currency", "USD", "true", true},
		{"tokens", "TOKENS", "false", false},
		{"old token key", "", "false", false},
		{"conflicting display", "USD", "false", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			UseTestDatabases(t)
			require.NoError(t, DB.Exec(`CREATE TABLE "top_ups" ("id" bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,"user_id" bigint,"amount" bigint,"money" double precision,"trade_no" varchar(255) UNIQUE,"payment_method" varchar(50),"payment_provider" varchar(50) DEFAULT '',"create_time" bigint,"complete_time" bigint,"status" text)`).Error)
			require.NoError(t, DB.Exec(`INSERT INTO "top_ups" ("user_id","amount","money","trade_no","payment_method","payment_provider","status") VALUES (1,9007199254740993,17.125,'existing-pending','alipay','epay','pending'),(1,3,2.5,'existing-success','stripe','stripe','success')`).Error)
			require.NoError(t, DB.AutoMigrate(&Option{}))
			legacyOptions := []Option{
				{Key: "TopupGroupRatio", Value: `{"vip":1.000000000000000001}`},
				{Key: "payment_setting.amount_options", Value: `[11,23]`},
				{Key: "payment_setting.amount_discount", Value: `{"11":0.900000000000000001}`},
			}
			require.NoError(t, DB.Create(&legacyOptions).Error)
			for key, value := range map[string]string{"general_setting.quota_display_type": tc.display, "DisplayInCurrencyEnabled": tc.legacyDisplay, "WaffoUnitPrice": "7.300000001", "WaffoCurrency": "CNY", "WaffoMinTopUp": "1", "WaffoEnabled": "true", "StripePriceId": "price_old", "CreemProducts": `[{"productId":"prod_old","name":"Old","quota":500001}]`} {
				require.NoError(t, DB.Create(&Option{Key: key, Value: value}).Error)
			}
			require.NoError(t, migrateCheckout(DB))
			require.NoError(t, migrateCheckout(DB))
			var stored Option
			require.NoError(t, DB.First(&stored, "key = ?", operation_setting.CheckoutConfigOption).Error)
			config, err := operation_setting.ParseCheckoutConfig(stored.Value)
			require.NoError(t, err)
			waffo, found := config.Providers[PaymentProviderWaffo]
			assert.Equal(t, tc.imported, found)
			if tc.imported {
				assert.Equal(t, "1.000000000000000001", waffo.GroupRatios["vip"])
				assert.Equal(t, []money.Amount{{Nanos: 11_000_000_000, Currency: "USD"}, {Nanos: 23_000_000_000, Currency: "USD"}}, waffo.Presets)
				assert.Equal(t, []operation_setting.CheckoutDiscount{{SourceAmount: money.Amount{Nanos: 11_000_000_000, Currency: "USD"}, Ratio: "0.900000000000000001"}}, waffo.Discounts)
			}
			for _, option := range legacyOptions {
				var storedSource Option
				require.NoError(t, DB.First(&storedSource, "key = ?", option.Key).Error)
				assert.Equal(t, option, storedSource)
			}
			_, found = config.Providers[PaymentProviderEpay]
			assert.False(t, found, "unknown legacy cash currency must not be guessed")
			require.Len(t, config.Providers[PaymentProviderCreem].Products, 1)
			assert.EqualValues(t, 1_000_002_000, config.Providers[PaymentProviderCreem].Products[0].SourceAmount.Nanos)
			var legacy TopUp
			require.NoError(t, DB.First(&legacy, "trade_no = ?", "existing-pending").Error)
			assert.EqualValues(t, 9_007_199_254_740_993, legacy.Amount)
			assert.Equal(t, 17.125, legacy.Money)
			assert.Equal(t, common.TopUpStatusPending, legacy.Status)
			assert.Zero(t, legacy.ContractVersion)
			var exactCount int64
			require.NoError(t, DB.Model(&CheckoutOrder{}).Count(&exactCount).Error)
			assert.Zero(t, exactCount)
			assert.Error(t, DB.Create(&TopUp{TradeNo: "existing-pending"}).Error)
		})
	}
}

func TestCheckoutConfigurationStrictCollections(t *testing.T) {
	valid := `{"providers":{"epay":{"enabled":false,"source_unit":{"nanos":"1000000000","currency":"USD"},"charge_unit":{"nanos":"1000000000","currency":"CNY"},"minimum":{"nanos":"1","currency":"USD"}}}}`
	_, err := operation_setting.ParseCheckoutConfig(valid)
	require.NoError(t, err)
	for _, invalid := range []string{
		strings.Replace(valid, `"enabled":false`, `"enabled":null`, 1),
		strings.Replace(valid, `"enabled":false`, `"enabled":false,"presets":null`, 1),
		strings.Replace(valid, `"enabled":false`, `"enabled":false,"group_ratios":{"vip":"0"}`, 1),
		strings.Replace(valid, `"enabled":false`, `"enabled":false,"discounts":[{"source_amount":{"nanos":"1","currency":"USD"},"ratio":"1.01"}]`, 1),
		strings.Replace(valid, `"enabled":false`, `"enabled":false,"discounts":[{"source_amount":{"nanos":"1","currency":"USD"},"ratio":0.5}]`, 1),
		`{"providers":{"stripe":{"enabled":false,"products":[]}}}`,
		`{"providers":{"stripe":{"enabled":true,"products":[{"product_id":"p","name":"P","source_amount":{"nanos":"1","currency":"USD"}}],"discounts":[{"source_amount":{"nanos":"1","currency":"USD"},"ratio":"0.5"}]}}}`,
		`{"providers":{"creem":{"enabled":true,"products":[{"product_id":"p","name":"P","source_amount":{"nanos":"1","currency":"USD"}}],"group_ratios":{"vip":"1"}}}}`,
		`{"providers":{"epay":null}}`,
	} {
		_, err := operation_setting.ParseCheckoutConfig(invalid)
		assert.Error(t, err, invalid)
	}
}

func TestCheckoutPaidIdentityCannotMoveAndPendingCashRepaysDebt(t *testing.T) {
	useCheckoutDB(t)
	user := insertUserForPaymentGuardTest(t, 8301, 29)
	debt := money.Amount{Nanos: 2_000_000_000, Currency: "USD"}
	require.NoError(t, DB.Transaction(func(tx *gorm.DB) error {
		_, err := postLedgerEntries(tx, []LedgerPosting{
			{Account: UserReceivableAccount(user.Id), Type: LedgerEntryCharge, Amount: debt, RequestId: "checkout-old-debt"},
			{Account: PlatformRevenueAccount("USD"), Type: LedgerEntryCharge, Amount: debt, RequestId: "checkout-old-debt"},
		})
		return err
	}))
	source := money.Amount{Nanos: 1_000_000_001, Currency: "USD"}
	request := CheckoutQuoteRequest{Provider: PaymentProviderEpay, PaymentMethod: "alipay", Selection: CheckoutSelection{Kind: "amount", SourceAmount: &source}}
	first, err := CreateCheckoutQuote(context.Background(), user.Id, request, nil, "", "", "", "")
	require.NoError(t, err)
	second, err := CreateCheckoutQuote(context.Background(), user.Id, request, nil, "", "", "", "")
	require.NoError(t, err)
	for _, order := range []*CheckoutOrder{first, second} {
		_, _, _, err = AcceptCheckoutQuote(context.Background(), user.Id, PaymentProviderEpay, order.TradeNo)
		require.NoError(t, err)
	}
	require.NoError(t, MarkCheckoutCreationUncertain(context.Background(), first.TradeNo))
	receipt := CheckoutPayment{TradeNo: first.TradeNo, PaidAmount: first.ChargeAmount, ProviderOrderID: "one-external-payment"}
	require.NoError(t, ConfirmCheckoutPayment(context.Background(), PaymentProviderEpay, receipt))
	assert.ErrorIs(t, UpdatePendingTopUpStatus(first.TradeNo, PaymentProviderEpay, common.TopUpStatusFailed), ErrTopUpStatusInvalid)
	receipt.TradeNo = second.TradeNo
	assert.Error(t, ConfirmCheckoutPayment(context.Background(), PaymentProviderEpay, receipt), "one provider payment cannot fund a second accepted quote")
	assert.Error(t, DB.Model(&TopUp{}).Where("trade_no = ?", first.TradeNo).Update("user_id", user.Id+1).Error)
	assert.Error(t, DB.Model(&TopUp{}).Where("trade_no = ?", first.TradeNo).Update("payment_provider", PaymentProviderStripe).Error)
	require.NoError(t, CompleteCheckoutPayment(context.Background(), first.TradeNo))
	wallet, err := GetUserWallet(user.Id)
	require.NoError(t, err)
	require.NotNil(t, wallet)
	assert.Zero(t, wallet.Balance.Nanos)
	assert.True(t, wallet.RequestsPaused)
	var receivable Wallet
	require.NoError(t, DB.Where("account_type = ? AND owner_id = ?", LedgerAccountUserReceivable, user.Id).First(&receivable).Error)
	assert.EqualValues(t, 999_999_999, receivable.Balance.Nanos)
	var revenue Wallet
	require.NoError(t, DB.Where("account_type = ?", LedgerAccountPlatformRevenue).First(&revenue).Error)
	assert.Equal(t, debt, revenue.Balance)
	receipt.ProviderOrderID = "second-external-payment"
	require.NoError(t, ConfirmCheckoutPayment(context.Background(), PaymentProviderEpay, receipt))
	require.NoError(t, CompleteCheckoutPayment(context.Background(), second.TradeNo))
	wallet, err = GetUserWallet(user.Id)
	require.NoError(t, err)
	assert.EqualValues(t, 2, wallet.Balance.Nanos)
	assert.False(t, wallet.RequestsPaused)
	reconciliation, err := ReconcileWallets(100)
	require.NoError(t, err)
	assert.Empty(t, reconciliation.Mismatches)
	assert.Equal(t, 29, getUserQuotaForPaymentGuardTest(t, user.Id))
}

func TestCheckoutRecoverySkipsOverflowWithoutLosingPaidReceipt(t *testing.T) {
	useCheckoutDB(t)
	fake := clock.UseFake(t, time.Unix(100, 0))
	blocked := insertUserForPaymentGuardTest(t, 8302, 31)
	ready := &User{Id: 8303, Username: "checkout-ready-user", Status: common.UserStatusEnabled, Quota: 37, AffCode: "ready"}
	require.NoError(t, DB.Create(ready).Error)
	_, err := PostLedgerEntries(LedgerPosting{Account: UserWalletAccount(blocked.Id), Type: LedgerEntryOpeningBalance, Amount: money.Amount{Nanos: math.MaxInt64, Currency: "USD"}})
	require.NoError(t, err)
	source := money.Amount{Nanos: 1, Currency: "USD"}
	var orders []*CheckoutOrder
	for _, user := range []*User{blocked, ready} {
		order, err := CreateCheckoutQuote(context.Background(), user.Id, CheckoutQuoteRequest{Provider: PaymentProviderEpay, PaymentMethod: "alipay", Selection: CheckoutSelection{Kind: "amount", SourceAmount: &source}}, nil, "", "", "", "")
		require.NoError(t, err)
		_, _, _, err = AcceptCheckoutQuote(context.Background(), user.Id, PaymentProviderEpay, order.TradeNo)
		require.NoError(t, err)
		require.NoError(t, ConfirmCheckoutPayment(context.Background(), PaymentProviderEpay, CheckoutPayment{TradeNo: order.TradeNo, PaidAmount: order.ChargeAmount, ProviderOrderID: "paid-" + order.TradeNo}))
		orders = append(orders, order)
		fake.Advance(time.Second)
	}
	completed, err := RecoverCheckoutPayments(context.Background(), 1)
	require.ErrorIs(t, err, money.ErrOverflow)
	assert.Zero(t, completed)
	assert.EqualValues(t, math.MaxInt64, paymentWalletBalance(t, blocked.Id).Nanos)
	assert.Equal(t, common.TopUpStatusPending, GetTopUpByTradeNo(orders[0].TradeNo).Status)
	var receipt CheckoutPayment
	require.NoError(t, DB.First(&receipt, "trade_no = ?", orders[0].TradeNo).Error)
	assert.Equal(t, orders[0].ChargeAmount, receipt.PaidAmount)
	completed, err = RecoverCheckoutPayments(context.Background(), 1)
	require.NoError(t, err)
	assert.Equal(t, 1, completed, "an older unfulfillable paid order must not starve the next receipt")
	assert.Equal(t, source, paymentWalletBalance(t, ready.Id))
	assert.Equal(t, 31, getUserQuotaForPaymentGuardTest(t, blocked.Id))
	assert.Equal(t, 37, getUserQuotaForPaymentGuardTest(t, ready.Id))
}
