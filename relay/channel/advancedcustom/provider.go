package advancedcustom

import (
	"errors"
	"fmt"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/gin-gonic/gin"
	"net/http"
	"strings"
)

type Provider struct {
	resolved  bool
	route     dto.AdvancedCustomRoute
	converter string
}

var _ channel.ProviderAdapter = (*Provider)(nil)

func (a *Provider) resolveProvider(c *gin.Context, info *channel.ProviderRequest) error {
	if a.resolved {
		return nil
	}
	if info == nil {
		return errors.New("missing provider metadata")
	}
	config := info.ChannelOtherSettings.AdvancedCustom
	if config == nil {
		return errors.New("advanced_custom is required")
	}
	if err := config.Validate(); err != nil {
		return err
	}
	incomingPath, _, _ := strings.Cut(info.RequestURLPath, "?")
	if info.Protocol != "" && info.ClientRequestURLPath != "" {
		incomingPath, _, _ = strings.Cut(info.ClientRequestURLPath, "?")
	}
	if c != nil && c.Request != nil && c.Request.URL != nil {
		incomingPath = c.Request.URL.Path
	}
	route, ok := config.MatchPathForModel(incomingPath, info.OriginModelName)
	if !ok {
		return fmt.Errorf("advanced custom channel does not support request path %s for model %s", incomingPath, info.OriginModelName)
	}
	route.Converter = strings.TrimSpace(route.Converter)
	if route.Converter == "" {
		route.Converter = relayconvert.ConverterNone
	}
	if info.Protocol != "" && route.Converter != relayconvert.ConverterNone &&
		!(info.Protocol == protocol.Rerank && route.Converter == dto.AdvancedCustomConverterSGLangRerank) {
		spec, ok := relayconvert.LookupRequestConverter(route.Converter)
		if !ok || spec.To != info.RelayFormat {
			return fmt.Errorf("advanced custom route converter %q does not target selected protocol %q", route.Converter, info.Protocol)
		}
	}
	a.route = route
	a.converter = route.Converter
	a.resolved = true
	return nil
}

func (a *Protocol) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	metadata := channel.NewProviderMetadata(info)
	defer channel.ApplyProviderMetadata(info, metadata)
	provider := &Provider{resolved: a.resolved, route: a.route, converter: a.converter}
	result, err := provider.RequestURL(metadata)
	a.resolved, a.route, a.converter = provider.resolved, provider.route, provider.converter
	return result, err
}

func (a *Protocol) SetupRequestHeader(c *gin.Context, header *http.Header, info *relaycommon.RelayInfo) error {
	metadata := channel.NewProviderMetadata(info)
	provider := &Provider{resolved: a.resolved, route: a.route, converter: a.converter}
	err := provider.SetupHeader(c, header, metadata)
	a.resolved, a.route, a.converter = provider.resolved, provider.route, provider.converter
	return err
}
