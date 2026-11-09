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
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// cozeResultBody carries cumulative per-field procurement facts alongside the
// message-list body. Missing fields never erase preceding reported values.
type cozeResultBody struct {
	io.ReadCloser
	usage      dto.Usage
	completion CozeChatResponse
	usageFacts relaycommon.ResponseFacts
	failure    *types.NewAPIError
}

func (body *cozeResultBody) ResponseError() *types.NewAPIError { return body.failure }
func (body *cozeResultBody) SetReader(reader io.ReadCloser)    { body.ReadCloser = reader }
func (body *cozeResultBody) observe(raw []byte) {
	body.usageFacts.RecordNativeUsage(&body.usage, raw, "data.usage.input_count", "data.usage.output_count")
}
func (body *cozeResultBody) failedResponse(response *http.Response, failure *types.NewAPIError) *http.Response {
	body.ReadCloser = io.NopCloser(bytes.NewReader(nil))
	body.failure = failure
	response.Body = body
	return response
}

var _ channel.ProviderRequestExecutor = (*Provider)(nil)

func (p *Provider) ExecuteRequest(c *gin.Context, metadata channel.ProviderMetadata, body io.Reader) (*http.Response, error) {
	account := metadata.ProviderRequest()
	factsValue, _ := common.GetContextKey(c, constant.ContextKeyResponseFacts)
	managed := factsValue != nil
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
	if managed && response != nil {
		response.Body = channel.BoundGatewayResponseBody(response.Body)
	}
	if err != nil {
		return nil, err
	}
	result := &cozeResultBody{}
	var created CozeChatResponse
	createdBody, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	response.Body.Close()
	result.observe(createdBody)
	err = common.Unmarshal(createdBody, &created)
	result.completion = created
	if response.StatusCode >= 400 {
		result.ReadCloser = io.NopCloser(bytes.NewReader(createdBody))
		response.Body = result
		return response, nil
	}
	if readErr != nil {
		if managed {
			return result.failedResponse(response, types.NewError(readErr, types.ErrorCodeReadResponseBodyFailed)), nil
		}
		return nil, readErr
	}
	if err != nil {
		if managed {
			return result.failedResponse(response, types.NewError(err, types.ErrorCodeBadResponseBody)), nil
		}
		return nil, err
	}
	if created.Code != 0 {
		rejection := types.NewOpenAIError(fmt.Errorf("Coze chat creation rejected"), types.ErrorCodeBadResponse, response.StatusCode)
		failure := rejection.ToOpenAIError()
		failure.Code = fmt.Sprint(created.Code)
		rejection.RelayError = failure
		if managed {
			return result.failedResponse(response, rejection), nil
		}
		return nil, rejection
	}
	query := url.Values{"conversation_id": {created.Data.ConversationId}, "chat_id": {created.Data.Id}}
	retrieveURL := account.ChannelBaseUrl + "/v3/chat/retrieve?" + query.Encode()
	lastResponse := response
	for {
		poll, err := p.newChatRequest(c, metadata, http.MethodGet, retrieveURL, nil)
		if err != nil {
			if managed {
				return result.failedResponse(lastResponse, types.NewError(err, types.ErrorCodeDoRequestFailed)), nil
			}
			return nil, err
		}
		response, err := client.Do(poll)
		if managed && response != nil {
			response.Body = channel.BoundGatewayResponseBody(response.Body)
		}
		if err != nil {
			if managed {
				return result.failedResponse(lastResponse, types.NewError(err, types.ErrorCodeDoRequestFailed)), nil
			}
			return nil, err
		}
		completionBody, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		result.observe(completionBody)
		var completion CozeChatResponse
		decodeBody := completionBody
		if managed {
			decodeBody = relaycommon.SanitizeNativeUsage(decodeBody, "data.usage")
		}
		decodeErr := common.Unmarshal(decodeBody, &completion)
		result.completion = completion
		lastResponse = response
		if managed && (response.StatusCode >= 400 || completion.Code != 0 || completion.Data.Status == "failed" || completion.Data.Status == "canceled" || completion.Data.Status == "requires_action") {
			if response.StatusCode < 400 {
				response.StatusCode = http.StatusBadGateway
			}
			result.ReadCloser = io.NopCloser(bytes.NewReader(completionBody))
			response.Body = result
			return response, nil
		}
		if readErr != nil {
			if managed {
				return result.failedResponse(response, types.NewError(readErr, types.ErrorCodeReadResponseBodyFailed)), nil
			}
			return nil, readErr
		}
		if decodeErr != nil {
			if managed {
				return result.failedResponse(response, types.NewError(decodeErr, types.ErrorCodeBadResponseBody)), nil
			}
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
				if managed {
					return result.failedResponse(response, types.NewError(err, types.ErrorCodeDoRequestFailed)), nil
				}
				return nil, err
			}
			fetched, err := client.Do(request)
			if managed && fetched != nil {
				fetched.Body = channel.BoundGatewayResponseBody(fetched.Body)
			}
			if err != nil {
				if managed {
					return result.failedResponse(response, types.NewError(err, types.ErrorCodeDoRequestFailed)), nil
				}
				return nil, err
			}
			result.ReadCloser = fetched.Body
			fetched.Body = result
			return fetched, nil
		case "failed", "canceled", "requires_action":
			return nil, fmt.Errorf("chat status: %s", completion.Data.Status)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-c.Request.Context().Done():
			timer.Stop()
			if managed {
				return result.failedResponse(lastResponse, types.NewError(c.Request.Context().Err(), types.ErrorCodeDoRequestFailed)), nil
			}
			return nil, c.Request.Context().Err()
		case <-timer.C:
		}
	}
}

func (p *Provider) newChatRequest(c *gin.Context, metadata channel.ProviderMetadata, method, destination string, body io.Reader) (*http.Request, error) {
	ctx := c.Request.Context()
	if method == http.MethodGet && metadata.ProviderRequest().BeginFollowupCall != nil {
		var err error
		ctx, err = metadata.ProviderRequest().BeginFollowupCall()
		if err != nil {
			return nil, err
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, destination, body)
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
