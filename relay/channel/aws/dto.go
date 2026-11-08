package aws

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/tidwall/sjson"
)

type AwsClaudeRequest struct {
	// AnthropicVersion should be "bedrock-2023-05-31"
	AnthropicVersion  string              `json:"anthropic_version"`
	AnthropicBeta     json.RawMessage     `json:"anthropic_beta,omitempty"`
	System            any                 `json:"system,omitempty"`
	Messages          []dto.ClaudeMessage `json:"messages"`
	MaxTokens         *uint               `json:"max_tokens,omitempty"`
	Temperature       *float64            `json:"temperature,omitempty"`
	TopP              *float64            `json:"top_p,omitempty"`
	TopK              *int                `json:"top_k,omitempty"`
	StopSequences     []string            `json:"stop_sequences,omitempty"`
	Tools             any                 `json:"tools,omitempty"`
	ToolChoice        any                 `json:"tool_choice,omitempty"`
	Safeguards        json.RawMessage     `json:"safeguards,omitempty"`
	ContextManagement json.RawMessage     `json:"context_management,omitempty"`
	Thinking          *dto.Thinking       `json:"thinking,omitempty"`
	OutputConfig      json.RawMessage     `json:"output_config,omitempty"`
	//Metadata         json.RawMessage     `json:"metadata,omitempty"`
}

func formatRequest(requestBody io.Reader, requestHeader http.Header) (*AwsClaudeRequest, error) {
	var awsClaudeRequest AwsClaudeRequest
	err := common.DecodeJson(requestBody, &awsClaudeRequest)
	if err != nil {
		return nil, err
	}
	awsClaudeRequest.AnthropicVersion = "bedrock-2023-05-31"

	// check header anthropic-beta
	var betas []string
	for beta := range strings.SplitSeq(requestHeader.Get("anthropic-beta"), ",") {
		if beta = strings.TrimSpace(beta); beta != "" {
			betas = append(betas, beta)
		}
	}
	if len(betas) > 0 {
		betaJson, err := common.Marshal(betas)
		if err != nil {
			return nil, err
		}
		awsClaudeRequest.AnthropicBeta = betaJson
	}
	logger.LogJson(context.Background(), "json", awsClaudeRequest)
	return &awsClaudeRequest, nil
}

// NovaMessage Nova模型使用messages-v1格式
type NovaMessage struct {
	Role    string        `json:"role"`
	Content []NovaContent `json:"content"`
}

type NovaContent struct {
	Text string `json:"text"`
}

func (*NovaRequest) ChatRequestCapabilities() channel.NativeChatCapabilities {
	return channel.NativeChatCapabilities{Parameters: []string{"max_tokens", "max_completion_tokens", "temperature", "top_p", "top_k", "stop"}, ContentTypes: []string{dto.ContentTypeText}}
}

func (*NovaRequest) ProjectChatRequest(body []byte, public dto.GeneralOpenAIRequest) (*dto.GeneralOpenAIRequest, error) {
	var wire NovaRequest
	if err := common.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	public.Messages = nil
	for _, message := range wire.Messages {
		var text strings.Builder
		for _, part := range message.Content {
			text.WriteString(part.Text)
		}
		public.Messages = append(public.Messages, dto.Message{Role: message.Role, Content: text.String()})
	}
	public.MaxTokens, public.MaxCompletionTokens = nil, nil
	public.Temperature, public.TopP, public.TopK, public.Stop = nil, nil, nil, nil
	if config := wire.InferenceConfig; config != nil {
		public.MaxCompletionTokens = config.MaxTokens
		public.Temperature, public.TopP, public.TopK, public.Stop = config.Temperature, config.TopP, config.TopK, config.StopSequences
	}
	return &public, nil
}

func (*NovaRequest) SetChatOutputLimit(body []byte, limit uint) ([]byte, error) {
	return sjson.SetBytes(body, "inferenceConfig.maxTokens", limit)
}

func (*AwsClaudeRequest) SetChatOutputLimit(body []byte, limit uint) ([]byte, error) {
	return sjson.SetBytes(body, "max_tokens", limit)
}

type NovaRequest struct {
	SchemaVersion   string               `json:"schemaVersion"`             // 请求版本，例如 "1.0"
	Messages        []NovaMessage        `json:"messages"`                  // 对话消息列表
	InferenceConfig *NovaInferenceConfig `json:"inferenceConfig,omitempty"` // 推理配置，可选
}

type NovaInferenceConfig struct {
	MaxTokens     *uint    `json:"maxTokens,omitempty"`     // 最大生成的 token 数
	Temperature   *float64 `json:"temperature,omitempty"`   // 随机性 (默认 0.7, 范围 0-1)
	TopP          *float64 `json:"topP,omitempty"`          // nucleus sampling (默认 0.9, 范围 0-1)
	TopK          *int     `json:"topK,omitempty"`          // 限制候选 token 数 (默认 50, 范围 0-128)
	StopSequences []string `json:"stopSequences,omitempty"` // 停止生成的序列
}

// 转换OpenAI请求为Nova格式
func convertToNovaRequest(req *dto.GeneralOpenAIRequest) *NovaRequest {
	novaMessages := make([]NovaMessage, len(req.Messages))
	for i, msg := range req.Messages {
		novaMessages[i] = NovaMessage{
			Role:    msg.Role,
			Content: []NovaContent{{Text: msg.StringContent()}},
		}
	}

	novaReq := &NovaRequest{
		SchemaVersion: "messages-v1",
		Messages:      novaMessages,
	}

	maxTokens := req.MaxTokens
	if req.MaxCompletionTokens != nil {
		maxTokens = req.MaxCompletionTokens
	}
	if maxTokens != nil || req.Temperature != nil || req.TopP != nil || req.TopK != nil || req.Stop != nil {
		novaReq.InferenceConfig = &NovaInferenceConfig{
			MaxTokens: maxTokens, Temperature: req.Temperature, TopP: req.TopP, TopK: req.TopK,
			StopSequences: parseStopSequences(req.Stop),
		}
	}

	return novaReq
}

// parseStopSequences 解析停止序列，支持字符串或字符串数组
func parseStopSequences(stop any) []string {
	if stop == nil {
		return nil
	}

	switch v := stop.(type) {
	case string:
		if v != "" {
			return []string{v}
		}
	case []string:
		return v
	case []any:
		var sequences []string
		for _, item := range v {
			if str, ok := item.(string); ok && str != "" {
				sequences = append(sequences, str)
			}
		}
		return sequences
	}
	return nil
}
