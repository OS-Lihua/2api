package controller

import (
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/gin-gonic/gin"
)

func retiredQuotaBilling(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	common.ApiErrorStatus(c, http.StatusGone,
		common.NewMessage("Quota-based billing queries have been retired. Use the authorized wallet APIs."),
		gin.H{"code": "quota_billing_retired", "message": i18n.T(c, i18n.MsgQuotaBillingRetired)})
}

func GetSubscription(c *gin.Context) {
	retiredQuotaBilling(c)
}

func GetUsage(c *gin.Context) {
	retiredQuotaBilling(c)
}
