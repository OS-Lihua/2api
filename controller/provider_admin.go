package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// The /api/admin/v1/providers handlers reuse the /api/channel handlers so both
// APIs keep the same validation, permission checks, cache refresh, and audit
// records until the legacy routes are removed. The credential is write-only:
// no response of this API contains a key or a part of one. A rejected request
// answers 400, a missing Provider 404, and an internal failure 500.

// providerView explicitly lists readable fields. Adapter configuration, upstream
// errors, and credentials are write-only, including custom authentication headers.
type providerView struct {
	ID                 int                       `json:"id"`
	Type               int                       `json:"type"`
	Name               string                    `json:"name"`
	Status             int                       `json:"status"`
	BaseURL            *string                   `json:"base_url"`
	Balance            float64                   `json:"balance"`
	BalanceUpdatedTime int64                     `json:"balance_updated_time"`
	CreatedTime        int64                     `json:"created_time"`
	Remark             *string                   `json:"remark"`
	RouteLimits        model.ProviderRouteLimits `json:"route_limits"`
	Info               struct {
		IsMultiKey   bool                  `json:"is_multi_key"`
		MultiKeySize int                   `json:"multi_key_size"`
		MultiKeyMode constant.MultiKeyMode `json:"multi_key_mode"`
	} `json:"channel_info"`
}

func readableProvider(provider *model.Provider) providerView {
	view := providerView{
		ID: provider.Id, Type: provider.Type, Name: provider.Name, Status: provider.Status,
		BaseURL: readableProviderURL(provider.BaseURL), Balance: provider.Balance,
		BalanceUpdatedTime: provider.BalanceUpdatedTime, CreatedTime: provider.CreatedTime, Remark: provider.Remark,
	}
	view.RouteLimits = provider.ProviderInfo.RouteLimits
	view.Info.IsMultiKey = provider.ProviderInfo.IsMultiKey
	view.Info.MultiKeySize = provider.ProviderInfo.MultiKeySize
	view.Info.MultiKeyMode = provider.ProviderInfo.MultiKeyMode
	return view
}

const (
	providerURLRedaction     = "REDACTED"
	providerURLRedactedQuery = "__redacted__"
	providerURLRedactedHost  = "redacted.invalid"
)

// readableProviderURL retains the endpoint but hides opaque URL credentials.
// The public marker also distinguishes a readback from an explicit replacement.
func readableProviderURL(raw *string) *string {
	if raw == nil || *raw == "" {
		return raw
	}
	endpoint, err := url.Parse(*raw)
	if err != nil || endpoint.Hostname() == "" || endpoint.Scheme == "" {
		redacted := "https://" + providerURLRedactedHost + "/"
		return &redacted
	}
	changed := false
	if endpoint.User != nil {
		endpoint.User = url.UserPassword(providerURLRedaction, providerURLRedaction)
		changed = true
	}
	if endpoint.RawQuery != "" {
		endpoint.RawQuery = providerURLRedactedQuery + "=1"
		changed = true
	}
	if endpoint.Fragment != "" {
		endpoint.Fragment, endpoint.RawFragment = providerURLRedaction, ""
		changed = true
	}
	if !changed {
		return raw
	}
	redacted := endpoint.String()
	return &redacted
}

// providerKeyView is the state of one key of a multi-key Provider.
type providerKeyView struct {
	Index        int   `json:"index"`
	Status       int   `json:"status"`
	DisabledTime int64 `json:"disabled_time,omitempty"`
}

type createProviderRequest struct {
	Mode                      string          `json:"mode"`
	MultiKeyMode              string          `json:"multi_key_mode"`
	BatchAddSetKeyPrefix2Name bool            `json:"batch_add_set_key_prefix_2_name"`
	Provider                  json.RawMessage `json:"provider"`
}

// validProviderAccountFields keeps retired routing settings out of account
// mutations. Omitted legacy fields already stored on a Provider are not rewritten.
func validProviderAccountFields(fields map[string]json.RawMessage) bool {
	if fields == nil {
		return false
	}
	for name, raw := range fields {
		for _, retired := range []string{"models", "model_mapping", "group", "priority", "weight", "tool_loss_policy"} {
			if strings.EqualFold(name, retired) {
				return false
			}
		}
		if strings.EqualFold(name, "param_override") {
			var paramOverride map[string]any
			if err := common.Unmarshal(raw, &paramOverride); err != nil || relaycommon.ValidateParamOverrideContextReferences(paramOverride) != nil {
				return false
			}
			continue
		}
		if !strings.EqualFold(name, "settings") {
			continue
		}
		var settings *string
		if err := common.Unmarshal(raw, &settings); err != nil {
			return false
		}
		if settings == nil || strings.TrimSpace(*settings) == "" {
			continue
		}
		var other map[string]json.RawMessage
		if err := common.DecodeJsonStrict(strings.NewReader(*settings), &other); err != nil || other == nil {
			return false
		}
		for field := range other {
			if strings.EqualFold(field, "tool_loss_policy") {
				return false
			}
		}
	}
	return true
}

// legacyProviderReply is what a reused /api/channel handler wrote.
type legacyProviderReply struct {
	status  int
	body    []byte
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Balance *float64        `json:"balance"`
	Raw     string          `json:"raw_response"`
}

// forward writes the reply body unchanged, so failures keep the legacy
// message fields. A failure the legacy handler sent as 200 becomes 400.
func (reply *legacyProviderReply) forward(c *gin.Context) {
	status := reply.status
	if !reply.Success && status < http.StatusBadRequest {
		status = http.StatusBadRequest
	}
	c.Data(status, "application/json; charset=utf-8", reply.body)
}

type bufferedResponseWriter struct {
	gin.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *bufferedResponseWriter) WriteHeader(code int)           { w.status = code }
func (w *bufferedResponseWriter) WriteHeaderNow()                {}
func (w *bufferedResponseWriter) Write(data []byte) (int, error) { return w.body.Write(data) }
func (w *bufferedResponseWriter) WriteString(s string) (int, error) {
	return w.body.WriteString(s)
}
func (w *bufferedResponseWriter) Status() int   { return w.status }
func (w *bufferedResponseWriter) Size() int     { return w.body.Len() }
func (w *bufferedResponseWriter) Written() bool { return w.body.Len() > 0 }

// runLegacyProviderHandler calls a /api/channel handler with a JSON body and
// route params, and returns its reply without writing it to the client.
func runLegacyProviderHandler(c *gin.Context, handler gin.HandlerFunc, body any, params gin.Params) (*legacyProviderReply, error) {
	originalWriter, originalBody, originalParams := c.Writer, c.Request.Body, c.Params
	defer func() {
		c.Writer, c.Request.Body, c.Params = originalWriter, originalBody, originalParams
	}()
	if body != nil {
		encoded, err := common.Marshal(body)
		if err != nil {
			return nil, err
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(encoded))
	}
	if params != nil {
		c.Params = params
	}
	writer := &bufferedResponseWriter{ResponseWriter: originalWriter, status: http.StatusOK}
	c.Writer = writer
	handler(c)

	reply := &legacyProviderReply{status: writer.status, body: writer.body.Bytes()}
	if err := common.Unmarshal(reply.body, reply); err != nil {
		return nil, err
	}
	return reply, nil
}

func providerIDParam(c *gin.Context) (int, bool) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
		return 0, false
	}
	return id, true
}

func respondProvider(c *gin.Context, id int) {
	provider, err := model.GetProviderById(id, false)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		common.ApiErrorStatus(c, http.StatusNotFound, common.NewMessage("Provider does not exist"))
		return
	}
	if err != nil {
		common.ApiErrorStatus(c, http.StatusInternalServerError, err)
		return
	}
	clearChannelInfo(provider)
	common.ApiSuccess(c, readableProvider(provider))
}

// ListProviders lists accounts in descending ID order. Filters: keyword (ID,
// name, or address host), status (enabled or disabled), and type.
func ListProviders(c *gin.Context) {
	parameters, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
		return
	}
	for name := range parameters {
		switch strings.ToLower(name) {
		case "group", "sort_by", "sort_order", "id_sort":
			common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
			return
		}
	}
	pageInfo := common.GetPageQuery(c)
	typeFilter := -1
	if value := parameters.Get("type"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
			return
		}
		typeFilter = parsed
	}
	query := model.DB.WithContext(c.Request.Context()).Model(&model.Provider{})
	query = applyChannelStatusFilter(query, parseStatusFilter(parameters.Get("status")))
	if typeFilter >= 0 {
		query = query.Where("type = ?", typeFilter)
	}
	if keyword := strings.TrimSpace(parameters.Get("keyword")); keyword != "" {
		pattern := "%" + keyword + "%"
		host := keyword
		if address, err := url.Parse(keyword); err == nil && address.Hostname() != "" {
			host = address.Host
		}
		// Address search must not act as an oracle for the hidden URL parts.
		const hostPattern = `^[A-Za-z][A-Za-z0-9+.-]*://(?:[^/?#]*@)?((?:\[[^]]+\]|[[:alnum:]._-]+)(?::[0-9]+)?)(?:[/?#]|$)`
		query = query.Where(`("id" = ? OR "name" LIKE ? OR substring("base_url" FROM ?) ILIKE ?)`, common.String2Int(keyword), pattern, hostPattern, "%"+host+"%")
	}

	var total int64
	if err := query.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		common.ApiErrorStatus(c, http.StatusInternalServerError, err)
		return
	}
	providers := make([]*model.Provider, 0)
	if err := query.Select("id", "type", "name", "status", "base_url", "balance", "balance_updated_time", "created_time", "remark", "channel_info").
		Order(`"id" DESC`).Limit(pageInfo.GetPageSize()).Offset(pageInfo.GetStartIdx()).
		Find(&providers).Error; err != nil {
		common.ApiErrorStatus(c, http.StatusInternalServerError, err)
		return
	}
	items := make([]providerView, 0, len(providers))
	for _, provider := range providers {
		clearChannelInfo(provider)
		items = append(items, readableProvider(provider))
	}
	common.ApiSuccess(c, gin.H{
		"items":     items,
		"total":     total,
		"page":      pageInfo.GetPage(),
		"page_size": pageInfo.GetPageSize(),
	})
}

func GetProvider(c *gin.Context) {
	id, ok := providerIDParam(c)
	if !ok {
		return
	}
	respondProvider(c, id)
}

// CreateProvider takes the Provider under "provider". mode is "single"
// (default), "batch" (one Provider per key line), or "multi_to_single" (one
// multi-key Provider). The reply holds only the new IDs.
func CreateProvider(c *gin.Context) {
	var request createProviderRequest
	if err := common.DecodeJsonStrict(c.Request.Body, &request); err != nil {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
		return
	}
	var fields map[string]json.RawMessage
	if err := common.Unmarshal(request.Provider, &fields); err != nil || !validProviderAccountFields(fields) || request.BatchAddSetKeyPrefix2Name {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
		return
	}
	if request.Mode == "" {
		request.Mode = "single"
	}
	reply, err := runLegacyProviderHandler(c, AddChannel, map[string]any{
		"mode":                            request.Mode,
		"multi_key_mode":                  request.MultiKeyMode,
		"batch_add_set_key_prefix_2_name": request.BatchAddSetKeyPrefix2Name,
		"channel":                         request.Provider,
	}, nil)
	if err != nil {
		common.ApiErrorStatus(c, http.StatusInternalServerError, err)
		return
	}
	if !reply.Success {
		reply.forward(c)
		return
	}
	var created struct {
		IDs []int `json:"ids"`
	}
	if err := common.Unmarshal(reply.Data, &created); err != nil {
		common.ApiErrorStatus(c, http.StatusInternalServerError, err)
		return
	}
	common.ApiSuccess(c, gin.H{"ids": created.IDs})
}

// UpdateProvider changes only the fields in the body. An absent or empty key
// keeps the stored credential; key_mode "append" adds keys to a multi-key
// Provider. Status changes go through the enable and disable routes.
func UpdateProvider(c *gin.Context) {
	id, ok := providerIDParam(c)
	if !ok {
		return
	}
	var fields map[string]json.RawMessage
	if err := common.DecodeJsonStrict(c.Request.Body, &fields); err != nil || !validProviderAccountFields(fields) {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
		return
	}
	fields["id"] = json.RawMessage(strconv.Itoa(id))
	// Validate the effective adapter configuration without resubmitting omitted
	// fields, which must preserve stored credentials and concurrent edits.
	candidate, err := model.GetProviderById(id, false)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		common.ApiErrorStatus(c, http.StatusNotFound, common.NewMessage("Provider does not exist"))
		return
	}
	if err != nil {
		common.ApiErrorStatus(c, http.StatusInternalServerError, err)
		return
	}
	if rawURL, provided := fields["base_url"]; provided {
		var replacement *string
		if err := common.Unmarshal(rawURL, &replacement); err != nil {
			common.ApiErrorStatus(c, http.StatusBadRequest, err)
			return
		}
		visible := readableProviderURL(candidate.BaseURL)
		if replacement != nil && visible != nil && candidate.BaseURL != nil && *visible != *candidate.BaseURL && *replacement == *visible {
			delete(fields, "base_url")
		} else if replacement != nil && *replacement != "" {
			endpoint, err := url.Parse(*replacement)
			if err != nil {
				common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Enter the full base URL to replace the hidden credentials"))
				return
			}
			incomplete := strings.EqualFold(endpoint.Hostname(), providerURLRedactedHost) || endpoint.Query().Has(providerURLRedactedQuery) || endpoint.Fragment == providerURLRedaction
			if endpoint.User != nil {
				userPassword, _ := endpoint.User.Password()
				incomplete = incomplete || endpoint.User.Username() == providerURLRedaction || userPassword == providerURLRedaction
			}
			if incomplete {
				common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Enter the full base URL to replace the hidden credentials"))
				return
			}
		}
	}
	patch, err := common.Marshal(fields)
	if err != nil {
		common.ApiErrorStatus(c, http.StatusBadRequest, err)
		return
	}
	if err := common.Unmarshal(patch, candidate); err != nil {
		common.ApiErrorStatus(c, http.StatusBadRequest, err)
		return
	}
	if err := validateChannel(candidate, false); err != nil {
		common.ApiErrorStatus(c, http.StatusBadRequest, err)
		return
	}
	reply, err := runLegacyProviderHandler(c, UpdateChannel, fields, nil)
	if err != nil {
		common.ApiErrorStatus(c, http.StatusInternalServerError, err)
		return
	}
	if !reply.Success {
		reply.forward(c)
		return
	}
	respondProvider(c, id)
}

func DeleteProvider(c *gin.Context) {
	id, ok := providerIDParam(c)
	if !ok {
		return
	}
	reply, err := runLegacyProviderHandler(c, DeleteChannel, nil, gin.Params{{Key: "id", Value: strconv.Itoa(id)}})
	if err != nil {
		common.ApiErrorStatus(c, http.StatusInternalServerError, err)
		return
	}
	reply.forward(c)
}

func setProviderStatus(c *gin.Context, status int) {
	id, ok := providerIDParam(c)
	if !ok {
		return
	}
	reply, err := runLegacyProviderHandler(c, UpdateChannelStatus, ProviderStatusRequest{Status: status}, gin.Params{{Key: "id", Value: strconv.Itoa(id)}})
	if err != nil {
		common.ApiErrorStatus(c, http.StatusInternalServerError, err)
		return
	}
	if !reply.Success {
		reply.forward(c)
		return
	}
	respondProvider(c, id)
}

func EnableProvider(c *gin.Context) {
	setProviderStatus(c, common.ChannelStatusEnabled)
}

func DisableProvider(c *gin.Context) {
	setProviderStatus(c, common.ChannelStatusManuallyDisabled)
}

// RefreshProviderBalance queries and stores the upstream balance. Raw upstream
// responses are not returned because they may contain account credentials.
func RefreshProviderBalance(c *gin.Context) {
	id, ok := providerIDParam(c)
	if !ok {
		return
	}
	reply, err := runLegacyProviderHandler(c, UpdateChannelBalance, nil, gin.Params{{Key: "id", Value: strconv.Itoa(id)}})
	if err != nil {
		common.ApiErrorStatus(c, http.StatusInternalServerError, err)
		return
	}
	if !reply.Success || reply.Raw != "" {
		common.ApiErrorStatus(c, http.StatusBadGateway, common.NewMessage("Failed to query balance"))
		return
	}
	respondProvider(c, id)
}

func manageProviderKeys(c *gin.Context, request MultiKeyManageRequest) (*legacyProviderReply, bool) {
	id, ok := providerIDParam(c)
	if !ok {
		return nil, false
	}
	request.ChannelId = id
	reply, err := runLegacyProviderHandler(c, ManageMultiKeys, request, nil)
	if err != nil {
		common.ApiErrorStatus(c, http.StatusInternalServerError, err)
		return nil, false
	}
	if !reply.Success {
		reply.forward(c)
		return nil, false
	}
	return reply, true
}

// ListProviderKeys pages through the keys of a multi-key Provider. Keys are
// identified by index; their values are never returned.
func ListProviderKeys(c *gin.Context) {
	request := MultiKeyManageRequest{Action: "get_key_status"}
	request.Page, _ = strconv.Atoi(c.Query("p"))
	request.PageSize, _ = strconv.Atoi(c.Query("page_size"))
	if value := c.Query("status"); value != "" {
		status, err := strconv.Atoi(value)
		if err != nil {
			common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
			return
		}
		request.Status = &status
	}
	reply, ok := manageProviderKeys(c, request)
	if !ok {
		return
	}
	var keyStatus MultiKeyStatusResponse
	if err := common.Unmarshal(reply.Data, &keyStatus); err != nil {
		common.ApiErrorStatus(c, http.StatusInternalServerError, err)
		return
	}
	keys := make([]providerKeyView, 0, len(keyStatus.Keys))
	for _, key := range keyStatus.Keys {
		keys = append(keys, providerKeyView{Index: key.Index, Status: key.Status, DisabledTime: key.DisabledTime})
	}
	common.ApiSuccess(c, gin.H{
		"keys":                  keys,
		"total":                 keyStatus.Total,
		"page":                  keyStatus.Page,
		"page_size":             keyStatus.PageSize,
		"total_pages":           keyStatus.TotalPages,
		"enabled_count":         keyStatus.EnabledCount,
		"manual_disabled_count": keyStatus.ManualDisabledCount,
		"auto_disabled_count":   keyStatus.AutoDisabledCount,
	})
}

// providerKeyAction runs a multi-key action. Actions on one key take the key
// index from the route; the reply is the Provider after the change.
func providerKeyAction(action string, perKey bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		request := MultiKeyManageRequest{Action: action}
		if perKey {
			index, err := strconv.Atoi(c.Param("index"))
			if err != nil || index < 0 {
				common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
				return
			}
			request.KeyIndex = &index
		}
		if _, ok := manageProviderKeys(c, request); !ok {
			return
		}
		id, _ := providerIDParam(c)
		respondProvider(c, id)
	}
}

var (
	EnableProviderKey          = providerKeyAction("enable_key", true)
	DisableProviderKey         = providerKeyAction("disable_key", true)
	DeleteProviderKey          = providerKeyAction("delete_key", true)
	EnableAllProviderKeys      = providerKeyAction("enable_all_keys", false)
	DisableAllProviderKeys     = providerKeyAction("disable_all_keys", false)
	DeleteDisabledProviderKeys = providerKeyAction("delete_disabled_keys", false)
)
