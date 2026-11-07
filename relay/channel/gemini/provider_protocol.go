package gemini

import (
	"fmt"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
)

var _ channel.ProviderProtocolComposer = (*Provider)(nil)

func (p *Provider) ComposeProtocol(kind protocol.Type, generic channel.ProtocolHandler) (channel.ProtocolHandler, error) {
	if kind == protocol.GeminiEmbed {
		return generic, nil
	}
	if channel.IsNonTextProtocol(kind) {
		return channel.ComposeNonTextProtocol(kind, generic, &Protocol{}, protocol.OpenAIImages)
	}
	switch kind {
	case protocol.GeminiGenerate:
		return generic, nil
	default:
		return nil, fmt.Errorf("gemini does not support text protocol %q", kind)
	}
}
