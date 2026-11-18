package model

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/shopspring/decimal"

	"gorm.io/gorm"
)

type Redemption struct {
	Id           int            `json:"id"`
	UserId       int            `json:"user_id"`
	Key          string         `json:"key" gorm:"type:char(32);uniqueIndex"`
	Status       int            `json:"status" gorm:"default:1"`
	Name         string         `json:"name" gorm:"index"`
	Amount       money.Amount   `json:"amount" gorm:"embedded;embeddedPrefix:amount_"`
	CreatedTime  int64          `json:"created_time" gorm:"bigint"`
	RedeemedTime int64          `json:"redeemed_time" gorm:"bigint"`
	Count        int            `json:"count" gorm:"-:all"` // only for api request
	UsedUserId   int            `json:"used_user_id"`
	DeletedAt    gorm.DeletedAt `gorm:"index"`
	ExpiredTime  int64          `json:"expired_time" gorm:"bigint"` // 过期时间，0 表示不过期
}

func GetAllRedemptions(startIdx int, num int) (redemptions []*Redemption, total int64, err error) {
	// 开始事务
	tx := DB.Begin()
	if tx.Error != nil {
		return nil, 0, tx.Error
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	// 获取总数
	err = tx.Model(&Redemption{}).Count(&total).Error
	if err != nil {
		tx.Rollback()
		return nil, 0, err
	}

	// 获取分页数据
	err = tx.Order("id desc").Limit(num).Offset(startIdx).Find(&redemptions).Error
	if err != nil {
		tx.Rollback()
		return nil, 0, err
	}

	// 提交事务
	if err = tx.Commit().Error; err != nil {
		return nil, 0, err
	}

	return redemptions, total, nil
}

func SearchRedemptions(keyword string, status string, startIdx int, num int) (redemptions []*Redemption, total int64, err error) {
	tx := DB.Begin()
	if tx.Error != nil {
		return nil, 0, tx.Error
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	query := tx.Model(&Redemption{})

	if keyword != "" {
		if id, err := strconv.Atoi(keyword); err == nil {
			query = query.Where("id = ? OR name LIKE ?", id, keyword+"%")
		} else {
			query = query.Where("name LIKE ?", keyword+"%")
		}
	}

	if status != "" {
		now := common.GetTimestamp()
		switch status {
		case "expired":
			query = query.Where(
				"status = ? AND expired_time != 0 AND expired_time < ?",
				common.RedemptionCodeStatusEnabled,
				now,
			)
		case strconv.Itoa(common.RedemptionCodeStatusEnabled):
			query = query.Where(
				"status = ? AND (expired_time = 0 OR expired_time >= ?)",
				common.RedemptionCodeStatusEnabled,
				now,
			)
		case strconv.Itoa(common.RedemptionCodeStatusDisabled):
			query = query.Where("status = ?", common.RedemptionCodeStatusDisabled)
		case strconv.Itoa(common.RedemptionCodeStatusUsed):
			query = query.Where("status = ?", common.RedemptionCodeStatusUsed)
		}
	}

	// Get total count
	err = query.Count(&total).Error
	if err != nil {
		tx.Rollback()
		return nil, 0, err
	}

	// Get paginated data
	err = query.Order("id desc").Limit(num).Offset(startIdx).Find(&redemptions).Error
	if err != nil {
		tx.Rollback()
		return nil, 0, err
	}

	if err = tx.Commit().Error; err != nil {
		return nil, 0, err
	}

	return redemptions, total, nil
}

func GetRedemptionById(id int) (*Redemption, error) {
	if id == 0 {
		return nil, common.NewMessage("ID is empty")
	}
	redemption := Redemption{Id: id}
	var err error = nil
	err = DB.First(&redemption, "id = ?", id).Error
	return &redemption, err
}

var ErrInvalidRedemptionAmount error = common.NewMessage("Invalid redemption amount")

func validateRedemptionAmount(amount money.Amount) error {
	if amount.Nanos <= 0 {
		return ErrInvalidRedemptionAmount
	}
	if _, err := money.ParseCurrency(string(amount.Currency)); err != nil {
		return ErrInvalidRedemptionAmount
	}
	return nil
}

// Redeem consumes a voucher and credits its original denomination at the current
// accounting FX. The status change, credit and debt repayment commit together.
func Redeem(key string, userId int) (money.Amount, error) {
	if key == "" || userId <= 0 {
		return money.Amount{}, ErrRedeemFailed
	}
	redemption := &Redemption{}
	var credited money.Amount
	common.RandomSleep()
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).Where(`"key" = ?`, key).First(redemption).Error; err != nil {
			return ErrRedeemFailed
		}
		if redemption.Status != common.RedemptionCodeStatusEnabled || redemption.RedeemedTime != 0 || redemption.UsedUserId != 0 {
			return ErrRedeemFailed
		}
		if redemption.ExpiredTime != 0 && redemption.ExpiredTime < common.GetTimestamp() {
			return ErrRedeemFailed
		}
		if err := validateRedemptionAmount(redemption.Amount); err != nil {
			return err
		}
		result := tx.Model(&Redemption{}).
			Where("id = ? AND status = ?", redemption.Id, common.RedemptionCodeStatusEnabled).
			Updates(map[string]any{
				"redeemed_time": common.GetTimestamp(),
				"status":        common.RedemptionCodeStatusUsed,
				"used_user_id":  userId,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRedeemFailed
		}
		var user User
		if err := tx.Select("id").First(&user, userId).Error; err != nil {
			return err
		}
		credited = redemption.Amount
		var fx decimal.NullDecimal
		if redemption.Amount.Currency != SettlementCurrency() {
			rate, err := GetAccountingFXRate(tx.Statement.Context, tx, redemption.Amount.Currency, SettlementCurrency())
			if err != nil {
				return err
			}
			value, err := redemption.Amount.Value().Convert(rate)
			if err != nil {
				return err
			}
			credited, err = value.Credit()
			if err != nil {
				return err
			}
			fx = decimal.NewNullDecimal(rate.Decimal())
		}
		posting := LedgerPosting{
			Account: UserWalletAccount(userId), Type: LedgerEntryTopUp, Amount: credited,
			IdempotencyKey: fmt.Sprintf("top_up:redemption:%d", redemption.Id),
			Remark:         "Redemption code redeemed", sourceAmount: &redemption.Amount, fxRate: fx,
		}
		if err := validateLedgerPosting(posting); err != nil {
			return err
		}
		_, err := postLedgerEntries(tx, []LedgerPosting{posting})
		return err
	})
	if err != nil {
		// Never log the voucher credential or database error details that could
		// contain it. Clients receive the same failure for every rejection.
		return money.Amount{}, ErrRedeemFailed
	}
	RecordLog(userId, LogTypeTopup, common.NewMessage("Redeemed a redemption code, code ID {{id}}", map[string]any{"id": redemption.Id}))
	return credited, nil
}

func (redemption *Redemption) Insert() error {
	if err := validateRedemptionAmount(redemption.Amount); err != nil {
		return err
	}
	var err error
	err = DB.Create(redemption).Error
	return err
}

func (redemption *Redemption) SelectUpdate() error {
	if redemption.Status != common.RedemptionCodeStatusEnabled && redemption.Status != common.RedemptionCodeStatusDisabled {
		return common.NewMessage("Redemption failed")
	}
	result := DB.Model(redemption).
		Where("status IN ? AND COALESCE(redeemed_time, 0) = 0 AND COALESCE(used_user_id, 0) = 0", []int{common.RedemptionCodeStatusEnabled, common.RedemptionCodeStatusDisabled}).
		Update("status", redemption.Status)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return common.NewMessage("Redemption failed")
	}
	return nil
}

// Update edits voucher terms without changing its redemption history or status.
func (redemption *Redemption) Update() error {
	if err := validateRedemptionAmount(redemption.Amount); err != nil {
		return err
	}
	var err error
	err = DB.Model(redemption).Select("name", "amount_nanos", "amount_currency", "expired_time").Updates(redemption).Error
	return err
}

func (redemption *Redemption) Delete() error {
	var err error
	err = DB.Delete(redemption).Error
	return err
}

func DeleteRedemptionById(id int) (err error) {
	if id == 0 {
		return common.NewMessage("ID is empty")
	}
	redemption := Redemption{Id: id}
	err = DB.Where(redemption).First(&redemption).Error
	if err != nil {
		return err
	}
	return redemption.Delete()
}

func DeleteInvalidRedemptions() (int64, error) {
	now := common.GetTimestamp()
	result := DB.Where("status IN ? OR (status = ? AND expired_time != 0 AND expired_time < ?)", []int{common.RedemptionCodeStatusUsed, common.RedemptionCodeStatusDisabled}, common.RedemptionCodeStatusEnabled, now).Delete(&Redemption{})
	return result.RowsAffected, result.Error
}

// BatchDeleteRedemptions soft-deletes the selected codes in one statement.
func BatchDeleteRedemptions(ids []int) (int64, error) {
	if len(ids) == 0 || len(ids) > 1000 {
		return 0, errors.New("select between 1 and 1000 redemption codes")
	}
	for _, id := range ids {
		if id <= 0 {
			return 0, errors.New("redemption IDs must be positive")
		}
	}
	result := DB.Where("id IN ?", ids).Delete(&Redemption{})
	return result.RowsAffected, result.Error
}
