package textprotocol

import (
	"encoding/json"
	"fmt"
	"strings"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/tokenkit"

	"github.com/tidwall/gjson"
)

type gatewayToolOutput struct{ name, arguments string }

// gatewayGeneratedOutput tracks delivered content and defers terminal
// snapshots until upstream completion is verified. Snapshots retain omitted blocks.
type gatewayGeneratedOutput struct {
	pendingTerminal []json.RawMessage
	terminalBytes   int
	sourceTerminal  bool
	terminalReady   bool
	text            strings.Builder
	parts           map[string]*strings.Builder
	partOrder       []string
	tools           map[string]gatewayToolOutput
	chatTools       map[[2]int64]struct{}
	choiceText      map[int64]*strings.Builder
	order           []string
}

func (o *gatewayGeneratedOutput) appendChoice(index int64, text string) {
	if o.choiceText == nil {
		o.choiceText = map[int64]*strings.Builder{}
	}
	if o.choiceText[index] == nil {
		o.choiceText[index] = &strings.Builder{}
	}
	o.choiceText[index].WriteString(text)
}

func (o *gatewayGeneratedOutput) TextTokenEstimate(model string) int {
	if len(o.choiceText) == 0 {
		return tokenkit.Count(model, o.String())
	}
	count := 0
	for _, part := range o.choiceText {
		count += tokenkit.Count(model, part.String())
	}
	return count
}

func (o *gatewayGeneratedOutput) ObserveGemini(body []byte) {
	for i, candidate := range gjson.GetBytes(body, "candidates").Array() {
		index := int64(i)
		if value := candidate.Get("index"); value.Exists() {
			index = value.Int()
		}
		var text strings.Builder
		for _, part := range candidate.Get("content.parts").Array() {
			text.WriteString(gatewayGeminiPartText(part))
		}
		o.appendChoice(index, text.String())
	}
}

func (o *gatewayGeneratedOutput) ObserveChat(body []byte) {
	for i, choice := range gjson.GetBytes(body, "choices").Array() {
		index := int64(i)
		if reported := choice.Get("index"); reported.Exists() {
			index = reported.Int()
		}
		o.appendChoice(index, relaycommon.ChatStreamChoiceBusinessText(choice))
		for j, tool := range choice.Get("delta.tool_calls").Array() {
			toolIndex := int64(j)
			if reported := tool.Get("index"); reported.Exists() {
				toolIndex = reported.Int()
			}
			if o.chatTools == nil {
				o.chatTools = map[[2]int64]struct{}{}
			}
			o.chatTools[[2]int64{index, toolIndex}] = struct{}{}
		}
	}
}

func (o *gatewayGeneratedOutput) ObserveResponses(body []byte) (string, bool) {
	root := gjson.ParseBytes(body)
	kind := root.Get("type").String()
	index := root.Get("output_index").Int()
	switch kind {
	case "response.output_text.delta", "response.output_text.done", "response.refusal.delta", "response.refusal.done",
		"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.reasoning_text.delta", "response.reasoning_text.done":
		category, partIndex := "content", root.Get("content_index").Int()
		if strings.HasPrefix(kind, "response.reasoning_summary") {
			category, partIndex = "summary", root.Get("summary_index").Int()
		}
		if strings.HasPrefix(kind, "response.reasoning_text") {
			category = "reasoning"
		}
		text := relaycommon.SourceText(root.Get("delta"))
		delta := strings.HasSuffix(kind, ".delta")
		if !delta {
			text = relaycommon.SourceText(root.Get("text"))
			if value := root.Get("refusal"); value.Exists() {
				text = relaycommon.SourceText(value)
			}
		}
		o.mergePart(fmt.Sprintf("%d/%s/%d", index, category, partIndex), text, delta)
		return text, true
	case "response.content_part.added", "response.content_part.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		category, partIndex := "content", root.Get("content_index").Int()
		if strings.HasPrefix(kind, "response.reasoning_summary") {
			category, partIndex = "summary", root.Get("summary_index").Int()
		}
		text := gatewayResponsesContentText(root.Get("part"))
		o.mergePart(fmt.Sprintf("%d/%s/%d", index, category, partIndex), text, false)
		return text, true
	case "response.completed", "response.done", "response.incomplete":
		var text strings.Builder
		for index, item := range root.Get("response.output").Array() {
			text.WriteString(o.ObserveItem(int64(index), item))
		}
		return text.String(), true
	case "response.output_item.added", "response.output_item.done":
		return o.ObserveItem(index, root.Get("item")), true
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta", "response.function_call_arguments.done", "response.custom_tool_call_input.done":
		arguments := relaycommon.SourceText(root.Get("delta"))
		delta := strings.HasSuffix(kind, ".delta")
		if !delta {
			arguments = relaycommon.SourceText(root.Get("arguments"))
			if value := root.Get("input"); value.Exists() {
				arguments = relaycommon.SourceText(value)
			}
		}
		id := root.Get("item_id").String()
		if id == "" {
			id = fmt.Sprintf("index:%d", index)
		}
		o.mergeTool(id, gatewayToolOutput{arguments: arguments}, delta)
		return arguments, true
	default:
		return "", false
	}
}

func (o *gatewayGeneratedOutput) ObserveItem(index int64, item gjson.Result) string {
	if name, arguments, ok := gatewayResponseTool(item); ok {
		id := item.Get("id").String()
		if id == "" {
			id = fmt.Sprintf("index:%d", index)
		}
		o.mergeTool(id, gatewayToolOutput{name, arguments}, false)
		return name + arguments
	}
	category := "content"
	if item.Get("type").String() == "reasoning" {
		category = "reasoning"
	}
	var text strings.Builder
	for partIndex, part := range item.Get("content").Array() {
		value := gatewayResponsesContentText(part)
		o.mergePart(fmt.Sprintf("%d/%s/%d", index, category, partIndex), value, false)
		text.WriteString(value)
	}
	for partIndex, part := range item.Get("summary").Array() {
		value := gatewayResponsesContentText(part)
		o.mergePart(fmt.Sprintf("%d/summary/%d", index, partIndex), value, false)
		text.WriteString(value)
	}
	return text.String()
}

func (o *gatewayGeneratedOutput) mergePart(id, text string, delta bool) {
	if text == "" {
		return
	}
	if o.parts == nil {
		o.parts = map[string]*strings.Builder{}
	}
	part, exists := o.parts[id]
	if !exists {
		part = &strings.Builder{}
		o.parts[id] = part
		o.partOrder = append(o.partOrder, id)
	}
	if delta {
		part.WriteString(text)
		return
	}
	if strings.HasPrefix(text, part.String()) {
		part.Reset()
		part.WriteString(text)
	} else if !strings.Contains(part.String(), text) {
		part.WriteString(text)
	}
}

func (o *gatewayGeneratedOutput) mergeTool(id string, incoming gatewayToolOutput, delta bool) {
	if o.tools == nil {
		o.tools = map[string]gatewayToolOutput{}
	}
	tool, exists := o.tools[id]
	if !exists {
		o.order = append(o.order, id)
	}
	if incoming.name != "" {
		tool.name = incoming.name
	}
	if delta {
		tool.arguments += incoming.arguments
	} else if incoming.arguments != "" {
		tool.arguments = incoming.arguments
	}
	o.tools[id] = tool
}

func (o *gatewayGeneratedOutput) String() string {
	var output strings.Builder
	output.WriteString(o.text.String())
	for _, id := range o.partOrder {
		output.WriteString(o.parts[id].String())
	}
	for _, id := range o.order {
		tool := o.tools[id]
		output.WriteString(tool.name)
		output.WriteString(tool.arguments)
	}
	return output.String()
}

func gatewayResponsesContentText(part gjson.Result) string {
	if value := part.Get("refusal"); value.Type == gjson.String {
		return relaycommon.SourceText(value)
	}
	return relaycommon.SourceText(part.Get("text"))
}

func gatewayResponseTool(item gjson.Result) (string, string, bool) {
	kind := item.Get("type").String()
	name := relaycommon.SourceText(item.Get("name"))
	switch kind {
	case "function_call":
		return name, relaycommon.SourceText(item.Get("arguments")), true
	case "custom_tool_call":
		return name, relaycommon.SourceText(item.Get("input")), true
	case "web_search_call", "file_search_call", "computer_call", "code_interpreter_call", "mcp_call", "local_shell_call":
		if name == "" {
			name = strings.TrimSuffix(kind, "_call")
		}
		var arguments strings.Builder
		for _, key := range []string{"action", "arguments", "input", "code", "queries"} {
			if value := item.Get(key); value.Exists() && value.Type != gjson.Null {
				arguments.WriteString(value.String())
			}
		}
		return name, arguments.String(), true
	default:
		return "", "", false
	}
}

func gatewayMediaOutput(body []byte, format types.RelayFormat) bool {
	if format == types.RelayFormatOpenAIResponses {
		root := gjson.ParseBytes(body)
		if root.Get("type").String() == "response.image_generation_call.partial_image" && relaycommon.SourceText(root.Get("partial_image_b64")) != "" {
			return true
		}
		items := root.Get("output").Array()
		items = append(items, root.Get("response.output").Array()...)
		if item := root.Get("item"); item.Exists() {
			items = append(items, item)
		}
		for _, item := range items {
			if item.Get("type").String() == "image_generation_call" && item.Get("result").Type == gjson.String && item.Get("result").String() != "" {
				return true
			}
		}
		return false
	}
	if format == types.RelayFormatOpenAI {
		for _, choice := range gjson.GetBytes(body, "choices").Array() {
			if relaycommon.SourceText(choice.Get("message.audio.data")) != "" || relaycommon.SourceText(choice.Get("delta.audio.data")) != "" {
				return true
			}
		}
		return false
	}
	if format != types.RelayFormatGemini {
		return false
	}
	for _, candidate := range gjson.GetBytes(body, "candidates").Array() {
		for _, part := range candidate.Get("content.parts").Array() {
			if relaycommon.SourceText(part.Get("inlineData.data")) != "" {
				return true
			}
		}
	}
	return false
}

func gatewayGeminiPartText(part gjson.Result) string {
	var text strings.Builder
	text.WriteString(relaycommon.SourceText(part.Get("text")))
	if part.Get("functionCall").Exists() {
		text.WriteString(relaycommon.SourceText(part.Get("functionCall.name")))
		text.WriteString(part.Get("functionCall.args").Raw)
	}
	text.WriteString(relaycommon.SourceText(part.Get("executableCode.code")))
	text.WriteString(relaycommon.SourceText(part.Get("codeExecutionResult.output")))
	return text.String()
}

func gatewayMessagesBlockText(block gjson.Result) string {
	switch block.Get("type").String() {
	case "text":
		return relaycommon.SourceText(block.Get("text"))
	case "thinking":
		return relaycommon.SourceText(block.Get("thinking"))
	case "tool_use", "server_tool_use", "mcp_tool_use":
		text := relaycommon.SourceText(block.Get("name"))
		input := block.Get("input")
		// An empty initial object is a placeholder before input_json_delta.
		if input.Exists() && input.Type != gjson.Null && input.Raw != "{}" {
			text += input.String()
		}
		return text
	default:
		return ""
	}
}
