package controller

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokenManagementRejectsRetiredFields(t *testing.T) {
	db := setupTokenControllerTestDB(t)
	token := seedToken(t, db, 1, "historical-token", "historical-token-test-key")
	token.RemainQuota = 73
	token.UsedQuota = 27
	token.UnlimitedQuota = false
	token.AllowUserQuotaQuery = true
	token.ModelLimitsEnabled = true
	token.ModelLimits = "historical-model"
	token.Group = "auto"
	token.AutoGroups = `["vip","default"]`
	token.CrossGroupRetry = true
	require.NoError(t, db.Save(token).Error)

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
				assert.Equal(t, *token, *saved, "rejected requests must preserve the credential and historical fields")
			})
		}
	}
}

func TestTokenUpdatePreservesHistoricalFields(t *testing.T) {
	db := setupTokenControllerTestDB(t)
	token := seedToken(t, db, 1, "historical-token", "historical-token-test-key")
	token.RemainQuota = 73
	token.UsedQuota = 27
	token.UnlimitedQuota = false
	token.AllowUserQuotaQuery = true
	token.ModelLimitsEnabled = true
	token.ModelLimits = "historical-model"
	token.Group = "auto"
	token.AutoGroups = `["vip","default"]`
	token.CrossGroupRetry = true
	require.NoError(t, db.Save(token).Error)

	update := *token
	update.Name = "updated-credential"
	update.ExpiredTime = 4102444800
	update.AllowIps = common.GetPointer("203.0.113.0/24")
	update.Status = common.TokenStatusDisabled
	update.RemainQuota = 500
	update.UnlimitedQuota = true
	update.AllowUserQuotaQuery = false
	update.ModelLimitsEnabled = false
	update.ModelLimits = "replacement-model"
	update.Group = "default"
	update.AutoGroups = ""
	update.CrossGroupRetry = false
	require.NoError(t, update.Update())

	saved, err := model.GetTokenByIds(token.Id, token.UserId)
	require.NoError(t, err)
	token.Name, token.ExpiredTime, token.AllowIps, token.Status = update.Name, update.ExpiredTime, update.AllowIps, update.Status
	assert.Equal(t, *token, *saved, "credential updates must preserve the historical migration and accounting fields")
}
