package controller

import (
	"bytes"
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
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/tokenkit"
	"github.com/gin-gonic/gin"
)

// Native search and classification operations use the same attempt and money
// lifecycle without projecting their requests or results through Chat.
func prepareGatewayNativeOperation(c *gin.Context, selection *service.GatewaySelection, native dto.Request, kind protocol.Type, started time.Time) (*gatewayPreparedRequest, error) {
	original, ok := native.(*helper.NativeOperationRequest)
	if !ok || original == nil || selection.Channel.Protocol != kind {
		return nil, service.ErrGatewayCapability
	}
	candidate := c.Copy()
	if err := middleware.SetupContextForGatewayCandidate(candidate, selection.Provider, selection.Channel.Model.ModelName); err != nil {
		return nil, service.ErrGatewayUnavailable
	}
	var overrides map[string]any
	if common.UnmarshalJsonStr(selection.Channel.ParameterOverrides, &overrides) != nil {
		return nil, service.ErrGatewayUnavailable
	}
	common.SetContextKey(candidate, constant.ContextKeyChannelParamOverride, overrides)
	snapshot, err := helper.DecodeNativeOperationRequest(original.RawBody, kind, original.Model)
	if err != nil {
		return nil, service.ErrGatewayCapability
	}
	format, _ := kind.RelayFormat()
	var metadataRequest dto.Request = snapshot
	if kind == protocol.AlphaSearch {
		metadataRequest = &snapshot.AlphaSearchRequest
	}
	info, err := relaycommon.GenRelayInfo(candidate, format, metadataRequest, nil)
	if err != nil {
		return nil, service.ErrGatewayCapability
	}
	info.Request = snapshot
	info.StartTime, info.TokenizerModel = started, selection.Channel.Model.ModelName
	info.InitChannelMeta(candidate)
	info.UpstreamModelName, info.IsModelMapped = selection.Channel.UpstreamModelName, true
	info.ResponseFacts = &relaycommon.ResponseFacts{OnBusinessOutput: func() {}}
	common.SetContextKey(candidate, constant.ContextKeyResponseFacts, info.ResponseFacts)
	common.SetContextKey(candidate, constant.ContextKeyOriginalModel, info.TokenizerModel)
	if info.StreamStatus == nil {
		info.StreamStatus = relaycommon.NewStreamStatus()
	}
	saleInput, err := helper.ResolveIncomingBillingExprRequestInput(candidate, info)
	if err != nil {
		return nil, service.ErrGatewayCapability
	}
	adaptor, err := relay.GetChannelAdaptor(info.ApiType, kind)
	if err != nil {
		return nil, service.ErrGatewayCapability
	}
	converted, err := adaptor.ConvertRequest(candidate, info, snapshot)
	if err != nil {
		return nil, service.ErrGatewayCapability
	}
	raw, ok := converted.(common.RawMessage)
	if !ok {
		return nil, service.ErrGatewayCapability
	}
	encoded := append([]byte(nil), raw...)
	if len(info.ParamOverride) > 0 {
		encoded, err = relaycommon.ApplyParamOverrideWithRelayInfo(encoded, info)
		if err != nil {
			return nil, service.ErrGatewayCapability
		}
	}
	effective, err := helper.DecodeNativeOperationRequest(encoded, kind, info.UpstreamModelName)
	if err != nil {
		return nil, service.ErrGatewayCapability
	}
	modelName := ojson.GetBytes(encoded, "model")
	if modelName.Exists() && (modelName.Type != ojson.String || modelName.String() != info.UpstreamModelName) || kind != protocol.DeepSeekTools && !modelName.Exists() {
		return nil, service.ErrGatewayCapability
	}
	for _, request := range []*helper.NativeOperationRequest{original, effective} {
		caps := selection.Capabilities
		if !slices.Contains(caps.InputModalities, capability.Text) || request.HasImages && !slices.Contains(caps.InputModalities, capability.Image) {
			return nil, service.ErrGatewayCapability
		}
		tokens := tokenkit.Count(info.TokenizerModel, string(request.RawBody))
		if caps.Limits.ContextLength != nil && int64(tokens) > *caps.Limits.ContextLength {
			return nil, service.ErrGatewayCapability
		}
		for _, prefix := range []string{"", "parameters."} {
			for _, field := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens", "min_tokens"} {
				limit := ojson.GetBytes(request.RawBody, prefix+field)
				if limit.Exists() && limit.Type != ojson.Null && (caps.Limits.MaxOutputTokens != nil && limit.Int() > *caps.Limits.MaxOutputTokens || caps.Limits.ContextLength != nil && limit.Int() > *caps.Limits.ContextLength-int64(tokens)) {
					return nil, service.ErrGatewayCapability
				}
			}
		}
	}
	tokens := tokenkit.Count(info.TokenizerModel, string(encoded))
	selection.InputTokens = tokens
	usage := &dto.Usage{PromptTokens: tokens, TotalTokens: tokens}
	headers, deferred, err := adaptor.PreviewHeaders(candidate, info, bytes.NewReader(encoded))
	if err != nil {
		return nil, service.ErrGatewayCapability
	}
	purchaseInput := billingexpr.RequestInput{Body: encoded, Headers: map[string]string{}, DeferredHeaders: deferred}
	for key, values := range headers {
		purchaseInput.Headers[strings.ToLower(key)] = strings.Join(values, ",")
	}
	if kind == protocol.AlphaSearch {
		saleInput.ToolCount, purchaseInput.ToolCount = common.GetPointer(1), common.GetPointer(1)
	}
	return &gatewayPreparedRequest{Context: candidate, Info: info, Adaptor: adaptor, Converted: converted, Encoded: encoded, SaleInput: saleInput, Usage: usage, Choices: 1,
		Facts: service.GatewayRouteFacts{Usage: usage, SaleInput: saleInput, PurchaseInput: purchaseInput}}, nil
}

// Embeddings and rerank share the Attempt and settlement lifecycle with text,
// but reserve only input usage and validate their own effective wire DTOs.
func prepareGatewayEmbeddingRoute(c *gin.Context, selection *service.GatewaySelection, native dto.Request, kind protocol.Type, accepted []string, started time.Time) (*gatewayPreparedRequest, error) {
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
	if err := middleware.SetupContextForGatewayCandidate(candidate, selection.Provider, selection.Channel.Model.ModelName); err != nil {
		return nil, service.ErrGatewayUnavailable
	}
	var overrides map[string]any
	if common.UnmarshalJsonStr(selection.Channel.ParameterOverrides, &overrides) != nil {
		return nil, service.ErrGatewayUnavailable
	}
	common.SetContextKey(candidate, constant.ContextKeyChannelParamOverride, overrides)
	snapshot, err := service.CloneGatewayRequest(native)
	if err != nil {
		return nil, service.ErrGatewayConversion
	}
	format, err := kind.RelayFormat()
	if err != nil {
		return nil, service.ErrGatewayConversion
	}
	info, err := relaycommon.GenRelayInfo(candidate, format, snapshot, nil)
	if err != nil {
		return nil, service.ErrGatewayCapability
	}
	info.StartTime = started
	info.TokenizerModel = selection.Channel.Model.ModelName
	info.InitChannelMeta(candidate)
	info.UpstreamModelName = selection.Channel.UpstreamModelName
	info.IsModelMapped = true
	info.ResponseFacts = &relaycommon.ResponseFacts{OnBusinessOutput: func() {}}
	common.SetContextKey(candidate, constant.ContextKeyResponseFacts, info.ResponseFacts)
	common.SetContextKey(candidate, constant.ContextKeyOriginalModel, info.TokenizerModel)
	if info.StreamStatus == nil {
		info.StreamStatus = relaycommon.NewStreamStatus()
	}
	saleInput, err := helper.ResolveIncomingBillingExprRequestInput(candidate, info)
	if err != nil {
		return nil, service.ErrGatewayCapability
	}
	if _, err := helper.GatewayEmbeddingUsage(candidate, snapshot, info, selection.Capabilities); err != nil {
		return nil, service.ErrGatewayCapability
	}
	if original, ok := snapshot.(*dto.EmbeddingRequest); ok && selection.Channel.Protocol == protocol.GeminiEmbed {
		var diagnostics []types.ConversionDiagnostic
		for _, parameter := range []struct {
			name    string
			present bool
		}{
			{"encoding_format", original.EncodingFormat != "" && original.EncodingFormat != "float"},
			{"user", original.User != ""},
			{"seed", original.Seed != nil},
			{"temperature", original.Temperature != nil},
			{"top_p", original.TopP != nil},
			{"frequency_penalty", original.FrequencyPenalty != nil},
			{"presence_penalty", original.PresencePenalty != nil},
		} {
			if parameter.present {
				diagnostics = append(diagnostics, types.ConversionDiagnostic{Code: "request_parameter_dropped", Path: parameter.name, Message: "The embedding conversion cannot preserve this parameter", Severity: types.ConversionDiagnosticError})
			}
		}
		if len(diagnostics) > 0 {
			return nil, &service.GatewayConversionError{Diagnostics: diagnostics}
		}
	}
	snapshot.SetModelName(info.UpstreamModelName)
	adaptor, err := relay.GetChannelAdaptor(info.ApiType, selection.Channel.Protocol)
	if err != nil {
		return nil, service.ErrGatewayConversion
	}
	converted, err := adaptor.ConvertRequest(candidate, info, snapshot)
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
	var controls struct {
		Stream bool `json:"stream"`
	}
	if common.Unmarshal(encoded, &controls) != nil || controls.Stream {
		return nil, service.ErrGatewayCapability
	}
	var effective dto.Request
	if projector, ok := converted.(channel.NativeRequestProjector); ok {
		effective, err = projector.ProjectNativeRequest(encoded)
	} else {
		switch selection.Channel.Protocol {
		case protocol.OpenAIEmbeddings:
			effective = &dto.EmbeddingRequest{}
		case protocol.GeminiEmbed:
			if info.IsGeminiBatchEmbedding {
				effective = &dto.GeminiBatchEmbeddingRequest{}
			} else {
				effective = &dto.GeminiEmbeddingRequest{}
			}
		case protocol.Rerank:
			effective = &dto.RerankRequest{}
		}
		err = common.Unmarshal(encoded, effective)
	}
	if err != nil || effective == nil {
		return nil, service.ErrGatewayCapability
	}
	modelName := info.UpstreamModelName
	switch wire := effective.(type) {
	case *dto.EmbeddingRequest:
		if wire.Model != modelName {
			return nil, service.ErrGatewayCapability
		}
		count := 1
		if inputs, ok := wire.Input.([]any); ok && len(inputs) > 0 {
			switch inputs[0].(type) {
			case string, []any:
				count = len(inputs)
			}
		}
		info.EmbeddingDimensions = make([]*int, count)
		for i := range info.EmbeddingDimensions {
			info.EmbeddingDimensions[i] = wire.Dimensions
		}
	case *dto.RerankRequest:
		if wire.Model != modelName {
			return nil, service.ErrGatewayCapability
		}
		info.Documents, info.ReturnDocuments = wire.Documents, wire.GetReturnDocuments()
	case *dto.GeminiEmbeddingRequest:
		if wire.Model != "models/"+modelName {
			return nil, service.ErrGatewayCapability
		}
		info.EmbeddingDimensions = []*int{wire.OutputDimensionality}
	case *dto.GeminiBatchEmbeddingRequest:
		for _, item := range wire.Requests {
			if item == nil || item.Model != "models/"+modelName {
				return nil, service.ErrGatewayCapability
			}
			info.EmbeddingDimensions = append(info.EmbeddingDimensions, item.OutputDimensionality)
		}
	default:
		return nil, service.ErrGatewayCapability
	}
	usage, err := helper.GatewayEmbeddingUsage(candidate, effective, info, selection.Capabilities)
	if err != nil {
		return nil, service.ErrGatewayCapability
	}
	selection.InputTokens = usage.PromptTokens
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
	quality := 0
	if kind != selection.Channel.Protocol {
		quality = 1
	}
	return &gatewayPreparedRequest{Context: candidate, Info: info, Adaptor: adaptor, Converted: converted, Encoded: encoded, SaleInput: saleInput, Usage: usage, Choices: 1,
		Facts: service.GatewayRouteFacts{Usage: usage, SaleInput: saleInput, PurchaseInput: purchaseInput, Quality: quality, ConversionLosses: service.GatewayConversionCodes(diagnostics)}}, nil
}
