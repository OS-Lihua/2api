package channel

import (
 "net/http"

 "github.com/QuantumNous/new-api/pkg/protocol"
 relaycommon "github.com/QuantumNous/new-api/relay/common"
 "github.com/QuantumNous/new-api/relaykit/types"
 "github.com/gin-gonic/gin"
)

// ProviderUsagePolicy declares provider-reported tool usage and its existing
// price keys. It contains no amount calculation or settlement state.
type ProviderUsagePolicy struct {
 ToolUsage func([]byte) map[string]int
 WebSearchPriceKey string
 WebFetchPriceKey string
}
type ProviderUsagePolicySource interface {
 UsagePolicy(protocol.Type, *ProviderRequest) ProviderUsagePolicy
}

type providerUsageProtocol struct {
 kind protocol.Type
 provider ProviderUsagePolicySource
 handler ProtocolHandler
}
func (p *providerUsageProtocol) apply(info *relaycommon.RelayInfo) {
 metadata:=NewProviderMetadata(info)
 metadata.Protocol=p.kind
 policy:=p.provider.UsagePolicy(p.kind,metadata)
 info.VendorToolUsage=policy.ToolUsage
 info.WebSearchBillingKey=policy.WebSearchPriceKey
 info.WebFetchBillingKey=policy.WebFetchPriceKey
}
func (p *providerUsageProtocol) ConvertRequest(c *gin.Context, info *relaycommon.RelayInfo, request any) (any,error) {
 p.apply(info)
 return p.handler.ConvertRequest(c,info,request)
}
func (p *providerUsageProtocol) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any,*types.NewAPIError) {
 p.apply(info)
 return p.handler.DoResponse(c,resp,info)
}
