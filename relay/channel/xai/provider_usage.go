package xai

import (
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
)

func (p *Provider) UsagePolicy(kind protocol.Type, request *channel.ProviderRequest) channel.ProviderUsagePolicy {
	if kind == protocol.OpenAIResponses {
		return channel.ProviderUsagePolicy{ToolUsage: xaiResponsesToolUsage}
	}
	return channel.ProviderUsagePolicy{}
}
