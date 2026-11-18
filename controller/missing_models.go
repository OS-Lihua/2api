package controller

import (
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
)

// GetMissingModels retains the old authenticated route without consulting legacy routing data.
func GetMissingModels(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	common.ApiErrorStatus(c, http.StatusGone,
		common.NewMessage("Legacy missing-model discovery has been retired. Use Models and metadata sync."),
		gin.H{"code": "legacy_missing_models_retired"})
}
