package coze

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/gin-gonic/gin"
)

type Protocol struct {
}

func (a *Protocol) ConvertGeminiRequest(*gin.Context, *common.RelayInfo, *dto.GeminiChatRequest) (any, error) {
	//TODO implement me
	return nil, errors.New("not implemented")
}

// ConvertAudioRequest implements channel.Adaptor.
func (a *Protocol) ConvertAudioRequest(c *gin.Context, info *common.RelayInfo, request dto.AudioRequest) (io.Reader, error) {
	return nil, errors.New("not implemented")
}

// ConvertClaudeRequest implements channel.Adaptor.
func (a *Protocol) ConvertClaudeRequest(c *gin.Context, info *common.RelayInfo, request *dto.ClaudeRequest) (any, error) {
	return nil, errors.New("not implemented")
}

// ConvertEmbeddingRequest implements channel.Adaptor.
func (a *Protocol) ConvertEmbeddingRequest(c *gin.Context, info *common.RelayInfo, request dto.EmbeddingRequest) (any, error) {
	return nil, errors.New("not implemented")
}

// ConvertImageRequest implements channel.Adaptor.
func (a *Protocol) ConvertImageRequest(c *gin.Context, info *common.RelayInfo, request dto.ImageRequest) (any, error) {
	return nil, errors.New("not implemented")
}

// ConvertOpenAIRequest implements channel.Adaptor.
func (a *Protocol) ConvertOpenAIRequest(c *gin.Context, info *common.RelayInfo, request *dto.GeneralOpenAIRequest) (any, error) {
	if request == nil {
		return nil, errors.New("request is nil")
	}
	return convertCozeChatRequest(c, *request), nil
}

// ConvertOpenAIResponsesRequest implements channel.Adaptor.
func (a *Protocol) ConvertOpenAIResponsesRequest(c *gin.Context, info *common.RelayInfo, request dto.OpenAIResponsesRequest) (any, error) {
	return nil, errors.New("not implemented")
}

// ConvertRerankRequest implements channel.Adaptor.
func (a *Protocol) ConvertRerankRequest(c *gin.Context, relayMode int, request dto.RerankRequest) (any, error) {
	return nil, errors.New("not implemented")
}

// DoRequest implements channel.Adaptor.
func (a *Protocol) DoRequest(c *gin.Context, info *common.RelayInfo, requestBody io.Reader) (any, error) {
	if info.IsStream {
		return channel.DoApiRequest(a, c, info, requestBody)
	}
	metadata := channel.NewProviderMetadata(info)
	overrides, err := channel.ResolveHeaderOverride(info, c)
	if err != nil {
		return nil, err
	}
	metadata.RequestHeaders = make(http.Header, len(overrides))
	for name, value := range overrides {
		metadata.RequestHeaders.Set(name, value)
	}
	return (&Provider{}).ExecuteRequest(c, metadata, requestBody)
}

// DoResponse implements channel.Adaptor.
func (a *Protocol) DoResponse(c *gin.Context, resp *http.Response, info *common.RelayInfo) (usage any, err *types.NewAPIError) {
	if response, ok := resp.Body.(*cozeResultBody); ok {
		c.Set("coze_token_count", response.completion.Data.Usage.TokenCount)
		c.Set("coze_output_count", response.completion.Data.Usage.OutputCount)
		c.Set("coze_input_count", response.completion.Data.Usage.InputCount)
	}
	if info.IsStream {
		usage, err = cozeChatStreamHandler(c, info, resp)
	} else {
		usage, err = cozeChatHandler(c, info, resp)
	}
	return
}

// GetChannelName implements channel.Adaptor.
func (a *Protocol) GetChannelName() string {
	return ChannelName
}

// GetModelList implements channel.Adaptor.
func (a *Protocol) GetModelList() []string {
	return ModelList
}

// GetRequestURL implements channel.Adaptor.
func (a *Provider) RequestURL(metadata channel.ProviderMetadata) (string, error) {
	info := metadata.ProviderRequest()
	return fmt.Sprintf("%s/v3/chat", info.ChannelBaseUrl), nil
}

// Init implements channel.Adaptor.
func (a *Protocol) Init(info *common.RelayInfo) {

}

// SetupRequestHeader implements channel.Adaptor.
func (a *Provider) SetupHeader(c *gin.Context, req *http.Header, metadata channel.ProviderMetadata) error {
	info := metadata.ProviderRequest()
	channel.SetupProviderRequestHeader(info, c, req)
	req.Set("Authorization", "Bearer "+info.ApiKey)
	return nil
}
