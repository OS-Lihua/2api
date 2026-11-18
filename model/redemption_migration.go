package model

import (
	"fmt"
	"math/big"

	"github.com/QuantumNous/new-api/pkg/money"
	"gorm.io/gorm"
)

// migrateRedemptionAmounts preserves historical vouchers in their original USD
// denomination. FX is explicit at redemption, never guessed during migration.
// Dropping quota in this transaction is the once-only conversion marker.
func migrateRedemptionAmounts(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if !tx.Migrator().HasTable(&Redemption{}) {
			return tx.AutoMigrate(&Redemption{})
		}
		if err := tx.Exec(`LOCK TABLE "redemptions" IN ACCESS EXCLUSIVE MODE`).Error; err != nil {
			return err
		}
		legacy := tx.Migrator().HasColumn(&Redemption{}, "quota")
		if legacy {
			// Existing rows need nullable columns until every obligation is
			// validated and populated. AutoMigrate then enforces NOT NULL.
			if err := tx.Exec(`ALTER TABLE "redemptions"
				ADD COLUMN IF NOT EXISTS "amount_nanos" bigint,
				ADD COLUMN IF NOT EXISTS "amount_currency" char(3)`).Error; err != nil {
				return err
			}
			var rows []struct {
				Id             int
				Quota          int64
				AmountNanos    *int64
				AmountCurrency *string
			}
			// Read no code credentials, and include soft-deleted history.
			if err := tx.Table("redemptions").Select("id", "quota", "amount_nanos", "amount_currency").Find(&rows).Error; err != nil {
				return err
			}
			for _, row := range rows {
				if row.Quota <= 0 {
					return fmt.Errorf("migrate redemption amount: invalid legacy amount for code ID %d", row.Id)
				}
				value, err := money.NewRationalValue(big.NewRat(row.Quota, 500000), "USD")
				if err != nil {
					return fmt.Errorf("migrate redemption amount for code ID %d: %w", row.Id, err)
				}
				amount, err := value.Credit()
				if err != nil {
					return err
				}
				if row.AmountNanos != nil || row.AmountCurrency != nil {
					if row.AmountNanos == nil || row.AmountCurrency == nil || *row.AmountNanos != amount.Nanos || *row.AmountCurrency != string(amount.Currency) {
						return fmt.Errorf("migrate redemption amount: conflicting amount for code ID %d", row.Id)
					}
					continue
				}
				if err := tx.Table("redemptions").Where("id = ?", row.Id).
					Updates(map[string]any{"amount_nanos": amount.Nanos, "amount_currency": amount.Currency}).Error; err != nil {
					return err
				}
			}
		}
		if err := tx.AutoMigrate(&Redemption{}); err != nil {
			return err
		}
		if legacy {
			return tx.Migrator().DropColumn(&Redemption{}, "quota")
		}
		return nil
	})
}
