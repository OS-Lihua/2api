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
	UpstreamErrorCode        string
}

// MarkBusinessOutput records accepted content and disarms first-byte budgets.
// Metadata and keep-alive writers must not call this method.
func (facts *ResponseFacts) MarkBusinessOutput() {
	facts.BusinessOutput = true
	if facts.OnBusinessOutput != nil {
		facts.OnBusinessOutput()
	}
}

func (info *RelayInfo) TokenizerModelName() string {
	if info.TokenizerModel != "" {
		return info.TokenizerModel
	}
	return info.UpstreamModelName
}
