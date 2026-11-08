package aws

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/gin-gonic/gin"
)

var _ channel.ProviderRequestExecutor = (*Provider)(nil)
var _ channel.ProviderRequestPreparer = (*Provider)(nil)

type awsStreamBody struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (body *awsStreamBody) Close() error {
	body.cancel()
	return body.PipeReader.Close()
}

func (p *Provider) ExecuteRequest(c *gin.Context, metadata channel.ProviderMetadata, body io.Reader) (*http.Response, error) {
	execute, err := p.PrepareExecution(c, metadata, body)
	if err != nil {
		return nil, err
	}
	return execute(c)
}

// PrepareExecution validates SDK configuration and freezes the final wire body
// without sending. The returned exchange reads the host's current send context.
func (p *Provider) PrepareExecution(c *gin.Context, metadata channel.ProviderMetadata, body io.Reader) (channel.ProviderExecution, error) {
	request := metadata.ProviderRequest()
	managed := false
	var facts *relaycommon.ResponseFacts
	if c != nil {
		value, _ := c.Get(string(constant.ContextKeyResponseFacts))
		facts, _ = value.(*relaycommon.ResponseFacts)
		managed = facts != nil && facts.OnBusinessOutput != nil
	}
	if request.ChannelOtherSettings.AwsApiType == dto.AwsApiTypeMantle {
		req, err := p.NewMantleRequest(c, metadata, body)
		if err != nil {
			return nil, err
		}
		client, err := service.GetHttpClientWithProxySettings(request.ChannelSetting.Proxy, request.ChannelSetting.HTTPSettings())
		if err != nil {
			return nil, err
		}
		if managed {
			client = channel.GatewayHTTPClient(client)
		}
		return func(c *gin.Context) (*http.Response, error) {
			if c != nil && c.Request != nil {
				req = req.WithContext(c.Request.Context())
			}
			return client.Do(req)
		}, nil
	}
	switch request.ChannelOtherSettings.AwsApiType {
	case "", dto.AwsApiTypeInvokeModel:
	default:
		return nil, fmt.Errorf("invalid AWS API type")
	}
	if request.Protocol != protocol.AnthropicMessages && request.Protocol != protocol.OpenAIChat {
		return nil, fmt.Errorf("unsupported AWS protocol %q", request.Protocol)
	}
	nova := isNovaModel(request.UpstreamModelName)
	if nova != (request.Protocol == protocol.OpenAIChat) {
		return nil, fmt.Errorf("AWS model does not support selected protocol %q", request.Protocol)
	}
	client, err := p.NewClient(metadata)
	if err != nil {
		return nil, err
	}
	modelID := getAwsModelID(request.UpstreamModelName)
	regionPrefix := getAwsRegionPrefix(client.Options().Region)
	if awsModelCanCrossRegion(modelID, regionPrefix) {
		modelID = awsModelCrossRegion(modelID, regionPrefix)
	}
	payload, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	if !nova && !managed {
		wire, err := formatRequest(bytes.NewReader(payload), request.RequestHeaders)
		if err != nil {
			return nil, err
		}
		payload, err = common.Marshal(wire)
		if err != nil {
			return nil, err
		}
	}
	prepared := &awsPreparedInvocation{client: client, modelID: modelID, payload: payload, stream: request.IsStream && !nova, managed: managed, facts: facts}
	if managed {
		httpClient := client.Options().HTTPClient
		if base, ok := httpClient.(*http.Client); ok {
			httpClient = channel.GatewayHTTPClient(base)
		}
		prepared.options = append(prepared.options, func(options *bedrockruntime.Options) {
			options.RetryMaxAttempts = 1
			options.HTTPClient = httpClient
		})
	}
	return prepared.execute, nil
}

type awsPreparedInvocation struct {
	client  *bedrockruntime.Client
	modelID string
	payload []byte
	stream  bool
	managed bool
	facts   *relaycommon.ResponseFacts
	options []func(*bedrockruntime.Options)
}

func (prepared *awsPreparedInvocation) failure(ctx context.Context, err error, operation string) *types.NewAPIError {
	if prepared.facts != nil {
		var source interface{ HTTPResponse() *smithyhttp.Response }
		if errors.As(err, &source) {
			response := source.HTTPResponse()
			if response != nil && response.Response != nil {
				prepared.facts.UpstreamRetryAfter = response.Header.Get("Retry-After")
			}
		}
	}
	return newAwsInvokeError(ctx, err, operation)
}

func (prepared *awsPreparedInvocation) execute(c *gin.Context) (*http.Response, error) {
	parent := context.Background()
	if c != nil && c.Request != nil {
		parent = c.Request.Context()
	}
	ctx, cancel := newAwsInvokeContext(parent, prepared.managed)
	if !prepared.stream {
		defer cancel()
		options := append([]func(*bedrockruntime.Options){}, prepared.options...)
		var capture *awsJSONResponseCapture
		if prepared.managed {
			capture = &awsJSONResponseCapture{}
			options = append(options, func(options *bedrockruntime.Options) {
				capture.client = options.HTTPClient
				options.HTTPClient = capture
			})
		}
		output, err := prepared.client.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{ModelId: aws.String(prepared.modelID), ContentType: aws.String("application/json"), Accept: aws.String("application/json"), Body: prepared.payload}, options...)
		if capture != nil && capture.response != nil && (capture.readErr != nil || err != nil) {
			if err != nil {
				if body, ok := capture.response.Body.(*awsJSONSourceBody); ok {
					body.failure = prepared.failure(parent, err, "InvokeModel")
				}
			}
			return capture.response, nil
		}
		if err != nil {
			return nil, prepared.failure(parent, err, "InvokeModel")
		}
		response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(output.Body))}
		response.Header.Set("Content-Type", "application/json")
		return response, nil
	}
	output, err := prepared.client.InvokeModelWithResponseStream(ctx, &bedrockruntime.InvokeModelWithResponseStreamInput{ModelId: aws.String(prepared.modelID), ContentType: aws.String("application/json"), Accept: aws.String("application/json"), Body: prepared.payload}, prepared.options...)
	if err != nil {
		cancel()
		return nil, prepared.failure(parent, err, "InvokeModelWithResponseStream")
	}
	stream := output.GetStream()
	reader, writer := io.Pipe()
	go func() {
		defer cancel()
		defer stream.Close()
		defer writer.Close()
		for {
			select {
			case <-ctx.Done():
				writer.CloseWithError(ctx.Err())
				return
			case event, ok := <-stream.Events():
				if !ok {
					if err := stream.Err(); err != nil {
						writer.CloseWithError(err)
					}
					return
				}
				chunk, ok := event.(*bedrocktypes.ResponseStreamMemberChunk)
				if !ok {
					writer.CloseWithError(fmt.Errorf("unexpected AWS stream event %T", event))
					return
				}
				if _, err := fmt.Fprintf(writer, "data: %s\n\n", chunk.Value.Bytes); err != nil {
					return
				}
			}
		}
	}()
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: &awsStreamBody{PipeReader: reader, cancel: cancel}}
	response.Header.Set("Content-Type", "text/event-stream")
	return response, nil
}
