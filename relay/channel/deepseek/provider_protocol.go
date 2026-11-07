package deepseek

import (
	"fmt"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
)

var _ channel.ProviderRequestFixer = (*Provider)(nil)
var _ channel.ProviderProtocolComposer = (*Provider)(nil)

func (p *Provider) ComposeProtocol(kind protocol.Type, generic channel.ProtocolHandler) (channel.ProtocolHandler, error) {
	switch kind {
	case protocol.OpenAIChat, protocol.OpenAIResponses, protocol.AnthropicMessages:
		return &channel.RequestRepairProtocol{Kind: kind, Public: generic, Provider: p, ProviderRendersReasoning: kind == protocol.AnthropicMessages}, nil
	default:
		return nil, fmt.Errorf("deepseek does not support protocol %q", kind)
	}
}

func (p *Provider) FixRequest(c *gin.Context, ctx *channel.ProtocolRequestContext, request any) (any, error) {
	info := channel.ProtocolRepairSnapshot(ctx)
	var result any = request
	var err error
	switch request := request.(type) {
	case *dto.GeneralOpenAIRequest:
		result, err = p.fixOpenAIRequest(c, info, request)
	case *dto.OpenAIResponsesRequest:
		result, err = p.fixOpenAIResponsesRequest(c, info, *request)
	case dto.OpenAIResponsesRequest:
		result, err = p.fixOpenAIResponsesRequest(c, info, request)
	case *dto.ClaudeRequest:
		err = applyDeepSeekV4ClaudeThinkingSuffix(info, request)
	}
	channel.CaptureProtocolRepair(ctx, info)
	return result, err
}
