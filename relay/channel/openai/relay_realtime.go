package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/clock"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

type realtimeMessage struct {
	kind int
	body []byte
	err  error
}

func readRealtimeSocket(ctx context.Context, conn *websocket.Conn, messages chan<- realtimeMessage) {
	defer func() {
		if recovered := recover(); recovered != nil {
			select {
			case messages <- realtimeMessage{err: errors.New("Realtime socket reader failed")}:
			case <-ctx.Done():
			}
		}
	}()
	for {
		kind, body, err := conn.ReadMessage()
		select {
		case messages <- realtimeMessage{kind, body, err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

// OpenaiRealtimeHandler has one owner for conversation, usage and billing state.
// The two readers only deliver bounded messages. Closing either side interrupts
// all readers and blocked writers before this handler returns.
func OpenaiRealtimeHandler(c *gin.Context, info *relaycommon.RelayInfo) (apiErr *types.NewAPIError, usage *dto.RealtimeUsage) {
	if info == nil || info.ClientWs == nil || info.TargetWs == nil {
		return types.NewError(errors.New("invalid websocket connection"), types.ErrorCodeBadResponse), nil
	}
	info.IsStream = true
	if info.StreamStatus == nil {
		info.StreamStatus = relaycommon.NewStreamStatus()
	}
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	client, target := info.ClientWs, info.TargetWs
	maximum := constant.MaxRequestBodyMB
	if maximum <= 0 {
		maximum = 128
	}
	client.SetReadLimit(int64(maximum) << 20)
	target.SetReadLimit(int64(helper.DefaultMaxScannerBufferSize))
	fromClient, fromTarget := make(chan realtimeMessage), make(chan realtimeMessage)
	var workers sync.WaitGroup
	var closeOnce sync.Once
	closeSockets := func() {
		closeOnce.Do(func() {
			message := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
			_ = target.WriteControl(websocket.CloseMessage, message, time.Now().Add(time.Second))
			_ = client.WriteControl(websocket.CloseMessage, message, time.Now().Add(time.Second))
			_ = target.Close()
			_ = client.Close()
		})
	}
	workers.Go(func() { readRealtimeSocket(ctx, client, fromClient) })
	workers.Go(func() { readRealtimeSocket(ctx, target, fromTarget) })
	workers.Go(func() { <-ctx.Done(); closeSockets() })
	stopLifetime := clock.AfterFunc(time.Hour, cancel)
	defer stopLifetime()
	idleDuration := time.Duration(constant.StreamingTimeout) * time.Second
	if idleDuration <= 0 {
		idleDuration = 300 * time.Second
	}
	idle := time.NewTimer(idleDuration)
	defer idle.Stop()
	accumulator := service.NewRealtimeUsageAccumulator(info)
	sum := &dto.RealtimeUsage{}
	input := &dto.Usage{}
	var clientWrite, targetWrite helper.StreamWriteBudget
	defer func() {
		cancel()
		closeSockets()
		workers.Wait()
		for _, result := range accumulator.Finish() {
			if err := recordRealtimeResponse(c, info, &result, sum); err != nil && apiErr == nil {
				apiErr = err
			}
		}
		usage = sum
	}()
	for {
		select {
		case incoming := <-fromClient:
			if incoming.err != nil {
				if info.RealtimePeerClosed != nil {
					info.RealtimePeerClosed(true)
				}
				if websocket.IsCloseError(incoming.err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonDone, nil)
				} else {
					info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonClientGone, incoming.err)
				}
				return nil, sum
			}
			idle.Reset(idleDuration)
			message := incoming.body
			if info.RealtimeRequestHandler != nil {
				var rejection *types.NewAPIError
				message, rejection = info.RealtimeRequestHandler(message)
				if rejection != nil {
					helper.WssError(c, client, rejection)
					return rejection, sum
				}
			}
			if message == nil {
				continue
			}
			var event dto.RealtimeEvent
			if err := common.Unmarshal(message, &event); err != nil {
				return types.NewErrorWithStatusCode(errors.New("invalid Realtime event"), types.ErrorCodeInvalidRequest, http.StatusBadRequest), sum
			}
			if event.Type == dto.RealtimeEventTypeSessionUpdate && event.Session != nil {
				if event.Session.Tools != nil {
					info.RealtimeTools = event.Session.Tools
				}
			}
			// Client item creation is input too; the legacy counter names the
			// corresponding server event. Never use output limits as this input.
			if event.Type == dto.RealtimeEventTypeConversationCreate {
				event.Type = dto.RealtimeEventConversationItemCreated
			}
			text, audio, err := service.CountTokenRealtime(info, event, info.TokenizerModelName())
			if err != nil {
				return types.NewError(err, types.ErrorCodeInvalidRequest), sum
			}
			input.PromptTokens += text + audio
			input.PromptTokensDetails.AudioTokens += audio
			if err := target.SetWriteDeadline(targetWrite.Deadline(time.Now(), len(message))); err != nil {
				return types.NewError(err, types.ErrorCodeBadResponse), sum
			}
			if err := target.WriteMessage(incoming.kind, message); err != nil {
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonScannerErr, err)
				return types.NewError(err, types.ErrorCodeBadResponse), sum
			}
		case incoming := <-fromTarget:
			if incoming.err != nil {
				if info.RealtimePeerClosed != nil {
					info.RealtimePeerClosed(false)
				}
				if websocket.IsCloseError(incoming.err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonDone, nil)
				} else {
					info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonScannerErr, incoming.err)
				}
				return nil, sum
			}
			idle.Reset(idleDuration)
			info.SetFirstResponseTime()
			var event struct {
				Type    string               `json:"type"`
				Session *dto.RealtimeSession `json:"session"`
			}
			if err := common.Unmarshal(incoming.body, &event); err != nil {
				result, _ := accumulator.Observe(incoming.body, input, false)
				if result != nil {
					if err := recordRealtimeResponse(c, info, result, sum); err != nil {
						return err, sum
					}
				}
				return types.NewError(errors.New("invalid upstream Realtime event"), types.ErrorCodeBadResponse), sum
			}
			if _, err := service.DecodeRealtimeStreamEvent(incoming.body); err != nil {
				result, _ := accumulator.Observe(incoming.body, input, false)
				if result != nil {
					if err := recordRealtimeResponse(c, info, result, sum); err != nil {
						return err, sum
					}
				}
				return types.NewError(errors.New("invalid upstream Realtime event"), types.ErrorCodeBadResponse), sum
			}
			if event.Session != nil && (event.Type == dto.RealtimeEventTypeSessionUpdated || event.Type == dto.RealtimeEventTypeSessionCreated) {
				info.InputAudioFormat = common.GetStringIfEmpty(event.Session.InputAudioFormat, info.InputAudioFormat)
				info.OutputAudioFormat = common.GetStringIfEmpty(event.Session.OutputAudioFormat, info.OutputAudioFormat)
				for direction, destination := range map[string]*string{"input": &info.InputAudioFormat, "output": &info.OutputAudioFormat} {
					switch ojson.GetBytes(incoming.body, "session.audio."+direction+".format.type").String() {
					case "audio/pcm":
						*destination = "pcm16"
					case "audio/pcmu":
						*destination = "g711_ulaw"
					case "audio/pcma":
						*destination = "g711_alaw"
					}
				}
			}
			presented := incoming.body
			var commands [][]byte
			if info.RealtimeEventHandler != nil {
				var rejection *types.NewAPIError
				presented, commands, rejection = info.RealtimeEventHandler(incoming.body)
				if rejection != nil {
					result, _ := accumulator.Observe(incoming.body, input, false)
					if result != nil {
						if err := recordRealtimeResponse(c, info, result, sum); err != nil {
							return err, sum
						}
					}
					helper.WssError(c, client, rejection)
					return rejection, sum
				}
			}
			writeErr := client.SetWriteDeadline(clientWrite.Deadline(time.Now(), len(presented)))
			if writeErr == nil {
				writeErr = client.WriteMessage(incoming.kind, presented)
			}
			result, observeErr := accumulator.Observe(incoming.body, input, writeErr == nil)
			if result != nil {
				if err := recordRealtimeResponse(c, info, result, sum); err != nil {
					return err, sum
				}
				if writeErr == nil && info.RealtimeAfterResponse != nil {
					outgoing, rejection := info.RealtimeAfterResponse()
					if rejection != nil {
						helper.WssError(c, client, rejection)
						return rejection, sum
					}
					commands = append(commands, outgoing...)
				}
				conversation := ojson.GetBytes(incoming.body, "response.conversation_id")
				if !conversation.Exists() || conversation.Type != ojson.Null {
					input.PromptTokens += result.Usage.CompletionTokens
					input.PromptTokensDetails.AudioTokens += result.Usage.CompletionTokenDetails.AudioTokens
				}
			}
			if observeErr != nil {
				return types.NewError(observeErr, types.ErrorCodeBadResponse), sum
			}
			if writeErr != nil {
				if info.ResponseFacts != nil {
					info.ResponseFacts.DownstreamWriteFailed = true
				}
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonClientGone, writeErr)
				return nil, sum
			}
			for _, command := range commands {
				if err := target.SetWriteDeadline(targetWrite.Deadline(time.Now(), len(command))); err != nil {
					return types.NewError(err, types.ErrorCodeBadResponse), sum
				}
				if err := target.WriteMessage(websocket.TextMessage, command); err != nil {
					return types.NewError(err, types.ErrorCodeBadResponse), sum
				}
			}
		case <-idle.C:
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonTimeout, context.DeadlineExceeded)
			return nil, sum
		case <-ctx.Done():
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonClientGone, ctx.Err())
			return nil, sum
		}
	}
}

func recordRealtimeResponse(c *gin.Context, info *relaycommon.RelayInfo, result *relaycommon.RealtimeResponseFacts, total *dto.RealtimeUsage) *types.NewAPIError {
	if info.RealtimeResponseHandler != nil {
		if err := info.RealtimeResponseHandler(result); err != nil {
			return err
		}
	}
	usage := &dto.RealtimeUsage{InputTokens: result.Usage.PromptTokens, OutputTokens: result.Usage.CompletionTokens, TotalTokens: result.Usage.TotalTokens,
		InputTokenDetails: result.Usage.PromptTokensDetails, OutputTokenDetails: result.Usage.CompletionTokenDetails}
	if err := preConsumeUsage(c, info, usage, total); err != nil {
		return types.NewError(fmt.Errorf("Realtime accounting failed: %w", err), types.ErrorCodeBadResponse)
	}
	return nil
}

func preConsumeUsage(ctx *gin.Context, info *relaycommon.RelayInfo, usage *dto.RealtimeUsage, totalUsage *dto.RealtimeUsage) error {
	if usage == nil || totalUsage == nil {
		return errors.New("invalid usage pointer")
	}
	totalUsage.TotalTokens += usage.TotalTokens
	totalUsage.InputTokens += usage.InputTokens
	totalUsage.OutputTokens += usage.OutputTokens
	totalUsage.InputTokenDetails.CachedTokens += usage.InputTokenDetails.CachedTokens
	totalUsage.InputTokenDetails.TextTokens += usage.InputTokenDetails.TextTokens
	totalUsage.InputTokenDetails.AudioTokens += usage.InputTokenDetails.AudioTokens
	totalUsage.InputTokenDetails.ImageTokens += usage.InputTokenDetails.ImageTokens
	totalUsage.InputTokenDetails.CacheWriteTokens += usage.InputTokenDetails.CacheWriteTokens
	totalUsage.OutputTokenDetails.TextTokens += usage.OutputTokenDetails.TextTokens
	totalUsage.OutputTokenDetails.AudioTokens += usage.OutputTokenDetails.AudioTokens
	totalUsage.OutputTokenDetails.ImageTokens += usage.OutputTokenDetails.ImageTokens
	totalUsage.OutputTokenDetails.ReasoningTokens += usage.OutputTokenDetails.ReasoningTokens
	if info.RealtimeResponseHandler != nil || info.ResponseFacts != nil {
		return nil
	}
	return service.PreWssConsumeQuota(ctx, info, usage)
}
