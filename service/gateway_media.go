package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
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
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/tokenkit"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

// gatewayImageInputs shares the canonical and uploaded image view. Remote
// references use tokenkit's existing typical-size estimate without a fetch.
func gatewayImageInputs(c *gin.Context, request *dto.ImageRequest, model string) ([]tokenkit.Image, error) {
	var inputs []tokenkit.Image
	for _, raw := range []common.RawMessage{request.Image, request.Images} {
		if len(raw) == 0 {
			continue
		}
		value := ojson.ParseBytes(raw)
		if value.Type == ojson.Null {
			continue
		}
		values := []ojson.Result{value}
		if value.IsArray() {
			values = value.Array()
		}
		if len(inputs)+len(values) > dto.MaxImageN {
			return nil, errors.New("image input count exceeds the request limit")
		}
		for _, value := range values {
			metadata := tokenkit.Image{}
			reference := value.Str
			if value.IsObject() {
				metadata.Detail = value.Get("detail").Str
				reference = value.Get("url").Str
				if reference == "" {
					reference = value.Get("image_url").Str
				}
				if reference == "" {
					reference = value.Get("image_url.url").Str
				}
			}
			if tokenkit.ImageNeedsSize(model, metadata.Detail) {
				encoded := reference
				if strings.HasPrefix(encoded, "data:") {
					_, payload, ok := strings.Cut(encoded, ",")
					encoded = ""
					if ok {
						encoded = payload
					}
				}
				if encoded != "" && !strings.Contains(encoded, ":") {
					config, _, err := image.DecodeConfig(base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded)))
					if err == nil {
						metadata.Width, metadata.Height = config.Width, config.Height
					}
				}
			}
			inputs = append(inputs, metadata)
		}
	}
	var uploaded []tokenkit.Image
	if form := c.Request.MultipartForm; form != nil {
		for field, files := range form.File {
			if field != "image" && !strings.HasPrefix(field, "image[") && field != "image_prompt" {
				continue
			}
			if len(uploaded)+len(files) > dto.MaxImageN {
				return nil, errors.New("image input count exceeds the request limit")
			}
			for _, header := range files {
				metadata := tokenkit.Image{}
				if tokenkit.ImageNeedsSize(model, "") {
					file, err := header.Open()
					if err != nil {
						return nil, errors.New("image input is unavailable")
					}
					config, _, err := image.DecodeConfig(file)
					_ = file.Close()
					if err == nil {
						metadata.Width, metadata.Height = config.Width, config.Height
					}
				}
				uploaded = append(uploaded, metadata)
			}
		}
	}
	if len(uploaded) > len(inputs) {
		return uploaded, nil
	}
	return inputs, nil
}

// CountGatewayMediaInput includes images, reference audio and transcripts in
// original or effective media input before the funding plan is frozen.
func CountGatewayMediaInput(c *gin.Context, request dto.Request, info *relaycommon.RelayInfo) (int, error) {
	meta := request.GetTokenCountMeta()
	inputMediaTokens := 0
	if request, ok := request.(*dto.ImageRequest); ok && info.ResponseFacts != nil {
		inputs, err := gatewayImageInputs(c, request, info.TokenizerModel)
		if err != nil {
			return 0, err
		}
		for _, input := range inputs {
			var clamp *common.QuotaClamp
			inputMediaTokens, clamp = common.QuotaFromFloatChecked(float64(inputMediaTokens) + float64(tokenkit.ImageTokens(info.TokenizerModel, input)))
			if clamp != nil {
				return 0, clamp
			}
		}
		info.ResponseFacts.ImageInputTokensEstimated = inputMediaTokens
	}
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
				inputMediaTokens, clamp = common.QuotaFromFloatChecked(float64(inputMediaTokens) + float64(tokens))
				if clamp != nil {
					return 0, clamp
				}
			}
			value := seconds.String()
			facts.AudioInputSeconds = &value
			facts.AudioInputTokensEstimated = inputMediaTokens
		}
	}
	input, err := CountRequestToken(c, meta, info)
	if err != nil {
		return 0, err
	}
	input, clamp := common.QuotaFromFloatChecked(float64(input) + float64(inputMediaTokens))
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
		inputs, err := gatewayImageInputs(c, image, "")
		if err != nil {
			return false
		}
		images := len(inputs)
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

const gatewayMediaContextKey = "gateway_prepared_media"

type gatewayMediaSource struct{ data, mime string }
type gatewayMediaContent struct{ base64, mime string }

func gatewayMediaIdentity(source types.FileSource) gatewayMediaSource {
	key := gatewayMediaSource{data: source.GetRawData()}
	if inline, ok := source.(*types.Base64Source); ok {
		key.mime = inline.MimeType
	}
	return key
}

// PrepareGatewayMedia resolves client media once before candidate preconversion.
// Both token estimation and converters subsequently read memory, including when
// the ordinary file loader initially chose its disk cache.
func PrepareGatewayMedia(c *gin.Context, request dto.Request) error {
	prepared := map[gatewayMediaSource]gatewayMediaContent{}
	_, nativeMessages := request.(*dto.ClaudeRequest)
	_, nativeResponses := request.(*dto.OpenAIResponsesRequest)
	for _, file := range request.GetTokenCountMeta().Files {
		source := file.Source
		if source == nil {
			continue
		}
		if (nativeMessages || nativeResponses) && file.FileType == types.FileTypeFile {
			if _, reference := source.(*types.URLSource); reference {
				continue
			}
		}
		identity := gatewayMediaIdentity(source)
		if _, exists := prepared[identity]; exists {
			continue
		}
		cached, err := LoadFileSource(c, source, "preparing gateway media")
		if err != nil {
			return errors.New("could not prepare request media")
		}
		data, err := cached.GetBase64Data()
		if err != nil {
			return errors.New("could not prepare request media")
		}
		prepared[identity] = gatewayMediaContent{data, cached.MimeType}
		if !cached.IsDisk() {
			continue
		}
		memory := types.NewMemoryCachedData(data, cached.MimeType, cached.Size)
		memory.ImageConfig, memory.ImageFormat = cached.ImageConfig, cached.ImageFormat
		if err := cached.Close(); err != nil {
			return errors.New("could not prepare request media")
		}
		source.SetCache(memory)
		switch original := source.(type) {
		case *types.URLSource:
			c.Set(getContextCacheKey(original.URL), memory)
		case *types.Base64Source:
			c.Set(getBase64ContextCacheKey(identity.data, identity.mime), memory)
		}
	}
	c.Set(gatewayMediaContextKey, prepared)
	return nil
}

func gatewayConversionContext(ctx context.Context, resolveReferences bool) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	var prepared map[gatewayMediaSource]gatewayMediaContent
	if c, ok := ctx.(*gin.Context); ok {
		if value, exists := c.Get(gatewayMediaContextKey); exists {
			prepared, _ = value.(map[gatewayMediaSource]gatewayMediaContent)
		}
	}
	return relayconvert.WithMediaResolver(ctx, relayconvert.MediaResolver{
		GetBase64Data: func(_ context.Context, source types.FileSource, _ ...string) (string, string, error) {
			if content, exists := prepared[gatewayMediaIdentity(source)]; exists {
				return content.base64, content.mime, nil
			}
			if c, ok := ctx.(*gin.Context); ok && resolveReferences {
				references, _ := c.Get(gatewayNativeMediaReferencesKey)
				native, _ := references.(map[string]bool)
				if _, url := source.(*types.URLSource); url && native[source.GetRawData()] {
					data, mime, err := GetBase64Data(c, source, "preparing converted gateway media")
					if err != nil {
						return "", "", errors.New("could not prepare request media")
					}
					if prepared == nil {
						prepared = map[gatewayMediaSource]gatewayMediaContent{}
						c.Set(gatewayMediaContextKey, prepared)
					}
					prepared[gatewayMediaIdentity(source)] = gatewayMediaContent{data, mime}
					return data, mime, nil
				}
			}
			if inline, ok := source.(*types.Base64Source); ok {
				if inline.MimeType != "" && !strings.HasPrefix(inline.Base64Data, "data:") {
					return inline.Base64Data, inline.MimeType, nil
				}
				mime, data, err := DecodeBase64FileData(inline.Base64Data)
				return data, mime, err
			}
			return "", "", errors.New("request media was not prepared")
		},
		DecodeBase64FileData: DecodeBase64FileData,
	})
}
