package controller

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/channelhealth"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/tokenkit"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

// responsesWSGateway composes the ordinary request middleware, shared Attempt
// runner and GatewayBilling lifecycle around one response.create.
func responsesWSGateway(c *gin.Context, call *relay.ResponsesWSCall) (bool, *types.NewAPIError) {
	offering := middleware.TextOffering(c)
	if offering == nil {
		if call.Established() || middleware.TextOfferingMissing(c) {
			service.RecordGatewayModelAccessDenied(c)
			return true, types.NewErrorWithStatusCode(errors.New("Model not found"), "model_not_found", http.StatusNotFound)
		}
		return false, nil
	}
	if call.LegacyConnection {
		return true, types.NewErrorWithStatusCode(errors.New("Connection billing identity changed; reconnect required"), "connection_locked", http.StatusConflict)
	}
	lockedOfferingID := middleware.TextOffering(c).ID
	call.AuthorizeControl = func(control *gin.Context) *types.NewAPIError {
		offering := middleware.TextOffering(control)
		if offering == nil || offering.ID != lockedOfferingID {
			return types.NewErrorWithStatusCode(errors.New("Model not found"), "model_not_found", http.StatusNotFound)
		}
		return authorizeWebSocketOffering(control)
	}
	if apiErr := authorizeWebSocketOffering(c); apiErr != nil {
		return true, apiErr
	}
	run := &textGatewayRun{}
	parent := c.Request
	attempts := service.RunGatewayAttempts(parent.Context(), service.GatewayPolicy(), func(budget *service.GatewayAttemptBudget, exclusions *service.GatewayExclusions) service.GatewayAttemptResult {
		return runResponsesWSGatewayAttempt(c, call, run, budget, exclusions)
	})
	c.Request = parent
	run.settle(parent.Context(), attempts)
	if run.status != 0 {
		if run.nativeError != nil {
			return true, run.nativeError
		}
		return true, types.NewErrorWithStatusCode(errors.New(run.message), types.ErrorCode(run.code), run.status, types.ErrOptionWithSkipRetry())
	}
	return true, nil
}

func authorizeWebSocketOffering(c *gin.Context) *types.NewAPIError {
	allowed, err := service.GatewayOfferingAllowed(c, middleware.TextOffering(c))
	if err != nil {
		return types.NewErrorWithStatusCode(errors.New("Authorization is temporarily unavailable"), "authorization_unavailable", http.StatusServiceUnavailable)
	}
	if !allowed {
		service.RecordGatewayModelAccessDenied(c)
		return types.NewErrorWithStatusCode(errors.New("Model not found"), "model_not_found", http.StatusNotFound)
	}
	return nil
}

// runRealtimeTransportAttempt opens the native session through the shared Route,
// health and attempt budget. Session setup does not generate a response. Each
// subsequent response reserves its known input and output plan before inference.
func runRealtimeTransportAttempt(c *gin.Context, call *relay.ResponsesWSCall, run *textGatewayRun, info *relaycommon.RelayInfo, selection *service.GatewaySelection, budget *service.GatewayAttemptBudget, stopProbe func(), prepared *relaychannel.PreparedWssRequest) (result service.GatewayAttemptResult) {
	ctx, err := info.BeginProviderCall()
	if err != nil {
		run.fail(c, http.StatusGatewayTimeout, "first_byte_budget", "First byte budget exhausted")
		return
	}
	call.Native.ConnectionReady = func() {
		stopProbe()
		selection.Permit.Disarm()
		budget.Handoff()
		_ = service.ChannelHealth.Release(context.WithoutCancel(ctx), selection.Admission)
		_ = selection.Permit.Release(context.WithoutCancel(ctx), true, 0)
	}
	parent := c.Request
	c.Request = parent.WithContext(ctx)
	_, apiErr := call.Exchange(c, info, selection.Channel.ID, selection.Provider.Id, prepared, nil)
	c.Request = parent
	result.TransportAttempt, result.Established = true, call.Established()
	if call.Established() {
		if errors.Is(context.Cause(ctx), service.ErrGatewayPermitLost) {
			run.nativeError = types.NewError(service.ErrGatewayPermitLost, types.ErrorCodeDoRequestFailed)
			run.fail(c, http.StatusServiceUnavailable, "channel_unavailable", "No Channel is available")
		}
		result.Failure.Class = "connection_established"
		return
	}
	if errors.Is(context.Cause(ctx), service.ErrGatewayPermitLost) {
		result.LocalRejection = true
		result.Failure = service.GatewayFailure{Class: "channel_unavailable", Switch: true, ExcludeProvider: true}
		run.fail(c, http.StatusServiceUnavailable, "channel_unavailable", "No Channel is available")
		return
	}
	status, code, errorType := prepared.HandshakeStatus(), "", ""
	if apiErr != nil {
		native := apiErr.ToOpenAIError()
		code, _ = native.Code.(string)
		errorType = native.Type
	}
	result.Failure = service.ClassifyGatewayFailure(status, code, true, budget.TimedOut(), parent.Context().Err() != nil, false, errorType)
	recordGatewayProviderFailure(c, info, selection, result.Failure)
	_, _ = service.ChannelHealth.Record(context.WithoutCancel(ctx), channelhealth.Sample{AttemptID: common.NewRequestId(), ChannelID: selection.Channel.ID, ProviderFailure: result.Failure.Switch, RateLimited: result.Failure.Class == "rate_limited", RetryAfter: prepared.RetryAfter(), Lease: selection.Admission.Lease})
	if result.Failure.ExcludeProvider {
		result.SkippedProvider = selection.Provider.Id
	}
	run.fail(c, http.StatusBadGateway, "upstream_error", "Upstream connection failed")
	return
}

func runResponsesWSGatewayAttempt(c *gin.Context, call *relay.ResponsesWSCall, run *textGatewayRun, budget *service.GatewayAttemptBudget, exclusions *service.GatewayExclusions) (result service.GatewayAttemptResult) {
	run.nativeError = nil
	run.status, run.code, run.message = 0, "", ""
	offering := middleware.TextOffering(c)
	started := c.GetTime(middleware.TextGatewayStartedAtKey)
	if call.Native != nil && !call.Native.StartedAt.IsZero() {
		started = call.Native.StartedAt
	}
	if started.IsZero() {
		started = clock.Now()
	}
	if run.sale == nil {
		var err error
		run.sale, err = model.CurrentPriceVersion(c.Request.Context(), model.DB, model.PriceOwnerOffering, offering.ID, started.Unix())
		if err == nil {
			run.saleFX, run.saleRate, err = service.GatewayFX(c.Request.Context(), run.sale.Currency)
		}
		if err == nil {
			run.ratio, err = service.ExactGatewayGroupRatio(common.GetContextKeyString(c, constant.ContextKeyUserGroup))
		}
		if err != nil {
			run.fail(c, http.StatusServiceUnavailable, "price_unavailable", "Model price is unavailable")
			return
		}
	}
	if call.Native != nil {
		if call.Established() && call.Native.Budget != nil {
			budget = call.Native.Budget
		}
		call.Native.Budget = budget
	}
	if call.Protocol() == protocol.OpenAIRealtime && billingexpr.UsesFixedPricingByHash(run.sale.Expression, billingexpr.ExprHashString(run.sale.Expression)) {
		run.fail(c, http.StatusBadRequest, "unsupported_pricing", "Fixed pricing is not supported for Realtime")
		return
	}
	mean, err := model.RecentOfferingOutputTokens(c.Request.Context(), offering.ID, started.UnixMilli())
	if err != nil {
		run.fail(c, http.StatusServiceUnavailable, "billing_unavailable", "Billing is unavailable")
		return
	}
	var rejected *service.GatewayParameterError
	candidates := map[int]*gatewayPreparedWebSocket{}
	selection, err := service.SelectGatewayRoute(c, offering, service.GatewayRouteInput{Protocol: call.Protocol(), Sale: run.sale, SaleFX: run.saleFX, Ratio: run.ratio, StartedAt: started, Failures: exclusions, Prepare: func(selection *service.GatewaySelection) (*service.GatewayRouteFacts, error) {
		prepared, err := prepareGatewayWebSocketRoute(c, selection, call, started, mean)
		if err != nil {
			if rejected == nil {
				var parameter *service.GatewayParameterError
				if errors.As(err, &parameter) {
					rejected = parameter
				}
			}
			return nil, err
		}
		candidates[selection.Channel.ID] = prepared
		return &prepared.Facts, nil
	}})
	if err != nil {
		status, code := http.StatusServiceUnavailable, "channel_unavailable"
		if errors.Is(err, service.ErrGatewayCapability) {
			status, code = http.StatusBadRequest, "unsupported_parameter"
			if rejected != nil {
				run.nativeError = types.NewErrorWithStatusCode(rejected, types.ErrorCode(code), status, types.ErrOptionWithSkipRetry())
				run.fail(c, status, code, rejected.Reason)
				return
			}
		}
		run.fail(c, status, code, "No Channel is available")
		return
	}
	chosen := candidates[selection.Channel.ID]
	c = chosen.Context
	budget.RequirePermit(selection.Permit)
	if call.Native == nil {
		call.WatchAttempt(budget.Context())
	}
	contactedProvider := false
	var observedUsage *dto.Usage
	defer func() {
		ctx := context.WithoutCancel(c.Request.Context())
		if observedUsage != nil {
			selection.Permit.Release(ctx, contactedProvider, int64(observedUsage.TotalTokens))
		} else {
			selection.Permit.Release(ctx, contactedProvider)
		}
	}()
	if call.Native != nil {
		call.Native.Capabilities = selection.Capabilities
		call.Native.InputUsage = chosen.InputUsage
	}
	stopProbe := service.ChannelHealth.KeepAlive(c.Request.Context(), selection.Admission)
	defer stopProbe()
	defer func() {
		if result.Attempt == nil {
			if call.Established() && run.code != "" {
				result.Failure = service.GatewayFailure{Class: run.code}
				result.Established = true
			}
			_ = service.ChannelHealth.Release(context.WithoutCancel(c.Request.Context()), selection.Admission)
			result.SkippedChannel = selection.Channel.ID
		}
	}()
	if call.LockedChannelID != 0 && (selection.Channel.ProviderID != call.LockedProviderID || selection.Provider.Type != call.LockedProviderType || selection.Channel.UpstreamModelName != call.LockedUpstreamModel) {
		run.fail(c, http.StatusConflict, "connection_locked", "Upstream connection settings changed; reconnect required")
		return
	}
	if call.Protocol() == protocol.OpenAIRealtime {
		// Validate the current purchase rule before touching the wallet. The
		// attempt still freezes its price and FX at the actual transport start.
		price, err := model.CurrentPriceVersion(c.Request.Context(), model.DB, model.PriceOwnerChannel, selection.Channel.ID, clock.Now().Unix())
		if err != nil {
			run.fail(c, http.StatusServiceUnavailable, "price_unavailable", "Model price is unavailable")
			return
		}
		if billingexpr.UsesFixedPricingByHash(price.Expression, billingexpr.ExprHashString(price.Expression)) {
			run.fail(c, http.StatusBadRequest, "unsupported_pricing", "Fixed pricing is not supported for Realtime")
			return
		}
	}
	info := chosen.Info
	committedKey := call.Established()
	info.BeginProviderCall = func() (context.Context, error) {
		ctx, err := budget.Start()
		if err != nil {
			return nil, err
		}
		if !committedKey {
			if err = selection.Provider.CommitRouteKey(ctx, info.ApiKey, info.ChannelMultiKeyIndex); err != nil {
				return nil, err
			}
			committedKey = true
		} else if err = selection.Provider.ValidateRouteKey(ctx, info.ApiKey, info.ChannelMultiKeyIndex); err != nil {
			return nil, err
		}
		contactedProvider = true
		return ctx, nil
	}
	var sentAt time.Time
	var firstByte time.Duration
	lostPermit := false
	info.ResponseFacts.OnBusinessOutput = func() {
		if firstByte == 0 && !sentAt.IsZero() {
			firstByte = max(clock.Now().Sub(sentAt), time.Nanosecond)
		}
		budget.Delivered()
	}
	if call.Native != nil && call.Native.TransportOnly {
		result = runRealtimeTransportAttempt(c, call, run, info, selection, budget, stopProbe, chosen.Transport)
		contactedProvider = result.TransportAttempt
		return
	}
	input, purchaseInput := chosen.Facts.SaleInput, chosen.Facts.PurchaseInput
	estimated := chosen.Facts.Usage
	mean = int64(estimated.CompletionTokens)
	prepared, payload := chosen.Transport, chosen.Payload
	if call.Native != nil {
		call.Native.OutputAudio = estimated.CompletionTokenDetails.AudioTokens > 0
	}
	source, reserveValue, _, err := service.GatewayPrice(run.sale, estimated, input, started, run.ratio, run.saleFX)
	reserve, reserveErr := reserveValue.Receivable()
	sourceReserve, sourceErr := source.Receivable()
	if err != nil || reserveErr != nil || sourceErr != nil {
		run.fail(c, http.StatusServiceUnavailable, "price_unavailable", "Model price is unavailable")
		return
	}
	if run.record == nil {
		record := &model.GatewayRequest{ID: info.RequestId, UserID: c.GetInt("id"), TokenID: c.GetInt("token_id"), ModelOfferingID: offering.ID, OfferingName: offering.Name, RequestedName: call.Request.Model,
			ChoiceCount: 1, SalePriceVersionID: run.sale.ID, SaleExpression: run.sale.Expression, SaleCurrency: run.sale.Currency, SaleFXRate: run.saleRate, GroupRatio: run.ratio.String(),
			Receivable: money.Amount{Currency: model.SettlementCurrency()}, StartedAt: started.UnixMilli(), UsageSource: "none", Status: "failed", LedgerEntryIDs: []string{}}
		if err = model.ReserveGateway(record, reserve, sourceReserve); err != nil {
			if errors.Is(err, model.ErrInsufficientWalletBalance) {
				result.Failure = service.GatewayFailure{Class: "insufficient_balance"}
				run.fail(c, http.StatusPaymentRequired, "insufficient_balance", "Insufficient wallet balance")
			} else {
				result.Failure = service.GatewayFailure{Class: "billing_unavailable"}
				run.fail(c, http.StatusServiceUnavailable, "billing_unavailable", "Billing is unavailable")
			}
			return
		}
		run.record, run.reservation, run.saleSource = record, reserve, sourceReserve
	} else if reserve.Nanos > run.reservation.Nanos {
		run.fail(c, http.StatusServiceUnavailable, "channel_unavailable", "No Channel is available")
		return
	}
	attemptContext, err := info.BeginProviderCall()
	if err != nil {
		code := "budget_exhausted"
		if errors.Is(err, service.ErrGatewayPermitLost) {
			code = "channel_unavailable"
		}
		run.fail(c, http.StatusServiceUnavailable, code, "Upstream request failed")
		return
	}
	// Credential locking is preparation. Freeze purchase facts after it, and
	// count Provider contact only when the prepared exchange starts.
	contactedProvider = false
	attemptStart := clock.Now()
	purchase, purchaseFX, rate, err := gatewayChannelPurchaseSnapshot(c.Request.Context(), selection.Channel.ID, attemptStart)
	if err != nil {
		run.fail(c, http.StatusServiceUnavailable, "price_unavailable", "Channel price is unavailable")
		return
	}
	if call.Protocol() == protocol.OpenAIRealtime && billingexpr.UsesFixedPricingByHash(purchase.Expression, billingexpr.ExprHashString(purchase.Expression)) {
		run.fail(c, http.StatusBadRequest, "unsupported_pricing", "Fixed pricing is not supported for Realtime")
		return
	}
	costEstimate, convertedEstimate, _, priceErr := service.GatewayPrice(purchase, estimated, purchaseInput, attemptStart, decimal.NewFromInt(1), purchaseFX)
	_, costErr := costEstimate.Cost()
	_, convertedErr := convertedEstimate.Cost()
	if priceErr != nil || costErr != nil || convertedErr != nil {
		run.fail(c, http.StatusServiceUnavailable, "price_unavailable", "Channel price is unavailable")
		return
	}
	comparison, err := service.GatewayComparisonCost(c.Request.Context(), costEstimate)
	if err != nil {
		run.fail(c, http.StatusServiceUnavailable, "price_unavailable", "Channel price is unavailable")
		return
	}
	profitable, err := service.GatewayRouteProfitAllowed(comparison, reserveValue, offering.MinimumMarginRate)
	if err != nil {
		run.fail(c, http.StatusServiceUnavailable, "price_unavailable", "Channel price is unavailable")
		return
	}
	selection.LossFallback = false
	if !profitable {
		if offering.AllowLossFallback {
			selection.LossFallback, err = selection.LossFallbackAllowed(c.Request.Context(), comparison, offering.MinimumMarginRate, contactedProvider)
		}
		if err != nil || !selection.LossFallback {
			run.fail(c, http.StatusServiceUnavailable, "channel_unavailable", "No Channel is available")
			return
		}
	}
	selection.Diagnostics["estimated_cost"], selection.Diagnostics["loss_fallback"] = comparison.String(), selection.LossFallback
	if call.Native != nil {
		call.Native.Validate = func(saleRequest, purchaseRequest billingexpr.RequestInput, knownInput *dto.Usage) error {
			probe := service.GatewayUsageSnapshot(knownInput)
			estimatedOutput := mean
			if limit := ojson.GetBytes(purchaseRequest.Body, "response.max_output_tokens"); limit.Exists() {
				estimatedOutput = min(estimatedOutput, limit.Int())
			}
			probe.CompletionTokens, probe.TotalTokens = int(estimatedOutput), probe.PromptTokens+int(estimatedOutput)
			probe.CompletionTokenDetails = dto.OutputTokenDetails{}
			if call.Native.OutputAudio {
				probe.CompletionTokenDetails.AudioTokens = int(estimatedOutput)
			}
			_, amount, _, quoteErr := service.GatewayPrice(run.sale, probe, saleRequest, started, run.ratio, run.saleFX)
			if quoteErr != nil {
				return quoteErr
			}
			required, quoteErr := amount.Receivable()
			if quoteErr != nil {
				return quoteErr
			}
			if required.Nanos > run.reservation.Nanos {
				return model.ErrInsufficientWalletBalance
			}
			original, converted, _, quoteErr := service.GatewayPrice(purchase, probe, purchaseRequest, attemptStart, decimal.NewFromInt(1), purchaseFX)
			if quoteErr != nil {
				return quoteErr
			}
			if _, quoteErr = original.Cost(); quoteErr != nil {
				return quoteErr
			}
			_, quoteErr = converted.Cost()
			return quoteErr
		}
	}
	attemptContext, err = budget.Start()
	if err != nil {
		run.fail(c, http.StatusServiceUnavailable, "budget_exhausted", "Upstream request failed")
		return
	}
	parent := c.Request
	c.Request = parent.WithContext(attemptContext)
	if call.Native == nil {
		cancelPermit := call.CancelWithCause
		defer context.AfterFunc(parent.Context(), func() { cancelPermit(context.Cause(parent.Context())) })()
	}
	defer func() { c.Request = parent }()
	attempt := &model.GatewayAttempt{ID: common.NewRequestId(), RequestID: run.record.ID, ChannelID: selection.Channel.ID, ProviderID: selection.Channel.ProviderID, ModelID: selection.Channel.ModelID, MemberTier: selection.Tier,
		StartedAt: attemptStart.UnixMilli(), UsageSource: "none", PurchasePriceVersionID: purchase.ID, PurchaseExpression: purchase.Expression, AccountingFXRate: rate,
		Cost: money.Amount{Currency: purchase.Currency}, ConvertedCost: money.Amount{Currency: model.SettlementCurrency()}, RouteDiagnostics: selection.Diagnostics, ConversionLosses: []string{}}
	sentAt = clock.Now()
	contactedProvider = true
	usage, apiErr := call.Exchange(c, info, selection.Channel.ID, selection.Channel.ProviderID, prepared, payload)
	// The Attempt context may be expired while the client is still present.
	// Classify against the parent before deciding cancellation accounting.
	c.Request = parent
	attempt.EndedAt = clock.Now().UnixMilli()
	lostPermit = errors.Is(context.Cause(attemptContext), service.ErrGatewayPermitLost)
	if lostPermit {
		if info.ResponseFacts.BusinessOutput {
			attempt.Status, attempt.ErrorClass = "partial", "channel_unavailable"
		} else {
			attempt.Status, attempt.ErrorClass = "failed", "channel_unavailable"
		}
	} else {
		attempt.Status, attempt.ErrorClass = gatewayAttemptOutcome(c, info, apiErr)
	}
	outcome := info.StreamStatus.OutcomeSnapshot()
	status, code, errorType := 0, outcome.ErrorCode, outcome.ErrorType
	if call.Established() {
		status = http.StatusOK
	}
	if outcome.ErrorStatus >= 400 {
		status = outcome.ErrorStatus
	}
	if apiErr != nil {
		status = apiErr.StatusCode
		if !call.Established() && prepared.HandshakeStatus() == 0 {
			status = 0
		}
		native := apiErr.ToOpenAIError()
		code, _ = native.Code.(string)
		errorType = native.Type
	}
	cancelled := parent.Context().Err() != nil && !call.TerminalDelivered() || outcome.Response == relaycommon.ResponseOutcomeCancelled
	normalBudget := (outcome.OutputLimitReached() || info.ResponseFacts.OutputLimitObserved) && apiErr == nil && call.TerminalDelivered() && info.StreamStatus.CompletedNormally() && !info.ResponseFacts.DownstreamWriteFailed && !cancelled
	if call.TerminalDelivered() && !info.ResponseFacts.DownstreamWriteFailed {
		attempt.Status = "failed"
		if info.ResponseFacts.BusinessOutput {
			attempt.Status = "partial"
			if info.StreamStatus.CompletedNormally() && !outcome.OutputLimitReached() && !info.ResponseFacts.OutputLimitObserved {
				attempt.Status = "success"
			}
		}
	}
	if outcome.Response == relaycommon.ResponseOutcomeCancelled {
		attempt.Status = "cancelled"
	}
	if normalBudget {
		attempt.Status = "partial"
		code = "gateway_output_limit"
	}
	if outcome.IncompleteReason == "content_filter" {
		code = "content_filter"
	}
	transportFailed := apiErr != nil || outcome.EndReason == relaycommon.StreamEndReasonScannerErr || outcome.EndReason == relaycommon.StreamEndReasonTimeout
	failure := service.ClassifyGatewayFailure(status, code, transportFailed, budget.TimedOut(), cancelled, info.ResponseFacts.DownstreamWriteFailed, errorType)
	lostPermit = errors.Is(context.Cause(attemptContext), service.ErrGatewayPermitLost)
	if lostPermit {
		failure = service.GatewayFailure{Class: "channel_unavailable", Switch: true}
	}
	if attempt.Status == "success" {
		service.RecordGatewayChannelAffinity(c, selection)
		failure = service.GatewayFailure{}
	}
	if attempt.Status != "success" {
		attempt.ErrorClass = failure.Class
	}
	recordGatewayProviderFailure(c, info, selection, failure)
	result = service.GatewayAttemptResult{Attempt: attempt, Failure: failure, Delivered: info.ResponseFacts.BusinessOutput, Established: call.Established(), HealthLease: selection.Admission.Lease, FirstByte: firstByte, RetryAfter: prepared.RetryAfter(), LocalRejection: lostPermit}
	if call.Native != nil {
		if call.Native.SaleInput != nil {
			input = call.Native.SaleInput()
		}
		if call.Native.PurchaseInput != nil {
			purchaseInput = call.Native.PurchaseInput()
		}
	}
	run.record.Status = attempt.Status
	if usage != nil {
		if !info.ResponseFacts.UsageReported && !info.ResponseFacts.PromptTokensReported && !info.ResponseFacts.InputDetailsReported && !info.ResponseFacts.CompletionTokensReported && !info.ResponseFacts.BusinessOutput && info.ResponseFacts.SourceGeneratedText == "" && info.ResponseFacts.GeneratedAudioBytes == 0 && (info.ResponseFacts.SourceOutputTokens == nil || *info.ResponseFacts.SourceOutputTokens == 0) {
			usage = nil
		}
	}
	if usage != nil {
		observedUsage = usage
		attempt.Usage, attempt.UsageSource = service.GatewayRecordUsage(usage), "estimated"
		if info.ResponseFacts.UsageReported && !info.ResponseFacts.UsageEstimated {
			attempt.UsageSource = "upstream"
		}
		cost, converted, trace, priceErr := service.GatewayPrice(purchase, usage, purchaseInput, attemptStart, decimal.NewFromInt(1), purchaseFX)
		trace.MatchedTier = ""
		attempt.Cost, err = cost.Cost()
		attempt.CostPending = err != nil || attempt.Cost.Currency != purchase.Currency
		attempt.ConvertedCost, err = converted.Cost()
		attempt.ConvertedCostPending = err != nil || attempt.ConvertedCost.Currency != model.SettlementCurrency()
		if run.attemptUsage == nil {
			run.attemptUsage = make(map[string]*dto.Usage)
			run.attemptTrace = make(map[string]billingexpr.MoneyTraceResult)
		}
		run.attemptUsage[attempt.ID] = service.GatewayUsageSnapshot(usage)
		run.attemptTrace[attempt.ID] = trace
		if priceErr != nil || attempt.CostPending || attempt.ConvertedCostPending {
			run.evaluationFailed = true
			run.record.ErrorCode = "billing_evaluation_failed"
		}
		if info.ResponseFacts.BusinessOutput {
			saleUsage := service.GatewayUsageSnapshot(usage)
			saleUsageSource := attempt.UsageSource
			completeOnDisconnect := info.ResponseFacts.CompletionTokensReported && info.ResponseFacts.DownstreamWriteFailed && outcome.Response == relaycommon.ResponseOutcomeCompleted
			if !completeOnDisconnect && !normalBudget && (attempt.Status == "partial" || !call.TerminalDelivered()) {
				delivered := tokenkit.Count(info.TokenizerModel, info.ResponseFacts.GeneratedText)
				if info.ResponseFacts.DeliveredOutputTokens != nil {
					delivered = *info.ResponseFacts.DeliveredOutputTokens
				}
				if delivered < saleUsage.CompletionTokens {
					saleUsageSource = "estimated"
					saleUsage.CompletionTokens, saleUsage.OutputTokens = delivered, delivered
					saleUsage.TotalTokens = saleUsage.PromptTokens + delivered
					saleUsage.CompletionTokenDetails = info.ResponseFacts.DeliveredOutputDetails
					details := saleUsage.CompletionTokenDetails
					saleUsage.OutputTokensDetails = &details
				}
			}
			run.saleUsage = saleUsage
			run.record.Usage, run.record.UsageSource = service.GatewayRecordUsage(saleUsage), saleUsageSource
			source, receivable, saleTrace, priceErr := service.GatewayPrice(run.sale, saleUsage, input, started, run.ratio, run.saleFX)
			saleTrace.MatchedTier = ""
			run.saleTrace = &saleTrace
			if attempt.Status != "success" && (saleTrace.BillingUnit == billingexpr.BillingUnitRequest || billingexpr.UsesOnlyFixedPricing(run.sale.Expression)) {
				source, _ = money.ParseValue("0", run.sale.Currency)
				receivable, _ = money.ParseValue("0", model.SettlementCurrency())
				priceErr = nil
			}
			run.record.Receivable, err = receivable.Receivable()
			if err == nil && priceErr == nil {
				run.saleSource, err = source.Receivable()
			}
			if err != nil || priceErr != nil {
				run.record.Receivable = money.Amount{Currency: model.SettlementCurrency()}
				run.record.SalePending = true
				run.evaluationFailed, run.saleEvaluationFailed = true, true
				run.record.ErrorCode = "billing_evaluation_failed"
			}
		}
	}
	if attempt.Status != "success" && apiErr != nil {
		if outcome.Response == relaycommon.ResponseOutcomeCancelled {
			run.fail(c, apiErr.StatusCode, string(apiErr.GetErrorCode()), apiErr.Error())
			run.nativeError = apiErr
			return
		}
		status := http.StatusBadGateway
		if failure.Class == "invalid_request" || failure.Class == "context_length" || failure.Class == "content_safety" {
			status = http.StatusBadRequest
		}
		run.fail(c, status, "upstream_error", "Upstream response failed")
	}
	return
}
