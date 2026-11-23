package controller

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestIdentityCreationDoesNotSeedLegacyQuota(t *testing.T) {
	for _, flow := range []string{"setup", "default_key"} {
		t.Run(flow, func(t *testing.T) {
			db := setupManageUserTestDB(t)
			require.NoError(t, db.AutoMigrate(&model.Setup{}, &model.Option{}, &model.Token{}))
			require.NoError(t, model.MigrateGatewayBilling(db))

			previousSetup, previousGenerate := constant.Setup, constant.GenerateDefaultToken
			previousRegister, previousPassword, previousEmail := common.RegisterEnabled, common.PasswordRegisterEnabled, common.EmailVerificationEnabled
			previousAuto := setting.DefaultUseAutoGroup
			previousSelf, previousDemo := operation_setting.SelfUseModeEnabled, operation_setting.DemoSiteEnabled
			previousReward, err := common.Marshal(operation_setting.GetRewardSetting())
			require.NoError(t, err)
			common.OptionMapRWMutex.Lock()
			previousOptions := common.OptionMap
			common.OptionMap = map[string]string{}
			common.OptionMapRWMutex.Unlock()
			t.Cleanup(func() {
				constant.Setup, constant.GenerateDefaultToken = previousSetup, previousGenerate
				common.RegisterEnabled, common.PasswordRegisterEnabled, common.EmailVerificationEnabled = previousRegister, previousPassword, previousEmail
				setting.DefaultUseAutoGroup = previousAuto
				operation_setting.SelfUseModeEnabled, operation_setting.DemoSiteEnabled = previousSelf, previousDemo
				require.NoError(t, operation_setting.ApplyRewardOption("reward_setting.config", string(previousReward)))
				common.OptionMapRWMutex.Lock()
				common.OptionMap = previousOptions
				common.OptionMapRWMutex.Unlock()
			})
			constant.Setup, constant.GenerateDefaultToken = false, true
			common.RegisterEnabled, common.PasswordRegisterEnabled, common.EmailVerificationEnabled = true, true, false
			setting.DefaultUseAutoGroup = true
			require.NoError(t, operation_setting.ApplyRewardOption("reward_setting.config", `{"signup":{"nanos":"1500000001","currency":"USD"},"inviter":{"nanos":"0","currency":"USD"},"invitee":{"nanos":"0","currency":"USD"}}`))

			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.POST("/api/setup", PostSetup)
			router.POST("/api/user/register", Register)
			path, body := "/api/setup", `{"username":"initial-root","password":"SetupPassword123!","confirmPassword":"SetupPassword123!"}`
			if flow == "default_key" {
				path, body = "/api/user/register", `{"username":"default-key-user","password":"SetupPassword123!"}`
			}
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			require.Equal(t, http.StatusOK, recorder.Code)
			var response struct {
				Success bool `json:"success"`
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
			require.True(t, response.Success)
			var user model.User
			require.NoError(t, db.First(&user).Error)
			assert.Zero(t, user.Quota, "new identities must not create a second legacy balance")
			assert.True(t, common.ValidatePasswordAndHash("SetupPassword123!", user.Password))
			assert.False(t, strings.Contains(recorder.Body.String(), user.Password))

			var entries []model.LedgerEntry
			require.NoError(t, db.Order("id").Find(&entries).Error)
			var tokens []model.Token
			require.NoError(t, db.Find(&tokens).Error)
			if flow == "setup" {
				assert.Equal(t, common.RoleRootUser, user.Role)
				assert.Empty(t, entries, "retiring an initializer does not authorize a root wallet grant")
				assert.Empty(t, tokens)
			} else {
				assert.Equal(t, common.RoleCommonUser, user.Role)
				require.Len(t, entries, 1)
				assert.Equal(t, money.Amount{Nanos: 1500000001, Currency: "USD"}, entries[0].Amount)
				require.Len(t, tokens, 1)
				token := tokens[0]
				for _, column := range []string{"model_limits_enabled", "group", "remain_quota", "used_quota", "unlimited_quota", "allow_user_quota_query"} {
					assert.False(t, db.Migrator().HasColumn("tokens", column), column)
				}
				assert.Equal(t, int64(-1), token.ExpiredTime)
				assert.False(t, strings.Contains(recorder.Body.String(), token.Key))
				credential, err := model.ValidateWalletToken(token.Key)
				require.NoError(t, err)
				assert.Equal(t, token.Id, credential.Id)
				assert.Equal(t, common.TokenStatusEnabled, credential.Status)
				for _, status := range []int{common.TokenStatusDisabled, common.TokenStatusExhausted, common.TokenStatusExpired} {
					require.NoError(t, db.Model(&model.Token{}).Where("id = ?", token.Id).Update("status", status).Error)
					_, err := model.ValidateWalletToken(token.Key)
					assert.ErrorIs(t, err, model.ErrTokenInvalid)
				}
				require.NoError(t, db.Model(&model.Token{}).Where("id = ?", token.Id).Updates(map[string]any{"status": common.TokenStatusEnabled, "expired_time": common.GetTimestamp() - 1}).Error)
				_, err = model.ValidateWalletToken(token.Key)
				assert.ErrorIs(t, err, model.ErrTokenInvalid)
				require.NoError(t, db.Model(&model.Token{}).Where("id = ?", token.Id).Updates(map[string]any{"status": common.TokenStatusEnabled, "expired_time": int64(-1)}).Error)
			}

			replay := httptest.NewRecorder()
			request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(replay, request)
			require.NoError(t, common.Unmarshal(replay.Body.Bytes(), &response))
			assert.False(t, response.Success)
			var userCount, tokenCount, entryCount int64
			require.NoError(t, db.Model(&model.User{}).Count(&userCount).Error)
			require.NoError(t, db.Model(&model.Token{}).Count(&tokenCount).Error)
			require.NoError(t, db.Model(&model.LedgerEntry{}).Count(&entryCount).Error)
			assert.Equal(t, int64(1), userCount)
			assert.Equal(t, int64(len(tokens)), tokenCount)
			assert.Equal(t, int64(len(entries)), entryCount)
		})
	}
}

func TestReferralMoneyHTTPContract(t *testing.T) {
	db := setupManageUserTestDB(t)
	require.NoError(t, model.MigrateGatewayBilling(db))
	require.NoError(t, db.AutoMigrate(&model.ReferralOpening{}))
	previous := *operation_setting.GetPaymentSetting()
	t.Cleanup(func() { *operation_setting.GetPaymentSetting() = previous })
	operation_setting.GetPaymentSetting().ComplianceConfirmed = true
	operation_setting.GetPaymentSetting().ComplianceTermsVersion = operation_setting.CurrentComplianceTermsVersion
	user := model.User{Username: "http-referral", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Quota: 777, UsedQuota: 137}
	require.NoError(t, db.Create(&user).Error)
	_, err := model.PostLedgerEntries(model.LedgerPosting{Account: model.LedgerAccount{Type: model.LedgerAccountUserReferral, OwnerId: user.Id, Currency: "USD"}, Type: model.LedgerEntryTopUp, Amount: money.Amount{Nanos: 9007199254740993, Currency: "USD"}})
	require.NoError(t, err)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("id", user.Id); c.Set("role", common.RoleRootUser) })
	router.GET("/api/user/referral-rewards", GetReferralRewards)
	router.POST("/api/user/aff_transfer", TransferAffQuota)
	router.GET("/api/user/self", GetSelf)
	router.GET("/api/user/", GetAllUsers)
	router.GET("/api/user/search", SearchUsers)
	router.GET("/api/user/:id", GetUser)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader(body)))
		return recorder
	}
	for _, path := range []string{"/api/user/referral-rewards", "/api/user/", "/api/user/search?keyword=http-referral", fmt.Sprintf("/api/user/%d", user.Id)} {
		recorder := request(http.MethodGet, path, "")
		var response struct {
			Success bool `json:"success"`
		}
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
		require.True(t, response.Success, path)
		assert.Contains(t, recorder.Body.String(), `"nanos":"9007199254740993"`)
		assert.NotContains(t, recorder.Body.String(), "aff_quota")
		assert.NotContains(t, recorder.Body.String(), "aff_history_quota")
		assert.NotContains(t, recorder.Body.String(), `"quota":`, path)
		assert.NotContains(t, recorder.Body.String(), `"used_quota":`, path)
	}
	self := request(http.MethodGet, "/api/user/self", "")
	var selfResult struct {
		Success bool `json:"success"`
	}
	require.NoError(t, common.Unmarshal(self.Body.Bytes(), &selfResult))
	require.True(t, selfResult.Success, "profile queries must work after the legacy reward columns are removed")
	assert.NotContains(t, self.Body.String(), `"quota":`)
	assert.NotContains(t, self.Body.String(), `"used_quota":`)
	for _, body := range []string{
		`{"quota":500000}`,
		`{"amount":{"nanos":1,"currency":"USD"},"transfer_id":"numeric"}`,
		`{"amount":{"nanos":"1","currency":"USD"}}`,
		`{"amount":{"nanos":"1","currency":"USD"},"transfer_id":"bad/id"}`,
		`{"amount":{"nanos":"1","currency":"CNY"},"transfer_id":"foreign"}`,
		`{"amount":{"nanos":"1","currency":"USD"},"transfer_id":"extra","quota":1}`,
		`{"amount":{"nanos":"1","currency":"USD"},"amount":{"nanos":"2","currency":"USD"},"transfer_id":"duplicate"}`,
	} {
		var result struct {
			Success bool `json:"success"`
		}
		recorder := request(http.MethodPost, "/api/user/aff_transfer", body)
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &result))
		assert.False(t, result.Success, body)
	}
	body := `{"amount":{"nanos":"9007199254740993","currency":"USD"},"transfer_id":"lost-response"}`
	operation_setting.GetPaymentSetting().ComplianceConfirmed = false
	blocked := request(http.MethodPost, "/api/user/aff_transfer", body)
	var result struct {
		Success bool `json:"success"`
		Data    struct {
			EntryID int64        `json:"entry_id"`
			Amount  money.Amount `json:"amount"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(blocked.Body.Bytes(), &result))
	assert.False(t, result.Success, "compliance is enforced by the server")
	operation_setting.GetPaymentSetting().ComplianceConfirmed = true
	for range 2 {
		recorder := request(http.MethodPost, "/api/user/aff_transfer", body)
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &result))
		require.True(t, result.Success)
		assert.Equal(t, money.Amount{Nanos: 9007199254740993, Currency: "USD"}, result.Data.Amount)
	}
	var entries int64
	require.NoError(t, db.Model(&model.LedgerEntry{}).Where("type = ?", model.LedgerEntryTransfer).Count(&entries).Error)
	assert.EqualValues(t, 2, entries, "HTTP replay must preserve the original debit and credit pair")
	persisted, err := model.GetUserById(user.Id, false)
	require.NoError(t, err)
	assert.Equal(t, 777, persisted.Quota)
	assert.Equal(t, 137, persisted.UsedQuota)
}

func setupManageUserTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	require.NoError(t, i18n.Init())
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousRedisEnabled, previousSecret := common.RedisEnabled, common.SessionSecret
	previousLogDatabaseType := common.LogDatabaseType()
	db := model.UseTestDatabases(t)
	logDB := model.LOG_DB
	if os.Getenv("TEST_MANAGE_USER_SEPARATE_LOG_DB") == "1" && !common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		logDB, _ = newAuditTestDatabase(t, "postgres")
	}
	model.DB, model.LOG_DB = db, logDB
	common.RedisEnabled = false
	common.SessionSecret = "manage-user-test-secret"

	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.RedisEnabled, common.SessionSecret = previousRedisEnabled, previousSecret
		common.SetLogDatabaseType(previousLogDatabaseType)
	})
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.UserSession{}, &model.CasbinRule{}, &model.AuthzRole{}, &model.AuthFlow{}, &model.TwoFA{}, &model.PasskeyCredential{}, &model.UserAccessToken{}))
	require.NoError(t, model.MigrateBalanceReminders(db))
	if !common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		require.NoError(t, logDB.AutoMigrate(&model.Log{}, &model.AuditLog{}))
	}
	return db
}

func performManageUserRequest(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	return performVerifiedManageUserRequest(t, body, service.AuthIdentity{UserID: 9999}, "")
}

// performVerifiedManageUserRequest drives ManageUser as the root operator from
// a dashboard login session, carrying the step-up proof status or role changes need.
func performVerifiedManageUserRequest(t *testing.T, body string, identity service.AuthIdentity, proof string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/user/manage", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	if proof != "" {
		c.Request.Header.Set("X-Security-Proof", proof)
	}
	c.Set("id", identity.UserID)
	c.Set("role", common.RoleRootUser)
	c.Set("username", "root-operator")
	c.Set("session_id", identity.SessionID)
	c.Set("auth_version", identity.UserAuthVersion)
	c.Set("session_version", identity.SessionVersion)
	c.Set(common.RequestIdKey, "quota-test-request")
	ManageUser(c)
	return recorder
}

// manageUserProof signs the root operator in and issues a password-method proof
// bound to one ManageUser operation.
func manageUserProof(t *testing.T, db *gorm.DB, operation service.VerificationOperation) (service.AuthIdentity, string) {
	t.Helper()
	operator := createQuotaTestOperator(t, db, common.RoleRootUser)
	require.NoError(t, model.PublishUserAuthCache(operator.Id))
	bundle, err := service.CreateLoginSession(operator.Id, "password", "127.0.0.1", "manage-user-test")
	require.NoError(t, err)
	identity, err := service.ParseAccessToken(bundle.AccessToken)
	require.NoError(t, err)
	binding, err := service.BindVerificationOperation(operation)
	require.NoError(t, err)
	proof, _, err := service.IssueSecurityProof(identity, service.VerificationMethodPassword, binding)
	require.NoError(t, err)
	return identity, proof
}

func TestManageUserDisableAdvancesAuthVersionOnceAndRevokesSession(t *testing.T) {
	db := setupManageUserTestDB(t)
	now := time.Now().Unix()
	user := model.User{
		Username: "managed-disable-user", Password: "password", Role: common.RoleCommonUser,
		Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1,
	}
	require.NoError(t, db.Create(&user).Error)
	require.NoError(t, db.Create(&model.UserSession{
		SID: "managed-disable-session", UserID: user.Id, Version: 1, UserAuthVersion: 1,
		Status: model.UserSessionStatusActive, RefreshHash: "refresh-hash", LoginMethod: "password",
		LastActiveAt: now, ExpiresAt: now + 3600,
	}).Error)

	identity, proof := manageUserProof(t, db, service.VerificationOperation{Scope: service.VerificationScopeAdminUserManage, Context: []byte(fmt.Sprintf(`{"user_id":%d,"action":"disable"}`, user.Id))})
	recorder := performVerifiedManageUserRequest(t, fmt.Sprintf(`{"id":%d,"action":"disable"}`, user.Id), identity, proof)
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), `"success":true`)

	var updated model.User
	require.NoError(t, db.First(&updated, user.Id).Error)
	assert.Equal(t, common.UserStatusDisabled, updated.Status)
	assert.EqualValues(t, 2, updated.AuthVersion)
	var session model.UserSession
	require.NoError(t, db.First(&session, "sid = ?", "managed-disable-session").Error)
	assert.Equal(t, model.UserSessionStatusRevoked, session.Status)
}

func TestManageUserDemoteAdvancesAuthVersionAndRevokesSessionsOnce(t *testing.T) {
	db := setupManageUserTestDB(t)
	previousMaster := common.IsMasterNode
	common.IsMasterNode = false
	t.Cleanup(func() { common.IsMasterNode = previousMaster })
	require.NoError(t, authz.Init(db))

	now := time.Now().Unix()
	user := model.User{
		Username: "managed-demote-user", Password: "password", Role: common.RoleAdminUser,
		Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1,
	}
	require.NoError(t, db.Create(&user).Error)
	for _, sid := range []string{"managed-demote-session-one", "managed-demote-session-two"} {
		require.NoError(t, db.Create(&model.UserSession{
			SID: sid, UserID: user.Id, Version: 1, UserAuthVersion: 1,
			Status: model.UserSessionStatusActive, RefreshHash: "refresh-" + sid, LoginMethod: "password",
			LastActiveAt: now, ExpiresAt: now + 3600,
		}).Error)
	}

	sessionUpdateCount := 0
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register("test:count_demote_session_updates", func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "user_sessions" {
			sessionUpdateCount++
		}
	}))

	identity, proof := manageUserProof(t, db, service.VerificationOperation{Scope: service.VerificationScopeAdminUserManage, Context: []byte(fmt.Sprintf(`{"user_id":%d,"action":"demote"}`, user.Id))})
	recorder := performVerifiedManageUserRequest(t, fmt.Sprintf(`{"id":%d,"action":"demote"}`, user.Id), identity, proof)
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), `"success":true`)

	var updated model.User
	require.NoError(t, db.First(&updated, user.Id).Error)
	assert.Equal(t, common.RoleCommonUser, updated.Role)
	assert.EqualValues(t, 2, updated.AuthVersion)
	var sessions []model.UserSession
	require.NoError(t, db.Where("user_id = ?", user.Id).Order("sid asc").Find(&sessions).Error)
	require.Len(t, sessions, 2)
	for _, session := range sessions {
		assert.Equal(t, model.UserSessionStatusRevoked, session.Status)
		assert.Equal(t, "admin_demote", session.RevokedReason)
	}
	assert.Equal(t, 1, sessionUpdateCount)
}

func TestManageUserDeleteReturnsImmediatelyAndUnknownActionFails(t *testing.T) {
	db := setupManageUserTestDB(t)
	deleted := model.User{
		Username: "managed-delete-user", Password: "password", Role: common.RoleCommonUser,
		Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: "delete-aff",
	}
	require.NoError(t, db.Create(&deleted).Error)

	identity, proof := manageUserProof(t, db, service.VerificationOperation{Scope: service.VerificationScopeAdminUserDelete, Context: []byte(fmt.Sprintf(`{"user_id":%d}`, deleted.Id))})
	recorder := performVerifiedManageUserRequest(t, fmt.Sprintf(`{"id":%d,"action":"delete"}`, deleted.Id), identity, proof)
	assert.Contains(t, recorder.Body.String(), `"success":true`)
	var deletedCount int64
	require.NoError(t, db.Unscoped().Model(&model.User{}).Where("id = ? AND deleted_at IS NOT NULL", deleted.Id).Count(&deletedCount).Error)
	assert.EqualValues(t, 1, deletedCount)

	unchanged := model.User{
		Username: "managed-unknown-user", Password: "password", Role: common.RoleCommonUser,
		Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: "unknown-aff",
	}
	require.NoError(t, db.Create(&unchanged).Error)
	recorder = performManageUserRequest(t, fmt.Sprintf(`{"id":%d,"action":"unknown"}`, unchanged.Id))
	assert.Contains(t, recorder.Body.String(), `"success":false`)
	require.NoError(t, db.First(&unchanged, unchanged.Id).Error)
	assert.EqualValues(t, 1, unchanged.AuthVersion)
	assert.Equal(t, common.UserStatusEnabled, unchanged.Status)
}

func createQuotaTestOperator(t *testing.T, db *gorm.DB, role int) model.User {
	t.Helper()
	if role == 0 {
		role = common.RoleRootUser
	}
	operator := model.User{Id: 9999, Username: "root-operator", Password: "root-operator-hash", Role: role, Status: common.UserStatusEnabled, AuthVersion: 1, AffCode: "root-operator-aff"}
	require.NoError(t, db.Create(&operator).Error)
	return operator
}

func setupManagedWalletTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := setupManageUserTestDB(t)
	require.NoError(t, model.MigrateGatewayBilling(db))
	previousMaster := common.IsMasterNode
	common.IsMasterNode = false
	t.Cleanup(func() { common.IsMasterNode = previousMaster })
	require.NoError(t, authz.Init(db))
	return db
}

func managedWalletTestRouter(t *testing.T, operator model.User) (*gin.Engine, string) {
	t.Helper()
	_, token := createAccessTokenTestSession(t, operator.Id, common.GetUUID())
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(middleware.RequestId(), middleware.AdminAPISecurity(), middleware.AccessTokenAudit())
	middleware.DeclareAccessTokenPermissionRoute(http.MethodGet, "/api/admin/v1/user-wallets/:user_id", authz.LedgerRead)
	middleware.DeclareAccessTokenPermissionRoute(http.MethodPost, "/api/admin/v1/user-wallets/:user_id/adjustments", authz.LedgerWrite)
	router.GET("/api/admin/v1/user-wallets/:user_id", middleware.RequirePermission(authz.LedgerRead), GetUserWallet)
	router.POST("/api/admin/v1/user-wallets/:user_id/adjustments", middleware.RequirePermission(authz.LedgerWrite), CreateUserWalletAdjustment)
	return router, token
}

func performManagedWalletRequest(router http.Handler, token, method string, userID int, body string) *httptest.ResponseRecorder {
	path := fmt.Sprintf("/api/admin/v1/user-wallets/%d", userID)
	if method == http.MethodPost {
		path += "/adjustments"
	}
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestManageUserRejectsRetiredQuotaMutations(t *testing.T) {
	db := setupManageUserTestDB(t)
	require.NoError(t, model.MigrateGatewayBilling(db))
	createQuotaTestOperator(t, db, common.RoleRootUser)
	user := model.User{Username: "retired-quota-owner", Role: common.RoleCommonUser, Quota: 1000, AuthVersion: 1}
	require.NoError(t, db.Create(&user).Error)
	_, err := model.PostLedgerEntries(model.LedgerPosting{Account: model.UserWalletAccount(user.Id), Type: model.LedgerEntryOpeningBalance, Amount: money.Amount{Nanos: 123, Currency: "USD"}})
	require.NoError(t, err)
	server := miniredis.RunT(t)
	previousRDB := common.RDB
	common.RDB = redis.NewClient(&redis.Options{Addr: server.Addr(), MaxRetries: -1})
	common.RedisEnabled = true
	t.Cleanup(func() { _ = common.RDB.Close(); common.RDB = previousRDB })
	_, err = model.GetUserCache(user.Id)
	require.NoError(t, err)
	var quotaKey string
	for _, key := range server.Keys() {
		if server.HGet(key, "Id") == strconv.Itoa(user.Id) && server.HGet(key, "CacheMAC") != "" {
			quotaKey = key
			break
		}
	}
	require.NotEmpty(t, quotaKey)
	server.HSet(quotaKey, "Quota", "900")
	for _, payload := range []string{
		`{"action":"add_quota","mode":"add","value":500}`,
		`{"action":"add_quota","mode":"subtract","value":500}`,
		`{"action":"add_quota","mode":"override","value":2000}`,
		`{"action":"add_quota","mode":"override","value":0}`,
		`{"action":"add_quota","mode":"override","value":-1}`,
		`{"action":"add_quota"}`,
		`{"action":"add_quota","mode":null,"value":null}`,
		`{"action":"add_quota","mode":{},"value":"500"}`,
		`{"action":"add_quota","mode":"add","value":9223372036854775808}`,
		`{"action":"adjust_wallet","amount":"1","remark":"retired-action"}`,
		`{"action":"disable","mode":"override","value":2000}`,
		`{"action":"disable"} {"action":"add_quota"}`,
	} {
		t.Run(payload, func(t *testing.T) {
			body := fmt.Sprintf(`{"id":%d,`, user.Id) + payload[1:]
			response := performManageUserRequest(t, body)
			assert.Contains(t, response.Body.String(), `"success":false`)
			var stored model.User
			require.NoError(t, db.First(&stored, user.Id).Error)
			assert.Equal(t, 1000, stored.Quota)
			assert.Equal(t, user.Status, stored.Status)
			assert.Equal(t, "900", server.HGet(quotaKey, "Quota"))
			wallet, err := model.GetUserWallet(user.Id)
			require.NoError(t, err)
			require.NotNil(t, wallet)
			assert.Equal(t, int64(123), wallet.Balance.Nanos)
			var entries int64
			require.NoError(t, db.Model(&model.LedgerEntry{}).Count(&entries).Error)
			assert.EqualValues(t, 1, entries)
		})
	}
}

func TestManageUserQuotaRecordsTopupAndAudit(t *testing.T) {
	// Keep the historical test name while moving new balance changes to the ledger.
	for _, tc := range []struct {
		name, amount         string
		before, delta, after int64
	}{
		{"add", "0.0000005", 1000, 500, 1500},
		{"subtract", "-0.0000005", 1000, -500, 500},
		{"subtract_to_zero", "-0.000001", 1000, -1000, 0},
		{"above_javascript_integer_precision", "0.000000001", 9007199254740992, 1, 9007199254740993},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupManagedWalletTestDB(t)
			operator := createQuotaTestOperator(t, db, common.RoleRootUser)
			user := model.User{Username: "wallet-adjustment-owner", Role: common.RoleCommonUser, Quota: 1000, AuthVersion: 1}
			require.NoError(t, db.Create(&user).Error)
			_, err := model.PostLedgerEntries(model.LedgerPosting{Account: model.UserWalletAccount(user.Id), Type: model.LedgerEntryOpeningBalance, Amount: money.Amount{Nanos: tc.before, Currency: "USD"}})
			require.NoError(t, err)
			router, token := managedWalletTestRouter(t, operator)
			response := performManagedWalletRequest(router, token, http.MethodPost, user.Id, fmt.Sprintf(`{"amount":%q,"remark":"  manual correction  "}`, tc.amount))
			require.Equal(t, http.StatusOK, response.Code)
			var result struct {
				Success bool
				Data    ledgerEntryView
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
			require.True(t, result.Success, response.Body.String())
			assert.Equal(t, amountString(money.Amount{Nanos: tc.delta, Currency: "USD"}), result.Data.Amount)
			assert.Equal(t, amountString(money.Amount{Nanos: tc.after, Currency: "USD"}), result.Data.BalanceAfter)
			var entry model.LedgerEntry
			require.NoError(t, db.First(&entry, result.Data.Id).Error)
			assert.Equal(t, model.LedgerEntryAdjustment, entry.Type)
			assert.Equal(t, money.Amount{Nanos: tc.delta, Currency: "USD"}, entry.Amount)
			assert.Equal(t, money.Amount{Nanos: tc.after, Currency: "USD"}, entry.BalanceAfter)
			assert.Equal(t, operator.Id, entry.OperatorId)
			assert.Equal(t, "manual correction", entry.Remark)
			assert.Equal(t, response.Header().Get(common.RequestIdKey), entry.RequestId)
			wallet, err := model.GetUserWallet(user.Id)
			require.NoError(t, err)
			require.NotNil(t, wallet)
			assert.Equal(t, wallet.Id, entry.WalletId)
			assert.Equal(t, entry.BalanceAfter, wallet.Balance)
			require.NoError(t, db.First(&user, user.Id).Error)
			assert.Equal(t, 1000, user.Quota)
			var audits []model.AuditLog
			require.NoError(t, model.AuditLogQuery().Where("request_id = ?", entry.RequestId).Find(&audits).Error)
			require.Len(t, audits, 1)
			assert.Equal(t, operator.Id, audits[0].UserId)
			assert.Equal(t, operator.Username, audits[0].Username)
			assert.Equal(t, "wallet.adjust", audits[0].Action)
			assert.True(t, audits[0].Success)
			require.NotNil(t, audits[0].Other.AdminInfo)
			assert.Equal(t, &model.AuditAdminInfo{AdminID: operator.Id, AdminUsername: operator.Username, AdminRole: common.RoleRootUser, AuthMethod: "session"}, audits[0].Other.AdminInfo)
			require.NotNil(t, audits[0].Other.AuditInfo)
			assert.Equal(t, strconv.Itoa(user.Id), audits[0].Other.AuditInfo.Params["user_id"])
			encoded, err := common.Marshal(audits[0])
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), token)
			assert.NotContains(t, string(encoded), "manual correction", "route audit must not copy request text")
		})
	}

	for _, tc := range []struct {
		mode, action, content string
		value, after          int
	}{
		{"add", "user.quota_add", "Increased user quota by 500", 500, 1500},
		{"subtract", "user.quota_subtract", "Decreased user quota by 500", 500, 500},
		{"override", "user.quota_override", "Overrode user quota from 1000 to 1500", 1500, 1500},
		{"override", "user.quota_override", "Overrode user quota from 1000 to 500", 500, 500},
		{"override", "user.quota_override", "Overrode user quota from 1000 to 1000", 1000, 1000},
		{"override", "user.quota_override", "Overrode user quota from 1000 to 0", 0, 0},
		{"override", "user.quota_override", "Overrode user quota from 1000 to -1", -1, -1},
	} {
		t.Run("historical_"+tc.content, func(t *testing.T) {
			db := setupManageUserTestDB(t)
			operator := createQuotaTestOperator(t, db, common.RoleRootUser)
			user := model.User{Username: "historical-quota-owner", Role: common.RoleCommonUser, Quota: 1000}
			require.NoError(t, db.Create(&user).Error)
			params := model.AuditFields{"target_user_id": user.Id, "target_username": user.Username, "mode": tc.mode, "requested_quota": tc.value, "from": 1000, "to": tc.after}
			if tc.mode != "override" {
				params["quota"] = tc.value
			}
			admin := &model.AuditAdminInfo{AdminID: operator.Id, AdminUsername: operator.Username, AdminRole: operator.Role, AuthMethod: "session"}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/user/manage", nil)
			c.Set(common.RequestIdKey, "historical-quota-request")
			content := auditContentEN(tc.action, params)
			assert.Equal(t, tc.content, content)
			model.RecordLogWithAdminInfo(user.Id, model.LogTypeTopup, content, admin, &model.AuditOperation{Action: tc.action, Params: params}, c)
			model.RecordOperationAuditLog(operator.Id, operator.Role, content, "127.0.0.1", tc.action, params, admin, nil, c)
			logs, total, err := model.GetAllLogs(model.LogFilter{Type: model.LogTypeTopup}, 0, 20)
			require.NoError(t, err)
			assert.EqualValues(t, 1, total)
			require.Len(t, logs, 1)
			assert.Equal(t, user.Id, logs[0].UserId)
			assert.Equal(t, user.Username, logs[0].Username)
			assert.Equal(t, tc.content, logs[0].Content)
			model.FormatAdminLogs(logs)
			var other model.AuditOther
			require.NoError(t, common.UnmarshalJsonStr(logs[0].Other, &other))
			assert.Equal(t, admin, other.AdminInfo)
			logs, total, err = model.GetUserLogs(user.Id, model.LogFilter{Type: model.LogTypeTopup}, 0, 20)
			require.NoError(t, err)
			assert.EqualValues(t, 1, total)
			require.Len(t, logs, 1)
			other = model.AuditOther{}
			require.NoError(t, common.UnmarshalJsonStr(logs[0].Other, &other))
			assert.Nil(t, other.AdminInfo)
			require.NotNil(t, other.Op)
			assert.Equal(t, tc.action, other.Op.Action)
			encoded, err := common.Marshal(other.Op.Params)
			require.NoError(t, err)
			expected, err := common.Marshal(params)
			require.NoError(t, err)
			assert.JSONEq(t, string(expected), string(encoded))
			assert.Equal(t, "historical-quota-request", logs[0].RequestId)
			assert.Empty(t, logs[0].Ip)
			logs, total, err = model.GetUserLogs(operator.Id, model.LogFilter{Type: model.LogTypeTopup}, 0, 20)
			require.NoError(t, err)
			assert.Zero(t, total)
			assert.Empty(t, logs)
			var audits []model.AuditLog
			require.NoError(t, model.AuditLogQuery().Find(&audits).Error)
			require.Len(t, audits, 1)
			assert.Equal(t, operator.Id, audits[0].UserId)
			assert.Equal(t, operator.Username, audits[0].Username)
			assert.Equal(t, tc.action, audits[0].Action)
			assert.True(t, audits[0].Success)
			assert.Equal(t, "historical-quota-request", audits[0].RequestId)
			encoded, err = common.Marshal(audits[0].Other.Op.Params)
			require.NoError(t, err)
			assert.JSONEq(t, string(expected), string(encoded))
		})
	}
}

func TestManageUserQuotaFailuresDoNotRecordTopup(t *testing.T) {
	for _, tc := range []struct {
		name, body, failedTable string
	}{
		{"zero_add", `{"amount":"0","remark":"correction"}`, ""},
		{"negative_subtract", `{"amount":"-0.000002","remark":"correction"}`, ""},
		{"invalid_mode", `{"amount":"0.0000005","remark":"correction","mode":"invalid"}`, ""},
		{"retired_override", `{"amount":"0.0000005","remark":"correction","mode":"override"}`, ""},
		{"retired_value", `{"amount":"0.0000005","remark":"correction","value":500}`, ""},
		{"retired_quota", `{"amount":"0.0000005","remark":"correction","quota":500}`, ""},
		{"trailing_json", `{"amount":"0.0000005","remark":"correction"} {}`, ""},
		{"numeric_amount", `{"amount":0.5,"remark":"correction"}`, ""},
		{"missing_remark", `{"amount":"0.0000005","remark":"  "}`, ""},
		{"add_update_error", `{"amount":"0.0000005","remark":"correction"}`, "wallets"},
		{"subtract_update_error", `{"amount":"-0.0000005","remark":"correction"}`, "wallets"},
		{"ledger_insert_error", `{"amount":"0.0000005","remark":"correction"}`, "ledger_entries"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupManagedWalletTestDB(t)
			operator := createQuotaTestOperator(t, db, common.RoleRootUser)
			user := model.User{Username: "wallet-failure-owner", Role: common.RoleCommonUser, Quota: 1000}
			require.NoError(t, db.Create(&user).Error)
			_, err := model.PostLedgerEntries(model.LedgerPosting{Account: model.UserWalletAccount(user.Id), Type: model.LedgerEntryOpeningBalance, Amount: money.Amount{Nanos: 1000, Currency: "USD"}})
			require.NoError(t, err)
			router, token := managedWalletTestRouter(t, operator)
			if tc.failedTable == "wallets" {
				require.NoError(t, db.Callback().Update().After("gorm:update").Register("test:fail_wallet_update", func(tx *gorm.DB) {
					if tx.Statement.Table == tc.failedTable {
						tx.AddError(errors.New("private wallet update unavailable"))
					}
				}))
			} else if tc.failedTable == "ledger_entries" {
				require.NoError(t, db.Callback().Create().After("gorm:create").Register("test:fail_wallet_entry", func(tx *gorm.DB) {
					if tx.Statement.Table == tc.failedTable {
						tx.AddError(errors.New("private ledger insert unavailable"))
					}
				}))
			}
			response := performManagedWalletRequest(router, token, http.MethodPost, user.Id, tc.body)
			assert.Contains(t, response.Body.String(), `"success":false`)
			assert.NotContains(t, response.Body.String(), "private")
			wallet, err := model.GetUserWallet(user.Id)
			require.NoError(t, err)
			require.NotNil(t, wallet)
			assert.Equal(t, int64(1000), wallet.Balance.Nanos)
			require.NoError(t, db.First(&user, user.Id).Error)
			assert.Equal(t, 1000, user.Quota)
			var entries, logs int64
			require.NoError(t, db.Model(&model.LedgerEntry{}).Count(&entries).Error)
			assert.EqualValues(t, 1, entries)
			require.NoError(t, model.LOG_DB.Model(&model.Log{}).Count(&logs).Error)
			assert.Zero(t, logs)
			var audits []model.AuditLog
			require.NoError(t, model.AuditLogQuery().Where("request_id = ?", response.Header().Get(common.RequestIdKey)).Find(&audits).Error)
			require.Len(t, audits, 1)
			assert.False(t, audits[0].Success)
			assert.Equal(t, "wallet.adjust", audits[0].Action)
			assert.Equal(t, operator.Id, audits[0].UserId)
			require.NotNil(t, audits[0].Other.AuditInfo)
			assert.Equal(t, strconv.Itoa(user.Id), audits[0].Other.AuditInfo.Params["user_id"])
			encoded, err := common.Marshal(audits[0])
			require.NoError(t, err)
			for _, forbidden := range []string{token, "private wallet", "private ledger", "wallet-failure-owner", `"from"`, `"to"`, `"requested_quota"`} {
				assert.NotContains(t, string(encoded), forbidden)
			}
		})
	}
}

func TestManageUserQuotaLogFailureKeepsSuccessfulAdjustment(t *testing.T) {
	for _, failedTable := range []string{"logs", "audit_logs"} {
		t.Run(failedTable, func(t *testing.T) {
			db := setupManagedWalletTestDB(t)
			operator := createQuotaTestOperator(t, db, common.RoleRootUser)
			user := model.User{Username: "wallet-log-failure-owner", Role: common.RoleCommonUser, Quota: 1000}
			require.NoError(t, db.Create(&user).Error)
			router, token := managedWalletTestRouter(t, operator)
			require.NoError(t, model.LOG_DB.Callback().Create().Before("gorm:create").Register("test:fail_wallet_log", func(tx *gorm.DB) {
				if tx.Statement.Table == failedTable {
					tx.AddError(errors.New("private audit unavailable"))
				}
			}))
			response := performManagedWalletRequest(router, token, http.MethodPost, user.Id, `{"amount":"0.0000005","remark":"correction"}`)
			require.Contains(t, response.Body.String(), `"success":true`)
			require.NoError(t, db.First(&user, user.Id).Error)
			assert.Equal(t, 1000, user.Quota)
			wallet, err := model.GetUserWallet(user.Id)
			require.NoError(t, err)
			require.NotNil(t, wallet)
			assert.Equal(t, money.Amount{Nanos: 500, Currency: "USD"}, wallet.Balance)
			var entries []model.LedgerEntry
			require.NoError(t, db.Find(&entries).Error)
			require.Len(t, entries, 1)
			assert.Equal(t, model.LedgerEntryAdjustment, entries[0].Type)
			assert.Equal(t, wallet.Balance, entries[0].Amount)
			assert.Equal(t, operator.Id, entries[0].OperatorId)
			assert.Equal(t, response.Header().Get(common.RequestIdKey), entries[0].RequestId)
			var logs, audits int64
			require.NoError(t, model.LOG_DB.Model(&model.Log{}).Count(&logs).Error)
			require.NoError(t, model.AuditLogQuery().Count(&audits).Error)
			assert.Zero(t, logs, "wallet changes are recorded in the ledger")
			if failedTable == "logs" {
				assert.EqualValues(t, 1, audits)
			} else {
				assert.Zero(t, audits)
			}
		})
	}
}

func TestManageUserQuotaTargetsAndWalletBounds(t *testing.T) {
	for _, tc := range []struct {
		name, body, message                string
		before                             int64
		targetID, targetRole, operatorRole int
		deleted, failRead                  bool
	}{
		{name: "zero_id", targetID: 0, message: "Invalid user ID"},
		{name: "negative_id", targetID: -1, message: "Invalid user ID"},
		{name: "missing", targetID: 12345, message: "User does not exist"},
		{name: "deleted", targetID: 1, deleted: true, message: "User does not exist"},
		{name: "peer_admin", targetID: 1, targetRole: common.RoleAdminUser, operatorRole: common.RoleAdminUser, message: "No permission to update users of same or higher permission level"},
		{name: "higher_role", targetID: 1, targetRole: common.RoleRootUser, operatorRole: common.RoleAdminUser, message: "No permission to update users of same or higher permission level"},
		{name: "read_error", targetID: 1, failRead: true},
		{name: "add_overflow", targetID: 1, before: 9223372036854775807, message: "The amount is outside the supported range"},
		{name: "subtract_underflow", targetID: 1, body: `{"amount":"-0.000000001","remark":"correction"}`, message: "Insufficient balance"},
		{name: "oversized_add", targetID: 1, body: `{"amount":"9223372036.854775808","remark":"correction"}`, message: "Invalid amount"},
		{name: "oversized_subtract", targetID: 1, body: `{"amount":"-9223372036.854775809","remark":"correction"}`, message: "Invalid amount"},
		{name: "oversized_override", targetID: 1, body: `{"amount":"0.000000001","remark":"correction","mode":"override","value":-9223372036854775808}`, message: "Invalid parameters"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupManagedWalletTestDB(t)
			operator := createQuotaTestOperator(t, db, tc.operatorRole)
			role := tc.targetRole
			if role == 0 {
				role = common.RoleCommonUser
			}
			user := model.User{Id: 1, Username: "private-target-name", Role: role, Quota: 731, AuthVersion: 1}
			require.NoError(t, db.Create(&user).Error)
			var originalEntries int64
			if tc.before > 0 {
				_, err := model.PostLedgerEntries(model.LedgerPosting{Account: model.UserWalletAccount(user.Id), Type: model.LedgerEntryOpeningBalance, Amount: money.Amount{Nanos: tc.before, Currency: "USD"}})
				require.NoError(t, err)
				originalEntries = 1
			}
			if tc.deleted {
				require.NoError(t, db.Delete(&user).Error)
			}
			if operator.Role == common.RoleAdminUser {
				require.NoError(t, authz.SetUserPermissions(operator.Id, authz.PermissionsMap{authz.ResourceLedger: {authz.ActionRead: true, authz.ActionWrite: true}}))
			}
			router, token := managedWalletTestRouter(t, operator)
			if tc.failRead {
				require.NoError(t, db.Callback().Query().Before("gorm:query").Register("test:wallet_target_read_error", func(tx *gorm.DB) {
					if target, ok := tx.Statement.Dest.(*model.User); ok && target.Id == user.Id {
						tx.AddError(errors.New("private database error"))
					}
				}))
			}
			body := tc.body
			if body == "" {
				body = `{"amount":"0.000000001","remark":"correction"}`
			}
			response := performManagedWalletRequest(router, token, http.MethodPost, tc.targetID, body)
			require.Contains(t, response.Body.String(), `"success":false`)
			if tc.message != "" {
				assert.Contains(t, response.Body.String(), tc.message)
			}
			if tc.failRead {
				require.NoError(t, db.Callback().Query().Remove("test:wallet_target_read_error"))
			}
			require.NoError(t, db.Unscoped().First(&user, user.Id).Error)
			assert.Equal(t, 731, user.Quota)
			wallet, err := model.GetUserWallet(user.Id)
			require.NoError(t, err)
			if tc.before == 0 {
				assert.Nil(t, wallet)
			} else {
				require.NotNil(t, wallet)
				assert.Equal(t, tc.before, wallet.Balance.Nanos)
			}
			var entries, logs int64
			require.NoError(t, db.Model(&model.LedgerEntry{}).Count(&entries).Error)
			assert.Equal(t, originalEntries, entries)
			require.NoError(t, model.LOG_DB.Model(&model.Log{}).Count(&logs).Error)
			assert.Zero(t, logs)
			var audits []model.AuditLog
			require.NoError(t, model.AuditLogQuery().Where("request_id = ?", response.Header().Get(common.RequestIdKey)).Find(&audits).Error)
			require.Len(t, audits, 1)
			assert.False(t, audits[0].Success)
			assert.Equal(t, operator.Id, audits[0].UserId)
			assert.Equal(t, "wallet.adjust", audits[0].Action)
			encoded, err := common.Marshal(audits[0])
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), user.Username)
			assert.NotContains(t, string(encoded), "private database error")
			assert.NotContains(t, string(encoded), token)
		})
	}
}

func TestManageUserQuotaMiddlewareKeepsOneOperationPerRequest(t *testing.T) {
	db := setupManagedWalletTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Option{}))
	require.NoError(t, model.EnsureLegacyAccessTokenRetireAt(time.Now().Unix()))
	operator := createQuotaTestOperator(t, db, common.RoleRootUser)
	user := model.User{Username: "audited-wallet-owner", Role: common.RoleCommonUser, Quota: 1000}
	require.NoError(t, db.Create(&user).Error)
	router, session := managedWalletTestRouter(t, operator)
	pat, _ := createScopedAccessToken(t, operator.Id, 0, "ledger:read", "ledger:write")
	readOnly, _ := createScopedAccessToken(t, operator.Id, 0, "ledger:read")
	for _, tc := range []struct {
		body, token string
		success     bool
		status      int
	}{
		{`{"amount":"0.0000001","remark":"audited correction"}`, pat, true, http.StatusOK},
		{`{"amount":"0","remark":"zero"}`, pat, false, http.StatusOK},
		{`{"amount":"0.0000001","remark":"retired","mode":"override"}`, pat, false, http.StatusOK},
		{`{"amount":`, pat, false, http.StatusOK},
		{`{"amount":"0.0000001","remark":"read only"}`, readOnly, false, http.StatusForbidden},
	} {
		response := performManagedWalletRequest(router, tc.token, http.MethodPost, user.Id, tc.body)
		assert.Equal(t, tc.status, response.Code)
		if tc.status == http.StatusOK {
			assert.Contains(t, response.Body.String(), fmt.Sprintf(`"success":%t`, tc.success))
		}
		requestID := response.Header().Get(common.RequestIdKey)
		require.NotEmpty(t, requestID)
		var audits []model.AuditLog
		require.NoError(t, model.AuditLogQuery().Where("request_id = ?", requestID).Find(&audits).Error)
		require.Len(t, audits, 2, "one operation audit and one PAT request audit")
		operationCount, tokenCount := 0, 0
		for _, audit := range audits {
			assert.Equal(t, tc.success, audit.Success)
			assert.Equal(t, tc.status, audit.Status)
			assert.Equal(t, operator.Id, audit.UserId)
			assert.Equal(t, "access_token", audit.AuthMethod)
			assert.Equal(t, "/api/admin/v1/user-wallets/:user_id/adjustments", audit.Route)
			if audit.Category == model.AuditCategoryOperation {
				operationCount++
				assert.Equal(t, "wallet.adjust", audit.Action)
				require.NotNil(t, audit.Other.AuditInfo)
				assert.Equal(t, strconv.Itoa(user.Id), audit.Other.AuditInfo.Params["user_id"])
			} else {
				tokenCount++
				assert.Equal(t, model.AuditCategoryAccessToken, audit.Category)
				assert.Equal(t, model.AccessTokenFingerprint(tc.token), audit.TokenRef)
			}
			encoded, err := common.Marshal(audit)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), tc.token)
			assert.NotContains(t, string(encoded), "audited correction")
		}
		assert.Equal(t, 1, operationCount)
		assert.Equal(t, 1, tokenCount)
		var count int64
		require.NoError(t, db.Model(&model.LedgerEntry{}).Where("request_id = ?", requestID).Count(&count).Error)
		if tc.success {
			assert.EqualValues(t, 1, count)
		} else {
			assert.Zero(t, count)
		}
	}
	legacy := gin.New()
	legacy.Use(middleware.RequestId())
	legacy.POST("/api/user/manage", middleware.AdminAuth(), ManageUser)
	for _, body := range []string{
		fmt.Sprintf(`{"id":%d,"action":"add_quota","mode":"add","value":100}`, user.Id),
		fmt.Sprintf(`{"id":%d,"action":"add_quota","mode":"override","value":0}`, user.Id),
		`{"id":`,
	} {
		response := accessTokenRequest(legacy, http.MethodPost, "/api/user/manage", session, "", body)
		assert.Contains(t, response.Body.String(), `"success":false`)
		var audits []model.AuditLog
		require.NoError(t, model.AuditLogQuery().Where("request_id = ?", response.Header().Get(common.RequestIdKey)).Find(&audits).Error)
		require.Len(t, audits, 1)
		assert.False(t, audits[0].Success)
		assert.Equal(t, "generic", audits[0].Action)
	}
	require.NoError(t, db.First(&user, user.Id).Error)
	assert.Equal(t, 1000, user.Quota)
	wallet, err := model.GetUserWallet(user.Id)
	require.NoError(t, err)
	require.NotNil(t, wallet)
	assert.Equal(t, int64(100), wallet.Balance.Nanos)
}

func TestManageUserQuotaConcurrentSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		before, first, second, after int64
		wantSuccess                  int
	}{
		{"serialized_exact_changes", 1000, 10, 20, 1030, 2},
		{"one_overflow_rolls_back", 9223372036854775806, 1, 1, 9223372036854775807, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupManagedWalletTestDB(t)
			operator := createQuotaTestOperator(t, db, common.RoleRootUser)
			user := model.User{Username: "concurrent-wallet-owner", Role: common.RoleCommonUser, Quota: 1000}
			require.NoError(t, db.Create(&user).Error)
			_, err := model.PostLedgerEntries(model.LedgerPosting{Account: model.UserWalletAccount(user.Id), Type: model.LedgerEntryOpeningBalance, Amount: money.Amount{Nanos: tc.before, Currency: "USD"}})
			require.NoError(t, err)
			router, token := managedWalletTestRouter(t, operator)
			var ready sync.WaitGroup
			ready.Add(2)
			release := make(chan struct{})
			results := make(chan *httptest.ResponseRecorder, 2)
			for _, delta := range []int64{tc.first, tc.second} {
				go func() {
					ready.Done()
					<-release
					body := fmt.Sprintf(`{"amount":%q,"remark":"concurrent correction"}`, amountString(money.Amount{Nanos: delta, Currency: "USD"}))
					results <- performManagedWalletRequest(router, token, http.MethodPost, user.Id, body)
				}()
			}
			ready.Wait()
			close(release)
			var committed []model.LedgerEntry
			for range 2 {
				response := <-results
				var result struct {
					Success bool
					Data    ledgerEntryView
				}
				require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
				var audits []model.AuditLog
				require.NoError(t, model.AuditLogQuery().Where("request_id = ?", response.Header().Get(common.RequestIdKey)).Find(&audits).Error)
				require.Len(t, audits, 1)
				assert.Equal(t, result.Success, audits[0].Success)
				if !result.Success {
					assert.Contains(t, response.Body.String(), "The amount is outside the supported range")
					continue
				}
				var entry model.LedgerEntry
				require.NoError(t, db.First(&entry, result.Data.Id).Error)
				committed = append(committed, entry)
			}
			require.Len(t, committed, tc.wantSuccess)
			sort.Slice(committed, func(i, j int) bool { return committed[i].BalanceAfter.Nanos < committed[j].BalanceAfter.Nanos })
			balance := tc.before
			for _, entry := range committed {
				assert.Equal(t, balance, entry.BalanceAfter.Nanos-entry.Amount.Nanos)
				balance = entry.BalanceAfter.Nanos
			}
			assert.Equal(t, tc.after, balance)
			wallet, err := model.GetUserWallet(user.Id)
			require.NoError(t, err)
			require.NotNil(t, wallet)
			assert.Equal(t, tc.after, wallet.Balance.Nanos)
			var count int64
			require.NoError(t, db.Model(&model.LedgerEntry{}).Count(&count).Error)
			assert.EqualValues(t, tc.wantSuccess+1, count)
			require.NoError(t, db.First(&user, user.Id).Error)
			assert.Equal(t, 1000, user.Quota)
		})
	}
}

func TestManageUserQuotaCacheUsesCommittedIntegerDifference(t *testing.T) {
	for _, tc := range []struct {
		name, body                                    string
		legacy, cached                                int
		before, after                                 int64
		failUpdate, failCache, missingCache, rejected bool
	}{
		{name: "add_preserves_reservations", legacy: 1000, cached: 900, before: 1000, after: 1500, body: `{"amount":"0.0000005","remark":"correction"}`},
		{name: "subtract_preserves_reservations", legacy: 1000, cached: 900, before: 1000, after: 500, body: `{"amount":"-0.0000005","remark":"correction"}`},
		{name: "override_preserves_reservations", legacy: 1000, cached: 900, before: 1000, after: 1000, body: `{"amount":"0.000002","remark":"correction","mode":"override"}`, rejected: true},
		{name: "large_odd_difference", legacy: common.MaxWalletQuota - 1, cached: common.MaxWalletQuota - 1, before: 9007199254740992, after: 9007199254740993, body: `{"amount":"0.000000001","remark":"precise correction"}`},
		{name: "rollback_does_not_change_cache", legacy: 1000, cached: 900, before: 1000, after: 1000, body: `{"amount":"-0.0000005","remark":"correction"}`, failUpdate: true},
		{name: "cache_error_keeps_committed_change", legacy: 1000, cached: 900, before: 1000, after: 1500, body: `{"amount":"0.0000005","remark":"correction"}`, failCache: true},
		{name: "missing_cache_is_not_partially_created", legacy: 1000, cached: 900, before: 1000, after: 1500, body: `{"amount":"0.0000005","remark":"correction"}`, missingCache: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupManagedWalletTestDB(t)
			operator := createQuotaTestOperator(t, db, common.RoleRootUser)
			user := model.User{Username: "cached-wallet-owner", Role: common.RoleCommonUser, Quota: tc.legacy, AuthVersion: 1}
			require.NoError(t, db.Create(&user).Error)
			_, err := model.PostLedgerEntries(model.LedgerPosting{Account: model.UserWalletAccount(user.Id), Type: model.LedgerEntryOpeningBalance, Amount: money.Amount{Nanos: tc.before, Currency: "USD"}})
			require.NoError(t, err)
			router, token := managedWalletTestRouter(t, operator)
			server := miniredis.RunT(t)
			previousRDB := common.RDB
			common.RDB = redis.NewClient(&redis.Options{Addr: server.Addr(), MaxRetries: -1})
			common.RedisEnabled = true
			t.Cleanup(func() { _ = common.RDB.Close(); common.RDB = previousRDB })
			cache, err := model.GetUserCache(user.Id)
			require.NoError(t, err)
			assert.Equal(t, user.Id, cache.Id)
			_, err = model.GetUserCache(operator.Id)
			require.NoError(t, err)
			var quotaKey string
			for _, key := range server.Keys() {
				if server.HGet(key, "Id") == strconv.Itoa(user.Id) && server.HGet(key, "CacheMAC") != "" {
					quotaKey = key
					break
				}
			}
			require.NotEmpty(t, quotaKey)
			assert.Empty(t, server.HGet(quotaKey, "Quota"))
			server.HSet(quotaKey, "Quota", strconv.Itoa(tc.cached))
			if tc.missingCache {
				server.Del(quotaKey)
			}
			if tc.failCache {
				server.SetError("ERR quota cache unavailable")
			}
			if tc.failUpdate {
				require.NoError(t, db.Callback().Update().After("gorm:update").Register("test:wallet_cache_rollback", func(tx *gorm.DB) {
					if tx.Statement.Table == "wallets" {
						tx.AddError(errors.New("wallet rollback"))
					}
				}))
			}
			response := performManagedWalletRequest(router, token, http.MethodPost, user.Id, tc.body)
			assert.Contains(t, response.Body.String(), fmt.Sprintf(`"success":%t`, !tc.failUpdate && !tc.rejected))
			require.NoError(t, db.First(&user, user.Id).Error)
			assert.Equal(t, tc.legacy, user.Quota)
			wallet, err := model.GetUserWallet(user.Id)
			require.NoError(t, err)
			require.NotNil(t, wallet)
			assert.Equal(t, tc.after, wallet.Balance.Nanos)
			if tc.failCache {
				server.SetError("")
			}
			if tc.missingCache {
				assert.False(t, server.Exists(quotaKey))
			} else {
				assert.Equal(t, strconv.Itoa(tc.cached), server.HGet(quotaKey, "Quota"))
			}
			var entries int64
			require.NoError(t, db.Model(&model.LedgerEntry{}).Count(&entries).Error)
			if tc.failUpdate || tc.rejected {
				assert.EqualValues(t, 1, entries)
			} else {
				assert.EqualValues(t, 2, entries)
			}
		})
	}
}

func TestManagedWalletAdjustmentRepaysDebtAtomically(t *testing.T) {
	db := setupManagedWalletTestDB(t)
	operator := createQuotaTestOperator(t, db, common.RoleRootUser)
	user := model.User{Username: "indebted-wallet-owner", Role: common.RoleCommonUser, Quota: 1000}
	require.NoError(t, db.Create(&user).Error)
	t.Setenv("GATEWAY_SETTLEMENT_JOURNAL_DIR", t.TempDir())
	zero := money.Amount{Currency: "USD"}
	request := model.GatewayRequest{ID: "existing-debt", UserID: user.Id, ModelOfferingID: 17, SaleCurrency: "USD", SaleFXRate: "1", Status: "success", Receivable: zero}
	require.NoError(t, model.ReserveGateway(&request, zero, zero))
	request.Receivable = money.Amount{Nanos: 1500, Currency: "USD"}
	require.NoError(t, model.PrepareGatewaySettlement(model.GatewaySettlement{Request: request, SaleSource: request.Receivable}))
	_, err := model.SettleGateway(request.ID)
	require.NoError(t, err)
	router, token := managedWalletTestRouter(t, operator)
	require.NoError(t, db.Callback().Create().After("gorm:create").Register("test:repayment_rollback", func(tx *gorm.DB) {
		if entry, ok := tx.Statement.Dest.(*model.LedgerEntry); ok && entry.Type == model.LedgerEntryRepayment {
			tx.AddError(errors.New("repayment unavailable"))
		}
	}))
	failed := performManagedWalletRequest(router, token, http.MethodPost, user.Id, `{"amount":"0.000001","remark":"debt correction"}`)
	assert.Contains(t, failed.Body.String(), `"success":false`)
	require.NoError(t, db.Callback().Create().Remove("test:repayment_rollback"))
	wallet, err := model.GetUserWallet(user.Id)
	require.NoError(t, err)
	require.NotNil(t, wallet)
	assert.Zero(t, wallet.Balance.Nanos)
	assert.True(t, wallet.RequestsPaused)
	outstanding, err := model.GetUserOutstanding(user.Id)
	require.NoError(t, err)
	assert.Equal(t, int64(1500), outstanding.Nanos)
	var count int64
	require.NoError(t, db.Model(&model.LedgerEntry{}).Count(&count).Error)
	assert.EqualValues(t, 2, count)
	for _, tc := range []struct {
		outstanding, spendable int64
		paused                 bool
	}{
		{500, 0, true}, {0, 500, false},
	} {
		response := performManagedWalletRequest(router, token, http.MethodPost, user.Id, `{"amount":"0.000001","remark":"debt correction"}`)
		require.Contains(t, response.Body.String(), `"success":true`)
		wallet, err = model.GetUserWallet(user.Id)
		require.NoError(t, err)
		assert.Equal(t, tc.spendable, wallet.Balance.Nanos)
		assert.Equal(t, tc.paused, wallet.RequestsPaused)
		outstanding, err = model.GetUserOutstanding(user.Id)
		require.NoError(t, err)
		assert.Equal(t, tc.outstanding, outstanding.Nanos)
		read := performManagedWalletRequest(router, token, http.MethodGet, user.Id, "")
		var result struct {
			Success bool
			Data    walletView
		}
		require.NoError(t, common.Unmarshal(read.Body.Bytes(), &result))
		require.True(t, result.Success)
		assert.Equal(t, tc.paused, result.Data.RequestsPaused)
		assert.Equal(t, amountString(money.Amount{Nanos: tc.spendable, Currency: "USD"}), result.Data.Balance)
	}
	var adjustments, repayments, revenues []model.LedgerEntry
	require.NoError(t, db.Where("type = ?", model.LedgerEntryAdjustment).Order("id").Find(&adjustments).Error)
	require.Len(t, adjustments, 2)
	for _, entry := range adjustments {
		assert.Equal(t, money.Amount{Nanos: 1000, Currency: "USD"}, entry.Amount)
		assert.Equal(t, entry.Amount, entry.SourceAmount)
		assert.Equal(t, operator.Id, entry.OperatorId)
		assert.NotEmpty(t, entry.RequestId)
	}
	require.NoError(t, db.Where("type = ?", model.LedgerEntryRepayment).Find(&repayments).Error)
	require.Len(t, repayments, 4)
	for _, entry := range repayments {
		assert.Equal(t, "existing-debt", entry.RequestId)
	}
	require.NoError(t, db.Where("account_type = ?", model.LedgerAccountPlatformRevenue).Find(&revenues).Error)
	require.Len(t, revenues, 1)
	assert.Equal(t, money.Amount{Nanos: 1500, Currency: "USD"}, revenues[0].Amount)
	reconciliation, err := model.ReconcileWallets(10)
	require.NoError(t, err)
	assert.Empty(t, reconciliation.Mismatches)
	require.NoError(t, db.First(&user, user.Id).Error)
	assert.Equal(t, 1000, user.Quota)
}

func TestManageUserEnableAndPromoteRemainAvailable(t *testing.T) {
	for _, action := range []string{"enable", "promote"} {
		t.Run(action, func(t *testing.T) {
			db := setupManageUserTestDB(t)
			user := model.User{Username: "managed-routine-user", Role: common.RoleCommonUser, Status: common.UserStatusDisabled, Quota: 1000, AuthVersion: 1}
			require.NoError(t, db.Create(&user).Error)
			identity, proof := manageUserProof(t, db, service.VerificationOperation{Scope: service.VerificationScopeAdminUserManage, Context: []byte(fmt.Sprintf(`{"user_id":%d,"action":%q}`, user.Id, action))})
			// Retired fields are rejected before consuming an otherwise valid proof.
			invalid := performVerifiedManageUserRequest(t, fmt.Sprintf(`{"id":%d,"action":%q,"mode":"override","value":500}`, user.Id, action), identity, proof)
			assert.Contains(t, invalid.Body.String(), `"success":false`)
			response := performVerifiedManageUserRequest(t, fmt.Sprintf(`{"id":%d,"action":%q}`, user.Id, action), identity, proof)
			require.Contains(t, response.Body.String(), `"success":true`)
			require.NoError(t, db.First(&user, user.Id).Error)
			if action == "enable" {
				assert.Equal(t, common.UserStatusEnabled, user.Status)
			} else {
				assert.Equal(t, common.RoleAdminUser, user.Role)
			}
			assert.Equal(t, 1000, user.Quota)
		})
	}
}

// Saving notification settings starts from what the user stored: fields the
// form does not manage stay, and switches an older client leaves out keep
// their stored values. Only administrators change the channel and upstream
// switches; the independent reminder uses null for inheritance and preserves
// the old quota fields solely as migration source.
func TestUpdateUserSettingKeepsWhatTheRequestLeavesOut(t *testing.T) {
	on, off := true, false
	stored := dto.UserSetting{
		NotifyType:                       dto.NotifyTypeBark,
		BarkUrl:                          "https://api.day.app/key/{{title}}/{{content}}",
		QuotaWarningThreshold:            500000,
		QuotaWarningEnabled:              &off,
		ChannelNotifyEnabled:             &on,
		UpstreamModelUpdateNotifyEnabled: true,
		Language:                         "zhCN",
		SidebarModules:                   `{"chat":{"enabled":true}}`,
		BillingPreference:                "wallet_only",
		AcceptUnsetRatioModel:            true,
	}
	kept := func(setting dto.UserSetting) dto.UserSetting {
		setting.Language, setting.SidebarModules, setting.BillingPreference = stored.Language, stored.SidebarModules, stored.BillingPreference
		setting.AcceptUnsetRatioModel = stored.AcceptUnsetRatioModel
		setting.QuotaWarningThreshold, setting.QuotaWarningEnabled = stored.QuotaWarningThreshold, stored.QuotaWarningEnabled
		return setting
	}
	switchesChanged := `{"notify_type":"email","balance_reminder":{"enabled":true,"threshold":null},"channel_notify_enabled":false,"upstream_model_update_notify_enabled":false}`
	for _, tc := range []struct {
		name        string
		role        int
		body        string
		wantSuccess bool
		want        dto.UserSetting
	}{
		{
			name:        "older client without the new switches",
			role:        common.RoleAdminUser,
			body:        `{"notify_type":"webhook","webhook_url":"https://example.com/hook","webhook_secret":"hook-secret","record_ip_log":true}`,
			wantSuccess: true,
			want: kept(dto.UserSetting{
				NotifyType: dto.NotifyTypeWebhook, WebhookUrl: "https://example.com/hook", WebhookSecret: "hook-secret",
				QuotaWarningThreshold: 2000, QuotaWarningEnabled: &off, ChannelNotifyEnabled: &on, UpstreamModelUpdateNotifyEnabled: true,
				AcceptUnsetRatioModel: true, RecordIpLog: true,
			}),
		},
		{
			name:        "administrator changes the switches and uses the site default threshold",
			role:        common.RoleAdminUser,
			body:        switchesChanged,
			wantSuccess: true,
			want:        kept(dto.UserSetting{NotifyType: dto.NotifyTypeEmail, QuotaWarningEnabled: &on, ChannelNotifyEnabled: &off}),
		},
		{
			name:        "user cannot change the administrator switches",
			role:        common.RoleCommonUser,
			body:        switchesChanged,
			wantSuccess: true,
			want:        kept(dto.UserSetting{NotifyType: dto.NotifyTypeEmail, QuotaWarningEnabled: &on, ChannelNotifyEnabled: &on, UpstreamModelUpdateNotifyEnabled: true}),
		},
		{
			name: "negative threshold",
			role: common.RoleCommonUser,
			body: `{"notify_type":"email","balance_reminder":{"enabled":true,"threshold":{"nanos":"-1","currency":"USD"}}}`,
			want: stored,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupManageUserTestDB(t)
			setting, err := common.Marshal(stored)
			require.NoError(t, err)
			user := model.User{Username: "setting-owner", Role: tc.role, Status: common.UserStatusEnabled, AffCode: "setting-owner-aff", Setting: string(setting)}
			require.NoError(t, db.Create(&user).Error)

			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPut, "/api/user/setting", strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set("id", user.Id)
			UpdateUserSetting(c)

			assert.Contains(t, recorder.Body.String(), fmt.Sprintf(`"success":%t`, tc.wantSuccess))
			require.NoError(t, db.First(&user, user.Id).Error)
			assert.Equal(t, tc.want, user.GetSetting())
		})
	}
}

func reminderSettingsHTTP(t *testing.T, handler gin.HandlerFunc, method, path, body string, user model.User) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("id", user.Id)
	c.Set("role", user.Role)
	handler(c)
	c.Writer.WriteHeaderNow()
	return recorder
}

func TestBalanceReminderUserHTTPContract(t *testing.T) {
	db := setupManageUserTestDB(t)
	source := `{"notify_type":"email","quota_warning_threshold":500000,"quota_warning_enabled":false,"language":"fr","billing_preference":"subscription_only","record_ip_log":false}`
	user := model.User{Username: "reminder-http", AffCode: "reminder-http", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Setting: source}
	require.NoError(t, db.Create(&user).Error)
	threshold := money.Amount{Nanos: 9007199254740993, Currency: "USD"}
	before, err := model.PatchUserNotificationSettings(t.Context(), user.Id, &model.BalanceReminderSettings{Enabled: false, Threshold: &threshold}, nil)
	require.NoError(t, err)
	for _, member := range []string{
		`"quota_warning_threshold":0`, `"quota_warning_threshold":null`, `"quota_warning_threshold":{}`, `"QUOTA_WARNING_THRESHOLD":1`, `"quota_warning_threſhold":1`,
		`"quota_warning_enabled":false`, `"quota_warning_enabled":null`, `"QUOTA_WARNING_ENABLED":true`, `"quota_warning_enabled":true`,
		`"quota_warning_enabled":true,"quota_warning_enabled":false`,
	} {
		t.Run(member, func(t *testing.T) {
			response := reminderSettingsHTTP(t, UpdateUserSetting, http.MethodPut, "/api/user/setting", `{"notify_type":"email","record_ip_log":true,"balance_reminder":{"enabled":true,"threshold":null},`+member+`}`, user)
			assert.Equal(t, http.StatusGone, response.Code)
			assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			var result struct {
				Success bool
				Code    string
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
			assert.False(t, result.Success)
			assert.Equal(t, "legacy_quota_reminder_settings_retired", result.Code)
			var saved model.User
			require.NoError(t, db.First(&saved, user.Id).Error)
			assert.Equal(t, source, saved.Setting)
			current, err := model.GetUserBalanceReminder(t.Context(), user.Id)
			require.NoError(t, err)
			assert.Equal(t, before, current)
			require.NoError(t, db.Model(&model.User{}).Where("id = ?", user.Id).Update("setting", source).Error)
		})
	}
	response := reminderSettingsHTTP(t, GetSelf, http.MethodGet, "/api/user/self", "", user)
	var result struct {
		Success bool
		Data    struct {
			BalanceReminder *model.BalanceReminderView `json:"balance_reminder"`
			Setting         string                     `json:"setting"`
		}
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
	require.True(t, result.Success)
	require.NotNil(t, result.Data.BalanceReminder)
	assert.Equal(t, before, result.Data.BalanceReminder)
	assert.Equal(t, source, result.Data.Setting, "historical source is retained, not used as live reminder policy")
	assert.Contains(t, response.Body.String(), `"nanos":"9007199254740993"`)
}

func TestBalanceReminderUserHTTPAtomicPatch(t *testing.T) {
	db := setupManageUserTestDB(t)
	source := `{"notify_type":"email","quota_warning_threshold":0.000000000000000000001,"quota_warning_enabled":false,"language":"fr","sidebar_modules":"{\"chat\":true}","billing_preference":"subscription_only","future_security_state":{"nonce":"9007199254740993"}}`
	user := model.User{Username: "reminder-atomic", AffCode: "reminder-atomic", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, AuthVersion: 1, Quota: 500000, Setting: source}
	require.NoError(t, db.Create(&user).Error)
	previousRDB := common.RDB
	server := miniredis.RunT(t)
	common.RedisEnabled = true
	common.RDB = redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = common.RDB.Close(); common.RDB = previousRDB })
	_, err := model.GetUserCache(user.Id)
	require.NoError(t, err)
	var original map[string]common.RawMessage
	require.NoError(t, common.UnmarshalJsonStr(source, &original))
	assertKept := func() {
		t.Helper()
		var saved model.User
		require.NoError(t, db.First(&saved, user.Id).Error)
		var fields map[string]common.RawMessage
		require.NoError(t, common.UnmarshalJsonStr(saved.Setting, &fields))
		for _, key := range []string{"quota_warning_threshold", "quota_warning_enabled", "language", "sidebar_modules", "billing_preference", "future_security_state"} {
			assert.Equal(t, original[key], fields[key], key)
		}
		cache, err := model.GetUserCache(user.Id)
		require.NoError(t, err)
		assert.Equal(t, saved.Setting, cache.Setting)
		assert.Equal(t, int64(1), cache.AuthVersion)
		assert.Equal(t, 500000, saved.Quota)
		exists, err := common.RDB.HExists(t.Context(), fmt.Sprintf("user:%d", user.Id), "Quota").Result()
		require.NoError(t, err)
		assert.False(t, exists)
	}
	put := func(body string) (*httptest.ResponseRecorder, *model.BalanceReminderView, bool) {
		t.Helper()
		response := reminderSettingsHTTP(t, UpdateUserSetting, http.MethodPut, "/api/user/setting", body, user)
		var result struct {
			Success bool
			Data    struct {
				Reminder *model.BalanceReminderView `json:"balance_reminder"`
			}
		}
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
		return response, result.Data.Reminder, result.Success
	}
	reads, unlocked, written := 0, 0, false
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("reminder_http_lock", func(tx *gorm.DB) {
		if tx.Statement.Table == "users" && !written {
			reads++
			if _, ok := tx.Statement.Clauses["FOR"]; !ok {
				unlocked++
			}
		}
	}))
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register("reminder_http_write", func(tx *gorm.DB) {
		if tx.Statement.Table == "users" {
			written = true
		}
	}))
	_, current, ok := put(`{"notify_type":"email","record_ip_log":true,"balance_reminder":{"enabled":true,"threshold":{"nanos":"9007199254740993","currency":"USD"}}}`)
	require.NoError(t, db.Callback().Query().Remove("reminder_http_lock"))
	require.NoError(t, db.Callback().Update().Remove("reminder_http_write"))
	require.True(t, ok)
	require.NotNil(t, current)
	assert.Equal(t, 1, reads)
	assert.Zero(t, unlocked)
	assert.True(t, written)
	assert.Equal(t, int64(9007199254740993), current.Threshold.Nanos)
	assertKept()
	_, omitted, ok := put(`{"notify_type":"webhook","webhook_url":"https://example.com/reminder","webhook_secret":"fixture-only","record_ip_log":false}`)
	require.True(t, ok)
	assert.Equal(t, current, omitted)
	assertKept()
	for _, body := range []string{
		`{"notify_type":"email","balance_reminder":null}`, `{"notify_type":"email","balance_reminder":{}}`,
		`{"notify_type":"email","balance_reminder":{"enabled":true}}`,
		`{"notify_type":"email","balance_reminder":{"enabled":true,"threshold":{"nanos":1,"currency":"USD"}}}`,
		`{"notify_type":"email","balance_reminder":{"enabled":true,"threshold":{"nanos":"9223372036854775808","currency":"USD"}}}`,
		`{"notify_type":"email","balance_reminder":{"enabled":true,"threshold":{"nanos":"1","currency":"EUR"}}}`,
		`{"notify_type":"email","balance_reminder":{"enabled":true,"enabled":false,"threshold":null}}`,
		`{"notify_type":"email","balance_reminder":{"enabled":true,"ENABLED":false,"threshold":null}}`,
		`{"notify_type":"email","balance_reminder":{"enabled":true,"threshold":null,"effective_enabled":true}}`,
		`{"notify_type":"email","balance_reminder":{"enabled":true,"threshold":null},"user_id":999}`,
		`{"notify_type":"webhook","webhook_url":"://bad","balance_reminder":{"enabled":false,"threshold":null}}`,
		`{"notify_type":"bark","bark_url":"ftp://example.com","balance_reminder":{"enabled":false,"threshold":null}}`,
		`{"notify_type":"gotify","gotify_url":"https://example.com","balance_reminder":{"enabled":false,"threshold":null}}`,
		`{"notify_type":"email","notification_email":"invalid","balance_reminder":{"enabled":false,"threshold":null}}`,
	} {
		t.Run(body, func(t *testing.T) {
			var before model.User
			require.NoError(t, db.First(&before, user.Id).Error)
			_, _, accepted := put(body)
			assert.False(t, accepted)
			var after model.User
			require.NoError(t, db.First(&after, user.Id).Error)
			assert.Equal(t, before.Setting, after.Setting)
			view, err := model.GetUserBalanceReminder(t.Context(), user.Id)
			require.NoError(t, err)
			assert.Equal(t, current, view)
			assertKept()
		})
	}
	require.NoError(t, db.Exec(`CREATE FUNCTION reject_http_reminder() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'reminder unavailable'; END $$; CREATE TRIGGER reject_http_reminder BEFORE INSERT OR UPDATE ON user_balance_reminders FOR EACH ROW EXECUTE FUNCTION reject_http_reminder()`).Error)
	var before model.User
	require.NoError(t, db.First(&before, user.Id).Error)
	_, _, ok = put(`{"notify_type":"email","record_ip_log":true,"balance_reminder":{"enabled":false,"threshold":null}}`)
	assert.False(t, ok)
	var after model.User
	require.NoError(t, db.First(&after, user.Id).Error)
	assert.Equal(t, before.Setting, after.Setting)
	assertKept()
	view, err := model.GetUserBalanceReminder(t.Context(), user.Id)
	require.NoError(t, err)
	assert.Equal(t, current, view)
	require.NoError(t, db.Exec("DROP TRIGGER reject_http_reminder ON user_balance_reminders").Error)
	_, zero, ok := put(`{"notify_type":"email","balance_reminder":{"enabled":true,"threshold":{"nanos":"0","currency":"USD"}}}`)
	require.True(t, ok)
	require.NotNil(t, zero.Threshold)
	assert.Zero(t, zero.EffectiveThreshold.Nanos)
	_, inherited, ok := put(`{"notify_type":"email","balance_reminder":{"enabled":false,"threshold":null}}`)
	require.True(t, ok)
	assert.Nil(t, inherited.Threshold)
	assert.False(t, inherited.EffectiveEnabled)
	assert.Equal(t, int64(2000000), inherited.EffectiveThreshold.Nanos)
	assertKept()
	for _, handler := range []gin.HandlerFunc{GetSelf, UpdateUserSetting} {
		missing := user
		missing.Id += 100000
		response := reminderSettingsHTTP(t, handler, http.MethodPut, "/api/user/setting", `{"notify_type":"email"}`, missing)
		assert.Contains(t, response.Body.String(), `"success":false`)
	}
	require.NoError(t, db.Exec("ALTER TABLE user_balance_reminders RENAME TO unavailable_reminder_preferences").Error)
	response := reminderSettingsHTTP(t, GetSelf, http.MethodGet, "/api/user/self", "", user)
	assert.Contains(t, response.Body.String(), `"success":false`)
	assert.NotContains(t, response.Body.String(), `"balance_reminder"`)
	require.NoError(t, db.Exec("ALTER TABLE unavailable_reminder_preferences RENAME TO user_balance_reminders").Error)
	var events int64
	require.NoError(t, db.Table("balance_reminder_events").Count(&events).Error)
	assert.Zero(t, events)
}

func TestLegacyUnsetPricePreferenceIsRetired(t *testing.T) {
	for _, tc := range []struct {
		name   string
		member string
	}{
		{"true", `"accept_unset_model_ratio_model":true`},
		{"false", `"accept_unset_model_ratio_model":false`},
		{"null", `"accept_unset_model_ratio_model":null`},
		{"object", `"accept_unset_model_ratio_model":{}`},
		{"case alias", `"ACCEPT_UNSET_MODEL_RATIO_MODEL":true`},
		{"unicode alias", `"accept_unſet_model_ratio_model":true`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupManageUserTestDB(t)
			stored := dto.UserSetting{NotifyType: dto.NotifyTypeEmail, QuotaWarningThreshold: 500000, AcceptUnsetRatioModel: true, Language: "zh", BillingPreference: "wallet_only"}
			encoded, err := common.Marshal(stored)
			require.NoError(t, err)
			user := model.User{Username: "retired-price-setting-owner", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, AffCode: "retired-price-setting-aff", Setting: string(encoded)}
			require.NoError(t, db.Create(&user).Error)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			body := `{"notify_type":"email","quota_warning_threshold":0,"record_ip_log":true,` + tc.member + `}`
			c.Request = httptest.NewRequest(http.MethodPut, "/api/user/setting", strings.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set("id", user.Id)
			UpdateUserSetting(c)

			assert.Equal(t, http.StatusGone, recorder.Code, "retired preference must be rejected before unrelated settings are written")
			assert.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
			var result struct {
				Success    bool   `json:"success"`
				Code       string `json:"code"`
				MessageKey string `json:"message_key"`
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &result))
			assert.False(t, result.Success)
			assert.Equal(t, "legacy_unpriced_model_setting_retired", result.Code)
			assert.Equal(t, "The setting for unpriced models has been retired.", result.MessageKey)
			require.NoError(t, db.First(&user, user.Id).Error)
			assert.Equal(t, string(encoded), user.Setting, "retired preference rejection must preserve all stored settings")
		})
	}
}
