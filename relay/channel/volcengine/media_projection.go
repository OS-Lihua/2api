package volcengine

import (
	"fmt"
	"io"
	"math"

	"github.com/MoeForge/moego/ojson"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

type speechRequestBody struct{ io.Reader }

func (speechRequestBody) ProjectAudioRequest(body []byte, public dto.AudioRequest) (*dto.AudioRequest, error) {
	if len(public.MaxNewTokens) > 0 || ojson.GetBytes(body, "max_new_tokens").Exists() || ojson.GetBytes(body, "request.max_new_tokens").Exists() {
		return nil, fmt.Errorf("Volcengine speech cannot enforce an output token limit")
	}
	var wire VolcengineTTSRequest
	if err := common.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	if wire.Request.Model != public.Model || wire.Request.Text == "" || wire.Request.Operation != "submit" || wire.Audio.Rate != 24000 {
		return nil, fmt.Errorf("invalid Volcengine speech parameters")
	}
	if math.IsNaN(wire.Audio.SpeedRatio) || math.IsInf(wire.Audio.SpeedRatio, 0) || wire.Audio.SpeedRatio < 0 || wire.Audio.SpeedRatio > 4 {
		return nil, fmt.Errorf("Volcengine speech speed exceeds the request limit")
	}
	if silence := wire.Request.SilenceDuration; silence != nil && (math.IsNaN(*silence) || math.IsInf(*silence, 0) || *silence < 0 || *silence > relaycommon.MaxTaskDurationSeconds*1000) {
		return nil, fmt.Errorf("Volcengine silence duration exceeds the request limit")
	}
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
	if wire.Request.SilenceDuration == nil {
		return 0, nil
	}
	seconds := *wire.Request.SilenceDuration / 1000
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds > relaycommon.MaxTaskDurationSeconds {
		return 0, fmt.Errorf("Volcengine silence duration exceeds the request limit")
	}
	tokens, clamp := common.AudioDurationTokens(seconds)
	if clamp != nil {
		return 0, fmt.Errorf("Volcengine silence duration exceeds the request limit")
	}
	return tokens, nil
}
