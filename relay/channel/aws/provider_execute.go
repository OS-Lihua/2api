package aws

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/gin-gonic/gin"
)

var _ channel.ProviderRequestExecutor = (*Provider)(nil)

type awsStreamBody struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (body *awsStreamBody) Close() error {
	body.cancel()
	return body.PipeReader.Close()
}

func (p *Provider) ExecuteRequest(c *gin.Context, metadata channel.ProviderMetadata, body io.Reader) (*http.Response, error) {
	request := metadata.ProviderRequest()
	if request.ChannelOtherSettings.AwsApiType == dto.AwsApiTypeMantle {
		req, err := p.NewMantleRequest(c, metadata, body)
		if err != nil {
			return nil, err
		}
		client, err := service.GetHttpClientWithProxySettings(request.ChannelSetting.Proxy, request.ChannelSetting.HTTPSettings())
		if err != nil {
			return nil, err
		}
		return client.Do(req)
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
	if !nova {
		wire, err := formatRequest(bytes.NewReader(payload), request.RequestHeaders)
		if err != nil {
			return nil, err
		}
		payload, err = common.Marshal(wire)
		if err != nil {
			return nil, err
		}
	}
	parent := context.Background()
	if c != nil && c.Request != nil {
		parent = c.Request.Context()
	}
	ctx, cancel := newAwsInvokeContext(parent)
	if !request.IsStream || nova {
		defer cancel()
		output, err := client.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{ModelId: aws.String(modelID), ContentType: aws.String("application/json"), Accept: aws.String("application/json"), Body: payload})
		if err != nil {
			return nil, newAwsInvokeError(parent, err, "InvokeModel")
		}
		response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(output.Body))}
		response.Header.Set("Content-Type", "application/json")
		return response, nil
	}
	output, err := client.InvokeModelWithResponseStream(ctx, &bedrockruntime.InvokeModelWithResponseStreamInput{ModelId: aws.String(modelID), ContentType: aws.String("application/json"), Accept: aws.String("application/json"), Body: payload})
	if err != nil {
		cancel()
		return nil, newAwsInvokeError(parent, err, "InvokeModelWithResponseStream")
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
