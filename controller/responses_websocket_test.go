package controller

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/capability"
	"github.com/QuantumNous/new-api/pkg/channelhealth"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/pkg/testdb"
	"github.com/QuantumNous/new-api/relay"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var responsesWSTestUserSequence atomic.Int64

func setupResponsesWSRequestTest(t *testing.T) (*model.User, *model.Token) {
	t.Helper()
	require.NoError(t, i18n.Init())
	previousRedis := common.RedisEnabled
	previousRDB := common.RDB
	previousMaster, previousMemory := common.IsMasterNode, common.MemoryCacheEnabled
	previousEnabled := setting.ModelRequestRateLimitEnabled
	previousDuration := setting.ModelRequestRateLimitDurationMinutes
	previousTotal := setting.ModelRequestRateLimitCount
	previousSuccess := setting.ModelRequestRateLimitSuccessCount
	setting.ModelRequestRateLimitMutex.Lock()
	previousGroups := setting.ModelRequestRateLimitGroup
	setting.ModelRequestRateLimitGroup = nil
	setting.ModelRequestRateLimitMutex.Unlock()
	common.IsMasterNode, common.MemoryCacheEnabled, common.RedisEnabled = false, false, false
	setting.ModelRequestRateLimitEnabled = false
	t.Cleanup(func() {
		common.RedisEnabled = previousRedis
		common.RDB = previousRDB
		common.IsMasterNode, common.MemoryCacheEnabled = previousMaster, previousMemory
		setting.ModelRequestRateLimitEnabled = previousEnabled
		setting.ModelRequestRateLimitDurationMinutes = previousDuration
		setting.ModelRequestRateLimitCount = previousTotal
		setting.ModelRequestRateLimitSuccessCount = previousSuccess
		setting.ModelRequestRateLimitMutex.Lock()
		setting.ModelRequestRateLimitGroup = previousGroups
		setting.ModelRequestRateLimitMutex.Unlock()
	})
	// Finalized request and attempt records share the isolated PostgreSQL schema
	// with wallets. UseTestDatabases restores LOG_DB and its database type.
	db := model.UseTestDatabases(t)
	model.LOG_DB = db
	common.SetLogDatabaseType(common.DatabaseTypePostgreSQL)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.ModelOffering{}, &model.ModelOfferingName{}, &model.ModelOfferingMember{}))
	// The shared in-memory limiter outlives each database fixture. Give every
	// user a separate request bucket, including when the tests run with -count.
	user := &model.User{Id: 5062000 + int(responsesWSTestUserSequence.Add(1)), Username: "responses-ws-user", Status: common.UserStatusEnabled, Group: "default", Quota: 1000, AuthVersion: 1}
	require.NoError(t, db.Create(user).Error)
	token := &model.Token{UserId: user.Id, Key: "responseswstoken", Status: common.TokenStatusEnabled, ExpiredTime: -1}
	require.NoError(t, db.Create(token).Error)
	return user, token
}

func newResponsesWSTestRunner(t *testing.T, token *model.Token) (relay.ResponsesWSRequestRunner, *http.Request) {
	t.Helper()
	var runner relay.ResponsesWSRequestRunner
	engine := gin.New()
	require.NoError(t, engine.SetTrustedProxies([]string{"127.0.0.1"}))
	engine.GET("/v1/responses", middleware.TokenAuth(), func(c *gin.Context) {
		runner = newResponsesWSRequestRunner(c)
	})
	request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("Authorization", "Bearer sk-"+token.Key)
	request.Header.Set("X-Forwarded-For", "203.0.113.8")
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Key", "handshake-key")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Extensions", "permessage-deflate")
	request.Header.Set("Sec-WebSocket-Protocol", "responses")
	request.Header.Set("Content-Length", "99")
	request.Header.Set("Content-Encoding", "gzip")
	request.Header.Set("X-Application-Header", "preserved")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	require.NotNil(t, runner)
	return runner, request
}

func TestRealtimeHandshakeLeaseLossRejectsNextGeneration(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint(lost), func(t *testing.T) {
			fake := clock.UseFake(t, time.Unix(100, 0))
			server := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: server.Addr(), MaxRetries: -1})
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			manager := service.NewGatewayRouteQuota(client)
			selection := &service.GatewaySelection{Provider: &model.Provider{Id: 1}, Channel: model.Channel{ID: 1, MaxConcurrency: 1}}
			permit, err := manager.Acquire(t.Context(), selection, 0)
			require.NoError(t, err)
			selection.Permit = permit
			accepted, releaseHandshake := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(releaseHandshake) })
			t.Cleanup(release)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, upgradeErr := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if !assert.NoError(t, upgradeErr) {
					return
				}
				defer ws.Close()
				select {
				case <-releaseHandshake:
				case <-r.Context().Done():
					return
				}
				_, _, _ = ws.ReadMessage()
			}))
			t.Cleanup(upstream.Close)
			call := &relay.ResponsesWSCall{Native: &relay.NativeWSLifecycle{TransportOnly: true}}
			established := false
			call.Native.Exchange = func(c *gin.Context, info *relaycommon.RelayInfo, channelID, providerID int, prepared *relaychannel.PreparedWssRequest, payload []byte) (*dto.Usage, *types.NewAPIError) {
				_, apiErr := relay.ExchangeRealtime(c, info, prepared, func() error {
					// Hold the completed client handshake until renewal and Redis
					// recovery finish, then let ConnectionReady release the Permit.
					close(accepted)
					<-releaseHandshake
					established = true
					call.Native.ConnectionReady()
					return nil
				})
				return nil, apiErr
			}
			call.Native.Established = func() bool { return established }
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Request.Header = make(http.Header)
			var budget *service.GatewayAttemptBudget
			var result service.GatewayAttemptResult
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				service.RunGatewayAttempts(t.Context(), service.GatewayAttemptPolicy{MaxAttempts: 1, TotalFirstByte: 2 * time.Minute, AttemptFirstByte: 10 * time.Minute}, func(current *service.GatewayAttemptBudget, exclusions *service.GatewayExclusions) service.GatewayAttemptResult {
					budget = current
					budget.RequirePermit(selection.Permit)
					info := &relaycommon.RelayInfo{ResponseFacts: &relaycommon.ResponseFacts{}, ProviderMeta: &relaycommon.ProviderMeta{ChannelType: constant.ChannelTypeOpenAI}}
					info.BeginProviderCall = budget.Start
					prepared, prepareErr := relaychannel.PrepareWssTransport(c, info, upstream.URL, make(http.Header))
					require.NoError(t, prepareErr)
					selection.Admission = channelhealth.Admission{ChannelID: selection.Channel.ID}
					result = runRealtimeTransportAttempt(c, call, &textGatewayRun{}, info, selection, budget, func() {}, prepared)
					return result
				})
			}()
			select {
			case <-accepted:
			case <-time.After(5 * time.Second):
				t.Fatal("actual Provider WebSocket handshake did not start")
			}
			if lost {
				server.SetError("ERR injected handshake quota renewal failure")
			}
			fake.Advance(time.Minute)
			server.SetError("")
			release()
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				t.Fatal("native WebSocket handshake did not finish")
			}
			next, err := manager.Acquire(t.Context(), selection, 0)
			require.NoError(t, err)
			budget.RequirePermit(next)
			ctx, err := budget.Start()
			if lost {
				assert.ErrorIs(t, err, service.ErrGatewayPermitLost)
				assert.True(t, established)
			} else {
				require.NoError(t, err)
				assert.NoError(t, ctx.Err())
				assert.True(t, established)
				assert.Equal(t, "connection_established", result.Failure.Class)
			}
			t.Cleanup(func() { require.NoError(t, next.Release(context.WithoutCancel(t.Context()), true)) })
		})
	}
}

func TestResponsesWSRequestRunnerRefreshesBillingContextAndCleansBody(t *testing.T) {
	user, token := setupResponsesWSRequestTest(t)
	allowedIP := "203.0.113.8"
	require.NoError(t, model.DB.Model(token).Update("allow_ips", allowedIP).Error)
	runner, handshake := newResponsesWSTestRunner(t, token)
	// A later upstream header mutation must not replace the client's credential.
	handshake.Header.Set("Authorization", "Bearer upstream-secret")
	handshake.Header.Set("X-Forwarded-For", "198.51.100.99")
	var storage common.BodyStorage
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"first"}`))
	require.Nil(t, runner(request, "ws-first", func(c *gin.Context) *types.NewAPIError {
		assert.Equal(t, user.Id, c.GetInt("id"))
		assert.Equal(t, token.Id, c.GetInt("token_id"))
		assert.Equal(t, token.Name, c.GetString("token_name"))
		for _, key := range []string{"token_quota", "user_quota"} {
			_, present := c.Get(key)
			assert.False(t, present, key)
		}
		assert.Equal(t, "203.0.113.8", c.ClientIP())
		assert.Equal(t, "ws-first", c.GetString(common.RequestIdKey))
		assert.Equal(t, "ws-first", c.Request.Context().Value(common.RequestIdKey))
		for _, header := range []string{"Connection", "Upgrade", "Sec-WebSocket-Key", "Sec-WebSocket-Version", "Sec-WebSocket-Extensions", "Sec-WebSocket-Protocol", "Content-Length", "Content-Encoding"} {
			assert.Empty(t, c.GetHeader(header))
		}
		assert.Equal(t, "preserved", c.GetHeader("X-Application-Header"))
		c.Set("previous_turn", true)
		var err error
		storage, err = common.GetBodyStorage(c)
		require.NoError(t, err)
		data, err := storage.Bytes()
		require.NoError(t, err)
		assert.JSONEq(t, `{"input":"first"}`, string(data))
		return nil
	}))
	_, err := storage.Bytes()
	assert.ErrorIs(t, err, common.ErrStorageClosed)
	testdb.SeedLegacyKeyQuota(t, model.DB, token.Id, testdb.LegacyKeyQuota{RemainQuota: 100})
	require.NoError(t, model.DB.Model(token).Update("name", "renamed Key").Error)
	testdb.SeedLegacyKeyQuota(t, model.DB, token.Id, testdb.LegacyKeyQuota{})
	require.NoError(t, model.DB.Model(user).Updates(map[string]any{
		"group": "updated", "setting": `{"billing_preference":"wallet_only"}`,
	}).Error)
	request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"second"}`))
	require.Nil(t, runner(request, "ws-second", func(c *gin.Context) *types.NewAPIError {
		assert.Equal(t, token.Id, c.GetInt("token_id"))
		assert.Equal(t, "renamed Key", c.GetString("token_name"))
		for _, key := range []string{"user_quota", "token_quota", "token_unlimited_quota", "token_model_limit_enabled", "token_model_limit"} {
			_, present := c.Get(key)
			assert.False(t, present, key)
		}
		assert.Equal(t, "updated", common.GetContextKeyString(c, constant.ContextKeyUsingGroup))
		userSetting, ok := common.GetContextKeyType[dto.UserSetting](c, constant.ContextKeyUserSetting)
		require.True(t, ok)
		assert.Equal(t, "wallet_only", userSetting.BillingPreference)
		assert.Equal(t, "ws-second", c.GetString(common.RequestIdKey))
		_, leaked := c.Get("previous_turn")
		assert.False(t, leaked)
		currentStorage, err := common.GetBodyStorage(c)
		require.NoError(t, err)
		assert.NotSame(t, storage, currentStorage)
		body, err := currentStorage.Bytes()
		require.NoError(t, err)
		assert.JSONEq(t, `{"input":"second"}`, string(body))
		return nil
	}))
}

func TestResponsesWSRequestRunnerRejectsRevokedCredentials(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		update func(*testing.T, *model.User, *model.Token)
	}{
		{name: "disabled token", status: http.StatusUnauthorized, update: func(t *testing.T, _ *model.User, token *model.Token) {
			require.NoError(t, model.DB.Model(token).Update("status", common.TokenStatusDisabled).Error)
		}},
		{name: "expired token", status: http.StatusUnauthorized, update: func(t *testing.T, _ *model.User, token *model.Token) {
			require.NoError(t, model.DB.Model(token).Update("expired_time", time.Now().Add(-time.Minute).Unix()).Error)
		}},
		{name: "revoked token", status: http.StatusUnauthorized, update: func(t *testing.T, _ *model.User, token *model.Token) {
			require.NoError(t, model.DB.Model(token).Update("key", "revoked-responses-key").Error)
		}},
		{name: "exhausted token status", status: http.StatusUnauthorized, update: func(t *testing.T, _ *model.User, token *model.Token) {
			require.NoError(t, model.DB.Model(token).Update("status", common.TokenStatusExhausted).Error)
		}},
		{name: "zero retired quota", status: http.StatusOK, update: func(t *testing.T, _ *model.User, token *model.Token) {
			testdb.SeedLegacyKeyQuota(t, model.DB, token.Id, testdb.LegacyKeyQuota{})
		}},
		{name: "disabled user", status: http.StatusForbidden, update: func(t *testing.T, user *model.User, _ *model.Token) {
			require.NoError(t, model.DB.Model(user).Update("status", common.UserStatusDisabled).Error)
		}},
		{name: "deleted token", status: http.StatusUnauthorized, update: func(t *testing.T, _ *model.User, token *model.Token) {
			require.NoError(t, model.DB.Delete(token).Error)
		}},
		{name: "changed IP restriction", status: http.StatusForbidden, update: func(t *testing.T, _ *model.User, token *model.Token) {
			require.NoError(t, model.DB.Model(token).Update("allow_ips", "198.51.100.0/24").Error)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user, token := setupResponsesWSRequestTest(t)
			runner, _ := newResponsesWSTestRunner(t, token)
			require.Nil(t, runner(httptest.NewRequest(http.MethodPost, "/v1/responses", nil), "before", func(*gin.Context) *types.NewAPIError { return nil }))
			tc.update(t, user, token)
			called := false
			apiError := runner(httptest.NewRequest(http.MethodPost, "/v1/responses", nil), "after", func(*gin.Context) *types.NewAPIError {
				called = true
				return nil
			})
			if tc.status == http.StatusOK {
				require.Nil(t, apiError)
				assert.True(t, called, "an enabled Key with zero retired quota remains valid")
				return
			}
			require.NotNil(t, apiError)
			assert.Equal(t, tc.status, apiError.StatusCode)
			assert.False(t, called)
			assert.NotContains(t, apiError.Error(), token.Key)
		})
	}
}

func TestResponsesWSRequestRunnerUsesExistingMemoryRateLimit(t *testing.T) {
	require.NoError(t, i18n.Init())
	_, token := setupResponsesWSRequestTest(t)
	setting.ModelRequestRateLimitEnabled = true
	setting.ModelRequestRateLimitDurationMinutes = 1
	setting.ModelRequestRateLimitCount = 1
	setting.ModelRequestRateLimitSuccessCount = 100
	runner, _ := newResponsesWSTestRunner(t, token)
	called := 0
	handle := func(*gin.Context) *types.NewAPIError { called++; return nil }
	require.Nil(t, runner(httptest.NewRequest(http.MethodPost, "/v1/responses", nil), "first", handle))
	apiError := runner(httptest.NewRequest(http.MethodPost, "/v1/responses", nil), "limited", handle)
	require.NotNil(t, apiError)
	assert.Equal(t, http.StatusTooManyRequests, apiError.StatusCode)
	message := i18n.Translate("", i18n.MsgRateLimitTotalReached, map[string]any{"Minutes": 1, "Max": 1})
	assert.Equal(t, common.MessageWithRequestId(message, "limited"), apiError.Error())
	assert.Equal(t, 1, called)
}

func TestResponsesWSRequestRunnerSharesRedisSuccessLimitWithHTTP(t *testing.T) {
	_, token := setupResponsesWSRequestTest(t)
	redisServer := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	common.RedisEnabled = true
	common.RDB = client
	setting.ModelRequestRateLimitEnabled = true
	setting.ModelRequestRateLimitDurationMinutes = 1
	setting.ModelRequestRateLimitCount = 0
	setting.ModelRequestRateLimitSuccessCount = 1
	runner, _ := newResponsesWSTestRunner(t, token)
	failed := types.NewErrorWithStatusCode(errors.New("invalid input"), types.ErrorCodeInvalidRequest, http.StatusBadRequest)
	assert.Same(t, failed, runner(httptest.NewRequest(http.MethodPost, "/v1/responses", nil), "failed", func(*gin.Context) *types.NewAPIError { return failed }))
	require.Nil(t, runner(httptest.NewRequest(http.MethodPost, "/v1/responses", nil), "successful", func(*gin.Context) *types.NewAPIError { return nil }))
	engine := gin.New()
	called := false
	engine.POST("/v1/responses", middleware.TokenAuth(), middleware.ModelRequestRateLimit(), func(c *gin.Context) { called = true })
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	request.Header.Set("Authorization", "Bearer sk-"+token.Key)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	assert.Equal(t, http.StatusTooManyRequests, response.Code)
	assert.False(t, called)
}

type responsesWSBillingTest struct {
	httpUpstream http.HandlerFunc
	httpDone     chan struct{}
	user         *model.User
	token        *model.Token
	provider     *model.Provider
	channel      *model.Channel
	offering     *model.ModelOffering
	sale         *model.PriceVersion
	purchase     *model.PriceVersion
	client       *websocket.Conn
	done         chan struct{}
	upstreamDone chan struct{}
	connections  atomic.Int32
	gatewayURL   string
	funded       int64
}

func (fixture *responsesWSBillingTest) closeAndWait(t *testing.T) {
	t.Helper()
	_ = fixture.client.Close()
	select {
	case <-fixture.done:
	case <-time.After(3 * time.Second):
		t.Error("gateway handler did not stop after closing its client")
	}
	if fixture.connections.Load() > 0 {
		select {
		case <-fixture.upstreamDone:
		case <-time.After(3 * time.Second):
			t.Error("upstream connection was not closed")
		}
	}
}

func (fixture *responsesWSBillingTest) waitForSettlements(t *testing.T, count int64) {
	t.Helper()
	require.Eventually(t, func() bool {
		var settled int64
		err := model.DB.Model(&model.GatewayBilling{}).Where("user_id = ? AND phase = ? AND published = ?", fixture.user.Id, "settled", true).Count(&settled).Error
		return err == nil && settled == count
	}, 3*time.Second, 10*time.Millisecond, "each logical request must finish settlement and publish its records")
}

func assertResponsesWSHealth(t *testing.T, fixture *responsesWSBillingTest, requests, successes int) {
	t.Helper()
	snapshots, err := service.ChannelHealth.Snapshots(t.Context(), []int{fixture.channel.ID})
	require.NoError(t, err)
	health := snapshots[fixture.channel.ID]
	assert.Equal(t, requests, health.Samples)
	if requests > 0 {
		assert.Equal(t, float64(successes)/float64(requests), health.SuccessRate)
	} else {
		assert.Zero(t, health.Failures)
		assert.Equal(t, "closed", health.Circuit)
	}
}

func newResponsesWSBillingTest(t *testing.T, expression string, handle func(*websocket.Conn, *http.Request), httpEvents ...string) *responsesWSBillingTest {
	t.Helper()
	user, token := setupResponsesWSRequestTest(t)
	previousTimeout, previousCount := constant.StreamingTimeout, constant.CountToken
	constant.StreamingTimeout, constant.CountToken = 30, true
	t.Cleanup(func() { constant.StreamingTimeout, constant.CountToken = previousTimeout, previousCount })
	t.Setenv("GATEWAY_SETTLEMENT_JOURNAL_DIR", t.TempDir())
	t.Setenv("GATEWAY_MAX_ATTEMPTS", "3")
	t.Setenv("GATEWAY_TOTAL_FIRST_BYTE_SECONDS", "90")
	t.Setenv("GATEWAY_ATTEMPT_FIRST_BYTE_SECONDS", "30")
	require.NoError(t, model.InitSettlementCurrency())
	require.NoError(t, model.DB.AutoMigrate(&model.Option{}, &model.Provider{}, &model.Vendor{}, &model.Model{}, &model.Channel{}, &model.TokenOfferingPolicy{}, &model.TokenOfferingException{}))
	require.NoError(t, model.MigratePricing(model.DB))
	require.NoError(t, model.MigrateGatewayBilling(model.DB))
	require.NoError(t, model.MigrateBalanceReminders(model.DB))
	require.NoError(t, model.MigrateLogDB())
	previousHealth, previousQuota := service.ChannelHealth, service.GatewayRouteQuota
	service.ChannelHealth = channelhealth.New(nil)
	service.GatewayRouteQuota = service.NewGatewayRouteQuota(nil)
	t.Cleanup(func() { service.ChannelHealth, service.GatewayRouteQuota = previousHealth, previousQuota })
	previousRatio := ratio_setting.GroupRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1}`))
	common.OptionMapRWMutex.Lock()
	previousOptions := common.OptionMap
	common.OptionMap = map[string]string{"GroupRatio": `{"default":1}`, "GroupGroupRatio": `{}`}
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(previousRatio))
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptions
		common.OptionMapRWMutex.Unlock()
	})
	require.NoError(t, model.DB.Model(user).Update("quota", 0).Error)
	_, err := model.TopUpWallet(model.WalletTopUp{UserId: user.Id, Source: money.Amount{Nanos: 1_000_000_000, Currency: "USD"}, Reference: "responses-ws-initial"})
	require.NoError(t, err)

	fixture := &responsesWSBillingTest{user: user, token: token, done: make(chan struct{}), upstreamDone: make(chan struct{}), httpDone: make(chan struct{}, 1), funded: 1_000_000_000}
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	common.RDB, common.RedisEnabled = redisClient, true
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })
	var upstreamClosed sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			if fixture.httpUpstream != nil {
				fixture.httpUpstream(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			for _, event := range httpEvents {
				_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
			}
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if !assert.NoError(t, err) {
			return
		}
		fixture.connections.Add(1)
		stop := context.AfterFunc(t.Context(), func() { _ = ws.Close() })
		defer stop()
		defer ws.Close()
		defer upstreamClosed.Do(func() { close(fixture.upstreamDone) })
		handle(ws, r)
	}))
	t.Cleanup(upstream.Close)
	provider := &model.Provider{Name: "responses-ws-upstream", Key: "upstream-first\nupstream-second", Status: common.ChannelStatusEnabled, Type: constant.ChannelTypeOpenAI, BaseURL: &upstream.URL,
		ProviderInfo: model.ProviderInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModePolling}}
	provider.SetSetting(dto.ProviderSettings{ResponsesWebSocketEnabled: true})
	provider.SetOtherSettings(dto.ProviderOtherSettings{AllowServiceTier: true})
	require.NoError(t, model.DB.Create(provider).Error)
	fixture.provider = provider
	publisher := &model.Vendor{Name: "Responses publisher", Status: 1}
	require.NoError(t, model.DB.Create(publisher).Error)
	unified := &model.Model{ModelName: "gpt-4o", VendorID: publisher.Id, Status: 1, NameRule: model.NameRuleExact,
		Capabilities: capability.Set{Features: []capability.Feature{capability.Streaming, capability.ToolCalling}, InputModalities: []capability.Modality{capability.Text}, OutputModalities: []capability.Modality{capability.Text}}}
	require.NoError(t, model.DB.Create(unified).Error)
	fixture.channel = &model.Channel{ProviderID: provider.Id, ModelID: unified.Id, Protocol: protocol.OpenAIResponses, UpstreamModelName: "gpt-4o", ParameterOverrides: `{}`, MappingConfirmed: true, Status: model.ChannelEnabled, Weight: 1}
	require.NoError(t, model.DB.Create(fixture.channel).Error)
	fixture.offering = &model.ModelOffering{Name: "ws-billing", Members: []model.ModelOfferingMember{{ModelID: unified.Id, Tier: 1}}, AllowLossFallback: true}
	require.NoError(t, model.SaveModelOffering(t.Context(), fixture.offering, "save"))
	fixture.sale = &model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: expression, EffectiveAt: clock.Now().Unix() - 1}
	fixture.purchase = &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: `tier("purchase", p + c)`, EffectiveAt: clock.Now().Unix() - 1}
	require.NoError(t, model.CreatePriceVersion(t.Context(), fixture.sale, user.Id))
	require.NoError(t, model.CreatePriceVersion(t.Context(), fixture.purchase, user.Id))
	require.NoError(t, model.SaveModelOffering(t.Context(), fixture.offering, "enable"))
	service.InitHttpClient()
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set(service.GatewayRouteRandomSource, func(uint64) uint64 { return 0 })
		c.Next()
	})
	engine.GET("/v1/responses", middleware.GatewayTokenAuth(), func(c *gin.Context) {
		defer close(fixture.done)
		c.Set(common.RequestIdKey, "responses-ws-billing")
		ResponsesWebSocket(c)
	})
	engine.POST("/v1/responses", middleware.BodyStorageCleanup(), middleware.GatewayTokenAuth(), middleware.ResolveTextOffering(), middleware.ModelRequestRateLimit(), func(c *gin.Context) {
		defer func() { fixture.httpDone <- struct{}{} }()
		c.Set(common.RequestIdKey, "responses-http-billing")
		TextGateway(c)
	})
	gateway := httptest.NewServer(engine)
	fixture.gatewayURL = gateway.URL
	t.Cleanup(gateway.Close)
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gateway.URL, "http")+"/v1/responses", http.Header{"Authorization": []string{"Bearer sk-" + token.Key}})
	require.NoError(t, err)
	fixture.client = client
	t.Cleanup(func() { fixture.closeAndWait(t) })
	return fixture
}

func TestResponsesInterruptedStreamHealth(t *testing.T) {
	for _, scenario := range []string{"websocket timeout", "sse timeout", "sse client cancel"} {
		t.Run(scenario, func(t *testing.T) {
			var idleClock *clock.Fake
			if scenario == "sse timeout" {
				idleClock = clock.UseFake(t, clock.Now())
			}
			httpStopped := make(chan struct{})
			events := []string{`{"type":"response.created","response":{"id":"partial","status":"in_progress"}}`, `{"type":"response.output_text.delta","delta":"hello"}`}
			fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
				if _, _, err := ws.ReadMessage(); !assert.NoError(t, err) {
					return
				}
				for _, event := range events {
					if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(event))) {
						return
					}
				}
				_, _, _ = ws.ReadMessage()
			})
			constant.StreamingTimeout = 1
			fixture.httpUpstream = func(w http.ResponseWriter, r *http.Request) {
				defer close(httpStopped)
				w.Header().Set("Content-Type", "text/event-stream")
				for _, event := range events {
					_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
				}
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}
			if scenario == "websocket timeout" {
				require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"hello"}`)))
				assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
				assert.Equal(t, "response.output_text.delta", readResponsesWSTestEvent(t, fixture.client)["type"])
				_, _, err := fixture.client.ReadMessage()
				require.Error(t, err, "idle timeout closes the incomplete stream")
			} else {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				request, err := http.NewRequestWithContext(ctx, http.MethodPost, fixture.gatewayURL+"/v1/responses", strings.NewReader(`{"model":"ws-billing","input":"hello","stream":true}`))
				require.NoError(t, err)
				request.Header.Set("Authorization", "Bearer sk-"+fixture.token.Key)
				request.Header.Set("Content-Type", "application/json")
				response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
				require.NoError(t, err)
				defer response.Body.Close()
				reader := bufio.NewReader(response.Body)
				for {
					line, err := reader.ReadString('\n')
					require.NoError(t, err)
					if strings.HasPrefix(line, "data: ") && strings.Contains(line, "response.output_text.delta") {
						assert.JSONEq(t, events[1], strings.TrimPrefix(strings.TrimSpace(line), "data: "))
						break
					}
				}
				if scenario == "sse client cancel" {
					cancel()
				} else {
					// Business delivery arms the idle timer before flushing this delta.
					// Advance that timer without spending the client's three-second
					// deadline on a real wait alongside routing and settlement I/O.
					idleClock.Advance(time.Second - time.Nanosecond)
					select {
					case <-httpStopped:
						t.Fatal("upstream stopped before the stream idle deadline")
					default:
					}
					idleClock.Advance(time.Nanosecond)
					_, err = io.Copy(io.Discard, reader)
					require.NoError(t, err)
				}
				require.NoError(t, response.Body.Close())
				select {
				case <-fixture.httpDone:
				case <-time.After(3 * time.Second):
					t.Fatal("interrupted SSE request did not finish")
				}
			}
			fixture.closeAndWait(t)
			requests := assertResponsesWSAccounting(t, fixture, []int64{0})
			assert.Equal(t, "estimated", requests[0].UsageSource)
			assert.Equal(t, int64(1), requests[0].Usage.CompletionTokens)
			if scenario == "sse client cancel" {
				assertResponsesWSHealth(t, fixture, 0, 0)
				return
			}
			assertResponsesWSHealth(t, fixture, 1, 0)
		})
	}
}

func readResponsesWSTestEvent(t *testing.T, client *websocket.Conn) map[string]any {
	t.Helper()
	require.NoError(t, client.SetReadDeadline(time.Now().Add(3*time.Second)))
	_, data, err := client.ReadMessage()
	require.NoError(t, err)
	var event map[string]any
	require.NoError(t, common.Unmarshal(data, &event))
	return event
}

func assertResponsesWSAccounting(t *testing.T, fixture *responsesWSBillingTest, charges []int64) []model.GatewayRequest {
	t.Helper()
	var requests []model.GatewayRequest
	require.NoError(t, model.LOG_DB.Where("token_id = ?", fixture.token.Id).Order("id").Find(&requests).Error)
	require.Len(t, requests, len(charges))
	var billings []model.GatewayBilling
	require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Order("request_id").Find(&billings).Error)
	require.Len(t, billings, len(charges))
	var attemptCount int64
	require.NoError(t, model.LOG_DB.Model(&model.GatewayAttempt{}).Count(&attemptCount).Error)
	assert.Equal(t, int64(len(charges)), attemptCount, "rejected local requests must not create Provider attempts")
	var charged int64
	for index, request := range requests {
		assert.Equal(t, fixture.user.Id, request.UserID)
		assert.Equal(t, fixture.token.Id, request.TokenID)
		assert.Equal(t, fixture.offering.ID, request.ModelOfferingID)
		assert.Equal(t, "ws-billing", request.OfferingName)
		assert.Equal(t, "ws-billing", request.RequestedName)
		assert.Equal(t, fixture.sale.ID, request.SalePriceVersionID)
		assert.Equal(t, fixture.sale.Expression, request.SaleExpression)
		assert.Equal(t, "1", request.GroupRatio)
		assert.Equal(t, "1", request.SaleFXRate)
		assert.Equal(t, money.Amount{Nanos: charges[index], Currency: "USD"}, request.Receivable)
		assert.Equal(t, request.ID, billings[index].RequestID)
		assert.Equal(t, "settled", billings[index].Phase)
		assert.True(t, billings[index].Published)
		var attempts []model.GatewayAttempt
		require.NoError(t, model.LOG_DB.Where("request_id = ?", request.ID).Find(&attempts).Error)
		require.Len(t, attempts, 1, "each logical create contacts the Provider once")
		attempt := attempts[0]
		assert.Equal(t, fixture.channel.ID, attempt.ChannelID)
		assert.Equal(t, fixture.provider.Id, attempt.ProviderID)
		assert.Equal(t, fixture.channel.ModelID, attempt.ModelID)
		assert.Equal(t, 1, attempt.MemberTier)
		assert.Equal(t, fixture.purchase.ID, attempt.PurchasePriceVersionID)
		assert.Equal(t, fixture.purchase.Expression, attempt.PurchaseExpression)
		assert.Equal(t, money.Amount{Nanos: (attempt.Usage.PromptTokens + attempt.Usage.CompletionTokens) * 1000, Currency: "USD"}, attempt.Cost)
		assert.Equal(t, attempt.Cost, attempt.ConvertedCost)
		assert.Equal(t, "1", attempt.AccountingFXRate)
		assert.Equal(t, "stop", attempt.FailoverDecision)
		assert.Equal(t, request.Status, attempt.Status)
		assert.Equal(t, request.Usage.PromptTokens+request.Usage.CompletionTokens, request.Usage.TotalTokens)
		assert.Equal(t, attempt.Usage.PromptTokens+attempt.Usage.CompletionTokens, attempt.Usage.TotalTokens)
		assert.NotEmpty(t, attempt.RouteDiagnostics)
		assert.NotEqual(t, "settlement_pending", request.Status)
		assert.NotContains(t, billings[index].Settlement, "sensitive upstream detail")
		var entries []model.LedgerEntry
		require.NoError(t, model.DB.Where("request_id = ?", request.ID).Find(&entries).Error)
		type posting struct {
			account model.LedgerAccountType
			kind    model.LedgerEntryType
			amount  int64
		}
		var expected []posting
		if reservation := billings[index].Reservation.Nanos; reservation > 0 {
			expected = append(expected, posting{model.LedgerAccountUserWallet, model.LedgerEntryPreConsume, -reservation}, posting{model.LedgerAccountUserWallet, model.LedgerEntryPreConsumeRelease, reservation})
		}
		if charges[index] > 0 {
			expected = append(expected, posting{model.LedgerAccountUserWallet, model.LedgerEntryCharge, -charges[index]}, posting{model.LedgerAccountPlatformRevenue, model.LedgerEntryCharge, charges[index]})
		}
		if attempt.Cost.Nanos > 0 {
			expected = append(expected, posting{model.LedgerAccountProviderPayable, model.LedgerEntryCharge, attempt.Cost.Nanos})
		}
		var actual []posting
		var entryIDs []string
		for _, entry := range entries {
			assert.Equal(t, money.Currency("USD"), entry.Amount.Currency)
			assert.NotZero(t, entry.Amount.Nanos)
			actual = append(actual, posting{entry.AccountType, entry.Type, entry.Amount.Nanos})
			entryIDs = append(entryIDs, fmt.Sprint(entry.Id))
			if entry.AccountType == model.LedgerAccountProviderPayable {
				assert.Equal(t, attempt.ID, entry.AttemptId)
			}
		}
		assert.ElementsMatch(t, expected, actual, "each reservation, release, charge, revenue and Provider cost posts once")
		assert.ElementsMatch(t, entryIDs, request.LedgerEntryIDs)
		charged += charges[index]
	}
	wallet, err := model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	assert.Equal(t, money.Amount{Nanos: fixture.funded - charged, Currency: "USD"}, wallet.Balance)
	reconciliation, err := model.ReconcileWallets(100)
	require.NoError(t, err)
	assert.Empty(t, reconciliation.Mismatches)
	require.NoError(t, model.DB.First(fixture.token, fixture.token.Id).Error)
	require.NoError(t, model.DB.First(fixture.user, fixture.user.Id).Error)
	for _, column := range []string{"remain_quota", "used_quota"} {
		assert.False(t, model.DB.Migrator().HasColumn("tokens", column), column)
	}
	assert.Zero(t, fixture.user.Quota)
	assert.Zero(t, fixture.user.UsedQuota)
	return requests
}

func TestResponsesWebSocketReusesConnectionAndSettlesEachRequest(t *testing.T) {
	type upstreamRequest struct {
		Authorization      string
		Type               string `json:"type"`
		Model              string `json:"model"`
		PreviousResponseID string `json:"previous_response_id"`
		ServiceTier        string `json:"service_tier"`
	}
	received := make(chan upstreamRequest, 3)
	fixture := newResponsesWSBillingTest(t, `param("service_tier") == "priority" ? tier("priority", p * 4) : tier("base", p * 2)`, func(ws *websocket.Conn, r *http.Request) {
		for index := 1; ; index++ {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			var event upstreamRequest
			if !assert.NoError(t, common.Unmarshal(data, &event)) {
				return
			}
			event.Authorization = r.Header.Get("Authorization")
			received <- event
			if index == 2 {
				duplicate := `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}`
				if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(duplicate))) {
					return
				}
			}
			terminal := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","status":"completed","model":"ws-billing","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}`, index)
			if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(terminal))) {
				return
			}
		}
	})
	client := fixture.client

	for index, payload := range []string{
		`{"type":"response.create","model":"ws-billing","input":"first","service_tier":"default"}`,
		`{"type":"response.create","model":"ws-billing","input":"second","previous_response_id":"resp_1","service_tier":"priority"}`,
	} {
		require.NoError(t, client.WriteMessage(websocket.TextMessage, []byte(payload)))
		require.NoError(t, client.SetReadDeadline(time.Now().Add(3*time.Second)))
		_, data, err := client.ReadMessage()
		require.NoError(t, err)
		var terminal struct {
			Type     string `json:"type"`
			Response struct {
				ID string `json:"id"`
			} `json:"response"`
		}
		require.NoError(t, common.Unmarshal(data, &terminal))
		require.Equal(t, "response.completed", terminal.Type, "unexpected response: %s", data)
		assert.Equal(t, fmt.Sprintf("resp_%d", index+1), terminal.Response.ID)
		observed := <-received
		assert.Equal(t, "Bearer upstream-first", observed.Authorization)
		assert.Equal(t, "gpt-4o", observed.Model)
		assert.Equal(t, "response.create", observed.Type)
		if index == 0 {
			assert.Empty(t, observed.PreviousResponseID)
			assert.Equal(t, "default", observed.ServiceTier)
			// A cheaper Channel added between creates must not redirect this session.
			alternative := *fixture.channel
			alternative.ID, alternative.UpstreamModelName = 0, "different-upstream-model"
			require.NoError(t, model.DB.Create(&alternative).Error)
			price := &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: alternative.ID, Currency: "USD", Expression: `tier("purchase", p * 0.5 + c * 0.5)`, EffectiveAt: clock.Now().Unix() - 1}
			require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.user.Id))
		} else {
			assert.Equal(t, "resp_1", observed.PreviousResponseID)
			assert.Equal(t, "priority", observed.ServiceTier)
		}
	}

	fixture.waitForSettlements(t, 2)
	wallet, err := model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	_, err = model.PostLedgerEntries(model.LedgerPosting{Account: model.UserWalletAccount(fixture.user.Id), Type: model.LedgerEntryAdjustment, Amount: money.Amount{Nanos: -wallet.Balance.Nanos, Currency: "USD"}})
	require.NoError(t, err)
	fixture.funded = 6_000_000
	require.NoError(t, client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"exhausted"}`)))
	require.NoError(t, client.SetReadDeadline(time.Now().Add(3*time.Second)))
	_, data, err := client.ReadMessage()
	require.NoError(t, err)
	var rejection struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
	}
	require.NoError(t, common.Unmarshal(data, &rejection))
	assert.Equal(t, "error", rejection.Type)
	assert.Equal(t, http.StatusPaymentRequired, rejection.Status)
	assert.Equal(t, int32(1), fixture.connections.Load())
	assert.Empty(t, received, "an empty wallet must reject the next request before contacting the Provider")
	fixture.closeAndWait(t)
	requests := assertResponsesWSAccounting(t, fixture, []int64{2_000_000, 4_000_000})
	for index, request := range requests {
		assert.Equal(t, fmt.Sprintf("responses-ws-billing-ws-%d", index), request.ID)
		assert.Equal(t, int64(1000), request.Usage.PromptTokens)
		assert.Equal(t, int64(10), request.Usage.CompletionTokens)
		assert.Equal(t, "upstream", request.UsageSource)
		assert.Equal(t, "success", request.Status)
	}
	assert.Equal(t, common.TokenStatusEnabled, fixture.token.Status)
	assertResponsesWSHealth(t, fixture, 2, 2)
}

// Both transports must reach the same upstream target with the same
// credential placement and settle identically for every supported channel type.
func TestResponsesWebSocketDialsNativeResponsesChannelTypes(t *testing.T) {
	type upstreamTarget struct {
		Path, Authorization, QueryKey string
	}
	queryAuthRoute := dto.ProviderOtherSettings{AdvancedCustom: &dto.AdvancedCustomConfig{Routes: []dto.AdvancedCustomRoute{{
		IncomingPath: "/v1/responses", UpstreamPath: "/upstream/responses",
		Auth: &dto.AdvancedCustomRouteAuth{Type: dto.AdvancedCustomAuthTypeQuery, Name: "api_key", Value: "{api_key}"},
	}}}}
	bearer := func(key string) upstreamTarget {
		return upstreamTarget{Path: "/v1/responses", Authorization: "Bearer " + key}
	}
	for _, tc := range []struct {
		name          string
		channelType   int
		otherSettings dto.ProviderOtherSettings
		want          func(key string) upstreamTarget
	}{
		{name: "new api", channelType: constant.ChannelTypeNewAPI, want: bearer},
		{name: "sub2api", channelType: constant.ChannelTypeSub2API, want: bearer},
		{name: "advanced custom query auth", channelType: constant.ChannelTypeAdvancedCustom, otherSettings: queryAuthRoute, want: func(key string) upstreamTarget {
			return upstreamTarget{Path: "/upstream/responses", QueryKey: key}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const terminal = `{"type":"response.completed","response":{"id":"first","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}`
			targets := make(chan upstreamTarget, 2)
			observe := func(r *http.Request) {
				targets <- upstreamTarget{Path: r.URL.Path, Authorization: r.Header.Get("Authorization"), QueryKey: r.URL.Query().Get("api_key")}
			}
			fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, r *http.Request) {
				observe(r)
				if _, _, err := ws.ReadMessage(); !assert.NoError(t, err) {
					return
				}
				if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(terminal))) {
					return
				}
				_, _, _ = ws.ReadMessage()
			})
			fixture.httpUpstream = func(w http.ResponseWriter, r *http.Request) {
				observe(r)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", terminal)
			}
			fixture.provider.SetOtherSettings(tc.otherSettings)
			require.NoError(t, model.DB.Model(fixture.provider).Updates(map[string]any{"type": tc.channelType, "settings": fixture.provider.OtherSettings}).Error)

			require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"hi"}`)))
			require.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			select {
			case target := <-targets:
				assert.Equal(t, tc.want("upstream-first"), target)
			case <-time.After(3 * time.Second):
				t.Fatal("Provider did not receive the WebSocket request")
			}

			fixture.waitForSettlements(t, 1)
			request, err := http.NewRequest(http.MethodPost, fixture.gatewayURL+"/v1/responses", strings.NewReader(`{"model":"ws-billing","input":"hi","stream":true}`))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer sk-"+fixture.token.Key)
			request.Header.Set("Content-Type", "application/json")
			response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
			require.NoError(t, err)
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			assert.Equal(t, http.StatusOK, response.StatusCode)
			assert.Contains(t, string(body), terminal)
			select {
			case <-fixture.httpDone:
			case <-time.After(3 * time.Second):
				t.Fatal("HTTP request did not finish")
			}
			// The polling multi-key channel rotates to its second key for the HTTP request.
			select {
			case target := <-targets:
				assert.Equal(t, tc.want("upstream-second"), target)
			case <-time.After(3 * time.Second):
				t.Fatal("Provider did not receive the HTTP request")
			}
			fixture.closeAndWait(t)
			requests := assertResponsesWSAccounting(t, fixture, []int64{2_000_000, 2_000_000})
			for _, request := range requests {
				assert.Equal(t, "success", request.Status)
				assert.Equal(t, "upstream", request.UsageSource)
				assert.Equal(t, int64(1000), request.Usage.PromptTokens)
				assert.Equal(t, int64(10), request.Usage.CompletionTokens)
			}
		})
	}
}

func TestResponsesWebSocketDisconnectSettlesDeliveredOutputOnce(t *testing.T) {
	fixture := newResponsesWSBillingTest(t, `tier("output", c * 2)`, func(ws *websocket.Conn, _ *http.Request) {
		if _, _, err := ws.ReadMessage(); !assert.NoError(t, err) {
			return
		}
		for _, event := range []string{
			`{"type":"response.created","response":{"id":"partial","status":"in_progress"}}`,
			`{"type":"response.output_text.delta","delta":"hello"}`,
		} {
			if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(event))) {
				return
			}
		}
		_, _, _ = ws.ReadMessage()
	})
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"hi","max_output_tokens":1}`)))
	assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
	delta := readResponsesWSTestEvent(t, fixture.client)
	require.Equal(t, "response.output_text.delta", delta["type"])
	assert.Equal(t, "hello", delta["delta"])
	fixture.closeAndWait(t)
	requests := assertResponsesWSAccounting(t, fixture, []int64{2_000})
	assert.Equal(t, int64(1), requests[0].Usage.CompletionTokens)
	assert.Equal(t, "estimated", requests[0].UsageSource)
	assertResponsesWSHealth(t, fixture, 0, 0)
}

func TestResponsesWebSocketCancelErrorDoesNotFinishActiveRequest(t *testing.T) {
	complete := make(chan struct{})
	finish := sync.OnceFunc(func() { close(complete) })
	defer finish()
	allowCreated := make(chan struct{}, 1)
	defer close(allowCreated)
	receivedCreate := make(chan struct{}, 1)
	fixture := newResponsesWSBillingTest(t, `tier("base", p * 2)`, func(ws *websocket.Conn, _ *http.Request) {
		_, create, err := ws.ReadMessage()
		if !assert.NoError(t, err) || !assert.Contains(t, string(create), `"type":"response.create"`) {
			return
		}
		receivedCreate <- struct{}{}
		<-allowCreated
		if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"active","status":"in_progress"}}`))) {
			return
		}
		_, cancel, err := ws.ReadMessage()
		if !assert.NoError(t, err) || !assert.JSONEq(t, `{"type":"response.cancel","response_id":"wrong"}`, string(cancel)) {
			return
		}
		if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"response_not_found","message":"No such response"}}`))) {
			return
		}
		<-complete
		if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"active","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}`))) {
			return
		}
		_, _, _ = ws.ReadMessage()
	})
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"hi"}`)))
	select {
	case <-receivedCreate:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not receive the initial request")
	}
	// Queue cancellation before the upstream accepts the response. The following
	// conflict is an acknowledgement that the client loop processed both frames.
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.cancel","response_id":"wrong"}`)))
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"before-acceptance"}`)))
	assert.Equal(t, float64(http.StatusConflict), readResponsesWSTestEvent(t, fixture.client)["status"])
	allowCreated <- struct{}{}
	assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
	cancelError := readResponsesWSTestEvent(t, fixture.client)
	assert.Equal(t, "error", cancelError["type"])
	assert.Equal(t, float64(http.StatusBadRequest), cancelError["status"])
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"overlapping","stream_id":"other","event_id":"busy"}`)))
	conflict := readResponsesWSTestEvent(t, fixture.client)
	assert.Equal(t, "error", conflict["type"])
	assert.Equal(t, float64(http.StatusConflict), conflict["status"])
	assert.Equal(t, "other", conflict["stream_id"])
	assert.Equal(t, "busy", conflict["event_id"])
	finish()
	assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
	fixture.closeAndWait(t)
	assertResponsesWSAccounting(t, fixture, []int64{2_000_000})
}

func TestResponsesWebSocketInitialUpstreamRejectionRefundsReservation(t *testing.T) {
	for _, tc := range []struct {
		name, upstream, wantMessage string
		status                      int
	}{
		{name: "structured error", upstream: `{"type":"error","response_id":"rejected","status":400,"error":{"type":"invalid_request_error","code":"invalid_input","message":"Invalid input"}}`, status: http.StatusBadRequest, wantMessage: "Invalid input"},
		// A frame without an error object still fails and releases its reservation.
		{name: "bare error", upstream: `{"type":"error","status":500,"message":"upstream rejected"}`, status: http.StatusBadGateway, wantMessage: "upstream rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			preConsumed := make(chan money.Amount, 1)
			userID := make(chan int, 1)
			// Observe the wallet hold after the request reaches the Provider.
			fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
				if _, _, err := ws.ReadMessage(); !assert.NoError(t, err) {
					return
				}
				wallet, err := model.GetUserWallet(<-userID)
				if !assert.NoError(t, err) {
					return
				}
				preConsumed <- wallet.Balance
				if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(tc.upstream))) {
					return
				}
				_, _, _ = ws.ReadMessage()
			})
			userID <- fixture.user.Id
			require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"hi","max_output_tokens":10}`)))
			rejection := readResponsesWSTestEvent(t, fixture.client)
			assert.Equal(t, "error", rejection["type"])
			require.Equal(t, float64(tc.status), rejection["status"])
			rejectionError, _ := rejection["error"].(map[string]any)
			assert.Equal(t, "new_api_error", rejectionError["type"])
			assert.Equal(t, "upstream_error", rejectionError["code"])
			assert.Equal(t, "Upstream response failed", rejectionError["message"])
			assert.NotContains(t, rejectionError["message"], tc.wantMessage)
			select {
			case balance := <-preConsumed:
				assert.Equal(t, money.Amount{Nanos: 998_000_000, Currency: "USD"}, balance, "funds are reserved before contacting the Provider")
			case <-time.After(3 * time.Second):
				t.Fatal("Provider did not observe the wallet reservation")
			}
			fixture.closeAndWait(t)
			requests := assertResponsesWSAccounting(t, fixture, []int64{0})
			assert.Equal(t, "failed", requests[0].Status)
			assert.Equal(t, "none", requests[0].UsageSource)
		})
	}
}

func TestResponsesStreamOutcomesPreserveAccounting(t *testing.T) {
	for _, transport := range []string{"websocket", "http-sse"} {
		for _, tc := range []struct {
			name, expression, terminal string
			// sseTerminal is the flat error envelope used by the Responses SSE
			// protocol; the WebSocket protocol nests the error instead.
			sseTerminal string
			delta       bool
			failed      bool
			ignored     bool
		}{
			{name: "failed-null-fixed", expression: `tier("request", fixed(0.002))`, terminal: `{"type":"response.failed","response":{"id":"first","status":"failed","usage":null,"error":{"code":"server_error","message":"sensitive upstream detail"}}}`, failed: true},
			{name: "failed-missing-fixed", expression: `tier("request", fixed(0.002))`, terminal: `{"type":"response.failed","response":{"id":"first","status":"failed"}}`, failed: true},
			{name: "failed-actual-usage", expression: `tier("input", p * 2)`, terminal: `{"type":"response.failed","response":{"id":"first","status":"failed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}`, failed: true},
			{name: "failed-estimated-output", expression: `tier("output", c * 2000)`, terminal: `{"type":"response.failed","response":{"id":"first","status":"failed","usage":null}}`, delta: true, failed: true},
			{name: "error-after-created", expression: `tier("request", fixed(0.002))`, terminal: `{"type":"error","status":500,"error":{"type":"server_error","code":"server_error","message":"Internal server error"}}`, sseTerminal: `{"type":"error","code":"server_error","message":"Internal server error","param":null,"sequence_number":2}`, failed: true},
			{name: "business-error-after-created", expression: `tier("request", fixed(0.002))`, terminal: `{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Input too long"}}`, sseTerminal: `{"type":"error","code":"context_length_exceeded","message":"Input too long","param":null,"sequence_number":2}`, failed: true, ignored: true},
			{name: "completed-at-output-limit", expression: `tier("request", fixed(0.002))`, terminal: `{"type":"response.incomplete","response":{"id":"first","status":"incomplete","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":1000,"output_tokens":1,"total_tokens":1001}}}`},
			{name: "completed-zero-fixed", expression: `tier("request", fixed(0.002))`, terminal: `{"type":"response.completed","response":{"id":"first","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`},
		} {
			t.Run(transport+"/"+tc.name, func(t *testing.T) {
				events := []string{`{"type":"response.created","response":{"id":"first","status":"in_progress"}}`}
				if tc.delta {
					events = append(events, `{"type":"response.output_text.delta","delta":"hello"}`)
				}
				terminal := tc.terminal
				if transport == "http-sse" && tc.sseTerminal != "" {
					terminal = tc.sseTerminal
				}
				events = append(events, terminal)
				fixture := newResponsesWSBillingTest(t, tc.expression, func(ws *websocket.Conn, _ *http.Request) {
					for turn := 0; ; turn++ {
						_, request, err := ws.ReadMessage()
						if err != nil {
							return
						}
						var body map[string]any
						if !assert.NoError(t, common.Unmarshal(request, &body)) {
							return
						}
						assert.Equal(t, "planner", body["stream_id"])
						assert.Equal(t, "gpt-4o", body["model"])
						output := events
						if transport == "http-sse" || turn > 0 {
							output = []string{`{"type":"response.completed","stream_id":"planner","response":{"id":"second","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1000,"output_tokens":1,"total_tokens":1001}}}`}
						}
						for _, event := range output {
							if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(event))) {
								return
							}
						}
					}
				}, events...)
				setting.ModelRequestRateLimitEnabled = true
				setting.ModelRequestRateLimitDurationMinutes = 1
				setting.ModelRequestRateLimitSuccessCount = 1
				setting.ModelRequestRateLimitCount = 0
				if transport == "http-sse" {
					request, err := http.NewRequest(http.MethodPost, fixture.gatewayURL+"/v1/responses", strings.NewReader(`{"model":"ws-billing","input":"hi","stream":true,"max_output_tokens":1}`))
					require.NoError(t, err)
					request.Header.Set("Authorization", "Bearer sk-"+fixture.token.Key)
					request.Header.Set("Content-Type", "application/json")
					response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
					require.NoError(t, err)
					body, err := io.ReadAll(response.Body)
					require.NoError(t, err)
					require.NoError(t, response.Body.Close())
					if tc.failed && !tc.delta {
						assert.Equal(t, http.StatusBadGateway, response.StatusCode)
						assert.JSONEq(t, `{"error":{"message":"Upstream response failed","type":"gateway_error","param":"","code":"upstream_error"}}`, string(body))
					} else if tc.failed {
						assert.Equal(t, http.StatusOK, response.StatusCode)
						assert.Contains(t, string(body), events[0])
						assert.Contains(t, string(body), events[1])
						assert.Contains(t, string(body), `"type":"response.failed"`)
						assert.Contains(t, string(body), `"code":"upstream_error"`)
						assert.NotContains(t, string(body), "[DONE]")
					} else {
						assert.Equal(t, http.StatusOK, response.StatusCode)
						assert.Contains(t, string(body), terminal)
					}
					assert.NotContains(t, string(body), "sensitive upstream detail")
				} else {
					require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","stream_id":"planner","model":"ws-billing","input":"hi","max_output_tokens":1}`)))
					for _, expected := range events {
						actual, err := common.Marshal(readResponsesWSTestEvent(t, fixture.client))
						require.NoError(t, err)
						assert.JSONEq(t, expected, string(actual), "upstream frames must be delivered once and unchanged")
					}
				}
				charges := []int64{0}
				if tc.name == "failed-actual-usage" && transport == "websocket" || tc.name == "failed-estimated-output" || tc.name == "completed-zero-fixed" {
					charges[0] = 2_000_000
				}
				if tc.failed {
					// The first terminal must complete settlement and middleware before
					// admitting this immediately following request.
					require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","stream_id":"planner","model":"ws-billing","input":"next","max_output_tokens":1}`)))
					next := readResponsesWSTestEvent(t, fixture.client)
					assert.Equal(t, "response.completed", next["type"], "the next create after a failed request must be admitted: %v", next)
					charges = append(charges, 2_000_000)
				}
				require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","event_id":"limited","stream_id":"planner","model":"ws-billing","input":"limited"}`)))
				limited := readResponsesWSTestEvent(t, fixture.client)
				assert.Equal(t, float64(http.StatusTooManyRequests), limited["status"])
				assert.Equal(t, "planner", limited["stream_id"])
				assert.Equal(t, "limited", limited["event_id"])
				fixture.closeAndWait(t)
				requests := assertResponsesWSAccounting(t, fixture, charges)
				if tc.failed {
					status := "failed"
					if charges[0] > 0 {
						status = "partial"
					}
					assert.Equal(t, status, requests[0].Status)
					assert.Equal(t, "success", requests[1].Status)
				} else if tc.name == "completed-at-output-limit" {
					assert.Equal(t, "partial", requests[0].Status)
				} else {
					assert.Equal(t, "success", requests[0].Status)
					assert.Equal(t, "upstream", requests[0].UsageSource)
					assert.Zero(t, requests[0].Usage.PromptTokens)
					assert.Zero(t, requests[0].Usage.CompletionTokens)
				}
				if tc.delta {
					assert.Equal(t, "estimated", requests[0].UsageSource)
					input := int64(1)
					if transport == "http-sse" {
						input = 9
					}
					assert.Equal(t, input, requests[0].Usage.PromptTokens)
					assert.Equal(t, int64(1), requests[0].Usage.CompletionTokens)
				}
				if tc.name == "failed-actual-usage" {
					var attempt model.GatewayAttempt
					require.NoError(t, model.LOG_DB.Where("request_id = ?", requests[0].ID).First(&attempt).Error)
					assert.Equal(t, "upstream", attempt.UsageSource)
					assert.Equal(t, int64(1000), attempt.Usage.PromptTokens)
					assert.Equal(t, int64(10), attempt.Usage.CompletionTokens)
					assert.Equal(t, money.Amount{Nanos: 1_010_000, Currency: "USD"}, attempt.Cost)
					if transport == "http-sse" {
						assert.Equal(t, "none", requests[0].UsageSource)
					} else {
						assert.Equal(t, "estimated", requests[0].UsageSource)
						assert.Equal(t, int64(1), requests[0].Usage.CompletionTokens)
					}
				}
				expectedRequests, successes := len(charges), 1
				if tc.ignored {
					expectedRequests--
				}
				if tc.name == "completed-at-output-limit" {
					expectedRequests, successes = 0, 0
				}
				assertResponsesWSHealth(t, fixture, expectedRequests, successes)
			})
		}
	}
}

func TestResponsesHTTPHealthCountsFinalResult(t *testing.T) {
	for _, tc := range []struct {
		name, code            string
		firstStatus, attempts int
		success               bool
		ignored               bool
	}{
		{"business rejection", "context_length_exceeded", 400, 1, false, true},
		{"credentials rejected as 400", "invalid_api_key", 400, 1, false, false},
		{"retry succeeds", "server_error", 500, 2, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(*websocket.Conn, *http.Request) {})
			if tc.success {
				alternative := *fixture.channel
				alternative.ID, alternative.UpstreamModelName = 0, "fallback-upstream-model"
				require.NoError(t, model.DB.Create(&alternative).Error)
				price := *fixture.purchase
				price.ID, price.OwnerID, price.ChannelID, price.Channel = 0, alternative.ID, nil, nil
				require.NoError(t, model.CreatePriceVersion(t.Context(), &price, fixture.user.Id))
			}
			var attempts atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if attempts.Add(1) == 1 {
					w.WriteHeader(tc.firstStatus)
					_, _ = fmt.Fprintf(w, `{"error":{"type":"%s","code":"%s","message":"test rejection"}}`, tc.code, tc.code)
					return
				}
				_, _ = fmt.Fprint(w, `{"id":"completed","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1000,"output_tokens":1,"total_tokens":1001}}`)
			}))
			t.Cleanup(upstream.Close)
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("name = ?", "responses-ws-upstream").Update("base_url", upstream.URL).Error)
			request, err := http.NewRequest(http.MethodPost, fixture.gatewayURL+"/v1/responses", strings.NewReader(`{"model":"ws-billing","input":"hello"}`))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer sk-"+fixture.token.Key)
			request.Header.Set("Content-Type", "application/json")
			response, err := http.DefaultClient.Do(request)
			require.NoError(t, err)
			_, err = io.Copy(io.Discard, response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			select {
			case <-fixture.httpDone:
			case <-time.After(3 * time.Second):
				t.Fatal("HTTP request did not finish")
			}
			fixture.closeAndWait(t)
			assert.Equal(t, int64(tc.attempts), attempts.Load())
			if !tc.success {
				assert.Equal(t, tc.firstStatus, response.StatusCode)
			} else {
				assert.Equal(t, http.StatusOK, response.StatusCode)
			}
			if tc.ignored {
				assertResponsesWSHealth(t, fixture, 0, 0)
			} else {
				assertResponsesWSHealth(t, fixture, 1, 0)
			}
			var records []model.GatewayAttempt
			require.NoError(t, model.LOG_DB.Order("started_at, id").Find(&records).Error)
			require.Len(t, records, tc.attempts)
			classes := map[string]string{"context_length_exceeded": "context_length", "invalid_api_key": "credentials", "server_error": "upstream_unavailable"}
			assert.Equal(t, classes[tc.code], records[0].ErrorClass)
			var requests []model.GatewayRequest
			require.NoError(t, model.LOG_DB.Where("token_id = ?", fixture.token.Id).Find(&requests).Error)
			require.Len(t, requests, 1, "attempts share one logical request")
			if tc.success {
				assert.Equal(t, "success", requests[0].Status)
				assert.Equal(t, "success", records[len(records)-1].Status)
				snapshots, err := service.ChannelHealth.Snapshots(t.Context(), []int{records[len(records)-1].ChannelID})
				require.NoError(t, err)
				assert.Equal(t, 1, snapshots[records[len(records)-1].ChannelID].Samples)
				assert.Equal(t, float64(1), snapshots[records[len(records)-1].ChannelID].SuccessRate)
			}
		})
	}
}

func TestResponsesWebSocketLocalErrorsKeepStreamIdentity(t *testing.T) {
	fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(*websocket.Conn, *http.Request) {
		t.Error("local rejection reached upstream")
	})
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","event_id":"invalid","stream_id":"planner","model":"ws-billing","max_output_tokens":18446744073709551615}`)))
	invalid := readResponsesWSTestEvent(t, fixture.client)
	assert.Equal(t, float64(http.StatusBadRequest), invalid["status"])
	assert.Equal(t, "planner", invalid["stream_id"])
	assert.Equal(t, "invalid", invalid["event_id"])
	fixture.token.Status = common.TokenStatusDisabled
	require.NoError(t, fixture.token.SelectUpdate())
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","event_id":"revoked","stream_id":"planner","model":"ws-billing","input":"hi"}`)))
	revoked := readResponsesWSTestEvent(t, fixture.client)
	assert.Equal(t, float64(http.StatusUnauthorized), revoked["status"])
	assert.Equal(t, "planner", revoked["stream_id"])
	assert.Equal(t, "revoked", revoked["event_id"])
	fixture.closeAndWait(t)
	assertResponsesWSAccounting(t, fixture, nil)
}

func TestResponsesWebSocketAmbiguousControlErrorClosesAndSettlesOnce(t *testing.T) {
	const terminal = `{"type":"error","status":500,"error":{"type":"server_error","code":"server_error","message":"Internal server error"}}`
	fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
		if _, _, err := ws.ReadMessage(); !assert.NoError(t, err) {
			return
		}
		if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"active","status":"in_progress"}}`))) {
			return
		}
		_, cancel, err := ws.ReadMessage()
		if !assert.NoError(t, err) {
			return
		}
		assert.Contains(t, string(cancel), `"type":"response.cancel"`)
		if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(terminal))) {
			return
		}
		_, _, _ = ws.ReadMessage()
	})
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"hi"}`)))
	assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.cancel","response_id":"active"}`)))
	body, err := common.Marshal(readResponsesWSTestEvent(t, fixture.client))
	require.NoError(t, err)
	assert.JSONEq(t, terminal, string(body))
	_, _, err = fixture.client.ReadMessage()
	// A dropped socket would read as a synthetic 1006; 1011 is the gateway's frame.
	var closeErr *websocket.CloseError
	require.ErrorAs(t, err, &closeErr, "an unattributed error must close the connection instead of remaining busy")
	assert.Equal(t, websocket.CloseInternalServerErr, closeErr.Code)
	fixture.closeAndWait(t)
	requests := assertResponsesWSAccounting(t, fixture, []int64{0})
	assert.Equal(t, "failed", requests[0].Status)
}

// Every way a session ends sends the client a close frame whose code says
// why. A dropped socket would read as a synthetic 1006 close error, so a
// received 1000, 1001, 1008 or 1011 proves the gateway sent the frame.
func TestResponsesWebSocketCloseCodes(t *testing.T) {
	require.NoError(t, i18n.Init())
	const (
		created   = `{"type":"response.created","response":{"id":"active","status":"in_progress"}}`
		delta     = `{"type":"response.output_text.delta","delta":"hello"}`
		completed = `{"type":"response.completed","response":{"id":"active","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}`
	)
	for _, tc := range []struct {
		name   string
		events []string // upstream events after the create, all relayed
		// After the events, and after the client read them with waitClient,
		// upstream sends upstreamClose or drops the socket. Otherwise it reads
		// until the gateway closes the connection.
		upstreamClose    int
		drop, waitClient bool
		timeout          bool
		end              func(*testing.T, *responsesWSBillingTest)
		code             int
		reason           string
		upstreamCode     int // the close code upstream receives, when checked
	}{
		{name: "upstream closes after completion", events: []string{completed}, upstreamClose: websocket.CloseNormalClosure, code: websocket.CloseNormalClosure, reason: "upstream connection closed"},
		{name: "idle upstream goes away", events: []string{completed}, waitClient: true, upstreamClose: websocket.CloseGoingAway, code: websocket.CloseGoingAway, reason: "upstream connection closed"},
		{name: "idle upstream drops", events: []string{completed}, waitClient: true, drop: true, code: websocket.CloseInternalServerErr, reason: "upstream connection closed"},
		// A normal upstream close still truncates the active response.
		{name: "upstream closes mid-response", events: []string{created, delta}, upstreamClose: websocket.CloseNormalClosure, code: websocket.CloseInternalServerErr, reason: "upstream connection closed"},
		{name: "upstream drops mid-response", events: []string{created, delta}, drop: true, code: websocket.CloseInternalServerErr, reason: "upstream connection closed"},
		{name: "upstream response times out", events: []string{created}, timeout: true, code: websocket.CloseInternalServerErr, reason: "upstream response timed out", upstreamCode: websocket.CloseInternalServerErr},
		{name: "client closes", events: []string{completed}, end: func(t *testing.T, fixture *responsesWSBillingTest) {
			require.NoError(t, fixture.client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second)))
		}, code: websocket.CloseNormalClosure, upstreamCode: websocket.CloseNormalClosure},
		{name: "channel disabled", events: []string{completed}, end: func(t *testing.T, fixture *responsesWSBillingTest) {
			require.Equal(t, 1, service.CloseActiveWebSocketsForChannel(fixture.provider.Id, service.ChannelDisabledCloseReason))
		}, code: websocket.ClosePolicyViolation, reason: service.ChannelDisabledCloseReason, upstreamCode: websocket.ClosePolicyViolation},
		{name: "token revoked", events: []string{completed}, end: func(t *testing.T, fixture *responsesWSBillingTest) {
			fixture.token.Status = common.TokenStatusDisabled
			require.NoError(t, fixture.token.SelectUpdate())
			require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"again"}`)))
			assert.Equal(t, float64(http.StatusUnauthorized), readResponsesWSTestEvent(t, fixture.client)["status"])
		}, code: websocket.ClosePolicyViolation, upstreamCode: websocket.ClosePolicyViolation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientRead := make(chan struct{})
			releaseClientRead := sync.OnceFunc(func() { close(clientRead) })
			defer releaseClientRead()
			upstreamCodes := make(chan int, 2)
			fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
				defaultClose := ws.CloseHandler()
				ws.SetCloseHandler(func(code int, text string) error {
					upstreamCodes <- code
					return defaultClose(code, text)
				})
				if _, _, err := ws.ReadMessage(); !assert.NoError(t, err) {
					return
				}
				for _, event := range tc.events {
					if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(event))) {
						return
					}
				}
				if tc.waitClient {
					<-clientRead
				}
				if tc.drop {
					return
				}
				if tc.upstreamClose != 0 && !assert.NoError(t, ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(tc.upstreamClose, "upstream shutdown"), time.Now().Add(time.Second))) {
					return
				}
				for {
					if _, _, err := ws.ReadMessage(); err != nil {
						return
					}
				}
			})
			if tc.timeout {
				constant.StreamingTimeout = 1
			}
			require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"hi"}`)))
			for _, event := range tc.events {
				actual, err := common.Marshal(readResponsesWSTestEvent(t, fixture.client))
				require.NoError(t, err)
				assert.JSONEq(t, event, string(actual))
			}
			releaseClientRead()
			if tc.end != nil {
				tc.end(t, fixture)
			}
			require.NoError(t, fixture.client.SetReadDeadline(time.Now().Add(5*time.Second)))
			_, _, err := fixture.client.ReadMessage()
			var closeErr *websocket.CloseError
			require.ErrorAs(t, err, &closeErr)
			assert.Equal(t, tc.code, closeErr.Code)
			assert.Equal(t, tc.reason, closeErr.Text)
			fixture.closeAndWait(t)
			charge := int64(2_000_000)
			if tc.events[len(tc.events)-1] != completed {
				charge = 0
			}
			assertResponsesWSAccounting(t, fixture, []int64{charge})
			if tc.upstreamCode != 0 {
				require.Len(t, upstreamCodes, 1)
				assert.Equal(t, tc.upstreamCode, <-upstreamCodes)
			}
		})
	}
}

// A control event queued while another is still outstanding is rejected
// locally instead of reaching upstream twice.
func TestResponsesWebSocketRejectsSecondControlWhileOnePending(t *testing.T) {
	receivedCancel := make(chan struct{}, 1)
	release := make(chan struct{})
	releaseProvider := sync.OnceFunc(func() { close(release) })
	defer releaseProvider()
	fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
		if _, _, err := ws.ReadMessage(); !assert.NoError(t, err) {
			return
		}
		if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"active","status":"in_progress"}}`))) {
			return
		}
		_, cancel, err := ws.ReadMessage()
		if !assert.NoError(t, err) || !assert.Contains(t, string(cancel), `"event_id":"first-cancel"`) {
			return
		}
		receivedCancel <- struct{}{}
		<-release
		if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"active","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}`))) {
			return
		}
		_, _, _ = ws.ReadMessage()
	})
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"hi"}`)))
	assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.cancel","event_id":"first-cancel","response_id":"active"}`)))
	select {
	case <-receivedCancel:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not receive the first cancel")
	}
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.cancel","event_id":"second-cancel","response_id":"active"}`)))
	rejected := readResponsesWSTestEvent(t, fixture.client)
	assert.Equal(t, "error", rejected["type"])
	assert.Equal(t, float64(http.StatusBadRequest), rejected["status"])
	assert.Equal(t, "second-cancel", rejected["event_id"])
	rejectedError, _ := rejected["error"].(map[string]any)
	assert.Contains(t, rejectedError["message"], "already pending")
	releaseProvider()
	assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
	fixture.closeAndWait(t)
	assertResponsesWSAccounting(t, fixture, []int64{2_000_000})
}

// A control error resolves the outstanding control, so a corrected cancel is
// forwarded instead of being rejected as still pending.
func TestResponsesWebSocketAcceptsCancelAfterControlError(t *testing.T) {
	fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
		if _, _, err := ws.ReadMessage(); !assert.NoError(t, err) {
			return
		}
		if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"active","status":"in_progress"}}`))) {
			return
		}
		_, cancel, err := ws.ReadMessage()
		if !assert.NoError(t, err) || !assert.JSONEq(t, `{"type":"response.cancel","response_id":"wrong"}`, string(cancel)) {
			return
		}
		if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"response_not_found","message":"No such response"}}`))) {
			return
		}
		_, cancel, err = ws.ReadMessage()
		if !assert.NoError(t, err) || !assert.JSONEq(t, `{"type":"response.cancel","response_id":"active"}`, string(cancel)) {
			return
		}
		if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.cancelled","response":{"id":"active","status":"cancelled","usage":{"input_tokens":1000,"output_tokens":0,"total_tokens":1000}}}`))) {
			return
		}
		_, _, _ = ws.ReadMessage()
	})
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"hi"}`)))
	assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.cancel","response_id":"wrong"}`)))
	cancelError := readResponsesWSTestEvent(t, fixture.client)
	assert.Equal(t, "error", cancelError["type"])
	assert.Equal(t, float64(http.StatusBadRequest), cancelError["status"])
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.cancel","response_id":"active"}`)))
	assert.Equal(t, "response.cancelled", readResponsesWSTestEvent(t, fixture.client)["type"])
	fixture.closeAndWait(t)
	requests := assertResponsesWSAccounting(t, fixture, []int64{0})
	assert.Equal(t, "cancelled", requests[0].Status)
}

// A late error that names the previous response must not end the request
// that follows it on the same connection, whether that response completed or
// failed.
func TestResponsesWebSocketIgnoresLateErrorForPreviousResponse(t *testing.T) {
	for _, tc := range []struct {
		name, firstTerminal, firstTerminalType string
	}{
		{name: "after completed", firstTerminalType: "response.completed", firstTerminal: `{"type":"response.completed","response":{"id":"first","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}`},
		{name: "after error", firstTerminalType: "error", firstTerminal: `{"type":"error","response_id":"first","status":500,"error":{"type":"server_error","code":"server_error","message":"Internal server error"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
				for _, turn := range [][]string{
					{`{"type":"response.created","response":{"id":"first","status":"in_progress"}}`, tc.firstTerminal},
					{
						`{"type":"error","response_id":"first","status":500,"error":{"type":"server_error","code":"server_error","message":"late failure"}}`,
						`{"type":"response.created","response":{"id":"second","status":"in_progress"}}`,
						`{"type":"response.completed","response":{"id":"second","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}`,
					},
				} {
					if _, _, err := ws.ReadMessage(); !assert.NoError(t, err) {
						return
					}
					for _, event := range turn {
						if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(event))) {
							return
						}
					}
				}
				_, _, _ = ws.ReadMessage()
			})
			require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"first"}`)))
			assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
			assert.Equal(t, tc.firstTerminalType, readResponsesWSTestEvent(t, fixture.client)["type"])
			require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"second"}`)))
			created := readResponsesWSTestEvent(t, fixture.client)
			require.Equal(t, "response.created", created["type"], "the late error must not reach the client or end the request: %v", created)
			assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
			fixture.closeAndWait(t)
			charges := []int64{2_000_000, 2_000_000}
			if tc.firstTerminalType == "error" {
				charges[0] = 0
			}
			assertResponsesWSAccounting(t, fixture, charges)
		})
	}
}

// An error that names a different response than the active one is relayed to
// the client and leaves the active generation running.
func TestResponsesWebSocketForwardsErrorForOtherResponseWithoutEndingRequest(t *testing.T) {
	const strayError = `{"type":"error","response_id":"other","status":500,"error":{"type":"server_error","code":"server_error","message":"unrelated failure"}}`
	fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
		if _, _, err := ws.ReadMessage(); !assert.NoError(t, err) {
			return
		}
		for _, event := range []string{
			`{"type":"response.created","response":{"id":"active","status":"in_progress"}}`,
			strayError,
			`{"type":"response.completed","response":{"id":"active","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}`,
		} {
			if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(event))) {
				return
			}
		}
		_, _, _ = ws.ReadMessage()
	})
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"hi"}`)))
	assert.Equal(t, "response.created", readResponsesWSTestEvent(t, fixture.client)["type"])
	stray, err := common.Marshal(readResponsesWSTestEvent(t, fixture.client))
	require.NoError(t, err)
	assert.JSONEq(t, strayError, string(stray))
	assert.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
	fixture.closeAndWait(t)
	requests := assertResponsesWSAccounting(t, fixture, []int64{2_000_000})
	assert.Equal(t, "success", requests[0].Status)
	assert.Equal(t, "upstream", requests[0].UsageSource)
	assertResponsesWSHealth(t, fixture, 1, 1)
}

// An established gateway connection pins the Channel, Provider, credential and
// native model. Candidate preparation cannot bypass that identity or WS opt-in.
func TestSelectResponsesWSChannelHonorsPinsAndFilters(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*service.GatewaySelection, *relay.ResponsesWSCall)
	}{
		{name: "same identity"},
		{name: "different channel", change: func(s *service.GatewaySelection, _ *relay.ResponsesWSCall) { s.Channel.ID++ }},
		{name: "different provider", change: func(s *service.GatewaySelection, _ *relay.ResponsesWSCall) { s.Provider.Id++ }},
		{name: "different provider type", change: func(s *service.GatewaySelection, _ *relay.ResponsesWSCall) {
			s.Provider.Type = constant.ChannelTypeNewAPI
		}},
		{name: "different native model", change: func(s *service.GatewaySelection, _ *relay.ResponsesWSCall) { s.Channel.UpstreamModelName = "different" }},
		{name: "different protocol", change: func(s *service.GatewaySelection, _ *relay.ResponsesWSCall) { s.Channel.Protocol = protocol.OpenAIChat }},
		{name: "websocket disabled", change: func(s *service.GatewaySelection, _ *relay.ResponsesWSCall) {
			s.Provider.SetSetting(dto.ProviderSettings{})
		}},
		{name: "missing pinned credential", change: func(_ *service.GatewaySelection, call *relay.ResponsesWSCall) { call.LockedKeyIndex = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			baseURL := "http://upstream.example"
			provider := &model.Provider{Id: 20, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Key: "provider-key", BaseURL: &baseURL}
			provider.SetSetting(dto.ProviderSettings{ResponsesWebSocketEnabled: true})
			selection := &service.GatewaySelection{Provider: provider, Channel: model.Channel{ID: 10, ProviderID: 20, Model: model.Model{ModelName: "gpt-4o"}, Protocol: protocol.OpenAIResponses, UpstreamModelName: "native", ParameterOverrides: `{}`}}
			call := &relay.ResponsesWSCall{Request: dto.OpenAIResponsesRequest{Model: "public"}, Body: []byte(`{"model":"public","input":"hi"}`),
				LockedChannelID: 10, LockedProviderID: 20, LockedProviderType: constant.ChannelTypeOpenAI, LockedUpstreamModel: "native",
				Native: &relay.NativeWSLifecycle{Protocol: protocol.OpenAIResponses, TransportOnly: true, Prepare: func(c *gin.Context, info *relaycommon.RelayInfo) (*relaychannel.PreparedWssRequest, error) {
					return relay.PrepareNativeWebSocketRequest(c, info, protocol.OpenAIResponses)
				}}}
			if tc.change != nil {
				tc.change(selection, call)
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(call.Body)))
			prepared, err := prepareGatewayWebSocketRoute(c, selection, call, time.Now(), 1024)
			if tc.change != nil {
				assert.ErrorIs(t, err, service.ErrGatewayUnavailable)
				assert.Nil(t, prepared)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, prepared)
			assert.Equal(t, "native", prepared.Info.UpstreamModelName)
			assert.Equal(t, 20, common.GetContextKeyInt(prepared.Context, constant.ContextKeyChannelId))
			assert.Equal(t, "Bearer provider-key", prepared.Transport.Headers().Get("Authorization"))
		})
	}
}

// Public names and aliases share Offering identity authorization in HTTP and WS.
// Legacy model-name suffix heuristics never broaden that authorization.
func TestCheckResponsesWSModelAccessMatchesHTTPTokenLimits(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(fmt.Sprint(allowed), func(t *testing.T) {
			fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(*websocket.Conn, *http.Request) { t.Error("authorization probe reached Provider") })
			fixture.offering.Aliases = []string{"ws-alias"}
			require.NoError(t, model.SaveModelOffering(t.Context(), fixture.offering, "save"))
			var ids []int
			if allowed {
				ids = []int{fixture.offering.ID}
			}
			_, err := model.MutateKeyPermissions(t.Context(), fixture.token.Id, fixture.user.Id, 0, "replace", model.KeyPermissions{DefaultAllow: false, ExceptionIDs: ids}, nil)
			require.NoError(t, err)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			c.Request.Header.Set("Authorization", "Bearer sk-"+fixture.token.Key)
			runner := newResponsesWSRequestRunner(c)
			httpEngine := gin.New()
			httpEngine.POST("/v1/responses", middleware.BodyStorageCleanup(), middleware.GatewayTokenAuth(), middleware.ResolveTextOffering(), func(c *gin.Context) {
				if apiErr := authorizeWebSocketOffering(c); apiErr != nil {
					c.Status(apiErr.StatusCode)
				}
			})
			for _, name := range []string{"ws-billing", "ws-alias", "gpt-4o", "ws-billing-high", "ws-billing@thinking:on"} {
				body := fmt.Sprintf(`{"model":%q,"input":"hi"}`, name)
				apiErr := runner(httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)), "auth-probe", authorizeWebSocketOffering)
				wsStatus := http.StatusOK
				if apiErr != nil {
					wsStatus = apiErr.StatusCode
					assert.Equal(t, types.ErrorCode("model_not_found"), apiErr.GetErrorCode())
					assert.NotContains(t, apiErr.Error(), fixture.token.Key)
				}
				request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
				request.Header.Set("Authorization", "Bearer sk-"+fixture.token.Key)
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				httpEngine.ServeHTTP(response, request)
				want := http.StatusNotFound
				if allowed && (name == "ws-billing" || name == "ws-alias") {
					want = http.StatusOK
				}
				assert.Equal(t, want, wsStatus, "WS name %s", name)
				assert.Equal(t, want, response.Code, "HTTP name %s", name)
			}
			fixture.closeAndWait(t)
			assertResponsesWSAccounting(t, fixture, nil)
			assert.Equal(t, common.TokenStatusEnabled, fixture.token.Status)
			assertResponsesWSHealth(t, fixture, 0, 0)
		})
	}
}

func TestSelectResponsesWSChannelAcceptsNativeResponsesChannelTypes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		kind     int
		route    *dto.AdvancedCustomRoute
		rejected bool
	}{
		{name: "azure", kind: constant.ChannelTypeAzure},
		{name: "new api", kind: constant.ChannelTypeNewAPI},
		{name: "sub2api", kind: constant.ChannelTypeSub2API},
		{name: "advanced native", kind: constant.ChannelTypeAdvancedCustom, route: &dto.AdvancedCustomRoute{IncomingPath: "/v1/responses", UpstreamPath: "/v1/responses"}},
		{name: "advanced explicit none", kind: constant.ChannelTypeAdvancedCustom, route: &dto.AdvancedCustomRoute{IncomingPath: "/v1/responses", UpstreamPath: "/v1/responses", Converter: "none"}},
		{name: "advanced converted", kind: constant.ChannelTypeAdvancedCustom, route: &dto.AdvancedCustomRoute{IncomingPath: "/v1/responses", UpstreamPath: "/v1/chat/completions", Converter: "openai_responses_to_openai_chat_completions"}, rejected: true},
		{name: "advanced missing responses", kind: constant.ChannelTypeAdvancedCustom, route: &dto.AdvancedCustomRoute{IncomingPath: "/v1/chat/completions", UpstreamPath: "/v1/chat/completions"}, rejected: true},
		{name: "anthropic", kind: constant.ChannelTypeAnthropic, rejected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, r *http.Request) {
				_, raw, err := ws.ReadMessage()
				if !assert.NoError(t, err) {
					return
				}
				assert.JSONEq(t, `{"type":"response.create","model":"gpt-4o","input":"hi"}`, string(raw))
				if tc.kind == constant.ChannelTypeAzure {
					assert.Equal(t, "upstream-first", r.Header.Get("api-key"))
					assert.Empty(t, r.Header.Get("Authorization"))
				} else {
					assert.Equal(t, "Bearer upstream-first", r.Header.Get("Authorization"))
				}
				if !assert.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"native","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}`))) {
					return
				}
				_, _, _ = ws.ReadMessage()
			})
			other := dto.ProviderOtherSettings{}
			if tc.route != nil {
				other.AdvancedCustom = &dto.AdvancedCustomConfig{Routes: []dto.AdvancedCustomRoute{*tc.route}}
			}
			fixture.provider.SetOtherSettings(other)
			require.NoError(t, model.DB.Model(fixture.provider).Updates(map[string]any{"type": tc.kind, "settings": fixture.provider.OtherSettings}).Error)
			require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"hi"}`)))
			event := readResponsesWSTestEvent(t, fixture.client)
			if tc.rejected {
				assert.Equal(t, "error", event["type"])
				assert.Equal(t, float64(http.StatusServiceUnavailable), event["status"])
				assert.Zero(t, fixture.connections.Load())
				fixture.closeAndWait(t)
				assertResponsesWSAccounting(t, fixture, nil)
				assertResponsesWSHealth(t, fixture, 0, 0)
				return
			}
			require.Equal(t, "response.completed", event["type"])
			fixture.closeAndWait(t)
			assert.Equal(t, int32(1), fixture.connections.Load())
			assertResponsesWSAccounting(t, fixture, []int64{2_000_000})
			assertResponsesWSHealth(t, fixture, 1, 1)
		})
	}
}

func TestResponsesWSChannelRoutingRequiresExplicitOptIn(t *testing.T) {
	var received atomic.Int32
	fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
			received.Add(1)
			if err := ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"opted-in","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}`)); err != nil {
				return
			}
		}
	})
	// A cheaper HTTP-only Provider belongs to the same Offering but cannot serve WS.
	other := &model.Provider{Name: "http-only", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Key: "http-key", BaseURL: fixture.provider.BaseURL}
	require.NoError(t, model.DB.Create(other).Error)
	alternative := *fixture.channel
	alternative.ID, alternative.ProviderID = 0, other.Id
	require.NoError(t, model.DB.Create(&alternative).Error)
	price := &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: alternative.ID, Currency: "USD", Expression: `tier("purchase", p * 0.5 + c * 0.5)`, EffectiveAt: clock.Now().Unix() - 1}
	require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.user.Id))
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"hi"}`)))
	require.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
	fixture.waitForSettlements(t, 1)
	fixture.provider.SetSetting(dto.ProviderSettings{ResponsesWebSocketEnabled: false})
	require.NoError(t, model.DB.Model(fixture.provider).Update("setting", fixture.provider.Setting).Error)
	require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"again"}`)))
	rejected := readResponsesWSTestEvent(t, fixture.client)
	assert.Equal(t, "error", rejected["type"])
	assert.Equal(t, float64(http.StatusServiceUnavailable), rejected["status"])
	fixture.closeAndWait(t)
	assert.Equal(t, int32(1), received.Load())
	assert.Equal(t, int32(1), fixture.connections.Load())
	assertResponsesWSAccounting(t, fixture, []int64{2_000_000})
	assertResponsesWSHealth(t, fixture, 1, 1)
}

// A live socket cannot switch to an eligible fallback after losing its binding.
// An unbound connection can select that same fallback through the Offering.
func TestSelectResponsesWSChannelHonorsStrictSessionBinding(t *testing.T) {
	for _, tc := range []struct {
		name        string
		established bool
		invalidate  func(*testing.T, *responsesWSBillingTest)
	}{
		{name: "unbound can choose fallback", invalidate: func(t *testing.T, f *responsesWSBillingTest) {
			require.NoError(t, model.DB.Model(f.channel).Update("status", model.ChannelDisabled).Error)
		}},
		{name: "disabled bound channel", established: true, invalidate: func(t *testing.T, f *responsesWSBillingTest) {
			require.NoError(t, model.DB.Model(f.channel).Update("status", model.ChannelDisabled).Error)
		}},
		{name: "disabled bound provider", established: true, invalidate: func(t *testing.T, f *responsesWSBillingTest) {
			require.NoError(t, model.DB.Model(f.provider).Update("status", common.ChannelStatusManuallyDisabled).Error)
		}},
		{name: "rotated bound credential", established: true, invalidate: func(t *testing.T, f *responsesWSBillingTest) {
			require.NoError(t, model.DB.Model(f.provider).Update("key", "rotated-first\nrotated-second").Error)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var received atomic.Int32
			fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(ws *websocket.Conn, _ *http.Request) {
				for {
					if _, _, err := ws.ReadMessage(); err != nil {
						return
					}
					index := received.Add(1)
					terminal := fmt.Sprintf(`{"type":"response.completed","response":{"id":"bound-%d","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1000,"output_tokens":10,"total_tokens":1010}}}`, index)
					if err := ws.WriteMessage(websocket.TextMessage, []byte(terminal)); err != nil {
						return
					}
				}
			})
			if tc.established {
				require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"first"}`)))
				require.Equal(t, "response.completed", readResponsesWSTestEvent(t, fixture.client)["type"])
				fixture.waitForSettlements(t, 1)
			}
			fallback := &model.Provider{Name: "fallback", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Key: "fallback-key", BaseURL: fixture.provider.BaseURL}
			fallback.SetSetting(dto.ProviderSettings{ResponsesWebSocketEnabled: true})
			require.NoError(t, model.DB.Create(fallback).Error)
			alternative := *fixture.channel
			alternative.ID, alternative.ProviderID = 0, fallback.Id
			require.NoError(t, model.DB.Create(&alternative).Error)
			price := &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: alternative.ID, Currency: "USD", Expression: `tier("purchase", p + c)`, EffectiveAt: clock.Now().Unix() - 1}
			require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.user.Id))
			tc.invalidate(t, fixture)
			require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"next"}`)))
			event := readResponsesWSTestEvent(t, fixture.client)
			if tc.established {
				assert.Equal(t, "error", event["type"])
				assert.Equal(t, float64(http.StatusServiceUnavailable), event["status"])
			} else {
				require.Equal(t, "response.completed", event["type"])
				fixture.channel, fixture.provider, fixture.purchase = &alternative, fallback, price
			}
			fixture.closeAndWait(t)
			assert.Equal(t, int32(1), received.Load())
			assert.Equal(t, int32(1), fixture.connections.Load())
			assertResponsesWSAccounting(t, fixture, []int64{2_000_000})
			assertResponsesWSHealth(t, fixture, 1, 1)
			assert.Equal(t, common.TokenStatusEnabled, fixture.token.Status)
		})
	}
}

// Rejections before Provider contact leave Channel health and wallets unchanged,
// including an unavailable route and a Key's live ModelOffering policy.
func TestResponsesWebSocketPreRoutingRejectionsFollowHealthClassification(t *testing.T) {
	require.NoError(t, i18n.Init())
	for _, tc := range []struct {
		name   string
		reject func(*testing.T, *responsesWSBillingTest)
		status int
	}{
		{name: "no eligible channel", status: http.StatusServiceUnavailable, reject: func(t *testing.T, fixture *responsesWSBillingTest) {
			require.NoError(t, model.DB.Model(fixture.channel).Update("status", model.ChannelDisabled).Error)
		}},
		{name: "token model limit", status: http.StatusNotFound, reject: func(t *testing.T, fixture *responsesWSBillingTest) {
			_, err := model.MutateKeyPermissions(t.Context(), fixture.token.Id, fixture.user.Id, 0, "deny_all", model.KeyPermissions{}, nil)
			require.NoError(t, err)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newResponsesWSBillingTest(t, `tier("request", fixed(0.002))`, func(*websocket.Conn, *http.Request) {
				t.Error("rejected request reached upstream")
			})
			tc.reject(t, fixture)
			require.NoError(t, fixture.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"ws-billing","input":"hi"}`)))
			rejected := readResponsesWSTestEvent(t, fixture.client)
			assert.Equal(t, "error", rejected["type"])
			assert.Equal(t, float64(tc.status), rejected["status"])
			fixture.closeAndWait(t)
			assertResponsesWSAccounting(t, fixture, nil)
			assertResponsesWSHealth(t, fixture, 0, 0)
		})
	}
}
