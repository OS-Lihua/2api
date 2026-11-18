package controller

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/protocol"

	"github.com/QuantumNous/new-api/setting/operation_setting"

	"io"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/dto"
	kitdto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShouldRetryHonorsPinRetryMode(t *testing.T) {
	openaiErr := types.NewOpenAIError(errors.New("upstream"), types.ErrorCodeBadResponseStatusCode, http.StatusInternalServerError)

	c := newPinRetryContext()
	assert.True(t, service.ShouldRetryRelayError(c, openaiErr, 1))

	origin := newPinRetryContext()
	service.GetChannelConstraints(origin).AddPin(dto.ProviderPin{
		ChannelId: 2,
		Source:    dto.PinSourceOriginTask,
		Rank:      dto.PinRankOriginTask,
		RetryMode: dto.PinRetrySameChannel,
	})
	assert.True(t, service.ShouldRetryRelayError(origin, openaiErr, 1), "origin pin retries on the same channel")

	token := newPinRetryContext()
	service.GetChannelConstraints(token).AddPin(dto.ProviderPin{
		ChannelId: 1,
		Source:    dto.PinSourceToken,
		Rank:      dto.PinRankToken,
		RetryMode: dto.PinRetrySingleAttempt,
	})
	assert.Equal(t, service.PolicyDecision{Action: "stop", Reason: "pinned_channel", Source: "channel_constraint"}, service.DecideRelayRetry(token, openaiErr, 1), "token pin suppresses retry")
}

func TestShouldRetryTaskRelayHonorsPinRetryMode(t *testing.T) {
	taskErr := &dto.TaskError{StatusCode: http.StatusInternalServerError}

	c := newPinRetryContext()
	assert.Equal(t, "retry", decideTaskRetry(c, taskErr, 1).Action)

	origin := newPinRetryContext()
	service.GetChannelConstraints(origin).AddPin(dto.ProviderPin{
		ChannelId: 2,
		Source:    dto.PinSourceOriginTask,
		Rank:      dto.PinRankOriginTask,
		RetryMode: dto.PinRetrySameChannel,
	})
	assert.Equal(t, "retry", decideTaskRetry(origin, taskErr, 1).Action)

	token := newPinRetryContext()
	service.GetChannelConstraints(token).AddPin(dto.ProviderPin{
		ChannelId: 1,
		Source:    dto.PinSourceToken,
		Rank:      dto.PinRankToken,
		RetryMode: dto.PinRetrySingleAttempt,
	})
	assert.Equal(t, service.PolicyDecision{Action: "stop", Reason: "pinned_channel", Source: "channel_constraint"}, decideTaskRetry(token, taskErr, 1))
}

func TestSameChannelPinsMergeToStricterRetryMode(t *testing.T) {
	c := newPinRetryContext()
	constraints := service.GetChannelConstraints(c)
	constraints.AddPin(dto.ProviderPin{
		ChannelId: 7,
		Source:    dto.PinSourceOriginTask,
		Rank:      dto.PinRankOriginTask,
		RetryMode: dto.PinRetrySameChannel,
	})
	constraints.AddPin(dto.ProviderPin{
		ChannelId: 7,
		Source:    dto.PinSourceToken,
		Rank:      dto.PinRankToken,
		RetryMode: dto.PinRetrySingleAttempt,
	})
	pin, found, overridden := constraints.ResolvedPin()
	require.True(t, found)
	assert.Equal(t, 7, pin.ChannelId)
	assert.Equal(t, dto.PinRetrySingleAttempt, pin.RetryMode)
	assert.Empty(t, overridden)
	assert.False(t, service.ShouldRetryRelayError(c, types.NewOpenAIError(errors.New("upstream"), types.ErrorCodeBadResponseStatusCode, http.StatusInternalServerError), 1))
}

func newPinRetryContext() *gin.Context {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return c
}

func TestRequestPolicyConfigReturnsSettingsWithoutMigration(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	GetRequestPolicy(ctx)
	assert.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.True(t, response.Success)
	assert.Contains(t, response.Data, "options")
	assert.NotContains(t, response.Data, "migration")
	assert.NotContains(t, response.Data, "differences")
}

func TestLegacyMonitorSettingsAreRetired(t *testing.T) {
	db := modelManagementDB(t)
	retired := map[string]string{
		"monitor_setting.auto_test_channel_enabled": "true",
		"monitor_setting.auto_test_channel_minutes": "15",
		"monitor_setting.channel_test_mode":         "scheduled_all",
		"monitor_setting.channel_test_concurrency":  "2",
	}
	previousPolicy := maps.Clone(model.CurrentRequestPolicy().Options)
	for key := range retired {
		delete(previousPolicy, key)
	}
	monitor := operation_setting.GetMonitorSetting()
	previousMonitor := *monitor
	t.Cleanup(func() {
		require.NoError(t, model.UpdateRequestPolicyOptions(previousPolicy))
		*monitor = previousMonitor
	})
	for key, value := range retired {
		require.NoError(t, db.Create(&model.Option{Key: key, Value: value}).Error)
		common.OptionMapRWMutex.Lock()
		common.OptionMap[key] = value
		common.OptionMapRWMutex.Unlock()
	}
	require.NoError(t, model.UpdateOption("RetryTimes", "2"))
	var before []model.Option
	require.NoError(t, db.Order("key").Find(&before).Error)
	common.OptionMapRWMutex.RLock()
	beforeOptions := maps.Clone(common.OptionMap)
	common.OptionMapRWMutex.RUnlock()
	beforeMonitor := *monitor
	beforePolicy := model.CurrentRequestPolicy()

	replacements := map[string]string{
		"monitor_setting.auto_test_channel_enabled": "false",
		"monitor_setting.auto_test_channel_minutes": "20",
		"monitor_setting.channel_test_mode":         "auto_ban_only",
		"monitor_setting.channel_test_concurrency":  "3",
	}
	for key, value := range replacements {
		for _, endpoint := range []struct {
			name, method, path string
			handler            gin.HandlerFunc
			body               any
		}{
			{"single", http.MethodPut, "/api/option/", UpdateOption, gin.H{"key": key, "value": value}},
			{"policy", http.MethodPatch, "/api/option/request_policy", UpdateRequestPolicy, gin.H{"options": map[string]string{key: value, "RetryTimes": "8"}}},
		} {
			t.Run(key+"/"+endpoint.name, func(t *testing.T) {
				var result map[string]any
				response := modelManagementRequest(t, endpoint.handler, endpoint.method, endpoint.path, endpoint.body, &result)
				require.Equal(t, http.StatusGone, response.Code)
				assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
				assert.Equal(t, false, result["success"])
				assert.Equal(t, "legacy_channel_management_retired", result["code"])
				assert.Equal(t, "Legacy Channel management has been retired. Use the Provider and Channel APIs instead.", result["message_key"])
				var after []model.Option
				require.NoError(t, db.Order("key").Find(&after).Error)
				assert.Equal(t, before, after, "mixed policy updates must not partially commit")
				common.OptionMapRWMutex.RLock()
				afterOptions := maps.Clone(common.OptionMap)
				common.OptionMapRWMutex.RUnlock()
				assert.Equal(t, beforeOptions, afterOptions)
				assert.Equal(t, beforeMonitor, *monitor)
				assert.Same(t, beforePolicy, model.CurrentRequestPolicy())
			})
		}
	}

	t.Run("internal_writes_cannot_restore_retired_settings", func(t *testing.T) {
		for key, value := range replacements {
			assert.False(t, model.IsRequestPolicyOption(key))
			assert.Error(t, model.UpdateOption(key, value))
			assert.Error(t, model.UpdateRequestPolicyOptions(map[string]string{key: value, "RetryTimes": "8"}))
			assert.Error(t, model.UpdateOptionsBulk(map[string]string{key: value, "RetryTimes": "8"}))
		}
		var after []model.Option
		require.NoError(t, db.Order("key").Find(&after).Error)
		assert.Equal(t, before, after)
		assert.Same(t, beforePolicy, model.CurrentRequestPolicy())
		assert.Equal(t, beforeMonitor, *monitor)
	})

	t.Run("legacy_read_values_do_not_poison_active_policy", func(t *testing.T) {
		legacy := maps.Clone(retired)
		for key := range legacy {
			legacy[key] = "malformed legacy value"
		}
		snapshot, err := model.BuildRequestPolicy(legacy)
		require.NoError(t, err)
		for key := range legacy {
			assert.NotContains(t, snapshot.Options, key)
		}
	})

	t.Run("strict_policy_payload_rejects_ambiguity_without_writes", func(t *testing.T) {
		for _, body := range []string{
			`null`,
			`{}`,
			`{"options":null}`,
			`{"options":{"RetryTimes":"3","RetryTimes":"8"}}`,
			`{"options":{},"options":{"RetryTimes":"8"}}`,
			`{"options":{"RetryTimes":"8"},"unknown":true}`,
			`{"options":{"RetryTimes":"8"}} {}`,
		} {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPatch, "/api/option/request_policy", strings.NewReader(body))
			UpdateRequestPolicy(ctx)
			assert.Equal(t, http.StatusBadRequest, recorder.Code, body)
			assert.Same(t, beforePolicy, model.CurrentRequestPolicy())
		}
		var after []model.Option
		require.NoError(t, db.Order("key").Find(&after).Error)
		assert.Equal(t, before, after)
	})

	t.Run("public_read_models_hide_retired_settings", func(t *testing.T) {
		var all struct {
			Success bool           `json:"success"`
			Data    []model.Option `json:"data"`
		}
		response := modelManagementRequest(t, GetOptions, http.MethodGet, "/api/option/", nil, &all)
		require.Equal(t, http.StatusOK, response.Code)
		require.True(t, all.Success)
		for _, option := range all.Data {
			assert.NotContains(t, retired, option.Key)
		}
		var policy struct {
			Success bool `json:"success"`
			Data    struct {
				Options map[string]string `json:"options"`
			} `json:"data"`
		}
		response = modelManagementRequest(t, GetRequestPolicy, http.MethodGet, "/api/option/request_policy", nil, &policy)
		require.Equal(t, http.StatusOK, response.Code)
		require.True(t, policy.Success)
		assert.Equal(t, "2", policy.Data.Options["RetryTimes"])
		for key := range retired {
			assert.NotContains(t, policy.Data.Options, key)
		}
	})

	t.Run("active_policy_updates_still_save_atomically", func(t *testing.T) {
		var result struct {
			Success bool `json:"success"`
			Data    struct {
				Options map[string]string `json:"options"`
			} `json:"data"`
		}
		response := modelManagementRequest(t, UpdateRequestPolicy, http.MethodPatch, "/api/option/request_policy", gin.H{
			"options": map[string]string{"RetryTimes": "3", "AutomaticDisableChannelEnabled": "false"},
		}, &result)
		require.Equal(t, http.StatusOK, response.Code)
		require.True(t, result.Success)
		assert.Equal(t, "3", result.Data.Options["RetryTimes"])
		assert.Equal(t, "false", result.Data.Options["AutomaticDisableChannelEnabled"])
		assert.Equal(t, 3, common.RetryTimes)
		for key, value := range retired {
			assert.NotContains(t, result.Data.Options, key)
			var option model.Option
			require.NoError(t, db.Where("key = ?", key).First(&option).Error)
			assert.Equal(t, value, option.Value, "retired values remain historical; they are not rewritten")
		}
	})
}

func TestChannelAffinityRetryPriorities(t *testing.T) {
	affinity := operation_setting.GetChannelAffinitySetting()
	previous := *affinity
	t.Cleanup(func() { *affinity = previous; service.ClearChannelAffinityCacheAll() })
	for _, cached := range []bool{false, true} {
		for _, tc := range []struct {
			name, mode   string
			binding, max int
			auto         []string
			cross, empty bool
			want         []int
		}{
			{name: "ordinary routing", mode: "off", binding: -1, max: 3, want: []int{24, 27, 30}},
			{name: "affinity miss", mode: "prefer", binding: -1, max: 3, want: []int{24, 27, 30}},
			{name: "affinity hit", mode: "prefer", binding: 1, max: 3, want: []int{27, 24, 30}},
			{name: "no retry budget", mode: "prefer", binding: 1, max: 1, want: []int{27}},
			{name: "one retry", mode: "prefer", binding: 1, max: 2, want: []int{27, 24}},
			{name: "strict session", mode: "strict", binding: 1, max: 3, want: []int{27}},
			{name: "auto group", mode: "prefer", binding: 1, max: 2, auto: []string{"default", "vip"}, want: []int{27, 24}},
			{name: "cross group", mode: "prefer", binding: 1, max: 5, auto: []string{"default", "vip"}, cross: true, want: []int{27, 24, 30, 34, 37}},
			{name: "skip empty group", mode: "prefer", binding: 1, max: 3, auto: []string{"empty", "default"}, empty: true, want: []int{27, 24, 30}},
		} {
			t.Run(fmt.Sprintf("cache=%t/%s", cached, tc.name), func(t *testing.T) {
				attempts := []int{}
				fixture, _ := newCanonicalRelayFixture(t, protocol.AnthropicMessages, "affinity-retry-test", func(w http.ResponseWriter, r *http.Request) {
					var id int
					_, err := fmt.Sscanf(r.Header.Get("X-Api-Key"), "retry-key-%d", &id)
					require.NoError(t, err)
					attempts = append(attempts, id)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"temporary upstream failure"},"usage":{"input_tokens":1,"output_tokens":1}}`)
				})
				require.NoError(t, fixture.db.First(fixture.provider, fixture.provider.Id).Error)
				require.NoError(t, fixture.db.First(fixture.channel, fixture.channel.ID).Error)
				var base model.Model
				require.NoError(t, fixture.db.First(&base, fixture.channel.ModelID).Error)
				members := []model.ModelOfferingMember{}
				channels := []model.Channel{}
				for i, label := range []int{24, 27, 30, 34, 37} {
					provider := *fixture.provider
					channel := *fixture.channel
					if i > 0 {
						provider.Id = 0
						provider.Name = fmt.Sprintf("retry-provider-%d", label)
						require.NoError(t, fixture.db.Create(&provider).Error)
						channel.ID = 0
					}
					require.NoError(t, fixture.db.Model(&provider).Updates(map[string]any{"key": fmt.Sprintf("retry-key-%d", label), "group": "retired-unrelated", "models": "retired-unrelated"}).Error)
					if i > 0 {
						metadata := base
						metadata.Id = 0
						metadata.ModelName = fmt.Sprintf("retry-model-%d", label)
						require.NoError(t, fixture.db.Create(&metadata).Error)
						channel.ModelID = metadata.Id
						channel.ProviderID = provider.Id
						require.NoError(t, fixture.db.Create(&channel).Error)
					}
					if i > 0 {
						require.NoError(t, model.CreatePriceVersion(t.Context(), &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: channel.ID, Currency: "USD", Expression: `tier("fixed",fixed(0.01))`, EffectiveAt: time.Now().Unix()}, fixture.user.Id))
					}
					tier := 1
					if i >= 3 {
						tier = 2
					}
					if tc.empty {
						tier++
					}
					members = append(members, model.ModelOfferingMember{OfferingID: fixture.offering.ID, ModelID: channel.ModelID, Tier: tier})
					channels = append(channels, channel)
				}
				if tc.empty {
					metadata := base
					metadata.Id = 0
					metadata.ModelName = "empty-member"
					require.NoError(t, fixture.db.Create(&metadata).Error)
					members = append(members, model.ModelOfferingMember{OfferingID: fixture.offering.ID, ModelID: metadata.Id, Tier: 1})
				}
				require.NoError(t, fixture.db.Where("offering_id = ?", fixture.offering.ID).Delete(&model.ModelOfferingMember{}).Error)
				require.NoError(t, fixture.db.Create(&members).Error)
				fixture.offering.Members = members
				common.MemoryCacheEnabled = cached
				model.InitProviderCache()
				require.False(t, fixture.db.Migrator().HasTable("abilities"))
				common.OptionMapRWMutex.Lock()
				old, existed := common.OptionMap["GatewayMaxAttempts"]
				common.OptionMap["GatewayMaxAttempts"] = fmt.Sprint(tc.max)
				common.OptionMapRWMutex.Unlock()
				t.Cleanup(func() {
					common.OptionMapRWMutex.Lock()
					defer common.OptionMapRWMutex.Unlock()
					if existed {
						common.OptionMap["GatewayMaxAttempts"] = old
					} else {
						delete(common.OptionMap, "GatewayMaxAttempts")
					}
				})
				*affinity = operation_setting.ProviderAffinitySetting{Enabled: true, DefaultTTLSeconds: 60, MaxEntries: 100, Rules: []operation_setting.ProviderAffinityRule{{Name: t.Name(), ModelRegex: []string{"^native-task-offering$"}, SessionMode: tc.mode, KeySources: []operation_setting.ProviderAffinityKeySource{{Type: "request_header", Key: "X-Session"}}}}}
				service.ClearChannelAffinityCacheAll()
				if tc.binding >= 0 {
					seed := newPinRetryContext()
					seed.Set("id", fixture.user.Id)
					common.SetContextKey(seed, constant.ContextKeyUserGroup, "default")
					seed.Request.Header.Set("X-Session", t.Name())
					seedGatewayAffinity(t, seed, fixture, &channels[tc.binding])
					t.Cleanup(func() { service.ClearCurrentChannelAffinityCache(seed) })
				}
				engine := gin.New()
				engine.Use(middleware.RequestId(), middleware.GatewayTokenAuth(), middleware.ResolveTextOffering())
				engine.POST("/v1/messages", func(c *gin.Context) {
					c.Set(service.GatewayRouteRandomSource, func(uint64) uint64 { return 0 })
					common.SetContextKey(c, constant.ContextKeyTokenAutoGroups, tc.auto)
					common.SetContextKey(c, constant.ContextKeyTokenCrossGroupRetry, tc.cross)
					TextGateway(c)
				})
				request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"affinity-retry-test","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`))
				request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("X-Session", t.Name())
				response := httptest.NewRecorder()
				engine.ServeHTTP(response, request)
				assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
				assert.Equal(t, tc.want, attempts, "unique Channel attempts follow authorized member tiers, not old groups or repeated Provider priority")
				assert.LessOrEqual(t, len(attempts), tc.max, "tier transitions must not reset the global attempt budget")
				assertSubmissionWallet(t, fixture, 2_000_000_000)
				requestID := response.Header().Get(common.RequestIdKey)
				for _, kind := range []model.LedgerEntryType{model.LedgerEntryPreConsume, model.LedgerEntryPreConsumeRelease} {
					var count int64
					require.NoError(t, fixture.db.Model(&model.LedgerEntry{}).Where("request_id = ? AND type = ?", requestID, kind).Count(&count).Error)
					assert.EqualValues(t, 1, count)
				}
				_, err := model.SettleGateway(requestID)
				require.NoError(t, err)
				assertSubmissionWallet(t, fixture, 2_000_000_000)
				assert.NotContains(t, response.Body.String(), fixture.token.Key)
				for _, label := range []int{24, 27, 30, 34, 37} {
					assert.NotContains(t, response.Body.String(), fmt.Sprintf("retry-key-%d", label))
				}
			})
		}
	}
}

func TestRequestPolicyRoutingDatabase(t *testing.T) {
	affinity := operation_setting.GetChannelAffinitySetting()
	previousAffinity := *affinity
	t.Cleanup(func() { *affinity = previousAffinity; service.ClearChannelAffinityCacheAll() })
	for _, cached := range []bool{false, true} {
		for _, keep := range []bool{false, true} {
			for _, tc := range []struct {
				globalMode, ruleMode string
				blocked              bool
			}{
				{"strict", "inherit", true},
				{"prefer", "strict", true},
				{"prefer", "inherit", false},
				{"strict", "prefer", false},
			} {
				t.Run(fmt.Sprintf("cache=%t/keep=%t/global=%s/rule=%s", cached, keep, tc.globalMode, tc.ruleMode), func(t *testing.T) {
					var calls atomic.Int32
					fixture, _ := newCanonicalRelayFixture(t, protocol.AnthropicMessages, "policy-test", func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"id":"msg-policy","type":"message","role":"assistant","model":"policy-test","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
					})
					// A real, authorized second Channel can serve the request, but it
					// must never bypass an unavailable strict session binding.
					require.NoError(t, fixture.db.First(fixture.provider, fixture.provider.Id).Error)
					require.NoError(t, fixture.db.First(fixture.channel, fixture.channel.ID).Error)
					otherProvider := *fixture.provider
					otherProvider.Id, otherProvider.Name = 0, "policy alternate"
					require.NoError(t, fixture.db.Create(&otherProvider).Error)
					otherChannel := *fixture.channel
					otherChannel.ID, otherChannel.ProviderID = 0, otherProvider.Id
					require.NoError(t, fixture.db.Create(&otherChannel).Error)
					require.NoError(t, model.CreatePriceVersion(t.Context(), &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: otherChannel.ID, Currency: "USD", Expression: `tier("fixed",fixed(0.001))`, EffectiveAt: time.Now().Unix()}, fixture.user.Id))
					common.MemoryCacheEnabled = cached
					model.InitProviderCache()
					snapshot, err := model.BuildRequestPolicy(map[string]string{
						"channel_affinity_setting.enabled":                  "true",
						"channel_affinity_setting.session_mode":             tc.globalMode,
						"channel_affinity_setting.keep_on_channel_disabled": fmt.Sprint(keep),
						"channel_affinity_setting.rules":                    fmt.Sprintf(`[{"name":"session","model_regex":[".*"],"key_sources":[{"type":"request_header","key":"X-Session"}],"session_mode":%q}]`, tc.ruleMode),
					})
					require.NoError(t, err)
					*affinity = snapshot.Affinity
					service.ClearChannelAffinityCacheAll()
					seed := newPinRetryContext()
					seed.Set("id", fixture.user.Id)
					common.SetContextKey(seed, constant.ContextKeyUserGroup, "default")
					seed.Request.Header.Set("X-Session", t.Name())
					_, found := service.GetPreferredGatewayChannelByAffinity(seed, fixture.offering.ID, 1, fixture.offering.Name)
					require.False(t, found)
					seedGatewayAffinity(t, seed, fixture, fixture.channel)
					require.NoError(t, fixture.db.Model(fixture.provider).Update("status", common.ChannelStatusAutoDisabled).Error)
					model.InitProviderCache()
					t.Cleanup(func() { service.ClearCurrentChannelAffinityCache(seed) })
					bound, found := service.GetPreferredGatewayChannelByAffinity(seed, fixture.offering.ID, 1, fixture.offering.Name)
					require.True(t, found)
					require.Equal(t, fixture.channel.ID, bound)
					var events []service.PolicyEvent
					engine := gin.New()
					engine.Use(middleware.RequestId(), middleware.GatewayTokenAuth(), middleware.ResolveTextOffering())
					engine.POST("/v1/messages", func(c *gin.Context) {
						TextGateway(c)
						events = service.RequestPolicy(c).Events()
					})
					request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"policy-test","messages":[{"role":"user","content":"Hello"}],"max_tokens":16}`))
					request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set("X-Session", t.Name())
					response := httptest.NewRecorder()
					engine.ServeHTTP(response, request)
					assert.True(t, slices.ContainsFunc(events, func(event service.PolicyEvent) bool { return event.Decision.Reason == "session_rule_matched" }), "decision events are recorded for every request")
					bound, found = service.GetPreferredGatewayChannelByAffinity(seed, fixture.offering.ID, 1, fixture.offering.Name)
					var reservations int64
					require.NoError(t, fixture.db.Model(&model.GatewayBilling{}).Count(&reservations).Error)
					assert.NotContains(t, response.Body.String(), fixture.token.Key)
					assert.NotContains(t, response.Body.String(), fixture.provider.Key)
					if tc.blocked {
						assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
						assert.Zero(t, calls.Load(), "strict bindings fail before Provider I/O")
						assert.Zero(t, reservations, "strict rejection cannot create a funds hold")
						assertSubmissionWallet(t, fixture, 2_000_000_000)
						assert.Equal(t, keep, found, "binding retention is independent of strict request handling")
						if found {
							assert.Equal(t, fixture.channel.ID, bound)
						}
						return
					}
					assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
					assert.EqualValues(t, 1, calls.Load())
					assert.EqualValues(t, 1, reservations)
					assertSubmissionWallet(t, fixture, 1_990_000_000)
					require.True(t, found)
					assert.Equal(t, otherChannel.ID, bound, "a successful fallback rebinds the session")
					requestID := response.Header().Get(common.RequestIdKey)
					attempts, total, err := model.ListGatewayAttempts(t.Context(), requestID, 0, 10)
					require.NoError(t, err)
					require.EqualValues(t, 1, total)
					assert.Equal(t, otherChannel.ID, attempts[0].ChannelID)
					assert.EqualValues(t, 1_000_000, attempts[0].ConvertedCost.Nanos)
					for _, entryType := range []model.LedgerEntryType{model.LedgerEntryPreConsume, model.LedgerEntryPreConsumeRelease} {
						var count int64
						require.NoError(t, fixture.db.Model(&model.LedgerEntry{}).Where("request_id = ? AND type = ?", requestID, entryType).Count(&count).Error)
						assert.EqualValues(t, 1, count)
					}
					_, err = model.SettleGateway(requestID)
					require.NoError(t, err)
					assertSubmissionWallet(t, fixture, 1_990_000_000)
				})
			}
		}
	}
}

func seedGatewayAffinity(t *testing.T, c *gin.Context, fixture *nativeGatewayTaskFixture, channel *model.Channel) {
	t.Helper()
	sale, err := model.CurrentPriceVersion(t.Context(), fixture.db, model.PriceOwnerOffering, fixture.offering.ID, time.Now().Unix())
	require.NoError(t, err)
	selection, err := service.SelectGatewayRoute(c, fixture.offering, service.GatewayRouteInput{
		PinnedChannelID: channel.ID, PinnedProviderID: channel.ProviderID, Protocol: channel.Protocol,
		Sale: sale, Ratio: decimal.NewFromInt(1), StartedAt: time.Now(),
		Prepare: func(*service.GatewaySelection) (*service.GatewayRouteFacts, error) {
			return &service.GatewayRouteFacts{Usage: &kitdto.Usage{}}, nil
		},
	})
	require.NoError(t, err)
	require.Equal(t, channel.ID, selection.Channel.ID)
	service.RecordGatewayChannelAffinity(c, selection)
	require.NoError(t, selection.Permit.Release(t.Context(), false))
	require.NoError(t, service.ChannelHealth.Release(t.Context(), selection.Admission))
}
