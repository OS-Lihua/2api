package controller

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

func GetSelfDashboardFinance(c *gin.Context)  { getDashboardFinance(c, false) }
func GetAdminDashboardFinance(c *gin.Context) { getDashboardFinance(c, true) }

func getDashboardFinance(c *gin.Context, admin bool) {
	filter, ok := dashboardFinanceFilter(c, admin)
	if !ok {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
		return
	}
	report, err := model.DashboardFinance(c.Request.Context(), filter)
	if err != nil {
		if errors.Is(err, model.ErrDashboardFinanceLimit) {
			common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
			return
		}
		// Never expose frozen request payloads, account identities or SQL errors.
		common.ApiErrorStatus(c, http.StatusInternalServerError, common.NewMessage("Failed to load"))
		return
	}
	common.ApiSuccess(c, report)
}

func dashboardFinanceFilter(c *gin.Context, admin bool) (model.DashboardFinanceFilter, bool) {
	filter := model.DashboardFinanceFilter{Bucket: "day", Timezone: "UTC", GroupBy: []string{}, Admin: admin}
	if !admin {
		filter.UserID = c.GetInt("id")
		if filter.UserID <= 0 {
			return filter, false
		}
	}
	// Query() discards malformed percent encodings. ParseQuery must reject them.
	values, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		return filter, false
	}
	for key, list := range values {
		if len(list) != 1 || list[0] == "" {
			return filter, false
		}
		switch key {
		case "from", "until", "bucket", "timezone", "group_by", "token_id", "model_offering_id":
		case "user_id", "channel_id", "provider_id":
			if !admin {
				return filter, false
			}
		default:
			return filter, false
		}
	}
	for _, field := range []struct {
		name   string
		target *int64
	}{{"from", &filter.From}, {"until", &filter.Until}} {
		text := values.Get(field.name)
		at, err := time.Parse(time.RFC3339Nano, text)
		if err != nil || !at.Equal(time.UnixMilli(at.UnixMilli())) {
			return filter, false
		}
		*field.target = at.UnixMilli()
	}
	if values.Has("bucket") {
		filter.Bucket = values.Get("bucket")
	}
	if values.Has("timezone") {
		filter.Timezone = values.Get("timezone")
	}
	if values.Has("group_by") {
		filter.GroupBy = strings.Split(values.Get("group_by"), ",")
	}
	for _, field := range []struct {
		name   string
		target *int
	}{
		{"user_id", &filter.UserID}, {"token_id", &filter.TokenID}, {"model_offering_id", &filter.OfferingID}, {"channel_id", &filter.ChannelID}, {"provider_id", &filter.ProviderID},
	} {
		if !values.Has(field.name) {
			continue
		}
		text := values.Get(field.name)
		value, err := strconv.Atoi(text)
		if err != nil || value <= 0 || strconv.Itoa(value) != text {
			return filter, false
		}
		*field.target = value
	}
	return filter, filter.Validate() == nil
}
