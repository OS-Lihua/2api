package nontextprotocol

import (
	"fmt"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// Gemini embedding usage is optional. An explicitly reported zero must remain
// zero; missing metadata retains the existing local input estimate.
func geminiEmbeddingResponse(c *gin.Context, response *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeReadResponseBodyFailed)
	}
	var wire struct {
		Embedding     *dto.ContentEmbedding             `json:"embedding"`
		Embeddings    []*dto.ContentEmbedding           `json:"embeddings"`
		UsageMetadata *dto.GeminiEmbeddingUsageMetadata `json:"usageMetadata"`
	}
	if err := common.Unmarshal(body, &wire); err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	embeddings := wire.Embeddings
	if wire.Embedding != nil {
		embeddings = []*dto.ContentEmbedding{wire.Embedding}
	}
	if len(embeddings) == 0 {
		return nil, types.NewError(fmt.Errorf("empty Gemini embedding response"), types.ErrorCodeBadResponseBody)
	}
	usage := &dto.Usage{}
	if wire.UsageMetadata == nil {
		usage = service.ResponseText2Usage(c, "", info.UpstreamModelName, info.GetEstimatePromptTokens())
	} else {
		metadata := &dto.GeminiUsageMetadata{
			PromptTokenCount:    wire.UsageMetadata.PromptTokenCount,
			TotalTokenCount:     wire.UsageMetadata.PromptTokenCount,
			PromptTokensDetails: wire.UsageMetadata.PromptTokenDetails,
		}
		usage = relayconvert.UsageFromGeminiMetadata(metadata, 0)
	}
	if info.RelayFormat != types.RelayFormatGemini {
		result := dto.OpenAIEmbeddingResponse{Object: "list", Model: info.UpstreamModelName, Usage: *usage, Data: make([]dto.OpenAIEmbeddingResponseItem, 0, len(embeddings))}
		for i, embedding := range embeddings {
			if embedding == nil {
				return nil, types.NewError(fmt.Errorf("nil Gemini embedding"), types.ErrorCodeBadResponseBody)
			}
			result.Data = append(result.Data, dto.OpenAIEmbeddingResponseItem{Object: "embedding", Index: i, Embedding: embedding.Values})
		}
		body, err = common.Marshal(result)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
	}
	service.IOCopyBytesGracefully(c, response, body)
	return usage, nil
}
