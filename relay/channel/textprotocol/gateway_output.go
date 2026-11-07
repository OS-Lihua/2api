package textprotocol

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

type gatewayToolOutput struct{ name, arguments string }

// gatewayGeneratedOutput retains only transient delivered text and tool inputs.
// Responses tool snapshots replace accumulated deltas rather than billing them twice.
type gatewayGeneratedOutput struct {
	text      strings.Builder
	reasoning strings.Builder
	tools     map[string]gatewayToolOutput
	order     []string
}

func (o *gatewayGeneratedOutput) ObserveResponses(body []byte) (string, bool) {
	root := gjson.ParseBytes(body)
	kind := root.Get("type").String()
	item := root.Get("item")
	id := root.Get("item_id").String()
	if item.Exists() {
		id = item.Get("id").String()
	}
	if id == "" {
		id = "index:" + root.Get("output_index").String()
	}
	var name, arguments string
	switch kind {
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		text := root.Get("delta").String()
		o.reasoning.WriteString(text)
		return text, true
	case "response.completed", "response.done", "response.incomplete":
		var business, text, reasoning strings.Builder
		for index, item := range root.Get("response.output").Array() {
			if name, arguments, ok := gatewayResponseTool(item); ok {
				id := item.Get("id").String()
				if id == "" {
					id = fmt.Sprintf("index:%d", index)
				}
				o.mergeTool(id, gatewayToolOutput{name, arguments}, false)
				business.WriteString(name + arguments)
			} else {
				builder := &text
				if item.Get("type").String() == "reasoning" {
					builder = &reasoning
				}
				for _, content := range item.Get("content").Array() {
					builder.WriteString(content.Get("text").String())
				}
				for _, summary := range item.Get("summary").Array() {
					builder.WriteString(summary.Get("text").String())
				}
			}
		}
		for _, snapshot := range []struct {
			builder *strings.Builder
			text    string
		}{{&o.text, text.String()}, {&o.reasoning, reasoning.String()}} {
			if snapshot.text == "" {
				continue
			}
			if strings.HasPrefix(snapshot.text, snapshot.builder.String()) {
				snapshot.builder.Reset()
				snapshot.builder.WriteString(snapshot.text)
			}
			business.WriteString(snapshot.text)
		}
		return business.String(), true
	case "response.output_item.added", "response.output_item.done":
		var ok bool
		name, arguments, ok = gatewayResponseTool(item)
		if !ok {
			return "", false
		}
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta",
		"response.function_call_arguments.done", "response.custom_tool_call_input.done":
		arguments = root.Get("delta").String()
		if strings.HasSuffix(kind, ".done") {
			arguments = root.Get("arguments").String()
			if value := root.Get("input"); value.Exists() {
				arguments = value.String()
			}
		}
	default:
		return "", false
	}
	o.mergeTool(id, gatewayToolOutput{name, arguments}, strings.HasSuffix(kind, ".delta"))
	return name + arguments, true
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
	output.WriteString(o.reasoning.String())
	output.WriteString(o.text.String())
	for _, id := range o.order {
		tool := o.tools[id]
		output.WriteString(tool.name)
		output.WriteString(tool.arguments)
	}
	return output.String()
}

func gatewayResponseTool(item gjson.Result) (string, string, bool) {
	kind := item.Get("type").String()
	name := item.Get("name").String()
	switch kind {
	case "function_call":
		return name, item.Get("arguments").String(), true
	case "custom_tool_call":
		return name, item.Get("input").String(), true
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

func gatewayMessagesBlockText(block gjson.Result) string {
	switch block.Get("type").String() {
	case "text":
		return block.Get("text").String()
	case "thinking":
		return block.Get("thinking").String()
	case "tool_use", "server_tool_use", "mcp_tool_use":
		text := block.Get("name").String()
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
