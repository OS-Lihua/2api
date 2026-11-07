package newapi

import (
	"fmt"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	"github.com/gin-gonic/gin"
)

func (p *Provider) ComposeProtocol(kind protocol.Type, generic channel.ProtocolHandler) (channel.ProtocolHandler, error) {
	switch kind {
	case protocol.OpenAIChat, protocol.OpenAIResponses, protocol.AnthropicMessages, protocol.GeminiGenerate:
		return &channel.RequestRepairProtocol{Kind: kind, Public: generic, Provider: p}, nil
	default:
		return nil, fmt.Errorf("newapi does not support text protocol %q", kind)
	}
}
func (p *Provider) FixRequest(c *gin.Context, context *channel.ProtocolRequestContext, request any) (any, error) {
	return (&openai.Provider{}).FixRequest(c, context, request)
}
