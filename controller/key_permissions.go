package controller

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func keyPermissionsOwner(c *gin.Context, id int) (int, error) {
	if !strings.HasPrefix(c.FullPath(), "/api/admin/v1/") {
		return c.GetInt("id"), nil
	}
	token, err := model.GetTokenById(id)
	if err != nil {
		return 0, err
	}
	var owner model.User
	if err := model.DB.WithContext(c.Request.Context()).Select("id", "role").First(&owner, token.UserId).Error; err != nil {
		return 0, err
	}
	if owner.Id != c.GetInt("id") && owner.Role >= c.GetInt("role") {
		return 0, gorm.ErrRecordNotFound
	}
	return token.UserId, nil
}

func ListKeyOfferings(c *gin.Context) {
	var user model.User
	if err := model.DB.WithContext(c.Request.Context()).Select("id", "group").First(&user, c.GetInt("id")).Error; err != nil {
		keyPermissionsError(c, err)
		return
	}
	choices := []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}{}
	if err := model.DB.WithContext(c.Request.Context()).Model(&model.ModelOffering{}).Select("id", "name").Where(`"visibility" = 'all' OR "groups" @> jsonb_build_array(?)`, user.Group).Order("name").Find(&choices).Error; err != nil {
		keyPermissionsError(c, err)
		return
	}
	common.ApiSuccess(c, choices)
}

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
	for _, id := range input.OfferingIDs {
		if id <= 0 {
			keyPermissionsError(c, common.NewMessage("Invalid parameters"))
			return
		}
	}
	action := input.Action
	if c.Request.Method == http.MethodPatch && action != "allow_all" && action != "deny_all" && action != "add" && action != "remove" {
		keyPermissionsError(c, common.NewMessage("Invalid parameters"))
		return
	}
	replacement := model.KeyPermissions{ExceptionIDs: input.ExceptionIDs}
	if c.Request.Method == http.MethodPut {
		if input.DefaultAllow == nil {
			keyPermissionsError(c, common.NewMessage("Invalid parameters"))
			return
		}
		action = "replace"
		replacement.DefaultAllow = *input.DefaultAllow
	}
	owner, err := keyPermissionsOwner(c, id)
	if err != nil {
		keyPermissionsError(c, err)
		return
	}
	permissions, err := model.MutateKeyPermissions(c.Request.Context(), id, owner, *input.Revision, action, replacement, input.OfferingIDs)
	if err != nil {
		keyPermissionsError(c, err)
		return
	}
	if strings.HasPrefix(c.FullPath(), "/api/admin/v1/") {
		recordManageAudit(c, "token.permissions.update", map[string]any{"id": id, "revision": permissions.Revision, "default_allow": permissions.DefaultAllow, "exception_count": len(permissions.ExceptionIDs)})
	} else {
		params := tokenAuditParams(c)
		params["id"], params["revision"], params["default_allow"], params["exception_count"] = id, permissions.Revision, permissions.DefaultAllow, len(permissions.ExceptionIDs)
		common.SetContextKey(c, constant.ContextKeyTokenAuditSucceeded, true)
	}
	common.ApiSuccess(c, permissions)
}

func GetKeyPermissions(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		common.ApiErrorStatus(c, http.StatusNotFound, common.NewMessage("Token not found"))
		return
	}
	owner, err := keyPermissionsOwner(c, id)
	if err != nil {
		keyPermissionsError(c, err)
		return
	}
	permissions, err := model.GetKeyPermissions(c.Request.Context(), id, owner)
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
