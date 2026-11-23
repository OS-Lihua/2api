package model

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// TokenOfferingPolicy is separate from credential caches. Absence means allow
// with no exceptions; permission reads always use the primary database.
type TokenOfferingPolicy struct {
	TokenID      int   `json:"-" gorm:"primaryKey;autoIncrement:false"`
	Token        Token `json:"-" gorm:"foreignKey:TokenID;constraint:OnDelete:CASCADE"`
	DefaultAllow bool  `json:"default_allow" gorm:"not null"`
	Revision     int64 `json:"revision" gorm:"not null"`
}

type TokenOfferingException struct {
	TokenID    int           `json:"-" gorm:"primaryKey;autoIncrement:false"`
	Token      Token         `json:"-" gorm:"foreignKey:TokenID;constraint:OnDelete:CASCADE"`
	OfferingID int           `json:"-" gorm:"primaryKey;autoIncrement:false;index"`
	Offering   ModelOffering `json:"-" gorm:"foreignKey:OfferingID;constraint:OnDelete:CASCADE"`
}

type KeyPermissions struct {
	DefaultAllow bool  `json:"default_allow"`
	ExceptionIDs []int `json:"exception_ids"`
	Revision     int64 `json:"revision"`
}

var (
	ErrKeyPermissionsConflict = errors.New("key permissions changed")
	ErrLegacyKeyPermissions   = errors.New("legacy Key permissions must be migrated by the master before serving")
	ErrLegacyKeyQuota         = errors.New("legacy Key quota requires offline migration; stop all nodes and run migrate-legacy")
)

type legacyKeyColumns struct {
	ModelLimits int64
	Routing     int64
	Quota       int64
}

func inspectLegacyKeyColumns(db *gorm.DB) (legacyKeyColumns, error) {
	var columns legacyKeyColumns
	err := db.Raw(`SELECT
 COUNT(*) FILTER (WHERE attname IN ('model_limits', 'model_limits_enabled')) AS model_limits,
 COUNT(*) FILTER (WHERE attname IN ('group', 'auto_groups', 'cross_group_retry')) AS routing,
 COUNT(*) FILTER (WHERE attname IN ('remain_quota', 'unlimited_quota', 'used_quota', 'allow_user_quota_query')) AS quota
 FROM pg_catalog.pg_attribute
 WHERE attrelid = to_regclass('tokens') AND attnum > 0 AND NOT attisdropped`).Scan(&columns).Error
	return columns, err
}

// MigrateKeyPermissions freezes legacy name allowlists as Offering identities
// and retires the old permission and routing columns in the same transaction.
func MigrateKeyPermissions(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SELECT pg_advisory_xact_lock(510050)`).Error; err != nil {
			return err
		}
		return migrateLegacyKeyPermissionsTx(tx, nil)
	})
}

// The caller holds the schema migration lock. The table lock serializes policy
// changes (which lock the Key row first) and prevents a last legacy write before
// DROP COLUMN. A fresh or already migrated database needs neither write.
func migrateLegacyKeyPermissionsTx(tx *gorm.DB, report *LegacyGatewayMigrationReport) error {
	columns, err := inspectLegacyKeyColumns(tx)
	if err != nil || columns.ModelLimits+columns.Routing == 0 {
		return err
	}
	if columns.ModelLimits != 0 && columns.ModelLimits != 2 {
		return ErrLegacyKeyPermissions
	}
	if err := tx.Exec(`LOCK TABLE "tokens" IN ACCESS EXCLUSIVE MODE`).Error; err != nil {
		return err
	}
	if columns.ModelLimits == 2 {
		if err := freezeLegacyKeyLimits(tx, report); err != nil {
			return err
		}
		if err := tx.Exec(`ALTER TABLE "tokens" DROP COLUMN "model_limits", DROP COLUMN "model_limits_enabled"`).Error; err != nil {
			return err
		}
	}
	if columns.Routing != 0 {
		// Older releases may have only some of these columns. None grants access.
		if err := tx.Exec(`ALTER TABLE "tokens" DROP COLUMN IF EXISTS "group", DROP COLUMN IF EXISTS "auto_groups", DROP COLUMN IF EXISTS "cross_group_retry"`).Error; err != nil {
			return err
		}
	}
	if report != nil {
		report.Counts["legacy_key_limit_columns_retired"] = columns.ModelLimits
		report.Counts["legacy_key_routing_columns_retired"] = columns.Routing
	}
	return nil
}

// retireLegacyKeyQuotaTx runs only after the offline migration has validated
// the database's native or converted proof. Key status and permissions stay intact.
func retireLegacyKeyQuotaTx(tx *gorm.DB, report *LegacyGatewayMigrationReport) error {
	columns, err := inspectLegacyKeyColumns(tx)
	if err != nil || columns.Quota == 0 {
		return err
	}
	if err := tx.Exec(`ALTER TABLE "tokens"
 DROP COLUMN IF EXISTS "remain_quota", DROP COLUMN IF EXISTS "unlimited_quota",
 DROP COLUMN IF EXISTS "used_quota", DROP COLUMN IF EXISTS "allow_user_quota_query"`).Error; err != nil {
		return err
	}
	report.Counts["legacy_key_quota_columns_retired"] = columns.Quota
	return nil
}

func freezeLegacyKeyLimits(tx *gorm.DB, report *LegacyGatewayMigrationReport) error {
	var names []ModelOfferingName
	if err := tx.Select("name", "offering_id").Find(&names).Error; err != nil {
		return err
	}
	offeringIDs := make(map[string]int, len(names))
	for _, name := range names {
		offeringIDs[name.Name] = name.OfferingID
	}
	var tokens []struct {
		Id          int
		ModelLimits string
	}
	return tx.Model(&Token{}).Select(`"id", COALESCE("model_limits", '') AS "model_limits"`).
		Where(`"model_limits_enabled" = TRUE AND NOT EXISTS (SELECT 1 FROM "token_offering_policies" WHERE "token_id" = "tokens"."id")`).
		FindInBatches(&tokens, 100, func(_ *gorm.DB, _ int) error {
			for _, token := range tokens {
				ids := map[int]bool{}
				unknown := map[string]bool{}
				for name := range strings.SplitSeq(token.ModelLimits, ",") {
					if id, known := offeringIDs[name]; known {
						ids[id] = true
					} else if report != nil && name != "" && !unknown[name] {
						unknown[name] = true
						report.Items = append(report.Items, LegacyGatewayMigrationItem{Resource: "key", ID: int64(token.Id), Name: name, Reason: "unknown_model_limit_name"})
					}
				}
				if err := tx.Omit("Token").Create(&TokenOfferingPolicy{TokenID: token.Id, DefaultAllow: false}).Error; err != nil {
					return err
				}
				for _, id := range slices.Sorted(maps.Keys(ids)) {
					if err := tx.Omit("Token", "Offering").Create(&TokenOfferingException{TokenID: token.Id, OfferingID: id}).Error; err != nil {
						return err
					}
				}
				if report != nil {
					if err := tx.Create(&legacyGatewayMigrationMapping{Resource: "key_policy", LegacyID: int64(token.Id), TargetID: int64(token.Id)}).Error; err != nil {
						return err
					}
					report.Counts["key_policies_created"]++
				}
			}
			return nil
		}).Error
}

func GetKeyPermissions(ctx context.Context, tokenID, ownerID int) (*KeyPermissions, error) {
	var permissions *KeyPermissions
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var token Token
		query := lockForUpdate(tx).Select("id").Where("id = ?", tokenID)
		if ownerID != 0 {
			query = query.Where("user_id = ?", ownerID)
		}
		if err := query.First(&token).Error; err != nil {
			return err
		}
		var err error
		permissions, err = readKeyPermissions(tx, tokenID)
		return err
	})
	return permissions, err
}

// MutateKeyPermissions serializes every operation on the credential row. A
// revision prevents an old form or replay from overwriting a newer policy.
func MutateKeyPermissions(ctx context.Context, tokenID, ownerID int, revision int64, action string, replacement KeyPermissions, ids []int) (*KeyPermissions, error) {
	var saved *KeyPermissions
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var token Token
		query := lockForUpdate(tx).Where("id = ?", tokenID)
		if ownerID != 0 {
			query = query.Where("user_id = ?", ownerID)
		}
		if err := query.First(&token).Error; err != nil {
			return err
		}
		current, err := readKeyPermissions(tx, tokenID)
		if err != nil {
			return err
		}
		if current.Revision != revision {
			return ErrKeyPermissionsConflict
		}
		previousIDs := slices.Clone(current.ExceptionIDs)
		switch action {
		case "allow_all", "deny_all":
			current.DefaultAllow = action == "allow_all"
			current.ExceptionIDs = []int{}
		case "replace":
			current.DefaultAllow = replacement.DefaultAllow
			current.ExceptionIDs = slices.Clone(replacement.ExceptionIDs)
		case "add":
			current.ExceptionIDs = append(current.ExceptionIDs, ids...)
		case "remove":
			current.ExceptionIDs = slices.DeleteFunc(current.ExceptionIDs, func(id int) bool { return slices.Contains(ids, id) })
		default:
			return common.NewMessage("Invalid parameters")
		}
		slices.Sort(current.ExceptionIDs)
		current.ExceptionIDs = slices.Compact(current.ExceptionIDs)
		if len(current.ExceptionIDs) > 1000 || len(current.ExceptionIDs) > 0 && current.ExceptionIDs[0] <= 0 {
			return common.NewMessage("Invalid parameters")
		}
		if len(current.ExceptionIDs) > 0 {
			var count int64
			// Retain existing hidden exceptions, but never reveal or add an Offering
			// outside the owner's current visibility boundary.
			var user User
			if err := tx.Select("id", "group").First(&user, token.UserId).Error; err != nil {
				return err
			}
			permitted := tx.Model(&ModelOffering{}).Where("id IN ?", current.ExceptionIDs).Where(`("visibility" = 'all' OR "groups" @> jsonb_build_array(?) OR "id" IN ?)`, user.Group, previousIDs)
			if err := permitted.Count(&count).Error; err != nil {
				return err
			}
			if count != int64(len(current.ExceptionIDs)) {
				return common.NewMessage("Invalid parameters")
			}
		}
		current.Revision++
		policy := TokenOfferingPolicy{TokenID: tokenID, DefaultAllow: current.DefaultAllow, Revision: current.Revision}
		if err := tx.Omit("Token").Save(&policy).Error; err != nil {
			return err
		}
		if err := tx.Where("token_id = ?", tokenID).Delete(&TokenOfferingException{}).Error; err != nil {
			return err
		}
		for _, id := range current.ExceptionIDs {
			exception := TokenOfferingException{TokenID: tokenID, OfferingID: id}
			if err := tx.Omit("Token", "Offering").Create(&exception).Error; err != nil {
				return err
			}
		}
		saved = current
		return nil
	})
	return saved, err
}

// VisibleOfferingsQuery applies live user status and Offering visibility for
// browser sessions as well as API Keys. A cached group cannot retain access.
func VisibleOfferingsQuery(ctx context.Context, userID int) *gorm.DB {
	db := DB.WithContext(ctx)
	users := db.Model(&User{}).Select("id").Where("id = ? AND status = ?", userID, common.UserStatusEnabled)
	return db.Model(&ModelOffering{}).
		Where("enabled = ?", true).
		Where("EXISTS (?)", users).
		Where(`("visibility" = 'all' OR "groups" @> jsonb_build_array((SELECT "group" FROM "users" WHERE "id" = ?)))`, userID)
}

// AllowedOfferingsQuery is the single API Key authorization predicate for
// discovery, detail and execution. Decision inputs use one primary DB snapshot.
func AllowedOfferingsQuery(ctx context.Context, userID, tokenID int) *gorm.DB {
	tokens := DB.WithContext(ctx).Model(&Token{}).Select("id").Where("id = ? AND user_id = ? AND status = ? AND (expired_time = -1 OR expired_time >= ?)", tokenID, userID, common.TokenStatusEnabled, common.GetTimestamp())
	return VisibleOfferingsQuery(ctx, userID).
		Where("EXISTS (?)", tokens).
		Where(`COALESCE((SELECT "default_allow" FROM "token_offering_policies" WHERE "token_id" = ?), TRUE) <> EXISTS (SELECT 1 FROM "token_offering_exceptions" WHERE "token_id" = ? AND "offering_id" = "model_offerings"."id")`, tokenID, tokenID)
}

func readKeyPermissions(db *gorm.DB, tokenID int) (*KeyPermissions, error) {
	policy := TokenOfferingPolicy{DefaultAllow: true}
	if err := db.First(&policy, "token_id = ?", tokenID).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	ids := []int{}
	if err := db.Model(&TokenOfferingException{}).Where("token_id = ?", tokenID).Order("offering_id").Pluck("offering_id", &ids).Error; err != nil {
		return nil, err
	}
	return &KeyPermissions{DefaultAllow: policy.DefaultAllow, ExceptionIDs: ids, Revision: policy.Revision}, nil
}
