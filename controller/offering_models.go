package controller

import (
	"errors"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func ListOfferingModels(c *gin.Context, modelType int) {
	offerings := []model.ModelOffering{}
	if err := model.AllowedOfferingsQuery(c.Request.Context(), c.GetInt("id"), c.GetInt("token_id")).Order("name").Find(&offerings).Error; err != nil {
		gatewayError(c, http.StatusServiceUnavailable, "authorization_unavailable", "Model authorization is unavailable")
		return
	}
	switch modelType {
	case constant.ChannelTypeAnthropic:
		entries := make([]dto.AnthropicModel, 0, len(offerings))
		for _, offering := range offerings {
			entries = append(entries, dto.AnthropicModel{ID: offering.Name, DisplayName: offering.Name, Type: "model", CreatedAt: time.Unix(offering.CreatedAt, 0).UTC().Format(time.RFC3339)})
		}
		first, last := "", ""
		if len(entries) > 0 {
			first, last = entries[0].ID, entries[len(entries)-1].ID
		}
		c.JSON(http.StatusOK, gin.H{"data": entries, "first_id": first, "last_id": last, "has_more": false})
	case constant.ChannelTypeGemini:
		entries := make([]dto.GeminiModel, 0, len(offerings))
		for _, offering := range offerings {
			entries = append(entries, dto.GeminiModel{Name: offering.Name, DisplayName: offering.Name})
		}
		c.JSON(http.StatusOK, gin.H{"models": entries, "nextPageToken": nil})
	default:
		entries := make([]dto.OpenAIModels, 0, len(offerings))
		for _, offering := range offerings {
			entries = append(entries, dto.OpenAIModels{Id: offering.Name, Object: "model", Created: int(offering.CreatedAt), OwnedBy: "system"})
		}
		c.JSON(http.StatusOK, gin.H{"object": "list", "data": entries})
	}
}

func RetrieveOfferingModel(c *gin.Context, modelType int) {
	offering, err := model.FindModelOfferingByName(c.Request.Context(), c.Param("model"))
	if errors.Is(err, gorm.ErrRecordNotFound) {
		gatewayError(c, 404, "model_not_found", "Model not found")
		return
	}
	if err != nil {
		gatewayError(c, 503, "authorization_unavailable", "Model authorization is unavailable")
		return
	}
	allowed, err := service.GatewayOfferingAllowed(c, offering)
	if err != nil {
		gatewayError(c, 503, "authorization_unavailable", "Model authorization is unavailable")
		return
	}
	if !allowed {
		gatewayError(c, 404, "model_not_found", "Model not found")
		return
	}
	if modelType == constant.ChannelTypeAnthropic {
		c.JSON(200, dto.AnthropicModel{ID: offering.Name, DisplayName: offering.Name, Type: "model", CreatedAt: time.Unix(offering.CreatedAt, 0).UTC().Format(time.RFC3339)})
		return
	}
	c.JSON(200, dto.OpenAIModels{Id: offering.Name, Object: "model", Created: int(offering.CreatedAt), OwnedBy: "system"})
}
