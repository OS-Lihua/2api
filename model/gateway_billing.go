package model

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// GatewayBilling is the primary-database lifecycle and log delivery outbox.
// A free request still has a lifecycle row, without zero ledger postings.
// RequestData and Settlement contain snapshots and quantitative facts only.
type GatewayBilling struct {
	RequestID   string       `gorm:"type:varchar(64);primaryKey"`
	UserID      int          `gorm:"not null;index"`
	Reservation money.Amount `gorm:"embedded;embeddedPrefix:reservation_"`
	RequestData string       `gorm:"type:text;not null"`
	Settlement  string       `gorm:"type:text;not null"`
	Phase       string       `gorm:"type:varchar(32);not null;index"`
	Published   bool         `gorm:"not null;index"`
	CreatedAt   int64        `gorm:"not null"`
	UpdatedAt   int64        `gorm:"not null"`
}

var ErrGatewayEvaluationPending = errors.New("gateway billing evaluation requires reconciliation")

type GatewaySettlement struct {
	SaleTrace            *billingexpr.MoneyTraceResult           `json:"sale_trace,omitempty"`
	AttemptTrace         map[string]billingexpr.MoneyTraceResult `json:"attempt_trace,omitempty"`
	SaleUsage            *dto.Usage                              `json:"sale_usage,omitempty"`
	AttemptUsage         map[string]*dto.Usage                   `json:"attempt_usage,omitempty"`
	SaleEvaluationFailed bool                                    `json:"sale_evaluation_failed,omitempty"`
	EvaluationFailed     bool                                    `json:"evaluation_failed,omitempty"`
	Request              GatewayRequest                          `json:"request"`
	Attempt              *GatewayAttempt                         `json:"attempt,omitempty"`
	Attempts             []GatewayAttempt                        `json:"attempts,omitempty"`
	SaleSource           money.Amount                            `json:"sale_source"`
	ReceivableDue        money.Amount                            `json:"receivable_due"`
	PostedSaleSource     money.Amount                            `json:"posted_sale_source"`
}

func gatewayPendingAccounting(db *gorm.DB) *gorm.DB {
	return db.Model(&GatewayBilling{}).Where(`"phase" IN ?`, []string{"settlement_pending", "evaluation_failed"})
}

func gatewayPendingSales(db *gorm.DB, userID int) *gorm.DB {
	return gatewayPendingAccounting(db).Where(`"user_id" = ? AND ("phase" = ? OR NULLIF("settlement", '')::jsonb ->> 'sale_evaluation_failed' = 'true')`, userID, "settlement_pending")
}

// HasPendingGatewaySale reports sales awaiting valuation or posting. Credits
// cannot make their reserved funds safe to spend before reconciliation.
func HasPendingGatewaySale(ctx context.Context, userID int) (bool, error) {
	var count int64
	err := gatewayPendingSales(DB.WithContext(ctx), userID).Count(&count).Error
	return count > 0, err
}

func MigrateGatewayBilling(db *gorm.DB) error {
	if err := migrateWalletLedger(db); err != nil {
		return err
	}
	if err := db.AutoMigrate(&GatewayBilling{}); err != nil {
		return err
	}
	return db.Exec(`CREATE INDEX IF NOT EXISTS "idx_gateway_billing_recovery" ON "gateway_billings" ("updated_at", "request_id") WHERE "phase" <> 'settled'`).Error
}

// ReserveGateway writes the request lifecycle and reservation in one primary
// transaction. Reusing a request ID cannot dispatch another Provider attempt.
func ReserveGateway(request *GatewayRequest, reservation, source money.Amount) error {
	return reserveGateway(request, reservation, source, nil)
}

// ReserveGatewayTask persists task recovery identity and the single reservation
// in the same primary transaction, before any Provider I/O.
func ReserveGatewayTask(request *GatewayRequest, reservation, source money.Amount, task *Task) error {
	if task == nil || task.UserId != request.UserID || task.PrivateData.Gateway == nil || task.PrivateData.Key != "" {
		return ErrLedgerInvalidEntry
	}
	return reserveGateway(request, reservation, source, task)
}

func reserveGateway(request *GatewayRequest, reservation, source money.Amount, task *Task) error {
	if request.ID == "" || request.UserID <= 0 || reservation.Currency != SettlementCurrency() || reservation.Nanos < 0 {
		return ErrLedgerInvalidEntry
	}
	if err := PreflightGatewaySettlementStorage(request.ID); err != nil {
		return err
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		row := GatewayBilling{RequestID: request.ID, UserID: request.UserID, Reservation: reservation, Phase: "reserved", CreatedAt: clock.Now().UnixMilli(), UpdatedAt: clock.Now().UnixMilli()}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		account := UserWalletAccount(request.UserID)
		wallets, err := lockLedgerWallets(tx, []LedgerPosting{{Account: account}})
		if err != nil {
			return err
		}
		wallet := wallets[account]
		var unresolved int64
		if err := gatewayPendingSales(tx, request.UserID).Count(&unresolved).Error; err != nil {
			return err
		}
		if unresolved > 0 || wallet.RequestsPaused || wallets[UserReceivableAccount(request.UserID)].Balance.Nanos > 0 || wallet.Balance.Nanos < reservation.Nanos {
			return ErrInsufficientWalletBalance
		}
		request.LedgerEntryIDs = []string{}
		if reservation.Nanos > 0 {
			rate, err := decimal.NewFromString(request.SaleFXRate)
			if err != nil {
				return err
			}
			debitSource := money.Amount{Nanos: -source.Nanos, Currency: source.Currency}
			posting := LedgerPosting{Account: account, Type: LedgerEntryPreConsume, Amount: money.Amount{Nanos: -reservation.Nanos, Currency: reservation.Currency}, RequestId: request.ID, IdempotencyKey: "gateway:" + request.ID + ":reserve", sourceAmount: &debitSource, fxRate: decimal.NewNullDecimal(rate)}
			if err := validateLedgerPosting(posting); err != nil {
				return err
			}
			entries, err := postLedgerEntries(tx, []LedgerPosting{posting})
			if err != nil {
				return err
			}
			request.LedgerEntryIDs = append(request.LedgerEntryIDs, strconv.FormatInt(entries[0].Id, 10))
		}
		encoded, err := common.Marshal(request)
		if err != nil {
			return err
		}
		if err := tx.Model(&row).Update("request_data", string(encoded)).Error; err != nil {
			return err
		}
		if task != nil {
			task.PrivateData.Gateway.Settlement.Request = *request
			return tx.Create(task).Error
		}
		return nil
	})
}

// PrepareGatewaySettlement freezes the final usage and amounts before posting.
// Once prepared, both ordinary completion and the recovery job use the same
// immutable intent; a retry never recomputes prices or adds another charge.
func PrepareGatewaySettlement(settlement GatewaySettlement) error {
	if settlement.ReceivableDue.Currency == "" {
		settlement.ReceivableDue = settlement.Request.Receivable
	}
	settlement, path, err := journalGatewaySettlement(settlement)
	if err != nil {
		return err
	}
	encoded, err := common.Marshal(settlement)
	if err != nil {
		return err
	}
	err = DB.Transaction(func(tx *gorm.DB) error {
		var row GatewayBilling
		if err := lockForUpdate(tx).First(&row, "request_id = ?", settlement.Request.ID).Error; err != nil {
			return err
		}
		if row.UserID != settlement.Request.UserID {
			return ErrLedgerInvalidEntry
		}
		if row.Phase != "reserved" {
			return nil
		}
		return tx.Model(&row).Updates(map[string]any{"settlement": string(encoded), "phase": "settlement_pending", "updated_at": clock.Now().UnixMilli()}).Error
	})
	if err != nil {
		return err
	}
	return removeGatewaySettlementJournal(path)
}

// SettleGateway atomically releases the reservation, charges once, recognizes
// revenue and records Provider payable in its original currency. A shortfall
// remains in the user's receivable account until subsequent credits pay it.
func SettleGateway(requestID string) (*GatewayRequest, error) {
	var result GatewayRequest
	var pending bool
	err := DB.Transaction(func(tx *gorm.DB) error {
		var row GatewayBilling
		if err := lockForUpdate(tx).First(&row, "request_id = ?", requestID).Error; err != nil {
			return err
		}
		if row.Phase == "settled" {
			return common.UnmarshalJsonStr(row.RequestData, &result)
		}
		if row.Phase == "evaluation_failed" {
			return ErrGatewayEvaluationPending
		}
		if row.Phase != "settlement_pending" {
			return errors.New("gateway settlement is not prepared")
		}
		var intent GatewaySettlement
		if err := common.UnmarshalJsonStr(row.Settlement, &intent); err != nil {
			return err
		}
		request := &intent.Request
		if request.ID != row.RequestID || request.UserID != row.UserID || request.Receivable.Currency != SettlementCurrency() || request.Receivable.Nanos < 0 || intent.SaleEvaluationFailed && (!request.SalePending || request.Receivable.Nanos != 0) {
			return ErrLedgerInvalidEntry
		}
		var original GatewayRequest
		if err := common.UnmarshalJsonStr(row.RequestData, &original); err != nil {
			return err
		}
		request.LedgerEntryIDs = original.LedgerEntryIDs
		account := UserWalletAccount(row.UserID)
		accounts := []LedgerPosting{{Account: account}, {Account: PlatformRevenueAccount(SettlementCurrency())}}
		attempts := intent.Attempts
		if intent.Attempt != nil {
			if len(attempts) != 0 {
				return ErrLedgerInvalidEntry
			}
			attempts = []GatewayAttempt{*intent.Attempt}
		}
		seen := map[string]bool{}
		for _, attempt := range attempts {
			if attempt.ID == "" || seen[attempt.ID] || attempt.RequestID != row.RequestID || attempt.ProviderID <= 0 || attempt.Cost.Nanos < 0 {
				return ErrLedgerInvalidEntry
			}
			seen[attempt.ID] = true
			accounts = append(accounts, LedgerPosting{Account: ProviderPayableAccount(attempt.ProviderID, attempt.Cost.Currency)})
		}
		wallets, err := lockLedgerWallets(tx, accounts)
		if err != nil {
			return err
		}
		available := money.Amount{Currency: SettlementCurrency()}
		if !intent.SaleEvaluationFailed {
			available, err = wallets[account].Balance.Add(row.Reservation)
			if err != nil {
				return err
			}
		}
		if intent.ReceivableDue.Currency == "" {
			intent.ReceivableDue = request.Receivable
		}
		// Existing debt has first claim on released reservations as on any
		// other credit. Keep the complete receivable in the immutable intent.
		collectible := max(int64(0), available.Nanos-wallets[UserReceivableAccount(row.UserID)].Balance.Nanos)
		collected := min(request.Receivable.Nanos, collectible)
		shortfall := request.Receivable.Nanos - collected
		if shortfall > 0 {
			request.ErrorCode = "wallet_shortfall"
		}
		saleRate, err := decimal.NewFromString(request.SaleFXRate)
		if err != nil || !saleRate.IsPositive() {
			return money.ErrInvalidRate
		}
		intent.PostedSaleSource = money.Amount{Currency: intent.SaleSource.Currency}
		if collected > 0 {
			intent.PostedSaleSource = intent.SaleSource
			if shortfall > 0 {
				// Reverse the frozen FX exactly; rounding belongs to this posting.
				source, err := money.NewRationalValue(new(big.Rat).Quo(big.NewRat(collected, 1_000_000_000), saleRate.Rat()), intent.SaleSource.Currency)
				if err != nil {
					return err
				}
				intent.PostedSaleSource, err = source.Receivable()
				if err != nil {
					return err
				}
			}
		}
		postings := []LedgerPosting{}
		if row.Reservation.Nanos > 0 && !intent.SaleEvaluationFailed {
			postings = append(postings, LedgerPosting{Account: account, Type: LedgerEntryPreConsumeRelease, Amount: row.Reservation, RequestId: requestID, IdempotencyKey: "gateway:" + requestID + ":release"})
		}
		if collected > 0 {
			posting := LedgerPosting{Account: account, Type: LedgerEntryCharge, Amount: money.Amount{Nanos: -collected, Currency: request.Receivable.Currency}, RequestId: requestID, IdempotencyKey: "gateway:" + requestID + ":charge"}
			debitSource := money.Amount{Nanos: -intent.PostedSaleSource.Nanos, Currency: intent.PostedSaleSource.Currency}
			posting.sourceAmount = &debitSource
			posting.fxRate = decimal.NewNullDecimal(saleRate)
			postings = append(postings, posting)
		}
		if shortfall > 0 {
			postings = append(postings, LedgerPosting{Account: UserReceivableAccount(row.UserID), Type: LedgerEntryCharge, Amount: money.Amount{Nanos: shortfall, Currency: request.Receivable.Currency}, RequestId: requestID, IdempotencyKey: "gateway:" + requestID + ":receivable"})
		}
		if request.Receivable.Nanos > 0 {
			postings = append(postings, LedgerPosting{Account: PlatformRevenueAccount(request.Receivable.Currency), Type: LedgerEntryCharge, Amount: request.Receivable, RequestId: requestID, IdempotencyKey: "gateway:" + requestID + ":revenue", sourceAmount: &intent.SaleSource, fxRate: decimal.NewNullDecimal(saleRate)})
		}
		for _, attempt := range attempts {
			if attempt.Cost.Nanos == 0 {
				continue
			}
			rate, err := decimal.NewFromString(attempt.AccountingFXRate)
			if err != nil {
				return err
			}
			postings = append(postings, LedgerPosting{Account: ProviderPayableAccount(attempt.ProviderID, attempt.Cost.Currency), Type: LedgerEntryCharge, Amount: attempt.Cost, RequestId: requestID, AttemptId: attempt.ID, IdempotencyKey: "gateway:" + attempt.ID + ":payable", fxRate: decimal.NewNullDecimal(rate)})
		}
		for _, posting := range postings {
			if err := validateLedgerPosting(posting); err != nil {
				return err
			}
		}
		entries, err := postLedgerEntries(tx, postings)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			request.LedgerEntryIDs = append(request.LedgerEntryIDs, strconv.FormatInt(entry.Id, 10))
		}
		encoded, err := common.Marshal(request)
		if err != nil {
			return err
		}
		finalized, err := common.Marshal(intent)
		if err != nil {
			return err
		}
		phase := "settled"
		if intent.EvaluationFailed {
			phase = "evaluation_failed"
			pending = true
		}
		if err := tx.Model(&row).Updates(map[string]any{"request_data": string(encoded), "settlement": string(finalized), "phase": phase, "updated_at": clock.Now().UnixMilli()}).Error; err != nil {
			return err
		}
		result = *request
		return nil
	})
	if err == nil && pending {
		return &result, ErrGatewayEvaluationPending
	}
	return &result, err
}

// PublishPendingGatewayRecords exposes accepted task state without finalizing
// funds. The primary lifecycle lock orders pending delivery before final rows.
func PublishPendingGatewayRecords(intent GatewaySettlement) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		var row GatewayBilling
		if err := lockForUpdate(tx).First(&row, "request_id = ?", intent.Request.ID).Error; err != nil {
			return err
		}
		if row.Phase != "reserved" {
			return nil
		}
		for i := range intent.Attempts {
			if err := appendGatewayRecord(&intent.Attempts[i]); err != nil {
				return err
			}
		}
		if intent.Attempt != nil {
			if err := appendGatewayRecord(intent.Attempt); err != nil {
				return err
			}
		}
		return appendGatewayRecord(&intent.Request)
	})
}

// PublishGatewayRecords delivers finalized rows at least once. Stable primary
// keys plus log-database deduplication make an interrupted delivery safe to retry.
func PublishGatewayRecords(requestID string) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		var row GatewayBilling
		if err := lockForUpdate(tx).First(&row, "request_id = ?", requestID).Error; err != nil {
			return err
		}
		if row.Published {
			return nil
		}
		if row.Phase != "settled" {
			return errors.New("gateway billing is not settled")
		}
		var intent GatewaySettlement
		if err := common.UnmarshalJsonStr(row.Settlement, &intent); err != nil {
			return err
		}
		for i := range intent.Attempts {
			if err := appendGatewayRecord(&intent.Attempts[i]); err != nil {
				return err
			}
		}
		if intent.Attempt != nil {
			if err := appendGatewayRecord(intent.Attempt); err != nil {
				return err
			}
		}
		if err := appendGatewayRecord(&intent.Request); err != nil {
			return err
		}
		return tx.Model(&row).Update("published", true).Error
	})
}

// RecoverGatewaySettlements retries durable settlement intents and log delivery.
// In-flight reservations are never released by a timeout: another process may
// still be delivering output. Their IDs remain available for reconciliation.
func RecoverGatewaySettlements(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	if err := recoverGatewaySettlementJournals(ctx, limit); err != nil {
		return 0, err
	}
	var rows []GatewayBilling
	if err := DB.WithContext(ctx).Where("phase IN ? AND published = ?", []string{"settlement_pending", "settled"}, false).Order("created_at, request_id").Limit(limit).Find(&rows).Error; err != nil {
		return 0, err
	}
	completed := 0
	for _, row := range rows {
		if ctx.Err() != nil {
			return completed, ctx.Err()
		}
		if _, err := SettleGateway(row.RequestID); err != nil {
			if errors.Is(err, ErrGatewayEvaluationPending) {
				continue
			}
			return completed, fmt.Errorf("gateway settlement %s: %w", row.RequestID, err)
		}
		if err := PublishGatewayRecords(row.RequestID); err != nil {
			return completed, err
		}
		completed++
	}
	var unresolved []string
	if err := DB.WithContext(ctx).Model(&GatewayBilling{}).Where("phase = ?", "evaluation_failed").Order("created_at, request_id").Limit(limit).Pluck("request_id", &unresolved).Error; err != nil {
		return completed, err
	}
	var pendingErr error
	for _, id := range unresolved {
		pendingErr = errors.Join(pendingErr, fmt.Errorf("gateway settlement %s: %w", id, ErrGatewayEvaluationPending))
	}
	return completed, pendingErr
}
