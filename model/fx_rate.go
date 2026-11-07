package model

import (
	"context"
	"regexp"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// FXRate stores a directional quote: one unit of FromCurrency buys Rate units
// of ToCurrency. A quote never implies an inverse or a path through other pairs.
type FXRate struct {
	FromCurrency money.Currency  `json:"from_currency" gorm:"type:char(3);primaryKey;check:fx_from_valid,from_currency ~ '^[A-Z]{3}$'"`
	ToCurrency   money.Currency  `json:"to_currency" gorm:"type:char(3);primaryKey;check:fx_to_valid,to_currency ~ '^[A-Z]{3}$' AND to_currency <> from_currency"`
	Rate         decimal.Decimal `json:"rate" gorm:"type:numeric(30,18);not null;check:fx_rate_positive,rate > 0 AND rate <= 1000000000"`
	BufferRate   decimal.Decimal `json:"buffer_rate" gorm:"type:numeric(30,18);not null;check:fx_buffer_valid,buffer_rate >= 0 AND buffer_rate <= 1"`
	UpdatedBy    int             `json:"updated_by" gorm:"not null"`
	UpdatedAt    int64           `json:"updated_at" gorm:"not null;autoUpdateTime:false"`
}

var fxDecimal = regexp.MustCompile(`^(0|[1-9][0-9]{0,9})(\.[0-9]{1,18})?$`)

func SaveFXRate(ctx context.Context, from, to money.Currency, rateText, bufferText string, actorID int) (*FXRate, error) {
	if bufferText == "" {
		bufferText = "0.02"
	}
	if !fxDecimal.MatchString(rateText) || !fxDecimal.MatchString(bufferText) {
		return nil, common.NewMessage("Invalid exchange rate or buffer")
	}
	rate, err := decimal.NewFromString(rateText)
	if err != nil {
		return nil, common.NewMessage("Invalid exchange rate or buffer")
	}
	buffer, err := decimal.NewFromString(bufferText)
	if err != nil || buffer.IsNegative() || buffer.GreaterThan(decimal.NewFromInt(1)) || rate.GreaterThan(decimal.NewFromInt(1000000000)) {
		return nil, common.NewMessage("Invalid exchange rate or buffer")
	}
	if _, err := money.NewRate(from, to, rate); err != nil {
		return nil, common.NewMessage("Invalid exchange rate or buffer")
	}
	record := &FXRate{FromCurrency: from, ToCurrency: to, Rate: rate, BufferRate: buffer, UpdatedBy: actorID, UpdatedAt: clock.Now().Unix()}
	err = DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "from_currency"}, {Name: "to_currency"}},
		DoUpdates: clause.AssignmentColumns([]string{"rate", "buffer_rate", "updated_by", "updated_at"}),
	}).Create(record).Error
	return record, err
}

// GetAccountingFXRate returns the administrator's raw quote for actual posting.
func GetAccountingFXRate(ctx context.Context, db *gorm.DB, from, to money.Currency) (money.Rate, error) {
	var quote FXRate
	if err := db.WithContext(ctx).Where("from_currency = ? AND to_currency = ?", from, to).First(&quote).Error; err != nil {
		return money.Rate{}, err
	}
	return money.NewRate(from, to, quote.Rate)
}

// GetComparisonFXRate adds the configured buffer for routing and margin checks.
func GetComparisonFXRate(ctx context.Context, db *gorm.DB, from, to money.Currency) (money.Rate, error) {
	var quote FXRate
	if err := db.WithContext(ctx).Where("from_currency = ? AND to_currency = ?", from, to).First(&quote).Error; err != nil {
		return money.Rate{}, err
	}
	return money.NewRate(from, to, quote.Rate.Mul(decimal.NewFromInt(1).Add(quote.BufferRate)))
}
