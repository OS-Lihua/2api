package model

import (
	"context"
	"errors"
	"slices"

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

var ErrKeyPermissionsConflict = errors.New("key permissions changed")

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
			if err := tx.Model(&ModelOffering{}).Where("id IN ?", current.ExceptionIDs).Count(&count).Error; err != nil {
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

// AllowedOfferingsQuery is the single authorization predicate for discovery,
// detail and execution. All decision inputs come from one primary DB snapshot;
// cached credential or group metadata cannot keep a revoked grant alive.
func AllowedOfferingsQuery(ctx context.Context, userID, tokenID int) *gorm.DB {
	db := DB.WithContext(ctx)
	users := db.Model(&User{}).Select("id").Where("id = ? AND status = ?", userID, common.UserStatusEnabled)
	tokens := db.Model(&Token{}).Select("id").Where("id = ? AND user_id = ? AND status = ? AND (expired_time = -1 OR expired_time >= ?)", tokenID, userID, common.TokenStatusEnabled, common.GetTimestamp())
	return db.Model(&ModelOffering{}).
		Where("enabled = ?", true).
		Where("EXISTS (?)", users).
		Where("EXISTS (?)", tokens).
		Where(`("visibility" = 'all' OR "groups" @> jsonb_build_array((SELECT "group" FROM "users" WHERE "id" = ?)))`, userID).
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
