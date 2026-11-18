package model

import (
	"context"

	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

const MaxWalletSummaryUsers = 100

// MoneyTotal is a read-only aggregate, not an amount that can be posted. Its
// signed integer nanos may exceed the storage range of an individual entry.
type MoneyTotal struct {
	Nanos    string         `json:"nanos"`
	Currency money.Currency `json:"currency"`
}

// UserWalletSummary reports posted consumption separately from spendable funds,
// unpaid debt and sales still awaiting accounting. Repayments are not new sales.
type UserWalletSummary struct {
	UserID         int          `json:"user_id"`
	Available      money.Amount `json:"available"`
	Outstanding    money.Amount `json:"outstanding"`
	Consumed       MoneyTotal   `json:"consumed"`
	SalePending    bool         `json:"sale_pending"`
	FundingPending bool         `json:"funding_pending"`
	RequestsPaused bool         `json:"requests_paused"`
	UpdatedAt      int64        `json:"-"`
}

// GetUserWalletSummaries reads one primary-database snapshot for the entire batch.
// Revenue entries include unpaid charges and signed corrections; log delivery,
// reservations, top-ups and debt repayments never supply the consumption total.
func GetUserWalletSummaries(ctx context.Context, userIDs []int) ([]UserWalletSummary, error) {
	if len(userIDs) == 0 || len(userIDs) > MaxWalletSummaryUsers {
		return nil, ErrLedgerInvalidEntry
	}
	seen := make(map[int]bool, len(userIDs))
	for _, id := range userIDs {
		if id <= 0 || seen[id] {
			return nil, ErrLedgerInvalidEntry
		}
		seen[id] = true
	}
	db := DB.WithContext(ctx)
	consumption := db.Model(&LedgerEntry{}).
		Select(`"gateway_billings"."user_id", "ledger_entries"."amount_currency" AS "currency", SUM("ledger_entries"."amount_nanos") AS "nanos"`).
		Joins(`JOIN "gateway_billings" ON "gateway_billings"."request_id" = "ledger_entries"."request_id"`).
		Where(`"gateway_billings"."user_id" IN ? AND "ledger_entries"."account_type" = ?`, userIDs, LedgerAccountPlatformRevenue).
		Group(`"gateway_billings"."user_id", "ledger_entries"."amount_currency"`)
	pending := gatewayPendingSales(db, userIDs...).Where(`"phase" <> ?`, "funding_pending").Select(`DISTINCT "user_id"`)
	funding := db.Model(&GatewayBilling{}).Where(`"user_id" IN ? AND "phase" = ?`, userIDs, "funding_pending").Select(`DISTINCT "user_id"`)
	var rows []struct {
		ID                  int
		AvailableNanos      int64
		AvailableCurrency   money.Currency
		OutstandingNanos    int64
		OutstandingCurrency money.Currency
		ConsumedNanos       decimal.Decimal
		ConsumedCurrency    money.Currency
		SalePending         bool
		FundingPending      bool
		RequestsPaused      bool
		UpdatedAt           int64
	}
	currency := SettlementCurrency()
	err := db.Model(&User{}).Select(`"users"."id",
 COALESCE("available"."balance_nanos", 0) AS "available_nanos",
 COALESCE("available"."balance_currency", ?) AS "available_currency",
 COALESCE("outstanding"."balance_nanos", 0) AS "outstanding_nanos",
 COALESCE("outstanding"."balance_currency", ?) AS "outstanding_currency",
 COALESCE("consumption"."nanos", 0) AS "consumed_nanos",
 COALESCE("consumption"."currency", ?) AS "consumed_currency",
 "pending"."user_id" IS NOT NULL AS "sale_pending",
 "funding"."user_id" IS NOT NULL AS "funding_pending",
 COALESCE("available"."requests_paused", false) AS "requests_paused",
 COALESCE("available"."updated_at", 0) AS "updated_at"`, currency, currency, currency).
		Joins(`LEFT JOIN "wallets" AS "available" ON "available"."owner_id" = "users"."id" AND "available"."account_type" = ?`, LedgerAccountUserWallet).
		Joins(`LEFT JOIN "wallets" AS "outstanding" ON "outstanding"."owner_id" = "users"."id" AND "outstanding"."account_type" = ?`, LedgerAccountUserReceivable).
		Joins(`LEFT JOIN (?) AS "consumption" ON "consumption"."user_id" = "users"."id"`, consumption).
		Joins(`LEFT JOIN (?) AS "pending" ON "pending"."user_id" = "users"."id"`, pending).
		Joins(`LEFT JOIN (?) AS "funding" ON "funding"."user_id" = "users"."id"`, funding).
		Where(`"users"."id" IN ?`, userIDs).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	byUser := make(map[int]UserWalletSummary, len(rows))
	for _, row := range rows {
		if row.AvailableCurrency != currency || row.OutstandingCurrency != currency || row.ConsumedCurrency != currency {
			return nil, money.ErrCurrencyMismatch
		}
		byUser[row.ID] = UserWalletSummary{
			UserID:         row.ID,
			Available:      money.Amount{Nanos: row.AvailableNanos, Currency: currency},
			Outstanding:    money.Amount{Nanos: row.OutstandingNanos, Currency: currency},
			Consumed:       MoneyTotal{Nanos: row.ConsumedNanos.StringFixed(0), Currency: currency},
			SalePending:    row.SalePending,
			FundingPending: row.FundingPending,
			RequestsPaused: row.RequestsPaused || row.SalePending || row.FundingPending || row.OutstandingNanos > 0,
			UpdatedAt:      row.UpdatedAt,
		}
	}
	result := make([]UserWalletSummary, 0, len(userIDs))
	for _, id := range userIDs {
		item, exists := byUser[id]
		if !exists {
			return nil, gorm.ErrRecordNotFound
		}
		result = append(result, item)
	}
	return result, nil
}
