package controller

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/relay/channel"
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

func gatewayError(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": types.OpenAIError{Code: code, Type: "gateway_error", Message: message}})
}

type textGatewayRun struct {
	record               *model.GatewayRequest
	sale                 *model.PriceVersion
	saleFX               *money.Rate
	saleRate             string
	ratio                decimal.Decimal
	saleSource           money.Amount
	reservation          money.Amount
	evaluationFailed     bool
	saleEvaluationFailed bool
	saleUsage            *dto.Usage
	attemptUsage         map[string]*dto.Usage
	saleTrace            *billingexpr.MoneyTraceResult
	attemptTrace         map[string]billingexpr.MoneyTraceResult
	status               int
	code, message        string
}

func (run *textGatewayRun) fail(c *gin.Context, status int, code, message string) {
	run.status, run.code, run.message = status, code, message
}

func gatewayChannelPurchaseSnapshot(ctx context.Context, channelID int, at time.Time) (*model.PriceVersion, *money.Rate, string, error) {
	price, err := model.CurrentPriceVersion(ctx, model.DB, model.PriceOwnerChannel, channelID, at.Unix())
	if err != nil {
		return nil, nil, "", err
	}
	fx, rate, err := service.GatewayFX(ctx, price.Currency)
	return price, fx, rate, err
}

func gatewayPreparationRejection(err error, budget *service.GatewayAttemptBudget, parent context.Context, providerID int, requestID string) service.GatewayAttemptResult {
	var certificateFailure *tls.CertificateVerificationError
	if errors.As(err, &certificateFailure) {
		logger.LogError(parent, common.LogText("Provider credential security failure request=%q provider=%d category=certificate_verification_failed", requestID, providerID))
	}
	var result service.GatewayAttemptResult
	var rejection *types.NewAPIError
	if !errors.As(err, &rejection) {
		return result
	}
	native := rejection.ToOpenAIError()
	code, _ := native.Code.(string)
	result.Failure = service.ClassifyGatewayFailure(rejection.StatusCode, code, false, budget.TimedOut(), parent.Err() != nil, false, native.Type)
	if result.Failure.Class == "credentials" {
		logger.LogError(parent, common.LogText("Provider credential authentication rejected request=%q provider=%d category=credentials_rejected", requestID, providerID))
	}
	if result.Failure.ExcludeProvider {
		result.SkippedProvider = providerID
	}
	return result
}

// TextGateway uses one reservation and one settlement for all actual sends.
func TextGateway(c *gin.Context) {
	if middleware.TextOffering(c) == nil {
		c.Next()
		return
	}
	run := &textGatewayRun{}
	parent := c.Request
	attempts := service.RunGatewayAttempts(parent.Context(), service.GatewayPolicy(), func(budget *service.GatewayAttemptBudget, exclusions *service.GatewayExclusions) service.GatewayAttemptResult {
		previousStatus, previousCode, previousMessage := run.status, run.code, run.message
		run.status, run.code, run.message = 0, "", ""
		result := runTextGatewayAttempt(c, run, budget, exclusions)
		if result.Attempt == nil && result.SkippedChannel == 0 && run.code == "channel_unavailable" && previousCode != "" {
			run.status, run.code, run.message = previousStatus, previousCode, previousMessage
		}
		return result
	})
	c.Request = parent
	if run.record != nil {
		run.record.EndedAt = clock.Now().UnixMilli()
		if run.evaluationFailed {
			run.record.Status = "settlement_pending"
		}
		intent := model.GatewaySettlement{Request: *run.record, Attempts: attempts, SaleSource: run.saleSource, EvaluationFailed: run.evaluationFailed, SaleEvaluationFailed: run.saleEvaluationFailed, SaleUsage: run.saleUsage, AttemptUsage: run.attemptUsage}
		if run.evaluationFailed {
			intent.SaleTrace, intent.AttemptTrace = run.saleTrace, run.attemptTrace
		}
		if err := model.PrepareGatewaySettlement(intent); err != nil {
			logger.LogError(parent.Context(), "gateway settlement preparation failed for request "+run.record.ID)
		} else if _, err := model.SettleGateway(run.record.ID); err != nil {
			logger.LogError(parent.Context(), "gateway settlement pending for request "+run.record.ID)
		} else if err := model.PublishGatewayRecords(run.record.ID); err != nil {
			logger.LogError(parent.Context(), "gateway record delivery pending for request "+run.record.ID)
		}
	}
	if parent.Context().Err() != nil {
		return
	}
	if run.status != 0 {
		if strings.HasPrefix(c.Writer.Header().Get("Content-Type"), "text/event-stream") {
			_ = helper.ObjectData(c, gin.H{"error": types.OpenAIError{Code: run.code, Type: "gateway_error", Message: run.message}})
		} else if !c.Writer.Written() {
			gatewayError(c, run.status, run.code, run.message)
		}
	}
}

// TextGateway dispatches only a resolved ModelOffering. Absence continues to the
// legacy middleware; known disabled and unauthorized offerings stop here.
func runTextGatewayAttempt(c *gin.Context, run *textGatewayRun, budget *service.GatewayAttemptBudget, exclusions *service.GatewayExclusions) (result service.GatewayAttemptResult) {
	offering := middleware.TextOffering(c)
	if offering == nil {
		c.Next()
		return
	}
	c.Abort()
	userGroup := common.GetContextKeyString(c, constant.ContextKeyUserGroup)
	if !offering.Enabled || offering.Visibility == "groups" && !slices.Contains(offering.Groups, userGroup) {
		run.fail(c, http.StatusNotFound, "model_not_found", "Model not found")
		return
	}
	request, err := helper.GetAndValidateTextRequest(c, relayconstant.RelayModeChatCompletions)
	if err != nil {
		run.fail(c, http.StatusBadRequest, "invalid_request", "Invalid chat request")
		return
	}
	requestedName := request.Model
	if c.GetBool("token_model_limit_enabled") {
		limits, _ := c.Get("token_model_limit")
		allowed, _ := limits.(map[string]bool)
		if !allowed[requestedName] && !allowed[offering.Name] {
			run.fail(c, http.StatusNotFound, "model_not_found", "Model not found")
			return
		}
	}
	started := c.GetTime(middleware.TextGatewayStartedAtKey)
	if started.IsZero() {
		started = clock.Now()
	}
	sale, saleFX, saleRate, ratio := run.sale, run.saleFX, run.saleRate, run.ratio
	if sale == nil {
		sale, err = model.CurrentPriceVersion(c.Request.Context(), model.DB, model.PriceOwnerOffering, offering.ID, started.Unix())
		if err != nil {
			run.fail(c, http.StatusServiceUnavailable, "price_unavailable", "Model price is unavailable")
			return
		}
		saleFX, saleRate, err = service.GatewayFX(c.Request.Context(), sale.Currency)
		if err != nil {
			run.fail(c, http.StatusServiceUnavailable, "price_unavailable", "Model price is unavailable")
			return
		}
		ratio, err = service.ExactGatewayGroupRatio(userGroup)
		if err != nil {
			run.fail(c, http.StatusServiceUnavailable, "price_unavailable", "Model price is unavailable")
			return
		}
		run.sale, run.saleFX, run.saleRate, run.ratio = sale, saleFX, saleRate, ratio
	}
	selection, preparedChat, err := gatewayChatRoute(c, run, offering, request, started, exclusions)
	if err != nil {
		if errors.Is(err, service.ErrGatewayCapability) || errors.Is(err, service.ErrGatewayConversion) {
			run.fail(c, http.StatusBadRequest, "unsupported_parameter", "No Channel supports the requested capabilities")
			return
		}
		if errors.Is(err, service.ErrGatewayPriceUnavailable) {
			run.fail(c, http.StatusServiceUnavailable, "price_unavailable", "Channel price is unavailable")
			return
		}
		run.fail(c, http.StatusServiceUnavailable, "channel_unavailable", "No Channel is available")
		return
	}
	c.Header("X-Gateway-Conversion-Loss", strings.Join(selection.ConversionLosses, ","))
	contactedProvider := false
	defer func() {
		ctx := context.WithoutCancel(c.Request.Context())
		if result.Attempt != nil && result.Attempt.UsageSource != "none" {
			_ = selection.Permit.Release(ctx, contactedProvider, result.Attempt.Usage.TotalTokens)
		} else {
			_ = selection.Permit.Release(ctx, contactedProvider)
		}
	}()
	stopProbe := service.ChannelHealth.KeepAlive(c.Request.Context(), selection.Admission)
	defer stopProbe()
	defer func() {
		if result.Attempt == nil {
			_ = service.ChannelHealth.Release(context.WithoutCancel(c.Request.Context()), selection.Admission)
		}
		if result.Attempt == nil && (run.code == "unsupported_parameter" || run.code == "channel_unavailable" || run.code == "price_unavailable" || run.code == "upstream_error") {
			result.SkippedChannel = selection.Channel.ID
		}
	}()
	maps.Copy(c.Keys, preparedChat.Context.Keys)
	info := preparedChat.Info
	info.StartTime, info.RequestId = started, c.GetString(common.RequestIdKey)
	if info.RequestId == "" {
		info.RequestId = common.NewRequestId()
		c.Header(common.RequestIdKey, info.RequestId)
	}
	if run.record != nil {
		info.RequestId = run.record.ID
	}
	info.TokenizerModel = selection.Channel.Model.ModelName
	if info.StreamStatus == nil {
		info.StreamStatus = relaycommon.NewStreamStatus()
	}
	var sentAt time.Time
	var firstByte time.Duration
	info.ResponseFacts = &relaycommon.ResponseFacts{OnBusinessOutput: func() {
		if firstByte == 0 && !sentAt.IsZero() {
			firstByte = max(clock.Now().Sub(sentAt), time.Nanosecond)
		}
		budget.Delivered()
	}, CanFinishStream: func() bool { return info.StreamStatus.CompletedNormally() }}
	committedKey := false
	info.BeginProviderCall = func() (context.Context, error) {
		ctx, err := budget.Start()
		if err != nil {
			return nil, err
		}
		if !committedKey {
			if err := selection.Provider.CommitRouteKey(ctx, info.ApiKey, info.ChannelMultiKeyIndex); err != nil {
				return nil, err
			}
			committedKey = true
		} else if err := selection.Provider.ValidateRouteKey(ctx, info.ApiKey, info.ChannelMultiKeyIndex); err != nil {
			return nil, err
		}
		contactedProvider = true
		return ctx, nil
	}
	common.SetContextKey(c, constant.ContextKeyResponseFacts, info.ResponseFacts)
	input := preparedChat.SaleInput
	request = preparedChat.Request
	adaptor, converted, encoded := preparedChat.Adaptor, preparedChat.Converted, preparedChat.Encoded
	estimatedInput, estimatedUsage, choiceCount := selection.InputTokens, preparedChat.Usage, preparedChat.Choices
	sourceValue, reserveValue, _, err := service.GatewayPrice(sale, estimatedUsage, input, started, ratio, saleFX)
	if err != nil {
		run.fail(c, http.StatusServiceUnavailable, "price_unavailable", "Model price is unavailable")
		return
	}
	reserve, err := reserveValue.Receivable()
	if err != nil {
		run.fail(c, http.StatusServiceUnavailable, "price_unavailable", "Model price is unavailable")
		return
	}
	sourceReserve, err := sourceValue.Receivable()
	if err != nil {
		run.fail(c, http.StatusServiceUnavailable, "price_unavailable", "Model price is unavailable")
		return
	}
	record := &model.GatewayRequest{ID: info.RequestId, UserID: c.GetInt("id"), TokenID: c.GetInt("token_id"), ModelOfferingID: offering.ID, OfferingName: offering.Name, RequestedName: requestedName,
		ChoiceCount: choiceCount, SalePriceVersionID: sale.ID, SaleExpression: sale.Expression, SaleCurrency: sale.Currency, SaleFXRate: saleRate, GroupRatio: ratio.String(),
		Receivable: money.Amount{Currency: model.SettlementCurrency()}, StartedAt: started.UnixMilli(), UsageSource: "none", Status: "failed", LedgerEntryIDs: []string{}}
	if run.record == nil {
		if err := model.ReserveGateway(record, reserve, sourceReserve); err != nil {
			if errors.Is(err, model.ErrInsufficientWalletBalance) {
				run.fail(c, http.StatusPaymentRequired, "insufficient_balance", "Insufficient wallet balance")
				return
			}
			run.fail(c, http.StatusServiceUnavailable, "billing_unavailable", "Billing is unavailable")
			return
		}
		run.record, run.saleSource, run.reservation = record, sourceReserve, reserve
	} else {
		if reserve.Nanos > run.reservation.Nanos {
			run.fail(c, http.StatusServiceUnavailable, "channel_unavailable", "No Channel is available")
			return
		}
		record = run.record
	}
	parent := c.Request
	defer func() { c.Request = parent }()
	var upstreamStatus int
	var upstreamCode, upstreamType, retryAfter string
	var transportFailed bool
	var attempt *model.GatewayAttempt
	var usage *dto.Usage
	var attemptErr *types.NewAPIError
	var purchase *model.PriceVersion
	var purchaseFX *money.Rate
	var purchaseInput billingexpr.RequestInput
	defer func() {
		record.EndedAt = clock.Now().UnixMilli()
		if attempt == nil && parent.Context().Err() != nil {
			record.Status = "cancelled"
		}
		if attempt != nil {
			attempt.EndedAt = record.EndedAt
			attempt.Status, attempt.ErrorClass = gatewayAttemptOutcome(c, info, attemptErr)
			if budget.TimedOut() && parent.Context().Err() == nil {
				attempt.Status = "failed"
			}
			outcome := info.StreamStatus.OutcomeSnapshot()
			if outcome.ErrorCode != "" {
				upstreamCode = outcome.ErrorCode
			}
			if upstreamCode == "" {
				upstreamCode = info.ResponseFacts.UpstreamErrorCode
			}
			if upstreamType == "" {
				upstreamType = outcome.ErrorType
			}
			if outcome.ErrorStatus >= 400 {
				upstreamStatus = outcome.ErrorStatus
			}
			if info.PerformanceBusinessRejection {
				upstreamCode = "content_filter"
			}
			failure := service.ClassifyGatewayFailure(upstreamStatus, upstreamCode, transportFailed, budget.TimedOut(), parent.Context().Err() != nil, info.ResponseFacts.DownstreamWriteFailed, upstreamType)
			if attempt.Status == "success" {
				service.RecordGatewayChannelAffinity(c, selection)
				failure = service.GatewayFailure{}
			}
			if attempt.Status != "success" {
				attempt.ErrorClass = failure.Class
			}
			if info.IsStream && attempt.Status != "success" && failure.Class == "upstream_error" && !failure.Switch && !info.ResponseFacts.DownstreamWriteFailed && parent.Context().Err() == nil && upstreamStatus == 200 {
				failure = service.GatewayFailure{Class: "stream_interrupted", Switch: true}
				attempt.ErrorClass = failure.Class
			}
			gatewayAutoDisableProvider(c, selection, info, failure)
			result = service.GatewayAttemptResult{Attempt: attempt, Failure: failure, Delivered: info.ResponseFacts.BusinessOutput,
				FirstByte: firstByte, HealthLease: selection.Admission.Lease}
			if failure.Class == "rate_limited" {
				result.RetryAfter = retryAfter
				if result.RetryAfter == "" {
					result.RetryAfter = info.ResponseFacts.UpstreamRetryAfter
				}
			}

			record.Status = attempt.Status
			if !info.ResponseFacts.BusinessOutput && info.ResponseFacts.GeneratedText == "" && !info.ResponseFacts.UsageReported && !info.ResponseFacts.PromptTokensReported && !info.ResponseFacts.CompletionTokensReported {
				usage = nil
			}
			if usage == nil && (info.ResponseFacts.BusinessOutput || info.ResponseFacts.GeneratedText != "") {
				usage = &dto.Usage{PromptTokens: estimatedInput, CompletionTokens: tokenkit.Count(info.TokenizerModel, info.ResponseFacts.GeneratedText)}
			}
			if usage != nil {
				if !info.ResponseFacts.UsageReported && !info.ResponseFacts.PromptTokensReported && (info.ResponseFacts.BusinessOutput || info.ResponseFacts.GeneratedText != "") {
					usage.PromptTokens = estimatedInput
				}
				usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
				if run.attemptUsage == nil {
					run.attemptUsage = make(map[string]*dto.Usage)
				}
				run.attemptUsage[attempt.ID] = service.GatewayUsageSnapshot(usage)
				attempt.Usage, attempt.UsageSource = service.GatewayRecordUsage(usage), "estimated"
				if info.ResponseFacts.UsageReported {
					attempt.UsageSource = "upstream"
				}
				cost, converted, costTrace, priceErr := service.GatewayPrice(purchase, usage, purchaseInput, time.UnixMilli(attempt.StartedAt), decimal.NewFromInt(1), purchaseFX)
				if run.attemptTrace == nil {
					run.attemptTrace = make(map[string]billingexpr.MoneyTraceResult)
				}
				// Tier names can be computed from credential-bearing probes.
				costTrace.MatchedTier = ""
				run.attemptTrace[attempt.ID] = costTrace
				originalCost, originalErr := cost.Cost()
				attempt.CostPending = originalErr != nil || originalCost.Currency != purchase.Currency
				if !attempt.CostPending {
					attempt.Cost = originalCost
				}
				convertedCost, convertedErr := converted.Cost()
				attempt.ConvertedCostPending = convertedErr != nil || convertedCost.Currency != model.SettlementCurrency()
				if !attempt.ConvertedCostPending {
					attempt.ConvertedCost = convertedCost
				}
				if priceErr != nil || attempt.CostPending || attempt.ConvertedCostPending {
					run.evaluationFailed = true
					record.ErrorCode = "billing_evaluation_failed"
				}
			}
			if info.ResponseFacts.BusinessOutput {
				record.ChoiceCount = choiceCount
				saleUsage := usage
				record.Usage, record.UsageSource = attempt.Usage, attempt.UsageSource
				estimateGeneratedOutput := attempt.Status == "partial" || !info.ResponseFacts.UsageReported && (attempt.Status == "cancelled" || info.ResponseFacts.DownstreamWriteFailed)
				if info.IsStream && estimateGeneratedOutput && info.ResponseFacts.CompletionTokensReported && usage != nil {
					// Procurement retains reported usage; an interrupted delivery bills only generated text.
					delivered := tokenkit.Count(info.TokenizerModel, info.ResponseFacts.GeneratedText)
					if info.ResponseFacts.GeneratedOutputTokens != nil {
						delivered = *info.ResponseFacts.GeneratedOutputTokens
					}
					if delivered < usage.CompletionTokens {
						snapshot := *usage
						snapshot.CompletionTokens = delivered
						snapshot.TotalTokens = snapshot.PromptTokens + delivered
						snapshot.OutputTokens = delivered
						snapshot.CompletionTokenDetails = dto.OutputTokenDetails{}
						snapshot.OutputTokensDetails = nil
						saleUsage = &snapshot
						record.Usage, record.UsageSource = service.GatewayRecordUsage(saleUsage), "estimated"
					}
				}
				run.saleUsage = service.GatewayUsageSnapshot(saleUsage)
				source, receivable, trace, priceErr := service.GatewayPrice(sale, saleUsage, input, started, ratio, saleFX)
				trace.MatchedTier = ""
				run.saleTrace = &trace
				if attempt.Status != "success" && (trace.BillingUnit == billingexpr.BillingUnitRequest || billingexpr.UsesOnlyFixedPricing(sale.Expression)) {
					source, _ = money.ParseValue("0", sale.Currency)
					receivable, _ = money.ParseValue("0", model.SettlementCurrency())
					priceErr = nil
				}
				if priceErr == nil {
					record.Receivable, priceErr = receivable.Receivable()
				}
				if priceErr == nil {
					var sourceCharge money.Amount
					sourceCharge, priceErr = source.Receivable()
					if priceErr == nil {
						sourceReserve = sourceCharge
					}
				}
				if priceErr != nil {
					record.SalePending = true
					record.Receivable = money.Amount{Currency: model.SettlementCurrency()}
					run.evaluationFailed = true
					run.saleEvaluationFailed = true
					record.ErrorCode = "billing_evaluation_failed"
				}
			}
		}
		run.saleSource = sourceReserve

	}()
	if c.Request.Context().Err() != nil {
		record.Status = "cancelled"
		return
	}
	// Admission checks avoid Provider preparation I/O without a usable purchase price.
	if _, _, _, err = gatewayChannelPurchaseSnapshot(c.Request.Context(), selection.Channel.ID, clock.Now()); err != nil {
		run.fail(c, http.StatusServiceUnavailable, "price_unavailable", "Channel price is unavailable")
		return
	}

	info.SetEstimatePromptTokens(estimatedInput)
	encoded, err = adaptor.PrepareChatRequest(c, info, converted, encoded)
	if err != nil {
		result = gatewayPreparationRejection(err, budget, parent.Context(), selection.Channel.ProviderID, info.RequestId)
		run.fail(c, http.StatusBadGateway, "upstream_error", "Upstream request failed")
		return
	}
	prepared, err := adaptor.PrepareRequest(c, info, bytes.NewReader(encoded))
	if err != nil {
		result = gatewayPreparationRejection(err, budget, parent.Context(), selection.Channel.ProviderID, info.RequestId)
		run.fail(c, http.StatusBadGateway, "upstream_error", "Upstream request failed")
		return
	}
	// Preparation shares the budget but is not part of the model Attempt or its purchase snapshot.
	attemptStart := clock.Now()
	var purchaseRate string
	purchase, purchaseFX, purchaseRate, err = gatewayChannelPurchaseSnapshot(c.Request.Context(), selection.Channel.ID, attemptStart)
	if err != nil {
		run.fail(c, http.StatusServiceUnavailable, "price_unavailable", "Channel price is unavailable")
		return
	}
	purchaseInput = billingexpr.RequestInput{Body: append([]byte(nil), encoded...), Headers: make(map[string]string)}
	for key, values := range prepared.Headers() {
		purchaseInput.Headers[strings.ToLower(key)] = strings.Join(values, ",")
	}
	costEstimate, convertedEstimate, _, err := service.GatewayPrice(purchase, estimatedUsage, purchaseInput, attemptStart, decimal.NewFromInt(1), purchaseFX)
	if err != nil {
		run.fail(c, http.StatusServiceUnavailable, "price_unavailable", "Channel price is unavailable")
		return
	}
	if _, err = costEstimate.Cost(); err != nil {
		run.fail(c, http.StatusServiceUnavailable, "price_unavailable", "Channel price is unavailable")
		return
	}
	if _, err = convertedEstimate.Cost(); err != nil {
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
			selection.LossFallback, err = selection.LossFallbackAllowed(c.Request.Context(), comparison, offering.MinimumMarginRate)
		}
		if err != nil || !selection.LossFallback {
			run.fail(c, http.StatusServiceUnavailable, "channel_unavailable", "No Channel is available")
			return
		}
	}
	selection.Diagnostics["estimated_cost"] = comparison.String()
	selection.Diagnostics["loss_fallback"] = selection.LossFallback
	attempt = &model.GatewayAttempt{ID: common.NewRequestId(), RequestID: record.ID, ChannelID: selection.Channel.ID, ProviderID: selection.Channel.ProviderID, ModelID: selection.Channel.ModelID, MemberTier: selection.Tier,
		StartedAt: attemptStart.UnixMilli(), FailoverDecision: "stop", FailoverReason: "single_attempt", CircuitAction: "none", RouteDiagnostics: selection.Diagnostics,
		UsageSource: "none", PurchasePriceVersionID: purchase.ID, PurchaseExpression: purchase.Expression, Cost: money.Amount{Currency: purchase.Currency}, AccountingFXRate: purchaseRate,
		ConvertedCost: money.Amount{Currency: model.SettlementCurrency()}, ConversionLosses: selection.ConversionLosses}
	attemptContext, budgetErr := info.BeginProviderCall()
	if budgetErr != nil {
		attempt = nil
		if errors.Is(budgetErr, service.ErrGatewayBudget) || parent.Context().Err() != nil {
			run.fail(c, http.StatusServiceUnavailable, "budget_exhausted", "Upstream request failed")
		} else {
			run.fail(c, http.StatusServiceUnavailable, "channel_unavailable", "No Channel is available")
		}
		return
	}
	contactedProvider = true
	c.Request = parent.WithContext(attemptContext)
	defer budget.Delivered()
	sentAt = clock.Now()
	response, err := adaptor.DoPreparedRequest(c, info, prepared)
	if err != nil {
		transportFailed = true
		attemptErr = types.NewError(err, types.ErrorCodeDoRequestFailed)
		var rejection *types.NewAPIError
		if errors.As(err, &rejection) && rejection.GetErrorCode() != types.ErrorCodeDoRequestFailed && rejection.StatusCode >= 100 && (!budget.TimedOut() || rejection.StatusCode != 500) {
			attemptErr = rejection
			upstreamStatus, transportFailed = rejection.StatusCode, false
			failure := rejection.ToOpenAIError()
			upstreamCode, _ = failure.Code.(string)
			if upstreamCode == "" {
				upstreamCode = string(rejection.GetErrorCode())
			}
			upstreamType = failure.Type
		}
		run.fail(c, http.StatusBadGateway, "upstream_error", "Upstream request failed")
		return
	}
	httpResponse, ok := response.(*http.Response)
	if !ok || httpResponse == nil {
		attemptErr = types.NewError(errors.New("missing upstream response"), types.ErrorCodeBadResponse)
		run.fail(c, http.StatusBadGateway, "upstream_error", "Invalid upstream response")
		return
	}
	upstreamStatus = httpResponse.StatusCode
	retryAfter = httpResponse.Header.Get("Retry-After")
	if httpResponse.StatusCode != http.StatusOK {
		// Parse only bounded structured codes; provider text and URLs never leave memory.
		raw, readErr := io.ReadAll(io.LimitReader(httpResponse.Body, 1<<20))
		_ = httpResponse.Body.Close()
		buffered := io.NopCloser(bytes.NewReader(raw))
		if body, ok := httpResponse.Body.(channel.ReplayableResponseBody); ok {
			body.SetReader(buffered)
		} else {
			httpResponse.Body = buffered
		}
		var payload struct {
			Error any `json:"error"`
		}
		if readErr == nil && common.Unmarshal(raw, &payload) == nil {
			switch failure := payload.Error.(type) {
			case string:
				upstreamCode = failure
			case map[string]any:
				upstreamType, _ = failure["type"].(string)
				upstreamCode, _ = failure["code"].(string)
				if upstreamCode == "" {
					upstreamCode, _ = failure["type"].(string)
				}
			}
		}

		// Error bodies use the same protocol parser to preserve reported usage
		// without forwarding Provider error text or treating it as output.
		stream := info.IsStream
		info.IsStream = false
		responseUsage, responseFailure := adaptor.DoResponse(c, httpResponse, info)
		usage, _ = responseUsage.(*dto.Usage)
		if responseFailure != nil {
			failure := responseFailure.ToOpenAIError()
			if upstreamCode == "" {
				upstreamCode, _ = failure.Code.(string)
			}
			if upstreamType == "" {
				upstreamType = failure.Type
			}
		}
		info.IsStream = stream
		attemptErr = types.NewErrorWithStatusCode(errors.New("upstream request failed"), types.ErrorCodeBadResponse, httpResponse.StatusCode)
		status := httpResponse.StatusCode
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		run.fail(c, status, "upstream_error", "Upstream request failed")
		return
	}
	info.IsStream = info.IsStream || strings.HasPrefix(httpResponse.Header.Get("Content-Type"), "text/event-stream")
	if info.IsStream {
		httpResponse.Body = helper.GatewayStreamIdleBody(httpResponse.Body, info)
	}
	responseUsage, responseErr := adaptor.DoResponse(c, httpResponse, info)
	usage, _ = responseUsage.(*dto.Usage)
	attemptErr = responseErr
	if responseErr != nil {
		apiError := responseErr.ToOpenAIError()
		upstreamType = apiError.Type
		upstreamCode, _ = apiError.Code.(string)
		if upstreamCode == "" {
			upstreamCode = apiError.Type
		}
	}
	if status, _ := gatewayAttemptOutcome(c, info, responseErr); status != "success" {
		run.fail(c, http.StatusBadGateway, "upstream_error", "Upstream response failed")
	}
	if responseErr != nil && !c.Writer.Written() {
		run.fail(c, http.StatusBadGateway, "upstream_error", "Upstream response failed")
	}
	return
}

// gatewayAutoDisableProvider retains the selected credential and slot when an
// account or authentication rejection changes Provider status.
func gatewayAutoDisableProvider(c *gin.Context, selection *service.GatewaySelection, info *relaycommon.RelayInfo, failure service.GatewayFailure) {
	if !common.AutomaticDisableChannelEnabled || !selection.Provider.GetAutoBan() || failure.Class != "credentials" && failure.Class != "account_quota" {
		return
	}
	index := info.ChannelMultiKeyIndex
	providerError := types.ProviderError{ChannelId: selection.Channel.ProviderID, ChannelName: selection.Provider.Name,
		IsMultiKey: info.ChannelIsMultiKey, UsingKey: info.ApiKey, AutoBan: true}
	const reason = "Provider rejected authentication or account quota"
	targetScope := "key"
	if failure.Class == "account_quota" {
		targetScope = "provider"
	}
	changed := service.DisableGatewayProvider(providerError, reason, failure.Class == "account_quota", index)
	actorRole := c.GetInt("role")
	if actorRole == 0 {
		if actor, err := model.GetUserCache(c.GetInt("id")); err == nil {
			actorRole = actor.Role
		}
	}
	model.RecordAuditLog(c, model.AuditLog{UserId: c.GetInt("id"), ActorRole: actorRole, AuthMethod: "api_token", Category: model.AuditCategorySecurity,
		Action: "provider.auto_disable", Success: changed, Content: reason,
		Other: model.AuditOther{Op: &model.AuditOperation{Action: "provider.auto_disable"}, RootInfo: model.AuditFields{
			"provider_id": selection.Channel.ProviderID, "channel_id": selection.Channel.ID, "key_index": index, "error_class": failure.Class, "target_scope": targetScope}}})
}

// gatewayAttemptOutcome distinguishes accepted business content from protocol
// framing, downstream failures and incomplete upstream streams.
func gatewayAttemptOutcome(c *gin.Context, info *relaycommon.RelayInfo, err *types.NewAPIError) (string, string) {
	if c.Request.Context().Err() != nil {
		return "cancelled", "client_cancelled"
	}
	if info.ResponseFacts.DownstreamWriteFailed {
		return "failed", "downstream_write_failed"
	}
	if info.PerformanceBusinessRejection {
		if info.ResponseFacts.BusinessOutput {
			return "partial", "content_safety"
		}
		return "failed", "content_safety"
	}
	if err != nil {
		if info.ResponseFacts.BusinessOutput {
			return "partial", "upstream_error"
		}
		return "failed", "upstream_error"
	}
	if info.IsStream {
		if !info.StreamStatus.CompletedNormally() {
			if info.ResponseFacts.BusinessOutput {
				return "partial", "stream_interrupted"
			}
			return "failed", "stream_interrupted"
		}
	}
	if !info.ResponseFacts.BusinessOutput {
		return "failed", "empty_response"
	}
	return "success", ""
}
