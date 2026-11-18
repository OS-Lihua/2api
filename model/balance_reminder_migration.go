package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

type balanceReminderMigration struct {
	ID          int   `gorm:"primaryKey;autoIncrement:false"`
	CompletedAt int64 `gorm:"not null"`
}

// legacyReminderNanos is the ONLY quota boundary for reminders. A fractional
// comparison threshold rounds up to the next nano, preserving integer crossings.
func legacyReminderNanos(raw []byte) (int64, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > 100 || raw[0] == '"' {
		return 0, errors.New("invalid legacy reminder threshold")
	}
	var number json.Number
	if err := common.DecodeJsonStrict(bytes.NewReader(raw), &number); err != nil {
		return 0, errors.New("invalid legacy reminder threshold")
	}
	value, err := decimal.NewFromString(number.String())
	if err != nil || value.IsNegative() || value.Exponent() > 100 || value.Exponent() < -100 {
		return 0, errors.New("invalid legacy reminder threshold")
	}
	nanos := value.Mul(decimal.NewFromInt(2000)).Ceil()
	if nanos.GreaterThan(decimal.NewFromInt(math.MaxInt64)) {
		return 0, errors.New("legacy reminder threshold overflow")
	}
	return nanos.IntPart(), nil
}

func migrateReminderSchema(tx *gorm.DB) error {
	if err := tx.AutoMigrate(&UserBalanceReminder{}, &balanceReminderEvent{}, &balanceReminderWindow{}, &balanceReminderMigration{}); err != nil {
		return err
	}
	for _, statement := range []string{
		`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='balance_reminder_user_threshold') THEN ALTER TABLE user_balance_reminders ADD CONSTRAINT balance_reminder_user_threshold CHECK ((threshold_nanos IS NULL AND threshold_currency IS NULL) OR (threshold_nanos IS NOT NULL AND threshold_nanos>=0 AND threshold_currency IS NOT NULL AND threshold_currency ~ '^[A-Z]{3}$')); END IF; END $$`,
		`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='balance_reminder_event_facts') THEN ALTER TABLE balance_reminder_events ADD CONSTRAINT balance_reminder_event_facts CHECK (scope IN ('wallet','subscription') AND threshold_nanos>=0 AND before_nanos=trunc(before_nanos) AND after_nanos=trunc(after_nanos) AND before_nanos>=threshold_nanos AND after_nanos<threshold_nanos AND ((scope='wallet' AND period_id=0 AND subscription_id=0) OR (scope='subscription' AND period_id>0 AND subscription_id>0)) AND status IN ('pending','delivering','retry','sent','skipped')); END IF; END $$`,
		`CREATE OR REPLACE FUNCTION protect_balance_reminder_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF TG_OP<>'UPDATE' THEN RAISE EXCEPTION 'reminder event identities are durable'; END IF; IF (to_jsonb(OLD)-ARRAY['status','next_attempt_at','claim_token','runner_id','lease_until','attempts','code']) IS DISTINCT FROM (to_jsonb(NEW)-ARRAY['status','next_attempt_at','claim_token','runner_id','lease_until','attempts','code']) THEN RAISE EXCEPTION 'reminder event facts are immutable'; END IF; RETURN NEW; END $$`,
		`DROP TRIGGER IF EXISTS balance_reminder_event_facts ON balance_reminder_events`,
		`CREATE TRIGGER balance_reminder_event_facts BEFORE UPDATE OR DELETE ON balance_reminder_events FOR EACH ROW EXECUTE FUNCTION protect_balance_reminder_event()`,
		`DROP TRIGGER IF EXISTS balance_reminder_event_no_truncate ON balance_reminder_events`,
		`CREATE TRIGGER balance_reminder_event_no_truncate BEFORE TRUNCATE ON balance_reminder_events FOR EACH STATEMENT EXECUTE FUNCTION protect_balance_reminder_event()`,
	} {
		if err := tx.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}

// MigrateBalanceReminders preserves all old source columns/options. Its marker
// covers only configuration conversion; no historical consumption is replayed.
func MigrateBalanceReminders(db *gorm.DB) error {
	if db == nil || db.Dialector.Name() != "postgres" {
		return errors.New("balance reminders require PostgreSQL")
	}
	return db.Session(&gorm.Session{Logger: logger.Discard}).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(510048)").Error; err != nil {
			return err
		}
		if err := tx.AutoMigrate(&Option{}); err != nil {
			return err
		}
		if err := migrateReminderSchema(tx); err != nil {
			return err
		}
		var invalidPreferences int64
		if err := tx.Model(&UserBalanceReminder{}).Where("user_id <= 0 OR (threshold_currency IS NOT NULL AND threshold_currency <> ?)", SettlementCurrency()).Count(&invalidPreferences).Error; err != nil {
			return err
		}
		if invalidPreferences != 0 {
			return errors.New("invalid confirmed reminder preferences")
		}
		var completed int64
		if err := tx.Model(&balanceReminderMigration{}).Where("id = 1").Count(&completed).Error; err != nil {
			return err
		}
		if completed != 0 {
			_, err := balanceReminderSite(tx)
			return err
		}
		if err := tx.Exec("LOCK TABLE options IN SHARE ROW EXCLUSIVE MODE").Error; err != nil {
			return err
		}
		var options []Option
		if err := tx.Where("key IN ?", []string{operation_setting.BalanceReminderConfigOption, "QuotaRemindEnabled", "QuotaRemindThreshold"}).Find(&options).Error; err != nil {
			return err
		}
		values := map[string]string{}
		for _, option := range options {
			if _, exists := values[option.Key]; exists {
				return errors.New("ambiguous reminder options")
			}
			values[option.Key] = option.Value
		}
		var site operation_setting.BalanceReminderConfig
		if value, exists := values[operation_setting.BalanceReminderConfigOption]; exists {
			var err error
			site, err = operation_setting.ParseBalanceReminderConfig(value)
			if err != nil {
				return errors.New("invalid reminder configuration")
			}
		} else {
			site = operation_setting.BalanceReminderConfig{Enabled: true, Threshold: money.Amount{Nanos: 2_000_000, Currency: SettlementCurrency()}}
			legacy := false
			if value, exists := values["QuotaRemindEnabled"]; exists {
				legacy = true
				if value != "true" && value != "false" {
					return errors.New("invalid legacy reminder switch")
				}
				site.Enabled = value == "true"
			}
			if value, exists := values["QuotaRemindThreshold"]; exists {
				legacy = true
				nanos, err := legacyReminderNanos([]byte(value))
				if err != nil {
					return err
				}
				site.Threshold.Nanos = nanos
			}
			if legacy && SettlementCurrency() != money.Currency("USD") {
				return errors.New("legacy reminder currency mismatch")
			}
			encoded, err := common.Marshal(site)
			if err != nil {
				return err
			}
			if err := tx.Create(&Option{Key: operation_setting.BalanceReminderConfigOption, Value: string(encoded)}).Error; err != nil {
				return err
			}
		}
		if err := ValidateBalanceReminderSiteConfig(site); err != nil {
			return err
		}
		if tx.Migrator().HasTable(&User{}) {
			if err := tx.Exec("LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE").Error; err != nil {
				return err
			}
			var lastID int
			first := true
			for {
				var users []struct {
					ID      int
					Setting string
				}
				query := tx.Table("users").Select("id,setting").Order("id").Limit(250)
				if !first {
					query = query.Where("id > ?", lastID)
				}
				first = false
				if err := query.Scan(&users).Error; err != nil {
					return err
				}
				if len(users) == 0 {
					break
				}
				for _, user := range users {
					if user.ID <= 0 {
						return errors.New("invalid legacy reminder user identity")
					}
					lastID = user.ID
					var existing int64
					if err := tx.Model(&UserBalanceReminder{}).Where("user_id = ?", user.ID).Count(&existing).Error; err != nil {
						return err
					}
					if existing != 0 {
						continue
					}
					raw := map[string]json.RawMessage{}
					if strings.TrimSpace(user.Setting) != "" {
						if err := common.DecodeJsonStrict(strings.NewReader(user.Setting), &raw); err != nil || raw == nil {
							return fmt.Errorf("invalid legacy reminder settings for user %d", user.ID)
						}
					}
					row := UserBalanceReminder{UserID: user.ID, Enabled: true}
					if value, exists := raw["quota_warning_enabled"]; exists && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
						if err := common.DecodeJsonStrict(bytes.NewReader(value), &row.Enabled); err != nil {
							return fmt.Errorf("invalid legacy reminder switch for user %d", user.ID)
						}
					}
					if value, exists := raw["quota_warning_threshold"]; exists && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
						nanos, err := legacyReminderNanos(value)
						if err != nil {
							return fmt.Errorf("invalid legacy reminder threshold for user %d", user.ID)
						}
						if nanos != 0 {
							if SettlementCurrency() != money.Currency("USD") {
								return fmt.Errorf("legacy reminder currency mismatch for user %d", user.ID)
							}
							currency := money.Currency("USD")
							row.ThresholdNanos = &nanos
							row.ThresholdCurrency = &currency
						}
					}
					if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
						return err
					}
				}
			}
		}
		return tx.Create(&balanceReminderMigration{ID: 1, CompletedAt: clock.Now().UnixMilli()}).Error
	})
}
