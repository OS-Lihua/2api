package openai

import (
	"bytes"
	"fmt"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/openrouter"
	"github.com/gin-gonic/gin"
)

// unwrapEnterpriseResponse owns the OpenRouter account response envelope. Public
// Chat parsing and usage extraction operate only on the enclosed payload.
func (p *Provider) unwrapEnterpriseResponse(body []byte) ([]byte, error) {
	var envelope openrouter.OpenRouterEnterpriseResponse
	if err := common.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	if !envelope.Success {
		return nil, fmt.Errorf("openrouter response success=false")
	}
	return envelope.Data, nil
}
func (p *Provider) FixResponse(c *gin.Context, context *channel.ProtocolResponseContext, response *http.Response) (*http.Response, error) {
	if context.Protocol != protocol.OpenAIChat || context.ChannelType != constant.ChannelTypeOpenRouter || !context.OpenRouterEnterprise || context.IsStream {
		return response, nil
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return nil, err
	}
	body, err = p.unwrapEnterpriseResponse(body)
	if err != nil {
		return nil, err
	}
	copy := *response
	copy.Header = response.Header.Clone()
	copy.Body = io.NopCloser(bytes.NewReader(body))
	copy.ContentLength = int64(len(body))
	copy.Header.Del("Content-Length")
	return &copy, nil
}
