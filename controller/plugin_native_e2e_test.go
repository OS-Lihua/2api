package controller

import (
	"context"
	"github.com/QuantumNous/new-api/pkg/protocol"

	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/capability"
	"github.com/QuantumNous/new-api/pkg/channelhealth"
	"github.com/QuantumNous/new-api/pkg/clock"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/relay"

	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// This fixture enters the production Offering task gateway. No legacy quota
// billing session or fabricated TaskGatewaySnapshot participates in its money.
type nativeGatewayTaskFixture struct {
	db             *gorm.DB
	user           *model.User
	token          *model.Token
	provider       *model.Provider
	channel        *model.Channel
	offering       *model.ModelOffering
	sale, purchase *model.PriceVersion
	generation     *pluginruntime.RoutingGeneration
}

func newNativeGatewayTaskFixture(t *testing.T, upstream http.HandlerFunc, pluginSource ...string) *nativeGatewayTaskFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	t.Setenv("GATEWAY_SETTLEMENT_JOURNAL_DIR", t.TempDir())
	previousRegistry := pluginruntime.DefaultRegistry
	registry, err := plugins.NewFactoryRegistry()
	require.NoError(t, err)
	for _, source := range pluginSource {
		_, err = registry.Register(source, pluginruntime.Options{})
		require.NoError(t, err)
	}
	pluginruntime.DefaultRegistry = registry
	t.Cleanup(func() { pluginruntime.DefaultRegistry = previousRegistry })
	previousMemory, previousRedis := common.MemoryCacheEnabled, common.RedisEnabled
	common.MemoryCacheEnabled, common.RedisEnabled = false, false
	t.Cleanup(func() {
		common.MemoryCacheEnabled, common.RedisEnabled = previousMemory, previousRedis
	})
	previousHealth, previousQuota := service.ChannelHealth, service.GatewayRouteQuota
	service.ChannelHealth, service.GatewayRouteQuota = channelhealth.New(nil), service.NewGatewayRouteQuota(nil)
	t.Cleanup(func() { service.ChannelHealth, service.GatewayRouteQuota = previousHealth, previousQuota })
	previousFactory := service.GetTaskAdaptorFunc
	service.GetTaskAdaptorFunc = func(platform constant.TaskPlatform) service.TaskPollingAdaptor { return relay.GetTaskAdaptor(platform) }
	t.Cleanup(func() { service.GetTaskAdaptorFunc = previousFactory })
	previousRatios := ratio_setting.GroupRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1}`))
	common.OptionMapRWMutex.Lock()
	previousOptions := common.OptionMap
	common.OptionMap = map[string]string{"GroupRatio": `{"default":1}`, "GroupGroupRatio": `{}`}
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(previousRatios))
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptions
		common.OptionMapRWMutex.Unlock()
	})
	db := model.UseTestDatabases(t)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Option{}, &model.Provider{}, &model.Vendor{}, &model.Model{}, &model.Channel{}, &model.ModelOffering{}, &model.ModelOfferingName{}, &model.ModelOfferingMember{}, &model.TokenOfferingPolicy{}, &model.TokenOfferingException{}, &model.Task{}, &model.LegacyTaskObligation{}, &model.LegacySubscriptionTaskObligation{}))
	require.NoError(t, model.InitSettlementCurrency())
	require.NoError(t, model.MigratePricing(db))
	require.NoError(t, model.MigrateGatewayBilling(db))
	require.NoError(t, model.MigrateBalanceReminders(db))
	require.NoError(t, model.MigrateLogDB())
	user := &model.User{Username: "native-gateway-user", Status: common.UserStatusEnabled, Group: "default", Role: common.RoleCommonUser, AuthVersion: 1, Quota: 1_000_000, Setting: `{"billing_preference":"wallet_only"}`}
	require.NoError(t, db.Create(user).Error)
	token := &model.Token{UserId: user.Id, Key: "nativegatewaytestkey", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 7654321}
	require.NoError(t, db.Create(token).Error)
	_, err = model.TopUpWallet(model.WalletTopUp{UserId: user.Id, Source: money.Amount{Nanos: 2_000_000_000, Currency: "USD"}, Reference: "native-task-initial"})
	require.NoError(t, err)
	vendor := &model.Vendor{Name: "Native task publisher", Status: 1}
	require.NoError(t, db.Create(vendor).Error)
	unified := &model.Model{ModelName: "kling-v3", VendorID: vendor.Id, Status: 1, NameRule: model.NameRuleExact, Capabilities: capability.Set{Features: []capability.Feature{capability.Streaming}, InputModalities: []capability.Modality{capability.Text, capability.Image}, OutputModalities: []capability.Modality{capability.Video}}}
	require.NoError(t, db.Create(unified).Error)
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	provider := &model.Provider{Name: "Kling gateway Provider", Type: constant.ChannelTypeKling, Key: "sk-test", Status: common.ChannelStatusEnabled, BaseURL: &server.URL}
	require.NoError(t, db.Create(provider).Error)
	channel := &model.Channel{ProviderID: provider.Id, ModelID: unified.Id, Protocol: "task:kling", UpstreamModelName: "kling-v3", ParameterOverrides: `{}`, MappingConfirmed: true, Status: model.ChannelEnabled, Weight: 1}
	require.NoError(t, db.Create(channel).Error)
	offering := &model.ModelOffering{Name: "native-task-offering", Aliases: []string{"kling-v3"}, Members: []model.ModelOfferingMember{{ModelID: unified.Id, Tier: 1}}}
	require.NoError(t, model.SaveModelOffering(t.Context(), offering, "save"))
	sale := &model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: offering.ID, Currency: "USD", Expression: `tier("units", u("units") * 0.125)`, EffectiveAt: clock.Now().Unix() - 1}
	purchase := &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: channel.ID, Currency: "USD", Expression: `tier("units", u("units") * 0.05)`, EffectiveAt: clock.Now().Unix() - 1}
	require.NoError(t, model.CreatePriceVersion(t.Context(), sale, user.Id))
	require.NoError(t, model.CreatePriceVersion(t.Context(), purchase, user.Id))
	require.NoError(t, model.SaveModelOffering(t.Context(), offering, "enable"))
	service.InitHttpClient()
	return &nativeGatewayTaskFixture{db: db, user: user, token: token, provider: provider, channel: channel, offering: offering, sale: sale, purchase: purchase, generation: registry.Generation()}
}

func (fixture *nativeGatewayTaskFixture) request(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	binding, found := fixture.generation.LookupDeclaredRoute(http.MethodPost, "/kling/v1/videos/text2video")
	require.True(t, found)
	require.Equal(t, "kling", binding.Plugin.Meta.Key)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/kling/v1/videos/text2video", strings.NewReader(`{"model_name":"kling-v3","prompt":"a lighthouse","duration":"5","mode":"pro"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Authorization", "Bearer sk-"+fixture.token.Key)
	middleware.RequestId()(c)
	middleware.TokenAuth()(c)
	require.False(t, c.IsAborted(), recorder.Body.String())
	c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{Generation: fixture.generation, Plugin: binding.Plugin, Route: binding.Route})
	middleware.ResolveTaskOffering()(c)
	middleware.ResolveTaskPluginOffering()(c)
	AuthorizeTaskGateway(c)
	middleware.PrepareTaskPluginRoute()(c)
	require.False(t, c.IsAborted(), recorder.Body.String())
	require.Equal(t, fixture.offering.ID, middleware.TextOffering(c).ID)
	require.Equal(t, "kling-v3", c.GetString("resolved_task_model"))
	require.Equal(t, "text_to_video", c.GetString("task_action"))
	return c, recorder
}

func (fixture *nativeGatewayTaskFixture) submit(t *testing.T) (*taskSubmissionOutcome, *gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	c, recorder := fixture.request(t)
	outcome, taskErr := executeTaskSubmission(c, nil)
	require.Nil(t, taskErr)
	require.NotNil(t, outcome)
	require.False(t, c.Writer.Written())
	return outcome, c, recorder
}

func assertNativeTaskSettlement(t *testing.T, fixture *nativeGatewayTaskFixture, task *model.Task) {
	t.Helper()
	snapshot := task.PrivateData.Gateway
	require.NotNil(t, snapshot)
	var billing model.GatewayBilling
	require.NoError(t, fixture.db.First(&billing, "request_id = ?", snapshot.Settlement.Request.ID).Error)
	require.Equal(t, "settled", billing.Phase)
	assert.True(t, billing.Published)
	// Use the canonical read APIs: ClickHouse retains append-only versions and
	// these queries apply FINAL rather than counting the pending and final rows.
	request, err := model.GetGatewayRequest(t.Context(), billing.RequestID, fixture.user.Id)
	require.NoError(t, err)
	assert.Equal(t, fixture.sale.ID, request.SalePriceVersionID)
	assert.Equal(t, fixture.sale.Expression, request.SaleExpression)
	assert.Equal(t, "upstream", request.UsageSource)
	assert.EqualValues(t, 1, request.Usage.TaskFacts["units"])
	assert.Equal(t, money.Amount{Nanos: 125_000_000, Currency: "USD"}, request.Receivable)
	attempts, total, err := model.ListGatewayAttempts(t.Context(), billing.RequestID, 0, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	require.Len(t, attempts, 1)
	assert.Equal(t, fixture.purchase.ID, attempts[0].PurchasePriceVersionID)
	assert.Equal(t, fixture.purchase.Expression, attempts[0].PurchaseExpression)
	assert.Equal(t, "upstream", attempts[0].UsageSource)
	assert.Equal(t, money.Amount{Nanos: 50_000_000, Currency: "USD"}, attempts[0].Cost)
	var entries []model.LedgerEntry
	require.NoError(t, fixture.db.Where("request_id = ?", billing.RequestID).Order("id").Find(&entries).Error)
	require.Len(t, entries, 5, "one reservation/release, one charge, one revenue and one payable")
	amounts := make(map[string]int64)
	for _, entry := range entries {
		assert.Equal(t, money.Currency("USD"), entry.Amount.Currency)
		key := string(entry.AccountType) + ":" + string(entry.Type)
		_, duplicate := amounts[key]
		assert.False(t, duplicate, "no second debit or compensating refund")
		amounts[key] = entry.Amount.Nanos
	}
	assert.Equal(t, map[string]int64{
		string(model.LedgerAccountUserWallet) + ":" + string(model.LedgerEntryPreConsume):        -500_000_000,
		string(model.LedgerAccountUserWallet) + ":" + string(model.LedgerEntryPreConsumeRelease): 500_000_000,
		string(model.LedgerAccountUserWallet) + ":" + string(model.LedgerEntryCharge):            -125_000_000,
		string(model.LedgerAccountPlatformRevenue) + ":" + string(model.LedgerEntryCharge):       125_000_000,
		string(model.LedgerAccountProviderPayable) + ":" + string(model.LedgerEntryCharge):       50_000_000,
	}, amounts)
	wallet, err := model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	assert.Equal(t, money.Amount{Nanos: 1_875_000_000, Currency: "USD"}, wallet.Balance)
	var user model.User
	var token model.Token
	require.NoError(t, fixture.db.First(&user, fixture.user.Id).Error)
	require.NoError(t, fixture.db.First(&token, fixture.token.Id).Error)
	assert.Equal(t, 1_000_000, user.Quota, "archived quota is not a second money lifecycle")
	assert.Equal(t, 7654321, token.RemainQuota)
	before, err := common.Marshal(entries)
	require.NoError(t, err)
	require.NoError(t, service.SettleGatewayTask(t.Context(), task))
	require.NoError(t, fixture.db.Where("request_id = ?", billing.RequestID).Order("id").Find(&entries).Error)
	after, err := common.Marshal(entries)
	require.NoError(t, err)
	assert.JSONEq(t, string(before), string(after), "terminal settlement replay is idempotent")
	reconciliation, err := model.ReconcileWallets(100)
	require.NoError(t, err)
	assert.Empty(t, reconciliation.Mismatches)
}

func TestKlingNativeRouteSubmitPollSettleAndQuery(t *testing.T) {
	var submitCalls, queryCalls atomic.Int32
	var fixture *nativeGatewayTaskFixture
	fixture = newNativeGatewayTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/kling/v1/videos/text2video":
			submitCalls.Add(1)
			body, err := io.ReadAll(r.Body)
			if !assert.NoError(t, err) {
				http.Error(w, "read request", 500)
				return
			}
			assert.Contains(t, string(body), `"model_name":"kling-v3"`)
			var entries []model.LedgerEntry
			assert.NoError(t, fixture.db.Where("type = ?", model.LedgerEntryPreConsume).Find(&entries).Error)
			if assert.Len(t, entries, 1, "reservation must commit before Provider acceptance") {
				assert.Equal(t, int64(-500_000_000), entries[0].Amount.Nanos)
				var billing model.GatewayBilling
				if assert.NoError(t, fixture.db.First(&billing, "request_id = ?", entries[0].RequestId).Error) {
					assert.Equal(t, "reserved", billing.Phase)
				}
			}
			_, _ = io.WriteString(w, `{"code":0,"message":"","data":{"task_id":"kling-private-1","task_status":"submitted"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/kling/v1/videos/text2video/kling-private-1":
			queryCalls.Add(1)
			_, _ = io.WriteString(w, `{"code":0,"message":"","data":{"task_id":"kling-private-1","task_status":"succeed","task_status_msg":"","task_result":{"videos":[{"id":"video-private","url":"https://cdn.example/video.mp4","duration":"5"}]},"final_unit_deduction":"1"}}`)
		default:
			http.NotFound(w, r)
		}
	})
	outcome, submitContext, submitRecorder := fixture.submit(t)
	presentTaskSubmission(submitContext, outcome)
	require.Equal(t, http.StatusOK, submitRecorder.Code)
	publicID := outcome.Task.TaskID
	require.NotEmpty(t, publicID)
	assert.Contains(t, submitRecorder.Body.String(), `"task_id":"`+publicID+`"`)
	assert.NotContains(t, submitRecorder.Body.String(), "kling-private-1")
	assert.Equal(t, int32(1), submitCalls.Load())
	var persisted model.Task
	require.NoError(t, fixture.db.Where("task_id = ?", publicID).First(&persisted).Error)
	assert.Equal(t, constant.TaskPlatform("kling"), persisted.Platform)
	assert.Equal(t, "kling-private-1", persisted.PrivateData.UpstreamTaskID)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSubmitted), persisted.Status)
	require.NotNil(t, persisted.PrivateData.Gateway)
	assert.True(t, persisted.PrivateData.Gateway.Accepted)
	// Repricing after acceptance cannot replace either frozen expression.
	for _, price := range []*model.PriceVersion{
		{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: `tier("changed", u("units") * 0.8)`, EffectiveAt: clock.Now().Unix()},
		{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: `tier("changed", u("units") * 0.4)`, EffectiveAt: clock.Now().Unix()},
	} {
		require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.user.Id))
	}
	service.DispatchPlatformUpdate(context.Background(), persisted.Platform, map[int][]string{fixture.provider.Id: {"kling-private-1"}}, map[string]*model.Task{"kling-private-1": &persisted})
	require.NoError(t, fixture.db.Where("task_id = ?", publicID).First(&persisted).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), persisted.Status)
	assert.Equal(t, "100%", persisted.Progress)
	assert.Equal(t, int32(1), queryCalls.Load())
	assertNativeTaskSettlement(t, fixture, &persisted)
	queryBinding, found := fixture.generation.LookupDeclaredRoute(http.MethodGet, "/kling/v1/videos/text2video/:task_id")
	require.True(t, found)
	queryRecorder := httptest.NewRecorder()
	queryContext, _ := gin.CreateTestContext(queryRecorder)
	queryContext.Request = httptest.NewRequest(http.MethodGet, "/kling/v1/videos/text2video/"+publicID, nil)
	queryContext.Params = gin.Params{{Key: "s4", Value: publicID}}
	queryContext.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{Generation: fixture.generation, Plugin: queryBinding.Plugin, Route: queryBinding.Route})
	common.SetContextKey(queryContext, constant.ContextKeyUserId, fixture.user.Id)
	middleware.PrepareTaskPluginRoute()(queryContext)
	require.True(t, queryContext.IsAborted())
	require.Equal(t, http.StatusOK, queryRecorder.Code)
	assert.Contains(t, queryRecorder.Body.String(), `"task_id":"`+publicID+`"`)
	assert.Contains(t, queryRecorder.Body.String(), `"task_status":"succeed"`)
	assert.NotContains(t, queryRecorder.Body.String(), "kling-private-1")
	assert.NotContains(t, queryRecorder.Body.String(), *fixture.provider.BaseURL)
}

// Once Ark's first event has reached a native client the submission settles
// and is never refunded. A stream Ark finished bills the images it delivered;
// one that broke off, whose first event the plugin could not read, or that
// kept sending keep-alive comments until the submission deadline bills the
// reserved images. A finished stream stays billed and logged when the host
// then fails to reserve its images or to store the task.
func TestDoubaoRelayedImageStreamSettlement(t *testing.T) {
	const seedream = "doubao-seedream-5-0-lite-260128"
	const image = `data: {"type":"image_generation.partial_succeeded","model":"` + seedream + `","created":1,"image_index":0,"url":"https://cdn.example/0.png","size":"2048x2048"}` + "\n\n"
	const completed = `data: {"type":"image_generation.completed","model":"` + seedream + `","created":1,"usage":{"generated_images":1,"output_tokens":16384,"total_tokens":16384}}` + "\n\n"
	previousDeadline := constant.TaskPluginProtocolTimeoutSeconds
	constant.TaskPluginProtocolTimeoutSeconds = 1
	t.Cleanup(func() { constant.TaskPluginProtocolTimeoutSeconds = previousDeadline })
	for _, name := range []string{"finished", "broken", "unreadable", "stalled", "reserve_fails", "insert_fails"} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			fixture := newNativeGatewayTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				write := func(frame string) { _, _ = io.WriteString(w, frame); w.(http.Flusher).Flush() }
				switch name {
				case "finished", "reserve_fails", "insert_fails":
					write(image + completed + "data: [DONE]\n\n")
				case "broken":
					write(image)
					panic(http.ErrAbortHandler)
				case "unreadable":
					write("data: {\"type\":\n\n")
					write(image)
				case "stalled":
					write(image)
					for range 50 {
						select {
						case <-r.Context().Done():
							return
						case <-time.After(100 * time.Millisecond):
							write(": keep-alive\n\n")
						}
					}
				}
			})
			caps := capability.Set{Features: []capability.Feature{capability.Streaming}, InputModalities: []capability.Modality{capability.Text, capability.Image}, OutputModalities: []capability.Modality{capability.Image}}
			require.NoError(t, fixture.db.Model(&model.Provider{}).Where("id = ?", fixture.provider.Id).Update("type", constant.ChannelTypeDoubaoVideo).Error)
			require.NoError(t, fixture.db.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Updates(map[string]any{"model_name": seedream, "capabilities": caps}).Error)
			require.NoError(t, fixture.db.Model(&model.Channel{}).Where("id = ?", fixture.channel.ID).Updates(map[string]any{"protocol": "task:doubao", "upstream_model_name": seedream}).Error)
			fixture.sale = &model.PriceVersion{OwnerType: fixture.sale.OwnerType, OwnerID: fixture.sale.OwnerID, Currency: "USD", Expression: `tier("images", u("images_above_1_5k") * 0.02)`, EffectiveAt: time.Now().Unix()}
			require.NoError(t, model.CreatePriceVersion(t.Context(), fixture.sale, fixture.user.Id))
			fixture.purchase = &model.PriceVersion{OwnerType: fixture.purchase.OwnerType, OwnerID: fixture.purchase.OwnerID, Currency: "USD", Expression: `tier("images", u("images_above_1_5k") * 0.005)`, EffectiveAt: time.Now().Unix()}
			require.NoError(t, model.CreatePriceVersion(t.Context(), fixture.purchase, fixture.user.Id))
			fixture.offering.Aliases = []string{seedream}
			require.NoError(t, model.SaveModelOffering(t.Context(), fixture.offering, "save"))
			if name == "reserve_fails" {
				_, err := model.PostLedgerEntries(model.LedgerPosting{Account: model.UserWalletAccount(fixture.user.Id), Type: model.LedgerEntryAdjustment, Amount: money.Amount{Nanos: -1990000000, Currency: "USD"}, IdempotencyKey: "insufficient-stream"})
				require.NoError(t, err)
			}
			if name == "insert_fails" {
				require.NoError(t, fixture.db.Exec(`CREATE FUNCTION reject_stream_task() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'task insertion rejected'; END $$`).Error)
				require.NoError(t, fixture.db.Exec(`CREATE TRIGGER reject_stream_task BEFORE INSERT ON tasks FOR EACH ROW EXECUTE FUNCTION reject_stream_task()`).Error)
			}
			binding, found := fixture.generation.LookupDeclaredRoute(http.MethodPost, "/api/v3/images/generations")
			require.True(t, found)
			require.Equal(t, "doubao", binding.Plugin.Meta.Key)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v3/images/generations", strings.NewReader(`{"model":"`+seedream+`","prompt":"`+name+`","stream":true,"sequential_image_generation":"auto","sequential_image_generation_options":{"max_images":3}}`))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
			middleware.RequestId()(c)
			middleware.GatewayTokenAuth()(c)
			c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{Generation: fixture.generation, Plugin: binding.Plugin, Route: binding.Route})
			middleware.ResolveTaskOffering()(c)
			middleware.ResolveTaskPluginOffering()(c)
			AuthorizeTaskGateway(c)
			middleware.PrepareTaskPluginRoute()(c)
			require.False(t, c.IsAborted(), recorder.Body.String())
			started := time.Now()
			outcome, taskErr := executeTaskSubmission(c, nil)
			assert.Less(t, time.Since(started), 10*time.Second, "the original stream deadline remains bounded")
			if name == "reserve_fails" || name == "insert_fails" {
				require.NotNil(t, taskErr)
				assert.Nil(t, outcome)
				assert.Zero(t, calls.Load())
				assert.Empty(t, recorder.Body.String())
				var count int64
				require.NoError(t, fixture.db.Model(&model.GatewayBilling{}).Count(&count).Error)
				assert.Zero(t, count)
				balance := int64(2000000000)
				if name == "reserve_fails" {
					balance = 10000000
					assert.Equal(t, http.StatusPaymentRequired, taskErr.StatusCode)
				}
				assertSubmissionWallet(t, fixture, balance)
				return
			}
			assert.EqualValues(t, 1, calls.Load())
			if name == "unreadable" {
				require.NotNil(t, taskErr)
				assert.Empty(t, recorder.Body.String(), "invalid first frame must not become client business output")
				assertSubmissionWallet(t, fixture, 2000000000)
				return
			}
			require.Nil(t, taskErr)
			require.NotNil(t, outcome)
			assert.Zero(t, outcome.Task.Quota)
			assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), outcome.Task.Status)
			assert.True(t, strings.HasPrefix(recorder.Body.String(), image), recorder.Body.String())
			relayed := recorder.Body.String()
			presentTaskSubmission(c, outcome)
			assert.Equal(t, relayed, recorder.Body.String(), "no JSON follows the native event stream")
			assertSubmissionWallet(t, fixture, 1980000000)
			var billing model.GatewayBilling
			require.NoError(t, fixture.db.First(&billing, "request_id = ?", outcome.Task.PrivateData.Gateway.Settlement.Request.ID).Error)
			var intent model.GatewaySettlement
			require.NoError(t, common.Unmarshal([]byte(billing.Settlement), &intent))
			assert.EqualValues(t, 20000000, intent.Request.Receivable.Nanos, "one delivered image, never the three-image budget")
			assert.EqualValues(t, 5000000, intent.Attempt.Cost.Nanos, "actual source image remains procurement")
			if name == "finished" {
				assert.Equal(t, "settled", billing.Phase)
			} else {
				assert.Equal(t, "evaluation_failed", billing.Phase)
				assert.True(t, intent.Attempt.CostPending)
			}
			var before, after []model.LedgerEntry
			require.NoError(t, fixture.db.Where("request_id = ?", billing.RequestID).Order("id").Find(&before).Error)
			_, _ = model.SettleGateway(billing.RequestID)
			require.NoError(t, fixture.db.Where("request_id = ?", billing.RequestID).Order("id").Find(&after).Error)
			assert.Equal(t, before, after)
		})
	}
}

// The native tools contract is exercised through its registered money gateway,
// not the retired quota orchestration helper.
func TestDeepSeekToolsHelper(t *testing.T) {
	const search, fetch = "/v1/tools/search", "/v1/tools/fetch"
	const results = `{"results":[{"title":"DeepSeek","url":"https://deepseek.com/","rerank_score":2.1}]}`
	for _, tc := range []struct {
		name, path, body, response string
		provider, status           int
		sale                       int64
	}{
		{"search with results", search, `{"query":"deepseek","count":2,"future_field":{"nested":true}}`, results, constant.ChannelTypeDeepSeek, 200, 10000000},
		{"search without results", search, `{"query":"site:none.invalid deepseek"}`, `{"results":[]}`, constant.ChannelTypeDeepSeek, 200, 0},
		{"fetch with page content", fetch, `{"url":"https://example.com/"}`, `{"title":"Example Domain","content":"# Example Domain"}`, constant.ChannelTypeDeepSeek, 200, 10000000},
		{"fetch failure keeps its status", fetch, `{"url":"https://example.com/missing"}`, `{"error":{"message":"failed to fetch URL","type":"not_found_error","param":null,"code":"invalid_request_error"}}`, constant.ChannelTypeDeepSeek, 404, 0},
		{"search through a New API channel", search, `{"query":"deepseek"}`, results, constant.ChannelTypeNewAPI, 200, 10000000},
		{"channel without the endpoint", search, `{"query":"deepseek"}`, "", constant.ChannelTypeOpenAI, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var path string
			var body []byte
			var calls atomic.Int32
			name := constant.DeepSeekSearchModel
			if tc.path == fetch {
				name = constant.DeepSeekFetchModel
			}
			fixture, engine := newCanonicalRelayFixture(t, protocol.DeepSeekTools, name, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				path = r.URL.Path
				body, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.response)
			})
			require.NoError(t, fixture.db.Model(&model.Provider{}).Where("id = ?", fixture.provider.Id).Update("type", tc.provider).Error)
			r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, r)
			if tc.status == 0 {
				assert.Equal(t, http.StatusBadRequest, response.Code)
				assert.Zero(t, calls.Load())
				assert.Empty(t, path)
				assertSubmissionWallet(t, fixture, 2000000000)
				var count int64
				require.NoError(t, fixture.db.Model(&model.GatewayBilling{}).Count(&count).Error)
				assert.Zero(t, count)
				return
			}
			assert.EqualValues(t, 1, calls.Load())
			assert.Equal(t, tc.path, path)
			assert.Equal(t, tc.body, string(body))
			assert.Equal(t, tc.status, response.Code, response.Body.String())
			if tc.status == http.StatusOK {
				assert.Equal(t, tc.response, response.Body.String())
			}
			assertSubmissionWallet(t, fixture, 2000000000-tc.sale)
			var billing model.GatewayBilling
			require.NoError(t, fixture.db.First(&billing).Error)
			var intent model.GatewaySettlement
			require.NoError(t, common.Unmarshal([]byte(billing.Settlement), &intent))
			assert.Equal(t, tc.sale, intent.Request.Receivable.Nanos)
			var entries, replay []model.LedgerEntry
			require.NoError(t, fixture.db.Where("request_id = ?", billing.RequestID).Order("id").Find(&entries).Error)
			var holds, releases int
			for _, entry := range entries {
				if entry.Type == model.LedgerEntryPreConsume {
					holds++
				}
				if entry.Type == model.LedgerEntryPreConsumeRelease {
					releases++
				}
			}
			assert.Equal(t, 1, holds)
			assert.Equal(t, 1, releases)
			_, err := model.SettleGateway(billing.RequestID)
			require.NoError(t, err)
			require.NoError(t, fixture.db.Where("request_id = ?", billing.RequestID).Order("id").Find(&replay).Error)
			assert.Equal(t, entries, replay)
		})
	}
}

func TestImageRequestReservesFinalQuantityBeforeUpstream(t *testing.T) {
	for _, tc := range []struct {
		name, body                          string
		override                            int
		count                               int
		passThrough, insufficient, previous bool
		status                              int
	}{
		{name: "top-level count", body: `{"model":"gpt-image-2","n":2,"parameters":{}}`, count: 2},
		{name: "legacy channel quantity override", body: `{"model":"gpt-image-2","n":1}`, override: 4, count: 4},
		{name: "expression channel quantity override", body: `{"model":"gpt-image-2","n":1}`, override: 4, count: 4},
		{name: "pass-through quantity", body: `{"model":"gpt-image-2","n":2,"parameters":{}}`, count: 2, passThrough: true},
		{name: "oversized override rejected", body: `{"model":"gpt-image-2","n":1}`, override: 129, status: 400},
		{name: "insufficient reservation blocks upstream", body: `{"model":"gpt-image-2","n":1}`, override: 4, count: 4, insufficient: true, status: 402},
		{name: "retry drops the previous attempt's quantity", body: `{"model":"gpt-image-2","n":1}`, count: 1, previous: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var received [][]byte
			var held []int64
			var fixture *nativeGatewayTaskFixture
			fixture, engine := newCanonicalRelayFixture(t, protocol.OpenAIImages, "gpt-image-2", func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				received = append(received, body)
				var billing model.GatewayBilling
				require.NoError(t, fixture.db.Where("phase = ?", "reserved").First(&billing).Error)
				held = append(held, billing.Reservation.Nanos)
				wallet, err := model.GetUserWallet(fixture.user.Id)
				require.NoError(t, err)
				assert.EqualValues(t, 2000000000-billing.Reservation.Nanos, wallet.Balance.Nanos)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(502)
				_, _ = io.WriteString(w, `{"error":{"message":"fixture upstream failure","type":"upstream_error"}}`)
			})
			if tc.override != 0 {
				override, err := common.Marshal(map[string]any{"operations": []any{map[string]any{"path": "n", "mode": "set", "value": tc.override}}})
				require.NoError(t, err)
				require.NoError(t, fixture.db.Model(&model.Channel{}).Where("id = ?", fixture.channel.ID).Update("parameter_overrides", string(override)).Error)
			}
			if tc.passThrough {
				settings := fixture.provider.GetSetting()
				settings.PassThroughBodyEnabled = true
				encoded, err := common.Marshal(settings)
				require.NoError(t, err)
				require.NoError(t, fixture.db.Model(&model.Provider{}).Where("id = ?", fixture.provider.Id).Update("setting", string(encoded)).Error)
			}
			if tc.insufficient {
				_, err := model.PostLedgerEntries(model.LedgerPosting{Account: model.UserWalletAccount(fixture.user.Id), Type: model.LedgerEntryAdjustment, Amount: money.Amount{Nanos: -1980000000, Currency: "USD"}, IdempotencyKey: "image-insufficient"})
				require.NoError(t, err)
			}
			send := func(body string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				engine.ServeHTTP(w, r)
				return w
			}
			if tc.previous {
				response := send(`{"model":"gpt-image-2","n":4}`)
				require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
				require.Len(t, held, 1)
				assert.EqualValues(t, 160000000, held[0])
			}
			response := send(tc.body)
			if tc.status != 0 {
				assert.Equal(t, tc.status, response.Code, response.Body.String())
				assert.Empty(t, received)
				var count int64
				require.NoError(t, fixture.db.Model(&model.GatewayBilling{}).Count(&count).Error)
				assert.Zero(t, count)
				cash := int64(2000000000)
				if tc.insufficient {
					cash = 20000000
				}
				assertSubmissionWallet(t, fixture, cash)
				return
			}
			assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), "channel_unavailable")
			wantCalls := 1
			if tc.previous {
				wantCalls++
			}
			require.Len(t, received, wantCalls)
			require.Len(t, held, wantCalls)
			var body map[string]any
			require.NoError(t, common.Unmarshal(received[len(received)-1], &body))
			assert.EqualValues(t, tc.count, body["n"])
			assert.EqualValues(t, int64(tc.count)*40000000, held[len(held)-1], "validated effective wire quantity determines the sole pre-I/O hold")
			assertSubmissionWallet(t, fixture, 2000000000)
			var billings []model.GatewayBilling
			require.NoError(t, fixture.db.Find(&billings).Error)
			for _, billing := range billings {
				var entries []model.LedgerEntry
				require.NoError(t, fixture.db.Where("request_id = ?", billing.RequestID).Find(&entries).Error)
				holds, releases := 0, 0
				for _, entry := range entries {
					if entry.Type == model.LedgerEntryPreConsume {
						holds++
					}
					if entry.Type == model.LedgerEntryPreConsumeRelease {
						releases++
					}
					assert.NotEqual(t, model.LedgerAccountPlatformRevenue, entry.AccountType, "failed business delivery must not be sold")
				}
				assert.Equal(t, 1, holds)
				assert.Equal(t, 1, releases)
				_, _ = model.SettleGateway(billing.RequestID)
				var after []model.LedgerEntry
				require.NoError(t, fixture.db.Where("request_id = ?", billing.RequestID).Find(&after).Error)
				assert.Equal(t, entries, after)
			}
		})
	}
}
