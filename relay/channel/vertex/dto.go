package vertex

import (
	"encoding/json"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

type VertexAIClaudeRequest struct {
	model            string
	AnthropicVersion string              `json:"anthropic_version"`
	Messages         []dto.ClaudeMessage `json:"messages"`
	System           any                 `json:"system,omitempty"`
	MaxTokens        *uint               `json:"max_tokens,omitempty"`
	StopSequences    []string            `json:"stop_sequences,omitempty"`
	Stream           *bool               `json:"stream,omitempty"`
	Temperature      *float64            `json:"temperature,omitempty"`
	TopP             *float64            `json:"top_p,omitempty"`
	TopK             *int                `json:"top_k,omitempty"`
	Tools            any                 `json:"tools,omitempty"`
	ToolChoice       any                 `json:"tool_choice,omitempty"`
	Thinking         *dto.Thinking       `json:"thinking,omitempty"`
	OutputConfig     json.RawMessage     `json:"output_config,omitempty"`
	//Metadata         json.RawMessage     `json:"metadata,omitempty"`
}

func (*VertexAIClaudeRequest) ChatOutputLimit(body []byte) (*uint, error) {
	var request VertexAIClaudeRequest
	if err := common.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	return request.MaxTokens, nil
}

func (request *VertexAIClaudeRequest) SetChatOutputLimit(body []byte, limit uint) ([]byte, error) {
	return channel.SetClaudeOutputLimit(body, request.model, limit)
}

func copyRequest(req *dto.ClaudeRequest, version string) *VertexAIClaudeRequest {
	return &VertexAIClaudeRequest{
		model:            req.Model,
		AnthropicVersion: version,
		System:           req.System,
		Messages:         req.Messages,
		MaxTokens:        req.MaxTokens,
		Stream:           req.Stream,
		Temperature:      req.Temperature,
		TopP:             req.TopP,
		TopK:             req.TopK,
		StopSequences:    req.StopSequences,
		Tools:            req.Tools,
		ToolChoice:       req.ToolChoice,
		Thinking:         req.Thinking,
		OutputConfig:     req.OutputConfig,
	}
}
