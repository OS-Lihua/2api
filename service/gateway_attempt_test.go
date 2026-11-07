package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGatewayKnownRejectionSurvivesBudgetCancellation(t *testing.T) {
	for _, test := range []struct {
		status           int
		code, class      string
		provider, change bool
	}{
		{401, "", "credentials", true, true}, {403, "", "credentials", true, true},
		{400, "", "invalid_request", false, false}, {413, "", "invalid_request", false, false}, {422, "", "invalid_request", false, false},
		{429, "insufficient_quota", "account_quota", true, true}, {404, "model_not_found", "model_not_found", false, true},
		{200, "content_filter", "content_safety", false, false}, {200, "context_length_exceeded", "context_length", false, false},
	} {
		t.Run(test.class, func(t *testing.T) {
			failure := service.ClassifyGatewayFailure(test.status, test.code, false, true, false, false)
			assert.Equal(t, test.class, failure.Class)
			assert.Equal(t, test.change, failure.Switch)
			assert.Equal(t, test.provider, failure.ExcludeProvider)
		})
	}
}

func TestGatewaySkippedCandidatesAndTiersShareTheSendBudget(t *testing.T) {
	fake := clock.UseFake(t, time.Unix(100, 0))
	calls := 0
	attempts := service.RunGatewayAttempts(t.Context(), service.GatewayAttemptPolicy{MaxAttempts: 5, TotalFirstByte: 35 * time.Second, AttemptFirstByte: 30 * time.Second}, func(b *service.GatewayAttemptBudget, e *service.GatewayExclusions) service.GatewayAttemptResult {
		calls++
		if calls == 1 {
			fake.Advance(time.Hour)
			return service.GatewayAttemptResult{SkippedChannel: 1}
		}
		assert.True(t, e.Channels[1])
		ctx, err := b.Start()
		require.NoError(t, err)
		if calls == 2 {
			fake.Advance(30 * time.Second)
		} else {
			fake.Advance(4 * time.Second)
			assert.NoError(t, ctx.Err(), "the exact five-second remainder allows another send")
			fake.Advance(time.Second)
		}
		assert.ErrorIs(t, context.Cause(ctx), service.ErrGatewayBudget)
		return service.GatewayAttemptResult{Attempt: &model.GatewayAttempt{ID: "attempt", ChannelID: calls, MemberTier: calls - 1}, Failure: service.GatewayFailure{Class: "first_byte_timeout", Switch: true}}
	})
	require.Len(t, attempts, 2)
	assert.Equal(t, 3, calls)
	assert.Equal(t, "first_byte_budget", attempts[1].FailoverReason)
	assert.Equal(t, 2, attempts[1].MemberTier)
}

func TestGatewayBudgetExcludesPreparationAndStopsAfterDelivery(t *testing.T) {
	fake := clock.UseFake(t, time.Unix(100, 0))
	calls := 0
	attempts := service.RunGatewayAttempts(t.Context(), service.GatewayAttemptPolicy{MaxAttempts: 3, TotalFirstByte: 90 * time.Second, AttemptFirstByte: 30 * time.Second}, func(b *service.GatewayAttemptBudget, e *service.GatewayExclusions) service.GatewayAttemptResult {
		calls++
		fake.Advance(2 * time.Minute) // Conversion and local validation consume no send budget.
		ctx, err := b.Start()
		require.NoError(t, err)
		fake.Advance(29 * time.Second)
		require.NoError(t, ctx.Err())
		if calls == 1 {
			fake.Advance(time.Second)
			assert.ErrorIs(t, context.Cause(ctx), service.ErrGatewayBudget)
			return service.GatewayAttemptResult{Attempt: &model.GatewayAttempt{ID: "failed", ChannelID: 1}, Failure: service.GatewayFailure{Class: "first_byte_timeout", Switch: true}}
		}
		b.Delivered()
		fake.Advance(5 * time.Minute)
		assert.NoError(t, ctx.Err(), "long output is governed by idle timeout after business delivery")
		return service.GatewayAttemptResult{Attempt: &model.GatewayAttempt{ID: "ok", ChannelID: 2}, Delivered: true}
	})
	require.Len(t, attempts, 2)
	assert.Equal(t, 2, calls)
}
