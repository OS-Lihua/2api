package controller

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"maps"
	"net/http"
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
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/tokenkit"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

func gatewayError(c *gin.Context, status int, code, message string) {
	if code == "model_not_found" {
		service.RecordGatewayModelAccessDenied(c)
	}
	kind, _ := middleware.TextGatewayEndpoint(c.Request.Method, c.Request.URL.Path)
	streaming := c.Writer.Written() && strings.HasPrefix(c.Writer.Header().Get("Content-Type"), "text/event-stream")
	if c.Writer.Written() && !streaming {
		return
	}
	if facts, ok := common.GetContextKeyType[*relaycommon.ResponseFacts](c, constant.ContextKeyResponseFacts); ok && facts != nil && (facts.DownstreamWriteFailed || facts.TerminalDelivered) {
		return
	}
	key := ""
	switch message {
	case "Model not found":
		key = "gateway.model_not_found"
	case "Invalid text request":
		key = "gateway.invalid_text_request"
	case "The request cannot be converted":
		key = "gateway.cannot_convert_request"
	case "No Channel supports the requested capabilities":
		key = "gateway.unsupported_capabilities"
	case "Channel price is unavailable":
		key = "gateway.channel_price_unavailable"
	case "No Channel is available":
		key = "gateway.channel_unavailable"
	case "Model price is unavailable":
		key = "gateway.model_price_unavailable"
	case "Insufficient wallet balance":
		key = "gateway.insufficient_balance"
	case "Billing is unavailable":
		key = "gateway.billing_unavailable"
	case "Upstream request failed":
		key = "gateway.upstream_request_failed"
	case "Invalid upstream response":
		key = "gateway.invalid_upstream_response"
	case "Upstream response failed":
		key = "gateway.upstream_response_failed"
	}
	if key != "" {
		message = i18n.T(c, key)
	} else if parameters, ok := strings.CutPrefix(message, "Unsupported parameters: "); ok {
		message = i18n.T(c, i18n.MsgGatewayUnsupportedParameters, map[string]any{"Parameters": parameters})
	} else {
		message = i18n.T(c, message)
	}
	var payload gin.H
	event := ""
	switch kind {
	case protocol.AnthropicMessages:
		payload = gin.H{"type": "error", "error": gin.H{"type": code, "message": message}}
		event = "error"
	case protocol.GeminiGenerate:
		googleStatus := "UNAVAILABLE"
		if status == http.StatusBadRequest {
			googleStatus = "INVALID_ARGUMENT"
		}
		if status == http.StatusNotFound {
			googleStatus = "NOT_FOUND"
		}
		payload = gin.H{"error": gin.H{"code": status, "status": googleStatus, "message": message, "details": []gin.H{{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": code, "domain": "gateway"}}}}
	default:
		protocolError := types.OpenAIError{Code: code, Type: "gateway_error", Message: message}
		payload = gin.H{"error": protocolError}
		if kind == protocol.OpenAIResponses && streaming {
			event = "response.failed"
			payload = gin.H{"type": event, "response": gin.H{"id": helper.GetResponseID(c), "object": "response", "status": "failed", "output": []any{}, "error": protocolError}}
		}
	}
	if !streaming {
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.AbortWithStatusJSON(status, payload)
		return
	}
	encoded, err := common.Marshal(payload)
	if err == nil {
		if event == "" {
			err = helper.StringData(c, string(encoded))
		} else {
			err = helper.ResponseChunkData(c, dto.ResponsesStreamResponse{Type: event}, string(encoded))
		}
	}
	if err != nil {
		if facts, ok := common.GetContextKeyType[*relaycommon.ResponseFacts](c, constant.ContextKeyResponseFacts); ok {
			facts.DownstreamWriteFailed = true
		}
	}
}

type textGatewayRun struct {
	firstModel           string
	firstTier            int
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
	nativeError          *types.NewAPIError
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

// runAuthorizedTextGateway uses one reservation and settlement for all actual sends.
func runAuthorizedTextGateway(c *gin.Context) {
	run := &textGatewayRun{}
	parent := c.Request
	attempts := service.RunGatewayAttempts(parent.Context(), service.GatewayPolicy(), func(budget *service.GatewayAttemptBudget, exclusions *service.GatewayExclusions) service.GatewayAttemptResult {
		previousStatus, previousCode, previousMessage := run.status, run.code, run.message
		previousTier := exclusions.MemberTier
		run.status, run.code, run.message = 0, "", ""
		result := runTextGatewayAttempt(c, run, budget, exclusions)
		if result.Attempt == nil && result.SkippedChannel == 0 && run.code == "channel_unavailable" && previousCode != "" && exclusions.MemberTier == previousTier {
			run.status, run.code, run.message = previousStatus, previousCode, previousMessage
		}
		return result
	})
	c.Request = parent
	run.settle(parent.Context(), attempts)
	if parent.Context().Err() != nil {
		return
	}
	if run.status != 0 {
		gatewayError(c, run.status, run.code, run.message)
	}
}

func (run *textGatewayRun) settle(ctx context.Context, attempts []model.GatewayAttempt) {
	if run.record == nil {
		return
	}
	run.record.EndedAt = clock.Now().UnixMilli()
	if run.evaluationFailed {
		run.record.Status = "settlement_pending"
	}
	intent := model.GatewaySettlement{Request: *run.record, Attempts: attempts, SaleSource: run.saleSource, EvaluationFailed: run.evaluationFailed, SaleEvaluationFailed: run.saleEvaluationFailed, SaleUsage: run.saleUsage, AttemptUsage: run.attemptUsage}
	if run.evaluationFailed {
		intent.SaleTrace, intent.AttemptTrace = run.saleTrace, run.attemptTrace
	}
	if err := model.PrepareGatewaySettlement(intent); err != nil {
		logger.LogError(ctx, "gateway settlement preparation failed for request "+run.record.ID)
	} else if _, err := model.SettleGateway(run.record.ID); err != nil {
		logger.LogError(ctx, "gateway settlement pending for request "+run.record.ID)
	} else if err := model.PublishGatewayRecords(run.record.ID); err != nil {
		logger.LogError(ctx, "gateway record delivery pending for request "+run.record.ID)
	}
}

// TextGateway authorizes requests before running the shared attempt lifecycle.
func TextGateway(c *gin.Context) {
	if middleware.TextOfferingMissing(c) {
		gatewayError(c, http.StatusNotFound, "model_not_found", "Model not found")
		return
	}
	offering := middleware.TextOffering(c)
	if offering == nil {
		kind, name := middleware.TextGatewayEndpoint(c.Request.Method, c.Request.URL.Path)
		if kind == "" {
			c.Next()
			return
		}
		if name == "" {
			var request struct {
				Model string `json:"model"`
			}
			if common.UnmarshalBodyReusable(c, &request) != nil || request.Model == "" {
				gatewayError(c, http.StatusBadRequest, "invalid_request", "Invalid text request")
				return
			}
		}
		// Resolution already distinguishes configured legacy models from missing names.
		// Every text protocol must enforce legacy quota before entering the legacy relay.
		if _, err := model.ValidateUserToken(c.GetString("token_key")); err != nil {
			if errors.Is(err, model.ErrDatabase) {
				gatewayError(c, http.StatusServiceUnavailable, "authorization_unavailable", i18n.T(c, i18n.MsgGatewayAuthorizationUnavailable))
			} else {
				gatewayError(c, http.StatusUnauthorized, "invalid_api_key", common.TranslateMessage(c, i18n.MsgTokenInvalid))
			}
			return
		}
		c.Next()
		return
	}
	c.Abort()
	allowed, err := service.GatewayOfferingAllowed(c, offering)
	if err != nil {
		gatewayError(c, http.StatusServiceUnavailable, "authorization_unavailable", i18n.T(c, i18n.MsgGatewayAuthorizationUnavailable))
		return
	}
	if !allowed {
		gatewayError(c, http.StatusNotFound, "model_not_found", i18n.T(c, i18n.MsgGatewayModelNotFound))
		return
	}
	runAuthorizedTextGateway(c)
}

func runTextGatewayAttempt(c *gin.Context, run *textGatewayRun, budget *service.GatewayAttemptBudget, exclusions *service.GatewayExclusions) (result service.GatewayAttemptResult) {
	offering := middleware.TextOffering(c)
	userGroup := common.GetContextKeyString(c, constant.ContextKeyUserGroup)
	kind, pathName := middleware.TextGatewayEndpoint(c.Request.Method, c.Request.URL.Path)
	format, _ := service.GatewayProtocolFormat(kind)
	native, err := helper.GetAndValidateRequest(c, format)
	if err != nil {
		run.fail(c, http.StatusBadRequest, "invalid_request", "Invalid text request")
		return
	}
	if pathName != "" {
		native.SetModelName(pathName)
	}
	if _, chat := native.(*dto.GeneralOpenAIRequest); !chat {
		if err := service.PrepareGatewayMedia(c, native); err != nil {
			run.fail(c, http.StatusBadRequest, "invalid_request", "Invalid text request")
			return
		}
	}
	request, err := service.GatewayChatProjection(c, native)
	if err != nil {
		run.fail(c, http.StatusBadRequest, "unsupported_parameter", "The request cannot be converted")
		return
	}
	projection := *request
	request = &projection
	request.Stream = common.GetPointer(native.IsStream(c.Request))
	if pathName != "" {
		request.Model = pathName
	}
	requestedName := request.Model
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
	if run.firstModel == "" {
		var first model.ModelOfferingMember
		for _, member := range offering.Members {
			if first.ModelID == 0 || member.Tier < first.Tier || member.Tier == first.Tier && member.ModelID < first.ModelID {
				first = member
			}
		}
		var unified model.Model
		if first.ModelID == 0 || model.DB.WithContext(c.Request.Context()).First(&unified, first.ModelID).Error != nil {
			run.fail(c, http.StatusServiceUnavailable, "channel_unavailable", "No Channel is available")
			return
		}
		run.firstModel, run.firstTier = unified.ModelName, first.Tier
	}
	selection, preparedChat, err := gatewayChatRoute(c, run, offering, request, native, kind, started, exclusions)
	if err != nil {
		var conversionErr *service.GatewayConversionError
		if errors.As(err, &conversionErr) {
			run.fail(c, http.StatusBadRequest, "unsupported_parameter", conversionErr.Error())
			return
		}
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
	selectedProvider, selectedKey, selectedKeyIndex := selection.Provider, info.ApiKey, info.ChannelMultiKeyIndex
	var providerCallRejected error
	info.BeginProviderCall = func() (context.Context, error) {
		ctx, err := budget.Start()
		if err != nil {
			providerCallRejected = err
			return nil, err
		}
		if !committedKey {
			if err := selectedProvider.CommitRouteKey(ctx, selectedKey, selectedKeyIndex); err != nil {
				providerCallRejected = err
				return nil, err
			}
			committedKey = true
		} else if err := selectedProvider.ValidateRouteKey(ctx, selectedKey, selectedKeyIndex); err != nil {
			providerCallRejected = err
			return nil, err
		}
		if _, err := budget.Start(); err != nil {
			providerCallRejected = err
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
	info.ResponseFacts.ExpectedChoices = choiceCount
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
			normalBudget := (outcome.OutputLimitReached() || info.ResponseFacts.OutputLimitObserved) && attemptErr == nil && (!info.IsStream || info.StreamStatus.CompletedNormally())
			if outcome.ErrorCode != "" {
				upstreamCode = outcome.ErrorCode
			}
			if upstreamCode == "" {
				upstreamCode = info.ResponseFacts.UpstreamErrorCode
			}
			if outcome.ErrorType != "" {
				upstreamType = outcome.ErrorType
			}
			if outcome.ErrorStatus >= 400 {
				upstreamStatus = outcome.ErrorStatus
			}
			if normalBudget {
				upstreamCode = "gateway_output_limit"
			}
			if info.PerformanceBusinessRejection {
				upstreamCode = "content_filter"
			}
			failure := service.ClassifyGatewayFailure(upstreamStatus, upstreamCode, transportFailed, budget.TimedOut(), parent.Context().Err() != nil, info.ResponseFacts.DownstreamWriteFailed, upstreamType)
			localRejection := providerCallRejected != nil
			if localRejection && parent.Context().Err() == nil && !info.ResponseFacts.DownstreamWriteFailed {
				if errors.Is(providerCallRejected, service.ErrGatewayBudget) {
					failure = service.GatewayFailure{Class: "first_byte_timeout", Switch: true}
				} else {
					failure = service.GatewayFailure{Class: "channel_unavailable", Switch: true, ExcludeProvider: true}
				}
			}
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
			recordGatewayProviderFailure(c, info, selection, failure)
			result = service.GatewayAttemptResult{Attempt: attempt, Failure: failure, Delivered: info.ResponseFacts.BusinessOutput,
				FirstByte: firstByte, HealthLease: selection.Admission.Lease, LocalRejection: localRejection}
			if failure.Class == "rate_limited" {
				result.RetryAfter = retryAfter
				if result.RetryAfter == "" {
					result.RetryAfter = info.ResponseFacts.UpstreamRetryAfter
				}
			}

			record.Status = attempt.Status
			if !info.ResponseFacts.BusinessOutput && !info.ResponseFacts.SourceOutputObserved && info.ResponseFacts.GeneratedText == "" && !info.ResponseFacts.UsageReported && !info.ResponseFacts.PromptTokensReported && !info.ResponseFacts.CompletionTokensReported && !info.ResponseFacts.IndependentUsageReported {
				usage = nil
			}
			if usage == nil && (info.ResponseFacts.BusinessOutput || info.ResponseFacts.GeneratedText != "") {
				usage = &dto.Usage{PromptTokens: estimatedInput, CompletionTokens: tokenkit.Count(info.TokenizerModel, info.ResponseFacts.GeneratedText)}
			}
			purchaseUsage := usage
			if usage != nil {
				if !info.ResponseFacts.UsageReported && !info.ResponseFacts.PromptTokensReported && (info.ResponseFacts.BusinessOutput || info.ResponseFacts.GeneratedText != "") {
					usage.PromptTokens = max(usage.PromptTokens, estimatedInput)
				}
				usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
				copy := *usage
				purchaseUsage = &copy
			}
			if info.ResponseFacts.SourceOutputObserved {
				if purchaseUsage == nil {
					purchaseUsage = &dto.Usage{}
				}
				if !info.ResponseFacts.PromptTokensReported {
					purchaseUsage.PromptTokens = max(purchaseUsage.PromptTokens, estimatedInput)
				}
				if !info.ResponseFacts.CompletionTokensReported {
					generated := tokenkit.Count(info.TokenizerModelName(), info.ResponseFacts.SourceGeneratedText)
					if info.ResponseFacts.SourceOutputTokens != nil {
						generated = *info.ResponseFacts.SourceOutputTokens
					}
					purchaseUsage.CompletionTokens = max(purchaseUsage.CompletionTokens, generated)
				}
			}
			if purchaseUsage != nil {
				purchaseUsage.TotalTokens = purchaseUsage.PromptTokens + purchaseUsage.CompletionTokens
				if run.attemptUsage == nil {
					run.attemptUsage = make(map[string]*dto.Usage)
				}
				run.attemptUsage[attempt.ID] = service.GatewayUsageSnapshot(purchaseUsage)
				attempt.Usage, attempt.UsageSource = service.GatewayRecordUsage(purchaseUsage), "estimated"
				if info.ResponseFacts.UsageReported {
					attempt.UsageSource = "upstream"
				}
				cost, converted, costTrace, priceErr := service.GatewayPrice(purchase, purchaseUsage, purchaseInput, time.UnixMilli(attempt.StartedAt), decimal.NewFromInt(1), purchaseFX)
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
				record.Usage, record.UsageSource = service.GatewayRecordUsage(usage), attempt.UsageSource
				estimateDeliveredOutput := attempt.Status == "partial" && !normalBudget || info.ResponseFacts.DownstreamWriteFailed || !info.ResponseFacts.UsageReported && attempt.Status == "cancelled"
				if info.IsStream && estimateDeliveredOutput && usage != nil {
					// Procurement retains reported usage; interrupted sales use delivered text.
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
	// Credential row-lock waits belong to preparation, before the purchase snapshot.
	previousContact := contactedProvider
	attemptContext, budgetErr := info.BeginProviderCall()
	contactedProvider = previousContact
	if budgetErr != nil {
		if errors.Is(budgetErr, service.ErrGatewayBudget) || parent.Context().Err() != nil {
			run.fail(c, http.StatusServiceUnavailable, "budget_exhausted", "Upstream request failed")
		} else {
			run.fail(c, http.StatusServiceUnavailable, "channel_unavailable", "No Channel is available")
		}
		return
	}
	c.Request = parent.WithContext(attemptContext)
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
	if _, err := budget.Start(); err != nil {
		attempt = nil
		run.fail(c, http.StatusServiceUnavailable, "budget_exhausted", "Upstream request failed")
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
	// This header describes local conversion facts, including an empty loss set.
	httpResponse.Header.Del("X-Gateway-Conversion-Loss")
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
	originalWriter := c.Writer
	originalHeaders := originalWriter.Header().Clone()
	defer func() {
		c.Writer = originalWriter
		delete(c.Keys, "event_stream_headers_set")
		if !info.ResponseFacts.BusinessOutput && !originalWriter.Written() {
			clear(originalWriter.Header())
			for key, values := range originalHeaders {
				originalWriter.Header()[key] = values
			}
		}
	}()
	info.IsStream = info.IsStream || strings.HasPrefix(httpResponse.Header.Get("Content-Type"), "text/event-stream")
	if info.IsStream {
		httpResponse.Body = helper.GatewayStreamIdleBody(httpResponse.Body, info)
	}
	if selection.Tier > run.firstTier {
		c.Header("X-Model-Fallback", run.firstModel+"->"+selection.Channel.Model.ModelName)
	} else {
		c.Writer.Header().Del("X-Model-Fallback")
	}
	responseUsage, responseErr := adaptor.DoResponse(c, httpResponse, info)
	usage, _ = responseUsage.(*dto.Usage)
	attemptErr = responseErr
	if responseErr != nil {
		transportFailed = responseErr.GetErrorCode() == types.ErrorCodeReadResponseBodyFailed
		apiError := responseErr.ToOpenAIError()
		upstreamType = apiError.Type
		upstreamCode, _ = apiError.Code.(string)
		if upstreamCode == "" {
			upstreamCode = apiError.Type
		}
	}
	if status, reason := gatewayAttemptOutcome(c, info, responseErr); status != "success" && reason != "output_limit" {
		run.fail(c, http.StatusBadGateway, "upstream_error", "Upstream response failed")
	}
	if responseErr != nil && !c.Writer.Written() {
		run.fail(c, http.StatusBadGateway, "upstream_error", "Upstream response failed")
	}
	return
}

// recordGatewayProviderFailure shares atomic Provider checks and excludes
// usable credentials from the audit event for every gateway transport.
func recordGatewayProviderFailure(c *gin.Context, info *relaycommon.RelayInfo, selection *service.GatewaySelection, failure service.GatewayFailure) {
	if !common.AutomaticDisableChannelEnabled || !selection.Provider.GetAutoBan() || failure.Class != "credentials" && failure.Class != "account_quota" {
		return
	}
	index := info.ChannelMultiKeyIndex
	providerError := types.ProviderError{ChannelId: selection.Channel.ProviderID, ChannelName: selection.Provider.Name, IsMultiKey: info.ChannelIsMultiKey, UsingKey: info.ApiKey, AutoBan: true}
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
	if info.StreamStatus.OutcomeSnapshot().OutputLimitReached() || info.ResponseFacts.OutputLimitObserved {
		return "partial", "output_limit"
	}
	if !info.ResponseFacts.BusinessOutput {
		return "failed", "empty_response"
	}
	return "success", ""
}
