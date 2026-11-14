package dto

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/relaykit/types"
)

type AudioRequest struct {
	Model          string          `json:"model"`
	Input          string          `json:"input"`
	Prompt         *string         `json:"prompt,omitempty"`
	Voice          string          `json:"voice"`
	Instructions   string          `json:"instructions,omitempty"`
	ResponseFormat string          `json:"response_format,omitempty"`
	Speed          *float64        `json:"speed,omitempty"`
	StreamFormat   string          `json:"stream_format,omitempty"`
	Stream         *bool           `json:"stream,omitempty"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
	// vllm-omini
	SampleRate              *int            `json:"sample_rate,omitempty"`
	TaskType                json.RawMessage `json:"task_type,omitempty"`
	Language                json.RawMessage `json:"language,omitempty"`
	RefAudio                json.RawMessage `json:"ref_audio,omitempty"`
	RefText                 json.RawMessage `json:"ref_text,omitempty"`
	References              json.RawMessage `json:"references,omitempty"`
	RefAudio2               json.RawMessage `json:"ref_audio_2,omitempty"`
	RefAudioCodes           json.RawMessage `json:"ref_audio_codes,omitempty"`
	SpeakerEmbedding        json.RawMessage `json:"speaker_embedding,omitempty"`
	ExtraParams             json.RawMessage `json:"extra_params,omitempty"`
	XVectorOnlyMode         json.RawMessage `json:"x_vector_only_mode,omitempty"`
	MaxNewTokens            json.RawMessage `json:"max_new_tokens,omitempty"`
	InitialCodecChunkFrames json.RawMessage `json:"initial_codec_chunk_frames,omitempty"`
}

func (r *AudioRequest) HasSpeechConditioning() bool {
	if r.Instructions != "" {
		return true
	}
	for _, value := range []json.RawMessage{r.RefAudio, r.RefText, r.References, r.RefAudio2, r.RefAudioCodes, r.SpeakerEmbedding, r.ExtraParams} {
		if raw := strings.TrimSpace(string(value)); raw != "" && raw != "null" && raw != `""` {
			return true
		}
	}
	return false
}

func (r *AudioRequest) GetTokenCountMeta() *types.TokenCountMeta {
	meta := &types.TokenCountMeta{
		CombineText: r.Input,
		TokenType:   types.TokenTypeTextNumber,
	}
	if r.Prompt != nil {
		if meta.CombineText != "" {
			meta.CombineText += "\n"
		}
		meta.CombineText += *r.Prompt
	}
	if strings.Contains(r.Model, "gpt") {
		meta.TokenType = types.TokenTypeTokenizer
	}
	return meta
}

func (r *AudioRequest) IsStream(c *http.Request) bool {
	return r.StreamFormat == "sse" || r.Stream != nil && *r.Stream && r.StreamFormat != "audio"
}

func (r *AudioRequest) SetModelName(modelName string) {
	if modelName != "" {
		r.Model = modelName
	}
}

type AudioResponse struct {
	Text string `json:"text"`
}

type WhisperVerboseJSONResponse struct {
	Task     string    `json:"task,omitempty"`
	Language string    `json:"language,omitempty"`
	Duration float64   `json:"duration,omitempty"`
	Text     string    `json:"text,omitempty"`
	Segments []Segment `json:"segments,omitempty"`
}

type Segment struct {
	Id               int     `json:"id"`
	Seek             int     `json:"seek"`
	Start            float64 `json:"start"`
	End              float64 `json:"end"`
	Text             string  `json:"text"`
	Tokens           []int   `json:"tokens"`
	Temperature      float64 `json:"temperature"`
	AvgLogprob       float64 `json:"avg_logprob"`
	CompressionRatio float64 `json:"compression_ratio"`
	NoSpeechProb     float64 `json:"no_speech_prob"`
}
