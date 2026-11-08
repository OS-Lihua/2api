package router

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newAsyncTaskFixture(t *testing.T, upstream http.HandlerFunc) textGatewayFixture {
	t.Helper()
	previousLimit := constant.TaskQueryLimit
	constant.TaskQueryLimit = 100
	t.Cleanup(func() { constant.TaskQueryLimit = previousLimit })
	previous := jsplugin.DefaultRegistry
	jsplugin.DefaultRegistry = jsplugin.NewRegistry()
	t.Cleanup(func() { jsplugin.DefaultRegistry = previous })
	source, err := plugins.Source("sora")
	require.NoError(t, err)
	_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "sora"})
	require.NoError(t, err)
	fixture := newTextGatewayFixture(t, upstream)
	require.NoError(t, model.DB.AutoMigrate(&model.Task{}))
	require.NoError(t, model.DB.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Update("model_name", "sora-2").Error)
	require.NoError(t, model.DB.Model(fixture.channel).Updates(map[string]any{"protocol": "task:sora", "upstream_model_name": "sora-2"}).Error)
	for _, price := range []*model.PriceVersion{
		{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: `tier("video", u("seconds") * 0.1)`, EffectiveAt: clock.Now().Unix()},
		{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: `tier("video", u("seconds") * 0.05)`, EffectiveAt: clock.Now().Unix()},
	} {
		require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
	}
	SetTaskRouter(fixture.engine)
	previousFactory := service.GetTaskAdaptorFunc
	service.GetTaskAdaptorFunc = func(platform constant.TaskPlatform) service.TaskPollingAdaptor { return relay.GetTaskAdaptor(platform) }
	t.Cleanup(func() { service.GetTaskAdaptorFunc = previousFactory })
	return fixture
}

func TestAsyncTaskOfferingReservesWalletBeforeAcceptance(t *testing.T) {
	calls := 0
	fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"private-job","status":"completed","seconds":"2"}`))
			return
		}
		calls++
		assert.Equal(t, "/v1/videos", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"private-job","status":"queued"}`))
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks/sora", strings.NewReader(`{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
	response := httptest.NewRecorder()
	fixture.engine.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, 1, calls)
	var receipt map[string]any
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &receipt))
	taskID, ok := receipt["task_id"].(string)
	require.True(t, ok)
	read := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+taskID, nil)
	read.Header.Set("Authorization", "Bearer "+fixture.token.Key)
	readResponse := httptest.NewRecorder()
	fixture.engine.ServeHTTP(readResponse, read)
	require.Equal(t, 200, readResponse.Code, readResponse.Body.String())
	assert.NotContains(t, readResponse.Body.String(), "private-job")
	assert.NotContains(t, readResponse.Body.String(), "provider-secret")
	foreign := &model.User{Username: "task-foreign", Status: common.UserStatusEnabled, Role: common.RoleCommonUser, Group: "default", AffCode: "foreign", AuthVersion: 1}
	require.NoError(t, model.DB.Create(foreign).Error)
	foreignKey := &model.Token{UserId: foreign.Id, Key: "taskforeignkey", Status: common.TokenStatusEnabled, ExpiredTime: -1}
	require.NoError(t, model.DB.Create(foreignKey).Error)
	for _, tc := range []struct {
		key, id string
		status  int
	}{
		{foreignKey.Key, taskID, 404}, {foreignKey.Key, "task_missing", 404}, {"invalidkey", taskID, 401},
	} {
		read := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+tc.id, nil)
		read.Header.Set("Authorization", "Bearer "+tc.key)
		response := httptest.NewRecorder()
		fixture.engine.ServeHTTP(response, read)
		require.Equal(t, tc.status, response.Code, response.Body.String())
		assert.NotContains(t, response.Body.String(), "private-job")
	}
	for _, updates := range []map[string]any{{"status": common.TokenStatusDisabled}, {"status": common.TokenStatusEnabled, "expired_time": 1}} {
		require.NoError(t, model.DB.Model(foreignKey).Updates(updates).Error)
		read := httptest.NewRequest(http.MethodGet, "/v1/tasks/"+taskID, nil)
		read.Header.Set("Authorization", "Bearer "+foreignKey.Key)
		response := httptest.NewRecorder()
		fixture.engine.ServeHTTP(response, read)
		require.Equal(t, 401, response.Code, response.Body.String())
	}
	var pendingRecords []model.GatewayRequest
	pendingRecords, _, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
	require.NoError(t, err)
	require.Len(t, pendingRecords, 1)
	assert.Equal(t, "pending", pendingRecords[0].Status)
	assert.EqualValues(t, 4, pendingRecords[0].Usage.TaskFacts["seconds"])
	adminRecords := adminSecurityRequest(fixture.engine, fixture.adminPAT, http.MethodGet, "/api/admin/v1/requests?status=pending", "")
	require.Equal(t, 200, adminRecords.Code, adminRecords.Body.String())
	assert.Contains(t, adminRecords.Body.String(), `"task_facts"`)
	assert.NotContains(t, adminRecords.Body.String(), "provider-secret")
	assert.NotContains(t, adminRecords.Body.String(), "private-job")
	assert.Equal(t, 403, adminSecurityRequest(fixture.engine, fixture.userPAT, http.MethodGet, "/api/admin/v1/requests", "").Code)
	selfRecords := adminSecurityRequest(fixture.engine, fixture.userPAT, http.MethodGet, "/api/requests/self?status=pending", "")
	require.Equal(t, 200, selfRecords.Code, selfRecords.Body.String())
	assert.Contains(t, selfRecords.Body.String(), `"task_facts"`)
	var wallet model.Wallet
	require.NoError(t, model.DB.Where("account_type = ? AND owner_id = ?", model.LedgerAccountUserWallet, fixture.user.Id).Take(&wallet).Error)
	assert.EqualValues(t, 640000000, wallet.Balance.Nanos)
	var entries []model.LedgerEntry
	require.NoError(t, model.DB.Where("request_id <> ''").Find(&entries).Error)
	require.Len(t, entries, 1)
	assert.Equal(t, model.LedgerEntryPreConsume, entries[0].Type)
	journal := t.TempDir()
	t.Setenv("GATEWAY_SETTLEMENT_JOURNAL_DIR", "/dev/null/unwritable")
	for range 2 {
		service.RunTaskPollingOnce(t.Context(), nil)
	}
	t.Setenv("GATEWAY_SETTLEMENT_JOURNAL_DIR", journal)
	runAsyncTaskRecoveryProcess(t, false)
	var terminal model.Task
	require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Take(&terminal).Error)
	require.Equal(t, model.TaskStatus(model.TaskStatusSuccess), terminal.Status)
	require.NoError(t, service.SettleGatewayTask(t.Context(), &terminal))
	requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	assert.Equal(t, "success", requests[0].Status)
	assert.EqualValues(t, 180000000, requests[0].Receivable.Nanos)
	attempts, total, err := model.ListGatewayAttempts(t.Context(), requests[0].ID, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	assert.EqualValues(t, 100000000, attempts[0].Cost.Nanos)
	require.NoError(t, model.DB.Where("id = ?", wallet.Id).Take(&wallet).Error)
	assert.EqualValues(t, 820000000, wallet.Balance.Nanos)
}

func runAsyncTaskRecoveryProcess(t *testing.T, poll bool) {
	t.Helper()
	var schema string
	require.NoError(t, model.DB.Raw("SELECT current_schema()").Scan(&schema).Error)
	primaryURL, err := url.Parse(os.Getenv("TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	query := primaryURL.Query()
	query.Set("search_path", schema)
	query.Set("application_name", "async49-poller")
	primaryURL.RawQuery = query.Encode()
	logDSN := primaryURL.String()
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		var database string
		require.NoError(t, model.LOG_DB.Raw("SELECT currentDatabase()").Scan(&database).Error)
		logURL, err := url.Parse(os.Getenv("TEST_CLICKHOUSE_LOG_DSN"))
		require.NoError(t, err)
		logURL.Path = "/" + database
		logDSN = logURL.String()
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	command := exec.CommandContext(t.Context(), executable, "-test.run=^TestAsyncTaskRecoveryProcess$", "-test.count=1")
	command.Env = append(os.Environ(), "ASYNC_TASK_RECOVERY_PRIMARY_DSN="+primaryURL.String(), "ASYNC_TASK_RECOVERY_LOG_DSN="+logDSN, fmt.Sprintf("ASYNC_TASK_RECOVERY_POLL=%t", poll))
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func TestAsyncTaskRecoveryProcess(t *testing.T) {
	primaryDSN := os.Getenv("ASYNC_TASK_RECOVERY_PRIMARY_DSN")
	if primaryDSN == "" {
		t.Skip("invoked by the asynchronous task process-restart regression")
	}
	primary, _, err := model.OpenDSN("ASYNC_TASK_RECOVERY_PRIMARY_DSN", primaryDSN, false)
	require.NoError(t, err)
	logs, kind, err := model.OpenDSN("ASYNC_TASK_RECOVERY_LOG_DSN", os.Getenv("ASYNC_TASK_RECOVERY_LOG_DSN"), true)
	require.NoError(t, err)
	model.DB, model.LOG_DB = primary, logs
	common.SetLogDatabaseType(kind)
	require.NoError(t, model.InitSettlementCurrency())
	if os.Getenv("ASYNC_TASK_RECOVERY_POLL") == "true" {
		if _, found := jsplugin.DefaultRegistry.Get("sora"); !found {
			source, err := plugins.Source("sora")
			require.NoError(t, err)
			_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "sora"})
			require.NoError(t, err)
		}
		service.InitHttpClient()
		service.GetTaskAdaptorFunc = func(platform constant.TaskPlatform) service.TaskPollingAdaptor { return relay.GetTaskAdaptor(platform) }
		constant.TaskQueryLimit = 100
		service.RunTaskPollingOnce(t.Context(), nil)
		var terminal model.Task
		require.NoError(t, model.DB.Where("private_data->'gateway' IS NOT NULL").Take(&terminal).Error)
		require.Equal(t, model.TaskStatus(model.TaskStatusSuccess), terminal.Status)
		return
	}
	completed, err := service.RecoverGatewayTasks(t.Context(), 100)
	require.NoError(t, err)
	require.Equal(t, 1, completed)
}

func TestAsyncTaskPersistenceExcludesCredentialsAndRequestContent(t *testing.T) {
	fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"id":"private-job","status":"completed","seconds":"2"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"private-job","status":"queued"}`))
	})
	var provider model.Provider
	require.NoError(t, model.DB.First(&provider, fixture.channel.ProviderID).Error)
	baseURL := strings.Replace(provider.GetBaseURL(), "http://", "http://url-user-secret:url-pass-secret@", 1) + "?api_key=url-query-secret"
	require.NoError(t, model.DB.Model(&provider).Update("base_url", baseURL).Error)
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks/sora", strings.NewReader(`{"model":"gateway-alias","prompt":"request-content-secret","seconds":"4"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
	request.Header.Set("X-Fixture-Secret", "client-header-secret")
	response := httptest.NewRecorder()
	fixture.engine.ServeHTTP(response, request)
	require.Equal(t, 200, response.Code, response.Body.String())
	for _, stage := range []string{"accepted", "completed"} {
		t.Run(stage, func(t *testing.T) {
			if stage == "completed" {
				require.NoError(t, model.DB.Exec(`CREATE FUNCTION "reject_task_settlement"() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture settlement storage unavailable'; END $$`).Error)
				require.NoError(t, model.DB.Exec(`CREATE TRIGGER "reject_task_settlement" BEFORE UPDATE ON "gateway_billings" FOR EACH ROW WHEN (NEW."phase" = 'settlement_pending') EXECUTE FUNCTION "reject_task_settlement"()`).Error)
				service.RunTaskPollingOnce(t.Context(), nil)
				require.NoError(t, model.DB.Exec(`DROP TRIGGER "reject_task_settlement" ON "gateway_billings"`).Error)
				require.NoError(t, model.DB.Exec(`DROP FUNCTION "reject_task_settlement"()`).Error)
			}
			var task model.Task
			require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Take(&task).Error)
			assert.Empty(t, task.PrivateData.Key)
			if stage == "completed" {
				require.Equal(t, model.TaskStatus(model.TaskStatusSuccess), task.Status)
			}
			var privateData string
			require.NoError(t, model.DB.Model(&model.Task{}).Select("private_data").Where("user_id = ?", fixture.user.Id).Scan(&privateData).Error)
			var lifecycle []string
			require.NoError(t, model.DB.Model(&model.GatewayBilling{}).Select("to_jsonb(gateway_billings)::text").Scan(&lifecycle).Error)
			require.Len(t, lifecycle, 1)
			requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			attempts, total, err := model.ListGatewayAttempts(t.Context(), requests[0].ID, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			records, err := common.Marshal([]any{requests, attempts})
			require.NoError(t, err)
			stored := []string{privateData, lifecycle[0], string(records)}
			journalFiles := 0
			require.NoError(t, filepath.WalkDir(os.Getenv("GATEWAY_SETTLEMENT_JOURNAL_DIR"), func(path string, entry os.DirEntry, err error) error {
				if err != nil || entry.IsDir() {
					return err
				}
				data, err := os.ReadFile(path)
				journalFiles++
				stored = append(stored, string(data))
				return err
			}))
			if stage == "completed" {
				assert.Equal(t, 1, journalFiles)
			}
			for _, data := range stored {
				for _, secret := range []string{"provider-secret", "url-user-secret", "url-pass-secret", "url-query-secret", "request-content-secret", "client-header-secret", fixture.token.Key} {
					assert.False(t, strings.Contains(data, secret), "persisted state contains synthetic secret at %s", stage)
				}
			}
		})
	}
	_, err := service.RecoverGatewayTasks(t.Context(), 100)
	require.NoError(t, err)
}

func TestAsyncTaskTerminalAccountingKeepsAcceptedIdentityAndFrozenPrices(t *testing.T) {
	for _, tc := range []struct {
		name, status, seconds    string
		wallet, sale, cost, debt int64
	}{
		{"success", "completed", "2", 820000000, 180000000, 100000000, 0},
		{"failure", "failed", "2", 1000000000, 0, 100000000, 0},
		{"cancelled", "cancelled", "2", 1000000000, 0, 100000000, 0},
		{"shortfall", "completed", "20", 0, 1800000000, 1000000000, 800000000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			submits, polls := 0, 0
			fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					t.Error("poll must use the original Provider's current configuration")
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				assert.Equal(t, "Bearer provider-secret", r.Header.Get("Authorization"))
				submits++
				_, _ = w.Write([]byte(`{"id":"private-job","status":"queued"}`))
			})
			request := httptest.NewRequest(http.MethodPost, "/v1/tasks/sora", strings.NewReader(`{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
			response := httptest.NewRecorder()
			fixture.engine.ServeHTTP(response, request)
			require.Equal(t, 200, response.Code, response.Body.String())
			otherCalls := 0
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { otherCalls++; w.WriteHeader(500) }))
			t.Cleanup(other.Close)
			var originalProvider model.Provider
			require.NoError(t, model.DB.First(&originalProvider, fixture.channel.ProviderID).Error)
			rotated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				polls++
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "/v1/videos/private-job", r.URL.Path)
				assert.Equal(t, "Bearer rotated-provider-secret", r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"id":"private-job","status":%q,"seconds":%q}`, tc.status, tc.seconds)
			}))
			t.Cleanup(rotated.Close)
			require.NoError(t, model.DB.Model(&originalProvider).Updates(map[string]any{
				"base_url": rotated.URL,
				"key":      "disabled-provider-secret\nrotated-provider-secret",
				"channel_info": model.ProviderInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyStatusList: map[int]int{
					0: common.ChannelStatusAutoDisabled, 1: common.ChannelStatusEnabled,
				}},
			}).Error)
			replacement := &model.Provider{Name: "Different task account", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, BaseURL: &other.URL, Key: "different-account-key"}
			require.NoError(t, model.DB.Create(replacement).Error)
			require.NoError(t, model.DB.Model(fixture.channel).Updates(map[string]any{"status": model.ChannelDisabled, "upstream_model_name": "different-model", "provider_id": replacement.Id}).Error)
			fixture.fake.Advance(time.Second)
			for _, price := range []*model.PriceVersion{
				{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: `tier("changed", u("seconds") * 9)`, EffectiveAt: clock.Now().Unix()},
				{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: `tier("changed", u("seconds") * 8)`, EffectiveAt: clock.Now().Unix()},
			} {
				require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
			}
			service.RunTaskPollingOnce(t.Context(), nil)
			service.RunTaskPollingOnce(t.Context(), nil)
			_, err := service.RecoverGatewayTasks(t.Context(), 100)
			require.NoError(t, err)
			assert.Equal(t, 1, submits)
			assert.Equal(t, 1, polls)
			assert.Zero(t, otherCalls)
			requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.Equal(t, tc.sale, requests[0].Receivable.Nanos)
			attempts, total, err := model.ListGatewayAttempts(t.Context(), requests[0].ID, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.Equal(t, tc.cost, attempts[0].Cost.Nanos)
			assert.Equal(t, fixture.channel.ID, attempts[0].ChannelID)
			var wallet model.Wallet
			require.NoError(t, model.DB.Where("account_type = ? AND owner_id = ?", model.LedgerAccountUserWallet, fixture.user.Id).Take(&wallet).Error)
			assert.Equal(t, tc.wallet, wallet.Balance.Nanos)
			outstanding, err := model.GetUserOutstanding(fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, tc.debt, outstanding.Nanos)
			var charges int64
			require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where(&model.LedgerEntry{RequestId: requests[0].ID, Type: model.LedgerEntryPreConsume, AccountType: model.LedgerAccountUserWallet}).Count(&charges).Error)
			assert.EqualValues(t, 1, charges)
			if tc.debt > 0 {
				_, err := model.TopUpWallet(model.WalletTopUp{UserId: fixture.user.Id, Source: money.Amount{Nanos: 100000000, Currency: "USD"}, Reference: "partial-debt"})
				require.NoError(t, err)
				debt, err := model.GetUserOutstanding(fixture.user.Id)
				require.NoError(t, err)
				assert.EqualValues(t, 700000000, debt.Nanos)
				require.NoError(t, model.DB.Where("id = ?", wallet.Id).Take(&wallet).Error)
				assert.Zero(t, wallet.Balance.Nanos)
				assert.True(t, wallet.RequestsPaused)
			}
		})
	}
}

func TestAsyncTaskUnavailableOriginalProviderKeepsAcceptedFacts(t *testing.T) {
	for _, change := range []string{"type", "keys"} {
		t.Run(change, func(t *testing.T) {
			calls := 0
			fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"private-job","status":"queued"}`))
			})
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
			require.Equal(t, 200, response.Code, response.Body.String())
			updates := map[string]any{"type": constant.ChannelTypeAnthropic}
			if change == "keys" {
				updates = map[string]any{"channel_info": model.ProviderInfo{IsMultiKey: true, MultiKeySize: 1, MultiKeyStatusList: map[int]int{0: common.ChannelStatusAutoDisabled}}}
			}
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Updates(updates).Error)
			for range 2 {
				service.RunTaskPollingOnce(t.Context(), nil)
			}
			assert.Equal(t, 1, calls, "unavailable original Provider must not cause another submission or query")
			var task model.Task
			require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Take(&task).Error)
			assert.Equal(t, model.TaskStatus(model.TaskStatusSubmitted), task.Status)
			require.NotNil(t, task.PrivateData.Gateway)
			assert.True(t, task.PrivateData.Gateway.Accepted)
			assert.Equal(t, "private-job", task.GetUpstreamTaskID())
			assert.Empty(t, task.PrivateData.Key)
			var wallet model.Wallet
			require.NoError(t, model.DB.Where("account_type = ? AND owner_id = ?", model.LedgerAccountUserWallet, fixture.user.Id).Take(&wallet).Error)
			assert.EqualValues(t, 640000000, wallet.Balance.Nanos)
		})
	}
}

func TestAsyncTaskRemixRetainsOriginalProviderAfterChannelEdit(t *testing.T) {
	calls := 0
	fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			assert.Equal(t, "/v1/videos", r.URL.Path)
		} else {
			assert.Equal(t, "/v1/videos/private-job-1/remix", r.URL.Path)
		}
		_, _ = fmt.Fprintf(w, `{"id":"private-job-%d","status":"queued"}`, calls)
	})
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
	require.Equal(t, 200, response.Code, response.Body.String())
	var original model.Task
	require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Take(&original).Error)
	// Use the actual upstream ID supplied at acceptance, distinct from its public ID.
	require.Equal(t, "private-job-1", original.GetUpstreamTaskID())
	otherCalls := 0
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { otherCalls++; w.WriteHeader(500) }))
	t.Cleanup(other.Close)
	replacement := &model.Provider{Name: "Replacement task account", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Key: "replacement-secret", BaseURL: &other.URL}
	require.NoError(t, model.DB.Create(replacement).Error)
	require.NoError(t, model.DB.Model(fixture.channel).Updates(map[string]any{"provider_id": replacement.Id, "upstream_model_name": "different-model"}).Error)
	body := fmt.Sprintf(`{"model":"gateway-alias","prompt":"A green bird","action":"remix","origin_task_ids":[%q]}`, original.TaskID)
	response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", body)
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.Equal(t, 2, calls)
	assert.Zero(t, otherCalls)
	var tasks []model.Task
	require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Order("id").Find(&tasks).Error)
	require.Len(t, tasks, 2)
	assert.Equal(t, original.ChannelId, tasks[1].ChannelId)
	assert.Equal(t, original.PrivateData.Gateway.ProviderID, tasks[1].PrivateData.Gateway.ProviderID)
	assert.Equal(t, original.PrivateData.Gateway.ChannelID, tasks[1].PrivateData.Gateway.ChannelID)
	assert.Empty(t, tasks[1].PrivateData.Key)
}

func TestAsyncTaskHostProtocolsUseOfferingMoneyLifecycle(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"/v1/videos", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`},
		{"/v1/responses", `{"model":"gateway-alias","input":"A bird","background":true,"seconds":"4"}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			calls := 0
			fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, "/v1/videos", r.URL.Path)
				assert.Equal(t, "Bearer provider-secret", r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"private-job","status":"queued"}`))
			})
			SetTaskPluginProtocolRouter(fixture.engine)
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, tc.path, tc.body)
			require.Equal(t, 200, response.Code, response.Body.String())
			assert.Equal(t, 1, calls)
			assert.NotContains(t, response.Body.String(), "private-job")
			var task model.Task
			require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Take(&task).Error)
			require.NotNil(t, task.PrivateData.Gateway)
			assert.True(t, task.PrivateData.Gateway.Accepted)
			assert.Empty(t, task.PrivateData.Key)
			var wallet model.Wallet
			require.NoError(t, model.DB.Where("account_type = ? AND owner_id = ?", model.LedgerAccountUserWallet, fixture.user.Id).Take(&wallet).Error)
			assert.EqualValues(t, 640000000, wallet.Balance.Nanos)
		})
	}
}

func TestAsyncTaskNativeOfferingUsesDeclaredPluginWithIndependentProvider(t *testing.T) {
	calls := 0
	fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, "/api/v3/contents/generations/tasks", r.URL.Path)
		assert.Equal(t, "Bearer provider-secret", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"private-job"}`))
	})
	source, err := plugins.Source("doubao")
	require.NoError(t, err)
	_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "doubao"})
	require.NoError(t, err)
	require.NoError(t, model.DB.Model(fixture.channel).Updates(map[string]any{"protocol": "task:doubao", "upstream_model_name": "doubao-seedance-1-0-pro-250528"}).Error)
	fixture.fake.Advance(time.Second)
	for _, price := range []*model.PriceVersion{
		{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: `tier("video", u("tokens") * 0.1 / 1000000)`, EffectiveAt: clock.Now().Unix()},
		{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: `tier("video", u("tokens") * 0.05 / 1000000)`, EffectiveAt: clock.Now().Unix()},
	} {
		require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
	}
	builder := newPluginGenerationBuilder(fixture.engine.Routes(), nil, productionPluginRouteHandlers)
	require.NoError(t, jsplugin.DefaultRegistry.SetGenerationPreparer(builder.prepare))
	dispatcher := &pluginRouteDispatcher{registry: jsplugin.DefaultRegistry}
	fixture.engine.NoRoute(dispatcher.dispatch)
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/doubao/api/v3/contents/generations/tasks", `{"model":"gateway-alias","content":[{"type":"text","text":"A bird"}],"duration":4}`)
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.Equal(t, 1, calls)
	assert.NotContains(t, response.Body.String(), "private-job")
	var task model.Task
	require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Take(&task).Error)
	require.NotNil(t, task.PrivateData.Gateway)
	assert.True(t, task.PrivateData.Gateway.Accepted)
	assert.Equal(t, constant.TaskPlatform("doubao"), task.Platform)
	assert.Equal(t, "gateway-alias", task.Properties.OriginModelName)
	assert.Empty(t, task.PrivateData.Key)
}

func TestAsyncTaskFailoverAndConcurrentCompletionChargeOnce(t *testing.T) {
	firstCalls := 0
	fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
		firstCalls++
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"synthetic-upstream-error-secret"}}`))
	})
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			entered <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			_, _ = w.Write([]byte(`{"id":"second-job","status":"completed","seconds":"2"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"second-job","status":"queued"}`))
	}))
	t.Cleanup(second.Close)
	provider := &model.Provider{Name: "Failover task account", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Key: "second-provider-secret", BaseURL: &second.URL}
	require.NoError(t, model.DB.Create(provider).Error)
	channel := &model.Channel{ProviderID: provider.Id, ModelID: fixture.channel.ModelID, Protocol: "task:sora", UpstreamModelName: "sora-2", MappingConfirmed: true, Status: model.ChannelEnabled, Weight: 1, ParameterOverrides: `{}`}
	require.NoError(t, model.DB.Create(channel).Error)
	require.NoError(t, model.CreatePriceVersion(t.Context(), &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: channel.ID, Currency: "USD", Expression: `tier("video", u("seconds") * 0.05)`, EffectiveAt: clock.Now().Unix()}, fixture.sale.CreatedBy))
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.Equal(t, 1, firstCalls)
	var workers sync.WaitGroup
	workers.Go(func() { service.RunTaskPollingOnce(t.Context(), nil) })
	<-entered
	workers.Go(func() { service.RunTaskPollingOnce(t.Context(), nil) })
	<-entered
	close(release)
	workers.Wait()
	_, err := service.RecoverGatewayTasks(t.Context(), 100)
	require.NoError(t, err)
	requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	assert.EqualValues(t, 180000000, requests[0].Receivable.Nanos)
	attempts, total, err := model.ListGatewayAttempts(t.Context(), requests[0].ID, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 2, total)
	for _, attempt := range attempts {
		if attempt.ProviderID == provider.Id {
			assert.Equal(t, "stop", attempt.FailoverDecision)
			assert.Equal(t, "success", attempt.Status)
			assert.EqualValues(t, 100000000, attempt.Cost.Nanos)
		} else {
			assert.Equal(t, "switch", attempt.FailoverDecision)
			assert.Equal(t, "failed", attempt.Status)
			assert.Zero(t, attempt.Cost.Nanos)
		}
	}
	for _, kind := range []model.LedgerEntryType{model.LedgerEntryPreConsume, model.LedgerEntryPreConsumeRelease, model.LedgerEntryCharge} {
		var count int64
		require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("request_id = ? AND account_type = ? AND type = ?", requests[0].ID, model.LedgerAccountUserWallet, kind).Count(&count).Error)
		assert.EqualValues(t, 1, count, kind)
	}
}

func TestAsyncTaskFreezesSaleAndPurchaseFX(t *testing.T) {
	fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"id":"private-job","status":"completed","seconds":"2"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"private-job","status":"queued"}`))
	})
	_, err := model.SaveFXRate(t.Context(), "EUR", "USD", "2", "0", fixture.sale.CreatedBy)
	require.NoError(t, err)
	fixture.fake.Advance(time.Second)
	for _, price := range []*model.PriceVersion{
		{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "EUR", Expression: `tier("video", u("seconds") * 0.1)`, EffectiveAt: clock.Now().Unix()},
		{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "EUR", Expression: `tier("video", u("seconds") * 0.05)`, EffectiveAt: clock.Now().Unix()},
	} {
		require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
	}
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
	require.Equal(t, 200, response.Code, response.Body.String())
	_, err = model.SaveFXRate(t.Context(), "EUR", "USD", "3", "0", fixture.sale.CreatedBy)
	require.NoError(t, err)
	service.RunTaskPollingOnce(t.Context(), nil)
	requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	assert.EqualValues(t, 360000000, requests[0].Receivable.Nanos)
	assert.Equal(t, "2", requests[0].SaleFXRate)
	attempts, total, err := model.ListGatewayAttempts(t.Context(), requests[0].ID, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	assert.Equal(t, money.Currency("EUR"), attempts[0].Cost.Currency)
	assert.EqualValues(t, 100000000, attempts[0].Cost.Nanos)
	assert.EqualValues(t, 200000000, attempts[0].ConvertedCost.Nanos)
	assert.Equal(t, "2", attempts[0].AccountingFXRate)
}

func TestAsyncTaskPersistenceFailureRollsBackReservation(t *testing.T) {
	calls := 0
	fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) { calls++ })
	require.NoError(t, model.DB.Exec(`CREATE FUNCTION "reject_task_creation"() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture task storage unavailable'; END $$`).Error)
	require.NoError(t, model.DB.Exec(`CREATE TRIGGER "reject_task_creation" BEFORE INSERT ON "tasks" FOR EACH ROW EXECUTE FUNCTION "reject_task_creation"()`).Error)
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
	assert.GreaterOrEqual(t, response.Code, 500)
	assert.Zero(t, calls)
	for _, record := range []any{&model.Task{}, &model.GatewayBilling{}} {
		var count int64
		require.NoError(t, model.DB.Model(record).Count(&count).Error)
		assert.Zero(t, count)
	}
	var count int64
	require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("request_id <> ''").Count(&count).Error)
	assert.Zero(t, count, "a failed durable task reservation must roll back together")
	var wallet model.Wallet
	require.NoError(t, model.DB.Where("account_type = ? AND owner_id = ?", model.LedgerAccountUserWallet, fixture.user.Id).Take(&wallet).Error)
	assert.EqualValues(t, 1000000000, wallet.Balance.Nanos)
}

func TestAsyncTaskVideoRemixUsesOwnedOriginalTask(t *testing.T) {
	calls := 0
	fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 2 {
			assert.Equal(t, "/v1/videos/private-job-1/remix", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"private-job-%d","status":"queued"}`, calls)
	})
	SetVideoRouter(fixture.engine)
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
	require.Equal(t, 200, response.Code, response.Body.String())
	var original model.Task
	require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Take(&original).Error)
	response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/videos/"+original.TaskID+"/remix", `{"prompt":"A green bird"}`)
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"object":"video"`)
	assert.NotContains(t, response.Body.String(), "private-job")
	assert.Equal(t, 2, calls)
	foreign := &model.User{Username: "remix-foreign", Status: common.UserStatusEnabled, Role: common.RoleCommonUser, Group: "default", AffCode: "remix-foreign", AuthVersion: 1}
	require.NoError(t, model.DB.Create(foreign).Error)
	foreignKey := &model.Token{UserId: foreign.Id, Key: "foreignremixkey", Status: common.TokenStatusEnabled, ExpiredTime: -1}
	require.NoError(t, model.DB.Create(foreignKey).Error)
	for _, id := range []string{original.TaskID, "task_missing"} {
		response := adminSecurityRequest(fixture.engine, foreignKey.Key, http.MethodPost, "/v1/videos/"+id+"/remix", `{"prompt":"A green bird"}`)
		assert.Equal(t, 404, response.Code, response.Body.String())
	}
	for _, updates := range []map[string]any{{"status": common.TokenStatusDisabled}, {"status": common.TokenStatusEnabled, "expired_time": 1}} {
		require.NoError(t, model.DB.Model(foreignKey).Updates(updates).Error)
		response := adminSecurityRequest(fixture.engine, foreignKey.Key, http.MethodPost, "/v1/videos/"+original.TaskID+"/remix", `{"prompt":"A green bird"}`)
		assert.Equal(t, 401, response.Code, response.Body.String())
	}
	assert.Equal(t, 2, calls)
}

func TestAsyncTaskSubmissionCannotOverwriteConcurrentPollCompletion(t *testing.T) {
	fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"id":"private-job","status":"completed","seconds":"2"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"private-job","status":"queued"}`))
	})
	require.NoError(t, model.DB.Exec(`CREATE FUNCTION "coordinate_task_submission"() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF current_setting('application_name') <> 'async49-poller' AND EXISTS (SELECT 1 FROM "tasks" WHERE "status" = 'SUBMITTED' AND "private_data"->'gateway'->>'accepted' = 'true') THEN PERFORM pg_advisory_xact_lock(49, 6655); END IF; RETURN NULL; END $$`).Error)
	require.NoError(t, model.DB.Exec(`CREATE TRIGGER "coordinate_task_submission" BEFORE UPDATE ON "tasks" FOR EACH STATEMENT EXECUTE FUNCTION "coordinate_task_submission"()`).Error)
	database, err := model.DB.DB()
	require.NoError(t, err)
	connection, err := database.Conn(t.Context())
	require.NoError(t, err)
	defer connection.Close()
	_, err = connection.ExecContext(t.Context(), `SELECT pg_advisory_lock(49, 6655)`)
	require.NoError(t, err)
	defer func() { _, _ = connection.ExecContext(context.Background(), `SELECT pg_advisory_unlock(49, 6655)`) }()
	responseReady := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responseReady <- adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
	}()
	var response *httptest.ResponseRecorder
	polled := false
	for response == nil {
		select {
		case response = <-responseReady:
		default:
			var blocked int64
			require.NoError(t, model.DB.WithContext(t.Context()).Raw(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND classid = 49 AND objid = 6655 AND NOT granted`).Scan(&blocked).Error)
			if blocked > 0 {
				// The controller is stopped before acquiring the task row lock.
				// A separate polling process can commit its terminal facts first.
				runAsyncTaskRecoveryProcess(t, true)
				polled = true
				_, err := connection.ExecContext(t.Context(), `SELECT pg_advisory_unlock(49, 6655)`)
				require.NoError(t, err)
				response = <-responseReady
			}
		}
	}
	require.Equal(t, 200, response.Code, response.Body.String())
	if !polled {
		runAsyncTaskRecoveryProcess(t, true)
	}
	var task model.Task
	require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Take(&task).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), task.Status)
	assert.EqualValues(t, 2, task.PrivateData.Gateway.CompletionFacts["seconds"])
	requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	assert.Equal(t, "success", requests[0].Status)
	assert.EqualValues(t, 180000000, requests[0].Receivable.Nanos)
}

func TestAsyncTaskRejectsUnboundedDurationBeforeReservation(t *testing.T) {
	calls := 0
	fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"private-job","status":"queued"}`))
	})
	for _, seconds := range []string{`"4001"`, `0`, `-1`, `"NaN"`} {
		request := httptest.NewRequest(http.MethodPost, "/v1/tasks/sora", strings.NewReader(`{"model":"gateway-alias","prompt":"A bird","seconds":`+seconds+`}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
		response := httptest.NewRecorder()
		fixture.engine.ServeHTTP(response, request)
		assert.Equal(t, 400, response.Code, response.Body.String())
	}
	assert.Zero(t, calls)
	var count int64
	require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("request_id <> ''").Count(&count).Error)
	assert.Zero(t, count)
}
