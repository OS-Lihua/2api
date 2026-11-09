package middleware

import (
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const textOfferingContextKey = "text_gateway_offering"
const textOfferingMissingContextKey = "text_gateway_offering_missing"
const TextGatewayStartedAtKey = "text_gateway_started_at"

// ResolveTextOffering resolves the request's public ModelOffering name.
// The gateway checks visibility and Key permissions before dispatch.
func ResolveTextOffering() gin.HandlerFunc {
	return func(c *gin.Context) {
		isRealtime := c.Request.Method == http.MethodGet && c.Request.URL.Path == "/v1/realtime"
		kind, name := TextGatewayEndpoint(c.Request.Method, c.Request.URL.Path)
		if isRealtime {
			name = c.Query("model")
		}
		if isRealtime && name == "" || !isRealtime && kind == "" {
			c.Next()
			return
		}
		if c.GetTime(TextGatewayStartedAtKey).IsZero() {
			c.Set(TextGatewayStartedAtKey, clock.Now())
		}
		if name == "" {
			var request struct {
				Model string `json:"model"`
			}
			if common.UnmarshalBodyReusable(c, &request) != nil || request.Model == "" {
				c.Next()
				return
			}
			name = request.Model
		}
		offering, err := model.FindModelOfferingByName(c.Request.Context(), name)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			legacy, legacyErr := model.LegacyModelConfigured(c.Request.Context(), name)
			if legacyErr != nil {
				abortWithOpenAiMessage(c, http.StatusInternalServerError, "Could not resolve model")
				return
			}
			c.Set(textOfferingMissingContextKey, !legacy)
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

// TextGatewayEndpoint identifies only the four stateless HTTP text operations.
// Gemini carries the offering name in its URL; the other protocols use JSON.
func TextGatewayEndpoint(method, path string) (protocol.Type, string) {
	if method != http.MethodPost {
		return "", ""
	}
	switch path {
	case "/v1/chat/completions":
		return protocol.OpenAIChat, ""
	case "/v1/responses":
		return protocol.OpenAIResponses, ""
	case "/v1/messages":
		return protocol.AnthropicMessages, ""
	}
	for _, prefix := range []string{"/v1beta/models/", "/v1/models/"} {
		if modelAction, ok := strings.CutPrefix(path, prefix); ok {
			for _, action := range []string{"generateContent", "streamGenerateContent"} {
				if name, found := strings.CutSuffix(modelAction, ":"+action); found && name != "" {
					return protocol.GeminiGenerate, name
				}
			}
		}
	}
	return "", ""
}

// TextOfferingMissing retains failed resolution until after credential checks.
// A configured legacy model continues through its existing quota and relay path.
func TextOfferingMissing(c *gin.Context) bool {
	return c.GetBool(textOfferingMissingContextKey)
}

func TextOffering(c *gin.Context) *model.ModelOffering {
	value, _ := c.Get(textOfferingContextKey)
	offering, _ := value.(*model.ModelOffering)
	return offering
}
