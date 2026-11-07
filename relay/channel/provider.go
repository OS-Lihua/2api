package channel

import (
	"net/http"
	"strings"

	appconstant "github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/protocol"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

// ProviderAdapter owns upstream addressing and authentication independently of protocol conversion.
type ProviderAdapter interface {
	RequestURL(metadata ProviderMetadata) (string, error)
	SetupHeader(c *gin.Context, header *http.Header, metadata ProviderMetadata) error
}

// ProviderBalanceRequestBuilder prepares account balance queries independently
// of relay protocol conversion.
type ProviderBalanceRequestBuilder interface {
	BuildBalanceRequest(ProviderMetadata) (string, http.Header, error)
}

// ProviderMetadata exposes only the account and request destination snapshot.
type ProviderMetadata interface {
	ProviderRequest() *ProviderRequest
}

// ProviderTransportSettings contains only settings used when authenticating an upstream connection.
type ProviderTransportSettings struct {
	Proxy                 string
	HTTPProtocol          string
	HTTP2ConnectionShards int
	TLSInsecureSkipVerify bool
}

func (s ProviderTransportSettings) HTTPSettings() dto.ProviderSettings {
	return dto.ProviderSettings{Proxy: s.Proxy, HTTPProtocol: s.HTTPProtocol, HTTP2ConnectionShards: s.HTTP2ConnectionShards, TLSInsecureSkipVerify: s.TLSInsecureSkipVerify}
}

// ProviderAccountSettings contains endpoint and credential selection settings.
type ProviderAccountSettings struct {
	AzureResponsesVersion string
	VertexKeyType         dto.VertexKeyType
	VertexProjectID       string
	VertexAPIVersion      string
	ClaudeBetaQuery       bool
	AwsKeyType            dto.AwsKeyType
	AwsApiType            dto.AwsApiType
	OllamaOpenAIChat      bool
	AdvancedCustom        *dto.AdvancedCustomConfig
}

// ProviderRequest contains no user, session, billing, or protocol conversion state.
// Treat its credentials and URLs as secrets; never log the snapshot.
type ProviderRequest struct {
	Protocol               protocol.Type
	ChannelType            int
	ChannelId              int
	ChannelIsMultiKey      bool
	ChannelMultiKeyIndex   int
	ChannelBaseUrl         string
	ApiVersion             string
	ApiKey                 string `json:"-"`
	Organization           string
	ChannelCreateTime      int64
	HeadersOverride        map[string]any
	ChannelSetting         ProviderTransportSettings
	ChannelOtherSettings   ProviderAccountSettings
	UpstreamModelName      string
	OriginModelName        string
	RequestURLPath         string
	ClientRequestURLPath   string
	RelayMode              int
	RelayFormat            types.RelayFormat
	IsStream               bool
	IsGeminiBatchEmbedding bool
	IsClaudeBetaQuery      bool
	DisablePing            bool
	// Payload is the final encoded request body when authentication requires a payload signature.
	Payload []byte `json:"-"`
	// RequestHeaders are the final headers after administrator overrides.
	RequestHeaders http.Header `json:"-"`
}

func (r *ProviderRequest) ProviderRequest() *ProviderRequest { return r }

// NewProviderMetadata is the host compatibility boundary. Only addressing and
// authentication settings cross into providers; the returned value retains no RelayInfo.
func NewProviderMetadata(info *relaycommon.RelayInfo) *ProviderRequest {
	if info == nil {
		return nil
	}
	r := &ProviderRequest{
		ChannelType: info.ChannelType, ChannelId: info.ChannelId,
		ChannelIsMultiKey: info.ChannelIsMultiKey, ChannelMultiKeyIndex: info.ChannelMultiKeyIndex,
		ChannelBaseUrl: info.ChannelBaseUrl, ApiVersion: info.ApiVersion, ApiKey: info.ApiKey,
		Organization: info.Organization, ChannelCreateTime: info.ChannelCreateTime,
		UpstreamModelName: info.UpstreamModelName, OriginModelName: info.OriginModelName,
		RequestURLPath: info.RequestURLPath, ClientRequestURLPath: info.RequestURLPath, RelayMode: info.RelayMode, RelayFormat: info.RelayFormat,
		IsStream: info.IsStream, IsGeminiBatchEmbedding: info.IsGeminiBatchEmbedding,
		IsClaudeBetaQuery: info.IsClaudeBetaQuery, DisablePing: info.DisablePing,
		ChannelSetting: ProviderTransportSettings{Proxy: info.ChannelSetting.Proxy,
			HTTPProtocol: info.ChannelSetting.HTTPProtocol, HTTP2ConnectionShards: info.ChannelSetting.HTTP2ConnectionShards,
			TLSInsecureSkipVerify: info.ChannelSetting.TLSInsecureSkipVerify},
		ChannelOtherSettings: ProviderAccountSettings{
			AzureResponsesVersion: info.ChannelOtherSettings.AzureResponsesVersion,
			VertexKeyType:         info.ChannelOtherSettings.VertexKeyType, VertexProjectID: info.ChannelOtherSettings.VertexProjectID,
			VertexAPIVersion: info.ChannelOtherSettings.VertexAPIVersion,
			ClaudeBetaQuery:  info.ChannelOtherSettings.ClaudeBetaQuery,
			AwsKeyType:       info.ChannelOtherSettings.AwsKeyType, AwsApiType: info.ChannelOtherSettings.AwsApiType,
			OllamaOpenAIChat: info.ChannelOtherSettings.OllamaOpenAIChat,
		},
	}
	if info.HeadersOverride != nil {
		r.HeadersOverride = make(map[string]any, len(info.HeadersOverride))
		for name := range info.HeadersOverride {
			r.HeadersOverride[name] = nil
		}
	}
	if config := info.ChannelOtherSettings.AdvancedCustom; config != nil {
		copyConfig := &dto.AdvancedCustomConfig{Routes: make([]dto.AdvancedCustomRoute, len(config.Routes))}
		for i, route := range config.Routes {
			copyConfig.Routes[i] = route
			copyConfig.Routes[i].Models = append([]string(nil), route.Models...)
			if route.Auth != nil {
				auth := *route.Auth
				copyConfig.Routes[i].Auth = &auth
			}
		}
		r.ChannelOtherSettings.AdvancedCustom = copyConfig
	}
	return r
}

// ApplyProviderMetadata preserves the legacy mutations made while choosing a destination.
func ApplyProviderMetadata(info *relaycommon.RelayInfo, request *ProviderRequest) {
	if info == nil || request == nil {
		return
	}
	info.ChannelBaseUrl = request.ChannelBaseUrl
	info.DisablePing = request.DisablePing
}

func SetupProviderRequestHeader(info *ProviderRequest, c *gin.Context, header *http.Header) {
	if info.RelayMode == constant.RelayModeAudioTranscription || info.RelayMode == constant.RelayModeAudioTranslation || info.RelayMode == constant.RelayModeRealtime {
		return
	}
	if c == nil || c.Request == nil {
		return
	}
	header.Set("Content-Type", c.Request.Header.Get("Content-Type"))
	header.Set("Accept", c.Request.Header.Get("Accept"))
	if info.IsStream && c.Request.Header.Get("Accept") == "" {
		header.Set("Accept", "text/event-stream")
	}
}

// ProviderRequestURL joins the upstream base and path, including Cloudflare gateway routing.
func ProviderRequestURL(baseURL, requestURL string, channelType int) string {
	if strings.HasPrefix(baseURL, "https://gateway.ai.cloudflare.com") {
		switch channelType {
		case appconstant.ChannelTypeOpenAI:
			requestURL = strings.TrimPrefix(requestURL, "/v1")
		case appconstant.ChannelTypeAzure:
			requestURL = strings.TrimPrefix(requestURL, "/openai/deployments")
		}
	}
	return baseURL + requestURL
}

// ProviderHeader reads an optional incoming header for provider-specific negotiation.
func ProviderHeader(c *gin.Context, name string) string {
	if c == nil || c.Request == nil {
		return ""
	}
	return strings.Join(c.Request.Header.Values(name), ",")
}
