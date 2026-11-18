package router

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/google/uuid"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type walletAPIResponse struct {
	Success bool           `json:"success"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data"`
}

func walletAPIRequest(t *testing.T, engine http.Handler, token, method, path, body string) walletAPIResponse {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		return walletAPIResponse{Message: fmt.Sprintf("HTTP %d", recorder.Code)}
	}
	var response walletAPIResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response), recorder.Body.String())
	return response
}

func createWalletTestAccessToken(t *testing.T, userID int, scopes ...string) string {
	t.Helper()
	suffix, err := common.GenerateRandomCharsKey(43)
	require.NoError(t, err)
	raw := model.AccessTokenPrefix + suffix
	token := &model.UserAccessToken{Name: "wallet test", TokenHash: model.AccessTokenFingerprint(raw), TokenHint: model.AccessTokenHint(raw)}
	require.NoError(t, token.SetScopes(scopes))
	require.NoError(t, model.CreateUserAccessToken(userID, token, service.AccessTokenMaxPerUser))
	return raw
}

func TestWalletFundingPendingProjection(t *testing.T) {
	previousMaster, previousRedis := common.IsMasterNode, common.RedisEnabled
	t.Cleanup(func() { common.IsMasterNode, common.RedisEnabled = previousMaster, previousRedis })
	common.IsMasterNode, common.RedisEnabled = true, false
	t.Setenv("GATEWAY_SETTLEMENT_JOURNAL_DIR", t.TempDir())
	db := model.UseTestDatabases(t)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(4)
	require.NoError(t, i18n.Init())
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.UserSession{}, &model.CasbinRule{}, &model.AuthzRole{}, &model.UserAccessToken{}, &model.Option{}))
	require.NoError(t, model.MigrateGatewayBilling(db))
	require.NoError(t, model.MigrateSubscriptionMoney(db))
	require.NoError(t, model.MigrateAuditLogs())
	require.NoError(t, model.EnsureLegacyAccessTokenRetireAt(time.Now().Unix()))
	require.NoError(t, authz.Init(db))
	admin := &model.User{Username: "funding-view-admin", Password: "placeholder", Role: common.RoleAdminUser, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: "funding-view-admin"}
	customer := &model.User{Username: "funding-view-customer", Password: "placeholder", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: "funding-view-customer", Setting: `{"billing_preference":"subscription_only"}`}
	require.NoError(t, db.Create(admin).Error)
	require.NoError(t, db.Create(customer).Error)
	adminToken := createWalletTestAccessToken(t, admin.Id, "ledger:read")
	selfToken := createWalletTestAccessToken(t, customer.Id, "wallet:read")
	_, err = model.TopUpWallet(model.WalletTopUp{UserId: customer.Id, Source: money.Amount{Nanos: 1_000_000_000, Currency: "USD"}, Reference: "funding-view-cash"})
	require.NoError(t, err)
	plan := &model.SubscriptionPlan{
		Title: "Funding view entitlement", Price: money.Amount{Currency: "USD"},
		Allowance: money.Amount{Nanos: 1_000_000_000, Currency: "USD"}, MoneyVersion: 1,
		DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1, Enabled: true,
	}
	require.NoError(t, db.Create(plan).Error)
	model.InvalidateSubscriptionPlanCache(plan.Id)
	t.Cleanup(func() { model.InvalidateSubscriptionPlanCache(plan.Id) })
	_, err = model.AdminBindSubscription(customer.Id, plan.Id, "funding-view-regression")
	require.NoError(t, err)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	SetApiRouter(engine)
	batchPath := fmt.Sprintf("/api/admin/v1/user-wallets?user_ids=%d", customer.Id)
	singlePath := fmt.Sprintf("/api/admin/v1/user-wallets/%d", customer.Id)

	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprintf("funding_pending_%t", pending), func(t *testing.T) {
			consumed := "0"
			if pending {
				request := model.GatewayRequest{ID: "funding-view-overrun", UserID: customer.Id, SaleCurrency: "USD", SaleFXRate: "1"}
				hold := money.Amount{Nanos: 200_000_000, Currency: "USD"}
				require.NoError(t, model.ReserveGateway(&request, hold, hold))
				request.Status = "success"
				request.StartedAt, request.EndedAt = time.Now().UnixMilli(), time.Now().UnixMilli()
				request.Receivable = money.Amount{Nanos: 1_200_000_000, Currency: "USD"}
				require.NoError(t, model.PrepareGatewaySettlement(model.GatewaySettlement{Request: request, SaleSource: request.Receivable}))
				result, err := model.SettleGateway(request.ID)
				require.ErrorIs(t, err, model.ErrGatewayFundingPending)
				require.NotNil(t, result)
				require.True(t, result.FundingPending)
				require.False(t, result.SalePending)
				var billing model.GatewayBilling
				require.NoError(t, db.First(&billing, "request_id = ?", request.ID).Error)
				require.Equal(t, "funding_pending", billing.Phase)
				consumed = "1200000000"
			}
			known := map[string]any{"nanos": consumed, "currency": "USD"}
			summaries, err := model.GetUserWalletSummaries(t.Context(), []int{customer.Id})
			require.NoError(t, err)
			require.Len(t, summaries, 1)
			require.Equal(t, pending, summaries[0].FundingPending)
			require.False(t, summaries[0].SalePending)
			require.Equal(t, consumed, summaries[0].Consumed.Nanos)
			require.Equal(t, money.Amount{Nanos: 1_000_000_000, Currency: "USD"}, summaries[0].Available)

			var beforeWallets []model.Wallet
			var beforeEntries []model.LedgerEntry
			var beforeBillings []model.GatewayBilling
			require.NoError(t, db.Order("id").Find(&beforeWallets).Error)
			require.NoError(t, db.Order("id").Find(&beforeEntries).Error)
			require.NoError(t, db.Order("request_id").Find(&beforeBillings).Error)

			batch := walletAPIRequest(t, engine, adminToken, http.MethodGet, batchPath, "")
			require.True(t, batch.Success, batch.Message)
			rows, ok := batch.Data["items"].([]any)
			require.True(t, ok)
			require.Len(t, rows, 1)
			row, ok := rows[0].(map[string]any)
			require.True(t, ok)
			assert.Contains(t, row, "funding_pending")
			assert.Equal(t, pending, row["funding_pending"])
			assert.Equal(t, false, row["sale_pending"])
			assert.Equal(t, known, row["consumed"])
			assert.Equal(t, map[string]any{"nanos": "1000000000", "currency": "USD"}, row["available"])

			for _, endpoint := range []struct{ name, token, path string }{
				{"self", selfToken, "/api/user/self/wallet"},
				{"admin single", adminToken, singlePath},
			} {
				response := walletAPIRequest(t, engine, endpoint.token, http.MethodGet, endpoint.path, "")
				require.True(t, response.Success, "%s: %s", endpoint.name, response.Message)
				assert.Equal(t, float64(customer.Id), response.Data["user_id"])
				assert.Equal(t, "USD", response.Data["currency"])
				assert.Equal(t, "1", response.Data["balance"])
				assert.Equal(t, known, response.Data["consumed"])
				assert.Equal(t, false, response.Data["sale_pending"])
				value, present := response.Data["funding_pending"]
				assert.True(t, present, "%s must project funding_pending, including explicit false", endpoint.name)
				assert.Equal(t, pending, value, "%s must match the same user's administrator batch state", endpoint.name)
			}

			var afterWallets []model.Wallet
			var afterEntries []model.LedgerEntry
			var afterBillings []model.GatewayBilling
			require.NoError(t, db.Order("id").Find(&afterWallets).Error)
			require.NoError(t, db.Order("id").Find(&afterEntries).Error)
			require.NoError(t, db.Order("request_id").Find(&afterBillings).Error)
			assert.Equal(t, beforeWallets, afterWallets, "wallet reads must not change any balance")
			assert.Equal(t, beforeEntries, afterEntries, "wallet reads must not post accounting entries")
			assert.Equal(t, beforeBillings, afterBillings, "wallet reads must preserve the frozen accounting state")
		})
	}
}

func TestWalletSummaryAPI(t *testing.T) {
	previousMaster, previousRedis := common.IsMasterNode, common.RedisEnabled
	t.Cleanup(func() { common.IsMasterNode, common.RedisEnabled = previousMaster, previousRedis })
	common.IsMasterNode, common.RedisEnabled = true, false
	t.Setenv("GATEWAY_SETTLEMENT_JOURNAL_DIR", t.TempDir())
	db := model.UseTestDatabases(t)
	require.NoError(t, i18n.Init())
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.UserSession{}, &model.CasbinRule{}, &model.AuthzRole{}, &model.UserAccessToken{}, &model.Option{}))
	require.NoError(t, model.MigrateGatewayBilling(db))
	require.NoError(t, model.MigrateBalanceReminders(db))
	require.NoError(t, model.MigrateAuditLogs())
	require.NoError(t, model.EnsureLegacyAccessTokenRetireAt(time.Now().Unix()))
	require.NoError(t, authz.Init(db))
	admin := &model.User{Username: "summary-admin", Password: "placeholder", Role: common.RoleAdminUser, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: "summary-admin"}
	customer := &model.User{Username: "summary-customer", Password: "placeholder", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: "summary-customer", Quota: 12345, UsedQuota: 67890}
	require.NoError(t, db.Create(admin).Error)
	require.NoError(t, db.Create(customer).Error)
	token := createWalletTestAccessToken(t, admin.Id, "ledger:read")
	usersOnly := createWalletTestAccessToken(t, admin.Id, "user:read")
	selfOnly := createWalletTestAccessToken(t, customer.Id, "wallet:read")
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	SetApiRouter(engine)
	path := fmt.Sprintf("/api/admin/v1/user-wallets?user_ids=%d,%d", customer.Id, admin.Id)
	response := walletAPIRequest(t, engine, token, http.MethodGet, path, "")
	require.True(t, response.Success, response.Message)
	zero := map[string]any{"nanos": "0", "currency": "USD"}
	assert.Equal(t, []any{
		map[string]any{"user_id": float64(customer.Id), "available": zero, "outstanding": zero, "consumed": zero, "sale_pending": false, "funding_pending": false, "requests_paused": false},
		map[string]any{"user_id": float64(admin.Id), "available": zero, "outstanding": zero, "consumed": zero, "sale_pending": false, "funding_pending": false, "requests_paused": false},
	}, response.Data["items"], "legacy quota fields are not financial balances or spending")
	assert.Equal(t, "HTTP 403", walletAPIRequest(t, engine, usersOnly, http.MethodGet, path, "").Message)
	assert.Equal(t, "HTTP 403", walletAPIRequest(t, engine, selfOnly, http.MethodGet, path, "").Message)
	assert.Equal(t, "HTTP 401", walletAPIRequest(t, engine, "", http.MethodGet, path, "").Message)
	expired := createWalletTestAccessToken(t, admin.Id, "ledger:read")
	require.NoError(t, db.Model(&model.UserAccessToken{}).Where("token_hash = ?", model.AccessTokenFingerprint(expired)).Update("expires_at", time.Now().Unix()-1).Error)
	assert.Equal(t, "HTTP 401", walletAPIRequest(t, engine, expired, http.MethodGet, path, "").Message)
	cacheResponse := adminSecurityRequest(engine, token, http.MethodGet, path, "")
	assert.Contains(t, cacheResponse.Header().Get("Cache-Control"), "no-store")

	t.Run("bounded unambiguous selectors", func(t *testing.T) {
		overLimit := make([]string, 101)
		for i := range overLimit {
			overLimit[i] = strconv.Itoa(i + 1)
		}
		for _, query := range []string{"", "?user_ids=", "?user_ids=0", "?user_ids=-1", "?user_ids=01", "?user_ids=1.0", "?user_ids=9223372036854775808", "?user_ids=1,1", "?user_ids=1,", "?user_ids=1&user_ids=2", "?user_ids=" + strings.Join(overLimit, ",")} {
			assert.Equal(t, "HTTP 400", walletAPIRequest(t, engine, token, http.MethodGet, "/api/admin/v1/user-wallets"+query, "").Message, query)
		}
		missing := fmt.Sprintf("/api/admin/v1/user-wallets?user_ids=%d,2147483647", customer.Id)
		assert.Equal(t, "HTTP 404", walletAPIRequest(t, engine, token, http.MethodGet, missing, "").Message)
		deleted := model.User{Username: "summary-deleted", AffCode: "summary-deleted", Status: common.UserStatusEnabled}
		require.NoError(t, db.Create(&deleted).Error)
		require.NoError(t, db.Delete(&deleted).Error)
		assert.Equal(t, "HTTP 404", walletAPIRequest(t, engine, token, http.MethodGet, fmt.Sprintf("/api/admin/v1/user-wallets?user_ids=%d", deleted.Id), "").Message)
	})

	t.Run("posted receivable debt refunds and pending sale", func(t *testing.T) {
		_, err := model.TopUpWallet(model.WalletTopUp{UserId: customer.Id, Source: money.Amount{Nanos: 23, Currency: "USD"}, OperatorId: admin.Id, Remark: "summary cash"})
		require.NoError(t, err)
		request := model.GatewayRequest{ID: "summary-sale", UserID: customer.Id, SaleCurrency: "USD", SaleFXRate: "1", Receivable: money.Amount{Currency: "USD"}}
		require.NoError(t, model.ReserveGateway(&request, money.Amount{Nanos: 5, Currency: "USD"}, money.Amount{Nanos: 5, Currency: "USD"}))
		items, err := model.GetUserWalletSummaries(t.Context(), []int{customer.Id})
		require.NoError(t, err)
		require.Len(t, items, 1)
		assert.EqualValues(t, 18, items[0].Available.Nanos)
		assert.Equal(t, "0", items[0].Consumed.Nanos, "a reservation is not a sale")
		request.Receivable.Nanos = 27
		require.NoError(t, model.PrepareGatewaySettlement(model.GatewaySettlement{Request: request, SaleSource: request.Receivable}))
		_, err = model.SettleGateway(request.ID)
		require.NoError(t, err)
		items, err = model.GetUserWalletSummaries(t.Context(), []int{customer.Id})
		require.NoError(t, err)
		assert.Equal(t, "27", items[0].Consumed.Nanos, "unpaid receivable remains part of consumption")
		assert.Zero(t, items[0].Available.Nanos)
		assert.EqualValues(t, 4, items[0].Outstanding.Nanos)
		assert.True(t, items[0].RequestsPaused)
		assert.False(t, items[0].SalePending, "known debt is not an unknown sale")
		_, err = model.PostLedgerEntries(
			model.LedgerPosting{Account: model.UserWalletAccount(customer.Id), Type: model.LedgerEntryRefund, Amount: money.Amount{Nanos: 3, Currency: "USD"}, RequestId: request.ID},
			model.LedgerPosting{Account: model.PlatformRevenueAccount("USD"), Type: model.LedgerEntryRefund, Amount: money.Amount{Nanos: -3, Currency: "USD"}, RequestId: request.ID},
		)
		require.NoError(t, err)
		items, err = model.GetUserWalletSummaries(t.Context(), []int{customer.Id})
		require.NoError(t, err)
		assert.Equal(t, "24", items[0].Consumed.Nanos)
		assert.EqualValues(t, 1, items[0].Outstanding.Nanos)
		_, err = model.TopUpWallet(model.WalletTopUp{UserId: customer.Id, Source: money.Amount{Nanos: 10, Currency: "USD"}, OperatorId: admin.Id, Remark: "repay debt"})
		require.NoError(t, err)
		correction, err := model.PostLedgerEntries(model.LedgerPosting{Account: model.PlatformRevenueAccount("USD"), Type: model.LedgerEntryAdjustment, Amount: money.Amount{Nanos: -2, Currency: "USD"}, RequestId: request.ID})
		require.NoError(t, err)
		items, err = model.GetUserWalletSummaries(t.Context(), []int{customer.Id})
		require.NoError(t, err)
		assert.Equal(t, "22", items[0].Consumed.Nanos)
		_, err = model.ReverseLedgerEntry(correction[0].Id, admin.Id, "reverse correction")
		require.NoError(t, err)
		items, err = model.GetUserWalletSummaries(t.Context(), []int{customer.Id})
		require.NoError(t, err)
		assert.Equal(t, "24", items[0].Consumed.Nanos, "repayment never recognizes revenue twice")
		assert.EqualValues(t, 9, items[0].Available.Nanos)
		assert.Zero(t, items[0].Outstanding.Nanos)
		assert.False(t, items[0].RequestsPaused)

		purchasePending := model.GatewayRequest{ID: "summary-purchase-pending", UserID: customer.Id, SaleCurrency: "USD", SaleFXRate: "1", Receivable: money.Amount{Currency: "USD"}}
		require.NoError(t, model.ReserveGateway(&purchasePending, purchasePending.Receivable, purchasePending.Receivable))
		purchasePending.Receivable.Nanos = 5
		require.NoError(t, model.PrepareGatewaySettlement(model.GatewaySettlement{Request: purchasePending, SaleSource: purchasePending.Receivable, EvaluationFailed: true}))
		_, err = model.SettleGateway(purchasePending.ID)
		require.ErrorIs(t, err, model.ErrGatewayEvaluationPending)
		items, err = model.GetUserWalletSummaries(t.Context(), []int{customer.Id})
		require.NoError(t, err)
		assert.Equal(t, "29", items[0].Consumed.Nanos)
		assert.False(t, items[0].SalePending, "unfinished procurement does not make the sale unknown")
		assert.False(t, items[0].RequestsPaused)
		salePending := model.GatewayRequest{ID: "summary-sale-pending", UserID: customer.Id, SaleCurrency: "USD", SaleFXRate: "1", Receivable: money.Amount{Currency: "USD"}}
		require.NoError(t, model.ReserveGateway(&salePending, money.Amount{Nanos: 1, Currency: "USD"}, money.Amount{Nanos: 1, Currency: "USD"}))
		salePending.SalePending = true
		require.NoError(t, model.PrepareGatewaySettlement(model.GatewaySettlement{Request: salePending, SaleSource: salePending.Receivable, EvaluationFailed: true, SaleEvaluationFailed: true}))
		_, err = model.SettleGateway(salePending.ID)
		require.ErrorIs(t, err, model.ErrGatewayEvaluationPending)
		response := walletAPIRequest(t, engine, token, http.MethodGet, path, "")
		require.True(t, response.Success, response.Message)
		rows := response.Data["items"].([]any)
		require.Len(t, rows, 2)
		assert.Equal(t, map[string]any{
			"user_id": float64(customer.Id), "available": map[string]any{"nanos": "3", "currency": "USD"},
			"outstanding": zero, "consumed": map[string]any{"nanos": "29", "currency": "USD"}, "sale_pending": true, "funding_pending": false, "requests_paused": true,
		}, rows[0], "primary facts remain visible without publishing any log rows")
		assert.Equal(t, zero, rows[1].(map[string]any)["consumed"], "another user's consumption cannot leak into the row")
		self := walletAPIRequest(t, engine, selfOnly, http.MethodGet, fmt.Sprintf("/api/user/self/wallet?user_id=%d", admin.Id), "")
		require.True(t, self.Success, self.Message)
		assert.Equal(t, float64(customer.Id), self.Data["user_id"])
		assert.Equal(t, "0.000000003", self.Data["balance"])
		assert.Equal(t, map[string]any{"nanos": "29", "currency": "USD"}, self.Data["consumed"])
		assert.Equal(t, true, self.Data["sale_pending"])
		profileOnly := createWalletTestAccessToken(t, customer.Id, "profile:read")
		assert.Equal(t, "HTTP 403", walletAPIRequest(t, engine, profileOnly, http.MethodGet, "/api/user/self/wallet", "").Message)
	})

	t.Run("unbounded cumulative amount and exact balance", func(t *testing.T) {
		large := model.User{Username: "summary-large", AffCode: "summary-large", Status: common.UserStatusEnabled}
		require.NoError(t, db.Create(&large).Error)
		charge := money.Amount{Nanos: 5_000_000_000_000_000_000, Currency: "USD"}
		for index := range 2 {
			_, err := model.TopUpWallet(model.WalletTopUp{UserId: large.Id, Source: charge, OperatorId: admin.Id, Remark: "historical cash"})
			require.NoError(t, err)
			request := model.GatewayRequest{ID: fmt.Sprintf("summary-large-%d", index), UserID: large.Id, SaleCurrency: "USD", SaleFXRate: "1", Receivable: money.Amount{Currency: "USD"}}
			require.NoError(t, model.ReserveGateway(&request, request.Receivable, request.Receivable))
			request.Receivable = charge
			require.NoError(t, model.PrepareGatewaySettlement(model.GatewaySettlement{Request: request, SaleSource: charge}))
			_, err = model.SettleGateway(request.ID)
			require.NoError(t, err)
			_, err = model.PostLedgerEntries(model.LedgerPosting{Account: model.PlatformRevenueAccount("USD"), Type: model.LedgerEntryAdjustment, Amount: money.Amount{Nanos: -charge.Nanos, Currency: "USD"}, Remark: "Unallocated platform correction"})
			require.NoError(t, err)
		}
		_, err := model.TopUpWallet(model.WalletTopUp{UserId: large.Id, Source: money.Amount{Nanos: 9007199254740993, Currency: "USD"}, OperatorId: admin.Id, Remark: "exact cash"})
		require.NoError(t, err)
		response := walletAPIRequest(t, engine, token, http.MethodGet, fmt.Sprintf("/api/admin/v1/user-wallets?user_ids=%d", large.Id), "")
		require.True(t, response.Success, response.Message)
		rows := response.Data["items"].([]any)
		require.Len(t, rows, 1)
		assert.Equal(t, map[string]any{"nanos": "10000000000000000000", "currency": "USD"}, rows[0].(map[string]any)["consumed"])
		assert.Equal(t, map[string]any{"nanos": "9007199254740993", "currency": "USD"}, rows[0].(map[string]any)["available"])
		_, err = model.PostLedgerEntries(model.LedgerPosting{Account: model.PlatformRevenueAccount("EUR"), Type: model.LedgerEntryCharge, Amount: money.Amount{Nanos: 1, Currency: "EUR"}, RequestId: "summary-large-0"})
		require.NoError(t, err)
		_, err = model.GetUserWalletSummaries(t.Context(), []int{large.Id})
		require.ErrorIs(t, err, money.ErrCurrencyMismatch, "mixed currencies cannot be silently summed")
	})
}

// The wallet admin API writes every balance change as a ledger entry. Admins
// may read wallets by default; only root may change them until it grants
// ledger:write, and nobody may change the wallet of an equal or higher role.
func TestWalletAdminAPI(t *testing.T) {
	previousMaster, previousRedis := common.IsMasterNode, common.RedisEnabled
	t.Cleanup(func() { common.IsMasterNode, common.RedisEnabled = previousMaster, previousRedis })
	common.IsMasterNode, common.RedisEnabled = true, false
	db := model.UseTestDatabases(t)
	require.NoError(t, i18n.Init())
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.UserSession{}, &model.CasbinRule{}, &model.AuthzRole{}, &model.UserAccessToken{}, &model.Option{}, &model.Wallet{}, &model.LedgerEntry{}))
	require.NoError(t, model.MigrateGatewayBilling(db))
	require.NoError(t, model.MigrateAuditLogs())
	require.NoError(t, model.EnsureLegacyAccessTokenRetireAt(time.Now().Unix()))
	require.NoError(t, authz.Init(db))

	users := map[string]*model.User{}
	for _, user := range []struct {
		name string
		role int
	}{{"root", common.RoleRootUser}, {"admin", common.RoleAdminUser}, {"customer", common.RoleCommonUser}, {"other-admin", common.RoleAdminUser}} {
		created := &model.User{Username: user.name, Password: "placeholder", Role: user.role, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: user.name}
		require.NoError(t, db.Create(created).Error)
		users[user.name] = created
	}
	root := createWalletTestAccessToken(t, users["root"].Id, "ledger:read", "ledger:write")
	admin := createWalletTestAccessToken(t, users["admin"].Id, "ledger:read", "ledger:write")
	readOnlyRoot := createWalletTestAccessToken(t, users["root"].Id, "ledger:read")
	customer := createWalletTestAccessToken(t, users["customer"].Id, "wallet:read")
	other := createWalletTestAccessToken(t, users["other-admin"].Id, "wallet:read")
	profileOnly := createWalletTestAccessToken(t, users["customer"].Id, "profile:read")

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	SetApiRouter(engine)
	wallet := fmt.Sprintf("/api/admin/v1/user-wallets/%d", users["customer"].Id)

	empty := walletAPIRequest(t, engine, admin, http.MethodGet, wallet, "")
	require.True(t, empty.Success, empty.Message)
	assert.Equal(t, map[string]any{"user_id": float64(users["customer"].Id), "currency": "USD", "balance": "0", "updated_at": float64(0), "consumed": map[string]any{"nanos": "0", "currency": "USD"}, "sale_pending": false, "funding_pending": false}, empty.Data)

	selfPath := "/api/user/self/wallet"
	selfEmpty := walletAPIRequest(t, engine, customer, http.MethodGet, selfPath, "")
	require.True(t, selfEmpty.Success, selfEmpty.Message)
	assert.Equal(t, empty.Data, selfEmpty.Data)
	selfEmptyEntries := walletAPIRequest(t, engine, customer, http.MethodGet, selfPath+"/entries", "")
	require.True(t, selfEmptyEntries.Success, selfEmptyEntries.Message)
	assert.Empty(t, selfEmptyEntries.Data["items"])
	assert.Equal(t, float64(0), selfEmptyEntries.Data["total"])
	for _, path := range []string{selfPath, selfPath + "/entries"} {
		assert.Equal(t, "HTTP 403", walletAPIRequest(t, engine, profileOnly, http.MethodGet, path, "").Message)
		assert.Equal(t, "HTTP 401", walletAPIRequest(t, engine, "", http.MethodGet, path, "").Message)
	}
	assert.Equal(t, "HTTP 403", walletAPIRequest(t, engine, customer, http.MethodGet, wallet, "").Message)

	topUp := `{"amount":"100","currency":"CNY","fx_rate":"0.1385","reference":"bank-1","remark":"wire transfer"}`
	assert.Equal(t, "HTTP 403", walletAPIRequest(t, engine, admin, http.MethodPost, wallet+"/top-ups", topUp).Message, "admins cannot change balances by default")
	assert.Equal(t, "HTTP 403", walletAPIRequest(t, engine, readOnlyRoot, http.MethodPost, wallet+"/top-ups", topUp).Message, "the token scope applies")

	credited := walletAPIRequest(t, engine, root, http.MethodPost, wallet+"/top-ups", topUp)
	require.True(t, credited.Success, credited.Message)
	assert.Equal(t, "top_up", credited.Data["type"])
	assert.Equal(t, "USD", credited.Data["currency"])
	assert.Equal(t, "13.85", credited.Data["amount"])
	assert.Equal(t, "13.85", credited.Data["balance_after"])
	assert.Equal(t, "CNY", credited.Data["source_currency"])
	assert.Equal(t, "100", credited.Data["source_amount"])
	assert.Equal(t, "0.1385", credited.Data["fx_rate"])
	assert.Equal(t, float64(users["root"].Id), credited.Data["operator_id"])
	assert.Equal(t, "wire transfer", credited.Data["remark"])

	for _, tc := range []struct {
		name        string
		path        string
		body        string
		wantMessage string
	}{
		{name: "same reference", path: wallet + "/top-ups", body: topUp, wantMessage: "This reference has already been credited"},
		{name: "other currency without a rate", path: wallet + "/top-ups", body: `{"amount":"1","currency":"EUR"}`, wantMessage: "An exchange rate is required for a top-up in another currency"},
		{name: "invalid currency", path: wallet + "/top-ups", body: `{"amount":"1","currency":"usd"}`, wantMessage: "Invalid currency"},
		{name: "negative top-up", path: wallet + "/top-ups", body: `{"amount":"-1"}`, wantMessage: "Invalid amount"},
		{name: "non-positive rate", path: wallet + "/top-ups", body: `{"amount":"1","currency":"EUR","fx_rate":"0"}`, wantMessage: "Invalid exchange rate"},
		{name: "amount finer than 10^-9", path: wallet + "/adjustments", body: `{"amount":"0.0000000001","remark":"x"}`, wantMessage: "Invalid amount"},
		{name: "amount beyond int64", path: wallet + "/adjustments", body: `{"amount":"9223372037","remark":"x"}`, wantMessage: "Invalid amount"},
		{name: "adjustment without a remark", path: wallet + "/adjustments", body: `{"amount":"-1"}`, wantMessage: "Remark is required"},
		{name: "adjustment below zero", path: wallet + "/adjustments", body: `{"amount":"-20","remark":"chargeback"}`, wantMessage: "Insufficient balance"},
		{name: "unknown user", path: "/api/admin/v1/user-wallets/999999/adjustments", body: `{"amount":"1","remark":"bonus"}`, wantMessage: "User does not exist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := walletAPIRequest(t, engine, root, http.MethodPost, tc.path, tc.body)
			assert.False(t, response.Success)
			assert.Equal(t, tc.wantMessage, response.Message)
		})
	}

	adjusted := walletAPIRequest(t, engine, root, http.MethodPost, wallet+"/adjustments", `{"amount":"-3.85","remark":"service credit expired"}`)
	require.True(t, adjusted.Success, adjusted.Message)
	assert.Equal(t, "adjustment", adjusted.Data["type"])
	assert.Equal(t, "-3.85", adjusted.Data["amount"])
	assert.Equal(t, "10", adjusted.Data["balance_after"])
	assert.Nil(t, adjusted.Data["fx_rate"])

	reversalPath := fmt.Sprintf("/api/admin/v1/ledger-entries/%d/reversal", int64(adjusted.Data["id"].(float64)))
	assert.Equal(t, "Remark is required", walletAPIRequest(t, engine, root, http.MethodPost, reversalPath, `{}`).Message)
	reversed := walletAPIRequest(t, engine, root, http.MethodPost, reversalPath, `{"remark":"adjusted by mistake"}`)
	require.True(t, reversed.Success, reversed.Message)
	assert.Equal(t, "3.85", reversed.Data["amount"])
	assert.Equal(t, "13.85", reversed.Data["balance_after"])
	assert.Equal(t, adjusted.Data["id"], reversed.Data["reverses_entry_id"])
	assert.Equal(t, "This ledger entry has already been reversed", walletAPIRequest(t, engine, root, http.MethodPost, reversalPath, `{"remark":"again"}`).Message)
	assert.Equal(t, "Ledger entry not found", walletAPIRequest(t, engine, root, http.MethodPost, "/api/admin/v1/ledger-entries/999999/reversal", `{"remark":"x"}`).Message)

	balance := walletAPIRequest(t, engine, admin, http.MethodGet, wallet, "")
	require.True(t, balance.Success, balance.Message)
	assert.Equal(t, "13.85", balance.Data["balance"])

	entries := walletAPIRequest(t, engine, admin, http.MethodGet, wallet+"/entries?p=1&page_size=2", "")
	require.True(t, entries.Success, entries.Message)
	assert.Equal(t, float64(3), entries.Data["total"])
	items := entries.Data["items"].([]any)
	require.Len(t, items, 2)
	assert.Equal(t, reversed.Data["id"], items[0].(map[string]any)["id"], "newest first")
	assert.Equal(t, adjusted.Data["id"], items[1].(map[string]any)["id"])

	// Once root grants ledger:write, an admin may change the wallet of a
	// regular user but not of another admin.
	require.NoError(t, authz.SetUserPermissions(users["admin"].Id, authz.PermissionsMap{authz.ResourceLedger: {authz.ActionRead: true, authz.ActionWrite: true}}))
	granted := walletAPIRequest(t, engine, admin, http.MethodPost, wallet+"/adjustments", `{"amount":"1","remark":"goodwill"}`)
	require.True(t, granted.Success, granted.Message)
	assert.Equal(t, "14.85", granted.Data["balance_after"])

	self := walletAPIRequest(t, engine, customer, http.MethodGet, selfPath+"?user_id="+fmt.Sprint(users["root"].Id), "")
	require.True(t, self.Success, self.Message)
	assert.Equal(t, "14.85", self.Data["balance"])
	assert.Equal(t, "USD", self.Data["currency"])
	assert.Equal(t, float64(users["customer"].Id), self.Data["user_id"])
	otherSelf := walletAPIRequest(t, engine, other, http.MethodGet, selfPath+"?user_id="+fmt.Sprint(users["customer"].Id), "")
	require.True(t, otherSelf.Success, otherSelf.Message)
	assert.Equal(t, "0", otherSelf.Data["balance"])
	selfEntries := walletAPIRequest(t, engine, customer, http.MethodGet, selfPath+"/entries?p=1&page_size=2", "")
	require.True(t, selfEntries.Success, selfEntries.Message)
	assert.Equal(t, float64(4), selfEntries.Data["total"])
	selfItems := selfEntries.Data["items"].([]any)
	require.Len(t, selfItems, 2)
	assert.Equal(t, granted.Data["id"], selfItems[0].(map[string]any)["id"])
	assert.Equal(t, "14.85", selfItems[0].(map[string]any)["balance_after"])
	for _, item := range selfItems {
		entry := item.(map[string]any)
		assert.NotContains(t, entry, "operator_id")
		assert.NotContains(t, entry, "remark")
		assert.NotContains(t, entry, "attempt_id")
	}
	otherEntries := walletAPIRequest(t, engine, other, http.MethodGet, selfPath+"/entries?user_id="+fmt.Sprint(users["customer"].Id), "")
	require.True(t, otherEntries.Success, otherEntries.Message)
	assert.Equal(t, float64(0), otherEntries.Data["total"])
	assert.Empty(t, otherEntries.Data["items"])
	otherAdmin := fmt.Sprintf("/api/admin/v1/user-wallets/%d/adjustments", users["other-admin"].Id)
	assert.Equal(t, "No permission to update users of same or higher permission level", walletAPIRequest(t, engine, admin, http.MethodPost, otherAdmin, `{"amount":"1","remark":"goodwill"}`).Message)

	var actions []string
	require.NoError(t, model.LOG_DB.Table("audit_logs").Where("user_id = ? AND category = ? AND success = ?", users["root"].Id, model.AuditCategoryOperation, true).Pluck("action", &actions).Error)
	assert.ElementsMatch(t, []string{"wallet.top_up", "wallet.adjust", "wallet.reverse_entry"}, actions)

	t.Run("referral rewards retain account owner authorization", func(t *testing.T) {
		for _, target := range []string{"other-admin", "root", "customer"} {
			posted, err := model.PostLedgerEntries(model.LedgerPosting{
				Account: model.LedgerAccount{Type: model.LedgerAccountUserReferral, OwnerId: users[target].Id, Currency: "USD"},
				Type:    model.LedgerEntryTopUp, Amount: money.Amount{Nanos: 10, Currency: "USD"},
			})
			require.NoError(t, err)
			path := fmt.Sprintf("/api/admin/v1/ledger-entries/%d/reversal", posted[0].Id)
			body := `{"remark":"correct invitation reward"}`
			assert.Equal(t, "HTTP 403", walletAPIRequest(t, engine, readOnlyRoot, http.MethodPost, path, body).Message)
			assert.Equal(t, "HTTP 403", walletAPIRequest(t, engine, customer, http.MethodPost, path, body).Message)
			response := walletAPIRequest(t, engine, admin, http.MethodPost, path, body)
			var balance model.Wallet
			require.NoError(t, db.First(&balance, posted[0].WalletId).Error)
			var reversals int64
			require.NoError(t, db.Model(&model.LedgerEntry{}).Where("reverses_entry_id = ?", posted[0].Id).Count(&reversals).Error)
			if target != "customer" {
				assert.False(t, response.Success, target)
				assert.Equal(t, "No permission to update users of same or higher permission level", response.Message, target)
				assert.EqualValues(t, 10, balance.Balance.Nanos, "unauthorized reversal must leave the referral balance unchanged")
				assert.Zero(t, reversals, "unauthorized reversal must not append an entry")
				continue
			}
			require.True(t, response.Success, response.Message)
			assert.Zero(t, balance.Balance.Nanos)
			assert.EqualValues(t, 1, reversals)
			assert.Equal(t, "This ledger entry has already been reversed", walletAPIRequest(t, engine, admin, http.MethodPost, path, body).Message)
		}
	})
}

func TestSubscriptionExactPurchaseAPI(t *testing.T) {
	engine, admin, _ := newAdminTestServer(t)
	db := model.DB
	require.NoError(t, db.AutoMigrate(&model.SubscriptionOrder{}, &model.TopUp{}, &model.Log{}))
	require.NoError(t, model.MigrateGatewayBilling(db))
	require.NoError(t, model.MigrateSubscriptionMoney(db))
	configured := operation_setting.GetPaymentSetting()
	previous := *configured
	t.Cleanup(func() { *configured = previous })
	configured.ComplianceConfirmed = true
	configured.ComplianceTermsVersion = operation_setting.CurrentComplianceTermsVersion
	adminToken := createWalletTestAccessToken(t, admin.Id, "billing:read", "billing:write")
	customer := model.User{Username: "subscription-customer", AffCode: "subscription-customer", Status: common.UserStatusEnabled, Role: common.RoleCommonUser, AuthVersion: 1, Quota: 999999}
	require.NoError(t, db.Create(&customer).Error)
	writer := createWalletTestAccessToken(t, customer.Id, "wallet:write", "wallet:read")
	reader := createWalletTestAccessToken(t, customer.Id, "wallet:read")
	body := `{"plan":{"title":"Exact API plan","price":{"nanos":"1000000001","currency":"USD"},"allowance":{"nanos":"9007199254740993","currency":"USD"},"unlimited":false,"enabled":true}}`
	for _, invalid := range []string{strings.Replace(body, `"price":`, `"price_amount":1,"price":`, 1), strings.Replace(body, `"unlimited":false`, `"total_amount":500000,"unlimited":false`, 1), strings.Replace(body, `"nanos":"1000000001"`, `"nanos":1000000001`, 1), strings.Replace(body, `"currency":"USD"`, `"currency":"usd"`, 1), strings.Replace(body, `"unlimited":false`, `"unlimited":true`, 1)} {
		response := walletAPIRequest(t, engine, adminToken, http.MethodPost, "/api/subscription/admin/plans", invalid)
		assert.False(t, response.Success)
	}
	var planCount int64
	require.NoError(t, db.Model(&model.SubscriptionPlan{}).Count(&planCount).Error)
	assert.Zero(t, planCount)
	response := walletAPIRequest(t, engine, adminToken, http.MethodPost, "/api/subscription/admin/plans", body)
	require.True(t, response.Success, response.Message)
	assert.Equal(t, map[string]any{"nanos": "1000000001", "currency": "USD"}, response.Data["price"])
	for _, key := range []string{"price_amount", "total_amount", "currency"} {
		assert.NotContains(t, response.Data, key)
	}
	planID := int(response.Data["id"].(float64))
	_, err := model.PostLedgerEntries(model.LedgerPosting{Account: model.UserWalletAccount(customer.Id), Type: model.LedgerEntryOpeningBalance, Amount: money.Amount{Nanos: 1000000002, Currency: "USD"}})
	require.NoError(t, err)
	id := uuid.NewString()
	displayed := subscriptionPlanFromAPI(t, engine, writer, planID)
	revision, ok := displayed["purchase_revision"].(string)
	require.True(t, ok)
	require.NotEmpty(t, revision)
	purchase := fmt.Sprintf(`{"plan_id":%d,"purchase_id":%q,"expected_revision":%q}`, planID, id, revision)
	assert.Equal(t, "HTTP 401", walletAPIRequest(t, engine, "", http.MethodPost, "/api/subscription/balance/pay", purchase).Message)
	assert.Equal(t, "HTTP 403", walletAPIRequest(t, engine, reader, http.MethodPost, "/api/subscription/balance/pay", purchase).Message)
	for _, invalid := range []string{fmt.Sprintf(`{"plan_id":%d}`, planID), fmt.Sprintf(`{"plan_id":%d,"purchase_id":%q}`, planID, id), strings.Replace(purchase, id, "not-a-uuid", 1), strings.Replace(purchase, revision, "not-a-revision", 1), strings.Replace(purchase, fmt.Sprintf("%q", revision), "null", 1), strings.Replace(purchase, fmt.Sprintf("%q", revision), "123", 1), strings.Replace(purchase, `"expected_revision":`, `"expected_revision":"duplicate","expected_revision":`, 1), strings.Replace(purchase, `"purchase_id":`, `"amount":1,"purchase_id":`, 1), strings.Replace(purchase, `"plan_id":`, `"plan_id":0,"plan_id":`, 1)} {
		assert.False(t, walletAPIRequest(t, engine, writer, http.MethodPost, "/api/subscription/balance/pay", invalid).Success)
	}
	response = walletAPIRequest(t, engine, writer, http.MethodPost, "/api/subscription/balance/pay", purchase)
	require.True(t, response.Success, response.Message)
	orderID := response.Data["order_id"]
	assert.NotEmpty(t, orderID)
	assert.Equal(t, map[string]any{"nanos": "1000000001", "currency": "USD"}, response.Data["wallet_charge"])
	assert.Nil(t, response.Data["fx_rate"])
	period := response.Data["period"].(map[string]any)
	assert.Equal(t, map[string]any{"nanos": "9007199254740993", "currency": "USD"}, period["available"])
	assert.NotContains(t, response.Data["subscription"], "amount_used")
	assert.NotContains(t, response.Data["subscription"], "amount_total")
	assert.Equal(t, revision, response.Data["purchase_revision"])
	changedBody := strings.Replace(body, `"nanos":"1000000001"`, `"nanos":"2000000001"`, 1)
	changedBody = strings.Replace(changedBody, `"unlimited":false`, `"duration_unit":"hour","duration_value":2,"quota_reset_period":"custom","quota_reset_custom_seconds":600,"unlimited":false`, 1)
	changed := walletAPIRequest(t, engine, adminToken, http.MethodPut, fmt.Sprintf("/api/subscription/admin/plans/%d", planID), changedBody)
	require.True(t, changed.Success, changed.Message)
	latest := subscriptionPlanFromAPI(t, engine, writer, planID)
	latestRevision := latest["purchase_revision"].(string)
	assert.NotEqual(t, revision, latestRevision)
	disabled := walletAPIRequest(t, engine, adminToken, http.MethodPatch, fmt.Sprintf("/api/subscription/admin/plans/%d", planID), `{"enabled":false}`)
	require.True(t, disabled.Success, disabled.Message)
	replay := walletAPIRequest(t, engine, writer, http.MethodPost, "/api/subscription/balance/pay", purchase)
	require.True(t, replay.Success, replay.Message)
	assert.Equal(t, response.Data, replay.Data, "accepted UUID replays the displayed original contract despite later plan edits and disable")
	tampered := adminSecurityRequest(engine, writer, http.MethodPost, "/api/subscription/balance/pay", strings.Replace(purchase, revision, latestRevision, 1))
	assert.Equal(t, http.StatusConflict, tampered.Code)
	var tamperedBody map[string]any
	require.NoError(t, common.Unmarshal(tampered.Body.Bytes(), &tamperedBody))
	assert.Equal(t, "subscription_purchase_revision_conflict", tamperedBody["code"])
	assert.NotContains(t, tamperedBody, "accepted", "accepted IDs must never be labelled safe to discard")
	conflict := fmt.Sprintf(`{"plan_id":%d,"purchase_id":%q,"expected_revision":%q}`, planID+1, id, revision)
	assert.Equal(t, "HTTP 409", walletAPIRequest(t, engine, writer, http.MethodPost, "/api/subscription/balance/pay", conflict).Message)
	self := walletAPIRequest(t, engine, writer, http.MethodGet, "/api/subscription/self", "")
	require.True(t, self.Success, self.Message)
	require.NoError(t, db.Exec("ALTER TABLE legacy_subscription_obligations RENAME TO failed_subscription_evidence").Error)
	failedRead := walletAPIRequest(t, engine, writer, http.MethodGet, "/api/subscription/self", "")
	assert.False(t, failedRead.Success, "failed financial evidence reads must not masquerade as empty subscriptions")
	require.NoError(t, db.Exec("ALTER TABLE failed_subscription_evidence RENAME TO legacy_subscription_obligations").Error)
	retried := walletAPIRequest(t, engine, writer, http.MethodGet, "/api/subscription/self", "")
	require.True(t, retried.Success, retried.Message)
	assert.Len(t, retried.Data["all_subscriptions"], 1)
	assert.Len(t, retried.Data["subscriptions"], 1)
	wallet, err := model.GetUserWallet(customer.Id)
	require.NoError(t, err)
	assert.EqualValues(t, 1, wallet.Balance.Nanos)
	var retained model.User
	require.NoError(t, db.First(&retained, customer.Id).Error)
	assert.Equal(t, 999999, retained.Quota)
	var logs int64
	require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("user_id = ? AND type = ?", customer.Id, model.LogTypeTopup).Count(&logs).Error)
	assert.EqualValues(t, 1, logs, "same purchase replay must not duplicate payment logs")
}

func subscriptionPlanFromAPI(t *testing.T, engine http.Handler, token string, planID int) map[string]any {
	t.Helper()
	response := adminSecurityRequest(engine, token, http.MethodGet, "/api/subscription/plans", "")
	require.Equal(t, http.StatusOK, response.Code)
	var envelope struct {
		Success bool `json:"success"`
		Data    []struct {
			Plan map[string]any `json:"plan"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &envelope))
	require.True(t, envelope.Success)
	for _, item := range envelope.Data {
		if int(item.Plan["id"].(float64)) == planID {
			return item.Plan
		}
	}
	require.FailNow(t, "subscription plan was not returned")
	return nil
}

func TestSubscriptionPurchaseConfirmationRevision(t *testing.T) {
	for _, tc := range []struct{ name, before, after string }{
		{"price", `"nanos":"1000000000"`, `"nanos":"2000000000"`},
		{"allowance", `"nanos":"9007199254740993"`, `"nanos":"9007199254740994"`},
		{"reset", `"quota_reset_custom_seconds":600`, `"quota_reset_custom_seconds":1200`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, admin, _ := newAdminTestServer(t)
			db := model.DB
			require.NoError(t, db.AutoMigrate(&model.SubscriptionOrder{}, &model.TopUp{}, &model.Log{}))
			require.NoError(t, model.MigrateGatewayBilling(db))
			require.NoError(t, model.MigrateSubscriptionMoney(db))
			configured := operation_setting.GetPaymentSetting()
			previous := *configured
			t.Cleanup(func() { *configured = previous })
			configured.ComplianceConfirmed = true
			configured.ComplianceTermsVersion = operation_setting.CurrentComplianceTermsVersion
			adminToken := createWalletTestAccessToken(t, admin.Id, "billing:read", "billing:write")
			customer := model.User{Username: "confirm-customer", AffCode: "confirm-customer", Status: common.UserStatusEnabled, Role: common.RoleCommonUser, AuthVersion: 1, Quota: 999999}
			require.NoError(t, db.Create(&customer).Error)
			writer := createWalletTestAccessToken(t, customer.Id, "wallet:write", "wallet:read")
			body := `{"plan":{"title":"Confirmed plan","price":{"nanos":"1000000000","currency":"USD"},"allowance":{"nanos":"9007199254740993","currency":"USD"},"unlimited":false,"enabled":true,"duration_unit":"hour","duration_value":2,"quota_reset_period":"custom","quota_reset_custom_seconds":600}}`
			created := walletAPIRequest(t, engine, adminToken, http.MethodPost, "/api/subscription/admin/plans", body)
			require.True(t, created.Success, created.Message)
			planID := int(created.Data["id"].(float64))
			displayed := subscriptionPlanFromAPI(t, engine, writer, planID)
			_, err := model.PostLedgerEntries(model.LedgerPosting{Account: model.UserWalletAccount(customer.Id), Type: model.LedgerEntryOpeningBalance, Amount: money.Amount{Nanos: 10_000_000_000, Currency: "USD"}})
			require.NoError(t, err)
			var beforeLedger int64
			require.NoError(t, db.Model(&model.LedgerEntry{}).Count(&beforeLedger).Error)
			updated := walletAPIRequest(t, engine, adminToken, http.MethodPut, fmt.Sprintf("/api/subscription/admin/plans/%d", planID), strings.Replace(body, tc.before, tc.after, 1))
			require.True(t, updated.Success, updated.Message)
			revision, ok := displayed["purchase_revision"].(string)
			require.True(t, ok)
			require.NotEmpty(t, revision)
			purchase := map[string]any{"plan_id": planID, "purchase_id": uuid.NewString(), "expected_revision": revision}
			encoded, err := common.Marshal(purchase)
			require.NoError(t, err)
			response := adminSecurityRequest(engine, writer, http.MethodPost, "/api/subscription/balance/pay", string(encoded))
			assert.Equal(t, http.StatusConflict, response.Code, response.Body.String())
			var rejected map[string]any
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &rejected))
			assert.Equal(t, "subscription_purchase_terms_changed", rejected["code"])
			assert.Equal(t, false, rejected["accepted"])
			for _, table := range []any{&model.SubscriptionOrder{}, &model.SubscriptionOrderMoney{}, &model.UserSubscription{}, &model.SubscriptionPeriod{}} {
				var count int64
				require.NoError(t, db.Model(table).Count(&count).Error)
				assert.Zero(t, count, "unconfirmed replacement terms cannot create %T", table)
			}
			var afterLedger int64
			require.NoError(t, db.Model(&model.LedgerEntry{}).Count(&afterLedger).Error)
			assert.Equal(t, beforeLedger, afterLedger)
			wallet, err := model.GetUserWallet(customer.Id)
			require.NoError(t, err)
			assert.EqualValues(t, 10_000_000_000, wallet.Balance.Nanos)
			latest := subscriptionPlanFromAPI(t, engine, writer, planID)
			assert.NotEqual(t, revision, latest["purchase_revision"])
			assert.Equal(t, latest["purchase_revision"], subscriptionPlanFromAPI(t, engine, writer, planID)["purchase_revision"], "unchanged terms have a deterministic revision")
			purchase["expected_revision"] = latest["purchase_revision"]
			purchase["purchase_id"] = uuid.NewString()
			encoded, err = common.Marshal(purchase)
			require.NoError(t, err)
			confirmed := walletAPIRequest(t, engine, writer, http.MethodPost, "/api/subscription/balance/pay", string(encoded))
			require.True(t, confirmed.Success, confirmed.Message)
			assert.Equal(t, latest["price"], confirmed.Data["wallet_charge"])
			assert.Equal(t, latest["purchase_revision"], confirmed.Data["purchase_revision"])
		})
	}
}
