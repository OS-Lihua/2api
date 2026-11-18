package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDashboardModelListsUseProviderAdaptorMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalRegistry := jsplugin.DefaultRegistry
	jsplugin.DefaultRegistry = jsplugin.NewRegistry()
	t.Cleanup(func() { jsplugin.DefaultRegistry = originalRegistry })
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	DashboardListModels(ctx)
	require.Equal(t, http.StatusOK, response.Code)
	var result struct {
		Success bool             `json:"success"`
		Data    map[int][]string `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
	require.True(t, result.Success)
	for _, tc := range []struct {
		kind int
		name string
	}{
		{constant.ChannelTypeOpenAI, "openai"},
		{constant.ChannelTypeCodex, "codex"},
		{constant.ChannelTypeOpenRouter, "openrouter"},
		{constant.ChannelTypeAzure, "azure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			apiType, ok := common.ChannelType2APIType(tc.kind)
			if tc.kind == constant.ChannelTypeAzure {
				require.False(t, ok, "unmapped legacy types must not invent an adaptor catalog")
				assert.NotContains(t, result.Data, tc.kind)
				return
			}
			require.True(t, ok)
			adaptor := relay.GetAdaptor(apiType)
			require.NotNil(t, adaptor)
			adaptor.Init(&relaycommon.RelayInfo{ProviderMeta: &relaycommon.ProviderMeta{ChannelType: tc.kind}})
			assert.Equal(t, tc.name, adaptor.GetChannelName())
			assert.Equal(t, append([]string(nil), adaptor.GetModelList()...), result.Data[tc.kind])
		})
	}
}

func TestOfferingModelIdentityOverridesStaticProviderMetadata(t *testing.T) {
	db, _, token := newOfferingDiscoveryFixture(t)
	offering := createDiscoveryOffering(t, db, "gpt-5.4", true)
	const created = 1720000017
	require.NoError(t, db.Model(offering).Update("created_at", created).Error)
	payload := decodeListModelsPayload(t, requestOfferingModels(t, token, constant.ChannelTypeOpenAI))
	require.Len(t, payload.Data, 1)
	assert.Equal(t, "gpt-5.4", payload.Data[0].Id)
	assert.Equal(t, "system", payload.Data[0].OwnedBy)
	assert.Equal(t, created, payload.Data[0].Created, "Offering identity must not use the old static model timestamp")
}

func TestOfferingModelDiscoveryDoesNotInventUnknownModels(t *testing.T) {
	db, _, token := newOfferingDiscoveryFixture(t)
	require.Empty(t, decodeListModelsPayload(t, requestOfferingModels(t, token, constant.ChannelTypeOpenAI)).Data)
	offering := createDiscoveryOffering(t, db, "custom-test-model", true)
	payload := decodeListModelsPayload(t, requestOfferingModels(t, token, constant.ChannelTypeOpenAI))
	require.Len(t, payload.Data, 1)
	assert.Equal(t, offering.Name, payload.Data[0].Id)
	assert.Equal(t, "system", payload.Data[0].OwnedBy)
	require.NoError(t, db.Model(offering).Update("enabled", false).Error)
	assert.Empty(t, decodeListModelsPayload(t, requestOfferingModels(t, token, constant.ChannelTypeOpenAI)).Data, "static/custom fallback cannot resurrect a disabled Offering")
}

func TestGetModelListGroupsUsesUserGroupWhenTokenGroupIsEmpty(t *testing.T) {
	db, _, token := newOfferingDiscoveryFixture(t)
	createDiscoveryOffering(t, db, "default-offering", true, "default")
	createDiscoveryOffering(t, db, "vip-offering", true, "vip")
	require.Equal(t, map[string]struct{}{"default-offering": {}}, decodeListModelsResponse(t, requestOfferingModels(t, token, constant.ChannelTypeOpenAI, func(c *gin.Context) { common.SetContextKey(c, constant.ContextKeyTokenGroup, "") })))
}

func TestGetModelListGroupsUsesExplicitTokenGroup(t *testing.T) {
	db, _, token := newOfferingDiscoveryFixture(t)
	createDiscoveryOffering(t, db, "default-offering", true, "default")
	createDiscoveryOffering(t, db, "vip-offering", true, "vip")
	require.Equal(t, map[string]struct{}{"default-offering": {}}, decodeListModelsResponse(t, requestOfferingModels(t, token, constant.ChannelTypeOpenAI, func(c *gin.Context) { common.SetContextKey(c, constant.ContextKeyTokenGroup, "vip") })), "retired token group is not authority to see another user group's Offering")
}

func TestGetModelListGroupsUsesFilteredTokenAutoGroupsSnapshot(t *testing.T) {
	db, user, token := newOfferingDiscoveryFixture(t)
	require.NoError(t, db.Model(user).Update("group", "vip").Error)
	createDiscoveryOffering(t, db, "vip-offering", true, "vip")
	retired := func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyUserGroup, "vip")
		common.SetContextKey(c, constant.ContextKeyTokenGroup, "auto")
		common.SetContextKey(c, constant.ContextKeyTokenAutoGroups, []string{"vip", "default"})
	}
	require.Equal(t, map[string]struct{}{"vip-offering": {}}, decodeListModelsResponse(t, requestOfferingModels(t, token, constant.ChannelTypeOpenAI, retired)))
	require.NoError(t, db.Model(user).Update("group", "default").Error)
	require.Empty(t, decodeListModelsResponse(t, requestOfferingModels(t, token, constant.ChannelTypeOpenAI, retired)), "a cached auto-group list cannot preserve revoked live visibility")
}
