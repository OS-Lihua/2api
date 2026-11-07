package service

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/convmeta"
	"github.com/QuantumNous/new-api/relaykit/types"
)

// GatewayProtocolFormat maps the four public text protocols independently of Provider type.
func GatewayProtocolFormat(kind protocol.Type) (types.RelayFormat, error) {
	switch kind {
	case protocol.OpenAIChat:
		return types.RelayFormatOpenAI, nil
	case protocol.OpenAIResponses:
		return types.RelayFormatOpenAIResponses, nil
	case protocol.AnthropicMessages:
		return types.RelayFormatClaude, nil
	case protocol.GeminiGenerate:
		return types.RelayFormatGemini, nil
	default:
		return "", fmt.Errorf("unsupported text protocol")
	}
}

// PreconvertGatewayRequest performs no transport or accounting. Every diagnostic
// requires explicit consent; converter errors (including stateful fields) cannot
// be waived by an accepted diagnostic code.
func PreconvertGatewayRequest(ctx context.Context, info convmeta.Meta, kind protocol.Type, request dto.Request, accepted []string) (*relayconvert.RequestResult, error) {
	format, err := GatewayProtocolFormat(kind)
	if err != nil {
		return nil, err
	}
	result, err := relayconvert.ConvertRequest(ctx, info, format, request)
	if err != nil {
		return result, err
	}
	var rejected []types.ConversionDiagnostic
	for _, diagnostic := range result.Diagnostics {
		if !slices.Contains(accepted, diagnostic.Code) {
			rejected = append(rejected, diagnostic)
		}
	}
	if len(rejected) > 0 {
		return result, &types.ConversionLossError{Diagnostics: rejected}
	}
	return result, nil
}

// GatewayConversionCodes produces a stable, deduplicated header and record value.
func GatewayConversionCodes(diagnostics []types.ConversionDiagnostic) []string {
	codes := []string{}
	for _, diagnostic := range diagnostics {
		if diagnostic.Code != "" && !slices.Contains(codes, diagnostic.Code) {
			codes = append(codes, diagnostic.Code)
		}
	}
	slices.Sort(codes)
	return codes
}

// GatewayConversionError exposes parameter names and fixed converter reasons,
// never a Provider's settings, credentials or upstream response body.
type GatewayConversionError struct {
	Diagnostics []types.ConversionDiagnostic
}

func (e *GatewayConversionError) Error() string {
	messages := []string{}
	for _, diagnostic := range e.Diagnostics {
		message := diagnostic.Path + ": " + diagnostic.Message
		if !slices.Contains(messages, message) {
			messages = append(messages, message)
		}
	}
	slices.Sort(messages)
	return "Unsupported parameters: " + strings.Join(messages, "; ")
}
