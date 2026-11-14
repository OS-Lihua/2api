package helper

import (
	"errors"
	"math"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/capability"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// GatewayEmbeddingInput validates embedding and rerank controls before either
// reservation or token counting. Token-array embeddings already contain tokens.
func GatewayEmbeddingInput(request dto.Request) (*types.TokenCountMeta, int, error) {
	invalid := errors.New("invalid embedding or rerank request")
	validDimensions := func(dimensions *int) bool {
		return dimensions == nil || *dimensions > 0 && !ExceedsMaxTokensLimit(common.GetPointer(uint(*dimensions)))
	}
	switch request := request.(type) {
	case *dto.EmbeddingRequest:
		if request == nil || !validDimensions(request.Dimensions) || request.EncodingFormat != "" && request.EncodingFormat != "float" && request.EncodingFormat != "base64" {
			return nil, 0, invalid
		}
		switch input := request.Input.(type) {
		case string:
			if input == "" {
				return nil, 0, invalid
			}
		case []any:
			if len(input) == 0 {
				return nil, 0, invalid
			}
			tokens := 0
			for _, item := range input {
				switch input[0].(type) {
				case float64:
					value, ok := item.(float64)
					if !ok || value < 0 || value > math.MaxInt32 || math.Trunc(value) != value {
						return nil, 0, invalid
					}
					tokens++
				case []any:
					ids, ok := item.([]any)
					if !ok || len(ids) == 0 {
						return nil, 0, invalid
					}
					for _, id := range ids {
						value, ok := id.(float64)
						if !ok || value < 0 || value > math.MaxInt32 || math.Trunc(value) != value {
							return nil, 0, invalid
						}
					}
					tokens += len(ids)
				case string:
					text, ok := item.(string)
					if !ok || text == "" {
						return nil, 0, invalid
					}
				case map[string]any:
					if _, ok := item.(map[string]any); !ok {
						return nil, 0, invalid
					}
					body, err := common.Marshal(item)
					var part dto.MediaContent
					if err != nil || common.Unmarshal(body, &part) != nil {
						return nil, 0, invalid
					}
					switch part.Type {
					case dto.ContentTypeText:
						if part.Text == "" {
							return nil, 0, invalid
						}
					case dto.ContentTypeImageURL, dto.ContentTypeVideoUrl:
						if source := part.ToFileSource(); source == nil || source.GetRawData() == "" {
							return nil, 0, invalid
						}
					default:
						return nil, 0, invalid
					}
				default:
					return nil, 0, invalid
				}
			}
			if ExceedsMaxTokensLimit(common.GetPointer(uint(tokens))) {
				return nil, 0, invalid
			}
			return request.GetTokenCountMeta(), tokens, nil
		default:
			return nil, 0, invalid
		}
		return request.GetTokenCountMeta(), 0, nil
	case *dto.GeminiEmbeddingRequest:
		if request == nil || len(request.Content.Parts) == 0 || !validDimensions(request.OutputDimensionality) {
			return nil, 0, invalid
		}
		chat := &dto.GeminiChatRequest{Contents: []dto.GeminiChatContent{request.Content}}
		meta := chat.GetTokenCountMeta()
		if meta.CombineText == "" && len(meta.Files) == 0 {
			return nil, 0, invalid
		}
		return meta, 0, nil
	case *dto.GeminiBatchEmbeddingRequest:
		if request == nil || len(request.Requests) == 0 {
			return nil, 0, invalid
		}
		meta := &types.TokenCountMeta{}
		var text strings.Builder
		for _, item := range request.Requests {
			part, _, err := GatewayEmbeddingInput(item)
			if err != nil {
				return nil, 0, err
			}
			if text.Len() > 0 {
				text.WriteByte('\n')
			}
			text.WriteString(part.CombineText)
			meta.Files = append(meta.Files, part.Files...)
		}
		meta.CombineText = text.String()
		return meta, 0, nil
	case *dto.RerankRequest:
		if request == nil || request.Query == "" || len(request.Documents) == 0 {
			return nil, 0, invalid
		}
		for _, value := range []*int{request.TopN, request.MaxChunkPerDoc, request.OverLapTokens} {
			if value != nil && (*value < 0 || ExceedsMaxTokensLimit(common.GetPointer(uint(*value)))) {
				return nil, 0, invalid
			}
		}
		return request.GetTokenCountMeta(), 0, nil
	default:
		return nil, 0, invalid
	}
}

// GatewayEmbeddingUsage counts the effective input without inventing generated
// output tokens for vectors or ranking scores.
func GatewayEmbeddingUsage(c *gin.Context, request dto.Request, info *relaycommon.RelayInfo, caps capability.Set) (*dto.Usage, error) {
	meta, encodedTokens, err := GatewayEmbeddingInput(request)
	if err != nil {
		return nil, err
	}
	if (meta.CombineText != "" || encodedTokens > 0) && !slices.Contains(caps.InputModalities, capability.Text) {
		return nil, service.ErrGatewayCapability
	}
	images := int64(0)
	for _, file := range meta.Files {
		modality := capability.PDF
		switch file.FileType {
		case types.FileTypeImage:
			modality = capability.Image
			images++
		case types.FileTypeVideo:
			modality = capability.Video
		case types.FileTypeAudio:
			modality = capability.Audio
		}
		if !slices.Contains(caps.InputModalities, modality) {
			return nil, service.ErrGatewayCapability
		}
	}
	if caps.Limits.MaxImages != nil && images > *caps.Limits.MaxImages {
		return nil, service.ErrGatewayCapability
	}
	var dimensions []*int
	switch request := request.(type) {
	case *dto.EmbeddingRequest:
		dimensions = []*int{request.Dimensions}
	case *dto.GeminiEmbeddingRequest:
		dimensions = []*int{request.OutputDimensionality}
	case *dto.GeminiBatchEmbeddingRequest:
		for _, item := range request.Requests {
			dimensions = append(dimensions, item.OutputDimensionality)
		}
	}
	for _, dimension := range dimensions {
		if dimension != nil && caps.Limits.EmbeddingDimensions != nil && int64(*dimension) > *caps.Limits.EmbeddingDimensions {
			return nil, service.ErrGatewayCapability
		}
	}
	usage, err := service.CountRequestUsage(c, meta, info, service.GatewayRequestTokenOptions(c))
	if err != nil {
		return nil, err
	}
	usage.PromptTokens += encodedTokens
	usage.TotalTokens = usage.PromptTokens
	if caps.Limits.ContextLength != nil && int64(usage.PromptTokens) > *caps.Limits.ContextLength {
		return nil, service.ErrGatewayCapability
	}
	return usage, nil
}
