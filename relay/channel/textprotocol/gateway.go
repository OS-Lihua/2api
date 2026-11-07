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
	"github.com/tidwall/gjson"
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
	meter := gatewayUsageAccumulator{format: p.format}
	usage, err := meter.Observe(body)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	if usage == nil {
		usage = &dto.Usage{}
	}
	facts := info.ResponseFacts
	prompt, completion := gatewayUsageCounters(body, p.format)
	facts.PromptTokensReported = prompt.Type == gjson.Number && prompt.Int() >= 0 && prompt.Int() <= 1<<31-1
	facts.CompletionTokensReported = completion.Type == gjson.Number && completion.Int() >= 0 && completion.Int() <= 1<<31-1
	facts.UsageReported = facts.PromptTokensReported && facts.CompletionTokensReported
	var generated strings.Builder
	nativeMessages := p.format == types.RelayFormatClaude && info.RelayFormat == types.RelayFormatClaude
	nativeGemini := p.format == types.RelayFormatGemini && info.RelayFormat == types.RelayFormatGemini
	if chat, ok := canonical.Value.(*dto.OpenAITextResponse); ok && !nativeMessages && !nativeGemini {
		for _, choice := range chat.Choices {
			generated.WriteString(choice.Message.StringContent())
			if choice.Message.ReasoningContent != nil {
				generated.WriteString(*choice.Message.ReasoningContent)
			}
			if p.format != types.RelayFormatOpenAIResponses || info.RelayFormat != types.RelayFormatOpenAIResponses {
				for _, tool := range choice.Message.ParseToolCalls() {
					generated.WriteString(tool.Function.Name)
					generated.WriteString(tool.Function.Arguments)
				}
			}
		}
	}
	if p.format == types.RelayFormatOpenAIResponses && info.RelayFormat == types.RelayFormatOpenAIResponses {
		for _, item := range gjson.GetBytes(body, "output").Array() {
			if name, arguments, ok := gatewayResponseTool(item); ok {
				generated.WriteString(name)
				generated.WriteString(arguments)
			}
		}
	}
	if nativeMessages {
		for _, block := range gjson.GetBytes(body, "content").Array() {
			generated.WriteString(gatewayMessagesBlockText(block))
		}
	}
	if nativeGemini {
		for _, candidate := range gjson.GetBytes(body, "candidates").Array() {
			for _, part := range candidate.Get("content.parts").Array() {
				generated.WriteString(gatewayGeminiPartText(part))
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
	upstreamError := gjson.GetBytes(body, "error")
	if resp.StatusCode != http.StatusOK || upstreamError.Exists() && upstreamError.Type != gjson.Null {
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
	if facts.GeneratedText != "" {
		facts.MarkBusinessOutput()
	}
	if p.format == types.RelayFormatOpenAIResponses {
		status := gjson.GetBytes(body, "status").String()
		if status != "" && status != "completed" {
			return usage, types.NewError(fmt.Errorf("upstream response did not complete"), types.ErrorCodeBadResponse)
		}
	}
	return usage, nil
}
