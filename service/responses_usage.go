package service

import (
	"errors"
	"fmt"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/tokenkit"
)

// ResponsesUsageAccumulator owns the accounting facts for one Responses stream.
// HTTP SSE and WebSocket transports feed the same events into it, then settle
// Finish's usage through the normal text billing path, including interrupted
// streams. Observe and Finish must be called by the same stream owner.
type responseOutputState struct {
	nameSeen    bool
	payloadSeen bool
	textSeen    bool
}

type ResponsesUsageAccumulator struct {
	retainedBytes, retainedLimit int
	budgetErr                    error
	discarded                    responseOutputState
	info                         *relaycommon.RelayInfo
	usage                        *dto.Usage
	outputText                   strings.Builder
	outputs                      map[string]*responseOutputState
	terminalOutputSeen           bool
	delivered                    *ResponsesUsageAccumulator
	imageCounter                 relaycommon.ImageGenerationCallCounter
	itemBytes                    int
	imageInputReported           bool
	imageCommitted               bool
	started                      bool
	finished                     bool
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

func (a *ResponsesUsageAccumulator) recordText(text string) {
	if a.budgetErr != nil {
		return
	}
	if len(text) > a.retainedLimit-a.retainedBytes {
		a.budgetErr = errors.New("Responses output memory budget exceeded")
		a.info.StreamStatus.RecordError("Responses output memory budget exceeded")
		return
	}
	a.retainedBytes += len(text)
	a.outputText.WriteString(text)
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
		return nil, err
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
			image, _ := relaycommon.NativeUsagePresence(raw, "response.usage.input_tokens_details.image_tokens", "", "")
			if image || len(raw) == 0 && event.Response.Usage.PromptTokensDetails.ImageTokens > 0 {
				a.imageInputReported = true
			}
			ApplyResponsesUsage(a.usage, event.Response.Usage, []byte(ojson.GetBytes(raw, "response.usage").Raw))
			if facts := a.info.ResponseFacts; facts != nil {
				prompt, completion := relaycommon.NativeUsagePresence(raw, "response.usage.input_tokens", "response.usage.output_tokens", "response.usage.total_tokens")
				reported := *a.usage
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
				state := a.outputState(output.ID, &i)
				if output.Type == dto.BuildInCallFunctionCall || output.Type == "custom_tool_call" {
					a.observeToolOutput(&output, &i)
				} else if !state.textSeen {
					a.recordText(relayconvert.ExtractOutputTextFromResponses(&dto.OpenAIResponsesResponse{Output: []dto.ResponsesOutput{output}}))
				}
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
				a.imageCounter.Observe(&event.Response.Output[i], &i)
			}
		}
		a.imageCounter.Commit(a.info)
		a.imageCommitted = true
	case "response.output_text.delta", "response.reasoning_summary_text.delta", "response.reasoning_text.delta", "response.refusal.delta":
		if event.Type == "response.output_text.delta" || event.Type == "response.refusal.delta" {
			a.outputState(event.ItemID, event.OutputIndex).textSeen = true
		}
		a.recordText(event.Delta)
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		a.outputState(event.ItemID, event.OutputIndex).payloadSeen = true
		a.recordText(event.Delta)
	case "response.output_item.added":
		a.observeToolOutput(event.Item, event.OutputIndex)
	case dto.ResponsesOutputTypeItemDone:
		if event.Item == nil {
			return
		}
		a.rememberOutputItem(event.Item)
		a.observeToolOutput(event.Item, event.OutputIndex)
		switch event.Item.Type {
		case dto.BuildInCallWebSearchCall, dto.BuildInCallFileSearchCall, dto.BuildInCallFunctionCall:
			a.info.CountBillableToolCall(event.Item.Type, event.Item.Name)
		case dto.ResponsesOutputTypeImageGenerationCall:
			if !a.imageCommitted {
				a.imageCounter.Observe(event.Item, event.OutputIndex)
			}
		}
	}
}

func (a *ResponsesUsageAccumulator) rememberOutputItem(item *dto.ResponsesOutput) {
	if a.info.ResponsesInputFacts == nil || item == nil || item.ID == "" {
		return
	}
	if item.Type != "message" && item.Type != "function_call" && item.Type != "custom_tool_call" {
		return
	}
	for _, part := range item.Content {
		if part.Type != "output_text" && part.Type != "input_text" {
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
	tokens := tokenkit.Count(a.info.TokenizerModelName(), meta.CombineText)
	if old := a.info.ResponsesItemUsage[item.ID]; old != nil && old.PromptTokens >= tokens {
		return
	}
	maximum := constant.MaxRequestBodyMB
	if maximum <= 0 {
		maximum = 128
	}
	if a.info.ResponsesItemUsage[item.ID] == nil {
		if a.itemBytes+len(item.ID)+128 > maximum<<20 {
			return
		}
		a.itemBytes += len(item.ID) + 128
	}
	if a.info.ResponsesItemUsage == nil {
		a.info.ResponsesItemUsage = map[string]*dto.Usage{}
	}
	a.info.ResponsesItemUsage[item.ID] = &dto.Usage{PromptTokens: tokens, TotalTokens: tokens, PromptTokensDetails: dto.InputTokenDetails{TextTokens: tokens}}
}

func (a *ResponsesUsageAccumulator) outputState(id string, index *int) *responseOutputState {
	idKey, indexKey := "", ""
	if id != "" {
		idKey = "id:" + id
	}
	if index != nil {
		indexKey = fmt.Sprintf("index:%d", *index)
	}
	if a.budgetErr != nil {
		return &a.discarded
	}
	keys := []string{idKey, indexKey}
	if idKey == "" && indexKey == "" {
		keys = []string{"unknown"}
	}
	additional := 0
	for _, key := range keys {
		if key != "" && a.outputs[key] == nil {
			additional += len(key) + 64
		}
	}
	if additional > a.retainedLimit-a.retainedBytes {
		a.budgetErr = errors.New("Responses output identity budget exceeded")
		a.info.StreamStatus.RecordError("Responses output identity budget exceeded")
		return &a.discarded
	}
	a.retainedBytes += additional
	state := a.outputs[idKey]
	if state == nil {
		state = a.outputs[indexKey]
	}
	if state == nil && (index == nil || *index == 0) {
		state = a.outputs["unknown"]
	}
	if state == nil {
		state = &responseOutputState{}
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

func (a *ResponsesUsageAccumulator) observeToolOutput(item *dto.ResponsesOutput, index *int) {
	if item == nil || item.Type != dto.BuildInCallFunctionCall && item.Type != "custom_tool_call" {
		return
	}
	state := a.outputState(item.ID, index)
	if !state.nameSeen && item.Name != "" {
		a.recordText(item.Name)
		state.nameSeen = true
	}
	payload := item.Arguments
	if item.Type == "custom_tool_call" {
		payload = item.Input
	}
	if !state.payloadSeen && len(payload) > 0 {
		var text string
		if common.Unmarshal(payload, &text) == nil {
			a.recordText(text)
		} else {
			a.outputText.Write(payload)
		}
		state.payloadSeen = true
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
	facts := a.info.ResponseFacts
	if facts != nil {
		facts.GeneratedText = a.outputText.String()
		delivered := 0
		if a.delivered != nil {
			delivered = a.delivered.Finish().CompletionTokens
		}
		facts.DeliveredOutputTokens = &delivered
		facts.DeliveredOutputDetails = dto.OutputTokenDetails{TextTokens: delivered}
	}
	if a.usage.CompletionTokens == 0 && (facts == nil || !facts.CompletionTokensReported) {
		if output := a.outputText.String(); output != "" {
			a.usage.CompletionTokens = tokenkit.Count(a.info.TokenizerModelName(), output)
		}
	}
	// Upstream bills the prompt as soon as it starts generating, so a stream
	// that produced any event but no usage still owes its input tokens unless
	// upstream reported an explicit failure.
	billsPrompt := a.usage.CompletionTokens != 0 || (a.started && !a.info.StreamStatus.ResponseFailed())
	if a.usage.PromptTokens == 0 && billsPrompt && (facts == nil || !facts.PromptTokensReported) {
		a.usage.PromptTokens = a.info.GetEstimatePromptTokens()
	}
	if input := a.info.ResponsesInputFacts; input != nil && (billsPrompt || a.usage.PromptTokens > 0) && !a.imageInputReported && (facts == nil || !facts.PromptTokensReported || a.usage.PromptTokens > 0) {
		a.usage.PromptTokensDetails.ImageTokens = min(a.usage.PromptTokens, input.Usage.PromptTokensDetails.ImageTokens)
		if facts != nil && a.usage.PromptTokensDetails.ImageTokens > 0 {
			facts.UsageEstimated = true
		}
	}
	a.usage.TotalTokens = a.usage.PromptTokens + a.usage.CompletionTokens
	if a.usage.BillingUsage != nil {
		a.usage.BillingUsage = dto.CloneBillingUsageWithEstimatedCompletion(a.usage.BillingUsage, a.usage.CompletionTokens)
	}
	return a.usage
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
