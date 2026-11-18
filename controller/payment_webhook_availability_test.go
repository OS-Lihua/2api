package controller

import (
	"context"
	"errors"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/webhook"
	waffoutils "github.com/waffo-com/waffo-go/utils"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Calcium-Ion/go-epay/epay"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func confirmPaymentComplianceForTest(t *testing.T) {
	t.Helper()
	paymentSetting := operation_setting.GetPaymentSetting()
	originalConfirmed := paymentSetting.ComplianceConfirmed
	originalTermsVersion := paymentSetting.ComplianceTermsVersion
	t.Cleanup(func() {
		paymentSetting.ComplianceConfirmed = originalConfirmed
		paymentSetting.ComplianceTermsVersion = originalTermsVersion
	})
	paymentSetting.ComplianceConfirmed = true
	paymentSetting.ComplianceTermsVersion = operation_setting.CurrentComplianceTermsVersion
}

func TestStripeWebhookEnabledRequiresTopUpAndWebhookConfig(t *testing.T) {
	confirmPaymentComplianceForTest(t)
	originalAPISecret := setting.StripeApiSecret
	originalWebhookSecret := setting.StripeWebhookSecret
	originalPriceID := setting.StripePriceId
	t.Cleanup(func() {
		setting.StripeApiSecret = originalAPISecret
		setting.StripeWebhookSecret = originalWebhookSecret
		setting.StripePriceId = originalPriceID
	})

	setting.StripeWebhookSecret = ""
	setting.StripeApiSecret = "sk_test_123"
	setting.StripePriceId = "price_123"
	require.False(t, isStripeWebhookEnabled())

	setting.StripeWebhookSecret = "whsec_test"
	require.True(t, isStripeWebhookEnabled())

	setting.StripePriceId = ""
	require.True(t, isStripeWebhookEnabled(), "accepted payments remain verifiable without a live price")
	setting.StripeApiSecret = ""
	require.True(t, isStripeWebhookEnabled())
}

func TestCreemWebhookEnabledRequiresTopUpAndWebhookConfig(t *testing.T) {
	confirmPaymentComplianceForTest(t)
	originalAPIKey := setting.CreemApiKey
	originalProducts := setting.CreemProducts
	originalWebhookSecret := setting.CreemWebhookSecret
	t.Cleanup(func() {
		setting.CreemApiKey = originalAPIKey
		setting.CreemProducts = originalProducts
		setting.CreemWebhookSecret = originalWebhookSecret
	})

	setting.CreemWebhookSecret = ""
	setting.CreemApiKey = "creem_api_key"
	setting.CreemProducts = `[{"productId":"prod_123"}]`
	require.False(t, isCreemWebhookEnabled())

	setting.CreemWebhookSecret = "creem_secret"
	require.True(t, isCreemWebhookEnabled())

	setting.CreemProducts = "[]"
	require.True(t, isCreemWebhookEnabled(), "accepted payments remain verifiable without live products")
	setting.CreemApiKey = ""
	require.True(t, isCreemWebhookEnabled())
}

func TestWaffoWebhookEnabledRequiresTopUpAndWebhookConfig(t *testing.T) {
	confirmPaymentComplianceForTest(t)
	originalEnabled := setting.WaffoEnabled
	originalSandbox := setting.WaffoSandbox
	originalAPIKey := setting.WaffoApiKey
	originalPrivateKey := setting.WaffoPrivateKey
	originalPublicCert := setting.WaffoPublicCert
	originalSandboxAPIKey := setting.WaffoSandboxApiKey
	originalSandboxPrivateKey := setting.WaffoSandboxPrivateKey
	originalSandboxPublicCert := setting.WaffoSandboxPublicCert
	t.Cleanup(func() {
		setting.WaffoEnabled = originalEnabled
		setting.WaffoSandbox = originalSandbox
		setting.WaffoApiKey = originalAPIKey
		setting.WaffoPrivateKey = originalPrivateKey
		setting.WaffoPublicCert = originalPublicCert
		setting.WaffoSandboxApiKey = originalSandboxAPIKey
		setting.WaffoSandboxPrivateKey = originalSandboxPrivateKey
		setting.WaffoSandboxPublicCert = originalSandboxPublicCert
	})

	setting.WaffoEnabled = true
	setting.WaffoSandbox = false
	setting.WaffoApiKey = ""
	setting.WaffoPrivateKey = "private"
	setting.WaffoPublicCert = "public"
	require.False(t, isWaffoWebhookEnabled())

	setting.WaffoApiKey = "api"
	require.True(t, isWaffoWebhookEnabled())

	setting.WaffoEnabled = false
	require.True(t, isWaffoWebhookEnabled(), "creation enablement never disables accepted callbacks")

	setting.WaffoEnabled = true
	setting.WaffoSandbox = true
	setting.WaffoSandboxApiKey = ""
	setting.WaffoSandboxPrivateKey = "sandbox_private"
	setting.WaffoSandboxPublicCert = "sandbox_public"
	require.False(t, isWaffoWebhookEnabled())

	setting.WaffoSandboxApiKey = "sandbox_api"
	require.True(t, isWaffoWebhookEnabled())
}

func TestWaffoPancakeWebhookEnabledRequiresTopUpAndWebhookConfig(t *testing.T) {
	confirmPaymentComplianceForTest(t)
	originalMerchantID := setting.WaffoPancakeMerchantID
	originalPrivateKey := setting.WaffoPancakePrivateKey
	originalProductID := setting.WaffoPancakeProductID
	t.Cleanup(func() {
		setting.WaffoPancakeMerchantID = originalMerchantID
		setting.WaffoPancakePrivateKey = originalPrivateKey
		setting.WaffoPancakeProductID = originalProductID
	})

	// Verification uses bundled public keys and the accepted order store.
	// Removing creation credentials must not strand an accepted payment.
	setting.WaffoPancakeMerchantID = ""
	setting.WaffoPancakePrivateKey = "private"
	setting.WaffoPancakeProductID = "product"
	require.True(t, isWaffoPancakeWebhookEnabled(), "bundled public keys verify accepted orders independently of creation credentials")

	setting.WaffoPancakeMerchantID = "merchant"
	require.True(t, isWaffoPancakeWebhookEnabled())

	setting.WaffoPancakeProductID = ""
	require.True(t, isWaffoPancakeWebhookEnabled(), "bundled public keys verify accepted orders independently of creation credentials")

	setting.WaffoPancakeProductID = "product"
	setting.WaffoPancakePrivateKey = ""
	require.True(t, isWaffoPancakeWebhookEnabled(), "bundled public keys verify accepted orders independently of creation credentials")
}

func TestEpayWebhookEnabledRequiresTopUpAndWebhookConfig(t *testing.T) {
	confirmPaymentComplianceForTest(t)
	originalPayAddress := operation_setting.PayAddress
	originalEpayID := operation_setting.EpayId
	originalEpayKey := operation_setting.EpayKey
	originalPayMethods := operation_setting.PayMethods
	t.Cleanup(func() {
		operation_setting.PayAddress = originalPayAddress
		operation_setting.EpayId = originalEpayID
		operation_setting.EpayKey = originalEpayKey
		operation_setting.PayMethods = originalPayMethods
	})

	operation_setting.PayAddress = "https://pay.example.com"
	operation_setting.EpayId = "epay_id"
	operation_setting.EpayKey = ""
	operation_setting.PayMethods = []map[string]string{{"type": "alipay"}}
	require.False(t, isEpayWebhookEnabled())

	operation_setting.EpayKey = "epay_key"
	require.True(t, isEpayWebhookEnabled())

	operation_setting.PayMethods = nil
	require.True(t, isEpayWebhookEnabled(), "removing offered methods never disables accepted callbacks")
}

func TestEpayCallbacksVerifyBeforeSettlement(t *testing.T) {
	confirmPaymentComplianceForTest(t)
	originalPayAddress := operation_setting.PayAddress
	originalEpayID := operation_setting.EpayId
	originalEpayKey := operation_setting.EpayKey
	originalPayMethods := operation_setting.PayMethods
	originalServerAddress := system_setting.ServerAddress
	originalDB := model.DB
	t.Cleanup(func() {
		operation_setting.PayAddress = originalPayAddress
		operation_setting.EpayId = originalEpayID
		operation_setting.EpayKey = originalEpayKey
		operation_setting.PayMethods = originalPayMethods
		system_setting.ServerAddress = originalServerAddress
		model.DB = originalDB
	})
	operation_setting.PayAddress = "https://pay.example.com"
	operation_setting.EpayId = "epay_id"
	operation_setting.EpayKey = "test-key"
	operation_setting.PayMethods = []map[string]string{{"type": "alipay"}}
	system_setting.ServerAddress = "https://dashboard.example.com"
	// Rejected callbacks and valid non-success events must never reach settlement.
	// A nil database makes an accidental attempt to settle fail the test.
	model.DB = nil
	require.True(t, isEpayWebhookEnabled())

	// Both ambiguous maps below had this same valid signing string in v0.0.4.
	// This must fail because of parameter ambiguity, not a mismatched signature.
	ambiguousSign := epay.MD5String("money=1.00&name=item&out_trade_no=other&out_trade_no=order&pid=epay_id&trade_status=TRADE_SUCCESS&type=alipay", "test-key")
	pendingSign := epay.MD5String("money=1.00&name=item%26other&out_trade_no=order&pid=epay_id&trade_status=WAIT_BUYER_PAY&type=alipay", "test-key")
	cases := []struct {
		name         string
		productName  string
		tradeNo      string
		status       string
		sign         string
		validPending bool
	}{
		{"separator in product name", "item&out_trade_no=other", "order", epay.StatusTradeSuccess, ambiguousSign, false},
		{"separator in order number", "item", "other&out_trade_no=order", epay.StatusTradeSuccess, ambiguousSign, false},
		{"invalid signature", "item", "order", epay.StatusTradeSuccess, "invalid", false},
		{"missing signature", "item", "order", epay.StatusTradeSuccess, "", false},
		{"valid pending with literal percent escape", "item%26other", "order", "WAIT_BUYER_PAY", pendingSign, true},
	}
	endpoints := []struct {
		name             string
		path             string
		handler          gin.HandlerFunc
		status           int
		rejectedBody     string
		pendingBody      string
		rejectedLocation string
		pendingLocation  string
	}{
		{
			name: "topup notify", path: "/api/user/epay/notify", handler: EpayNotify,
			status: http.StatusOK, rejectedBody: "fail", pendingBody: "success",
		},
		{
			name: "subscription notify", path: "/api/subscription/epay/notify", handler: SubscriptionEpayNotify,
			status: http.StatusOK, rejectedBody: "fail", pendingBody: "fail",
		},
		{
			name: "subscription return", path: "/api/subscription/epay/return", handler: SubscriptionEpayReturn,
			status:           http.StatusFound,
			rejectedLocation: "https://dashboard.example.com/wallet?pay=fail",
			pendingLocation:  "https://dashboard.example.com/wallet?pay=pending",
		},
	}
	router := gin.New()
	for _, endpoint := range endpoints {
		router.Any(endpoint.path, endpoint.handler)
	}
	for _, endpoint := range endpoints {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			for _, tc := range cases {
				t.Run(endpoint.name+"/"+method+"/"+tc.name, func(t *testing.T) {
					params := url.Values{
						"pid": {"epay_id"}, "type": {"alipay"}, "money": {"1.00"},
						"name": {tc.productName}, "out_trade_no": {tc.tradeNo},
						"trade_status": {tc.status}, "sign_type": {"MD5"},
					}
					if tc.sign != "" {
						params.Set("sign", tc.sign)
					}
					request := httptest.NewRequest(method, endpoint.path+"?"+params.Encode(), nil)
					if method == http.MethodPost {
						request = httptest.NewRequest(method, endpoint.path, strings.NewReader(params.Encode()))
						request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
					}
					recorder := httptest.NewRecorder()
					require.NotPanics(t, func() { router.ServeHTTP(recorder, request) })
					assert.Equal(t, endpoint.status, recorder.Code)
					wantBody, wantLocation := endpoint.rejectedBody, endpoint.rejectedLocation
					if tc.validPending {
						wantBody, wantLocation = endpoint.pendingBody, endpoint.pendingLocation
					}
					if wantBody != "" {
						assert.Equal(t, wantBody, recorder.Body.String())
					}
					assert.Equal(t, wantLocation, recorder.Header().Get("Location"))
				})
			}
		}
	}
}

func TestCheckoutStripeSignedPaidReceiptSurvivesWalletFailure(t *testing.T) {
	useCheckoutControllerDB(t)
	originalSecret, originalPrice, originalKey := setting.StripeWebhookSecret, setting.StripePriceId, setting.StripeApiSecret
	setting.StripeWebhookSecret, setting.StripePriceId, setting.StripeApiSecret = "whsec_checkout_fixture", "", ""
	t.Cleanup(func() {
		setting.StripeWebhookSecret, setting.StripePriceId, setting.StripeApiSecret = originalSecret, originalPrice, originalKey
	})
	require.NoError(t, operation_setting.ApplyCheckoutOption(operation_setting.CheckoutConfigOption, `{"providers":{"stripe":{"enabled":true,"products":[{"product_id":"price_paid","name":"Reward","source_amount":{"nanos":"1000000001","currency":"USD"}}]}}}`))
	order, err := model.CreateCheckoutQuote(context.Background(), 42, model.CheckoutQuoteRequest{Provider: "stripe", PaymentMethod: "stripe", Selection: model.CheckoutSelection{Kind: "product", ProductID: "price_paid", Quantity: "1"}}, &model.CheckoutExternalPrice{ProductID: "price_paid", UnitAmount: money.Amount{Nanos: 1_230_000_000, Currency: "EUR"}, Final: false, Mode: "test"}, "", "", "", "")
	require.NoError(t, err)
	_, _, _, err = model.AcceptCheckoutQuote(context.Background(), 42, "stripe", order.TradeNo)
	require.NoError(t, err)
	require.NoError(t, model.SaveCheckoutRemoteIdentity(context.Background(), order.TradeNo, "cs_paid", "https://checkout.stripe.com/fixture"))
	require.NoError(t, operation_setting.ApplyCheckoutOption(operation_setting.CheckoutConfigOption, `{"providers":{}}`))
	payload := map[string]any{"id": "cs_paid", "object": "checkout.session", "client_reference_id": order.TradeNo, "status": "complete", "payment_status": "paid", "currency": "eur", "amount_total": int64(100), "livemode": false, "customer": "cus_paid", "metadata": map[string]string{"checkout_reference": order.TradeNo, "checkout_price": "price_paid", "checkout_quantity": "1"}}
	send := func(object map[string]any, validSignature bool) *httptest.ResponseRecorder {
		body, err := common.Marshal(map[string]any{"id": "evt_paid", "object": "event", "type": "checkout.session.completed", "api_version": stripe.APIVersion, "data": map[string]any{"object": object}})
		require.NoError(t, err)
		signature := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: body, Secret: setting.StripeWebhookSecret}).Header
		if !validSignature {
			signature = "invalid"
		}
		return checkoutRequest(t, string(body), func(c *gin.Context) { c.Request.Header.Set("Stripe-Signature", signature); StripeWebhook(c) })
	}
	assert.Equal(t, http.StatusBadRequest, send(payload, false).Code)
	for _, field := range []string{"amount_total", "currency", "id", "livemode", "metadata"} {
		wrong := make(map[string]any, len(payload))
		for k, v := range payload {
			wrong[k] = v
		}
		switch field {
		case "amount_total":
			delete(wrong, field)
		case "currency":
			wrong[field] = "usd"
		case "id":
			wrong[field] = "cs_other"
		case "livemode":
			wrong[field] = true
		case "metadata":
			wrong[field] = map[string]string{"checkout_reference": order.TradeNo, "checkout_price": "price_other", "checkout_quantity": "1"}
		}
		assert.Equal(t, http.StatusInternalServerError, send(wrong, true).Code, field)
	}
	var receipts int64
	require.NoError(t, model.DB.Model(&model.CheckoutPayment{}).Count(&receipts).Error)
	assert.Zero(t, receipts)
	const callback = "checkout-customer-failure"
	require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "users" {
			tx.AddError(errors.New("forced customer update failure"))
		}
	}))
	response := send(payload, true)
	require.NoError(t, model.DB.Callback().Update().Remove(callback))
	assert.Equal(t, http.StatusInternalServerError, response.Code)
	var receipt model.CheckoutPayment
	require.NoError(t, model.DB.First(&receipt, "trade_no = ?", order.TradeNo).Error)
	assert.Equal(t, money.Amount{Nanos: 1_000_000_000, Currency: "EUR"}, receipt.PaidAmount, "promotion changes cash while promised credit remains frozen")
	assert.Equal(t, common.TopUpStatusPending, model.GetTopUpByTradeNo(order.TradeNo).Status)
	var entries int64
	require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Count(&entries).Error)
	assert.Zero(t, entries)
	require.NoError(t, model.ManualCompleteTopUp(order.TradeNo, ""))
	assert.Equal(t, http.StatusOK, send(payload, true).Code)
	wallet, err := model.GetUserWallet(42)
	require.NoError(t, err)
	require.NotNil(t, wallet)
	assert.Equal(t, order.CreditAmount, wallet.Balance)
	var user model.User
	require.NoError(t, model.DB.First(&user, 42).Error)
	assert.Equal(t, "cus_paid", user.StripeCustomer)
	require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("type = ?", model.LedgerEntryTopUp).Count(&entries).Error)
	assert.EqualValues(t, 1, entries)
}

func TestCheckoutCreemUsesSignedAmountPaidWithTax(t *testing.T) {
	useCheckoutControllerDB(t)
	oldSecret, oldProducts, oldKey := setting.CreemWebhookSecret, setting.CreemProducts, setting.CreemApiKey
	setting.CreemWebhookSecret, setting.CreemProducts, setting.CreemApiKey = "creem_webhook_fixture", "[]", ""
	t.Cleanup(func() {
		setting.CreemWebhookSecret, setting.CreemProducts, setting.CreemApiKey = oldSecret, oldProducts, oldKey
	})
	require.NoError(t, operation_setting.ApplyCheckoutOption(operation_setting.CheckoutConfigOption, `{"providers":{"creem":{"enabled":true,"products":[{"product_id":"prod_paid","name":"Reward","source_amount":{"nanos":"1000000001","currency":"USD"}}]}}}`))
	order, err := model.CreateCheckoutQuote(context.Background(), 42, model.CheckoutQuoteRequest{Provider: "creem", PaymentMethod: "creem", Selection: model.CheckoutSelection{Kind: "product", ProductID: "prod_paid", Quantity: "1"}}, &model.CheckoutExternalPrice{ProductID: "prod_paid", UnitAmount: money.Amount{Nanos: 10_000_000_000, Currency: "EUR"}, Final: false, Mode: "test"}, "", "", "", "")
	require.NoError(t, err)
	_, _, _, err = model.AcceptCheckoutQuote(context.Background(), 42, "creem", order.TradeNo)
	require.NoError(t, err)
	require.NoError(t, model.SaveCheckoutRemoteIdentity(context.Background(), order.TradeNo, "ch_paid", "https://checkout.creem.io/fixture"))
	object := map[string]any{"id": "ch_paid", "request_id": order.TradeNo, "mode": "test", "units": 1, "order": map[string]any{"id": "ord_paid", "status": "paid", "type": "onetime", "mode": "test", "currency": "EUR", "amount_paid": 1210, "sub_total": 1000, "tax_amount": 210, "product": "prod_paid"}, "product": map[string]any{"id": "prod_paid"}, "customer": map[string]any{"email": "paid@example.com"}}
	send := func(object map[string]any, signature bool) *httptest.ResponseRecorder {
		body, err := common.Marshal(map[string]any{"id": "evt_creem", "eventType": "checkout.completed", "object": object})
		require.NoError(t, err)
		signed := generateCreemSignature(string(body), setting.CreemWebhookSecret)
		if !signature {
			signed = "invalid"
		}
		return checkoutRequest(t, string(body), func(c *gin.Context) { c.Request.Header.Set(CreemSignatureHeader, signed); CreemWebhook(c) })
	}
	assert.Equal(t, http.StatusUnauthorized, send(object, false).Code)
	paid := object["order"].(map[string]any)
	delete(paid, "amount_paid")
	assert.Equal(t, http.StatusBadRequest, send(object, true).Code)
	paid["amount_paid"] = 1210
	object["units"] = 2
	assert.Equal(t, http.StatusBadRequest, send(object, true).Code)
	object["units"] = 1
	for _, invalid := range []struct {
		fields map[string]any
		key    string
		value  any
		status int
	}{
		{object, "id", "ch_other", http.StatusInternalServerError},
		{object, "mode", "prod", http.StatusBadRequest},
		{paid, "product", "prod_other", http.StatusBadRequest},
		{paid, "mode", "prod", http.StatusBadRequest},
		{paid, "currency", "USD", http.StatusInternalServerError},
		{paid, "amount_paid", int64(-1), http.StatusInternalServerError},
		{paid, "amount_paid", "1210", http.StatusBadRequest},
	} {
		previous := invalid.fields[invalid.key]
		invalid.fields[invalid.key] = invalid.value
		assert.Equal(t, invalid.status, send(object, true).Code, invalid.key)
		invalid.fields[invalid.key] = previous
	}
	paid["status"] = "pending"
	assert.Equal(t, http.StatusOK, send(object, true).Code)
	var receipts int64
	require.NoError(t, model.DB.Model(&model.CheckoutPayment{}).Count(&receipts).Error)
	assert.Zero(t, receipts, "invalid or unpaid tax-inclusive notifications must not become paid facts")
	paid["status"] = "paid"
	assert.Equal(t, http.StatusOK, send(object, true).Code)
	assert.Equal(t, http.StatusOK, send(object, true).Code)
	paid["amount_paid"] = 1211
	assert.Equal(t, http.StatusInternalServerError, send(object, true).Code, "a replay cannot change the verified tax-inclusive paid amount")
	var receipt model.CheckoutPayment
	require.NoError(t, model.DB.First(&receipt, "trade_no = ?", order.TradeNo).Error)
	assert.Equal(t, money.Amount{Nanos: 12_100_000_000, Currency: "EUR"}, receipt.PaidAmount)
	wallet, err := model.GetUserWallet(42)
	require.NoError(t, err)
	require.NotNil(t, wallet)
	assert.Equal(t, order.CreditAmount, wallet.Balance)
	var user model.User
	require.NoError(t, model.DB.First(&user, 42).Error)
	assert.Equal(t, "paid@example.com", user.Email)
	var topupLogs int64
	require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("user_id = ? AND type = ?", 42, model.LogTypeTopup).Count(&topupLogs).Error)
	assert.EqualValues(t, 1, topupLogs, "verified fulfillment records one top-up log even after callback replay")
}

func TestCheckoutWaffoSignedCallbackMatchesFrozenMoneyAndBuyer(t *testing.T) {
	useCheckoutControllerDB(t)
	oldSandbox, oldEnabled := setting.WaffoSandbox, setting.WaffoEnabled
	oldKey, oldPrivate, oldPublic, oldMerchant := setting.WaffoApiKey, setting.WaffoPrivateKey, setting.WaffoPublicCert, setting.WaffoMerchantId
	t.Cleanup(func() {
		setting.WaffoSandbox, setting.WaffoEnabled = oldSandbox, oldEnabled
		setting.WaffoApiKey, setting.WaffoPrivateKey, setting.WaffoPublicCert, setting.WaffoMerchantId = oldKey, oldPrivate, oldPublic, oldMerchant
	})
	keys, err := waffoutils.GenerateKeyPair()
	require.NoError(t, err)
	setting.WaffoSandbox, setting.WaffoEnabled = false, false
	setting.WaffoApiKey, setting.WaffoPrivateKey, setting.WaffoPublicCert, setting.WaffoMerchantId = "waffo_fixture", keys.PrivateKey, keys.PublicKey, "waffo_merchant"
	require.NoError(t, operation_setting.ApplyCheckoutOption(operation_setting.CheckoutConfigOption, `{"providers":{"waffo":{"enabled":true,"source_unit":{"nanos":"1000000000","currency":"USD"},"charge_unit":{"nanos":"7300000000","currency":"CNY"},"minimum":{"nanos":"1","currency":"USD"}}}}`))
	quote := checkoutQuoteForTest(t, `{"provider":"waffo","selection":{"kind":"amount","source_amount":{"nanos":"1000000001","currency":"USD"}}}`)
	_, _, _, err = model.AcceptCheckoutQuote(context.Background(), 42, "waffo", quote.QuoteID)
	require.NoError(t, err)
	require.NoError(t, model.SaveCheckoutRemoteIdentity(context.Background(), quote.QuoteID, "acquiring_paid", "https://checkout.waffo.com/fixture"))
	result := map[string]any{"merchantOrderId": quote.QuoteID, "paymentRequestId": quote.QuoteID, "acquiringOrderId": "acquiring_paid", "orderStatus": "PAY_SUCCESS", "orderCurrency": "CNY", "orderAmount": "7.31", "merchantInfo": map[string]string{"merchantId": "waffo_merchant"}, "userInfo": map[string]string{"userId": "42"}}
	send := func(valid bool) *httptest.ResponseRecorder {
		body, err := common.Marshal(map[string]any{"eventType": "PAYMENT_NOTIFICATION", "result": result})
		require.NoError(t, err)
		signature, err := waffoutils.Sign(string(body), keys.PrivateKey)
		require.NoError(t, err)
		if !valid {
			signature = "invalid"
		}
		return checkoutRequest(t, string(body), func(c *gin.Context) { c.Request.Header.Set("X-SIGNATURE", signature); WaffoWebhook(c) })
	}
	assert.Equal(t, http.StatusBadRequest, send(false).Code)
	result["orderAmount"] = "7.30"
	send(true)
	var count int64
	require.NoError(t, model.DB.Model(&model.CheckoutPayment{}).Count(&count).Error)
	assert.Zero(t, count)
	result["orderAmount"] = "7.31"
	result["userInfo"] = map[string]string{"userId": "43"}
	send(true)
	require.NoError(t, model.DB.Model(&model.CheckoutPayment{}).Count(&count).Error)
	assert.Zero(t, count)
	result["userInfo"] = map[string]string{"userId": "42"}
	send(true)
	send(true)
	require.NoError(t, model.DB.Model(&model.CheckoutPayment{}).Count(&count).Error)
	assert.EqualValues(t, 1, count)
	wallet, err := model.GetUserWallet(42)
	require.NoError(t, err)
	require.NotNil(t, wallet)
	assert.Equal(t, quote.CreditAmount, wallet.Balance)
}
