package channel

import (
	"net/http"

	"github.com/QuantumNous/new-api/pkg/protocol"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

// ProtocolRequestContext contains only request repair inputs and outputs. It
// carries no credentials, user, reservation, price, or settlement state.
type ProtocolRequestContext struct {
	Protocol               protocol.Type
	ChannelType            int
	OriginModelName        string
	UpstreamModelName      string
	IsStream               bool
	SupportStreamOptions   bool
	ReasoningEffort        string
	ReasoningConversion    *dto.ReasoningConversionState
	RequestConversionChain []types.RelayFormat
	RelayFormat            types.RelayFormat
	RelayMode              int
	SystemPrompt           string
	SystemPromptOverride   bool
	Diagnostics            []types.ConversionDiagnostic
}

type ProviderRequestFixer interface {
	FixRequest(*gin.Context, *ProtocolRequestContext, any) (any, error)
}

// RequestRepairProtocol applies provider request repairs after public conversion;
// responses continue through the selected public protocol parser.
type RequestRepairProtocol struct {
	Kind     protocol.Type
	Public   ProtocolHandler
	Provider ProviderRequestFixer
	// ProviderRendersReasoning preserves the provider's own thinking dialect.
	ProviderRendersReasoning bool
}

func (p *RequestRepairProtocol) ConvertRequest(c *gin.Context, info *relaycommon.RelayInfo, request any) (any, error) {
	state := info.ReasoningConversion
	if p.ProviderRendersReasoning {
		info.ReasoningConversion = nil
	}
	converted, err := p.Public.ConvertRequest(c, info, request)
	info.ReasoningConversion = state
	if err != nil {
		return nil, err
	}
	ctx := NewProtocolRequestContext(p.Kind, info)
	repaired, err := p.Provider.FixRequest(c, ctx, converted)
	ApplyProtocolRequestContext(c, info, ctx)
	return repaired, err
}
func (p *RequestRepairProtocol) DoResponse(c *gin.Context, response *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	return p.Public.DoResponse(c, response, info)
}
func NewProtocolRequestContext(kind protocol.Type, info *relaycommon.RelayInfo) *ProtocolRequestContext {
	return &ProtocolRequestContext{Protocol: kind, ChannelType: info.ChannelType,
		OriginModelName: info.OriginModelName, UpstreamModelName: info.UpstreamModelName,
		IsStream: info.IsStream, SupportStreamOptions: info.SupportStreamOptions,
		ReasoningEffort: info.ReasoningEffort, ReasoningConversion: info.ReasoningConversion,
		RequestConversionChain: append([]types.RelayFormat(nil), info.RequestConversionChain...),
		RelayFormat:            info.RelayFormat, RelayMode: protocolRelayMode(kind),
		SystemPrompt: info.ChannelSetting.SystemPrompt, SystemPromptOverride: info.ChannelSetting.SystemPromptOverride}
}
func ApplyProtocolRequestContext(c *gin.Context, info *relaycommon.RelayInfo, ctx *ProtocolRequestContext) {
	info.UpstreamModelName = ctx.UpstreamModelName
	info.SetReasoningEffort(ctx.ReasoningEffort)
	info.RecordConversionDiagnostics(c, ctx.Diagnostics)
}

// ProtocolRepairSnapshot adapts legacy repair implementations at the host
// boundary. It allocates isolated state; it never retains the host RelayInfo.
func ProtocolRepairSnapshot(ctx *ProtocolRequestContext) *relaycommon.RelayInfo {
	info := &relaycommon.RelayInfo{ProviderMeta: &relaycommon.ProviderMeta{}, OriginModelName: ctx.OriginModelName,
		IsStream: ctx.IsStream, ReasoningEffort: ctx.ReasoningEffort,
		ReasoningConversion: ctx.ReasoningConversion, RequestConversionChain: ctx.RequestConversionChain,
		RelayFormat: ctx.RelayFormat, RelayMode: ctx.RelayMode}
	info.ChannelType = ctx.ChannelType
	info.UpstreamModelName = ctx.UpstreamModelName
	info.SupportStreamOptions = ctx.SupportStreamOptions
	info.ChannelSetting.SystemPrompt = ctx.SystemPrompt
	info.ChannelSetting.SystemPromptOverride = ctx.SystemPromptOverride
	return info
}
func CaptureProtocolRepair(ctx *ProtocolRequestContext, info *relaycommon.RelayInfo) {
	ctx.UpstreamModelName = info.UpstreamModelName
	ctx.ReasoningEffort = info.ReasoningEffort
	ctx.Diagnostics = info.ConversionDiagnostics()
}
