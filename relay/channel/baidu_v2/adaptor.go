package baidu_v2

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/relay/channel"
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
	adaptor := openai.Adaptor{}
	return adaptor.ConvertClaudeRequest(c, info, req)
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
	switch info.RelayMode {
	case constant.RelayModeChatCompletions:
		return fmt.Sprintf("%s/v2/chat/completions", info.ChannelBaseUrl), nil
	case constant.RelayModeEmbeddings:
		return fmt.Sprintf("%s/v2/embeddings", info.ChannelBaseUrl), nil
	case constant.RelayModeImagesGenerations:
		return fmt.Sprintf("%s/v2/images/generations", info.ChannelBaseUrl), nil
	case constant.RelayModeImagesEdits:
		return fmt.Sprintf("%s/v2/images/edits", info.ChannelBaseUrl), nil
	case constant.RelayModeRerank:
		return fmt.Sprintf("%s/v2/rerank", info.ChannelBaseUrl), nil
	default:
	}
	return "", fmt.Errorf("unsupported relay mode: %d", info.RelayMode)
}

func (a *Provider) SetupHeader(c *gin.Context, req *http.Header, metadata channel.ProviderMetadata) error {
	info := metadata.ProviderRequest()
	channel.SetupProviderRequestHeader(info, c, req)
	keyParts := strings.Split(info.ApiKey, "|")
	if len(keyParts) == 0 || keyParts[0] == "" {
		return errors.New("invalid API key: authorization token is required")
	}
	if len(keyParts) > 1 {
		if keyParts[1] != "" {
			req.Set("appid", keyParts[1])
		}
	}
	req.Set("Authorization", "Bearer "+keyParts[0])
	return nil
}

func (a *Protocol) ConvertOpenAIRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest) (any, error) {
	return (&Provider{}).fixOpenAIRequest(c, info, request)
}

func (a *Provider) fixOpenAIRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest) (any, error) {
	if request == nil {
		return nil, errors.New("request is nil")
	}
	if strings.HasSuffix(info.UpstreamModelName, "-search") {
		info.UpstreamModelName = strings.TrimSuffix(info.UpstreamModelName, "-search")
		request.Model = info.UpstreamModelName
		if len(request.WebSearch) == 0 {
			toMap := request.ToMap()
			toMap["web_search"] = map[string]any{
				"enable":          true,
				"enable_citation": true,
				"enable_trace":    true,
				"enable_status":   false,
			}
			return toMap, nil
		}
		return request, nil
	}
	return request, nil
}

func (a *Protocol) ConvertRerankRequest(c *gin.Context, relayMode int, request dto.RerankRequest) (any, error) {
	return nil, errors.New("not implemented")
}

func (a *Protocol) ConvertEmbeddingRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.EmbeddingRequest) (any, error) {
	//TODO implement me
	return nil, errors.New("not implemented")
}

func (a *Protocol) ConvertOpenAIResponsesRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.OpenAIResponsesRequest) (any, error) {
	// TODO implement me
	return nil, errors.New("not implemented")
}

func (a *Protocol) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (any, error) {
	return channel.DoApiRequest(a, c, info, requestBody)
}

func (a *Protocol) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (usage any, err *types.NewAPIError) {
	adaptor := openai.Adaptor{}
	usage, err = adaptor.DoResponse(c, resp, info)
	return
}

func (a *Protocol) GetModelList() []string {
	return ModelList
}

func (a *Protocol) GetChannelName() string {
	return ChannelName
}
