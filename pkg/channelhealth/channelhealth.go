// Package channelhealth coordinates runtime Channel circuits and recent health.
package channelhealth

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/go-redis/redis/v8"
)

type Sample struct {
	AttemptID                string
	ChannelID                int
	Success, ProviderFailure bool
	RateLimited              bool
	FirstByte                time.Duration
	RetryAfter               string
	Lease                    string
}

type Snapshot struct {
	Circuit          string  `json:"circuit"`
	RetryAt          int64   `json:"retry_at"`
	Failures         int     `json:"failures"`
	Samples          int     `json:"samples"`
	SuccessRate      float64 `json:"success_rate"`
	FirstByteMillis  int64   `json:"first_byte_ms"`
	FirstByteSamples int     `json:"first_byte_samples"`
}

type Admission struct {
	ChannelID int
	Lease     string
}
type observation struct {
	At        int64
	Success   bool
	FirstByte int64
}
type state struct {
	RetryAt           int64
	Failures, Trips   int
	Lease             string
	LeaseUntil        int64
	Recent            []observation
	completedAttempts []string
}

// Manager uses process memory for one node or optimistic Redis transactions for
// all nodes. Redis errors fail admission closed, avoiding independent probes.
// Deduplication retains the last 100 completed Attempt identities per Channel.
type Manager struct {
	client *redis.Client
	mu     sync.Mutex
	states map[int]*state
}

func New(client *redis.Client) *Manager {
	return &Manager{client: client, states: map[int]*state{}}
}

// transact watches every candidate together, so choosing the earliest circuit
// and claiming its probe are one atomic operation across serving nodes.
func (m *Manager) transact(ctx context.Context, ids []int, event string, apply func(map[int]*state, bool) bool) error {
	if len(ids) == 0 {
		apply(map[int]*state{}, false)
		return nil
	}
	if m.client == nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, id := range ids {
			if m.states[id] == nil {
				m.states[id] = &state{}
			}
		}
		current := m.states[ids[0]]
		duplicate := event != "" && slices.Contains(current.completedAttempts, event)
		if apply(m.states, duplicate) && event != "" {
			current.completedAttempts = append(current.completedAttempts, event)
			if len(current.completedAttempts) > 100 {
				current.completedAttempts = slices.Clone(current.completedAttempts[len(current.completedAttempts)-100:])
			}
		}
		return nil
	}
	keys := make([]string, 0, len(ids)+1)
	for _, id := range ids {
		keys = append(keys, "gateway:channel-health:"+strconv.Itoa(id))
	}
	eventKey := "gateway:channel-health:attempt:" + event
	ringKey := "gateway:channel-health:attempts:" + strconv.Itoa(ids[0])
	if event != "" {
		keys = append(keys, ringKey, eventKey)
	}
	for range 64 {
		err := m.client.Watch(ctx, func(tx *redis.Tx) error {
			reads := make([]*redis.StringCmd, len(keys))
			_, err := tx.Pipelined(ctx, func(pipe redis.Pipeliner) error {
				for i, key := range keys {
					reads[i] = pipe.Get(ctx, key)
				}
				return nil
			})
			if err != nil && !errors.Is(err, redis.Nil) {
				return err
			}
			states := map[int]*state{}
			for i, id := range ids {
				raw, err := reads[i].Bytes()
				if err != nil && !errors.Is(err, redis.Nil) {
					return err
				}
				current := &state{}
				if len(raw) > 0 {
					if err := common.Unmarshal(raw, current); err != nil {
						return errors.New("invalid Channel health state")
					}
				}
				states[id] = current
			}
			var completed []string
			duplicate, evicted := false, ""
			if event != "" {
				raw, err := reads[len(ids)].Bytes()
				if err != nil && !errors.Is(err, redis.Nil) {
					return err
				}
				if len(raw) > 0 {
					if err := common.Unmarshal(raw, &completed); err != nil {
						return errors.New("invalid Channel health Attempt history")
					}
				}
				legacy := reads[len(ids)+1]
				if err := legacy.Err(); err != nil && !errors.Is(err, redis.Nil) {
					return err
				}
				duplicate = slices.Contains(completed, event) || legacy.Err() == nil
			}
			if !apply(states, duplicate) {
				return nil
			}
			if event != "" {
				completed = append(completed, event)
				if len(completed) > 100 {
					evicted = completed[0]
					completed = completed[len(completed)-100:]
				}
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				for i, id := range ids {
					raw, err := common.Marshal(states[id])
					if err != nil {
						return err
					}
					expiration := 24 * time.Hour
					if states[id].Trips > 0 {
						expiration = 0
					}
					pipe.Set(ctx, keys[i], raw, expiration)
				}
				if event != "" {
					raw, err := common.Marshal(completed)
					if err != nil {
						return err
					}
					// Keep the ring separate so older nodes cannot erase it while
					// rewriting Channel state. Legacy readers still see recent IDs.
					pipe.Set(ctx, ringKey, raw, 7*24*time.Hour)
					pipe.Set(ctx, eventKey, "1", 7*24*time.Hour)
					if evicted != "" {
						pipe.Del(ctx, "gateway:channel-health:attempt:"+evicted)
					}
				}
				return nil
			})
			return err
		}, keys...)
		if !errors.Is(err, redis.TxFailedErr) {
			return err
		}
	}
	return errors.New("Channel health contention")
}

func (m *Manager) Snapshots(ctx context.Context, ids []int) (map[int]Snapshot, error) {
	result := map[int]Snapshot{}
	err := m.transact(ctx, ids, "", func(states map[int]*state, _ bool) bool {
		result = map[int]Snapshot{}
		for _, id := range ids {
			s := states[id]
			snapshot := Snapshot{Circuit: "closed", RetryAt: s.RetryAt, Failures: s.Failures, SuccessRate: 1}
			if s.Trips > 0 {
				snapshot.Circuit = "open"
			}
			if s.Lease != "" && s.LeaseUntil > clock.Now().UnixMilli() {
				snapshot.Circuit = "half_open"
			}
			successes := 0
			var latency int64
			latencyCount := 0
			for _, entry := range s.Recent {
				if entry.At < clock.Now().Add(-5*time.Minute).UnixMilli() {
					continue
				}
				snapshot.Samples++
				if entry.Success {
					successes++
				}
				if entry.FirstByte >= 0 {
					latency += entry.FirstByte
					latencyCount++
				}
			}
			if snapshot.Samples > 0 {
				snapshot.SuccessRate = float64(successes) / float64(snapshot.Samples)
			}
			if latencyCount > 0 {
				snapshot.FirstByteSamples = latencyCount
				snapshot.FirstByteMillis = latency / int64(latencyCount)
			}
			result[id] = snapshot
		}
		return false
	})
	return result, err
}

// Acquire takes candidates already ordered by Route. The earliest expired
// circuit receives one recovery probe before normal health ordering. If all
// circuits are open, only the earliest can probe, including before cooldown.
func (m *Manager) Acquire(ctx context.Context, ids []int) (Admission, error) {
	admission := Admission{}
	lease := common.NewRequestId()
	err := m.transact(ctx, ids, "", func(states map[int]*state, _ bool) bool {
		admission = Admission{}
		now := clock.Now().UnixMilli()
		earliest := 0
		for _, id := range ids {
			if states[id].Trips == 0 {
				continue
			}
			if earliest == 0 || states[id].RetryAt < states[earliest].RetryAt || states[id].RetryAt == states[earliest].RetryAt && id < earliest {
				earliest = id
			}
		}
		probe := earliest != 0 && states[earliest].RetryAt <= now && (states[earliest].Lease == "" || states[earliest].LeaseUntil <= now)
		if !probe {
			for _, id := range ids {
				if states[id].Trips == 0 {
					admission.ChannelID = id
					return false
				}
			}
		}
		if earliest == 0 {
			return false
		}
		s := states[earliest]
		if s.Lease != "" && s.LeaseUntil > now {
			return false
		}
		// Covers the maximum managed first-byte budget plus delivery/idle time;
		// completion or abandoned preparation releases the lease immediately.
		s.Lease, s.LeaseUntil = lease, now+int64(10*time.Minute/time.Millisecond)
		admission = Admission{ChannelID: earliest, Lease: lease}
		return true
	})
	return admission, err
}

type probeLease struct {
	manager   *Manager
	ctx       context.Context
	admission Admission
	mu        sync.Mutex
	closed    bool
	stop      func() bool
}

// KeepAlive retains a single probe while a long response is still in flight.
// A crashed node stops renewing, so its lease remains bounded.
func (m *Manager) KeepAlive(ctx context.Context, admission Admission) func() {
	if admission.Lease == "" {
		return func() {}
	}
	lease := &probeLease{manager: m, ctx: ctx, admission: admission}
	lease.stop = clock.AfterFunc(time.Minute, lease.renew)
	return lease.close
}

func (lease *probeLease) renew() {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.closed || lease.ctx.Err() != nil {
		return
	}
	_ = lease.manager.transact(lease.ctx, []int{lease.admission.ChannelID}, "", func(states map[int]*state, _ bool) bool {
		s := states[lease.admission.ChannelID]
		if s.Lease != lease.admission.Lease {
			return false
		}
		s.LeaseUntil = clock.Now().Add(10 * time.Minute).UnixMilli()
		return true
	})
	lease.stop = clock.AfterFunc(time.Minute, lease.renew)
}

func (lease *probeLease) close() {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	lease.closed = true
	lease.stop()
}

func (m *Manager) Release(ctx context.Context, admission Admission) error {
	if admission.Lease == "" {
		return nil
	}
	return m.transact(ctx, []int{admission.ChannelID}, "", func(states map[int]*state, _ bool) bool {
		s := states[admission.ChannelID]
		if s.Lease != admission.Lease {
			return false
		}
		s.Lease, s.LeaseUntil = "", 0
		return true
	})
}

// RetryAfter accepts both RFC HTTP dates and integer delta seconds. Invalid or
// overflowing values use the normal exponential cooldown. The validity flag
// preserves explicit zero and elapsed dates as immediate shared probes.
func RetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "+") || strings.HasPrefix(value, "-") {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds >= 0 && seconds <= int64((1<<63-1)/time.Second) {
			return time.Duration(seconds) * time.Second, true
		}
		return 0, false
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(at.Sub(now), 0), true
	}
	return 0, false
}

// Record consumes an actual completed Attempt. Recovery and settlement do not
// call this API; recent Attempt identities protect accidental duplicate use.
func (m *Manager) Record(ctx context.Context, sample Sample) (string, error) {
	if sample.AttemptID == "" || sample.ChannelID <= 0 {
		return "none", errors.New("actual Attempt identity required")
	}
	action := "none"
	err := m.transact(ctx, []int{sample.ChannelID}, sample.AttemptID, func(states map[int]*state, duplicate bool) bool {
		action = "none"
		if duplicate {
			return false
		}
		s := states[sample.ChannelID]
		now := clock.Now()
		ownsLease := sample.Lease != "" && s.Lease == sample.Lease
		if ownsLease {
			s.Lease, s.LeaseUntil = "", 0
		}
		if !sample.Success && !sample.ProviderFailure {
			return true
		}
		firstByte := int64(-1)
		if sample.FirstByte > 0 {
			firstByte = sample.FirstByte.Milliseconds()
		}
		s.Recent = append(s.Recent, observation{At: now.UnixMilli(), Success: sample.Success, FirstByte: firstByte})
		if len(s.Recent) > 100 {
			s.Recent = slices.Clone(s.Recent[len(s.Recent)-100:])
		}
		// Fence obsolete probes and successes that predate a trip. Ordinary
		// in-flight failures still update cooldowns, including Retry-After.
		if !ownsLease && (sample.Lease != "" || sample.Success && s.Trips > 0) {
			return true
		}
		if sample.Success {
			if s.Trips > 0 {
				action = "closed"
			}
			s.Failures, s.Trips, s.RetryAt = 0, 0, 0
			s.Lease, s.LeaseUntil = "", 0
			return true
		}
		s.Failures++
		if s.Failures >= 5 || s.Trips > 0 || sample.RateLimited || sample.RetryAfter != "" {
			s.Trips = min(s.Trips+1, 5)
			cooldown := min(time.Minute*time.Duration(1<<(s.Trips-1)), 10*time.Minute)
			if delay, valid := RetryAfter(sample.RetryAfter, now); valid {
				cooldown = delay
			}
			s.RetryAt = now.Add(cooldown).UnixMilli()
			action = "opened"
		}
		return true
	})
	return action, err
}
