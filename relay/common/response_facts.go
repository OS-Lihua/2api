package common

import (
	"bytes"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/capability"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/tokenkit"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ChatUsagePresence reports bounded base counters independently. A reported
// zero remains authoritative even when the other counter needs estimation.
func ChatUsagePresence(body []byte) (prompt, completion bool) {
	return NativeUsagePresence(body, "usage.prompt_tokens", "usage.completion_tokens", "usage.total_tokens")
}

// MediaUsagePresence reports the OpenAI Images/audio counters independently.
// Explicit zero is authoritative; malformed or out-of-range counters are absent.
func MediaUsagePresence(body []byte) (prompt, completion bool) {
	input, output := "usage.input_tokens", "usage.output_tokens"
	if raw := ojson.GetBytes(body, input).Raw; raw == "" || raw == "null" {
		input = "usage.prompt_tokens"
	}
	if raw := ojson.GetBytes(body, output).Raw; raw == "" || raw == "null" {
		output = "usage.completion_tokens"
	}
	return NativeUsagePresence(body, input, output, "")
}

// RealtimeResponseFacts is transient evidence for one native response. The
// lifecycle stores quantitative usage and frozen pricing, never this object.
type RealtimeResponseFacts struct {
	ID           string
	Usage        *dto.Usage
	Facts        *ResponseFacts
	Outcome      StreamOutcome
	StreamStatus *StreamStatus
}

// ResponsesInputFacts freezes billable input and reusable conversation quantities.
// Context excludes configuration that the Responses protocol does not inherit.
type ResponsesInputFacts struct {
	Usage           *dto.Usage
	Context         *dto.Usage
	ImageCount      int
	InputModalities []capability.Modality
	ToolCalls       bool
}

// ChatStreamBusinessText extracts generated content independently of delivery.
func ChatStreamBusinessText(body []byte) string {
	var text strings.Builder
	for _, choice := range gjson.GetBytes(body, "choices").Array() {
		text.WriteString(ChatStreamChoiceBusinessText(choice))
	}
	return text.String()
}

// SourceText accepts only an independently complete JSON string.
func SourceText(value gjson.Result) string {
	var text string
	if value.Type != gjson.String || common.UnmarshalJsonStr(value.Raw, &text) != nil {
		return ""
	}
	return text
}

// SourceReasoningText preserves an explicit empty primary and recovers a complete alias.
func SourceReasoningText(message gjson.Result) string {
	if value := message.Get("reasoning_content"); value.Type == gjson.String {
		var text string
		if common.UnmarshalJsonStr(value.Raw, &text) == nil {
			return text
		}
	}
	return SourceText(message.Get("reasoning"))
}

// ChatStreamChoiceBusinessText preserves the content within one independent output.
func ChatStreamChoiceBusinessText(choice gjson.Result) string {
	var text strings.Builder
	text.WriteString(SourceText(choice.Get("delta.content")))
	text.WriteString(SourceReasoningText(choice.Get("delta")))
	text.WriteString(SourceText(choice.Get("delta.refusal")))
	text.WriteString(SourceText(choice.Get("delta.audio.transcript")))
	text.WriteString(SourceText(choice.Get("delta.function_call.name")))
	text.WriteString(SourceText(choice.Get("delta.function_call.arguments")))
	for _, tool := range choice.Get("delta.tool_calls").Array() {
		for _, field := range []string{"function.name", "function.arguments", "custom.name", "custom.input"} {
			text.WriteString(SourceText(tool.Get(field)))
		}
	}
	return text.String()
}

// ResponseFacts separates upstream-reported zero usage from a local estimate,
// and business output from protocol framing and keep-alive pings. It contains
// transient source and delivered output only; callers must never persist either.
type ResponseFacts struct {
	UsageEstimated           bool
	UsageReported            bool
	InputDetailsReported     bool
	PromptTokensReported     bool
	CompletionTokensReported bool
	IndependentUsageReported bool
	BusinessOutput           bool
	TerminalDelivered        bool `json:"-"`
	ExpectedChoices          int  `json:"-"`
	DownstreamWriteFailed    bool
	GeneratedToolCalls       bool
	GeneratedAudioBytes      int64
	GeneratedText            string
	// ImageCount is the bounded payload count, including an empty response.
	// Nil means that an actual count is not yet known.
	ImageCount *int64
	// Media duration strings are finite decimal quantities, never money.
	AudioInputSeconds                  *string
	AudioOutputSeconds                 *string
	AudioOutputSampleRate              int
	AudioOutputChannels                int
	DeliveredAudioBytes                int64
	DeliveredAudioSeconds              *string
	GeneratedImageCount                int64
	DeliveredImageCount                int64
	AudioInputDurationReported         bool
	AudioInputDurationInvalid          bool
	AudioOutputDurationReported        bool
	AudioOutputDurationInvalid         bool
	AudioOutputTokensUnknown           bool
	AudioOutputTokensDerived           bool
	AudioOutputTokensReported          bool
	AudioInputTokensEstimated          int
	AudioInputTokensReported           bool
	TextInputTokensReported            bool
	TextInputTokensEstimated           int
	ImageOutputEstimationStarted       bool
	ImageOutputTokensEstimate          *int
	DeliveredImageOutputTokensEstimate *int
	ImageOutputTokensUnknown           bool
	ImageOutputTokensEstimated         bool
	ImageOutputSize                    string
	ImageOutputQuality                 string
	ImageInputTokensEstimated          int
	ImageInputTokensReported           bool
	ImageOutputTokensReported          bool
	SourceOutputDetails                dto.OutputTokenDetails
	SourceGeneratedText                string `json:"-"`
	SourceOutputObserved               bool   `json:"-"`
	SourceOutputTokens                 *int   `json:"-"`
	GeneratedOutputTokens              *int
	DeliveredOutputTokens              *int
	DeliveredOutputDetails             dto.OutputTokenDetails
	OutputLimitObserved                bool   `json:"-"`
	UnterminatedStreamData             []byte `json:"-"`
	UnterminatedStreamEvent            string `json:"-"`
	UpstreamRetryAfter                 string // Transient; only the parsed cooldown may enter records.
	OnBusinessOutput                   func()
	CanFinishStream                    func() bool `json:"-"`
	UpstreamErrorCode                  string
	// StreamPrelude belongs only to this Attempt and is never persisted. Hold
	// metadata until business output commits its protocol and loss headers.
	StreamPrelude []byte              `json:"-"`
	StreamHeaders map[string][]string `json:"-"`
}

// AudioPCMFrameBytes is the size of one complete signed 16-bit sample frame.
func (facts *ResponseFacts) AudioPCMFrameBytes() int64 {
	channels := int64(1)
	if facts != nil && facts.AudioOutputChannels > 0 && facts.AudioOutputChannels <= 1<<31-1 {
		channels = int64(facts.AudioOutputChannels)
	}
	return channels * 2
}

// AudioPCMBytesPerSecond uses the effective or reported signed 16-bit PCM
// format. OpenAI's default remains mono 24 kHz when no format was supplied.
func (facts *ResponseFacts) AudioPCMBytesPerSecond() int64 {
	rate := int64(24000)
	if facts != nil {
		if facts.AudioOutputSampleRate > 0 && facts.AudioOutputSampleRate <= 1<<31-1 {
			rate = int64(facts.AudioOutputSampleRate)
		}
	}
	return rate * facts.AudioPCMFrameBytes()
}

// RecordChatSource retains generated facts when a native transport fails after
// returning a decodable response. It does not confirm downstream delivery.
func (facts *ResponseFacts) RecordChatSource(response *dto.OpenAITextResponse, model string) {
	var text strings.Builder
	tokens := 0
	for _, choice := range response.Choices {
		var output strings.Builder
		output.WriteString(choice.Message.StringContent())
		output.WriteString(choice.Message.GetReasoningContent())
		for _, call := range choice.Message.ParseToolCalls() {
			facts.SourceOutputObserved = true
			output.WriteString(call.Function.Name)
			output.WriteString(call.Function.Arguments)
			output.Write(call.Custom)
		}
		candidate := output.String()
		text.WriteString(candidate)
		tokens += tokenkit.Count(model, candidate)
	}
	facts.SourceGeneratedText = text.String()
	facts.SourceOutputTokens = &tokens
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
		value := gjson.GetBytes(body, path)
		raw := value.Raw
		if !sourceNumberComplete(body, value) {
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
	if path == "" {
		usage = gjson.ParseBytes(body)
	}
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
		object := gjson.GetBytes(body, prefix)
		if prefix == "" {
			object = gjson.ParseBytes(body)
		}
		for key, value := range object.Map() {
			field := key
			if prefix != "" {
				field = prefix + "." + key
			}
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
			if value.IsObject() && path != "" {
				paths = append(paths, field)
			}
		}
	}
	return body
}

// RecordTokenDetailPresence preserves independently valid modality reports,
// including zero, without assigning presence to an absent detail.
func (facts *ResponseFacts) RecordTokenDetailPresence(body []byte, usagePath string) {
	if facts == nil {
		return
	}
	if usagePath != "" {
		usagePath += "."
	}
	for _, detail := range []struct {
		path     string
		reported *bool
	}{
		{"prompt_tokens_details.audio_tokens", &facts.AudioInputTokensReported},
		{"input_tokens_details.audio_tokens", &facts.AudioInputTokensReported},
		{"input_token_details.audio_tokens", &facts.AudioInputTokensReported},
		{"prompt_tokens_details.image_tokens", &facts.ImageInputTokensReported},
		{"input_tokens_details.image_tokens", &facts.ImageInputTokensReported},
		{"input_token_details.image_tokens", &facts.ImageInputTokensReported},
		{"completion_tokens_details.audio_tokens", &facts.AudioOutputTokensReported},
		{"output_tokens_details.audio_tokens", &facts.AudioOutputTokensReported},
		{"completion_tokens_details.image_tokens", &facts.ImageOutputTokensReported},
		{"output_tokens_details.image_tokens", &facts.ImageOutputTokensReported},
	} {
		path := usagePath + detail.path
		present, _ := NativeUsagePresence(body, path, "", "")
		if present {
			raw := ojson.GetBytes(body, path)
			end := raw.Index + len(raw.Raw)
			if raw.Index < 0 || end > len(body) {
				continue
			}
			tail := bytes.TrimLeft(body[end:], " \r\n\t")
			present = len(tail) > 0 && (tail[0] == ',' || tail[0] == '}')
		}
		*detail.reported = *detail.reported || present
	}
	for _, group := range []struct {
		path         string
		audio, image *bool
	}{
		{"promptTokensDetails", &facts.AudioInputTokensReported, &facts.ImageInputTokensReported},
		{"candidatesTokensDetails", &facts.AudioOutputTokensReported, &facts.ImageOutputTokensReported},
	} {
		for _, detail := range ojson.GetBytes(body, usagePath+group.path).Array() {
			var count int64
			raw := detail.Get("tokenCount")
			if raw.Type != ojson.Number || common.UnmarshalJsonStr(raw.Raw, &count) != nil || count < 0 || count > 1<<31-1 {
				continue
			}
			switch detail.Get("modality").Str {
			case "AUDIO":
				*group.audio = true
			case "IMAGE":
				*group.image = true
			}
		}
	}
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
	if index := strings.LastIndexByte(promptPath, '.'); index >= 0 {
		facts.RecordTokenDetailPresence(body, promptPath[:index])
	}
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

// NativeTextSource extracts independently valid text without decoding unrelated
// native fields. Each candidate remains a separate tokenizer input.
func NativeTextSource(body []byte, choicesPath string, textPaths ...string) *dto.OpenAITextResponse {
	choices := []gjson.Result{gjson.ParseBytes(body)}
	if choicesPath != "" {
		choices = gjson.GetBytes(body, choicesPath).Array()
	}
	var source dto.OpenAITextResponse
	for i, choice := range choices {
		var text strings.Builder
		found := false
		for _, path := range textPaths {
			raw := choice.Get(path)
			var part string
			if raw.Type == gjson.String && common.UnmarshalJsonStr(raw.Raw, &part) == nil {
				text.WriteString(part)
				found = true
			}
		}
		if found {
			source.Choices = append(source.Choices, dto.OpenAITextResponseChoice{Index: i, Message: dto.Message{Role: "assistant", Content: text.String()}})
		}
	}
	return &source
}

// RecordNativeTextSource retains a native JSON source snapshot for procurement.
func (facts *ResponseFacts) RecordNativeTextSource(body []byte, choicesPath, textPath, model string) {
	source := NativeTextSource(body, choicesPath, textPath)
	if len(source.Choices) > 0 {
		facts.RecordChatSource(source, model)
	}
}

// SourceUsageObject retains independently complete fields from a usage object.
func SourceUsageObject(body []byte, value gjson.Result) map[string]any {
	known, ok := sourceUsageValue(body, value, 0)
	if !ok {
		return nil
	}
	fields, _ := known.(map[string]any)
	return fields
}

func sourceNumberComplete(body []byte, value gjson.Result) bool {
	end := value.Index + len(value.Raw)
	// A number at EOF may only be a prefix of an unreceived value.
	return value.Type == gjson.Number && end < len(body) && strings.ContainsRune(" \t\r\n,}]", rune(body[end]))
}

func sourceUsageValue(body []byte, value gjson.Result, depth int) (any, bool) {
	if value.Type == gjson.Number && !sourceNumberComplete(body, value) {
		return nil, false
	}
	var decoded any
	if common.UnmarshalUseNumber([]byte(value.Raw), &decoded) == nil {
		return decoded, true
	}
	if depth >= 128 {
		return nil, false
	}
	if value.IsObject() {
		fields := map[string]any{}
		value.ForEach(func(key, child gjson.Result) bool {
			if known, ok := sourceUsageValue(body, child, depth+1); ok {
				fields[key.Str] = known
			}
			return true
		})
		return fields, len(fields) > 0
	}
	if value.IsArray() {
		var fields []any
		value.ForEach(func(_, child gjson.Result) bool {
			if known, ok := sourceUsageValue(body, child, depth+1); ok {
				fields = append(fields, known)
			}
			return true
		})
		return fields, len(fields) > 0
	}
	return nil, false
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
	object := SourceUsageObject(body, raw)
	if object == nil {
		return nil
	}
	encoded, err := common.Marshal(object)
	if err != nil {
		return err
	}
	if err := common.Unmarshal(encoded, usage); err != nil {
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
