package controller

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type historicalTokenQuota struct {
	RemainQuota         int
	UsedQuota           int
	UnlimitedQuota      bool
	AllowUserQuotaQuery bool
}

func seedHistoricalTokenQuota(t *testing.T, db *gorm.DB, id int, quota historicalTokenQuota) {
	t.Helper()
	require.NoError(t, db.Exec(`ALTER TABLE tokens ADD COLUMN remain_quota bigint,
		ADD COLUMN used_quota bigint, ADD COLUMN unlimited_quota boolean,
		ADD COLUMN allow_user_quota_query boolean`).Error)
	result := db.Exec(`UPDATE tokens SET remain_quota = ?, used_quota = ?, unlimited_quota = ?, allow_user_quota_query = ? WHERE id = ?`,
		quota.RemainQuota, quota.UsedQuota, quota.UnlimitedQuota, quota.AllowUserQuotaQuery, id)
	require.NoError(t, result.Error)
	require.EqualValues(t, 1, result.RowsAffected)
}

func assertHistoricalTokenQuota(t *testing.T, db *gorm.DB, id int, want historicalTokenQuota) {
	t.Helper()
	var got historicalTokenQuota
	require.NoError(t, db.Raw(`SELECT remain_quota, used_quota, unlimited_quota, allow_user_quota_query FROM tokens WHERE id = ?`, id).
		Row().Scan(&got.RemainQuota, &got.UsedQuota, &got.UnlimitedQuota, &got.AllowUserQuotaQuery))
	assert.Equal(t, want, got, "credential operations must not alter historical quota data")
}

func TestTokenManagementRejectsRetiredFields(t *testing.T) {
	db := setupTokenControllerTestDB(t)
	token := seedToken(t, db, 1, "historical-token", "historical-token-test-key")
	assertTokenQuotaColumnsAbsent(t, db)

	for _, field := range []struct {
		name  string
		value any
	}{
		{"remain_quota", 500},
		{"unlimited_quota", true},
		{"used_quota", 0},
		{"allow_user_quota_query", false},
		{"model_limits_enabled", false},
		{"model_limits", "replacement-model"},
		{"group", "default"},
		{"auto_groups", []string{"default"}},
		{"auto_groups", nil},
		{"auto_groups", []string{}},
		{"cross_group_retry", false},
	} {
		for _, operation := range []string{"create", "update", "status"} {
			t.Run(operation+"/"+field.name, func(t *testing.T) {
				method, target := http.MethodPost, "/api/token/"
				body := map[string]any{"name": "replacement-name", "expired_time": -1, "allow_ips": "203.0.113.0/24"}
				if operation != "create" {
					method = http.MethodPut
					body["id"] = token.Id
				}
				if operation == "status" {
					target += "?status_only=true"
					body = map[string]any{"id": token.Id, "status": common.TokenStatusDisabled}
				}
				body[field.name] = field.value
				ctx, recorder := newAuthenticatedContext(t, method, target, body, token.UserId)
				if operation == "create" {
					AddToken(ctx)
				} else {
					UpdateToken(ctx)
				}
				assert.False(t, decodeAPIResponse(t, recorder).Success, "retired field %s must be rejected", field.name)
				var count int64
				require.NoError(t, db.Model(&model.Token{}).Count(&count).Error)
				assert.EqualValues(t, 1, count, "rejected requests must not create credentials")
				saved, err := model.GetTokenByIds(token.Id, token.UserId)
				require.NoError(t, err)
				assert.Equal(t, *token, *saved, "rejected requests must preserve the credential")
			})
		}
	}
}

func TestTokenUpdatePreservesHistoricalFields(t *testing.T) {
	db := setupTokenControllerTestDB(t)
	token := seedToken(t, db, 1, "historical-token", "historical-token-test-key")
	quota := historicalTokenQuota{RemainQuota: 73, UsedQuota: 27, AllowUserQuotaQuery: true}
	seedHistoricalTokenQuota(t, db, token.Id, quota)

	update := *token
	update.Name = "updated-credential"
	update.ExpiredTime = 4102444800
	update.AllowIps = common.GetPointer("203.0.113.0/24")
	update.Status = common.TokenStatusDisabled
	update.AccessedTime = 500 // Not an editable credential setting.
	require.NoError(t, update.Update())

	saved, err := model.GetTokenByIds(token.Id, token.UserId)
	require.NoError(t, err)
	token.Name, token.ExpiredTime, token.AllowIps, token.Status = update.Name, update.ExpiredTime, update.AllowIps, update.Status
	assert.Equal(t, *token, *saved, "credential updates must preserve identity and access history")
	assertHistoricalTokenQuota(t, db, token.Id, quota)
}
