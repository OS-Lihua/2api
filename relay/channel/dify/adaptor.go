package dify

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/gin-gonic/gin"
)

const (
	BotTypeChatFlow   = 1 // chatflow default
	BotTypeAgent      = 2
	BotTypeWorkFlow   = 3
	BotTypeCompletion = 4
)

type Protocol struct {
	BotType int
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
	//if strings.HasPrefix(info.UpstreamModelName, "agent") {
	//	a.BotType = BotTypeAgent
	//} else if strings.HasPrefix(info.UpstreamModelName, "workflow") {
	//	a.BotType = BotTypeWorkFlow
	//} else if strings.HasPrefix(info.UpstreamModelName, "chat") {
	//	a.BotType = BotTypeCompletion
	//} else {
	//}
	a.BotType = BotTypeChatFlow

}

func (a *Provider) RequestURL(metadata channel.ProviderMetadata) (string, error) {
	info := metadata.ProviderRequest()
	switch a.BotType {
	case BotTypeWorkFlow:
		return fmt.Sprintf("%s/v1/workflows/run", info.ChannelBaseUrl), nil
	case BotTypeCompletion:
		return fmt.Sprintf("%s/v1/completion-messages", info.ChannelBaseUrl), nil
	case BotTypeAgent:
		fallthrough
	default:
		return fmt.Sprintf("%s/v1/chat-messages", info.ChannelBaseUrl), nil
	}
}

func (a *Provider) SetupHeader(c *gin.Context, req *http.Header, metadata channel.ProviderMetadata) error {
	info := metadata.ProviderRequest()
	channel.SetupProviderRequestHeader(info, c, req)
	req.Set("Authorization", "Bearer "+info.ApiKey)
	return nil
}

func (a *Protocol) ConvertOpenAIRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest) (any, error) {
	if request == nil {
		return nil, errors.New("request is nil")
	}
	return requestOpenAI2Dify(c, info, *request), nil
}

func (a *Protocol) ConvertRerankRequest(c *gin.Context, relayMode int, request dto.RerankRequest) (any, error) {
	return nil, nil
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
	if info.IsStream {
		return difyStreamHandler(c, info, resp)
	} else {
		return difyHandler(c, info, resp)
	}
}

func (a *Protocol) GetModelList() []string {
	return ModelList
}

func (a *Protocol) GetChannelName() string {
	return ChannelName
}
