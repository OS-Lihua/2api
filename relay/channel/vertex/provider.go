package vertex

import (
	"context"
	"errors"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"net/http"
	"strings"
)

type Provider struct {
	RequestMode        int
	AccountCredentials Credentials
	accessToken        string
}

var _ channel.ProviderAdapter = (*Provider)(nil)
var _ channel.ProviderHeaderPreviewer = (*Provider)(nil)
var _ channel.ProviderAccountPreparer = (*Provider)(nil)

func (a *Provider) PrepareAccount(ctx context.Context, metadata channel.ProviderMetadata, begin func() (context.Context, error)) error {
	info := metadata.ProviderRequest()
	if info.ChannelOtherSettings.VertexKeyType == dto.VertexKeyTypeAPIKey {
		return nil
	}
	if err := common.Unmarshal([]byte(info.ApiKey), &a.AccountCredentials); err != nil {
		return errors.New("failed to decode credentials file")
	}
	var err error
	a.accessToken, err = getProviderAccessTokenWithContext(ctx, a, info, begin)
	return err
}

func (a *Provider) initRequestMode(info *channel.ProviderRequest) {
	if info.Protocol != "" {
		switch info.Protocol {
		case protocol.AnthropicMessages:
			a.RequestMode = RequestModeClaude
		case protocol.GeminiGenerate, protocol.GeminiEmbed, protocol.OpenAIImages:
			a.RequestMode = RequestModeGemini
		case protocol.OpenAIChat:
			a.RequestMode = RequestModeOpenSource
		default:
			a.RequestMode = 0
		}
		return
	}
	if a.RequestMode != 0 {
		return
	}
	if strings.HasPrefix(info.UpstreamModelName, "claude") {
		a.RequestMode = RequestModeClaude
	} else if strings.Contains(info.UpstreamModelName, "llama") || strings.Contains(info.UpstreamModelName, "-maas") {
		a.RequestMode = RequestModeOpenSource
	} else {
		a.RequestMode = RequestModeGemini
	}
}

func (a *Protocol) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	provider := &Provider{RequestMode: a.RequestMode, AccountCredentials: a.AccountCredentials}
	result, err := provider.RequestURL(channel.NewProviderMetadata(info))
	a.AccountCredentials = provider.AccountCredentials
	return result, err
}

func (a *Protocol) SetupRequestHeader(c *gin.Context, header *http.Header, info *relaycommon.RelayInfo) error {
	provider := &Provider{RequestMode: a.RequestMode, AccountCredentials: a.AccountCredentials}
	err := provider.SetupHeader(c, header, channel.NewProviderMetadata(info))
	a.AccountCredentials = provider.AccountCredentials
	return err
}
