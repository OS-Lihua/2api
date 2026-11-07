package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/capability"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"slices"
)

type singleChannelRequest struct {
	ProviderID         int             `json:"provider_id"`
	ModelID            int             `json:"model_id"`
	Protocol           protocol.Type   `json:"protocol"`
	UpstreamModelName  string          `json:"upstream_model_name"`
	Capabilities       *capability.Set `json:"capabilities"`
	ParameterOverrides json.RawMessage `json:"parameter_overrides"`
	RPM                int64           `json:"rpm"`
	TPM                int64           `json:"tpm"`
	MaxConcurrency     int64           `json:"max_concurrency"`
	Weight             int64           `json:"weight"`
}

type singleChannelView struct {
	model.Channel
	ParameterOverrides json.RawMessage `json:"parameter_overrides"`
}

func channelView(record model.Channel) singleChannelView {
	return singleChannelView{Channel: record, ParameterOverrides: json.RawMessage(record.ParameterOverrides)}
}

func singleChannelError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	var message *common.Message
	var pgError *pgconn.PgError
	switch {
	case errors.As(err, &message):
		status = http.StatusBadRequest
	case errors.Is(err, gorm.ErrRecordNotFound):
		status = http.StatusNotFound
		err = common.NewMessage("Channel, provider, or model not found")
	case errors.As(err, &pgError):
		if pgError.Code == "23505" {
			status = http.StatusConflict
			err = common.NewMessage("This provider, protocol, and upstream model already have a channel")
		} else if pgError.Code == "23503" {
			status = http.StatusConflict
			err = common.NewMessage("The channel is still referenced")
		} else {
			status = http.StatusInternalServerError
			err = common.NewMessage("Failed to save channel")
		}
	}
	if status == http.StatusInternalServerError {
		err = common.NewMessage("Failed to save channel")
	}
	common.ApiErrorStatus(c, status, err)
}

// ChannelProtocolOptions exposes the same protocol catalog used by validation.
func ChannelProtocolOptions(c *gin.Context) {
	protocols := protocol.Builtins()
	snapshot := jsplugin.DefaultRegistry.Snapshot()
	keys := []string{}
	for _, metas := range [][]jsplugin.Meta{snapshot.Factory, snapshot.Override} {
		for _, meta := range metas {
			if _, ok := jsplugin.DefaultRegistry.Get(meta.Key); ok && !slices.Contains(keys, meta.Key) {
				keys = append(keys, meta.Key)
			}
		}
	}
	slices.Sort(keys)
	for _, key := range keys {
		protocols = append(protocols, protocol.Type("task:"+key))
	}
	common.ApiSuccess(c, protocols)
}

func ListSingleModelChannels(c *gin.Context) {
	query := model.DB.Model(&model.Channel{})
	for _, name := range []string{"provider_id", "model_id"} {
		if raw := c.Query(name); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value <= 0 {
				common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
				return
			}
			query = query.Where(name+" = ?", value)
		}
	}
	page := common.GetPageQuery(c)
	var total int64
	if err := query.Count(&total).Error; err != nil {
		singleChannelError(c, err)
		return
	}
	records := []model.Channel{}
	if err := query.Order("id").Offset(page.GetStartIdx()).Limit(page.GetPageSize()).Find(&records).Error; err != nil {
		singleChannelError(c, err)
		return
	}
	items := make([]singleChannelView, 0, len(records))
	for _, record := range records {
		items = append(items, channelView(record))
	}
	common.ApiSuccess(c, gin.H{"items": items, "total": total, "page": page.GetPage(), "page_size": page.GetPageSize()})
}

func GetSingleModelChannel(c *gin.Context) {
	id, ok := providerIDParam(c)
	if !ok {
		return
	}
	var record model.Channel
	if err := model.DB.First(&record, id).Error; err != nil {
		singleChannelError(c, err)
		return
	}
	common.ApiSuccess(c, channelView(record))
}

func mutateSingleModelChannel(c *gin.Context, action string) {
	var record model.Channel
	if c.Param("id") != "" {
		id, ok := providerIDParam(c)
		if !ok {
			return
		}
		if err := model.DB.First(&record, id).Error; err != nil {
			singleChannelError(c, err)
			return
		}
	}
	if action == "save" {
		request := singleChannelRequest{ProviderID: record.ProviderID, ModelID: record.ModelID, Protocol: record.Protocol, UpstreamModelName: record.UpstreamModelName, Capabilities: record.Capabilities, ParameterOverrides: json.RawMessage(record.ParameterOverrides), RPM: record.RPM, TPM: record.TPM, MaxConcurrency: record.MaxConcurrency, Weight: record.Weight}
		if record.ID == 0 {
			request.Weight = 1
			request.ParameterOverrides = json.RawMessage(`{}`)
		}
		if err := common.DecodeJson(c.Request.Body, &request); err != nil {
			common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
			return
		}
		record.ProviderID = request.ProviderID
		record.ModelID = request.ModelID
		record.Protocol = request.Protocol
		record.UpstreamModelName = request.UpstreamModelName
		record.Capabilities = request.Capabilities
		record.ParameterOverrides = string(request.ParameterOverrides)
		record.RPM = request.RPM
		record.TPM = request.TPM
		record.MaxConcurrency = request.MaxConcurrency
		record.Weight = request.Weight
	}
	if key, ok := record.Protocol.TaskPluginKey(); ok {
		if _, available := jsplugin.DefaultRegistry.Get(key); !available {
			singleChannelError(c, common.NewMessage("Task plugin is not available"))
			return
		}
	}
	created := record.ID == 0
	if err := model.SaveChannel(c.Request.Context(), &record, action); err != nil {
		singleChannelError(c, err)
		return
	}
	auditAction := "channel." + action
	if action == "save" {
		auditAction = "channel.update"
		if created {
			auditAction = "channel.create"
		}
	}
	recordManageAudit(c, auditAction, map[string]any{"id": record.ID, "provider_id": record.ProviderID, "model_id": record.ModelID})
	common.ApiSuccess(c, channelView(record))
}

func SaveSingleModelChannel(c *gin.Context)    { mutateSingleModelChannel(c, "save") }
func ConfirmSingleModelChannel(c *gin.Context) { mutateSingleModelChannel(c, "confirm") }
func EnableSingleModelChannel(c *gin.Context)  { mutateSingleModelChannel(c, "enable") }
func DisableSingleModelChannel(c *gin.Context) { mutateSingleModelChannel(c, "disable") }

func DeleteSingleModelChannel(c *gin.Context) {
	id, ok := providerIDParam(c)
	if !ok {
		return
	}
	result := model.DB.Delete(&model.Channel{}, id)
	if result.Error != nil {
		singleChannelError(c, result.Error)
		return
	}
	if result.RowsAffected == 0 {
		singleChannelError(c, gorm.ErrRecordNotFound)
		return
	}
	recordManageAudit(c, "channel.delete", map[string]any{"id": id})
	common.ApiSuccess(c, gin.H{"id": id})
}

// Suggestions preserve all version, date, quantization, and context suffixes.
// Only slash-separated namespaces and casing are ignored. Ambiguous matches
// return every candidate and no preselection.
func SuggestChannelModels(c *gin.Context) {
	upstream := strings.TrimSpace(c.Query("upstream_model_name"))
	if upstream == "" {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid upstream model name"))
		return
	}
	upstream = upstream[strings.LastIndex(upstream, "/")+1:]
	var records []model.Model
	if err := model.DB.Where("name_rule = ?", model.NameRuleExact).Order("id").Find(&records).Error; err != nil {
		singleChannelError(c, err)
		return
	}
	candidates := []adminModelResponse{}
	for _, record := range records {
		name := record.ModelName[strings.LastIndex(record.ModelName, "/")+1:]
		if strings.EqualFold(name, upstream) {
			candidates = append(candidates, newAdminModelResponse(&record))
		}
	}
	var selected *int
	if len(candidates) == 1 {
		selected = &candidates[0].ID
	}
	common.ApiSuccess(c, gin.H{"items": candidates, "selected_model_id": selected})
}
