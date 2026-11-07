package ali

import (
	"fmt"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
)

func (p *Provider) ComposeProtocol(kind protocol.Type, generic channel.ProtocolHandler) (channel.ProtocolHandler, error) {
	if channel.IsNonTextProtocol(kind) {
		return channel.ComposeNonTextProtocol(kind, generic, &Protocol{}, protocol.OpenAIEmbeddings, protocol.Rerank)
	}
	switch kind {
	case protocol.OpenAIChat, protocol.OpenAIResponses, protocol.AnthropicMessages:
		return generic, nil
	default:
		return nil, fmt.Errorf("ali does not support protocol %q", kind)
	}
}
