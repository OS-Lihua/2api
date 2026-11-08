package openai

import (
	"encoding/json"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

// mediaUsageCounters bounds each reported quantity independently. A malformed
// total or sibling cannot erase a valid base counter or explicit zero.
func mediaUsageCounters(body []byte, depth int) map[string]any {
	var fields map[string]json.RawMessage
	if common.Unmarshal(body, &fields) != nil {
		return nil
	}
	bounded := make(map[string]any, len(fields))
	for name, raw := range fields {
		if depth == 0 {
			switch name {
			case "prompt_tokens", "completion_tokens", "input_tokens", "output_tokens", "total_tokens", "prompt_cache_hit_tokens", "claude_cache_creation_5_m_tokens", "claude_cache_creation_1_h_tokens", "prompt_tokens_details", "completion_tokens_details", "input_token_details", "input_tokens_details", "output_tokens_details":
			default:
				continue
			}
		}
		if strings.HasSuffix(name, "_details") && depth < 2 {
			if nested := mediaUsageCounters(raw, depth+1); nested != nil {
				bounded[name] = nested
			}
			continue
		}
		var count int64
		if string(raw) != "null" && common.Unmarshal(raw, &count) == nil && count >= 0 && count <= 1<<31-1 {
			bounded[name] = count
		}
	}
	if depth == 0 {
		if details, ok := bounded["input_token_details"]; ok {
			bounded["input_tokens_details"] = details
			delete(bounded, "input_token_details")
		}
	}
	return bounded
}

func reportedMediaUsage(body []byte) *dto.Usage {
	usage := &dto.Usage{}
	var envelope struct {
		Usage json.RawMessage `json:"usage"`
	}
	if common.Unmarshal(body, &envelope) != nil {
		return usage
	}
	bounded := mediaUsageCounters(envelope.Usage, 0)
	encoded, err := common.Marshal(bounded)
	if err == nil {
		_ = common.Unmarshal(encoded, usage)
	}
	normalizeOpenAIUsage(usage)
	return usage
}

// mergeMediaTokenDetails preserves cumulative details omitted by later frames;
// an explicitly reported zero still resets its own counter.
func mergeMediaTokenDetails(usage *dto.Usage, body []byte) {
	var envelope struct {
		Usage json.RawMessage `json:"usage"`
	}
	if common.Unmarshal(body, &envelope) != nil {
		return
	}
	fields := mediaUsageCounters(envelope.Usage, 0)
	for _, alias := range []struct{ from, to string }{{"input_tokens_details", "prompt_tokens_details"}, {"output_tokens_details", "completion_tokens_details"}} {
		if detail, ok := fields[alias.from]; ok {
			fields[alias.to] = detail
		}
	}
	for name := range fields {
		if name != "prompt_tokens_details" && name != "completion_tokens_details" && name != "prompt_cache_hit_tokens" && name != "claude_cache_creation_5_m_tokens" && name != "claude_cache_creation_1_h_tokens" {
			delete(fields, name)
		}
	}
	encoded, err := common.Marshal(fields)
	if err == nil {
		_ = common.Unmarshal(encoded, usage)
	}
}

// mediaResponseEnvelope retains independently complete classification and usage
// fields from a truncated response, without treating them as a valid payload.
func mediaResponseEnvelope(body []byte) []byte {
	if ojson.ValidBytes(body) {
		return body
	}
	recovered := map[string]common.RawMessage{}
	for _, name := range []string{"usage", "error", "type", "code", "duration"} {
		if raw := ojson.GetBytes(body, name); raw.Exists() && ojson.Valid(raw.Raw) {
			recovered[name] = common.RawMessage(raw.Raw)
		}
	}
	if len(recovered) > 0 {
		if encoded, err := common.Marshal(recovered); err == nil {
			return encoded
		}
	}
	return body
}

// observeMediaError keeps structured classification facts without copying a
// Provider's message or other error details into downstream output or logs.
func observeMediaError(info *relaycommon.RelayInfo, body []byte, status int) *types.OpenAIError {
	var envelope struct {
		Type  json.RawMessage `json:"type"`
		Code  json.RawMessage `json:"code"`
		Error json.RawMessage `json:"error"`
	}
	if common.Unmarshal(body, &envelope) != nil {
		return nil
	}
	var fault *types.OpenAIError
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		var nested struct {
			Type json.RawMessage `json:"type"`
			Code json.RawMessage `json:"code"`
		}
		if common.Unmarshal(envelope.Error, &nested) == nil {
			var kind, code string
			_ = common.Unmarshal(nested.Type, &kind)
			_ = common.Unmarshal(nested.Code, &code)
			fault = &types.OpenAIError{Type: kind, Code: code}
		} else {
			kind := "unknown_error"
			var message string
			if common.Unmarshal(envelope.Error, &message) == nil {
				kind = "error"
			}
			fault = &types.OpenAIError{Type: kind}
		}
	} else {
		var kind, code string
		_ = common.Unmarshal(envelope.Type, &kind)
		_ = common.Unmarshal(envelope.Code, &code)
		switch strings.ToLower(strings.TrimSpace(kind)) {
		case "error", "upstream_error":
			fault = &types.OpenAIError{Type: kind, Code: code}
		default:
			return nil
		}
	}
	if info.StreamStatus == nil {
		info.StreamStatus = relaycommon.NewStreamStatus()
	}
	code, _ := fault.Code.(string)
	info.StreamStatus.MarkFailed(code, fault.Type, status)
	return fault
}

// observeMediaUsage preserves each reported counter, including explicit zero,
// across OpenAI-compatible image and audio response envelopes.
func observeMediaUsage(info *relaycommon.RelayInfo, usage *dto.Usage, body []byte) {
	if info.ResponseFacts == nil || usage == nil {
		return
	}
	for _, prefix := range []string{"usage.input_token_details", "usage.input_tokens_details", "usage.prompt_tokens_details"} {
		text, _ := relaycommon.NativeUsagePresence(body, prefix+".text_tokens", "", "")
		audio, _ := relaycommon.NativeUsagePresence(body, prefix+".audio_tokens", "", "")
		image, _ := relaycommon.NativeUsagePresence(body, prefix+".image_tokens", "", "")
		info.ResponseFacts.ImageInputTokensReported = info.ResponseFacts.ImageInputTokensReported || image
		info.ResponseFacts.TextInputTokensReported = info.ResponseFacts.TextInputTokensReported || text
		info.ResponseFacts.AudioInputTokensReported = info.ResponseFacts.AudioInputTokensReported || audio
	}
	path := "usage.output_tokens_details.audio_tokens"
	if raw := ojson.GetBytes(body, path).Raw; raw == "" || raw == "null" {
		path = "usage.completion_tokens_details.audio_tokens"
	}
	if reported, _ := relaycommon.NativeUsagePresence(body, path, "", ""); reported {
		info.ResponseFacts.AudioOutputTokensReported = true
	}
	for _, prefix := range []string{"usage.output_tokens_details", "usage.completion_tokens_details"} {
		if reported, _ := relaycommon.NativeUsagePresence(body, prefix+".image_tokens", "", ""); reported {
			info.ResponseFacts.ImageOutputTokensReported = true
		}
	}
	prompt, completion := relaycommon.MediaUsagePresence(body)
	if raw := ojson.GetBytes(body, "usage.input_tokens").Raw; prompt && raw != "" && raw != "null" {
		usage.PromptTokens = usage.InputTokens
	}
	if raw := ojson.GetBytes(body, "usage.output_tokens").Raw; completion && raw != "" && raw != "null" {
		usage.CompletionTokens = usage.OutputTokens
	}
	info.ResponseFacts.PromptTokensReported = info.ResponseFacts.PromptTokensReported || prompt
	info.ResponseFacts.CompletionTokensReported = info.ResponseFacts.CompletionTokensReported || completion
	info.ResponseFacts.UsageReported = info.ResponseFacts.PromptTokensReported && info.ResponseFacts.CompletionTokensReported
}

func applyProtocolUsagePostProcessing(c *gin.Context, info *relaycommon.RelayInfo, usage *dto.Usage, responseBody []byte) {
	if !channel.RepairProviderUsage(c, usage, responseBody) {
		applyUsagePostProcessing(info, usage, responseBody)
	}
}

func (p *Provider) RepairUsage(context *channel.ProtocolResponseContext, usage *dto.Usage, body []byte) {
	if context.ChannelType != constant.ChannelTypeOpenAI || usage == nil || usage.PromptTokensDetails.CachedTokens != 0 {
		return
	}
	if cachedTokens, ok := extractLlamaCachedTokensFromBody(body); ok {
		usage.PromptTokensDetails.CachedTokens = cachedTokens
	}
}

func applyUsagePostProcessing(info *relaycommon.RelayInfo, usage *dto.Usage, responseBody []byte) {
	if info == nil || usage == nil {
		return
	}

	switch info.ChannelType {
	case constant.ChannelTypeDeepSeek:
		if usage.PromptTokensDetails.CachedTokens == 0 && usage.PromptCacheHitTokens != 0 {
			usage.PromptTokensDetails.CachedTokens = usage.PromptCacheHitTokens
		}
	case constant.ChannelTypeZhipu_v4:
		// 智普的cached_tokens在标准位置: usage.prompt_tokens_details.cached_tokens
		if usage.PromptTokensDetails.CachedTokens == 0 {
			if usage.InputTokensDetails != nil && usage.InputTokensDetails.CachedTokens > 0 {
				usage.PromptTokensDetails.CachedTokens = usage.InputTokensDetails.CachedTokens
			} else if cachedTokens, ok := extractCachedTokensFromBody(responseBody); ok {
				usage.PromptTokensDetails.CachedTokens = cachedTokens
			} else if usage.PromptCacheHitTokens > 0 {
				usage.PromptTokensDetails.CachedTokens = usage.PromptCacheHitTokens
			}
		}
	case constant.ChannelTypeMoonshot:
		// Moonshot的cached_tokens在非标准位置: choices[].usage.cached_tokens
		if usage.PromptTokensDetails.CachedTokens == 0 {
			if usage.InputTokensDetails != nil && usage.InputTokensDetails.CachedTokens > 0 {
				usage.PromptTokensDetails.CachedTokens = usage.InputTokensDetails.CachedTokens
			} else if cachedTokens, ok := extractMoonshotCachedTokensFromBody(responseBody); ok {
				usage.PromptTokensDetails.CachedTokens = cachedTokens
			} else if cachedTokens, ok := extractCachedTokensFromBody(responseBody); ok {
				usage.PromptTokensDetails.CachedTokens = cachedTokens
			} else if usage.PromptCacheHitTokens > 0 {
				usage.PromptTokensDetails.CachedTokens = usage.PromptCacheHitTokens
			}
		}
	case constant.ChannelTypeOpenAI:
		if usage.PromptTokensDetails.CachedTokens == 0 {
			if cachedTokens, ok := extractLlamaCachedTokensFromBody(responseBody); ok {
				usage.PromptTokensDetails.CachedTokens = cachedTokens
			}
		}
	}
}

func extractCachedTokensFromBody(body []byte) (int, bool) {
	if len(body) == 0 {
		return 0, false
	}

	var payload struct {
		Usage struct {
			PromptTokensDetails struct {
				CachedTokens *int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CachedTokens         *int `json:"cached_tokens"`
			PromptCacheHitTokens *int `json:"prompt_cache_hit_tokens"`
		} `json:"usage"`
	}

	if err := common.Unmarshal(body, &payload); err != nil {
		return 0, false
	}

	if payload.Usage.PromptTokensDetails.CachedTokens != nil {
		return *payload.Usage.PromptTokensDetails.CachedTokens, true
	}
	if payload.Usage.CachedTokens != nil {
		return *payload.Usage.CachedTokens, true
	}
	if payload.Usage.PromptCacheHitTokens != nil {
		return *payload.Usage.PromptCacheHitTokens, true
	}
	return 0, false
}

// extractMoonshotCachedTokensFromBody 从Moonshot的非标准位置提取cached_tokens
// Moonshot的流式响应格式: {"choices":[{"usage":{"cached_tokens":111}}]}
func extractMoonshotCachedTokensFromBody(body []byte) (int, bool) {
	if len(body) == 0 {
		return 0, false
	}

	var payload struct {
		Choices []struct {
			Usage struct {
				CachedTokens *int `json:"cached_tokens"`
			} `json:"usage"`
		} `json:"choices"`
	}

	if err := common.Unmarshal(body, &payload); err != nil {
		return 0, false
	}

	// 遍历choices查找cached_tokens
	for _, choice := range payload.Choices {
		if choice.Usage.CachedTokens != nil && *choice.Usage.CachedTokens > 0 {
			return *choice.Usage.CachedTokens, true
		}
	}

	return 0, false
}

// extractLlamaCachedTokensFromBody 从llama.cpp的非标准位置提取cache_n
func extractLlamaCachedTokensFromBody(body []byte) (int, bool) {
	if len(body) == 0 {
		return 0, false
	}

	var payload struct {
		Timings struct {
			CachedTokens *int `json:"cache_n"`
		} `json:"timings"`
	}

	if err := common.Unmarshal(body, &payload); err != nil {
		return 0, false
	}

	if payload.Timings.CachedTokens == nil {
		return 0, false
	}
	return *payload.Timings.CachedTokens, true
}
