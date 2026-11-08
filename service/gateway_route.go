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
	"github.com/QuantumNous/new-api/pkg/protocol"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

type GatewaySelection struct {
	Channel      model.Channel
	Provider     *model.Provider
	InputTokens  int
	Capabilities capability.Set
	Diagnostics  map[string]any
	Admission    channelhealth.Admission
}

var ErrGatewayCapability = errors.New("no Channel supports the requested capabilities")
var ErrGatewayUnavailable = errors.New("no Channel is available")

// SelectGatewayChannel applies scope and capability filters before runtime
// health ordering and atomically claims any selected half-open probe.
type GatewayChannelRequirements struct {
	Protocol           protocol.Type
	ChannelID          int
	Responses          *dto.OpenAIResponsesRequest
	ResponsesWebSocket bool
	InputTokens        *int
}

func SelectGatewayChannel(c *gin.Context, offering *model.ModelOffering, request *dto.GeneralOpenAIRequest, failures ...*GatewayExclusions) (*GatewaySelection, error) {
	return SelectGatewayProtocolChannel(c, offering, request, GatewayChannelRequirements{Protocol: protocol.OpenAIChat}, failures...)
}

// SelectGatewayProtocolChannel uses the shared candidate and health admission
// path for a native transport. ChannelID constrains an established connection.
func SelectGatewayProtocolChannel(c *gin.Context, offering *model.ModelOffering, request *dto.GeneralOpenAIRequest, requirements GatewayChannelRequirements, failures ...*GatewayExclusions) (*GatewaySelection, error) {
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
	eligible := []*GatewaySelection{}
	for _, candidate := range candidates {
		if len(failures) > 0 && (failures[0].Channels[candidate.ID] || failures[0].Providers[candidate.ProviderID]) {
			counts["already_failed"]++
			continue
		}
		reason := ""
		var provider *model.Provider
		var caps capability.Set
		var tokens int
		switch {
		case candidate.Status != model.ChannelEnabled || !candidate.MappingConfirmed || candidate.Model.Status != 1:
			reason = "disabled"
		case requirements.ChannelID != 0 && candidate.ID != requirements.ChannelID:
			reason = "connection_locked"
		case candidate.Protocol != requirements.Protocol:
			reason = "protocol"
		default:
			var err error
			provider, err = model.CacheGetProvider(candidate.ProviderID)
			if err != nil || provider == nil || provider.Status != common.ChannelStatusEnabled {
				reason = "disabled"
				break
			}
			if requirements.ResponsesWebSocket && !provider.GetSetting().ResponsesWebSocketEnabled {
				reason = "protocol"
				break
			}
			caps, err = candidate.EffectiveCapabilities(candidate.Model.Capabilities)
			if err != nil || requirements.Responses == nil && !GatewayCapabilitiesMatch(request, caps, 0) || requirements.Responses != nil && !GatewayResponsesCapabilitiesMatch(requirements.Responses, caps, 0) {
				reason = "capability"
				break
			}
			common.SetContextKey(c, constant.ContextKeyOriginalModel, candidate.Model.ModelName)
			info := relaycommon.GenRelayInfoOpenAI(c, request)
			info.TokenizerModel = candidate.Model.ModelName
			meta := request.GetTokenCountMeta()
			if requirements.Responses != nil {
				meta = requirements.Responses.GetTokenCountMeta()
			}
			if requirements.InputTokens != nil {
				tokens = *requirements.InputTokens
			} else {
				tokens, err = CountRequestToken(c, meta, info)
			}
			if err != nil {
				return nil, errors.New("could not estimate input tokens")
			}
			if requirements.Responses == nil && !GatewayCapabilitiesMatch(request, caps, tokens) || requirements.Responses != nil && !GatewayResponsesCapabilitiesMatch(requirements.Responses, caps, tokens) {
				reason = "capability"
				break
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
		eligible = append(eligible, &GatewaySelection{Channel: candidate, Provider: provider, InputTokens: tokens, Capabilities: caps})
	}
	diagnostics["excluded_counts"], diagnostics["excluded"] = counts, excluded
	if len(eligible) == 0 {
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
		left, right := health[a.Channel.ID], health[b.Channel.ID]
		if order := cmp.Compare(right.SuccessRate, left.SuccessRate); order != 0 {
			return order
		}
		return cmp.Compare(left.FirstByteMillis, right.FirstByteMillis)
	})
	for i, candidate := range eligible {
		channelIDs[i] = candidate.Channel.ID
	}
	admission, err := ChannelHealth.Acquire(c.Request.Context(), channelIDs)
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

// GatewayResponsesCapabilitiesMatch checks native request features without
// converting stateful Responses fields through another wire protocol.
func GatewayResponsesCapabilitiesMatch(request *dto.OpenAIResponsesRequest, caps capability.Set, inputTokens int) bool {
	projection := &dto.GeneralOpenAIRequest{Model: request.Model, Stream: common.GetPointer(true), MaxCompletionTokens: request.MaxOutputTokens}
	if len(request.Tools) > 0 && string(request.Tools) != "[]" && string(request.Tools) != "null" {
		projection.Tools = make([]dto.ToolCallRequest, 1)
	}
	if request.Reasoning != nil {
		projection.ReasoningEffort = request.Reasoning.Effort
		if projection.ReasoningEffort == "" {
			projection.ReasoningEffort = "medium"
		}
	}
	if len(request.PromptCacheKey) > 0 {
		projection.PromptCacheKey = string(request.PromptCacheKey)
	}
	projection.PromptCacheOptions, projection.PromptCacheRetention = request.PromptCacheOptions, request.PromptCacheRetention
	var text struct {
		Format *dto.ResponseFormat `json:"format"`
	}
	if len(request.Text) > 0 && common.Unmarshal(request.Text, &text) != nil {
		return false
	}
	projection.ResponseFormat = text.Format
	if !GatewayCapabilitiesMatch(projection, caps, inputTokens) {
		return false
	}
	var images int64
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
	return caps.Limits.MaxImages == nil || images <= *caps.Limits.MaxImages
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
