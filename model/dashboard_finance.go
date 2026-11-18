package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

const MaxDashboardFinanceRange = 31 * 24 * time.Hour
const maxDashboardFinanceRows = 10000

var ErrDashboardFinanceLimit = errors.New("dashboard finance report exceeds the supported grouping limit")

// UserID is enforced by the self controller, never taken from a self query string.
// From/Until are a half-open request-start cohort, not ledger posting dates.
type DashboardFinanceFilter struct {
	UserID, TokenID, OfferingID, ChannelID, ProviderID int
	From, Until                                        int64
	Bucket, Timezone                                   string
	GroupBy                                            []string
	Admin                                              bool
}

type DashboardFinanceMetric struct {
	KnownAmount  MoneyTotal  `json:"known_amount"`
	TotalAmount  *MoneyTotal `json:"total_amount"`
	PendingCount string      `json:"pending_count"`
}

type DashboardFinanceProcurement struct {
	Cost                 DashboardFinanceMetric   `json:"cost"`
	OriginalCosts        []DashboardFinanceMetric `json:"original_costs"`
	PurchasePendingCount string                   `json:"purchase_pending_count"`
	FXPendingCount       string                   `json:"fx_pending_count"`
}

type DashboardFinanceTotals struct {
	RequestCount        string                           `json:"request_count"`
	InFlightCount       string                           `json:"in_flight_count"`
	SucceededCount      string                           `json:"succeeded_count"`
	FailedCount         string                           `json:"failed_count"`
	OtherTerminalCount  string                           `json:"other_terminal_count"`
	Revenue             DashboardFinanceMetric           `json:"revenue"`
	Collected           DashboardFinanceMetric           `json:"collected"`
	Outstanding         DashboardFinanceMetric           `json:"outstanding"`
	FundingPendingCount string                           `json:"funding_pending_count"`
	Usage               map[string]DashboardFinanceUsage `json:"usage"`
	Procurement         *DashboardFinanceProcurement     `json:"procurement,omitempty"`
	Margin              *DashboardFinanceMetric          `json:"margin,omitempty"`
}

type DashboardFinanceBucket struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

type DashboardFinanceRow struct {
	BucketStart int64             `json:"bucket_start"`
	BucketEnd   int64             `json:"bucket_end"`
	Dimensions  map[string]int    `json:"dimensions"`
	Names       map[string]string `json:"names"`
	DashboardFinanceTotals
}

type DashboardFinanceReport struct {
	From                    int64                    `json:"from"`
	Until                   int64                    `json:"until"`
	Bucket                  string                   `json:"bucket"`
	Timezone                string                   `json:"timezone"`
	GroupBy                 []string                 `json:"group_by"`
	TimeBasis               string                   `json:"time_basis"`
	Coverage                string                   `json:"coverage"`
	RevenueAttribution      string                   `json:"revenue_attribution"`
	CostAttribution         string                   `json:"cost_attribution,omitempty"`
	PendingCountBasis       string                   `json:"pending_count_basis"`
	UnattributedAdjustments string                   `json:"unattributed_adjustments"`
	Buckets                 []DashboardFinanceBucket `json:"buckets"`
	Totals                  DashboardFinanceTotals   `json:"totals"`
	Items                   []DashboardFinanceRow    `json:"items"`
}

// Validate also protects direct callers; no SQL identifiers come from the filter.
func (f DashboardFinanceFilter) Validate() error {
	if f.From <= 0 || f.Until <= f.From || f.Until-f.From > MaxDashboardFinanceRange.Milliseconds() || (!f.Admin && f.UserID <= 0) {
		return common.NewMessage("Invalid parameters")
	}
	if f.UserID < 0 || f.TokenID < 0 || f.OfferingID < 0 || f.ChannelID < 0 || f.ProviderID < 0 || (!f.Admin && (f.ChannelID != 0 || f.ProviderID != 0)) {
		return common.NewMessage("Invalid parameters")
	}
	if f.Timezone == "" || f.Timezone == "Local" {
		return common.NewMessage("Invalid parameters")
	}
	if _, err := time.LoadLocation(f.Timezone); err != nil {
		return common.NewMessage("Invalid parameters")
	}
	if !slices.Contains([]string{"hour", "day", "week"}, f.Bucket) {
		return common.NewMessage("Invalid parameters")
	}
	seen := map[string]bool{}
	for _, dimension := range f.GroupBy {
		if seen[dimension] || !slices.Contains([]string{"user", "token", "model_offering", "channel", "provider"}, dimension) || (!f.Admin && dimension != "token" && dimension != "model_offering") {
			return common.NewMessage("Invalid parameters")
		}
		seen[dimension] = true
	}
	return nil
}

// Hour buckets are elapsed hours anchored at local midnight, capped at the next
// midnight. This preserves both repeated DST hours and non-hour DST transitions.
// Day/week use calendar boundaries (ISO Monday), not fixed 24/168-hour durations.
func dashboardFinanceBucket(at time.Time, location *time.Location, grain string) DashboardFinanceBucket {
	local := at.In(location)
	// Calendar arithmetic is performed on a UTC date label, not on a normalized
	// local midnight: some IANA dates begin at 01:00 or are skipped altogether.
	date := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	days := 1
	if grain == "week" {
		date = date.AddDate(0, 0, -(int(local.Weekday())+6)%7)
		days = 7
	}
	start := dashboardCivilBoundary(date, location)
	end := dashboardCivilBoundary(date.AddDate(0, 0, days), location)
	if grain == "hour" {
		start = start.Add(at.Sub(start).Truncate(time.Hour))
		if next := start.Add(time.Hour); next.Before(end) {
			end = next
		}
	}
	return DashboardFinanceBucket{Start: start.UnixMilli(), End: end.UnixMilli()}
}

func dashboardCivilBoundary(date time.Time, location *time.Location) time.Time {
	ordinal := func(at time.Time) int { return at.Year()*10000 + int(at.Month())*100 + at.Day() }
	target := ordinal(date)
	boundary := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, location)
	if ordinal(boundary) == target && ordinal(boundary.Add(-time.Millisecond)) < target {
		return boundary
	}
	// time.Date may normalize a nonexistent midnight into the preceding date.
	// Locate the actual first instant at/after the requested civil date instead.
	low, high := date.Add(-36*time.Hour).UnixMilli(), date.Add(36*time.Hour).UnixMilli()
	for high-low > 1 {
		mid := low + (high-low)/2
		if ordinal(time.UnixMilli(mid).In(location)) >= target {
			high = mid
		} else {
			low = mid
		}
	}
	return time.UnixMilli(high).In(location)
}

type dashboardFinanceAccumulator struct {
	revenue, collected, outstanding, cost                                               decimal.Decimal
	requests, inFlight, succeeded, failed, other                                        decimal.Decimal
	salePending, fundingPending, purchasePending, fxPending, costPending, marginPending decimal.Decimal
	original                                                                            map[money.Currency]decimal.Decimal
	usage                                                                               map[string]*dashboardUsageAccumulator
}

func newDashboardFinanceAccumulator() *dashboardFinanceAccumulator {
	return &dashboardFinanceAccumulator{original: map[money.Currency]decimal.Decimal{}, usage: newDashboardUsage()}
}

func dashboardMetric(value decimal.Decimal, currency money.Currency, pending decimal.Decimal) DashboardFinanceMetric {
	known := MoneyTotal{Nanos: value.StringFixed(0), Currency: currency}
	metric := DashboardFinanceMetric{KnownAmount: known, PendingCount: pending.StringFixed(0)}
	if pending.IsZero() {
		metric.TotalAmount = &known
	}
	return metric
}

func (a *dashboardFinanceAccumulator) result(admin bool, currency money.Currency) DashboardFinanceTotals {
	result := DashboardFinanceTotals{
		RequestCount: a.requests.StringFixed(0), InFlightCount: a.inFlight.StringFixed(0), SucceededCount: a.succeeded.StringFixed(0), FailedCount: a.failed.StringFixed(0), OtherTerminalCount: a.other.StringFixed(0),
		Revenue: dashboardMetric(a.revenue, currency, a.salePending), Collected: dashboardMetric(a.collected, currency, a.salePending.Add(a.fundingPending)), Outstanding: dashboardMetric(a.outstanding, currency, a.salePending.Add(a.fundingPending)), Usage: dashboardUsageResult(a.usage),
		FundingPendingCount: a.fundingPending.StringFixed(0),
	}
	if admin {
		p := &DashboardFinanceProcurement{Cost: dashboardMetric(a.cost, currency, a.costPending), OriginalCosts: []DashboardFinanceMetric{}, PurchasePendingCount: a.purchasePending.StringFixed(0), FXPendingCount: a.fxPending.StringFixed(0)}
		currencies := make([]money.Currency, 0, len(a.original))
		for c := range a.original {
			currencies = append(currencies, c)
		}
		slices.Sort(currencies)
		for _, c := range currencies {
			// Complete original-cost totals are withheld while any procurement is
			// unresolved; an unknown price can also have an unknown currency.
			p.OriginalCosts = append(p.OriginalCosts, dashboardMetric(a.original[c], c, a.purchasePending))
		}
		result.Procurement = p
		margin := dashboardMetric(a.revenue.Sub(a.cost), currency, a.marginPending)
		result.Margin = &margin
	}
	return result
}

func (a *dashboardFinanceAccumulator) add(b *dashboardFinanceAccumulator) {
	for _, field := range []struct {
		target *decimal.Decimal
		value  decimal.Decimal
	}{
		{&a.revenue, b.revenue}, {&a.collected, b.collected}, {&a.outstanding, b.outstanding}, {&a.cost, b.cost},
		{&a.requests, b.requests}, {&a.inFlight, b.inFlight}, {&a.succeeded, b.succeeded}, {&a.failed, b.failed}, {&a.other, b.other},
		{&a.salePending, b.salePending}, {&a.fundingPending, b.fundingPending}, {&a.purchasePending, b.purchasePending}, {&a.fxPending, b.fxPending}, {&a.costPending, b.costPending}, {&a.marginPending, b.marginPending},
	} {
		*field.target = field.target.Add(field.value)
	}
	for c, amount := range b.original {
		a.original[c] = a.original[c].Add(amount)
	}
	addDashboardUsage(a.usage, b.usage)
}

// A contribution holds one request's portion of a dimension row. Pending flags
// are booleans until merged, so two unfinished attempts never double count a
// request. Overall totals are formed independently of dimension fan-out.
type dashboardFinanceContribution struct {
	*dashboardFinanceAccumulator
	sale, purchase, fx bool
}

func newDashboardFinanceContribution() *dashboardFinanceContribution {
	return &dashboardFinanceContribution{dashboardFinanceAccumulator: newDashboardFinanceAccumulator()}
}
func (c *dashboardFinanceContribution) finish() {
	one := decimal.NewFromInt(1)
	if c.sale {
		c.salePending = one
	}
	if c.purchase {
		c.purchasePending = one
	}
	if c.fx {
		c.fxPending = one
	}
	if c.purchase || c.fx {
		c.costPending = one
	}
	if c.sale || c.purchase || c.fx {
		c.marginPending = one
	}
}

type dashboardLedgerAggregate struct {
	RequestId, AttemptId                          string
	AccountType                                   LedgerAccountType
	Currency                                      money.Currency
	Amount, Converted, Initial, SnapshotConverted decimal.Decimal
	SnapshotPosts                                 int64
}

// This is the same frozen-cost rule as MarginReport: never reconvert the initial
// rounded payable instead of its independently rounded ConvertedCost snapshot.
func dashboardLedgerAggregates(tx *gorm.DB, ids []string) (map[string][]dashboardLedgerAggregate, error) {
	var entries []dashboardLedgerAggregate
	err := tx.Model(&LedgerEntry{}).Select(`"request_id", "attempt_id", "account_type", "amount_currency" AS "currency", SUM("amount_nanos") AS "amount", SUM(ROUND("amount_nanos"::numeric * COALESCE("fx_rate", 1))) AS "converted",
 COALESCE(SUM("amount_nanos") FILTER (WHERE "idempotency_key" = 'gateway:' || "attempt_id" || ':payable'), 0) AS "initial",
 SUM(CASE WHEN "idempotency_key" = 'gateway:' || "attempt_id" || ':payable' THEN 1 WHEN "reverses_entry_id" IN (SELECT "id" FROM "ledger_entries" AS "source" WHERE "source"."idempotency_key" = 'gateway:' || "ledger_entries"."attempt_id" || ':payable') THEN -1 ELSE 0 END) AS "snapshot_posts",
 SUM(CASE WHEN "idempotency_key" = 'gateway:' || "attempt_id" || ':payable' OR "reverses_entry_id" IN (SELECT "id" FROM "ledger_entries" AS "source" WHERE "source"."idempotency_key" = 'gateway:' || "ledger_entries"."attempt_id" || ':payable') THEN ROUND("amount_nanos"::numeric * COALESCE("fx_rate", 1)) ELSE 0 END) AS "snapshot_converted"`).
		Where(`"request_id" IN ? AND ("account_type" IN ? OR ("account_type" IN ? AND "type" IN ?))`, ids, []LedgerAccountType{LedgerAccountPlatformRevenue, LedgerAccountProviderPayable, LedgerAccountUserReceivable}, []LedgerAccountType{LedgerAccountUserWallet, LedgerAccountSubscription, LedgerAccountSubscriptionUnlimited}, []LedgerEntryType{LedgerEntryCharge, LedgerEntryRepayment, LedgerEntryRefund}).
		Group(`"request_id", "attempt_id", "account_type", "amount_currency"`).Scan(&entries).Error
	result := map[string][]dashboardLedgerAggregate{}
	for _, entry := range entries {
		result[entry.RequestId] = append(result[entry.RequestId], entry)
	}
	return result, err
}

func dashboardAddSale(c *dashboardFinanceContribution, lifecycle GatewayBilling, request GatewayRequest, entries []dashboardLedgerAggregate, currency money.Currency) error {
	one := decimal.NewFromInt(1)
	c.requests = one
	inFlight := lifecycle.Phase == "reserved"
	if inFlight {
		c.inFlight = one
	} else {
		switch request.Status {
		case "success":
			c.succeeded = one
		case "failed":
			c.failed = one
		default:
			c.other = one
		}
	}
	c.sale = inFlight || lifecycle.Phase == "settlement_pending" || request.SalePending
	if request.FundingPending {
		c.fundingPending = one
	}
	if request.Receivable.Currency != "" && request.Receivable.Currency != currency {
		return money.ErrCurrencyMismatch
	}
	for _, entry := range entries {
		if entry.AccountType == LedgerAccountProviderPayable {
			continue
		}
		if entry.Currency != currency {
			return money.ErrCurrencyMismatch
		}
		switch entry.AccountType {
		case LedgerAccountPlatformRevenue:
			c.revenue = c.revenue.Add(entry.Amount)
		case LedgerAccountUserReceivable:
			c.outstanding = c.outstanding.Add(entry.Amount)
		case LedgerAccountUserWallet, LedgerAccountSubscription, LedgerAccountSubscriptionUnlimited:
			c.collected = c.collected.Sub(entry.Amount)
		}
	}
	return dashboardAddUsage(c.usage, request, inFlight)
}

func dashboardAddCost(c *dashboardFinanceContribution, lifecycle GatewayBilling, attempt GatewayAttempt, entries []dashboardLedgerAggregate, currency money.Currency) error {
	pendingPost := lifecycle.Phase == "settlement_pending"
	c.purchase = c.purchase || attempt.CostPending || pendingPost
	c.fx = c.fx || attempt.ConvertedCostPending
	if !attempt.CostPending {
		if _, err := money.ParseCurrency(string(attempt.Cost.Currency)); err != nil {
			return err
		}
		if _, ok := c.original[attempt.Cost.Currency]; !ok {
			c.original[attempt.Cost.Currency] = decimal.Zero
		}
	}
	if !attempt.CostPending && !attempt.ConvertedCostPending && attempt.ConvertedCost.Currency != currency {
		return money.ErrCurrencyMismatch
	}
	found := attempt.Cost.Nanos == 0
	if !attempt.CostPending && !attempt.ConvertedCostPending && attempt.Cost.Nanos == 0 && !pendingPost {
		c.cost = c.cost.Add(decimal.NewFromInt(attempt.ConvertedCost.Nanos))
	}
	for _, entry := range entries {
		if entry.AccountType != LedgerAccountProviderPayable || entry.AttemptId != attempt.ID {
			continue
		}
		if _, err := money.ParseCurrency(string(entry.Currency)); err != nil {
			return err
		}
		c.original[entry.Currency] = c.original[entry.Currency].Add(entry.Amount)
		if !attempt.CostPending {
			if entry.Currency != attempt.Cost.Currency || !entry.Initial.Equal(decimal.NewFromInt(attempt.Cost.Nanos)) {
				return fmt.Errorf("procurement ledger does not match frozen attempt")
			}
			found = true
		}
		if !attempt.ConvertedCostPending && !attempt.CostPending {
			c.cost = c.cost.Add(entry.Converted.Sub(entry.SnapshotConverted).Add(decimal.NewFromInt(attempt.ConvertedCost.Nanos).Mul(decimal.NewFromInt(entry.SnapshotPosts))))
		}
	}
	if !attempt.CostPending && !found && !pendingPost {
		return fmt.Errorf("missing procurement ledger for frozen attempt")
	}
	return nil
}

func dashboardDimensions(filter DashboardFinanceFilter, request GatewayRequest, attempt GatewayAttempt) map[string]int {
	values := map[string]int{"user": request.UserID, "token": request.TokenID, "model_offering": request.ModelOfferingID, "channel": attempt.ChannelID, "provider": attempt.ProviderID}
	result := map[string]int{}
	for _, dimension := range filter.GroupBy {
		result[dimension] = values[dimension]
	}
	return result
}
func dashboardDimensionKey(bucket DashboardFinanceBucket, dimensions map[string]int) string {
	return fmt.Sprintf("%020d/%020d/%020d/%020d/%020d/%020d", bucket.Start, dimensions["user"], dimensions["token"], dimensions["model_offering"], dimensions["channel"], dimensions["provider"])
}

// DashboardFinance uses a single repeatable-read primary snapshot for lifecycle,
// ledger, totals, usage and optional labels. It never reads LOG_DB or legacy quota.
func DashboardFinance(ctx context.Context, filter DashboardFinanceFilter) (*DashboardFinanceReport, error) {
	if err := filter.Validate(); err != nil {
		return nil, err
	}
	location, _ := time.LoadLocation(filter.Timezone)
	currency := SettlementCurrency()
	report := &DashboardFinanceReport{From: filter.From, Until: filter.Until, Bucket: filter.Bucket, Timezone: filter.Timezone, GroupBy: append([]string{}, filter.GroupBy...), TimeBasis: "request_started_at", Coverage: "primary_gateway_only", RevenueAttribution: "last_attempt", PendingCountBasis: "distinct_requests", UnattributedAdjustments: "excluded", Buckets: []DashboardFinanceBucket{}, Items: []DashboardFinanceRow{}}
	if filter.Admin {
		report.CostAttribution = "all_attempts"
	}
	for at := filter.From; at < filter.Until; {
		bucket := dashboardFinanceBucket(time.UnixMilli(at), location, filter.Bucket)
		if bucket.End <= at || len(report.Buckets) >= 1000 {
			return nil, common.NewMessage("Invalid parameters")
		}
		report.Buckets = append(report.Buckets, bucket)
		at = bucket.End
	}
	total := newDashboardFinanceAccumulator()
	groups := map[string]*dashboardFinanceAccumulator{}
	rows := map[string]DashboardFinanceRow{}
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Model(&GatewayBilling{}).Order("request_id").Where(`("request_data"::jsonb ->> 'started_at')::bigint >= ? AND ("request_data"::jsonb ->> 'started_at')::bigint < ?`, filter.From, filter.Until)
		if filter.UserID > 0 {
			query = query.Where("user_id = ?", filter.UserID)
		}
		// These identities are frozen at reservation. Final usage/status are not.
		if filter.TokenID > 0 {
			query = query.Where(`("request_data"::jsonb ->> 'token_id')::bigint = ?`, filter.TokenID)
		}
		if filter.OfferingID > 0 {
			query = query.Where(`("request_data"::jsonb ->> 'model_offering_id')::bigint = ?`, filter.OfferingID)
		}
		var batch []GatewayBilling
		if err := query.FindInBatches(&batch, 250, func(_ *gorm.DB, _ int) error {
			ids := make([]string, len(batch))
			for i := range batch {
				ids[i] = batch[i].RequestID
			}
			ledger, err := dashboardLedgerAggregates(tx, ids)
			if err != nil {
				return err
			}
			for _, lifecycle := range batch {
				var request GatewayRequest
				if err := common.UnmarshalJsonStr(lifecycle.RequestData, &request); err != nil {
					return err
				}
				var intent GatewaySettlement
				if lifecycle.Settlement != "" {
					if err := common.UnmarshalJsonStr(lifecycle.Settlement, &intent); err != nil {
						return err
					}
					if intent.Request.ID != request.ID || intent.Request.UserID != request.UserID || intent.Request.StartedAt != request.StartedAt || intent.Request.TokenID != request.TokenID || intent.Request.ModelOfferingID != request.ModelOfferingID {
						return fmt.Errorf("inconsistent frozen request identity")
					}
					request = intent.Request
				}
				if request.ID != lifecycle.RequestID || request.UserID != lifecycle.UserID {
					return fmt.Errorf("inconsistent request owner")
				}
				if !slices.Contains([]string{"reserved", "settlement_pending", "settled", "evaluation_failed", "funding_pending"}, lifecycle.Phase) {
					return fmt.Errorf("invalid request accounting phase")
				}
				request.SalePending = request.SalePending || intent.SaleEvaluationFailed
				attempts := intent.Attempts
				if intent.Attempt != nil {
					attempts = []GatewayAttempt{*intent.Attempt}
				}
				final := GatewayAttempt{}
				if len(attempts) > 0 {
					final = attempts[len(attempts)-1]
				}
				bucket := dashboardFinanceBucket(time.UnixMilli(request.StartedAt), location, filter.Bucket)
				parts := map[string]*dashboardFinanceContribution{}
				whole := newDashboardFinanceContribution()
				part := func(attempt GatewayAttempt) *dashboardFinanceContribution {
					dimensions := dashboardDimensions(filter, request, attempt)
					key := dashboardDimensionKey(bucket, dimensions)
					if parts[key] == nil {
						parts[key] = newDashboardFinanceContribution()
						rows[key] = DashboardFinanceRow{BucketStart: bucket.Start, BucketEnd: bucket.End, Dimensions: dimensions, Names: map[string]string{}}
					}
					return parts[key]
				}
				matches := func(attempt GatewayAttempt) bool {
					return (filter.ChannelID == 0 || filter.ChannelID == attempt.ChannelID) && (filter.ProviderID == 0 || filter.ProviderID == attempt.ProviderID)
				}
				if matches(final) {
					for _, target := range []*dashboardFinanceContribution{whole, part(final)} {
						if err := dashboardAddSale(target, lifecycle, request, ledger[request.ID], currency); err != nil {
							return err
						}
						if lifecycle.Phase == "reserved" {
							target.purchase, target.fx = true, true
						}
					}
				}
				seenAttempts := map[string]bool{}
				for _, attempt := range attempts {
					if attempt.ID == "" || seenAttempts[attempt.ID] || attempt.RequestID != request.ID {
						return fmt.Errorf("invalid frozen attempt identity")
					}
					seenAttempts[attempt.ID] = true
					if !filter.Admin || !matches(attempt) {
						continue
					}
					for _, target := range []*dashboardFinanceContribution{whole, part(attempt)} {
						if err := dashboardAddCost(target, lifecycle, attempt, ledger[request.ID], currency); err != nil {
							return err
						}
					}
				}
				whole.finish()
				total.add(whole.dashboardFinanceAccumulator)
				for key, contribution := range parts {
					contribution.finish()
					if groups[key] == nil {
						groups[key] = newDashboardFinanceAccumulator()
					}
					groups[key].add(contribution.dashboardFinanceAccumulator)
				}
				if len(groups) > maxDashboardFinanceRows {
					return ErrDashboardFinanceLimit
				}
			}
			return nil
		}).Error; err != nil {
			return err
		}
		keys := make([]string, 0, len(groups))
		for key := range groups {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			row := rows[key]
			row.DashboardFinanceTotals = groups[key].result(filter.Admin, currency)
			report.Items = append(report.Items, row)
		}
		report.Totals = total.result(filter.Admin, currency)
		return dashboardFinanceNames(tx, report.Items)
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return report, err
}

// Bulk optional name enrichment is a left-side display lookup: names never
// choose ownership, membership or aggregation, and deleted rows never drop facts.
func dashboardFinanceNames(tx *gorm.DB, rows []DashboardFinanceRow) error {
	for _, field := range []struct{ dimension, table, column string }{
		{"user", "users", "username"}, {"token", "tokens", "name"}, {"model_offering", "model_offerings", "name"}, {"channel", "channels", "upstream_model_name"}, {"provider", "providers", "name"},
	} {
		ids := map[int]bool{}
		for _, row := range rows {
			if id := row.Dimensions[field.dimension]; id > 0 {
				ids[id] = true
			}
		}
		if len(ids) == 0 {
			continue
		}
		values := make([]int, 0, len(ids))
		for id := range ids {
			values = append(values, id)
		}
		var labels []struct {
			ID   int
			Name string
		}
		if err := tx.Table(field.table).Select("id, "+field.column+" AS name").Where("id IN ?", values).Scan(&labels).Error; err != nil {
			return err
		}
		names := map[int]string{}
		for _, label := range labels {
			names[label.ID] = label.Name
		}
		for i := range rows {
			if name, ok := names[rows[i].Dimensions[field.dimension]]; ok {
				rows[i].Names[field.dimension] = name
			}
		}
	}
	return nil
}
