package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// BalanceReminderSettings is a complete replacement, not a merge: nil Threshold
// inherits the site value; an explicit zero is never inheritance.
type BalanceReminderSettings struct {
	Enabled   bool          `json:"enabled"`
	Threshold *money.Amount `json:"threshold"`
}

func (s *BalanceReminderSettings) UnmarshalJSON(data []byte) error {
	var input struct {
		Enabled   *bool           `json:"enabled"`
		Threshold json.RawMessage `json:"threshold"`
	}
	if err := common.DecodeJsonStrict(bytes.NewReader(data), &input); err != nil {
		return common.NewMessage("Invalid parameters")
	}
	if input.Enabled == nil || len(input.Threshold) == 0 {
		return common.NewMessage("Invalid parameters")
	}
	next := BalanceReminderSettings{Enabled: *input.Enabled}
	if !bytes.Equal(bytes.TrimSpace(input.Threshold), []byte("null")) {
		var amount struct {
			Nanos    *int64         `json:"nanos,string"`
			Currency money.Currency `json:"currency"`
		}
		if err := common.DecodeJsonStrict(bytes.NewReader(input.Threshold), &amount); err != nil || amount.Nanos == nil {
			return common.NewMessage("Invalid amount")
		}
		next.Threshold = &money.Amount{Nanos: *amount.Nanos, Currency: amount.Currency}
	}
	if err := validateBalanceReminderSettings(next); err != nil {
		return err
	}
	*s = next
	return nil
}

type BalanceReminderView struct {
	Enabled            bool          `json:"enabled"`
	Threshold          *money.Amount `json:"threshold"`
	EffectiveEnabled   bool          `json:"effective_enabled"`
	EffectiveThreshold money.Amount  `json:"effective_threshold"`
}

type UserBalanceReminder struct {
	UserID            int             `gorm:"primaryKey;autoIncrement:false"`
	Enabled           bool            `gorm:"not null"`
	ThresholdNanos    *int64          `gorm:"type:bigint"`
	ThresholdCurrency *money.Currency `gorm:"type:char(3)"`
}

func validateBalanceReminderSettings(s BalanceReminderSettings) error {
	if s.Threshold == nil {
		return nil
	}
	return ValidateBalanceReminderSiteConfig(operation_setting.BalanceReminderConfig{Threshold: *s.Threshold})
}

func ValidateBalanceReminderSiteConfig(c operation_setting.BalanceReminderConfig) error {
	if c.Threshold.Nanos < 0 {
		return common.NewMessage("Invalid amount")
	}
	if _, err := money.ParseCurrency(string(c.Threshold.Currency)); err != nil {
		return common.NewMessage("Invalid currency")
	}
	if c.Threshold.Currency != SettlementCurrency() {
		return money.ErrCurrencyMismatch
	}
	return nil
}

func balanceReminderSite(tx *gorm.DB) (operation_setting.BalanceReminderConfig, error) {
	var option Option
	if err := tx.Where("key = ?", operation_setting.BalanceReminderConfigOption).Take(&option).Error; err != nil {
		return operation_setting.BalanceReminderConfig{}, err
	}
	value, err := operation_setting.ParseBalanceReminderConfig(option.Value)
	if err != nil {
		return value, err
	}
	return value, ValidateBalanceReminderSiteConfig(value)
}

func GetBalanceReminderSiteSettings(ctx context.Context) (operation_setting.BalanceReminderConfig, error) {
	return balanceReminderSite(DB.WithContext(ctx))
}

func balanceReminderView(tx *gorm.DB, userID int) (*BalanceReminderView, error) {
	site, err := balanceReminderSite(tx)
	if err != nil {
		return nil, err
	}
	result := &BalanceReminderView{Enabled: true, EffectiveEnabled: site.Enabled, EffectiveThreshold: site.Threshold}
	var row UserBalanceReminder
	err = tx.Where("user_id = ?", userID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	result.Enabled = row.Enabled
	result.EffectiveEnabled = site.Enabled && row.Enabled
	if (row.ThresholdNanos == nil) != (row.ThresholdCurrency == nil) {
		return nil, ErrLedgerInvalidEntry
	}
	if row.ThresholdNanos != nil {
		result.Threshold = &money.Amount{Nanos: *row.ThresholdNanos, Currency: *row.ThresholdCurrency}
		if err := validateBalanceReminderSettings(BalanceReminderSettings{Threshold: result.Threshold}); err != nil {
			return nil, err
		}
		result.EffectiveThreshold = *result.Threshold
	}
	return result, nil
}

func GetUserBalanceReminder(ctx context.Context, userID int) (*BalanceReminderView, error) {
	if userID <= 0 {
		return nil, common.NewMessage("Invalid parameters")
	}
	if err := DB.WithContext(ctx).Select("id").First(&User{}, userID).Error; err != nil {
		return nil, err
	}
	return balanceReminderView(DB.WithContext(ctx), userID)
}

// PatchUserNotificationSettings locks and reads the current User before invoking
// a pure patch. Only Setting is persisted, irrespective of other callback edits.
// The caller publishes caches after success, never from inside this transaction.
func PatchUserNotificationSettings(ctx context.Context, userID int, reminder *BalanceReminderSettings, patch func(*User) error) (*BalanceReminderView, error) {
	if userID <= 0 {
		return nil, common.NewMessage("Invalid parameters")
	}
	if reminder != nil {
		if err := validateBalanceReminderSettings(*reminder); err != nil {
			return nil, err
		}
	}
	var result *BalanceReminderView
	err := DB.WithContext(ctx).Session(&gorm.Session{Logger: logger.Discard}).Transaction(func(tx *gorm.DB) error {
		var user User
		if err := lockForUpdate(tx).First(&user, userID).Error; err != nil {
			return err
		}
		if patch != nil {
			if err := patch(&user); err != nil {
				return err
			}
			if err := tx.Model(&User{}).Where("id = ?", userID).UpdateColumn("setting", user.Setting).Error; err != nil {
				return err
			}
		}
		if reminder != nil {
			row := UserBalanceReminder{UserID: userID, Enabled: reminder.Enabled}
			if reminder.Threshold != nil {
				nanos, currency := reminder.Threshold.Nanos, reminder.Threshold.Currency
				row.ThresholdNanos = &nanos
				row.ThresholdCurrency = &currency
			}
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}}, DoUpdates: clause.AssignmentColumns([]string{"enabled", "threshold_nanos", "threshold_currency"})}).Create(&row).Error; err != nil {
				return err
			}
		}
		var err error
		result, err = balanceReminderView(tx, userID)
		return err
	})
	return result, err
}

// A reminder observation is deliberately unbounded. It is not a wallet amount
// and must never pass through int64, money.Value, or floating point arithmetic.
type balanceReminderObservation struct {
	UserID         int
	WalletID       int
	SubscriptionID int
	PeriodID       int64
	Scope          string
	Currency       money.Currency
	Nanos          decimal.Decimal
}

func observeBalanceReminder(tx *gorm.DB, userID int, account LedgerAccount) (*balanceReminderObservation, error) {
	if account.Type != LedgerAccountUserWallet && account.Type != LedgerAccountSubscription && account.Type != LedgerAccountSubscriptionUnlimited {
		return nil, nil
	}
	result := &balanceReminderObservation{UserID: userID, Currency: account.Currency, Scope: "wallet"}
	if account.Type == LedgerAccountSubscription || account.Type == LedgerAccountSubscriptionUnlimited {
		var period SubscriptionPeriod
		if err := tx.First(&period, account.OwnerId).Error; err != nil {
			return nil, err
		}
		if period.UserID != userID || period.Allowance.Currency != account.Currency || period.account() != account {
			return nil, ErrLedgerInvalidEntry
		}
		if period.Unlimited {
			return nil, nil
		}
		var current int64
		if err := tx.Model(&UserSubscription{}).Where("id = ? AND user_id = ? AND status = ? AND end_time > ? AND current_period_id = ?", period.SubscriptionID, userID, "active", clock.Now().Unix(), period.ID).Count(&current).Error; err != nil {
			return nil, err
		}
		if current == 0 {
			return nil, nil
		}
		result.Scope = "subscription"
		result.PeriodID = period.ID
		result.SubscriptionID = period.SubscriptionID
	} else if account.OwnerId != userID {
		return nil, ErrLedgerInvalidEntry
	}
	var row struct {
		ID    int
		Nanos decimal.Decimal
	}
	err := tx.Raw(`SELECT w.id, w.balance_nanos::numeric - COALESCE((SELECT SUM(e.amount_nanos::numeric) FROM ledger_entries e WHERE e.wallet_id=w.id AND e.type IN ('pre_consume','pre_consume_release')),0) - CASE WHEN w.account_type='user_wallet' THEN COALESCE((SELECT d.balance_nanos::numeric FROM wallets d WHERE d.account_type='user_receivable' AND d.owner_id=w.owner_id AND d.balance_currency=w.balance_currency),0) ELSE 0 END AS nanos FROM wallets w WHERE w.account_type=? AND w.owner_id=? AND w.balance_currency=?`, account.Type, account.OwnerId, account.Currency).Scan(&row).Error
	if err != nil {
		return nil, err
	}
	if row.ID == 0 {
		return nil, ErrLedgerInvalidEntry
	}
	result.WalletID = row.ID
	result.Nanos = row.Nanos
	return result, nil
}

// Caller already owns the complete funding wallet locks. Invoke only for actual
// new consumption; reservations, releases and repayment alone are not events.
func enqueueBalanceReminder(tx *gorm.DB, before *balanceReminderObservation, account LedgerAccount, identity string) error {
	if before == nil {
		return nil
	}
	after, err := observeBalanceReminder(tx, before.UserID, account)
	if err != nil {
		return err
	}
	if after == nil {
		return nil
	}
	policy, err := balanceReminderView(tx, before.UserID)
	if err != nil {
		return err
	}
	threshold := decimal.NewFromInt(policy.EffectiveThreshold.Nanos)
	if !policy.EffectiveEnabled || before.Currency != policy.EffectiveThreshold.Currency || before.Nanos.LessThan(threshold) || !after.Nanos.LessThan(threshold) {
		return nil
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", identity, before.WalletID)))
	event := balanceReminderEvent{Identity: hex.EncodeToString(digest[:]), UserID: before.UserID, WalletID: before.WalletID, SubscriptionID: before.SubscriptionID, PeriodID: before.PeriodID, Scope: before.Scope, BeforeNanos: before.Nanos, AfterNanos: after.Nanos, Threshold: policy.EffectiveThreshold, CreatedAt: clock.Now().UnixMilli(), Status: "pending", NextAttemptAt: clock.Now().UnixMilli()}
	return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "identity"}}, DoNothing: true}).Create(&event).Error
}
