package baidu

import (
	"context"
	"errors"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"net/http"
	"strings"
	"time"
)

// Provider owns upstream addressing and authentication.
type Provider struct{ accessToken string }

func (p *Provider) PrepareAccount(ctx context.Context, metadata channel.ProviderMetadata, begin func() (context.Context, error)) error {
	account := metadata.ProviderRequest()
	if len(strings.Split(account.ApiKey, "|")) != 2 {
		return errors.New("invalid Baidu credentials")
	}
	if cached, ok := baiduTokenStore.Load(account.ApiKey); ok {
		if token, ok := cached.(BaiduAccessToken); ok && clock.Now().Add(time.Hour).Before(token.ExpiresAt) {
			p.accessToken = token.AccessToken
			return nil
		}
	}
	client, err := service.GetVerifiedHttpClientWithProxy(account.ChannelSetting.Proxy)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	if begin != nil {
		ctx, err = begin()
		if err != nil {
			return err
		}
		client = channel.GatewayHTTPClient(client)
	}
	token, err := getBaiduAccessTokenHelper(ctx, client, account.ApiKey)
	if err != nil {
		return err
	}
	p.accessToken = token.AccessToken
	return nil
}

var _ channel.ProviderAdapter = (*Provider)(nil)

func (a *Protocol) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	metadata := channel.NewProviderMetadata(info)
	defer channel.ApplyProviderMetadata(info, metadata)
	return (&Provider{}).RequestURL(metadata)
}

func (a *Protocol) SetupRequestHeader(c *gin.Context, header *http.Header, info *relaycommon.RelayInfo) error {
	metadata := channel.NewProviderMetadata(info)
	defer channel.ApplyProviderMetadata(info, metadata)
	return (&Provider{}).SetupHeader(c, header, metadata)
}
