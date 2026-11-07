package middleware

import (
	"errors"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const textOfferingContextKey = "text_gateway_offering"
const TextGatewayStartedAtKey = "text_gateway_started_at"

// ResolveTextOffering runs before token validation so a wallet request does not
// mutate the token's retired quota status. It does not authorize the offering;
// the normal token checks and the gateway's visibility check still apply.
func ResolveTextOffering() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodPost || c.Request.URL.Path != "/v1/chat/completions" {
			c.Next()
			return
		}
		c.Set(TextGatewayStartedAtKey, clock.Now())
		var request struct {
			Model string `json:"model"`
		}
		if common.UnmarshalBodyReusable(c, &request) != nil || request.Model == "" {
			c.Next()
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

func TextOffering(c *gin.Context) *model.ModelOffering {
	value, _ := c.Get(textOfferingContextKey)
	offering, _ := value.(*model.ModelOffering)
	return offering
}
