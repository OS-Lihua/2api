// Package nontextprotocol implements the public synchronous embedding, image,
// audio and rerank protocols. Provider wire dialects compose these handlers.
package nontextprotocol

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/gemini"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
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
	if p.kind == protocol.GeminiEmbed {
		return geminiEmbeddingResponse(c, resp, info)
	}
	if p.kind == protocol.OpenAIEmbeddings && info.RelayFormat == types.RelayFormatGemini {
		return openAIEmbeddingToGemini(c, resp, info)
	}
	return p.openai.DoResponse(c, resp, info)
}

func openAIEmbeddingToGemini(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeReadResponseBodyFailed)
	}
	var response struct {
		Data  []dto.OpenAIEmbeddingResponseItem `json:"data"`
		Usage *dto.Usage                        `json:"usage"`
	}
	if err = common.Unmarshal(body, &response); err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	if len(response.Data) == 0 {
		return nil, types.NewError(fmt.Errorf("empty embedding response"), types.ErrorCodeBadResponseBody)
	}
	usage := response.Usage
	if usage == nil {
		usage = service.ResponseText2Usage(c, "", info.UpstreamModelName, info.GetEstimatePromptTokens())
	}
	var metadata *dto.GeminiEmbeddingUsageMetadata
	if response.Usage != nil {
		metadata = &dto.GeminiEmbeddingUsageMetadata{PromptTokenCount: usage.PromptTokens}
	}
	var result any
	if info.IsGeminiBatchEmbedding {
		embeddings := make([]*dto.ContentEmbedding, 0, len(response.Data))
		for _, item := range response.Data {
			embeddings = append(embeddings, &dto.ContentEmbedding{Values: item.Embedding})
		}
		result = dto.GeminiBatchEmbeddingResponse{Embeddings: embeddings, UsageMetadata: metadata}
	} else {
		result = dto.GeminiEmbeddingResponse{Embedding: dto.ContentEmbedding{Values: response.Data[0].Embedding}, UsageMetadata: metadata}
	}
	body, err = common.Marshal(result)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	service.IOCopyBytesGracefully(c, resp, body)
	return usage, nil
}
