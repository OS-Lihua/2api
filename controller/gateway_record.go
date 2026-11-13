package controller

import (
	"errors"
	"net/http"
	"slices"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// UserGatewayRequest is an allowlist. Future procurement and route fields on
// the stored record cannot accidentally become visible to the request owner.
type UserGatewayRequest struct {
	SalePending        bool               `json:"sale_pending,omitempty"`
	ID                 string             `json:"id"`
	UserID             int                `json:"user_id"`
	TokenID            int                `json:"token_id"`
	ModelOfferingID    int                `json:"model_offering_id"`
	OfferingName       string             `json:"offering_name"`
	RequestedName      string             `json:"requested_name"`
	ChoiceCount        int                `json:"choice_count"`
	SalePriceVersionID int                `json:"sale_price_version_id"`
	SaleCurrency       money.Currency     `json:"sale_currency"`
	SaleFXRate         string             `json:"sale_fx_rate"`
	Usage              model.GatewayUsage `json:"usage"`
	UsageSource        string             `json:"usage_source"`
	GroupRatio         string             `json:"group_ratio"`
	Receivable         money.Amount       `json:"receivable"`
	LedgerEntryIDs     []string           `json:"ledger_entry_ids"`
	StartedAt          int64              `json:"started_at"`
	EndedAt            int64              `json:"ended_at"`
	Status             string             `json:"status"`
	ErrorCode          string             `json:"error_code,omitempty"`
}

type UserGatewayAttempt struct {
	ID               string             `json:"id"`
	RequestID        string             `json:"request_id"`
	StartedAt        int64              `json:"started_at"`
	EndedAt          int64              `json:"ended_at"`
	Status           string             `json:"status"`
	ErrorClass       string             `json:"error_class"`
	Usage            model.GatewayUsage `json:"usage"`
	UsageSource      string             `json:"usage_source"`
	ConversionLosses []string           `json:"conversion_losses"`
}

func gatewayRecordError(c *gin.Context, err error) {
	status, message := http.StatusInternalServerError, common.NewMessage("Failed to retrieve logs")
	if errors.Is(err, gorm.ErrRecordNotFound) {
		status, message = http.StatusNotFound, common.NewMessage("Not found")
	}
	common.ApiErrorStatus(c, status, message)
}

func gatewayRecordPage(c *gin.Context) (*common.PageInfo, bool) {
	page := &common.PageInfo{Page: 1, PageSize: 20}
	pageText := c.Query("page")
	if pageText == "" {
		pageText = c.Query("p")
	}
	if pageText != "" {
		number, err := strconv.Atoi(pageText)
		if err != nil || number < 1 || number > 1_000_000 {
			common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid request"))
			return nil, false
		}
		page.Page = number
	}
	if text := c.Query("page_size"); text != "" {
		number, err := strconv.Atoi(text)
		if err != nil || number < 1 || number > 100 {
			common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid request"))
			return nil, false
		}
		page.PageSize = number
	}
	return page, true
}

func ListGatewayRequests(c *gin.Context)     { listGatewayRequests(c, false) }
func ListSelfGatewayRequests(c *gin.Context) { listGatewayRequests(c, true) }

func listGatewayRequests(c *gin.Context, self bool) {
	c.Header("Cache-Control", "no-store")
	page, ok := gatewayRecordPage(c)
	if !ok {
		return
	}
	filter := model.GatewayRequestFilter{Status: c.Query("status")}
	if filter.Status != "" && !slices.Contains([]string{"success", "failed", "cancelled", "partial", "settlement_pending", "pending", "submission_unknown"}, filter.Status) {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid request"))
		return
	}
	for _, parameter := range []struct {
		name   string
		target *int
	}{{"user_id", &filter.UserID}, {"token_id", &filter.TokenID}, {"model_offering_id", &filter.ModelOfferingID}} {
		if self && parameter.name == "user_id" {
			continue
		}
		if text, present := c.GetQuery(parameter.name); present {
			value, err := strconv.Atoi(text)
			if err != nil || value < 1 {
				common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid request"))
				return
			}
			*parameter.target = value
		}
	}
	for _, parameter := range []struct {
		name   string
		target *int64
	}{{"started_after", &filter.StartedAfter}, {"started_before", &filter.StartedBefore}} {
		if text, present := c.GetQuery(parameter.name); present {
			value, err := strconv.ParseInt(text, 10, 64)
			if err != nil || value < 1 {
				common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid request"))
				return
			}
			*parameter.target = value
		}
	}
	if self {
		filter.UserID = c.GetInt("id")
		if filter.UserID <= 0 {
			gatewayRecordError(c, gorm.ErrRecordNotFound)
			return
		}
	}
	items, total, err := model.ListGatewayRequests(c.Request.Context(), filter, page.GetStartIdx(), page.PageSize)
	if err != nil {
		gatewayRecordError(c, err)
		return
	}
	var payload any = items
	if self {
		users := make([]UserGatewayRequest, 0, len(items))
		for _, item := range items {
			users = append(users, UserGatewayRequest{SalePending: item.SalePending, ID: item.ID, UserID: item.UserID, TokenID: item.TokenID, ModelOfferingID: item.ModelOfferingID, OfferingName: item.OfferingName, RequestedName: item.RequestedName,
				ChoiceCount: item.ChoiceCount, SalePriceVersionID: item.SalePriceVersionID, SaleCurrency: item.SaleCurrency, SaleFXRate: item.SaleFXRate, Usage: item.Usage, UsageSource: item.UsageSource, GroupRatio: item.GroupRatio,
				Receivable: item.Receivable, LedgerEntryIDs: item.LedgerEntryIDs, StartedAt: item.StartedAt, EndedAt: item.EndedAt, Status: item.Status, ErrorCode: item.ErrorCode})
		}
		payload = users
	}
	response := gin.H{"items": payload, "total": total, "page": page.Page, "page_size": page.PageSize}
	if self {
		outstanding, err := model.GetUserOutstanding(filter.UserID)
		if err != nil {
			gatewayRecordError(c, err)
			return
		}
		response["outstanding"] = outstanding
	}
	common.ApiSuccess(c, response)
}

func ListGatewayAttempts(c *gin.Context)     { listGatewayAttempts(c, false) }
func ListSelfGatewayAttempts(c *gin.Context) { listGatewayAttempts(c, true) }

func listGatewayAttempts(c *gin.Context, self bool) {
	c.Header("Cache-Control", "no-store")
	page, ok := gatewayRecordPage(c)
	if !ok {
		return
	}
	ownerID := 0
	if self {
		ownerID = c.GetInt("id")
		if ownerID <= 0 {
			gatewayRecordError(c, gorm.ErrRecordNotFound)
			return
		}
	}
	request, err := model.GetGatewayRequest(c.Request.Context(), c.Param("id"), ownerID)
	if err != nil {
		gatewayRecordError(c, err)
		return
	}
	items, total, err := model.ListGatewayAttempts(c.Request.Context(), request.ID, page.GetStartIdx(), page.PageSize)
	if err != nil {
		gatewayRecordError(c, err)
		return
	}
	var payload any = items
	if self {
		users := make([]UserGatewayAttempt, 0, len(items))
		for _, item := range items {
			users = append(users, UserGatewayAttempt{ID: item.ID, RequestID: item.RequestID, StartedAt: item.StartedAt, EndedAt: item.EndedAt, Status: item.Status, ErrorClass: item.ErrorClass, Usage: item.Usage, UsageSource: item.UsageSource, ConversionLosses: item.ConversionLosses})
		}
		payload = users
	}
	common.ApiSuccess(c, gin.H{"items": payload, "total": total, "page": page.Page, "page_size": page.PageSize})
}
