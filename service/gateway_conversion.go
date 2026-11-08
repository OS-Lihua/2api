package service

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/convmeta"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/tidwall/gjson"
)

// GatewayProtocolFormat maps the four public text protocols independently of Provider type.
func GatewayProtocolFormat(kind protocol.Type) (types.RelayFormat, error) {
	switch kind {
	case protocol.OpenAIChat, protocol.OpenAIResponses, protocol.AnthropicMessages, protocol.GeminiGenerate:
		return kind.RelayFormat()
	default:
		return "", fmt.Errorf("unsupported text protocol")
	}
}

// GatewayChatProjection exposes token and capability facts for validation only.
// The native DTO remains the source of the actual candidate conversion.
func GatewayChatProjection(ctx context.Context, request dto.Request) (*dto.GeneralOpenAIRequest, error) {
	if chat, ok := request.(*dto.GeneralOpenAIRequest); ok {
		return chat, nil
	}
	if responses, ok := request.(*dto.OpenAIResponsesRequest); ok {
		copy := *responses
		copy.PreviousResponseID = ""
		copy.Conversation, copy.Prompt, copy.ContextManagement = nil, nil, nil
		request = &copy
	}
	result, err := relayconvert.ConvertRequest(gatewayConversionContext(ctx), nil, types.RelayFormatOpenAI, request)
	if err != nil {
		return nil, err
	}
	projectionMedia := map[string]dto.MediaContent{}
	if gemini, ok := request.(*dto.GeminiChatRequest); ok {
		for i, content := range gemini.Contents {
			for j, part := range content.Parts {
				if part.FileData == nil || part.FileData.FileUri == "" {
					continue
				}
				mime := strings.ToLower(part.FileData.MimeType)
				path := fmt.Sprintf("contents[%d].parts[%d].fileData", i, j)
				switch {
				case strings.HasPrefix(mime, "audio/"):
					projectionMedia[path] = dto.MediaContent{Type: dto.ContentTypeInputAudio, InputAudio: &dto.MessageInputAudio{Data: part.FileData.FileUri, Format: strings.TrimPrefix(mime, "audio/")}}
				case mime == "application/pdf":
					projectionMedia[path] = dto.MediaContent{Type: dto.ContentTypeFile, File: &dto.MessageFile{FileName: "reference.pdf", FileData: part.FileData.FileUri}}
				}
			}
		}
	}
	var referencedMedia []dto.MediaContent
	for _, diagnostic := range result.Diagnostics {
		if (diagnostic.Code == "unsupported_media_type" || diagnostic.Code == "unsupported_media_source") && diagnostic.Severity == types.ConversionDiagnosticError {
			if media, represented := projectionMedia[diagnostic.Path]; represented {
				referencedMedia = append(referencedMedia, media)
				continue
			}
			return nil, fmt.Errorf("request media cannot be represented for validation")
		}
	}
	chat, ok := result.Value.(*dto.GeneralOpenAIRequest)
	if !ok {
		return nil, fmt.Errorf("missing text request projection")
	}
	if len(referencedMedia) > 0 {
		// This validation-only bucket retains native reference modalities and
		// their conservative input estimate. It is never an upstream request.
		chat.Messages = append(chat.Messages, dto.Message{Role: "user", Content: referencedMedia})
	}
	if claude, ok := request.(*dto.ClaudeRequest); ok {
		format := gjson.GetBytes(claude.OutputConfig, "format")
		if format.Exists() && format.Type != gjson.Null || gjson.ParseBytes(claude.OutputFormat).IsObject() {
			chat.ResponseFormat = &dto.ResponseFormat{Type: "json_object"}
		}
	}
	if gemini, ok := request.(*dto.GeminiChatRequest); ok {
		if len(gemini.GenerationConfig.ResponseModalities) > 0 {
			modalities := make([]string, 0, len(gemini.GenerationConfig.ResponseModalities))
			for _, modality := range gemini.GenerationConfig.ResponseModalities {
				modalities = append(modalities, strings.ToLower(modality))
			}
			chat.Modalities, err = common.Marshal(modalities)
			if err != nil {
				return nil, err
			}
		}
		if gemini.CachedContent != "" {
			chat.PromptCacheKey = gemini.CachedContent
		}
		if gemini.GenerationConfig.ResponseSchema != nil || len(gemini.GenerationConfig.ResponseJsonSchema) > 0 || gemini.GenerationConfig.ResponseMimeType == "application/json" {
			chat.ResponseFormat = &dto.ResponseFormat{Type: "json_object"}
		}
	}
	return chat, nil
}

// PreconvertGatewayRequest performs no transport or accounting. Every diagnostic
// requires explicit consent; converter errors (including stateful fields) cannot
// be waived by an accepted diagnostic code.
func PreconvertGatewayRequest(ctx context.Context, info convmeta.Meta, kind protocol.Type, request dto.Request, accepted []string) (*relayconvert.RequestResult, error) {
	format, err := GatewayProtocolFormat(kind)
	if err != nil {
		return nil, err
	}
	clone, err := CloneGatewayRequest(request)
	if err != nil {
		return nil, err
	}
	result, err := relayconvert.ConvertRequest(gatewayConversionContext(ctx), info, format, clone)
	if err != nil {
		return result, err
	}
	return result, CheckGatewayConversionLosses(result.Diagnostics, accepted)
}

// CloneGatewayRequest isolates candidate conversion and RelayInfo normalization
// from the original downstream request.
func CloneGatewayRequest(request dto.Request) (dto.Request, error) {
	var clone dto.Request
	switch request.(type) {
	case *dto.GeneralOpenAIRequest:
		clone = &dto.GeneralOpenAIRequest{}
	case *dto.OpenAIResponsesRequest:
		clone = &dto.OpenAIResponsesRequest{}
	case *dto.ClaudeRequest:
		clone = &dto.ClaudeRequest{}
	case *dto.GeminiChatRequest:
		clone = &dto.GeminiChatRequest{}
	default:
		return nil, fmt.Errorf("unsupported text request")
	}
	body, err := common.Marshal(request)
	if err != nil {
		return nil, err
	}
	if string(body) == "null" {
		return nil, fmt.Errorf("missing text request")
	}
	if err := common.Unmarshal(body, clone); err != nil {
		return nil, err
	}
	return clone, nil
}

func GatewayAcceptedConversionLosses(offering *model.ModelOffering) []string {
	accepted := append([]string(nil), model_setting.GetGlobalSettings().AcceptedConversionLosses...)
	return append(accepted, offering.AcceptedConversionLosses...)
}

// CheckGatewayConversionLosses requires an exact explicit code for every loss.
func CheckGatewayConversionLosses(diagnostics []types.ConversionDiagnostic, accepted []string) error {
	var rejected []types.ConversionDiagnostic
	for _, diagnostic := range diagnostics {
		if !slices.Contains(accepted, diagnostic.Code) {
			rejected = append(rejected, diagnostic)
		}
	}
	if len(rejected) > 0 {
		return &types.ConversionLossError{Diagnostics: rejected}
	}
	return nil
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
