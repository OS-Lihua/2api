package controller

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func ListMarginEstimates(c *gin.Context) {
	var report *model.PriceMarginReport
	err := model.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		var err error
		report, err = model.CheckPriceMargins(c.Request.Context(), tx, nil, nil, false)
		return err
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		pricingError(c, err)
		return
	}
	common.ApiSuccess(c, report)
}

func GetMarginReport(c *gin.Context) {
	filter := model.MarginFilter{Timezone: c.DefaultQuery("timezone", "UTC"), GroupBy: strings.Split(c.DefaultQuery("group_by", "model_offering,channel,provider,day"), ",")}
	for _, field := range []struct {
		name   string
		target *int
	}{{"model_offering_id", &filter.OfferingID}, {"channel_id", &filter.ChannelID}, {"provider_id", &filter.ProviderID}} {
		if c.Request.URL.Query().Has(field.name) {
			text := c.Query(field.name)
			value, err := strconv.Atoi(text)
			if err != nil || value <= 0 {
				common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
				return
			}
			*field.target = value
		}
	}
	location, err := time.LoadLocation(filter.Timezone)
	if err != nil {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
		return
	}
	for _, field := range []struct {
		name   string
		target *int64
	}{{"from", &filter.From}, {"until", &filter.Until}} {
		if c.Request.URL.Query().Has(field.name) {
			text := c.Query(field.name)
			value, err := time.Parse(time.RFC3339, text)
			if err != nil {
				value, err = time.ParseInLocation(time.DateOnly, text, location)
			}
			if err != nil || value.UnixMilli() <= 0 {
				common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
				return
			}
			*field.target = value.UnixMilli()
		}
	}
	if filter.Until > 0 && filter.From >= filter.Until {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
		return
	}
	for _, dimension := range filter.GroupBy {
		switch dimension {
		case "model_offering", "channel", "provider", "day":
		default:
			common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
			return
		}
	}
	page, ok := gatewayRecordPage(c)
	if !ok {
		return
	}
	items, total, err := model.MarginReport(c.Request.Context(), filter, page.GetStartIdx(), page.GetPageSize())
	if err != nil {
		pricingError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"items": items, "total": total, "page": page.GetPage(), "page_size": page.GetPageSize(), "timezone": filter.Timezone, "revenue_attribution": "last_attempt", "time_basis": "request_started_at"})
}
