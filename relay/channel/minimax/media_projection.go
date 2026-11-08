package minimax

import (
	"bytes"
	"fmt"

	"github.com/MoeForge/moego/ojson"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

type speechRequestBody struct{ *bytes.Reader }

func (speechRequestBody) ProjectAudioRequest(body []byte, public dto.AudioRequest) (*dto.AudioRequest, error) {
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
	public.Input, public.Voice = wire.Text, wire.VoiceSetting.VoiceID
	public.ResponseFormat = wire.AudioSetting.Format
	return &public, nil
}
