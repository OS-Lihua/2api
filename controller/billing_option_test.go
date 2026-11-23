package controller

import (
	"fmt"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/shopspring/decimal"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/pkg/testdb"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocalQuotaUnitIsReadOnlyHistoricalMetadata(t *testing.T) {
	for _, mode := range []string{"projection", "http", "internal", "bulk", "startup", "startup_missing", "startup_malformed", "startup_zero"} {
		t.Run(mode, func(t *testing.T) {
			db := setupManageUserTestDB(t)
			previousUnit, previousTopup := common.QuotaPerUnit, common.TopUpLink
			previousConfig := config.GlobalConfig.ExportAllConfigs()
			common.OptionMapRWMutex.Lock()
			previousOptions := common.OptionMap
			common.OptionMap = map[string]string{"TopUpLink": "https://example.invalid/original"}
			expectedUnit := "1000000.000"
			switch mode {
			case "startup_missing":
				expectedUnit = "500000"
			case "startup_malformed":
				expectedUnit = "NaN"
			case "startup_zero":
				expectedUnit = "0"
			}
			if mode != "startup_missing" {
				common.OptionMap["QuotaPerUnit"] = expectedUnit
			}
			beforeMap := maps.Clone(common.OptionMap)
			common.OptionMapRWMutex.Unlock()
			common.QuotaPerUnit, common.TopUpLink = 500000, beforeMap["TopUpLink"]
			t.Cleanup(func() {
				common.OptionMapRWMutex.Lock()
				common.OptionMap = previousOptions
				common.OptionMapRWMutex.Unlock()
				common.QuotaPerUnit, common.TopUpLink = previousUnit, previousTopup
				require.NoError(t, config.GlobalConfig.LoadFromDB(previousConfig))
			})
			for key, value := range beforeMap {
				require.NoError(t, db.Create(&model.Option{Key: key, Value: value}).Error)
			}
			var before []model.Option
			require.NoError(t, db.Order("key").Find(&before).Error)
			operator := model.User{Id: 1, Role: common.RoleRootUser}
			switch mode {
			case "projection":
				response := reminderSettingsHTTP(t, GetOptions, http.MethodGet, "/api/option/", "", operator)
				var result struct {
					Success bool
					Data    []model.Option
				}
				require.Equal(t, http.StatusOK, response.Code)
				require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
				require.True(t, result.Success)
				projected := make(map[string]string)
				for _, option := range result.Data {
					projected[option.Key] = option.Value
				}
				assert.NotContains(t, projected, "QuotaPerUnit")
				assert.Equal(t, beforeMap["TopUpLink"], projected["TopUpLink"])
			case "http":
				for _, key := range []string{"QuotaPerUnit", "QUOTAPERUNIT", "quotaperunit"} {
					for _, value := range []any{"2000000", 0, nil, map[string]any{"amount": 1}} {
						body, err := common.Marshal(OptionUpdateRequest{Key: key, Value: value})
						require.NoError(t, err)
						response := reminderSettingsHTTP(t, UpdateOption, http.MethodPut, "/api/option/", string(body), operator)
						assert.Equal(t, http.StatusGone, response.Code, key)
						assert.Equal(t, "no-store", response.Header().Get("Cache-Control"), key)
						var result struct {
							Success bool
							Code    string
						}
						require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
						assert.False(t, result.Success)
						assert.Equal(t, "legacy_quota_settings_retired", result.Code)
					}
				}
			case "internal", "bulk":
				for _, key := range []string{"QuotaPerUnit", "QUOTAPERUNIT", "quotaperunit"} {
					if mode == "internal" {
						assert.ErrorContains(t, model.UpdateOption(key, "2000000"), operation_setting.RetiredQuotaSettingsMessage)
					} else {
						assert.ErrorContains(t, model.UpdateOptionsBulk(map[string]string{key: "2000000", "TopUpLink": "https://example.invalid/changed"}), operation_setting.RetiredQuotaSettingsMessage)
					}
				}
			default:
				for range 2 {
					model.InitOptionMap()
					assert.Equal(t, float64(500000), common.QuotaPerUnit, "loading source metadata must not mutate the old runtime divisor")
				}
			}
			var after []model.Option
			require.NoError(t, db.Order("key").Find(&after).Error)
			assert.Equal(t, before, after, "retirement preserves source bytes and rejects mixed writes atomically")
			common.OptionMapRWMutex.RLock()
			afterMap := maps.Clone(common.OptionMap)
			common.OptionMapRWMutex.RUnlock()
			if strings.HasPrefix(mode, "startup") {
				if mode == "startup_missing" {
					assert.NotContains(t, afterMap, "QuotaPerUnit", "fresh options cannot advertise an editable legacy unit")
				} else {
					assert.Equal(t, expectedUnit, afterMap["QuotaPerUnit"])
				}
			} else {
				assert.Equal(t, beforeMap, afterMap)
			}
			assert.Equal(t, float64(500000), common.QuotaPerUnit)
			assert.Equal(t, beforeMap["TopUpLink"], common.TopUpLink)
			response := reminderSettingsHTTP(t, GetStatus, http.MethodGet, "/api/status", "", operator)
			var status struct {
				Success bool
				Data    map[string]common.RawMessage
			}
			require.Equal(t, http.StatusOK, response.Code)
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &status))
			require.True(t, status.Success)
			_, liveUnit := status.Data["quota_per_unit"]
			assert.False(t, liveUnit, "status cannot advertise a live quota unit")
			rawDisplaySource, exists := status.Data["legacy_quota_per_unit"]
			require.True(t, exists, "historical display metadata must remain available")
			var displaySource string
			require.NoError(t, common.Unmarshal(rawDisplaySource, &displaySource))
			assert.Equal(t, expectedUnit, displaySource, "only historical display reads the original source, including invalid text")
			require.NoError(t, model.UpdateOption("TopUpLink", "https://example.invalid/active"))
			assert.Equal(t, "https://example.invalid/active", common.TopUpLink)
		})
	}
}

func TestLegacyAutoGroupOptionsAreRetired(t *testing.T) {
	db := setupManageUserTestDB(t)
	historical := map[string]string{
		"AutoGroups": `["vip","default"]`, "DefaultUseAutoGroup": "true", "MaxTokenAutoGroups": "5", "maxtokenautogroups": "4",
		"GroupRatio": `{"vip":0.875,"default":1}`, "GroupGroupRatio": `{"vip":{"vip":0.75}}`,
	}
	var before []model.Option
	for key, value := range historical {
		require.NoError(t, db.Create(&model.Option{Key: key, Value: value}).Error)
	}
	require.NoError(t, db.Order("key").Find(&before).Error)
	common.OptionMapRWMutex.Lock()
	previousOptions := common.OptionMap
	common.OptionMap = maps.Clone(historical)
	common.OptionMapRWMutex.Unlock()
	previousTopUp := common.TopUpLink
	previousDefault, previousMax, previousGroups := setting.DefaultUseAutoGroup, setting.GetMaxTokenAutoGroups(), setting.AutoGroups2JsonString()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptions
		common.OptionMapRWMutex.Unlock()
		common.TopUpLink = previousTopUp
		setting.DefaultUseAutoGroup = previousDefault
		require.NoError(t, setting.UpdateMaxTokenAutoGroups(strconv.Itoa(previousMax)))
		require.NoError(t, setting.UpdateAutoGroupsByJsonString(previousGroups))
	})

	actor := model.User{Id: 1, Role: common.RoleRootUser}
	response := reminderSettingsHTTP(t, GetOptions, http.MethodGet, "/api/option/", "", actor)
	var listed struct {
		Success bool
		Data    []model.Option
	}
	require.Equal(t, http.StatusOK, response.Code)
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &listed))
	require.True(t, listed.Success)
	projected := make(map[string]string)
	for _, option := range listed.Data {
		projected[option.Key] = option.Value
	}
	assert.Equal(t, historical["GroupRatio"], projected["GroupRatio"])
	assert.Equal(t, historical["GroupGroupRatio"], projected["GroupGroupRatio"])

	for _, key := range []string{"AutoGroups", "DefaultUseAutoGroup", "MaxTokenAutoGroups", "maxtokenautogroups"} {
		t.Run(key, func(t *testing.T) {
			assert.NotContains(t, projected, key)
			body, err := common.Marshal(OptionUpdateRequest{Key: key, Value: "7"})
			require.NoError(t, err)
			response := reminderSettingsHTTP(t, UpdateOption, http.MethodPut, "/api/option/", string(body), actor)
			assert.Equal(t, http.StatusGone, response.Code)
			assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			var result struct {
				Success bool
				Code    string
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
			assert.False(t, result.Success)
			assert.Equal(t, "legacy_auto_group_settings_retired", result.Code)
			assert.Error(t, model.UpdateOption(key, "7"))
		})
	}
	assert.Error(t, model.UpdateOptionsBulk(map[string]string{
		"TopUpLink": "https://example.invalid/changed", "MaxTokenAutoGroups": "9",
	}))
	var after []model.Option
	require.NoError(t, db.Order("key").Find(&after).Error)
	assert.Equal(t, before, after, "retirement preserves historical rows and rejects mixed writes atomically")
	common.OptionMapRWMutex.RLock()
	afterMap := maps.Clone(common.OptionMap)
	common.OptionMapRWMutex.RUnlock()
	assert.Equal(t, historical, afterMap)
	discounts, err := model.ExactGroupDiscounts([]string{"vip", "default"})
	require.NoError(t, err)
	assert.Equal(t, "0.75", discounts["vip"].String(), "own-group sale discount still takes precedence")
	assert.Equal(t, "1", discounts["default"].String())

	response = reminderSettingsHTTP(t, GetStatus, http.MethodGet, "/api/status", "", actor)
	var status struct {
		Success bool
		Data    map[string]common.RawMessage
	}
	require.Equal(t, http.StatusOK, response.Code)
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &status))
	require.True(t, status.Success)
	assert.NotContains(t, status.Data, "default_use_auto_group")
}

func TestLegacyStatisticsSettingsAreRetired(t *testing.T) {
	for _, mode := range []string{"projection", "status", "http", "internal", "bulk", "active"} {
		t.Run(mode, func(t *testing.T) {
			db := setupManageUserTestDB(t)
			previousTopup := common.TopUpLink
			common.OptionMapRWMutex.Lock()
			previousOptions := common.OptionMap
			common.OptionMap = map[string]string{
				"DataExportEnabled": "true", "DataExportInterval": "5", "DataExportDefaultTime": "hour", "TopUpLink": "https://example.invalid/original",
			}
			beforeMap := maps.Clone(common.OptionMap)
			common.OptionMapRWMutex.Unlock()
			common.TopUpLink = beforeMap["TopUpLink"]
			t.Cleanup(func() {
				common.OptionMapRWMutex.Lock()
				common.OptionMap = previousOptions
				common.OptionMapRWMutex.Unlock()
				common.TopUpLink = previousTopup
			})
			for key, value := range beforeMap {
				require.NoError(t, db.Create(&model.Option{Key: key, Value: value}).Error)
			}
			var before []model.Option
			require.NoError(t, db.Order("key").Find(&before).Error)
			operator := model.User{Id: 1, Role: common.RoleRootUser}
			retired := []string{"DataExportEnabled", "DataExportInterval", "DataExportDefaultTime"}
			switch mode {
			case "projection":
				response := reminderSettingsHTTP(t, GetOptions, http.MethodGet, "/api/option/", "", operator)
				var result struct {
					Success bool
					Data    []model.Option
				}
				require.Equal(t, http.StatusOK, response.Code)
				require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
				require.True(t, result.Success)
				projected := make(map[string]string)
				for _, option := range result.Data {
					projected[option.Key] = option.Value
				}
				for _, key := range retired {
					assert.NotContains(t, projected, key)
				}
				assert.Equal(t, beforeMap["TopUpLink"], projected["TopUpLink"])
			case "status":
				response := reminderSettingsHTTP(t, GetStatus, http.MethodGet, "/api/status", "", operator)
				var result struct {
					Success bool
					Data    map[string]common.RawMessage
				}
				require.Equal(t, http.StatusOK, response.Code)
				require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
				require.True(t, result.Success)
				assert.NotContains(t, result.Data, "enable_data_export")
				assert.NotContains(t, result.Data, "data_export_default_time")
				assert.NotContains(t, result.Data, "enable_batch_update")
				assert.Contains(t, result.Data, "log_export_enabled", "historical log export remains independently available")
				assert.Contains(t, result.Data, "faq_enabled")
			case "http":
				for _, key := range append(retired, "DATAEXPORTENABLED") {
					for _, value := range []any{"9", nil} {
						body, err := common.Marshal(OptionUpdateRequest{Key: key, Value: value})
						require.NoError(t, err)
						response := reminderSettingsHTTP(t, UpdateOption, http.MethodPut, "/api/option/", string(body), operator)
						assert.Equal(t, http.StatusGone, response.Code, key)
						assert.Equal(t, "no-store", response.Header().Get("Cache-Control"), key)
						var result struct {
							Success bool
							Code    string
						}
						require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
						assert.False(t, result.Success)
						assert.Equal(t, "legacy_statistics_settings_retired", result.Code)
					}
				}
			case "internal":
				for _, key := range retired {
					assert.Error(t, model.UpdateOption(key, "9"), key)
				}
			case "bulk":
				assert.Error(t, model.UpdateOptionsBulk(map[string]string{"TopUpLink": "https://example.invalid/changed", "DataExportInterval": "9"}))
			case "active":
				body, err := common.Marshal(OptionUpdateRequest{Key: "TopUpLink", Value: "https://example.invalid/active"})
				require.NoError(t, err)
				response := reminderSettingsHTTP(t, UpdateOption, http.MethodPut, "/api/option/", string(body), operator)
				require.Equal(t, http.StatusOK, response.Code)
				var result struct{ Success bool }
				require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
				require.True(t, result.Success)
				beforeMap["TopUpLink"] = "https://example.invalid/active"
				for i := range before {
					if before[i].Key == "TopUpLink" {
						before[i].Value = beforeMap["TopUpLink"]
					}
				}
			}
			var after []model.Option
			require.NoError(t, db.Order("key").Find(&after).Error)
			assert.Equal(t, before, after, "retirement preserves historical source and rejects mixed writes atomically")
			common.OptionMapRWMutex.RLock()
			afterMap := maps.Clone(common.OptionMap)
			common.OptionMapRWMutex.RUnlock()
			assert.Equal(t, beforeMap, afterMap)
			assert.Equal(t, beforeMap["TopUpLink"], common.TopUpLink)
		})
	}
}

func TestGetOptionsOmitsRetiredPricingWithoutChangingLegacySources(t *testing.T) {
	retiredKeys := []string{
		"ModelPrice", "ModelRatio", "CompletionRatio", "CacheRatio", "CreateCacheRatio",
		"ImageRatio", "AudioRatio", "AudioCompletionRatio", "billing_setting.billing_mode",
		"billing_setting.billing_expr", "billing_setting.plugin_billing_expr", "CompletionRatioMeta", "ExposeRatioEnabled", operation_setting.ToolPriceOptionKey,
	}
	for _, configured := range []bool{false, true} {
		t.Run(fmt.Sprintf("configured_%t", configured), func(t *testing.T) {
			db := modelManagementDB(t)
			values := map[string]string{
				"GatewayMaxAttempts":                "3",
				"global.accepted_conversion_losses": "[]",
			}
			if configured {
				for _, key := range retiredKeys {
					values[key] = `{"legacy-zero":0,"legacy-priced":1.25}`
				}
				values["billing_setting.billing_mode"] = `{"legacy-zero":"tiered_expr","legacy-priced":"tiered_expr"}`
				values["billing_setting.billing_expr"] = `{"legacy-zero":"0","legacy-priced":"p * 0.125"}`
				values["billing_setting.plugin_billing_expr"] = `{}`
				values["ExposeRatioEnabled"] = "true"
				config.UpdateConfigFromMap(config.GlobalConfig.Get("billing_setting"), map[string]string{
					"billing_mode":        values["billing_setting.billing_mode"],
					"billing_expr":        values["billing_setting.billing_expr"],
					"plugin_billing_expr": values["billing_setting.plugin_billing_expr"],
				})
			}
			for key, value := range values {
				require.NoError(t, db.Create(&model.Option{Key: key, Value: value}).Error)
			}
			common.OptionMapRWMutex.Lock()
			common.OptionMap = maps.Clone(values)
			common.OptionMapRWMutex.Unlock()
			beforeConfig := config.GlobalConfig.ExportAllConfigs()
			var before []model.Option
			require.NoError(t, db.Order("key").Find(&before).Error)

			for range 2 {
				var result struct {
					Success bool `json:"success"`
					Data    []struct {
						Key   string `json:"key"`
						Value string `json:"value"`
					} `json:"data"`
				}
				response := modelManagementRequest(t, GetOptions, http.MethodGet, "/api/option/", nil, &result)
				require.Equal(t, http.StatusOK, response.Code)
				require.True(t, result.Success)
				options := make(map[string]string, len(result.Data))
				for _, option := range result.Data {
					assert.NotContains(t, options, option.Key, "option keys must be unique")
					options[option.Key] = option.Value
				}
				for _, key := range retiredKeys {
					assert.NotContains(t, options, key)
				}
				assert.Equal(t, "3", options["GatewayMaxAttempts"])
				assert.Equal(t, "[]", options["global.accepted_conversion_losses"])
			}

			var after []model.Option
			require.NoError(t, db.Order("key").Find(&after).Error)
			assert.Equal(t, before, after, "HTTP projection must preserve the migration source")
			common.OptionMapRWMutex.RLock()
			afterOptions := maps.Clone(common.OptionMap)
			common.OptionMapRWMutex.RUnlock()
			assert.Equal(t, values, afterOptions)
			assert.Equal(t, beforeConfig, config.GlobalConfig.ExportAllConfigs())
		})
	}
}

func TestLegacyModelPricingOptionWritesAreRetired(t *testing.T) {
	keys := []string{
		"ModelPrice", "ModelRatio", "CompletionRatio", "CacheRatio", "CreateCacheRatio",
		"ImageRatio", "AudioRatio", "AudioCompletionRatio", "billing_setting.billing_mode",
		"billing_setting.billing_expr", "billing_setting.plugin_billing_expr", "ExposeRatioEnabled", operation_setting.ToolPriceOptionKey,
	}
	for _, mode := range []string{"single", "bulk", "http"} {
		t.Run(mode, func(t *testing.T) {
			db := modelManagementDB(t)
			previousTopup := common.TopUpLink
			t.Cleanup(func() { common.TopUpLink = previousTopup })
			common.TopUpLink = "https://example.invalid/original"
			values := map[string]string{"TopUpLink": common.TopUpLink}
			for _, key := range keys {
				values[key] = `{"legacy-zero":0,"legacy-paid":1.25}`
			}
			values["billing_setting.billing_mode"] = `{"legacy-zero":"tiered_expr","legacy-paid":"ratio"}`
			values["billing_setting.billing_expr"] = `{"legacy-zero":"0"}`
			values["billing_setting.plugin_billing_expr"] = `{}`
			values["ExposeRatioEnabled"] = "true"
			for key, value := range values {
				require.NoError(t, db.Create(&model.Option{Key: key, Value: value}).Error)
			}
			common.OptionMapRWMutex.Lock()
			common.OptionMap = maps.Clone(values)
			common.OptionMapRWMutex.Unlock()
			beforeConfig := config.GlobalConfig.ExportAllConfigs()
			var before []model.Option
			require.NoError(t, db.Order("key").Find(&before).Error)
			for _, key := range keys {
				for _, spelling := range []string{key, strings.ToUpper(key)} {
					switch mode {
					case "single":
						assert.ErrorContains(t, model.UpdateOption(spelling, `{}`), "Model-name pricing has been retired", spelling)
					case "bulk":
						assert.ErrorContains(t, model.UpdateOptionsBulk(map[string]string{
							spelling: `{}`, "TopUpLink": "https://example.invalid/changed",
						}), "Model-name pricing has been retired", spelling)
					case "http":
						for _, value := range []any{`{}`, nil, map[string]any{"legacy-zero": 0}} {
							var response struct{ Success bool }
							recorder := modelManagementRequest(t, UpdateOption, http.MethodPut, "/api/option/", OptionUpdateRequest{Key: spelling, Value: value}, &response)
							assert.Equal(t, http.StatusGone, recorder.Code, spelling)
							assert.Equal(t, "no-store", recorder.Header().Get("Cache-Control"), spelling)
							assert.False(t, response.Success)
						}
					}
					var after []model.Option
					require.NoError(t, db.Order("key").Find(&after).Error)
					assert.Equal(t, before, after, "rejected writes preserve every migration source row")
					common.OptionMapRWMutex.RLock()
					afterOptions := maps.Clone(common.OptionMap)
					common.OptionMapRWMutex.RUnlock()
					assert.Equal(t, values, afterOptions)
					assert.Equal(t, beforeConfig, config.GlobalConfig.ExportAllConfigs())
					assert.Equal(t, "https://example.invalid/original", common.TopUpLink)
				}
			}
			// Rejecting historical pricing never disables unrelated live settings.
			require.NoError(t, model.UpdateOptionsBulk(map[string]string{"TopUpLink": "https://example.invalid/active"}))
			assert.Equal(t, "https://example.invalid/active", common.TopUpLink)
			var link model.Option
			require.NoError(t, db.Where("key = ?", "TopUpLink").First(&link).Error)
			assert.Equal(t, common.TopUpLink, link.Value)
		})
	}
}

func TestLegacyModelPricingReloadPreservesSourceWithoutApplyingSettings(t *testing.T) {
	db := modelManagementDB(t)
	previousOptions := config.GlobalConfig.ExportAllConfigs()
	previousTopUpLink := common.TopUpLink
	t.Cleanup(func() {
		operation_setting.LoadToolPricesFromJSONString(previousOptions[operation_setting.ToolPriceOptionKey])
		common.TopUpLink = previousTopUpLink
	})
	const modelName = "old-pricing-reload-model"
	const toolName = "old-pricing-reload-tool"
	historical := map[string]string{
		"ModelPrice":                         `{"old-pricing-reload-model":1.25}`,
		"ModelRatio":                         `{"old-pricing-reload-model":0.75}`,
		"billing_setting.billing_mode":       `{"old-pricing-reload-model":"tiered_expr"}`,
		"billing_setting.billing_expr":       `{"old-pricing-reload-model":"p * 0.125"}`,
		operation_setting.ToolPriceOptionKey: `{"old-pricing-reload-tool":2.5}`,
	}
	for key, value := range historical {
		require.NoError(t, db.Create(&model.Option{Key: key, Value: value}).Error)
	}
	var before []model.Option
	require.NoError(t, db.Order("key").Find(&before).Error)
	for range 2 {
		model.InitOptionMap()
		_, hasPrice := ratio_setting.GetModelPrice(modelName, false)
		_, hasRatio, _ := ratio_setting.GetModelRatio(modelName)
		assert.False(t, hasPrice)
		assert.False(t, hasRatio)
		assert.Zero(t, operation_setting.GetToolPrice(toolName))
		loaded := config.GlobalConfig.ExportAllConfigs()
		assert.Equal(t, previousOptions["billing_setting.billing_mode"], loaded["billing_setting.billing_mode"])
		assert.Equal(t, previousOptions["billing_setting.billing_expr"], loaded["billing_setting.billing_expr"])
		common.OptionMapRWMutex.RLock()
		stored := maps.Clone(common.OptionMap)
		common.OptionMapRWMutex.RUnlock()
		for key, value := range historical {
			assert.Equal(t, value, stored[key])
		}
	}
	var after []model.Option
	require.NoError(t, db.Order("key").Find(&after).Error)
	assert.Equal(t, before, after)
}

func TestLegacyViolationFeeSettingsAreRetired(t *testing.T) {
	db := modelManagementDB(t)
	previousTopUpLink := common.TopUpLink
	t.Cleanup(func() { common.TopUpLink = previousTopUpLink })
	const topUpLink = "https://example.invalid/original"
	historical := map[string]string{
		"grok.violation_deduction_enabled": "false",
		"grok.violation_deduction_amount":  "0.125",
	}
	for key, value := range historical {
		require.NoError(t, db.Create(&model.Option{Key: key, Value: value}).Error)
	}
	require.NoError(t, db.Create(&model.Option{Key: "TopUpLink", Value: topUpLink}).Error)
	var before []model.Option
	require.NoError(t, db.Order("key").Find(&before).Error)
	for range 2 {
		model.InitOptionMap()
		loaded := config.GlobalConfig.ExportAllConfigs()
		for key, value := range historical {
			assert.NotContains(t, loaded, key, "reload must not re-enable %s", key)
			common.OptionMapRWMutex.RLock()
			stored := common.OptionMap[key]
			common.OptionMapRWMutex.RUnlock()
			assert.Equal(t, value, stored)
		}
	}
	var options struct {
		Success bool           `json:"success"`
		Data    []model.Option `json:"data"`
	}
	recorder := modelManagementRequest(t, GetOptions, http.MethodGet, "/api/option/", nil, &options)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.True(t, options.Success)
	visible := make(map[string]string, len(options.Data))
	for _, option := range options.Data {
		visible[option.Key] = option.Value
	}
	assert.Equal(t, topUpLink, visible["TopUpLink"])
	for key := range historical {
		assert.NotContains(t, visible, key)
		for _, spelling := range []string{key, strings.ToUpper(key)} {
			for _, value := range []any{nil, "0.75"} {
				var response struct {
					Success bool   `json:"success"`
					Code    string `json:"code"`
				}
				recorder := modelManagementRequest(t, UpdateOption, http.MethodPut, "/api/option/", OptionUpdateRequest{Key: spelling, Value: value}, &response)
				assert.Equal(t, http.StatusGone, recorder.Code, spelling)
				assert.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
				assert.False(t, response.Success)
				assert.Equal(t, "legacy_violation_fee_settings_retired", response.Code)
			}
			assert.ErrorContains(t, model.UpdateOption(spelling, "0.75"), "Legacy violation fee settings have been retired.")
			assert.ErrorContains(t, model.UpdateOptionsBulk(map[string]string{
				spelling: "0.75", "TopUpLink": "https://example.invalid/changed",
			}), "Legacy violation fee settings have been retired.")
		}
	}
	var after []model.Option
	require.NoError(t, db.Order("key").Find(&after).Error)
	assert.Equal(t, before, after)
	assert.Equal(t, topUpLink, common.TopUpLink)
}

func TestBalanceReminderSiteHTTPContract(t *testing.T) {
	db := setupManageUserTestDB(t)
	common.OptionMapRWMutex.Lock()
	previous := common.OptionMap
	common.OptionMap = map[string]string{"QuotaRemindEnabled": "true", "QuotaRemindThreshold": "1000", "GatewayMaxAttempts": "3", operation_setting.BalanceReminderConfigOption: `{"enabled":true,"threshold":{"nanos":"2000000","currency":"USD"}}`}
	common.OptionMapRWMutex.Unlock()
	previousConfig := config.GlobalConfig.Get("balance_reminder_setting")
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previous
		common.OptionMapRWMutex.Unlock()
		config.GlobalConfig.Register("balance_reminder_setting", previousConfig)
	})
	for key, value := range map[string]string{"QuotaRemindEnabled": "true", "QuotaRemindThreshold": "1000"} {
		require.NoError(t, db.Create(&model.Option{Key: key, Value: value}).Error)
	}
	operator := model.User{Id: 1, Role: common.RoleRootUser}
	var before []model.Option
	require.NoError(t, db.Order("key").Find(&before).Error)
	for _, key := range []string{"QuotaRemindEnabled", "QuotaRemindThreshold", "QUOTAREMINDENABLED", "QuotaRemindThreſhold"} {
		for _, value := range []any{nil, true, "2000"} {
			body, err := common.Marshal(OptionUpdateRequest{Key: key, Value: value})
			require.NoError(t, err)
			response := reminderSettingsHTTP(t, UpdateOption, http.MethodPut, "/api/option/", string(body), operator)
			assert.Equal(t, http.StatusGone, response.Code)
			assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
			var result struct {
				Success bool
				Code    string
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
			assert.False(t, result.Success)
			assert.Equal(t, "legacy_quota_reminder_settings_retired", result.Code)
		}
	}
	var after []model.Option
	require.NoError(t, db.Order("key").Find(&after).Error)
	assert.Equal(t, before, after)
	response := reminderSettingsHTTP(t, GetOptions, http.MethodGet, "/api/option/", "", operator)
	var options struct {
		Success bool
		Data    []model.Option
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &options))
	require.True(t, options.Success)
	visible := map[string]string{}
	for _, option := range options.Data {
		visible[option.Key] = option.Value
	}
	assert.NotContains(t, visible, "QuotaRemindEnabled")
	assert.NotContains(t, visible, "QuotaRemindThreshold")
	assert.Equal(t, "3", visible["GatewayMaxAttempts"])
	assert.Contains(t, visible, operation_setting.BalanceReminderConfigOption)
	response = reminderSettingsHTTP(t, GetStatus, http.MethodGet, "/api/status", "", operator)
	var status struct {
		Success bool
		Data    map[string]common.RawMessage
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &status))
	require.True(t, status.Success)
	assert.NotContains(t, status.Data, "quota_remind_enabled")
	assert.NotContains(t, status.Data, "quota_remind_threshold")
	for _, value := range []string{`{}`, `null`, `{"enabled":true,"threshold":{"nanos":1,"currency":"USD"}}`, `{"enabled":true,"threshold":{"nanos":"1","currency":"EUR"}}`, `{"enabled":true,"enabled":false,"threshold":{"nanos":"1","currency":"USD"}}`, `{"enabled":true,"threshold":null}`} {
		body, err := common.Marshal(OptionUpdateRequest{Key: operation_setting.BalanceReminderConfigOption, Value: value})
		require.NoError(t, err)
		response = reminderSettingsHTTP(t, UpdateOption, http.MethodPut, "/api/option/", string(body), operator)
		var rejected struct{ Success bool }
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &rejected))
		assert.False(t, rejected.Success)
		var unchanged []model.Option
		require.NoError(t, db.Order("key").Find(&unchanged).Error)
		assert.Equal(t, before, unchanged)
	}
	for _, body := range []string{
		`{"key":"balance_reminder_setting.config","value":{"enabled":true,"threshold":{"nanos":"1","currency":"USD"}}}`,
		`{"key":"balance_reminder_setting.config","value":true}`,
		`{"key":"balance_reminder_setting.config","key":"balance_reminder_setting.config","value":"{\"enabled\":true,\"threshold\":{\"nanos\":\"1\",\"currency\":\"USD\"}}"}`,
		`{"key":"balance_reminder_setting.config","value":"{\"enabled\":true,\"threshold\":{\"nanos\":\"1\",\"currency\":\"USD\"}}","unknown":true}`,
		`{"key":"balance_reminder_setting.config","value":"{\"enabled\":true,\"ENABLED\":false,\"threshold\":{\"nanos\":\"1\",\"currency\":\"USD\"}}"}`,
	} {
		response = reminderSettingsHTTP(t, UpdateOption, http.MethodPut, "/api/option/", body, operator)
		var rejected struct{ Success bool }
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &rejected))
		assert.False(t, rejected.Success)
		var unchanged []model.Option
		require.NoError(t, db.Order("key").Find(&unchanged).Error)
		assert.Equal(t, before, unchanged)
	}
	exact := `{"enabled":true,"threshold":{"nanos":"9007199254740993","currency":"USD"}}`
	body, err := common.Marshal(OptionUpdateRequest{Key: operation_setting.BalanceReminderConfigOption, Value: exact})
	require.NoError(t, err)
	response = reminderSettingsHTTP(t, UpdateOption, http.MethodPut, "/api/option/", string(body), operator)
	var accepted struct{ Success bool }
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &accepted))
	require.True(t, accepted.Success)
	saved, err := model.GetBalanceReminderSiteSettings(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(9007199254740993), saved.Threshold.Nanos)
	common.OptionMapRWMutex.RLock()
	beforeRuntime := maps.Clone(common.OptionMap)
	common.OptionMapRWMutex.RUnlock()
	beforeConfig := config.GlobalConfig.ExportAllConfigs()[operation_setting.BalanceReminderConfigOption]
	require.NoError(t, db.Exec(`CREATE FUNCTION reject_http_reminder_option() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'reminder option unavailable'; END $$; CREATE TRIGGER reject_http_reminder_option BEFORE UPDATE ON options FOR EACH ROW WHEN (OLD.key = 'balance_reminder_setting.config') EXECUTE FUNCTION reject_http_reminder_option()`).Error)
	body, err = common.Marshal(OptionUpdateRequest{Key: operation_setting.BalanceReminderConfigOption, Value: `{"enabled":false,"threshold":{"nanos":"0","currency":"USD"}}`})
	require.NoError(t, err)
	response = reminderSettingsHTTP(t, UpdateOption, http.MethodPut, "/api/option/", string(body), operator)
	var rejected struct{ Success bool }
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &rejected))
	assert.False(t, rejected.Success)
	afterFailure, err := model.GetBalanceReminderSiteSettings(t.Context())
	require.NoError(t, err)
	assert.Equal(t, saved, afterFailure)
	common.OptionMapRWMutex.RLock()
	afterRuntime := maps.Clone(common.OptionMap)
	common.OptionMapRWMutex.RUnlock()
	assert.Equal(t, beforeRuntime, afterRuntime)
	assert.Equal(t, beforeConfig, config.GlobalConfig.ExportAllConfigs()[operation_setting.BalanceReminderConfigOption])
	require.NoError(t, db.Exec("DROP TRIGGER reject_http_reminder_option ON options").Error)
	var legacy []model.Option
	require.NoError(t, db.Where("key IN ?", []string{"QuotaRemindEnabled", "QuotaRemindThreshold"}).Order("key").Find(&legacy).Error)
	assert.Equal(t, []model.Option{{Key: "QuotaRemindEnabled", Value: "true"}, {Key: "QuotaRemindThreshold", Value: "1000"}}, legacy)
}

func TestLegacyQuotaSettingsRejectLiveMutation(t *testing.T) {
	for _, tc := range []struct {
		key, before, after string
	}{
		{"quota_setting.trust_quota_usd", "10", "25"},
		{"quota_setting.pre_consume_multiplier", "1", "2"},
		{"quota_setting.enable_free_model_pre_consume", "true", "false"},
		{"PreConsumedQuota", "500", "600"},
	} {
		for _, stored := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stored=%t", tc.key, stored), func(t *testing.T) {
				db := modelManagementDB(t)
				previousQuota := *operation_setting.GetQuotaSetting()
				t.Cleanup(func() { *operation_setting.GetQuotaSetting() = previousQuota })
				initial := map[string]string{tc.key: tc.before, "GatewayMaxAttempts": "3"}
				for key, value := range initial {
					if key == tc.key && !stored {
						continue
					}
					require.NoError(t, db.Create(&model.Option{Key: key, Value: value}).Error)
				}
				common.OptionMapRWMutex.Lock()
				common.OptionMap = maps.Clone(initial)
				common.OptionMapRWMutex.Unlock()
				var before []model.Option
				require.NoError(t, db.Order("key").Find(&before).Error)

				for range 2 {
					var projection struct {
						Success bool           `json:"success"`
						Data    []model.Option `json:"data"`
					}
					response := modelManagementRequest(t, GetOptions, http.MethodGet, "/api/option/", nil, &projection)
					require.Equal(t, http.StatusOK, response.Code)
					require.True(t, projection.Success)
					visible := map[string]string{}
					for _, option := range projection.Data {
						visible[option.Key] = option.Value
					}
					assert.NotContains(t, visible, tc.key, "retired quota policy must not remain an editable live setting")
					assert.Equal(t, "3", visible["GatewayMaxAttempts"], "active gateway controls must remain visible")
				}
				for _, value := range []any{tc.after, nil} {
					var result struct {
						Success bool   `json:"success"`
						Code    string `json:"code"`
					}
					response := modelManagementRequest(t, UpdateOption, http.MethodPut, "/api/option/", OptionUpdateRequest{Key: tc.key, Value: value}, &result)
					assert.Equal(t, http.StatusGone, response.Code)
					assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
					assert.False(t, result.Success)
					assert.Equal(t, "legacy_quota_settings_retired", result.Code)
				}
				assert.ErrorContains(t, model.UpdateOption(tc.key, tc.after), "Legacy quota reservation settings have been retired.")
				assert.ErrorContains(t, model.UpdateOptionsBulk(map[string]string{tc.key: tc.after, "GatewayMaxAttempts": "4"}), "Legacy quota reservation settings have been retired.")
				var after []model.Option
				require.NoError(t, db.Order("key").Find(&after).Error)
				assert.Equal(t, before, after, "retirement must preserve the complete historical source and reject mixed writes atomically")
				common.OptionMapRWMutex.RLock()
				current := maps.Clone(common.OptionMap)
				common.OptionMapRWMutex.RUnlock()
				assert.Equal(t, initial, current, "rejected mutations cannot change live settings")
				assert.Equal(t, previousQuota, *operation_setting.GetQuotaSetting())

				var active struct{ Success bool }
				response := modelManagementRequest(t, UpdateOption, http.MethodPut, "/api/option/", OptionUpdateRequest{Key: "GatewayMaxAttempts", Value: "4"}, &active)
				require.Equal(t, http.StatusOK, response.Code)
				require.True(t, active.Success, "active gateway controls must remain writable")
				var saved model.Option
				require.NoError(t, db.Where(&model.Option{Key: "GatewayMaxAttempts"}).First(&saved).Error)
				assert.Equal(t, "4", saved.Value)
			})
		}
	}
}

func TestUpdateOptionRejectsInvalidTaskBillingExpressions(t *testing.T) {
	database := modelManagementDB(t)
	declareLegacyProviderRoutingColumns(t, database)
	const pluginKey = "billing-save-probe"
	const modelName = "billing-save-model"
	source := `
export const meta = {
  apiVersion: 1, key: "billing-save-probe", name: "Billing Save Probe", version: "1.0.0", author: {name: "Test"},
  models: ["billing-save-model"], fetchMode: "per_task",
  usageSchema: {seconds: {type: "number", unit: "second"}}
};
export function buildSubmitRequest() { return {}; }
export function parseSubmitResponse() { return {}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {}; }
`
	_, err := jsplugin.DefaultRegistry.Register(source, jsplugin.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { jsplugin.DefaultRegistry.Unregister(pluginKey) })
	legacySource, err := model.LoadLegacyPricingConversionSource(database, jsplugin.DefaultRegistry.Generation())
	require.NoError(t, err)

	tests := []struct {
		name       string
		expression string
		errorText  string
	}{
		{
			name:       "invalid syntax",
			expression: `tier("base",`,
			errorText:  "expr compile error",
		},
		{
			name:       "undeclared usage key",
			expression: `tier("base", u("clips") * 0.1)`,
			errorText:  `usage key \"clips\" is not declared`,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			expressions, marshalErr := common.Marshal(map[string]string{modelName: testCase.expression})
			require.NoError(t, marshalErr)
			body, marshalErr := common.Marshal(OptionUpdateRequest{
				Key:   "billing_setting.billing_expr",
				Value: string(expressions),
			})
			require.NoError(t, marshalErr)
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			context.Request = httptest.NewRequest(http.MethodPut, "/api/option/", strings.NewReader(string(body)))

			// Retain offline migration validation while the live option is retired.
			_, validationErr := legacySource.Preview(modelName, model.PricingValues{"billing_setting.billing_expr": testCase.expression})
			require.Error(t, validationErr)
			assert.Contains(t, validationErr.Error(), strings.ReplaceAll(testCase.errorText, `\"`, `"`))
			UpdateOption(context)

			assert.Equal(t, http.StatusGone, recorder.Code)
			assert.Contains(t, recorder.Body.String(), `"success":false`)
			assert.Contains(t, recorder.Body.String(), "PriceVersions")
		})
	}
}

func TestUpdateOptionRejectsUsageExpressionWithoutTaskPlugin(t *testing.T) {
	database := modelManagementDB(t)
	declareLegacyProviderRoutingColumns(t, database)
	legacySource, err := model.LoadLegacyPricingConversionSource(database, jsplugin.DefaultRegistry.Generation())
	require.NoError(t, err)
	const modelName = "billing-save-model-without-plugin"
	expressions, err := common.Marshal(map[string]string{
		modelName: `u("mode") == "std" ? 1 : 2`,
	})
	require.NoError(t, err)
	body, err := common.Marshal(OptionUpdateRequest{
		Key:   "billing_setting.billing_expr",
		Value: string(expressions),
	})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(
		http.MethodPut,
		"/api/option/",
		strings.NewReader(string(body)),
	)

	_, validationErr := legacySource.Preview(modelName, model.PricingValues{"billing_setting.billing_expr": `u("mode") == "std" ? 1 : 2`})
	require.Error(t, validationErr)
	assert.Contains(t, validationErr.Error(), "mode")
	assert.Contains(t, validationErr.Error(), "no task plugin usage schema")
	UpdateOption(context)

	assert.Equal(t, http.StatusGone, recorder.Code)
	assert.Contains(t, recorder.Body.String(), `"success":false`)
	assert.Contains(t, recorder.Body.String(), "PriceVersions")
}

func TestUpdateOptionAliasBillingExprUsesPluginSchema(t *testing.T) {
	database := modelManagementDB(t)
	require.NoError(t, database.AutoMigrate(&model.Log{}))
	const pluginKey = "billing-alias-probe"
	source := `
export const meta = {
  apiVersion: 1, key: "billing-alias-probe", name: "Billing Alias Probe", version: "1.0.0", author: {name: "Test"},
  models: ["declared-model"], fetchMode: "per_task",
  usageSchema: {seconds: {type: "number", unit: "second"}, image_count: {type: "number", unit: "count"}},
  usageProfiles: [{models: ["declared-model"], schema: {seconds: {type: "number", unit: "second"}}}]
};
export function buildSubmitRequest() { return {}; }
export function parseSubmitResponse() { return {}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {}; }
`
	_, err := jsplugin.DefaultRegistry.Register(source, jsplugin.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { jsplugin.DefaultRegistry.Unregister(pluginKey) })

	mapping := `{"alias-model":"declared-model"}`
	provider := model.Provider{
		Id:           1,
		Type:         54,
		Key:          "key-1",
		Status:       common.ChannelStatusEnabled,
		Name:         "ch-1",
		Group:        "default",
		Models:       "alias-model,declared-model",
		ModelMapping: &mapping,
	}
	declareLegacyProviderRoutingColumns(t, database)
	require.NoError(t, database.Create(&provider).Error)
	seedLegacyProviderRouting(t, database, &provider)
	legacySource, err := model.LoadLegacyPricingConversionSource(database, jsplugin.DefaultRegistry.Generation())
	require.NoError(t, err)

	saved := map[string]string{}
	require.NoError(t, config.GlobalConfig.SaveToDB(func(key, value string) error {
		saved[key] = value
		return nil
	}))
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(saved))
	})

	putExpr := func(modelName, expression string) *httptest.ResponseRecorder {
		t.Helper()
		expressions, marshalErr := common.Marshal(map[string]string{modelName: expression})
		require.NoError(t, marshalErr)
		body, marshalErr := common.Marshal(OptionUpdateRequest{
			Key:   "billing_setting.billing_expr",
			Value: string(expressions),
		})
		require.NoError(t, marshalErr)
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Request = httptest.NewRequest(http.MethodPut, "/api/option/", strings.NewReader(string(body)))
		UpdateOption(context)
		return recorder
	}

	// Alias/profile semantics remain enforced by the offline validation seam.
	preview, err := legacySource.Preview("alias-model", model.PricingValues{"billing_setting.billing_expr": `u("seconds")`})
	require.NoError(t, err)
	assert.Equal(t, "Task pricing must be converted manually using the task usage schema.", preview.UnsupportedReason)
	for _, name := range []string{"alias-model", "declared-model"} {
		_, err := legacySource.Preview(name, model.PricingValues{"billing_setting.billing_expr": `u("image_count")`})
		require.Error(t, err)
		assert.Contains(t, err.Error(), `usage key "image_count" is not declared`)
	}
	_, err = legacySource.Preview("unknown-alias-model", model.PricingValues{"billing_setting.billing_expr": `u("seconds")`})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no task plugin usage schema")

	// Valid syntax and aliases cannot reactivate the retired HTTP write path.
	for _, input := range []struct{ name, expression string }{
		{"alias-model", `u("seconds")`},
		{"alias-model", `u("image_count")`},
		{"declared-model", `u("image_count")`},
		{"unknown-alias-model", `u("seconds")`},
	} {
		response := putExpr(input.name, input.expression)
		assert.Equal(t, http.StatusGone, response.Code)
		assert.Contains(t, response.Body.String(), `"success":false`)
		assert.Contains(t, response.Body.String(), "PriceVersions")
	}
}

// Covers configuration persistence, validation, reservation and reconciliation
// through the same public entry points used by the relay.
func TestPreConsumePolicyDatabase(t *testing.T) {
	previousConfig := config.GlobalConfig.ExportAllConfigs()
	previousUnit := common.QuotaPerUnit
	common.QuotaPerUnit = 500000
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(previousConfig))
		common.QuotaPerUnit = previousUnit
	})
	db := modelManagementDB(t)
	require.NoError(t, db.AutoMigrate(&model.Token{}))
	// Seed retained historical options directly; live writes are retired.
	for key, value := range map[string]string{
		"quota_setting.trust_quota_usd":        "10.5",
		"quota_setting.pre_consume_multiplier": "1.5",
	} {
		require.NoError(t, operation_setting.ValidateQuotaOption(key, value))
		require.NoError(t, db.Create(&model.Option{Key: key, Value: value}).Error)
	}
	var savedOptions []model.Option
	require.NoError(t, db.Find(&savedOptions).Error)
	stored := map[string]string{}
	for _, option := range savedOptions {
		stored[option.Key] = option.Value
	}
	for range 2 {
		operation_setting.GetQuotaSetting().TrustQuotaUSD = 10
		operation_setting.GetQuotaSetting().PreConsumeMultiplier = 1
		require.NoError(t, config.GlobalConfig.LoadFromDB(stored))
		assert.Equal(t, 10.5, operation_setting.GetQuotaSetting().TrustQuotaUSD)
		assert.Equal(t, 1.5, operation_setting.GetQuotaSetting().PreConsumeMultiplier)
	}
	for _, tc := range []struct{ key, value string }{
		{"trust_quota_usd", "-1"}, {"trust_quota_usd", "NaN"}, {"trust_quota_usd", "+Inf"},
		{"pre_consume_multiplier", "0"}, {"pre_consume_multiplier", "-0.5"},
		{"pre_consume_multiplier", "NaN"}, {"pre_consume_multiplier", "Inf"},
		{"pre_consume_multiplier", "1e309"}, {"pre_consume_multiplier", ""},
	} {
		key := "quota_setting." + tc.key
		assert.Error(t, operation_setting.ValidateQuotaOption(key, tc.value), "%s=%q", key, tc.value)
		var response struct{ Success bool }
		recorder := modelManagementRequest(t, UpdateOption, http.MethodPut, "/api/option/", OptionUpdateRequest{Key: key, Value: tc.value}, &response)
		assert.Equal(t, http.StatusGone, recorder.Code)
		assert.False(t, response.Success, "%s=%q", key, tc.value)
		var saved model.Option
		require.NoError(t, db.Where(&model.Option{Key: key}).First(&saved).Error)
		assert.Equal(t, stored[key], saved.Value, "invalid saves must preserve the previous value")
	}
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		"billing_setting.billing_mode":    `{"policy-model":"tiered_expr"}`,
		"billing_setting.billing_expr":    `{"policy-model":"tier(\"base\", p * 3 + c * 15)"}`,
		"group_ratio_setting.group_ratio": `{"default":1}`,
	}))
	cases := []struct {
		name                  string
		threshold, multiplier float64
		wallet, token         int
		force, unlimited      bool
		wantHeld              int
	}{
		{"above threshold", 10, 1, 5500000, 5500000, false, false, 0},
		{"custom threshold", 20, 1, 5500000, 5500000, false, false, 1500},
		{"zero disables bypass", 0, 1, 5500000, 5500000, false, false, 1500},
		{"wallet equals threshold", 10, 1, 5000000, 5500000, false, false, 1500},
		{"token equals threshold", 10, 1, 5500000, 5000000, false, false, 1500},
		{"fractional threshold", 10.5, 1, 5250001, 5250001, false, false, 0},
		{"unlimited token", 10, 1, 5500000, 0, false, true, 0},
		{"forced reservation", 10, 1, 5500000, 5500000, true, false, 1500},
		{"half input cost", 0, 0.5, 100000, 100000, false, false, 750},
		{"fractional multiple refunds excess", 0, 2.5, 100000, 100000, false, false, 3750},
	}
	t.Setenv("GATEWAY_SETTLEMENT_JOURNAL_DIR", t.TempDir())
	require.NoError(t, model.InitSettlementCurrency())
	require.NoError(t, model.MigrateSubscriptionMoney(db))
	require.NoError(t, model.MigrateGatewayBilling(db))
	require.NoError(t, model.MigrateBalanceReminders(db))
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, operation_setting.ValidateQuotaOption("quota_setting.trust_quota_usd", strconv.FormatFloat(tc.threshold, 'f', -1, 64)))
			require.NoError(t, operation_setting.ValidateQuotaOption("quota_setting.pre_consume_multiplier", strconv.FormatFloat(tc.multiplier, 'f', -1, 64)))
			operation_setting.GetQuotaSetting().TrustQuotaUSD = tc.threshold
			operation_setting.GetQuotaSetting().PreConsumeMultiplier = tc.multiplier
			user := model.User{Username: fmt.Sprintf("policy-user-%d", i), Quota: tc.wallet, Group: "default", Setting: `{"billing_preference":"wallet_only"}`, AffCode: fmt.Sprintf("policy-aff-%d", i)}
			require.NoError(t, db.Create(&user).Error)
			token := model.Token{UserId: user.Id, Key: fmt.Sprintf("policy-token-%d", i)}
			require.NoError(t, db.Create(&token).Error)
			legacy := testdb.LegacyKeyQuota{RemainQuota: tc.token, UnlimitedQuota: tc.unlimited}
			testdb.SeedLegacyKeyQuota(t, db, token.Id, legacy)
			_, err := model.TopUpWallet(model.WalletTopUp{UserId: user.Id, Source: money.Amount{Nanos: 10000000, Currency: "USD"}, Reference: fmt.Sprintf("policy-opening-%d", i)})
			require.NoError(t, err)
			price := &model.PriceVersion{ID: i + 100, Currency: "USD", Expression: `tier("base", p * 3 + c * 15)`}
			_, estimate, _, err := service.GatewayPrice(price, &dto.Usage{PromptTokens: 1000}, billingexpr.RequestInput{}, time.Now(), decimal.NewFromInt(1), nil)
			require.NoError(t, err)
			reserved, err := estimate.Receivable()
			require.NoError(t, err)
			assert.EqualValues(t, 3000000, reserved.Nanos, "retired trust and quota multipliers cannot bypass or increase the sole money reservation")
			request := model.GatewayRequest{ID: fmt.Sprintf("policy-%d", i), UserID: user.Id, TokenID: token.Id, SalePriceVersionID: price.ID, SaleExpression: price.Expression, SaleCurrency: "USD", SaleFXRate: "1", GroupRatio: "1", StartedAt: time.Now().Unix(), Status: "reserved"}
			require.NoError(t, model.ReserveGateway(&request, reserved, reserved))
			wallet, err := model.GetUserWallet(user.Id)
			require.NoError(t, err)
			assert.EqualValues(t, 7000000, wallet.Balance.Nanos)
			_, actual, _, err := service.GatewayPrice(price, &dto.Usage{PromptTokens: 1000, CompletionTokens: 100}, billingexpr.RequestInput{}, time.Now(), decimal.NewFromInt(1), nil)
			require.NoError(t, err)
			request.Receivable, err = actual.Receivable()
			require.NoError(t, err)
			assert.EqualValues(t, 4500000, request.Receivable.Nanos)
			request.Status = "succeeded"
			require.NoError(t, model.PrepareGatewaySettlement(model.GatewaySettlement{Request: request, SaleSource: request.Receivable}))
			for range 2 {
				_, err = model.SettleGateway(request.ID)
				require.NoError(t, err)
			}
			wallet, err = model.GetUserWallet(user.Id)
			require.NoError(t, err)
			assert.EqualValues(t, 5500000, wallet.Balance.Nanos)
			require.NoError(t, db.First(&user, user.Id).Error)
			assert.Equal(t, tc.wallet, user.Quota)
			assert.Equal(t, legacy, testdb.ReadLegacyKeyQuota(t, db, token.Id))
			var holds, releases int64
			require.NoError(t, db.Model(&model.LedgerEntry{}).Where("request_id = ? AND type = ?", request.ID, model.LedgerEntryPreConsume).Count(&holds).Error)
			require.NoError(t, db.Model(&model.LedgerEntry{}).Where("request_id = ? AND type = ?", request.ID, model.LedgerEntryPreConsumeRelease).Count(&releases).Error)
			assert.EqualValues(t, 1, holds)
			assert.EqualValues(t, 1, releases)
			reconciled, err := model.ReconcileWallets(100)
			require.NoError(t, err)
			assert.Empty(t, reconciled.Mismatches)
		})
	}
}

func TestPreConsumeMultiplierRejectsInvalidRuntimeAndOverflow(t *testing.T) {
	fixture := newNativeGatewayTaskFixture(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("a rejected historical quote cannot contact a Provider")
	})
	var originalEntries int64
	require.NoError(t, fixture.db.Model(&model.LedgerEntry{}).Count(&originalEntries).Error)
	previous := config.GlobalConfig.ExportAllConfigs()
	t.Cleanup(func() { require.NoError(t, config.GlobalConfig.LoadFromDB(previous)) })
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		"billing_setting.billing_mode":    `{"policy-overflow":"tiered_expr"}`,
		"billing_setting.billing_expr":    `{"policy-overflow":"tier(\"base\", p * 3)"}`,
		"group_ratio_setting.group_ratio": `{"default":1}`,
	}))
	for _, multiplier := range []float64{0, -0.5, math.NaN(), math.Inf(1), math.MaxFloat64} {
		operation_setting.GetQuotaSetting().PreConsumeMultiplier = multiplier
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		info := &relaycommon.RelayInfo{OriginModelName: "policy-overflow", UserGroup: "default", UsingGroup: "default", BillingRequestInput: &billingexpr.RequestInput{}}
		_, err := helper.ModelPriceHelper(ctx, info, 1000, &types.TokenCountMeta{})
		require.Error(t, err)
		var lifecycles, entries int64
		require.NoError(t, fixture.db.Model(&model.GatewayBilling{}).Count(&lifecycles).Error)
		require.NoError(t, fixture.db.Model(&model.LedgerEntry{}).Count(&entries).Error)
		assert.Zero(t, lifecycles)
		assert.Equal(t, originalEntries, entries, "invalid quotation cannot create any hold or ledger entry")
		assertSubmissionWallet(t, fixture, 2000000000)
	}
}

func TestRegistrationRewardSettingsAreAtomicAndComplianceBound(t *testing.T) {
	db := model.UseTestDatabases(t)
	require.NoError(t, db.AutoMigrate(&model.Option{}, &model.User{}))
	require.NoError(t, model.MigrateAuditLogs())
	if !common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		require.NoError(t, model.LOG_DB.AutoMigrate(&model.Log{}))
	}
	previous := config.GlobalConfig.Get("reward_setting")
	previousRedis, previousMemory := common.RedisEnabled, common.MemoryCacheEnabled
	common.RedisEnabled, common.MemoryCacheEnabled = false, false
	previousPayment := *operation_setting.GetPaymentSetting()
	common.OptionMapRWMutex.Lock()
	previousOptions := common.OptionMap
	common.OptionMap = make(map[string]string)
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		config.GlobalConfig.Register("reward_setting", previous)
		common.RedisEnabled, common.MemoryCacheEnabled = previousRedis, previousMemory
		*operation_setting.GetPaymentSetting() = previousPayment
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptions
		common.OptionMapRWMutex.Unlock()
	})
	operation_setting.GetPaymentSetting().ComplianceConfirmed = true
	operation_setting.GetPaymentSetting().ComplianceTermsVersion = operation_setting.CurrentComplianceTermsVersion
	const initial = `{"signup":{"nanos":"9007199254740993","currency":"USD"},"inviter":{"nanos":"3","currency":"CNY"},"invitee":{"nanos":"1","currency":"EUR"}}`
	var result struct{ Success bool }
	modelManagementRequest(t, UpdateOption, http.MethodPut, "/api/option/", OptionUpdateRequest{Key: "reward_setting.config", Value: initial}, &result)
	require.True(t, result.Success)
	want := operation_setting.RewardConfig{
		Signup:  money.Amount{Nanos: 9007199254740993, Currency: "USD"},
		Inviter: money.Amount{Nanos: 3, Currency: "CNY"},
		Invitee: money.Amount{Nanos: 1, Currency: "EUR"},
	}
	assert.Equal(t, want, operation_setting.GetRewardSetting())
	for _, body := range []string{
		`{}`,
		strings.Replace(initial, `"signup":{"nanos":"9007199254740993","currency":"USD"}`, `"signup":null`, 1),
		strings.Replace(initial, `"9007199254740993"`, `9007199254740993`, 1),
		strings.Replace(initial, `"9007199254740993"`, `"-1"`, 1),
		strings.Replace(initial, `"9007199254740993"`, `"9223372036854775808"`, 1),
		strings.Replace(initial, `"USD"`, `"US1"`, 1),
		strings.Replace(initial, `"signup":`, `"unexpected":`, 1),
		strings.Replace(initial, `"nanos":"9007199254740993"`, `"nanos":"2","nanos":"9007199254740993"`, 1),
		strings.Replace(initial, `"signup":`, `"signup":{"nanos":"1","currency":"USD"},"signup":`, 1),
	} {
		var rejected struct{ Success bool }
		modelManagementRequest(t, UpdateOption, http.MethodPut, "/api/option/", OptionUpdateRequest{Key: "reward_setting.config", Value: body}, &rejected)
		assert.False(t, rejected.Success, body)
		assert.Equal(t, want, operation_setting.GetRewardSetting(), "invalid settings must not partially replace the active snapshot")
		var stored model.Option
		require.NoError(t, db.First(&stored, `"key" = ?`, "reward_setting.config").Error)
		assert.Equal(t, initial, stored.Value, "invalid settings must not overwrite persisted rewards")
	}
	for _, key := range []string{"QuotaForNewUser", "QuotaForInviter", "QuotaForInvitee", "reward_setting.signup", "reward_setting"} {
		var rejected struct{ Success bool }
		modelManagementRequest(t, UpdateOption, http.MethodPut, "/api/option/", OptionUpdateRequest{Key: key, Value: "1"}, &rejected)
		assert.False(t, rejected.Success, key)
	}
	operation_setting.GetPaymentSetting().ComplianceConfirmed = false
	var blocked struct{ Success bool }
	modelManagementRequest(t, UpdateOption, http.MethodPut, "/api/option/", OptionUpdateRequest{Key: "reward_setting.config", Value: initial}, &blocked)
	assert.False(t, blocked.Success, "invitation rewards require server-side compliance confirmation")
	assert.Equal(t, want, operation_setting.GetRewardSetting())
	const noInvitation = `{"signup":{"nanos":"1","currency":"USD"},"inviter":{"nanos":"0","currency":"CNY"},"invitee":{"nanos":"0","currency":"EUR"}}`
	var allowed struct{ Success bool }
	modelManagementRequest(t, UpdateOption, http.MethodPut, "/api/option/", OptionUpdateRequest{Key: "reward_setting.config", Value: noInvitation}, &allowed)
	require.True(t, allowed.Success, "disabled invitation awards do not block signup-only rewards")
	assert.Equal(t, operation_setting.RewardConfig{Signup: money.Amount{Nanos: 1, Currency: "USD"}, Inviter: money.Amount{Currency: "CNY"}, Invitee: money.Amount{Currency: "EUR"}}, operation_setting.GetRewardSetting())
}

func TestCheckinHTTPAmountContractAndLog(t *testing.T) {
	t.Setenv("SETTLEMENT_CURRENCY", "USD")
	db := model.UseTestDatabases(t)
	require.NoError(t, model.InitSettlementCurrency())
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Checkin{}, &model.Option{}))
	require.NoError(t, model.MigrateGatewayBilling(db))
	if !common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		require.NoError(t, model.LOG_DB.AutoMigrate(&model.Log{}))
	}
	previous := config.GlobalConfig.Get("checkin_setting")
	previousRedis, previousMemory := common.RedisEnabled, common.MemoryCacheEnabled
	common.RedisEnabled, common.MemoryCacheEnabled = false, false
	common.OptionMapRWMutex.Lock()
	previousOptions := common.OptionMap
	common.OptionMap = make(map[string]string)
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		config.GlobalConfig.Register("checkin_setting", previous)
		common.RedisEnabled, common.MemoryCacheEnabled = previousRedis, previousMemory
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptions
		common.OptionMapRWMutex.Unlock()
	})
	clock.UseFake(t, time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC))
	user := model.User{Username: "checkin-http", Password: "password", Status: common.UserStatusEnabled, Quota: 17}
	require.NoError(t, db.Create(&user).Error)
	const reward = `{"min":{"nanos":"9007199254740993","currency":"USD"},"max":{"nanos":"9007199254740993","currency":"USD"}}`
	for key, value := range map[string]string{"checkin_setting.enabled": "true", "checkin_setting.reward_range": reward} {
		var response struct{ Success bool }
		modelManagementRequest(t, UpdateOption, http.MethodPut, "/api/option/", OptionUpdateRequest{Key: key, Value: value}, &response)
		require.True(t, response.Success)
	}
	// The existing option endpoint must reject retired keys and unknown bounds.
	for _, request := range []OptionUpdateRequest{
		{Key: "checkin_setting.min_quota", Value: 1000},
		{Key: "checkin_setting.max_quota", Value: 10000},
		{Key: "checkin_setting.unknown", Value: "x"},
		{Key: "checkin_setting.reward_range", Value: `{"min":{"nanos":"0","currency":"USD"},"max":{"nanos":"1","currency":"USD"},"quota":1}`},
	} {
		var response struct{ Success bool }
		modelManagementRequest(t, UpdateOption, http.MethodPut, "/api/option/", request, &response)
		assert.False(t, response.Success)
	}
	perform := func(handler gin.HandlerFunc, method string) map[string]any {
		t.Helper()
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Request = httptest.NewRequest(method, "/api/user/checkin?month=2026-11", nil)
		context.Set("id", user.Id)
		handler(context)
		assert.Equal(t, http.StatusOK, recorder.Code)
		var response map[string]any
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
		return response
	}
	posted := perform(DoCheckin, http.MethodPost)
	require.Equal(t, true, posted["success"])
	assert.Equal(t, map[string]any{"amount": map[string]any{"nanos": "9007199254740993", "currency": "USD"}, "checkin_date": "2026-11-14"}, posted["data"])
	status := perform(GetCheckinStatus, http.MethodGet)
	require.Equal(t, true, status["success"])
	data := status["data"].(map[string]any)
	assert.Len(t, data, 3)
	assert.Equal(t, true, data["enabled"])
	var expectedRange map[string]any
	require.NoError(t, common.UnmarshalJsonStr(reward, &expectedRange))
	assert.Equal(t, expectedRange, data["reward_range"])
	stats := data["stats"].(map[string]any)
	assert.Equal(t, float64(1), stats["total_checkins"])
	assert.Equal(t, float64(1), stats["checkin_count"])
	assert.Equal(t, true, stats["checked_in_today"])
	assert.Equal(t, []any{map[string]any{"nanos": "9007199254740993", "currency": "USD"}}, stats["total_awarded"])
	assert.Equal(t, []any{posted["data"]}, stats["records"])
	assert.NotContains(t, stats, "total_quota")
	assert.Equal(t, false, perform(DoCheckin, http.MethodPost)["success"])
	wallet, err := model.GetUserWallet(user.Id)
	require.NoError(t, err)
	require.NotNil(t, wallet)
	assert.Equal(t, money.Amount{Nanos: 9007199254740993, Currency: "USD"}, wallet.Balance)
	require.NoError(t, db.First(&user, user.Id).Error)
	assert.Equal(t, 17, user.Quota)
	var logs []model.Log
	require.NoError(t, model.LOG_DB.Where("user_id = ? AND type = ?", user.Id, model.LogTypeSystem).Find(&logs).Error)
	require.Len(t, logs, 1, "rejected replay must not produce another success log")
	assert.Equal(t, "Daily check-in, received 9007199.254740993 USD", logs[0].Content)
	assert.Zero(t, logs[0].Quota)
	var other struct {
		Parts []*common.Message `json:"content_parts"`
	}
	require.NoError(t, common.UnmarshalJsonStr(logs[0].Other, &other))
	require.Len(t, other.Parts, 1)
	assert.Equal(t, "Daily check-in, received {{amount}}", other.Parts[0].Key)
	assert.Equal(t, map[string]any{"amount": "9007199.254740993 USD"}, other.Parts[0].Params)
}
