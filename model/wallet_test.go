package model

import (
	"fmt"
	"math"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// useWalletLedgerDB gives the test its own PostgreSQL schema with the wallet
// and ledger tables migrated the same way startup migrates them.
func useWalletLedgerDB(t *testing.T) {
	t.Helper()
	UseTestDatabases(t)
	require.NoError(t, migrateWalletLedger(DB))
}

func usd(nanos int64) money.Amount {
	return money.Amount{Nanos: nanos, Currency: "USD"}
}

func mustRate(t *testing.T, from, to money.Currency, rate string) *money.Rate {
	t.Helper()
	r, err := money.NewRate(from, to, decimal.RequireFromString(rate))
	require.NoError(t, err)
	return &r
}

func TestTopUpWalletCreditsSettlementCurrency(t *testing.T) {
	useWalletLedgerDB(t)

	tests := []struct {
		name       string
		userID     int
		source     money.Amount
		rate       *money.Rate
		wantAmount money.Amount
		wantRate   string
	}{
		{
			name:       "settlement currency",
			userID:     1,
			source:     usd(10_000_000_000),
			wantAmount: usd(10_000_000_000),
		},
		{
			// 1 CNY × 0.1234567891 = 0.1234567891 USD; a credit rounds down.
			name:       "other currency at the given rate",
			userID:     2,
			source:     money.Amount{Nanos: 1_000_000_000, Currency: "CNY"},
			rate:       mustRate(t, "CNY", "USD", "0.1234567891"),
			wantAmount: usd(123_456_789),
			wantRate:   "0.1234567891",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry, err := TopUpWallet(WalletTopUp{UserId: tt.userID, Source: tt.source, Rate: tt.rate, Reference: tt.name, OperatorId: 7})
			require.NoError(t, err)

			assert.Equal(t, LedgerEntryTopUp, entry.Type)
			assert.Equal(t, tt.wantAmount, entry.Amount)
			assert.Equal(t, tt.wantAmount, entry.BalanceAfter)
			assert.Equal(t, tt.source, entry.SourceAmount)
			if tt.wantRate == "" {
				assert.False(t, entry.FxRate.Valid)
			} else {
				require.True(t, entry.FxRate.Valid)
				assert.Equal(t, tt.wantRate, entry.FxRate.Decimal.String())
			}
			assert.Equal(t, 7, entry.OperatorId)

			wallet, err := GetUserWallet(tt.userID)
			require.NoError(t, err)
			require.NotNil(t, wallet)
			assert.Equal(t, tt.wantAmount, wallet.Balance)

			entries, total, err := ListLedgerEntries(wallet.Id, 0, 10)
			require.NoError(t, err)
			assert.Equal(t, int64(1), total)
			require.Len(t, entries, 1)
			assert.Equal(t, entry.Id, entries[0].Id)
			assert.Equal(t, tt.source, entries[0].SourceAmount)
			assert.Equal(t, entry.FxRate.Decimal.String(), entries[0].FxRate.Decimal.String())
		})
	}
}

func TestTopUpWalletRejectsInvalidConversion(t *testing.T) {
	useWalletLedgerDB(t)

	tests := []struct {
		name    string
		source  money.Amount
		rate    *money.Rate
		wantErr error
	}{
		{name: "other currency without a rate", source: money.Amount{Nanos: 1, Currency: "CNY"}, wantErr: ErrLedgerRateRequired},
		{name: "rate from another currency", source: money.Amount{Nanos: 1, Currency: "CNY"}, rate: mustRate(t, "EUR", "USD", "1.1"), wantErr: money.ErrCurrencyMismatch},
		{name: "rate to another currency", source: money.Amount{Nanos: 1, Currency: "CNY"}, rate: mustRate(t, "CNY", "EUR", "0.1"), wantErr: money.ErrCurrencyMismatch},
		{name: "rate for the settlement currency", source: usd(1), rate: mustRate(t, "CNY", "USD", "0.1"), wantErr: money.ErrCurrencyMismatch},
		{name: "zero amount", source: usd(0), wantErr: ErrLedgerInvalidEntry},
		{name: "negative amount", source: usd(-1), wantErr: ErrLedgerInvalidEntry},
		{name: "rounds down to zero", source: money.Amount{Nanos: 1, Currency: "CNY"}, rate: mustRate(t, "CNY", "USD", "0.5"), wantErr: ErrLedgerInvalidEntry},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := TopUpWallet(WalletTopUp{UserId: 1, Source: tt.source, Rate: tt.rate})
			require.ErrorIs(t, err, tt.wantErr)
		})
	}

	wallet, err := GetUserWallet(1)
	require.NoError(t, err)
	assert.Nil(t, wallet, "a rejected top-up must not leave a wallet behind")
}

func TestPostLedgerEntriesEnforcesTypeAndSign(t *testing.T) {
	useWalletLedgerDB(t)
	_, err := TopUpWallet(WalletTopUp{UserId: 1, Source: usd(100)})
	require.NoError(t, err)

	user := UserWalletAccount(1)
	revenue := PlatformRevenueAccount("USD")
	payable := ProviderPayableAccount(3, "CNY")
	tests := []struct {
		name    string
		posting LedgerPosting
		wantErr error
	}{
		{name: "user pre-consume", posting: LedgerPosting{Account: user, Type: LedgerEntryPreConsume, Amount: usd(-10)}},
		{name: "user pre-consume release", posting: LedgerPosting{Account: user, Type: LedgerEntryPreConsumeRelease, Amount: usd(10)}},
		{name: "user charge", posting: LedgerPosting{Account: user, Type: LedgerEntryCharge, Amount: usd(-5)}},
		{name: "user refund", posting: LedgerPosting{Account: user, Type: LedgerEntryRefund, Amount: usd(5)}},
		{name: "user adjustment up", posting: LedgerPosting{Account: user, Type: LedgerEntryAdjustment, Amount: usd(1)}},
		{name: "user adjustment down", posting: LedgerPosting{Account: user, Type: LedgerEntryAdjustment, Amount: usd(-1)}},
		{name: "user opening balance", posting: LedgerPosting{Account: user, Type: LedgerEntryOpeningBalance, Amount: usd(1)}},
		{name: "revenue charge", posting: LedgerPosting{Account: revenue, Type: LedgerEntryCharge, Amount: usd(5)}},
		{name: "revenue refund", posting: LedgerPosting{Account: revenue, Type: LedgerEntryRefund, Amount: usd(-5)}},
		{name: "provider cost", posting: LedgerPosting{Account: payable, Type: LedgerEntryCharge, Amount: money.Amount{Nanos: 7, Currency: "CNY"}}},

		{name: "positive pre-consume", posting: LedgerPosting{Account: user, Type: LedgerEntryPreConsume, Amount: usd(10)}, wantErr: ErrLedgerInvalidEntry},
		{name: "negative release", posting: LedgerPosting{Account: user, Type: LedgerEntryPreConsumeRelease, Amount: usd(-10)}, wantErr: ErrLedgerInvalidEntry},
		{name: "positive charge", posting: LedgerPosting{Account: user, Type: LedgerEntryCharge, Amount: usd(5)}, wantErr: ErrLedgerInvalidEntry},
		{name: "negative refund", posting: LedgerPosting{Account: user, Type: LedgerEntryRefund, Amount: usd(-5)}, wantErr: ErrLedgerInvalidEntry},
		{name: "negative top-up", posting: LedgerPosting{Account: user, Type: LedgerEntryTopUp, Amount: usd(-5)}, wantErr: ErrLedgerInvalidEntry},
		{name: "negative opening balance", posting: LedgerPosting{Account: user, Type: LedgerEntryOpeningBalance, Amount: usd(-1)}, wantErr: ErrLedgerInvalidEntry},
		{name: "zero adjustment", posting: LedgerPosting{Account: user, Type: LedgerEntryAdjustment, Amount: usd(0)}, wantErr: ErrLedgerInvalidEntry},
		{name: "negative revenue charge", posting: LedgerPosting{Account: revenue, Type: LedgerEntryCharge, Amount: usd(-5)}, wantErr: ErrLedgerInvalidEntry},
		{name: "pre-consume on revenue", posting: LedgerPosting{Account: revenue, Type: LedgerEntryPreConsume, Amount: usd(-5)}, wantErr: ErrLedgerInvalidEntry},
		{name: "top-up on provider payable", posting: LedgerPosting{Account: payable, Type: LedgerEntryTopUp, Amount: money.Amount{Nanos: 7, Currency: "CNY"}}, wantErr: ErrLedgerInvalidEntry},
		{name: "unknown entry type", posting: LedgerPosting{Account: user, Type: "bonus", Amount: usd(1)}, wantErr: ErrLedgerInvalidEntry},
		{name: "unknown account type", posting: LedgerPosting{Account: LedgerAccount{Type: "escrow", Currency: "USD"}, Type: LedgerEntryAdjustment, Amount: usd(1)}, wantErr: ErrLedgerInvalidEntry},
		{name: "amount in another currency", posting: LedgerPosting{Account: user, Type: LedgerEntryAdjustment, Amount: money.Amount{Nanos: 1, Currency: "CNY"}}, wantErr: money.ErrCurrencyMismatch},
		{name: "user wallet in another currency", posting: LedgerPosting{Account: LedgerAccount{Type: LedgerAccountUserWallet, OwnerId: 1, Currency: "CNY"}, Type: LedgerEntryAdjustment, Amount: money.Amount{Nanos: 1, Currency: "CNY"}}, wantErr: money.ErrCurrencyMismatch},
		{name: "balance overflow", posting: LedgerPosting{Account: user, Type: LedgerEntryAdjustment, Amount: usd(math.MaxInt64)}, wantErr: money.ErrOverflow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries, err := PostLedgerEntries(tt.posting)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Len(t, entries, 1)
			assert.Equal(t, tt.posting.Amount, entries[0].Amount)
			assert.Equal(t, tt.posting.Type, entries[0].Type)
			assert.Equal(t, tt.posting.Account.Type, entries[0].AccountType)
		})
	}

	// 100 + (-10 + 10 - 5 + 5 + 1 - 1 + 1) = 101; rejected postings change nothing.
	wallet, err := GetUserWallet(1)
	require.NoError(t, err)
	assert.Equal(t, usd(101), wallet.Balance)
}

func TestPostLedgerEntriesKeepsUserWalletNonNegative(t *testing.T) {
	useWalletLedgerDB(t)
	_, err := TopUpWallet(WalletTopUp{UserId: 1, Source: usd(10)})
	require.NoError(t, err)
	user := UserWalletAccount(1)

	_, err = PostLedgerEntries(LedgerPosting{Account: user, Type: LedgerEntryPreConsume, Amount: usd(-11)})
	require.ErrorIs(t, err, ErrInsufficientWalletBalance)

	entries, err := PostLedgerEntries(LedgerPosting{Account: user, Type: LedgerEntryCharge, Amount: usd(-10)})
	require.NoError(t, err)
	assert.Equal(t, usd(0), entries[0].BalanceAfter)

	_, err = PostLedgerEntries(LedgerPosting{Account: user, Type: LedgerEntryAdjustment, Amount: usd(-1)})
	require.ErrorIs(t, err, ErrInsufficientWalletBalance)

	// Platform and Provider accounts may go negative, for example after a refund.
	entries, err = PostLedgerEntries(LedgerPosting{Account: PlatformRevenueAccount("USD"), Type: LedgerEntryRefund, Amount: usd(-3)})
	require.NoError(t, err)
	assert.Equal(t, usd(-3), entries[0].BalanceAfter)
}

func TestPostLedgerEntriesIsAllOrNothing(t *testing.T) {
	useWalletLedgerDB(t)
	_, err := TopUpWallet(WalletTopUp{UserId: 1, Source: usd(10)})
	require.NoError(t, err)

	// A request charge to the user and the matching revenue, then a second
	// charge the wallet cannot cover: the whole batch must roll back.
	_, err = PostLedgerEntries(
		LedgerPosting{Account: UserWalletAccount(1), Type: LedgerEntryCharge, Amount: usd(-6), RequestId: "req-1"},
		LedgerPosting{Account: PlatformRevenueAccount("USD"), Type: LedgerEntryCharge, Amount: usd(6), RequestId: "req-1"},
		LedgerPosting{Account: UserWalletAccount(1), Type: LedgerEntryCharge, Amount: usd(-6), RequestId: "req-2"},
	)
	require.ErrorIs(t, err, ErrInsufficientWalletBalance)

	wallet, err := GetUserWallet(1)
	require.NoError(t, err)
	assert.Equal(t, usd(10), wallet.Balance)
	_, total, err := ListLedgerEntries(wallet.Id, 0, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	var revenueWallets int64
	require.NoError(t, DB.Model(&Wallet{}).Where("account_type = ?", LedgerAccountPlatformRevenue).Count(&revenueWallets).Error)
	assert.Zero(t, revenueWallets)

	entries, err := PostLedgerEntries(
		LedgerPosting{Account: UserWalletAccount(1), Type: LedgerEntryCharge, Amount: usd(-6), RequestId: "req-1", AttemptId: "att-1"},
		LedgerPosting{Account: PlatformRevenueAccount("USD"), Type: LedgerEntryCharge, Amount: usd(6), RequestId: "req-1"},
	)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, usd(4), entries[0].BalanceAfter)
	assert.Equal(t, usd(6), entries[1].BalanceAfter)
	assert.Equal(t, "req-1", entries[0].RequestId)
	assert.Equal(t, "att-1", entries[0].AttemptId)
}

func TestLedgerIdempotencyKeyIsUnique(t *testing.T) {
	useWalletLedgerDB(t)
	first, err := TopUpWallet(WalletTopUp{UserId: 1, Source: usd(10), Reference: "bank-42"})
	require.NoError(t, err)

	_, err = TopUpWallet(WalletTopUp{UserId: 1, Source: usd(10), Reference: "bank-42"})
	require.ErrorIs(t, err, ErrDuplicateLedgerEntry)
	_, err = TopUpWallet(WalletTopUp{UserId: 2, Source: usd(10), Reference: "bank-42"})
	require.ErrorIs(t, err, ErrDuplicateLedgerEntry)

	wallet, err := GetUserWallet(1)
	require.NoError(t, err)
	assert.Equal(t, first.BalanceAfter, wallet.Balance)

	posting := LedgerPosting{Account: UserWalletAccount(1), Type: LedgerEntryPreConsume, Amount: usd(-1), RequestId: "req-1", IdempotencyKey: "pre_consume:req-1"}
	_, err = PostLedgerEntries(posting)
	require.NoError(t, err)
	_, err = PostLedgerEntries(posting)
	require.ErrorIs(t, err, ErrDuplicateLedgerEntry)
}

func TestLedgerEntriesAreAppendOnly(t *testing.T) {
	useWalletLedgerDB(t)
	entry, err := TopUpWallet(WalletTopUp{UserId: 1, Source: usd(10)})
	require.NoError(t, err)

	require.Error(t, DB.Model(&LedgerEntry{}).Where("id = ?", entry.Id).Update("amount_nanos", 20).Error)
	require.Error(t, DB.Where("id = ?", entry.Id).Delete(&LedgerEntry{}).Error)
	require.Error(t, DB.Exec("TRUNCATE ledger_entries CASCADE").Error)
	require.Error(t, DB.Exec(`INSERT INTO ledger_entries (wallet_id, account_type, type, amount_nanos, amount_currency, balance_after_nanos, balance_after_currency, source_nanos, source_currency, created_at)
		VALUES (?, 'user_wallet', 'adjustment', 0, 'USD', 10, 'USD', 0, 'USD', 0)`, entry.WalletId).Error, "a zero amount is rejected by the database")

	var stored LedgerEntry
	require.NoError(t, DB.First(&stored, entry.Id).Error)
	assert.Equal(t, usd(10), stored.Amount)
}

func TestReverseLedgerEntry(t *testing.T) {
	useWalletLedgerDB(t)
	topUp, err := TopUpWallet(WalletTopUp{
		UserId: 1,
		Source: money.Amount{Nanos: 10_000_000_000, Currency: "CNY"},
		Rate:   mustRate(t, "CNY", "USD", "0.14"),
	})
	require.NoError(t, err)
	require.Equal(t, usd(1_400_000_000), topUp.Amount)
	charges, err := PostLedgerEntries(LedgerPosting{Account: UserWalletAccount(1), Type: LedgerEntryCharge, Amount: usd(-400_000_000), RequestId: "req-1"})
	require.NoError(t, err)

	reversal, err := ReverseLedgerEntry(charges[0].Id, 9, "charged twice")
	require.NoError(t, err)
	assert.Equal(t, LedgerEntryCharge, reversal.Type)
	assert.Equal(t, usd(400_000_000), reversal.Amount)
	assert.Equal(t, usd(1_400_000_000), reversal.BalanceAfter)
	require.NotNil(t, reversal.ReversesEntryId)
	assert.Equal(t, charges[0].Id, *reversal.ReversesEntryId)
	assert.Equal(t, "req-1", reversal.RequestId)
	assert.Equal(t, 9, reversal.OperatorId)
	assert.Equal(t, "charged twice", reversal.Remark)

	_, err = ReverseLedgerEntry(charges[0].Id, 9, "again")
	require.ErrorIs(t, err, ErrLedgerEntryAlreadyReversed)
	_, err = ReverseLedgerEntry(reversal.Id, 9, "undo the undo")
	require.ErrorIs(t, err, ErrLedgerReversalNotReversible)
	_, err = ReverseLedgerEntry(999_999, 9, "missing")
	require.ErrorIs(t, err, ErrLedgerEntryNotFound)

	topUpReversal, err := ReverseLedgerEntry(topUp.Id, 9, "payment bounced")
	require.NoError(t, err)
	assert.Equal(t, usd(-1_400_000_000), topUpReversal.Amount)
	assert.Equal(t, money.Amount{Nanos: -10_000_000_000, Currency: "CNY"}, topUpReversal.SourceAmount)
	require.True(t, topUpReversal.FxRate.Valid)
	assert.Equal(t, "0.14", topUpReversal.FxRate.Decimal.String())
	assert.Equal(t, usd(0), topUpReversal.BalanceAfter)

	wallet, err := GetUserWallet(1)
	require.NoError(t, err)
	assert.Equal(t, usd(0), wallet.Balance)
}

func TestReverseLedgerEntryKeepsUserWalletNonNegative(t *testing.T) {
	useWalletLedgerDB(t)
	topUp, err := TopUpWallet(WalletTopUp{UserId: 1, Source: usd(10)})
	require.NoError(t, err)
	_, err = PostLedgerEntries(LedgerPosting{Account: UserWalletAccount(1), Type: LedgerEntryCharge, Amount: usd(-4)})
	require.NoError(t, err)

	_, err = ReverseLedgerEntry(topUp.Id, 9, "payment bounced after use")
	require.ErrorIs(t, err, ErrInsufficientWalletBalance)
}

func TestReconcileWalletsReportsBalanceThatDiffersFromEntries(t *testing.T) {
	useWalletLedgerDB(t)
	for userID := 1; userID <= 3; userID++ {
		_, err := TopUpWallet(WalletTopUp{UserId: userID, Source: usd(100)})
		require.NoError(t, err)
	}
	_, err := PostLedgerEntries(
		LedgerPosting{Account: UserWalletAccount(2), Type: LedgerEntryCharge, Amount: usd(-30)},
		LedgerPosting{Account: PlatformRevenueAccount("USD"), Type: LedgerEntryCharge, Amount: usd(30)},
	)
	require.NoError(t, err)

	result, err := ReconcileWallets(2)
	require.NoError(t, err)
	assert.Equal(t, int64(4), result.CheckedWallets)
	assert.Empty(t, result.Mismatches)

	// Simulate a balance written outside the ledger.
	wallet, err := GetUserWallet(2)
	require.NoError(t, err)
	require.NoError(t, DB.Model(&Wallet{}).Where("id = ?", wallet.Id).Update("balance_nanos", 75).Error)

	result, err = ReconcileWallets(2)
	require.NoError(t, err)
	assert.Equal(t, int64(4), result.CheckedWallets)
	require.Len(t, result.Mismatches, 1)
	mismatch := result.Mismatches[0]
	assert.Equal(t, wallet.Id, mismatch.WalletId)
	assert.Equal(t, LedgerAccountUserWallet, mismatch.AccountType)
	assert.Equal(t, 2, mismatch.OwnerId)
	assert.Equal(t, usd(75), mismatch.Balance)
	assert.Equal(t, "70", mismatch.EntryTotalNanos.String())
}

func TestConcurrentChargesKeepBalanceEqualToEntries(t *testing.T) {
	useWalletLedgerDB(t)
	_, err := TopUpWallet(WalletTopUp{UserId: 1, Source: usd(100)})
	require.NoError(t, err)

	// 40 requests each charge 3 against a balance of 100: exactly 33 fit.
	const requests = 40
	errs := make(chan error, requests)
	var wg sync.WaitGroup
	for i := range requests {
		wg.Go(func() {
			_, err := PostLedgerEntries(
				LedgerPosting{Account: UserWalletAccount(1), Type: LedgerEntryCharge, Amount: usd(-3), RequestId: fmt.Sprintf("req-%d", i)},
				LedgerPosting{Account: PlatformRevenueAccount("USD"), Type: LedgerEntryCharge, Amount: usd(3), RequestId: fmt.Sprintf("req-%d", i)},
			)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)

	succeeded := 0
	for err := range errs {
		if err == nil {
			succeeded++
			continue
		}
		require.ErrorIs(t, err, ErrInsufficientWalletBalance)
	}
	assert.Equal(t, 33, succeeded)

	wallet, err := GetUserWallet(1)
	require.NoError(t, err)
	assert.Equal(t, usd(1), wallet.Balance)
	var revenue Wallet
	require.NoError(t, DB.Where("account_type = ?", LedgerAccountPlatformRevenue).First(&revenue).Error)
	assert.Equal(t, usd(99), revenue.Balance)

	result, err := ReconcileWallets(100)
	require.NoError(t, err)
	assert.Equal(t, int64(2), result.CheckedWallets)
	assert.Empty(t, result.Mismatches)
}

func TestMigrateWalletLedgerIsIdempotent(t *testing.T) {
	useWalletLedgerDB(t)
	entry, err := TopUpWallet(WalletTopUp{UserId: 1, Source: usd(10), Reference: "bank-1"})
	require.NoError(t, err)

	require.NoError(t, migrateWalletLedger(DB))

	wallet, err := GetUserWallet(1)
	require.NoError(t, err)
	assert.Equal(t, usd(10), wallet.Balance)
	_, err = TopUpWallet(WalletTopUp{UserId: 1, Source: usd(10), Reference: "bank-1"})
	require.ErrorIs(t, err, ErrDuplicateLedgerEntry)
	require.Error(t, DB.Where("id = ?", entry.Id).Delete(&LedgerEntry{}).Error)
	require.Error(t, DB.Create(&Wallet{AccountType: LedgerAccountUserWallet, OwnerId: 1, Balance: money.Amount{Currency: "CNY"}}).Error, "a user has one wallet")
}
