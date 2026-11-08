package controller

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/capability"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
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
		if call.Established() {
			return true, types.NewErrorWithStatusCode(errors.New("Model not found"), "model_not_found", http.StatusNotFound)
		}
		return false, nil
	}
	if call.LegacyConnection {
		return true, types.NewErrorWithStatusCode(errors.New("Connection billing identity changed; reconnect required"), "connection_locked", http.StatusConflict)
	}
	if apiErr := authorizeWebSocketOffering(c, call.Request.Model); apiErr != nil {
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
		return true, types.NewErrorWithStatusCode(errors.New(run.message), types.ErrorCode(run.code), run.status, types.ErrOptionWithSkipRetry())
	}
	return true, nil
}

func authorizeWebSocketOffering(c *gin.Context, requested string) *types.NewAPIError {
	offering := middleware.TextOffering(c)
	group := common.GetContextKeyString(c, constant.ContextKeyUserGroup)
	if offering == nil || !offering.Enabled || offering.Visibility == "groups" && !slices.Contains(offering.Groups, group) {
		return types.NewErrorWithStatusCode(errors.New("Model not found"), "model_not_found", http.StatusNotFound)
	}
	if c.GetBool("token_model_limit_enabled") {
		raw, _ := c.Get("token_model_limit")
		limits, _ := raw.(map[string]bool)
		if !limits[requested] && !limits[offering.Name] {
			return types.NewErrorWithStatusCode(errors.New("Model not found"), "model_not_found", http.StatusNotFound)
		}
	}
	return nil
}

func runResponsesWSGatewayAttempt(c *gin.Context, call *relay.ResponsesWSCall, run *textGatewayRun, budget *service.GatewayAttemptBudget, exclusions *service.GatewayExclusions) (result service.GatewayAttemptResult) {
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
	// The projection is used only for shared capability admission. The native
	// Responses body, including state references, is sent without conversion.
	request := &dto.GeneralOpenAIRequest{Model: call.Request.Model, Stream: common.GetPointer(true), MaxCompletionTokens: call.Request.MaxOutputTokens}
	request.Messages = []dto.Message{{Role: "user", Content: string(call.Request.Input)}}
	if call.Native != nil {
		call.Native.Budget = budget
	}
	if call.Protocol() == protocol.OpenAIRealtime && billingexpr.UsesFixedPricingByHash(run.sale.Expression, billingexpr.ExprHashString(run.sale.Expression)) {
		run.fail(c, http.StatusBadRequest, "unsupported_pricing", "Fixed pricing is not supported for Realtime")
		return
	}
	var inputTokens *int
	if call.Native != nil && call.Native.InputUsage != nil {
		inputTokens = &call.Native.InputUsage.PromptTokens
	}
	selection, err := service.SelectGatewayProtocolChannel(c, offering, request, service.GatewayChannelRequirements{
		Protocol: call.Protocol(), ChannelID: call.LockedChannelID, Responses: &call.Request, ResponsesWebSocket: call.Protocol() == protocol.OpenAIResponses, InputTokens: inputTokens,
	}, exclusions)
	if err != nil {
		status, code := http.StatusServiceUnavailable, "channel_unavailable"
		if errors.Is(err, service.ErrGatewayCapability) {
			status, code = http.StatusBadRequest, "unsupported_parameter"
		}
		run.fail(c, status, code, "No Channel is available")
		return
	}
	if call.Native != nil {
		call.Native.Capabilities = selection.Capabilities
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
	var pinnedKey []int
	if call.LockedChannelID != 0 {
		pinnedKey = []int{call.LockedKeyIndex}
	}
	if apiErr := middleware.SetupContextForSelectedChannel(c, selection.Provider, selection.Channel.Model.ModelName, pinnedKey...); apiErr != nil {
		run.fail(c, http.StatusServiceUnavailable, "channel_unavailable", "No Channel is available")
		return
	}
	common.SetContextKey(c, constant.ContextKeyOriginalModel, call.Request.Model)
	info := relaycommon.GenRelayInfoResponses(c, &call.Request)
	info.InitChannelMeta(c)
	if call.Protocol() == protocol.OpenAIRealtime {
		info.RelayFormat, info.RelayMode = types.RelayFormatOpenAIRealtime, relayconstant.RelayModeRealtime
		info.InputAudioFormat, info.OutputAudioFormat = "pcm16", "pcm16"
	}
	info.BeginProviderCall = budget.Start
	info.UpstreamModelName, info.TokenizerModel = selection.Channel.UpstreamModelName, selection.Channel.Model.ModelName
	info.IsModelMapped, info.IsStream = true, true
	info.RequestId, info.StartTime = c.GetString(common.RequestIdKey), started
	info.StreamStatus = relaycommon.NewStreamStatus()
	var sentAt time.Time
	var firstByte time.Duration
	info.ResponseFacts = &relaycommon.ResponseFacts{OnBusinessOutput: func() {
		if firstByte == 0 && !sentAt.IsZero() {
			firstByte = max(clock.Now().Sub(sentAt), time.Nanosecond)
		}
		budget.Delivered()
	}}
	common.SetContextKey(c, constant.ContextKeyResponseFacts, info.ResponseFacts)
	saleInput, err := helper.ResolveIncomingBillingExprRequestInput(c, info)
	if err != nil {
		run.fail(c, http.StatusBadRequest, "invalid_request", "Invalid Responses request")
		return
	}
	// Channel overrides must not change the connection's native model identity.
	document := ojson.NewDoc(call.Body)
	if err = document.Set("model", info.UpstreamModelName); err != nil {
		run.fail(c, http.StatusBadRequest, "invalid_request", "Invalid Responses request")
		return
	}
	var overrides map[string]any
	if err = common.UnmarshalJsonStr(selection.Channel.ParameterOverrides, &overrides); err != nil {
		run.fail(c, http.StatusServiceUnavailable, "channel_unavailable", "Invalid Channel settings")
		return
	}
	info.ParamOverride = overrides
	body, err := relaycommon.ApplyParamOverrideWithRelayInfo(document.Bytes(), info)
	if err == nil && call.Protocol() == protocol.OpenAIRealtime {
		knownInput := 0
		if inputTokens != nil {
			knownInput = *inputTokens
		}
		body, err = helper.NormalizeRealtimeRequestLimits(body, selection.Capabilities, knownInput)
	}
	var effective dto.OpenAIResponsesRequest
	if err != nil || common.Unmarshal(body, &effective) != nil || effective.Model != info.UpstreamModelName || helper.ValidateResponsesMultipliers(body) != nil || !service.GatewayResponsesCapabilitiesMatch(&effective, selection.Capabilities, 0) {
		run.fail(c, http.StatusBadRequest, "unsupported_parameter", "Invalid Channel request parameters")
		return
	}
	// Count native input rather than treating max_output_tokens as usage.
	common.SetContextKey(c, constant.ContextKeyOriginalModel, info.TokenizerModel)
	estimatedInput := 0
	if inputTokens != nil {
		estimatedInput = *inputTokens
	} else {
		estimatedInput, err = service.CountRequestToken(c, effective.GetTokenCountMeta(), info)
	}
	if err != nil || !service.GatewayResponsesCapabilitiesMatch(&effective, selection.Capabilities, estimatedInput) {
		run.fail(c, http.StatusBadRequest, "unsupported_parameter", "Channel parameters exceed the request capabilities")
		return
	}
	if effective.MaxOutputTokens == nil && call.Protocol() == protocol.OpenAIResponses {
		var limit int64
		if maximum := selection.Capabilities.Limits.MaxOutputTokens; maximum != nil {
			limit = *maximum
		}
		if contextLength := selection.Capabilities.Limits.ContextLength; contextLength != nil {
			remaining := *contextLength - int64(estimatedInput)
			if limit == 0 {
				limit = remaining
			} else {
				limit = min(limit, remaining)
			}
		}
		if limit > 0 {
			document = ojson.NewDoc(body)
			if err = document.Set("max_output_tokens", limit); err != nil {
				run.fail(c, http.StatusBadRequest, "invalid_request", "Invalid Responses request")
				return
			}
			body = document.Bytes()
			effective.MaxOutputTokens = common.GetPointer(uint(limit))
		}
	}
	info.SetEstimatePromptTokens(estimatedInput)
	mean, err := model.RecentOfferingOutputTokens(c.Request.Context(), offering.ID, started.UnixMilli())
	if err != nil {
		run.fail(c, http.StatusServiceUnavailable, "billing_unavailable", "Billing is unavailable")
		return
	}
	if limit := effective.MaxOutputTokens; limit != nil {
		mean = min(mean, int64(*limit))
	}
	if limit := selection.Capabilities.Limits.MaxOutputTokens; limit != nil {
		mean = min(mean, *limit)
	}
	if limit := selection.Capabilities.Limits.ContextLength; limit != nil {
		mean = min(mean, *limit-int64(estimatedInput))
	}
	if mean < 0 {
		run.fail(c, http.StatusBadRequest, "unsupported_parameter", "Channel context limit exceeded")
		return
	}
	input := saleInput
	estimated := &dto.Usage{PromptTokens: estimatedInput, CompletionTokens: int(mean), TotalTokens: estimatedInput + int(mean)}
	if call.Protocol() == protocol.OpenAIRealtime {
		if call.Native.InputUsage != nil {
			estimated.PromptTokensDetails = call.Native.InputUsage.PromptTokensDetails.Clone()
		}
		outputs := ojson.GetBytes(body, "output_modalities")
		if !outputs.Exists() {
			outputs = ojson.GetBytes(body, "modalities")
		}
		call.Native.OutputAudio = !outputs.Exists() && slices.Contains(selection.Capabilities.OutputModalities, capability.Audio)
		for _, output := range outputs.Array() {
			if output.String() == "audio" {
				call.Native.OutputAudio = true
			}
		}
		if call.Native.OutputAudio {
			estimated.CompletionTokenDetails.AudioTokens = int(mean)
		}
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
	prepared, err := call.Prepare(c, info)
	if err != nil {
		run.fail(c, http.StatusServiceUnavailable, "channel_unavailable", "Upstream connection settings are unavailable")
		return
	}
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
	payload, err := call.CreatePayload(body)
	if err != nil {
		run.fail(c, http.StatusBadRequest, "invalid_request", "Invalid Responses request")
		return
	}
	purchaseInput := billingexpr.RequestInput{Body: payload, Headers: map[string]string{}}
	for key, values := range prepared.Headers() {
		purchaseInput.Headers[strings.ToLower(key)] = strings.Join(values, ",")
	}
	costEstimate, convertedEstimate, _, priceErr := service.GatewayPrice(purchase, estimated, purchaseInput, attemptStart, decimal.NewFromInt(1), purchaseFX)
	_, costErr := costEstimate.Cost()
	_, convertedErr := convertedEstimate.Cost()
	if priceErr != nil || costErr != nil || convertedErr != nil {
		run.fail(c, http.StatusServiceUnavailable, "price_unavailable", "Channel price is unavailable")
		return
	}
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
				additional, additionalErr := required.Sub(run.reservation)
				if additionalErr != nil {
					return additionalErr
				}
				wallet, walletErr := model.GetUserWallet(run.record.UserID)
				if walletErr != nil {
					return walletErr
				}
				if wallet.RequestsPaused || additional.Nanos > wallet.Balance.Nanos {
					return model.ErrInsufficientWalletBalance
				}
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
	attemptContext, err := budget.Start()
	if err != nil {
		run.fail(c, http.StatusServiceUnavailable, "budget_exhausted", "Upstream request failed")
		return
	}
	parent := c.Request
	c.Request = parent.WithContext(attemptContext)
	defer func() { c.Request = parent }()
	attempt := &model.GatewayAttempt{ID: common.NewRequestId(), RequestID: run.record.ID, ChannelID: selection.Channel.ID, ProviderID: selection.Channel.ProviderID, ModelID: selection.Channel.ModelID, MemberTier: 1,
		StartedAt: attemptStart.UnixMilli(), UsageSource: "none", PurchasePriceVersionID: purchase.ID, PurchaseExpression: purchase.Expression, AccountingFXRate: rate,
		Cost: money.Amount{Currency: purchase.Currency}, ConvertedCost: money.Amount{Currency: model.SettlementCurrency()}, RouteDiagnostics: selection.Diagnostics, ConversionLosses: []string{}}
	sentAt = clock.Now()
	usage, apiErr := call.Exchange(c, info, selection.Channel.ID, selection.Channel.ProviderID, prepared, payload)
	// The Attempt context may be expired while the client is still present.
	// Classify against the parent before deciding cancellation accounting.
	c.Request = parent
	attempt.EndedAt = clock.Now().UnixMilli()
	attempt.Status, attempt.ErrorClass = gatewayAttemptOutcome(c, info, apiErr)
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
	if call.TerminalDelivered() && !info.ResponseFacts.DownstreamWriteFailed {
		attempt.Status = "failed"
		if info.ResponseFacts.BusinessOutput {
			attempt.Status = "partial"
			if info.StreamStatus.CompletedNormally() {
				attempt.Status = "success"
			}
		}
	}
	if outcome.Response == relaycommon.ResponseOutcomeCancelled {
		attempt.Status = "cancelled"
	}
	if outcome.Response == relaycommon.ResponseOutcomeIncomplete && outcome.IncompleteReason == "max_output_tokens" && outcome.EndReason == relaycommon.StreamEndReasonDone && !outcome.HasErrors && !info.ResponseFacts.DownstreamWriteFailed && info.ResponseFacts.BusinessOutput && !cancelled {
		attempt.Status = "success"
	}
	if outcome.IncompleteReason == "content_filter" {
		code = "content_filter"
	}
	transportFailed := apiErr != nil || outcome.EndReason == relaycommon.StreamEndReasonScannerErr || outcome.EndReason == relaycommon.StreamEndReasonTimeout
	failure := service.ClassifyGatewayFailure(status, code, transportFailed, budget.TimedOut(), cancelled, info.ResponseFacts.DownstreamWriteFailed, errorType)
	if attempt.Status == "success" {
		failure = service.GatewayFailure{}
	}
	if attempt.Status != "success" {
		attempt.ErrorClass = failure.Class
	}
	recordGatewayProviderFailure(c, info, selection, failure)
	result = service.GatewayAttemptResult{Attempt: attempt, Failure: failure, Delivered: info.ResponseFacts.BusinessOutput, Established: call.Established(), HealthLease: selection.Admission.Lease, FirstByte: firstByte, RetryAfter: prepared.RetryAfter()}
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
		if !info.ResponseFacts.UsageReported && !info.ResponseFacts.PromptTokensReported && !info.ResponseFacts.CompletionTokensReported && !info.ResponseFacts.BusinessOutput && info.ResponseFacts.GeneratedText == "" {
			usage = nil
		}
	}
	if usage != nil {
		attempt.Usage, attempt.UsageSource = service.GatewayRecordUsage(usage), "estimated"
		if info.ResponseFacts.UsageReported {
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
			if attempt.Status == "partial" || !call.TerminalDelivered() {
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
		status := http.StatusBadGateway
		if failure.Class == "invalid_request" || failure.Class == "context_length" || failure.Class == "content_safety" {
			status = http.StatusBadRequest
		}
		run.fail(c, status, "upstream_error", "Upstream response failed")
	}
	return
}
