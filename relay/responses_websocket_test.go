package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// normalizeResponsesWSTestMessage runs the read-loop envelope parse followed by
// request normalization, exactly as the session does for one response.create.
func normalizeResponsesWSTestMessage(message []byte) (responsesWSCreateRequest, string, error) {
	envelope, streamID, err := parseResponsesWSEnvelope(message)
	if err != nil {
		return responsesWSCreateRequest{StreamID: streamID}, envelope.EventID, err
	}
	create, err := normalizeResponsesWSCreateEvent(message, envelope, streamID)
	return create, envelope.EventID, err
}

func TestNormalizeResponsesWSMaxOutputTokens(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{value: "0", valid: true},
		{value: "1073741823", valid: true},
		{value: "1073741824"},
		{value: "18446744073686646784"},
		{value: "-1"},
	} {
		for _, wrapped := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/wrapped=%t", tc.value, wrapped), func(t *testing.T) {
				fields := `"model":"gpt-5.1","input":"hi","max_output_tokens":` + tc.value
				payload := `{"type":"response.create",` + fields + `}`
				if wrapped {
					payload = `{"type":"response.create","response":{` + fields + `}}`
				}
				create, _, err := normalizeResponsesWSTestMessage([]byte(payload))
				if !tc.valid {
					require.Error(t, err)
					assert.Equal(t, http.StatusBadRequest, newResponsesWSInvalidRequestError(err).StatusCode)
					return
				}
				require.NoError(t, err)
				require.NotNil(t, create.Request.MaxOutputTokens)
				assert.Equal(t, tc.value, fmt.Sprint(*create.Request.MaxOutputTokens))
			})
		}
	}
}

func TestResponsesWSAzureDialUsesAzureResponsesURLAndAPIKey(t *testing.T) {
	for _, tc := range []struct {
		name           string
		host           string
		responsesVer   string
		wantPath       string
		wantAPIVersion string
		channelAPIVer  string
	}{
		{name: "openai v1 default preview", wantPath: "/openai/v1/responses", wantAPIVersion: "preview"},
		{name: "channel responses version", responsesVer: "2025-04-01-preview", wantPath: "/openai/v1/responses", wantAPIVersion: "2025-04-01-preview"},
		{name: "cognitiveservices uses channel api version", host: "cognitiveservices.azure.com", channelAPIVer: "2025-03-01-preview", wantPath: "/openai/responses", wantAPIVersion: "2025-03-01-preview"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handshake := make(chan *http.Request, 1)
			upgrader := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handshake <- r
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				_ = conn.Close()
			}))
			defer server.Close()

			// The host check only inspects the base URL text, so a path segment
			// is enough to select the cognitiveservices URL form.
			baseURL := server.URL
			if tc.host != "" {
				baseURL += "/" + tc.host
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			info := &relaycommon.RelayInfo{
				RelayMode:      relayconstant.RelayModeResponses,
				RequestURLPath: "/v1/responses",
				ProviderMeta: &relaycommon.ProviderMeta{
					ChannelType:          constant.ChannelTypeAzure,
					ChannelBaseUrl:       baseURL,
					ApiType:              constant.APITypeOpenAI,
					ApiVersion:           tc.channelAPIVer,
					ApiKey:               "azure-key",
					UpstreamModelName:    "gpt-test",
					ChannelOtherSettings: dto.ProviderOtherSettings{AzureResponsesVersion: tc.responsesVer},
				},
			}
			adaptor := GetAdaptor(info.ApiType)
			adaptor.Init(info)
			target, err := relaychannel.DoWssRequest(adaptor, c, info, nil)
			require.NoError(t, err)
			_ = target.Close()

			r := <-handshake
			assert.Equal(t, strings.TrimPrefix(baseURL, server.URL)+tc.wantPath, r.URL.Path)
			assert.Equal(t, tc.wantAPIVersion, r.URL.Query().Get("api-version"))
			assert.Equal(t, "azure-key", r.Header.Get("api-key"))
			assert.Empty(t, r.Header.Get("Authorization"))
		})
	}
}

func TestRestoreConnectionContextRejectsChangedAdvancedCustomRoute(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{RelayMode: relayconstant.RelayModeResponses, RequestURLPath: "/v1/responses", OriginModelName: "ws-model", ResponseFacts: &relaycommon.ResponseFacts{},
		ProviderMeta: &relaycommon.ProviderMeta{ChannelType: constant.ChannelTypeAdvancedCustom, ApiType: constant.APITypeAdvancedCustom, ChannelBaseUrl: "http://upstream.example", UpstreamModelName: "ws-model", ApiKey: "provider-key"}}
	info.ChannelOtherSettings = dto.ProviderOtherSettings{AdvancedCustom: &dto.AdvancedCustomConfig{Routes: []dto.AdvancedCustomRoute{{IncomingPath: "/v1/responses", UpstreamPath: "/v1/responses"}}}}
	call := &ResponsesWSCall{session: &responsesWSSession{}}
	prepared, err := call.Prepare(c, info)
	require.NoError(t, err)
	call.session.gatewayChannelID = 10
	call.session.gatewayTransportIdentity = prepared.Identity()

	// Request metadata and an explicit native converter retain the handshake.
	info.ChannelOtherSettings = dto.ProviderOtherSettings{AdvancedCustom: &dto.AdvancedCustomConfig{Routes: []dto.AdvancedCustomRoute{{IncomingPath: "/v1/responses", UpstreamPath: "/v1/responses", Converter: "none", Models: []string{"ws-model", "other-model"}}}}}
	refreshed, err := call.Prepare(c, info)
	require.NoError(t, err)
	assert.Equal(t, prepared.Identity(), refreshed.Identity())

	info.ChannelOtherSettings = dto.ProviderOtherSettings{AdvancedCustom: &dto.AdvancedCustomConfig{Routes: []dto.AdvancedCustomRoute{{IncomingPath: "/v1/responses", UpstreamPath: "/v2/responses"}}}}
	refreshed, err = call.Prepare(c, info)
	assert.Nil(t, refreshed)
	assert.ErrorContains(t, err, "upstream connection settings changed; reconnect required")
}

func TestNormalizeResponsesWSCreateEventWrapper(t *testing.T) {
	message := []byte(`{
		"type": "response.create",
		"event_id": "evt_1",
		"generate": false,
		"response": {
			"model": "gpt-5.3-codex-spark",
			"input": "hi",
			"store": false,
			"stream": true,
			"stream_options": {"include_usage": true}
		}
	}`)

	create, eventID, err := normalizeResponsesWSTestMessage(message)
	if err != nil {
		t.Fatalf("normalizeResponsesWSTestMessage() error = %v", err)
	}
	req := create.Request
	if eventID != "evt_1" {
		t.Fatalf("eventID = %q, want evt_1", eventID)
	}
	if req.Model != "gpt-5.3-codex-spark" {
		t.Fatalf("model = %q", req.Model)
	}
	if strings.TrimSpace(string(create.Generate)) != "false" {
		t.Fatalf("generate = %s, want false", create.Generate)
	}
	if req.Stream != nil {
		t.Fatalf("stream = %v, want nil", req.Stream)
	}
	if req.StreamOptions != nil {
		t.Fatalf("stream_options = %#v, want nil", req.StreamOptions)
	}
	if strings.TrimSpace(string(req.Store)) != "false" {
		t.Fatalf("store = %s, want false", req.Store)
	}
}

func TestNormalizeResponsesWSCreateEventFlat(t *testing.T) {
	message := []byte(`{
		"type": "response.create",
		"event_id": "evt_2",
		"model": "gpt-5.3-codex-spark",
		"input": "hi",
		"generate": false,
		"stream": true,
		"background": true,
		"stream_options": {"include_usage": true}
	}`)

	create, eventID, err := normalizeResponsesWSTestMessage(message)
	if err != nil {
		t.Fatalf("normalizeResponsesWSTestMessage() error = %v", err)
	}
	req := create.Request
	if eventID != "evt_2" {
		t.Fatalf("eventID = %q, want evt_2", eventID)
	}
	if req.Model != "gpt-5.3-codex-spark" {
		t.Fatalf("model = %q", req.Model)
	}
	if strings.TrimSpace(string(create.Generate)) != "false" {
		t.Fatalf("generate = %s, want false", create.Generate)
	}
	if req.Stream != nil {
		t.Fatalf("stream = %v, want nil", req.Stream)
	}
	if req.StreamOptions != nil {
		t.Fatalf("stream_options = %#v, want nil", req.StreamOptions)
	}
}

// The HTTP body and the upstream event keep the client's member order and
// number spelling; only the WebSocket members change.
func TestResponsesWSCreateEventKeepsClientBytes(t *testing.T) {
	message := []byte(`{"type":"response.create","model":"m","text":{"format":{"type":"json_schema","name":"r","schema":{"properties":{"b":{},"a":{}}}}},"temperature":1.0,"stream":true,"generate":false}`)

	create, _, err := normalizeResponsesWSTestMessage(message)
	require.NoError(t, err)
	assert.Equal(t, `{"model":"m","text":{"format":{"type":"json_schema","name":"r","schema":{"properties":{"b":{},"a":{}}}}},"temperature":1.0}`, string(create.Body))

	event, err := buildResponsesWSCreateEvent(create.Body, create.Generate, "s1")
	require.NoError(t, err)
	assert.Equal(t, `{"model":"m","text":{"format":{"type":"json_schema","name":"r","schema":{"properties":{"b":{},"a":{}}}}},"temperature":1.0,"type":"response.create","stream_id":"s1","generate":false}`, string(event))

	_, _, err = normalizeResponsesWSTestMessage([]byte(`{"type":"response.create","response":["m"]}`))
	assert.Error(t, err)
}

func TestBuildResponsesWSCreateEventIsFlat(t *testing.T) {
	payload := []byte(`{
		"model": "gpt-5.3-codex-spark",
		"input": "hi",
		"store": false,
		"event_id": "evt_upstream",
		"stream": true,
		"background": true,
		"stream_options": {"include_usage": true}
	}`)

	got, err := buildResponsesWSCreateEvent(payload, common.RawMessage(`false`), "")
	if err != nil {
		t.Fatalf("buildResponsesWSCreateEvent() error = %v", err)
	}
	var data map[string]any
	if err := common.Unmarshal(got, &data); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if data["type"] != responsesWSEventTypeResponseCreate {
		t.Fatalf("type = %#v", data["type"])
	}
	if data["model"] != "gpt-5.3-codex-spark" || data["input"] != "hi" || data["store"] != false {
		t.Fatalf("unexpected flat event fields: %s", got)
	}
	if data["generate"] != false {
		t.Fatalf("generate = %#v, want false", data["generate"])
	}
	for _, key := range []string{"response", "event_id", "stream", "background", "stream_options"} {
		if _, ok := data[key]; ok {
			t.Fatalf("field %q should not be present in upstream event: %s", key, got)
		}
	}
}

func TestHTTPResponsesRequestOmitsWebSocketMetadata(t *testing.T) {
	var req dto.OpenAIResponsesRequest
	require.NoError(t, common.Unmarshal([]byte(`{"model":"gpt-5.3-codex-spark","input":"hi","generate":false,"stream_id":"planner"}`), &req))
	got, err := common.Marshal(req)
	require.NoError(t, err)
	var data map[string]any
	require.NoError(t, common.Unmarshal(got, &data))
	assert.NotContains(t, data, "stream_id")
	assert.NotContains(t, data, "generate")
}

func TestBuildResponsesWSErrorPayloadIncludesStatus(t *testing.T) {
	require.NoError(t, i18n.Init())
	payload, err := buildResponsesWSErrorPayload(i18n.LangEn, "evt_err", "", types.NewErrorWithStatusCode(
		errors.New("model is required"),
		types.ErrorCodeInvalidRequest,
		http.StatusBadRequest,
		types.ErrOptionWithSkipRetry(),
	))
	if err != nil {
		t.Fatalf("buildResponsesWSErrorPayload() error = %v", err)
	}
	var data struct {
		Type    string             `json:"type"`
		Status  int                `json:"status"`
		EventID string             `json:"event_id"`
		Error   *types.OpenAIError `json:"error"`
	}
	if err := common.Unmarshal(payload, &data); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if data.Type != "error" || data.Status != http.StatusBadRequest || data.EventID != "evt_err" {
		t.Fatalf("unexpected error event: %s", payload)
	}
	if data.Error == nil || data.Error.Code != string(types.ErrorCodeInvalidRequest) {
		t.Fatalf("unexpected error body: %#v", data.Error)
	}
	provider := types.WithOpenAIError(types.OpenAIError{Message: "Unknown Realtime input reference", Type: string(types.ErrorTypeNewAPIError), Code: "provider_code"}, http.StatusBadRequest)
	payload, err = buildResponsesWSErrorPayload(i18n.LangZhCN, "provider", "", provider)
	require.NoError(t, err)
	require.NoError(t, common.Unmarshal(payload, &data))
	require.NotNil(t, data.Error)
	assert.Equal(t, "Unknown Realtime input reference", data.Error.Message)
	assert.Equal(t, "provider_code", data.Error.Code)
}

func TestResponsesWSInvalidRequestErrorUsesBadRequestStatus(t *testing.T) {
	require.NoError(t, i18n.Init())
	payload, err := buildResponsesWSErrorPayload(i18n.LangEn, "", "", newResponsesWSInvalidRequestError(errors.New("bad event")))
	if err != nil {
		t.Fatalf("buildResponsesWSErrorPayload() error = %v", err)
	}
	var data struct {
		Status int `json:"status"`
	}
	if err := common.Unmarshal(payload, &data); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if data.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", data.Status, http.StatusBadRequest)
	}
}

func newTestResponsesWSTarget(t *testing.T) (*websocket.Conn, func()) {
	t.Helper()
	target, _, cleanup := newTestWebSocketPair(t)
	return target, cleanup
}

func newTestWebSocketPair(t *testing.T) (*websocket.Conn, *websocket.Conn, func()) {
	t.Helper()
	upgrader := websocket.Upgrader{}
	serverConnCh := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		serverConnCh <- conn
	}))

	targetURL := "ws" + strings.TrimPrefix(server.URL, "http")
	target, _, err := websocket.DefaultDialer.Dial(targetURL, nil)
	if err != nil {
		server.Close()
		t.Fatalf("dial websocket: %v", err)
	}
	serverConn := <-serverConnCh
	cleanup := func() {
		_ = target.Close()
		_ = serverConn.Close()
		server.Close()
	}
	return target, serverConn, cleanup
}

func TestRealtimeTransportDeduplicatesNativeResponseUsage(t *testing.T) {
	client, gatewayClient, closeClient := newTestWebSocketPair(t)
	defer closeClient()
	gatewayTarget, provider, closeTarget := newTestWebSocketPair(t)
	defer closeTarget()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime?model=public", nil)
	info := relaycommon.GenRelayInfoWs(c, gatewayClient)
	info.ProviderMeta = &relaycommon.ProviderMeta{ApiType: constant.APITypeOpenAI, ChannelType: constant.ChannelTypeOpenAI, UpstreamModelName: "gpt-4o"}
	info.TargetWs = gatewayTarget
	info.TokenizerModel = "gpt-4o"
	info.UsePrice = true // The legacy transport fixture must not access quota storage.
	info.ResponseFacts = &relaycommon.ResponseFacts{}
	type outcome struct {
		usage *dto.RealtimeUsage
		err   *types.NewAPIError
	}
	done := make(chan outcome, 1)
	go func() {
		adaptor := GetAdaptor(constant.APITypeOpenAI)
		adaptor.Init(info)
		usage, err := adaptor.DoResponse(c, nil, info)
		converted, _ := usage.(*dto.RealtimeUsage)
		done <- outcome{converted, err}
	}()
	events := []string{
		`{"type":"response.output_text.delta","response_id":"a","delta":"hello"}`,
		`{"type":"response.done","response":{"id":"a","status":"completed","usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12,"input_token_details":{"text_tokens":6,"audio_tokens":4,"cached_tokens":3},"output_token_details":{"text_tokens":1,"audio_tokens":1}}}}`,
		`{"type":"response.done","response":{"id":"a","status":"completed","usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}}`,
		`{"type":"response.done","response":{"id":"b","status":"completed","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`,
	}
	for _, event := range events {
		require.NoError(t, provider.WriteMessage(websocket.TextMessage, []byte(event)))
	}
	require.NoError(t, client.SetReadDeadline(time.Now().Add(5*time.Second)))
	for range len(events) {
		_, _, err := client.ReadMessage()
		require.NoError(t, err)
	}
	require.NoError(t, provider.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second)))
	select {
	case result := <-done:
		require.Nil(t, result.err)
		require.NotNil(t, result.usage)
		assert.Equal(t, 10, result.usage.InputTokens)
		assert.Equal(t, 2, result.usage.OutputTokens)
		assert.Equal(t, 4, result.usage.InputTokenDetails.AudioTokens)
		assert.Equal(t, 3, result.usage.InputTokenDetails.CachedTokens)
		assert.Equal(t, 1, result.usage.OutputTokenDetails.AudioTokens)
	case <-time.After(5 * time.Second):
		t.Fatal("Realtime transport did not stop")
	}
}

func TestResponsesWSRequiresModelOfferingGateway(t *testing.T) {
	require.NoError(t, i18n.Init())
	client, server, cleanup := newTestWebSocketPair(t)
	defer cleanup()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ResponsesWebSocketHelper(c, server, func(request *http.Request, _ string, handle func(*gin.Context) *types.NewAPIError) *types.NewAPIError {
			requestContext, _ := gin.CreateTestContext(httptest.NewRecorder())
			requestContext.Request = request
			return handle(requestContext)
		}, nil)
	}()
	require.NoError(t, client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","event_id":"missing-gateway","stream_id":"s1","model":"public","input":"hi"}`)))
	require.NoError(t, client.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, raw, err := client.ReadMessage()
	require.NoError(t, err)
	var event responsesWSErrorEvent
	require.NoError(t, common.Unmarshal(raw, &event))
	assert.Equal(t, "error", event.Type)
	assert.Equal(t, http.StatusNotFound, event.Status)
	assert.Equal(t, "missing-gateway", event.EventID)
	assert.Equal(t, "s1", event.StreamID)
	require.NotNil(t, event.Error)
	assert.Equal(t, "model_not_found", event.Error.Code)
	require.NoError(t, client.Close())
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("gateway rejection did not release the WebSocket worker")
	}
}

func TestResponsesWSGatewayTransportKeepsOneConnection(t *testing.T) {
	var connections atomic.Int32
	upstreamDone := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if !assert.NoError(t, err) {
			return
		}
		connections.Add(1)
		defer peer.Close()
		defer close(upstreamDone)
		_, _, err = peer.ReadMessage()
		if !assert.NoError(t, err) {
			return
		}
		_ = peer.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"first","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`))
		_, _, _ = peer.ReadMessage()
	}))
	defer upstream.Close()
	client, server, cleanup := newTestWebSocketPair(t)
	defer cleanup()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	results := make(chan *dto.Usage, 1)
	done := make(chan struct{})
	runner := func(request *http.Request, _ string, handle func(*gin.Context) *types.NewAPIError) *types.NewAPIError {
		requestContext, _ := gin.CreateTestContext(httptest.NewRecorder())
		requestContext.Request = request
		return handle(requestContext)
	}
	go func() {
		defer close(done)
		ResponsesWebSocketHelper(c, server, runner, func(c *gin.Context, call *ResponsesWSCall) *types.NewAPIError {
			info := &relaycommon.RelayInfo{RelayMode: relayconstant.RelayModeResponses, RequestURLPath: "/v1/responses", ResponseFacts: &relaycommon.ResponseFacts{},
				ProviderMeta: &relaycommon.ProviderMeta{ChannelType: constant.ChannelTypeOpenAI, ApiType: constant.APITypeOpenAI, ChannelBaseUrl: upstream.URL, UpstreamModelName: "native", ApiKey: "provider-secret"}}
			prepared, err := call.Prepare(c, info)
			if err != nil {
				return newResponsesWSInvalidRequestError(err)
			}
			payload, err := call.CreatePayload([]byte(`{"model":"native","input":"hi"}`))
			if err != nil {
				return newResponsesWSInvalidRequestError(err)
			}
			channelID := 10
			if call.LockedChannelID != 0 {
				channelID = 11
			}
			usage, apiErr := call.Exchange(c, info, channelID, 20, prepared, payload)
			if apiErr == nil {
				results <- usage
			}
			return apiErr
		})
	}()
	require.NoError(t, client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"public","input":"hi"}`)))
	require.NoError(t, client.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, raw, err := client.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"id":"first"`)
	usage := <-results
	assert.Zero(t, usage.TotalTokens)
	require.NoError(t, client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"public","input":"next"}`)))
	_, raw, err = client.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(raw), "upstream connection identity changed")
	require.NoError(t, client.Close())
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("gateway did not stop")
	}
	select {
	case <-upstreamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not stop")
	}
	assert.Equal(t, int32(1), connections.Load())
}

func TestResponsesWSMessageSizeLimit(t *testing.T) {
	previous := constant.MaxRequestBodyMB
	constant.MaxRequestBodyMB = 1
	t.Cleanup(func() { constant.MaxRequestBodyMB = previous })
	client, server, cleanup := newTestWebSocketPair(t)
	defer cleanup()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	var admitted atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		ResponsesWebSocketHelper(c, server, func(*http.Request, string, func(*gin.Context) *types.NewAPIError) *types.NewAPIError {
			admitted.Store(true)
			return nil
		}, nil)
	}()
	_ = client.WriteMessage(websocket.TextMessage, []byte(strings.Repeat("x", (1<<20)+1)))
	require.NoError(t, client.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, _, err := client.ReadMessage()
	assert.True(t, websocket.IsCloseError(err, websocket.CloseMessageTooBig), "oversized message must be rejected before admission: %v", err)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("websocket request did not exit after oversized message")
	}
	assert.False(t, admitted.Load())
}

func TestResponsesWSShutdownInterruptsBusyWriter(t *testing.T) {
	client, server, cleanupClient := newTestWebSocketPair(t)
	defer cleanupClient()
	target, peer, cleanupTarget := newTestWebSocketPair(t)
	defer cleanupTarget()
	ctx, cancel := context.WithCancelCause(context.Background())
	s := &responsesWSSession{ctx: ctx, cancel: cancel, client: server, target: target}
	// Blocked network writers own these locks. Shutdown must not wait for
	// them: its close frames are bounded control writes, and closing the
	// sockets is what interrupts those writers.
	s.clientWriteMu.Lock()
	defer s.clientWriteMu.Unlock()
	s.targetWriteMu.Lock()
	defer s.targetWriteMu.Unlock()
	done := make(chan struct{})
	go func() { s.shutdown(websocket.CloseInternalServerErr, "upstream connection closed"); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown waited for a network writer")
	}
	// A dropped socket would read as a synthetic 1006; 1011 is the frame.
	for _, conn := range []*websocket.Conn{client, peer} {
		require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
		_, _, err := conn.ReadMessage()
		assert.True(t, websocket.IsCloseError(err, websocket.CloseInternalServerErr), "want a 1011 close frame, got %v", err)
	}
	assert.Nil(t, s.getTarget())

	// An upstream dial that completes after shutdown is closed with a frame too.
	late, latePeer, cleanupLate := newTestWebSocketPair(t)
	defer cleanupLate()
	assert.False(t, s.setTarget(late))
	require.NoError(t, latePeer.SetReadDeadline(time.Now().Add(time.Second)))
	_, _, err := latePeer.ReadMessage()
	assert.True(t, websocket.IsCloseError(err, websocket.CloseNormalClosure), "want a 1000 close frame, got %v", err)
}

func TestResponsesWSPassthroughPreservesRawPricingParameters(t *testing.T) {
	create, _, err := normalizeResponsesWSTestMessage([]byte(`{"type":"response.create","generate":false,"response":{"model":"gpt-5.1","input":"hi","vendor":{"tier":"premium"},"stream":true}}`))
	require.NoError(t, err)
	call := &ResponsesWSCall{create: create}
	payload, err := call.CreatePayload(create.Body)
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"response.create","generate":false,"model":"gpt-5.1","input":"hi","vendor":{"tier":"premium"}}`, string(payload))
}

func TestResponsesWSStreamIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, fields, want string
		invalid            bool
	}{
		{name: "default", fields: `"model":"gpt-4o"`},
		{name: "named", fields: `"model":"gpt-4o","stream_id":"planner.A-1_2"`, want: "planner.A-1_2"},
		{name: "wrapped", fields: `"response":{"model":"gpt-4o","stream_id":"wrapped"}`, want: "wrapped"},
		{name: "top-level-wins", fields: `"stream_id":"outer","response":{"model":"gpt-4o","stream_id":"inner"}`, want: "outer"},
		{name: "maximum", fields: `"model":"gpt-4o","stream_id":"` + strings.Repeat("a", 256) + `"`, want: strings.Repeat("a", 256)},
		{name: "too-long", fields: `"stream_id":"` + strings.Repeat("a", 257) + `"`, invalid: true},
		{name: "empty", fields: `"stream_id":""`, invalid: true},
		{name: "null", fields: `"stream_id":null`, invalid: true},
		{name: "number", fields: `"stream_id":123`, invalid: true},
		{name: "unicode", fields: `"stream_id":"计划"`, invalid: true},
		{name: "spaces", fields: `"stream_id":"a b"`, invalid: true},
		{name: "invalid-top-level", fields: `"stream_id":"","response":{"stream_id":"inner"}`, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			create, eventID, err := normalizeResponsesWSTestMessage([]byte(`{"type":"response.create","event_id":"test",` + tc.fields + `}`))
			assert.Equal(t, "test", eventID)
			if tc.invalid {
				require.ErrorContains(t, err, "stream_id")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, create.StreamID)
			assert.NotContains(t, string(create.Body), "stream_id")
			// Channel overrides cannot replace the client's stream identity.
			for _, body := range [][]byte{create.Body, []byte(strings.TrimSuffix(string(create.Body), "}") + `,"stream_id":"override"}`)} {
				call := &ResponsesWSCall{create: create}
				payload, err := call.CreatePayload(body)
				require.NoError(t, err)
				var event map[string]any
				require.NoError(t, common.Unmarshal(payload, &event))
				assert.Equal(t, "response.create", event["type"])
				if tc.want == "" {
					assert.NotContains(t, event, "stream_id")
				} else {
					assert.Equal(t, tc.want, event["stream_id"])
				}
			}
		})
	}
}

func TestResponsesWSErrorAttribution(t *testing.T) {
	for _, tc := range []struct {
		name, payload, control            string
		terminal, ambiguous, controlError bool
	}{
		{name: "active-server-failure", payload: `{"status":500,"error":{"type":"server_error"}}`, terminal: true},
		{name: "other-stream", payload: `{"stream_id":"other","status":500}`},
		{name: "previous-response", payload: `{"response_id":"previous","status":500}`},
		{name: "cancel-event-id", payload: `{"event_id":"cancel","status":500}`, control: `{"event_id":"cancel"}`, controlError: true},
		{name: "cancel-target", payload: `{"response_id":"missing","status":400}`, control: `{"response_id":"missing"}`, controlError: true},
		{name: "cancel-code", payload: `{"error":{"type":"invalid_request_error","code":"response_not_found"}}`, control: `{"response_id":"missing"}`, controlError: true},
		{name: "ambiguous-after-cancel", payload: `{"status":500}`, control: `{"response_id":"active"}`, terminal: true, ambiguous: true},
		{name: "explicit-generation-failure", payload: `{"response_id":"active","status":500}`, control: `{"response_id":"active"}`, terminal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var event responsesWSErrorEvent
			require.NoError(t, common.Unmarshal([]byte(tc.payload), &event))
			terminal, ambiguous, controlError := responsesWSErrorEndsRequest(event, "planner", "active", []byte(tc.control))
			assert.Equal(t, tc.terminal, terminal)
			assert.Equal(t, tc.ambiguous, ambiguous)
			assert.Equal(t, tc.controlError, controlError)
		})
	}
}
