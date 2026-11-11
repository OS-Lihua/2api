package cohere

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
)

func requestOpenAI2Cohere(textRequest dto.GeneralOpenAIRequest) *CohereRequest {
	cohereReq := CohereRequest{
		Model:            textRequest.Model,
		ChatHistory:      []ChatHistory{},
		Message:          "",
		Stream:           lo.FromPtrOr(textRequest.Stream, false),
		MaxTokens:        textRequest.GetMaxTokens(),
		Temperature:      textRequest.Temperature,
		P:                textRequest.TopP,
		K:                textRequest.TopK,
		FrequencyPenalty: textRequest.FrequencyPenalty,
		PresencePenalty:  textRequest.PresencePenalty,
		Seed:             textRequest.Seed,
		StopSequences:    textRequest.Stop,
	}
	if common.CohereSafetySetting != "NONE" {
		cohereReq.SafetyMode = common.CohereSafetySetting
	}
	if stop, ok := textRequest.Stop.(string); ok {
		cohereReq.StopSequences = []string{stop}
	}
	if textRequest.MaxTokens == nil && textRequest.MaxCompletionTokens == nil {
		cohereReq.MaxTokens = 4000
	}
	lastUser := -1
	for index, message := range textRequest.Messages {
		if message.Role == "user" {
			lastUser = index
		}
	}
	for index, msg := range textRequest.Messages {
		if index == lastUser {
			cohereReq.Message = msg.StringContent()
		} else {
			var role string
			if msg.Role == "assistant" {
				role = "CHATBOT"
			} else if msg.Role == "system" {
				role = "SYSTEM"
			} else {
				role = "USER"
			}
			cohereReq.ChatHistory = append(cohereReq.ChatHistory, ChatHistory{
				Role:    role,
				Message: msg.StringContent(),
			})
		}
	}

	return &cohereReq
}

func requestConvertRerank2Cohere(rerankRequest dto.RerankRequest) *CohereRerankRequest {
	topN := lo.FromPtrOr(rerankRequest.TopN, 1)
	if topN <= 0 {
		topN = 1
	}
	cohereReq := CohereRerankRequest{
		Query:           rerankRequest.Query,
		Documents:       rerankRequest.Documents,
		Model:           rerankRequest.Model,
		TopN:            topN,
		ReturnDocuments: true,
	}
	return &cohereReq
}

func stopReasonCohere2OpenAI(reason string) string {
	switch reason {
	case "COMPLETE":
		return "stop"
	case "ERROR_TOXIC":
		return constant.FinishReasonContentFilter
	case "MAX_TOKENS":
		return "length"
	default:
		return reason
	}
}

// recordCohereStreamSnapshot adds only the previously unseen part of a final
// response.text snapshot to procurement facts, without publishing it downstream.
func recordCohereStreamSnapshot(c *gin.Context, info *relaycommon.RelayInfo, body []byte) {
	if info.ResponseFacts == nil {
		return
	}
	source := relaycommon.NativeTextSource(body, "", "response.text")
	if len(source.Choices) == 0 {
		return
	}
	text := source.Choices[0].Message.StringContent()
	if suffix, ok := strings.CutPrefix(text, info.ResponseFacts.SourceGeneratedText); ok {
		source.Choices[0].Message.Content = suffix
		helper.RecordNativeStreamSource(c, info, source)
	}
}

func cohereStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)
	responseId := helper.GetResponseID(c)
	createdTime := common.GetTimestamp()
	usage := &dto.Usage{}
	var responseText strings.Builder
	finished := false
	managed := info.ResponseFacts != nil && info.ResponseFacts.OnBusinessOutput != nil
	if info.StreamStatus == nil {
		info.StreamStatus = relaycommon.NewStreamStatus()
	}
	info.StreamStatus.RequireTerminal()
	defer info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonEOF, nil)
	defer helper.EstimateMissingChatUsage(c, info, usage)
	sourceReader := &io.LimitedReader{R: resp.Body, N: math.MaxInt64}
	scanner := helper.NewStreamScanner(sourceReader)
	defer func() {
		if !managed {
			return
		}
		// Drain only bytes already received by this scanner.
		sourceReader.N = 0
		for scanner.Scan() {
			info.ResponseFacts.RecordNativeUsage(usage, scanner.Bytes(), "response.meta.billed_units.input_tokens", "response.meta.billed_units.output_tokens")
			helper.RecordNativeStreamSource(c, info, relaycommon.NativeTextSource(scanner.Bytes(), "", "text"))
			recordCohereStreamSnapshot(c, info, scanner.Bytes())
		}
	}()
	helper.SetEventStreamHeaders(c)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		if finished {
			info.ResponseFacts.RecordNativeUsage(usage, scanner.Bytes(), "response.meta.billed_units.input_tokens", "response.meta.billed_units.output_tokens")
			helper.RecordNativeStreamSource(c, info, relaycommon.NativeTextSource(scanner.Bytes(), "", "text"))
			recordCohereStreamSnapshot(c, info, scanner.Bytes())
			return usage, types.NewError(fmt.Errorf("Cohere stream contains data after completion"), types.ErrorCodeBadResponseBody)
		}
		info.SetFirstResponseTime()
		body := scanner.Bytes()
		if info.ResponseFacts != nil {
			info.ResponseFacts.RecordNativeUsage(usage, body, "response.meta.billed_units.input_tokens", "response.meta.billed_units.output_tokens")
			body = relaycommon.SanitizeNativeUsage(body, "response.meta.billed_units")
		}
		var cohereResp CohereResponse
		if err := common.Unmarshal(body, &cohereResp); err != nil {
			helper.RecordNativeStreamSource(c, info, relaycommon.NativeTextSource(body, "", "text"))
			recordCohereStreamSnapshot(c, info, body)
			if info.ResponseFacts == nil {
				continue
			}
			return usage, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		if cohereResp.Response != nil {
			recordCohereStreamSnapshot(c, info, body)
			reported := dto.Usage{PromptTokens: cohereResp.Response.Meta.BilledUnits.InputTokens, CompletionTokens: cohereResp.Response.Meta.BilledUnits.OutputTokens}
			if info.ResponseFacts != nil {
				prompt, completion := relaycommon.NativeUsagePresence(body, "response.meta.billed_units.input_tokens", "response.meta.billed_units.output_tokens", "")
				info.ResponseFacts.RecordUsage(usage, &reported, prompt, completion)
			} else {
				*usage = reported
			}
		}
		response := dto.ChatCompletionsStreamResponse{Id: responseId, Created: createdTime, Object: "chat.completion.chunk", Model: info.UpstreamModelName}
		choice := dto.ChatCompletionsStreamResponseChoice{Index: 0}
		if cohereResp.IsFinished {
			finishReason := stopReasonCohere2OpenAI(cohereResp.FinishReason)
			choice.FinishReason = &finishReason
		} else {
			choice.Delta.Role = "assistant"
			choice.Delta.SetContentString(cohereResp.Text)
			responseText.WriteString(cohereResp.Text)
		}
		response.Choices = []dto.ChatCompletionsStreamResponseChoice{choice}
		if err := helper.SendChatChunk(c, info, &response); err != nil {
			return usage, types.NewError(err, types.ErrorCodeBadResponse)
		}
		if cohereResp.IsFinished {
			if strings.HasPrefix(cohereResp.FinishReason, "ERROR") {
				return usage, types.NewError(fmt.Errorf("Cohere stream failed"), types.ErrorCodeBadResponse)
			}
			info.StreamStatus.MarkCompleted()
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonDone, nil)
			if !managed {
				break
			}
			finished = true
		}
	}
	if err := scanner.Err(); err != nil {
		info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonScannerErr, err)
		return usage, types.NewError(err, types.ErrorCodeReadResponseBodyFailed)
	}
	helper.Done(c)
	if info.ResponseFacts == nil && usage.PromptTokens == 0 {
		*usage = *service.ResponseText2Usage(c, responseText.String(), info.UpstreamModelName, info.GetEstimatePromptTokens())
	}
	return usage, nil
}

func cohereHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	createdTime := common.GetTimestamp()
	responseBody, readErr := io.ReadAll(resp.Body)
	service.CloseResponseBodyGracefully(resp)
	usage := dto.Usage{}
	if info.ResponseFacts != nil {
		responseBody = relaycommon.SanitizeNativeUsage(responseBody, "meta.billed_units")
		info.ResponseFacts.RecordNativeUsage(&usage, responseBody, "meta.billed_units.input_tokens", "meta.billed_units.output_tokens")
	}
	var cohereResp CohereResponseResult
	if info.ResponseFacts != nil {
		info.ResponseFacts.RecordNativeTextSource(responseBody, "", "text", info.TokenizerModelName())
	}
	err := common.Unmarshal(responseBody, &cohereResp)
	if readErr != nil {
		return &usage, types.NewError(readErr, types.ErrorCodeReadResponseBodyFailed)
	}
	if err != nil {
		return &usage, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	if info.ResponseFacts == nil {
		usage.PromptTokens = cohereResp.Meta.BilledUnits.InputTokens
		usage.CompletionTokens = cohereResp.Meta.BilledUnits.OutputTokens
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}

	var openaiResp dto.OpenAITextResponse
	openaiResp.Id = cohereResp.ResponseId
	openaiResp.Created = createdTime
	openaiResp.Object = "chat.completion"
	openaiResp.Model = info.UpstreamModelName
	openaiResp.Usage = usage

	openaiResp.Choices = []dto.OpenAITextResponseChoice{
		{
			Index:        0,
			Message:      dto.Message{Content: cohereResp.Text, Role: "assistant"},
			FinishReason: stopReasonCohere2OpenAI(cohereResp.FinishReason),
		},
	}

	if info.ResponseFacts != nil {
		prompt, completion := relaycommon.NativeUsagePresence(responseBody, "meta.billed_units.input_tokens", "meta.billed_units.output_tokens", "")
		info.ResponseFacts.RecordUsage(&openaiResp.Usage, &usage, prompt, completion)
		if strings.HasPrefix(cohereResp.FinishReason, "ERROR") && cohereResp.FinishReason != "ERROR_TOXIC" {
			return &openaiResp.Usage, types.NewError(fmt.Errorf("Cohere response failed"), types.ErrorCodeBadResponse)
		}
		return helper.WriteChatResponse(c, info, resp, &openaiResp)
	}
	jsonResponse, err := common.Marshal(openaiResp)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(resp.StatusCode)
	_, _ = c.Writer.Write(jsonResponse)
	return &usage, nil
}

func cohereRerankHandler(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (*dto.Usage, *types.NewAPIError) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	service.CloseResponseBodyGracefully(resp)
	var cohereResp CohereRerankResponseResult
	err = common.Unmarshal(responseBody, &cohereResp)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	usage := dto.Usage{}
	if cohereResp.Meta.BilledUnits.InputTokens == 0 {
		usage.PromptTokens = info.GetEstimatePromptTokens()
		usage.CompletionTokens = 0
		usage.TotalTokens = info.GetEstimatePromptTokens()
	} else {
		usage.PromptTokens = cohereResp.Meta.BilledUnits.InputTokens
		usage.CompletionTokens = cohereResp.Meta.BilledUnits.OutputTokens
		usage.TotalTokens = cohereResp.Meta.BilledUnits.InputTokens + cohereResp.Meta.BilledUnits.OutputTokens
	}

	var rerankResp dto.RerankResponse
	rerankResp.Results = cohereResp.Results
	rerankResp.Usage = usage

	jsonResponse, err := common.Marshal(rerankResp)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(resp.StatusCode)
	_, err = c.Writer.Write(jsonResponse)
	return &usage, nil
}
