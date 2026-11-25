package jina

import (
	"fmt"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
)

var _ channel.ProviderProtocolComposer = (*Provider)(nil)

func (p *Provider) ComposeProtocol(kind protocol.Type, generic channel.ProtocolHandler) (channel.ProtocolHandler, error) {
	if kind == protocol.OpenAIEmbeddings || kind == protocol.Rerank {
		return &channel.WireProtocol{Kind: kind, Public: generic, Wire: &Protocol{}, SupportsCrossProtocol: kind == protocol.OpenAIEmbeddings, PublicResponse: true}, nil
	}
	return nil, fmt.Errorf("jina does not serve text protocol %q", kind)
}
