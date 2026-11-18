package service

import (
	"context"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const balanceReminderBatchSize = 20

// BalanceReminderSummary counts attempts, not external delivery guarantees.
type BalanceReminderSummary struct {
	Claimed  int `json:"claimed"`
	Accepted int `json:"accepted"`
	Retry    int `json:"retry"`
	Skipped  int `json:"skipped"`
}

// BalanceReminderHandler drains the independent durable outbox. Clearing system
// task history cannot erase event identities, receipts or retry obligations.
type BalanceReminderHandler struct{}

func (BalanceReminderHandler) Type() string            { return model.SystemTaskTypeBalanceReminder }
func (BalanceReminderHandler) Interval() time.Duration { return time.Minute }
func (BalanceReminderHandler) NewPayload() any         { return nil }
func (BalanceReminderHandler) Enabled() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	due, err := model.HasDueBalanceReminders(ctx)
	if err != nil {
		common.SysLog(common.LogText("balance reminder queue could not be read"))
	}
	return err == nil && due
}

func (BalanceReminderHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	if ctx.Err() != nil {
		return
	}
	// Verify the original system-task lease before allowing any notification I/O.
	if err := model.UpdateSystemTaskState(task.TaskID, runnerID, nil); err != nil {
		return
	}
	summary, err := RunBalanceReminders(ctx, runnerID)
	if ctx.Err() != nil {
		return
	}
	status, message := model.SystemTaskStatusSucceeded, ""
	if err != nil {
		status, message = model.SystemTaskStatusFailed, "Balance reminder delivery could not complete."
	}
	if err := model.FinishSystemTask(task.TaskID, runnerID, status, summary, message); err != nil {
		common.SysLog(common.LogText("balance reminder job could not persist its result"))
	}
}

// RunBalanceReminders claims one event at a time so no event lease expires while
// another recipient is being contacted. Every I/O has a real 20-second deadline,
// below the 60-second fenced event lease. No database lock crosses network I/O.
func RunBalanceReminders(ctx context.Context, runnerID string) (BalanceReminderSummary, error) {
	var summary BalanceReminderSummary
	for range balanceReminderBatchSize {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		due, err := model.HasDueBalanceReminders(ctx)
		if err != nil {
			return summary, errors.New("balance reminder queue could not be read")
		}
		if !due {
			break
		}
		claims, err := model.ClaimBalanceReminders(ctx, runnerID, 1, time.Minute, getDuration(), constant.NotifyLimitCount)
		if err != nil {
			return summary, errors.New("balance reminder could not be claimed")
		}
		if len(claims) == 0 {
			continue
		} // A skipped/limited head row is not an empty queue.
		event := claims[0]
		summary.Claimed++
		result, allowed, err := deliverBalanceReminder(ctx, event)
		if err != nil {
			return summary, err
		} // Lease recovery preserves the unknown outcome.
		if !allowed {
			summary.Skipped++
			continue
		}
		outcome := model.BalanceReminderOutcome{Code: result.Code}
		switch result.Status {
		case NotificationAccepted:
			outcome.Status = "sent"
		case NotificationUnconfigured, NotificationRejected:
			outcome.Status = "skipped"
		default:
			outcome.Status = "retry"
			outcome.RetryAt = clock.Now().Add(time.Minute * time.Duration(1<<min(max(event.Attempts-1, 0), 6)))
		}
		// Only the fenced receipt write may outlive cancellation. Never detach the
		// network attempt. Losing the fence leaves recovery to the current owner.
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err = model.FinishBalanceReminderDelivery(finishCtx, event.ID, event.ClaimToken, outcome)
		cancel()
		if err != nil {
			return summary, errors.New("balance reminder outcome could not be persisted")
		}
		switch outcome.Status {
		case "sent":
			summary.Accepted++
		case "retry":
			summary.Retry++
		case "skipped":
			summary.Skipped++
		}
	}
	return summary, ctx.Err()
}

func deliverBalanceReminder(ctx context.Context, event model.BalanceReminderDelivery) (NotificationDeliveryResult, bool, error) {
	var user model.User
	err := model.DB.WithContext(ctx).Session(&gorm.Session{Logger: logger.Discard}).Select("id", "email", "status", "setting").First(&user, event.UserID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) || err == nil && user.Status != common.UserStatusEnabled {
		return NotificationDeliveryResult{NotificationRejected, model.BalanceReminderUserUnavailable}, true, nil
	}
	if err != nil {
		return NotificationDeliveryResult{}, false, errors.New("balance reminder recipient could not be read")
	}
	var setting dto.UserSetting
	if user.Setting != "" && common.UnmarshalJsonStr(user.Setting, &setting) != nil {
		return NotificationDeliveryResult{NotificationRejected, model.BalanceReminderInvalidDestination}, true, nil
	}
	data, err := balanceReminderNotification(event, setting.Language)
	if err != nil {
		return NotificationDeliveryResult{}, false, err
	}
	allowed, err := model.CheckBalanceReminderDelivery(ctx, event.ID, event.ClaimToken)
	if err != nil {
		return NotificationDeliveryResult{}, false, errors.New("balance reminder eligibility could not be checked")
	}
	if !allowed {
		return NotificationDeliveryResult{}, false, nil
	}
	remaining := time.UnixMilli(event.LeaseUntil).Sub(clock.Now()) - 5*time.Second
	if remaining <= 0 {
		return NotificationDeliveryResult{}, false, errors.New("balance reminder delivery lease expired")
	}
	attemptCtx, cancel := context.WithTimeout(ctx, min(20*time.Second, remaining))
	defer cancel()
	return DeliverUserNotification(attemptCtx, user.Email, setting, data), true, nil
}

func balanceReminderNotification(event model.BalanceReminderDelivery, language string) (dto.Notify, error) {
	balance, err := decimal.NewFromString(event.After.Nanos)
	if err != nil || !balance.Equal(balance.Truncate(0)) || event.After.Currency != event.Threshold.Currency {
		return dto.Notify{}, errors.New("balance reminder snapshot is invalid")
	}
	title := i18n.MsgBalanceReminderWalletTitle
	if event.Scope == "subscription" {
		title = i18n.MsgBalanceReminderSubscriptionTitle
	}
	params := map[string]any{
		"Time":      time.UnixMilli(event.CreatedAt).UTC().Format(time.RFC3339Nano),
		"Balance":   balance.Shift(-9).String() + " " + string(event.After.Currency),
		"Threshold": event.Threshold.Value().String() + " " + string(event.Threshold.Currency),
	}
	return dto.NewNotify(model.SystemTaskTypeBalanceReminder, i18n.Translate(language, title), i18n.Translate(language, i18n.MsgBalanceReminderContent, params), nil), nil
}
