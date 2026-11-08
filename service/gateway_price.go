package service

import (
	"context"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/money"
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
func GatewayPrice(price *model.PriceVersion, usage *dto.Usage, request billingexpr.RequestInput, at time.Time, ratio decimal.Decimal, rate *money.Rate) (money.Value, money.Value, billingexpr.MoneyTraceResult, error) {
	used, err := billingexpr.MoneyUsedVars(price.Expression)
	if err != nil {
		return money.Value{}, money.Value{}, billingexpr.MoneyTraceResult{}, err
	}
	quantities, err := billingexpr.MoneyTokenQuantities(usage, used)
	if err != nil {
		return money.Value{}, money.Value{}, billingexpr.MoneyTraceResult{}, err
	}
	value, trace, err := billingexpr.RunMoneyExpr(price.Expression, billingexpr.MoneyInput{TokenQuantities: quantities, Request: request, Time: at}, price.Currency)
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

func GatewayRecordUsage(usage *dto.Usage) model.GatewayUsage {
	if usage == nil {
		return model.GatewayUsage{}
	}
	return model.GatewayUsage{PromptTokens: int64(usage.PromptTokens), CompletionTokens: int64(usage.CompletionTokens), TotalTokens: int64(usage.PromptTokens) + int64(usage.CompletionTokens), CachedTokens: int64(usage.PromptTokensDetails.CachedTokens), CacheCreationTokens: int64(usage.PromptTokensDetails.CacheCreationTokensTotal()), CacheCreation12hTokens: int64(usage.PromptTokensDetails.CacheWrite12hTokens()), ImageInputTokens: int64(usage.PromptTokensDetails.ImageTokens), AudioInputTokens: int64(usage.PromptTokensDetails.AudioTokens), ImageOutputTokens: int64(usage.CompletionTokenDetails.ImageTokens), AudioOutputTokens: int64(usage.CompletionTokenDetails.AudioTokens)}
}
