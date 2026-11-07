package moonshot

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	channelconstant "github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/claude"
	"github.com/QuantumNous/new-api/relay/channel/openai"
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

func (a *Protocol) ConvertClaudeRequest(c *gin.Context, info *relaycommon.RelayInfo, req *dto.ClaudeRequest) (any, error) {
	adaptor := claude.Adaptor{}
	return adaptor.ConvertClaudeRequest(c, info, req)
}

func (a *Protocol) ConvertAudioRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.AudioRequest) (io.Reader, error) {
	//TODO implement me
	return nil, errors.New("not supported")
}

func (a *Protocol) ConvertImageRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.ImageRequest) (any, error) {
	adaptor := openai.Adaptor{}
	return adaptor.ConvertImageRequest(c, info, request)
}

func (a *Protocol) Init(info *relaycommon.RelayInfo) {
}

func (a *Provider) RequestURL(metadata channel.ProviderMetadata) (string, error) {
	info := metadata.ProviderRequest()
	baseURL := info.ChannelBaseUrl
	if specialPlan, ok := channelconstant.ChannelSpecialBases[baseURL]; ok {
		if info.RelayFormat == types.RelayFormatClaude {
			return fmt.Sprintf("%s/v1/messages", specialPlan.ClaudeBaseURL), nil
		}
		if info.RelayFormat == types.RelayFormatOpenAI {
			return fmt.Sprintf("%s/chat/completions", specialPlan.OpenAIBaseURL), nil
		}
	}

	switch info.RelayFormat {
	case types.RelayFormatClaude:
		return fmt.Sprintf("%s/anthropic/v1/messages", info.ChannelBaseUrl), nil
	default:
		if info.RelayMode == constant.RelayModeRerank {
			return fmt.Sprintf("%s/v1/rerank", info.ChannelBaseUrl), nil
		} else if info.RelayMode == constant.RelayModeEmbeddings {
			return fmt.Sprintf("%s/v1/embeddings", info.ChannelBaseUrl), nil
		} else if info.RelayMode == constant.RelayModeImagesGenerations {
			return fmt.Sprintf("%s/v1/images/generations", info.ChannelBaseUrl), nil
		} else if info.RelayMode == constant.RelayModeImagesEdits {
			return fmt.Sprintf("%s/v1/images/edits", info.ChannelBaseUrl), nil
		} else if info.RelayMode == constant.RelayModeChatCompletions {
			return fmt.Sprintf("%s/v1/chat/completions", info.ChannelBaseUrl), nil
		} else if info.RelayMode == constant.RelayModeCompletions {
			return fmt.Sprintf("%s/v1/completions", info.ChannelBaseUrl), nil
		} else if info.RelayMode == constant.RelayModeResponses {
			return fmt.Sprintf("%s/v1/responses", info.ChannelBaseUrl), nil
		}
		return fmt.Sprintf("%s/v1/chat/completions", info.ChannelBaseUrl), nil
	}
}

func (a *Provider) SetupHeader(c *gin.Context, req *http.Header, metadata channel.ProviderMetadata) error {
	info := metadata.ProviderRequest()
	channel.SetupProviderRequestHeader(info, c, req)
	req.Set("Authorization", fmt.Sprintf("Bearer %s", info.ApiKey))
	return nil
}

func (a *Protocol) ConvertOpenAIRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest) (any, error) {
	return (&Provider{}).fixOpenAIRequest(c, info, request)
}

func (a *Provider) fixOpenAIRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest) (any, error) {
	if request.Temperature != nil && isTemperatureOneOnlyModel(getUpstreamModelName(info, request.Model)) && *request.Temperature != 1.0 {
		request.Temperature = common.GetPointer[float64](1.0)
	}
	return request, nil
}

func getUpstreamModelName(info *relaycommon.RelayInfo, fallback string) string {
	if info != nil && info.ProviderMeta != nil && info.UpstreamModelName != "" {
		return info.UpstreamModelName
	}
	return fallback
}

func isTemperatureOneOnlyModel(model string) bool {
	return strings.EqualFold(model, "kimi-k2.6")
}

// ConvertOpenAIResponsesRequest forwards Responses requests natively. Kimi's
// /v1/responses currently serves kimi-k3 only; other models are rejected by
// the upstream, not here.
func (a *Protocol) ConvertOpenAIResponsesRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.OpenAIResponsesRequest) (any, error) {
	return request, nil
}

func (a *Protocol) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (any, error) {
	return channel.DoApiRequest(a, c, info, requestBody)
}

func (a *Protocol) ConvertRerankRequest(c *gin.Context, relayMode int, request dto.RerankRequest) (any, error) {
	return request, nil
}

func (a *Protocol) ConvertEmbeddingRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.EmbeddingRequest) (any, error) {
	return request, nil
}

func (a *Protocol) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (usage any, err *types.NewAPIError) {
	switch info.RelayFormat {
	case types.RelayFormatClaude:
		adaptor := claude.Adaptor{}
		return adaptor.DoResponse(c, resp, info)
	default:
		adaptor := openai.Adaptor{}
		return adaptor.DoResponse(c, resp, info)
	}
}

func (a *Protocol) GetModelList() []string {
	return ModelList
}

func (a *Protocol) GetChannelName() string {
	return ChannelName
}
