package model

import (
	"fmt"
	"math/big"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"gorm.io/gorm"
)

// migrateCheckinAmounts converts historical records and reward options together.
// Historical signed quota is exact USD history, already reflected in legacy
// balances. No FX, wallet or ledger writes belong in this migration.
func migrateCheckinAmounts(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.AutoMigrate(&Option{}); err != nil {
			return err
		}
		if err := tx.Exec(`LOCK TABLE "options" IN ACCESS EXCLUSIVE MODE`).Error; err != nil {
			return err
		}
		legacy := false
		if tx.Migrator().HasTable(&Checkin{}) {
			if err := tx.Exec(`LOCK TABLE "checkins" IN ACCESS EXCLUSIVE MODE`).Error; err != nil {
				return err
			}
			legacy = tx.Migrator().HasColumn(&Checkin{}, "quota_awarded")
		}
		if legacy {
			if err := tx.Exec(`ALTER TABLE "checkins"
				ADD COLUMN IF NOT EXISTS "amount_nanos" bigint,
				ADD COLUMN IF NOT EXISTS "amount_currency" char(3)`).Error; err != nil {
				return err
			}
			var rows []struct {
				Id             int
				QuotaAwarded   int64
				AmountNanos    *int64
				AmountCurrency *string
			}
			if err := tx.Table("checkins").Select("id", "quota_awarded", "amount_nanos", "amount_currency").Find(&rows).Error; err != nil {
				return err
			}
			for _, row := range rows {
				value, err := money.NewRationalValue(big.NewRat(row.QuotaAwarded, 500000), "USD")
				if err != nil {
					return fmt.Errorf("migrate check-in amount for ID %d: %w", row.Id, err)
				}
				amount, err := value.Credit()
				if err != nil {
					return err
				}
				if row.AmountNanos != nil || row.AmountCurrency != nil {
					if row.AmountNanos == nil || row.AmountCurrency == nil || *row.AmountNanos != amount.Nanos || *row.AmountCurrency != string(amount.Currency) {
						return fmt.Errorf("migrate check-in amount: conflicting amount for ID %d", row.Id)
					}
					continue
				}
				if err := tx.Table("checkins").Where("id = ?", row.Id).
					Updates(map[string]any{"amount_nanos": amount.Nanos, "amount_currency": amount.Currency}).Error; err != nil {
					return err
				}
			}
		}
		if err := migrateCheckinRewardOptions(tx); err != nil {
			return err
		}
		if err := tx.AutoMigrate(&Checkin{}); err != nil {
			return err
		}
		if legacy {
			return tx.Migrator().DropColumn(&Checkin{}, "quota_awarded")
		}
		return nil
	})
}

// migrateCheckinRewardOptions refuses ambiguous or out-of-range legacy settings.
// An absent old bound uses its released default, matching the old configuration.
func migrateCheckinRewardOptions(tx *gorm.DB) error {
	const rangeKey = "checkin_setting.reward_range"
	oldKeys := []string{"checkin_setting.min_quota", "checkin_setting.max_quota"}
	var options []Option
	if err := tx.Where(`"key" IN ?`, append(oldKeys, rangeKey)).Find(&options).Error; err != nil {
		return err
	}
	stored := make(map[string]string, len(options))
	for _, option := range options {
		stored[option.Key] = option.Value
	}
	var current operation_setting.CheckinRewardRange
	currentText, hasCurrent := stored[rangeKey]
	if hasCurrent {
		if err := common.UnmarshalJsonStr(currentText, &current); err != nil {
			return fmt.Errorf("migrate check-in reward range: invalid current range")
		}
	}
	_, hasMin := stored[oldKeys[0]]
	_, hasMax := stored[oldKeys[1]]
	if !hasMin && !hasMax {
		return nil
	}
	bounds := []int64{1000, 10000}
	for i, key := range oldKeys {
		if text, exists := stored[key]; exists {
			quota, err := strconv.ParseInt(text, 10, 64)
			if err != nil || quota < 0 {
				return fmt.Errorf("migrate check-in reward range: invalid legacy %s", key)
			}
			bounds[i] = quota
		}
	}
	converted := operation_setting.CheckinRewardRange{}
	for i, target := range []*money.Amount{&converted.Min, &converted.Max} {
		value, err := money.NewRationalValue(big.NewRat(bounds[i], 500000), "USD")
		if err != nil {
			return fmt.Errorf("migrate check-in reward range: %w", err)
		}
		*target, err = value.Credit()
		if err != nil {
			return err
		}
	}
	if err := converted.Validate(); err != nil {
		return fmt.Errorf("migrate check-in reward range: invalid legacy bounds")
	}
	if hasCurrent && current != converted {
		return fmt.Errorf("migrate check-in reward range: conflicting legacy and current ranges")
	}
	if !hasCurrent {
		encoded, err := common.Marshal(converted)
		if err != nil {
			return err
		}
		if err := tx.Create(&Option{Key: rangeKey, Value: string(encoded)}).Error; err != nil {
			return err
		}
	}
	return tx.Where(`"key" IN ?`, oldKeys).Delete(&Option{}).Error
}
