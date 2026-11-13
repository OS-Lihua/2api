package textprotocol

import (
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/tokenkit"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// gatewayUsageAccumulator overlays only present upstream fields. A zero resets
// its field; an omitted field retains the preceding cumulative fact.
type gatewayUsageAccumulator struct {
	format        types.RelayFormat
	context       *gin.Context
	snapshot      map[string]any
	repairedCache *int
}

func (a *gatewayUsageAccumulator) Observe(body []byte) (*dto.Usage, error) {
	root := gjson.ParseBytes(body)
	if a.format == types.RelayFormatOpenAIResponses && root.Get("response").Exists() {
		root = root.Get("response")
	}
	if a.format == types.RelayFormatClaude && root.Get("message").Exists() {
		root = root.Get("message")
	}
	key := "usage"
	if a.format == types.RelayFormatGemini {
		key = "usageMetadata"
	}
	cacheReported := false
	if a.format == types.RelayFormatOpenAI {
		for _, path := range []string{"usage.prompt_tokens_details.cached_tokens", "usage.input_tokens_details.cached_tokens", "usage.cached_tokens", "usage.prompt_cache_hit_tokens", "timings.cache_n"} {
			cacheReported = cacheReported || gatewayValidCounter(root.Get(path))
		}
		for _, choice := range root.Get("choices").Array() {
			cacheReported = cacheReported || gatewayValidCounter(choice.Get("usage.cached_tokens"))
		}
	}
	raw := root.Get(key)
	if (!raw.Exists() || raw.Type == gjson.Null) && !cacheReported {
		return nil, nil
	}
	incoming := map[string]any{}
	if raw.Exists() && raw.Type != gjson.Null {
		object := relaycommon.SourceUsageObject(body, raw)
		if object != nil {
			incoming = object
		} else if !cacheReported {
			return nil, nil
		}
	}
	if a.snapshot == nil {
		a.snapshot = map[string]any{}
	}
	mergeGatewayUsageSnapshot(a.snapshot, incoming)
	var schema any
	switch a.format {
	case types.RelayFormatClaude:
		schema = &dto.ClaudeUsage{}
	case types.RelayFormatGemini:
		schema = &dto.GeminiUsageMetadata{}
	default:
		schema = &dto.Usage{}
	}
	for key, value := range a.snapshot {
		field, err := common.Marshal(map[string]any{key: value})
		if err != nil {
			return nil, err
		}
		if common.Unmarshal(field, schema) != nil {
			delete(a.snapshot, key)
		}
	}
	encoded, err := common.Marshal(a.snapshot)
	if err != nil {
		return nil, err
	}
	var usage *dto.Usage
	promptKey, completionKey := "prompt_tokens", "completion_tokens"
	switch a.format {
	case types.RelayFormatClaude:
		var native dto.ClaudeUsage
		if err := common.Unmarshal(encoded, &native); err != nil {
			return nil, err
		}
		promptKey, completionKey = "input_tokens", "output_tokens"
		billing := dto.NewClaudeMessagesBillingUsage(&native)
		usage, _ = billing.CanonicalUsage()
		if usage == nil {
			usage = &dto.Usage{UsageSemantic: dto.BillingUsageSemanticAnthropic, UsageSource: dto.BillingUsageSourceClaudeMessages, BillingUsage: billing}
		}
	case types.RelayFormatGemini:
		var native dto.GeminiUsageMetadata
		if err := common.Unmarshal(encoded, &native); err != nil {
			return nil, err
		}
		// Total is not a substitute for an independently present output count.
		native.TotalTokenCount = 0
		promptKey, completionKey = "promptTokenCount", "candidatesTokenCount"
		usage = relayconvert.UsageFromGeminiMetadata(&native, 0)
	default:
		var native dto.Usage
		if err := common.Unmarshal(encoded, &native); err != nil {
			return nil, err
		}
		if a.format == types.RelayFormatOpenAIResponses {
			promptKey, completionKey = "input_tokens", "output_tokens"
			usage = relayconvert.UsageFromResponsesUsage(&native)
		} else {
			channel.RepairProviderUsage(a.context, &native, body)
			usage = relayconvert.UsageFromChatUsage(&native)
			usage.PromptTokensDetails = native.PromptTokensDetails.Clone()
			if usage.PromptTokensDetails.CachedTokens < 0 || int64(usage.PromptTokensDetails.CachedTokens) > 1<<31-1 {
				usage.PromptTokensDetails.CachedTokens = 0
			}
			cached := gjson.GetBytes(encoded, "prompt_tokens_details.cached_tokens")
			if gatewayValidCounter(cached) {
				usage.PromptTokensDetails.CachedTokens = int(cached.Int())
			} else if !cacheReported && usage.PromptTokensDetails.CachedTokens == 0 && a.repairedCache != nil {
				usage.PromptTokensDetails.CachedTokens = *a.repairedCache
			}
			if cacheReported || usage.PromptTokensDetails.CachedTokens != 0 || a.repairedCache != nil {
				count := usage.PromptTokensDetails.CachedTokens
				a.repairedCache = &count
			}
		}
	}
	// Missing base counters may be bounded by independently reported subsets.
	// An explicit base zero remains authoritative even with inconsistent details.
	if _, present := a.snapshot[promptKey]; !present && a.format != types.RelayFormatClaude {
		details := usage.PromptTokensDetails
		usage.PromptTokens = max(usage.PromptTokens, details.CachedTokens+details.CacheCreationTokensTotal(), details.TextTokens+details.ImageTokens+details.AudioTokens)
	}
	if _, present := a.snapshot[completionKey]; !present {
		details := usage.CompletionTokenDetails
		known := details.TextTokens + details.ImageTokens + details.AudioTokens
		if a.format == types.RelayFormatGemini {
			known += details.ReasoningTokens
		} else {
			known = max(known, details.ReasoningTokens)
		}
		usage.CompletionTokens = max(usage.CompletionTokens, known)
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	if a.context != nil {
		if facts, ok := common.GetContextKeyType[*relaycommon.ResponseFacts](a.context, constant.ContextKeyResponseFacts); ok {
			facts.RecordTokenDetailPresence(encoded, "")
		}
	}
	return usage, nil
}

// CompletionTokenEstimate preserves independently reported Gemini thought output
// while estimating a missing candidate count from candidate text alone.
func (a *gatewayUsageAccumulator) CompletionTokenEstimate(output *gatewayGeneratedOutput, usage *dto.Usage, model string) int {
	estimate := output.TextTokenEstimate(model)
	if a.format != types.RelayFormatGemini {
		return estimate
	}
	if _, present := a.snapshot["candidatesTokenCount"]; present {
		return estimate
	}
	if _, present := a.snapshot["thoughtsTokenCount"]; !present {
		return estimate
	}
	candidates := 0
	for _, text := range output.geminiCandidates {
		candidates += tokenkit.Count(model, text.String())
	}
	reasoning := usage.CompletionTokenDetails.ReasoningTokens
	return max(candidates, usage.CompletionTokens-reasoning) + reasoning
}

func (a *gatewayUsageAccumulator) BaseCounters() (bool, bool) {
	prompt, completion := "prompt_tokens", "completion_tokens"
	switch a.format {
	case types.RelayFormatOpenAIResponses, types.RelayFormatClaude:
		prompt, completion = "input_tokens", "output_tokens"
	case types.RelayFormatGemini:
		prompt, completion = "promptTokenCount", "candidatesTokenCount"
	}
	_, hasPrompt := a.snapshot[prompt]
	_, hasCompletion := a.snapshot[completion]
	return hasPrompt, hasCompletion
}

// ReportedCounters identifies independently valid usage, including cache/audio
// details and explicit zero, without treating total as a base counter.
func (a *gatewayUsageAccumulator) ReportedCounters() bool {
	return a.repairedCache != nil || gatewayReportedCounters(a.snapshot)
}

func gatewayReportedCounters(value any) bool {
	switch fields := value.(type) {
	case map[string]any:
		for key, field := range fields {
			if _, numeric := field.(float64); numeric && strings.Contains(strings.ToLower(key), "token") && !strings.Contains(strings.ToLower(key), "total") {
				return true
			}
			if gatewayReportedCounters(field) {
				return true
			}
		}
	case []any:
		for _, field := range fields {
			if gatewayReportedCounters(field) {
				return true
			}
		}
	}
	return false
}

// NormalizedBody isolates invalid usage fields from the content DTO parser.
func (a *gatewayUsageAccumulator) NormalizedBody(body []byte) ([]byte, error) {
	key := "usage"
	if a.format == types.RelayFormatGemini {
		key = "usageMetadata"
	}
	if a.format == types.RelayFormatOpenAIResponses && gjson.GetBytes(body, "response").Exists() {
		key = "response." + key
	}
	if a.format == types.RelayFormatClaude && gjson.GetBytes(body, "message").Exists() {
		key = "message." + key
	}
	if !gjson.GetBytes(body, key).Exists() {
		return body, nil
	}
	snapshot := a.snapshot
	if snapshot == nil {
		snapshot = map[string]any{}
	}
	encoded, err := common.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	return sjson.SetRawBytes(body, key, encoded)
}

func gatewayValidCounter(value gjson.Result) bool {
	var count int64
	return value.Type == gjson.Number && common.UnmarshalJsonStr(value.Raw, &count) == nil && count >= 0 && count <= 1<<31-1
}

func mergeGatewayUsageSnapshot(current, incoming map[string]any) {
	for key, value := range incoming {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "token") && !strings.HasSuffix(lower, "details") || strings.HasSuffix(lower, "_requests") {
			encoded, err := common.Marshal(value)
			if err != nil || !gatewayValidCounter(gjson.ParseBytes(encoded)) {
				continue
			}
			current[key] = float64(gjson.ParseBytes(encoded).Int())
			continue
		}
		if strings.HasSuffix(lower, "details") {
			_, valid := value.([]any)
			if strings.Contains(key, "_") {
				_, valid = value.(map[string]any)
			}
			if !valid {
				continue
			}
		}
		if key == "cache_creation" || key == "server_tool_use" || key == "billing_usage" {
			if _, valid := value.(map[string]any); !valid {
				continue
			}
		}
		if fields, ok := value.(map[string]any); ok {
			nested, _ := current[key].(map[string]any)
			if nested == nil {
				nested = map[string]any{}
			}
			mergeGatewayUsageSnapshot(nested, fields)
			current[key] = nested
			continue
		}
		if values, ok := value.([]any); ok {
			previous, _ := current[key].([]any)
			for _, entry := range values {
				fields, ok := entry.(map[string]any)
				if !ok {
					continue
				}
				modality, valid := fields["modality"].(string)
				if !valid {
					continue
				}
				var merged map[string]any
				for _, old := range previous {
					if old, ok := old.(map[string]any); ok && old["modality"] == modality {
						merged = old
						break
					}
				}
				if merged == nil {
					merged = map[string]any{}
					previous = append(previous, merged)
				}
				mergeGatewayUsageSnapshot(merged, fields)
			}
			current[key] = previous
			continue
		}
		current[key] = value
	}
}
