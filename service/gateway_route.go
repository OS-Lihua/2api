package service

import (
	"errors"
	"slices"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/capability"
	"github.com/QuantumNous/new-api/pkg/protocol"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/convmeta"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

type GatewaySelection struct {
	Channel        model.Channel
	Provider       *model.Provider
	InputTokens    int
	Capabilities   capability.Set
	Diagnostics    map[string]any
	Conversion     *relayconvert.RequestResult
	ResponsesTools *convmeta.ResponsesToolState
}

var ErrGatewayCapability = errors.New("no Channel supports the requested capabilities")
var ErrGatewayUnavailable = errors.New("no Channel is available")

// SelectGatewayChannel preconverts candidates before any Attempt or reservation.
// Same-protocol candidates rank before good, fair and discouraged conversions.
func SelectGatewayChannel(c *gin.Context, offering *model.ModelOffering, request *dto.GeneralOpenAIRequest, native ...dto.Request) (*GatewaySelection, error) {
	original := dto.Request(request)
	if len(native) > 0 {
		original = native[0]
	}
	ids := []int{}
	for _, member := range offering.Members {
		if member.Tier == 1 {
			ids = append(ids, member.ModelID)
		}
	}
	var candidates []model.Channel
	if err := model.OfferingChannelsQuery(model.DB.WithContext(c.Request.Context()), offering, ids).Preload("Model").Order("id").Find(&candidates).Error; err != nil {
		return nil, err
	}
	diagnostics := map[string]any{"method": "single_channel", "candidate_count": len(candidates), "eligible_count": 0}
	counts := map[string]int{}
	excluded := []map[string]any{}
	accepted := GatewayAcceptedConversionLosses(offering)
	var selected *GatewaySelection
	var rejected []types.ConversionDiagnostic
	bestRank := 5
	for _, candidate := range candidates {
		reason := ""
		var provider *model.Provider
		var caps capability.Set
		var tokens int
		var conversion *relayconvert.RequestResult
		var responsesTools *convmeta.ResponsesToolState
		rank := 0
		switch {
		case candidate.Status != model.ChannelEnabled || !candidate.MappingConfirmed || candidate.Model.Status != 1:
			reason = "disabled"
		case candidate.Protocol != protocol.OpenAIChat && candidate.Protocol != protocol.OpenAIResponses && candidate.Protocol != protocol.AnthropicMessages && candidate.Protocol != protocol.GeminiGenerate:
			reason = "protocol"
		default:
			var err error
			provider, err = model.CacheGetProvider(candidate.ProviderID)
			if err != nil || provider == nil || provider.Status != common.ChannelStatusEnabled {
				reason = "disabled"
				break
			}
			caps, err = candidate.EffectiveCapabilities(candidate.Model.Capabilities)
			if err != nil || !GatewayCapabilitiesMatch(request, caps, 0) {
				reason = "capability"
				break
			}
			common.SetContextKey(c, constant.ContextKeyOriginalModel, candidate.Model.ModelName)
			info := relaycommon.GenRelayInfoOpenAI(c, request)
			info.TokenizerModel = candidate.Model.ModelName
			tokens, err = CountRequestToken(c, request.GetTokenCountMeta(), info)
			if err != nil {
				return nil, errors.New("could not estimate input tokens")
			}
			if !GatewayCapabilitiesMatch(request, caps, tokens) {
				reason = "capability"
				break
			}
			var providerSettings dto.ProviderSettings
			if provider.Setting != nil && *provider.Setting != "" && common.UnmarshalJsonStr(*provider.Setting, &providerSettings) != nil {
				reason = "configuration"
				break
			}
			var otherSettings dto.ProviderOtherSettings
			if provider.OtherSettings != "" && common.UnmarshalJsonStr(provider.OtherSettings, &otherSettings) != nil {
				reason = "configuration"
				break
			}
			if preset := common.GetAdvancedCustomPreset(provider.Type); preset != nil {
				otherSettings.AdvancedCustom = preset
			}
			info.ProviderMeta = &relaycommon.ProviderMeta{
				ChannelType: provider.Type, ChannelId: candidate.ProviderID,
				UpstreamModelName: candidate.UpstreamModelName,
				ChannelSetting:    providerSettings, ChannelOtherSettings: otherSettings,
			}
			info.ChannelOtherSettings.ToolLossPolicy = string(types.ConversionLossPolicyAllow)
			conversion, err = PreconvertGatewayRequest(c, info, candidate.Protocol, original, accepted)
			if err != nil {
				reason = "conversion_loss"
				if conversion != nil {
					rejected = append(rejected, conversion.Diagnostics...)
				}
				if conversion == nil || len(conversion.Diagnostics) == 0 {
					rejected = append(rejected, types.ConversionDiagnostic{Code: "request_conversion_failed", Path: "request", Message: "The target protocol cannot represent this request", Severity: types.ConversionDiagnosticError})
				}
				break
			}
			responsesTools = info.ResponsesToolState()
			if conversion.From != conversion.To {
				rank = 3
				if conversion.Quality == relayconvert.RequestConverterQualityGood {
					rank = 1
				} else if conversion.Quality == relayconvert.RequestConverterQualityFair {
					rank = 2
				}
			}
		}
		if reason != "" {
			counts[reason]++
			if len(excluded) < 10 {
				excluded = append(excluded, map[string]any{"channel_id": candidate.ID, "reason": reason})
			}
			continue
		}
		diagnostics["eligible_count"] = diagnostics["eligible_count"].(int) + 1
		if selected == nil || rank < bestRank {
			bestRank = rank
			selected = &GatewaySelection{Channel: candidate, Provider: provider, InputTokens: tokens, Capabilities: caps, Conversion: conversion, ResponsesTools: responsesTools}
		}
	}
	diagnostics["excluded_counts"], diagnostics["excluded"] = counts, excluded
	if selected == nil {
		if counts["conversion_loss"] > 0 {
			return nil, &GatewayConversionError{Diagnostics: rejected}
		}
		if counts["capability"] > 0 {
			return nil, ErrGatewayCapability
		}
		return nil, ErrGatewayUnavailable
	}
	selected.Diagnostics = diagnostics
	return selected, nil
}

// GatewayCapabilitiesMatch checks the same declaration before and after
// parameter overrides. It uses the protocol DTO's media and tool extraction.
func GatewayCapabilitiesMatch(request *dto.GeneralOpenAIRequest, caps capability.Set, inputTokens int) bool {
	if request.Stream != nil && *request.Stream && !slices.Contains(caps.Features, capability.Streaming) {
		return false
	}
	tools := len(request.Tools) > 0 || len(request.Functions) > 0
	for _, message := range request.Messages {
		tools = tools || len(message.Tools) > 0 || len(message.ParseToolCalls()) > 0 || message.Role == "tool"
	}
	if tools && !slices.Contains(caps.Features, capability.ToolCalling) {
		return false
	}
	if (request.ReasoningEffort != "" && request.ReasoningEffort != "none" || len(request.Reasoning) > 0) && !slices.Contains(caps.Features, capability.Reasoning) {
		return false
	}
	if request.ResponseFormat != nil && request.ResponseFormat.Type != "" && request.ResponseFormat.Type != "text" && !slices.Contains(caps.Features, capability.StructuredOutput) {
		return false
	}
	if (request.PromptCacheKey != "" || len(request.PromptCacheOptions) > 0 || len(request.PromptCacheRetention) > 0) && !slices.Contains(caps.Features, capability.Caching) {
		return false
	}
	if !slices.Contains(caps.InputModalities, capability.Text) || !slices.Contains(caps.OutputModalities, capability.Text) {
		return false
	}
	var outputs []capability.Modality
	if len(request.Modalities) > 0 && common.Unmarshal(request.Modalities, &outputs) != nil {
		return false
	}
	for _, output := range outputs {
		if !slices.Contains(caps.OutputModalities, output) {
			return false
		}
	}
	images := int64(0)
	for _, file := range request.GetTokenCountMeta().Files {
		modality := capability.PDF
		switch file.FileType {
		case types.FileTypeImage:
			modality = capability.Image
			images++
		case types.FileTypeAudio:
			modality = capability.Audio
		case types.FileTypeVideo:
			modality = capability.Video
		}
		if !slices.Contains(caps.InputModalities, modality) {
			return false
		}
	}
	output := int64(request.GetMaxTokens())
	if caps.Limits.MaxOutputTokens != nil && output > *caps.Limits.MaxOutputTokens {
		return false
	}
	if caps.Limits.ContextLength != nil && int64(inputTokens)+output > *caps.Limits.ContextLength {
		return false
	}
	return caps.Limits.MaxImages == nil || images <= *caps.Limits.MaxImages
}
