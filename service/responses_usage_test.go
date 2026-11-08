package service

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/base64"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/capability"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/tokenkit"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRealtimeConversationFreezesItemsAcrossDeletionAndAudioTruncation(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/v1/realtime", nil)
	conversation := NewRealtimeConversation(c, "gpt-4o")
	_, err := conversation.Observe([]byte(`{"type":"conversation.item.create","item":{"id":"input","type":"message","role":"user","content":[{"type":"input_text","text":" hello hello"}]}}`), true)
	require.NoError(t, err)
	assert.True(t, conversation.Pending())
	assert.Zero(t, conversation.Usage(false).PromptTokens, "Only acknowledged items enter default context")
	_, err = conversation.Observe([]byte(`{"type":"conversation.item.created","item":{"id":"input","type":"message","role":"user","content":[{"type":"input_text","text":" hello hello"}]}}`), false)
	require.NoError(t, err)
	assert.False(t, conversation.Pending())
	snapshot := conversation.Snapshot()
	usage, err := snapshot.Estimate([]byte(`{"conversation":"none"}`))
	require.NoError(t, err)
	assert.Equal(t, 2, usage.PromptTokens, "Out-of-band membership does not replace default input")
	usage, err = snapshot.Estimate([]byte(`{"input":[]}`))
	require.NoError(t, err)
	assert.Zero(t, usage.PromptTokens)
	_, err = conversation.Observe([]byte(`{"type":"conversation.item.delete","item_id":"input"}`), true)
	require.NoError(t, err)
	assert.Equal(t, 2, conversation.Usage(false).PromptTokens, "Deletion waits for native acknowledgement")
	_, err = conversation.Observe([]byte(`{"type":"conversation.item.deleted","item_id":"input"}`), false)
	require.NoError(t, err)
	usage, err = snapshot.Estimate([]byte(`{"input":[{"type":"item_reference","id":"input"}]}`))
	require.NoError(t, err)
	assert.Equal(t, 2, usage.PromptTokens, "An admitted plan retains its frozen input")
	_, err = conversation.Snapshot().Estimate([]byte(`{"input":[{"type":"item_reference","id":"input"}]}`))
	require.Error(t, err)
	assert.Zero(t, conversation.Usage(false).PromptTokens)

	_, err = conversation.Observe([]byte(`{"type":"response.output_item.done","response_id":"r","item":{"id":"voice","type":"message","role":"assistant","content":[{"type":"audio","transcript":" hello hello"}]}}`), false)
	require.NoError(t, err)
	result := &relaycommon.RealtimeResponseFacts{ID: "r", Facts: &relaycommon.ResponseFacts{}, Usage: &dto.Usage{CompletionTokens: 12, CompletionTokenDetails: dto.OutputTokenDetails{AudioTokens: 10}}}
	require.NoError(t, conversation.Complete(result, true))
	snapshot = conversation.Snapshot()
	assert.Equal(t, 12, conversation.Usage(false).PromptTokens)
	_, err = conversation.Observe([]byte(`{"type":"conversation.item.truncate","item_id":"voice","content_index":0,"audio_end_ms":250}`), true)
	require.NoError(t, err)
	assert.Equal(t, 12, conversation.Usage(false).PromptTokens)
	_, err = conversation.Observe([]byte(`{"type":"conversation.item.truncated","item_id":"voice","content_index":0,"audio_end_ms":250}`), false)
	require.NoError(t, err)
	assert.Equal(t, 5, conversation.Usage(false).PromptTokens)
	assert.Equal(t, 5, conversation.Usage(false).PromptTokensDetails.AudioTokens)
	usage, err = snapshot.Estimate([]byte(`{"input":[{"type":"item_reference","id":"voice"}]}`))
	require.NoError(t, err)
	assert.Equal(t, 12, usage.PromptTokens)
	usage, err = conversation.Snapshot().Estimate([]byte(`{"input":[{"type":"item_reference","id":"voice"}]}`))
	require.NoError(t, err)
	assert.Equal(t, 5, usage.PromptTokens)
	_, err = conversation.Observe([]byte(`{"type":"conversation.item.deleted","item_id":"voice"}`), false)
	require.NoError(t, err)
	assert.Zero(t, conversation.Usage(false).PromptTokens)
	_, err = conversation.Snapshot().EstimateForCapabilities([]byte(`{}`), capability.Set{InputModalities: []capability.Modality{capability.Text}})
	require.NoError(t, err, "Deleting the audio item removes its input modality")
	for _, edit := range []struct {
		name, frame string
		expected    int
	}{
		{"delete before terminal", `{"type":"conversation.item.deleted","item_id":"voice"}`, 0},
		{"truncate before terminal", `{"type":"conversation.item.truncated","item_id":"voice","content_index":0,"audio_end_ms":250}`, 5},
	} {
		t.Run(edit.name, func(t *testing.T) {
			conversation := NewRealtimeConversation(c, "gpt-4o")
			_, err := conversation.Observe([]byte(`{"type":"response.output_item.added","response_id":"r","item":{"id":"voice","type":"message","role":"assistant","content":[{"type":"audio"}]}}`), false)
			require.NoError(t, err)
			_, err = conversation.Observe([]byte(edit.frame), false)
			require.NoError(t, err)
			require.NoError(t, conversation.Complete(result, true))
			assert.Equal(t, edit.expected, conversation.Usage(false).PromptTokens, "Late model completion cannot undo an acknowledged context edit")
		})
	}
	appendAudio := []byte(`{"type":"input_audio_buffer.append","audio":"` + base64.StdEncoding.EncodeToString(make([]byte, 4800)) + `"}`)
	_, err = conversation.Observe(appendAudio, true)
	require.NoError(t, err)
	_, err = conversation.Observe([]byte(`{"type":"input_audio_buffer.committed","item_id":"turn1"}`), false)
	require.NoError(t, err)
	_, err = conversation.Observe(appendAudio, true)
	require.NoError(t, err)
	_, err = conversation.Observe([]byte(`{"type":"input_audio_buffer.committed","item_id":"turn1"}`), false)
	require.NoError(t, err)
	_, err = conversation.Observe([]byte(`{"type":"input_audio_buffer.committed","item_id":"turn2"}`), false)
	require.NoError(t, err)
	assert.Equal(t, 2, conversation.Usage(false).PromptTokensDetails.AudioTokens, "A duplicate commit cannot consume the next audio turn")
}

func TestRealtimeUsageFreezesResponseCodecAndCountsTranscript(t *testing.T) {
	for _, kind := range []string{"response.audio_transcript.delta", "response.output_audio_transcript.delta"} {
		t.Run(kind, func(t *testing.T) {
			info := &relaycommon.RelayInfo{TokenizerModel: "gpt-4o", ProviderMeta: &relaycommon.ProviderMeta{}, OutputAudioFormat: "pcm16", StreamStatus: relaycommon.NewStreamStatus(), ResponseFacts: &relaycommon.ResponseFacts{}}
			accumulator := NewRealtimeUsageAccumulator(info)
			for _, raw := range []string{
				`{"type":"response.created","response":{"id":"voice","audio":{"output":{"format":{"type":"audio/pcmu"}}}}}`,
				`{"type":"response.output_audio.delta","response_id":"voice","item_id":"item","output_index":0,"delta":"` + base64.StdEncoding.EncodeToString(make([]byte, 8000)) + `"}`,
				`{"type":"` + kind + `","response_id":"voice","item_id":"item","output_index":0,"delta":" hello hello"}`,
			} {
				_, err := accumulator.Observe([]byte(raw), &dto.Usage{}, true)
				require.NoError(t, err)
			}
			result, err := accumulator.Observe([]byte(`{"type":"response.done","response":{"id":"voice","status":"completed","output":[{"id":"item","type":"message","content":[{"type":"audio","transcript":" hello hello"}]}]}}`), &dto.Usage{}, true)
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.Equal(t, 20, result.Usage.CompletionTokenDetails.AudioTokens)
			assert.Equal(t, 22, result.Usage.CompletionTokens)
			require.NotNil(t, result.Facts.GeneratedOutputTokens)
			assert.Equal(t, 22, *result.Facts.GeneratedOutputTokens)
			require.NotNil(t, result.Facts.DeliveredOutputTokens)
			assert.Equal(t, 22, *result.Facts.DeliveredOutputTokens)
		})
	}
}

func TestRealtimeUsageRetainsEstimatedImagesAndExplicitZero(t *testing.T) {
	for _, tc := range []struct {
		name, usage string
		image       int
	}{
		{"missing usage", "", 765},
		{"missing image detail", `,"usage":{"input_tokens":765,"output_tokens":1}`, 765},
		{"explicit image zero", `,"usage":{"input_tokens":765,"output_tokens":1,"input_token_details":{"image_tokens":0}}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := &relaycommon.RelayInfo{TokenizerModel: "gpt-4o", ProviderMeta: &relaycommon.ProviderMeta{}, StreamStatus: relaycommon.NewStreamStatus(), ResponseFacts: &relaycommon.ResponseFacts{}}
			accumulator := NewRealtimeUsageAccumulator(info)
			input := &dto.Usage{PromptTokens: 765, TotalTokens: 765, PromptTokensDetails: dto.InputTokenDetails{ImageTokens: 765}}
			_, err := accumulator.Observe([]byte(`{"type":"response.created","response":{"id":"image"}}`), input, true)
			require.NoError(t, err)
			_, err = accumulator.Observe([]byte(`{"type":"response.text.delta","response_id":"image","delta":"hello"}`), input, true)
			require.NoError(t, err)
			result, err := accumulator.Observe([]byte(`{"type":"response.done","response":{"id":"image","status":"completed"`+tc.usage+`}}`), input, true)
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.Equal(t, 765, result.Usage.PromptTokens)
			assert.Equal(t, tc.image, result.Usage.PromptTokensDetails.ImageTokens)
		})
	}
}

func TestResponsesUsageRejectsInvalidCumulativeFieldsAndRetainsCacheLifetimeZero(t *testing.T) {
	info := &relaycommon.RelayInfo{ProviderMeta: &relaycommon.ProviderMeta{UpstreamModelName: "gpt-4o"}, StreamStatus: relaycommon.NewStreamStatus(), ResponseFacts: &relaycommon.ResponseFacts{}}
	accumulator := NewResponsesUsageAccumulator(info)
	for _, raw := range []string{
		`{"type":"response.created","response":{"usage":{"input_tokens":100,"output_tokens":10,"total_tokens":110,"input_tokens_details":{"cache_write_tokens":100,"cache_write_tokens_details":{"ttl_12h_tokens":100,"ttl_30m_tokens":0}}}}}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":100,"output_tokens":2147483648,"total_tokens":2147483748,"input_tokens_details":{"cache_write_tokens":100,"cache_write_tokens_details":{"ttl_12h_tokens":0,"ttl_30m_tokens":100}}}}}`,
	} {
		var event dto.ResponsesStreamResponse
		require.NoError(t, common.UnmarshalJsonStr(raw, &event))
		accumulator.Observe(&event, []byte(raw))
	}
	usage := accumulator.Finish()
	assert.Equal(t, 100, usage.PromptTokens)
	assert.Equal(t, 10, usage.CompletionTokens)
	assert.Equal(t, 110, usage.TotalTokens)
	require.NotNil(t, usage.PromptTokensDetails.CacheWriteTokensDetails)
	assert.Zero(t, usage.PromptTokensDetails.CacheWriteTokensDetails.TTL12hTokens)
	assert.Equal(t, 100, usage.PromptTokensDetails.CacheWriteTokensDetails.TTL30mTokens)
	assert.True(t, info.ResponseFacts.UsageReported)
}

func TestResponsesUsageEstimatesMixedAndCustomTools(t *testing.T) {
	for _, tc := range []struct{ name, delta, terminal, output string }{
		{name: "text and terminal function", delta: `{"type":"response.output_text.delta","output_index":0,"delta":"hello "}`, terminal: `{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello "}]},{"type":"function_call","name":"lookup","arguments":"{}"}]}}`, output: "hello lookup{}"},
		{name: "terminal custom tool", terminal: `{"type":"response.completed","response":{"status":"completed","output":[{"type":"custom_tool_call","name":"apply_patch","input":"hello"}]}}`, output: "apply_patchhello"},
		{name: "custom tool delta and terminal", delta: `{"type":"response.custom_tool_call_input.delta","output_index":0,"delta":"hello"}`, terminal: `{"type":"response.completed","response":{"status":"completed","output":[{"type":"custom_tool_call","name":"apply_patch","input":"hello"}]}}`, output: "helloapply_patch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := &relaycommon.RelayInfo{TokenizerModel: "gpt-4o", ProviderMeta: &relaycommon.ProviderMeta{UpstreamModelName: "gpt-4o"}, StreamStatus: relaycommon.NewStreamStatus(), ResponseFacts: &relaycommon.ResponseFacts{}}
			info.SetEstimatePromptTokens(10)
			accumulator := NewResponsesUsageAccumulator(info)
			for _, raw := range []string{tc.delta, tc.terminal} {
				if raw == "" {
					continue
				}
				var event dto.ResponsesStreamResponse
				require.NoError(t, common.UnmarshalJsonStr(raw, &event))
				accumulator.Observe(&event, []byte(raw))
			}
			usage := accumulator.Finish()
			assert.Equal(t, 10, usage.PromptTokens)
			assert.Equal(t, tc.output, info.ResponseFacts.GeneratedText)
			assert.Equal(t, tokenkit.Count("gpt-4o", tc.output), usage.CompletionTokens)
			assert.False(t, info.ResponseFacts.UsageReported)
		})
	}
}

func TestRealtimeUsageCorrelatesConcurrentResponses(t *testing.T) {
	info := &relaycommon.RelayInfo{TokenizerModel: "gpt-4o", ProviderMeta: &relaycommon.ProviderMeta{UpstreamModelName: "upstream"}, OutputAudioFormat: "pcm16", ResponseFacts: &relaycommon.ResponseFacts{}}
	accumulator := NewRealtimeUsageAccumulator(info)
	for _, id := range []string{"a", "b"} {
		result, err := accumulator.Observe([]byte(`{"type":"response.created","response":{"id":"`+id+`","status":"in_progress"}}`), &dto.Usage{PromptTokens: 10}, true)
		require.NoError(t, err)
		assert.Nil(t, result)
	}
	_, err := accumulator.Observe([]byte(`{"type":"response.output_text.delta","response_id":"a","delta":"hello"}`), nil, true)
	require.NoError(t, err)
	audio := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 24000)))
	for range 2 {
		_, err := accumulator.Observe([]byte(`{"type":"response.output_audio.delta","response_id":"b","delta":"`+audio+`"}`), nil, true)
		require.NoError(t, err)
	}
	b := []byte(`{"type":"response.done","response":{"id":"b","status":"completed","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0,"input_token_details":{"audio_tokens":0},"output_token_details":{"audio_tokens":0}}}}`)
	result, err := accumulator.Observe(b, nil, true)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "b", result.ID)
	assert.Zero(t, result.Usage.PromptTokens)
	assert.Zero(t, result.Usage.CompletionTokens)
	assert.Zero(t, result.Usage.CompletionTokenDetails.AudioTokens)
	assert.Equal(t, "upstream", result.Usage.UsageSource)
	assert.Equal(t, 20, *result.Facts.GeneratedOutputTokens)
	assert.True(t, result.Facts.BusinessOutput)
	a := []byte(`{"type":"response.done","response":{"id":"a","status":"completed","usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12,"input_token_details":{"cached_tokens":3,"audio_tokens":4},"output_token_details":{"audio_tokens":1}}}}`)
	result, err = accumulator.Observe(a, nil, true)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "a", result.ID)
	assert.Equal(t, 10, result.Usage.PromptTokens)
	assert.Equal(t, 2, result.Usage.CompletionTokens)
	assert.Equal(t, 3, result.Usage.PromptTokensDetails.CachedTokens)
	assert.Equal(t, 4, result.Usage.PromptTokensDetails.AudioTokens)
	assert.Equal(t, 1, result.Usage.CompletionTokenDetails.AudioTokens)
	result, err = accumulator.Observe(a, nil, true)
	require.NoError(t, err)
	assert.Nil(t, result, "replayed cumulative terminal cannot produce another settlement quantity")
	assert.Empty(t, accumulator.Finish())
}

func TestRealtimeUsageEstimatesMissingAudioAcrossChunkBoundaries(t *testing.T) {
	info := &relaycommon.RelayInfo{TokenizerModel: "gpt-4o", ProviderMeta: &relaycommon.ProviderMeta{UpstreamModelName: "upstream"}, OutputAudioFormat: "pcm16"}
	accumulator := NewRealtimeUsageAccumulator(info)
	_, err := accumulator.Observe([]byte(`{"type":"response.created","response":{"id":"audio","status":"in_progress"}}`), &dto.Usage{PromptTokens: 5, PromptTokensDetails: dto.InputTokenDetails{AudioTokens: 4}}, true)
	require.NoError(t, err)
	audio := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 24000)))
	for range 2 {
		_, err = accumulator.Observe([]byte(`{"type":"response.output_audio.delta","response_id":"audio","delta":"`+audio+`"}`), nil, false)
		require.NoError(t, err)
	}
	_, err = accumulator.Observe([]byte(`{"type":"response.output_audio_transcript.delta","response_id":"audio","delta":"hello"}`), nil, false)
	require.NoError(t, err)
	results := accumulator.Finish()
	require.Len(t, results, 1)
	assert.Equal(t, 5, results[0].Usage.PromptTokens)
	assert.Equal(t, 4, results[0].Usage.PromptTokensDetails.AudioTokens)
	assert.Equal(t, 21, results[0].Usage.CompletionTokens) // 20 audio tokens plus one transcript token.
	assert.Equal(t, 20, results[0].Usage.CompletionTokenDetails.AudioTokens)
	assert.Equal(t, "estimated", results[0].Usage.UsageSource)
	assert.False(t, results[0].Facts.BusinessOutput, "failed writes preserve procurement evidence without claiming delivery")
	assert.Empty(t, accumulator.Finish())
}

func TestResponsesUsageAccumulatorPreservesExplicitZero(t *testing.T) {
	info := &relaycommon.RelayInfo{
		ProviderMeta:  &relaycommon.ProviderMeta{UpstreamModelName: "gpt-4o"},
		StreamStatus:  relaycommon.NewStreamStatus(),
		ResponseFacts: &relaycommon.ResponseFacts{},
	}
	info.SetEstimatePromptTokens(100)
	accumulator := NewResponsesUsageAccumulator(info)
	initial := []byte(`{"type":"response.created","response":{"status":"in_progress","usage":{"input_tokens":100,"output_tokens":10,"total_tokens":110,"input_tokens_details":{"cached_tokens":20,"audio_tokens":5},"output_tokens_details":{"audio_tokens":2}}}}`)
	var initialEvent dto.ResponsesStreamResponse
	require.NoError(t, common.Unmarshal(initial, &initialEvent))
	accumulator.Observe(&initialEvent, initial)
	accumulator.Observe(&dto.ResponsesStreamResponse{Type: "response.output_text.delta", Delta: "hello"}, nil)
	raw := []byte(`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0,"input_tokens_details":{"cached_tokens":0,"audio_tokens":0},"output_tokens_details":{"audio_tokens":0}}}}`)
	var event dto.ResponsesStreamResponse
	require.NoError(t, common.Unmarshal(raw, &event))
	accumulator.Observe(&event, raw)
	accumulator.Observe(&event, raw)
	usage := accumulator.Finish()
	assert.Zero(t, usage.PromptTokens)
	assert.Zero(t, usage.CompletionTokens)
	assert.True(t, info.ResponseFacts.UsageReported)
	assert.Zero(t, usage.PromptTokensDetails.CachedTokens)
	assert.Zero(t, usage.PromptTokensDetails.AudioTokens)
	assert.Zero(t, usage.CompletionTokenDetails.AudioTokens)
	assert.Same(t, usage, accumulator.Finish())
}

func TestResponsesUsageAccumulatorEstimatesTerminalFunctionCall(t *testing.T) {
	info := &relaycommon.RelayInfo{ProviderMeta: &relaycommon.ProviderMeta{UpstreamModelName: "gpt-4o"}, StreamStatus: relaycommon.NewStreamStatus(), ResponseFacts: &relaycommon.ResponseFacts{}}
	info.SetEstimatePromptTokens(100)
	accumulator := NewResponsesUsageAccumulator(info)
	raw := []byte(`{"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","name":"lookup","arguments":"{\"city\":\"Paris\"}"}]}}`)
	var event dto.ResponsesStreamResponse
	require.NoError(t, common.Unmarshal(raw, &event))
	accumulator.Observe(&event, raw)
	usage := accumulator.Finish()
	assert.Equal(t, 100, usage.PromptTokens)
	assert.Equal(t, tokenkit.Count("gpt-4o", `lookup{"city":"Paris"}`), usage.CompletionTokens)
	assert.False(t, info.ResponseFacts.UsageReported)
}

func TestResponsesUsageAccumulatorTerminalAccounting(t *testing.T) {
	operation_setting.SetToolPriceForTest("responses_priced_fn", 5)
	t.Cleanup(func() { operation_setting.DeleteToolPriceForTest("responses_priced_fn") })
	for _, tc := range []struct {
		eventType  string
		wantImages int
	}{
		{eventType: "response.completed", wantImages: 1},
		{eventType: "response.done", wantImages: 1},
		{eventType: "response.incomplete", wantImages: 1},
		{eventType: "response.failed", wantImages: 1},
		{eventType: "response.cancelled", wantImages: 1},
		{eventType: "response.canceled", wantImages: 1},
	} {
		t.Run(tc.eventType, func(t *testing.T) {
			info := &relaycommon.RelayInfo{OriginModelName: "gpt-5.1", StreamStatus: relaycommon.NewStreamStatus()}
			accumulator := NewResponsesUsageAccumulator(info)
			for _, item := range []dto.ResponsesOutput{
				{Type: dto.BuildInCallWebSearchCall},
				{Type: dto.BuildInCallFileSearchCall},
				{Type: dto.BuildInCallFunctionCall, Name: "responses_priced_fn"},
				{Type: dto.BuildInCallFunctionCall, Name: "responses_unpriced_fn"},
			} {
				accumulator.Observe(&dto.ResponsesStreamResponse{Type: dto.ResponsesOutputTypeItemDone, Item: &item}, nil)
			}
			image := dto.ResponsesOutput{ID: "image-1", Type: dto.ResponsesOutputTypeImageGenerationCall, Status: "completed", Result: "image-data"}
			accumulator.Observe(&dto.ResponsesStreamResponse{Type: dto.ResponsesOutputTypeItemDone, Item: &image}, nil)
			upstream := &dto.Usage{InputTokens: 20, OutputTokens: 5, TotalTokens: 25, InputTokensDetails: &dto.InputTokenDetails{CachedTokens: 4}}
			upstream.BillingUsage = dto.NewOpenAIResponsesBillingUsage(upstream)
			terminal := &dto.ResponsesStreamResponse{
				Type: tc.eventType,
				Response: &dto.OpenAIResponsesResponse{
					Usage: upstream, Output: []dto.ResponsesOutput{image},
				},
			}
			accumulator.Observe(terminal, nil)
			accumulator.Observe(terminal, nil)
			usage := accumulator.Finish()
			assert.Equal(t, tc.eventType == "response.failed", info.StreamStatus.ResponseFailed())
			assert.NotEmpty(t, info.StreamStatus.ResponseOutcome())

			assert.Equal(t, 20, usage.PromptTokens)
			assert.Equal(t, 5, usage.CompletionTokens)
			assert.Equal(t, 25, usage.TotalTokens)
			assert.Equal(t, 4, usage.PromptTokensDetails.CachedTokens)
			require.NotNil(t, usage.BillingUsage)
			assert.Equal(t, upstream.BillingUsage, usage.BillingUsage)
			assert.NotSame(t, upstream.BillingUsage, usage.BillingUsage)
			tools := info.ResponsesUsageInfo.BuiltInTools
			for _, name := range []string{dto.BuildInToolWebSearchPreview, dto.BuildInToolFileSearch, "responses_priced_fn"} {
				require.Contains(t, tools, name)
				assert.Equal(t, 1, tools[name].CallCount)
			}
			assert.NotContains(t, tools, "responses_unpriced_fn")
			require.Contains(t, tools, dto.BuildInToolImageGeneration)
			assert.Equal(t, tc.wantImages, tools[dto.BuildInToolImageGeneration].CallCount)
		})
	}
}

func TestResponsesUsageAccumulatorInterruptedTextFallback(t *testing.T) {
	for _, withUsage := range []bool{false, true} {
		name := "disconnect without terminal usage"
		if withUsage {
			name = "failed response preserves native billing usage"
		}
		t.Run(name, func(t *testing.T) {
			info := &relaycommon.RelayInfo{ProviderMeta: &relaycommon.ProviderMeta{UpstreamModelName: "gpt-4o"}}
			info.SetEstimatePromptTokens(100)
			accumulator := NewResponsesUsageAccumulator(info)
			accumulator.Observe(&dto.ResponsesStreamResponse{Type: "response.output_text.delta", Delta: "hello"}, nil)
			if withUsage {
				upstream := &dto.Usage{InputTokens: 20, InputTokensDetails: &dto.InputTokenDetails{CachedTokens: 4}}
				upstream.BillingUsage = dto.NewOpenAIResponsesBillingUsage(upstream)
				accumulator.Observe(&dto.ResponsesStreamResponse{Type: "response.failed", Response: &dto.OpenAIResponsesResponse{Usage: upstream}}, nil)
			}
			usage := accumulator.Finish()
			assert.Equal(t, 1, usage.CompletionTokens)
			if withUsage {
				assert.Equal(t, 20, usage.PromptTokens)
				assert.Equal(t, 21, usage.TotalTokens)
				require.NotNil(t, usage.BillingUsage)
				assert.Equal(t, dto.BillingUsageSourceOAIResponses, usage.BillingUsage.Source)
				assert.True(t, usage.BillingUsage.Estimated)
				canonical, ok := usage.BillingUsage.CanonicalUsage()
				require.True(t, ok)
				assert.Equal(t, 4, canonical.PromptTokensDetails.CachedTokens)
				assert.Equal(t, 1, canonical.CompletionTokens)
			} else {
				assert.Equal(t, 100, usage.PromptTokens)
				assert.Equal(t, 101, usage.TotalTokens)
			}
			accumulator.Observe(&dto.ResponsesStreamResponse{Type: "response.output_text.delta", Delta: " late output"}, nil)
			assert.Equal(t, usage, accumulator.Finish())
			assert.Equal(t, 1, usage.CompletionTokens)
		})
	}
}

func TestObserveResponsesOutcomeRecordsProtocolFacts(t *testing.T) {
	for _, tc := range []struct {
		name        string
		event       string
		wantOutcome relaycommon.ResponseOutcome
		wantCode    string
		wantType    string
		wantIncompl string
	}{
		{"flat sse error", `{"type":"error","code":"context_length_exceeded","message":"too long"}`, relaycommon.ResponseOutcomeFailed, "context_length_exceeded", "", ""},
		{"done with failed status", `{"type":"response.done","response":{"status":"failed","error":{"code":"invalid_api_key","type":"invalid_request_error","message":"bad key"}}}`, relaycommon.ResponseOutcomeFailed, "invalid_api_key", "invalid_request_error", ""},
		{"incomplete keeps reason", `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`, relaycommon.ResponseOutcomeIncomplete, "", "", "max_output_tokens"},
		{"in progress is not terminal", `{"type":"response.created","response":{"status":"in_progress"}}`, relaycommon.ResponseOutcomeUnknown, "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var event dto.ResponsesStreamResponse
			require.NoError(t, common.UnmarshalJsonStr(tc.event, &event))
			info := &relaycommon.RelayInfo{StreamStatus: relaycommon.NewStreamStatus()}
			ObserveResponsesOutcome(info, &event)
			outcome := info.StreamStatus.OutcomeSnapshot()
			assert.Equal(t, tc.wantOutcome, outcome.Response)
			assert.Equal(t, tc.wantCode, outcome.ErrorCode)
			assert.Equal(t, tc.wantType, outcome.ErrorType)
			assert.Equal(t, tc.wantIncompl, outcome.IncompleteReason)
		})
	}
}

func TestResponsesUsageAccumulatorDisconnectBillsCompletedImage(t *testing.T) {
	info := &relaycommon.RelayInfo{OriginModelName: "gpt-5.1", StreamStatus: relaycommon.NewStreamStatus()}
	accumulator := NewResponsesUsageAccumulator(info)
	accumulator.Observe(&dto.ResponsesStreamResponse{
		Type: dto.ResponsesOutputTypeItemDone,
		Item: &dto.ResponsesOutput{ID: "complete-image", Type: dto.ResponsesOutputTypeImageGenerationCall, Status: "completed", Result: "final-image-data"},
	}, nil)
	accumulator.Observe(&dto.ResponsesStreamResponse{
		Type: dto.ResponsesOutputTypeItemDone,
		Item: &dto.ResponsesOutput{ID: "partial-image", Type: dto.ResponsesOutputTypeImageGenerationCall, Status: "partial", Result: "partial-image-data"},
	}, nil)
	usage := accumulator.Finish()
	assert.Zero(t, usage.TotalTokens)
	require.Contains(t, info.ResponsesUsageInfo.BuiltInTools, dto.BuildInToolImageGeneration)
	assert.Equal(t, 1, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolImageGeneration].CallCount)
	accumulator.Finish()
	assert.Equal(t, 1, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolImageGeneration].CallCount)
}

func TestApplyResponsesUsageCopiesTokenDetails(t *testing.T) {
	dst := &dto.Usage{}
	src := &dto.Usage{
		InputTokens:  11,
		OutputTokens: 7,
		TotalTokens:  18,
		InputTokensDetails: &dto.InputTokenDetails{
			CachedTokens:         3,
			CachedCreationTokens: 2,
			TextTokens:           6,
			AudioTokens:          4,
			ImageTokens:          5,
		},
		OutputTokensDetails: &dto.OutputTokenDetails{
			TextTokens:      1,
			AudioTokens:     2,
			ImageTokens:     3,
			ReasoningTokens: 4,
		},
		PromptCacheHitTokens: 3,
		UsageSemantic:        "openai",
		UsageSource:          "upstream",
	}

	ApplyResponsesUsage(dst, src)

	assert.Equal(t, 11, dst.PromptTokens)
	assert.Equal(t, 7, dst.CompletionTokens)
	assert.Equal(t, 18, dst.TotalTokens)
	require.NotNil(t, dst.InputTokensDetails)
	assert.Equal(t, *src.InputTokensDetails, dst.PromptTokensDetails)
	assert.Equal(t, src.InputTokensDetails, dst.InputTokensDetails)
	assert.NotSame(t, src.InputTokensDetails, dst.InputTokensDetails)
	assert.Equal(t, *src.OutputTokensDetails, dst.CompletionTokenDetails)
	require.NotNil(t, dst.OutputTokensDetails)
	assert.Equal(t, *src.OutputTokensDetails, *dst.OutputTokensDetails)
	assert.NotSame(t, src.OutputTokensDetails, dst.OutputTokensDetails)
	assert.Equal(t, 3, dst.PromptCacheHitTokens)
	assert.Equal(t, "openai", dst.UsageSemantic)
	assert.Equal(t, "upstream", dst.UsageSource)
}

func TestApplyResponsesUsageFallsBackToCompletionTokenDetails(t *testing.T) {
	dst := &dto.Usage{}
	src := &dto.Usage{
		CompletionTokenDetails: dto.OutputTokenDetails{
			ReasoningTokens: 9,
		},
	}

	ApplyResponsesUsage(dst, src)

	assert.Equal(t, 9, dst.CompletionTokenDetails.ReasoningTokens)
	require.NotNil(t, dst.OutputTokensDetails)
	assert.Equal(t, 9, dst.OutputTokensDetails.ReasoningTokens)
}

func TestApplyResponsesUsagePreservesBillingSnapshotAcrossPartialUpdates(t *testing.T) {
	dst := &dto.Usage{}
	src := &dto.Usage{
		InputTokens:  20,
		OutputTokens: 10,
		TotalTokens:  30,
		InputTokensDetails: &dto.InputTokenDetails{
			CachedTokens: 3,
			AudioTokens:  2,
		},
		CompletionTokenDetails: dto.OutputTokenDetails{AudioTokens: 4},
		UsageSemantic:          "openai",
		UsageSource:            "upstream",
	}
	src.BillingUsage = dto.NewOpenAIResponsesBillingUsage(src)
	ApplyResponsesUsage(dst, src)
	ApplyResponsesUsage(dst, &dto.Usage{
		OutputTokens:        12,
		InputTokensDetails:  &dto.InputTokenDetails{},
		OutputTokensDetails: &dto.OutputTokenDetails{ReasoningTokens: 5},
	})

	assert.Equal(t, 20, dst.PromptTokens)
	assert.Equal(t, 12, dst.CompletionTokens)
	assert.Equal(t, 32, dst.TotalTokens)
	assert.Equal(t, 3, dst.PromptTokensDetails.CachedTokens)
	assert.Equal(t, 2, dst.PromptTokensDetails.AudioTokens)
	assert.Equal(t, 4, dst.CompletionTokenDetails.AudioTokens)
	assert.Equal(t, 5, dst.CompletionTokenDetails.ReasoningTokens)
	require.NotNil(t, dst.OutputTokensDetails)
	assert.Equal(t, dst.CompletionTokenDetails, *dst.OutputTokensDetails)
	assert.Equal(t, "openai", dst.UsageSemantic)
	assert.Equal(t, "upstream", dst.UsageSource)
	require.NotNil(t, dst.BillingUsage)
	assert.Equal(t, src.BillingUsage, dst.BillingUsage)
	assert.NotSame(t, src.BillingUsage, dst.BillingUsage)
}

func TestResponsesUsageAccumulatorMissingUsageEstimation(t *testing.T) {
	const model = "gpt-4o"
	const summary = "Inspect the repository before editing."
	const arguments = `{"command":["bash","-lc","ls"]}`
	inProgress := &dto.OpenAIResponsesResponse{Status: []byte(`"in_progress"`)}
	for _, tc := range []struct {
		name           string
		events         []dto.ResponsesStreamResponse
		wantPrompt     int
		wantCompletion int
	}{
		{
			name: "tool call stream cut before terminal usage",
			events: []dto.ResponsesStreamResponse{
				{Type: "response.created", Response: inProgress},
				{Type: "response.reasoning_summary_text.delta", Delta: summary},
				{Type: "response.function_call_arguments.delta", Delta: arguments},
				{Type: dto.ResponsesOutputTypeItemDone, Item: &dto.ResponsesOutput{Type: dto.BuildInCallFunctionCall, Name: "shell"}},
			},
			wantPrompt:     100,
			wantCompletion: tokenkit.Count(model, summary+arguments+"shell"),
		},
		{
			name: "created only then disconnect bills the prompt",
			events: []dto.ResponsesStreamResponse{
				{Type: "response.created", Response: inProgress},
			},
			wantPrompt: 100,
		},
		{
			name: "incomplete without usage bills the prompt",
			events: []dto.ResponsesStreamResponse{
				{Type: "response.created", Response: inProgress},
				{Type: "response.incomplete", Response: &dto.OpenAIResponsesResponse{Status: []byte(`"incomplete"`)}},
			},
			wantPrompt: 100,
		},
		{
			name: "completed without usage estimates from terminal output",
			events: []dto.ResponsesStreamResponse{
				{Type: "response.completed", Response: &dto.OpenAIResponsesResponse{
					Status: []byte(`"completed"`),
					Output: []dto.ResponsesOutput{{
						Type:    "message",
						Role:    "assistant",
						Content: []dto.ResponsesOutputContent{{Type: "output_text", Text: "final answer"}},
					}},
				}},
			},
			wantPrompt:     100,
			wantCompletion: tokenkit.Count(model, "final answer"),
		},
		{
			name: "explicit failure without usage bills nothing",
			events: []dto.ResponsesStreamResponse{
				{Type: "response.created", Response: inProgress},
				{Type: "response.failed", Response: &dto.OpenAIResponsesResponse{Status: []byte(`"failed"`)}},
			},
		},
		{
			name: "flat error event bills nothing",
			events: []dto.ResponsesStreamResponse{
				{Type: "response.created", Response: inProgress},
				{Type: "error", Code: "server_error"},
			},
		},
		{
			name: "no upstream events bills nothing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := &relaycommon.RelayInfo{
				ProviderMeta: &relaycommon.ProviderMeta{UpstreamModelName: model},
				StreamStatus: relaycommon.NewStreamStatus(),
			}
			info.SetEstimatePromptTokens(100)
			accumulator := NewResponsesUsageAccumulator(info)
			for i := range tc.events {
				accumulator.Observe(&tc.events[i], nil)
			}
			usage := accumulator.Finish()
			assert.Equal(t, tc.wantPrompt, usage.PromptTokens)
			assert.Equal(t, tc.wantCompletion, usage.CompletionTokens)
			assert.Equal(t, tc.wantPrompt+tc.wantCompletion, usage.TotalTokens)
		})
	}
}
