package common

import (
	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

// ChatUsagePresence reports bounded base counters independently. A reported
// zero remains authoritative even when the other counter needs estimation.
func ChatUsagePresence(body []byte) (prompt, completion bool) {
	return NativeUsagePresence(body, "usage.prompt_tokens", "usage.completion_tokens", "usage.total_tokens")
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
	OnBusinessOutput           func()
}

// MarkBusinessOutput records accepted content and disarms first-byte budgets.
// Metadata and keep-alive writers must not call this method.
func (facts *ResponseFacts) MarkBusinessOutput() {
	facts.BusinessOutput = true
	if facts.OnBusinessOutput != nil {
		facts.OnBusinessOutput()
	}
}

// NativeUsagePresence applies the Chat counter bounds to provider field names.
// The provider parser still owns the envelope and the counter values. A bad
// total cannot invalidate independently valid input or output counters.
func NativeUsagePresence(body []byte, promptPath, completionPath, _ string) (bool, bool) {
	var present [2]bool
	for i, path := range []string{promptPath, completionPath} {
		if path == "" {
			continue
		}
		raw := ojson.GetBytes(body, path).Raw
		if raw == "" || raw == "null" {
			continue
		}
		var count int64
		if common.UnmarshalJsonStr(raw, &count) != nil || count < 0 || count > 1<<31-1 {
			continue
		}
		present[i] = true
	}
	return present[0], present[1]
}

// RecordUsage merges independently reported base counters, including explicit
// zero. Missing or invalid fields cannot erase an earlier valid stream counter.
func (facts *ResponseFacts) RecordUsage(usage, reported *dto.Usage, prompt, completion bool) {
	incoming := *reported
	if prompt {
		usage.PromptTokens = incoming.PromptTokens
		facts.PromptTokensReported = true
	} else if !facts.PromptTokensReported {
		usage.PromptTokens = 0
	}
	if completion {
		usage.CompletionTokens = incoming.CompletionTokens
		facts.CompletionTokensReported = true
	} else if !facts.CompletionTokensReported {
		usage.CompletionTokens = 0
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	facts.UsageReported = facts.PromptTokensReported && facts.CompletionTokensReported
}

func (info *RelayInfo) TokenizerModelName() string {
	if info.TokenizerModel != "" {
		return info.TokenizerModel
	}
	return info.UpstreamModelName
}
