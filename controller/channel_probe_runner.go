package controller

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// ChannelProbeCapabilities lists request features that can be observed with
// bounded synthetic requests. Results describe observations, not declarations.
var channelProbeCapabilities = []string{"streaming", "tool_calling", "reasoning", "structured_output", "caching", "input_text", "input_image", "input_audio", "input_video", "input_pdf", "output_text", "output_image", "output_audio", "output_video", "output_pdf", "context_length", "max_output_tokens", "max_images", "max_duration_seconds", "embedding_dimensions"}

// ProbeChannel uses the same Provider/Protocol composition as production calls.
// No caller headers, user identity, wallet or accounting state enter the call.
func probeChannel(ctx context.Context, record model.Channel, provider *model.Provider, result *model.ChannelProbeResult) (string, string) {
	feature := result.Capability
	request := &dto.GeneralOpenAIRequest{Model: record.UpstreamModelName, Messages: []dto.Message{{Role: "user", Content: "Reply with ok."}}, MaxTokens: common.GetPointer(uint(64))}
	switch feature {
	case "streaming":
		request.Stream = common.GetPointer(true)
	case "tool_calling":
		if common.UnmarshalJsonStr(`{"messages":[{"role":"user","content":"Call probe_echo with value ok."}],"tools":[{"type":"function","function":{"name":"probe_echo","description":"Echo the probe value","parameters":{"type":"object","properties":{"value":{"type":"string","enum":["ok"]}},"required":["value"],"additionalProperties":false}}}],"tool_choice":{"type":"function","function":{"name":"probe_echo"}}}`, request) != nil {
			return "failed", "request_invalid"
		}
	case "structured_output":
		request.Messages[0].Content = `Return JSON with probe equal to ok.`
		request.ResponseFormat = &dto.ResponseFormat{Type: "json_schema", JsonSchema: []byte(`{"name":"probe","strict":true,"schema":{"type":"object","properties":{"probe":{"type":"string","enum":["ok"]}},"required":["probe"],"additionalProperties":false}}`)}
	case "reasoning":
		request.ReasoningEffort = "low"
		request.MaxTokens = common.GetPointer(uint(2048))
		request.Messages[0].Content = "Think through this problem, then give only the answer: if three boxes contain 2, 4 and 8 items, how many items remain after moving half of each box into a fourth box?"
	case "caching":
		request.Messages[0].Content = []dto.MediaContent{{Type: "text", Text: strings.Repeat("This is a synthetic cache probe with no private information. ", 160), CacheControl: []byte(`{"type":"ephemeral"}`)}, {Type: "text", Text: "Reply with ok."}}
		request.PromptCacheKey = "gateway-capability-probe"
	}
	if _, task := record.Protocol.TaskPluginKey(); task {
		if feature == "caching" {
			status, reason := probeTaskChannel(ctx, record, provider, result, request)
			if status == "failed" {
				return status, reason
			}
		}
		return probeTaskChannel(ctx, record, provider, result, request)
	}
	apiType, supported := common.ChannelType2APIType(provider.Type)
	if !supported {
		return "inconclusive", "protocol_unavailable"
	}
	adaptor, err := relay.GetChannelAdaptor(apiType, record.Protocol)
	if err != nil {
		return "inconclusive", "protocol_unavailable"
	}
	key, index, keyErr := provider.GetNextEnabledKey()
	if keyErr != nil {
		return "failed", "credential_unavailable"
	}
	recipe, err := channelProbeRecipeFor(ctx, record, feature, request)
	if err != nil {
		return "inconclusive", "request_not_representable"
	}
	result.TestedValue = recipe.TestedValue
	// Provider metadata is copied from trusted configuration. The synthetic
	// request has a fixed budget even when configured overrides are applied.
	organization := ""
	if provider.OpenAIOrganization != nil {
		organization = *provider.OpenAIOrganization
	}
	info := &relaycommon.RelayInfo{ProviderMeta: &relaycommon.ProviderMeta{ChannelType: provider.Type, ChannelId: provider.Id, ChannelIsMultiKey: provider.ProviderInfo.IsMultiKey, ChannelMultiKeyIndex: index, ChannelBaseUrl: provider.GetBaseURL(), ApiType: apiType, ApiKey: key, Organization: organization, ChannelCreateTime: provider.CreatedTime, HeadersOverride: provider.GetHeaderOverride(), ChannelSetting: provider.GetSetting(), ChannelOtherSettings: provider.GetOtherSettings(), UpstreamModelName: record.UpstreamModelName}, DisablePing: true, OriginModelName: record.UpstreamModelName, RelayFormat: recipe.Format, RelayMode: recipe.Mode, IsStream: feature == "streaming", IsChannelTest: true}
	cleanRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", nil)
	if err != nil {
		return "failed", "request_invalid"
	}
	cleanRequest.Header.Set("Content-Type", recipe.ContentType)
	cleanRequest.Body = io.NopCloser(bytes.NewReader(recipe.Body))
	writer := &probeResponseWriter{header: make(http.Header)}
	c := &gin.Context{Request: cleanRequest, Writer: writer}
	logger.SuppressPayloadLogs(c)
	converted, err := adaptor.ConvertRequest(c, info, recipe.Request)
	if err != nil {
		return "inconclusive", "request_not_representable"
	}
	var encoded []byte
	if reader, ok := converted.(io.Reader); ok {
		encoded, err = io.ReadAll(io.LimitReader(reader, 1<<20))
	} else {
		encoded, err = common.Marshal(converted)
	}
	if err != nil {
		return "failed", "request_invalid"
	}
	info.Request = recipe.Request
	info.ParamOverride = provider.GetParamOverride()
	encoded, err = relaycommon.ApplyParamOverrideWithRelayInfo(encoded, info)
	if err != nil {
		return "failed", "request_invalid"
	}
	info.ParamOverride = nil
	if common.UnmarshalJsonStr(record.ParameterOverrides, &info.ParamOverride) != nil {
		return "failed", "request_invalid"
	}
	encoded, err = relaycommon.ApplyParamOverrideWithRelayInfo(encoded, info)
	maxCount := int64(1)
	if record.Protocol == protocol.OpenAIImages {
		maxCount = 2
	}
	if err != nil || !probeRequestWithinBudget(gjson.ParseBytes(encoded), maxCount) {
		return "failed", "request_invalid"
	}
	if recipe.Format == types.RelayFormatOpenAI && !probeHasOutputBudget(gjson.ParseBytes(encoded)) {
		return "failed", "request_invalid"
	}
	if helper.ExceedsMaxTokensLimit(request.MaxTokens, request.MaxCompletionTokens) {
		return "failed", "request_invalid"
	}
	attempts := 1
	if feature == "caching" {
		attempts = 2
	}
	for attempt := range attempts {
		upstream, err := adaptor.DoRequest(c, info, bytes.NewReader(encoded))
		if err != nil {
			if ctx.Err() != nil {
				return "failed", "request_timeout"
			}
			return "failed", "transport_failed"
		}
		response, ok := upstream.(*http.Response)
		if !ok || response == nil || response.Body == nil {
			return "failed", "response_invalid"
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
		response.Body.Close()
		if readErr != nil {
			return "failed", "response_interrupted"
		}
		if len(body) > 1<<20 {
			return "failed", "response_too_large"
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return "failed", "upstream_rejected"
		}
		replay := *response
		replay.Body = io.NopCloser(bytes.NewReader(body))
		bounded := *response
		bounded.Body = io.NopCloser(bytes.NewReader(body))
		repaired, repairErr := adaptor.FixResponse(c, info, &bounded)
		if repairErr != nil || repaired == nil || repaired.Body == nil {
			return "failed", "response_invalid"
		}
		body, readErr = io.ReadAll(io.LimitReader(repaired.Body, (1<<20)+1))
		repaired.Body.Close()
		if readErr != nil {
			return "failed", "response_interrupted"
		}
		if len(body) > 1<<20 {
			return "failed", "response_too_large"
		}
		response = repaired
		if attempt+1 < attempts {
			continue
		}
		writer.body.Reset()
		_, parseErr := adaptor.DoResponse(c, &replay, info)
		if parseErr != nil {
			return "failed", "response_invalid"
		}
		if writer.overflow {
			return "failed", "response_too_large"
		}
		normalizedKind := record.Protocol
		switch recipe.Format {
		case types.RelayFormatOpenAI:
			normalizedKind = protocol.OpenAIChat
		case types.RelayFormatEmbedding:
			normalizedKind = protocol.OpenAIEmbeddings
		}
		if feature == "streaming" {
			if info.StreamStatus != nil && info.StreamStatus.EndReason == relaycommon.StreamEndReasonEOF {
				status, _ := observeProbeStream(record.Protocol, response.Header.Get("Content-Type"), body)
				if status != "passed" {
					return "inconclusive", "behavior_not_observed"
				}
			}
			return observeProbeStream(normalizedKind, writer.Header().Get("Content-Type"), writer.body.Bytes())
		}
		observed := observeChannelProbe(recipe, normalizedKind, feature, writer.body.Bytes())
		// Usage evidence must come from the bounded upstream response, never
		// from a protocol handler's fallback token estimates.
		if provider.Type == constant.ChannelTypeOllama && record.Protocol == protocol.OpenAIChat && gjson.GetBytes(body, "message").IsObject() {
			native := gjson.ParseBytes(body)
			message := map[string]any{"content": native.Get("message.content").String(), "reasoning_content": native.Get("message.thinking").String()}
			usage := map[string]any{}
			for from, to := range map[string]string{"prompt_eval_count": "prompt_tokens", "eval_count": "completion_tokens"} {
				if value := native.Get(from); value.Type == gjson.Number {
					usage[to] = value.Value()
				}
			}
			body, _ = common.Marshal(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": native.Get("done_reason").String()}}, "usage": usage})
		}
		if feature == "caching" || feature == "reasoning" || feature == "context_length" || feature == "max_output_tokens" || feature == "output_image" && record.Protocol == protocol.GeminiGenerate {
			observed = observeChannelProbe(recipe, record.Protocol, feature, body)
		}
		if observed {
			if recipe.SampledLimit {
				return "inconclusive", "limit_sample_only"
			}
			return "passed", "verified"
		}
	}
	return "inconclusive", "behavior_not_observed"
}

// probeRequestWithinBudget checks multiplier fields after all overrides,
// including nested passthrough parameters, before any network operation.
func probeRequestWithinBudget(value gjson.Result, maxCount int64) bool {
	valid := true
	value.ForEach(func(key, item gjson.Result) bool {
		if item.IsObject() || item.IsArray() {
			valid = probeRequestWithinBudget(item, maxCount)
			return valid
		}
		switch key.String() {
		case "max_tokens", "max_completion_tokens", "max_output_tokens", "maxOutputTokens", "max_new_tokens", "maxTokens", "MaxTokens", "num_predict", "maxTokenCount":
			valid = item.Type == gjson.Number && item.Float() >= 1 && item.Float() <= 2048 && item.Float() == float64(item.Int())
		case "n", "batch_size", "batch_count", "num_images", "num_outputs", "sampleCount", "numberOfVideos", "number_of_videos", "num_videos", "video_count", "numberOfImages", "number_of_images", "numImages":
			valid = item.Type == gjson.Number && item.Float() >= 1 && item.Float() <= float64(maxCount) && item.Float() == float64(item.Int())
		case "candidateCount":
			valid = item.Type == gjson.Number && item.Float() == 1
		}
		return valid
	})
	return valid
}

// probeHasOutputBudget checks actual generation controls at known wire paths.
// A lookalike field inside arbitrary metadata must not substitute for a cap.
func probeHasOutputBudget(value gjson.Result) bool {
	for _, path := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens", "max_new_tokens", "maxTokens", "MaxTokens", "num_predict", "generationConfig.maxOutputTokens", "generation_config.max_output_tokens", "parameters.max_tokens", "parameters.max_new_tokens", "parameters.max_output_tokens", "parameters.maxOutputTokens", "parameter.chat.max_tokens", "inferenceConfig.maxTokens", "textGenerationConfig.maxTokenCount", "options.num_predict", "instances.0.max_tokens"} {
		limit := value.Get(path)
		if limit.Type == gjson.Number && limit.Float() >= 1 && limit.Float() <= 2048 && limit.Float() == float64(limit.Int()) {
			return true
		}
	}
	return false
}

// probeResponseWriter isolates upstream stream headers from the management
// response. Probe calls read the upstream body directly and never relay output.
type probeResponseWriter struct {
	bufferedResponseWriter
	header   http.Header
	maxBytes int
	overflow bool
}

func (w *probeResponseWriter) Flush()              {}
func (w *probeResponseWriter) Header() http.Header { return w.header }
func (w *probeResponseWriter) Write(data []byte) (int, error) {
	limit := w.maxBytes
	if limit == 0 {
		limit = 1 << 20
	}
	if w.body.Len()+len(data) > limit {
		w.overflow = true
		return 0, io.ErrShortWrite
	}
	return w.body.Write(data)
}
func (w *probeResponseWriter) WriteString(text string) (int, error) { return w.Write([]byte(text)) }

// observeProbeJSON checks evidence in the selected wire protocol. Usage is read
// only to observe caching/reasoning; it is never fabricated or billed.
func observeProbeJSON(kind protocol.Type, feature string, body []byte) bool {
	root := gjson.ParseBytes(body)
	var text, reasoning string
	var calls []gjson.Result
	var cached, reasoningTokens int64
	switch kind {
	case protocol.OpenAIChat:
		text = root.Get("choices.0.message.content").String()
		reasoning = root.Get("choices.0.message.reasoning_content").String() + root.Get("choices.0.message.reasoning").String()
		calls = root.Get("choices.0.message.tool_calls").Array()
		cached = root.Get("usage.prompt_tokens_details.cached_tokens").Int()
		reasoningTokens = root.Get("usage.completion_tokens_details.reasoning_tokens").Int()
	case protocol.OpenAIResponses:
		for _, output := range root.Get("output").Array() {
			switch output.Get("type").String() {
			case "message":
				for _, part := range output.Get("content").Array() {
					if part.Get("type").String() == "output_text" {
						text += part.Get("text").String()
					}
				}
			case "function_call":
				calls = append(calls, output)
			case "reasoning":
				reasoning += output.Get("summary.0.text").String()
			}
		}
		cached = root.Get("usage.input_tokens_details.cached_tokens").Int()
		reasoningTokens = root.Get("usage.output_tokens_details.reasoning_tokens").Int()
	case protocol.AnthropicMessages:
		for _, part := range root.Get("content").Array() {
			switch part.Get("type").String() {
			case "text":
				text += part.Get("text").String()
			case "thinking":
				reasoning += part.Get("thinking").String()
			case "tool_use":
				calls = append(calls, part)
			}
		}
		cached = root.Get("usage.cache_read_input_tokens").Int()
	case protocol.GeminiGenerate:
		for _, part := range root.Get("candidates.0.content.parts").Array() {
			if part.Get("thought").Bool() {
				reasoning += part.Get("text").String()
			} else {
				text += part.Get("text").String()
			}
			if part.Get("functionCall").Exists() {
				calls = append(calls, part.Get("functionCall"))
			}
		}
		cached = root.Get("usageMetadata.cachedContentTokenCount").Int()
		reasoningTokens = root.Get("usageMetadata.thoughtsTokenCount").Int()
	}
	switch feature {
	case "structured_output":
		return gjson.Valid(text) && gjson.Get(text, "probe").String() == "ok" && len(gjson.Parse(text).Map()) == 1
	case "reasoning":
		return strings.TrimSpace(reasoning) != "" || reasoningTokens > 0
	case "caching":
		return cached > 0
	case "tool_calling":
		for _, call := range calls {
			name := call.Get("name").String()
			args := call.Get("arguments").String()
			if kind == protocol.OpenAIChat {
				name = call.Get("function.name").String()
				args = call.Get("function.arguments").String()
			}
			if kind == protocol.AnthropicMessages {
				args = call.Get("input").Raw
			}
			if kind == protocol.GeminiGenerate {
				args = call.Get("args").Raw
			}
			if name == "probe_echo" && gjson.Valid(args) && gjson.Get(args, "value").String() == "ok" {
				return true
			}
		}
	}
	return false
}

func observeProbeStream(kind protocol.Type, contentType string, body []byte) (string, string) {
	if !strings.HasPrefix(contentType, "text/event-stream") {
		return "inconclusive", "behavior_not_observed"
	}
	observed, complete := false, false
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		payload, ok := strings.CutPrefix(scanner.Text(), "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "[DONE]" {
			complete = true
			continue
		}
		if !gjson.Valid(payload) {
			continue
		}
		item := gjson.Parse(payload)
		switch kind {
		case protocol.OpenAIChat:
			observed = observed || item.Get("choices.0.delta.content").String() != ""
			complete = complete || item.Get("choices.0.finish_reason").String() != ""
		case protocol.OpenAIResponses:
			observed = observed || (item.Get("type").String() == "response.output_text.delta" && item.Get("delta").String() != "")
			complete = complete || item.Get("type").String() == "response.completed"
		case protocol.AnthropicMessages:
			observed = observed || (item.Get("type").String() == "content_block_delta" && item.Get("delta.text").String() != "")
			complete = complete || item.Get("type").String() == "message_stop"
		case protocol.GeminiGenerate:
			observed = observed || item.Get("candidates.0.content.parts.0.text").String() != ""
			complete = complete || item.Get("candidates.0.finishReason").String() == "STOP"
		}
	}
	if scanner.Err() != nil {
		return "failed", "response_invalid"
	}
	if observed && complete {
		return "passed", "verified"
	}
	return "inconclusive", "behavior_not_observed"
}
