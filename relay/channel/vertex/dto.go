package vertex

import (
	"encoding/json"
	"fmt"

	"github.com/QuantumNous/new-api/common"

	"github.com/QuantumNous/new-api/relaykit/dto"
)

type VertexAIClaudeRequest struct {
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

type vertexClaudeRequest struct {
	Request *dto.ClaudeRequest
}

func (r *vertexClaudeRequest) MarshalJSON() ([]byte, error) {
	body, err := common.Marshal(r.Request)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := common.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	delete(fields, "model")
	fields["anthropic_version"], err = common.Marshal(anthropicVersion)
	if err != nil {
		return nil, err
	}
	return common.Marshal(fields)
}

func (r *vertexClaudeRequest) ProjectNativeRequest(body []byte) (dto.Request, error) {
	var fields map[string]json.RawMessage
	if err := common.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	if _, ok := fields["model"]; ok {
		return nil, fmt.Errorf("Vertex model is controlled by the request URL")
	}
	var request dto.ClaudeRequest
	if err := common.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	if (request.Stream != nil && *request.Stream) != (r.Request.Stream != nil && *r.Request.Stream) {
		return nil, fmt.Errorf("Vertex stream must match the request transport")
	}
	request.Model = r.Request.Model
	return &request, nil
}

func copyRequest(req *dto.ClaudeRequest, version string) *VertexAIClaudeRequest {
	return &VertexAIClaudeRequest{
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
