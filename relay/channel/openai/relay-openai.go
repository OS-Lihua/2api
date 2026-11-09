package openai

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/tokenkit"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
)

func sendStreamData(c *gin.Context, info *relaycommon.RelayInfo, data string, forceFormat bool, thinkToContent bool) (writeErr error) {
	if data == "" {
		return nil
	}
	if info.ResponseFacts != nil {
		if !info.ShouldIncludeUsage {
			var fields map[string]common.RawMessage
			if common.UnmarshalJsonStr(data, &fields) == nil && fields["usage"] != nil {
				delete(fields, "usage")
				encoded, err := common.Marshal(fields)
				if err != nil {
					return err
				}
				data = string(encoded)
			}
		}
		var frame dto.ChatCompletionsStreamResponse
		if common.UnmarshalJsonStr(data, &frame) == nil {
			business := false
			for _, choice := range frame.Choices {
				business = business || choice.Delta.GetContentString() != "" || choice.Delta.GetReasoningContent() != "" || len(choice.Delta.ToolCalls) > 0
			}
			defer func() {
				if writeErr != nil && !errors.Is(writeErr, helper.ErrGatewayStreamPreludeLimit) {
					info.ResponseFacts.DownstreamWriteFailed = true
				}
				var flushErr *helper.StreamFlushError
				if business && (writeErr == nil || errors.As(writeErr, &flushErr)) {
					info.ResponseFacts.MarkBusinessOutput()
				}
			}()
		}
	}

	if !forceFormat && !thinkToContent {
		return helper.StringData(c, data)
	}

	if thinkToContent {
		body, err := helper.ChatThinkingToContent(info, []byte(data))
		if err != nil {
			return err
		}
		return helper.StringData(c, string(body))
	}
	var lastStreamResponse dto.ChatCompletionsStreamResponse
	if err := common.UnmarshalJsonStr(data, &lastStreamResponse); err != nil {
		return err
	}
	return helper.ObjectData(c, lastStreamResponse)
}

func OaiStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		logger.LogError(c, common.LogText("invalid response or response body"))
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}

	defer service.CloseResponseBodyGracefully(resp)

	model := info.UpstreamModelName
	var responseId string
	var createAt int64 = 0
	var systemFingerprint string
	var containStreamUsage bool
	var responseTextBuilder strings.Builder
	var toolCount int
	var usage = &dto.Usage{}
	var lastStreamData string
	var secondLastStreamData string // 保留倒数第二个stream data；部分兼容网关把完整usage放在倒数第二个事件
	seenStreamToolCalls := make(map[string]struct{})
	var streamFunctionCallNames []string
	var reportedStreamUsage *dto.Usage

	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		if lastStreamData != "" {
			if err := HandleStreamFormat(c, info, lastStreamData, info.ChannelSetting.ForceFormat, info.ChannelSetting.ThinkingToContent); err != nil {
				common.SysLog(common.LogText("error handling stream format: %s", err.Error()))
				sr.Error(err)
			}
		}
		if len(data) > 0 {
			if lastStreamData != "" {
				secondLastStreamData = lastStreamData
			}

			lastStreamData = data
			if info.ResponseFacts != nil {
				prompt, completion := relaycommon.ChatUsagePresence([]byte(data))
				var frame struct {
					Usage map[string]common.RawMessage `json:"usage"`
				}
				if common.UnmarshalJsonStr(data, &frame) == nil && frame.Usage != nil {
					if !prompt {
						delete(frame.Usage, "prompt_tokens")
					}
					if !completion {
						delete(frame.Usage, "completion_tokens")
					}
					delete(frame.Usage, "total_tokens")
					for key, value := range frame.Usage {
						if strings.TrimSpace(string(value)) == "null" {
							delete(frame.Usage, key)
						}
					}
					encoded, err := common.Marshal(frame.Usage)
					if reportedStreamUsage == nil {
						reportedStreamUsage = &dto.Usage{}
					}
					if err == nil && common.Unmarshal(encoded, reportedStreamUsage) == nil {
						reportedStreamUsage.TotalTokens = reportedStreamUsage.PromptTokens + reportedStreamUsage.CompletionTokens
						info.ResponseFacts.PromptTokensReported = info.ResponseFacts.PromptTokensReported || prompt
						info.ResponseFacts.CompletionTokensReported = info.ResponseFacts.CompletionTokensReported || completion
					}
				}
			}
			if info.ResponseFacts != nil && info.ResponseFacts.OnBusinessOutput != nil {
				var envelope struct {
					Error common.RawMessage `json:"error"`
				}
				if common.UnmarshalJsonStr(data, &envelope) == nil && len(envelope.Error) > 0 && strings.TrimSpace(string(envelope.Error)) != "null" {
					var failure struct {
						Code   any    `json:"code"`
						Type   string `json:"type"`
						Status int    `json:"status"`
					}
					_ = common.Unmarshal(envelope.Error, &failure)
					info.ResponseFacts.UpstreamErrorCode, _ = failure.Code.(string)
					if info.ResponseFacts.UpstreamErrorCode == "" {
						info.ResponseFacts.UpstreamErrorCode = failure.Type
					}
					if info.ResponseFacts.UpstreamErrorCode == "" {
						info.ResponseFacts.UpstreamErrorCode = "upstream_error"
					}
					info.StreamStatus.MarkFailed(info.ResponseFacts.UpstreamErrorCode, failure.Type, failure.Status)
					lastStreamData = ""
					sr.Stop(errors.New("upstream stream failed"))
					return
				}
			}
			observeStreamChoices(info, data, seenStreamToolCalls, &streamFunctionCallNames)
			if err := processTokenData(info, data, &responseTextBuilder, &toolCount); err != nil {
				if info.ResponseFacts != nil {
					err = errors.New("invalid upstream stream data")
				}
				logger.LogError(c, common.LogText("error processing stream token data: %s", err.Error()))
				if info.ResponseFacts != nil && info.ResponseFacts.OnBusinessOutput != nil {
					lastStreamData = ""
					sr.Stop(err)
					return
				}
				sr.Error(err)
			}
			if info.PerformanceBusinessRejection && info.ResponseFacts != nil && info.ResponseFacts.OnBusinessOutput != nil {
				sr.Stop(errors.New("upstream content safety rejection"))
			}
		}
	})

	info.StreamStatus.RequireTerminal()

	// 处理最后的响应
	shouldSendLastResp := true
	if err := handleLastResponse(lastStreamData, &responseId, &createAt, &systemFingerprint, &model, &usage,
		&containStreamUsage, info, &shouldSendLastResp); err != nil {
		if info.ResponseFacts != nil && info.ResponseFacts.OnBusinessOutput != nil {
			logger.LogError(c, common.LogText("invalid upstream stream response"))
		} else {
			logger.LogError(c, common.LogText("error handling last response: %s, lastStreamData: [%s]", err.Error(), lastStreamData))
		}
	}

	// 部分兼容网关把完整的累计usage附在倒数第二个事件上，随后发送一个空的最后事件。
	// 仅当最后一个事件没有有效usage时，回退到倒数第二个事件的完整快照。
	usageFrame := lastStreamData
	if !containStreamUsage && secondLastStreamData != "" {
		var streamResp struct {
			Usage *dto.Usage `json:"usage"`
		}
		err := common.Unmarshal([]byte(secondLastStreamData), &streamResp)
		if err == nil && streamResp.Usage != nil &&
			(info.UpstreamProtocol != "" && streamResp.Usage.PromptTokens >= 0 && streamResp.Usage.CompletionTokens >= 0 && streamResp.Usage.TotalTokens >= 0 ||
				streamResp.Usage.PromptTokens > 0 && (streamResp.Usage.CompletionTokens > 0 || streamResp.Usage.TotalTokens > 0)) {
			usage = dto.MergeUsageNonZero(usage, streamResp.Usage)
			containStreamUsage = true
			usageFrame = secondLastStreamData

			if common.DebugEnabled {
				logger.LogDebug(c, "usage extracted from second last SSE: PromptTokens=%d, CompletionTokens=%d, TotalTokens=%d, InputTokens=%d, OutputTokens=%d",
					usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens,
					usage.InputTokens, usage.OutputTokens)
			}
		}
	}

	if info.RelayFormat == types.RelayFormatOpenAI {
		if shouldSendLastResp {
			_ = sendStreamData(c, info, lastStreamData, info.ChannelSetting.ForceFormat, info.ChannelSetting.ThinkingToContent)
		}
	}

	generatedUsage := service.ResponseText2Usage(c, responseTextBuilder.String(), info.TokenizerModelName(), info.GetEstimatePromptTokens())
	generatedUsage.CompletionTokens += toolCount * 7
	if info.ResponseFacts != nil {
		containStreamUsage = info.ResponseFacts.PromptTokensReported && info.ResponseFacts.CompletionTokensReported
		info.ResponseFacts.UsageReported = containStreamUsage
		info.ResponseFacts.GeneratedText = responseTextBuilder.String()
		info.ResponseFacts.GeneratedOutputTokens = &generatedUsage.CompletionTokens
		if containStreamUsage {
			usage = reportedStreamUsage
		}
	}
	if !containStreamUsage {
		usage = generatedUsage
		if info.ResponseFacts != nil && reportedStreamUsage != nil {
			snapshot := *reportedStreamUsage
			if !info.ResponseFacts.PromptTokensReported {
				snapshot.PromptTokens = usage.PromptTokens
			}
			if !info.ResponseFacts.CompletionTokensReported {
				snapshot.CompletionTokens = usage.CompletionTokens
			}
			snapshot.TotalTokens = snapshot.PromptTokens + snapshot.CompletionTokens
			usage = &snapshot
		}
	}

	applyProtocolUsagePostProcessing(c, info, usage, common.StringToByteSlice(usageFrame))

	for _, name := range streamFunctionCallNames {
		info.CountBillableToolCall(dto.BuildInCallFunctionCall, name)
	}

	if info.ResponseFacts != nil && info.ResponseFacts.OnBusinessOutput != nil && (!info.ResponseFacts.BusinessOutput || !info.StreamStatus.CompletedNormally()) {
		return usage, nil
	}
	HandleFinalResponse(c, info, lastStreamData, responseId, createAt, model, systemFingerprint, usage, containStreamUsage)

	return usage, nil
}

// observeStreamChoices collects billable function call names and records the
// finish reason facts used by health sampling from one parsed chunk.
func observeStreamChoices(info *relaycommon.RelayInfo, data string, seen map[string]struct{}, names *[]string) {
	var streamResponse dto.ChatCompletionsStreamResponse
	if err := common.UnmarshalJsonStr(data, &streamResponse); err != nil {
		return
	}
	for _, choice := range streamResponse.Choices {
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			if *choice.FinishReason == constant.FinishReasonContentFilter {
				info.PerformanceBusinessRejection = true
				if info.ResponseFacts != nil && info.ResponseFacts.OnBusinessOutput != nil {
					info.StreamStatus.MarkFailed("content_filter", "", 0)
				}
			}
			info.StreamStatus.MarkCompleted()
		}
		for i, tc := range choice.Delta.ToolCalls {
			name := strings.TrimSpace(tc.Function.Name)
			if name == "" {
				continue
			}
			toolIdx := i
			if tc.Index != nil {
				toolIdx = *tc.Index
			}
			fallbackKey := fmt.Sprintf("index\x00%d\x00%d\x00%s", choice.Index, toolIdx, name)
			activeKey := fmt.Sprintf("active\x00%d\x00%d\x00%s", choice.Index, toolIdx, name)
			callID := strings.TrimSpace(tc.ID)
			if callID != "" {
				idKey := fmt.Sprintf("id\x00%d\x00%s", choice.Index, callID)
				if _, ok := seen[idKey]; ok {
					continue
				}
				seen[idKey] = struct{}{}
				seen[activeKey] = struct{}{}
				if _, delayedID := seen[fallbackKey]; delayedID {
					delete(seen, fallbackKey)
					continue
				}
			} else {
				if _, ok := seen[fallbackKey]; ok {
					continue
				}
				if _, ok := seen[activeKey]; ok {
					continue
				}
				seen[fallbackKey] = struct{}{}
				seen[activeKey] = struct{}{}
			}
			*names = append(*names, name)
		}
	}
}

func OpenaiHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)

	var simpleResponse dto.OpenAITextResponse
	var body io.Reader = resp.Body
	if info.ResponseFacts != nil && resp.StatusCode != http.StatusOK {
		body = io.LimitReader(body, 1<<20)
	}
	responseBody, err := io.ReadAll(body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	if info.ResponseFacts == nil || info.ResponseFacts.OnBusinessOutput == nil {
		logger.LogDebug(c, "upstream response body: %s", responseBody)
	}
	// Unmarshal to simpleResponse
	if !channel.ProviderResponseWasRepaired(c) && info.ChannelType == constant.ChannelTypeOpenRouter && info.ChannelOtherSettings.IsOpenRouterEnterprise() {
		responseBody, err = (&Provider{}).unwrapEnterpriseResponse(responseBody)
		if err != nil {
			return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
		}
	}

	err = common.Unmarshal(responseBody, &simpleResponse)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}

	if info.ResponseFacts != nil {
		info.ResponseFacts.PromptTokensReported, info.ResponseFacts.CompletionTokensReported = relaycommon.ChatUsagePresence(responseBody)
		info.ResponseFacts.UsageReported = info.ResponseFacts.PromptTokensReported && info.ResponseFacts.CompletionTokensReported
	}
	var responseErr *types.NewAPIError
	if oaiError := simpleResponse.GetOpenAIError(); oaiError != nil && (oaiError.Type != "" || info.ResponseFacts != nil && info.ResponseFacts.OnBusinessOutput != nil) {
		responseErr = types.WithOpenAIError(*oaiError, resp.StatusCode)
	} else if info.ResponseFacts != nil && resp.StatusCode != http.StatusOK {
		responseErr = types.NewOpenAIError(fmt.Errorf("upstream request failed"), types.ErrorCodeBadResponse, resp.StatusCode)
	}
	if responseErr != nil {
		if info.ResponseFacts != nil && (info.ResponseFacts.PromptTokensReported || info.ResponseFacts.CompletionTokensReported) {
			applyProtocolUsagePostProcessing(c, info, &simpleResponse.Usage, responseBody)
			return &simpleResponse.Usage, responseErr
		}
		return nil, responseErr
	}

	info.ObserveResponseModel(simpleResponse.Model)
	for _, choice := range simpleResponse.Choices {
		if choice.FinishReason == constant.FinishReasonContentFilter {
			info.PerformanceBusinessRejection = true
			common.SetContextKey(c, constant.ContextKeyAdminRejectReason, "openai_finish_reason=content_filter")
			break
		}
	}

	for _, choice := range simpleResponse.Choices {
		for _, tc := range choice.Message.ParseToolCalls() {
			info.CountBillableToolCall(dto.BuildInCallFunctionCall, tc.Function.Name)
		}
	}
	info.ApplyVendorToolUsage(responseBody)

	forceFormat := false
	if info.ChannelSetting.ForceFormat {
		forceFormat = true
	}

	usageModified := false
	reportedUsage := ojson.GetBytes(responseBody, "usage").IsObject()
	if info.ResponseFacts != nil {
		reportedUsage = info.ResponseFacts.UsageReported
		info.ResponseFacts.UsageReported = reportedUsage
		var output strings.Builder
		for _, choice := range simpleResponse.Choices {
			output.WriteString(choice.Message.StringContent())
			output.WriteString(choice.Message.GetReasoningContent())
			for _, call := range choice.Message.ParseToolCalls() {
				output.WriteString(call.Function.Name)
				output.WriteString(call.Function.Arguments)
				output.Write(call.Custom)
			}
		}
		info.ResponseFacts.GeneratedText = output.String()
	}
	if (simpleResponse.Usage.PromptTokens == 0 && (info.UpstreamProtocol == "" || !reportedUsage)) || info.ResponseFacts != nil && !reportedUsage {
		completionTokens := simpleResponse.Usage.CompletionTokens
		if completionTokens == 0 && (info.ResponseFacts == nil || !info.ResponseFacts.CompletionTokensReported) {
			for _, choice := range simpleResponse.Choices {
				var output strings.Builder
				output.WriteString(choice.Message.StringContent())
				output.WriteString(choice.Message.GetReasoningContent())
				if info.ResponseFacts != nil {
					for _, call := range choice.Message.ParseToolCalls() {
						output.WriteString(call.Function.Name)
						output.WriteString(call.Function.Arguments)
						output.Write(call.Custom)
					}
				}
				completionTokens += tokenkit.Count(info.TokenizerModelName(), output.String())
			}
		}
		promptTokens := info.GetEstimatePromptTokens()
		if info.ResponseFacts != nil && info.ResponseFacts.PromptTokensReported {
			promptTokens = simpleResponse.Usage.PromptTokens
		}
		simpleResponse.Usage.PromptTokens = promptTokens
		simpleResponse.Usage.CompletionTokens = completionTokens
		simpleResponse.Usage.TotalTokens = promptTokens + completionTokens
		usageModified = true
	}

	applyProtocolUsagePostProcessing(c, info, &simpleResponse.Usage, responseBody)

	switch info.RelayFormat {
	case types.RelayFormatOpenAI:
		if usageModified {
			usageJSON, err := common.Marshal(simpleResponse.Usage)
			if err != nil {
				return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
			}
			responseBody, err = ojson.SetRawBytes(responseBody, "usage", usageJSON)
			if err != nil {
				return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
			}
		}
		if forceFormat {
			responseBody, err = common.Marshal(simpleResponse)
			if err != nil {
				return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
			}
		} else {
			break
		}
	case types.RelayFormatClaude:
		convertResult, err := service.ConvertResponse(c, info, types.RelayFormatClaude, &simpleResponse)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		claudeRespStr, err := common.Marshal(convertResult.Value)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		responseBody = claudeRespStr
	case types.RelayFormatGemini:
		convertResult, err := service.ConvertResponse(c, info, types.RelayFormatGemini, &simpleResponse)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		geminiRespStr, err := common.Marshal(convertResult.Value)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		responseBody = geminiRespStr
	}

	writeErr := service.IOCopyBytesGracefully(c, resp, responseBody)
	if info.ResponseFacts != nil {
		if writeErr != nil {
			info.ResponseFacts.DownstreamWriteFailed = true
			return &simpleResponse.Usage, types.NewError(writeErr, types.ErrorCodeBadResponse)
		}
		if info.ResponseFacts.GeneratedText != "" {
			info.ResponseFacts.MarkBusinessOutput()
		}
	}

	return &simpleResponse.Usage, nil
}
