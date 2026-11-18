package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/money"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

type gatewayTaskDeliveryWriter struct {
	gin.ResponseWriter
	written int
	failed  bool
}

func (writer *gatewayTaskDeliveryWriter) Write(body []byte) (int, error) {
	n, err := writer.ResponseWriter.Write(body)
	writer.written += n
	if err != nil || n != len(body) {
		writer.failed = true
		if err == nil {
			err = io.ErrShortWrite
		}
	}
	return n, err
}

func (writer *gatewayTaskDeliveryWriter) WriteString(body string) (int, error) {
	return writer.Write([]byte(body))
}

// BeginGatewayTaskDelivery records one immutable delivery outcome for an inline
// business result, regardless of which public presenter delivers it. Call finish
// after presentation; nested observations must finish in reverse order.
func BeginGatewayTaskDelivery(c *gin.Context, task *model.Task) func(bool) {
	if task == nil || task.PrivateData.Gateway == nil || task.PrivateData.Gateway.Stream == nil || !task.PrivateData.Gateway.Stream.Pending {
		return func(bool) {}
	}
	writer := &gatewayTaskDeliveryWriter{ResponseWriter: c.Writer}
	c.Writer = writer
	return func(presented bool) {
		c.Writer = writer.ResponseWriter
		delivered := presented && !writer.failed && writer.written > 0 && writer.Status()/100 == 2 && task.Status == model.TaskStatusSuccess
		ctx := context.WithoutCancel(c.Request.Context())
		persisted, err := model.CompleteGatewayTaskDelivery(ctx, task.ID, delivered)
		if err == nil {
			_ = SettleGatewayTask(ctx, persisted)
		}
	}
}

// GatewayTaskProviderIdentity resolves the original Provider configuration without
// selecting a key. New operations retain its multi-key slot identity at dispatch.
func GatewayTaskProviderIdentity(task *model.Task) (*model.Provider, error) {
	snapshot := task.PrivateData.Gateway
	if legacy := task.PrivateData.LegacyObligation; legacy != nil {
		if snapshot != nil || task.Platform != "midjourney" || legacy.ProviderID <= 0 || legacy.ProviderID != task.ChannelId || legacy.ProviderType <= 0 || task.PrivateData.UpstreamTaskID == "" {
			return nil, fmt.Errorf("task Provider identity is inconsistent")
		}
		provider, err := model.GetProviderById(legacy.ProviderID, true)
		if err != nil || provider.Type != legacy.ProviderType {
			return nil, fmt.Errorf("task Provider identity is unavailable")
		}
		return provider, nil
	}
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
	return provider, nil
}

// GatewayTaskProvider resolves one enabled credential for a persisted operation.
// Operational address, keys, proxy and headers stay in Provider configuration.
func GatewayTaskProvider(task *model.Task) (*model.Provider, error) {
	provider, _, err := GatewayTaskProviderWithContext(context.Background(), task)
	return provider, err
}

// GatewayTaskProviderWithContext selects one current credential and retains its
// actual slot for polling and content preparation. Later I/O rechecks this slot.
func GatewayTaskProviderWithContext(ctx context.Context, task *model.Task) (*model.Provider, int, error) {
	if task == nil {
		return nil, 0, model.ErrProviderStatusConflict
	}
	if task.PrivateData.Gateway == nil && task.PrivateData.LegacyObligation == nil {
		provider, key, index, err := legacyTaskProvider(ctx, task)
		if err != nil {
			return nil, 0, err
		}
		pinned := *provider
		pinned.Key = key
		pinned.ProviderInfo.IsMultiKey = false
		return &pinned, index, nil
	}
	provider, err := GatewayTaskProviderIdentity(task)
	if err != nil {
		return nil, 0, err
	}
	key, index, keyErr := provider.PreviewEnabledKey()
	if keyErr != nil || key == "" {
		return nil, 0, fmt.Errorf("task Provider has no available credential")
	}
	if err := provider.CommitRouteKey(ctx, key, index); err != nil {
		return nil, 0, err
	}
	pinned := *provider
	pinned.Key = key
	pinned.ProviderInfo.IsMultiKey = false
	return &pinned, index, nil
}

// GatewayTaskCredentialCheck retains only a request-scoped authority check for
// the original Provider and the credential actually chosen for this operation.
// It never stores credentials on the task or substitutes a newly configured key.
func GatewayTaskCredentialCheck(task *model.Task, key string, index int) func(context.Context) error {
	if task == nil {
		return nil
	}
	if task.PrivateData.Gateway == nil && task.PrivateData.LegacyObligation == nil {
		return func(ctx context.Context) error {
			_, expectedKey, expectedIndex, err := legacyTaskProvider(ctx, task)
			if err != nil || expectedKey != key || expectedIndex != index {
				return model.ErrProviderStatusConflict
			}
			return nil
		}
	}
	provider, err := GatewayTaskProviderIdentity(task)
	return func(ctx context.Context) error {
		if err != nil || index < 0 {
			return model.ErrProviderStatusConflict
		}
		return provider.ValidateRouteKey(ctx, key, index)
	}
}

// GatewayTaskHTTPTransport validates the same authority before each network
// exchange, including redirects and plugin helper fetches.
func GatewayTaskHTTPTransport(transport http.RoundTripper, authorize func(context.Context) error) http.RoundTripper {
	if authorize == nil {
		return transport
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	return gatewayTaskHTTPTransport{RoundTripper: transport, authorize: authorize}
}

type gatewayTaskHTTPTransport struct {
	http.RoundTripper
	authorize func(context.Context) error
}

func (transport gatewayTaskHTTPTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if err := transport.authorize(request.Context()); err != nil {
		return nil, err
	}
	return transport.RoundTripper.RoundTrip(request)
}

type taskPollingPluginIdentity interface {
	TaskPluginIdentity() (string, string)
}

func legacyTaskProvider(ctx context.Context, task *model.Task) (*model.Provider, string, int, error) {
	verified, err := model.ValidateLegacyTaskPolling(ctx, task)
	if err != nil || !verified {
		return nil, "", 0, errors.New("legacy task evidence is not available for polling")
	}
	provider, err := model.GetProviderById(task.ChannelId, true)
	if err != nil {
		return nil, "", 0, errors.New("legacy task Provider is unavailable")
	}
	identity := task.PrivateData.Execution.TaskPlugin
	plugin, ok := jsplugin.DefaultRegistry.Generation().Get(identity.Key)
	if !ok || plugin.Meta.Version != identity.Version {
		return nil, "", 0, errors.New("legacy task plugin is unavailable")
	}
	if provider.Type == constant.ChannelTypeTaskPlugin || provider.Type == constant.ChannelTypeNewAPI {
		if !provider.GetSetting().BindsTaskPlugin(identity.Key) {
			return nil, "", 0, errors.New("legacy task Provider plugin binding has changed")
		}
	} else if !slices.Contains(plugin.Meta.ChannelTypes, provider.Type) {
		return nil, "", 0, errors.New("legacy task Provider plugin binding has changed")
	}
	key := task.PrivateData.Key
	index := 0
	if key == "" {
		// A sole current key is not evidence of the credential that created
		// this historical task. Do not query another upstream identity.
		return nil, "", 0, errors.New("legacy task original credential is unavailable")
	} else if provider.ProviderInfo.IsMultiKey {
		index = slices.Index(provider.GetKeys(), key)
	}
	if key == "" || index < 0 || provider.ValidateRouteKey(ctx, key, index) != nil {
		return nil, "", 0, errors.New("legacy task original credential is unavailable")
	}
	return provider, key, index, nil
}

// UpdateLegacyTask collects business facts only, with immutable source identity
// and one task per query. It does not manufacture a Gateway or money contract.
func UpdateLegacyTask(ctx context.Context, task *model.Task) error {
	provider, key, index, err := legacyTaskProvider(ctx, task)
	if err != nil {
		return err
	}
	adaptor := GetTaskAdaptorFunc(task.Platform)
	identity, ok := adaptor.(taskPollingPluginIdentity)
	if !ok {
		return errors.New("legacy task plugin identity is unavailable")
	}
	pluginKey, version := identity.TaskPluginIdentity()
	source := task.PrivateData.Execution.TaskPlugin
	if pluginKey != source.Key || version != source.Version {
		return errors.New("legacy task plugin identity has changed")
	}
	pinned := *provider
	pinned.Key = key
	pinned.ProviderInfo.IsMultiKey = false
	upstreamID := task.GetUpstreamTaskID()
	tasks := map[string]*model.Task{upstreamID: task}
	if batch, ok := adaptor.(BatchTaskPollingAdaptor); ok && batch.FetchMode() == "batch" {
		return updateBatchTasks(ctx, batch, provider.Id, []string{upstreamID}, tasks, gatewayTaskProviderSelection{Provider: &pinned, KeyIndex: index})
	}
	adaptor.Init(&relaycommon.RelayInfo{ProviderMeta: &relaycommon.ProviderMeta{
		ChannelType: provider.Type, ChannelId: provider.Id, ChannelBaseUrl: provider.GetBaseURL(), ChannelSetting: provider.GetSetting(), HeadersOverride: provider.GetHeaderOverride(), ApiKey: key, ChannelMultiKeyIndex: index,
	}})
	return updateVideoSingleTask(ctx, adaptor, &pinned, upstreamID, tasks)
}

// UpdateGatewayTask polls one accepted task using its original Provider.
// Identical private task IDs at other Providers cannot collide.
func UpdateGatewayTask(ctx context.Context, task *model.Task) error {
	provider, keyIndex, err := GatewayTaskProviderWithContext(ctx, task)
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
		return updateBatchTasks(ctx, batch, provider.Id, []string{upstreamID}, tasks, gatewayTaskProviderSelection{Provider: provider, KeyIndex: keyIndex})
	}
	adaptor.Init(&relaycommon.RelayInfo{ProviderMeta: &relaycommon.ProviderMeta{
		ChannelType: provider.Type, ChannelId: provider.Id, ChannelBaseUrl: provider.GetBaseURL(), ChannelSetting: provider.GetSetting(), HeadersOverride: provider.GetHeaderOverride(), ApiKey: provider.Key, ChannelMultiKeyIndex: keyIndex,
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
	intent.Request.Usage.BillingUsageSchema = snapshot.BillingUsageSchema
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
		attempt.Usage.BillingUsageSchema = snapshot.BillingUsageSchema
		attempt.UsageSource = "estimated"
		intent.Attempts = append(append([]model.GatewayAttempt(nil), snapshot.PreviousAttempts...), attempt)
		intent.Attempt = nil
	}
	return model.PublishPendingGatewayRecords(intent)
}

// gatewayTaskUsageSource distinguishes reported facts from retained estimates,
// including the image_count scalar alias of the same task quantity.
func gatewayTaskUsageSource(expression string, completed map[string]any) string {
	keys := billingexpr.UsedUsageKeys(expression)
	used, err := billingexpr.MoneyUsedVars(expression)
	if err != nil {
		return "estimated"
	}
	if used["image_count"] {
		if _, reported := completed["image_count"]; !reported {
			return "estimated"
		}
	}
	for key := range keys {
		if _, reported := completed[key]; !reported {
			return "estimated"
		}
	}
	return "upstream"
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
	if snapshot.Stream != nil && snapshot.Stream.Pending {
		return fmt.Errorf("task delivery outcome is unresolved")
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
	intent.Request.Usage.BillingUsageSchema = snapshot.BillingUsageSchema
	request := &intent.Request
	if intent.Attempt == nil {
		return fmt.Errorf("task has no accepted attempt")
	}
	acceptedAttempt := *intent.Attempt
	intent.Attempt = &acceptedAttempt
	attempt := intent.Attempt
	attempt.Usage.BillingUsageSchema = snapshot.BillingUsageSchema
	facts := maps.Clone(snapshot.Facts)
	if facts == nil {
		facts = make(map[string]any)
	}
	incomplete := snapshot.Stream != nil && snapshot.Stream.Incomplete && attempt.ErrorClass != "channel_unavailable"
	if snapshot.CompletionUnknown && snapshot.Stream == nil {
		// Estimated quantities cannot replace an omitted or invalid completion.
		// Preserve validated completion facts and host-enforced Channel inputs.
		for name, value := range facts {
			switch value.(type) {
			case string, bool:
			default:
				delete(facts, name)
			}
		}
		maps.Copy(facts, snapshot.RetainedFacts)
	}
	if snapshot.Stream != nil {
		for name, value := range facts {
			switch value.(type) {
			case string, bool:
			default:
				delete(facts, name)
			}
		}
	}
	maps.Copy(facts, snapshot.CompletionFacts)
	saleFacts := facts
	if snapshot.Stream != nil {
		saleFacts = make(map[string]any)
		for name, value := range facts {
			switch value.(type) {
			case string, bool:
				saleFacts[name] = value
			}
		}
		maps.Copy(saleFacts, snapshot.Stream.Delivered)
	}
	request.EndedAt = clock.Now().UnixMilli()
	request.Status = "failed"
	request.UsageSource = gatewayTaskUsageSource(request.SaleExpression, snapshot.CompletionFacts)
	request.Usage.TaskFacts = saleFacts
	request.Receivable = money.Amount{Currency: model.SettlementCurrency()}
	intent.SaleSource = money.Amount{Currency: request.SaleCurrency}
	attempt.EndedAt = request.EndedAt
	attempt.Status = "failed"
	attempt.Usage.TaskFacts = facts
	attempt.UsageSource = gatewayTaskUsageSource(attempt.PurchaseExpression, snapshot.CompletionFacts)
	if task.Status == model.TaskStatusSuccess && (snapshot.Stream == nil || snapshot.Stream.BusinessOutput) {
		partial := (snapshot.Stream != nil && (snapshot.Stream.Incomplete || snapshot.Stream.DeliveryFailed)) || attempt.Status == "partial"
		if partial {
			request.Status, attempt.Status = "partial", "partial"
		} else {
			request.Status, attempt.Status = "success", "success"
		}
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
			source, converted, evaluated, err := GatewayPrice(price, nil, billingexpr.RequestInput{Usage: saleFacts, MoneySnapshot: snapshot.SaleConditions, RequireMoneySnapshot: true}, time.UnixMilli(request.StartedAt), ratio, fx)
			trace, saleErr = evaluated, err
			if saleErr == nil {
				intent.SaleSource, saleErr = source.Receivable()
			}
			if saleErr == nil {
				request.Receivable, saleErr = converted.Receivable()
			}
		}
		trace.MatchedTier = ""
		trace.RequestSnapshot = snapshot.SaleConditions
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
	if task.Status == model.TaskStatusFailure || snapshot.Stream != nil {
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
	cost, converted, trace, priceErr := GatewayPrice(purchase, nil, billingexpr.RequestInput{Usage: costFacts, MoneySnapshot: snapshot.PurchaseConditions, RequireMoneySnapshot: true}, time.UnixMilli(attempt.StartedAt), decimal.NewFromInt(1), purchaseFX)
	trace.MatchedTier = ""
	trace.RequestSnapshot = snapshot.PurchaseConditions
	intent.AttemptTrace = maps.Clone(intent.AttemptTrace)
	if intent.AttemptTrace == nil {
		intent.AttemptTrace = make(map[string]billingexpr.MoneyTraceResult)
	}
	intent.AttemptTrace[attempt.ID] = trace
	originalCost, originalErr := cost.Cost()
	originalKnown := originalErr == nil && originalCost.Currency == purchase.Currency
	if originalKnown {
		attempt.Cost = originalCost
	}
	attempt.CostPending = !originalKnown || incomplete
	convertedCost, convertedErr := converted.Cost()
	convertedKnown := fxErr == nil && convertedErr == nil && convertedCost.Currency == model.SettlementCurrency()
	if convertedKnown {
		attempt.ConvertedCost = convertedCost
	} else {
		attempt.ConvertedCost = money.Amount{Currency: model.SettlementCurrency()}
	}
	attempt.ConvertedCostPending = !convertedKnown || incomplete
	intent.EvaluationFailed = intent.SaleEvaluationFailed || priceErr != nil || attempt.CostPending || attempt.ConvertedCostPending
	for _, previous := range snapshot.PreviousAttempts {
		intent.EvaluationFailed = intent.EvaluationFailed || previous.CostPending || previous.ConvertedCostPending
	}
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
