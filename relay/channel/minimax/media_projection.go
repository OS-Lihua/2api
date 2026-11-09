package minimax

import (
	"bytes"
	"fmt"

	"github.com/MoeForge/moego/ojson"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
)

func (MiniMaxImageRequest) ProjectImageRequest(body []byte, public dto.ImageRequest) (*dto.ImageRequest, error) {
	var input dto.ImageRequest
	if err := common.Unmarshal(body, &input); err != nil {
		return nil, err
	}
	for _, request := range []dto.ImageRequest{public, input} {
		if request.Quality != "" {
			return nil, types.NewMediaParameterError("quality", "media_control_unsupported")
		}
		for _, value := range []common.RawMessage{request.Image, request.Images, request.Mask} {
			if len(value) > 0 && string(value) != "null" {
				return nil, fmt.Errorf("MiniMax image generation cannot preserve image or mask inputs")
			}
		}
	}
	if value := ojson.GetBytes(body, "stream"); value.Exists() && value.Raw != "null" {
		return nil, fmt.Errorf("MiniMax image wire does not support stream overrides")
	}
	var wire struct {
		Model          string `json:"model"`
		Prompt         string `json:"prompt"`
		N              *uint  `json:"n"`
		AspectRatio    string `json:"aspect_ratio"`
		ResponseFormat string `json:"response_format"`
		Width, Height  *int
	}
	if err := common.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	count := uint(1)
	if wire.N != nil {
		count = *wire.N
	}
	if count == 0 || count > 9 {
		return nil, types.NewMediaParameterError("n", "media_capability_limit_exceeded")
	}
	if wire.Model != public.Model || wire.Prompt == "" {
		return nil, fmt.Errorf("invalid MiniMax image parameters")
	}
	switch wire.AspectRatio {
	case "", "1:1", "16:9", "4:3", "3:2", "2:3", "3:4", "9:16", "21:9":
	default:
		return nil, fmt.Errorf("unsupported MiniMax image aspect ratio")
	}
	if wire.Width != nil || wire.Height != nil {
		if wire.Width == nil || wire.Height == nil {
			return nil, types.NewMediaParameterError("size", "media_format_unsupported")
		}
		if err := validateMiniMaxDimensions(*wire.Width, *wire.Height); err != nil {
			return nil, err
		}
	}
	if wire.AspectRatio == "" && wire.Width != nil {
		public.Size = fmt.Sprintf("%dx%d", *wire.Width, *wire.Height)
	} else {
		sizes := map[string]string{"": "1024x1024", "1:1": "1024x1024", "16:9": "1280x720", "4:3": "1152x864", "3:2": "1248x832", "2:3": "832x1248", "3:4": "864x1152", "9:16": "720x1280", "21:9": "1344x576"}
		public.Size = sizes[wire.AspectRatio]
	}
	switch wire.ResponseFormat {
	case "", "url":
		public.ResponseFormat = "url"
	case "base64":
		public.ResponseFormat = "b64_json"
	default:
		return nil, fmt.Errorf("unsupported MiniMax image response format")
	}
	public.Model, public.Prompt, public.N = wire.Model, wire.Prompt, &count
	return &public, nil
}

type speechRequestBody struct{ *bytes.Reader }

func (speechRequestBody) ProjectAudioRequest(body []byte, public dto.AudioRequest) (*dto.AudioRequest, error) {
	var conditioning dto.AudioRequest
	if err := common.Unmarshal(body, &conditioning); err != nil {
		return nil, err
	}
	if public.HasSpeechConditioning() || conditioning.HasSpeechConditioning() {
		return nil, fmt.Errorf("MiniMax speech cannot preserve the requested conditioning inputs")
	}
	if len(public.MaxNewTokens) > 0 || ojson.GetBytes(body, "max_new_tokens").Exists() {
		return nil, fmt.Errorf("MiniMax speech cannot enforce an output token limit")
	}
	var wire MiniMaxTTSRequest
	if err := common.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	if wire.Model != public.Model || wire.Text == "" || wire.OutputFormat != "hex" || wire.Stream {
		return nil, fmt.Errorf("invalid MiniMax speech parameters")
	}
	if wire.AudioSetting == nil || wire.AudioSetting.SampleRate != 24000 || wire.AudioSetting.Channel != 1 {
		return nil, fmt.Errorf("MiniMax speech requires mono audio at 24000 Hz")
	}
	if public.SampleRate != nil && *public.SampleRate != wire.AudioSetting.SampleRate {
		return nil, fmt.Errorf("MiniMax sample rate override cannot be preserved")
	}
	public.SampleRate = common.GetPointer(wire.AudioSetting.SampleRate)
	public.Input, public.Voice = wire.Text, wire.VoiceSetting.VoiceID
	public.ResponseFormat = wire.AudioSetting.Format
	return &public, nil
}
