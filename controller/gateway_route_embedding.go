package controller

import (
	"bytes"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

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
