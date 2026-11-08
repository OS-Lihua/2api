package service

import (
	"cmp"
	"errors"
	"slices"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/capability"
	"github.com/QuantumNous/new-api/pkg/channelhealth"
	"github.com/QuantumNous/new-api/pkg/clock"
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
	Admission      channelhealth.Admission
	conversionRank int
}

var ErrGatewayCapability = errors.New("no Channel supports the requested capabilities")
var ErrGatewayUnavailable = errors.New("no Channel is available")

// GatewaySelectionOptions keeps the native sending source separate from its
// validation projection and excludes candidates rejected by previous attempts.
type GatewaySelectionOptions struct {
	Native     dto.Request
	Exclusions *GatewayExclusions
}

// SelectGatewayChannel preconverts candidates before any Attempt or reservation.
// Protocol and conversion quality rank before runtime health ordering; the
// shared health manager owns admission and recovery probe claims.
func SelectGatewayChannel(c *gin.Context, offering *model.ModelOffering, request *dto.GeneralOpenAIRequest, options ...GatewaySelectionOptions) (*GatewaySelection, error) {
	original := dto.Request(request)
	var failures *GatewayExclusions
	if len(options) > 0 {
		if options[0].Native != nil {
			original = options[0].Native
		}
		failures = options[0].Exclusions
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
	eligible := []*GatewaySelection{}
	var rejected []types.ConversionDiagnostic
	_, mediaPrepared := c.Get(gatewayMediaContextKey)
	for _, candidate := range candidates {
		if failures != nil && (failures.Channels[candidate.ID] || failures.Providers[candidate.ProviderID]) {
			counts["already_failed"]++
			continue
		}
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
			if !mediaPrepared && candidate.Protocol != protocol.OpenAIChat {
				if err := PrepareGatewayMedia(c, original); err != nil {
					reason = "conversion_loss"
					rejected = append(rejected, types.ConversionDiagnostic{Code: "request_media_unavailable", Path: "request", Message: "The request media could not be prepared", Severity: types.ConversionDiagnosticError})
					break
				}
				mediaPrepared = true
			}
			common.SetContextKey(c, constant.ContextKeyOriginalModel, candidate.Model.ModelName)
			info := relaycommon.GenRelayInfoOpenAI(c, request)
			info.TokenizerModel = candidate.Model.ModelName
			tokens, err = CountRequestToken(c, request.GetTokenCountMeta(), info, GatewayRequestTokenOptions(c))
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
		eligible = append(eligible, &GatewaySelection{Channel: candidate, Provider: provider, InputTokens: tokens, Capabilities: caps, Conversion: conversion, ResponsesTools: responsesTools, conversionRank: rank})
	}
	diagnostics["excluded_counts"], diagnostics["excluded"] = counts, excluded
	if len(eligible) == 0 {
		if counts["conversion_loss"] > 0 {
			return nil, &GatewayConversionError{Diagnostics: rejected}
		}
		if counts["capability"] > 0 {
			return nil, ErrGatewayCapability
		}
		return nil, ErrGatewayUnavailable
	}
	channelIDs := make([]int, 0, len(eligible))
	for _, candidate := range eligible {
		channelIDs = append(channelIDs, candidate.Channel.ID)
	}
	health, err := ChannelHealth.Snapshots(c.Request.Context(), channelIDs)
	if err != nil {
		return nil, ErrGatewayUnavailable
	}
	slices.SortStableFunc(eligible, func(a, b *GatewaySelection) int {
		if order := cmp.Compare(a.conversionRank, b.conversionRank); order != 0 {
			return order
		}
		left, right := health[a.Channel.ID], health[b.Channel.ID]
		if order := cmp.Compare(right.SuccessRate, left.SuccessRate); order != 0 {
			return order
		}
		return cmp.Compare(left.FirstByteMillis, right.FirstByteMillis)
	})
	for i, candidate := range eligible {
		channelIDs[i] = candidate.Channel.ID
	}
	var admission channelhealth.Admission
	availableBand := false
	firstBandEnd := 0
	for start := 0; start < len(eligible); {
		end := start + 1
		for end < len(eligible) && eligible[end].conversionRank == eligible[start].conversionRank {
			end++
		}
		if start == 0 {
			firstBandEnd = end
		}
		available := false
		for _, candidate := range eligible[start:end] {
			snapshot := health[candidate.Channel.ID]
			available = available || snapshot.Circuit == "closed" || snapshot.RetryAt <= clock.Now().UnixMilli()
		}
		if available {
			availableBand = true
			admission, err = ChannelHealth.Acquire(c.Request.Context(), channelIDs[start:end])
			if err != nil {
				return nil, ErrGatewayUnavailable
			}
			if admission.ChannelID != 0 {
				break
			}
		}
		start = end
	}
	if admission.ChannelID == 0 && !availableBand {
		admission, err = ChannelHealth.Acquire(c.Request.Context(), channelIDs[:firstBandEnd])
	}
	if err != nil || admission.ChannelID == 0 {
		return nil, ErrGatewayUnavailable
	}
	var selected *GatewaySelection
	for _, candidate := range eligible {
		if candidate.Channel.ID == admission.ChannelID {
			selected = candidate
			continue
		}
		if health[candidate.Channel.ID].Circuit != "closed" {
			counts["circuit_open"]++
		}
	}
	selected.Admission = admission
	diagnostics["health"] = health[selected.Channel.ID]
	diagnostics["probe"] = admission.Lease != ""
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
	caching := request.PromptCacheKey != "" || len(request.PromptCacheOptions) > 0 || len(request.PromptCacheRetention) > 0
	textInput := request.RequiresTextInput || request.Prompt != nil || request.Input != nil || request.Instruction != ""
	for _, message := range request.Messages {
		calls := message.ParseToolCalls()
		tools = tools || len(message.Tools) > 0 || len(calls) > 0 || message.Role == "tool"
		textInput = textInput || message.GetReasoningContent() != "" || len(calls) > 0
		for _, part := range message.ParseContent() {
			caching = caching || len(part.CacheControl) > 0 && string(part.CacheControl) != "null"
			textInput = textInput || part.Type == dto.ContentTypeText && part.Text != ""
		}
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
	if caching && !slices.Contains(caps.Features, capability.Caching) {
		return false
	}
	if textInput && !slices.Contains(caps.InputModalities, capability.Text) {
		return false
	}
	outputs := []capability.Modality{capability.Text}
	if len(request.Modalities) > 0 && common.Unmarshal(request.Modalities, &outputs) != nil {
		return false
	}
	if len(outputs) == 0 {
		outputs = []capability.Modality{capability.Text}
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
