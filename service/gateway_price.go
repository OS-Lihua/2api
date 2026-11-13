package service

import (
	"context"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/money"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/shopspring/decimal"
)

// ExactGatewayGroupRatio reads the configured decimal before legacy float
// caches deserialize it. The user group discounts the sale only.
func ExactGatewayGroupRatio(group string) (decimal.Decimal, error) {
	discounts, err := model.ExactGroupDiscounts([]string{group})
	if err != nil {
		return decimal.Decimal{}, err
	}
	return discounts[group], nil
}

// GatewayFX freezes the actual directional quote. Same-currency accounting
// records the identity rate explicitly and never reads a synthetic FX row.
func GatewayFX(ctx context.Context, from money.Currency) (*money.Rate, string, error) {
	if from == model.SettlementCurrency() {
		return nil, "1", nil
	}
	rate, err := model.GetAccountingFXRate(ctx, model.DB, from, model.SettlementCurrency())
	if err != nil {
		return nil, "", err
	}
	return &rate, rate.Decimal().String(), nil
}

// GatewayPrice evaluates one frozen expression and applies the exact discount
// and frozen accounting quote. Rounding belongs to the caller's ledger boundary.
func GatewayPrice(price *model.PriceVersion, usage *dto.Usage, request billingexpr.RequestInput, at time.Time, ratio decimal.Decimal, rate *money.Rate, actualImageCount ...int) (money.Value, money.Value, billingexpr.MoneyTraceResult, error) {
	used, err := billingexpr.MoneyUsedVars(price.Expression)
	if err != nil {
		return money.Value{}, money.Value{}, billingexpr.MoneyTraceResult{}, err
	}
	var quantities map[string]decimal.Decimal
	if request.Usage == nil {
		quantities, err = billingexpr.MoneyTokenQuantities(usage, used)
		if err != nil {
			return money.Value{}, money.Value{}, billingexpr.MoneyTraceResult{}, err
		}
	}
	request.UnmeasuredTokens = billingexpr.NormalizeUnmeasuredOutputTokens(request.UnmeasuredTokens, used)
	input := billingexpr.MoneyInput{TaskUsage: request.Usage != nil, TokenQuantities: quantities, Request: request, Time: at}
	if len(actualImageCount) > 1 {
		return money.Value{}, money.Value{}, billingexpr.MoneyTraceResult{}, errors.New("multiple actual image counts")
	}
	if len(actualImageCount) == 1 {
		input.ImageCount = &actualImageCount[0]
	}
	value, trace, err := billingexpr.RunMoneyExpr(price.Expression, input, price.Currency)
	if err != nil {
		return money.Value{}, money.Value{}, trace, err
	}
	value, err = money.MulUnitPrice(ratio, value)
	if err != nil {
		return money.Value{}, money.Value{}, trace, err
	}
	converted := value
	if rate != nil {
		converted, err = value.Convert(*rate)
	}
	return value, converted, trace, err
}

// GatewayUsageSnapshot retains quantitative pricing facts without provider
// extensions, request content or arbitrary source/semantic strings.
func GatewayUsageSnapshot(usage *dto.Usage) *dto.Usage {
	if usage == nil {
		return nil
	}
	snapshot := *usage
	snapshot.Cost, snapshot.BillingUsage = nil, nil
	snapshot.UsageSource = ""
	if snapshot.UsageSemantic != "anthropic" {
		snapshot.UsageSemantic = ""
	}
	snapshot.PromptTokensDetails = usage.PromptTokensDetails.Clone()
	if usage.InputTokensDetails != nil {
		details := usage.InputTokensDetails.Clone()
		snapshot.InputTokensDetails = &details
	}
	if usage.OutputTokensDetails != nil {
		details := *usage.OutputTokensDetails
		snapshot.OutputTokensDetails = &details
	}
	return &snapshot
}

func GatewayRecordUsage(usage *dto.Usage, facts ...*relaycommon.ResponseFacts) model.GatewayUsage {
	if usage == nil {
		return model.GatewayUsage{}
	}
	record := model.GatewayUsage{PromptTokens: int64(usage.PromptTokens), CompletionTokens: int64(usage.CompletionTokens), TotalTokens: int64(usage.PromptTokens) + int64(usage.CompletionTokens), CachedTokens: int64(usage.PromptTokensDetails.CachedTokens), CacheCreationTokens: int64(usage.PromptTokensDetails.CacheCreationTokensTotal()), CacheCreation12hTokens: int64(usage.PromptTokensDetails.CacheWrite12hTokens())}
	source := &relaycommon.ResponseFacts{}
	if len(facts) > 0 && facts[0] != nil {
		source = facts[0]
		if source.ImageOutputTokensUnknown {
			record.UnmeasuredTokens = []string{"c", "img_o"}
		}
		if source.AudioOutputTokensUnknown {
			record.UnmeasuredTokens = []string{"c", "ao"}
		}
		record.ImageCount = source.ImageCount
		record.AudioInputSeconds = source.AudioInputSeconds
		record.AudioOutputSeconds = source.AudioOutputSeconds
	}
	for _, detail := range []struct {
		value  int
		known  bool
		target **int64
	}{
		{usage.PromptTokensDetails.ImageTokens, source.ImageInputTokensReported, &record.ImageInputTokens},
		{usage.PromptTokensDetails.AudioTokens, source.AudioInputTokensReported || source.AudioInputSeconds != nil || source.AudioInputTokensEstimated > 0, &record.AudioInputTokens},
		{usage.CompletionTokenDetails.ImageTokens, source.ImageOutputTokensReported || source.ImageOutputTokensEstimated, &record.ImageOutputTokens},
		{usage.CompletionTokenDetails.AudioTokens, source.AudioOutputTokensReported || source.AudioOutputTokensDerived || source.AudioOutputSeconds != nil, &record.AudioOutputTokens},
	} {
		if detail.value != 0 || detail.known {
			value := int64(detail.value)
			*detail.target = &value
		}
	}
	return record
}
