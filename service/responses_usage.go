package service

import (
	"cmp"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/tokenkit"
)

type responseContentKey struct {
	index   int
	summary bool
}

type responseOutputText struct {
	observedBytes int
	text          strings.Builder
}

type responseOutputState struct {
	nameSeen      bool
	name, payload responseOutputText
	index         int
	hasIndex      bool
	content       map[responseContentKey]*responseOutputText
}

type nativeInputPresence struct{ text, audio, image, cached, cacheWrite bool }

// ResponsesUsageAccumulator owns the accounting facts for one Responses stream.
// HTTP SSE and WebSocket transports feed the same events into it, then settle
// Finish's usage through the normal text billing path, including interrupted
// streams. Observe and Finish must be called by the same stream owner.
type ResponsesUsageAccumulator struct {
	retainedBytes, retainedLimit         int
	unretainedOutputTokens               int
	budgetErr                            error
	completionReported                   bool
	info                                 *relaycommon.RelayInfo
	usage                                *dto.Usage
	outputBytes                          int
	outputOrder                          []*responseOutputState
	outputs                              map[string]*responseOutputState
	terminalOutputSeen                   bool
	delivered                            *ResponsesUsageAccumulator
	imageCounter                         relaycommon.ImageGenerationCallCounter
	itemBytes                            int
	reportedInputDetails                 dto.InputTokenDetails
	inputPresence                        nativeInputPresence
	promptReported, inputDetailsReported bool
	imageInputReported                   bool
	imageCommitted                       bool
	started                              bool
	finished                             bool
}

func NewResponsesUsageAccumulator(info *relaycommon.RelayInfo) *ResponsesUsageAccumulator {
	maximum := constant.MaxRequestBodyMB
	if maximum <= 0 {
		maximum = 128
	}
	return &ResponsesUsageAccumulator{retainedLimit: maximum << 20, info: info, usage: &dto.Usage{}, outputs: map[string]*responseOutputState{}}
}

// Err reports a cumulative resource violation to the existing stream owner.
func (a *ResponsesUsageAccumulator) Err() error {
	if a.budgetErr != nil {
		return a.budgetErr
	}
	if a.delivered != nil {
		return a.delivered.Err()
	}
	return nil
}

func (a *ResponsesUsageAccumulator) recordText(output *responseOutputText, text string) {
	if text == "" {
		return
	}
	output.observedBytes += len(text)
	if a.budgetErr != nil {
		a.unretainedOutputTokens += tokenkit.Count(a.info.TokenizerModelName(), text)
		return
	}
	if len(text) > a.retainedLimit-a.retainedBytes {
		a.unretainedOutputTokens += tokenkit.Count(a.info.TokenizerModelName(), text)
		a.budgetErr = errors.New("Responses output memory budget exceeded")
		a.info.StreamStatus.RecordError("Responses output memory budget exceeded")
		return
	}
	a.retainedBytes += len(text)
	a.outputBytes += len(text)
	output.text.WriteString(text)
}

// DecodeResponsesStreamEvent decodes protocol framing independently from usage.
// A malformed counter cannot discard another valid counter or business output.
func DecodeResponsesStreamEvent(raw []byte) (*dto.ResponsesStreamResponse, error) {
	body := raw
	reported := ojson.GetBytes(raw, "response.usage")
	if reported.Exists() {
		document := ojson.NewDoc(raw)
		if err := document.Delete("response.usage"); err != nil {
			return nil, err
		}
		body = document.Bytes()
	}
	var event dto.ResponsesStreamResponse
	if err := common.Unmarshal(body, &event); err != nil {
		var identity struct {
			Response *struct {
				ID string `json:"id"`
			} `json:"response"`
		}
		if common.Unmarshal(body, &identity) != nil || identity.Response == nil || identity.Response.ID == "" || !reported.IsObject() {
			return nil, err
		}
		partial := &dto.ResponsesStreamResponse{Response: &dto.OpenAIResponsesResponse{ID: identity.Response.ID, Usage: &dto.Usage{}}}
		ApplyResponsesUsage(partial.Response.Usage, &dto.Usage{}, []byte(reported.Raw))
		return partial, err
	}
	if event.Response != nil && reported.IsObject() {
		event.Response.Usage = &dto.Usage{}
		ApplyResponsesUsage(event.Response.Usage, &dto.Usage{}, []byte(reported.Raw))
	}
	return &event, nil
}

// Observe feeds one decoded stream event. raw is the same event's wire bytes;
// the vendor tool-usage reader runs on it once, at the terminal event.
func (a *ResponsesUsageAccumulator) Observe(event *dto.ResponsesStreamResponse, raw []byte) {
	if a == nil || event == nil || a.finished {
		return
	}
	if event.Response != nil {
		a.info.ObserveResponseModel(event.Response.Model)
		if event.Response.Usage != nil {
			prompt, completion := relaycommon.NativeUsagePresence(raw, "response.usage.input_tokens", "response.usage.output_tokens", "")
			if len(raw) == 0 {
				a.promptReported = a.promptReported || event.Response.Usage.InputTokens > 0 || event.Response.Usage.PromptTokens > 0
				a.completionReported = a.completionReported || event.Response.Usage.OutputTokens > 0 || event.Response.Usage.CompletionTokens > 0
			}
			a.completionReported = a.completionReported || completion
			a.promptReported = a.promptReported || prompt
			text, _ := relaycommon.NativeUsagePresence(raw, "response.usage.input_tokens_details.text_tokens", "", "")
			a.inputPresence.text = a.inputPresence.text || text
			cached, _ := relaycommon.NativeUsagePresence(raw, "response.usage.input_tokens_details.cached_tokens", "", "")
			a.inputPresence.cached = a.inputPresence.cached || cached
			for _, path := range []string{"cached_creation_tokens", "cache_write_tokens", "cache_creation_input_tokens"} {
				if valid, _ := relaycommon.NativeUsagePresence(raw, "response.usage.input_tokens_details."+path, "", ""); valid {
					a.inputPresence.cacheWrite = true
				}
			}
			audio, _ := relaycommon.NativeUsagePresence(raw, "response.usage.input_tokens_details.audio_tokens", "", "")
			image, _ := relaycommon.NativeUsagePresence(raw, "response.usage.input_tokens_details.image_tokens", "", "")
			a.inputPresence.audio = a.inputPresence.audio || audio
			a.inputPresence.image = a.inputPresence.image || image
			for _, path := range []string{"cached_tokens", "cached_creation_tokens", "cache_write_tokens", "cache_creation_input_tokens", "cached_tokens_details.text_tokens", "cached_tokens_details.image_tokens", "cached_tokens_details.audio_tokens", "cache_write_tokens_details.ttl_12h_tokens", "cache_write_tokens_details.ttl_30m_tokens"} {
				if valid, _ := relaycommon.NativeUsagePresence(raw, "response.usage.input_tokens_details."+path, "", ""); valid {
					a.inputDetailsReported = true
				}
			}
			if valid, _ := relaycommon.NativeUsagePresence(raw, "response.usage.prompt_cache_hit_tokens", "", ""); valid {
				a.inputDetailsReported = true
			}
			a.inputDetailsReported = a.inputDetailsReported || audio || image || text
			if image || len(raw) == 0 && event.Response.Usage.PromptTokensDetails.ImageTokens > 0 {
				a.imageInputReported = true
			}
			// Rebuild derived totals from the latest valid reported details;
			// a later explicit zero must be able to clear an earlier estimate.
			a.usage.PromptTokensDetails = a.reportedInputDetails.Clone()
			if a.usage.InputTokensDetails != nil {
				details := a.reportedInputDetails.Clone()
				a.usage.InputTokensDetails = &details
			}
			ApplyResponsesUsage(a.usage, event.Response.Usage, []byte(ojson.GetBytes(raw, "response.usage").Raw))
			a.reportedInputDetails = a.usage.PromptTokensDetails.Clone()
			if subtypes := a.usage.PromptTokensDetails.CachedTokensDetails; subtypes != nil {
				changed := false
				if !a.inputPresence.cached {
					cached := 0
					for _, part := range []*int{subtypes.TextTokens, subtypes.ImageTokens, subtypes.AudioTokens} {
						if part != nil {
							cached += max(0, *part)
						}
					}
					if cached > a.usage.PromptTokensDetails.CachedTokens {
						a.usage.PromptTokensDetails.CachedTokens = cached
						changed = true
					}
				}
				for _, part := range []struct {
					reported bool
					total    *int
					subset   *int
				}{{a.inputPresence.text, &a.usage.PromptTokensDetails.TextTokens, subtypes.TextTokens}, {a.inputPresence.audio, &a.usage.PromptTokensDetails.AudioTokens, subtypes.AudioTokens}, {a.inputPresence.image, &a.usage.PromptTokensDetails.ImageTokens, subtypes.ImageTokens}} {
					if !part.reported && part.subset != nil && *part.subset > *part.total {
						*part.total = *part.subset
						changed = true
					}
				}
				if changed {
					if a.usage.InputTokensDetails != nil {
						details := a.usage.PromptTokensDetails.Clone()
						a.usage.InputTokensDetails = &details
					}
					if a.info.ResponseFacts != nil {
						a.info.ResponseFacts.UsageEstimated = true
					}
				}
			}
			if lifetimes := a.usage.PromptTokensDetails.CacheWriteTokensDetails; lifetimes != nil && !a.inputPresence.cacheWrite {
				total := lifetimes.TTL12hTokens + lifetimes.TTL30mTokens
				if total > a.usage.PromptTokensDetails.CacheCreationTokensTotal() {
					a.usage.PromptTokensDetails.CacheWriteTokens = total
					if a.usage.InputTokensDetails != nil {
						details := a.usage.PromptTokensDetails.Clone()
						a.usage.InputTokensDetails = &details
					}
					if a.info.ResponseFacts != nil {
						a.info.ResponseFacts.UsageEstimated = true
					}
				}
			}
			if facts := a.info.ResponseFacts; facts != nil {
				facts.RecordTokenDetailPresence(raw, "response.usage")
				prompt, completion := relaycommon.NativeUsagePresence(raw, "response.usage.input_tokens", "response.usage.output_tokens", "response.usage.total_tokens")
				facts.InputDetailsReported = a.inputDetailsReported
				if len(raw) == 0 {
					prompt, completion = a.promptReported, a.completionReported
				}
				reported := *a.usage
				facts.IndependentUsageReported = facts.IndependentUsageReported || a.inputDetailsReported
				for _, path := range []string{"text_tokens", "audio_tokens", "image_tokens", "reasoning_tokens"} {
					if valid, _ := relaycommon.NativeUsagePresence(raw, "response.usage.output_tokens_details."+path, "", ""); valid {
						facts.IndependentUsageReported = true
					}
				}
				facts.RecordUsage(a.usage, &reported, prompt, completion)
			}
		}
	}
	a.started = true
	ObserveResponsesOutcome(a.info, event)
	switch event.Type {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		if event.Response != nil && !a.terminalOutputSeen {
			for i, output := range event.Response.Output {
				a.rememberOutputItem(&output)
				a.observeOutputItem(&output, &i)
			}
			a.terminalOutputSeen = true
		}
		// Vendor counts are cumulative on the terminal event and replace the
		// web_search_call items counted from output_item.done.
		a.info.ApplyVendorToolUsage(raw)
		if a.imageCommitted {
			return
		}
		// Images that completed before any terminal, failed ones included,
		// were delivered and stay billable; Observe still skips unfinished
		// image items.
		if event.Response != nil {
			for i := range event.Response.Output {
				a.observeImageOutput(&event.Response.Output[i], &i)
			}
		}
		a.imageCounter.Commit(a.info)
		a.imageCommitted = true
	case "response.output_text.delta", "response.reasoning_summary_text.delta", "response.reasoning_text.delta", "response.refusal.delta":
		state := a.outputState(event.ItemID, event.OutputIndex)
		key := responseContentKey{}
		index := event.ContentIndex
		if event.Type == "response.reasoning_summary_text.delta" {
			key.summary = true
			index = event.SummaryIndex
		}
		if index != nil {
			key.index = *index
		}
		text := a.outputContent(state, key)
		a.recordText(text, event.Delta)
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		state := a.outputState(event.ItemID, event.OutputIndex)
		a.recordText(&state.payload, event.Delta)
	case "response.output_item.added":
		a.observeOutputItem(event.Item, event.OutputIndex)
	case dto.ResponsesOutputTypeItemDone:
		if event.Item == nil {
			return
		}
		a.rememberOutputItem(event.Item)
		a.observeOutputItem(event.Item, event.OutputIndex)
		switch event.Item.Type {
		case dto.BuildInCallWebSearchCall, dto.BuildInCallFileSearchCall, dto.BuildInCallFunctionCall:
			a.info.CountBillableToolCall(event.Item.Type, event.Item.Name)
		case dto.ResponsesOutputTypeImageGenerationCall:
			if !a.imageCommitted {
				a.observeImageOutput(event.Item, event.OutputIndex)
			}
		}
	}
}

func (a *ResponsesUsageAccumulator) observeImageOutput(item *dto.ResponsesOutput, index *int) {
	if item == nil || item.Type != dto.ResponsesOutputTypeImageGenerationCall {
		return
	}
	a.outputState(item.ID, index)
	if item.CallId != "" {
		a.outputState("image-call:"+item.CallId, nil)
	}
	a.outputState(fmt.Sprintf("image-result:%x", sha256.Sum256([]byte(item.Result))), nil)
	// Preserve completed procurement on the last frame; the counter keeps only
	// fixed-size digests and the owner stops reading when Err is non-nil.
	a.imageCounter.Observe(item, index)
}

func nativeOutputText(item *dto.ResponsesOutput) string {
	var text strings.Builder
	for _, content := range item.Content {
		text.WriteString(content.GetText())
	}
	return text.String()
}

func (a *ResponsesUsageAccumulator) rememberOutputItem(item *dto.ResponsesOutput) {
	if a.info.ResponsesInputFacts == nil || item == nil || item.ID == "" {
		return
	}
	if item.Type != "message" && item.Type != "function_call" && item.Type != "custom_tool_call" {
		return
	}
	for _, part := range item.Content {
		if part.Type != "output_text" && part.Type != "input_text" && part.Type != "text" && part.Type != "refusal" {
			return
		}
	}
	body, err := common.Marshal([]dto.ResponsesOutput{*item})
	if err != nil {
		return
	}
	request := dto.OpenAIResponsesRequest{Input: body}
	meta := request.GetTokenCountMeta()
	if len(meta.Files) > 0 {
		return
	}
	text := meta.CombineText
	if item.Type == "message" {
		text = nativeOutputText(item)
	}
	tokens := tokenkit.Count(a.info.TokenizerModelName(), text)
	toolCalls := NativeItemUsesTools(item.Type)
	if old := a.info.ResponsesItemFacts[item.ID]; old != nil {
		old.ToolCalls = old.ToolCalls || toolCalls
		if old.Usage.PromptTokens >= tokens {
			return
		}
	}
	maximum := constant.MaxRequestBodyMB
	if maximum <= 0 {
		maximum = 128
	}
	if a.info.ResponsesItemFacts[item.ID] == nil {
		if a.itemBytes+len(item.ID)+128 > maximum<<20 {
			return
		}
		a.itemBytes += len(item.ID) + 128
	}
	if a.info.ResponsesItemFacts == nil {
		a.info.ResponsesItemFacts = map[string]*relaycommon.ResponsesInputFacts{}
	}
	a.info.ResponsesItemFacts[item.ID] = &relaycommon.ResponsesInputFacts{Usage: &dto.Usage{PromptTokens: tokens, TotalTokens: tokens, PromptTokensDetails: dto.InputTokenDetails{TextTokens: tokens}}, ToolCalls: toolCalls}
}

func (a *ResponsesUsageAccumulator) outputState(id string, index *int) *responseOutputState {
	idKey, indexKey := "", ""
	if id != "" {
		idKey = "id:" + id
	}
	if index != nil {
		indexKey = fmt.Sprintf("index:%d", *index)
	}
	state := a.outputs[idKey]
	if state == nil {
		state = a.outputs[indexKey]
	}
	if state == nil && (index == nil || *index == 0) {
		state = a.outputs["unknown"]
	}
	if a.budgetErr != nil {
		if state != nil {
			return state
		}
		return &responseOutputState{}
	}
	keys := []string{idKey, indexKey}
	if idKey == "" && indexKey == "" {
		keys = []string{"unknown"}
	}
	additional := 0
	if state == nil {
		additional += 256
	}
	for _, key := range keys {
		if key != "" && a.outputs[key] == nil {
			additional += len(key) + 64
		}
	}
	if additional > a.retainedLimit-a.retainedBytes {
		a.budgetErr = errors.New("Responses output identity budget exceeded")
		a.info.StreamStatus.RecordError("Responses output identity budget exceeded")
		if state != nil {
			return state
		}
		return &responseOutputState{}
	}
	a.retainedBytes += additional
	if state == nil {
		state = &responseOutputState{}
		a.outputOrder = append(a.outputOrder, state)
	}
	if index != nil && !state.hasIndex {
		state.index, state.hasIndex = *index, true
	}
	if idKey != "" {
		a.outputs[idKey] = state
	}
	if indexKey != "" {
		a.outputs[indexKey] = state
	}
	if idKey == "" && indexKey == "" {
		a.outputs["unknown"] = state
	}
	return state
}

func (a *ResponsesUsageAccumulator) outputContent(state *responseOutputState, key responseContentKey) *responseOutputText {
	if text := state.content[key]; text != nil {
		return text
	}
	additional := 128
	if state.content == nil {
		additional += 256
	}
	if a.budgetErr != nil {
		return &responseOutputText{}
	}
	if additional > a.retainedLimit-a.retainedBytes {
		a.budgetErr = errors.New("Responses output identity budget exceeded")
		a.info.StreamStatus.RecordError("Responses output identity budget exceeded")
		return &responseOutputText{}
	}
	a.retainedBytes += additional
	if state.content == nil {
		state.content = map[responseContentKey]*responseOutputText{}
	}
	text := &responseOutputText{}
	state.content[key] = text
	return text
}

func (a *ResponsesUsageAccumulator) observeOutputItem(item *dto.ResponsesOutput, index *int) {
	if item == nil {
		return
	}
	if NativeItemUsesTools(item.Type) && a.info.ResponseFacts != nil {
		a.info.ResponseFacts.GeneratedToolCalls = true
	}
	state := a.outputState(item.ID, index)
	if item.Type != dto.BuildInCallFunctionCall && item.Type != "custom_tool_call" {
		for i, content := range item.Content {
			text := content.GetText()
			if text == "" {
				continue
			}
			output := a.outputContent(state, responseContentKey{index: i})
			if len(text) > output.observedBytes {
				a.recordText(output, text[output.observedBytes:])
			}
		}
		for i, summary := range item.Summary {
			if summary.Text == "" {
				continue
			}
			output := a.outputContent(state, responseContentKey{index: i, summary: true})
			if len(summary.Text) > output.observedBytes {
				a.recordText(output, summary.Text[output.observedBytes:])
			}
		}
		return
	}
	if !state.nameSeen && item.Name != "" {
		a.recordText(&state.name, item.Name)
		state.nameSeen = true
	}
	payload := item.Arguments
	if item.Type == "custom_tool_call" {
		payload = item.Input
	}
	if len(payload) > 0 {
		var text string
		if common.Unmarshal(payload, &text) != nil {
			text = string(payload)
		}
		if len(text) > state.payload.observedBytes {
			a.recordText(&state.payload, text[state.payload.observedBytes:])
		}
	}
}

// RecordDelivery observes only content accepted by the downstream writer.
// Reported counters describe upstream work and are excluded from the delivery
// estimate used to cap an interrupted sale.
func (a *ResponsesUsageAccumulator) RecordDelivery(event *dto.ResponsesStreamResponse, raw []byte) {
	if event == nil || a.info == nil || a.info.ResponseFacts == nil {
		return
	}
	if a.delivered == nil {
		info := &relaycommon.RelayInfo{TokenizerModel: a.info.TokenizerModelName(), ProviderMeta: &relaycommon.ProviderMeta{UpstreamModelName: a.info.GetUpstreamModelName()}, StreamStatus: relaycommon.NewStreamStatus(), ResponseFacts: &relaycommon.ResponseFacts{}}
		a.delivered = NewResponsesUsageAccumulator(info)
	}
	copy := *event
	if event.Response != nil {
		response := *event.Response
		response.Usage = nil
		copy.Response = &response
	}
	a.delivered.Observe(&copy, raw)
}

func (a *ResponsesUsageAccumulator) Finish() *dto.Usage {
	if a.finished {
		return a.usage
	}
	a.finished = true
	// A final image item can already have reached the client before the stream
	// disconnects, so completed tool usage is retained even without a terminal.
	if !a.imageCommitted {
		a.imageCounter.Commit(a.info)
		a.imageCommitted = true
	}
	// Keep stream chunks separate until each content part is complete, then
	// tokenize their canonical output order once, independent of arrival order.
	slices.SortStableFunc(a.outputOrder, func(left, right *responseOutputState) int {
		if left.hasIndex != right.hasIndex {
			if left.hasIndex {
				return -1
			}
			return 1
		}
		return cmp.Compare(left.index, right.index)
	})
	var output strings.Builder
	output.Grow(a.outputBytes)
	for _, state := range a.outputOrder {
		output.WriteString(state.name.text.String())
		output.WriteString(state.payload.text.String())
		state.name.text.Reset()
		state.payload.text.Reset()
		keys := slices.Collect(maps.Keys(state.content))
		slices.SortFunc(keys, func(left, right responseContentKey) int {
			if left.summary != right.summary {
				if left.summary {
					return 1
				}
				return -1
			}
			return cmp.Compare(left.index, right.index)
		})
		for _, key := range keys {
			text := state.content[key]
			output.WriteString(text.text.String())
			text.text.Reset()
		}
	}
	generatedText := output.String()
	facts := a.info.ResponseFacts
	if facts != nil {
		facts.SourceGeneratedText = generatedText
		delivered := 0
		if a.delivered != nil {
			delivered = a.delivered.Finish().CompletionTokens
			if text := a.delivered.info.ResponseFacts.SourceGeneratedText; text != "" || facts.GeneratedText == "" {
				facts.GeneratedText = text
			}
		}
		if delivered == 0 && facts.GeneratedText != "" {
			delivered = tokenkit.Count(a.info.TokenizerModelName(), facts.GeneratedText)
		}
		facts.GeneratedOutputTokens = &delivered
		facts.DeliveredOutputTokens = &delivered
		facts.DeliveredOutputDetails = dto.OutputTokenDetails{TextTokens: delivered}
	}
	completionReported := a.completionReported || facts != nil && facts.CompletionTokensReported
	generated := a.unretainedOutputTokens
	if generatedText != "" {
		generated += tokenkit.Count(a.info.TokenizerModelName(), generatedText)
	}
	if facts != nil && facts.DeliveredOutputTokens != nil {
		generated = max(generated, *facts.DeliveredOutputTokens)
	}
	if !completionReported {
		details := a.usage.CompletionTokenDetails
		generated = max(generated, max(details.TextTokens, details.ReasoningTokens)+details.AudioTokens+details.ImageTokens)
		a.usage.CompletionTokens = max(a.usage.CompletionTokens, generated)
	}
	if facts != nil {
		facts.SourceOutputTokens = &generated
		facts.SourceOutputObserved = facts.SourceOutputObserved || generated > 0 || facts.GeneratedToolCalls || facts.GeneratedAudioBytes > 0
	}
	// Upstream bills the prompt as soon as it starts generating, so a stream
	// that produced any event but no usage still owes its input tokens unless
	// upstream reported an explicit failure.
	billsPrompt := a.usage.CompletionTokens != 0 || (a.started && !a.info.StreamStatus.ResponseFailed())
	promptReported := a.promptReported || facts != nil && facts.PromptTokensReported
	if a.usage.PromptTokens == 0 && billsPrompt && !promptReported {
		a.usage.PromptTokens = a.info.GetEstimatePromptTokens()
	}
	if input := a.info.ResponsesInputFacts; input != nil && (billsPrompt || a.usage.PromptTokens > 0) && !a.imageInputReported && (facts == nil || !facts.PromptTokensReported || a.usage.PromptTokens > 0) {
		a.usage.PromptTokensDetails.ImageTokens = max(a.usage.PromptTokensDetails.ImageTokens, min(a.usage.PromptTokens, input.Usage.PromptTokensDetails.ImageTokens))
		if facts != nil && a.usage.PromptTokensDetails.ImageTokens > 0 {
			facts.UsageEstimated = true
		}
	}
	if !promptReported && (billsPrompt || a.usage.PromptTokens > 0 || a.inputDetailsReported) {
		var estimate *dto.Usage
		if input := a.info.ResponsesInputFacts; input != nil {
			estimate = input.Usage
		}
		a.usage.PromptTokens = max(a.usage.PromptTokens, inputTokenLowerBound(a.usage, estimate, a.inputPresence))
	}
	a.usage.TotalTokens = a.usage.PromptTokens + a.usage.CompletionTokens
	if a.usage.BillingUsage != nil {
		a.usage.BillingUsage = dto.CloneBillingUsageWithEstimatedCompletion(a.usage.BillingUsage, a.usage.CompletionTokens)
	}
	return a.usage
}

// inputTokenLowerBound combines disjoint modalities and overlapping cache
// facts without treating cached input as additional input.
func inputTokenLowerBound(usage, estimate *dto.Usage, presence nativeInputPresence) int {
	details := usage.PromptTokensDetails
	text, audio, image := details.TextTokens, details.AudioTokens, details.ImageTokens
	if !presence.text && estimate != nil {
		text = max(text, max(0, estimate.PromptTokens-estimate.PromptTokensDetails.AudioTokens-estimate.PromptTokensDetails.ImageTokens))
	}
	if subtypes := details.CachedTokensDetails; subtypes != nil {
		if !presence.text && subtypes.TextTokens != nil {
			text = max(text, *subtypes.TextTokens)
		}
		if !presence.audio && subtypes.AudioTokens != nil {
			audio = max(audio, *subtypes.AudioTokens)
		}
		if !presence.image && subtypes.ImageTokens != nil {
			image = max(image, *subtypes.ImageTokens)
		}
	}
	cached := max(details.CachedTokens, usage.PromptCacheHitTokens)
	if subtypes := details.CachedTokensDetails; subtypes != nil {
		count := 0
		for _, part := range []*int{subtypes.TextTokens, subtypes.ImageTokens, subtypes.AudioTokens} {
			if part != nil {
				count += max(0, *part)
			}
		}
		cached = max(cached, count)
	}
	return max(text+audio+image, cached+details.CacheCreationTokensTotal())
}

// ObserveResponsesOutcome records the protocol outcome of one Responses event
// on the stream status for health classification. Only codes and types are
// kept; messages never leave the event.
func ObserveResponsesOutcome(info *relaycommon.RelayInfo, event *dto.ResponsesStreamResponse) {
	if info == nil || info.StreamStatus == nil || event == nil {
		return
	}
	var responseStatus string
	if event.Response != nil {
		_ = common.Unmarshal(event.Response.Status, &responseStatus)
	}
	switch {
	case event.Type == "error" || event.Type == "response.failed" || event.Type == "response.error" || responseStatus == "failed":
		code, errorType := event.Code, ""
		if event.Response != nil {
			if oaiErr := event.Response.GetOpenAIError(); oaiErr != nil {
				if oaiErr.Code != nil {
					code = fmt.Sprint(oaiErr.Code)
				}
				errorType = oaiErr.Type
			}
		}
		info.StreamStatus.MarkFailed(code, errorType, 0)
	case event.Type == "response.incomplete" || responseStatus == "incomplete":
		reason := ""
		if event.Response != nil && event.Response.IncompleteDetails != nil {
			reason = event.Response.IncompleteDetails.Reason
		}
		info.StreamStatus.MarkIncomplete(reason)
	case event.Type == "response.cancelled" || event.Type == "response.canceled" || responseStatus == "cancelled":
		info.StreamStatus.MarkCancelled()
	case event.Type == "response.completed" || event.Type == "response.done" || responseStatus == "completed":
		info.StreamStatus.MarkCompleted()
	}
}

func ApplyResponsesUsage(dst *dto.Usage, src *dto.Usage, rawUsage ...[]byte) {
	if dst == nil || src == nil {
		return
	}
	incoming := relayconvert.NormalizeResponsesUsage(src)
	if len(rawUsage) > 0 && len(rawUsage[0]) > 0 {
		raw := rawUsage[0]
		// Validate before merging: an invalid later snapshot cannot replace
		// an earlier authoritative count or create an accounting failure.
		fields := map[string]*int{
			"input_tokens":                                     &dst.PromptTokens,
			"output_tokens":                                    &dst.CompletionTokens,
			"prompt_cache_hit_tokens":                          &dst.PromptCacheHitTokens,
			"input_tokens_details.cached_tokens":               &dst.PromptTokensDetails.CachedTokens,
			"input_tokens_details.cached_creation_tokens":      &dst.PromptTokensDetails.CachedCreationTokens,
			"input_tokens_details.cache_write_tokens":          &dst.PromptTokensDetails.CacheWriteTokens,
			"input_tokens_details.cache_creation_input_tokens": &dst.PromptTokensDetails.CacheCreationInputTokens,
			"input_tokens_details.text_tokens":                 &dst.PromptTokensDetails.TextTokens,
			"input_tokens_details.image_tokens":                &dst.PromptTokensDetails.ImageTokens,
			"input_tokens_details.audio_tokens":                &dst.PromptTokensDetails.AudioTokens,
			"output_tokens_details.text_tokens":                &dst.CompletionTokenDetails.TextTokens,
			"output_tokens_details.image_tokens":               &dst.CompletionTokenDetails.ImageTokens,
			"output_tokens_details.audio_tokens":               &dst.CompletionTokenDetails.AudioTokens,
			"output_tokens_details.reasoning_tokens":           &dst.CompletionTokenDetails.ReasoningTokens,
		}
		if ojson.GetBytes(raw, "input_tokens_details.cache_write_tokens_details").IsObject() {
			if dst.PromptTokensDetails.CacheWriteTokensDetails == nil {
				dst.PromptTokensDetails.CacheWriteTokensDetails = &dto.CacheWriteTokenDetails{}
			}
			fields["input_tokens_details.cache_write_tokens_details.ttl_12h_tokens"] = &dst.PromptTokensDetails.CacheWriteTokensDetails.TTL12hTokens
			fields["input_tokens_details.cache_write_tokens_details.ttl_30m_tokens"] = &dst.PromptTokensDetails.CacheWriteTokensDetails.TTL30mTokens
		}
		for path, destination := range fields {
			if valid, _ := relaycommon.NativeUsagePresence(raw, path, "", ""); valid {
				*destination = int(ojson.GetBytes(raw, path).Int())
			}
		}
		if ojson.GetBytes(raw, "input_tokens_details.cached_tokens_details").IsObject() {
			if dst.PromptTokensDetails.CachedTokensDetails == nil {
				dst.PromptTokensDetails.CachedTokensDetails = &dto.CachedTokenDetails{}
			}
			details := dst.PromptTokensDetails.CachedTokensDetails
			for path, destination := range map[string]**int{
				"input_tokens_details.cached_tokens_details.text_tokens":  &details.TextTokens,
				"input_tokens_details.cached_tokens_details.image_tokens": &details.ImageTokens,
				"input_tokens_details.cached_tokens_details.audio_tokens": &details.AudioTokens,
			} {
				if valid, _ := relaycommon.NativeUsagePresence(raw, path, "", ""); valid {
					count := int(ojson.GetBytes(raw, path).Int())
					*destination = &count
				}
			}
		}
		dst.InputTokens, dst.OutputTokens = dst.PromptTokens, dst.CompletionTokens
		dst.TotalTokens = dst.PromptTokens + dst.CompletionTokens
		if incoming.UsageSemantic != "" {
			dst.UsageSemantic = incoming.UsageSemantic
		}
		if incoming.UsageSource != "" {
			dst.UsageSource = incoming.UsageSource
		}
		if dst.InputTokensDetails != nil || ojson.GetBytes(raw, "input_tokens_details").IsObject() {
			details := dst.PromptTokensDetails.Clone()
			dst.InputTokensDetails = &details
		}
		if dst.OutputTokensDetails != nil || ojson.GetBytes(raw, "output_tokens_details").IsObject() {
			details := dst.CompletionTokenDetails
			dst.OutputTokensDetails = &details
		}
		if incoming.BillingUsage != nil || dst.BillingUsage != nil {
			dst.BillingUsage = dto.NewOpenAIResponsesBillingUsage(dst)
		}
	} else {
		if src.InputTokensDetails != nil {
			details := src.InputTokensDetails.Clone()
			incoming.InputTokensDetails = &details
		}
		if src.OutputTokensDetails != nil {
			incoming.CompletionTokenDetails = *src.OutputTokensDetails
		}
		incoming.PromptCacheHitTokens = src.PromptCacheHitTokens
		dto.MergeUsageNonZero(dst, incoming)
	}
	outputDetails := dst.CompletionTokenDetails
	if outputDetails != (dto.OutputTokenDetails{}) || dst.OutputTokensDetails != nil {
		dst.OutputTokensDetails = &outputDetails
	}
}
