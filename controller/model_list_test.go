package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"time"

	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type listModelsResponse struct {
	Success bool               `json:"success"`
	Data    []dto.OpenAIModels `json:"data"`
	Object  string             `json:"object"`
}

type userModelsResponse struct {
	Success bool     `json:"success"`
	Data    []string `json:"data"`
}

func setupModelListControllerTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gin.SetMode(gin.TestMode)
	common.RedisEnabled = false
	db := model.UseTestDatabases(t)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Provider{}, &model.Model{}, &model.Vendor{}, &model.Channel{}, &model.ModelOffering{}, &model.ModelOfferingMember{}, &model.TokenOfferingPolicy{}, &model.TokenOfferingException{}))
	require.NoError(t, model.MigratePricing(db))
	require.NoError(t, model.MigrateKeyPermissions(db))
	require.NoError(t, i18n.Init())
	require.False(t, db.Migrator().HasTable("abilities"))
	return db
}

func withTieredBillingConfig(t *testing.T, modes map[string]string, exprs map[string]string) {
	t.Helper()

	saved := map[string]string{}
	require.NoError(t, config.GlobalConfig.SaveToDB(func(key, value string) error {
		if strings.HasPrefix(key, "billing_setting.") {
			saved[key] = value
		}
		return nil
	}))
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(saved))
	})

	modeBytes, err := common.Marshal(modes)
	require.NoError(t, err)
	exprBytes, err := common.Marshal(exprs)
	require.NoError(t, err)

	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		"billing_setting.billing_mode": string(modeBytes),
		"billing_setting.billing_expr": string(exprBytes),
	}))
}

func withSelfUseModeDisabled(t *testing.T) {
	t.Helper()

	original := operation_setting.SelfUseModeEnabled
	operation_setting.SelfUseModeEnabled = false
	t.Cleanup(func() {
		operation_setting.SelfUseModeEnabled = original
	})
}

func withSelfUseModeEnabled(t *testing.T) {
	t.Helper()

	original := operation_setting.SelfUseModeEnabled
	operation_setting.SelfUseModeEnabled = true
	t.Cleanup(func() {
		operation_setting.SelfUseModeEnabled = original
	})
}

func decodeListModelsPayload(t *testing.T, recorder *httptest.ResponseRecorder) listModelsResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var payload listModelsResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	require.Equal(t, "list", payload.Object)
	for _, entry := range payload.Data {
		require.Equal(t, "system", entry.OwnedBy, "public identity belongs to the Offering, not a Provider account")
	}
	return payload
}

func decodeListModelsResponse(t *testing.T, recorder *httptest.ResponseRecorder) map[string]struct{} {
	t.Helper()

	payload := decodeListModelsPayload(t, recorder)
	ids := make(map[string]struct{}, len(payload.Data))
	for _, item := range payload.Data {
		ids[item.Id] = struct{}{}
	}
	return ids
}

func decodeUserModelsResponse(t *testing.T, recorder *httptest.ResponseRecorder) []string {
	t.Helper()

	require.Equal(t, http.StatusOK, recorder.Code)
	var payload userModelsResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	require.True(t, payload.Success)
	return payload.Data
}

func TestGetUserModelsListsVisibleOfferingsWithoutRouteGroups(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.ModelOffering{}))
	require.NoError(t, db.Create(&model.User{Id: 1002, Username: "playground-model-user", Group: "default", Status: common.UserStatusEnabled}).Error)
	require.NoError(t, db.Create(&model.Provider{Id: 1, Name: "legacy-only-names", Status: common.ChannelStatusEnabled, Models: "legacy-model,legacy-vip-model", Group: "default,vip"}).Error)
	require.False(t, db.Migrator().HasTable("abilities"))
	for _, offering := range []*model.ModelOffering{
		{Name: "public-offering", Visibility: "all", Groups: []string{}, Enabled: true},
		{Name: "own-group-offering", Visibility: "groups", Groups: []string{"default"}, Enabled: true},
		{Name: "other-group-offering", Visibility: "groups", Groups: []string{"vip"}, Enabled: true},
		{Name: "disabled-offering", Visibility: "all", Groups: []string{}, Enabled: false},
	} {
		offering.RouteMethod, offering.AcceptedConversionLosses = "cost", []string{}
		require.NoError(t, db.Create(offering).Error)
	}
	for _, group := range []string{"", "default", "vip", "auto"} {
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Request = httptest.NewRequest(http.MethodGet, "/api/user/self/models?group="+group, nil)
		context.Set("id", 1002)
		GetUserModels(context)
		assert.Equal(t, []string{"own-group-offering", "public-offering"}, decodeUserModelsResponse(t, recorder), "a route group cannot reveal hidden Offerings")
	}
}

func TestGetUserModelsRechecksLiveUserAndOfferingVisibility(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.ModelOffering{}))
	user := &model.User{Id: 1003, Username: "playground-live-model-user", Group: "default", Status: common.UserStatusEnabled}
	require.NoError(t, db.Create(user).Error)
	offering := &model.ModelOffering{Name: "group-offering", Visibility: "groups", Groups: []string{"default"}, Enabled: true, RouteMethod: "cost", AcceptedConversionLosses: []string{}}
	require.NoError(t, db.Create(offering).Error)
	for _, state := range []struct {
		group      string
		userStatus int
		enabled    bool
		want       []string
	}{
		{"default", common.UserStatusEnabled, true, []string{"group-offering"}},
		{"vip", common.UserStatusEnabled, true, []string{}},
		{"default", common.UserStatusDisabled, true, []string{}},
		{"default", common.UserStatusEnabled, false, []string{}},
	} {
		require.NoError(t, db.Model(user).Updates(map[string]any{"group": state.group, "status": state.userStatus}).Error)
		require.NoError(t, db.Model(offering).Update("enabled", state.enabled).Error)
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Request = httptest.NewRequest(http.MethodGet, "/api/user/self/models", nil)
		context.Set("id", user.Id)
		context.Set("group", "default")
		GetUserModels(context)
		assert.Equal(t, state.want, decodeUserModelsResponse(t, recorder))
	}
}

func TestListModelsIncludesTieredBillingModel(t *testing.T) {
	withSelfUseModeDisabled(t)
	withTieredBillingConfig(t, map[string]string{"zz-tiered-empty-expr-model": "tiered_expr", "zz-tiered-missing-expr-model": "tiered_expr"}, map[string]string{"zz-tiered-empty-expr-model": " "})
	db, user, token := newOfferingDiscoveryFixture(t)
	valid := createDiscoveryOffering(t, db, "zz-tiered-visible-model", true)
	empty := createDiscoveryOffering(t, db, "zz-tiered-empty-expr-model", false)
	missing := createDiscoveryOffering(t, db, "zz-tiered-missing-expr-model", false)
	unpriced := createDiscoveryOffering(t, db, "zz-unpriced-model", false)
	price := &model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: valid.ID, Currency: "USD", Expression: `tier("base",p*1+c*2)`, EffectiveAt: 1}
	require.NoError(t, model.CreatePriceVersion(t.Context(), price, user.Id))
	require.Error(t, model.CreatePriceVersion(t.Context(), &model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: empty.ID, Currency: "USD", Expression: " ", EffectiveAt: 1}, user.Id))
	ids := decodeListModelsResponse(t, requestOfferingModels(t, token, constant.ChannelTypeOpenAI))
	require.Equal(t, map[string]struct{}{"zz-tiered-visible-model": {}}, ids)
	stored, err := model.CurrentPriceVersion(t.Context(), db, model.PriceOwnerOffering, valid.ID, time.Now().Unix())
	require.NoError(t, err)
	assert.Equal(t, price.ID, stored.ID)
	assert.Equal(t, `tier("base",p*1+c*2)`, stored.Expression)
	for _, offering := range []*model.ModelOffering{empty, missing, unpriced} {
		_, err = model.CurrentPriceVersion(t.Context(), db, model.PriceOwnerOffering, offering.ID, time.Now().Unix())
		require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	}
	assert.False(t, db.Migrator().HasTable("abilities"))
}

func TestListModelsUsesAdvancedCustomEndpointTypesFromPricingCache(t *testing.T) {
	withSelfUseModeEnabled(t)
	db, _, token := newOfferingDiscoveryFixture(t)
	createDiscoveryOffering(t, db, "gemini-3.5-flash", true)
	provider := &model.Provider{Type: constant.ChannelTypeAdvancedCustom, Name: "advanced-custom", Key: "advanced-custom-secret", Status: common.ChannelStatusEnabled, Models: "retired-model", Group: "retired-group"}
	provider.SetOtherSettings(dto.ProviderOtherSettings{AdvancedCustom: &dto.AdvancedCustomConfig{Routes: []dto.AdvancedCustomRoute{{IncomingPath: "/v1/chat/completions", UpstreamPath: "/v1/chat/completions"}, {IncomingPath: "/v1/responses", UpstreamPath: "/v1beta/models/{model}:generateContent", Converter: "openai_responses_to_gemini_generate_content", Models: []string{"re:^gemini-"}}}}})
	require.NoError(t, db.Create(provider).Error)
	metadata := &model.Model{ModelName: "private-gemini-model", Status: 1}
	require.NoError(t, db.Create(metadata).Error)
	for _, kind := range []protocol.Type{protocol.OpenAIChat, protocol.OpenAIResponses} {
		require.NoError(t, db.Create(&model.Channel{ProviderID: provider.Id, ModelID: metadata.Id, Protocol: kind, UpstreamModelName: "gemini-3.5-flash", ParameterOverrides: `{}`, Status: model.ChannelEnabled, MappingConfirmed: true}).Error)
	}
	for _, cache := range []bool{false, true} {
		previous := common.MemoryCacheEnabled
		common.MemoryCacheEnabled = cache
		model.InitProviderCache()
		payload := decodeListModelsPayload(t, requestOfferingModels(t, token, constant.ChannelTypeOpenAI))
		require.Len(t, payload.Data, 1)
		assert.Equal(t, "gemini-3.5-flash", payload.Data[0].Id)
		assert.Empty(t, payload.Data[0].SupportedEndpointTypes, "public discovery does not expose Provider composition or imply callability")
		require.NoError(t, model.FillModelSupportedEndpoints([]*model.Model{metadata}))
		assert.ElementsMatch(t, []string{string(constant.EndpointTypeOpenAI), string(constant.EndpointTypeOpenAIResponse), string(constant.EndpointTypeOpenAIResponseCompact)}, metadata.SupportedEndpoints)
		common.MemoryCacheEnabled = previous
	}
	assert.False(t, db.Migrator().HasTable("abilities"))
}

func TestListModelsTokenLimitIncludesTieredBillingModel(t *testing.T) {
	withSelfUseModeDisabled(t)
	db, user, token := newOfferingDiscoveryFixture(t)
	allowed := createDiscoveryOffering(t, db, "zz-token-tiered-visible-model", true)
	for _, name := range []string{"zz-token-tiered-empty-expr-model", "zz-token-tiered-missing-expr-model", "zz-token-unpriced-model"} {
		createDiscoveryOffering(t, db, name, true)
	}
	require.NoError(t, model.CreatePriceVersion(t.Context(), &model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: allowed.ID, Currency: "USD", Expression: `tier("base",p*1+c*2)`, EffectiveAt: 1}, user.Id))
	require.NoError(t, db.Create(&model.TokenOfferingPolicy{TokenID: token.Id, DefaultAllow: false}).Error)
	require.NoError(t, db.Create(&model.TokenOfferingException{TokenID: token.Id, OfferingID: allowed.ID}).Error)
	ids := decodeListModelsResponse(t, requestOfferingModels(t, token, constant.ChannelTypeOpenAI, func(c *gin.Context) {
		c.Set("token_model_limit_enabled", true)
		c.Set("token_model_limit", map[string]bool{"zz-token-unpriced-model": true})
	}))
	assert.Equal(t, map[string]struct{}{"zz-token-tiered-visible-model": {}}, ids, "only current Offering permission grants discovery; legacy ModelLimits neither grant nor deny")
	require.NoError(t, db.Where("token_id = ?", token.Id).Delete(&model.TokenOfferingException{}).Error)
	assert.Empty(t, decodeListModelsResponse(t, requestOfferingModels(t, token, constant.ChannelTypeOpenAI)), "revocation applies without cache invalidation")
}

func TestListModelsTokenLimitUsesResolvedCustomAutoGroups(t *testing.T) {
	withSelfUseModeEnabled(t)
	db, user, token := newOfferingDiscoveryFixture(t)
	require.NoError(t, db.Model(user).Update("group", "vip").Error)
	allowed := createDiscoveryOffering(t, db, "zz-vip-allowed", true, "vip")
	createDiscoveryOffering(t, db, "zz-vip-denied", true, "vip")
	createDiscoveryOffering(t, db, "zz-default-outside-snapshot", true, "default")
	createDiscoveryOffering(t, db, "zz-not-enabled", false)
	require.NoError(t, db.Create(&model.TokenOfferingPolicy{TokenID: token.Id, DefaultAllow: false}).Error)
	require.NoError(t, db.Create(&model.TokenOfferingException{TokenID: token.Id, OfferingID: allowed.ID}).Error)
	retired := func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyUserGroup, "vip")
		c.Set("token_group", "auto")
		c.Set("token_auto_groups", []string{"vip"})
		c.Set("token_model_limit_enabled", true)
		c.Set("token_model_limit", map[string]bool{"zz-vip-allowed": true, "zz-default-outside-snapshot": true})
	}
	assert.Equal(t, map[string]struct{}{"zz-vip-allowed": {}}, decodeListModelsResponse(t, requestOfferingModels(t, token, constant.ChannelTypeOpenAI, retired)))
	require.NoError(t, db.Model(user).Update("group", "default").Error)
	response := requestOfferingModels(t, token, constant.ChannelTypeAnthropic, retired)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var payload struct {
		Data    []dto.AnthropicModel `json:"data"`
		FirstID string               `json:"first_id"`
		LastID  string               `json:"last_id"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
	assert.Empty(t, payload.Data)
	assert.Empty(t, payload.FirstID)
	assert.Empty(t, payload.LastID)
}

func TestSetupLoginDoesNotTouchPasswordWhenPasswordFieldOmitted(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AuditLog{}, &model.UserSession{}, &model.TwoFA{}, &model.PasskeyCredential{}))

	hashedPassword, err := common.Password2Hash("CurrentPassword123")
	require.NoError(t, err)
	user := &model.User{
		Username: "twofa-user",
		Password: hashedPassword,
		Role:     common.RoleCommonUser,
		Status:   common.UserStatusEnabled,
		Group:    "default",
	}
	require.NoError(t, db.Create(user).Error)

	router := gin.New()
	router.GET("/", func(c *gin.Context) {
		setupLogin(&model.User{
			Id:          user.Id,
			AuthVersion: user.AuthVersion,
			Username:    user.Username,
			Role:        user.Role,
			Status:      user.Status,
			Group:       user.Group,
		}, nil, c)
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	var stored model.User
	require.NoError(t, db.First(&stored, user.Id).Error)
	assert.Equal(t, hashedPassword, stored.Password)
}

func newOfferingDiscoveryFixture(t *testing.T) (*gorm.DB, *model.User, *model.Token) {
	t.Helper()
	db := setupModelListControllerTestDB(t)
	user := &model.User{Id: 1001, Username: "offering-discovery", Group: "default", Role: common.RoleCommonUser, Status: common.UserStatusEnabled}
	require.NoError(t, db.Create(user).Error)
	token := &model.Token{UserId: user.Id, Key: "offering-discovery-test-key", Name: "discovery", Status: common.TokenStatusEnabled, ExpiredTime: -1}
	require.NoError(t, db.Create(token).Error)
	return db, user, token
}

func createDiscoveryOffering(t *testing.T, db *gorm.DB, name string, enabled bool, groups ...string) *model.ModelOffering {
	t.Helper()
	visibility := "all"
	if len(groups) > 0 {
		visibility = "groups"
	}
	offering := &model.ModelOffering{Name: name, Visibility: visibility, Groups: append([]string{}, groups...), Enabled: enabled, RouteMethod: "cost", AcceptedConversionLosses: []string{}}
	require.NoError(t, db.Create(offering).Error)
	return offering
}

func requestOfferingModels(t *testing.T, token *model.Token, kind int, decorate ...func(*gin.Context)) *httptest.ResponseRecorder {
	t.Helper()
	engine := gin.New()
	engine.Use(middleware.GatewayTokenAuth())
	engine.GET("/v1/models", func(c *gin.Context) {
		for _, fn := range decorate {
			fn(c)
		}
		ListOfferingModels(c, kind)
	})
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+token.Key)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	assert.NotContains(t, response.Body.String(), token.Key)
	return response
}
