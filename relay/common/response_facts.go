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

// ResponseFacts separates upstream-reported zero usage from a local estimate,
// and business output from protocol framing and keep-alive pings. It contains
// transient source and delivered output only; callers must never persist either.
type ResponseFacts struct {
	UsageReported            bool
	PromptTokensReported     bool
	CompletionTokensReported bool
	IndependentUsageReported bool
	BusinessOutput           bool
	TerminalDelivered        bool `json:"-"`
	ExpectedChoices          int  `json:"-"`
	DownstreamWriteFailed    bool
	GeneratedText            string
	SourceGeneratedText      string `json:"-"`
	SourceOutputObserved     bool   `json:"-"`
	SourceOutputTokens       *int   `json:"-"`
	GeneratedOutputTokens    *int
	OnBusinessOutput         func()
	CanFinishStream          func() bool `json:"-"`
	UpstreamErrorCode        string
	// StreamPrelude belongs only to this Attempt and is never persisted. Hold
	// metadata until business output commits its protocol and loss headers.
	StreamPrelude []byte              `json:"-"`
	StreamHeaders map[string][]string `json:"-"`
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
