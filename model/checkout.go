package model

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

const (
	CheckoutContractVersion = 1
	CheckoutPurposeWallet   = "wallet_topup"
	CheckoutStatusQuoted    = "quoted"
)

var ErrCheckoutPending = common.NewMessage("Payment is being processed")

type CheckoutSelection struct {
	Kind         string        `json:"kind"`
	SourceAmount *money.Amount `json:"source_amount,omitempty"`
	ProductID    string        `json:"product_id,omitempty"`
	Quantity     string        `json:"quantity,omitempty"`
}

func (selection *CheckoutSelection) UnmarshalJSON(data []byte) error {
	type plain CheckoutSelection
	var next plain
	if common.DecodeJsonStrict(bytes.NewReader(data), &next) != nil {
		return common.NewMessage("Invalid parameters")
	}
	var fields map[string]json.RawMessage
	if common.Unmarshal(data, &fields) != nil || fields == nil {
		return common.NewMessage("Invalid parameters")
	}
	for _, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return common.NewMessage("Invalid parameters")
		}
	}
	*selection = CheckoutSelection(next)
	return nil
}

type CheckoutQuoteRequest struct {
	Provider       string            `json:"provider"`
	PaymentMethod  string            `json:"payment_method,omitempty"`
	PayMethodIndex *int              `json:"pay_method_index,omitempty"`
	Selection      CheckoutSelection `json:"selection"`
}

type CheckoutFX struct {
	FromCurrency money.Currency `json:"from_currency"`
	ToCurrency   money.Currency `json:"to_currency"`
	Rate         string         `json:"rate"`
}

type CheckoutQuoteResponse struct {
	QuoteID      string       `json:"quote_id"`
	ExpiresAt    int64        `json:"expires_at"`
	ChargeAmount money.Amount `json:"charge_amount"`
	ChargeFinal  bool         `json:"charge_final"`
	SourceAmount money.Amount `json:"source_amount"`
	CreditAmount money.Amount `json:"credit_amount"`
	FX           *CheckoutFX  `json:"fx"`
}

// CheckoutOrder holds exact facts only for new wallet checkouts. Legacy top-ups
// and subscription history have no row here, so unknown money stays unknown.
type CheckoutOrder struct {
	TradeNo            string              `json:"-" gorm:"type:varchar(255);primaryKey"`
	ExpiresAt          int64               `json:"expires_at" gorm:"not null"`
	AcceptedAt         int64               `json:"accepted_at" gorm:"not null"`
	ChargeAmount       money.Amount        `json:"charge_amount" gorm:"embedded;embeddedPrefix:charge_"`
	ChargeFinal        bool                `json:"charge_final" gorm:"not null"`
	UnitChargeAmount   money.Amount        `json:"unit_charge_amount" gorm:"embedded;embeddedPrefix:unit_charge_"`
	PromotionCodes     bool                `json:"-" gorm:"not null"`
	TaxMode            string              `json:"-" gorm:"type:varchar(32);not null"`
	SourceAmount       money.Amount        `json:"source_amount" gorm:"embedded;embeddedPrefix:source_"`
	CreditAmount       money.Amount        `json:"credit_amount" gorm:"embedded;embeddedPrefix:credit_"`
	FXRate             decimal.NullDecimal `json:"-" gorm:"type:numeric(30,18)"`
	FXFrozenAt         int64               `json:"fx_frozen_at" gorm:"not null"`
	FX                 *CheckoutFX         `json:"fx" gorm:"-"`
	ProductID          string              `json:"product_id,omitempty" gorm:"type:varchar(255);not null"`
	Quantity           string              `json:"quantity,omitempty" gorm:"type:varchar(32);not null"`
	ProviderMode       string              `json:"-" gorm:"type:varchar(16);not null"`
	ProviderStoreID    string              `json:"-" gorm:"type:varchar(255);not null"`
	ProviderMerchantID string              `json:"-" gorm:"type:varchar(255);not null"`
	ProviderSessionID  string              `json:"provider_session_id,omitempty" gorm:"type:varchar(255);not null"`
	PayMethodType      string              `json:"-" gorm:"type:varchar(255);not null"`
	PayMethodName      string              `json:"-" gorm:"type:varchar(255);not null"`
	CreationState      string              `json:"creation_state" gorm:"type:varchar(16);not null"`
	// CheckoutURL never contains the Pancake session-token fragment.
	CheckoutURL          string        `json:"-" gorm:"type:text;not null"`
	PaidAmount           *money.Amount `json:"paid_amount" gorm:"-"`
	PaidAt               int64         `json:"paid_at" gorm:"-"`
	FulfillmentAttemptAt int64         `json:"-" gorm:"not null"`
	FulfillmentPending   bool          `json:"fulfillment_pending" gorm:"-"`
}

// CheckoutPayment is the durable verified payment receipt. Its amount exists
// only after payment confirmation, independently of wallet fulfillment.
type CheckoutPayment struct {
	TradeNo           string       `gorm:"type:varchar(255);primaryKey"`
	Provider          string       `gorm:"type:varchar(50);not null"`
	PaidAmount        money.Amount `gorm:"embedded;embeddedPrefix:paid_"`
	ProviderOrderID   string       `gorm:"type:varchar(255);not null"`
	ProviderSessionID string       `gorm:"type:varchar(255);not null"`
	EventID           string       `gorm:"type:varchar(255);not null"`
	PaidAt            int64        `gorm:"not null"`
	CustomerID        string       `gorm:"type:varchar(255);not null"`
	CustomerEmail     string       `gorm:"type:varchar(320);not null"`
}

// CheckoutExternalPrice is a verified provider product/price snapshot, obtained
// with merchant credentials before a quote is issued, never a local preview.
type CheckoutExternalPrice struct {
	ProductID      string
	UnitAmount     money.Amount
	Final          bool
	Mode           string
	PromotionCodes bool
	TaxMode        string
}

func (order *CheckoutOrder) Quote() CheckoutQuoteResponse {
	var fx *CheckoutFX
	if order.FXRate.Valid {
		fx = &CheckoutFX{FromCurrency: order.SourceAmount.Currency, ToCurrency: order.CreditAmount.Currency, Rate: order.FXRate.Decimal.String()}
	}
	return CheckoutQuoteResponse{QuoteID: order.TradeNo, ExpiresAt: order.ExpiresAt, ChargeAmount: order.ChargeAmount, ChargeFinal: order.ChargeFinal, SourceAmount: order.SourceAmount, CreditAmount: order.CreditAmount, FX: fx}
}

func CheckoutQuantity(selection CheckoutSelection, provider string) (int64, error) {
	text := selection.Quantity
	if text == "" {
		text = "1"
	}
	quantity, err := strconv.ParseInt(text, 10, 64)
	if err != nil || quantity <= 0 || quantity > 10000 || strconv.FormatInt(quantity, 10) != text || (provider == PaymentProviderCreem && quantity != 1) {
		return 0, common.NewMessage("Invalid parameters")
	}
	return quantity, nil
}

// CreateCheckoutQuote validates the complete selection and freezes accounting
// promises before any remote checkout can be created.
func CreateCheckoutQuote(ctx context.Context, userID int, request CheckoutQuoteRequest, external *CheckoutExternalPrice, payMethodType, payMethodName, providerMode, storeID string) (*CheckoutOrder, error) {
	if userID <= 0 {
		return nil, common.NewMessage("User does not exist")
	}
	configuration := operation_setting.GetCheckoutConfig()
	provider, exists := configuration.Providers[request.Provider]
	if !exists || !provider.Enabled || !provider.Configured(request.Provider) || !operation_setting.IsPaymentComplianceConfirmed() {
		return nil, common.NewMessage("Payment configuration error")
	}
	var user User
	if err := DB.WithContext(ctx).First(&user, userID).Error; err != nil || user.Status != common.UserStatusEnabled {
		return nil, common.NewMessage("User does not exist")
	}
	ratio := decimal.NewFromInt(1)
	if text, ok := provider.GroupRatios[user.Group]; ok {
		var err error
		ratio, err = operation_setting.CheckoutRatio(text)
		if err != nil {
			return nil, err
		}
	}
	selection := request.Selection
	var source money.Amount
	var chargeValue money.Value
	var err error
	var quantity string
	final := true
	if request.Provider == PaymentProviderStripe || request.Provider == PaymentProviderCreem {
		if selection.Kind != "product" || selection.SourceAmount != nil || selection.ProductID == "" || external == nil || external.ProductID != selection.ProductID || !operation_setting.ValidCheckoutAmount(external.UnitAmount) {
			return nil, common.NewMessage("Invalid parameters")
		}
		count, err := CheckoutQuantity(selection, request.Provider)
		if err != nil {
			return nil, err
		}
		found := false
		for _, product := range provider.Products {
			if product.ProductID != selection.ProductID {
				continue
			}
			value, err := product.SourceAmount.Value().MulQuantity(count)
			if err != nil {
				return nil, err
			}
			value, err = money.MulUnitPrice(ratio, value)
			if err != nil {
				return nil, err
			}
			source, err = value.Credit()
			if err != nil {
				return nil, err
			}
			found = true
			break
		}
		if !found {
			return nil, common.NewMessage("Product does not exist")
		}
		chargeValue, err = external.UnitAmount.Value().MulQuantity(count)
		if err != nil {
			return nil, err
		}
		quantity = strconv.FormatInt(count, 10)
		final = external.Final
		providerMode = external.Mode
	} else {
		if selection.Kind != "amount" || selection.SourceAmount == nil || selection.ProductID != "" || selection.Quantity != "" || !operation_setting.ValidCheckoutAmount(*selection.SourceAmount) || selection.SourceAmount.Currency != provider.SourceUnit.Currency || selection.SourceAmount.Nanos < provider.Minimum.Nanos {
			return nil, common.NewMessage("Invalid amount")
		}
		source = *selection.SourceAmount
		factor := new(big.Rat).SetFrac(big.NewInt(source.Nanos), big.NewInt(provider.SourceUnit.Nanos))
		factor.Mul(factor, ratio.Rat())
		for _, discount := range provider.Discounts {
			if discount.SourceAmount == source {
				discountRatio, err := operation_setting.CheckoutRatio(discount.Ratio)
				if err != nil {
					return nil, err
				}
				factor.Mul(factor, discountRatio.Rat())
				break
			}
		}
		factor.Mul(factor, new(big.Rat).SetFrac(big.NewInt(provider.ChargeUnit.Nanos), big.NewInt(1_000_000_000)))
		chargeValue, err = money.NewRationalValue(factor, provider.ChargeUnit.Currency)
		if err != nil {
			return nil, err
		}
		if request.Provider == PaymentProviderWaffoPancake {
			final = false
		}
	}
	if !operation_setting.ValidCheckoutAmount(source) {
		return nil, common.NewMessage("Invalid amount")
	}
	charge, err := QuantizeCheckoutCharge(chargeValue, request.Provider)
	if err != nil || charge.Nanos <= 0 {
		return nil, common.NewMessage("Invalid amount")
	}
	now := clock.Now().Unix()
	order := &CheckoutOrder{TradeNo: "CHECKOUT-" + rand.Text(), ExpiresAt: now + 15*60, ChargeAmount: charge, ChargeFinal: final, SourceAmount: source, FXFrozenAt: now, ProductID: selection.ProductID, Quantity: quantity, ProviderMode: providerMode, ProviderStoreID: storeID, PayMethodType: payMethodType, PayMethodName: payMethodName, CreationState: "quoted"}
	if request.Provider == PaymentProviderWaffoPancake {
		order.ProductID = setting.WaffoPancakeProductID
		order.ProviderMerchantID = setting.WaffoPancakeMerchantID
	}
	if external != nil {
		order.UnitChargeAmount = external.UnitAmount
		order.PromotionCodes = external.PromotionCodes
		order.TaxMode = external.TaxMode
	} else {
		order.UnitChargeAmount = *provider.ChargeUnit
	}
	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		creditValue := source.Value()
		if source.Currency != SettlementCurrency() {
			rate, err := GetAccountingFXRate(ctx, tx, source.Currency, SettlementCurrency())
			if err != nil {
				return err
			}
			creditValue, err = creditValue.Convert(rate)
			if err != nil {
				return err
			}
			order.FXRate = decimal.NewNullDecimal(rate.Decimal())
		}
		order.CreditAmount, err = creditValue.Credit()
		if err != nil || order.CreditAmount.Nanos <= 0 {
			return common.NewMessage("Invalid amount")
		}
		topUp := &TopUp{UserId: userID, TradeNo: order.TradeNo, PaymentMethod: request.PaymentMethod, PaymentProvider: request.Provider, CreateTime: now, Status: CheckoutStatusQuoted, ContractVersion: CheckoutContractVersion, Purpose: CheckoutPurposeWallet}
		if err := tx.Omit("Amount", "Money").Create(topUp).Error; err != nil {
			return err
		}
		return tx.Create(order).Error
	})
	return order, err
}

// CheckoutCurrencyDigits describes provider wire units, independently of display.
func CheckoutCurrencyDigits(provider string, currency money.Currency) int {
	if provider == PaymentProviderStripe {
		switch currency {
		case "BIF", "CLP", "DJF", "GNF", "JPY", "KMF", "KRW", "MGA", "PYG", "RWF", "VND", "VUV", "XAF", "XOF", "XPF":
			return 0
		}
	}
	if provider == PaymentProviderWaffo {
		switch currency {
		case "IDR", "JPY", "KRW", "VND":
			return 0
		}
	}
	return 2
}

// QuantizeCheckoutCharge rounds up once to the provider's payable minor unit.
func QuantizeCheckoutCharge(value money.Value, provider string) (money.Amount, error) {
	amount, err := value.Receivable()
	if err != nil || amount.Nanos <= 0 {
		return money.Amount{}, common.NewMessage("Invalid amount")
	}
	quantum := int64(10_000_000)
	if CheckoutCurrencyDigits(provider, amount.Currency) == 0 {
		quantum = 1_000_000_000
	}
	whole, remainder := amount.Nanos/quantum, amount.Nanos%quantum
	if remainder != 0 {
		whole++
	}
	quantized := new(big.Rat).Mul(big.NewRat(whole, 1), big.NewRat(quantum, 1_000_000_000))
	result, err := money.NewRationalValue(quantized, amount.Currency)
	if err != nil {
		return money.Amount{}, err
	}
	return result.Receivable()
}

func CheckoutAmountString(amount money.Amount, provider string) string {
	digits := CheckoutCurrencyDigits(provider, amount.Currency)
	if digits == 0 {
		return strconv.FormatInt(amount.Nanos/1_000_000_000, 10)
	}
	return strconv.FormatInt(amount.Nanos/1_000_000_000, 10) + "." + strconv.FormatInt(100+amount.Nanos%1_000_000_000/10_000_000, 10)[1:]
}

func AcceptCheckoutQuote(ctx context.Context, userID int, provider, quoteID string) (*TopUp, *CheckoutOrder, bool, error) {
	var topUp TopUp
	var order CheckoutOrder
	claimed := false
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).Where("trade_no = ? AND user_id = ?", quoteID, userID).First(&topUp).Error; err != nil {
			return common.NewMessage("Top-up order does not exist")
		}
		if topUp.ContractVersion != CheckoutContractVersion || topUp.Purpose != CheckoutPurposeWallet || topUp.PaymentProvider != provider {
			return ErrPaymentMethodMismatch
		}
		var user User
		if err := tx.First(&user, userID).Error; err != nil || user.Status != common.UserStatusEnabled {
			return common.NewMessage("User does not exist")
		}
		if err := lockForUpdate(tx).First(&order, "trade_no = ?", quoteID).Error; err != nil {
			return err
		}
		if order.CreationState == "quoted" {
			if order.ExpiresAt <= clock.Now().Unix() || topUp.Status != CheckoutStatusQuoted {
				return common.NewMessage("Invalid parameters")
			}
			order.AcceptedAt = clock.Now().Unix()
			order.CreationState = "creating"
			topUp.Status = common.TopUpStatusPending
			if err := tx.Model(&topUp).Update("status", topUp.Status).Error; err != nil {
				return err
			}
			claimed = true
			return tx.Model(&order).Updates(map[string]any{"accepted_at": order.AcceptedAt, "creation_state": order.CreationState}).Error
		}
		if topUp.Status != common.TopUpStatusPending && topUp.Status != common.TopUpStatusSuccess {
			return ErrTopUpStatusInvalid
		}
		return nil
	})
	return &topUp, &order, claimed, err
}

func GetCheckoutOrder(ctx context.Context, tradeNo string) (*CheckoutOrder, error) {
	var order CheckoutOrder
	err := DB.WithContext(ctx).First(&order, "trade_no = ?", tradeNo).Error
	return &order, err
}

// MarkCheckoutCreationUncertain preserves an accepted obligation after any remote
// failure. The same quote can never claim a second remote creation.
func MarkCheckoutCreationUncertain(ctx context.Context, tradeNo string) error {
	return DB.WithContext(ctx).Model(&CheckoutOrder{}).Where("trade_no = ? AND creation_state = ?", tradeNo, "creating").Update("creation_state", "uncertain").Error
}

func SaveCheckoutRemoteIdentity(ctx context.Context, tradeNo, sessionID, checkoutURL string) error {
	if index := strings.IndexByte(checkoutURL, '#'); index >= 0 {
		checkoutURL = checkoutURL[:index]
	}
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var order CheckoutOrder
		if err := lockForUpdate(tx).First(&order, "trade_no = ?", tradeNo).Error; err != nil {
			return err
		}
		if order.ProviderSessionID != "" && order.ProviderSessionID != sessionID {
			return ErrPaymentMethodMismatch
		}
		return tx.Model(&order).Updates(map[string]any{"provider_session_id": sessionID, "checkout_url": checkoutURL, "creation_state": "created"}).Error
	})
}

// ConfirmCheckoutPayment persists the authenticated receipt before fulfillment.
// A failed wallet transaction cannot erase the fact that payment was received.
func ConfirmCheckoutPayment(ctx context.Context, provider string, receipt CheckoutPayment) error {
	if receipt.TradeNo == "" || receipt.ProviderOrderID == "" || receipt.PaidAmount.Nanos < 0 || len(receipt.EventID) > 255 || len(receipt.ProviderOrderID) > 255 || len(receipt.ProviderSessionID) > 255 || len(receipt.CustomerID) > 255 || len(receipt.CustomerEmail) > 320 {
		return ErrLedgerInvalidEntry
	}
	if _, err := money.ParseCurrency(string(receipt.PaidAmount.Currency)); err != nil {
		return err
	}
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var topUp TopUp
		if err := lockForUpdate(tx).First(&topUp, "trade_no = ?", receipt.TradeNo).Error; err != nil {
			return err
		}
		if topUp.ContractVersion != CheckoutContractVersion || topUp.Purpose != CheckoutPurposeWallet || topUp.PaymentProvider != provider {
			return ErrPaymentMethodMismatch
		}
		if topUp.Status != common.TopUpStatusPending && topUp.Status != common.TopUpStatusSuccess && topUp.Status != common.TopUpStatusFailed && topUp.Status != common.TopUpStatusExpired {
			return ErrTopUpStatusInvalid
		}
		var order CheckoutOrder
		if err := lockForUpdate(tx).First(&order, "trade_no = ?", receipt.TradeNo).Error; err != nil {
			return err
		}
		if order.CreationState == "quoted" || receipt.PaidAmount.Currency != order.ChargeAmount.Currency || (order.ChargeFinal && receipt.PaidAmount != order.ChargeAmount) || (order.ProviderSessionID != "" && receipt.ProviderSessionID != order.ProviderSessionID) {
			return ErrPaymentMethodMismatch
		}
		var existing CheckoutPayment
		if err := tx.First(&existing, "trade_no = ?", receipt.TradeNo).Error; err == nil {
			if existing.PaidAmount != receipt.PaidAmount || existing.ProviderOrderID != receipt.ProviderOrderID || existing.ProviderSessionID != receipt.ProviderSessionID {
				return ErrPaymentMethodMismatch
			}
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if order.ProviderSessionID == "" && receipt.ProviderSessionID != "" {
			if err := tx.Model(&order).Updates(map[string]any{"provider_session_id": receipt.ProviderSessionID, "creation_state": "created"}).Error; err != nil {
				return err
			}
		}
		if topUp.Status != common.TopUpStatusPending && topUp.Status != common.TopUpStatusSuccess {
			if err := tx.Model(&topUp).Update("status", common.TopUpStatusPending).Error; err != nil {
				return err
			}
		}
		receipt.Provider = provider
		receipt.PaidAt = clock.Now().Unix()
		return tx.Create(&receipt).Error
	})
}

func CompleteCheckoutPayment(ctx context.Context, tradeNo string) error {
	var topUp TopUp
	completed := false
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).First(&topUp, "trade_no = ?", tradeNo).Error; err != nil {
			return err
		}
		if topUp.ContractVersion != CheckoutContractVersion || topUp.Purpose != CheckoutPurposeWallet {
			return ErrPaymentMethodMismatch
		}
		if topUp.Status == common.TopUpStatusSuccess {
			return nil
		}
		if topUp.Status != common.TopUpStatusPending {
			return ErrTopUpStatusInvalid
		}
		var receipt CheckoutPayment
		if err := tx.First(&receipt, "trade_no = ?", tradeNo).Error; err != nil {
			return err
		}
		if err := creditPaymentTopUpWallet(tx, &topUp); err != nil {
			return err
		}
		if err := tx.Model(&topUp).Updates(map[string]any{"status": common.TopUpStatusSuccess, "complete_time": clock.Now().Unix()}).Error; err != nil {
			return err
		}
		completed = true
		return nil
	})
	if err == nil && completed {
		RecordTopupLog(topUp.UserId, common.NewMessage("Online top-up succeeded"), "", topUp.PaymentMethod, topUp.PaymentProvider)
	}
	return err
}

// postCheckoutWalletCredit composes the frozen promise with the existing ledger
// transaction. Callers hold the parent order lock; no current pricing or FX is read.
func postCheckoutWalletCredit(tx *gorm.DB, topUp *TopUp) error {
	if topUp.ContractVersion != CheckoutContractVersion || topUp.Purpose != CheckoutPurposeWallet {
		return ErrPaymentMethodMismatch
	}
	var order CheckoutOrder
	if err := tx.First(&order, "trade_no = ?", topUp.TradeNo).Error; err != nil {
		return err
	}
	if order.CreationState == "quoted" || !operation_setting.ValidCheckoutAmount(order.SourceAmount) || !operation_setting.ValidCheckoutAmount(order.CreditAmount) || order.CreditAmount.Currency != SettlementCurrency() {
		return ErrLedgerInvalidEntry
	}
	value := order.SourceAmount.Value()
	if order.SourceAmount.Currency != order.CreditAmount.Currency {
		if !order.FXRate.Valid {
			return ErrLedgerRateRequired
		}
		rate, err := money.NewRate(order.SourceAmount.Currency, order.CreditAmount.Currency, order.FXRate.Decimal)
		if err != nil {
			return err
		}
		value, err = value.Convert(rate)
		if err != nil {
			return err
		}
	} else if order.FXRate.Valid {
		return ErrLedgerInvalidEntry
	}
	credit, err := value.Credit()
	if err != nil || credit != order.CreditAmount {
		return ErrLedgerInvalidEntry
	}
	var user User
	if err := tx.Select("id").First(&user, topUp.UserId).Error; err != nil {
		return err
	}
	posting := LedgerPosting{Account: UserWalletAccount(topUp.UserId), Type: LedgerEntryTopUp, Amount: order.CreditAmount, IdempotencyKey: fmt.Sprintf("top_up:payment:%x", sha256.Sum256([]byte(topUp.TradeNo))), Remark: "Payment top-up completed", sourceAmount: &order.SourceAmount, fxRate: order.FXRate}
	if err := validateLedgerPosting(posting); err != nil {
		return err
	}
	_, err = postLedgerEntries(tx, []LedgerPosting{posting})
	return err
}

// hydrateTopUpHistory preserves legacy values as text and classifies subscription
// history by its real order identity, never by a zero monetary sentinel.
func hydrateTopUpHistory(tx *gorm.DB, topUps []*TopUp) error {
	if len(topUps) == 0 {
		return nil
	}
	tradeNos := make([]string, 0, len(topUps))
	exactTradeNos := make([]string, 0, len(topUps))
	for _, topUp := range topUps {
		tradeNos = append(tradeNos, topUp.TradeNo)
		if topUp.ContractVersion == CheckoutContractVersion {
			exactTradeNos = append(exactTradeNos, topUp.TradeNo)
		}
	}
	var legacy []struct {
		TradeNo      string
		LegacyAmount *string
		LegacyMoney  *float64
	}
	if err := tx.Model(&TopUp{}).Select("trade_no, amount::text AS legacy_amount, money AS legacy_money").Where("trade_no IN ? AND contract_version = 0", tradeNos).Find(&legacy).Error; err != nil {
		return err
	}
	legacyByTradeNo := make(map[string]int, len(legacy))
	for i := range legacy {
		legacyByTradeNo[legacy[i].TradeNo] = i
	}
	var subscriptions []SubscriptionOrder
	if err := tx.Select("trade_no").Where("trade_no IN ?", tradeNos).Find(&subscriptions).Error; err != nil {
		return err
	}
	subscriptionByTradeNo := make(map[string]bool, len(subscriptions))
	for _, subscription := range subscriptions {
		subscriptionByTradeNo[subscription.TradeNo] = true
	}
	var orders []CheckoutOrder
	var receipts []CheckoutPayment
	if len(exactTradeNos) > 0 {
		if err := tx.Where("trade_no IN ?", exactTradeNos).Find(&orders).Error; err != nil {
			return err
		}
		if err := tx.Where("trade_no IN ?", exactTradeNos).Find(&receipts).Error; err != nil {
			return err
		}
	}
	orderByTradeNo := make(map[string]*CheckoutOrder, len(orders))
	for i := range orders {
		orders[i].FX = orders[i].Quote().FX
		orderByTradeNo[orders[i].TradeNo] = &orders[i]
	}
	receiptByTradeNo := make(map[string]CheckoutPayment, len(receipts))
	for _, receipt := range receipts {
		receiptByTradeNo[receipt.TradeNo] = receipt
	}
	for _, topUp := range topUps {
		if topUp.ContractVersion == CheckoutContractVersion {
			order := orderByTradeNo[topUp.TradeNo]
			if order == nil {
				return ErrTopUpNotFound
			}
			if receipt, paid := receiptByTradeNo[topUp.TradeNo]; paid {
				order.PaidAmount, order.PaidAt = &receipt.PaidAmount, receipt.PaidAt
				order.FulfillmentPending = topUp.Status != common.TopUpStatusSuccess
			}
			topUp.Checkout = order
			continue
		}
		topUp.Purpose = "legacy_topup"
		if subscriptionByTradeNo[topUp.TradeNo] {
			topUp.Purpose = "subscription_history"
		}
		if index, ok := legacyByTradeNo[topUp.TradeNo]; ok {
			topUp.LegacyAmount = legacy[index].LegacyAmount
			if legacy[index].LegacyMoney != nil {
				text := strconv.FormatFloat(*legacy[index].LegacyMoney, 'f', -1, 64)
				topUp.LegacyMoney = &text
			}
		}
	}
	return nil
}

func RecoverCheckoutPayments(ctx context.Context, limit int) (int, error) {
	var receipts []CheckoutPayment
	if err := DB.WithContext(ctx).Table("checkout_payments AS payment").Select("payment.*").Joins("JOIN top_ups AS topup ON topup.trade_no = payment.trade_no").Joins("JOIN checkout_orders AS checkout ON checkout.trade_no = payment.trade_no").Where("topup.contract_version = ? AND topup.purpose = ? AND topup.status = ?", CheckoutContractVersion, CheckoutPurposeWallet, common.TopUpStatusPending).Order("checkout.fulfillment_attempt_at, payment.paid_at, payment.trade_no").Limit(min(max(limit, 1), 1000)).Find(&receipts).Error; err != nil {
		return 0, err
	}
	completed := 0
	var failures []error
	for _, receipt := range receipts {
		if err := DB.WithContext(ctx).Model(&CheckoutOrder{}).Where("trade_no = ?", receipt.TradeNo).Update("fulfillment_attempt_at", clock.Now().Unix()).Error; err != nil {
			failures = append(failures, err)
			continue
		}
		if err := CompleteCheckoutPayment(ctx, receipt.TradeNo); err != nil {
			failures = append(failures, err)
		} else {
			completed++
		}
	}
	return completed, errors.Join(failures...)
}
