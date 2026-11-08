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
	"math/big"

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
	maxNanos  = big.NewInt(math.MaxInt64)
	minNanos  = big.NewInt(math.MinInt64)
	nanoScale = big.NewRat(1_000_000_000, 1)
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
	return Value{amount: big.NewRat(a.Nanos, 1_000_000_000), currency: a.Currency}
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

// Value is an exact, unrounded amount in major currency units. Rational values
// preserve divisions such as 1/3 until an Amount is written. Every Value stays
// within the range an Amount can store, so the operation causing an overflow
// reports it. Values are immutable and can be copied safely.
type Value struct {
	amount   *big.Rat
	currency Currency
}

// NewValue validates the currency code and the range.
func NewValue(amount decimal.Decimal, currency Currency) (Value, error) {
	return NewRationalValue(amount.Rat(), currency)
}

// NewRationalValue keeps an exact quotient without decimal division or
// intermediate rounding. It copies amount and validates the currency and range.
func NewRationalValue(amount *big.Rat, currency Currency) (Value, error) {
	if _, err := ParseCurrency(string(currency)); err != nil {
		return Value{}, err
	}
	if amount == nil {
		return Value{}, fmt.Errorf("money: nil rational amount")
	}
	return boundedValue(new(big.Rat).Set(amount), currency)
}

// ParseValue parses a decimal string such as "0.0375" in major currency units.
func ParseValue(amount string, currency Currency) (Value, error) {
	parsed, err := decimal.NewFromString(amount)
	if err != nil {
		return Value{}, fmt.Errorf("money: parse %q: %w", amount, err)
	}
	return NewValue(parsed, currency)
}

func boundedValue(amount *big.Rat, currency Currency) (Value, error) {
	nanos := new(big.Rat).Mul(amount, nanoScale)
	if nanos.Cmp(new(big.Rat).SetInt(maxNanos)) > 0 || nanos.Cmp(new(big.Rat).SetInt(minNanos)) < 0 {
		return Value{}, fmt.Errorf("%w: %s %s", ErrOverflow, amount.RatString(), currency)
	}
	return Value{amount: amount, currency: currency}, nil
}

func (v Value) rational() *big.Rat {
	if v.amount == nil {
		return new(big.Rat)
	}
	return v.amount
}

// Currency returns the currency of v.
func (v Value) Currency() Currency {
	return v.currency
}

// String formats finite decimals as "0.0375 USD" and repeating decimals as
// an exact fraction, for example "1/3 USD". It never rounds the amount.
func (v Value) String() string {
	amount := v.rational()
	if places, finite := amount.FloatPrec(); finite {
		return amount.FloatString(places) + " " + string(v.currency)
	}
	return amount.RatString() + " " + string(v.currency)
}

// Compare orders two unrounded amounts without rounding or currency coercion.
func (v Value) Compare(other Value) (int, error) {
	if v.currency != other.currency {
		return 0, ErrCurrencyMismatch
	}
	return v.rational().Cmp(other.rational()), nil
}

// Add returns v + other.
func (v Value) Add(other Value) (Value, error) {
	if v.currency != other.currency {
		return Value{}, fmt.Errorf("%w: %s + %s", ErrCurrencyMismatch, v.currency, other.currency)
	}
	return boundedValue(new(big.Rat).Add(v.rational(), other.rational()), v.currency)
}

// Sub returns v - other.
func (v Value) Sub(other Value) (Value, error) {
	if v.currency != other.currency {
		return Value{}, fmt.Errorf("%w: %s - %s", ErrCurrencyMismatch, v.currency, other.currency)
	}
	return boundedValue(new(big.Rat).Sub(v.rational(), other.rational()), v.currency)
}

// MulQuantity returns v multiplied by a whole count, such as an image count.
func (v Value) MulQuantity(quantity int64) (Value, error) {
	return boundedValue(new(big.Rat).Mul(v.rational(), big.NewRat(quantity, 1)), v.currency)
}

// MulUnitPrice returns quantity × unitPrice. The quantity may be fractional,
// such as millions of tokens or seconds.
func MulUnitPrice(quantity decimal.Decimal, unitPrice Value) (Value, error) {
	return boundedValue(new(big.Rat).Mul(unitPrice.rational(), quantity.Rat()), unitPrice.currency)
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
	return boundedValue(new(big.Rat).Mul(v.rational(), rate.rate.Rat()), rate.to)
}

// Receivable writes a charge to a user: v rounded up (toward positive
// infinity) to 10^-9, so the platform never receives less than the
// calculated value.
func (v Value) Receivable() (Amount, error) {
	return v.writeNanos(1)
}

// Cost writes a purchase cost rounded to the nearest 10^-9, with halves
// rounded away from zero. Costs are reconciled against provider invoices, so
// the rounding must not drift in one direction across many entries.
func (v Value) Cost() (Amount, error) {
	return v.writeNanos(0)
}

// Credit writes an amount the platform credits to a user, such as a top-up
// converted from another currency: v rounded down (toward negative infinity)
// to 10^-9, so the platform never credits more than the calculated value.
func (v Value) Credit() (Amount, error) {
	return v.writeNanos(-1)
}

// direction selects ceiling (+1), nearest half away from zero (0), or floor (-1).
func (v Value) writeNanos(direction int) (Amount, error) {
	scaled := new(big.Rat).Mul(v.rational(), nanoScale)
	nanos, remainder := new(big.Int), new(big.Int)
	nanos.QuoRem(scaled.Num(), scaled.Denom(), remainder)
	if remainder.Sign() != 0 {
		switch {
		case direction > 0 && remainder.Sign() > 0:
			nanos.Add(nanos, big.NewInt(1))
		case direction < 0 && remainder.Sign() < 0:
			nanos.Sub(nanos, big.NewInt(1))
		case direction == 0:
			twiceRemainder := new(big.Int).Lsh(new(big.Int).Abs(remainder), 1)
			if twiceRemainder.Cmp(scaled.Denom()) >= 0 {
				nanos.Add(nanos, big.NewInt(int64(remainder.Sign())))
			}
		}
	}
	if nanos.Cmp(maxNanos) > 0 || nanos.Cmp(minNanos) < 0 {
		return Amount{}, fmt.Errorf("%w: %s", ErrOverflow, v.String())
	}
	return Amount{Nanos: nanos.Int64(), Currency: v.currency}, nil
}
