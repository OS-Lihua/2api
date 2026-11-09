package service

import (
	"net/http"

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

// RecordGatewayModelAccessDenied records the same safe event for hidden and
// unknown models. Protocol formatters may share it without duplicating events.
func RecordGatewayModelAccessDenied(c *gin.Context) {
	const recordedKey = "gateway_model_access_denial_recorded"
	if c.GetBool(recordedKey) {
		return
	}
	c.Set(recordedKey, true)
	role := 0
	if user, err := model.GetUserCache(c.GetInt("id")); err == nil {
		role = user.Role
	}
	model.RecordAuditLog(c, model.AuditLog{
		UserId: c.GetInt("id"), ActorRole: role,
		Category: model.AuditCategorySecurity, Action: "gateway.model_access.denied",
		AuthMethod: "api_key", Status: http.StatusNotFound, Success: false,
		Content: "Model not found",
		Other: model.AuditOther{Op: &model.AuditOperation{
			Action: "gateway.model_access.denied",
			Params: model.AuditFields{"token_id": c.GetInt("token_id")},
		}},
	})
}
