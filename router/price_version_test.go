package router

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPriceVersionManagementHTTP(t *testing.T) {
	engine, actor, token := newAdminTestServer(t)
	db := model.DB
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.ModelOffering{}, &model.ModelOfferingMember{}, &model.ModelOfferingName{}))
	require.NoError(t, model.MigratePricing(db))
	require.NoError(t, model.MigratePricing(db))
	t.Cleanup(model.InstallPriceVersionCheckers)
	fake := clock.UseFake(t, time.Now().Truncate(time.Second))
	now := clock.Now().Unix()
	publisher := model.Vendor{Name: "Pricing publisher", Status: 1}
	require.NoError(t, publisher.Insert())
	unified := model.Model{ModelName: "priced-model", VendorID: publisher.Id, Status: 1}
	require.NoError(t, unified.Insert())
	provider := model.Provider{Name: "Pricing provider", Type: 1, Status: common.ChannelStatusEnabled, Key: "not-exposed"}
	require.NoError(t, db.Create(&provider).Error)
	channel, err := model.MutateChannel(t.Context(), 0, model.ChannelSave, func(row *model.Channel) error {
		*row = model.Channel{ProviderID: provider.Id, ModelID: unified.Id, Protocol: "openai_chat", UpstreamModelName: "upstream", ParameterOverrides: `{}`}
		return nil
	})
	require.NoError(t, err)
	_, err = model.MutateChannel(t.Context(), channel.ID, model.ChannelConfirm, nil)
	require.NoError(t, err)
	offering := model.ModelOffering{Name: "priced-offering", Members: []model.ModelOfferingMember{{ModelID: unified.Id, Tier: 1}}}
	require.NoError(t, model.SaveModelOffering(t.Context(), &offering, "save"))
	call := func(method, path, body string, status int) map[string]any {
		response := adminSecurityRequest(engine, token, method, path, body)
		require.Equal(t, status, response.Code, response.Body.String())
		result := map[string]any{}
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result), response.Body.String())
		return result
	}
	priceBody := func(owner string, id int, expression string, at int64) string {
		body, err := common.Marshal(map[string]any{"owner_type": owner, "owner_id": id, "currency": "EUR", "expression": expression, "effective_at": at, "id": 999, "created_by": 999, "created_at": 1})
		require.NoError(t, err)
		return string(body)
	}
	current := fmt.Sprintf("/api/admin/v1/price-versions/current?owner_type=channel&owner_id=%d", channel.ID)
	channelPath := fmt.Sprintf("/api/admin/v1/channels/%d", channel.ID)
	offeringPath := fmt.Sprintf("/api/admin/v1/model-offerings/%d", offering.ID)

	t.Run("omitted effective time uses the server clock and explicit schedules remain exact", func(t *testing.T) {
		fake.Set(time.Unix(now+7200, 0))
		t.Cleanup(func() { fake.Set(time.Unix(now, 0)) })
		owner := model.ModelOffering{Name: "server-timed-offering", Members: []model.ModelOfferingMember{{ModelID: unified.Id, Tier: 1}}}
		require.NoError(t, model.SaveModelOffering(t.Context(), &owner, "save"))
		body := map[string]any{"owner_type": "model_offering", "owner_id": owner.ID, "currency": "EUR", "expression": `tier("request", fixed(0))`, "id": 999, "created_by": 999, "created_at": 1}
		encoded, err := common.Marshal(body)
		require.NoError(t, err)
		immediate := call("POST", "/api/admin/v1/price-versions", string(encoded), 200)["data"].(map[string]any)
		assert.EqualValues(t, now+7200, immediate["effective_at"])
		assert.EqualValues(t, clock.Now().Unix(), immediate["created_at"])
		assert.EqualValues(t, actor.Id, immediate["created_by"])
		assert.NotEqualValues(t, 999, immediate["id"])
		fake.Advance(time.Second)
		body["effective_at"] = nil // Null has the same immediate semantics as omission.
		encoded, err = common.Marshal(body)
		require.NoError(t, err)
		assert.EqualValues(t, now+7201, call("POST", "/api/admin/v1/price-versions", string(encoded), 200)["data"].(map[string]any)["effective_at"])
		scheduled := call("POST", "/api/admin/v1/price-versions", priceBody("model_offering", owner.ID, `tier("request", fixed(0))`, now+10000), 200)["data"].(map[string]any)
		assert.EqualValues(t, now+10000, scheduled["effective_at"])
		for _, invalid := range []any{0, -1, model.MaxPriceEffectiveAt + 1, "tomorrow", 1.5} {
			body["effective_at"] = invalid
			encoded, err = common.Marshal(body)
			require.NoError(t, err)
			call("POST", "/api/admin/v1/price-versions", string(encoded), 400)
		}
	})

	t.Run("future prices do not activate owners and free current prices do", func(t *testing.T) {
		call("POST", "/api/admin/v1/price-versions", priceBody("channel", channel.ID, `v1:tier("future", p * 2 + c * 8)`, now+3600), 200)
		call("POST", "/api/admin/v1/price-versions", priceBody("model_offering", offering.ID, `tier("future", fixed(0.2))`, now+3600), 200)
		call("GET", current, "", 404)
		call("POST", channelPath+"/enable", `{}`, 400)
		call("POST", offeringPath+"/enable", `{}`, 400)
		expression := `v1:tier("free", fixed(0)) * image_count`
		created := call("POST", "/api/admin/v1/price-versions", priceBody("channel", channel.ID, expression, now), 200)["data"].(map[string]any)
		assert.Equal(t, "EUR", created["currency"])
		assert.Equal(t, expression, created["expression"])
		assert.EqualValues(t, actor.Id, created["created_by"])
		assert.EqualValues(t, now, created["created_at"])
		assert.NotEqualValues(t, 999, created["id"])
		call("POST", "/api/admin/v1/price-versions", priceBody("model_offering", offering.ID, `tier("free", fixed(0))`, now), 200)
		call("POST", channelPath+"/enable", `{}`, 200)
		call("POST", offeringPath+"/enable", `{}`, 200)
		assert.Equal(t, created, call("GET", current, "", 200)["data"])
		assert.Equal(t, created, call("GET", fmt.Sprintf("/api/admin/v1/price-versions/%v", created["id"]), "", 200)["data"])
		call("POST", "/api/admin/v1/price-versions", priceBody("channel", channel.ID, `p * 1`, now), 409)
		future := call("GET", current+fmt.Sprintf("&at=%d", now+3600), "", 200)["data"].(map[string]any)
		assert.EqualValues(t, now+3600, future["effective_at"])
		assert.Equal(t, created, call("GET", current+fmt.Sprintf("&at=%d", now+3599), "", 200)["data"])
		fake.Advance(time.Hour)
		assert.Equal(t, future, call("GET", current, "", 200)["data"])
		versions := call("GET", fmt.Sprintf("/api/admin/v1/price-versions?owner_type=channel&owner_id=%d&p=1&page_size=1", channel.ID), "", 200)["data"].(map[string]any)
		assert.EqualValues(t, 2, versions["total"])
		require.Len(t, versions["items"], 1)
		assert.Equal(t, future, versions["items"].([]any)[0])
		// A second migration preserves both historical rows and append-only guards.
		require.NoError(t, model.MigratePricing(db))
		for _, statement := range []string{
			fmt.Sprintf(`UPDATE "price_versions" SET "expression" = '0' WHERE "id" = %v`, created["id"]),
			fmt.Sprintf(`DELETE FROM "price_versions" WHERE "id" = %v`, created["id"]),
			`TRUNCATE "price_versions"`,
		} {
			assert.Error(t, db.Exec(statement).Error)
		}
		assert.Equal(t, created, call("GET", fmt.Sprintf("/api/admin/v1/price-versions/%v", created["id"]), "", 200)["data"])
		for _, method := range []string{"PUT", "PATCH", "DELETE"} {
			response := adminSecurityRequest(engine, token, method, fmt.Sprintf("/api/admin/v1/price-versions/%v", created["id"]), `{}`)
			assert.NotEqual(t, 200, response.Code)
		}
		call("POST", channelPath+"/disable", `{}`, 200)
		assert.Equal(t, "The channel is still referenced", call("DELETE", channelPath, "", 409)["message_key"])
		assert.EqualValues(t, channel.ID, call("GET", channelPath, "", 200)["data"].(map[string]any)["id"])
		call("POST", offeringPath+"/disable", `{}`, 200)
		assert.Equal(t, "The ModelOffering is still referenced", call("DELETE", offeringPath, "", 409)["message_key"])
		assert.EqualValues(t, offering.ID, call("GET", offeringPath, "", 200)["data"].(map[string]any)["id"])
	})

	t.Run("invalid writes and queries fail without database details", func(t *testing.T) {
		for _, body := range []string{
			`{}`, `{"owner_type":"user","owner_id":1}`, `{"owner_type":"channel","owner_id":-1}`,
			fmt.Sprintf(`{"owner_type":"channel","owner_id":%d,"currency":"eur","expression":"0","effective_at":%d}`, channel.ID, now),
			fmt.Sprintf(`{"owner_type":"channel","owner_id":%d,"currency":"EUR","expression":"0","effective_at":0}`, channel.ID),
			priceBody("channel", channel.ID, `-1`, now+1), priceBody("channel", channel.ID, `1 / 0`, now+1),
			priceBody("channel", channel.ID, `v2:p * 1`, now+1), priceBody("channel", channel.ID, `tier("bad", fixed(1)) * p`, now+1),
			priceBody("channel", channel.ID, `u("seconds") * 2`, now+1),
		} {
			result := call("POST", "/api/admin/v1/price-versions", body, 400)
			assert.NotContains(t, fmt.Sprint(result), "SQLSTATE")
		}
		call("POST", "/api/admin/v1/price-versions", priceBody("channel", 999999, `0`, now), 404)
		for _, path := range []string{current + "&at=NaN", current + "&at=-1", current + "&at=", "/api/admin/v1/price-versions?owner_id=", "/api/admin/v1/price-versions?owner_type=", "/api/admin/v1/price-versions?owner_id=-1", "/api/admin/v1/price-versions?owner_type=unknown", "/api/admin/v1/price-versions/nope"} {
			call("GET", path, "", 400)
		}
		call("GET", "/api/admin/v1/price-versions/999999", "", 404)
	})

	t.Run("precise directional exchange quotes distinguish comparison and accounting", func(t *testing.T) {
		saved := call("PUT", "/api/admin/v1/fx-rates", `{"from_currency":"USD","to_currency":"EUR","rate":"0.923456789123456789"}`, 200)["data"].(map[string]any)
		assert.Equal(t, "0.02", saved["buffer_rate"])
		assert.Equal(t, "0.923456789123456789", saved["rate"])
		assert.EqualValues(t, actor.Id, saved["updated_by"])
		quote := call("GET", "/api/admin/v1/fx-rates/quote?from_currency=USD&to_currency=EUR", "", 200)["data"].(map[string]any)
		assert.Equal(t, "0.923456789123456789", quote["raw_rate"])
		assert.Equal(t, quote["raw_rate"], quote["accounting_rate"])
		assert.Equal(t, "0.94192592490592592478", quote["comparison_rate"])
		accounting, err := model.GetAccountingFXRate(t.Context(), db, "USD", "EUR")
		require.NoError(t, err)
		comparison, err := model.GetComparisonFXRate(t.Context(), db, "USD", "EUR")
		require.NoError(t, err)
		assert.Equal(t, quote["accounting_rate"], accounting.Decimal().String())
		assert.Equal(t, quote["comparison_rate"], comparison.Decimal().String())
		value, err := money.ParseValue("1", "USD")
		require.NoError(t, err)
		converted, err := value.Convert(accounting)
		require.NoError(t, err)
		assert.Equal(t, "0.923456789123456789 EUR", converted.String())
		call("GET", "/api/admin/v1/fx-rates/quote?from_currency=EUR&to_currency=USD", "", 404)
		call("GET", "/api/admin/v1/fx-rates/quote?from_currency=USD&to_currency=USD", "", 400)
		call("PUT", "/api/admin/v1/fx-rates", `{"from_currency":"USD","to_currency":"EUR","rate":"0.9","buffer_rate":"0"}`, 200)
		quote = call("GET", "/api/admin/v1/fx-rates/quote?from_currency=USD&to_currency=EUR", "", 200)["data"].(map[string]any)
		assert.Equal(t, "0.9", quote["comparison_rate"])
		list := call("GET", "/api/admin/v1/fx-rates", "", 200)["data"].(map[string]any)
		assert.EqualValues(t, 1, list["total"])
		for _, body := range []string{
			`{"from_currency":"USD","to_currency":"EUR","rate":0.9}`,
			`{"from_currency":"USD","to_currency":"EUR","rate":"1e-2"}`,
			`{"from_currency":"USD","to_currency":"EUR","rate":"0"}`,
			`{"from_currency":"USD","to_currency":"EUR","rate":"-1"}`,
			`{"from_currency":"USD","to_currency":"EUR","rate":"0.0000000000000000001"}`,
			`{"from_currency":"USD","to_currency":"EUR","rate":"1000000001"}`,
			`{"from_currency":"USD","to_currency":"EUR","rate":"1","buffer_rate":"1.01"}`,
			`{"from_currency":"USD","to_currency":"EUR","rate":"1","buffer_rate":"-0.01"}`,
			`{"from_currency":"USD","to_currency":"USD","rate":"1"}`,
		} {
			call("PUT", "/api/admin/v1/fx-rates", body, 400)
		}
	})

	t.Run("task prices validate selected schema and host usage bounds", func(t *testing.T) {
		previous := jsplugin.DefaultRegistry
		jsplugin.DefaultRegistry = jsplugin.NewRegistry()
		t.Cleanup(func() { jsplugin.DefaultRegistry = previous })
		source, err := plugins.Source("kling")
		require.NoError(t, err)
		source = strings.Replace(source, "  usageSchema: {", `  usageProfiles: [{models: ["kling-v2-6"], schema: {seconds: {type: "number", unit: "second", description: {en: "Video generation unit price"}}}}],
  usageSchema: {`, 1)
		plugin, err := jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "kling"})
		require.NoError(t, err)
		taskModel := model.Model{ModelName: "kling-v2-5-turbo", VendorID: publisher.Id, Status: 1}
		require.NoError(t, taskModel.Insert())
		taskChannel := model.Channel{ProviderID: provider.Id, ModelID: taskModel.Id, Protocol: "task:kling", UpstreamModelName: taskModel.ModelName, ParameterOverrides: `{}`, Status: model.ChannelPending}
		require.NoError(t, db.Omit("Model", "Provider").Create(&taskChannel).Error)
		schema, _ := plugin.Meta.UsageForModel(taskModel.ModelName)
		require.Equal(t, "number", schema["units"].Type)
		expression := `tier("task", u("units") * 0.4)`
		saved := call("POST", "/api/admin/v1/price-versions", priceBody("channel", taskChannel.ID, expression, now), 200)["data"].(map[string]any)
		assert.Equal(t, expression, saved["expression"])
		call("POST", "/api/admin/v1/price-versions", priceBody("channel", taskChannel.ID, `u("undeclared") * 1`, now+1), 400)
		call("POST", "/api/admin/v1/price-versions", priceBody("channel", taskChannel.ID, `tier("request", fixed(1))`, now+1), 400)
		taskOffering := model.ModelOffering{Name: "task-priced-offering", Members: []model.ModelOfferingMember{{ModelID: taskModel.Id, Tier: 1}}}
		require.NoError(t, model.SaveModelOffering(t.Context(), &taskOffering, "save"))
		call("POST", "/api/admin/v1/price-versions", priceBody("model_offering", taskOffering.ID, expression, now), 200)
		// Task schemas are available before the first Channel is configured.
		unserved := model.Model{ModelName: "KLING-V3", VendorID: publisher.Id, Status: 1}
		require.NoError(t, unserved.Insert())
		unservedOffering := model.ModelOffering{Name: "unserved-task-offering", Members: []model.ModelOfferingMember{{ModelID: unserved.Id, Tier: 1}}}
		require.NoError(t, model.SaveModelOffering(t.Context(), &unservedOffering, "save"))
		call("POST", "/api/admin/v1/price-versions", priceBody("model_offering", unservedOffering.ID, expression, now), 200)
		foldedChannel := model.Channel{ProviderID: provider.Id, ModelID: unserved.Id, Protocol: "task:kling", UpstreamModelName: "KLING-V3", ParameterOverrides: `{}`, Status: model.ChannelPending}
		require.NoError(t, db.Omit("Model", "Provider").Create(&foldedChannel).Error)
		call("POST", "/api/admin/v1/price-versions", priceBody("channel", foldedChannel.ID, expression, now), 200)
		profiled := model.Model{ModelName: "kling-v2-6", VendorID: publisher.Id, Status: 1}
		require.NoError(t, profiled.Insert())
		profiledProvider := model.Provider{Name: "Profiled provider", Type: 1, Status: common.ChannelStatusEnabled, Key: "not-exposed"}
		require.NoError(t, db.Create(&profiledProvider).Error)
		profiledChannel := model.Channel{ProviderID: profiledProvider.Id, ModelID: profiled.Id, Protocol: "task:kling", UpstreamModelName: "kling-v2-5-turbo", ParameterOverrides: `{}`, Status: model.ChannelPending}
		require.NoError(t, db.Omit("Model", "Provider").Create(&profiledChannel).Error)
		call("POST", "/api/admin/v1/price-versions", priceBody("channel", profiledChannel.ID, `u("seconds") * 0.1`, now), 200)
		call("POST", "/api/admin/v1/price-versions", priceBody("channel", profiledChannel.ID, expression, now+1), 400)

		for _, scope := range []model.OfferingChannelScope{
			{IncludeChannelIDs: []int{channel.ID}},
			{ExcludeChannelIDs: []int{taskChannel.ID}},
			{IncludeProviderIDs: []int{provider.Id}, ExcludeChannelIDs: []int{taskChannel.ID}},
		} {
			scoped := model.ModelOffering{Name: fmt.Sprintf("scoped-offering-%d", len(scope.IncludeProviderIDs)*2+len(scope.ExcludeChannelIDs)), Members: []model.ModelOfferingMember{{ModelID: unified.Id, Tier: 1}, {ModelID: taskModel.Id, Tier: 1}}, ChannelScope: scope}
			require.NoError(t, model.SaveModelOffering(t.Context(), &scoped, "save"))
			call("POST", "/api/admin/v1/price-versions", priceBody("model_offering", scoped.ID, `tier("request", fixed(0))`, now), 200)
		}

		unionScoped := model.ModelOffering{Name: "union-scoped-offering", Members: []model.ModelOfferingMember{{ModelID: taskModel.Id, Tier: 1}}, ChannelScope: model.OfferingChannelScope{IncludeProviderIDs: []int{profiledProvider.Id}, IncludeChannelIDs: []int{taskChannel.ID}}}
		require.NoError(t, model.SaveModelOffering(t.Context(), &unionScoped, "save"))
		call("POST", "/api/admin/v1/price-versions", priceBody("model_offering", unionScoped.ID, expression, now), 200)
		excludedProvider := model.ModelOffering{Name: "excluded-provider-offering", Members: []model.ModelOfferingMember{{ModelID: taskModel.Id, Tier: 1}}, ChannelScope: model.OfferingChannelScope{IncludeChannelIDs: []int{taskChannel.ID}, ExcludeProviderIDs: []int{provider.Id}}}
		require.NoError(t, model.SaveModelOffering(t.Context(), &excludedProvider, "save"))
		call("POST", "/api/admin/v1/price-versions", priceBody("model_offering", excludedProvider.ID, expression, now), 400)
		call("POST", "/api/admin/v1/price-versions", priceBody("model_offering", excludedProvider.ID, `tier("request", fixed(0))`, now), 200)
		deletedScopeChannel := model.Channel{ProviderID: provider.Id, ModelID: taskModel.Id, Protocol: "openai_chat", UpstreamModelName: "deleted-scope", ParameterOverrides: `{}`, Status: model.ChannelPending}
		require.NoError(t, db.Omit("Model", "Provider").Create(&deletedScopeChannel).Error)
		deletedScope := model.ModelOffering{Name: "deleted-scope-offering", Members: []model.ModelOfferingMember{{ModelID: taskModel.Id, Tier: 1}}, ChannelScope: model.OfferingChannelScope{IncludeChannelIDs: []int{deletedScopeChannel.ID}}}
		require.NoError(t, model.SaveModelOffering(t.Context(), &deletedScope, "save"))
		call("DELETE", fmt.Sprintf("/api/admin/v1/channels/%d", deletedScopeChannel.ID), "", 200)
		call("POST", "/api/admin/v1/price-versions", priceBody("model_offering", deletedScope.ID, expression, now), 400)
		call("POST", "/api/admin/v1/price-versions", priceBody("model_offering", deletedScope.ID, `tier("request", fixed(0))`, now), 200)

		require.NoError(t, authz.SetUserPermissions(actor.Id, authz.PermissionsMap{"task_plugin": {"bind": true}}))
		token = createWalletTestAccessToken(t, actor.Id, "pricing:read", "pricing:write", "model:read", "model:write", "channel:read", "channel:write", "channel:operate", "task_plugin:bind")
		mutableChannel, err := model.MutateChannel(t.Context(), 0, model.ChannelSave, func(row *model.Channel) error {
			*row = model.Channel{ProviderID: provider.Id, ModelID: taskModel.Id, Protocol: "openai_chat", UpstreamModelName: "mutable", ParameterOverrides: `{}`}
			return nil
		})
		require.NoError(t, err)
		mutablePath := fmt.Sprintf("/api/admin/v1/channels/%d", mutableChannel.ID)
		call("POST", "/api/admin/v1/price-versions", priceBody("channel", mutableChannel.ID, `tier("request", fixed(0))`, now), 200)
		call("PATCH", mutablePath, `{"protocol":"task:kling","upstream_model_name":"endpoint-mutable"}`, 200)
		call("POST", mutablePath+"/confirm", `{}`, 200)
		call("POST", mutablePath+"/enable", `{}`, 400)
		assert.Equal(t, "disabled", call("GET", mutablePath, "", 200)["data"].(map[string]any)["status"])
		call("POST", "/api/admin/v1/price-versions", priceBody("channel", mutableChannel.ID, `u("units") * 0`, clock.Now().Unix()+1), 200)
		fake.Advance(time.Second)
		call("POST", mutablePath+"/enable", `{}`, 200)

		mutableOffering := model.ModelOffering{Name: "mutable-priced-offering", Members: []model.ModelOfferingMember{{ModelID: unified.Id, Tier: 1}}, ChannelScope: model.OfferingChannelScope{IncludeChannelIDs: []int{channel.ID}}}
		require.NoError(t, model.SaveModelOffering(t.Context(), &mutableOffering, "save"))
		mutableOfferingPath := fmt.Sprintf("/api/admin/v1/model-offerings/%d", mutableOffering.ID)
		oldPrice := call("POST", "/api/admin/v1/price-versions", priceBody("model_offering", mutableOffering.ID, `tier("request", fixed(0))`, now), 200)["data"]
		call("POST", mutableOfferingPath+"/enable", `{}`, 200)
		before := call("GET", mutableOfferingPath, "", 200)["data"]
		changed := fmt.Sprintf(`{"name":"mutable-priced-offering","members":[{"model_id":%d,"tier":1}],"channel_scope":{}}`, taskModel.Id)
		call("PUT", mutableOfferingPath, changed, 400)
		assert.Equal(t, before, call("GET", mutableOfferingPath, "", 200)["data"])
		assert.Equal(t, oldPrice, call("GET", fmt.Sprintf("/api/admin/v1/price-versions/current?owner_type=model_offering&owner_id=%d", mutableOffering.ID), "", 200)["data"])
		call("POST", mutableOfferingPath+"/disable", `{}`, 200)
		call("PUT", mutableOfferingPath, changed, 200)
		call("POST", mutableOfferingPath+"/enable", `{}`, 400)
		call("POST", "/api/admin/v1/price-versions", priceBody("model_offering", mutableOffering.ID, `u("units") * 0`, clock.Now().Unix()+1), 200)
		fake.Advance(time.Second)
		call("POST", mutableOfferingPath+"/enable", `{}`, 200)

		unavailableScoped := model.ModelOffering{Name: "unavailable-scoped-offering", Members: []model.ModelOfferingMember{{ModelID: unified.Id, Tier: 1}, {ModelID: taskModel.Id, Tier: 1}}, ChannelScope: model.OfferingChannelScope{IncludeChannelIDs: []int{channel.ID}}}
		require.NoError(t, model.SaveModelOffering(t.Context(), &unavailableScoped, "save"))
		jsplugin.DefaultRegistry.SetEnabled(false)
		call("POST", "/api/admin/v1/price-versions", priceBody("channel", taskChannel.ID, expression, now+1), 400)
		call("POST", "/api/admin/v1/price-versions", priceBody("model_offering", unavailableScoped.ID, `tier("request", fixed(0))`, now+2), 200)
	})

	t.Run("pricing scope Casbin and centralized audit protect mutations", func(t *testing.T) {
		limited := createWalletTestAccessToken(t, actor.Id, "pricing:read")
		response := adminSecurityRequest(engine, limited, "PUT", "/api/admin/v1/fx-rates", `{}`)
		assert.Equal(t, 403, response.Code)
		require.NoError(t, authz.SetUserPermissions(actor.Id, authz.PermissionsMap{"pricing": {"write": false}}))
		response = adminSecurityRequest(engine, token, "PUT", "/api/admin/v1/fx-rates", `{}`)
		assert.Equal(t, 403, response.Code)
		logs, _, err := model.GetAuditLogs(model.AuditLogFilter{Category: model.AuditCategoryOperation}, 0, 1000, common.RoleRootUser)
		require.NoError(t, err)
		successes := map[string]bool{}
		failures := map[string]bool{}
		for _, entry := range logs {
			if entry.Success {
				successes[entry.Action] = true
			} else {
				failures[entry.Action] = true
			}
		}
		assert.True(t, successes["price_version.create"])
		assert.True(t, successes["fx_rate.save"])
		assert.True(t, failures["price_version.create"])
		assert.True(t, failures["fx_rate.save"])
	})
}
