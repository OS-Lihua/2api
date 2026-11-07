package tencent

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"net/http"
)

type Provider struct {
	Sign      string
	Action    string
	Version   string
	Timestamp int64
}

var _ channel.ProviderAdapter = (*Provider)(nil)

func (a *Protocol) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	return (&Provider{}).RequestURL(channel.NewProviderMetadata(info))
}

func (a *Protocol) SetupRequestHeader(c *gin.Context, header *http.Header, info *relaycommon.RelayInfo) error {
	return (&Provider{Sign: a.Sign, Action: a.Action, Version: a.Version, Timestamp: a.Timestamp}).SetupHeader(c, header, channel.NewProviderMetadata(info))
}

func (a *Provider) initSigning() {
	if a.Action == "" {
		a.Action = "ChatCompletions"
	}
	if a.Version == "" {
		a.Version = "2023-09-01"
	}
	if a.Timestamp == 0 {
		a.Timestamp = common.GetTimestamp()
	}
}
