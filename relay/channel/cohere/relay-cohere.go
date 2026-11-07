package cohere

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
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
		Model:       textRequest.Model,
		ChatHistory: []ChatHistory{},
		Message:     "",
		Stream:      lo.FromPtrOr(textRequest.Stream, false),
		MaxTokens:   textRequest.GetMaxTokens(),
	}
	if common.CohereSafetySetting != "NONE" {
		cohereReq.SafetyMode = common.CohereSafetySetting
	}
	if cohereReq.MaxTokens == 0 {
		cohereReq.MaxTokens = 4000
	}
	for _, msg := range textRequest.Messages {
		if msg.Role == "user" {
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
	case "MAX_TOKENS":
		return "max_tokens"
	default:
		return reason
	}
}

func cohereStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)
	responseId := helper.GetResponseID(c)
	createdTime := common.GetTimestamp()
	usage := &dto.Usage{}
	var responseText strings.Builder
	if info.StreamStatus == nil {
		info.StreamStatus = relaycommon.NewStreamStatus()
	}
	info.StreamStatus.RequireTerminal()
	defer info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonEOF, nil)
	defer helper.EstimateMissingChatUsage(c, info, usage)
	scanner := helper.NewStreamScanner(resp.Body)
	helper.SetEventStreamHeaders(c)
	for scanner.Scan() {
		info.SetFirstResponseTime()
		var cohereResp CohereResponse
		if err := common.Unmarshal(scanner.Bytes(), &cohereResp); err != nil {
			if info.ResponseFacts == nil {
				continue
			}
			return usage, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		if cohereResp.Response != nil {
			reported := dto.Usage{PromptTokens: cohereResp.Response.Meta.BilledUnits.InputTokens, CompletionTokens: cohereResp.Response.Meta.BilledUnits.OutputTokens}
			if info.ResponseFacts != nil {
				prompt, completion := relaycommon.NativeUsagePresence(scanner.Bytes(), "response.meta.billed_units.input_tokens", "response.meta.billed_units.output_tokens", "")
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
			break
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
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	service.CloseResponseBodyGracefully(resp)
	var cohereResp CohereResponseResult
	err = common.Unmarshal(responseBody, &cohereResp)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	usage := dto.Usage{}
	usage.PromptTokens = cohereResp.Meta.BilledUnits.InputTokens
	usage.CompletionTokens = cohereResp.Meta.BilledUnits.OutputTokens
	usage.TotalTokens = cohereResp.Meta.BilledUnits.InputTokens + cohereResp.Meta.BilledUnits.OutputTokens

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
