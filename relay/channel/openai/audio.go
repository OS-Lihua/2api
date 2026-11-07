package openai

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

// WriteAudioResponse delivers decoded native speech through the shared OpenAI
// audio presentation and duration accounting path, including JSON-style SSE.
func WriteAudioResponse(c *gin.Context, info *relaycommon.RelayInfo, response *http.Response, body []byte) *dto.Usage {
	canonical := *response
	canonical.Header = response.Header.Clone()
	canonical.Header.Del("Content-Length")
	format := "mp3"
	if request, ok := info.Request.(*dto.AudioRequest); ok && request.ResponseFormat != "" {
		format = request.ResponseFormat
	}
	contentType := "application/octet-stream"
	switch format {
	case "mp3":
		contentType = "audio/mpeg"
	case "opus":
		contentType = "audio/ogg"
	case "wav", "flac", "aac", "pcm":
		contentType = "audio/" + format
	}
	canonical.Header.Set("Content-Type", contentType)
	canonical.Body = io.NopCloser(bytes.NewReader(body))
	if info.IsStream {
		chunk, err := common.Marshal(struct {
			Type  string `json:"type"`
			Audio string `json:"audio"`
		}{"speech.audio.delta", base64.StdEncoding.EncodeToString(body)})
		if err != nil {
			return &dto.Usage{}
		}
		canonical.Header.Set("Content-Type", "text/event-stream")
		canonical.Body = io.NopCloser(strings.NewReader("data: " + string(chunk) + "\n\ndata: [DONE]\n\n"))
	}
	return OpenaiTTSHandler(c, &canonical, info)
}

func OpenaiTTSHandler(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) *dto.Usage {
	// the status code has been judged before, if there is a body reading failure,
	// it should be regarded as a non-recoverable error, so it should not return err for external retry.
	// Analogous to nginx's load balancing, it will only retry if it can't be requested or
	// if the upstream returns a specific status code, once the upstream has already written the header,
	// the subsequent failure of the response body should be regarded as a non-recoverable error,
	// and can be terminated directly.
	defer service.CloseResponseBodyGracefully(resp)
	usage := &dto.Usage{}
	if info.ResponseFacts != nil && (resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices) {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return usage
		}
		var envelope struct {
			Usage dto.Usage `json:"usage"`
		}
		if common.Unmarshal(body, &envelope) == nil {
			usage = &envelope.Usage
			normalizeOpenAIUsage(usage)
			observeMediaUsage(info, usage, body)
		}
		return usage
	}
	usage.PromptTokens = info.GetEstimatePromptTokens()
	usage.TotalTokens = info.GetEstimatePromptTokens()
	for k, v := range resp.Header {
		if !service.ShouldCopyUpstreamHeader(c, k, v) {
			continue
		}
		c.Writer.Header().Set(k, v[0])
	}
	c.Writer.WriteHeader(resp.StatusCode)

	if info.IsStream && info.ResponseFacts != nil {
		var audioBytes int64
		helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
			var chunk struct {
				Type  string     `json:"type"`
				Audio string     `json:"audio"`
				Delta string     `json:"delta"`
				Usage *dto.Usage `json:"usage"`
			}
			if err := common.UnmarshalJsonStr(data, &chunk); err != nil {
				sr.Stop(err)
				return
			}
			if chunk.Usage != nil {
				normalizeOpenAIUsage(chunk.Usage)
				prompt, completion := relaycommon.MediaUsagePresence([]byte(data))
				observeMediaUsage(info, chunk.Usage, []byte(data))
				if prompt {
					usage.PromptTokens, usage.PromptTokensDetails = chunk.Usage.PromptTokens, chunk.Usage.PromptTokensDetails
				}
				if completion {
					usage.CompletionTokens, usage.CompletionTokenDetails = chunk.Usage.CompletionTokens, chunk.Usage.CompletionTokenDetails
				}
			}
			var business bool
			if chunk.Type == "speech.audio.delta" {
				encoded := chunk.Audio
				if encoded == "" {
					encoded = chunk.Delta
				}
				audio, err := base64.StdEncoding.DecodeString(encoded)
				if err != nil || int64(len(audio)) > math.MaxInt64-audioBytes {
					sr.Stop(errors.New("invalid speech audio chunk"))
					return
				}
				audioBytes += int64(len(audio))
				business = len(audio) > 0
			}
			writeErr := helper.StringData(c, data)
			var flushErr *helper.StreamFlushError
			if business && (writeErr == nil || errors.As(writeErr, &flushErr)) {
				info.ResponseFacts.BusinessOutput = true
			}
			if writeErr != nil {
				info.ResponseFacts.DownstreamWriteFailed = true
				sr.Stop(writeErr)
			}
		})
		info.ResponseFacts.GeneratedAudioBytes = audioBytes
		if request, ok := info.Request.(*dto.AudioRequest); ok && request.ResponseFormat == "pcm" {
			seconds := decimal.NewFromInt(audioBytes).Div(decimal.NewFromInt(48000)).String()
			info.ResponseFacts.AudioOutputSeconds = &seconds
		}
		if !info.ResponseFacts.CompletionTokensReported && audioBytes > 0 {
			var tokens int
			var clamp *common.QuotaClamp
			if request, ok := info.Request.(*dto.AudioRequest); ok && request.ResponseFormat == "pcm" {
				tokens, clamp = common.AudioDurationTokens(float64(audioBytes) / 48000)
			} else {
				tokens, clamp = common.QuotaRoundChecked(math.Ceil(float64(audioBytes) / 1000))
			}
			usage.CompletionTokens, usage.CompletionTokenDetails.AudioTokens = tokens, tokens
			if clamp != nil {
				info.QuotaClamp = clamp
			}
		}
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		if info.StreamStatus != nil && info.StreamStatus.EndReason == relaycommon.StreamEndReasonDone {
			if err := helper.StringData(c, "[DONE]"); err != nil {
				info.ResponseFacts.DownstreamWriteFailed = true
			}
		}
	} else if info.IsStream {
		helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
			if service.SundaySearch(data, "usage") {
				var simpleResponse dto.SimpleResponse
				if err := common.Unmarshal([]byte(data), &simpleResponse); err != nil {
					logger.LogError(c, err.Error())
					sr.Error(err)
				} else if simpleResponse.Usage.TotalTokens != 0 {
					usage.PromptTokens = simpleResponse.Usage.InputTokens
					usage.CompletionTokens = simpleResponse.OutputTokens
					usage.TotalTokens = simpleResponse.TotalTokens
				}
			}
			if err := helper.StringData(c, data); err != nil {
				sr.Error(err)
			}
		})
	} else {
		common.SetContextKey(c, constant.ContextKeyLocalCountTokens, true)
		// 读取响应体到缓冲区
		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			logger.LogError(c, common.LogText("failed to read TTS response body: %v", err))
			c.Writer.WriteHeaderNow()
			return usage
		}

		// 写入响应到客户端
		c.Writer.WriteHeaderNow()
		n, writeErr := c.Writer.Write(bodyBytes)
		if writeErr != nil {
			logger.LogError(c, common.LogText("failed to write TTS response: %v", writeErr))
		}
		if info.ResponseFacts != nil {
			info.ResponseFacts.DownstreamWriteFailed = writeErr != nil || n != len(bodyBytes)
			info.ResponseFacts.BusinessOutput = !info.ResponseFacts.DownstreamWriteFailed && len(bodyBytes) > 0
		}

		// 计算音频时长并更新 usage
		audioFormat := "mp3" // 默认格式
		if audioReq, ok := info.Request.(*dto.AudioRequest); ok && audioReq.ResponseFormat != "" {
			audioFormat = audioReq.ResponseFormat
		}

		var duration float64
		var durationErr error

		if audioFormat == "pcm" {
			// PCM 格式没有文件头，根据 OpenAI TTS 的 PCM 参数计算时长
			// 采样率: 24000 Hz, 位深度: 16-bit (2 bytes), 声道数: 1
			const sampleRate = 24000
			const bytesPerSample = 2
			const channels = 1
			duration = float64(len(bodyBytes)) / float64(sampleRate*bytesPerSample*channels)
		} else {
			ext := "." + audioFormat
			reader := bytes.NewReader(bodyBytes)
			duration, durationErr = common.GetAudioDuration(c.Request.Context(), reader, ext)
		}

		if info.ResponseFacts != nil {
			info.ResponseFacts.GeneratedAudioBytes = int64(len(bodyBytes))
		}
		usage.PromptTokensDetails.TextTokens = usage.PromptTokens
		if info.ResponseFacts != nil && durationErr == nil && !math.IsNaN(duration) && !math.IsInf(duration, 0) && duration >= 0 {
			seconds := decimal.NewFromFloat(duration).String()
			info.ResponseFacts.AudioOutputSeconds = &seconds
		}

		if durationErr != nil {
			logger.LogWarn(c, common.LogText("failed to get audio duration: %v", durationErr))
			// 如果无法获取时长，则设置保底的 CompletionTokens，根据body大小计算
			sizeInKB := float64(len(bodyBytes)) / 1000.0
			estimatedTokens, clamp := common.QuotaRoundChecked(math.Ceil(sizeInKB))
			if clamp != nil {
				info.QuotaClamp = clamp
			}
			usage.CompletionTokens = estimatedTokens
			usage.CompletionTokenDetails.AudioTokens = estimatedTokens
		} else {
			// 计算 token: ceil(duration) / 60.0 * 1000，即每分钟 1000 tokens。
			// duration 解析自上游返回的音频元数据，饱和转换防止 int 回绕。
			completionTokens, clamp := common.AudioDurationTokens(duration)
			if clamp != nil {
				info.QuotaClamp = clamp
			}
			usage.CompletionTokens = completionTokens
			usage.CompletionTokenDetails.AudioTokens = completionTokens
		}
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}

	return usage
}

func OpenaiSTTHandler(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo, responseFormat string) (*types.NewAPIError, *dto.Usage) {
	defer service.CloseResponseBodyGracefully(resp)

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError), nil
	}
	if info.ResponseFacts != nil {
		info.ResponseFacts.PromptTokensReported, info.ResponseFacts.CompletionTokensReported = relaycommon.MediaUsagePresence(responseBody)
		info.ResponseFacts.UsageReported = info.ResponseFacts.PromptTokensReported && info.ResponseFacts.CompletionTokensReported
	}
	var responseData struct {
		Usage *dto.Usage `json:"usage"`
		Text  string     `json:"text"`
	}
	decodeErr := common.Unmarshal(responseBody, &responseData)
	if decodeErr == nil && responseData.Usage != nil {
		normalizeOpenAIUsage(responseData.Usage)
		observeMediaUsage(info, responseData.Usage, responseBody)
	}
	if info.ResponseFacts != nil && (resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices) {
		return types.NewError(fmt.Errorf("upstream transcription request failed"), types.ErrorCodeBadResponse), responseData.Usage
	}
	writeErr := service.IOCopyBytesGracefully(c, resp, responseBody)
	if info.ResponseFacts != nil {
		if writeErr != nil {
			info.ResponseFacts.DownstreamWriteFailed = true
			return types.NewError(writeErr, types.ErrorCodeBadResponse), responseData.Usage
		}
		text := responseData.Text
		if responseFormat == "text" || responseFormat == "srt" || responseFormat == "vtt" {
			text = strings.TrimSpace(string(responseBody))
		}
		info.ResponseFacts.GeneratedText = text
		info.ResponseFacts.BusinessOutput = text != ""
	}
	if decodeErr == nil && responseData.Usage != nil {
		if responseData.Usage.TotalTokens > 0 || info.UpstreamProtocol != "" {
			usage := responseData.Usage
			if usage.PromptTokens == 0 {
				usage.PromptTokens = usage.InputTokens
			}
			if usage.CompletionTokens == 0 {
				usage.CompletionTokens = usage.OutputTokens
			}
			return nil, usage
		}
	}

	usage := &dto.Usage{}
	usage.PromptTokens = info.GetEstimatePromptTokens()
	usage.CompletionTokens = 0
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	return nil, usage
}
