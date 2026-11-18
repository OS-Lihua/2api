package model

import (
	"fmt"
	"math/big"

	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
	"gorm.io/gorm"
)

// SubscriptionPlanMoneyOpening preserves the released terms without treating
// the plan's purchase price as the value of its independently granted allowance.
type SubscriptionPlanMoneyOpening struct {
	PlanID         int          `gorm:"primaryKey;autoIncrement:false"`
	SourcePrice    string       `gorm:"type:text;not null"`
	SourceCurrency string       `gorm:"type:text;not null"`
	SourceTotal    int64        `gorm:"not null"`
	Price          money.Amount `gorm:"embedded;embeddedPrefix:price_"`
	Allowance      money.Amount `gorm:"embedded;embeddedPrefix:allowance_"`
	Unlimited      bool         `gorm:"not null"`
	ConvertedAt    int64        `gorm:"not null"`
}

// SubscriptionMoneyOpening records the historical aggregate debit position,
// which may include unverifiable preconsumes. It is not proof of final usage,
// a new request charge, a wallet credit, or a refundable reservation.
type SubscriptionMoneyOpening struct {
	SubscriptionID int          `gorm:"primaryKey;autoIncrement:false"`
	PlanID         int          `gorm:"not null"`
	PeriodID       int64        `gorm:"not null;uniqueIndex"`
	SourceTotal    int64        `gorm:"not null"`
	SourceUsed     int64        `gorm:"not null"`
	Allowance      money.Amount `gorm:"embedded;embeddedPrefix:allowance_"`
	Consumed       money.Amount `gorm:"embedded;embeddedPrefix:consumed_"`
	Unlimited      bool         `gorm:"not null"`
	ConvertedAt    int64        `gorm:"not null"`
}

// LegacySubscriptionObligation preserves a released request's ambiguous funding
// evidence. "consumed" was not a final-settlement marker. Neither its original
// period nor a refundable/final amount can be inferred from the current instance.
// Resolution requires new verified evidence, never a rewrite of this snapshot.
type LegacySubscriptionObligation struct {
	SourceRecordID    int    `gorm:"primaryKey;autoIncrement:false"`
	RequestID         string `gorm:"type:varchar(64);not null;index"`
	UserID            int    `gorm:"not null;index"`
	SubscriptionID    int    `gorm:"not null;index"`
	SourcePreConsumed int64  `gorm:"not null"`
	SourceStatus      string `gorm:"type:text;not null"`
	SourceCreatedAt   int64  `gorm:"not null"`
	SourceUpdatedAt   int64  `gorm:"not null"`
	State             string `gorm:"type:varchar(32);not null;index"`
	CapturedAt        int64  `gorm:"not null"`
}

// GetLegacySubscriptionReconciliationPending decorates already-authorized
// subscription IDs; it does not select an audience or infer final consumption.
func GetLegacySubscriptionReconciliationPending(db *gorm.DB, subscriptionIDs []int) (map[int]bool, error) {
	pending := make(map[int]bool)
	if len(subscriptionIDs) == 0 {
		return pending, nil
	}
	if db == nil {
		return nil, fmt.Errorf("subscription reconciliation requires a database")
	}
	var ids []int
	if err := db.Model(&LegacySubscriptionObligation{}).Distinct("subscription_id").
		Where("subscription_id IN ? AND state = ?", subscriptionIDs, "unverified").
		Pluck("subscription_id", &ids).Error; err != nil {
		return nil, err
	}
	for _, id := range ids {
		pending[id] = true
	}
	// Old cleanup deleted preconsume records regardless of settlement state.
	// A nonzero aggregate opening is not a receipt proving those uses final.
	ids = nil
	if err := db.Model(&SubscriptionMoneyOpening{}).
		Where("subscription_id IN ? AND source_used > 0", subscriptionIDs).
		Pluck("subscription_id", &ids).Error; err != nil {
		return nil, err
	}
	for _, id := range ids {
		pending[id] = true
	}
	ids = nil
	if err := db.Model(&LegacySubscriptionTaskObligation{}).
		Where("subscription_id IN ? AND state = ?", subscriptionIDs, "unverified").
		Distinct("subscription_id").Pluck("subscription_id", &ids).Error; err != nil {
		return nil, err
	}
	for _, id := range ids {
		pending[id] = true
	}
	return pending, nil
}

func migrateLegacySubscriptionObligations(tx *gorm.DB) error {
	if !tx.Migrator().HasTable(&SubscriptionPreConsumeRecord{}) {
		return nil
	}
	type sourceRecord struct {
		ID                   int `gorm:"primaryKey"`
		RequestID            string
		UserID               int
		UserSubscriptionID   int
		PreConsumed          int64
		Status               string
		CreatedAt, UpdatedAt int64
		SubscriptionOwnerID  *int
		ExistingUserID       *int
	}
	var batch []sourceRecord
	return tx.Table("subscription_pre_consume_records AS p").
		Select(`p.*, s."user_id" AS "subscription_owner_id", u."id" AS "existing_user_id"`).
		Joins(`LEFT JOIN "user_subscriptions" s ON s."id"=p."user_subscription_id"`).
		Joins(`LEFT JOIN "users" u ON u."id"=p."user_id"`).
		FindInBatches(&batch, 250, func(_ *gorm.DB, _ int) error {
			ids := make([]int, 0, len(batch))
			for _, source := range batch {
				ids = append(ids, source.ID)
			}
			var existing []LegacySubscriptionObligation
			if err := tx.Where("source_record_id IN ?", ids).Find(&existing).Error; err != nil {
				return err
			}
			byID := make(map[int]LegacySubscriptionObligation, len(existing))
			for _, record := range existing {
				byID[record.SourceRecordID] = record
			}
			pending := make([]LegacySubscriptionObligation, 0, len(batch))
			for _, source := range batch {
				// Released administrators could delete an instance without deleting
				// its preconsume records. Keep that independent evidence, but never
				// accept a different owner when the parent still exists.
				if source.ID <= 0 || source.RequestID == "" || source.UserID <= 0 || source.UserSubscriptionID <= 0 || source.PreConsumed < 0 ||
					source.ExistingUserID == nil || (source.SubscriptionOwnerID != nil && *source.SubscriptionOwnerID != source.UserID) {
					return fmt.Errorf("legacy subscription preconsume %d has invalid source identity or amount", source.ID)
				}
				state := "unverified"
				if source.Status == "refunded" {
					state = "source_refunded"
				}
				record := LegacySubscriptionObligation{
					SourceRecordID: source.ID, RequestID: source.RequestID, UserID: source.UserID,
					SubscriptionID: source.UserSubscriptionID, SourcePreConsumed: source.PreConsumed,
					SourceStatus: source.Status, SourceCreatedAt: source.CreatedAt, SourceUpdatedAt: source.UpdatedAt,
					State: state, CapturedAt: clock.Now().Unix(),
				}
				if prior, found := byID[source.ID]; found {
					record.CapturedAt = prior.CapturedAt
					if record != prior {
						return fmt.Errorf("legacy subscription preconsume %d changed after capture", source.ID)
					}
					continue
				}
				pending = append(pending, record)
			}
			if len(pending) != 0 {
				return tx.Create(&pending).Error
			}
			return nil
		}).Error
}

// legacySubscriptionAllowance is only a source migration: the released quota
// unit is exactly 1/500000 USD. Serving code must never call this conversion.
func legacySubscriptionAllowance(source int64) (money.Amount, error) {
	if source < 0 {
		return money.Amount{}, fmt.Errorf("negative legacy subscription allowance")
	}
	value, err := money.NewRationalValue(big.NewRat(source, 500000), "USD")
	if err != nil {
		return money.Amount{}, err
	}
	return value.Credit()
}

// MigrateSubscriptionMoney converts every released instance once, including
// expired and cancelled instances. It does not recalculate from current plans,
// reset an existing period, or interpret legacy pre-consumes as new requests.
func MigrateSubscriptionMoney(db *gorm.DB) error {
	if db == nil || db.Dialector.Name() != "postgres" {
		return fmt.Errorf("subscription money migration requires PostgreSQL")
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SELECT pg_advisory_xact_lock(510046)`).Error; err != nil {
			return err
		}
		// The migration owns schema changes. Acquire existing financial tables
		// before parent subscription tables, matching the serving wallet-first order.
		for _, table := range []string{"wallets", "ledger_entries", "users", "subscription_plans", "user_subscriptions", "subscription_pre_consume_records", "tasks"} {
			if tx.Migrator().HasTable(table) {
				if err := tx.Exec(fmt.Sprintf(`LOCK TABLE "%s" IN ACCESS EXCLUSIVE MODE`, table)).Error; err != nil {
					return err
				}
			}
		}
		if err := migrateWalletLedger(tx); err != nil {
			return err
		}
		if err := tx.AutoMigrate(&SubscriptionPlanMoneyOpening{}, &SubscriptionMoneyOpening{}, &LegacySubscriptionObligation{}, &LegacySubscriptionTaskObligation{}); err != nil {
			return err
		}
		if !tx.Migrator().HasTable(&SubscriptionPlan{}) {
			if err := tx.AutoMigrate(&SubscriptionPlan{}); err != nil {
				return err
			}
		} else if err := tx.Exec(`ALTER TABLE "subscription_plans"
 ADD COLUMN IF NOT EXISTS "price_nanos" bigint,
 ADD COLUMN IF NOT EXISTS "price_currency" char(3),
 ADD COLUMN IF NOT EXISTS "allowance_nanos" bigint,
 ADD COLUMN IF NOT EXISTS "allowance_currency" char(3),
 ADD COLUMN IF NOT EXISTS "unlimited" boolean NOT NULL DEFAULT false,
 ADD COLUMN IF NOT EXISTS "money_version" integer NOT NULL DEFAULT 0`).Error; err != nil {
			return err
		}
		if !tx.Migrator().HasTable(&UserSubscription{}) {
			if err := tx.AutoMigrate(&UserSubscription{}); err != nil {
				return err
			}
		} else if err := tx.Exec(`ALTER TABLE "user_subscriptions"
 ADD COLUMN IF NOT EXISTS "allowance_nanos" bigint,
 ADD COLUMN IF NOT EXISTS "allowance_currency" char(3),
 ADD COLUMN IF NOT EXISTS "unlimited" boolean NOT NULL DEFAULT false,
 ADD COLUMN IF NOT EXISTS "money_version" integer NOT NULL DEFAULT 0,
 ADD COLUMN IF NOT EXISTS "current_period_id" bigint NOT NULL DEFAULT 0`).Error; err != nil {
			return err
		}

		var legacyPlans int64
		if err := tx.Table("subscription_plans").Where(`COALESCE("money_version", 0) = 0`).Count(&legacyPlans).Error; err != nil {
			return err
		}
		if legacyPlans > 0 {
			for _, column := range []string{"price_amount", "currency", "total_amount"} {
				if !tx.Migrator().HasColumn("subscription_plans", column) {
					return fmt.Errorf("legacy subscription plan source is incomplete")
				}
			}
			var sources []struct {
				ID             int
				SourcePrice    *string
				SourceCurrency *string
				SourceTotal    *int64
			}
			if err := tx.Table("subscription_plans").Select(`"id", "price_amount"::text AS "source_price", "currency" AS "source_currency", "total_amount" AS "source_total"`).Where(`COALESCE("money_version", 0) = 0`).Order("id").Find(&sources).Error; err != nil {
				return err
			}
			for _, source := range sources {
				if source.SourcePrice == nil || source.SourceCurrency == nil || source.SourceTotal == nil {
					return fmt.Errorf("subscription plan %d has incomplete source terms", source.ID)
				}
				currency, err := money.ParseCurrency(*source.SourceCurrency)
				if err != nil {
					return fmt.Errorf("subscription plan %d has invalid source currency", source.ID)
				}
				value, err := money.ParseValue(*source.SourcePrice, currency)
				if err != nil {
					return fmt.Errorf("subscription plan %d has invalid source price: %w", source.ID, err)
				}
				comparison, err := value.Compare(money.Amount{Currency: currency}.Value())
				if err != nil || comparison < 0 {
					return fmt.Errorf("subscription plan %d has negative source price", source.ID)
				}
				price, err := value.Receivable()
				if err != nil {
					return err
				}
				allowance, err := legacySubscriptionAllowance(*source.SourceTotal)
				if err != nil {
					return fmt.Errorf("subscription plan %d: %w", source.ID, err)
				}
				if SettlementCurrency() != allowance.Currency {
					return fmt.Errorf("legacy subscription allowances require USD settlement")
				}
				opening := SubscriptionPlanMoneyOpening{PlanID: source.ID, SourcePrice: *source.SourcePrice, SourceCurrency: *source.SourceCurrency, SourceTotal: *source.SourceTotal, Price: price, Allowance: allowance, Unlimited: *source.SourceTotal == 0, ConvertedAt: clock.Now().Unix()}
				if err := tx.Create(&opening).Error; err != nil {
					return err
				}
				if err := tx.Table("subscription_plans").Where("id = ?", source.ID).Updates(map[string]any{"price_nanos": price.Nanos, "price_currency": price.Currency, "allowance_nanos": allowance.Nanos, "allowance_currency": allowance.Currency, "unlimited": opening.Unlimited, "money_version": 1}).Error; err != nil {
					return err
				}
			}
		}

		var legacySubscriptions int64
		if err := tx.Table("user_subscriptions").Where(`COALESCE("money_version", 0) = 0`).Count(&legacySubscriptions).Error; err != nil {
			return err
		}
		var openings []SubscriptionMoneyOpening
		if legacySubscriptions > 0 {
			for _, column := range []string{"amount_total", "amount_used"} {
				if !tx.Migrator().HasColumn("user_subscriptions", column) {
					return fmt.Errorf("legacy subscription instance source is incomplete")
				}
			}
			var sources []struct {
				ID, PlanID              int
				SourceTotal, SourceUsed *int64
				CurrentPeriodID         int64
			}
			if err := tx.Table("user_subscriptions").Select(`"id", "plan_id", "amount_total" AS "source_total", "amount_used" AS "source_used", "current_period_id"`).Where(`COALESCE("money_version", 0) = 0`).Order("id").Find(&sources).Error; err != nil {
				return err
			}
			for _, source := range sources {
				if source.SourceTotal == nil || source.SourceUsed == nil || source.CurrentPeriodID != 0 {
					return fmt.Errorf("subscription %d has incomplete or conflicting source terms", source.ID)
				}
				allowance, err := legacySubscriptionAllowance(*source.SourceTotal)
				if err != nil {
					return fmt.Errorf("subscription %d allowance: %w", source.ID, err)
				}
				consumed, err := legacySubscriptionAllowance(*source.SourceUsed)
				if err != nil {
					return fmt.Errorf("subscription %d consumption: %w", source.ID, err)
				}
				if *source.SourceTotal != 0 && consumed.Nanos > allowance.Nanos {
					return fmt.Errorf("subscription %d consumption exceeds its finite allowance", source.ID)
				}
				if SettlementCurrency() != allowance.Currency {
					return fmt.Errorf("legacy subscription allowances require USD settlement")
				}
				opening := SubscriptionMoneyOpening{SubscriptionID: source.ID, PlanID: source.PlanID, SourceTotal: *source.SourceTotal, SourceUsed: *source.SourceUsed, Allowance: allowance, Consumed: consumed, Unlimited: *source.SourceTotal == 0, ConvertedAt: clock.Now().Unix()}
				openings = append(openings, opening)
				if err := tx.Table("user_subscriptions").Where("id = ?", source.ID).Updates(map[string]any{"allowance_nanos": allowance.Nanos, "allowance_currency": allowance.Currency, "unlimited": opening.Unlimited, "money_version": 1}).Error; err != nil {
					return err
				}
			}
		}
		// Retained source values are read-only evidence. New canonical records must
		// neither require a legacy price nor acquire fabricated zero-quota defaults.
		for _, source := range []struct {
			table   string
			columns []string
		}{
			{"subscription_plans", []string{"price_amount", "currency", "total_amount"}},
			{"user_subscriptions", []string{"amount_total", "amount_used"}},
		} {
			for _, column := range source.columns {
				if tx.Migrator().HasColumn(source.table, column) {
					statement := fmt.Sprintf(`ALTER TABLE "%s" ALTER COLUMN "%s" DROP NOT NULL, ALTER COLUMN "%s" DROP DEFAULT`, source.table, column, column)
					if err := tx.Exec(statement).Error; err != nil {
						return err
					}
				}
			}
		}
		// NOT NULL is added only after every released row has an exact value.
		if err := tx.AutoMigrate(&SubscriptionPlan{}, &UserSubscription{}, &SubscriptionOrderMoney{}); err != nil {
			return err
		}
		if err := MigrateSubscriptionFunding(tx); err != nil {
			return err
		}
		for _, opening := range openings {
			var subscription UserSubscription
			if err := tx.First(&subscription, opening.SubscriptionID).Error; err != nil {
				return err
			}
			if err := createSubscriptionPeriodTx(tx, &subscription, opening.Consumed, "legacy_opening"); err != nil {
				return fmt.Errorf("subscription %d period: %w", opening.SubscriptionID, err)
			}
			opening.PeriodID = subscription.CurrentPeriodID
			if err := tx.Create(&opening).Error; err != nil {
				return err
			}
		}
		for _, table := range []string{"subscription_plans", "user_subscriptions"} {
			var invalid int64
			if err := tx.Table(table).Where(`"money_version" IS DISTINCT FROM 1`).Count(&invalid).Error; err != nil {
				return err
			}
			if invalid != 0 {
				return fmt.Errorf("unsupported subscription money version in %s", table)
			}
		}
		if err := migrateLegacySubscriptionObligations(tx); err != nil {
			return err
		}
		if err := migrateLegacySubscriptionTaskObligations(tx); err != nil {
			return err
		}
		for _, table := range []string{"subscription_plan_money_openings", "subscription_money_openings", "legacy_subscription_obligations", "legacy_subscription_task_obligations"} {
			for _, statement := range []string{
				fmt.Sprintf(`CREATE OR REPLACE FUNCTION "%[1]s_immutable"() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION '%[1]s is immutable'; END $$ LANGUAGE plpgsql`, table),
				fmt.Sprintf(`DROP TRIGGER IF EXISTS "%[1]s_immutable" ON "%[1]s"`, table),
				fmt.Sprintf(`CREATE TRIGGER "%[1]s_immutable" BEFORE UPDATE OR DELETE OR TRUNCATE ON "%[1]s" FOR EACH STATEMENT EXECUTE FUNCTION "%[1]s_immutable"()`, table),
			} {
				if err := tx.Exec(statement).Error; err != nil {
					return err
				}
			}
		}
		return migrateSubscriptionOrderMoneyProtection(tx)
	})
}

func migrateSubscriptionOrderMoneyProtection(tx *gorm.DB) error {
	for _, statement := range []string{
		`CREATE OR REPLACE FUNCTION "subscription_order_moneys_immutable"() RETURNS trigger AS $$
 BEGIN
  IF TG_OP <> 'UPDATE' THEN
   RAISE EXCEPTION 'subscription payment contract is immutable';
  END IF;
  IF (to_jsonb(NEW) - 'user_subscription_id') IS DISTINCT FROM (to_jsonb(OLD) - 'user_subscription_id')
   OR (NEW.user_subscription_id = OLD.user_subscription_id
    OR (OLD.user_subscription_id = 0 AND NEW.user_subscription_id > 0)) IS NOT TRUE THEN
   RAISE EXCEPTION 'subscription payment contract is immutable';
  END IF;
  RETURN NEW;
 END $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS "subscription_order_moneys_immutable" ON "subscription_order_moneys"`,
		`CREATE TRIGGER "subscription_order_moneys_immutable" BEFORE UPDATE OR DELETE ON "subscription_order_moneys" FOR EACH ROW EXECUTE FUNCTION "subscription_order_moneys_immutable"()`,
		`DROP TRIGGER IF EXISTS "subscription_order_moneys_no_truncate" ON "subscription_order_moneys"`,
		`CREATE TRIGGER "subscription_order_moneys_no_truncate" BEFORE TRUNCATE ON "subscription_order_moneys" FOR EACH STATEMENT EXECUTE FUNCTION "subscription_order_moneys_immutable"()`,
	} {
		if err := tx.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
