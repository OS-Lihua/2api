package controller

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type tokenAPIResponse struct {
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type tokenPageResponse struct {
	Items []tokenResponseItem `json:"items"`
}

type tokenResponseItem struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Key    string `json:"key"`
	Status int    `json:"status"`
}

type tokenKeyResponse struct {
	Key string `json:"key"`
}

func assertTokenManagementResponse(t *testing.T, recorder *httptest.ResponseRecorder, token *model.Token) {
	t.Helper()
	response := decodeAPIResponse(t, recorder)
	var fields map[string]common.RawMessage
	require.NoError(t, common.Unmarshal(response.Data, &fields))
	if items, paged := fields["items"]; paged {
		var entries []common.RawMessage
		require.NoError(t, common.Unmarshal(items, &entries))
		require.Len(t, entries, 1)
		fields = nil
		require.NoError(t, common.Unmarshal(entries[0], &fields))
	}
	assert.Equal(t, []string{"accessed_time", "allow_ips", "created_time", "expired_time", "id", "key", "name", "status"}, slices.Sorted(maps.Keys(fields)))
	var maskedKey string
	require.NoError(t, common.Unmarshal(fields["key"], &maskedKey))
	assert.Equal(t, token.GetMaskedKey(), maskedKey)
	assert.NotContains(t, recorder.Body.String(), token.GetFullKey())
}

type legacyToken struct {
	Id                 int    `gorm:"primaryKey"`
	UserId             int    `gorm:"index"`
	Key                string `gorm:"column:key;type:char(48);uniqueIndex"`
	Status             int    `gorm:"default:1"`
	Name               string `gorm:"index"`
	CreatedTime        int64  `gorm:"bigint"`
	AccessedTime       int64  `gorm:"bigint"`
	ExpiredTime        int64  `gorm:"bigint;default:-1"`
	RemainQuota        int    `gorm:"default:0"`
	UnlimitedQuota     bool
	ModelLimitsEnabled bool
	ModelLimits        string  `gorm:"type:text"`
	AllowIps           *string `gorm:"default:''"`
	UsedQuota          int     `gorm:"default:0"`
	Group              string  `gorm:"column:group;default:''"`
	CrossGroupRetry    bool
	DeletedAt          gorm.DeletedAt `gorm:"index"`
}

func (legacyToken) TableName() string {
	return "tokens"
}

func openTokenControllerTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	gin.SetMode(gin.TestMode)
	common.RedisEnabled = false
	return model.UseTestDatabases(t)
}

func migrateTokenControllerTestDB(t *testing.T, db *gorm.DB) {
	t.Helper()

	if err := db.AutoMigrate(&model.Token{}); err != nil {
		t.Fatalf("failed to migrate token table: %v", err)
	}
}

func setupTokenControllerTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	db := openTokenControllerTestDB(t)
	migrateTokenControllerTestDB(t, db)
	return db
}

func seedToken(t *testing.T, db *gorm.DB, userID int, name string, rawKey string) *model.Token {
	t.Helper()

	token := &model.Token{
		UserId:         userID,
		Name:           name,
		Key:            rawKey,
		Status:         common.TokenStatusEnabled,
		CreatedTime:    1,
		AccessedTime:   1,
		ExpiredTime:    -1,
		RemainQuota:    100,
		UnlimitedQuota: true,
		Group:          "default",
	}
	if err := db.Create(token).Error; err != nil {
		t.Fatalf("failed to create token: %v", err)
	}
	return token
}

func newAuthenticatedContext(t *testing.T, method string, target string, body any, userID int) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()

	var requestBody *bytes.Reader
	if body != nil {
		payload, err := common.Marshal(body)
		if err != nil {
			t.Fatalf("failed to marshal request body: %v", err)
		}
		requestBody = bytes.NewReader(payload)
	} else {
		requestBody = bytes.NewReader(nil)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(method, target, requestBody)
	if body != nil {
		ctx.Request.Header.Set("Content-Type", "application/json")
	}
	ctx.Set("id", userID)
	return ctx, recorder
}

func decodeAPIResponse(t *testing.T, recorder *httptest.ResponseRecorder) tokenAPIResponse {
	t.Helper()

	var response tokenAPIResponse
	if err := common.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to decode api response: %v", err)
	}
	return response
}

func getTokenKeyColumnType(t *testing.T, db *gorm.DB) string {
	t.Helper()

	var dataType string
	var maxLength sql.NullInt64
	if err := db.Raw(`SELECT data_type, character_maximum_length
		FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = ? AND column_name = ?`,
		"tokens", "key").Row().Scan(&dataType, &maxLength); err != nil {
		t.Fatalf("failed to inspect token key column: %v", err)
	}
	switch strings.ToLower(dataType) {
	case "character varying":
		return fmt.Sprintf("varchar(%d)", maxLength.Int64)
	case "character":
		return fmt.Sprintf("char(%d)", maxLength.Int64)
	default:
		if maxLength.Valid {
			return fmt.Sprintf("%s(%d)", strings.ToLower(dataType), maxLength.Int64)
		}
		return strings.ToLower(dataType)
	}
}

func getTokenAutoGroupsColumnType(t *testing.T, db *gorm.DB) string {
	t.Helper()

	var dataType string
	if err := db.Raw(`SELECT data_type FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = ? AND column_name = ?`,
		"tokens", "auto_groups").Scan(&dataType).Error; err != nil {
		t.Fatalf("failed to inspect token auto_groups column: %v", err)
	}
	return strings.ToLower(dataType)
}

func TestTokenMigrationFromChar48ToVarchar128(t *testing.T) {
	db := openTokenControllerTestDB(t)

	legacyKey := strings.Repeat("a", 48)
	longKey := strings.Repeat("b", 64)

	if err := db.AutoMigrate(&legacyToken{}); err != nil {
		t.Fatalf("failed to create legacy token schema: %v", err)
	}
	if err := db.Create(&legacyToken{
		UserId:             7,
		Key:                legacyKey,
		Status:             common.TokenStatusEnabled,
		Name:               "legacy-token",
		CreatedTime:        1,
		AccessedTime:       1,
		ExpiredTime:        -1,
		RemainQuota:        100,
		UnlimitedQuota:     true,
		ModelLimitsEnabled: false,
		ModelLimits:        "",
		AllowIps:           common.GetPointer(""),
		UsedQuota:          0,
		Group:              "default",
		CrossGroupRetry:    false,
	}).Error; err != nil {
		t.Fatalf("failed to seed legacy token row: %v", err)
	}

	if got := getTokenKeyColumnType(t, db); got != "char(48)" {
		t.Fatalf("expected legacy key column type char(48), got %q", got)
	}

	migrateTokenControllerTestDB(t, db)

	if got := getTokenKeyColumnType(t, db); got != "varchar(128)" {
		t.Fatalf("expected migrated key column type varchar(128), got %q", got)
	}
	if !db.Migrator().HasColumn(&model.Token{}, "auto_groups") {
		t.Fatal("expected migration to add auto_groups column")
	}
	if got := getTokenAutoGroupsColumnType(t, db); got != "text" {
		t.Fatalf("expected migrated auto_groups column type text, got %q", got)
	}

	var migratedToken model.Token
	if err := db.First(&migratedToken, "name = ?", "legacy-token").Error; err != nil {
		t.Fatalf("failed to load migrated token row: %v", err)
	}
	if migratedToken.Key != legacyKey {
		t.Fatalf("expected migrated token key %q, got %q", legacyKey, migratedToken.Key)
	}
	if migratedToken.Name != "legacy-token" {
		t.Fatalf("expected migrated token name to be preserved, got %q", migratedToken.Name)
	}
	if migratedToken.AutoGroups != "" {
		t.Fatalf("expected legacy token to inherit global Auto groups, got %q", migratedToken.AutoGroups)
	}
	require.True(t, db.Migrator().HasColumn(&model.Token{}, "allow_user_quota_query"))
	assert.False(t, migratedToken.AllowUserQuotaQuery, "tokens created before the upgrade keep user quota queries off")

	inserted := model.Token{
		UserId:             8,
		Name:               "long-token",
		Key:                longKey,
		Status:             common.TokenStatusEnabled,
		CreatedTime:        1,
		AccessedTime:       1,
		ExpiredTime:        -1,
		RemainQuota:        200,
		UnlimitedQuota:     true,
		ModelLimitsEnabled: false,
		ModelLimits:        "",
		AllowIps:           common.GetPointer(""),
		UsedQuota:          0,
		Group:              "default",
		CrossGroupRetry:    false,
	}
	if err := db.Create(&inserted).Error; err != nil {
		t.Fatalf("failed to insert long token after migration: %v", err)
	}

	var fetched model.Token
	if err := db.First(&fetched, "id = ?", inserted.Id).Error; err != nil {
		t.Fatalf("failed to fetch long token after migration: %v", err)
	}
	if fetched.Key != longKey {
		t.Fatalf("expected long token key %q, got %q", longKey, fetched.Key)
	}
}

func TestTokenAutoMigrateUsesVarchar128KeyColumn(t *testing.T) {
	db := setupTokenControllerTestDB(t)

	if got := getTokenKeyColumnType(t, db); got != "varchar(128)" {
		t.Fatalf("expected key column type varchar(128), got %q", got)
	}
	if got := getTokenAutoGroupsColumnType(t, db); got != "text" {
		t.Fatalf("expected auto_groups column type text, got %q", got)
	}
}

func TestGetAllTokensMasksKeyInResponse(t *testing.T) {
	db := setupTokenControllerTestDB(t)
	token := seedToken(t, db, 1, "list-token", "abcd1234efgh5678")
	seedToken(t, db, 2, "other-user-token", "zzzz1234yyyy5678")

	ctx, recorder := newAuthenticatedContext(t, http.MethodGet, "/api/token/?p=1&size=10", nil, 1)
	GetAllTokens(ctx)

	response := decodeAPIResponse(t, recorder)
	if !response.Success {
		t.Fatalf("expected success response, got message: %s", response.Message)
	}

	var page tokenPageResponse
	if err := common.Unmarshal(response.Data, &page); err != nil {
		t.Fatalf("failed to decode token page response: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("expected exactly one token, got %d", len(page.Items))
	}
	if page.Items[0].Key != token.GetMaskedKey() {
		t.Fatalf("expected masked key %q, got %q", token.GetMaskedKey(), page.Items[0].Key)
	}
	if strings.Contains(recorder.Body.String(), token.Key) {
		t.Fatalf("list response leaked raw token key: %s", recorder.Body.String())
	}
	assertTokenManagementResponse(t, recorder, token)
}

func TestSearchTokensMasksKeyInResponse(t *testing.T) {
	db := setupTokenControllerTestDB(t)
	token := seedToken(t, db, 1, "searchable-token", "ijkl1234mnop5678")

	ctx, recorder := newAuthenticatedContext(t, http.MethodGet, "/api/token/search?keyword=searchable-token&p=1&size=10", nil, 1)
	SearchTokens(ctx)

	response := decodeAPIResponse(t, recorder)
	if !response.Success {
		t.Fatalf("expected success response, got message: %s", response.Message)
	}

	var page tokenPageResponse
	if err := common.Unmarshal(response.Data, &page); err != nil {
		t.Fatalf("failed to decode search response: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("expected exactly one search result, got %d", len(page.Items))
	}
	if page.Items[0].Key != token.GetMaskedKey() {
		t.Fatalf("expected masked search key %q, got %q", token.GetMaskedKey(), page.Items[0].Key)
	}
	if strings.Contains(recorder.Body.String(), token.Key) {
		t.Fatalf("search response leaked raw token key: %s", recorder.Body.String())
	}
	assertTokenManagementResponse(t, recorder, token)
}

func TestGetTokenMasksKeyInResponse(t *testing.T) {
	db := setupTokenControllerTestDB(t)
	token := seedToken(t, db, 1, "detail-token", "qrst1234uvwx5678")

	ctx, recorder := newAuthenticatedContext(t, http.MethodGet, "/api/token/"+strconv.Itoa(token.Id), nil, 1)
	ctx.Params = gin.Params{{Key: "id", Value: strconv.Itoa(token.Id)}}
	GetToken(ctx)

	response := decodeAPIResponse(t, recorder)
	if !response.Success {
		t.Fatalf("expected success response, got message: %s", response.Message)
	}

	var detail tokenResponseItem
	if err := common.Unmarshal(response.Data, &detail); err != nil {
		t.Fatalf("failed to decode token detail response: %v", err)
	}
	if detail.Key != token.GetMaskedKey() {
		t.Fatalf("expected masked detail key %q, got %q", token.GetMaskedKey(), detail.Key)
	}
	if strings.Contains(recorder.Body.String(), token.Key) {
		t.Fatalf("detail response leaked raw token key: %s", recorder.Body.String())
	}
	assertTokenManagementResponse(t, recorder, token)
}

func TestUpdateTokenMasksKeyInResponse(t *testing.T) {
	db := setupTokenControllerTestDB(t)
	token := seedToken(t, db, 1, "editable-token", "yzab1234cdef5678")

	body := map[string]any{
		"id":           token.Id,
		"name":         "updated-token",
		"expired_time": -1,
		"allow_ips":    "203.0.113.0/24",
	}

	ctx, recorder := newAuthenticatedContext(t, http.MethodPut, "/api/token/", body, 1)
	UpdateToken(ctx)

	response := decodeAPIResponse(t, recorder)
	if !response.Success {
		t.Fatalf("expected success response, got message: %s", response.Message)
	}

	var detail tokenResponseItem
	if err := common.Unmarshal(response.Data, &detail); err != nil {
		t.Fatalf("failed to decode token update response: %v", err)
	}
	if detail.Key != token.GetMaskedKey() {
		t.Fatalf("expected masked update key %q, got %q", token.GetMaskedKey(), detail.Key)
	}
	if strings.Contains(recorder.Body.String(), token.Key) {
		t.Fatalf("update response leaked raw token key: %s", recorder.Body.String())
	}
	assertTokenManagementResponse(t, recorder, token)
}

func TestGetTokenKeyRequiresOwnershipAndReturnsFullKey(t *testing.T) {
	db := setupTokenControllerTestDB(t)
	token := seedToken(t, db, 1, "owned-token", "owner1234token5678")

	authorizedCtx, authorizedRecorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/"+strconv.Itoa(token.Id)+"/key", nil, 1)
	authorizedCtx.Params = gin.Params{{Key: "id", Value: strconv.Itoa(token.Id)}}
	GetTokenKey(authorizedCtx)

	authorizedResponse := decodeAPIResponse(t, authorizedRecorder)
	if !authorizedResponse.Success {
		t.Fatalf("expected authorized key fetch to succeed, got message: %s", authorizedResponse.Message)
	}

	var keyData tokenKeyResponse
	if err := common.Unmarshal(authorizedResponse.Data, &keyData); err != nil {
		t.Fatalf("failed to decode token key response: %v", err)
	}
	if keyData.Key != token.GetFullKey() {
		t.Fatalf("expected full key %q, got %q", token.GetFullKey(), keyData.Key)
	}

	unauthorizedCtx, unauthorizedRecorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/"+strconv.Itoa(token.Id)+"/key", nil, 2)
	unauthorizedCtx.Params = gin.Params{{Key: "id", Value: strconv.Itoa(token.Id)}}
	GetTokenKey(unauthorizedCtx)

	unauthorizedResponse := decodeAPIResponse(t, unauthorizedRecorder)
	if unauthorizedResponse.Success {
		t.Fatalf("expected unauthorized key fetch to fail")
	}
	if strings.Contains(unauthorizedRecorder.Body.String(), token.Key) {
		t.Fatalf("unauthorized key response leaked raw token key: %s", unauthorizedRecorder.Body.String())
	}
}

func TestTokenManagementCreatesCredentialSettings(t *testing.T) {
	db := setupTokenControllerTestDB(t)
	body := map[string]any{"name": "new-credential", "expired_time": -1, "allow_ips": "203.0.113.0/24"}
	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/", body, 1)
	AddToken(ctx)
	require.True(t, decodeAPIResponse(t, recorder).Success)
	tokens, err := model.GetAllUserTokens(1, 0, 10)
	require.NoError(t, err)
	require.Len(t, tokens, 1)
	token := tokens[0]
	assert.Equal(t, "new-credential", token.Name)
	assert.Equal(t, int64(-1), token.ExpiredTime)
	require.NotNil(t, token.AllowIps)
	assert.Equal(t, "203.0.113.0/24", *token.AllowIps)
	assert.Equal(t, common.TokenStatusEnabled, token.Status)
	assert.NotEmpty(t, token.GetFullKey())
	assert.NotContains(t, recorder.Body.String(), token.GetFullKey())
	assert.Zero(t, token.RemainQuota)
	assert.Zero(t, token.UsedQuota)
	assert.False(t, token.UnlimitedQuota)
	assert.False(t, token.AllowUserQuotaQuery)
	assert.False(t, token.ModelLimitsEnabled)
	assert.Empty(t, token.ModelLimits)
	assert.Empty(t, token.Group)
	assert.Empty(t, token.AutoGroups)
	assert.False(t, token.CrossGroupRetry)
	var count int64
	require.NoError(t, db.Model(&model.Token{}).Count(&count).Error)
	assert.EqualValues(t, 1, count)
}

func TestTokenManagementUpdatesPreserveHistoryAndPermissionRevision(t *testing.T) {
	db := setupTokenControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.TokenOfferingPolicy{}, &model.TokenOfferingException{}))
	token := seedToken(t, db, 1, "historical-credential", "history-permission-test-key")
	token.RemainQuota, token.UsedQuota = 73, 27
	token.UnlimitedQuota = false
	token.AllowUserQuotaQuery, token.ModelLimitsEnabled = true, true
	token.ModelLimits, token.Group = "historical-model", "auto"
	token.AutoGroups, token.CrossGroupRetry = `["vip","default"]`, true
	require.NoError(t, db.Save(token).Error)
	permissions, err := model.MutateKeyPermissions(t.Context(), token.Id, token.UserId, 0, "deny_all", model.KeyPermissions{}, nil)
	require.NoError(t, err)
	require.Equal(t, int64(1), permissions.Revision)

	for _, tc := range []struct {
		name, target string
		body         map[string]any
		status       int
	}{
		{"edit", "/api/token/", map[string]any{"id": token.Id, "name": "updated-credential", "expired_time": int64(4102444800), "allow_ips": "203.0.113.0/24"}, common.TokenStatusEnabled},
		{"disable", "/api/token/?status_only=true", map[string]any{"id": token.Id, "status": common.TokenStatusDisabled}, common.TokenStatusDisabled},
		{"enable", "/api/token/?status_only=true", map[string]any{"id": token.Id, "status": common.TokenStatusEnabled}, common.TokenStatusEnabled},
		{"clear IP and expiry", "/api/token/", map[string]any{"id": token.Id, "name": "updated-credential", "expired_time": -1, "allow_ips": ""}, common.TokenStatusEnabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, recorder := newAuthenticatedContext(t, http.MethodPut, tc.target, tc.body, token.UserId)
			UpdateToken(ctx)
			require.True(t, decodeAPIResponse(t, recorder).Success)
			assertTokenManagementResponse(t, recorder, token)
			if tc.target == "/api/token/" {
				token.Name = tc.body["name"].(string)
				token.AllowIps = common.GetPointer(tc.body["allow_ips"].(string))
				if tc.name == "edit" {
					token.ExpiredTime = 4102444800
				} else {
					token.ExpiredTime = -1
				}
			}
			token.Status = tc.status
			saved, err := model.GetTokenByIds(token.Id, token.UserId)
			require.NoError(t, err)
			assert.Equal(t, *token, *saved)
			savedPermissions, err := model.GetKeyPermissions(t.Context(), token.Id, token.UserId)
			require.NoError(t, err)
			assert.Equal(t, permissions, savedPermissions, "credential settings must not bypass permission revisions")
		})
	}
}

func TestAddTokenPreservesCountLimit(t *testing.T) {
	db := setupTokenControllerTestDB(t)
	settings := operation_setting.GetTokenSetting()
	previousLimit := settings.MaxUserTokens
	settings.MaxUserTokens = 1
	t.Cleanup(func() { settings.MaxUserTokens = previousLimit })
	seedToken(t, db, 1, "existing-credential", "count-limit-test-key")
	ctx, recorder := newAuthenticatedContext(t, http.MethodPost, "/api/token/", map[string]any{"name": "over-limit", "expired_time": -1}, 1)
	AddToken(ctx)
	assert.False(t, decodeAPIResponse(t, recorder).Success)
	tokens, err := model.GetAllUserTokens(1, 0, 10)
	require.NoError(t, err)
	require.Len(t, tokens, 1)
	assert.Equal(t, "existing-credential", tokens[0].Name)
}

func TestGetTokenUsageRetiresQuotaWithoutBypassingIPWhitelist(t *testing.T) {
	require.NoError(t, i18n.Init())
	for _, tc := range []struct {
		name       string
		allow      bool
		expired    bool
		allowIps   string
		remoteAddr string
		wantStatus int
		language   string
		message    string
	}{
		{name: "switch off", remoteAddr: "203.0.113.7:4321", wantStatus: http.StatusGone},
		{name: "simplified Chinese client", remoteAddr: "203.0.113.7:4321", wantStatus: http.StatusGone, language: "zh-CN", message: "基于旧额度的账单查询已退役，请使用已授权的钱包 API。"},
		{name: "traditional Chinese client", remoteAddr: "203.0.113.7:4321", wantStatus: http.StatusGone, language: "zh-TW", message: "以舊額度為基礎的帳單查詢已退役，請使用已授權的錢包 API。"},
		{name: "switch on without IP whitelist", allow: true, remoteAddr: "203.0.113.7:4321", wantStatus: http.StatusGone},
		{name: "switch on with matching IP whitelist", allow: true, allowIps: "198.51.100.1\n203.0.113.0/24", remoteAddr: "203.0.113.7:4321", wantStatus: http.StatusGone},
		{name: "expired key with the switch on", allow: true, expired: true, remoteAddr: "203.0.113.7:4321", wantStatus: http.StatusGone},
		{name: "switch on from an IP outside the whitelist", allow: true, allowIps: "198.51.100.1", remoteAddr: "203.0.113.7:4321", wantStatus: http.StatusForbidden},
		{name: "switch off from an IP outside the whitelist", allowIps: "198.51.100.1", remoteAddr: "203.0.113.7:4321", wantStatus: http.StatusForbidden},
		{name: "unresolved client IP with a whitelist", allow: true, allowIps: "203.0.113.0/24", remoteAddr: "unresolved", wantStatus: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupTokenControllerTestDB(t)
			require.NoError(t, db.AutoMigrate(&model.User{}))
			// Initialize production column quoting for the token key lookup.
			t.Setenv("LOG_SQL_DSN", "")
			require.NoError(t, model.InitLogDB())
			user := &model.User{Username: "quota-owner", Password: "placeholder", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", Quota: 12345, UsedQuota: 678, AuthVersion: 1, AffCode: "quota-owner"}
			require.NoError(t, db.Create(user).Error)
			token := seedToken(t, db, user.Id, "usage-token", "usagekey1234")
			updates := map[string]any{"allow_user_quota_query": tc.allow, "allow_ips": tc.allowIps, "used_quota": 30}
			if tc.expired {
				updates["status"], updates["expired_time"] = common.TokenStatusExpired, 1
			}
			require.NoError(t, db.Model(token).Updates(updates).Error)

			router := gin.New()
			router.GET("/api/usage/token/", middleware.TokenAuthReadOnly(), GetTokenUsage)
			request := httptest.NewRequest(http.MethodGet, "/api/usage/token/", nil)
			request.Header.Set("Authorization", "Bearer sk-usagekey1234")
			language := tc.language
			if language == "" {
				language = "en-US"
			}
			request.Header.Set("Accept-Language", language)
			request.RemoteAddr = tc.remoteAddr
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)

			require.Equal(t, tc.wantStatus, recorder.Code, recorder.Body.String())
			for _, field := range []string{"total_granted", "total_available", "total_used", "unlimited_quota", "model_limits", "user_quota", "balance", "consumed", "outstanding"} {
				assert.NotContains(t, recorder.Body.String(), `"`+field+`"`)
			}
			if tc.wantStatus == http.StatusGone {
				message := tc.message
				if message == "" {
					message = "Quota-based billing queries have been retired. Use the authorized wallet APIs."
				}
				expected, err := common.Marshal(gin.H{"success": false, "message": message, "message_key": "Quota-based billing queries have been retired. Use the authorized wallet APIs.", "code": "quota_billing_retired"})
				require.NoError(t, err)
				assert.JSONEq(t, string(expected), recorder.Body.String())
				assert.Contains(t, recorder.Header().Get("Cache-Control"), "no-store")
			}
			saved, err := model.GetTokenByIds(token.Id, user.Id)
			require.NoError(t, err)
			assert.Equal(t, 100, saved.RemainQuota)
			assert.Equal(t, 30, saved.UsedQuota)
			assert.Equal(t, tc.allow, saved.AllowUserQuotaQuery)
			assert.Equal(t, tc.allowIps, *saved.AllowIps)
			wantTokenStatus := common.TokenStatusEnabled
			if tc.expired {
				wantTokenStatus = common.TokenStatusExpired
			}
			assert.Equal(t, wantTokenStatus, saved.Status)
		})
	}
}

func TestAPITokenAuditDatabase(t *testing.T) {
	for _, separateLog := range []bool{false, true} {
		t.Run(fmt.Sprintf("separate_log=%v", separateLog), func(t *testing.T) {
			previousDB, previousLogDB := model.DB, model.LOG_DB
			previousLog := common.LogDatabaseType()
			previousRedis, previousMaster, previousSecret := common.RedisEnabled, common.IsMasterNode, common.SessionSecret
			t.Cleanup(func() {
				model.DB, model.LOG_DB = previousDB, previousLogDB
				common.SetLogDatabaseType(previousLog)
				common.RedisEnabled, common.IsMasterNode, common.SessionSecret = previousRedis, previousMaster, previousSecret
			})
			common.RedisEnabled, common.IsMasterNode = false, true
			common.SessionSecret = "api-token-audit-test-secret"
			t.Setenv("LOG_SQL_DSN", "")
			db, _ := newAuditTestDatabase(t, "postgres")
			model.DB = db
			require.NoError(t, db.AutoMigrate(&model.User{}, &model.UserSession{}, &model.UserAccessToken{}, &model.Token{}))
			// Use the startup path that places the audit table on the primary database.
			require.NoError(t, model.InitLogDB())
			if separateLog {
				logDB, _ := newAuditTestDatabase(t, "postgres")
				model.LOG_DB = logDB
				require.NoError(t, model.MigrateAuditLogs())
			}
			verifyAPITokenAudit(t)
			if separateLog {
				var count int64
				require.NoError(t, db.Model(&model.AuditLog{}).Count(&count).Error)
				assert.Zero(t, count, "all audit events must use the configured log database")
			}
		})
	}
}

func verifyAPITokenAudit(t *testing.T) {
	t.Helper()
	user := &model.User{Username: "token-owner", Password: "placeholder", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: "token-owner"}
	require.NoError(t, model.DB.Create(user).Error)
	pat, _ := createScopedAccessToken(t, user.Id, time.Now().Add(time.Hour).Unix(), "api_key:reveal")
	other := &model.User{Username: "other-owner", Password: "placeholder", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: "other-owner"}
	require.NoError(t, model.DB.Create(other).Error)
	session := &model.UserSession{SID: "token-audit-session", UserID: user.Id, Version: 1, UserAuthVersion: 1, Status: model.UserSessionStatusActive, RefreshHash: "placeholder", LoginMethod: "password", LastActiveAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Hour).Unix()}
	require.NoError(t, model.CreateUserSession(session))
	jwt, _, err := service.IssueAccessToken(service.AuthIdentity{UserID: user.Id, SessionID: session.SID, UserAuthVersion: 1, SessionVersion: 1})
	require.NoError(t, err)
	router := gin.New()
	router.Use(middleware.RequestId(), middleware.AccessTokenAudit())
	tokenRoutes := router.Group("/api/token", middleware.UserAuth(), middleware.TokenOperationAudit())
	tokenRoutes.POST("/", AddToken)
	tokenRoutes.PUT("/", UpdateToken)
	tokenRoutes.DELETE("/:id", DeleteToken)
	tokenRoutes.POST("/batch", DeleteTokenBatch)
	tokenRoutes.POST("/batch/keys", GetTokenKeysBatch)
	tokenRoutes.POST("/:id/key", func(c *gin.Context) {
		if c.GetHeader("X-Test-Limit") != "" {
			c.AbortWithStatusJSON(429, gin.H{"success": false})
			return
		}
		c.Next()
	}, GetTokenKey)
	tokenRoutes.GET("/", GetAllTokens)
	tokenRoutes.GET("/:id", GetToken)
	tokenRoutes.GET("/search", SearchTokens)
	router.GET("/api/audit/self", middleware.UserAuth(), GetAuditLogs)

	for index, tc := range []struct {
		name, method, path, body, action string
		success                          bool
		params                           string
		initialStatus                    int
		failWrite, rateLimit, usePAT     bool
		expired                          bool
	}{
		{name: "create", method: "POST", path: "/", body: `{"name":"created","expired_time":-1}`, action: "token.create", success: true},
		{name: "invalid create", method: "POST", path: "/", body: `{"name":"attempt","remain_quota":-1}`, action: "token.create", params: `{}`},
		{name: "malformed body", method: "POST", path: "/", body: `{"key":"raw-body-secret"`, action: "token.create", params: `{}`},
		{name: "validation error exceeds audit buffer", method: "POST", path: "/", body: `{"expired_time":` + strings.Repeat("9", 64*1024) + `}`, action: "token.create", params: `{}`},
		{name: "create storage failure", method: "POST", path: "/", body: `{"name":"attempt","expired_time":-1}`, action: "token.create", params: `{"name":"attempt"}`, failWrite: true},
		{name: "normalized update", method: "PUT", path: "/", body: `{"id":$id,"name":"renamed","expired_time":-1,"allow_ips":""}`, action: "token.update", success: true, params: `{"id":$id,"name":"renamed","changed_fields":["name"]}`},
		{name: "configuration values stay private", method: "PUT", path: "/", body: `{"id":$id,"name":"owned","expired_time":42,"allow_ips":"203.0.113.57"}`, action: "token.update", success: true, params: `{"id":$id,"name":"owned","changed_fields":["expired_time","allow_ips"]}`},
		{name: "unchanged update", method: "PUT", path: "/", body: `{"id":$id,"name":"owned","expired_time":-1,"allow_ips":""}`, action: "token.update", success: true, params: `{"id":$id,"name":"owned","changed_fields":[]}`},
		{name: "retired user quota query update", method: "PUT", path: "/", body: `{"id":$id,"name":"owned","expired_time":-1,"allow_ips":"","allow_user_quota_query":true}`, action: "token.update", params: `{}`},
		{name: "retired model settings stay private", method: "PUT", path: "/", body: `{"id":$id,"name":"owned","expired_time":-1,"model_limits":"private-model-configuration"}`, action: "token.update", params: `{}`},
		{name: "successful response exceeds audit buffer", method: "PUT", path: "/", body: `{"id":$id,"name":"owned","expired_time":-1,"allow_ips":"` + strings.Repeat("203.0.113.0/24\\n", 5000) + `"}`, action: "token.update", success: true, params: `{"id":$id,"name":"owned","changed_fields":["allow_ips"]}`},
		{name: "update storage failure", method: "PUT", path: "/", body: `{"id":$id,"name":"failed-rename","expired_time":-1}`, action: "token.update", params: `{"id":$id,"name":"owned"}`, failWrite: true},
		{name: "foreign update", method: "PUT", path: "/", body: `{"id":$other,"name":"forged-name","expired_time":-1}`, action: "token.update", params: `{"id":$other}`},
		{name: "foreign status update", method: "PUT", path: "/?status_only=true", body: `{"id":$other,"status":1}`, action: "token.status_update", params: `{"id":$other}`},
		{name: "invalid status", method: "PUT", path: "/?status_only=true", body: `{"id":$id,"status":4}`, action: "token.status_update", params: `{"id":$id}`},
		{name: "status update rejects configuration", method: "PUT", path: "/?status_only=true", body: `{"id":$id,"status":2,"allow_ips":"203.0.113.57"}`, action: "token.status_update", params: `{}`},
		{name: "configuration update rejects status", method: "PUT", path: "/", body: `{"id":$id,"name":"owned","expired_time":-1,"status":2}`, action: "token.update", params: `{}`},
		{name: "credential injection", method: "POST", path: "/", body: `{"name":"injected","key":"raw-body-secret"}`, action: "token.create", params: `{}`},
		{name: "ownership injection", method: "PUT", path: "/", body: `{"id":$id,"user_id":$other,"name":"injected","expired_time":-1}`, action: "token.update", params: `{}`},
		{name: "trailing body", method: "POST", path: "/", body: `{"name":"injected","expired_time":-1} {"key":"raw-body-secret"}`, action: "token.create", params: `{}`},
		{name: "long name", method: "POST", path: "/", body: `{"name":"` + strings.Repeat("n", 51) + `","expired_time":-1}`, action: "token.create", params: `{}`},
		{name: "long update name", method: "PUT", path: "/", body: `{"id":$id,"name":"` + strings.Repeat("n", 51) + `","expired_time":-1}`, action: "token.update", params: `{"id":$id}`},
		{name: "missing status", method: "PUT", path: "/?status_only=true", body: `{"id":$id}`, action: "token.status_update", params: `{"id":$id}`},
		{name: "exhausted enable", method: "PUT", path: "/?status_only=true", body: `{"id":$id,"status":1}`, action: "token.status_update", success: true, params: `{"id":$id,"name":"owned","from":4,"to":1}`, initialStatus: common.TokenStatusExhausted},
		{name: "disable", method: "PUT", path: "/?status_only=true", body: `{"id":$id,"status":2}`, action: "token.status_update", success: true, params: `{"id":$id,"name":"owned","from":1,"to":2}`},
		{name: "enable", method: "PUT", path: "/?status_only=true", body: `{"id":$id,"status":1}`, action: "token.status_update", success: true, params: `{"id":$id,"name":"owned","from":2,"to":1}`, initialStatus: common.TokenStatusDisabled},
		{name: "expired enable", method: "PUT", path: "/?status_only=true", body: `{"id":$id,"status":1}`, action: "token.status_update", params: `{"id":$id,"name":"owned"}`, initialStatus: common.TokenStatusExpired},
		{name: "expired disabled enable", method: "PUT", path: "/?status_only=true", body: `{"id":$id,"status":1}`, action: "token.status_update", params: `{"id":$id,"name":"owned"}`, initialStatus: common.TokenStatusDisabled, expired: true},
		{name: "expired exhausted enable", method: "PUT", path: "/?status_only=true", body: `{"id":$id,"status":1}`, action: "token.status_update", params: `{"id":$id,"name":"owned"}`, initialStatus: common.TokenStatusExhausted, expired: true},
		{name: "delete", method: "DELETE", path: "/$id", action: "token.delete", success: true, params: `{"id":$id,"name":"owned"}`},
		{name: "foreign delete", method: "DELETE", path: "/$other", action: "token.delete", params: `{"id":$other}`},
		{name: "missing delete", method: "DELETE", path: "/999999", action: "token.delete", params: `{"id":999999}`},
		{name: "key view", method: "POST", path: "/$id/key", action: "token.key_view", success: true, params: `{"id":$id,"name":"owned"}`},
		{name: "PAT key view", method: "POST", path: "/$id/key", action: "token.key_view", success: true, params: `{"id":$id,"name":"owned"}`, usePAT: true},
		{name: "foreign key view", method: "POST", path: "/$other/key", action: "token.key_view", params: `{"id":$other}`},
		{name: "rate limited key view", method: "POST", path: "/$id/key", action: "token.key_view", params: `{"id":$id}`, rateLimit: true},
		{name: "batch delete partial and duplicate", method: "POST", path: "/batch", body: `{"ids":[$id,$id,$other,999999]}`, action: "token.delete_batch", success: true, params: `{"requested_ids":[$id,$id,$other,999999],"total":4,"count":1}`},
		{name: "empty batch delete", method: "POST", path: "/batch", body: `{"ids":[]}`, action: "token.delete_batch", params: `{"requested_ids":[],"total":0}`},
		{name: "batch keys partial and duplicate", method: "POST", path: "/batch/keys", body: `{"ids":[$id,$id,$other,999999]}`, action: "token.key_view_batch", success: true, params: `{"requested_ids":[$id,$id,$other,999999],"total":4,"count":1,"returned_ids":[$id]}`},
		{name: "batch keys no matches", method: "POST", path: "/batch/keys", body: `{"ids":[$other,999999]}`, action: "token.key_view_batch", success: true, params: `{"requested_ids":[$other,999999],"total":2,"count":0,"returned_ids":[]}`},
		{name: "empty batch keys", method: "POST", path: "/batch/keys", body: `{"ids":[]}`, action: "token.key_view_batch", params: `{"requested_ids":[],"total":0}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			empty := ""
			owned := &model.Token{UserId: user.Id, Name: "owned", Key: fmt.Sprintf("owned-key-secret-%d", index), Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 100, UnlimitedQuota: true, Group: "auto", CrossGroupRetry: true, AutoGroups: `["default"]`, AllowIps: &empty}
			if tc.initialStatus != 0 {
				owned.Status = tc.initialStatus
			}
			if owned.Status == common.TokenStatusExpired || tc.expired {
				owned.ExpiredTime = 1
			}
			if owned.Status == common.TokenStatusExhausted {
				owned.RemainQuota, owned.UnlimitedQuota = 0, false
			}
			foreign := &model.Token{UserId: other.Id, Name: "private-foreign-name", Key: fmt.Sprintf("foreign-key-secret-%d", index)}
			require.NoError(t, model.DB.Create(owned).Error)
			require.NoError(t, model.DB.Create(foreign).Error)
			replace := strings.NewReplacer("$id", strconv.Itoa(owned.Id), "$other", strconv.Itoa(foreign.Id))
			if tc.failWrite {
				fail := func(tx *gorm.DB) {
					if tx.Statement.Table == "tokens" {
						_ = tx.AddError(errors.New("raw-storage-error-secret"))
					}
				}
				if tc.method == "POST" {
					require.NoError(t, model.DB.Callback().Create().Before("gorm:create").Register("token-audit:fail", fail))
					t.Cleanup(func() { require.NoError(t, model.DB.Callback().Create().Remove("token-audit:fail")) })
				} else {
					require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register("token-audit:fail", fail))
					t.Cleanup(func() { require.NoError(t, model.DB.Callback().Update().Remove("token-audit:fail")) })
				}
			}
			request := httptest.NewRequest(tc.method, "/api/token"+replace.Replace(tc.path), strings.NewReader(replace.Replace(tc.body)))
			credential := jwt
			if tc.usePAT {
				credential = pat
			}
			request.Header.Set("Authorization", "Bearer "+credential)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("User-Agent", "api-token-audit-client")
			request.RemoteAddr = "192.0.2.12:4321"
			if tc.rateLimit {
				request.Header.Set("X-Test-Limit", "1")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if strings.Contains(tc.name, "exceeds audit buffer") {
				assert.Greater(t, response.Body.Len(), 64*1024)
			}
			var events []model.AuditLog
			require.NoError(t, model.AuditLogQuery().Where("request_id = ?", response.Header().Get(common.RequestIdKey)).Find(&events).Error)
			expectedEvents := 1
			if tc.usePAT {
				expectedEvents = 2
			}
			require.Len(t, events, expectedEvents)
			var operation *model.AuditLog
			for i := range events {
				if events[i].Category == model.AuditCategorySecurity {
					require.Nil(t, operation, "one operation event per request")
					operation = &events[i]
				} else {
					assert.Equal(t, model.AuditCategoryAccessToken, events[i].Category)
					assert.Equal(t, model.AccessTokenFingerprint(pat), events[i].TokenRef)
				}
			}
			require.NotNil(t, operation)
			assert.Equal(t, tc.action, operation.Action)
			assert.Equal(t, tc.success, operation.Success)
			assert.Equal(t, response.Code, operation.Status)
			if tc.rateLimit {
				assert.Equal(t, 429, response.Code)
			} else {
				assert.Equal(t, 200, response.Code)
				assert.Equal(t, tc.success, decodeAPIResponse(t, response).Success)
			}
			assert.Equal(t, user.Id, operation.UserId)
			assert.Equal(t, user.Username, operation.Username)
			assert.Equal(t, common.RoleCommonUser, operation.ActorRole)
			assert.Equal(t, "192.0.2.12", operation.Ip)
			assert.Equal(t, "api-token-audit-client", operation.UserAgent)
			assert.Equal(t, tc.method, operation.Method)
			expectedRoute := strings.NewReplacer("$id", ":id", "$other", ":id", "999999", ":id").Replace(strings.Split(tc.path, "?")[0])
			assert.Equal(t, "/api/token"+expectedRoute, operation.Route)
			assert.NotEmpty(t, operation.RequestId)
			assert.Empty(t, operation.TokenRef)
			assert.Nil(t, operation.Other.AdminInfo)
			authMethod := "session"
			if tc.usePAT {
				authMethod = "access_token"
			}
			assert.Equal(t, authMethod, operation.AuthMethod)
			require.NotNil(t, operation.Other.Op)
			assert.Equal(t, tc.action, operation.Other.Op.Action)
			params, err := common.Marshal(operation.Other.Op.Params)
			require.NoError(t, err)
			if tc.name == "create" {
				var created model.Token
				require.NoError(t, model.DB.Where("user_id = ? AND name = ?", user.Id, "created").First(&created).Error)
				assert.JSONEq(t, fmt.Sprintf(`{"id":%d,"name":"created"}`, created.Id), string(params))
				assert.NotContains(t, string(params), created.Key)
			} else if tc.params == `{}` {
				assert.Empty(t, operation.Other.Op.Params)
			} else {
				assert.JSONEq(t, replace.Replace(tc.params), string(params))
			}
			encoded, err := common.Marshal(events)
			require.NoError(t, err)
			for _, secret := range []string{pat, jwt, owned.Key, foreign.Key, foreign.Name, "raw-body-secret", "raw-storage-error-secret", "private-model-configuration", "203.0.113.57", "Authorization"} {
				assert.NotContains(t, string(encoded), secret)
			}
			if tc.action == "token.key_view" && tc.success {
				assert.Contains(t, response.Body.String(), owned.GetFullKey())
			}
		})
	}

	t.Run("bounded batch metadata", func(t *testing.T) {
		ids := make([]int, 101)
		for i := range ids {
			ids[i] = 10000 + i
		}
		body, err := common.Marshal(TokenBatch{Ids: ids})
		require.NoError(t, err)
		for _, path := range []string{"/api/token/batch", "/api/token/batch/keys"} {
			request := httptest.NewRequest("POST", path, bytes.NewReader(body))
			request.Header.Set("Authorization", "Bearer "+jwt)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			var event model.AuditLog
			require.NoError(t, model.AuditLogQuery().Where("request_id = ?", response.Header().Get(common.RequestIdKey)).First(&event).Error)
			var params struct {
				IDs       []int `json:"requested_ids"`
				Truncated bool  `json:"requested_ids_truncated"`
				Total     int   `json:"total"`
				Count     *int  `json:"count"`
			}
			encoded, err := common.Marshal(event.Other.Op.Params)
			require.NoError(t, err)
			require.NoError(t, common.Unmarshal(encoded, &params))
			assert.Equal(t, ids[:100], params.IDs)
			assert.True(t, params.Truncated)
			assert.Equal(t, 101, params.Total)
			assert.Equal(t, path == "/api/token/batch", event.Success)
			if event.Success {
				require.NotNil(t, params.Count)
				assert.Zero(t, *params.Count)
			} else {
				assert.Nil(t, params.Count)
			}
		}
	})

	t.Run("reads and unauthenticated writes add no operation audit", func(t *testing.T) {
		for _, tc := range []struct{ method, path, credential string }{
			{"GET", "/api/token/", jwt}, {"GET", "/api/token/search?keyword=private-search", jwt},
			{"GET", "/api/token/999999", jwt}, {"POST", "/api/token/", ""},
		} {
			response := auditRequest(router, tc.method, tc.path, tc.credential)
			var count int64
			require.NoError(t, model.AuditLogQuery().Where("request_id = ?", response.Header().Get(common.RequestIdKey)).Count(&count).Error)
			assert.Zero(t, count)
		}
	})

	t.Run("self audit excludes other owners", func(t *testing.T) {
		model.RecordAuditLog(nil, model.AuditLog{UserId: other.Id, Username: other.Username, ActorRole: common.RoleCommonUser, Category: model.AuditCategorySecurity, Action: "token.delete", Content: "other-user-audit", Success: true})
		response := auditRequest(router, "GET", "/api/audit/self?category=security&page_size=100", jwt)
		assert.Equal(t, 200, response.Code)
		assert.Contains(t, response.Body.String(), "token.create")
		assert.NotContains(t, response.Body.String(), "other-user-audit")
	})

	t.Run("audit storage failure preserves operation result", func(t *testing.T) {
		require.NoError(t, model.LOG_DB.Callback().Create().Before("gorm:create").Register("token-audit:log-fail", func(tx *gorm.DB) {
			if tx.Statement.Table == "audit_logs" {
				_ = tx.AddError(errors.New("audit unavailable"))
			}
		}))
		t.Cleanup(func() { require.NoError(t, model.LOG_DB.Callback().Create().Remove("token-audit:log-fail")) })
		request := httptest.NewRequest("POST", "/api/token/", strings.NewReader(`{"name":"audit-down","expired_time":-1}`))
		request.Header.Set("Authorization", "Bearer "+jwt)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		require.True(t, decodeAPIResponse(t, response).Success)
		var count int64
		require.NoError(t, model.DB.Model(&model.Token{}).Where("name = ?", "audit-down").Count(&count).Error)
		assert.EqualValues(t, 1, count)
	})
}
