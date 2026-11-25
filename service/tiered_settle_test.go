package service

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGatewayPriceCacheFallbackAndContext(t *testing.T) {
	const samePrice = `len == 1000 ? tier("base", p * 60 + c * 120) : tier("wrong context", 0)`
	const splitTTL = `tier("base", p * 3 + c * 15 + cc * 3.75 + cc1h * 6)`
	for _, tc := range []struct {
		name       string
		expression string
		usage      dto.Usage
		want       string
	}{
		{"OpenAI cache at input price", samePrice, dto.Usage{PromptTokens: 1000, CompletionTokens: 100, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 200}}, "0.072 USD"},
		{"Claude cache at input price", samePrice, dto.Usage{PromptTokens: 800, CompletionTokens: 100, UsageSemantic: "anthropic", PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 200}}, "0.072 USD"},
		{"Claude incomplete TTL split", splitTTL, dto.Usage{PromptTokens: 100, CompletionTokens: 50, UsageSemantic: "anthropic", ClaudeCacheCreation1hTokens: 4000, PromptTokensDetails: dto.InputTokenDetails{CachedCreationTokens: 10000}}, "0.04755 USD"},
		{"Claude long writes at weighted input price", `tier("base", p * 3)`, dto.Usage{PromptTokens: 100, UsageSemantic: "anthropic", ClaudeCacheCreation5mTokens: 2000, ClaudeCacheCreation1hTokens: 10000, PromptTokensDetails: dto.InputTokenDetails{CachedCreationTokens: 12000}}, "0.0543 USD"},
		{"OpenAI long writes at weighted input price", `tier("base", p * 10 + cr)`, dto.Usage{PromptTokens: 12000, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 4000, CacheWriteTokens: 7000, CacheWriteTokensDetails: &dto.CacheWriteTokenDetails{TTL30mTokens: 3000, TTL12hTokens: 4000}}}, "0.124 USD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			price := model.PriceVersion{Expression: tc.expression, Currency: "USD"}
			amount, _, _, err := GatewayPrice(&price, &tc.usage, billingexpr.RequestInput{}, time.Time{}, decimal.NewFromInt(1), nil)
			require.NoError(t, err)
			assert.Equal(t, tc.want, amount.String())
		})
	}
}

func TestGatewayPriceImageCacheBreakdown(t *testing.T) {
	const expression = `tier("standard", p * 5 + cr * 1.25 + img * 8 + img_cr * 2 + c * 30)`
	for _, tc := range []struct {
		name       string
		details    *dto.CachedTokenDetails
		expression string
		want       string
	}{
		{"mixed cache", &dto.CachedTokenDetails{ImageTokens: common.GetPointer(200), TextTokens: common.GetPointer(100)}, expression, "0.008225 USD"},
		{"explicit free image cache", &dto.CachedTokenDetails{ImageTokens: common.GetPointer(200)}, `tier("standard", p * 5 + cr * 1.25 + img * 8 + img_cr * 0 + c * 30)`, "0.007825 USD"},
		{"explicit zero", &dto.CachedTokenDetails{ImageTokens: common.GetPointer(0)}, expression, "0.008675 USD"},
		{"missing breakdown", nil, expression, "0.008675 USD"},
		{"invalid modality sum", &dto.CachedTokenDetails{ImageTokens: common.GetPointer(200), TextTokens: common.GetPointer(101)}, expression, "0.008675 USD"},
		{"old expression unchanged", &dto.CachedTokenDetails{ImageTokens: common.GetPointer(200)}, `p * 5 + cr * 1.25 + img * 8 + c * 30`, "0.008675 USD"},
		{"only image cache separately priced", &dto.CachedTokenDetails{ImageTokens: common.GetPointer(200)}, `p * 5 + img_cr * 2 + c * 30`, "0.0074 USD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usage := &dto.Usage{PromptTokens: 1000, CompletionTokens: 100, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 300, ImageTokens: 600, CachedTokensDetails: tc.details}}
			price := model.PriceVersion{Expression: tc.expression, Currency: "USD"}
			amount, _, _, err := GatewayPrice(&price, usage, billingexpr.RequestInput{}, time.Time{}, decimal.NewFromInt(1), nil)
			require.NoError(t, err)
			assert.Equal(t, tc.want, amount.String())
		})
	}
}

func TestPrepareTieredBillingForSelectedGroupUpdatesReservation(t *testing.T) {
	// A later price cannot rewrite the sole reservation or frozen sale discount.
	task := seedCanonicalWalletTaskWithPricing(t, 1000, `u("tokens") / 1000000000`, `u("tokens") / 2000000000`, map[string]any{"tokens": 1000}, "0.1")
	request := task.PrivateData.Gateway.Settlement.Request
	changed := request
	changed.GroupRatio = "0.2"
	amount := money.Amount{Nanos: 2000, Currency: "USD"}
	require.Error(t, model.ReserveGateway(&changed, amount, amount))
	var billing model.GatewayBilling
	require.NoError(t, model.DB.First(&billing, "request_id = ?", request.ID).Error)
	assert.Equal(t, money.Amount{Nanos: 1000, Currency: "USD"}, billing.Reservation)
	var frozen model.GatewayRequest
	require.NoError(t, common.UnmarshalJsonStr(billing.RequestData, &frozen))
	assert.Equal(t, "0.1", frozen.GroupRatio)
	task.Status = model.TaskStatusSuccess
	task.PrivateData.Gateway.CompletionFacts = map[string]any{"tokens": 1000}
	require.NoError(t, SettleGatewayTask(t.Context(), task))
	assertCanonicalWalletTaskAccounting(t, task, 1000, 100, 500)
}

func TestPrepareTieredBillingForSelectedGroupStartsBillingAfterFreeGroup(t *testing.T) {
	// An explicit free sale still creates its lifecycle. A later paid route
	// cannot turn it into a second reservation or consume legacy token quota.
	task := seedCanonicalWalletTaskWithPricing(t, 0, `u("tokens") / 1000000000`, `u("tokens") / 2000000000`, map[string]any{"tokens": 1000}, "0")
	changed := task.PrivateData.Gateway.Settlement.Request
	changed.GroupRatio = "0.2"
	amount := money.Amount{Nanos: 200, Currency: "USD"}
	require.Error(t, model.ReserveGateway(&changed, amount, amount))
	task.Status = model.TaskStatusSuccess
	task.PrivateData.Gateway.CompletionFacts = map[string]any{"tokens": 1000}
	require.NoError(t, SettleGatewayTask(t.Context(), task))
	assertCanonicalWalletTaskAccounting(t, task, 0, 0, 500)
}

func TestPrepareTieredBillingForSelectedGroupPaidToFreeKeepsFreeModelFalse(t *testing.T) {
	task := seedCanonicalWalletTaskWithPricing(t, 1000, `u("tokens") / 1000000000`, `u("tokens") / 2000000000`, map[string]any{"tokens": 1000}, "0.1")
	changed := task.PrivateData.Gateway.Settlement.Request
	changed.GroupRatio = "0"
	zero := money.Amount{Currency: "USD"}
	require.Error(t, model.ReserveGateway(&changed, zero, zero))
	task.Status = model.TaskStatusSuccess
	task.PrivateData.Gateway.CompletionFacts = map[string]any{"tokens": 1000}
	require.NoError(t, SettleGatewayTask(t.Context(), task))
	assertCanonicalWalletTaskAccounting(t, task, 1000, 100, 500)
}

func TestPrepareTieredBillingForSelectedGroupTopUpArrearsAllowsNegativeBalance(t *testing.T) {
	// A real post-delivery shortfall is a receivable, never a negative wallet
	// or permission to reserve more money before a later Provider attempt.
	task := seedCanonicalWalletTask(t, 1000)
	request := task.PrivateData.Gateway.Settlement.Request
	larger := money.Amount{Nanos: 10000, Currency: "USD"}
	require.Error(t, model.ReserveGateway(&request, larger, larger))
	task.Status = model.TaskStatusSuccess
	task.PrivateData.Gateway.CompletionFacts = map[string]any{"tokens": 10000}
	require.NoError(t, SettleGatewayTask(t.Context(), task))
	wallet, err := model.GetUserWallet(77)
	require.NoError(t, err)
	assert.Zero(t, wallet.Balance.Nanos)
	var debt model.Wallet
	require.NoError(t, model.DB.Where("account_type = ? AND owner_id = ?", model.LedgerAccountUserReceivable, 77).Take(&debt).Error)
	assert.EqualValues(t, 1112, debt.Balance.Nanos)
	record, err := model.GetGatewayRequest(t.Context(), request.ID, 77)
	require.NoError(t, err)
	assert.EqualValues(t, 10000, record.Receivable.Nanos)
	var first, second []model.LedgerEntry
	require.NoError(t, model.DB.Order("id").Find(&first).Error)
	require.NoError(t, SettleGatewayTask(t.Context(), task))
	require.NoError(t, model.DB.Order("id").Find(&second).Error)
	assert.Equal(t, first, second)
	assert.Equal(t, 8000, getTokenRemainQuota(t, 77))
	assert.Zero(t, getUserQuota(t, 77))
	var holds int64
	require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("request_id = ? AND type = ?", request.ID, model.LedgerEntryPreConsume).Count(&holds).Error)
	assert.EqualValues(t, 1, holds)
	reconciliation, err := model.ReconcileWallets(100)
	require.NoError(t, err)
	assert.Empty(t, reconciliation.Mismatches)
}

func TestBillingSessionReserveWalletTopUpDecrementsBalance(t *testing.T) {
	task := seedCanonicalWalletTask(t, 1000)
	request := task.PrivateData.Gateway.Settlement.Request
	more := money.Amount{Nanos: 2000, Currency: "USD"}
	require.Error(t, model.ReserveGateway(&request, more, more))
	wallet, err := model.GetUserWallet(77)
	require.NoError(t, err)
	assert.EqualValues(t, 7888, wallet.Balance.Nanos)
	task.Status = model.TaskStatusSuccess
	task.PrivateData.Gateway.CompletionFacts = map[string]any{"tokens": 800}
	require.NoError(t, SettleGatewayTask(t.Context(), task))
	assertCanonicalWalletTaskAccounting(t, task, 1000, 800, 400)
}
