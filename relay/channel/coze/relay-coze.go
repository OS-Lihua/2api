package coze

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/samber/lo"
	"github.com/tidwall/gjson"

	"github.com/gin-gonic/gin"
)

func convertCozeChatRequest(c *gin.Context, request dto.GeneralOpenAIRequest) *CozeChatRequest {
	var messages []CozeEnterMessage
	for _, message := range request.Messages {
		if message.Role == "user" || message.Role == "assistant" {
			messageType := "question"
			if message.Role == "assistant" {
				messageType = "answer"
			}
			messages = append(messages, CozeEnterMessage{
				Role:        message.Role,
				Type:        messageType,
				Content:     message.StringContent(),
				ContentType: "text",
			})
		}
	}
	user := request.User
	if len(user) == 0 {
		user, _ = common.Marshal(helper.GetResponseID(c))
	}
	cozeRequest := &CozeChatRequest{
		BotId:              c.GetString("bot_id"),
		UserId:             user,
		AdditionalMessages: messages,
		Stream:             lo.FromPtrOr(request.Stream, false),
	}
	return cozeRequest
}

func cozeChatHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)
	var usage dto.Usage
	usage.PromptTokens = c.GetInt("coze_input_count")
	usage.CompletionTokens = c.GetInt("coze_output_count")
	usage.TotalTokens = c.GetInt("coze_token_count")
	if info.ResponseFacts != nil {
		if result, ok := channel.GatewayResponseBodySource(resp.Body).(*cozeResultBody); ok {
			info.ResponseFacts.RecordUsage(&usage, &result.usage, result.usageFacts.PromptTokensReported, result.usageFacts.CompletionTokensReported)
		} else {
			info.ResponseFacts.RecordUsage(&usage, &usage, false, false)
		}
		if result, ok := channel.GatewayResponseBodySource(resp.Body).(*cozeResultBody); ok && result.failure != nil {
			return &usage, result.failure
		}
		if resp.StatusCode != http.StatusOK {
			return &usage, types.NewErrorWithStatusCode(errors.New("Coze request failed"), types.ErrorCodeBadResponse, resp.StatusCode)
		}
	}
	responseBody, err := io.ReadAll(resp.Body)
	if info.ResponseFacts != nil {
		var source dto.OpenAITextResponse
		for _, item := range gjson.GetBytes(responseBody, "data").Array() {
			if item.Get("type").String() != "answer" {
				continue
			}
			text := relaycommon.SourceText(item.Get("content"))
			reasoning := relaycommon.SourceText(item.Get("reasoning_content"))
			message := dto.Message{Role: "assistant", Content: text}
			if reasoning != "" {
				message.ReasoningContent = &reasoning
			}
			source.Choices = append(source.Choices, dto.OpenAITextResponseChoice{Message: message})
		}
		info.ResponseFacts.RecordChatSource(&source, info.TokenizerModelName())
	}
	if err != nil {
		return &usage, types.NewError(err, types.ErrorCodeReadResponseBodyFailed)
	}
	var response dto.OpenAITextResponse
	var cozeResponse CozeChatDetailResponse
	response.Model = info.UpstreamModelName
	if err := common.Unmarshal(responseBody, &cozeResponse); err != nil {
		return &usage, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	if cozeResponse.Code != 0 {
		return &usage, types.NewError(errors.New(cozeResponse.Msg), types.ErrorCodeBadResponseBody)
	}
	response.Usage = usage
	response.Id = helper.GetResponseID(c)

	var responseContent json.RawMessage
	var responseReasoning string
	for _, data := range cozeResponse.Data {
		if data.Type == "answer" {
			responseContent = data.Content
			responseReasoning = data.ReasoningContent
			response.Created = data.CreatedAt
		}
	}
	// 添加 response.Choices
	response.Choices = []dto.OpenAITextResponseChoice{
		{
			Index:        0,
			Message:      dto.Message{Role: "assistant", Content: responseContent},
			FinishReason: "stop",
		},
	}
	if info.ResponseFacts != nil {
		if responseReasoning != "" {
			response.Choices[0].Message.ReasoningContent = &responseReasoning
		}
		if len(responseContent) > 0 {
			var text string
			if err := common.Unmarshal(responseContent, &text); err != nil {
				return &usage, types.NewError(err, types.ErrorCodeBadResponseBody)
			}
			response.Choices[0].Message.Content = text
		}
		sourceText, sourceTokens, sourceObserved := info.ResponseFacts.SourceGeneratedText, info.ResponseFacts.SourceOutputTokens, info.ResponseFacts.SourceOutputObserved
		result, failure := helper.WriteChatResponse(c, info, resp, &response)
		info.ResponseFacts.SourceGeneratedText, info.ResponseFacts.SourceOutputTokens = sourceText, sourceTokens
		info.ResponseFacts.SourceOutputObserved = info.ResponseFacts.SourceOutputObserved || sourceObserved
		return result, failure
	}
	jsonResponse, err := common.Marshal(response)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(resp.StatusCode)
	_, _ = c.Writer.Write(jsonResponse)

	return &usage, nil
}

func cozeGeneratedMessage(root gjson.Result) bool {
	if role := relaycommon.SourceText(root.Get("role")); role != "" && role != "assistant" {
		return false
	}
	switch relaycommon.SourceText(root.Get("type")) {
	case "", "answer", "function_call", "follow_up":
		return true
	default:
		return false
	}
}

func cozeChatStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)
	scanner := helper.NewStreamScanner(resp.Body)
	managed := info.ResponseFacts != nil && info.ResponseFacts.OnBusinessOutput != nil
	if managed {
		scanner = helper.NewSSEEventScanner(resp.Body, info.ResponseFacts)
	}
	helper.SetEventStreamHeaders(c)
	if info.StreamStatus == nil {
		info.StreamStatus = relaycommon.NewStreamStatus()
	}
	info.StreamStatus.RequireTerminal()
	defer info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonEOF, nil)
	usage := &dto.Usage{}
	defer helper.EstimateMissingChatUsage(c, info, usage)
	id := helper.GetResponseID(c)
	var responseText, currentEvent, currentData string
	type sourceMessage struct{ content, reasoning string }
	sourceMessages := map[string]sourceMessage{}
	observeCompletedSource := func(data []byte) {
		root := gjson.ParseBytes(data)
		if !cozeGeneratedMessage(root) {
			return
		}
		id := relaycommon.SourceText(root.Get("id"))
		previous := sourceMessages[id]
		content, reasoning := relaycommon.SourceText(root.Get("content")), relaycommon.SourceText(root.Get("reasoning_content"))
		var suffix strings.Builder
		if strings.HasPrefix(content, previous.content) {
			suffix.WriteString(content[len(previous.content):])
			previous.content = content
		}
		if strings.HasPrefix(reasoning, previous.reasoning) {
			suffix.WriteString(reasoning[len(previous.reasoning):])
			previous.reasoning = reasoning
		}
		sourceMessages[id] = previous
		helper.RecordNativeStreamSource(c, info, &dto.OpenAITextResponse{Choices: []dto.OpenAITextResponseChoice{{Message: dto.Message{Role: "assistant", Content: suffix.String()}}}})
	}
	for scanner.Scan() {
		if managed {
			event := helper.ParseSSEEvent(scanner.Text())
			if event.Event == "conversation.message.delta" {
				root := gjson.Parse(event.Data)
				if !cozeGeneratedMessage(root) {
					continue
				}
				if info.StreamStatus.CompletedNormally() {
					helper.RecordNativeStreamSource(c, info, relaycommon.NativeTextSource([]byte(event.Data), "", "content", "reasoning_content"))
					return usage, types.NewError(fmt.Errorf("Coze stream contains generation after completion"), types.ErrorCodeBadResponseBody)
				}
				id := relaycommon.SourceText(root.Get("id"))
				message := sourceMessages[id]
				message.content += relaycommon.SourceText(root.Get("content"))
				message.reasoning += relaycommon.SourceText(root.Get("reasoning_content"))
				sourceMessages[id] = message
			}
			if event.Event == "conversation.message.completed" {
				observeCompletedSource([]byte(event.Data))
			}
			if event.Event != "" && event.HasData {
				if err := handleCozeEvent(c, event.Event, event.Data, &responseText, usage, id, info); err != nil {
					return usage, types.NewError(err, types.ErrorCodeBadResponse)
				}
			}
			continue
		}
		line := scanner.Text()
		if line == "" {
			if currentEvent != "" && currentData != "" {
				if err := handleCozeEvent(c, currentEvent, currentData, &responseText, usage, id, info); err != nil {
					return usage, types.NewError(err, types.ErrorCodeBadResponse)
				}
				currentEvent, currentData = "", ""
			}
			continue
		}
		if event, ok := strings.CutPrefix(line, "event:"); ok {
			currentEvent = strings.TrimSpace(event)
		} else if data, ok := strings.CutPrefix(line, "data:"); ok {
			currentData = strings.TrimSpace(data)
		}
	}
	if managed && len(info.ResponseFacts.UnterminatedStreamData) > 0 {
		partial := info.ResponseFacts.UnterminatedStreamData
		_ = recordCozeStreamUsage(info, usage, string(partial))
		event := info.ResponseFacts.UnterminatedStreamEvent
		if event == "conversation.message.delta" && cozeGeneratedMessage(gjson.ParseBytes(partial)) {
			helper.RecordNativeStreamSource(c, info, relaycommon.NativeTextSource(partial, "", "content", "reasoning_content"))
		}
		if event == "conversation.message.completed" {
			observeCompletedSource(partial)
		}
		info.ResponseFacts.UnterminatedStreamData = nil
		info.ResponseFacts.UnterminatedStreamEvent = ""
	}
	if !managed && currentEvent != "" && currentData != "" {
		if err := handleCozeEvent(c, currentEvent, currentData, &responseText, usage, id, info); err != nil {
			return usage, types.NewError(err, types.ErrorCodeBadResponse)
		}
	}
	if err := scanner.Err(); err != nil {
		info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonScannerErr, err)
		return usage, types.NewError(err, types.ErrorCodeReadResponseBodyFailed)
	}
	helper.Done(c)
	if info.ResponseFacts == nil && usage.TotalTokens == 0 {
		*usage = *service.ResponseText2Usage(c, responseText, info.UpstreamModelName, c.GetInt("coze_input_count"))
	}
	return usage, nil
}

func recordCozeStreamUsage(info *relaycommon.RelayInfo, usage *dto.Usage, data string) error {
	if info.ResponseFacts != nil {
		info.ResponseFacts.RecordNativeUsage(usage, []byte(data), "usage.input_count", "usage.output_count")
		data = string(relaycommon.SanitizeNativeUsage([]byte(data), "usage"))
	}
	var chatData CozeChatResponseData
	if err := common.UnmarshalJsonStr(data, &chatData); err != nil {
		return err
	}
	reported := dto.Usage{PromptTokens: chatData.Usage.InputCount, CompletionTokens: chatData.Usage.OutputCount, TotalTokens: chatData.Usage.TokenCount}
	if info.ResponseFacts != nil {
		prompt, completion := relaycommon.NativeUsagePresence([]byte(data), "usage.input_count", "usage.output_count", "usage.token_count")
		info.ResponseFacts.RecordUsage(usage, &reported, prompt, completion)
	} else {
		*usage = reported
	}
	return nil
}

func handleCozeEvent(c *gin.Context, event string, data string, responseText *string, usage *dto.Usage, id string, info *relaycommon.RelayInfo) error {
	switch event {
	case "conversation.chat.completed", "conversation.chat.failed", "conversation.chat.canceled", "error":
		if err := recordCozeStreamUsage(info, usage, data); err != nil {
			return err
		}
		if event != "conversation.chat.completed" {
			return fmt.Errorf("Coze stream failed")
		}
		stopResponse := helper.GenerateStopResponse(id, common.GetTimestamp(), info.UpstreamModelName, "stop")
		if err := helper.SendChatChunk(c, info, stopResponse); err != nil {
			return err
		}
		info.StreamStatus.MarkCompleted()
		info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonDone, nil)
	case "conversation.message.delta":
		var messageData CozeChatV3MessageDetail
		if err := common.UnmarshalJsonStr(data, &messageData); err != nil {
			helper.RecordNativeStreamSource(c, info, relaycommon.NativeTextSource([]byte(data), "", "content", "reasoning_content"))
			return err
		}
		var content string
		if err := common.Unmarshal(messageData.Content, &content); err != nil {
			helper.RecordNativeStreamSource(c, info, relaycommon.NativeTextSource([]byte(data), "", "content", "reasoning_content"))
			return err
		}
		*responseText += content
		response := dto.ChatCompletionsStreamResponse{Id: id, Object: "chat.completion.chunk", Created: common.GetTimestamp(), Model: info.UpstreamModelName}
		choice := dto.ChatCompletionsStreamResponseChoice{Index: 0}
		choice.Delta.SetContentString(content)
		choice.Delta.SetReasoningContent(messageData.ReasoningContent)
		response.Choices = []dto.ChatCompletionsStreamResponseChoice{choice}
		return helper.SendChatChunk(c, info, &response)
	}
	return nil
}

func checkIfChatComplete(a *Protocol, c *gin.Context, info *relaycommon.RelayInfo) (error, bool) {
	requestURL := fmt.Sprintf("%s/v3/chat/retrieve", info.ChannelBaseUrl)

	requestURL = requestURL + "?conversation_id=" + c.GetString("coze_conversation_id") + "&chat_id=" + c.GetString("coze_chat_id")
	// 将 conversationId和chatId作为参数发送get请求
	req, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		return err, false
	}
	err = a.SetupRequestHeader(c, &req.Header, info)
	if err != nil {
		return err, false
	}

	resp, err := doRequest(req, info) // 调用 doRequest
	if err != nil {
		return err, false
	}
	if resp == nil { // 确保在 doRequest 失败时 resp 不为 nil 导致 panic
		return fmt.Errorf("resp is nil"), false
	}
	defer resp.Body.Close() // 确保响应体被关闭

	// 解析 resp 到 CozeChatResponse
	var cozeResponse CozeChatResponse
	responseBody, err := io.ReadAll(resp.Body)
	if info.ResponseFacts != nil {
		responseBody = relaycommon.SanitizeNativeUsage(responseBody, "data.usage")
	}
	if err != nil {
		return fmt.Errorf("read response body failed: %w", err), false
	}
	err = common.Unmarshal(responseBody, &cozeResponse)
	if err != nil {
		return fmt.Errorf("unmarshal response body failed: %w", err), false
	}
	if cozeResponse.Data.Status == "completed" {
		// 在上下文设置 usage
		c.Set("coze_token_count", cozeResponse.Data.Usage.TokenCount)
		c.Set("coze_output_count", cozeResponse.Data.Usage.OutputCount)
		c.Set("coze_input_count", cozeResponse.Data.Usage.InputCount)
		return nil, true
	} else if cozeResponse.Data.Status == "failed" || cozeResponse.Data.Status == "canceled" || cozeResponse.Data.Status == "requires_action" {
		return fmt.Errorf("chat status: %s", cozeResponse.Data.Status), false
	} else {
		return nil, false
	}
}

func getChatDetail(a *Protocol, c *gin.Context, info *relaycommon.RelayInfo) (*http.Response, error) {
	requestURL := fmt.Sprintf("%s/v3/chat/message/list", info.ChannelBaseUrl)

	requestURL = requestURL + "?conversation_id=" + c.GetString("coze_conversation_id") + "&chat_id=" + c.GetString("coze_chat_id")
	req, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("new request failed: %w", err)
	}
	err = a.SetupRequestHeader(c, &req.Header, info)
	if err != nil {
		return nil, fmt.Errorf("setup request header failed: %w", err)
	}
	resp, err := doRequest(req, info)
	if err != nil {
		return nil, fmt.Errorf("do request failed: %w", err)
	}
	return resp, nil
}

func doRequest(req *http.Request, info *relaycommon.RelayInfo) (*http.Response, error) {
	client, err := service.GetHttpClientWithProxySettings(info.ChannelSetting.Proxy, info.ChannelSetting)
	if err != nil {
		return nil, fmt.Errorf("new proxy http client failed: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil { // 增加对 client.Do(req) 返回错误的检查
		return nil, fmt.Errorf("client.Do failed: %w", err)
	}
	// _ = resp.Body.Close()
	return resp, nil
}
