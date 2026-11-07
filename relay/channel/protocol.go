package channel

import (
	"fmt"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/protocol"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

// ProviderProtocolComposer declares a provider's wire dialect for an explicitly
// selected public protocol. Implementations must reject incompatible protocols.
type ProviderProtocolComposer interface {
	ComposeProtocol(protocol.Type, ProtocolHandler) (ProtocolHandler, error)
}

// ChatRequestProjector exposes effective validation facts from a selected native
// wire DTO. It neither chooses a protocol nor performs transport or billing.
type ChatRequestProjector interface {
	ProjectChatRequest([]byte, dto.GeneralOpenAIRequest) (*dto.GeneralOpenAIRequest, error)
}

// ChatRequestPreparer resolves Provider-managed request data after reservation.
// The selected wire DTO retains its own upload and request format.
type ChatRequestPreparer interface {
	PrepareChatRequest(*gin.Context, *relaycommon.RelayInfo, []byte) ([]byte, error)
}

// ChatOutputLimitSetter applies a validated default to a native wire body.
type ChatOutputLimitSetter interface {
	SetChatOutputLimit([]byte, uint) ([]byte, error)
}

// ProjectChatRequestControls retains public multiplier validation even when a
// native override contains public fields that the wire service would ignore.
func ProjectChatRequestControls(body []byte, public dto.GeneralOpenAIRequest) (*dto.GeneralOpenAIRequest, error) {
	var controls struct {
		N                   *int  `json:"n"`
		MaxTokens           *uint `json:"max_tokens"`
		MaxCompletionTokens *uint `json:"max_completion_tokens"`
		MinTokens           *uint `json:"min_tokens"`
	}
	if err := common.Unmarshal(body, &controls); err != nil {
		return nil, err
	}
	if controls.N != nil {
		public.N = controls.N
	}
	if controls.MaxTokens != nil {
		public.MaxTokens = controls.MaxTokens
	}
	if controls.MaxCompletionTokens != nil {
		public.MaxCompletionTokens = controls.MaxCompletionTokens
	}
	if controls.MinTokens != nil {
		public.MinTokens = controls.MinTokens
	}
	return &public, nil
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
		if handler == nil {
			return nil, fmt.Errorf("provider did not supply a handler for protocol %q", kind)
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
	info.RelayMode = p.relayMode(info)
	defer func() { info.RelayMode = mode }()
	converted, err = p.Wire.ConvertRequest(c, info, converted)
	if err == nil && converted == nil {
		return nil, fmt.Errorf("provider wire conversion for %q returned no request", p.Kind)
	}
	return converted, err
}
func (p *WireProtocol) DoResponse(c *gin.Context, response *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	mode := info.RelayMode
	info.RelayMode = p.relayMode(info)
	defer func() { info.RelayMode = mode }()
	if p.SupportsCrossProtocol && p.Kind == protocol.OpenAIEmbeddings && info.RelayFormat == types.RelayFormatGemini {
		return p.Public.DoResponse(c, response, info)
	}
	return p.Wire.DoResponse(c, response, info)
}

func (p *WireProtocol) relayMode(info *relaycommon.RelayInfo) int {
	if p.Kind == protocol.OpenAIImages && info.RelayMode == relayconstant.RelayModeImagesEdits ||
		p.Kind == protocol.OpenAIAudioTranscription && info.RelayMode == relayconstant.RelayModeAudioTranslation {
		return info.RelayMode
	}
	return protocolRelayMode(p.Kind)
}

// ProtocolRelayFormat maps only the explicit public protocol enum.
func ProtocolRelayFormat(kind protocol.Type) (types.RelayFormat, error) {
	return kind.RelayFormat()
}

func protocolRelayMode(kind protocol.Type) int {
	switch kind {
	case protocol.OpenAIResponses:
		return relayconstant.RelayModeResponses
	case protocol.GeminiGenerate:
		return relayconstant.RelayModeGemini
	case protocol.GeminiEmbed, protocol.OpenAIEmbeddings:
		return relayconstant.RelayModeEmbeddings
	case protocol.OpenAIImages:
		return relayconstant.RelayModeImagesGenerations
	case protocol.OpenAIAudioSpeech:
		return relayconstant.RelayModeAudioSpeech
	case protocol.OpenAIAudioTranscription:
		return relayconstant.RelayModeAudioTranscription
	case protocol.Rerank:
		return relayconstant.RelayModeRerank
	default:
		return relayconstant.RelayModeChatCompletions
	}
}
