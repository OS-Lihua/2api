package controller

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

func modelOfferingError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	message := common.NewMessage("Failed to save ModelOffering")
	var validation *common.Message
	var databaseError *pgconn.PgError
	switch {
	case errors.As(err, &validation):
		status, message = http.StatusBadRequest, validation
	case errors.Is(err, gorm.ErrRecordNotFound):
		status, message = http.StatusNotFound, common.NewMessage("ModelOffering not found")
	case errors.As(err, &databaseError):
		switch databaseError.Code {
		case "23505":
			status, message = http.StatusConflict, common.NewMessage("ModelOffering name or alias already exists")
		case "23503", "23001":
			status, message = http.StatusConflict, common.NewMessage("The ModelOffering is still referenced")
		}
	}
	common.ApiErrorStatus(c, status, message)
}

func modelOfferingID(c *gin.Context) (int, bool) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		modelOfferingError(c, gorm.ErrRecordNotFound)
		return 0, false
	}
	return id, true
}

func ListModelOfferings(c *gin.Context) {
	query := model.DB.WithContext(c.Request.Context()).Model(&model.ModelOffering{})
	if keyword := strings.TrimSpace(c.Query("keyword")); keyword != "" {
		names := model.DB.Model(&model.ModelOfferingName{}).Select("offering_id").Where("name ILIKE ?", "%"+keyword+"%")
		query = query.Where("id IN (?)", names)
	}
	page := common.GetPageQuery(c)
	var total int64
	if err := query.Count(&total).Error; err != nil {
		modelOfferingError(c, err)
		return
	}
	records := []model.ModelOffering{}
	if err := query.Preload("Names", func(tx *gorm.DB) *gorm.DB { return tx.Order("name") }).Preload("Members", func(tx *gorm.DB) *gorm.DB { return tx.Order("tier, model_id") }).Order("id").Offset(page.GetStartIdx()).Limit(page.GetPageSize()).Find(&records).Error; err != nil {
		modelOfferingError(c, err)
		return
	}
	for i := range records {
		records[i].Aliases = []string{}
		for _, name := range records[i].Names {
			if name.Name != records[i].Name {
				records[i].Aliases = append(records[i].Aliases, name.Name)
			}
		}
	}
	common.ApiSuccess(c, gin.H{"items": records, "total": total, "page": page.GetPage(), "page_size": page.GetPageSize()})
}

func GetModelOffering(c *gin.Context) {
	id, ok := modelOfferingID(c)
	if !ok {
		return
	}
	record, err := model.GetModelOffering(c.Request.Context(), id)
	if err != nil {
		modelOfferingError(c, err)
		return
	}
	common.ApiSuccess(c, record)
}

func mutateModelOffering(c *gin.Context, action string) {
	id := 0
	if c.Param("id") != "" {
		var ok bool
		id, ok = modelOfferingID(c)
		if !ok {
			return
		}
	}
	var record model.ModelOffering
	if action == "save" {
		if err := common.DecodeJson(c.Request.Body, &record); err != nil {
			common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
			return
		}
	}
	// The route chooses identity. Status and timestamps are assigned from the
	// locked declaration, so request JSON cannot bypass the activation checks.
	record.ID = id
	if err := model.SaveModelOffering(c.Request.Context(), &record, action); err != nil {
		modelOfferingError(c, err)
		return
	}
	auditAction := "model_offering." + action
	if action == "save" {
		auditAction = "model_offering.update"
		if id == 0 {
			auditAction = "model_offering.create"
		}
	}
	recordManageAudit(c, auditAction, map[string]any{"id": record.ID, "name": record.Name})
	// Read the canonical ordering of aliases and members for every mutation.
	saved, err := model.GetModelOffering(c.Request.Context(), record.ID)
	if err != nil {
		modelOfferingError(c, err)
		return
	}
	common.ApiSuccess(c, saved)
}

func SaveModelOffering(c *gin.Context)    { mutateModelOffering(c, "save") }
func EnableModelOffering(c *gin.Context)  { mutateModelOffering(c, "enable") }
func DisableModelOffering(c *gin.Context) { mutateModelOffering(c, "disable") }

func DeleteModelOffering(c *gin.Context) {
	id, ok := modelOfferingID(c)
	if !ok {
		return
	}
	result := model.DB.WithContext(c.Request.Context()).Delete(&model.ModelOffering{}, id)
	if result.Error != nil {
		modelOfferingError(c, result.Error)
		return
	}
	if result.RowsAffected == 0 {
		modelOfferingError(c, gorm.ErrRecordNotFound)
		return
	}
	recordManageAudit(c, "model_offering.delete", map[string]any{"id": id})
	common.ApiSuccess(c, gin.H{"id": id})
}
