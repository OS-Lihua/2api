package textprotocol

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/protocol"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/tokenkit"
	"github.com/gin-gonic/gin"
)

// gatewayJSONResponse uses the shared conversion registry and preserves the
// delivery/presence contract of the money gateway across all four protocols.
func (p *Handler) gatewayJSONResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeReadResponseBodyFailed)
	}
	var response any
	switch p.kind {
	case protocol.OpenAIChat:
		response = &dto.OpenAITextResponse{}
	case protocol.OpenAIResponses:
		response = &dto.OpenAIResponsesResponse{}
	case protocol.AnthropicMessages:
		response = &dto.ClaudeResponse{}
	case protocol.GeminiGenerate:
		response = &dto.GeminiChatResponse{}
	}
	if err := common.Unmarshal(body, response); err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	canonical, err := relayconvert.ConvertResponse(c, info, types.RelayFormatOpenAI, response)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	usage := canonical.Usage
	if usage == nil {
		usage = &dto.Usage{}
	}
	facts := info.ResponseFacts
	var presence struct {
		Usage struct {
			Prompt     *int64 `json:"prompt_tokens"`
			Completion *int64 `json:"completion_tokens"`
			Input      *int64 `json:"input_tokens"`
			Output     *int64 `json:"output_tokens"`
		} `json:"usage"`
		Gemini struct {
			Prompt     *int64 `json:"promptTokenCount"`
			Completion *int64 `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := common.Unmarshal(body, &presence); err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	prompt, completion := presence.Usage.Prompt, presence.Usage.Completion
	if p.kind == protocol.OpenAIResponses || p.kind == protocol.AnthropicMessages {
		prompt, completion = presence.Usage.Input, presence.Usage.Output
	} else if p.kind == protocol.GeminiGenerate {
		prompt, completion = presence.Gemini.Prompt, presence.Gemini.Completion
	}
	facts.PromptTokensReported = prompt != nil && *prompt >= 0 && *prompt <= 1<<31-1
	facts.CompletionTokensReported = completion != nil && *completion >= 0 && *completion <= 1<<31-1
	facts.UsageReported = facts.PromptTokensReported && facts.CompletionTokensReported
	var generated strings.Builder
	if chat, ok := canonical.Value.(*dto.OpenAITextResponse); ok {
		for _, choice := range chat.Choices {
			generated.WriteString(choice.Message.StringContent())
			if choice.Message.ReasoningContent != nil {
				generated.WriteString(*choice.Message.ReasoningContent)
			}
			for _, tool := range choice.Message.ParseToolCalls() {
				generated.WriteString(tool.Function.Name)
				generated.WriteString(tool.Function.Arguments)
			}
		}
	}
	facts.GeneratedText = generated.String()
	if !facts.PromptTokensReported {
		usage.PromptTokens = info.GetEstimatePromptTokens()
	}
	if !facts.CompletionTokensReported {
		usage.CompletionTokens = tokenkit.Count(info.TokenizerModelName(), facts.GeneratedText)
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	if resp.StatusCode != http.StatusOK {
		return usage, types.NewError(fmt.Errorf("upstream response failed"), types.ErrorCodeBadResponse)
	}
	converted, err := relayconvert.ConvertResponse(c, info, info.RelayFormat, response)
	if err != nil {
		return usage, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	encoded, err := common.Marshal(converted.Value)
	if err != nil {
		return usage, types.NewError(err, types.ErrorCodeJsonMarshalFailed)
	}
	if err := service.IOCopyBytesGracefully(c, resp, encoded); err != nil {
		facts.DownstreamWriteFailed = true
		return usage, types.NewError(err, types.ErrorCodeBadResponse)
	}
	facts.BusinessOutput = facts.GeneratedText != ""
	return usage, nil
}
