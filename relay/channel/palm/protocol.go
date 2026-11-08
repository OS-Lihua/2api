package palm

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
	a.Init(info)
	switch request := request.(type) {
	case *dto.GeneralOpenAIRequest:
		if request == nil {
			return nil, fmt.Errorf("nil PaLM request")
		}
		if request.MaxTokens != nil || request.MaxCompletionTokens != nil || len(request.Tools) > 0 {
			return nil, fmt.Errorf("PaLM generateMessage does not support output token limits or tools")
		}
		wire := &PaLMChatRequest{Temperature: request.Temperature, CandidateCount: request.N, TopP: request.TopP}
		if request.TopK != nil {
			if *request.TopK < 0 {
				return nil, fmt.Errorf("invalid PaLM top_k")
			}
			value := uint(*request.TopK)
			wire.TopK = &value
		}
		for _, message := range request.Messages {
			if message.Role != "user" && message.Role != "assistant" {
				return nil, fmt.Errorf("PaLM generateMessage does not support message role %q", message.Role)
			}
			if !message.IsStringContent() {
				return nil, fmt.Errorf("PaLM generateMessage requires text messages")
			}
			wire.Prompt.Messages = append(wire.Prompt.Messages, PaLMChatMessage{Author: message.Role, Content: message.StringContent()})
		}
		return wire, nil
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
		return nil, fmt.Errorf("unsupported palm wire request %T", request)
	}
}
