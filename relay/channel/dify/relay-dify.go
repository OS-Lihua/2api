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
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/samber/lo"

	"github.com/gin-gonic/gin"
)

func uploadDifyFile(c *gin.Context, info *relaycommon.RelayInfo, user string, media dto.MediaContent) *DifyFile {
	uploadUrl := fmt.Sprintf("%s/v1/files/upload", info.ChannelBaseUrl)
	switch media.Type {
	case dto.ContentTypeImageURL:
		// Decode base64 data
		imageMedia := media.GetImageMedia()
		base64Data := imageMedia.Url
		// Remove base64 prefix if exists (e.g., "data:image/jpeg;base64,")
		if idx := strings.Index(base64Data, ","); idx != -1 {
			base64Data = base64Data[idx+1:]
		}

		// Decode base64 string
		decodedData, err := base64.StdEncoding.DecodeString(base64Data)
		if err != nil {
			common.SysLog(common.LogText("failed to decode base64: %s", err.Error()))
			return nil
		}

		// Create temporary file
		tempFile, err := os.CreateTemp("", "dify-upload-*")
		if err != nil {
			common.SysLog(common.LogText("failed to create temp file: %s", err.Error()))
			return nil
		}
		defer tempFile.Close()
		defer os.Remove(tempFile.Name())

		// Write decoded data to temp file
		if _, err := tempFile.Write(decodedData); err != nil {
			common.SysLog(common.LogText("failed to write to temp file: %s", err.Error()))
			return nil
		}

		// Create multipart form
		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)

		// Add user field
		if err := writer.WriteField("user", user); err != nil {
			common.SysLog(common.LogText("failed to add user field: %s", err.Error()))
			return nil
		}

		// Create form file with proper mime type
		mimeType := imageMedia.MimeType
		if mimeType == "" {
			mimeType = "image/jpeg" // default mime type
		}

		// Create form file
		part, err := writer.CreateFormFile("file", fmt.Sprintf("image.%s", strings.TrimPrefix(mimeType, "image/")))
		if err != nil {
			common.SysLog(common.LogText("failed to create form file: %s", err.Error()))
			return nil
		}

		// Copy file content to form
		if _, err = io.Copy(part, bytes.NewReader(decodedData)); err != nil {
			common.SysLog(common.LogText("failed to copy file content: %s", err.Error()))
			return nil
		}
		writer.Close()

		// Create HTTP request
		req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, uploadUrl, body)
		if err != nil {
			common.SysLog(common.LogText("failed to create request: %s", err.Error()))
			return nil
		}

		req.Header.Set("Content-Type", writer.FormDataContentType())
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", info.ApiKey))

		// Send request
		client := service.GetHttpClient()
		resp, err := client.Do(req)
		if err != nil {
			common.SysLog(common.LogText("failed to send request: %s", err.Error()))
			return nil
		}
		defer resp.Body.Close()
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			return nil
		}

		// Parse response
		var result struct {
			Id string `json:"id"`
		}
		if err := common.DecodeJson(resp.Body, &result); err != nil {
			common.SysLog(common.LogText("failed to decode response: %s", err.Error()))
			return nil
		}

		if result.Id == "" {
			return nil
		}
		return &DifyFile{
			UploadFileId: result.Id,
			Type:         "image",
			TransferMode: "local_file",
		}
	}
	return nil
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
						file = uploadDifyFile(c, info, difyReq.User, mediaContent)
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
		uploaded := uploadDifyFile(c, info, wire.User, media)
		if uploaded == nil {
			return nil, fmt.Errorf("Dify file upload failed")
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

func difyStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	var responseText string
	usage := &dto.Usage{}
	var nodeToken int
	helper.SetEventStreamHeaders(c)
	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		info.StreamStatus.RequireTerminal()
		var difyResponse DifyChunkChatCompletionResponse
		if err := common.Unmarshal([]byte(data), &difyResponse); err != nil {
			common.SysLog(common.LogText("error unmarshalling stream response: %s", err.Error()))
			sr.Error(err)
			return
		}
		if info.ResponseFacts != nil {
			prompt, completion := relaycommon.ChatUsagePresence([]byte(ojson.Get(data, "metadata").Raw))
			if rawUsage := ojson.Get(data, "metadata.usage"); rawUsage.IsObject() {
				previousPrompt, previousCompletion := usage.PromptTokens, usage.CompletionTokens
				if err := common.UnmarshalJsonStr(rawUsage.Raw, usage); err != nil {
					sr.Error(err)
					return
				}
				usage.PromptTokens, usage.CompletionTokens = previousPrompt, previousCompletion
			}
			info.ResponseFacts.RecordUsage(usage, &difyResponse.MetaData.Usage, prompt, completion)
		}
		if difyResponse.Event == "message_end" {
			if info.ResponseFacts == nil {
				usage = &difyResponse.MetaData.Usage
			}
			info.StreamStatus.MarkCompleted()
			sr.Done()
			return
		} else if difyResponse.Event == "error" {
			sr.Stop(fmt.Errorf("dify error event"))
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
			text := openaiResponse.Choices[0].Delta.GetContentString()
			if text != "" {
				info.ResponseFacts.MarkBusinessOutput()
				info.ResponseFacts.GeneratedText += text
			}
		}
		if err != nil {
			sr.Stop(fmt.Errorf("Dify stream delivery failed"))
		}
	})
	helper.Done(c)
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
	return usage, nil
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
