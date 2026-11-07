package model

import (
	"crypto/sha256"
	"fmt"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// IncreaseGatewayReservation funds an increase in the same request reservation
// before another effective media quantity is sent. It preserves the frozen
// price/FX snapshot and posts through the existing wallet/debt primitive.
// An operation is immutable and idempotent, including after final settlement.
func IncreaseGatewayReservation(requestID, operation string, additional, source money.Amount) ([]string, error) {
	if requestID == "" || operation == "" || additional.Currency != SettlementCurrency() || additional.Nanos <= 0 || source.Nanos < 0 {
		return nil, ErrLedgerInvalidEntry
	}
	if err := PreflightGatewaySettlementStorage(requestID); err != nil {
		return nil, err
	}
	operationHash := sha256.Sum256([]byte(operation))
	key := fmt.Sprintf("gateway:%s:reserve:%x", requestID, operationHash[:16])
	var ids []string
	err := DB.Transaction(func(tx *gorm.DB) error {
		var row GatewayBilling
		if err := lockForUpdate(tx).First(&row, "request_id = ?", requestID).Error; err != nil {
			return err
		}
		var request GatewayRequest
		if err := common.UnmarshalJsonStr(row.RequestData, &request); err != nil {
			return err
		}
		if source.Currency != request.SaleCurrency {
			return money.ErrCurrencyMismatch
		}
		ids = append([]string(nil), request.LedgerEntryIDs...)
		var existing []LedgerEntry
		if err := tx.Where("idempotency_key = ?", key).Limit(1).Find(&existing).Error; err != nil {
			return err
		}
		if len(existing) != 0 {
			entry := existing[0]
			if entry.RequestId != requestID || entry.Amount.Currency != additional.Currency || entry.Amount.Nanos != -additional.Nanos || entry.SourceAmount.Currency != source.Currency || entry.SourceAmount.Nanos != -source.Nanos {
				return ErrLedgerInvalidEntry
			}
			return nil
		}
		if row.Phase != "reserved" {
			return ErrLedgerInvalidEntry
		}
		total, err := row.Reservation.Add(additional)
		if err != nil {
			return err
		}
		account := UserWalletAccount(row.UserID)
		wallets, err := lockLedgerWallets(tx, []LedgerPosting{{Account: account}})
		if err != nil {
			return err
		}
		if wallets[account].RequestsPaused || wallets[UserReceivableAccount(row.UserID)].Balance.Nanos > 0 || wallets[account].Balance.Nanos < additional.Nanos {
			return ErrInsufficientWalletBalance
		}
		rate, err := decimal.NewFromString(request.SaleFXRate)
		if err != nil {
			return err
		}
		debitSource := money.Amount{Nanos: -source.Nanos, Currency: source.Currency}
		posting := LedgerPosting{Account: account, Type: LedgerEntryPreConsume, Amount: money.Amount{Nanos: -additional.Nanos, Currency: additional.Currency}, RequestId: requestID, IdempotencyKey: key, sourceAmount: &debitSource, fxRate: decimal.NewNullDecimal(rate)}
		if err := validateLedgerPosting(posting); err != nil {
			return err
		}
		entries, err := postLedgerEntries(tx, []LedgerPosting{posting})
		if err != nil {
			return err
		}
		request.LedgerEntryIDs = append(request.LedgerEntryIDs, strconv.FormatInt(entries[0].Id, 10))
		encoded, err := common.Marshal(request)
		if err != nil {
			return err
		}
		if err := tx.Model(&row).Updates(map[string]any{"reservation_nanos": total.Nanos, "request_data": string(encoded), "updated_at": clock.Now().UnixMilli()}).Error; err != nil {
			return err
		}
		ids = request.LedgerEntryIDs
		return nil
	})
	return ids, err
}
