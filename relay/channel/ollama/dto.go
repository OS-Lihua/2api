package ollama

import (
	"encoding/json"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

type OllamaChatMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	Images     []string         `json:"images,omitempty"`
	ToolCalls  []OllamaToolCall `json:"tool_calls,omitempty"`
	ToolName   string           `json:"tool_name,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	Thinking   json.RawMessage  `json:"thinking,omitempty"`
}

type OllamaToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

type OllamaTool struct {
	Type     string             `json:"type"`
	Function OllamaToolFunction `json:"function"`
}

type OllamaToolCall struct {
	ID       string `json:"id,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments any    `json:"arguments"`
	} `json:"function"`
}

type OllamaChatRequest struct {
	Model     string              `json:"model"`
	Messages  []OllamaChatMessage `json:"messages"`
	Tools     any                 `json:"tools,omitempty"`
	Format    any                 `json:"format,omitempty"`
	Stream    bool                `json:"stream"`
	Options   map[string]any      `json:"options,omitempty"`
	KeepAlive any                 `json:"keep_alive,omitempty"`
	Think     json.RawMessage     `json:"think,omitempty"`
}

func (*OllamaChatRequest) ProjectChatRequest(body []byte, public dto.GeneralOpenAIRequest) (*dto.GeneralOpenAIRequest, error) {
	var wire OllamaChatRequest
	if err := common.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	effective, err := channel.ProjectChatRequestControls(body, public)
	if err != nil {
		return nil, err
	}
	effective.Model, effective.Stream = wire.Model, common.GetPointer(wire.Stream)
	if raw, ok := wire.Options["num_predict"]; ok {
		encoded, err := common.Marshal(raw)
		if err != nil {
			return nil, err
		}
		var limit uint
		if err := common.Unmarshal(encoded, &limit); err != nil || limit == 0 {
			return nil, fmt.Errorf("invalid Ollama output limit")
		}
		effective.MaxCompletionTokens = &limit
	}
	effective.Messages = make([]dto.Message, 0, len(wire.Messages))
	for _, message := range wire.Messages {
		content := []dto.MediaContent{{Type: dto.ContentTypeText, Text: message.Content}}
		for _, image := range message.Images {
			content = append(content, dto.MediaContent{Type: dto.ContentTypeImageURL, ImageUrl: &dto.MessageImageUrl{Url: "data:image/jpeg;base64," + image}})
		}
		projected := dto.Message{Role: message.Role, Content: content}
		if len(message.ToolCalls) > 0 {
			calls, _ := ollamaToolCallsToOpenAI(message.ToolCalls, 0, false)
			projected.ToolCalls, err = common.Marshal(calls)
			if err != nil {
				return nil, err
			}
		}
		effective.Messages = append(effective.Messages, projected)
	}
	if wire.Tools != nil {
		encoded, err := common.Marshal(wire.Tools)
		if err != nil {
			return nil, err
		}
		if err := common.Unmarshal(encoded, &effective.Tools); err != nil {
			return nil, err
		}
	}
	if len(wire.Think) > 0 {
		var enabled bool
		if common.Unmarshal(wire.Think, &enabled) == nil {
			effective.ReasoningEffort = "none"
			if enabled {
				effective.ReasoningEffort = "high"
			}
		} else if err := common.Unmarshal(wire.Think, &effective.ReasoningEffort); err != nil {
			return nil, fmt.Errorf("invalid Ollama reasoning setting")
		}
	}
	if wire.Format != nil && wire.Format != "" {
		effective.ResponseFormat = &dto.ResponseFormat{Type: "json_schema"}
	}
	return effective, nil
}

type OllamaGenerateRequest struct {
	Model     string          `json:"model"`
	Prompt    string          `json:"prompt,omitempty"`
	Suffix    string          `json:"suffix,omitempty"`
	Images    []string        `json:"images,omitempty"`
	Format    any             `json:"format,omitempty"`
	Stream    bool            `json:"stream"`
	Options   map[string]any  `json:"options,omitempty"`
	KeepAlive any             `json:"keep_alive,omitempty"`
	Think     json.RawMessage `json:"think,omitempty"`
}

type OllamaEmbeddingRequest struct {
	Model      string         `json:"model"`
	Input      any            `json:"input"`
	Options    map[string]any `json:"options,omitempty"`
	Dimensions int            `json:"dimensions,omitempty"`
}

type OllamaEmbeddingResponse struct {
	Error           string      `json:"error,omitempty"`
	Model           string      `json:"model"`
	Embeddings      [][]float64 `json:"embeddings"`
	PromptEvalCount int         `json:"prompt_eval_count,omitempty"`
}

type OllamaTagsResponse struct {
	Models []OllamaModel `json:"models"`
}

type OllamaModel struct {
	Name       string            `json:"name"`
	Size       int64             `json:"size"`
	Digest     string            `json:"digest,omitempty"`
	ModifiedAt string            `json:"modified_at"`
	Details    OllamaModelDetail `json:"details"`
}

type OllamaModelDetail struct {
	ParentModel       string   `json:"parent_model,omitempty"`
	Format            string   `json:"format,omitempty"`
	Family            string   `json:"family,omitempty"`
	Families          []string `json:"families,omitempty"`
	ParameterSize     string   `json:"parameter_size,omitempty"`
	QuantizationLevel string   `json:"quantization_level,omitempty"`
}

type OllamaPullRequest struct {
	Name   string `json:"name"`
	Stream bool   `json:"stream,omitempty"`
}

type OllamaPullResponse struct {
	Status    string `json:"status"`
	Digest    string `json:"digest,omitempty"`
	Total     int64  `json:"total,omitempty"`
	Completed int64  `json:"completed,omitempty"`
}

type OllamaDeleteRequest struct {
	Name string `json:"name"`
}
