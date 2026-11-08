package service

import (
	"context"
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
	taskCompleted, err := RecoverGatewayTasks(ctx, 100)
	if err != nil {
		failSystemTask(task, runnerID, err)
		return
	}
	completed, err := model.RecoverGatewaySettlements(ctx, 100)
	completed += taskCompleted
	if err != nil {
		failSystemTask(task, runnerID, err)
		return
	}
	if err := model.FinishSystemTask(task.TaskID, runnerID, model.SystemTaskStatusSucceeded, map[string]int{"completed": completed}, ""); err != nil {
		logSystemTaskLockError(ctx, task, err)
	}
}

func init() { RegisterSystemTaskHandler(GatewayRecoveryHandler{}) }
