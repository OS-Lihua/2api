package relay

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/protocol"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	openaichannel "github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	relaytypes "github.com/QuantumNous/new-api/relaykit/types"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelProtocolSelectsConversionIndependentlyOfProvider(t *testing.T) {
	for _, tc := range []struct {
		name        string
		apiType     int
		channelType int
		upstream    protocol.Type
		path        string
		authHeader  string
		auth        string
		response    string
		bodyField   string
	}{
		{"deepseek chat", constant.APITypeDeepSeek, constant.ChannelTypeDeepSeek, protocol.OpenAIChat, "/v1/chat/completions", "Authorization", "Bearer provider-key", `{"id":"chat_1","object":"chat.completion","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`, "messages"},
		{"deepseek messages", constant.APITypeDeepSeek, constant.ChannelTypeDeepSeek, protocol.AnthropicMessages, "/anthropic/v1/messages", "Authorization", "Bearer provider-key", `{"id":"msg_1","type":"message","role":"assistant","model":"test-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`, "messages"},
		{"deepseek responses", constant.APITypeDeepSeek, constant.ChannelTypeDeepSeek, protocol.OpenAIResponses, "/responses", "Authorization", "Bearer provider-key", `{"id":"resp_1","object":"response","status":"completed","model":"test-model","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`, "input"},
		{"openai messages", constant.APITypeOpenAI, constant.ChannelTypeOpenAI, protocol.AnthropicMessages, "/v1/messages", "Authorization", "Bearer provider-key", `{"id":"msg_1","type":"message","role":"assistant","model":"test-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`, "messages"},
		{"openai gemini", constant.APITypeOpenAI, constant.ChannelTypeOpenAI, protocol.GeminiGenerate, "/v1beta/models/test-model:generateContent", "Authorization", "Bearer provider-key", `{"candidates":[{"content":{"parts":[{"text":"ok"}],"role":"model"},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`, "contents"},
		{"azure chat", constant.APITypeOpenAI, constant.ChannelTypeAzure, protocol.OpenAIChat, "/openai/deployments/test-model/chat/completions", "api-key", "provider-key", `{"id":"chat_1","object":"chat.completion","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`, "messages"},
		{"custom header", constant.APITypeAdvancedCustom, constant.ChannelTypeAdvancedCustom, protocol.OpenAIChat, "/merchant/chat", "X-Provider-Secret", "provider-key", `{"id":"chat_1","object":"chat.completion","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`, "messages"},
		{"openai chat", constant.APITypeOpenAI, constant.ChannelTypeOpenAI, protocol.OpenAIChat, "/v1/chat/completions", "Authorization", "Bearer provider-key", `{"id":"chat_1","object":"chat.completion","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`, "messages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captured := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, tc.path, r.URL.Path)
				assert.Equal(t, tc.auth, r.Header.Get(tc.authHeader))
				if tc.channelType == constant.ChannelTypeAzure {
					assert.Equal(t, constant.AzureDefaultAPIVersion, r.URL.Query().Get("api-version"))
				}
				assert.Empty(t, r.Header.Get("Cookie"))
				body, err := io.ReadAll(r.Body)
				assert.NoError(t, err)
				captured <- body
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.response)
			}))
			defer server.Close()
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
			c.Request.Header.Set("Content-Type", "application/json")
			info := &relaycommon.RelayInfo{
				RelayMode:              relayconstant.RelayModeChatCompletions,
				RelayFormat:            relaytypes.RelayFormatOpenAI,
				OriginModelName:        "test-model",
				RequestURLPath:         "/v1/chat/completions",
				RequestConversionChain: []relaytypes.RelayFormat{relaytypes.RelayFormatOpenAI},
				ProviderMeta:           &relaycommon.ProviderMeta{ChannelType: tc.channelType, ApiType: tc.apiType, ChannelBaseUrl: server.URL, ApiKey: "provider-key", UpstreamModelName: "test-model"},
			}
			if tc.apiType == constant.APITypeAdvancedCustom {
				info.ChannelOtherSettings.AdvancedCustom = &dto.AdvancedCustomConfig{Routes: []dto.AdvancedCustomRoute{{
					IncomingPath: "/v1/chat/completions", UpstreamPath: "/merchant/chat", Converter: relayconvert.ConverterNone,
					Auth: &dto.AdvancedCustomRouteAuth{Type: dto.AdvancedCustomAuthTypeHeader, Name: "X-Provider-Secret", Value: "{api_key}"},
				}}}
			}
			adaptor, err := GetChannelAdaptor(tc.apiType, tc.upstream)
			require.NoError(t, err)
			zero := 0.0
			request, err := adaptor.ConvertRequest(c, info, &dto.GeneralOpenAIRequest{Model: "test-model", Messages: []dto.Message{{Role: "user", Content: "hello"}}, Temperature: &zero})
			require.NoError(t, err)
			body, err := common.Marshal(request)
			require.NoError(t, err)
			response, err := adaptor.DoRequest(c, info, strings.NewReader(string(body)))
			require.NoError(t, err)
			usage, apiErr := adaptor.DoResponse(c, response.(*http.Response), info)
			require.Nil(t, apiErr)
			require.IsType(t, &dto.Usage{}, usage)
			assert.Equal(t, 3, usage.(*dto.Usage).PromptTokens)
			assert.Equal(t, 2, usage.(*dto.Usage).CompletionTokens)
			assert.Contains(t, recorder.Body.String(), "ok")
			var upstreamBody map[string]any
			require.NoError(t, common.Unmarshal(<-captured, &upstreamBody))
			assert.Contains(t, upstreamBody, tc.bodyField)
			if tc.upstream == protocol.GeminiGenerate {
				require.Contains(t, upstreamBody, "generationConfig")
				assert.Equal(t, float64(0), upstreamBody["generationConfig"].(map[string]any)["temperature"])
			} else {
				assert.Equal(t, float64(0), upstreamBody["temperature"])
			}
		})
	}
}

func TestChannelProtocolPreservesReportedZeroUsage(t *testing.T) {
	originalTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = originalTimeout })
	for _, tc := range []struct {
		name     string
		protocol protocol.Type
		stream   bool
		response string
	}{
		{"chat JSON", protocol.OpenAIChat, false, `{"id":"chat_1","object":"chat.completion","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`},
		{"responses JSON", protocol.OpenAIResponses, false, `{"id":"resp_1","object":"response","status":"completed","model":"test-model","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}`},
		{"gemini JSON", protocol.GeminiGenerate, false, `{"candidates":[{"content":{"parts":[{"text":"ok"}],"role":"model"},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":0,"candidatesTokenCount":0,"totalTokenCount":0}}`},
		{"gemini SSE", protocol.GeminiGenerate, true, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}],\"role\":\"model\"},\"finishReason\":\"STOP\",\"index\":0}],\"usageMetadata\":{\"promptTokenCount\":0,\"candidatesTokenCount\":0,\"totalTokenCount\":0}}\n\n"},
		{"chat SSE", protocol.OpenAIChat, true, "data: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\",\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\",\"model\":\"test-model\",\"choices\":[],\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0,\"total_tokens\":0}}\n\ndata: [DONE]\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				_, _ = io.WriteString(w, tc.response)
			}))
			defer server.Close()
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
			c.Request.Header.Set("Content-Type", "application/json")
			info := &relaycommon.RelayInfo{
				RelayMode: relayconstant.RelayModeChatCompletions, RelayFormat: relaytypes.RelayFormatOpenAI,
				OriginModelName: "test-model", RequestURLPath: "/v1/chat/completions",
				RequestConversionChain: []relaytypes.RelayFormat{relaytypes.RelayFormatOpenAI},
				ProviderMeta:           &relaycommon.ProviderMeta{ChannelType: constant.ChannelTypeOpenAI, ApiType: constant.APITypeOpenAI, ChannelBaseUrl: server.URL, ApiKey: "provider-key", UpstreamModelName: "test-model"},
			}
			info.IsStream = tc.stream
			info.ShouldIncludeUsage = true
			info.SetEstimatePromptTokens(100)
			adaptor, err := GetChannelAdaptor(constant.APITypeOpenAI, tc.protocol)
			require.NoError(t, err)
			request, err := adaptor.ConvertRequest(c, info, &dto.GeneralOpenAIRequest{Model: "test-model", Stream: &tc.stream, Messages: []dto.Message{{Role: "user", Content: "hello"}}})
			require.NoError(t, err)
			encoded, err := common.Marshal(request)
			require.NoError(t, err)
			response, err := adaptor.DoRequest(c, info, strings.NewReader(string(encoded)))
			require.NoError(t, err)
			usage, apiErr := adaptor.DoResponse(c, response.(*http.Response), info)
			require.Nil(t, apiErr)
			require.IsType(t, &dto.Usage{}, usage)
			assert.Zero(t, usage.(*dto.Usage).PromptTokens)
			assert.Zero(t, usage.(*dto.Usage).CompletionTokens)
			assert.Zero(t, usage.(*dto.Usage).TotalTokens)
			if tc.stream {
				assert.Contains(t, recorder.Body.String(), `"total_tokens":0`)
			} else {
				var delivered dto.OpenAITextResponse
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &delivered))
				assert.Zero(t, delivered.Usage.TotalTokens)
			}
		})
	}
}

func TestChannelProtocolDoesNotExposeDestinationSecrets(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	info := &relaycommon.RelayInfo{ProviderMeta: &relaycommon.ProviderMeta{
		ChannelType: constant.ChannelTypeOpenAI, ChannelBaseUrl: "http://[invalid/?api_key=key-secret&session=url-secret",
		ApiKey: "key-secret", UpstreamModelName: "test-model",
	}}
	adaptor, err := GetChannelAdaptor(constant.APITypeOpenAI, protocol.OpenAIChat)
	require.NoError(t, err)
	_, err = adaptor.DoRequest(c, info, strings.NewReader("{}"))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "key-secret")
	assert.NotContains(t, err.Error(), "url-secret")
}

func TestChannelProtocolEstimatesMissingNovaUsageFromGeneratedContent(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	info := &relaycommon.RelayInfo{
		RelayFormat: relaytypes.RelayFormatOpenAI, RelayMode: relayconstant.RelayModeChatCompletions,
		ProviderMeta: &relaycommon.ProviderMeta{ChannelType: constant.ChannelTypeAws, UpstreamModelName: "amazon.nova-lite-v1:0"},
	}
	info.SetEstimatePromptTokens(11)
	adaptor, err := GetChannelAdaptor(constant.APITypeAws, protocol.OpenAIChat)
	require.NoError(t, err)
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"output":{"message":{"content":[{"text":"ok"}]}}}`))}
	usage, apiErr := adaptor.DoResponse(c, response, info)
	require.Nil(t, apiErr)
	require.IsType(t, &dto.Usage{}, usage)
	assert.Equal(t, 11, usage.(*dto.Usage).PromptTokens)
	assert.Positive(t, usage.(*dto.Usage).CompletionTokens)
	var delivered dto.OpenAITextResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &delivered))
	assert.Equal(t, 11, delivered.Usage.PromptTokens)
}

func TestChannelProtocolServesNonTextProviders(t *testing.T) {
	imageN := uint(1)
	for _, tc := range []struct {
		name        string
		apiType     int
		channelType int
		protocol    protocol.Type
		mode        int
		format      relaytypes.RelayFormat
		model       string
		path        string
		request     any
		response    string
	}{
		{"jina embeddings", constant.APITypeJina, constant.ChannelTypeJina, protocol.OpenAIEmbeddings, relayconstant.RelayModeEmbeddings, relaytypes.RelayFormatEmbedding, "jina-embeddings-v3", "/v1/embeddings", dto.EmbeddingRequest{Model: "jina-embeddings-v3", Input: "hello"}, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"model":"jina-embeddings-v3","usage":{"prompt_tokens":3,"total_tokens":3}}`},
		{"replicate images", constant.APITypeReplicate, constant.ChannelTypeReplicate, protocol.OpenAIImages, relayconstant.RelayModeImagesGenerations, relaytypes.RelayFormatOpenAIImage, "black-forest-labs/flux-schnell", "/v1/models/black-forest-labs/flux-schnell/predictions", dto.ImageRequest{Model: "black-forest-labs/flux-schnell", Prompt: "a tree", N: &imageN}, `{"status":"succeeded","output":["https://example.test/image.png"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, tc.path, r.URL.Path)
				assert.Equal(t, "Bearer provider-key", r.Header.Get("Authorization"))
				var body map[string]any
				assert.NoError(t, common.DecodeJson(r.Body, &body))
				if tc.protocol == protocol.OpenAIEmbeddings {
					assert.Equal(t, "hello", body["input"])
				} else {
					assert.Equal(t, map[string]any{"prompt": "a tree", "num_outputs": float64(1)}, body["input"])
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.response)
			}))
			defer server.Close()
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader("{}"))
			c.Request.Header.Set("Content-Type", "application/json")
			info := &relaycommon.RelayInfo{
				RelayMode: tc.mode, RelayFormat: tc.format, OriginModelName: tc.model, RequestURLPath: tc.path,
				ProviderMeta: &relaycommon.ProviderMeta{ChannelType: tc.channelType, ApiType: tc.apiType, ChannelBaseUrl: server.URL, ApiKey: "provider-key", UpstreamModelName: tc.model},
			}
			adaptor, err := GetChannelAdaptor(tc.apiType, tc.protocol)
			require.NoError(t, err)
			request, err := adaptor.ConvertRequest(c, info, tc.request)
			require.NoError(t, err)
			encoded, err := common.Marshal(request)
			require.NoError(t, err)
			response, err := adaptor.DoRequest(c, info, strings.NewReader(string(encoded)))
			require.NoError(t, err)
			usage, apiErr := adaptor.DoResponse(c, response.(*http.Response), info)
			require.Nil(t, apiErr)
			require.IsType(t, &dto.Usage{}, usage)
			if tc.protocol == protocol.OpenAIEmbeddings {
				assert.Equal(t, 3, usage.(*dto.Usage).PromptTokens)
				assert.Contains(t, recorder.Body.String(), "embedding")
			} else {
				assert.Contains(t, recorder.Body.String(), "https://example.test/image.png")
			}
		})
	}
}

func TestChannelProtocolRetainsSparkAPIVersion(t *testing.T) {
	provider := GetProviderAdapter(constant.APITypeXunfei)
	require.NotNil(t, provider)
	destination, err := provider.RequestURL(&relaychannel.ProviderRequest{
		Protocol: protocol.OpenAIChat, RequestURLPath: "/v1/chat/completions",
		ClientRequestURLPath: "/v1/chat/completions?api-version=v4.0",
		ApiKey:               "appid|test-key|test-secret", UpstreamModelName: "SparkDesk",
	})
	require.NoError(t, err)
	parsed, err := url.Parse(destination)
	require.NoError(t, err)
	assert.Equal(t, "/v4.0/chat", parsed.Path)
}

func TestChannelProtocolPreservesDeepSeekReasoningRepairs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/anthropic/v1/messages", r.URL.Path)
		var body map[string]any
		assert.NoError(t, common.DecodeJson(r.Body, &body))
		assert.Equal(t, float64(64), body["max_tokens"])
		assert.Equal(t, 0.7, body["temperature"])
		assert.Equal(t, map[string]any{"type": "enabled"}, body["thinking"])
		assert.Equal(t, map[string]any{"effort": "low"}, body["output_config"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"deepseek-flash","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	defer server.Close()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	c.Request.Header.Set("Content-Type", "application/json")
	maxTokens := uint(64)
	temperature := 0.7
	original := &dto.ClaudeRequest{Model: "deepseek-flash@effort:low", MaxTokens: &maxTokens, Temperature: &temperature, Messages: []dto.ClaudeMessage{{Role: "user", Content: "hello"}}}
	info := &relaycommon.RelayInfo{
		Request: original, RelayMode: relayconstant.RelayModeChatCompletions, RelayFormat: relaytypes.RelayFormatClaude,
		OriginModelName: original.Model, RequestURLPath: "/v1/messages", RequestConversionChain: []relaytypes.RelayFormat{relaytypes.RelayFormatClaude},
		ProviderMeta: &relaycommon.ProviderMeta{ChannelType: constant.ChannelTypeDeepSeek, ApiType: constant.APITypeDeepSeek, ChannelBaseUrl: server.URL, ApiKey: "provider-key", UpstreamModelName: original.Model},
	}
	outbound, err := common.DeepCopy(original)
	require.NoError(t, err)
	require.NoError(t, helper.ApplyReasoningModelSuffix(c, info, outbound))
	adaptor, err := GetChannelAdaptor(constant.APITypeDeepSeek, protocol.AnthropicMessages)
	require.NoError(t, err)
	request, err := adaptor.ConvertRequest(c, info, outbound)
	require.NoError(t, err)
	encoded, err := common.Marshal(request)
	require.NoError(t, err)
	response, err := adaptor.DoRequest(c, info, strings.NewReader(string(encoded)))
	require.NoError(t, err)
	usage, apiErr := adaptor.DoResponse(c, response.(*http.Response), info)
	require.Nil(t, apiErr)
	assert.Equal(t, 3, usage.(*dto.Usage).PromptTokens)
	assert.Contains(t, recorder.Body.String(), "ok")
}

func TestChannelProtocolVolcengineSpeechClosesWebSocket(t *testing.T) {
	for _, abort := range []bool{false, true} {
		name := "complete"
		if abort {
			name = "client abort"
		}
		t.Run(name, func(t *testing.T) {
			closed := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(closed)
				assert.Equal(t, "/v1/audio/speech", r.URL.Path)
				assert.Equal(t, "Bearer;test-token", r.Header.Get("Authorization"))
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if !assert.NoError(t, err) {
					return
				}
				defer conn.Close()
				messageType, frame, err := conn.ReadMessage()
				if !assert.NoError(t, err) {
					return
				}
				assert.Equal(t, websocket.BinaryMessage, messageType)
				if !assert.GreaterOrEqual(t, len(frame), 8) {
					return
				}
				assert.Equal(t, []byte{0x11, 0x10, 0x10, 0}, frame[:4])
				var request struct {
					App struct {
						AppID string `json:"appid"`
						Token string `json:"token"`
					} `json:"app"`
					Request struct {
						Text string `json:"text"`
					} `json:"request"`
				}
				assert.NoError(t, common.Unmarshal(frame[8:], &request))
				assert.Equal(t, "app-test", request.App.AppID)
				assert.Equal(t, "test-token", request.App.Token)
				assert.Equal(t, "hello", request.Request.Text)
				// Audio-only server frame: v1 header, signed sequence, payload size, audio.
				audio := []byte{0x11, 0xb3, 0, 0, 0xff, 0xff, 0xff, 0xff, 0, 0, 0, 3, 'a', 'b', 'c'}
				if abort {
					audio[1] = 0xb1
					audio[4], audio[5], audio[6], audio[7] = 0, 0, 0, 1
				}
				if !assert.NoError(t, conn.WriteMessage(websocket.BinaryMessage, audio)) {
					return
				}
				_, _, _ = conn.ReadMessage()
			}))
			defer server.Close()
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader("{}"))
			c.Request.Header.Set("Content-Type", "application/json")
			info := &relaycommon.RelayInfo{RelayMode: relayconstant.RelayModeAudioSpeech, RelayFormat: relaytypes.RelayFormatOpenAIAudio,
				OriginModelName: "tts-1", RequestURLPath: "/v1/audio/speech", ProviderMeta: &relaycommon.ProviderMeta{ChannelType: constant.ChannelTypeVolcEngine, ApiType: constant.APITypeVolcEngine, ChannelBaseUrl: strings.Replace(server.URL, "http://", "ws://", 1), ApiKey: "app-test|test-token", UpstreamModelName: "tts-1"}}
			info.SetEstimatePromptTokens(11)
			adaptor, err := GetChannelAdaptor(constant.APITypeVolcEngine, protocol.OpenAIAudioSpeech)
			require.NoError(t, err)
			request, err := adaptor.ConvertRequest(c, info, dto.AudioRequest{Model: "tts-1", Input: "hello", Voice: "alloy", ResponseFormat: "mp3"})
			require.NoError(t, err)
			var encoded []byte
			if reader, ok := request.(io.Reader); ok {
				encoded, err = io.ReadAll(reader)
			} else {
				encoded, err = common.Marshal(request)
			}
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), "test-token")
			response, err := adaptor.DoRequest(c, info, strings.NewReader(string(encoded)))
			require.NoError(t, err)
			upstream := response.(*http.Response)
			if abort {
				audio := make([]byte, 3)
				_, err = io.ReadFull(upstream.Body, audio)
				require.NoError(t, err)
				assert.Equal(t, "abc", string(audio))
				require.NoError(t, upstream.Body.Close())
			} else {
				usage, apiErr := adaptor.DoResponse(c, upstream, info)
				require.Nil(t, apiErr)
				assert.Equal(t, "abc", recorder.Body.String())
				assert.Equal(t, "audio/mpeg", recorder.Header().Get("Content-Type"))
				assert.Equal(t, 11, usage.(*dto.Usage).PromptTokens)
			}
			select {
			case <-closed:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream WebSocket remained open after response close")
			}
		})
	}
}

func TestChannelProtocolTranscriptionPreservesMultipartAndUsage(t *testing.T) {
	for _, tc := range []struct {
		format, response string
		promptTokens     int
	}{
		{"json", `{"text":"hello","usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`, 0},
		{"text", "hello", 11},
	} {
		t.Run(tc.format, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/v1/audio/transcriptions", r.URL.Path)
				assert.Equal(t, "Bearer provider-key", r.Header.Get("Authorization"))
				if !assert.NoError(t, r.ParseMultipartForm(1<<20)) {
					return
				}
				defer r.MultipartForm.RemoveAll()
				assert.Equal(t, "whisper-1", r.FormValue("model"))
				assert.Equal(t, "0", r.FormValue("temperature"))
				assert.Equal(t, tc.format, r.FormValue("response_format"))
				file, header, err := r.FormFile("file")
				if !assert.NoError(t, err) {
					return
				}
				defer file.Close()
				assert.Equal(t, "voice.wav", header.Filename)
				payload, err := io.ReadAll(file)
				assert.NoError(t, err)
				assert.Equal(t, "voice-bytes", string(payload))
				w.Header().Set("Content-Type", "text/plain")
				if tc.format == "json" {
					w.Header().Set("Content-Type", "application/json")
				}
				_, _ = io.WriteString(w, tc.response)
			}))
			defer server.Close()
			var incoming bytes.Buffer
			writer := multipart.NewWriter(&incoming)
			require.NoError(t, writer.WriteField("model", "whisper-1"))
			require.NoError(t, writer.WriteField("temperature", "0"))
			require.NoError(t, writer.WriteField("response_format", tc.format))
			part, err := writer.CreateFormFile("file", "voice.wav")
			require.NoError(t, err)
			_, err = io.WriteString(part, "voice-bytes")
			require.NoError(t, err)
			require.NoError(t, writer.Close())
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &incoming)
			c.Request.Header.Set("Content-Type", writer.FormDataContentType())
			t.Cleanup(func() {
				if c.Request.MultipartForm != nil {
					_ = c.Request.MultipartForm.RemoveAll()
				}
			})
			info := &relaycommon.RelayInfo{RelayMode: relayconstant.RelayModeAudioTranscription, RelayFormat: relaytypes.RelayFormatOpenAIAudio,
				OriginModelName: "whisper-1", RequestURLPath: "/v1/audio/transcriptions", ProviderMeta: &relaycommon.ProviderMeta{ChannelType: constant.ChannelTypeOpenAI, ApiType: constant.APITypeOpenAI, ChannelBaseUrl: server.URL, ApiKey: "provider-key", UpstreamModelName: "whisper-1"}}
			info.SetEstimatePromptTokens(11)
			adaptor, err := GetChannelAdaptor(constant.APITypeOpenAI, protocol.OpenAIAudioTranscription)
			require.NoError(t, err)
			converted, err := adaptor.ConvertRequest(c, info, dto.AudioRequest{Model: "whisper-1", ResponseFormat: tc.format})
			require.NoError(t, err)
			body, ok := converted.(io.Reader)
			require.True(t, ok, "multipart conversion must return an encoded body")
			response, err := adaptor.DoRequest(c, info, body)
			require.NoError(t, err)
			usage, apiErr := adaptor.DoResponse(c, response.(*http.Response), info)
			require.Nil(t, apiErr)
			assert.Equal(t, tc.promptTokens, usage.(*dto.Usage).PromptTokens)
			assert.Equal(t, tc.response, recorder.Body.String())
		})
	}
}

func TestChannelProtocolGeminiEmbeddingSelectsSingleOrBatch(t *testing.T) {
	zeroDimensions := 0
	for _, tc := range []struct {
		name, action, wantBody, response string
		format                           relaytypes.RelayFormat
		request                          any
	}{
		{"OpenAI batch with explicit zero", "batchEmbedContents", `{"requests":[{"model":"models/dense-v1","content":{"parts":[{"text":"hello"}]},"outputDimensionality":0}]}`, `{"embeddings":[{"values":[0.1,0.2]}],"usageMetadata":{"promptTokenCount":0}}`, relaytypes.RelayFormatEmbedding, dto.EmbeddingRequest{Model: "dense-v1", Input: "hello", Dimensions: &zeroDimensions}},
		{"native single", "embedContent", `{"model":"models/dense-v1","content":{"parts":[{"text":"hello"}]},"outputDimensionality":4}`, `{"embedding":{"values":[0.1,0.2]},"usageMetadata":{"promptTokenCount":0}}`, relaytypes.RelayFormatGemini, &dto.GeminiEmbeddingRequest{Content: dto.GeminiChatContent{Parts: []dto.GeminiPart{{Text: "hello"}}}, OutputDimensionality: 4}},
		{"native batch", "batchEmbedContents", `{"requests":[{"model":"models/dense-v1","content":{"parts":[{"text":"hello"}]},"outputDimensionality":4}]}`, `{"embeddings":[{"values":[0.1,0.2]}],"usageMetadata":{"promptTokenCount":0}}`, relaytypes.RelayFormatGemini, &dto.GeminiBatchEmbeddingRequest{Requests: []*dto.GeminiEmbeddingRequest{{Content: dto.GeminiChatContent{Parts: []dto.GeminiPart{{Text: "hello"}}}, OutputDimensionality: 4}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/v1beta/models/dense-v1:"+tc.action, r.URL.Path)
				assert.Equal(t, "provider-key", r.Header.Get("x-goog-api-key"))
				body, err := io.ReadAll(r.Body)
				assert.NoError(t, err)
				assert.JSONEq(t, tc.wantBody, string(body))
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.response)
			}))
			defer server.Close()
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader("{}"))
			c.Request.Header.Set("Content-Type", "application/json")
			info := &relaycommon.RelayInfo{RelayMode: relayconstant.RelayModeEmbeddings, RelayFormat: tc.format, OriginModelName: "dense-v1", RequestURLPath: "/v1/embeddings",
				ProviderMeta: &relaycommon.ProviderMeta{ChannelType: constant.ChannelTypeGemini, ApiType: constant.APITypeGemini, ChannelBaseUrl: server.URL, ApiKey: "provider-key", UpstreamModelName: "dense-v1"}}
			info.SetEstimatePromptTokens(11)
			adaptor, err := GetChannelAdaptor(constant.APITypeGemini, protocol.GeminiEmbed)
			require.NoError(t, err)
			request, err := adaptor.ConvertRequest(c, info, tc.request)
			require.NoError(t, err)
			encoded, err := common.Marshal(request)
			require.NoError(t, err)
			response, err := adaptor.DoRequest(c, info, strings.NewReader(string(encoded)))
			require.NoError(t, err)
			usage, apiErr := adaptor.DoResponse(c, response.(*http.Response), info)
			require.Nil(t, apiErr)
			assert.Zero(t, usage.(*dto.Usage).PromptTokens)
			assert.Zero(t, usage.(*dto.Usage).TotalTokens)
			if tc.format == relaytypes.RelayFormatGemini {
				assert.JSONEq(t, tc.response, recorder.Body.String())
			} else {
				var delivered dto.OpenAIEmbeddingResponse
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &delivered))
				require.Len(t, delivered.Data, 1)
				assert.Equal(t, []float64{0.1, 0.2}, delivered.Data[0].Embedding)
			}
		})
	}
}

func TestPrepareResponsesRequestRetainsConvertedAdaptor(t *testing.T) {
	type capturedRequest struct {
		path string
		body []byte
	}
	captured := make(chan capturedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		captured <- capturedRequest{path: r.URL.Path, body: body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	}))
	defer server.Close()

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-4o")
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeAdvancedCustom)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, server.URL)
	common.SetContextKey(c, constant.ContextKeyChannelKey, "test-key")
	common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ProviderOtherSettings{
		AdvancedCustom: &dto.AdvancedCustomConfig{Routes: []dto.AdvancedCustomRoute{{
			IncomingPath: "/v1/responses", UpstreamPath: "/v1/chat/completions", Converter: relayconvert.ConverterOpenAIResponsesToOpenAIChat,
		}}},
	})
	request := &dto.OpenAIResponsesRequest{Model: "gpt-4o", Input: []byte(`"hello"`)}
	info := relaycommon.GenRelayInfoResponses(c, request)
	adaptor, body, closer, apiErr := PrepareResponsesRequest(c, info, request)
	require.Nil(t, apiErr)
	defer closer.Close()
	response, err := adaptor.DoRequest(c, info, body)
	require.NoError(t, err)
	httpResponse, ok := response.(*http.Response)
	require.True(t, ok)
	usage, apiErr := adaptor.DoResponse(c, httpResponse, info)
	require.Nil(t, apiErr)
	require.IsType(t, &dto.Usage{}, usage)
	assert.Equal(t, 5, usage.(*dto.Usage).TotalTokens)

	upstream := <-captured
	assert.Equal(t, "/v1/chat/completions", upstream.path)
	var upstreamRequest dto.GeneralOpenAIRequest
	require.NoError(t, common.Unmarshal(upstream.body, &upstreamRequest))
	require.Len(t, upstreamRequest.Messages, 1)
	assert.Equal(t, "hello", upstreamRequest.Messages[0].StringContent())
	assert.Equal(t, []relaytypes.RelayFormat{relaytypes.RelayFormatOpenAIResponses, relaytypes.RelayFormatOpenAI}, info.RequestConversionChain)
}

func TestIsResponsesEventStreamContentType(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		want        bool
	}{
		{name: "plain", contentType: "text/event-stream", want: true},
		{name: "mixed case with charset", contentType: "Text/Event-Stream; charset=utf-8", want: true},
		{name: "json", contentType: "application/json", want: false},
		{name: "empty", contentType: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isResponsesEventStreamContentType(tt.contentType))
		})
	}
}

func TestRecalcQuotaFromRatiosIgnoresInvalidMultipliers(t *testing.T) {
	info := &relaycommon.RelayInfo{
		PriceData: hosttypes.PriceData{
			Quota: 100,
		},
	}
	info.PriceData.AddOtherRatio("duration", 2)

	quota, ok := recalcQuotaFromRatios(info, map[string]float64{
		"duration": 3,
		"zero":     0,
		"negative": -1,
		"nan":      math.NaN(),
		"inf":      math.Inf(1),
	})

	require.True(t, ok)
	assert.Equal(t, 150, quota)
	assert.True(t, info.PriceData.HasOtherRatio("duration"))
}

func TestRecalcQuotaFromRatiosRejectsAllInvalidAdjustedRatios(t *testing.T) {
	info := &relaycommon.RelayInfo{
		PriceData: hosttypes.PriceData{
			Quota: 100,
		},
	}
	info.PriceData.AddOtherRatio("duration", 2)

	quota, ok := recalcQuotaFromRatios(info, map[string]float64{
		"zero":     0,
		"negative": -1,
		"nan":      math.NaN(),
		"inf":      math.Inf(1),
	})

	require.False(t, ok)
	assert.Equal(t, 0, quota)
	assert.True(t, info.PriceData.HasOtherRatio("duration"))
}

func TestTextRequestViaResponsesConvertsClaudeDirectly(t *testing.T) {
	type capturedRequest struct {
		path string
		body []byte
	}
	captured := make(chan capturedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		captured <- capturedRequest{path: r.URL.Path, body: body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_1",
			"object":"response",
			"status":"completed",
			"model":"gpt-5.6-sol",
			"output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],
			"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}
		}`))
	}))
	defer server.Close()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("Content-Type", "application/json")

	info := &relaycommon.RelayInfo{
		RelayMode:              relayconstant.RelayModeChatCompletions,
		RelayFormat:            relaytypes.RelayFormatClaude,
		OriginModelName:        "gpt-5.6-sol",
		RequestConversionChain: []relaytypes.RelayFormat{relaytypes.RelayFormatClaude},
		ProviderMeta: &relaycommon.ProviderMeta{
			ChannelType:       constant.ChannelTypeOpenAI,
			ChannelBaseUrl:    server.URL,
			ApiKey:            "test-key",
			UpstreamModelName: "gpt-5.6-sol",
		},
	}
	adaptor := &openaichannel.Adaptor{}
	adaptor.Init(info)
	request := &dto.ClaudeRequest{
		Model:    "gpt-5.6-sol",
		Thinking: &dto.Thinking{Type: "adaptive", Display: "summarized"},
		Messages: []dto.ClaudeMessage{{Role: "user", Content: "hello"}},
	}

	usage, apiErr := textRequestViaResponses(c, info, adaptor, request)

	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 5, usage.TotalTokens)
	assert.Equal(t, []relaytypes.RelayFormat{relaytypes.RelayFormatClaude, relaytypes.RelayFormatOpenAIResponses}, info.RequestConversionChain)

	upstream := <-captured
	assert.Equal(t, "/v1/responses", upstream.path)
	var upstreamBody map[string]any
	require.NoError(t, common.Unmarshal(upstream.body, &upstreamBody))
	assert.NotContains(t, upstreamBody, "messages")
	reasoning, ok := upstreamBody["reasoning"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "high", reasoning["effort"])
	assert.Equal(t, "detailed", reasoning["summary"])

	var response dto.ClaudeResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.Len(t, response.Content, 1)
	assert.Equal(t, "ok", response.Content[0].GetText())
}

func TestApplySystemPromptIfNeededSkipsToolLoadingMessages(t *testing.T) {
	tools := json.RawMessage(`[{"type":"function","function":{"name":"get_current_time","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]`)
	toolLoading := dto.Message{Role: "system", Tools: tools}
	user := dto.Message{Role: "user", Content: "What time is it in Beijing?"}

	tests := []struct {
		name         string
		messages     []dto.Message
		wantMessages []dto.Message
		wantOverride bool
	}{
		{
			name:     "tool loading message alone is not a system prompt",
			messages: []dto.Message{toolLoading, user},
			wantMessages: []dto.Message{
				{Role: "system", Content: "Answer in English."},
				toolLoading,
				user,
			},
		},
		{
			name:     "override targets the real system prompt only",
			messages: []dto.Message{toolLoading, {Role: "system", Content: "You are Kimi."}, user},
			wantMessages: []dto.Message{
				toolLoading,
				{Role: "system", Content: "Answer in English.\nYou are Kimi."},
				user,
			},
			wantOverride: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			info := &relaycommon.RelayInfo{
				ProviderMeta: &relaycommon.ProviderMeta{
					ChannelSetting: dto.ProviderSettings{
						SystemPrompt:         "Answer in English.",
						SystemPromptOverride: true,
					},
				},
			}
			request := &dto.GeneralOpenAIRequest{
				Model:    "kimi-k3",
				Messages: append([]dto.Message(nil), tt.messages...),
			}

			applySystemPromptIfNeeded(c, info, request)

			require.Len(t, request.Messages, len(tt.wantMessages))
			for i, want := range tt.wantMessages {
				got := request.Messages[i]
				assert.Equal(t, want.Role, got.Role, "message %d role", i)
				assert.Equal(t, want.Content, got.Content, "message %d content", i)
				if len(want.Tools) > 0 {
					assert.JSONEq(t, string(want.Tools), string(got.Tools), "message %d tools", i)
				} else {
					assert.Empty(t, got.Tools, "message %d tools", i)
				}
			}
			_, overrideSet := common.GetContextKey(c, constant.ContextKeySystemPromptOverride)
			assert.Equal(t, tt.wantOverride, overrideSet)
		})
	}
}
