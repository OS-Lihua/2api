package middleware

import (
	"errors"
	"net/http"
	"slices"

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
func ResolveTextOffering(paths ...string) gin.HandlerFunc {
	if len(paths) == 0 {
		paths = []string{"/v1/chat/completions"}
	}
	return func(c *gin.Context) {
		isRealtime := c.Request.Method == http.MethodGet && c.Request.URL.Path == "/v1/realtime"
		if !isRealtime && (c.Request.Method != http.MethodPost || !slices.Contains(paths, c.Request.URL.Path)) {
			c.Next()
			return
		}
		c.Set(TextGatewayStartedAtKey, clock.Now())
		var request struct {
			Model string `json:"model"`
		}
		if isRealtime {
			request.Model = c.Query("model")
		} else if common.UnmarshalBodyReusable(c, &request) != nil {
			c.Next()
			return
		}
		if request.Model == "" {
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
