package model

import (
	"errors"
	"fmt"
	"math/big"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// ReferralOpening preserves the released cumulative history separately from the
// still-payable opening balance. Neither value is recalculated from later rewards.
type ReferralOpening struct {
	UserID         int             `gorm:"primaryKey;autoIncrement:false"`
	Available      money.Amount    `gorm:"embedded;embeddedPrefix:available_"`
	EarnedNanos    decimal.Decimal `gorm:"type:numeric;not null"`
	EarnedCurrency money.Currency  `gorm:"size:3;not null"`
}

func legacyRewardAmount(quota int64) (money.Amount, error) {
	if quota < 0 {
		return money.Amount{}, fmt.Errorf("negative legacy reward")
	}
	value, err := money.NewRationalValue(big.NewRat(quota, 500000), "USD")
	if err != nil {
		return money.Amount{}, err
	}
	return value.Credit()
}

// migrateUserRewards moves outstanding invitation obligations once and retires
// the integer writers. The user wallet opening does not include aff_quota.
func migrateUserRewards(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.AutoMigrate(&ReferralOpening{}, &Option{}); err != nil {
			return err
		}
		if err := tx.Exec(`LOCK TABLE "options" IN ACCESS EXCLUSIVE MODE`).Error; err != nil {
			return err
		}
		if err := migrateRegistrationRewardOptions(tx); err != nil {
			return err
		}
		if !tx.Migrator().HasTable("users") {
			return nil
		}
		if err := tx.Exec(`LOCK TABLE "users" IN ACCESS EXCLUSIVE MODE`).Error; err != nil {
			return err
		}
		hasAvailable, hasHistory := tx.Migrator().HasColumn("users", "aff_quota"), tx.Migrator().HasColumn("users", "aff_history")
		if hasAvailable != hasHistory {
			return fmt.Errorf("incomplete legacy invitation reward columns")
		}
		if hasAvailable {
			var rows []struct {
				ID         int
				AffQuota   int64
				AffHistory int64
			}
			if err := tx.Table("users").Select("id", "aff_quota", "aff_history").Order("id").Find(&rows).Error; err != nil {
				return err
			}
			for _, row := range rows {
				available, err := legacyRewardAmount(row.AffQuota)
				if err != nil {
					return fmt.Errorf("user %d invitation balance: %w", row.ID, err)
				}
				if row.AffHistory < 0 {
					return fmt.Errorf("user %d invitation history is negative", row.ID)
				}
				// The released unit is exactly 2,000 USD nanos. Cumulative
				// history is not a payable balance and has no int64 nanos limit.
				earned := decimal.NewFromInt(row.AffHistory).Mul(decimal.NewFromInt(2000))
				if available.Nanos == 0 && earned.IsZero() {
					continue
				}
				if SettlementCurrency() != "USD" {
					return fmt.Errorf("legacy invitation obligations require USD settlement")
				}
				opening := ReferralOpening{UserID: row.ID, Available: available, EarnedNanos: earned, EarnedCurrency: "USD"}
				var stored ReferralOpening
				err = tx.First(&stored, "user_id = ?", row.ID).Error
				if err == nil {
					if stored.UserID != opening.UserID || stored.Available != opening.Available || stored.EarnedCurrency != opening.EarnedCurrency || !stored.EarnedNanos.Equal(opening.EarnedNanos) {
						return fmt.Errorf("conflicting invitation opening for user %d", row.ID)
					}
				} else if errors.Is(err, gorm.ErrRecordNotFound) {
					if err := tx.Create(&opening).Error; err != nil {
						return err
					}
				} else {
					return err
				}
				if available.Nanos == 0 {
					continue
				}
				key := fmt.Sprintf("referral-opening:%d", row.ID)
				var entry LedgerEntry
				err = tx.Where("idempotency_key = ?", key).Take(&entry).Error
				if err == nil {
					var wallet Wallet
					if err := tx.First(&wallet, entry.WalletId).Error; err != nil {
						return err
					}
					if wallet.AccountType != LedgerAccountUserReferral || wallet.OwnerId != row.ID || entry.Type != LedgerEntryOpeningBalance || entry.Amount != available {
						return fmt.Errorf("conflicting invitation opening ledger for user %d", row.ID)
					}
				} else if errors.Is(err, gorm.ErrRecordNotFound) {
					posting := LedgerPosting{Account: userReferralAccount(row.ID), Type: LedgerEntryOpeningBalance, Amount: available, IdempotencyKey: key, Remark: "Historical invitation reward opening balance"}
					if err := validateLedgerPosting(posting); err != nil {
						return err
					}
					if _, err := postLedgerEntries(tx, []LedgerPosting{posting}); err != nil {
						return err
					}
				} else {
					return err
				}
			}
			if err := tx.Exec(`ALTER TABLE "users" DROP COLUMN "aff_quota", DROP COLUMN "aff_history"`).Error; err != nil {
				return err
			}
		}
		for _, statement := range []string{
			`CREATE OR REPLACE FUNCTION "referral_openings_immutable"() RETURNS trigger AS $$
BEGIN
 RAISE EXCEPTION 'referral_openings is immutable';
END
$$ LANGUAGE plpgsql`,
			`DROP TRIGGER IF EXISTS "referral_openings_immutable" ON "referral_openings"`,
			`CREATE TRIGGER "referral_openings_immutable" BEFORE UPDATE OR DELETE OR TRUNCATE ON "referral_openings" FOR EACH STATEMENT EXECUTE FUNCTION "referral_openings_immutable"()`,
		} {
			if err := tx.Exec(statement).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func migrateRegistrationRewardOptions(tx *gorm.DB) error {
	keys := []string{"QuotaForNewUser", "QuotaForInviter", "QuotaForInvitee"}
	var options []Option
	if err := tx.Where(`"key" IN ?`, append(keys, "reward_setting.config")).Find(&options).Error; err != nil {
		return err
	}
	stored := make(map[string]string, len(options))
	for _, item := range options {
		stored[item.Key] = item.Value
	}
	next := operation_setting.RewardConfig{Signup: money.Amount{Currency: "USD"}, Inviter: money.Amount{Currency: "USD"}, Invitee: money.Amount{Currency: "USD"}}
	legacy := false
	for i, target := range []*money.Amount{&next.Signup, &next.Inviter, &next.Invitee} {
		text, exists := stored[keys[i]]
		if !exists {
			continue
		}
		legacy = true
		quota, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid legacy reward option %s", keys[i])
		}
		*target, err = legacyRewardAmount(quota)
		if err != nil {
			return fmt.Errorf("legacy reward option %s: %w", keys[i], err)
		}
	}
	if current, exists := stored["reward_setting.config"]; exists {
		var parsed operation_setting.RewardConfig
		if err := common.UnmarshalJsonStr(current, &parsed); err != nil {
			return fmt.Errorf("invalid reward configuration")
		}
		if legacy && next != parsed {
			return fmt.Errorf("conflicting legacy reward configuration")
		}
	} else if legacy {
		encoded, err := common.Marshal(next)
		if err != nil {
			return err
		}
		if err := tx.Create(&Option{Key: "reward_setting.config", Value: string(encoded)}).Error; err != nil {
			return err
		}
	}
	return tx.Where(`"key" IN ?`, keys).Delete(&Option{}).Error
}
