package model

import (
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// historicalTokenCacheMAC freezes the pre-retirement JSON field order and the
// mutable fields that did not participate in the old signature.
func historicalTokenCacheMAC(t *testing.T, token Token, autoGroups string, legacyLimits bool) string {
	t.Helper()
	payload := struct {
		ID                  int     `json:"id"`
		UserID              int     `json:"user_id"`
		Key                 string  `json:"key"`
		Status              int     `json:"status"`
		Name                string  `json:"name"`
		CreatedTime         int64   `json:"created_time"`
		AccessedTime        int64   `json:"accessed_time"`
		ExpiredTime         int64   `json:"expired_time"`
		RemainQuota         int     `json:"remain_quota"`
		UnlimitedQuota      bool    `json:"unlimited_quota"`
		ModelLimitsEnabled  bool    `json:"model_limits_enabled,omitempty"`
		ModelLimits         string  `json:"model_limits,omitempty"`
		AllowIPs            *string `json:"allow_ips"`
		UsedQuota           int     `json:"used_quota"`
		Group               string  `json:"group,omitempty"`
		CrossGroupRetry     bool    `json:"cross_group_retry,omitempty"`
		DeletedAt           gorm.DeletedAt
		AllowUserQuotaQuery bool `json:"allow_user_quota_query"`
	}{
		ID: token.Id, UserID: token.UserId, Status: token.Status, Name: token.Name,
		CreatedTime: token.CreatedTime, ExpiredTime: token.ExpiredTime, AllowIPs: token.AllowIps,
		DeletedAt: token.DeletedAt, AllowUserQuotaQuery: true,
	}
	if payload.AllowIPs != nil && *payload.AllowIPs == "" {
		payload.AllowIPs = nil
	}
	if autoGroups != "" {
		payload.Group, payload.CrossGroupRetry = "auto", true
	}
	if legacyLimits {
		payload.ModelLimitsEnabled, payload.ModelLimits = true, "allowed-model"
	}
	raw, err := common.Marshal(payload)
	require.NoError(t, err)
	if autoGroups != "" {
		mac, err := authCacheMAC(getTokenCacheKey(token.Key), struct {
			Token          json.RawMessage
			AutoGroups     string
			CacheExpiresAt int64
		}{raw, autoGroups, token.CacheExpiresAt})
		require.NoError(t, err)
		return mac
	}
	mac, err := authCacheMAC(getTokenCacheKey(token.Key), struct {
		Token          json.RawMessage
		CacheExpiresAt int64
	}{raw, token.CacheExpiresAt})
	require.NoError(t, err)
	return mac
}

func assertTokenCacheQuotaFieldsAbsent(t *testing.T, key string) {
	t.Helper()
	for _, field := range []string{"RemainQuota", "UsedQuota", "UnlimitedQuota", "AllowUserQuotaQuery"} {
		exists, err := common.RDB.HExists(t.Context(), key, field).Result()
		require.NoError(t, err)
		assert.False(t, exists, field)
	}
}

func TestAuthCacheIntegrityDatabase(t *testing.T) {
	db := UseTestDatabases(t)
	require.NoError(t, db.AutoMigrate(&User{}, &Token{}, &UserSession{}))
	useUserCacheMiniRedis(t)

	user := User{Username: "cache-integrity-user", AffCode: "cache-int", Role: common.RoleCommonUser,
		Status: common.UserStatusEnabled, AuthVersion: 1, Group: "default", Quota: 100}
	require.NoError(t, DB.Create(&user).Error)
	t.Cleanup(func() { assert.NoError(t, db.Unscoped().Delete(&user).Error) })
	require.NoError(t, populateUserCache(user))
	userCacheKey := getUserCacheKey(user.Id)
	assertNoUserQuota := func(t *testing.T) {
		t.Helper()
		exists, err := common.RDB.HExists(t.Context(), userCacheKey, "Quota").Result()
		require.NoError(t, err)
		assert.False(t, exists, "user identity caching must not publish retired quota")
	}
	assertNoUserQuota(t)

	t.Run("user_metadata_and_partial_refresh", func(t *testing.T) {
		for _, fields := range []map[string]any{
			{"Role": common.RoleRootUser},
			{"AuthVersion": 99, "Status": common.UserStatusDisabled},
			{"Group": "admin", "Setting": `{"language":"evil"}`},
		} {
			fields["Quota"] = 30 // Historical or stale monetary data has no authority.
			require.NoError(t, common.RDB.HSet(t.Context(), userCacheKey, fields).Err())
			_, err := cacheGetUserBase(user.Id)
			require.Error(t, err)
			// The version fence intentionally rejects snapshots below a cached version.
			if _, ok := fields["AuthVersion"]; ok {
				require.NoError(t, common.RDB.HSet(t.Context(), getUserCacheKey(user.Id), "AuthVersion", user.AuthVersion).Err())
			}
			loaded, err := GetUserCache(user.Id)
			require.NoError(t, err)
			assert.Equal(t, user.Role, loaded.Role)
			assert.Equal(t, user.Group, loaded.Group)
			assert.Equal(t, user.Status, loaded.Status)
			cached, err := cacheGetUserBase(user.Id)
			require.NoError(t, err)
			assert.Equal(t, user.Role, cached.Role)
			assert.Equal(t, user.Group, cached.Group)
			assert.Equal(t, user.Status, cached.Status)
			assertNoUserQuota(t)
		}
		require.NoError(t, common.RDB.HSet(t.Context(), userCacheKey, "Role", common.RoleRootUser, "Quota", 30).Err())
		require.NoError(t, DB.Model(&User{}).Where("id = ?", user.Id).Update("group", "paid").Error)
		require.NoError(t, RefreshUserGroupCache(user.Id))
		cached, err := cacheGetUserBase(user.Id)
		require.NoError(t, err)
		assert.Equal(t, common.RoleCommonUser, cached.Role)
		assert.Equal(t, "paid", cached.Group)
		assertNoUserQuota(t)
	})

	allowIPs := "192.168.0.0/16"
	dbAccessedAt := common.GetTimestamp() - 60
	cachedAccessedAt := dbAccessedAt + 30
	token := Token{UserId: user.Id, Key: "cache-integrity-token", Status: common.TokenStatusEnabled,
		ExpiredTime: -1, AccessedTime: dbAccessedAt, AllowIps: &allowIPs}
	require.NoError(t, DB.Create(&token).Error)
	t.Cleanup(func() { assert.NoError(t, db.Unscoped().Delete(&token).Error) })
	_, err := GetTokenByKey(token.Key, false)
	require.NoError(t, err)
	require.NoError(t, common.RDB.HSet(t.Context(), getTokenCacheKey(token.Key), "AccessedTime", cachedAccessedAt).Err())

	t.Run("token_identity_and_permissions", func(t *testing.T) {
		for _, fields := range []map[string]any{
			{"UserId": user.Id + 1},
			{"Name": "tampered credential name"},
			{"AllowIps": ""},
			{"Status": common.TokenStatusDisabled, "ExpiredTime": 1},
			{"CreatedTime": "garbage"},
			{"DeletedAt": time.Now().Format(time.RFC3339)},
		} {
			require.NoError(t, common.RDB.HSet(t.Context(), getTokenCacheKey(token.Key), fields).Err())
			_, err := cacheGetTokenByKey(token.Key)
			require.Error(t, err)
			_, err = GetTokenByKey(token.Key, false)
			require.NoError(t, err)
			cached, err := cacheGetTokenByKey(token.Key)
			require.NoError(t, err)
			assert.Equal(t, token.UserId, cached.UserId)
			assert.Equal(t, token.Name, cached.Name)
			assert.Equal(t, token.CreatedTime, cached.CreatedTime)
			assert.Equal(t, token.AllowIps, cached.AllowIps)
			assert.Equal(t, token.Status, cached.Status)
			assert.Equal(t, token.ExpiredTime, cached.ExpiredTime)
			assert.Equal(t, cachedAccessedAt, cached.AccessedTime)
			assertTokenCacheQuotaFieldsAbsent(t, getTokenCacheKey(token.Key))
		}
	})

	for _, tc := range []struct {
		name, autoGroups string
		legacyLimits     bool
	}{
		{name: "retired_key_quota_cache_format"},
		{name: "retired_key_routing_cache_format", autoGroups: `["default"]`},
		{name: "retired_key_routing_and_limits_cache_format", autoGroups: `["default"]`, legacyLimits: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cached, err := cacheGetTokenByKey(token.Key)
			require.NoError(t, err)
			key := getTokenCacheKey(token.Key)
			mac := historicalTokenCacheMAC(t, *cached, tc.autoGroups, tc.legacyLimits)
			require.NotEqual(t, cached.CacheMAC, mac)
			fields := map[string]any{"RemainQuota": -30, "UsedQuota": 70, "UnlimitedQuota": false,
				"AllowUserQuotaQuery": true, "CacheMAC": mac}
			if tc.autoGroups != "" {
				fields["Group"], fields["CrossGroupRetry"], fields["AutoGroups"] = "auto", "true", tc.autoGroups
			}
			if tc.legacyLimits {
				fields["ModelLimitsEnabled"], fields["ModelLimits"] = "true", "allowed-model"
			}
			require.NoError(t, common.RDB.HSet(t.Context(), key, fields).Err())
			_, err = cacheGetTokenByKey(token.Key)
			require.ErrorContains(t, err, "token cache integrity check failed")
			loaded, err := GetTokenByKey(token.Key, false)
			require.NoError(t, err)
			assert.Equal(t, token.UserId, loaded.UserId)
			repaired, err := cacheGetTokenByKey(token.Key)
			require.NoError(t, err)
			assert.Equal(t, cachedAccessedAt, repaired.AccessedTime)
			assertTokenCacheQuotaFieldsAbsent(t, key)
			for _, field := range []string{"ModelLimitsEnabled", "ModelLimits", "Group", "CrossGroupRetry", "AutoGroups"} {
				exists, err := common.RDB.HExists(t.Context(), key, field).Result()
				require.NoError(t, err)
				assert.False(t, exists, field)
			}
		})
	}

	session := newTestUserSession("cache-integrity-session", user.Id, time.Now().Unix())
	require.NoError(t, CreateUserSession(session))
	t.Cleanup(func() { assert.NoError(t, db.Delete(session).Error) })
	t.Run("session_owner_and_versions", func(t *testing.T) {
		require.NoError(t, common.RDB.HSet(t.Context(), userSessionCacheKey(session.SID),
			"UserID", user.Id+1, "Version", 2, "UserAuthVersion", 2, "ExpiresAt", session.ExpiresAt+3600).Err())
		_, err := getUserSessionCache(session.SID)
		require.Error(t, err)
		// A higher cached version is a deny fence for delayed active snapshots.
		require.NoError(t, common.RDB.HSet(t.Context(), userSessionCacheKey(session.SID), "Version", 1).Err())
		loaded, err := GetUserSessionCached(session.SID)
		require.NoError(t, err)
		assert.Equal(t, session.UserID, loaded.UserID)
		assert.Equal(t, session.UserAuthVersion, loaded.UserAuthVersion)
		assert.Equal(t, session.ExpiresAt, loaded.ExpiresAt)
	})

	t.Run("unsigned_and_expired_cache", func(t *testing.T) {
		for _, key := range []string{getUserCacheKey(user.Id), getTokenCacheKey(token.Key), userSessionCacheKey(session.SID)} {
			require.NoError(t, common.RDB.HDel(t.Context(), key, "CacheMAC", "CacheExpiresAt").Err())
		}
		_, err := GetUserCache(user.Id)
		require.NoError(t, err)
		_, err = GetTokenByKey(token.Key, false)
		require.NoError(t, err)
		_, err = GetUserSessionCached(session.SID)
		require.NoError(t, err)
		cachedUser, err := cacheGetUserBase(user.Id)
		require.NoError(t, err)
		cachedToken, err := cacheGetTokenByKey(token.Key)
		require.NoError(t, err)
		cachedSession, err := getUserSessionCache(session.SID)
		require.NoError(t, err)
		expired := time.Now().Add(-time.Second).UnixMilli()
		cachedUser.CacheExpiresAt = expired
		cachedToken.CacheExpiresAt = expired
		cachedSession.CacheExpiresAt = expired
		userMAC, err := cachedUser.cacheMAC()
		require.NoError(t, err)
		tokenMAC, err := cachedToken.cacheMAC()
		require.NoError(t, err)
		sessionMAC, err := authCacheMAC(userSessionCacheKey(session.SID), cachedSession)
		require.NoError(t, err)
		for key, mac := range map[string]string{getUserCacheKey(user.Id): userMAC, getTokenCacheKey(token.Key): tokenMAC, userSessionCacheKey(session.SID): sessionMAC} {
			require.NoError(t, common.RDB.HSet(t.Context(), key, "CacheExpiresAt", expired, "CacheMAC", mac).Err())
			require.NoError(t, common.RDB.Persist(t.Context(), key).Err())
		}
		_, err = cacheGetUserBase(user.Id)
		assert.Error(t, err)
		_, err = cacheGetTokenByKey(token.Key)
		assert.Error(t, err)
		_, err = getUserSessionCache(session.SID)
		assert.Error(t, err)
		_, err = GetUserCache(user.Id)
		require.NoError(t, err)
		_, err = GetTokenByKey(token.Key, false)
		require.NoError(t, err)
		_, err = GetUserSessionCached(session.SID)
		require.NoError(t, err)
	})

	t.Run("repair_preserves_concurrent_access_time", func(t *testing.T) {
		require.NoError(t, common.RDB.HSet(t.Context(), getTokenCacheKey(token.Key), "CacheMAC", "invalid").Err())
		snapshotRead := make(chan struct{})
		releaseRepair := make(chan struct{}, 1)
		defer close(releaseRepair)
		var intercepted atomic.Bool
		require.NoError(t, DB.Callback().Query().After("gorm:query").Register("auth_cache_access_time_race", func(tx *gorm.DB) {
			if tx.Statement.Table == "tokens" && intercepted.CompareAndSwap(false, true) {
				close(snapshotRead)
				<-releaseRepair
			}
		}))
		t.Cleanup(func() { assert.NoError(t, DB.Callback().Query().Remove("auth_cache_access_time_race")) })
		done := make(chan error, 1)
		go func() {
			_, err := GetTokenByKey(token.Key, false)
			done <- err
		}()
		<-snapshotRead
		newerAccessedAt := cachedAccessedAt + 10
		require.NoError(t, common.RDB.HSet(t.Context(), getTokenCacheKey(token.Key),
			"AccessedTime", newerAccessedAt, "RemainQuota", 20, "UsedQuota", 80).Err())
		releaseRepair <- struct{}{}
		require.NoError(t, <-done)
		cached, err := cacheGetTokenByKey(token.Key)
		require.NoError(t, err)
		assert.Equal(t, newerAccessedAt, cached.AccessedTime)
		assertTokenCacheQuotaFieldsAbsent(t, getTokenCacheKey(token.Key))
	})

	t.Run("cross_identity_copy_and_database_failure", func(t *testing.T) {
		for source, destination := range map[string]string{
			getUserCacheKey(user.Id):         getUserCacheKey(user.Id + 1000),
			getTokenCacheKey(token.Key):      getTokenCacheKey("unknown-token"),
			userSessionCacheKey(session.SID): userSessionCacheKey("unknown-session"),
		} {
			fields, err := common.RDB.HGetAll(t.Context(), source).Result()
			require.NoError(t, err)
			require.NoError(t, common.RDB.HSet(t.Context(), destination, fields).Err())
		}
		require.NoError(t, common.RDB.HSet(t.Context(), getUserCacheKey(user.Id+1000), "Id", user.Id+1000).Err())
		require.NoError(t, common.RDB.HSet(t.Context(), userSessionCacheKey("unknown-session"), "SID", "unknown-session").Err())
		_, err := GetUserCache(user.Id + 1000)
		assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
		_, err = GetTokenByKey("unknown-token", false)
		assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
		_, err = GetUserSessionCached("unknown-session")
		assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
		require.NoError(t, DB.Callback().Query().Before("gorm:query").Register("auth_cache_db_failure", func(tx *gorm.DB) {
			tx.AddError(errors.New("database unavailable"))
		}))
		defer func() { require.NoError(t, DB.Callback().Query().Remove("auth_cache_db_failure")) }()
		_, err = GetTokenByKey(token.Key, false)
		require.NoError(t, err, "valid token cache must not query the database")
		_, err = GetUserSessionCached(session.SID)
		require.NoError(t, err, "valid session cache must not query the database")
		require.NoError(t, common.RDB.HSet(t.Context(), getTokenCacheKey(token.Key), "CacheMAC", "invalid").Err())
		require.NoError(t, common.RDB.HSet(t.Context(), userSessionCacheKey(session.SID), "CacheMAC", "invalid").Err())
		_, err = GetTokenByKey(token.Key, false)
		assert.Error(t, err)
		_, err = GetUserSessionCached(session.SID)
		assert.Error(t, err)
	})

	t.Run("session_revocation_cannot_be_cleared", func(t *testing.T) {
		revoked, err := RevokeUserSession(user.Id, session.SID, "test")
		require.NoError(t, err)
		require.True(t, revoked)
		require.NoError(t, common.RDB.HSet(t.Context(), userSessionCacheKey(session.SID),
			"Status", UserSessionStatusActive, "RevokedAt", 0, "RevokedReason", "").Err())
		_, err = GetUserSessionCached(session.SID)
		assert.ErrorIs(t, err, ErrUserSessionInactive)
	})

	t.Run("retired_user_quota_and_unreadable_key_access_time", func(t *testing.T) {
		require.NoError(t, common.RDB.HSet(t.Context(), userCacheKey, "Quota", "garbage").Err())
		cached, err := cacheGetUserBase(user.Id)
		require.NoError(t, err, "retired fields cannot invalidate the current identity")
		assert.Equal(t, common.RoleCommonUser, cached.Role)
		require.NoError(t, common.RDB.HSet(t.Context(), userCacheKey, "CacheSchema", 2).Err())
		_, err = cacheGetUserBase(user.Id)
		require.Error(t, err, "superseded hashes must be refreshed from the primary")
		require.NoError(t, common.RDB.HSet(t.Context(), getTokenCacheKey(token.Key),
			"AccessedTime", "garbage", "RemainQuota", "garbage").Err())
		_, err = GetUserCache(user.Id)
		require.NoError(t, err)
		_, err = GetTokenByKey(token.Key, false)
		require.NoError(t, err)
		cachedUser, err := cacheGetUserBase(user.Id)
		require.NoError(t, err)
		assert.Equal(t, common.RoleCommonUser, cachedUser.Role)
		assert.Equal(t, "paid", cachedUser.Group)
		assertNoUserQuota(t)
		var saved User
		require.NoError(t, DB.First(&saved, user.Id).Error)
		assert.Equal(t, user.Quota, saved.Quota, "the offline source stays unchanged")
		cachedToken, err := cacheGetTokenByKey(token.Key)
		require.NoError(t, err)
		assert.Equal(t, dbAccessedAt, cachedToken.AccessedTime)
		assertTokenCacheQuotaFieldsAbsent(t, getTokenCacheKey(token.Key))
	})
}
