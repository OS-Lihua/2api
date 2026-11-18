// Package nontextprotocol implements the public synchronous embedding, image,
// audio and rerank protocols. Provider wire dialects compose these handlers.
package nontextprotocol

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/gemini"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

// Handler preserves the selected protocol and audio response format across
// conversion and response parsing. ConvertRequest returns a DTO; provider wire
// conversion may subsequently return io.Reader for multipart or binary bodies.
type Handler struct {
	kind   protocol.Type
	openai openai.Protocol
}

func New(kind protocol.Type) (channel.ProtocolHandler, error) {
	if strings.HasPrefix(string(kind), "task:") {
		return nil, fmt.Errorf("protocol %q uses the existing TaskAdaptor entry point", kind)
	}
	if !channel.IsNonTextProtocol(kind) {
		return nil, fmt.Errorf("unsupported non-text protocol %q", kind)
	}
	return &Handler{kind: kind}, nil
}

func (p *Handler) ConvertRequest(c *gin.Context, info *relaycommon.RelayInfo, request any) (any, error) {
	if p.kind == protocol.AlphaSearch || p.kind == protocol.DeepSeekTools || p.kind == protocol.OpenAIModerations {
		native, ok := request.(*helper.NativeOperationRequest)
		if !ok || native == nil {
			return nil, errors.New("missing native operation request")
		}
		supported := false
		switch info.ChannelType {
		case constant.ChannelTypeNewAPI, constant.ChannelTypeSub2API, constant.ChannelTypeAdvancedCustom:
			supported = true
		case constant.ChannelTypeCodex:
			supported = p.kind == protocol.AlphaSearch
		case constant.ChannelTypeDeepSeek:
			supported = p.kind == protocol.DeepSeekTools
		case constant.ChannelTypeOpenAI:
			supported = p.kind == protocol.OpenAIModerations
		}
		if !supported {
			return nil, errors.New("provider does not support native operation")
		}
		if info.ChannelType == constant.ChannelTypeAdvancedCustom {
			config := info.ChannelOtherSettings.AdvancedCustom
			if config == nil {
				return nil, errors.New("missing native route")
			}
			route, ok := config.MatchPathForModel(c.Request.URL.Path, info.OriginModelName)
			if !ok || !route.IsNative() {
				return nil, errors.New("native operation requires a pass-through route")
			}
		}
		body := append([]byte(nil), native.RawBody...)
		if (p.kind != protocol.DeepSeekTools || ojson.GetBytes(body, "model").Exists()) && ojson.GetBytes(body, "model").String() != info.UpstreamModelName {
			document := ojson.NewDoc(body)
			if err := document.Set("model", info.UpstreamModelName); err != nil {
				return nil, err
			}
			body = document.Bytes()
		}
		info.FinalRequestRelayFormat, _ = p.kind.RelayFormat()
		return common.RawMessage(body), nil
	}
	format, _ := channel.ProtocolRelayFormat(p.kind)
	if info.RelayFormat != format && !(p.kind == protocol.OpenAIEmbeddings && info.RelayFormat == types.RelayFormatGemini || p.kind == protocol.GeminiEmbed && info.RelayFormat == types.RelayFormatEmbedding) {
		return nil, fmt.Errorf("protocol %q does not support downstream format %q", p.kind, info.RelayFormat)
	}
	info.FinalRequestRelayFormat = format
	switch r := request.(type) {
	case dto.GeminiEmbeddingRequest:
		request = &r
	case dto.GeminiBatchEmbeddingRequest:
		request = &r
	case *dto.EmbeddingRequest:
		if r != nil {
			request = *r
		}
	case *dto.ImageRequest:
		if r != nil {
			request = *r
		}
	case *dto.AudioRequest:
		if r != nil {
			request = *r
		}
	case *dto.RerankRequest:
		if r != nil {
			request = *r
		}
	}
	switch p.kind {
	case protocol.OpenAIEmbeddings:
		if r, ok := request.(dto.EmbeddingRequest); ok {
			return r, nil
		}
		return geminiEmbeddingToOpenAI(info, request)
	case protocol.GeminiEmbed:
		switch r := request.(type) {
		case dto.EmbeddingRequest:
			converted, err := (&gemini.Protocol{}).ConvertEmbeddingRequest(c, info, r)
			if err != nil {
				return nil, err
			}
			if r.Dimensions != nil {
				for _, item := range converted.(map[string]any)["requests"].([]map[string]any) {
					item["outputDimensionality"] = *r.Dimensions
				}
			}
			return converted, nil
		case *dto.GeminiEmbeddingRequest:
			if r != nil {
				info.IsGeminiBatchEmbedding = false
				r.Model = "models/" + info.UpstreamModelName
				return r, nil
			}
		case *dto.GeminiBatchEmbeddingRequest:
			if r != nil {
				for _, item := range r.Requests {
					if item == nil {
						return nil, fmt.Errorf("nil Gemini embedding request")
					}
				}
				info.IsGeminiBatchEmbedding = true
				r.SetModelName("models/" + info.UpstreamModelName)
				return r, nil
			}
		}
	case protocol.OpenAIImages:
		if r, ok := request.(dto.ImageRequest); ok {
			if _, err := r.ImageCount(false); err != nil {
				return nil, err
			}
			return r, nil
		}
	case protocol.OpenAIAudioSpeech, protocol.OpenAIAudioTranscription:
		if r, ok := request.(dto.AudioRequest); ok {
			p.openai.ResponseFormat = r.ResponseFormat
			return r, nil
		}
	case protocol.Rerank:
		if r, ok := request.(dto.RerankRequest); ok {
			return r, nil
		}
	}
	return nil, fmt.Errorf("invalid request %T for protocol %q", request, p.kind)
}

// Only plain text and the shared dimensions field have a stable embedding
// conversion. Reject Gemini task/title hints and media instead of dropping them.
func geminiEmbeddingToOpenAI(info *relaycommon.RelayInfo, request any) (any, error) {
	var items []*dto.GeminiEmbeddingRequest
	switch r := request.(type) {
	case *dto.GeminiEmbeddingRequest:
		info.IsGeminiBatchEmbedding = false
		items = []*dto.GeminiEmbeddingRequest{r}
	case *dto.GeminiBatchEmbeddingRequest:
		if r == nil {
			return nil, fmt.Errorf("nil Gemini embedding batch")
		}
		info.IsGeminiBatchEmbedding = true
		items = r.Requests
	default:
		return nil, fmt.Errorf("invalid embedding request %T", request)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("empty Gemini embedding batch")
	}
	inputs := make([]string, 0, len(items))
	var dimensions *int
	for i, item := range items {
		if item == nil || item.TaskType != "" || item.Title != "" {
			return nil, fmt.Errorf("Gemini taskType and title cannot be converted to OpenAI embeddings")
		}
		if i > 0 && (dimensions == nil && item.OutputDimensionality != nil ||
			dimensions != nil && (item.OutputDimensionality == nil || *dimensions != *item.OutputDimensionality)) {
			return nil, fmt.Errorf("embedding batch dimensions must match")
		}
		dimensions = item.OutputDimensionality
		parts := make([]string, 0, len(item.Content.Parts))
		for _, part := range item.Content.Parts {
			if part.Text == "" || part.InlineData != nil || part.FileData != nil || part.FunctionCall != nil || part.FunctionResponse != nil {
				return nil, fmt.Errorf("embedding conversion requires plain text")
			}
			parts = append(parts, part.Text)
		}
		if len(parts) == 0 {
			return nil, fmt.Errorf("embedding input is empty")
		}
		inputs = append(inputs, strings.Join(parts, "\n"))
	}
	return dto.EmbeddingRequest{Model: info.UpstreamModelName, Input: inputs, Dimensions: dimensions}, nil
}

func (p *Handler) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	info.FinalRequestRelayFormat, _ = channel.ProtocolRelayFormat(p.kind)
	info.UpstreamProtocol = p.kind
	if p.kind == protocol.AlphaSearch || p.kind == protocol.DeepSeekTools || p.kind == protocol.OpenAIModerations {
		return nativeOperationResponse(c, resp, info, p.kind)
	}
	if p.kind == protocol.GeminiEmbed || p.kind == protocol.OpenAIEmbeddings && (info.ResponseFacts != nil || info.RelayFormat == types.RelayFormatGemini) {
		return embeddingResponse(c, resp, info, p.kind)
	}
	return p.openai.DoResponse(c, resp, info)
}

// Native operations preserve their JSON envelope and report delivery separately
// from procurement. An empty DeepSeek result is a completed, uncharged response.
func nativeOperationResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo, kind protocol.Type) (any, *types.NewAPIError) {
	defer resp.Body.Close()
	body, readErr := io.ReadAll(channel.BoundGatewayResponseBody(resp.Body))
	facts := info.ResponseFacts
	usage := &dto.Usage{}
	if facts == nil {
		return nil, types.NewError(errors.New("missing operation facts"), types.ErrorCodeBadResponse)
	}
	_ = facts.RecordChatUsage(usage, body, "usage")
	promptPath, completionPath := "usage.prompt_tokens", "usage.completion_tokens"
	if !ojson.GetBytes(body, promptPath).Exists() {
		promptPath = "usage.input_tokens"
	}
	if !ojson.GetBytes(body, completionPath).Exists() {
		completionPath = "usage.output_tokens"
	}
	facts.RecordNativeUsage(usage, body, promptPath, completionPath)
	if readErr != nil {
		return usage, types.NewError(errors.New("native operation response read failed"), types.ErrorCodeReadResponseBodyFailed)
	}
	var object map[string]common.RawMessage
	if common.Unmarshal(body, &object) != nil || object == nil || info.IsStream || strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return usage, types.NewError(errors.New("invalid native operation response"), types.ErrorCodeBadResponse)
	}
	failure := ojson.GetBytes(body, "error")
	if resp.StatusCode != http.StatusOK || failure.Exists() && failure.Type != ojson.Null {
		facts.UpstreamErrorCode = ojson.GetBytes(body, "error.code").String()
		return usage, types.WithOpenAIError(types.OpenAIError{Code: facts.UpstreamErrorCode, Type: ojson.GetBytes(body, "error.type").String(), Message: "Upstream request failed"}, http.StatusBadGateway)
	}
	valid, billable := false, false
	switch kind {
	case protocol.OpenAIModerations:
		results := ojson.GetBytes(body, "results")
		valid = results.IsArray() && len(results.Array()) > 0
		for _, result := range results.Array() {
			flagged := result.Get("flagged")
			valid = valid && result.IsObject() && (flagged.Type == ojson.True || flagged.Type == ojson.False)
		}
		billable = valid
	case protocol.DeepSeekTools:
		if info.RelayMode == relayconstant.RelayModeToolsFetch {
			content := ojson.GetBytes(body, "content")
			valid, billable = content.Type == ojson.String, content.Type == ojson.String && content.String() != ""
		} else {
			results := ojson.GetBytes(body, "results")
			valid, billable = results.IsArray(), results.IsArray() && len(results.Array()) > 0
		}
	case protocol.AlphaSearch:
		// A typed search result, including an empty result set, completes one search.
		results := ojson.GetBytes(body, "results")
		valid, billable = results.IsArray(), results.IsArray()
		if valid {
			facts.SourceToolCount = common.GetPointer(1)
		}
	}
	if !valid {
		return usage, types.WithOpenAIError(types.OpenAIError{Code: "empty_response", Type: "server_error", Message: "Invalid upstream response"}, http.StatusBadGateway)
	}
	facts.SourceOutputObserved = billable
	c.Header("Content-Type", "application/json")
	c.Status(resp.StatusCode)
	n, err := c.Writer.Write(body)
	if err != nil || n != len(body) {
		facts.DownstreamWriteFailed = true
		return usage, types.NewError(errors.New("native operation delivery failed"), types.ErrorCodeBadResponse)
	}
	facts.NativeOperationCompleted = true
	if billable {
		facts.MarkBusinessOutput()
	}
	return usage, nil
}
