package common_handler

import (
	"bytes"
	"fmt"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/tidwall/gjson"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/relay/channel/xinference"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

func RerankHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if info.ResponseFacts != nil {
		return gatewayRerankResponse(c, info, resp)
	}
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	service.CloseResponseBodyGracefully(resp)
	logger.LogDebug(c, "reranker response body: %s", responseBody)
	var jinaResp dto.RerankResponse
	if info.ChannelType == constant.ChannelTypeXinference {
		var xinRerankResponse xinference.XinRerankResponse
		err = common.Unmarshal(responseBody, &xinRerankResponse)
		if err != nil {
			return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
		}
		jinaRespResults := make([]dto.RerankResponseResult, len(xinRerankResponse.Results))
		for i, result := range xinRerankResponse.Results {
			respResult := dto.RerankResponseResult{
				Index:          result.Index,
				RelevanceScore: result.RelevanceScore,
			}
			if info.ReturnDocuments {
				var document any
				if result.Document != nil {
					if doc, ok := result.Document.(string); ok {
						if doc == "" {
							document = info.Documents[result.Index]
						} else {
							document = doc
						}
					} else {
						document = result.Document
					}
				}
				respResult.Document = document
			}
			jinaRespResults[i] = respResult
		}
		jinaResp = dto.RerankResponse{
			Results: jinaRespResults,
			Usage: dto.Usage{
				PromptTokens: info.GetEstimatePromptTokens(),
				TotalTokens:  info.GetEstimatePromptTokens(),
			},
		}
	} else {
		err = common.Unmarshal(responseBody, &jinaResp)
		if err != nil {
			return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
		}
		jinaResp.Usage.PromptTokens = jinaResp.Usage.TotalTokens
	}

	c.Writer.Header().Set("Content-Type", "application/json")
	c.JSON(http.StatusOK, jinaResp)
	return &jinaResp.Usage, nil
}

// NonTextResponseError retains classification facts without forwarding or
// logging the Provider's message or arbitrary error details.
func NonTextResponseError(info *relaycommon.RelayInfo, body []byte, status int) *types.NewAPIError {
	var envelope struct {
		Error common.RawMessage `json:"error"`
	}
	if common.Unmarshal(body, &envelope) == nil && len(envelope.Error) != 0 && !bytes.Equal(bytes.TrimSpace(envelope.Error), []byte("null")) {
		var detail struct {
			Code common.RawMessage `json:"code"`
			Type string            `json:"type"`
		}
		_ = common.Unmarshal(envelope.Error, &detail)
		code, kind := "", detail.Type
		_ = common.Unmarshal(detail.Code, &code)
		if code == "" {
			var nativeStatus int
			if common.Unmarshal(detail.Code, &nativeStatus) == nil && nativeStatus >= 400 && nativeStatus <= 599 {
				status = nativeStatus
			}
		}
		if kind == "" {
			kind = "upstream_error"
		}
		if info.StreamStatus == nil {
			info.StreamStatus = relaycommon.NewStreamStatus()
		}
		info.StreamStatus.MarkFailed(code, kind, status)
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		return types.WithOpenAIError(types.OpenAIError{Message: "Upstream request failed", Type: kind, Code: code}, status)
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return types.NewOpenAIError(fmt.Errorf("upstream request failed"), types.ErrorCodeBadResponse, status)
	}
	return nil
}

func gatewayRerankResponse(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)
	body, readErr := io.ReadAll(resp.Body)
	usage := &dto.Usage{}
	promptPath := "usage.prompt_tokens"
	prompt, _ := relaycommon.NativeUsagePresence(body, promptPath, "", "")
	if !prompt {
		promptPath = "usage.total_tokens"
		prompt, _ = relaycommon.NativeUsagePresence(body, promptPath, "", "")
	}
	if prompt {
		usage.PromptTokens = int(gjson.GetBytes(body, promptPath).Int())
		usage.TotalTokens = usage.PromptTokens
	}
	facts := info.ResponseFacts
	facts.PromptTokensReported, facts.CompletionTokensReported = prompt, true
	facts.UsageReported, facts.UsageEstimated = prompt, !prompt
	facts.RecordTokenDetailPresence(body, "usage")
	if fault := NonTextResponseError(info, body, resp.StatusCode); fault != nil {
		return usage, fault
	}
	if readErr != nil {
		return usage, types.NewError(fmt.Errorf("incomplete rerank response"), types.ErrorCodeReadResponseBodyFailed)
	}
	var wire struct {
		Results []struct {
			Index          *int     `json:"index"`
			RelevanceScore *float64 `json:"relevance_score"`
			Document       any      `json:"document"`
		} `json:"results"`
	}
	if common.Unmarshal(body, &wire) != nil {
		return usage, types.NewError(fmt.Errorf("invalid rerank response"), types.ErrorCodeBadResponseBody)
	}
	if len(wire.Results) == 0 {
		return usage, types.NewError(fmt.Errorf("empty rerank response"), types.ErrorCodeEmptyResponse)
	}
	result := dto.RerankResponse{Results: make([]dto.RerankResponseResult, 0, len(wire.Results))}
	seen := make(map[int]bool, len(wire.Results))
	for _, item := range wire.Results {
		if item.Index == nil || *item.Index < 0 || len(info.Documents) > 0 && *item.Index >= len(info.Documents) || seen[*item.Index] || item.RelevanceScore == nil {
			return usage, types.NewError(fmt.Errorf("invalid rerank result"), types.ErrorCodeBadResponseBody)
		}
		seen[*item.Index] = true
		if info.ChannelType == constant.ChannelTypeXinference && info.ReturnDocuments {
			if document, ok := item.Document.(string); ok && document == "" && *item.Index < len(info.Documents) {
				item.Document = info.Documents[*item.Index]
			}
		}
		result.Results = append(result.Results, dto.RerankResponseResult{Index: *item.Index, RelevanceScore: *item.RelevanceScore, Document: item.Document})
	}
	if !prompt {
		usage.PromptTokens = info.GetEstimatePromptTokens()
		usage.TotalTokens = usage.PromptTokens
	}
	zero := 0
	facts.SourceOutputObserved, facts.SourceOutputTokens = true, &zero
	result.Usage = *usage
	body, err := common.Marshal(result)
	if err != nil {
		return usage, types.NewError(fmt.Errorf("invalid rerank response"), types.ErrorCodeBadResponseBody)
	}
	if err := helper.WriteGatewayJSONResponse(c, resp, facts, body, false); err != nil {
		facts.DownstreamWriteFailed = true
		return usage, types.NewError(fmt.Errorf("rerank delivery failed"), types.ErrorCodeBadResponse)
	}
	facts.MarkBusinessOutput()
	return usage, nil
}
