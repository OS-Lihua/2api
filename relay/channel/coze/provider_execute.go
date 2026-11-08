package coze

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// cozeResultBody carries the unmodified completion envelope alongside the
// message-list body. The protocol parser owns extraction of its usage fields.
type cozeResultBody struct {
	io.ReadCloser
	completion     CozeChatResponse
	completionBody []byte
	failure        *types.NewAPIError
}

func (body *cozeResultBody) ResponseError() *types.NewAPIError { return body.failure }

func (body *cozeResultBody) SetReader(reader io.ReadCloser) { body.ReadCloser = reader }

var _ channel.ProviderRequestExecutor = (*Provider)(nil)

func (p *Provider) ExecuteRequest(c *gin.Context, metadata channel.ProviderMetadata, body io.Reader) (*http.Response, error) {
	account := metadata.ProviderRequest()
	if account.IsStream {
		return nil, channel.ErrUseHTTPTransport
	}
	client, err := service.GetHttpClientWithProxySettings(account.ChannelSetting.Proxy, account.ChannelSetting.HTTPSettings())
	if err != nil {
		return nil, err
	}
	if value, ok := common.GetContextKey(c, constant.ContextKeyResponseFacts); ok {
		if facts, _ := value.(*relaycommon.ResponseFacts); facts != nil && facts.OnBusinessOutput != nil {
			client = channel.GatewayHTTPClient(client)
		}
	}
	destination, err := p.RequestURL(metadata)
	if err != nil {
		return nil, err
	}
	request, err := p.newChatRequest(c, metadata, http.MethodPost, destination, body)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	var created CozeChatResponse
	createdBody, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	response.Body.Close()
	err = common.Unmarshal(createdBody, &created)
	if response.StatusCode >= 400 {
		response.Body = &cozeResultBody{ReadCloser: io.NopCloser(bytes.NewReader(createdBody)), completion: created, completionBody: createdBody}
		return response, nil
	}
	if readErr != nil {
		return nil, readErr
	}
	if err != nil {
		return nil, err
	}
	if created.Code != 0 {
		rejection := types.NewOpenAIError(fmt.Errorf("Coze chat creation rejected"), types.ErrorCodeBadResponse, response.StatusCode)
		failure := rejection.ToOpenAIError()
		failure.Code = fmt.Sprint(created.Code)
		rejection.RelayError = failure
		return nil, rejection
	}
	query := url.Values{"conversation_id": {created.Data.ConversationId}, "chat_id": {created.Data.Id}}
	retrieveURL := account.ChannelBaseUrl + "/v3/chat/retrieve?" + query.Encode()
	for {
		poll, err := p.newChatRequest(c, metadata, http.MethodGet, retrieveURL, nil)
		if err != nil {
			return nil, err
		}
		response, err := client.Do(poll)
		if err != nil {
			return nil, err
		}
		var completion CozeChatResponse
		completionBody, err := io.ReadAll(response.Body)
		response.Body.Close()
		decodeBody := completionBody
		if facts, _ := common.GetContextKey(c, constant.ContextKeyResponseFacts); facts != nil {
			decodeBody = relaycommon.SanitizeNativeUsage(decodeBody, "data.usage")
		}
		decodeErr := common.Unmarshal(decodeBody, &completion)
		if facts, _ := common.GetContextKey(c, constant.ContextKeyResponseFacts); facts != nil &&
			(response.StatusCode >= 400 || completion.Code != 0 || completion.Data.Status == "failed" || completion.Data.Status == "canceled" || completion.Data.Status == "requires_action") {
			if response.StatusCode < 400 {
				response.StatusCode = http.StatusBadGateway
			}
			response.Body = &cozeResultBody{ReadCloser: io.NopCloser(bytes.NewReader(completionBody)), completion: completion, completionBody: completionBody}
			return response, nil
		}
		if err != nil {
			if facts, _ := common.GetContextKey(c, constant.ContextKeyResponseFacts); facts != nil {
				response.Body = &cozeResultBody{ReadCloser: io.NopCloser(bytes.NewReader(completionBody)), completion: completion, completionBody: completionBody, failure: types.NewError(err, types.ErrorCodeReadResponseBodyFailed)}
				return response, nil
			}
			return nil, err
		}
		if decodeErr != nil {
			return nil, decodeErr
		}
		if completion.Code != 0 {
			return nil, fmt.Errorf("Coze chat polling failed: %s", completion.Msg)
		}
		switch completion.Data.Status {
		case "completed":
			destination := account.ChannelBaseUrl + "/v3/chat/message/list?" + query.Encode()
			request, err := p.newChatRequest(c, metadata, http.MethodGet, destination, nil)
			if err != nil {
				return nil, err
			}
			response, err := client.Do(request)
			if err != nil {
				if facts, _ := common.GetContextKey(c, constant.ContextKeyResponseFacts); facts != nil {
					// Completed generation remains payable when fetching its messages fails.
					return &http.Response{
						StatusCode: http.StatusBadGateway,
						Header:     make(http.Header),
						Body:       &cozeResultBody{ReadCloser: io.NopCloser(bytes.NewReader(nil)), completion: completion, completionBody: completionBody},
					}, nil
				}
				return nil, err
			}
			response.Body = &cozeResultBody{ReadCloser: response.Body, completion: completion, completionBody: completionBody}
			return response, nil
		case "failed", "canceled", "requires_action":
			return nil, fmt.Errorf("chat status: %s", completion.Data.Status)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-c.Request.Context().Done():
			timer.Stop()
			return nil, c.Request.Context().Err()
		case <-timer.C:
		}
	}
}
func (p *Provider) newChatRequest(c *gin.Context, metadata channel.ProviderMetadata, method, destination string, body io.Reader) (*http.Request, error) {
	request, err := http.NewRequestWithContext(c.Request.Context(), method, destination, body)
	if err != nil {
		return nil, err
	}
	if err := p.SetupHeader(c, &request.Header, metadata); err != nil {
		return nil, err
	}
	for name, values := range metadata.ProviderRequest().RequestHeaders {
		request.Header[name] = append([]string(nil), values...)
		if name == "Host" {
			request.Host = request.Header.Get(name)
		}
	}
	return request, nil
}
