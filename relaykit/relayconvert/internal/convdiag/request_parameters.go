package convdiag

import (
	"context"
	"strings"

	"github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/QuantumNous/new-api/relaykit/types"
)

// InspectRequestParameters reports cache and output-schema controls that the
// converter actually omitted. It observes wire DTOs rather than converting them.
func InspectRequestParameters(ctx context.Context, source, target any, from, to types.RelayFormat) error {
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
