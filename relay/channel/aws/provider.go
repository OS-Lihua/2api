package aws

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/gin-gonic/gin"
	"net/http"
	"strings"
	"time"
)

type Provider struct{ ClientMode ClientMode }

var _ channel.ProviderAdapter = (*Provider)(nil)

func (a *Protocol) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	provider := &Provider{ClientMode: a.ClientMode}
	result, err := provider.RequestURL(channel.NewProviderMetadata(info))
	a.ClientMode = provider.ClientMode
	return result, err
}

func (a *Protocol) SetupRequestHeader(c *gin.Context, header *http.Header, info *relaycommon.RelayInfo) error {
	return (&Provider{ClientMode: a.ClientMode}).SetupHeader(c, header, channel.NewProviderMetadata(info))
}

// SignRequest authenticates the final Mantle payload after host header overrides.
func (a *Provider) SignRequest(req *http.Request, metadata channel.ProviderMetadata, body []byte) error {
	info := metadata.ProviderRequest()
	parts := strings.Split(info.ApiKey, "|")
	if len(parts) != 2 && len(parts) != 3 {
		return fmt.Errorf("invalid AWS credentials")
	}
	for _, connection := range req.Header.Values("Connection") {
		for name := range strings.SplitSeq(connection, ",") {
			req.Header.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		req.Header.Del(name)
	}
	if len(parts) == 2 {
		return nil
	}
	req.Header.Del("Authorization")
	req.Header.Del("x-api-key")
	credentials := aws.Credentials{AccessKeyID: parts[0], SecretAccessKey: parts[1]}
	hash := sha256.Sum256(body)
	if err := v4.NewSigner().SignHTTP(req.Context(), credentials, req, hex.EncodeToString(hash[:]), "bedrock-mantle", parts[2], time.Now()); err != nil {
		return fmt.Errorf("sign AWS Mantle request: %w", err)
	}
	return nil
}
