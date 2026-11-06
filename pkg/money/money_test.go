package money_test

import (
	"math"
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/pkg/testdb"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// maxUnits is math.MaxInt64 10^-9 units written in major units.
const maxUnits = "9223372036.854775807"

func value(t *testing.T, amount string, currency money.Currency) money.Value {
	t.Helper()
	v, err := money.ParseValue(amount, currency)
	require.NoError(t, err)
	return v
}

func TestParseCurrency(t *testing.T) {
	tests := []struct {
		code    string
		wantErr bool
	}{
		{code: "USD"},
		{code: "CNY"},
		{code: "usd", wantErr: true},
		{code: "US", wantErr: true},
		{code: "USDT", wantErr: true},
		{code: "U$D", wantErr: true},
		{code: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			currency, err := money.ParseCurrency(tt.code)
			if tt.wantErr {
				require.ErrorIs(t, err, money.ErrInvalidCurrency)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, money.Currency(tt.code), currency)
		})
	}
}

func TestNewValueRejectsInvalidInput(t *testing.T) {
	_, err := money.ParseValue("1", "usd")
	require.ErrorIs(t, err, money.ErrInvalidCurrency)

	_, err = money.ParseValue("NaN", "USD")
	require.Error(t, err)

	_, err = money.ParseValue("9223372036.8547758071", "USD")
	require.ErrorIs(t, err, money.ErrOverflow)

	_, err = money.ParseValue("-9223372036.8547758081", "USD")
	require.ErrorIs(t, err, money.ErrOverflow)

	_, err = money.NewAmount(1, "")
	require.ErrorIs(t, err, money.ErrInvalidCurrency)
}

func TestValueArithmetic(t *testing.T) {
	tests := []struct {
		name    string
		op      func(t *testing.T) (money.Value, error)
		want    string
		wantErr error
	}{
		{
			name: "add",
			op:   func(t *testing.T) (money.Value, error) { return value(t, "0.1", "USD").Add(value(t, "0.2", "USD")) },
			want: "0.3 USD",
		},
		{
			name:    "add different currencies",
			op:      func(t *testing.T) (money.Value, error) { return value(t, "1", "USD").Add(value(t, "1", "CNY")) },
			wantErr: money.ErrCurrencyMismatch,
		},
		{
			name: "add up to the maximum",
			op: func(t *testing.T) (money.Value, error) {
				return value(t, "9223372036", "USD").Add(value(t, "0.854775807", "USD"))
			},
			want: maxUnits + " USD",
		},
		{
			name: "add past the maximum",
			op: func(t *testing.T) (money.Value, error) {
				return value(t, maxUnits, "USD").Add(value(t, "0.000000001", "USD"))
			},
			wantErr: money.ErrOverflow,
		},
		{
			name: "sub to a negative value",
			op:   func(t *testing.T) (money.Value, error) { return value(t, "1", "USD").Sub(value(t, "1.25", "USD")) },
			want: "-0.25 USD",
		},
		{
			name:    "sub different currencies",
			op:      func(t *testing.T) (money.Value, error) { return value(t, "1", "USD").Sub(value(t, "1", "EUR")) },
			wantErr: money.ErrCurrencyMismatch,
		},
		{
			name: "sub past the minimum",
			op: func(t *testing.T) (money.Value, error) {
				return value(t, "-9223372036.854775808", "USD").Sub(value(t, "0.000000001", "USD"))
			},
			wantErr: money.ErrOverflow,
		},
		{
			name: "mul quantity",
			op:   func(t *testing.T) (money.Value, error) { return value(t, "0.04", "USD").MulQuantity(128) },
			want: "5.12 USD",
		},
		{
			name: "mul negative quantity",
			op:   func(t *testing.T) (money.Value, error) { return value(t, "0.04", "USD").MulQuantity(-3) },
			want: "-0.12 USD",
		},
		{
			name: "mul quantity overflow",
			op: func(t *testing.T) (money.Value, error) {
				return value(t, "0.000000002", "USD").MulQuantity(math.MaxInt64)
			},
			wantErr: money.ErrOverflow,
		},
		{
			name: "mul unit price keeps digits below 10^-9",
			op: func(t *testing.T) (money.Value, error) {
				// 7 tokens at $0.0375 per 1M tokens.
				return money.MulUnitPrice(decimal.RequireFromString("0.000007"), value(t, "0.0375", "USD"))
			},
			want: "0.0000002625 USD",
		},
		{
			name: "mul unit price overflow",
			op: func(t *testing.T) (money.Value, error) {
				return money.MulUnitPrice(decimal.RequireFromString("2147483647"), value(t, "5", "USD"))
			},
			wantErr: money.ErrOverflow,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.op(t)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got.String())
		})
	}
}

func TestValueConvert(t *testing.T) {
	usdToCNY, err := money.NewRate("USD", "CNY", decimal.RequireFromString("7.1234"))
	require.NoError(t, err)

	converted, err := value(t, "0.0000002625", "USD").Convert(usdToCNY)
	require.NoError(t, err)
	assert.Equal(t, "0.0000018698925 CNY", converted.String())

	_, err = value(t, "1", "EUR").Convert(usdToCNY)
	require.ErrorIs(t, err, money.ErrCurrencyMismatch)

	_, err = value(t, "2000000000", "USD").Convert(usdToCNY)
	require.ErrorIs(t, err, money.ErrOverflow)

	invalidRates := []struct {
		name     string
		from, to money.Currency
		rate     string
	}{
		{name: "zero", from: "USD", to: "CNY", rate: "0"},
		{name: "negative", from: "USD", to: "CNY", rate: "-7.1"},
		{name: "same currency", from: "USD", to: "USD", rate: "1"},
	}
	for _, tt := range invalidRates {
		t.Run(tt.name, func(t *testing.T) {
			_, err := money.NewRate(tt.from, tt.to, decimal.RequireFromString(tt.rate))
			require.ErrorIs(t, err, money.ErrInvalidRate)
		})
	}
	_, err = money.NewRate("USD", "cny", decimal.RequireFromString("7.1"))
	require.ErrorIs(t, err, money.ErrInvalidCurrency)
}

func TestValueWrite(t *testing.T) {
	tests := []struct {
		name           string
		value          string
		wantReceivable int64
		wantCost       int64
		costErr        error
	}{
		{name: "exact", value: "1.5", wantReceivable: 1_500_000_000, wantCost: 1_500_000_000},
		{name: "zero", value: "0", wantReceivable: 0, wantCost: 0},
		{name: "positive fraction rounds up", value: "0.0000002625", wantReceivable: 263, costErr: money.ErrInexact},
		{name: "smallest positive fraction rounds up", value: "0.0000000000001", wantReceivable: 1, costErr: money.ErrInexact},
		{name: "negative fraction rounds toward positive", value: "-0.0000002625", wantReceivable: -262, costErr: money.ErrInexact},
		{name: "maximum", value: maxUnits, wantReceivable: math.MaxInt64, wantCost: math.MaxInt64},
		{name: "minimum", value: "-9223372036.854775808", wantReceivable: math.MinInt64, wantCost: math.MinInt64},
		{name: "fraction below the maximum rounds up to it", value: "9223372036.8547758061", wantReceivable: math.MaxInt64, costErr: money.ErrInexact},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := value(t, tt.value, "USD")

			receivable, err := v.Receivable()
			require.NoError(t, err)
			assert.Equal(t, money.Amount{Nanos: tt.wantReceivable, Currency: "USD"}, receivable)

			cost, err := v.Cost()
			if tt.costErr != nil {
				require.ErrorIs(t, err, tt.costErr)
			} else {
				require.NoError(t, err)
				assert.Equal(t, money.Amount{Nanos: tt.wantCost, Currency: "USD"}, cost)
			}
		})
	}
}

func TestAmountArithmetic(t *testing.T) {
	usd := func(nanos int64) money.Amount { return money.Amount{Nanos: nanos, Currency: "USD"} }
	tests := []struct {
		name    string
		op      func() (money.Amount, error)
		want    money.Amount
		wantErr error
	}{
		{name: "add", op: func() (money.Amount, error) { return usd(1).Add(usd(2)) }, want: usd(3)},
		{name: "add up to the maximum", op: func() (money.Amount, error) { return usd(math.MaxInt64 - 1).Add(usd(1)) }, want: usd(math.MaxInt64)},
		{name: "add overflow", op: func() (money.Amount, error) { return usd(math.MaxInt64).Add(usd(1)) }, wantErr: money.ErrOverflow},
		{
			name:    "add different currencies",
			op:      func() (money.Amount, error) { return usd(1).Add(money.Amount{Nanos: 1, Currency: "CNY"}) },
			wantErr: money.ErrCurrencyMismatch,
		},
		{name: "sub", op: func() (money.Amount, error) { return usd(1).Sub(usd(3)) }, want: usd(-2)},
		{name: "sub down to the minimum", op: func() (money.Amount, error) { return usd(-1).Sub(usd(math.MaxInt64)) }, want: usd(math.MinInt64)},
		{name: "sub overflow", op: func() (money.Amount, error) { return usd(math.MinInt64).Sub(usd(1)) }, wantErr: money.ErrOverflow},
		{
			name:    "sub different currencies",
			op:      func() (money.Amount, error) { return usd(1).Sub(money.Amount{Nanos: 1, Currency: "CNY"}) },
			wantErr: money.ErrCurrencyMismatch,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.op()
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

type ledgerRow struct {
	ID     int64
	Amount money.Amount `gorm:"embedded;embeddedPrefix:amount_"`
}

func TestAmountPostgresStorageAndSum(t *testing.T) {
	dsn := testdb.PostgresDSN(t)
	if dsn == "" {
		t.Skipf("%s is not set", testdb.PostgresEnv)
	}
	db, _, err := model.OpenDSN(testdb.PostgresEnv, dsn, false)
	require.NoError(t, err)
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	require.NoError(t, db.AutoMigrate(&ledgerRow{}))

	rows := []ledgerRow{
		{Amount: money.Amount{Nanos: math.MaxInt64, Currency: "USD"}},
		{Amount: money.Amount{Nanos: math.MaxInt64, Currency: "USD"}},
		{Amount: money.Amount{Nanos: 263, Currency: "USD"}},
		{Amount: money.Amount{Nanos: math.MinInt64, Currency: "CNY"}},
		{Amount: money.Amount{Nanos: -1, Currency: "CNY"}},
	}
	require.NoError(t, db.Create(&rows).Error)

	var stored []ledgerRow
	require.NoError(t, db.Order("id").Find(&stored).Error)
	require.Len(t, stored, len(rows))
	for i := range rows {
		assert.Equal(t, rows[i].Amount, stored[i].Amount)
	}

	// PostgreSQL returns SUM(bigint) as numeric, so a total beyond int64 stays exact.
	var sums []struct {
		Currency money.Currency
		Total    decimal.Decimal
	}
	require.NoError(t, db.Model(&ledgerRow{}).
		Select("amount_currency AS currency, SUM(amount_nanos) AS total").
		Group("amount_currency").Order("amount_currency").
		Scan(&sums).Error)
	require.Len(t, sums, 2)
	assert.Equal(t, money.Currency("CNY"), sums[0].Currency)
	assert.Equal(t, "-9223372036854775809", sums[0].Total.String())
	assert.Equal(t, money.Currency("USD"), sums[1].Currency)
	assert.Equal(t, "18446744073709551877", sums[1].Total.String())
}
