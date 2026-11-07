package model

import (
	"context"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"gorm.io/gorm"
)

const (
	PriceOwnerOffering        = "model_offering"
	PriceOwnerChannel         = "channel"
	MaxPriceEffectiveAt int64 = 253402300799 // Last Unix second in year 9999.
)

// PriceVersion is an immutable expression in its declared currency. Token
// coefficients are per million tokens; task expressions return currency units.
// Nullable foreign keys retain owners and prevent deletion of price history.
type PriceVersion struct {
	ID          int            `json:"id" gorm:"primaryKey"`
	OwnerType   string         `json:"owner_type" gorm:"type:varchar(20);not null;uniqueIndex:idx_price_owner_effective"`
	OwnerID     int            `json:"owner_id" gorm:"not null;uniqueIndex:idx_price_owner_effective"`
	OfferingID  *int           `json:"-" gorm:"index"`
	Offering    *ModelOffering `json:"-" gorm:"foreignKey:OfferingID;constraint:OnDelete:RESTRICT"`
	ChannelID   *int           `json:"-" gorm:"index"`
	Channel     *Channel       `json:"-" gorm:"foreignKey:ChannelID;constraint:OnDelete:RESTRICT"`
	Currency    money.Currency `json:"currency" gorm:"type:char(3);not null;check:price_currency_valid,currency ~ '^[A-Z]{3}$'"`
	Expression  string         `json:"expression" gorm:"type:text;not null;check:price_expression_bounded,octet_length(expression) BETWEEN 1 AND 16384"`
	EffectiveAt int64          `json:"effective_at" gorm:"not null;uniqueIndex:idx_price_owner_effective;check:price_effective_valid,effective_at BETWEEN 1 AND 253402300799"`
	CreatedBy   int            `json:"created_by" gorm:"not null"`
	Creator     *User          `json:"-" gorm:"foreignKey:CreatedBy;constraint:OnDelete:RESTRICT"`
	CreatedAt   int64          `json:"created_at" gorm:"not null;autoCreateTime:false"`
}

// CurrentPriceVersion uses the latest effective time, with equal-time writes
// rejected by the unique owner/effective_at index. It never infers a free price.
func CurrentPriceVersion(ctx context.Context, db *gorm.DB, ownerType string, ownerID int, at int64) (*PriceVersion, error) {
	var record PriceVersion
	err := db.WithContext(ctx).Where("owner_type = ? AND owner_id = ? AND effective_at <= ?", ownerType, ownerID, at).Order("effective_at DESC").First(&record).Error
	return &record, err
}

type PriceVersionChecker struct{}

func (PriceVersionChecker) HasPurchasePrice(ctx context.Context, db *gorm.DB, channel *Channel) (bool, error) {
	price, err := CurrentPriceVersion(ctx, db, PriceOwnerChannel, channel.ID, clock.Now().Unix())
	if err == gorm.ErrRecordNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	err = validatePriceExpression(ctx, db, price.Expression, nil, channel)
	return err == nil, err
}

func (PriceVersionChecker) HasSalePrice(ctx context.Context, db *gorm.DB, offering *ModelOffering) (bool, error) {
	price, err := CurrentPriceVersion(ctx, db, PriceOwnerOffering, offering.ID, clock.Now().Unix())
	if err == gorm.ErrRecordNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	err = validatePriceExpression(ctx, db, price.Expression, offering, nil)
	return err == nil, err
}

func init() { InstallPriceVersionCheckers() }

// InstallPriceVersionCheckers wires the persisted current prices into activation.
func InstallPriceVersionCheckers() {
	SetChannelPurchasePriceChecker(PriceVersionChecker{})
	SetOfferingSalePriceChecker(PriceVersionChecker{})
}

func CreatePriceVersion(ctx context.Context, record *PriceVersion, actorID int) error {
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := preparePriceVersion(ctx, tx, record, actorID, nil); err != nil {
			return err
		}
		return tx.Omit("Creator", "Channel", "Offering").Create(record).Error
	})
}

// preparePriceVersion validates without allocating an ID. A staged Channel
// allows batch validation to use the same rules before any row is inserted.
func preparePriceVersion(ctx context.Context, tx *gorm.DB, record *PriceVersion, actorID int, channel *Channel) error {
	if (record.OwnerType != PriceOwnerOffering && record.OwnerType != PriceOwnerChannel) || (record.OwnerID <= 0 && (channel == nil || channel.ID != record.OwnerID)) {
		return common.NewMessage("Invalid price owner")
	}
	if _, err := money.ParseCurrency(string(record.Currency)); err != nil {
		return common.NewMessage("Invalid price currency")
	}
	if record.EffectiveAt <= 0 || record.EffectiveAt > MaxPriceEffectiveAt {
		return common.NewMessage("Invalid price effective time")
	}
	if strings.TrimSpace(record.Expression) == "" || len(record.Expression) > 16384 {
		return common.NewMessage("Invalid price expression")
	}
	switch record.OwnerType {
	case PriceOwnerOffering:
		var owner ModelOffering
		if err := lockForUpdate(tx).First(&owner, record.OwnerID).Error; err != nil {
			return err
		}
		if err := tx.Where("offering_id = ?", owner.ID).Find(&owner.Members).Error; err != nil {
			return err
		}
		if err := validatePriceExpression(ctx, tx, record.Expression, &owner, nil); err != nil {
			return err
		}
		record.OfferingID, record.ChannelID = &record.OwnerID, nil
	case PriceOwnerChannel:
		if channel == nil {
			channel = &Channel{}
			if err := lockForUpdate(tx).First(channel, record.OwnerID).Error; err != nil {
				return err
			}
		}
		if err := validatePriceExpression(ctx, tx, record.Expression, nil, channel); err != nil {
			return err
		}
		record.ChannelID, record.OfferingID = &record.OwnerID, nil
	}
	record.ID, record.CreatedBy, record.CreatedAt = 0, actorID, clock.Now().Unix()
	record.Creator, record.Channel, record.Offering = nil, nil, nil
	return nil
}

// validatePriceExpression checks the proposed owner declaration, which may not
// have been persisted yet. All reads stay on its mutation transaction.
func validatePriceExpression(ctx context.Context, db *gorm.DB, expression string, offering *ModelOffering, channel *Channel) error {
	db = db.WithContext(ctx)
	var channels []Channel
	var ids []int
	if channel != nil {
		channels = []Channel{*channel}
		ids = []int{channel.ModelID}
	} else {
		for _, member := range offering.Members {
			ids = append(ids, member.ModelID)
		}
		query := db.Where("model_id IN ?", ids)
		scope := offering.ChannelScope
		// Provider and Channel inclusions form a union. Exclusions always win.
		// Keep missing include IDs restrictive rather than inferring all Channels.
		if len(scope.IncludeProviderIDs)+len(scope.IncludeChannelIDs) > 0 {
			query = query.Where("(provider_id IN ? OR id IN ?)", scope.IncludeProviderIDs, scope.IncludeChannelIDs)
		}
		if len(scope.ExcludeProviderIDs) > 0 {
			query = query.Where("provider_id NOT IN ?", scope.ExcludeProviderIDs)
		}
		if len(scope.ExcludeChannelIDs) > 0 {
			query = query.Where("id NOT IN ?", scope.ExcludeChannelIDs)
		}
		if err := query.Order("id").Find(&channels).Error; err != nil {
			return err
		}
	}
	var members []Model
	if err := db.Where("id IN ?", ids).Order("id").Find(&members).Error; err != nil {
		return err
	}
	if len(members) != len(ids) {
		return gorm.ErrRecordNotFound
	}
	models := make(map[int]Model, len(members))
	for _, member := range members {
		models[member.Id] = member
	}
	generation := jsplugin.DefaultRegistry.Generation()
	validatedSchemas := map[string]bool{}
	served := map[int]bool{}
	ordinary := false
	for _, candidate := range channels {
		served[candidate.ModelID] = true
		key, task := candidate.Protocol.TaskPluginKey()
		if !task {
			ordinary = true
			continue
		}
		plugin, ok := generation.Get(key)
		if !ok {
			return common.NewMessage("Task price requires an available plugin usage schema")
		}
		clientModel := models[candidate.ModelID].ModelName
		if canonical, ok := generation.CanonicalModel(clientModel); ok {
			clientModel = canonical
		}
		upstreamModel := candidate.UpstreamModelName
		if canonical, ok := generation.CanonicalModel(upstreamModel); ok {
			upstreamModel = canonical
		}
		if !slices.Contains(plugin.Meta.Models, clientModel) && !slices.Contains(plugin.Meta.Models, upstreamModel) {
			return common.NewMessage("Task price requires an available plugin usage schema")
		}
		schema, _ := plugin.Meta.UsageForModels(upstreamModel, clientModel)
		if err := validateTaskPriceSchema(expression, schema, validatedSchemas); err != nil {
			return err
		}
	}
	if offering != nil {
		scope := offering.ChannelScope
		unrestricted := len(scope.IncludeProviderIDs)+len(scope.IncludeChannelIDs)+len(scope.ExcludeProviderIDs)+len(scope.ExcludeChannelIDs) == 0
		// Before Channels exist, an unrestricted member can still declare task
		// pricing. A scoped-out member must not widen the selected schemas.
		if unrestricted {
			for _, member := range members {
				if served[member.Id] {
					continue
				}
				name := member.ModelName
				if canonical, ok := generation.CanonicalModel(name); ok {
					name = canonical
				}
				plugins := generation.PluginsByModel(name)
				if len(plugins) == 0 {
					ordinary = true
				}
				for _, plugin := range plugins {
					schema, _ := plugin.Meta.UsageForModels(name)
					if err := validateTaskPriceSchema(expression, schema, validatedSchemas); err != nil {
						return err
					}
				}
			}
		}
	}
	if ordinary || len(validatedSchemas) == 0 {
		if err := billing_setting.SmokeTestExpr(expression); err != nil {
			return common.NewMessage("Invalid price expression")
		}
	}
	return nil
}

// Identical selected schemas share one smoke validation, even when many
// Channels or member Models use the same plugin profile.
func validateTaskPriceSchema(expression string, schema map[string]jsplugin.UsageFieldSchema, validated map[string]bool) error {
	encoded, err := common.Marshal(schema)
	if err != nil {
		return common.NewMessage("Invalid price expression")
	}
	key := string(encoded)
	if validated[key] {
		return nil
	}
	if err := billing_setting.SmokeTestTaskExpr(expression, schema); err != nil {
		return common.NewMessage("Invalid price expression")
	}
	validated[key] = true
	return nil
}

// MigratePricing creates primary-database pricing tables and protects immutable
// history even from SQL writes outside the management API. It is idempotent.
func MigratePricing(db *gorm.DB) error {
	if err := db.AutoMigrate(&PriceVersion{}, &FXRate{}); err != nil {
		return err
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		for _, statement := range []string{
			`DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'price_owner_valid' AND conrelid = '"price_versions"'::regclass) THEN
     ALTER TABLE "price_versions" ADD CONSTRAINT "price_owner_valid" CHECK (
      ("owner_type" = 'model_offering' AND "owner_id" > 0 AND "offering_id" IS NOT NULL AND "offering_id" = "owner_id" AND "channel_id" IS NULL) OR
      ("owner_type" = 'channel' AND "owner_id" > 0 AND "channel_id" IS NOT NULL AND "channel_id" = "owner_id" AND "offering_id" IS NULL));
    END IF;
   END $$`,
			`CREATE OR REPLACE FUNCTION "price_versions_append_only"() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'price_versions is append-only; create a new version instead'; END $$ LANGUAGE plpgsql`,
			`DROP TRIGGER IF EXISTS "price_versions_append_only" ON "price_versions"`,
			`CREATE TRIGGER "price_versions_append_only" BEFORE UPDATE OR DELETE ON "price_versions" FOR EACH ROW EXECUTE FUNCTION "price_versions_append_only"()`,
			`DROP TRIGGER IF EXISTS "price_versions_no_truncate" ON "price_versions"`,
			`CREATE TRIGGER "price_versions_no_truncate" BEFORE TRUNCATE ON "price_versions" FOR EACH STATEMENT EXECUTE FUNCTION "price_versions_append_only"()`,
		} {
			if err := tx.Exec(statement).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		InstallPriceVersionCheckers()
	}
	return err
}
