package textprotocol

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/tokenkit"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const nativeResponseBufferLimit = 64 << 20

// nativeResponseWriter captures the native parser's canonical Chat output.
// Native completion is withheld until that parser has validated its source.
// Only the public handler writes to the real client and marks delivery.
type nativeResponseWriter struct {
	gin.ResponseWriter
	header         http.Header
	status         int
	size           int
	stream         bool
	facts          *relaycommon.ResponseFacts
	body           bytes.Buffer
	pending        []byte
	pipe           *io.PipeWriter
	finish         string
	metadata       map[string]any
	source         gatewayGeneratedOutput
	terminalFrames [][]byte
	terminalBytes  int
	err            error
}

func (w *nativeResponseWriter) Header() http.Header    { return w.header }
func (w *nativeResponseWriter) Status() int            { return w.status }
func (w *nativeResponseWriter) Size() int              { return w.size }
func (w *nativeResponseWriter) Written() bool          { return w.size >= 0 }
func (w *nativeResponseWriter) WriteHeader(status int) { w.status = status }
func (w *nativeResponseWriter) WriteHeaderNow() {
	if w.size < 0 {
		w.size = 0
	}
}
func (w *nativeResponseWriter) Flush()                                {}
func (w *nativeResponseWriter) FlushError() error                     { return w.err }
func (w *nativeResponseWriter) WriteString(value string) (int, error) { return w.Write([]byte(value)) }

func (w *nativeResponseWriter) Write(body []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if len(body)+w.body.Len()+len(w.pending)+w.terminalBytes > nativeResponseBufferLimit {
		w.err = errors.New("native response exceeds capture limit")
		return 0, w.err
	}
	w.WriteHeaderNow()
	if !w.stream {
		n, err := w.body.Write(body)
		w.size += n
		return n, err
	}
	w.pending = append(w.pending, body...)
	for {
		end := bytes.Index(w.pending, []byte("\n\n"))
		if end < 0 {
			break
		}
		event := helper.ParseSSEEvent(string(w.pending[:end]))
		w.pending = w.pending[end+2:]
		if !event.HasData || event.Data == "[DONE]" {
			continue
		}
		data := []byte(event.Data)
		w.source.text.WriteString(gatewayStreamBusinessText(data, types.RelayFormatOpenAI))
		w.source.ObserveChatTools(data)
		w.facts.SourceOutputObserved = w.facts.SourceOutputObserved || w.source.String() != "" || len(w.source.chatTools) > 0 || gatewayMediaOutput(data, types.RelayFormatOpenAI)
		for _, name := range []string{"id", "model", "object", "system_fingerprint", "service_tier"} {
			if value := gjson.GetBytes(data, name); value.Type == gjson.String && value.String() != "" {
				w.metadata[name] = value.String()
			}
		}
		if created := gjson.GetBytes(data, "created"); created.Type == gjson.Number {
			w.metadata["created"] = created.Int()
		}
		for index, choice := range gjson.GetBytes(data, "choices").Array() {
			if reason := choice.Get("finish_reason").String(); reason != "" {
				w.finish = reason
				var err error
				data, err = sjson.SetBytes(data, "choices."+strconv.Itoa(index)+".finish_reason", nil)
				if err != nil {
					w.err = err
					return 0, err
				}
			}
		}
		data = nativeReportedUsage(data, w.facts)
		if w.finish != "" {
			w.terminalFrames = append(w.terminalFrames, data)
			w.terminalBytes += len(data) + 8
			continue
		}
		if err := w.forward(data); err != nil {
			w.err = err
			return 0, err
		}
	}
	w.size += len(body)
	return len(body), nil
}

func (w *nativeResponseWriter) forward(body []byte) error {
	frame := append(append([]byte("data: "), body...), '\n', '\n')
	_, err := w.pipe.Write(frame)
	return err
}

// finishStream releases a source terminal group only after native validation.
// Discarded content remains a procurement fact even when it was never delivered.
func (w *nativeResponseWriter) finishStream(usage *dto.Usage, complete bool) {
	if !complete || w.err != nil {
		_ = w.pipe.CloseWithError(errors.New("native response did not complete"))
		return
	}
	for _, body := range w.terminalFrames {
		if err := w.forward(body); err != nil {
			_ = w.pipe.CloseWithError(err)
			return
		}
	}
	reason := w.finish
	if w.facts.OutputLimitObserved || reason == "max_tokens" || reason == "MAX_TOKENS" {
		reason = "length"
	}
	if reason == "" {
		reason = "stop"
	}
	terminal := maps.Clone(w.metadata)
	terminal["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": reason}}
	if usage != nil {
		terminal["usage"] = usage
	}
	body, err := common.Marshal(terminal)
	if err == nil {
		err = w.forward(nativeReportedUsage(body, w.facts))
	}
	_ = w.pipe.CloseWithError(err)
}

func nativeReportedUsage(body []byte, facts *relaycommon.ResponseFacts) []byte {
	if !gjson.GetBytes(body, "usage").IsObject() {
		return body
	}
	if !facts.IndependentUsageReported {
		for name := range gjson.GetBytes(body, "usage").Map() {
			if name != "prompt_tokens" && name != "completion_tokens" && name != "total_tokens" {
				body, _ = sjson.DeleteBytes(body, "usage."+name)
			}
		}
	}
	if !facts.PromptTokensReported {
		body, _ = sjson.DeleteBytes(body, "usage.prompt_tokens")
	}
	if !facts.CompletionTokensReported {
		body, _ = sjson.DeleteBytes(body, "usage.completion_tokens")
	}
	if facts.PromptTokensReported && facts.CompletionTokensReported {
		body, _ = sjson.SetBytes(body, "usage.total_tokens", gjson.GetBytes(body, "usage.prompt_tokens").Int()+gjson.GetBytes(body, "usage.completion_tokens").Int())
	} else {
		body, _ = sjson.DeleteBytes(body, "usage.total_tokens")
	}
	return body
}

// DoNativeResponse keeps a provider's parser behind the same public delivery
// module as ordinary protocols. Its private facts describe source observations,
// while the public handler owns conversion, terminal validation and real writes.
func (p *Handler) DoNativeResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo, wire channel.ProtocolHandler) (any, *types.NewAPIError) {
	nativeContext, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	nativeInfo := *info
	nativeInfo.RelayFormat = types.RelayFormatOpenAI
	nativeInfo.RelayMode = relayconstant.RelayModeChatCompletions
	nativeInfo.StreamStatus = relaycommon.NewStreamStatus()
	nativeInfo.ResponseFacts = &relaycommon.ResponseFacts{
		ExpectedChoices:  info.ResponseFacts.ExpectedChoices,
		OnBusinessOutput: func() {},
		CanFinishStream:  func() bool { return nativeInfo.StreamStatus.CompletedNormally() },
	}
	nativeGin := c.Copy()
	nativeGin.Request = c.Request.WithContext(nativeContext)
	common.SetContextKey(nativeGin, constant.ContextKeyResponseFacts, nativeInfo.ResponseFacts)
	writer := &nativeResponseWriter{
		ResponseWriter: c.Writer, header: c.Writer.Header().Clone(), status: resp.StatusCode,
		size: -1, stream: info.IsStream, facts: nativeInfo.ResponseFacts,
		metadata: map[string]any{"id": helper.GetResponseID(c), "model": info.UpstreamModelName, "object": "chat.completion.chunk", "created": common.GetTimestamp()},
	}
	nativeGin.Writer = writer
	nativeResponse := *resp
	canonical := *resp
	canonical.Header = resp.Header.Clone()
	canonical.Header.Del("Content-Length")

	var sourceUsage *dto.Usage
	var sourceErr *types.NewAPIError
	if !info.IsStream {
		value, err := wire.DoResponse(nativeGin, &nativeResponse, &nativeInfo)
		sourceUsage, _ = value.(*dto.Usage)
		sourceUsage = normalizeNativeReportedUsage(nativeGin, sourceUsage, nativeInfo.ResponseFacts)
		sourceErr = err
		if err != nil {
			mergeNativeSourceFacts(info, &nativeInfo, writer)
			return sourceUsage, err
		}
		canonical.Header.Set("Content-Type", "application/json")
		canonical.Body = io.NopCloser(bytes.NewReader(nativeReportedUsage(writer.body.Bytes(), nativeInfo.ResponseFacts)))
		usage, failure := p.DoResponse(c, &canonical, info)
		mergeNativeSourceFacts(info, &nativeInfo, writer)
		return nativeResponseUsage(usage, sourceUsage, nativeInfo.ResponseFacts), failure
	}

	reader, output := io.Pipe()
	writer.pipe = output
	canonical.Header.Set("Content-Type", "text/event-stream")
	canonical.Body = reader
	type nativeResult struct {
		usage *dto.Usage
		err   *types.NewAPIError
	}
	finished := make(chan nativeResult, 1)
	go func() {
		var result nativeResult
		defer func() {
			if recover() != nil {
				result.err = types.NewError(errors.New("native response parser failed"), types.ErrorCodeBadResponse)
			}
			writer.finishStream(result.usage, result.err == nil && nativeInfo.StreamStatus.CompletedNormally())
			finished <- result
		}()
		value, err := wire.DoResponse(nativeGin, &nativeResponse, &nativeInfo)
		result.usage, _ = value.(*dto.Usage)
		result.usage = normalizeNativeReportedUsage(nativeGin, result.usage, nativeInfo.ResponseFacts)
		result.err = err
	}()
	usage, failure := p.DoResponse(c, &canonical, info)
	_ = reader.Close()
	cancel()
	_ = resp.Body.Close()
	result := <-finished
	sourceUsage, sourceErr = result.usage, result.err
	mergeNativeSourceFacts(info, &nativeInfo, writer)
	if failure == nil {
		failure = sourceErr
	}
	return nativeResponseUsage(usage, sourceUsage, nativeInfo.ResponseFacts), failure
}

func nativeResponseUsage(value any, source *dto.Usage, facts *relaycommon.ResponseFacts) any {
	usage, _ := value.(*dto.Usage)
	if source == nil || !facts.PromptTokensReported && !facts.CompletionTokensReported && !facts.IndependentUsageReported {
		return value
	}
	copy := *source
	if usage != nil {
		if !facts.PromptTokensReported {
			copy.PromptTokens = max(copy.PromptTokens, usage.PromptTokens)
		}
		if !facts.CompletionTokensReported {
			copy.CompletionTokens = max(copy.CompletionTokens, usage.CompletionTokens)
		}
	}
	copy.TotalTokens = copy.PromptTokens + copy.CompletionTokens
	return &copy
}

func normalizeNativeReportedUsage(c *gin.Context, usage *dto.Usage, facts *relaycommon.ResponseFacts) *dto.Usage {
	if usage == nil {
		return nil
	}
	body, err := common.Marshal(map[string]any{"usage": usage})
	if err != nil {
		return usage
	}
	meter := gatewayUsageAccumulator{format: types.RelayFormatOpenAI, context: c}
	normalized, err := meter.Observe(nativeReportedUsage(body, facts))
	if err != nil || normalized == nil {
		return usage
	}
	if !facts.PromptTokensReported {
		normalized.PromptTokens = max(normalized.PromptTokens, usage.PromptTokens)
	}
	if !facts.CompletionTokensReported {
		normalized.CompletionTokens = max(normalized.CompletionTokens, usage.CompletionTokens)
	}
	normalized.TotalTokens = normalized.PromptTokens + normalized.CompletionTokens
	return normalized
}

func mergeNativeSourceFacts(info, native *relaycommon.RelayInfo, writer *nativeResponseWriter) {
	facts, source := info.ResponseFacts, native.ResponseFacts
	facts.PromptTokensReported = facts.PromptTokensReported || source.PromptTokensReported
	facts.CompletionTokensReported = facts.CompletionTokensReported || source.CompletionTokensReported
	facts.UsageReported = facts.PromptTokensReported && facts.CompletionTokensReported
	facts.IndependentUsageReported = facts.IndependentUsageReported || source.IndependentUsageReported
	if writer.stream {
		facts.SourceGeneratedText = writer.source.String() + source.SourceGeneratedText
		count := tokenkit.Count(info.TokenizerModelName(), facts.SourceGeneratedText) + len(writer.source.chatTools)*7
		facts.SourceOutputTokens = &count
	}
	facts.SourceOutputObserved = facts.SourceOutputObserved || source.SourceOutputObserved
	info.PerformanceBusinessRejection = info.PerformanceBusinessRejection || native.PerformanceBusinessRejection
	if facts.UpstreamErrorCode == "" {
		facts.UpstreamErrorCode = source.UpstreamErrorCode
	}
	if outcome := native.StreamStatus.OutcomeSnapshot(); outcome.Response == relaycommon.ResponseOutcomeFailed {
		info.StreamStatus.MarkFailed(outcome.ErrorCode, outcome.ErrorType, outcome.ErrorStatus)
	}
}
