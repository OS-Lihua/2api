package controller

import (
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/pkg/protocol"
	taskplugin "github.com/QuantumNous/new-api/relay/channel/task/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// TaskGateway submits a resolved ModelOffering through the shared attempt and
// money lifecycle. Accepted tasks retain the selected Channel for all polling.
func TaskGateway(c *gin.Context) {
	if middleware.TextOffering(c) == nil || !strings.HasPrefix(c.Request.URL.Path, "/v1/tasks/") && !c.GetBool("task_gateway_endpoint") {
		c.Next()
		return
	}
	c.Abort()
	if _, exists := c.Get(jsplugin.ContextKeyPinnedEndpoint); exists {
		RelayTaskPluginEndpoint(c, presentGatewayTaskSubmission)
		return
	}
	presentGatewayTaskSubmission(c)
}

func presentGatewayTaskSubmission(c *gin.Context) {
	outcome, taskErr := submitGatewayTask(c)
	if taskErr != nil {
		respondTaskSubmissionError(c, taskErr)
		return
	}
	if c.GetBool("task_gateway_video") {
		c.JSON(http.StatusOK, outcome.Task.ToOpenAIVideo())
		return
	}
	presentTaskSubmission(c, outcome)
}

// submitGatewayTask shares the durable submission boundary with native and
// host-protocol presenters. It performs no downstream response writes.
func submitGatewayTask(c *gin.Context) (*taskSubmissionOutcome, *dto.TaskError) {
	offering := middleware.TextOffering(c)
	group := common.GetContextKeyString(c, constant.ContextKeyUserGroup)
	if !offering.Enabled || offering.Visibility == "groups" && !slices.Contains(offering.Groups, group) {
		return nil, &dto.TaskError{StatusCode: 404, Code: "model_not_found", Message: "Model not found", LocalError: true}
	}
	pinValue, _ := c.Get(jsplugin.ContextKeyPinnedPlugin)
	pin, ok := pinValue.(jsplugin.PinnedPlugin)
	if !ok || pin.Plugin == nil {
		return nil, &dto.TaskError{StatusCode: 400, Code: "invalid_request", Message: "Task plugin is unavailable", LocalError: true}
	}
	origins, _ := common.GetContextKeyType[[]*model.Task](c, constant.ContextKeyOriginTasks)
	pinnedChannelID, pinnedProviderID := 0, 0
	for _, origin := range origins {
		if origin.PrivateData.Gateway == nil || !origin.PrivateData.Gateway.Accepted || origin.UserId != c.GetInt("id") || origin.Platform != constant.TaskPlatform(pin.Plugin.Meta.Key) {
			return nil, &dto.TaskError{StatusCode: 400, Code: "origin_task_not_found", Message: "Origin task is unavailable", LocalError: true}
		}
		if pinnedChannelID != 0 && (origin.PrivateData.Gateway.ChannelID != pinnedChannelID || origin.PrivateData.Gateway.ProviderID != pinnedProviderID) {
			return nil, &dto.TaskError{StatusCode: 400, Code: "origin_task_channel_conflict", Message: "Origin tasks must belong to the same Channel", LocalError: true}
		}
		pinnedChannelID = origin.PrivateData.Gateway.ChannelID
		pinnedProviderID = origin.PrivateData.Gateway.ProviderID
	}
	started := c.GetTime(middleware.TextGatewayStartedAtKey)
	sale, err := model.CurrentPriceVersion(c.Request.Context(), model.DB, model.PriceOwnerOffering, offering.ID, started.Unix())
	if err != nil {
		return nil, &dto.TaskError{StatusCode: 503, Code: "price_unavailable", Message: "Model price is unavailable", LocalError: true}
	}
	saleFX, saleRate, err := service.GatewayFX(c.Request.Context(), sale.Currency)
	if err != nil {
		return nil, &dto.TaskError{StatusCode: 503, Code: "price_unavailable", Message: "Model price is unavailable", LocalError: true}
	}
	ratio, err := service.ExactGatewayGroupRatio(group)
	if err != nil {
		return nil, &dto.TaskError{StatusCode: 503, Code: "price_unavailable", Message: "Model price is unavailable", LocalError: true}
	}
	requestID := c.GetString(common.RequestIdKey)
	if requestID == "" {
		requestID = common.NewRequestId()
	}
	requested := c.GetString("resolved_task_model")
	if c.GetBool("token_model_limit_enabled") {
		limits, _ := c.Get("token_model_limit")
		allowed, _ := limits.(map[string]bool)
		if !allowed[requested] && !allowed[offering.Name] {
			return nil, &dto.TaskError{StatusCode: http.StatusNotFound, Code: "model_not_found", Message: "Model not found", LocalError: true}
		}
	}
	var record *model.GatewayRequest
	var saleSource money.Amount
	var accepted *model.Task
	var acceptedInfo *relaycommon.RelayInfo
	var pending *model.Task
	var previousAttempts []model.GatewayAttempt
	publicTaskID := model.GenerateTaskID()
	upstreamAccepted := false
	var lastStatus = http.StatusServiceUnavailable
	var lastCode = "channel_unavailable"
	parent := c.Request
	attempts := service.RunGatewayAttempts(parent.Context(), service.GatewayPolicy(), func(budget *service.GatewayAttemptBudget, exclusions *service.GatewayExclusions) (result service.GatewayAttemptResult) {
		defer func() {
			if result.Attempt != nil {
				previousAttempts = append(previousAttempts, *result.Attempt)
			}
		}()
		var candidates []model.Channel
		ids := make([]int, 0, len(offering.Members))
		for _, member := range offering.Members {
			ids = append(ids, member.ModelID)
		}
		query := model.OfferingChannelsQuery(model.DB, offering, ids).Preload("Provider").Preload("Model").Where("channels.status = ? AND channels.protocol = ?", model.ChannelEnabled, protocol.Type("task:"+pin.Plugin.Meta.Key)).Order("channels.id")
		if err := query.Find(&candidates).Error; err != nil {
			return
		}
		var selected *model.Channel
		for i := range candidates {
			candidate := &candidates[i]
			if pinnedChannelID != 0 && candidate.ID != pinnedChannelID {
				continue
			}
			if exclusions.Channels[candidate.ID] || !candidate.MappingConfirmed || candidate.Model.Status != 1 {
				continue
			}
			providerID := candidate.ProviderID
			if pinnedProviderID != 0 {
				providerID = pinnedProviderID
			} else if candidate.Provider.Id == 0 || candidate.Provider.Status != common.ChannelStatusEnabled {
				continue
			}
			if exclusions.Providers[providerID] {
				continue
			}
			selected = candidate
			break
		}
		if selected == nil {
			return
		}
		result.SkippedChannel = selected.ID
		provider := &selected.Provider
		if len(origins) > 0 {
			var err error
			provider, err = service.GatewayTaskProvider(origins[0])
			if err != nil {
				return
			}
		}
		c.Set("task_gateway_channel_selected", true)
		setupErr := middleware.SetupContextForSelectedChannel(c, provider, requested)
		c.Set("task_gateway_channel_selected", false)
		if setupErr != nil {
			return
		}
		info := &relaycommon.RelayInfo{UserId: c.GetInt("id"), TokenId: c.GetInt("token_id"), UsingGroup: group, OriginModelName: requested, StartTime: started, RequestId: requestID, TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: publicTaskID}}
		info.InitChannelMeta(c)
		info.Action = c.GetString("task_action")
		info.UpstreamModelName = selected.UpstreamModelName
		if len(origins) > 0 {
			info.UpstreamModelName = origins[0].Properties.UpstreamModelName
			info.OriginTaskID = origins[0].GetUpstreamTaskID()
			for _, origin := range origins {
				info.OriginTasks = append(info.OriginTasks, relaycommon.OriginTaskRef{TaskID: origin.TaskID, UpstreamTaskID: origin.GetUpstreamTaskID(), Action: origin.Action, Status: string(origin.Status), Data: origin.Data})
			}
		}
		adaptor := taskplugin.New(pin.Plugin)
		adaptor.Init(info)
		info.BeginProviderCall = budget.Start
		info.ResponseFacts = &relaycommon.ResponseFacts{OnBusinessOutput: budget.Delivered}
		facts, err := adaptor.ExtractUsageFactsValidated(c, info)
		if err != nil {
			lastStatus, lastCode = 400, "invalid_request"
			return
		}
		if len(origins) > 0 {
			merged := maps.Clone(origins[0].PrivateData.Gateway.Facts)
			maps.Copy(merged, facts)
			facts = merged
		}
		sourceValue, _, err := billingexpr.RunMoneyExpr(sale.Expression, billingexpr.MoneyInput{TaskUsage: true, Request: billingexpr.RequestInput{Usage: facts}, Time: started}, sale.Currency)
		if err != nil {
			lastStatus, lastCode = 503, "price_unavailable"
			return
		}
		sourceValue, err = money.MulUnitPrice(ratio, sourceValue)
		if err != nil {
			return
		}
		reserveValue := sourceValue
		if saleFX != nil {
			reserveValue, err = sourceValue.Convert(*saleFX)
			if err != nil {
				return
			}
		}
		reserve, err := reserveValue.Receivable()
		if err != nil {
			return
		}
		source, err := sourceValue.Receivable()
		if err != nil {
			return
		}
		if record == nil {
			record = &model.GatewayRequest{ID: requestID, UserID: info.UserId, TokenID: info.TokenId, ModelOfferingID: offering.ID, OfferingName: offering.Name, RequestedName: requested, ChoiceCount: 1, SalePriceVersionID: sale.ID, SaleExpression: sale.Expression, SaleCurrency: sale.Currency, SaleFXRate: saleRate, GroupRatio: ratio.String(), Receivable: money.Amount{Currency: model.SettlementCurrency()}, StartedAt: started.UnixMilli(), UsageSource: "none", Status: "pending", LedgerEntryIDs: []string{}}
			pending = model.InitTask(constant.TaskPlatform(pin.Plugin.Meta.Key), info)
			pending.Status = model.TaskStatusUnknown
			pending.Action = info.Action
			pending.PrivateData.Key = ""
			pending.PrivateData.Execution = service.TaskExecutionSnapshotFromContext(c)
			pending.PrivateData.Gateway = &model.TaskGatewaySnapshot{ProviderType: info.ChannelType, ChannelID: selected.ID, ProviderID: provider.Id, Facts: facts, SaleSource: source, Settlement: model.GatewaySettlement{Request: *record}}
			if err := model.ReserveGatewayTask(record, reserve, source, pending); err != nil {
				record, pending = nil, nil
				lastStatus, lastCode = 503, "task_persistence_failed"
				if errors.Is(err, model.ErrInsufficientWalletBalance) {
					lastStatus, lastCode = 402, "insufficient_balance"
					result.Failure = service.GatewayFailure{Class: "insufficient_balance"}
				}
				return
			}
			saleSource = source
		} else {
			var billing model.GatewayBilling
			if model.DB.Where("request_id = ?", requestID).Take(&billing).Error != nil || reserve.Nanos > billing.Reservation.Nanos {
				return
			}
		}
		requestedAction := info.Action
		if taskErr := adaptor.ValidateRequestAndSetAction(c, info); taskErr != nil {
			lastStatus, lastCode = 400, "invalid_request"
			return
		}
		if len(origins) > 0 && requestedAction == constant.TaskActionRemix && info.Action != requestedAction {
			lastStatus, lastCode = 400, "unsupported_task_action"
			return
		}
		body, err := adaptor.BuildRequestBody(c, info)
		if err != nil {
			return
		}
		purchase, _, rate, err := gatewayChannelPurchaseSnapshot(c.Request.Context(), selected.ID, clock.Now())
		if err != nil {
			return
		}
		attemptContext, err := budget.Start()
		if err != nil {
			return
		}
		c.Request = parent.WithContext(attemptContext)
		defer func() { c.Request = parent }()
		attempt := &model.GatewayAttempt{ID: common.NewRequestId(), RequestID: requestID, ChannelID: selected.ID, ProviderID: provider.Id, ModelID: selected.ModelID, StartedAt: clock.Now().UnixMilli(), PurchasePriceVersionID: purchase.ID, PurchaseExpression: purchase.Expression, AccountingFXRate: rate, Cost: money.Amount{Currency: purchase.Currency}, ConvertedCost: money.Amount{Currency: model.SettlementCurrency()}, UsageSource: "none", Status: "failed", ConversionLosses: []string{}, RouteDiagnostics: map[string]any{}}
		for _, member := range offering.Members {
			if member.ModelID == selected.ModelID {
				attempt.MemberTier = member.Tier
			}
		}
		snapshot := &model.TaskGatewaySnapshot{ProviderType: info.ChannelType, ChannelID: selected.ID, ProviderID: provider.Id, Facts: facts, SaleSource: saleSource, PreviousAttempts: append([]model.GatewayAttempt(nil), previousAttempts...), Settlement: model.GatewaySettlement{Request: *record, Attempt: attempt}}
		pending.ChannelId = info.ChannelId
		pending.Properties.UpstreamModelName = info.UpstreamModelName
		pending.PrivateData.Gateway = snapshot
		pending.PrivateData.Key = ""
		if err := pending.Update(); err != nil {
			lastStatus, lastCode = 500, "task_persistence_failed"
			return
		}
		if _, err := budget.Start(); err != nil {
			return
		}
		result.SkippedChannel = 0
		result.Attempt = attempt
		response, err := adaptor.DoRequest(c, info, body)
		attempt.EndedAt = clock.Now().UnixMilli()
		if err != nil || response == nil {
			result.Failure = service.ClassifyGatewayFailure(0, "", true, budget.TimedOut(), parent.Context().Err() != nil, false)
			attempt.ErrorClass = result.Failure.Class
			return
		}
		defer response.Body.Close()
		if response.StatusCode/100 != 2 {
			_, _ = io.Copy(io.Discard, response.Body)
			result.Failure = service.ClassifyGatewayFailure(response.StatusCode, "", false, budget.TimedOut(), parent.Context().Err() != nil, false)
			attempt.ErrorClass = result.Failure.Class
			return
		}
		// Once accepted, never submit this operation on another Channel.
		upstreamAccepted = true
		result.Delivered = true
		parsed, taskErr := adaptor.ParseResponse(c, response, info)
		if taskErr != nil || parsed == nil {
			lastStatus, lastCode = 502, "task_response_invalid"
			return
		}
		budget.Delivered()
		attempt.Status = "pending"
		snapshot.Accepted = true
		pending.Status = model.TaskStatusSubmitted
		pending.Data = parsed.TaskData
		pending.PrivateData.UpstreamTaskID = parsed.UpstreamTaskID
		pending.PrivateData.PluginState = parsed.PluginState
		if parsed.Immediate != nil {
			pending.Status = model.TaskStatus(parsed.Immediate.Status)
			pending.Progress = parsed.Immediate.Progress
			pending.FinishTime = clock.Now().Unix()
			snapshot.CompletionFacts = parsed.Immediate.UsageFacts
			if pending.Status == model.TaskStatusFailure {
				pending.FailReason = "Task failed"
			}
		}
		// Publish acceptance only after the shared runner finalizes its Attempt.
		// Once visible to pollers, submission must never save this stale row again.
		accepted = pending
		acceptedInfo = info
		return
	})
	c.Request = parent
	if pending != nil && len(attempts) > 0 {
		// The shared runner finalizes switch decisions after the callback.
		// Preserve its records rather than the callback's provisional copies.
		finalAttempt := attempts[len(attempts)-1]
		pending.PrivateData.Gateway.Settlement.Attempt = &finalAttempt
		pending.PrivateData.Gateway.PreviousAttempts = append([]model.GatewayAttempt(nil), attempts[:len(attempts)-1]...)
		if err := pending.Update(); err != nil {
			lastStatus, lastCode = 503, "task_persistence_failed"
			if upstreamAccepted {
				return nil, &dto.TaskError{StatusCode: lastStatus, Code: lastCode, Message: "Task state could not be persisted", LocalError: true}
			}
		}
	}
	if accepted != nil {
		_ = service.PublishGatewayTask(accepted)
		if accepted.Status == model.TaskStatusSuccess || accepted.Status == model.TaskStatusFailure {
			_ = service.SettleGatewayTask(context.WithoutCancel(parent.Context()), accepted)
		}
		return &taskSubmissionOutcome{Task: accepted, RelayInfo: acceptedInfo}, nil
	}
	if upstreamAccepted && pending != nil {
		_ = service.PublishGatewayTask(pending)
	}
	if record != nil && !upstreamAccepted {
		record.Status = "failed"
		if pending != nil {
			pending.Status = model.TaskStatusFailure
			pending.Progress = "100%"
			_ = pending.Update()
		}
		record.EndedAt = clock.Now().UnixMilli()
		intent := model.GatewaySettlement{Request: *record, Attempts: attempts, SaleSource: money.Amount{Currency: sale.Currency}}
		if err := model.PrepareGatewaySettlement(intent); err == nil {
			_, _ = model.SettleGateway(record.ID)
			_ = model.PublishGatewayRecords(record.ID)
		}
	}
	return nil, &dto.TaskError{StatusCode: lastStatus, Code: lastCode, Message: http.StatusText(lastStatus), LocalError: true}
}
