package controller

import (
	"bytes"
	"context"
	"errors"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/tokenkit"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

type mediaGatewayRun struct {
	c                                      *gin.Context
	offering                               *model.ModelOffering
	parsed                                 *helper.GatewayMediaRequest
	started                                time.Time
	id                                     string
	name                                   string
	sale                                   *model.PriceVersion
	saleFX                                 *money.Rate
	saleRate                               string
	ratio                                  decimal.Decimal
	record                                 *model.GatewayRequest
	reservation, saleSource                money.Amount
	status                                 int
	code                                   string
	evaluationFailed, saleEvaluationFailed bool
	saleUsage                              *dto.Usage
	attemptUsage                           map[string]*dto.Usage
	saleTrace                              *billingexpr.MoneyTraceResult
	attemptTrace                           map[string]billingexpr.MoneyTraceResult
}

// MediaGateway composes authenticated synchronous image/audio protocols with
// the shared attempt budget and the single GatewayBilling money lifecycle.
func MediaGateway(c *gin.Context) {
	switch c.Request.URL.Path {
	case "/v1/images/generations", "/v1/images/edits", "/v1/audio/speech", "/v1/audio/transcriptions":
	default:
		c.Next()
		return
	}
	offering := middleware.TextOffering(c)
	if offering == nil {
		c.Next()
		return
	}
	c.Abort()
	defer func() {
		if c.Request.MultipartForm != nil {
			_ = c.Request.MultipartForm.RemoveAll()
		}
	}()
	userGroup := common.GetContextKeyString(c, constant.ContextKeyUserGroup)
	if !offering.Enabled || offering.Visibility == "groups" && !slices.Contains(offering.Groups, userGroup) {
		gatewayError(c, 404, "model_not_found", i18n.T(c, i18n.MsgGatewayModelNotFound))
		return
	}
	parsed, err := helper.ParseGatewayMediaRequest(c)
	if err != nil {
		gatewayError(c, 400, "invalid_request", i18n.T(c, i18n.MsgGatewayInvalidMediaRequest))
		return
	}
	started := c.GetTime(middleware.TextGatewayStartedAtKey)
	if started.IsZero() {
		started = clock.Now()
	}
	priceMessage := i18n.T(c, i18n.MsgGatewayPriceUnavailable)
	sale, err := model.CurrentPriceVersion(c.Request.Context(), model.DB, model.PriceOwnerOffering, offering.ID, started.Unix())
	if err != nil {
		gatewayError(c, 503, "price_unavailable", priceMessage)
		return
	}
	fx, rate, err := service.GatewayFX(c.Request.Context(), sale.Currency)
	if err != nil {
		gatewayError(c, 503, "price_unavailable", priceMessage)
		return
	}
	ratio, err := service.ExactGatewayGroupRatio(common.GetContextKeyString(c, constant.ContextKeyUserGroup))
	if err != nil {
		gatewayError(c, 503, "price_unavailable", priceMessage)
		return
	}
	id := c.GetString(common.RequestIdKey)
	if id == "" {
		id = common.NewRequestId()
		c.Set(common.RequestIdKey, id)
		c.Header(common.RequestIdKey, id)
	}
	name := offering.Name
	switch request := parsed.Request.(type) {
	case *dto.ImageRequest:
		name = request.Model
	case *dto.AudioRequest:
		name = request.Model
	}
	if c.GetBool("token_model_limit_enabled") {
		limits, _ := c.Get("token_model_limit")
		allowed, _ := limits.(map[string]bool)
		if !allowed[name] && !allowed[offering.Name] {
			gatewayError(c, 404, "model_not_found", i18n.T(c, i18n.MsgGatewayModelNotFound))
			return
		}
	}
	run := &mediaGatewayRun{c: c, name: name, offering: offering, parsed: parsed, started: started, id: id, sale: sale, saleFX: fx, saleRate: rate, ratio: ratio, status: 503, code: "channel_unavailable", saleSource: money.Amount{Currency: sale.Currency}}
	attempts := service.RunGatewayAttempts(c.Request.Context(), service.GatewayPolicy(), run.attempt)
	if run.record != nil {
		run.record.EndedAt = clock.Now().UnixMilli()
		if run.record.Status != "success" && run.record.ErrorCode != "billing_evaluation_failed" {
			run.record.ErrorCode = run.code
		}
		if run.evaluationFailed {
			run.record.Status = "settlement_pending"
		}
		intent := model.GatewaySettlement{Request: *run.record, Attempts: attempts, SaleSource: run.saleSource,
			EvaluationFailed: run.evaluationFailed, SaleEvaluationFailed: run.saleEvaluationFailed,
			SaleUsage: run.saleUsage, AttemptUsage: run.attemptUsage}
		if run.evaluationFailed {
			intent.SaleTrace, intent.AttemptTrace = run.saleTrace, run.attemptTrace
		}
		if err := model.PrepareGatewaySettlement(intent); err != nil {
			logger.LogError(c.Request.Context(), common.LogText("media gateway settlement preparation failed for request %s", id))
		} else if _, err := model.SettleGateway(id); err != nil {
			logger.LogError(c.Request.Context(), common.LogText("media gateway settlement pending for request %s", id))
		} else if err := model.PublishGatewayRecords(id); err != nil {
			logger.LogError(c.Request.Context(), common.LogText("media gateway record delivery pending for request %s", id))
		}
	}
	if run.status != 0 && c.Request.Context().Err() == nil {
		message := i18n.T(c, i18n.MsgGatewayMediaRequestFailed)
		if c.Writer.Written() && strings.HasPrefix(c.Writer.Header().Get("Content-Type"), "text/event-stream") {
			_ = helper.ObjectData(c, gin.H{"error": types.OpenAIError{Code: run.code, Type: "gateway_error", Message: message}})
		} else if !c.Writer.Written() {
			gatewayError(c, run.status, run.code, message)
		}
	}
}

func (run *mediaGatewayRun) attempt(budget *service.GatewayAttemptBudget, exclusions *service.GatewayExclusions) (result service.GatewayAttemptResult) {
	c := run.c
	selection, err := service.SelectGatewayMediaChannel(c, run.offering, run.parsed.Protocol, run.parsed.Request, exclusions.Channels, exclusions.Providers)
	if err != nil {
		if errors.Is(err, service.ErrGatewayCapability) {
			run.status, run.code = 400, "unsupported_parameter"
		}
		return result
	}
	stopProbe := service.ChannelHealth.KeepAlive(c.Request.Context(), selection.Admission)
	defer stopProbe()
	defer func() {
		if result.Attempt == nil {
			_ = service.ChannelHealth.Release(context.WithoutCancel(c.Request.Context()), selection.Admission)
		}
	}()
	skipped := service.GatewayAttemptResult{SkippedChannel: selection.Channel.ID}
	if setupErr := middleware.SetupContextForSelectedChannel(c, selection.Provider, selection.Channel.Model.ModelName); setupErr != nil {
		return skipped
	}
	var overrides map[string]any
	if common.UnmarshalJsonStr(selection.Channel.ParameterOverrides, &overrides) != nil {
		return skipped
	}
	common.SetContextKey(c, constant.ContextKeyChannelParamOverride, overrides)
	var info *relaycommon.RelayInfo
	if run.parsed.Protocol == protocol.OpenAIImages {
		info = relaycommon.GenRelayInfoImage(c, run.parsed.Request)
	} else {
		info = relaycommon.GenRelayInfoOpenAIAudio(c, run.parsed.Request)
	}
	info.InitChannelMeta(c)
	info.StartTime, info.RequestId = run.started, run.id
	info.UpstreamModelName, info.OriginModelName = selection.Channel.UpstreamModelName, selection.Channel.UpstreamModelName
	info.TokenizerModel = selection.Channel.Model.ModelName
	info.IsModelMapped = true
	info.StreamStatus = relaycommon.NewStreamStatus()
	var sentAt time.Time
	var firstByte time.Duration
	info.ResponseFacts = &relaycommon.ResponseFacts{OnBusinessOutput: func() {
		if firstByte == 0 && !sentAt.IsZero() {
			firstByte = max(clock.Now().Sub(sentAt), time.Nanosecond)
		}
		budget.Delivered()
	}, CanFinishStream: func() bool { return info.StreamStatus.CompletedNormally() }}
	info.BeginProviderCall = budget.Start
	common.SetContextKey(c, constant.ContextKeyResponseFacts, info.ResponseFacts)
	common.SetContextKey(c, constant.ContextKeyOriginalModel, info.TokenizerModel)
	if helper.ValidateGatewayMediaMultipliers(run.parsed.Input.Body, selection.Capabilities.Limits) != nil {
		run.status, run.code = 400, "unsupported_parameter"
		return skipped
	}
	adaptor, err := relay.GetChannelAdaptor(info.ApiType, run.parsed.Protocol)
	if err != nil {
		run.status, run.code = 400, "unsupported_parameter"
		return skipped
	}
	prepared, err := adaptor.PrepareMediaWire(c, info, run.parsed)
	if err != nil {
		run.status, run.code = 400, "unsupported_parameter"
		return skipped
	}
	meta := prepared.Request.GetTokenCountMeta()
	if audio, ok := prepared.Request.(*dto.AudioRequest); ok && audio.Instructions != "" {
		meta.CombineText += "\n" + audio.Instructions
	}
	inputTokens, err := service.CountRequestToken(c, meta, info)
	if err != nil || !service.GatewayMediaCapabilitiesMatch(c, run.parsed.Protocol, prepared.Request, selection.Capabilities, inputTokens, info.ResponseFacts) {
		run.status, run.code = 400, "unsupported_parameter"
		return skipped
	}
	limits := selection.Capabilities.Limits
	if limits.ContextLength != nil {
		remaining := *limits.ContextLength - int64(inputTokens)
		if limits.MaxOutputTokens == nil || *limits.MaxOutputTokens > remaining {
			limits.MaxOutputTokens = &remaining
		}
	}
	if audio, ok := prepared.Request.(*dto.AudioRequest); ok && run.parsed.Protocol == protocol.OpenAIAudioSpeech && len(audio.MaxNewTokens) == 0 && limits.MaxOutputTokens != nil {
		if err := adaptor.SetMediaOutputLimit(prepared, *limits.MaxOutputTokens); err != nil {
			run.status, run.code = 400, "unsupported_parameter"
			return skipped
		}
	}
	if len(prepared.Input.Body) > 0 && helper.ValidateGatewayMediaMultipliers(prepared.Input.Body, limits) != nil {
		run.status, run.code = 400, "unsupported_parameter"
		return skipped
	}
	info.SetEstimatePromptTokens(inputTokens)
	mean, err := model.RecentOfferingOutputTokens(c.Request.Context(), run.offering.ID, run.started.UnixMilli())
	if err != nil {
		run.code = "billing_unavailable"
		return result
	}
	if limit := limits.MaxOutputTokens; limit != nil {
		mean = min(mean, *limit)
	}
	if audio, ok := prepared.Request.(*dto.AudioRequest); ok && len(audio.MaxNewTokens) > 0 {
		var limit uint
		if common.Unmarshal(audio.MaxNewTokens, &limit) != nil {
			run.status, run.code = 400, "unsupported_parameter"
			return skipped
		}
		mean = min(mean, int64(limit))
	}
	count := 1
	if prepared.Input.ImageCount != nil {
		count = *prepared.Input.ImageCount
	}
	if mean > int64(common.MaxQuota)/int64(count) {
		run.status, run.code = 400, "unsupported_parameter"
		return skipped
	}
	mean *= int64(count)
	estimatedUsage := &dto.Usage{PromptTokens: inputTokens, CompletionTokens: int(mean), TotalTokens: inputTokens + int(mean)}
	if run.parsed.Protocol == protocol.OpenAIAudioSpeech {
		estimatedUsage.CompletionTokenDetails.AudioTokens = int(mean)
	}
	if run.parsed.Protocol == protocol.OpenAIAudioTranscription {
		estimatedUsage.PromptTokensDetails.AudioTokens = inputTokens
	}
	saleInput := run.parsed.Input
	saleInput.ImageCount = prepared.Input.ImageCount
	source, reserveValue, _, err := service.GatewayPrice(run.sale, estimatedUsage, saleInput, run.started, run.ratio, run.saleFX)
	if err != nil {
		run.code = "price_unavailable"
		return result
	}
	reserve, err := reserveValue.Receivable()
	if err != nil {
		run.code = "price_unavailable"
		return result
	}
	sourceReserve, err := source.Receivable()
	if err != nil {
		run.code = "price_unavailable"
		return result
	}
	if _, _, _, err := gatewayChannelPurchaseSnapshot(c.Request.Context(), selection.Channel.ID, clock.Now()); err != nil {
		run.code = "price_unavailable"
		return skipped
	}
	if run.record == nil {
		run.record = &model.GatewayRequest{ID: run.id, UserID: c.GetInt("id"), TokenID: c.GetInt("token_id"), ModelOfferingID: run.offering.ID, OfferingName: run.offering.Name, RequestedName: run.name, ChoiceCount: count, SalePriceVersionID: run.sale.ID, SaleExpression: run.sale.Expression, SaleCurrency: run.sale.Currency, SaleFXRate: run.saleRate, GroupRatio: run.ratio.String(), Receivable: money.Amount{Currency: model.SettlementCurrency()}, StartedAt: run.started.UnixMilli(), Status: "failed", UsageSource: "none", LedgerEntryIDs: []string{}}
		if err := model.ReserveGateway(run.record, reserve, sourceReserve); err != nil {
			run.record = nil
			if errors.Is(err, model.ErrInsufficientWalletBalance) {
				run.status, run.code = 402, "insufficient_balance"
			} else {
				run.code = "billing_unavailable"
			}
			return result
		}
		run.reservation = reserve
	} else if reserve.Nanos > run.reservation.Nanos {
		run.code = "reservation_exceeded"
		return skipped
	}
	if c.Request.Context().Err() != nil {
		run.record.Status = "cancelled"
		run.code = "client_cancelled"
		return result
	}
	parent := c.Request
	defer func() { c.Request = parent }()
	c.Request.Header.Set("Content-Type", prepared.ContentType)
	prepared.Prepared, err = adaptor.PrepareRequest(c, info, bytes.NewReader(prepared.Body))
	if err != nil {
		run.code = "upstream_error"
		result = gatewayPreparationRejection(err, budget, parent.Context(), selection.Channel.ProviderID, run.id)
		if result.SkippedProvider == 0 && result.Failure.Class == "" {
			result.SkippedChannel = selection.Channel.ID
		}
		return result
	}
	prepared.Input.Headers = make(map[string]string)
	for key, values := range prepared.Prepared.Headers() {
		prepared.Input.Headers[strings.ToLower(key)] = strings.Join(values, ",")
	}
	at := clock.Now()
	purchase, purchaseFX, purchaseRate, err := gatewayChannelPurchaseSnapshot(c.Request.Context(), selection.Channel.ID, at)
	if err != nil {
		run.code = "price_unavailable"
		return skipped
	}
	estimatedCost, estimatedConverted, _, err := service.GatewayPrice(purchase, estimatedUsage, prepared.Input, at, decimal.NewFromInt(1), purchaseFX)
	if err != nil {
		run.code = "price_unavailable"
		return skipped
	}
	if _, err := estimatedCost.Cost(); err != nil {
		run.code = "price_unavailable"
		return skipped
	}
	if _, err := estimatedConverted.Cost(); err != nil {
		run.code = "price_unavailable"
		return skipped
	}
	ctx, err := budget.Start()
	if err != nil {
		run.code = "first_byte_budget"
		return result
	}
	c.Request = parent.WithContext(ctx)
	attempt := &model.GatewayAttempt{ID: common.NewRequestId(), RequestID: run.id, ChannelID: selection.Channel.ID, ProviderID: selection.Provider.Id, ModelID: selection.Channel.ModelID, StartedAt: at.UnixMilli(), PurchasePriceVersionID: purchase.ID, PurchaseExpression: purchase.Expression, AccountingFXRate: purchaseRate, Cost: money.Amount{Currency: purchase.Currency}, ConvertedCost: money.Amount{Currency: model.SettlementCurrency()}, UsageSource: "none", RouteDiagnostics: selection.Diagnostics, CircuitAction: "none", ConversionLosses: []string{}}
	for _, member := range run.offering.Members {
		if member.ModelID == selection.Channel.ModelID {
			attempt.MemberTier = member.Tier
			break
		}
	}
	if attempt.MemberTier > 1 {
		first := 0
		for _, member := range run.offering.Members {
			if member.Tier == 1 {
				first = member.ModelID
				break
			}
		}
		var firstModel model.Model
		if first > 0 && model.DB.WithContext(c.Request.Context()).First(&firstModel, first).Error == nil {
			c.Header("X-Model-Fallback", firstModel.ModelName+"->"+selection.Channel.Model.ModelName)
		}
	}
	var usage *dto.Usage
	var apiErr *types.NewAPIError
	status, code, transport := 0, "", false
	retryAfter := ""
	defer budget.Delivered()
	sentAt = clock.Now()
	response, err := adaptor.DoPreparedRequest(c, info, prepared.Prepared)
	if err != nil {
		transport = true
		apiErr = types.NewError(err, types.ErrorCodeDoRequestFailed)
	} else if upstream, ok := response.(*http.Response); !ok || upstream == nil {
		apiErr = types.NewError(errors.New("missing media response"), types.ErrorCodeBadResponse)
	} else {
		status = upstream.StatusCode
		retryAfter = upstream.Header.Get("Retry-After")
		if info.IsStream {
			upstream.Body = helper.GatewayStreamIdleBody(upstream.Body, info)
		}
		value, responseErr := adaptor.DoResponse(c, upstream, info)
		usage, _ = value.(*dto.Usage)
		apiErr = responseErr
	}
	if usage == nil {
		usage = &dto.Usage{}
	}
	errorType := ""
	if apiErr != nil {
		envelope := apiErr.ToOpenAIError()
		code, _ = envelope.Code.(string)
		errorType = envelope.Type
		if status < 400 && apiErr.StatusCode >= 400 {
			status = apiErr.StatusCode
		}
	}
	if info.StreamStatus != nil {
		outcome := info.StreamStatus.OutcomeSnapshot()
		if outcome.ErrorCode != "" {
			code = outcome.ErrorCode
		}
		if outcome.ErrorType != "" {
			errorType = outcome.ErrorType
		}
		if outcome.ErrorStatus >= 400 {
			status = outcome.ErrorStatus
		}
		if outcome.EndReason == relaycommon.StreamEndReasonPingFail {
			info.ResponseFacts.DownstreamWriteFailed = true
		}
		if status == http.StatusOK && code == "" && outcome.Response == relaycommon.ResponseOutcomeIncomplete && !budget.TimedOut() && parent.Context().Err() == nil && !info.ResponseFacts.DownstreamWriteFailed {
			code = "stream_interrupted"
		}
		transport = transport || outcome.EndReason == relaycommon.StreamEndReasonScannerErr
	}
	facts := info.ResponseFacts
	generated := facts.BusinessOutput || facts.GeneratedImageCount > 0 || facts.GeneratedAudioBytes > 0 || facts.GeneratedText != "" || facts.ImageCount != nil && *facts.ImageCount > 0
	if !facts.PromptTokensReported && !facts.AudioInputDurationReported && (generated || facts.CompletionTokensReported || facts.AudioInputDurationInvalid) {
		usage.PromptTokens = inputTokens
		if run.parsed.Protocol == protocol.OpenAIAudioTranscription {
			usage.PromptTokensDetails.AudioTokens = inputTokens
		}
	}
	if !facts.CompletionTokensReported && facts.GeneratedText != "" {
		usage.CompletionTokens = tokenkit.Count(info.TokenizerModel, facts.GeneratedText)
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	attempt.Usage = service.GatewayRecordUsage(usage, facts)
	if info.QuotaClamp != nil {
		attempt.RouteDiagnostics["quota_saturation"] = info.QuotaClamp.AuditMap()
	}
	if generated || facts.PromptTokensReported || facts.CompletionTokensReported || facts.AudioInputDurationReported || facts.AudioInputDurationInvalid {
		attempt.UsageSource = "estimated"
	}
	if facts.UsageReported {
		attempt.UsageSource = "upstream"
	}
	actualCount := []int{}
	if facts.ImageCount != nil {
		actualCount = append(actualCount, int(*facts.ImageCount))
	}
	if run.attemptUsage == nil {
		run.attemptUsage = make(map[string]*dto.Usage)
	}
	run.attemptUsage[attempt.ID] = service.GatewayUsageSnapshot(usage)
	if attempt.UsageSource != "none" {
		cost, converted, trace, priceErr := service.GatewayPrice(purchase, usage, prepared.Input, at, decimal.NewFromInt(1), purchaseFX, actualCount...)
		trace.MatchedTier = ""
		if run.attemptTrace == nil {
			run.attemptTrace = make(map[string]billingexpr.MoneyTraceResult)
		}
		run.attemptTrace[attempt.ID] = trace
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
			run.record.ErrorCode = "billing_evaluation_failed"
		}
	}
	attempt.EndedAt = clock.Now().UnixMilli()
	attempt.Status, attempt.ErrorClass = gatewayAttemptOutcome(c, info, apiErr)
	if status != 0 && (status < 200 || status >= 300) && parent.Context().Err() == nil {
		attempt.Status = "failed"
	}
	if budget.TimedOut() && parent.Context().Err() == nil {
		attempt.Status = "failed"
	}
	failure := service.ClassifyGatewayFailure(status, code, transport, budget.TimedOut(), parent.Context().Err() != nil, facts.DownstreamWriteFailed, errorType)
	if attempt.Status == "success" {
		failure = service.GatewayFailure{}
		if run.record.ErrorCode != "billing_evaluation_failed" {
			run.record.ErrorCode = ""
		}
	} else {
		attempt.ErrorClass = failure.Class
	}
	gatewayAutoDisableProvider(c, selection, info, failure)
	run.record.Status = attempt.Status
	if facts.BusinessOutput {
		run.record.ChoiceCount = count
		run.record.Usage = attempt.Usage
		run.record.UsageSource = attempt.UsageSource
	} else {
		billable := *facts
		if facts.ImageCount != nil {
			zero := int64(0)
			billable.ImageCount = &zero
		}
		run.record.Usage = service.GatewayRecordUsage(&dto.Usage{}, &billable)
		run.record.UsageSource = "none"
	}
	if facts.BusinessOutput {
		saleUsage := usage
		billableFacts := *facts
		saleCount := actualCount
		if info.IsStream && run.parsed.Protocol == protocol.OpenAIImages && facts.DownstreamWriteFailed && !facts.UsageReported && facts.ImageCount != nil {
			delivered := min(facts.DeliveredImageCount, *facts.ImageCount)
			billableFacts.ImageCount = &delivered
			saleCount = []int{int(delivered)}
			run.record.UsageSource = "estimated"
		}
		estimateDelivered := attempt.Status == "partial" || !facts.UsageReported && (attempt.Status == "cancelled" || facts.DownstreamWriteFailed)
		if info.IsStream && run.parsed.Protocol == protocol.OpenAIAudioSpeech && estimateDelivered {
			output, clamp := common.QuotaRoundChecked(math.Ceil(float64(facts.DeliveredAudioBytes) / 1000))
			if audio, ok := prepared.Request.(*dto.AudioRequest); ok && audio.ResponseFormat == "pcm" {
				output, clamp = common.AudioDurationTokens(float64(facts.DeliveredAudioBytes) / 48000)
				seconds := decimal.NewFromInt(facts.DeliveredAudioBytes).Div(decimal.NewFromInt(48000)).String()
				billableFacts.AudioOutputSeconds = &seconds
			}
			if clamp != nil {
				attempt.RouteDiagnostics["quota_saturation"] = clamp.AuditMap()
			}
			output = min(output, usage.CompletionTokens)
			if output < usage.CompletionTokens {
				snapshot := *usage
				snapshot.CompletionTokens, snapshot.OutputTokens = output, output
				snapshot.TotalTokens = snapshot.PromptTokens + output
				snapshot.CompletionTokenDetails = dto.OutputTokenDetails{AudioTokens: output}
				if facts.AudioOutputTokensReported {
					snapshot.CompletionTokenDetails.AudioTokens = min(output, usage.CompletionTokenDetails.AudioTokens)
				}
				snapshot.OutputTokensDetails = nil
				saleUsage = &snapshot
				run.record.UsageSource = "estimated"
			}
		}
		run.record.Usage = service.GatewayRecordUsage(saleUsage, &billableFacts)
		run.saleUsage = service.GatewayUsageSnapshot(saleUsage)
		source, receivable, trace, priceErr := service.GatewayPrice(run.sale, saleUsage, saleInput, run.started, run.ratio, run.saleFX, saleCount...)
		trace.MatchedTier = ""
		run.saleTrace = &trace
		fixedRequest := trace.BillingUnit == billingexpr.BillingUnitRequest && !trace.FixedPricePerImage
		if trace.FixedPrice == nil {
			fixedRequest = billingexpr.UsesOnlyFixedRequestPricing(run.sale.Expression)
		}
		if attempt.Status != "success" && fixedRequest {
			source, _ = money.ParseValue("0", run.sale.Currency)
			receivable, _ = money.ParseValue("0", model.SettlementCurrency())
			priceErr = nil
		}
		if priceErr == nil {
			run.record.Receivable, priceErr = receivable.Receivable()
		}
		if priceErr == nil {
			run.saleSource, priceErr = source.Receivable()
		}
		if priceErr != nil {
			run.record.Receivable = money.Amount{Currency: model.SettlementCurrency()}
			run.record.SalePending = true
			run.evaluationFailed, run.saleEvaluationFailed = true, true
			run.record.ErrorCode = "billing_evaluation_failed"
		}
	}
	run.status, run.code = 0, ""
	if attempt.Status != "success" {
		run.status, run.code = status, failure.Class
		if run.code == "" {
			run.code = "upstream_error"
		}
		if run.status < 400 || run.status > 599 {
			run.status = 502
		}
	}
	if attempt.Status != "success" && run.record.ErrorCode == "" {
		run.record.ErrorCode = attempt.ErrorClass
	}
	result = service.GatewayAttemptResult{Attempt: attempt, Failure: failure, Delivered: facts.BusinessOutput,
		FirstByte: firstByte, HealthLease: selection.Admission.Lease}
	if failure.Class == "rate_limited" {
		result.RetryAfter = retryAfter
		if result.RetryAfter == "" {
			result.RetryAfter = facts.UpstreamRetryAfter
		}
	}
	return result
}
