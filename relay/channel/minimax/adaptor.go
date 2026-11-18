package minimax

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/claude"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
)

type Protocol struct {
}

func (a *Protocol) ConvertGeminiRequest(*gin.Context, *relaycommon.RelayInfo, *dto.GeminiChatRequest) (any, error) {
	return nil, errors.New("not implemented")
}

func (a *Protocol) ConvertClaudeRequest(c *gin.Context, info *relaycommon.RelayInfo, req *dto.ClaudeRequest) (any, error) {
	adaptor := claude.Adaptor{}
	return adaptor.ConvertClaudeRequest(c, info, req)
}

func (a *Protocol) ConvertAudioRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.AudioRequest) (io.Reader, error) {
	if info.RelayMode != constant.RelayModeAudioSpeech {
		return nil, errors.New("unsupported audio relay mode")
	}

	voiceID := request.Voice
	speed := lo.FromPtrOr(request.Speed, 0.0)
	outputFormat := request.ResponseFormat

	minimaxRequest := MiniMaxTTSRequest{
		Model: info.OriginModelName,
		Text:  request.Input,
		VoiceSetting: VoiceSetting{
			VoiceID: voiceID,
			Speed:   speed,
		},
		AudioSetting: &AudioSetting{
			Format: outputFormat,
		},
		OutputFormat: outputFormat,
	}

	if info.ResponseFacts != nil {
		minimaxRequest.Model = info.UpstreamModelName
		minimaxRequest.OutputFormat = "hex"
		minimaxRequest.AudioSetting.SampleRate = 24000
		minimaxRequest.AudioSetting.Channel = 1
		if minimaxRequest.AudioSetting.Format == "" {
			minimaxRequest.AudioSetting.Format = "mp3"
		}
	}

	// 同步扩展字段的厂商自定义metadata
	if len(request.Metadata) > 0 {
		var err error
		if info.ResponseFacts != nil {
			err = common.DecodeJsonStrict(bytes.NewReader(request.Metadata), &minimaxRequest)
		} else {
			err = common.Unmarshal(request.Metadata, &minimaxRequest)
		}
		if err != nil {
			return nil, fmt.Errorf("error unmarshalling metadata to minimax request: %w", err)
		}
	}

	jsonData, err := common.Marshal(minimaxRequest)
	if err != nil {
		return nil, fmt.Errorf("error marshalling minimax request: %w", err)
	}
	if outputFormat != "hex" {
		outputFormat = "url"
	}

	c.Set("response_format", outputFormat)

	// Debug: log the request structure
	// fmt.Printf("MiniMax TTS Request: %s\n", string(jsonData))

	if info.ResponseFacts != nil {
		return speechRequestBody{bytes.NewReader(jsonData)}, nil
	}
	return bytes.NewReader(jsonData), nil
}

func (a *Protocol) ConvertImageRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.ImageRequest) (any, error) {
	if info.RelayMode != constant.RelayModeImagesGenerations {
		return nil, fmt.Errorf("unsupported image relay mode: %d", info.RelayMode)
	}
	converted := oaiImage2MiniMaxImageRequest(request)
	if info.ResponseFacts != nil {
		if converted.AigcWatermark != nil {
			converted.AigcWatermark = new(*converted.AigcWatermark)
		}
		if request.Size != "" && request.Size != "auto" {
			widthText, heightText, found := strings.Cut(request.Size, "x")
			width, widthErr := strconv.Atoi(widthText)
			height, heightErr := strconv.Atoi(heightText)
			if !found || widthErr != nil || heightErr != nil {
				return nil, types.NewMediaParameterError("size", "media_format_unsupported")
			}
			if err := validateMiniMaxDimensions(width, height); err != nil {
				return nil, err
			}
			converted.Width, converted.Height, converted.AspectRatio = &width, &height, ""
		}
		if len(request.ExtraFields) > 0 {
			var extra map[string]common.RawMessage
			if err := common.Unmarshal(request.ExtraFields, &extra); err != nil {
				return nil, types.NewMediaParameterError("request", "media_control_unsupported")
			}
			parameters := map[string]any{
				"model": &converted.Model, "prompt": &converted.Prompt, "n": &converted.N,
				"width": &converted.Width, "height": &converted.Height, "seed": &converted.Seed,
				"aspect_ratio": &converted.AspectRatio, "response_format": &converted.ResponseFormat,
				"prompt_optimizer": &converted.PromptOptimizer, "aigc_watermark": &converted.AigcWatermark,
			}
			for field, raw := range extra {
				target, ok := parameters[field]
				if !ok {
					if field == "quality" {
						return nil, types.NewMediaParameterError("quality", "media_control_unsupported")
					}
					return nil, types.NewMediaParameterError("request", "media_parameters_unrepresentable")
				}
				if err := common.Unmarshal(raw, target); err != nil {
					return nil, types.NewMediaParameterError(field, "media_control_unsupported")
				}
			}
			if (extra["width"] != nil || extra["height"] != nil) && extra["aspect_ratio"] == nil && request.Extra["aspect_ratio"] == nil {
				converted.AspectRatio = ""
			}
		}
		if raw, exists := request.Extra["seed"]; exists {
			if err := common.Unmarshal(raw, &converted.Seed); err != nil {
				return nil, types.NewMediaParameterError("seed", "media_control_unsupported")
			}
		}
	}
	return converted, nil
}

func (a *Protocol) Init(info *relaycommon.RelayInfo) {
}

func (a *Provider) RequestURL(metadata channel.ProviderMetadata) (string, error) {
	info := metadata.ProviderRequest()
	return providerRequestURL(info)
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
	return request, nil
}

func (a *Protocol) ConvertRerankRequest(c *gin.Context, relayMode int, request dto.RerankRequest) (any, error) {
	return nil, nil
}

func (a *Protocol) ConvertEmbeddingRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.EmbeddingRequest) (any, error) {
	return request, nil
}

func (a *Protocol) ConvertOpenAIResponsesRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.OpenAIResponsesRequest) (any, error) {
	return nil, errors.New("not implemented")
}

func (a *Protocol) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (any, error) {
	return channel.DoApiRequest(a, c, info, requestBody)
}

func (a *Protocol) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (usage any, err *types.NewAPIError) {
	if info.RelayMode == constant.RelayModeAudioSpeech {
		return handleTTSResponse(c, resp, info)
	}
	if info.RelayMode == constant.RelayModeImagesGenerations {
		return miniMaxImageHandler(c, resp, info)
	}

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
