package deepseek

import (
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

func (p *Provider) UsagePolicy(kind protocol.Type, request *channel.ProviderRequest) channel.ProviderUsagePolicy {
	policy := channel.ProviderUsagePolicy{WebSearchPriceKey: SearchToolPriceKey, WebFetchPriceKey: FetchToolPriceKey}
	if kind == protocol.OpenAIResponses {
		policy.ToolUsage = responsesToolUsage
	}
	return policy
}

func (p *Provider) RepairUsage(context *channel.ProtocolResponseContext, usage *dto.Usage, body []byte) {
	if usage != nil && usage.PromptTokensDetails.CachedTokens == 0 && usage.PromptCacheHitTokens != 0 {
		usage.PromptTokensDetails.CachedTokens = usage.PromptCacheHitTokens
	}
}
