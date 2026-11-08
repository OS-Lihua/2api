package channel

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

// NativeChatCapabilities describes controls and content that the native request
// actually retains. It does not describe the Model's general capabilities.
type NativeChatCapabilities struct {
	Parameters   []string
	ContentTypes []string
}

type NativeChatRequestContract interface {
	ChatRequestCapabilities() NativeChatCapabilities
}

// DiagnoseNativeChatRequest applies strict conversion acceptance to the final
// provider dialect as well as to public protocol conversions.
func DiagnoseNativeChatRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest, caps NativeChatCapabilities, actual *dto.GeneralOpenAIRequest) error {
	body, err := common.Marshal(request)
	if err != nil {
		return err
	}
	var fields map[string]common.RawMessage
	if err := common.Unmarshal(body, &fields); err != nil {
		return err
	}
	if actual == nil {
		return fmt.Errorf("native request has no effective wire projection")
	}
	encoded, err := common.Marshal(actual)
	if err != nil {
		return err
	}
	var after map[string]common.RawMessage
	if err := common.Unmarshal(encoded, &after); err != nil {
		return err
	}
	var diagnostics []types.ConversionDiagnostic
	keys := slices.Sorted(maps.Keys(fields))
	for _, key := range keys {
		if len(fields[key]) == 0 || string(fields[key]) == "null" {
			continue
		}
		// Native response parsers supply usage to the shared presenter; the
		// presenter implements stream_options independently of provider syntax.
		if key == "model" || key == "messages" || key == "stream" || key == "stream_options" {
			continue
		}
		if slices.Contains(caps.Parameters, key) {
			raw := after[key]
			if key == "max_tokens" || key == "max_completion_tokens" {
				raw = after["max_completion_tokens"]
				if len(raw) == 0 {
					raw = after["max_tokens"]
				}
			}
			var beforeValue, afterValue any
			if err := common.Unmarshal(fields[key], &beforeValue); err != nil {
				return err
			}
			if len(raw) > 0 {
				if err := common.Unmarshal(raw, &afterValue); err != nil {
					return err
				}
			}
			if key == "stop" {
				if text, ok := beforeValue.(string); ok {
					beforeValue = []any{text}
				}
				if text, ok := afterValue.(string); ok {
					afterValue = []any{text}
				}
			}
			if reflect.DeepEqual(beforeValue, afterValue) {
				continue
			}
		}
		if key == "n" && (request.N == nil || *request.N == 1) {
			continue
		}
		if key == "reasoning_effort" && request.ReasoningEffort == "none" {
			continue
		}
		if key == "response_format" && request.ResponseFormat != nil && request.ResponseFormat.Type == "text" {
			continue
		}
		diagnostics = append(diagnostics, types.ConversionDiagnostic{Code: "request_parameter_dropped", Path: key, Severity: types.ConversionDiagnosticWarning, From: types.RelayFormatOpenAI, To: types.RelayFormatOpenAI, Message: "The native request did not retain this request parameter"})
	}
	for index, message := range request.Messages {
		for _, part := range message.ParseContent() {
			if !slices.Contains(caps.ContentTypes, part.Type) {
				diagnostics = append(diagnostics, types.ConversionDiagnostic{Code: "unsupported_media_type", Path: fmt.Sprintf("messages[%d].content", index), Severity: types.ConversionDiagnosticError, From: types.RelayFormatOpenAI, To: types.RelayFormatOpenAI, Message: "The native request cannot represent this content type"})
			}
			if len(part.CacheControl) > 0 && string(part.CacheControl) != "null" {
				diagnostics = append(diagnostics, types.ConversionDiagnostic{Code: "cache_control_dropped", Path: fmt.Sprintf("messages[%d].content.cache_control", index), Severity: types.ConversionDiagnosticWarning, From: types.RelayFormatOpenAI, To: types.RelayFormatOpenAI, Message: "The native request omitted the cache boundary"})
			}
		}
		if !slices.Contains(caps.Parameters, "tools") && (message.Role == "tool" || len(message.ParseToolCalls()) > 0 || len(message.Tools) > 0) {
			diagnostics = append(diagnostics, types.ConversionDiagnostic{Code: "request_parameter_dropped", Path: fmt.Sprintf("messages[%d].tool_calls", index), Severity: types.ConversionDiagnosticWarning, From: types.RelayFormatOpenAI, To: types.RelayFormatOpenAI, Message: "The native request did not retain tool message semantics"})
		}
	}
	info.RecordConversionDiagnostics(c, diagnostics)
	return nil
}

// ProjectNativeChatParameters clears unsupported public controls. An override
// cannot turn an ignored public field into an effective native wire constraint.
func ProjectNativeChatParameters(body []byte, public dto.GeneralOpenAIRequest, native any, caps NativeChatCapabilities) (*dto.GeneralOpenAIRequest, error) {
	var wire map[string]common.RawMessage
	if err := common.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	kind := reflect.TypeOf(native)
	for kind.Kind() == reflect.Pointer {
		kind = kind.Elem()
	}
	if kind.Kind() != reflect.Struct {
		return nil, fmt.Errorf("invalid native request type")
	}
	properties := make(map[string]bool)
	for index := range kind.NumField() {
		field := kind.Field(index)
		key, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if key != "" && key != "-" {
			properties[key] = true
		}
	}
	for key := range wire {
		if !properties[key] {
			return nil, fmt.Errorf("native wire does not support request parameter %s", key)
		}
	}
	encoded, err := common.Marshal(public)
	if err != nil {
		return nil, err
	}
	var fields map[string]common.RawMessage
	if err := common.Unmarshal(encoded, &fields); err != nil {
		return nil, err
	}
	for key := range fields {
		if key != "model" && key != "messages" && key != "stream" && key != "stream_options" && !slices.Contains(caps.Parameters, key) {
			delete(fields, key)
		}
	}
	encoded, err = common.Marshal(fields)
	if err != nil {
		return nil, err
	}
	var result dto.GeneralOpenAIRequest
	if err := common.Unmarshal(encoded, &result); err != nil {
		return nil, err
	}
	result.RequiresTextInput = public.RequiresTextInput
	return &result, nil
}
