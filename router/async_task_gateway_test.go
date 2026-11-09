package router

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/capability"
	"github.com/QuantumNous/new-api/pkg/channelhealth"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
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
	require.NoError(t, model.DB.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Updates(map[string]any{"model_name": "sora-2", "capabilities": capability.Set{Features: []capability.Feature{capability.Streaming}, InputModalities: []capability.Modality{capability.Text, capability.Image, capability.Video}, OutputModalities: []capability.Modality{capability.Video}}}).Error)
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

func TestAsyncTaskUsesOfferingIDPermissionsAcrossEntrypoints(t *testing.T) {
	for _, endpoint := range []struct{ name, path, body string }{
		{"task", "/v1/tasks/sora", `{"model":"$MODEL","prompt":"A bird","seconds":"4"}`},
		{"video", "/v1/videos", `{"model":"$MODEL","prompt":"A bird","seconds":"4"}`},
		{"responses", "/v1/responses", `{"model":"$MODEL","input":"A bird","background":true,"seconds":"4"}`},
		{"native", "/ali/api/v1/services/aigc/multimodal-generation/generation", `{"model":"$MODEL","input":{"messages":[{"role":"user","content":[{"text":"A bird"}]}]},"parameters":{"enable_interleave":true,"max_images":1}}`},
	} {
		t.Run(endpoint.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := func(w http.ResponseWriter, r *http.Request) {
				id := calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if endpoint.name == "native" {
					_, _ = io.WriteString(w, `{"output":{"choices":[{"message":{"content":[{"image":"https://artifact.invalid/bird.png"}]},"finish_reason":"stop"}]},"usage":{"image_count":1}}`)
					return
				}
				_, _ = fmt.Fprintf(w, `{"id":"permission-job-%d","status":"queued"}`, id)
			}
			var fixture textGatewayFixture
			if endpoint.name == "native" {
				fixture = newNativeImageTaskFixture(t, "alibaba", upstream)
			} else {
				fixture = newAsyncTaskFixture(t, upstream)
			}
			pat := createWalletTestAccessToken(t, fixture.user.Id, "api_key:read", "api_key:write")
			policy := fmt.Sprintf("/api/token/%d/permissions", fixture.token.Id)
			denied := adminSecurityRequest(fixture.engine, pat, http.MethodPatch, policy, `{"action":"deny_all","revision":0}`)
			require.Equal(t, http.StatusOK, denied.Code, denied.Body.String())
			var inaccessible map[string]any
			for _, name := range []string{fixture.offering.Name, "gateway-alias", "missing-task-offering"} {
				response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, endpoint.path, strings.ReplaceAll(endpoint.body, "$MODEL", name))
				require.Equal(t, http.StatusNotFound, response.Code, "%s: %s", name, response.Body.String())
				var payload map[string]any
				require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
				if endpoint.name == "native" {
					assert.Equal(t, "NotFound", payload["code"])
					delete(payload, "request_id")
				} else {
					assert.Contains(t, response.Body.String(), "model_not_found")
				}
				if inaccessible == nil {
					inaccessible = payload
				} else {
					assert.Equal(t, inaccessible, payload, "hidden and unknown Offering names must share their protocol error")
				}
			}
			assert.Zero(t, calls.Load())
			var reservations int64
			require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("type = ?", model.LedgerEntryPreConsume).Count(&reservations).Error)
			assert.Zero(t, reservations)
			opened := adminSecurityRequest(fixture.engine, pat, http.MethodPatch, policy, fmt.Sprintf(`{"action":"add","offering_ids":[%d],"revision":1}`, fixture.offering.ID))
			require.Equal(t, http.StatusOK, opened.Code, opened.Body.String())
			for _, name := range []string{fixture.offering.Name, "gateway-alias"} {
				response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, endpoint.path, strings.ReplaceAll(endpoint.body, "$MODEL", name))
				require.Equal(t, http.StatusOK, response.Code, "%s: %s", name, response.Body.String())
			}
			assert.EqualValues(t, 2, calls.Load())
			require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("type = ?", model.LedgerEntryPreConsume).Count(&reservations).Error)
			assert.EqualValues(t, 2, reservations)
			closed := adminSecurityRequest(fixture.engine, pat, http.MethodPatch, policy, `{"action":"deny_all","revision":2}`)
			require.Equal(t, http.StatusOK, closed.Code, closed.Body.String())
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, endpoint.path, strings.ReplaceAll(endpoint.body, "$MODEL", "gateway-alias"))
			require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
			assert.EqualValues(t, 2, calls.Load(), "live permission revocation must stop dispatch")
			replayed := adminSecurityRequest(fixture.engine, pat, http.MethodPatch, policy, `{"action":"deny_all","revision":2}`)
			require.Equal(t, http.StatusConflict, replayed.Code, replayed.Body.String())
			for _, credential := range []struct {
				updates map[string]any
				status  int
			}{
				{map[string]any{"status": common.TokenStatusDisabled}, http.StatusUnauthorized},
				{map[string]any{"expired_time": 1}, http.StatusUnauthorized},
				{map[string]any{"allow_ips": "203.0.113.0/24"}, http.StatusForbidden},
			} {
				require.NoError(t, model.DB.Model(fixture.token).Updates(map[string]any{"status": common.TokenStatusEnabled, "expired_time": -1, "allow_ips": ""}).Error)
				require.NoError(t, model.DB.Model(fixture.token).Updates(credential.updates).Error)
				response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, endpoint.path, strings.ReplaceAll(endpoint.body, "$MODEL", "gateway-alias"))
				require.Equal(t, credential.status, response.Code, response.Body.String())
				assert.EqualValues(t, 2, calls.Load())
			}
		})
	}
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
		exactPrice               bool
	}{
		{"success", "completed", "2", 820000000, 180000000, 100000000, 0, false},
		{"failure", "failed", "2", 1000000000, 0, 100000000, 0, false},
		{"explicit failed zero", "failed", "0", 1000000000, 0, 0, 0, false},
		{"cancelled", "cancelled", "2", 1000000000, 0, 100000000, 0, false},
		{"shortfall", "completed", "20", 0, 1800000000, 1000000000, 800000000, false},
		{"exact decimal price", "completed", "2", 820000000, 180000000, 100000000, 0, true},
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
			if tc.exactPrice {
				fixture.fake.Advance(time.Second)
				for _, price := range []*model.PriceVersion{
					{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: `0.100000000000000001 > 0.1 ? tier("video", u("seconds") * 0.1) : tier("unreachable", -1)`, EffectiveAt: clock.Now().Unix()},
					{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: `0.100000000000000001 > 0.1 ? tier("video", u("seconds") * 0.05) : tier("unreachable", -1)`, EffectiveAt: clock.Now().Unix()},
				} {
					require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
				}
			}
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
	expensive := fmt.Sprintf(`{"model":"gateway-alias","prompt":"A longer green bird","action":"remix","seconds":"20","origin_task_ids":[%q]}`, original.TaskID)
	response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", expensive)
	require.Equal(t, 402, response.Code, response.Body.String())
	assert.Equal(t, 1, calls, "explicit remix quantities must be funded before dispatch")
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
	SetVideoRouter(fixture.engine)
	for _, language := range []struct{ locale, message string }{{"en", "Task plugin does not support video remix"}, {"zh-CN", "该任务插件不支持视频 remix"}, {"zh-TW", "此任務外掛不支援影片 remix"}} {
		request := httptest.NewRequest(http.MethodPost, "/v1/videos/"+task.TaskID+"/remix", strings.NewReader(`{"prompt":"A green bird"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
		request.Header.Set("Accept-Language", language.locale)
		response = httptest.NewRecorder()
		fixture.engine.ServeHTTP(response, request)
		assert.Equal(t, 400, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), language.message)
	}
	assert.Equal(t, 1, calls, "unsupported remix must not submit an unrelated generation")
	var postings int64
	require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("request_id <> ''").Count(&postings).Error)
	assert.EqualValues(t, 1, postings, "unsupported remix must be rejected before reservation")
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
	require.ErrorIs(t, err, model.ErrGatewayEvaluationPending)
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
			assert.True(t, attempt.CostPending, "the rejected attempt has no reported procurement facts")
			assert.True(t, attempt.ConvertedCostPending)
		}
	}
	assert.Equal(t, "settlement_pending", requests[0].Status)
	assert.False(t, requests[0].SalePending)
	for _, kind := range []model.LedgerEntryType{model.LedgerEntryPreConsume, model.LedgerEntryPreConsumeRelease, model.LedgerEntryCharge} {
		var count int64
		require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("request_id = ? AND account_type = ? AND type = ?", requests[0].ID, model.LedgerAccountUserWallet, kind).Count(&count).Error)
		assert.EqualValues(t, 1, count, kind)
	}
}

func TestAsyncTaskTriesAffordableCandidateBeforeReserving(t *testing.T) {
	for _, affordable := range []bool{true, false} {
		t.Run(fmt.Sprintf("affordable=%t", affordable), func(t *testing.T) {
			var firstCalls, secondSubmissions atomic.Int32
			fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				firstCalls.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
			})
			require.NoError(t, model.DB.Model(fixture.offering).Update("route_method", "weight").Error)
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, `{"id":"funded-job","status":"completed","seconds":"4"}`)
					return
				}
				secondSubmissions.Add(1)
				var body map[string]any
				if !assert.NoError(t, common.DecodeJson(r.Body, &body)) {
					return
				}
				assert.Equal(t, "4", fmt.Sprint(body["seconds"]))
				_, _ = io.WriteString(w, `{"id":"funded-job","status":"queued"}`)
			}))
			t.Cleanup(second.Close)
			provider := &model.Provider{Name: "Affordable task account", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Key: "affordable-provider-key", BaseURL: &second.URL}
			require.NoError(t, model.DB.Create(provider).Error)
			overrides := `{}`
			if affordable {
				overrides = `{"seconds":"4"}`
			}
			channel := &model.Channel{ProviderID: provider.Id, ModelID: fixture.channel.ModelID, Protocol: "task:sora", UpstreamModelName: "sora-2", MappingConfirmed: true, Status: model.ChannelEnabled, Weight: 1, ParameterOverrides: overrides}
			require.NoError(t, model.DB.Create(channel).Error)
			require.NoError(t, model.CreatePriceVersion(t.Context(), &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: channel.ID, Currency: "USD", Expression: `tier("video", u("seconds") * 0.05)`, EffectiveAt: clock.Now().Unix()}, fixture.sale.CreatedBy))
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"12"}`)
			assert.Zero(t, firstCalls.Load(), "insufficient funds must not contact the first Provider")
			if !affordable {
				require.Equal(t, http.StatusPaymentRequired, response.Code, response.Body.String())
				assert.Zero(t, secondSubmissions.Load())
				var count int64
				require.NoError(t, model.DB.Model(&model.GatewayBilling{}).Count(&count).Error)
				assert.Zero(t, count)
				require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("type = ?", model.LedgerEntryPreConsume).Count(&count).Error)
				assert.Zero(t, count)
				return
			}
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.EqualValues(t, 1, secondSubmissions.Load())
			service.RunTaskPollingOnce(t.Context(), nil)
			service.RunTaskPollingOnce(t.Context(), nil)
			requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.EqualValues(t, 360000000, requests[0].Receivable.Nanos)
			attempts, total, err := model.ListGatewayAttempts(t.Context(), requests[0].ID, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.Equal(t, channel.ID, attempts[0].ChannelID)
			assert.EqualValues(t, 200000000, attempts[0].Cost.Nanos)
			assertTextGatewayLedger(t, requests[0], 200000000)
			var reservations int64
			require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("request_id = ? AND type = ?", requests[0].ID, model.LedgerEntryPreConsume).Count(&reservations).Error)
			assert.EqualValues(t, 1, reservations)
		})
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

func TestAsyncTaskInsufficientWalletDoesNotExchangeProviderCredentials(t *testing.T) {
	const exchangedKey = "task-oauth-fixture"
	var funded atomic.Bool
	var generationCalls atomic.Int32
	fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
		generationCalls.Add(1)
		assert.True(t, funded.Load(), "unfunded task must not reach generation")
		assert.Equal(t, "Bearer "+exchangedKey, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"private-oauth-job","status":"queued"}`))
	})
	source, err := plugins.Source("sora")
	require.NoError(t, err)
	source = strings.Replace(source, "apiVersion: 1,", `apiVersion: 1, auth: "vertex_oauth",`, 1)
	source = strings.ReplaceAll(source, `Authorization: "Bearer " + ctx.apiKey`, `Authorization: ctx.authHeader`)
	source = strings.Replace(source, "export function parseSubmitResponse(ctx, resp) {", `export function parseSubmitResponse(ctx, resp) {
		if (ctx.authHeader !== "Bearer task-oauth-fixture") throw new Error("accepted response lost its prepared authentication context");`, 1)
	jsplugin.DefaultRegistry = jsplugin.NewRegistry()
	_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "sora"})
	require.NoError(t, err)
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(private)
	require.NoError(t, err)
	credentials, err := common.Marshal(map[string]string{"project_id": "synthetic-task-project", "client_email": "synthetic@example.invalid", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))})
	require.NoError(t, err)
	require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("key", string(credentials)).Error)
	_, err = model.PostLedgerEntries(model.LedgerPosting{Account: model.UserWalletAccount(fixture.user.Id), Type: model.LedgerEntryAdjustment, Amount: money.Amount{Nanos: -1000000000, Currency: "USD"}, IdempotencyKey: "task-empty-wallet"})
	require.NoError(t, err)
	var provider model.Provider
	require.NoError(t, model.DB.First(&provider, fixture.channel.ProviderID).Error)
	generationURL, err := url.Parse(provider.GetBaseURL())
	require.NoError(t, err)
	var exchanges atomic.Int32
	scheduled := &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: `tier("new", u("seconds") * 0.2)`, EffectiveAt: clock.Now().Unix() + 1}
	require.NoError(t, model.CreatePriceVersion(t.Context(), scheduled, fixture.sale.CreatedBy))
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		var reservations int64
		require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("request_id <> '' AND type = ?", model.LedgerEntryPreConsume).Count(&reservations).Error)
		assert.EqualValues(t, 1, reservations, "credential exchange must follow the reservation")
		fixture.fake.Advance(2 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":%q,"token_type":"Bearer","expires_in":3600}`, exchangedKey)
	}))
	t.Cleanup(broker.Close)
	brokerURL, err := url.Parse(broker.URL)
	require.NoError(t, err)
	client := service.GetHttpClient()
	original := client.Transport
	t.Cleanup(func() { client.Transport = original })
	transport := original
	if transport == nil {
		transport = http.DefaultTransport
	}
	client.Transport = channelProbeRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == generationURL.Host {
			return transport.RoundTrip(request)
		}
		if request.URL.Host != "www.googleapis.com" || request.URL.Path != "/oauth2/v4/token" {
			return nil, fmt.Errorf("unexpected credential destination")
		}
		copyRequest := request.Clone(request.Context())
		copyURL := *request.URL
		copyURL.Scheme, copyURL.Host = brokerURL.Scheme, brokerURL.Host
		copyRequest.URL, copyRequest.Host = &copyURL, brokerURL.Host
		return transport.RoundTrip(copyRequest)
	})
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
	assert.Equal(t, 402, response.Code, response.Body.String())
	assert.Zero(t, exchanges.Load(), "Provider credential I/O must wait for reservation")
	assert.NotContains(t, response.Body.String(), "PRIVATE KEY")
	assert.NotContains(t, response.Body.String(), exchangedKey)
	_, err = model.TopUpWallet(model.WalletTopUp{UserId: fixture.user.Id, Source: money.Amount{Nanos: 1000000000, Currency: "USD"}, Reference: "task-fund-oauth"})
	require.NoError(t, err)
	funded.Store(true)
	response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.EqualValues(t, 1, exchanges.Load())
	assert.EqualValues(t, 1, generationCalls.Load())
	var task model.Task
	require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Take(&task).Error)
	assert.Empty(t, task.PrivateData.Key)
	persisted, err := common.Marshal(task)
	require.NoError(t, err)
	assert.NotContains(t, string(persisted), exchangedKey)
	assert.NotContains(t, string(persisted), "PRIVATE KEY")
	attempts, total, err := model.ListGatewayAttempts(t.Context(), task.PrivateData.Gateway.Settlement.Request.ID, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	assert.Equal(t, scheduled.ID, attempts[0].PurchasePriceVersionID, "purchase price freezes at the model send after OAuth preparation")
	assert.GreaterOrEqual(t, attempts[0].StartedAt, scheduled.EffectiveAt*1000)
}

func TestAsyncTaskRecoveryDoesNotStarveLaterTerminalFacts(t *testing.T) {
	for _, tc := range []struct {
		name          string
		badTerminal   bool
		limit, passes int
		wallet        int64
	}{
		{"pending", false, 1, 2, 460000000},
		{"evaluation failure", true, 2, 1, 764000000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var submitted atomic.Int32
			fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost {
					_, _ = fmt.Fprintf(w, `{"id":"recovery-job-%d","status":"queued"}`, submitted.Add(1))
					return
				}
				status := "completed"
				if strings.HasSuffix(r.URL.Path, "recovery-job-1") && !tc.badTerminal {
					status = "queued"
				}
				_, _ = fmt.Fprintf(w, `{"id":%q,"status":%q,"seconds":"2"}`, strings.TrimPrefix(r.URL.Path, "/v1/videos/"), status)
			})
			if tc.badTerminal {
				fixture.fake.Advance(time.Second)
				require.NoError(t, model.CreatePriceVersion(t.Context(), &model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: `u("seconds") == 2 ? tier("overflow", 10000000000) : tier("video", u("seconds") * 0.01)`, EffectiveAt: clock.Now().Unix()}, fixture.sale.CreatedBy))
			}
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
			require.Equal(t, 200, response.Code, response.Body.String())
			fixture.fake.Advance(time.Second)
			require.NoError(t, model.CreatePriceVersion(t.Context(), &model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: `tier("video", u("seconds") * 0.1)`, EffectiveAt: clock.Now().Unix()}, fixture.sale.CreatedBy))
			response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"Another bird","seconds":"4"}`)
			require.Equal(t, 200, response.Code, response.Body.String())
			var later model.Task
			require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Order("id DESC").First(&later).Error)
			journal := t.TempDir()
			t.Setenv("GATEWAY_SETTLEMENT_JOURNAL_DIR", "/dev/null/unwritable")
			service.RunTaskPollingOnce(t.Context(), nil)
			t.Setenv("GATEWAY_SETTLEMENT_JOURNAL_DIR", journal)
			if tc.badTerminal {
				// Another request already has a prepared financial intent. A bad
				// task must not stop the scheduled handler from replaying it.
				text := model.GatewayRequest{ID: "unrelated-recovery-text", UserID: fixture.user.Id, SaleCurrency: "USD", SaleFXRate: "1", GroupRatio: "1", Receivable: money.Amount{Nanos: 20000000, Currency: "USD"}, Status: "success"}
				require.NoError(t, model.ReserveGateway(&text, text.Receivable, text.Receivable))
				require.NoError(t, model.PrepareGatewaySettlement(model.GatewaySettlement{Request: text, SaleSource: text.Receivable}))
				require.NoError(t, model.DB.AutoMigrate(&model.SystemTask{}, &model.SystemTaskLock{}))
				job, err := model.CreateSystemTask("gateway_settlement", nil, nil)
				require.NoError(t, err)
				claimed, won, err := model.ClaimSystemTask(job.ID, job.Type, "async49-recovery", common.GetTimestamp()+60)
				require.NoError(t, err)
				require.True(t, won)
				service.GatewayRecoveryHandler{}.Run(t.Context(), claimed, "async49-recovery")
				finished, err := model.GetSystemTaskByTaskID(job.TaskID)
				require.NoError(t, err)
				assert.Equal(t, model.SystemTaskStatusFailed, finished.Status, "the bad task must remain visible")
				recovered, err := model.GetGatewayRequest(t.Context(), text.ID, fixture.user.Id)
				require.NoError(t, err)
				assert.Equal(t, "success", recovered.Status)
				assert.EqualValues(t, 20000000, recovered.Receivable.Nanos)
			} else {
				for range tc.passes {
					fixture.fake.Advance(time.Millisecond)
					_, err := service.RecoverGatewayTasks(t.Context(), tc.limit)
					require.NoError(t, err)
				}
			}
			record, err := model.GetGatewayRequest(t.Context(), later.PrivateData.Gateway.Settlement.Request.ID, fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, "success", record.Status)
			assert.EqualValues(t, 180000000, record.Receivable.Nanos)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, tc.wallet, wallet.Balance.Nanos)
		})
	}
}

func TestAsyncTaskUnknownSubmissionCannotStarvePollingOrTimeouts(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired_%t", expired), func(t *testing.T) {
			var accept atomic.Bool
			var polls atomic.Int32
			fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					polls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"id":"known-job","status":"completed","seconds":"2"}`))
					return
				}
				if !accept.Load() {
					_, err := io.Copy(io.Discard, r.Body)
					require.NoError(t, err)
					connection, _, err := w.(http.Hijacker).Hijack()
					require.NoError(t, err)
					require.NoError(t, connection.Close())
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"known-job","status":"queued"}`))
			})
			fixture.fake.Advance(time.Second)
			for _, owner := range []struct {
				kind string
				id   int
			}{{model.PriceOwnerOffering, fixture.offering.ID}, {model.PriceOwnerChannel, fixture.channel.ID}} {
				require.NoError(t, model.CreatePriceVersion(t.Context(), &model.PriceVersion{OwnerType: owner.kind, OwnerID: owner.id, Currency: "USD", Expression: `tier("free",0)`, EffectiveAt: clock.Now().Unix()}, fixture.sale.CreatedBy))
			}
			unknown := 1
			if expired {
				unknown = 100
			}
			for range unknown {
				response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
				require.Equal(t, 502, response.Code, response.Body.String())
			}
			fixture.fake.Advance(time.Second)
			for _, price := range []*model.PriceVersion{
				{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: `tier("video",u("seconds")*0.1)`, EffectiveAt: clock.Now().Unix()},
				{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: `tier("video",u("seconds")*0.05)`, EffectiveAt: clock.Now().Unix()},
			} {
				require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
			}
			accept.Store(true)
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"Another bird","seconds":"4"}`)
			require.Equal(t, 200, response.Code, response.Body.String())
			var known model.Task
			require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Order("id DESC").First(&known).Error)
			previousLimit, previousTimeout := constant.TaskQueryLimit, constant.TaskTimeoutMinutes
			constant.TaskQueryLimit, constant.TaskTimeoutMinutes = 1, 0
			t.Cleanup(func() { constant.TaskQueryLimit, constant.TaskTimeoutMinutes = previousLimit, previousTimeout })
			if expired {
				constant.TaskTimeoutMinutes = 1
				cutoff := time.Now().Unix() - 3600
				require.NoError(t, model.DB.Model(&model.Task{}).Where("user_id = ? AND id <> ?", fixture.user.Id, known.ID).Update("submit_time", cutoff-1).Error)
				require.NoError(t, model.DB.Model(&known).Update("submit_time", cutoff).Error)
			}
			service.RunTaskPollingOnce(t.Context(), nil)
			var refreshed model.Task
			require.NoError(t, model.DB.First(&refreshed, known.ID).Error)
			if expired {
				assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), refreshed.Status)
				assert.Zero(t, polls.Load())
			} else {
				assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), refreshed.Status)
				assert.EqualValues(t, 1, polls.Load())
			}
			var retained int64
			require.NoError(t, model.DB.Model(&model.Task{}).Where("user_id = ? AND id <> ? AND status = ?", fixture.user.Id, known.ID, model.TaskStatusUnknown).Count(&retained).Error)
			assert.EqualValues(t, unknown, retained, "unknown accepted identity must remain pending, not be discarded to free the queue")
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			if expired {
				assert.EqualValues(t, 1000000000, wallet.Balance.Nanos)
			} else {
				assert.EqualValues(t, 820000000, wallet.Balance.Nanos)
			}
		})
	}
}

func TestAsyncTaskUncertainSubmissionKeepsOneReservationWithoutRetry(t *testing.T) {
	var calls atomic.Int32
	fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, err := io.Copy(io.Discard, r.Body)
		assert.NoError(t, err)
		connection, _, err := w.(http.Hijacker).Hijack()
		if !assert.NoError(t, err) {
			return
		}
		// The vendor has accepted the complete body, but its reply is lost.
		assert.NoError(t, connection.Close())
	})
	var fallbackCalls atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"duplicate-job","status":"queued"}`))
	}))
	t.Cleanup(fallback.Close)
	provider := &model.Provider{Name: "Uncertain task fallback", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Key: "fallback-fixture-key", BaseURL: &fallback.URL}
	require.NoError(t, model.DB.Create(provider).Error)
	var fallbackModel model.Model
	require.NoError(t, model.DB.First(&fallbackModel, fixture.channel.ModelID).Error)
	fallbackModel.Id, fallbackModel.ModelName = 0, "sora-2-pro"
	require.NoError(t, model.DB.Create(&fallbackModel).Error)
	fixture.offering.Members = append(fixture.offering.Members, model.ModelOfferingMember{ModelID: fallbackModel.Id, Tier: 2})
	require.NoError(t, model.SaveModelOffering(t.Context(), fixture.offering, "save"))
	channel := &model.Channel{ProviderID: provider.Id, ModelID: fallbackModel.Id, Protocol: "task:sora", UpstreamModelName: "sora-2-pro", MappingConfirmed: true, Status: model.ChannelEnabled, Weight: 1, ParameterOverrides: `{}`}
	require.NoError(t, model.DB.Create(channel).Error)
	require.NoError(t, model.CreatePriceVersion(t.Context(), &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: channel.ID, Currency: "USD", Expression: `tier("video", u("seconds") * 0.05)`, EffectiveAt: clock.Now().Unix()}, fixture.sale.CreatedBy))
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
	assert.Equal(t, 502, response.Code)
	assert.Zero(t, fallbackCalls.Load(), "a possibly accepted task must never be submitted to a fallback Provider")
	assert.EqualValues(t, 1, calls.Load(), "an indeterminate submission must not create another vendor task")
	fixture.fake.Advance(365 * 24 * time.Hour)
	service.RunTaskPollingOnce(t.Context(), nil)
	_, err := service.RecoverGatewayTasks(t.Context(), 100)
	require.NoError(t, err)
	assert.EqualValues(t, 1, calls.Load(), "unknown upstream identity must not be polled")
	var tasks []model.Task
	require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Find(&tasks).Error)
	require.Len(t, tasks, 1)
	require.NotNil(t, tasks[0].PrivateData.Gateway)
	assert.False(t, tasks[0].PrivateData.Gateway.Accepted)
	assert.Empty(t, tasks[0].PrivateData.Key)
	requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	assert.Equal(t, "submission_unknown", requests[0].Status)
	var wallet model.Wallet
	require.NoError(t, model.DB.Where("account_type = ? AND owner_id = ?", model.LedgerAccountUserWallet, fixture.user.Id).Take(&wallet).Error)
	assert.EqualValues(t, 640000000, wallet.Balance.Nanos)
	for _, entry := range []struct {
		kind  model.LedgerEntryType
		count int64
	}{
		{model.LedgerEntryPreConsume, 1},
		{model.LedgerEntryPreConsumeRelease, 0},
		{model.LedgerEntryCharge, 0},
	} {
		var count int64
		require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("request_id = ? AND account_type = ? AND type = ?", requests[0].ID, model.LedgerAccountUserWallet, entry.kind).Count(&count).Error)
		assert.Equal(t, entry.count, count)
	}
}

func TestAsyncTaskFreezesOriginalSaleAndFinalWirePricingConditions(t *testing.T) {
	const clientHeader = "client-fast-opaque-credential-not-for-storage"
	const wireHeader = "wire-fast-opaque-credential-not-for-storage"
	for _, tc := range []struct {
		name, saleCondition, purchaseCondition string
		balance                                int64
		multipart, omitContentType             bool
	}{
		{"request predicates", `has(header("x-task-rate"), "client-fast")`, `has(header("x-task-rate"), "wire-fast")`, 100000000, false, false},
		{"measured conditions and initially skipped probes", `has(header("x-task-rate"), "client-fast") && u("seconds") <= 2`, `u("seconds") <= 2 && has(header("x-task-rate"), "wire-fast")`, 550000000, false, false},
		{"multipart scalar probes with file", `has(header("x-task-rate"), "client-fast")`, `has(header("x-task-rate"), "wire-fast")`, 100000000, true, false},
		{"default JSON descriptor without content type", `has(header("x-task-rate"), "client-fast")`, `has(header("x-task-rate"), "wire-fast")`, 100000000, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					_, _ = w.Write([]byte(`{"id":"private-probe-job","status":"completed","seconds":"2"}`))
					return
				}
				assert.Equal(t, wireHeader, r.Header.Get("X-Task-Rate"))
				if tc.omitContentType {
					assert.Empty(t, r.Header.Get("Content-Type"))
				}
				if tc.multipart {
					if !assert.NoError(t, r.ParseMultipartForm(1<<20)) {
						return
					}
					assert.Equal(t, "sora-2", r.FormValue("model"))
					assert.Len(t, r.MultipartForm.File["input_reference"], 1)
				} else {
					var body map[string]any
					assert.NoError(t, common.DecodeJson(r.Body, &body))
					assert.Equal(t, "sora-2", body["model"])
				}
				_, _ = w.Write([]byte(`{"id":"private-probe-job","status":"queued"}`))
			})
			if tc.omitContentType {
				source, err := plugins.Source("sora")
				require.NoError(t, err)
				source = strings.ReplaceAll(source, `headers["Content-Type"] = "application/json";`, "")
				_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "sora"})
				require.NoError(t, err)
			}
			header, err := common.Marshal(map[string]string{"X-Task-Rate": wireHeader})
			require.NoError(t, err)
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("header_override", string(header)).Error)
			fixture.fake.Advance(time.Second)
			for _, price := range []*model.PriceVersion{
				{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: fmt.Sprintf(`tier("video", u("seconds") * 0.1) * (%s ? 2 : 1) * (param("model") == "gateway-alias" ? 1.25 : 1)`, tc.saleCondition), EffectiveAt: clock.Now().Unix()},
				{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: fmt.Sprintf(`tier("video", u("seconds") * 0.05) * (%s ? 3 : 1) * (param("model") == "sora-2" ? 2 : 1)`, tc.purchaseCondition), EffectiveAt: clock.Now().Unix()},
			} {
				require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/tasks/sora", strings.NewReader(`{"model":"gateway-alias","prompt":"private-pricing-request-content","seconds":"4"}`))
			request.Header.Set("Content-Type", "application/json")
			if tc.multipart {
				var payload bytes.Buffer
				writer := multipart.NewWriter(&payload)
				for name, value := range map[string]string{"model": "gateway-alias", "prompt": "private-pricing-request-content", "seconds": "4"} {
					require.NoError(t, writer.WriteField(name, value))
				}
				image, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
				require.NoError(t, err)
				file, err := writer.CreateFormFile("input_reference", "frame.png")
				require.NoError(t, err)
				_, err = file.Write(image)
				require.NoError(t, err)
				require.NoError(t, writer.Close())
				request = httptest.NewRequest(http.MethodPost, "/v1/videos", &payload)
				request.Header.Set("Content-Type", writer.FormDataContentType())
			}
			request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
			request.Header.Set("X-Task-Rate", clientHeader)
			response := httptest.NewRecorder()
			fixture.engine.ServeHTTP(response, request)
			require.Equal(t, 200, response.Code, response.Body.String())
			var wallet model.Wallet
			require.NoError(t, model.DB.Where("account_type = ? AND owner_id = ?", model.LedgerAccountUserWallet, fixture.user.Id).Take(&wallet).Error)
			assert.Equal(t, tc.balance, wallet.Balance.Nanos, "reservation must use the original client price conditions")
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("header_override", `{"X-Task-Rate":"changed-wire-condition"}`).Error)
			runAsyncTaskRecoveryProcess(t, true)
			requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.EqualValues(t, 450000000, requests[0].Receivable.Nanos)
			attempts, total, err := model.ListGatewayAttempts(t.Context(), requests[0].ID, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.EqualValues(t, 600000000, attempts[0].Cost.Nanos)
			for _, query := range []struct{ table, field string }{{"tasks", "private_data"}, {"gateway_billings", "request_data"}, {"gateway_billings", "settlement"}} {
				var values []string
				require.NoError(t, model.DB.Table(query.table).Pluck(query.field, &values).Error)
				for _, value := range values {
					for _, secret := range []string{clientHeader, wireHeader, "provider-secret", "private-pricing-request-content", "Bearer " + fixture.token.Key} {
						assert.False(t, strings.Contains(value, secret), "persisted pricing conditions must not retain raw probes or request content")
					}
				}
			}
		})
	}
}

func TestAsyncTaskPricingRejectsUnsafeDurableProbes(t *testing.T) {
	calls := 0
	fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) { calls++ })
	pricingPAT := createWalletTestAccessToken(t, fixture.sale.CreatedBy, "pricing:read", "pricing:write")
	for _, owner := range []struct {
		kind string
		id   int
	}{{model.PriceOwnerOffering, fixture.offering.ID}, {model.PriceOwnerChannel, fixture.channel.ID}} {
		for i, expression := range []string{
			`tier("video", u("seconds") * (param("rate") ?? 0.1))`,
			`u("seconds") < (param("threshold") ?? 3) ? tier("low", u("seconds") * 0.1) : tier("high", u("seconds") * 0.2)`,
			`let flag = u("seconds") > 2; header("x-mode") == (flag ? "fast" : "slow") ? tier("a", u("seconds") * 0.2) : tier("b", u("seconds") * 0.1)`,
			`let read = header; has(read("x-mode"), "fast") ? tier("a", u("seconds") * 0.2) : tier("b", u("seconds") * 0.1)`,
		} {
			body, err := common.Marshal(map[string]any{"owner_type": owner.kind, "owner_id": owner.id, "currency": "USD", "expression": expression, "effective_at": clock.Now().Unix() + int64(i+1)})
			require.NoError(t, err)
			response := adminSecurityRequest(fixture.engine, pricingPAT, http.MethodPost, "/api/admin/v1/price-versions", string(body))
			assert.Equal(t, 400, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), "independent boolean conditions")
		}
	}
	// Historical or externally inserted prices must fail closed at dispatch too.
	fixture.fake.Advance(10 * time.Second)
	price := &model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, OfferingID: &fixture.offering.ID, Currency: "USD", Expression: `tier("video", u("seconds") * float(header("x-rate")))`, EffectiveAt: clock.Now().Unix(), CreatedAt: clock.Now().Unix(), CreatedBy: fixture.sale.CreatedBy}
	require.NoError(t, model.DB.Omit("Creator", "Channel", "Offering").Create(price).Error)
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks/sora", strings.NewReader(`{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
	request.Header.Set("X-Rate", "0.1")
	response := httptest.NewRecorder()
	fixture.engine.ServeHTTP(response, request)
	assert.Equal(t, 503, response.Code, response.Body.String())
	assert.Zero(t, calls)
	var count int64
	require.NoError(t, model.DB.Model(&model.GatewayBilling{}).Count(&count).Error)
	assert.Zero(t, count)
}

func TestAsyncTaskMissingPricingSnapshotKeepsUnknownAmountPending(t *testing.T) {
	for _, missingSale := range []bool{true, false} {
		t.Run(fmt.Sprintf("sale=%v", missingSale), func(t *testing.T) {
			fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					_, _ = w.Write([]byte(`{"id":"lost-context-job","status":"completed","seconds":"2"}`))
					return
				}
				_, _ = w.Write([]byte(`{"id":"lost-context-job","status":"queued"}`))
			})
			fixture.fake.Advance(time.Second)
			for _, price := range []*model.PriceVersion{
				{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: `tier("video", u("seconds") * 0.1) * (has(header("x-task-rate"), "fast") ? 2 : 1)`, EffectiveAt: clock.Now().Unix()},
				{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: `tier("video", u("seconds") * 0.05) * (has(header("x-task-rate"), "fast") ? 2 : 1)`, EffectiveAt: clock.Now().Unix()},
			} {
				require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
			}
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("header_override", `{"X-Task-Rate":"fast"}`).Error)
			request := httptest.NewRequest(http.MethodPost, "/v1/tasks/sora", strings.NewReader(`{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
			request.Header.Set("X-Task-Rate", "fast")
			response := httptest.NewRecorder()
			fixture.engine.ServeHTTP(response, request)
			require.Equal(t, 200, response.Code, response.Body.String())
			var task model.Task
			require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Take(&task).Error)
			if missingSale {
				task.PrivateData.Gateway.SaleConditions = nil
			} else {
				task.PrivateData.Gateway.PurchaseConditions = nil
			}
			require.NoError(t, task.Update())
			service.RunTaskPollingOnce(t.Context(), nil)
			requestRecord, err := model.GetGatewayRequest(t.Context(), task.PrivateData.Gateway.Settlement.Request.ID, fixture.user.Id)
			require.NoError(t, err)
			assert.Equal(t, "settlement_pending", requestRecord.Status)
			assert.Equal(t, missingSale, requestRecord.SalePending)
			attempts, total, err := model.ListGatewayAttempts(t.Context(), requestRecord.ID, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.Equal(t, !missingSale, attempts[0].CostPending)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			expected := int64(640000000)
			if missingSale {
				expected = 280000000
			}
			assert.Equal(t, expected, wallet.Balance.Nanos)
		})
	}
}

func TestAsyncTaskPricingSnapshotReplaysOnlyCapturedRequestConditions(t *testing.T) {
	const expression = `u("seconds") > 2 ? tier("initial", u("seconds") * 0.1) : (float(param("rate")) > 0 ? tier("final", u("seconds") * 0.3) : tier("zero", 0))`
	_, trace, err := billingexpr.RunMoneyExpr(expression, billingexpr.MoneyInput{TaskUsage: true, Request: billingexpr.RequestInput{Usage: map[string]any{"seconds": 4}, Body: []byte(`{}`), FreezeMoneyRequest: true}}, "USD")
	require.NoError(t, err)
	require.NotNil(t, trace.RequestSnapshot)
	_, _, err = billingexpr.RunMoneyExpr(expression, billingexpr.MoneyInput{TaskUsage: true, Request: billingexpr.RequestInput{Usage: map[string]any{"seconds": 2}, MoneySnapshot: trace.RequestSnapshot}}, "USD")
	require.ErrorContains(t, err, "frozen pricing condition is unavailable")
	_, _, err = billingexpr.RunMoneyExpr(expression+` + 0`, billingexpr.MoneyInput{TaskUsage: true, Request: billingexpr.RequestInput{Usage: map[string]any{"seconds": 2}, MoneySnapshot: trace.RequestSnapshot}}, "USD")
	require.ErrorContains(t, err, "does not match its expression")
	const zeroFalse = `param("zero") == 0 && param("disabled") == false ? tier("explicit", u("seconds") * 0.2) : tier("other", u("seconds") * 0.1)`
	var snapshots []*billingexpr.MoneyRequestSnapshot
	for _, body := range []string{`{"zero":0,"disabled":false}`, `{}`} {
		_, trace, err := billingexpr.RunMoneyExpr(zeroFalse, billingexpr.MoneyInput{TaskUsage: true, Request: billingexpr.RequestInput{Usage: map[string]any{"seconds": 4}, Body: []byte(body), FreezeMoneyRequest: true}}, "USD")
		require.NoError(t, err)
		snapshots = append(snapshots, trace.RequestSnapshot)
	}
	// Reuse the cached expression after a second request captures opposite values.
	for i, expected := range []int64{400000000, 200000000} {
		value, _, err := billingexpr.RunMoneyExpr(zeroFalse, billingexpr.MoneyInput{TaskUsage: true, Request: billingexpr.RequestInput{Usage: map[string]any{"seconds": 2}, MoneySnapshot: snapshots[i]}}, "USD")
		require.NoError(t, err)
		amount, err := value.Receivable()
		require.NoError(t, err)
		assert.Equal(t, expected, amount.Nanos)
	}
	for condition := range snapshots[0].Conditions {
		delete(snapshots[0].Conditions, condition)
	}
	_, _, err = billingexpr.RunMoneyExpr(zeroFalse, billingexpr.MoneyInput{TaskUsage: true, Request: billingexpr.RequestInput{Usage: map[string]any{"seconds": 2}, MoneySnapshot: snapshots[0]}}, "USD")
	require.ErrorContains(t, err, "frozen pricing condition is unavailable", "an incomplete snapshot must not re-evaluate absent raw probes")
}

func TestAsyncTaskEvaluationFailuresUseExistingGatewayReconciliation(t *testing.T) {
	for _, tc := range []struct {
		name, sale, purchase, status               string
		currency                                   money.Currency
		salePending, costPending, convertedPending bool
		wallet, charge, payable                    int64
	}{
		{"sale overflow", `u("seconds") == 2 ? tier("overflow", 10000000000) : tier("video", u("seconds") * 0.1)`, `tier("video", u("seconds") * 0.05)`, "completed", "USD", true, false, false, 640000000, 0, 100000000},
		{"purchase overflow", `tier("video", u("seconds") * 0.1)`, `u("seconds") == 2 ? tier("overflow", 10000000000) : tier("video", u("seconds") * 0.05)`, "completed", "USD", false, true, true, 820000000, 180000000, 0},
		{"purchase FX overflow", `tier("video", u("seconds") * 0.1)`, `u("seconds") == 2 ? tier("higher", 10) : tier("video", u("seconds") * 0.05)`, "completed", "EUR", false, false, true, 820000000, 180000000, 10000000000},
		{"failed without actual usage", `tier("video", u("seconds") * 0.1)`, `tier("video", u("seconds") * 0.05)`, "failed", "USD", false, true, true, 1000000000, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			submits := 0
			fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					id := strings.TrimPrefix(r.URL.Path, "/v1/videos/")
					if tc.status == "failed" {
						_, _ = fmt.Fprintf(w, `{"id":%q,"status":"failed"}`, id)
					} else {
						_, _ = fmt.Fprintf(w, `{"id":%q,"status":%q,"seconds":"2"}`, id, tc.status)
					}
					return
				}
				submits++
				_, _ = fmt.Fprintf(w, `{"id":"private-job-%d","status":"queued"}`, submits)
			})
			if tc.currency == "EUR" {
				_, err := model.SaveFXRate(t.Context(), "EUR", "USD", "1000000000", "0", fixture.sale.CreatedBy)
				require.NoError(t, err)
			}
			fixture.fake.Advance(time.Second)
			for _, price := range []*model.PriceVersion{
				{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: tc.sale, EffectiveAt: clock.Now().Unix()},
				{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: tc.currency, Expression: tc.purchase, EffectiveAt: clock.Now().Unix()},
			} {
				require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
			}
			body := `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", body)
			require.Equal(t, 200, response.Code, response.Body.String())
			service.RunTaskPollingOnce(t.Context(), nil)
			_, err := service.RecoverGatewayTasks(t.Context(), 100)
			assert.ErrorIs(t, err, model.ErrGatewayEvaluationPending)
			requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.Equal(t, "settlement_pending", requests[0].Status)
			assert.Equal(t, tc.salePending, requests[0].SalePending)
			assert.Equal(t, tc.charge, requests[0].Receivable.Nanos)
			stale, staleTotal, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id, Status: "pending"}, 0, 10)
			require.NoError(t, err)
			assert.Empty(t, stale)
			assert.Zero(t, staleTotal, "pending accounting supersedes its older submission record")
			attempts, total, err := model.ListGatewayAttempts(t.Context(), requests[0].ID, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.Equal(t, tc.costPending, attempts[0].CostPending)
			assert.Equal(t, tc.convertedPending, attempts[0].ConvertedCostPending)
			assert.Equal(t, tc.payable, attempts[0].Cost.Nanos)
			for _, view := range []struct {
				key, path string
				admin     bool
			}{
				{fixture.userPAT, "/api/requests/self?status=settlement_pending", false},
				{fixture.adminPAT, "/api/admin/v1/requests/" + requests[0].ID + "/attempts", true},
			} {
				response := adminSecurityRequest(fixture.engine, view.key, http.MethodGet, view.path, "")
				require.Equal(t, 200, response.Code)
				var page struct {
					Data struct {
						Items []map[string]any
						Total int64
					}
				}
				require.NoError(t, common.Unmarshal(response.Body.Bytes(), &page))
				require.Len(t, page.Data.Items, 1)
				assert.EqualValues(t, 1, page.Data.Total)
				if view.admin {
					assert.Equal(t, tc.costPending, page.Data.Items[0]["cost_pending"] == true)
					assert.Equal(t, tc.convertedPending, page.Data.Items[0]["converted_cost_pending"] == true)
				} else {
					assert.Equal(t, tc.salePending, page.Data.Items[0]["sale_pending"] == true)
					assert.NotContains(t, page.Data.Items[0], "cost_pending")
				}
			}
			for _, account := range []struct {
				kind  model.LedgerAccountType
				owner int
				nanos int64
			}{
				{model.LedgerAccountUserWallet, fixture.user.Id, tc.wallet},
				{model.LedgerAccountProviderPayable, fixture.channel.ProviderID, tc.payable},
			} {
				var wallet model.Wallet
				require.NoError(t, model.DB.Where("account_type = ? AND owner_id = ?", account.kind, account.owner).Take(&wallet).Error)
				assert.Equal(t, account.nanos, wallet.Balance.Nanos)
			}
			_, err = model.TopUpWallet(model.WalletTopUp{UserId: fixture.user.Id, Source: money.Amount{Nanos: 100000000, Currency: "USD"}, Reference: "pending-task-credit"})
			require.NoError(t, err)
			response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", body)
			if tc.salePending {
				assert.Equal(t, 402, response.Code, "a credit must not bypass unknown sale accounting")
				assert.Equal(t, 1, submits)
			} else {
				assert.Equal(t, 200, response.Code, "pending procurement must not freeze known user funds")
				assert.Equal(t, 2, submits)
			}
		})
	}
}

func TestAsyncTaskImmediateImagesReturnOnceWithoutPersistingResults(t *testing.T) {
	for _, path := range []string{"/doubao/api/v3/images/generations", "/v1/images/generations"} {
		t.Run(path, func(t *testing.T) {
			calls := 0
			const imageURL = "https://artifact.invalid/retention-image.png"
			fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, "/api/v3/images/generations", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"model":"doubao-seedream-5-0-pro-260628","data":[{"url":%q,"size":"2048x2048"}],"usage":{"generated_images":1}}`, imageURL)
			})
			source, err := plugins.Source("doubao")
			require.NoError(t, err)
			jsplugin.DefaultRegistry = jsplugin.NewRegistry()
			_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "doubao"})
			require.NoError(t, err)
			require.NoError(t, model.DB.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Update("capabilities", capability.Set{InputModalities: []capability.Modality{capability.Text, capability.Image}, OutputModalities: []capability.Modality{capability.Image}}).Error)
			require.NoError(t, model.DB.Model(fixture.channel).Updates(map[string]any{"protocol": "task:doubao", "upstream_model_name": "doubao-seedream-5-0-pro-260628"}).Error)
			fixture.fake.Advance(time.Second)
			for _, price := range []*model.PriceVersion{
				{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: `tier("image", (u("images_up_to_1_5k") + u("images_above_1_5k")) * 0.1)`, EffectiveAt: clock.Now().Unix()},
				{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: `tier("image", (u("images_up_to_1_5k") + u("images_above_1_5k")) * 0.05)`, EffectiveAt: clock.Now().Unix()},
			} {
				require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
			}
			builder := newPluginGenerationBuilder(fixture.engine.Routes(), nil, productionPluginRouteHandlers)
			require.NoError(t, jsplugin.DefaultRegistry.SetGenerationPreparer(builder.prepare))
			dispatcher := &pluginRouteDispatcher{registry: jsplugin.DefaultRegistry}
			fixture.engine.NoRoute(dispatcher.dispatch)
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, path, `{"model":"gateway-alias","prompt":"A bird","size":"2K","response_format":"url"}`)
			require.Equal(t, 200, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), imageURL, "the client must receive the complete result once")
			var task model.Task
			require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Take(&task).Error)
			assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), task.Status)
			assert.True(t, task.PrivateData.ResultDiscarded)
			assert.Empty(t, task.Data, "inline image responses must not be stored")
			query := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodGet, "/v1/tasks/"+task.TaskID, "")
			assert.Equal(t, 200, query.Code, "task metadata remains available without the discarded result")
			var metadata map[string]any
			require.NoError(t, common.Unmarshal(query.Body.Bytes(), &metadata))
			assert.NotContains(t, metadata, "data")
			query = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodGet, "/v1/tasks/"+task.TaskID+"/artifacts", "")
			require.Equal(t, 200, query.Code)
			var artifacts struct {
				Items []struct{ Key string } `json:"artifacts"`
			}
			require.NoError(t, common.Unmarshal(query.Body.Bytes(), &artifacts))
			assert.Empty(t, artifacts.Items, "discarded results must not expose retrievable artifacts")
			assert.Equal(t, 1, calls)
			requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.Equal(t, "success", requests[0].Status)
			assert.EqualValues(t, 90000000, requests[0].Receivable.Nanos)
			attempts, total, err := model.ListGatewayAttempts(t.Context(), requests[0].ID, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.EqualValues(t, 50000000, attempts[0].Cost.Nanos)
			assert.Equal(t, money.Currency("USD"), attempts[0].Cost.Currency)
		})
	}
}

func TestAsyncTaskRemixCredentialFailureDisablesOnlySelectedOriginalKey(t *testing.T) {
	fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/remix") {
			assert.Equal(t, "Bearer spare-secret", r.Header.Get("Authorization"))
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_api_key"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"original-job","status":"queued"}`))
	})
	previous := common.AutomaticDisableChannelEnabled
	common.AutomaticDisableChannelEnabled = true
	t.Cleanup(func() { common.AutomaticDisableChannelEnabled = previous })
	provider, err := model.GetProviderById(fixture.channel.ProviderID, true)
	require.NoError(t, err)
	provider.Key = "first-secret\nspare-secret"
	provider.AutoBan = common.GetPointer(1)
	provider.ProviderInfo = model.ProviderInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModePolling}
	require.NoError(t, model.DB.Model(provider).Select("key", "auto_ban", "channel_info").Updates(provider).Error)
	SetVideoRouter(fixture.engine)
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
	require.Equal(t, 200, response.Code, response.Body.String())
	var task model.Task
	require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Take(&task).Error)
	response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/videos/"+task.TaskID+"/remix", `{"prompt":"A blue bird"}`)
	require.Equal(t, 401, response.Code, response.Body.String())
	provider, err = model.GetProviderById(provider.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusEnabled, provider.Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, provider.ProviderInfo.MultiKeyStatusList[1])
	assert.NotEqual(t, common.ChannelStatusAutoDisabled, provider.ProviderInfo.MultiKeyStatusList[0])
}

func TestAsyncTaskNativeDecodeKeepsLaterCompatibleModel(t *testing.T) {
	var calls atomic.Int32
	fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		require.NoError(t, common.DecodeJson(r.Body, &body))
		assert.Equal(t, "doubao-seedream-5-0-pro-260628", body["model"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"url":"https://artifact.invalid/result.png","size":"2048x2048"}],"usage":{"generated_images":1}}`))
	})
	source, err := plugins.Source("doubao")
	require.NoError(t, err)
	jsplugin.DefaultRegistry = jsplugin.NewRegistry()
	_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "doubao"})
	require.NoError(t, err)
	var first model.Model
	require.NoError(t, model.DB.First(&first, fixture.channel.ModelID).Error)
	first.ModelName = "doubao-seedream-4-0-250828"
	first.Capabilities.OutputModalities = []capability.Modality{capability.Image}
	require.NoError(t, model.DB.Save(&first).Error)
	require.NoError(t, model.DB.Model(fixture.channel).Updates(map[string]any{"protocol": "task:doubao", "upstream_model_name": first.ModelName}).Error)
	second := &model.Model{ModelName: "doubao-seedream-5-0-pro-260628", VendorID: first.VendorID, Status: 1, NameRule: model.NameRuleExact, Capabilities: first.Capabilities}
	require.NoError(t, model.DB.Create(second).Error)
	channel := &model.Channel{ProviderID: fixture.channel.ProviderID, ModelID: second.Id, Protocol: "task:doubao", UpstreamModelName: second.ModelName, MappingConfirmed: true, Status: model.ChannelEnabled, Weight: 1, ParameterOverrides: `{}`}
	require.NoError(t, model.DB.Create(channel).Error)
	fixture.fake.Advance(time.Second)
	for _, price := range []*model.PriceVersion{
		{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: `tier("image", u("images_above_1_5k") * 0.1)`, EffectiveAt: clock.Now().Unix()},
		{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: `tier("image", u("images_above_1_5k") * 0.05)`, EffectiveAt: clock.Now().Unix()},
		{OwnerType: model.PriceOwnerChannel, OwnerID: channel.ID, Currency: "USD", Expression: `tier("image", u("images_above_1_5k") * 0.05)`, EffectiveAt: clock.Now().Unix()},
	} {
		require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
	}
	fixture.offering.Members = append(fixture.offering.Members, model.ModelOfferingMember{ModelID: second.Id, Tier: 1})
	require.NoError(t, model.SaveModelOffering(t.Context(), fixture.offering, "save"))
	builder := newPluginGenerationBuilder(fixture.engine.Routes(), nil, productionPluginRouteHandlers)
	require.NoError(t, jsplugin.DefaultRegistry.SetGenerationPreparer(builder.prepare))
	dispatcher := &pluginRouteDispatcher{registry: jsplugin.DefaultRegistry}
	fixture.engine.NoRoute(dispatcher.dispatch)
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/doubao/api/v3/images/generations", `{"model":"gateway-alias","prompt":"A bird","size":"2K","output_format":"png"}`)
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.EqualValues(t, 1, calls.Load())
	requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	attempts, total, err := model.ListGatewayAttempts(t.Context(), requests[0].ID, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	assert.Equal(t, channel.ID, attempts[0].ChannelID)
	assert.EqualValues(t, 50000000, attempts[0].Cost.Nanos)
	admin := createWalletTestAccessToken(t, fixture.sale.CreatedBy, "channel:operate")
	disabled := adminSecurityRequest(fixture.engine, admin, http.MethodPost, fmt.Sprintf("/api/admin/v1/providers/%d/disable", fixture.channel.ProviderID), "")
	require.Equal(t, 200, disabled.Code, disabled.Body.String())
	response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/doubao/api/v3/images/generations", `{"model":"gateway-alias","prompt":"A bird","size":"2K","output_format":"png"}`)
	assert.Equal(t, 503, response.Code, response.Body.String())
	assert.EqualValues(t, 1, calls.Load())
}

func TestAsyncTaskVideoFallbackCannotBypassDisabledProvider(t *testing.T) {
	var downloads, redirected atomic.Int32
	var redirect atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		assert.Empty(t, r.Header.Get("Authorization"))
		if redirect.Load() && r.URL.Path == "/movie.mp4" {
			close(entered)
			<-release
			w.Header().Set("Location", "/redirected.mp4")
			w.WriteHeader(http.StatusFound)
			return
		}
		if r.URL.Path == "/redirected.mp4" {
			redirected.Add(1)
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("synthetic-video"))
	}))
	t.Cleanup(media.Close)
	fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = fmt.Fprintf(w, `{"id":"movie","status":"succeeded","content":{"video_url":%q},"usage":{"completion_tokens":20}}`, media.URL+"/movie.mp4")
			return
		}
		_, _ = w.Write([]byte(`{"id":"movie"}`))
	})
	fetch := system_setting.GetFetchSetting()
	previousFetch, previousWorker := *fetch, system_setting.WorkerUrl
	fetch.EnableSSRFProtection, system_setting.WorkerUrl = false, ""
	t.Cleanup(func() { *fetch, system_setting.WorkerUrl = previousFetch, previousWorker })
	source, err := plugins.Source("doubao")
	require.NoError(t, err)
	jsplugin.DefaultRegistry = jsplugin.NewRegistry()
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
	SetVideoRouter(fixture.engine)
	builder := newPluginGenerationBuilder(fixture.engine.Routes(), nil, productionPluginRouteHandlers)
	require.NoError(t, jsplugin.DefaultRegistry.SetGenerationPreparer(builder.prepare))
	dispatcher := &pluginRouteDispatcher{registry: jsplugin.DefaultRegistry}
	fixture.engine.NoRoute(dispatcher.dispatch)
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/doubao", `{"model":"gateway-alias","prompt":"A bird","duration":4,"resolution":"720p"}`)
	require.Equal(t, 200, response.Code, response.Body.String())
	service.RunTaskPollingOnce(t.Context(), nil)
	var task model.Task
	require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Take(&task).Error)
	require.Equal(t, model.TaskStatus(model.TaskStatusSuccess), task.Status)
	require.Equal(t, media.URL+"/movie.mp4", task.GetResultURL())
	path := "/v1/videos/" + task.TaskID + "/content"
	response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodGet, path, "")
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.Equal(t, "synthetic-video", response.Body.String())
	assert.EqualValues(t, 1, downloads.Load())
	fullData := string(task.Data)
	// Older snapshots may retain the result URL without an artifact projection.
	require.NoError(t, model.DB.Model(&model.Task{}).Where("id = ?", task.ID).Update("data", "{}").Error)
	response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodGet, path, "")
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.EqualValues(t, 2, downloads.Load())
	before, err := model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	redirect.Store(true)
	finished := make(chan *httptest.ResponseRecorder, 1)
	go func() { finished <- adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodGet, path, "") }()
	select {
	case <-entered:
	case early := <-finished:
		require.FailNowf(t, "content request returned before reaching its redirect", "%d %s", early.Code, early.Body.String())
	}
	admin := createWalletTestAccessToken(t, fixture.sale.CreatedBy, "channel:operate")
	response = adminSecurityRequest(fixture.engine, admin, http.MethodPost, fmt.Sprintf("/api/admin/v1/providers/%d/disable", fixture.channel.ProviderID), "")
	require.Equal(t, 200, response.Code, response.Body.String())
	releaseOnce()
	response = <-finished
	assert.Equal(t, 502, response.Code, response.Body.String())
	assert.Zero(t, redirected.Load(), "fallback redirects must recheck the same original credential")
	require.NoError(t, model.DB.Model(&model.Task{}).Where("id = ?", task.ID).Update("data", fullData).Error)
	response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodGet, path, "")
	assert.Equal(t, 503, response.Code, response.Body.String())
	assert.EqualValues(t, 3, downloads.Load(), "a rejected original Provider must not fall through to a URL without authority")
	task.PrivateData.ResultURL = "data:video/mp4;base64," + base64.StdEncoding.EncodeToString([]byte("synthetic-video"))
	require.NoError(t, model.DB.Model(&model.Task{}).Where("id = ?", task.ID).Update("private_data", task.PrivateData).Error)
	response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodGet, path, "")
	assert.Equal(t, 200, response.Code, response.Body.String())
	assert.Equal(t, "synthetic-video", response.Body.String())
	assert.EqualValues(t, 3, downloads.Load(), "local results require no Provider call")
	after, err := model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	assert.Equal(t, before.Balance, after.Balance)
}

func TestAsyncTaskFirstTailAndReferenceRequireDeclaredInputs(t *testing.T) {
	for _, test := range []struct {
		name, plugin, model, path, body, result, quantity string
		input                                             capability.Modality
	}{
		{"start-end2video", "vidu", "viduq3-turbo", "/vidu/ent/v2/start-end2video", `{"model":"gateway-alias","prompt":"A bird","images":["https://image.invalid/first.png","https://image.invalid/last.png"],"duration":4,"resolution":"720p"}`, `{"task_id":"vidu-job","state":"created"}`, "duration", capability.Image},
		{"reference2video", "vidu", "viduq3-turbo", "/vidu/ent/v2/reference2video", `{"model":"gateway-alias","prompt":"A bird","images":["https://image.invalid/first.png","https://image.invalid/last.png"],"duration":4,"resolution":"720p"}`, `{"task_id":"vidu-job","state":"created"}`, "duration", capability.Image},
		{"jimeng-first-tail", "jimeng", "jimeng_i2v_first_tail_v30", "/jimeng/api/common/v3/submit_task_v2", `{"req_key":"gateway-alias","prompt":"A bird","image_urls":["https://image.invalid/first.png","https://image.invalid/last.png"],"frames":121}`, `{"code":10000,"data":{"task_id":"jimeng-job"}}`, "seconds", capability.Image},
		{"xai-audio-reference", "xai", "grok-imagine-video-1.5", "/xai/v1/videos/generations", `{"model":"gateway-alias","prompt":"A bird","duration":4,"reference_audios":[{"voice_id":"synthetic-voice"}]}`, `{"request_id":"xai-job"}`, "seconds", capability.Audio},
		{"vidu-metadata-prompt", "vidu", "viduq3-turbo", "/v1/tasks/vidu", `{"model":"gateway-alias","images":["https://image.invalid/first.png","https://image.invalid/last.png"],"duration":4,"metadata":{"prompt":"A bird"}}`, `{"task_id":"vidu-job","state":"created"}`, "duration", capability.Text},
		{"jimeng-metadata-prompt", "jimeng", "jimeng_i2v_first_tail_v30", "/v1/tasks/jimeng", `{"model":"gateway-alias","images":["https://image.invalid/first.png","https://image.invalid/last.png"],"duration":5,"metadata":{"prompt":"A bird","frames":121}}`, `{"code":10000,"data":{"task_id":"jimeng-job"}}`, "seconds", capability.Text},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if test.plugin == "jimeng" || test.input == capability.Text {
					var body map[string]any
					require.NoError(t, common.DecodeJson(r.Body, &body))
					if test.plugin == "jimeng" {
						assert.Equal(t, test.model, body["req_key"])
						assert.NotContains(t, body, "model")
					}
					if test.input == capability.Text {
						assert.Equal(t, "A bird", body["prompt"])
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.result))
			})
			source, err := plugins.Source(test.plugin)
			require.NoError(t, err)
			jsplugin.DefaultRegistry = jsplugin.NewRegistry()
			_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: test.plugin})
			require.NoError(t, err)
			initialInput := capability.Text
			if test.input == capability.Text {
				initialInput = capability.Image
			}
			caps := capability.Set{InputModalities: []capability.Modality{initialInput}, OutputModalities: []capability.Modality{capability.Video}}
			modelName := test.model
			if test.plugin == "jimeng" {
				modelName = "jimeng_vgfm_t2v_l20"
			}
			require.NoError(t, model.DB.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Updates(map[string]any{"model_name": modelName, "capabilities": caps}).Error)
			require.NoError(t, model.DB.Model(fixture.channel).Updates(map[string]any{"protocol": "task:" + test.plugin, "upstream_model_name": test.model}).Error)
			if test.plugin == "jimeng" {
				require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("key", "synthetic-ak|synthetic-sk").Error)
			}
			fixture.fake.Advance(time.Second)
			for _, price := range []*model.PriceVersion{
				{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: fmt.Sprintf(`tier("video", u(%q) * 0.1)`, test.quantity), EffectiveAt: clock.Now().Unix()},
				{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: fmt.Sprintf(`tier("video", u(%q) * 0.05)`, test.quantity), EffectiveAt: clock.Now().Unix()},
			} {
				require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
			}
			builder := newPluginGenerationBuilder(fixture.engine.Routes(), nil, productionPluginRouteHandlers)
			require.NoError(t, jsplugin.DefaultRegistry.SetGenerationPreparer(builder.prepare))
			dispatcher := &pluginRouteDispatcher{registry: jsplugin.DefaultRegistry}
			fixture.engine.NoRoute(dispatcher.dispatch)
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, test.path, test.body)
			assert.Equal(t, 400, response.Code, response.Body.String())
			assert.Zero(t, calls.Load())
			var reservations int64
			require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("type = ?", model.LedgerEntryPreConsume).Count(&reservations).Error)
			assert.Zero(t, reservations)
			caps.InputModalities = append(caps.InputModalities, test.input)
			require.NoError(t, model.DB.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Update("capabilities", caps).Error)
			response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, test.path, test.body)
			require.Equal(t, 200, response.Code, response.Body.String())
			assert.EqualValues(t, 1, calls.Load())
			if test.plugin == "jimeng" {
				var task model.Task
				require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Take(&task).Error)
				queryPath, queryBody := "/jimeng/api/common/v3/get_result_v2", fmt.Sprintf(`{"req_key":"gateway-alias","task_id":%q}`, task.TaskID)
				for range 2 {
					response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, queryPath, queryBody)
					require.Equal(t, 200, response.Code, response.Body.String())
				}
				foreign := &model.User{Username: "native-task-foreign", Status: common.UserStatusEnabled, Role: common.RoleCommonUser, Group: "default", AuthVersion: 1}
				require.NoError(t, model.DB.Create(foreign).Error)
				foreignKey := &model.Token{UserId: foreign.Id, Key: "nativetaskforeign", Status: common.TokenStatusEnabled, ExpiredTime: -1}
				require.NoError(t, model.DB.Create(foreignKey).Error)
				response = adminSecurityRequest(fixture.engine, foreignKey.Key, http.MethodPost, queryPath, queryBody)
				assert.Equal(t, 200, response.Code, response.Body.String())
				var unavailable map[string]any
				require.NoError(t, common.Unmarshal(response.Body.Bytes(), &unavailable))
				assert.Equal(t, map[string]any{"status": "not_found"}, unavailable["data"])
				for _, updates := range []map[string]any{{"status": common.TokenStatusDisabled}, {"status": common.TokenStatusEnabled, "expired_time": 1}} {
					require.NoError(t, model.DB.Model(fixture.token).Updates(updates).Error)
					response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, queryPath, queryBody)
					assert.Equal(t, 401, response.Code, response.Body.String())
				}
				require.NoError(t, model.DB.Model(fixture.token).Updates(map[string]any{"status": common.TokenStatusEnabled, "expired_time": -1}).Error)
				assert.EqualValues(t, 1, calls.Load(), "native queries must not resubmit the accepted task")
			}
			if test.input == capability.Image {
				maximum := int64(1)
				caps.Limits.MaxImages = &maximum
				require.NoError(t, model.DB.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Update("capabilities", caps).Error)
				response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, test.path, test.body)
				assert.Equal(t, 400, response.Code, response.Body.String())
				assert.EqualValues(t, 1, calls.Load(), "frame and reference counts must respect the effective image limit")
				caps.Limits.MaxImages = nil
				require.NoError(t, model.DB.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Update("capabilities", caps).Error)
			}
			narrowed := capability.Set{InputModalities: []capability.Modality{initialInput}, OutputModalities: []capability.Modality{capability.Video}}
			require.NoError(t, model.DB.Model(fixture.channel).Update("capabilities", narrowed).Error)
			response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, test.path, test.body)
			assert.Equal(t, 400, response.Code, response.Body.String())
			assert.EqualValues(t, 1, calls.Load(), "a Channel may narrow its Model's input capabilities")
		})
	}
}

func TestAsyncTaskNativeModelCarriersResolveOffering(t *testing.T) {
	for _, test := range []struct {
		plugin, model, path, body, result, quantity string
		output                                      capability.Modality
	}{
		{"vertex-ai", "veo-3.0-generate-001", "/vertex-ai/v1/projects/client/locations/us-central1/publishers/google/models/gateway-alias:predictLongRunning", `{"instances":[{"prompt":"A bird"}],"parameters":{"durationSeconds":4,"resolution":"720p"}}`, `{"name":"projects/provider/locations/us-central1/publishers/google/models/veo-3.0-generate-001/operations/private-job"}`, "seconds", capability.Video},
		{"sunoapi", "suno_music", "/suno/submit/music", `{"prompt":"A bird"}`, `{"code":"success","data":"private-song-job"}`, "clips", capability.Audio},
	} {
		t.Run(test.plugin, func(t *testing.T) {
			var calls atomic.Int32
			fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body map[string]any
				require.NoError(t, common.DecodeJson(r.Body, &body))
				assert.NotContains(t, body, "model")
				if test.plugin == "vertex-ai" {
					assert.Contains(t, r.URL.Path, "/models/"+test.model+":predictLongRunning")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.result))
			})
			source, err := plugins.Source(test.plugin)
			require.NoError(t, err)
			jsplugin.DefaultRegistry = jsplugin.NewRegistry()
			_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: test.plugin})
			require.NoError(t, err)
			require.NoError(t, model.DB.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Updates(map[string]any{"model_name": test.model, "capabilities": capability.Set{InputModalities: []capability.Modality{capability.Text}, OutputModalities: []capability.Modality{test.output}}}).Error)
			require.NoError(t, model.DB.Model(fixture.channel).Updates(map[string]any{"protocol": "task:" + test.plugin, "upstream_model_name": test.model}).Error)
			if test.plugin == "vertex-ai" {
				require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("type", constant.ChannelTypeNewAPI).Error)
			}
			if test.plugin == "sunoapi" {
				require.NoError(t, model.DB.Create(&model.ModelOfferingName{Name: test.model, OfferingID: fixture.offering.ID}).Error)
			}
			fixture.fake.Advance(time.Second)
			for _, price := range []*model.PriceVersion{
				{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: fmt.Sprintf(`tier("task",u(%q)*0.1)`, test.quantity), EffectiveAt: clock.Now().Unix()},
				{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: fmt.Sprintf(`tier("task",u(%q)*0.05)`, test.quantity), EffectiveAt: clock.Now().Unix()},
			} {
				require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
			}
			builder := newPluginGenerationBuilder(fixture.engine.Routes(), nil, productionPluginRouteHandlers)
			require.NoError(t, jsplugin.DefaultRegistry.SetGenerationPreparer(builder.prepare))
			dispatcher := &pluginRouteDispatcher{registry: jsplugin.DefaultRegistry}
			fixture.engine.NoRoute(dispatcher.dispatch)
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, test.path, test.body)
			require.Equal(t, 200, response.Code, response.Body.String())
			assert.EqualValues(t, 1, calls.Load())
			var reservations int64
			require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("type = ?", model.LedgerEntryPreConsume).Count(&reservations).Error)
			assert.EqualValues(t, 1, reservations)
		})
	}
}

func TestAsyncTaskNativeLegacySubmissionStillRequiresLegacyQuota(t *testing.T) {
	var calls atomic.Int32
	fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"id":"unexpected"}`))
	})
	source, err := plugins.Source("doubao")
	require.NoError(t, err)
	jsplugin.DefaultRegistry = jsplugin.NewRegistry()
	_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "doubao"})
	require.NoError(t, err)
	builder := newPluginGenerationBuilder(fixture.engine.Routes(), nil, productionPluginRouteHandlers)
	require.NoError(t, jsplugin.DefaultRegistry.SetGenerationPreparer(builder.prepare))
	dispatcher := &pluginRouteDispatcher{registry: jsplugin.DefaultRegistry}
	fixture.engine.NoRoute(dispatcher.dispatch)
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/doubao/api/v3/contents/generations/tasks", `{"model":"doubao-seedance-1-0-pro-250528","content":[{"type":"text","text":"A bird"}]}`)
	assert.Equal(t, 401, response.Code, response.Body.String())
	assert.Zero(t, calls.Load())
	var reservations int64
	require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("type = ?", model.LedgerEntryPreConsume).Count(&reservations).Error)
	assert.Zero(t, reservations)
}

func TestAsyncTaskCapabilitiesMustBeKnownAndPureBeforeAdmission(t *testing.T) {
	for _, test := range []struct{ name, hook string }{
		{"missing", ""},
		{"unknown-modality", `export function extractCapabilities() { return {input_modalities:["opaque"],output_modalities:["video"]}; }`},
		{"missing-input", `export function extractCapabilities() { return {output_modalities:["video"]}; }`},
		{"unknown-image-count", `export function extractCapabilities(ctx) { const facts=ignoredCapabilities(ctx); delete facts.image_count; return facts; }`},
		{"provider-io", `export function extractCapabilities(ctx) { utils.fetch({url:ctx.baseUrl+"/preflight",method:"GET"}); return {input_modalities:["text"],output_modalities:["video"]}; }`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = w.Write([]byte(`{"id":"unexpected"}`))
			})
			source, err := plugins.Source("sora")
			require.NoError(t, err)
			updated := strings.Replace(source, "export function extractCapabilities(ctx)", "function ignoredCapabilities(ctx)", 1)
			require.NotEqual(t, source, updated)
			jsplugin.DefaultRegistry = jsplugin.NewRegistry()
			_, err = jsplugin.DefaultRegistry.RegisterFactory(updated+"\n"+test.hook, jsplugin.Options{Key: "sora"})
			require.NoError(t, err)
			body := `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`
			if test.name == "unknown-image-count" {
				var unified model.Model
				require.NoError(t, model.DB.First(&unified, fixture.channel.ModelID).Error)
				maximum := int64(1)
				unified.Capabilities.Limits.MaxImages = &maximum
				require.NoError(t, model.DB.Model(&unified).Update("capabilities", unified.Capabilities).Error)
				body = `{"model":"gateway-alias","prompt":"A bird","seconds":"4","input_reference":"https://image.invalid/first.png"}`
			}
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", body)
			assert.Equal(t, 400, response.Code, response.Body.String())
			assert.Zero(t, calls.Load())
			var reservations int64
			require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("type = ?", model.LedgerEntryPreConsume).Count(&reservations).Error)
			assert.Zero(t, reservations)
		})
	}
}

func TestAsyncTaskRouteEnforcesTokenFactsAndOutputModality(t *testing.T) {
	for _, tokenLimited := range []bool{false, true} {
		t.Run(fmt.Sprintf("token_limited_%t", tokenLimited), func(t *testing.T) {
			var calls atomic.Int32
			fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = w.Write([]byte(`{"id":"guarded-job","status":"queued"}`))
			})
			tokenSource := ""
			if tokenLimited {
				source, err := plugins.Source("sora")
				require.NoError(t, err)
				source = strings.Replace(source, "usageSchema: {", `usageExamples: [{label: "Synthetic task", facts: {seconds: 4, size: "720x1280", tokens: 20}}], usageSchema: {tokens: {type:"number", unit:"token", description:{en:"Generation token unit price", zh:"生成 token 单价"}},`, 1)
				source = strings.Replace(source, `return { seconds: videoDuration(req), size: req.size || "720x1280" };`, `return { seconds: videoDuration(req), size: req.size || "720x1280", tokens: 20 };`, 1)
				jsplugin.DefaultRegistry = jsplugin.NewRegistry()
				_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "sora"})
				require.NoError(t, err)
				tokenSource = source
				require.NoError(t, model.DB.Model(fixture.channel).Update("tpm", 10).Error)
			} else {
				var unified model.Model
				require.NoError(t, model.DB.First(&unified, fixture.channel.ModelID).Error)
				unified.Capabilities.OutputModalities = []capability.Modality{capability.Text}
				require.NoError(t, model.DB.Model(&unified).Update("capabilities", unified.Capabilities).Error)
			}
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/videos", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
			want := 400
			if tokenLimited {
				want = 503
			}
			assert.Equal(t, want, response.Code, response.Body.String())
			assert.Zero(t, calls.Load())
			var reservations int64
			require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("type = ?", model.LedgerEntryPreConsume).Count(&reservations).Error)
			assert.Zero(t, reservations)
			if tokenLimited {
				require.NoError(t, model.DB.Model(fixture.channel).Update("tpm", 20).Error)
				tokenSource = strings.Replace(tokenSource, "return { taskId, taskData: body };", `return {taskId, taskData:body, immediate:{status:"SUCCESS"}};`, 1)
				tokenSource = strings.Replace(tokenSource, "const data = body || {};", "const data = body || {}; facts.tokens = 0;", 1)
				jsplugin.DefaultRegistry = jsplugin.NewRegistry()
				_, err := jsplugin.DefaultRegistry.RegisterFactory(tokenSource, jsplugin.Options{Key: "sora"})
				require.NoError(t, err)
				for range 2 {
					response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/videos", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
					require.Equal(t, 200, response.Code, response.Body.String())
				}
				assert.EqualValues(t, 2, calls.Load(), "reported token zero must replace the forecast on the same permit")
			}
		})
	}
}

func TestAsyncTaskImmediateFailureKeepsReportedProcurement(t *testing.T) {
	fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"failed-job","status":"failed","seconds":"2"}`))
	})
	source, err := plugins.Source("sora")
	require.NoError(t, err)
	source = strings.Replace(source, "return { taskId, taskData: body };", `return { taskId, taskData: body, immediate: {status: "FAILURE"} };`, 1)
	jsplugin.DefaultRegistry = jsplugin.NewRegistry()
	_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "sora"})
	require.NoError(t, err)
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
	require.Equal(t, 200, response.Code, response.Body.String())
	requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	assert.Zero(t, requests[0].Receivable.Nanos)
	assert.False(t, requests[0].SalePending)
	attempts, total, err := model.ListGatewayAttempts(t.Context(), requests[0].ID, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	assert.False(t, attempts[0].CostPending)
	assert.EqualValues(t, 100000000, attempts[0].Cost.Nanos)
	wallet, err := model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	assert.EqualValues(t, 1000000000, wallet.Balance.Nanos)
}

func TestAsyncTaskChargedXaiFailureDoesNotChargeUser(t *testing.T) {
	failure := map[string]any{"status": "failed", "usage": map[string]any{"cost_in_usd_ticks": 1}, "error": map[string]any{"message": "generation failed"}}
	fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_, _ = io.WriteString(w, `{"request_id":"charged-failure-job"}`)
			return
		}
		encoded, err := common.Marshal(failure)
		if assert.NoError(t, err) {
			_, _ = w.Write(encoded)
		}
	})
	source, err := plugins.Source("xai")
	require.NoError(t, err)
	jsplugin.DefaultRegistry = jsplugin.NewRegistry()
	loaded, err := jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "xai"})
	require.NoError(t, err)
	legacy, err := loaded.Engine.Call(t.Context(), "parseTaskResult", map[string]any{}, failure, map[string]any{"status": 200})
	require.NoError(t, err)
	legacyResult, ok := legacy.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "SUCCESS", legacyResult["status"], "legacy quota compatibility retains its existing charged-failure outcome")
	require.NoError(t, model.DB.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Update("model_name", "grok-imagine-video-1.5").Error)
	require.NoError(t, model.DB.Model(fixture.channel).Updates(map[string]any{"protocol": "task:xai", "upstream_model_name": "grok-imagine-video-1.5"}).Error)
	fixture.fake.Advance(time.Second)
	for _, price := range []*model.PriceVersion{
		{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: `tier("video", u("seconds") * 0.1)`, EffectiveAt: clock.Now().Unix()},
		{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: `tier("video", u("seconds") * 0.05)`, EffectiveAt: clock.Now().Unix()},
	} {
		require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
	}
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/xai", `{"model":"gateway-alias","prompt":"A bird","duration":4}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	service.RunTaskPollingOnce(t.Context(), nil)
	var task model.Task
	require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Take(&task).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), task.Status)
	assert.Equal(t, true, task.PrivateData.Gateway.CompletionFacts["moderated"])
	request, err := model.GetGatewayRequest(t.Context(), response.Header().Get(common.RequestIdKey), fixture.user.Id)
	require.NoError(t, err)
	assert.Zero(t, request.Receivable.Nanos)
	assert.False(t, request.SalePending)
	attempts, total, err := model.ListGatewayAttempts(t.Context(), request.ID, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	assert.True(t, attempts[0].CostPending, "a charged failure without a reported duration must retain unknown procurement")
	wallet, err := model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	assert.EqualValues(t, 1000000000, wallet.Balance.Nanos)
}

func TestAsyncTaskPollingRechecksProviderAfterPluginPreparation(t *testing.T) {
	for _, selectedSlot := range []bool{false, true} {
		t.Run(fmt.Sprintf("selected_slot_%t", selectedSlot), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var queryCalls atomic.Int32
			fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/poll-prepare" {
					close(entered)
					<-release
					_, _ = w.Write([]byte(`{"ready":true}`))
					return
				}
				if r.Method == http.MethodGet {
					queryCalls.Add(1)
					_, _ = w.Write([]byte(`{"id":"guarded-job","status":"completed","seconds":"2"}`))
					return
				}
				_, _ = w.Write([]byte(`{"id":"guarded-job","status":"queued"}`))
			})
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			source, err := plugins.Source("sora")
			require.NoError(t, err)
			source = strings.Replace(source, "export function buildQueryRequest(ctx) {", `export function buildQueryRequest(ctx) {
 utils.fetch({url: ctx.baseUrl + "/poll-prepare"});`, 1)
			jsplugin.DefaultRegistry = jsplugin.NewRegistry()
			_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "sora"})
			require.NoError(t, err)
			if selectedSlot {
				provider, err := model.GetProviderById(fixture.channel.ProviderID, true)
				require.NoError(t, err)
				provider.Key = "provider-secret\nprovider-secret"
				provider.Keys = nil
				provider.ProviderInfo.IsMultiKey = true
				provider.ProviderInfo.MultiKeySize = 2
				provider.ProviderInfo.MultiKeyMode = constant.MultiKeyModePolling
				provider.ProviderInfo.MultiKeyPollingIndex = 0
				require.NoError(t, model.DB.Save(provider).Error)
			}
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
			require.Equal(t, 200, response.Code, response.Body.String())
			finished := make(chan struct{})
			go func() { service.RunTaskPollingOnce(t.Context(), nil); close(finished) }()
			select {
			case <-entered:
			case <-finished:
				t.Fatal("polling finished before preparation")
			case <-t.Context().Done():
				t.Fatal(t.Context().Err())
			}
			admin := createWalletTestAccessToken(t, fixture.sale.CreatedBy, "channel:operate")
			path := fmt.Sprintf("/api/admin/v1/providers/%d/disable", fixture.channel.ProviderID)
			if selectedSlot {
				path = fmt.Sprintf("/api/admin/v1/providers/%d/keys/1/disable", fixture.channel.ProviderID)
			}
			response = adminSecurityRequest(fixture.engine, admin, http.MethodPost, path, "")
			require.Equal(t, 200, response.Code, response.Body.String())
			once.Do(func() { close(release) })
			<-finished
			assert.Zero(t, queryCalls.Load(), "a committed Provider disable must prevent the prepared poll from using its old credential")
			var task model.Task
			require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Take(&task).Error)
			assert.NotEqual(t, model.TaskStatusSuccess, task.Status)
			assert.True(t, task.PrivateData.Gateway.Accepted)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			assert.EqualValues(t, 640000000, wallet.Balance.Nanos)
		})
	}
}

func TestAsyncTaskRouteDefersPluginFetchUntilAfterReservation(t *testing.T) {
	var preparationCalls, submissionCalls atomic.Int32
	fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/prepare" {
			preparationCalls.Add(1)
			var reservations int64
			require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("type = ?", model.LedgerEntryPreConsume).Count(&reservations).Error)
			assert.EqualValues(t, 1, reservations)
			_, _ = w.Write([]byte(`{"ready":true}`))
			return
		}
		submissionCalls.Add(1)
		_, _ = w.Write([]byte(`{"id":"prepared-job","status":"queued"}`))
	})
	source, err := plugins.Source("sora")
	require.NoError(t, err)
	source = strings.Replace(source, "export function buildSubmitRequest(ctx) {", `export function buildSubmitRequest(ctx) {
 utils.fetch({url: ctx.baseUrl + "/prepare"});`, 1)
	jsplugin.DefaultRegistry = jsplugin.NewRegistry()
	_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "sora"})
	require.NoError(t, err)
	_, err = model.PostLedgerEntries(model.LedgerPosting{Account: model.UserWalletAccount(fixture.user.Id), Type: model.LedgerEntryAdjustment, Amount: money.Amount{Nanos: -1000000000, Currency: "USD"}, IdempotencyKey: "task-fetch-empty-wallet"})
	require.NoError(t, err)
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
	assert.Equal(t, 402, response.Code, response.Body.String())
	assert.Zero(t, preparationCalls.Load())
	assert.Zero(t, submissionCalls.Load())
	_, err = model.TopUpWallet(model.WalletTopUp{UserId: fixture.user.Id, Source: money.Amount{Nanos: 1000000000, Currency: "USD"}, Reference: "task-fetch-fund"})
	require.NoError(t, err)
	response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.EqualValues(t, 1, preparationCalls.Load())
	assert.EqualValues(t, 1, submissionCalls.Load())
}

func TestAsyncTaskRouteEnforcesDurationAndConcurrentAdmission(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		t.Run(fmt.Sprintf("concurrent_%t", concurrent), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 && concurrent {
					close(entered)
					<-release
				}
				_, _ = w.Write([]byte(`{"id":"admitted-job","status":"queued"}`))
			})
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			if !concurrent {
				var unified model.Model
				require.NoError(t, model.DB.First(&unified, fixture.channel.ModelID).Error)
				limit := int64(2)
				unified.Capabilities.Limits.MaxDurationSeconds = &limit
				require.NoError(t, model.DB.Model(&unified).Update("capabilities", unified.Capabilities).Error)
				response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
				assert.Equal(t, 400, response.Code, response.Body.String())
				assert.Zero(t, calls.Load())
				var reserved int64
				require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("type = ?", model.LedgerEntryPreConsume).Count(&reserved).Error)
				assert.Zero(t, reserved)
				return
			}
			require.NoError(t, model.DB.Model(fixture.channel).Update("max_concurrency", 1).Error)
			finished := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				finished <- adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
			}()
			select {
			case <-entered:
			case response := <-finished:
				t.Fatalf("submission returned before Provider was entered: %d %s", response.Code, response.Body.String())
			case <-t.Context().Done():
				t.Fatal(t.Context().Err())
			}
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"Another bird","seconds":"4"}`)
			assert.Equal(t, 503, response.Code, response.Body.String())
			assert.EqualValues(t, 1, calls.Load())
			once.Do(func() { close(release) })
			response = <-finished
			require.Equal(t, 200, response.Code, response.Body.String())
			response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"Later bird","seconds":"4"}`)
			require.Equal(t, 200, response.Code, response.Body.String())
			assert.EqualValues(t, 2, calls.Load(), "the selected permit must be released once submission is accepted")
		})
	}
}

func TestAsyncTaskRouteUsesEffectivePurchaseCostAndProfit(t *testing.T) {
	for _, tc := range []struct {
		allowLoss, secondPlugin       bool
		firstReject, firstUnsupported bool
		path                          string
	}{{path: "/v1/tasks/sora"}, {allowLoss: true, path: "/v1/tasks/sora"}, {secondPlugin: true, path: "/v1/videos"}, {secondPlugin: true, path: "/v1/responses"}, {secondPlugin: true, firstReject: true, path: "/v1/responses"}, {secondPlugin: true, firstUnsupported: true, path: "/v1/responses"}} {
		t.Run(fmt.Sprintf("allow_loss_%t_second_plugin_%t_reject_%t_unsupported_%t%s", tc.allowLoss, tc.secondPlugin, tc.firstReject, tc.firstUnsupported, tc.path), func(t *testing.T) {
			firstCalls, secondCalls := 0, 0
			fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				firstCalls++
				if tc.firstReject {
					w.WriteHeader(503)
					_, _ = w.Write([]byte(`{"error":{"code":"overloaded"}}`))
					return
				}
				_, _ = w.Write([]byte(`{"id":"expensive-job","status":"queued"}`))
			})
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				secondCalls++
				_, _ = w.Write([]byte(`{"id":"cheap-job","status":"queued"}`))
			}))
			t.Cleanup(second.Close)
			provider := &model.Provider{Name: "Cheap task Provider", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Key: "cheap-secret", BaseURL: &second.URL}
			require.NoError(t, model.DB.Create(provider).Error)
			channelProtocol := "task:sora"
			if tc.secondPlugin {
				source, err := plugins.Source("sora")
				require.NoError(t, err)
				if tc.firstUnsupported {
					jsplugin.DefaultRegistry = jsplugin.NewRegistry()
					narrowed := strings.Replace(source, `supports: ["stream", "sync", "background"]`, `supports: ["stream", "sync"]`, 1)
					_, err = jsplugin.DefaultRegistry.RegisterFactory(narrowed, jsplugin.Options{Key: "sora"})
					require.NoError(t, err)
				}
				source = strings.Replace(source, `key: "sora"`, `key: "cheapsora"`, 1)
				source = strings.Replace(source, `channelTypes: [55, 1],`, `channelTypes: [],`, 1)
				_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "cheapsora"})
				require.NoError(t, err)
				channelProtocol = "task:cheapsora"
			}
			channel := &model.Channel{ProviderID: provider.Id, ModelID: fixture.channel.ModelID, Protocol: protocol.Type(channelProtocol), UpstreamModelName: "sora-2", MappingConfirmed: true, Status: model.ChannelEnabled, Weight: 1, ParameterOverrides: `{}`}
			require.NoError(t, model.DB.Create(channel).Error)
			fixture.fake.Advance(time.Second)
			firstPrice := `tier("video", u("seconds") * 0.2)`
			if tc.firstReject {
				firstPrice = `tier("video", u("seconds") * 0.001)`
			}
			for _, price := range []*model.PriceVersion{
				{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: firstPrice, EffectiveAt: clock.Now().Unix()},
				{OwnerType: model.PriceOwnerChannel, OwnerID: channel.ID, Currency: "USD", Expression: `tier("video", u("seconds") * 0.01)`, EffectiveAt: clock.Now().Unix()},
			} {
				require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
			}
			require.NoError(t, model.DB.Model(fixture.offering).Updates(map[string]any{"route_method": "cost", "allow_loss_fallback": tc.allowLoss}).Error)
			body := `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`
			if tc.path == "/v1/responses" {
				body = `{"model":"gateway-alias","input":"A bird","background":true,"seconds":"4"}`
			}
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, tc.path, body)
			require.Equal(t, 200, response.Code, response.Body.String())
			assert.EqualValues(t, map[bool]int{false: 0, true: 1}[tc.firstReject], firstCalls, "Route must exclude lossmaking candidates and try a cheaper valid candidate only once")
			assert.Equal(t, 1, secondCalls)
			if tc.secondPlugin {
				var task model.Task
				require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Take(&task).Error)
				assert.Equal(t, constant.TaskPlatform("cheapsora"), task.Platform)
				require.NotNil(t, task.PrivateData.Execution)
				require.NotNil(t, task.PrivateData.Execution.TaskPlugin)
				assert.Equal(t, "cheapsora", task.PrivateData.Execution.TaskPlugin.Key)
			}
			requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			attempts, total, err := model.ListGatewayAttempts(t.Context(), requests[0].ID, 0, 10)
			require.NoError(t, err)
			wantedAttempts := 1
			if tc.firstReject {
				wantedAttempts = 2
			}
			require.EqualValues(t, wantedAttempts, total)
			index := slices.IndexFunc(attempts, func(attempt model.GatewayAttempt) bool { return attempt.ChannelID == channel.ID })
			require.NotEqual(t, -1, index)
			assert.Equal(t, "cost", attempts[index].RouteDiagnostics["method"])
			var entries []model.LedgerEntry
			require.NoError(t, model.DB.Where("request_id = ? AND type = ?", requests[0].ID, model.LedgerEntryPreConsume).Find(&entries).Error)
			require.Len(t, entries, 1)
			assert.EqualValues(t, -360000000, entries[0].Amount.Nanos)
		})
	}
}

func TestAsyncTaskHealthAdmissionSkipsCoolingChannel(t *testing.T) {
	firstCalls := 0
	fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
		firstCalls++
		w.Header().Set("Retry-After", "172800")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limit_exceeded"}}`))
	})
	secondCalls := 0
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"healthy-job-%d","status":"queued"}`, secondCalls)
	}))
	t.Cleanup(second.Close)
	provider := &model.Provider{Name: "Healthy task Provider", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Key: "healthy-secret", BaseURL: &second.URL}
	require.NoError(t, model.DB.Create(provider).Error)
	channel := &model.Channel{ProviderID: provider.Id, ModelID: fixture.channel.ModelID, Protocol: "task:sora", UpstreamModelName: "sora-2", MappingConfirmed: true, Status: model.ChannelEnabled, Weight: 1, ParameterOverrides: `{}`}
	require.NoError(t, model.DB.Create(channel).Error)
	require.NoError(t, model.CreatePriceVersion(t.Context(), &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: channel.ID, Currency: "USD", Expression: `tier("video", u("seconds") * 0.05)`, EffectiveAt: clock.Now().Unix()}, fixture.sale.CreatedBy))
	for range 2 {
		response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
		require.Equal(t, 200, response.Code, response.Body.String())
	}
	assert.Equal(t, 1, firstCalls, "a cooling Channel must not be submitted again while a healthy Channel exists")
	assert.Equal(t, 2, secondCalls)
}

func TestAsyncTaskRejectedAttemptsPreserveErrorsAndUnknownCosts(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{{400, "invalid_request_error"}, {413, "invalid_request_error"}, {422, "invalid_request_error"}, {401, "invalid_api_key"}, {429, "rate_limit_exceeded"}, {400, "insufficient_quota"}, {503, "overloaded"}} {
		t.Run(fmt.Sprintf("%d_%s", tc.status, tc.code), func(t *testing.T) {
			fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "172800")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"message":"private-provider-error-secret"}}`, tc.code)
			})
			previous := common.AutomaticDisableChannelEnabled
			fixture.fake.Advance(time.Second)
			require.NoError(t, model.CreatePriceVersion(t.Context(), &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: `tier("video", u("seconds") * 0.05) * (has(header("x-purchase-band"), "private") ? 2 : 1)`, EffectiveAt: clock.Now().Unix()}, fixture.sale.CreatedBy))
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("header_override", `{"X-Purchase-Band":"private-price-band-secret"}`).Error)
			common.AutomaticDisableChannelEnabled = true
			t.Cleanup(func() { common.AutomaticDisableChannelEnabled = previous })
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("auto_ban", 1).Error)
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
			assert.Equal(t, tc.status, response.Code, response.Body.String())
			assert.NotContains(t, response.Body.String(), "private-provider-error-secret")
			requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.Zero(t, requests[0].Receivable.Nanos)
			assert.False(t, requests[0].SalePending)
			assert.Equal(t, "settlement_pending", requests[0].Status)
			attempts, total, err := model.ListGatewayAttempts(t.Context(), requests[0].ID, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.True(t, attempts[0].CostPending)
			assert.True(t, attempts[0].ConvertedCostPending)
			var billing model.GatewayBilling
			require.NoError(t, model.DB.Where("request_id = ?", requests[0].ID).Take(&billing).Error)
			var intent model.GatewaySettlement
			require.NoError(t, common.UnmarshalJsonStr(billing.Settlement, &intent))
			trace := intent.AttemptTrace[attempts[0].ID]
			require.NotNil(t, trace.RequestSnapshot, "unknown procurement must retain the safe final-wire conditions for reconciliation")
			assert.Equal(t, billingexpr.ExprHashString(attempts[0].PurchaseExpression), trace.RequestSnapshot.ExpressionHash)
			assert.NotEmpty(t, trace.RequestSnapshot.Conditions)
			assert.Empty(t, trace.UnroundedAmount, "an estimated amount must not become the unknown actual cost")
			assert.NotContains(t, billing.Settlement, "private-price-band-secret")
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			assert.EqualValues(t, 1000000000, wallet.Balance.Nanos)
			if tc.status == 401 || tc.code == "insufficient_quota" {
				var provider model.Provider
				require.NoError(t, model.DB.First(&provider, fixture.channel.ProviderID).Error)
				assert.Equal(t, common.ChannelStatusAutoDisabled, provider.Status)
				audits, _, err := model.GetAuditLogs(model.AuditLogFilter{UserId: fixture.user.Id, Category: model.AuditCategorySecurity}, 0, 100, common.RoleRootUser)
				require.NoError(t, err)
				require.Len(t, audits, 1)
				assert.Equal(t, "provider.auto_disable", audits[0].Action)
				assert.True(t, audits[0].Success)
				encoded, err := common.Marshal(audits)
				require.NoError(t, err)
				assert.NotContains(t, string(encoded), "provider-secret")
				assert.NotContains(t, string(encoded), "private-provider-error-secret")
			}
			if tc.status == 429 {
				details := adminSecurityRequest(fixture.engine, fixture.adminPAT, http.MethodGet, fmt.Sprintf("/api/admin/v1/channels/%d", fixture.channel.ID), "")
				require.Equal(t, 200, details.Code)
				var view struct {
					Data struct {
						Health channelhealth.Snapshot `json:"health"`
					} `json:"data"`
				}
				require.NoError(t, common.Unmarshal(details.Body.Bytes(), &view))
				assert.Equal(t, "open", view.Data.Health.Circuit)
				assert.Equal(t, fixture.fake.Now().Add(48*time.Hour).UnixMilli(), view.Data.Health.RetryAt)
			}
		})
	}
}

func TestAsyncTaskPollingKeepsOtherProvidersAndLegacyProgressing(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_, _ = io.WriteString(w, `{"id":"slow-poll-job","status":"queued"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	first := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	fixture.fake.Advance(time.Second)
	peer := addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_, _ = io.WriteString(w, `{"id":"ready-poll-job","status":"queued"}`)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/v1/videos/")
		_, _ = fmt.Fprintf(w, `{"id":%q,"status":"completed","seconds":"2"}`, id)
	})
	require.NoError(t, model.DB.Model(peer).Updates(map[string]any{"protocol": "task:sora", "upstream_model_name": "sora-2"}).Error)
	require.NoError(t, model.CreatePriceVersion(t.Context(), &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: peer.ID, Currency: "USD", Expression: `tier("video", u("seconds") * 0.05)`, EffectiveAt: clock.Now().Unix()}, fixture.sale.CreatedBy))
	require.NoError(t, model.DB.Model(fixture.channel).Update("status", model.ChannelDisabled).Error)
	second := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	var ready model.Task
	require.NoError(t, model.DB.Where("user_id = ? AND channel_id = ?", fixture.user.Id, peer.ProviderID).Take(&ready).Error)
	legacy := &model.Task{TaskID: "legacy-poll-progress", Platform: ready.Platform, UserId: fixture.user.Id, Group: ready.Group, ChannelId: ready.ChannelId, Action: ready.Action, Status: model.TaskStatusSubmitted, SubmitTime: clock.Now().Unix(), Properties: ready.Properties, PrivateData: model.TaskPrivateData{UpstreamTaskID: "legacy-poll-job"}}
	require.NoError(t, model.DB.Create(legacy).Error)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { service.RunTaskPollingOnce(ctx, nil); close(done) }()
	defer func() { cancel(); <-done }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "polling did not reach the controlled slow Provider")
	}
	assert.Eventually(t, func() bool {
		var completed int64
		err := model.DB.Model(&model.Task{}).Where("id IN ? AND status = ?", []int64{ready.ID, legacy.ID}, model.TaskStatusSuccess).Count(&completed).Error
		return err == nil && completed == 2
	}, 2*time.Second, 10*time.Millisecond, "another Provider's accepted task and legacy task must complete while the slow Provider is still blocked")
}

func TestAsyncTaskPollingHonorsCallerCancellation(t *testing.T) {
	for _, test := range []struct{ hook, batch bool }{{}, {hook: true}, {batch: true}, {hook: true, batch: true}} {
		t.Run(fmt.Sprintf("parse_hook=%t/batch=%t", test.hook, test.batch), func(t *testing.T) {
			started, stopped, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost {
					_, _ = io.WriteString(w, `{"id":"cancel-poll-job","status":"queued"}`)
					return
				}
				if !test.hook || r.URL.Path == "/parse-hold" {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					close(started)
					select {
					case <-r.Context().Done():
					case <-release:
					}
					close(stopped)
				}
				_, _ = io.WriteString(w, `{"id":"cancel-poll-job","status":"completed","seconds":"2"}`)
			})
			if test.hook || test.batch {
				source, err := plugins.Source("sora")
				require.NoError(t, err)
				if test.batch {
					// Add synthetic v1 batch hooks to exercise the host lifecycle.
					anchor := `fetchMode: "per_task"`
					require.Contains(t, source, anchor)
					source = strings.Replace(source, anchor, `fetchMode: "batch"`, 1)
					source += `
export function buildBatchQueryRequest(ctx,tasks) { return {url:ctx.baseUrl+"/v1/videos/"+tasks[0].taskId,method:"GET"}; }
export function parseBatchResult(ctx,body) {`
					if test.hook {
						source += `utils.fetch({url:ctx.baseUrl+"/parse-hold",method:"GET"});`
					}
					source += `return [{taskId:ctx.tasks[0].taskId,status:"SUCCESS",data:body}]; }`
				} else {
					anchor := "export function parseTaskResult(ctx, body) {"
					require.Contains(t, source, anchor)
					source = strings.Replace(source, anchor, anchor+` utils.fetch({url:ctx.baseUrl+"/parse-hold",method:"GET"});`, 1)
				}
				jsplugin.DefaultRegistry = jsplugin.NewRegistry()
				_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "sora"})
				require.NoError(t, err)
			}
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var task model.Task
			require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Take(&task).Error)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- service.UpdateGatewayTask(ctx, &task) }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				require.FailNow(t, "poll did not reach the controlled Provider read")
			}
			cancel()
			select {
			case err := <-done:
				assert.ErrorIs(t, err, context.Canceled)
			case <-time.After(time.Second):
				assert.Fail(t, "caller cancellation did not stop in-flight task polling")
				releaseOnce.Do(func() { close(release) })
				<-done
			}
			select {
			case <-stopped:
			case <-time.After(time.Second):
				assert.Fail(t, "caller cancellation did not reach the Provider connection")
			}
			require.NoError(t, model.DB.First(&task, task.ID).Error)
			assert.True(t, task.PrivateData.Gateway.Accepted)
			assert.Zero(t, task.PrivateData.PollFailures)
		})
	}
}

func TestAsyncTaskModelFallbackFinalTierDeterminesStatus(t *testing.T) {
	for _, test := range []struct {
		name, code   string
		tier, status int
		disabled     bool
	}{
		{name: "same tier retains upstream rejection", tier: 1, disabled: true, status: http.StatusUnauthorized, code: "upstream_error"},
		{name: "later tier unavailable", tier: 2, disabled: true, status: http.StatusServiceUnavailable, code: "channel_unavailable"},
		{name: "later tier capability mismatch", tier: 2, status: http.StatusBadRequest, code: "unsupported_parameter"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls, lowerCalls atomic.Int32
			fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":{"code":"invalid_api_key","message":"private-provider-error-secret"}}`)
			})
			fixture.fake.Advance(time.Second)
			lower := addTextGatewayChannel(t, fixture, func(w http.ResponseWriter, r *http.Request) { lowerCalls.Add(1) })
			var source model.Model
			require.NoError(t, model.DB.First(&source, fixture.channel.ModelID).Error)
			member := &model.Model{ModelName: fmt.Sprintf("fallback-%d", lower.ID), Status: 1, NameRule: model.NameRuleExact, Capabilities: source.Capabilities}
			require.NoError(t, model.DB.Create(member).Error)
			require.NoError(t, model.DB.Model(lower).Updates(map[string]any{"protocol": "task:sora", "upstream_model_name": "sora-2-pro", "model_id": member.Id}).Error)
			require.NoError(t, model.CreatePriceVersion(t.Context(), &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: lower.ID, Currency: "USD", Expression: `tier("video", u("seconds") * 0.05)`, EffectiveAt: clock.Now().Unix()}, fixture.sale.CreatedBy))
			fixture.offering.Members = append(fixture.offering.Members, model.ModelOfferingMember{ModelID: member.Id, Tier: test.tier})
			require.NoError(t, model.SaveModelOffering(t.Context(), fixture.offering, "save"))
			if test.disabled {
				require.NoError(t, model.DB.Model(lower).Update("status", model.ChannelDisabled).Error)
			} else {
				caps := capability.Set{InputModalities: []capability.Modality{capability.Image}, OutputModalities: []capability.Modality{capability.Video}}
				require.NoError(t, model.DB.Model(&model.Model{}).Where("id = ?", lower.ModelID).Update("capabilities", caps).Error)
			}
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
			assert.Equal(t, test.status, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), `"code":"`+test.code+`"`)
			assert.NotContains(t, response.Body.String(), "private-provider-error-secret")
			assert.EqualValues(t, 1, calls.Load())
			assert.Zero(t, lowerCalls.Load())
			attempts, total, err := model.ListGatewayAttempts(t.Context(), response.Header().Get(common.RequestIdKey), 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.Equal(t, fixture.channel.ID, attempts[0].ChannelID)
			assert.Equal(t, "credentials", attempts[0].ErrorClass)
			assert.True(t, attempts[0].CostPending)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			assert.EqualValues(t, 1000000000, wallet.Balance.Nanos)
		})
	}
}

func TestAsyncTaskChannelOverridesChangeWireAndBillingFacts(t *testing.T) {
	for _, path := range []string{"/v1/tasks/sora", "/v1/videos"} {
		t.Run(path, func(t *testing.T) {
			fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					_, _ = w.Write([]byte(`{"id":"override-job","status":"completed"}`))
					return
				}
				var body map[string]any
				require.NoError(t, common.DecodeJson(r.Body, &body))
				assert.Equal(t, "8", body["seconds"])
				_, _ = w.Write([]byte(`{"id":"override-job","status":"queued"}`))
			})
			require.NoError(t, model.DB.Model(fixture.channel).Update("parameter_overrides", `{"seconds":"8"}`).Error)
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, path, `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
			require.Equal(t, 200, response.Code, response.Body.String())
			var task model.Task
			require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Take(&task).Error)
			assert.EqualValues(t, 8, task.PrivateData.Gateway.Facts["seconds"])
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			assert.EqualValues(t, 280000000, wallet.Balance.Nanos)
			service.RunTaskPollingOnce(t.Context(), nil)
			requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.EqualValues(t, 720000000, requests[0].Receivable.Nanos)
			assert.Equal(t, "estimated", requests[0].UsageSource)
		})
	}
}

func TestAsyncTaskImageCountUsesCompletedFactsOnBothSides(t *testing.T) {
	calls := 0
	fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":{"choices":[{"message":{"content":[{"image":"https://artifact.invalid/one.png"},{"image":"https://artifact.invalid/two.png"},{"image":"https://artifact.invalid/three.png"}]}}]},"usage":{"image_count":3}}`))
	})
	source, err := plugins.Source("alibaba")
	require.NoError(t, err)
	jsplugin.DefaultRegistry = jsplugin.NewRegistry()
	_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "alibaba"})
	require.NoError(t, err)
	require.NoError(t, model.DB.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Updates(map[string]any{"model_name": "qwen-image-2.0", "capabilities": capability.Set{InputModalities: []capability.Modality{capability.Text, capability.Image}, OutputModalities: []capability.Modality{capability.Image}}}).Error)
	require.NoError(t, model.DB.Model(fixture.channel).Updates(map[string]any{"protocol": "task:alibaba", "upstream_model_name": "qwen-image-2.0"}).Error)
	fixture.fake.Advance(time.Second)
	for _, price := range []*model.PriceVersion{
		{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: `tier("image", image_count * 0.1)`, EffectiveAt: clock.Now().Unix()},
		{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: `tier("image", u("image_count") * 0.05)`, EffectiveAt: clock.Now().Unix()},
	} {
		require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
	}
	pricingPAT := createWalletTestAccessToken(t, fixture.sale.CreatedBy, "pricing:read", "pricing:write")
	invalid, err := common.Marshal(map[string]any{"owner_type": model.PriceOwnerOffering, "owner_id": fixture.offering.ID, "currency": "USD", "expression": `tier("image", (image_count - 1) * 0.1)`, "effective_at": clock.Now().Unix() + 1000})
	require.NoError(t, err)
	invalidResponse := adminSecurityRequest(fixture.engine, pricingPAT, http.MethodPost, "/api/admin/v1/price-versions", string(invalid))
	assert.Equal(t, 400, invalidResponse.Code, "task image-count prices must be safe for an explicit zero fact")
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/images/generations", `{"model":"gateway-alias","prompt":"A bird","n":4,"response_format":"url"}`)
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.Equal(t, 1, calls)
	requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	assert.EqualValues(t, 270000000, requests[0].Receivable.Nanos)
	assert.EqualValues(t, 3, requests[0].Usage.TaskFacts["image_count"])
	attempts, total, err := model.ListGatewayAttempts(t.Context(), requests[0].ID, 0, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	assert.EqualValues(t, 150000000, attempts[0].Cost.Nanos)
	var reservation model.LedgerEntry
	require.NoError(t, model.DB.Where("request_id = ? AND type = ?", requests[0].ID, model.LedgerEntryPreConsume).Take(&reservation).Error)
	assert.EqualValues(t, -360000000, reservation.Amount.Nanos)
	wallet, err := model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	assert.EqualValues(t, 730000000, wallet.Balance.Nanos)
}

func TestAsyncTaskUnavailableChannelUsesBackendLanguage(t *testing.T) {
	fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Fail(t, "unavailable task plugin must not contact its Provider")
	})
	require.NoError(t, model.DB.Model(fixture.channel).Update("protocol", "task:unavailable-plugin").Error)
	for _, language := range []struct{ locale, message string }{{"en", "Task Channel is unavailable"}, {"zh-CN", "任务通道不可用"}, {"zh-TW", "任務通道無法使用"}} {
		request := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`))
		request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept-Language", language.locale)
		response := httptest.NewRecorder()
		fixture.engine.ServeHTTP(response, request)
		assert.Equal(t, 503, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), language.message)
	}
}

type taskProviderCredentialLock struct {
	tx  *gorm.DB
	pid int
}

func lockTaskProviderCredential(t *testing.T, providerID int) taskProviderCredentialLock {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	lock := taskProviderCredentialLock{tx: model.DB.WithContext(ctx).Begin()}
	require.NoError(t, lock.tx.Error)
	t.Cleanup(func() { _ = lock.tx.Rollback().Error })
	var locked int
	require.NoError(t, lock.tx.Raw(`SELECT "id" FROM "providers" WHERE "id" = ? FOR UPDATE`, providerID).Scan(&locked).Error)
	require.Equal(t, providerID, locked)
	require.NoError(t, lock.tx.Raw(`SELECT pg_backend_pid()`).Scan(&lock.pid).Error)
	return lock
}

func (lock taskProviderCredentialLock) wait(t *testing.T, finished chan *httptest.ResponseRecorder) {
	t.Helper()
	ctx := lock.tx.Statement.Context
	var waiting int
	for waiting == 0 && ctx.Err() == nil {
		select {
		case early := <-finished:
			finished <- early
			require.FailNowf(t, "task did not wait for its credential lock", "%d %s", early.Code, early.Body.String())
		default:
		}
		require.NoError(t, model.DB.WithContext(ctx).Raw(`SELECT COALESCE(min(pid),0) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND ? = ANY(pg_blocking_pids(pid))`, lock.pid).Scan(&waiting).Error)
	}
	require.NotZero(t, waiting)
}

func TestAsyncTaskCredentialLockWaitRechecksRemainingBudget(t *testing.T) {
	for _, test := range []struct {
		name, budget            string
		refreshPrice, allowLoss bool
		revokeKey               bool
		status                  int
	}{
		{name: "remaining budget", budget: "6", status: http.StatusServiceUnavailable},
		{name: "new purchase price and FX", budget: "60", refreshPrice: true, allowLoss: true, status: http.StatusOK},
		{name: "new loss without fallback", budget: "60", refreshPrice: true, status: http.StatusServiceUnavailable},
		{name: "revoked key after wait", budget: "60", revokeKey: true, status: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					assert.Equal(t, "/v1/videos/too-late-job", r.URL.Path)
					_, _ = io.WriteString(w, `{"id":"too-late-job","status":"completed","seconds":"2"}`)
					return
				}
				_, _ = io.WriteString(w, `{"id":"too-late-job","status":"queued"}`)
			})
			t.Setenv("GATEWAY_TOTAL_FIRST_BYTE_SECONDS", test.budget)
			var scheduled *model.PriceVersion
			if test.refreshPrice {
				fixture.offering.AllowLossFallback = test.allowLoss
				require.NoError(t, model.SaveModelOffering(t.Context(), fixture.offering, "save"))
				fixture.fake.Advance(time.Duration(59-clock.Now().Second()) * time.Second)
				_, err := model.SaveFXRate(t.Context(), "EUR", "USD", "2", "0", fixture.sale.CreatedBy)
				require.NoError(t, err)
				scheduled = &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "EUR", Expression: fmt.Sprintf(`tier("video", u("seconds") * 0.03 * (minute("UTC") == %d ? 2 : 1))`, clock.Now().Add(2*time.Second).UTC().Minute()), EffectiveAt: clock.Now().Unix() + 2}
				for _, price := range []*model.PriceVersion{
					{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "EUR", Expression: `tier("video", u("seconds") * 0.01)`, EffectiveAt: clock.Now().Unix()}, scheduled,
				} {
					err := model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy)
					if err != nil {
						var confirmation *model.MarginConfirmationError
						require.ErrorAs(t, err, &confirmation)
						price.MarginConfirmation = confirmation.Report.Revision
						err = model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy)
					}
					require.NoError(t, err)
				}
			}
			held := lockTaskProviderCredential(t, fixture.channel.ProviderID)
			lock := held.tx
			finished := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				finished <- adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
			}()
			completed := false
			defer func() {
				_ = lock.Rollback().Error
				if !completed {
					<-finished
				}
			}()
			held.wait(t, finished)
			fixture.fake.Advance(2 * time.Second)
			if test.refreshPrice {
				_, err := model.SaveFXRate(t.Context(), "EUR", "USD", "3", "0", fixture.sale.CreatedBy)
				if err != nil {
					var confirmation *model.MarginConfirmationError
					require.ErrorAs(t, err, &confirmation)
					_, err = model.SaveFXRate(t.Context(), "EUR", "USD", "3", "0", fixture.sale.CreatedBy, confirmation.Report.Revision)
				}
				require.NoError(t, err)
			}
			if test.revokeKey {
				require.NoError(t, lock.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Update("key", "rotated-task-fixture-key").Error)
			}
			require.NoError(t, lock.Commit().Error)
			response := <-finished
			completed = true
			require.Equal(t, test.status, response.Code, response.Body.String())
			if test.status != http.StatusOK {
				assert.Zero(t, calls.Load())
				wallet, err := model.GetUserWallet(fixture.user.Id)
				require.NoError(t, err)
				assert.EqualValues(t, 1000000000, wallet.Balance.Nanos)
				_, total, err := model.ListGatewayAttempts(t.Context(), response.Header().Get(common.RequestIdKey), 0, 10)
				require.NoError(t, err)
				assert.Zero(t, total)
				return
			}
			assert.EqualValues(t, 1, calls.Load())
			service.RunTaskPollingOnce(t.Context(), nil)
			assert.EqualValues(t, 2, calls.Load())
			attempts, total, err := model.ListGatewayAttempts(t.Context(), response.Header().Get(common.RequestIdKey), 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.Equal(t, scheduled.ID, attempts[0].PurchasePriceVersionID)
			assert.GreaterOrEqual(t, attempts[0].StartedAt, scheduled.EffectiveAt*1000)
			assert.Equal(t, "3", attempts[0].AccountingFXRate)
			assert.Equal(t, money.Currency("EUR"), attempts[0].Cost.Currency)
			assert.EqualValues(t, 120000000, attempts[0].Cost.Nanos)
			assert.EqualValues(t, 360000000, attempts[0].ConvertedCost.Nanos)
			request, err := model.GetGatewayRequest(t.Context(), attempts[0].RequestID, fixture.user.Id)
			require.NoError(t, err)
			assert.EqualValues(t, 180000000, request.Receivable.Nanos)
			var payables []model.LedgerEntry
			require.NoError(t, model.DB.Where("request_id = ? AND account_type = ?", request.ID, model.LedgerAccountProviderPayable).Find(&payables).Error)
			require.Len(t, payables, 1)
			assert.Equal(t, money.Amount{Nanos: 120000000, Currency: "EUR"}, payables[0].Amount)
			assert.Equal(t, "3", payables[0].FxRate.Decimal.String())
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			assert.EqualValues(t, 820000000, wallet.Balance.Nanos)
			var reservations int64
			require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("request_id = ? AND type = ?", request.ID, model.LedgerEntryPreConsume).Count(&reservations).Error)
			assert.EqualValues(t, 1, reservations)
		})
	}
}

func TestAsyncTaskFinalLossFallbackRechecksPeerAvailability(t *testing.T) {
	for _, test := range []struct {
		name             string
		shared, wantPeer bool
	}{
		{name: "disabled Channel"},
		{name: "disabled Provider"},
		{name: "exhausted Provider balance"},
		{name: "busy Provider"}, {name: "busy Provider", shared: true},
		{name: "occupied probe"}, {name: "occupied probe", shared: true},
		{name: "shared Provider", wantPeer: true}, {name: "shared Provider", shared: true, wantPeer: true},
		{name: "available peer", wantPeer: true},
		{name: "own probe", wantPeer: true}, {name: "own probe", shared: true, wantPeer: true},
		{name: "foreign probe fence"}, {name: "foreign probe fence", shared: true},
	} {
		t.Run(fmt.Sprintf("%s/redis=%t", test.name, test.shared), func(t *testing.T) {
			var calls atomic.Int32
			upstream := func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					assert.Equal(t, "/v1/videos/peer-job", r.URL.Path)
					_, _ = io.WriteString(w, `{"id":"peer-job","status":"completed","seconds":"2"}`)
					return
				}
				_, _ = io.WriteString(w, `{"id":"peer-job","status":"queued"}`)
			}
			fixture := newAsyncTaskFixture(t, upstream)
			fixture.offering.RouteMethod = "weight"
			require.NoError(t, model.SaveModelOffering(t.Context(), fixture.offering, "save"))
			fixture.fake.Advance(time.Second)
			peer := addTextGatewayChannel(t, fixture, upstream)
			peer.Protocol, peer.UpstreamModelName = "task:sora", "sora-2"
			if test.name == "shared Provider" {
				peer.ProviderID = fixture.channel.ProviderID
				peer.UpstreamModelName = "sora-2-pro"
			}
			require.NoError(t, model.DB.Model(peer).Updates(map[string]any{"protocol": peer.Protocol, "upstream_model_name": peer.UpstreamModelName, "provider_id": peer.ProviderID}).Error)
			var provider model.Provider
			require.NoError(t, model.DB.First(&provider, peer.ProviderID).Error)
			if test.name == "busy Provider" || test.name == "shared Provider" {
				provider.ProviderInfo.RouteLimits.MaxConcurrency = 1
				if test.name == "shared Provider" {
					provider.ProviderInfo.RouteLimits.RPM = 1
				}
				require.NoError(t, model.DB.Model(&provider).Update("channel_info", provider.ProviderInfo).Error)
			}
			var fence *model.Channel
			if test.name == "foreign probe fence" {
				fence = addTextGatewayChannel(t, fixture, upstream)
				require.NoError(t, model.DB.Model(fence).Updates(map[string]any{"protocol": "task:sora", "upstream_model_name": "sora-2"}).Error)
			}
			prices := []*model.PriceVersion{
				{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: `tier("video",u("seconds")*0.01)`, EffectiveAt: clock.Now().Unix()},
				{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: `tier("video",u("seconds")*0.2)`, EffectiveAt: clock.Now().Unix() + 2},
				{OwnerType: model.PriceOwnerChannel, OwnerID: peer.ID, Currency: "USD", Expression: `tier("video",u("seconds")*0.005)`, EffectiveAt: clock.Now().Unix()},
			}
			if fence != nil {
				prices = append(prices, &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: fence.ID, Currency: "USD", Expression: `tier("video",u("seconds")*0.005)`, EffectiveAt: clock.Now().Unix()})
			}
			for _, price := range prices {
				err := model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy)
				if err != nil {
					var confirmation *model.MarginConfirmationError
					require.ErrorAs(t, err, &confirmation)
					price.MarginConfirmation = confirmation.Report.Revision
					err = model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy)
				}
				require.NoError(t, err)
			}
			if test.shared {
				server := miniredis.RunT(t)
				client := redis.NewClient(&redis.Options{Addr: server.Addr()})
				t.Cleanup(func() { require.NoError(t, client.Close()) })
				service.GatewayRouteQuota = service.NewGatewayRouteQuota(client)
				service.ChannelHealth = channelhealth.New(client)
			}
			if test.name == "own probe" {
				for i, channel := range []*model.Channel{fixture.channel, peer} {
					_, err := service.ChannelHealth.Record(t.Context(), channelhealth.Sample{AttemptID: fmt.Sprintf("own-probe-open-%d", channel.ID), ChannelID: channel.ID, ProviderFailure: true, RateLimited: true, RetryAfter: fmt.Sprint(60 * (i + 1))})
					require.NoError(t, err)
				}
			}
			busy := &service.GatewaySelection{Channel: *peer, Provider: &provider}
			var occupied *service.GatewayRoutePermit
			var probe channelhealth.Admission
			defer func() {
				if occupied != nil {
					require.NoError(t, occupied.Release(t.Context(), false))
				}
				if probe.Lease != "" {
					require.NoError(t, service.ChannelHealth.Release(t.Context(), probe))
				}
			}()
			held := lockTaskProviderCredential(t, fixture.channel.ProviderID)
			finished := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				finished <- adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/tasks/sora", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
			}()
			completed := false
			defer func() {
				_ = held.tx.Rollback().Error
				if !completed {
					<-finished
				}
			}()
			held.wait(t, finished)
			fixture.fake.Advance(2 * time.Second)
			switch test.name {
			case "disabled Channel":
				require.NoError(t, model.DB.Model(peer).Update("status", model.ChannelDisabled).Error)
			case "disabled Provider":
				require.NoError(t, model.DB.Model(&provider).Update("status", common.ChannelStatusManuallyDisabled).Error)
			case "exhausted Provider balance":
				require.NoError(t, model.DB.Model(&provider).Updates(map[string]any{"balance": 0, "balance_updated_time": clock.Now().Unix()}).Error)
			case "busy Provider":
				var err error
				occupied, err = service.GatewayRouteQuota.Acquire(t.Context(), busy, 0)
				require.NoError(t, err)
			case "foreign probe fence":
				for _, channel := range []*model.Channel{peer, fence} {
					_, err := service.ChannelHealth.Record(t.Context(), channelhealth.Sample{AttemptID: fmt.Sprintf("foreign-fence-open-%d", channel.ID), ChannelID: channel.ID, ProviderFailure: true, RateLimited: true, RetryAfter: "1"})
					require.NoError(t, err)
				}
				var err error
				probe, err = service.ChannelHealth.Acquire(t.Context(), []int{fence.ID})
				require.NoError(t, err)
				require.Equal(t, fence.ID, probe.ChannelID)
				require.NotEmpty(t, probe.Lease)
				fixture.fake.Advance(2 * time.Second)
			case "occupied probe":
				_, err := service.ChannelHealth.Record(t.Context(), channelhealth.Sample{AttemptID: "late-peer-probe", ChannelID: peer.ID, ProviderFailure: true, RateLimited: true, RetryAfter: "60"})
				require.NoError(t, err)
				probe, err = service.ChannelHealth.Acquire(t.Context(), []int{peer.ID})
				require.NoError(t, err)
				require.Equal(t, peer.ID, probe.ChannelID)
				require.NotEmpty(t, probe.Lease)
			}
			require.NoError(t, held.tx.Commit().Error)
			response := <-finished
			completed = true
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.EqualValues(t, 1, calls.Load())
			service.RunTaskPollingOnce(t.Context(), nil)
			assert.EqualValues(t, 2, calls.Load())
			attempts, total, err := model.ListGatewayAttempts(t.Context(), response.Header().Get(common.RequestIdKey), 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			channelID, cost := fixture.channel.ID, int64(400000000)
			if test.wantPeer {
				channelID, cost = peer.ID, 10000000
			}
			assert.Equal(t, channelID, attempts[0].ChannelID)
			assert.Equal(t, cost, attempts[0].Cost.Nanos)
			request, err := model.GetGatewayRequest(t.Context(), attempts[0].RequestID, fixture.user.Id)
			require.NoError(t, err)
			assert.EqualValues(t, 180000000, request.Receivable.Nanos)
			assertTextGatewayLedger(t, *request, cost)
			var reservations int64
			require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("request_id = ? AND type = ?", request.ID, model.LedgerEntryPreConsume).Count(&reservations).Error)
			assert.EqualValues(t, 1, reservations)
			if occupied != nil {
				assert.ErrorIs(t, service.GatewayRouteQuota.Check(t.Context(), busy, 0), service.ErrGatewayRateLimited)
			}
		})
	}
}

func TestAsyncTaskImageInRequestPollingUsesActualCredentialSlot(t *testing.T) {
	previousTimeout := constant.TaskPluginProtocolTimeoutSeconds
	constant.TaskPluginProtocolTimeoutSeconds = 5
	t.Cleanup(func() { constant.TaskPluginProtocolTimeoutSeconds = previousTimeout })
	var submitted, polled atomic.Int32
	fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, []string{"Bearer image-slot-one", "Bearer image-slot-two"}, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			polled.Add(1)
			_, _ = w.Write([]byte(`{"output":{"task_id":"image-job","task_status":"SUCCEEDED","results":[{"url":"https://artifact.invalid/image.png"}]},"usage":{"image_count":1}}`))
			return
		}
		submitted.Add(1)
		_, _ = w.Write([]byte(`{"output":{"task_id":"image-job","task_status":"PENDING"}}`))
	})
	source, err := plugins.Source("alibaba")
	require.NoError(t, err)
	jsplugin.DefaultRegistry = jsplugin.NewRegistry()
	_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "alibaba"})
	require.NoError(t, err)
	require.NoError(t, model.DB.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Updates(map[string]any{"model_name": "wanx2.1-t2i-turbo", "capabilities": capability.Set{InputModalities: []capability.Modality{capability.Text}, OutputModalities: []capability.Modality{capability.Image}}}).Error)
	require.NoError(t, model.DB.Model(fixture.channel).Updates(map[string]any{"protocol": "task:alibaba", "upstream_model_name": "wanx2.1-t2i-turbo"}).Error)
	require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", fixture.channel.ProviderID).Updates(map[string]any{"key": "image-slot-one\nimage-slot-two", "channel_info": model.ProviderInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModePolling}}).Error)
	fixture.fake.Advance(time.Second)
	for _, price := range []*model.PriceVersion{
		{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: `tier("image",u("image_count")*0.1)`, EffectiveAt: clock.Now().Unix()},
		{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: `tier("image",u("image_count")*0.05)`, EffectiveAt: clock.Now().Unix()},
	} {
		require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
	}
	response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/v1/images/generations", `{"model":"gateway-alias","prompt":"A bird","n":1,"response_format":"url"}`)
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.EqualValues(t, 1, submitted.Load())
	assert.EqualValues(t, 1, polled.Load())
	wallet, err := model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	assert.EqualValues(t, 910000000, wallet.Balance.Nanos)
}

func TestAsyncTaskNativeMediaFactsUseFinalWire(t *testing.T) {
	for _, test := range []struct {
		name, plugin, model, path, body, overrides, expression, result string
		maximum                                                        int64
	}{
		{"vidu-native-unbounded", "vidu", "viduq3-pro", "/vidu/ent/v2/text2video", `{"model":"gateway-alias","prompt":"A bird","duration":4001}`, `{}`, `u("duration")/10000`, `{"task_id":"oversized-job"}`, 16},
		{"doubao-native-override", "doubao", "doubao-seedance-1-0-pro-250528", "/doubao/api/v3/contents/generations/tasks", `{"model":"gateway-alias","content":[{"type":"text","text":"A bird"}],"duration":10}`, `{"seconds":4}`, `u("tokens")/1000000`, `{"id":"oversized-job"}`, 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.result))
			})
			source, err := plugins.Source(test.plugin)
			require.NoError(t, err)
			jsplugin.DefaultRegistry = jsplugin.NewRegistry()
			_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: test.plugin})
			require.NoError(t, err)
			caps := capability.Set{InputModalities: []capability.Modality{capability.Text}, OutputModalities: []capability.Modality{capability.Video}, Limits: capability.Limits{MaxDurationSeconds: &test.maximum}}
			require.NoError(t, model.DB.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Updates(map[string]any{"model_name": test.model, "capabilities": caps}).Error)
			require.NoError(t, model.DB.Model(fixture.channel).Updates(map[string]any{"protocol": "task:" + test.plugin, "upstream_model_name": test.model, "parameter_overrides": test.overrides}).Error)
			fixture.fake.Advance(time.Second)
			for _, price := range []*model.PriceVersion{{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: `tier("task", (` + test.expression + `)*0.1)`, EffectiveAt: clock.Now().Unix()}, {OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: `tier("task", (` + test.expression + `)*0.05)`, EffectiveAt: clock.Now().Unix()}} {
				require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
			}
			builder := newPluginGenerationBuilder(fixture.engine.Routes(), nil, productionPluginRouteHandlers)
			require.NoError(t, jsplugin.DefaultRegistry.SetGenerationPreparer(builder.prepare))
			dispatcher := &pluginRouteDispatcher{registry: jsplugin.DefaultRegistry}
			fixture.engine.NoRoute(dispatcher.dispatch)
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, test.path, test.body)
			assert.Equal(t, 400, response.Code, response.Body.String())
			assert.Zero(t, calls.Load())
			var reservations int64
			require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("type = ?", model.LedgerEntryPreConsume).Count(&reservations).Error)
			assert.Zero(t, reservations)
			require.NoError(t, model.DB.Model(fixture.channel).Update("parameter_overrides", `{}`).Error)
			var valid map[string]any
			require.NoError(t, common.UnmarshalJsonStr(test.body, &valid))
			valid["duration"] = 4
			encoded, err := common.Marshal(valid)
			require.NoError(t, err)
			response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, test.path, string(encoded))
			assert.Equal(t, 200, response.Code, response.Body.String())
			assert.EqualValues(t, 1, calls.Load())
			if test.plugin == "doubao" {
				require.NoError(t, model.DB.Model(fixture.channel).Update("parameter_overrides", `{"size":"480p"}`).Error)
				response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, test.path, string(encoded))
				require.Equal(t, 200, response.Code, response.Body.String())
				requests, _, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
				require.NoError(t, err)
				require.Len(t, requests, 2)
				for _, request := range requests {
					assert.EqualValues(t, 194400, request.Usage.TaskFacts["tokens"], "native wire omits resolution, so a normalized size override cannot reduce the default upper-bound reservation")
				}
			}
		})
	}
}

func TestAsyncTaskPhysicalDurationAndEffectiveImageModalities(t *testing.T) {
	for _, test := range []struct {
		name, plugin, model, body, result, expression string
		input, output                                 []capability.Modality
		maximum                                       int64
		status                                        int
	}{
		{"credit-duration", "kling", "kling-v3", `{"model":"gateway-alias","prompt":"A bird","duration":10}`, `{"code":0,"data":{"task_id":"kling-job"}}`, `u("units")`, []capability.Modality{capability.Text}, []capability.Modality{capability.Video}, 5, 400},
		{"token-duration", "doubao", "doubao-seedance-1-0-pro-250528", `{"model":"gateway-alias","prompt":"A bird","seconds":"10"}`, `{"id":"doubao-job"}`, `u("tokens") / 1000000`, []capability.Modality{capability.Text}, []capability.Modality{capability.Video}, 5, 400},
		{"unbounded-wire-duration", "doubao", "doubao-seedance-1-0-pro-250528", `{"model":"gateway-alias","prompt":"A bird","seconds":"4001"}`, `{"id":"doubao-job"}`, `u("tokens") / 1000000000`, []capability.Modality{capability.Text}, []capability.Modality{capability.Video}, 0, 400},
		{"image-only-layers", "doubao", "doubao-seedream-5-0-pro-260628", `{"model":"gateway-alias","image":"https://image.invalid/source.png","layer_decomposition":true,"size":"2K"}`, `{"data":[{"url":"https://artifact.invalid/layer.png","size":"1024x1024"}],"usage":{"generated_images":1}}`, `(u("images_up_to_1_5k") + u("images_above_1_5k")) / 100`, []capability.Modality{capability.Image}, []capability.Modality{capability.Image}, 0, 200},
		{"interleaved-output", "alibaba", "wan2.6-image", `{"model":"gateway-alias","input":{"messages":[{"role":"user","content":[{"text":"A bird"}]}]},"parameters":{"enable_interleave":true,"max_images":1}}`, `{"output":{"choices":[{"message":{"content":[{"text":"A bird"},{"image":"https://artifact.invalid/bird.png"}]}}]},"usage":{"image_count":1}}`, `u("image_count")`, []capability.Modality{capability.Text}, []capability.Modality{capability.Image}, 0, 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.result))
			})
			source, err := plugins.Source(test.plugin)
			require.NoError(t, err)
			jsplugin.DefaultRegistry = jsplugin.NewRegistry()
			_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: test.plugin})
			require.NoError(t, err)
			caps := capability.Set{InputModalities: test.input, OutputModalities: test.output}
			if test.maximum > 0 {
				caps.Limits.MaxDurationSeconds = &test.maximum
			}
			require.NoError(t, model.DB.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Updates(map[string]any{"model_name": test.model, "capabilities": caps}).Error)
			require.NoError(t, model.DB.Model(fixture.channel).Updates(map[string]any{"protocol": "task:" + test.plugin, "upstream_model_name": test.model}).Error)
			fixture.fake.Advance(time.Second)
			for _, price := range []*model.PriceVersion{
				{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: `tier("task", (` + test.expression + `) * 0.1)`, EffectiveAt: clock.Now().Unix()},
				{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: `tier("task", (` + test.expression + `) * 0.05)`, EffectiveAt: clock.Now().Unix()},
			} {
				require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
			}
			path := "/v1/tasks/" + test.plugin
			if test.plugin == "alibaba" {
				builder := newPluginGenerationBuilder(fixture.engine.Routes(), nil, productionPluginRouteHandlers)
				require.NoError(t, jsplugin.DefaultRegistry.SetGenerationPreparer(builder.prepare))
				dispatcher := &pluginRouteDispatcher{registry: jsplugin.DefaultRegistry}
				fixture.engine.NoRoute(dispatcher.dispatch)
				path = "/ali/api/v1/services/aigc/multimodal-generation/generation"
			}
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, path, test.body)
			require.Equal(t, test.status, response.Code, response.Body.String())
			if test.status == 200 {
				assert.EqualValues(t, 1, calls.Load())
				return
			}
			assert.Zero(t, calls.Load())
			var reservations int64
			require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("type = ?", model.LedgerEntryPreConsume).Count(&reservations).Error)
			assert.Zero(t, reservations)
			if test.name == "interleaved-output" {
				caps.OutputModalities = append(caps.OutputModalities, capability.Text)
				require.NoError(t, model.DB.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Update("capabilities", caps).Error)
				response = adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, path, test.body)
				require.Equal(t, 200, response.Code, response.Body.String())
				assert.EqualValues(t, 1, calls.Load())
			}
		})
	}
}

func newNativeImageTaskFixture(t *testing.T, pluginKey string, upstream http.HandlerFunc) textGatewayFixture {
	t.Helper()
	fixture := newAsyncTaskFixture(t, upstream)
	modelName, quantity := "wan2.6-image", "image_count"
	if pluginKey == "doubao" {
		modelName, quantity = "doubao-seedream-5-0-lite-260128", "images_above_1_5k"
	}
	source, err := plugins.Source(pluginKey)
	require.NoError(t, err)
	jsplugin.DefaultRegistry = jsplugin.NewRegistry()
	_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: pluginKey})
	require.NoError(t, err)
	caps := capability.Set{Features: []capability.Feature{capability.Streaming}, InputModalities: []capability.Modality{capability.Text, capability.Image}, OutputModalities: []capability.Modality{capability.Text, capability.Image}}
	require.NoError(t, model.DB.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Updates(map[string]any{"model_name": modelName, "capabilities": caps}).Error)
	require.NoError(t, model.DB.Model(fixture.channel).Updates(map[string]any{"protocol": "task:" + pluginKey, "upstream_model_name": modelName}).Error)
	fixture.fake.Advance(time.Second)
	for _, price := range []*model.PriceVersion{
		{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: fmt.Sprintf(`tier("image", u(%q) * 0.1)`, quantity), EffectiveAt: clock.Now().Unix()},
		{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: fmt.Sprintf(`tier("image", u(%q) * 0.05)`, quantity), EffectiveAt: clock.Now().Unix()},
	} {
		require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
	}
	builder := newPluginGenerationBuilder(fixture.engine.Routes(), nil, productionPluginRouteHandlers)
	require.NoError(t, jsplugin.DefaultRegistry.SetGenerationPreparer(builder.prepare))
	fixture.engine.NoRoute((&pluginRouteDispatcher{registry: jsplugin.DefaultRegistry}).dispatch)
	return fixture
}

func TestAsyncTaskNativeStreamSettlesObservedFacts(t *testing.T) {
	first := "data: {\"output\":{\"choices\":[{\"message\":{\"content\":[{\"image\":\"https://artifact.invalid/bird.png\"}]}}]},\"usage\":{\"image_count\":1}}\n\n"
	padding := "data: {\"output\":{\"choices\":[{\"message\":{\"content\":[{\"text\":\"" + strings.Repeat("x", 512<<10) + "\"}]}}]}}\n\n"
	overflow := first + "data: {\"output\":{\"choices\":[{\"message\":{\"content\":[{\"image\":\"https://artifact.invalid/second.png\"}]}}]},\"usage\":{\"image_count\":2}}\n\n" + strings.Repeat(padding, 2)
	text := `{"text":"` + strings.Repeat("x", 512<<10) + `"}`
	image := `{"image":"https://artifact.invalid/second.png"}`
	sameEvent := first + padding + `data: {"output":{"choices":[{"message":{"content":[`
	for _, test := range []struct {
		name, stream   string
		sale, cost     int64
		costPending    bool
		completeWrites int
		status         int
	}{
		{"metadata only", ": keep-alive\n\ndata: invalid-json\n\n", 0, 0, true, -1, http.StatusOK},
		{"invalid first frame", "data: invalid-json\n\n", 0, 0, true, -1, http.StatusBadGateway},
		{"empty stream", "", 0, 0, true, -1, http.StatusBadGateway},
		{"known source survives cumulative limit", overflow, 90000000, 100000000, true, 1, http.StatusOK},
		{"invalid recovered count retains prior source", sameEvent + text + `]}}]},"usage":{"image_count":-1}}` + "\n\n", 90000000, 50000000, true, -1, http.StatusOK},
		{"same event image before oversized text", sameEvent + image + "," + text + `]}}]},"usage":{"image_count":2}}` + "\n\n", 90000000, 100000000, true, -1, http.StatusOK},
		{"same event image after oversized text", sameEvent + text + "," + image + `]}}]},"usage":{"image_count":2}}` + "\n\n", 90000000, 100000000, true, -1, http.StatusOK},
		{"same event before first delivery", `data: {"output":{"choices":[{"message":{"content":[` + image + `,{"image":"https://artifact.invalid/third.png"},{"text":"` + strings.Repeat("<", 200<<10) + `"}]}}]},"usage":{"image_count":2}}` + "\n\n", 0, 100000000, true, -1, http.StatusBadGateway},
		{"one image before invalid event", "data: {\"output\":{\"choices\":[{\"message\":{\"content\":[{\"image\":\"https://artifact.invalid/bird.png\"}]}}]},\"usage\":{\"image_count\":1}}\n\ndata: invalid-json\n\n", 90000000, 50000000, true, -1, http.StatusOK},
		{"completed image", "data: {\"output\":{\"choices\":[{\"message\":{\"content\":[{\"image\":\"https://artifact.invalid/bird.png\"}]},\"finish_reason\":\"stop\"}]},\"usage\":{\"image_count\":1}}\n\n", 90000000, 50000000, false, -1, http.StatusOK},
		{"short first image write", "data: {\"output\":{\"choices\":[{\"message\":{\"content\":[{\"image\":\"https://artifact.invalid/bird.png\"}]},\"finish_reason\":\"stop\"}]},\"usage\":{\"image_count\":1}}\n\n", 0, 50000000, false, 0, http.StatusOK},
		{"short second image write", "data: {\"output\":{\"choices\":[{\"message\":{\"content\":[{\"image\":\"https://artifact.invalid/bird.png\"}]}}]}}\n\ndata: {\"output\":{\"choices\":[{\"message\":{\"content\":[{\"image\":\"https://artifact.invalid/second.png\"}]},\"finish_reason\":\"stop\"}]},\"usage\":{\"image_count\":2}}\n\n", 90000000, 100000000, false, 1, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			fixture := newNativeImageTaskFixture(t, "alibaba", func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, "enable", r.Header.Get("X-DashScope-Sse"))
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, test.stream)
			})
			request := httptest.NewRequest(http.MethodPost, "/ali/api/v1/services/aigc/multimodal-generation/generation", strings.NewReader(`{"model":"gateway-alias","input":{"messages":[{"role":"user","content":[{"text":"A bird"}]}]},"parameters":{"enable_interleave":true,"max_images":4}}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
			request.Header.Set("X-DashScope-Sse", "enable")
			response := httptest.NewRecorder()
			var writer http.ResponseWriter = response
			if test.completeWrites >= 0 {
				remaining := test.completeWrites
				writer = gatewayPrefixOnlyWriter{ResponseRecorder: response, nilError: true, completeWrites: &remaining}
			}
			fixture.engine.ServeHTTP(writer, request)
			require.Equal(t, test.status, response.Code, response.Body.String())
			assert.EqualValues(t, 1, calls.Load())
			for range 2 {
				_, err := service.RecoverGatewayTasks(t.Context(), 100)
				if test.costPending {
					require.ErrorIs(t, err, model.ErrGatewayEvaluationPending)
				} else {
					require.NoError(t, err)
				}
			}
			requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.Equal(t, test.sale, requests[0].Receivable.Nanos)
			assert.False(t, requests[0].SalePending)
			attempts, total, err := model.ListGatewayAttempts(t.Context(), requests[0].ID, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.Equal(t, test.cost, attempts[0].Cost.Nanos)
			assert.Equal(t, test.costPending, attempts[0].CostPending)
			wallet, err := model.GetUserWallet(fixture.user.Id)
			require.NoError(t, err)
			assert.EqualValues(t, 1000000000-test.sale, wallet.Balance.Nanos)
			assertTextGatewayLedger(t, requests[0], test.cost)
			var reservations int64
			require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("request_id = ? AND type = ?", requests[0].ID, model.LedgerEntryPreConsume).Count(&reservations).Error)
			assert.EqualValues(t, 1, reservations)
		})
	}
}

func TestAsyncTaskNativeStreamRetainsAcceptedIdentityAfterInvalidDelta(t *testing.T) {
	for _, test := range []struct {
		name            string
		replacement     string
		eraseID, cancel bool
		immediate       bool
	}{
		{name: "erase-id-false"},
		{name: "erase-id-true", eraseID: true},
		{name: "replace-id", replacement: "other-real-id"},
		{name: "caller-cancelled", cancel: true},
		{name: "current-source-with-recovered-id", eraseID: true, immediate: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var submissions, queries atomic.Int32
			fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					queries.Add(1)
					assert.Equal(t, "/v1/videos/accepted-real-id", r.URL.Path)
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"accepted-real-id","status":"completed","seconds":"2"}`)
					return
				}
				submissions.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: Hello\n\ndata: invalid-delta\n\n")
			})
			source, err := plugins.Source("sora")
			require.NoError(t, err)
			source = strings.Replace(source, "apiVersion: 1,", `apiVersion: 1,
		requiredCapabilities: ["submit-sse-business@1"], submitResponseTypes: ["json", "sse"],
		routes: [{method:"POST", path:"/sora/native-stream-job", type:"submit", decode:"submit", render:"created"}],`, 1)
			source = strings.Replace(source, "export function buildSubmitRequest(ctx) {", `export function buildSubmitRequest(ctx) {
		return {url:ctx.baseUrl+"/v1/videos", method:"POST", headers:{"Content-Type":"application/json",Authorization:"Bearer "+ctx.apiKey}, body:ctx.requestBody, responseType:"sse"};`, 1)
			source += `
	export const native = {
		submit(ctx) { return {kind:"submit", model:ctx.body.value.model, requestBody:ctx.body.value}; },
		created(ctx, task) { return {id:task.task_id}; }
	};
	export function parseSubmitEventDelta(ctx, event, state) {
		if (event.data === "Hello") return {changes:[{op:"set",path:[],value:{id:"accepted-real-id",status:"queued",seconds:1}}],state:{},done:false,business:HAS_BUSINESS};
		return {changes:INVALID_CHANGES,state:{},done:false,business:false};
	}`
			source = strings.Replace(source, "HAS_BUSINESS", fmt.Sprintf("%t", test.immediate), 1)
			if test.immediate {
				source = strings.Replace(source, "return { taskId, taskData: body };", `return {taskId,taskData:body,immediate:{status:"SUCCESS",progress:"100%"}};`, 1)
			}
			badChanges := `[{op:"append",path:["missing"],value:1}]`
			if test.eraseID || test.replacement != "" {
				badChanges = fmt.Sprintf(`[{op:"set",path:["id"],value:%q},{op:"append",path:["missing"],value:1}]`, test.replacement)
			}
			if test.immediate {
				badChanges = `[{op:"set",path:["seconds"],value:2},` + strings.TrimPrefix(badChanges, "[")
			}
			source = strings.Replace(source, "INVALID_CHANGES", badChanges, 1)
			_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "sora"})
			require.NoError(t, err)
			builder := newPluginGenerationBuilder(fixture.engine.Routes(), nil, productionPluginRouteHandlers)
			require.NoError(t, jsplugin.DefaultRegistry.SetGenerationPreparer(builder.prepare))
			fixture.engine.NoRoute((&pluginRouteDispatcher{registry: jsplugin.DefaultRegistry}).dispatch)
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			request := httptest.NewRequest(http.MethodPost, "/sora/native-stream-job", strings.NewReader(`{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)).WithContext(parent)
			request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			var writer http.ResponseWriter = response
			if test.cancel {
				writer = cancelOnOutputWriter{ResponseRecorder: response, cancel: cancel}
			}
			fixture.engine.ServeHTTP(writer, request)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), "data: Hello")
			if test.cancel {
				require.ErrorIs(t, parent.Err(), context.Canceled)
			}
			var task model.Task
			require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Take(&task).Error)
			require.Equal(t, "accepted-real-id", task.PrivateData.UpstreamTaskID)
			if test.immediate {
				require.Equal(t, model.TaskStatus(model.TaskStatusSuccess), task.Status)
				assert.EqualValues(t, 2, task.PrivateData.Gateway.CompletionFacts["seconds"])
				for range 2 {
					_, err := service.RecoverGatewayTasks(t.Context(), 100)
					require.ErrorIs(t, err, model.ErrGatewayEvaluationPending)
				}
				request, err := model.GetGatewayRequest(t.Context(), task.PrivateData.Gateway.Settlement.Request.ID, fixture.user.Id)
				require.NoError(t, err)
				assert.EqualValues(t, 90000000, request.Receivable.Nanos)
				assertTextGatewayLedger(t, *request, 100000000)
				assert.Zero(t, queries.Load())
			} else {
				require.Equal(t, model.TaskStatus(model.TaskStatusSubmitted), task.Status)
				service.RunTaskPollingOnce(t.Context(), nil)
				require.NoError(t, model.DB.First(&task, task.ID).Error)
				assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), task.Status)
				assert.EqualValues(t, 1, queries.Load())
			}
			assert.Equal(t, fixture.channel.ID, task.ChannelId)
			assert.EqualValues(t, 1, submissions.Load())
			attempts, total, err := model.ListGatewayAttempts(t.Context(), task.PrivateData.Gateway.Settlement.Request.ID, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.EqualValues(t, 100000000, attempts[0].Cost.Nanos)
			var reservations int64
			require.NoError(t, model.DB.Model(&model.LedgerEntry{}).Where("request_id = ? AND type = ?", attempts[0].RequestID, model.LedgerEntryPreConsume).Count(&reservations).Error)
			assert.EqualValues(t, 1, reservations)
		})
	}
}

func TestAsyncTaskNativeStreamProjectsCompletionData(t *testing.T) {
	for _, mode := range []string{"snapshot", "delta"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			fixture := newAsyncTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: first\n\ndata: fail\n\n")
			})
			source, err := plugins.Source("sora")
			require.NoError(t, err)
			source = strings.Replace(source, "apiVersion: 1,", `apiVersion: 1,
				requiredCapabilities:["submit-sse-business@1"],submitResponseTypes:["json","sse"],
				routes:[{method:"POST",path:"/sora/projected-native-stream",type:"submit",decode:"submit",render:"created"}],`, 1)
			source = strings.Replace(source, "export function buildSubmitRequest(ctx) {", `export function buildSubmitRequest(ctx) {
				return {url:ctx.baseUrl+"/v1/videos",method:"POST",headers:{"Content-Type":"application/json",Authorization:"Bearer "+ctx.apiKey},body:ctx.requestBody,responseType:"sse"};`, 1)
			source = strings.Replace(source, "return { taskId, taskData: body };", `return {taskId,taskData:body.output,immediate:{status:"SUCCESS",progress:"100%"}};`, 1)
			source += `
				export const native = {
					submit(ctx) { return {kind:"submit",model:ctx.body.value.model,requestBody:ctx.body.value}; },
					created(ctx,task) { return {id:task.task_id}; }
				};`
			sale := int64(180000000)
			if mode == "snapshot" {
				source += `
					export function parseSubmitEvent(ctx,event,state) {
						if(event.data === "first") return {state:{id:"accepted-projected-id",output:{seconds:2}},done:false,business:true};
						throw new Error("synthetic truncated snapshot");
					}`
			} else {
				sale = 90000000
				source += `
					export function parseSubmitEventDelta(ctx,event,state) {
						if(event.data === "first") return {changes:[{op:"set",path:[],value:{id:"accepted-projected-id",output:{seconds:1}}}],state:{},done:false,business:true};
						return {changes:[{op:"set",path:["output","seconds"],value:2},{op:"set",path:["id"],value:"different-id"},{op:"append",path:["missing"],value:1}],state:{},done:false,business:true};
					}`
			}
			_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "sora"})
			require.NoError(t, err)
			builder := newPluginGenerationBuilder(fixture.engine.Routes(), nil, productionPluginRouteHandlers)
			require.NoError(t, jsplugin.DefaultRegistry.SetGenerationPreparer(builder.prepare))
			fixture.engine.NoRoute((&pluginRouteDispatcher{registry: jsplugin.DefaultRegistry}).dispatch)
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/sora/projected-native-stream", `{"model":"gateway-alias","prompt":"A bird","seconds":"4"}`)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), "data: first")
			assert.NotContains(t, response.Body.String(), "data: fail")
			assert.EqualValues(t, 1, calls.Load())
			var task model.Task
			require.NoError(t, model.DB.Where("user_id = ?", fixture.user.Id).Take(&task).Error)
			assert.Equal(t, "accepted-projected-id", task.PrivateData.UpstreamTaskID)
			assert.EqualValues(t, 2, task.PrivateData.Gateway.CompletionFacts["seconds"])
			for range 2 {
				_, err := service.RecoverGatewayTasks(t.Context(), 100)
				require.ErrorIs(t, err, model.ErrGatewayEvaluationPending)
			}
			request, err := model.GetGatewayRequest(t.Context(), task.PrivateData.Gateway.Settlement.Request.ID, fixture.user.Id)
			require.NoError(t, err)
			assert.False(t, request.SalePending)
			assert.Equal(t, sale, request.Receivable.Nanos)
			attempts, total, err := model.ListGatewayAttempts(t.Context(), request.ID, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.EqualValues(t, 100000000, attempts[0].Cost.Nanos)
			assert.True(t, attempts[0].CostPending)
			assertTextGatewayLedger(t, *request, 100000000)
		})
	}
}

func TestAsyncTaskDoubaoNativeStreamPreservesInputFacts(t *testing.T) {
	for _, test := range []struct {
		name                        string
		reference, zeroInput, empty bool
		sale, cost                  int64
	}{
		{"known zero input", false, false, false, 90000000, 50000000},
		{"known reference input", true, false, false, 99000000, 55000000},
		{"reported zero overrides reference count", true, true, false, 90000000, 50000000},
		{"missing source stays unknown", false, false, true, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			fixture := newNativeImageTaskFixture(t, "doubao", func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				if test.empty {
					return
				}
				usage := `{}`
				if test.zeroInput {
					usage = `{"input_images":0}`
				}
				_, _ = io.WriteString(w, "data: {\"type\":\"image_generation.partial_succeeded\",\"image_index\":0,\"url\":\"https://artifact.invalid/bird.png\",\"size\":\"2048x2048\"}\n\ndata: {\"type\":\"image_generation.completed\",\"usage\":"+usage+"}\n\ndata: [DONE]\n\n")
			})
			fixture.fake.Advance(time.Second)
			for _, price := range []*model.PriceVersion{
				{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: `tier("image", u("images_above_1_5k") * 0.1 + u("input_images") * 0.01)`, EffectiveAt: clock.Now().Unix()},
				{OwnerType: model.PriceOwnerChannel, OwnerID: fixture.channel.ID, Currency: "USD", Expression: `tier("image", u("images_above_1_5k") * 0.05 + u("input_images") * 0.005)`, EffectiveAt: clock.Now().Unix()},
			} {
				require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.sale.CreatedBy))
			}
			body := map[string]any{"model": "gateway-alias", "prompt": "A bird", "size": "2K", "stream": true, "sequential_image_generation": "auto", "sequential_image_generation_options": map[string]any{"max_images": 4}}
			if test.reference {
				body["image"] = []string{"https://artifact.invalid/reference.png"}
			}
			encoded, err := common.Marshal(body)
			require.NoError(t, err)
			response := adminSecurityRequest(fixture.engine, fixture.token.Key, http.MethodPost, "/doubao/api/v3/images/generations", string(encoded))
			status := http.StatusOK
			if test.empty {
				status = http.StatusBadGateway
			}
			require.Equal(t, status, response.Code, response.Body.String())
			assert.EqualValues(t, 1, calls.Load())
			for range 2 {
				_, err = service.RecoverGatewayTasks(t.Context(), 100)
				if test.empty {
					require.ErrorIs(t, err, model.ErrGatewayEvaluationPending)
				} else {
					require.NoError(t, err)
				}
			}
			requests, total, err := model.ListGatewayRequests(t.Context(), model.GatewayRequestFilter{UserID: fixture.user.Id}, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.False(t, requests[0].SalePending)
			assert.Equal(t, test.sale, requests[0].Receivable.Nanos)
			attempts, total, err := model.ListGatewayAttempts(t.Context(), requests[0].ID, 0, 10)
			require.NoError(t, err)
			require.EqualValues(t, 1, total)
			assert.Equal(t, test.cost, attempts[0].Cost.Nanos)
			assert.Equal(t, test.empty, attempts[0].CostPending)
			if test.empty {
				assert.NotContains(t, attempts[0].Usage.TaskFacts, "images_above_1_5k")
			}
			assertTextGatewayLedger(t, requests[0], test.cost)
		})
	}
}

func TestAsyncTaskNativeStreamHonorsBudgetAndCallerCancellation(t *testing.T) {
	for _, cause := range []string{"single second budget", "caller cancellation"} {
		t.Run(cause, func(t *testing.T) {
			entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			fixture := newNativeImageTaskFixture(t, "alibaba", func(w http.ResponseWriter, r *http.Request) {
				_, err := io.Copy(io.Discard, r.Body)
				if !assert.NoError(t, err) {
					return
				}
				close(entered)
				select {
				case <-r.Context().Done():
					close(cancelled)
				case <-release:
				}
			})
			t.Setenv("GATEWAY_MAX_ATTEMPTS", "1")
			t.Setenv("GATEWAY_ATTEMPT_FIRST_BYTE_SECONDS", "1")
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			request := httptest.NewRequest(http.MethodPost, "/ali/api/v1/services/aigc/multimodal-generation/generation", strings.NewReader(`{"model":"gateway-alias","input":{"messages":[{"role":"user","content":[{"text":"A bird"}]}]},"parameters":{"enable_interleave":true,"max_images":4}}`)).WithContext(parent)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
			request.Header.Set("X-DashScope-Sse", "enable")
			finished := make(chan struct{})
			go func() {
				fixture.engine.ServeHTTP(httptest.NewRecorder(), request)
				close(finished)
			}()
			defer func() { close(release); <-finished }()
			guard, stop := context.WithTimeout(t.Context(), 10*time.Second)
			defer stop()
			select {
			case <-entered:
			case <-guard.Done():
				require.FailNow(t, "model request did not reach the controlled Provider")
			}
			if cause == "single second budget" {
				fixture.fake.Advance(time.Second)
			} else {
				cancel()
			}
			select {
			case <-cancelled:
			case <-guard.Done():
				require.FailNow(t, "actual Provider request ignored the original budget or caller cancellation")
			}
			select {
			case <-finished:
			case <-guard.Done():
				require.FailNow(t, "submission did not finish after its Provider request was cancelled")
			}
		})
	}
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
