package channel

import (
	"fmt"
	"slices"

	"github.com/QuantumNous/new-api/pkg/protocol"
)

// IsNonTextProtocol identifies the public synchronous media and ranking protocols.
// Task protocols use the existing TaskAdaptor entry point.
func IsNonTextProtocol(kind protocol.Type) bool {
	switch kind {
	case protocol.OpenAIEmbeddings, protocol.GeminiEmbed, protocol.OpenAIImages, protocol.OpenAIAudioSpeech, protocol.OpenAIAudioTranscription, protocol.Rerank:
		return true
	}
	return false
}

// ComposeNonTextProtocol declares actual, implemented provider wire capabilities.
// The public handler validates the selected DTO before the wire implementation
// converts it. Both ports are narrow ProtocolHandlers, never legacy Adaptors.
func ComposeNonTextProtocol(kind protocol.Type, public, wire ProtocolHandler, supported ...protocol.Type) (ProtocolHandler, error) {
	if !slices.Contains(supported, kind) {
		return nil, fmt.Errorf("provider does not support non-text protocol %q", kind)
	}
	return &WireProtocol{Kind: kind, Public: public, Wire: wire}, nil
}
