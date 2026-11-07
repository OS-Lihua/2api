package model

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
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

type GatewaySettlement struct {
	Request          GatewayRequest  `json:"request"`
	Attempt          *GatewayAttempt `json:"attempt,omitempty"`
	SaleSource       money.Amount    `json:"sale_source"`
	ReceivableDue    money.Amount    `json:"receivable_due"`
	PostedSaleSource money.Amount    `json:"posted_sale_source"`
}

func MigrateGatewayBilling(db *gorm.DB) error {
	if err := migrateWalletLedger(db); err != nil {
		return err
	}
	return db.AutoMigrate(&GatewayBilling{})
}

// ReserveGateway writes the request lifecycle and reservation in one primary
// transaction. Reusing a request ID cannot dispatch another Provider attempt.
func ReserveGateway(request *GatewayRequest, reservation, source money.Amount) error {
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
		if wallet.RequestsPaused || wallets[UserReceivableAccount(request.UserID)].Balance.Nanos > 0 || wallet.Balance.Nanos < reservation.Nanos {
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
		return tx.Model(&row).Update("request_data", string(encoded)).Error
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
	err := DB.Transaction(func(tx *gorm.DB) error {
		var row GatewayBilling
		if err := lockForUpdate(tx).First(&row, "request_id = ?", requestID).Error; err != nil {
			return err
		}
		if row.Phase == "settled" {
			return common.UnmarshalJsonStr(row.RequestData, &result)
		}
		if row.Phase != "settlement_pending" {
			return errors.New("gateway settlement is not prepared")
		}
		var intent GatewaySettlement
		if err := common.UnmarshalJsonStr(row.Settlement, &intent); err != nil {
			return err
		}
		request := &intent.Request
		if request.ID != row.RequestID || request.UserID != row.UserID || request.Receivable.Currency != SettlementCurrency() || request.Receivable.Nanos < 0 {
			return ErrLedgerInvalidEntry
		}
		var original GatewayRequest
		if err := common.UnmarshalJsonStr(row.RequestData, &original); err != nil {
			return err
		}
		request.LedgerEntryIDs = original.LedgerEntryIDs
		account := UserWalletAccount(row.UserID)
		accounts := []LedgerPosting{{Account: account}, {Account: PlatformRevenueAccount(SettlementCurrency())}}
		if intent.Attempt != nil {
			if intent.Attempt.RequestID != row.RequestID || intent.Attempt.Cost.Nanos < 0 {
				return ErrLedgerInvalidEntry
			}
			accounts = append(accounts, LedgerPosting{Account: ProviderPayableAccount(intent.Attempt.ProviderID, intent.Attempt.Cost.Currency)})
		}
		wallets, err := lockLedgerWallets(tx, accounts)
		if err != nil {
			return err
		}
		available, err := wallets[account].Balance.Add(row.Reservation)
		if err != nil {
			return err
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
		if row.Reservation.Nanos > 0 {
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
		if attempt := intent.Attempt; attempt != nil && attempt.Cost.Nanos > 0 {
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
		if err := tx.Model(&row).Updates(map[string]any{"request_data": string(encoded), "settlement": string(finalized), "phase": "settled", "updated_at": clock.Now().UnixMilli()}).Error; err != nil {
			return err
		}
		result = *request
		return nil
	})
	return &result, err
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
			return completed, fmt.Errorf("gateway settlement %s: %w", row.RequestID, err)
		}
		if err := PublishGatewayRecords(row.RequestID); err != nil {
			return completed, err
		}
		completed++
	}
	return completed, nil
}
