package controller

import (
	"bytes"
	"errors"
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
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
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

func prepareGatewayChatRoute(c *gin.Context, selection *service.GatewaySelection, original *dto.GeneralOpenAIRequest, native dto.Request, kind protocol.Type, accepted []string, started time.Time, mean int64) (*gatewayPreparedChat, error) {
	candidate := c.Copy()
	if selection.Provider.Setting != nil && *selection.Provider.Setting != "" {
		var settings dto.ProviderSettings
		if common.UnmarshalJsonStr(*selection.Provider.Setting, &settings) != nil {
			return nil, service.ErrGatewayUnavailable
		}
	}
	if selection.Provider.OtherSettings != "" {
		var settings dto.ProviderOtherSettings
		if common.UnmarshalJsonStr(selection.Provider.OtherSettings, &settings) != nil {
			return nil, service.ErrGatewayUnavailable
		}
	}
	if setupErr := middleware.SetupContextForGatewayCandidate(candidate, selection.Provider, selection.Channel.Model.ModelName); setupErr != nil {
		return nil, service.ErrGatewayUnavailable
	}
	var overrides map[string]any
	if common.UnmarshalJsonStr(selection.Channel.ParameterOverrides, &overrides) != nil {
		return nil, service.ErrGatewayUnavailable
	}
	common.SetContextKey(candidate, constant.ContextKeyChannelParamOverride, overrides)
	nativeSnapshot, err := service.CloneGatewayRequest(native)
	if err != nil {
		return nil, service.ErrGatewayConversion
	}
	if selection.Channel.Protocol != kind {
		if err := service.PrepareGatewayMedia(candidate, nativeSnapshot); err != nil {
			return nil, &service.GatewayConversionError{Diagnostics: []types.ConversionDiagnostic{{Code: "request_media_unavailable", Path: "request", Message: "The request media could not be prepared", Severity: types.ConversionDiagnosticError}}}
		}
	}
	request, err := service.GatewayChatProjection(candidate, nativeSnapshot)
	if err != nil {
		return nil, service.ErrGatewayCapability
	}
	format, err := service.GatewayProtocolFormat(kind)
	if err != nil {
		return nil, service.ErrGatewayConversion
	}
	info, err := relaycommon.GenRelayInfo(candidate, format, nativeSnapshot, nil)
	if err != nil {
		return nil, service.ErrGatewayCapability
	}
	info.ResponseFacts = &relaycommon.ResponseFacts{OnBusinessOutput: func() {}}
	common.SetContextKey(candidate, constant.ContextKeyResponseFacts, info.ResponseFacts)
	if info.StreamStatus == nil {
		info.StreamStatus = relaycommon.NewStreamStatus()
	}
	info.StartTime = started
	info.TokenizerModel = selection.Channel.Model.ModelName
	info.InitChannelMeta(candidate)
	info.ChannelOtherSettings.ToolLossPolicy = string(types.ConversionLossPolicyAllow)
	info.UpstreamModelName = selection.Channel.UpstreamModelName
	info.IsModelMapped = true
	saleInput, err := helper.ResolveIncomingBillingExprRequestInput(candidate, info)
	if err != nil {
		return nil, service.ErrGatewayCapability
	}
	common.SetContextKey(candidate, constant.ContextKeyOriginalModel, info.TokenizerModel)
	counterInfo := *info
	counterInfo.RelayFormat = types.RelayFormatOpenAI
	originalTokens, err := service.CountRequestToken(candidate, request.GetTokenCountMeta(), &counterInfo, service.GatewayRequestTokenOptions(candidate))
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
	conversion, err := service.PreconvertGatewayRequest(candidate, info, selection.Channel.Protocol, nativeSnapshot, accepted)
	if err != nil {
		if conversion != nil && len(conversion.Diagnostics) > 0 {
			return nil, &service.GatewayConversionError{Diagnostics: conversion.Diagnostics}
		}
		return nil, service.ErrGatewayConversion
	}
	conversion.Value.(dto.Request).SetModelName(info.UpstreamModelName)
	info.RecordConversionDiagnostics(candidate, conversion.Diagnostics)
	if wire, ok := conversion.Value.(*dto.GeneralOpenAIRequest); ok && info.IsStream {
		if info.SupportStreamOptions {
			wire.StreamOptions = &dto.StreamOptions{IncludeUsage: true}
		} else {
			wire.StreamOptions = nil
		}
	}
	converted, err := adaptor.ConvertRequest(candidate, info, conversion.Value)
	if err != nil {
		if diagnostics := info.ConversionDiagnostics(); len(diagnostics) > 0 {
			return nil, &service.GatewayConversionError{Diagnostics: diagnostics}
		}
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
	encoded, err = adaptor.FinalizeChatRequest(candidate, info, converted, encoded)
	if err != nil {
		return nil, service.ErrGatewayCapability
	}
	effective, err := adaptor.ProjectChatRequest(candidate, info, converted, request, encoded)
	if err != nil {
		return nil, service.ErrGatewayCapability
	}
	if _, err = helper.ValidateTextRequest(candidate, effective, relayconstant.RelayModeChatCompletions, selection.Channel.Protocol == protocol.OpenAIResponses); err != nil || effective.Model != selection.Channel.UpstreamModelName || effective.IsStream(candidate.Request) != info.IsStream {
		return nil, service.ErrGatewayCapability
	}
	requestedOutputLimit := original.MaxCompletionTokens
	if requestedOutputLimit == nil {
		requestedOutputLimit = original.MaxTokens
	}
	effectiveOutputLimit := effective.MaxCompletionTokens
	if effectiveOutputLimit == nil {
		effectiveOutputLimit = effective.MaxTokens
	}
	if requestedOutputLimit != nil && *requestedOutputLimit == 0 && (effectiveOutputLimit == nil || *effectiveOutputLimit != 0) {
		parameter := "max_tokens"
		if original.MaxCompletionTokens != nil {
			parameter = "max_completion_tokens"
		}
		info.RecordConversionDiagnostics(candidate, []types.ConversionDiagnostic{{Code: "request_parameter_dropped", Path: parameter, Message: "The final native request must preserve the explicit zero output limit", Severity: types.ConversionDiagnosticError}})
		return nil, &service.GatewayConversionError{Diagnostics: info.ConversionDiagnostics()}
	}
	common.SetContextKey(candidate, constant.ContextKeyOriginalModel, info.TokenizerModel)
	tokens, err := service.CountRequestToken(candidate, effective.GetTokenCountMeta(), &counterInfo, service.GatewayRequestTokenOptions(candidate))
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
		boundDefault := requestedOutputLimit == nil
		for _, key := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens", "generationConfig", "generation_config"} {
			if _, overridden := info.ParamOverride[key]; overridden {
				boundDefault = false
			}
		}
		convertedDefaultExceedsLimit := boundDefault && effective.GetMaxTokens() > uint(limit)
		if effectiveOutputLimit == nil || convertedDefaultExceedsLimit {
			request.MaxCompletionTokens = common.GetPointer(uint(limit))
			encoded, err = adaptor.SetChatOutputLimit(converted, encoded, uint(limit))
			if err == nil {
				effective, err = adaptor.ProjectChatRequest(candidate, info, converted, request, encoded)
			}
			if err != nil {
				return nil, service.ErrGatewayCapability
			}
			if _, err = helper.ValidateTextRequest(candidate, effective, relayconstant.RelayModeChatCompletions, selection.Channel.Protocol == protocol.OpenAIResponses); err != nil {
				return nil, service.ErrGatewayCapability
			}
		}
	}
	if !service.GatewayCapabilitiesMatch(effective, selection.Capabilities, tokens) {
		return nil, service.ErrGatewayCapability
	}
	effectiveOutputLimit = effective.MaxCompletionTokens
	if effectiveOutputLimit == nil {
		effectiveOutputLimit = effective.MaxTokens
	}
	if effectiveOutputLimit != nil {
		mean = min(mean, int64(*effectiveOutputLimit))
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
	diagnostics := info.ConversionDiagnostics()
	if info.ConversionDiagnosticsTruncated() {
		return nil, &service.GatewayConversionError{Diagnostics: diagnostics}
	}
	if err := service.CheckGatewayConversionLosses(diagnostics, accepted); err != nil {
		return nil, &service.GatewayConversionError{Diagnostics: diagnostics}
	}
	losses := service.GatewayConversionCodes(diagnostics)
	quality := 0
	if conversion.From != conversion.To {
		quality = 2
		if conversion.Quality == relayconvert.RequestConverterQualityGood {
			quality = 1
		}
	}
	return &gatewayPreparedChat{Context: candidate, Info: info, Adaptor: adaptor, Request: request, Converted: converted, Encoded: encoded, SaleInput: saleInput, Usage: usage, Choices: choices,
		Facts: service.GatewayRouteFacts{Usage: usage, SaleInput: saleInput, PurchaseInput: purchaseInput, Quality: quality, ConversionLosses: losses}}, nil
}

// Keep the sale declaration in the caller's frozen money lifecycle.
func gatewayChatRoute(c *gin.Context, run *textGatewayRun, offering *model.ModelOffering, request *dto.GeneralOpenAIRequest, native dto.Request, kind protocol.Type, started time.Time, exclusions *service.GatewayExclusions) (*service.GatewaySelection, *gatewayPreparedChat, error) {
	mean, err := model.RecentOfferingOutputTokens(c.Request.Context(), offering.ID, started.UnixMilli())
	if err != nil {
		return nil, nil, err
	}
	prepared := map[int]*gatewayPreparedChat{}
	accepted := service.GatewayAcceptedConversionLosses(offering)
	var conversionFailure *service.GatewayConversionError
	selection, err := service.SelectGatewayRoute(c, offering, service.GatewayRouteInput{Protocol: kind, AcceptedConversionLosses: accepted, Sale: run.sale, SaleFX: run.saleFX, Ratio: run.ratio, StartedAt: started, Failures: exclusions,
		Prepare: func(selection *service.GatewaySelection) (*service.GatewayRouteFacts, error) {
			if !service.GatewayCapabilitiesMatch(request, selection.Capabilities, 0) {
				return nil, service.ErrGatewayCapability
			}
			candidate, err := prepareGatewayChatRoute(c, selection, request, native, kind, accepted, started, mean)
			if err != nil {
				if errors.As(err, &conversionFailure) {
					return nil, service.ErrGatewayConversion
				}
				return nil, err
			}
			prepared[selection.Channel.ID] = candidate
			return &candidate.Facts, nil
		}})
	if err != nil {
		if errors.Is(err, service.ErrGatewayConversion) && conversionFailure != nil {
			return nil, nil, conversionFailure
		}
		return nil, nil, err
	}
	return selection, prepared[selection.Channel.ID], nil
}
