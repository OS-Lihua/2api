package service

import (
	"bytes"
	"encoding/base64"
	"errors"
	"math"
	"net/http"
	"slices"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/capability"
	"github.com/QuantumNous/new-api/pkg/protocol"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	constant2 "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

// CountGatewayMediaInput includes reference audio and its transcript in the
// original or effective speech input before the funding plan is frozen.
func CountGatewayMediaInput(c *gin.Context, request dto.Request, info *relaycommon.RelayInfo) (int, error) {
	meta := request.GetTokenCountMeta()
	referenceTokens := 0
	if audio, ok := request.(*dto.AudioRequest); ok && info.ResponseFacts != nil && info.RelayMode == constant2.RelayModeAudioSpeech {
		facts := info.ResponseFacts
		facts.AudioInputSeconds = nil
		facts.AudioInputTokensEstimated = 0
		for _, value := range []common.RawMessage{audio.RefAudio2, audio.RefAudioCodes, audio.SpeakerEmbedding, audio.ExtraParams} {
			if raw := strings.TrimSpace(string(value)); raw != "" && raw != "null" {
				return 0, errors.New("speech conditioning input cannot be measured locally")
			}
		}
		if audio.Instructions != "" {
			meta.CombineText += "\n" + audio.Instructions
		}
		transcript := ""
		if len(audio.RefText) > 0 && string(audio.RefText) != "null" {
			if common.Unmarshal(audio.RefText, &transcript) != nil {
				return 0, errors.New("invalid speech reference transcript")
			}
		}
		var references []string
		singleReference := false
		if len(audio.RefAudio) > 0 && string(audio.RefAudio) != "null" {
			var single string
			if common.Unmarshal(audio.RefAudio, &single) == nil {
				references = []string{single}
				singleReference = true
			} else if common.Unmarshal(audio.RefAudio, &references) != nil {
				return 0, errors.New("invalid speech reference audio")
			}
			if len(references) == 0 {
				return 0, errors.New("invalid speech reference audio")
			}
		}
		if len(audio.References) > 0 && string(audio.References) != "null" {
			var aliases []struct {
				AudioPath string  `json:"audio_path"`
				Text      *string `json:"text"`
			}
			if common.Unmarshal(audio.References, &aliases) != nil || len(aliases) != 1 || aliases[0].AudioPath == "" {
				return 0, errors.New("invalid speech reference alias")
			}
			alias := aliases[0]
			if len(references) > 0 && (!singleReference || references[0] != alias.AudioPath) {
				return 0, errors.New("conflicting speech reference audio")
			}
			references = []string{alias.AudioPath}
			if alias.Text != nil && strings.TrimSpace(*alias.Text) != "" {
				if transcript != "" && transcript != *alias.Text {
					return 0, errors.New("conflicting speech reference transcript")
				}
				transcript = *alias.Text
			}
		}
		if transcript != "" {
			meta.CombineText += "\n" + transcript
		}
		if len(references) > 0 {
			seconds := decimal.Zero
			for _, reference := range references {
				mediaType := ""
				if strings.HasPrefix(reference, "data:") {
					header, _, ok := strings.Cut(reference, ",")
					if !ok || !strings.HasPrefix(header, "data:audio/") || !strings.HasSuffix(header, ";base64") {
						return 0, errors.New("invalid speech reference audio")
					}
					mediaType = strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
				} else if strings.Contains(reference, ",") {
					return 0, errors.New("speech reference audio must be measurable inline data")
				}
				encoded, err := DecodeBase64AudioData(reference)
				if err != nil {
					return 0, errors.New("speech reference audio must be measurable inline data")
				}
				data, err := base64.StdEncoding.DecodeString(encoded)
				if err != nil || len(data) == 0 || constant.MaxFileDownloadMB > 0 && len(data) > constant.MaxFileDownloadMB*1024*1024 {
					return 0, errors.New("invalid speech reference audio")
				}
				if detected := http.DetectContentType(data); detected != "application/octet-stream" {
					mediaType = detected
				}
				_, format, _ := strings.Cut(mediaType, "/")
				format = strings.TrimPrefix(format, "x-")
				switch format {
				case "wave", "vnd.wave":
					format = "wav"
				case "mpeg":
					format = "mp3"
				case "mp4":
					format = "m4a"
				}
				duration, err := common.GetAudioDuration(c.Request.Context(), bytes.NewReader(data), "."+format)
				if err != nil || math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 || duration > relaycommon.MaxTaskDurationSeconds {
					return 0, errors.New("speech reference duration exceeds the request limit")
				}
				seconds = seconds.Add(decimal.NewFromFloat(duration))
				if seconds.GreaterThan(decimal.NewFromInt(relaycommon.MaxTaskDurationSeconds)) {
					return 0, errors.New("speech reference duration exceeds the request limit")
				}
				tokens, clamp := common.AudioDurationTokens(duration)
				if clamp != nil {
					return 0, clamp
				}
				referenceTokens, clamp = common.QuotaFromFloatChecked(float64(referenceTokens) + float64(tokens))
				if clamp != nil {
					return 0, clamp
				}
			}
			value := seconds.String()
			facts.AudioInputSeconds = &value
			facts.AudioInputTokensEstimated = referenceTokens
		}
	}
	input, err := CountRequestToken(c, meta, info)
	if err != nil {
		return 0, err
	}
	input, clamp := common.QuotaFromFloatChecked(float64(input) + float64(referenceTokens))
	if clamp != nil {
		return 0, clamp
	}
	return input, nil
}

// GatewayMediaCapabilitiesMatch is applied to both original and effective DTOs.
// Uploaded audio duration is measured before reservation; no request duration or
// output ceiling is accepted as actual usage.
func GatewayMediaCapabilitiesMatch(c *gin.Context, kind protocol.Type, request dto.Request, caps capability.Set, inputTokens int, facts *relaycommon.ResponseFacts) bool {
	if request.IsStream(c.Request) && !slices.Contains(caps.Features, capability.Streaming) {
		return false
	}
	input, output := capability.Text, capability.Image
	switch kind {
	case protocol.OpenAIImages:
		image, ok := request.(*dto.ImageRequest)
		if !ok {
			return false
		}
		count, err := image.ImageCount(false)
		if err != nil || caps.Limits.MaxImages != nil && int64(count) > *caps.Limits.MaxImages {
			return false
		}
		uploadedImages := 0
		if form := c.Request.MultipartForm; form != nil {
			for field, files := range form.File {
				if field == "image" || strings.HasPrefix(field, "image[") {
					uploadedImages += len(files)
				}
			}
		}
		images := 0
		for _, raw := range []common.RawMessage{image.Image, image.Images} {
			if len(raw) == 0 {
				continue
			}
			value := ojson.ParseBytes(raw)
			if value.IsArray() {
				images += len(value.Array())
			} else if value.Exists() {
				images++
			}
		}
		// Uploaded files and the effective DTO are separate input views.
		images = max(images, uploadedImages)
		if images > dto.MaxImageN || images > 0 && !slices.Contains(caps.InputModalities, capability.Image) || caps.Limits.MaxImages != nil && int64(images) > *caps.Limits.MaxImages {
			return false
		}
	case protocol.OpenAIAudioSpeech:
		if caps.Limits.MaxDurationSeconds != nil {
			return false
		}
		if facts != nil && facts.AudioInputSeconds != nil && !slices.Contains(caps.InputModalities, capability.Audio) {
			return false
		}
		output = capability.Audio
	case protocol.OpenAIAudioTranscription:
		input, output = capability.Audio, capability.Text
	default:
		return false
	}
	if !slices.Contains(caps.InputModalities, input) || !slices.Contains(caps.OutputModalities, output) {
		return false
	}
	if caps.Limits.ContextLength != nil && int64(inputTokens) > *caps.Limits.ContextLength {
		return false
	}
	if facts != nil && facts.AudioInputSeconds != nil && caps.Limits.MaxDurationSeconds != nil {
		seconds, err := decimal.NewFromString(*facts.AudioInputSeconds)
		if err != nil || seconds.IsNegative() || seconds.GreaterThan(decimal.NewFromInt(*caps.Limits.MaxDurationSeconds)) {
			return false
		}
	}
	return true
}
