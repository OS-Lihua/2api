package common

import (
	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

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
	OnBusinessOutput         func()
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
// The provider parser still owns the envelope and the counter values.
func NativeUsagePresence(body []byte, promptPath, completionPath, totalPath string) (bool, bool) {
	var present [3]bool
	for i, path := range []string{promptPath, completionPath, totalPath} {
		if path == "" {
			continue
		}
		raw := ojson.GetBytes(body, path).Raw
		if raw == "" || raw == "null" {
			continue
		}
		var count int64
		if common.UnmarshalJsonStr(raw, &count) != nil || count < 0 || count > 1<<31-1 {
			if i == 2 {
				return false, false
			}
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
