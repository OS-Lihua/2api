package model

import (
	"context"
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// SubscriptionPeriod fixes one grant independently of later resets. Its account
// is never reused; accepted requests retain this identity after expiry or reset.
type SubscriptionPeriod struct {
	ID             int64             `gorm:"primaryKey"`
	SubscriptionID int               `gorm:"not null;uniqueIndex:subscription_period_sequence,priority:1"`
	UserID         int               `gorm:"not null;index"`
	Sequence       int64             `gorm:"not null;uniqueIndex:subscription_period_sequence,priority:2"`
	Allowance      money.Amount      `gorm:"embedded;embeddedPrefix:allowance_"`
	Unlimited      bool              `gorm:"not null"`
	Reason         string            `gorm:"type:varchar(32);not null"`
	CreatedAt      int64             `gorm:"not null"`
	Subscription   *UserSubscription `gorm:"foreignKey:SubscriptionID;constraint:OnDelete:RESTRICT" json:"-"`
}

func MigrateSubscriptionFunding(db *gorm.DB) error {
	if err := db.AutoMigrate(&SubscriptionPeriod{}); err != nil {
		return err
	}
	for _, statement := range []string{
		`CREATE OR REPLACE FUNCTION "subscription_periods_immutable"() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'subscription periods are immutable'; END $$`,
		`DROP TRIGGER IF EXISTS "subscription_periods_immutable" ON "subscription_periods"`,
		`CREATE TRIGGER "subscription_periods_immutable" BEFORE UPDATE OR DELETE ON "subscription_periods" FOR EACH ROW EXECUTE FUNCTION "subscription_periods_immutable"()`,
		`DROP TRIGGER IF EXISTS "subscription_periods_no_truncate" ON "subscription_periods"`,
		`CREATE TRIGGER "subscription_periods_no_truncate" BEFORE TRUNCATE ON "subscription_periods" FOR EACH STATEMENT EXECUTE FUNCTION "subscription_periods_immutable"()`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}

func (period SubscriptionPeriod) account() LedgerAccount {
	kind := LedgerAccountSubscription
	if period.Unlimited {
		kind = LedgerAccountSubscriptionUnlimited
	}
	return LedgerAccount{Type: kind, OwnerId: int(period.ID), Currency: period.Allowance.Currency}
}

// createSubscriptionPeriodTx requires the caller's parent subscription lock.
// Only the new, unpublished account is acquired here; existing wallets must be
// locked before the parent. This does not mutate any historical quota field.
func createSubscriptionPeriodTx(tx *gorm.DB, subscription *UserSubscription, consumed money.Amount, reason string) error {
	if subscription.Id <= 0 || subscription.UserId <= 0 || subscription.MoneyVersion != 1 || subscription.Allowance.Currency != SettlementCurrency() || subscription.Allowance.Nanos < 0 || consumed.Currency != subscription.Allowance.Currency || consumed.Nanos < 0 || !subscription.Unlimited && consumed.Nanos > subscription.Allowance.Nanos {
		return ErrLedgerInvalidEntry
	}
	if reason != "grant" && reason != "reset" && reason != "legacy_opening" || reason != "legacy_opening" && consumed.Nanos != 0 {
		return ErrLedgerInvalidEntry
	}
	var parent UserSubscription
	if err := lockForUpdate(tx).Select("id", "current_period_id").First(&parent, subscription.Id).Error; err != nil {
		return err
	}
	sequence := int64(1)
	if parent.CurrentPeriodID != 0 {
		var previous SubscriptionPeriod
		if err := tx.First(&previous, parent.CurrentPeriodID).Error; err != nil {
			return err
		}
		if previous.SubscriptionID != subscription.Id || previous.UserID != subscription.UserId {
			return ErrLedgerInvalidEntry
		}
		if reason != "reset" {
			return errors.New("subscription already has an entitlement period")
		}
		sequence = previous.Sequence + 1
		if sequence <= previous.Sequence {
			return money.ErrOverflow
		}
	}
	period := SubscriptionPeriod{SubscriptionID: subscription.Id, UserID: subscription.UserId, Sequence: sequence, Allowance: subscription.Allowance, Unlimited: subscription.Unlimited, Reason: reason, CreatedAt: clock.Now().Unix()}
	if err := tx.Create(&period).Error; err != nil {
		return err
	}
	account := period.account()
	if _, err := ensureWallet(tx, account); err != nil {
		return err
	}
	postings := []LedgerPosting{}
	key := fmt.Sprintf("subscription:period:%d:", period.ID)
	if !period.Unlimited && period.Allowance.Nanos > 0 {
		postings = append(postings, LedgerPosting{Account: account, Type: LedgerEntrySubscriptionGrant, Amount: period.Allowance, IdempotencyKey: key + "grant", Remark: reason})
	}
	if consumed.Nanos > 0 {
		postings = append(postings, LedgerPosting{Account: account, Type: LedgerEntrySubscriptionOpeningUsed, Amount: money.Amount{Nanos: -consumed.Nanos, Currency: consumed.Currency}, IdempotencyKey: key + "opening_used", Remark: reason})
	}
	for _, posting := range postings {
		if err := validateLedgerPosting(posting); err != nil {
			return err
		}
	}
	if _, err := postLedgerEntries(tx, postings); err != nil {
		return err
	}
	if err := tx.Model(&UserSubscription{}).Where("id = ?", subscription.Id).UpdateColumn("current_period_id", period.ID).Error; err != nil {
		return err
	}
	subscription.CurrentPeriodID = period.ID
	return nil
}

type SubscriptionPeriodSummary struct {
	PeriodID  int64        `json:"period_id"`
	Sequence  int64        `json:"sequence"`
	Allowance money.Amount `json:"allowance"`
	Consumed  money.Amount `json:"consumed"`
	Available money.Amount `json:"available"`
	Reserved  money.Amount `json:"reserved"`
	Unlimited bool         `json:"unlimited"`
}

// GetSubscriptionPeriodSummary derives all mutable amounts from the ledger.
// An unlimited account has no finite available amount; Unlimited distinguishes
// its zero presentation value from an exhausted finite grant.
func GetSubscriptionPeriodSummary(ctx context.Context, subscriptionID int) (*SubscriptionPeriodSummary, error) {
	var period SubscriptionPeriod
	if err := DB.WithContext(ctx).Where(`"id" = (SELECT "current_period_id" FROM "user_subscriptions" WHERE "id" = ?)`, subscriptionID).First(&period).Error; err != nil {
		return nil, err
	}
	account := period.account()
	var sums struct {
		Consumed decimal.Decimal
		Reserved decimal.Decimal
		Balance  decimal.Decimal
	}
	err := DB.WithContext(ctx).Model(&LedgerEntry{}).Select(`COALESCE(-SUM("amount_nanos") FILTER (WHERE "type" IN ?), 0) AS "consumed", COALESCE(-SUM("amount_nanos") FILTER (WHERE "type" IN ?), 0) AS "reserved", COALESCE(SUM("amount_nanos"), 0) AS "balance"`, []LedgerEntryType{LedgerEntryCharge, LedgerEntryRefund, LedgerEntrySubscriptionOpeningUsed}, []LedgerEntryType{LedgerEntryPreConsume, LedgerEntryPreConsumeRelease}).Where(`"wallet_id" = (SELECT "id" FROM "wallets" WHERE "account_type" = ? AND "owner_id" = ? AND "balance_currency" = ?)`, account.Type, account.OwnerId, account.Currency).Scan(&sums).Error
	if err != nil {
		return nil, err
	}
	result := &SubscriptionPeriodSummary{PeriodID: period.ID, Sequence: period.Sequence, Allowance: period.Allowance, Unlimited: period.Unlimited}
	for _, item := range []struct {
		amount *money.Amount
		nanos  decimal.Decimal
	}{{&result.Consumed, sums.Consumed}, {&result.Reserved, sums.Reserved}, {&result.Available, sums.Balance}} {
		value, err := money.NewValue(item.nanos.Shift(-9), account.Currency)
		if err != nil {
			return nil, err
		}
		*item.amount, err = value.Receivable()
		if err != nil {
			return nil, err
		}
	}
	if period.Unlimited {
		result.Available = money.Amount{Currency: account.Currency}
	}
	return result, nil
}

type GatewayFundingSnapshot struct {
	Version        int    `json:"version"`
	Preference     string `json:"preference"`
	Source         string `json:"source"`
	SubscriptionID int    `json:"subscription_id,omitempty"`
	PeriodID       int64  `json:"period_id,omitempty"`
}

var errSubscriptionFundingChanged = errors.New("subscription period changed during funding selection")

// lockSubscriptionFundingWallets must precede every user/subscription row lock.
// Include extra existing accounts before locking, never acquire them afterward.
func lockSubscriptionFundingWallets(tx *gorm.DB, userIDs []int, extra ...LedgerPosting) (map[LedgerAccount]*Wallet, error) {
	postings := append([]LedgerPosting{}, extra...)
	for _, id := range userIDs {
		postings = append(postings, LedgerPosting{Account: UserWalletAccount(id)})
	}
	var periods []SubscriptionPeriod
	if err := tx.Where("user_id IN ?", userIDs).Find(&periods).Error; err != nil {
		return nil, err
	}
	for _, period := range periods {
		postings = append(postings, LedgerPosting{Account: period.account()})
	}
	return lockLedgerWallets(tx, postings)
}

// reserveGatewayFundingTx takes every existing wallet before user/subscription
// locks. A changed candidate set causes the complete reservation to retry.
func reserveGatewayFundingTx(tx *gorm.DB, request *GatewayRequest, reservation money.Amount) (LedgerAccount, error) {
	var user User
	if err := tx.Select("id", "setting").First(&user, request.UserID).Error; err != nil {
		return LedgerAccount{}, err
	}
	preference := common.NormalizeBillingPreference(user.GetSetting().BillingPreference)
	var candidates []UserSubscription
	if preference != "wallet_only" {
		if err := tx.Where("user_id = ? AND status = ? AND end_time > ?", request.UserID, "active", clock.Now().Unix()).Order("end_time, id").Find(&candidates).Error; err != nil {
			return LedgerAccount{}, err
		}
	}
	accounts := []LedgerPosting{{Account: UserWalletAccount(request.UserID)}}
	periods := make(map[int64]SubscriptionPeriod, len(candidates))
	for _, subscription := range candidates {
		if subscription.MoneyVersion != 1 || subscription.CurrentPeriodID == 0 {
			return LedgerAccount{}, errors.New("subscription money migration is incomplete")
		}
		var period SubscriptionPeriod
		if err := tx.First(&period, subscription.CurrentPeriodID).Error; err != nil {
			return LedgerAccount{}, err
		}
		if period.UserID != request.UserID || period.SubscriptionID != subscription.Id {
			return LedgerAccount{}, ErrLedgerInvalidEntry
		}
		periods[period.ID] = period
		accounts = append(accounts, LedgerPosting{Account: period.account()})
	}
	wallets, err := lockLedgerWallets(tx, accounts)
	if err != nil {
		return LedgerAccount{}, err
	}
	if err := lockForUpdate(tx).Select("id", "setting").First(&user, request.UserID).Error; err != nil {
		return LedgerAccount{}, err
	}
	if common.NormalizeBillingPreference(user.GetSetting().BillingPreference) != preference {
		return LedgerAccount{}, errSubscriptionFundingChanged
	}
	var current []UserSubscription
	if preference != "wallet_only" {
		if err := lockForUpdate(tx).Where("user_id = ? AND status = ? AND end_time > ?", request.UserID, "active", clock.Now().Unix()).Order("end_time, id").Find(&current).Error; err != nil {
			return LedgerAccount{}, err
		}
		if len(current) != len(candidates) {
			return LedgerAccount{}, errSubscriptionFundingChanged
		}
		for i := range current {
			if current[i].Id != candidates[i].Id || current[i].CurrentPeriodID != candidates[i].CurrentPeriodID {
				return LedgerAccount{}, errSubscriptionFundingChanged
			}
		}
	}
	walletAccount := UserWalletAccount(request.UserID)
	wallet := wallets[walletAccount]
	var unresolved int64
	if err := gatewayPendingSales(tx, request.UserID).Count(&unresolved).Error; err != nil {
		return LedgerAccount{}, err
	}
	if unresolved > 0 || wallet.RequestsPaused || wallets[UserReceivableAccount(request.UserID)].Balance.Nanos > 0 {
		return LedgerAccount{}, ErrInsufficientWalletBalance
	}
	chosen := LedgerAccount{}
	funding := GatewayFundingSnapshot{Version: 1, Preference: preference, Source: "wallet"}
	if (preference == "wallet_only" || preference == "wallet_first") && wallet.Balance.Nanos >= reservation.Nanos {
		chosen = walletAccount
	} else if preference != "wallet_only" {
		overflow := true
		for i := range current {
			subscription := &current[i]
			if !subscription.AllowWalletOverflow {
				overflow = false
			}
			if chosen.Type != "" {
				continue
			}
			var plan SubscriptionPlan
			if err := tx.First(&plan, subscription.PlanId).Error; err != nil {
				return LedgerAccount{}, err
			}
			previousPeriod := subscription.CurrentPeriodID
			if err := maybeResetUserSubscriptionWithPlanTx(tx, subscription, &plan, clock.Now().Unix()); err != nil {
				return LedgerAccount{}, err
			}
			if subscription.CurrentPeriodID != previousPeriod {
				// This account was just created under our parent lock and is not
				// published. No existing wallet is acquired after the parent.
				var period SubscriptionPeriod
				if err := tx.First(&period, subscription.CurrentPeriodID).Error; err != nil {
					return LedgerAccount{}, err
				}
				var wallet Wallet
				account := period.account()
				if err := tx.Where("account_type = ? AND owner_id = ? AND balance_currency = ?", account.Type, account.OwnerId, account.Currency).Take(&wallet).Error; err != nil {
					return LedgerAccount{}, err
				}
				periods[period.ID], wallets[account] = period, &wallet
			}
			period := periods[subscription.CurrentPeriodID]
			if period.Unlimited || wallets[period.account()].Balance.Nanos >= reservation.Nanos {
				chosen = period.account()
				funding.Source, funding.SubscriptionID, funding.PeriodID = "subscription", subscription.Id, period.ID
			}
		}
		if chosen.Type == "" && preference == "subscription_first" && (len(current) == 0 || overflow) && wallet.Balance.Nanos >= reservation.Nanos {
			chosen = walletAccount
		}
	}
	if chosen.Type == "" {
		return LedgerAccount{}, ErrInsufficientWalletBalance
	}
	request.Funding = funding
	return chosen, nil
}
