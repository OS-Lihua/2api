package controller

import (
	"bytes"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// gatewayPreparedChat stays in request memory. Route evaluates the same
// effective quantities and probes later sent by the selected candidate.
type gatewayPreparedChat struct {
	Context   *gin.Context
	Info      *relaycommon.RelayInfo
	Adaptor   *relay.ChannelAdaptor
	Request   *dto.GeneralOpenAIRequest
	Converted any
	Encoded   []byte
	SaleInput billingexpr.RequestInput
	Usage     *dto.Usage
	Choices   int
	Facts     service.GatewayRouteFacts
}

func prepareGatewayChatRoute(c *gin.Context, selection *service.GatewaySelection, original *dto.GeneralOpenAIRequest, started time.Time, mean int64) (*gatewayPreparedChat, error) {
	candidate := c.Copy()
	raw, err := common.Marshal(original)
	if err != nil {
		return nil, service.ErrGatewayCapability
	}
	request := &dto.GeneralOpenAIRequest{}
	if err = common.Unmarshal(raw, request); err != nil {
		return nil, service.ErrGatewayCapability
	}
	if setupErr := middleware.SetupContextForGatewayCandidate(candidate, selection.Provider, selection.Channel.Model.ModelName); setupErr != nil {
		return nil, service.ErrGatewayUnavailable
	}
	var overrides map[string]any
	if common.UnmarshalJsonStr(selection.Channel.ParameterOverrides, &overrides) != nil {
		return nil, service.ErrGatewayUnavailable
	}
	common.SetContextKey(candidate, constant.ContextKeyChannelParamOverride, overrides)
	info := relaycommon.GenRelayInfoOpenAI(candidate, request)
	info.ResponseFacts = &relaycommon.ResponseFacts{}
	if info.StreamStatus == nil {
		info.StreamStatus = relaycommon.NewStreamStatus()
	}
	info.StartTime = started
	info.TokenizerModel = selection.Channel.Model.ModelName
	info.InitChannelMeta(candidate)
	info.UpstreamModelName = selection.Channel.UpstreamModelName
	info.IsModelMapped = true
	saleInput, err := helper.ResolveIncomingBillingExprRequestInput(candidate, info)
	if err != nil {
		return nil, service.ErrGatewayCapability
	}
	common.SetContextKey(candidate, constant.ContextKeyOriginalModel, info.TokenizerModel)
	originalTokens, err := service.CountRequestToken(candidate, request.GetTokenCountMeta(), info)
	if err != nil || !service.GatewayCapabilitiesMatch(request, selection.Capabilities, originalTokens) {
		return nil, service.ErrGatewayCapability
	}
	request.Model = info.UpstreamModelName
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
		return nil, service.ErrGatewayConversion
	}
	converted, err := adaptor.ConvertRequest(candidate, info, request)
	if err != nil {
		return nil, service.ErrGatewayConversion
	}
	encoded, err := common.Marshal(converted)
	if err != nil {
		return nil, service.ErrGatewayConversion
	}
	if len(info.ParamOverride) > 0 {
		encoded, err = relaycommon.ApplyParamOverrideWithRelayInfo(encoded, info)
		if err != nil {
			return nil, service.ErrGatewayUnavailable
		}
	}
	effective, err := adaptor.ProjectChatRequest(info, converted, request, encoded)
	if err != nil {
		return nil, service.ErrGatewayCapability
	}
	if _, err = helper.ValidateTextRequest(candidate, effective, relayconstant.RelayModeChatCompletions); err != nil || effective.Model != selection.Channel.UpstreamModelName || effective.IsStream(candidate.Request) != info.IsStream {
		return nil, service.ErrGatewayCapability
	}
	common.SetContextKey(candidate, constant.ContextKeyOriginalModel, info.TokenizerModel)
	tokens, err := service.CountRequestToken(candidate, effective.GetTokenCountMeta(), info)
	if err != nil {
		return nil, service.ErrGatewayCapability
	}
	selection.InputTokens = tokens
	if selection.Capabilities.Limits.MaxOutputTokens != nil || selection.Capabilities.Limits.ContextLength != nil {
		var limit int64
		if output := selection.Capabilities.Limits.MaxOutputTokens; output != nil {
			limit = *output
		}
		if context := selection.Capabilities.Limits.ContextLength; context != nil {
			remaining := *context - int64(tokens)
			if limit == 0 {
				limit = remaining
			} else {
				limit = min(limit, remaining)
			}
		}
		if limit <= 0 {
			return nil, service.ErrGatewayCapability
		}
		convertedDefaultExceedsLimit := original.GetMaxTokens() == 0 && len(info.ParamOverride) == 0 && effective.GetMaxTokens() > uint(limit)
		if effective.GetMaxTokens() == 0 || convertedDefaultExceedsLimit {
			request.MaxCompletionTokens = common.GetPointer(uint(limit))
			encoded, err = adaptor.SetChatOutputLimit(converted, encoded, uint(limit))
			if err == nil {
				effective, err = adaptor.ProjectChatRequest(info, converted, request, encoded)
			}
			if err != nil {
				return nil, service.ErrGatewayCapability
			}
			if _, err = helper.ValidateTextRequest(candidate, effective, relayconstant.RelayModeChatCompletions); err != nil {
				return nil, service.ErrGatewayCapability
			}
		}
	}
	if !service.GatewayCapabilitiesMatch(effective, selection.Capabilities, tokens) {
		return nil, service.ErrGatewayCapability
	}
	if limit := effective.GetMaxTokens(); limit > 0 {
		mean = min(mean, int64(limit))
	}
	if limit := selection.Capabilities.Limits.MaxOutputTokens; limit != nil {
		mean = min(mean, *limit)
	}
	if limit := selection.Capabilities.Limits.ContextLength; limit != nil {
		mean = min(mean, *limit-int64(tokens))
	}
	choices := 1
	if effective.N != nil {
		choices = *effective.N
	}
	if mean < 0 || choices < 1 || choices > helper.MaxChatCompletionsN || mean > ((1<<31)-1)/int64(choices) {
		return nil, service.ErrGatewayCapability
	}
	mean *= int64(choices)
	usage := &dto.Usage{PromptTokens: tokens, CompletionTokens: int(mean), TotalTokens: tokens + int(mean)}
	headers, deferredHeaders, err := adaptor.PreviewHeaders(candidate, info, bytes.NewReader(encoded))
	if err != nil {
		return nil, service.ErrGatewayUnavailable
	}
	purchaseInput := billingexpr.RequestInput{Body: encoded, Headers: map[string]string{}, DeferredHeaders: deferredHeaders}
	for key, values := range headers {
		purchaseInput.Headers[strings.ToLower(key)] = strings.Join(values, ",")
	}
	losses := []string{}
	if info.ConversionDiagnosticsTruncated() {
		return nil, service.ErrGatewayConversion
	}
	for _, diagnostic := range info.ConversionDiagnostics() {
		if diagnostic.Severity == types.ConversionDiagnosticError {
			return nil, service.ErrGatewayConversion
		}
		if !slices.Contains(losses, diagnostic.Code) {
			losses = append(losses, diagnostic.Code)
		}
	}
	quality := 0
	if selection.Channel.Protocol != protocol.OpenAIChat {
		quality = 1
		if info.RequestConversionQuality != "good" {
			quality = 2
		}
	}
	return &gatewayPreparedChat{Context: candidate, Info: info, Adaptor: adaptor, Request: request, Converted: converted, Encoded: encoded, SaleInput: saleInput, Usage: usage, Choices: choices,
		Facts: service.GatewayRouteFacts{Usage: usage, SaleInput: saleInput, PurchaseInput: purchaseInput, Quality: quality, ConversionLosses: losses}}, nil
}

// Keep the sale declaration in the caller's frozen money lifecycle.
func gatewayChatRoute(c *gin.Context, run *textGatewayRun, offering *model.ModelOffering, request *dto.GeneralOpenAIRequest, started time.Time, exclusions *service.GatewayExclusions) (*service.GatewaySelection, *gatewayPreparedChat, error) {
	mean, err := model.RecentOfferingOutputTokens(c.Request.Context(), offering.ID, started.UnixMilli())
	if err != nil {
		return nil, nil, err
	}
	prepared := map[int]*gatewayPreparedChat{}
	selection, err := service.SelectGatewayRoute(c, offering, service.GatewayRouteInput{Protocol: protocol.OpenAIChat, Sale: run.sale, SaleFX: run.saleFX, Ratio: run.ratio, StartedAt: started, Failures: exclusions,
		Prepare: func(selection *service.GatewaySelection) (*service.GatewayRouteFacts, error) {
			if !service.GatewayCapabilitiesMatch(request, selection.Capabilities, 0) {
				return nil, service.ErrGatewayCapability
			}
			candidate, err := prepareGatewayChatRoute(c, selection, request, started, mean)
			if err != nil {
				return nil, err
			}
			prepared[selection.Channel.ID] = candidate
			return &candidate.Facts, nil
		}})
	if err != nil {
		return nil, nil, err
	}
	return selection, prepared[selection.Channel.ID], nil
}
