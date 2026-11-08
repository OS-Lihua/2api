package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/base64"
	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/capability"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/tokenkit"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// A Realtime Response owns one ordinary gateway lifecycle. The first pending
// Response also owns the handshake; metadata alone never creates a sale.
// Financial workers wait for the existing native transport's single owner.
type realtimeGatewayResponse struct {
	session                     *realtimeGatewaySession
	call                        *relay.ResponsesWSCall
	steps                       chan *realtimeGatewayAttempt
	finalized                   chan struct{}
	apiErr                      *types.NewAPIError
	last                        realtimeGatewayCompletion
	established                 bool
	terminalDelivered           bool
	finishing                   bool
	conversationInput           *dto.Usage
	tokenizerModel, inputFormat string
}

type realtimeGatewayAttempt struct {
	channelID, providerID int
	finished              bool
	stopTimeout           func() bool
	info                  *relaycommon.RelayInfo
	prepared              *relaychannel.PreparedWssRequest
	context               context.Context
	reply                 chan realtimeGatewayCompletion
	saleInput             billingexpr.RequestInput
	purchaseInput         billingexpr.RequestInput
	input                 *dto.Usage
	response              *realtimeGatewayResponse
	defaultConversation   bool
}

type realtimeGatewayCompletion struct {
	result            *relaycommon.RealtimeResponseFacts
	apiErr            *types.NewAPIError
	saleInput         billingexpr.RequestInput
	purchaseInput     billingexpr.RequestInput
	terminalDelivered bool
}

type realtimeQueuedRequest struct {
	body      []byte
	startedAt time.Time
}

type realtimeVADTurn struct{ stopped, committed, generated bool }

type realtimeGatewaySession struct {
	context                                       context.Context
	cancel                                        context.CancelCauseFunc
	requestContext                                context.Context
	cancelRequests                                context.CancelFunc
	runner                                        relay.ResponsesWSRequestRunner
	model                                         string
	offeringID                                    int
	auth                                          relay.ResponsesWSRequestRunner
	authBody                                      []byte
	authID                                        string
	sequence                                      int
	initial                                       *realtimeGatewayResponse
	pending                                       *realtimeGatewayAttempt
	awaiting                                      *realtimeGatewayAttempt
	active                                        map[string]*realtimeGatewayAttempt
	completed                                     map[string]bool
	responses                                     []*realtimeGatewayResponse
	workers                                       sync.WaitGroup
	channelID, providerID, providerType, keyIndex int
	upstreamModel                                 string
	tokenizerModel                                string
	identity                                      [32]byte
	defaults                                      []byte
	inputText                                     strings.Builder
	clientGone                                    bool
	queued                                        []realtimeQueuedRequest
	configPending                                 bool
	capabilities                                  capability.Set
	inputFormat, outputFormat                     string
	audioBytes                                    int64
	bufferedAudioTokens, inputAudioTokens         int
	assistantTokens, assistantAudioTokens         int
	commitPending                                 bool
	vadPath                                       string
	autoResponse, autoInterrupt                   bool
	nextAutoResponse, nextAutoInterrupt           bool
	pendingDetection                              bool
	nextVADPath                                   string
	vadType, nextVADType                          string
	vadTurns                                      map[string]*realtimeVADTurn
	autoPending                                   bool
	queuedBytes                                   int
}

func RealtimeGateway(c *gin.Context) {
	offering := middleware.TextOffering(c)
	if offering == nil {
		c.Next()
		return
	}
	c.Abort()
	key, keyErr := base64.StdEncoding.DecodeString(c.GetHeader("Sec-WebSocket-Key"))
	if !websocket.IsWebSocketUpgrade(c.Request) || c.GetHeader("Sec-WebSocket-Version") != "13" || keyErr != nil || len(key) != 16 {
		gatewayError(c, http.StatusBadRequest, "invalid_request", "Invalid WebSocket handshake")
		return
	}
	if !offering.Enabled || offering.Visibility == "groups" && !slices.Contains(offering.Groups, common.GetContextKeyString(c, constant.ContextKeyUserGroup)) {
		gatewayError(c, http.StatusNotFound, "model_not_found", "Model not found")
		return
	}
	requestContext, cancelRequests := context.WithCancel(c.Request.Context())
	ioContext, cancel := context.WithCancelCause(c.Request.Context())
	session := &realtimeGatewaySession{
		context: ioContext, cancel: cancel, requestContext: requestContext, cancelRequests: cancelRequests,
		runner: newResponsesWSRequestRunner(c), auth: newResponsesWSRequestRunner(c, true),
		offeringID: offering.ID, model: c.Query("model"),
		active: map[string]*realtimeGatewayAttempt{}, completed: map[string]bool{},
		defaults: []byte(`{}`), vadTurns: map[string]*realtimeVADTurn{}, inputFormat: "pcm16", outputFormat: "pcm16", configPending: true,
	}
	session.authID = c.GetString(common.RequestIdKey)
	if session.authID == "" {
		session.authID = common.NewRequestId()
	}
	session.authBody, _ = common.Marshal(map[string]string{"model": session.model})
	defer cancelRequests()
	defer cancel(nil)
	session.initial = session.begin([]byte(`{}`), nil, &dto.Usage{}, c.GetTime(middleware.TextGatewayStartedAtKey))
	first, apiErr := session.next(session.initial)
	if apiErr != nil {
		gatewayError(c, apiErr.StatusCode, string(apiErr.GetErrorCode()), apiErr.Error())
		session.workers.Wait()
		return
	}
	var client *websocket.Conn
	defer func() {
		if client != nil {
			_ = client.Close()
		}
	}()
	for {
		transportInfo := *first.info
		transportInfo.ClientWs = client
		transportInfo.ResponseFacts = &relaycommon.ResponseFacts{}
		transportInfo.StreamStatus = relaycommon.NewStreamStatus()
		transportInfo.RealtimeRequestHandler = session.request
		transportInfo.RealtimeEventHandler = session.event
		transportInfo.RealtimeAfterResponse = session.afterResponse
		transportInfo.RealtimeResponseHandler = session.response
		transportInfo.RealtimeInputUsage = session.inputUsage
		transportInfo.RealtimeBusinessOutput = session.businessOutput
		transportInfo.RealtimePeerClosed = session.peerClosed
		nativeContext := c.Copy()
		nativeContext.Request = c.Request.WithContext(first.context)
		_, exchangeErr := relay.ExchangeRealtime(nativeContext, &transportInfo, first.prepared, func() error {
			session.channelID, session.providerID, session.providerType = first.channelID, first.providerID, first.info.ChannelType
			session.tokenizerModel = first.info.TokenizerModelName()
			session.capabilities = first.response.call.Native.Capabilities
			session.keyIndex, session.upstreamModel = first.info.ChannelMultiKeyIndex, first.info.UpstreamModelName
			session.identity = first.prepared.Identity()
			first.response.established = true
			var upgradeErr error
			client, upgradeErr = upgrader.Upgrade(c.Writer, c.Request, nil)
			if upgradeErr != nil {
				session.clientGone = true
				cancelRequests()
				return upgradeErr
			}
			transportInfo.ClientWs = client
			nativeContext.Request = c.Request.WithContext(ioContext)
			first.stopTimeout = context.AfterFunc(first.context, func() { cancel(context.Cause(first.context)) })
			if first.response.call.Native.ConnectionReady != nil {
				first.response.call.Native.ConnectionReady()
			}
			first.response.call.Native.Budget.Pause()
			return nil
		})
		if !first.response.established {
			session.pending = nil
			session.finish(first, realtimeGatewayCompletion{apiErr: exchangeErr})
			first, apiErr = session.next(session.initial)
			if apiErr == nil {
				continue
			}
			if client == nil && c.Request.Context().Err() == nil {
				gatewayError(c, apiErr.StatusCode, string(apiErr.GetErrorCode()), apiErr.Error())
			}
			cancelRequests()
			session.workers.Wait()
			return
		}
		if session.clientGone {
			cancelRequests()
		}
		completion := realtimeGatewayCompletion{apiErr: exchangeErr}
		if errors.Is(context.Cause(ioContext), service.ErrGatewayBudget) {
			completion.apiErr = types.NewError(service.ErrGatewayBudget, types.ErrorCodeDoRequestFailed)
		}
		session.finish(first, completion)
		if session.pending != nil {
			session.finish(session.pending, completion)
			session.pending = nil
		}
		if session.awaiting != nil {
			session.finish(session.awaiting, completion)
			session.awaiting = nil
		}
		for _, attempt := range session.active {
			session.finish(attempt, completion)
		}
		session.active = map[string]*realtimeGatewayAttempt{}
		session.workers.Wait()
		return
	}
}

func (session *realtimeGatewaySession) begin(body, original []byte, input *dto.Usage, startedAt time.Time) *realtimeGatewayResponse {
	session.sequence++
	requestID := fmt.Sprintf("%s-rt-%d", session.authID, session.sequence)
	response := &realtimeGatewayResponse{session: session, steps: make(chan *realtimeGatewayAttempt), finalized: make(chan struct{}), established: session.channelID != 0}
	document := ojson.NewDoc(body)
	_ = document.Set("model", session.model)
	projection := document.Bytes()
	call := &relay.ResponsesWSCall{Body: projection, LockedChannelID: session.channelID, LockedProviderID: session.providerID, LockedProviderType: session.providerType,
		LockedKeyIndex: session.keyIndex, LockedUpstreamModel: session.upstreamModel}
	decodeBody := ojson.NewDoc(append([]byte(nil), projection...))
	if value := ojson.GetBytes(projection, "max_output_tokens"); value.Type == ojson.String && value.String() == "inf" {
		_ = decodeBody.Delete("max_output_tokens")
	}
	if err := common.Unmarshal(decodeBody.Bytes(), &call.Request); err != nil {
		response.apiErr = types.NewErrorWithStatusCode(errors.New("Invalid Realtime response"), types.ErrorCodeInvalidRequest, http.StatusBadRequest)
		close(response.finalized)
		return response
	}
	response.conversationInput = session.defaultInput(false)
	instructions := session.instructionTokens()
	response.conversationInput.PromptTokens -= instructions
	response.conversationInput.PromptTokensDetails.TextTokens -= instructions
	response.tokenizerModel, response.inputFormat = session.tokenizerModel, session.inputFormat
	if session.channelID != 0 {
		var err error
		input, err = response.estimateInput(projection)
		if err != nil {
			response.apiErr = types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest)
			close(response.finalized)
			return response
		}
	}
	call.Native = &relay.NativeWSLifecycle{Protocol: protocol.OpenAIRealtime, TransportOnly: session.channelID == 0, OriginalBody: append([]byte(nil), original...), StartedAt: startedAt, InputUsage: service.GatewayUsageSnapshot(input), EstimateInput: response.estimateInput, Prepare: response.prepare, CreatePayload: response.payload,
		Exchange: response.exchange, Established: response.connected, TerminalDelivered: response.delivered, SaleInput: response.saleInput, PurchaseInput: response.purchaseInput}
	if !call.Native.TransportOnly && session.sequence == 2 {
		call.Native.Budget = session.initial.call.Native.Budget
	}
	response.call = call
	session.responses = append(session.responses, response)
	session.workers.Go(func() {
		defer close(response.finalized)
		request, err := http.NewRequestWithContext(session.requestContext, http.MethodPost, "/v1/responses", bytes.NewReader(projection))
		if err != nil {
			response.apiErr = types.NewError(err, types.ErrorCodeInvalidRequest)
			return
		}
		response.apiErr = session.runner(request, requestID, func(c *gin.Context) *types.NewAPIError { _, apiErr := responsesWSGateway(c, call); return apiErr })
	})
	return response
}

func (session *realtimeGatewaySession) next(response *realtimeGatewayResponse) (*realtimeGatewayAttempt, *types.NewAPIError) {
	select {
	case attempt := <-response.steps:
		response.finishing = false
		return attempt, nil
	case <-response.finalized:
		if response.apiErr != nil {
			return nil, response.apiErr
		}
		return nil, types.NewError(errors.New("Realtime request did not start"), types.ErrorCodeBadResponse)
	case <-session.context.Done():
		return nil, types.NewError(context.Cause(session.context), types.ErrorCodeDoRequestFailed)
	}
}

func (response *realtimeGatewayResponse) estimateInput(body []byte) (*dto.Usage, error) {
	input := service.GatewayUsageSnapshot(response.conversationInput)
	value := ojson.ParseBytes(body)
	items := value.Get("input")
	if items.Exists() && items.Type != ojson.Null || value.Get("conversation").String() == "none" {
		input = &dto.Usage{}
		if items.Exists() && items.Type != ojson.Null && !items.IsArray() {
			return nil, errors.New("Realtime input must be an array")
		}
		for _, item := range items.Array() {
			switch item.Get("type").String() {
			case "function_call":
				input.PromptTokens += tokenkit.Count(response.tokenizerModel, item.Get("name").String()+item.Get("arguments").String())
			case "function_call_output":
				input.PromptTokens += tokenkit.Count(response.tokenizerModel, item.Get("output").String())
			case "message", "":
				for _, part := range item.Get("content").Array() {
					switch part.Get("type").String() {
					case "input_text", "text":
						input.PromptTokens += tokenkit.Count(response.tokenizerModel, part.Get("text").String())
					case "input_audio":
						audio, err := base64.StdEncoding.DecodeString(part.Get("audio").String())
						if err != nil {
							return nil, errors.New("Invalid Realtime input audio")
						}
						tokens := service.CountRealtimeAudioTokens(int64(len(audio)), response.inputFormat, true)
						input.PromptTokens += tokens
						input.PromptTokensDetails.AudioTokens += tokens
					default:
						return nil, errors.New("Unsupported Realtime input content")
					}
				}
			default:
				return nil, errors.New("Unsupported Realtime input item")
			}
		}
	}
	input.PromptTokens += tokenkit.Count(response.tokenizerModel, value.Get("instructions").String())
	for _, tool := range value.Get("tools").Array() {
		input.PromptTokens += tokenkit.Count(response.tokenizerModel, tool.Raw)
	}
	input.PromptTokensDetails.TextTokens = input.PromptTokens - input.PromptTokensDetails.AudioTokens
	input.TotalTokens = input.PromptTokens
	return input, nil
}

func (response *realtimeGatewayResponse) prepare(c *gin.Context, info *relaycommon.RelayInfo) (*relaychannel.PreparedWssRequest, error) {
	prepared, err := relay.PrepareNativeWebSocketRequest(c, info, protocol.OpenAIRealtime)
	if err != nil {
		return nil, err
	}
	if response.established && prepared.Identity() != response.session.identity {
		return nil, errors.New("Upstream connection settings changed; reconnect required")
	}
	return prepared, nil
}
func (response *realtimeGatewayResponse) payload(body []byte) ([]byte, error) {
	document := ojson.NewDoc(body)
	if err := document.Delete("model"); err != nil {
		return nil, err
	}
	eventID := document.Get("event_id")
	if err := document.Delete("event_id"); err != nil {
		return nil, err
	}
	event := ojson.NewDoc([]byte(`{"type":"response.create"}`))
	if eventID.Exists() {
		if eventID.Type != ojson.String {
			return nil, errors.New("event_id must be a string")
		}
		if err := event.SetRawBytes("event_id", []byte(eventID.Raw)); err != nil {
			return nil, err
		}
	}
	if err := event.SetRawBytes("response", document.Bytes()); err != nil {
		return nil, err
	}
	return event.Bytes(), nil
}
func (response *realtimeGatewayResponse) connected() bool { return response.established }
func (response *realtimeGatewayResponse) delivered() bool { return response.terminalDelivered }
func (response *realtimeGatewayResponse) saleInput() billingexpr.RequestInput {
	return response.last.saleInput
}
func (response *realtimeGatewayResponse) purchaseInput() billingexpr.RequestInput {
	return response.last.purchaseInput
}

func (response *realtimeGatewayResponse) exchange(c *gin.Context, info *relaycommon.RelayInfo, channelID, providerID int, prepared *relaychannel.PreparedWssRequest, payload []byte) (*dto.Usage, *types.NewAPIError) {
	attempt := &realtimeGatewayAttempt{channelID: channelID, providerID: providerID, info: info, prepared: prepared, context: c.Request.Context(), reply: make(chan realtimeGatewayCompletion, 1), response: response,
		saleInput: billingexpr.RequestInput{Body: response.call.Native.OriginalBody, Headers: info.RequestHeaders}, purchaseInput: billingexpr.RequestInput{Body: payload, Headers: map[string]string{}}, input: response.call.Native.InputUsage}
	for name, values := range prepared.Headers() {
		attempt.purchaseInput.Headers[strings.ToLower(name)] = strings.Join(values, ",")
	}
	select {
	case response.steps <- attempt:
	case <-response.session.context.Done():
		return nil, types.NewError(context.Cause(response.session.context), types.ErrorCodeDoRequestFailed)
	}
	completion := <-attempt.reply
	response.last = completion
	response.terminalDelivered = completion.terminalDelivered
	if completion.result == nil {
		return nil, completion.apiErr
	}
	onBusiness := info.ResponseFacts.OnBusinessOutput
	*info.ResponseFacts = *completion.result.Facts
	info.ResponseFacts.OnBusinessOutput = onBusiness
	info.StreamStatus = completion.result.StreamStatus
	return completion.result.Usage, completion.apiErr
}

func (session *realtimeGatewaySession) finish(attempt *realtimeGatewayAttempt, completion realtimeGatewayCompletion) {
	if attempt.finished {
		return
	}
	attempt.finished = true
	attempt.response.finishing = true
	if attempt.stopTimeout != nil {
		attempt.stopTimeout()
	}
	completion.saleInput, completion.purchaseInput = attempt.saleInput, attempt.purchaseInput
	attempt.reply <- completion
}
func (session *realtimeGatewaySession) inputUsage(id string) *dto.Usage {
	if attempt := session.active[id]; attempt != nil {
		return attempt.input
	}
	return nil
}
func (session *realtimeGatewaySession) businessOutput(id string) {
	if attempt := session.active[id]; attempt != nil {
		attempt.info.ResponseFacts.MarkBusinessOutput()
	}
}
func (session *realtimeGatewaySession) peerClosed(client bool) { session.clientGone = client }

func (session *realtimeGatewaySession) request(raw []byte) ([]byte, *types.NewAPIError) {
	return session.requestAt(raw, clock.Now())
}

func (session *realtimeGatewaySession) requestAt(raw []byte, startedAt time.Time) ([]byte, *types.NewAPIError) {
	authRequest, err := http.NewRequestWithContext(session.requestContext, http.MethodPost, "/v1/responses", bytes.NewReader(session.authBody))
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeInvalidRequest)
	}
	if apiErr := session.auth(authRequest, session.authID, session.authorize); apiErr != nil {
		return nil, apiErr
	}
	for _, path := range []string{"model", "session.model", "response.model"} {
		if ojson.GetBytes(raw, path).Exists() {
			return nil, types.NewErrorWithStatusCode(errors.New("Connection model is locked"), "connection_locked", http.StatusConflict)
		}
	}
	kind := ojson.GetBytes(raw, "type").String()
	if (session.awaiting != nil || session.configPending || session.commitPending) && kind != "response.cancel" {
		maximum := constant.MaxRequestBodyMB
		if maximum <= 0 {
			maximum = 128
		}
		remaining := maximum<<20 - session.queuedBytes
		if len(raw) > remaining {
			return nil, types.NewErrorWithStatusCode(errors.New("Realtime pending request body budget exceeded"), types.ErrorCodeInvalidRequest, http.StatusRequestEntityTooLarge)
		}
		session.queued = append(session.queued, realtimeQueuedRequest{body: raw, startedAt: startedAt})
		session.queuedBytes += len(raw)
		return nil, nil
	}
	if kind == "session.update" {
		settings, apiErr := session.prepareSessionSettings([]byte(ojson.GetBytes(raw, "session").Raw), false)
		if apiErr != nil {
			return nil, apiErr
		}
		command := ojson.NewDoc(raw)
		if err := command.SetRawBytes("session", settings); err != nil {
			return nil, types.NewError(err, types.ErrorCodeInvalidRequest)
		}
		session.configPending = true
		return command.Bytes(), nil
	}
	if kind == "input_audio_buffer.append" {
		if !slices.Contains(session.capabilities.InputModalities, capability.Audio) {
			return nil, types.NewErrorWithStatusCode(errors.New("Audio input is not supported on this connection"), types.ErrorCodeInvalidRequest, http.StatusBadRequest)
		}
		value := ojson.GetBytes(raw, "audio")
		if value.Type != ojson.String {
			return nil, types.NewErrorWithStatusCode(errors.New("audio must contain base64 data"), types.ErrorCodeInvalidRequest, http.StatusBadRequest)
		}
		audio, decodeErr := base64.StdEncoding.DecodeString(value.String())
		if decodeErr != nil || int64(len(audio)) > math.MaxInt64-session.audioBytes {
			return nil, types.NewErrorWithStatusCode(errors.New("Invalid Realtime audio buffer"), types.ErrorCodeInvalidRequest, http.StatusBadRequest)
		}
		nextBytes := session.audioBytes + int64(len(audio))
		input := session.defaultInput(true)
		additional := service.CountRealtimeAudioTokens(nextBytes, session.inputFormat, true) - service.CountRealtimeAudioTokens(session.audioBytes, session.inputFormat, true)
		input.PromptTokens += additional
		input.PromptTokensDetails.AudioTokens += additional
		if maximum := session.capabilities.Limits.ContextLength; maximum != nil && int64(input.PromptTokens) > *maximum {
			return nil, types.NewErrorWithStatusCode(errors.New("Channel context limit exceeded"), types.ErrorCodeInvalidRequest, http.StatusBadRequest)
		}
		session.audioBytes = nextBytes
		return raw, nil
	}
	if kind == "input_audio_buffer.commit" {
		session.commitPending = true
		return raw, nil
	}
	if kind == "input_audio_buffer.clear" {
		session.audioBytes, session.bufferedAudioTokens = 0, 0
		return raw, nil
	}
	if kind == "conversation.item.create" {
		for _, content := range ojson.GetBytes(raw, "item.content").Array() {
			session.inputText.WriteString(content.Get("text").String())
		}
		return raw, nil
	}
	if kind != "response.create" {
		return raw, nil
	}
	body := ojson.GetBytes(raw, "response")
	if body.Exists() && !body.IsObject() {
		return nil, types.NewErrorWithStatusCode(errors.New("Invalid Realtime response"), types.ErrorCodeInvalidRequest, http.StatusBadRequest)
	}
	if !body.Exists() {
		body = ojson.Parse(`{}`)
	}
	document := ojson.NewDoc(append([]byte(nil), session.defaults...))
	for name, value := range body.Map() {
		if err := document.SetRawBytes(name, []byte(value.Raw)); err != nil {
			return nil, types.NewError(err, types.ErrorCodeInvalidRequest)
		}
	}
	if err := helper.ValidateRealtimeMultipliers(document.Bytes()); err != nil {
		return nil, types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest)
	}
	if eventID := ojson.GetBytes(raw, "event_id"); eventID.Exists() {
		if eventID.Type != ojson.String {
			return nil, types.NewErrorWithStatusCode(errors.New("event_id must be a string"), types.ErrorCodeInvalidRequest, http.StatusBadRequest)
		}
		if err := document.SetRawBytes("event_id", []byte(eventID.Raw)); err != nil {
			return nil, types.NewError(err, types.ErrorCodeInvalidRequest)
		}
	}
	attempt, apiErr := session.admitGeneration(document.Bytes(), raw, session.defaultInput(false), startedAt)
	if apiErr != nil {
		return nil, apiErr
	}
	input := service.GatewayUsageSnapshot(attempt.response.call.Native.InputUsage)
	// Send the payload already converted, bounded and priced before reserve.
	payload := attempt.purchaseInput.Body
	attempt.purchaseInput.Body = payload
	attempt.input = service.GatewayUsageSnapshot(input)
	if err = attempt.response.call.Native.Validate(attempt.saleInput, attempt.purchaseInput, input); err != nil {
		if errors.Is(err, model.ErrInsufficientWalletBalance) {
			return nil, types.NewErrorWithStatusCode(err, "insufficient_balance", http.StatusPaymentRequired)
		}
		return nil, types.NewErrorWithStatusCode(errors.New("Realtime price cannot be evaluated"), types.ErrorCodeInvalidRequest, http.StatusBadRequest)
	}
	if _, err = attempt.response.call.Native.Budget.Start(); err != nil {
		return nil, types.NewError(err, types.ErrorCodeDoRequestFailed)
	}
	if attempt.stopTimeout == nil {
		attempt.stopTimeout = context.AfterFunc(attempt.context, func() { session.cancel(context.Cause(attempt.context)) })
	}
	attempt.defaultConversation = ojson.GetBytes(payload, "response.conversation").String() != "none"
	session.pending, session.awaiting = nil, attempt
	return payload, nil
}
func (session *realtimeGatewaySession) admitGeneration(body, original []byte, input *dto.Usage, startedAt time.Time) (*realtimeGatewayAttempt, *types.NewAPIError) {
	if session.pending != nil {
		return session.pending, nil
	}
	// Completed receipts finish posting before another admission. Responses
	// that are still generating remain concurrent on this connection.
	for _, previous := range session.responses {
		if !previous.finishing {
			continue
		}
		select {
		case <-previous.finalized:
		case <-session.context.Done():
			return nil, types.NewError(context.Cause(session.context), types.ErrorCodeDoRequestFailed)
		}
	}
	response := session.begin(body, original, input, startedAt)
	attempt, apiErr := session.next(response)
	if apiErr != nil {
		return nil, apiErr
	}
	session.pending = attempt
	attempt.response.call.Native.Budget.Pause()
	return attempt, nil
}

func (session *realtimeGatewaySession) authorize(c *gin.Context) *types.NewAPIError {
	offering := middleware.TextOffering(c)
	if offering == nil || offering.ID != session.offeringID {
		return types.NewErrorWithStatusCode(errors.New("Model not found"), "model_not_found", http.StatusNotFound)
	}
	return authorizeWebSocketOffering(c, session.model)
}

func (session *realtimeGatewaySession) instructionTokens() int {
	tokens := tokenkit.Count(session.tokenizerModel, ojson.GetBytes(session.defaults, "instructions").String())
	for _, tool := range ojson.GetBytes(session.defaults, "tools").Array() {
		tokens += tokenkit.Count(session.tokenizerModel, tool.Raw)
	}
	return tokens
}

func (session *realtimeGatewaySession) defaultInput(buffered bool) *dto.Usage {
	audio := session.inputAudioTokens + session.assistantAudioTokens
	if buffered {
		audio += session.bufferedAudioTokens + service.CountRealtimeAudioTokens(session.audioBytes, session.inputFormat, true)
	}
	text := tokenkit.Count(session.tokenizerModel, session.inputText.String()) + session.instructionTokens() + session.assistantTokens - session.assistantAudioTokens
	return &dto.Usage{PromptTokens: text + audio, TotalTokens: text + audio, PromptTokensDetails: dto.InputTokenDetails{TextTokens: text, AudioTokens: audio}}
}

// prepareSessionSettings keeps native VAD detection while moving inference and
// interruption commands behind the existing request reservation boundary.
func (session *realtimeGatewaySession) prepareSessionSettings(raw []byte, initial bool) ([]byte, *types.NewAPIError) {
	value := ojson.ParseBytes(raw)
	if !value.IsObject() {
		return nil, types.NewErrorWithStatusCode(errors.New("Invalid Realtime session"), types.ErrorCodeInvalidRequest, http.StatusBadRequest)
	}
	if kind := value.Get("type"); kind.Exists() && kind.String() != "realtime" {
		return nil, types.NewErrorWithStatusCode(errors.New("Only conversational Realtime sessions are supported"), types.ErrorCodeInvalidRequest, http.StatusBadRequest)
	}
	for _, path := range []string{"input_audio_transcription", "audio.input.transcription"} {
		if transcription := value.Get(path); transcription.Exists() && transcription.Type != ojson.Null {
			return nil, types.NewErrorWithStatusCode(errors.New("Input audio transcription is not supported on this connection"), types.ErrorCodeInvalidRequest, http.StatusBadRequest)
		}
	}
	for _, path := range []string{"turn_detection.idle_timeout_ms", "audio.input.turn_detection.idle_timeout_ms"} {
		if timeout := value.Get(path); timeout.Exists() && timeout.Type != ojson.Null {
			return nil, types.NewErrorWithStatusCode(errors.New("idle_timeout_ms is not supported on this connection"), types.ErrorCodeInvalidRequest, http.StatusBadRequest)
		}
	}
	if err := helper.ValidateRealtimeMultipliers(raw); err != nil {
		return nil, types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest)
	}
	for _, path := range []string{"input_audio_format", "output_audio_format", "audio.input.format.type", "audio.output.format.type"} {
		if format := value.Get(path); format.Exists() && format.Type != ojson.Null {
			if _, valid := service.NormalizeRealtimeAudioFormat(format.String()); !valid {
				return nil, types.NewErrorWithStatusCode(errors.New("Unsupported Realtime audio format"), types.ErrorCodeInvalidRequest, http.StatusBadRequest)
			}
		}
	}
	for _, path := range []string{"audio.input.format.rate", "audio.output.format.rate"} {
		if rate := value.Get(path); rate.Exists() {
			var sampleRate int
			if common.UnmarshalJsonStr(rate.Raw, &sampleRate) != nil || sampleRate != 24000 {
				return nil, types.NewErrorWithStatusCode(errors.New("PCM audio requires a sample rate of 24000"), types.ErrorCodeInvalidRequest, http.StatusBadRequest)
			}
		}
	}
	path := "turn_detection"
	if value.Get("audio.input.turn_detection").Exists() || value.Get("audio").IsObject() {
		path = "audio.input.turn_detection"
	}
	detection := value.Get(path)
	if !detection.Exists() {
		if initial {
			return []byte(`{}`), nil
		}
		return raw, nil
	}
	create, interrupt := session.autoResponse, session.autoInterrupt
	if initial || session.vadType == "" {
		create, interrupt = true, true
	}
	kind := detection.Get("type").String()
	if kind == "" {
		kind = session.vadType
	}
	if detection.Type == ojson.Null {
		create, interrupt, kind = false, false, ""
	} else {
		if !detection.IsObject() || kind != "server_vad" && kind != "semantic_vad" {
			return nil, types.NewErrorWithStatusCode(errors.New("Unsupported Realtime turn detection"), types.ErrorCodeInvalidRequest, http.StatusBadRequest)
		}
		for _, setting := range []struct {
			name   string
			target *bool
		}{{"create_response", &create}, {"interrupt_response", &interrupt}} {
			if flag := detection.Get(setting.name); flag.Exists() {
				if common.UnmarshalJsonStr(flag.Raw, setting.target) != nil {
					return nil, types.NewErrorWithStatusCode(errors.New("Turn detection flags must be boolean"), types.ErrorCodeInvalidRequest, http.StatusBadRequest)
				}
			}
		}
	}
	if initial {
		session.autoResponse, session.autoInterrupt, session.vadType = create, interrupt, kind
		session.vadPath = path
	} else {
		session.nextAutoResponse, session.nextAutoInterrupt, session.nextVADType = create, interrupt, kind
		session.pendingDetection = true
		session.nextVADPath = path
	}
	document := ojson.NewDoc(append([]byte(nil), raw...))
	if initial {
		document = ojson.NewDoc([]byte(`{}`))
	}
	if detection.Type == ojson.Null {
		if initial {
			return document.Bytes(), nil
		}
		_ = document.SetRawBytes(path, []byte(`null`))
		return document.Bytes(), nil
	}
	physical := ojson.NewDoc([]byte(detection.Raw))
	_ = physical.Set("type", kind)
	_ = physical.Set("create_response", false)
	_ = physical.Set("interrupt_response", false)
	if err := document.SetRawBytes(path, physical.Bytes()); err != nil {
		return nil, types.NewError(err, types.ErrorCodeInvalidRequest)
	}
	if value.Get("type").String() == "realtime" || strings.HasPrefix(path, "audio.") {
		_ = document.Set("type", "realtime")
	}
	return document.Bytes(), nil
}

func (session *realtimeGatewaySession) activeDefault() bool {
	if session.awaiting != nil && session.awaiting.defaultConversation {
		return true
	}
	for _, attempt := range session.active {
		if attempt.defaultConversation {
			return true
		}
	}
	return false
}

func (session *realtimeGatewaySession) event(raw []byte) ([]byte, [][]byte, *types.NewAPIError) {
	kind := ojson.GetBytes(raw, "type").String()
	switch kind {
	case "session.created", "session.updated":
		value := ojson.GetBytes(raw, "session")
		var commands [][]byte
		if kind == "session.created" {
			session.configPending = false
			settings, apiErr := session.prepareSessionSettings([]byte(value.Raw), true)
			if apiErr != nil {
				return nil, nil, apiErr
			}
			if len(ojson.ParseBytes(settings).Map()) > 0 {
				command := ojson.NewDoc([]byte(`{"type":"session.update"}`))
				if err := command.SetRawBytes("session", settings); err != nil {
					return nil, nil, types.NewError(err, types.ErrorCodeBadResponse)
				}
				commands = append(commands, command.Bytes())
				session.configPending = true
			}
		} else {
			if session.pendingDetection {
				session.autoResponse, session.autoInterrupt, session.vadType = session.nextAutoResponse, session.nextAutoInterrupt, session.nextVADType
				session.vadPath, session.pendingDetection = session.nextVADPath, false
			}
			physical := value.Get(session.vadPath)
			if session.vadPath != "" && physical.IsObject() && (physical.Get("create_response").Bool() || physical.Get("interrupt_response").Bool()) {
				return nil, nil, types.NewError(errors.New("Upstream automatic generation could not be controlled"), types.ErrorCodeBadResponse)
			}
			session.configPending = false
		}
		for _, direction := range []string{"input", "output"} {
			format := value.Get(direction + "_audio_format")
			if native := value.Get("audio." + direction + ".format.type"); native.Exists() {
				format = native
			}
			if normalized, valid := service.NormalizeRealtimeAudioFormat(format.String()); valid {
				if direction == "input" {
					if normalized != session.inputFormat {
						session.bufferedAudioTokens += service.CountRealtimeAudioTokens(session.audioBytes, session.inputFormat, true)
						session.audioBytes = 0
					}
					session.inputFormat = normalized
				} else {
					session.outputFormat = normalized
				}
			}
		}
		document := ojson.NewDoc([]byte(`{}`))
		for _, field := range []string{"instructions", "tools", "tool_choice", "temperature", "modalities", "output_modalities", "max_output_tokens"} {
			if property := value.Get(field); property.Exists() {
				_ = document.SetRawBytes(field, []byte(property.Raw))
			}
		}
		if property := value.Get("max_response_output_tokens"); property.Exists() && !value.Get("max_output_tokens").Exists() {
			_ = document.SetRawBytes("max_output_tokens", []byte(property.Raw))
		}
		session.defaults = document.Bytes()
		presented := ojson.NewDoc(append([]byte(nil), raw...))
		if value.Get("model").Exists() {
			_ = presented.Set("session.model", session.model)
		}
		if session.vadPath != "" && value.Get(session.vadPath).IsObject() {
			_ = presented.Set("session."+session.vadPath+".create_response", session.autoResponse)
			_ = presented.Set("session."+session.vadPath+".interrupt_response", session.autoInterrupt)
		}
		queued, apiErr := session.releaseQueuedRequests()
		return presented.Bytes(), append(commands, queued...), apiErr
	case "response.created":
		id := ojson.GetBytes(raw, "response.id").String()
		if id == "" || session.awaiting == nil {
			return nil, nil, types.NewError(errors.New("Unexpected Realtime response identity"), types.ErrorCodeBadResponse)
		}
		session.active[id], session.awaiting = session.awaiting, nil
		commands, apiErr := session.releaseQueuedRequests()
		return raw, commands, apiErr
	case "input_audio_buffer.speech_started", "input_audio_buffer.speech_stopped", "input_audio_buffer.committed":
		id := ojson.GetBytes(raw, "item_id").String()
		if id == "" {
			return nil, nil, types.NewError(errors.New("Realtime audio event has no item identity"), types.ErrorCodeBadResponse)
		}
		turn := session.vadTurns[id]
		if turn == nil {
			turn = &realtimeVADTurn{}
			session.vadTurns[id] = turn
		}
		if kind == "input_audio_buffer.speech_started" {
			var commands [][]byte
			if session.autoInterrupt {
				for responseID, attempt := range session.active {
					if !attempt.defaultConversation {
						continue
					}
					command, err := common.Marshal(map[string]string{"type": "response.cancel", "response_id": responseID})
					if err != nil {
						return nil, nil, types.NewError(err, types.ErrorCodeBadResponse)
					}
					commands = append(commands, command)
				}
			}
			return raw, commands, nil
		}
		if kind == "input_audio_buffer.speech_stopped" {
			turn.stopped = true
		}
		if kind == "input_audio_buffer.committed" && !turn.committed {
			turn.committed, session.commitPending = true, false
			session.inputAudioTokens += session.bufferedAudioTokens + service.CountRealtimeAudioTokens(session.audioBytes, session.inputFormat, true)
			session.audioBytes, session.bufferedAudioTokens = 0, 0
		}
		if session.autoResponse && turn.stopped && turn.committed && !turn.generated {
			turn.generated = true
			if session.activeDefault() {
				session.autoPending = session.autoInterrupt
			} else {
				command, apiErr := session.requestAt([]byte(`{"type":"response.create","response":{}}`), clock.Now())
				if apiErr != nil {
					return nil, nil, apiErr
				}
				if command != nil {
					return raw, [][]byte{command}, nil
				}
			}
		}
		commands, apiErr := session.releaseQueuedRequests()
		return raw, commands, apiErr
	}
	return raw, nil, nil
}

func (session *realtimeGatewaySession) afterResponse() ([][]byte, *types.NewAPIError) {
	if session.autoPending && !session.activeDefault() && session.autoResponse {
		session.autoPending = false
		command, apiErr := session.requestAt([]byte(`{"type":"response.create","response":{}}`), clock.Now())
		if apiErr != nil {
			return nil, apiErr
		}
		if command != nil {
			return [][]byte{command}, nil
		}
	}
	return session.releaseQueuedRequests()
}
func (session *realtimeGatewaySession) releaseQueuedRequests() ([][]byte, *types.NewAPIError) {
	var commands [][]byte
	for len(session.queued) > 0 && session.awaiting == nil && !session.configPending && !session.commitPending {
		queued := session.queued[0]
		session.queued[0] = realtimeQueuedRequest{}
		session.queued = session.queued[1:]
		session.queuedBytes -= len(queued.body)
		command, apiErr := session.requestAt(queued.body, queued.startedAt)
		if apiErr != nil {
			return nil, apiErr
		}
		if command != nil {
			commands = append(commands, command)
		}
	}
	return commands, nil
}

func (session *realtimeGatewaySession) response(result *relaycommon.RealtimeResponseFacts) *types.NewAPIError {
	if session.completed[result.ID] {
		return nil
	}
	attempt := session.active[result.ID]
	if attempt == nil {
		return types.NewError(errors.New("Realtime usage has no admitted request"), types.ErrorCodeBadResponse)
	}
	session.completed[result.ID] = true
	delete(session.active, result.ID)
	if attempt.defaultConversation {
		generated := result.Usage.CompletionTokens
		audio := result.Usage.CompletionTokenDetails.AudioTokens
		if result.Facts.GeneratedOutputTokens != nil {
			generated = max(generated, *result.Facts.GeneratedOutputTokens)
			audio = max(audio, *result.Facts.GeneratedOutputTokens-tokenkit.Count(session.tokenizerModel, result.Facts.GeneratedText))
		}
		session.assistantTokens += generated
		session.assistantAudioTokens += min(audio, generated)
	}
	session.finish(attempt, realtimeGatewayCompletion{result: result, terminalDelivered: result.Outcome.EndReason == relaycommon.StreamEndReasonDone && !result.Facts.DownstreamWriteFailed})
	return nil
}
