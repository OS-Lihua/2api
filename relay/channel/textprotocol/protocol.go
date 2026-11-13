package textprotocol

import (
	"fmt"
	"mime"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/claude"
	"github.com/QuantumNous/new-api/relay/channel/gemini"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

// Handler owns the four public text protocols. Provider identity never selects
// the conversion target or response parser.
type Handler struct {
	kind   protocol.Type
	format types.RelayFormat
}

var _ channel.ProtocolHandler = (*Handler)(nil)

func New(kind protocol.Type) (channel.ProtocolHandler, error) {
	switch kind {
	case protocol.OpenAIChat, protocol.OpenAIResponses, protocol.AnthropicMessages, protocol.GeminiGenerate:
	default:
		return nil, fmt.Errorf("unsupported text protocol %q", kind)
	}
	format, err := channel.ProtocolRelayFormat(kind)
	if err != nil {
		return nil, err
	}
	return &Handler{kind: kind, format: format}, nil
}
func (p *Handler) ConvertRequest(c *gin.Context, info *relaycommon.RelayInfo, request any) (any, error) {
	return channel.ConvertProtocolRequest(c, info, p.kind, request)
}
func (p *Handler) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	info.FinalRequestRelayFormat = p.format
	info.UpstreamProtocol = p.kind
	if info.ResponseFacts != nil {
		resp.Body = channel.BoundGatewayResponseBody(resp.Body)
		contentType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		jsonResponse := contentType == "application/json" || strings.HasSuffix(contentType, "+json")
		if contentType == "text/event-stream" || info.IsStream && !jsonResponse {
			return p.gatewayStreamResponse(c, resp, info)
		}
		return p.gatewayJSONResponse(c, resp, info)
	}
	switch p.kind {
	case protocol.OpenAIChat:
		if info.RelayFormat == types.RelayFormatOpenAIResponses {
			if info.IsStream {
				return openai.OaiChatToResponsesStreamHandler(c, info, resp)
			}
			return openai.OaiChatToResponsesHandler(c, info, resp)
		}
		if info.IsStream {
			return openai.OaiStreamHandler(c, info, resp)
		}
		return openai.OpenaiHandler(c, info, resp)
	case protocol.OpenAIResponses:
		if info.RelayFormat != types.RelayFormatOpenAIResponses {
			if info.IsStream {
				return openai.OaiResponsesToChatStreamHandler(c, info, resp)
			}
			return openai.OaiResponsesToChatHandler(c, info, resp)
		}
		if info.IsStream {
			return openai.OaiResponsesStreamHandler(c, info, resp)
		}
		return openai.OaiResponsesHandler(c, info, resp)
	case protocol.AnthropicMessages:
		if info.RelayFormat == types.RelayFormatOpenAIResponses && info.IsStream {
			return claude.ClaudeResponsesStreamHandler(c, resp, info)
		}
		if info.IsStream {
			return claude.ClaudeStreamHandler(c, resp, info)
		}
		return claude.ClaudeHandler(c, resp, info)
	case protocol.GeminiGenerate:
		if info.RelayFormat == types.RelayFormatOpenAIResponses {
			if info.IsStream {
				return gemini.GeminiResponsesStreamHandler(c, info, resp)
			}
			return gemini.GeminiResponsesHandler(c, info, resp)
		}
		if info.RelayFormat == types.RelayFormatGemini {
			if info.IsStream {
				return gemini.GeminiTextGenerationStreamHandler(c, info, resp)
			}
			return gemini.GeminiTextGenerationHandler(c, info, resp)
		}
		if info.IsStream {
			return gemini.GeminiChatStreamHandler(c, info, resp)
		}
		return gemini.GeminiChatHandler(c, info, resp)
	}
	return nil, types.NewError(fmt.Errorf("unsupported text protocol %q", p.kind), types.ErrorCodeInvalidRequest)
}
