package tencent

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

// https://cloud.tencent.com/document/product/1729/97732

func requestOpenAI2Tencent(a *Protocol, request dto.GeneralOpenAIRequest) *TencentChatRequest {
	messages := make([]*TencentMessage, 0, len(request.Messages))
	for i := 0; i < len(request.Messages); i++ {
		message := request.Messages[i]
		messages = append(messages, &TencentMessage{
			Content: message.StringContent(),
			Role:    message.Role,
		})
	}
	var req = TencentChatRequest{
		Stream:   request.Stream,
		Messages: messages,
		Model:    &request.Model,
	}
	if request.TopP != nil {
		req.TopP = request.TopP
	}
	req.Temperature = request.Temperature
	return &req
}

func responseTencent2OpenAI(response *TencentChatResponse) *dto.OpenAITextResponse {
	fullTextResponse := dto.OpenAITextResponse{
		Id:      response.Id,
		Object:  "chat.completion",
		Created: common.GetTimestamp(),
		Usage: dto.Usage{
			PromptTokens:     response.Usage.PromptTokens,
			CompletionTokens: response.Usage.CompletionTokens,
			TotalTokens:      response.Usage.TotalTokens,
		},
	}
	if len(response.Choices) > 0 {
		choice := dto.OpenAITextResponseChoice{
			Index: 0,
			Message: dto.Message{
				Role:    "assistant",
				Content: response.Choices[0].Messages.Content,
			},
			FinishReason: response.Choices[0].FinishReason,
		}
		fullTextResponse.Choices = append(fullTextResponse.Choices, choice)
	}
	return &fullTextResponse
}

func streamResponseTencent2OpenAI(TencentResponse *TencentChatResponse) *dto.ChatCompletionsStreamResponse {
	response := dto.ChatCompletionsStreamResponse{
		Object:  "chat.completion.chunk",
		Created: common.GetTimestamp(),
		Model:   "tencent-hunyuan",
	}
	if len(TencentResponse.Choices) > 0 {
		var choice dto.ChatCompletionsStreamResponseChoice
		choice.Delta.SetContentString(TencentResponse.Choices[0].Delta.Content)
		if reason := TencentResponse.Choices[0].FinishReason; reason != "" {
			choice.FinishReason = &reason
		}
		response.Choices = append(response.Choices, choice)
	}
	return &response
}

func tencentStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	var responseText strings.Builder
	usage := &dto.Usage{}
	defer helper.EstimateMissingChatUsage(c, info, usage)
	if info.StreamStatus == nil {
		info.StreamStatus = relaycommon.NewStreamStatus()
	}
	info.StreamStatus.RequireTerminal()
	defer info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonEOF, nil)
	defer service.CloseResponseBodyGracefully(resp)
	scanner := helper.NewStreamScanner(resp.Body)
	scanner.Split(bufio.ScanLines)

	helper.SetEventStreamHeaders(c)

	for scanner.Scan() {
		data := scanner.Text()
		if len(data) < 5 || !strings.HasPrefix(data, "data:") {
			continue
		}
		data = strings.TrimPrefix(data, "data:")

		var tencentResponse TencentChatResponse
		err := common.Unmarshal([]byte(data), &tencentResponse)
		if err != nil {
			common.SysLog(common.LogText("error unmarshalling stream response: %s", err.Error()))
			continue
		}

		if info.ResponseFacts != nil {
			prompt, completion := relaycommon.NativeUsagePresence([]byte(data), "Usage.PromptTokens", "Usage.CompletionTokens", "Usage.TotalTokens")
			reported := dto.Usage{PromptTokens: tencentResponse.Usage.PromptTokens, CompletionTokens: tencentResponse.Usage.CompletionTokens}
			info.ResponseFacts.RecordUsage(usage, &reported, prompt, completion)
			if tencentResponse.Error.Code != 0 {
				return usage, types.NewError(fmt.Errorf("Tencent stream failed"), types.ErrorCodeBadResponse)
			}
		}
		response := streamResponseTencent2OpenAI(&tencentResponse)
		if len(response.Choices) != 0 {
			responseText.WriteString(response.Choices[0].Delta.GetContentString())
		}

		if err := helper.SendChatChunk(c, info, response); err != nil {
			return usage, types.NewError(err, types.ErrorCodeBadResponse)
		}
		for _, choice := range response.Choices {
			if choice.FinishReason != nil {
				info.StreamStatus.MarkCompleted()
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonDone, nil)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonScannerErr, err)
		return usage, types.NewError(err, types.ErrorCodeReadResponseBodyFailed)
	}
	helper.Done(c)
	if info.ResponseFacts == nil {
		return service.ResponseText2Usage(c, responseText.String(), info.UpstreamModelName, info.GetEstimatePromptTokens()), nil
	}
	return usage, nil
}

func tencentHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	var tencentSb TencentChatResponseSB
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	service.CloseResponseBodyGracefully(resp)
	err = common.Unmarshal(responseBody, &tencentSb)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	fullTextResponse := responseTencent2OpenAI(&tencentSb.Response)
	if info.ResponseFacts != nil {
		prompt, completion := relaycommon.NativeUsagePresence(responseBody, "Response.Usage.PromptTokens", "Response.Usage.CompletionTokens", "Response.Usage.TotalTokens")
		info.ResponseFacts.RecordUsage(&fullTextResponse.Usage, &fullTextResponse.Usage, prompt, completion)
	}
	if tencentSb.Response.Error.Code != 0 {
		return &fullTextResponse.Usage, types.WithOpenAIError(types.OpenAIError{
			Message: tencentSb.Response.Error.Message,
			Code:    tencentSb.Response.Error.Code,
		}, resp.StatusCode)
	}
	if info.ResponseFacts != nil {
		return helper.WriteChatResponse(c, info, resp, fullTextResponse)
	}
	jsonResponse, err := common.Marshal(fullTextResponse)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(resp.StatusCode)
	service.IOCopyBytesGracefully(c, resp, jsonResponse)
	return &fullTextResponse.Usage, nil
}

func parseTencentConfig(config string) (appId int64, secretId string, secretKey string, err error) {
	parts := strings.Split(config, "|")
	if len(parts) != 3 {
		err = errors.New("invalid tencent config")
		return
	}
	appId, err = strconv.ParseInt(parts[0], 10, 64)
	secretId = parts[1]
	secretKey = parts[2]
	return
}

func sha256hex(s string) string {
	b := sha256.Sum256([]byte(s))
	return hex.EncodeToString(b[:])
}

func hmacSha256(s, key string) string {
	hashed := hmac.New(sha256.New, []byte(key))
	hashed.Write([]byte(s))
	return string(hashed.Sum(nil))
}

func getTencentSign(req TencentChatRequest, adaptor *Protocol, secId, secKey string) string {
	payload, _ := common.Marshal(req)
	return (&Provider{Action: adaptor.Action, Timestamp: adaptor.Timestamp}).SignPayload(payload, secId, secKey)
}

func (adaptor *Provider) SignPayload(payload []byte, secId, secKey string) string {
	// build canonical request string
	host := "hunyuan.tencentcloudapi.com"
	httpRequestMethod := "POST"
	canonicalURI := "/"
	canonicalQueryString := ""
	canonicalHeaders := fmt.Sprintf("content-type:%s\nhost:%s\nx-tc-action:%s\n",
		"application/json", host, strings.ToLower(adaptor.Action))
	signedHeaders := "content-type;host;x-tc-action"
	hashedRequestPayload := sha256hex(string(payload))
	canonicalRequest := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s",
		httpRequestMethod,
		canonicalURI,
		canonicalQueryString,
		canonicalHeaders,
		signedHeaders,
		hashedRequestPayload)
	// build string to sign
	algorithm := "TC3-HMAC-SHA256"
	requestTimestamp := strconv.FormatInt(adaptor.Timestamp, 10)
	timestamp, _ := strconv.ParseInt(requestTimestamp, 10, 64)
	t := time.Unix(timestamp, 0).UTC()
	// must be the format 2006-01-02, ref to package time for more info
	date := t.Format("2006-01-02")
	credentialScope := fmt.Sprintf("%s/%s/tc3_request", date, "hunyuan")
	hashedCanonicalRequest := sha256hex(canonicalRequest)
	string2sign := fmt.Sprintf("%s\n%s\n%s\n%s",
		algorithm,
		requestTimestamp,
		credentialScope,
		hashedCanonicalRequest)

	// sign string
	secretDate := hmacSha256(date, "TC3"+secKey)
	secretService := hmacSha256("hunyuan", secretDate)
	secretKey := hmacSha256("tc3_request", secretService)
	signature := hex.EncodeToString([]byte(hmacSha256(string2sign, secretKey)))

	// build authorization
	authorization := fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algorithm,
		secId,
		credentialScope,
		signedHeaders,
		signature)
	return authorization
}
