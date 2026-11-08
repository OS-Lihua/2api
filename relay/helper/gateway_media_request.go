package helper

import (
	"fmt"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/capability"
	"github.com/QuantumNous/new-api/pkg/protocol"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/gin-gonic/gin"

	"github.com/MoeForge/moego/ojson"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/shopspring/decimal"
)

// GatewayMediaRequest carries transient request data, never settlement data.
type GatewayMediaRequest struct {
	Protocol protocol.Type
	Request  dto.Request
	Input    billingexpr.RequestInput
}

// ParseGatewayMediaRequest validates all scalar multiplier paths before DTO
// parsing. Multipart files stay in the reusable request storage, outside probes.
func ParseGatewayMediaRequest(c *gin.Context) (*GatewayMediaRequest, error) {
	if c.Request.Method != http.MethodPost {
		return nil, fmt.Errorf("media requests require POST")
	}
	parsed := &GatewayMediaRequest{}
	switch c.Request.URL.Path {
	case "/v1/images/generations", "/v1/images/edits":
		parsed.Protocol = protocol.OpenAIImages
	case "/v1/audio/speech":
		parsed.Protocol = protocol.OpenAIAudioSpeech
	case "/v1/audio/transcriptions":
		parsed.Protocol = protocol.OpenAIAudioTranscription
	default:
		return nil, fmt.Errorf("unsupported media endpoint")
	}
	var body []byte
	if strings.HasPrefix(c.GetHeader("Content-Type"), "multipart/form-data") {
		form, err := common.ParseMultipartFormReusable(c)
		if err != nil {
			return nil, err
		}
		c.Request.MultipartForm = form
		c.Set("gateway_media_multipart", true)
		body, err = GatewayMediaMultipartInput(form)
		if err != nil {
			return nil, err
		}
	} else {
		storage, err := common.GetBodyStorage(c)
		if err != nil {
			return nil, err
		}
		body, err = storage.Bytes()
		if err != nil {
			return nil, err
		}
	}
	if err := ValidateGatewayMediaMultipliers(body); err != nil {
		return nil, err
	}
	mode := relayconstant.Path2RelayMode(c.Request.URL.Path)
	if parsed.Protocol == protocol.OpenAIImages {
		request, err := GetAndValidOpenAIImageRequest(c, mode)
		if err != nil {
			return nil, err
		}
		count, err := request.ImageCount(false)
		if err != nil {
			return nil, err
		}
		request.N = common.GetPointer(uint(count))
		if c.Request.MultipartForm != nil {
			c.Request.MultipartForm.Value["n"] = []string{strconv.Itoa(count)}
		}
		parsed.Request = request
		parsed.Input.ImageCount = &count
	} else {
		request, err := GetAndValidAudioRequest(c, mode)
		if err != nil {
			return nil, err
		}
		if parsed.Protocol == protocol.OpenAIAudioSpeech && (request.Input == "" || request.Voice == "") {
			return nil, fmt.Errorf("speech input and voice are required")
		}
		if parsed.Protocol == protocol.OpenAIAudioTranscription && (c.Request.MultipartForm == nil || len(c.Request.MultipartForm.File["file"]) != 1) {
			return nil, fmt.Errorf("transcription requires exactly one audio file")
		}
		parsed.Request = request
	}
	parsed.Input.Body = body
	parsed.Input.Headers = make(map[string]string)
	for key, values := range c.Request.Header {
		parsed.Input.Headers[strings.ToLower(key)] = strings.Join(values, ",")
	}
	return parsed, nil
}

// GatewayMediaMultipartInput converts only form scalars to exact JSON probes.
// Files are deliberately excluded, and duplicate scalar fields are rejected.
func GatewayMediaMultipartInput(form *multipart.Form) ([]byte, error) {
	scalars := make(map[string]common.RawMessage, len(form.Value))
	for key, values := range form.Value {
		if len(values) != 1 {
			return nil, fmt.Errorf("media scalar %s must occur once", key)
		}
		value := values[0]
		switch strings.ToLower(key) {
		case "n", "batch_size", "batch_count", "num_images", "num_outputs", "samplecount", "sample_count", "max_images", "max_tokens", "max_completion_tokens", "max_output_tokens", "max_new_tokens", "maxoutputtokens", "duration", "seconds", "duration_seconds", "silence_duration", "speed", "width", "height", "output_quality", "output_compression", "parameters", "metadata", "stream", "watermark", "prompt_extend":
			scalars[key] = common.RawMessage(value)
		default:
			encoded, err := common.Marshal(value)
			if err != nil {
				return nil, err
			}
			scalars[key] = encoded
		}
	}
	return common.Marshal(scalars)
}

// MaxGatewayImageDimension bounds both sides of a requested image. It also
// bounds auto/provider resolution hints before a price probe can multiply them.
const MaxGatewayImageDimension = 4096

// ValidateGatewayMediaMultipliers validates the original and final wire JSON.
// It traverses provider parameters and metadata too; callers of multipart paths
// must pass their scalar fields as JSON before conversion and after overrides.
func ValidateGatewayMediaMultipliers(body []byte, declared ...capability.Limits) error {
	if len(declared) > 1 {
		return fmt.Errorf("multiple media capability limits")
	}
	if !ojson.ValidBytes(body) {
		return fmt.Errorf("invalid media request")
	}
	pending := []ojson.Result{ojson.ParseBytes(body)}
	for len(pending) > 0 {
		item := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if item.IsArray() {
			for value := range item.Values() {
				pending = append(pending, value)
			}
			continue
		}
		if !item.IsObject() {
			continue
		}
		for key, value := range item.All() {
			name := strings.ToLower(key.Str)
			var ceiling int64
			minimum := decimal.Zero
			switch name {
			case "n", "batch_size", "batch_count", "num_images", "num_outputs", "samplecount", "sample_count", "max_images":
				ceiling = dto.MaxImageN
			case "max_tokens", "max_completion_tokens", "max_output_tokens", "max_new_tokens", "maxoutputtokens":
				ceiling = maxTokensLimit
			case "duration", "seconds", "duration_seconds":
				ceiling = relaycommon.MaxTaskDurationSeconds
			case "silence_duration":
				ceiling = relaycommon.MaxTaskDurationSeconds * 1000
			case "width", "height":
				ceiling = MaxGatewayImageDimension
				minimum = decimal.NewFromInt(1)
			case "output_quality", "output_compression":
				ceiling = 100
			case "speed":
				ceiling = 4
				minimum = decimal.New(25, -2)
			case "quality":
				if value.Type != ojson.String {
					return fmt.Errorf("quality must be a supported string")
				}
				switch value.Str {
				case "", "auto", "low", "medium", "high", "standard", "hd":
					continue
				default:
					return fmt.Errorf("unsupported quality")
				}
			case "aspect_ratio", "aspectratio":
				if value.Type != ojson.String {
					return fmt.Errorf("%s must be a bounded aspect ratio", name)
				}
				if value.Str == "" || value.Str == "auto" {
					continue
				}
				widthText, heightText, found := strings.Cut(value.Str, ":")
				width, widthErr := strconv.ParseUint(widthText, 10, 32)
				height, heightErr := strconv.ParseUint(heightText, 10, 32)
				if !found || widthErr != nil || heightErr != nil || width == 0 || height == 0 || width > MaxGatewayImageDimension || height > MaxGatewayImageDimension {
					return fmt.Errorf("%s exceeds the image dimension limit", name)
				}
				continue
			case "size", "resolution", "imagesize":
				if value.Type != ojson.String {
					return fmt.Errorf("%s must be a bounded image size", name)
				}
				if value.Str == "" || value.Str == "auto" || value.Str == "1K" || value.Str == "2K" || value.Str == "4K" {
					continue
				}
				widthText, heightText, found := strings.Cut(value.Str, "x")
				width, widthErr := strconv.ParseUint(widthText, 10, 32)
				height, heightErr := strconv.ParseUint(heightText, 10, 32)
				if !found || widthErr != nil || heightErr != nil || width == 0 || height == 0 || width > MaxGatewayImageDimension || height > MaxGatewayImageDimension {
					return fmt.Errorf("%s exceeds the image dimension limit", name)
				}
				continue
			}
			if ceiling > 0 {
				if len(declared) == 1 {
					var limit *int64
					switch name {
					case "n", "batch_size", "batch_count", "num_images", "num_outputs", "samplecount", "sample_count", "max_images":
						limit = declared[0].MaxImages
					case "max_tokens", "max_completion_tokens", "max_output_tokens", "max_new_tokens", "maxoutputtokens":
						limit = declared[0].MaxOutputTokens
					case "duration", "seconds", "duration_seconds":
						limit = declared[0].MaxDurationSeconds
					case "silence_duration":
						if seconds := declared[0].MaxDurationSeconds; seconds != nil {
							if *seconds < 0 {
								return fmt.Errorf("invalid media duration limit")
							}
							milliseconds := min(int64(relaycommon.MaxTaskDurationSeconds), *seconds) * 1000
							limit = &milliseconds
						}
					}
					if limit != nil {
						ceiling = min(ceiling, *limit)
					}
				}
				if value.Type != ojson.Number {
					return fmt.Errorf("%s must be numeric", name)
				}
				if len(value.Raw) > 128 {
					return fmt.Errorf("%s exceeds numeric precision limits", name)
				}
				number, err := decimal.NewFromString(value.Raw)
				if err != nil || number.Exponent() > 128 || number.Exponent() < -128 {
					return fmt.Errorf("%s exceeds numeric precision limits", name)
				}
				if number.LessThan(minimum) || number.GreaterThan(decimal.NewFromInt(ceiling)) {
					return fmt.Errorf("%s is outside the media request limit", name)
				}
				if name != "speed" && name != "duration" && name != "seconds" && name != "duration_seconds" && name != "silence_duration" && !number.Equal(number.Truncate(0)) {
					return fmt.Errorf("%s must be an integer", name)
				}
				continue
			}
			if value.IsObject() || value.IsArray() {
				pending = append(pending, value)
			}
		}
	}
	return nil
}
