package controller

import (
	"context"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
)

// RegisterScheduledSystemTasks registers active polling and reconciliation jobs,
// plus handlers that close queued legacy maintenance jobs without executing them.
// Call this before service.StartSystemTaskRunner.
func RegisterScheduledSystemTasks() {
	service.RegisterSystemTaskHandler(providerTestHandler{})
	service.RegisterSystemTaskHandler(modelUpdateHandler{})
	service.RegisterSystemTaskHandler(midjourneyPollHandler{})
	service.RegisterSystemTaskHandler(asyncTaskPollHandler{})
	service.RegisterSystemTaskHandler(service.WalletReconcileHandler{})
	service.RegisterSystemTaskHandler(service.BalanceReminderHandler{})
}

// providerTestHandler preserves queued job history without calling the retired
// Provider-model test chain. New Channels use their explicit probe API.
type providerTestHandler struct{}

func (providerTestHandler) Type() string { return model.SystemTaskTypeChannelTest }

func (providerTestHandler) Enabled() bool { return false }

func (providerTestHandler) Interval() time.Duration { return 0 }

func (providerTestHandler) NewPayload() any { return nil }

// providerTestTaskPayload retains the format of historical channel_test jobs.
type providerTestTaskPayload struct {
	Mode   string `json:"mode,omitempty"`
	Notify bool   `json:"notify,omitempty"`
}

func (providerTestHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	if ctx != nil && ctx.Err() != nil {
		return
	}
	finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil,
		common.NewMessage("Legacy Channel management has been retired. Use the Provider and Channel APIs instead."))
}

// modelUpdateHandler closes queued legacy discovery jobs without rewriting
// Provider.Models or abilities. New Channel imports require explicit mappings.
type modelUpdateHandler struct{}

func (modelUpdateHandler) Type() string { return model.SystemTaskTypeModelUpdate }

func (modelUpdateHandler) Enabled() bool { return false }

func (modelUpdateHandler) Interval() time.Duration { return 0 }

func (modelUpdateHandler) NewPayload() any { return nil }

// modelUpdateTaskPayload retains the format of historical model_update jobs.
type modelUpdateTaskPayload struct {
	Manual bool `json:"manual,omitempty"`
}

func (modelUpdateHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	if ctx != nil && ctx.Err() != nil {
		return
	}
	finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil,
		common.NewMessage("Legacy Channel management has been retired. Use the Provider and Channel APIs instead."))
}

// midjourneyPollHandler closes queued legacy jobs without querying Providers or
// changing historical task/accounting facts. Migrated tasks use async_task_poll.
type midjourneyPollHandler struct{}

func (midjourneyPollHandler) Type() string { return model.SystemTaskTypeMidjourneyPoll }

func (midjourneyPollHandler) Enabled() bool { return false }

func (midjourneyPollHandler) Interval() time.Duration { return 0 }

func (midjourneyPollHandler) NewPayload() any { return nil }

func (midjourneyPollHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	if ctx != nil && ctx.Err() != nil {
		return
	}
	finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil,
		common.NewMessage("Legacy Midjourney polling has been retired. Migrate historical tasks before starting the server."))
}

// asyncTaskPollHandler runs one canonical task polling pass per scheduled run.
// Enabled folds in the unfinished task check so an idle system schedules no rows.
type asyncTaskPollHandler struct{}

func (asyncTaskPollHandler) Type() string { return model.SystemTaskTypeAsyncTaskPoll }

func (asyncTaskPollHandler) Enabled() bool {
	return constant.UpdateTask && model.HasUnfinishedSyncTasks()
}

func (asyncTaskPollHandler) Interval() time.Duration { return 15 * time.Second }

func (asyncTaskPollHandler) NewPayload() any { return nil }

func (asyncTaskPollHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	summary := service.RunTaskPollingOnce(ctx, service.NewSystemTaskProgressReporter(task, runnerID))
	finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusSucceeded, summary, nil)
}

func finishSystemTaskHandler(task *model.SystemTask, runnerID string, status model.SystemTaskStatus, result any, runErr error) {
	errorMessage := ""
	if runErr != nil {
		errorMessage = runErr.Error()
	}
	if err := model.FinishSystemTask(task.TaskID, runnerID, status, result, errorMessage); err != nil {
		common.SysLog(common.LogText("system task %s failed to persist result: %v", task.TaskID, err))
	}
}
