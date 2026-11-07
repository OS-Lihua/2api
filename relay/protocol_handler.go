package relay

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/nontextprotocol"
	"github.com/QuantumNous/new-api/relay/channel/textprotocol"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

// ChannelAdaptor composes an account transport and an independently selected
// wire protocol. It owns neither billing nor route selection.
type ChannelAdaptor struct {
	provider       channel.ProviderAdapter
	protocol       channel.ProtocolHandler
	kind           protocol.Type
	requestURLPath string
}

func GetChannelAdaptor(apiType int, upstream protocol.Type) (*ChannelAdaptor, error) {
	handler, err := GetProtocolHandler(upstream)
	if err != nil {
		return nil, err
	}
	provider := GetProviderAdapter(apiType)
	if provider == nil {
		return nil, fmt.Errorf("invalid provider API type %d", apiType)
	}
	handler, err = channel.ComposeProviderProtocol(provider, upstream, handler)
	if err != nil {
		return nil, err
	}
	return &ChannelAdaptor{provider: provider, protocol: handler, kind: upstream}, nil
}

// FixResponse applies the selected Provider's response-envelope repair without
// running protocol presentation or usage processing.
func (a *ChannelAdaptor) FixResponse(c *gin.Context, info *relaycommon.RelayInfo, response *http.Response) (*http.Response, error) {
	fixer, ok := a.provider.(channel.ProviderResponseFixer)
	if !ok {
		return response, nil
	}
	return fixer.FixResponse(c, &channel.ProtocolResponseContext{Protocol: a.kind, ChannelType: info.ChannelType, OpenRouterEnterprise: info.ChannelOtherSettings.IsOpenRouterEnterprise(), IsStream: info.IsStream}, response)
}

func GetProtocolHandler(kind protocol.Type) (channel.ProtocolHandler, error) {
	if channel.IsNonTextProtocol(kind) || strings.HasPrefix(string(kind), "task:") {
		return nontextprotocol.New(kind)
	}
	return textprotocol.New(kind)
}

func (a *ChannelAdaptor) ConvertRequest(c *gin.Context, info *relaycommon.RelayInfo, request any) (any, error) {
	previousPath := info.RequestURLPath
	a.requestURLPath = ""
	converted, err := a.protocol.ConvertRequest(c, info, request)
	if err == nil && (info.RequestURLPath != previousPath || a.kind == protocol.OpenAIImages) {
		a.requestURLPath = info.RequestURLPath
	}
	return converted, err
}

func (a *ChannelAdaptor) ProjectChatRequest(info *relaycommon.RelayInfo, converted any, public *dto.GeneralOpenAIRequest, body []byte) (*dto.GeneralOpenAIRequest, error) {
	if projector, ok := converted.(channel.ChatRequestProjector); ok {
		return projector.ProjectChatRequest(body, *public)
	}
	if _, ok := converted.(*dto.GeneralOpenAIRequest); ok {
		var effective dto.GeneralOpenAIRequest
		if err := common.Unmarshal(body, &effective); err != nil {
			return nil, err
		}
		return &effective, nil
	}
	if len(info.ParamOverride) > 0 {
		return nil, errors.New("native request overrides require a wire projection")
	}
	// An unchanged native conversion retains the already validated public facts.
	effective := *public
	return &effective, nil
}

func (a *ChannelAdaptor) SetChatOutputLimit(converted any, body []byte, limit uint) ([]byte, error) {
	if setter, ok := converted.(channel.ChatOutputLimitSetter); ok {
		return setter.SetChatOutputLimit(body, limit)
	}
	if _, ok := converted.(*dto.GeneralOpenAIRequest); !ok {
		return body, nil
	}
	var wire map[string]common.RawMessage
	if err := common.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	encoded, err := common.Marshal(limit)
	if err != nil {
		return nil, err
	}
	wire["max_completion_tokens"] = encoded
	return common.Marshal(wire)
}

func (a *ChannelAdaptor) providerMetadata(info *relaycommon.RelayInfo) *channel.ProviderRequest {
	metadata := channel.NewProviderMetadata(info)
	metadata.Protocol = a.kind
	switch a.kind {
	case protocol.OpenAIChat:
		metadata.RelayFormat = types.RelayFormatOpenAI
		metadata.RelayMode = relayconstant.RelayModeChatCompletions
		metadata.RequestURLPath = "/v1/chat/completions"
	case protocol.OpenAIResponses:
		metadata.RelayFormat = types.RelayFormatOpenAIResponses
		metadata.RelayMode = relayconstant.RelayModeResponses
		metadata.RequestURLPath = "/v1/responses"
	case protocol.AnthropicMessages:
		metadata.RelayFormat = types.RelayFormatClaude
		metadata.RelayMode = relayconstant.RelayModeChatCompletions
		metadata.RequestURLPath = "/v1/messages"
	case protocol.GeminiGenerate:
		metadata.RelayFormat = types.RelayFormatGemini
		metadata.RelayMode = relayconstant.RelayModeGemini
		metadata.RequestURLPath = "/v1beta/models/" + info.UpstreamModelName + ":generateContent"
		if info.IsStream {
			metadata.RequestURLPath = "/v1beta/models/" + info.UpstreamModelName + ":streamGenerateContent?alt=sse"
		}
	case protocol.OpenAIEmbeddings:
		metadata.RelayFormat = types.RelayFormatEmbedding
		metadata.RelayMode = relayconstant.RelayModeEmbeddings
		metadata.RequestURLPath = "/v1/embeddings"
	case protocol.GeminiEmbed:
		metadata.RelayFormat = types.RelayFormatGemini
		metadata.RelayMode = relayconstant.RelayModeEmbeddings
		action := ":embedContent"
		if info.IsGeminiBatchEmbedding {
			action = ":batchEmbedContents"
		}
		metadata.RequestURLPath = "/v1beta/models/" + info.UpstreamModelName + action
	case protocol.OpenAIImages:
		metadata.RelayFormat = types.RelayFormatOpenAIImage
		metadata.RelayMode = relayconstant.RelayModeImagesGenerations
		metadata.RequestURLPath = "/v1/images/generations"
		if info.RelayMode == relayconstant.RelayModeImagesEdits {
			metadata.RelayMode = info.RelayMode
			metadata.RequestURLPath = "/v1/images/edits"
		}
	case protocol.OpenAIAudioSpeech:
		metadata.RelayFormat = types.RelayFormatOpenAIAudio
		metadata.RelayMode = relayconstant.RelayModeAudioSpeech
		metadata.RequestURLPath = "/v1/audio/speech"
	case protocol.OpenAIAudioTranscription:
		metadata.RelayFormat = types.RelayFormatOpenAIAudio
		metadata.RelayMode = relayconstant.RelayModeAudioTranscription
		metadata.RequestURLPath = "/v1/audio/transcriptions"
		if info.RelayMode == relayconstant.RelayModeAudioTranslation {
			metadata.RelayMode = info.RelayMode
			metadata.RequestURLPath = "/v1/audio/translations"
		}
	case protocol.Rerank:
		metadata.RelayFormat = types.RelayFormatRerank
		metadata.RelayMode = relayconstant.RelayModeRerank
		metadata.RequestURLPath = "/v1/rerank"
	}
	if a.requestURLPath != "" {
		metadata.RequestURLPath = a.requestURLPath
	}
	return metadata
}

// PreparedChannelRequest holds the existing transport's destination, body and
// final headers in request memory. Preparing it performs no upstream I/O.
type PreparedChannelRequest struct {
	adaptor  *ChannelAdaptor
	request  *http.Request
	metadata *channel.ProviderRequest
	body     io.Reader
	execute  channel.ProviderExecution
}

func (r *PreparedChannelRequest) Headers() http.Header { return r.request.Header.Clone() }

func (a *ChannelAdaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (any, error) {
	prepared, err := a.PrepareRequest(c, info, requestBody)
	if err != nil {
		return nil, err
	}
	return a.DoPreparedRequest(c, info, prepared)
}

func (a *ChannelAdaptor) PrepareRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (*PreparedChannelRequest, error) {
	if c == nil || c.Request == nil || info == nil || info.ProviderMeta == nil {
		return nil, errors.New("missing upstream request metadata")
	}
	if strings.TrimSpace(info.ChannelSetting.Proxy) != "" {
		if _, _, err := common.ParseProxyURLRuntime(info.ChannelSetting.Proxy); err != nil {
			return nil, errors.New("invalid provider proxy URL")
		}
	}
	metadata := a.providerMetadata(info)
	if providerNeedsPayload(a.provider) {
		payload, err := io.ReadAll(requestBody)
		if err != nil {
			return nil, errors.New("could not read upstream request")
		}
		metadata.Payload = payload
		requestBody = bytes.NewReader(payload)
	}
	requestURL, err := a.provider.RequestURL(metadata)
	if err != nil {
		return nil, errors.New("provider URL construction failed")
	}
	info.DisablePing = metadata.DisablePing
	request := &http.Request{Header: make(http.Header)}
	if requestURL != "" {
		request, err = http.NewRequestWithContext(c.Request.Context(), c.Request.Method, requestURL, requestBody)
		if err != nil || request.URL.Hostname() == "" {
			return nil, errors.New("invalid upstream request URL")
		}
		request.URL.Scheme = strings.ToLower(request.URL.Scheme)
		_, hasExecutor := a.provider.(channel.ProviderRequestExecutor)
		sdkWebsocket := hasExecutor && (request.URL.Scheme == "ws" || request.URL.Scheme == "wss")
		if request.URL.Scheme != "http" && request.URL.Scheme != "https" && !sdkWebsocket {
			return nil, errors.New("invalid upstream request URL scheme")
		}
		if channel.IsNonTextProtocol(a.kind) {
			request.Header.Set("Content-Type", c.Request.Header.Get("Content-Type"))
		}
		channel.ApplyUpstreamBodyMetadata(request, requestBody)
	}
	if err := a.provider.SetupHeader(c, &request.Header, metadata); err != nil {
		return nil, errors.New("provider authentication failed")
	}
	if err := channel.ApplyHeaderOverride(info, c, request); err != nil {
		return nil, errors.New("invalid provider header override")
	}
	metadata.RequestHeaders = request.Header.Clone()
	prepared := &PreparedChannelRequest{adaptor: a, request: request, metadata: metadata, body: requestBody}
	if preparer, ok := a.provider.(channel.ProviderRequestPreparer); ok {
		execute, err := preparer.PrepareExecution(c, metadata, requestBody)
		if err != nil || execute == nil {
			return nil, errors.New("provider execution preparation failed")
		}
		prepared.execute = execute
	}
	return prepared, nil
}

func (a *ChannelAdaptor) DoPreparedRequest(c *gin.Context, info *relaycommon.RelayInfo, prepared *PreparedChannelRequest) (any, error) {
	if prepared == nil || prepared.adaptor != a {
		return nil, errors.New("missing prepared upstream request")
	}
	if prepared.execute != nil {
		response, err := prepared.execute(c)
		if err != nil {
			return nil, providerRequestError(err)
		}
		return response, nil
	}
	if executor, ok := a.provider.(channel.ProviderRequestExecutor); ok {
		response, executeErr := executor.ExecuteRequest(c, prepared.metadata, prepared.body)
		if !errors.Is(executeErr, channel.ErrUseHTTPTransport) {
			if executeErr != nil {
				return nil, providerRequestError(executeErr)
			}
			if response == nil {
				return nil, errors.New("provider did not return an upstream response")
			}
			return response, nil
		}
	}
	if prepared.request.URL == nil {
		return nil, errors.New("provider did not supply an upstream destination")
	}
	response, err := channel.DoRequest(c, prepared.request.WithContext(c.Request.Context()), info)
	if err != nil {
		return nil, providerRequestError(err)
	}
	return response, nil
}

func providerRequestError(err error) *types.NewAPIError {
	var apiErr *types.NewAPIError
	if errors.As(err, &apiErr) {
		failure := apiErr.ToOpenAIError()
		redacted := *apiErr
		redacted.Err = errors.New("upstream request failed")
		redacted.RelayError = types.OpenAIError{Message: "upstream request failed", Type: failure.Type, Code: failure.Code}
		redacted.Metadata = nil
		return &redacted
	}
	return types.NewError(errors.New("upstream request failed"), types.ErrorCodeDoRequestFailed)
}

func (a *ChannelAdaptor) DoResponse(c *gin.Context, response *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	info.UpstreamProtocol = a.kind
	return a.protocol.DoResponse(c, response, info)
}
