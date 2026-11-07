package cohere

import (
	"fmt"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
)

var _ channel.ProviderProtocolComposer = (*Provider)(nil)

func (p *Provider) ComposeProtocol(kind protocol.Type, generic channel.ProtocolHandler) (channel.ProtocolHandler, error) {
	switch kind {
	case protocol.OpenAIChat:
		return &channel.WireProtocol{Kind: kind, Public: generic, Wire: &Protocol{}}, nil
	default:
		return nil, fmt.Errorf("cohere does not support text protocol %q", kind)
	}
}
