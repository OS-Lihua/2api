package model

import (
	"context"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

var ErrModelOfferingMember = common.NewMessage("The Model is still a member of a ModelOffering")

// ModelOffering is the model identity sold to users, independent of its
// member Models and the Channels ultimately selected to serve a request.
type ModelOffering struct {
	ID                       int                   `json:"id" gorm:"primaryKey"`
	Name                     string                `json:"name" gorm:"type:varchar(128);not null"`
	Aliases                  []string              `json:"aliases" gorm:"-"`
	Names                    []ModelOfferingName   `json:"-" gorm:"foreignKey:OfferingID;constraint:OnDelete:CASCADE"`
	Visibility               string                `json:"visibility" gorm:"type:varchar(16);not null;check:offering_visibility_valid,visibility IN ('all','groups')"`
	Groups                   []string              `json:"groups" gorm:"serializer:json;type:jsonb;not null"`
	Members                  []ModelOfferingMember `json:"members" gorm:"foreignKey:OfferingID;constraint:OnDelete:CASCADE"`
	ChannelScope             OfferingChannelScope  `json:"channel_scope" gorm:"serializer:json;type:jsonb;not null"`
	RouteMethod              string                `json:"route_method" gorm:"type:varchar(16);not null;check:offering_route_valid,route_method IN ('cost','weight','latency')"`
	AcceptedConversionLosses []string              `json:"accepted_conversion_losses" gorm:"serializer:json;type:jsonb;not null"`
	MinimumMarginRate        decimal.Decimal       `json:"minimum_margin_rate" gorm:"type:numeric;not null;check:offering_margin_valid,minimum_margin_rate >= 0 AND minimum_margin_rate <= 1"`
	AllowLossFallback        bool                  `json:"allow_loss_fallback" gorm:"not null"`
	Enabled                  bool                  `json:"enabled" gorm:"not null"`
	CreatedAt                int64                 `json:"created_at" gorm:"not null"`
	UpdatedAt                int64                 `json:"updated_at" gorm:"not null"`
}

// All callable names share a single unique key, including primary names.
// A name cannot be an alias of one offering and the primary name of another.
type ModelOfferingName struct {
	Name       string `gorm:"type:varchar(128);primaryKey"`
	OfferingID int    `gorm:"not null;index"`
}

type ModelOfferingMember struct {
	OfferingID int   `json:"-" gorm:"primaryKey;autoIncrement:false"`
	ModelID    int   `json:"model_id" gorm:"primaryKey;autoIncrement:false"`
	Model      Model `json:"-" gorm:"constraint:OnDelete:RESTRICT"`
	Tier       int   `json:"tier" gorm:"not null;check:offering_member_tier_positive,tier >= 1"`
}

// Scope IDs survive target deletion. A deleted include target must continue
// restricting the candidate set; it must never become an unrestricted scope.
// Saving requires live references, while disabling is always allowed.
type OfferingChannelScope struct {
	IncludeProviderIDs []int `json:"include_provider_ids"`
	ExcludeProviderIDs []int `json:"exclude_provider_ids"`
	IncludeChannelIDs  []int `json:"include_channel_ids"`
	ExcludeChannelIDs  []int `json:"exclude_channel_ids"`
}

// OfferingSalePriceChecker validates the proposed declaration before it is
// persisted, in the mutation transaction. An absent checker denies activation;
// an explicit zero price is still configured.
type OfferingSalePriceChecker interface {
	HasSalePrice(context.Context, *gorm.DB, *ModelOffering) (bool, error)
}

var offeringPrices struct {
	sync.RWMutex
	checker OfferingSalePriceChecker
}

func SetOfferingSalePriceChecker(checker OfferingSalePriceChecker) {
	offeringPrices.Lock()
	defer offeringPrices.Unlock()
	offeringPrices.checker = checker
}

func GetModelOffering(ctx context.Context, id int) (*ModelOffering, error) {
	var record ModelOffering
	err := DB.WithContext(ctx).Preload("Names", func(tx *gorm.DB) *gorm.DB { return tx.Order("name") }).Preload("Members", func(tx *gorm.DB) *gorm.DB { return tx.Order("tier, model_id") }).First(&record, id).Error
	if err != nil {
		return nil, err
	}
	record.Aliases = []string{}
	for _, name := range record.Names {
		if name.Name != record.Name {
			record.Aliases = append(record.Aliases, name.Name)
		}
	}
	return &record, nil
}

// SaveModelOffering replaces a declaration atomically, including its names and
// members. Renaming reserves the old primary name as an alias in the same
// transaction. The activation actions always use the locked current declaration.
func SaveModelOffering(ctx context.Context, record *ModelOffering, action string) error {
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var previous ModelOffering
		if record.ID != 0 {
			if err := lockForUpdate(tx).First(&previous, record.ID).Error; err != nil {
				return err
			}
			if action == "disable" {
				*record = previous
				record.Enabled = false
				record.UpdatedAt = clock.Now().Unix()
				return tx.Model(&ModelOffering{}).Where("id = ?", record.ID).Updates(map[string]any{"enabled": false, "updated_at": record.UpdatedAt}).Error
			}
			if action != "save" {
				*record = previous
				if err := tx.Where("offering_id = ?", record.ID).Order("tier, model_id").Find(&record.Members).Error; err != nil {
					return err
				}
				var names []ModelOfferingName
				if err := tx.Where("offering_id = ?", record.ID).Find(&names).Error; err != nil {
					return err
				}
				for _, name := range names {
					if name.Name != record.Name {
						record.Aliases = append(record.Aliases, name.Name)
					}
				}
			}
			record.CreatedAt = previous.CreatedAt
			record.Enabled = previous.Enabled
		} else {
			record.CreatedAt = clock.Now().Unix()
			record.Enabled = false
		}
		if previous.Name != "" && previous.Name != strings.TrimSpace(record.Name) {
			record.Aliases = append(record.Aliases, previous.Name)
		}
		if err := record.normalize(); err != nil {
			return err
		}
		ids := make([]int, 0, len(record.Members))
		for _, member := range record.Members {
			ids = append(ids, member.ModelID)
		}
		slices.Sort(ids)
		var members []Model
		if err := lockForUpdate(tx).Where("id IN ? AND name_rule = ?", ids, NameRuleExact).Order("id").Find(&members).Error; err != nil {
			return err
		}
		if len(members) != len(ids) {
			return common.NewMessage("Every member must reference an existing Model")
		}
		for _, scope := range []struct {
			ids    []int
			entity any
		}{{record.ChannelScope.IncludeProviderIDs, &Provider{}}, {record.ChannelScope.ExcludeProviderIDs, &Provider{}}, {record.ChannelScope.IncludeChannelIDs, &Channel{}}, {record.ChannelScope.ExcludeChannelIDs, &Channel{}}} {
			if len(scope.ids) == 0 {
				continue
			}
			var count int64
			if err := tx.Model(scope.entity).Where("id IN ?", scope.ids).Count(&count).Error; err != nil {
				return err
			}
			if count != int64(len(scope.ids)) {
				return common.NewMessage("Channel scope references a missing Provider or Channel")
			}
		}
		switch action {
		case "enable":
			record.Enabled = true
		case "save":
		default:
			return common.NewMessage("Invalid ModelOffering action")
		}
		if record.Enabled {
			offeringPrices.RLock()
			checker := offeringPrices.checker
			offeringPrices.RUnlock()
			configured := false
			if checker != nil {
				var err error
				configured, err = checker.HasSalePrice(ctx, tx, record)
				if err != nil {
					return err
				}
			}
			if !configured {
				return common.NewMessage("Configure a sale price before enabling the ModelOffering")
			}
		}
		record.UpdatedAt = clock.Now().Unix()
		if action == "enable" {
			return tx.Model(&ModelOffering{}).Where("id = ?", record.ID).Updates(map[string]any{"enabled": true, "updated_at": record.UpdatedAt}).Error
		}
		if err := tx.Omit("Names", "Members").Save(record).Error; err != nil {
			return err
		}
		if err := tx.Where("offering_id = ?", record.ID).Delete(&ModelOfferingName{}).Error; err != nil {
			return err
		}
		names := []ModelOfferingName{{Name: record.Name, OfferingID: record.ID}}
		for _, alias := range record.Aliases {
			names = append(names, ModelOfferingName{Name: alias, OfferingID: record.ID})
		}
		if err := tx.Create(&names).Error; err != nil {
			return err
		}
		if err := tx.Where("offering_id = ?", record.ID).Delete(&ModelOfferingMember{}).Error; err != nil {
			return err
		}
		for i := range record.Members {
			record.Members[i].OfferingID = record.ID
		}
		return tx.Omit("Model").Create(&record.Members).Error
	})
}

func (record *ModelOffering) normalize() error {
	if len(record.Aliases) > 1000 {
		return common.NewMessage("A ModelOffering can have at most 1000 aliases")
	}
	record.Name = strings.TrimSpace(record.Name)
	names := append([]string{record.Name}, record.Aliases...)
	for _, name := range names {
		if name != strings.TrimSpace(name) || name == "" || utf8.RuneCountInString(name) > 128 || strings.ContainsFunc(name, unicode.IsSpace) || strings.ContainsFunc(name, unicode.IsControl) {
			return common.NewMessage("ModelOffering names must contain 1 to 128 characters without whitespace")
		}
	}
	record.Aliases = []string{}
	seenNames := map[string]bool{record.Name: true}
	for _, name := range names[1:] {
		if !seenNames[name] {
			record.Aliases = append(record.Aliases, name)
			seenNames[name] = true
		}
	}
	slices.Sort(record.Aliases)
	if len(record.Members) == 0 || len(record.Members) > 1000 {
		return common.NewMessage("A ModelOffering must have between 1 and 1000 member Models")
	}
	seen := map[int]bool{}
	hasFirstTier := false
	for _, member := range record.Members {
		if member.ModelID <= 0 || member.Tier < 1 || seen[member.ModelID] {
			return common.NewMessage("Members must have distinct Models and positive tiers starting at 1")
		}
		seen[member.ModelID] = true
		hasFirstTier = hasFirstTier || member.Tier == 1
	}
	if !hasFirstTier {
		return common.NewMessage("Members must have distinct Models and positive tiers starting at 1")
	}
	if record.Visibility == "" {
		record.Visibility = "all"
	}
	if record.Groups == nil {
		record.Groups = []string{}
	}
	if record.Visibility != "all" && record.Visibility != "groups" || record.Visibility == "groups" && len(record.Groups) == 0 {
		return common.NewMessage("Select all users or at least one user group")
	}
	for _, group := range record.Groups {
		if strings.TrimSpace(group) == "" || len(group) > 128 {
			return common.NewMessage("Invalid user group")
		}
	}
	if record.RouteMethod == "" {
		record.RouteMethod = "cost"
	}
	if !slices.Contains([]string{"cost", "weight", "latency"}, record.RouteMethod) {
		return common.NewMessage("Invalid route method")
	}
	if record.MinimumMarginRate.IsNegative() || record.MinimumMarginRate.GreaterThan(decimal.NewFromInt(1)) {
		return common.NewMessage("Minimum margin rate must be between 0 and 1")
	}
	if record.AcceptedConversionLosses == nil {
		record.AcceptedConversionLosses = []string{}
	}
	for _, code := range record.AcceptedConversionLosses {
		if code == "" || strings.TrimSpace(code) != code || len(code) > 128 {
			return common.NewMessage("Invalid conversion loss code")
		}
	}
	for _, ids := range []*[]int{&record.ChannelScope.IncludeProviderIDs, &record.ChannelScope.ExcludeProviderIDs, &record.ChannelScope.IncludeChannelIDs, &record.ChannelScope.ExcludeChannelIDs} {
		if *ids == nil {
			*ids = []int{}
		}
		slices.Sort(*ids)
		*ids = slices.Compact(*ids)
		if len(*ids) > 1000 || len(*ids) > 0 && (*ids)[0] <= 0 {
			return common.NewMessage("Channel scope must contain positive Provider or Channel IDs")
		}
	}
	for _, pair := range []struct{ included, excluded []int }{{record.ChannelScope.IncludeProviderIDs, record.ChannelScope.ExcludeProviderIDs}, {record.ChannelScope.IncludeChannelIDs, record.ChannelScope.ExcludeChannelIDs}} {
		for _, id := range pair.included {
			if slices.Contains(pair.excluded, id) {
				return common.NewMessage("A Provider or Channel cannot be both included and excluded")
			}
		}
	}
	return nil
}
