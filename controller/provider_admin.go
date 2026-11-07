package controller

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// The /api/admin/v1/providers handlers reuse the /api/channel handlers so both
// APIs keep the same validation, permission checks, cache refresh, and audit
// records until the legacy routes are removed. The credential is write-only:
// no response of this API contains a key or a part of one.

// providerView is a Provider as this API returns it. The shallower Key field
// hides the embedded credential and is always empty, so it is omitted.
type providerView struct {
	*model.Provider
	Key string `json:"key,omitempty"`
}

// providerKeyView is the state of one key of a multi-key Provider.
type providerKeyView struct {
	Index        int    `json:"index"`
	Status       int    `json:"status"`
	DisabledTime int64  `json:"disabled_time,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

type createProviderRequest struct {
	Mode                      string          `json:"mode"`
	MultiKeyMode              string          `json:"multi_key_mode"`
	BatchAddSetKeyPrefix2Name bool            `json:"batch_add_set_key_prefix_2_name"`
	Provider                  json.RawMessage `json:"provider"`
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

// forward writes the reply unchanged; failures keep the legacy message fields.
func (reply *legacyProviderReply) forward(c *gin.Context) {
	c.Data(reply.status, "application/json; charset=utf-8", reply.body)
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
		common.ApiErrorT(c, "Invalid parameters")
		return 0, false
	}
	return id, true
}

func respondProvider(c *gin.Context, id int) {
	provider, err := model.GetProviderById(id, false)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	clearChannelInfo(provider)
	common.ApiSuccess(c, providerView{Provider: provider})
}

// ListProviders lists Providers. Filters: keyword (ID, name, or base URL),
// group, status (enabled or disabled), and type.
func ListProviders(c *gin.Context) {
	pageInfo := common.GetPageQuery(c)
	idSort, _ := strconv.ParseBool(c.Query("id_sort"))
	sortOptions := model.NewProviderSortOptions(c.Query("sort_by"), c.Query("sort_order"), idSort)
	typeFilter := -1
	if value := c.Query("type"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			common.ApiErrorT(c, "Invalid parameters")
			return
		}
		typeFilter = parsed
	}
	query := buildChannelListQuery(model.NormalizeProviderGroupFilter(c.Query("group")), parseStatusFilter(c.Query("status")), typeFilter)
	if keyword := strings.TrimSpace(c.Query("keyword")); keyword != "" {
		pattern := "%" + keyword + "%"
		query = query.Where(`(id = ? OR name LIKE ? OR "base_url" LIKE ?)`, common.String2Int(keyword), pattern, pattern)
	}

	var total int64
	if err := query.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	providers := make([]*model.Provider, 0)
	if err := sortOptions.Apply(query).Omit("key").
		Limit(pageInfo.GetPageSize()).Offset(pageInfo.GetStartIdx()).
		Find(&providers).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	items := make([]providerView, 0, len(providers))
	for _, provider := range providers {
		clearChannelInfo(provider)
		items = append(items, providerView{Provider: provider})
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
	if err := c.ShouldBindJSON(&request); err != nil {
		common.ApiError(c, err)
		return
	}
	if len(request.Provider) == 0 {
		common.ApiErrorT(c, "Invalid parameters")
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
		common.ApiError(c, err)
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
		common.ApiError(c, err)
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
	if err := c.ShouldBindJSON(&fields); err != nil {
		common.ApiError(c, err)
		return
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	fields["id"] = json.RawMessage(strconv.Itoa(id))
	reply, err := runLegacyProviderHandler(c, UpdateChannel, fields, nil)
	if err != nil {
		common.ApiError(c, err)
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
		common.ApiError(c, err)
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
		common.ApiError(c, err)
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

// RefreshProviderBalance queries the upstream balance and stores it. The reply
// is the Provider with the new balance; raw_response is set when the upstream
// answer could not be read as a balance.
func RefreshProviderBalance(c *gin.Context) {
	id, ok := providerIDParam(c)
	if !ok {
		return
	}
	reply, err := runLegacyProviderHandler(c, UpdateChannelBalance, nil, gin.Params{{Key: "id", Value: strconv.Itoa(id)}})
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if !reply.Success {
		reply.forward(c)
		return
	}
	if reply.Raw != "" {
		common.ApiSuccess(c, gin.H{"raw_response": reply.Raw})
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
		common.ApiError(c, err)
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
			common.ApiErrorT(c, "Invalid parameters")
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
		common.ApiError(c, err)
		return
	}
	keys := make([]providerKeyView, 0, len(keyStatus.Keys))
	for _, key := range keyStatus.Keys {
		keys = append(keys, providerKeyView{Index: key.Index, Status: key.Status, DisabledTime: key.DisabledTime, Reason: key.Reason})
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
				common.ApiErrorT(c, "Invalid parameters")
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
