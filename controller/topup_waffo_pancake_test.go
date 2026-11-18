package controller

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatWaffoPancakeAmount_UsesDisplayPriceString(t *testing.T) {
	for _, tc := range []struct{ amount, expected string }{{"29", "29.00"}, {"29.9", "29.90"}, {"29.999", "30.00"}, {"0.000000001", "0.01"}} {
		value, err := money.ParseValue(tc.amount, "USD")
		require.NoError(t, err)
		amount, err := model.QuantizeCheckoutCharge(value, model.PaymentProviderWaffoPancake)
		require.NoError(t, err)
		assert.Equal(t, tc.expected, model.CheckoutAmountString(amount, model.PaymentProviderWaffoPancake))
	}
}

func TestGetWaffoPancakePayMoney(t *testing.T) {
	useCheckoutControllerDB(t)
	display, quota, price := operation_setting.GetGeneralSetting().QuotaDisplayType, common.QuotaPerUnit, setting.WaffoPancakeUnitPrice
	t.Cleanup(func() {
		operation_setting.GetGeneralSetting().QuotaDisplayType, common.QuotaPerUnit, setting.WaffoPancakeUnitPrice = display, quota, price
	})
	common.QuotaPerUnit, setting.WaffoPancakeUnitPrice = 0, 900
	require.NoError(t, operation_setting.ApplyCheckoutOption(operation_setting.CheckoutConfigOption, `{"providers":{"waffo_pancake":{"enabled":true,"source_unit":{"nanos":"1000000000","currency":"USD"},"charge_unit":{"nanos":"2500000000","currency":"USD"},"minimum":{"nanos":"1","currency":"USD"},"group_ratios":{"vip":"1.2"},"discounts":[{"source_amount":{"nanos":"10000000000","currency":"USD"},"ratio":"0.8"}]}}}`))
	source := money.Amount{Nanos: 10_000_000_000, Currency: "USD"}
	for _, display := range []string{"USD", "CNY", "TOKENS"} {
		operation_setting.GetGeneralSetting().QuotaDisplayType = display
		order, err := model.CreateCheckoutQuote(context.Background(), 42, model.CheckoutQuoteRequest{Provider: "waffo_pancake", PaymentMethod: "waffo_pancake", Selection: model.CheckoutSelection{Kind: "amount", SourceAmount: &source}}, nil, "", "", "", "")
		require.NoError(t, err)
		assert.Equal(t, money.Amount{Nanos: 24_000_000_000, Currency: "USD"}, order.ChargeAmount)
		assert.Equal(t, source, order.CreditAmount)
		assert.False(t, order.ChargeFinal)
	}
}

func TestCheckoutPancakeTokenFailureNeverCreatesSecondSession(t *testing.T) {
	useCheckoutControllerDB(t)
	merchant, key, store, product := setting.WaffoPancakeMerchantID, setting.WaffoPancakePrivateKey, setting.WaffoPancakeStoreID, setting.WaffoPancakeProductID
	transport, client := http.DefaultTransport, http.DefaultClient
	t.Cleanup(func() {
		setting.WaffoPancakeMerchantID, setting.WaffoPancakePrivateKey, setting.WaffoPancakeStoreID, setting.WaffoPancakeProductID = merchant, key, store, product
		http.DefaultTransport, http.DefaultClient = transport, client
	})
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	setting.WaffoPancakeMerchantID = "MER_AbCdEfGhIjKlMnOpQrStUv"
	setting.WaffoPancakePrivateKey = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)}))
	setting.WaffoPancakeStoreID = "STO_AbCdEfGhIjKlMnOpQrStUv"
	setting.WaffoPancakeProductID = "PROD_AbCdEfGhIjKlMnOpQrStUv"
	http.DefaultClient = &http.Client{}
	require.NoError(t, operation_setting.ApplyCheckoutOption(operation_setting.CheckoutConfigOption, `{"providers":{"waffo_pancake":{"enabled":true,"source_unit":{"nanos":"1000000000","currency":"USD"},"charge_unit":{"nanos":"1000000000","currency":"USD"},"minimum":{"nanos":"1","currency":"USD"}}}}`))
	sessions, tokens := 0, 0
	http.DefaultTransport = checkoutHTTPTransport(func(request *http.Request) (*http.Response, error) {
		var payload map[string]any
		require.NoError(t, common.DecodeJson(request.Body, &payload))
		response := ""
		status := http.StatusOK
		switch request.URL.Path {
		case "/v1/actions/checkout/create-session":
			sessions++
			topUp := model.GetTopUpByTradeNo(payload["orderMerchantExternalId"].(string))
			require.NotNil(t, topUp)
			assert.Equal(t, common.TopUpStatusPending, topUp.Status)
			assert.Equal(t, "1.01", payload["priceSnapshot"].(map[string]any)["amount"])
			assert.Equal(t, setting.WaffoPancakeProductID, payload["productId"])
			response = `{"data":{"sessionId":"SES_fixture","checkoutUrl":"https://checkout.waffo.ai/fixture","expiresAt":"2026-11-15T00:00:00Z"}}`
		case "/v1/actions/auth/issue-session-token":
			tokens++
			assert.Equal(t, "new-api-user-42", payload["buyerIdentity"])
			if tokens == 1 {
				status = http.StatusServiceUnavailable
				response = `{"errors":[{"message":"temporary token failure"}]}`
			} else {
				response = `{"data":{"token":"fixture-buyer-token","expiresAt":"2026-11-15T00:00:00Z"}}`
			}
		default:
			t.Fatalf("unexpected provider path %s", request.URL.Path)
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
	})
	quote := checkoutQuoteForTest(t, `{"provider":"waffo_pancake","selection":{"kind":"amount","source_amount":{"nanos":"1000000001","currency":"USD"}}}`)
	first := checkoutRequest(t, `{"quote_id":"`+quote.QuoteID+`"}`, RequestWaffoPancakePay)
	assert.Contains(t, first.Body.String(), "Payment is being processed")
	replay := checkoutRequest(t, `{"quote_id":"`+quote.QuoteID+`"}`, RequestWaffoPancakePay)
	assert.Contains(t, replay.Body.String(), `"message":"success"`)
	assert.Contains(t, replay.Body.String(), "#token=fixture-buyer-token")
	assert.Equal(t, 1, sessions)
	assert.Equal(t, 2, tokens)
	order, err := model.GetCheckoutOrder(context.Background(), quote.QuoteID)
	require.NoError(t, err)
	assert.Equal(t, "SES_fixture", order.ProviderSessionID)
	assert.Equal(t, "https://checkout.waffo.ai/fixture", order.CheckoutURL)
	assert.NotContains(t, order.CheckoutURL, "token")
}
