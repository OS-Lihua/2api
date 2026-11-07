package router

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/capability"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type channelProbeRoundTripper func(*http.Request) (*http.Response, error)

func (f channelProbeRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

// configuredChannelPrice models a configured price version, including zero.
type configuredChannelPrice struct{}

func (configuredChannelPrice) HasPurchasePrice(context.Context, *gorm.DB, *model.Channel) (bool, error) {
	return true, nil
}

// Exercise the management boundary with actual PAT authentication and Casbin.
func TestSingleModelChannelAPI(t *testing.T) {
	model.SetChannelPurchasePriceChecker(nil)
	t.Cleanup(func() { model.SetChannelPurchasePriceChecker(nil) })
	previousMaster, previousRedis := common.IsMasterNode, common.RedisEnabled
	t.Cleanup(func() { common.IsMasterNode, common.RedisEnabled = previousMaster, previousRedis })
	common.IsMasterNode, common.RedisEnabled = true, false
	db := model.UseTestDatabases(t)
	require.NoError(t, i18n.Init())
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.UserSession{}, &model.CasbinRule{}, &model.AuthzRole{}, &model.UserAccessToken{}, &model.Option{}, &model.Provider{}, &model.Channel{}, &model.ChannelProbeResult{}, &model.Ability{}, &model.TaskPlugin{}, &model.Model{}, &model.Vendor{}, &model.Wallet{}, &model.LedgerEntry{}))
	require.NoError(t, model.MigrateAuditLogs())
	require.NoError(t, model.EnsureLegacyAccessTokenRetireAt(time.Now().Unix()))
	require.NoError(t, authz.Init(db))
	root := model.User{Username: "channel-root", Password: "placeholder", Role: common.RoleRootUser, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: "channel-root"}
	require.NoError(t, db.Create(&root).Error)
	seedBalance, err := money.NewAmount(10_000_000_000, model.SettlementCurrency())
	require.NoError(t, err)
	_, err = model.TopUpWallet(model.WalletTopUp{UserId: root.Id, Source: seedBalance, Reference: "channel-probe-fixture"})
	require.NoError(t, err)
	token := createWalletTestAccessToken(t, root.Id, "channel:read", "channel:write", "channel:sensitive_write", "channel:operate", "model:write", "model:read", "ledger:read")
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	SetApiRouter(engine)
	call := func(method, path, body string) (int, map[string]any) {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, request)
		result := map[string]any{}
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &result), recorder.Body.String())
		return recorder.Code, result
	}
	walletPath := fmt.Sprintf("/api/admin/v1/user-wallets/%d", root.Id)
	walletStatus, walletBefore := call(http.MethodGet, walletPath, "")
	require.Equal(t, http.StatusOK, walletStatus, walletBefore)
	require.Equal(t, true, walletBefore["success"])
	assert.Equal(t, "10", walletBefore["data"].(map[string]any)["balance"])
	publisher := model.Vendor{Name: "Channel publisher", Status: 1}
	require.NoError(t, publisher.Insert())
	unified := model.Model{ModelName: "gpt-4o", VendorID: publisher.Id, Status: 1}
	require.NoError(t, unified.Insert())
	provider := model.Provider{Name: "Channel provider", Key: "secret", Status: common.ChannelStatusEnabled, Type: 1}
	require.NoError(t, db.Create(&provider).Error)
	body := fmt.Sprintf(`{"provider_id":%d,"model_id":%d,"protocol":"openai_chat","upstream_model_name":"openai/GPT-4o","rpm":60,"tpm":10000,"max_concurrency":2,"weight":10,"parameter_overrides":{"temperature":0,"private_marker":"channel-override-must-not-be-audited"}}`, provider.Id, unified.Id)
	status, created := call(http.MethodPost, "/api/admin/v1/channels", body)
	require.Equal(t, http.StatusOK, status, created)
	require.Equal(t, true, created["success"], created)
	data := created["data"].(map[string]any)
	assert.Equal(t, "pending", data["status"])
	assert.Equal(t, false, data["mapping_confirmed"])
	path := fmt.Sprintf("/api/admin/v1/channels/%v", data["id"])
	status, read := call(http.MethodGet, path, "")
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, data, read["data"])
	status, duplicate := call(http.MethodPost, "/api/admin/v1/channels", body)
	assert.Equal(t, http.StatusConflict, status, duplicate)
	status, _ = call(http.MethodPost, path+"/enable", "")
	assert.Equal(t, http.StatusBadRequest, status)
	status, confirmed := call(http.MethodPost, path+"/confirm", "")
	require.Equal(t, http.StatusOK, status, confirmed)
	assert.Equal(t, "disabled", confirmed["data"].(map[string]any)["status"])
	status, _ = call(http.MethodPost, path+"/enable", "")
	assert.Equal(t, http.StatusBadRequest, status, "no purchase price checker must deny activation")
	status, listed := call(http.MethodGet, fmt.Sprintf("/api/admin/v1/channels?provider_id=%d", provider.Id), "")
	require.Equal(t, http.StatusOK, status)
	assert.EqualValues(t, 1, listed["data"].(map[string]any)["total"])

	t.Run("capability probes are readable before channel activation", func(t *testing.T) {
		status, result := call(http.MethodGet, path+"/probes", "")
		require.Equal(t, http.StatusOK, status, result)
		assert.Empty(t, result["data"].(map[string]any)["results"])
		assert.NotEmpty(t, result["data"].(map[string]any)["capabilities"])
	})
	t.Run("probe input cannot override the destination or caller credentials", func(t *testing.T) {
		for _, body := range []string{`{"capability":"unknown"}`, `{"capability":"streaming","base_url":"http://127.0.0.1/"}`, `{"capability":"streaming","authorization":"secret"}`} {
			status, result := call(http.MethodPost, path+"/probes", body)
			assert.Equal(t, http.StatusBadRequest, status, result)
		}
		original := token
		token = createWalletTestAccessToken(t, root.Id, "channel:read")
		status, _ := call(http.MethodPost, path+"/probes", `{"capability":"streaming"}`)
		assert.Equal(t, http.StatusForbidden, status)
		token = original
	})
	t.Run("probes observe real upstream behavior using the channel protocol without changing declarations", func(t *testing.T) {
		fake := clock.UseFake(t, time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC))
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
			assert.NotContains(t, r.Header.Get("Authorization"), token)
			var sent map[string]any
			require.NoError(t, common.DecodeJson(r.Body, &sent))
			assert.Equal(t, "probe-model", sent["model"])
			fake.Advance(time.Second)
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/v1/responses" {
				_, _ = w.Write([]byte(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"probe\":\"ok\"}"}]}]}`))
				return
			}
			assert.Equal(t, "/v1/chat/completions", r.URL.Path)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"plain text, not structured JSON"},"finish_reason":"stop"}]}`))
		}))
		defer upstream.Close()
		require.NoError(t, db.Model(&provider).Update("base_url", upstream.URL).Error)
		for _, protocolName := range []string{"openai_chat", "openai_responses"} {
			status, created := call(http.MethodPost, "/api/admin/v1/channels", fmt.Sprintf(`{"provider_id":%d,"model_id":%d,"protocol":%q,"upstream_model_name":"probe-model"}`, provider.Id, unified.Id, protocolName))
			require.Equal(t, http.StatusOK, status, created)
			probePath := fmt.Sprintf("/api/admin/v1/channels/%v", created["data"].(map[string]any)["id"])
			status, result := call(http.MethodPost, probePath+"/probes", `{"capability":"structured_output"}`)
			require.Equal(t, http.StatusOK, status, result)
			observation := result["data"].(map[string]any)
			if protocolName == "openai_responses" {
				assert.Equal(t, "passed", observation["status"])
			} else {
				assert.Equal(t, "inconclusive", observation["status"], "2xx alone cannot verify a capability")
			}
			assert.Equal(t, protocolName, observation["protocol"])
			assert.EqualValues(t, fake.Now().Unix(), observation["finished_at"])
			_, persisted := call(http.MethodGet, probePath+"/probes", "")
			assert.Equal(t, observation, persisted["data"].(map[string]any)["results"].([]any)[0])
			_, listed := call(http.MethodGet, fmt.Sprintf("/api/admin/v1/channels?provider_id=%d", provider.Id), "")
			for _, item := range listed["data"].(map[string]any)["items"].([]any) {
				view := item.(map[string]any)
				if view["id"] == created["data"].(map[string]any)["id"] {
					assert.Equal(t, observation, view["latest_probe"])
				}
			}
			_, after := call(http.MethodGet, probePath, "")
			assert.Equal(t, created["data"], after["data"])
		}
	})
	t.Run("probe applies channel overrides and rejects an enlarged request budget", func(t *testing.T) {
		calls := 0
		expectedMessages := 1
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			var sent map[string]any
			require.NoError(t, common.DecodeJson(r.Body, &sent))
			assert.EqualValues(t, 0, sent["temperature"])
			assert.Len(t, sent["messages"], expectedMessages)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"probe\":\"ok\"}"}}]}`))
		}))
		defer upstream.Close()
		require.NoError(t, db.Model(&provider).Update("base_url", upstream.URL).Error)
		status, result := call(http.MethodPatch, path, `{"parameter_overrides":{"temperature":0}}`)
		require.Equal(t, http.StatusOK, status, result)
		status, result = call(http.MethodPost, path+"/probes", `{"capability":"structured_output"}`)
		require.Equal(t, http.StatusOK, status, result)
		assert.Equal(t, "passed", result["data"].(map[string]any)["status"])
		expectedMessages = 2
		require.NoError(t, db.Model(&provider).Update("param_override", `{"operations":[{"mode":"append","path":"messages","value":{"role":"user","content":"extra probe instruction"}}]}`).Error)
		status, result = call(http.MethodPost, path+"/probes", `{"capability":"structured_output"}`)
		require.Equal(t, http.StatusOK, status, result)
		require.NoError(t, db.Model(&provider).Update("param_override", nil).Error)
		for _, invalid := range []string{`{"max_tokens":9999999999}`, `{"operations":[{"mode":"delete","path":"max_tokens"}]}`, `{"operations":[{"mode":"delete","path":"max_tokens"},{"mode":"set","path":"metadata.max_tokens","value":1}]}`} {
			status, result = call(http.MethodPatch, path, `{"parameter_overrides":`+invalid+`}`)
			require.Equal(t, http.StatusOK, status, result)
			status, result = call(http.MethodPost, path+"/probes", `{"capability":"structured_output"}`)
			require.Equal(t, http.StatusOK, status, result)
			assert.Equal(t, "request_invalid", result["data"].(map[string]any)["reason"])
		}
		assert.Equal(t, 2, calls)
		_, _ = call(http.MethodPatch, path, `{"parameter_overrides":{}}`)
	})
	t.Run("each capability requires evidence and safe upstream failures remain private", func(t *testing.T) {
		imageFixture, err := os.ReadFile("../controller/probe-fixtures/red.png")
		require.NoError(t, err)
		audioFixture, err := os.ReadFile("../controller/probe-fixtures/seven.wav")
		require.NoError(t, err)
		cases := []struct {
			name, protocol, feature, contentType, body, want string
			status                                           int
		}{
			{"stream chunks", "openai_chat", "streaming", "text/event-stream", "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n", "passed", 200},
			{"stream without completion", "openai_chat", "streaming", "text/event-stream", "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n", "inconclusive", 200},
			{"tool call", "openai_chat", "tool_calling", "application/json", `{"choices":[{"message":{"tool_calls":[{"function":{"name":"probe_echo","arguments":"{\"value\":\"ok\"}"}}]}}]}`, "passed", 200},
			{"ignored tool", "openai_chat", "tool_calling", "application/json", `{"choices":[{"message":{"content":"ok"}}]}`, "inconclusive", 200},
			{"reasoning", "openai_chat", "reasoning", "application/json", `{"choices":[{"message":{"content":"7","reasoning_content":"Half of 14 is 7"}}]}`, "passed", 200},
			{"cache hit", "openai_chat", "caching", "application/json", `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens_details":{"cached_tokens":1024}}}`, "passed", 200},
			{"cache miss", "openai_chat", "caching", "application/json", `{"choices":[{"message":{"content":"ok"}}]}`, "inconclusive", 200},
			{"messages JSON", "anthropic_messages", "structured_output", "application/json", `{"content":[{"type":"text","text":"{\"probe\":\"ok\"}"}]}`, "passed", 200},
			{"gemini JSON", "gemini_generate", "structured_output", "application/json", `{"candidates":[{"content":{"parts":[{"text":"{\"probe\":\"ok\"}"}]}}]}`, "passed", 200},
			{"embedding input", "openai_embeddings", "input_text", "application/json", `{"data":[{"index":0,"embedding":[0.2,0.4]}]}`, "passed", 200},
			{"embedding dimensions", "openai_embeddings", "embedding_dimensions", "application/json", `{"data":[{"index":0,"embedding":[0.2,0.4]}]}`, "passed", 200},
			{"embedding wrong dimensions", "openai_embeddings", "embedding_dimensions", "application/json", `{"data":[{"index":0,"embedding":[0.2]}]}`, "inconclusive", 200},
			{"rerank input", "rerank", "input_text", "application/json", `{"results":[{"index":0,"relevance_score":1}]}`, "passed", 200},
			{"image input", "openai_chat", "input_image", "application/json", `{"choices":[{"message":{"content":"red"}}]}`, "passed", 200},
			{"ignored image input", "openai_chat", "input_image", "application/json", `{"choices":[{"message":{"content":"green"}}]}`, "inconclusive", 200},
			{"audio input", "openai_chat", "input_audio", "application/json", `{"choices":[{"message":{"content":"seven"}}]}`, "passed", 200},
			{"video input", "openai_chat", "input_video", "application/json", `{"choices":[{"message":{"content":"red"}}]}`, "passed", 200},
			{"PDF input", "openai_chat", "input_pdf", "application/json", `{"choices":[{"message":{"content":"GWP4831"}}]}`, "passed", 200},
			{"text output", "openai_chat", "output_text", "application/json", `{"choices":[{"message":{"content":"ok"}}]}`, "passed", 200},
			{"image output", "openai_images", "output_image", "application/json", fmt.Sprintf(`{"data":[{"b64_json":%q}]}`, base64.StdEncoding.EncodeToString(imageFixture)), "passed", 200},
			{"image response without payload", "openai_images", "output_image", "application/json", `{"data":[{"revised_prompt":"red"}]}`, "inconclusive", 200},
			{"audio output", "openai_audio_speech", "output_audio", "audio/wav", string(audioFixture), "passed", 200},
			{"audio transcription", "openai_audio_transcription", "input_audio", "application/json", `{"text":"seven"}`, "passed", 200},
			{"reported context boundary", "openai_chat", "context_length", "application/json", `{"choices":[{"message":{"content":"GWP4831"}}],"usage":{"prompt_tokens":128,"completion_tokens":4}}`, "passed", 200},
			{"bounded context sample", "openai_chat", "context_length", "application/json", `{"choices":[{"message":{"content":"GWP4831"}}]}`, "inconclusive", 200},
			{"actual output limit", "openai_chat", "max_output_tokens", "application/json", `{"choices":[{"message":{"content":"probe probe"},"finish_reason":"length"}],"usage":{"completion_tokens":64}}`, "passed", 200},
			{"image count", "openai_images", "max_images", "application/json", fmt.Sprintf(`{"data":[{"b64_json":%q}]}`, base64.StdEncoding.EncodeToString(imageFixture)), "passed", 200},
			{"one-second video input limit", "openai_chat", "max_duration_seconds", "application/json", `{"choices":[{"message":{"content":"red"}}]}`, "passed", 200},
			{"upstream error body", "openai_chat", "structured_output", "application/json", `{"error":{"message":"Provider secret raw-upstream-body-marker https://user:pass@example.invalid/?key=secret"}}`, "failed", 502},
		}
		for index, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				calls := 0
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
					w.Header().Set("Content-Type", tc.contentType)
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				}))
				defer upstream.Close()
				require.NoError(t, db.Model(&provider).Update("base_url", upstream.URL).Error)
				status, created := call(http.MethodPost, "/api/admin/v1/channels", fmt.Sprintf(`{"provider_id":%d,"model_id":%d,"protocol":%q,"upstream_model_name":"behavior-probe-%d"}`, provider.Id, unified.Id, tc.protocol, index))
				require.Equal(t, http.StatusOK, status, created)
				probePath := fmt.Sprintf("/api/admin/v1/channels/%v/probes", created["data"].(map[string]any)["id"])
				status, result := call(http.MethodPost, probePath, fmt.Sprintf(`{"capability":%q}`, tc.feature))
				require.Equal(t, http.StatusOK, status, result)
				assert.Equal(t, tc.want, result["data"].(map[string]any)["status"], result)
				expectedCalls := 1
				if tc.feature == "caching" {
					expectedCalls = 2
				}
				assert.Equal(t, expectedCalls, calls)
				encoded, err := common.Marshal(result)
				require.NoError(t, err)
				assert.NotContains(t, string(encoded), "raw-upstream-body-marker")
				assert.NotContains(t, string(encoded), provider.Key)
				assert.NotContains(t, string(encoded), upstream.URL)
			})
		}
	})
	t.Run("interrupted observations recover persistently in the list and details", func(t *testing.T) {
		fake := clock.UseFake(t, time.Date(2026, 11, 7, 20, 0, 0, 0, time.UTC))
		var channel model.Channel
		require.NoError(t, db.Where("provider_id = ?", provider.Id).Order("id").First(&channel).Error)
		result := model.ChannelProbeResult{ChannelID: channel.ID, Capability: "streaming", Protocol: channel.Protocol, StartedAt: fake.Now().Unix() - 96, DeadlineAt: fake.Now().Unix() - 1, Status: "running", ActorID: root.Id}
		require.NoError(t, db.Create(&result).Error)
		status, listed := call(http.MethodGet, fmt.Sprintf("/api/admin/v1/channels?provider_id=%d", provider.Id), "")
		require.Equal(t, http.StatusOK, status, listed)
		latest := listed["data"].(map[string]any)["items"].([]any)[0].(map[string]any)["latest_probe"].(map[string]any)
		assert.Equal(t, "inconclusive", latest["status"])
		assert.Equal(t, "probe_interrupted", latest["reason"])
		assert.EqualValues(t, fake.Now().Unix(), latest["finished_at"])
		fake.Advance(time.Hour)
		_, details := call(http.MethodGet, fmt.Sprintf("/api/admin/v1/channels/%d/probes", channel.ID), "")
		for _, item := range details["data"].(map[string]any)["results"].([]any) {
			observation := item.(map[string]any)
			if observation["capability"] == "streaming" {
				assert.Equal(t, latest, observation)
			}
		}
	})
	t.Run("task probes submit, poll and inspect actual media without changing the channel", func(t *testing.T) {
		previousRegistry := jsplugin.DefaultRegistry
		jsplugin.DefaultRegistry = jsplugin.NewRegistry()
		defer func() { jsplugin.DefaultRegistry = previousRegistry }()
		source := `export const meta={apiVersion:1,key:"probe",name:"Probe",version:"1.0.0",author:{name:"Test"},models:["task-probe-upstream"],fetchMode:"per_task",protocols:["openai_video"]};
export const protocols={openai_video:{decodeRequest(ctx){return {kind:"submit",model:ctx.model,requestBody:ctx.body.value}},render(ctx,task){return {id:task.task_id,status:"completed"}}}};
export function buildSubmitRequest(ctx){return {url:ctx.baseUrl+"/submit",method:"POST",headers:{Authorization:"Bearer "+ctx.apiKey},body:{model:ctx.upstreamModel,prompt:ctx.requestBody.prompt,seconds:4}}}
export function parseSubmitResponse(ctx,r){return {taskId:r.body.id,taskData:r.body}}
export function buildQueryRequest(ctx){return {url:ctx.baseUrl+"/poll/"+ctx.taskId,headers:{Authorization:"Bearer "+ctx.apiKey}}}
export function parseTaskResult(ctx,body){return {status:body.status,taskId:body.id,progress:"100%"}}
export function listArtifacts(task){return task.status==="SUCCESS"?[{key:"video",type:"video",mimeType:"video/mp4"}]:[]}
export function buildContentRequest(ctx){return {url:ctx.baseUrl+"/content/"+ctx.upstreamTaskId,headers:{Authorization:"Bearer "+ctx.apiKey}}}`
		_, err := jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "probe"})
		require.NoError(t, err)
		video, err := os.ReadFile("../controller/probe-fixtures/red.mp4")
		require.NoError(t, err)
		validMedia := true
		var cancelProbe context.CancelFunc
		counts := map[string]int{}
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counts[r.URL.Path]++
			assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
			switch r.URL.Path {
			case "/submit":
				var sent map[string]any
				require.NoError(t, common.DecodeJson(r.Body, &sent))
				assert.Equal(t, "task-probe-upstream", sent["model"])
				assert.EqualValues(t, 4, sent["seconds"])
				_, _ = w.Write([]byte(`{"id":"private-task-123","private_marker":"do-not-store-task-response"}`))
			case "/poll/private-task-123":
				if cancelProbe != nil {
					cancelProbe()
					<-r.Context().Done()
					return
				}
				_, _ = w.Write([]byte(`{"id":"private-task-123","status":"SUCCESS","private_marker":"do-not-store-task-response"}`))
			case "/content/private-task-123":
				if validMedia {
					w.Header().Set("Content-Type", "video/mp4")
					_, _ = w.Write(video)
				} else {
					_, _ = w.Write([]byte(`{"not":"a video"}`))
				}
			default:
				t.Errorf("unexpected probe path %s", r.URL.Path)
			}
		}))
		defer upstream.Close()
		require.NoError(t, db.Model(&provider).Update("base_url", upstream.URL).Error)
		originalToken := token
		token = createWalletTestAccessToken(t, root.Id, "channel:read", "channel:write", "channel:operate", "task_plugin:bind")
		defer func() { token = originalToken }()
		status, created := call(http.MethodPost, "/api/admin/v1/channels", fmt.Sprintf(`{"provider_id":%d,"model_id":%d,"protocol":"task:probe","upstream_model_name":"task-probe-upstream"}`, provider.Id, unified.Id))
		require.Equal(t, http.StatusOK, status, created)
		channelPath := fmt.Sprintf("/api/admin/v1/channels/%v", created["data"].(map[string]any)["id"])
		for _, expected := range []string{"passed", "inconclusive"} {
			status, result := call(http.MethodPost, channelPath+"/probes", `{"capability":"output_video"}`)
			require.Equal(t, http.StatusOK, status, result)
			assert.Equal(t, expected, result["data"].(map[string]any)["status"], result)
			encoded, err := common.Marshal(result)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), "do-not-store-task-response")
			assert.NotContains(t, string(encoded), "private-task-123")
			assert.NotContains(t, string(encoded), provider.Key)
			validMedia = false
		}
		assert.Equal(t, map[string]int{"/submit": 2, "/poll/private-task-123": 2, "/content/private-task-123": 2}, counts)
		probeCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cancelProbe = cancel
		cancelled := httptest.NewRequest(http.MethodPost, channelPath+"/probes", strings.NewReader(`{"capability":"output_video"}`)).WithContext(probeCtx)
		cancelled.Header.Set("Authorization", "Bearer "+token)
		cancelled.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, cancelled)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		var cancelledResult map[string]any
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &cancelledResult))
		assert.Equal(t, "failed", cancelledResult["data"].(map[string]any)["status"])
		_, latest := call(http.MethodGet, channelPath+"/probes", "")
		assert.Equal(t, cancelledResult["data"], latest["data"].(map[string]any)["results"].([]any)[0])
		_, after := call(http.MethodGet, channelPath, "")
		assert.Equal(t, created["data"], after["data"])
	})
	t.Run("one active probe per channel prevents duplicate upstream work", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		done := make(chan *httptest.ResponseRecorder, 1)
		var calls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				close(started)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"probe\":\"ok\"}"}}]}`))
		}))
		t.Cleanup(upstream.Close)
		t.Cleanup(func() {
			select {
			case <-release:
			default:
				close(release)
			}
		})
		require.NoError(t, db.Model(&provider).Update("base_url", upstream.URL).Error)
		request := httptest.NewRequest(http.MethodPost, path+"/probes", strings.NewReader(`{"capability":"structured_output"}`))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		go func() { recorder := httptest.NewRecorder(); engine.ServeHTTP(recorder, request); done <- recorder }()
		select {
		case <-started:
		case early := <-done:
			t.Fatalf("probe returned before calling the upstream: %d %s", early.Code, early.Body.String())
		}
		status, result := call(http.MethodPost, path+"/probes", `{"capability":"streaming"}`)
		assert.Equal(t, http.StatusConflict, status, result)
		close(release)
		completed := <-done
		assert.Equal(t, http.StatusOK, completed.Code, completed.Body.String())
		assert.EqualValues(t, 1, calls.Load())
	})
	t.Run("provider OAuth exchanges inherit cancellation without leaking credentials", func(t *testing.T) {
		private, err := rsa.GenerateKey(rand.Reader, 2048)
		require.NoError(t, err)
		der, err := x509.MarshalPKCS8PrivateKey(private)
		require.NoError(t, err)
		credentials, err := common.Marshal(map[string]string{"project_id": "probe-project", "client_email": "probe@example.invalid", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))})
		require.NoError(t, err)
		type cancellationMarker struct{}
		marker := cancellationMarker{}
		probeCtx, cancel := context.WithCancel(context.WithValue(context.Background(), marker, true))
		defer cancel()
		var bounded atomic.Bool
		var resourceCalls atomic.Int32
		resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			resourceCalls.Add(1)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		}))
		defer resource.Close()
		broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.NoError(t, r.ParseForm())
			assert.NotEmpty(t, r.Form.Get("assertion"))
			assert.Empty(t, r.Header.Get("Authorization"))
			cancel()
			<-r.Context().Done()
		}))
		defer broker.Close()
		brokerURL, err := url.Parse(broker.URL)
		require.NoError(t, err)
		service.InitHttpClient()
		client := service.GetHttpClient()
		original := client.Transport
		t.Cleanup(func() { client.Transport = original })
		transport := original
		if transport == nil {
			transport = http.DefaultTransport
		}
		client.Transport = channelProbeRoundTripper(func(request *http.Request) (*http.Response, error) {
			if request.URL.Host != "www.googleapis.com" || request.URL.Path != "/oauth2/v4/token" {
				return nil, errors.New("unexpected outbound destination")
			}
			if request.Context().Value(marker) != true || request.Context().Done() == nil {
				return nil, errors.New("token exchange lost request cancellation")
			}
			bounded.Store(true)
			copyRequest := request.Clone(request.Context())
			copyURL := *request.URL
			copyURL.Scheme = brokerURL.Scheme
			copyURL.Host = brokerURL.Host
			copyRequest.URL = &copyURL
			copyRequest.Host = brokerURL.Host
			return transport.RoundTrip(copyRequest)
		})
		account := model.Provider{Name: "OAuth probe Provider", Type: constant.ChannelTypeVertexAi, Key: string(credentials), BaseURL: common.GetPointer(resource.URL), Status: common.ChannelStatusEnabled}
		require.NoError(t, db.Create(&account).Error)
		status, created := call(http.MethodPost, "/api/admin/v1/channels", fmt.Sprintf(`{"provider_id":%d,"model_id":%d,"protocol":"openai_chat","upstream_model_name":"llama-3"}`, account.Id, unified.Id))
		require.Equal(t, http.StatusOK, status, created)
		probePath := fmt.Sprintf("/api/admin/v1/channels/%v/probes", created["data"].(map[string]any)["id"])
		request := httptest.NewRequest(http.MethodPost, probePath, strings.NewReader(`{"capability":"input_text"}`)).WithContext(probeCtx)
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, request)
		assert.ErrorIs(t, probeCtx.Err(), context.Canceled)
		require.True(t, bounded.Load(), recorder.Body.String())
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		var result map[string]any
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &result))
		assert.Equal(t, "failed", result["data"].(map[string]any)["status"])
		assert.Zero(t, resourceCalls.Load())
		assert.NotContains(t, recorder.Body.String(), string(credentials))
		assert.NotContains(t, recorder.Body.String(), "PRIVATE KEY")
	})
	t.Run("Gemini embedding probes use the production OpenAI-to-Gemini conversion", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "secret", r.Header.Get("x-goog-api-key"))
			assert.Empty(t, r.Header.Get("Authorization"))
			assert.True(t, strings.HasSuffix(r.URL.Path, ":batchEmbedContents"), r.URL.Path)
			var body map[string]any
			require.NoError(t, common.DecodeJson(r.Body, &body))
			requests := body["requests"].([]any)
			require.Len(t, requests, 1)
			assert.EqualValues(t, 2, requests[0].(map[string]any)["outputDimensionality"])
			_, _ = w.Write([]byte(`{"embeddings":[{"values":[0.2,0.4]}]}`))
		}))
		defer upstream.Close()
		account := model.Provider{Name: "Gemini probe Provider", Type: constant.ChannelTypeGemini, Key: "secret", BaseURL: common.GetPointer(upstream.URL), Status: common.ChannelStatusEnabled}
		require.NoError(t, db.Create(&account).Error)
		status, created := call(http.MethodPost, "/api/admin/v1/channels", fmt.Sprintf(`{"provider_id":%d,"model_id":%d,"protocol":"gemini_embed","upstream_model_name":"embed-gemini"}`, account.Id, unified.Id))
		require.Equal(t, http.StatusOK, status, created)
		probePath := fmt.Sprintf("/api/admin/v1/channels/%v/probes", created["data"].(map[string]any)["id"])
		status, result := call(http.MethodPost, probePath, `{"capability":"embedding_dimensions"}`)
		require.Equal(t, http.StatusOK, status, result)
		assert.Equal(t, "passed", result["data"].(map[string]any)["status"], result)
		assert.EqualValues(t, 2, result["data"].(map[string]any)["tested_value"])
	})
	t.Run("capability narrowing and model edits stay consistent", func(t *testing.T) {
		unified.Capabilities = capability.Set{Features: []capability.Feature{capability.Streaming, capability.ToolCalling}, InputModalities: []capability.Modality{capability.Text, capability.Image}, OutputModalities: []capability.Modality{capability.Text}, Limits: capability.Limits{ContextLength: common.GetPointer(int64(32000)), MaxOutputTokens: common.GetPointer(int64(4096)), MaxImages: common.GetPointer(int64(8)), MaxDurationSeconds: common.GetPointer(int64(60)), EmbeddingDimensions: common.GetPointer(int64(3072))}}
		require.NoError(t, unified.UpdateDeclaration())
		for _, invalid := range []string{
			`{"features":["reasoning"]}`, `{"input_modalities":["audio"]}`, `{"output_modalities":["image"]}`,
			`{"limits":{"context_length":32001}}`, `{"limits":{"max_output_tokens":4097}}`, `{"limits":{"max_images":9}}`, `{"limits":{"max_duration_seconds":61}}`, `{"limits":{"embedding_dimensions":3073}}`,
			`{"features":["unknown"]}`, `{"limits":{"max_images":0}}`,
		} {
			status, result := call(http.MethodPatch, path, `{"capabilities":`+invalid+`}`)
			assert.Equal(t, http.StatusBadRequest, status, result)
		}
		narrowed := `{"features":["streaming"],"input_modalities":["text"],"output_modalities":["text"],"limits":{"context_length":16000,"max_output_tokens":2048,"max_images":4,"max_duration_seconds":30,"embedding_dimensions":1024}}`
		status, result := call(http.MethodPatch, path, `{"capabilities":`+narrowed+`}`)
		require.Equal(t, http.StatusOK, status, result)
		model.SetChannelPurchasePriceChecker(configuredChannelPrice{})
		status, result = call(http.MethodPost, path+"/enable", "")
		require.Equal(t, http.StatusOK, status, result)
		assert.Equal(t, "enabled", result["data"].(map[string]any)["status"])
		// A Model change may not leave an enabled Channel with unsupported features.
		modelBody := fmt.Sprintf(`{"name":"gpt-4o","publisher_id":%d,"capabilities":{"features":[],"input_modalities":["text"],"output_modalities":["text"],"limits":{"context_length":8000}}}`, publisher.Id)
		status, result = call(http.MethodPut, fmt.Sprintf("/api/admin/v1/models/%d", unified.Id), modelBody)
		assert.Equal(t, http.StatusBadRequest, status, result)
		status, result = call(http.MethodPatch, path, `{"model_id":999999}`)
		assert.Equal(t, http.StatusNotFound, status, result)
		status, result = call(http.MethodPatch, path, `{"upstream_model_name":"openai/gpt-4o-new"}`)
		require.Equal(t, http.StatusOK, status, result)
		assert.Equal(t, "pending", result["data"].(map[string]any)["status"])
		assert.Equal(t, false, result["data"].(map[string]any)["mapping_confirmed"])
		status, _ = call(http.MethodPost, path+"/enable", "")
		assert.Equal(t, http.StatusBadRequest, status)
		status, _ = call(http.MethodPost, path+"/confirm", "")
		require.Equal(t, http.StatusOK, status)
		require.NoError(t, db.Model(&provider).Update("status", common.ChannelStatusManuallyDisabled).Error)
		status, _ = call(http.MethodPost, path+"/enable", "")
		assert.Equal(t, http.StatusBadRequest, status)
		require.NoError(t, db.Model(&provider).Update("status", common.ChannelStatusEnabled).Error)
		status, _ = call(http.MethodPost, path+"/enable", "")
		require.Equal(t, http.StatusOK, status)
		status, result = call(http.MethodPost, path+"/disable", "")
		require.Equal(t, http.StatusOK, status, result)
		assert.Equal(t, "disabled", result["data"].(map[string]any)["status"])
	})
	t.Run("referenced models cannot become metadata matching rules", func(t *testing.T) {
		legacyBody := fmt.Sprintf(`{"id":%d,"model_name":"gpt-4o","vendor_id":%d,"name_rule":1,"status":1,"sync_official":1}`, unified.Id, publisher.Id)
		_, result := call(http.MethodPut, "/api/models/", legacyBody)
		assert.Equal(t, false, result["success"], result)
		status, _ := call(http.MethodGet, fmt.Sprintf("/api/admin/v1/models/%d", unified.Id), "")
		assert.Equal(t, http.StatusOK, status)
	})
	t.Run("referenced models cannot be soft deleted", func(t *testing.T) {
		status, result := call(http.MethodDelete, fmt.Sprintf("/api/admin/v1/models/%d", unified.Id), "")
		assert.Equal(t, http.StatusConflict, status, result)
		status, _ = call(http.MethodGet, fmt.Sprintf("/api/admin/v1/models/%d", unified.Id), "")
		assert.Equal(t, http.StatusOK, status)
	})
	t.Run("suggestions ignore only casing and slash namespaces", func(t *testing.T) {
		status, result := call(http.MethodGet, "/api/admin/v1/channels/suggest-models?upstream_model_name=tenant/OPENAI/GPT-4O", "")
		require.Equal(t, http.StatusOK, status, result)
		assert.EqualValues(t, unified.Id, result["data"].(map[string]any)["selected_model_id"])
		_, result = call(http.MethodGet, "/api/admin/v1/channels/suggest-models?upstream_model_name=gpt-4o-q4", "")
		assert.Empty(t, result["data"].(map[string]any)["items"])
		variant := model.Model{ModelName: "vendor/GPT-4O", VendorID: publisher.Id, Status: 1}
		require.NoError(t, variant.Insert())
		_, result = call(http.MethodGet, "/api/admin/v1/channels/suggest-models?upstream_model_name=gpt-4o", "")
		assert.Nil(t, result["data"].(map[string]any)["selected_model_id"])
		assert.Len(t, result["data"].(map[string]any)["items"], 2)
	})
	t.Run("reject invalid limits and protocols", func(t *testing.T) {
		for _, invalid := range []string{`{"rpm":-1}`, `{"tpm":-1}`, `{"max_concurrency":-1}`, `{"weight":-1}`, `{"rpm":9223372036854775808}`, `{"weight":1.5}`, `{"parameter_overrides":[]}`, `{"parameter_overrides":null}`, `{"protocol":"openai"}`, `{"protocol":"task:missing"}`, `{"capabilities":{"features":["unknown"]}}`} {
			status, result := call(http.MethodPatch, path, invalid)
			assert.Equal(t, http.StatusBadRequest, status, result)
		}
	})
	t.Run("concurrent patches preserve fields omitted by the later body", func(t *testing.T) {
		reader, writer := io.Pipe()
		defer reader.Close()
		defer writer.Close()
		request := httptest.NewRequest(http.MethodPatch, path, reader)
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		done := make(chan struct{})
		go func() { defer close(done); engine.ServeHTTP(recorder, request) }()
		prefixRead := make(chan error, 1)
		go func() { _, err := writer.Write([]byte(`{`)); prefixRead <- err }()
		select {
		case err := <-prefixRead:
			require.NoError(t, err)
		case <-done:
			t.Fatalf("PATCH returned before reading its body: %d %s", recorder.Code, recorder.Body.String())
		}
		status, result := call(http.MethodPatch, path, `{"tpm":77}`)
		require.Equal(t, http.StatusOK, status, result)
		_, err := writer.Write([]byte(`"weight":2}`))
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		<-done
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		status, result = call(http.MethodGet, path, "")
		require.Equal(t, http.StatusOK, status, result)
		assert.EqualValues(t, 77, result["data"].(map[string]any)["tpm"])
		assert.EqualValues(t, 2, result["data"].(map[string]any)["weight"])
	})
	t.Run("task binding needs permission and unavailable plugins can be disabled", func(t *testing.T) {
		previousRegistry := jsplugin.DefaultRegistry
		jsplugin.DefaultRegistry = jsplugin.NewRegistry()
		defer func() { jsplugin.DefaultRegistry = previousRegistry }()
		source, err := plugins.Source("kling")
		require.NoError(t, err)
		_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "kling"})
		require.NoError(t, err)
		taskBody := fmt.Sprintf(`{"provider_id":%d,"model_id":%d,"protocol":"task:kling","upstream_model_name":"task-model"}`, provider.Id, unified.Id)
		status, result := call(http.MethodPost, "/api/admin/v1/channels", taskBody)
		assert.Equal(t, http.StatusForbidden, status, result)
		original := token
		token = createWalletTestAccessToken(t, root.Id, "channel:read", "channel:write", "channel:operate", "task_plugin:bind")
		defer func() { token = original }()
		status, result = call(http.MethodPost, "/api/admin/v1/channels", taskBody)
		require.Equal(t, http.StatusOK, status, result)
		taskPath := fmt.Sprintf("/api/admin/v1/channels/%v", result["data"].(map[string]any)["id"])
		status, _ = call(http.MethodPost, taskPath+"/confirm", "")
		require.Equal(t, http.StatusOK, status)
		status, _ = call(http.MethodPost, taskPath+"/enable", "")
		require.Equal(t, http.StatusOK, status)
		jsplugin.DefaultRegistry.SetEnabled(false)
		status, _ = call(http.MethodPost, taskPath+"/enable", "")
		assert.Equal(t, http.StatusBadRequest, status)
		status, result = call(http.MethodPost, taskPath+"/disable", "")
		require.Equal(t, http.StatusOK, status, result)
		assert.Equal(t, "disabled", result["data"].(map[string]any)["status"])
		status, _ = call(http.MethodDelete, taskPath, "")
		require.Equal(t, http.StatusOK, status)
	})
	t.Run("PAT scopes and Casbin guard every action", func(t *testing.T) {
		original := token
		token = createWalletTestAccessToken(t, root.Id, "channel:read")
		for _, action := range []struct{ method, path, body string }{{http.MethodPost, "/api/admin/v1/channels", body}, {http.MethodPatch, path, `{"weight":2}`}, {http.MethodDelete, path, ""}, {http.MethodPost, path + "/confirm", ""}, {http.MethodPost, path + "/enable", ""}, {http.MethodPost, path + "/disable", ""}, {http.MethodPost, path + "/probes", `{"capability":"streaming"}`}} {
			status, _ := call(action.method, action.path, action.body)
			assert.Equal(t, http.StatusForbidden, status, action)
		}
		customer := model.User{Username: "channel-customer", Password: "placeholder", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: "channel-customer"}
		require.NoError(t, db.Create(&customer).Error)
		token = createWalletTestAccessToken(t, customer.Id, "channel:read", "channel:write", "channel:operate")
		deniedProbeStatus, _ := call(http.MethodPost, path+"/probes", `{"capability":"streaming"}`)
		assert.Equal(t, http.StatusForbidden, deniedProbeStatus)
		status, _ := call(http.MethodGet, path, "")
		assert.Equal(t, http.StatusForbidden, status)
		token = "invalid"
		status, _ = call(http.MethodGet, path, "")
		assert.Equal(t, http.StatusUnauthorized, status)
		expired := createWalletTestAccessToken(t, root.Id, "channel:read")
		require.NoError(t, db.Model(&model.UserAccessToken{}).Where("token_hash = ?", model.AccessTokenFingerprint(expired)).Update("expires_at", time.Now().Unix()-1).Error)
		token = expired
		status, _ = call(http.MethodGet, path, "")
		assert.Equal(t, http.StatusUnauthorized, status)
		status, _ = call(http.MethodPost, path+"/probes", `{"capability":"streaming"}`)
		assert.Equal(t, http.StatusUnauthorized, status)
		bundle, err := service.CreateLoginSession(root.Id, "password", "127.0.0.1", "channel-api-test")
		require.NoError(t, err)
		token = bundle.AccessToken
		status, _ = call(http.MethodGet, path, "")
		assert.Equal(t, http.StatusOK, status, "admin browser-session access token")
		_, err = model.RevokeUserSession(root.Id, bundle.Session.SID, "test-revocation")
		require.NoError(t, err)
		status, _ = call(http.MethodGet, path, "")
		assert.Equal(t, http.StatusUnauthorized, status, "revoked session cannot access Channel API")
		status, _ = call(http.MethodPost, path+"/probes", `{"capability":"streaming"}`)
		assert.Equal(t, http.StatusUnauthorized, status)
		token = original
	})
	status, _ = call(http.MethodDelete, path, "")
	assert.Equal(t, http.StatusOK, status)
	status, _ = call(http.MethodGet, path, "")
	assert.Equal(t, http.StatusNotFound, status)
	status, walletAfter := call(http.MethodGet, walletPath, "")
	require.Equal(t, http.StatusOK, status, walletAfter)
	assert.Equal(t, walletBefore, walletAfter, "administrative probes must not debit the initiating administrator")
	_, entries := call(http.MethodGet, walletPath+"/entries", "")
	assert.EqualValues(t, 1, entries["data"].(map[string]any)["total"], "only the fixture top-up is recorded")
	audits, _, err := model.GetAuditLogs(model.AuditLogFilter{UserId: root.Id, Category: model.AuditCategoryOperation}, 0, 200, common.RoleRootUser)
	require.NoError(t, err)
	actions := map[string]bool{}
	for _, audit := range audits {
		actions[audit.Action] = true
		encoded, err := common.Marshal(audit)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), "channel-override-must-not-be-audited")
		assert.NotContains(t, string(encoded), token)
		assert.NotContains(t, string(encoded), provider.Key)
	}
	for _, action := range []string{"channel.create", "channel.update", "channel.confirm", "channel.enable", "channel.disable", "channel.delete", "channel.probe"} {
		assert.True(t, actions[action], action)
	}
}
