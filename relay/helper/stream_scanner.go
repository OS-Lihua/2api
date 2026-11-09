package helper

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/bytedance/gopkg/util/gopool"

	"github.com/gin-gonic/gin"
)

const (
	InitialScannerBufferSize    = 64 << 10  // 64KB (64*1024)
	DefaultMaxScannerBufferSize = 128 << 20 // 64MB (64*1024*1024) default SSE buffer size
	DefaultPingInterval         = 10 * time.Second
	// streamWriteTimeout bounds a write blocked on a client that stopped
	// reading so the unconditional wg.Wait() in cleanup can always finish.
	// Without it, a connected client with a full TCP buffer (no server
	// WriteTimeout) could hang the handler forever. StreamWriteBudget adds the
	// time a client reading at streamMinClientRate needs for the data.
	streamWriteTimeout  = 30 * time.Second
	streamMinClientRate = 8 << 10 // bytes per second
)

func getScannerBufferSize() int {
	if constant.StreamScannerMaxBufferMB > 0 {
		return constant.StreamScannerMaxBufferMB << 20
	}
	return DefaultMaxScannerBufferSize
}

// NewStreamScanner shares relay scanner configuration. Callers buffering bounded
// task state may additionally cap a line without increasing the configured limit.
func NewStreamScanner(reader io.Reader, maxBytes ...int) *bufio.Scanner {
	limit := getScannerBufferSize()
	if len(maxBytes) > 0 && maxBytes[0] > 0 {
		limit = min(limit, maxBytes[0])
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, min(InitialScannerBufferSize, limit)), limit)
	return scanner
}

// NewSSEEventScanner shares event framing and retains incomplete source data
// for procurement observers without presenting it as a delivered event.
func NewSSEEventScanner(reader io.Reader, facts *relaycommon.ResponseFacts) *bufio.Scanner {
	scanner := NewStreamScanner(reader)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		advance, token, err := splitSSEEvents(data, atEOF)
		if err != nil && facts != nil {
			event := ParseSSEEvent(string(data))
			facts.UnterminatedStreamData = []byte(event.Data)
			facts.UnterminatedStreamEvent = event.Event
		}
		return advance, token, err
	})
	return scanner
}

type SSEEvent struct {
	Event   string
	Data    string
	HasData bool
}

// splitSSEEvents confirms the blank line that terminates an SSE event. EOF
// cannot complete a pending data event; comments without data may be discarded.
func splitSSEEvents(data []byte, atEOF bool) (int, []byte, error) {
	lineStart := 0
	for index := 0; index < len(data); {
		if data[index] != '\r' && data[index] != '\n' {
			index++
			continue
		}
		next := index + 1
		if data[index] == '\r' {
			if next == len(data) && !atEOF && index != lineStart {
				return 0, nil, nil
			}
			if next < len(data) && data[next] == '\n' {
				next++
			}
		}
		if index == lineStart {
			return next, data[:index], nil
		}
		lineStart, index = next, next
	}
	if atEOF {
		if ParseSSEEvent(string(data)).HasData {
			return 0, nil, io.ErrUnexpectedEOF
		}
		return len(data), nil, nil
	}
	return 0, nil, nil
}

// ParseSSEEvent joins data fields and retains the event name of one record.
func ParseSSEEvent(event string) SSEEvent {
	event = strings.TrimPrefix(event, "\xef\xbb\xbf")
	event = strings.ReplaceAll(event, "\r\n", "\n")
	event = strings.ReplaceAll(event, "\r", "\n")
	var data strings.Builder
	result := SSEEvent{}
	for line := range strings.SplitSeq(event, "\n") {
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		if field == "event" {
			result.Event = value
			continue
		}
		if field != "data" {
			continue
		}
		if result.HasData {
			data.WriteByte('\n')
		}
		data.WriteString(value)
		result.HasData = true
	}
	result.Data = data.String()
	return result
}

func copyCodexSSEHeaders(c *gin.Context, resp *http.Response) {
	if c == nil || c.Writer == nil || resp == nil {
		return
	}
	headers := c.Writer.Header()
	if facts, ok := common.GetContextKeyType[*relaycommon.ResponseFacts](c, constant.ContextKeyResponseFacts); ok && facts != nil && facts.OnBusinessOutput != nil && !facts.BusinessOutput {
		if facts.StreamHeaders == nil {
			facts.StreamHeaders = make(map[string][]string)
		}
		headers = http.Header(facts.StreamHeaders)
	}
	for _, name := range []string{"X-Reasoning-Included", "X-Codex-Turn-State"} {
		values := resp.Header.Values(name)
		if !service.ShouldCopyUpstreamHeader(c, name, values) {
			continue
		}
		for _, value := range values {
			if value != "" {
				headers.Add(name, value)
			}
		}
	}
}

// StreamWriteBudget gives the writes of one stream a deadline of
// streamWriteTimeout past the time a client reading at streamMinClientRate
// needs for everything written so far. A fixed deadline per write cut slow
// clients off in the middle of a multi-megabyte image event, and the timeout
// looked like a client disconnect. The budget carries over between writes
// because the kernel can still hold megabytes of earlier writes and wakes a
// blocked writer only after a large part of them drains. A client that stops
// reading is still cut off once the budget runs out. Callers serialize writes.
type StreamWriteBudget struct {
	drainBy time.Time
}

// Deadline returns the write deadline for n more bytes written at now.
func (b *StreamWriteBudget) Deadline(now time.Time, n int) time.Time {
	if b.drainBy.Before(now) {
		b.drainBy = now
	}
	b.drainBy = b.drainBy.Add(time.Duration(n) * time.Second / streamMinClientRate)
	return b.drainBy.Add(streamWriteTimeout)
}

// streamResponseWriter sets the connection write deadline from its budget
// before every write. Writers that don't support deadlines (e.g. httptest
// recorders) are silently ignored.
type streamResponseWriter struct {
	gin.ResponseWriter
	budget StreamWriteBudget
}

func (w *streamResponseWriter) Write(p []byte) (int, error) {
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(w.budget.Deadline(time.Now(), len(p)))
	return w.ResponseWriter.Write(p)
}

func (w *streamResponseWriter) WriteString(s string) (int, error) {
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(w.budget.Deadline(time.Now(), len(s)))
	return w.ResponseWriter.WriteString(s)
}

// Unwrap lets http.ResponseController reach the connection.
func (w *streamResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func StreamScannerHandler(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo, dataHandler func(data string, sr *StreamResult), stoppedSource ...func(string)) {

	if resp == nil || dataHandler == nil {
		return
	}

	// Finish the request keepalive before changing headers. The scanner owns
	// subsequent ping and data writes through its shared write mutex.
	if writer, ok := c.Writer.(*streamKeepAliveWriter); ok {
		writer.stop()
	}

	managed := info.ResponseFacts != nil && info.ResponseFacts.OnBusinessOutput != nil
	if info.StreamStatus == nil || !managed {
		info.StreamStatus = relaycommon.NewStreamStatus()
	}

	ctx, cancel := context.WithCancel(context.Background())

	streamingTimeout := time.Duration(constant.StreamingTimeout) * time.Second

	var (
		stopChan    = make(chan bool, 3) // 增加缓冲区避免阻塞
		scanner     = NewStreamScanner(resp.Body)
		ticker      *time.Ticker
		timeout     <-chan time.Time
		pingTicker  *time.Ticker
		writeMutex  sync.Mutex     // Mutex to protect concurrent writes
		wg          sync.WaitGroup // 用于等待所有 goroutine 退出
		cleanupOnce sync.Once
		stopOnce    sync.Once
	)

	if !managed {
		ticker = time.NewTicker(streamingTimeout)
		timeout = ticker.C
	}

	stop := func() {
		stopOnce.Do(func() {
			close(stopChan)
		})
	}

	generalSettings := operation_setting.GetGeneralSetting()
	pingEnabled := generalSettings.PingIntervalEnabled && !info.DisablePing
	pingInterval := time.Duration(generalSettings.PingIntervalSeconds) * time.Second
	if pingInterval <= 0 {
		pingInterval = DefaultPingInterval
	}

	if pingEnabled {
		pingTicker = time.NewTicker(pingInterval)
	}

	logger.LogDebug(c, "relay timeout seconds: %d", common.RelayTimeout)
	logger.LogDebug(c, "relay max idle conns: %d", common.RelayMaxIdleConns)
	logger.LogDebug(c, "relay max idle conns per host: %d", common.RelayMaxIdleConnsPerHost)
	logger.LogDebug(c, "streaming timeout seconds: %d", int64(streamingTimeout.Seconds()))
	logger.LogDebug(c, "ping interval seconds: %d", int64(pingInterval.Seconds()))

	cleanup := func() {
		cleanupOnce.Do(func() {
			cancel()
			stop()
			if resp.Body != nil {
				_ = resp.Body.Close()
			}

			if ticker != nil {
				ticker.Stop()
			}
			if pingTicker != nil {
				pingTicker.Stop()
			}

			wg.Wait()
		})
	}
	// Ensure gin.Context is not returned to Gin's pool while any stream goroutine can still use it.
	defer cleanup()

	if managed {
		scanner = NewSSEEventScanner(resp.Body, info.ResponseFacts)
	} else {
		scanner.Split(bufio.ScanLines)
	}
	copyCodexSSEHeaders(c, resp)
	SetEventStreamHeaders(c)

	ctx = context.WithValue(ctx, "stop_chan", stopChan)

	// Handle ping data sending with improved error handling
	if pingEnabled && pingTicker != nil {
		wg.Add(1)
		gopool.Go(func() {
			defer func() {
				if r := recover(); r != nil {
					logger.LogError(c, common.LogText("ping goroutine panic: %v", r))
					info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonPanic, fmt.Errorf("ping panic: %v", r))
					stop()
				}
				logger.LogDebug(c, "ping goroutine exited")
				wg.Done()
			}()

			// 添加超时保护，防止 goroutine 无限运行
			maxPingDuration := 30 * time.Minute // 最大 ping 持续时间
			pingTimeout := time.NewTimer(maxPingDuration)
			defer pingTimeout.Stop()

			for {
				select {
				case <-pingTicker.C:
					var err error
					func() {
						writeMutex.Lock()
						defer writeMutex.Unlock()
						err = PingData(c)
						// Cancellation can stop PingData before it writes downstream.
						if err != nil && info.ResponseFacts != nil && !errors.Is(err, c.Request.Context().Err()) {
							info.ResponseFacts.DownstreamWriteFailed = true
						}
					}()
					if err != nil {
						logger.LogError(c, common.LogText("ping data error: %s", err.Error()))
						info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonPingFail, err)
						return
					}
					logger.LogDebug(c, "ping data sent")
				case <-ctx.Done():
					return
				case <-stopChan:
					return
				case <-c.Request.Context().Done():
					// 监听客户端断开连接
					return
				case <-pingTimeout.C:
					logger.LogError(c, common.LogText("ping goroutine max duration reached"))
					return
				}
			}
		})
	}

	dataChan := make(chan SSEEvent, 10)
	var pendingSource string
	sr := newStreamResult(info.StreamStatus)

	wg.Add(1)
	gopool.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				logger.LogError(c, common.LogText("data handler goroutine panic: %v", r))
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonPanic, fmt.Errorf("handler panic: %v", r))
			}
			stop()
			wg.Done()
		}()
		for event := range dataChan {
			sr.reset()
			func() {
				writeMutex.Lock()
				defer writeMutex.Unlock()
				sr.eventName = event.Event
				dataHandler(event.Data, sr)
			}()
			if sr.IsStopped() {
				return
			}
		}
	})

	// Scanner goroutine with improved error handling
	wg.Add(1)
	common.RelayCtxGo(ctx, func() {
		defer func() {
			close(dataChan)
			if r := recover(); r != nil {
				logger.LogError(c, common.LogText("scanner goroutine panic: %v", r))
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonPanic, fmt.Errorf("scanner panic: %v", r))
			}
			stop()
			logger.LogDebug(c, "scanner goroutine exited")
			wg.Done()
		}()

		for scanner.Scan() {
			if ticker != nil {
				ticker.Reset(streamingTimeout)
			}
			data := scanner.Text()
			if info.ResponseFacts != nil {
				logger.LogDebug(c, "stream scanner line: bytes=%d", len(data))
			} else {
				logger.LogDebug(c, "stream scanner data: %s", data)
			}

			event := SSEEvent{}
			if managed {
				event = ParseSSEEvent(data)
				data = ""
				if event.HasData {
					data = event.Data
				}
			} else {
				var hasDataPrefix bool
				data, hasDataPrefix = strings.CutPrefix(data, "data:")
				if !hasDataPrefix && strings.TrimSpace(data) != "[DONE]" {
					data = ""
				}
			}
			data = strings.TrimSpace(data)
			event.Data = data
			select {
			case <-stopChan:
				pendingSource = data
				return
			case <-ctx.Done():
				pendingSource = data
				return
			default:
			}
			if data == "" {
				continue
			}
			if data != "[DONE]" {
				info.SetFirstResponseTime()
				info.ReceivedResponseCount++

				select {
				case dataChan <- event:
				case <-ctx.Done():
					pendingSource = data
					return
				case <-stopChan:
					pendingSource = data
					return
				}
			} else {
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonDone, nil)
				logger.LogDebug(c, "received [DONE], stopping scanner")
				return
			}
		}

		// Closing the upstream body is part of stopping the scanner. Its EOF or
		// read error must not replace the actual stop reason or add a false error.
		if ctx.Err() != nil {
			return
		}
		if err := scanner.Err(); err != nil {
			if err != io.EOF {
				if info.ResponseFacts != nil {
					err = errors.New("upstream stream read failed")
				}
				logger.LogError(c, common.LogText("scanner error: %s", err.Error()))
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonScannerErr, err)
			}
		}
		info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonEOF, nil)
	})

	// 主循环等待完成或超时
	select {
	case <-timeout:
		info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonTimeout, nil)
	case <-stopChan:
		// EndReason already set by the goroutine that triggered stopChan
	case <-c.Request.Context().Done():
		// 客户端断开：立即 cleanup 关闭上游 resp.Body，解除 scanner 阻塞并让上游停止生成，
		// 避免为已放弃的请求继续消费上游 token。
		info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonClientGone, c.Request.Context().Err())
	}

	cleanup()
	// Source observation runs only after both stream goroutines have joined.
	// It retains received frames without another public write or terminal callback.
	if len(stoppedSource) > 0 && stoppedSource[0] != nil {
		for event := range dataChan {
			stoppedSource[0](event.Data)
		}
		if pendingSource != "" && pendingSource != "[DONE]" {
			stoppedSource[0](pendingSource)
		}
	}
	// All stream goroutines have joined. Done from a successfully delivered
	// protocol completion can now correct a cancellation that won while its
	// callback was still running. Bare EOF and failed writes never qualify.
	if sr.done && info.StreamStatus.ResponseOutcome() == string(relaycommon.ResponseOutcomeCompleted) &&
		info.StreamStatus.EndReason == relaycommon.StreamEndReasonClientGone &&
		errors.Is(info.StreamStatus.EndError, context.Canceled) && !info.StreamStatus.HasErrors() {
		info.StreamStatus.EndReason = relaycommon.StreamEndReasonDone
		info.StreamStatus.EndError = nil
	}
	if info.StreamStatus.IsNormalEnd() && !info.StreamStatus.HasErrors() {
		logger.LogInfo(c, common.LogText("stream ended: %s", info.StreamStatus.Summary()))
	} else {
		logger.LogError(c, common.LogText("stream ended: %s, received=%d", info.StreamStatus.Summary(), info.ReceivedResponseCount))
	}
}
