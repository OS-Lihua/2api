package controller

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Calcium-Ion/go-epay/epay"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func useCheckoutControllerDB(t *testing.T) {
	t.Helper()
	confirmPaymentComplianceForTest(t)
	db := model.UseTestDatabases(t)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.TopUp{}, &model.CheckoutOrder{}, &model.CheckoutPayment{}, &model.Wallet{}, &model.LedgerEntry{}, &model.SubscriptionOrder{}, &model.SubscriptionPlan{}, &model.UserSubscription{}, &model.Log{}, &model.FXRate{}))
	require.NoError(t, db.Create(&model.User{Id: 42, Username: "checkout-user", Status: common.UserStatusEnabled, Quota: common.MaxWalletQuota, Group: "vip"}).Error)
	config := operation_setting.GetCheckoutConfig()
	originalAddress, originalID, originalKey, originalMethods := operation_setting.PayAddress, operation_setting.EpayId, operation_setting.EpayKey, operation_setting.PayMethods
	originalServer, originalRedis := system_setting.ServerAddress, common.RedisEnabled
	common.RedisEnabled = false
	operation_setting.PayAddress, operation_setting.EpayId, operation_setting.EpayKey = "https://pay.example.com", "checkout-merchant", "checkout-fixture-key"
	operation_setting.PayMethods = []map[string]string{{"type": "alipay", "name": "Alipay"}}
	system_setting.ServerAddress = "https://console.example.com"
	t.Cleanup(func() {
		encoded, err := common.Marshal(config)
		require.NoError(t, err)
		require.NoError(t, operation_setting.ApplyCheckoutOption(operation_setting.CheckoutConfigOption, string(encoded)))
		operation_setting.PayAddress, operation_setting.EpayId, operation_setting.EpayKey, operation_setting.PayMethods = originalAddress, originalID, originalKey, originalMethods
		system_setting.ServerAddress, common.RedisEnabled = originalServer, originalRedis
	})
	require.NoError(t, operation_setting.ApplyCheckoutOption(operation_setting.CheckoutConfigOption, `{"providers":{"epay":{"enabled":true,"source_unit":{"nanos":"1000000000","currency":"USD"},"charge_unit":{"nanos":"7300000000","currency":"CNY"},"minimum":{"nanos":"1","currency":"USD"}}}}`))
}

func checkoutRequest(t *testing.T, body string, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Set("id", 42)
	c.Request = httptest.NewRequest(http.MethodPost, "/checkout", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler(c)
	c.Writer.WriteHeaderNow()
	return response
}

func checkoutQuoteForTest(t *testing.T, body string) model.CheckoutQuoteResponse {
	t.Helper()
	response := checkoutRequest(t, body, RequestCheckoutQuote)
	var result struct {
		Success bool
		Data    model.CheckoutQuoteResponse
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
	require.True(t, result.Success, response.Body.String())
	require.NotEmpty(t, result.Data.QuoteID)
	return result.Data
}

func TestTopUpQuotaValidation(t *testing.T) {
	useCheckoutControllerDB(t)
	oldDisplay, oldQuota := operation_setting.GetGeneralSetting().QuotaDisplayType, common.QuotaPerUnit
	t.Cleanup(func() {
		operation_setting.GetGeneralSetting().QuotaDisplayType = oldDisplay
		common.QuotaPerUnit = oldQuota
	})
	for _, display := range []string{"USD", "CNY", "TOKENS"} {
		operation_setting.GetGeneralSetting().QuotaDisplayType = display
		common.QuotaPerUnit = 0
		quote := checkoutQuoteForTest(t, `{"provider":"epay","payment_method":"alipay","selection":{"kind":"amount","source_amount":{"nanos":"1000000001","currency":"USD"}}}`)
		assert.Equal(t, money.Amount{Nanos: 1_000_000_001, Currency: "USD"}, quote.CreditAmount)
		assert.Equal(t, money.Amount{Nanos: 7_310_000_000, Currency: "CNY"}, quote.ChargeAmount)
	}
}

func TestValidateTopUpQuotaReturnsMaximumAmount(t *testing.T) {
	value, err := money.ParseValue("9223372036.85", "USD")
	require.NoError(t, err)
	amount, err := model.QuantizeCheckoutCharge(value, model.PaymentProviderEpay)
	require.NoError(t, err)
	assert.EqualValues(t, 9_223_372_036_850_000_000, amount.Nanos)
	_, err = model.QuantizeCheckoutCharge(money.Amount{Nanos: math.MaxInt64, Currency: "USD"}.Value(), model.PaymentProviderEpay)
	require.ErrorIs(t, err, money.ErrOverflow)
}

func TestRequestAmountRejectsTopUpThatCannotBeSettled(t *testing.T) {
	useCheckoutControllerDB(t)
	for _, amount := range []string{`{"nanos":1,"currency":"USD"}`, `{"nanos":"9223372036854775808","currency":"USD"}`, `{"nanos":"0","currency":"USD"}`, `{"nanos":"1","currency":"USD","quota":1}`} {
		response := checkoutRequest(t, `{"provider":"epay","payment_method":"alipay","selection":{"kind":"amount","source_amount":`+amount+`}}`, RequestAmount)
		assert.Contains(t, response.Body.String(), `"message":"error"`)
	}
	var count int64
	require.NoError(t, model.DB.Model(&model.TopUp{}).Count(&count).Error)
	assert.Zero(t, count)
}

func TestRequestAmountRejectsTopUpThatWouldOverflowWallet(t *testing.T) {
	useCheckoutControllerDB(t)
	quote := checkoutQuoteForTest(t, `{"provider":"epay","payment_method":"alipay","selection":{"kind":"amount","source_amount":{"nanos":"1","currency":"USD"}}}`)
	assert.EqualValues(t, 1, quote.CreditAmount.Nanos, "legacy users.quota never blocks new wallet quotes")
	response := checkoutRequest(t, `{"quote_id":"`+quote.QuoteID+`"}`, RequestEpay)
	var created struct {
		Message string
		Data    map[string]string
		URL     string
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &created))
	require.Equal(t, "success", created.Message, response.Body.String())
	assert.Equal(t, "0.01", created.Data["money"])
	assert.Equal(t, quote.QuoteID, created.Data["out_trade_no"])
	replay := checkoutRequest(t, `{"quote_id":"`+quote.QuoteID+`"}`, RequestEpay)
	assert.JSONEq(t, response.Body.String(), replay.Body.String())
	var legacy struct {
		Amount *int64
		Money  *float64
	}
	require.NoError(t, model.DB.Model(&model.TopUp{}).Select("amount,money").Where("trade_no = ?", quote.QuoteID).Scan(&legacy).Error)
	assert.Nil(t, legacy.Amount)
	assert.Nil(t, legacy.Money)
	params := map[string]string{"pid": "checkout-merchant", "out_trade_no": quote.QuoteID, "trade_no": "epay-external", "money": "0.01", "type": "alipay", "trade_status": epay.StatusTradeSuccess, "sign_type": "MD5"}
	params, err := epay.GenerateParamsWithError(params, "checkout-fixture-key")
	require.NoError(t, err)
	values := make([]string, 0, len(params))
	for key, value := range params {
		values = append(values, key+"="+value)
	}
	result := checkoutRequest(t, strings.Join(values, "&"), func(c *gin.Context) {
		c.Request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		EpayNotify(c)
	})
	assert.Equal(t, "success", result.Body.String())
	wallet, err := model.GetUserWallet(42)
	require.NoError(t, err)
	require.NotNil(t, wallet)
	assert.EqualValues(t, 1, wallet.Balance.Nanos)
}

func TestValidateCreditedQuotaRejectsOverflow(t *testing.T) {
	for _, input := range []string{"", "-1", "0.0000000001", "9223372036.854775808"} {
		_, err := service.CheckoutMajorAmount(input, "USD")
		require.Error(t, err, input)
	}
	actual, err := service.CheckoutMajorAmount("9007199.254740993", "USD")
	require.NoError(t, err)
	assert.EqualValues(t, 9_007_199_254_740_993, actual.Nanos)
}

func TestStripeCreditedQuotaIncludesGroupRatio(t *testing.T) {
	useCheckoutControllerDB(t)
	require.NoError(t, operation_setting.ApplyCheckoutOption(operation_setting.CheckoutConfigOption, `{"providers":{"stripe":{"enabled":true,"group_ratios":{"vip":"1.25"},"products":[{"product_id":"price_exact","name":"Wallet","source_amount":{"nanos":"1000000001","currency":"USD"}}]}}}`))
	price := &model.CheckoutExternalPrice{ProductID: "price_exact", UnitAmount: money.Amount{Nanos: 990_000_000, Currency: "EUR"}, Final: false, Mode: "test"}
	quote, err := model.CreateCheckoutQuote(context.Background(), 42, model.CheckoutQuoteRequest{Provider: "stripe", PaymentMethod: "stripe", Selection: model.CheckoutSelection{Kind: "product", ProductID: "price_exact", Quantity: "3"}}, price, "", "", "", "")
	require.NoError(t, err)
	assert.EqualValues(t, 3_750_000_003, quote.SourceAmount.Nanos)
	assert.Equal(t, money.Amount{Nanos: 2_970_000_000, Currency: "EUR"}, quote.ChargeAmount)
	assert.Equal(t, quote.SourceAmount, quote.CreditAmount)
	assert.False(t, quote.ChargeFinal)
}

type checkoutHTTPTransport func(*http.Request) (*http.Response, error)

func (transport checkoutHTTPTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestCheckoutAuthoritativeProductCreationAndTimeoutReplay(t *testing.T) {
	for _, provider := range []string{"stripe", "creem"} {
		t.Run(provider, func(t *testing.T) {
			useCheckoutControllerDB(t)
			oldTransport := http.DefaultTransport
			stripeKey, stripeSecret, creemKey, creemSecret, creemMode := setting.StripeApiSecret, setting.StripeWebhookSecret, setting.CreemApiKey, setting.CreemWebhookSecret, setting.CreemTestMode
			oldPromotion := setting.StripePromotionCodesEnabled
			setting.StripeApiSecret, setting.StripeWebhookSecret, setting.CreemApiKey, setting.CreemWebhookSecret, setting.CreemTestMode = "sk_test_fixture", "whsec_fixture", "creem_fixture", "creem_fixture_secret", true
			setting.StripePromotionCodesEnabled = true
			t.Cleanup(func() {
				http.DefaultTransport = oldTransport
				setting.StripeApiSecret, setting.StripeWebhookSecret, setting.CreemApiKey, setting.CreemWebhookSecret, setting.CreemTestMode = stripeKey, stripeSecret, creemKey, creemSecret, creemMode
				setting.StripePromotionCodesEnabled = oldPromotion
			})
			require.NoError(t, operation_setting.ApplyCheckoutOption(operation_setting.CheckoutConfigOption, `{"providers":{"`+provider+`":{"enabled":true,"products":[{"product_id":"product_exact","name":"Reward","source_amount":{"nanos":"1000000001","currency":"USD"}}]}}}`))
			creates := 0
			timeout := false
			http.DefaultTransport = checkoutHTTPTransport(func(request *http.Request) (*http.Response, error) {
				var payload string
				if request.Method == http.MethodGet {
					if provider == "stripe" {
						payload = `{"id":"product_exact","active":true,"currency":"eur","unit_amount":123,"unit_amount_decimal":"123","type":"one_time","billing_scheme":"per_unit","livemode":false,"tax_behavior":"unspecified"}`
					} else {
						payload = `{"id":"product_exact","price":123,"currency":"EUR","billing_type":"onetime","status":"active","mode":"test"}`
					}
				} else {
					creates++
					row := model.GetTopUpByTradeNo(request.Header.Get("Idempotency-Key"))
					if provider == "stripe" {
						require.NoError(t, request.ParseForm())
						assert.Equal(t, "false", request.Form.Get("adaptive_pricing[enabled]"))
						assert.Equal(t, "eur", request.Form.Get("currency"))
						assert.Equal(t, "product_exact", request.Form.Get("line_items[0][price]"))
						assert.Equal(t, "2", request.Form.Get("line_items[0][quantity]"))
						require.NotNil(t, row)
						assert.Equal(t, common.TopUpStatusPending, row.Status)
						payload = `{"id":"cs_fixture","url":"https://checkout.stripe.com/pay/cs_fixture"}`
					} else {
						var sent map[string]any
						require.NoError(t, common.DecodeJson(request.Body, &sent))
						assert.Equal(t, "product_exact", sent["product_id"])
						row = model.GetTopUpByTradeNo(sent["request_id"].(string))
						require.NotNil(t, row)
						assert.Equal(t, common.TopUpStatusPending, row.Status)
						payload = `{"id":"ch_fixture","checkout_url":"https://checkout.creem.io/ch_fixture"}`
					}
					if timeout {
						return nil, errors.New("simulated remote timeout")
					}
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload))}, nil
			})
			quantity := "1"
			handler := RequestCreemPay
			if provider == "stripe" {
				quantity = "2"
				handler = RequestStripePay
			}
			request := `{"provider":"` + provider + `","selection":{"kind":"product","product_id":"product_exact","quantity":"` + quantity + `"}}`
			quote := checkoutQuoteForTest(t, request)
			assert.Equal(t, "EUR", string(quote.ChargeAmount.Currency))
			assert.False(t, quote.ChargeFinal)
			created := checkoutRequest(t, `{"quote_id":"`+quote.QuoteID+`"}`, handler)
			assert.Contains(t, created.Body.String(), `"message":"success"`)
			replay := checkoutRequest(t, `{"quote_id":"`+quote.QuoteID+`"}`, handler)
			assert.JSONEq(t, created.Body.String(), replay.Body.String())
			assert.Equal(t, 1, creates)
			for _, body := range []string{`{"amount":1}`, `{"quote_id":"` + quote.QuoteID + `","amount":1}`, `{"quote_id":"` + quote.QuoteID + `","credit_amount":{"nanos":"1","currency":"USD"}}`} {
				assert.Contains(t, checkoutRequest(t, body, handler).Body.String(), `"message":"error"`)
			}
			timeout = true
			uncertain := checkoutQuoteForTest(t, request)
			first := checkoutRequest(t, `{"quote_id":"`+uncertain.QuoteID+`"}`, handler)
			assert.Contains(t, first.Body.String(), "Payment is being processed")
			retry := checkoutRequest(t, `{"quote_id":"`+uncertain.QuoteID+`"}`, handler)
			assert.Contains(t, retry.Body.String(), "Payment is being processed")
			assert.Equal(t, 2, creates)
			persisted, err := model.GetCheckoutOrder(context.Background(), uncertain.QuoteID)
			require.NoError(t, err)
			assert.Equal(t, "uncertain", persisted.CreationState)
		})
	}
}
