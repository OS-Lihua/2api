package protocol

import (
	"fmt"

	"github.com/QuantumNous/new-api/relaykit/types"
)

// RelayFormat identifies the public DTO for an explicit Channel protocol.
func (t Type) RelayFormat() (types.RelayFormat, error) {
	switch t {
	case OpenAIChat:
		return types.RelayFormatOpenAI, nil
	case OpenAIResponses:
		return types.RelayFormatOpenAIResponses, nil
	case AnthropicMessages:
		return types.RelayFormatClaude, nil
	case GeminiGenerate, GeminiEmbed:
		return types.RelayFormatGemini, nil
	case OpenAIEmbeddings:
		return types.RelayFormatEmbedding, nil
	case OpenAIImages:
		return types.RelayFormatOpenAIImage, nil
	case OpenAIAudioSpeech, OpenAIAudioTranscription:
		return types.RelayFormatOpenAIAudio, nil
	case Rerank:
		return types.RelayFormatRerank, nil
	default:
		return "", fmt.Errorf("unsupported protocol %q", t)
	}
}
