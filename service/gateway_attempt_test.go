package service_test

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/channelhealth"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelHealthSharedProbeAndAttemptIdempotency(t *testing.T) {
	fake := clock.UseFake(t, time.Unix(1000, 0))
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "redis"}[shared], func(t *testing.T) {
			var backend *redis.Client
			if shared {
				backend = client
			}
			first := channelhealth.New(backend)
			second := first
			if shared {
				second = channelhealth.New(backend)
			}
			ctx := t.Context()
			for i := range 5 {
				action, err := first.Record(ctx, channelhealth.Sample{AttemptID: fmt.Sprint(i), ChannelID: 1, ProviderFailure: true})
				require.NoError(t, err)
				if i == 4 {
					assert.Equal(t, "opened", action)
				}
			}
			states, err := second.Snapshots(ctx, []int{1})
			require.NoError(t, err)
			assert.Equal(t, clock.Now().Add(time.Minute).UnixMilli(), states[1].RetryAt)
			// Replaying the same actual Attempt changes neither samples nor cooldown.
			_, err = second.Record(ctx, channelhealth.Sample{AttemptID: "4", ChannelID: 1, ProviderFailure: true})
			require.NoError(t, err)
			admission, err := first.Acquire(ctx, []int{1, 2})
			require.NoError(t, err)
			assert.Equal(t, 2, admission.ChannelID)
			fake.Advance(time.Minute)
			probe, err := first.Acquire(ctx, []int{1, 2})
			require.NoError(t, err)
			require.Equal(t, 1, probe.ChannelID)
			require.NotEmpty(t, probe.Lease)
			require.NoError(t, first.Release(ctx, probe))
			type probeResult struct {
				admission channelhealth.Admission
				err       error
			}
			results := make(chan probeResult, 2)
			start := make(chan struct{})
			var workers sync.WaitGroup
			for _, node := range []*channelhealth.Manager{first, second} {
				workers.Go(func() { <-start; value, err := node.Acquire(ctx, []int{1}); results <- probeResult{value, err} })
			}
			close(start)
			workers.Wait()
			close(results)
			admitted := 0
			for result := range results {
				require.NoError(t, result.err)
				if result.admission.ChannelID > 0 {
					admitted++
					probe = result.admission
				}
			}
			require.Equal(t, 1, admitted, "two concurrent nodes share one half-open probe")
			blocked, err := second.Acquire(ctx, []int{1})
			require.NoError(t, err)
			assert.Zero(t, blocked.ChannelID)
			_, err = first.Record(ctx, channelhealth.Sample{AttemptID: "probe-failure", ChannelID: 1, ProviderFailure: true, Lease: probe.Lease})
			require.NoError(t, err)
			states, err = second.Snapshots(ctx, []int{1})
			require.NoError(t, err)
			assert.Equal(t, clock.Now().Add(2*time.Minute).UnixMilli(), states[1].RetryAt)
			fake.Advance(2 * time.Minute)
			probe, err = second.Acquire(ctx, []int{1})
			require.NoError(t, err)
			_, err = second.Record(ctx, channelhealth.Sample{AttemptID: "probe-success", ChannelID: 1, Success: true, FirstByte: 20 * time.Millisecond, Lease: probe.Lease})
			require.NoError(t, err)
			states, err = first.Snapshots(ctx, []int{1})
			require.NoError(t, err)
			assert.Zero(t, states[1].RetryAt)
			assert.Equal(t, 7, states[1].Samples)
			assert.Equal(t, int64(20), states[1].FirstByteMillis)
		})
	}
}

func TestChannelHealthRetryAfterAndEarliestLayerProbe(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprint(shared), func(t *testing.T) {
			fake := clock.UseFake(t, time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC))
			var client *redis.Client
			if shared {
				server := miniredis.RunT(t)
				client = redis.NewClient(&redis.Options{Addr: server.Addr()})
				t.Cleanup(func() { require.NoError(t, client.Close()) })
			}
			first := channelhealth.New(client)
			second := first
			if shared {
				second = channelhealth.New(client)
			}
			for _, tc := range []struct {
				id     int
				header string
				delay  time.Duration
			}{
				{1, "120", 2 * time.Minute}, {2, clock.Now().Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
				{3, "private-provider-secret", time.Minute}, {4, "999999999999999999999", time.Minute}, {5, "-1", time.Minute},
			} {
				_, err := first.Record(t.Context(), channelhealth.Sample{AttemptID: fmt.Sprint(tc.id), ChannelID: tc.id, ProviderFailure: true, RetryAfter: tc.header})
				require.NoError(t, err)
				states, err := second.Snapshots(t.Context(), []int{tc.id})
				require.NoError(t, err)
				assert.Equal(t, clock.Now().Add(tc.delay).UnixMilli(), states[tc.id].RetryAt)
			}
			// All circuits are open: the earliest wins regardless of caller ordering,
			// and a second node cannot claim the next circuit while it is in flight.
			probe, err := first.Acquire(t.Context(), []int{1, 2, 3})
			require.NoError(t, err)
			assert.Equal(t, 3, probe.ChannelID)
			blocked, err := second.Acquire(t.Context(), []int{2, 1, 3})
			require.NoError(t, err)
			assert.Zero(t, blocked.ChannelID)
			// Equal expiry times still choose a single stable Channel across nodes.
			tied, err := second.Acquire(t.Context(), []int{4, 3})
			require.NoError(t, err)
			assert.Zero(t, tied.ChannelID)
			require.NoError(t, first.Release(t.Context(), probe))
			for index, cooldown := range []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute} {
				probe, err = second.Acquire(t.Context(), []int{3})
				require.NoError(t, err)
				_, err = second.Record(t.Context(), channelhealth.Sample{AttemptID: fmt.Sprintf("backoff-%d", index), ChannelID: 3, ProviderFailure: true, Lease: probe.Lease})
				require.NoError(t, err)
				states, err := first.Snapshots(t.Context(), []int{3})
				require.NoError(t, err)
				assert.Equal(t, clock.Now().Add(cooldown).UnixMilli(), states[3].RetryAt)
				fake.Advance(cooldown)
			}
			_, err = first.Record(t.Context(), channelhealth.Sample{AttemptID: "cancelled", ChannelID: 6})
			require.NoError(t, err)
			states, err := second.Snapshots(t.Context(), []int{6})
			require.NoError(t, err)
			assert.Zero(t, states[6].Samples)
			assert.Zero(t, states[6].Failures)
		})
	}
}

func TestChannelHealthProbeLeaseSurvivesLongResponseAndExpiresAfterCrash(t *testing.T) {
	fake := clock.UseFake(t, time.Unix(100, 0))
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	first, second := channelhealth.New(client), channelhealth.New(client)
	_, err := first.Record(t.Context(), channelhealth.Sample{AttemptID: "429", ChannelID: 1, ProviderFailure: true, RetryAfter: "60"})
	require.NoError(t, err)
	probe, err := first.Acquire(t.Context(), []int{1})
	require.NoError(t, err)
	stop := first.KeepAlive(t.Context(), probe)
	fake.Advance(11 * time.Minute)
	blocked, err := second.Acquire(t.Context(), []int{1})
	require.NoError(t, err)
	assert.Zero(t, blocked.ChannelID)
	stop()
	fake.Advance(11 * time.Minute)
	replacement, err := second.Acquire(t.Context(), []int{1})
	require.NoError(t, err)
	assert.Equal(t, 1, replacement.ChannelID)
	assert.NotEqual(t, probe.Lease, replacement.Lease)
}

func TestChannelHealthLongRetryAfterStillRequiresSharedProbe(t *testing.T) {
	fake := clock.UseFake(t, time.Unix(100, 0))
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	first, second := channelhealth.New(client), channelhealth.New(client)
	_, err := first.Record(t.Context(), channelhealth.Sample{AttemptID: "long-429", ChannelID: 1, ProviderFailure: true, RetryAfter: "86400"})
	require.NoError(t, err)
	server.FastForward(48 * time.Hour)
	fake.Advance(48 * time.Hour)
	probe, err := first.Acquire(t.Context(), []int{1})
	require.NoError(t, err)
	require.NotEmpty(t, probe.Lease, "a long idle cooldown cannot turn an open circuit into unbounded normal admission")
	blocked, err := second.Acquire(t.Context(), []int{1})
	require.NoError(t, err)
	assert.Zero(t, blocked.ChannelID)
}

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
