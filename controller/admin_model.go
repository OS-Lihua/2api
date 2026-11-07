package controller

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/capability"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// adminModelRequest is the body of POST and PUT /api/admin/v1/models. PUT
// replaces every field; absent status and sync_official default to 1.
type adminModelRequest struct {
	Name         string         `json:"name"`
	PublisherID  int            `json:"publisher_id"`
	Description  string         `json:"description"`
	Icon         string         `json:"icon"`
	Tags         string         `json:"tags"`
	Endpoints    string         `json:"endpoints"`
	Status       *int           `json:"status,omitempty"`
	SyncOfficial *int           `json:"sync_official,omitempty"`
	Capabilities capability.Set `json:"capabilities"`
}

type adminModelResponse struct {
	ID           int            `json:"id"`
	Name         string         `json:"name"`
	PublisherID  int            `json:"publisher_id"`
	Description  string         `json:"description"`
	Icon         string         `json:"icon"`
	Tags         string         `json:"tags"`
	Endpoints    string         `json:"endpoints"`
	Status       int            `json:"status"`
	SyncOfficial int            `json:"sync_official"`
	Capabilities capability.Set `json:"capabilities"`
	CreatedTime  int64          `json:"created_time"`
	UpdatedTime  int64          `json:"updated_time"`
}

func newAdminModelResponse(record *model.Model) adminModelResponse {
	return adminModelResponse{
		ID:           record.Id,
		Name:         record.ModelName,
		PublisherID:  record.VendorID,
		Description:  record.Description,
		Icon:         record.Icon,
		Tags:         record.Tags,
		Endpoints:    record.Endpoints,
		Status:       record.Status,
		SyncOfficial: record.SyncOfficial,
		Capabilities: record.Capabilities,
		CreatedTime:  record.CreatedTime,
		UpdatedTime:  record.UpdatedTime,
	}
}

// modelNameMaxLength matches the size of the model_name column.
const modelNameMaxLength = 128

// declaration validates the request and copies it onto record. Names are kept
// as written: a version, date, quantization, or context suffix makes a
// different Model.
func (request adminModelRequest) declaration(record *model.Model) error {
	name := strings.TrimSpace(request.Name)
	if name == "" {
		return common.NewMessage("Model name cannot be empty")
	}
	if utf8.RuneCountInString(name) > modelNameMaxLength {
		return common.NewMessage("Model name must be at most {{max}} characters", map[string]any{"max": modelNameMaxLength})
	}
	if request.PublisherID <= 0 {
		return common.NewMessage("Publisher is required")
	}
	var publishers int64
	if err := model.DB.Model(&model.Vendor{}).Where("id = ?", request.PublisherID).Count(&publishers).Error; err != nil {
		return err
	}
	if publishers == 0 {
		return common.NewMessage("Publisher does not exist")
	}
	status, syncOfficial := 1, 1
	if request.Status != nil {
		status = *request.Status
	}
	if request.SyncOfficial != nil {
		syncOfficial = *request.SyncOfficial
	}
	if status != 0 && status != 1 {
		return common.NewMessage("Invalid catalog visibility")
	}
	if syncOfficial != 0 && syncOfficial != 1 {
		return common.NewMessage("Invalid metadata sync setting")
	}
	if err := model.ValidateModelEndpoints(request.Endpoints); err != nil {
		return err
	}
	capabilities, err := request.Capabilities.Normalize()
	if err != nil {
		return err
	}
	record.ModelName = name
	record.VendorID = request.PublisherID
	record.Description = request.Description
	record.Icon = request.Icon
	record.Tags = request.Tags
	record.Endpoints = request.Endpoints
	record.Status = status
	record.SyncOfficial = syncOfficial
	record.NameRule = model.NameRuleExact
	record.Capabilities = capabilities
	return nil
}

func adminModelID(c *gin.Context) (int, bool) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		common.ApiErrorStatus(c, http.StatusNotFound, common.NewMessage("Model not found"))
		return 0, false
	}
	return id, true
}

func adminModelLookupError(c *gin.Context, err error) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		common.ApiErrorStatus(c, http.StatusNotFound, common.NewMessage("Model not found"))
		return
	}
	common.ApiErrorStatus(c, http.StatusInternalServerError, err)
}

// adminModelNameTaken reports a name used by any metadata row, including
// rule rows, because the column is unique.
func adminModelNameTaken(c *gin.Context, id int, name string) bool {
	duplicated, err := model.IsModelNameDuplicated(id, name)
	if err != nil {
		common.ApiErrorStatus(c, http.StatusInternalServerError, err)
		return true
	}
	if duplicated {
		common.ApiErrorStatus(c, http.StatusConflict, common.NewMessage("Model name already exists"))
		return true
	}
	return false
}

// AdminListModels handles GET /api/admin/v1/models.
func AdminListModels(c *gin.Context) {
	publisherID := 0
	if raw := c.Query("publisher_id"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid publisher ID"))
			return
		}
		publisherID = parsed
	}
	pageInfo := common.GetPageQuery(c)
	records, total, err := model.SearchExactModels(strings.TrimSpace(c.Query("keyword")), publisherID, pageInfo.GetStartIdx(), pageInfo.GetPageSize())
	if err != nil {
		common.ApiErrorStatus(c, http.StatusInternalServerError, err)
		return
	}
	items := make([]adminModelResponse, 0, len(records))
	for _, record := range records {
		items = append(items, newAdminModelResponse(record))
	}
	common.ApiSuccess(c, gin.H{"items": items, "total": total, "page": pageInfo.GetPage(), "page_size": pageInfo.GetPageSize()})
}

// AdminGetModel handles GET /api/admin/v1/models/:id.
func AdminGetModel(c *gin.Context) {
	id, ok := adminModelID(c)
	if !ok {
		return
	}
	record, err := model.GetExactModel(id)
	if err != nil {
		adminModelLookupError(c, err)
		return
	}
	common.ApiSuccess(c, newAdminModelResponse(record))
}

// AdminCreateModel handles POST /api/admin/v1/models.
func AdminCreateModel(c *gin.Context) {
	var request adminModelRequest
	if err := common.DecodeJson(c.Request.Body, &request); err != nil {
		common.ApiErrorStatus(c, http.StatusBadRequest, err)
		return
	}
	var record model.Model
	if err := request.declaration(&record); err != nil {
		common.ApiErrorStatus(c, http.StatusBadRequest, err)
		return
	}
	if adminModelNameTaken(c, 0, record.ModelName) {
		return
	}
	if err := record.Insert(); err != nil {
		common.ApiErrorStatus(c, http.StatusInternalServerError, err)
		return
	}
	model.RefreshPricing()
	recordManageAudit(c, "model.create", map[string]any{"id": record.Id, "model_name": record.ModelName})
	common.ApiSuccess(c, newAdminModelResponse(&record))
}

// AdminUpdateModel handles PUT /api/admin/v1/models/:id.
func AdminUpdateModel(c *gin.Context) {
	id, ok := adminModelID(c)
	if !ok {
		return
	}
	var request adminModelRequest
	if err := common.DecodeJson(c.Request.Body, &request); err != nil {
		common.ApiErrorStatus(c, http.StatusBadRequest, err)
		return
	}
	record, err := model.GetExactModel(id)
	if err != nil {
		adminModelLookupError(c, err)
		return
	}
	if err := request.declaration(record); err != nil {
		common.ApiErrorStatus(c, http.StatusBadRequest, err)
		return
	}
	if adminModelNameTaken(c, id, record.ModelName) {
		return
	}
	if err := record.UpdateDeclaration(); err != nil {
		common.ApiErrorStatus(c, http.StatusInternalServerError, err)
		return
	}
	model.RefreshPricing()
	recordManageAudit(c, "model.update", map[string]any{"id": record.Id, "model_name": record.ModelName})
	common.ApiSuccess(c, newAdminModelResponse(record))
}

// AdminDeleteModel handles DELETE /api/admin/v1/models/:id. Channel model
// lists and prices are left unchanged.
func AdminDeleteModel(c *gin.Context) {
	id, ok := adminModelID(c)
	if !ok {
		return
	}
	record, err := model.GetExactModel(id)
	if err != nil {
		adminModelLookupError(c, err)
		return
	}
	if _, err := model.DeleteModelMetadata([]int{id}, false, false); err != nil {
		common.ApiErrorStatus(c, http.StatusInternalServerError, err)
		return
	}
	recordManageAudit(c, "model.delete", map[string]any{"model_ids": []int{id}, "model_name": record.ModelName})
	common.ApiSuccess(c, gin.H{"id": id})
}
