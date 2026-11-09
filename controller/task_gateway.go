package controller

import (
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/money"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// AuthorizeTaskGateway checks resolved identities before plugin decoding.
func AuthorizeTaskGateway(c *gin.Context) {
	if taskErr := taskGatewayAccessError(c); taskErr != nil {
		c.Abort()
		if kind, _ := middleware.TextGatewayEndpoint(c.Request.Method, c.Request.URL.Path); kind != "" {
			gatewayError(c, taskErr.StatusCode, taskErr.Code, taskErr.Message)
			return
		}
		respondTaskSubmissionError(c, taskErr)
		return
	}
	c.Next()
}

func taskGatewayAccessError(c *gin.Context) *dto.TaskError {
	if !middleware.TextOfferingMissing(c) {
		offering := middleware.TextOffering(c)
		if offering == nil {
			return nil
		}
		allowed, err := service.GatewayOfferingAllowed(c, offering)
		if err != nil {
			return &dto.TaskError{StatusCode: http.StatusServiceUnavailable, Code: "authorization_unavailable", Message: http.StatusText(http.StatusServiceUnavailable), LocalError: true}
		}
		if allowed {
			return nil
		}
	}
	service.RecordGatewayModelAccessDenied(c)
	return &dto.TaskError{StatusCode: http.StatusNotFound, Code: "model_not_found", Message: "Model not found", LocalError: true}
}

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
	if taskErr := taskGatewayAccessError(c); taskErr != nil {
		return nil, taskErr
	}
	pinValue, _ := c.Get(jsplugin.ContextKeyPinnedPlugin)
	pin, ok := pinValue.(jsplugin.PinnedPlugin)
	if !ok || pin.Plugin == nil {
		return nil, &dto.TaskError{StatusCode: 400, Code: "invalid_request", Message: "Task plugin is unavailable", LocalError: true}
	}
	origins, _ := common.GetContextKeyType[[]*model.Task](c, constant.ContextKeyOriginTasks)
	pinnedChannelID, pinnedProviderID := 0, 0
	for _, origin := range origins {
		if origin.PrivateData.Gateway == nil || !origin.PrivateData.Gateway.Accepted || origin.UserId != c.GetInt("id") || origin.Platform != origins[0].Platform {
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
	var record *model.GatewayRequest
	var saleSource money.Amount
	var accepted *model.Task
	var acceptedInfo *relaycommon.RelayInfo
	var pending *model.Task
	var previousAttempts []model.GatewayAttempt
	attemptTraces := make(map[string]billingexpr.MoneyTraceResult)
	publicTaskID := model.GenerateTaskID()
	upstreamAccepted := false
	var lastStatus = http.StatusServiceUnavailable
	var lastCode = "channel_unavailable"
	parent := c.Request
	originalContext := c.Copy()
	originalContext.Request = parent.Clone(parent.Context())
	attempts := service.RunGatewayAttempts(parent.Context(), service.GatewayPolicy(), func(budget *service.GatewayAttemptBudget, exclusions *service.GatewayExclusions) (result service.GatewayAttemptResult) {
		defer func() {
			if result.Attempt != nil {
				previousAttempts = append(previousAttempts, *result.Attempt)
			}
		}()
		prepared := map[int]*gatewayPreparedTask{}
		selection, err := service.SelectGatewayRoute(c, offering, service.GatewayRouteInput{PinnedChannelID: pinnedChannelID, PinnedProviderID: pinnedProviderID, Sale: sale, SaleFX: saleFX, Ratio: ratio, StartedAt: started, Failures: exclusions,
			Prepare: func(selection *service.GatewaySelection) (*service.GatewayRouteFacts, error) {
				if !strings.HasPrefix(string(selection.Channel.Protocol), "task:") || pinnedChannelID != 0 && selection.Channel.ID != pinnedChannelID {
					return nil, service.ErrGatewayConversion
				}
				if len(origins) > 0 {
					if string(origins[0].Platform) != strings.TrimPrefix(string(selection.Channel.Protocol), "task:") {
						return nil, service.ErrGatewayConversion
					}
					provider, err := service.GatewayTaskProviderIdentity(origins[0])
					if err != nil || provider.Id != selection.Provider.Id {
						return nil, service.ErrGatewayUnavailable
					}
				}
				candidate, err := prepareGatewayTaskRoute(originalContext, selection, pin.Plugin, requested, requestID, publicTaskID, started, origins)
				if err != nil {
					return nil, err
				}
				prepared[selection.Channel.ID] = candidate
				return &candidate.RouteFacts, nil
			}})
		if err != nil {
			if errors.Is(err, service.ErrGatewayCapability) || errors.Is(err, service.ErrGatewayConversion) {
				lastStatus, lastCode = 400, "unsupported_parameter"
			}
			if errors.Is(err, service.ErrGatewayPriceUnavailable) {
				lastStatus, lastCode = 503, "price_unavailable"
			}
			return
		}
		selected, provider := &selection.Channel, selection.Provider
		candidate := prepared[selected.ID]
		maps.Copy(c.Keys, candidate.Context.Keys)
		info, adaptor, facts, saleInput := candidate.Info, candidate.Adaptor, candidate.Facts, candidate.SaleInput
		info.SubmittedUsage = maps.Clone(facts)
		contactedProvider := false
		defer func() {
			ctx := context.WithoutCancel(parent.Context())
			if accepted != nil && accepted == pending && (accepted.Status == model.TaskStatusSuccess || accepted.Status == model.TaskStatusFailure) {
				schema, _ := candidate.Plugin.Meta.UsageForModels(info.UpstreamModelName, info.OriginModelName)
				if tokens, known, err := gatewayTaskTokens(schema, accepted.PrivateData.Gateway.CompletionFacts, false); err == nil && known {
					_ = selection.Permit.Release(ctx, contactedProvider, tokens)
					return
				}
			}
			_ = selection.Permit.Release(ctx, contactedProvider)
		}()
		stopProbe := service.ChannelHealth.KeepAlive(parent.Context(), selection.Admission)
		defer stopProbe()
		defer func() {
			if result.Attempt == nil {
				_ = service.ChannelHealth.Release(context.WithoutCancel(parent.Context()), selection.Admission)
			}
		}()
		result.HealthLease = selection.Admission.Lease
		result.SkippedChannel = selected.ID
		committedKey := false
		info.BeginProviderCall = func() (context.Context, error) {
			ctx, err := budget.Start()
			if err != nil {
				return nil, err
			}
			if !committedKey {
				if err := provider.CommitRouteKey(ctx, info.ApiKey, info.ChannelMultiKeyIndex); err != nil {
					return nil, err
				}
				committedKey = true
			} else if err := provider.ValidateRouteKey(ctx, info.ApiKey, info.ChannelMultiKeyIndex); err != nil {
				return nil, err
			}
			ctx, err = budget.Start()
			if err != nil {
				return nil, err
			}
			contactedProvider = true
			return ctx, nil
		}
		info.ResponseFacts = &relaycommon.ResponseFacts{OnBusinessOutput: budget.Delivered}
		common.SetContextKey(c, constant.ContextKeyResponseFacts, info.ResponseFacts)
		sourceValue, reserveValue, saleTrace, err := service.GatewayPrice(sale, nil, saleInput, started, ratio, saleFX)
		if err != nil {
			lastStatus, lastCode = 503, "price_unavailable"
			return
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
			pending = model.InitTask(constant.TaskPlatform(candidate.Plugin.Meta.Key), info)
			pending.Status = model.TaskStatusUnknown
			pending.Action = info.Action
			pending.PrivateData.Key = ""
			pending.PrivateData.Execution = service.TaskExecutionSnapshotFromContext(c)
			pending.PrivateData.Gateway = &model.TaskGatewaySnapshot{ProviderType: info.ChannelType, ChannelID: selected.ID, ProviderID: provider.Id, Facts: facts, SaleSource: source, SaleConditions: saleTrace.RequestSnapshot, Settlement: model.GatewaySettlement{Request: *record}}
			if err := model.ReserveGatewayTask(record, reserve, source, pending); err != nil {
				record, pending = nil, nil
				lastStatus, lastCode = 503, "task_persistence_failed"
				if errors.Is(err, model.ErrInsufficientWalletBalance) {
					// The shared preflight exclusions can try a smaller reservation.
					lastStatus, lastCode = 402, "insufficient_balance"
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
		// Reject missing prices before Provider preparation I/O; freeze the actual
		// version again at the model-send boundary after preparation completes.
		if _, _, _, err := gatewayChannelPurchaseSnapshot(c.Request.Context(), selected.ID, clock.Now()); err != nil {
			return
		}
		body, err := adaptor.BuildRequestBody(c, info)
		if err != nil {
			return
		}
		attemptStart := clock.Now()
		purchase, _, rate, err := gatewayChannelPurchaseSnapshot(c.Request.Context(), selected.ID, attemptStart)
		if err != nil {
			return
		}
		attemptContext, err := budget.Start()
		if err != nil {
			return
		}
		c.Request = parent.WithContext(attemptContext)
		defer func() { c.Request = parent }()
		attempt := &model.GatewayAttempt{ID: common.NewRequestId(), RequestID: requestID, ChannelID: selected.ID, ProviderID: provider.Id, ModelID: selected.ModelID, StartedAt: attemptStart.UnixMilli(), PurchasePriceVersionID: purchase.ID, PurchaseExpression: purchase.Expression, AccountingFXRate: rate, Cost: money.Amount{Currency: purchase.Currency}, ConvertedCost: money.Amount{Currency: model.SettlementCurrency()}, UsageSource: "none", Status: "failed", MemberTier: selection.Tier, ConversionLosses: selection.ConversionLosses, RouteDiagnostics: selection.Diagnostics}
		attempt.Usage.TaskFacts = maps.Clone(facts)
		attempt.UsageSource = "estimated"
		for _, member := range offering.Members {
			if member.ModelID == selected.ModelID {
				attempt.MemberTier = member.Tier
			}
		}
		snapshot := &model.TaskGatewaySnapshot{ProviderType: info.ChannelType, ChannelID: selected.ID, ProviderID: provider.Id, Facts: facts, SaleSource: saleSource, SaleConditions: pending.PrivateData.Gateway.SaleConditions, PreviousAttempts: append([]model.GatewayAttempt(nil), previousAttempts...), Settlement: model.GatewaySettlement{Request: *record, Attempt: attempt, AttemptTrace: attemptTraces}}
		pending.ChannelId = info.ChannelId
		pending.Platform = constant.TaskPlatform(candidate.Plugin.Meta.Key)
		pending.Action = info.Action
		pending.PrivateData.Execution = service.TaskExecutionSnapshotFromContext(c)
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
		var pricingErr error
		info.OnPreparedRequest = func(wire *http.Request) error {
			input, err := helper.BuildTaskBillingRequestInput(wire)
			if err == nil {
				input.Usage, input.FreezeMoneyRequest = facts, true
				value, trace, evalErr := billingexpr.RunMoneyExpr(purchase.Expression, billingexpr.MoneyInput{TaskUsage: true, Request: input, Time: time.UnixMilli(attempt.StartedAt)}, purchase.Currency)
				err = evalErr
				if err == nil {
					comparison, compareErr := service.GatewayComparisonCost(wire.Context(), value)
					err = compareErr
					if err == nil {
						allowed, profitErr := service.GatewayRouteProfitAllowed(comparison, selection.EstimatedSale, offering.MinimumMarginRate)
						err = profitErr
						if err == nil && !allowed && offering.AllowLossFallback {
							allowed, err = selection.LossFallbackAllowed(wire.Context(), comparison, offering.MinimumMarginRate)
						}
						if err == nil && !allowed {
							err = service.ErrGatewayUnavailable
						}
					}
				}
				if err == nil {
					snapshot.PurchaseConditions = trace.RequestSnapshot
					attemptTraces[attempt.ID] = billingexpr.MoneyTraceResult{Currency: purchase.Currency, RequestSnapshot: trace.RequestSnapshot}
					err = pending.Update()
					if err == nil {
						_, err = info.BeginProviderCall()
					}
				}
			}
			pricingErr = err
			return err
		}
		result.SkippedChannel = 0
		result.Attempt = attempt
		response, err := adaptor.DoRequest(c, info, body)
		attempt.EndedAt = clock.Now().UnixMilli()
		if pricingErr != nil {
			result.Attempt, result.SkippedChannel = nil, selected.ID
			lastStatus, lastCode = 503, "price_unavailable"
			return
		}
		if err != nil || response == nil {
			result.Failure = service.ClassifyGatewayFailure(0, "", true, budget.TimedOut(), parent.Context().Err() != nil, false)
			attempt.ErrorClass = result.Failure.Class
			if adaptor.SubmissionMayHaveBeenAccepted() {
				upstreamAccepted = true
				result.Delivered = true
				attempt.Status = "submission_unknown"
				lastStatus, lastCode = 502, "task_submission_unknown"
			}
			return
		}
		defer response.Body.Close()
		if response.StatusCode/100 != 2 {
			raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			var payload struct {
				Error any    `json:"error"`
				Code  string `json:"code"`
			}
			var code, kind string
			if common.Unmarshal(raw, &payload) == nil {
				code = payload.Code
				switch failure := payload.Error.(type) {
				case string:
					code = failure
				case map[string]any:
					kind, _ = failure["type"].(string)
					code, _ = failure["code"].(string)
					if code == "" {
						code = kind
					}
				}
			}
			result.Failure = service.ClassifyGatewayFailure(response.StatusCode, code, false, budget.TimedOut(), parent.Context().Err() != nil, false, kind)
			if result.Failure.Class == "rate_limited" {
				result.RetryAfter = response.Header.Get("Retry-After")
			}
			gatewayAutoDisableProvider(c, selected.ID, provider, info, result.Failure)
			attempt.ErrorClass = result.Failure.Class
			// A rejection without reported billing facts cannot establish a free
			// Provider operation. Reconcile procurement without holding user funds.
			attempt.CostPending, attempt.ConvertedCostPending = true, true
			lastStatus, lastCode = response.StatusCode, "upstream_error"
			if lastStatus < 400 || lastStatus > 599 {
				lastStatus = http.StatusBadGateway
			}
			return
		}
		result.FirstByte = max(clock.Now().Sub(attemptStart), time.Nanosecond)
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
			snapshot.Stream = parsed.Stream
			if pending.Status == model.TaskStatusFailure {
				pending.FailReason = "Task failed"
			}
		}
		applyTaskResultRetention(c, pending, parsed.Immediate)
		// Publish acceptance only after the shared runner finalizes its Attempt.
		// Once visible to pollers, submission must never save this stale row again.
		service.RecordGatewayChannelAffinity(c, selection)
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
		if accepted.Status == model.TaskStatusFailure && accepted.PrivateData.Gateway.Stream != nil && !c.Writer.Written() {
			return nil, &dto.TaskError{StatusCode: http.StatusBadGateway, Code: "task_response_invalid", Message: http.StatusText(http.StatusBadGateway), LocalError: true}
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
		intent := model.GatewaySettlement{Request: *record, Attempts: attempts, AttemptTrace: attemptTraces, SaleSource: money.Amount{Currency: sale.Currency}}
		for _, attempt := range attempts {
			if attempt.CostPending || attempt.ConvertedCostPending {
				intent.EvaluationFailed = true
				intent.Request.Status = "settlement_pending"
				intent.Request.ErrorCode = "billing_evaluation_failed"
			}
		}
		if err := model.PrepareGatewaySettlement(intent); err == nil {
			_, _ = model.SettleGateway(record.ID)
			_ = model.PublishGatewayRecords(record.ID)
		}
	}
	return nil, &dto.TaskError{StatusCode: lastStatus, Code: lastCode, Message: http.StatusText(lastStatus), LocalError: true}
}
