package controller

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func pricingError(c *gin.Context, err error) {
	var marginError *model.MarginConfirmationError
	if errors.As(err, &marginError) {
		c.JSON(http.StatusConflict, gin.H{"success": false, "message": "Negative margin requires confirmation", "message_key": "Negative margin requires confirmation", "data": marginError.Report})
		return
	}
	status, message := http.StatusInternalServerError, common.NewMessage("Failed to access pricing")
	var validation *common.Message
	var databaseError *pgconn.PgError
	switch {
	case errors.Is(err, money.ErrOverflow):
		status, message = http.StatusBadRequest, common.NewMessage("The amount is outside the supported range")
	case errors.As(err, &validation):
		status, message = http.StatusBadRequest, validation
	case errors.Is(err, gorm.ErrRecordNotFound):
		status, message = http.StatusNotFound, common.NewMessage("Price owner, version, or exchange rate not found")
	case errors.As(err, &databaseError):
		switch databaseError.Code {
		case "23505":
			status, message = http.StatusConflict, common.NewMessage("A price version already exists at this effective time")
		case "23503", "23001":
			status, message = http.StatusConflict, common.NewMessage("Pricing references an unavailable record")
		}
	}
	common.ApiErrorStatus(c, status, message)
}

func ListPriceVersions(c *gin.Context) {
	query := model.DB.WithContext(c.Request.Context()).Model(&model.PriceVersion{})
	if owner := c.Query("owner_type"); c.Request.URL.Query().Has("owner_type") {
		if owner != model.PriceOwnerOffering && owner != model.PriceOwnerChannel {
			pricingError(c, common.NewMessage("Invalid price owner"))
			return
		}
		query = query.Where("owner_type = ?", owner)
	}
	if value := c.Query("owner_id"); c.Request.URL.Query().Has("owner_id") {
		id, err := strconv.Atoi(value)
		if err != nil || id <= 0 {
			pricingError(c, common.NewMessage("Invalid price owner"))
			return
		}
		query = query.Where("owner_id = ?", id)
	}
	page := common.GetPageQuery(c)
	var total int64
	if err := query.Count(&total).Error; err != nil {
		pricingError(c, err)
		return
	}
	records := []model.PriceVersion{}
	if err := query.Order("effective_at DESC, id DESC").Offset(page.GetStartIdx()).Limit(page.GetPageSize()).Find(&records).Error; err != nil {
		pricingError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"items": records, "total": total, "page": page.GetPage(), "page_size": page.GetPageSize()})
}

func GetPriceVersion(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		pricingError(c, common.NewMessage("Invalid price version ID"))
		return
	}
	var record model.PriceVersion
	if err := model.DB.WithContext(c.Request.Context()).First(&record, id).Error; err != nil {
		pricingError(c, err)
		return
	}
	common.ApiSuccess(c, record)
}

func GetCurrentPriceVersion(c *gin.Context) {
	owner, idText := c.Query("owner_type"), c.Query("owner_id")
	id, err := strconv.Atoi(idText)
	if err != nil || id <= 0 || (owner != model.PriceOwnerOffering && owner != model.PriceOwnerChannel) {
		pricingError(c, common.NewMessage("Invalid price owner"))
		return
	}
	at := clock.Now().Unix()
	if text := c.Query("at"); c.Request.URL.Query().Has("at") {
		at, err = strconv.ParseInt(text, 10, 64)
		if err != nil || at <= 0 || at > model.MaxPriceEffectiveAt {
			pricingError(c, common.NewMessage("Invalid price effective time"))
			return
		}
	}
	record, err := model.CurrentPriceVersion(c.Request.Context(), model.DB, owner, id, at)
	if err != nil {
		pricingError(c, err)
		return
	}
	common.ApiSuccess(c, record)
}

func CreatePriceVersion(c *gin.Context) {
	var request struct {
		MarginConfirmation string         `json:"margin_confirmation"`
		OwnerType          string         `json:"owner_type"`
		OwnerID            int            `json:"owner_id"`
		Currency           money.Currency `json:"currency"`
		Expression         string         `json:"expression"`
		EffectiveAt        *int64         `json:"effective_at"`
	}
	if err := common.DecodeJson(c.Request.Body, &request); err != nil {
		pricingError(c, common.NewMessage("Invalid parameters"))
		return
	}
	record := model.PriceVersion{
		MarginConfirmation: request.MarginConfirmation,
		OwnerType:          request.OwnerType, OwnerID: request.OwnerID,
		Currency: request.Currency, Expression: request.Expression,
		EffectiveAt: clock.Now().Unix(),
	}
	// Omission and null mean immediate at server time. Explicit zero remains
	// supplied input and must fail the model's scheduled-time validation.
	if request.EffectiveAt != nil {
		record.EffectiveAt = *request.EffectiveAt
	}
	if err := model.CreatePriceVersion(c.Request.Context(), &record, c.GetInt("id")); err != nil {
		pricingError(c, err)
		return
	}
	report := record.MarginCheck
	record.MarginCheck = nil
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "", "data": record, "margin_check": report})
}

func ListFXRates(c *gin.Context) {
	page := common.GetPageQuery(c)
	query := model.DB.WithContext(c.Request.Context()).Model(&model.FXRate{})
	var total int64
	if err := query.Count(&total).Error; err != nil {
		pricingError(c, err)
		return
	}
	records := []model.FXRate{}
	if err := query.Order("from_currency, to_currency").Offset(page.GetStartIdx()).Limit(page.GetPageSize()).Find(&records).Error; err != nil {
		pricingError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"items": records, "total": total, "page": page.GetPage(), "page_size": page.GetPageSize()})
}

func SaveFXRate(c *gin.Context) {
	var request struct {
		MarginConfirmation string         `json:"margin_confirmation"`
		FromCurrency       money.Currency `json:"from_currency"`
		ToCurrency         money.Currency `json:"to_currency"`
		Rate               string         `json:"rate"`
		BufferRate         *string        `json:"buffer_rate"`
	}
	if err := common.DecodeJson(c.Request.Body, &request); err != nil {
		pricingError(c, common.NewMessage("Invalid parameters"))
		return
	}
	buffer := "0.02"
	if request.BufferRate != nil {
		buffer = *request.BufferRate
		if buffer == "" {
			pricingError(c, common.NewMessage("Invalid exchange rate or buffer"))
			return
		}
	}
	record, err := model.SaveFXRate(c.Request.Context(), request.FromCurrency, request.ToCurrency, request.Rate, buffer, c.GetInt("id"), request.MarginConfirmation)
	if err != nil {
		pricingError(c, err)
		return
	}
	report := record.MarginCheck
	record.MarginCheck = nil
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "", "data": record, "margin_check": report})
}

func GetFXQuote(c *gin.Context) {
	from, fromErr := money.ParseCurrency(c.Query("from_currency"))
	to, toErr := money.ParseCurrency(c.Query("to_currency"))
	if fromErr != nil || toErr != nil || from == to {
		pricingError(c, common.NewMessage("Invalid exchange rate or buffer"))
		return
	}
	var record model.FXRate
	if err := model.DB.WithContext(c.Request.Context()).Where("from_currency = ? AND to_currency = ?", from, to).First(&record).Error; err != nil {
		pricingError(c, err)
		return
	}
	comparison, err := money.NewRate(from, to, record.Rate.Mul(decimal.NewFromInt(1).Add(record.BufferRate)))
	if err != nil {
		pricingError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"from_currency": from, "to_currency": to, "raw_rate": record.Rate.String(), "accounting_rate": record.Rate.String(), "comparison_rate": comparison.Decimal().String(), "buffer_rate": record.BufferRate.String()})
}
