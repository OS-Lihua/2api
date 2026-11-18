package model

import (
	"fmt"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/protocol"
)

// FillModelSupportedEndpoints reports administrative native declarations, not
// request-dependent protocol conversion or current callability. Stable Model IDs
// own enabled, confirmed Channel declarations; the Model's explicit endpoints
// supplement them, never restrict them. Read the current rows instead of the
// legacy name/ability/pricing cache so channel and metadata edits need no cache
// invalidation, price lookup, provider composition, or remote calls.
func FillModelSupportedEndpoints(models []*Model) error {
	ids := make([]int, 0, len(models))
	for _, metadata := range models {
		if metadata != nil && metadata.Id > 0 {
			ids = append(ids, metadata.Id)
		}
	}
	var channels []struct {
		ModelID  int
		Protocol protocol.Type
	}
	if len(ids) > 0 {
		if err := DB.Model(&Channel{}).
			Select("DISTINCT channels.model_id, channels.protocol").
			Joins("JOIN providers ON providers.id = channels.provider_id").
			Where("channels.model_id IN ? AND channels.status = ? AND channels.mapping_confirmed = ? AND providers.status = ?", ids, ChannelEnabled, true, common.ChannelStatusEnabled).
			Find(&channels).Error; err != nil {
			return err
		}
	}
	byModel := make(map[int][]string, len(ids))
	for _, channel := range channels {
		endpoints, err := nativeProtocolEndpoints(channel.Protocol)
		if err != nil {
			return err
		}
		byModel[channel.ModelID] = append(byModel[channel.ModelID], endpoints...)
	}
	for _, metadata := range models {
		if metadata == nil {
			continue
		}
		metadata.SupportedEndpoints = append([]string{}, byModel[metadata.Id]...)
		var explicit any
		// Preserve legacy malformed metadata without inventing a declaration.
		// Valid object keys and array entries are open surface labels, not a
		// closed protocol enum; custom paths do not replace native surfaces.
		if common.UnmarshalJsonStr(metadata.Endpoints, &explicit) == nil {
			switch values := explicit.(type) {
			case map[string]any:
				for name, value := range values {
					if strings.TrimSpace(name) == "" {
						continue
					}
					switch value.(type) {
					case string, map[string]any:
						metadata.SupportedEndpoints = append(metadata.SupportedEndpoints, name)
					}
				}
			case []any:
				for _, value := range values {
					if name, ok := value.(string); ok && strings.TrimSpace(name) != "" {
						metadata.SupportedEndpoints = append(metadata.SupportedEndpoints, name)
					}
				}
			}
		}
		slices.Sort(metadata.SupportedEndpoints)
		metadata.SupportedEndpoints = slices.Compact(metadata.SupportedEndpoints)
	}
	return nil
}

func nativeProtocolEndpoints(kind protocol.Type) ([]string, error) {
	switch kind {
	case protocol.OpenAIChat:
		return []string{string(constant.EndpointTypeOpenAI)}, nil
	case protocol.OpenAIResponses:
		return []string{string(constant.EndpointTypeOpenAIResponse), string(constant.EndpointTypeOpenAIResponseCompact)}, nil
	case protocol.OpenAIRealtime:
		return []string{"openai-realtime"}, nil
	case protocol.AnthropicMessages:
		return []string{string(constant.EndpointTypeAnthropic)}, nil
	case protocol.GeminiGenerate:
		return []string{string(constant.EndpointTypeGemini)}, nil
	case protocol.OpenAIEmbeddings:
		return []string{string(constant.EndpointTypeEmbeddings)}, nil
	case protocol.GeminiEmbed:
		return []string{"gemini-embed"}, nil
	case protocol.OpenAIImages:
		return []string{string(constant.EndpointTypeImageGeneration)}, nil
	case protocol.OpenAIAudioSpeech:
		return []string{"openai-audio-speech"}, nil
	case protocol.OpenAIAudioTranscription:
		return []string{"openai-audio-transcription"}, nil
	case protocol.Rerank:
		return []string{string(constant.EndpointTypeJinaRerank)}, nil
	case protocol.AlphaSearch:
		return []string{string(constant.EndpointTypeOpenAIAlphaSearch)}, nil
	case protocol.DeepSeekTools:
		return []string{string(constant.EndpointTypeDeepSeekToolsSearch), string(constant.EndpointTypeDeepSeekToolsFetch)}, nil
	case protocol.OpenAIModerations:
		return []string{"openai-moderations"}, nil
	default:
		if _, ok := kind.TaskPluginKey(); ok {
			return []string{string(kind)}, nil
		}
		return nil, fmt.Errorf("unsupported native channel protocol %q", kind)
	}
}
