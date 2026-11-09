package vertex

import (
	"fmt"

	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
)

var _ channel.ProviderProtocolComposer = (*Provider)(nil)
var _ channel.ProviderRequestFixer = (*Provider)(nil)

func (p *Provider) ComposeProtocol(kind protocol.Type, generic channel.ProtocolHandler) (channel.ProtocolHandler, error) {
	if channel.IsNonTextProtocol(kind) {
		return channel.ComposeNonTextProtocol(kind, generic, &Protocol{RequestMode: RequestModeGemini}, protocol.OpenAIImages)
	}
	switch kind {
	case protocol.OpenAIChat, protocol.AnthropicMessages, protocol.GeminiGenerate:
		return &channel.RequestRepairProtocol{Kind: kind, Public: generic, Provider: p}, nil
	default:
		return nil, fmt.Errorf("vertex does not support protocol %q", kind)
	}
}
func (p *Provider) FixRequest(c *gin.Context, ctx *channel.ProtocolRequestContext, request any) (any, error) {
	switch ctx.Protocol {
	case protocol.AnthropicMessages:
		req, ok := request.(*dto.ClaudeRequest)
		if !ok || req == nil {
			return nil, fmt.Errorf("expected Anthropic Messages DTO, got %T", request)
		}
		model := req.Model
		if mapped, ok := claudeModelMap[ctx.UpstreamModelName]; ok {
			model = mapped
		}
		if c != nil {
			c.Set("request_model", model)
		}
		return &vertexClaudeRequest{Request: req}, nil
	case protocol.GeminiGenerate:
		req, ok := request.(*dto.GeminiChatRequest)
		if !ok || req == nil {
			return nil, fmt.Errorf("expected Gemini generateContent DTO, got %T", request)
		}
		return req, nil
	case protocol.OpenAIChat:
		if _, ok := request.(*dto.GeneralOpenAIRequest); !ok {
			return nil, fmt.Errorf("expected OpenAI Chat DTO, got %T", request)
		}
		return request, nil
	default:
		return nil, fmt.Errorf("vertex does not support protocol %q", ctx.Protocol)
	}
}
