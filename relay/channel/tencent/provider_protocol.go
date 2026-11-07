package tencent

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

var _ channel.ProviderProtocolComposer = (*Provider)(nil)

func (p *Provider) ComposeProtocol(kind protocol.Type, generic channel.ProtocolHandler) (channel.ProtocolHandler, error) {
	switch kind {
	case protocol.OpenAIChat, protocol.OpenAIResponses:
		return &tencentProtocol{kind: kind, public: generic, native: &Protocol{}}, nil
	default:
		return nil, fmt.Errorf("Tencent does not support protocol %q", kind)
	}
}

// tencentProtocol selects only the account's wire dialect, never its public
// protocol. The native TC3 API and TokenHub share no legacy Adaptor callback.
type tencentProtocol struct {
	kind   protocol.Type
	public channel.ProtocolHandler
	native *Protocol
}

func (p *tencentProtocol) ConvertRequest(c *gin.Context, info *relaycommon.RelayInfo, request any) (any, error) {
	if strings.Contains(info.ApiKey, "|") {
		if p.kind != protocol.OpenAIChat {
			return nil, fmt.Errorf("Tencent TC3 requires explicitly selected OpenAI Chat protocol")
		}
		wire := channel.WireProtocol{Kind: p.kind, Public: p.public, Wire: p.native}
		return wire.ConvertRequest(c, info, request)
	}
	converted, err := p.public.ConvertRequest(c, info, request)
	if err != nil {
		return nil, err
	}
	context := channel.NewProtocolRequestContext(p.kind, info)
	repaired, err := (&openai.Provider{}).FixRequest(c, context, converted)
	channel.ApplyProtocolRequestContext(c, info, context)
	return repaired, err
}
func (p *tencentProtocol) DoResponse(c *gin.Context, response *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	if strings.Contains(info.ApiKey, "|") {
		return p.native.DoResponse(c, response, info)
	}
	return p.public.DoResponse(c, response, info)
}
