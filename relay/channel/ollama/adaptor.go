package ollama

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/claude"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/gin-gonic/gin"
)

type Protocol struct {
}

func (a *Protocol) ConvertGeminiRequest(*gin.Context, *relaycommon.RelayInfo, *dto.GeminiChatRequest) (any, error) {
	return nil, errors.New("not implemented")
}

func (a *Protocol) ConvertClaudeRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.ClaudeRequest) (any, error) {
	adaptor := claude.Adaptor{}
	return adaptor.ConvertClaudeRequest(c, info, request)
}

func (a *Protocol) ConvertAudioRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.AudioRequest) (io.Reader, error) {
	return nil, errors.New("not implemented")
}

func (a *Protocol) ConvertImageRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.ImageRequest) (any, error) {
	return nil, errors.New("not implemented")
}

func (a *Protocol) Init(info *relaycommon.RelayInfo) {
	// OpenAI-compatible chat responses are handled by the OpenAI adaptor, which
	// relies on the thinking-to-content state initialized here.
	(&openai.Adaptor{}).Init(info)
}

func (a *Provider) RequestURL(metadata channel.ProviderMetadata) (string, error) {
	info := metadata.ProviderRequest()
	switch info.RelayFormat {
	case types.RelayFormatClaude:
		return (&claude.Provider{}).RequestURL(info)
	default:
		switch info.RelayMode {
		case relayconstant.RelayModeEmbeddings:
			return fmt.Sprintf("%s/api/embed", info.ChannelBaseUrl), nil
		case relayconstant.RelayModeResponses:
			return fmt.Sprintf("%s/v1/responses", info.ChannelBaseUrl), nil
		case relayconstant.RelayModeResponsesCompact:
			return fmt.Sprintf("%s/v1/responses/compact", info.ChannelBaseUrl), nil
		case relayconstant.RelayModeCompletions:
			return fmt.Sprintf("%s/api/generate", info.ChannelBaseUrl), nil
		default:
			if info.ChannelOtherSettings.OllamaOpenAIChat {
				return fmt.Sprintf("%s/v1/chat/completions", info.ChannelBaseUrl), nil
			}
			return fmt.Sprintf("%s/api/chat", info.ChannelBaseUrl), nil
		}
	}
}

func (a *Provider) SetupHeader(c *gin.Context, req *http.Header, metadata channel.ProviderMetadata) error {
	info := metadata.ProviderRequest()
	channel.SetupProviderRequestHeader(info, c, req)
	req.Set("Authorization", "Bearer "+info.ApiKey)
	switch info.RelayFormat {
	case types.RelayFormatClaude:
		claude.CommonProviderClaudeHeadersOperation(c, req, info)
		anthropicVersion := channel.ProviderHeader(c, "anthropic-version")
		if anthropicVersion == "" {
			anthropicVersion = "2023-06-01"
		}
		req.Set("anthropic-version", anthropicVersion)
	}
	return nil
}

func (a *Protocol) ConvertOpenAIRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest) (any, error) {
	if request == nil {
		return nil, errors.New("request is nil")
	}
	switch info.RelayMode {
	case relayconstant.RelayModeCompletions:
		return openAIToGenerate(c, request)
	default:
		if info.ChannelOtherSettings.OllamaOpenAIChat {
			return (&openai.Adaptor{}).ConvertOpenAIRequest(c, info, request)
		}
		return openAIChatToOllamaChat(c, request)
	}
}

func (a *Protocol) ConvertRerankRequest(c *gin.Context, relayMode int, request dto.RerankRequest) (any, error) {
	return nil, nil
}

func (a *Protocol) ConvertEmbeddingRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.EmbeddingRequest) (any, error) {
	return requestOpenAI2Embeddings(request), nil
}

func (a *Protocol) ConvertOpenAIResponsesRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.OpenAIResponsesRequest) (any, error) {
	adaptor := openai.Adaptor{}
	return adaptor.ConvertOpenAIResponsesRequest(c, info, request)
}

func (a *Protocol) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (any, error) {
	return channel.DoApiRequest(a, c, info, requestBody)
}

func (a *Protocol) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (usage any, err *types.NewAPIError) {
	switch info.RelayFormat {
	case types.RelayFormatClaude:
		adaptor := claude.Adaptor{}
		return adaptor.DoResponse(c, resp, info)
	default:
		switch info.RelayMode {
		case relayconstant.RelayModeEmbeddings:
			return ollamaEmbeddingHandler(c, info, resp)
		case relayconstant.RelayModeResponses, relayconstant.RelayModeResponsesCompact:
			adaptor := openai.Adaptor{}
			return adaptor.DoResponse(c, resp, info)
		default:
			// /api/generate always returns native NDJSON; chat follows the channel setting.
			if info.RelayMode != relayconstant.RelayModeCompletions && info.ChannelOtherSettings.OllamaOpenAIChat {
				adaptor := openai.Adaptor{}
				return adaptor.DoResponse(c, resp, info)
			}
			if info.IsStream {
				return ollamaStreamHandler(c, info, resp)
			}
			return ollamaChatHandler(c, info, resp)
		}
	}
}

func (a *Protocol) GetModelList() []string {
	return ModelList
}

func (a *Protocol) GetChannelName() string {
	return ChannelName
}
