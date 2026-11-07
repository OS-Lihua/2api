// Package money represents amounts as a fixed-point int64 in 10^-9 units of
// an ISO 4217 currency and calculates them with exact decimals, never float64.
//
// Amount is the stored form. Value is the unrounded form used during
// calculation. Every operation rejects a currency mismatch and any result
// outside the int64 range of 10^-9 units instead of truncating it. A Value
// becomes an Amount in one of three ways: Receivable rounds a user charge up
// to 10^-9, Credit rounds a user credit down to 10^-9, and Cost rounds a
// purchase cost to the nearest 10^-9.
package money

import (
	"errors"
	"fmt"
	"math"

	"github.com/shopspring/decimal"
)

// Scale is the number of decimal places an Amount stores: one Amount unit is
// 10^-9 of the currency's major unit.
const Scale = 9

var (
	ErrInvalidCurrency  = errors.New("money: invalid ISO 4217 currency code")
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	ErrOverflow         = errors.New("money: amount outside the int64 range of 10^-9 units")
	ErrInvalidRate      = errors.New("money: invalid exchange rate")
)

var (
	maxNanos = decimal.NewFromInt(math.MaxInt64)
	minNanos = decimal.NewFromInt(math.MinInt64)
)

// Currency is an ISO 4217 alphabetic code such as "USD".
type Currency string

// ParseCurrency accepts a code of exactly three uppercase ASCII letters.
func ParseCurrency(code string) (Currency, error) {
	if len(code) != 3 {
		return "", fmt.Errorf("%w: %q", ErrInvalidCurrency, code)
	}
	for i := range len(code) {
		if code[i] < 'A' || code[i] > 'Z' {
			return "", fmt.Errorf("%w: %q", ErrInvalidCurrency, code)
		}
	}
	return Currency(code), nil
}

// Amount is a stored amount: Nanos counts 10^-9 units of Currency. Embed it in
// a GORM model with `gorm:"embedded;embeddedPrefix:<name>_"` to get a bigint
// and a char(3) column. In JSON, Nanos is a string so that JavaScript clients
// do not lose precision above 2^53.
type Amount struct {
	Nanos    int64    `json:"nanos,string" gorm:"type:bigint;not null"`
	Currency Currency `json:"currency" gorm:"type:char(3);not null"`
}

// NewAmount validates the currency code.
func NewAmount(nanos int64, currency Currency) (Amount, error) {
	if _, err := ParseCurrency(string(currency)); err != nil {
		return Amount{}, err
	}
	return Amount{Nanos: nanos, Currency: currency}, nil
}

// Value returns the exact calculation form of the amount.
func (a Amount) Value() Value {
	return Value{amount: decimal.New(a.Nanos, -Scale), currency: a.Currency}
}

// Add returns a + b. It rejects a currency mismatch and an int64 overflow.
func (a Amount) Add(b Amount) (Amount, error) {
	sum, err := a.Value().Add(b.Value())
	if err != nil {
		return Amount{}, err
	}
	return sum.Cost()
}

// Sub returns a - b. It rejects a currency mismatch and an int64 overflow.
func (a Amount) Sub(b Amount) (Amount, error) {
	difference, err := a.Value().Sub(b.Value())
	if err != nil {
		return Amount{}, err
	}
	return difference.Cost()
}

// Value is an exact, unrounded amount in major currency units. Every Value
// stays within the range an Amount can store, so an overflow is reported by
// the operation that causes it.
type Value struct {
	amount   decimal.Decimal
	currency Currency
}

// NewValue validates the currency code and the range.
func NewValue(amount decimal.Decimal, currency Currency) (Value, error) {
	if _, err := ParseCurrency(string(currency)); err != nil {
		return Value{}, err
	}
	return boundedValue(amount, currency)
}

// ParseValue parses a decimal string such as "0.0375" in major currency units.
func ParseValue(amount string, currency Currency) (Value, error) {
	parsed, err := decimal.NewFromString(amount)
	if err != nil {
		return Value{}, fmt.Errorf("money: parse %q: %w", amount, err)
	}
	return NewValue(parsed, currency)
}

func boundedValue(amount decimal.Decimal, currency Currency) (Value, error) {
	nanos := amount.Shift(Scale)
	if nanos.GreaterThan(maxNanos) || nanos.LessThan(minNanos) {
		return Value{}, fmt.Errorf("%w: %s %s", ErrOverflow, amount.String(), currency)
	}
	return Value{amount: amount, currency: currency}, nil
}

// Currency returns the currency of v.
func (v Value) Currency() Currency {
	return v.currency
}

// String formats v as "<amount> <currency>", for example "0.0375 USD".
func (v Value) String() string {
	return v.amount.String() + " " + string(v.currency)
}

// Add returns v + other.
func (v Value) Add(other Value) (Value, error) {
	if v.currency != other.currency {
		return Value{}, fmt.Errorf("%w: %s + %s", ErrCurrencyMismatch, v.currency, other.currency)
	}
	return boundedValue(v.amount.Add(other.amount), v.currency)
}

// Sub returns v - other.
func (v Value) Sub(other Value) (Value, error) {
	if v.currency != other.currency {
		return Value{}, fmt.Errorf("%w: %s - %s", ErrCurrencyMismatch, v.currency, other.currency)
	}
	return boundedValue(v.amount.Sub(other.amount), v.currency)
}

// MulQuantity returns v multiplied by a whole count, such as an image count.
func (v Value) MulQuantity(quantity int64) (Value, error) {
	return boundedValue(v.amount.Mul(decimal.NewFromInt(quantity)), v.currency)
}

// MulUnitPrice returns quantity × unitPrice. The quantity may be fractional,
// such as millions of tokens or seconds.
func MulUnitPrice(quantity decimal.Decimal, unitPrice Value) (Value, error) {
	return boundedValue(unitPrice.amount.Mul(quantity), unitPrice.currency)
}

// Rate is an exchange rate: one unit of From buys Rate units of To.
type Rate struct {
	from Currency
	to   Currency
	rate decimal.Decimal
}

// NewRate requires two different valid currencies and a positive rate.
func NewRate(from Currency, to Currency, rate decimal.Decimal) (Rate, error) {
	if _, err := ParseCurrency(string(from)); err != nil {
		return Rate{}, err
	}
	if _, err := ParseCurrency(string(to)); err != nil {
		return Rate{}, err
	}
	if from == to {
		return Rate{}, fmt.Errorf("%w: %s to itself", ErrInvalidRate, from)
	}
	if !rate.IsPositive() {
		return Rate{}, fmt.Errorf("%w: %s %s to %s", ErrInvalidRate, rate.String(), from, to)
	}
	return Rate{from: from, to: to, rate: rate}, nil
}

// From returns the source currency of r.
func (r Rate) From() Currency {
	return r.from
}

// To returns the target currency of r.
func (r Rate) To() Currency {
	return r.to
}

// Decimal returns the exact rate: one unit of From buys Decimal units of To.
func (r Rate) Decimal() decimal.Decimal {
	return r.rate
}

// Convert returns v in the target currency of rate. v must be in the source
// currency of rate.
func (v Value) Convert(rate Rate) (Value, error) {
	if v.currency != rate.from {
		return Value{}, fmt.Errorf("%w: %s converted with a %s rate", ErrCurrencyMismatch, v.currency, rate.from)
	}
	return boundedValue(v.amount.Mul(rate.rate), rate.to)
}

// Receivable writes a charge to a user: v rounded up (toward positive
// infinity) to 10^-9, so the platform never receives less than the
// calculated value.
func (v Value) Receivable() (Amount, error) {
	return amountFromNanos(v.amount.Shift(Scale).Ceil(), v)
}

// Cost writes a purchase cost rounded to the nearest 10^-9, with halves
// rounded away from zero. Costs are reconciled against provider invoices, so
// the rounding must not drift in one direction across many entries.
func (v Value) Cost() (Amount, error) {
	return amountFromNanos(v.amount.Shift(Scale).Round(0), v)
}

// Credit writes an amount the platform credits to a user, such as a top-up
// converted from another currency: v rounded down (toward negative infinity)
// to 10^-9, so the platform never credits more than the calculated value.
func (v Value) Credit() (Amount, error) {
	return amountFromNanos(v.amount.Shift(Scale).Floor(), v)
}

func amountFromNanos(nanos decimal.Decimal, v Value) (Amount, error) {
	if nanos.GreaterThan(maxNanos) || nanos.LessThan(minNanos) {
		return Amount{}, fmt.Errorf("%w: %s", ErrOverflow, v.String())
	}
	return Amount{Nanos: nanos.IntPart(), Currency: v.currency}, nil
}
