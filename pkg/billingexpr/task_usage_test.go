package billingexpr

import (
	"testing"

	"encoding/json"

	"github.com/QuantumNous/new-api/pkg/money"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTaskMoneyExpressionUsesCanonicalUnits(t *testing.T) {
	constant, _, err := RunMoneyExpr(`tier("job", 0.1)`, MoneyInput{TaskUsage: true}, "USD")
	require.NoError(t, err)
	assert.Equal(t, "0.1 USD", constant.String())
	_, _, err = RunMoneyExpr(`tier("job", fixed(0.1))`, MoneyInput{TaskUsage: true}, "USD")
	require.Error(t, err)
	const expression = `u("resolution") == "1080p" && u("audio") ? tier("video", u("seconds") * 0.123456789123456789) : tier("silent", u("seconds") * 0.2)`
	for _, tc := range []struct {
		name    string
		seconds any
		audio   bool
		want    string
	}{
		{"decimal fact", json.Number("1.25"), true, "0.15432098640432098625 USD"},
		{"explicit zero", 0.0, true, "0 USD"},
		{"boolean condition", int64(2), false, "0.4 USD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			amount, _, err := RunMoneyExpr(expression, MoneyInput{Request: RequestInput{Usage: map[string]any{"seconds": tc.seconds, "resolution": "1080p", "audio": tc.audio}}}, "USD")
			require.NoError(t, err)
			assert.Equal(t, tc.want, amount.String())
		})
	}
	_, _, err = RunMoneyExpr(`tier("video", u("seconds") * 0.4)`, MoneyInput{Request: RequestInput{Usage: map[string]any{"seconds": -1}}}, "USD")
	require.Error(t, err)
	_, _, err = RunMoneyExpr(`tier("video", u("seconds") * 0.4)`, MoneyInput{Request: RequestInput{Usage: map[string]any{"seconds": json.Number("99999999999999999999")}}}, "USD")
	require.ErrorIs(t, err, money.ErrOverflow)
}

func TestTaskUsageExpressionUsesFactsAndTaskQuotaConversion(t *testing.T) {
	expression := `tier("1080p", u("seconds") * (u("resolution") == "1080p" ? 0.4 : 0.2))`
	cost, trace, err := RunExprWithRequest(expression, TokenParams{}, RequestInput{Usage: map[string]any{"seconds": 10.0, "resolution": "1080p"}})
	require.NoError(t, err)
	assert.Equal(t, 4.0, cost)
	assert.Equal(t, "1080p", trace.MatchedTier)
	result, err := ComputeTieredQuotaWithRequest(&BillingSnapshot{ExprString: expression, ExprHash: ExprHashString(expression), GroupRatio: 2, QuotaPerUnit: 500000, ExprVersion: 1, TaskUsageBilling: true}, TokenParams{}, RequestInput{Usage: map[string]any{"seconds": 10.0, "resolution": "1080p"}})
	require.NoError(t, err)
	assert.Equal(t, 4_000_000, result.ActualQuotaAfterGroup)
}
