package ollama

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/tidwall/gjson"

	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
)

type ollamaChatStreamChunk struct {
	Model     string `json:"model"`
	CreatedAt string `json:"created_at"`
	// chat
	Message *struct {
		Role      string           `json:"role"`
		Content   string           `json:"content"`
		Thinking  json.RawMessage  `json:"thinking"`
		ToolCalls []OllamaToolCall `json:"tool_calls"`
	} `json:"message"`
	// generate
	Response           string `json:"response"`
	Done               bool   `json:"done"`
	DoneReason         string `json:"done_reason"`
	TotalDuration      int64  `json:"total_duration"`
	LoadDuration       int64  `json:"load_duration"`
	PromptEvalCount    *int   `json:"prompt_eval_count"`
	EvalCount          *int   `json:"eval_count"`
	PromptEvalDuration int64  `json:"prompt_eval_duration"`
	EvalDuration       int64  `json:"eval_duration"`
}

func ollamaToolCallsToOpenAI(toolCalls []OllamaToolCall, startIndex int, includeIndex bool) ([]dto.ToolCallResponse, int) {
	if len(toolCalls) == 0 {
		return nil, startIndex
	}
	result := make([]dto.ToolCallResponse, 0, len(toolCalls))
	for _, tc := range toolCalls {
		var argBytes []byte
		var err error
		if tc.Function.Arguments == nil {
			argBytes = []byte("{}")
		} else {
			argBytes, err = common.Marshal(tc.Function.Arguments)
			if err != nil || len(argBytes) == 0 {
				argBytes = []byte("{}")
			}
		}
		toolCallID := tc.ID
		if toolCallID == "" {
			toolCallID = fmt.Sprintf("call_%d", startIndex)
		}
		tr := dto.ToolCallResponse{
			ID:   toolCallID,
			Type: "function",
			Function: dto.FunctionResponse{
				Name:      tc.Function.Name,
				Arguments: string(argBytes),
			},
		}
		if includeIndex {
			tr.SetIndex(startIndex)
		}
		startIndex++
		result = append(result, tr)
	}
	return result, startIndex
}

func buildOllamaStreamDelta(chunk *ollamaChatStreamChunk, responseID string, created int64, model string, toolCallIndex *int) (dto.ChatCompletionsStreamResponse, bool) {
	delta := dto.ChatCompletionsStreamResponse{
		Id:      responseID,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []dto.ChatCompletionsStreamResponseChoice{{
			Index: 0,
			Delta: dto.ChatCompletionsStreamResponseChoiceDelta{Role: "assistant"},
		}},
	}

	var content string
	if chunk.Message != nil {
		content = chunk.Message.Content
	} else {
		content = chunk.Response
	}
	if content != "" {
		delta.Choices[0].Delta.SetContentString(content)
	}

	hasPayload := content != ""
	if chunk.Message != nil && len(chunk.Message.Thinking) > 0 {
		raw := strings.TrimSpace(string(chunk.Message.Thinking))
		if raw != "" && raw != "null" {
			// Unmarshal the JSON string to get the actual content without quotes
			var thinkingContent string
			if err := common.Unmarshal(chunk.Message.Thinking, &thinkingContent); err == nil {
				delta.Choices[0].Delta.SetReasoningContent(thinkingContent)
			} else {
				// Fallback to raw string if it's not a JSON string
				delta.Choices[0].Delta.SetReasoningContent(raw)
			}
			hasPayload = true
		}
	}
	if chunk.Message != nil && len(chunk.Message.ToolCalls) > 0 {
		delta.Choices[0].Delta.ToolCalls, *toolCallIndex = ollamaToolCallsToOpenAI(chunk.Message.ToolCalls, *toolCallIndex, true)
		hasPayload = true
	}

	return delta, hasPayload
}

func toUnix(ts string) int64 {
	if ts == "" {
		return time.Now().Unix()
	}
	// try time.RFC3339 or with nanoseconds
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t2, err2 := time.Parse(time.RFC3339, ts)
		if err2 == nil {
			return t2.Unix()
		}
		return time.Now().Unix()
	}
	return t.Unix()
}

func sendOllamaDelta(c *gin.Context, info *relaycommon.RelayInfo, delta dto.ChatCompletionsStreamResponse) error {
	err := helper.ObjectData(c, delta)
	if err != nil && info.ResponseFacts != nil {
		info.ResponseFacts.DownstreamWriteFailed = true
	}
	var flushErr *helper.StreamFlushError
	if info.ResponseFacts != nil && (err == nil || errors.As(err, &flushErr)) {
		for _, choice := range delta.Choices {
			text := choice.Delta.GetContentString() + choice.Delta.GetReasoningContent()
			for _, call := range choice.Delta.ToolCalls {
				text += call.Function.Name + call.Function.Arguments
			}
			if text != "" || len(choice.Delta.ToolCalls) > 0 {
				info.ResponseFacts.MarkBusinessOutput()
				info.ResponseFacts.GeneratedText += text
			}
		}
	}
	return err
}

func ollamaStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (usage *dto.Usage, responseErr *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("empty response"), types.ErrorCodeBadResponse, http.StatusBadRequest)
	}
	defer service.CloseResponseBodyGracefully(resp)

	if info.StreamStatus == nil {
		info.StreamStatus = relaycommon.NewStreamStatus()
	}
	info.StreamStatus.RequireTerminal()
	defer info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonEOF, nil)
	helper.SetEventStreamHeaders(c)
	scanner := helper.NewStreamScanner(resp.Body)
	usage = &dto.Usage{}
	defer func() {
		if info.ResponseFacts == nil || info.ResponseFacts.UsageReported {
			return
		}
		estimated := service.ResponseText2Usage(c, info.ResponseFacts.GeneratedText, info.TokenizerModelName(), info.GetEstimatePromptTokens())
		if !info.ResponseFacts.PromptTokensReported {
			usage.PromptTokens = estimated.PromptTokens
		}
		if !info.ResponseFacts.CompletionTokensReported {
			usage.CompletionTokens = estimated.CompletionTokens
		}
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}()
	var model = info.UpstreamModelName
	var responseId = common.GetUUID()
	var created = time.Now().Unix()
	var toolCallIndex int
	start := helper.GenerateStartEmptyResponse(responseId, created, model, nil)
	if data, err := common.Marshal(start); err == nil {
		_ = helper.StringData(c, string(data))
	}

	for scanner.Scan() {
		line := scanner.Text()
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if info.ResponseFacts != nil && info.ResponseFacts.OnBusinessOutput != nil && info.StreamStatus.CompletedNormally() {
			return usage, types.NewError(fmt.Errorf("Ollama stream contains data after completion"), types.ErrorCodeBadResponseBody)
		}
		var chunk ollamaChatStreamChunk
		if info.ResponseFacts != nil {
			info.ResponseFacts.RecordNativeUsage(usage, []byte(line), "prompt_eval_count", "eval_count")
			line = string(relaycommon.SanitizeNativeUsage([]byte(line), ""))
		}
		if err := common.Unmarshal([]byte(line), &chunk); err != nil {
			helper.RecordNativeStreamSource(c, info, ollamaSourceResponse([]string{line}, toolCallIndex))
			info.StreamStatus.MarkFailed("invalid_response", "upstream_error", 0)
			logger.LogError(c, "ollama stream JSON decode failed")
			return usage, types.NewOpenAIError(fmt.Errorf("invalid Ollama response"), types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
		}
		if chunk.Model != "" {
			model = chunk.Model
		}
		created = toUnix(chunk.CreatedAt)

		if chunk.PromptEvalCount != nil || chunk.EvalCount != nil {
			if info.ResponseFacts == nil {
				usage.PromptTokens = lo.FromPtr(chunk.PromptEvalCount)
				usage.CompletionTokens = lo.FromPtr(chunk.EvalCount)
			} else {
				if count := chunk.PromptEvalCount; count != nil && *count >= 0 && *count <= 1<<31-1 {
					usage.PromptTokens = *count
					info.ResponseFacts.PromptTokensReported = true
				}
				if count := chunk.EvalCount; count != nil && *count >= 0 && *count <= 1<<31-1 {
					usage.CompletionTokens = *count
					info.ResponseFacts.CompletionTokensReported = true
				}
				info.ResponseFacts.UsageReported = info.ResponseFacts.PromptTokensReported && info.ResponseFacts.CompletionTokensReported
			}
			usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		}
		if delta, hasPayload := buildOllamaStreamDelta(&chunk, responseId, created, model, &toolCallIndex); hasPayload {
			if err := sendOllamaDelta(c, info, delta); err != nil {
				return usage, types.NewError(err, types.ErrorCodeBadResponseBody)
			}
		}
		if !chunk.Done {
			continue
		}
		info.StreamStatus.MarkCompleted()
		info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonDone, nil)
		if info.ResponseFacts != nil && !info.ResponseFacts.UsageReported {
			estimated := service.ResponseText2Usage(c, info.ResponseFacts.GeneratedText, info.TokenizerModelName(), info.GetEstimatePromptTokens())
			if !info.ResponseFacts.PromptTokensReported {
				usage.PromptTokens = estimated.PromptTokens
			}
			if !info.ResponseFacts.CompletionTokensReported {
				usage.CompletionTokens = estimated.CompletionTokens
			}
			usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		}
		finishReason := chunk.DoneReason
		if finishReason == "" {
			finishReason = "stop"
		}
		if toolCallIndex > 0 && finishReason == "stop" {
			finishReason = constant.FinishReasonToolCalls
		}
		// emit stop delta
		if stop := helper.GenerateStopResponse(responseId, created, model, finishReason); stop != nil {
			if data, err := common.Marshal(stop); err == nil {
				_ = helper.StringData(c, string(data))
			}
		}
		// emit usage frame
		if final := helper.GenerateFinalUsageResponse(responseId, created, model, *usage); final != nil {
			if data, err := common.Marshal(final); err == nil {
				_ = helper.StringData(c, string(data))
			}
		}
		// send [DONE]
		if info.ResponseFacts == nil || info.ResponseFacts.OnBusinessOutput == nil || info.ResponseFacts.BusinessOutput {
			helper.Done(c)
		}
		if info.ResponseFacts == nil || info.ResponseFacts.OnBusinessOutput == nil {
			break
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonScannerErr, err)
		return usage, types.NewError(fmt.Errorf("Ollama stream read failed"), types.ErrorCodeBadResponseBody)
	}
	return usage, nil
}

// ollamaSourceResponse extracts complete generation fields independently from
// metadata and terminal decoding, retaining earlier NDJSON fragments.
func ollamaSourceResponse(lines []string, index int) *dto.OpenAITextResponse {
	var content, reasoning strings.Builder
	var calls []dto.ToolCallResponse
	for _, line := range lines {
		var text string
		if common.UnmarshalJsonStr(gjson.Get(line, "message.content").Raw, &text) != nil || text == "" {
			_ = common.UnmarshalJsonStr(gjson.Get(line, "response").Raw, &text)
		}
		content.WriteString(text)
		var thought string
		if common.UnmarshalJsonStr(gjson.Get(line, "message.thinking").Raw, &thought) == nil {
			reasoning.WriteString(thought)
		}
		for _, raw := range gjson.Get(line, "message.tool_calls").Array() {
			var call OllamaToolCall
			if common.UnmarshalJsonStr(raw.Raw, &call) != nil {
				continue
			}
			var converted []dto.ToolCallResponse
			converted, index = ollamaToolCallsToOpenAI([]OllamaToolCall{call}, index, true)
			calls = append(calls, converted...)
		}
	}
	message := dto.Message{Role: "assistant", Content: content.String()}
	if reasoning.Len() > 0 {
		message.ReasoningContent = common.GetPointer(reasoning.String())
	}
	if len(calls) > 0 {
		message.ToolCalls, _ = common.Marshal(calls)
	}
	return &dto.OpenAITextResponse{Choices: []dto.OpenAITextResponseChoice{{Message: message}}}
}

// non-stream handler for chat/generate
func ollamaChatHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	body, readErr := io.ReadAll(resp.Body)
	service.CloseResponseBodyGracefully(resp)
	var knownUsage *dto.Usage
	if info.ResponseFacts != nil {
		knownUsage = &dto.Usage{}
		info.ResponseFacts.RecordNativeUsage(knownUsage, body, "prompt_eval_count", "eval_count")
	}
	raw := string(body)
	lines := strings.Split(raw, "\n")
	var complete common.RawMessage
	if common.Unmarshal(body, &complete) == nil {
		lines = []string{raw}
	}
	if info.ResponseFacts != nil {
		info.ResponseFacts.RecordChatSource(ollamaSourceResponse(lines, 0), info.TokenizerModelName())
	}
	var (
		aggContent       strings.Builder
		reasoningBuilder strings.Builder
		lastChunk        ollamaChatStreamChunk
		parsedAny        bool
		parsingErr       error
		toolCallIndex    int
		toolCalls        []dto.ToolCallResponse
	)
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		if info.ResponseFacts != nil {
			info.ResponseFacts.RecordNativeUsage(knownUsage, []byte(ln), "prompt_eval_count", "eval_count")
			ln = string(relaycommon.SanitizeNativeUsage([]byte(ln), ""))
		}
		var ck ollamaChatStreamChunk
		if err := common.Unmarshal([]byte(ln), &ck); err != nil {
			parsingErr = err
			if len(lines) == 1 {
				if readErr != nil {
					return knownUsage, types.NewOpenAIError(readErr, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
				}
				return knownUsage, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
			}
			continue
		}
		parsedAny = true
		lastChunk = ck
		if ck.Message != nil && len(ck.Message.Thinking) > 0 {
			raw := strings.TrimSpace(string(ck.Message.Thinking))
			if raw != "" && raw != "null" {
				// Unmarshal the JSON string to get the actual content without quotes
				var thinkingContent string
				if err := common.Unmarshal(ck.Message.Thinking, &thinkingContent); err == nil {
					reasoningBuilder.WriteString(thinkingContent)
				} else {
					// Fallback to raw string if it's not a JSON string
					reasoningBuilder.WriteString(raw)
				}
			}
		}
		if ck.Message != nil && ck.Message.Content != "" {
			aggContent.WriteString(ck.Message.Content)
		} else if ck.Response != "" {
			aggContent.WriteString(ck.Response)
		}
		if ck.Message != nil && len(ck.Message.ToolCalls) > 0 {
			var converted []dto.ToolCallResponse
			converted, toolCallIndex = ollamaToolCallsToOpenAI(ck.Message.ToolCalls, toolCallIndex, false)
			toolCalls = append(toolCalls, converted...)
		}
	}

	if !parsedAny {
		var single ollamaChatStreamChunk
		if info.ResponseFacts != nil {
			body = relaycommon.SanitizeNativeUsage(body, "")
		}
		if err := common.Unmarshal(body, &single); err != nil {
			if readErr != nil {
				return knownUsage, types.NewOpenAIError(readErr, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
			}
			return knownUsage, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
		}
		parsingErr = nil
		lastChunk = single
		if single.Message != nil {
			if len(single.Message.Thinking) > 0 {
				raw := strings.TrimSpace(string(single.Message.Thinking))
				if raw != "" && raw != "null" {
					// Unmarshal the JSON string to get the actual content without quotes
					var thinkingContent string
					if err := common.Unmarshal(single.Message.Thinking, &thinkingContent); err == nil {
						reasoningBuilder.WriteString(thinkingContent)
					} else {
						// Fallback to raw string if it's not a JSON string
						reasoningBuilder.WriteString(raw)
					}
				}
			}
			aggContent.WriteString(single.Message.Content)
			if len(single.Message.ToolCalls) > 0 {
				var converted []dto.ToolCallResponse
				converted, toolCallIndex = ollamaToolCallsToOpenAI(single.Message.ToolCalls, toolCallIndex, false)
				toolCalls = append(toolCalls, converted...)
			}
		} else {
			aggContent.WriteString(single.Response)
		}
	}

	model := lastChunk.Model
	if model == "" {
		model = info.UpstreamModelName
	}
	created := toUnix(lastChunk.CreatedAt)
	usage := &dto.Usage{PromptTokens: lo.FromPtr(lastChunk.PromptEvalCount), CompletionTokens: lo.FromPtr(lastChunk.EvalCount)}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	content := aggContent.String()
	finishReason := lastChunk.DoneReason
	if finishReason == "" {
		finishReason = "stop"
	}
	if len(toolCalls) > 0 && finishReason == "stop" {
		finishReason = constant.FinishReasonToolCalls
	}

	msg := dto.Message{Role: "assistant"}
	if content != "" {
		msg.SetStringContent(content)
	}
	if len(toolCalls) > 0 {
		if rawToolCalls, err := common.Marshal(toolCalls); err == nil {
			msg.ToolCalls = rawToolCalls
		}
	}
	if rc := reasoningBuilder.String(); rc != "" {
		msg.ReasoningContent = &rc
	}
	if info.ResponseFacts != nil {
		usage = knownUsage
		if resp.StatusCode != http.StatusOK {
			return usage, types.NewErrorWithStatusCode(fmt.Errorf("upstream request failed"), types.ErrorCodeBadResponse, resp.StatusCode)
		}
		var output strings.Builder
		output.WriteString(content)
		output.WriteString(msg.GetReasoningContent())
		for _, call := range toolCalls {
			output.WriteString(call.Function.Name)
			output.WriteString(call.Function.Arguments)
		}
		info.ResponseFacts.GeneratedText = output.String()
		if !info.ResponseFacts.UsageReported {
			estimated := service.ResponseText2Usage(c, output.String(), info.TokenizerModelName(), info.GetEstimatePromptTokens())
			if !info.ResponseFacts.PromptTokensReported {
				usage.PromptTokens = estimated.PromptTokens
			}
			if !info.ResponseFacts.CompletionTokensReported {
				usage.CompletionTokens = estimated.CompletionTokens
			}
			usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		}
	}
	if info.ResponseFacts != nil && resp.StatusCode != http.StatusOK {
		return usage, types.NewErrorWithStatusCode(fmt.Errorf("upstream request failed"), types.ErrorCodeBadResponse, resp.StatusCode)
	}
	full := dto.OpenAITextResponse{
		Id:      common.GetUUID(),
		Model:   model,
		Object:  "chat.completion",
		Created: created,
		Choices: []dto.OpenAITextResponseChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: finishReason,
		}},
		Usage: *usage,
	}
	if info.ResponseFacts != nil && parsingErr == nil {
		info.ResponseFacts.RecordChatSource(&full, info.TokenizerModelName())
	}
	if readErr != nil {
		return usage, types.NewOpenAIError(readErr, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	if info.ResponseFacts != nil {
		if parsingErr != nil {
			return usage, types.NewError(parsingErr, types.ErrorCodeBadResponseBody)
		}
		if !lastChunk.Done {
			return usage, types.NewError(fmt.Errorf("Ollama response did not complete"), types.ErrorCodeBadResponseBody)
		}
		return helper.WriteChatResponse(c, info, resp, &full)
	}
	out, _ := common.Marshal(full)
	_ = service.IOCopyBytesGracefully(c, resp, out)
	return usage, nil
}
