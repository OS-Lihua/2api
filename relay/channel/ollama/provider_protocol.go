package ollama

import (
	"fmt"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
)

var _ channel.ProviderProtocolComposer = (*Provider)(nil)

func (p *Provider) ComposeProtocol(kind protocol.Type, generic channel.ProtocolHandler) (channel.ProtocolHandler, error) {
	if channel.IsNonTextProtocol(kind) {
		return channel.ComposeNonTextProtocol(kind, generic, &Protocol{}, protocol.OpenAIEmbeddings)
	}
	switch kind {
	case protocol.OpenAIChat, protocol.OpenAIResponses, protocol.AnthropicMessages:
		return &channel.WireProtocol{Kind: kind, Public: generic, Wire: &Protocol{}, ReportsGatewayFacts: true}, nil
	default:
		return nil, fmt.Errorf("ollama does not support text protocol %q", kind)
	}
}
