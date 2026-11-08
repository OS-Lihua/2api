package textprotocol

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type gatewayStreamChoice struct {
	finished                                         bool
	audioID, audioData, audioTranscript, audioExpiry bool
	audioDone                                        bool
}

func (p *Handler) gatewayStreamResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)
	state, err := relayconvert.NewResponseStreamState(p.format, info.RelayFormat, relayconvert.ResponseStreamOptions{ID: helper.GetResponseID(c), Model: info.UpstreamModelName, IncludeUsage: true, EmitSequenceNumber: true})
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponse)
	}
	usage := &dto.Usage{}
	meter := gatewayUsageAccumulator{format: p.format, context: c}
	var streamErr error
	var output gatewayGeneratedOutput
	var sourceOutput gatewayGeneratedOutput
	expectedChoices := max(1, info.ResponseFacts.ExpectedChoices)
	choices := map[int]gatewayStreamChoice{}
	sourceBudgetEnd := false
	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		info.StreamStatus.RequireTerminal()
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
		info.ResponseFacts.PromptTokensReported = info.ResponseFacts.PromptTokensReported || gatewayValidCounter(prompt)
		info.ResponseFacts.CompletionTokensReported = info.ResponseFacts.CompletionTokensReported || gatewayValidCounter(completion)
		info.ResponseFacts.UsageReported = info.ResponseFacts.PromptTokensReported && info.ResponseFacts.CompletionTokensReported
		info.ResponseFacts.IndependentUsageReported = meter.ReportedCounters()
		normalized, err := meter.NormalizedBody([]byte(data))
		if err != nil {
			streamErr = err
			sr.Stop(err)
			return
		}
		if observeGatewayError(info, []byte(data)) {
			streamErr = errors.New("upstream stream failed")
			sr.Stop(streamErr)
			return
		}
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
		if err := common.Unmarshal(normalized, event); err != nil {
			streamErr = err
			sr.Stop(err)
			return
		}
		if p.format == types.RelayFormatOpenAIResponses {
			sourceOutput.ObserveResponses(normalized)
		} else {
			sourceOutput.text.WriteString(gatewayStreamBusinessText(normalized, p.format))
			if p.format == types.RelayFormatOpenAI {
				sourceOutput.ObserveChat(normalized)
			} else if p.format == types.RelayFormatGemini {
				sourceOutput.ObserveGemini(normalized)
			}
		}
		info.ResponseFacts.SourceOutputObserved = info.ResponseFacts.SourceOutputObserved || sourceOutput.String() != "" || gatewayMediaOutput(normalized, p.format)
		safety := observeGatewaySafety(info, []byte(data), p.format)
		if p.format == types.RelayFormatOpenAIResponses {
			service.ObserveResponsesOutcome(info, event.(*dto.ResponsesStreamResponse))
		}
		results, err := relayconvert.ConvertStreamResponseChunk(c, info, state, event)
		if err != nil {
			streamErr = err
			sr.Stop(err)
			return
		}
		sourceBudgetEnd = sourceBudgetEnd || gatewayOutputLimitReached(normalized, p.format)
		switch p.format {
		case types.RelayFormatClaude:
			if gjson.Get(data, "type").String() == "message_stop" {
				if sourceBudgetEnd {
					info.StreamStatus.MarkIncomplete("max_output_tokens")
				} else {
					info.StreamStatus.MarkCompleted()
				}
			}
		case types.RelayFormatOpenAI, types.RelayFormatGemini:
			path, terminal := "choices", "finish_reason"
			if p.format == types.RelayFormatGemini {
				path, terminal = "candidates", "finishReason"
			}
			for i, choice := range gjson.Get(data, path).Array() {
				index := i
				if value := choice.Get("index"); value.Exists() {
					index = int(value.Int())
				}
				if index < 0 || index >= expectedChoices {
					streamErr = errors.New("invalid upstream choice index")
					sr.Stop(streamErr)
					return
				}
				current := choices[index]
				current.finished = current.finished || choice.Get(terminal).String() != ""
				if p.format == types.RelayFormatOpenAI {
					if delta := choice.Get("delta"); delta.IsObject() {
						current.audioDone = false
						audio := delta.Get("audio")
						current.audioID = current.audioID || audio.Get("id").Type == gjson.String
						current.audioData = current.audioData || audio.Get("data").Type == gjson.String
						current.audioTranscript = current.audioTranscript || audio.Get("transcript").Type == gjson.String
						current.audioExpiry = current.audioExpiry || audio.Get("expires_at").Type == gjson.Number
						expiryOnly := audio.Get("expires_at").Type == gjson.Number
						for key, value := range audio.Map() {
							if key != "expires_at" && value.Type != gjson.Null {
								expiryOnly = false
							}
						}
						for key, value := range delta.Map() {
							if key != "audio" && value.Type != gjson.Null {
								expiryOnly = false
							}
						}
						current.audioDone = expiryOnly && current.audioID && current.audioData && current.audioTranscript && current.audioExpiry
					}
				}
				choices[index] = current
			}
		}
		if info.StreamStatus.ResponseFailed() {
			streamErr = errors.New("upstream stream failed")
			sr.Stop(streamErr)
			return
		}
		if resp.StatusCode != http.StatusOK {
			return
		}
		output.sourceTerminal = gatewayTerminalSnapshot(normalized, p.format)
		if p.format == info.RelayFormat {
			if _, err := writeGatewayStreamResult(c, info, json.RawMessage(normalized), &output); err != nil {
				streamErr = err
				sr.Stop(err)
				return
			}
		} else {
			for _, result := range results {
				_, writeErr := writeGatewayStreamResult(c, info, result.Value, &output)
				if writeErr != nil {
					streamErr = writeErr
					sr.Stop(writeErr)
					return
				}
			}
		}
		if safety {
			info.StreamStatus.MarkFailed("content_filter", "", 0)
			streamErr = errors.New("upstream content safety rejection")
			sr.Stop(streamErr)
		}
	})
	// An unterminated event cannot confirm delivery or a protocol terminal.
	// Complete JSON inside it can still report a known upstream purchase usage.
	partial := info.ResponseFacts.UnterminatedStreamData
	info.ResponseFacts.UnterminatedStreamData = nil
	if len(partial) > 0 && gjson.ValidBytes(partial) {
		if measured, err := meter.Observe(partial); err == nil {
			if measured != nil {
				usage = measured
			}
			prompt, completion := gatewayUsageCounters(partial, p.format)
			info.ResponseFacts.PromptTokensReported = info.ResponseFacts.PromptTokensReported || gatewayValidCounter(prompt)
			info.ResponseFacts.CompletionTokensReported = info.ResponseFacts.CompletionTokensReported || gatewayValidCounter(completion)
			info.ResponseFacts.UsageReported = info.ResponseFacts.PromptTokensReported && info.ResponseFacts.CompletionTokensReported
			info.ResponseFacts.IndependentUsageReported = meter.ReportedCounters()
		}
		if normalized, err := meter.NormalizedBody(partial); err == nil {
			if p.format == types.RelayFormatOpenAIResponses {
				sourceOutput.ObserveResponses(normalized)
			} else {
				sourceOutput.text.WriteString(gatewayStreamBusinessText(normalized, p.format))
				if p.format == types.RelayFormatOpenAI {
					sourceOutput.ObserveChat(normalized)
				} else if p.format == types.RelayFormatGemini {
					sourceOutput.ObserveGemini(normalized)
				}
			}
			info.ResponseFacts.SourceOutputObserved = info.ResponseFacts.SourceOutputObserved || sourceOutput.String() != "" || gatewayMediaOutput(normalized, p.format)
		}
	}
	info.StreamStatus.RequireTerminal()
	if p.format == types.RelayFormatOpenAI || p.format == types.RelayFormatGemini {
		ended := info.StreamStatus.OutcomeSnapshot().EndReason
		complete := len(choices) == expectedChoices
		for _, choice := range choices {
			complete = complete && (choice.finished || choice.audioDone)
		}
		if complete {
			if sourceBudgetEnd {
				info.StreamStatus.MarkIncomplete("max_output_tokens")
			} else {
				info.StreamStatus.MarkCompleted()
			}
		} else if ended == relaycommon.StreamEndReasonDone {
			info.StreamStatus.MarkFailed("stream_interrupted", "", 0)
		}
	}
	if info.StreamStatus.OutcomeSnapshot().Response == relaycommon.ResponseOutcomeUnknown {
		info.StreamStatus.MarkFailed("stream_interrupted", "", 0)
	}
	if streamErr == nil && !info.StreamStatus.CompletedNormally() {
		streamErr = errors.New("upstream stream did not complete")
	}
	if resp.StatusCode != http.StatusOK {
		streamErr = errors.New("upstream response failed")
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
	if streamErr == nil {
		output.terminalReady = true
		for _, value := range output.pendingTerminal {
			if _, err := writeGatewayStreamResult(c, info, value, &output); err != nil {
				streamErr = err
				break
			}
		}
	}
	budgetEnd := streamErr == nil && info.StreamStatus.CompletedNormally() && info.StreamStatus.OutcomeSnapshot().OutputLimitReached()
	if budgetEnd && !info.ResponseFacts.BusinessOutput && len(info.ResponseFacts.StreamPrelude) > 0 {
		maps.Copy(c.Writer.Header(), info.ResponseFacts.StreamHeaders)
		info.ResponseFacts.StreamHeaders = nil
		prelude := string(info.ResponseFacts.StreamPrelude)
		info.ResponseFacts.StreamPrelude = nil
		if err := helper.WriteGatewayStreamFrame(c, nil, prelude, false); err != nil {
			info.ResponseFacts.DownstreamWriteFailed = true
			streamErr = err
		}
	}
	info.ResponseFacts.SourceGeneratedText = sourceOutput.String()
	info.ResponseFacts.GeneratedText = output.String()
	if p.format == types.RelayFormatOpenAI || p.format == types.RelayFormatGemini {
		sourceTokens := sourceOutput.TextTokenEstimate(info.TokenizerModelName())
		if p.format == types.RelayFormatOpenAI {
			sourceTokens += len(sourceOutput.chatTools) * 7
		}
		info.ResponseFacts.SourceOutputTokens = &sourceTokens
	}
	generatedTokens := output.TextTokenEstimate(info.TokenizerModelName())
	if p.format == types.RelayFormatOpenAI && info.RelayFormat == types.RelayFormatOpenAI {
		generatedTokens += len(output.chatTools) * 7
	}
	if info.RelayFormat == types.RelayFormatOpenAI || info.RelayFormat == types.RelayFormatGemini {
		info.ResponseFacts.GeneratedOutputTokens = &generatedTokens
	}
	if !info.ResponseFacts.PromptTokensReported {
		usage.PromptTokens = max(usage.PromptTokens, info.GetEstimatePromptTokens())
	}
	if !info.ResponseFacts.CompletionTokensReported {
		usage.CompletionTokens = max(usage.CompletionTokens, generatedTokens)
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	if streamErr != nil {
		return usage, types.NewError(streamErr, types.ErrorCodeBadResponse)
	}
	if info.RelayFormat == types.RelayFormatOpenAI {
		if budgetEnd && !info.ResponseFacts.BusinessOutput {
			if err := helper.StringData(c, "[DONE]"); err != nil {
				info.ResponseFacts.DownstreamWriteFailed = true
				return usage, types.NewError(err, types.ErrorCodeBadResponse)
			}
		} else if err := helper.Done(c); err != nil {
			info.ResponseFacts.DownstreamWriteFailed = true
			return usage, types.NewError(err, types.ErrorCodeBadResponse)
		}
	}
	return usage, nil
}

func gatewayOutputLimitReached(body []byte, format types.RelayFormat) bool {
	switch format {
	case types.RelayFormatOpenAIResponses:
		return gjson.GetBytes(body, "status").String() == "incomplete" && gjson.GetBytes(body, "incomplete_details.reason").String() == "max_output_tokens"
	case types.RelayFormatClaude:
		return gjson.GetBytes(body, "stop_reason").String() == "max_tokens" || gjson.GetBytes(body, "delta.stop_reason").String() == "max_tokens"
	case types.RelayFormatOpenAI, types.RelayFormatGemini:
		path, terminal, reason := "choices", "finish_reason", "length"
		if format == types.RelayFormatGemini {
			path, terminal, reason = "candidates", "finishReason", "MAX_TOKENS"
		}
		for _, choice := range gjson.GetBytes(body, path).Array() {
			if choice.Get(terminal).String() == reason {
				return true
			}
		}
	}
	return false
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
	case json.RawMessage:
		event = gjson.GetBytes(v, "type").String()
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
	}
	body, err := common.Marshal(value)
	if err != nil {
		return "", err
	}
	if info.RelayFormat == types.RelayFormatOpenAI && !info.ShouldIncludeUsage {
		if len(gjson.GetBytes(body, "choices").Array()) == 0 {
			return "", nil
		}
		body, err = sjson.DeleteBytes(body, "usage")
		if err != nil {
			return "", err
		}
	}
	terminal := gatewayTerminalSnapshot(body, info.RelayFormat)
	if (terminal || output.sourceTerminal || len(output.pendingTerminal) > 0) && !output.terminalReady && !info.PerformanceBusinessRejection {
		// Terminal snapshots can contain complete image or audio payloads.
		if output.terminalBytes+len(body) > 64<<20 {
			return "", helper.ErrGatewayStreamPreludeLimit
		}
		output.terminalBytes += len(body)
		output.pendingTerminal = append(output.pendingTerminal, json.RawMessage(body))
		return "", nil
	}
	generatedBody := body
	if info.RelayFormat == types.RelayFormatOpenAI && info.ChannelSetting.ThinkingToContent {
		body, err = helper.ChatThinkingToContent(info, body)
		if err != nil {
			return "", err
		}
	}
	frame := "data: " + string(body) + "\n\n"
	if event != "" {
		frame = "event: " + event + "\n" + frame
	}
	c.Header("Content-Type", "text/event-stream")
	if err := c.Request.Context().Err(); err != nil {
		return "", err
	}
	text := gatewayStreamBusinessText(generatedBody, info.RelayFormat)
	if info.RelayFormat == types.RelayFormatOpenAIResponses {
		var probe gatewayGeneratedOutput
		if value, handled := probe.ObserveResponses(body); handled {
			text = value
		}
	}
	business := text != "" || gatewayMediaOutput(body, info.RelayFormat)
	writeErr := helper.WriteGatewayStreamFrame(c, info.ResponseFacts, frame, business)
	if writeErr != nil {
		if !errors.Is(writeErr, helper.ErrGatewayStreamPreludeLimit) {
			info.ResponseFacts.DownstreamWriteFailed = true
		}
		var flushErr *helper.StreamFlushError
		if !errors.As(writeErr, &flushErr) {
			return "", writeErr
		}
	}
	if info.RelayFormat == types.RelayFormatOpenAI {
		output.ObserveChat(generatedBody)
	} else if info.RelayFormat == types.RelayFormatGemini {
		output.ObserveGemini(generatedBody)
	}
	if info.RelayFormat == types.RelayFormatOpenAIResponses {
		if toolText, handled := output.ObserveResponses(body); handled {
			text = toolText
		} else {
			output.text.WriteString(text)
		}
	} else {
		output.text.WriteString(text)
	}
	if writeErr == nil && info.ResponseFacts.BusinessOutput && info.PerformanceBusinessRejection && info.RelayFormat == types.RelayFormatOpenAIResponses && event == "response.incomplete" {
		info.ResponseFacts.TerminalDelivered = true
	}
	return text, writeErr
}

func gatewayTerminalSnapshot(body []byte, format types.RelayFormat) bool {
	root := gjson.ParseBytes(body)
	switch format {
	case types.RelayFormatClaude:
		event := root.Get("type").String()
		return event == "message_stop" || event == "message_delta" && root.Get("delta.stop_reason").String() != ""
	case types.RelayFormatOpenAIResponses:
		event := root.Get("type").String()
		return event == "response.completed" || event == "response.done" || event == "response.incomplete" && root.Get("response.incomplete_details.reason").String() == "max_output_tokens"
	case types.RelayFormatOpenAI:
		for _, choice := range root.Get("choices").Array() {
			if choice.Get("finish_reason").String() != "" || choice.Get("delta.audio.expires_at").Type == gjson.Number {
				return true
			}
		}
	case types.RelayFormatGemini:
		for _, candidate := range root.Get("candidates").Array() {
			if candidate.Get("finishReason").String() != "" {
				return true
			}
		}
	}
	return false
}

func gatewayStreamBusinessText(body []byte, format types.RelayFormat) string {
	root := gjson.ParseBytes(body)
	var text strings.Builder
	switch format {
	case types.RelayFormatOpenAI:
		return relaycommon.ChatStreamBusinessText(body)
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
