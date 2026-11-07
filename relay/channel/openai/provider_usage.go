package openai

import (
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
)

func (p *Provider) UsagePolicy(kind protocol.Type, request *channel.ProviderRequest) channel.ProviderUsagePolicy {
	if request.ChannelType == constant.ChannelTypeAzure && kind == protocol.OpenAIResponses {
		return channel.ProviderUsagePolicy{ToolUsage: azureResponsesToolUsage}
	}
	return channel.ProviderUsagePolicy{}
}
