package channel

import (
	"net/http"

	"github.com/QuantumNous/new-api/pkg/protocol"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

// ProviderUsageFixer repairs provider-specific usage field locations without
// changing public protocol selection or introducing estimated usage.
type ProtocolResponseContext struct {
	Protocol             protocol.Type
	ChannelType          int
	OpenRouterEnterprise bool
	IsStream             bool
}

type ProviderUsageFixer interface {
	RepairUsage(*ProtocolResponseContext, *dto.Usage, []byte)
}
type usageRepairContext struct {
	context  *ProtocolResponseContext
	provider ProviderUsageFixer
}

const usageRepairContextKey = "channel.protocol.provider_usage_repair"

type usageRepairProtocol struct {
	kind     protocol.Type
	provider ProviderUsageFixer
	handler  ProtocolHandler
}

func (p *usageRepairProtocol) ConvertRequest(c *gin.Context, info *relaycommon.RelayInfo, request any) (any, error) {
	return p.handler.ConvertRequest(c, info, request)
}
func (p *usageRepairProtocol) DoResponse(c *gin.Context, response *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	if c == nil {
		return p.handler.DoResponse(c, response, info)
	}
	previous, _ := c.Get(usageRepairContextKey)
	context := &ProtocolResponseContext{Protocol: p.kind, ChannelType: info.ChannelType, OpenRouterEnterprise: info.ChannelOtherSettings.IsOpenRouterEnterprise(), IsStream: info.IsStream}
	c.Set(usageRepairContextKey, usageRepairContext{context: context, provider: p.provider})
	defer c.Set(usageRepairContextKey, previous)
	return p.handler.DoResponse(c, response, info)
}

// RepairProviderUsage returns false for legacy callers, which retain their
// compatibility repair path until the final host switch.
func RepairProviderUsage(c *gin.Context, usage *dto.Usage, body []byte) bool {
	if c == nil {
		return false
	}
	value, exists := c.Get(usageRepairContextKey)
	if !exists {
		return false
	}
	repair, ok := value.(usageRepairContext)
	if !ok {
		return false
	}
	repair.provider.RepairUsage(repair.context, usage, body)
	return true
}
