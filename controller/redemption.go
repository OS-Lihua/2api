package controller

import (
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/gin-gonic/gin"
)

func GetAllRedemptions(c *gin.Context) {
	pageInfo := common.GetPageQuery(c)
	redemptions, total, err := model.GetAllRedemptions(pageInfo.GetStartIdx(), pageInfo.GetPageSize())
	if err != nil {
		common.ApiError(c, err)
		return
	}
	pageInfo.SetTotal(int(total))
	pageInfo.SetItems(redemptions)
	common.ApiSuccess(c, pageInfo)
	return
}

func SearchRedemptions(c *gin.Context) {
	keyword := c.Query("keyword")
	status := c.Query("status")
	pageInfo := common.GetPageQuery(c)
	redemptions, total, err := model.SearchRedemptions(keyword, status, pageInfo.GetStartIdx(), pageInfo.GetPageSize())
	if err != nil {
		common.ApiError(c, err)
		return
	}
	pageInfo.SetTotal(int(total))
	pageInfo.SetItems(redemptions)
	common.ApiSuccess(c, pageInfo)
	return
}

func GetRedemption(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		common.ApiError(c, err)
		return
	}
	redemption, err := model.GetRedemptionById(id)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    redemption,
	})
	return
}

func AddRedemption(c *gin.Context) {
	if !operation_setting.IsPaymentComplianceConfirmed() {
		common.ApiErrorT(c, "Payment, redemption, subscription, and invitation reward features are disabled. The administrator must confirm compliance terms before enabling them.")
		return
	}

	var redemption struct {
		Name        string       `json:"name"`
		Count       int          `json:"count"`
		Amount      money.Amount `json:"amount"`
		ExpiredTime int64        `json:"expired_time"`
	}
	if err := common.DecodeJsonStrict(c.Request.Body, &redemption); err != nil {
		common.ApiErrorT(c, "Invalid parameters")
		return
	}
	if utf8.RuneCountInString(redemption.Name) == 0 || utf8.RuneCountInString(redemption.Name) > 20 {
		common.ApiErrorT(c, "Redemption code name length must be between 1-20")
		return
	}
	if redemption.Count <= 0 {
		common.ApiErrorT(c, "Redemption code count must be greater than 0")
		return
	}
	if redemption.Count > 100 {
		common.ApiErrorT(c, "Maximum 100 redemption codes can be generated at once")
		return
	}
	if _, err := money.NewAmount(redemption.Amount.Nanos, redemption.Amount.Currency); err != nil || redemption.Amount.Nanos <= 0 {
		common.ApiError(c, model.ErrInvalidRedemptionAmount)
		return
	}
	if err := validateExpiredTime(redemption.ExpiredTime); err != nil {
		common.ApiError(c, err)
		return
	}
	var keys []string
	for i := 0; i < redemption.Count; i++ {
		key := common.GetUUID()
		cleanRedemption := model.Redemption{
			UserId:      c.GetInt("id"),
			Name:        redemption.Name,
			Key:         key,
			CreatedTime: common.GetTimestamp(),
			Amount:      redemption.Amount,
			Status:      common.RedemptionCodeStatusEnabled,
			ExpiredTime: redemption.ExpiredTime,
		}
		if err := cleanRedemption.Insert(); err != nil {
			common.SysError(common.LogText("Failed to create redemption code"))
			common.ApiErrorStatus(c, http.StatusOK, common.NewMessage("Failed to create redemption code, please try again later"), gin.H{"data": keys})
			return
		}
		keys = append(keys, key)
	}
	recordManageAudit(c, "redemption.create", map[string]any{
		"name":   redemption.Name,
		"count":  redemption.Count,
		"amount": redemption.Amount,
	})
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    keys,
	})
	return
}

func DeleteRedemption(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	err := model.DeleteRedemptionById(id)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
	return
}

func UpdateRedemption(c *gin.Context) {
	var cleanRedemption *model.Redemption
	var err error
	if c.Query("status_only") != "" {
		var request struct {
			Id     int `json:"id"`
			Status int `json:"status"`
		}
		if common.DecodeJsonStrict(c.Request.Body, &request) != nil || request.Id <= 0 ||
			(request.Status != common.RedemptionCodeStatusEnabled && request.Status != common.RedemptionCodeStatusDisabled) {
			common.ApiErrorT(c, "Invalid parameters")
			return
		}
		cleanRedemption, err = model.GetRedemptionById(request.Id)
		if err == nil {
			cleanRedemption.Status = request.Status
			err = cleanRedemption.SelectUpdate()
		}
	} else {
		var request struct {
			Id          int          `json:"id"`
			Name        string       `json:"name"`
			Amount      money.Amount `json:"amount"`
			ExpiredTime int64        `json:"expired_time"`
		}
		if common.DecodeJsonStrict(c.Request.Body, &request) != nil || request.Id <= 0 {
			common.ApiErrorT(c, "Invalid parameters")
			return
		}
		if utf8.RuneCountInString(request.Name) == 0 || utf8.RuneCountInString(request.Name) > 20 {
			common.ApiErrorT(c, "Redemption code name length must be between 1-20")
			return
		}
		if _, err := money.NewAmount(request.Amount.Nanos, request.Amount.Currency); err != nil || request.Amount.Nanos <= 0 {
			common.ApiError(c, model.ErrInvalidRedemptionAmount)
			return
		}
		if err := validateExpiredTime(request.ExpiredTime); err != nil {
			common.ApiError(c, err)
			return
		}
		cleanRedemption, err = model.GetRedemptionById(request.Id)
		if err == nil {
			cleanRedemption.Name = request.Name
			cleanRedemption.Amount = request.Amount
			cleanRedemption.ExpiredTime = request.ExpiredTime
			err = cleanRedemption.Update()
		}
	}
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    cleanRedemption,
	})
	return
}

func DeleteInvalidRedemption(c *gin.Context) {
	rows, err := model.DeleteInvalidRedemptions()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    rows,
	})
	return
}

func validateExpiredTime(expired int64) error {
	if expired != 0 && expired < common.GetTimestamp() {
		return common.NewMessage("Expiration time cannot be earlier than current time")
	}
	return nil
}

func DeleteRedemptionBatch(c *gin.Context) {
	var request struct {
		Ids []int `json:"ids" binding:"required,min=1,max=1000,dive,gt=0"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		common.ApiErrorT(c, "Invalid parameters")
		return
	}
	count, err := model.BatchDeleteRedemptions(request.Ids)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	recordManageAudit(c, "redemption.delete_batch", map[string]any{
		"count":                    count,
		"total":                    len(request.Ids),
		"requested_redemption_ids": request.Ids,
	})
	common.ApiSuccess(c, count)
}
