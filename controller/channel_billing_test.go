package controller

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpdateAllChannelsBalanceUsesStableAccountOrder(t *testing.T) {
	db := model.UseTestDatabases(t)
	require.NoError(t, db.AutoMigrate(&model.Provider{}, &model.User{}))
	previousCache, previousRedis, previousInterval := common.MemoryCacheEnabled, common.RedisEnabled, common.RequestInterval
	common.MemoryCacheEnabled, common.RedisEnabled, common.RequestInterval = true, false, 0
	t.Cleanup(func() {
		model.InitProviderCache()
		common.MemoryCacheEnabled, common.RedisEnabled, common.RequestInterval = previousCache, previousRedis, previousInterval
	})
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if r.URL.Path == "/balance" {
			name = "raw"
		} else {
			assert.Equal(t, "/v1/usage", r.URL.Path)
		}
		mu.Lock()
		calls = append(calls, name)
		mu.Unlock()
		switch name {
		case "positive":
			_, _ = w.Write([]byte(`{"remaining":12.5,"isValid":true}`))
		case "zero", "noauto":
			_, _ = w.Write([]byte(`{"remaining":0,"isValid":true}`))
		case "error":
			http.Error(w, "fixture upstream unavailable", http.StatusServiceUnavailable)
		case "raw":
			_, _ = w.Write([]byte(`{"credits":{"remaining":0},"unit":"upstream-only"}`))
		default:
			t.Errorf("filtered Provider reached the upstream: %s", name)
		}
	}))
	defer server.Close()
	accounts := []model.Provider{
		{Id: 11, Name: "positive", Priority: common.GetPointer(int64(100))},
		{Id: 12, Name: "zero", Priority: common.GetPointer(int64(0))},
		{Id: 13, Name: "error", Priority: common.GetPointer(int64(-100))},
		{Id: 14, Name: "disabled", Priority: common.GetPointer(int64(999)), Status: common.ChannelStatusManuallyDisabled},
		{Id: 15, Name: "multi", Priority: common.GetPointer(int64(1000)), ProviderInfo: model.ProviderInfo{IsMultiKey: true}},
		{Id: 16, Name: "noauto", Priority: common.GetPointer(int64(50)), AutoBan: common.GetPointer(0)},
		{Id: 17, Name: "raw", Priority: common.GetPointer(int64(25)), Type: constant.ChannelTypeAdvancedCustom, OtherSettings: `{"advanced_custom":{"advanced_routes":[{"incoming_path":"/v1/dashboard/billing/credit_grants","upstream_path":"/balance","converter":"none"}]}}`},
	}
	for index := range accounts {
		account := &accounts[index]
		account.Key, account.BaseURL, account.Balance = account.Name, common.GetPointer(server.URL), 99
		if account.Type == 0 {
			account.Type = constant.ChannelTypeSub2API
		}
		if account.Status == 0 {
			account.Status = common.ChannelStatusEnabled
		}
		require.NoError(t, db.Create(account).Error)
	}
	model.InitProviderCache()
	require.NoError(t, updateAllChannelsBalance())
	mu.Lock()
	assert.Equal(t, []string{"raw", "noauto", "error", "zero", "positive"}, calls, "live balance refresh must ignore historical Provider.Priority")
	mu.Unlock()
	for _, original := range accounts {
		var stored model.Provider
		require.NoError(t, db.First(&stored, original.Id).Error)
		assert.Equal(t, original.Priority, stored.Priority)
		assert.Equal(t, original.OtherSettings, stored.OtherSettings)
		assert.Equal(t, original.Key, stored.Key)
		expectedStatus := original.Status
		switch original.Name {
		case "positive":
			assert.Equal(t, 12.5, stored.Balance)
			assert.Positive(t, stored.BalanceUpdatedTime)
		case "zero", "noauto":
			assert.Zero(t, stored.Balance)
			assert.Positive(t, stored.BalanceUpdatedTime)
			if original.Name == "zero" {
				expectedStatus = common.ChannelStatusAutoDisabled
				assert.Equal(t, "Insufficient balance", stored.GetOtherInfo()["status_reason"])
			}
		default:
			assert.Equal(t, original.Balance, stored.Balance)
			assert.Zero(t, stored.BalanceUpdatedTime, "filtered, failed, and raw-only responses do not fabricate a balance")
		}
		assert.Equal(t, expectedStatus, stored.Status)
		cached, err := model.CacheGetProvider(original.Id)
		require.NoError(t, err)
		assert.Equal(t, expectedStatus, cached.Status, "zero-balance disable is published to the live Provider cache")
	}

	mu.Lock()
	calls = nil
	mu.Unlock()
	require.NoError(t, db.Exec("ALTER TABLE providers RENAME TO unavailable_providers").Error)
	t.Cleanup(func() { require.NoError(t, db.Exec("ALTER TABLE unavailable_providers RENAME TO providers").Error) })
	require.Error(t, updateAllChannelsBalance(), "a failed account read must not look like an empty successful scan")
	mu.Lock()
	assert.Empty(t, calls)
	mu.Unlock()
}

func TestGetDeepSeekBalanceUSD(t *testing.T) {
	tests := []struct {
		name            string
		responseJSON    string
		usdExchangeRate float64
		want            float64
		wantErrContains string
	}{
		{
			name:            "prefers USD when USD precedes CNY",
			responseJSON:    `{"balance_infos":[{"currency":"USD","total_balance":"12.50"},{"currency":"CNY","total_balance":"73.00"}]}`,
			usdExchangeRate: 7.3,
			want:            12.5,
		},
		{
			name:            "prefers USD when CNY precedes USD",
			responseJSON:    `{"balance_infos":[{"currency":"CNY","total_balance":"73.00"},{"currency":"USD","total_balance":"12.50"}]}`,
			usdExchangeRate: 7.3,
			want:            12.5,
		},
		{
			name:            "converts CNY when USD is absent",
			responseJSON:    `{"balance_infos":[{"currency":"CNY","total_balance":"73.00"}]}`,
			usdExchangeRate: 7.3,
			want:            10,
		},
		{
			name:            "converts CNY when USD balance is zero",
			responseJSON:    `{"balance_infos":[{"currency":"USD","total_balance":"0.00"},{"currency":"CNY","total_balance":"73.00"}]}`,
			usdExchangeRate: 7.3,
			want:            10,
		},
		{
			name:            "converts CNY when zero USD follows CNY",
			responseJSON:    `{"balance_infos":[{"currency":"CNY","total_balance":"73.00"},{"currency":"USD","total_balance":"0.00"}]}`,
			usdExchangeRate: 7.3,
			want:            10,
		},
		{
			name:            "returns zero when USD balance is zero and CNY is absent",
			responseJSON:    `{"balance_infos":[{"currency":"USD","total_balance":"0.00"}]}`,
			usdExchangeRate: 7.3,
			want:            0,
		},
		{
			name:            "returns error when USD and CNY are absent",
			responseJSON:    `{"balance_infos":[{"currency":"EUR","total_balance":"10.00"}]}`,
			usdExchangeRate: 7.3,
			wantErrContains: "currency USD or CNY not found",
		},
		{
			name:            "returns USD parse error instead of falling back to CNY",
			responseJSON:    `{"balance_infos":[{"currency":"USD","total_balance":"invalid"},{"currency":"CNY","total_balance":"73.00"}]}`,
			usdExchangeRate: 7.3,
			wantErrContains: "invalid syntax",
		},
		{
			name:            "rejects NaN USD balance",
			responseJSON:    `{"balance_infos":[{"currency":"USD","total_balance":"NaN"}]}`,
			usdExchangeRate: 7.3,
			wantErrContains: "USD balance must be finite",
		},
		{
			name:            "rejects negative USD balance",
			responseJSON:    `{"balance_infos":[{"currency":"USD","total_balance":"-1.00"}]}`,
			usdExchangeRate: 7.3,
			wantErrContains: "USD balance must be non-negative",
		},
		{
			name:            "rejects positive infinity CNY balance",
			responseJSON:    `{"balance_infos":[{"currency":"CNY","total_balance":"+Inf"}]}`,
			usdExchangeRate: 7.3,
			wantErrContains: "CNY balance must be finite",
		},
		{
			name:            "rejects negative CNY balance",
			responseJSON:    `{"balance_infos":[{"currency":"CNY","total_balance":"-7.30"}]}`,
			usdExchangeRate: 7.3,
			wantErrContains: "CNY balance must be non-negative",
		},
		{
			name:            "returns error for non-positive CNY exchange rate",
			responseJSON:    `{"balance_infos":[{"currency":"CNY","total_balance":"73.00"}]}`,
			usdExchangeRate: 0,
			wantErrContains: "USD exchange rate must be greater than zero",
		},
		{
			name:            "rejects NaN CNY exchange rate",
			responseJSON:    `{"balance_infos":[{"currency":"CNY","total_balance":"73.00"}]}`,
			usdExchangeRate: math.NaN(),
			wantErrContains: "USD exchange rate must be finite",
		},
		{
			name:            "rejects positive infinity CNY exchange rate",
			responseJSON:    `{"balance_infos":[{"currency":"CNY","total_balance":"73.00"}]}`,
			usdExchangeRate: math.Inf(1),
			wantErrContains: "USD exchange rate must be finite",
		},
		{
			name:            "rejects CNY conversion overflow",
			responseJSON:    `{"balance_infos":[{"currency":"CNY","total_balance":"1.7976931348623157e+308"}]}`,
			usdExchangeRate: math.SmallestNonzeroFloat64,
			wantErrContains: "converted USD balance must be finite",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var response DeepSeekUsageResponse
			require.NoError(t, common.Unmarshal([]byte(test.responseJSON), &response))

			balance, err := getDeepSeekBalanceUSD(response, test.usdExchangeRate)
			if test.wantErrContains != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), test.wantErrContains)
				return
			}

			require.NoError(t, err)
			assert.InDelta(t, test.want, balance, 1e-12)
		})
	}
}

func TestUpdateChannelNewAPIBalanceUsesTheSmallerRemainingQuota(t *testing.T) {
	for _, tc := range []struct {
		name    string
		usage   string
		want    float64
		wantErr string
	}{
		{name: "user quota below the key quota", usage: `{"code":true,"data":{"total_available":2000000,"unlimited_quota":false,"user_quota":{"total_available":500000}}}`, want: 0.5},
		{name: "key quota below the user quota", usage: `{"code":true,"data":{"total_available":500000,"unlimited_quota":false,"user_quota":{"total_available":2000000}}}`, want: 0.5},
		{name: "unlimited key uses the user quota", usage: `{"code":true,"data":{"total_available":0,"unlimited_quota":true,"user_quota":{"total_available":3000000}}}`, want: 3},
		{name: "key quota alone without user quota", usage: `{"code":true,"data":{"total_available":1500000,"unlimited_quota":false}}`, want: 1.5},
		{name: "unlimited key without user quota", usage: `{"code":true,"data":{"total_available":0,"unlimited_quota":true}}`, wantErr: "Allow querying user quota"},
		{name: "upstream rejects the key", usage: `{"success":false,"message":"invalid token"}`, wantErr: "invalid token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupSub2APIBalanceTest(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/usage/token/":
					assert.Equal(t, "Bearer sk-upstream", r.Header.Get("Authorization"))
					_, _ = w.Write([]byte(tc.usage))
				case "/api/status":
					assert.Empty(t, r.Header.Get("Authorization"), "the key is not sent to the public status endpoint")
					_, _ = w.Write([]byte(`{"success":true,"data":{"quota_per_unit":1000000}}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			baseURL := server.URL + "/"
			channel := &model.Provider{Type: constant.ChannelTypeNewAPI, BaseURL: &baseURL, Key: "sk-upstream", Name: "new-api-upstream"}
			require.NoError(t, model.DB.Create(channel).Error)

			result, err := updateChannelBalance(channel)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.InDelta(t, tc.want, result.Balance, 1e-9)
			assert.InDelta(t, tc.want, channel.Balance, 1e-9)
		})
	}
}
