package service

import (
	"context"
	"errors"
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
	if limit <= 0 {
		return 0, nil
	}
	tasks, err := model.RecoverableGatewayTasks(ctx, limit)
	if err != nil {
		return 0, err
	}
	completed := 0
	var failures error
	for i := range tasks {
		if err := ctx.Err(); err != nil {
			return completed, errors.Join(failures, err)
		}
		task := &tasks[i]
		var taskErr error
		if task.Status == model.TaskStatusSuccess || task.Status == model.TaskStatusFailure {
			taskErr = SettleGatewayTask(ctx, task)
			if taskErr == nil {
				completed++
			}
		} else {
			taskErr = PublishGatewayTask(task)
		}
		failures = errors.Join(failures, taskErr)
		// Rotate attempted work behind older queued work, even when one task's
		// facts cannot yet be evaluated or its log projection is unavailable.
		err := model.DB.WithContext(ctx).Model(&model.GatewayBilling{}).
			Where("request_id = ? AND phase <> ?", task.PrivateData.Gateway.Settlement.Request.ID, "settled").
			Update("updated_at", clock.Now().UnixMilli()).Error
		failures = errors.Join(failures, err)
	}
	return completed, failures
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
	if snapshot == nil || snapshot.Settlement.Request.UserID != task.UserId {
		return fmt.Errorf("task billing identity is inconsistent")
	}
	if task.Status != model.TaskStatusSuccess && task.Status != model.TaskStatusFailure {
		return fmt.Errorf("task is not terminal")
	}
	var billing model.GatewayBilling
	if err := model.DB.WithContext(ctx).Select("phase").Where("request_id = ? AND user_id = ?", snapshot.Settlement.Request.ID, task.UserId).Take(&billing).Error; err != nil {
		return err
	}
	if billing.Phase != "reserved" {
		// Prepared facts are immutable, including unresolved evaluations.
		if _, err := model.SettleGateway(snapshot.Settlement.Request.ID); err != nil {
			return err
		}
		return model.PublishGatewayRecords(snapshot.Settlement.Request.ID)
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
	if facts == nil {
		facts = make(map[string]any)
	}
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
	if task.Status == model.TaskStatusSuccess {
		request.Status, attempt.Status = "success", "success"
		ratio, saleErr := decimal.NewFromString(request.GroupRatio)
		var fx *money.Rate
		if request.SaleCurrency != model.SettlementCurrency() {
			rate, err := decimal.NewFromString(request.SaleFXRate)
			if err == nil {
				quote, quoteErr := money.NewRate(request.SaleCurrency, model.SettlementCurrency(), rate)
				fx, err = &quote, quoteErr
			}
			saleErr = errors.Join(saleErr, err)
		}
		trace := billingexpr.MoneyTraceResult{Currency: request.SaleCurrency}
		if saleErr == nil {
			price := &model.PriceVersion{Expression: request.SaleExpression, Currency: request.SaleCurrency}
			source, converted, evaluated, err := GatewayPrice(price, nil, billingexpr.RequestInput{Usage: facts, MoneySnapshot: snapshot.SaleConditions}, time.UnixMilli(request.StartedAt), ratio, fx)
			trace, saleErr = evaluated, err
			if saleErr == nil {
				intent.SaleSource, saleErr = source.Receivable()
			}
			if saleErr == nil {
				request.Receivable, saleErr = converted.Receivable()
			}
		}
		trace.MatchedTier = ""
		intent.SaleTrace = &trace
		if saleErr != nil {
			request.SalePending = true
			request.Receivable = money.Amount{Currency: model.SettlementCurrency()}
			intent.SaleEvaluationFailed = true
		}
	}
	// Failed delivery owes no sale. Procurement uses reported quantities;
	// requested quantities cannot confirm what an unsuccessful Provider billed.
	costFacts := facts
	if task.Status == model.TaskStatusFailure {
		costFacts = maps.Clone(snapshot.CompletionFacts)
		if costFacts == nil {
			costFacts = make(map[string]any)
		}
		for name, value := range facts {
			switch value.(type) {
			case string, bool:
				costFacts[name] = value
			}
		}
	}
	var purchaseFX *money.Rate
	var fxErr error
	if attempt.Cost.Currency != model.SettlementCurrency() {
		rate, err := decimal.NewFromString(attempt.AccountingFXRate)
		fxErr = err
		if err == nil {
			quote, err := money.NewRate(attempt.Cost.Currency, model.SettlementCurrency(), rate)
			purchaseFX, fxErr = &quote, err
		}
	}
	purchase := &model.PriceVersion{Expression: attempt.PurchaseExpression, Currency: attempt.Cost.Currency}
	cost, converted, trace, priceErr := GatewayPrice(purchase, nil, billingexpr.RequestInput{Usage: costFacts, MoneySnapshot: snapshot.PurchaseConditions}, time.UnixMilli(attempt.StartedAt), decimal.NewFromInt(1), purchaseFX)
	trace.MatchedTier = ""
	intent.AttemptTrace = map[string]billingexpr.MoneyTraceResult{attempt.ID: trace}
	originalCost, originalErr := cost.Cost()
	attempt.CostPending = originalErr != nil || originalCost.Currency != purchase.Currency
	if !attempt.CostPending {
		attempt.Cost = originalCost
	}
	convertedCost, convertedErr := converted.Cost()
	attempt.ConvertedCostPending = fxErr != nil || convertedErr != nil || convertedCost.Currency != model.SettlementCurrency()
	if !attempt.ConvertedCostPending {
		attempt.ConvertedCost = convertedCost
	}
	intent.EvaluationFailed = intent.SaleEvaluationFailed || priceErr != nil || attempt.CostPending || attempt.ConvertedCostPending
	if intent.EvaluationFailed {
		request.Status = "settlement_pending"
		request.ErrorCode = "billing_evaluation_failed"
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
