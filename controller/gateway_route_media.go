package controller

import (
	"bytes"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// gatewayPreparedMedia contains only transient wire, quantity and price probes.
// Shared Route owns selection and admission; transport preparation follows funding.
type gatewayPreparedMedia struct {
	Context   *gin.Context
	Info      *relaycommon.RelayInfo
	Adaptor   *relay.ChannelAdaptor
	Request   *relay.PreparedMediaRequest
	Usage     *dto.Usage
	SaleInput billingexpr.RequestInput
	Choices   int
	Facts     service.GatewayRouteFacts
}

func prepareGatewayMediaRoute(c *gin.Context, selection *service.GatewaySelection, parsed *helper.GatewayMediaRequest, started time.Time, id string, mean int64) (*gatewayPreparedMedia, error) {
	if selection.Channel.Protocol != parsed.Protocol {
		return nil, service.ErrGatewayCapability
	}
	apiType, ok := common.ChannelType2APIType(selection.Provider.Type)
	if !ok {
		return nil, service.ErrGatewayCapability
	}
	adaptor, err := relay.GetChannelAdaptor(apiType, selection.Channel.Protocol)
	if err != nil {
		return nil, service.ErrGatewayCapability
	}
	// Filter this preview without changing stored Key status or rotation state.
	provider := *selection.Provider
	if provider.ProviderInfo.IsMultiKey {
		statuses := maps.Clone(provider.ProviderInfo.MultiKeyStatusList)
		for index, key := range provider.GetKeys() {
			if adaptor.ValidateCredentialFormat(key) != nil {
				if statuses == nil {
					statuses = make(map[int]int)
				}
				statuses[index] = common.ChannelStatusAutoDisabled
			}
		}
		provider.ProviderInfo.MultiKeyStatusList = statuses
	} else if adaptor.ValidateCredentialFormat(provider.Key) != nil {
		return nil, service.ErrGatewayUnavailable
	}
	candidate := c.Copy()
	candidate.Request = c.Request.Clone(c.Request.Context())
	if err := middleware.SetupContextForGatewayCandidate(candidate, &provider, selection.Channel.Model.ModelName); err != nil {
		return nil, service.ErrGatewayUnavailable
	}
	var overrides map[string]any
	if common.UnmarshalJsonStr(selection.Channel.ParameterOverrides, &overrides) != nil {
		return nil, service.ErrGatewayUnavailable
	}
	common.SetContextKey(candidate, constant.ContextKeyChannelParamOverride, overrides)
	var info *relaycommon.RelayInfo
	if parsed.Protocol == protocol.OpenAIImages {
		info = relaycommon.GenRelayInfoImage(candidate, parsed.Request)
	} else {
		info = relaycommon.GenRelayInfoOpenAIAudio(candidate, parsed.Request)
	}
	info.InitChannelMeta(candidate)
	info.StartTime, info.RequestId = started, id
	info.UpstreamModelName, info.OriginModelName = selection.Channel.UpstreamModelName, selection.Channel.UpstreamModelName
	info.TokenizerModel = selection.Channel.Model.ModelName
	info.IsModelMapped = true
	info.StreamStatus = relaycommon.NewStreamStatus()
	info.ResponseFacts = &relaycommon.ResponseFacts{}
	common.SetContextKey(candidate, constant.ContextKeyResponseFacts, info.ResponseFacts)
	common.SetContextKey(candidate, constant.ContextKeyOriginalModel, info.TokenizerModel)
	if helper.ValidateGatewayMediaMultipliers(parsed.Input.Body, selection.Capabilities.Limits) != nil {
		return nil, service.ErrGatewayCapability
	}
	prepared, err := adaptor.PrepareMediaWire(candidate, info, parsed)
	if err != nil {
		return nil, service.ErrGatewayCapability
	}
	meta := prepared.Request.GetTokenCountMeta()
	if audio, ok := prepared.Request.(*dto.AudioRequest); ok && audio.Instructions != "" {
		meta.CombineText += "\n" + audio.Instructions
	}
	input, err := service.CountRequestToken(candidate, meta, info)
	if err != nil || !service.GatewayMediaCapabilitiesMatch(candidate, parsed.Protocol, prepared.Request, selection.Capabilities, input, info.ResponseFacts) {
		return nil, service.ErrGatewayCapability
	}
	selection.InputTokens = input
	limits := selection.Capabilities.Limits
	if limits.ContextLength != nil {
		remaining := *limits.ContextLength - int64(input)
		if limits.MaxOutputTokens == nil || *limits.MaxOutputTokens > remaining {
			limits.MaxOutputTokens = &remaining
		}
	}
	if limits.MaxOutputTokens != nil && parsed.Protocol != protocol.OpenAIAudioSpeech {
		return nil, service.ErrGatewayCapability
	}
	if audio, ok := prepared.Request.(*dto.AudioRequest); ok && parsed.Protocol == protocol.OpenAIAudioSpeech && len(audio.MaxNewTokens) == 0 && limits.MaxOutputTokens != nil {
		if err := adaptor.SetMediaOutputLimit(prepared, *limits.MaxOutputTokens); err != nil {
			return nil, service.ErrGatewayCapability
		}
	}
	if len(prepared.Input.Body) > 0 && helper.ValidateGatewayMediaMultipliers(prepared.Input.Body, limits) != nil {
		return nil, service.ErrGatewayCapability
	}
	info.SetEstimatePromptTokens(input)
	mean = max(mean, int64(prepared.MinimumOutputTokens))
	if limit := limits.MaxOutputTokens; limit != nil {
		if int64(prepared.MinimumOutputTokens) > *limit {
			return nil, service.ErrGatewayCapability
		}
		mean = min(mean, *limit)
	}
	if audio, ok := prepared.Request.(*dto.AudioRequest); ok && len(audio.MaxNewTokens) > 0 {
		var limit uint
		if common.Unmarshal(audio.MaxNewTokens, &limit) != nil {
			return nil, service.ErrGatewayCapability
		}
		mean = min(mean, int64(limit))
	}
	count := 1
	if prepared.Input.ImageCount != nil {
		count = *prepared.Input.ImageCount
	}
	if mean < 0 || count < 1 || count > dto.MaxImageN || mean > int64(common.MaxQuota)/int64(count) {
		return nil, service.ErrGatewayCapability
	}
	mean *= int64(count)
	usage := &dto.Usage{PromptTokens: input, CompletionTokens: int(mean), TotalTokens: input + int(mean)}
	if parsed.Protocol == protocol.OpenAIAudioSpeech {
		usage.CompletionTokenDetails.AudioTokens = int(mean)
	}
	if parsed.Protocol == protocol.OpenAIAudioTranscription {
		usage.PromptTokensDetails.AudioTokens = input
	}
	headers, deferred, err := adaptor.PreviewHeaders(candidate, info, bytes.NewReader(prepared.Body))
	if err != nil {
		return nil, service.ErrGatewayUnavailable
	}
	prepared.Input.Headers = map[string]string{}
	prepared.Input.DeferredHeaders = deferred
	for key, values := range headers {
		prepared.Input.Headers[strings.ToLower(key)] = strings.Join(values, ",")
	}
	if info.ConversionDiagnosticsTruncated() {
		return nil, service.ErrGatewayConversion
	}
	losses := []string{}
	for _, diagnostic := range info.ConversionDiagnostics() {
		if diagnostic.Severity == types.ConversionDiagnosticError {
			return nil, service.ErrGatewayConversion
		}
		if !slices.Contains(losses, diagnostic.Code) {
			losses = append(losses, diagnostic.Code)
		}
	}
	saleInput := parsed.Input
	saleInput.ImageCount = prepared.Input.ImageCount
	return &gatewayPreparedMedia{Context: candidate, Info: info, Adaptor: adaptor, Request: prepared, Usage: usage, SaleInput: saleInput, Choices: count,
		Facts: service.GatewayRouteFacts{Usage: usage, SaleInput: saleInput, PurchaseInput: prepared.Input, ConversionLosses: losses}}, nil
}
