package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	appconstant "github.com/QuantumNous/new-api/constant"
	appdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	appmodel "github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/capability"
	perfmetrics "github.com/QuantumNous/new-api/pkg/perf_metrics"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/pkg/wsmanager"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/tokenkit"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

const responsesWSEventTypeResponseCreate = "response.create"
const responsesWSWriteTimeout = 30 * time.Second

// ResponsesWSRequestRunner executes the existing authentication and rate-limit
// middleware around one complete request, without a second HTTP connection.
type ResponsesWSRequestRunner func(*http.Request, string, func(*gin.Context) *types.NewAPIError) *types.NewAPIError

// ResponsesWSGateway handles resolved ModelOfferings on the existing session.
// Returning false keeps legacy requests on their original billing path.
type ResponsesWSGateway func(*gin.Context, *ResponsesWSCall) (bool, *types.NewAPIError)

// NativeWSLifecycle lets the same gateway lifecycle wait for a multiplexed
// native response. The existing transport remains the only socket owner.
// These callbacks and probe values are transient and must never be persisted.
type NativeWSLifecycle struct {
	Protocol          protocol.Type
	TransportOnly     bool
	OriginalBody      []byte
	ConnectionReady   func()
	StartedAt         time.Time
	Budget            *service.GatewayAttemptBudget
	OutputAudio       bool
	InputUsage        *dto.Usage
	EstimateInput     func([]byte, capability.Set) (*dto.Usage, error)
	Capabilities      capability.Set
	Validate          func(billingexpr.RequestInput, billingexpr.RequestInput, *dto.Usage) error
	Prepare           func(*gin.Context, *relaycommon.RelayInfo) (*relaychannel.PreparedWssRequest, error)
	CreatePayload     func([]byte) ([]byte, error)
	Exchange          func(*gin.Context, *relaycommon.RelayInfo, int, int, *relaychannel.PreparedWssRequest, []byte) (*dto.Usage, *types.NewAPIError)
	Established       func() bool
	TerminalDelivered func() bool
	SaleInput         func() billingexpr.RequestInput
	PurchaseInput     func() billingexpr.RequestInput
}

// ResponsesWSCall exposes one logical request. Only non-secret Channel
// identity survives between calls; credentials stay in the Provider transport.
type ResponsesWSCall struct {
	AuthorizeControl    func(*gin.Context) *types.NewAPIError
	Native              *NativeWSLifecycle
	Request             dto.OpenAIResponsesRequest
	Body                []byte
	LegacyConnection    bool
	LockedChannelID     int
	LockedKeyIndex      int
	LockedProviderType  int
	LockedProviderID    int
	LockedUpstreamModel string
	session             *responsesWSSession
	state               *responsesWSCallState
	create              responsesWSCreateRequest
}

// SetControlAuthenticator validates controls without admitting a generation.
func (call *ResponsesWSCall) SetControlAuthenticator(runner ResponsesWSRequestRunner) {
	call.state.controlRunner = runner
}

// EstimateInput resolves quantities known on this connection. Per-request
// configuration is billed now but does not enter previous_response_id history.
func (call *ResponsesWSCall) EstimateInput(c *gin.Context, info *relaycommon.RelayInfo, request *dto.OpenAIResponsesRequest, caps capability.Set) (*relaycommon.ResponsesInputFacts, error) {
	if len(request.Conversation) > 0 && ojson.GetBytes(request.Conversation, "@this").Type != ojson.Null {
		return nil, errors.New("Unknown conversation context; provide complete input")
	}
	meta := request.GetTokenCountMeta()
	usage, err := service.CountRequestUsage(c, meta, info)
	if err != nil {
		return nil, err
	}
	images := 0
	for _, file := range meta.Files {
		if file.FileType == types.FileTypeImage {
			images++
		}
	}
	var inherited []*dto.Usage
	if request.PreviousResponseID != "" {
		previous, ok := call.session.contexts[request.PreviousResponseID]
		if !ok {
			return nil, errors.New("Unknown response context; provide complete input")
		}
		inherited = append(inherited, previous.Context)
		images += previous.ImageCount
	}
	for _, item := range ojson.GetBytes(request.Input, "@this").Array() {
		if item.Get("type").String() != "item_reference" {
			continue
		}
		known := call.session.items[item.Get("id").String()]
		if known == nil {
			return nil, errors.New("Unknown response item; provide complete input")
		}
		inherited = append(inherited, known)
	}
	for _, previous := range inherited {
		total := usage.PromptTokens + previous.PromptTokens
		if total < usage.PromptTokens {
			return nil, errors.New("Response context exceeds token limits")
		}
		usage.PromptTokens = total
		usage.PromptTokensDetails.ImageTokens += previous.PromptTokensDetails.ImageTokens
		usage.PromptTokensDetails.AudioTokens += previous.PromptTokensDetails.AudioTokens
	}
	if images > 0 && (!slices.Contains(caps.InputModalities, capability.Image) || caps.Limits.MaxImages != nil && int64(images) > *caps.Limits.MaxImages) {
		return nil, service.ErrGatewayCapability
	}
	if usage.PromptTokensDetails.AudioTokens > 0 && !slices.Contains(caps.InputModalities, capability.Audio) {
		return nil, service.ErrGatewayCapability
	}
	usage.TotalTokens = usage.PromptTokens
	usage.PromptTokensDetails.TextTokens = usage.PromptTokens - usage.PromptTokensDetails.ImageTokens - usage.PromptTokensDetails.AudioTokens
	contextUsage := service.GatewayUsageSnapshot(usage)
	inputOnly := *request
	inputOnly.Instructions, inputOnly.Metadata, inputOnly.Text, inputOnly.ToolChoice, inputOnly.Prompt, inputOnly.Tools = nil, nil, nil, nil, nil, nil
	model := common.GetContextKeyString(c, appconstant.ContextKeyOriginalModel)
	contextUsage.PromptTokens += tokenkit.Count(model, inputOnly.GetTokenCountMeta().CombineText) - tokenkit.Count(model, meta.CombineText)
	contextUsage.TotalTokens = contextUsage.PromptTokens
	contextUsage.PromptTokensDetails.TextTokens = contextUsage.PromptTokens - contextUsage.PromptTokensDetails.ImageTokens - contextUsage.PromptTokensDetails.AudioTokens
	return &relaycommon.ResponsesInputFacts{Usage: usage, Context: contextUsage, ImageCount: images}, nil
}

// Prepare uses the existing Provider transport without sending any bytes.
func (call *ResponsesWSCall) Prepare(c *gin.Context, info *relaycommon.RelayInfo) (*relaychannel.PreparedWssRequest, error) {
	if call.Native != nil {
		return call.Native.Prepare(c, info)
	}
	prepared, err := PrepareNativeWebSocketRequest(c, info, protocol.OpenAIResponses)
	if err != nil {
		return nil, err
	}
	if call.Established() && prepared.Identity() != call.session.gatewayTransportIdentity {
		return nil, errors.New("upstream connection settings changed; reconnect required")
	}
	return prepared, nil
}

// Exchange sends one prepared request on the session's single upstream socket.
// The caller reserves funds and freezes the Attempt before calling Exchange.
func (call *ResponsesWSCall) Exchange(c *gin.Context, info *relaycommon.RelayInfo, channelID, providerID int, prepared *relaychannel.PreparedWssRequest, payload []byte) (*dto.Usage, *types.NewAPIError) {
	if call.Native != nil {
		return call.Native.Exchange(c, info, channelID, providerID, prepared, payload)
	}
	s := call.session
	call.state.authorizeControl = call.AuthorizeControl
	if s.lockedChannelID != 0 {
		return nil, newResponsesWSInvalidRequestError(errors.New("connection billing identity changed; reconnect required"))
	}
	if s.gatewayChannelID != 0 && (s.gatewayChannelID != channelID || s.gatewayProviderID != providerID || s.gatewayUpstreamModel != info.UpstreamModelName) {
		return nil, newResponsesWSInvalidRequestError(errors.New("upstream connection identity changed; reconnect required"))
	}
	if s.getTarget() == nil {
		if prepared == nil {
			return nil, types.NewError(errors.New("missing prepared Provider transport"), types.ErrorCodeInvalidRequest)
		}
		target, err := relaychannel.DoPreparedWssRequest(c, prepared)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeDoRequestFailed)
		}
		if !s.setTarget(target) {
			return nil, types.NewError(context.Canceled, types.ErrorCodeBadResponse, types.ErrOptionWithSkipRetry())
		}
		s.gatewayChannelID, s.gatewayProviderID, s.gatewayUpstreamModel = channelID, providerID, info.UpstreamModelName
		s.gatewayKeyIndex, s.gatewayProviderType = info.ChannelMultiKeyIndex, info.ChannelType
		s.gatewayTransportIdentity = prepared.Identity()
		s.registerChannelClose(providerID)
		s.startTargetReader(target)
	}
	if err := s.writeTarget(websocket.TextMessage, payload); err != nil {
		call.state.closeCode = websocket.CloseInternalServerErr
		return nil, types.NewError(err, types.ErrorCodeBadResponse, types.ErrOptionWithSkipRetry())
	}
	previousID := s.lastResponseID
	usage, apiErr := s.receive(c, call.state, info, call.create)
	if input := info.ResponsesInputFacts; input != nil && s.lastResponseID != "" && s.lastResponseID != previousID {
		id := s.lastResponseID
		maximum := appconstant.MaxRequestBodyMB
		if maximum <= 0 {
			maximum = 128
		}
		if s.contextBytes+len(id)+256 > maximum<<20 {
			call.state.closeCode = websocket.CloseMessageTooBig
		} else {
			contextUsage := service.GatewayUsageSnapshot(input.Context)
			if usage != nil {
				configuration := max(0, input.Usage.PromptTokens-input.Context.PromptTokens)
				contextUsage.PromptTokens = max(contextUsage.PromptTokens, max(0, usage.PromptTokens-configuration))
				contextUsage.PromptTokensDetails.ImageTokens = max(contextUsage.PromptTokensDetails.ImageTokens, usage.PromptTokensDetails.ImageTokens)
				contextUsage.PromptTokensDetails.AudioTokens = max(contextUsage.PromptTokensDetails.AudioTokens, usage.PromptTokensDetails.AudioTokens)
				contextUsage.PromptTokensDetails.TextTokens = contextUsage.PromptTokens - contextUsage.PromptTokensDetails.ImageTokens - contextUsage.PromptTokensDetails.AudioTokens
			}
			generated := 0
			if info.ResponseFacts != nil {
				generated = tokenkit.Count(info.TokenizerModelName(), info.ResponseFacts.GeneratedText)
			}
			if usage != nil {
				generated = max(generated, usage.CompletionTokens)
			}
			if total := contextUsage.PromptTokens + generated; total >= contextUsage.PromptTokens {
				contextUsage.PromptTokens, contextUsage.TotalTokens = total, total
				contextUsage.PromptTokensDetails.TextTokens += generated
				if s.contexts == nil {
					s.contexts = map[string]relaycommon.ResponsesInputFacts{}
				}
				if _, exists := s.contexts[id]; !exists {
					s.contextBytes += len(id) + 256
				}
				s.contexts[id] = relaycommon.ResponsesInputFacts{Context: contextUsage, ImageCount: input.ImageCount}
				for itemID, item := range info.ResponsesItemUsage {
					if s.items[itemID] != nil {
						continue
					}
					if s.contextBytes+len(itemID)+128 > maximum<<20 {
						call.state.closeCode = websocket.CloseMessageTooBig
						break
					}
					if s.items == nil {
						s.items = map[string]*dto.Usage{}
					}
					s.contextBytes += len(itemID) + 128
					s.items[itemID] = service.GatewayUsageSnapshot(item)
				}
			} else {
				call.state.closeCode = websocket.CloseMessageTooBig
			}
		}
	}
	return usage, apiErr
}

// Established includes a successful handshake followed by a failed generation.
func (call *ResponsesWSCall) Established() bool {
	if call.Native != nil {
		return call.Native.Established()
	}
	return call.session.gatewayChannelID != 0
}

func (call *ResponsesWSCall) TerminalDelivered() bool {
	if call.Native != nil {
		return call.Native.TerminalDelivered()
	}
	return call.state.terminalDelivered
}

func (call *ResponsesWSCall) Protocol() protocol.Type {
	if call.Native != nil {
		return call.Native.Protocol
	}
	return protocol.OpenAIResponses
}

func (call *ResponsesWSCall) CreatePayload(body []byte) ([]byte, error) {
	if call.Native != nil {
		return call.Native.CreatePayload(body)
	}
	return buildResponsesWSCreateEvent(body, call.create.Generate, call.create.StreamID)
}

type responsesWSCreateEvent struct {
	Type     string            `json:"type"`
	EventID  string            `json:"event_id,omitempty"`
	StreamID common.RawMessage `json:"stream_id,omitempty"`
	Generate common.RawMessage `json:"generate,omitempty"`
	Request  common.RawMessage `json:"response,omitempty"`
}

type responsesWSCreateRequest struct {
	Request  dto.OpenAIResponsesRequest
	Body     []byte
	Generate common.RawMessage
	StreamID string
}

type responsesWSErrorEvent struct {
	Type       string             `json:"type"`
	Status     int                `json:"status"`
	EventID    string             `json:"event_id,omitempty"`
	StreamID   string             `json:"stream_id,omitempty"`
	ResponseID string             `json:"response_id,omitempty"`
	Error      *types.OpenAIError `json:"error"`
}

type responsesWSMessage struct {
	kind int
	body []byte
	err  error
}

// responsesWSControl is a client control event (response.cancel) whose
// envelope the read loop already parsed.
type responsesWSControl struct {
	body              []byte
	eventID, streamID string
}

// Only the request worker reads or changes billing state. Socket readers pass
// bounded messages to it; cancellation never performs an independent refund.
type responsesWSCallState struct {
	controlRunner     ResponsesWSRequestRunner
	authorizeControl  func(*gin.Context) *types.NewAPIError
	inbox             chan responsesWSMessage
	controls          chan responsesWSControl
	done              chan struct{}
	terminal          *responsesWSMessage
	finishing         atomic.Bool
	terminalDelivered bool
	// A nonzero closeCode ends the connection after this request.
	closeCode   int
	closeReason string
}

type responsesWSSession struct {
	ctx                      context.Context
	cancel                   context.CancelFunc
	client                   *websocket.Conn
	runner                   ResponsesWSRequestRunner
	gateway                  ResponsesWSGateway
	gatewayChannelID         int
	gatewayKeyIndex          int
	gatewayProviderType      int
	gatewayProviderID        int
	gatewayTransportIdentity [32]byte
	gatewayUpstreamModel     string
	request                  *http.Request
	requestID                string
	nextEventIndex           int
	workers                  sync.WaitGroup
	closeOnce                sync.Once

	clientWriteMu sync.Mutex
	targetWriteMu sync.Mutex
	connectionMu  sync.Mutex
	target        *websocket.Conn
	unregister    func()
	stateMu       sync.Mutex
	current       *responsesWSCallState

	// clientWriteBudget is guarded by clientWriteMu.
	clientWriteBudget helper.StreamWriteBudget

	// These fields belong to the serial request worker and describe the actual
	// established connection. Per-request token/user data is never stored here.
	items           map[string]*dto.Usage
	contexts        map[string]relaycommon.ResponsesInputFacts
	contextBytes    int
	lastResponseID  string
	lockedModel     string
	lockedChannelID int
	lockedGroup     string
	lockedKey       string
	lockedKeyIndex  int
	lockedContext   map[appconstant.ContextKey]any
	lockedRoute     dto.AdvancedCustomRoute
}

func ResponsesWebSocketHelper(c *gin.Context, client *websocket.Conn, runner ResponsesWSRequestRunner, gateways ...ResponsesWSGateway) *types.NewAPIError {
	ctx, cancel := context.WithCancel(c.Request.Context())
	s := &responsesWSSession{ctx: ctx, cancel: cancel, client: client, runner: runner,
		request: c.Request.Clone(ctx), requestID: c.GetString(common.RequestIdKey)}
	if len(gateways) > 0 {
		s.gateway = gateways[0]
	}
	if s.requestID == "" {
		s.requestID = common.NewRequestId()
	}
	maxMB := appconstant.MaxRequestBodyMB
	if maxMB <= 0 {
		maxMB = 128
	}
	client.SetReadLimit(int64(maxMB) << 20)
	defer func() {
		// Unless an earlier shutdown ended the read, the client closed or left.
		// Gorilla already answered a client close frame, so this tells the upstream.
		s.shutdown(websocket.CloseNormalClosure, "")
		s.workers.Wait()
	}()

	for {
		_, message, err := client.ReadMessage()
		if err != nil {
			return nil
		}
		envelope, streamID, err := parseResponsesWSEnvelope(message)
		eventType := envelope.Type
		if err != nil {
			s.sendError(envelope.EventID, streamID, newResponsesWSInvalidRequestError(err))
			continue
		}
		if eventType != responsesWSEventTypeResponseCreate {
			// Controls are owned by the active request too. In particular a cancel
			// arriving during authentication must not precede its upstream create.
			state := s.getCurrent()
			if eventType != "response.cancel" || state == nil {
				s.sendError(envelope.EventID, streamID, newResponsesWSInvalidRequestError(fmt.Errorf("unsupported websocket event %q", eventType)))
				continue
			}
			select {
			case state.controls <- responsesWSControl{body: message, eventID: envelope.EventID, streamID: streamID}:
			case <-state.done:
			case <-s.ctx.Done():
				return nil
			default:
				s.sendError(envelope.EventID, streamID, newResponsesWSInvalidRequestError(errors.New("a response control event is already pending")))
			}
			continue
		}
		// A completed logical response may still be committing its ledger
		// intent. Preserve serial admission for an immediate next create.
		if previous := s.getCurrent(); previous != nil && previous.finishing.Load() {
			select {
			case <-previous.done:
			case <-s.ctx.Done():
				return nil
			}
		}
		state := &responsesWSCallState{inbox: make(chan responsesWSMessage), controls: make(chan responsesWSControl, 1), done: make(chan struct{})}
		if !s.tryReserveCurrent(state) {
			s.sendError(envelope.EventID, streamID, types.NewErrorWithStatusCode(errors.New("another response.create is already in progress on this websocket connection"), types.ErrorCodeInvalidRequest, http.StatusConflict, types.ErrOptionWithSkipRetry()))
			continue
		}
		requestID := fmt.Sprintf("%s-ws-%d", s.requestID, s.nextEventIndex)
		s.nextEventIndex++
		s.workers.Go(func() { s.runRequest(state, message, envelope, streamID, requestID) })
	}
}

func (s *responsesWSSession) runRequest(state *responsesWSCallState, message []byte, envelope responsesWSCreateEvent, streamID string, requestID string) {
	create, parseErr := normalizeResponsesWSCreateEvent(message, envelope, streamID)
	var apiErr *types.NewAPIError
	var upstreamURLs []string
	defer func() {
		if recovered := recover(); recovered != nil {
			apiErr = types.NewError(fmt.Errorf("responses websocket request panic: %v", recovered), types.ErrorCodeBadResponse, types.ErrOptionWithSkipRetry())
			state.closeCode = websocket.CloseInternalServerErr
		}
		// Finish the middleware count before publishing the terminal event. Hold
		// admission while writing it so an immediate next create cannot race
		// the current request's release. A failing request also shuts down
		// before that release, so a concurrent upstream close cannot report it
		// as a normal closure; shutdown itself takes neither lock.
		outgoing := state.terminal
		if apiErr != nil {
			if body, err := buildResponsesWSErrorPayload(envelope.EventID, streamID, apiErr, upstreamURLs...); err == nil {
				outgoing = &responsesWSMessage{kind: websocket.TextMessage, body: body}
			}
		}
		s.clientWriteMu.Lock()
		s.stateMu.Lock()
		if outgoing != nil {
			if err := s.client.SetWriteDeadline(s.clientWriteBudget.Deadline(time.Now(), len(outgoing.body))); err != nil {
				state.closeCode = websocket.CloseInternalServerErr
			} else if err := s.client.WriteMessage(outgoing.kind, outgoing.body); err != nil {
				state.closeCode = websocket.CloseInternalServerErr
			}
		}
		if state.closeCode != 0 {
			s.shutdown(state.closeCode, state.closeReason)
		}
		s.current = nil
		close(state.done)
		s.stateMu.Unlock()
		s.clientWriteMu.Unlock()
	}()
	request := s.request.Clone(s.ctx)
	requestBody := message
	if parseErr == nil {
		requestBody = create.Body
	}
	request.Body = io.NopCloser(bytes.NewReader(requestBody))
	request.ContentLength = int64(len(requestBody))
	request.Header.Set("Content-Type", "application/json")
	apiErr = s.runner(request, requestID, func(c *gin.Context) *types.NewAPIError {
		if parseErr != nil {
			return newResponsesWSInvalidRequestError(parseErr)
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(create.Body))
		c.Request.ContentLength = int64(len(create.Body))
		if s.gateway != nil {
			call := &ResponsesWSCall{Request: create.Request, Body: create.Body, LegacyConnection: s.lockedChannelID != 0, LockedChannelID: s.gatewayChannelID,
				LockedProviderID: s.gatewayProviderID, LockedKeyIndex: s.gatewayKeyIndex, LockedProviderType: s.gatewayProviderType, LockedUpstreamModel: s.gatewayUpstreamModel, session: s, state: state, create: create}
			if handled, gatewayErr := s.gateway(c, call); handled {
				return gatewayErr
			}
		}
		callErr := s.runCall(c, state, create)
		upstreamURLs = common.UpstreamAddresses(c)
		return callErr
	})
	if apiErr != nil && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
		state.closeCode = websocket.ClosePolicyViolation
	}
}

func (s *responsesWSSession) runCall(c *gin.Context, state *responsesWSCallState, create responsesWSCreateRequest) (apiErr *types.NewAPIError) {
	policy := service.RequestPolicy(c)
	modelName := create.Request.Model
	started := time.Now()
	var info *relaycommon.RelayInfo
	billingPrepared := false
	defer func() {
		if recovered := recover(); recovered != nil {
			apiErr = types.NewError(fmt.Errorf("responses websocket call panic: %v", recovered), types.ErrorCodeBadResponse, types.ErrOptionWithSkipRetry())
			state.closeCode = websocket.CloseInternalServerErr
		}
		if info == nil && modelName != "" {
			info = &relaycommon.RelayInfo{OriginModelName: modelName, UsingGroup: common.GetContextKeyString(c, appconstant.ContextKeyUsingGroup), StartTime: started}
		}
		perfmetrics.RecordRelayResult(c.Request.Context(), info, apiErr)
		// Settlement already marks the request policy successful, and nothing
		// reads a termination decision after this point on the WebSocket path,
		// so neither policy record belongs here.
		if info != nil && billingPrepared {
			apiErr = RefundFailedRequestBilling(c, info, apiErr)
		}
	}()
	if modelName == "" {
		return newResponsesWSInvalidRequestError(errors.New("model is required"))
	}
	if s.lockedModel != "" && modelName != s.lockedModel {
		return newResponsesWSInvalidRequestError(fmt.Errorf("responses websocket connection is locked to model %q", s.lockedModel))
	}
	if apiErr = checkResponsesWSModelAccess(c, modelName); apiErr != nil {
		return apiErr
	}
	common.SetContextKey(c, appconstant.ContextKeyOriginalModel, modelName)
	common.SetContextKey(c, appconstant.ContextKeyRequestStartTime, time.Now())
	service.GetChannelConstraints(c).AddFilter(appdto.ProviderFilter{Kind: appdto.FilterRequestPath, RequestPath: c.Request.URL.Path})

	if s.lockedChannelID != 0 {
		if apiErr = s.restoreConnectionContext(c, modelName); apiErr != nil {
			return apiErr
		}
		info = relaycommon.GenRelayInfoResponses(c, &create.Request)
		info.IsStream = true
		common.SetContextKey(c, appconstant.ContextKeyIsStream, true)
		if apiErr = PrepareRequestBilling(c, info); apiErr != nil {
			return apiErr
		}
		billingPrepared = true
		var payload []byte
		payload, apiErr = buildResponsesWSCreatePayload(c, info, create.Request, create.Generate, create.StreamID)
		if apiErr != nil {
			return apiErr
		}
		if err := s.writeTarget(websocket.TextMessage, payload); err != nil {
			state.closeCode = websocket.CloseInternalServerErr
			return types.NewError(err, types.ErrorCodeBadResponse, types.ErrOptionWithSkipRetry())
		}
	} else {
		retry := &service.RetryParam{Ctx: c, TokenGroup: common.GetContextKeyString(c, appconstant.ContextKeyUsingGroup), ModelName: modelName, RequestPath: c.Request.URL.Path, Retry: common.GetPointer(0)}
		for ; retry.GetRetry() <= common.RetryTimes; retry.IncreaseRetry() {
			var channel *appmodel.Provider
			channel, apiErr = selectResponsesWSChannel(c, modelName, retry)
			if apiErr != nil {
				return apiErr
			}
			service.AppendUsedChannel(c, channel.Id)
			if info == nil {
				info = relaycommon.GenRelayInfoResponses(c, &create.Request)
				info.IsStream = true
				common.SetContextKey(c, appconstant.ContextKeyIsStream, true)
				if apiErr = PrepareRequestBilling(c, info); apiErr != nil {
					return apiErr
				}
				billingPrepared = true
			} else {
				info.PriceData.GroupRatioInfo = helper.HandleGroupRatio(c, info)
				if apiErr = service.PrepareTieredBillingForSelectedGroup(c, info); apiErr != nil {
					return apiErr
				}
			}
			info.RetryIndex = retry.GetRetry()
			policy.BeginAttempt(channel, info.UsingGroup)
			var payload []byte
			payload, apiErr = buildResponsesWSCreatePayload(c, info, create.Request, create.Generate, create.StreamID)
			if apiErr != nil {
				return apiErr
			}
			adaptor := GetAdaptor(info.ApiType)
			adaptor.Init(info)
			target, dialErr := relaychannel.DoWssRequest(adaptor, c, info, nil)
			if dialErr != nil {
				apiErr = service.NormalizeViolationFeeError(types.NewError(dialErr, types.ErrorCodeDoRequestFailed))
				service.ResetStatusCode(apiErr, c.GetString("status_code_mapping"))
				info.LastError = apiErr
				decision := service.DecideRelayRetry(c, apiErr, common.RetryTimes-retry.GetRetry())
				service.RecordPolicyFailure(c, channel.Id, apiErr, decision)
				service.ProcessChannelError(c, *types.NewChannelError(channel.Id, channel.Type, channel.Name, channel.ProviderInfo.IsMultiKey, info.ApiKey, channel.GetAutoBan()), apiErr, info)
				if decision.Action == "retry" {
					continue
				}
				return apiErr
			}
			if !s.setTarget(target) {
				return types.NewError(context.Canceled, types.ErrorCodeBadResponse, types.ErrOptionWithSkipRetry())
			}
			if err := s.writeTarget(websocket.TextMessage, payload); err != nil {
				state.closeCode = websocket.CloseInternalServerErr
				return types.NewError(err, types.ErrorCodeBadResponse, types.ErrOptionWithSkipRetry())
			}
			s.lockedModel, s.lockedChannelID, s.lockedGroup = modelName, channel.Id, info.UsingGroup
			s.lockedKey, s.lockedKeyIndex = info.ApiKey, info.ChannelMultiKeyIndex
			s.lockedRoute, _ = channel.GetOtherSettings().AdvancedCustom.MatchPathForModel(c.Request.URL.Path, modelName)
			s.lockedContext = make(map[appconstant.ContextKey]any)
			for _, key := range []appconstant.ContextKey{
				appconstant.ContextKeyChannelId, appconstant.ContextKeyChannelName, appconstant.ContextKeyChannelType,
				appconstant.ContextKeyChannelCreateTime, appconstant.ContextKeyChannelSetting, appconstant.ContextKeyChannelOtherSetting,
				appconstant.ContextKeyChannelParamOverride, appconstant.ContextKeyChannelHeaderOverride, appconstant.ContextKeyChannelOrganization,
				appconstant.ContextKeyChannelAutoBan, appconstant.ContextKeyChannelModelMapping, appconstant.ContextKeyChannelStatusCodeMapping,
				appconstant.ContextKeyChannelIsMultiKey, appconstant.ContextKeyChannelMultiKeyIndex, appconstant.ContextKeyChannelKey, appconstant.ContextKeyChannelBaseUrl,
				appconstant.ContextKeyUpstreamRequestOrigin,
			} {
				if value, ok := c.Get(string(key)); ok {
					s.lockedContext[key] = value
				}
			}
			s.registerChannelClose(channel.Id)
			s.startTargetReader(target)
			apiErr = nil
			break
		}
		if apiErr != nil {
			return apiErr
		}
	}

	usage, receiveErr := s.receive(c, state, info, create)
	if receiveErr == nil {
		ConsumeResponsesQuota(c, info, usage)
	}
	return receiveErr
}

func (s *responsesWSSession) authorizeControl(state *responsesWSCallState, create responsesWSCreateRequest) *types.NewAPIError {
	request := s.request.Clone(s.ctx)
	request.Body = io.NopCloser(bytes.NewReader(create.Body))
	request.ContentLength = int64(len(create.Body))
	request.Header.Set("Content-Type", "application/json")
	runner := state.controlRunner
	if runner == nil {
		runner = s.runner
	}
	return runner(request, common.NewRequestId(), func(c *gin.Context) *types.NewAPIError {
		if state.authorizeControl != nil {
			return state.authorizeControl(c)
		}
		return nil
	})
}

func (s *responsesWSSession) receive(c *gin.Context, state *responsesWSCallState, info *relaycommon.RelayInfo, create responsesWSCreateRequest) (*dto.Usage, *types.NewAPIError) {
	accumulator := service.NewResponsesUsageAccumulator(info)
	info.StreamStatus = relaycommon.NewStreamStatus()
	info.StreamStatus.RequireTerminal()
	common.SetContextKey(c, appconstant.ContextKeyResponseStreamStatus, info.StreamStatus)
	timeout := time.Duration(appconstant.StreamingTimeout) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	idle := time.NewTimer(timeout)
	defer idle.Stop()
	accepted := false
	var pendingControl []byte
	var sentControl []byte
	var responseID string
	for {
		select {
		case incoming := <-state.inbox:
			idle.Reset(timeout)
			if incoming.err != nil {
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonScannerErr, incoming.err)
				state.closeCode, state.closeReason = websocket.CloseInternalServerErr, "upstream connection closed"
				return accumulator.Finish(), nil
			}
			info.SetFirstResponseTime()
			var event struct {
				dto.ResponsesStreamResponse
				StreamID string `json:"stream_id"`
			}
			decoded, decodeErr := service.DecodeResponsesStreamEvent(incoming.body)
			if decodeErr != nil {
				info.StreamStatus.RecordError("invalid upstream websocket event")
			} else {
				event.ResponsesStreamResponse = *decoded
				event.StreamID = ojson.GetBytes(incoming.body, "stream_id").String()
				if event.Type != "error" && event.StreamID != "" && event.StreamID != create.StreamID {
					if err := s.writeClient(incoming.kind, incoming.body); err != nil {
						s.shutdown(websocket.CloseInternalServerErr, "")
					}
					continue
				}
				// A repeated terminal from the previous response must never finish
				// a subsequent request on this persistent connection.
				if event.Response != nil && event.Response.ID != "" && event.Response.ID == s.lastResponseID {
					continue
				}
				if event.Type == "error" {
					var rejection responsesWSErrorEvent
					_ = common.Unmarshal(incoming.body, &rejection)
					if rejection.ResponseID != "" && rejection.ResponseID == s.lastResponseID {
						continue
					}
					terminal, ambiguous, controlError := responsesWSErrorEndsRequest(rejection, create.StreamID, responseID, sentControl)
					if !terminal {
						if err := s.writeClient(incoming.kind, incoming.body); err != nil {
							s.shutdown(websocket.CloseInternalServerErr, "")
						}
						// Only a control error in this stream resolves its pending control.
						if controlError {
							sentControl = nil
						}
						continue
					}
					if accepted {
						if rejection.Error != nil {
							code := ""
							if rejection.Error.Code != nil {
								code = fmt.Sprint(rejection.Error.Code)
							}
							info.StreamStatus.MarkFailed(code, rejection.Error.Type, rejection.Status)
						}
						accumulator.Observe(&event.ResponsesStreamResponse, incoming.body)
						info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonDone, nil)
						s.lastResponseID = responseID
						if info.ResponseFacts != nil {
							state.finishing.Store(true)
							if err := s.deliver(info, incoming, &event.ResponsesStreamResponse, accumulator); err != nil {
								state.closeCode = websocket.CloseInternalServerErr
							} else {
								state.terminalDelivered = true
							}
						} else {
							state.terminal = &incoming
						}
						if ambiguous {
							state.closeCode = websocket.CloseInternalServerErr
						}
						return accumulator.Finish(), nil
					}
					if rejection.Error == nil {
						// An error frame without an error object still describes the
						// rejected request; keep the client-facing type stable.
						rejection.Error = &types.OpenAIError{Type: "invalid_request_error", Message: event.Message, Code: event.Code}
					}
					rejected := types.WithOpenAIError(*rejection.Error, rejection.Status, types.ErrOptionWithSkipRetry())
					if rejected.StatusCode < 400 || rejected.StatusCode > 599 {
						rejected.StatusCode = http.StatusBadRequest
					}
					return accumulator.Finish(), rejected
				}
				if strings.HasPrefix(event.Type, "response.") {
					if !accepted {
						// Like HTTP, bind the session only once upstream accepted the request.
						service.RecordChannelAffinity(c, s.lockedChannelID)
					}
					accepted = true
					if event.Response != nil && event.Response.ID != "" {
						responseID = event.Response.ID
					}
				}
				accumulator.Observe(&event.ResponsesStreamResponse, incoming.body)
			}
			switch event.Type {
			case "response.completed", "response.done", "response.incomplete", "response.failed", "response.cancelled", "response.canceled":
				if event.Response != nil {
					s.lastResponseID = event.Response.ID
				}
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonDone, nil)
				if info.ResponseFacts != nil {
					state.finishing.Store(true)
					if err := s.deliver(info, incoming, &event.ResponsesStreamResponse, accumulator); err != nil {
						state.closeCode = websocket.CloseInternalServerErr
					} else {
						state.terminalDelivered = true
					}
				} else {
					state.terminal = &incoming
				}
				return accumulator.Finish(), nil
			}
			if err := s.deliver(info, incoming, &event.ResponsesStreamResponse, accumulator); err != nil {
				s.shutdown(websocket.CloseInternalServerErr, "")
			}
			if accepted && pendingControl != nil {
				if apiErr := s.authorizeControl(state, create); apiErr != nil {
					info.StreamStatus.MarkCancelled()
					state.closeCode = websocket.ClosePolicyViolation
					return accumulator.Finish(), apiErr
				}
				if err := s.writeTarget(websocket.TextMessage, pendingControl); err != nil {
					s.shutdown(websocket.CloseInternalServerErr, "")
				}
				sentControl = pendingControl
				pendingControl = nil
			}
		case control := <-state.controls:
			if apiErr := s.authorizeControl(state, create); apiErr != nil {
				info.StreamStatus.MarkCancelled()
				state.closeCode = websocket.ClosePolicyViolation
				return accumulator.Finish(), apiErr
			}
			if pendingControl != nil || sentControl != nil {
				s.sendError(control.eventID, control.streamID, newResponsesWSInvalidRequestError(errors.New("a response control event is already pending")))
				continue
			}
			if !accepted {
				pendingControl = control.body
				continue
			}
			if err := s.writeTarget(websocket.TextMessage, control.body); err != nil {
				s.shutdown(websocket.CloseInternalServerErr, "")
			}
			sentControl = control.body
		case <-idle.C:
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonTimeout, context.DeadlineExceeded)
			state.closeCode, state.closeReason = websocket.CloseInternalServerErr, "upstream response timed out"
			return accumulator.Finish(), nil
		case <-c.Request.Context().Done():
			s.shutdown(websocket.CloseInternalServerErr, "request ended")
			if errors.Is(context.Cause(c.Request.Context()), service.ErrGatewayBudget) {
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonTimeout, service.ErrGatewayBudget)
				state.closeCode, state.closeReason = websocket.CloseInternalServerErr, "upstream response timed out"
			} else {
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonClientGone, c.Request.Context().Err())
			}
			return accumulator.Finish(), nil
		}
	}
}

// deliver confirms business output only after a complete WebSocket message is
// accepted by the downstream writer. Session metadata never disarms a budget.
func (s *responsesWSSession) deliver(info *relaycommon.RelayInfo, incoming responsesWSMessage, event *dto.ResponsesStreamResponse, accumulator *service.ResponsesUsageAccumulator) error {
	err := s.writeClient(incoming.kind, incoming.body)
	facts := info.ResponseFacts
	if facts == nil {
		return err
	}
	if err != nil {
		facts.DownstreamWriteFailed = true
		return err
	}
	accumulator.RecordDelivery(event, incoming.body)
	business := false
	switch event.Type {
	case "response.output_text.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta", "response.reasoning_summary_text.delta", "response.reasoning_text.delta", "response.refusal.delta", "response.audio.delta", "response.output_audio.delta":
		business = event.Delta != ""
	case "response.output_item.added", "response.output_item.done":
		business = event.Item != nil && (event.Item.Name != "" || len(event.Item.Arguments) > 0 || event.Item.Result != "")
	}
	if event.Response != nil {
		business = business || relayconvert.ExtractOutputTextFromResponses(event.Response) != ""
		for _, output := range event.Response.Output {
			business = business || output.Name != "" || len(output.Arguments) > 0 || output.Result != ""
		}
	}
	if business {
		facts.MarkBusinessOutput()
	}
	return nil
}

// A single generation is active, but a cancel can fail independently. Correlate
// available IDs first; uncorrelated control/server errors require a bounded
// connection close because continuing could leave the generation stuck forever.
func responsesWSErrorEndsRequest(event responsesWSErrorEvent, streamID, responseID string, control []byte) (terminal, ambiguous, controlError bool) {
	if len(control) > 0 {
		var pending struct {
			EventID    string `json:"event_id"`
			StreamID   string `json:"stream_id"`
			ResponseID string `json:"response_id"`
		}
		_ = common.Unmarshal(control, &pending)
		if event.EventID != "" && event.EventID == pending.EventID {
			return false, false, true
		}
		if event.ResponseID != "" && event.ResponseID == pending.ResponseID && event.ResponseID != responseID {
			return false, false, true
		}
		if (event.StreamID == "" || event.StreamID == pending.StreamID) && event.Error != nil && event.Error.Type == "invalid_request_error" {
			switch event.Error.Code {
			case "response_not_found", "response_not_active", "response_already_completed":
				return false, false, true
			}
		}
	}
	if event.StreamID != "" && event.StreamID != streamID || responseID != "" && event.ResponseID != "" && event.ResponseID != responseID {
		return false, false, false
	}
	return true, len(control) > 0 && event.ResponseID == "", false
}

func (s *responsesWSSession) restoreConnectionContext(c *gin.Context, model string) *types.NewAPIError {
	// The channel type is not rechecked: a locked channel keeps its type, and
	// the FilterResponsesWebSocket rules were applied when it was selected.
	channel, err := appmodel.CacheGetProvider(s.lockedChannelID)
	if err != nil || channel == nil || channel.Status != common.ChannelStatusEnabled || !channel.GetSetting().ResponsesWebSocketEnabled {
		return types.NewErrorWithStatusCode(errors.New("Responses WebSocket is disabled for this channel"), types.ErrorCode(appdto.FilterResponsesWebSocket), http.StatusForbidden, types.ErrOptionWithSkipRetry())
	}
	keyEnabled := channel.Key == s.lockedKey
	if channel.ProviderInfo.IsMultiKey {
		keys := channel.GetKeys()
		status := channel.ProviderInfo.MultiKeyStatusList[s.lockedKeyIndex]
		keyEnabled = s.lockedKeyIndex >= 0 && s.lockedKeyIndex < len(keys) && keys[s.lockedKeyIndex] == s.lockedKey && (status == 0 || status == common.ChannelStatusEnabled)
	}
	if !keyEnabled {
		return types.NewErrorWithStatusCode(errors.New("the upstream connection credential is no longer enabled"), types.ErrorCodeAccessDenied, http.StatusForbidden, types.ErrOptionWithSkipRetry())
	}
	// Changes that alter the physical upstream connection require a new
	// handshake. Request-level settings can be refreshed without rotating keys.
	if channel.GetBaseURL() != s.lockedContext[appconstant.ContextKeyChannelBaseUrl] ||
		!reflect.DeepEqual(channel.GetHeaderOverride(), s.lockedContext[appconstant.ContextKeyChannelHeaderOverride]) {
		return types.NewErrorWithStatusCode(errors.New("upstream connection settings changed; reconnect required"), types.ErrorCodeAccessDenied, http.StatusForbidden, types.ErrOptionWithSkipRetry())
	}
	if previous, ok := s.lockedContext[appconstant.ContextKeyChannelSetting].(dto.ProviderSettings); ok && previous.Proxy != channel.GetSetting().Proxy {
		return types.NewErrorWithStatusCode(errors.New("upstream proxy changed; reconnect required"), types.ErrorCodeAccessDenied, http.StatusForbidden, types.ErrOptionWithSkipRetry())
	}
	// An advanced custom connection was dialed through the route matched for
	// this model. Only the fields that shape the upstream handshake force a
	// reconnect: target path, protocol conversion and credential placement.
	// Other channel types match the zero route on both sides.
	route, _ := channel.GetOtherSettings().AdvancedCustom.MatchPathForModel(c.Request.URL.Path, model)
	if strings.TrimSpace(route.UpstreamPath) != strings.TrimSpace(s.lockedRoute.UpstreamPath) ||
		route.IsNative() != s.lockedRoute.IsNative() ||
		!reflect.DeepEqual(route.Auth, s.lockedRoute.Auth) {
		return types.NewErrorWithStatusCode(errors.New("upstream route changed; reconnect required"), types.ErrorCodeAccessDenied, http.StatusForbidden, types.ErrOptionWithSkipRetry())
	}
	if pin, found, _ := service.GetChannelConstraints(c).ResolvedPin(); found {
		if pin.ChannelId != s.lockedChannelID {
			return types.NewErrorWithStatusCode(errors.New("channel pin changed; reconnect required"), types.ErrorCodeAccessDenied, http.StatusForbidden, types.ErrOptionWithSkipRetry())
		}
	} else {
		group := common.GetContextKeyString(c, appconstant.ContextKeyUsingGroup)
		if group == "auto" {
			if !slices.Contains(service.GetRequestAutoGroups(c, common.GetContextKeyString(c, appconstant.ContextKeyUserGroup)), s.lockedGroup) {
				return types.NewErrorWithStatusCode(errors.New("the connection group is no longer allowed"), types.ErrorCodeAccessDenied, http.StatusForbidden, types.ErrOptionWithSkipRetry())
			}
			group = s.lockedGroup
			common.SetContextKey(c, appconstant.ContextKeyAutoGroup, group)
		}
		if !appmodel.IsProviderEnabledForGroupModel(group, model, s.lockedChannelID) {
			return types.NewErrorWithStatusCode(errors.New("the connection channel is no longer allowed for this group and model"), types.ErrorCodeAccessDenied, http.StatusForbidden, types.ErrOptionWithSkipRetry())
		}
	}
	for key, value := range s.lockedContext {
		c.Set(string(key), value)
	}
	common.SetContextKey(c, appconstant.ContextKeyChannelSetting, channel.GetSetting())
	common.SetContextKey(c, appconstant.ContextKeyChannelOtherSetting, channel.GetOtherSettings())
	common.SetContextKey(c, appconstant.ContextKeyChannelParamOverride, channel.GetParamOverride())
	common.SetContextKey(c, appconstant.ContextKeyChannelModelMapping, channel.GetModelMapping())
	common.SetContextKey(c, appconstant.ContextKeyChannelStatusCodeMapping, channel.GetStatusCodeMapping())
	policy := service.RequestPolicy(c)
	policy.BeginAttempt(channel, s.lockedGroup)
	policy.AddEvent(service.PolicyEvent{ChannelID: channel.Id, Decision: service.PolicyDecision{Action: "select", Reason: "pinned_channel", Source: "channel_constraint"}})
	return nil
}

func buildResponsesWSCreatePayload(c *gin.Context, info *relaycommon.RelayInfo, req dto.OpenAIResponsesRequest, generate common.RawMessage, streamID string) ([]byte, *types.NewAPIError) {
	_, body, closer, apiErr := PrepareResponsesRequest(c, info, &req)
	if apiErr != nil {
		return nil, apiErr
	}
	defer closer.Close()
	jsonData, err := io.ReadAll(body)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeReadRequestBodyFailed, types.ErrOptionWithSkipRetry())
	}
	event, err := buildResponsesWSCreateEvent(jsonData, generate, streamID)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
	}
	return event, nil
}

func (s *responsesWSSession) startTargetReader(target *websocket.Conn) {
	limit := int64(helper.DefaultMaxScannerBufferSize)
	if appconstant.StreamScannerMaxBufferMB > 0 {
		limit = int64(appconstant.StreamScannerMaxBufferMB) << 20
	}
	target.SetReadLimit(limit)
	s.workers.Go(func() {
		for {
			kind, body, err := target.ReadMessage()
			incoming := responsesWSMessage{kind: kind, body: body, err: err}
			if state := s.getCurrent(); state != nil {
				select {
				case state.inbox <- incoming:
					if err != nil {
						return
					}
					continue
				case <-state.done:
				case <-s.ctx.Done():
					return
				}
			}
			// No request owns this message. An idle upstream that closed
			// normally ends the client session with the same code.
			if err != nil {
				code := websocket.CloseInternalServerErr
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					code = err.(*websocket.CloseError).Code
				}
				s.shutdown(code, "upstream connection closed")
				return
			}
			if writeErr := s.writeClient(kind, body); writeErr != nil {
				s.shutdown(websocket.CloseInternalServerErr, "")
				return
			}
		}
	})
}

func (s *responsesWSSession) getCurrent() *responsesWSCallState {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.current
}

func (s *responsesWSSession) tryReserveCurrent(state *responsesWSCallState) bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.current != nil {
		return false
	}
	s.current = state
	return true
}

func (s *responsesWSSession) getTarget() *websocket.Conn {
	s.connectionMu.Lock()
	defer s.connectionMu.Unlock()
	return s.target
}

func (s *responsesWSSession) setTarget(target *websocket.Conn) bool {
	s.connectionMu.Lock()
	defer s.connectionMu.Unlock()
	if s.ctx.Err() != nil {
		_ = target.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		_ = target.Close()
		return false
	}
	s.target = target
	return true
}

func (s *responsesWSSession) writeTarget(kind int, message []byte) error {
	s.targetWriteMu.Lock()
	defer s.targetWriteMu.Unlock()
	target := s.getTarget()
	if target == nil {
		return errors.New("responses websocket upstream is not connected")
	}
	if err := target.SetWriteDeadline(time.Now().Add(responsesWSWriteTimeout)); err != nil {
		return err
	}
	return target.WriteMessage(kind, message)
}

func (s *responsesWSSession) writeClient(kind int, message []byte) error {
	s.clientWriteMu.Lock()
	defer s.clientWriteMu.Unlock()
	if err := s.client.SetWriteDeadline(s.clientWriteBudget.Deadline(time.Now(), len(message))); err != nil {
		return err
	}
	return s.client.WriteMessage(kind, message)
}

func (s *responsesWSSession) sendError(eventID, streamID string, apiErr *types.NewAPIError) {
	if apiErr == nil {
		return
	}
	payload, err := buildResponsesWSErrorPayload(eventID, streamID, apiErr)
	if err == nil {
		_ = s.writeClient(websocket.TextMessage, payload)
	}
}

// shutdown ends the connection once. Each peer gets a close frame that says
// why, bounded to one second, and both sockets close even if a write failed.
// Close frames are control writes, which gorilla allows alongside a blocked
// data writer, so shutdown takes no write lock and never waits for workers.
func (s *responsesWSSession) shutdown(code int, reason string) {
	s.closeOnce.Do(func() {
		s.cancel()
		s.connectionMu.Lock()
		target, unregister := s.target, s.unregister
		s.target, s.unregister = nil, nil
		s.connectionMu.Unlock()
		if unregister != nil {
			unregister()
		}
		closeMessage := websocket.FormatCloseMessage(code, reason)
		_ = s.client.WriteControl(websocket.CloseMessage, closeMessage, time.Now().Add(time.Second))
		if target != nil {
			_ = target.WriteControl(websocket.CloseMessage, closeMessage, time.Now().Add(time.Second))
			_ = target.Close()
		}
		_ = s.client.Close()
	})
}

func (s *responsesWSSession) registerChannelClose(channelID int) {
	unregister := wsmanager.Register(channelID, wsmanager.KindResponses, s.closeForPolicy)
	s.connectionMu.Lock()
	if s.ctx.Err() != nil {
		s.connectionMu.Unlock()
		unregister()
		return
	}
	s.unregister = unregister
	s.connectionMu.Unlock()
}

// closeForPolicy tells both peers why the connection ends, like the realtime
// relay does.
func (s *responsesWSSession) closeForPolicy(reason string) {
	s.shutdown(websocket.ClosePolicyViolation, reason)
}

// Stream identity belongs to the WebSocket envelope. For the legacy wrapped
// input, the top-level field takes precedence over response.stream_id.
func parseResponsesWSEnvelope(message []byte) (responsesWSCreateEvent, string, error) {
	var event responsesWSCreateEvent
	if err := common.Unmarshal(message, &event); err != nil {
		return event, "", fmt.Errorf("invalid websocket event json: %w", err)
	}
	streamRaw := event.StreamID
	if len(streamRaw) == 0 && len(event.Request) > 0 {
		var wrapped struct {
			StreamID common.RawMessage `json:"stream_id"`
		}
		if err := common.Unmarshal(event.Request, &wrapped); err == nil {
			streamRaw = wrapped.StreamID
		}
	}
	var streamID string
	if len(streamRaw) > 0 {
		if err := common.Unmarshal(streamRaw, &streamID); err != nil || len(streamID) < 1 || len(streamID) > 256 {
			return event, "", errors.New("stream_id must contain 1-256 ASCII letters, digits, underscores, hyphens, or periods")
		}
		for _, char := range streamID {
			if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-' || char == '.') {
				return event, "", errors.New("stream_id must contain only ASCII letters, digits, underscores, hyphens, or periods")
			}
		}
	}
	if strings.TrimSpace(event.Type) == "" {
		return event, streamID, errors.New("websocket event type is required")
	}
	return event, streamID, nil
}

func newResponsesWSInvalidRequestError(err error) *types.NewAPIError {
	return types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
}

// normalizeResponsesWSCreateEvent builds the HTTP-shaped request body from a
// response.create whose envelope the read loop already parsed and validated.
// The body is the message with the WebSocket members removed, and is decoded
// once as the typed request; the envelope is not parsed again.
func normalizeResponsesWSCreateEvent(message []byte, event responsesWSCreateEvent, streamID string) (responsesWSCreateRequest, error) {
	create := responsesWSCreateRequest{StreamID: streamID, Generate: event.Generate}
	// Edit the wrapped request on its own so outer fields cannot enter the HTTP body.
	source := message
	if len(event.Request) > 0 {
		source = event.Request
	}
	if !ojson.GetBytesNoCopy(source, "@this").IsObject() {
		return create, errors.New("response.create request must be a JSON object")
	}
	doc := ojson.NewDoc(source)
	if len(event.Request) > 0 {
		if generate := doc.Get("generate"); len(create.Generate) == 0 && generate.Exists() {
			create.Generate = common.RawMessage(generate.Raw)
		}
	} else {
		for _, key := range []string{"type", "event_id", "background", "stream", "stream_options"} {
			if err := relaycommon.DeleteJSONMember(doc, key); err != nil {
				return create, err
			}
		}
	}
	for _, key := range []string{"generate", "stream_id"} {
		if err := relaycommon.DeleteJSONMember(doc, key); err != nil {
			return create, err
		}
	}
	payload := doc.Bytes()
	if err := common.Unmarshal(payload, &create.Request); err != nil {
		return create, err
	}
	if err := helper.ValidateResponsesMultipliers(payload); err != nil {
		return create, err
	}
	if helper.ExceedsMaxTokensLimit(create.Request.MaxOutputTokens) {
		return create, errors.New("max_output_tokens is invalid")
	}
	create.Request.Stream = nil
	create.Request.StreamOptions = nil
	create.Body = payload
	return create, nil
}

func buildResponsesWSCreateEvent(jsonData []byte, generate common.RawMessage, streamID string) ([]byte, error) {
	if !ojson.ValidBytes(jsonData) || !ojson.GetBytesNoCopy(jsonData, "@this").IsObject() {
		return nil, errors.New("response.create body must be a JSON object")
	}
	event := ojson.NewDoc(jsonData)
	// The normalized body never carries stream_id; a channel parameter
	// override may inject one, and it must not shadow the envelope's value.
	keys := []string{"type", "stream_id", "event_id", "background", "stream", "stream_options"}
	if len(generate) > 0 {
		keys = append(keys, "generate")
	}
	for _, key := range keys {
		if err := relaycommon.DeleteJSONMember(event, key); err != nil {
			return nil, err
		}
	}
	if err := event.Set("type", responsesWSEventTypeResponseCreate); err != nil {
		return nil, err
	}
	if streamID != "" {
		if err := event.Set("stream_id", streamID); err != nil {
			return nil, err
		}
	}
	if len(generate) > 0 {
		if err := event.SetRawBytes("generate", generate); err != nil {
			return nil, err
		}
	}
	return event.Bytes(), nil
}

func buildResponsesWSErrorPayload(eventID, streamID string, apiErr *types.NewAPIError, upstreamURLs ...string) ([]byte, error) {
	if apiErr == nil {
		return nil, errors.New("api error is nil")
	}
	status := apiErr.StatusCode
	if status == 0 {
		status = http.StatusInternalServerError
	}
	openaiErr := apiErr.ToOpenAIError(upstreamURLs...)
	return common.Marshal(&responsesWSErrorEvent{
		Type:     "error",
		Status:   status,
		EventID:  eventID,
		StreamID: streamID,
		Error:    &openaiErr,
	})
}

// checkResponsesWSModelAccess applies the token model limit with the same name
// matching as the HTTP distributor. Unlike HTTP, it also runs for requests
// pinned to a channel: the check is stricter there on purpose, because a
// persistent connection keeps serving the model after the pin was resolved.
func checkResponsesWSModelAccess(c *gin.Context, modelName string) *types.NewAPIError {
	if !common.GetContextKeyBool(c, appconstant.ContextKeyTokenModelLimitEnabled) {
		return nil
	}
	raw, ok := common.GetContextKey(c, appconstant.ContextKeyTokenModelLimit)
	if !ok {
		return types.NewErrorWithStatusCode(errors.New("token has no model access"), types.ErrorCodeAccessDenied, http.StatusForbidden, types.ErrOptionWithSkipRetry())
	}
	tokenModelLimit, ok := raw.(map[string]bool)
	if !ok {
		tokenModelLimit = map[string]bool{}
	}
	if !middleware.TokenModelLimitAllows(tokenModelLimit, modelName) {
		return types.NewErrorWithStatusCode(fmt.Errorf("token is not allowed to use model %s", modelName), types.ErrorCodeAccessDenied, http.StatusForbidden, types.ErrOptionWithSkipRetry())
	}
	return nil
}

// selectResponsesWSChannel narrows the shared HTTP selection to channels that
// speak the Responses WebSocket protocol and renders its outcome as a
// non-retryable NewAPIError.
func selectResponsesWSChannel(c *gin.Context, modelName string, retryParam *service.RetryParam) (*appmodel.Provider, *types.NewAPIError) {
	constraints := service.GetChannelConstraints(c)
	if !slices.ContainsFunc(constraints.Filters, func(filter appdto.ProviderFilter) bool {
		return filter.Kind == appdto.FilterResponsesWebSocket
	}) {
		constraints.AddFilter(appdto.ProviderFilter{Kind: appdto.FilterResponsesWebSocket})
	}
	channel, _, selectErr := service.SelectChannelForRequest(c, modelName, retryParam)
	if selectErr != nil {
		message := selectErr.Message
		if selectErr.MessageID != "" {
			message = i18n.T(c, selectErr.MessageID, selectErr.Params)
		}
		code := selectErr.Code
		if code == "" {
			code = types.ErrorCodeGetChannelFailed
		}
		return nil, types.NewErrorWithStatusCode(errors.New(message), code, selectErr.StatusCode, types.ErrOptionWithSkipRetry())
	}
	if err := middleware.SetupContextForSelectedChannel(c, channel, modelName); err != nil {
		return nil, err
	}
	return channel, nil
}
