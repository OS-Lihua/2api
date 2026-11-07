package router

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"

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

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	SetApiRouter(engine)
	wallet := fmt.Sprintf("/api/admin/v1/user-wallets/%d", users["customer"].Id)

	empty := walletAPIRequest(t, engine, admin, http.MethodGet, wallet, "")
	require.True(t, empty.Success, empty.Message)
	assert.Equal(t, map[string]any{"user_id": float64(users["customer"].Id), "currency": "USD", "balance": "0", "updated_at": float64(0)}, empty.Data)

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
	otherAdmin := fmt.Sprintf("/api/admin/v1/user-wallets/%d/adjustments", users["other-admin"].Id)
	assert.Equal(t, "No permission to update users of same or higher permission level", walletAPIRequest(t, engine, admin, http.MethodPost, otherAdmin, `{"amount":"1","remark":"goodwill"}`).Message)

	var actions []string
	require.NoError(t, model.LOG_DB.Table("audit_logs").Where("user_id = ? AND category = ? AND success = ?", users["root"].Id, model.AuditCategoryOperation, true).Pluck("action", &actions).Error)
	assert.ElementsMatch(t, []string{"wallet.top_up", "wallet.adjust", "wallet.reverse_entry"}, actions)
}
