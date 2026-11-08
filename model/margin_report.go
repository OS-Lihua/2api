package model

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// MarginFilter uses request start time, with an inclusive start and exclusive end.
// Revenue is attributed once to the last Attempt; every Attempt carries its cost.
type MarginFilter struct {
	OfferingID, ChannelID, ProviderID int
	From, Until                       int64
	GroupBy                           []string
	Timezone                          string
}

type MarginRow struct {
	ModelOfferingID      int            `json:"model_offering_id,omitempty"`
	ChannelID            int            `json:"channel_id,omitempty"`
	ProviderID           int            `json:"provider_id,omitempty"`
	Date                 string         `json:"date,omitempty"`
	Revenue              money.Amount   `json:"revenue"`
	Collected            money.Amount   `json:"collected"`
	Outstanding          money.Amount   `json:"outstanding"`
	Cost                 money.Amount   `json:"cost"`
	OriginalCosts        []money.Amount `json:"original_costs"`
	Margin               *money.Amount  `json:"margin"`
	KnownMargin          money.Amount   `json:"known_margin"`
	SalePending          int            `json:"sale_pending"`
	CostPending          int            `json:"cost_pending"`
	ConvertedCostPending int            `json:"converted_cost_pending"`
}

type marginTotals struct {
	MarginRow
	revenue, collected, outstanding, cost decimal.Decimal
	original                              map[money.Currency]decimal.Decimal
}

// MarginReport reads a consistent primary-database snapshot. Log outbox delivery
// never participates in accounting. Ledger sums are numeric, grouped by currency;
// immutable settlement facts supply dimensions and unfinished accounting flags.
func MarginReport(ctx context.Context, filter MarginFilter, offset, limit int) ([]MarginRow, int, error) {
	location, err := time.LoadLocation(filter.Timezone)
	if err != nil {
		return nil, 0, err
	}
	groups := map[string]bool{}
	for _, name := range filter.GroupBy {
		if !slices.Contains([]string{"model_offering", "channel", "provider", "day"}, name) {
			return nil, 0, fmt.Errorf("invalid margin dimension")
		}
		groups[name] = true
	}
	totals := map[string]*marginTotals{}
	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Model(&GatewayBilling{}).Order("request_id")
		if filter.OfferingID > 0 {
			query = query.Where(`("request_data"::jsonb ->> 'model_offering_id')::bigint = ?`, filter.OfferingID)
		}
		if filter.From > 0 {
			query = query.Where(`("request_data"::jsonb ->> 'started_at')::bigint >= ?`, filter.From)
		}
		if filter.Until > 0 {
			query = query.Where(`("request_data"::jsonb ->> 'started_at')::bigint < ?`, filter.Until)
		}
		var batch []GatewayBilling
		return query.FindInBatches(&batch, 250, func(_ *gorm.DB, _ int) error {
			ids := make([]string, len(batch))
			for i := range batch {
				ids[i] = batch[i].RequestID
			}
			var entries []struct {
				RequestId, AttemptId string
				AccountType          LedgerAccountType
				Currency             money.Currency
				Amount               decimal.Decimal
				Converted            decimal.Decimal
				Initial              decimal.Decimal
				SnapshotConverted    decimal.Decimal
				SnapshotPosts        int64
			}
			// Initial costs and their reversals use the immutable converted
			// snapshot. Other corrections use the FX frozen on their ledger entry.
			if err := tx.Model(&LedgerEntry{}).Select(`"request_id", "attempt_id", "account_type", "amount_currency" AS "currency", SUM("amount_nanos") AS "amount", SUM(ROUND("amount_nanos"::numeric * COALESCE("fx_rate", 1))) AS "converted",
 COALESCE(SUM("amount_nanos") FILTER (WHERE "idempotency_key" = 'gateway:' || "attempt_id" || ':payable'), 0) AS "initial",
 SUM(CASE WHEN "idempotency_key" = 'gateway:' || "attempt_id" || ':payable' THEN 1 WHEN "reverses_entry_id" IN (SELECT "id" FROM "ledger_entries" AS "source" WHERE "source"."idempotency_key" = 'gateway:' || "ledger_entries"."attempt_id" || ':payable') THEN -1 ELSE 0 END) AS "snapshot_posts",
 SUM(CASE WHEN "idempotency_key" = 'gateway:' || "attempt_id" || ':payable' OR "reverses_entry_id" IN (SELECT "id" FROM "ledger_entries" AS "source" WHERE "source"."idempotency_key" = 'gateway:' || "ledger_entries"."attempt_id" || ':payable') THEN ROUND("amount_nanos"::numeric * COALESCE("fx_rate", 1)) ELSE 0 END) AS "snapshot_converted"`).
				Where(`"request_id" IN ? AND ("account_type" IN ? OR ("account_type" = ? AND "type" IN ?))`, ids, []LedgerAccountType{LedgerAccountPlatformRevenue, LedgerAccountProviderPayable, LedgerAccountUserReceivable}, LedgerAccountUserWallet, []LedgerEntryType{LedgerEntryCharge, LedgerEntryRepayment, LedgerEntryRefund}).
				Group(`"request_id", "attempt_id", "account_type", "amount_currency"`).Scan(&entries).Error; err != nil {
				return err
			}
			byRequest := map[string][]int{}
			for i := range entries {
				byRequest[entries[i].RequestId] = append(byRequest[entries[i].RequestId], i)
			}
			for _, lifecycle := range batch {
				var request GatewayRequest
				if err := common.UnmarshalJsonStr(lifecycle.RequestData, &request); err != nil {
					return err
				}
				if request.Receivable.Currency != "" && request.Receivable.Currency != SettlementCurrency() {
					return money.ErrCurrencyMismatch
				}
				var intent GatewaySettlement
				if lifecycle.Settlement != "" {
					if err := common.UnmarshalJsonStr(lifecycle.Settlement, &intent); err != nil {
						return err
					}
				}
				attempts := intent.Attempts
				if intent.Attempt != nil {
					attempts = []GatewayAttempt{*intent.Attempt}
				}
				final := GatewayAttempt{}
				if len(attempts) > 0 {
					final = attempts[len(attempts)-1]
				}
				facts := append([]GatewayAttempt{final}, attempts...)
				for index, attempt := range facts {
					if filter.ChannelID > 0 && attempt.ChannelID != filter.ChannelID || filter.ProviderID > 0 && attempt.ProviderID != filter.ProviderID {
						continue
					}
					dimension := MarginRow{}
					if groups["model_offering"] {
						dimension.ModelOfferingID = request.ModelOfferingID
					}
					if groups["channel"] {
						dimension.ChannelID = attempt.ChannelID
					}
					if groups["provider"] {
						dimension.ProviderID = attempt.ProviderID
					}
					if groups["day"] {
						dimension.Date = time.UnixMilli(request.StartedAt).In(location).Format(time.DateOnly)
					}
					key := fmt.Sprintf("%020d/%020d/%020d/%s", dimension.ModelOfferingID, dimension.ChannelID, dimension.ProviderID, dimension.Date)
					total := totals[key]
					if total == nil {
						total = &marginTotals{MarginRow: dimension, original: map[money.Currency]decimal.Decimal{}}
						totals[key] = total
					}
					if index == 0 {
						if request.SalePending || lifecycle.Phase == "reserved" || lifecycle.Phase == "settlement_pending" {
							total.SalePending++
						}
					} else {
						if !attempt.CostPending && !attempt.ConvertedCostPending && attempt.ConvertedCost.Currency != SettlementCurrency() {
							return money.ErrCurrencyMismatch
						}
						if attempt.CostPending || lifecycle.Phase == "settlement_pending" {
							total.CostPending++
						}
						if attempt.ConvertedCostPending {
							total.ConvertedCostPending++
						}
						if !attempt.CostPending && !attempt.ConvertedCostPending && attempt.Cost.Nanos == 0 && lifecycle.Phase != "settlement_pending" {
							total.cost = total.cost.Add(decimal.NewFromInt(attempt.ConvertedCost.Nanos))
						}
					}
					foundCost := attempt.Cost.Nanos == 0
					for _, entryIndex := range byRequest[request.ID] {
						entry := entries[entryIndex]
						if index == 0 {
							if entry.AccountType != LedgerAccountProviderPayable && entry.Currency != SettlementCurrency() {
								return money.ErrCurrencyMismatch
							}
							switch entry.AccountType {
							case LedgerAccountPlatformRevenue:
								total.revenue = total.revenue.Add(entry.Amount)
							case LedgerAccountUserReceivable:
								total.outstanding = total.outstanding.Add(entry.Amount)
							case LedgerAccountUserWallet:
								total.collected = total.collected.Sub(entry.Amount)
							}
						} else if entry.AccountType == LedgerAccountProviderPayable && entry.AttemptId == attempt.ID {
							total.original[entry.Currency] = total.original[entry.Currency].Add(entry.Amount)
							if !attempt.CostPending {
								if entry.Currency != attempt.Cost.Currency || !entry.Initial.Equal(decimal.NewFromInt(attempt.Cost.Nanos)) {
									return fmt.Errorf("procurement ledger does not match frozen attempt %s", attempt.ID)
								}
								foundCost = true
							}
							if !attempt.ConvertedCostPending && !attempt.CostPending {
								converted := entry.Converted.Sub(entry.SnapshotConverted).Add(decimal.NewFromInt(attempt.ConvertedCost.Nanos).Mul(decimal.NewFromInt(entry.SnapshotPosts)))
								total.cost = total.cost.Add(converted)
							}
						}
					}
					if index > 0 && !attempt.CostPending && !foundCost && lifecycle.Phase != "settlement_pending" {
						return fmt.Errorf("missing procurement ledger for frozen attempt %s", attempt.ID)
					}
				}
			}
			return nil
		}).Error
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	keys := make([]string, 0, len(totals))
	for key := range totals {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	items := make([]MarginRow, 0, min(limit, len(keys)))
	for _, key := range keys[min(offset, len(keys)):min(offset+limit, len(keys))] {
		total := totals[key]
		for _, field := range []struct {
			amount *money.Amount
			value  decimal.Decimal
		}{{&total.Revenue, total.revenue}, {&total.Collected, total.collected}, {&total.Outstanding, total.outstanding}, {&total.Cost, total.cost}, {&total.KnownMargin, total.revenue.Sub(total.cost)}} {
			value, err := money.NewValue(field.value.Shift(-9), SettlementCurrency())
			if err != nil {
				return nil, 0, err
			}
			*field.amount, err = value.Cost()
			if err != nil {
				return nil, 0, err
			}
		}
		total.OriginalCosts = []money.Amount{}
		currencies := make([]string, 0, len(total.original))
		for currency := range total.original {
			currencies = append(currencies, string(currency))
		}
		slices.Sort(currencies)
		for _, currency := range currencies {
			value, err := money.NewValue(total.original[money.Currency(currency)].Shift(-9), money.Currency(currency))
			if err != nil {
				return nil, 0, err
			}
			amount, err := value.Cost()
			if err != nil {
				return nil, 0, err
			}
			total.OriginalCosts = append(total.OriginalCosts, amount)
		}
		if total.SalePending+total.CostPending+total.ConvertedCostPending == 0 {
			total.Margin = &total.KnownMargin
		}
		items = append(items, total.MarginRow)
	}
	return items, len(keys), nil
}
