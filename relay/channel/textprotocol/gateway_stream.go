package textprotocol

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
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

func (p *Handler) gatewayStreamResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)
	state, err := relayconvert.NewResponseStreamState(p.format, info.RelayFormat, relayconvert.ResponseStreamOptions{ID: helper.GetResponseID(c), Model: info.UpstreamModelName, IncludeUsage: true, EmitSequenceNumber: true})
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponse)
	}
	usage := &dto.Usage{}
	meter := gatewayUsageAccumulator{format: p.format}
	var streamErr error
	var output gatewayGeneratedOutput
	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		info.StreamStatus.RequireTerminal()
		var event any
		switch p.format {
		case types.RelayFormatOpenAI:
			event = &dto.ChatCompletionsStreamResponse{}
		case types.RelayFormatOpenAIResponses:
			event = &dto.ResponsesStreamResponse{}
		case types.RelayFormatClaude:
			event = &dto.ClaudeResponse{}
		case types.RelayFormatGemini:
			event = &dto.GeminiChatResponse{}
		}
		if err := common.UnmarshalJsonStr(data, event); err != nil {
			streamErr = err
			sr.Stop(err)
			return
		}
		results, err := relayconvert.ConvertStreamResponseChunk(c, info, state, event)
		if err != nil {
			streamErr = err
			sr.Stop(err)
			return
		}
		measured, err := meter.Observe([]byte(data))
		if err != nil {
			streamErr = err
			sr.Stop(err)
			return
		}
		if measured != nil {
			usage = measured
		}
		prompt, completion := gatewayUsageCounters([]byte(data), p.format)
		if prompt.Type == gjson.Number && prompt.Int() >= 0 && prompt.Int() <= 1<<31-1 {
			info.ResponseFacts.PromptTokensReported = true
		}
		if completion.Type == gjson.Number && completion.Int() >= 0 && completion.Int() <= 1<<31-1 {
			info.ResponseFacts.CompletionTokensReported = true
		}
		info.ResponseFacts.UsageReported = info.ResponseFacts.PromptTokensReported && info.ResponseFacts.CompletionTokensReported
		switch p.format {
		case types.RelayFormatOpenAIResponses:
			service.ObserveResponsesOutcome(info, event.(*dto.ResponsesStreamResponse))
		case types.RelayFormatClaude:
			if gjson.Get(data, "type").String() == "message_stop" {
				info.StreamStatus.MarkCompleted()
			}
		case types.RelayFormatOpenAI:
			for _, choice := range gjson.Get(data, "choices").Array() {
				if choice.Get("finish_reason").String() != "" {
					info.StreamStatus.MarkCompleted()
				}
			}
		case types.RelayFormatGemini:
			for _, candidate := range gjson.Get(data, "candidates").Array() {
				if candidate.Get("finishReason").String() != "" {
					info.StreamStatus.MarkCompleted()
				}
			}
		}
		upstreamError := gjson.Get(data, "error")
		if upstreamError.Exists() && upstreamError.Type != gjson.Null || gjson.Get(data, "type").String() == "error" || info.StreamStatus.ResponseFailed() {
			streamErr = errors.New("upstream stream failed")
			sr.Stop(streamErr)
			return
		}
		for _, result := range results {
			_, writeErr := writeGatewayStreamResult(c, info, result.Value, &output)
			if writeErr != nil {
				streamErr = writeErr
				sr.Stop(writeErr)
				return
			}
		}
	})
	info.StreamStatus.RequireTerminal()
	outcome := info.StreamStatus.OutcomeSnapshot()
	terminal := outcome.Response == relaycommon.ResponseOutcomeCompleted || outcome.Response == relaycommon.ResponseOutcomeUnknown && outcome.EndReason == relaycommon.StreamEndReasonDone
	if streamErr == nil && (!info.StreamStatus.IsNormalEnd() || outcome.HasErrors || !terminal) {
		streamErr = errors.New("upstream stream did not complete")
	}
	if streamErr == nil {
		results, err := relayconvert.FinalizeStreamResponse(c, info, state)
		streamErr = err
		for _, result := range results {
			if streamErr != nil {
				break
			}
			_, err := writeGatewayStreamResult(c, info, result.Value, &output)
			streamErr = err
		}
	}
	info.ResponseFacts.GeneratedText = output.String()
	if !info.ResponseFacts.PromptTokensReported {
		usage.PromptTokens = info.GetEstimatePromptTokens()
	}
	if !info.ResponseFacts.CompletionTokensReported {
		usage.CompletionTokens = tokenkit.Count(info.TokenizerModelName(), output.String())
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	if streamErr != nil {
		return usage, types.NewError(streamErr, types.ErrorCodeBadResponse)
	}
	if info.RelayFormat == types.RelayFormatOpenAI {
		helper.Done(c)
	}
	return usage, nil
}

func gatewayUsageCounters(body []byte, format types.RelayFormat) (gjson.Result, gjson.Result) {
	switch format {
	case types.RelayFormatOpenAIResponses:
		root := gjson.ParseBytes(body)
		if root.Get("response").Exists() {
			root = root.Get("response")
		}
		return root.Get("usage.input_tokens"), root.Get("usage.output_tokens")
	case types.RelayFormatClaude:
		root := gjson.ParseBytes(body)
		if root.Get("message").Exists() {
			root = root.Get("message")
		}
		return root.Get("usage.input_tokens"), root.Get("usage.output_tokens")
	case types.RelayFormatGemini:
		return gjson.GetBytes(body, "usageMetadata.promptTokenCount"), gjson.GetBytes(body, "usageMetadata.candidatesTokenCount")
	default:
		return gjson.GetBytes(body, "usage.prompt_tokens"), gjson.GetBytes(body, "usage.completion_tokens")
	}
}

func writeGatewayStreamResult(c *gin.Context, info *relaycommon.RelayInfo, value any, output *gatewayGeneratedOutput) (string, error) {
	event := ""
	switch v := value.(type) {
	case relayconvert.ChatToResponsesStreamEvent:
		event, value = v.Type, v.Payload
	case *dto.ResponsesStreamResponse:
		event = v.Type
	case dto.ResponsesStreamResponse:
		event = v.Type
	case *dto.ClaudeResponse:
		event = v.Type
	case dto.ClaudeResponse:
		event = v.Type
	case *dto.ChatCompletionsStreamResponse:
		if !info.ShouldIncludeUsage && len(v.Choices) == 0 {
			return "", nil
		}
		if !info.ShouldIncludeUsage {
			copy := *v
			copy.Usage = nil
			value = &copy
		}
	}
	body, err := common.Marshal(value)
	if err != nil {
		return "", err
	}
	frame := "data: " + string(body) + "\n\n"
	if event != "" {
		frame = "event: " + event + "\n" + frame
	}
	c.Header("Content-Type", "text/event-stream")
	if err := c.Request.Context().Err(); err != nil {
		return "", err
	}
	n, err := c.Writer.Write([]byte(frame))
	if err != nil || n != len(frame) {
		info.ResponseFacts.DownstreamWriteFailed = true
		if err == nil {
			err = io.ErrShortWrite
		}
		return "", err
	}
	text := gatewayStreamBusinessText(body, info.RelayFormat)
	if info.RelayFormat == types.RelayFormatOpenAIResponses {
		if toolText, handled := output.ObserveResponses(body); handled {
			text = toolText
		} else {
			output.text.WriteString(text)
		}
	} else {
		output.text.WriteString(text)
	}
	if text != "" {
		info.ResponseFacts.BusinessOutput = true
	}
	if err := helper.FlushWriter(c); err != nil {
		info.ResponseFacts.DownstreamWriteFailed = true
		return text, &helper.StreamFlushError{Err: err}
	}
	return text, nil
}

func gatewayStreamBusinessText(body []byte, format types.RelayFormat) string {
	root := gjson.ParseBytes(body)
	var text strings.Builder
	switch format {
	case types.RelayFormatOpenAI:
		for _, choice := range root.Get("choices").Array() {
			text.WriteString(choice.Get("delta.content").String())
			text.WriteString(choice.Get("delta.reasoning_content").String())
			for _, tool := range choice.Get("delta.tool_calls").Array() {
				text.WriteString(tool.Get("function.name").String())
				text.WriteString(tool.Get("function.arguments").String())
			}
		}
	case types.RelayFormatOpenAIResponses:
		switch root.Get("type").String() {
		case "response.output_text.delta", "response.reasoning_summary_text.delta", "response.reasoning_text.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta", "response.refusal.delta":
			text.WriteString(root.Get("delta").String())
		case "response.output_item.added":
			if kind := root.Get("item.type").String(); kind == "function_call" || kind == "custom_tool_call" {
				text.WriteString(root.Get("item.name").String())
				text.WriteString(root.Get("item.input").String())
			}
		}
	case types.RelayFormatClaude:
		if root.Get("type").String() == "content_block_start" {
			text.WriteString(gatewayMessagesBlockText(root.Get("content_block")))
		}
		if root.Get("type").String() == "content_block_delta" {
			for _, key := range []string{"text", "thinking", "partial_json"} {
				text.WriteString(root.Get("delta." + key).String())
			}
		}
	case types.RelayFormatGemini:
		for _, candidate := range root.Get("candidates").Array() {
			for _, part := range candidate.Get("content.parts").Array() {
				text.WriteString(gatewayGeminiPartText(part))
			}
		}
	default:
		return ""
	}
	return text.String()
}
