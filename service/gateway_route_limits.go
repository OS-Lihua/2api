package service

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/go-redis/redis/v8"
)

// GatewayRouteQuota shares Provider and Channel admission across all protocol
// entry points. Redis deployments use one transaction for both scopes.
var GatewayRouteQuota = NewGatewayRouteQuota(nil)

var ErrGatewayRateLimited = errors.New("Provider or Channel admission limit reached")

type gatewayQuotaState struct {
	Window   int64            `json:"window"`
	Requests int64            `json:"requests"`
	Tokens   int64            `json:"tokens"`
	Leases   map[string]int64 `json:"leases"`
}

type GatewayRouteQuotaManager struct {
	client *redis.Client
	mu     sync.Mutex
	states map[string]*gatewayQuotaState
}

func NewGatewayRouteQuota(client *redis.Client) *GatewayRouteQuotaManager {
	return &GatewayRouteQuotaManager{client: client, states: map[string]*gatewayQuotaState{}}
}

type gatewayQuotaScope struct {
	key    string
	limits model.ProviderRouteLimits
}

func gatewayQuotaScopes(selection *GatewaySelection) []gatewayQuotaScope {
	return []gatewayQuotaScope{
		{"new-api:route-quota:v1:provider:" + strconv.Itoa(selection.Provider.Id), selection.Provider.ProviderInfo.RouteLimits},
		{"new-api:route-quota:v1:channel:" + strconv.Itoa(selection.Channel.ID), model.ProviderRouteLimits{RPM: selection.Channel.RPM, TPM: selection.Channel.TPM, MaxConcurrency: selection.Channel.MaxConcurrency}},
	}
}

func (manager *GatewayRouteQuotaManager) transact(ctx context.Context, scopes []gatewayQuotaScope, change func(map[string]*gatewayQuotaState) error) error {
	now := clock.Now()
	keys := make([]string, len(scopes))
	for i, scope := range scopes {
		keys[i] = scope.key
	}
	if manager.client == nil {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		states := map[string]*gatewayQuotaState{}
		for _, key := range keys {
			state := manager.states[key]
			if state == nil {
				state = &gatewayQuotaState{}
				manager.states[key] = state
			}
			normalizeGatewayQuota(state, now)
			states[key] = state
		}
		return change(states)
	}
	for range 8 {
		err := manager.client.Watch(ctx, func(tx *redis.Tx) error {
			states := map[string]*gatewayQuotaState{}
			for _, key := range keys {
				raw, err := tx.Get(ctx, key).Result()
				state := &gatewayQuotaState{}
				if err != nil && !errors.Is(err, redis.Nil) {
					return err
				}
				if err == nil {
					if err = common.UnmarshalJsonStr(raw, state); err != nil {
						return err
					}
				}
				normalizeGatewayQuota(state, now)
				states[key] = state
			}
			if err := change(states); err != nil {
				return err
			}
			_, err := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				for _, key := range keys {
					raw, err := common.Marshal(states[key])
					if err != nil {
						return err
					}
					pipe.Set(ctx, key, raw, 15*time.Minute)
				}
				return nil
			})
			return err
		}, keys...)
		if !errors.Is(err, redis.TxFailedErr) {
			return err
		}
	}
	return ErrGatewayUnavailable
}

func normalizeGatewayQuota(state *gatewayQuotaState, now time.Time) {
	window := now.Unix() / 60
	if state.Window != window {
		state.Window = window
		state.Requests = 0
		state.Tokens = 0
	}
	if state.Leases == nil {
		state.Leases = map[string]int64{}
	}
	for lease, until := range state.Leases {
		if until <= now.UnixMilli() {
			delete(state.Leases, lease)
		}
	}
}

func (manager *GatewayRouteQuotaManager) Check(ctx context.Context, selection *GatewaySelection, tokens int64) error {
	return manager.admit(ctx, selection, tokens, "", nil, false)
}

func (manager *GatewayRouteQuotaManager) admit(ctx context.Context, selection *GatewaySelection, tokens int64, lease string, held *GatewayRoutePermit, used bool) error {
	if tokens < 0 || tokens > 1<<31-1 {
		return ErrGatewayRateLimited
	}
	scopes := gatewayQuotaScopes(selection)
	return manager.transact(ctx, scopes, func(states map[string]*gatewayQuotaState) error {
		for _, scope := range scopes {
			state := states[scope.key]
			limits := scope.limits
			requests, forecast, concurrent := state.Requests, state.Tokens, int64(len(state.Leases))
			if held != nil {
				if _, owns := state.Leases[held.lease]; owns {
					concurrent--
					if !used && state.Window == held.window {
						requests = max(0, requests-1)
						forecast = max(0, forecast-held.tokens)
					}
				}
			}
			if !limits.Valid() || limits.RPM > 0 && requests >= limits.RPM || limits.TPM > 0 && (tokens > limits.TPM || forecast > limits.TPM-tokens) || limits.MaxConcurrency > 0 && concurrent >= limits.MaxConcurrency {
				return ErrGatewayRateLimited
			}
		}
		if lease == "" {
			return nil
		}
		for _, scope := range scopes {
			state := states[scope.key]
			state.Requests++
			state.Tokens += tokens
			state.Leases[lease] = clock.Now().Add(10 * time.Minute).UnixMilli()
		}
		return nil
	})
}

type GatewayRoutePermit struct {
	manager        *GatewayRouteQuotaManager
	scopes         []gatewayQuotaScope
	lease          string
	tokens, window int64
	mu             sync.Mutex
	stop           func() bool
	closed         bool
}

// CheckAlternative tests admission after this Permit releases its slot. An
// unused reservation also returns its request and token forecast.
func (permit *GatewayRoutePermit) CheckAlternative(ctx context.Context, selection *GatewaySelection, tokens int64, used bool) error {
	if permit == nil {
		return ErrGatewayUnavailable
	}
	permit.mu.Lock()
	defer permit.mu.Unlock()
	if permit.closed {
		return permit.manager.Check(ctx, selection, tokens)
	}
	return permit.manager.admit(ctx, selection, tokens, "", permit, used)
}

func (manager *GatewayRouteQuotaManager) Acquire(ctx context.Context, selection *GatewaySelection, tokens int64) (*GatewayRoutePermit, error) {
	lease := common.NewRequestId()
	if err := manager.admit(ctx, selection, tokens, lease, nil, false); err != nil {
		return nil, err
	}
	permit := &GatewayRoutePermit{manager: manager, scopes: gatewayQuotaScopes(selection), lease: lease, tokens: tokens, window: clock.Now().Unix() / 60}
	permit.stop = clock.AfterFunc(time.Minute, permit.renew)
	return permit, nil
}

func (permit *GatewayRoutePermit) renew() {
	permit.mu.Lock()
	defer permit.mu.Unlock()
	if permit.closed {
		return
	}
	// Renewal is independent of a cancelled downstream until the actual Provider
	// call has returned and released its concurrency slot.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = permit.manager.transact(ctx, permit.scopes, func(states map[string]*gatewayQuotaState) error {
		for _, scope := range permit.scopes {
			state := states[scope.key]
			if _, ok := state.Leases[permit.lease]; ok {
				state.Leases[permit.lease] = clock.Now().Add(10 * time.Minute).UnixMilli()
			}
		}
		return nil
	})
	permit.stop = clock.AfterFunc(time.Minute, permit.renew)
}

// Release returns concurrency once. A candidate that never contacted its
// Provider also returns its provisional RPM/TPM reservation. A supplied actual
// token count replaces the forecast, including an explicitly reported zero.
func (permit *GatewayRoutePermit) Release(ctx context.Context, used bool, actualTokens ...int64) error {
	if permit == nil {
		return nil
	}
	permit.mu.Lock()
	defer permit.mu.Unlock()
	if permit.closed {
		return nil
	}
	if permit.stop != nil {
		permit.stop()
	}
	err := permit.manager.transact(ctx, permit.scopes, func(states map[string]*gatewayQuotaState) error {
		for _, scope := range permit.scopes {
			state := states[scope.key]
			if _, ok := state.Leases[permit.lease]; !ok {
				continue
			}
			delete(state.Leases, permit.lease)
			if state.Window == permit.window {
				if !used {
					state.Requests = max(0, state.Requests-1)
					state.Tokens = max(0, state.Tokens-permit.tokens)
				} else if len(actualTokens) > 0 && actualTokens[0] >= 0 {
					state.Tokens = max(0, state.Tokens-permit.tokens)
					if actualTokens[0] > (1<<63-1)-state.Tokens {
						// Exhaust only this runtime counter; recorded usage and
						// financial amounts retain their original exact values.
						state.Tokens = 1<<63 - 1
					} else {
						state.Tokens += actualTokens[0]
					}
				}
			}
		}
		return nil
	})
	if err == nil {
		permit.closed = true
	}
	return err
}
