package textprotocol

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/protocol"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
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
	if kind == "" {
		kind = failure.Get("status").String()
	}
	if code == "" {
		code = kind
	}
	status := 0
	if value := failure.Get("code"); value.Type == gjson.Number && value.Int() >= 400 && value.Int() <= 599 {
		status = int(value.Int())
	}
	if status == http.StatusNotFound && kind == "NOT_FOUND" && info.UpstreamModelName != "" {
		message := strings.ToLower(failure.Get("message").String())
		if strings.Contains(message, "models/"+strings.ToLower(info.UpstreamModelName)) && (strings.Contains(message, "not found") || strings.Contains(message, "not supported")) {
			code = "model_not_found"
		}
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
	case types.RelayFormatOpenAIResponses:
		if response := root.Get("response"); response.IsObject() {
			root = response
		}
		rejected = root.Get("incomplete_details.reason").String() == "content_filter"
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
	body, readErr := io.ReadAll(resp.Body)
	facts := info.ResponseFacts
	var sourceOutput gatewayGeneratedOutput
	root := gjson.ParseBytes(body)
	switch p.format {
	case types.RelayFormatOpenAIResponses:
		for index, item := range root.Get("output").Array() {
			sourceOutput.ObserveItem(int64(index), item)
		}
	case types.RelayFormatClaude:
		for _, block := range root.Get("content").Array() {
			sourceOutput.text.WriteString(gatewayMessagesBlockText(block))
		}
	case types.RelayFormatGemini:
		for _, candidate := range root.Get("candidates").Array() {
			for _, part := range candidate.Get("content.parts").Array() {
				sourceOutput.text.WriteString(gatewayGeminiPartText(part))
			}
		}
	case types.RelayFormatOpenAI:
		for _, choice := range root.Get("choices").Array() {
			message := choice.Get("message")
			content := message.Get("content")
			if content.Type == gjson.String {
				sourceOutput.text.WriteString(content.String())
			} else {
				for _, part := range content.Array() {
					sourceOutput.text.WriteString(part.Get("text").String())
				}
			}
			reasoning := message.Get("reasoning_content")
			if !reasoning.Exists() {
				reasoning = message.Get("reasoning")
			}
			sourceOutput.text.WriteString(reasoning.String())
			sourceOutput.text.WriteString(message.Get("refusal").String())
			sourceOutput.text.WriteString(message.Get("audio.transcript").String())
			for _, tool := range message.Get("tool_calls").Array() {
				for _, path := range []string{"function.name", "function.arguments", "custom.name", "custom.input"} {
					sourceOutput.text.WriteString(tool.Get(path).String())
				}
			}
		}
	}
	facts.SourceGeneratedText = sourceOutput.String()
	facts.SourceOutputObserved = facts.SourceGeneratedText != "" || gatewayMediaOutput(body, p.format)
	observeGatewaySafety(info, body, p.format)
	meter := gatewayUsageAccumulator{format: p.format, context: c}
	usage, err := meter.Observe(body)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	if usage == nil {
		usage = &dto.Usage{}
	}
	prompt, completion := gatewayUsageCounters(body, p.format)
	facts.PromptTokensReported, facts.CompletionTokensReported = gatewayValidCounter(prompt), gatewayValidCounter(completion)
	facts.UsageReported = facts.PromptTokensReported && facts.CompletionTokensReported
	facts.IndependentUsageReported = meter.ReportedCounters()
	if readErr != nil {
		return usage, types.NewError(readErr, types.ErrorCodeReadResponseBodyFailed)
	}
	normalized, err := meter.NormalizedBody(body)
	if err != nil {
		return usage, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	protocolError := observeGatewayError(info, body)
	if resp.StatusCode != http.StatusOK || protocolError {
		return usage, types.NewError(fmt.Errorf("upstream response failed"), types.ErrorCodeBadResponse)
	}
	if info.IsStream {
		return usage, types.NewError(fmt.Errorf("upstream returned JSON for a stream request"), types.ErrorCodeBadResponse)
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
	budgetEnd := p.format == types.RelayFormatOpenAIResponses && gjson.GetBytes(body, "status").String() == "incomplete" && gjson.GetBytes(body, "incomplete_details.reason").String() == "max_output_tokens"
	if budgetEnd {
		info.StreamStatus.MarkIncomplete("max_output_tokens")
	}
	business := facts.GeneratedText != "" || gatewayMediaOutput(encoded, info.RelayFormat)
	if !business && !budgetEnd {
		if !info.PerformanceBusinessRejection {
			info.StreamStatus.MarkFailed("empty_response", "", 0)
		}
		return usage, types.NewError(fmt.Errorf("upstream response contained no business output"), types.ErrorCodeBadResponse)
	}
	for key, values := range resp.Header {
		if len(values) > 0 && service.ShouldCopyUpstreamHeader(c, key, values) {
			c.Writer.Header().Set(key, values[0])
		}
	}
	c.Writer.Header().Set("Content-Length", fmt.Sprint(len(encoded)))
	c.Writer.WriteHeader(resp.StatusCode)
	written, writeErr := c.Writer.Write(encoded)
	if writeErr == nil && written != len(encoded) {
		writeErr = io.ErrShortWrite
	}
	if writeErr != nil {
		facts.DownstreamWriteFailed = true
		return usage, types.NewError(writeErr, types.ErrorCodeBadResponse)
	}
	if business {
		facts.MarkBusinessOutput()
	}
	if err := helper.FlushWriter(c); err != nil {
		facts.DownstreamWriteFailed = true
		return usage, types.NewError(err, types.ErrorCodeBadResponse)
	}
	if p.format == types.RelayFormatOpenAIResponses {
		status := gjson.GetBytes(body, "status").String()
		if status != "" && status != "completed" && !budgetEnd {
			return usage, types.NewError(fmt.Errorf("upstream response did not complete"), types.ErrorCodeBadResponse)
		}
	}
	return usage, nil
}
