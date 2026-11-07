package controller

import (
	"errors"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/money"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// walletView is a wallet in the admin API. Amounts are decimal strings in
// major currency units, for example "12.5".
type walletView struct {
	UserId         int            `json:"user_id"`
	Currency       money.Currency `json:"currency"`
	Balance        string         `json:"balance"`
	UpdatedAt      int64          `json:"updated_at"`
	Outstanding    string         `json:"outstanding,omitempty"`
	RequestsPaused bool           `json:"requests_paused,omitempty"`
}

type ledgerEntryView struct {
	Id              int64                   `json:"id"`
	Type            model.LedgerEntryType   `json:"type"`
	AccountType     model.LedgerAccountType `json:"account_type"`
	Currency        money.Currency          `json:"currency"`
	Amount          string                  `json:"amount"`
	BalanceAfter    string                  `json:"balance_after"`
	SourceCurrency  money.Currency          `json:"source_currency"`
	SourceAmount    string                  `json:"source_amount"`
	FxRate          *string                 `json:"fx_rate"`
	ReversesEntryId *int64                  `json:"reverses_entry_id"`
	RequestId       string                  `json:"request_id"`
	AttemptId       string                  `json:"attempt_id"`
	OperatorId      int                     `json:"operator_id"`
	Remark          string                  `json:"remark"`
	CreatedAt       int64                   `json:"created_at"`
}

func amountString(amount money.Amount) string {
	return decimal.New(amount.Nanos, -money.Scale).String()
}

func newLedgerEntryView(entry *model.LedgerEntry) ledgerEntryView {
	view := ledgerEntryView{
		Id:              entry.Id,
		Type:            entry.Type,
		AccountType:     entry.AccountType,
		Currency:        entry.Amount.Currency,
		Amount:          amountString(entry.Amount),
		BalanceAfter:    amountString(entry.BalanceAfter),
		SourceCurrency:  entry.SourceAmount.Currency,
		SourceAmount:    amountString(entry.SourceAmount),
		ReversesEntryId: entry.ReversesEntryId,
		RequestId:       entry.RequestId,
		AttemptId:       entry.AttemptId,
		OperatorId:      entry.OperatorId,
		Remark:          entry.Remark,
		CreatedAt:       entry.CreatedAt,
	}
	if entry.FxRate.Valid {
		rate := entry.FxRate.Decimal.String()
		view.FxRate = &rate
	}
	return view
}

// parseExactAmount parses a decimal string in major units that must be
// exact at 10^-9; the API rejects an amount it would have to round.
func parseExactAmount(text string, currency money.Currency) (money.Amount, bool) {
	parsed, err := decimal.NewFromString(strings.TrimSpace(text))
	if err != nil || !parsed.Shift(money.Scale).IsInteger() {
		return money.Amount{}, false
	}
	value, err := money.NewValue(parsed, currency)
	if err != nil {
		return money.Amount{}, false
	}
	amount, err := value.Cost()
	if err != nil {
		return money.Amount{}, false
	}
	return amount, true
}

// walletTargetUser loads the user in the :user_id path parameter. Writes
// also require the caller to outrank the user, as for other user changes.
func walletTargetUser(c *gin.Context, write bool) (*model.User, bool) {
	userId, err := strconv.Atoi(c.Param("user_id"))
	if err != nil || userId <= 0 {
		common.ApiErrorT(c, "Invalid user ID")
		return nil, false
	}
	user, err := model.GetUserById(userId, false)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		common.ApiErrorT(c, "User does not exist")
		return nil, false
	}
	if err != nil {
		common.ApiError(c, err)
		return nil, false
	}
	if write && !canManageTargetRole(c.GetInt("role"), user.Role) {
		common.ApiErrorT(c, "No permission to update users of same or higher permission level")
		return nil, false
	}
	return user, true
}

func respondLedgerError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, model.ErrInsufficientWalletBalance):
		common.ApiErrorT(c, "Insufficient balance")
	case errors.Is(err, model.ErrDuplicateLedgerEntry):
		common.ApiErrorT(c, "This reference has already been credited")
	case errors.Is(err, model.ErrLedgerEntryNotFound):
		common.ApiErrorT(c, "Ledger entry not found")
	case errors.Is(err, model.ErrLedgerEntryAlreadyReversed):
		common.ApiErrorT(c, "This ledger entry has already been reversed")
	case errors.Is(err, model.ErrLedgerReversalNotReversible):
		common.ApiErrorT(c, "A reversal cannot be reversed")
	case errors.Is(err, model.ErrLedgerRateRequired):
		common.ApiErrorT(c, "An exchange rate is required for a top-up in another currency")
	case errors.Is(err, money.ErrCurrencyMismatch):
		common.ApiErrorT(c, "The currency does not match the wallet")
	case errors.Is(err, money.ErrOverflow):
		common.ApiErrorT(c, "The amount is outside the supported range")
	case errors.Is(err, model.ErrLedgerInvalidEntry):
		common.ApiErrorT(c, "Invalid ledger entry")
	default:
		common.SysError(common.LogText("ledger write failed: %s", err.Error()))
		common.ApiErrorT(c, "Database error, please contact the administrator")
	}
}

// GetUserWallet returns a user's wallet. A user without ledger entries has a
// zero balance in the settlement currency.
func GetUserWallet(c *gin.Context) {
	user, ok := walletTargetUser(c, false)
	if !ok {
		return
	}
	wallet, err := model.GetUserWallet(user.Id)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	view := walletView{UserId: user.Id, Currency: model.SettlementCurrency(), Balance: "0"}
	if wallet != nil {
		view.Currency = wallet.Balance.Currency
		view.Balance = amountString(wallet.Balance)
		view.UpdatedAt = wallet.UpdatedAt
	}
	outstanding, err := model.GetUserOutstanding(user.Id)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if outstanding.Nanos > 0 {
		view.Outstanding = amountString(outstanding)
		view.RequestsPaused = true
	}
	common.ApiSuccess(c, view)
}

// GetUserWalletEntries lists a user's ledger entries, newest first.
func GetUserWalletEntries(c *gin.Context) {
	user, ok := walletTargetUser(c, false)
	if !ok {
		return
	}
	pageInfo := common.GetPageQuery(c)
	views := []ledgerEntryView{}
	wallet, err := model.GetUserWallet(user.Id)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if wallet != nil {
		entries, total, err := model.ListLedgerEntries(wallet.Id, pageInfo.GetStartIdx(), pageInfo.GetPageSize())
		if err != nil {
			common.ApiError(c, err)
			return
		}
		for _, entry := range entries {
			views = append(views, newLedgerEntryView(entry))
		}
		pageInfo.SetTotal(int(total))
	}
	pageInfo.SetItems(views)
	common.ApiSuccess(c, pageInfo)
}

type walletTopUpRequest struct {
	// Amount is the amount received, in Currency.
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
	// FxRate converts one unit of Currency into the settlement currency; it
	// is required when Currency is not the settlement currency.
	FxRate    string `json:"fx_rate"`
	Reference string `json:"reference"`
	Remark    string `json:"remark"`
}

// CreateUserWalletTopUp credits a payment received outside the gateway, such
// as a bank transfer, to a user wallet.
func CreateUserWalletTopUp(c *gin.Context) {
	user, ok := walletTargetUser(c, true)
	if !ok {
		return
	}
	var req walletTopUpRequest
	if err := common.DecodeJson(c.Request.Body, &req); err != nil {
		common.ApiErrorT(c, "Invalid parameters")
		return
	}
	currency := model.SettlementCurrency()
	if req.Currency != "" {
		parsed, err := money.ParseCurrency(req.Currency)
		if err != nil {
			common.ApiErrorT(c, "Invalid currency")
			return
		}
		currency = parsed
	}
	source, ok := parseExactAmount(req.Amount, currency)
	if !ok || source.Nanos <= 0 {
		common.ApiErrorT(c, "Invalid amount")
		return
	}
	topUp := model.WalletTopUp{
		UserId:     user.Id,
		Source:     source,
		Reference:  strings.TrimSpace(req.Reference),
		OperatorId: c.GetInt("id"),
		Remark:     strings.TrimSpace(req.Remark),
	}
	if req.FxRate != "" {
		parsed, err := decimal.NewFromString(strings.TrimSpace(req.FxRate))
		if err != nil {
			common.ApiErrorT(c, "Invalid exchange rate")
			return
		}
		rate, err := money.NewRate(currency, model.SettlementCurrency(), parsed)
		if err != nil {
			common.ApiErrorT(c, "Invalid exchange rate")
			return
		}
		topUp.Rate = &rate
	}
	entry, err := model.TopUpWallet(topUp)
	if err != nil {
		respondLedgerError(c, err)
		return
	}
	common.ApiSuccess(c, newLedgerEntryView(entry))
}

type walletAdjustmentRequest struct {
	// Amount is signed and in the settlement currency.
	Amount string `json:"amount"`
	Remark string `json:"remark"`
}

// CreateUserWalletAdjustment raises or lowers a user wallet balance. A
// remark is required so that the ledger explains the change.
func CreateUserWalletAdjustment(c *gin.Context) {
	user, ok := walletTargetUser(c, true)
	if !ok {
		return
	}
	var req walletAdjustmentRequest
	if err := common.DecodeJson(c.Request.Body, &req); err != nil {
		common.ApiErrorT(c, "Invalid parameters")
		return
	}
	amount, ok := parseExactAmount(req.Amount, model.SettlementCurrency())
	if !ok || amount.Nanos == 0 {
		common.ApiErrorT(c, "Invalid amount")
		return
	}
	remark := strings.TrimSpace(req.Remark)
	if remark == "" {
		common.ApiErrorT(c, "Remark is required")
		return
	}
	entries, err := model.PostLedgerEntries(model.LedgerPosting{
		Account:    model.UserWalletAccount(user.Id),
		Type:       model.LedgerEntryAdjustment,
		Amount:     amount,
		OperatorId: c.GetInt("id"),
		Remark:     remark,
	})
	if err != nil {
		respondLedgerError(c, err)
		return
	}
	common.ApiSuccess(c, newLedgerEntryView(entries[0]))
}

type ledgerReversalRequest struct {
	Remark string `json:"remark"`
}

// ReverseLedgerEntry writes the entry that cancels a ledger entry. A remark
// is required.
func ReverseLedgerEntry(c *gin.Context) {
	entryId, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || entryId <= 0 {
		common.ApiErrorT(c, "Invalid ID")
		return
	}
	wallet, err := model.GetLedgerEntryWallet(entryId)
	if err != nil {
		respondLedgerError(c, err)
		return
	}
	if wallet.AccountType == model.LedgerAccountUserWallet {
		owner, err := model.GetUserById(wallet.OwnerId, false)
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			common.ApiError(c, err)
			return
		}
		if err == nil && !canManageTargetRole(c.GetInt("role"), owner.Role) {
			common.ApiErrorT(c, "No permission to update users of same or higher permission level")
			return
		}
	}
	var req ledgerReversalRequest
	if err := common.DecodeJson(c.Request.Body, &req); err != nil {
		common.ApiErrorT(c, "Invalid parameters")
		return
	}
	remark := strings.TrimSpace(req.Remark)
	if remark == "" {
		common.ApiErrorT(c, "Remark is required")
		return
	}
	entry, err := model.ReverseLedgerEntry(entryId, c.GetInt("id"), remark)
	if err != nil {
		respondLedgerError(c, err)
		return
	}
	common.ApiSuccess(c, newLedgerEntryView(entry))
}
