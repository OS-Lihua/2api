package xunfei

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

func (p *Provider) ComposeProtocol(kind protocol.Type, generic channel.ProtocolHandler) (channel.ProtocolHandler, error) {
	if kind != protocol.OpenAIChat {
		return nil, fmt.Errorf("Xunfei does not support protocol %q", kind)
	}
	return &sparkProtocol{public: generic, wire: &Protocol{}}, nil
}

// sparkProtocol converts the Spark envelope and feeds the shared Chat parser.
// WebSocket execution and account fields belong to Provider.ExecuteRequest.
type sparkProtocol struct {
	public channel.ProtocolHandler
	wire   *Protocol
}

func (p *sparkProtocol) ConvertRequest(c *gin.Context, info *relaycommon.RelayInfo, request any) (any, error) {
	req, err := p.public.ConvertRequest(c, info, request)
	if err != nil {
		return nil, err
	}
	return p.wire.ConvertRequest(c, info, req)
}
func (p *sparkProtocol) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewError(fmt.Errorf("missing Spark response"), types.ErrorCodeBadResponseBody)
	}
	reportedUsage := make(map[string]int)
	if !info.IsStream {
		defer resp.Body.Close()
		var content strings.Builder
		var usage dto.Usage
		var last XunfeiChatResponse
		var responseErr *types.NewAPIError
		scanner := helper.NewStreamScanner(resp.Body)
		for scanner.Scan() {
			if err := common.Unmarshal(scanner.Bytes(), &last); err != nil {
				responseErr = types.NewError(err, types.ErrorCodeBadResponseBody)
				break
			}
			if info.ResponseFacts != nil {
				recordSparkUsage(scanner.Bytes(), reportedUsage, last.Payload.Usage.Text)
			}
			if last.Header.Code != 0 {
				responseErr = types.NewError(fmt.Errorf("Xunfei response error %d", last.Header.Code), types.ErrorCodeBadResponseBody)
				break
			}
			if len(last.Payload.Choices.Text) > 0 {
				content.WriteString(last.Payload.Choices.Text[0].Content)
			}
			usage.PromptTokens += last.Payload.Usage.Text.PromptTokens
			usage.CompletionTokens += last.Payload.Usage.Text.CompletionTokens
			usage.TotalTokens += last.Payload.Usage.Text.TotalTokens
		}
		if err := scanner.Err(); err != nil {
			responseErr = types.NewError(err, types.ErrorCodeReadResponseBodyFailed)
		}
		if responseErr != nil {
			if info.ResponseFacts == nil {
				return nil, responseErr
			}
			promptTokens, prompt := reportedUsage["prompt_tokens"]
			completionTokens, completion := reportedUsage["completion_tokens"]
			reported := dto.Usage{PromptTokens: promptTokens, CompletionTokens: completionTokens}
			usage = dto.Usage{}
			info.ResponseFacts.RecordUsage(&usage, &reported, prompt, completion)
			return &usage, responseErr
		}
		response := responseXunfei2OpenAI(&last)
		response.Choices[0].Message.Content = content.String()
		response.Usage = usage
		response.Model = info.UpstreamModelName
		wire := struct {
			*dto.OpenAITextResponse
			Usage any `json:"usage,omitempty"`
		}{OpenAITextResponse: response, Usage: &usage}
		if info.ResponseFacts != nil {
			wire.Usage = nil
			if len(reportedUsage) > 0 {
				wire.Usage = reportedUsage
			}
		}
		encoded, err := common.Marshal(wire)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		resp.Body = io.NopCloser(bytes.NewReader(encoded))
		return p.public.DoResponse(c, resp, info)
	}
	original := resp.Body
	reader, writer := io.Pipe()
	go func() {
		defer original.Close()
		defer writer.Close()
		scanner := helper.NewStreamScanner(original)
		var usage dto.Usage
		for scanner.Scan() {
			var frame XunfeiChatResponse
			if err := common.Unmarshal(scanner.Bytes(), &frame); err != nil {
				writer.CloseWithError(err)
				return
			}
			if frame.Header.Code != 0 && info.ResponseFacts == nil {
				writer.CloseWithError(fmt.Errorf("Xunfei response error %d", frame.Header.Code))
				return
			}
			usage.PromptTokens += frame.Payload.Usage.Text.PromptTokens
			usage.CompletionTokens += frame.Payload.Usage.Text.CompletionTokens
			usage.TotalTokens += frame.Payload.Usage.Text.TotalTokens
			chunk := streamResponseXunfei2OpenAI(&frame)
			chunk.Model = info.UpstreamModelName
			if frame.Header.Code != 0 {
				chunk.Choices = nil
			}
			if frame.Payload.Choices.Status == 2 {
				chunk.Usage = &usage
			}
			wire := struct {
				*dto.ChatCompletionsStreamResponse
				Usage any `json:"usage,omitempty"`
			}{ChatCompletionsStreamResponse: chunk, Usage: chunk.Usage}
			if info.ResponseFacts != nil {
				recordSparkUsage(scanner.Bytes(), reportedUsage, frame.Payload.Usage.Text)
				wire.Usage = nil
				if len(reportedUsage) > 0 {
					wire.Usage = reportedUsage
				}
			}
			encoded, err := common.Marshal(wire)
			if err != nil {
				writer.CloseWithError(err)
				return
			}
			if _, err := fmt.Fprintf(writer, "data: %s\n\n", encoded); err != nil {
				return
			}
			if frame.Header.Code != 0 {
				writer.CloseWithError(fmt.Errorf("Xunfei response error %d", frame.Header.Code))
				return
			}
		}
		if err := scanner.Err(); err != nil {
			writer.CloseWithError(err)
			return
		}
		io.WriteString(writer, "data: [DONE]\n\n")
	}()
	resp.Body = &sparkTranslatedBody{PipeReader: reader, source: original}
	resp.Header.Set("Content-Type", "text/event-stream")
	return p.public.DoResponse(c, resp, info)
}

func recordSparkUsage(body []byte, reported map[string]int, usage dto.Usage) {
	prompt, completion := relaycommon.NativeUsagePresence(body, "payload.usage.text.prompt_tokens", "payload.usage.text.completion_tokens", "payload.usage.text.total_tokens")
	if prompt {
		reported["prompt_tokens"] = usage.PromptTokens
	}
	if completion {
		reported["completion_tokens"] = usage.CompletionTokens
	}
}

type sparkTranslatedBody struct {
	*io.PipeReader
	source io.ReadCloser
}

func (b *sparkTranslatedBody) Close() error { b.source.Close(); return b.PipeReader.Close() }
