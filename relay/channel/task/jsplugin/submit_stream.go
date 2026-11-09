package jsplugin

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/gin-gonic/gin"
)

// submitEventStream is how one SSE submission is read: client, when set, is
// the client of a native route, which receives every upstream event as it
// arrives, and frameLimit bounds each event.
type submitEventStream struct {
	client     *gin.Context
	frameLimit int64
	money      bool
}

// readSubmitEvents owns SSE framing and bounded source state; the plugin owns
// vendor payload and terminal semantics. Money submissions parse source first
// and keep a separately bounded receipt of byte-complete public frames. Legacy
// native submissions retain their earlier forwarding and quota behavior.
func (a *TaskAdaptor) readSubmitEvents(parent context.Context, resp *http.Response, driverContext map[string]any, stream submitEventStream) (_ any, stopped bool, _ error) {
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/event-stream" {
		return nil, false, fmt.Errorf("expected a text/event-stream submit response")
	}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	stopClose := context.AfterFunc(ctx, func() { _ = resp.Body.Close() })
	defer stopClose()
	idleTimeout := time.Duration(constant.StreamingTimeout) * time.Second
	if idleTimeout <= 0 {
		idleTimeout = 30 * time.Second
	}
	timer := time.AfterFunc(idleTimeout, func() { cancel(fmt.Errorf("task submit stream idle timeout")) })
	defer timer.Stop()

	a.deliveredSubmitData = nil
	a.submitDeliveryFailed = false
	supportsBusiness := slices.Contains(a.plugin.Meta.RequiredCapabilities, pluginruntime.CapabilitySubmitSSEBusiness)
	scanner := helper.NewStreamScanner(resp.Body, int(stream.frameLimit)+1)
	hook := "parseSubmitEvent"
	var accumulated, delivered *pluginruntime.JSONState
	if pluginruntime.UsesSubmitEventDelta(a.plugin.Meta, a.plugin.Engine) {
		hook = "parseSubmitEventDelta"
		accumulated = pluginruntime.NewJSONState(maxTaskPluginPersistedJSONBytes)
		if stream.money && stream.client != nil {
			delivered = pluginruntime.NewJSONState(maxTaskPluginPersistedJSONBytes)
		}
	}
	var state any
	var data []string
	var eventName, lastID string
	var frame strings.Builder
	relayed, completed := false, false
	var readErr error
	frameBytes := 0
	firstLine := true
	for scanner.Scan() {
		if readErr = context.Cause(ctx); readErr != nil {
			break
		}
		timer.Reset(idleTimeout)
		line := scanner.Text()
		if firstLine {
			line = strings.TrimPrefix(line, "\ufeff")
			firstLine = false
		}
		frameBytes += len(line) + 1
		if int64(frameBytes) > stream.frameLimit {
			readErr = fmt.Errorf("task submit SSE event exceeds size limit")
			break
		}
		if stream.client != nil {
			frame.WriteString(line)
			frame.WriteByte('\n')
		}
		if line != "" {
			field, value, _ := strings.Cut(line, ":")
			value, _ = strings.CutPrefix(value, " ")
			switch field {
			case "data":
				data = append(data, value)
			case "event":
				eventName = value
			case "id":
				if !strings.ContainsRune(value, '\x00') {
					lastID = value
				}
			}
			continue
		}
		frameBytes = 0
		currentFrame := frame.String()
		frame.Reset()
		if stream.client != nil && !stream.money {
			_, _ = a.relaySubmitEvent(stream.client, currentFrame, false)
			relayed = true
		}
		if len(data) == 0 {
			if stream.money && stream.client != nil && !a.submitDeliveryFailed {
				_, writeErr := a.relaySubmitEvent(stream.client, currentFrame, false)
				a.submitDeliveryFailed = writeErr != nil
				relayed = stream.client.Writer.Written()
			}
			eventName = ""
			continue
		}
		if eventName == "" {
			eventName = "message"
		}
		event := map[string]any{"event": eventName, "id": lastID, "data": strings.Join(data, "\n")}
		value, err := a.plugin.Engine.Call(ctx, hook, driverContext, event, state)
		if err != nil {
			readErr = err
			break
		}
		result, ok := value.(map[string]any)
		if !ok {
			readErr = fmt.Errorf("%s must return an object", hook)
			break
		}
		nextState, hasState := result["state"]
		done, hasDone := result["done"].(bool)
		business, hasBusiness := result["business"].(bool)
		if supportsBusiness && !hasBusiness {
			readErr = fmt.Errorf("%s must return a boolean business flag", hook)
			break
		}
		if !hasState || !hasDone {
			readErr = fmt.Errorf("%s must return state and a boolean done", hook)
			break
		}
		encoded, err := common.Marshal(nextState)
		if err != nil {
			readErr = fmt.Errorf("invalid submit stream state: %w", err)
			break
		}
		if len(encoded) > maxTaskPluginPersistedJSONBytes {
			readErr = fmt.Errorf("task submit stream state exceeds size limit")
			break
		}
		if accumulated != nil {
			if len(result) != 3 && (len(result) != 4 || !hasBusiness) {
				readErr = fmt.Errorf("parseSubmitEventDelta must return changes, state, done and an optional boolean business")
				break
			}
			// Control state is separate from the full result and is the only
			// snapshot passed back to JS on the next event.
			if len(encoded) > 64<<10 {
				readErr = fmt.Errorf("task submit stream control state exceeds size limit")
				break
			}
			if stream.money {
				err = accumulated.ApplySource(ctx, result["changes"])
			} else {
				err = accumulated.Apply(ctx, result["changes"])
			}
			if err != nil {
				readErr = fmt.Errorf("invalid submit stream changes: %w", err)
				if !stream.money {
					accumulated, state = nil, nil
				}
				break
			}
		}
		// Keep the exact encoded-byte limit above. Plain JSON state goes back
		// to the next event as is; codec-specific values (for example exported
		// typed arrays) retain the old normalization.
		if isPlainJSONValue(nextState, 0) {
			state = nextState
		} else {
			var decoded any
			if err = common.Unmarshal(encoded, &decoded); err != nil {
				readErr = err
				break
			}
			state = decoded
		}
		if stream.money && stream.client != nil && !a.submitDeliveryFailed {
			written, writeErr := a.relaySubmitEvent(stream.client, currentFrame, business)
			a.submitDeliveryFailed = writeErr != nil
			relayed = stream.client.Writer.Written()
			if written {
				if delivered != nil {
					if err := delivered.ApplyAtomic(context.WithoutCancel(ctx), result["changes"]); err != nil {
						readErr = err
						break
					}
				} else {
					a.deliveredSubmitData = state
				}
			}
		}
		if done {
			completed = true
			break
		}
		data = nil
		eventName = ""
	}
	if !completed && readErr == nil {
		readErr = context.Cause(ctx)
	}
	if !completed && readErr == nil && scanner.Err() != nil {
		readErr = fmt.Errorf("read task submit stream: %w", scanner.Err())
	}
	result := state
	if accumulated != nil {
		var valueErr error
		if result, valueErr = accumulated.Value(); valueErr != nil && readErr == nil {
			readErr = valueErr
		}
	}
	if delivered != nil {
		value, err := delivered.Value()
		if err == nil {
			a.deliveredSubmitData = value
		}
	}
	switch {
	case readErr == nil && completed:
		return result, false, nil
	case !relayed:
		if readErr == nil {
			readErr = fmt.Errorf("task submit stream ended before the plugin reported completion")
		}
		if stream.money && stream.client != nil {
			return result, true, readErr
		}
		return nil, false, readErr
	case readErr == nil:
		logger.LogDebug(stream.client, "task_plugin subsystem=adaptor event=relayed_stream_closed plugin=%q reason=eof_before_done", a.plugin.Meta.Key)
		return result, stream.money, nil
	default:
		if stream.money {
			logger.LogWarn(stream.client, common.LogText("task plugin %s preserves observed usage after an incomplete submit stream: %s", a.plugin.Meta.Key, readErr.Error()))
		} else {
			logger.LogWarn(stream.client, common.LogText("task plugin %s settles a relayed submit stream that stopped early at the reserved quota: %s", a.plugin.Meta.Key, readErr.Error()))
		}
		// The client's stream is cut off rather than closed, so a downstream
		// gateway relaying it keeps its reservation too.
		_ = http.NewResponseController(stream.client.Writer).SetWriteDeadline(time.Now())
		if stream.money {
			return result, true, readErr
		}
		return result, true, nil
	}
}

// relaySubmitEvent writes one upstream SSE event, as the vendor framed it, to
// the client of a native route. The first event commits a 200 event-stream
// response; a client that has left only misses the rest.
func (a *TaskAdaptor) relaySubmitEvent(c *gin.Context, frame string, business bool) (bool, error) {
	helper.SetEventStreamHeaders(c)
	n, err := c.Writer.WriteString(frame)
	written := n == len(frame)
	if written && business && a.info != nil && a.info.BeginProviderCall != nil {
		a.submitBusiness.Store(true)
		if a.info.ResponseFacts != nil {
			a.info.ResponseFacts.MarkBusinessOutput()
		}
	}
	if err != nil {
		return written, err
	}
	if !written {
		return false, io.ErrShortWrite
	}
	return true, helper.FlushWriter(c)
}
