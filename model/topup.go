package model

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"math/big"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/money"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

type TopUp struct {
	Id              int            `json:"id"`
	UserId          int            `json:"user_id" gorm:"index"`
	Amount          int64          `json:"amount"`
	Money           float64        `json:"money"`
	TradeNo         string         `json:"trade_no" gorm:"unique;type:varchar(255);index"`
	PaymentMethod   string         `json:"payment_method" gorm:"type:varchar(50)"`
	PaymentProvider string         `json:"payment_provider" gorm:"type:varchar(50);default:''"`
	CreateTime      int64          `json:"create_time"`
	CompleteTime    int64          `json:"complete_time"`
	Status          string         `json:"status"`
	LegacyAmount    *string        `json:"legacy_amount" gorm:"-"`
	LegacyMoney     *string        `json:"legacy_money" gorm:"-"`
	ContractVersion int            `json:"contract_version" gorm:"not null;default:0"`
	Purpose         string         `json:"purpose" gorm:"type:varchar(32);not null;default:''"`
	Checkout        *CheckoutOrder `json:"checkout,omitempty" gorm:"-"`
}

const (
	PaymentMethodStripe       = "stripe"
	PaymentMethodCreem        = "creem"
	PaymentMethodWaffo        = "waffo"
	PaymentMethodWaffoPancake = "waffo_pancake"
	PaymentMethodBalance      = "balance"
)

const (
	PaymentProviderEpay         = "epay"
	PaymentProviderStripe       = "stripe"
	PaymentProviderCreem        = "creem"
	PaymentProviderWaffo        = "waffo"
	PaymentProviderWaffoPancake = "waffo_pancake"
	PaymentProviderBalance      = "balance"
)

var (
	ErrPaymentMethodMismatch = errors.New("payment method mismatch")
	ErrTopUpNotFound         = errors.New("topup not found")
	ErrTopUpStatusInvalid    = errors.New("topup status invalid")
	// The quota errors reach the web console, so they carry message keys.
	ErrInvalidTopUpQuota        error = common.NewMessage("Invalid top-up quota")
	ErrTopUpQuotaLimitExceeded  error = common.NewMessage("Top-up quota limit exceeded")
	ErrWalletQuotaLimitExceeded error = common.NewMessage("Wallet quota limit exceeded")
)

// MarshalJSON keeps nonfinite historical floats representable as unknown cash
// while LegacyMoney retains their original text. New checkout cash is projected
// exclusively from its exact facts and verified receipt.
func (topUp TopUp) MarshalJSON() ([]byte, error) {
	type plain TopUp
	var legacyMoney *float64
	if topUp.ContractVersion == 0 && !math.IsNaN(topUp.Money) && !math.IsInf(topUp.Money, 0) {
		legacyMoney = &topUp.Money
	}
	return common.Marshal(struct {
		plain
		Money *float64 `json:"money"`
	}{plain: plain(topUp), Money: legacyMoney})
}

func (topUp *TopUp) Insert() error {
	var err error
	err = DB.Create(topUp).Error
	return err
}

// ValidateTopUpQuotaCapacity retains the legacy pre-payment quota check for
// checkout compatibility. Payment completion enforces the money wallet range.

// creditPaymentTopUpWallet decodes historical order amounts only at payment
// completion. It posts within the order transaction, including debt repayment.
func creditPaymentTopUpWallet(tx *gorm.DB, topUp *TopUp) error {
	if topUp.ContractVersion == CheckoutContractVersion {
		var receipt CheckoutPayment
		if err := tx.First(&receipt, "trade_no = ?", topUp.TradeNo).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return common.NewMessage("Payment has not been confirmed")
			}
			return err
		}
		if err := postCheckoutWalletCredit(tx, topUp); err != nil {
			return err
		}
		if topUp.PaymentProvider == PaymentProviderStripe && receipt.CustomerID != "" {
			return tx.Model(&User{}).Where("id = ?", topUp.UserId).Update("stripe_customer", receipt.CustomerID).Error
		}
		if topUp.PaymentProvider == PaymentProviderCreem && receipt.CustomerEmail != "" {
			return tx.Model(&User{}).Where("id = ? AND (email = '' OR email IS NULL)", topUp.UserId).Update("email", receipt.CustomerEmail).Error
		}
		return nil
	}
	if topUp.ContractVersion != 0 {
		return ErrLedgerInvalidEntry
	}
	// Subscription history never funds a wallet, even if a historical row has
	// a nonzero amount. The associated subscription order defines its purpose.
	var subscriptions int64
	if err := tx.Model(&SubscriptionOrder{}).Where("trade_no = ?", topUp.TradeNo).Count(&subscriptions).Error; err != nil {
		return err
	}
	if subscriptions != 0 || topUp.Amount <= 0 {
		return ErrLedgerInvalidEntry
	}
	var sourceValue money.Value
	var err error
	switch topUp.PaymentProvider {
	case PaymentProviderStripe:
		if topUp.Money <= 0 || math.IsNaN(topUp.Money) || math.IsInf(topUp.Money, 0) {
			return ErrLedgerInvalidEntry
		}
		// Money is a legacy stored float. Decode it once; all later arithmetic
		// stays exact and rounds only when writing monetary amounts.
		sourceValue, err = money.NewValue(decimal.NewFromFloat(topUp.Money), "USD")
	case PaymentProviderCreem:
		// Historical Creem Amount counts quota at the fixed original scale.
		sourceValue, err = money.NewRationalValue(big.NewRat(topUp.Amount, 500000), "USD")
	default:
		sourceValue, err = money.NewValue(decimal.NewFromInt(topUp.Amount), "USD")
	}
	if err != nil {
		return err
	}
	source, err := sourceValue.Credit()
	if err != nil {
		return err
	}
	if source.Nanos <= 0 {
		return ErrLedgerInvalidEntry
	}
	creditValue := sourceValue
	var fx decimal.NullDecimal
	if SettlementCurrency() != source.Currency {
		rate, err := GetAccountingFXRate(tx.Statement.Context, tx, source.Currency, SettlementCurrency())
		if err != nil {
			return err
		}
		creditValue, err = sourceValue.Convert(rate)
		if err != nil {
			return err
		}
		fx = decimal.NewNullDecimal(rate.Decimal())
	}
	amount, err := creditValue.Credit()
	if err != nil {
		return err
	}
	posting := LedgerPosting{
		Account:        UserWalletAccount(topUp.UserId),
		Type:           LedgerEntryTopUp,
		Amount:         amount,
		IdempotencyKey: fmt.Sprintf("top_up:payment:%x", sha256.Sum256([]byte(topUp.TradeNo))),
		Remark:         "Payment top-up completed",
		sourceAmount:   &source,
		fxRate:         fx,
	}
	if err := validateLedgerPosting(posting); err != nil {
		return err
	}
	// Preserve the legacy completion contract: a missing user cannot receive
	// an orphan wallet credit or a successful order.
	var user User
	if err := tx.Select("id").First(&user, topUp.UserId).Error; err != nil {
		return err
	}
	_, err = postLedgerEntries(tx, []LedgerPosting{posting})
	return err
}

func (topUp *TopUp) Update() error {
	var err error
	err = DB.Omit("Amount", "Money").Save(topUp).Error
	return err
}

func GetTopUpById(id int) *TopUp {
	var topUp *TopUp
	var err error
	err = DB.Where("id = ?", id).First(&topUp).Error
	if err != nil {
		return nil
	}
	return topUp
}

func GetTopUpByTradeNo(tradeNo string) *TopUp {
	var topUp *TopUp
	var err error
	err = DB.Where("trade_no = ?", tradeNo).First(&topUp).Error
	if err != nil {
		return nil
	}
	return topUp
}

func UpdatePendingTopUpStatus(tradeNo string, expectedPaymentProvider string, targetStatus string) error {
	if tradeNo == "" {
		return errors.New("payment order number not provided")
	}

	return DB.Transaction(func(tx *gorm.DB) error {
		topUp := &TopUp{}
		if err := lockForUpdate(tx).Where(`"trade_no" = ?`, tradeNo).First(topUp).Error; err != nil {
			return ErrTopUpNotFound
		}
		if expectedPaymentProvider != "" && topUp.PaymentProvider != expectedPaymentProvider {
			return ErrPaymentMethodMismatch
		}
		if topUp.Status != common.TopUpStatusPending {
			return ErrTopUpStatusInvalid
		}
		if topUp.ContractVersion == CheckoutContractVersion {
			var paid int64
			if err := tx.Model(&CheckoutPayment{}).Where("trade_no = ?", tradeNo).Count(&paid).Error; err != nil {
				return err
			}
			if paid != 0 {
				return ErrTopUpStatusInvalid
			}
		}

		topUp.Status = targetStatus
		return tx.Omit("Amount", "Money").Save(topUp).Error
	})
}

// RechargeEpay completes the order and credits its wallet in one transaction.
// The order row lock prevents duplicate credits across callbacks and instances.
// alreadyDone is true when a previous callback completed the order.
func RechargeEpay(tradeNo string, actualPaymentMethod string, callerIp string) (alreadyDone bool, err error) {
	if tradeNo == "" {
		return false, errors.New("payment order number not provided")
	}

	topUp := &TopUp{}
	err = DB.Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).Where(`"trade_no" = ?`, tradeNo).First(topUp).Error; err != nil {
			return ErrTopUpNotFound
		}
		if topUp.PaymentProvider != PaymentProviderEpay {
			return ErrPaymentMethodMismatch
		}
		if topUp.Status == common.TopUpStatusSuccess {
			alreadyDone = true
			return nil
		}
		if topUp.Status != common.TopUpStatusPending {
			return ErrTopUpStatusInvalid
		}
		if actualPaymentMethod != "" && topUp.PaymentMethod != actualPaymentMethod {
			topUp.PaymentMethod = actualPaymentMethod
		}
		topUp.CompleteTime = common.GetTimestamp()
		topUp.Status = common.TopUpStatusSuccess
		if err := tx.Omit("Amount", "Money").Save(topUp).Error; err != nil {
			return err
		}
		return creditPaymentTopUpWallet(tx, topUp)
	})
	if err != nil {
		if !errors.Is(err, ErrTopUpNotFound) && !errors.Is(err, ErrPaymentMethodMismatch) && !errors.Is(err, ErrTopUpStatusInvalid) {
			common.SysError(common.LogText("epay topup failed: %s", err.Error()))
		}
		return false, err
	}
	if alreadyDone {
		return true, nil
	}
	RecordTopupLog(topUp.UserId, common.NewMessage("Online top-up succeeded"), callerIp, topUp.PaymentMethod, PaymentProviderEpay)
	return false, nil
}

func Recharge(referenceId string, customerId string, callerIp string) (err error) {
	if referenceId == "" {
		return errors.New("payment order number not provided")
	}

	topUp := &TopUp{}

	err = DB.Transaction(func(tx *gorm.DB) error {
		err := lockForUpdate(tx).Where(`"trade_no" = ?`, referenceId).First(topUp).Error
		if err != nil {
			return errors.New("top-up order not found")
		}

		if topUp.PaymentProvider != PaymentProviderStripe {
			return ErrPaymentMethodMismatch
		}

		if topUp.Status != common.TopUpStatusPending {
			return errors.New("invalid top-up order status")
		}

		topUp.CompleteTime = common.GetTimestamp()
		topUp.Status = common.TopUpStatusSuccess
		err = tx.Omit("Amount", "Money").Save(topUp).Error
		if err != nil {
			return err
		}

		if err := creditPaymentTopUpWallet(tx, topUp); err != nil {
			return err
		}
		return tx.Model(&User{}).Where("id = ?", topUp.UserId).Update("stripe_customer", customerId).Error
	})

	if err != nil {
		common.SysError(common.LogText("topup failed: %s", err.Error()))
		return errors.New("top-up failed")
	}
	RecordTopupLog(topUp.UserId, common.NewMessage("Online top-up succeeded"), callerIp, topUp.PaymentMethod, PaymentMethodStripe)

	return nil
}

// topUpQueryWindowSeconds 限制充值记录查询的时间窗口（秒）。
const topUpQueryWindowSeconds int64 = 30 * 24 * 60 * 60

// topUpQueryCutoff 返回允许查询的最早 create_time（秒级 Unix 时间戳）。
func topUpQueryCutoff() int64 {
	return common.GetTimestamp() - topUpQueryWindowSeconds
}

func GetUserTopUps(userId int, pageInfo *common.PageInfo) (topups []*TopUp, total int64, err error) {
	// Start transaction
	tx := DB.Begin()
	if tx.Error != nil {
		return nil, 0, tx.Error
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	cutoff := topUpQueryCutoff()

	// Get total count within transaction
	err = tx.Model(&TopUp{}).Where("user_id = ? AND create_time >= ?", userId, cutoff).Count(&total).Error
	if err != nil {
		tx.Rollback()
		return nil, 0, err
	}

	// Get paginated topups within same transaction
	err = tx.Where("user_id = ? AND create_time >= ?", userId, cutoff).Order("id desc").Limit(pageInfo.GetPageSize()).Offset(pageInfo.GetStartIdx()).Find(&topups).Error
	if err != nil {
		tx.Rollback()
		return nil, 0, err
	}

	// Commit transaction
	if err = hydrateTopUpHistory(tx, topups); err != nil {
		tx.Rollback()
		return nil, 0, err
	}
	if err = tx.Commit().Error; err != nil {
		return nil, 0, err
	}

	return topups, total, nil
}

// GetAllTopUps 获取全平台的充值记录（管理员使用，不限制时间窗口）
func GetAllTopUps(pageInfo *common.PageInfo) (topups []*TopUp, total int64, err error) {
	tx := DB.Begin()
	if tx.Error != nil {
		return nil, 0, tx.Error
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	if err = tx.Model(&TopUp{}).Count(&total).Error; err != nil {
		tx.Rollback()
		return nil, 0, err
	}

	if err = tx.Order("id desc").Limit(pageInfo.GetPageSize()).Offset(pageInfo.GetStartIdx()).Find(&topups).Error; err != nil {
		tx.Rollback()
		return nil, 0, err
	}

	if err = hydrateTopUpHistory(tx, topups); err != nil {
		tx.Rollback()
		return nil, 0, err
	}
	if err = tx.Commit().Error; err != nil {
		return nil, 0, err
	}

	return topups, total, nil
}

// searchTopUpCountHardLimit 搜索充值记录时 COUNT 的安全上限，
// 防止对超大表执行无界 COUNT 触发 DoS。
const searchTopUpCountHardLimit = 10000

// SearchUserTopUps 按订单号搜索某用户的充值记录
func SearchUserTopUps(userId int, keyword string, pageInfo *common.PageInfo) (topups []*TopUp, total int64, err error) {
	tx := DB.Begin()
	if tx.Error != nil {
		return nil, 0, tx.Error
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	query := tx.Model(&TopUp{}).Where("user_id = ? AND create_time >= ?", userId, topUpQueryCutoff())
	if keyword != "" {
		pattern, perr := sanitizeLikePattern(keyword)
		if perr != nil {
			tx.Rollback()
			return nil, 0, perr
		}
		query = query.Where("trade_no LIKE ? ESCAPE '!'", pattern)
	}

	if err = query.Limit(searchTopUpCountHardLimit).Count(&total).Error; err != nil {
		tx.Rollback()
		common.SysError(common.LogText("failed to count search topups: %s", err.Error()))
		return nil, 0, common.NewMessage("Failed to search top-up records")
	}

	if err = query.Order("id desc").Limit(pageInfo.GetPageSize()).Offset(pageInfo.GetStartIdx()).Find(&topups).Error; err != nil {
		tx.Rollback()
		common.SysError(common.LogText("failed to search topups: %s", err.Error()))
		return nil, 0, common.NewMessage("Failed to search top-up records")
	}

	if err = hydrateTopUpHistory(tx, topups); err != nil {
		tx.Rollback()
		return nil, 0, err
	}
	if err = tx.Commit().Error; err != nil {
		return nil, 0, err
	}
	return topups, total, nil
}

// SearchAllTopUps 按订单号搜索全平台充值记录（管理员使用，不限制时间窗口）
func SearchAllTopUps(keyword string, pageInfo *common.PageInfo) (topups []*TopUp, total int64, err error) {
	tx := DB.Begin()
	if tx.Error != nil {
		return nil, 0, tx.Error
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	query := tx.Model(&TopUp{})
	if keyword != "" {
		pattern, perr := sanitizeLikePattern(keyword)
		if perr != nil {
			tx.Rollback()
			return nil, 0, perr
		}
		query = query.Where("trade_no LIKE ? ESCAPE '!'", pattern)
	}

	if err = query.Limit(searchTopUpCountHardLimit).Count(&total).Error; err != nil {
		tx.Rollback()
		common.SysError(common.LogText("failed to count search topups: %s", err.Error()))
		return nil, 0, common.NewMessage("Failed to search top-up records")
	}

	if err = query.Order("id desc").Limit(pageInfo.GetPageSize()).Offset(pageInfo.GetStartIdx()).Find(&topups).Error; err != nil {
		tx.Rollback()
		common.SysError(common.LogText("failed to search topups: %s", err.Error()))
		return nil, 0, common.NewMessage("Failed to search top-up records")
	}

	if err = hydrateTopUpHistory(tx, topups); err != nil {
		tx.Rollback()
		return nil, 0, err
	}
	if err = tx.Commit().Error; err != nil {
		return nil, 0, err
	}
	return topups, total, nil
}

// ManualCompleteTopUp 管理员手动完成订单并给用户充值
func ManualCompleteTopUp(tradeNo string, callerIp string) error {
	if tradeNo == "" {
		return common.NewMessage("Order number not provided")
	}

	topUp := &TopUp{}
	completed := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		// 行级锁，避免并发补单
		if err := lockForUpdate(tx).Where(`"trade_no" = ?`, tradeNo).First(topUp).Error; err != nil {
			return common.NewMessage("Top-up order does not exist")
		}

		// 幂等处理：已成功直接返回
		if topUp.Status == common.TopUpStatusSuccess {
			return nil
		}

		if topUp.Status != common.TopUpStatusPending {
			return common.NewMessage("Order is not pending payment and cannot be completed")
		}

		// 标记完成
		topUp.CompleteTime = common.GetTimestamp()
		topUp.Status = common.TopUpStatusSuccess
		if err := tx.Omit("Amount", "Money").Save(topUp).Error; err != nil {
			return err
		}

		if err := creditPaymentTopUpWallet(tx, topUp); err != nil {
			return err
		}
		completed = true
		return nil
	})

	if err != nil {
		return err
	}

	if completed {
		RecordTopupLog(topUp.UserId, common.NewMessage("Administrator completed the top-up order"), callerIp, topUp.PaymentMethod, "admin")
	}
	return nil
}
func RechargeCreem(referenceId string, customerEmail string, customerName string, callerIp string) (err error) {
	if referenceId == "" {
		return errors.New("payment order number not provided")
	}

	topUp := &TopUp{}

	err = DB.Transaction(func(tx *gorm.DB) error {
		err := lockForUpdate(tx).Where(`"trade_no" = ?`, referenceId).First(topUp).Error
		if err != nil {
			return errors.New("top-up order not found")
		}

		if topUp.PaymentProvider != PaymentProviderCreem {
			return ErrPaymentMethodMismatch
		}

		if topUp.Status != common.TopUpStatusPending {
			return errors.New("invalid top-up order status")
		}

		topUp.CompleteTime = common.GetTimestamp()
		topUp.Status = common.TopUpStatusSuccess
		err = tx.Omit("Amount", "Money").Save(topUp).Error
		if err != nil {
			return err
		}

		if err := creditPaymentTopUpWallet(tx, topUp); err != nil {
			return err
		}
		if customerEmail != "" {
			return tx.Model(&User{}).Where("id = ? AND (email = '' OR email IS NULL)", topUp.UserId).
				Update("email", customerEmail).Error
		}
		return nil
	})

	if err != nil {
		common.SysError(common.LogText("creem topup failed: %s", err.Error()))
		return errors.New("top-up failed")
	}
	RecordTopupLog(topUp.UserId, common.NewMessage("Online top-up succeeded"), callerIp, topUp.PaymentMethod, PaymentMethodCreem)

	return nil
}

func RechargeWaffo(tradeNo string, callerIp string) (err error) {
	if tradeNo == "" {
		return errors.New("payment order number not provided")
	}

	completed := false
	topUp := &TopUp{}

	err = DB.Transaction(func(tx *gorm.DB) error {
		err := lockForUpdate(tx).Where(`"trade_no" = ?`, tradeNo).First(topUp).Error
		if err != nil {
			return errors.New("top-up order not found")
		}

		if topUp.PaymentProvider != PaymentProviderWaffo {
			return ErrPaymentMethodMismatch
		}

		if topUp.Status == common.TopUpStatusSuccess {
			return nil // 幂等：已成功直接返回
		}

		if topUp.Status != common.TopUpStatusPending {
			return errors.New("invalid top-up order status")
		}

		topUp.CompleteTime = common.GetTimestamp()
		topUp.Status = common.TopUpStatusSuccess
		if err := tx.Omit("Amount", "Money").Save(topUp).Error; err != nil {
			return err
		}

		if err := creditPaymentTopUpWallet(tx, topUp); err != nil {
			return err
		}
		completed = true
		return nil
	})

	if err != nil {
		common.SysError(common.LogText("waffo topup failed: %s", err.Error()))
		return errors.New("top-up failed")
	}
	if completed {
		RecordTopupLog(topUp.UserId, common.NewMessage("Online top-up succeeded"), callerIp, topUp.PaymentMethod, PaymentMethodWaffo)
	}

	return nil
}

func RechargeWaffoPancake(tradeNo string) (err error) {
	if tradeNo == "" {
		return errors.New("payment order number not provided")
	}

	completed := false
	topUp := &TopUp{}

	err = DB.Transaction(func(tx *gorm.DB) error {
		err := lockForUpdate(tx).Where(`"trade_no" = ?`, tradeNo).First(topUp).Error
		if err != nil {
			return errors.New("top-up order not found")
		}

		if topUp.PaymentProvider != PaymentProviderWaffoPancake {
			return ErrPaymentMethodMismatch
		}

		if topUp.Status == common.TopUpStatusSuccess {
			return nil
		}

		if topUp.Status != common.TopUpStatusPending {
			return errors.New("invalid top-up order status")
		}

		topUp.CompleteTime = common.GetTimestamp()
		topUp.Status = common.TopUpStatusSuccess
		if err := tx.Omit("Amount", "Money").Save(topUp).Error; err != nil {
			return err
		}

		if err := creditPaymentTopUpWallet(tx, topUp); err != nil {
			return err
		}
		completed = true
		return nil
	})

	if err != nil {
		common.SysError(common.LogText("waffo pancake topup failed: %s", err.Error()))
		return errors.New("top-up failed")
	}
	if completed {
		RecordLog(topUp.UserId, LogTypeTopup, common.NewMessage("Online top-up succeeded"))
	}

	return nil
}
