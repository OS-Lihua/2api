package service

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/MoeForge/moego/base64"
	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/tokenkit"
)

// RealtimeUsageResult contains one native response's quantitative facts. Raw
// events, audio and generated text never belong in a durable settlement intent.
type RealtimeUsageResult = relaycommon.RealtimeResponseFacts

type realtimeResponseUsage struct {
	info                                  *relaycommon.RelayInfo
	accumulator                           *ResponsesUsageAccumulator
	input                                 *dto.Usage
	audioBytes                            int64
	deliveredAudioBytes                   int64
	inputTextReported, outputTextReported bool
	audioReported                         bool
	inputAudioReported                    bool
	inputImageReported                    bool
	outputAudioFormat                     string
}

// RealtimeUsageAccumulator correlates native response IDs, including concurrent
// conversation:none responses. It reuses Responses counter normalization and
// presence handling; it does not reserve, price, post or publish anything.
// A single transport owner must call Observe and Finish.
type RealtimeUsageAccumulator struct {
	info          *relaycommon.RelayInfo
	active        map[string]*realtimeResponseUsage
	completed     map[string]bool
	identityBytes int
	identityLimit int
}

func NewRealtimeUsageAccumulator(info *relaycommon.RelayInfo) *RealtimeUsageAccumulator {
	maximum := constant.MaxRequestBodyMB
	if maximum <= 0 {
		maximum = 128
	}
	return &RealtimeUsageAccumulator{info: info, active: map[string]*realtimeResponseUsage{}, completed: map[string]bool{}, identityLimit: maximum << 20}
}

// Observe records one upstream event after the downstream write has either
// succeeded or failed. delivered never means merely buffered or parsed.
// input is the known native conversation/input estimate at response creation;
// declared output budgets are not input usage.
func (accumulator *RealtimeUsageAccumulator) Observe(raw []byte, input *dto.Usage, delivered bool) (*RealtimeUsageResult, error) {
	kind := ojson.GetBytes(raw, "type").String()
	if !strings.HasPrefix(kind, "response.") {
		return nil, nil
	}
	id := ojson.GetBytes(raw, "response.id").String()
	if id == "" {
		id = ojson.GetBytes(raw, "response_id").String()
	}
	if id == "" {
		return nil, errors.New("Realtime response event has no response identity")
	}
	if accumulator.completed[id] {
		return nil, nil
	}
	state := accumulator.active[id]
	if state == nil {
		// Retain completed IDs for the session so a late terminal can never
		// charge again. Bound this identity inventory by the existing body
		// memory budget instead of silently evicting financial identities.
		if len(id)+64 > accumulator.identityLimit-accumulator.identityBytes {
			return nil, errors.New("Realtime response identity budget exceeded")
		}
		accumulator.identityBytes += len(id) + 64
		info := &relaycommon.RelayInfo{TokenizerModel: accumulator.info.TokenizerModelName(),
			ProviderMeta: &relaycommon.ProviderMeta{UpstreamModelName: accumulator.info.GetUpstreamModelName()},
			StreamStatus: relaycommon.NewStreamStatus(), ResponseFacts: &relaycommon.ResponseFacts{}}
		info.ResponseFacts.OnBusinessOutput = func() {
			if accumulator.info.RealtimeBusinessOutput != nil {
				accumulator.info.RealtimeBusinessOutput(id)
			} else if accumulator.info.ResponseFacts != nil {
				accumulator.info.ResponseFacts.MarkBusinessOutput()
			}
		}
		if accumulator.info.RealtimeInputUsage != nil {
			input = accumulator.info.RealtimeInputUsage(id)
		}
		if input == nil {
			input = &dto.Usage{}
		}
		info.SetEstimatePromptTokens(input.PromptTokens)
		state = &realtimeResponseUsage{info: info, accumulator: NewResponsesUsageAccumulator(info), input: GatewayUsageSnapshot(input), outputAudioFormat: accumulator.info.OutputAudioFormat}
		if callback := accumulator.info.RealtimeOutputAudioFormat; callback != nil {
			if format := callback(id); format != "" {
				state.outputAudioFormat = format
			}
		}
		accumulator.active[id] = state
	}
	if kind == "response.created" {
		format := ojson.GetBytes(raw, "response.audio.output.format.type")
		if !format.Exists() {
			format = ojson.GetBytes(raw, "response.output_audio_format")
		}
		if format.Exists() {
			normalized, valid := NormalizeRealtimeAudioFormat(format.String())
			if !valid {
				return nil, errors.New("Unsupported Realtime response audio format")
			}
			state.outputAudioFormat = normalized
		}
	}
	document := ojson.NewDoc(raw)
	for _, field := range []struct{ old, current string }{
		{"response.usage.input_token_details", "response.usage.input_tokens_details"},
		{"response.usage.output_token_details", "response.usage.output_tokens_details"},
	} {
		if value := ojson.GetBytes(raw, field.old); value.Exists() {
			if err := document.SetRawBytes(field.current, []byte(value.Raw)); err != nil {
				return nil, err
			}
		}
	}
	if kind == "response.text.delta" || kind == "response.audio_transcript.delta" || kind == "response.output_audio_transcript.delta" {
		if err := document.Set("type", "response.output_text.delta"); err != nil {
			return nil, err
		}
	}
	if kind == "response.audio_transcript.done" || kind == "response.output_audio_transcript.done" {
		var index *int
		if value := ojson.GetBytes(raw, "output_index"); value.Exists() {
			parsed := int(value.Int())
			index = &parsed
		}
		transcript := ""
		complete := ojson.GetBytes(raw, "transcript").String()
		key := responseContentKey{}
		if contentIndex := ojson.GetBytes(raw, "content_index"); contentIndex.Exists() {
			key.index = int(contentIndex.Int())
		}
		seen := state.accumulator.outputContent(state.accumulator.outputState(ojson.GetBytes(raw, "item_id").String(), index), key).observedBytes
		if len(complete) > seen {
			transcript = complete[seen:]
		}
		if err := document.Set("type", "response.output_text.delta"); err != nil {
			return nil, err
		}
		if err := document.Set("delta", transcript); err != nil {
			return nil, err
		}
	}
	outputs := []string{"item"}
	for i := range ojson.GetBytes(raw, "response.output").Array() {
		outputs = append(outputs, fmt.Sprintf("response.output.%d", i))
	}
	for _, output := range outputs {
		for contentIndex, content := range ojson.GetBytes(raw, output+".content").Array() {
			if transcript := content.Get("transcript"); transcript.Type == ojson.String {
				path := fmt.Sprintf("%s.content.%d", output, contentIndex)
				if err := document.Set(path+".type", "output_text"); err != nil {
					return nil, err
				}
				if err := document.Set(path+".text", transcript.String()); err != nil {
					return nil, err
				}
			}
		}
	}
	if reason := ojson.GetBytes(raw, "response.status_details.reason"); reason.Type == ojson.String {
		if err := document.Set("response.incomplete_details.reason", reason.String()); err != nil {
			return nil, err
		}
	}
	if failure := ojson.GetBytes(raw, "response.status_details.error"); failure.IsObject() {
		if err := document.SetRawBytes("response.error", []byte(failure.Raw)); err != nil {
			return nil, err
		}
	}
	normalized := document.Bytes()
	event, err := DecodeRealtimeStreamEvent(normalized)
	if event != nil && event.Response != nil && event.Response.Usage != nil {
		if valid, _ := relaycommon.NativeUsagePresence(normalized, "response.usage.input_tokens_details.text_tokens", "", ""); valid {
			state.inputTextReported = true
		}
		if valid, _ := relaycommon.NativeUsagePresence(normalized, "response.usage.output_tokens_details.text_tokens", "", ""); valid {
			state.outputTextReported = true
		}
		if valid, _ := relaycommon.NativeUsagePresence(normalized, "response.usage.output_tokens_details.audio_tokens", "", ""); valid {
			state.audioReported = true
		}
		if valid, _ := relaycommon.NativeUsagePresence(normalized, "response.usage.input_tokens_details.image_tokens", "", ""); valid {
			state.inputImageReported = true
		}
		if valid, _ := relaycommon.NativeUsagePresence(normalized, "response.usage.input_tokens_details.audio_tokens", "", ""); valid {
			state.inputAudioReported = true
		}
	}
	if err != nil {
		if event != nil {
			state.accumulator.Observe(event, normalized)
		}
		return nil, err
	}
	if kind == "response.audio.delta" || kind == "response.output_audio.delta" {
		audio, err := base64.StdEncoding.DecodeString(event.Delta)
		if err != nil {
			return nil, errors.New("invalid Realtime audio delta")
		}
		if int64(len(audio)) > math.MaxInt64-state.audioBytes {
			return nil, errors.New("Realtime audio quantity overflow")
		}
		state.audioBytes += int64(len(audio))
		if delivered {
			state.deliveredAudioBytes += int64(len(audio))
		}
	}
	if !delivered {
		state.info.ResponseFacts.DownstreamWriteFailed = true
	}
	state.accumulator.Observe(event, normalized)
	if delivered {
		state.accumulator.RecordDelivery(event, normalized)
	}
	if delivered && NativeResponseHasBusinessOutput(event) {
		state.info.ResponseFacts.MarkBusinessOutput()
	}
	if err := state.accumulator.Err(); err != nil {
		return nil, err
	}
	if kind != "response.done" {
		return nil, nil
	}
	state.info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonDone, nil)
	result := accumulator.finish(id, state)
	return &result, nil
}

// DecodeRealtimeStreamEvent preserves native union metadata while exposing
// only quantity facts to the shared Responses counter.
func DecodeRealtimeStreamEvent(raw []byte) (*dto.ResponsesStreamResponse, error) {
	if limit := ojson.GetBytes(raw, "response.max_output_tokens"); limit.Type == ojson.String && limit.String() == "inf" {
		document := ojson.NewDoc(raw)
		if err := document.Delete("response.max_output_tokens"); err != nil {
			return nil, err
		}
		raw = document.Bytes()
	}
	return DecodeResponsesStreamEvent(raw)
}

func (accumulator *RealtimeUsageAccumulator) finish(id string, state *realtimeResponseUsage) RealtimeUsageResult {
	usage := state.accumulator.Finish()
	facts := state.info.ResponseFacts
	facts.GeneratedAudioBytes = state.audioBytes
	text := tokenkit.Count(state.info.TokenizerModelName(), facts.GeneratedText)
	audio := CountRealtimeAudioTokens(state.audioBytes, state.outputAudioFormat, false)
	generated := text + audio
	if facts.GeneratedOutputTokens != nil {
		generated = max(generated, *facts.GeneratedOutputTokens)
	}
	facts.GeneratedOutputDetails = usage.CompletionTokenDetails
	facts.GeneratedOutputDetails.AudioTokens = max(facts.GeneratedOutputDetails.AudioTokens, audio)
	facts.GeneratedOutputDetails.TextTokens = max(facts.GeneratedOutputDetails.TextTokens, text)
	facts.GeneratedOutputTokens = &generated
	delivered := CountRealtimeAudioTokens(state.deliveredAudioBytes, state.outputAudioFormat, false)
	if facts.DeliveredOutputTokens != nil {
		delivered += *facts.DeliveredOutputTokens
	}
	facts.DeliveredOutputTokens = &delivered
	facts.DeliveredOutputDetails.AudioTokens = CountRealtimeAudioTokens(state.deliveredAudioBytes, state.outputAudioFormat, false)
	estimated := !facts.UsageReported
	if !facts.CompletionTokensReported {
		usage.CompletionTokens = max(usage.CompletionTokens, generated)
	}
	if !facts.PromptTokensReported && (generated > 0 || state.audioBytes > 0) {
		usage.PromptTokens = state.input.PromptTokens
	}
	if !state.audioReported && audio > 0 && (!facts.CompletionTokensReported || usage.CompletionTokens > 0) {
		usage.CompletionTokenDetails.AudioTokens = min(audio, usage.CompletionTokens)
		estimated = true
	}
	if !state.inputAudioReported && state.input.PromptTokensDetails.AudioTokens > 0 && (!facts.PromptTokensReported || usage.PromptTokens > 0) {
		usage.PromptTokensDetails.AudioTokens = max(usage.PromptTokensDetails.AudioTokens, min(state.input.PromptTokensDetails.AudioTokens, usage.PromptTokens))
		estimated = true
	}
	if !state.inputImageReported && state.input.PromptTokensDetails.ImageTokens > 0 && (!facts.PromptTokensReported || usage.PromptTokens > 0) {
		usage.PromptTokensDetails.ImageTokens = max(usage.PromptTokensDetails.ImageTokens, min(state.input.PromptTokensDetails.ImageTokens, max(0, usage.PromptTokens-usage.PromptTokensDetails.AudioTokens)))
		estimated = true
	}
	facts.InputDetailsReported = facts.InputDetailsReported || state.inputTextReported || state.inputAudioReported || state.inputImageReported
	if !facts.PromptTokensReported && (usage.PromptTokens > 0 || generated > 0 || state.audioBytes > 0 || facts.InputDetailsReported) {
		usage.PromptTokens = max(usage.PromptTokens, inputTokenLowerBound(usage, state.input, nativeInputPresence{text: state.inputTextReported, audio: state.inputAudioReported, image: state.inputImageReported}))
	}
	if !state.inputTextReported {
		usage.PromptTokensDetails.TextTokens = max(0, usage.PromptTokens-usage.PromptTokensDetails.AudioTokens-usage.PromptTokensDetails.ImageTokens)
	}
	if !facts.CompletionTokensReported {
		knownText := usage.CompletionTokenDetails.TextTokens
		if !state.outputTextReported {
			knownText = max(knownText, text)
		}
		usage.CompletionTokens = max(usage.CompletionTokens, max(knownText, usage.CompletionTokenDetails.ReasoningTokens)+usage.CompletionTokenDetails.AudioTokens+usage.CompletionTokenDetails.ImageTokens)
	}
	if !state.outputTextReported {
		usage.CompletionTokenDetails.TextTokens = max(0, usage.CompletionTokens-usage.CompletionTokenDetails.AudioTokens-usage.CompletionTokenDetails.ImageTokens)
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	if estimated {
		usage.UsageSource = "estimated"
		facts.UsageReported = false
	} else {
		usage.UsageSource = "upstream"
	}
	delete(accumulator.active, id)
	accumulator.completed[id] = true
	return RealtimeUsageResult{ID: id, Usage: usage, Facts: facts, Outcome: state.info.StreamStatus.OutcomeSnapshot(), StreamStatus: state.info.StreamStatus}
}

// Finish preserves the evidence for every still-active response on disconnect.
// Calling it again produces no additional quantitative results.
func (accumulator *RealtimeUsageAccumulator) Finish() []RealtimeUsageResult {
	ids := make([]string, 0, len(accumulator.active))
	for id := range accumulator.active {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	results := make([]RealtimeUsageResult, 0, len(ids))
	for _, id := range ids {
		state := accumulator.active[id]
		if accumulator.info.StreamStatus != nil {
			state.info.StreamStatus.SetEndReason(accumulator.info.StreamStatus.OutcomeSnapshot().EndReason, nil)
		}
		results = append(results, accumulator.finish(id, state))
	}
	return results
}

// NativeResponseHasBusinessOutput recognizes delivered content across native streams.
func NativeResponseHasBusinessOutput(event *dto.ResponsesStreamResponse) bool {
	switch event.Type {
	case "response.output_text.delta", "response.text.delta", "response.audio.delta", "response.output_audio.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.delta", "response.refusal.delta":
		return event.Delta != ""
	case "response.output_item.added", "response.output_item.done":
		if event.Item != nil {
			if event.Item.Name != "" || len(event.Item.Arguments) > 0 || event.Item.Result != "" {
				return true
			}
			for _, part := range event.Item.Summary {
				if part.Text != "" {
					return true
				}
			}
			for _, content := range event.Item.Content {
				if content.Text != "" || content.Refusal != "" {
					return true
				}
			}
		}
	}
	if event.Response != nil {
		for _, output := range event.Response.Output {
			if output.Name != "" || len(output.Arguments) > 0 || output.Result != "" {
				return true
			}
			for _, part := range output.Summary {
				if part.Text != "" {
					return true
				}
			}
			for _, content := range output.Content {
				if content.Text != "" || content.Refusal != "" {
					return true
				}
			}
		}
	}
	return false
}
