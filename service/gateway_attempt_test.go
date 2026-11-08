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

func TestChannelHealthRecentReplayAcrossNodesAndLegacyKeys(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	first, second := channelhealth.New(client), channelhealth.New(client)
	// This persisted key is the deduplication format used before bounded history.
	require.NoError(t, client.Set(t.Context(), "gateway:channel-health:attempt:legacy", "1", 7*24*time.Hour).Err())
	_, err := first.Record(t.Context(), channelhealth.Sample{AttemptID: "legacy", ChannelID: 1, ProviderFailure: true})
	require.NoError(t, err)
	states, err := first.Snapshots(t.Context(), []int{1})
	require.NoError(t, err)
	assert.Zero(t, states[1].Samples)

	sample := channelhealth.Sample{AttemptID: "recent", ChannelID: 1, ProviderFailure: true}
	var workers sync.WaitGroup
	results := make(chan error, 2)
	start := make(chan struct{})
	for _, node := range []*channelhealth.Manager{first, second} {
		workers.Go(func() {
			<-start
			_, err := node.Record(t.Context(), sample)
			results <- err
		})
	}
	close(start)
	workers.Wait()
	for range 2 {
		require.NoError(t, <-results)
	}
	states, err = second.Snapshots(t.Context(), []int{1})
	require.NoError(t, err)
	assert.Equal(t, 1, states[1].Samples)
	assert.Equal(t, 1, states[1].Failures)
	// An older node must still recognize a recently completed new-node Attempt.
	seen, err := client.Exists(t.Context(), "gateway:channel-health:attempt:recent").Result()
	require.NoError(t, err)
	assert.EqualValues(t, 1, seen)
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
				{8, "172800", 48 * time.Hour}, {9, clock.Now().Add(48 * time.Hour).Format(http.TimeFormat), 48 * time.Hour},
				{10, "+1", time.Minute}, {14, "-0", time.Minute},
				{11, "0", 0}, {12, clock.Now().Add(-time.Second).Format(http.TimeFormat), 0},
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
	for _, tc := range []struct {
		name    string
		start   int64
		header  string
		advance time.Duration
	}{
		{"long idle cooldown", 100, "86400", 48 * time.Hour}, {"zero delay at epoch", 0, "0", 0}, {"zero delay before epoch", -1, "0", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := clock.UseFake(t, time.Unix(tc.start, 0))
			server := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: server.Addr()})
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			for _, backend := range []*redis.Client{nil, client} {
				fake.Set(time.Unix(tc.start, 0))
				first := channelhealth.New(backend)
				second := first
				if backend != nil {
					second = channelhealth.New(backend)
				}
				_, err := first.Record(t.Context(), channelhealth.Sample{AttemptID: "long-429", ChannelID: 1, ProviderFailure: true, RateLimited: true, RetryAfter: tc.header})
				require.NoError(t, err)
				if backend != nil {
					server.FastForward(tc.advance)
				}
				fake.Advance(tc.advance)
				states, err := first.Snapshots(t.Context(), []int{1})
				require.NoError(t, err)
				assert.Equal(t, "open", states[1].Circuit)
				probe, err := first.Acquire(t.Context(), []int{1})
				require.NoError(t, err)
				require.NotEmpty(t, probe.Lease, "cooldown expiry still requires a shared recovery probe")
				blocked, err := second.Acquire(t.Context(), []int{1})
				require.NoError(t, err)
				assert.Zero(t, blocked.ChannelID)
			}
		})
	}
}

func TestChannelHealthInflightRateLimitExtendsCooldownAfterTrip(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, activeProbe := range []bool{false, true} {
			t.Run(fmt.Sprintf("redis=%t/probe=%t", shared, activeProbe), func(t *testing.T) {
				fake := clock.UseFake(t, time.Unix(1000, 0))
				var client *redis.Client
				if shared {
					server := miniredis.RunT(t)
					client = redis.NewClient(&redis.Options{Addr: server.Addr()})
					t.Cleanup(func() { require.NoError(t, client.Close()) })
				}
				manager := channelhealth.New(client)
				ordinary, err := manager.Acquire(t.Context(), []int{1})
				require.NoError(t, err)
				require.Empty(t, ordinary.Lease)
				_, err = manager.Record(t.Context(), channelhealth.Sample{AttemptID: "trip", ChannelID: 1, ProviderFailure: true, RateLimited: true, RetryAfter: "60"})
				require.NoError(t, err)
				wantCircuit := "open"
				if activeProbe {
					probe, err := manager.Acquire(t.Context(), []int{1})
					require.NoError(t, err)
					require.NotEmpty(t, probe.Lease)
					wantCircuit = "half_open"
				}
				fake.Advance(time.Second)
				_, err = manager.Record(t.Context(), channelhealth.Sample{AttemptID: "inflight-429", ChannelID: 1, ProviderFailure: true, RateLimited: true, RetryAfter: "172800", Lease: ordinary.Lease})
				require.NoError(t, err)
				states, err := manager.Snapshots(t.Context(), []int{1})
				require.NoError(t, err)
				assert.Equal(t, clock.Now().Add(48*time.Hour).UnixMilli(), states[1].RetryAt)
				assert.Equal(t, wantCircuit, states[1].Circuit)
				if activeProbe {
					blocked, err := manager.Acquire(t.Context(), []int{1})
					require.NoError(t, err)
					assert.Zero(t, blocked.ChannelID)
				}
			})
		}
	}
}

func TestChannelHealthAllOpenProbeRemainsExclusiveAfterRetryOrderChanges(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, release := range []bool{false, true} {
			t.Run(fmt.Sprintf("redis=%t/release=%t", shared, release), func(t *testing.T) {
				fake := clock.UseFake(t, time.Unix(1000, 0))
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
				ctx := t.Context()
				ordinary, err := first.Acquire(ctx, []int{1})
				require.NoError(t, err)
				require.Equal(t, channelhealth.Admission{ChannelID: 1}, ordinary)
				for _, sample := range []channelhealth.Sample{
					{AttemptID: "open-one", ChannelID: 1, ProviderFailure: true, RateLimited: true, RetryAfter: "60"},
					{AttemptID: "open-two", ChannelID: 2, ProviderFailure: true, RateLimited: true, RetryAfter: "120"},
				} {
					_, err = first.Record(ctx, sample)
					require.NoError(t, err)
				}
				probe, err := first.Acquire(ctx, []int{1, 2})
				require.NoError(t, err)
				require.Equal(t, 1, probe.ChannelID)
				require.NotEmpty(t, probe.Lease)
				_, err = second.Record(ctx, channelhealth.Sample{AttemptID: "late-ordinary", ChannelID: 1,
					ProviderFailure: true, RateLimited: true, RetryAfter: "172800", Lease: ordinary.Lease})
				require.NoError(t, err)
				states, err := second.Snapshots(ctx, []int{1, 2})
				require.NoError(t, err)
				assert.Equal(t, "half_open", states[1].Circuit)
				assert.Equal(t, clock.Now().Add(48*time.Hour).UnixMilli(), states[1].RetryAt)
				blocked, err := second.Acquire(ctx, []int{2, 1})
				require.NoError(t, err)
				assert.Equal(t, channelhealth.Admission{}, blocked, "Retry-After reordering must retain candidate-set probe exclusivity")
				blocked, err = second.AcquireWithProbeFence(ctx, []int{2}, []int{1, 2})
				require.NoError(t, err)
				assert.Equal(t, channelhealth.Admission{}, blocked, "quality selection must not hide a lease in the validated pool")
				normal, err := second.Acquire(ctx, []int{1, 2, 3})
				require.NoError(t, err)
				assert.Equal(t, channelhealth.Admission{ChannelID: 3}, normal, "closed candidates still serve traffic")
				normal, err = second.AcquireWithProbeFence(ctx, []int{3}, []int{1, 2, 3})
				require.NoError(t, err)
				assert.Equal(t, channelhealth.Admission{ChannelID: 3}, normal, "the full probe fence must not block closed traffic")
				if release {
					require.NoError(t, first.Release(ctx, probe))
				} else {
					fake.Advance(11 * time.Minute)
				}
				preferred, err := second.AcquireWithProbeFence(ctx, []int{1}, []int{1, 2})
				require.NoError(t, err)
				require.Equal(t, 1, preferred.ChannelID, "a lower-quality candidate cannot win by expiring earlier")
				require.NotEmpty(t, preferred.Lease)
				require.NoError(t, second.Release(ctx, preferred))
				replacement, err := second.Acquire(ctx, []int{1, 2})
				require.NoError(t, err)
				require.Equal(t, 2, replacement.ChannelID)
				require.NotEmpty(t, replacement.Lease)
				assert.NotEqual(t, probe.Lease, replacement.Lease)
				require.NoError(t, first.Release(ctx, probe))
				blocked, err = first.Acquire(ctx, []int{1, 2})
				require.NoError(t, err)
				assert.Equal(t, channelhealth.Admission{}, blocked, "stale release must not clear the replacement probe")
				unrelated, err := first.Acquire(ctx, []int{1})
				require.NoError(t, err)
				assert.Equal(t, 1, unrelated.ChannelID, "a lease outside the current candidate set must not block admission")
				assert.NotEmpty(t, unrelated.Lease)
			})
		}
	}
}

func TestChannelHealthStaleCompletionCannotChangeCurrentCircuit(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, tc := range []struct {
			name, retryAfter         string
			oldProbe, success, close bool
		}{
			{name: "success started before trip", success: true},
			{name: "superseded probe success", oldProbe: true, success: true},
			{name: "superseded probe failure", oldProbe: true},
			{name: "superseded probe rate limit", oldProbe: true, retryAfter: "172800"},
			{name: "superseded failure after recovery", oldProbe: true, close: true},
		} {
			t.Run(fmt.Sprintf("redis=%t/%s", shared, tc.name), func(t *testing.T) {
				fake := clock.UseFake(t, time.Unix(1000, 0))
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
				_, err := first.Record(t.Context(), channelhealth.Sample{AttemptID: "trip", ChannelID: 1, ProviderFailure: true, RateLimited: true})
				require.NoError(t, err)
				stale := channelhealth.Sample{AttemptID: "late", ChannelID: 1, Success: tc.success, ProviderFailure: !tc.success,
					RateLimited: tc.retryAfter != "", RetryAfter: tc.retryAfter}
				if tc.oldProbe {
					old, err := first.Acquire(t.Context(), []int{1})
					require.NoError(t, err)
					stale.Lease = old.Lease
					fake.Advance(24 * time.Hour)
				}
				current, err := second.Acquire(t.Context(), []int{1})
				require.NoError(t, err)
				require.NotEmpty(t, current.Lease)
				if tc.close {
					_, err = second.Record(t.Context(), channelhealth.Sample{AttemptID: "recovery", ChannelID: 1, Success: true, Lease: current.Lease})
					require.NoError(t, err)
				}
				before, err := second.Snapshots(t.Context(), []int{1})
				require.NoError(t, err)
				action, err := first.Record(t.Context(), stale)
				require.NoError(t, err)
				assert.Equal(t, "none", action)
				after, err := second.Snapshots(t.Context(), []int{1})
				require.NoError(t, err)
				assert.Equal(t, before[1].Circuit, after[1].Circuit)
				assert.Equal(t, before[1].RetryAt, after[1].RetryAt)
				assert.Equal(t, before[1].Failures, after[1].Failures)
				if !tc.close {
					blocked, err := first.Acquire(t.Context(), []int{1})
					require.NoError(t, err)
					assert.Zero(t, blocked.ChannelID, "the current probe still owns admission")
					_, err = second.Record(t.Context(), channelhealth.Sample{AttemptID: "recovery", ChannelID: 1, Success: true, Lease: current.Lease})
					require.NoError(t, err)
					closed, err := first.Snapshots(t.Context(), []int{1})
					require.NoError(t, err)
					assert.Equal(t, "closed", closed[1].Circuit)
				}
			})
		}
	}
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

func TestGatewayNativeTransportBudget(t *testing.T) {
	t.Run("handshakes consume the shared maximum", func(t *testing.T) {
		clock.UseFake(t, time.Unix(100, 0))
		calls := 0
		attempts := service.RunGatewayAttempts(t.Context(), service.GatewayAttemptPolicy{MaxAttempts: 3, TotalFirstByte: 90 * time.Second, AttemptFirstByte: 30 * time.Second}, func(b *service.GatewayAttemptBudget, e *service.GatewayExclusions) service.GatewayAttemptResult {
			calls++
			_, err := b.Start()
			require.NoError(t, err)
			for id := range calls - 1 {
				assert.True(t, e.Channels[id+1])
			}
			return service.GatewayAttemptResult{TransportAttempt: true, SkippedChannel: calls, Failure: service.GatewayFailure{Class: "connection", Switch: true}}
		})
		assert.Equal(t, 3, calls)
		assert.Empty(t, attempts, "Connection setup does not create model-generation records")
	})
	t.Run("metadata wait retains handshake expenditure", func(t *testing.T) {
		fake := clock.UseFake(t, time.Unix(100, 0))
		service.RunGatewayAttempts(t.Context(), service.GatewayAttemptPolicy{MaxAttempts: 3, TotalFirstByte: 20 * time.Second, AttemptFirstByte: 30 * time.Second}, func(b *service.GatewayAttemptBudget, e *service.GatewayExclusions) service.GatewayAttemptResult {
			ctx, err := b.Start()
			require.NoError(t, err)
			fake.Advance(4 * time.Second)
			b.Pause()
			fake.Advance(time.Hour)
			require.NoError(t, ctx.Err())
			resumed, err := b.Start()
			require.NoError(t, err)
			assert.NoError(t, resumed.Err())
			fake.Advance(16*time.Second - time.Nanosecond)
			require.NoError(t, ctx.Err())
			fake.Advance(time.Nanosecond)
			assert.ErrorIs(t, context.Cause(ctx), service.ErrGatewayBudget)
			return service.GatewayAttemptResult{TransportAttempt: true, Established: true}
		})
	})
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
