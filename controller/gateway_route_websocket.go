package controller

import (
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/capability"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// gatewayPreparedWebSocket contains only transient, pure candidate preparation.
// Route owns eligibility, quota, profit, affinity and health admission.
type gatewayPreparedWebSocket struct {
	Context    *gin.Context
	Info       *relaycommon.RelayInfo
	Transport  *relaychannel.PreparedWssRequest
	Payload    []byte
	InputUsage *dto.Usage
	Facts      service.GatewayRouteFacts
}

func prepareGatewayWebSocketRoute(c *gin.Context, selection *service.GatewaySelection, call *relay.ResponsesWSCall, started time.Time, mean int64) (*gatewayPreparedWebSocket, error) {
	if selection.Channel.Protocol != call.Protocol() || call.LockedChannelID != 0 && (selection.Channel.ID != call.LockedChannelID || selection.Provider.Id != call.LockedProviderID || selection.Provider.Type != call.LockedProviderType || selection.Channel.UpstreamModelName != call.LockedUpstreamModel) {
		return nil, service.ErrGatewayUnavailable
	}
	if call.Protocol() == protocol.OpenAIResponses && !selection.Provider.GetSetting().ResponsesWebSocketEnabled {
		return nil, service.ErrGatewayUnavailable
	}
	candidate := c.Copy()
	candidate.Request = c.Request.Clone(c.Request.Context())
	var pinned []int
	if call.LockedChannelID != 0 {
		pinned = []int{call.LockedKeyIndex}
	}
	if apiErr := middleware.SetupContextForGatewayCandidate(candidate, selection.Provider, selection.Channel.Model.ModelName, pinned...); apiErr != nil {
		return nil, service.ErrGatewayUnavailable
	}
	var overrides map[string]any
	if common.UnmarshalJsonStr(selection.Channel.ParameterOverrides, &overrides) != nil {
		return nil, service.ErrGatewayUnavailable
	}
	common.SetContextKey(candidate, constant.ContextKeyChannelParamOverride, overrides)
	common.SetContextKey(candidate, constant.ContextKeyOriginalModel, call.Request.Model)
	info := relaycommon.GenRelayInfoResponses(candidate, &call.Request)
	info.InitChannelMeta(candidate)
	info.UpstreamModelName, info.TokenizerModel = selection.Channel.UpstreamModelName, selection.Channel.Model.ModelName
	info.IsModelMapped, info.IsStream = true, true
	info.RequestId, info.StartTime = c.GetString(common.RequestIdKey), started
	info.ResponseFacts, info.StreamStatus = &relaycommon.ResponseFacts{}, relaycommon.NewStreamStatus()
	if call.Protocol() == protocol.OpenAIRealtime {
		info.RelayFormat, info.RelayMode = types.RelayFormatOpenAIRealtime, relayconstant.RelayModeRealtime
		info.InputAudioFormat, info.OutputAudioFormat = "pcm16", "pcm16"
	}
	common.SetContextKey(candidate, constant.ContextKeyResponseFacts, info.ResponseFacts)
	saleInput, err := helper.ResolveIncomingBillingExprRequestInput(candidate, info)
	if err != nil {
		return nil, &service.GatewayParameterError{Path: "request", Reason: "Invalid request metadata"}
	}
	if call.Native != nil && len(call.Native.OriginalBody) > 0 {
		saleInput.Body = call.Native.OriginalBody
	}
	document := ojson.NewDoc(append([]byte(nil), call.Body...))
	if document.Set("model", info.UpstreamModelName) != nil {
		return nil, &service.GatewayParameterError{Path: "model", Reason: "Invalid effective WebSocket request"}
	}
	incomingHeaders := info.RequestHeaders
	info.RequestHeaders = relay.SanitizeNativeClientHeaders(incomingHeaders)
	body, err := relaycommon.ApplyParamOverrideWithRelayInfo(document.Bytes(), info)
	info.RequestHeaders = incomingHeaders
	if err != nil {
		return nil, &service.GatewayParameterError{Path: "parameter_overrides", Reason: "Invalid request parameter overrides"}
	}
	purchaseInput := billingexpr.RequestInput{Headers: map[string]string{}}
	result := &gatewayPreparedWebSocket{Context: candidate, Info: info}
	if call.Native != nil && call.Native.TransportOnly {
		prepared, err := call.Prepare(candidate, info)
		if err != nil {
			return nil, service.ErrGatewayUnavailable
		}
		for name, values := range prepared.Headers() {
			purchaseInput.Headers[strings.ToLower(name)] = strings.Join(values, ",")
		}
		result.Transport = prepared
		result.InputUsage = &dto.Usage{}
		result.Facts = service.GatewayRouteFacts{Usage: &dto.Usage{}, SaleInput: saleInput, PurchaseInput: purchaseInput}
		return result, nil
	}
	originalTokens := 0
	if call.Native != nil && call.Native.InputUsage != nil {
		originalTokens = call.Native.InputUsage.PromptTokens
	} else {
		common.SetContextKey(candidate, constant.ContextKeyOriginalModel, info.TokenizerModel)
		var original *relaycommon.ResponsesInputFacts
		original, err = call.EstimateInput(candidate, info, &call.Request, selection.Capabilities)
		if err == nil {
			originalTokens = original.Usage.PromptTokens
		}
	}
	if err != nil {
		var parameter *service.GatewayParameterError
		if errors.As(err, &parameter) {
			return nil, parameter
		}
		return nil, &service.GatewayParameterError{Path: "input", Reason: err.Error()}
	}
	if failure := service.GatewayResponsesCapabilitiesFailure(&call.Request, selection.Capabilities, originalTokens, call.Protocol() == protocol.OpenAIRealtime); failure != nil {
		return nil, failure
	}
	validateMultipliers := helper.ValidateResponsesMultipliers
	if call.Protocol() == protocol.OpenAIRealtime {
		validateMultipliers = helper.ValidateRealtimeMultipliers
	}
	if err := validateMultipliers(body); err != nil {
		path := "request"
		var field interface{ ParameterPath() string }
		if errors.As(err, &field) {
			path = field.ParameterPath()
		}
		return nil, &service.GatewayParameterError{Path: path, Reason: err.Error()}
	}
	inputUsage := &dto.Usage{}
	if call.Native != nil && call.Native.EstimateInput != nil {
		inputUsage, err = call.Native.EstimateInput(body, selection.Capabilities)
		if err == nil {
			body, err = helper.NormalizeRealtimeRequestLimits(body, selection.Capabilities, inputUsage.PromptTokens)
			if err != nil {
				path := "request"
				var field interface{ ParameterPath() string }
				if errors.As(err, &field) {
					path = field.ParameterPath()
				}
				return nil, &service.GatewayParameterError{Path: path, Reason: err.Error()}
			}
		}
	} else {
		var effective dto.OpenAIResponsesRequest
		if common.Unmarshal(body, &effective) != nil {
			return nil, &service.GatewayParameterError{Path: "request", Reason: "Invalid effective WebSocket request"}
		}
		info.ResponsesInputFacts, err = call.EstimateInput(candidate, info, &effective, selection.Capabilities)
		if err == nil {
			inputUsage = info.ResponsesInputFacts.Usage
		}
	}
	nativeModalities := call.Protocol() == protocol.OpenAIRealtime
	var effective dto.OpenAIResponsesRequest
	if err != nil {
		var parameter *service.GatewayParameterError
		if errors.As(err, &parameter) {
			return nil, parameter
		}
		return nil, &service.GatewayParameterError{Path: "input", Reason: err.Error()}
	}
	if common.Unmarshal(body, &effective) != nil {
		return nil, &service.GatewayParameterError{Path: "request", Reason: "Invalid effective WebSocket request"}
	}
	if effective.Model != info.UpstreamModelName {
		return nil, &service.GatewayParameterError{Path: "model", Reason: "Connection model is locked"}
	}
	if failure := service.GatewayResponsesCapabilitiesFailure(&effective, selection.Capabilities, inputUsage.PromptTokens, nativeModalities); failure != nil {
		return nil, failure
	}
	if effective.MaxOutputTokens == nil && call.Protocol() == protocol.OpenAIResponses {
		var limit int64
		if maximum := selection.Capabilities.Limits.MaxOutputTokens; maximum != nil {
			limit = *maximum
		}
		if contextLength := selection.Capabilities.Limits.ContextLength; contextLength != nil {
			remaining := *contextLength - int64(inputUsage.PromptTokens)
			if limit == 0 {
				limit = remaining
			} else {
				limit = min(limit, remaining)
			}
		}
		if limit > 0 {
			document = ojson.NewDoc(body)
			if document.Set("max_output_tokens", limit) != nil {
				return nil, &service.GatewayParameterError{Path: "max_output_tokens", Reason: "Invalid effective WebSocket request"}
			}
			body = document.Bytes()
			effective.MaxOutputTokens = common.GetPointer(uint(limit))
		}
	}
	if failure := service.GatewayResponsesCapabilitiesFailure(&effective, selection.Capabilities, inputUsage.PromptTokens, nativeModalities); failure != nil {
		return nil, failure
	}
	if effective.MaxOutputTokens != nil {
		mean = min(mean, int64(*effective.MaxOutputTokens))
	}
	if limit := selection.Capabilities.Limits.MaxOutputTokens; limit != nil {
		mean = min(mean, *limit)
	}
	if limit := selection.Capabilities.Limits.ContextLength; limit != nil {
		mean = min(mean, *limit-int64(inputUsage.PromptTokens))
	}
	if mean < 0 {
		return nil, &service.GatewayParameterError{Path: "input", Reason: "Context token limit exceeded"}
	}
	estimated := service.GatewayUsageSnapshot(inputUsage)
	estimated.CompletionTokens, estimated.TotalTokens = int(mean), estimated.PromptTokens+int(mean)
	if call.Protocol() == protocol.OpenAIRealtime {
		for _, path := range []string{"audio.output.format.type", "output_audio_format"} {
			if format := ojson.GetBytes(body, path); format.Exists() {
				if _, valid := service.NormalizeRealtimeAudioFormat(format.String()); format.Type != ojson.String || !valid {
					return nil, &service.GatewayParameterError{Path: path, Reason: "Unsupported Realtime response audio format"}
				}
			}
		}
		if rate := ojson.GetBytes(body, "audio.output.format.rate"); rate.Exists() && (rate.Type != ojson.Number || rate.Raw != "24000") {
			return nil, &service.GatewayParameterError{Path: "audio.output.format.rate", Reason: "PCM audio requires a sample rate of 24000"}
		}
		outputs := ojson.GetBytes(body, "output_modalities")
		if !outputs.Exists() {
			outputs = ojson.GetBytes(body, "modalities")
		}
		if outputs.Exists() && !outputs.IsArray() {
			return nil, &service.GatewayParameterError{Path: "output_modalities", Reason: "Invalid output modalities"}
		}
		audio := !outputs.Exists() && slices.Contains(selection.Capabilities.OutputModalities, capability.Audio)
		for _, output := range outputs.Array() {
			if output.Type != ojson.String || !slices.Contains(selection.Capabilities.OutputModalities, capability.Modality(output.String())) {
				return nil, &service.GatewayParameterError{Path: "output_modalities", Reason: "Output modality is not supported on this channel"}
			}
			if output.String() == "audio" {
				audio = true
			}
		}
		if audio {
			estimated.CompletionTokenDetails.AudioTokens = int(mean)
		}
	}
	payload, err := call.CreatePayload(body)
	if err != nil {
		return nil, &service.GatewayParameterError{Path: "request", Reason: "Invalid effective WebSocket request"}
	}
	prepared, err := call.Prepare(candidate, info)
	if err != nil {
		return nil, service.ErrGatewayUnavailable
	}
	for name, values := range prepared.Headers() {
		purchaseInput.Headers[strings.ToLower(name)] = strings.Join(values, ",")
	}
	result.Transport = prepared
	purchaseInput.Body = payload
	info.SetEstimatePromptTokens(inputUsage.PromptTokens)
	selection.InputTokens = inputUsage.PromptTokens
	result.Payload, result.InputUsage = payload, inputUsage
	result.Facts = service.GatewayRouteFacts{Usage: estimated, SaleInput: saleInput, PurchaseInput: purchaseInput}
	return result, nil
}
