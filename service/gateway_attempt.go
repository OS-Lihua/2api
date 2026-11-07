package service

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/clock"
)

// GatewayAttemptPolicy bounds actual upstream sends across the whole request.
// Environment configuration is read once per request; it is not persisted.
type GatewayAttemptPolicy struct {
	MaxAttempts                      int
	TotalFirstByte, AttemptFirstByte time.Duration
}

func GatewayPolicy() GatewayAttemptPolicy {
	p := GatewayAttemptPolicy{3, 90 * time.Second, 30 * time.Second}
	if n, err := strconv.Atoi(os.Getenv("GATEWAY_MAX_ATTEMPTS")); err == nil && n >= 1 && n <= 5 {
		p.MaxAttempts = n
	}
	if n, err := strconv.Atoi(os.Getenv("GATEWAY_TOTAL_FIRST_BYTE_SECONDS")); err == nil && n >= 5 && n <= 600 {
		p.TotalFirstByte = time.Duration(n) * time.Second
	}
	if n, err := strconv.Atoi(os.Getenv("GATEWAY_ATTEMPT_FIRST_BYTE_SECONDS")); err == nil && n >= 1 && n <= 600 {
		p.AttemptFirstByte = time.Duration(n) * time.Second
	}
	return p
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
func ClassifyGatewayFailure(status int, code string, transport, timeout, cancelled, downstream bool) GatewayFailure {
	switch {
	case cancelled:
		return GatewayFailure{Class: "client_cancelled"}
	case downstream:
		return GatewayFailure{Class: "downstream_write_failed"}
	case code == "context_length_exceeded" || code == "context_window_exceeded":
		return GatewayFailure{Class: "context_length"}
	case code == "content_filter" || code == "content_policy_violation" || code == "safety":
		return GatewayFailure{Class: "content_safety"}
	case code == "insufficient_quota" || code == "insufficient_balance" || code == "credit_balance_too_low" || code == "billing_hard_limit_reached":
		return GatewayFailure{Class: "account_quota", Switch: true, ExcludeProvider: true}
	case status == 401 || status == 403 || code == "invalid_api_key" || code == "authentication_error":
		return GatewayFailure{Class: "credentials", Switch: true, ExcludeProvider: true}
	case status == 404 && (code == "model_not_found" || code == "model_not_exist" || code == "not_found_error" || strings.HasPrefix(code, "model '") && strings.HasSuffix(code, "' not found") || strings.HasPrefix(code, "model \"") && strings.HasSuffix(code, "\" not found")):
		return GatewayFailure{Class: "model_not_found", Switch: true}
	case status == 429 || code == "rate_limit_exceeded":
		return GatewayFailure{Class: "rate_limited", Switch: true}
	case status == 500 || status == 502 || status == 503 || status == 504 || status == 529 || code == "overloaded" || code == "overloaded_error":
		return GatewayFailure{Class: "upstream_unavailable", Switch: true}
	case status == 400 || status == 413 || status == 422:
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

// GatewayAttemptBudget starts its timer only at the actual send boundary.
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
	SkippedChannel int
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
		if result.SkippedChannel > 0 && result.Attempt == nil {
			if exclusions.Channels[result.SkippedChannel] {
				break
			}
			exclusions.Channels[result.SkippedChannel] = true
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
