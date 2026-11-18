package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/stripe/stripe-go/v81"
	"time"

	"github.com/Calcium-Ion/go-epay/epay"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	waffoorder "github.com/waffo-com/waffo-go/types/order"
)

// confirmExactCheckout commits the authenticated paid fact before attempting
// wallet fulfillment. A failure leaves the paid obligation available for retry.
func confirmExactCheckout(ctx context.Context, provider string, receipt model.CheckoutPayment) error {
	if err := model.ConfirmCheckoutPayment(ctx, provider, receipt); err != nil {
		return err
	}
	return model.CompleteCheckoutPayment(ctx, receipt.TradeNo)
}

func handleExactStripeEvent(ctx context.Context, event stripe.Event) (bool, error) {
	reference := event.GetObjectValue("client_reference_id")
	topUp := model.GetTopUpByTradeNo(reference)
	if topUp == nil || topUp.ContractVersion != model.CheckoutContractVersion {
		if strings.HasPrefix(reference, "CHECKOUT-") {
			return true, model.ErrTopUpNotFound
		}
		return false, nil
	}
	if event.Type != stripe.EventTypeCheckoutSessionCompleted && event.Type != stripe.EventTypeCheckoutSessionAsyncPaymentSucceeded {
		return true, nil
	}
	var session struct {
		ID            string            `json:"id"`
		Reference     string            `json:"client_reference_id"`
		Status        string            `json:"status"`
		PaymentStatus string            `json:"payment_status"`
		AmountTotal   *int64            `json:"amount_total"`
		Currency      string            `json:"currency"`
		Livemode      *bool             `json:"livemode"`
		Metadata      map[string]string `json:"metadata"`
	}
	if common.Unmarshal(event.Data.Raw, &session) != nil {
		return true, model.ErrPaymentMethodMismatch
	}
	if session.PaymentStatus != "paid" {
		return true, nil
	}
	order, err := model.GetCheckoutOrder(ctx, reference)
	if err != nil {
		return true, err
	}
	mode := "test"
	if session.Livemode != nil && *session.Livemode {
		mode = "live"
	}
	if session.ID == "" || session.Reference != reference || session.Status != "complete" || session.AmountTotal == nil || session.Livemode == nil || mode != order.ProviderMode || session.Metadata["checkout_reference"] != reference || session.Metadata["checkout_price"] != order.ProductID || session.Metadata["checkout_quantity"] != order.Quantity {
		return true, model.ErrPaymentMethodMismatch
	}
	currency, err := money.ParseCurrency(strings.ToUpper(session.Currency))
	if err != nil {
		return true, err
	}
	paid, err := service.CheckoutMinorAmount(*session.AmountTotal, currency, model.PaymentProviderStripe)
	if err != nil {
		return true, err
	}
	return true, confirmExactCheckout(ctx, model.PaymentProviderStripe, model.CheckoutPayment{TradeNo: reference, PaidAmount: paid, ProviderOrderID: session.ID, ProviderSessionID: session.ID, EventID: event.ID, CustomerID: event.GetObjectValue("customer")})
}

type checkoutCreateRequest struct {
	QuoteID    string `json:"quote_id"`
	SuccessURL string `json:"success_url,omitempty"`
	CancelURL  string `json:"cancel_url,omitempty"`
}

func requestCheckoutPayment(c *gin.Context, provider string) {
	var request checkoutCreateRequest
	if common.DecodeJsonStrict(c.Request.Body, &request) != nil || request.QuoteID == "" || len(request.QuoteID) > 255 || (provider != model.PaymentProviderStripe && (request.SuccessURL != "" || request.CancelURL != "")) {
		paymentError(c, common.NewMessage("Invalid parameters"))
		return
	}
	if request.SuccessURL != "" && common.ValidateRedirectURL(request.SuccessURL) != nil {
		paymentError(c, common.NewMessage("Payment success redirect URL is not in the trusted domain list"))
		return
	}
	if request.CancelURL != "" && common.ValidateRedirectURL(request.CancelURL) != nil {
		paymentError(c, common.NewMessage("Payment cancel redirect URL is not in the trusted domain list"))
		return
	}
	if !checkoutProviderAvailable(provider) {
		paymentError(c, common.NewMessage("Payment configuration error"))
		return
	}
	ctx := c.Request.Context()
	topUp, order, claimed, err := model.AcceptCheckoutQuote(ctx, c.GetInt("id"), provider, request.QuoteID)
	if err != nil {
		paymentError(c, err)
		return
	}
	user, err := model.GetUserById(topUp.UserId, false)
	if err != nil || user == nil {
		paymentError(c, common.NewMessage("User does not exist"))
		return
	}
	if topUp.Status == common.TopUpStatusSuccess {
		paymentError(c, common.NewMessage("Order is not pending payment and cannot be completed"))
		return
	}
	if provider == model.PaymentProviderEpay {
		if order.ProviderStoreID != operation_setting.EpayId {
			paymentError(c, common.NewMessage("Payment configuration error"))
			return
		}
		client := GetEpayClient()
		if client == nil {
			paymentError(c, common.NewMessage("Payment configuration error"))
			return
		}
		notify, _ := url.Parse(service.GetCallbackAddress() + "/api/user/epay/notify")
		redirect, _ := url.Parse(paymentReturnPath("/wallet?show_history=true"))
		uri, form, err := client.Purchase(&epay.PurchaseArgs{Type: topUp.PaymentMethod, ServiceTradeNo: order.TradeNo, Name: "Wallet top-up", Money: model.CheckoutAmountString(order.ChargeAmount, provider), Device: epay.PC, NotifyUrl: notify, ReturnUrl: redirect})
		if err != nil {
			paymentError(c, common.NewMessage("Failed to start payment"))
			return
		}
		if err := model.SaveCheckoutRemoteIdentity(context.WithoutCancel(ctx), order.TradeNo, "", ""); err != nil {
			paymentError(c, common.NewMessage("Failed to start payment"))
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "success", "data": form, "url": uri})
		return
	}
	if !claimed && (order.ProviderSessionID == "" || order.CheckoutURL == "") {
		paymentError(c, model.ErrCheckoutPending)
		return
	}
	if claimed {
		// A claimed checkout is never remotely recreated, even after a timeout.
		// Persist failures with an uncancelled context so a request cancellation
		// cannot erase the accepted obligation.
		sessionID, checkoutURL := "", ""
		switch provider {
		case model.PaymentProviderStripe, model.PaymentProviderCreem:
			if request.SuccessURL == "" {
				request.SuccessURL = paymentReturnPath("/wallet?show_history=true")
			}
			if request.CancelURL == "" {
				request.CancelURL = paymentReturnPath("/wallet")
			}
			sessionID, checkoutURL, err = service.CreateProductCheckout(ctx, topUp, order, user, request.SuccessURL, request.CancelURL)
		case model.PaymentProviderWaffo:
			mode := "prod"
			if setting.WaffoSandbox {
				mode = "test"
			}
			if order.ProviderStoreID != setting.WaffoMerchantId || order.ProviderMode != mode {
				err = common.NewMessage("Payment configuration error")
				break
			}
			var sdkErr error
			sdk, sdkErr := getWaffoSDK()
			if sdkErr != nil {
				err = sdkErr
				break
			}
			notify := service.GetCallbackAddress() + "/api/waffo/webhook"
			if setting.WaffoNotifyUrl != "" {
				notify = setting.WaffoNotifyUrl
			}
			redirect := paymentReturnPath("/wallet?show_history=true")
			if setting.WaffoReturnUrl != "" {
				redirect = setting.WaffoReturnUrl
			}
			response, createErr := sdk.Order().Create(ctx, &waffoorder.CreateOrderParams{
				PaymentRequestID: order.TradeNo, MerchantOrderID: order.TradeNo,
				OrderAmount: model.CheckoutAmountString(order.ChargeAmount, provider), OrderCurrency: string(order.ChargeAmount.Currency),
				OrderDescription: "Wallet top-up", OrderRequestedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), NotifyURL: notify,
				MerchantInfo: &waffoorder.MerchantInfo{MerchantID: order.ProviderStoreID},
				UserInfo:     &waffoorder.UserInfo{UserID: strconv.Itoa(user.Id), UserEmail: getWaffoUserEmail(user), UserTerminal: "WEB"},
				PaymentInfo:  &waffoorder.PaymentInfo{ProductName: "ONE_TIME_PAYMENT", PayMethodType: order.PayMethodType, PayMethodName: order.PayMethodName},
				GoodsInfo:    &waffoorder.GoodsInfo{GoodsName: "Wallet top-up", AppName: common.SystemName}, SuccessRedirectURL: redirect, FailedRedirectURL: redirect,
			}, nil)
			if createErr != nil {
				err = createErr
				break
			}
			if response == nil || !response.IsSuccess() || response.GetData() == nil {
				err = fmt.Errorf("checkout creation rejected")
				break
			}
			data := response.GetData()
			sessionID, checkoutURL = data.AcquiringOrderID, data.FetchRedirectURL()
			if checkoutURL == "" {
				checkoutURL = data.OrderAction
			}
			if sessionID == "" || checkoutURL == "" {
				err = fmt.Errorf("empty checkout response")
			}
		case model.PaymentProviderWaffoPancake:
			if order.ProviderMerchantID != setting.WaffoPancakeMerchantID {
				err = common.NewMessage("Payment configuration error")
				break
			}
			err = service.CreateExactPancakeSession(ctx, order, user)
			if err == nil {
				order, err = model.GetCheckoutOrder(ctx, order.TradeNo)
			}
		default:
			err = common.NewMessage("Invalid parameters")
		}
		if err == nil && provider != model.PaymentProviderWaffoPancake {
			err = model.SaveCheckoutRemoteIdentity(context.WithoutCancel(ctx), order.TradeNo, sessionID, checkoutURL)
			order.ProviderSessionID, order.CheckoutURL = sessionID, checkoutURL
		}
		if err != nil {
			if markErr := model.MarkCheckoutCreationUncertain(context.WithoutCancel(ctx), topUp.TradeNo); markErr != nil {
				paymentError(c, common.NewMessage("Failed to create order"))
				return
			}
			paymentError(c, model.ErrCheckoutPending)
			return
		}
	}
	switch provider {
	case model.PaymentProviderStripe:
		c.JSON(http.StatusOK, gin.H{"message": "success", "data": gin.H{"pay_link": order.CheckoutURL}})
	case model.PaymentProviderCreem:
		c.JSON(http.StatusOK, gin.H{"message": "success", "data": gin.H{"checkout_url": order.CheckoutURL, "order_id": order.TradeNo}})
	case model.PaymentProviderWaffo:
		c.JSON(http.StatusOK, gin.H{"message": "success", "data": gin.H{"payment_url": order.CheckoutURL, "order_id": order.TradeNo}})
	case model.PaymentProviderWaffoPancake:
		checkoutURL, err := service.ResumeExactPancakeSession(ctx, order, topUp.UserId)
		if err != nil {
			paymentError(c, model.ErrCheckoutPending)
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "success", "data": gin.H{"checkout_url": checkoutURL, "session_id": order.ProviderSessionID, "order_id": order.TradeNo}})
	}
}

func checkoutProviderAvailable(provider string) bool {
	if !operation_setting.IsPaymentComplianceConfirmed() {
		return false
	}
	switch provider {
	case model.PaymentProviderEpay:
		return isEpayWebhookConfigured() && len(operation_setting.PayMethods) > 0
	case model.PaymentProviderStripe:
		return setting.StripeApiSecret != "" && isStripeWebhookConfigured()
	case model.PaymentProviderCreem:
		return setting.CreemApiKey != "" && isCreemWebhookConfigured()
	case model.PaymentProviderWaffo:
		return setting.WaffoMerchantId != "" && isWaffoWebhookConfigured()
	case model.PaymentProviderWaffoPancake:
		return setting.WaffoPancakeMerchantID != "" && setting.WaffoPancakePrivateKey != "" && setting.WaffoPancakeStoreID != "" && setting.WaffoPancakeProductID != ""
	}
	return false
}

func checkoutProviderInfo() []gin.H {
	configuration := operation_setting.GetCheckoutConfig()
	providers := make([]gin.H, 0, len(operation_setting.CheckoutProviders))
	for _, name := range operation_setting.CheckoutProviders {
		provider, exists := configuration.Providers[name]
		kind := "amount"
		if name == model.PaymentProviderStripe || name == model.PaymentProviderCreem {
			kind = "product"
		}
		info := gin.H{"provider": name, "configured": exists && provider.Configured(name), "enabled": exists && provider.Enabled && checkoutProviderAvailable(name), "selection_kind": kind, "presets": append([]money.Amount{}, provider.Presets...), "products": append([]operation_setting.CheckoutProduct{}, provider.Products...)}
		if provider.Minimum != nil {
			info["minimum"] = *provider.Minimum
		}
		if provider.SourceUnit != nil {
			info["source_unit"] = *provider.SourceUnit
		}
		providers = append(providers, info)
	}
	return providers
}

func RequestCheckoutQuote(c *gin.Context) { requestCheckoutQuote(c, "") }

func requestCheckoutQuote(c *gin.Context, expectedProvider string) {
	var request model.CheckoutQuoteRequest
	if common.DecodeJsonStrict(c.Request.Body, &request) != nil || request.Provider == "" || (expectedProvider != "" && request.Provider != expectedProvider) || !checkoutProviderAvailable(request.Provider) {
		paymentError(c, common.NewMessage("Invalid parameters"))
		return
	}
	id := c.GetInt("id")
	user, err := model.GetUserById(id, false)
	if err != nil || user == nil || user.Id <= 0 || user.Status != common.UserStatusEnabled {
		paymentError(c, common.NewMessage("User does not exist"))
		return
	}
	if request.PayMethodIndex != nil && request.Provider != model.PaymentProviderWaffo {
		paymentError(c, common.NewMessage("Invalid parameters"))
		return
	}
	payMethodType, payMethodName, mode, storeID := "", "", "", ""
	switch request.Provider {
	case model.PaymentProviderEpay:
		if !operation_setting.ContainsPayMethod(request.PaymentMethod) {
			paymentError(c, common.NewMessage("Payment method does not exist"))
			return
		}
		storeID = operation_setting.EpayId
	case model.PaymentProviderWaffo:
		if request.PaymentMethod != "" && request.PaymentMethod != model.PaymentMethodWaffo {
			paymentError(c, common.NewMessage("Unsupported payment method"))
			return
		}
		request.PaymentMethod = model.PaymentMethodWaffo
		methods := setting.GetWaffoPayMethods()
		if request.PayMethodIndex != nil {
			index := *request.PayMethodIndex
			if index < 0 || index >= len(methods) {
				paymentError(c, common.NewMessage("Unsupported payment method"))
				return
			}
			payMethodType, payMethodName = methods[index].PayMethodType, methods[index].PayMethodName
		}
		mode, storeID = "prod", setting.WaffoMerchantId
		if setting.WaffoSandbox {
			mode = "test"
		}
	default:
		if request.PaymentMethod != "" && request.PaymentMethod != request.Provider {
			paymentError(c, common.NewMessage("Unsupported payment method"))
			return
		}
		request.PaymentMethod = request.Provider
		if request.Provider == model.PaymentProviderWaffoPancake {
			storeID = setting.WaffoPancakeStoreID
		}
	}
	var external *model.CheckoutExternalPrice
	if request.Provider == model.PaymentProviderStripe || request.Provider == model.PaymentProviderCreem {
		provider, exists := operation_setting.GetCheckoutConfig().Providers[request.Provider]
		found := false
		if exists && provider.Enabled && request.Selection.Kind == "product" && request.Selection.SourceAmount == nil {
			for _, product := range provider.Products {
				if product.ProductID == request.Selection.ProductID {
					found = true
					break
				}
			}
		}
		if !found {
			paymentError(c, common.NewMessage("Product does not exist"))
			return
		}
		if _, err := model.CheckoutQuantity(request.Selection, request.Provider); err != nil {
			paymentError(c, err)
			return
		}
		external, err = service.GetCheckoutExternalPrice(c.Request.Context(), request.Provider, request.Selection.ProductID)
		if err != nil {
			paymentError(c, err)
			return
		}
	}
	quote, err := model.CreateCheckoutQuote(c.Request.Context(), id, request, external, payMethodType, payMethodName, mode, storeID)
	if err != nil {
		paymentError(c, err)
		return
	}
	common.ApiSuccess(c, quote.Quote())
}
