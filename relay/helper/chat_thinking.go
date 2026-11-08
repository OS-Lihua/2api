package helper

import (
	"strconv"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ChatThinkingToContent applies the Provider's presentation setting without
// dropping unrelated native fields. Added tags are presentation, not usage.
func ChatThinkingToContent(info *relaycommon.RelayInfo, body []byte) ([]byte, error) {
	choices := gjson.GetBytes(body, "choices").Array()
	if info.ThinkingContentInfo.Choices == nil {
		info.ThinkingContentInfo.Choices = map[int]*relaycommon.ThinkingContentInfo{}
	}
	for i, choice := range choices {
		index := i
		if value := choice.Get("index"); value.Exists() {
			index = int(value.Int())
		}
		state := info.ThinkingContentInfo.Choices[index]
		if state == nil {
			state = &relaycommon.ThinkingContentInfo{IsFirstThinkingContent: true}
			info.ThinkingContentInfo.Choices[index] = state
		}
		first := state.IsFirstThinkingContent
		sent := state.HasSentThinkingContent
		closed := state.SendLastThinkingContent
		delta := choice.Get("delta")
		reasoning := delta.Get("reasoning_content")
		if !reasoning.Exists() || reasoning.Type == gjson.Null {
			reasoning = delta.Get("reasoning")
		}
		thought := reasoning.String()
		content := delta.Get("content").String()
		text := thought
		if thought != "" {
			if first {
				text = "<think>\n" + text
			}
			sent = true
		}
		if content != "" {
			if sent && !closed {
				text += "\n</think>\n"
				closed = true
			}
			text += content
		}
		if thought != "" || content != "" {
			var err error
			body, err = sjson.SetBytes(body, "choices."+strconv.Itoa(i)+".delta.content", text)
			if err != nil {
				return nil, err
			}
		}
		var err error
		body, err = sjson.DeleteBytes(body, "choices."+strconv.Itoa(i)+".delta.reasoning_content")
		if err != nil {
			return nil, err
		}
		body, err = sjson.DeleteBytes(body, "choices."+strconv.Itoa(i)+".delta.reasoning")
		if err != nil {
			return nil, err
		}
		state.IsFirstThinkingContent = first && !sent
		state.HasSentThinkingContent = sent
		state.SendLastThinkingContent = closed
	}
	return body, nil
}
