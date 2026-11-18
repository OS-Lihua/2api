package model

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// Checkin records the actual awarded amount. Historical rows retain their USD
// denomination; migration never credits a historical check-in again.
type Checkin struct {
	Id          int          `json:"id" gorm:"primaryKey;autoIncrement"`
	UserId      int          `json:"user_id" gorm:"not null;uniqueIndex:idx_user_checkin_date"`
	CheckinDate string       `json:"checkin_date" gorm:"type:varchar(10);not null;uniqueIndex:idx_user_checkin_date"`
	Amount      money.Amount `json:"amount" gorm:"embedded;embeddedPrefix:amount_"`
	CreatedAt   int64        `json:"created_at" gorm:"bigint;autoCreateTime:false"`
}

// CheckinRecord excludes internal row and user IDs from the calendar API.
type CheckinRecord struct {
	CheckinDate string       `json:"checkin_date"`
	Amount      money.Amount `json:"amount"`
}

// CheckinAwardTotal may exceed the int64 storage range of an individual award.
type CheckinAwardTotal = MoneyTotal

func (Checkin) TableName() string { return "checkins" }

func GetUserCheckinRecords(userId int, startDate, endDate string) ([]Checkin, error) {
	records := make([]Checkin, 0)
	err := DB.Where("user_id = ? AND checkin_date >= ? AND checkin_date <= ?", userId, startDate, endDate).
		Order("checkin_date DESC").Find(&records).Error
	return records, err
}

func HasCheckedInToday(userId int) (bool, error) {
	var count int64
	err := DB.Model(&Checkin{}).Where("user_id = ? AND checkin_date = ?", userId, clock.Now().Format("2006-01-02")).Count(&count).Error
	return count > 0, err
}

// checkinReward samples inclusive bounds without overflowing max-min+1.
func checkinReward(rewardRange operation_setting.CheckinRewardRange, reader io.Reader) (money.Amount, error) {
	if err := rewardRange.Validate(); err != nil {
		return money.Amount{}, err
	}
	width := new(big.Int).Sub(big.NewInt(rewardRange.Max.Nanos), big.NewInt(rewardRange.Min.Nanos))
	width.Add(width, big.NewInt(1))
	offset, err := rand.Int(reader, width)
	if err != nil {
		return money.Amount{}, err
	}
	offset.Add(offset, big.NewInt(rewardRange.Min.Nanos))
	return money.NewAmount(offset.Int64(), rewardRange.Min.Currency)
}

// UserCheckin commits the date, credit and automatic debt repayment together.
func UserCheckin(userId int) (*Checkin, error) {
	setting := operation_setting.GetCheckinSetting()
	if !setting.Enabled {
		return nil, common.NewMessage("Check-in feature is not enabled")
	}
	if userId <= 0 {
		return nil, common.NewMessage("Invalid parameters")
	}
	now := clock.Now()
	source, err := checkinReward(setting.RewardRange, rand.Reader)
	if err != nil {
		return nil, err
	}
	checkin := &Checkin{
		UserId: userId, CheckinDate: now.Format("2006-01-02"), CreatedAt: now.Unix(),
		Amount: money.Amount{Currency: SettlementCurrency()},
	}
	err = DB.Transaction(func(tx *gorm.DB) error {
		var user User
		if err := tx.Select("id").First(&user, userId).Error; err != nil {
			return err
		}
		// The unique user/date index serializes competing check-ins. A later
		// posting failure rolls this row back with the wallet and ledger writes.
		if err := tx.Create(checkin).Error; err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "idx_user_checkin_date" {
				return common.NewMessage("Already checked in today")
			}
			return err
		}
		if source.Nanos == 0 {
			return nil
		}
		credited := source
		var fx decimal.NullDecimal
		if source.Currency != SettlementCurrency() {
			rate, err := GetAccountingFXRate(tx.Statement.Context, tx, source.Currency, SettlementCurrency())
			if err != nil {
				return err
			}
			value, err := source.Value().Convert(rate)
			if err != nil {
				return err
			}
			credited, err = value.Credit()
			if err != nil {
				return err
			}
			fx = decimal.NewNullDecimal(rate.Decimal())
		}
		// A positive promised reward must not become a successful zero credit.
		if credited.Nanos == 0 {
			return common.NewMessage("Invalid amount")
		}
		posting := LedgerPosting{
			Account: UserWalletAccount(userId), Type: LedgerEntryTopUp, Amount: credited,
			IdempotencyKey: fmt.Sprintf("checkin:%d:%s", userId, checkin.CheckinDate),
			Remark:         "Daily check-in", sourceAmount: &source, fxRate: fx,
		}
		if err := validateLedgerPosting(posting); err != nil {
			return err
		}
		if _, err := postLedgerEntries(tx, []LedgerPosting{posting}); err != nil {
			return err
		}
		checkin.Amount = credited
		return tx.Model(checkin).Updates(map[string]any{"amount_nanos": credited.Nanos, "amount_currency": credited.Currency}).Error
	})
	if err != nil {
		var message *common.Message
		if errors.As(err, &message) {
			return nil, message
		}
		return nil, common.NewMessage("Check-in failed, please try again later")
	}
	return checkin, nil
}

func GetUserCheckinStats(userId int, month string) (map[string]any, error) {
	records, err := GetUserCheckinRecords(userId, month+"-01", month+"-31")
	if err != nil {
		return nil, err
	}
	checkinRecords := make([]CheckinRecord, len(records))
	for i, record := range records {
		checkinRecords[i] = CheckinRecord{CheckinDate: record.CheckinDate, Amount: record.Amount}
	}
	hasCheckedToday, err := HasCheckedInToday(userId)
	if err != nil {
		return nil, err
	}
	var totalCheckins int64
	if err := DB.Model(&Checkin{}).Where("user_id = ?", userId).Count(&totalCheckins).Error; err != nil {
		return nil, err
	}
	var grouped []struct {
		Currency money.Currency
		Nanos    decimal.Decimal
	}
	if err := DB.Model(&Checkin{}).Where("user_id = ?", userId).
		Select("amount_currency AS currency, SUM(amount_nanos) AS nanos").Group("amount_currency").Order("amount_currency").Scan(&grouped).Error; err != nil {
		return nil, err
	}
	totals := make([]CheckinAwardTotal, len(grouped))
	for i, total := range grouped {
		totals[i] = CheckinAwardTotal{Nanos: total.Nanos.String(), Currency: total.Currency}
	}
	return map[string]any{
		"total_awarded": totals, "total_checkins": totalCheckins, "checkin_count": len(records),
		"checked_in_today": hasCheckedToday, "records": checkinRecords,
	}, nil
}
