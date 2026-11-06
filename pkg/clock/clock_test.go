package clock_test

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNowDefaultsToSystemTime(t *testing.T) {
	before := time.Now()
	now := clock.Now()
	after := time.Now()

	assert.False(t, now.Before(before))
	assert.False(t, now.After(after))
}

// A cooldown that ends one minute after it starts, checked without waiting.
func TestFakeClockAdvancesPastCooldown(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	fake := clock.UseFake(t, start)
	cooldownEnd := clock.Now().Add(time.Minute)

	fake.Advance(59 * time.Second)
	assert.True(t, clock.Now().Before(cooldownEnd))

	fake.Advance(time.Second)
	assert.Equal(t, cooldownEnd, clock.Now())

	fake.Set(start)
	assert.Equal(t, start, clock.Now())
}

func TestUseFakeRestoresSystemTimeAfterTest(t *testing.T) {
	past := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	t.Run("fake", func(t *testing.T) {
		clock.UseFake(t, past)
		require.Equal(t, past, clock.Now())
	})

	assert.True(t, clock.Now().After(past.Add(24*time.Hour)))
}
