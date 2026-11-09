package volcengine

import (
	"fmt"
	"io"
	"math"

	"github.com/MoeForge/moego/ojson"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

type speechRequestBody struct{ io.Reader }

func (speechRequestBody) ProjectAudioRequest(body []byte, public dto.AudioRequest) (*dto.AudioRequest, error) {
	if public.HasSpeechConditioning() {
		return nil, fmt.Errorf("Volcengine speech cannot preserve the requested conditioning inputs")
	}
	for _, fields := range [][]byte{body, []byte(ojson.GetBytes(body, "request").Raw)} {
		var conditioning dto.AudioRequest
		if err := common.Unmarshal(fields, &conditioning); err != nil {
			return nil, err
		}
		if conditioning.HasSpeechConditioning() {
			return nil, fmt.Errorf("Volcengine speech cannot preserve the requested conditioning inputs")
		}
	}
	if len(public.MaxNewTokens) > 0 || ojson.GetBytes(body, "max_new_tokens").Exists() || ojson.GetBytes(body, "request.max_new_tokens").Exists() {
		return nil, fmt.Errorf("Volcengine speech cannot enforce an output token limit")
	}
	var wire VolcengineTTSRequest
	if err := common.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	if wire.Request.Model != public.Model || wire.Request.Text == "" || wire.Request.Operation != "submit" || wire.Audio.Rate != 24000 || wire.Audio.VoiceType == "" {
		return nil, fmt.Errorf("invalid Volcengine speech parameters")
	}
	if _, err := (speechRequestBody{}).MinimumAudioOutputTokens(body); err != nil {
		return nil, err
	}
	if public.SampleRate != nil && *public.SampleRate != wire.Audio.Rate {
		return nil, fmt.Errorf("Volcengine sample rate override cannot be preserved")
	}
	public.SampleRate = common.GetPointer(wire.Audio.Rate)
	public.Input, public.Voice = wire.Request.Text, wire.Audio.VoiceType
	switch wire.Audio.Encoding {
	case "mp3", "wav", "pcm":
		public.ResponseFormat = wire.Audio.Encoding
	case "ogg_opus":
		public.ResponseFormat = "opus"
	default:
		return nil, fmt.Errorf("unsupported Volcengine audio encoding")
	}
	return &public, nil
}

func (speechRequestBody) MinimumAudioOutputTokens(body []byte) (int, error) {
	var wire VolcengineTTSRequest
	if err := common.Unmarshal(body, &wire); err != nil {
		return 0, err
	}
	// V1 shares transports across voice families; select controls by exact voice ID.
	// https://www.volcengine.com/docs/6561/79823
	// https://www.volcengine.com/docs/doubao-voice/http-one-timecompositenon-streaming-interface-v1-not-recommended
	minSpeed, maxSpeed, largeModel := 1.0, 1.0, false
	switch wire.Audio.VoiceType {
	case "zh_male_M392_conversation_wvae_bigtts", "zh_female_cancan_mars_bigtts":
		minSpeed, maxSpeed, largeModel = 0.1, 2, true
	case "BV700_streaming":
		minSpeed, maxSpeed = 0.2, 3
	}
	speed := wire.Audio.SpeedRatio
	if !ojson.GetBytes(body, "audio.speed_ratio").Exists() {
		speed = 1
	}
	if math.IsNaN(speed) || math.IsInf(speed, 0) || speed < minSpeed || speed > maxSpeed {
		return 0, fmt.Errorf("Volcengine speech speed is unsupported for the selected voice")
	}
	if wire.Request.SilenceDuration == nil {
		if wire.Request.EnableTrailingSilenceAudio != nil && *wire.Request.EnableTrailingSilenceAudio {
			return 0, fmt.Errorf("Volcengine trailing silence requires an explicit duration")
		}
		return 0, nil
	}
	if !largeModel || wire.Request.EnableTrailingSilenceAudio == nil || !*wire.Request.EnableTrailingSilenceAudio {
		return 0, fmt.Errorf("Volcengine trailing silence requires a supported voice and enabled switch")
	}
	milliseconds := *wire.Request.SilenceDuration
	if math.IsNaN(milliseconds) || math.IsInf(milliseconds, 0) || milliseconds < 0 || milliseconds > 30_000 {
		return 0, fmt.Errorf("Volcengine trailing silence exceeds the request limit")
	}
	seconds := milliseconds / 1000
	tokens, clamp := common.AudioDurationTokens(seconds)
	if clamp != nil {
		return 0, fmt.Errorf("Volcengine silence duration exceeds the request limit")
	}
	return tokens, nil
}
