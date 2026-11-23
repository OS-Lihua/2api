package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokenCacheDoesNotPublishRetiredRoutingFields(t *testing.T) {
	useUserCacheMiniRedis(t)
	token := Token{Id: 42, UserId: 7, Key: "token-auto-groups-cache-key", Name: "auto-cache"}
	require.NoError(t, cacheSetTokenForTest(token))
	key := getTokenCacheKey(token.Key)
	for _, field := range []string{"Group", "CrossGroupRetry", "AutoGroups"} {
		exists, err := common.RDB.HExists(t.Context(), key, field).Result()
		require.NoError(t, err)
		assert.False(t, exists, field)
	}
	// Extra fields do not affect the accepted current credential projection.
	require.NoError(t, common.RDB.HSet(t.Context(), key, "Group", "vip", "CrossGroupRetry", "true", "AutoGroups", `["vip","default"]`).Err())
	cached, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, token.UserId, cached.UserId)
	assert.Equal(t, token.Id, cached.Id)
}

func TestTokenCachePublishesAccessTimeWithoutRetiredQuotaFields(t *testing.T) {
	server := useUserCacheMiniRedis(t)
	token := Token{Id: 43, UserId: 7, Key: "token-account-cache-key", Name: "account-cache", AccessedTime: 1234}

	initialized, err := cacheInitToken(token)
	require.NoError(t, err)
	require.Equal(t, 1, initialized)
	cached, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, token.Id, cached.Id)
	assert.Equal(t, token.UserId, cached.UserId)
	assert.Equal(t, token.AccessedTime, cached.AccessedTime)
	key := getTokenCacheKey(token.Key)
	assertTokenCacheQuotaFieldsAbsent(t, key)
	assert.Positive(t, server.TTL(key))
}

func TestTokenUpdateSynchronouslyInvalidatesPreheatedCredentialCache(t *testing.T) {
	truncateTables(t)
	useUserCacheMiniRedis(t)
	token := Token{
		UserId:      7,
		Key:         "token-auto-groups-update-cache-key",
		Name:        "auto-cache-update",
		Status:      common.TokenStatusEnabled,
		ExpiredTime: -1,
	}
	require.NoError(t, token.Insert())
	require.NoError(t, cacheSetTokenForTest(token))

	preheated, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, common.TokenStatusEnabled, preheated.Status)

	token.AllowIps = common.GetPointer("203.0.113.0/24")
	token.Status = common.TokenStatusDisabled
	require.NoError(t, token.Update())
	// Invalidate the pre-update snapshot before storing narrower credential
	// restrictions.
	_, cacheErr := cacheGetTokenByKey(token.Key)
	require.Error(t, cacheErr, "the pre-update cache entry must be invalidated")
	reloaded, err := GetTokenByKey(token.Key, false)
	require.NoError(t, err)
	assert.Equal(t, common.TokenStatusDisabled, reloaded.Status)
	require.NotNil(t, reloaded.AllowIps)
	assert.Equal(t, "203.0.113.0/24", *reloaded.AllowIps)
	_, err = ValidateWalletToken(token.Key)
	assert.ErrorIs(t, err, ErrTokenInvalid)

	// A delayed reader must not restore an enabled snapshot through the fence.
	initialized, err := cacheInitToken(*preheated)
	require.NoError(t, err)
	assert.Zero(t, initialized)
	credential, err := GetTokenCredential(token.Key)
	require.NoError(t, err)
	assert.Equal(t, common.TokenStatusDisabled, credential.Status)
	require.NotNil(t, credential.AllowIps)
	assert.Equal(t, "203.0.113.0/24", *credential.AllowIps)
}

// cacheSetTokenForTest 以测试身份写入完整的已签名凭证缓存。
func cacheSetTokenForTest(token Token) error {
	_, err := cacheInitToken(token)
	return err
}
