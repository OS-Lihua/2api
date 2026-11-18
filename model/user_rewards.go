package model

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// ReferralRewards exposes an exact balance and an unbounded cumulative total.
type ReferralRewards struct {
	Available money.Amount      `json:"available"`
	Earned    CheckinAwardTotal `json:"earned"`
	Invited   int               `json:"invited"`
}

func GetReferralRewards(userID int) (ReferralRewards, error) {
	rewards, err := GetReferralRewardsForUsers([]int{userID})
	if err != nil {
		return ReferralRewards{}, err
	}
	result, exists := rewards[userID]
	if !exists {
		return ReferralRewards{}, gorm.ErrRecordNotFound
	}
	return result, nil
}

// GetReferralRewardsForUsers projects balances and totals in one database snapshot.
// The batch also serves administrator lists without a query for each user.
func GetReferralRewardsForUsers(userIDs []int) (map[int]ReferralRewards, error) {
	result := make(map[int]ReferralRewards, len(userIDs))
	if len(userIDs) == 0 {
		return result, nil
	}
	var rows []struct {
		ID                int
		AffCount          int
		AvailableNanos    int64
		AvailableCurrency money.Currency
		OpeningCurrency   money.Currency
		EarnedNanos       decimal.Decimal
	}
	earned := DB.Model(&LedgerEntry{}).Select(`"wallet_id", SUM("amount_nanos") AS "nanos"`).
		Where(`"type" = ?`, LedgerEntryTopUp).Group(`"wallet_id"`)
	err := DB.Table("users").Select(`"users"."id", "users"."aff_count",
	 COALESCE("wallets"."balance_nanos", 0) AS "available_nanos",
	 COALESCE("wallets"."balance_currency", ?) AS "available_currency",
	 COALESCE("referral_openings"."earned_currency", ?) AS "opening_currency",
	 COALESCE("referral_openings"."earned_nanos", 0) + COALESCE("rewards"."nanos", 0) AS "earned_nanos"`, SettlementCurrency(), SettlementCurrency()).
		Joins(`LEFT JOIN "wallets" ON "wallets"."owner_id" = "users"."id" AND "wallets"."account_type" = ?`, LedgerAccountUserReferral).
		Joins(`LEFT JOIN "referral_openings" ON "referral_openings"."user_id" = "users"."id"`).
		Joins(`LEFT JOIN (?) AS "rewards" ON "rewards"."wallet_id" = "wallets"."id"`, earned).
		Where(`"users"."id" IN ?`, userIDs).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.AvailableCurrency != SettlementCurrency() || row.OpeningCurrency != row.AvailableCurrency {
			return nil, money.ErrCurrencyMismatch
		}
		result[row.ID] = ReferralRewards{
			Available: money.Amount{Nanos: row.AvailableNanos, Currency: row.AvailableCurrency},
			Earned:    CheckinAwardTotal{Nanos: row.EarnedNanos.StringFixed(0), Currency: row.AvailableCurrency}, Invited: row.AffCount,
		}
	}
	return result, nil
}

var referralTransferID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// TransferReferralRewards atomically debits rewards and credits the money wallet.
// A lost response is retried with the same identity and cannot issue another credit.
func TransferReferralRewards(userID int, amount money.Amount, transferID string) (*LedgerEntry, error) {
	if userID <= 0 || amount.Nanos <= 0 || amount.Currency != SettlementCurrency() || !referralTransferID.MatchString(transferID) {
		return nil, common.NewMessage("Invalid amount")
	}
	key := fmt.Sprintf("referral-transfer:%d:%s", userID, transferID)
	postings := []LedgerPosting{
		{Account: userReferralAccount(userID), Type: LedgerEntryTransfer, Amount: money.Amount{Nanos: -amount.Nanos, Currency: amount.Currency}, IdempotencyKey: key + ":rewards", Remark: "Invitation reward transfer"},
		{Account: UserWalletAccount(userID), Type: LedgerEntryTransfer, Amount: amount, IdempotencyKey: key + ":wallet", Remark: "Invitation reward transfer"},
	}
	for _, posting := range postings {
		if err := validateLedgerPosting(posting); err != nil {
			return nil, err
		}
	}
	var credit *LedgerEntry
	err := DB.Transaction(func(tx *gorm.DB) error {
		// Checkout fulfillment locks wallets before updating users. Take the
		// complete shared wallet lock set before acquiring this user row too.
		if _, err := lockLedgerWallets(tx, postings); err != nil {
			return err
		}
		var user User
		if err := lockForUpdate(tx).Select("id").Where("status = ?", common.UserStatusEnabled).First(&user, userID).Error; err != nil {
			return err
		}
		var existing LedgerEntry
		err := tx.Where("idempotency_key = ?", key+":wallet").Take(&existing).Error
		if err == nil {
			if existing.Amount != amount || existing.Type != LedgerEntryTransfer {
				return common.NewMessage("Invalid parameters")
			}
			credit = &existing
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		entries, err := postLedgerEntries(tx, postings)
		if errors.Is(err, ErrInsufficientWalletBalance) {
			return common.NewMessage("Insufficient balance")
		}
		if err != nil {
			return err
		}
		credit = entries[1]
		return nil
	})
	return credit, err
}

func userReferralAccount(userID int) LedgerAccount {
	return LedgerAccount{Type: LedgerAccountUserReferral, OwnerId: userID, Currency: SettlementCurrency()}
}

// rewardPosting freezes the accounting conversion in the same transaction as the award.
func rewardPosting(tx *gorm.DB, account LedgerAccount, source money.Amount, key, remark string) (LedgerPosting, error) {
	credited := source
	var fx decimal.NullDecimal
	if source.Currency != account.Currency {
		rate, err := GetAccountingFXRate(tx.Statement.Context, tx, source.Currency, account.Currency)
		if err != nil {
			return LedgerPosting{}, err
		}
		value, err := source.Value().Convert(rate)
		if err != nil {
			return LedgerPosting{}, err
		}
		credited, err = value.Credit()
		if err != nil {
			return LedgerPosting{}, err
		}
		fx = decimal.NewNullDecimal(rate.Decimal())
	}
	if credited.Nanos <= 0 {
		return LedgerPosting{}, common.NewMessage("Invalid amount")
	}
	posting := LedgerPosting{Account: account, Type: LedgerEntryTopUp, Amount: credited,
		IdempotencyKey: key, Remark: remark, sourceAmount: &source, fxRate: fx}
	return posting, validateLedgerPosting(posting)
}

// grantRegistrationRewards belongs to the owning user-creation transaction.
// Post-commit profile initialization must never issue financial side effects.
func grantRegistrationRewards(tx *gorm.DB, user *User, inviterID int) error {
	setting := operation_setting.GetRewardSetting()
	postings := make([]LedgerPosting, 0, 3)
	add := func(account LedgerAccount, amount money.Amount, kind, remark string) error {
		if amount.Nanos == 0 {
			return nil
		}
		posting, err := rewardPosting(tx, account, amount, fmt.Sprintf("registration:%d:%s", user.Id, kind), remark)
		if err != nil {
			return err
		}
		postings = append(postings, posting)
		return nil
	}
	if err := add(UserWalletAccount(user.Id), setting.Signup, "signup", "Sign-up reward"); err != nil {
		return err
	}
	invitation := inviterID > 0 && operation_setting.IsPaymentComplianceConfirmed()
	if invitation {
		var inviter User
		if err := tx.Select("id").Where("id = ? AND status = ? AND id <> ?", inviterID, common.UserStatusEnabled, user.Id).Take(&inviter).Error; err != nil {
			return err
		}
		if err := add(UserWalletAccount(user.Id), setting.Invitee, "invitee", "Invitation code reward"); err != nil {
			return err
		}
		if err := add(userReferralAccount(inviterID), setting.Inviter, "inviter", "Invitation reward"); err != nil {
			return err
		}
	}
	if len(postings) > 0 {
		if _, err := postLedgerEntries(tx, postings); err != nil {
			return err
		}
	}
	if invitation {
		// Follow wallet-before-user ordering and recheck eligibility in the
		// update that records the invitation. Any failure rolls back all awards.
		result := tx.Model(&User{}).Where("id = ? AND status = ? AND id <> ?", inviterID, common.UserStatusEnabled, user.Id).
			UpdateColumn("aff_count", gorm.Expr("aff_count + 1"))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return common.NewMessage("User does not exist")
		}
	}
	return nil
}
