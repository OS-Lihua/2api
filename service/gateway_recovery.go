package service

import (
	"context"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/model"
)

// GatewayRecoveryHandler retries primary settlement intents and log delivery
// with the existing multi-node scheduled-task locking and injected clock.
type GatewayRecoveryHandler struct{}

func (GatewayRecoveryHandler) Type() string            { return "gateway_settlement" }
func (GatewayRecoveryHandler) Enabled() bool           { return true }
func (GatewayRecoveryHandler) Interval() time.Duration { return time.Minute }
func (GatewayRecoveryHandler) NewPayload() any         { return nil }
func (GatewayRecoveryHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	completed, settlementErr := model.RecoverGatewaySettlements(ctx, 100)
	taskCompleted, taskErr := RecoverGatewayTasks(ctx, 100)
	completed += taskCompleted
	if err := errors.Join(settlementErr, taskErr); err != nil {
		failSystemTask(task, runnerID, err)
		return
	}
	if err := model.FinishSystemTask(task.TaskID, runnerID, model.SystemTaskStatusSucceeded, map[string]int{"completed": completed}, ""); err != nil {
		logSystemTaskLockError(ctx, task, err)
	}
}

func init() { RegisterSystemTaskHandler(GatewayRecoveryHandler{}) }
