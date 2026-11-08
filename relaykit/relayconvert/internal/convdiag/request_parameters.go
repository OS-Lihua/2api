package convdiag

import (
	"context"
	"reflect"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/QuantumNous/new-api/relaykit/types"
)

// InspectRequestParameters reports generation, cache and output controls that
// the converter omitted or changed. It compares public wire DTO semantics.
func InspectRequestParameters(ctx context.Context, source, target any, from, to types.RelayFormat, openRouter bool) error {
	beforeBody, err := kitutil.Marshal(source)
	if err != nil {
		return err
	}
	afterBody, err := kitutil.Marshal(target)
	if err != nil {
		return err
	}
	var before, after map[string]any
	if err := kitutil.Unmarshal(beforeBody, &before); err != nil {
		return err
	}
	if err := kitutil.Unmarshal(afterBody, &after); err != nil {
		return err
	}
	formats := map[types.RelayFormat]int{types.RelayFormatOpenAI: 0, types.RelayFormatOpenAIResponses: 1, types.RelayFormatClaude: 2, types.RelayFormatGemini: 3}
	fromIndex, fromKnown := formats[from]
	toIndex, toKnown := formats[to]
	if fromKnown && toKnown {
		for _, paths := range [][4]string{
			{"temperature", "temperature", "temperature", "generationConfig.temperature"},
			{"top_p", "top_p", "top_p", "generationConfig.topP"},
			{"top_k", "top_k", "top_k", "generationConfig.topK"},
			{"stop", "stop", "stop_sequences", "generationConfig.stopSequences"},
			{"seed", "", "", "generationConfig.seed"},
			{"presence_penalty", "presence_penalty", "", "generationConfig.presencePenalty"},
			{"frequency_penalty", "frequency_penalty", "", "generationConfig.frequencyPenalty"},
			{"logit_bias", "", "", ""},
			{"logprobs", "", "", "generationConfig.responseLogprobs"},
			{"top_logprobs", "top_logprobs", "", "generationConfig.logprobs"},
			{"min_p", "min_p", "", ""},
			{"repetition_penalty", "repetition_penalty", "", ""},
			{"", "max_tool_calls", "", ""},
			{"min_tokens", "", "", ""},
			{"structured_outputs", "", "", ""},
			{"return_token_ids", "", "", ""},
			{"separate_reasoning", "", "", ""},
			{"stream_reasoning", "", "", ""},
			{"regex", "", "", ""},
			{"ebnf", "", "", ""},
			{"stop_token_ids", "", "", ""},
			{"stop_regex", "", "", ""},
			{"no_stop_trim", "", "", ""},
			{"ignore_eos", "", "", ""},
			{"skip_special_tokens", "", "", ""},
			{"continue_final_message", "", "", ""},
			{"cache_salt", "", "", ""},
			{"thinking_token_budget", "", "", ""},
			{"include_reasoning", "", "", ""},
			{"reasoning_split", "", "", ""},
			{"chat_template_kwargs", "", "", ""},
			{"vl_high_resolution_images", "", "", ""},
			{"store", "store", "", ""},
			{"service_tier", "service_tier", "service_tier", ""},
			{"user", "user", "metadata.user_id", ""},
			{"safety_identifier", "safety_identifier", "", ""},
			{"prediction", "", "", ""},
			{"verbosity", "text.verbosity", "", ""},
			{"", "", "", "generationConfig.mediaResolution"},
			{"", "", "", "generationConfig.mediaProcessing"},
			{"", "", "", "generationConfig.enableEnhancedCivicAnswers"},
			{"", "", "", "generationConfig.speechConfig"},
			{"", "", "", "generationConfig.imageConfig"},
		} {
			path := paths[fromIndex]
			if path == "" {
				continue
			}
			value := requestParameterAt(before, path)
			afterValue := requestParameterAt(after, paths[toIndex])
			if paths[0] == "stop" {
				if text, ok := value.(string); ok {
					value = []any{text}
				}
				if text, ok := afterValue.(string); ok {
					afterValue = []any{text}
				}
				beforeStops, _ := value.([]any)
				afterStops, _ := afterValue.([]any)
				if from == types.RelayFormatGemini && to == types.RelayFormatOpenAI && len(afterStops) > 0 && len(afterStops) < len(beforeStops) && reflect.DeepEqual(beforeStops[:len(afterStops)], afterStops) {
					// The converter already reports stop_sequences_truncated.
					continue
				}
			}
			if value != nil && !reflect.DeepEqual(value, afterValue) {
				Add(ctx, types.ConversionDiagnostic{Code: "request_parameter_dropped", Path: path, Message: "The target request did not retain this request parameter", Severity: types.ConversionDiagnosticWarning, From: from, To: to})
			}
		}
	}
	InspectCacheControls(ctx, before, after, from, to, to == types.RelayFormatClaude || to == types.RelayFormatOpenAI && openRouter)
	if !slices.Equal(requestOutputModalities(before, from), requestOutputModalities(after, to)) {
		path := "modalities"
		if from == types.RelayFormatGemini {
			path = "generationConfig.responseModalities"
		}
		Add(ctx, types.ConversionDiagnostic{Code: "response_modalities_dropped", Path: path, Message: "The target request did not retain the requested output modalities", Severity: types.ConversionDiagnosticWarning, From: from, To: to})
	}
	if before["safetySettings"] != nil && after["safetySettings"] == nil {
		Add(ctx, types.ConversionDiagnostic{Code: "safety_settings_dropped", Path: "safetySettings", Message: "The target request omitted the safety settings", Severity: types.ConversionDiagnosticWarning, From: from, To: to})
	}
	if requestCandidateCount(before, from) != requestCandidateCount(after, to) {
		path := "n"
		if from == types.RelayFormatGemini {
			path = "generationConfig.candidateCount"
		}
		Add(ctx, types.ConversionDiagnostic{Code: "candidate_count_dropped", Path: path, Message: "The target request did not retain the requested number of choices", Severity: types.ConversionDiagnosticWarning, From: from, To: to})
	}
	for _, key := range []string{"prompt_cache_key", "prompt_cache_options", "prompt_cache_retention", "cachedContent"} {
		if before[key] != nil && after[key] == nil {
			Add(ctx, types.ConversionDiagnostic{Code: "cache_parameter_dropped", Path: key, Message: "The target request omitted this cache parameter", Severity: types.ConversionDiagnosticWarning, From: from, To: to})
		}
	}
	var paths []string
	switch from {
	case types.RelayFormatOpenAI:
		if format, ok := before["response_format"].(map[string]any); ok && format["type"] != "text" {
			paths = append(paths, "response_format")
		}
	case types.RelayFormatOpenAIResponses:
		if format, ok := requestParameterAt(before, "text.format").(map[string]any); ok && format["type"] != "text" {
			paths = append(paths, "text.format")
		}
	case types.RelayFormatClaude:
		if requestParameterAt(before, "output_config.format") != nil {
			paths = append(paths, "output_config.format")
		}
		if before["output_format"] != nil {
			paths = append(paths, "output_format")
		}
	case types.RelayFormatGemini:
		for _, key := range []string{"responseSchema", "responseJsonSchema", "responseMimeType"} {
			path := "generationConfig." + key
			if value := requestParameterAt(before, path); value != nil && value != "text/plain" {
				paths = append(paths, path)
			}
		}
	}
	preserved := false
	switch to {
	case types.RelayFormatOpenAI:
		preserved = after["response_format"] != nil
	case types.RelayFormatOpenAIResponses:
		preserved = requestParameterAt(after, "text.format") != nil
	case types.RelayFormatClaude:
		preserved = after["output_format"] != nil || requestParameterAt(after, "output_config.format") != nil
	case types.RelayFormatGemini:
		preserved = requestParameterAt(after, "generationConfig.responseMimeType") != nil || requestParameterAt(after, "generationConfig.responseSchema") != nil || requestParameterAt(after, "generationConfig.responseJsonSchema") != nil
	}
	if !preserved {
		for _, path := range paths {
			Add(ctx, types.ConversionDiagnostic{Code: "response_schema_dropped", Path: path, Message: "The target request omitted the structured output constraint", Severity: types.ConversionDiagnosticWarning, From: from, To: to})
		}
	}
	return nil
}

func requestCandidateCount(request map[string]any, format types.RelayFormat) int64 {
	path := "n"
	if format == types.RelayFormatGemini {
		path = "generationConfig.candidateCount"
	}
	if count, ok := requestParameterAt(request, path).(float64); ok && count >= 1 && count <= 1<<31-1 {
		return int64(count)
	}
	return 1
}

func requestOutputModalities(request map[string]any, format types.RelayFormat) []string {
	path := "modalities"
	if format == types.RelayFormatGemini {
		path = "generationConfig.responseModalities"
	}
	values, _ := requestParameterAt(request, path).([]any)
	if len(values) == 0 {
		return []string{"text"}
	}
	modalities := make([]string, 0, len(values))
	for _, value := range values {
		if name, ok := value.(string); ok {
			modalities = append(modalities, strings.ToLower(name))
		}
	}
	slices.Sort(modalities)
	return slices.Compact(modalities)
}

func requestParameterAt(value map[string]any, path string) any {
	var current any = value
	for key := range strings.SplitSeq(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[key]
	}
	return current
}
