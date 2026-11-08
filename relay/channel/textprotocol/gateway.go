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

// observeGatewayError preserves structured protocol facts for the shared
// failure classifier. Raw provider messages never leave the parser.
func observeGatewayError(info *relaycommon.RelayInfo, body []byte) bool {
	root := gjson.ParseBytes(body)
	failure := root.Get("error")
	if (!failure.Exists() || failure.Type == gjson.Null) && root.Get("type").String() != "error" {
		return false
	}
	if !failure.IsObject() {
		failure = root
	}
	code, kind := failure.Get("code").String(), failure.Get("type").String()
	if code == "" {
		code = kind
	}
	status := 0
	if value := failure.Get("code"); value.Type == gjson.Number && value.Int() >= 400 && value.Int() <= 599 {
		status = int(value.Int())
	}
	info.StreamStatus.MarkFailed(code, kind, status)
	return true
}

// observeGatewaySafety records protocol rejection semantics independently of
// any content included in the same response. Delivery still controls charging.
func observeGatewaySafety(info *relaycommon.RelayInfo, body []byte, format types.RelayFormat) bool {
	root := gjson.ParseBytes(body)
	rejected := false
	switch format {
	case types.RelayFormatGemini:
		rejected = root.Get("promptFeedback.blockReason").String() != ""
		for _, candidate := range root.Get("candidates").Array() {
			switch candidate.Get("finishReason").String() {
			case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT":
				rejected = true
			}
		}
	case types.RelayFormatOpenAI:
		for _, choice := range root.Get("choices").Array() {
			rejected = rejected || choice.Get("finish_reason").String() == "content_filter"
		}
	case types.RelayFormatClaude:
		rejected = root.Get("stop_reason").String() == "refusal" || root.Get("delta.stop_reason").String() == "refusal"
	}
	info.PerformanceBusinessRejection = info.PerformanceBusinessRejection || rejected
	return rejected
}

// gatewayJSONResponse uses the shared conversion registry and preserves the
// delivery/presence contract of the money gateway across all four protocols.
func (p *Handler) gatewayJSONResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeReadResponseBodyFailed)
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
	facts.PromptTokensReported, facts.CompletionTokensReported = gatewayValidCounter(prompt), gatewayValidCounter(completion)
	facts.UsageReported = facts.PromptTokensReported && facts.CompletionTokensReported
	facts.IndependentUsageReported = meter.ReportedCounters()
	normalized, err := meter.NormalizedBody(body)
	if err != nil {
		return usage, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	protocolError := observeGatewayError(info, body)
	if resp.StatusCode != http.StatusOK || protocolError {
		return usage, types.NewError(fmt.Errorf("upstream response failed"), types.ErrorCodeBadResponse)
	}
	observeGatewaySafety(info, body, p.format)
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
	if err := common.Unmarshal(normalized, response); err != nil {
		return usage, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	canonical, err := relayconvert.ConvertResponse(c, info, types.RelayFormatOpenAI, response)
	if err != nil {
		return usage, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	var generated strings.Builder
	nativeMessages := p.format == types.RelayFormatClaude && info.RelayFormat == types.RelayFormatClaude
	nativeGemini := p.format == types.RelayFormatGemini && info.RelayFormat == types.RelayFormatGemini
	nativeResponses := p.format == types.RelayFormatOpenAIResponses && info.RelayFormat == types.RelayFormatOpenAIResponses
	if chat, ok := canonical.Value.(*dto.OpenAITextResponse); ok && !nativeMessages && !nativeGemini && !nativeResponses {
		for _, choice := range chat.Choices {
			generated.WriteString(choice.Message.StringContent())
			generated.WriteString(choice.Message.GetReasoningContent())
			if p.format != types.RelayFormatOpenAIResponses || info.RelayFormat != types.RelayFormatOpenAIResponses {
				for _, tool := range choice.Message.ParseToolCalls() {
					generated.WriteString(tool.Function.Name)
					generated.WriteString(tool.Function.Arguments)
					generated.WriteString(gjson.GetBytes(tool.Custom, "name").String())
					generated.WriteString(gjson.GetBytes(tool.Custom, "input").String())
				}
			}
		}
	}
	if nativeResponses {
		var output gatewayGeneratedOutput
		for index, item := range gjson.GetBytes(body, "output").Array() {
			output.ObserveItem(int64(index), item)
		}
		generated.WriteString(output.String())
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
	if p.format == types.RelayFormatOpenAI && info.RelayFormat == types.RelayFormatOpenAI {
		for _, choice := range gjson.GetBytes(body, "choices").Array() {
			generated.WriteString(choice.Get("message.refusal").String())
			generated.WriteString(choice.Get("message.audio.transcript").String())
		}
	}
	facts.GeneratedText = generated.String()
	if !facts.PromptTokensReported {
		usage.PromptTokens = max(usage.PromptTokens, info.GetEstimatePromptTokens())
	}
	if !facts.CompletionTokensReported {
		usage.CompletionTokens = max(usage.CompletionTokens, tokenkit.Count(info.TokenizerModelName(), facts.GeneratedText))
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	converted, err := relayconvert.ConvertResponse(c, info, info.RelayFormat, response)
	if err != nil {
		return usage, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	encoded, err := common.Marshal(converted.Value)
	if err != nil {
		return usage, types.NewError(err, types.ErrorCodeJsonMarshalFailed)
	}
	if p.format == info.RelayFormat {
		encoded = normalized
	}
	if err := service.IOCopyBytesGracefully(c, resp, encoded); err != nil {
		facts.DownstreamWriteFailed = true
		return usage, types.NewError(err, types.ErrorCodeBadResponse)
	}
	if facts.GeneratedText != "" || gatewayMediaOutput(encoded, info.RelayFormat) {
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
