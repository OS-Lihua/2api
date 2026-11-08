package service

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/shopspring/decimal"
)

// GatewayTaskProvider resolves credentials within the task's original Provider.
// Operational address, keys, proxy and headers stay in Provider configuration.
func GatewayTaskProvider(task *model.Task) (*model.Provider, error) {
	snapshot := task.PrivateData.Gateway
	if snapshot == nil {
		return model.CacheGetProvider(task.ChannelId)
	}
	if snapshot.ProviderID <= 0 || snapshot.ProviderID != task.ChannelId || snapshot.ChannelID <= 0 || snapshot.Settlement.Attempt == nil || snapshot.Settlement.Attempt.ProviderID != snapshot.ProviderID || snapshot.Settlement.Attempt.ChannelID != snapshot.ChannelID {
		return nil, fmt.Errorf("task Provider identity is inconsistent")
	}
	provider, err := model.GetProviderById(snapshot.ProviderID, true)
	if err != nil {
		return nil, fmt.Errorf("task Provider is unavailable")
	}
	if provider.Type != snapshot.ProviderType {
		return nil, fmt.Errorf("task Provider identity has changed")
	}
	key, _, keyErr := provider.GetNextEnabledKey()
	if keyErr != nil || key == "" {
		return nil, fmt.Errorf("task Provider has no available credential")
	}
	pinned := *provider
	pinned.Key = key
	pinned.ProviderInfo.IsMultiKey = false
	return &pinned, nil
}

// UpdateGatewayTask polls one accepted task using its original Provider.
// Identical private task IDs at other Providers cannot collide.
func UpdateGatewayTask(ctx context.Context, task *model.Task) error {
	provider, err := GatewayTaskProvider(task)
	if err != nil {
		return err
	}
	adaptor := GetTaskAdaptorFunc(task.Platform)
	if adaptor == nil {
		return fmt.Errorf("task plugin is unavailable")
	}
	upstreamID := task.GetUpstreamTaskID()
	tasks := map[string]*model.Task{upstreamID: task}
	if batch, ok := adaptor.(BatchTaskPollingAdaptor); ok && batch.FetchMode() == "batch" {
		return updateBatchTasks(ctx, batch, provider.Id, []string{upstreamID}, tasks, provider)
	}
	adaptor.Init(&relaycommon.RelayInfo{ProviderMeta: &relaycommon.ProviderMeta{
		ChannelType: provider.Type, ChannelId: provider.Id, ChannelBaseUrl: provider.GetBaseURL(), ChannelSetting: provider.GetSetting(), HeadersOverride: provider.GetHeaderOverride(), ApiKey: provider.Key,
	}})
	return updateVideoSingleTask(ctx, adaptor, provider, upstreamID, tasks)
}

// RecoverGatewayTasks retries terminal task settlement from durable facts.
func RecoverGatewayTasks(ctx context.Context, limit int) (int, error) {
	tasks, err := model.RecoverableGatewayTasks(ctx, limit)
	if err != nil {
		return 0, err
	}
	completed := 0
	for i := range tasks {
		if err := ctx.Err(); err != nil {
			return completed, err
		}
		if tasks[i].Status != model.TaskStatusSuccess && tasks[i].Status != model.TaskStatusFailure {
			if err := PublishGatewayTask(&tasks[i]); err != nil {
				return completed, err
			}
			continue
		}
		if err := SettleGatewayTask(ctx, &tasks[i]); err != nil {
			return completed, err
		}
		completed++
	}
	return completed, nil
}

// PublishGatewayTask projects durable submission state into the existing
// request and Attempt views. It never posts a ledger entry.
func PublishGatewayTask(task *model.Task) error {
	snapshot := task.PrivateData.Gateway
	if snapshot == nil {
		return fmt.Errorf("task has no gateway snapshot")
	}
	intent := snapshot.Settlement
	intent.Request.Status = "pending"
	if !snapshot.Accepted {
		intent.Request.Status = "submission_unknown"
	}
	intent.Request.Usage.TaskFacts = snapshot.Facts
	intent.Request.UsageSource = "estimated"
	if intent.Attempt != nil {
		attempt := *intent.Attempt
		attempt.Status = intent.Request.Status
		attempt.EndedAt = 0
		attempt.Usage.TaskFacts = snapshot.Facts
		attempt.UsageSource = "estimated"
		intent.Attempts = append(append([]model.GatewayAttempt(nil), snapshot.PreviousAttempts...), attempt)
		intent.Attempt = nil
	}
	return model.PublishPendingGatewayRecords(intent)
}

// SettleGatewayTask uses one plugin fact vector for both sides of the accepted
// task. It replays the existing immutable settlement lifecycle on every retry.
func SettleGatewayTask(ctx context.Context, task *model.Task) error {
	snapshot := task.PrivateData.Gateway
	if snapshot == nil {
		return fmt.Errorf("task has no gateway snapshot")
	}
	intent := snapshot.Settlement
	request := &intent.Request
	if intent.Attempt == nil {
		return fmt.Errorf("task has no accepted attempt")
	}
	acceptedAttempt := *intent.Attempt
	intent.Attempt = &acceptedAttempt
	attempt := intent.Attempt
	facts := maps.Clone(snapshot.Facts)
	maps.Copy(facts, snapshot.CompletionFacts)
	request.EndedAt = clock.Now().UnixMilli()
	request.Status = "failed"
	request.UsageSource = "upstream"
	for key := range billingexpr.UsedUsageKeys(request.SaleExpression) {
		if _, reported := snapshot.CompletionFacts[key]; !reported {
			request.UsageSource = "estimated"
		}
	}
	request.Usage.TaskFacts = facts
	request.Receivable = money.Amount{Currency: model.SettlementCurrency()}
	intent.SaleSource = money.Amount{Currency: request.SaleCurrency}
	attempt.EndedAt = request.EndedAt
	attempt.Status = "failed"
	attempt.Usage.TaskFacts = facts
	attempt.UsageSource = "upstream"
	for key := range billingexpr.UsedUsageKeys(attempt.PurchaseExpression) {
		if _, reported := snapshot.CompletionFacts[key]; !reported {
			attempt.UsageSource = "estimated"
		}
	}
	at := time.UnixMilli(request.StartedAt)
	if task.Status == model.TaskStatusSuccess {
		source, _, err := billingexpr.RunMoneyExpr(request.SaleExpression, billingexpr.MoneyInput{TaskUsage: true, Request: billingexpr.RequestInput{Usage: facts}, Time: at}, request.SaleCurrency)
		if err != nil {
			return err
		}
		ratio, err := decimal.NewFromString(request.GroupRatio)
		if err != nil {
			return err
		}
		source, err = money.MulUnitPrice(ratio, source)
		if err != nil {
			return err
		}
		intent.SaleSource, err = source.Receivable()
		if err != nil {
			return err
		}
		receivable := source
		if request.SaleCurrency != model.SettlementCurrency() {
			rate, parseErr := decimal.NewFromString(request.SaleFXRate)
			if parseErr != nil {
				return parseErr
			}
			fx, rateErr := money.NewRate(request.SaleCurrency, model.SettlementCurrency(), rate)
			if rateErr != nil {
				return rateErr
			}
			receivable, err = source.Convert(fx)
			if err != nil {
				return err
			}
		}
		request.Receivable, err = receivable.Receivable()
		if err != nil {
			return err
		}
		request.Status = "success"
		attempt.Status = "success"
	}
	// A failed task is free to the user; any completion facts reported by the
	// Provider still incur its purchase price.
	if task.Status == model.TaskStatusSuccess || len(snapshot.CompletionFacts) > 0 {
		cost, _, err := billingexpr.RunMoneyExpr(attempt.PurchaseExpression, billingexpr.MoneyInput{TaskUsage: true, Request: billingexpr.RequestInput{Usage: facts}, Time: time.UnixMilli(attempt.StartedAt)}, attempt.Cost.Currency)
		if err != nil {
			return err
		}
		attempt.Cost, err = cost.Cost()
		if err != nil {
			return err
		}
		converted := cost
		if attempt.Cost.Currency != model.SettlementCurrency() {
			rate, parseErr := decimal.NewFromString(attempt.AccountingFXRate)
			if parseErr != nil {
				return parseErr
			}
			fx, rateErr := money.NewRate(attempt.Cost.Currency, model.SettlementCurrency(), rate)
			if rateErr != nil {
				return rateErr
			}
			converted, err = cost.Convert(fx)
			if err != nil {
				return err
			}
		}
		attempt.ConvertedCost, err = converted.Cost()
		if err != nil {
			return err
		}
	}
	if len(snapshot.PreviousAttempts) > 0 {
		intent.Attempts = append(append([]model.GatewayAttempt(nil), snapshot.PreviousAttempts...), *attempt)
		intent.Attempt = nil
	}
	if err := model.PrepareGatewaySettlement(intent); err != nil {
		return err
	}
	if _, err := model.SettleGateway(request.ID); err != nil {
		return err
	}
	return model.PublishGatewayRecords(request.ID)
}
