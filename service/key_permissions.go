package service

import (
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

// GatewayOfferingAllowed checks the authenticated user's Key against the live
// Offering identity. Resolve names first; aliases never change authorization.
func GatewayOfferingAllowed(c *gin.Context, offering *model.ModelOffering) (bool, error) {
	if offering == nil || c.GetInt("id") <= 0 || c.GetInt("token_id") <= 0 {
		return false, nil
	}
	var count int64
	err := model.AllowedOfferingsQuery(c.Request.Context(), c.GetInt("id"), c.GetInt("token_id")).Where("model_offerings.id = ?", offering.ID).Count(&count).Error
	return count == 1, err
}
