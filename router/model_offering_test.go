package router

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// A configured free price satisfies activation just like a non-zero price.
// Actual price-version persistence is exercised when #33 supplies this seam.
type freeOfferingSalePrice struct{}

func (freeOfferingSalePrice) HasSalePrice(context.Context, *gorm.DB, *model.ModelOffering) (bool, error) {
	return true, nil
}

func TestModelOfferingAPI(t *testing.T) {
	model.SetOfferingSalePriceChecker(nil)
	t.Cleanup(func() { model.SetOfferingSalePriceChecker(nil) })
	master, redis := common.IsMasterNode, common.RedisEnabled
	t.Cleanup(func() { common.IsMasterNode, common.RedisEnabled = master, redis })
	common.IsMasterNode, common.RedisEnabled = true, false
	db := model.UseTestDatabases(t)
	require.NoError(t, i18n.Init())
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.UserSession{}, &model.CasbinRule{}, &model.AuthzRole{}, &model.UserAccessToken{}, &model.Option{}, &model.Provider{}, &model.Channel{}, &model.Model{}, &model.Vendor{}, &model.ModelOffering{}, &model.ModelOfferingName{}, &model.ModelOfferingMember{}, &model.Ability{}, &model.TaskPlugin{}))
	require.NoError(t, model.MigrateAuditLogs())
	require.NoError(t, model.EnsureLegacyAccessTokenRetireAt(time.Now().Unix()))
	require.NoError(t, authz.Init(db))
	root := model.User{Username: "offering-root", Password: "placeholder", Role: common.RoleRootUser, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: "offering-root"}
	require.NoError(t, db.Create(&root).Error)
	token := createWalletTestAccessToken(t, root.Id, "model:read", "model:write")
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	SetApiRouter(engine)
	call := func(method, endpoint, body string) (int, map[string]any) {
		request := httptest.NewRequest(method, endpoint, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, request)
		response := map[string]any{}
		if recorder.Header().Get("Content-Type") != "" && strings.Contains(recorder.Header().Get("Content-Type"), "json") {
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response), recorder.Body.String())
		}
		return recorder.Code, response
	}
	publisher := model.Vendor{Name: "Offering publisher", Status: 1}
	require.NoError(t, publisher.Insert())
	unified := model.Model{ModelName: "offering-model", VendorID: publisher.Id, Status: 1}
	require.NoError(t, unified.Insert())
	body := fmt.Sprintf(`{"name":"public-model","aliases":["public-alias"],"members":[{"model_id":%d,"tier":1}]}`, unified.Id)
	status, response := call(http.MethodPost, "/api/admin/v1/model-offerings", body)
	require.Equal(t, http.StatusOK, status, response)
	require.Equal(t, true, response["success"], response)
	data := response["data"].(map[string]any)
	assert.Equal(t, "public-model", data["name"])
	assert.Equal(t, []any{"public-alias"}, data["aliases"])
	assert.Equal(t, false, data["enabled"])
	assert.Equal(t, "all", data["visibility"])
	assert.Equal(t, "cost", data["route_method"])
	assert.Equal(t, "0", data["minimum_margin_rate"])
	assert.Equal(t, false, data["allow_loss_fallback"])
	endpoint := fmt.Sprintf("/api/admin/v1/model-offerings/%v", data["id"])
	status, response = call(http.MethodGet, endpoint, "")
	require.Equal(t, http.StatusOK, status, response)
	assert.Equal(t, data, response["data"])

	t.Run("metadata sync preserves offering members without Channels", func(t *testing.T) {
		status, response := call(http.MethodPost, "/api/admin/v1/models", fmt.Sprintf(`{"name":"sync-member","publisher_id":%d,"description":"Local description","sync_official":1,"capabilities":{"features":["streaming"],"input_modalities":["text"],"output_modalities":["text"]}}`, publisher.Id))
		require.Equal(t, http.StatusOK, status, response)
		member := response["data"].(map[string]any)
		memberID := int(member["id"].(float64))
		memberEndpoint := fmt.Sprintf("/api/admin/v1/models/%d", memberID)
		companion := model.Model{ModelName: "sync-companion", VendorID: publisher.Id, Description: "Keep companion", Status: 1, SyncOfficial: 1}
		require.NoError(t, companion.Insert())
		companionEndpoint := fmt.Sprintf("/api/admin/v1/models/%d", companion.Id)
		status, response = call(http.MethodGet, companionEndpoint, "")
		require.Equal(t, http.StatusOK, status, response)
		companionBefore := response["data"]
		status, response = call(http.MethodPost, "/api/admin/v1/model-offerings", fmt.Sprintf(`{"name":"sync-offering","aliases":["sync-alias"],"members":[{"model_id":%d,"tier":1}]}`, memberID))
		require.Equal(t, http.StatusOK, status, response)
		offering := response["data"]
		offeringEndpoint := fmt.Sprintf("/api/admin/v1/model-offerings/%v", offering.(map[string]any)["id"])
		var channels int64
		require.NoError(t, db.Model(&model.Channel{}).Where("model_id = ?", memberID).Count(&channels).Error)
		require.Zero(t, channels, "the member reference alone must protect Model identity")

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if strings.HasSuffix(r.URL.Path, "vendors.json") {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{"model_name":"sync-companion","description":"Must roll back","name_rule":0,"status":1},{"model_name":"sync-member","description":"Synced description","name_rule":1,"status":1,"tags":"synced"}]`))
		}))
		defer upstream.Close()
		t.Setenv("SYNC_UPSTREAM_BASE", upstream.URL)
		t.Setenv("SYNC_HTTP_RETRY", "1")
		status, response = call(http.MethodGet, "/api/models/sync_upstream/preview?locale=en", "")
		require.Equal(t, http.StatusOK, status, response)
		require.Equal(t, true, response["success"], response)
		preview := response["data"].(map[string]any)
		versions := make(map[string]string)
		for _, item := range preview["candidates"].([]any) {
			candidate := item.(map[string]any)
			versions[candidate["model_name"].(string)] = candidate["record_version"].(string)
		}
		require.NotEmpty(t, versions["sync-member"])
		require.NotEmpty(t, versions["sync-companion"])
		selection := model.MetadataSyncSelection{ModelName: "sync-member", RecordVersion: versions["sync-member"], Fields: []string{"name_rule", "description", "tags"}}
		request := map[string]any{"locale": "en", "source_version": preview["source"].(map[string]any)["version"], "selections": []model.MetadataSyncSelection{
			{ModelName: "sync-companion", RecordVersion: versions["sync-companion"], Fields: []string{"description"}},
			selection,
		}}
		encoded, err := common.Marshal(request)
		require.NoError(t, err)
		status, response = call(http.MethodPost, "/api/models/sync_upstream", string(encoded))
		require.Equal(t, http.StatusBadRequest, status, response)
		assert.Equal(t, false, response["success"], response)
		status, response = call(http.MethodGet, companionEndpoint, "")
		require.Equal(t, http.StatusOK, status, response)
		assert.Equal(t, companionBefore, response["data"], "the entire batch must roll back")
		status, response = call(http.MethodGet, memberEndpoint, "")
		require.Equal(t, http.StatusOK, status, response)
		assert.Equal(t, member, response["data"], "failed sync must preserve name, Publisher, capabilities and metadata")
		status, response = call(http.MethodGet, offeringEndpoint, "")
		require.Equal(t, http.StatusOK, status, response)
		assert.Equal(t, offering, response["data"], "failed sync must preserve member tiers and names")

		selection.Fields = []string{"description", "tags"}
		request["selections"] = []model.MetadataSyncSelection{selection}
		encoded, err = common.Marshal(request)
		require.NoError(t, err)
		status, response = call(http.MethodPost, "/api/models/sync_upstream", string(encoded))
		require.Equal(t, http.StatusOK, status, response)
		require.Equal(t, true, response["success"], response)
		status, response = call(http.MethodGet, memberEndpoint, "")
		require.Equal(t, http.StatusOK, status, response)
		synced := response["data"].(map[string]any)
		assert.Equal(t, "Synced description", synced["description"])
		assert.Equal(t, "synced", synced["tags"])
		assert.Equal(t, member["name"], synced["name"])
		assert.Equal(t, member["publisher_id"], synced["publisher_id"])
		assert.Equal(t, member["capabilities"], synced["capabilities"])
		status, response = call(http.MethodPut, offeringEndpoint, fmt.Sprintf(`{"name":"sync-offering","aliases":["sync-alias"],"members":[{"model_id":%d,"tier":1}]}`, memberID))
		require.Equal(t, http.StatusOK, status, response)
		saved := response["data"].(map[string]any)
		for _, field := range []string{"name", "aliases", "members"} {
			assert.Equal(t, offering.(map[string]any)[field], saved[field], field)
		}
	})

	t.Run("primary names and aliases share a unique namespace", func(t *testing.T) {
		for _, name := range []string{"public-model", "public-alias"} {
			duplicate := fmt.Sprintf(`{"name":%q,"members":[{"model_id":%d,"tier":1}]}`, name, unified.Id)
			status, response := call(http.MethodPost, "/api/admin/v1/model-offerings", duplicate)
			assert.Equal(t, http.StatusConflict, status, response)
		}
		status, response := call(http.MethodGet, "/api/admin/v1/model-offerings?keyword=public-alias", "")
		require.Equal(t, http.StatusOK, status, response)
		assert.EqualValues(t, 1, response["data"].(map[string]any)["total"])
	})

	t.Run("renames preserve the old primary name and conflicts roll back", func(t *testing.T) {
		status, response := call(http.MethodPut, endpoint, fmt.Sprintf(`{"name":"renamed-model","members":[{"model_id":%d,"tier":1}],"aliases":["public-alias"]}`, unified.Id))
		require.Equal(t, http.StatusOK, status, response)
		assert.Equal(t, []any{"public-alias", "public-model"}, response["data"].(map[string]any)["aliases"])
		status, response = call(http.MethodPost, "/api/admin/v1/model-offerings", fmt.Sprintf(`{"name":"another-model","members":[{"model_id":%d,"tier":1}],"aliases":["reserved-alias"]}`, unified.Id))
		require.Equal(t, http.StatusOK, status, response)
		status, response = call(http.MethodPut, endpoint, fmt.Sprintf(`{"name":"reserved-alias","members":[{"model_id":%d,"tier":1}]}`, unified.Id))
		assert.Equal(t, http.StatusConflict, status, response)
		status, response = call(http.MethodGet, endpoint, "")
		require.Equal(t, http.StatusOK, status, response)
		unchanged := response["data"].(map[string]any)
		assert.Equal(t, "renamed-model", unchanged["name"])
		assert.Equal(t, []any{"public-alias", "public-model"}, unchanged["aliases"])
		assert.Len(t, unchanged["members"], 1)
	})

	t.Run("configuration preserves member tiers scope and exact margin", func(t *testing.T) {
		second := model.Model{ModelName: "offering-model-second", VendorID: publisher.Id, Status: 1}
		require.NoError(t, second.Insert())
		third := model.Model{ModelName: "offering-model-third", VendorID: publisher.Id, Status: 1}
		require.NoError(t, third.Insert())
		provider := model.Provider{Name: "offering-provider", Key: "private-provider-secret", Status: common.ChannelStatusEnabled, Type: 1}
		require.NoError(t, db.Create(&provider).Error)
		channel, err := model.MutateChannel(t.Context(), 0, model.ChannelSave, func(record *model.Channel) error {
			*record = model.Channel{ProviderID: provider.Id, ModelID: second.Id, Protocol: "openai_chat", UpstreamModelName: "upstream", ParameterOverrides: `{}`, Weight: 1}
			return nil
		})
		require.NoError(t, err)
		configured := fmt.Sprintf(`{"name":"renamed-model","aliases":["public-alias","public-model"],"visibility":"groups","groups":["default","paid"],"members":[{"model_id":%d,"tier":1},{"model_id":%d,"tier":1},{"model_id":%d,"tier":2}],"channel_scope":{"include_provider_ids":[%d],"exclude_channel_ids":[%d]},"route_method":"weight","accepted_conversion_losses":["tool_description_dropped"],"minimum_margin_rate":"0.123456789123456789","allow_loss_fallback":true}`, unified.Id, second.Id, third.Id, provider.Id, channel.ID)
		status, response := call(http.MethodPut, endpoint, configured)
		require.Equal(t, http.StatusOK, status, response)
		record := response["data"].(map[string]any)
		assert.Equal(t, "groups", record["visibility"])
		assert.Equal(t, []any{"default", "paid"}, record["groups"])
		assert.Len(t, record["members"], 3)
		assert.Equal(t, "weight", record["route_method"])
		assert.Equal(t, "0.123456789123456789", record["minimum_margin_rate"])
		assert.Equal(t, true, record["allow_loss_fallback"])
		assert.Equal(t, []any{"tool_description_dropped"}, record["accepted_conversion_losses"])
		scope := record["channel_scope"].(map[string]any)
		assert.Equal(t, []any{float64(provider.Id)}, scope["include_provider_ids"])
		assert.Equal(t, []any{float64(channel.ID)}, scope["exclude_channel_ids"])
		assert.Equal(t, []any{}, scope["exclude_provider_ids"])
		status, response = call(http.MethodGet, "/api/admin/v1/model-offerings?keyword=public-model", "")
		require.Equal(t, http.StatusOK, status, response)
		assert.Len(t, response["data"].(map[string]any)["items"], 1)
		item := response["data"].(map[string]any)["items"].([]any)[0].(map[string]any)
		assert.Equal(t, record, item)
		for _, invalid := range []struct {
			name  string
			field string
			value any
		}{
			{"invalid route", "route_method", "random"},
			{"negative margin", "minimum_margin_rate", "-0.01"},
			{"margin above one", "minimum_margin_rate", "1.01"},
			{"invalid margin", "minimum_margin_rate", "NaN"},
			{"no groups", "groups", []string{}},
			{"no members", "members", []any{}},
			{"missing model", "members", []map[string]int{{"model_id": 999999, "tier": 1}}},
			{"duplicate member", "members", []map[string]int{{"model_id": unified.Id, "tier": 1}, {"model_id": unified.Id, "tier": 2}}},
			{"no first tier", "members", []map[string]int{{"model_id": unified.Id, "tier": 2}}},
			{"invalid tier", "members", []map[string]int{{"model_id": unified.Id, "tier": 0}}},
			{"negative scope", "channel_scope", map[string][]int{"include_provider_ids": {-1}}},
			{"missing provider", "channel_scope", map[string][]int{"include_provider_ids": {999999}}},
			{"missing channel", "channel_scope", map[string][]int{"exclude_channel_ids": {999999}}},
			{"conflicting scope", "channel_scope", map[string][]int{"include_provider_ids": {provider.Id}, "exclude_provider_ids": {provider.Id}}},
			{"empty alias", "aliases", []string{""}},
			{"whitespace name", "name", "two words"},
		} {
			t.Run(invalid.name, func(t *testing.T) {
				var declaration map[string]any
				require.NoError(t, common.UnmarshalJsonStr(configured, &declaration))
				declaration[invalid.field] = invalid.value
				encoded, err := common.Marshal(declaration)
				require.NoError(t, err)
				status, response := call(http.MethodPut, endpoint, string(encoded))
				assert.Equal(t, http.StatusBadRequest, status, response)
			})
		}
		status, response = call(http.MethodGet, endpoint, "")
		require.Equal(t, http.StatusOK, status, response)
		assert.Equal(t, record, response["data"], "rejected edits must leave the entire declaration intact")
	})

	t.Run("sale price required for activation and JSON cannot bypass it", func(t *testing.T) {
		status, response := call(http.MethodPost, endpoint+"/enable", "")
		assert.Equal(t, http.StatusBadRequest, status, response)
		status, response = call(http.MethodPut, endpoint, fmt.Sprintf(`{"name":"renamed-model","members":[{"model_id":%d,"tier":1}],"enabled":true,"id":999999}`, unified.Id))
		require.Equal(t, http.StatusOK, status, response)
		assert.Equal(t, false, response["data"].(map[string]any)["enabled"])
		assert.Equal(t, data["id"], response["data"].(map[string]any)["id"])
		model.SetOfferingSalePriceChecker(freeOfferingSalePrice{})
		defer model.SetOfferingSalePriceChecker(nil)
		status, response = call(http.MethodPost, endpoint+"/enable", "")
		require.Equal(t, http.StatusOK, status, response)
		assert.Equal(t, true, response["data"].(map[string]any)["enabled"])
		status, response = call(http.MethodPost, endpoint+"/disable", "")
		require.Equal(t, http.StatusOK, status, response)
		assert.Equal(t, false, response["data"].(map[string]any)["enabled"])
	})

	t.Run("read scopes cannot modify any offering action", func(t *testing.T) {
		original := token
		token = createWalletTestAccessToken(t, root.Id, "model:read")
		defer func() { token = original }()
		status, response := call(http.MethodGet, endpoint, "")
		require.Equal(t, http.StatusOK, status, response)
		for _, action := range []struct{ method, endpoint, body string }{
			{http.MethodPost, "/api/admin/v1/model-offerings", body},
			{http.MethodPut, endpoint, body}, {http.MethodDelete, endpoint, ""},
			{http.MethodPost, endpoint + "/enable", ""}, {http.MethodPost, endpoint + "/disable", ""},
		} {
			status, response := call(action.method, action.endpoint, action.body)
			assert.Equal(t, http.StatusForbidden, status, response)
		}
		token = "invalid"
		status, _ = call(http.MethodGet, endpoint, "")
		assert.Equal(t, http.StatusUnauthorized, status)
		customer := model.User{Username: "offering-customer", Password: "placeholder", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: "offering-customer"}
		require.NoError(t, db.Create(&customer).Error)
		token = createWalletTestAccessToken(t, customer.Id, "model:read", "model:write")
		status, _ = call(http.MethodGet, endpoint, "")
		assert.Equal(t, http.StatusForbidden, status)
	})

	t.Run("Casbin restrictions and session revocation are enforced", func(t *testing.T) {
		original := token
		defer func() { token = original }()
		admin := model.User{Username: "offering-admin", Password: "placeholder", Role: common.RoleAdminUser, Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: "offering-admin"}
		require.NoError(t, db.Create(&admin).Error)
		token = createWalletTestAccessToken(t, admin.Id, "model:read", "model:write")
		status, response := call(http.MethodGet, endpoint, "")
		require.Equal(t, http.StatusOK, status, response)
		require.NoError(t, authz.SetUserPermissions(admin.Id, authz.PermissionsMap{"model": {"read": false, "write": false}}))
		status, response = call(http.MethodGet, endpoint, "")
		assert.Equal(t, http.StatusForbidden, status, response)
		status, response = call(http.MethodPost, endpoint+"/disable", "")
		assert.Equal(t, http.StatusForbidden, status, response)
		bundle, err := service.CreateLoginSession(root.Id, "password", "127.0.0.1", "offering-api-test")
		require.NoError(t, err)
		token = bundle.AccessToken
		status, response = call(http.MethodGet, endpoint, "")
		require.Equal(t, http.StatusOK, status, response)
		_, err = model.RevokeUserSession(root.Id, bundle.Session.SID, "offering-test")
		require.NoError(t, err)
		status, response = call(http.MethodGet, endpoint, "")
		assert.Equal(t, http.StatusUnauthorized, status, response)
	})

	t.Run("an offering can be disabled after a scoped Channel is deleted", func(t *testing.T) {
		provider := model.Provider{Name: "offering-deleted-scope", Key: "test-secret", Type: 1, Status: common.ChannelStatusEnabled}
		require.NoError(t, db.Create(&provider).Error)
		channel, err := model.MutateChannel(t.Context(), 0, model.ChannelSave, func(record *model.Channel) error {
			*record = model.Channel{ProviderID: provider.Id, ModelID: unified.Id, Protocol: "openai_chat", UpstreamModelName: "deleted-scope", ParameterOverrides: `{}`, Weight: 1}
			return nil
		})
		require.NoError(t, err)
		declaration := fmt.Sprintf(`{"name":"deleted-scope-offering","members":[{"model_id":%d,"tier":1}],"channel_scope":{"include_channel_ids":[%d]}}`, unified.Id, channel.ID)
		status, response := call(http.MethodPost, "/api/admin/v1/model-offerings", declaration)
		require.Equal(t, http.StatusOK, status, response)
		scopedEndpoint := fmt.Sprintf("/api/admin/v1/model-offerings/%v", response["data"].(map[string]any)["id"])
		model.SetOfferingSalePriceChecker(freeOfferingSalePrice{})
		defer model.SetOfferingSalePriceChecker(nil)
		status, response = call(http.MethodPost, scopedEndpoint+"/enable", "")
		require.Equal(t, http.StatusOK, status, response)
		original := token
		token = createWalletTestAccessToken(t, root.Id, "model:read", "model:write", "channel:write")
		defer func() { token = original }()
		status, response = call(http.MethodDelete, fmt.Sprintf("/api/admin/v1/channels/%d", channel.ID), "")
		require.Equal(t, http.StatusOK, status, response)
		status, response = call(http.MethodPost, scopedEndpoint+"/disable", "")
		require.Equal(t, http.StatusOK, status, response)
		assert.Equal(t, false, response["data"].(map[string]any)["enabled"])
		assert.Equal(t, []any{float64(channel.ID)}, response["data"].(map[string]any)["channel_scope"].(map[string]any)["include_channel_ids"], "a deleted include target must not turn into unrestricted routing")
		status, response = call(http.MethodDelete, scopedEndpoint, "")
		require.Equal(t, http.StatusOK, status, response)
	})

	t.Run("member Models retain their identity through legacy metadata edits", func(t *testing.T) {
		status, response := call(http.MethodPut, "/api/models/", fmt.Sprintf(`{"id":%d,"model_name":"offering-model","vendor_id":%d,"status":1,"name_rule":1}`, unified.Id, publisher.Id))
		require.Equal(t, http.StatusOK, status, response)
		require.Equal(t, false, response["success"], response)
		status, response = call(http.MethodGet, fmt.Sprintf("/api/admin/v1/models/%d", unified.Id), "")
		require.Equal(t, http.StatusOK, status, response)
		assert.Equal(t, "offering-model", response["data"].(map[string]any)["name"])
	})

	t.Run("alias count is bounded before persistence", func(t *testing.T) {
		aliases := make([]string, 1001)
		for i := range aliases {
			aliases[i] = fmt.Sprintf("alias-%d", i)
		}
		encoded, err := common.Marshal(map[string]any{"name": "too-many-aliases", "aliases": aliases, "members": []map[string]int{{"model_id": unified.Id, "tier": 1}}})
		require.NoError(t, err)
		status, response := call(http.MethodPost, "/api/admin/v1/model-offerings", string(encoded))
		assert.Equal(t, http.StatusBadRequest, status, response)
	})

	t.Run("writes leave audit events without usable credentials", func(t *testing.T) {
		entries, _, err := model.GetAuditLogs(model.AuditLogFilter{UserId: root.Id}, 0, 100, common.RoleRootUser)
		require.NoError(t, err)
		found := map[string]bool{}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Action, "model_offering.") {
				continue
			}
			found[entry.Action] = true
			encoded, err := common.Marshal(entry)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), token)
			assert.NotContains(t, string(encoded), "private-provider-secret")
			assert.Equal(t, root.Id, entry.UserId)
		}
		for _, action := range []string{"model_offering.create", "model_offering.update", "model_offering.enable", "model_offering.disable"} {
			assert.True(t, found[action], action)
		}
	})

	t.Run("member Models cannot be deleted while an offering uses them", func(t *testing.T) {
		status, response := call(http.MethodDelete, fmt.Sprintf("/api/admin/v1/models/%d", unified.Id), "")
		assert.Equal(t, http.StatusConflict, status, response)
		status, response = call(http.MethodGet, endpoint, "")
		require.Equal(t, http.StatusOK, status, response)
		assert.Len(t, response["data"].(map[string]any)["members"], 1)
	})

	status, response = call(http.MethodDelete, endpoint, "")
	require.Equal(t, http.StatusOK, status, response)
	status, response = call(http.MethodGet, endpoint, "")
	assert.Equal(t, http.StatusNotFound, status, response)
	status, response = call(http.MethodPost, "/api/admin/v1/model-offerings", fmt.Sprintf(`{"name":"renamed-model","members":[{"model_id":%d,"tier":1}]}`, unified.Id))
	assert.Equal(t, http.StatusOK, status, "deletion releases primary names and aliases: %v", response)
}
