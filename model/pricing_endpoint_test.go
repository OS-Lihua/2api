package model

import (
	"fmt"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestProviderCacheDoesNotReadLegacyTaskAliases(t *testing.T) {
	database := UseTestDatabases(t)
	require.NoError(t, database.AutoMigrate(&Provider{}))
	addLegacyProviderMappingColumns(t, database)
	provider := &Provider{Type: constant.ChannelTypeOpenAI, Key: "cache-test-key", Status: common.ChannelStatusEnabled}
	require.NoError(t, database.Create(provider).Error)
	setLegacyProviderMapping(t, database, provider.Id, "unrelated", `{"unrelated":"foreign-model"}`)
	previous := common.MemoryCacheEnabled
	t.Cleanup(func() { common.MemoryCacheEnabled = previous })
	reads := 0
	const callback = "test:forbid_legacy_alias_projection"
	require.NoError(t, database.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "providers" && len(tx.Statement.Selects) > 0 {
			reads++
			tx.AddError(fmt.Errorf("legacy Provider alias projection is unavailable"))
		}
	}))
	t.Cleanup(func() { require.NoError(t, database.Callback().Query().Remove(callback)) })
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("memory=%t", enabled), func(t *testing.T) {
			common.MemoryCacheEnabled = enabled
			InitProviderCache()
			InitProviderCache()
			got, err := CacheGetProvider(provider.Id)
			require.NoError(t, err)
			assert.Equal(t, provider.Key, got.Key)
			assert.Zero(t, reads, "account-cache refresh must not rebuild global task aliases")
		})
	}
}

func resetPricingEndpointTestTables(t *testing.T) {
	t.Helper()
	UseTestDatabases(t)
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	require.NoError(t, DB.AutoMigrate(&Provider{}, &Model{}, &Vendor{}, &Channel{}))
	require.False(t, DB.Migrator().HasTable("abilities"))
	InitProviderCache()
	t.Cleanup(func() {
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
	})
}

func insertPricingEndpointChannel(t *testing.T, channelID int, channelType int, settings dto.ProviderOtherSettings) {
	t.Helper()
	channel := &Provider{
		Id: channelID, Type: channelType, Key: fmt.Sprintf("key-%d", channelID),
		Status: common.ChannelStatusEnabled, Name: fmt.Sprintf("channel-%d", channelID),
	}
	if settings.AdvancedCustom != nil {
		channel.SetOtherSettings(settings)
	}
	require.NoError(t, DB.Create(channel).Error)
}

func pricingEndpointAdvancedCustomConfig(routes ...dto.AdvancedCustomRoute) dto.ProviderOtherSettings {
	return dto.ProviderOtherSettings{AdvancedCustom: &dto.AdvancedCustomConfig{Routes: routes}}
}

func nativeEndpointModel(t *testing.T, providerID int, name string, kind protocol.Type, explicit string) *Model {
	t.Helper()
	metadata := &Model{ModelName: name, Status: 1, NameRule: NameRuleExact, Endpoints: explicit}
	require.NoError(t, DB.Create(metadata).Error)
	if kind != "" {
		channel := Channel{ProviderID: providerID, ModelID: metadata.Id, Protocol: kind,
			UpstreamModelName: name, ParameterOverrides: "{}", Status: ChannelEnabled, MappingConfirmed: true}
		require.NoError(t, DB.Omit("Provider", "Model").Create(&channel).Error)
	}
	return metadata
}

func TestProviderAdvancedCustomUsesConfiguredEndpointTypes(t *testing.T) {
	resetPricingEndpointTestTables(t)
	insertPricingEndpointChannel(t, 101, constant.ChannelTypeAdvancedCustom, pricingEndpointAdvancedCustomConfig(
		dto.AdvancedCustomRoute{IncomingPath: "/v1/chat/completions", UpstreamPath: "/v1/chat/completions"},
		dto.AdvancedCustomRoute{IncomingPath: "/v1/responses", UpstreamPath: "/v1beta/models/{model}:generateContent", Converter: "openai_responses_to_gemini_generate_content", Models: []string{"re:^gemini-"}},
	))
	gemini := nativeEndpointModel(t, 101, "gemini-2.5-flash", protocol.OpenAIChat, "")
	gpt := nativeEndpointModel(t, 101, "gpt-4o", protocol.OpenAIChat, "")
	InitProviderCache()
	config := channel2advancedCustomConfig[101]
	require.NotNil(t, config)
	assert.Equal(t, []constant.EndpointType{constant.EndpointTypeOpenAI, constant.EndpointTypeOpenAIResponse}, config.SupportedEndpointTypesForModel(gemini.ModelName))
	assert.Equal(t, []constant.EndpointType{constant.EndpointTypeOpenAI}, config.SupportedEndpointTypesForModel(gpt.ModelName))
	require.NoError(t, FillModelSupportedEndpoints([]*Model{gemini, gpt}))
	assert.Equal(t, []string{"openai"}, gemini.SupportedEndpoints, "conversion routes must not invent a native Channel declaration")
	assert.Equal(t, []string{"openai"}, gpt.SupportedEndpoints)
}

func TestModelMetadataEndpointsSupplementNativeChannels(t *testing.T) {
	resetPricingEndpointTestTables(t)
	insertPricingEndpointChannel(t, 103, constant.ChannelTypeAdvancedCustom, pricingEndpointAdvancedCustomConfig(
		dto.AdvancedCustomRoute{IncomingPath: "/v1/responses", UpstreamPath: "/v1beta/models/{model}:generateContent", Converter: "openai_responses_to_gemini_generate_content", Models: []string{"re:^gemini-"}},
	))
	metadata := nativeEndpointModel(t, 103, "gemini-2.5-flash", protocol.OpenAIResponses, `{"openai":"/v1/chat/completions"}`)
	require.NoError(t, FillModelSupportedEndpoints([]*Model{metadata}))
	assert.Equal(t, []string{"openai", "openai-response", "openai-response-compact"}, metadata.SupportedEndpoints)
}

func TestModelMetadataEndpointsWithoutMatchingProviderRoute(t *testing.T) {
	resetPricingEndpointTestTables(t)
	insertPricingEndpointChannel(t, 104, constant.ChannelTypeAdvancedCustom, pricingEndpointAdvancedCustomConfig(
		dto.AdvancedCustomRoute{IncomingPath: "/v1/responses", UpstreamPath: "/v1beta/models/{model}:generateContent", Converter: "openai_responses_to_gemini_generate_content", Models: []string{"re:^gemini-"}},
	))
	metadata := nativeEndpointModel(t, 104, "metadata-only-model", "", `{"openai":"/v1/chat/completions"}`)
	InitProviderCache()
	require.NotNil(t, channel2advancedCustomConfig[104])
	assert.Empty(t, channel2advancedCustomConfig[104].SupportedEndpointTypesForModel(metadata.ModelName))
	require.NoError(t, FillModelSupportedEndpoints([]*Model{metadata}))
	assert.Equal(t, []string{"openai"}, metadata.SupportedEndpoints)
	var channels int64
	require.NoError(t, DB.Model(&Channel{}).Count(&channels).Error)
	assert.Zero(t, channels, "metadata does not create a callable Channel")
}

func TestModelNativeEndpointsDoNotRequireAdvancedCustomConfig(t *testing.T) {
	resetPricingEndpointTestTables(t)
	insertPricingEndpointChannel(t, 102, constant.ChannelTypeAdvancedCustom, dto.ProviderOtherSettings{})
	metadata := nativeEndpointModel(t, 102, "gpt-4o", protocol.OpenAIChat, "")
	InitProviderCache()
	assert.Nil(t, channel2advancedCustomConfig[102])
	require.NoError(t, FillModelSupportedEndpoints([]*Model{metadata}))
	assert.Equal(t, []string{"openai"}, metadata.SupportedEndpoints)
}

func TestModelNativeChannelEndpointsDoNotInferConvertedProtocols(t *testing.T) {
	resetPricingEndpointTestTables(t)
	insertPricingEndpointChannel(t, 201, constant.ChannelTypeOpenAI, dto.ProviderOtherSettings{})
	insertPricingEndpointChannel(t, 202, constant.ChannelTypeGemini, dto.ProviderOtherSettings{})
	insertPricingEndpointChannel(t, 203, constant.ChannelTypeAnthropic, dto.ProviderOtherSettings{})
	gpt := nativeEndpointModel(t, 201, "gpt-4o", protocol.OpenAIChat, "")
	gemini := nativeEndpointModel(t, 202, "gemini-2.5-flash", protocol.GeminiGenerate, "")
	claude := nativeEndpointModel(t, 203, "claude-3-5-sonnet", protocol.AnthropicMessages, "")
	require.NoError(t, FillModelSupportedEndpoints([]*Model{gpt, gemini, claude}))
	assert.Equal(t, []string{"openai"}, gpt.SupportedEndpoints)
	assert.Equal(t, []string{"gemini"}, gemini.SupportedEndpoints)
	assert.Equal(t, []string{"anthropic"}, claude.SupportedEndpoints)
}

func TestProviderCacheRefreshesCommittedAdvancedCustomRoutes(t *testing.T) {
	resetPricingEndpointTestTables(t)
	insertPricingEndpointChannel(t, 301, constant.ChannelTypeAdvancedCustom, pricingEndpointAdvancedCustomConfig(
		dto.AdvancedCustomRoute{IncomingPath: "/v1/chat/completions", UpstreamPath: "/v1/chat/completions"},
	))
	metadata := nativeEndpointModel(t, 301, "gemini-3.5-flash", protocol.OpenAIChat, "")
	InitProviderCache()
	require.NotNil(t, channel2advancedCustomConfig[301])
	require.Equal(t, []constant.EndpointType{constant.EndpointTypeOpenAI}, channel2advancedCustomConfig[301].SupportedEndpointTypesForModel(metadata.ModelName))
	var provider Provider
	require.NoError(t, DB.First(&provider, 301).Error)
	provider.SetOtherSettings(pricingEndpointAdvancedCustomConfig(
		dto.AdvancedCustomRoute{IncomingPath: "/v1/chat/completions", UpstreamPath: "/v1/chat/completions"},
		dto.AdvancedCustomRoute{IncomingPath: "/v1/responses", UpstreamPath: "/v1beta/models/{model}:generateContent", Converter: "openai_responses_to_gemini_generate_content", Models: []string{"re:^gemini-"}},
	))
	require.NoError(t, DB.Model(&Provider{}).Where("id = ?", provider.Id).Update("settings", provider.OtherSettings).Error)
	InitProviderCache()
	require.NotNil(t, channel2advancedCustomConfig[301])
	assert.Equal(t, []constant.EndpointType{constant.EndpointTypeOpenAI, constant.EndpointTypeOpenAIResponse}, channel2advancedCustomConfig[301].SupportedEndpointTypesForModel(metadata.ModelName))
	require.NoError(t, FillModelSupportedEndpoints([]*Model{metadata}))
	assert.Equal(t, []string{"openai"}, metadata.SupportedEndpoints, "editing conversion configuration must not rewrite native metadata")
}

func TestNativeEndpointsAreAvailableBeforeProviderCacheStartup(t *testing.T) {
	resetPricingEndpointTestTables(t)
	insertPricingEndpointChannel(t, 302, constant.ChannelTypeAdvancedCustom, pricingEndpointAdvancedCustomConfig(
		dto.AdvancedCustomRoute{IncomingPath: "/v1/chat/completions", UpstreamPath: "/v1/chat/completions"},
		dto.AdvancedCustomRoute{IncomingPath: "/v1/responses", UpstreamPath: "/v1beta/models/{model}:generateContent", Converter: "openai_responses_to_gemini_generate_content", Models: []string{"re:^gemini-"}},
	))
	metadata := nativeEndpointModel(t, 302, "gemini-3.5-flash", protocol.OpenAIResponses, "")
	require.NoError(t, FillModelSupportedEndpoints([]*Model{metadata}))
	require.Equal(t, []string{"openai-response", "openai-response-compact"}, metadata.SupportedEndpoints)
	assert.NotContains(t, channel2advancedCustomConfig, 302)
	InitProviderCache()
	require.NoError(t, FillModelSupportedEndpoints([]*Model{metadata}))
	assert.Equal(t, []string{"openai-response", "openai-response-compact"}, metadata.SupportedEndpoints)
	require.NotNil(t, channel2advancedCustomConfig[302])
	assert.Equal(t, []constant.EndpointType{constant.EndpointTypeOpenAI, constant.EndpointTypeOpenAIResponse}, channel2advancedCustomConfig[302].SupportedEndpointTypesForModel(metadata.ModelName))
}

func TestProviderCachePublishesCommittedStateWithoutRetainingAccountLock(t *testing.T) {
	for _, operation := range []string{"full_sync", "account_update", "status_transition", "stale_transition"} {
		t.Run(operation, func(t *testing.T) {
			resetPricingEndpointTestTables(t)
			insertPricingEndpointChannel(t, 451, constant.ChannelTypeAdvancedCustom, pricingEndpointAdvancedCustomConfig(dto.AdvancedCustomRoute{IncomingPath: "/v1/responses", UpstreamPath: "/v1/responses"}))
			InitProviderCache()
			before, err := GetProviderById(451, true)
			require.NoError(t, err)
			after := *before
			after.Status = common.ChannelStatusManuallyDisabled
			require.NoError(t, DB.Model(&Provider{}).Where("id = ?", after.Id).Update("status", after.Status).Error)
			if operation == "stale_transition" {
				before.Key = "stale-in-memory-only"
			}
			type publication struct {
				provider *Provider
				config   *dto.AdvancedCustomConfig
				err      error
			}
			done := make(chan publication, 1)
			go func() {
				switch operation {
				case "full_sync":
					InitProviderCache()
				case "account_update":
					CacheUpdateProvider(&after)
				default:
					cacheProviderStatusTransition(before, &after)
				}
				cached, err := CacheGetProvider(451)
				channelSyncLock.RLock()
				config := channel2advancedCustomConfig[451]
				channelSyncLock.RUnlock()
				done <- publication{provider: cached, config: config, err: err}
			}()
			// Readers must observe the committed account and routes without a
			// retained publication lock; the retired pricing lock is not a dependency.
			select {
			case got := <-done:
				require.NoError(t, got.err)
				require.NotNil(t, got.provider)
				assert.Equal(t, after.Status, got.provider.Status)
				assert.Equal(t, after.Key, got.provider.Key)
				require.NotNil(t, got.config)
				assert.Equal(t, []constant.EndpointType{constant.EndpointTypeOpenAIResponse}, got.config.SupportedEndpointTypesForModel("gpt-5.4"))
			case <-time.After(3 * time.Second):
				t.Fatal("account publication or its next reader retained the account lock")
			}
		})
	}
}

func TestProviderCacheRefreshFailureEvictsOnlyTarget(t *testing.T) {
	resetPricingEndpointTestTables(t)
	for _, id := range []int{461, 462} {
		insertPricingEndpointChannel(t, id, constant.ChannelTypeAdvancedCustom, pricingEndpointAdvancedCustomConfig(dto.AdvancedCustomRoute{IncomingPath: "/v1/responses", UpstreamPath: "/v1/responses"}))
	}
	InitProviderCache()
	before, err := GetProviderById(461, true)
	require.NoError(t, err)
	after := *before
	after.Status = common.ChannelStatusManuallyDisabled
	before.Key = "stale-in-memory-only"
	require.NoError(t, DB.Delete(&Provider{}, 461).Error)
	cacheProviderStatusTransition(before, &after)
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()
	assert.NotContains(t, channelsIDM, 461)
	assert.NotContains(t, channel2advancedCustomConfig, 461)
	require.Contains(t, channelsIDM, 462)
	assert.Equal(t, common.ChannelStatusEnabled, channelsIDM[462].Status)
	assert.Contains(t, channel2advancedCustomConfig, 462)
}

func TestCacheUpdateChannelSyncsAdvancedCustomConfig(t *testing.T) {
	resetPricingEndpointTestTables(t)
	channel := &Provider{Id: 401, Type: constant.ChannelTypeAdvancedCustom, Key: "key-401", Status: common.ChannelStatusEnabled, Name: "channel-401"}
	channel.SetOtherSettings(pricingEndpointAdvancedCustomConfig(dto.AdvancedCustomRoute{
		IncomingPath: "/v1/responses", UpstreamPath: "/v1beta/models/{model}:generateContent", Converter: "openai_responses_to_gemini_generate_content",
	}))
	CacheUpdateProvider(channel)
	require.NotNil(t, channel2advancedCustomConfig[401])
	assert.Equal(t, []constant.EndpointType{constant.EndpointTypeOpenAIResponse}, channel2advancedCustomConfig[401].SupportedEndpointTypesForModel("gemini-3.5-flash"))
	channel.SetOtherSettings(pricingEndpointAdvancedCustomConfig(dto.AdvancedCustomRoute{IncomingPath: "/v1/chat/completions", UpstreamPath: "/v1/chat/completions"}))
	CacheUpdateProvider(channel)
	require.NotNil(t, channel2advancedCustomConfig[401])
	assert.Equal(t, []constant.EndpointType{constant.EndpointTypeOpenAI}, channel2advancedCustomConfig[401].SupportedEndpointTypesForModel("gemini-3.5-flash"))
	channel.Type = constant.ChannelTypeOpenAI
	CacheUpdateProvider(channel)
	assert.Nil(t, channel2advancedCustomConfig[401])
}
