package aws

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/sjson"
)

// AwsClaudeRequest preserves the complete Messages DTO. Finalization selects
// Mantle JSON or the InvokeModel envelope using the actual Provider metadata.
type AwsClaudeRequest struct {
	Request          *dto.ClaudeRequest
	AnthropicVersion string
	AnthropicBeta    json.RawMessage
	mantle           bool
}

var _ channel.ChatRequestFinalizer = (*AwsClaudeRequest)(nil)
var _ channel.NativeRequestProjector = (*AwsClaudeRequest)(nil)

func (wire *AwsClaudeRequest) FinalizeChatRequest(c *gin.Context, info *relaycommon.RelayInfo, body []byte) ([]byte, error) {
	metadata := channel.NewProviderMetadata(info)
	headers := make(http.Header)
	if err := (&Provider{}).SetupHeader(c, &headers, metadata); err != nil {
		return nil, err
	}
	overrides, err := channel.ResolveHeaderOverride(info, c)
	if err != nil {
		return nil, err
	}
	for key, value := range overrides {
		headers.Set(key, value)
	}
	metadata.RequestHeaders = headers
	wire.mantle = metadata.ChannelOtherSettings.AwsApiType == dto.AwsApiTypeMantle
	if wire.mantle {
		request, err := decodeAwsClaudeRequest(body)
		if err != nil {
			return nil, err
		}
		if request.Model != "" && request.Model != metadata.UpstreamModelName && request.Model != mantleModelName(metadata.UpstreamModelName) {
			return nil, errors.New("AWS Mantle model does not match the selected model")
		}
		if request.Stream != nil && *request.Stream != metadata.IsStream {
			return nil, errors.New("AWS Mantle streaming does not match the selected request")
		}
	}
	return finalizeAwsClaudeBody(body, metadata, wire.Request.Stream)
}

func (request *AwsClaudeRequest) MarshalJSON() ([]byte, error) {
	body, err := common.Marshal(request.Request)
	if err != nil {
		return nil, err
	}
	var fields map[string]common.RawMessage
	if err := common.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	delete(fields, "model")
	delete(fields, "stream")
	fields["anthropic_version"], err = common.Marshal(request.AnthropicVersion)
	if err != nil {
		return nil, err
	}
	if len(request.AnthropicBeta) > 0 {
		fields["anthropic_beta"] = request.AnthropicBeta
	}
	return common.Marshal(fields)
}

func (wire *AwsClaudeRequest) ProjectNativeRequest(body []byte) (dto.Request, error) {
	var envelope map[string]common.RawMessage
	if err := common.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	request, err := decodeAwsClaudeRequest(body)
	if err != nil {
		return nil, err
	}
	if wire.mantle {
		if wire.Request == nil || request.Model != mantleModelName(wire.Request.Model) {
			return nil, errors.New("AWS Mantle model does not match the selected model")
		}
		stream := wire.Request.Stream != nil && *wire.Request.Stream
		if (request.Stream != nil && *request.Stream) != stream {
			return nil, errors.New("AWS Mantle streaming does not match the selected request")
		}
	} else {
		if _, present := envelope["model"]; present {
			return nil, errors.New("Bedrock model identity belongs to the SDK request")
		}
		if _, present := envelope["stream"]; present {
			return nil, errors.New("Bedrock streaming belongs to the SDK request")
		}
	}
	if wire.Request != nil {
		request.Model = wire.Request.Model
	}
	return request, nil
}

// The legacy transport retains its established field set and serialization.
type legacyAwsClaudeRequest struct {
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
}

func formatRequest(requestBody io.Reader, requestHeader http.Header) (*legacyAwsClaudeRequest, error) {
	var request legacyAwsClaudeRequest
	if err := common.DecodeJson(requestBody, &request); err != nil {
		return nil, err
	}
	request.AnthropicVersion = "bedrock-2023-05-31"
	var betas []string
	for beta := range strings.SplitSeq(requestHeader.Get("anthropic-beta"), ",") {
		if beta = strings.TrimSpace(beta); beta != "" {
			betas = append(betas, beta)
		}
	}
	if len(betas) > 0 {
		var err error
		request.AnthropicBeta, err = common.Marshal(betas)
		if err != nil {
			return nil, err
		}
	}
	return &request, nil
}

func decodeAwsClaudeRequest(body []byte) (*dto.ClaudeRequest, error) {
	var request dto.ClaudeRequest
	if err := common.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	if helper.ExceedsMaxTokensLimit(request.MaxTokens, request.MaxTokensToSample) {
		return nil, errors.New("AWS Claude max token limit is invalid")
	}
	if request.MaxTokensToSample != nil {
		return nil, errors.New("AWS Messages does not support max_tokens_to_sample")
	}
	return &request, nil
}

// finalizeAwsClaudeBody preserves native fields while resolving the exact wire
// envelope. It is also used for synthetic probes that carry no Gin metadata.
func finalizeAwsClaudeBody(body []byte, metadata *channel.ProviderRequest, stream *bool) ([]byte, error) {
	request, err := decodeAwsClaudeRequest(body)
	if err != nil {
		return nil, err
	}
	var fields map[string]common.RawMessage
	if err := common.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, errors.New("AWS Claude request must be a JSON object")
	}
	if metadata.ChannelOtherSettings.AwsApiType == dto.AwsApiTypeMantle {
		model := mantleModelName(metadata.UpstreamModelName)
		delete(fields, "anthropic_version")
		delete(fields, "anthropic_beta")
		fields["model"], err = common.Marshal(model)
		if err != nil {
			return nil, err
		}
		if request.Stream == nil && (metadata.IsStream || stream != nil) {
			fields["stream"], err = common.Marshal(metadata.IsStream)
			if err != nil {
				return nil, err
			}
		}
		return common.Marshal(fields)
	}
	if _, present := fields["model"]; present {
		return nil, errors.New("Bedrock model identity belongs to the SDK request")
	}
	if _, present := fields["stream"]; present {
		return nil, errors.New("Bedrock streaming belongs to the SDK request")
	}
	fields["anthropic_version"], err = common.Marshal("bedrock-2023-05-31")
	if err != nil {
		return nil, err
	}
	delete(fields, "anthropic_beta")
	var betas []string
	for beta := range strings.SplitSeq(metadata.RequestHeaders.Get("anthropic-beta"), ",") {
		if beta = strings.TrimSpace(beta); beta != "" {
			betas = append(betas, beta)
		}
	}
	if len(betas) > 0 {
		fields["anthropic_beta"], err = common.Marshal(betas)
		if err != nil {
			return nil, err
		}
	}
	return common.Marshal(fields)
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
	for _, part := range wire.System {
		public.Messages = append(public.Messages, dto.Message{Role: "system", Content: part.Text})
	}
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

type NovaRequest struct {
	SchemaVersion   string               `json:"schemaVersion"` // 请求版本，例如 "1.0"
	Messages        []NovaMessage        `json:"messages"`      // 对话消息列表
	System          []NovaContent        `json:"system,omitempty"`
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
	novaMessages := make([]NovaMessage, 0, len(req.Messages))
	var system []NovaContent
	for _, msg := range req.Messages {
		if msg.Role == "system" || msg.Role == "developer" {
			system = append(system, NovaContent{Text: msg.StringContent()})
			continue
		}
		novaMessages = append(novaMessages, NovaMessage{Role: msg.Role, Content: []NovaContent{{Text: msg.StringContent()}}})
	}

	novaReq := &NovaRequest{
		SchemaVersion: "messages-v1",
		Messages:      novaMessages,
		System:        system,
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
