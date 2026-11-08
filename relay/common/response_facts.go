package common

import (
	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"strings"
)

// ChatUsagePresence reports bounded base counters independently. A reported
// zero remains authoritative even when the other counter needs estimation.
func ChatUsagePresence(body []byte) (prompt, completion bool) {
	return NativeUsagePresence(body, "usage.prompt_tokens", "usage.completion_tokens", "usage.total_tokens")
}

// ChatStreamBusinessText extracts generated content independently of delivery.
func ChatStreamBusinessText(body []byte) string {
	var text strings.Builder
	for _, choice := range gjson.GetBytes(body, "choices").Array() {
		text.WriteString(choice.Get("delta.content").String())
		if reasoning := choice.Get("delta.reasoning_content"); reasoning.Exists() {
			text.WriteString(reasoning.String())
		} else {
			text.WriteString(choice.Get("delta.reasoning").String())
		}
		text.WriteString(choice.Get("delta.refusal").String())
		text.WriteString(choice.Get("delta.audio.transcript").String())
		for _, tool := range choice.Get("delta.tool_calls").Array() {
			for _, field := range []string{"function.name", "function.arguments", "custom.name", "custom.input"} {
				text.WriteString(tool.Get(field).String())
			}
		}
	}
	return text.String()
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
	OutputLimitObserved      bool   `json:"-"`
	UnterminatedStreamData   []byte `json:"-"`
	UpstreamRetryAfter       string // Transient; only the parsed cooldown may enter records.
	OnBusinessOutput         func()
	CanFinishStream          func() bool `json:"-"`
	UpstreamErrorCode        string
	// StreamPrelude belongs only to this Attempt and is never persisted. Hold
	// metadata until business output commits its protocol and loss headers.
	StreamPrelude []byte              `json:"-"`
	StreamHeaders map[string][]string `json:"-"`
}

// RecordChatSource retains generated facts when a native transport fails after
// returning a decodable response. It does not confirm downstream delivery.
func (facts *ResponseFacts) RecordChatSource(response *dto.OpenAITextResponse) {
	var text strings.Builder
	for _, choice := range response.Choices {
		text.WriteString(choice.Message.StringContent())
		text.WriteString(choice.Message.GetReasoningContent())
		for _, call := range choice.Message.ParseToolCalls() {
			facts.SourceOutputObserved = true
			text.WriteString(call.Function.Name)
			text.WriteString(call.Function.Arguments)
			text.Write(call.Custom)
		}
	}
	facts.SourceGeneratedText = text.String()
	facts.SourceOutputObserved = facts.SourceOutputObserved || facts.SourceGeneratedText != ""
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

// SanitizeNativeUsage removes only invalid numeric usage fields before a
// provider DTO is decoded. Unrelated broken totals/details cannot erase bases.
func SanitizeNativeUsage(body []byte, path string) []byte {
	usage := gjson.GetBytes(body, path)
	if !usage.Exists() {
		return body
	}
	if !usage.IsObject() {
		body, _ = sjson.DeleteBytes(body, path)
		return body
	}
	paths := []string{path}
	for index := 0; index < len(paths); index++ {
		prefix := paths[index]
		for key, value := range gjson.GetBytes(body, prefix).Map() {
			field := prefix + "." + key
			name := strings.ToLower(key)
			if strings.HasSuffix(name, "tokens") || strings.HasSuffix(name, "count") {
				if valid, _ := NativeUsagePresence(body, field, "", ""); !valid {
					body, _ = sjson.DeleteBytes(body, field)
				}
				continue
			}
			if strings.HasSuffix(name, "_details") && !value.IsObject() {
				body, _ = sjson.DeleteBytes(body, field)
				continue
			}
			if value.IsObject() {
				paths = append(paths, field)
			}
		}
	}
	return body
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

// RecordNativeUsage retains independently valid base counters before decoding
// a provider envelope or reporting its transport error.
func (facts *ResponseFacts) RecordNativeUsage(usage *dto.Usage, body []byte, promptPath, completionPath string) {
	prompt, completion := NativeUsagePresence(body, promptPath, completionPath, "")
	reported := dto.Usage{}
	if prompt {
		reported.PromptTokens = int(gjson.GetBytes(body, promptPath).Int())
	}
	if completion {
		reported.CompletionTokens = int(gjson.GetBytes(body, completionPath).Int())
	}
	facts.RecordUsage(usage, &reported, prompt, completion)
}

// RecordChatUsage merges a native usage object with Chat counter names.
// Omitted detail fields retain their preceding snapshot; explicit zeros reset it.
func (facts *ResponseFacts) RecordChatUsage(usage *dto.Usage, body []byte, path string) error {
	raw := gjson.GetBytes(body, path)
	if !raw.IsObject() {
		return nil
	}
	prompt, completion := NativeUsagePresence(body, path+".prompt_tokens", path+".completion_tokens", "")
	paths := []string{path}
	for index := 0; index < len(paths) && !facts.IndependentUsageReported; index++ {
		prefix := paths[index]
		for key, value := range gjson.GetBytes(body, prefix).Map() {
			if prefix == path && (key == "prompt_tokens" || key == "completion_tokens" || key == "total_tokens") {
				continue
			}
			field := prefix + "." + key
			if value.IsObject() {
				paths = append(paths, field)
				continue
			}
			if strings.Contains(strings.ToLower(key), "token") {
				valid, _ := NativeUsagePresence(body, field, "", "")
				facts.IndependentUsageReported = facts.IndependentUsageReported || valid
			}
		}
	}
	previousPrompt, previousCompletion := usage.PromptTokens, usage.CompletionTokens
	if err := common.UnmarshalJsonStr(raw.Raw, usage); err != nil {
		return err
	}
	reported := *usage
	usage.PromptTokens, usage.CompletionTokens = previousPrompt, previousCompletion
	facts.RecordUsage(usage, &reported, prompt, completion)
	return nil
}

func (info *RelayInfo) TokenizerModelName() string {
	if info.TokenizerModel != "" {
		return info.TokenizerModel
	}
	return info.UpstreamModelName
}
