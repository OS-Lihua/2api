package controller

import (
	"encoding/csv"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
)

const maxBulkImportBytes = 2 << 20

func BulkImport(c *gin.Context) {
	dryRun := false
	if value, exists := c.GetQuery("dry_run"); exists {
		if value != "true" && value != "false" {
			common.ApiErrorStatus(c, 400, common.NewMessage("Invalid parameters"))
			return
		}
		dryRun = value == "true"
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxBulkImportBytes))
	if err != nil {
		common.ApiErrorStatus(c, 413, common.NewMessage("Import exceeds the upload limit"))
		return
	}
	var rows []model.BulkImportRow
	switch c.ContentType() {
	case "application/json":
		var request struct {
			Rows []model.BulkImportRow `json:"rows"`
		}
		err = common.DecodeJsonStrict(strings.NewReader(string(body)), &request)
		rows = request.Rows
	case "text/csv":
		reader := csv.NewReader(strings.NewReader(string(body)))
		var columns []string
		columns, err = reader.Read()
		seen := map[string]bool{}
		for _, column := range columns {
			if seen[column] || (column != "resource" && column != "action" && column != "ref" && column != "data") {
				err = io.ErrUnexpectedEOF
			}
			seen[column] = true
		}
		if !seen["resource"] || !seen["action"] || !seen["data"] {
			err = io.ErrUnexpectedEOF
		}
		for err == nil {
			var fields []string
			fields, err = reader.Read()
			if err == io.EOF {
				err = nil
				break
			}
			if err != nil {
				break
			}
			line, _ := reader.FieldPos(0)
			row := model.BulkImportRow{Line: line}
			for i, column := range columns {
				switch column {
				case "resource":
					row.Resource = fields[i]
				case "action":
					row.Action = fields[i]
				case "ref":
					row.Ref = fields[i]
				case "data":
					row.Data = common.RawMessage(fields[i])
				}
			}
			rows = append(rows, row)
			if len(rows) > model.MaxBulkImportRows {
				err = io.ErrUnexpectedEOF
			}
		}
	default:
		common.ApiErrorStatus(c, 415, common.NewMessage("Use JSON or CSV for bulk import"))
		return
	}
	if err != nil || len(rows) == 0 || len(rows) > model.MaxBulkImportRows {
		line := 1
		var csvError *csv.ParseError
		if errors.As(err, &csvError) {
			line = csvError.Line
		}
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid import file", "message_key": "Invalid import file", "data": model.BulkImportResult{DryRun: dryRun, Rows: []model.BulkImportPreview{{Row: line, Error: "Invalid import file"}}}})
		return
	}
	requested := &model.BulkImportResult{DryRun: dryRun, Rows: make([]model.BulkImportPreview, len(rows))}
	for i, row := range rows {
		line := row.Line
		if line == 0 {
			line = i + 1
		}
		requested.Rows[i] = model.BulkImportPreview{Row: line, Resource: row.Resource, Action: row.Action, Error: "not validated"}
	}
	middleware.SetBulkImportAudit(c, requested)
	for _, row := range rows {
		expectedResource, expectedAction := "", ""
		switch c.FullPath() {
		case "/api/admin/v1/providers/import":
			expectedResource, expectedAction = "providers", "create"
		case "/api/admin/v1/channels/import":
			expectedResource, expectedAction = "channels", "create"
		case "/api/admin/v1/price-versions/import":
			expectedResource, expectedAction = "price-versions", "create"
		case "/api/admin/v1/channels/bulk-confirm":
			expectedResource, expectedAction = "channels", "confirm"
		case "/api/admin/v1/channels/bulk-enable":
			expectedResource, expectedAction = "channels", "enable"
		}
		if expectedResource != "" && (row.Resource != expectedResource || row.Action != expectedAction) {
			common.ApiErrorStatus(c, 400, common.NewMessage("Invalid import row"))
			return
		}
		permission := authz.ChannelWrite
		plugin := false
		switch row.Resource {
		case "providers":
			permission = authz.ChannelSensitiveWrite
			var input model.BulkProviderInput
			if common.DecodeJsonStrict(strings.NewReader(string(row.Data)), &input) == nil {
				var setting dto.ProviderSettings
				if input.Setting != nil {
					_ = common.UnmarshalJsonStr(*input.Setting, &setting)
				}
				plugin = input.Type == constant.ChannelTypeTaskPlugin || len(setting.TaskPluginBindings()) > 0
			}
		case "price-versions":
			permission = authz.PricingWrite
		case "channels":
			if row.Action == "confirm" || row.Action == "enable" {
				permission = authz.ChannelOperate
			}
			var input model.BulkChannelInput
			if common.DecodeJsonStrict(strings.NewReader(string(row.Data)), &input) == nil {
				_, plugin = input.Protocol.TaskPluginKey()
			}
		}
		permissions := []authz.Permission{permission}
		if plugin {
			permissions = append(permissions, authz.TaskPluginBind)
		}
		for _, required := range permissions {
			if !authz.Can(c.GetInt("id"), c.GetInt("role"), required) || (c.GetBool("use_access_token") && !service.AccessTokenScopeGranted(c.GetStringSlice("access_token_scopes"), service.AccessTokenScopeOf(required))) {
				common.ApiErrorStatus(c, 403, common.NewMessage("No permission to perform this action"))
				return
			}
		}
	}
	recordManageAudit(c, "bulk_import.execute", map[string]any{"count": len(rows), "dry_run": dryRun})
	approval := model.BulkMarginApproval{Revision: c.GetHeader("X-Margin-Confirmation")}
	if text := c.GetHeader("X-Margin-Effective-At"); text != "" {
		at, err := strconv.ParseInt(text, 10, 64)
		if err != nil || at <= 0 || at > model.MaxPriceEffectiveAt {
			common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
			return
		}
		approval.EffectiveAt = at
	}
	if approval.Revision != "" && len(approval.Revision) != 64 {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
		return
	}
	result, err := model.ImportBatch(c.Request.Context(), rows, dryRun, c.GetInt("id"), approval)
	if result != nil {
		middleware.SetBulkImportAudit(c, result)
	}
	if err != nil {
		var marginError *model.MarginConfirmationError
		if errors.As(err, &marginError) {
			pricingError(c, err)
			return
		}
		common.ApiErrorStatus(c, 409, common.NewMessage("Bulk import could not be committed"))
		return
	}
	if !result.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "data": result})
		return
	}
	if !dryRun {
		model.InitProviderCache()
	}
	common.ApiSuccess(c, result)
}
