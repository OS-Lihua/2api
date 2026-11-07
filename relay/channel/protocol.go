package channel

import (
	"fmt"
	"net/http"

	"github.com/QuantumNous/new-api/pkg/protocol"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

// ProviderProtocolComposer declares a provider's wire dialect for an explicitly
// selected public protocol. Implementations must reject incompatible protocols.
type ProviderProtocolComposer interface {
	ComposeProtocol(protocol.Type, ProtocolHandler) (ProtocolHandler, error)
}

func ComposeProviderProtocol(provider ProviderAdapter, kind protocol.Type, generic ProtocolHandler) (ProtocolHandler, error) {
	if generic == nil {
		return nil, fmt.Errorf("missing handler for protocol %q", kind)
	}
	handler := ProtocolHandler(&PublicProtocol{Kind: kind, Handler: generic})
	if composer, ok := provider.(ProviderProtocolComposer); ok {
		var err error
		handler, err = composer.ComposeProtocol(kind, handler)
		if err != nil {
			return nil, err
		}
	}
	if source, ok := provider.(ProviderUsagePolicySource); ok {
		handler = &providerUsageProtocol{kind: kind, provider: source, handler: handler}
	}
	if fixer, ok := provider.(ProviderUsageFixer); ok {
		handler = &usageRepairProtocol{kind: kind, provider: fixer, handler: handler}
	}
	if fixer, ok := provider.(ProviderResponseFixer); ok {
		handler = &responseRepairProtocol{kind: ProtocolResponseContext{Protocol: kind}, provider: fixer, handler: handler}
	}
	return handler, nil
}

// ConvertProtocolRequest selects the public DTO independently of provider identity.
func ConvertProtocolRequest(c *gin.Context, info *relaycommon.RelayInfo, kind protocol.Type, request any) (any, error) {
	format, err := ProtocolRelayFormat(kind)
	if err != nil {
		return nil, err
	}
	result, err := relayconvert.ConvertRequest(c, info, format, request)
	if err != nil {
		return nil, err
	}
	info.RecordConversionDiagnostics(c, result.Diagnostics)
	relaycommon.AppendRequestConversionFromRequest(info, result.Value)
	info.FinalRequestRelayFormat = format
	return result.Value, nil
}

// WireProtocol composes a public DTO conversion with an explicitly declared
// provider wire dialect. The dialect owns its response parser and usage facts.
// It never receives or calls the legacy Adaptor interface.
type WireProtocol struct {
	Kind                  protocol.Type
	Public                ProtocolHandler
	Wire                  ProtocolHandler
	SupportsCrossProtocol bool
}

func (p *WireProtocol) ConvertRequest(c *gin.Context, info *relaycommon.RelayInfo, request any) (any, error) {
	if !p.SupportsCrossProtocol {
		format, err := ProtocolRelayFormat(p.Kind)
		if err != nil {
			return nil, err
		}
		if info.RelayFormat != format {
			return nil, fmt.Errorf("provider wire dialect for %q does not support downstream format %q", p.Kind, info.RelayFormat)
		}
	}
	converted, err := p.Public.ConvertRequest(c, info, request)
	if err != nil {
		return nil, err
	}
	mode := info.RelayMode
	info.RelayMode = protocolRelayMode(p.Kind)
	defer func() { info.RelayMode = mode }()
	return p.Wire.ConvertRequest(c, info, converted)
}
func (p *WireProtocol) DoResponse(c *gin.Context, response *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	mode := info.RelayMode
	info.RelayMode = protocolRelayMode(p.Kind)
	defer func() { info.RelayMode = mode }()
	return p.Wire.DoResponse(c, response, info)
}

// ProtocolRelayFormat maps only the explicit public protocol enum.
func ProtocolRelayFormat(kind protocol.Type) (types.RelayFormat, error) {
	switch kind {
	case protocol.OpenAIChat:
		return types.RelayFormatOpenAI, nil
	case protocol.OpenAIResponses:
		return types.RelayFormatOpenAIResponses, nil
	case protocol.AnthropicMessages:
		return types.RelayFormatClaude, nil
	case protocol.GeminiGenerate:
		return types.RelayFormatGemini, nil
	default:
		return "", fmt.Errorf("unsupported text protocol %q", kind)
	}
}

func protocolRelayMode(kind protocol.Type) int {
	switch kind {
	case protocol.OpenAIResponses:
		return relayconstant.RelayModeResponses
	case protocol.GeminiGenerate:
		return relayconstant.RelayModeGemini
	default:
		return relayconstant.RelayModeChatCompletions
	}
}
