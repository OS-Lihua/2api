package middleware

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetupContextForTokenDoesNotPublishLegacyRoutingOrQuota(t *testing.T) {
	for _, groups := range []struct{ name, stored string }{
		{"ordered", `["vip","default"]`},
		{"empty", `[]`},
		{"malformed", `not-json`},
	} {
		t.Run(groups.name, func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			var token model.Token
			payload, err := common.Marshal(map[string]any{
				"id": 1, "user_id": 2, "group": "auto", "cross_group_retry": true, "auto_groups": groups.stored,
				"remain_quota": 12, "unlimited_quota": false,
			})
			require.NoError(t, err)
			require.NoError(t, common.Unmarshal(payload, &token))
			require.NoError(t, SetupContextForToken(ctx, &token))
			assert.Equal(t, token.UserId, ctx.GetInt("id"))
			assert.Equal(t, token.Id, ctx.GetInt("token_id"))
			for _, key := range []string{"token_group", "token_cross_group_retry", "token_auto_groups", "token_unlimited_quota", "token_quota"} {
				_, present := ctx.Get(key)
				assert.False(t, present, key)
			}
		})
	}
}
