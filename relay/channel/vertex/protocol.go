package vertex

import (
	"fmt"

	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
)

// Adaptor preserves the legacy entry points and struct literals until the host
// switches callers to the independent provider and protocol ports.
type Adaptor = Protocol

var _ channel.ProtocolHandler = (*Protocol)(nil)

// ConvertRequest dispatches a DTO to the actual wire implementation. Public
// protocol selection is performed before this provider dialect boundary.
func (a *Protocol) ConvertRequest(c *gin.Context, info *relaycommon.RelayInfo, request any) (any, error) {
	switch request := request.(type) {
	case *dto.GeneralOpenAIRequest:
		return a.ConvertOpenAIRequest(c, info, request)
	case *dto.ClaudeRequest:
		return a.ConvertClaudeRequest(c, info, request)
	case *dto.GeminiChatRequest:
		return a.ConvertGeminiRequest(c, info, request)
	case *dto.OpenAIResponsesRequest:
		if request == nil {
			return nil, fmt.Errorf("nil Responses request")
		}
		return a.ConvertOpenAIResponsesRequest(c, info, *request)
	case dto.OpenAIResponsesRequest:
		return a.ConvertOpenAIResponsesRequest(c, info, request)
	case dto.EmbeddingRequest:
		return a.ConvertEmbeddingRequest(c, info, request)
	case dto.RerankRequest:
		return a.ConvertRerankRequest(c, info.RelayMode, request)
	case dto.ImageRequest:
		return a.ConvertImageRequest(c, info, request)
	case dto.AudioRequest:
		return a.ConvertAudioRequest(c, info, request)
	default:
		return nil, fmt.Errorf("unsupported vertex wire request %T", request)
	}
}
