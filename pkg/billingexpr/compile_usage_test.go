package billingexpr_test

import (
	"testing"

	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompiledExpressionRunsValidationWithoutTheRuntimeCache(t *testing.T) {
	expression, err := billingexpr.Compile(`tier("base", p * 2 + c * 8)`)
	require.NoError(t, err)
	value, trace, err := expression.Run(billingexpr.TokenParams{P: 100, C: 10}, billingexpr.RequestInput{})
	require.NoError(t, err)
	assert.Equal(t, float64(280), value)
	assert.Equal(t, "base", trace.MatchedTier)
	assert.Empty(t, expression.UsageKeys())
	assert.False(t, expression.UsesFixedPricing())
	free, err := billingexpr.Compile(`tier("free", fixed(0))`)
	require.NoError(t, err)
	assert.True(t, free.UsesFixedPricing())
	_, err = billingexpr.Compile(`tier("invalid", fixed(-1))`)
	require.Error(t, err)
}

func TestUsedUsageKeysExtractsLiteralCallsAndSkipsDynamicArguments(t *testing.T) {
	expression := `tier("base", u(" seconds ") * 0.4 + (u("clips") > 0 ? u("clips") * 0.1 : 0)) + (u(header("usage-key")) == nil ? 0 : 0)`

	keys := billingexpr.UsedUsageKeys(expression)

	assert.Equal(t, map[string]bool{"seconds": true, "clips": true}, keys)
}

func TestUsedUsageKeysReturnsNilForEmptyOrInvalidExpressions(t *testing.T) {
	assert.Nil(t, billingexpr.UsedUsageKeys(""))
	assert.Nil(t, billingexpr.UsedUsageKeys(`tier("broken",`))
}
