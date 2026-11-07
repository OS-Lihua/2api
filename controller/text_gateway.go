package controller

import (
	"bytes"
	"errors"
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

func gatewayError(c *gin.Context, status int, code, message string) {
	kind, _ := middleware.TextGatewayEndpoint(c.Request.Method, c.Request.URL.Path)
	switch kind {
	case protocol.AnthropicMessages:
		c.AbortWithStatusJSON(status, gin.H{"type": "error", "error": gin.H{"type": code, "message": message}})
	case protocol.GeminiGenerate:
		googleStatus := "UNAVAILABLE"
		if status == http.StatusBadRequest {
			googleStatus = "INVALID_ARGUMENT"
		}
		if status == http.StatusNotFound {
			googleStatus = "NOT_FOUND"
		}
		c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"code": status, "status": googleStatus, "message": message, "details": []gin.H{{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": code, "domain": "gateway"}}}})
	default:
		c.AbortWithStatusJSON(status, gin.H{"error": types.OpenAIError{Code: code, Type: "gateway_error", Message: message}})
	}
}

// TextGateway dispatches only a resolved ModelOffering. Absence continues to the
// legacy middleware; known disabled and unauthorized offerings stop here.
func TextGateway(c *gin.Context) {
	offering := middleware.TextOffering(c)
	if offering == nil {
		c.Next()
		return
	}
	c.Abort()
	userGroup := common.GetContextKeyString(c, constant.ContextKeyUserGroup)
	if !offering.Enabled || offering.Visibility == "groups" && !slices.Contains(offering.Groups, userGroup) {
		gatewayError(c, http.StatusNotFound, "model_not_found", "Model not found")
		return
	}
	kind, pathName := middleware.TextGatewayEndpoint(c.Request.Method, c.Request.URL.Path)
	format, _ := service.GatewayProtocolFormat(kind)
	native, err := helper.GetAndValidateRequest(c, format)
	if err != nil {
		gatewayError(c, http.StatusBadRequest, "invalid_request", "Invalid text request")
		return
	}
	if pathName != "" {
		native.SetModelName(pathName)
	}
	request, err := service.GatewayChatProjection(c, native)
	if err != nil {
		gatewayError(c, http.StatusBadRequest, "unsupported_parameter", "The request cannot be converted")
		return
	}
	request.Stream = common.GetPointer(native.IsStream(c.Request))
	if pathName != "" {
		request.Model = pathName
	}
	requestedName := request.Model
	if c.GetBool("token_model_limit_enabled") {
		limits, _ := c.Get("token_model_limit")
		allowed, _ := limits.(map[string]bool)
		if !allowed[requestedName] && !allowed[offering.Name] {
			gatewayError(c, http.StatusNotFound, "model_not_found", "Model not found")
			return
		}
	}
	started := c.GetTime(middleware.TextGatewayStartedAtKey)
	if started.IsZero() {
		started = clock.Now()
	}
	sale, err := model.CurrentPriceVersion(c.Request.Context(), model.DB, model.PriceOwnerOffering, offering.ID, started.Unix())
	if err != nil {
		gatewayError(c, http.StatusServiceUnavailable, "price_unavailable", "Model price is unavailable")
		return
	}
	saleFX, saleRate, err := service.GatewayFX(c.Request.Context(), sale.Currency)
	if err != nil {
		gatewayError(c, http.StatusServiceUnavailable, "price_unavailable", "Model price is unavailable")
		return
	}
	ratio, err := service.ExactGatewayGroupRatio(userGroup)
	if err != nil {
		gatewayError(c, http.StatusServiceUnavailable, "price_unavailable", "Model price is unavailable")
		return
	}
	selection, err := service.SelectGatewayChannel(c, offering, request, native)
	if err != nil {
		var conversionErr *service.GatewayConversionError
		if errors.As(err, &conversionErr) {
			gatewayError(c, http.StatusBadRequest, "unsupported_parameter", conversionErr.Error())
			return
		}
		if errors.Is(err, service.ErrGatewayCapability) {
			gatewayError(c, http.StatusBadRequest, "unsupported_parameter", "No Channel supports the requested capabilities")
			return
		}
		gatewayError(c, http.StatusServiceUnavailable, "channel_unavailable", "No Channel is available")
		return
	}
	if setupErr := middleware.SetupContextForSelectedChannel(c, selection.Provider, selection.Channel.Model.ModelName); setupErr != nil {
		gatewayError(c, http.StatusServiceUnavailable, "channel_unavailable", "No Channel is available")
		return
	}
	// Account credentials and headers come from Provider; model identity and
	// parameter overrides come only from the selected single-model Channel.
	var overrides map[string]any
	if err := common.UnmarshalJsonStr(selection.Channel.ParameterOverrides, &overrides); err != nil {
		gatewayError(c, http.StatusServiceUnavailable, "channel_unavailable", "Invalid Channel settings")
		return
	}
	common.SetContextKey(c, constant.ContextKeyChannelParamOverride, overrides)
	info, err := relaycommon.GenRelayInfo(c, format, native, nil)
	if err != nil {
		gatewayError(c, http.StatusBadRequest, "invalid_request", "Invalid text request")
		return
	}
	info.StartTime, info.RequestId = started, c.GetString(common.RequestIdKey)
	if info.RequestId == "" {
		info.RequestId = common.NewRequestId()
		c.Header(common.RequestIdKey, info.RequestId)
	}
	info.TokenizerModel = selection.Channel.Model.ModelName
	info.ResponseFacts = &relaycommon.ResponseFacts{}
	common.SetContextKey(c, constant.ContextKeyResponseFacts, info.ResponseFacts)
	info.InitChannelMeta(c)
	info.UpstreamModelName = selection.Channel.UpstreamModelName
	info.IsModelMapped = true
	request.Model = info.UpstreamModelName
	native.SetModelName(info.UpstreamModelName)
	input, err := helper.ResolveIncomingBillingExprRequestInput(c, info)
	if err != nil {
		gatewayError(c, http.StatusBadRequest, "invalid_request", "Invalid chat request")
		return
	}
	if info.IsStream {
		info.ShouldIncludeUsage = request.StreamOptions == nil || request.StreamOptions.IncludeUsage
		if info.SupportStreamOptions {
			request.StreamOptions = &dto.StreamOptions{IncludeUsage: true}
		} else {
			request.StreamOptions = nil
		}
	}
	adaptor, err := relay.GetChannelAdaptor(info.ApiType, selection.Channel.Protocol)
	if err != nil {
		gatewayError(c, http.StatusServiceUnavailable, "channel_unavailable", "No Channel is available")
		return
	}
	if _, err := helper.ValidateTextRequest(c, request, relayconstant.RelayModeChatCompletions); err != nil || !service.GatewayCapabilitiesMatch(request, selection.Capabilities, selection.InputTokens) {
		gatewayError(c, http.StatusBadRequest, "unsupported_parameter", "Channel parameters exceed the request capabilities")
		return
	}
	converted, err := adaptor.ConvertRequest(c, info, native)
	if err != nil {
		gatewayError(c, http.StatusBadRequest, "unsupported_parameter", "The request cannot be sent to this Channel")
		return
	}
	losses := service.GatewayConversionCodes(info.ConversionDiagnostics())
	if len(losses) > 0 {
		c.Header("X-Gateway-Conversion-Loss", strings.Join(losses, ","))
	}
	encoded, err := common.Marshal(converted)
	if err != nil {
		gatewayError(c, http.StatusBadRequest, "invalid_request", "Invalid chat request")
		return
	}
	if len(info.ParamOverride) > 0 {
		encoded, err = relaycommon.ApplyParamOverrideWithRelayInfo(encoded, info)
		if err != nil {
			gatewayError(c, http.StatusServiceUnavailable, "channel_unavailable", "Invalid Channel settings")
			return
		}
	}
	effective, err := adaptor.ProjectChatRequest(info, converted, request, encoded)
	if err != nil {
		gatewayError(c, http.StatusBadRequest, "unsupported_parameter", "Invalid Channel request parameters")
		return
	}
	if _, err := helper.ValidateTextRequest(c, effective, relayconstant.RelayModeChatCompletions); err != nil || effective.Model != selection.Channel.UpstreamModelName || effective.IsStream(c.Request) != info.IsStream || !service.GatewayCapabilitiesMatch(effective, selection.Capabilities, selection.InputTokens) {
		gatewayError(c, http.StatusBadRequest, "unsupported_parameter", "Channel parameters exceed the request capabilities")
		return
	}
	common.SetContextKey(c, constant.ContextKeyOriginalModel, info.TokenizerModel)
	estimatedInput, err := service.CountRequestToken(c, effective.GetTokenCountMeta(), info)
	if err != nil || !service.GatewayCapabilitiesMatch(effective, selection.Capabilities, estimatedInput) {
		gatewayError(c, http.StatusBadRequest, "unsupported_parameter", "Channel parameters exceed the request capabilities")
		return
	}
	if effective.GetMaxTokens() == 0 && (selection.Capabilities.Limits.MaxOutputTokens != nil || selection.Capabilities.Limits.ContextLength != nil) {
		var limit int64
		if output := selection.Capabilities.Limits.MaxOutputTokens; output != nil {
			limit = *output
		}
		if context := selection.Capabilities.Limits.ContextLength; context != nil {
			remaining := *context - int64(estimatedInput)
			if limit == 0 {
				limit = remaining
			} else {
				limit = min(limit, remaining)
			}
		}
		if limit <= 0 {
			gatewayError(c, http.StatusBadRequest, "unsupported_parameter", "No output tokens fit the request context")
			return
		}
		request.MaxCompletionTokens = common.GetPointer(uint(limit))
		encoded, err = adaptor.SetChatOutputLimit(converted, encoded, uint(limit))
		if err == nil {
			effective, err = adaptor.ProjectChatRequest(info, converted, request, encoded)
		}
		if err != nil {
			gatewayError(c, http.StatusBadRequest, "unsupported_parameter", "Invalid Channel request parameters")
			return
		}
		if _, err := helper.ValidateTextRequest(c, effective, relayconstant.RelayModeChatCompletions); err != nil || !service.GatewayCapabilitiesMatch(effective, selection.Capabilities, estimatedInput) {
			gatewayError(c, http.StatusBadRequest, "unsupported_parameter", "Channel parameters exceed the request capabilities")
			return
		}
	}
	mean, err := model.RecentOfferingOutputTokens(c.Request.Context(), offering.ID, started.UnixMilli())
	if err != nil {
		gatewayError(c, http.StatusServiceUnavailable, "billing_unavailable", "Billing is unavailable")
		return
	}
	if limit := effective.GetMaxTokens(); limit > 0 {
		mean = min(mean, int64(limit))
	}
	if limit := selection.Capabilities.Limits.MaxOutputTokens; limit != nil {
		mean = min(mean, *limit)
	}
	choiceCount := 1
	if effective.N != nil {
		choiceCount = *effective.N
	}
	mean *= int64(choiceCount)
	estimatedUsage := &dto.Usage{PromptTokens: estimatedInput, CompletionTokens: int(mean), TotalTokens: estimatedInput + int(mean)}
	sourceValue, reserveValue, _, err := service.GatewayPrice(sale, estimatedUsage, input, started, ratio, saleFX)
	if err != nil {
		gatewayError(c, http.StatusServiceUnavailable, "price_unavailable", "Model price is unavailable")
		return
	}
	reserve, err := reserveValue.Receivable()
	if err != nil {
		gatewayError(c, http.StatusServiceUnavailable, "price_unavailable", "Model price is unavailable")
		return
	}
	sourceReserve, err := sourceValue.Receivable()
	if err != nil {
		gatewayError(c, http.StatusServiceUnavailable, "price_unavailable", "Model price is unavailable")
		return
	}
	record := &model.GatewayRequest{ID: info.RequestId, UserID: c.GetInt("id"), TokenID: c.GetInt("token_id"), ModelOfferingID: offering.ID, OfferingName: offering.Name, RequestedName: requestedName,
		ChoiceCount: choiceCount, SalePriceVersionID: sale.ID, SaleExpression: sale.Expression, SaleCurrency: sale.Currency, SaleFXRate: saleRate, GroupRatio: ratio.String(),
		Receivable: money.Amount{Currency: model.SettlementCurrency()}, StartedAt: started.UnixMilli(), UsageSource: "none", Status: "failed", LedgerEntryIDs: []string{}}
	if err := model.ReserveGateway(record, reserve, sourceReserve); err != nil {
		if errors.Is(err, model.ErrInsufficientWalletBalance) {
			gatewayError(c, http.StatusPaymentRequired, "insufficient_balance", "Insufficient wallet balance")
			return
		}
		gatewayError(c, http.StatusServiceUnavailable, "billing_unavailable", "Billing is unavailable")
		return
	}
	var attempt *model.GatewayAttempt
	var usage *dto.Usage
	var attemptErr *types.NewAPIError
	var purchase *model.PriceVersion
	var purchaseFX *money.Rate
	var purchaseInput billingexpr.RequestInput
	var estimatedCost, estimatedConverted money.Amount
	defer func() {
		record.EndedAt = clock.Now().UnixMilli()
		if attempt != nil {
			attempt.EndedAt = record.EndedAt
			attempt.Status, attempt.ErrorClass = gatewayAttemptOutcome(c, info, attemptErr)
			record.Status = attempt.Status
			if !info.ResponseFacts.BusinessOutput && !info.ResponseFacts.UsageReported && !info.ResponseFacts.PromptTokensReported && !info.ResponseFacts.CompletionTokensReported {
				usage = nil
			}
			if usage == nil && info.ResponseFacts.BusinessOutput {
				usage = &dto.Usage{PromptTokens: estimatedInput, CompletionTokens: tokenkit.Count(info.TokenizerModel, info.ResponseFacts.GeneratedText)}
			}
			if usage != nil {
				if !info.ResponseFacts.UsageReported && !info.ResponseFacts.PromptTokensReported && info.ResponseFacts.BusinessOutput {
					usage.PromptTokens = estimatedInput
				}
				usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
				attempt.Usage, attempt.UsageSource = service.GatewayRecordUsage(usage), "estimated"
				if info.ResponseFacts.UsageReported {
					attempt.UsageSource = "upstream"
				}
				cost, converted, _, priceErr := service.GatewayPrice(purchase, usage, purchaseInput, time.UnixMilli(attempt.StartedAt), decimal.NewFromInt(1), purchaseFX)
				if priceErr == nil {
					attempt.Cost, priceErr = cost.Cost()
				}
				if priceErr == nil {
					attempt.ConvertedCost, priceErr = converted.Cost()
				}
				if priceErr != nil {
					attempt.Cost, attempt.ConvertedCost = estimatedCost, estimatedConverted
					attempt.UsageSource = "estimated"
					record.ErrorCode = "billing_evaluation_failed"
				}
			}
			if info.ResponseFacts.BusinessOutput {
				record.Usage, record.UsageSource = attempt.Usage, attempt.UsageSource
				source, receivable, trace, priceErr := service.GatewayPrice(sale, usage, input, started, ratio, saleFX)
				if trace.BillingUnit == billingexpr.BillingUnitRequest && attempt.Status != "success" {
					source, _ = money.ParseValue("0", sale.Currency)
					receivable, _ = money.ParseValue("0", model.SettlementCurrency())
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
					record.Receivable = reserve
					record.UsageSource = "estimated"
					record.ErrorCode = "billing_evaluation_failed"
				}
			}
		}
		intent := model.GatewaySettlement{Request: *record, Attempt: attempt, SaleSource: sourceReserve}
		if err := model.PrepareGatewaySettlement(intent); err != nil {
			logger.LogError(c.Request.Context(), "gateway settlement preparation failed for request "+record.ID)
			return
		}
		if _, err := model.SettleGateway(record.ID); err != nil {
			logger.LogError(c.Request.Context(), "gateway settlement pending for request "+record.ID)
			return
		}
		if err := model.PublishGatewayRecords(record.ID); err != nil {
			logger.LogError(c.Request.Context(), "gateway record delivery pending for request "+record.ID)
		}
	}()
	if c.Request.Context().Err() != nil {
		record.Status = "cancelled"
		return
	}
	attemptStart := clock.Now()
	purchase, err = model.CurrentPriceVersion(c.Request.Context(), model.DB, model.PriceOwnerChannel, selection.Channel.ID, attemptStart.Unix())
	if err != nil {
		gatewayError(c, http.StatusServiceUnavailable, "price_unavailable", "Channel price is unavailable")
		return
	}
	var purchaseRate string
	purchaseFX, purchaseRate, err = service.GatewayFX(c.Request.Context(), purchase.Currency)
	if err != nil {
		gatewayError(c, http.StatusServiceUnavailable, "price_unavailable", "Channel price is unavailable")
		return
	}
	info.SetEstimatePromptTokens(estimatedInput)
	prepared, err := adaptor.PrepareRequest(c, info, bytes.NewReader(encoded))
	if err != nil {
		gatewayError(c, http.StatusBadGateway, "upstream_error", "Upstream request failed")
		return
	}
	purchaseInput = billingexpr.RequestInput{Body: append([]byte(nil), encoded...), Headers: make(map[string]string)}
	for key, values := range prepared.Headers() {
		purchaseInput.Headers[strings.ToLower(key)] = strings.Join(values, ",")
	}
	costEstimate, convertedEstimate, _, err := service.GatewayPrice(purchase, estimatedUsage, purchaseInput, attemptStart, decimal.NewFromInt(1), purchaseFX)
	if err != nil {
		gatewayError(c, http.StatusServiceUnavailable, "price_unavailable", "Channel price is unavailable")
		return
	}
	estimatedCost, err = costEstimate.Cost()
	if err != nil {
		gatewayError(c, http.StatusServiceUnavailable, "price_unavailable", "Channel price is unavailable")
		return
	}
	estimatedConverted, err = convertedEstimate.Cost()
	if err != nil {
		gatewayError(c, http.StatusServiceUnavailable, "price_unavailable", "Channel price is unavailable")
		return
	}
	attempt = &model.GatewayAttempt{ID: common.NewRequestId(), RequestID: record.ID, ChannelID: selection.Channel.ID, ProviderID: selection.Channel.ProviderID, ModelID: selection.Channel.ModelID, MemberTier: 1,
		StartedAt: attemptStart.UnixMilli(), FailoverDecision: "stop", FailoverReason: "single_attempt", CircuitAction: "none", RouteDiagnostics: selection.Diagnostics,
		UsageSource: "none", PurchasePriceVersionID: purchase.ID, PurchaseExpression: purchase.Expression, Cost: money.Amount{Currency: purchase.Currency}, AccountingFXRate: purchaseRate,
		ConvertedCost: money.Amount{Currency: model.SettlementCurrency()}, ConversionLosses: losses}
	response, err := adaptor.DoPreparedRequest(c, info, prepared)
	if err != nil {
		attemptErr = types.NewError(err, types.ErrorCodeDoRequestFailed)
		gatewayError(c, http.StatusBadGateway, "upstream_error", "Upstream request failed")
		return
	}
	httpResponse, ok := response.(*http.Response)
	if !ok || httpResponse == nil {
		attemptErr = types.NewError(errors.New("missing upstream response"), types.ErrorCodeBadResponse)
		gatewayError(c, http.StatusBadGateway, "upstream_error", "Invalid upstream response")
		return
	}
	if httpResponse.StatusCode != http.StatusOK {
		// Error bodies use the same protocol parser to preserve reported usage
		// without forwarding Provider error text or treating it as output.
		stream := info.IsStream
		info.IsStream = false
		result, _ := adaptor.DoResponse(c, httpResponse, info)
		usage, _ = result.(*dto.Usage)
		info.IsStream = stream
		attemptErr = types.NewErrorWithStatusCode(errors.New("upstream request failed"), types.ErrorCodeBadResponse, httpResponse.StatusCode)
		status := httpResponse.StatusCode
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		gatewayError(c, status, "upstream_error", "Upstream request failed")
		return
	}
	info.IsStream = info.IsStream || strings.HasPrefix(httpResponse.Header.Get("Content-Type"), "text/event-stream")
	result, responseErr := adaptor.DoResponse(c, httpResponse, info)
	usage, _ = result.(*dto.Usage)
	attemptErr = responseErr
	if responseErr != nil && !c.Writer.Written() {
		gatewayError(c, http.StatusBadGateway, "upstream_error", "Upstream response failed")
	}
}

// gatewayAttemptOutcome classifies facts without persisting provider messages.
// It is independent of any future failover policy.
func gatewayAttemptOutcome(c *gin.Context, info *relaycommon.RelayInfo, err *types.NewAPIError) (string, string) {
	if c.Request.Context().Err() != nil {
		return "cancelled", "client_cancelled"
	}
	if info.ResponseFacts.DownstreamWriteFailed {
		return "failed", "downstream_write_failed"
	}
	if err != nil {
		if info.ResponseFacts.BusinessOutput {
			return "partial", "upstream_error"
		}
		return "failed", "upstream_error"
	}
	if info.IsStream {
		outcome := info.StreamStatus.OutcomeSnapshot()
		if outcome.Response == relaycommon.ResponseOutcomeFailed || outcome.Response == relaycommon.ResponseOutcomeIncomplete || outcome.HasErrors || !info.StreamStatus.IsNormalEnd() || outcome.ExpectsTerminal && outcome.Response == relaycommon.ResponseOutcomeUnknown && outcome.EndReason != relaycommon.StreamEndReasonDone {
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
