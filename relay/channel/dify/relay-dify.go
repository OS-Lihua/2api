package dify

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"

	"github.com/MoeForge/moego/base64"
	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/samber/lo"

	"github.com/gin-gonic/gin"
)

func uploadDifyFile(c *gin.Context, info *relaycommon.RelayInfo, user string, media dto.MediaContent) (*DifyFile, error) {
	failure := errors.New("Dify file upload failed")
	if media.Type != dto.ContentTypeImageURL {
		return nil, failure
	}
	imageMedia := media.GetImageMedia()
	base64Data := imageMedia.Url
	if _, data, ok := strings.Cut(base64Data, ","); ok {
		base64Data = data
	}
	decodedData, err := base64.StdEncoding.DecodeString(base64Data)
	if err != nil {
		return nil, failure
	}
	tempFile, err := os.CreateTemp("", "dify-upload-*")
	if err != nil {
		return nil, failure
	}
	defer tempFile.Close()
	defer os.Remove(tempFile.Name())
	if _, err := tempFile.Write(decodedData); err != nil {
		return nil, failure
	}
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	if err := writer.WriteField("user", user); err != nil {
		return nil, failure
	}
	mimeType := imageMedia.MimeType
	if mimeType == "" {
		mimeType = "image/jpeg"
	}
	part, err := writer.CreateFormFile("file", fmt.Sprintf("image.%s", strings.TrimPrefix(mimeType, "image/")))
	if err != nil {
		return nil, failure
	}
	if _, err := io.Copy(part, bytes.NewReader(decodedData)); err != nil {
		return nil, failure
	}
	if err := writer.Close(); err != nil {
		return nil, failure
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, info.ChannelBaseUrl+"/v1/files/upload", body)
	if err != nil {
		return nil, failure
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+info.ApiKey)
	client := service.GetHttpClient()
	if info.BeginProviderCall != nil {
		client, err = service.GetHttpClientWithProxySettings(info.ChannelSetting.Proxy, info.ChannelSetting)
		if err != nil {
			return nil, failure
		}
		ctx, err := info.BeginProviderCall()
		if err != nil {
			return nil, failure
		}
		req = req.WithContext(ctx)
		client = channel.GatewayHTTPClient(client)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, failure
	}
	defer resp.Body.Close()
	var payload struct {
		ID   string `json:"id"`
		Code string `json:"code"`
		Type string `json:"type"`
	}
	decodeErr := common.DecodeJson(io.LimitReader(resp.Body, 1<<20), &payload)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices || payload.Code != "" {
		rejection := types.NewOpenAIError(failure, types.ErrorCodeBadResponse, resp.StatusCode)
		upstream := rejection.ToOpenAIError()
		if payload.Code != "" {
			upstream.Code = payload.Code
		}
		if payload.Type != "" {
			upstream.Type = payload.Type
		}
		rejection.RelayError = upstream
		return nil, rejection
	}
	if decodeErr != nil || payload.ID == "" {
		return nil, failure
	}
	return &DifyFile{UploadFileId: payload.ID, Type: "image", TransferMode: "local_file"}, nil
}

func requestOpenAI2Dify(c *gin.Context, info *relaycommon.RelayInfo, request dto.GeneralOpenAIRequest) *DifyChatRequest {
	difyReq := DifyChatRequest{
		Inputs:           make(map[string]any),
		AutoGenerateName: false,
	}

	difyReq.User = helper.GetResponseID(c)
	if len(request.User) > 0 {
		var user string
		if common.Unmarshal(request.User, &user) == nil && user != "" {
			difyReq.User = user
		}
	}

	files := make([]DifyFile, 0)
	var content strings.Builder
	for _, message := range request.Messages {
		if message.Role == "system" {
			content.WriteString("SYSTEM: \n" + message.StringContent() + "\n")
		} else if message.Role == "assistant" {
			content.WriteString("ASSISTANT: \n" + message.StringContent() + "\n")
		} else {
			parseContent := message.ParseContent()
			for _, mediaContent := range parseContent {
				switch mediaContent.Type {
				case dto.ContentTypeText:
					content.WriteString("USER: \n" + mediaContent.Text + "\n")
				case dto.ContentTypeImageURL:
					media := mediaContent.GetImageMedia()
					var file *DifyFile
					if media.IsRemoteImage() {
						// 修复 #2083: 远程图片分支此前未初始化 file，
						// 导致 file.Type = ... 触发 nil pointer dereference
						// 而 panic（500: "invalid memory address or nil pointer dereference"）。
						file = &DifyFile{
							Type:         "image",
							TransferMode: "remote_url",
							URL:          media.Url,
						}
					} else if info.ResponseFacts != nil {
						// Keep inline media transient until reservation and resolve only
						// the files retained by the effective Channel overrides.
						file = &DifyFile{Type: "image", TransferMode: "local_file", URL: media.Url}
					} else {
						file, _ = uploadDifyFile(c, info, difyReq.User, mediaContent)
					}
					if file != nil {
						files = append(files, *file)
					}
				}
			}
		}
	}
	difyReq.Query = content.String()
	difyReq.Files = files
	mode := "blocking"
	if lo.FromPtrOr(request.Stream, false) {
		mode = "streaming"
	}
	difyReq.ResponseMode = mode
	return &difyReq
}

func (*DifyChatRequest) PrepareChatRequest(c *gin.Context, info *relaycommon.RelayInfo, body []byte) ([]byte, error) {
	var wire DifyChatRequest
	if err := common.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("invalid Dify chat request")
	}
	changed := false
	for i, file := range wire.Files {
		media := dto.MediaContent{Type: dto.ContentTypeImageURL, ImageUrl: &dto.MessageImageUrl{Url: file.URL}}
		if file.URL == "" || media.GetImageMedia().IsRemoteImage() {
			continue
		}
		if err := c.Request.Context().Err(); err != nil {
			return nil, fmt.Errorf("Dify file upload cancelled")
		}
		uploaded, err := uploadDifyFile(c, info, wire.User, media)
		if err != nil {
			return nil, err
		}
		wire.Files[i], changed = *uploaded, true
	}
	if !changed {
		return body, nil
	}
	document := ojson.NewDoc(body)
	if err := document.Set("files", wire.Files); err != nil {
		return nil, fmt.Errorf("invalid Dify chat request")
	}
	return document.Bytes(), nil
}

func streamResponseDify2OpenAI(difyResponse DifyChunkChatCompletionResponse) *dto.ChatCompletionsStreamResponse {
	response := dto.ChatCompletionsStreamResponse{
		Object:  "chat.completion.chunk",
		Created: common.GetTimestamp(),
		Model:   "dify",
	}
	var choice dto.ChatCompletionsStreamResponseChoice
	if strings.HasPrefix(difyResponse.Event, "workflow_") {
		if constant.DifyDebug {
			text := "Workflow: " + difyResponse.Data.WorkflowId
			if difyResponse.Event == "workflow_finished" {
				text += " " + difyResponse.Data.Status
			}
			choice.Delta.SetReasoningContent(text + "\n")
		}
	} else if strings.HasPrefix(difyResponse.Event, "node_") {
		if constant.DifyDebug {
			text := "Node: " + difyResponse.Data.NodeType
			if difyResponse.Event == "node_finished" {
				text += " " + difyResponse.Data.Status
			}
			choice.Delta.SetReasoningContent(text + "\n")
		}
	} else if difyResponse.Event == "message" || difyResponse.Event == "agent_message" {
		if difyResponse.Answer == "<details style=\"color:gray;background-color: #f8f8f8;padding: 8px;border-radius: 4px;\" open> <summary> Thinking... </summary>\n" {
			difyResponse.Answer = "<think>"
		} else if difyResponse.Answer == "</details>" {
			difyResponse.Answer = "</think>"
		}

		choice.Delta.SetContentString(difyResponse.Answer)
	}
	response.Choices = append(response.Choices, choice)
	return &response
}

func recordDifyStreamUsage(info *relaycommon.RelayInfo, usage *dto.Usage, data string, reported *dto.Usage) error {
	prompt, completion := relaycommon.ChatUsagePresence([]byte(ojson.Get(data, "metadata").Raw))
	if rawUsage := ojson.Get(data, "metadata.usage"); rawUsage.IsObject() {
		previousPrompt, previousCompletion := usage.PromptTokens, usage.CompletionTokens
		if err := common.UnmarshalJsonStr(rawUsage.Raw, usage); err != nil {
			return err
		}
		usage.PromptTokens, usage.CompletionTokens = previousPrompt, previousCompletion
	}
	info.ResponseFacts.RecordUsage(usage, reported, prompt, completion)
	return nil
}

func difyStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	var responseText string
	usage := &dto.Usage{}
	var nodeToken int
	var streamError *types.NewAPIError
	helper.SetEventStreamHeaders(c)
	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		info.StreamStatus.RequireTerminal()
		var difyResponse DifyChunkChatCompletionResponse
		if err := common.Unmarshal([]byte(data), &difyResponse); err != nil {
			if info.ResponseFacts != nil {
				err = errors.New("invalid upstream stream data")
			}
			common.SysLog(common.LogText("error unmarshalling stream response: %s", err.Error()))
			sr.Error(err)
			return
		}
		if info.ResponseFacts != nil {
			if err := recordDifyStreamUsage(info, usage, data, &difyResponse.MetaData.Usage); err != nil {
				sr.Error(err)
				return
			}
		}
		if difyResponse.Event == "message_end" {
			if info.ResponseFacts == nil {
				usage = &difyResponse.MetaData.Usage
			}
			info.StreamStatus.MarkCompleted()
			sr.Done()
			return
		} else if difyResponse.Event == "error" {
			if info.ResponseFacts != nil && info.ResponseFacts.OnBusinessOutput != nil {
				status := difyResponse.Status
				if status < 400 || status > 599 {
					status = http.StatusBadGateway
				}
				info.StreamStatus.MarkFailed(difyResponse.Code, "upstream_error", status)
				streamError = types.WithOpenAIError(types.OpenAIError{Message: "Upstream stream failed", Type: "upstream_error", Code: difyResponse.Code}, status)
			}
			sr.Stop(fmt.Errorf("dify error event"))
			return
		}
		// Workflow diagnostics are metadata, not model reasoning or billable output.
		if info.ResponseFacts != nil && (strings.HasPrefix(difyResponse.Event, "workflow_") || strings.HasPrefix(difyResponse.Event, "node_")) {
			return
		}
		openaiResponse := *streamResponseDify2OpenAI(difyResponse)
		if len(openaiResponse.Choices) != 0 {
			responseText += openaiResponse.Choices[0].Delta.GetContentString()
			if openaiResponse.Choices[0].Delta.ReasoningContent != nil {
				nodeToken += 1
			}
		}
		err := helper.ObjectData(c, openaiResponse)
		if err != nil && info.ResponseFacts != nil {
			info.ResponseFacts.DownstreamWriteFailed = true
		}
		var flushErr *helper.StreamFlushError
		if info.ResponseFacts != nil && len(openaiResponse.Choices) > 0 && (err == nil || errors.As(err, &flushErr)) {
			text := openaiResponse.Choices[0].Delta.GetContentString() + openaiResponse.Choices[0].Delta.GetReasoningContent()
			if text != "" {
				info.ResponseFacts.MarkBusinessOutput()
				info.ResponseFacts.GeneratedText += text
			}
		}
		if err != nil {
			sr.Stop(fmt.Errorf("Dify stream delivery failed"))
		}
	})
	if info.ResponseFacts != nil && len(info.ResponseFacts.UnterminatedStreamData) > 0 {
		partial := info.ResponseFacts.UnterminatedStreamData
		info.ResponseFacts.UnterminatedStreamData = nil
		var reported DifyChunkChatCompletionResponse
		if common.Unmarshal(partial, &reported) == nil {
			_ = recordDifyStreamUsage(info, usage, string(partial), &reported.MetaData.Usage)
			if (reported.Event == "message" || reported.Event == "agent_message") && reported.Answer != "" {
				info.ResponseFacts.SourceGeneratedText += reported.Answer
				info.ResponseFacts.SourceOutputObserved = true
			}
		}
	}
	if info.ResponseFacts == nil || info.ResponseFacts.OnBusinessOutput == nil || info.ResponseFacts.BusinessOutput && info.StreamStatus.CompletedNormally() {
		helper.Done(c)
	}
	if info.ResponseFacts != nil {
		if !info.ResponseFacts.UsageReported {
			estimated := service.ResponseText2Usage(c, info.ResponseFacts.GeneratedText, info.TokenizerModelName(), info.GetEstimatePromptTokens())
			if !info.ResponseFacts.PromptTokensReported {
				usage.PromptTokens = estimated.PromptTokens
			}
			if !info.ResponseFacts.CompletionTokensReported {
				usage.CompletionTokens = estimated.CompletionTokens
			}
			usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		}
	} else {
		if usage.TotalTokens == 0 {
			usage = service.ResponseText2Usage(c, responseText, info.UpstreamModelName, info.GetEstimatePromptTokens())
		}
		usage.CompletionTokens += nodeToken
	}
	return usage, streamError
}

func difyHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	var difyResponse DifyChatCompletionResponse
	responseBody, err := io.ReadAll(resp.Body)

	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	service.CloseResponseBodyGracefully(resp)
	err = common.Unmarshal(responseBody, &difyResponse)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	if info.ResponseFacts != nil {
		prompt, completion := relaycommon.ChatUsagePresence([]byte(ojson.GetBytes(responseBody, "metadata").Raw))
		info.ResponseFacts.RecordUsage(&difyResponse.MetaData.Usage, &difyResponse.MetaData.Usage, prompt, completion)
	}
	fullTextResponse := dto.OpenAITextResponse{
		Id:      difyResponse.ConversationId,
		Object:  "chat.completion",
		Created: common.GetTimestamp(),
		Usage:   difyResponse.MetaData.Usage,
	}
	choice := dto.OpenAITextResponseChoice{
		Index: 0,
		Message: dto.Message{
			Role:    "assistant",
			Content: difyResponse.Answer,
		},
		FinishReason: "stop",
	}
	fullTextResponse.Choices = append(fullTextResponse.Choices, choice)
	if info.ResponseFacts != nil {
		return helper.WriteChatResponse(c, info, resp, &fullTextResponse)
	}
	jsonResponse, err := common.Marshal(fullTextResponse)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(resp.StatusCode)
	_, _ = c.Writer.Write(jsonResponse)
	return &difyResponse.MetaData.Usage, nil
}
