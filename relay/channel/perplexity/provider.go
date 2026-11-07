package perplexity

import (
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"net/http"
)

// Provider owns upstream addressing and authentication.
type Provider struct{}

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
