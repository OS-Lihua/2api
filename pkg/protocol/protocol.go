// Package protocol defines the wire protocols used by Channels and adapters.
// Plugin availability is a host concern; Validate checks protocol syntax only.
package protocol

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

type Type string

const (
	OpenAIChat               Type = "openai_chat"
	OpenAIResponses          Type = "openai_responses"
	AnthropicMessages        Type = "anthropic_messages"
	GeminiGenerate           Type = "gemini_generate"
	OpenAIEmbeddings         Type = "openai_embeddings"
	GeminiEmbed              Type = "gemini_embed"
	OpenAIImages             Type = "openai_images"
	OpenAIAudioSpeech        Type = "openai_audio_speech"
	OpenAIAudioTranscription Type = "openai_audio_transcription"
	Rerank                   Type = "rerank"
)

var pluginKey = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,29}$`)

// Builtins returns the supported non-task protocols in display order.
var builtinTypes = []Type{OpenAIChat, OpenAIResponses, AnthropicMessages, GeminiGenerate, OpenAIEmbeddings, GeminiEmbed, OpenAIImages, OpenAIAudioSpeech, OpenAIAudioTranscription, Rerank}

func Builtins() []Type { return slices.Clone(builtinTypes) }

func (t Type) TaskPluginKey() (string, bool) {
	key, ok := strings.CutPrefix(string(t), "task:")
	return key, ok && pluginKey.MatchString(key)
}

func (t Type) Validate() error {
	if slices.Contains(builtinTypes, t) {
		return nil
	}
	if _, ok := t.TaskPluginKey(); ok {
		return nil
	}
	return fmt.Errorf("invalid protocol %q", t)
}
