package relay

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/textprotocol"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

// ChannelAdaptor composes an account transport and an independently selected
// wire protocol. It owns neither billing nor route selection.
type ChannelAdaptor struct {
	provider channel.ProviderAdapter
	protocol channel.ProtocolHandler
	kind     protocol.Type
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

func GetProtocolHandler(kind protocol.Type) (channel.ProtocolHandler, error) {
	return textprotocol.New(kind)
}

func (a *ChannelAdaptor) ConvertRequest(c *gin.Context, info *relaycommon.RelayInfo, request any) (any, error) {
	return a.protocol.ConvertRequest(c, info, request)
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
	}
	return metadata
}

func (a *ChannelAdaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (any, error) {
	if c == nil || c.Request == nil || info == nil || info.ProviderMeta == nil {
		return nil, errors.New("missing upstream request metadata")
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
		if err != nil {
			return nil, errors.New("invalid upstream request URL")
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
	if executor, ok := a.provider.(channel.ProviderRequestExecutor); ok {
		response, executeErr := executor.ExecuteRequest(c, metadata, requestBody)
		if !errors.Is(executeErr, channel.ErrUseHTTPTransport) {
			if executeErr != nil {
				return nil, providerRequestError(executeErr)
			}
			return response, nil
		}
	}
	if requestURL == "" {
		return nil, errors.New("provider did not supply an upstream destination")
	}
	response, err := channel.DoRequest(c, request, info)
	if err != nil {
		return nil, providerRequestError(err)
	}
	return response, nil
}

func providerRequestError(err error) *types.NewAPIError {
	var apiErr *types.NewAPIError
	if errors.As(err, &apiErr) {
		redacted := *apiErr
		redacted.Err = errors.New("upstream request failed")
		redacted.RelayError = nil
		redacted.Metadata = nil
		return &redacted
	}
	return types.NewError(errors.New("upstream request failed"), types.ErrorCodeDoRequestFailed)
}

func (a *ChannelAdaptor) DoResponse(c *gin.Context, response *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	info.UpstreamProtocol = a.kind
	return a.protocol.DoResponse(c, response, info)
}
