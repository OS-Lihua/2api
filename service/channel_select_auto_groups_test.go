package service

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/protocol"
	kitdto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupChannelSelectAutoGroupsTest(t *testing.T) *gorm.DB {
	t.Helper()
	db := model.UseTestDatabases(t)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Provider{}, &model.Channel{}, &model.ModelOffering{}, &model.ModelOfferingMember{}, &model.TokenOfferingPolicy{}, &model.TokenOfferingException{}))
	require.NoError(t, model.MigratePricing(db))
	require.NoError(t, model.MigrateKeyPermissions(db))
	require.NoError(t, db.Create(&model.User{Id: 7, Username: "offering-route-owner", Group: "default", Status: common.UserStatusEnabled}).Error)
	require.NoError(t, db.Create(&model.Token{Id: 7, UserId: 7, Key: "selector-test-key", Status: common.TokenStatusEnabled, ExpiredTime: -1}).Error)
	require.False(t, db.Migrator().HasTable("abilities"), "runtime routing must not recreate retired projections")
	previousCache := common.MemoryCacheEnabled
	t.Cleanup(func() { common.MemoryCacheEnabled = previousCache })
	return db
}

func TestCacheGetRandomSatisfiedChannelUsesTokenAutoGroupsWhenGlobalAutoIsEmpty(t *testing.T) {
	// The former two-group positive route is now two explicit Offering member
	// tiers. Neither global nor token Auto-group order is an authority source.
	db := setupChannelSelectAutoGroupsTest(t)
	previousGroups := setting.AutoGroups2JsonString()
	require.NoError(t, setting.UpdateAutoGroupsByJsonString(`[]`))
	t.Cleanup(func() { require.NoError(t, setting.UpdateAutoGroupsByJsonString(previousGroups)) })
	offering := &model.ModelOffering{Name: "auto-groups-runtime-model", Visibility: "all", Groups: []string{}, Enabled: true, RouteMethod: "cost", AcceptedConversionLosses: []string{}}
	for index, group := range []string{"vip", "default"} {
		provider := &model.Provider{Id: 2101 + index, Type: constant.ChannelTypeOpenAI, Key: fmt.Sprintf("route-key-%d", index), Name: group, Status: common.ChannelStatusEnabled, Group: "retired-unrelated", Models: "retired-unrelated"}
		require.NoError(t, db.Create(provider).Error)
		metadata := &model.Model{ModelName: fmt.Sprintf("route-model-%d", index), Status: 1}
		require.NoError(t, db.Create(metadata).Error)
		channel := &model.Channel{ProviderID: provider.Id, ModelID: metadata.Id, Protocol: protocol.OpenAIChat, UpstreamModelName: metadata.ModelName, ParameterOverrides: `{}`, Status: model.ChannelEnabled, MappingConfirmed: true}
		require.NoError(t, db.Create(channel).Error)
		offering.Members = append(offering.Members, model.ModelOfferingMember{ModelID: metadata.Id, Tier: index + 1})
		require.NoError(t, model.CreatePriceVersion(t.Context(), &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: channel.ID, Currency: "USD", Expression: `tier("fixed", fixed(0.001))`, EffectiveAt: 1}, 7))
	}
	require.NoError(t, db.Create(offering).Error)
	require.NoError(t, model.CreatePriceVersion(t.Context(), &model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: offering.ID, Currency: "USD", Expression: `tier("fixed", fixed(0.01))`, EffectiveAt: 1}, 7))
	sale, err := model.CurrentPriceVersion(t.Context(), db, model.PriceOwnerOffering, offering.ID, time.Now().Unix())
	require.NoError(t, err)
	for _, cache := range []bool{false, true} {
		t.Run(fmt.Sprintf("cache=%t", cache), func(t *testing.T) {
			common.MemoryCacheEnabled = cache
			model.InitProviderCache()
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			ctx.Set("id", 7)
			common.SetContextKey(ctx, constant.ContextKeyUserGroup, "default")
			ctx.Set("token_auto_groups", []string{"invalid", "default", "vip"})
			ctx.Set("token_cross_group_retry", true)
			input := GatewayRouteInput{Protocol: protocol.OpenAIChat, Sale: sale, Ratio: decimal.NewFromInt(1), Prepare: func(*GatewaySelection) (*GatewayRouteFacts, error) {
				return &GatewayRouteFacts{Usage: &kitdto.Usage{}}, nil
			}}
			first, err := SelectGatewayRoute(ctx, offering, input)
			require.NoError(t, err)
			assert.Equal(t, 2101, first.Provider.Id)
			assert.Equal(t, 1, first.Tier)
			require.NoError(t, first.Permit.Release(t.Context(), false))
			require.NoError(t, ChannelHealth.Release(t.Context(), first.Admission))
			input.Failures = &GatewayExclusions{Channels: map[int]bool{first.Channel.ID: true}, Providers: map[int]bool{}}
			second, err := SelectGatewayRoute(ctx, offering, input)
			require.NoError(t, err)
			assert.Equal(t, 2102, second.Provider.Id)
			assert.Equal(t, 2, second.Tier)
			require.NoError(t, second.Permit.Release(t.Context(), false))
			require.NoError(t, ChannelHealth.Release(t.Context(), second.Admission))
			assert.Empty(t, ctx.GetString("auto_group"))
			assert.Empty(t, setting.GetAutoGroups())
			assert.False(t, db.Migrator().HasTable("abilities"))
		})
	}
}
