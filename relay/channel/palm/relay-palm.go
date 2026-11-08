package palm

import (
	"fmt"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

// https://developers.generativeai.google/api/rest/generativelanguage/models/generateMessage#request-body
// https://developers.generativeai.google/api/rest/generativelanguage/models/generateMessage#response-body

func responsePaLM2OpenAI(response *PaLMChatResponse) *dto.OpenAITextResponse {
	fullTextResponse := dto.OpenAITextResponse{
		Choices: make([]dto.OpenAITextResponseChoice, 0, len(response.Candidates)),
	}
	for i, candidate := range response.Candidates {
		choice := dto.OpenAITextResponseChoice{
			Index: i,
			Message: dto.Message{
				Role:    "assistant",
				Content: candidate.Content,
			},
			FinishReason: "stop",
		}
		fullTextResponse.Choices = append(fullTextResponse.Choices, choice)
	}
	return &fullTextResponse
}

func streamResponsePaLM2OpenAI(palmResponse *PaLMChatResponse) *dto.ChatCompletionsStreamResponse {
	var choice dto.ChatCompletionsStreamResponseChoice
	if len(palmResponse.Candidates) > 0 {
		choice.Delta.SetContentString(palmResponse.Candidates[0].Content)
	}
	choice.FinishReason = &constant.FinishReasonStop
	var response dto.ChatCompletionsStreamResponse
	response.Object = "chat.completion.chunk"
	response.Model = "palm2"
	response.Choices = []dto.ChatCompletionsStreamResponseChoice{choice}
	return &response
}

func palmStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*types.NewAPIError, string) {
	defer service.CloseResponseBodyGracefully(resp)
	body, readErr := io.ReadAll(resp.Body)
	var palmResponse PaLMChatResponse
	if err := common.Unmarshal(body, &palmResponse); err != nil {
		if readErr != nil {
			return types.NewError(readErr, types.ErrorCodeReadResponseBodyFailed), ""
		}
		return types.NewError(err, types.ErrorCodeBadResponseBody), ""
	}
	if readErr != nil {
		if info.ResponseFacts != nil {
			info.ResponseFacts.RecordChatSource(responsePaLM2OpenAI(&palmResponse), info.TokenizerModelName())
		}
		return types.NewError(readErr, types.ErrorCodeReadResponseBodyFailed), ""
	}
	if resp.StatusCode != http.StatusOK || palmResponse.Error.Code != 0 || len(palmResponse.Candidates) == 0 {
		return types.NewError(fmt.Errorf("PaLM response failed"), types.ErrorCodeBadResponse), ""
	}
	response := streamResponsePaLM2OpenAI(&palmResponse)
	response.Id, response.Created = helper.GetResponseID(c), common.GetTimestamp()
	if info.StreamStatus == nil {
		info.StreamStatus = relaycommon.NewStreamStatus()
	}
	helper.SetEventStreamHeaders(c)
	if err := helper.SendChatChunk(c, info, response); err != nil {
		return types.NewError(err, types.ErrorCodeBadResponse), palmResponse.Candidates[0].Content
	}
	info.StreamStatus.MarkCompleted()
	info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonDone, nil)
	helper.Done(c)
	return nil, palmResponse.Candidates[0].Content
}

func palmHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	responseBody, readErr := io.ReadAll(resp.Body)
	service.CloseResponseBodyGracefully(resp)
	var palmResponse PaLMChatResponse
	err := common.Unmarshal(responseBody, &palmResponse)
	if err != nil {
		if readErr != nil {
			return nil, types.NewOpenAIError(readErr, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
		}
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if palmResponse.Error.Code != 0 || len(palmResponse.Candidates) == 0 {
		return nil, types.WithOpenAIError(types.OpenAIError{
			Message: palmResponse.Error.Message,
			Type:    palmResponse.Error.Status,
			Param:   "",
			Code:    palmResponse.Error.Code,
		}, resp.StatusCode)
	}
	fullTextResponse := responsePaLM2OpenAI(&palmResponse)
	if readErr != nil {
		if info.ResponseFacts != nil {
			info.ResponseFacts.RecordChatSource(fullTextResponse, info.TokenizerModelName())
		}
		return &fullTextResponse.Usage, types.NewOpenAIError(readErr, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	if info.ResponseFacts != nil {
		return helper.WriteChatResponse(c, info, resp, fullTextResponse)
	}
	usage := service.ResponseText2Usage(c, palmResponse.Candidates[0].Content, info.UpstreamModelName, info.GetEstimatePromptTokens())
	fullTextResponse.Usage = *usage
	jsonResponse, err := common.Marshal(fullTextResponse)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(resp.StatusCode)
	service.IOCopyBytesGracefully(c, resp, jsonResponse)
	return usage, nil
}
