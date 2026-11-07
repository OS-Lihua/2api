package controller

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func offeringModelsError(c *gin.Context, modelType, status int, code, message string) {
	if code == "model_not_found" {
		message = i18n.T(c, i18n.MsgGatewayModelNotFound)
	} else if code == "authorization_unavailable" {
		message = i18n.T(c, i18n.MsgGatewayAuthorizationUnavailable)
	}
	switch modelType {
	case constant.ChannelTypeAnthropic:
		kind := "api_error"
		if status == http.StatusNotFound {
			kind = "not_found_error"
		}
		c.AbortWithStatusJSON(status, gin.H{"type": "error", "error": gin.H{"type": kind, "code": code, "message": message}})
	case constant.ChannelTypeGemini:
		kind := "UNAVAILABLE"
		if status == http.StatusNotFound {
			kind = "NOT_FOUND"
		}
		c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"code": status, "message": message, "status": kind, "details": []gin.H{{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": code, "domain": "gateway"}}}})
	default:
		gatewayError(c, status, code, message)
	}
}

func ListOfferingModels(c *gin.Context, modelType int) {
	offerings := []model.ModelOffering{}
	if err := model.AllowedOfferingsQuery(c.Request.Context(), c.GetInt("id"), c.GetInt("token_id")).Order("name").Find(&offerings).Error; err != nil {
		offeringModelsError(c, modelType, http.StatusServiceUnavailable, "authorization_unavailable", "Model authorization is unavailable")
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
	offering, err := model.FindModelOfferingByName(c.Request.Context(), strings.TrimPrefix(c.Param("model"), "/"))
	if errors.Is(err, gorm.ErrRecordNotFound) {
		offeringModelsError(c, modelType, 404, "model_not_found", "Model not found")
		return
	}
	if err != nil {
		offeringModelsError(c, modelType, 503, "authorization_unavailable", "Model authorization is unavailable")
		return
	}
	allowed, err := service.GatewayOfferingAllowed(c, offering)
	if err != nil {
		offeringModelsError(c, modelType, 503, "authorization_unavailable", "Model authorization is unavailable")
		return
	}
	if !allowed {
		offeringModelsError(c, modelType, 404, "model_not_found", "Model not found")
		return
	}
	if modelType == constant.ChannelTypeGemini {
		c.JSON(200, struct {
			dto.GeminiModel
			Members []model.ModelOfferingMember `json:"members"`
		}{dto.GeminiModel{Name: offering.Name, DisplayName: offering.Name}, offering.Members})
		return
	}
	if modelType == constant.ChannelTypeAnthropic {
		c.JSON(200, struct {
			dto.AnthropicModel
			Members []model.ModelOfferingMember `json:"members"`
		}{dto.AnthropicModel{ID: offering.Name, DisplayName: offering.Name, Type: "model", CreatedAt: time.Unix(offering.CreatedAt, 0).UTC().Format(time.RFC3339)}, offering.Members})
		return
	}
	c.JSON(200, struct {
		dto.OpenAIModels
		Members []model.ModelOfferingMember `json:"members"`
	}{dto.OpenAIModels{Id: offering.Name, Object: "model", Created: int(offering.CreatedAt), OwnedBy: "system"}, offering.Members})
}
