package service

import (
	"context"
	"fmt"
	"maps"
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
	"github.com/gin-gonic/gin"
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

const gatewayNativeMediaReferencesKey = "gateway_native_media_references"

// GatewayRequestTokenOptions keeps native references and opaque image codecs
// out of forced downloads or decoding. Their upstream representation is intact.
func GatewayRequestTokenOptions(c *gin.Context) RequestTokenOptions {
	references, _ := c.Get(gatewayNativeMediaReferencesKey)
	native, _ := references.(map[string]bool)
	return RequestTokenOptions{UseDeclaredMediaTypes: true, NativeReferences: native}
}

// GatewayChatProjection exposes token and capability facts for validation only.
// The native DTO remains the source of the actual candidate conversion.
func GatewayChatProjection(ctx context.Context, request dto.Request) (*dto.GeneralOpenAIRequest, error) {
	if chat, ok := request.(*dto.GeneralOpenAIRequest); ok {
		var files []dto.MediaContent
		references := map[string]bool{}
		if c, ok := ctx.(*gin.Context); ok {
			if previous, found := c.Get(gatewayNativeMediaReferencesKey); found {
				if stored, ok := previous.(map[string]bool); ok {
					maps.Copy(references, stored)
				}
			}
		}
		represented := map[string]int{}
		for _, message := range chat.Messages {
			for _, part := range message.ParseContent() {
				if file := part.GetFile(); part.Type == dto.ContentTypeFile && file != nil && file.FileData != "" {
					represented[file.FileData]++
				}
			}
		}
		for _, message := range chat.Messages {
			for _, part := range message.ParseContent() {
				file := part.GetFile()
				if part.Type != dto.ContentTypeFile || file == nil || file.FileId == "" || file.FileData != "" {
					continue
				}
				references[file.FileId] = true
				if represented[file.FileId] > 0 {
					represented[file.FileId]--
					continue
				}
				files = append(files, dto.MediaContent{Type: dto.ContentTypeFile, File: &dto.MessageFile{FileName: "reference.pdf", FileData: file.FileId}})
			}
		}
		if len(files) == 0 {
			return chat, nil
		}
		if c, ok := ctx.(*gin.Context); ok {
			c.Set(gatewayNativeMediaReferencesKey, references)
		}
		projection := *chat
		projection.Messages = append(slices.Clone(chat.Messages), dto.Message{Role: "user", Content: files})
		return &projection, nil
	}
	requiresTextInput := false
	if responses, ok := request.(*dto.OpenAIResponsesRequest); ok {
		requiresTextInput = gjson.GetBytes(responses.Prompt, "id").String() != "" || responses.PreviousResponseID != "" || len(responses.Conversation) > 0 && string(responses.Conversation) != "null"
		copy := *responses
		copy.PreviousResponseID = ""
		copy.Conversation, copy.Prompt, copy.ContextManagement = nil, nil, nil
		request = &copy
	}
	projectionRequest := request
	if claude, ok := request.(*dto.ClaudeRequest); ok {
		projection := *claude
		projection.Messages = slices.Clone(claude.Messages)
		for i, message := range claude.Messages {
			blocks, err := message.ParseContent()
			if err != nil {
				continue
			}
			blocks = slices.Clone(blocks)
			for j, block := range blocks {
				if block.Type != "tool_result" || block.IsStringContent() {
					continue
				}
				content := []dto.ClaudeMediaMessage{}
				for _, part := range block.ParseMediaContent() {
					if part.Type != "image" && part.Type != "document" {
						content = append(content, part)
					}
				}
				blocks[j].Content = content
			}
			projection.Messages[i].Content = blocks
		}
		projectionRequest = &projection
	}
	result, err := relayconvert.ConvertRequest(gatewayConversionContext(ctx), nil, types.RelayFormatOpenAI, projectionRequest)
	if err != nil {
		return nil, err
	}
	preservedImageReferences := false
	projectionMedia := map[string]dto.MediaContent{}
	var referencedMedia []dto.MediaContent
	references := map[string]bool{}
	if responses, ok := request.(*dto.OpenAIResponsesRequest); ok {
		for _, input := range gjson.ParseBytes(responses.Input).Array() {
			parts := input.Get("content").Array()
			switch input.Get("type").String() {
			case "function_call_output", "custom_tool_call_output":
				parts = input.Get("output").Array()
			case "input_image", "input_file":
				parts = []gjson.Result{input}
			}
			for _, part := range parts {
				fields := part
				if file := part.Get("file"); file.IsObject() {
					fields = file
				}
				id := fields.Get("file_id").String()
				if id == "" {
					continue
				}
				switch part.Get("type").String() {
				case "input_file":
					referencedMedia = append(referencedMedia, dto.MediaContent{Type: dto.ContentTypeFile, File: &dto.MessageFile{FileName: "reference.pdf", FileData: id}})
				case "input_image":
					preservedImageReferences = true
					referencedMedia = append(referencedMedia, dto.MediaContent{Type: dto.ContentTypeImageURL, ImageUrl: &dto.MessageImageUrl{Url: id, Detail: part.Get("detail").String(), MimeType: "image/png"}})
				default:
					continue
				}
				references[id] = true
			}
		}
		if c, ok := ctx.(*gin.Context); ok {
			c.Set(gatewayNativeMediaReferencesKey, references)
		}
	}
	if claude, ok := request.(*dto.ClaudeRequest); ok {
		type nativeBlock struct {
			dto.ClaudeMediaMessage
			path   string
			nested bool
		}
		var mediaBlocks []nativeBlock
		for i, message := range claude.Messages {
			blocks, _ := message.ParseContent()
			for j, block := range blocks {
				mediaBlocks = append(mediaBlocks, nativeBlock{ClaudeMediaMessage: block, path: fmt.Sprintf("messages[%d].content[%d]", i, j)})
			}
		}
		for index := 0; index < len(mediaBlocks); index++ {
			block := mediaBlocks[index]
			if block.Type == "tool_result" && !block.IsStringContent() {
				for i, part := range block.ParseMediaContent() {
					mediaBlocks = append(mediaBlocks, nativeBlock{ClaudeMediaMessage: part, path: fmt.Sprintf("%s.content[%d]", block.path, i), nested: true})
				}
			}
			if block.Source == nil {
				continue
			}
			if block.nested && (block.Type == "image" || block.Type == "document") {
				source := block.Source
				if block.Type == "document" && source.Type == "text" {
					text, ok := source.Data.(string)
					if !ok {
						return nil, fmt.Errorf("tool result document text cannot be represented for validation")
					}
					referencedMedia = append(referencedMedia, dto.MediaContent{Type: dto.ContentTypeText, Text: text, CacheControl: block.CacheControl})
					continue
				}
				reference := source.Url
				if source.Type == "file" && source.FileID != nil {
					reference = *source.FileID
				} else if source.Type == "base64" {
					if data, ok := source.Data.(string); ok && data != "" {
						reference = "data:" + source.MediaType + ";base64," + data
					}
				}
				if reference == "" {
					return nil, fmt.Errorf("tool result media cannot be represented for validation")
				}
				if source.Type == "file" || source.Type == "url" {
					references[reference] = true
				}
				media := dto.MediaContent{Type: dto.ContentTypeFile, File: &dto.MessageFile{FileName: "reference.pdf", FileData: reference}}
				if block.Type == "image" {
					media = dto.MediaContent{Type: dto.ContentTypeImageURL, ImageUrl: &dto.MessageImageUrl{Url: reference, MimeType: source.MediaType}}
				}
				media.CacheControl = block.CacheControl
				referencedMedia = append(referencedMedia, media)
				continue
			}
			var media dto.MediaContent
			reference := ""
			if block.Type == "document" && block.Source.Type == "url" {
				reference = block.Source.Url
				media = dto.MediaContent{Type: dto.ContentTypeFile, File: &dto.MessageFile{FileName: "reference.pdf", FileData: reference}}
			} else if block.Source.Type == "file" && block.Source.FileID != nil {
				reference = *block.Source.FileID
				switch block.Type {
				case "document":
					media = dto.MediaContent{Type: dto.ContentTypeFile, File: &dto.MessageFile{FileName: "reference.pdf", FileData: reference}}
				case "image":
					media = dto.MediaContent{Type: dto.ContentTypeImageURL, ImageUrl: &dto.MessageImageUrl{Url: reference, MimeType: "image/png"}}
				default:
					continue
				}
			}
			if reference == "" {
				continue
			}
			references[reference] = true
			if block.nested {
				referencedMedia = append(referencedMedia, media)
			} else {
				projectionMedia[block.path+".source"] = media
			}
		}
		if c, ok := ctx.(*gin.Context); ok {
			c.Set(gatewayNativeMediaReferencesKey, references)
		}
	}
	if gemini, ok := request.(*dto.GeminiChatRequest); ok {
		if c, ok := ctx.(*gin.Context); ok {
			if previous, found := c.Get(gatewayNativeMediaReferencesKey); found {
				if stored, ok := previous.(map[string]bool); ok && stored != nil {
					references = stored
				}
			}
		}
		type mediaPart struct {
			dto.GeminiPart
			path   string
			nested bool
		}
		var parts []mediaPart
		for i, content := range gemini.Contents {
			for j, part := range content.Parts {
				path := fmt.Sprintf("contents[%d].parts[%d]", i, j)
				parts = append(parts, mediaPart{GeminiPart: part, path: path})
				if part.FunctionResponse != nil && len(part.FunctionResponse.Parts) > 0 {
					var nested []dto.GeminiPart
					if err := common.Unmarshal(part.FunctionResponse.Parts, &nested); err != nil {
						return nil, fmt.Errorf("invalid Gemini function response parts: %w", err)
					}
					for k, child := range nested {
						parts = append(parts, mediaPart{GeminiPart: child, path: fmt.Sprintf("%s.functionResponse.parts[%d]", path, k), nested: true})
					}
				}
			}
		}
		for _, part := range parts {
			path := part.path + ".inlineData"
			if part.InlineData != nil && part.InlineData.Data != "" {
				mime := strings.ToLower(part.InlineData.MimeType)
				switch {
				case strings.HasPrefix(mime, "audio/"):
					projectionMedia[path] = dto.MediaContent{Type: dto.ContentTypeInputAudio, InputAudio: &dto.MessageInputAudio{Data: part.InlineData.Data, Format: strings.TrimPrefix(mime, "audio/")}}
				case strings.HasPrefix(mime, "image/"):
					projectionMedia[path] = dto.MediaContent{Type: dto.ContentTypeImageURL, ImageUrl: &dto.MessageImageUrl{Url: "data:" + mime + ";base64," + part.InlineData.Data, Detail: "auto", MimeType: mime}}
				case strings.HasPrefix(mime, "video/"):
					projectionMedia[path] = dto.MediaContent{Type: dto.ContentTypeVideoUrl, VideoUrl: &dto.MessageVideoUrl{Url: "data:" + mime + ";base64," + part.InlineData.Data}}
				case mime == "application/pdf":
					projectionMedia[path] = dto.MediaContent{Type: dto.ContentTypeFile, File: &dto.MessageFile{FileName: "document.pdf", FileData: "data:application/pdf;base64," + part.InlineData.Data}}
				}
			}
			if part.FileData != nil && part.FileData.FileUri != "" {
				references[part.FileData.FileUri] = true
				mime := strings.ToLower(part.FileData.MimeType)
				path = part.path + ".fileData"
				switch {
				case strings.HasPrefix(mime, "image/"):
					projectionMedia[path] = dto.MediaContent{Type: dto.ContentTypeImageURL, ImageUrl: &dto.MessageImageUrl{Url: part.FileData.FileUri, Detail: "auto", MimeType: mime}}
				case strings.HasPrefix(mime, "audio/"):
					projectionMedia[path] = dto.MediaContent{Type: dto.ContentTypeInputAudio, InputAudio: &dto.MessageInputAudio{Data: part.FileData.FileUri, Format: strings.TrimPrefix(mime, "audio/")}}
				case strings.HasPrefix(mime, "video/"):
					projectionMedia[path] = dto.MediaContent{Type: dto.ContentTypeVideoUrl, VideoUrl: &dto.MessageVideoUrl{Url: part.FileData.FileUri}}
				case mime == "application/pdf":
					projectionMedia[path] = dto.MediaContent{Type: dto.ContentTypeFile, File: &dto.MessageFile{FileName: "reference.pdf", FileData: part.FileData.FileUri}}
				}
			}
			if part.nested {
				media, represented := projectionMedia[path]
				if !represented {
					return nil, fmt.Errorf("tool result media cannot be represented for validation")
				}
				if media.Type == dto.ContentTypeImageURL && part.InlineData != nil && !slices.Contains([]string{"image/png", "image/jpeg", "image/webp", "image/gif"}, strings.ToLower(part.InlineData.MimeType)) {
					if source := media.ToFileSource(); source != nil {
						references[source.GetRawData()] = true
					}
				}
				referencedMedia = append(referencedMedia, media)
			}
		}
		if c, ok := ctx.(*gin.Context); ok {
			c.Set(gatewayNativeMediaReferencesKey, references)
		}
	}
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.Code == "unsupported_image_file_reference" && preservedImageReferences {
			continue
		}
		if (diagnostic.Code == "unsupported_media_type" || diagnostic.Code == "unsupported_media_source") && diagnostic.Severity == types.ConversionDiagnosticError {
			if media, represented := projectionMedia[diagnostic.Path]; represented {
				if media.Type == dto.ContentTypeImageURL {
					if source := media.ToFileSource(); source != nil {
						references[source.GetRawData()] = true
					}
				}
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
	chat.RequiresTextInput = requiresTextInput
	if native, ok := request.(*dto.ClaudeRequest); ok {
		chat.RequiresToolCalling = len(gjson.ParseBytes(native.McpServers).Array()) > 0
	}
	if len(chat.Tools) == 0 {
		var nativeTools []byte
		switch native := request.(type) {
		case *dto.OpenAIResponsesRequest:
			nativeTools = native.Tools
		case *dto.ClaudeRequest:
			nativeTools, err = common.Marshal(native.Tools)
			if err != nil {
				return nil, err
			}
		case *dto.GeminiChatRequest:
			nativeTools = native.Tools
		}
		if tools := gjson.ParseBytes(nativeTools).Array(); len(tools) > 0 {
			// Retain a native tool capability fact omitted by the Chat encoder.
			// This validation projection is never the upstream send source.
			chat.Tools = []dto.ToolCallRequest{{Type: tools[0].Get("type").String(), Native: []byte(tools[0].Raw)}}
		}
	}
	if len(chat.PromptCacheOptions) == 0 {
		for _, diagnostic := range result.Diagnostics {
			if diagnostic.Code != "cache_control_dropped" {
				continue
			}
			body, err := common.Marshal(request)
			if err != nil {
				return nil, err
			}
			path := strings.NewReplacer("[", ".", "]", "").Replace(diagnostic.Path)
			control := gjson.GetBytes(body, path)
			if control.Exists() && control.Type != gjson.Null {
				// Keep the native cache requirement in the validation projection.
				chat.PromptCacheOptions = []byte(control.Raw)
				break
			}
		}
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

// CheckGatewayConversionLosses never waives errors and requires an exact
// explicit code for every other conversion loss.
func CheckGatewayConversionLosses(diagnostics []types.ConversionDiagnostic, accepted []string) error {
	var rejected []types.ConversionDiagnostic
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == types.ConversionDiagnosticError || !slices.Contains(accepted, diagnostic.Code) {
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
