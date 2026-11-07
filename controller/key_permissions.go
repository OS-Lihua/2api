package controller

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func keyPermissionsError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	message := common.NewMessage("Failed to update token permissions")
	var validation *common.Message
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		status, message = http.StatusNotFound, common.NewMessage("Token not found")
	case errors.Is(err, model.ErrKeyPermissionsConflict):
		status, message = http.StatusConflict, common.NewMessage("Token permissions changed. Reload and try again")
	case errors.As(err, &validation):
		status, message = http.StatusBadRequest, validation
	}
	common.ApiErrorStatus(c, status, message)
}

func UpdateKeyPermissions(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		keyPermissionsError(c, gorm.ErrRecordNotFound)
		return
	}
	var input struct {
		DefaultAllow *bool  `json:"default_allow"`
		ExceptionIDs []int  `json:"exception_ids"`
		Revision     *int64 `json:"revision"`
		Action       string `json:"action"`
		OfferingIDs  []int  `json:"offering_ids"`
	}
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 65536), &input); err != nil || input.Revision == nil || *input.Revision < 0 || len(input.OfferingIDs) > 1000 {
		keyPermissionsError(c, common.NewMessage("Invalid parameters"))
		return
	}
	action := input.Action
	replacement := model.KeyPermissions{ExceptionIDs: input.ExceptionIDs}
	if c.Request.Method == http.MethodPut {
		if input.DefaultAllow == nil {
			keyPermissionsError(c, common.NewMessage("Invalid parameters"))
			return
		}
		action = "replace"
		replacement.DefaultAllow = *input.DefaultAllow
	}
	permissions, err := model.MutateKeyPermissions(c.Request.Context(), id, c.GetInt("id"), *input.Revision, action, replacement, input.OfferingIDs)
	if err != nil {
		keyPermissionsError(c, err)
		return
	}
	recordManageAudit(c, "token.permissions.update", map[string]any{"id": id, "revision": permissions.Revision, "default_allow": permissions.DefaultAllow, "exception_count": len(permissions.ExceptionIDs)})
	common.ApiSuccess(c, permissions)
}

func GetKeyPermissions(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		common.ApiErrorStatus(c, http.StatusNotFound, common.NewMessage("Token not found"))
		return
	}
	permissions, err := model.GetKeyPermissions(c.Request.Context(), id, c.GetInt("id"))
	if errors.Is(err, gorm.ErrRecordNotFound) {
		common.ApiErrorStatus(c, http.StatusNotFound, common.NewMessage("Token not found"))
		return
	}
	if err != nil {
		common.ApiErrorStatus(c, http.StatusInternalServerError, common.NewMessage("Failed to get token permissions"))
		return
	}
	common.ApiSuccess(c, permissions)
}
