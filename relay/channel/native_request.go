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
	beforeMessages, err := nativeChatMessageSemantics(request.Messages)
	if err != nil {
		return err
	}
	afterMessages, err := nativeChatMessageSemantics(actual.Messages)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(beforeMessages, afterMessages) {
		diagnostics = append(diagnostics, types.ConversionDiagnostic{Code: "request_parameter_dropped", Path: "messages", Severity: types.ConversionDiagnosticWarning, From: types.RelayFormatOpenAI, To: types.RelayFormatOpenAI, Message: "The native request changed message roles, history or content"})
	}
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
	result.RequiresToolCalling = public.RequiresToolCalling
	return &result, nil
}

func nativeChatMessageSemantics(messages []dto.Message) (any, error) {
	normalized := slices.Clone(messages)
	for i := range normalized {
		parts := normalized[i].ParseContent()
		textOnly := true
		for _, part := range parts {
			if part.Type != dto.ContentTypeText {
				textOnly = false
				break
			}
		}
		if textOnly {
			normalized[i].Content = normalized[i].StringContent()
		} else {
			normalized[i].Content = parts
		}
	}
	grouped := make([]dto.Message, 0, len(normalized))
	for _, message := range normalized {
		if len(grouped) > 0 && message.Role == "system" && grouped[len(grouped)-1].Role == "system" {
			before, after := grouped[len(grouped)-1], message
			previousText, previousOK := before.Content.(string)
			text, textOK := after.Content.(string)
			before.Content, after.Content = nil, nil
			beforeBody, err := common.Marshal(before)
			if err != nil {
				return nil, err
			}
			afterBody, err := common.Marshal(after)
			if err != nil {
				return nil, err
			}
			if previousOK && textOK && string(beforeBody) == string(afterBody) {
				grouped[len(grouped)-1].Content = previousText + "\n" + text
				continue
			}
		}
		grouped = append(grouped, message)
	}
	body, err := common.Marshal(grouped)
	if err != nil {
		return nil, err
	}
	var value any
	if err := common.Unmarshal(body, &value); err != nil {
		return nil, err
	}
	return value, nil
}
