package model

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

var ErrBalanceReminderLeaseLost = errors.New("balance reminder delivery lease lost")

const (
	BalanceReminderDelivered          = "delivered"
	BalanceReminderDeliveryFailed     = "delivery_failed"
	BalanceReminderNotConfigured      = "not_configured"
	BalanceReminderRateLimited        = "rate_limited"
	BalanceReminderTransportRejected  = "transport_rejected"
	BalanceReminderDisabled           = "disabled"
	BalanceReminderStalePeriod        = "stale_period"
	BalanceReminderRecovered          = "recovered"
	BalanceReminderUserUnavailable    = "user_unavailable"
	BalanceReminderCancelled          = "cancelled"
	BalanceReminderTimeout            = "timeout"
	BalanceReminderInvalidDestination = "invalid_destination"
)

type balanceReminderEvent struct {
	ID             int64           `gorm:"primaryKey"`
	Identity       string          `gorm:"type:char(64);not null;uniqueIndex"`
	UserID         int             `gorm:"not null;index"`
	Scope          string          `gorm:"type:varchar(16);not null"`
	WalletID       int             `gorm:"not null"`
	SubscriptionID int             `gorm:"not null"`
	PeriodID       int64           `gorm:"not null"`
	BeforeNanos    decimal.Decimal `gorm:"type:numeric;not null"`
	AfterNanos     decimal.Decimal `gorm:"type:numeric;not null"`
	Threshold      money.Amount    `gorm:"embedded;embeddedPrefix:threshold_"`
	CreatedAt      int64           `gorm:"not null"`
	Status         string          `gorm:"type:varchar(16);not null;index:balance_reminder_due,priority:1"`
	NextAttemptAt  int64           `gorm:"not null;index:balance_reminder_due,priority:2"`
	ClaimToken     string          `gorm:"type:varchar(36);not null"`
	RunnerID       string          `gorm:"type:varchar(128);not null"`
	LeaseUntil     int64           `gorm:"not null"`
	Attempts       int             `gorm:"not null"`
	Code           string          `gorm:"type:varchar(32);not null"`
}

type balanceReminderWindow struct {
	UserID   int   `gorm:"primaryKey;autoIncrement:false"`
	EndsAt   int64 `gorm:"not null"`
	Attempts int   `gorm:"not null"`
}

// CreatedAt and LeaseUntil are Unix milliseconds, including across processes.
// Scope is exactly wallet or subscription. Before/After are signed, unbounded
// integer nanos, not spendable ledger amounts or present-time promises.
type BalanceReminderDelivery struct {
	ID             int64
	UserID         int
	Scope          string
	WalletID       int
	SubscriptionID int
	PeriodID       int64
	Before         MoneyTotal
	After          MoneyTotal
	Threshold      money.Amount
	CreatedAt      int64
	ClaimToken     string
	LeaseUntil     int64
	Attempts       int
}

type BalanceReminderOutcome struct {
	Status  string
	RetryAt time.Time
	Code    string
}

func (e balanceReminderEvent) delivery() BalanceReminderDelivery {
	return BalanceReminderDelivery{ID: e.ID, UserID: e.UserID, Scope: e.Scope, WalletID: e.WalletID, SubscriptionID: e.SubscriptionID, PeriodID: e.PeriodID, Before: MoneyTotal{Nanos: e.BeforeNanos.StringFixed(0), Currency: e.Threshold.Currency}, After: MoneyTotal{Nanos: e.AfterNanos.StringFixed(0), Currency: e.Threshold.Currency}, Threshold: e.Threshold, CreatedAt: e.CreatedAt, ClaimToken: e.ClaimToken, LeaseUntil: e.LeaseUntil, Attempts: e.Attempts}
}

func balanceReminderEligibility(tx *gorm.DB, event balanceReminderEvent) (string, error) {
	var users int64
	if err := tx.Model(&User{}).Where("id = ? AND status = ?", event.UserID, common.UserStatusEnabled).Count(&users).Error; err != nil {
		return "", err
	}
	if users != 1 {
		return BalanceReminderUserUnavailable, nil
	}
	policy, err := balanceReminderView(tx, event.UserID)
	if err != nil {
		return "", err
	}
	if !policy.EffectiveEnabled {
		return BalanceReminderDisabled, nil
	}
	var wallet Wallet
	if err := tx.First(&wallet, event.WalletID).Error; err != nil {
		return "", err
	}
	account := LedgerAccount{Type: wallet.AccountType, OwnerId: wallet.OwnerId, Currency: wallet.Balance.Currency}
	if event.Scope == "wallet" {
		if account != UserWalletAccount(event.UserID) || event.PeriodID != 0 {
			return "", ErrLedgerInvalidEntry
		}
	} else if event.Scope == "subscription" {
		if account.Type != LedgerAccountSubscription || int64(account.OwnerId) != event.PeriodID {
			return "", ErrLedgerInvalidEntry
		}
	} else {
		return "", ErrLedgerInvalidEntry
	}
	current, err := observeBalanceReminder(tx, event.UserID, account)
	if err != nil {
		return "", err
	}
	if current == nil {
		return BalanceReminderStalePeriod, nil
	}
	if current.Currency != policy.EffectiveThreshold.Currency {
		return "", money.ErrCurrencyMismatch
	}
	if !current.Nanos.LessThan(decimal.NewFromInt(policy.EffectiveThreshold.Nanos)) {
		return BalanceReminderRecovered, nil
	}
	return "", nil
}

// HasDueBalanceReminders reports work using the same due-time and expired-lease
// predicate as ClaimBalanceReminders. It does not consume a claim or budget.
func HasDueBalanceReminders(ctx context.Context) (bool, error) {
	now := clock.Now().UnixMilli()
	var id int64
	result := DB.WithContext(ctx).Session(&gorm.Session{Logger: logger.Discard}).Model(&balanceReminderEvent{}).Select("id").Where("((status IN ? AND next_attempt_at <= ?) OR (status = ? AND lease_until <= ?))", []string{"pending", "retry"}, now, "delivering", now).Limit(1).Scan(&id)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected != 0, nil
}

// ClaimBalanceReminders reserves a durable fixed-window attempt budget and a
// fenced lease in one short transaction. No network work or funding lock occurs.
func ClaimBalanceReminders(ctx context.Context, runnerID string, limit int, lease, window time.Duration, windowLimit int) ([]BalanceReminderDelivery, error) {
	if runnerID == "" || len(runnerID) > 128 || limit < 1 || limit > 100 || lease < time.Second || lease > 10*time.Minute || window < time.Second || window > 365*24*time.Hour || windowLimit < 0 {
		return nil, common.NewMessage("Invalid parameters")
	}
	deliveries := []BalanceReminderDelivery{}
	err := DB.WithContext(ctx).Session(&gorm.Session{Logger: logger.Discard}).Transaction(func(tx *gorm.DB) error {
		now := clock.Now().UnixMilli()
		var events []balanceReminderEvent
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("((status IN ? AND next_attempt_at <= ?) OR (status = ? AND lease_until <= ?))", []string{"pending", "retry"}, now, "delivering", now).Order("next_attempt_at,id").Limit(limit).Find(&events).Error; err != nil {
			return err
		}
		// Multiple workers may claim different events for the same users. Acquire all
		// window rows in sorted order, rather than event order, to avoid inversion.
		ids := []int{}
		seen := map[int]bool{}
		for _, event := range events {
			if !seen[event.UserID] {
				ids = append(ids, event.UserID)
				seen[event.UserID] = true
			}
		}
		sort.Ints(ids)
		windows := map[int]*balanceReminderWindow{}
		for _, id := range ids {
			row := balanceReminderWindow{UserID: id}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
				return err
			}
			if err := lockForUpdate(tx).Where("user_id = ?", id).Take(&row).Error; err != nil {
				return err
			}
			windows[id] = &row
		}
		for _, event := range events {
			code, err := balanceReminderEligibility(tx, event)
			if err != nil {
				return err
			}
			if code != "" {
				if err := tx.Model(&event).Updates(map[string]any{"status": "skipped", "code": code, "claim_token": "", "lease_until": 0}).Error; err != nil {
					return err
				}
				continue
			}
			budget := windows[event.UserID]
			if budget.EndsAt <= now {
				budget.EndsAt = now + window.Milliseconds()
				budget.Attempts = 0
				if err := tx.Save(budget).Error; err != nil {
					return err
				}
			}
			if budget.Attempts >= windowLimit {
				if err := tx.Model(&event).Updates(map[string]any{"status": "retry", "code": BalanceReminderRateLimited, "next_attempt_at": budget.EndsAt, "claim_token": "", "lease_until": 0}).Error; err != nil {
					return err
				}
				continue
			}
			budget.Attempts++
			if err := tx.Save(budget).Error; err != nil {
				return err
			}
			event.Status = "delivering"
			event.ClaimToken = uuid.NewString()
			event.RunnerID = runnerID
			event.LeaseUntil = now + lease.Milliseconds()
			event.Attempts++
			event.Code = ""
			if err := tx.Model(&event).Updates(map[string]any{"status": event.Status, "claim_token": event.ClaimToken, "runner_id": runnerID, "lease_until": event.LeaseUntil, "attempts": event.Attempts, "code": ""}).Error; err != nil {
				return err
			}
			deliveries = append(deliveries, event.delivery())
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return deliveries, nil
}

// false means this token is no longer authorized, or the event was explicitly
// skipped. A database/financial read failure is always an error, never false/nil.
func CheckBalanceReminderDelivery(ctx context.Context, eventID int64, claimToken string) (bool, error) {
	allowed := false
	err := DB.WithContext(ctx).Session(&gorm.Session{Logger: logger.Discard}).Transaction(func(tx *gorm.DB) error {
		var event balanceReminderEvent
		err := lockForUpdate(tx).Where("id = ? AND status = ? AND claim_token = ? AND lease_until > ?", eventID, "delivering", claimToken, clock.Now().UnixMilli()).Take(&event).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		code, err := balanceReminderEligibility(tx, event)
		if err != nil {
			return err
		}
		if code != "" {
			return tx.Model(&event).Updates(map[string]any{"status": "skipped", "code": code, "claim_token": "", "lease_until": 0}).Error
		}
		allowed = true
		return nil
	})
	return allowed, err
}

func FinishBalanceReminderDelivery(ctx context.Context, eventID int64, claimToken string, outcome BalanceReminderOutcome) error {
	codes := map[string]bool{BalanceReminderDelivered: true, BalanceReminderDeliveryFailed: true, BalanceReminderNotConfigured: true, BalanceReminderRateLimited: true, BalanceReminderTransportRejected: true, BalanceReminderDisabled: true, BalanceReminderStalePeriod: true, BalanceReminderRecovered: true, BalanceReminderUserUnavailable: true, BalanceReminderCancelled: true, BalanceReminderTimeout: true, BalanceReminderInvalidDestination: true}
	if !codes[outcome.Code] || outcome.Status != "sent" && outcome.Status != "retry" && outcome.Status != "skipped" || outcome.Status == "sent" && outcome.Code != BalanceReminderDelivered || outcome.Status != "sent" && outcome.Code == BalanceReminderDelivered {
		return common.NewMessage("Invalid parameters")
	}
	now := clock.Now().UnixMilli()
	if outcome.Status == "retry" && outcome.RetryAt.UnixMilli() <= now {
		return common.NewMessage("Invalid parameters")
	}
	result := DB.WithContext(ctx).Session(&gorm.Session{Logger: logger.Discard}).Model(&balanceReminderEvent{}).Where("id = ? AND status = ? AND claim_token = ? AND lease_until > ?", eventID, "delivering", claimToken, now).Updates(map[string]any{"status": outcome.Status, "code": outcome.Code, "next_attempt_at": outcome.RetryAt.UnixMilli(), "claim_token": "", "lease_until": 0, "runner_id": ""})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrBalanceReminderLeaseLost
	}
	return nil
}
