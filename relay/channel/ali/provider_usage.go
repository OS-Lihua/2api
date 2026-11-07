package ali

import (
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
)

func (p *Provider) UsagePolicy(kind protocol.Type, request *channel.ProviderRequest) channel.ProviderUsagePolicy {
	policy := channel.ProviderUsagePolicy{}
	if kind == protocol.OpenAIResponses {
		policy.ToolUsage = aliResponsesToolUsage
	}
	if kind == protocol.AnthropicMessages && supportsAliAnthropicMessages(request.UpstreamModelName) {
		policy.WebSearchPriceKey = aliSearchAgentKey
	}
	return policy
}
