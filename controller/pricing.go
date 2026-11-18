package controller

import (
	"net/http"

	"github.com/QuantumNous/new-api/pkg/capability"
	"github.com/QuantumNous/new-api/pkg/clock"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// SaleCatalogPrice deliberately excludes procurement, creator and margin data.
// Expression is the immutable source in Currency; it is never a float quote.
type SaleCatalogPrice struct {
	ID          int    `json:"id"`
	Currency    string `json:"currency"`
	Expression  string `json:"expression"`
	EffectiveAt int64  `json:"effective_at"`
}

type SaleCatalogMember struct {
	ModelID       int            `json:"model_id"`
	Name          string         `json:"name"`
	Tier          int            `json:"tier"`
	Description   string         `json:"description"`
	Icon          string         `json:"icon"`
	Tags          string         `json:"tags"`
	PublisherID   int            `json:"publisher_id"`
	PublisherName string         `json:"publisher_name"`
	Capabilities  capability.Set `json:"capabilities"`
}

type SaleCatalogOffering struct {
	ID      int                 `json:"id"`
	Name    string              `json:"name"`
	Aliases []string            `json:"aliases"`
	Members []SaleCatalogMember `json:"members"`
	Price   *SaleCatalogPrice   `json:"price"`
}

func GetPricing(c *gin.Context) {
	ctx := c.Request.Context()
	var offerings []model.ModelOffering
	if err := model.VisibleOfferingsQuery(ctx, c.GetInt("id")).
		Preload("Names", func(db *gorm.DB) *gorm.DB { return db.Order("name") }).
		Preload("Members", func(db *gorm.DB) *gorm.DB { return db.Order("tier, model_id") }).
		Preload("Members.Model").Order("name, id").Find(&offerings).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	now := clock.Now().Unix()
	ids := make([]int, 0, len(offerings))
	for _, offering := range offerings {
		ids = append(ids, offering.ID)
	}
	publisherIDs := []int{}
	for _, offering := range offerings {
		for _, member := range offering.Members {
			if member.Model.VendorID > 0 {
				publisherIDs = append(publisherIDs, member.Model.VendorID)
			}
		}
	}
	publishers := map[int]string{}
	if len(publisherIDs) > 0 {
		var records []model.Vendor
		if err := model.DB.WithContext(ctx).Select("id, name").Where("id IN ?", publisherIDs).Find(&records).Error; err != nil {
			common.ApiError(c, err)
			return
		}
		for _, publisher := range records {
			publishers[publisher.Id] = publisher.Name
		}
	}
	var versions []model.PriceVersion
	if len(ids) > 0 {
		// A single server instant and one batch query select exactly the same
		// effective version as CurrentPriceVersion, without an N+1 lookup.
		err := model.DB.WithContext(ctx).Select("DISTINCT ON (owner_id) id, owner_id, currency, expression, effective_at").
			Where("owner_type = ? AND owner_id IN ? AND effective_at <= ?", model.PriceOwnerOffering, ids, now).
			Order("owner_id, effective_at DESC").Find(&versions).Error
		if err != nil {
			common.ApiError(c, err)
			return
		}
	}
	prices := make(map[int]*SaleCatalogPrice, len(versions))
	for _, version := range versions {
		prices[version.OwnerID] = &SaleCatalogPrice{ID: version.ID, Currency: string(version.Currency), Expression: version.Expression, EffectiveAt: version.EffectiveAt}
	}
	items := make([]SaleCatalogOffering, 0, len(offerings))
	for _, offering := range offerings {
		item := SaleCatalogOffering{ID: offering.ID, Name: offering.Name, Aliases: []string{}, Members: []SaleCatalogMember{}, Price: prices[offering.ID]}
		for _, name := range offering.Names {
			if name.Name != offering.Name {
				item.Aliases = append(item.Aliases, name.Name)
			}
		}
		for _, member := range offering.Members {
			metadata := member.Model
			item.Members = append(item.Members, SaleCatalogMember{ModelID: member.ModelID, Name: metadata.ModelName, Tier: member.Tier, Description: metadata.Description, Icon: metadata.Icon, Tags: metadata.Tags, PublisherID: metadata.VendorID, PublisherName: publishers[metadata.VendorID], Capabilities: metadata.Capabilities})
		}
		items = append(items, item)
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": items, "as_of": now})
}

// Retain the offline legacy migration code, but close all live ratio editors
// and float synchronization endpoints after the Offering cutover.
func RetiredModelPricing(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusGone, gin.H{"success": false, "message": model.RetiredModelPricingMessage})
}
