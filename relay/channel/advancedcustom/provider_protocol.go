package advancedcustom

import (
	"fmt"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	"github.com/gin-gonic/gin"
)

func (p *Provider) ComposeProtocol(kind protocol.Type, generic channel.ProtocolHandler) (channel.ProtocolHandler, error) {
	if kind == protocol.GeminiEmbed {
		return generic, nil
	}
	if channel.IsNonTextProtocol(kind) {
		return channel.ComposeNonTextProtocol(kind, generic, &Protocol{}, protocol.OpenAIEmbeddings, protocol.OpenAIImages, protocol.OpenAIAudioSpeech, protocol.OpenAIAudioTranscription, protocol.Rerank)
	}
	switch kind {
	case protocol.OpenAIChat, protocol.OpenAIResponses, protocol.AnthropicMessages, protocol.GeminiGenerate:
		return &channel.RequestRepairProtocol{Kind: kind, Public: generic, Provider: p}, nil
	default:
		return nil, fmt.Errorf("advanced custom does not support text protocol %q", kind)
	}
}

func (p *Provider) FixRequest(c *gin.Context, context *channel.ProtocolRequestContext, request any) (any, error) {
	repair := *context
	repair.ChannelType = constant.ChannelTypeOpenAI
	result, err := (&openai.Provider{}).FixRequest(c, &repair, request)
	context.UpstreamModelName = repair.UpstreamModelName
	context.ReasoningEffort = repair.ReasoningEffort
	context.Diagnostics = repair.Diagnostics
	return result, err
}
