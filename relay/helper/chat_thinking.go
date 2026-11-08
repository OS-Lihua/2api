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
	first := info.ThinkingContentInfo.IsFirstThinkingContent
	sent := info.ThinkingContentInfo.HasSentThinkingContent
	closed := info.ThinkingContentInfo.SendLastThinkingContent
	for i, choice := range choices {
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
	}
	info.ThinkingContentInfo.IsFirstThinkingContent = first && !sent
	info.ThinkingContentInfo.HasSentThinkingContent = sent
	info.ThinkingContentInfo.SendLastThinkingContent = closed
	return body, nil
}
