package controller

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupRedemptionControllerTest(t *testing.T) {
	t.Helper()
	confirmPaymentComplianceForTest(t)
	previousRedis := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = previousRedis })
	db := model.UseTestDatabases(t)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Redemption{}, &model.AuditLog{}, &model.Log{}, &model.Wallet{}, &model.LedgerEntry{}))
	require.NoError(t, db.Create(&model.User{Id: 17, Username: "redemption-management", Quota: 77, Status: common.UserStatusEnabled}).Error)
}

func performRedemptionRequest(t *testing.T, method, path, body string, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	router := gin.New()
	router.Use(middleware.RequestId(), func(c *gin.Context) {
		c.Set("id", 17)
		c.Set("role", common.RoleAdminUser)
		c.Next()
	})
	router.Handle(method, "/redemption", handler)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	return response
}

func TestRedemptionCreateAcceptsOnlyMoneyAndServerOwnedIdentity(t *testing.T) {
	setupRedemptionControllerTest(t)
	valid := map[string]any{"name": "money-vouchers", "count": 2, "amount": money.Amount{Nanos: 9223372036854775807, Currency: "USD"}, "expired_time": int64(0)}
	for _, field := range []string{"quota", "id", "user_id", "key", "status", "used_user_id", "redeemed_time", "created_time", "DeletedAt"} {
		payload := make(map[string]any, len(valid)+1)
		for key, value := range valid {
			payload[key] = value
		}
		payload[field] = 1
		body, err := common.Marshal(payload)
		require.NoError(t, err)
		response := performRedemptionRequest(t, http.MethodPost, "/redemption", string(body), AddRedemption)
		var result struct{ Success bool }
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
		assert.False(t, result.Success, "client field %s must be rejected", field)
	}
	for _, amount := range []string{`null`, `{"nanos":1,"currency":"USD"}`, `{"nanos":"0.1","currency":"USD"}`, `{"nanos":"9223372036854775808","currency":"USD"}`, `{"nanos":"0","currency":"USD"}`, `{"nanos":"-1","currency":"USD"}`, `{"nanos":"1","currency":"usd"}`, `{"nanos":"1","currency":"USD","quota":1}`} {
		response := performRedemptionRequest(t, http.MethodPost, "/redemption", `{"name":"invalid","count":1,"amount":`+amount+`}`, AddRedemption)
		var result struct{ Success bool }
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
		assert.False(t, result.Success)
	}
	var count int64
	require.NoError(t, model.DB.Model(&model.Redemption{}).Count(&count).Error)
	assert.Zero(t, count, "rejected requests must never partially create codes")
	body, err := common.Marshal(valid)
	require.NoError(t, err)
	response := performRedemptionRequest(t, http.MethodPost, "/redemption", string(body), AddRedemption)
	var result struct {
		Success bool
		Data    []string
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
	require.True(t, result.Success)
	require.Len(t, result.Data, 2)
	var rows []model.Redemption
	require.NoError(t, model.DB.Find(&rows).Error)
	require.Len(t, rows, 2)
	for _, row := range rows {
		assert.Equal(t, money.Amount{Nanos: 9223372036854775807, Currency: "USD"}, row.Amount)
		assert.Equal(t, 17, row.UserId)
		assert.Equal(t, common.RedemptionCodeStatusEnabled, row.Status)
		assert.Zero(t, row.UsedUserId)
		assert.Zero(t, row.RedeemedTime)
	}
	var audit model.AuditLog
	require.NoError(t, model.AuditLogQuery().Where("action = ?", "redemption.create").First(&audit).Error)
	encoded, err := common.Marshal(audit)
	require.NoError(t, err)
	for _, key := range result.Data {
		assert.NotContains(t, string(encoded), key)
	}
	require.NotNil(t, audit.Other.Op)
	encoded, err = common.Marshal(audit.Other.Op.Params)
	require.NoError(t, err)
	assert.JSONEq(t, `{"amount":{"nanos":"9223372036854775807","currency":"USD"},"count":2,"name":"money-vouchers"}`, string(encoded))
	assert.NotContains(t, string(encoded), `"quota"`)
}

func TestRedemptionUpdatePreservesHistoryAndStatusToggles(t *testing.T) {
	setupRedemptionControllerTest(t)
	voucher := model.Redemption{Name: "original", UserId: 17, Key: "10000000000000000000000000000001", Amount: money.Amount{Nanos: 1, Currency: "USD"}}
	require.NoError(t, voucher.Insert())
	for _, field := range []string{"quota", "user_id", "key", "status", "used_user_id", "redeemed_time", "created_time"} {
		payload := map[string]any{"id": voucher.Id, "name": "changed", "amount": money.Amount{Nanos: 2, Currency: "CNY"}, "expired_time": 0, field: 1}
		body, err := common.Marshal(payload)
		require.NoError(t, err)
		response := performRedemptionRequest(t, http.MethodPut, "/redemption", string(body), UpdateRedemption)
		var result struct{ Success bool }
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
		assert.False(t, result.Success)
	}
	stored, err := model.GetRedemptionById(voucher.Id)
	require.NoError(t, err)
	assert.Equal(t, voucher, *stored)
	payload, err := common.Marshal(map[string]any{"id": voucher.Id, "name": "changed", "amount": money.Amount{Nanos: 3, Currency: "CNY"}, "expired_time": 0})
	require.NoError(t, err)
	response := performRedemptionRequest(t, http.MethodPut, "/redemption", string(payload), UpdateRedemption)
	var result struct {
		Success bool
		Data    model.Redemption
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
	require.True(t, result.Success)
	assert.Equal(t, money.Amount{Nanos: 3, Currency: "CNY"}, result.Data.Amount)
	for _, status := range []int{common.RedemptionCodeStatusDisabled, common.RedemptionCodeStatusEnabled} {
		body := fmt.Sprintf(`{"id":%d,"status":%d}`, voucher.Id, status)
		response := performRedemptionRequest(t, http.MethodPut, "/redemption?status_only=true", body, UpdateRedemption)
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
		require.True(t, result.Success)
		assert.Equal(t, status, result.Data.Status)
		assert.Equal(t, money.Amount{Nanos: 3, Currency: "CNY"}, result.Data.Amount)
	}
	require.NoError(t, model.DB.Model(&voucher).Updates(map[string]any{"status": common.RedemptionCodeStatusUsed, "used_user_id": 23, "redeemed_time": 100}).Error)
	for _, body := range []string{fmt.Sprintf(`{"id":%d,"status":1}`, voucher.Id), fmt.Sprintf(`{"id":%d,"status":1,"quota":1}`, voucher.Id), fmt.Sprintf(`{"id":%d,"status":3}`, voucher.Id)} {
		response := performRedemptionRequest(t, http.MethodPut, "/redemption?status_only=true", body, UpdateRedemption)
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
		assert.False(t, result.Success)
		assert.NotContains(t, response.Body.String(), "redeem.failed", "internal error identifiers must not reach the administrator")
	}
	stored, err = model.GetRedemptionById(voucher.Id)
	require.NoError(t, err)
	assert.Equal(t, common.RedemptionCodeStatusUsed, stored.Status)
	assert.Equal(t, 23, stored.UsedUserId)
	assert.Equal(t, int64(100), stored.RedeemedTime)
}

func TestRedemptionTopUpReturnsCreditedMoney(t *testing.T) {
	setupRedemptionControllerTest(t)
	amount := money.Amount{Nanos: 9007199254740993, Currency: "USD"}
	voucher := model.Redemption{Name: "precise", Key: "10000000000000000000000000000001", Amount: amount}
	require.NoError(t, voucher.Insert())
	body, err := common.Marshal(map[string]string{"key": voucher.Key})
	require.NoError(t, err)
	response := performRedemptionRequest(t, http.MethodPost, "/redemption", string(body), TopUp)
	var result struct {
		Success bool
		Data    money.Amount
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
	require.True(t, result.Success)
	assert.Equal(t, amount, result.Data)
	assert.Contains(t, response.Body.String(), `"nanos":"9007199254740993"`)
	response = performRedemptionRequest(t, http.MethodPost, "/redemption", string(body), TopUp)
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
	assert.False(t, result.Success)
	wallet, err := model.GetUserWallet(17)
	require.NoError(t, err)
	require.NotNil(t, wallet)
	assert.Equal(t, amount, wallet.Balance)
	var user model.User
	require.NoError(t, model.DB.First(&user, 17).Error)
	assert.Equal(t, 77, user.Quota)
	var logs []model.Log
	require.NoError(t, model.LOG_DB.Find(&logs).Error)
	encoded, err := common.Marshal(logs)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), voucher.Key)
}

func TestDeleteRedemptionBatch(t *testing.T) {
	previousRedis := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = previousRedis })
	// UseTestDatabases restores LOG_DB and the log database type.
	db := model.UseTestDatabases(t)
	logDB, _ := newAuditTestDatabase(t, "postgres")
	model.LOG_DB = logDB
	common.SetLogDatabaseType(common.DatabaseTypePostgreSQL)
	for _, table := range []any{&model.User{}, &model.UserSession{}, &model.Redemption{}} {
		require.False(t, db.Migrator().HasTable(table), "use an empty test database")
		require.NoError(t, db.AutoMigrate(table))
		t.Cleanup(func() { require.NoError(t, db.Migrator().DropTable(table)) })
	}
	require.False(t, logDB.Migrator().HasTable(&model.AuditLog{}), "use an empty test log database")
	require.NoError(t, logDB.AutoMigrate(&model.AuditLog{}))
	t.Cleanup(func() { require.NoError(t, logDB.Migrator().DropTable(&model.AuditLog{})) })
	admin := model.User{Username: "redemption-audit-admin", Password: "unused", Role: common.RoleAdminUser, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1}
	require.NoError(t, db.Create(&admin).Error)
	_, token := createAccessTokenTestSession(t, admin.Id, "redemption-batch-session")
	codes := make([]model.Redemption, 16)
	for index := range codes {
		codes[index] = model.Redemption{Name: "selected", Key: fmt.Sprintf("%032d", index+1), Amount: money.Amount{Nanos: 100, Currency: "USD"}, Status: common.RedemptionCodeStatusEnabled}
	}
	codes[1].Status = common.RedemptionCodeStatusUsed
	codes[15].Name = "unselected"
	codes[15].Status = common.RedemptionCodeStatusDisabled
	require.NoError(t, model.DB.Create(&codes).Error)
	router := gin.New()
	router.Use(middleware.RequestId())
	router.POST("/api/redemption/batch", middleware.AdminAuth(), DeleteRedemptionBatch)

	overLimit := make([]int, 1001)
	for index := range overLimit {
		overLimit[index] = codes[0].Id
	}
	oversized, err := common.Marshal(map[string]any{"ids": overLimit})
	require.NoError(t, err)
	for _, body := range []string{"{}", `{"ids":[]}`, `{"ids":null}`, `{"ids":[0]}`, `{"ids":[1,-1]}`, `{"ids":["1"]}`, "{", string(oversized)} {
		t.Run("invalid_"+body[:min(len(body), 30)], func(t *testing.T) {
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/api/redemption/batch", bytes.NewBufferString(body))
			request.Header.Set("Authorization", "Bearer "+token)
			router.ServeHTTP(response, request)
			var result struct {
				Success bool `json:"success"`
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
			assert.False(t, result.Success)
			var count int64
			require.NoError(t, model.DB.Model(&model.Redemption{}).Count(&count).Error)
			assert.EqualValues(t, 16, count)
			var events []model.AuditLog
			require.NoError(t, logDB.Where("request_id = ? AND category = ?", response.Header().Get(common.RequestIdKey), model.AuditCategoryOperation).Find(&events).Error)
			require.Len(t, events, 1)
			assert.False(t, events[0].Success)
			assert.Equal(t, "redemption.delete_batch", events[0].Action)
		})
	}
	_, err = model.BatchDeleteRedemptions(nil)
	require.Error(t, err)
	requestedIDs := make([]int, 0, 17)
	for _, code := range codes[:15] {
		requestedIDs = append(requestedIDs, code.Id)
	}
	requestedIDs = append(requestedIDs, codes[0].Id, 999999)
	payload, err := common.Marshal(map[string]any{"ids": requestedIDs})
	require.NoError(t, err)
	for _, expectedCount := range []int64{15, 0} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/redemption/batch", bytes.NewReader(payload))
		request.Header.Set("Authorization", "Bearer "+token)
		router.ServeHTTP(response, request)
		assert.Equal(t, http.StatusOK, response.Code)
		var result struct {
			Success bool  `json:"success"`
			Data    int64 `json:"data"`
		}
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
		assert.True(t, result.Success)
		assert.Equal(t, expectedCount, result.Data)
		var events []model.AuditLog
		require.NoError(t, logDB.Where("request_id = ? AND category = ?", response.Header().Get(common.RequestIdKey), model.AuditCategoryOperation).Find(&events).Error)
		require.Len(t, events, 1, "one operation event, without a duplicate single-delete fallback")
		event := events[0]
		assert.Equal(t, "redemption.delete_batch", event.Action)
		assert.Equal(t, fmt.Sprintf("Batch deleted %d redemption codes", expectedCount), event.Content)
		assert.True(t, event.Success)
		assert.Equal(t, admin.Id, event.UserId)
		assert.Equal(t, "/api/redemption/batch", event.Route)
		require.NotNil(t, event.Other.Op)
		encoded, err := common.Marshal(event.Other.Op.Params)
		require.NoError(t, err)
		var params struct {
			Count int64 `json:"count"`
			Total int   `json:"total"`
			IDs   []int `json:"requested_redemption_ids"`
		}
		require.NoError(t, common.Unmarshal(encoded, &params))
		assert.Equal(t, expectedCount, params.Count)
		assert.Equal(t, len(requestedIDs), params.Total)
		assert.Equal(t, requestedIDs, params.IDs)
		encoded, err = common.Marshal(event)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), token)
		for _, code := range codes {
			assert.NotContains(t, string(encoded), code.Key)
		}
	}
	var active []model.Redemption
	require.NoError(t, model.DB.Find(&active).Error)
	require.Len(t, active, 1)
	assert.Equal(t, codes[15], active[0])
	var all []model.Redemption
	require.NoError(t, model.DB.Unscoped().Order("id").Find(&all).Error)
	require.Len(t, all, 16)
	for _, code := range all[:15] {
		assert.True(t, code.DeletedAt.Valid)
	}
	assert.False(t, all[15].DeletedAt.Valid)
}
