package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"

	"github.com/MoeForge/moego/ojson"

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

func normalizeSpeechOutputUsage(info *relaycommon.RelayInfo, usage *dto.Usage) {
	if info.ResponseFacts != nil && info.ResponseFacts.CompletionTokensReported && !info.ResponseFacts.AudioOutputTokensReported {
		usage.CompletionTokenDetails.AudioTokens = usage.CompletionTokens
	}
}

func applySpeechUsageHeaders(info *relaycommon.RelayInfo, usage *dto.Usage, headers http.Header) {
	if info.ChannelType != constant.ChannelTypeVLLM {
		return
	}
	for _, counter := range []struct {
		header string
		target *int
		output bool
	}{
		{"X-VLLM-OMNI-INPUT-TOKENS", &usage.PromptTokens, false},
		{"X-VLLM-OMNI-OUTPUT-TOKENS", &usage.CompletionTokens, true},
		{"X-VLLM-OMNI-INPUT-TEXT-TOKENS", &usage.PromptTokensDetails.TextTokens, false},
		{"X-VLLM-OMNI-INPUT-AUDIO-TOKENS", &usage.PromptTokensDetails.AudioTokens, false},
	} {
		var reported *bool
		if facts := info.ResponseFacts; facts != nil {
			switch counter.header {
			case "X-VLLM-OMNI-INPUT-TOKENS":
				reported = &facts.PromptTokensReported
			case "X-VLLM-OMNI-OUTPUT-TOKENS":
				reported = &facts.CompletionTokensReported
			case "X-VLLM-OMNI-INPUT-TEXT-TOKENS":
				reported = &facts.TextInputTokensReported
			case "X-VLLM-OMNI-INPUT-AUDIO-TOKENS":
				reported = &facts.AudioInputTokensReported
			}
		}
		if reported != nil && *reported {
			continue
		}
		value, err := strconv.ParseInt(headers.Get(counter.header), 10, 32)
		if err != nil || value < 0 {
			continue
		}
		*counter.target = int(value)
		if counter.output && (info.ResponseFacts == nil || !info.ResponseFacts.AudioOutputTokensReported) {
			usage.CompletionTokenDetails.AudioTokens = int(value)
			if info.ResponseFacts != nil {
				info.ResponseFacts.AudioOutputTokensReported = true
			}
		}
		if reported != nil {
			*reported = true
		}
	}
	if info.ResponseFacts != nil {
		info.ResponseFacts.UsageReported = info.ResponseFacts.PromptTokensReported && info.ResponseFacts.CompletionTokensReported
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
}

// speechAudioPayload requires the selected format's payload evidence before
// accepting audio with an incorrect JSON MIME, and excludes JSON-like bodies.
func speechAudioPayload(ctx context.Context, body []byte, format string, facts *relaycommon.ResponseFacts) bool {
	if len(body) == 0 || ojson.ValidBytes(body) {
		return false
	}
	payload := bytes.TrimSpace(body)
	if len(payload) == 0 {
		return false
	}
	first := payload[0]
	if first == '{' || first == '[' || first == '"' || first == '-' || first >= '0' && first <= '9' || bytes.HasPrefix(payload, []byte("true")) || bytes.HasPrefix(payload, []byte("false")) || bytes.HasPrefix(payload, []byte("null")) {
		return false
	}
	if format != "pcm" {
		duration, err := speechAudioDuration(ctx, body, format, facts)
		return err == nil && !math.IsNaN(duration) && !math.IsInf(duration, 0) && duration > 0
	}
	channels := int64(1)
	if facts != nil && facts.AudioOutputChannels > 0 && facts.AudioOutputChannels <= 1<<31-1 {
		channels = int64(facts.AudioOutputChannels)
	}
	return int64(len(body))%(channels*2) == 0
}

func speechAudioDuration(ctx context.Context, audio []byte, format string, facts *relaycommon.ResponseFacts) (float64, error) {
	if format == "pcm" {
		return float64(len(audio)) / float64(facts.AudioPCMBytesPerSecond()), nil
	}
	return common.GetAudioDuration(ctx, bytes.NewReader(audio), "."+format)
}

// EstimateGeneratedSpeech retains procurement quantities before presentation.
// Partial PCM has a known duration; incomplete encoded audio uses a byte estimate.
func EstimateGeneratedSpeech(c *gin.Context, info *relaycommon.RelayInfo, audio []byte, complete bool) *dto.Usage {
	usage := &dto.Usage{PromptTokens: info.GetEstimatePromptTokens()}
	usage.PromptTokensDetails.TextTokens = usage.PromptTokens
	format := "mp3"
	if request, ok := info.Request.(*dto.AudioRequest); ok && request.ResponseFormat != "" {
		format = request.ResponseFormat
	}
	if info.ResponseFacts != nil {
		info.ResponseFacts.GeneratedAudioBytes = max(info.ResponseFacts.GeneratedAudioBytes, int64(len(audio)))
	}
	var duration float64
	durationErr := errors.New("incomplete encoded audio")
	if complete || format == "pcm" {
		duration, durationErr = speechAudioDuration(c.Request.Context(), audio, format, info.ResponseFacts)
	}
	var clamp *common.QuotaClamp
	if durationErr != nil {
		usage.CompletionTokens, clamp = common.QuotaRoundChecked(math.Ceil(float64(len(audio)) / 1000))
	} else {
		usage.CompletionTokens, clamp = common.AudioDurationTokens(duration)
		if info.ResponseFacts != nil && !math.IsNaN(duration) && !math.IsInf(duration, 0) && duration >= 0 {
			seconds := decimal.NewFromFloat(duration).String()
			info.ResponseFacts.AudioOutputSeconds = &seconds
		}
	}
	if clamp != nil {
		info.QuotaClamp = clamp
	}
	usage.CompletionTokenDetails.AudioTokens = usage.CompletionTokens
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	return usage
}

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
		if info.ResponseFacts != nil {
			_ = EstimateGeneratedSpeech(c, info, body, true)
		}
		chunk, err := common.Marshal(struct {
			Type  string `json:"type"`
			Audio string `json:"audio"`
		}{"speech.audio.delta", base64.StdEncoding.EncodeToString(body)})
		if err != nil {
			return &dto.Usage{}
		}
		canonical.Header.Set("Content-Type", "text/event-stream")
		canonical.Body = io.NopCloser(strings.NewReader("data: " + string(chunk) + "\n\ndata: {\"type\":\"speech.audio.done\"}\n\ndata: [DONE]\n\n"))
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
	audioFormat := "mp3"
	if request, ok := info.Request.(*dto.AudioRequest); ok && request.ResponseFormat != "" {
		audioFormat = request.ResponseFormat
	}
	formatFacts := info.ResponseFacts
	if formatFacts == nil {
		formatFacts = &relaycommon.ResponseFacts{}
		if request, ok := info.Request.(*dto.AudioRequest); ok && request.SampleRate != nil && info.ChannelType == constant.ChannelTypeVLLM {
			formatFacts.AudioOutputSampleRate = *request.SampleRate
		}
	}
	if info.ChannelType == constant.ChannelTypeVLLM {
		for header, target := range map[string]*int{
			"X-Audio-Sample-Rate": &formatFacts.AudioOutputSampleRate,
			"X-Audio-Channels":    &formatFacts.AudioOutputChannels,
		} {
			if raw := resp.Header.Get(header); raw != "" {
				if value, err := strconv.ParseInt(raw, 10, 32); err == nil && value > 0 {
					*target = int(value)
				}
			}
		}
	}
	var bufferedAudio []byte
	var bufferedAudioErr error
	var audioBuffered bool
	if info.ResponseFacts != nil && (resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices || strings.Contains(resp.Header.Get("Content-Type"), "json")) {
		body, readErr := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusOK && !info.IsStream && speechAudioPayload(c.Request.Context(), body, audioFormat, formatFacts) {
			bufferedAudio, bufferedAudioErr, audioBuffered = body, readErr, true
			resp.Header = resp.Header.Clone()
			contentType := "audio/" + audioFormat
			if audioFormat == "mp3" {
				contentType = "audio/mpeg"
			} else if audioFormat == "opus" {
				contentType = "audio/ogg"
			}
			resp.Header.Set("Content-Type", contentType)
		} else {
			usageBody := body
			if !ojson.ValidBytes(body) {
				recovered := map[string]common.RawMessage{}
				for _, name := range []string{"usage", "error", "type", "code"} {
					if raw := ojson.GetBytes(body, name); raw.Exists() && ojson.Valid(raw.Raw) {
						recovered[name] = common.RawMessage(raw.Raw)
					}
				}
				if len(recovered) > 0 {
					if encoded, err := common.Marshal(recovered); err == nil {
						usageBody = encoded
					}
				}
			}
			usage = reportedMediaUsage(usageBody)
			observeMediaUsage(info, usage, usageBody)
			normalizeSpeechOutputUsage(info, usage)
			applySpeechUsageHeaders(info, usage, resp.Header)
			fault := observeMediaError(info, usageBody, resp.StatusCode)
			if info.StreamStatus == nil {
				info.StreamStatus = relaycommon.NewStreamStatus()
			}
			if readErr != nil {
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonScannerErr, readErr)
				info.StreamStatus.MarkIncomplete("incomplete_audio_response")
			}
			if fault == nil && resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
				info.StreamStatus.MarkFailed("", "invalid_response", http.StatusBadGateway)
			}
			return usage
		}
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
		var audioBytes, deliveredAudioBytes int64
		var generatedPCMSeconds, deliveredPCMSeconds big.Rat
		var encodedAudio bytes.Buffer
		var completed bool
		helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
			eventBody := []byte(data)
			if !ojson.ValidBytes(eventBody) {
				sr.Stop(errors.New("invalid speech stream event"))
				return
			}
			if raw := ojson.GetBytes(eventBody, "usage"); raw.Exists() && raw.Raw != "null" {
				reported := reportedMediaUsage(eventBody)
				prompt, completion := relaycommon.MediaUsagePresence(eventBody)
				observeMediaUsage(info, reported, eventBody)
				if prompt {
					usage.PromptTokens = reported.PromptTokens
				}
				if completion {
					usage.CompletionTokens = reported.CompletionTokens
				}
				mergeMediaTokenDetails(usage, eventBody)
			}
			if observeMediaError(info, eventBody, resp.StatusCode) != nil {
				sr.Stop(errors.New("upstream speech stream failed"))
				return
			}
			var chunk struct {
				Type       string `json:"type"`
				Audio      string `json:"audio"`
				Delta      string `json:"delta"`
				SampleRate *int   `json:"sample_rate"`
				Channels   *int   `json:"channels"`
			}
			if err := common.Unmarshal(eventBody, &chunk); err != nil {
				sr.Stop(errors.New("invalid speech stream event"))
				return
			}
			if info.ChannelType == constant.ChannelTypeVLLM {
				for _, field := range []struct{ source, target *int }{{chunk.SampleRate, &formatFacts.AudioOutputSampleRate}, {chunk.Channels, &formatFacts.AudioOutputChannels}} {
					if field.source == nil {
						continue
					}
					if *field.source <= 0 || *field.source > 1<<31-1 {
						sr.Stop(errors.New("invalid speech audio format"))
						return
					}
					*field.target = *field.source
				}
			}
			if chunk.Type == "speech.audio.done" {
				if audioBytes == 0 {
					sr.Stop(errors.New("speech stream completed without audio"))
					return
				}
				completed = true
			}
			var business bool
			var chunkBytes int64
			var chunkPCMSeconds big.Rat
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
				chunkBytes = int64(len(audio))
				audioBytes += chunkBytes
				if audioFormat == "pcm" {
					chunkPCMSeconds.SetFrac64(chunkBytes, formatFacts.AudioPCMBytesPerSecond())
					generatedPCMSeconds.Add(&generatedPCMSeconds, &chunkPCMSeconds)
				}
				if audioFormat != "pcm" && info.ResponseFacts.AudioOutputSeconds == nil {
					_, _ = encodedAudio.Write(audio)
				}
				business = len(audio) > 0
			}
			writeErr := helper.StringData(c, data)
			var flushErr *helper.StreamFlushError
			if business && (writeErr == nil || errors.As(writeErr, &flushErr)) {
				deliveredAudioBytes += chunkBytes
				deliveredPCMSeconds.Add(&deliveredPCMSeconds, &chunkPCMSeconds)
				info.ResponseFacts.MarkBusinessOutput()
			}
			if writeErr != nil {
				info.ResponseFacts.DownstreamWriteFailed = true
				sr.Stop(writeErr)
			} else if chunk.Type == "speech.audio.done" {
				sr.Done()
			}
		})
		if info.StreamStatus != nil {
			info.StreamStatus.RequireTerminal()
			if completed && info.StreamStatus.IsNormalEnd() && !info.StreamStatus.HasErrors() {
				info.StreamStatus.MarkCompleted()
			} else {
				info.StreamStatus.MarkIncomplete("missing_audio_completion")
			}
		}
		generatedAudioBytes := max(info.ResponseFacts.GeneratedAudioBytes, audioBytes)
		info.ResponseFacts.GeneratedAudioBytes = generatedAudioBytes
		info.ResponseFacts.DeliveredAudioBytes = deliveredAudioBytes
		if audioFormat == "pcm" {
			if generatedAudioBytes <= audioBytes || info.ResponseFacts.AudioOutputSeconds == nil {
				duration, _ := generatedPCMSeconds.Float64()
				seconds := decimal.NewFromFloat(duration).String()
				info.ResponseFacts.AudioOutputSeconds = &seconds
			}
			duration, _ := deliveredPCMSeconds.Float64()
			seconds := decimal.NewFromFloat(duration).String()
			info.ResponseFacts.DeliveredAudioSeconds = &seconds
		} else if info.ResponseFacts.AudioOutputSeconds == nil && completed {
			if duration, err := speechAudioDuration(c.Request.Context(), encodedAudio.Bytes(), audioFormat, info.ResponseFacts); err == nil && !math.IsNaN(duration) && !math.IsInf(duration, 0) && duration >= 0 {
				seconds := decimal.NewFromFloat(duration).String()
				info.ResponseFacts.AudioOutputSeconds = &seconds
			}
		}
		// Speech output is audio unless an explicit detail counter says otherwise.
		normalizeSpeechOutputUsage(info, usage)
		if !info.ResponseFacts.CompletionTokensReported && generatedAudioBytes > 0 {
			var tokens int
			var clamp *common.QuotaClamp
			if info.ResponseFacts.AudioOutputSeconds != nil {
				duration, _ := strconv.ParseFloat(*info.ResponseFacts.AudioOutputSeconds, 64)
				tokens, clamp = common.AudioDurationTokens(duration)
			} else {
				tokens, clamp = common.QuotaRoundChecked(math.Ceil(float64(generatedAudioBytes) / 1000))
			}
			usage.CompletionTokens = tokens
			if !info.ResponseFacts.AudioOutputTokensReported {
				usage.CompletionTokenDetails.AudioTokens = tokens
			}
			if clamp != nil {
				info.QuotaClamp = clamp
			}
		}
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		if info.StreamStatus != nil && info.StreamStatus.EndReason == relaycommon.StreamEndReasonDone && info.StreamStatus.ResponseOutcome() == string(relaycommon.ResponseOutcomeCompleted) {
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
		bodyBytes, err := bufferedAudio, bufferedAudioErr
		if !audioBuffered {
			bodyBytes, err = io.ReadAll(resp.Body)
		}
		if err != nil {
			logger.LogError(c, common.LogText("failed to read TTS response body: %v", err))
			if info.ResponseFacts != nil {
				usage = EstimateGeneratedSpeech(c, info, bodyBytes, false)
				if info.StreamStatus == nil {
					info.StreamStatus = relaycommon.NewStreamStatus()
				}
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonScannerErr, err)
				info.StreamStatus.MarkIncomplete("incomplete_audio_response")
			} else {
				c.Writer.WriteHeaderNow()
			}
			applySpeechUsageHeaders(info, usage, resp.Header)
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
			if !info.ResponseFacts.DownstreamWriteFailed && len(bodyBytes) > 0 {
				info.ResponseFacts.MarkBusinessOutput()
			}
		}

		duration, durationErr := speechAudioDuration(c.Request.Context(), bodyBytes, audioFormat, formatFacts)

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
		applySpeechUsageHeaders(info, usage, resp.Header)
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
	structured := responseFormat != "text" && responseFormat != "srt" && responseFormat != "vtt" || strings.Contains(resp.Header.Get("Content-Type"), "json")
	var responseData struct {
		Usage *dto.Usage `json:"usage"`
		Text  string     `json:"text"`
	}
	var decodeErr error
	if info.ResponseFacts != nil {
		if structured {
			var envelope struct {
				Usage json.RawMessage `json:"usage"`
				Text  string          `json:"text"`
			}
			decodeErr = common.Unmarshal(responseBody, &envelope)
			responseData.Text = envelope.Text
			if decodeErr == nil {
				info.ResponseFacts.PromptTokensReported, info.ResponseFacts.CompletionTokensReported = relaycommon.MediaUsagePresence(responseBody)
				info.ResponseFacts.UsageReported = info.ResponseFacts.PromptTokensReported && info.ResponseFacts.CompletionTokensReported
				if len(envelope.Usage) > 0 && string(envelope.Usage) != "null" {
					responseData.Usage = reportedMediaUsage(responseBody)
				}
			}
		}
	} else {
		decodeErr = common.Unmarshal(responseBody, &responseData)
	}
	if decodeErr == nil && responseData.Usage != nil {
		normalizeOpenAIUsage(responseData.Usage)
		observeMediaUsage(info, responseData.Usage, responseBody)
	}
	if info.ResponseFacts != nil && structured && decodeErr == nil {
		duration := ojson.GetBytes(responseBody, "usage.seconds")
		if !duration.Exists() {
			duration = ojson.GetBytes(responseBody, "duration")
		}
		if duration.Exists() {
			info.ResponseFacts.AudioInputDurationInvalid = true
			if duration.Type == ojson.Number && helper.ValidateGatewayMediaMultipliers([]byte(`{"duration":`+duration.Raw+`}`)) == nil {
				seconds, err := decimal.NewFromString(duration.Raw)
				if err == nil {
					text := seconds.String()
					info.ResponseFacts.AudioInputSeconds = &text
					info.ResponseFacts.AudioInputDurationReported = true
					info.ResponseFacts.AudioInputDurationInvalid = false
					if !info.ResponseFacts.PromptTokensReported {
						if responseData.Usage == nil {
							responseData.Usage = &dto.Usage{}
						}
						// Ceil the exact JSON duration before converting the bounded
						// whole seconds to the shared audio token unit.
						tokens, clamp := common.AudioDurationTokens(seconds.Ceil().InexactFloat64())
						responseData.Usage.PromptTokens = tokens
						responseData.Usage.PromptTokensDetails.AudioTokens = tokens
						if clamp != nil {
							info.QuotaClamp = clamp
						}
					}
				}
			} else if duration.Type == ojson.Number {
				seconds, _ := strconv.ParseFloat(duration.Raw, 64)
				_, clamp := common.AudioDurationTokens(seconds)
				if clamp != nil {
					info.QuotaClamp = clamp
				}
			}
		}
	}
	if info.ResponseFacts != nil && structured {
		if fault := observeMediaError(info, responseBody, resp.StatusCode); fault != nil {
			return types.WithOpenAIError(*fault, resp.StatusCode), responseData.Usage
		}
	}
	if info.ResponseFacts != nil && (resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices) {
		return types.NewError(fmt.Errorf("upstream transcription request failed"), types.ErrorCodeBadResponse), responseData.Usage
	}
	if info.ResponseFacts != nil {
		text := responseData.Text
		if responseFormat == "text" || responseFormat == "srt" || responseFormat == "vtt" {
			text = strings.TrimSpace(string(responseBody))
		}
		info.ResponseFacts.GeneratedText = text
	}
	writeErr := service.IOCopyBytesGracefully(c, resp, responseBody)
	if info.ResponseFacts != nil {
		if writeErr != nil {
			info.ResponseFacts.DownstreamWriteFailed = true
			return types.NewError(writeErr, types.ErrorCodeBadResponse), responseData.Usage
		}
		if info.ResponseFacts.GeneratedText != "" {
			info.ResponseFacts.MarkBusinessOutput()
		}
	}
	if decodeErr == nil && responseData.Usage != nil {
		if responseData.Usage.TotalTokens > 0 || info.UpstreamProtocol != "" || info.ResponseFacts != nil {
			usage := responseData.Usage
			if usage.PromptTokens == 0 {
				usage.PromptTokens = usage.InputTokens
			}
			if usage.CompletionTokens == 0 {
				usage.CompletionTokens = usage.OutputTokens
			}
			if info.ResponseFacts != nil && !info.ResponseFacts.PromptTokensReported && !info.ResponseFacts.AudioInputDurationReported {
				usage.PromptTokens = info.GetEstimatePromptTokens()
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
