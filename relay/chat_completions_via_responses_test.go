package relay

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/protocol"
	openaichannel "github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	relaytypes "github.com/QuantumNous/new-api/relaykit/types"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
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
