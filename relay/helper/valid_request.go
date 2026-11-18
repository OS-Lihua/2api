package helper

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/pkg/capability"
	"github.com/QuantumNous/new-api/pkg/protocol"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/samber/lo"

	"github.com/gin-gonic/gin"
)

func GetAndValidateRequest(c *gin.Context, format types.RelayFormat) (request dto.Request, err error) {
	relayMode := relayconstant.Path2RelayMode(c.Request.URL.Path)

	switch format {
	case types.RelayFormatOpenAI:
		request, err = GetAndValidateTextRequest(c, relayMode)
	case types.RelayFormatGemini:
		if strings.Contains(c.Request.URL.Path, ":embedContent") {
			request, err = GetAndValidateGeminiEmbeddingRequest(c)
		} else if strings.Contains(c.Request.URL.Path, ":batchEmbedContents") {
			request, err = GetAndValidateGeminiBatchEmbeddingRequest(c)
		} else {
			request, err = GetAndValidateGeminiRequest(c)
		}
	case types.RelayFormatClaude:
		request, err = GetAndValidateClaudeRequest(c)
	case types.RelayFormatOpenAIResponses:
		request, err = GetAndValidateResponsesRequest(c)
	case types.RelayFormatOpenAIResponsesCompaction:
		request, err = GetAndValidateResponsesCompactionRequest(c)
	case types.RelayFormatOpenAIAlphaSearch:
		request, err = GetAndValidateAlphaSearchRequest(c)
	case types.RelayFormatDeepSeekTools:
		request, err = GetAndValidateDeepSeekToolsRequest(c)

	case types.RelayFormatOpenAIImage:
		request, err = GetAndValidOpenAIImageRequest(c, relayMode)
	case types.RelayFormatEmbedding:
		request, err = GetAndValidateEmbeddingRequest(c, relayMode)
	case types.RelayFormatRerank:
		request, err = GetAndValidateRerankRequest(c)
	case types.RelayFormatOpenAIAudio:
		request, err = GetAndValidAudioRequest(c, relayMode)
	case types.RelayFormatOpenAIRealtime:
		request = &dto.BaseRequest{}
	default:
		return nil, fmt.Errorf("unsupported relay format: %s", format)
	}
	return request, err
}

func GetAndValidAudioRequest(c *gin.Context, relayMode int) (*dto.AudioRequest, error) {
	audioRequest := &dto.AudioRequest{}
	err := common.UnmarshalBodyReusable(c, audioRequest)
	if err != nil {
		return nil, err
	}
	switch relayMode {
	case relayconstant.RelayModeAudioSpeech:
		if audioRequest.Model == "" {
			return nil, errors.New("model is required")
		}
	default:
		if audioRequest.Model == "" {
			return nil, errors.New("model is required")
		}
		if audioRequest.ResponseFormat == "" {
			audioRequest.ResponseFormat = "json"
		}
	}
	return audioRequest, nil
}

func GetAndValidateRerankRequest(c *gin.Context) (*dto.RerankRequest, error) {
	var rerankRequest *dto.RerankRequest
	err := common.UnmarshalBodyReusable(c, &rerankRequest)
	if err != nil {
		logger.LogError(c, common.LogText("getAndValidateTextRequest failed: %s", err.Error()))
		return nil, types.NewError(err, types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}

	if rerankRequest == nil {
		return nil, errors.New("request is required")
	}
	if rerankRequest.Query == "" {
		return nil, types.NewError(fmt.Errorf("query is empty"), types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}
	if len(rerankRequest.Documents) == 0 {
		return nil, types.NewError(fmt.Errorf("documents is empty"), types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}
	return rerankRequest, nil
}

func GetAndValidateEmbeddingRequest(c *gin.Context, relayMode int) (*dto.EmbeddingRequest, error) {
	var embeddingRequest *dto.EmbeddingRequest
	err := common.UnmarshalBodyReusable(c, &embeddingRequest)
	if err != nil {
		logger.LogError(c, common.LogText("getAndValidateTextRequest failed: %s", err.Error()))
		return nil, types.NewError(err, types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}

	if embeddingRequest == nil || embeddingRequest.Input == nil {
		return nil, fmt.Errorf("input is empty")
	}
	if relayMode == relayconstant.RelayModeModerations && embeddingRequest.Model == "" {
		embeddingRequest.Model = "omni-moderation-latest"
	}
	if relayMode == relayconstant.RelayModeEmbeddings && embeddingRequest.Model == "" {
		embeddingRequest.Model = c.Param("model")
	}
	return embeddingRequest, nil
}

// maxTokensLimit bounds user-supplied max token fields. These values feed
// pre-consume quota math (preConsumedTokens * ratio); an unbounded value can
// overflow the conversion and corrupt billing.
const maxTokensLimit = math.MaxInt32 / 2

// ExceedsMaxTokensLimit checks token limits before they reach billing arithmetic.
func ExceedsMaxTokensLimit(values ...*uint) bool {
	for _, v := range values {
		if lo.FromPtrOr(v, uint(0)) > maxTokensLimit {
			return true
		}
	}
	return false
}

type requestParameterError struct{ path, reason string }

func (err *requestParameterError) Error() string         { return err.reason }
func (err *requestParameterError) ParameterPath() string { return err.path }

// Realtime accepts integer output budgets from 1 through 4096, or "inf".
// https://github.com/openai/openai-python/blob/a1175a35ba6aa2d6bf98d26088a572ab0c5f4453/src/openai/types/realtime/realtime_response_create_params.py
const RealtimeMaxOutputTokens = 4096

// ValidateRealtimeMultipliers accepts the native infinity sentinel while keeping
// all numeric and compatible-container limits behind the shared token guard.
func ValidateRealtimeMultipliers(body []byte) error {
	if common.JsonHasDuplicateFields(body) {
		return &requestParameterError{path: "request", reason: "Invalid JSON request body"}
	}
	document := ojson.NewDoc(append([]byte(nil), body...))
	for _, prefix := range []string{"", "parameters."} {
		for _, field := range []string{"max_output_tokens", "max_response_output_tokens"} {
			path := prefix + field
			value := ojson.GetBytes(body, path)
			if !value.Exists() || value.Type == ojson.Null {
				continue
			}
			if value.Type == ojson.String && value.String() == "inf" {
				if err := document.Delete(path); err != nil {
					return err
				}
				continue
			}
			var count uint
			if common.UnmarshalJsonStr(value.Raw, &count) != nil || count < 1 || count > RealtimeMaxOutputTokens || ExceedsMaxTokensLimit(&count) {
				return &requestParameterError{path: path, reason: fmt.Sprintf("%s must be an integer between 1 and 4096 or inf", path)}
			}
		}
	}
	return ValidateResponsesMultipliers(document.Bytes())
}

// NormalizeRealtimeRequestLimits bounds the effective native request before
// Provider I/O. Infinity remains a budget and never becomes observed usage.
func NormalizeRealtimeRequestLimits(body []byte, caps capability.Set, inputTokens int) ([]byte, error) {
	if err := ValidateRealtimeMultipliers(body); err != nil {
		return nil, err
	}
	limit := int64(RealtimeMaxOutputTokens)
	if maximum := caps.Limits.MaxOutputTokens; maximum != nil {
		limit = min(limit, *maximum)
	}
	if contextLength := caps.Limits.ContextLength; contextLength != nil {
		limit = min(limit, *contextLength-int64(inputTokens))
	}
	if limit < 1 {
		return nil, &requestParameterError{path: "input", reason: "Realtime context limit exceeded"}
	}
	document := ojson.NewDoc(append([]byte(nil), body...))
	for _, prefix := range []string{"", "parameters."} {
		for _, field := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens", "max_response_output_tokens", "min_tokens"} {
			path := prefix + field
			value := ojson.GetBytes(body, path)
			if !value.Exists() || value.Type == ojson.Null {
				continue
			}
			if value.Type == ojson.String && value.String() == "inf" {
				if err := document.Set(path, limit); err != nil {
					return nil, err
				}
				continue
			}
			var count uint
			if common.UnmarshalJsonStr(value.Raw, &count) != nil || count > uint(limit) {
				return nil, &requestParameterError{path: path, reason: fmt.Sprintf("%s exceeds the connection output limit", path)}
			}
		}
	}
	if value := ojson.GetBytes(body, "max_output_tokens"); !value.Exists() || value.Type == ojson.Null {
		if err := document.Set("max_output_tokens", limit); err != nil {
			return nil, err
		}
	}
	return document.Bytes(), nil
}

// ValidateResponsesMultipliers bounds compatible passthrough token limits too.
// Responses has one output response; a vendor n>1 cannot silently multiply it.
func ValidateResponsesMultipliers(body []byte) error {
	var envelope struct{}
	if common.Unmarshal(body, &envelope) != nil || !ojson.ParseBytes(body).IsObject() || common.JsonHasDuplicateFields(body) {
		return &requestParameterError{path: "request", reason: "Invalid JSON request body"}
	}
	if container := ojson.GetBytes(body, "parameters"); container.Exists() && container.Type != ojson.Null && !container.IsObject() {
		return &requestParameterError{path: "parameters", reason: "parameters must be an object"}
	}
	for _, prefix := range []string{"", "parameters."} {
		if value := ojson.GetBytes(body, prefix+"n"); value.Exists() && value.Type != ojson.Null {
			var count int
			if common.UnmarshalJsonStr(value.Raw, &count) != nil || count != 1 {
				return &requestParameterError{path: prefix + "n", reason: prefix + "n must be 1 for Responses"}
			}
		}
		for _, field := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens", "min_tokens"} {
			path := prefix + field
			value := ojson.GetBytes(body, path)
			if !value.Exists() || value.Type == ojson.Null {
				continue
			}
			var count uint
			if common.UnmarshalJsonStr(value.Raw, &count) != nil || ExceedsMaxTokensLimit(&count) {
				return &requestParameterError{path: path, reason: prefix + "max_tokens is invalid"}
			}
		}
	}
	return nil
}

func GetAndValidateResponsesRequest(c *gin.Context) (*dto.OpenAIResponsesRequest, error) {
	request := &dto.OpenAIResponsesRequest{}
	err := common.UnmarshalBodyReusable(c, request)
	if err != nil {
		return nil, err
	}
	if request.Model == "" {
		return nil, errors.New("model is required")
	}
	if ExceedsMaxTokensLimit(request.MaxOutputTokens) {
		return nil, errors.New("max_output_tokens is invalid")
	}
	return request, nil
}

// NativeOperationRequest retains the complete host-native JSON document. Its
// routing identity is separate from an optional DeepSeek body.model.
type NativeOperationRequest struct {
	dto.AlphaSearchRequest
	HasImages bool `json:"-"`
}

func DecodeNativeOperationRequest(body []byte, kind protocol.Type, name string) (*NativeOperationRequest, error) {
	if err := ValidateResponsesMultipliers(body); err != nil {
		return nil, err
	}
	var controls struct {
		Model      *string `json:"model"`
		Stream     *bool   `json:"stream"`
		Parameters struct {
			Stream *bool `json:"stream"`
		} `json:"parameters"`
	}
	if common.Unmarshal(body, &controls) != nil || controls.Stream != nil && *controls.Stream || controls.Parameters.Stream != nil && *controls.Parameters.Stream {
		return nil, errors.New("native operation requires synchronous JSON")
	}
	if kind != protocol.DeepSeekTools {
		if controls.Model != nil && *controls.Model != "" {
			name = *controls.Model
		}
		if name == "" && kind == protocol.OpenAIModerations {
			name = "text-moderation-stable"
		}
		if name == "" {
			return nil, errors.New("model is required")
		}
	}
	request := &NativeOperationRequest{AlphaSearchRequest: dto.AlphaSearchRequest{Model: name, RawBody: append([]byte(nil), body...)}}
	if kind == protocol.OpenAIModerations {
		input := ojson.GetBytes(body, "input")
		if input.Type == ojson.String {
			if input.String() == "" {
				return nil, errors.New("input is empty")
			}
		} else if input.IsArray() && len(input.Array()) > 0 {
			for _, item := range input.Array() {
				if item.Type == ojson.String && item.String() != "" {
					continue
				}
				if item.Get("type").String() == "text" && item.Get("text").Type == ojson.String && item.Get("text").String() != "" {
					continue
				}
				if item.Get("type").String() == "image_url" && item.Get("image_url.url").Type == ojson.String && item.Get("image_url.url").String() != "" {
					request.HasImages = true
					continue
				}
				return nil, errors.New("invalid moderation input")
			}
		} else {
			return nil, errors.New("input is empty")
		}
	}
	return request, nil
}

func GetAndValidateAlphaSearchRequest(c *gin.Context) (*dto.AlphaSearchRequest, error) {
	request := &dto.AlphaSearchRequest{}
	if err := common.UnmarshalBodyReusable(c, request); err != nil {
		return nil, err
	}
	if request.Model == "" {
		return nil, errors.New("model is required")
	}
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return nil, err
	}
	rawBody, err := storage.Bytes()
	if err != nil {
		return nil, err
	}
	request.RawBody = rawBody
	return request, nil
}

// GetAndValidateDeepSeekToolsRequest keeps the raw body for forwarding. The
// upstream validates query/url itself, and no field changes the per-call price.
func GetAndValidateDeepSeekToolsRequest(c *gin.Context) (*dto.DeepSeekToolsRequest, error) {
	request := &dto.DeepSeekToolsRequest{}
	if err := common.UnmarshalBodyReusable(c, request); err != nil {
		return nil, err
	}
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return nil, err
	}
	rawBody, err := storage.Bytes()
	if err != nil {
		return nil, err
	}
	if len(rawBody) == 0 {
		return nil, errors.New("request body is empty")
	}
	request.RawBody = rawBody
	return request, nil
}

func GetAndValidateResponsesCompactionRequest(c *gin.Context) (*dto.OpenAIResponsesCompactionRequest, error) {
	request := &dto.OpenAIResponsesCompactionRequest{}
	if err := common.UnmarshalBodyReusable(c, request); err != nil {
		return nil, err
	}
	if request.Model == "" {
		return nil, errors.New("model is required")
	}
	return request, nil
}

func GetAndValidOpenAIImageRequest(c *gin.Context, relayMode int) (*dto.ImageRequest, error) {
	imageRequest := &dto.ImageRequest{}

	switch relayMode {
	case relayconstant.RelayModeImagesEdits:
		if strings.Contains(c.Request.Header.Get("Content-Type"), "multipart/form-data") {
			form := c.Request.MultipartForm
			if form == nil {
				var err error
				form, err = common.ParseMultipartFormReusable(c)
				if err != nil {
					return nil, fmt.Errorf("failed to parse image edit form request: %w", err)
				}
			}
			formData := url.Values(form.Value)
			c.Request.MultipartForm = form
			c.Request.PostForm = formData
			imageRequest.Prompt = formData.Get("prompt")
			imageRequest.Model = formData.Get("model")
			if nValue := strings.TrimSpace(formData.Get("n")); nValue != "" {
				n, err := strconv.Atoi(nValue)
				if err != nil || n < 0 || n > dto.MaxImageN {
					return nil, fmt.Errorf("n must be an integer between 1 and %d", dto.MaxImageN)
				}
				imageRequest.N = common.GetPointer(uint(n))
			}
			imageRequest.Quality = formData.Get("quality")
			imageRequest.Size = formData.Get("size")
			if parameters := formData.Get("parameters"); parameters != "" {
				imageRequest.Extra = map[string]json.RawMessage{"parameters": json.RawMessage(parameters)}
			}
			if streamValue := strings.TrimSpace(formData.Get("stream")); streamValue != "" {
				stream, err := strconv.ParseBool(streamValue)
				if err != nil {
					return nil, fmt.Errorf("invalid stream value: %w", err)
				}
				imageRequest.Stream = common.GetPointer(stream)
			}
			if imageValue := formData.Get("image"); imageValue != "" {
				imageRequest.Image, _ = common.Marshal(imageValue)
			}

			if imageRequest.Model == "gpt-image-1" {
				if imageRequest.Quality == "" {
					imageRequest.Quality = "standard"
				}
			}
			if imageRequest.N == nil || *imageRequest.N == 0 {
				imageRequest.N = common.GetPointer(uint(1))
			}

			hasWatermark := formData.Has("watermark")
			if hasWatermark {
				watermark := formData.Get("watermark") == "true"
				imageRequest.Watermark = &watermark
			}
			break
		}
		fallthrough
	default:
		err := common.UnmarshalBodyReusable(c, imageRequest)
		if err != nil {
			return nil, err
		}

		if imageRequest.Model == "" {
			return nil, errors.New("model is required")
		}

		if strings.Contains(imageRequest.Size, "×") {
			return nil, errors.New("size an unexpected error occurred in the parameter, please use 'x' instead of the multiplication sign '×'")
		}

		if imageRequest.N != nil && *imageRequest.N > dto.MaxImageN {
			return nil, fmt.Errorf("n must be an integer between 1 and %d", dto.MaxImageN)
		}

		if err := imageRequest.NormalizeLegacyDalleImageRequest(); err != nil {
			return nil, err
		}
		if imageRequest.Model == "gpt-image-1" {
			if imageRequest.Quality == "" {
				imageRequest.Quality = "auto"
			}
		}

		//if imageRequest.Prompt == "" {
		//	return nil, errors.New("prompt is required")
		//}

		if imageRequest.N == nil || *imageRequest.N == 0 {
			imageRequest.N = common.GetPointer(uint(1))
		}
	}

	// Provider parameters can override the top-level count. Validate before
	// pricing so malformed multipliers return a client error, not a pricing
	// failure after reservation has started.
	if raw, exists := imageRequest.Extra["parameters"]; exists {
		parameters := &dto.ImageBillingParameters{}
		if err := common.Unmarshal(raw, parameters); err != nil {
			return nil, fmt.Errorf("invalid image parameters: %w", err)
		}
		imageRequest.BillingParameters = parameters
	}
	if _, err := imageRequest.ImageCount(false); err != nil {
		return nil, err
	}
	return imageRequest, nil
}

func GetAndValidateClaudeRequest(c *gin.Context) (textRequest *dto.ClaudeRequest, err error) {
	textRequest = &dto.ClaudeRequest{}
	err = common.UnmarshalBodyReusable(c, textRequest)
	if err != nil {
		return nil, err
	}
	if textRequest.Messages == nil || len(textRequest.Messages) == 0 {
		return nil, errors.New("field messages is required")
	}
	if textRequest.Model == "" {
		return nil, errors.New("field model is required")
	}
	if ExceedsMaxTokensLimit(textRequest.MaxTokens, textRequest.MaxTokensToSample) {
		return nil, errors.New("max_tokens is invalid")
	}

	//if textRequest.Stream {
	//	relayInfo.IsStream = true
	//}

	return textRequest, nil
}

// CompletionsParameters are native text-completion fields absent from the Chat DTO.
// Keep the host endpoint independent of relaykit's Chat request contract.
type CompletionsParameters struct {
	Echo     *bool           `json:"echo,omitempty"`
	LogProbs json.RawMessage `json:"logprobs,omitempty"`
	BestOf   *int            `json:"best_of,omitempty"`
}

// CompletionsPromptShape distinguishes prompt batches from already tokenized
// input, whose token IDs must be counted without tokenizing their decimal text.
func CompletionsPromptShape(prompt any) (int, int, error) {
	if prompt == nil {
		return 1, 0, nil
	}
	if _, ok := prompt.(string); ok {
		return 1, 0, nil
	}
	items, ok := prompt.([]any)
	if !ok || len(items) == 0 {
		return 0, 0, errors.New("prompt is invalid")
	}
	batches := 1
	var tokenPrompts [][]any
	switch items[0].(type) {
	case string:
		if len(items) > MaxChatCompletionsN {
			return 0, 0, errors.New("prompt batch is too large")
		}
		for _, item := range items {
			if _, ok := item.(string); !ok {
				return 0, 0, errors.New("prompt is invalid")
			}
		}
		return len(items), 0, nil
	case []any:
		batches = len(items)
		if batches > MaxChatCompletionsN {
			return 0, 0, errors.New("prompt batch is too large")
		}
		for _, item := range items {
			ids, ok := item.([]any)
			if !ok || len(ids) == 0 {
				return 0, 0, errors.New("prompt token IDs are invalid")
			}
			tokenPrompts = append(tokenPrompts, ids)
		}
	default:
		tokenPrompts = [][]any{items}
	}
	tokens := 0
	for _, ids := range tokenPrompts {
		for _, value := range ids {
			id, ok := value.(float64)
			if !ok || id < 0 || id > math.MaxInt32 || math.Trunc(id) != id {
				return 0, 0, errors.New("prompt token IDs are invalid")
			}
		}
		tokens += len(ids)
		if tokens > maxTokensLimit {
			return 0, 0, errors.New("prompt token IDs exceed the token limit")
		}
	}
	return batches, tokens, nil
}

func DecodeCompletionsRequest(body []byte) (*dto.GeneralOpenAIRequest, CompletionsParameters, error) {
	type projection dto.GeneralOpenAIRequest
	wire := struct {
		*projection
		CompletionsParameters
		LogProbs json.RawMessage `json:"logprobs,omitempty"`
	}{projection: &projection{}}
	if err := common.Unmarshal(body, &wire); err != nil {
		return nil, wire.CompletionsParameters, err
	}
	if _, _, err := CompletionsPromptShape(wire.Prompt); err != nil {
		return nil, wire.CompletionsParameters, err
	}
	wire.CompletionsParameters.LogProbs = wire.LogProbs
	if len(wire.LogProbs) > 0 && string(wire.LogProbs) != "null" {
		var count int
		if err := common.Unmarshal(wire.LogProbs, &count); err != nil {
			// Preserve the previously accepted boolean compatibility field.
			var enabled bool
			if common.Unmarshal(wire.LogProbs, &enabled) != nil {
				return nil, wire.CompletionsParameters, errors.New("logprobs is invalid")
			}
		} else if count < 0 || count > 5 {
			return nil, wire.CompletionsParameters, errors.New("logprobs is invalid")
		}
	}
	choices := 1
	if wire.N != nil {
		choices = *wire.N
	}
	if wire.BestOf != nil && (*wire.BestOf < max(1, choices) || *wire.BestOf > MaxChatCompletionsN || wire.Stream != nil && *wire.Stream) {
		return nil, wire.CompletionsParameters, errors.New("best_of is invalid")
	}
	return (*dto.GeneralOpenAIRequest)(wire.projection), wire.CompletionsParameters, nil
}

func GetAndValidateTextRequest(c *gin.Context, relayMode int) (*dto.GeneralOpenAIRequest, error) {
	if relayMode == relayconstant.RelayModeCompletions {
		storage, err := common.GetBodyStorage(c)
		if err != nil {
			return nil, err
		}
		body, err := storage.Bytes()
		if err != nil {
			return nil, err
		}
		request, _, err := DecodeCompletionsRequest(body)
		if err != nil {
			return nil, err
		}
		return ValidateTextRequest(c, request, relayMode)
	}
	textRequest := &dto.GeneralOpenAIRequest{}
	err := common.UnmarshalBodyReusable(c, textRequest)
	if err != nil {
		return nil, err
	}

	return ValidateTextRequest(c, textRequest, relayMode)
}

// MaxChatCompletionsN bounds the number of generated choices before pricing or
// forwarding, including a Channel's parameter overrides.
const MaxChatCompletionsN = 128

func ValidateTextRequest(c *gin.Context, textRequest *dto.GeneralOpenAIRequest, relayMode int, nativeProjection ...bool) (*dto.GeneralOpenAIRequest, error) {
	if textRequest.N != nil && (*textRequest.N < 1 || *textRequest.N > MaxChatCompletionsN) {
		return nil, errors.New("n is invalid")
	}
	if relayMode == relayconstant.RelayModeModerations && textRequest.Model == "" {
		textRequest.Model = "text-moderation-latest"
	}
	if relayMode == relayconstant.RelayModeEmbeddings && textRequest.Model == "" {
		textRequest.Model = c.Param("model")
	}

	if ExceedsMaxTokensLimit(textRequest.MaxTokens, textRequest.MaxCompletionTokens) {
		return nil, errors.New("max_tokens is invalid")
	}
	if ExceedsMaxTokensLimit(textRequest.MinTokens) {
		return nil, errors.New("min_tokens is invalid")
	}
	if textRequest.Model == "" {
		return nil, errors.New("model is required")
	}
	if textRequest.WebSearchOptions != nil {
		if textRequest.WebSearchOptions.SearchContextSize != "" {
			validSizes := map[string]bool{
				"high":   true,
				"medium": true,
				"low":    true,
			}
			if !validSizes[textRequest.WebSearchOptions.SearchContextSize] {
				return nil, errors.New("invalid search_context_size, must be one of: high, medium, low")
			}
		} else {
			textRequest.WebSearchOptions.SearchContextSize = "medium"
		}
	}
	switch relayMode {
	case relayconstant.RelayModeCompletions:
		if textRequest.Prompt == "" {
			return nil, errors.New("field prompt is required")
		}
	case relayconstant.RelayModeChatCompletions:
		// For FIM (Fill-in-the-middle) requests with prefix/suffix, messages is optional
		// It will be filled by provider-specific adaptors if needed (e.g., SiliconFlow)。Or it is allowed by model vendor(s) (e.g., DeepSeek)
		if len(textRequest.Messages) == 0 && textRequest.Prefix == nil && textRequest.Suffix == nil && (len(nativeProjection) == 0 || !nativeProjection[0]) {
			return nil, errors.New("field messages is required")
		}
	case relayconstant.RelayModeEmbeddings:
	case relayconstant.RelayModeModerations:
		if textRequest.Input == nil || textRequest.Input == "" {
			return nil, errors.New("field input is required")
		}
	case relayconstant.RelayModeEdits:
		if textRequest.Instruction == "" {
			return nil, errors.New("field instruction is required")
		}
	}
	return textRequest, nil
}

func GetAndValidateGeminiRequest(c *gin.Context) (*dto.GeminiChatRequest, error) {
	request := &dto.GeminiChatRequest{}
	err := common.UnmarshalBodyReusable(c, request)
	if err != nil {
		return nil, err
	}
	if len(request.Contents) == 0 && len(request.Requests) == 0 {
		return nil, errors.New("contents is required")
	}
	if ExceedsMaxTokensLimit(request.GenerationConfig.MaxOutputTokens) {
		return nil, errors.New("maxOutputTokens is invalid")
	}

	//if c.Query("alt") == "sse" {
	//	relayInfo.IsStream = true
	//}

	return request, nil
}

func GetAndValidateGeminiEmbeddingRequest(c *gin.Context) (*dto.GeminiEmbeddingRequest, error) {
	request := &dto.GeminiEmbeddingRequest{}
	err := common.UnmarshalBodyReusable(c, request)
	if err != nil {
		return nil, err
	}
	return request, nil
}

func GetAndValidateGeminiBatchEmbeddingRequest(c *gin.Context) (*dto.GeminiBatchEmbeddingRequest, error) {
	request := &dto.GeminiBatchEmbeddingRequest{}
	err := common.UnmarshalBodyReusable(c, request)
	if err != nil {
		return nil, err
	}
	return request, nil
}
