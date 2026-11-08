package service

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/setting"
)

// GatewayAttemptPolicy bounds actual upstream sends across the whole request.
// The effective persisted configuration is frozen once per request.
type GatewayAttemptPolicy struct {
	MaxAttempts                      int
	TotalFirstByte, AttemptFirstByte time.Duration
}

func GatewayPolicy() GatewayAttemptPolicy {
	values := setting.GatewayBudgetDefaults()
	common.OptionMapRWMutex.RLock()
	for key := range values {
		if raw, ok := common.OptionMap[key]; ok && setting.ValidateGatewayBudgetOption(key, raw) == nil {
			values[key], _ = strconv.Atoi(raw)
		}
	}
	common.OptionMapRWMutex.RUnlock()
	return GatewayAttemptPolicy{
		MaxAttempts:      values["GatewayMaxAttempts"],
		TotalFirstByte:   time.Duration(values["GatewayTotalFirstByteSeconds"]) * time.Second,
		AttemptFirstByte: time.Duration(values["GatewayAttemptFirstByteSeconds"]) * time.Second,
	}
}

// GatewayExclusions is request-local. Credentials and account quota exclude a
// Provider; other failures exclude only the actual Channel.
type GatewayExclusions struct{ Channels, Providers map[int]bool }

type GatewayFailure struct {
	Class                   string
	Switch, ExcludeProvider bool
}

// ClassifyGatewayFailure accepts protocol facts, including a native error reason.
// Its returned labels are fixed and safe to persist. Local validation and
// authorization must not be supplied as upstream HTTP failures.
func ClassifyGatewayFailure(status int, code string, transport, timeout, cancelled, downstream bool, errorTypes ...string) GatewayFailure {
	kind := ""
	if len(errorTypes) > 0 {
		kind = errorTypes[0]
	}
	switch {
	case cancelled:
		return GatewayFailure{Class: "client_cancelled"}
	case downstream:
		return GatewayFailure{Class: "downstream_write_failed"}
	case code == "context_length_exceeded" || code == "context_window_exceeded" || kind == "context_length_exceeded":
		return GatewayFailure{Class: "context_length"}
	case code == "content_filter" || code == "content_policy_violation" || code == "safety" || kind == "content_policy_violation" || kind == "content_filter":
		return GatewayFailure{Class: "content_safety"}
	case code == "baidu:17" || code == "insufficient_quota" || code == "insufficient_balance" || code == "credit_balance_too_low" || code == "billing_hard_limit_reached" || code == "ServiceQuotaExceededException":
		return GatewayFailure{Class: "account_quota", Switch: true, ExcludeProvider: true}
	case code == "baidu:110" || code == "baidu:111" || code == "invalid_client" || code == "unauthorized_client" || status == 401 || status == 403 || code == "invalid_api_key" || code == "authentication_error" || kind == "authentication_error":
		return GatewayFailure{Class: "credentials", Switch: true, ExcludeProvider: true}
	case code == "stream_idle_timeout":
		return GatewayFailure{Class: "stream_interrupted", Switch: true}
	case (status == 200 || status == 404) && (code == "model_not_found" || code == "model_not_exist" || code == "not_found_error" || code == "ResourceNotFoundException" || code == "DeploymentNotFound" || strings.HasPrefix(code, "model '") && strings.HasSuffix(code, "' not found") || strings.HasPrefix(code, "model \"") && strings.HasSuffix(code, "\" not found")):
		return GatewayFailure{Class: "model_not_found", Switch: true}
	case status == 429 || code == "rate_limit_exceeded" || code == "ThrottlingException":
		return GatewayFailure{Class: "rate_limited", Switch: true}
	case status == 500 || status == 502 || status == 503 || status == 504 || status == 529 || code == "overloaded" || code == "overloaded_error":
		return GatewayFailure{Class: "upstream_unavailable", Switch: true}
	case status == 400 || status == 413 || status == 422 || code == "invalid_request_error" || kind == "invalid_request_error" || code == "ValidationException":
		return GatewayFailure{Class: "invalid_request"}
	case timeout:
		return GatewayFailure{Class: "first_byte_timeout", Switch: true}
	case transport:
		return GatewayFailure{Class: "connection_failed", Switch: true}
	default:
		return GatewayFailure{Class: "upstream_error"}
	}
}

var ErrGatewayBudget = errors.New("gateway first byte budget exhausted")

// GatewayAttemptBudget starts at the first actual Provider call, including
// native request preparation. Later calls reuse its deadline without resetting it.
// Delivered must be called by the same shared delivery fact used for billing.
type GatewayAttemptBudget struct {
	parent    context.Context
	policy    GatewayAttemptPolicy
	remaining time.Duration
	started   time.Time
	elapsed   time.Duration
	finished  bool
	stop      func() bool
	cancel    context.CancelCauseFunc
	sent      bool
	ctx       context.Context
}

func (b *GatewayAttemptBudget) Start() (context.Context, error) {
	if err := b.parent.Err(); err != nil {
		return nil, err
	}
	if b.ctx != nil {
		if cause := context.Cause(b.ctx); cause != nil {
			return nil, cause
		}
		if b.remaining-clock.Now().Sub(b.started) < 5*time.Second {
			return nil, ErrGatewayBudget
		}
		return b.ctx, nil
	}
	remaining := b.remaining
	if remaining < 5*time.Second {
		return nil, ErrGatewayBudget
	}
	ctx, cancel := context.WithCancelCause(b.parent)
	b.ctx = ctx
	b.started = clock.Now()
	b.cancel, b.sent = cancel, true
	b.stop = clock.AfterFunc(min(remaining, b.policy.AttemptFirstByte), func() { cancel(ErrGatewayBudget) })
	return ctx, nil
}
func (b *GatewayAttemptBudget) Delivered() {
	if b.sent && !b.finished {
		b.finished = true
		b.elapsed = min(clock.Now().Sub(b.started), b.remaining, b.policy.AttemptFirstByte)
	}
	if b.stop != nil {
		b.stop()
	}
}
func (b *GatewayAttemptBudget) TimedOut() bool {
	return b.ctx != nil && errors.Is(context.Cause(b.ctx), ErrGatewayBudget)
}

type GatewayAttemptResult struct {
	Attempt   *model.GatewayAttempt
	Failure   GatewayFailure
	Delivered bool
	// Skipped excludes a candidate rejected by preparation, without an Attempt.
	SkippedChannel  int
	SkippedProvider int
}

// RunGatewayAttempts owns the shared attempt count, first-byte deadline and
// request-local exclusions. The callback prepares a candidate, calls Start
// immediately before sending, and returns its finalized quantitative facts.
func RunGatewayAttempts(ctx context.Context, policy GatewayAttemptPolicy, execute func(*GatewayAttemptBudget, *GatewayExclusions) GatewayAttemptResult) []model.GatewayAttempt {
	if policy.MaxAttempts < 1 || policy.MaxAttempts > 5 || policy.TotalFirstByte < 5*time.Second || policy.AttemptFirstByte <= 0 {
		return nil
	}
	exclusions := &GatewayExclusions{Channels: map[int]bool{}, Providers: map[int]bool{}}
	remaining := policy.TotalFirstByte
	attempts := []model.GatewayAttempt{}
	for len(attempts) < policy.MaxAttempts && ctx.Err() == nil && remaining >= 5*time.Second {
		budget := &GatewayAttemptBudget{parent: ctx, policy: policy, remaining: remaining}
		result := execute(budget, exclusions)
		budget.Delivered()
		remaining -= budget.elapsed
		if budget.cancel != nil {
			budget.cancel(nil)
		}
		if result.Attempt == nil && result.Failure.Class != "" && !result.Failure.Switch {
			if len(attempts) > 0 && attempts[len(attempts)-1].FailoverDecision == "switch" {
				attempts[len(attempts)-1].FailoverDecision = "stop"
				attempts[len(attempts)-1].FailoverReason = result.Failure.Class
			}
			break
		}
		if result.SkippedChannel > 0 && result.Attempt == nil {
			if exclusions.Channels[result.SkippedChannel] {
				break
			}
			exclusions.Channels[result.SkippedChannel] = true
			if result.SkippedProvider > 0 {
				exclusions.Providers[result.SkippedProvider] = true
			}
			continue
		}
		if result.Attempt == nil {
			if len(attempts) > 0 && attempts[len(attempts)-1].FailoverDecision == "switch" {
				attempts[len(attempts)-1].FailoverDecision = "stop"
				attempts[len(attempts)-1].FailoverReason = "candidates_exhausted"
			}
			break
		}
		attempt := result.Attempt
		attempt.FailoverDecision, attempt.FailoverReason = "stop", result.Failure.Class
		if attempt.FailoverReason == "" {
			attempt.FailoverReason = "completed"
		}
		canSwitch := result.Failure.Switch && !result.Delivered && ctx.Err() == nil
		switch {
		case result.Delivered:
			attempt.FailoverReason = "business_delivered"
		case ctx.Err() != nil:
			attempt.FailoverReason = "client_cancelled"
		case len(attempts)+1 >= policy.MaxAttempts && canSwitch:
			attempt.FailoverReason = "attempt_limit"
		case remaining < 5*time.Second && canSwitch:
			attempt.FailoverReason = "first_byte_budget"
		case canSwitch:
			attempt.FailoverDecision = "switch"
		}
		attempts = append(attempts, *attempt)
		if attempt.FailoverDecision != "switch" {
			break
		}
		exclusions.Channels[attempt.ChannelID] = true
		if result.Failure.ExcludeProvider {
			exclusions.Providers[attempt.ProviderID] = true
		}
	}
	return attempts
}
