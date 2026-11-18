package service

import (
	"bytes"
	"context"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/setting"
	"github.com/stripe/stripe-go/v81"
)

// checkoutProviderGET decodes exact provider fields without passing monetary
// values through SDK float fields. Bodies and usable credentials are not logged.
func checkoutProviderGET(ctx context.Context, provider, path string, result any) error {
	base := "https://api.stripe.com/v1/"
	if provider == model.PaymentProviderCreem {
		base = "https://api.creem.io/v1/"
		if setting.CreemTestMode {
			base = "https://test-api.creem.io/v1/"
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return common.NewMessage("Payment configuration error")
	}
	if provider == model.PaymentProviderStripe {
		if setting.StripeApiSecret == "" {
			return common.NewMessage("Payment configuration error")
		}
		request.SetBasicAuth(setting.StripeApiSecret, "")
		request.Header.Set("Stripe-Version", stripe.APIVersion)
	} else if provider == model.PaymentProviderCreem {
		if setting.CreemApiKey == "" {
			return common.NewMessage("Payment configuration error")
		}
		request.Header.Set("x-api-key", setting.CreemApiKey)
	} else {
		return common.NewMessage("Payment configuration error")
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return common.NewMessage("Failed to start payment")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return common.NewMessage("Payment configuration error")
	}
	if err := common.DecodeJson(io.LimitReader(response.Body, 1<<20), result); err != nil {
		return common.NewMessage("Payment configuration error")
	}
	return nil
}

// GetCheckoutExternalPrice retrieves a current fixed, one-time provider price.
// Tiering, customer-chosen prices and fractional minor-unit prices require a
// separate pricing contract; they never fall back to the legacy preview.
func GetCheckoutExternalPrice(ctx context.Context, provider, productID string) (*model.CheckoutExternalPrice, error) {
	if productID == "" || len(productID) > 255 {
		return nil, common.NewMessage("Product does not exist")
	}
	if provider == model.PaymentProviderStripe {
		var price struct {
			ID                string  `json:"id"`
			Active            bool    `json:"active"`
			Currency          string  `json:"currency"`
			UnitAmount        *int64  `json:"unit_amount"`
			UnitAmountDecimal *string `json:"unit_amount_decimal"`
			Type              string  `json:"type"`
			BillingScheme     string  `json:"billing_scheme"`
			CustomUnitAmount  any     `json:"custom_unit_amount"`
			TransformQuantity any     `json:"transform_quantity"`
			TaxBehavior       string  `json:"tax_behavior"`
			Livemode          *bool   `json:"livemode"`
		}
		if err := checkoutProviderGET(ctx, provider, "prices/"+url.PathEscape(productID), &price); err != nil {
			return nil, err
		}
		if price.ID != productID || !price.Active || price.Type != "one_time" || price.BillingScheme != "per_unit" || price.CustomUnitAmount != nil || price.TransformQuantity != nil || price.UnitAmount == nil || *price.UnitAmount <= 0 || price.Livemode == nil {
			return nil, common.NewMessage("Payment configuration error")
		}
		if price.UnitAmountDecimal != nil {
			minor, ok := new(big.Rat).SetString(*price.UnitAmountDecimal)
			if !ok || minor.Cmp(big.NewRat(*price.UnitAmount, 1)) != 0 {
				return nil, common.NewMessage("Payment configuration error")
			}
		}
		currency, err := money.ParseCurrency(strings.ToUpper(price.Currency))
		if err != nil {
			return nil, common.NewMessage("Payment configuration error")
		}
		amount, err := CheckoutMinorAmount(*price.UnitAmount, currency, provider)
		if err != nil {
			return nil, err
		}
		mode := "test"
		if *price.Livemode {
			mode = "live"
		}
		return &model.CheckoutExternalPrice{ProductID: productID, UnitAmount: amount, Final: !setting.StripePromotionCodesEnabled, Mode: mode, PromotionCodes: setting.StripePromotionCodesEnabled, TaxMode: price.TaxBehavior}, nil
	}
	if provider == model.PaymentProviderCreem {
		var product struct {
			ID          string `json:"id"`
			Price       *int64 `json:"price"`
			Currency    string `json:"currency"`
			BillingType string `json:"billing_type"`
			Status      string `json:"status"`
			TaxMode     string `json:"tax_mode"`
			Mode        string `json:"mode"`
		}
		if err := checkoutProviderGET(ctx, provider, "products?product_id="+url.QueryEscape(productID), &product); err != nil {
			return nil, err
		}
		mode := "prod"
		if setting.CreemTestMode {
			mode = "test"
		}
		if product.ID != productID || product.Price == nil || *product.Price <= 0 || product.BillingType != "onetime" || product.Status != "active" || product.Mode != mode {
			return nil, common.NewMessage("Payment configuration error")
		}
		currency, err := money.ParseCurrency(product.Currency)
		if err != nil {
			return nil, common.NewMessage("Payment configuration error")
		}
		amount, err := CheckoutMinorAmount(*product.Price, currency, provider)
		if err != nil {
			return nil, err
		}
		return &model.CheckoutExternalPrice{ProductID: productID, UnitAmount: amount, Final: false, Mode: mode, TaxMode: product.TaxMode}, nil
	}
	return nil, common.NewMessage("Invalid parameters")
}

// CreateProductCheckout creates a session from frozen external price facts. It
// never accepts a client price, reward, or quantity override.
func CreateProductCheckout(ctx context.Context, topUp *model.TopUp, order *model.CheckoutOrder, user *model.User, successURL, cancelURL string) (string, string, error) {
	price, err := GetCheckoutExternalPrice(ctx, topUp.PaymentProvider, order.ProductID)
	if err != nil {
		return "", "", err
	}
	if price.UnitAmount != order.UnitChargeAmount || price.Mode != order.ProviderMode || price.TaxMode != order.TaxMode {
		return "", "", common.NewMessage("Payment configuration error")
	}
	var endpoint string
	var body io.Reader
	contentType := "application/json"
	if topUp.PaymentProvider == model.PaymentProviderStripe {
		endpoint = "https://api.stripe.com/v1/checkout/sessions"
		values := url.Values{
			"client_reference_id": {order.TradeNo}, "mode": {"payment"},
			"success_url": {successURL}, "cancel_url": {cancelURL},
			"line_items[0][price]": {order.ProductID}, "line_items[0][quantity]": {order.Quantity},
			"currency":                     {strings.ToLower(string(order.ChargeAmount.Currency))},
			"allow_promotion_codes":        {strconv.FormatBool(order.PromotionCodes)},
			"adaptive_pricing[enabled]":    {"false"},
			"automatic_tax[enabled]":       {"false"},
			"metadata[checkout_reference]": {order.TradeNo},
			"metadata[checkout_price]":     {order.ProductID},
			"metadata[checkout_quantity]":  {order.Quantity},
		}
		if user.StripeCustomer != "" {
			values.Set("customer", user.StripeCustomer)
		} else {
			values.Set("customer_creation", "always")
			if user.Email != "" {
				values.Set("customer_email", user.Email)
			}
		}
		contentType, body = "application/x-www-form-urlencoded", strings.NewReader(values.Encode())
	} else if topUp.PaymentProvider == model.PaymentProviderCreem {
		endpoint = "https://api.creem.io/v1/checkouts"
		if order.ProviderMode == "test" {
			endpoint = "https://test-api.creem.io/v1/checkouts"
		}
		payload := map[string]any{"product_id": order.ProductID, "request_id": order.TradeNo, "units": 1}
		if user.Email != "" {
			payload["customer"] = map[string]string{"email": user.Email}
		}
		encoded, err := common.Marshal(payload)
		if err != nil {
			return "", "", err
		}
		body = bytes.NewReader(encoded)
	} else {
		return "", "", common.NewMessage("Invalid parameters")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return "", "", common.NewMessage("Failed to start payment")
	}
	request.Header.Set("Content-Type", contentType)
	if topUp.PaymentProvider == model.PaymentProviderStripe {
		request.SetBasicAuth(setting.StripeApiSecret, "")
		request.Header.Set("Stripe-Version", stripe.APIVersion)
		request.Header.Set("Idempotency-Key", order.TradeNo)
	} else {
		request.Header.Set("x-api-key", setting.CreemApiKey)
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return "", "", common.NewMessage("Failed to start payment")
	}
	defer response.Body.Close()
	var remote struct {
		ID          string `json:"id"`
		URL         string `json:"url"`
		CheckoutURL string `json:"checkout_url"`
	}
	if response.StatusCode/100 != 2 || common.DecodeJson(io.LimitReader(response.Body, 1<<20), &remote) != nil || remote.ID == "" {
		return "", "", common.NewMessage("Failed to start payment")
	}
	if topUp.PaymentProvider == model.PaymentProviderCreem {
		remote.URL = remote.CheckoutURL
	}
	parsed, err := url.Parse(remote.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
		return "", "", common.NewMessage("Failed to start payment")
	}
	return remote.ID, remote.URL, nil
}

// CheckoutRecoveryHandler retries verified payments with the existing scheduler's
// multi-instance locking. Financial failures remain visible as pending receipts.
type CheckoutRecoveryHandler struct{}

func (CheckoutRecoveryHandler) Type() string            { return "checkout_fulfillment" }
func (CheckoutRecoveryHandler) Enabled() bool           { return true }
func (CheckoutRecoveryHandler) Interval() time.Duration { return time.Minute }
func (CheckoutRecoveryHandler) NewPayload() any         { return nil }
func (CheckoutRecoveryHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	completed, err := model.RecoverCheckoutPayments(ctx, 100)
	if err != nil {
		failSystemTask(task, runnerID, err)
		return
	}
	if err := model.FinishSystemTask(task.TaskID, runnerID, model.SystemTaskStatusSucceeded, map[string]int{"completed": completed}, ""); err != nil {
		logSystemTaskLockError(ctx, task, err)
	}
}

func init() { RegisterSystemTaskHandler(CheckoutRecoveryHandler{}) }

// CheckoutMajorAmount accepts an exact provider decimal without rounding away
// any received money. Missing, negative and sub-nano amounts are rejected.
func CheckoutMajorAmount(text string, currency money.Currency) (money.Amount, error) {
	if text == "" || strings.TrimSpace(text) != text {
		return money.Amount{}, common.NewMessage("Invalid amount")
	}
	value, err := money.ParseValue(text, currency)
	if err != nil {
		return money.Amount{}, err
	}
	amount, err := value.Credit()
	if err != nil || amount.Nanos < 0 {
		return money.Amount{}, common.NewMessage("Invalid amount")
	}
	comparison, err := value.Compare(amount.Value())
	if err != nil || comparison != 0 {
		return money.Amount{}, common.NewMessage("Invalid amount")
	}
	return amount, nil
}

// CheckoutMinorAmount converts provider integer wire units directly to money.
func CheckoutMinorAmount(minor int64, currency money.Currency, provider string) (money.Amount, error) {
	if minor < 0 {
		return money.Amount{}, common.NewMessage("Invalid amount")
	}
	scale := int64(100)
	if model.CheckoutCurrencyDigits(provider, currency) == 0 {
		scale = 1
	}
	value, err := money.NewRationalValue(big.NewRat(minor, scale), currency)
	if err != nil {
		return money.Amount{}, err
	}
	return value.Receivable()
}
