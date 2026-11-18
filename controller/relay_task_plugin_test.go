package controller

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/capability"
	"github.com/QuantumNous/new-api/pkg/clock"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestPresentTaskSubmissionUsesNativePresenterAfterPersistence(t *testing.T) {
	plugin, err := pluginruntime.CompilePlugin(`
export const meta = {apiVersion:1,key:"presenter-test",name:"Presenter",version:"1.0.0",author:{name:"Test"},models:["model"],fetchMode:"per_task",routes:[{method:"POST",path:"/vendor/jobs",type:"submit",decode:"decode",render:"created"}]};
export const native = {decode:function(ctx){return {kind:"submit",model:"model",requestBody:ctx.body.value};},created:function(ctx,task){return {data:{task_id:task.task_id},upstream:task.data};}};
export function buildSubmitRequest(){return {}} export function parseSubmitResponse(){return {taskId:"upstream"}} export function buildQueryRequest(){return {}} export function parseTaskResult(){return {status:"SUCCESS"}}
`, pluginruntime.Options{})
	require.NoError(t, err)
	priceData := types.PriceData{}
	priceData.AddOtherRatio("seconds", 5)
	task := &model.Task{TaskID: "task_public", SubmitTime: 123}
	task.SetData(map[string]any{"task_id": "upstream_private"})
	outcome := &taskSubmissionOutcome{
		Result:    &relay.TaskSubmitResult{},
		Task:      task,
		RelayInfo: &relaycommon.RelayInfo{PriceData: priceData},
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/vendor/jobs", strings.NewReader(`{"model":"model"}`))
	c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{Plugin: plugin, Route: plugin.Meta.Routes[0]})
	c.Set(pluginruntime.ContextKeyRouteRequest, pluginruntime.RouteRequestContext{Path: "/vendor/jobs", Method: http.MethodPost, Body: map[string]any{"kind": "json", "value": map[string]any{"model": "model"}}})

	presentTaskSubmission(c, outcome)

	assert.JSONEq(t, `{
		"data":{"task_id":"task_public"},
		"upstream":{"task_id":"upstream_private"}
	}`, recorder.Body.String())
	assert.JSONEq(t, `{"seconds":5}`, recorder.Header().Get("X-New-Api-Other-Ratios"))
}

// The submit presenter may answer with the vendor's status; a native route that
// already relayed the vendor's event stream gets nothing more.
func TestPresentTaskSubmissionHonorsReplyWrapperAndRelayedStreams(t *testing.T) {
	plugin, err := pluginruntime.CompilePlugin(`
export const meta = {apiVersion:1,key:"accepted-test",name:"Accepted",version:"1.0.0",author:{name:"Test"},models:["model"],fetchMode:"per_task",routes:[{method:"POST",path:"/vendor/jobs",type:"submit",decode:"decode",render:"created"}]};
export const native = {decode:function(ctx){return {kind:"submit",model:"model",requestBody:ctx.body.value};},created:function(ctx,task){return {__status:202,__body:{id:task.task_id}};}};
export function buildSubmitRequest(){return {}} export function parseSubmitResponse(){return {taskId:"upstream"}} export function buildQueryRequest(){return {}} export function parseTaskResult(){return {status:"SUCCESS"}}
`, pluginruntime.Options{})
	require.NoError(t, err)
	present := func(recorder *httptest.ResponseRecorder, written bool) {
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/vendor/jobs", strings.NewReader(`{"model":"model"}`))
		c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{Plugin: plugin, Route: plugin.Meta.Routes[0]})
		c.Set(pluginruntime.ContextKeyRouteRequest, pluginruntime.RouteRequestContext{Path: "/vendor/jobs", Method: http.MethodPost, Body: map[string]any{"kind": "json", "value": map[string]any{"model": "model"}}})
		if written {
			_, _ = c.Writer.WriteString("data: {}\n\n")
		}
		presentTaskSubmission(c, &taskSubmissionOutcome{Result: &relay.TaskSubmitResult{}, Task: &model.Task{TaskID: "task_public"}, RelayInfo: &relaycommon.RelayInfo{}})
	}

	accepted := httptest.NewRecorder()
	present(accepted, false)
	assert.Equal(t, http.StatusAccepted, accepted.Code)
	assert.JSONEq(t, `{"id":"task_public"}`, accepted.Body.String())

	streamed := httptest.NewRecorder()
	present(streamed, true)
	assert.Equal(t, "data: {}\n\n", streamed.Body.String())
}

func TestPresentTaskSubmissionFallbackUsesPersistedPublicID(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	outcome := &taskSubmissionOutcome{
		Result:    &relay.TaskSubmitResult{},
		Task:      &model.Task{TaskID: "task_persisted", SubmitTime: 456},
		RelayInfo: &relaycommon.RelayInfo{OriginModelName: "video-model"},
	}

	presentTaskSubmission(c, outcome)

	assert.JSONEq(t, `{
		"id":"task_persisted",
		"task_id":"task_persisted",
		"status":"queued",
		"model":"video-model",
		"created_at":456
	}`, recorder.Body.String())
}

func TestPresentTaskSubmissionUsesHostOpenAIVideoCreateReceipt(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set(pluginruntime.ContextKeyPinnedEndpoint, pluginruntime.PinnedEndpoint{
		Protocol:  "openai_video",
		Operation: pluginruntime.HostProtocolOperation{Name: "create"},
	})
	task := &model.Task{
		TaskID:     "task_public",
		Status:     model.TaskStatusSubmitted,
		Progress:   "0%",
		CreatedAt:  456,
		Properties: model.Properties{OriginModelName: "video-model"},
	}
	outcome := &taskSubmissionOutcome{Result: &relay.TaskSubmitResult{}, Task: task, RelayInfo: &relaycommon.RelayInfo{}}

	presentTaskSubmission(c, outcome)

	assert.JSONEq(t, `{"id":"task_public","object":"video","model":"video-model","status":"queued","progress":0,"created_at":456}`, recorder.Body.String())
	assert.NotContains(t, recorder.Body.String(), "task_id")
}

func canonicalSubmissionPlugin(retain bool) string {
	return fmt.Sprintf(`
export const meta={apiVersion:1,key:"kling",name:"Submission contracts",version:"1.2.3",author:{name:"Community Author",url:"https://plugins.example/author"},models:["kling-v3"],channelTypes:[%d],fetchMode:"per_task",usageSchema:{units:{type:"number",unit:"count"},missing:{type:"number",unit:"count"}},routes:[{method:"POST",path:"/kling/v1/videos/text2video",type:"submit",decode:"decode",render:"created",retainResult:%t}]};
export const native={decode:ctx=>({kind:"submit",model:"kling-v3",action:"text_to_video",requestBody:ctx.body.value}),created:(ctx,task)=>({id:task.task_id})};
export function extractUsage(){return {units:4};}
export function extractCapabilities(){return {input_modalities:["text"],output_modalities:["video"]};}
export function buildSubmitRequest(ctx){return {method:"POST",url:ctx.baseUrl+"/compile",body:ctx.requestBody};}
export function parseSubmitResponse(ctx,resp){const result={taskId:resp.body.task_id||"upstream_private",taskData:resp.body};if(resp.body.status)result.immediate={status:resp.body.status,progress:"100%%",reason:"provider rejected job"};return result;}
export function extractUsageOnComplete(ctx,result,body){return body.usage;}
export function parseTaskResult(){throw new Error("completed submission must not poll");}
export function buildQueryRequest(){throw new Error("completed submission must not poll");}
`, constant.ChannelTypeKling, retain)
}

func submissionResponse(body string, calls *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			calls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func assertSubmissionWallet(t *testing.T, fixture *nativeGatewayTaskFixture, nanos int64) {
	t.Helper()
	wallet, err := model.GetUserWallet(fixture.user.Id)
	require.NoError(t, err)
	assert.EqualValues(t, nanos, wallet.Balance.Nanos)
	var user model.User
	var token model.Token
	require.NoError(t, fixture.db.First(&user, fixture.user.Id).Error)
	require.NoError(t, fixture.db.First(&token, fixture.token.Id).Error)
	assert.Equal(t, 1000000, user.Quota)
	assert.Equal(t, 7654321, token.RemainQuota)
	assert.Zero(t, user.UsedQuota)
	assert.Zero(t, token.UsedQuota)
	var logs int64
	require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("user_id = ?", user.Id).Count(&logs).Error)
	assert.Zero(t, logs, "canonical consumption must not revive legacy consume logs")
	reconciliation, err := model.ReconcileWallets(100)
	require.NoError(t, err)
	assert.Empty(t, reconciliation.Mismatches)
}

func TestExecuteTaskSubmissionRefundsWhenInsertFails(t *testing.T) {
	var calls atomic.Int32
	fixture := newNativeGatewayTaskFixture(t, submissionResponse(`{"task_id":"upstream_private"}`, &calls), canonicalSubmissionPlugin(true))
	require.NoError(t, fixture.db.Exec(`CREATE FUNCTION reject_submission_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'task insert rejected'; END $$`).Error)
	require.NoError(t, fixture.db.Exec(`CREATE TRIGGER reject_submission_insert BEFORE INSERT ON tasks FOR EACH ROW EXECUTE FUNCTION reject_submission_insert()`).Error)
	c, _ := fixture.request(t)
	outcome, taskErr := executeTaskSubmission(c, nil)
	assert.Nil(t, outcome)
	require.NotNil(t, taskErr)
	assert.Equal(t, "task_persistence_failed", taskErr.Code)
	assert.Zero(t, calls.Load(), "task identity and sole reservation are atomic before Provider I/O")
	for _, table := range []any{&model.Task{}, &model.GatewayBilling{}} {
		var count int64
		require.NoError(t, fixture.db.Model(table).Count(&count).Error)
		assert.Zero(t, count)
	}
	var holds int64
	require.NoError(t, fixture.db.Model(&model.LedgerEntry{}).Where("type IN ?", []model.LedgerEntryType{model.LedgerEntryPreConsume, model.LedgerEntryPreConsumeRelease}).Count(&holds).Error)
	assert.Zero(t, holds, "rolled back reservation needs no compensating second write")
	assertSubmissionWallet(t, fixture, 2000000000)
	assert.False(t, c.Writer.Written())
}

func TestExecuteTaskSubmissionSettlementFailureStaysDurableAndWritesNothing(t *testing.T) {
	fixture := newNativeGatewayTaskFixture(t, submissionResponse(`{"status":"SUCCESS","usage":{"units":2}}`, nil), canonicalSubmissionPlugin(true))
	require.NoError(t, fixture.db.Exec(`CREATE FUNCTION reject_submission_cost() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.account_type='provider_payable' THEN RAISE EXCEPTION 'cost posting rejected'; END IF; RETURN NEW; END $$`).Error)
	require.NoError(t, fixture.db.Exec(`CREATE TRIGGER reject_submission_cost AFTER INSERT ON ledger_entries FOR EACH ROW EXECUTE FUNCTION reject_submission_cost()`).Error)
	outcome, c, _ := fixture.submit(t)
	var stored model.Task
	require.NoError(t, fixture.db.First(&stored, outcome.Task.ID).Error)
	assert.True(t, stored.PrivateData.Gateway.Accepted)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), stored.Status)
	var billing model.GatewayBilling
	require.NoError(t, fixture.db.First(&billing, "request_id = ?", stored.PrivateData.Gateway.Settlement.Request.ID).Error)
	assert.Equal(t, "settlement_pending", billing.Phase)
	assert.NotEmpty(t, billing.Settlement, "immutable final facts survive posting failure")
	assertSubmissionWallet(t, fixture, 1500000000)
	assert.False(t, c.Writer.Written())
	require.NoError(t, fixture.db.Exec(`DROP TRIGGER reject_submission_cost ON ledger_entries`).Error)
	_, err := model.RecoverGatewaySettlements(t.Context(), 20)
	require.NoError(t, err)
	assertSubmissionWallet(t, fixture, 1750000000)
	var before, after []model.LedgerEntry
	require.NoError(t, fixture.db.Where("request_id = ?", billing.RequestID).Order("id").Find(&before).Error)
	require.Len(t, before, 5)
	_, err = model.RecoverGatewaySettlements(t.Context(), 20)
	require.NoError(t, err)
	require.NoError(t, fixture.db.Where("request_id = ?", billing.RequestID).Order("id").Find(&after).Error)
	assert.Equal(t, before, after)
}

func TestExecuteTaskSubmissionPersistsPinnedPluginProvenance(t *testing.T) {
	fixture := newNativeGatewayTaskFixture(t, submissionResponse(`{"task_id":"upstream-private"}`, nil), canonicalSubmissionPlugin(true))
	c, _ := fixture.request(t)
	c.Set(common.RequestIdKey, "request-public")
	outcome, taskErr := executeTaskSubmission(c, nil)
	require.Nil(t, taskErr)
	require.NotNil(t, outcome)
	var stored model.Task
	require.NoError(t, fixture.db.First(&stored, outcome.Task.ID).Error)
	require.NotNil(t, stored.PrivateData.Execution)
	provenance := stored.PrivateData.Execution
	require.NotNil(t, provenance.TaskPlugin)
	assert.Equal(t, "request-public", provenance.RequestID)
	assert.Equal(t, "/kling/v1/videos/text2video", provenance.RequestPath)
	assert.Equal(t, "kling", provenance.TaskPlugin.Key)
	assert.Equal(t, "1.2.3", provenance.TaskPlugin.Version)
	assert.Equal(t, fixture.generation.Number, provenance.TaskPlugin.Generation)
	require.NotNil(t, provenance.TaskPlugin.Author)
	assert.Equal(t, "Community Author", provenance.TaskPlugin.Author.Name)
	assert.Equal(t, "https://plugins.example/author", provenance.TaskPlugin.Author.URL)
	assert.Equal(t, "upstream-private", stored.PrivateData.UpstreamTaskID)
	assert.NotEqual(t, stored.PrivateData.UpstreamTaskID, stored.TaskID)
	assert.Empty(t, stored.PrivateData.Key)
	assertSubmissionWallet(t, fixture, 1500000000)
	assert.False(t, c.Writer.Written())
}

func TestExecuteTaskSubmissionHonorsRouteRetainResult(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		discarded    bool
	}{
		{"immediate success is discarded", "SUCCESS", true}, {"immediate failure is discarded", "FAILURE", true}, {"asynchronous result is retained", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"status":"` + tc.status + `","usage":{"units":2},"data":[{"url":"https://cdn.example/a.png"}]}`
			fixture := newNativeGatewayTaskFixture(t, submissionResponse(body, nil), canonicalSubmissionPlugin(false))
			outcome, _, _ := fixture.submit(t)
			assert.JSONEq(t, body, string(outcome.Task.Data))
			assert.Equal(t, tc.discarded, outcome.Task.PrivateData.ResultDiscarded)
			assert.Equal(t, !tc.discarded, outcome.Task.ResultRetrievable())
			var stored model.Task
			require.NoError(t, fixture.db.First(&stored, outcome.Task.ID).Error)
			assert.Equal(t, tc.discarded, stored.PrivateData.ResultDiscarded)
			var nulls int64
			require.NoError(t, fixture.db.Model(&model.Task{}).Where("id = ? AND data IS NULL", stored.ID).Count(&nulls).Error)
			if tc.discarded {
				assert.EqualValues(t, 1, nulls)
				artifacts, err := projectTaskArtifacts(&stored)
				require.NoError(t, err)
				assert.Empty(t, artifacts)
			} else {
				assert.Zero(t, nulls)
				assert.JSONEq(t, body, string(stored.Data))
			}
			listed := model.TaskGetAllUserTask(fixture.user.Id, 0, 10, model.SyncTaskQueryParams{})
			require.Len(t, listed, 1)
			assert.Empty(t, listed[0].Data)
		})
	}
}

func TestExecuteTaskSubmissionRefundsCancellationBeforeDurableBarrier(t *testing.T) {
	var calls atomic.Int32
	fixture := newNativeGatewayTaskFixture(t, submissionResponse(`{"task_id":"private"}`, &calls), canonicalSubmissionPlugin(true))
	c, _ := fixture.request(t)
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	require.NoError(t, fixture.db.Callback().Create().After("gorm:create").Register("test:cancel-reservation", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "tasks" {
			cancel()
		}
	}))
	t.Cleanup(func() { _ = fixture.db.Callback().Create().Remove("test:cancel-reservation") })
	outcome, taskErr := executeTaskSubmission(c, nil)
	assert.Nil(t, outcome)
	require.NotNil(t, taskErr)
	assert.Zero(t, calls.Load())
	assertSubmissionWallet(t, fixture, 2000000000)
	var holds, releases int64
	require.NoError(t, fixture.db.Model(&model.LedgerEntry{}).Where("type = ?", model.LedgerEntryPreConsume).Count(&holds).Error)
	require.NoError(t, fixture.db.Model(&model.LedgerEntry{}).Where("type = ?", model.LedgerEntryPreConsumeRelease).Count(&releases).Error)
	assert.EqualValues(t, 1, holds)
	assert.EqualValues(t, 1, releases)
	assert.False(t, c.Writer.Written())
}

func TestExecuteTaskSubmissionDisconnectBeforeUpstreamAcceptanceSkipsSubmitAndRefunds(t *testing.T) {
	var calls atomic.Int32
	fixture := newNativeGatewayTaskFixture(t, submissionResponse(`{"task_id":"private"}`, &calls), canonicalSubmissionPlugin(true))
	c, _ := fixture.request(t)
	ctx, cancel := context.WithCancel(c.Request.Context())
	cancel()
	c.Request = c.Request.WithContext(ctx)
	outcome, taskErr := executeTaskSubmission(c, nil)
	assert.Nil(t, outcome)
	require.NotNil(t, taskErr)
	assert.Zero(t, calls.Load())
	var rows int64
	require.NoError(t, fixture.db.Model(&model.GatewayBilling{}).Count(&rows).Error)
	assert.Zero(t, rows)
	assertSubmissionWallet(t, fixture, 2000000000)
	assert.False(t, c.Writer.Written())
}

func TestExecuteTaskSubmissionCallerCancellationDuringSubmitRefundsBeforeDurableBarrier(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	fixture := newNativeGatewayTaskFixture(t, func(w http.ResponseWriter, r *http.Request) { close(started); <-release }, canonicalSubmissionPlugin(true))
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	c, _ := fixture.request(t)
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	done := make(chan struct{})
	var outcome *taskSubmissionOutcome
	var taskErr *dto.TaskError
	go func() { defer close(done); outcome, taskErr = executeTaskSubmission(c, nil) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "submission did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "submission did not stop after disconnect")
	}
	close(release)
	assert.Nil(t, outcome)
	require.NotNil(t, taskErr)
	// A transmitted operation with no acknowledgement is not a proven rejection.
	// The canonical acceptance boundary preserves its sole hold for recovery.
	var stored model.Task
	require.NoError(t, fixture.db.Take(&stored).Error)
	require.NotNil(t, stored.PrivateData.Gateway)
	assert.False(t, stored.PrivateData.Gateway.Accepted)
	assert.Equal(t, model.TaskStatus(model.TaskStatusUnknown), stored.Status)
	assertSubmissionWallet(t, fixture, 1500000000)
	var releases int64
	require.NoError(t, fixture.db.Model(&model.LedgerEntry{}).Where("type = ?", model.LedgerEntryPreConsumeRelease).Count(&releases).Error)
	assert.Zero(t, releases)
	assert.False(t, c.Writer.Written())
}

func TestExecuteTaskSubmissionDisconnectAfterDurableInsertDoesNotRefund(t *testing.T) {
	fixture := newNativeGatewayTaskFixture(t, submissionResponse(`{"task_id":"upstream_private"}`, nil), canonicalSubmissionPlugin(true))
	c, _ := fixture.request(t)
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	require.NoError(t, fixture.db.Callback().Update().After("gorm:update").Register("test:cancel-accepted", func(tx *gorm.DB) {
		if task, ok := tx.Statement.Model.(*model.Task); ok && task.PrivateData.Gateway != nil && task.PrivateData.Gateway.Accepted {
			cancel()
		}
	}))
	t.Cleanup(func() { _ = fixture.db.Callback().Update().Remove("test:cancel-accepted") })
	outcome, taskErr := executeTaskSubmission(c, nil)
	require.Nil(t, taskErr)
	require.NotNil(t, outcome)
	assert.ErrorIs(t, ctx.Err(), context.Canceled, "the test must cross the accepted durable boundary")
	var stored model.Task
	require.NoError(t, fixture.db.First(&stored, outcome.Task.ID).Error)
	assert.True(t, stored.PrivateData.Gateway.Accepted)
	assert.Equal(t, "upstream_private", stored.PrivateData.UpstreamTaskID)
	assertSubmissionWallet(t, fixture, 1500000000)
	assert.False(t, c.Writer.Written())
}

func setupTaskSubmissionDatabase(t *testing.T, migrate bool, events *[]string) *gorm.DB {
	t.Helper()
	previousDB := model.DB
	var models []any
	if migrate {
		models = append(models, &model.Task{})
	}
	database := openTaskTestDatabase(t, models...)
	require.NoError(t, database.Callback().Create().Before("gorm:create").Register("test:task-submit-order", func(*gorm.DB) {
		*events = append(*events, "insert")
	}))
	model.DB = database
	t.Cleanup(func() { model.DB = previousDB })
	return database
}

func taskSubmissionTestContext() *gin.Context {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/plugin/submit", strings.NewReader(`{}`))
	return c
}

// openTaskTestDatabase opens an empty PostgreSQL schema, migrates the given
// models and returns the connection without changing model.DB.
func openTaskTestDatabase(t *testing.T, models ...any) *gorm.DB {
	t.Helper()
	db, _ := newAuditTestDatabase(t, "postgres")
	require.NoError(t, db.AutoMigrate(models...))
	return db
}

// Uses the Offering gateway, submit adaptor, immutable PriceVersions, sole
// reservation and exact ledger; missing completion quantities remain pending.
func TestImmediateTaskSettlementDatabase(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		actual       any
		nanos        int64
		pending      bool
	}{
		{"partial", "SUCCESS", 2, 250000000, false}, {"zero", "SUCCESS", 0, 0, false}, {"larger", "SUCCESS", 6, 750000000, false},
		{"invalid usage", "SUCCESS", -1, 0, true}, {"expression failure", "SUCCESS", 7, 0, true}, {"negative result", "SUCCESS", 8, 0, true}, {"missing usage", "SUCCESS", nil, 0, true}, {"failed", "FAILURE", 9, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"status": tc.status}
			if tc.actual != nil {
				body["usage"] = map[string]any{"units": tc.actual}
			}
			encoded, err := common.Marshal(body)
			require.NoError(t, err)
			fixture := newNativeGatewayTaskFixture(t, submissionResponse(string(encoded), nil), canonicalSubmissionPlugin(true))
			const expression = `u("units") == 7 ? tier("missing", u("missing") * 1.0) : u("units") == 8 ? tier("invalid", -1.0) : tier("units", u("units") * 0.125)`
			fixture.sale = &model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: fixture.offering.ID, Currency: "USD", Expression: expression, EffectiveAt: time.Now().Unix()}
			require.NoError(t, model.CreatePriceVersion(t.Context(), fixture.sale, fixture.user.Id))
			outcome, c, _ := fixture.submit(t)
			var stored model.Task
			require.NoError(t, fixture.db.First(&stored, outcome.Task.ID).Error)
			assert.Zero(t, stored.Quota)
			assert.Equal(t, model.TaskStatus(tc.status), stored.Status)
			assert.Positive(t, stored.FinishTime)
			var billing model.GatewayBilling
			require.NoError(t, fixture.db.First(&billing, "request_id = ?", stored.PrivateData.Gateway.Settlement.Request.ID).Error)
			if tc.pending {
				assert.Equal(t, "evaluation_failed", billing.Phase)
				assertSubmissionWallet(t, fixture, 1500000000)
				_, err = model.SettleGateway(billing.RequestID)
				assert.ErrorIs(t, err, model.ErrGatewayEvaluationPending)
			} else {
				assert.Equal(t, "settled", billing.Phase)
				assertSubmissionWallet(t, fixture, 2000000000-tc.nanos)
				record, err := model.GetGatewayRequest(t.Context(), billing.RequestID, fixture.user.Id)
				require.NoError(t, err)
				assert.EqualValues(t, tc.nanos, record.Receivable.Nanos)
				_, err = model.SettleGateway(billing.RequestID)
				require.NoError(t, err)
				assertSubmissionWallet(t, fixture, 2000000000-tc.nanos)
			}
			assert.False(t, c.Writer.Written())
		})
	}
}

func TestAcceptedSubmitStreamNeverRetries(t *testing.T) {
	c := taskSubmissionTestContext()
	assert.Equal(t, service.PolicyDecision{Action: "stop", Reason: "task_accepted", Source: "system"}, decideTaskRetry(c, &dto.TaskError{StatusCode: 502, LocalError: true, NoRetry: true}, 3))
}

// Local task rejections carry a message but no cause; the response and the
// decision record must still be produced.
func TestRespondTaskSubmissionErrorWithoutCause(t *testing.T) {
	previousErrorLog := constant.ErrorLogEnabled
	constant.ErrorLogEnabled = false
	t.Cleanup(func() { constant.ErrorLogEnabled = previousErrorLog })
	c := taskSubmissionTestContext()
	taskErr := &dto.TaskError{Code: "get_channel_failed", Message: "no channel", StatusCode: http.StatusServiceUnavailable, LocalError: true}
	require.NotPanics(t, func() { respondTaskSubmissionError(c, taskErr) })
	assert.Equal(t, http.StatusServiceUnavailable, c.Writer.Status())
	events := service.RequestPolicy(c).Events()
	require.Len(t, events, 1)
	assert.Equal(t, service.PolicyDecision{Action: "stop", Reason: "request_failed", Source: "system"}, events[0].Decision)
	assert.Equal(t, http.StatusServiceUnavailable, events[0].Status)
	assert.Equal(t, service.PolicyDecision{Action: "stop", Reason: "local_rejection", Source: "system"}, decideTaskRetry(c, &dto.TaskError{StatusCode: http.StatusForbidden, LocalError: true, Message: "billing"}, 2), "local errors stop once the status rules do not force a retry")
}

func TestExecuteTaskSubmissionRefundsWhenFinalReserveFails(t *testing.T) {
	var calls atomic.Int32
	fixture := newNativeGatewayTaskFixture(t, submissionResponse(`{"status":"SUCCESS","usage":{"units":6}}`, &calls), canonicalSubmissionPlugin(true))
	// Insufficient funds reject the sole reservation before any submit, not a
	// second post-acceptance reserve followed by an invented refund.
	_, err := model.PostLedgerEntries(model.LedgerPosting{Account: model.UserWalletAccount(fixture.user.Id), Type: model.LedgerEntryAdjustment, Amount: money.Amount{Nanos: -1900000000, Currency: "USD"}, IdempotencyKey: "submission-insufficient", Remark: "fixture balance"})
	require.NoError(t, err)
	c, _ := fixture.request(t)
	outcome, taskErr := executeTaskSubmission(c, nil)
	assert.Nil(t, outcome)
	require.NotNil(t, taskErr)
	assert.Equal(t, http.StatusPaymentRequired, taskErr.StatusCode)
	assert.Zero(t, calls.Load())
	var rows int64
	require.NoError(t, fixture.db.Model(&model.GatewayBilling{}).Count(&rows).Error)
	assert.Zero(t, rows)
	assertSubmissionWallet(t, fixture, 100000000)
	assert.False(t, c.Writer.Written())
}

func submitCanonicalNativeBody(t *testing.T, fixture *nativeGatewayTaskFixture, body string) (*taskSubmissionOutcome, *dto.TaskError, *httptest.ResponseRecorder) {
	t.Helper()
	require.NoError(t, i18n.Init())
	binding, found := fixture.generation.LookupDeclaredRoute(http.MethodPost, "/kling/v1/videos/text2video")
	require.True(t, found)
	var outcome *taskSubmissionOutcome
	var taskErr *dto.TaskError
	engine := gin.New()
	engine.Use(middleware.RequestId(), middleware.TokenAuth(), func(c *gin.Context) {
		c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{Generation: fixture.generation, Plugin: binding.Plugin, Route: binding.Route})
	}, middleware.ResolveTaskOffering(), middleware.ResolveTaskPluginOffering(), AuthorizeTaskGateway, middleware.PrepareTaskPluginRoute())
	engine.POST("/kling/v1/videos/text2video", func(c *gin.Context) { outcome, taskErr = executeTaskSubmission(c, nil) })
	request := httptest.NewRequest(http.MethodPost, "/kling/v1/videos/text2video", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer sk-"+fixture.token.Key)
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	return outcome, taskErr, recorder
}

func assertCanonicalTaskMapping(t *testing.T, alias, mapping, rewrite string) {
	t.Helper()
	var wire map[string]any
	var calls atomic.Int32
	source := strings.Replace(canonicalSubmissionPlugin(true), `model:"kling-v3",action:`, `model:ctx.body.value.model_name,action:`, 1)
	source = strings.Replace(source, `body:ctx.requestBody};`, `body:{upstreamModel:ctx.upstreamModel,model:ctx.model},rewriteModel:`+strconv.Quote(rewrite)+`};`, 1)
	fixture := newNativeGatewayTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.NoError(t, common.DecodeJson(r.Body, &wire))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"task_id":"mapped-private","status":"SUCCESS","usage":{"units":2}}`)
	}, source)
	fixture.offering.Aliases = []string{alias}
	require.NoError(t, model.SaveModelOffering(t.Context(), fixture.offering, "save"))
	require.NoError(t, fixture.db.Model(&model.Provider{}).Where("id = ?", fixture.provider.Id).Update("model_mapping", mapping).Error)
	// The sole current mapping is the Channel, not a recursive Provider alias chain.
	require.NoError(t, fixture.db.Model(&model.Channel{}).Where("id = ?", fixture.channel.ID).Update("upstream_model_name", "kling-v3").Error)
	outcome, taskErr, response := submitCanonicalNativeBody(t, fixture, `{"model_name":`+strconv.Quote(alias)+`,"prompt":"mapping"}`)
	require.Nil(t, taskErr, response.Body.String())
	require.NotNil(t, outcome, response.Body.String())
	assert.EqualValues(t, 1, calls.Load())
	assert.Equal(t, "kling-v3", wire["upstreamModel"])
	assert.Equal(t, alias, outcome.Task.Properties.OriginModelName)
	assert.Equal(t, "kling-v3", outcome.Task.Properties.UpstreamModelName)
	assert.Equal(t, fixture.sale.ID, outcome.Task.PrivateData.Gateway.Settlement.Request.SalePriceVersionID)
	assertSubmissionWallet(t, fixture, 1750000000)
}

func TestRelayTaskSubmitMapsBeforeValidateWhenOriginSet(t *testing.T) {
	assertCanonicalTaskMapping(t, "alias-model", `{"alias-model":"mid-model","mid-model":"wrong-model"}`, "")
}
func TestRelayTaskSubmitDeclaredNameWithoutMappingIsUnchanged(t *testing.T) {
	assertCanonicalTaskMapping(t, "kling-v3", `{}`, "")
}
func TestRelayTaskSubmitDoesNotApplyMappingTwice(t *testing.T) {
	assertCanonicalTaskMapping(t, "alias-model", `{"alias-model":"kling-v3","kling-v3":"wrong-model"}`, "kling-v3")
}
func TestRelayTaskSubmitEmptyOriginKeepsLateMapping(t *testing.T) {
	var calls atomic.Int32
	source := strings.Replace(canonicalSubmissionPlugin(true), `model:"kling-v3",action:`, `model:ctx.body.value.model_name,action:`, 1)
	fixture := newNativeGatewayTaskFixture(t, submissionResponse(`{"task_id":"must-not-submit"}`, &calls), source)
	outcome, taskErr, response := submitCanonicalNativeBody(t, fixture, `{"prompt":"no authorized Offering"}`)
	assert.Nil(t, outcome)
	assert.Nil(t, taskErr)
	assert.Equal(t, http.StatusBadRequest, response.Code)
	assert.Zero(t, calls.Load())
	assertSubmissionWallet(t, fixture, 2000000000)
	var count int64
	require.NoError(t, fixture.db.Model(&model.GatewayBilling{}).Count(&count).Error)
	assert.Zero(t, count, "no action-derived legacy fallback may synthesize an authorization or hold")
}

func TestRelayTaskSubmitAcceptsAnySuccessfulUpstreamStatus(t *testing.T) {
	for _, status := range []int{200, 201, 202, 400, 502} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var calls atomic.Int32
			source := strings.Replace(canonicalSubmissionPlugin(true), `taskData:resp.body`, `taskData:{status:resp.statusCode}`, 1)
			fixture := newNativeGatewayTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"task_id":"job-42","message":"upstream-private-message"}`)
			}, source)
			c, response := fixture.request(t)
			outcome, taskErr := executeTaskSubmission(c, nil)
			assert.EqualValues(t, 1, calls.Load())
			assert.Empty(t, response.Body.String())
			if status/100 != 2 {
				require.NotNil(t, taskErr)
				assert.Equal(t, status, taskErr.StatusCode)
				assert.Nil(t, outcome)
				assert.NotContains(t, taskErr.Message, "upstream-private-message")
				assertSubmissionWallet(t, fixture, 2000000000)
				return
			}
			require.Nil(t, taskErr)
			require.NotNil(t, outcome)
			assert.Equal(t, "job-42", outcome.Task.GetUpstreamTaskID())
			assert.JSONEq(t, `{"status":`+strconv.Itoa(status)+`}`, string(outcome.Task.Data))
			assert.True(t, outcome.Task.PrivateData.Gateway.Accepted)
			assertSubmissionWallet(t, fixture, 1500000000)
			var billing model.GatewayBilling
			require.NoError(t, fixture.db.First(&billing).Error)
			assert.Equal(t, "reserved", billing.Phase)
			var count int64
			require.NoError(t, fixture.db.Model(&model.LedgerEntry{}).Where("request_id = ? AND type = ?", billing.RequestID, model.LedgerEntryPreConsume).Count(&count).Error)
			assert.EqualValues(t, 1, count)
		})
	}
}

func TestRelayTaskSubmitRetryRepricesAndReservesBeforeUpstream(t *testing.T) {
	for _, tc := range []struct {
		name      string
		nextUnits int
		cash      int64
		wantCalls []string
	}{
		{"per-call plugin drops the previous expression", 2, 2000000000, []string{"a", "b"}},
		{"pricier plugin is held before submission", 8, 2000000000, []string{"a"}},
		{"unaffordable plugin is never submitted", 8, 500000000, []string{"a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			var held []int64
			var fixture *nativeGatewayTaskFixture
			source := strings.Replace(canonicalSubmissionPlugin(true), `models:["kling-v3"]`, `models:["kling-v3","retry-secondary"]`, 1)
			source = strings.Replace(source, `extractUsage(){return {units:4};}`, fmt.Sprintf(`extractUsage(ctx){return {units:ctx.baseUrl.endsWith("/b")?%d:4};}`, tc.nextUnits), 1)
			fixture = newNativeGatewayTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
				name := "a"
				if strings.HasPrefix(r.URL.Path, "/b/") {
					name = "b"
				}
				calls = append(calls, name)
				var billing model.GatewayBilling
				require.NoError(t, fixture.db.First(&billing).Error)
				held = append(held, billing.Reservation.Nanos)
				wallet, err := model.GetUserWallet(fixture.user.Id)
				require.NoError(t, err)
				assert.Equal(t, tc.cash-500000000, wallet.Balance.Nanos)
				w.Header().Set("Content-Type", "application/json")
				if name == "a" {
					w.WriteHeader(503)
					_, _ = io.WriteString(w, `{"error":{"type":"server_error"}}`)
					return
				}
				_, _ = io.WriteString(w, `{"task_id":"second-original-provider","status":"SUCCESS","usage":{"units":2}}`)
			}, source)
			if tc.cash < 2000000000 {
				_, err := model.PostLedgerEntries(model.LedgerPosting{Account: model.UserWalletAccount(fixture.user.Id), Type: model.LedgerEntryAdjustment, Amount: money.Amount{Nanos: tc.cash - 2000000000, Currency: "USD"}, IdempotencyKey: "retry-cash"})
				require.NoError(t, err)
			}
			var original model.Model
			require.NoError(t, fixture.db.First(&original, fixture.channel.ModelID).Error)
			original.Id = 0
			original.ModelName = "retry-secondary"
			require.NoError(t, fixture.db.Create(&original).Error)
			address := *fixture.provider.BaseURL + "/b"
			provider := &model.Provider{Name: "second original Provider", Type: fixture.provider.Type, Key: "second-current-key", Status: common.ChannelStatusEnabled, BaseURL: &address}
			require.NoError(t, fixture.db.Create(provider).Error)
			channel := &model.Channel{ProviderID: provider.Id, ModelID: original.Id, Protocol: "task:kling", UpstreamModelName: "kling-v3", ParameterOverrides: `{}`, MappingConfirmed: true, Status: model.ChannelEnabled, Weight: 1}
			require.NoError(t, fixture.db.Create(channel).Error)
			price := &model.PriceVersion{OwnerType: model.PriceOwnerChannel, OwnerID: channel.ID, Currency: "USD", Expression: `tier("second",u("units")*0.1)`, EffectiveAt: clock.Now().Unix() - 1}
			require.NoError(t, model.CreatePriceVersion(t.Context(), price, fixture.user.Id))
			fixture.offering.Members = append(fixture.offering.Members, model.ModelOfferingMember{ModelID: original.Id, Tier: 2})
			require.NoError(t, model.SaveModelOffering(t.Context(), fixture.offering, "save"))
			c, _ := fixture.request(t)
			outcome, taskErr := executeTaskSubmission(c, nil)
			assert.Equal(t, tc.wantCalls, calls)
			for _, amount := range held {
				assert.EqualValues(t, 500000000, amount, "the first hold is immutable, even with extra wallet funds")
			}
			var billing model.GatewayBilling
			require.NoError(t, fixture.db.First(&billing).Error)
			var entries []model.LedgerEntry
			require.NoError(t, fixture.db.Where("request_id = ?", billing.RequestID).Find(&entries).Error)
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
			if len(tc.wantCalls) == 1 {
				require.NotNil(t, taskErr)
				assert.Nil(t, outcome)
				assertSubmissionWallet(t, fixture, tc.cash)
				return
			}
			require.Nil(t, taskErr)
			require.NotNil(t, outcome)
			assert.Equal(t, provider.Id, outcome.Task.PrivateData.Gateway.ProviderID)
			assert.Equal(t, price.ID, outcome.Task.PrivateData.Gateway.Settlement.Attempt.PurchasePriceVersionID)
			assert.Equal(t, fixture.sale.ID, outcome.Task.PrivateData.Gateway.Settlement.Request.SalePriceVersionID)
			assertSubmissionWallet(t, fixture, tc.cash-250000000)
			var costs int64
			require.NoError(t, fixture.db.Model(&model.LedgerEntry{}).Where("request_id = ? AND account_type = ?", billing.RequestID, model.LedgerAccountProviderPayable).Select("COALESCE(SUM(amount_nanos),0)").Scan(&costs).Error)
			assert.EqualValues(t, 200000000, costs)
		})
	}
}

func TestDistributeHonorsOriginTaskChannelPin(t *testing.T) {
	for _, cached := range []bool{false, true} {
		for _, mode := range []string{"allowed", "offering_denied", "foreign_owner", "disabled_provider"} {
			t.Run(fmt.Sprintf("cache=%t/%s", cached, mode), func(t *testing.T) { assertCanonicalOriginBoundary(t, cached, mode) })
		}
	}
}

func TestDistributeTokenPinBeatsOriginPin(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprintf("cache=%t", cached), func(t *testing.T) { assertCanonicalOriginBoundary(t, cached, "pin_conflict") })
	}
}

func TestDistributePinViolatingIdentityFilterErrors(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprintf("cache=%t", cached), func(t *testing.T) { assertCanonicalOriginBoundary(t, cached, "identity_mismatch") })
	}
}

func assertCanonicalOriginBoundary(t *testing.T, cached bool, mode string) {
	t.Helper()
	require.NoError(t, i18n.Init())
	source := strings.Replace(canonicalSubmissionPlugin(true), `model:"kling-v3",action:`, `model:ctx.body.value.model||"kling-v3",originTaskIds:ctx.body.value.parents||[],action:`, 1)
	source = strings.Replace(source, `body:ctx.requestBody`, `headers:{Authorization:ctx.authHeader},body:{model:ctx.upstreamModel,parent:(ctx.originTasks||[]).map(t=>t.upstreamTaskId)}`, 1)
	var calls atomic.Int32
	fixture := newNativeGatewayTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		var body map[string]any
		require.NoError(t, common.DecodeJsonStrict(r.Body, &body))
		assert.Equal(t, "kling-v3", body["model"])
		if call == 2 {
			assert.Equal(t, []any{"source_task_1"}, body["parent"])
		} else {
			assert.Empty(t, body["parent"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"task_id":"source_task_%d","status":"SUCCESS","usage":{"units":1}}`, call)
	}, source)
	common.MemoryCacheEnabled = cached
	model.InitProviderCache()
	binding, found := fixture.generation.LookupDeclaredRoute(http.MethodPost, "/kling/v1/videos/text2video")
	require.True(t, found)
	other := *fixture.provider
	other.Id = 0
	other.Name = "unrelated-provider"
	other.Key = "unrelated-provider-credential"
	require.NoError(t, fixture.db.Create(&other).Error)
	engine := gin.New()
	engine.Use(middleware.RequestId(), middleware.TaskOriginAuth())
	engine.POST("/kling/v1/videos/text2video", func(c *gin.Context) {
		c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{Generation: fixture.generation, Plugin: binding.Plugin, Route: binding.Route})
		if c.Request.Header.Get("X-Child") == "yes" && mode == "pin_conflict" {
			service.GetChannelConstraints(c).AddPin(dto.ProviderPin{ChannelId: other.Id, Source: dto.PinSourceToken, Rank: dto.PinRankToken, RetryMode: dto.PinRetrySingleAttempt})
		}
	}, middleware.ResolveTaskOffering(), middleware.ResolveTaskPluginOffering(), AuthorizeTaskGateway, middleware.PrepareTaskPluginRoute(), presentGatewayTaskSubmission)
	request := func(body, key string, child bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/kling/v1/videos/text2video", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+key)
		if child {
			r.Header.Set("X-Child", "yes")
		}
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, r)
		assert.NotContains(t, w.Body.String(), fixture.token.Key)
		assert.NotContains(t, w.Body.String(), other.Key)
		return w
	}
	parentResponse := request(`{"model":"kling-v3","prompt":"parent"}`, fixture.token.Key, false)
	require.Equal(t, http.StatusOK, parentResponse.Code, parentResponse.Body.String())
	var view struct {
		ID string `json:"id"`
	}
	require.NoError(t, common.Unmarshal(parentResponse.Body.Bytes(), &view))
	require.NotEmpty(t, view.ID)
	var parent model.Task
	require.NoError(t, fixture.db.First(&parent, "task_id = ?", view.ID).Error)
	require.NotNil(t, parent.PrivateData.Gateway)
	assert.Equal(t, fixture.provider.Id, parent.PrivateData.Gateway.ProviderID)
	assert.Equal(t, fixture.channel.ID, parent.PrivateData.Gateway.ChannelID)
	assertSubmissionWallet(t, fixture, 1_875_000_000)
	childOffering := &model.ModelOffering{Name: "child-offering", Members: []model.ModelOfferingMember{{ModelID: fixture.channel.ModelID, Tier: 1}}}
	require.NoError(t, model.SaveModelOffering(t.Context(), childOffering, "save"))
	require.NoError(t, model.CreatePriceVersion(t.Context(), &model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: childOffering.ID, Currency: "USD", Expression: fixture.sale.Expression, EffectiveAt: 1}, fixture.user.Id))
	require.NoError(t, model.SaveModelOffering(t.Context(), childOffering, "enable"))
	key := fixture.token.Key
	want := http.StatusOK
	switch mode {
	case "offering_denied":
		require.NoError(t, fixture.db.Create(&model.TokenOfferingPolicy{TokenID: fixture.token.Id, DefaultAllow: false}).Error)
		require.NoError(t, fixture.db.Create(&model.TokenOfferingException{TokenID: fixture.token.Id, OfferingID: fixture.offering.ID}).Error)
		want = http.StatusNotFound
	case "foreign_owner":
		u := model.User{Username: "foreign-origin", AffCode: "foreign-origin", Status: common.UserStatusEnabled, Group: "default", Role: common.RoleCommonUser}
		require.NoError(t, fixture.db.Create(&u).Error)
		token := model.Token{UserId: u.Id, Key: "foreign-origin-key", Status: common.TokenStatusEnabled, ExpiredTime: -1}
		require.NoError(t, fixture.db.Create(&token).Error)
		key = token.Key
		want = http.StatusBadRequest
	case "disabled_provider":
		require.NoError(t, fixture.db.Model(fixture.provider).Update("status", common.ChannelStatusManuallyDisabled).Error)
		model.InitProviderCache()
		want = http.StatusServiceUnavailable
	case "pin_conflict":
		want = http.StatusBadRequest
	case "identity_mismatch":
		require.NoError(t, fixture.db.Model(fixture.channel).Update("protocol", "task:sora").Error)
		want = http.StatusServiceUnavailable
	}
	response := request(fmt.Sprintf(`{"model":"child-offering","parents":[%q],"prompt":"child"}`, parent.TaskID), key, true)
	assert.Equal(t, want, response.Code, response.Body.String())
	if mode != "allowed" {
		assert.EqualValues(t, 1, calls.Load(), "rejection must precede Provider I/O")
		var count int64
		require.NoError(t, fixture.db.Model(&model.GatewayBilling{}).Count(&count).Error)
		assert.EqualValues(t, 1, count, "rejection must precede another lifecycle/reservation")
		assertSubmissionWallet(t, fixture, 1_875_000_000)
		return
	}
	assert.EqualValues(t, 2, calls.Load())
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &view))
	var child model.Task
	require.NoError(t, fixture.db.First(&child, "task_id = ?", view.ID).Error)
	require.NotNil(t, child.PrivateData.Gateway)
	assert.Equal(t, fixture.provider.Id, child.PrivateData.Gateway.ProviderID)
	assert.Equal(t, fixture.channel.ID, child.PrivateData.Gateway.ChannelID)
	assert.Equal(t, childOffering.ID, child.PrivateData.Gateway.Settlement.Request.ModelOfferingID)
	for _, task := range []*model.Task{&parent, &child} {
		id := task.PrivateData.Gateway.Settlement.Request.ID
		for _, kind := range []model.LedgerEntryType{model.LedgerEntryPreConsume, model.LedgerEntryPreConsumeRelease} {
			var n int64
			require.NoError(t, fixture.db.Model(&model.LedgerEntry{}).Where("request_id = ? AND type = ?", id, kind).Count(&n).Error)
			assert.EqualValues(t, 1, n)
		}
		require.NoError(t, service.SettleGatewayTask(t.Context(), task))
	}
	assertSubmissionWallet(t, fixture, 1_750_000_000)
}

func TestRouteTaskPluginVideoBoundaryPreservesRawAndOpenAISora(t *testing.T) {
	for _, cached := range []bool{false, true} {
		for _, taskRoute := range []bool{false, true} {
			t.Run(fmt.Sprintf("cache=%t/task=%t", cached, taskRoute), func(t *testing.T) {
				var submits, queries atomic.Int32
				name := "unmatched-video"
				if taskRoute {
					name = "sora-2"
				}
				fileBytes := []byte{0, 255, 1, 2, 128}
				require.NoError(t, i18n.Init())
				fixture := newNativeGatewayTaskFixture(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.Method == http.MethodPost {
						submits.Add(1)
						assert.Equal(t, "/v1/videos", r.URL.Path)
						require.NoError(t, r.ParseMultipartForm(1<<20))
						assert.Equal(t, "sora-2", r.FormValue("model"))
						assert.Equal(t, "raw video prompt", r.FormValue("prompt"))
						file, _, err := r.FormFile("input_reference")
						require.NoError(t, err)
						actual, err := io.ReadAll(file)
						require.NoError(t, err)
						require.NoError(t, file.Close())
						assert.Equal(t, fileBytes, actual)
						_, _ = io.WriteString(w, `{"id":"source_sora","status":"queued"}`)
					} else {
						queries.Add(1)
						assert.Equal(t, "/v1/videos/source_sora", r.URL.Path)
						_, _ = io.WriteString(w, `{"id":"source_sora","status":"completed","seconds":4}`)
					}
				})
				require.NoError(t, fixture.db.Model(fixture.provider).Update("type", constant.ChannelTypeOpenAI).Error)
				require.NoError(t, fixture.db.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Update("model_name", name).Error)
				require.NoError(t, fixture.db.Model(fixture.channel).Updates(map[string]any{"protocol": protocol.OpenAIChat, "upstream_model_name": name}).Error)
				fixture.offering.Aliases = []string{name}
				expression := `tier("fixed", fixed(0.01))`
				if taskRoute {
					caps := capability.Set{InputModalities: []capability.Modality{capability.Text, capability.Image}, OutputModalities: []capability.Modality{capability.Video}}
					require.NoError(t, fixture.db.Model(&model.Model{}).Where("id = ?", fixture.channel.ModelID).Update("capabilities", caps).Error)
					require.NoError(t, fixture.db.Model(fixture.channel).Updates(map[string]any{"protocol": "task:sora", "capabilities": caps}).Error)
					expression = `tier("seconds", u("seconds") * 0.0025)`
				}
				for _, old := range []*model.PriceVersion{fixture.sale, fixture.purchase} {
					require.NoError(t, model.CreatePriceVersion(t.Context(), &model.PriceVersion{OwnerType: old.OwnerType, OwnerID: old.OwnerID, Currency: "USD", Expression: expression, EffectiveAt: clock.Now().Unix()}, fixture.user.Id))
				}
				require.NoError(t, model.SaveModelOffering(t.Context(), fixture.offering, "save"))
				common.MemoryCacheEnabled = cached
				model.InitProviderCache()
				var raw bytes.Buffer
				writer := multipart.NewWriter(&raw)
				require.NoError(t, writer.WriteField("model", name))
				require.NoError(t, writer.WriteField("prompt", "raw video prompt"))
				require.NoError(t, writer.WriteField("seconds", "4"))
				part, err := writer.CreateFormFile("input_reference", "raw.bin")
				require.NoError(t, err)
				_, err = part.Write(fileBytes)
				require.NoError(t, err)
				require.NoError(t, writer.Close())
				body := append([]byte(nil), raw.Bytes()...)
				engine := gin.New()
				engine.Use(middleware.RequestId(), middleware.TaskOriginAuth(), middleware.ResolveTaskOffering(), AuthorizeTaskGateway)
				engine.POST("/v1/videos", middleware.RouteTaskPluginEndpoint(), func(c *gin.Context) {
					if !taskRoute {
						storage, err := common.GetBodyStorage(c)
						require.NoError(t, err)
						actual, err := storage.Bytes()
						require.NoError(t, err)
						assert.Equal(t, body, actual, "unmatched ordinary video remains byte-identical")
						_, pinned := c.Get(pluginruntime.ContextKeyPinnedEndpoint)
						assert.False(t, pinned, "ordinary input must not invoke a plugin decoder")
						assert.Empty(t, c.GetString("resolved_task_model"))
					}
				}, middleware.PrepareTaskPluginEndpoint(), TaskGateway, RelayNotImplemented)
				request := httptest.NewRequest(http.MethodPost, "/v1/videos", bytes.NewReader(body))
				request.Header.Set("Content-Type", writer.FormDataContentType())
				request.Header.Set("Authorization", "Bearer "+fixture.token.Key)
				response := httptest.NewRecorder()
				engine.ServeHTTP(response, request)
				assert.NotContains(t, response.Body.String(), fixture.token.Key)
				if !taskRoute {
					assert.Equal(t, http.StatusNotImplemented, response.Code, response.Body.String())
					assert.Zero(t, submits.Load())
					assert.Zero(t, queries.Load())
					var count int64
					require.NoError(t, fixture.db.Model(&model.GatewayBilling{}).Count(&count).Error)
					assert.Zero(t, count)
					assertSubmissionWallet(t, fixture, 2_000_000_000)
					return
				}
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				assert.EqualValues(t, 1, submits.Load())
				var task model.Task
				require.NoError(t, fixture.db.First(&task).Error)
				require.NotNil(t, task.PrivateData.Gateway)
				assert.Equal(t, "sora", string(task.Platform))
				assert.Equal(t, fixture.provider.Id, task.PrivateData.Gateway.ProviderID)
				assert.True(t, task.PrivateData.Gateway.Accepted)
				assertSubmissionWallet(t, fixture, 1_990_000_000)
				require.NoError(t, service.UpdateGatewayTask(t.Context(), &task))
				assert.EqualValues(t, 1, queries.Load())
				require.NoError(t, fixture.db.First(&task, task.ID).Error)
				assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), task.Status)
				for _, kind := range []model.LedgerEntryType{model.LedgerEntryPreConsume, model.LedgerEntryPreConsumeRelease} {
					var n int64
					require.NoError(t, fixture.db.Model(&model.LedgerEntry{}).Where("request_id = ? AND type = ?", task.PrivateData.Gateway.Settlement.Request.ID, kind).Count(&n).Error)
					assert.EqualValues(t, 1, n)
				}
				require.NoError(t, service.SettleGatewayTask(t.Context(), &task))
				assertSubmissionWallet(t, fixture, 1_990_000_000)
			})
		}
	}
}
