package router

import (
	"fmt"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarginBulkDraftBindingHTTP(t *testing.T) {
	for _, change := range []string{"draft", "stored channel overrides"} {
		t.Run(change, func(t *testing.T) {
			fixture := newTextGatewayFixture(t, func(http.ResponseWriter, *http.Request) {})
			require.NoError(t, model.DB.AutoMigrate(&model.ChannelProbeResult{}))
			manager := createWalletTestAccessToken(t, fixture.sale.CreatedBy, "pricing:write", "channel:write", "channel:read", "channel:operate")
			source := fmt.Sprintf(`{"rows":[{"resource":"channels","action":"create","data":{"provider_id":%d,"model_id":%d,"protocol":"openai_chat","upstream_model_name":"extra-channel","parameter_overrides":{"temperature":0.1}}},{"resource":"channels","action":"confirm","data":{"id":%d}},{"resource":"price-versions","action":"create","data":{"owner_type":"channel","owner_id":%d,"currency":"USD","expression":"tier(\"purchase\",fixed(2))"}},{"resource":"price-versions","action":"create","data":{"owner_type":"model_offering","owner_id":%d,"currency":"USD","expression":"tier(\"sale\",fixed(1))"}}]}`, fixture.channel.ProviderID, fixture.channel.ModelID, fixture.channel.ID, fixture.channel.ID, fixture.offering.ID)
			preview := adminSecurityRequest(fixture.engine, manager, http.MethodPost, "/api/admin/v1/bulk-import?dry_run=true", source)
			require.Equal(t, http.StatusOK, preview.Code, preview.Body.String())
			var reply struct {
				Data struct {
					MarginCheck model.PriceMarginReport `json:"margin_check"`
				} `json:"data"`
			}
			require.NoError(t, common.Unmarshal(preview.Body.Bytes(), &reply))
			approval := reply.Data.MarginCheck
			if change == "draft" {
				source = strings.Replace(source, `"temperature":0.1`, `"temperature":0.2`, 1)
			} else {
				updated := adminSecurityRequest(fixture.engine, manager, http.MethodPatch, fmt.Sprintf("/api/admin/v1/channels/%d", fixture.channel.ID), `{"parameter_overrides":{"temperature":0.2}}`)
				require.Equal(t, http.StatusOK, updated.Code, updated.Body.String())
			}
			request := httptest.NewRequest(http.MethodPost, "/api/admin/v1/bulk-import", strings.NewReader(source))
			request.Header.Set("Authorization", "Bearer "+manager)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Margin-Confirmation", approval.Revision)
			request.Header.Set("X-Margin-Effective-At", strconv.FormatInt(approval.EffectiveAt, 10))
			rejected := httptest.NewRecorder()
			fixture.engine.ServeHTTP(rejected, request)
			require.Equal(t, http.StatusConflict, rejected.Code, rejected.Body.String())
			var refreshed struct {
				Data model.PriceMarginReport `json:"data"`
			}
			require.NoError(t, common.Unmarshal(rejected.Body.Bytes(), &refreshed))
			assert.NotEqual(t, approval.Revision, refreshed.Data.Revision)
			listed := adminSecurityRequest(fixture.engine, manager, http.MethodGet, "/api/admin/v1/channels", "")
			require.Equal(t, http.StatusOK, listed.Code, listed.Body.String())
			var channels struct {
				Data struct {
					Total int `json:"total"`
				} `json:"data"`
			}
			require.NoError(t, common.Unmarshal(listed.Body.Bytes(), &channels))
			assert.Equal(t, 1, channels.Data.Total, "rejected batch must not create its extra Channel")
		})
	}
}

func TestMarginBulkConfirmationHTTP(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, err := fmt.Fprint(w, `{"choices":[{"message":{"content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
		assert.NoError(t, err)
	})
	manager := createWalletTestAccessToken(t, fixture.sale.CreatedBy, "pricing:write", "channel:write", "channel:operate")
	source := fmt.Sprintf(`{"rows":[{"resource":"channels","action":"confirm","data":{"id":%d}},{"resource":"price-versions","action":"create","data":{"owner_type":"channel","owner_id":%d,"currency":"USD","expression":"tier(\"purchase\",fixed(2))"}},{"resource":"price-versions","action":"create","data":{"owner_type":"model_offering","owner_id":%d,"currency":"USD","expression":"tier(\"sale\",fixed(1))"}}]}`, fixture.channel.ID, fixture.channel.ID, fixture.offering.ID)
	preview := adminSecurityRequest(fixture.engine, manager, http.MethodPost, "/api/admin/v1/bulk-import?dry_run=true", source)
	require.Equal(t, http.StatusOK, preview.Code, preview.Body.String())
	var reply struct {
		Data struct {
			Valid       bool                     `json:"valid"`
			MarginCheck *model.PriceMarginReport `json:"margin_check"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(preview.Body.Bytes(), &reply))
	require.True(t, reply.Data.Valid)
	require.NotNil(t, reply.Data.MarginCheck)
	require.True(t, reply.Data.MarginCheck.RequiresConfirmation)
	rejected := adminSecurityRequest(fixture.engine, manager, http.MethodPost, "/api/admin/v1/bulk-import", source)
	require.Equal(t, http.StatusConflict, rejected.Code, rejected.Body.String())
	fixture.fake.Advance(3 * time.Second)
	traffic := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/"+"chat/"+"completions", `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`)
	require.Equal(t, http.StatusOK, traffic.Code, traffic.Body.String())
	request := httptest.NewRequest(http.MethodPost, "/api/admin/v1/bulk-import", strings.NewReader(source))
	request.Header.Set("Authorization", "Bearer "+manager)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Margin-Confirmation", reply.Data.MarginCheck.Revision)
	request.Header.Set("X-Margin-Effective-At", strconv.FormatInt(reply.Data.MarginCheck.EffectiveAt, 10))
	response := httptest.NewRecorder()
	fixture.engine.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var committed struct {
		Data struct {
			Committed bool `json:"committed"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &committed))
	assert.True(t, committed.Data.Committed)
}

func TestMarginPriceConfirmationContextHTTP(t *testing.T) {
	for _, change := range []string{"new usage", "new current purchase price", "new planned purchase price", "sale discount", "special sale discount", "precise sale discount"} {
		t.Run(change, func(t *testing.T) {
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, err := fmt.Fprint(w, `{"choices":[{"message":{"content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
				assert.NoError(t, err)
			})
			if change == "special sale discount" {
				common.OptionMapRWMutex.Lock()
				common.OptionMap["GroupRatio"] = `{"default":0.9,"excluded":0.1}`
				common.OptionMap["GroupGroupRatio"] = `{"default":{"default":0.5}}`
				common.OptionMapRWMutex.Unlock()
				fixture.offering.Visibility, fixture.offering.Groups = "groups", []string{"default"}
				require.NoError(t, model.SaveModelOffering(t.Context(), fixture.offering, "save"))
			}
			if change == "precise sale discount" {
				common.OptionMapRWMutex.Lock()
				common.OptionMap["GroupRatio"] = `{"default":0.900000000000000001}`
				common.OptionMapRWMutex.Unlock()
			}
			body := `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`
			traffic := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/"+"chat/"+"completions", body)
			require.Equal(t, http.StatusOK, traffic.Code, traffic.Body.String())
			manager := createWalletTestAccessToken(t, fixture.sale.CreatedBy, "pricing:read", "pricing:write")
			expression := `tier("sale",fixed(0.000001))`
			if strings.Contains(change, "sale discount") {
				expression = `p * 1.05 + c * 2.1`
			}
			draft := map[string]any{"owner_type": "model_offering", "owner_id": fixture.offering.ID, "currency": "USD", "expression": expression}
			if change == "new planned purchase price" {
				draft["effective_at"] = clock.Now().Unix() + 120
			}
			encoded, err := common.Marshal(draft)
			require.NoError(t, err)
			warning := adminSecurityRequest(fixture.engine, manager, http.MethodPost, "/api/admin/v1/price-versions", string(encoded))
			require.Equal(t, http.StatusConflict, warning.Code, warning.Body.String())
			var initial struct {
				Data model.PriceMarginReport `json:"data"`
			}
			require.NoError(t, common.Unmarshal(warning.Body.Bytes(), &initial))
			require.Len(t, initial.Data.Estimates, 1)
			if strings.Contains(change, "sale discount") {
				estimate := initial.Data.Estimates[0]
				require.NotNil(t, estimate.Revenue)
				require.NotNil(t, estimate.Cost)
				require.NotNil(t, estimate.Margin)
				revenue, margin := int64(18900), int64(-1100)
				if change == "special sale discount" {
					revenue, margin = 10500, -9500
				}
				if change == "precise sale discount" {
					revenue = 18901
				}
				assert.Equal(t, revenue, estimate.Revenue.Nanos)
				assert.EqualValues(t, 20000, estimate.Cost.Nanos)
				assert.Equal(t, margin, estimate.Margin.Nanos)
				return
			}
			fixture.fake.Advance(2 * time.Second)
			var purchase model.PriceVersion
			if change == "new usage" {
				traffic = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/"+"chat/"+"completions", body)
				require.Equal(t, http.StatusOK, traffic.Code, traffic.Body.String())
			} else {
				purchaseTime := clock.Now().Unix()
				if change == "new planned purchase price" {
					purchaseTime += 30
				}
				updated := adminSecurityRequest(fixture.engine, manager, http.MethodPost, "/api/admin/v1/price-versions", fmt.Sprintf(`{"owner_type":"channel","owner_id":%d,"currency":"USD","expression":"p * 1.1 + c * 2.2","effective_at":%d}`, fixture.channel.ID, purchaseTime))
				require.Equal(t, http.StatusOK, updated.Code, updated.Body.String())
				var reply struct {
					Data model.PriceVersion `json:"data"`
				}
				require.NoError(t, common.Unmarshal(updated.Body.Bytes(), &reply))
				purchase = reply.Data
			}
			draft["effective_at"], draft["margin_confirmation"] = initial.Data.EffectiveAt, initial.Data.Revision
			encoded, err = common.Marshal(draft)
			require.NoError(t, err)
			confirmed := adminSecurityRequest(fixture.engine, manager, http.MethodPost, "/api/admin/v1/price-versions", string(encoded))
			if change == "new usage" {
				require.Equal(t, http.StatusOK, confirmed.Code, confirmed.Body.String())
				return
			}
			require.Equal(t, http.StatusConflict, confirmed.Code, confirmed.Body.String())
			var refreshed struct {
				Data model.PriceMarginReport `json:"data"`
			}
			require.NoError(t, common.Unmarshal(confirmed.Body.Bytes(), &refreshed))
			require.Len(t, refreshed.Data.Estimates, 1)
			assert.NotEqual(t, initial.Data.Revision, refreshed.Data.Revision)
			assert.Equal(t, purchase.ID, refreshed.Data.Estimates[0].PurchasePriceVersionID)
			require.NotNil(t, refreshed.Data.Estimates[0].Cost)
			assert.EqualValues(t, 22000, refreshed.Data.Estimates[0].Cost.Nanos)
		})
	}
}

func TestMarginMixedCurrencyHTTP(t *testing.T) {
	engine, actor, token := newAdminTestServer(t)
	t.Setenv("GATEWAY_SETTLEMENT_JOURNAL_DIR", t.TempDir())
	require.NoError(t, model.MigrateGatewayBilling(model.DB))
	zero := money.Amount{Currency: "USD"}
	request := model.GatewayRequest{ID: "mixed-currency-margin", UserID: actor.Id, ModelOfferingID: 17, SaleCurrency: "USD", SaleFXRate: "1", StartedAt: clock.Now().UnixMilli(), Status: "success", Receivable: zero}
	require.NoError(t, model.ReserveGateway(&request, zero, zero))
	require.NoError(t, model.PrepareGatewaySettlement(model.GatewaySettlement{Request: request, SaleSource: zero}))
	_, err := model.SettleGateway(request.ID)
	require.NoError(t, err)
	_, err = model.PostLedgerEntries(model.LedgerPosting{Account: model.PlatformRevenueAccount("EUR"), Type: model.LedgerEntryCharge, Amount: money.Amount{Nanos: 1, Currency: "EUR"}, RequestId: request.ID})
	require.NoError(t, err)
	response := adminSecurityRequest(engine, token, http.MethodGet, "/api/admin/v1/reports/margin?group_by=model_offering", "")
	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "Cannot combine amounts in different currencies")
}

func TestMarginCacheAndChoiceWorkloadHTTP(t *testing.T) {
	fixture := newTextGatewayFixture(t, func(http.ResponseWriter, *http.Request) {})
	common.OptionMapRWMutex.Lock()
	common.OptionMap["GroupRatio"] = `{"default":1}`
	common.OptionMapRWMutex.Unlock()
	expression := "p + cr * 2 + cc * 3 + cc1h * 4 + c * 5"
	for _, price := range []*model.PriceVersion{
		{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: expression, EffectiveAt: clock.Now().Unix()},
		{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: expression, EffectiveAt: clock.Now().Unix()},
	} {
		require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
	}
	for index, tc := range []struct{ choices, prompt, read, short, long, delivered, generated int }{
		{2, 10, 20, 30, 40, 200, 400},
		{1, 30, 40, 50, 60, 50, 100},
	} {
		zero := money.Amount{Currency: "USD"}
		request := model.GatewayRequest{ID: fmt.Sprintf("cache-choice-%d", index), UserID: fixture.user.Id, ModelOfferingID: fixture.offering.ID, ChoiceCount: tc.choices, StartedAt: clock.Now().UnixMilli(), Status: "partial", SaleCurrency: "USD", SaleFXRate: "1", Receivable: zero}
		require.NoError(t, model.ReserveGateway(&request, zero, zero))
		saleUsage := &dto.Usage{UsageSemantic: "anthropic", PromptTokens: tc.prompt, CompletionTokens: tc.delivered, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: tc.read}, ClaudeCacheCreation5mTokens: tc.short, ClaudeCacheCreation1hTokens: tc.long}
		sourceUsage := *saleUsage
		sourceUsage.CompletionTokens = tc.generated
		attempt := model.GatewayAttempt{ID: request.ID + "-attempt", RequestID: request.ID, ChannelID: fixture.channel.ID, ProviderID: fixture.channel.ProviderID, Status: "failed", Cost: zero, ConvertedCost: zero, AccountingFXRate: "1"}
		require.NoError(t, model.PrepareGatewaySettlement(model.GatewaySettlement{Request: request, Attempts: []model.GatewayAttempt{attempt}, SaleSource: zero, SaleUsage: saleUsage, AttemptUsage: map[string]*dto.Usage{attempt.ID: &sourceUsage}}))
		_, err := model.SettleGateway(request.ID)
		require.NoError(t, err)
	}
	manager := createWalletTestAccessToken(t, fixture.sale.CreatedBy, "pricing:read")
	response := adminSecurityRequest(fixture.engine, manager, http.MethodGet, "/api/admin/v1/price-versions/margins", "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var reply struct {
		Data model.PriceMarginReport `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &reply))
	require.Len(t, reply.Data.Estimates, 1)
	estimate := reply.Data.Estimates[0]
	require.NotNil(t, estimate.Revenue)
	require.NotNil(t, estimate.Cost)
	assert.EqualValues(t, 775000, estimate.Revenue.Nanos)
	assert.EqualValues(t, 1150000, estimate.Cost.Nanos)
	assert.True(t, estimate.Negative)
}

func TestMarginLegacyUsageHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, expression           string
		prompt, cached, completion int64
		unknown                    bool
	}{
		{"basic input", "p * 2", 1000000, 0, 0, false},
		{"unknown cached input", "p * 2", 1000000, 100, 0, true},
		{"known output despite cached input", "c * 2", 200, 100, 1000000, false},
		{"known cache reads", "cr * 20000", 200, 100, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newTextGatewayFixture(t, func(http.ResponseWriter, *http.Request) {})
			common.OptionMapRWMutex.Lock()
			common.OptionMap["GroupRatio"] = `{"default":1}`
			common.OptionMapRWMutex.Unlock()
			sale := &model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: `tier("sale",fixed(1))`, EffectiveAt: clock.Now().Unix()}
			require.NoError(t, model.CreatePriceVersion(t.Context(), sale, fixture.sale.CreatedBy))
			purchase := &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: tc.expression, EffectiveAt: clock.Now().Unix()}
			require.NoError(t, model.CreatePriceVersion(t.Context(), purchase, fixture.sale.CreatedBy))
			zero := money.Amount{Currency: "USD"}
			usage := model.GatewayUsage{PromptTokens: tc.prompt, CompletionTokens: tc.completion, TotalTokens: tc.prompt + tc.completion, CachedTokens: tc.cached}
			request := model.GatewayRequest{ID: "legacy-normal-success", UserID: fixture.user.Id, ModelOfferingID: fixture.offering.ID, ChoiceCount: 1, StartedAt: clock.Now().Add(-time.Hour).UnixMilli(), Status: "success", Usage: usage, SaleCurrency: "USD", SaleFXRate: "1", Receivable: money.Amount{Nanos: 1000000000, Currency: "USD"}}
			require.NoError(t, model.ReserveGateway(&request, zero, zero))
			attempt := model.GatewayAttempt{ID: "legacy-normal-attempt", RequestID: request.ID, ChannelID: fixture.channel.ID, ProviderID: fixture.channel.ProviderID, Status: "success", Usage: usage, Cost: money.Amount{Nanos: 2000000000, Currency: "USD"}, ConvertedCost: money.Amount{Nanos: 2000000000, Currency: "USD"}, AccountingFXRate: "1"}
			// This is the pre-#51 immutable intent shape: normal successes did
			// not include SaleUsage or AttemptUsage DTO snapshots.
			require.NoError(t, model.PrepareGatewaySettlement(model.GatewaySettlement{Request: request, Attempts: []model.GatewayAttempt{attempt}, SaleSource: request.Receivable}))
			_, err := model.SettleGateway(request.ID)
			require.NoError(t, err)
			fixture.fake.Advance(time.Second)
			manager := createWalletTestAccessToken(t, fixture.sale.CreatedBy, "pricing:read", "pricing:write")
			draft := fmt.Sprintf(`{"owner_type":"channel","owner_id":%d,"currency":"USD","expression":%q}`, fixture.channel.ID, tc.expression)
			response := adminSecurityRequest(fixture.engine, manager, http.MethodPost, "/api/admin/v1/price-versions", draft)
			if tc.unknown {
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				var reply struct {
					MarginCheck model.PriceMarginReport `json:"margin_check"`
				}
				require.NoError(t, common.Unmarshal(response.Body.Bytes(), &reply))
				require.Len(t, reply.MarginCheck.Estimates, 1)
				assert.Equal(t, "usage_details_unavailable", reply.MarginCheck.Estimates[0].Status)
				assert.Nil(t, reply.MarginCheck.Estimates[0].Cost)
				return
			}
			require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
			var reply struct {
				Data model.PriceMarginReport `json:"data"`
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &reply))
			require.Len(t, reply.Data.Estimates, 1)
			estimate := reply.Data.Estimates[0]
			require.NotNil(t, estimate.Revenue)
			require.NotNil(t, estimate.Cost)
			assert.EqualValues(t, 1000000000, estimate.Revenue.Nanos)
			assert.EqualValues(t, 2000000000, estimate.Cost.Nanos)
			assert.True(t, estimate.Negative)
		})
	}
}

func TestMarginOfferingWorkloadHTTP(t *testing.T) {
	calls := 0
	fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		prompt := 10000
		if calls > 0 {
			prompt = 100
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, err := fmt.Fprintf(w, `{"choices":[{"message":{"content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":0,"total_tokens":%d}}`, prompt, prompt)
		assert.NoError(t, err)
	})
	common.OptionMapRWMutex.Lock()
	common.OptionMap["GroupRatio"] = `{"default":1}`
	common.OptionMapRWMutex.Unlock()
	sale := &model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: "p * 10", EffectiveAt: clock.Now().Unix()}
	require.NoError(t, model.CreatePriceVersion(t.Context(), sale, fixture.sale.CreatedBy))
	purchase := &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: "p * 0.1", EffectiveAt: clock.Now().Unix()}
	require.NoError(t, model.CreatePriceVersion(t.Context(), purchase, fixture.sale.CreatedBy))
	body := `{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`
	first := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/"+"chat/"+"completions", body)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	second := *fixture.channel
	second.ID = 0
	second.UpstreamModelName = "second-workload"
	require.NoError(t, model.DB.Create(&second).Error)
	purchase = &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: second.ID, Currency: "USD", Expression: "p * 2.5", EffectiveAt: clock.Now().Unix()}
	require.NoError(t, model.CreatePriceVersion(t.Context(), purchase, fixture.sale.CreatedBy))
	require.NoError(t, model.DB.Model(fixture.channel).Update("status", model.ChannelDisabled).Error)
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/"+"chat/"+"completions", body)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, 2, calls)
	fixture.fake.Advance(time.Second)
	manager := createWalletTestAccessToken(t, fixture.sale.CreatedBy, "pricing:read", "pricing:write")
	draft := fmt.Sprintf(`{"owner_type":"model_offering","owner_id":%d,"currency":"USD","expression":"p * 1.5"}`, fixture.offering.ID)
	denied := adminSecurityRequest(fixture.engine, manager, http.MethodPost, "/api/admin/v1/price-versions", draft)
	assert.Equal(t, http.StatusConflict, denied.Code, denied.Body.String())
	var reply struct {
		Data model.PriceMarginReport `json:"data"`
	}
	require.NoError(t, common.Unmarshal(denied.Body.Bytes(), &reply))
	require.Len(t, reply.Data.Estimates, 2)
	for _, estimate := range reply.Data.Estimates {
		require.NotNil(t, estimate.Revenue)
		require.NotNil(t, estimate.Cost)
		assert.EqualValues(t, 7575000, estimate.Revenue.Nanos)
		if estimate.ChannelID == second.ID {
			assert.EqualValues(t, 12625000, estimate.Cost.Nanos)
			assert.True(t, estimate.Negative)
		}
	}
}

func TestMarginGuardUsesMediaQuantities(t *testing.T) {
	for _, tc := range []struct {
		name, expression, batch                  string
		images, unknown, sale, zero, fx, partial bool
		residual, legacy                         bool
	}{
		{name: "batch per-image sale and token purchase", batch: "per-image-sale", expression: `tier("tokens",c*100)`},
		{name: "batch partial sale and generated token purchase", batch: "per-image-sale", partial: true, expression: `tier("tokens",c*100)`},
		{name: "batch per-image sale and image-token purchase", batch: "per-image-sale", expression: `tier("tokens",img_o*100)`},
		{name: "batch fixed-request sale and token purchase", batch: "fixed-sale", expression: `tier("tokens",c*100)`},
		{name: "batch token sale and per-image purchase", batch: "token-sale", sale: true, expression: `tier("tokens",c*100)`},
		{name: "legacy batch per-image sale and token purchase", batch: "per-image-sale", legacy: true, expression: `tier("tokens",c*100)`},
		{name: "normalized residual sale", sale: true, residual: true, expression: `image_count > 0 ? tier("text",c*7) : tier("image",img_o*7)`},
		{name: "legacy normalized residual sale", sale: true, residual: true, legacy: true, expression: `image_count > 0 ? tier("text",c*7) : tier("image",img_o*7)`},
		{name: "image quantity price", images: true, expression: `tier("image",fixed(0.05))*image_count`},
		{name: "separate delivered and generated image counts", images: true, partial: true, expression: `tier("image",fixed(0.05))*image_count`},
		{name: "image quantity FX", images: true, fx: true},
		{name: "unknown procurement", unknown: true, expression: `tier("tokens",img_o*7)`},
		{name: "unknown base procurement", unknown: true, expression: `tier("tokens",c*7)`},
		{name: "unknown sale", unknown: true, sale: true, expression: `tier("tokens",img_o*7)`},
		{name: "reported zero", zero: true, expression: `tier("tokens",img_o*7)`},
		{name: "unused unknown", expression: `true ? tier("request",fixed(0.02)) : tier("tokens",img_o*7)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newMediaGatewayFixture(t, protocol.OpenAIImages, func(w http.ResponseWriter, r *http.Request) {
				var input struct {
					N int `json:"n"`
				}
				require.NoError(t, common.DecodeJson(r.Body, &input))
				data := make([]string, input.N)
				for i := range data {
					data[i] = `{"url":"https://image.invalid/one"}`
				}
				usage := ""
				if tc.zero {
					usage = `,"usage":{"input_tokens":0,"output_tokens":0}`
				} else if tc.batch != "" {
					usage = fmt.Sprintf(`,"usage":{"input_tokens":0,"output_tokens":%d}`, input.N*1000)
				}
				w.Header().Set("Content-Type", "application/json")
				_, err := fmt.Fprintf(w, `{"data":[%s]%s}`, strings.Join(data, ","), usage)
				require.NoError(t, err)
			})
			common.OptionMapRWMutex.Lock()
			common.OptionMap["GroupRatio"] = `{"default":1}`
			common.OptionMapRWMutex.Unlock()
			currency := money.Currency("USD")
			if tc.fx {
				currency = "EUR"
				_, err := model.SaveFXRate(t.Context(), "EUR", "USD", "1", "0", fixture.sale.CreatedBy)
				require.NoError(t, err)
			}
			saleExpression, purchaseExpression := `tier("request",fixed(0.04))`, `tier("request",fixed(0.02))`
			if tc.images {
				saleExpression += `*image_count`
				purchaseExpression += `*image_count`
			}
			switch tc.batch {
			case "per-image-sale":
				saleExpression = `tier("image",fixed(0.06))*image_count`
			case "fixed-sale":
				saleExpression = `tier("request",fixed(0.12))`
			case "token-sale":
				saleExpression = `tier("request",fixed(0.2))`
				purchaseExpression = `tier("image",fixed(0.06))*image_count`
			}
			fixture.fake.Advance(time.Second)
			for _, price := range []*model.PriceVersion{{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: saleExpression, EffectiveAt: clock.Now().Unix()}, {OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: currency, Expression: purchaseExpression, EffectiveAt: clock.Now().Unix()}} {
				require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
			}
			for count := 1; count <= 2; count++ {
				if tc.batch != "" && count == 1 {
					continue
				}
				body := fmt.Sprintf(`{"model":"gateway-alias","prompt":"draw","n":%d}`, count)
				var response *httptest.ResponseRecorder
				if tc.partial && count == 2 {
					request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(fmt.Sprintf(`{"model":"gateway-alias","prompt":"draw","n":%d,"stream":true}`, count)))
					request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
					request.Header.Set("Content-Type", "application/json")
					response = httptest.NewRecorder()
					fixture.engine.ServeHTTP(&mediaFrameFailureWriter{ResponseRecorder: response, failFrame: 2}, request)
				} else {
					response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/images/generations", body)
				}
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				fixture.fake.Advance(time.Second)
			}
			if tc.legacy {
				var rows []model.GatewayBilling
				require.NoError(t, model.DB.Find(&rows).Error)
				expectedHistory := 2
				if tc.batch != "" {
					expectedHistory = 1
				}
				require.Len(t, rows, expectedHistory)
				for _, row := range rows {
					var intent model.GatewaySettlement
					require.NoError(t, common.UnmarshalJsonStr(row.Settlement, &intent))
					intent.SaleUsage, intent.AttemptUsage = nil, nil
					data, err := common.Marshal(intent)
					require.NoError(t, err)
					require.NoError(t, model.DB.Model(&row).Update("settlement", string(data)).Error)
				}
			}
			manager := createWalletTestAccessToken(t, fixture.sale.CreatedBy, "pricing:read", "pricing:write")
			owner, id := "channel", fixture.channel.ID
			if tc.sale {
				owner, id = "model_offering", fixture.offering.ID
			}
			path, method := "/api/admin/v1/price-versions", http.MethodPost
			draft := fmt.Sprintf(`{"owner_type":%q,"owner_id":%d,"currency":"USD","expression":%q}`, owner, id, tc.expression)
			if tc.fx {
				path, method = "/api/admin/v1/fx-rates", http.MethodPut
				draft = `{"from_currency":"EUR","to_currency":"USD","rate":"3","buffer_rate":"0"}`
			}
			response := adminSecurityRequest(fixture.engine, manager, method, path, draft)
			var reply struct {
				Data        model.PriceMarginReport `json:"data"`
				MarginCheck model.PriceMarginReport `json:"margin_check"`
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &reply))
			if tc.batch != "" {
				report := reply.Data
				revenue, cost := int64(120_000_000), int64(200_000_000)
				if tc.partial {
					revenue = 60_000_000
				}
				if tc.sale {
					require.Equal(t, http.StatusOK, response.Code, response.Body.String())
					report = reply.MarginCheck
					revenue, cost = 200_000_000, 120_000_000
				} else {
					require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
				}
				require.Len(t, report.Estimates, 1)
				estimate := report.Estimates[0]
				assert.Equal(t, "estimated", estimate.Status)
				require.NotNil(t, estimate.Revenue)
				require.NotNil(t, estimate.Cost)
				assert.Equal(t, revenue, estimate.Revenue.Nanos)
				assert.Equal(t, cost, estimate.Cost.Nanos)
				assert.Equal(t, !tc.sale, estimate.Negative)
				assert.Equal(t, !tc.sale, report.RequiresConfirmation)
				return
			}
			if tc.residual {
				require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
				require.True(t, reply.Data.RequiresConfirmation)
				require.Len(t, reply.Data.Estimates, 1)
				estimate := reply.Data.Estimates[0]
				assert.Equal(t, "estimated", estimate.Status)
				require.NotNil(t, estimate.Revenue)
				require.NotNil(t, estimate.Cost)
				assert.Zero(t, estimate.Revenue.Nanos)
				assert.EqualValues(t, 20_000_000, estimate.Cost.Nanos)
				assert.True(t, estimate.Negative)
				return
			}
			if tc.images {
				require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
				require.True(t, reply.Data.RequiresConfirmation)
				require.Len(t, reply.Data.Estimates, 1)
				estimate := reply.Data.Estimates[0]
				require.NotNil(t, estimate.Revenue)
				require.NotNil(t, estimate.Cost)
				revenue := int64(60_000_000)
				if tc.partial {
					revenue = 40_000_000
				}
				assert.EqualValues(t, revenue, estimate.Revenue.Nanos)
				cost := int64(75_000_000)
				if tc.fx {
					cost = 90_000_000
				}
				assert.EqualValues(t, cost, estimate.Cost.Nanos)
				assert.True(t, estimate.Negative)
				return
			}
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.Len(t, reply.MarginCheck.Estimates, 1)
			estimate := reply.MarginCheck.Estimates[0]
			if tc.unknown {
				assert.Equal(t, "usage_details_unavailable", estimate.Status)
				assert.Nil(t, estimate.Revenue)
				assert.Nil(t, estimate.Cost)
				return
			}
			assert.Equal(t, "estimated", estimate.Status)
			require.NotNil(t, estimate.Cost)
			cost := int64(20_000_000)
			if tc.zero {
				cost = 0
			}
			assert.EqualValues(t, cost, estimate.Cost.Nanos)
		})
	}
}

func TestMarginGuardUsesGatewayUsage(t *testing.T) {
	for _, tc := range []struct {
		name, body, expression string
		stream                 bool
		cost                   int64
	}{
		{name: "normal success contributes to seven day means", body: `{"choices":[{"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, expression: `p * 100`, cost: 1000000},
		{name: "procurement uses reported output after interrupted delivery", stream: true, body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":1000,\"total_tokens\":1010}}\n\n", expression: `c * 10`, cost: 10000000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newTextGatewayFixture(t, func(w http.ResponseWriter, _ *http.Request) {
				contentType := "application/json"
				if tc.stream {
					contentType = "text/event-stream"
				}
				w.Header().Set("Content-Type", contentType)
				_, err := fmt.Fprint(w, tc.body)
				assert.NoError(t, err)
			})
			request := fmt.Sprintf(`{"model":"gateway-alias","messages":[{"role":"user","content":"Hello"}],"stream":%t,"max_tokens":16}`, tc.stream)
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/"+"chat/"+"completions", request)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			manager := createWalletTestAccessToken(t, fixture.sale.CreatedBy, "pricing:read", "pricing:write")
			draft, err := common.Marshal(map[string]any{"owner_type": "channel", "owner_id": fixture.channel.ID, "currency": "USD", "expression": tc.expression})
			require.NoError(t, err)
			denied := adminSecurityRequest(fixture.engine, manager, http.MethodPost, "/api/admin/v1/price-versions", string(draft))
			require.Equal(t, http.StatusConflict, denied.Code, denied.Body.String())
			var reply struct {
				Data model.PriceMarginReport `json:"data"`
			}
			require.NoError(t, common.Unmarshal(denied.Body.Bytes(), &reply))
			require.Len(t, reply.Data.Estimates, 1)
			estimate := reply.Data.Estimates[0]
			assert.Equal(t, 1, estimate.Samples)
			require.NotNil(t, estimate.Cost)
			assert.EqualValues(t, tc.cost, estimate.Cost.Nanos)
		})
	}
}

func TestMarginReportOverflowHTTP(t *testing.T) {
	engine, actor, token := newAdminTestServer(t)
	t.Setenv("GATEWAY_SETTLEMENT_JOURNAL_DIR", t.TempDir())
	require.NoError(t, model.MigrateGatewayBilling(model.DB))
	for index := range 2 {
		customer := model.User{Username: fmt.Sprintf("overflow-%d", index), Password: "unused", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", AffCode: fmt.Sprintf("overflow-%d", index)}
		require.NoError(t, model.DB.Create(&customer).Error)
		zero := money.Amount{Currency: "USD"}
		request := model.GatewayRequest{ID: fmt.Sprintf("overflow-%d", index), UserID: customer.Id, ModelOfferingID: 17, SaleCurrency: "USD", SaleFXRate: "1", StartedAt: 1735776000000, Status: "success", Receivable: money.Amount{Nanos: 5000000000000000000, Currency: "USD"}}
		require.NoError(t, model.ReserveGateway(&request, zero, zero))
		require.NoError(t, model.PrepareGatewaySettlement(model.GatewaySettlement{Request: request, SaleSource: request.Receivable}))
		_, err := model.SettleGateway(request.ID)
		require.NoError(t, err)
		if index == 0 {
			_, err = model.PostLedgerEntries(model.LedgerPosting{Account: model.PlatformRevenueAccount("USD"), Type: model.LedgerEntryRefund, Amount: money.Amount{Nanos: -5000000000000000000, Currency: "USD"}, OperatorId: actor.Id})
			require.NoError(t, err)
		}
	}
	response := adminSecurityRequest(engine, token, http.MethodGet, "/api/admin/v1/reports/margin?group_by=model_offering", "")
	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "The amount is outside the supported range")
}

func TestMarginPendingAccountingHTTP(t *testing.T) {
	for _, tc := range []struct {
		name                                string
		salePending, costPending, fxPending bool
		original, converted                 int64
	}{
		{name: "unknown sale retains known procurement", salePending: true, original: 1000000000, converted: 2000000000},
		{name: "unknown procurement retains known sale", costPending: true},
		{name: "explicit free procurement remains known zero", original: 0, converted: 0},
		{name: "frozen conversion retains independent rounding", original: 1, converted: 1},
		{name: "unknown conversion retains original payable", fxPending: true, original: 1000000000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, actor, token := newAdminTestServer(t)
			t.Setenv("GATEWAY_SETTLEMENT_JOURNAL_DIR", t.TempDir())
			require.NoError(t, model.MigrateGatewayBilling(model.DB))
			request := model.GatewayRequest{ID: "pending-margin", UserID: actor.Id, ModelOfferingID: 17, SaleCurrency: "USD", SaleFXRate: "1", StartedAt: 1735776000000, Status: "success", Receivable: money.Amount{Nanos: 1000000000, Currency: "USD"}}
			zero := money.Amount{Currency: "USD"}
			require.NoError(t, model.ReserveGateway(&request, zero, zero))
			if tc.salePending {
				request.SalePending = true
				request.Receivable.Nanos = 0
			}
			attempt := model.GatewayAttempt{ID: "pending-attempt", RequestID: request.ID, ChannelID: 31, ProviderID: 41, Status: "success", Cost: money.Amount{Nanos: tc.original, Currency: "EUR"}, AccountingFXRate: "2", ConvertedCost: money.Amount{Nanos: tc.converted, Currency: "USD"}, CostPending: tc.costPending, ConvertedCostPending: tc.fxPending}
			if tc.fxPending {
				attempt.ConvertedCost.Nanos = 0
			}
			pending := tc.salePending || tc.costPending || tc.fxPending
			require.NoError(t, model.PrepareGatewaySettlement(model.GatewaySettlement{Request: request, Attempt: &attempt, SaleSource: request.Receivable, EvaluationFailed: pending, SaleEvaluationFailed: tc.salePending}))
			_, err := model.SettleGateway(request.ID)
			if pending {
				require.ErrorIs(t, err, model.ErrGatewayEvaluationPending)
			} else {
				require.NoError(t, err)
			}
			response := adminSecurityRequest(engine, token, http.MethodGet, "/api/admin/v1/reports/margin?group_by=model_offering", "")
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var reply struct {
				Data struct {
					Items []model.MarginRow `json:"items"`
				} `json:"data"`
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &reply))
			require.Len(t, reply.Data.Items, 1)
			row := reply.Data.Items[0]
			if pending {
				assert.Nil(t, row.Margin)
			} else {
				require.NotNil(t, row.Margin)
			}
			if !tc.costPending && !tc.fxPending {
				assert.EqualValues(t, tc.converted, row.Cost.Nanos)
			}
			assert.Equal(t, tc.salePending, row.SalePending > 0)
			assert.Equal(t, tc.costPending, row.CostPending > 0)
			assert.Equal(t, tc.fxPending, row.ConvertedCostPending > 0)
			if !tc.costPending {
				require.Len(t, row.OriginalCosts, 1)
				assert.EqualValues(t, tc.original, row.OriginalCosts[0].Nanos)
			}
			if !tc.salePending {
				assert.EqualValues(t, 1000000000, row.Revenue.Nanos)
			}
		})
	}
}

func TestMarginReportHTTP(t *testing.T) {
	engine, actor, token := newAdminTestServer(t)
	t.Setenv("GATEWAY_SETTLEMENT_JOURNAL_DIR", t.TempDir())
	require.NoError(t, model.MigrateGatewayBilling(model.DB))
	request := model.GatewayRequest{ID: "margin-request", UserID: actor.Id, ModelOfferingID: 17, OfferingName: "margin-offering", RequestedName: "margin-offering", SaleCurrency: "USD", SaleFXRate: "1", StartedAt: 1735776000000, EndedAt: 1735776001000, Status: "success", Receivable: money.Amount{Nanos: 1000000000, Currency: "USD"}}
	zero := money.Amount{Currency: "USD"}
	require.NoError(t, model.ReserveGateway(&request, zero, zero))
	attempts := []model.GatewayAttempt{
		{ID: "margin-failed", RequestID: request.ID, ChannelID: 31, ProviderID: 41, Status: "failed", Cost: money.Amount{Nanos: 100000000, Currency: "EUR"}, AccountingFXRate: "2", ConvertedCost: money.Amount{Nanos: 200000000, Currency: "USD"}},
		{ID: "margin-success", RequestID: request.ID, ChannelID: 32, ProviderID: 42, Status: "success", Cost: money.Amount{Nanos: 300000000, Currency: "EUR"}, AccountingFXRate: "2", ConvertedCost: money.Amount{Nanos: 600000000, Currency: "USD"}},
	}
	require.NoError(t, model.PrepareGatewaySettlement(model.GatewaySettlement{Request: request, Attempts: attempts, SaleSource: request.Receivable}))
	_, err := model.SettleGateway(request.ID)
	require.NoError(t, err)
	// The ledger has the complete revenue and unpaid debt. Log delivery has not run.
	response := adminSecurityRequest(engine, token, http.MethodGet, "/api/admin/v1/reports/margin?group_by=model_offering", "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var reply struct {
		Data struct {
			Items []struct {
				Revenue     money.Amount  `json:"revenue"`
				Cost        money.Amount  `json:"cost"`
				Margin      *money.Amount `json:"margin"`
				Collected   money.Amount  `json:"collected"`
				Outstanding money.Amount  `json:"outstanding"`
			} `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &reply))
	require.Len(t, reply.Data.Items, 1)
	row := reply.Data.Items[0]
	assert.EqualValues(t, 1000000000, row.Revenue.Nanos)
	assert.EqualValues(t, 800000000, row.Cost.Nanos)
	require.NotNil(t, row.Margin)
	assert.EqualValues(t, 200000000, row.Margin.Nanos)
	assert.Zero(t, row.Collected.Nanos)
	assert.EqualValues(t, 1000000000, row.Outstanding.Nanos)
	require.NoError(t, model.MigrateGatewayRecords())
	for range 2 {
		require.NoError(t, model.PublishGatewayRecords(request.ID))
	}
	republished := adminSecurityRequest(engine, token, http.MethodGet, "/api/admin/v1/reports/margin?group_by=model_offering", "")
	assert.JSONEq(t, response.Body.String(), republished.Body.String(), "log delivery must not duplicate accounting")
	_, err = model.TopUpWallet(model.WalletTopUp{UserId: actor.Id, Source: money.Amount{Nanos: 400000000, Currency: "USD"}, Reference: "margin-partial-payment"})
	require.NoError(t, err)
	paid := adminSecurityRequest(engine, token, http.MethodGet, "/api/admin/v1/reports/margin?group_by=model_offering", "")
	require.Equal(t, http.StatusOK, paid.Code, paid.Body.String())
	require.NoError(t, common.Unmarshal(paid.Body.Bytes(), &reply))
	assert.EqualValues(t, 1000000000, reply.Data.Items[0].Revenue.Nanos)
	assert.EqualValues(t, 400000000, reply.Data.Items[0].Collected.Nanos)
	assert.EqualValues(t, 600000000, reply.Data.Items[0].Outstanding.Nanos)
	for _, query := range []string{"provider_id=41", "channel_id=31", "model_offering_id=17&from=2025-01-02&until=2025-01-03&timezone=Asia%2FShanghai"} {
		filtered := adminSecurityRequest(engine, token, http.MethodGet, "/api/admin/v1/reports/margin?group_by=provider&"+query, "")
		require.Equal(t, http.StatusOK, filtered.Code, filtered.Body.String())
	}

	// Negative prices must be checked on the server, including saves that bypass the UI.
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.ModelOffering{}, &model.ModelOfferingMember{}, &model.ModelOfferingName{}))
	require.NoError(t, model.MigratePricing(model.DB))
	publisher := model.Vendor{Name: "margin publisher", Status: 1}
	require.NoError(t, publisher.Insert())
	unified := model.Model{ModelName: "margin model", VendorID: publisher.Id, Status: 1}
	require.NoError(t, unified.Insert())
	provider := model.Provider{Name: "margin provider", Type: 1, Status: common.ChannelStatusEnabled, Key: "private"}
	require.NoError(t, model.DB.Create(&provider).Error)
	channel := model.Channel{ProviderID: provider.Id, ModelID: unified.Id, Protocol: "openai_chat", UpstreamModelName: "margin", ParameterOverrides: `{}`, Status: model.ChannelPending}
	require.NoError(t, model.DB.Omit("Model", "Provider").Create(&channel).Error)
	offering := model.ModelOffering{Name: "guarded-margin", Members: []model.ModelOfferingMember{{ModelID: unified.Id, Tier: 1}}}
	require.NoError(t, model.SaveModelOffering(t.Context(), &offering, "save"))
	purchase := fmt.Sprintf(`{"owner_type":"channel","owner_id":%d,"currency":"USD","expression":"tier(\"request\", fixed(2))"}`, channel.ID)
	require.Equal(t, http.StatusOK, adminSecurityRequest(engine, token, http.MethodPost, "/api/admin/v1/price-versions", purchase).Code)
	sale := fmt.Sprintf(`{"owner_type":"model_offering","owner_id":%d,"currency":"USD","expression":"tier(\"request\", fixed(1))"}`, offering.ID)
	rejected := adminSecurityRequest(engine, token, http.MethodPost, "/api/admin/v1/price-versions", sale)
	require.Equal(t, http.StatusConflict, rejected.Code, rejected.Body.String())
	var warning struct {
		Data model.PriceMarginReport `json:"data"`
	}
	require.NoError(t, common.Unmarshal(rejected.Body.Bytes(), &warning))
	require.True(t, warning.Data.RequiresConfirmation)
	require.Len(t, warning.Data.Estimates, 1)
	assert.True(t, warning.Data.Estimates[0].Negative)
	var draft map[string]any
	require.NoError(t, common.UnmarshalJsonStr(sale, &draft))
	draft["effective_at"] = warning.Data.EffectiveAt
	draft["margin_confirmation"] = "stale"
	encoded, err := common.Marshal(draft)
	require.NoError(t, err)
	assert.Equal(t, http.StatusConflict, adminSecurityRequest(engine, token, http.MethodPost, "/api/admin/v1/price-versions", string(encoded)).Code)
	draft["margin_confirmation"] = warning.Data.Revision
	encoded, err = common.Marshal(draft)
	require.NoError(t, err)
	confirmed := adminSecurityRequest(engine, token, http.MethodPost, "/api/admin/v1/price-versions", string(encoded))
	require.Equal(t, http.StatusOK, confirmed.Code, confirmed.Body.String())

	// Freeze actual recent usage in the existing lifecycle. The exact mean is
	// 5/3 tokens; decimal division must not change the selected pricing tier.
	_, err = model.TopUpWallet(model.WalletTopUp{UserId: actor.Id, Source: money.Amount{Nanos: 600000000, Currency: "USD"}, Reference: "margin-complete-payment"})
	require.NoError(t, err)
	for index, prompt := range []int{1, 2, 2} {
		recent := model.GatewayRequest{ID: fmt.Sprintf("mean-%d", index), UserID: actor.Id, ModelOfferingID: offering.ID, SaleCurrency: "USD", SaleFXRate: "1", StartedAt: clock.Now().UnixMilli(), Status: "success", Receivable: zero}
		require.NoError(t, model.ReserveGateway(&recent, zero, zero))
		require.NoError(t, model.PrepareGatewaySettlement(model.GatewaySettlement{Request: recent, SaleSource: zero, SaleUsage: &dto.Usage{PromptTokens: prompt, TotalTokens: prompt}}))
		_, err := model.SettleGateway(recent.ID)
		require.NoError(t, err)
	}
	exactSale := fmt.Sprintf(`{"owner_type":"model_offering","owner_id":%d,"currency":"USD","expression":"len == 5 / 3 ? tier(\"exact\", fixed(3)) : tier(\"rounded\", fixed(0))","effective_at":%d}`, offering.ID, clock.Now().Unix()+1)
	exact := adminSecurityRequest(engine, token, http.MethodPost, "/api/admin/v1/price-versions", exactSale)
	require.Equal(t, http.StatusOK, exact.Code, exact.Body.String())

	// A changed procurement version invalidates an earlier loss approval.
	fake := clock.UseFake(t, time.Now().Truncate(time.Second).Add(2*time.Second))
	fxPurchase := fmt.Sprintf(`{"owner_type":"channel","owner_id":%d,"currency":"EUR","expression":"tier(\"request\", fixed(0.5))"}`, channel.ID)
	require.Equal(t, http.StatusOK, adminSecurityRequest(engine, token, http.MethodPost, "/api/admin/v1/price-versions", fxPurchase).Code)
	fxBody := `{"from_currency":"EUR","to_currency":"USD","rate":"10","buffer_rate":"0.02"}`
	fxDenied := adminSecurityRequest(engine, token, http.MethodPut, "/api/admin/v1/fx-rates", fxBody)
	require.Equal(t, http.StatusConflict, fxDenied.Code, fxDenied.Body.String())
	require.NoError(t, common.Unmarshal(fxDenied.Body.Bytes(), &warning))
	oldRevision := warning.Data.Revision
	fake.Advance(time.Second)
	fxPurchase = fmt.Sprintf(`{"owner_type":"channel","owner_id":%d,"currency":"EUR","expression":"tier(\"request\", fixed(0.4))"}`, channel.ID)
	require.Equal(t, http.StatusOK, adminSecurityRequest(engine, token, http.MethodPost, "/api/admin/v1/price-versions", fxPurchase).Code)
	staleFX := fmt.Sprintf(`{"from_currency":"EUR","to_currency":"USD","rate":"10","buffer_rate":"0.02","margin_confirmation":"%s"}`, oldRevision)
	fxDenied = adminSecurityRequest(engine, token, http.MethodPut, "/api/admin/v1/fx-rates", staleFX)
	require.Equal(t, http.StatusConflict, fxDenied.Code, fxDenied.Body.String())
	require.NoError(t, common.Unmarshal(fxDenied.Body.Bytes(), &warning))
	assert.NotEqual(t, oldRevision, warning.Data.Revision)
	currentFX := fmt.Sprintf(`{"from_currency":"EUR","to_currency":"USD","rate":"10","buffer_rate":"0.02","margin_confirmation":"%s"}`, warning.Data.Revision)
	fxSaved := adminSecurityRequest(engine, token, http.MethodPut, "/api/admin/v1/fx-rates", currentFX)
	require.Equal(t, http.StatusOK, fxSaved.Code, fxSaved.Body.String())

	assert.Equal(t, http.StatusUnauthorized, adminSecurityRequest(engine, "", http.MethodGet, "/api/admin/v1/reports/margin", "").Code)
	scoped := createWalletTestAccessToken(t, actor.Id, "pricing:read")
	assert.Equal(t, http.StatusForbidden, adminSecurityRequest(engine, scoped, http.MethodGet, "/api/admin/v1/reports/margin", "").Code)
	for _, query := range []string{"provider_id=", "channel_id=-1", "group_by=invalid", "from=", "timezone=not-a-zone", "from=2025-01-03&until=2025-01-02", "page=-1", "page_size=101"} {
		assert.Equal(t, http.StatusBadRequest, adminSecurityRequest(engine, token, http.MethodGet, "/api/admin/v1/reports/margin?"+query, "").Code, query)
	}
	for _, tc := range []struct {
		query string
		rows  int
		date  string
	}{
		{query: "from=2025-01-02&until=2025-01-03&timezone=America%2FLos_Angeles", rows: 0},
		{query: "from=2025-01-01&until=2025-01-02&timezone=America%2FLos_Angeles", rows: 1, date: "2025-01-01"},
	} {
		filtered := adminSecurityRequest(engine, token, http.MethodGet, "/api/admin/v1/reports/margin?model_offering_id=17&group_by=day&"+tc.query, "")
		require.Equal(t, http.StatusOK, filtered.Code, filtered.Body.String())
		var page struct {
			Data struct {
				Items []model.MarginRow `json:"items"`
			} `json:"data"`
		}
		require.NoError(t, common.Unmarshal(filtered.Body.Bytes(), &page))
		require.Len(t, page.Data.Items, tc.rows)
		if tc.rows > 0 {
			assert.Equal(t, tc.date, page.Data.Items[0].Date)
			assert.EqualValues(t, 800000000, page.Data.Items[0].Cost.Nanos)
		}
	}
	expired := createWalletTestAccessToken(t, actor.Id, "ledger:read")
	require.NoError(t, model.DB.Model(&model.UserAccessToken{}).Where("token_hash = ?", model.AccessTokenFingerprint(expired)).Update("expires_at", clock.Now().Unix()-1).Error)
	assert.Equal(t, http.StatusUnauthorized, adminSecurityRequest(engine, expired, http.MethodGet, "/api/admin/v1/reports/margin", "").Code)
	revoked := createWalletTestAccessToken(t, actor.Id, "ledger:read")
	stored, err := model.FindUserAccessTokenByHash(model.AccessTokenFingerprint(revoked))
	require.NoError(t, err)
	_, err = model.DeleteUserAccessToken(actor.Id, stored.Id)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, adminSecurityRequest(engine, revoked, http.MethodGet, "/api/admin/v1/reports/margin", "").Code)
	customer := model.User{Username: "margin-customer", Password: "unused", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: "margin-customer"}
	require.NoError(t, model.DB.Create(&customer).Error)
	customerToken := createWalletTestAccessToken(t, customer.Id, "ledger:read")
	assert.Equal(t, http.StatusForbidden, adminSecurityRequest(engine, customerToken, http.MethodGet, "/api/admin/v1/reports/margin", "").Code)
}
