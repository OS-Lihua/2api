package router

import (
	"fmt"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
			if tc.original > 0 {
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
