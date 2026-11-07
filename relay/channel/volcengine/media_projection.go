package volcengine

import (
	"fmt"
	"io"
	"math"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

type speechRequestBody struct{ io.Reader }

func (speechRequestBody) ProjectAudioRequest(body []byte, public dto.AudioRequest) (*dto.AudioRequest, error) {
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
	if math.IsNaN(wire.Request.SilenceDuration) || math.IsInf(wire.Request.SilenceDuration, 0) || wire.Request.SilenceDuration < 0 || wire.Request.SilenceDuration > relaycommon.MaxTaskDurationSeconds*1000 {
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
