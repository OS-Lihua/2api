package common

import "github.com/QuantumNous/new-api/common"

// ChatUsagePresence reports bounded base counters independently. A reported
// zero remains authoritative even when the other counter needs estimation.
func ChatUsagePresence(body []byte) (prompt, completion bool) {
	var response struct {
		Usage *struct {
			PromptTokens     *int64 `json:"prompt_tokens"`
			CompletionTokens *int64 `json:"completion_tokens"`
			TotalTokens      *int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if common.Unmarshal(body, &response) != nil || response.Usage == nil {
		return false, false
	}
	usage := response.Usage
	if usage.TotalTokens != nil && (*usage.TotalTokens < 0 || *usage.TotalTokens > 1<<31-1) {
		return false, false
	}
	return usage.PromptTokens != nil && *usage.PromptTokens >= 0 && *usage.PromptTokens <= 1<<31-1,
		usage.CompletionTokens != nil && *usage.CompletionTokens >= 0 && *usage.CompletionTokens <= 1<<31-1
}

// MediaUsagePresence reports the OpenAI Images/audio counters independently.
// Explicit zero is authoritative; malformed or out-of-range counters are absent.
func MediaUsagePresence(body []byte) (prompt, completion bool) {
	var response struct {
		Usage *struct {
			InputTokens      *int64 `json:"input_tokens"`
			OutputTokens     *int64 `json:"output_tokens"`
			PromptTokens     *int64 `json:"prompt_tokens"`
			CompletionTokens *int64 `json:"completion_tokens"`
			TotalTokens      *int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if common.Unmarshal(body, &response) != nil || response.Usage == nil {
		return false, false
	}
	usage := response.Usage
	if usage.TotalTokens != nil && (*usage.TotalTokens < 0 || *usage.TotalTokens > 1<<31-1) {
		return false, false
	}
	input, output := usage.InputTokens, usage.OutputTokens
	if input == nil {
		input = usage.PromptTokens
	}
	if output == nil {
		output = usage.CompletionTokens
	}
	return input != nil && *input >= 0 && *input <= 1<<31-1,
		output != nil && *output >= 0 && *output <= 1<<31-1
}

// ResponseFacts separates upstream-reported zero usage from a local estimate,
// and business output from protocol framing and keep-alive pings. It contains
// transient output only; callers must never persist GeneratedText.
type ResponseFacts struct {
	UsageReported            bool
	PromptTokensReported     bool
	CompletionTokensReported bool
	BusinessOutput           bool
	DownstreamWriteFailed    bool
	GeneratedText            string
	// ImageCount is the bounded payload count, including an empty response.
	// Nil means that an actual count is not yet known.
	ImageCount *int64
	// Media duration strings are finite decimal quantities, never money.
	AudioInputSeconds          *string
	AudioOutputSeconds         *string
	GeneratedAudioBytes        int64
	GeneratedImageCount        int64
	AudioInputDurationReported bool
}

func (info *RelayInfo) TokenizerModelName() string {
	if info.TokenizerModel != "" {
		return info.TokenizerModel
	}
	return info.UpstreamModelName
}
