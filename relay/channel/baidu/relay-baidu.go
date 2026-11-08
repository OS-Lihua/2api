package baidu

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/clock"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/samber/lo"

	"github.com/gin-gonic/gin"
)

// https://cloud.baidu.com/doc/WENXINWORKSHOP/s/flfmc9do2

var baiduTokenStore sync.Map

func requestOpenAI2Baidu(request dto.GeneralOpenAIRequest) *BaiduChatRequest {
	baiduRequest := BaiduChatRequest{
		Temperature:    request.Temperature,
		TopP:           request.TopP,
		PenaltyScore:   request.FrequencyPenalty,
		Stream:         lo.FromPtrOr(request.Stream, false),
		DisableSearch:  false,
		EnableCitation: false,
		UserId:         request.User,
	}
	if request.MaxTokens != nil || request.MaxCompletionTokens != nil {
		maxTokens := int(request.GetMaxTokens())
		baiduRequest.MaxOutputTokens = &maxTokens
	}
	var system strings.Builder
	for _, message := range request.Messages {
		if message.Role == "system" {
			if system.Len() > 0 {
				system.WriteByte('\n')
			}
			system.WriteString(message.StringContent())
		} else {
			baiduRequest.Messages = append(baiduRequest.Messages, BaiduMessage{
				Role:    message.Role,
				Content: message.StringContent(),
			})
		}
	}
	baiduRequest.System = system.String()
	return &baiduRequest
}

func responseBaidu2OpenAI(response *BaiduChatResponse) *dto.OpenAITextResponse {
	choice := dto.OpenAITextResponseChoice{
		Index: 0,
		Message: dto.Message{
			Role:    "assistant",
			Content: response.Result,
		},
		FinishReason: "stop",
	}
	if response.IsTruncated {
		choice.FinishReason = "length"
	}
	fullTextResponse := dto.OpenAITextResponse{
		Id:      response.Id,
		Object:  "chat.completion",
		Created: response.Created,
		Choices: []dto.OpenAITextResponseChoice{choice},
		Usage:   response.Usage,
	}
	return &fullTextResponse
}

func streamResponseBaidu2OpenAI(baiduResponse *BaiduChatStreamResponse) *dto.ChatCompletionsStreamResponse {
	var choice dto.ChatCompletionsStreamResponseChoice
	choice.Delta.SetContentString(baiduResponse.Result)
	if baiduResponse.IsEnd {
		choice.FinishReason = &constant.FinishReasonStop
		if baiduResponse.IsTruncated {
			choice.FinishReason = common.GetPointer("length")
		}
	}
	response := dto.ChatCompletionsStreamResponse{
		Id:      baiduResponse.Id,
		Object:  "chat.completion.chunk",
		Created: baiduResponse.Created,
		Model:   "ernie-bot",
		Choices: []dto.ChatCompletionsStreamResponseChoice{choice},
	}
	return &response
}

func embeddingRequestOpenAI2Baidu(request dto.EmbeddingRequest) *BaiduEmbeddingRequest {
	return &BaiduEmbeddingRequest{
		Input: request.ParseInput(),
	}
}

func embeddingResponseBaidu2OpenAI(response *BaiduEmbeddingResponse) *dto.OpenAIEmbeddingResponse {
	openAIEmbeddingResponse := dto.OpenAIEmbeddingResponse{
		Object: "list",
		Data:   make([]dto.OpenAIEmbeddingResponseItem, 0, len(response.Data)),
		Model:  "baidu-embedding",
		Usage:  response.Usage,
	}
	for _, item := range response.Data {
		openAIEmbeddingResponse.Data = append(openAIEmbeddingResponse.Data, dto.OpenAIEmbeddingResponseItem{
			Object:    item.Object,
			Index:     item.Index,
			Embedding: item.Embedding,
		})
	}
	return &openAIEmbeddingResponse
}

func baiduStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*types.NewAPIError, *dto.Usage) {
	usage := &dto.Usage{}
	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		if info.ResponseFacts != nil {
			info.StreamStatus.RequireTerminal()
		}
		if info.ResponseFacts != nil && info.ResponseFacts.OnBusinessOutput != nil && info.StreamStatus.CompletedNormally() {
			sr.Error(errors.New("Baidu stream contains data after completion"))
			return
		}
		if info.ResponseFacts != nil {
			info.ResponseFacts.RecordNativeUsage(usage, []byte(data), "usage.prompt_tokens", "usage.completion_tokens")
			data = string(relaycommon.SanitizeNativeUsage([]byte(data), "usage"))
		}
		var baiduResponse BaiduChatStreamResponse
		if err := common.Unmarshal([]byte(data), &baiduResponse); err != nil {
			common.SysLog(common.LogText("error unmarshalling stream response: %s", err.Error()))
			sr.Error(err)
			return
		}
		if info.ResponseFacts != nil {
			if err := info.ResponseFacts.RecordChatUsage(usage, []byte(data), "usage"); err != nil {
				sr.Error(err)
				return
			}
			if baiduResponse.ErrorMsg != "" || baiduResponse.ErrorCode != 0 {
				info.StreamStatus.MarkFailed(fmt.Sprintf("baidu:%d", baiduResponse.ErrorCode), "upstream_error", 0)
				sr.Stop(fmt.Errorf("Baidu stream failed"))
				return
			}
		} else if baiduResponse.Usage.TotalTokens != 0 {
			usage.TotalTokens = baiduResponse.Usage.TotalTokens
			usage.PromptTokens = baiduResponse.Usage.PromptTokens
			usage.CompletionTokens = baiduResponse.Usage.TotalTokens - baiduResponse.Usage.PromptTokens
		}
		response := streamResponseBaidu2OpenAI(&baiduResponse)
		if err := helper.SendChatChunk(c, info, response); err != nil {
			sr.Stop(err)
			return
		}
		if baiduResponse.IsEnd {
			info.StreamStatus.MarkCompleted()
			if info.ResponseFacts == nil || info.ResponseFacts.OnBusinessOutput == nil {
				sr.Done()
			}
		}
	})
	if info.ResponseFacts != nil && len(info.ResponseFacts.UnterminatedStreamData) > 0 {
		info.ResponseFacts.RecordNativeUsage(usage, info.ResponseFacts.UnterminatedStreamData, "usage.prompt_tokens", "usage.completion_tokens")
		partial := relaycommon.SanitizeNativeUsage(info.ResponseFacts.UnterminatedStreamData, "usage")
		info.ResponseFacts.UnterminatedStreamData = nil
		var reported BaiduChatStreamResponse
		if common.Unmarshal(partial, &reported) == nil {
			_ = info.ResponseFacts.RecordChatUsage(usage, partial, "usage")
			if reported.Result != "" {
				info.ResponseFacts.SourceGeneratedText += reported.Result
				info.ResponseFacts.SourceOutputObserved = true
			}
		}
	}
	service.CloseResponseBodyGracefully(resp)
	helper.EstimateMissingChatUsage(c, info, usage)
	return nil, usage
}

func baiduHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*types.NewAPIError, *dto.Usage) {
	var baiduResponse BaiduChatResponse
	responseBody, readErr := io.ReadAll(resp.Body)
	service.CloseResponseBodyGracefully(resp)
	var knownUsage *dto.Usage
	if info.ResponseFacts != nil {
		knownUsage = &baiduResponse.Usage
		info.ResponseFacts.RecordNativeUsage(knownUsage, responseBody, "usage.prompt_tokens", "usage.completion_tokens")
		responseBody = relaycommon.SanitizeNativeUsage(responseBody, "usage")
	}
	err := common.Unmarshal(responseBody, &baiduResponse)
	if err != nil {
		if readErr != nil {
			return types.NewError(readErr, types.ErrorCodeReadResponseBodyFailed), knownUsage
		}
		return types.NewError(err, types.ErrorCodeBadResponseBody), knownUsage
	}
	if info.ResponseFacts != nil {
		if err := info.ResponseFacts.RecordChatUsage(&baiduResponse.Usage, responseBody, "usage"); err != nil {
			return types.NewError(err, types.ErrorCodeBadResponseBody), nil
		}
	}
	if readErr != nil {
		if info.ResponseFacts != nil {
			info.ResponseFacts.RecordChatSource(responseBaidu2OpenAI(&baiduResponse))
		}
		return types.NewError(readErr, types.ErrorCodeReadResponseBodyFailed), knownUsage
	}
	if baiduResponse.ErrorMsg != "" || baiduResponse.ErrorCode != 0 {
		if info.ResponseFacts != nil {
			return types.WithOpenAIError(types.OpenAIError{Message: "Baidu request failed", Type: "upstream_error", Code: fmt.Sprintf("baidu:%d", baiduResponse.ErrorCode)}, resp.StatusCode), &baiduResponse.Usage
		}
		return types.NewError(fmt.Errorf("%s", baiduResponse.ErrorMsg), types.ErrorCodeBadResponseBody), &baiduResponse.Usage
	}
	fullTextResponse := responseBaidu2OpenAI(&baiduResponse)
	if info.ResponseFacts != nil {
		usage, responseErr := helper.WriteChatResponse(c, info, resp, fullTextResponse)
		return responseErr, usage
	}
	jsonResponse, err := common.Marshal(fullTextResponse)
	if err != nil {
		return types.NewError(err, types.ErrorCodeBadResponseBody), nil
	}
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(resp.StatusCode)
	_, err = c.Writer.Write(jsonResponse)
	return nil, &fullTextResponse.Usage
}

func baiduEmbeddingHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*types.NewAPIError, *dto.Usage) {
	var baiduResponse BaiduEmbeddingResponse
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return types.NewError(err, types.ErrorCodeBadResponseBody), nil
	}
	service.CloseResponseBodyGracefully(resp)
	err = common.Unmarshal(responseBody, &baiduResponse)
	if err != nil {
		return types.NewError(err, types.ErrorCodeBadResponseBody), nil
	}
	if baiduResponse.ErrorMsg != "" {
		return types.NewError(fmt.Errorf("%s", baiduResponse.ErrorMsg), types.ErrorCodeBadResponseBody), nil
	}
	fullTextResponse := embeddingResponseBaidu2OpenAI(&baiduResponse)
	jsonResponse, err := common.Marshal(fullTextResponse)
	if err != nil {
		return types.NewError(err, types.ErrorCodeBadResponseBody), nil
	}
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(resp.StatusCode)
	_, err = c.Writer.Write(jsonResponse)
	return nil, &fullTextResponse.Usage
}

func getBaiduAccessToken(apiKey string) (string, error) {
	if val, ok := baiduTokenStore.Load(apiKey); ok {
		var accessToken BaiduAccessToken
		if accessToken, ok = val.(BaiduAccessToken); ok {
			// soon this will expire
			if clock.Now().Add(time.Hour).After(accessToken.ExpiresAt) {
				go func() {
					_, _ = refreshBaiduAccessToken(apiKey)
				}()
			}
			return accessToken.AccessToken, nil
		}
	}
	accessToken, err := refreshBaiduAccessToken(apiKey)
	if err != nil {
		return "", err
	}
	if accessToken == nil {
		return "", errors.New("getBaiduAccessToken return a nil token")
	}
	return (*accessToken).AccessToken, nil
}

func refreshBaiduAccessToken(apiKey string) (*BaiduAccessToken, error) {
	client, err := service.GetVerifiedHttpClientWithProxy("")
	if err != nil {
		return nil, err
	}
	defer client.CloseIdleConnections()
	token, err := getBaiduAccessTokenHelper(context.Background(), client, apiKey)
	var certificateFailure *tls.CertificateVerificationError
	if errors.As(err, &certificateFailure) {
		common.SysError(common.LogText("Provider credential security failure provider_type=baidu category=certificate_verification_failed"))
	}
	return token, err
}

func getBaiduAccessTokenHelper(ctx context.Context, client *http.Client, apiKey string) (*BaiduAccessToken, error) {
	parts := strings.Split(apiKey, "|")
	if len(parts) != 2 {
		return nil, errors.New("invalid baidu apikey")
	}
	values := url.Values{"grant_type": {"client_credentials"}, "client_id": {parts[0]}, "client_secret": {parts[1]}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://aip.baidubce.com/oauth/2.0/token", strings.NewReader(values.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Add("Accept", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	var accessToken BaiduAccessToken
	err = common.DecodeJson(io.LimitReader(res.Body, 1<<20), &accessToken)
	if res.StatusCode >= 400 || accessToken.Error != "" {
		rejection := types.NewOpenAIError(errors.New("Baidu token request rejected"), types.ErrorCodeBadResponse, res.StatusCode)
		failure := rejection.ToOpenAIError()
		if accessToken.Error != "" {
			failure.Code = accessToken.Error
		}
		rejection.RelayError = failure
		return nil, rejection
	}
	if err != nil {
		return nil, errors.New("invalid Baidu token response")
	}
	if accessToken.ExpiresIn <= 0 || accessToken.ExpiresIn > (1<<63-1)/int64(time.Second) {
		return nil, errors.New("invalid Baidu token expiry")
	}
	if accessToken.AccessToken == "" {
		return nil, errors.New("getBaiduAccessTokenHelper get empty access token")
	}
	accessToken.ExpiresAt = clock.Now().Add(time.Duration(accessToken.ExpiresIn) * time.Second)
	baiduTokenStore.Store(apiKey, accessToken)
	return &accessToken, nil
}
