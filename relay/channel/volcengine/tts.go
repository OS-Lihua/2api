package volcengine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/MoeForge/moego/base64"
	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/shopspring/decimal"
)

type VolcengineTTSRequest struct {
	App     VolcengineTTSApp     `json:"app"`
	User    VolcengineTTSUser    `json:"user"`
	Audio   VolcengineTTSAudio   `json:"audio"`
	Request VolcengineTTSReqInfo `json:"request"`
}

type VolcengineTTSApp struct {
	AppID   string `json:"appid"`
	Token   string `json:"token"`
	Cluster string `json:"cluster"`
}

type VolcengineTTSUser struct {
	UID string `json:"uid"`
}

type VolcengineTTSAudio struct {
	VoiceType        string  `json:"voice_type"`
	Encoding         string  `json:"encoding"`
	SpeedRatio       float64 `json:"speed_ratio"`
	Rate             int     `json:"rate"`
	Bitrate          int     `json:"bitrate,omitempty"`
	LoudnessRatio    float64 `json:"loudness_ratio,omitempty"`
	EnableEmotion    bool    `json:"enable_emotion,omitempty"`
	Emotion          string  `json:"emotion,omitempty"`
	EmotionScale     float64 `json:"emotion_scale,omitempty"`
	ExplicitLanguage string  `json:"explicit_language,omitempty"`
	ContextLanguage  string  `json:"context_language,omitempty"`
}

type VolcengineTTSReqInfo struct {
	ReqID                      string                   `json:"reqid"`
	Text                       string                   `json:"text"`
	Operation                  string                   `json:"operation"`
	Model                      string                   `json:"model,omitempty"`
	TextType                   string                   `json:"text_type,omitempty"`
	EnableTrailingSilenceAudio *bool                    `json:"enable_trailing_silence_audio,omitempty"`
	SilenceDuration            *float64                 `json:"silence_duration,omitempty"`
	WithTimestamp              any                      `json:"with_timestamp,omitempty"`
	ExtraParam                 *VolcengineTTSExtraParam `json:"extra_param,omitempty"`
}

type VolcengineTTSExtraParam struct {
	DisableMarkdownFilter      bool                      `json:"disable_markdown_filter,omitempty"`
	EnableLatexTn              bool                      `json:"enable_latex_tn,omitempty"`
	MuteCutThreshold           string                    `json:"mute_cut_threshold,omitempty"`
	MuteCutRemainMs            string                    `json:"mute_cut_remain_ms,omitempty"`
	DisableEmojiFilter         bool                      `json:"disable_emoji_filter,omitempty"`
	UnsupportedCharRatioThresh float64                   `json:"unsupported_char_ratio_thresh,omitempty"`
	AigcWatermark              bool                      `json:"aigc_watermark,omitempty"`
	CacheConfig                *VolcengineTTSCacheConfig `json:"cache_config,omitempty"`
}

type VolcengineTTSCacheConfig struct {
	TextType int  `json:"text_type,omitempty"`
	UseCache bool `json:"use_cache,omitempty"`
}

type VolcengineTTSResponse struct {
	ReqID    string                     `json:"reqid"`
	Code     int                        `json:"code"`
	Message  string                     `json:"message"`
	Sequence int                        `json:"sequence"`
	Data     string                     `json:"data"`
	Addition *VolcengineTTSAdditionInfo `json:"addition,omitempty"`
}

type VolcengineTTSAdditionInfo struct {
	Duration string `json:"duration"`
}

var openAIToVolcengineVoiceMap = map[string]string{
	"alloy":   "zh_male_M392_conversation_wvae_bigtts",
	"echo":    "zh_male_wenhao_mars_bigtts",
	"fable":   "zh_female_tianmei_mars_bigtts",
	"onyx":    "zh_male_zhibei_mars_bigtts",
	"nova":    "zh_female_shuangkuaisisi_mars_bigtts",
	"shimmer": "zh_female_cancan_mars_bigtts",
}

var responseFormatToEncodingMap = map[string]string{
	"mp3":  "mp3",
	"opus": "ogg_opus",
	"aac":  "mp3",
	"flac": "mp3",
	"wav":  "wav",
	"pcm":  "pcm",
}

func parseVolcengineAuth(apiKey string) (appID, token string, err error) {
	parts := strings.Split(apiKey, "|")
	if len(parts) != 2 {
		return "", "", errors.New("invalid api key format, expected: appid|access_token")
	}
	return parts[0], parts[1], nil
}

func mapVoiceType(openAIVoice string) string {
	if voice, ok := openAIToVolcengineVoiceMap[openAIVoice]; ok {
		return voice
	}
	return openAIVoice
}

func mapEncoding(responseFormat string) string {
	if encoding, ok := responseFormatToEncodingMap[responseFormat]; ok {
		return encoding
	}
	return "mp3"
}

func getContentTypeByEncoding(encoding string) string {
	contentTypeMap := map[string]string{
		"mp3":      "audio/mpeg",
		"ogg_opus": "audio/ogg",
		"wav":      "audio/wav",
		"pcm":      "audio/pcm",
	}
	if ct, ok := contentTypeMap[encoding]; ok {
		return ct
	}
	return "application/octet-stream"
}

func speechError(code int) *types.NewAPIError {
	status := http.StatusBadGateway
	switch code {
	case 3001, 3006, 3010, 3011, 3050:
		status = http.StatusBadRequest
	case 3003:
		status = http.StatusTooManyRequests
	case 3005, 3031, 3040:
		status = http.StatusServiceUnavailable
	case 3030, 3032:
		status = http.StatusGatewayTimeout
	}
	return types.WithOpenAIError(types.OpenAIError{Type: "upstream_error", Code: fmt.Sprintf("volcengine:%d", code), Message: "Volcengine speech request failed"}, status)
}

func handleTTSResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo, encoding string) (usage any, err *types.NewAPIError) {
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	var audioData []byte
	if info.ResponseFacts != nil {
		if duration := ojson.GetBytes(body, "addition.duration"); duration.Exists() && duration.Raw != "null" {
			info.ResponseFacts.AudioOutputDurationInvalid = true
			if duration.Type == ojson.String {
				milliseconds, err := decimal.NewFromString(duration.Str)
				if err == nil {
					openai.ObserveSpeechOutputSeconds(info, milliseconds.Shift(-3).String())
				} else {
					openai.ObserveSpeechOutputSeconds(info, duration.Str)
				}
			}
		}
		var encoded string
		if raw := ojson.GetBytes(body, "data").Raw; raw != "" && common.UnmarshalJsonStr(raw, &encoded) == nil {
			if decoded, decodeErr := base64.StdEncoding.DecodeString(encoded); decodeErr == nil && len(decoded) > 0 {
				audioData = decoded
				usage = openai.EstimateGeneratedSpeech(c, info, audioData, true)
			}
		}
		if usage == nil && info.ResponseFacts.AudioOutputDurationReported {
			usage = openai.EstimateGeneratedSpeech(c, info, nil, true)
		}
	}
	if readErr != nil {
		if info.ResponseFacts != nil {
			if info.StreamStatus == nil {
				info.StreamStatus = relaycommon.NewStreamStatus()
			}
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonScannerErr, readErr)
			info.StreamStatus.MarkIncomplete("incomplete_audio_response")
		}
		return usage, types.NewErrorWithStatusCode(errors.New("failed to read volcengine response"), types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	var volcResp VolcengineTTSResponse
	if unmarshalErr := common.Unmarshal(body, &volcResp); unmarshalErr != nil {
		return usage, types.NewErrorWithStatusCode(errors.New("failed to parse volcengine response"), types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if volcResp.Code != 3000 {
		return usage, speechError(volcResp.Code)
	}
	if audioData == nil {
		var decodeErr error
		audioData, decodeErr = base64.StdEncoding.DecodeString(volcResp.Data)
		if decodeErr != nil {
			return usage, types.NewErrorWithStatusCode(errors.New("failed to decode audio data"), types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
		}
	}
	if info.ResponseFacts != nil {
		return openai.WriteAudioResponse(c, info, resp, audioData), nil
	}
	contentType := getContentTypeByEncoding(encoding)
	c.Header("Content-Type", contentType)
	c.Data(http.StatusOK, contentType, audioData)
	usage = &dto.Usage{PromptTokens: info.GetEstimatePromptTokens(), CompletionTokens: 0, TotalTokens: info.GetEstimatePromptTokens()}
	return usage, nil
}

func generateRequestID() string {
	return uuid.New().String()
}

func handleTTSWebSocketResponse(c *gin.Context, requestURL string, volcRequest VolcengineTTSRequest, info *relaycommon.RelayInfo, encoding string) (usage any, err *types.NewAPIError) {
	_, token, parseErr := parseVolcengineAuth(info.ApiKey)
	if parseErr != nil {
		return nil, types.NewErrorWithStatusCode(
			parseErr,
			types.ErrorCodeChannelInvalidKey,
			http.StatusUnauthorized,
		)
	}

	header := http.Header{}
	header.Set("Authorization", fmt.Sprintf("Bearer;%s", token))

	conn, resp, dialErr := websocket.DefaultDialer.DialContext(context.Background(), requestURL, header)
	if dialErr != nil {
		if resp != nil {
			return nil, types.NewErrorWithStatusCode(
				fmt.Errorf("failed to connect to websocket: %w, status: %d", dialErr, resp.StatusCode),
				types.ErrorCodeBadResponseStatusCode,
				http.StatusBadGateway,
			)
		}
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("failed to connect to websocket: %w", dialErr),
			types.ErrorCodeBadResponseStatusCode,
			http.StatusBadGateway,
		)
	}
	defer conn.Close()

	payload, marshalErr := common.Marshal(volcRequest)
	if marshalErr != nil {
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("failed to marshal request: %w", marshalErr),
			types.ErrorCodeBadRequestBody,
			http.StatusInternalServerError,
		)
	}

	if sendErr := FullClientRequest(conn, payload); sendErr != nil {
		return nil, types.NewErrorWithStatusCode(
			fmt.Errorf("failed to send request: %w", sendErr),
			types.ErrorCodeBadRequestBody,
			http.StatusInternalServerError,
		)
	}

	contentType := getContentTypeByEncoding(encoding)
	c.Header("Content-Type", contentType)
	c.Header("Transfer-Encoding", "chunked")

	for {
		msg, recvErr := ReceiveMessage(conn)
		if recvErr != nil {
			if websocket.IsCloseError(recvErr, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				break
			}
			return nil, types.NewErrorWithStatusCode(
				fmt.Errorf("failed to receive message: %w", recvErr),
				types.ErrorCodeBadResponse,
				http.StatusInternalServerError,
			)
		}

		switch msg.MsgType {
		case MsgTypeError:
			return nil, types.NewErrorWithStatusCode(
				fmt.Errorf("received error from server: code=%d, %s", msg.ErrorCode, string(msg.Payload)),
				types.ErrorCodeBadResponse,
				http.StatusBadRequest,
			)
		case MsgTypeFrontEndResultServer:
			continue
		case MsgTypeAudioOnlyServer:
			if len(msg.Payload) > 0 {
				if _, writeErr := c.Writer.Write(msg.Payload); writeErr != nil {
					return nil, types.NewErrorWithStatusCode(
						fmt.Errorf("failed to write audio data: %w", writeErr),
						types.ErrorCodeBadResponse,
						http.StatusInternalServerError,
					)
				}
				c.Writer.Flush()
			}

			if msg.Sequence < 0 {
				c.Status(http.StatusOK)
				usage = &dto.Usage{
					PromptTokens:     info.GetEstimatePromptTokens(),
					CompletionTokens: 0,
					TotalTokens:      info.GetEstimatePromptTokens(),
				}
				return usage, nil
			}
		default:
			continue
		}
	}

	c.Status(http.StatusOK)
	usage = &dto.Usage{
		PromptTokens:     info.GetEstimatePromptTokens(),
		CompletionTokens: 0,
		TotalTokens:      info.GetEstimatePromptTokens(),
	}
	return usage, nil
}
