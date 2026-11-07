package middleware

import (
	"errors"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// ResolveMediaOffering runs before TokenAuth only on synchronous media create
// endpoints. Resolution does not grant permission; the gateway checks the same
// Offering identity policy as text after credential validation.
func ResolveMediaOffering() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodPost {
			c.Next()
			return
		}
		switch c.Request.URL.Path {
		case "/v1/images/generations", "/v1/images/edits", "/v1/audio/speech", "/v1/audio/transcriptions":
		default:
			c.Next()
			return
		}
		c.Set(TextGatewayStartedAtKey, clock.Now())
		var request struct {
			Model string `json:"model"`
		}
		if common.UnmarshalBodyReusable(c, &request) != nil || request.Model == "" {
			abortWithOpenAiMessage(c, http.StatusBadRequest, i18n.T(c, i18n.MsgDistributorInvalidParseModel))
			return
		}
		offering, err := model.FindModelOfferingByName(c.Request.Context(), request.Model)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.Next()
			return
		}
		if err != nil {
			abortWithOpenAiMessage(c, http.StatusInternalServerError, "Could not resolve model")
			return
		}
		c.Set(textOfferingContextKey, offering)
		c.Next()
	}
}
