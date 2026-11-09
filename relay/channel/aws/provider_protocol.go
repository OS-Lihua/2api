package aws

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

var _ channel.ProviderProtocolComposer = (*Provider)(nil)
var _ channel.ProviderRequestFixer = (*Provider)(nil)

func (p *Provider) ComposeProtocol(kind protocol.Type, generic channel.ProtocolHandler) (channel.ProtocolHandler, error) {
	switch kind {
	case protocol.AnthropicMessages:
		return &channel.RequestRepairProtocol{Kind: kind, Public: generic, Provider: p}, nil
	case protocol.OpenAIChat:
		return &novaProtocol{public: generic}, nil
	default:
		return nil, fmt.Errorf("AWS does not support protocol %q", kind)
	}
}
func (p *Provider) FixRequest(c *gin.Context, ctx *channel.ProtocolRequestContext, request any) (any, error) {
	req, ok := request.(*dto.ClaudeRequest)
	if !ok || req == nil {
		return nil, fmt.Errorf("expected Anthropic Messages DTO, got %T", request)
	}
	if isNovaModel(ctx.UpstreamModelName) {
		return nil, fmt.Errorf("Nova requires explicitly selected OpenAI Chat protocol")
	}
	repaired, err := p.repairClaudeMedia(c, req)
	if err != nil {
		return nil, err
	}
	return &AwsClaudeRequest{Request: repaired.(*dto.ClaudeRequest), AnthropicVersion: "bedrock-2023-05-31"}, nil
}

// novaProtocol is the Bedrock Nova wire dialect of the public Chat protocol.
// Its token counts come only from the Nova usage object.
type novaProtocol struct{ public channel.ProtocolHandler }

func (p *novaProtocol) ConvertRequest(c *gin.Context, info *relaycommon.RelayInfo, request any) (any, error) {
	req, err := p.public.ConvertRequest(c, info, request)
	if err != nil {
		return nil, err
	}
	chat, ok := req.(*dto.GeneralOpenAIRequest)
	if !ok || chat == nil {
		return nil, fmt.Errorf("expected OpenAI Chat DTO, got %T", req)
	}
	if !isNovaModel(info.UpstreamModelName) {
		return nil, fmt.Errorf("AWS model requires explicitly selected Anthropic Messages protocol")
	}
	if info.ChannelOtherSettings.AwsApiType == dto.AwsApiTypeMantle {
		return nil, fmt.Errorf("AWS Mantle does not support Nova")
	}
	for index, message := range chat.Messages {
		switch message.Role {
		case "user", "assistant", "system", "developer":
		default:
			return nil, &types.ConversionLossError{Diagnostics: []types.ConversionDiagnostic{{Code: "request_parameter_dropped", Path: fmt.Sprintf("messages[%d].role", index), Message: "The native Nova request cannot represent this message role", Severity: types.ConversionDiagnosticError}}}
		}
	}
	wire := convertToNovaRequest(chat)
	if info.ResponseFacts != nil && info.ResponseFacts.OnBusinessOutput != nil {
		encoded, err := common.Marshal(wire)
		if err != nil {
			return nil, err
		}
		actual, err := wire.ProjectChatRequest(encoded, *chat)
		if err != nil {
			return nil, err
		}
		if err := channel.DiagnoseNativeChatRequest(c, info, chat, wire.ChatRequestCapabilities(), actual); err != nil {
			return nil, err
		}
	}
	return wire, nil
}
func (p *novaProtocol) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	usage := &dto.Usage{}
	if info.ResponseFacts != nil {
		body = relaycommon.SanitizeNativeUsage(body, "usage")
		info.ResponseFacts.RecordNativeUsage(usage, body, "usage.inputTokens", "usage.outputTokens")
		var text strings.Builder
		for _, part := range gjson.GetBytes(body, "output.message.content").Array() {
			text.WriteString(relaycommon.SourceText(part.Get("text")))
		}
		info.ResponseFacts.RecordChatSource(&dto.OpenAITextResponse{Choices: []dto.OpenAITextResponseChoice{{Message: dto.Message{Role: "assistant", Content: text.String()}}}}, info.TokenizerModelName())
	}
	response, reportedUsage, err := novaChatResponse(body, info.UpstreamModelName, helper.GetResponseID(c))
	if captured, ok := resp.Body.(channel.ResponseErrorCarrier); ok && captured.ResponseError() != nil {
		return usage, captured.ResponseError()
	}
	if readErr != nil {
		return usage, types.NewError(readErr, types.ErrorCodeReadResponseBodyFailed)
	}
	if err != nil {
		return usage, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	var reported any
	if reportedUsage {
		reported = &response.Usage
	}
	if info.ResponseFacts != nil {
		prompt, completion := relaycommon.NativeUsagePresence(body, "usage.inputTokens", "usage.outputTokens", "usage.totalTokens")
		fields := make(map[string]int)
		if prompt {
			fields["prompt_tokens"] = response.Usage.PromptTokens
		}
		if completion {
			fields["completion_tokens"] = response.Usage.CompletionTokens
		}
		reported = nil
		if len(fields) > 0 {
			reported = fields
		}
	}
	if info.IsStream {
		// The legacy Nova integration uses InvokeModel even for streaming clients.
		// Emit one content chunk and the reported usage through the shared parser.
		chunk := dto.ChatCompletionsStreamResponse{Id: response.Id, Object: "chat.completion.chunk", Created: common.GetTimestamp(), Model: response.Model}
		if reportedUsage {
			chunk.Usage = &response.Usage
		}
		choice := dto.ChatCompletionsStreamResponseChoice{Index: 0, FinishReason: common.GetPointer(response.Choices[0].FinishReason)}
		choice.Delta.SetContentString(response.Choices[0].Message.StringContent())
		chunk.Choices = []dto.ChatCompletionsStreamResponseChoice{choice}
		wire := struct {
			*dto.ChatCompletionsStreamResponse
			Usage any `json:"usage,omitempty"`
		}{ChatCompletionsStreamResponse: &chunk, Usage: reported}
		encoded, err := common.Marshal(wire)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		resp.Body = io.NopCloser(bytes.NewReader(append(append([]byte("data: "), encoded...), []byte("\n\ndata: [DONE]\n\n")...)))
		resp.Header.Set("Content-Type", "text/event-stream")
		return p.public.DoResponse(c, resp, info)
	}
	wire := struct {
		*dto.OpenAITextResponse
		Usage any `json:"usage,omitempty"`
	}{OpenAITextResponse: response, Usage: reported}
	encoded, err := common.Marshal(wire)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	resp.Body = io.NopCloser(bytes.NewReader(encoded))
	return p.public.DoResponse(c, resp, info)
}
func novaChatResponse(body []byte, model, id string) (*dto.OpenAITextResponse, bool, error) {
	var response struct {
		StopReason string `json:"stopReason"`
		Output     struct {
			Message struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		} `json:"output"`
		Usage *struct {
			InputTokens  int `json:"inputTokens"`
			OutputTokens int `json:"outputTokens"`
			TotalTokens  int `json:"totalTokens"`
		} `json:"usage"`
	}
	if err := common.Unmarshal(body, &response); err != nil {
		return nil, false, err
	}
	var content strings.Builder
	for _, part := range response.Output.Message.Content {
		content.WriteString(part.Text)
	}
	finishReason := "stop"
	switch response.StopReason {
	case "max_tokens":
		finishReason = "length"
	case "content_filtered", "guardrail_intervened":
		finishReason = "content_filter"
	case "tool_use":
		finishReason = "tool_calls"
	}
	chat := &dto.OpenAITextResponse{Id: id, Object: "chat.completion", Created: common.GetTimestamp(), Model: model,
		Choices: []dto.OpenAITextResponseChoice{{Index: 0, Message: dto.Message{Role: "assistant", Content: content.String()}, FinishReason: finishReason}},
	}
	if response.Usage != nil {
		chat.Usage = dto.Usage{PromptTokens: response.Usage.InputTokens, CompletionTokens: response.Usage.OutputTokens, TotalTokens: response.Usage.TotalTokens}
	}
	return chat, response.Usage != nil, nil
}
