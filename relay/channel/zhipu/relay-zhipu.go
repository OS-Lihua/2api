package zhipu

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// https://open.bigmodel.cn/doc/api#chatglm_std
// chatglm_std, chatglm_lite
// https://open.bigmodel.cn/api/paas/v3/model-api/chatglm_std/invoke
// https://open.bigmodel.cn/api/paas/v3/model-api/chatglm_std/sse-invoke

var zhipuTokens sync.Map
var expSeconds int64 = 24 * 3600

func getZhipuToken(apikey string) string {
	data, ok := zhipuTokens.Load(apikey)
	if ok {
		tokenData := data.(zhipuTokenData)
		if time.Now().Before(tokenData.ExpiryTime) {
			return tokenData.Token
		}
	}

	split := strings.Split(apikey, ".")
	if len(split) != 2 {
		common.SysLog(common.LogText("invalid zhipu key"))
		return ""
	}

	id := split[0]
	secret := split[1]

	expMillis := time.Now().Add(time.Duration(expSeconds)*time.Second).UnixNano() / 1e6
	expiryTime := time.Now().Add(time.Duration(expSeconds) * time.Second)

	timestamp := time.Now().UnixNano() / 1e6

	payload := jwt.MapClaims{
		"api_key":   id,
		"exp":       expMillis,
		"timestamp": timestamp,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, payload)

	token.Header["alg"] = "HS256"
	token.Header["sign_type"] = "SIGN"

	tokenString, err := token.SignedString([]byte(secret))
	if err != nil {
		return ""
	}

	zhipuTokens.Store(apikey, zhipuTokenData{
		Token:      tokenString,
		ExpiryTime: expiryTime,
	})

	return tokenString
}

func requestOpenAI2Zhipu(request dto.GeneralOpenAIRequest) *ZhipuRequest {
	messages := make([]ZhipuMessage, 0, len(request.Messages))
	for _, message := range request.Messages {
		if message.Role == "system" {
			messages = append(messages, ZhipuMessage{
				Role:    "system",
				Content: message.StringContent(),
			})
			messages = append(messages, ZhipuMessage{
				Role:    "user",
				Content: "Okay",
			})
		} else {
			messages = append(messages, ZhipuMessage{
				Role:    message.Role,
				Content: message.StringContent(),
			})
		}
	}
	return &ZhipuRequest{
		Prompt:      messages,
		Temperature: request.Temperature,
		TopP:        request.TopP,
		Incremental: false,
	}
}

func responseZhipu2OpenAI(response *ZhipuResponse) *dto.OpenAITextResponse {
	fullTextResponse := dto.OpenAITextResponse{
		Id:      response.Data.TaskId,
		Object:  "chat.completion",
		Created: common.GetTimestamp(),
		Choices: make([]dto.OpenAITextResponseChoice, 0, len(response.Data.Choices)),
		Usage:   response.Data.Usage,
	}
	for i, choice := range response.Data.Choices {
		openaiChoice := dto.OpenAITextResponseChoice{
			Index: i,
			Message: dto.Message{
				Role:    choice.Role,
				Content: strings.Trim(choice.Content, "\""),
			},
			FinishReason: "",
		}
		if i == len(response.Data.Choices)-1 {
			openaiChoice.FinishReason = "stop"
		}
		fullTextResponse.Choices = append(fullTextResponse.Choices, openaiChoice)
	}
	return &fullTextResponse
}

func streamResponseZhipu2OpenAI(zhipuResponse string) *dto.ChatCompletionsStreamResponse {
	var choice dto.ChatCompletionsStreamResponseChoice
	choice.Delta.SetContentString(zhipuResponse)
	response := dto.ChatCompletionsStreamResponse{
		Object:  "chat.completion.chunk",
		Created: common.GetTimestamp(),
		Model:   "chatglm",
		Choices: []dto.ChatCompletionsStreamResponseChoice{choice},
	}
	return &response
}

func streamMetaResponseZhipu2OpenAI(zhipuResponse *ZhipuStreamMetaResponse) (*dto.ChatCompletionsStreamResponse, *dto.Usage) {
	var choice dto.ChatCompletionsStreamResponseChoice
	choice.Delta.SetContentString("")
	choice.FinishReason = &constant.FinishReasonStop
	response := dto.ChatCompletionsStreamResponse{
		Id:      zhipuResponse.RequestId,
		Object:  "chat.completion.chunk",
		Created: common.GetTimestamp(),
		Model:   "chatglm",
		Choices: []dto.ChatCompletionsStreamResponseChoice{choice},
	}
	return &response, &zhipuResponse.Usage
}

func zhipuStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)
	usage := &dto.Usage{}
	if info.StreamStatus == nil {
		info.StreamStatus = relaycommon.NewStreamStatus()
	}
	info.StreamStatus.RequireTerminal()
	defer info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonEOF, nil)
	defer helper.EstimateMissingChatUsage(c, info, usage)
	sourceReader := &io.LimitedReader{R: resp.Body, N: math.MaxInt64}
	scanner := helper.NewStreamScanner(sourceReader)
	defer func() {
		if info.ResponseFacts == nil || info.ResponseFacts.OnBusinessOutput == nil {
			return
		}
		// Drain only bytes already received by this scanner.
		sourceReader.N = 0
		for scanner.Scan() {
			if data, ok := strings.CutPrefix(scanner.Text(), "meta:"); ok {
				info.ResponseFacts.RecordNativeUsage(usage, []byte(data), "usage.prompt_tokens", "usage.completion_tokens")
				_ = info.ResponseFacts.RecordChatUsage(usage, []byte(data), "usage")
			} else if data, ok := strings.CutPrefix(scanner.Text(), "data:"); ok {
				helper.RecordNativeStreamSource(c, info, &dto.OpenAITextResponse{Choices: []dto.OpenAITextResponseChoice{{Message: dto.Message{Role: "assistant", Content: data}}}})
			}
		}
	}()
	helper.SetEventStreamHeaders(c)
	for scanner.Scan() {
		line := scanner.Text()
		var response *dto.ChatCompletionsStreamResponse
		if data, ok := strings.CutPrefix(line, "data:"); ok {
			if info.ResponseFacts != nil && info.ResponseFacts.OnBusinessOutput != nil && info.StreamStatus.CompletedNormally() && data != "" {
				helper.RecordNativeStreamSource(c, info, &dto.OpenAITextResponse{Choices: []dto.OpenAITextResponseChoice{{Message: dto.Message{Role: "assistant", Content: data}}}})
				return usage, types.NewError(fmt.Errorf("Zhipu stream contains generation after completion"), types.ErrorCodeBadResponseBody)
			}
			response = streamResponseZhipu2OpenAI(data)
		} else if data, ok := strings.CutPrefix(line, "meta:"); ok {
			var meta ZhipuStreamMetaResponse
			if info.ResponseFacts != nil {
				info.ResponseFacts.RecordNativeUsage(usage, []byte(data), "usage.prompt_tokens", "usage.completion_tokens")
				data = string(relaycommon.SanitizeNativeUsage([]byte(data), "usage"))
				_ = info.ResponseFacts.RecordChatUsage(usage, []byte(data), "usage")
			}
			if err := common.UnmarshalJsonStr(data, &meta); err != nil {
				if info.ResponseFacts == nil {
					continue
				}
				return usage, types.NewError(err, types.ErrorCodeBadResponseBody)
			}
			if info.ResponseFacts != nil {
				prompt, completion := relaycommon.ChatUsagePresence([]byte(data))
				info.ResponseFacts.RecordUsage(usage, &meta.Usage, prompt, completion)
			} else {
				*usage = meta.Usage
			}
			if meta.TaskStatus == "FAIL" || meta.TaskStatus == "FAILED" {
				return usage, types.NewError(fmt.Errorf("Zhipu stream failed"), types.ErrorCodeBadResponse)
			}
			response, _ = streamMetaResponseZhipu2OpenAI(&meta)
			info.StreamStatus.MarkCompleted()
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonDone, nil)
		} else {
			continue
		}
		if err := helper.SendChatChunk(c, info, response); err != nil {
			return usage, types.NewError(err, types.ErrorCodeBadResponse)
		}
	}
	if err := scanner.Err(); err != nil {
		info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonScannerErr, err)
		return usage, types.NewError(err, types.ErrorCodeReadResponseBodyFailed)
	}
	helper.Done(c)
	return usage, nil
}

func zhipuHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	var zhipuResponse ZhipuResponse
	responseBody, readErr := io.ReadAll(resp.Body)
	service.CloseResponseBodyGracefully(resp)
	var knownUsage *dto.Usage
	if info.ResponseFacts != nil {
		knownUsage = &zhipuResponse.Data.Usage
		info.ResponseFacts.RecordNativeUsage(knownUsage, responseBody, "data.usage.prompt_tokens", "data.usage.completion_tokens")
		responseBody = relaycommon.SanitizeNativeUsage(responseBody, "data.usage")
		_ = info.ResponseFacts.RecordChatUsage(knownUsage, responseBody, "data.usage")
	}
	if info.ResponseFacts != nil {
		info.ResponseFacts.RecordNativeTextSource(responseBody, "data.choices", "content", info.TokenizerModelName())
	}
	err := common.Unmarshal(responseBody, &zhipuResponse)
	if err != nil {
		if readErr != nil {
			return knownUsage, types.NewError(readErr, types.ErrorCodeReadResponseBodyFailed)
		}
		return knownUsage, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if info.ResponseFacts != nil {
		prompt, completion := relaycommon.NativeUsagePresence(responseBody, "data.usage.prompt_tokens", "data.usage.completion_tokens", "data.usage.total_tokens")
		info.ResponseFacts.RecordUsage(&zhipuResponse.Data.Usage, &zhipuResponse.Data.Usage, prompt, completion)
	}
	if readErr != nil {
		if info.ResponseFacts != nil {
			info.ResponseFacts.RecordChatSource(responseZhipu2OpenAI(&zhipuResponse), info.TokenizerModelName())
		}
		return knownUsage, types.NewOpenAIError(readErr, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	if !zhipuResponse.Success {
		return &zhipuResponse.Data.Usage, types.WithOpenAIError(types.OpenAIError{
			Message: zhipuResponse.Msg,
			Code:    zhipuResponse.Code,
		}, resp.StatusCode)
	}
	fullTextResponse := responseZhipu2OpenAI(&zhipuResponse)
	if info.ResponseFacts != nil {
		return helper.WriteChatResponse(c, info, resp, fullTextResponse)
	}
	jsonResponse, err := common.Marshal(fullTextResponse)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(resp.StatusCode)
	_, err = c.Writer.Write(jsonResponse)
	return &fullTextResponse.Usage, nil
}
