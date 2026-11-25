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

// ResolveTextOffering resolves public names for stateless text, media, and
// native realtime HTTP. The gateway still checks visibility and Key permissions
// before dispatch.
func ResolveTextOffering() gin.HandlerFunc {
	return func(c *gin.Context) {
		isRealtime := c.Request.Method == http.MethodGet && c.Request.URL.Path == "/v1/realtime"
		kind, name := TextGatewayEndpoint(c.Request.Method, c.Request.URL.Path)
		if isRealtime {
			name = c.Query("model")
		}
		if (isRealtime && name == "") || (!isRealtime && kind == "" && !IsMediaGatewayEndpoint(c.Request.Method, c.Request.URL.Path)) {
			c.Next()
			return
		}
		resolveOffering(c, name)
	}
}

// ResolveTaskOffering shares Offering resolution with stateless text routes.
// Credential and Offering authorization checks remain in their normal handlers.
func ResolveTaskOffering() gin.HandlerFunc {
	return func(c *gin.Context) { resolveOffering(c, "") }
}

func resolveOffering(c *gin.Context, name string) {
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

// TextGatewayEndpoint identifies stateless text, embedding and rerank operations.
// Gemini and the legacy engine URL carry the offering name in the path.
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
	case "/v1/embeddings":
		return protocol.OpenAIEmbeddings, ""
	case "/v1/rerank":
		return protocol.Rerank, ""
	}
	if engine, found := strings.CutPrefix(path, "/v1/engines/"); found {
		if name, found := strings.CutSuffix(engine, "/embeddings"); found && name != "" && !strings.Contains(name, "/") {
			return protocol.OpenAIEmbeddings, name
		}
	}
	for _, prefix := range []string{"/v1beta/models/", "/v1/models/"} {
		if modelAction, ok := strings.CutPrefix(path, prefix); ok {
			for _, action := range []struct {
				name string
				kind protocol.Type
			}{
				{"generateContent", protocol.GeminiGenerate},
				{"streamGenerateContent", protocol.GeminiGenerate},
				{"embedContent", protocol.GeminiEmbed},
				{"batchEmbedContents", protocol.GeminiEmbed},
			} {
				if name, found := strings.CutSuffix(modelAction, ":"+action.name); found && name != "" {
					return action.kind, name
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
