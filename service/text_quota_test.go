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

// These cases exercise exact pricing and the real reservation, settlement,
// journal and outbox. Historical integer counters are immutable evidence.
func TestFixedPriceBillingDatabase(t *testing.T) {
	const mixed = `len <= 32000 ? tier("short", fixed(0.01)) : tier("long", p * 2)`
	const flat = `tier("request", fixed(0.01))`
	const imageExpression = `tier("standard", p * 5 + cr * 1.25 + img * 8 + img_cr * 2 + c * 30)`
	imageUsage := &dto.Usage{PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100,
		PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 300, ImageTokens: 600, CachedTokensDetails: &dto.CachedTokenDetails{ImageTokens: common.GetPointer(200)}}}
	for _, tc := range []struct {
		name, expression, group                                string
		estimate                                               int
		usage                                                  *dto.Usage
		audio, stream, refund, insufficient, realtime, pending bool
		wallet, retryReservation                               int64
		requestedImages, actualImages, outboundImages          int
		lateOverride                                           bool
		want                                                   int64
		unit                                                   billingexpr.BillingUnit
	}{
		{name: "missing usage charges fixed price once", expression: flat, want: 10000000, unit: billingexpr.BillingUnitRequest},
		{name: "zero usage charges fixed price once", expression: flat, usage: &dto.Usage{}, want: 10000000, unit: billingexpr.BillingUnitRequest},
		{name: "stream charges once", expression: flat, stream: true, usage: &dto.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120}, want: 10000000, unit: billingexpr.BillingUnitRequest},
		{name: "audio zero usage charges once", expression: flat, audio: true, usage: &dto.Usage{}, want: 10000000, unit: billingexpr.BillingUnitRequest},
		{name: "audio missing usage charges once", expression: flat, audio: true, want: 10000000, unit: billingexpr.BillingUnitRequest},
		{name: "token reservation refunds to fixed price", expression: mixed, estimate: 50000, usage: &dto.Usage{PromptTokens: 100, TotalTokens: 100}, want: 10000000, unit: billingexpr.BillingUnitRequest},
		{name: "fixed reservation settles actual token branch", expression: mixed, estimate: 100, usage: &dto.Usage{PromptTokens: 50000, TotalTokens: 50000}, want: 100000000, unit: billingexpr.BillingUnitToken},
		{name: "missing usage stays pending not estimated actual", expression: mixed, estimate: 50000, pending: true},
		{name: "evaluation error retains original reservation", expression: `p == 50 ? tier("error", param("missing") * p + img_cr * 2) : tier("request", fixed(0.01))`, estimate: 100, usage: &dto.Usage{PromptTokens: 50, TotalTokens: 50}, pending: true},
		{name: "explicit zero remains free", expression: `tier("free", fixed(0))`, usage: &dto.Usage{PromptTokens: 100, TotalTokens: 100}, unit: billingexpr.BillingUnitRequest},
		{name: "frozen request multiplier and group discount", expression: flat + ` * (param("fast") == true ? 2 : 1)`, group: "1.5", want: 30000000, unit: billingexpr.BillingUnitRequest},
		{name: "failed request refunds exactly once", expression: flat, refund: true},
		{name: "insufficient wallet leaves tokens unchanged", expression: flat, insufficient: true},
		{name: "image cache stream refunds unused reservation", expression: imageExpression, estimate: 10000, usage: imageUsage, stream: true, want: 8225000, unit: billingexpr.BillingUnitToken},
		{name: "audio records actual image cache inputs", expression: imageExpression, estimate: 10000, usage: imageUsage, audio: true, want: 8225000, unit: billingexpr.BillingUnitToken},
		{name: "realtime records reported expression inputs", expression: imageExpression, estimate: 10000, usage: imageUsage, realtime: true, want: 8000000, unit: billingexpr.BillingUnitToken},
		{name: "image cache insufficient wallet", expression: imageExpression, estimate: 10000, insufficient: true},
		{name: "image quantity refunds missing images", expression: `tier("image", fixed(0.04)) * image_count`, requestedImages: 3, actualImages: 2, want: 80000000, unit: billingexpr.BillingUnitRequest},
		{name: "image quantity preserves labeled request estimate", expression: `tier("image", fixed(0.04)) * image_count`, requestedImages: 3, want: 120000000, unit: billingexpr.BillingUnitRequest},
		{name: "image quantity zero price stays free", expression: `tier("image", fixed(0)) * image_count`, requestedImages: 3, actualImages: 2, unit: billingexpr.BillingUnitRequest},
		{name: "image quantity failure refunds reservation", expression: `tier("image", fixed(0.04)) * image_count`, requestedImages: 3, refund: true},
		{name: "image quantity exceeds one-image wallet", expression: `tier("image", fixed(0.04)) * image_count`, requestedImages: 4, wallet: 40000000, insufficient: true},
		{name: "effective image quantity is reserved before first send", expression: `tier("image", fixed(0.04)) * image_count`, requestedImages: 1, outboundImages: 4, actualImages: 4, want: 160000000, unit: billingexpr.BillingUnitRequest},
		{name: "late image override cannot append another hold", expression: `tier("image", fixed(0.04)) * image_count`, requestedImages: 1, outboundImages: 4, wallet: 80000000, lateOverride: true, refund: true},
		{name: "pricier retry cannot append another hold", expression: flat, retryReservation: 80000000, refund: true},
		{name: "pricier retry cannot overdraw remaining wallet", expression: flat, retryReservation: 80000000, wallet: 40000000, refund: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seedSubscriptionTaskMoney(t, 0)
			require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", 77).Update("setting", `{"billing_preference":"wallet_only"}`).Error)
			starting := int64(2000000000)
			if tc.insufficient {
				starting = 8888
			}
			if tc.wallet != 0 {
				starting = tc.wallet
			}
			if starting > 8888 {
				_, err := model.TopUpWallet(model.WalletTopUp{UserId: 77, Source: money.Amount{Nanos: starting - 8888, Currency: "USD"}, Reference: "fixed-price-fixture"})
				require.NoError(t, err)
			}
			group := tc.group
			if group == "" {
				group = "1"
			}
			requestInput := billingexpr.RequestInput{Body: []byte(`{"fast":true}`)}
			if tc.requestedImages > 0 {
				requestInput.ImageCount = &tc.requestedImages
			}
			estimateInput := requestInput
			if tc.outboundImages > 0 && !tc.lateOverride {
				estimateInput.ImageCount = &tc.outboundImages
			}
			price := model.PriceVersion{ID: 1, Currency: "USD", Expression: tc.expression}
			estimated, _, _, err := GatewayPrice(&price, &dto.Usage{PromptTokens: tc.estimate, TotalTokens: tc.estimate}, estimateInput, time.Unix(1700000000, 0), decimal.RequireFromString(group), nil)
			require.NoError(t, err)
			reserved, err := estimated.Receivable()
			require.NoError(t, err)
			request := model.GatewayRequest{ID: "fixed-price-accounting", UserID: 77, TokenID: 77, SalePriceVersionID: price.ID, SaleExpression: price.Expression, SaleCurrency: "USD", SaleFXRate: "1", GroupRatio: group, StartedAt: time.Now().UnixMilli(), RequestedName: "frozen-offering"}
			err = model.ReserveGateway(&request, reserved, reserved)
			if tc.insufficient {
				require.ErrorIs(t, err, model.ErrInsufficientWalletBalance)
				var count int64
				require.NoError(t, model.DB.Model(&model.GatewayBilling{}).Where("request_id = ?", request.ID).Count(&count).Error)
				assert.Zero(t, count)
				assert.Equal(t, 8000, getTokenRemainQuota(t, 77))
				wallet, err := model.GetUserWallet(77)
				require.NoError(t, err)
				assert.Equal(t, starting, wallet.Balance.Nanos)
				return
			}
			require.NoError(t, err)
			wallet, err := model.GetUserWallet(77)
			require.NoError(t, err)
			assert.Equal(t, starting-reserved.Nanos, wallet.Balance.Nanos)
			if tc.retryReservation > 0 || tc.lateOverride {
				extra := tc.retryReservation
				if tc.lateOverride {
					extra = 160000000
				}
				amount := money.Amount{Nanos: extra, Currency: "USD"}
				require.Error(t, model.ReserveGateway(&request, amount, amount), "the same request cannot acquire a second hold")
			}
			usage := GatewayUsageSnapshot(tc.usage)
			if tc.realtime && usage != nil {
				usage = &dto.Usage{PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens, TotalTokens: usage.TotalTokens}
			}
			actualInput := requestInput
			if usage == nil {
				actualInput.UnmeasuredTokens = map[string]bool{"p": true, "c": true, "len": true, "img": true, "img_cr": true}
			}
			request.Usage = GatewayRecordUsage(usage)
			request.UsageSource = "upstream"
			if usage == nil {
				request.UsageSource = "none"
			}
			if tc.requestedImages > 0 {
				count := tc.actualImages
				if count == 0 {
					count = *estimateInput.ImageCount
					request.UsageSource = "estimated"
				}
				request.Usage.ImageCount = common.GetPointer(int64(count))
			}
			request.Status = "success"
			request.Receivable = money.Amount{Currency: "USD"}
			intent := model.GatewaySettlement{Request: request, SaleSource: money.Amount{Currency: "USD"}, SaleUsage: usage}
			if tc.refund {
				intent.Request.Status = "failed"
			} else {
				counts := []int{}
				if request.Usage.ImageCount != nil {
					counts = append(counts, int(*request.Usage.ImageCount))
				}
				evaluationUsage := usage
				if evaluationUsage == nil {
					evaluationUsage = &dto.Usage{} // presence remains unknown in actualInput
				}
				actual, _, trace, priceErr := GatewayPrice(&price, evaluationUsage, actualInput, time.Unix(1700000000, 0), decimal.RequireFromString(group), nil, counts...)
				if tc.pending {
					require.Error(t, priceErr)
					intent.SaleEvaluationFailed, intent.EvaluationFailed, intent.Request.SalePending = true, true, true
				} else {
					require.NoError(t, priceErr)
					amount, err := actual.Receivable()
					require.NoError(t, err)
					assert.Equal(t, tc.want, amount.Nanos)
					intent.Request.Receivable, intent.SaleSource = amount, amount
					assert.Equal(t, tc.unit, trace.BillingUnit)
				}
				trace.MatchedTier = ""
				intent.SaleTrace = &trace
			}
			intent.Attempt = &model.GatewayAttempt{ID: "fixed-price-attempt", RequestID: request.ID, ProviderID: 77, Status: "success", Usage: request.Usage, UsageSource: request.UsageSource, PurchasePriceVersionID: 2, PurchaseExpression: "fixed(0.001)", Cost: money.Amount{Nanos: 1000000, Currency: "USD"}, ConvertedCost: money.Amount{Nanos: 1000000, Currency: "USD"}, AccountingFXRate: "1"}
			require.NoError(t, model.PrepareGatewaySettlement(intent))
			_, err = model.SettleGateway(request.ID)
			if tc.pending {
				require.ErrorIs(t, err, model.ErrGatewayEvaluationPending)
			} else {
				require.NoError(t, err)
				require.NoError(t, model.PublishGatewayRecords(request.ID))
			}
			var first, repeated []model.LedgerEntry
			require.NoError(t, model.DB.Where("request_id = ?", request.ID).Order("id").Find(&first).Error)
			for range 2 {
				_, err = model.SettleGateway(request.ID)
				if tc.pending {
					require.ErrorIs(t, err, model.ErrGatewayEvaluationPending)
				} else {
					require.NoError(t, err)
					require.NoError(t, model.PublishGatewayRecords(request.ID))
				}
			}
			require.NoError(t, model.DB.Where("request_id = ?", request.ID).Order("id").Find(&repeated).Error)
			assert.Equal(t, first, repeated)
			wallet, err = model.GetUserWallet(77)
			require.NoError(t, err)
			if tc.pending {
				assert.Equal(t, starting-reserved.Nanos, wallet.Balance.Nanos)
			} else {
				assert.Equal(t, starting-tc.want, wallet.Balance.Nanos)
			}
			var holds, releases int
			var cost int64
			for _, entry := range first {
				if entry.Type == model.LedgerEntryPreConsume {
					holds++
				}
				if entry.Type == model.LedgerEntryPreConsumeRelease {
					releases++
				}
				if entry.AccountType == model.LedgerAccountProviderPayable {
					cost += entry.Amount.Nanos
				}
			}
			wantHolds := 0
			if reserved.Nanos != 0 {
				wantHolds = 1
			}
			assert.Equal(t, wantHolds, holds)
			if tc.pending {
				assert.Zero(t, releases)
			} else {
				assert.Equal(t, wantHolds, releases)
			}
			assert.EqualValues(t, 1000000, cost, "actual source procurement survives failed delivery and unknown sale")
			if !tc.pending {
				record, err := model.GetGatewayRequest(t.Context(), request.ID, 77)
				require.NoError(t, err)
				assert.EqualValues(t, tc.want, record.Receivable.Nanos)
				assert.Equal(t, request.Usage, record.Usage)
				assert.Equal(t, request.UsageSource, record.UsageSource)
				assert.Equal(t, price.Expression, record.SaleExpression)
				assert.Equal(t, price.ID, record.SalePriceVersionID)
			}
			if tc.requestedImages > 0 {
				assert.Equal(t, tc.requestedImages, *requestInput.ImageCount, "actual facts must not mutate original request facts")
			}
			assert.Zero(t, getUserQuota(t, 77))
			assert.Equal(t, 8000, getTokenRemainQuota(t, 77))
			used, requests := getUserUsageAccounting(t, 77)
			assert.Zero(t, used)
			assert.Zero(t, requests)
			assert.Zero(t, getChannelUsedQuota(t, 77))
			assert.Zero(t, countLogs(t), "money accounting must not create legacy consume logs")
			reconciliation, err := model.ReconcileWallets(100)
			require.NoError(t, err)
			assert.Empty(t, reconciliation.Mismatches)
		})
	}
}

func TestGatewayPriceUsesNativeBillingUsage(t *testing.T) {
	for _, tc := range []struct {
		name, expression string
		usage            *dto.BillingUsage
		nanos            int64
	}{
		{
			name:       "Anthropic split cache and context length",
			expression: `len == 120 ? tier("expected", p + cr * 0.1 + cc * 1.25 + cc1h * 2 + c * 2) : tier("wrong", fixed(0))`,
			usage: dto.NewClaudeMessagesBillingUsage(&dto.ClaudeUsage{
				InputTokens:              70,
				CacheReadInputTokens:     30,
				CacheCreationInputTokens: 20,
				OutputTokens:             7,
				CacheCreation: &dto.ClaudeCacheCreationUsage{
					Ephemeral5mInputTokens: 12,
					Ephemeral1hInputTokens: 8,
				},
			}),
			nanos: 118000,
		},
		{
			name:       "Gemini tool and thinking tokens",
			expression: `tier("expected", p + cr * 0.1 + c * 2)`,
			usage: dto.NewGeminiChatBillingUsage(&dto.GeminiUsageMetadata{
				PromptTokenCount:        100,
				ToolUsePromptTokenCount: 5,
				CandidatesTokenCount:    20,
				ThoughtsTokenCount:      3,
				TotalTokenCount:         128,
				CachedContentTokenCount: 7,
			}),
			nanos: 144700,
		},
		{
			name:       "OpenAI Responses cache details",
			expression: `tier("expected", p + cr * 0.5 + cc * 2 + c * 2)`,
			usage: dto.NewOpenAIResponsesBillingUsage(&dto.Usage{
				InputTokens: 100, OutputTokens: 10, TotalTokens: 110,
				InputTokensDetails: &dto.InputTokenDetails{CachedTokens: 80, CacheWriteTokens: 10},
			}),
			nanos: 90000,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			converted := &dto.Usage{PromptTokens: 999, CompletionTokens: 999, TotalTokens: 1998, BillingUsage: tc.usage}
			price := model.PriceVersion{Currency: "USD", Expression: tc.expression}
			value, _, trace, err := GatewayPrice(&price, effectiveBillingUsage(converted), billingexpr.RequestInput{}, time.Unix(1700000000, 0), decimal.NewFromInt(1), nil)
			require.NoError(t, err)
			amount, err := value.Receivable()
			require.NoError(t, err)
			assert.Equal(t, tc.nanos, amount.Nanos)
			assert.Equal(t, "expected", trace.MatchedTier)
			assert.Equal(t, 999, converted.PromptTokens)
		})
	}
}

func TestUsageFromOpenAIBillingUsageNormalizesCacheDetailsWithoutOverwritingCanonicalValues(t *testing.T) {
	responsesUsage := &dto.Usage{
		InputTokens:          100,
		OutputTokens:         10,
		PromptCacheHitTokens: 55,
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: 8,
			TextTokens:   12,
		},
		InputTokensDetails: &dto.InputTokenDetails{
			CachedTokens:         40,
			CachedCreationTokens: 5,
			CacheWriteTokens:     6,
			TextTokens:           60,
			ImageTokens:          7,
			AudioTokens:          9,
		},
	}

	billingUsage := dto.NewOpenAIResponsesBillingUsage(responsesUsage)
	usage := effectiveBillingUsage(&dto.Usage{BillingUsage: billingUsage})

	require.Equal(t, 8, usage.PromptTokensDetails.CachedTokens)
	require.Equal(t, 5, usage.PromptTokensDetails.CachedCreationTokens)
	require.Equal(t, 6, usage.PromptTokensDetails.CacheWriteTokens)
	require.Equal(t, 12, usage.PromptTokensDetails.TextTokens)
	require.Equal(t, 7, usage.PromptTokensDetails.ImageTokens)
	require.Equal(t, 9, usage.PromptTokensDetails.AudioTokens)
	require.Zero(t, billingUsage.OpenAIUsage.PromptTokensDetails.CachedCreationTokens)
}

func TestUsageFromOpenAIBillingUsageFallsBackToPromptCacheHitTokens(t *testing.T) {
	usage := effectiveBillingUsage(&dto.Usage{
		BillingUsage: dto.NewOpenAIChatBillingUsage(&dto.Usage{
			PromptTokens:         100,
			CompletionTokens:     10,
			PromptCacheHitTokens: 35,
		}),
	})

	require.Equal(t, 35, usage.PromptTokensDetails.CachedTokens)
}

func TestUsageBillingPathForLog(t *testing.T) {
	require.Equal(t, usageBillingPathAnthropic, usageBillingPathForLog(true, &dto.Usage{
		BillingUsage: dto.NewClaudeMessagesBillingUsage(&dto.ClaudeUsage{InputTokens: 1}),
	}))
	invalidBillingUsage := &dto.Usage{
		PromptTokens: 1,
		BillingUsage: &dto.BillingUsage{
			Source:   dto.BillingUsageSourceClaudeMessages,
			Semantic: dto.BillingUsageSemanticAnthropic,
		},
	}
	require.Equal(t, usageBillingPathLocal, usageBillingPathForLog(true, invalidBillingUsage))
	require.Equal(t, usageBillingPathUpstream, usageBillingPathForLog(false, invalidBillingUsage))
	require.Equal(t, usageBillingPathUpstream, usageBillingPathForLog(false, &dto.Usage{}))
	require.Equal(t, usageBillingPathOpenAI, usageBillingPathForLog(false, &dto.Usage{
		BillingUsage: dto.NewOpenAIChatBillingUsage(&dto.Usage{PromptTokens: 1}),
	}))
	require.Equal(t, usageBillingPathAnthropic, usageBillingPathForLog(false, &dto.Usage{
		BillingUsage: dto.NewClaudeMessagesBillingUsage(&dto.ClaudeUsage{InputTokens: 1}),
	}))
	require.Equal(t, usageBillingPathGemini, usageBillingPathForLog(false, &dto.Usage{
		BillingUsage: dto.NewGeminiChatBillingUsage(&dto.GeminiUsageMetadata{PromptTokenCount: 1}),
	}))
	require.Equal(t, usageBillingPathGeminiEstimated, usageBillingPathForLog(true, &dto.Usage{
		BillingUsage: dto.NewEstimatedGeminiChatBillingUsage(&dto.Usage{PromptTokens: 1}),
	}))
}

func TestAppendUsageBillingPathForLogWritesAdminInfo(t *testing.T) {
	other := model.NewLogOther()
	appendUsageBillingPathForLog(other, true, &dto.Usage{
		BillingUsage: dto.NewClaudeMessagesBillingUsage(&dto.ClaudeUsage{InputTokens: 1}),
	})

	adminInfo, ok := other.Snapshot()["admin_info"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, usageBillingPathAnthropic, adminInfo["usage_billing_path"])

	other = model.NewLogOther()
	appendUsageBillingPathForLog(other, true, nil)
	adminInfo, ok = other.Snapshot()["admin_info"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, usageBillingPathLocal, adminInfo["usage_billing_path"])
}

// OpenAI 12h cache writes bill at twice the write price and keep their own
// count in the consume log, with ratio and expression pricing alike.
func TestPostTextConsumeQuotaBillsOpenAI12hCacheWrites(t *testing.T) {
	usage := &dto.Usage{PromptTokens: 12000, TotalTokens: 12000, PromptTokensDetails: dto.InputTokenDetails{
		CachedTokens: 4000, CacheWriteTokens: 7000,
		CacheWriteTokensDetails: &dto.CacheWriteTokenDetails{TTL30mTokens: 3000, TTL12hTokens: 4000},
	}}
	for _, expression := range []string{
		`tier("standard", p * 10 + c * 50 + cr + cc * 12.5 + cc12h * 25)`,
		`tier("standard", p * 10 + c * 50 + cr + cc * 12.5)`,
	} {
		t.Run(expression, func(t *testing.T) {
			seedSubscriptionTaskMoney(t, 0)
			require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", 77).Update("setting", `{"billing_preference":"wallet_only"}`).Error)
			_, err := model.TopUpWallet(model.WalletTopUp{UserId: 77, Source: money.Amount{Nanos: 200000000, Currency: "USD"}, Reference: "cache-write-fixture"})
			require.NoError(t, err)
			price := model.PriceVersion{ID: 1, Currency: "USD", Expression: expression}
			actual, _, trace, err := GatewayPrice(&price, usage, billingexpr.RequestInput{}, time.Now(), decimal.NewFromInt(1), nil)
			require.NoError(t, err)
			amount, err := actual.Receivable()
			require.NoError(t, err)
			assert.EqualValues(t, 151500000, amount.Nanos)
			request := model.GatewayRequest{ID: "cache-write-price", UserID: 77, TokenID: 77, SalePriceVersionID: 1, SaleExpression: expression, SaleCurrency: "USD", SaleFXRate: "1", GroupRatio: "1", Usage: GatewayRecordUsage(usage), UsageSource: "upstream", Receivable: amount, Status: "success"}
			reservation := money.Amount{Nanos: 200000000, Currency: "USD"}
			require.NoError(t, model.ReserveGateway(&request, reservation, reservation))
			trace.MatchedTier = ""
			require.NoError(t, model.PrepareGatewaySettlement(model.GatewaySettlement{Request: request, SaleSource: amount, SaleUsage: usage, SaleTrace: &trace}))
			_, err = model.SettleGateway(request.ID)
			require.NoError(t, err)
			require.NoError(t, model.PublishGatewayRecords(request.ID))
			record, err := model.GetGatewayRequest(t.Context(), request.ID, 77)
			require.NoError(t, err)
			assert.EqualValues(t, 151500000, record.Receivable.Nanos)
			assert.EqualValues(t, 7000, record.Usage.CacheCreationTokens)
			assert.EqualValues(t, 4000, record.Usage.CacheCreation12hTokens)
			assert.EqualValues(t, 4000, record.Usage.CachedTokens)
			wallet, err := model.GetUserWallet(77)
			require.NoError(t, err)
			assert.EqualValues(t, 48508888, wallet.Balance.Nanos)
			assert.Equal(t, 8000, getTokenRemainQuota(t, 77))
			assert.Zero(t, countLogs(t))
		})
	}
}

// A request whose upstream returns no usage is logged without a charge. The
// reason is stored as content_parts, which the web console renders in the
// viewer's language; content keeps the English text.
func TestPostTextConsumeQuotaLogsMissingUsageAsStructuredContent(t *testing.T) {
	// Old messages remain readable; they must not be regenerated as evidence
	// that missing usage is an actual zero-cost request.
	truncate(t)
	const message = "Upstream returned no usage, so nothing was charged (possibly an upstream timeout)"
	other, err := common.Marshal(map[string]any{"content_parts": []any{map[string]any{"key": message}}})
	require.NoError(t, err)
	fixture := model.Log{UserId: 62, Type: model.LogTypeConsume, Content: message, Other: string(other)}
	require.NoError(t, model.LOG_DB.Create(&fixture).Error)
	logs, total, err := model.GetUserLogs(62, model.LogFilter{}, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Len(t, logs, 1)
	assert.Zero(t, logs[0].Quota)
	assert.Equal(t, message, logs[0].Content)
	var decoded map[string]any
	require.NoError(t, common.UnmarshalJsonStr(logs[0].Other, &decoded))
	assert.Equal(t, []any{map[string]any{"key": message}}, decoded["content_parts"])
}
