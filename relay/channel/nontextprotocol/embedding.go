package nontextprotocol

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/protocol"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/common_handler"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Embeddings have input usage only. Preserve its presence before validating the
// payload so a malformed response or failed delivery cannot erase procurement.
func embeddingResponse(c *gin.Context, response *http.Response, info *relaycommon.RelayInfo, kind protocol.Type) (any, *types.NewAPIError) {
	defer response.Body.Close()
	body, readErr := io.ReadAll(response.Body)
	usage := &dto.Usage{}
	promptPath, usagePath := "usage.prompt_tokens", "usage"
	if kind == protocol.GeminiEmbed {
		promptPath, usagePath = "usageMetadata.promptTokenCount", "usageMetadata"
	}
	prompt, _ := relaycommon.NativeUsagePresence(body, promptPath, "", "")
	if !prompt && kind == protocol.OpenAIEmbeddings {
		if total, _ := relaycommon.NativeUsagePresence(body, "usage.total_tokens", "", ""); total {
			prompt, promptPath = true, "usage.total_tokens"
		}
	}
	if prompt {
		usage.PromptTokens = int(gjson.GetBytes(body, promptPath).Int())
	}
	if kind == protocol.OpenAIEmbeddings {
		var details dto.Usage
		clean := relaycommon.SanitizeNativeUsage(body, usagePath)
		if common.UnmarshalJsonStr(gjson.GetBytes(clean, usagePath).Raw, &details) == nil {
			usage.PromptTokensDetails = details.PromptTokensDetails
			usage.InputTokensDetails = details.InputTokensDetails
		}
	} else if gjson.GetBytes(body, usagePath).IsObject() {
		metadata := &dto.GeminiUsageMetadata{PromptTokenCount: usage.PromptTokens, TotalTokenCount: usage.PromptTokens}
		for i, detail := range gjson.GetBytes(body, usagePath+".promptTokenDetails").Array() {
			present, _ := relaycommon.NativeUsagePresence(body, fmt.Sprintf("%s.promptTokenDetails.%d.tokenCount", usagePath, i), "", "")
			if present {
				metadata.PromptTokensDetails = append(metadata.PromptTokensDetails, dto.GeminiPromptTokensDetails{Modality: detail.Get("modality").String(), TokenCount: int(detail.Get("tokenCount").Int())})
			}
		}
		usage = relayconvert.UsageFromGeminiMetadata(metadata, 0)
		if !prompt {
			// A metadata object without a valid input counter must not freeze
			// a synthetic zero that would override the later local estimate.
			usage.BillingUsage = nil
		}
	}
	usage.CompletionTokens, usage.TotalTokens = 0, usage.PromptTokens
	if facts := info.ResponseFacts; facts != nil {
		facts.PromptTokensReported, facts.CompletionTokensReported = prompt, true
		facts.UsageReported, facts.UsageEstimated = prompt, !prompt
		facts.RecordTokenDetailPresence(body, usagePath)
		if kind == protocol.GeminiEmbed {
			// Gemini embeddings use singular promptTokenDetails, while the
			// common Gemini generation presence reader uses promptTokensDetails.
			for i, detail := range gjson.GetBytes(body, usagePath+".promptTokenDetails").Array() {
				present, _ := relaycommon.NativeUsagePresence(body, fmt.Sprintf("%s.promptTokenDetails.%d.tokenCount", usagePath, i), "", "")
				if present {
					switch detail.Get("modality").String() {
					case "IMAGE":
						facts.ImageInputTokensReported = true
					case "AUDIO":
						facts.AudioInputTokensReported = true
					}
				}
			}
		}
	}
	if fault := common_handler.NonTextResponseError(info, body, response.StatusCode); fault != nil {
		return usage, fault
	}
	if readErr != nil {
		return usage, types.NewError(fmt.Errorf("incomplete embedding response"), types.ErrorCodeReadResponseBodyFailed)
	}
	var embeddings []*dto.ContentEmbedding
	var vectorDimensions []int
	if kind == protocol.GeminiEmbed {
		var wire struct {
			Embedding  *dto.ContentEmbedding   `json:"embedding"`
			Embeddings []*dto.ContentEmbedding `json:"embeddings"`
		}
		if err := common.Unmarshal(body, &wire); err != nil {
			return usage, types.NewError(fmt.Errorf("invalid embedding response"), types.ErrorCodeBadResponseBody)
		}
		embeddings = wire.Embeddings
		if !info.IsGeminiBatchEmbedding {
			embeddings = []*dto.ContentEmbedding{wire.Embedding}
		}
		if len(embeddings) == 0 {
			return usage, types.NewError(fmt.Errorf("empty embedding response"), types.ErrorCodeEmptyResponse)
		}
		vectorDimensions = make([]int, len(embeddings))
		for i, embedding := range embeddings {
			if embedding == nil || len(embedding.Values) == 0 {
				return usage, types.NewError(fmt.Errorf("empty embedding vector"), types.ErrorCodeBadResponseBody)
			}
			path := "embedding.values"
			if info.IsGeminiBatchEmbedding {
				path = fmt.Sprintf("embeddings.%d.values", i)
			}
			for _, value := range gjson.GetBytes(body, path).Array() {
				if value.Type != gjson.Number {
					return usage, types.NewError(fmt.Errorf("invalid embedding component"), types.ErrorCodeBadResponseBody)
				}
			}
			vectorDimensions[i] = len(embedding.Values)
		}
	} else {
		var wire struct {
			Data []struct {
				Index     *int              `json:"index"`
				Embedding common.RawMessage `json:"embedding"`
			} `json:"data"`
		}
		if err := common.Unmarshal(body, &wire); err != nil {
			return usage, types.NewError(fmt.Errorf("invalid embedding response"), types.ErrorCodeBadResponseBody)
		}
		if len(wire.Data) == 0 {
			return usage, types.NewError(fmt.Errorf("empty embedding response"), types.ErrorCodeEmptyResponse)
		}
		embeddings = make([]*dto.ContentEmbedding, len(wire.Data))
		vectorDimensions = make([]int, len(wire.Data))
		seen := make(map[int]bool, len(wire.Data))
		for _, item := range wire.Data {
			if item.Index == nil || *item.Index < 0 || *item.Index >= len(wire.Data) || seen[*item.Index] {
				return usage, types.NewError(fmt.Errorf("invalid embedding index"), types.ErrorCodeBadResponseBody)
			}
			seen[*item.Index] = true
			var values []float64
			if err := common.Unmarshal(item.Embedding, &values); err != nil || len(values) == 0 {
				var encoded string
				if common.Unmarshal(item.Embedding, &encoded) != nil || encoded == "" || info.RelayFormat == types.RelayFormatGemini {
					return usage, types.NewError(fmt.Errorf("invalid embedding vector"), types.ErrorCodeBadResponseBody)
				}
				decoded, err := base64.StdEncoding.DecodeString(encoded)
				if err != nil || len(decoded) == 0 || len(decoded)%4 != 0 {
					return usage, types.NewError(fmt.Errorf("invalid base64 embedding"), types.ErrorCodeBadResponseBody)
				}
				vectorDimensions[*item.Index] = len(decoded) / 4
			} else {
				for _, value := range gjson.ParseBytes(item.Embedding).Array() {
					if value.Type != gjson.Number {
						return usage, types.NewError(fmt.Errorf("invalid embedding component"), types.ErrorCodeBadResponseBody)
					}
				}
				vectorDimensions[*item.Index] = len(values)
			}
			embeddings[*item.Index] = &dto.ContentEmbedding{Values: values}
		}
	}
	if len(info.EmbeddingDimensions) > 0 {
		if len(embeddings) != len(info.EmbeddingDimensions) {
			return usage, types.NewError(fmt.Errorf("embedding response count does not match input"), types.ErrorCodeBadResponseBody)
		}
		for i, expected := range info.EmbeddingDimensions {
			if expected != nil && vectorDimensions[i] != *expected {
				return usage, types.NewError(fmt.Errorf("embedding response dimension does not match request"), types.ErrorCodeBadResponseBody)
			}
		}
	}
	if !prompt {
		estimated := service.ResponseText2Usage(c, "", info.UpstreamModelName, info.GetEstimatePromptTokens())
		usage.PromptTokens, usage.TotalTokens = estimated.PromptTokens, estimated.PromptTokens
	}
	if facts := info.ResponseFacts; facts != nil {
		zero := 0
		facts.SourceOutputObserved, facts.SourceOutputTokens = true, &zero
	}
	var result any
	if kind == protocol.GeminiEmbed && info.RelayFormat != types.RelayFormatGemini {
		converted := dto.OpenAIEmbeddingResponse{Object: "list", Model: info.UpstreamModelName, Usage: *usage, Data: make([]dto.OpenAIEmbeddingResponseItem, 0, len(embeddings))}
		for i, embedding := range embeddings {
			converted.Data = append(converted.Data, dto.OpenAIEmbeddingResponseItem{Object: "embedding", Index: i, Embedding: embedding.Values})
		}
		result = converted
	} else if kind == protocol.OpenAIEmbeddings && info.RelayFormat == types.RelayFormatGemini {
		var metadata *dto.GeminiEmbeddingUsageMetadata
		if prompt {
			metadata = &dto.GeminiEmbeddingUsageMetadata{PromptTokenCount: usage.PromptTokens}
		}
		if info.IsGeminiBatchEmbedding {
			result = dto.GeminiBatchEmbeddingResponse{Embeddings: embeddings, UsageMetadata: metadata}
		} else {
			if len(embeddings) != 1 {
				return usage, types.NewError(fmt.Errorf("invalid single embedding response"), types.ErrorCodeBadResponseBody)
			}
			result = dto.GeminiEmbeddingResponse{Embedding: *embeddings[0], UsageMetadata: metadata}
		}
	}
	if result != nil {
		var err error
		body, err = common.Marshal(result)
		if err != nil {
			return usage, types.NewError(fmt.Errorf("invalid embedding response"), types.ErrorCodeBadResponseBody)
		}
	}
	if info.ResponseFacts != nil {
		if err := helper.WriteGatewayJSONResponse(c, response, info.ResponseFacts, body, false); err != nil {
			info.ResponseFacts.DownstreamWriteFailed = true
			return usage, types.NewError(fmt.Errorf("embedding delivery failed"), types.ErrorCodeBadResponse)
		}
		info.ResponseFacts.MarkBusinessOutput()
	} else {
		service.IOCopyBytesGracefully(c, response, body)
	}
	return usage, nil
}
