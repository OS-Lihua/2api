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
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

type GatewaySelection struct {
	Channel      model.Channel
	Provider     *model.Provider
	InputTokens  int
	Capabilities capability.Set
	Diagnostics  map[string]any
}

var ErrGatewayCapability = errors.New("no Channel supports the requested capabilities")
var ErrGatewayUnavailable = errors.New("no Channel is available")

// SelectGatewayChannel considers only the current member tier and the exact
// OpenAI Chat protocol. Ordering by Channel ID makes this single-attempt policy
// deterministic; later route policies can replace selection without replacing
// the shared scope, capability, transport or accounting boundaries.
func SelectGatewayChannel(c *gin.Context, offering *model.ModelOffering, request *dto.GeneralOpenAIRequest, failures ...*GatewayExclusions) (*GatewaySelection, error) {
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
	var selected *GatewaySelection
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
		case candidate.Protocol != protocol.OpenAIChat:
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
		}
		if reason != "" {
			counts[reason]++
			if len(excluded) < 10 {
				excluded = append(excluded, map[string]any{"channel_id": candidate.ID, "reason": reason})
			}
			continue
		}
		diagnostics["eligible_count"] = diagnostics["eligible_count"].(int) + 1
		if selected == nil {
			selected = &GatewaySelection{Channel: candidate, Provider: provider, InputTokens: tokens, Capabilities: caps}
		}
	}
	diagnostics["excluded_counts"], diagnostics["excluded"] = counts, excluded
	if selected == nil {
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
