package cohere

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/gin-gonic/gin"
)

type Protocol struct {
}

func (a *Protocol) ConvertGeminiRequest(*gin.Context, *relaycommon.RelayInfo, *dto.GeminiChatRequest) (any, error) {
	//TODO implement me
	return nil, errors.New("not implemented")
}

func (a *Protocol) ConvertClaudeRequest(*gin.Context, *relaycommon.RelayInfo, *dto.ClaudeRequest) (any, error) {
	//TODO implement me
	panic("implement me")
}

func (a *Protocol) ConvertAudioRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.AudioRequest) (io.Reader, error) {
	//TODO implement me
	return nil, errors.New("not implemented")
}

func (a *Protocol) ConvertImageRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.ImageRequest) (any, error) {
	//TODO implement me
	return nil, errors.New("not implemented")
}

func (a *Protocol) Init(info *relaycommon.RelayInfo) {
}

func (a *Provider) RequestURL(metadata channel.ProviderMetadata) (string, error) {
	info := metadata.ProviderRequest()
	if info.RelayMode == constant.RelayModeRerank {
		return fmt.Sprintf("%s/v1/rerank", info.ChannelBaseUrl), nil
	} else {
		return fmt.Sprintf("%s/v1/chat", info.ChannelBaseUrl), nil
	}
}

func (a *Provider) SetupHeader(c *gin.Context, req *http.Header, metadata channel.ProviderMetadata) error {
	info := metadata.ProviderRequest()
	channel.SetupProviderRequestHeader(info, c, req)
	req.Set("Authorization", fmt.Sprintf("Bearer %s", info.ApiKey))
	return nil
}

func (a *Protocol) ConvertOpenAIRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest) (any, error) {
	if info.ResponseFacts != nil && info.ResponseFacts.OnBusinessOutput != nil {
		var diagnostics []types.ConversionDiagnostic
		for _, parameter := range []struct {
			path    string
			present bool
		}{
			{"tools", len(request.Tools) > 0}, {"functions", len(request.Functions) > 0}, {"response_format", request.ResponseFormat != nil && request.ResponseFormat.Type != "text"},
			{"reasoning_effort", request.ReasoningEffort != "" && request.ReasoningEffort != "none"}, {"prompt_cache_key", request.PromptCacheKey != ""}, {"prompt_cache_options", len(request.PromptCacheOptions) > 0}, {"prompt_cache_retention", len(request.PromptCacheRetention) > 0},
			{"logprobs", request.LogProbs != nil && *request.LogProbs}, {"top_logprobs", request.TopLogProbs != nil && *request.TopLogProbs > 0}, {"audio", len(request.Audio) > 0}, {"prediction", len(request.Prediction) > 0}, {"logit_bias", len(request.LogitBias) > 0},
		} {
			if parameter.present {
				diagnostics = append(diagnostics, types.ConversionDiagnostic{Code: "request_parameter_dropped", Path: parameter.path, Message: "The native Cohere request cannot represent this parameter", Severity: types.ConversionDiagnosticError})
			}
		}
		if len(request.Modalities) > 0 {
			var modalities []string
			if err := common.Unmarshal(request.Modalities, &modalities); err != nil {
				return nil, err
			}
			for _, modality := range modalities {
				if modality != "text" {
					diagnostics = append(diagnostics, types.ConversionDiagnostic{Code: "request_parameter_dropped", Path: "modalities", Message: "The native Cohere request cannot represent this parameter", Severity: types.ConversionDiagnosticError})
					break
				}
			}
		}
		for i, message := range request.Messages {
			for _, parameter := range []struct {
				path    string
				present bool
			}{{"role", message.Role == "tool"}, {"tool_calls", len(message.ParseToolCalls()) > 0}, {"tools", len(message.Tools) > 0}} {
				if parameter.present {
					diagnostics = append(diagnostics, types.ConversionDiagnostic{Code: "request_parameter_dropped", Path: fmt.Sprintf("messages[%d].%s", i, parameter.path), Message: "The native Cohere request cannot represent this parameter", Severity: types.ConversionDiagnosticError})
				}
			}
			for j, part := range message.ParseContent() {
				if len(part.CacheControl) > 0 && string(part.CacheControl) != "null" {
					diagnostics = append(diagnostics, types.ConversionDiagnostic{Code: "request_parameter_dropped", Path: fmt.Sprintf("messages[%d].content[%d].cache_control", i, j), Message: "The native Cohere request cannot represent this parameter", Severity: types.ConversionDiagnosticError})
				}
				if part.Type != dto.ContentTypeText {
					diagnostics = append(diagnostics, types.ConversionDiagnostic{Code: "request_parameter_dropped", Path: fmt.Sprintf("messages[%d].content[%d]", i, j), Message: "The native Cohere request cannot represent this parameter", Severity: types.ConversionDiagnosticError})
				}
			}
		}
		if len(diagnostics) > 0 {
			return nil, &types.ConversionLossError{Diagnostics: diagnostics}
		}
	}
	return requestOpenAI2Cohere(*request), nil
}

func (a *Protocol) ConvertOpenAIResponsesRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.OpenAIResponsesRequest) (any, error) {
	// TODO implement me
	return nil, errors.New("not implemented")
}

func (a *Protocol) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (any, error) {
	return channel.DoApiRequest(a, c, info, requestBody)
}

func (a *Protocol) ConvertRerankRequest(c *gin.Context, relayMode int, request dto.RerankRequest) (any, error) {
	return requestConvertRerank2Cohere(request), nil
}

func (a *Protocol) ConvertEmbeddingRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.EmbeddingRequest) (any, error) {
	//TODO implement me
	return nil, errors.New("not implemented")
}

func (a *Protocol) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (usage any, err *types.NewAPIError) {
	if info.RelayMode == constant.RelayModeRerank {
		usage, err = cohereRerankHandler(c, resp, info)
	} else {
		if info.IsStream {
			usage, err = cohereStreamHandler(c, info, resp) // TODO: fix this
		} else {
			usage, err = cohereHandler(c, info, resp)
		}
	}
	return
}

func (a *Protocol) GetModelList() []string {
	return ModelList
}

func (a *Protocol) GetChannelName() string {
	return ChannelName
}
