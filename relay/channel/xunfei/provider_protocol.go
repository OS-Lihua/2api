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
	"github.com/tidwall/gjson"
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
	wire, err := p.wire.ConvertRequest(c, info, req)
	if err != nil {
		return nil, err
	}
	if info.ResponseFacts != nil && info.ResponseFacts.OnBusinessOutput != nil {
		if chat, ok := req.(*dto.GeneralOpenAIRequest); ok {
			contract, ok := wire.(channel.NativeChatRequestContract)
			if !ok {
				return nil, fmt.Errorf("native request does not declare retained parameters")
			}
			projector, ok := wire.(channel.ChatRequestProjector)
			if !ok {
				return nil, fmt.Errorf("native request does not expose its effective wire projection")
			}
			encoded, err := common.Marshal(wire)
			if err != nil {
				return nil, err
			}
			actual, err := projector.ProjectChatRequest(encoded, *chat)
			if err != nil {
				return nil, err
			}
			if err := channel.DiagnoseNativeChatRequest(c, info, chat, contract.ChatRequestCapabilities(), actual); err != nil {
				return nil, err
			}
		}
	}
	return wire, nil
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
			raw := scanner.Bytes()
			if info.ResponseFacts != nil {
				recordSparkUsage(raw, reportedUsage)
				for _, choice := range relaycommon.NativeTextSource(raw, "payload.choices.text", "content").Choices {
					content.WriteString(choice.Message.StringContent())
				}
				raw = relaycommon.SanitizeNativeUsage(raw, "payload.usage.text")
			}
			if err := common.Unmarshal(raw, &last); err != nil {
				responseErr = types.NewError(err, types.ErrorCodeBadResponseBody)
				break
			}
			if last.Header.Code != 0 {
				responseErr = types.NewError(fmt.Errorf("Xunfei response error %d", last.Header.Code), types.ErrorCodeBadResponseBody)
				break
			}
			if info.ResponseFacts == nil && len(last.Payload.Choices.Text) > 0 {
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
			info.ResponseFacts.SourceGeneratedText = content.String()
			info.ResponseFacts.SourceOutputObserved = content.Len() > 0
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
	var sourceText strings.Builder
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer original.Close()
		defer writer.Close()
		scanner := helper.NewStreamScanner(original)
		var usage dto.Usage
		for scanner.Scan() {
			raw := scanner.Bytes()
			if info.ResponseFacts != nil {
				recordSparkUsage(raw, reportedUsage)
				for _, choice := range relaycommon.NativeTextSource(raw, "payload.choices.text", "content").Choices {
					sourceText.WriteString(choice.Message.StringContent())
				}
				raw = relaycommon.SanitizeNativeUsage(raw, "payload.usage.text")
			}
			if err := c.Request.Context().Err(); err != nil {
				_ = writer.CloseWithError(err)
				return
			}
			var frame XunfeiChatResponse
			if err := common.Unmarshal(raw, &frame); err != nil {
				if info.ResponseFacts != nil && len(reportedUsage) > 0 {
					metadata, marshalErr := common.Marshal(struct {
						Choices []any          `json:"choices"`
						Usage   map[string]int `json:"usage"`
					}{Choices: []any{}, Usage: reportedUsage})
					if marshalErr == nil {
						if _, writeErr := fmt.Fprintf(writer, "data: %s\n\n", metadata); writeErr != nil {
							return
						}
					}
				}
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
	value, failure := p.public.DoResponse(c, resp, info)
	_ = resp.Body.Close()
	<-finished
	if info.ResponseFacts != nil {
		usage, _ := value.(*dto.Usage)
		if usage == nil {
			usage = &dto.Usage{}
		}
		promptTokens, prompt := reportedUsage["prompt_tokens"]
		completionTokens, completion := reportedUsage["completion_tokens"]
		estimatedPrompt, estimatedCompletion := usage.PromptTokens, usage.CompletionTokens
		info.ResponseFacts.RecordUsage(usage, &dto.Usage{PromptTokens: promptTokens, CompletionTokens: completionTokens}, prompt, completion)
		if !info.ResponseFacts.PromptTokensReported {
			usage.PromptTokens = estimatedPrompt
		}
		if !info.ResponseFacts.CompletionTokensReported {
			usage.CompletionTokens = estimatedCompletion
		}
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		if sourceText.Len() > 0 {
			info.ResponseFacts.RecordChatSource(&dto.OpenAITextResponse{Choices: []dto.OpenAITextResponseChoice{{Message: dto.Message{Role: "assistant", Content: sourceText.String()}}}}, info.TokenizerModelName())
		}
		return usage, failure
	}
	return value, failure
}

func recordSparkUsage(body []byte, reported map[string]int) {
	prompt, completion := relaycommon.NativeUsagePresence(body, "payload.usage.text.prompt_tokens", "payload.usage.text.completion_tokens", "payload.usage.text.total_tokens")
	if prompt {
		reported["prompt_tokens"] = int(gjson.GetBytes(body, "payload.usage.text.prompt_tokens").Int())
	}
	if completion {
		reported["completion_tokens"] = int(gjson.GetBytes(body, "payload.usage.text.completion_tokens").Int())
	}
}

type sparkTranslatedBody struct {
	*io.PipeReader
	source io.ReadCloser
}

func (b *sparkTranslatedBody) Close() error { b.source.Close(); return b.PipeReader.Close() }
