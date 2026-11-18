package controller

import (
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

func Playground(c *gin.Context) {
	c.Abort()
	if c.GetBool("use_access_token") {
		gatewayError(c, http.StatusForbidden, "access_denied", i18n.T(c, i18n.MsgRelayAccessTokenUnsupported))
		return
	}
	user, err := model.GetUserCache(c.GetInt("id"))
	if err != nil {
		gatewayError(c, http.StatusServiceUnavailable, "authorization_unavailable", i18n.T(c, i18n.MsgGatewayAuthorizationUnavailable))
		return
	}
	user.WriteContext(c)
	common.SetContextKey(c, constant.ContextKeyUsingGroup, user.Group)
	c.Set("token_name", "playground")
	offering := middleware.TextOffering(c)
	if offering == nil {
		if !middleware.TextOfferingMissing(c) {
			gatewayError(c, http.StatusBadRequest, "invalid_request", "Invalid text request")
		} else {
			gatewayError(c, http.StatusNotFound, "model_not_found", "Model not found")
		}
		return
	}
	var count int64
	if err := model.VisibleOfferingsQuery(c.Request.Context(), user.Id).Where("model_offerings.id = ?", offering.ID).Count(&count).Error; err != nil {
		gatewayError(c, http.StatusServiceUnavailable, "authorization_unavailable", i18n.T(c, i18n.MsgGatewayAuthorizationUnavailable))
		return
	}
	if count != 1 {
		gatewayError(c, http.StatusNotFound, "model_not_found", "Model not found")
		return
	}
	runAuthorizedTextGateway(c)
}
