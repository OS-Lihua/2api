package controller

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func ListRecommendationRules(c *gin.Context) {
	page := common.GetPageQuery(c)
	query := model.DB.Model(&model.RecommendationRule{})
	if kind := c.Query("kind"); kind != "" {
		query = query.Where("kind = ?", kind)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		common.ApiErrorStatus(c, 500, err)
		return
	}
	items := make([]model.RecommendationRule, 0)
	if err := query.Order("priority DESC, length(pattern) DESC, id ASC").Offset(page.GetStartIdx()).Limit(page.GetPageSize()).Find(&items).Error; err != nil {
		common.ApiErrorStatus(c, 500, err)
		return
	}
	common.ApiSuccess(c, gin.H{"items": items, "total": total})
}

func SaveRecommendationRule(c *gin.Context) {
	var rule model.RecommendationRule
	if err := common.DecodeJson(c.Request.Body, &rule); err != nil {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid recommendation rule"))
		return
	}
	rule.ID = 0
	if c.Request.Method == http.MethodPut {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil || id <= 0 {
			common.ApiErrorStatus(c, 404, common.NewMessage("Recommendation rule not found"))
			return
		}
		var saved model.RecommendationRule
		if err := model.DB.First(&saved, id).Error; err != nil {
			recommendationLookupError(c, err)
			return
		}
		rule.ID = id
	}
	if err := rule.Normalize(); err != nil {
		common.ApiErrorStatus(c, 400, err)
		return
	}
	var err error
	if rule.ID == 0 {
		err = model.DB.Create(&rule).Error
	} else {
		err = model.DB.Model(&rule).Select("kind", "pattern", "priority", "enabled", "provider_name", "provider_type", "publisher_id", "publisher_name").Updates(&rule).Error
	}
	if err != nil {
		common.ApiErrorStatus(c, 500, err)
		return
	}
	recordManageAudit(c, "recommendation_rule.save", map[string]any{"id": rule.ID, "kind": rule.Kind})
	common.ApiSuccess(c, rule)
}

func DeleteRecommendationRule(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		common.ApiErrorStatus(c, 404, common.NewMessage("Recommendation rule not found"))
		return
	}
	result := model.DB.Delete(&model.RecommendationRule{}, id)
	if result.Error != nil {
		recommendationLookupError(c, result.Error)
		return
	}
	if result.RowsAffected == 0 {
		recommendationLookupError(c, gorm.ErrRecordNotFound)
		return
	}
	recordManageAudit(c, "recommendation_rule.delete", map[string]any{"id": id})
	common.ApiSuccess(c, gin.H{"id": id})
}

func recommendationLookupError(c *gin.Context, err error) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		common.ApiErrorStatus(c, 404, common.NewMessage("Recommendation rule not found"))
		return
	}
	common.ApiErrorStatus(c, 500, err)
}

func QueryRecommendations(c *gin.Context) {
	kind := c.Param("kind")
	if kind != "provider" && kind != "publisher" {
		common.ApiErrorStatus(c, 400, common.NewMessage("Invalid recommendation rule"))
		return
	}
	providerType, _ := strconv.Atoi(c.Query("type"))
	items, err := model.FindRecommendations(kind, c.Query("url"), c.Query("model_name"), providerType)
	if err != nil {
		common.ApiErrorStatus(c, 500, err)
		return
	}
	common.ApiSuccess(c, gin.H{"items": items})
}
