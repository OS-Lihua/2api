package service

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/big"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/capability"
	"github.com/QuantumNous/new-api/pkg/channelhealth"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

type GatewaySelection struct {
	Channel          model.Channel
	Provider         *model.Provider
	Tier             int
	InputTokens      int
	Capabilities     capability.Set
	Diagnostics      map[string]any
	Admission        channelhealth.Admission
	EstimatedCost    money.Value
	EstimatedSale    money.Value
	Score            *big.Rat
	Quality          int
	ConversionLosses []string
	EstimatedTokens  int64
	Permit           *GatewayRoutePermit
	LossFallback     bool
	profitFacts      *GatewayRouteFacts
	profitPeers      []*GatewaySelection
	affinity         providerAffinityMeta
	hasAffinity      bool
}

// GatewayRouteFacts contains transient, validated pricing inputs. Preparation
// must not contact a Provider; bodies and headers never enter diagnostics.
type GatewayRouteFacts struct {
	Usage         *dto.Usage
	SaleInput     billingexpr.RequestInput
	PurchaseInput billingexpr.RequestInput
	// MoneyInput supports non-token quantities through the same exact evaluator.
	MoneyInput       *billingexpr.MoneyInput
	Quality          int
	ConversionLosses []string
}

// GatewayRouteInput lets protocol owners prepare their DTO while sharing all
// status, health, profit, affinity and scoring decisions in this Route.
type GatewayRouteInput struct {
	// Accepted task continuations retain their original Channel and Provider
	// even if the Channel's configuration later points at another Provider.
	PinnedChannelID          int
	PinnedProviderID         int
	Tier                     int
	Protocol                 protocol.Type
	AcceptedConversionLosses []string
	Sale                     *model.PriceVersion
	SaleFX                   *money.Rate
	Ratio                    decimal.Decimal
	StartedAt                time.Time
	Failures                 *GatewayExclusions
	Prepare                  func(*GatewaySelection) (*GatewayRouteFacts, error)
}

// GatewayParameterError retains a native parameter rejection through Route selection.
type GatewayParameterError struct{ Path, Reason string }

func (err *GatewayParameterError) ParameterPath() string { return err.Path }
func (err *GatewayParameterError) Error() string         { return err.Reason }
func (err *GatewayParameterError) Unwrap() error         { return ErrGatewayCapability }

var ErrGatewayCapability = errors.New("no Channel supports the requested capabilities")
var ErrGatewayConversion = errors.New("no Channel supports the requested conversion")
var ErrGatewayUnavailable = errors.New("no Channel is available")
var ErrGatewayPriceUnavailable = errors.New("Channel price is unavailable")

// GatewayRouteRandomSource is a context key for an optional bounded random
// source. Production uses math/rand/v2; HTTP tests can supply exact draws.
const GatewayRouteRandomSource = "gateway_route_random_source"

func SelectGatewayRoute(c *gin.Context, offering *model.ModelOffering, input GatewayRouteInput) (*GatewaySelection, error) {
	if input.Sale == nil || input.Prepare == nil || input.Ratio.IsNegative() {
		return nil, ErrGatewayUnavailable
	}
	purchaseAt := clock.Now()
	if input.StartedAt.IsZero() {
		input.StartedAt = purchaseAt
	}
	ids := []int{}
	tiers := map[int]int{}
	for _, member := range offering.Members {
		if input.Failures != nil && member.Tier < input.Failures.MemberTier {
			continue
		}
		if input.Tier != 0 && member.Tier != input.Tier {
			continue
		}
		ids = append(ids, member.ModelID)
		tiers[member.ModelID] = member.Tier
	}
	var candidates []model.Channel
	db := model.DB.WithContext(c.Request.Context())
	if err := model.OfferingChannelsQuery(db, offering, ids).Preload("Model").Order("id").Find(&candidates).Error; err != nil {
		return nil, err
	}
	if input.PinnedChannelID != 0 || input.PinnedProviderID != 0 {
		if input.PinnedChannelID <= 0 || input.PinnedProviderID <= 0 {
			return nil, ErrGatewayUnavailable
		}
		candidates = slices.DeleteFunc(candidates, func(candidate model.Channel) bool { return candidate.ID != input.PinnedChannelID })
		for i := range candidates {
			candidates[i].ProviderID = input.PinnedProviderID
		}
	}
	providerIDs := make([]int, 0, len(candidates))
	channelIDs := make([]int, 0, len(candidates))
	for _, candidate := range candidates {
		providerIDs = append(providerIDs, candidate.ProviderID)
		channelIDs = append(channelIDs, candidate.ID)
	}
	var providers []model.Provider
	if err := db.Where("id IN ?", providerIDs).Find(&providers).Error; err != nil {
		return nil, err
	}
	current := make(map[int]*model.Provider, len(providers))
	for i := range providers {
		current[providers[i].Id] = &providers[i]
	}
	health, err := ChannelHealth.Snapshots(c.Request.Context(), channelIDs)
	if err != nil {
		return nil, ErrGatewayUnavailable
	}
	counts := map[string]int{}
	tierCounts := map[string]int{}
	excluded := []map[string]any{}
	diagnostics := map[string]any{"method": offering.RouteMethod, "candidate_count": len(candidates), "excluded_counts": counts, "affinity_hit": false, "loss_fallback": false}
	reject := func(id int, reason string) {
		counts[reason]++
		tierCounts[reason]++
		if len(excluded) < 10 {
			excluded = append(excluded, map[string]any{"channel_id": id, "reason": reason})
		}
	}
	tierIDs := []int{}
	for _, tier := range tiers {
		if !slices.Contains(tierIDs, tier) {
			tierIDs = append(tierIDs, tier)
		}
	}
	slices.Sort(tierIDs)
	lastStage := 0
	for _, tier := range tierIDs {
		if input.Failures != nil {
			input.Failures.MemberTier = tier
		}
		lastStage = 0
		clear(tierCounts)
		active := []*GatewaySelection{}
		for _, candidate := range candidates {
			if tiers[candidate.ModelID] != tier {
				continue
			}
			provider := current[candidate.ProviderID]
			if candidate.Status != model.ChannelEnabled || !candidate.MappingConfirmed || candidate.Model.Status != 1 || provider == nil || provider.Status != common.ChannelStatusEnabled || provider.ProviderInfo.IsMultiKey && !gatewayProviderHasKey(provider) {
				reject(candidate.ID, "disabled")
				lastStage = max(lastStage, 1)
				continue
			}
			if input.Failures != nil && (input.Failures.Channels[candidate.ID] || input.Failures.Providers[candidate.ProviderID]) {
				reject(candidate.ID, "already_failed")
				lastStage = max(lastStage, 2)
				continue
			}
			protocolMatch := true
			switch input.Protocol {
			case protocol.OpenAIEmbeddings, protocol.GeminiEmbed:
				protocolMatch = candidate.Protocol == protocol.OpenAIEmbeddings || candidate.Protocol == protocol.GeminiEmbed
			case protocol.Rerank:
				protocolMatch = candidate.Protocol == protocol.Rerank
			}
			if !protocolMatch {
				reject(candidate.ID, "protocol")
				lastStage = max(lastStage, 4)
				continue
			}
			active = append(active, &GatewaySelection{Channel: candidate, Provider: provider, Tier: tier})
		}
		hasClosed := slices.ContainsFunc(active, func(selection *GatewaySelection) bool {
			return health[selection.Channel.ID].Circuit == "closed"
		})
		eligible := []*GatewaySelection{}
		priced := []*GatewaySelection{}
		unprofitable := []*GatewaySelection{}
		eligibleFence, lossFence := []int{}, []int{}
		for _, selection := range active {
			id := selection.Channel.ID
			snapshot := health[id]
			ready := snapshot.Circuit == "closed" || snapshot.Circuit == "open" && snapshot.RetryAt <= clock.Now().UnixMilli()
			// Keep the full all-open set for Manager's atomic lease fence and
			// earliest-probe choice after candidate validation.
			if !ready && hasClosed {
				reject(id, "circuit_open")
				lastStage = max(lastStage, 3)
				continue
			}
			caps, err := selection.Channel.EffectiveCapabilities(selection.Channel.Model.Capabilities)
			if err != nil {
				reject(id, "capability")
				lastStage = max(lastStage, 4)
				continue
			}
			selection.Capabilities = caps
			facts, err := input.Prepare(selection)
			if err != nil {
				reason, stage := "preparation", 6
				if errors.Is(err, ErrGatewayCapability) {
					reason, stage = "capability", 4
				}
				if errors.Is(err, ErrGatewayPriceUnavailable) {
					reason, stage = "price_unavailable", 7
				}
				if errors.Is(err, ErrGatewayConversion) {
					reason, stage = "conversion_loss", 5
				}
				reject(id, reason)
				lastStage = max(lastStage, stage)
				continue
			}
			selection.Quality = facts.Quality
			if input.Protocol != "" {
				selection.Quality = max(1, selection.Quality)
				if selection.Channel.Protocol == input.Protocol {
					selection.Quality = 0
				}
			}
			accepted := input.AcceptedConversionLosses
			if accepted == nil {
				accepted = offering.AcceptedConversionLosses
			}
			blocked := slices.ContainsFunc(facts.ConversionLosses, func(code string) bool { return !slices.Contains(accepted, code) })
			if blocked {
				reject(id, "conversion_loss")
				lastStage = max(lastStage, 5)
				continue
			}
			selection.ConversionLosses = slices.Clone(facts.ConversionLosses)
			selection.EstimatedTokens = int64(selection.InputTokens)
			if facts.Usage != nil {
				selection.EstimatedTokens = int64(facts.Usage.TotalTokens)
			}
			if selection.Provider.BalanceUpdatedTime > 0 && selection.Provider.Balance <= 0 {
				reject(id, "provider_balance")
				lastStage = max(lastStage, 6)
				continue
			}
			purchase, err := model.CurrentPriceVersion(c.Request.Context(), model.DB, model.PriceOwnerChannel, id, purchaseAt.Unix())
			if err != nil {
				reject(id, "price_unavailable")
				lastStage = max(lastStage, 7)
				continue
			}
			var purchaseFX *money.Rate
			if purchase.Currency != model.SettlementCurrency() {
				rate, err := model.GetComparisonFXRate(c.Request.Context(), model.DB, purchase.Currency, model.SettlementCurrency())
				if err != nil {
					reject(id, "price_unavailable")
					lastStage = max(lastStage, 7)
					continue
				}
				purchaseFX = &rate
			}
			var cost, sale money.Value
			if facts.MoneyInput == nil {
				_, cost, _, err = GatewayPrice(purchase, facts.Usage, facts.PurchaseInput, purchaseAt, decimal.NewFromInt(1), purchaseFX)
				if err == nil {
					_, sale, _, err = GatewayPrice(input.Sale, facts.Usage, facts.SaleInput, input.StartedAt, input.Ratio, input.SaleFX)
				}
			} else {
				probe := *facts.MoneyInput
				probe.Request = facts.PurchaseInput
				probe.Time = purchaseAt
				cost, _, err = billingexpr.RunMoneyExpr(purchase.Expression, probe, purchase.Currency)
				if err == nil && purchaseFX != nil {
					cost, err = cost.Convert(*purchaseFX)
				}
				probe.Request = facts.SaleInput
				probe.Time = input.StartedAt
				if err == nil {
					sale, _, err = billingexpr.RunMoneyExpr(input.Sale.Expression, probe, input.Sale.Currency)
				}
				if err == nil {
					sale, err = money.MulUnitPrice(input.Ratio, sale)
				}
				if err == nil && input.SaleFX != nil {
					sale, err = sale.Convert(*input.SaleFX)
				}
			}
			if err != nil || cost.Currency() != model.SettlementCurrency() || sale.Currency() != model.SettlementCurrency() {
				reject(id, "price_unavailable")
				lastStage = max(lastStage, 7)
				continue
			}
			selection.EstimatedCost, selection.EstimatedSale = cost, sale
			selection.profitFacts = facts
			allowed, err := GatewayRouteProfitAllowed(cost, sale, offering.MinimumMarginRate)
			if err != nil {
				reject(id, "price_unavailable")
				lastStage = max(lastStage, 7)
				continue
			}
			// Quota availability limits selection, but a busy candidate can still
			// hold the only probe lease for this valid all-open pool.
			if allowed {
				eligibleFence = append(eligibleFence, id)
			} else {
				lossFence = append(lossFence, id)
			}
			if err := GatewayRouteQuota.Check(c.Request.Context(), selection, selection.EstimatedTokens); err != nil {
				reason := "admission_unavailable"
				if errors.Is(err, ErrGatewayRateLimited) {
					reason = "rate_limited"
				}
				reject(id, reason)
				lastStage = max(lastStage, 6)
				continue
			}
			priced = append(priced, selection)
			if !allowed {
				reject(id, "profit")
				lastStage = max(lastStage, 7)
				unprofitable = append(unprofitable, selection)
				continue
			}
			eligible = append(eligible, selection)
		}
		probeFence := eligibleFence
		if offering.AllowLossFallback {
			probeFence = append(probeFence, lossFence...)
		}
		for phase := range 2 {
			lossFallback := phase == 1
			if lossFallback {
				if !offering.AllowLossFallback {
					break
				}
				eligible = unprofitable
			}
			if len(eligible) == 0 {
				continue
			}
			// Protocol quality orders groups; exhausted groups cannot discard
			// other valid Channels in the same Model tier.
			slices.SortStableFunc(eligible, func(a, b *GatewaySelection) int { return cmp.Compare(a.Quality, b.Quality) })
			for len(eligible) > 0 {
				end := 1
				for end < len(eligible) && eligible[end].Quality == eligible[0].Quality {
					end++
				}
				group := eligible[:end:end]
				eligible = eligible[end:]
				rankMethod := offering.RouteMethod
				if lossFallback {
					// Loss fallback uses purchase cost; health and weight only order equal costs.
					rankMethod = "weight"
				}
				ranked, err := rankGatewayChannels(c, rankMethod, group, health)
				if err != nil {
					return nil, err
				}
				if lossFallback {
					// Keep later candidates for atomic admission failures, in purchase-cost order.
					slices.SortStableFunc(ranked, func(a, b *GatewaySelection) int { order, _ := a.EstimatedCost.Compare(b.EstimatedCost); return order })
				}
				preferred, found := GetPreferredGatewayChannelByAffinity(c, offering.ID, tier, offering.Name)
				if found && !lossFallback {
					for i, selection := range ranked {
						if selection.Channel.ID == preferred {
							ranked = append([]*GatewaySelection{selection}, append(ranked[:i], ranked[i+1:]...)...)
							break
						}
					}
				}
				for len(ranked) > 0 {
					ordered := make([]int, len(ranked))
					for i, selection := range ranked {
						ordered[i] = selection.Channel.ID
					}
					admission, err := ChannelHealth.AcquireWithProbeFence(c.Request.Context(), ordered, probeFence)
					if err != nil {
						return nil, ErrGatewayUnavailable
					}
					if admission.ChannelID == 0 {
						lastStage = max(lastStage, 6)
						unavailable := make(map[int]bool, len(ranked))
						for _, selection := range ranked {
							reject(selection.Channel.ID, "circuit_open")
							unavailable[selection.Channel.ID] = true
						}
						priced = slices.DeleteFunc(priced, func(peer *GatewaySelection) bool { return unavailable[peer.Channel.ID] })
						break
					}
					index := slices.IndexFunc(ranked, func(selection *GatewaySelection) bool { return selection.Channel.ID == admission.ChannelID })
					selected := ranked[index]
					permit, err := GatewayRouteQuota.Acquire(c.Request.Context(), selected, selected.EstimatedTokens)
					if err != nil {
						_ = ChannelHealth.Release(c.Request.Context(), admission)
						reason := "admission_unavailable"
						if errors.Is(err, ErrGatewayRateLimited) {
							reason = "rate_limited"
						}
						reject(selected.Channel.ID, reason)
						lastStage = max(lastStage, 6)
						priced = slices.DeleteFunc(priced, func(peer *GatewaySelection) bool { return peer.Channel.ID == selected.Channel.ID })
						ranked = slices.Delete(ranked, index, index+1)
						continue
					}
					selected.Admission, selected.Permit = admission, permit
					selected.LossFallback, selected.profitPeers = lossFallback, priced
					selected.affinity, selected.hasAffinity = getChannelAffinityMeta(c)
					diagnostics["eligible_count"] = len(ranked)
					diagnostics["excluded"] = excluded
					diagnostics["member_tier"] = tier
					diagnostics["health"] = health[selected.Channel.ID]
					diagnostics["probe"] = admission.Lease != ""
					diagnostics["loss_fallback"] = lossFallback
					diagnostics["affinity_hit"] = found && selected.Channel.ID == preferred && !lossFallback
					diagnostics["estimated_cost"] = selected.EstimatedCost.String()
					diagnostics["estimated_receivable"] = selected.EstimatedSale.String()
					diagnostics["score"] = selected.Score.RatString()
					selected.Diagnostics = diagnostics
					return selected, nil
				}
			}
		}
	}
	if lastStage == 4 {
		return nil, ErrGatewayCapability
	}
	if lastStage == 5 {
		return nil, ErrGatewayConversion
	}
	if lastStage == 7 && tierCounts["price_unavailable"] > 0 && tierCounts["profit"] == 0 {
		return nil, ErrGatewayPriceUnavailable
	}
	return nil, ErrGatewayUnavailable
}

// GatewayResponsesCapabilitiesMatch checks native request features without
// converting stateful Responses fields through another wire protocol.
func GatewayResponsesCapabilitiesMatch(request *dto.OpenAIResponsesRequest, caps capability.Set, inputTokens int, nativeModalities ...bool) bool {
	return GatewayResponsesCapabilitiesFailure(request, caps, inputTokens, nativeModalities...) == nil
}

// NativeItemUsesTools identifies protocol items that require tool capability.
func NativeItemUsesTools(kind string) bool {
	switch kind {
	case "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output",
		"local_shell_call", "local_shell_call_output", "shell_call", "shell_call_output",
		"apply_patch_call", "apply_patch_call_output", "computer_call", "computer_call_output",
		"web_search_call", "file_search_call", "code_interpreter_call", "image_generation_call",
		"mcp_call", "mcp_list_tools", "mcp_approval_request", "mcp_approval_response":
		return true
	}
	return false
}

// GatewayResponsesCapabilitiesFailure identifies the first unsupported native parameter.
func GatewayResponsesCapabilitiesFailure(request *dto.OpenAIResponsesRequest, caps capability.Set, inputTokens int, nativeModalities ...bool) *GatewayParameterError {
	projection := &dto.GeneralOpenAIRequest{Model: request.Model, Stream: common.GetPointer(true), MaxCompletionTokens: request.MaxOutputTokens}
	for _, part := range request.ParseInput() {
		if part.Type == "input_text" && part.Text != "" {
			projection.RequiresTextInput = true
			break
		}
	}
	if instructions := string(request.Instructions); instructions != "" && instructions != "null" && instructions != `""` {
		projection.RequiresTextInput = true
	}
	toolPath := "tools"
	if len(request.Tools) > 0 && string(request.Tools) != "[]" && string(request.Tools) != "null" {
		projection.RequiresToolCalling = true
	} else {
		for i, item := range ojson.GetBytes(request.Input, "@this").Array() {
			if NativeItemUsesTools(item.Get("type").String()) || item.Get("role").String() == "tool" {
				projection.RequiresToolCalling = true
				toolPath = fmt.Sprintf("input[%d].type", i)
				break
			}
		}
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
		return &GatewayParameterError{Path: "text.format", Reason: "Invalid text format"}
	}
	projection.ResponseFormat = text.Format
	if failure := gatewayCapabilitiesFailure(projection, caps, inputTokens, nativeModalities...); failure != nil {
		switch failure.Path {
		case "tools":
			failure.Path = toolPath
		case "reasoning_effort":
			failure.Path = "reasoning.effort"
		case "response_format":
			failure.Path = "text.format"
		case "max_completion_tokens":
			failure.Path = "max_output_tokens"
		case "messages":
			failure.Path = "input"
		}
		return failure
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
			return &GatewayParameterError{Path: "input", Reason: "Input modality is not supported on this channel"}
		}
	}
	if caps.Limits.MaxImages != nil && images > *caps.Limits.MaxImages {
		return &GatewayParameterError{Path: "input", Reason: "Image count limit exceeded"}
	}
	return nil
}

// GatewayComparisonCost applies the current directional quote and its buffer.
// Accounting uses the independently frozen, unbuffered quote.
func GatewayComparisonCost(ctx context.Context, source money.Value) (money.Value, error) {
	if source.Currency() == model.SettlementCurrency() {
		return source, nil
	}
	rate, err := model.GetComparisonFXRate(ctx, model.DB, source.Currency(), model.SettlementCurrency())
	if err != nil {
		return money.Value{}, err
	}
	return source.Convert(rate)
}

// LossFallbackAllowed rechecks the same tier's still-available peers when
// procurement changes during Provider preparation. A used admission retains
// its request and token charges when testing a replacement on the same Provider.
func (selection *GatewaySelection) LossFallbackAllowed(ctx context.Context, cost money.Value, margin decimal.Decimal, used bool) (bool, error) {
	if len(selection.profitPeers) == 0 {
		return false, ErrGatewayUnavailable
	}
	ids, providerIDs := []int{}, []int{}
	for _, peer := range selection.profitPeers {
		if peer.Channel.ID != selection.Channel.ID {
			ids = append(ids, peer.Channel.ID)
			providerIDs = append(providerIDs, peer.Provider.Id)
		}
	}
	if len(ids) == 0 {
		return true, nil
	}
	db := model.DB.WithContext(ctx)
	var channels []model.Channel
	if err := db.Where("id IN ?", ids).Preload("Model").Find(&channels).Error; err != nil {
		return false, err
	}
	var providers []model.Provider
	if err := db.Where("id IN ?", providerIDs).Find(&providers).Error; err != nil {
		return false, err
	}
	currentChannels := make(map[int]model.Channel, len(channels))
	for _, channel := range channels {
		currentChannels[channel.ID] = channel
	}
	currentProviders := make(map[int]*model.Provider, len(providers))
	for i := range providers {
		currentProviders[providers[i].Id] = &providers[i]
	}
	at := clock.Now()
	eligible := []*GatewaySelection{}
	for _, peer := range selection.profitPeers {
		if peer.Channel.ID == selection.Channel.ID {
			continue
		}
		channel, exists := currentChannels[peer.Channel.ID]
		provider := currentProviders[peer.Provider.Id]
		if !exists || channel.ProviderID != peer.Provider.Id || channel.ModelID != peer.Channel.ModelID || channel.Protocol != peer.Channel.Protocol || channel.UpstreamModelName != peer.Channel.UpstreamModelName {
			continue
		}
		if channel.Status != model.ChannelEnabled || !channel.MappingConfirmed || channel.Model.Status != 1 {
			continue
		}
		if provider == nil || provider.Status != common.ChannelStatusEnabled || provider.ProviderInfo.IsMultiKey && !gatewayProviderHasKey(provider) || provider.BalanceUpdatedTime > 0 && provider.Balance <= 0 {
			continue
		}
		price, err := model.CurrentPriceVersion(ctx, model.DB, model.PriceOwnerChannel, peer.Channel.ID, at.Unix())
		if errors.Is(err, gorm.ErrRecordNotFound) {
			continue
		}
		if err != nil {
			return false, err
		}
		facts := peer.profitFacts
		var source money.Value
		if facts.MoneyInput == nil {
			source, _, _, err = GatewayPrice(price, facts.Usage, facts.PurchaseInput, at, decimal.NewFromInt(1), nil)
		} else {
			probe := *facts.MoneyInput
			probe.Request, probe.Time = facts.PurchaseInput, at
			source, _, err = billingexpr.RunMoneyExpr(price.Expression, probe, price.Currency)
		}
		if err != nil {
			continue
		}
		comparison, err := GatewayComparisonCost(ctx, source)
		if err != nil {
			continue
		}
		live := *peer
		live.Channel, live.Provider, live.EstimatedCost = channel, provider, comparison
		eligible = append(eligible, &live)
	}
	ids = ids[:0]
	for _, peer := range eligible {
		ids = append(ids, peer.Channel.ID)
	}
	health, err := ChannelHealth.Snapshots(ctx, ids)
	if err != nil {
		return false, ErrGatewayUnavailable
	}
	hasClosed := slices.ContainsFunc(eligible, func(peer *GatewaySelection) bool { return health[peer.Channel.ID].Circuit == "closed" })
	eligible = slices.DeleteFunc(eligible, func(peer *GatewaySelection) bool {
		snapshot := health[peer.Channel.ID]
		ready := snapshot.Circuit == "closed" || snapshot.Circuit == "open" && snapshot.RetryAt <= clock.Now().UnixMilli()
		return !ready && hasClosed
	})
	ids = ids[:0]
	for _, peer := range eligible {
		ids = append(ids, peer.Channel.ID)
	}
	for _, peer := range eligible {
		if err := selection.Permit.CheckAlternative(ctx, peer, peer.EstimatedTokens, used); err != nil {
			if errors.Is(err, ErrGatewayRateLimited) {
				continue
			}
			return false, err
		}
		admitted, err := ChannelHealth.CheckWithProbeFence(ctx, []int{peer.Channel.ID}, ids)
		if err != nil {
			return false, ErrGatewayUnavailable
		}
		if admitted == 0 {
			continue
		}
		allowed, err := GatewayRouteProfitAllowed(peer.EstimatedCost, peer.EstimatedSale, margin)
		if err != nil {
			continue
		}
		order, err := peer.EstimatedCost.Compare(cost)
		if err != nil {
			return false, err
		}
		if allowed || order < 0 {
			return false, nil
		}
	}
	return true, nil
}

// GatewayRouteProfitAllowed compares exact amounts without dividing by sales.
// A free sale accepts only a free purchase, including at a positive margin floor.
func GatewayRouteProfitAllowed(cost, sale money.Value, margin decimal.Decimal) (bool, error) {
	if cost.Currency() != sale.Currency() {
		return false, money.ErrCurrencyMismatch
	}
	if _, err := money.ParseCurrency(string(cost.Currency())); err != nil {
		return false, err
	}
	zero := money.Amount{Currency: cost.Currency()}.Value()
	costSign, _ := cost.Compare(zero)
	saleSign, _ := sale.Compare(zero)
	if costSign < 0 || saleSign < 0 || margin.IsNegative() || margin.GreaterThan(decimal.NewFromInt(1)) {
		return false, errors.New("invalid Route profit estimate")
	}
	ceiling, err := money.MulUnitPrice(decimal.NewFromInt(1).Sub(margin), sale)
	if err != nil {
		return false, err
	}
	order, err := cost.Compare(ceiling)
	return order <= 0, err
}

func gatewayProviderHasKey(provider *model.Provider) bool {
	for index := range provider.GetKeys() {
		if status, exists := provider.ProviderInfo.MultiKeyStatusList[index]; !exists || status == common.ChannelStatusEnabled {
			return true
		}
	}
	return false
}

func rankGatewayChannels(c *gin.Context, method string, candidates []*GatewaySelection, health map[int]channelhealth.Snapshot) ([]*GatewaySelection, error) {
	for _, candidate := range candidates {
		snapshot := health[candidate.Channel.ID]
		// Health is dimensionless. Currency values remain exact throughout.
		success := decimal.NewFromFloat(snapshot.SuccessRate)
		penalty := decimal.NewFromInt(2).Sub(success).Mul(decimal.NewFromInt(1).Add(decimal.New(snapshot.FirstByteMillis, -5)))
		score := new(big.Rat).SetInt64(candidate.Channel.Weight)
		if candidate.Channel.Weight == 0 {
			score.SetInt64(0)
		}
		candidate.Score = score.Quo(score, penalty.Rat())
	}
	if method == "latency" {
		for _, candidate := range candidates {
			snapshot := health[candidate.Channel.ID]
			latency := int64(1000)
			if snapshot.FirstByteSamples > 0 {
				latency = max(snapshot.FirstByteMillis, 1)
			}
			penalty := new(big.Rat).Mul(big.NewRat(latency, 1), decimal.NewFromInt(2).Sub(decimal.NewFromFloat(snapshot.SuccessRate)).Rat())
			candidate.Score = new(big.Rat).Inv(penalty)
		}
		slices.SortStableFunc(candidates, func(a, b *GatewaySelection) int { return b.Score.Cmp(a.Score) })
		return candidates, nil
	}
	if method == "cost" || method == "" {
		adjusted := map[int]money.Value{}
		for _, candidate := range candidates {
			snapshot := health[candidate.Channel.ID]
			penalty := decimal.NewFromInt(2).Sub(decimal.NewFromFloat(snapshot.SuccessRate)).Mul(decimal.NewFromInt(1).Add(decimal.New(snapshot.FirstByteMillis, -5)))
			value, err := money.MulUnitPrice(penalty, candidate.EstimatedCost)
			if err != nil {
				return nil, ErrGatewayUnavailable
			}
			adjusted[candidate.Channel.ID] = value
		}
		slices.SortStableFunc(candidates, func(a, b *GatewaySelection) int {
			order, _ := adjusted[a.Channel.ID].Compare(adjusted[b.Channel.ID])
			return order
		})
		ceiling, err := money.MulUnitPrice(decimal.New(105, -2), adjusted[candidates[0].Channel.ID])
		if err != nil {
			return nil, ErrGatewayUnavailable
		}
		end := 1
		for end < len(candidates) {
			order, _ := adjusted[candidates[end].Channel.ID].Compare(ceiling)
			if order > 0 {
				break
			}
			end++
		}
		front := gatewayWeightedChoice(c, candidates[:end])
		candidates[0], candidates[front] = candidates[front], candidates[0]
		return candidates, nil
	}
	front := gatewayWeightedChoice(c, candidates)
	candidates[0], candidates[front] = candidates[front], candidates[0]
	return candidates, nil
}

func gatewayWeightedChoice(c *gin.Context, candidates []*GatewaySelection) int {
	total := new(big.Rat)
	for _, candidate := range candidates {
		total.Add(total, candidate.Score)
	}
	if total.Sign() == 0 {
		return 0
	}
	random := rand.Uint64N
	if source, ok := c.Get(GatewayRouteRandomSource); ok {
		if injected, valid := source.(func(uint64) uint64); valid {
			random = injected
		}
	}
	const resolution = uint64(1) << 53
	draw := new(big.Rat).Mul(total, big.NewRat(int64(random(resolution)%resolution), int64(resolution)))
	cumulative := new(big.Rat)
	for i, candidate := range candidates {
		cumulative.Add(cumulative, candidate.Score)
		if draw.Cmp(cumulative) < 0 {
			return i
		}
	}
	return len(candidates) - 1
}

// GatewayCapabilitiesMatch checks original and effective protocol facts.
func GatewayCapabilitiesMatch(request *dto.GeneralOpenAIRequest, caps capability.Set, inputTokens int, nativeModalities ...bool) bool {
	return gatewayCapabilitiesFailure(request, caps, inputTokens, nativeModalities...) == nil
}

func gatewayCapabilitiesFailure(request *dto.GeneralOpenAIRequest, caps capability.Set, inputTokens int, nativeModalities ...bool) *GatewayParameterError {
	if request.Stream != nil && *request.Stream && !slices.Contains(caps.Features, capability.Streaming) {
		return &GatewayParameterError{Path: "stream", Reason: "Streaming is not supported on this channel"}
	}
	tools := request.RequiresToolCalling || len(request.Tools) > 0 || len(request.Functions) > 0
	for _, message := range request.Messages {
		tools = tools || len(message.Tools) > 0 || len(message.ParseToolCalls()) > 0 || message.Role == "tool"
	}
	if tools && !slices.Contains(caps.Features, capability.ToolCalling) {
		return &GatewayParameterError{Path: "tools", Reason: "Tool calling is not supported on this channel"}
	}
	if (request.ReasoningEffort != "" && request.ReasoningEffort != "none" || len(request.Reasoning) > 0) && !slices.Contains(caps.Features, capability.Reasoning) {
		return &GatewayParameterError{Path: "reasoning_effort", Reason: "Reasoning is not supported on this channel"}
	}
	if request.ResponseFormat != nil && request.ResponseFormat.Type != "" && request.ResponseFormat.Type != "text" && !slices.Contains(caps.Features, capability.StructuredOutput) {
		return &GatewayParameterError{Path: "response_format", Reason: "Structured output is not supported on this channel"}
	}
	for messageIndex, message := range request.Messages {
		for contentIndex, part := range message.ParseContent() {
			if len(part.CacheControl) > 0 && string(part.CacheControl) != "null" && !slices.Contains(caps.Features, capability.Caching) {
				return &GatewayParameterError{Path: fmt.Sprintf("messages[%d].content[%d].cache_control", messageIndex, contentIndex), Reason: "Prompt caching is not supported on this channel"}
			}
		}
	}
	if (request.PromptCacheKey != "" || len(request.PromptCacheOptions) > 0 || len(request.PromptCacheRetention) > 0) && !slices.Contains(caps.Features, capability.Caching) {
		path := "prompt_cache_key"
		if len(request.PromptCacheOptions) > 0 {
			path = "prompt_cache_options"
		}
		if len(request.PromptCacheRetention) > 0 {
			path = "prompt_cache_retention"
		}
		return &GatewayParameterError{Path: path, Reason: "Prompt caching is not supported on this channel"}
	}
	requiresText := request.RequiresTextInput || request.Prompt != nil || request.Input != nil
	for _, message := range request.Messages {
		if message.StringContent() != "" || message.GetReasoningContent() != "" || len(message.ParseToolCalls()) > 0 {
			requiresText = true
			break
		}
	}
	if requiresText && !slices.Contains(caps.InputModalities, capability.Text) {
		return &GatewayParameterError{Path: "messages", Reason: "Input or output text is not supported on this channel"}
	}
	var outputs []capability.Modality
	if len(request.Modalities) > 0 && common.Unmarshal(request.Modalities, &outputs) != nil {
		return &GatewayParameterError{Path: "modalities", Reason: "Invalid output modalities"}
	}
	if len(outputs) == 0 && (len(nativeModalities) == 0 || !nativeModalities[0]) {
		outputs = []capability.Modality{capability.Text}
	}
	for _, output := range outputs {
		if !slices.Contains(caps.OutputModalities, output) {
			return &GatewayParameterError{Path: "modalities", Reason: "Output modality is not supported on this channel"}
		}
	}
	images := int64(0)
	for _, file := range request.GetTokenCountMeta().Files {
		modality := capability.PDF
		if file.Source != nil && request.NativeTextReferences[file.Source.GetRawData()] {
			modality = capability.Text
		}
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
			return &GatewayParameterError{Path: "messages", Reason: "Input modality is not supported on this channel"}
		}
	}
	output := int64(request.GetMaxTokens())
	if caps.Limits.MaxOutputTokens != nil && output > *caps.Limits.MaxOutputTokens {
		return &GatewayParameterError{Path: "max_completion_tokens", Reason: "Output token limit exceeded"}
	}
	if caps.Limits.ContextLength != nil && int64(inputTokens)+output > *caps.Limits.ContextLength {
		return &GatewayParameterError{Path: "messages", Reason: "Context token limit exceeded"}
	}
	if caps.Limits.MaxImages != nil && images > *caps.Limits.MaxImages {
		return &GatewayParameterError{Path: "messages", Reason: "Image count limit exceeded"}
	}
	return nil
}
