package channel

import (
	"net/http"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

type ProviderResponseFixer interface {
	FixResponse(*gin.Context, *ProtocolResponseContext, *http.Response) (*http.Response, error)
}
type responseRepairProtocol struct {
	kind     ProtocolResponseContext
	provider ProviderResponseFixer
	handler  ProtocolHandler
}

const responseRepairContextKey = "channel.protocol.provider_response_repaired"

func (p *responseRepairProtocol) ConvertRequest(c *gin.Context, info *relaycommon.RelayInfo, request any) (any, error) {
	return p.handler.ConvertRequest(c, info, request)
}
func (p *responseRepairProtocol) DoResponse(c *gin.Context, response *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	context := p.kind
	context.ChannelType = info.ChannelType
	context.OpenRouterEnterprise = info.ChannelOtherSettings.IsOpenRouterEnterprise()
	context.IsStream = info.IsStream
	repaired, err := p.provider.FixResponse(c, &context, response)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	if c != nil {
		previous, _ := c.Get(responseRepairContextKey)
		c.Set(responseRepairContextKey, true)
		defer c.Set(responseRepairContextKey, previous)
	}
	return p.handler.DoResponse(c, repaired, info)
}
func ProviderResponseWasRepaired(c *gin.Context) bool {
	if c == nil {
		return false
	}
	value, exists := c.Get(responseRepairContextKey)
	return exists && value == true
}
