package common

import "github.com/QuantumNous/new-api/common"

// HasReportedChatUsage recognizes bounded base counters, including explicit
// zeros. Empty or incomplete usage objects require local estimation.
func HasReportedChatUsage(body []byte) bool {
	var response struct {
		Usage *struct {
			PromptTokens     *int64 `json:"prompt_tokens"`
			CompletionTokens *int64 `json:"completion_tokens"`
			TotalTokens      *int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if common.Unmarshal(body, &response) != nil || response.Usage == nil {
		return false
	}
	usage := response.Usage
	return usage.PromptTokens != nil && *usage.PromptTokens >= 0 && *usage.PromptTokens <= 1<<31-1 &&
		usage.CompletionTokens != nil && *usage.CompletionTokens >= 0 && *usage.CompletionTokens <= 1<<31-1 &&
		(usage.TotalTokens == nil || *usage.TotalTokens >= 0 && *usage.TotalTokens <= 1<<31-1)
}

// ResponseFacts separates upstream-reported zero usage from a local estimate,
// and business output from protocol framing and keep-alive pings. It contains
// transient output only; callers must never persist GeneratedText.
type ResponseFacts struct {
	UsageReported  bool
	BusinessOutput bool
	GeneratedText  string
}

func (info *RelayInfo) TokenizerModelName() string {
	if info.TokenizerModel != "" {
		return info.TokenizerModel
	}
	return info.UpstreamModelName
}
