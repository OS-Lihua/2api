package model

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// LedgerAccountType is the kind of account a wallet holds.
type LedgerAccountType string

const (
	// LedgerAccountUserWallet is a user's balance in the settlement currency;
	// its owner is the user ID.
	LedgerAccountUserWallet LedgerAccountType = "user_wallet"
	// LedgerAccountUserReceivable holds the user's unpaid charges. Repayments
	// reduce this asset without recognizing revenue a second time.
	LedgerAccountUserReceivable LedgerAccountType = "user_receivable"
	// LedgerAccountPlatformRevenue is the platform's revenue in one currency;
	// its owner is 0.
	LedgerAccountPlatformRevenue LedgerAccountType = "platform_revenue"
	// LedgerAccountProviderPayable is what the platform owes a Provider in the
	// Provider's currency; its owner is the Provider ID.
	LedgerAccountProviderPayable LedgerAccountType = "provider_payable"
)

// LedgerEntryType is the reason for a ledger entry.
type LedgerEntryType string

const (
	LedgerEntryRepayment         LedgerEntryType = "repayment"
	LedgerEntryTopUp             LedgerEntryType = "top_up"
	LedgerEntryPreConsume        LedgerEntryType = "pre_consume"
	LedgerEntryPreConsumeRelease LedgerEntryType = "pre_consume_release"
	LedgerEntryCharge            LedgerEntryType = "charge"
	LedgerEntryRefund            LedgerEntryType = "refund"
	LedgerEntryAdjustment        LedgerEntryType = "adjustment"
	LedgerEntryOpeningBalance    LedgerEntryType = "opening_balance"
)

type ledgerSign int

const (
	ledgerSignPositive ledgerSign = iota + 1
	ledgerSignNegative
	ledgerSignEither
)

// ledgerEntrySigns lists the entry types each account accepts and the sign
// of their amounts. A positive amount increases the balance. On a Provider
// payable account a charge is a purchase cost the platform owes, and a
// refund is a credit from the Provider. A reversal has the opposite sign.
var ledgerEntrySigns = map[LedgerAccountType]map[LedgerEntryType]ledgerSign{
	LedgerAccountUserReceivable: {
		LedgerEntryCharge:    ledgerSignPositive,
		LedgerEntryRepayment: ledgerSignNegative,
	},
	LedgerAccountUserWallet: {
		LedgerEntryRepayment:         ledgerSignNegative,
		LedgerEntryTopUp:             ledgerSignPositive,
		LedgerEntryPreConsume:        ledgerSignNegative,
		LedgerEntryPreConsumeRelease: ledgerSignPositive,
		LedgerEntryCharge:            ledgerSignNegative,
		LedgerEntryRefund:            ledgerSignPositive,
		LedgerEntryAdjustment:        ledgerSignEither,
		LedgerEntryOpeningBalance:    ledgerSignPositive,
	},
	LedgerAccountPlatformRevenue: {
		LedgerEntryCharge:     ledgerSignPositive,
		LedgerEntryRefund:     ledgerSignNegative,
		LedgerEntryAdjustment: ledgerSignEither,
	},
	LedgerAccountProviderPayable: {
		LedgerEntryCharge:     ledgerSignPositive,
		LedgerEntryRefund:     ledgerSignNegative,
		LedgerEntryAdjustment: ledgerSignEither,
	},
}

const (
	ledgerRemarkMaxLength         = 255
	ledgerReferenceMaxLength      = 64
	ledgerIdempotencyKeyMaxLength = 128

	ledgerIdempotencyKeyIndex = "idx_ledger_entries_idempotency_key"
	ledgerReversalIndex       = "idx_ledger_entries_reverses_entry_id"
)

var (
	ErrLedgerInvalidEntry          = errors.New("invalid ledger entry")
	ErrLedgerRateRequired          = errors.New("an exchange rate is required for a top-up in another currency")
	ErrInsufficientWalletBalance   = errors.New("insufficient wallet balance")
	ErrDuplicateLedgerEntry        = errors.New("a ledger entry with this idempotency key already exists")
	ErrLedgerEntryNotFound         = errors.New("ledger entry not found")
	ErrLedgerEntryAlreadyReversed  = errors.New("ledger entry is already reversed")
	ErrLedgerReversalNotReversible = errors.New("a reversal cannot be reversed")
)

var settlementCurrency money.Currency = "USD"

// InitSettlementCurrency reads SETTLEMENT_CURRENCY (default USD), the ISO
// 4217 currency of every user wallet. Changing it after wallets exist makes
// postings to those wallets fail with a currency mismatch.
func InitSettlementCurrency() error {
	currency, err := money.ParseCurrency(common.GetEnvOrDefaultString("SETTLEMENT_CURRENCY", "USD"))
	if err != nil {
		return fmt.Errorf("SETTLEMENT_CURRENCY: %w", err)
	}
	settlementCurrency = currency
	return nil
}

// SettlementCurrency returns the currency of user wallets.
func SettlementCurrency() money.Currency {
	return settlementCurrency
}

// Wallet holds the balance of one account. The balance always equals the
// sum of the wallet's ledger entries; only ledger postings change it.
type Wallet struct {
	Id             int64             `json:"id" gorm:"primaryKey"`
	AccountType    LedgerAccountType `json:"account_type" gorm:"type:varchar(32);not null"`
	OwnerId        int               `json:"owner_id" gorm:"not null"`
	Balance        money.Amount      `json:"balance" gorm:"embedded;embeddedPrefix:balance_"`
	CreatedAt      int64             `json:"created_at" gorm:"bigint;not null"`
	UpdatedAt      int64             `json:"updated_at" gorm:"bigint;not null"`
	RequestsPaused bool              `json:"requests_paused" gorm:"not null;default:false"`
}

// LedgerEntry is one append-only change to a wallet. The database rejects
// updates and deletes; a mistake is corrected by a reversal entry.
type LedgerEntry struct {
	Id          int64             `json:"id" gorm:"primaryKey"`
	WalletId    int64             `json:"wallet_id" gorm:"not null"`
	AccountType LedgerAccountType `json:"account_type" gorm:"type:varchar(32);not null"`
	Type        LedgerEntryType   `json:"type" gorm:"type:varchar(32);not null;index"`
	// Amount is the signed change in the wallet currency.
	Amount money.Amount `json:"amount" gorm:"embedded;embeddedPrefix:amount_"`
	// BalanceAfter is the wallet balance right after this entry.
	BalanceAfter money.Amount `json:"balance_after" gorm:"embedded;embeddedPrefix:balance_after_"`
	// SourceAmount is the amount in the currency it was received in. It equals
	// Amount unless a top-up was converted at FxRate.
	SourceAmount money.Amount `json:"source_amount" gorm:"embedded;embeddedPrefix:source_"`
	// FxRate converts one unit of the source currency into the wallet
	// currency; it is null when no conversion took place.
	FxRate          decimal.NullDecimal `json:"fx_rate" gorm:"type:numeric"`
	ReversesEntryId *int64              `json:"reverses_entry_id,omitempty" gorm:"uniqueIndex:idx_ledger_entries_reverses_entry_id"`
	RequestId       string              `json:"request_id" gorm:"type:varchar(64);not null;default:'';index"`
	AttemptId       string              `json:"attempt_id" gorm:"type:varchar(64);not null;default:'';index"`
	IdempotencyKey  *string             `json:"-" gorm:"type:varchar(128);uniqueIndex:idx_ledger_entries_idempotency_key"`
	OperatorId      int                 `json:"operator_id" gorm:"not null;default:0"`
	Remark          string              `json:"remark" gorm:"type:varchar(255);not null;default:''"`
	CreatedAt       int64               `json:"created_at" gorm:"bigint;not null;index"`
}

// LedgerAccount identifies a wallet.
type LedgerAccount struct {
	Type     LedgerAccountType
	OwnerId  int
	Currency money.Currency
}

// UserWalletAccount is the wallet of a user in the settlement currency.
func UserWalletAccount(userId int) LedgerAccount {
	return LedgerAccount{Type: LedgerAccountUserWallet, OwnerId: userId, Currency: settlementCurrency}
}

// UserReceivableAccount holds unpaid charges in the settlement currency.
func UserReceivableAccount(userId int) LedgerAccount {
	return LedgerAccount{Type: LedgerAccountUserReceivable, OwnerId: userId, Currency: settlementCurrency}
}

// PlatformRevenueAccount is the platform revenue account in currency.
func PlatformRevenueAccount(currency money.Currency) LedgerAccount {
	return LedgerAccount{Type: LedgerAccountPlatformRevenue, Currency: currency}
}

// ProviderPayableAccount is the amount owed to a Provider in its currency.
func ProviderPayableAccount(providerId int, currency money.Currency) LedgerAccount {
	return LedgerAccount{Type: LedgerAccountProviderPayable, OwnerId: providerId, Currency: currency}
}

// LedgerPosting is one ledger entry to write. Amount is signed and must be in
// the account currency.
type LedgerPosting struct {
	Account   LedgerAccount
	Type      LedgerEntryType
	Amount    money.Amount
	RequestId string
	AttemptId string
	// IdempotencyKey, when set, is unique across the ledger; a second posting
	// with the same key fails with ErrDuplicateLedgerEntry.
	IdempotencyKey string
	OperatorId     int
	Remark         string

	sourceAmount    *money.Amount
	fxRate          decimal.NullDecimal
	reversesEntryId *int64
}

// PostLedgerEntries writes all postings in one transaction: either every
// entry is written and every balance updated, or nothing changes. Each wallet
// is created on first use and locked with SELECT ... FOR UPDATE in ID order,
// so concurrent postings serialize per wallet without deadlocks. A posting
// that would make a user wallet negative fails with
// ErrInsufficientWalletBalance.
func PostLedgerEntries(postings ...LedgerPosting) ([]*LedgerEntry, error) {
	for _, posting := range postings {
		if err := validateLedgerPosting(posting); err != nil {
			return nil, err
		}
	}
	var entries []*LedgerEntry
	err := DB.Transaction(func(tx *gorm.DB) error {
		var err error
		entries, err = postLedgerEntries(tx, postings)
		return err
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func validateLedgerPosting(posting LedgerPosting) error {
	signs, ok := ledgerEntrySigns[posting.Account.Type]
	if !ok {
		return fmt.Errorf("%w: unknown account type %q", ErrLedgerInvalidEntry, posting.Account.Type)
	}
	if posting.Account.Type == LedgerAccountPlatformRevenue {
		if posting.Account.OwnerId != 0 {
			return fmt.Errorf("%w: platform revenue owner must be zero", ErrLedgerInvalidEntry)
		}
	} else if posting.Account.OwnerId <= 0 {
		return fmt.Errorf("%w: account owner must be positive", ErrLedgerInvalidEntry)
	}
	sign, ok := signs[posting.Type]
	if !ok {
		return fmt.Errorf("%w: %s entries are not allowed on %s accounts", ErrLedgerInvalidEntry, posting.Type, posting.Account.Type)
	}
	if _, err := money.ParseCurrency(string(posting.Account.Currency)); err != nil {
		return err
	}
	if (posting.Account.Type == LedgerAccountUserWallet || posting.Account.Type == LedgerAccountUserReceivable) && posting.Account.Currency != SettlementCurrency() {
		return fmt.Errorf("%w: user wallets must use %s", money.ErrCurrencyMismatch, SettlementCurrency())
	}
	if posting.Amount.Currency != posting.Account.Currency {
		return fmt.Errorf("%w: %s amount posted to a %s account", money.ErrCurrencyMismatch, posting.Amount.Currency, posting.Account.Currency)
	}
	if posting.Amount.Nanos == 0 {
		return fmt.Errorf("%w: amount must not be zero", ErrLedgerInvalidEntry)
	}
	positive := posting.Amount.Nanos > 0
	if posting.reversesEntryId != nil {
		// A reversal cancels its entry, so its sign is the opposite one.
		positive = !positive
	}
	if sign == ledgerSignPositive && !positive || sign == ledgerSignNegative && positive {
		return fmt.Errorf("%w: %s amount %d has the wrong sign", ErrLedgerInvalidEntry, posting.Type, posting.Amount.Nanos)
	}
	if utf8.RuneCountInString(posting.Remark) > ledgerRemarkMaxLength {
		return fmt.Errorf("%w: remark is longer than %d characters", ErrLedgerInvalidEntry, ledgerRemarkMaxLength)
	}
	if len(posting.IdempotencyKey) > ledgerIdempotencyKeyMaxLength || len(posting.RequestId) > 64 || len(posting.AttemptId) > 64 {
		return fmt.Errorf("%w: reference is too long", ErrLedgerInvalidEntry)
	}
	return nil
}

func postLedgerEntries(tx *gorm.DB, postings []LedgerPosting) ([]*LedgerEntry, error) {
	wallets, err := lockLedgerWallets(tx, postings)
	if err != nil {
		return nil, err
	}
	now := clock.Now().Unix()
	entries := make([]*LedgerEntry, 0, len(postings))
	for _, posting := range postings {
		wallet := wallets[posting.Account]
		balance, err := wallet.Balance.Add(posting.Amount)
		if err != nil {
			return nil, err
		}
		if (wallet.AccountType == LedgerAccountUserWallet || wallet.AccountType == LedgerAccountUserReceivable) && balance.Nanos < 0 {
			return nil, ErrInsufficientWalletBalance
		}
		entry := &LedgerEntry{
			WalletId:        wallet.Id,
			AccountType:     wallet.AccountType,
			Type:            posting.Type,
			Amount:          posting.Amount,
			BalanceAfter:    balance,
			SourceAmount:    posting.Amount,
			ReversesEntryId: posting.reversesEntryId,
			RequestId:       posting.RequestId,
			AttemptId:       posting.AttemptId,
			OperatorId:      posting.OperatorId,
			Remark:          posting.Remark,
			CreatedAt:       now,
		}
		if posting.sourceAmount != nil {
			entry.SourceAmount = *posting.sourceAmount
		}
		entry.FxRate = posting.fxRate
		if posting.IdempotencyKey != "" {
			key := posting.IdempotencyKey
			entry.IdempotencyKey = &key
		}
		if err := tx.Create(entry).Error; err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == ledgerIdempotencyKeyIndex {
				return nil, ErrDuplicateLedgerEntry
			}
			if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == ledgerReversalIndex {
				return nil, ErrLedgerEntryAlreadyReversed
			}
			return nil, err
		}
		updates := map[string]any{"balance_nanos": balance.Nanos, "updated_at": now}
		if err := tx.Model(&Wallet{}).Where("id = ?", wallet.Id).Updates(updates).Error; err != nil {
			return nil, err
		}
		wallet.Balance = balance
		wallet.UpdatedAt = now
		entries = append(entries, entry)
	}
	// Sweep after the whole batch: a settlement can release its reservation
	// and charge in one posting. All account locks were acquired up front.
	swept := make(map[int]bool)
	for _, posting := range postings {
		if (posting.Account.Type != LedgerAccountUserWallet && posting.Account.Type != LedgerAccountUserReceivable) || posting.Type == LedgerEntryRepayment || swept[posting.Account.OwnerId] {
			continue
		}
		swept[posting.Account.OwnerId] = true
		account := UserWalletAccount(posting.Account.OwnerId)
		wallet := wallets[account]
		debt := wallets[UserReceivableAccount(posting.Account.OwnerId)]
		payment := min(wallet.Balance.Nanos, debt.Balance.Nanos)
		if payment > 0 {
			paid, err := repayWalletReceivables(tx, wallet, debt, payment)
			if err != nil {
				return nil, err
			}
			entries = append(entries, paid...)
		}
		if err := tx.Model(&Wallet{}).Where("id = ?", wallet.Id).Update("requests_paused", debt.Balance.Nanos > 0).Error; err != nil {
			return nil, err
		}
	}
	return entries, nil
}

// repayWalletReceivables allocates funds to the oldest unpaid request first.
// Both wallets are already locked. The paired entries retain the debt's request
// and identify the funding wallet entry without changing the original credit.
func repayWalletReceivables(tx *gorm.DB, wallet, debt *Wallet, available int64) ([]*LedgerEntry, error) {
	var funding LedgerEntry
	if err := tx.Where("wallet_id = ? AND amount_nanos > 0", wallet.Id).Order("id DESC").First(&funding).Error; err != nil {
		return nil, err
	}
	var unpaid []struct {
		RequestId string
		FirstId   int64
		Nanos     decimal.Decimal
	}
	if err := tx.Model(&LedgerEntry{}).Select(`"request_id", MIN("id") AS "first_id", SUM("amount_nanos") AS "nanos"`).
		Where("wallet_id = ?", debt.Id).Group(`"request_id"`).Having(`SUM("amount_nanos") > 0`).Order(`MIN("id")`).Scan(&unpaid).Error; err != nil {
		return nil, err
	}
	var entries []*LedgerEntry
	for _, item := range unpaid {
		value, err := money.NewValue(item.Nanos.Shift(-money.Scale), debt.Balance.Currency)
		if err != nil {
			return nil, err
		}
		amount, err := value.Credit()
		if err != nil {
			return nil, err
		}
		payment := min(available, amount.Nanos)
		key := fmt.Sprintf("repayment:%d:%d", funding.Id, item.FirstId)
		remark := fmt.Sprintf("Receivable repayment funded by ledger entry %d", funding.Id)
		paid, err := postLedgerEntries(tx, []LedgerPosting{
			{Account: UserWalletAccount(wallet.OwnerId), Type: LedgerEntryRepayment, Amount: money.Amount{Nanos: -payment, Currency: wallet.Balance.Currency}, RequestId: item.RequestId, IdempotencyKey: key + ":wallet", Remark: remark},
			{Account: UserReceivableAccount(wallet.OwnerId), Type: LedgerEntryRepayment, Amount: money.Amount{Nanos: -payment, Currency: wallet.Balance.Currency}, RequestId: item.RequestId, IdempotencyKey: key + ":receivable", Remark: remark},
		})
		if err != nil {
			return nil, err
		}
		entries = append(entries, paid...)
		wallet.Balance = paid[0].BalanceAfter
		debt.Balance = paid[1].BalanceAfter
		available -= payment
		if available == 0 {
			return entries, nil
		}
	}
	return nil, fmt.Errorf("%w: receivable balance differs from unpaid requests", ErrLedgerInvalidEntry)
}

// lockLedgerWallets creates any missing wallet and locks every wallet the
// postings touch, in ID order. Missing wallets are created in a fixed account
// order, so two transactions that create the same new wallets cannot wait on
// each other.
func lockLedgerWallets(tx *gorm.DB, postings []LedgerPosting) (map[LedgerAccount]*Wallet, error) {
	accountList := make([]LedgerAccount, 0, len(postings))
	for _, posting := range postings {
		if posting.Account.Type == LedgerAccountUserWallet || posting.Account.Type == LedgerAccountUserReceivable {
			debt := UserReceivableAccount(posting.Account.OwnerId)
			user := UserWalletAccount(posting.Account.OwnerId)
			if !slices.Contains(accountList, user) {
				accountList = append(accountList, user)
			}
			if !slices.Contains(accountList, debt) {
				accountList = append(accountList, debt)
			}
		}
		if !slices.Contains(accountList, posting.Account) {
			accountList = append(accountList, posting.Account)
		}
	}
	slices.SortFunc(accountList, func(a, b LedgerAccount) int {
		return cmp.Or(cmp.Compare(a.Type, b.Type), cmp.Compare(a.OwnerId, b.OwnerId), cmp.Compare(a.Currency, b.Currency))
	})
	ids := make([]int64, 0, len(accountList))
	accounts := make(map[int64]LedgerAccount, len(accountList))
	for _, account := range accountList {
		id, err := ensureWallet(tx, account)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
		accounts[id] = account
	}
	slices.Sort(ids)
	var locked []*Wallet
	if err := lockForUpdate(tx).Where("id IN ?", ids).Order("id").Find(&locked).Error; err != nil {
		return nil, err
	}
	if len(locked) != len(ids) {
		return nil, fmt.Errorf("lock wallets: found %d of %d", len(locked), len(ids))
	}
	wallets := make(map[LedgerAccount]*Wallet, len(locked))
	for _, wallet := range locked {
		wallets[accounts[wallet.Id]] = wallet
	}
	return wallets, nil
}

// ensureWallet returns the ID of the account's wallet, creating it with a
// zero balance when it does not exist. A user has one wallet; when it holds
// another currency the posting fails with money.ErrCurrencyMismatch.
func ensureWallet(tx *gorm.DB, account LedgerAccount) (int64, error) {
	findWallet := func() ([]Wallet, error) {
		var found []Wallet
		err := tx.Select("id", "balance_currency").
			Where("account_type = ? AND owner_id = ?", account.Type, account.OwnerId).
			Where("(balance_currency = ? OR account_type = ?)", account.Currency, LedgerAccountUserWallet).
			Limit(1).Find(&found).Error
		return found, err
	}
	found, err := findWallet()
	if err != nil {
		return 0, err
	}
	if len(found) == 0 {
		now := clock.Now().Unix()
		wallet := Wallet{
			AccountType: account.Type,
			OwnerId:     account.OwnerId,
			Balance:     money.Amount{Currency: account.Currency},
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		// A concurrent transaction may create the same wallet first.
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&wallet).Error; err != nil {
			return 0, err
		}
		if found, err = findWallet(); err != nil {
			return 0, err
		}
		if len(found) == 0 {
			return 0, fmt.Errorf("create wallet for %s %d", account.Type, account.OwnerId)
		}
	}
	existing := found[0]
	if existing.Balance.Currency != account.Currency {
		return 0, fmt.Errorf("%w: wallet holds %s, posting is in %s", money.ErrCurrencyMismatch, existing.Balance.Currency, account.Currency)
	}
	return existing.Id, nil
}

// WalletTopUp credits a user wallet. Source is the amount received; when it
// is not in the settlement currency, Rate must convert it into the
// settlement currency, and the entry records both.
type WalletTopUp struct {
	UserId int
	Source money.Amount
	Rate   *money.Rate
	// Reference identifies the payment, such as a bank transfer number. A
	// reference can be credited once.
	Reference  string
	OperatorId int
	Remark     string
}

// TopUpWallet writes a top-up entry. A converted amount is rounded down to
// 10^-9, so the platform never credits more than the payment is worth.
func TopUpWallet(topUp WalletTopUp) (*LedgerEntry, error) {
	if len(topUp.Reference) > ledgerReferenceMaxLength {
		return nil, fmt.Errorf("%w: reference is longer than %d characters", ErrLedgerInvalidEntry, ledgerReferenceMaxLength)
	}
	if topUp.Source.Nanos <= 0 {
		return nil, fmt.Errorf("%w: top-up amount must be positive", ErrLedgerInvalidEntry)
	}
	amount := topUp.Source
	if topUp.Rate == nil && topUp.Source.Currency != settlementCurrency {
		return nil, ErrLedgerRateRequired
	}
	if topUp.Rate != nil {
		if topUp.Rate.To() != settlementCurrency {
			return nil, fmt.Errorf("%w: rate converts to %s, wallets hold %s", money.ErrCurrencyMismatch, topUp.Rate.To(), settlementCurrency)
		}
		converted, err := topUp.Source.Value().Convert(*topUp.Rate)
		if err != nil {
			return nil, err
		}
		amount, err = converted.Credit()
		if err != nil {
			return nil, err
		}
	}
	posting := LedgerPosting{
		Account:      UserWalletAccount(topUp.UserId),
		Type:         LedgerEntryTopUp,
		Amount:       amount,
		OperatorId:   topUp.OperatorId,
		Remark:       topUp.Remark,
		sourceAmount: &topUp.Source,
	}
	if topUp.Rate != nil {
		posting.fxRate = decimal.NewNullDecimal(topUp.Rate.Decimal())
	}
	if topUp.Reference != "" {
		posting.IdempotencyKey = "top_up:" + topUp.Reference
	}
	entries, err := PostLedgerEntries(posting)
	if err != nil {
		return nil, err
	}
	return entries[0], nil
}

// ReverseLedgerEntry writes an entry that cancels entryId: the same account
// and type with the opposite amount. An entry can be reversed once, and a
// reversal cannot itself be reversed.
func ReverseLedgerEntry(entryId int64, operatorId int, remark string) (*LedgerEntry, error) {
	var reversal *LedgerEntry
	err := DB.Transaction(func(tx *gorm.DB) error {
		var original LedgerEntry
		if err := tx.First(&original, entryId).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrLedgerEntryNotFound
			}
			return err
		}
		if original.ReversesEntryId != nil || original.Type == LedgerEntryRepayment || original.AccountType == LedgerAccountUserReceivable {
			return ErrLedgerReversalNotReversible
		}
		var wallet Wallet
		if err := tx.Select("id", "account_type", "owner_id", "balance_currency").First(&wallet, original.WalletId).Error; err != nil {
			return err
		}
		amount, err := money.Amount{Currency: original.Amount.Currency}.Sub(original.Amount)
		if err != nil {
			return err
		}
		source, err := money.Amount{Currency: original.SourceAmount.Currency}.Sub(original.SourceAmount)
		if err != nil {
			return err
		}
		posting := LedgerPosting{
			Account:         LedgerAccount{Type: wallet.AccountType, OwnerId: wallet.OwnerId, Currency: wallet.Balance.Currency},
			Type:            original.Type,
			Amount:          amount,
			RequestId:       original.RequestId,
			AttemptId:       original.AttemptId,
			OperatorId:      operatorId,
			Remark:          remark,
			sourceAmount:    &source,
			fxRate:          original.FxRate,
			reversesEntryId: &original.Id,
		}
		if err := validateLedgerPosting(posting); err != nil {
			return err
		}
		entries, err := postLedgerEntries(tx, []LedgerPosting{posting})
		if err != nil {
			return err
		}
		reversal = entries[0]
		return nil
	})
	if err != nil {
		return nil, err
	}
	return reversal, nil
}

// GetUserWallet returns the user's wallet, or nil when the user has no
// ledger entries yet.
func GetUserWallet(userId int) (*Wallet, error) {
	var wallet Wallet
	err := DB.Where("account_type = ? AND owner_id = ?", LedgerAccountUserWallet, userId).First(&wallet).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &wallet, nil
}

// GetUserOutstanding returns unpaid receivables from the primary ledger.
func GetUserOutstanding(userId int) (money.Amount, error) {
	var wallet Wallet
	err := DB.Where("account_type = ? AND owner_id = ?", LedgerAccountUserReceivable, userId).Take(&wallet).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return money.Amount{Currency: SettlementCurrency()}, nil
	}
	return wallet.Balance, err
}

// GetLedgerEntryWallet returns the wallet that holds a ledger entry.
func GetLedgerEntryWallet(entryId int64) (*Wallet, error) {
	var wallet Wallet
	err := DB.Where("id = (?)", DB.Model(&LedgerEntry{}).Select("wallet_id").Where("id = ?", entryId)).First(&wallet).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrLedgerEntryNotFound
	}
	if err != nil {
		return nil, err
	}
	return &wallet, nil
}

// ListLedgerEntries returns a wallet's entries, newest first, and the total
// entry count.
func ListLedgerEntries(walletId int64, offset int, limit int) ([]*LedgerEntry, int64, error) {
	var total int64
	if err := DB.Model(&LedgerEntry{}).Where("wallet_id = ?", walletId).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var entries []*LedgerEntry
	err := DB.Where("wallet_id = ?", walletId).Order("id DESC").Offset(offset).Limit(limit).Find(&entries).Error
	if err != nil {
		return nil, 0, err
	}
	return entries, total, nil
}

// migrateWalletLedger creates the wallet and ledger tables with the
// constraints GORM tags cannot express: one wallet per account and currency,
// one wallet per user, non-zero amounts in the wallet currency, and a
// trigger that makes ledger entries append-only. It is idempotent.
func migrateWalletLedger(db *gorm.DB) error {
	if err := db.AutoMigrate(&Wallet{}, &LedgerEntry{}); err != nil {
		return err
	}
	statements := []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS "idx_wallets_account" ON "wallets" ("account_type", "owner_id", "balance_currency")`,
		`CREATE UNIQUE INDEX IF NOT EXISTS "idx_wallets_user_wallet" ON "wallets" ("owner_id") WHERE "account_type" = 'user_wallet'`,
		`CREATE INDEX IF NOT EXISTS "idx_ledger_entries_wallet_id_id" ON "ledger_entries" ("wallet_id", "id")`,
		`DO $$
BEGIN
	IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_ledger_entries_wallet' AND conrelid = '"ledger_entries"'::regclass) THEN
		ALTER TABLE "ledger_entries" ADD CONSTRAINT "fk_ledger_entries_wallet" FOREIGN KEY ("wallet_id") REFERENCES "wallets" ("id");
	END IF;
	IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_ledger_entries_amount_nonzero' AND conrelid = '"ledger_entries"'::regclass) THEN
		ALTER TABLE "ledger_entries" ADD CONSTRAINT "chk_ledger_entries_amount_nonzero" CHECK ("amount_nanos" <> 0);
	END IF;
END $$`,
		`CREATE OR REPLACE FUNCTION "ledger_entries_append_only"() RETURNS trigger AS $$
BEGIN
	RAISE EXCEPTION 'ledger_entries is append-only; write a reversal entry instead';
END
$$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS "ledger_entries_append_only" ON "ledger_entries"`,
		`CREATE TRIGGER "ledger_entries_append_only" BEFORE UPDATE OR DELETE ON "ledger_entries" FOR EACH ROW EXECUTE FUNCTION "ledger_entries_append_only"()`,
		`DROP TRIGGER IF EXISTS "ledger_entries_no_truncate" ON "ledger_entries"`,
		`CREATE TRIGGER "ledger_entries_no_truncate" BEFORE TRUNCATE ON "ledger_entries" FOR EACH STATEMENT EXECUTE FUNCTION "ledger_entries_append_only"()`,
	}
	return db.Transaction(func(tx *gorm.DB) error {
		for _, statement := range statements {
			if err := tx.Exec(statement).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// WalletMismatch is a wallet whose balance differs from the sum of its
// ledger entries, or that holds entries in another currency.
type WalletMismatch struct {
	WalletId    int64             `json:"wallet_id"`
	AccountType LedgerAccountType `json:"account_type"`
	OwnerId     int               `json:"owner_id"`
	Balance     money.Amount      `json:"balance"`
	// EntryTotalNanos is the sum of the wallet's entries in 10^-9 units. It is
	// a decimal because a corrupt ledger can sum beyond int64.
	EntryTotalNanos        decimal.Decimal `json:"entry_total_nanos"`
	ForeignCurrencyEntries int64           `json:"foreign_currency_entries"`
}

// WalletReconciliation is the result of one reconciliation run.
type WalletReconciliation struct {
	CheckedWallets int64            `json:"checked_wallets"`
	Mismatches     []WalletMismatch `json:"mismatches"`
}

// ReconcileWallets checks every wallet, batchSize wallets per query, and
// reports each one whose balance is not the sum of its ledger entries. Each
// query reads a batch of wallets and their entries from one snapshot, so
// postings committed during the run do not cause false mismatches.
func ReconcileWallets(batchSize int) (WalletReconciliation, error) {
	result := WalletReconciliation{Mismatches: []WalletMismatch{}}
	var lastId int64
	for {
		var rows []struct {
			Id                     int64
			AccountType            LedgerAccountType
			OwnerId                int
			BalanceNanos           int64
			BalanceCurrency        money.Currency
			EntryTotal             decimal.Decimal
			ForeignCurrencyEntries int64
		}
		batch := DB.Model(&Wallet{}).Where(`"id" > ?`, lastId).Order(`"id"`).Limit(batchSize)
		err := DB.Table("(?) AS w", batch).
			Select(`w."id", w."account_type", w."owner_id", w."balance_nanos", w."balance_currency",
				COALESCE(SUM(e."amount_nanos"), 0) AS "entry_total",
				COUNT(e."id") FILTER (WHERE e."amount_currency" <> w."balance_currency") AS "foreign_currency_entries"`).
			Joins(`LEFT JOIN "ledger_entries" e ON e."wallet_id" = w."id"`).
			Group(`w."id", w."account_type", w."owner_id", w."balance_nanos", w."balance_currency"`).
			Order(`w."id"`).
			Scan(&rows).Error
		if err != nil {
			return WalletReconciliation{}, err
		}
		for _, row := range rows {
			result.CheckedWallets++
			if row.EntryTotal.Equal(decimal.NewFromInt(row.BalanceNanos)) && row.ForeignCurrencyEntries == 0 {
				continue
			}
			result.Mismatches = append(result.Mismatches, WalletMismatch{
				WalletId:               row.Id,
				AccountType:            row.AccountType,
				OwnerId:                row.OwnerId,
				Balance:                money.Amount{Nanos: row.BalanceNanos, Currency: row.BalanceCurrency},
				EntryTotalNanos:        row.EntryTotal,
				ForeignCurrencyEntries: row.ForeignCurrencyEntries,
			})
		}
		if len(rows) < batchSize {
			return result, nil
		}
		lastId = rows[len(rows)-1].Id
	}
}
