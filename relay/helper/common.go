package helper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// streamKeepAliveWriter 在业务写入前保活，写入开始时等待心跳退出。
type streamKeepAliveWriter struct {
	gin.ResponseWriter
	mutex       sync.Mutex
	cancel      context.CancelFunc
	done        chan struct{}
	pingContext *gin.Context
}

func (writer *streamKeepAliveWriter) stop() {
	writer.cancel()
	<-writer.done
}

func (writer *streamKeepAliveWriter) Unwrap() http.ResponseWriter {
	return writer.ResponseWriter
}

func (writer *streamKeepAliveWriter) Write(data []byte) (int, error) {
	writer.stop()
	return writer.ResponseWriter.Write(data)
}

func (writer *streamKeepAliveWriter) WriteString(data string) (int, error) {
	writer.stop()
	return writer.ResponseWriter.WriteString(data)
}

func (writer *streamKeepAliveWriter) WriteHeader(status int) {
	writer.stop()
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *streamKeepAliveWriter) WriteHeaderNow() {
	writer.stop()
	writer.ResponseWriter.WriteHeaderNow()
}

func (writer *streamKeepAliveWriter) Flush() {
	writer.stop()
	writer.ResponseWriter.Flush()
}

func (writer *streamKeepAliveWriter) Status() int {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return writer.ResponseWriter.Status()
}

func (writer *streamKeepAliveWriter) Size() int {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return writer.ResponseWriter.Size()
}

func (writer *streamKeepAliveWriter) Written() bool {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return writer.ResponseWriter.Written()
}

func (writer *streamKeepAliveWriter) ping() error {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return PingData(writer.pingContext)
}

// StartStreamKeepAlive 按 interval 发送心跳，覆盖请求准备和重试；业务写入开始后交给流处理器。
// 内层请求复用外层的保活，内层清理不会提前结束它。
func StartStreamKeepAlive(c *gin.Context, interval time.Duration) func() {
	if previous, ok := c.Writer.(*streamKeepAliveWriter); ok {
		select {
		case <-previous.done:
			c.Writer = previous.ResponseWriter
		default:
			return func() {}
		}
	}
	if requestContextDone(c) {
		return func() {}
	}
	if interval <= 0 {
		interval = DefaultPingInterval
	}
	SetEventStreamHeaders(c)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 120*time.Minute)
	pingContext := c.Copy()
	// 心跳使用原 writer，业务写入才会触发交接。
	pingContext.Writer = c.Writer
	writer := &streamKeepAliveWriter{ResponseWriter: c.Writer, cancel: cancel, done: make(chan struct{}), pingContext: pingContext}
	c.Writer = writer
	go writer.keepAlive(ctx, interval)
	return writer.stop
}

func (writer *streamKeepAliveWriter) keepAlive(ctx context.Context, interval time.Duration) {
	defer close(writer.done)
	defer writer.cancel()
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.LogDebug(writer.pingContext, "SSE ping goroutine panic recovered: %v", recovered)
		}
	}()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ctx.Err() != nil || writer.ping() != nil {
				return
			}
		}
	}
}

func FlushWriter(c *gin.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("flush panic recovered: %v", r)
		}
	}()

	if c == nil || c.Writer == nil {
		return nil
	}

	if requestContextDone(c) {
		return fmt.Errorf("request context done: %w", c.Request.Context().Err())
	}

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return errors.New("streaming error: flusher not found")
	}

	flusher.Flush()
	// Gin's http.Flusher wrapper discards the underlying FlushError result.
	// Flush wrappers first so their buffers are preserved, then surface any
	// stored write error from an error-aware writer underneath them.
	var writer http.ResponseWriter = c.Writer
	for {
		if flusher, ok := writer.(interface{ FlushError() error }); ok {
			return flusher.FlushError()
		}
		unwrapper, ok := writer.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		writer = unwrapper.Unwrap()
	}
	return nil
}

func requestContextDone(c *gin.Context) bool {
	return c != nil && c.Request != nil && c.Request.Context().Err() != nil
}

func SetEventStreamHeaders(c *gin.Context) {
	// 检查是否已经设置过头部
	if _, exists := c.Get("event_stream_headers_set"); exists {
		return
	}

	// 设置标志，表示头部已经设置过
	c.Set("event_stream_headers_set", true)

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("Transfer-Encoding", "chunked")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer = &streamResponseWriter{ResponseWriter: c.Writer}
}

func ClaudeData(c *gin.Context, resp dto.ClaudeResponse) error {
	if requestContextDone(c) {
		return nil
	}

	jsonData, err := common.Marshal(resp)
	if err != nil {
		common.SysError(common.LogText("error marshalling stream response: %s", err.Error()))
	} else {
		c.Render(-1, common.CustomEvent{Data: fmt.Sprintf("event: %s\n", resp.Type)})
		c.Render(-1, common.CustomEvent{Data: "data: " + string(jsonData)})
	}
	_ = FlushWriter(c)
	return nil
}

func ClaudeChunkData(c *gin.Context, resp dto.ClaudeResponse, data string) {
	if requestContextDone(c) {
		return
	}

	c.Render(-1, common.CustomEvent{Data: fmt.Sprintf("event: %s\n", resp.Type)})
	c.Render(-1, common.CustomEvent{Data: fmt.Sprintf("data: %s\n", data)})
	_ = FlushWriter(c)
}

func ResponseChunkData(c *gin.Context, resp dto.ResponsesStreamResponse, data string) error {
	if requestContextDone(c) {
		return fmt.Errorf("request context done: %w", c.Request.Context().Err())
	}

	event := common.CustomEvent{Data: fmt.Sprintf("event: %s\n", resp.Type)}
	if err := event.Render(c.Writer); err != nil {
		return err
	}
	event.Data = fmt.Sprintf("data: %s", data)
	if err := event.Render(c.Writer); err != nil {
		return err
	}
	return FlushWriter(c)
}

// StreamFlushError means a complete frame was accepted before a write or flush error.
// Callers can distinguish accepted business content from a partial frame write.
type StreamFlushError struct {
	Err error
}

func (e *StreamFlushError) Error() string { return e.Err.Error() }
func (e *StreamFlushError) Unwrap() error { return e.Err }

// ErrGatewayStreamPreludeLimit is an upstream framing failure before any
// downstream write. It must remain eligible for pre-business failover.
var ErrGatewayStreamPreludeLimit = errors.New("upstream stream prelude exceeds limit")

// WriteGatewayStreamFrame holds an Attempt's prelude until its first business
// frame. A failed candidate cannot commit stale protocol IDs or loss headers.
func WriteGatewayStreamFrame(c *gin.Context, facts *relaycommon.ResponseFacts, frame string, business bool) error {
	if err := c.Request.Context().Err(); err != nil {
		return err
	}
	if facts != nil && facts.OnBusinessOutput != nil && !facts.BusinessOutput {
		if !business {
			if len(facts.StreamPrelude)+len(frame) > 1<<20 {
				return ErrGatewayStreamPreludeLimit
			}
			facts.StreamPrelude = append(facts.StreamPrelude, frame...)
			return nil
		}
		maps.Copy(c.Writer.Header(), facts.StreamHeaders)
		facts.StreamHeaders = nil
		frame = string(facts.StreamPrelude) + frame
		facts.StreamPrelude = nil
	}
	n, err := c.Writer.Write([]byte(frame))
	if n != len(frame) {
		if err == nil {
			err = io.ErrShortWrite
		}
		return err
	}
	if facts != nil && business {
		facts.MarkBusinessOutput()
	}
	if err != nil {
		return &StreamFlushError{Err: err}
	}
	if err := FlushWriter(c); err != nil {
		return &StreamFlushError{Err: err}
	}
	return nil
}

// WriteGatewayJSONResponse records complete business writes and checks the
// error-aware flush boundary for both generic and native protocol writers.
func WriteGatewayJSONResponse(c *gin.Context, resp *http.Response, facts *relaycommon.ResponseFacts, body []byte, business bool) error {
	if err := c.Request.Context().Err(); err != nil {
		return err
	}
	if resp != nil {
		for name, values := range resp.Header {
			if len(values) > 0 && service.ShouldCopyUpstreamHeader(c, name, values) {
				c.Writer.Header().Set(name, values[0])
			}
		}
	}
	c.Header("Content-Type", "application/json")
	c.Header("Content-Length", fmt.Sprint(len(body)))
	status := http.StatusOK
	if resp != nil {
		status = resp.StatusCode
	}
	c.Writer.WriteHeader(status)
	written, err := c.Writer.Write(body)
	if err == nil && written != len(body) {
		err = io.ErrShortWrite
	}
	if business && written == len(body) {
		facts.MarkBusinessOutput()
	}
	if err != nil {
		facts.DownstreamWriteFailed = true
		return err
	}
	if err := FlushWriter(c); err != nil {
		facts.DownstreamWriteFailed = true
		return err
	}
	return nil
}

// RecordNativeStreamSource keeps a received generation chunk as procurement
// evidence when unrelated native fields prevent canonical delivery.
func RecordNativeStreamSource(c *gin.Context, info *relaycommon.RelayInfo, source *dto.OpenAITextResponse) {
	if info.ResponseFacts == nil || source == nil || len(source.Choices) == 0 {
		return
	}
	type sourceChoice struct {
		Index int         `json:"index"`
		Delta dto.Message `json:"delta"`
	}
	frame := struct {
		Choices []sourceChoice `json:"choices"`
	}{}
	for _, choice := range source.Choices {
		frame.Choices = append(frame.Choices, sourceChoice{Index: choice.Index, Delta: choice.Message})
	}
	body, err := common.Marshal(frame)
	if err != nil {
		return
	}
	text := relaycommon.ChatStreamBusinessText(body)
	info.ResponseFacts.SourceGeneratedText += text
	info.ResponseFacts.SourceOutputObserved = info.ResponseFacts.SourceOutputObserved || text != ""
	recordNativeCaptureSource(c, body)
}

func recordNativeCaptureSource(c *gin.Context, body []byte) {
	for writer := http.ResponseWriter(c.Writer); writer != nil; {
		if recorder, ok := writer.(interface{ RecordChatStreamSource([]byte) }); ok {
			recorder.RecordChatStreamSource(body)
			break
		}
		wrapper, ok := writer.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		writer = wrapper.Unwrap()
	}
}

func StringData(c *gin.Context, str string) error {
	if c == nil || c.Writer == nil {
		return errors.New("context or writer is nil")
	}

	if facts, ok := common.GetContextKeyType[*relaycommon.ResponseFacts](c, constant.ContextKeyResponseFacts); ok && facts != nil && facts.OnBusinessOutput != nil {
		if str == "[DONE]" {
			err := WriteGatewayStreamFrame(c, nil, "data: [DONE]\n\n", false)
			if err != nil {
				facts.DownstreamWriteFailed = true
			}
			return err
		}
		var chunk dto.ChatCompletionsStreamResponse
		if common.UnmarshalJsonStr(str, &chunk) == nil && (chunk.Choices != nil || chunk.Usage != nil) {
			business := false
			for _, choice := range chunk.Choices {
				facts.OutputLimitObserved = facts.OutputLimitObserved || choice.FinishReason != nil && *choice.FinishReason == "length"
				business = business || choice.Delta.GetContentString() != "" || choice.Delta.GetReasoningContent() != "" || len(choice.Delta.ToolCalls) > 0
			}
			text := relaycommon.ChatStreamBusinessText([]byte(str))
			facts.SourceGeneratedText += text
			facts.SourceOutputObserved = facts.SourceOutputObserved || text != "" || business
			recordNativeCaptureSource(c, []byte(str))
			return WriteGatewayStreamFrame(c, facts, "data: "+str+"\n\n", business)
		}
	}
	if requestContextDone(c) {
		return fmt.Errorf("request context done: %w", c.Request.Context().Err())
	}
	if facts, _ := common.GetContextKey(c, constant.ContextKeyResponseFacts); facts == nil {
		c.Render(-1, common.CustomEvent{Data: "data: " + str})
		return FlushWriter(c)
	}
	event := common.CustomEvent{Data: "data: " + str}
	if err := event.Render(c.Writer); err != nil {
		return err
	}
	if err := FlushWriter(c); err != nil {
		return &StreamFlushError{Err: err}
	}
	return nil
}

func PingData(c *gin.Context) error {
	if c == nil || c.Writer == nil {
		return errors.New("context or writer is nil")
	}

	if requestContextDone(c) {
		return fmt.Errorf("request context done: %w", c.Request.Context().Err())
	}

	if facts, ok := common.GetContextKeyType[*relaycommon.ResponseFacts](c, constant.ContextKeyResponseFacts); ok && facts != nil && facts.OnBusinessOutput != nil && !facts.BusinessOutput {
		return nil
	}
	if _, err := c.Writer.Write([]byte(": PING\n\n")); err != nil {
		return fmt.Errorf("write ping data failed: %w", err)
	}
	return FlushWriter(c)
}

func ObjectData(c *gin.Context, object any) error {
	if object == nil {
		return errors.New("object is nil")
	}
	jsonData, err := common.Marshal(object)
	if err != nil {
		return fmt.Errorf("error marshalling object: %w", err)
	}
	return StringData(c, string(jsonData))
}

// SendChatChunk records only complete business frames accepted downstream.
// A flush failure after the frame is distinct from a partial frame write.
func SendChatChunk(c *gin.Context, info *relaycommon.RelayInfo, response *dto.ChatCompletionsStreamResponse) error {
	facts := info.ResponseFacts
	if facts != nil && facts.OnBusinessOutput != nil {
		for _, choice := range response.Choices {
			if choice.FinishReason != nil && *choice.FinishReason == constant.FinishReasonContentFilter {
				info.PerformanceBusinessRejection = true
				info.StreamStatus.MarkFailed("content_filter", "", 0)
			}
		}
	}
	err := ObjectData(c, response)
	if facts == nil {
		return err
	}
	if err != nil {
		if !errors.Is(err, ErrGatewayStreamPreludeLimit) {
			facts.DownstreamWriteFailed = true
		}
		var flushErr *StreamFlushError
		if !errors.As(err, &flushErr) {
			return err
		}
	}
	for _, choice := range response.Choices {
		text := choice.Delta.GetContentString() + choice.Delta.GetReasoningContent()
		for _, call := range choice.Delta.ToolCalls {
			text += call.Function.Name + call.Function.Arguments
		}
		if text != "" || len(choice.Delta.ToolCalls) > 0 {
			facts.GeneratedText += text
			facts.MarkBusinessOutput()
		}
	}
	if err == nil && facts.OnBusinessOutput != nil && info.PerformanceBusinessRejection {
		return errors.New("upstream content safety rejection")
	}
	return err
}

// EstimateMissingChatUsage fills only counters absent from the provider facts.
func EstimateMissingChatUsage(c *gin.Context, info *relaycommon.RelayInfo, usage *dto.Usage) {
	facts := info.ResponseFacts
	if facts == nil || facts.UsageReported {
		return
	}
	common.SetContextKey(c, constant.ContextKeyLocalCountTokens, true)
	if !facts.PromptTokensReported {
		usage.PromptTokens = info.GetEstimatePromptTokens()
	}
	if !facts.CompletionTokensReported {
		switch {
		case facts.SourceOutputTokens != nil:
			usage.CompletionTokens = *facts.SourceOutputTokens
		case facts.GeneratedOutputTokens != nil:
			usage.CompletionTokens = *facts.GeneratedOutputTokens
		default:
			usage.CompletionTokens = service.ResponseText2Usage(c, facts.GeneratedText, info.TokenizerModelName(), info.GetEstimatePromptTokens()).CompletionTokens
		}
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
}

// WriteChatResponse records delivery of an already converted native response.
// Provider parsers must record counter presence before calling this function.
func WriteChatResponse(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response, response *dto.OpenAITextResponse) (*dto.Usage, *types.NewAPIError) {
	facts := info.ResponseFacts
	if resp.StatusCode != http.StatusOK {
		return &response.Usage, types.NewErrorWithStatusCode(errors.New("upstream request failed"), types.ErrorCodeBadResponse, resp.StatusCode)
	}
	facts.RecordChatSource(response, info.TokenizerModelName())
	business := false
	for _, choice := range response.Choices {
		facts.OutputLimitObserved = facts.OutputLimitObserved || choice.FinishReason == "length"
		if facts.OnBusinessOutput != nil && choice.FinishReason == constant.FinishReasonContentFilter {
			info.PerformanceBusinessRejection = true
		}
		business = business || len(choice.Message.ParseToolCalls()) > 0
	}
	facts.GeneratedText = facts.SourceGeneratedText
	EstimateMissingChatUsage(c, info, &response.Usage)
	encoded, err := common.Marshal(response)
	if err != nil {
		return &response.Usage, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	if err := c.Request.Context().Err(); err != nil {
		return &response.Usage, types.NewError(err, types.ErrorCodeBadResponse)
	}
	if err := WriteGatewayJSONResponse(c, resp, facts, encoded, business || facts.GeneratedText != ""); err != nil {
		return &response.Usage, types.NewError(err, types.ErrorCodeBadResponse)
	}
	facts.GeneratedOutputTokens = facts.SourceOutputTokens
	return &response.Usage, nil
}

func Done(c *gin.Context) error {
	if value, ok := common.GetContextKey(c, constant.ContextKeyResponseFacts); ok {
		if facts, _ := value.(*relaycommon.ResponseFacts); facts != nil && facts.OnBusinessOutput != nil {
			if !facts.BusinessOutput || facts.CanFinishStream != nil && !facts.CanFinishStream() {
				return nil
			}
		}
	}
	return StringData(c, "[DONE]")
}

func WssString(c *gin.Context, ws *websocket.Conn, str string) error {
	if ws == nil {
		logger.LogError(c, common.LogText("websocket connection is nil"))
		return errors.New("websocket connection is nil")
	}
	//common.LogInfo(c, fmt.Sprintf("sending message: %s", str))
	return ws.WriteMessage(1, []byte(str))
}

func WssObject(c *gin.Context, ws *websocket.Conn, object any) error {
	jsonData, err := common.Marshal(object)
	if err != nil {
		return fmt.Errorf("error marshalling object: %w", err)
	}
	if ws == nil {
		logger.LogError(c, common.LogText("websocket connection is nil"))
		return errors.New("websocket connection is nil")
	}
	//common.LogInfo(c, fmt.Sprintf("sending message: %s", jsonData))
	return ws.WriteMessage(1, jsonData)
}

func WssError(c *gin.Context, ws *websocket.Conn, apiErr *types.NewAPIError, upstreamURLs ...string) {
	if ws == nil {
		return
	}
	openaiError := i18n.LocalizeRelayError(i18n.StatedLang(c), apiErr, upstreamURLs...)
	errorObj := &dto.RealtimeEvent{
		Type:    "error",
		EventId: GetLocalRealtimeID(c),
		Error:   &openaiError,
	}
	_ = WssObject(c, ws, errorObj)
}

func GetResponseID(c *gin.Context) string {
	logID := c.GetString(common.RequestIdKey)
	return fmt.Sprintf("chatcmpl-%s", logID)
}

func GetLocalRealtimeID(c *gin.Context) string {
	logID := c.GetString(common.RequestIdKey)
	return fmt.Sprintf("evt_%s", logID)
}

func GenerateStartEmptyResponse(id string, createAt int64, model string, systemFingerprint *string) *dto.ChatCompletionsStreamResponse {
	return &dto.ChatCompletionsStreamResponse{
		Id:                id,
		Object:            "chat.completion.chunk",
		Created:           createAt,
		Model:             model,
		SystemFingerprint: systemFingerprint,
		Choices: []dto.ChatCompletionsStreamResponseChoice{
			{
				Delta: dto.ChatCompletionsStreamResponseChoiceDelta{
					Role:    "assistant",
					Content: common.GetPointer(""),
				},
			},
		},
	}
}

func GenerateStopResponse(id string, createAt int64, model string, finishReason string) *dto.ChatCompletionsStreamResponse {
	return &dto.ChatCompletionsStreamResponse{
		Id:                id,
		Object:            "chat.completion.chunk",
		Created:           createAt,
		Model:             model,
		SystemFingerprint: nil,
		Choices: []dto.ChatCompletionsStreamResponseChoice{
			{
				FinishReason: &finishReason,
			},
		},
	}
}

func GenerateFinalUsageResponse(id string, createAt int64, model string, usage dto.Usage) *dto.ChatCompletionsStreamResponse {
	return &dto.ChatCompletionsStreamResponse{
		Id:                id,
		Object:            "chat.completion.chunk",
		Created:           createAt,
		Model:             model,
		SystemFingerprint: nil,
		Choices:           make([]dto.ChatCompletionsStreamResponseChoice, 0),
		Usage:             &usage,
	}
}
