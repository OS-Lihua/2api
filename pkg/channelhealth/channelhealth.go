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
	RetryAt         int64
	Failures, Trips int
	Lease           string
	LeaseUntil      int64
	Recent          []observation
}

// Manager uses process memory for one node or optimistic Redis transactions for
// all nodes. Redis errors fail admission closed, avoiding independent probes.
type seenEvent struct {
	id      string
	expires int64
}

type Manager struct {
	client    *redis.Client
	mu        sync.Mutex
	states    map[int]*state
	seen      map[string]int64
	seenQueue []seenEvent
	seenHead  int
}

func New(client *redis.Client) *Manager {
	return &Manager{client: client, states: map[int]*state{}, seen: map[string]int64{}}
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
		now := clock.Now().UnixMilli()
		for m.seenHead < len(m.seenQueue) && m.seenQueue[m.seenHead].expires <= now {
			event := m.seenQueue[m.seenHead]
			if m.seen[event.id] == event.expires {
				delete(m.seen, event.id)
			}
			m.seenHead++
		}
		if m.seenHead > 1024 && m.seenHead > len(m.seenQueue)/2 {
			m.seenQueue = slices.Clone(m.seenQueue[m.seenHead:])
			m.seenHead = 0
		}
		for _, id := range ids {
			if m.states[id] == nil {
				m.states[id] = &state{}
			}
		}
		duplicate := event != "" && m.seen[event] > now
		if apply(m.states, duplicate) && event != "" {
			m.seen[event] = now + int64(7*24*time.Hour/time.Millisecond)
			m.seenQueue = append(m.seenQueue, seenEvent{id: event, expires: m.seen[event]})
		}
		return nil
	}
	keys := make([]string, 0, len(ids)+1)
	for _, id := range ids {
		keys = append(keys, "gateway:channel-health:"+strconv.Itoa(id))
	}
	eventKey := "gateway:channel-health:attempt:" + event
	if event != "" {
		keys = append(keys, eventKey)
	}
	for range 64 {
		err := m.client.Watch(ctx, func(tx *redis.Tx) error {
			states := map[int]*state{}
			for i, id := range ids {
				raw, err := tx.Get(ctx, keys[i]).Bytes()
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
			duplicate := false
			if event != "" {
				exists, err := tx.Exists(ctx, eventKey).Result()
				if err != nil {
					return err
				}
				duplicate = exists > 0
			}
			if !apply(states, duplicate) {
				return nil
			}
			_, err := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				for i, id := range ids {
					raw, err := common.Marshal(states[id])
					if err != nil {
						return err
					}
					expiration := 24 * time.Hour
					if states[id].RetryAt > 0 {
						expiration = 0
					}
					pipe.Set(ctx, keys[i], raw, expiration)
				}
				if event != "" {
					pipe.Set(ctx, eventKey, "1", 7*24*time.Hour)
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
			if s.RetryAt > 0 {
				snapshot.Circuit = "open"
			}
			if s.LeaseUntil > clock.Now().UnixMilli() {
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
			if states[id].RetryAt == 0 {
				continue
			}
			if earliest == 0 || states[id].RetryAt < states[earliest].RetryAt || states[id].RetryAt == states[earliest].RetryAt && id < earliest {
				earliest = id
			}
		}
		probe := earliest != 0 && states[earliest].RetryAt <= now && states[earliest].LeaseUntil <= now
		if !probe {
			for _, id := range ids {
				if states[id].RetryAt == 0 {
					admission.ChannelID = id
					return false
				}
			}
		}
		if earliest == 0 {
			return false
		}
		s := states[earliest]
		if s.LeaseUntil > now {
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
// excessive values use the normal exponential cooldown. No raw header is kept.
func RetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds > 0 && seconds <= 86400 {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	if at, err := http.ParseTime(value); err == nil {
		delay := at.Sub(now)
		if delay > 0 && delay <= 24*time.Hour {
			return delay
		}
	}
	return 0
}

// Record consumes an actual completed Attempt once. Recovery and settlement do
// not call this API; the idempotency key also protects accidental duplicate use.
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
		if sample.Lease != "" && s.Lease == sample.Lease {
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
		if sample.Success {
			if s.RetryAt > 0 {
				action = "closed"
			}
			s.Failures, s.Trips, s.RetryAt = 0, 0, 0
			s.Lease, s.LeaseUntil = "", 0
			return true
		}
		s.Failures++
		if s.Failures >= 5 || s.RetryAt > 0 || sample.RetryAfter != "" {
			s.Trips = min(s.Trips+1, 5)
			cooldown := min(time.Minute*time.Duration(1<<(s.Trips-1)), 10*time.Minute)
			if delay := RetryAfter(sample.RetryAfter, now); delay > 0 {
				cooldown = delay
			}
			s.RetryAt = now.Add(cooldown).UnixMilli()
			action = "opened"
		}
		return true
	})
	return action, err
}
