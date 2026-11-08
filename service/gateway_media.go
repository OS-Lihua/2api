package service

import (
	"cmp"
	"slices"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/capability"
	"github.com/QuantumNous/new-api/pkg/protocol"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

// SelectGatewayMediaChannel preserves Offering scope and member tiers while
// requiring the exact synchronous media protocol. Request-local exclusions are
// owned by the shared attempt runner.
func SelectGatewayMediaChannel(c *gin.Context, offering *model.ModelOffering, kind protocol.Type, request dto.Request, excludedChannels, excludedProviders map[int]bool) (*GatewaySelection, error) {
	ids := make([]int, 0, len(offering.Members))
	tiers := make(map[int]int, len(offering.Members))
	for _, member := range offering.Members {
		ids = append(ids, member.ModelID)
		tiers[member.ModelID] = member.Tier
	}
	var candidates []model.Channel
	if err := model.OfferingChannelsQuery(model.DB.WithContext(c.Request.Context()), offering, ids).Preload("Model").Order("id").Find(&candidates).Error; err != nil {
		return nil, err
	}
	slices.SortFunc(candidates, func(a, b model.Channel) int {
		if order := cmp.Compare(tiers[a.ModelID], tiers[b.ModelID]); order != 0 {
			return order
		}
		return cmp.Compare(a.ID, b.ID)
	})
	diagnostics := map[string]any{"method": "single_channel", "candidate_count": len(candidates), "eligible_count": 0}
	counts := map[string]int{}
	excluded := []map[string]any{}
	var eligible []*GatewaySelection
	for _, candidate := range candidates {
		reason := ""
		var provider *model.Provider
		var caps capability.Set
		switch {
		case candidate.Status != model.ChannelEnabled || !candidate.MappingConfirmed || candidate.Model.Status != 1:
			reason = "disabled"
		case candidate.Protocol != kind:
			reason = "protocol"
		case excludedChannels[candidate.ID] || excludedProviders[candidate.ProviderID]:
			reason = "request_failed"
		default:
			var err error
			provider, err = model.CacheGetProvider(candidate.ProviderID)
			if err != nil || provider == nil || provider.Status != common.ChannelStatusEnabled {
				reason = "disabled"
				break
			}
			caps, err = candidate.EffectiveCapabilities(candidate.Model.Capabilities)
			if err != nil || !GatewayMediaCapabilitiesMatch(c, kind, request, caps, 0, nil) {
				reason = "capability"
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
		eligible = append(eligible, &GatewaySelection{Channel: candidate, Provider: provider, Capabilities: caps})
	}
	diagnostics["excluded_counts"], diagnostics["excluded"] = counts, excluded
	if len(eligible) == 0 {
		if counts["capability"] > 0 {
			return nil, ErrGatewayCapability
		}
		return nil, ErrGatewayUnavailable
	}
	// Keep member tiers distinct; health admission owns the probe lease.
	for len(eligible) > 0 {
		tier := tiers[eligible[0].Channel.ModelID]
		end := 0
		for end < len(eligible) && tiers[eligible[end].Channel.ModelID] == tier {
			end++
		}
		group := eligible[:end]
		channelIDs := make([]int, len(group))
		for i, candidate := range group {
			channelIDs[i] = candidate.Channel.ID
		}
		health, err := ChannelHealth.Snapshots(c.Request.Context(), channelIDs)
		if err != nil {
			return nil, ErrGatewayUnavailable
		}
		slices.SortStableFunc(group, func(a, b *GatewaySelection) int {
			left, right := health[a.Channel.ID], health[b.Channel.ID]
			if order := cmp.Compare(right.SuccessRate, left.SuccessRate); order != 0 {
				return order
			}
			return cmp.Compare(left.FirstByteMillis, right.FirstByteMillis)
		})
		for i, candidate := range group {
			channelIDs[i] = candidate.Channel.ID
		}
		admission, err := ChannelHealth.Acquire(c.Request.Context(), channelIDs)
		if err != nil {
			return nil, ErrGatewayUnavailable
		}
		for _, candidate := range group {
			if candidate.Channel.ID == admission.ChannelID {
				candidate.Admission = admission
				diagnostics["health"] = health[candidate.Channel.ID]
				diagnostics["probe"] = admission.Lease != ""
				candidate.Diagnostics = diagnostics
				return candidate, nil
			}
		}
		eligible = eligible[end:]
	}
	return nil, ErrGatewayUnavailable
}

// GatewayMediaCapabilitiesMatch is applied to both original and effective DTOs.
// Uploaded audio duration is measured before reservation; no request duration or
// output ceiling is accepted as actual usage.
func GatewayMediaCapabilitiesMatch(c *gin.Context, kind protocol.Type, request dto.Request, caps capability.Set, inputTokens int, facts *relaycommon.ResponseFacts) bool {
	if request.IsStream(c.Request) && !slices.Contains(caps.Features, capability.Streaming) {
		return false
	}
	input, output := capability.Text, capability.Image
	switch kind {
	case protocol.OpenAIImages:
		image, ok := request.(*dto.ImageRequest)
		if !ok {
			return false
		}
		count, err := image.ImageCount(false)
		if err != nil || caps.Limits.MaxImages != nil && int64(count) > *caps.Limits.MaxImages {
			return false
		}
		images := 0
		if form := c.Request.MultipartForm; form != nil {
			for field, files := range form.File {
				if field == "image" || strings.HasPrefix(field, "image[") {
					images += len(files)
				}
			}
		}
		for _, raw := range []common.RawMessage{image.Image, image.Images} {
			if len(raw) == 0 {
				continue
			}
			value := ojson.ParseBytes(raw)
			if value.IsArray() {
				images += len(value.Array())
			} else if value.Exists() {
				images++
			}
		}
		if images > dto.MaxImageN || images > 0 && !slices.Contains(caps.InputModalities, capability.Image) || caps.Limits.MaxImages != nil && int64(images) > *caps.Limits.MaxImages {
			return false
		}
	case protocol.OpenAIAudioSpeech:
		output = capability.Audio
	case protocol.OpenAIAudioTranscription:
		input, output = capability.Audio, capability.Text
	default:
		return false
	}
	if !slices.Contains(caps.InputModalities, input) || !slices.Contains(caps.OutputModalities, output) {
		return false
	}
	if caps.Limits.ContextLength != nil && int64(inputTokens) > *caps.Limits.ContextLength {
		return false
	}
	if facts != nil && facts.AudioInputSeconds != nil && caps.Limits.MaxDurationSeconds != nil {
		seconds, err := decimal.NewFromString(*facts.AudioInputSeconds)
		if err != nil || seconds.IsNegative() || seconds.GreaterThan(decimal.NewFromInt(*caps.Limits.MaxDurationSeconds)) {
			return false
		}
	}
	return true
}
