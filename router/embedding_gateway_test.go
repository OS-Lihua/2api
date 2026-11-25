package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmbeddingGatewayUsesOfferingWalletAndPriceVersions(t *testing.T) {
	for _, test := range []struct {
		name, path, body, upstream, responseField string
		protocol                                  protocol.Type
		provider                                  int
	}{
		{
			name: "OpenAI embeddings", path: "/v1/embeddings", protocol: protocol.OpenAIEmbeddings, provider: constant.ChannelTypeOpenAI,
			body:          `{"model":"gateway-alias","input":"A bird","dimensions":2}`,
			upstream:      `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":12,"total_tokens":12}}`,
			responseField: "data",
		},
		{
			name: "Gemini embeddings", path: "/v1beta/models/gateway-alias:embedContent", protocol: protocol.GeminiEmbed, provider: constant.ChannelTypeGemini,
			body:          `{"content":{"parts":[{"text":"A bird"}]},"outputDimensionality":2}`,
			upstream:      `{"embedding":{"values":[0.1,0.2]},"usageMetadata":{"promptTokenCount":12}}`,
			responseField: "embedding",
		},
		{
			name: "legacy engine embeddings", path: "/v1/engines/gateway-alias/embeddings", protocol: protocol.OpenAIEmbeddings, provider: constant.ChannelTypeOpenAI,
			body:     `{"input":"A bird","dimensions":2}`,
			upstream: `{"data":[{"index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":12,"total_tokens":12}}`, responseField: "data",
		},
		{
			name: "Gemini batch", path: "/v1/models/gateway-alias:batchEmbedContents", protocol: protocol.GeminiEmbed, provider: constant.ChannelTypeGemini,
			body:     `{"requests":[{"content":{"parts":[{"text":"A bird"}]},"outputDimensionality":2},{"content":{"parts":[{"text":"A tree"}]},"outputDimensionality":2}]}`,
			upstream: `{"embeddings":[{"values":[0.1,0.2]},{"values":[0.3,0.4]}],"usageMetadata":{"promptTokenCount":12}}`, responseField: "embeddings",
		},
		{
			name: "OpenAI to Gemini", path: "/v1/embeddings", protocol: protocol.GeminiEmbed, provider: constant.ChannelTypeGemini,
			body:     `{"model":"gateway-alias","input":["A bird","A tree"],"dimensions":2}`,
			upstream: `{"embeddings":[{"values":[0.1,0.2]},{"values":[0.3,0.4]}],"usageMetadata":{"promptTokenCount":12}}`, responseField: "data",
		},
		{
			name: "Gemini to OpenAI", path: "/v1beta/models/gateway-alias:embedContent", protocol: protocol.OpenAIEmbeddings, provider: constant.ChannelTypeOpenAI,
			body:     `{"content":{"parts":[{"text":"A bird"}]},"outputDimensionality":2}`,
			upstream: `{"data":[{"index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":12,"total_tokens":12}}`, responseField: "embedding",
		},
		{
			name: "Gemini batch to OpenAI", path: "/v1beta/models/gateway-alias:batchEmbedContents", protocol: protocol.OpenAIEmbeddings, provider: constant.ChannelTypeOpenAI,
			body:     `{"requests":[{"content":{"parts":[{"text":"A bird"}]},"outputDimensionality":2},{"content":{"parts":[{"text":"A tree"}]},"outputDimensionality":2}]}`,
			upstream: `{"data":[{"index":0,"embedding":[0.1,0.2]},{"index":1,"embedding":[0.3,0.4]}],"usage":{"prompt_tokens":12,"total_tokens":12}}`, responseField: "embeddings",
		},
		{
			name: "Jina embeddings", path: "/v1/embeddings", protocol: protocol.OpenAIEmbeddings, provider: constant.ChannelTypeJina,
			body:     `{"model":"gateway-alias","input":"A bird","dimensions":2}`,
			upstream: `{"data":[{"index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":12}}`, responseField: "data",
		},
		{
			name: "Jina base64 embeddings", path: "/v1/embeddings", protocol: protocol.OpenAIEmbeddings, provider: constant.ChannelTypeJina,
			body:     `{"model":"gateway-alias","input":"A bird","dimensions":2,"encoding_format":"base64"}`,
			upstream: `{"data":[{"index":0,"embedding":"zczMPc3MTD4="}],"usage":{"prompt_tokens":12}}`, responseField: "data",
		},
		{
			name: "Jina rerank", path: "/v1/rerank", protocol: protocol.Rerank, provider: constant.ChannelTypeJina,
			body:     `{"model":"gateway-alias","query":"bird","documents":["A bird"],"return_documents":false,"overlap_tokens":0}`,
			upstream: `{"results":[{"index":0,"relevance_score":0.9}],"usage":{"total_tokens":12}}`, responseField: "results",
		},
		{
			name: "Cohere rerank", path: "/v1/rerank", protocol: protocol.Rerank, provider: constant.ChannelTypeCohere,
			body:     `{"model":"gateway-alias","query":"bird","documents":["A bird"],"return_documents":false}`,
			upstream: `{"results":[{"index":0,"relevance_score":0.9}],"usage":{"total_tokens":12}}`, responseField: "results",
		},
		{
			name: "rerank", path: "/v1/rerank", protocol: protocol.Rerank, provider: constant.ChannelTypeOpenAI,
			body:          `{"model":"gateway-alias","query":"bird","documents":["A bird","A tree"],"top_n":1}`,
			upstream:      `{"results":[{"index":0,"relevance_score":0.9}],"usage":{"total_tokens":12}}`,
			responseField: "results",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			var fixture textGatewayFixture
			fixture = newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				wire, err := io.ReadAll(r.Body)
				assert.NoError(t, err)
				assert.Contains(t, string(wire), "upstream-model")
				assert.NotContains(t, string(wire), "gateway-alias")
				if test.name == "Jina base64 embeddings" {
					assert.Contains(t, string(wire), `"encoding_format":"base64"`)
				}
				if test.protocol == protocol.GeminiEmbed {
					assert.Contains(t, r.URL.Path, "models/upstream-model:")
				}
				// A price change after sending must affect neither side of this Attempt.
				fixture.fake.Advance(time.Second)
				for _, current := range []*model.PriceVersion{fixture.sale, fixture.purchase} {
					expression := `tier("changed", fixed(0.5))`
					if current.OwnerType == model.PriceOwnerChannel {
						expression = `tier("changed", fixed(0.1))`
					}
					price := &model.PriceVersion{OwnerType: current.OwnerType, OwnerID: current.OwnerID, Currency: current.Currency, Expression: expression, EffectiveAt: fixture.fake.Now().Unix()}
					assert.NoError(t, model.CreatePriceVersion(t.Context(), price, current.CreatedBy))
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, test.upstream)
			})
			require.NoError(t, model.DB.Model(fixture.channel).Update("protocol", test.protocol).Error)
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", test.provider).Error)
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, test.path, test.body)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.EqualValues(t, 1, calls.Load())
			var payload map[string]any
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
			assert.Contains(t, payload, test.responseField)
			record, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, "success", record.Status)
			assert.Equal(t, "gateway-alias", record.RequestedName)
			assert.Equal(t, int64(12), record.Usage.PromptTokens)
			assert.Zero(t, record.Usage.CompletionTokens)
			assert.Equal(t, int64(21_600), record.Receivable.Nanos)
			assert.Equal(t, fixture.sale.ID, record.SalePriceVersionID)
			attempts, total, err := model.ListGatewayAttempts(t.Context(), record.ID, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.Equal(t, fixture.channel.ID, attempts[0].ChannelID)
			assert.Equal(t, int64(12_000), attempts[0].Cost.Nanos)
			assert.Equal(t, fixture.purchase.ID, attempts[0].PurchasePriceVersionID)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, int64(999_978_400), wallet.Balance.Nanos)
			assertTextGatewayLedger(t, *record, 12_000)
		})
	}
}

func TestEmbeddingGatewayRejectsInvalidIngressAndWire(t *testing.T) {
	for _, test := range []struct {
		name, path, body, override string
		kind                       protocol.Type
		provider                   int
		maxDimensions              int64
	}{
		{name: "original dimension capability", body: `{"input":"bird","dimensions":3}`, maxDimensions: 2},
		{name: "final dimension capability", body: `{"input":"bird","dimensions":2}`, override: `{"dimensions":3}`, maxDimensions: 2},
		{name: "native writer has no delivery facts", body: `{"input":"bird"}`, provider: constant.ChannelTypeMokaAI},
		{name: "zero dimensions", body: `{"input":"bird","dimensions":0}`},
		{name: "explicit zero cannot be dropped", body: `{"input":"bird","temperature":0}`, kind: protocol.GeminiEmbed},
		{name: "negative dimensions", body: `{"input":"bird","dimensions":-1}`},
		{name: "oversized dimensions", body: `{"input":"bird","dimensions":2147483648}`},
		{name: "fractional token IDs", body: `{"input":[1,2.5]}`},
		{name: "negative token IDs", body: `{"input":[-1,2]}`},
		{name: "mixed token and text", body: `{"input":["bird",12]}`},
		{name: "empty batch", body: `{"input":[]}`},
		{name: "final dimensions", body: `{"input":"bird","dimensions":2}`, override: `{"dimensions":-1}`},
		{name: "selected model", body: `{"input":"bird"}`, override: `{"model":"other-model"}`},
		{name: "stream override", body: `{"input":"bird"}`, override: `{"stream":true}`},
		{name: "Gemini null batch item", path: "/v1beta/models/gateway-alias:batchEmbedContents", kind: protocol.GeminiEmbed, body: `{"requests":[null]}`},
		{name: "Gemini empty content", path: "/v1beta/models/gateway-alias:embedContent", kind: protocol.GeminiEmbed, body: `{"content":{"parts":[]}}`},
		{name: "Gemini final dimensions", path: "/v1beta/models/gateway-alias:embedContent", kind: protocol.GeminiEmbed, body: `{"content":{"parts":[{"text":"bird"}]}}`, override: `{"outputDimensionality":-1}`},
		{name: "Gemini hint cannot be converted", path: "/v1beta/models/gateway-alias:embedContent", kind: protocol.OpenAIEmbeddings, body: `{"content":{"parts":[{"text":"bird"}]},"taskType":"RETRIEVAL_QUERY"}`},
		{name: "token IDs cannot be converted", kind: protocol.GeminiEmbed, body: `{"input":[1,2]}`},
		{name: "rerank negative chunks", path: "/v1/rerank", kind: protocol.Rerank, body: `{"query":"bird","documents":["bird"],"max_chunk_per_doc":-1}`},
		{name: "rerank final chunks", path: "/v1/rerank", kind: protocol.Rerank, body: `{"query":"bird","documents":["bird"]}`, override: `{"max_chunk_per_doc":2147483648}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
			if test.kind == "" {
				test.kind = protocol.OpenAIEmbeddings
			}
			if test.path == "" {
				test.path = "/v1/embeddings"
			}
			if test.override == "" {
				test.override = `{}`
			}
			require.NoError(t, model.DB.Model(fixture.channel).Updates(map[string]any{"protocol": test.kind, "parameter_overrides": test.override}).Error)
			if test.provider != 0 {
				require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", test.provider).Error)
			}
			if test.maxDimensions != 0 {
				var unified model.Model
				require.NoError(t, model.DB.First(&unified, fixture.channel.ModelID).Error)
				unified.Capabilities.Limits.EmbeddingDimensions = &test.maxDimensions
				require.NoError(t, model.DB.Model(&unified).Select("capabilities").Updates(&unified).Error)
			}
			body := test.body
			if !strings.Contains(test.path, "/models/") {
				body = strings.Replace(body, `{`, `{"model":"gateway-alias",`, 1)
			}
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, test.path, body)
			assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			assert.Zero(t, calls.Load())
			var holds int64
			require.NoError(t, model.DB.Model(&model.GatewayBilling{}).Where("user_id = ?", fixture.user.Id).Count(&holds).Error)
			assert.Zero(t, holds)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, int64(1_000_000_000), wallet.Balance.Nanos)
		})
	}
}

func TestEmbeddingGatewayUsagePresenceAndFailedDelivery(t *testing.T) {
	for _, endpoint := range []struct {
		kind                            protocol.Type
		path, body, result, usage, zero string
	}{
		{protocol.OpenAIEmbeddings, "/v1/embeddings", `{"model":"gateway-alias","input":[[1,2],[3]]}`, `"data":[{"index":0,"embedding":[0.1,0.2]},{"index":1,"embedding":[0.3,0.4]}]`, `"usage":{"prompt_tokens":12}`, `"usage":{"prompt_tokens":0}`},
		{protocol.GeminiEmbed, "/v1beta/models/gateway-alias:embedContent", `{"content":{"parts":[{"text":"A bird"}]}}`, `"embedding":{"values":[0.1,0.2]}`, `"usageMetadata":{"promptTokenCount":12}`, `"usageMetadata":{"promptTokenCount":0}`},
		{protocol.Rerank, "/v1/rerank", `{"model":"gateway-alias","query":"bird","documents":["A bird"]}`, `"results":[{"index":0,"relevance_score":0.9}]`, `"usage":{"total_tokens":12}`, `"usage":{"total_tokens":0}`},
	} {
		for _, outcome := range []string{"reported zero", "missing usage", "empty usage", "structured error", "short write", "full write error", "flush error"} {
			t.Run(string(endpoint.kind)+"/"+outcome, func(t *testing.T) {
				payload := `{` + endpoint.result + `,` + endpoint.usage + `}`
				switch outcome {
				case "reported zero":
					payload = `{` + endpoint.result + `,` + endpoint.zero + `}`
				case "missing usage":
					payload = `{` + endpoint.result + `}`
				case "empty usage":
					usageKey := "usage"
					if endpoint.kind == protocol.GeminiEmbed {
						usageKey = "usageMetadata"
					}
					payload = `{` + endpoint.result + `,"` + usageKey + `":{}}`
				case "structured error":
					payload = `{"error":{"code":"invalid_request","message":"private upstream details"},` + endpoint.usage + `}`
				}
				fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, payload)
				})
				require.NoError(t, model.DB.Model(fixture.channel).Update("protocol", endpoint.kind).Error)
				if endpoint.kind == protocol.GeminiEmbed {
					require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", constant.ChannelTypeGemini).Error)
				}
				request := httptest.NewRequest(http.MethodPost, endpoint.path, strings.NewReader(endpoint.body))
				request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
				request.Header.Set("Content-Type", "application/json")
				writer := httptest.NewRecorder()
				var downstream http.ResponseWriter = writer
				switch outcome {
				case "short write":
					downstream = gatewayPrefixOnlyWriter{ResponseRecorder: writer, nilError: true}
				case "full write error":
					downstream = gatewayPrefixOnlyWriter{ResponseRecorder: writer, full: true}
				case "flush error":
					downstream = gatewayFailingFlushWriter{writer}
				}
				fixture.engine.ServeHTTP(downstream, request)
				record, err := model.GetGatewayRequest(t.Context(), writer.Header().Get(common.RequestIdKey), fixture.user.Id)
				require.NoError(t, err, writer.Body.String())
				attempts, total, err := model.ListGatewayAttempts(t.Context(), record.ID, 0, 10)
				require.NoError(t, err)
				require.EqualValues(t, 1, total)
				failed := outcome == "structured error" || strings.Contains(outcome, "write") || outcome == "flush error"
				if failed {
					assert.Equal(t, "failed", record.Status)
					assert.Zero(t, record.Receivable.Nanos)
					assert.Equal(t, int64(12), attempts[0].Usage.PromptTokens)
					assert.Equal(t, int64(12_000), attempts[0].Cost.Nanos)
				} else {
					assert.Equal(t, http.StatusOK, writer.Code, writer.Body.String())
					assert.Equal(t, "success", record.Status)
					if outcome == "reported zero" {
						assert.Zero(t, record.Usage.PromptTokens)
						assert.Equal(t, "upstream", record.UsageSource)
					} else {
						assert.Positive(t, record.Usage.PromptTokens)
						assert.Equal(t, "estimated", record.UsageSource)
						if endpoint.kind == protocol.OpenAIEmbeddings {
							assert.Equal(t, int64(3), record.Usage.PromptTokens)
						}
					}
					assert.Zero(t, record.Usage.CompletionTokens)
					assert.Equal(t, record.Usage.PromptTokens*1800, record.Receivable.Nanos)
					assert.Equal(t, record.Usage.PromptTokens*1000, attempts[0].Cost.Nanos)
				}
				assert.NotContains(t, writer.Body.String(), "private upstream details")
				assertTextGatewayLedger(t, *record, attempts[0].Cost.Nanos)
			})
		}
	}
}

func TestEmbeddingGatewayRejectsIncompleteOrInvalidVectorsBeforeCharging(t *testing.T) {
	for _, test := range []struct {
		name, path, body, result string
		kind                     protocol.Type
	}{
		{name: "OpenAI missing batch item", body: `{"model":"gateway-alias","input":["bird","tree"]}`, result: `"data":[{"index":0,"embedding":[0.1,0.2]}]`},
		{name: "OpenAI extra vector", body: `{"model":"gateway-alias","input":"bird"}`, result: `"data":[{"index":0,"embedding":[0.1,0.2]},{"index":1,"embedding":[0.3,0.4]}]`},
		{name: "OpenAI wrong dimensions", body: `{"model":"gateway-alias","input":"bird","dimensions":2}`, result: `"data":[{"index":0,"embedding":[0.1]}]`},
		{name: "OpenAI null component", body: `{"model":"gateway-alias","input":"bird"}`, result: `"data":[{"index":0,"embedding":[null,0.2]}]`},
		{name: "base64 wrong dimensions", body: `{"model":"gateway-alias","input":"bird","dimensions":3,"encoding_format":"base64"}`, result: `"data":[{"index":0,"embedding":"zczMPc3MTD4="}]`},
		{name: "OpenAI to Gemini null component", path: "/v1beta/models/gateway-alias:embedContent", body: `{"content":{"parts":[{"text":"bird"}]}}`, result: `"data":[{"index":0,"embedding":[null,0.2]}]`},
		{name: "Gemini missing batch item", kind: protocol.GeminiEmbed, path: "/v1beta/models/gateway-alias:batchEmbedContents", body: `{"requests":[{"content":{"parts":[{"text":"bird"}]}},{"content":{"parts":[{"text":"tree"}]}}]}`, result: `"embeddings":[{"values":[0.1,0.2]}]`},
		{name: "Gemini wrong dimensions", kind: protocol.GeminiEmbed, path: "/v1beta/models/gateway-alias:embedContent", body: `{"content":{"parts":[{"text":"bird"}]} ,"outputDimensionality":2}`, result: `"embedding":{"values":[0.1]}`},
		{name: "Gemini null component", kind: protocol.GeminiEmbed, path: "/v1beta/models/gateway-alias:embedContent", body: `{"content":{"parts":[{"text":"bird"}]}}`, result: `"embedding":{"values":[null,0.2]}`},
		{name: "Gemini to OpenAI missing batch item", kind: protocol.GeminiEmbed, body: `{"model":"gateway-alias","input":["bird","tree"]}`, result: `"embeddings":[{"values":[0.1,0.2]}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			usage := `"usage":{"prompt_tokens":12}`
			if test.kind == protocol.GeminiEmbed {
				usage = `"usageMetadata":{"promptTokenCount":12}`
			}
			var calls atomic.Int32
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{`+test.result+`,`+usage+`}`)
			})
			t.Setenv("GATEWAY_MAX_ATTEMPTS", "1")
			if test.kind == "" {
				test.kind = protocol.OpenAIEmbeddings
			}
			if test.path == "" {
				test.path = "/v1/embeddings"
			}
			require.NoError(t, model.DB.Model(fixture.channel).Update("protocol", test.kind).Error)
			if test.kind == protocol.GeminiEmbed {
				require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", constant.ChannelTypeGemini).Error)
			}
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, test.path, test.body)
			assert.GreaterOrEqual(t, response.Code, http.StatusBadRequest, response.Body.String())
			assert.EqualValues(t, 1, calls.Load())
			record, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, "failed", record.Status)
			assert.Zero(t, record.Receivable.Nanos)
			attempts, total, err := model.ListGatewayAttempts(t.Context(), record.ID, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.Equal(t, int64(12), attempts[0].Usage.PromptTokens)
			assert.Equal(t, int64(12_000), attempts[0].Cost.Nanos)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, int64(1_000_000_000), wallet.Balance.Nanos)
			assertTextGatewayLedger(t, *record, 12_000)
		})
	}
}

func TestEmbeddingGatewayFreezesOriginalAndEffectivePriceProbes(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Dimensions int `json:"dimensions"`
		}
		assert.NoError(t, common.DecodeJson(r.Body, &body))
		assert.Equal(t, 3, body.Dimensions)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[0.1,0.2,0.3]}],"usage":{"prompt_tokens":12}}`)
	})
	require.NoError(t, model.DB.Model(fixture.channel).Updates(map[string]any{"protocol": protocol.OpenAIEmbeddings, "parameter_overrides": `{"dimensions":3}`}).Error)
	for _, price := range []*model.PriceVersion{
		{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: `param("dimensions") == 2 ? tier("original", p * 2) : tier("other", p * 3)`, EffectiveAt: fixture.fake.Now().Unix()},
		{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: `param("dimensions") == 3 ? tier("wire", p) : tier("other", p * 0.5)`, EffectiveAt: fixture.fake.Now().Unix()},
	} {
		require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
	}
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/embeddings", `{"model":"gateway-alias","input":"bird","dimensions":2}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	record, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
	require.NoError(t, err)
	assert.Equal(t, int64(21_600), record.Receivable.Nanos)
	attempts, total, err := model.ListGatewayAttempts(t.Context(), record.ID, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	assert.Equal(t, int64(12_000), attempts[0].Cost.Nanos)
	assertTextGatewayLedger(t, *record, 12_000)
}

func TestEmbeddingGatewayCancellationStopsProviderAndReleasesReservation(t *testing.T) {
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer close(release)
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-release:
		}
	})
	require.NoError(t, model.DB.Model(fixture.channel).Update("protocol", protocol.OpenAIEmbeddings).Error)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"gateway-alias","input":"bird"}`)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
	request.Header.Set("Content-Type", "application/json")
	writer := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); fixture.engine.ServeHTTP(writer, request) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Provider was not contacted")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("Provider remained active after client cancellation")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Cancelled request did not settle")
	}
	record, err := model.GetGatewayRequest(t.Context(), writer.Header().Get(common.RequestIdKey), fixture.user.Id)
	require.NoError(t, err)
	assert.Equal(t, "cancelled", record.Status)
	assert.Zero(t, record.Receivable.Nanos)
	wallet, err := model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	assert.Equal(t, int64(1_000_000_000), wallet.Balance.Nanos)
	assertTextGatewayLedger(t, *record, 0)
}

func TestEmbeddingGatewayAuthorizesBeforeParsingEveryEntrypoint(t *testing.T) {
	for _, path := range []string{"/v1/embeddings", "/v1/engines/gateway-alias/embeddings", "/v1/rerank", "/v1/models/gateway-alias:embedContent", "/v1beta/models/gateway-alias:embedContent", "/v1beta/models/gateway-alias:batchEmbedContents"} {
		t.Run(path, func(t *testing.T) {
			var calls atomic.Int32
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
			body := `{"model":"gateway-alias","input":null,"requests":[null]}`
			for _, status := range []int{common.TokenStatusDisabled, common.TokenStatusEnabled} {
				expiry := int64(-1)
				if status == common.TokenStatusEnabled {
					expiry = 1
				}
				require.NoError(t, model.DB.Model(fixture.token).Updates(map[string]any{"status": status, "expired_time": expiry}).Error)
				response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, path, body)
				assert.Equal(t, http.StatusUnauthorized, response.Code, response.Body.String())
			}
			require.NoError(t, model.DB.Model(fixture.token).Updates(map[string]any{"status": common.TokenStatusEnabled, "expired_time": -1}).Error)
			fixture.offering.Visibility, fixture.offering.Groups = "groups", []string{"paid"}
			require.NoError(t, model.SaveModelOffering(t.Context(), fixture.offering, "save"))
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, path, body)
			assert.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
			assert.Zero(t, calls.Load())
			var holds int64
			require.NoError(t, model.DB.Model(&model.GatewayBilling{}).Where("user_id = ?", fixture.user.Id).Count(&holds).Error)
			assert.Zero(t, holds)
		})
	}
}

func TestEmbeddingGatewayFiltersProtocolMismatchBeforeDispatch(t *testing.T) {
	for _, kind := range []protocol.Type{protocol.OpenAIEmbeddings, protocol.Rerank} {
		t.Run(string(kind), func(t *testing.T) {
			var rejectedCalls atomic.Int32
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { rejectedCalls.Add(1) })
			compatible := addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if kind == protocol.Rerank {
					_, _ = io.WriteString(w, `{"results":[{"index":0,"relevance_score":0.9}],"usage":{"total_tokens":12}}`)
				} else {
					_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":12}}`)
				}
			})
			require.NoError(t, model.DB.Model(compatible).Update("protocol", kind).Error)
			path, body := "/v1/embeddings", `{"model":"gateway-alias","input":"bird"}`
			if kind == protocol.Rerank {
				path, body = "/v1/rerank", `{"model":"gateway-alias","query":"bird","documents":["bird"]}`
			}
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, path, body)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.Zero(t, rejectedCalls.Load())
			attempts, total, err := model.ListGatewayAttempts(t.Context(), response.Header().Get(common.RequestIdKey), 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.Equal(t, compatible.ID, attempts[0].ChannelID)
			diagnostics, err := common.Marshal(attempts[0].RouteDiagnostics)
			require.NoError(t, err)
			assert.Contains(t, string(diagnostics), `"protocol":1`)
		})
	}
}
