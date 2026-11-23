package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relay/channel/task/taskcommon"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type taskPollingFetchAdaptor struct {
	mu           sync.Mutex
	initInfo     *relaycommon.RelayInfo
	taskIDs      []string
	fetched      chan string
	blockTaskID  string
	blockStarted chan struct{}
	releaseBlock chan struct{}
	blockOnce    sync.Once
}

type batchPollingAdaptor struct {
	taskPollingFetchAdaptor
	batchCalls int
	batchIDs   []string
	results    map[string]*BatchTaskResult
}

func (a *batchPollingAdaptor) FetchMode() string { return "batch" }
func (a *batchPollingAdaptor) FetchBatchTasks(_ string, _ string, tasks []*model.Task, _ string) (*http.Response, error) {
	a.batchCalls++
	a.batchIDs = a.batchIDs[:0]
	for _, task := range tasks {
		a.batchIDs = append(a.batchIDs, task.GetUpstreamTaskID())
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader([]byte(`{}`)))}, nil
}
func (a *batchPollingAdaptor) ParseBatchResult(_ []*model.Task, _ *http.Response, _ []byte) (map[string]*BatchTaskResult, error) {
	if a.results != nil {
		return a.results, nil
	}
	results := make(map[string]*BatchTaskResult, len(a.batchIDs))
	for _, taskID := range a.batchIDs {
		results[taskID] = &BatchTaskResult{TaskInfo: relaycommon.TaskInfo{TaskID: taskID, Status: model.TaskStatusInProgress, Url: "https://example.com/result"}}
	}
	return results, nil
}

func (a *taskPollingFetchAdaptor) Init(info *relaycommon.RelayInfo) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.initInfo = info
}
func (a *taskPollingFetchAdaptor) initChannelMeta() *relaycommon.ProviderMeta {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.initInfo == nil {
		return nil
	}
	return a.initInfo.ProviderMeta
}

func (a *taskPollingFetchAdaptor) FetchTask(_ string, _ string, task *model.Task, _ string) (*http.Response, error) {
	taskID := ""
	if task != nil {
		taskID = task.GetUpstreamTaskID()
	}
	if taskID == a.blockTaskID && a.releaseBlock != nil {
		a.blockOnce.Do(func() {
			if a.blockStarted != nil {
				close(a.blockStarted)
			}
		})
		<-a.releaseBlock
	}

	a.mu.Lock()
	a.taskIDs = append(a.taskIDs, taskID)
	a.mu.Unlock()
	if a.fetched != nil {
		select {
		case a.fetched <- taskID:
		default:
		}
	}

	response := taskdto.TaskResponse[model.Task]{
		Code: taskdto.TaskSuccessCode,
		Data: model.Task{
			TaskID:   taskID,
			Status:   model.TaskStatusInProgress,
			Progress: "30%",
		},
	}
	responseBody, err := common.Marshal(response)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(responseBody)),
	}, nil
}

func (a *taskPollingFetchAdaptor) ParseTaskResult(*model.Task, *http.Response, []byte) (*relaycommon.TaskInfo, error) {
	return &relaycommon.TaskInfo{Status: model.TaskStatusInProgress}, nil
}

func (a *taskPollingFetchAdaptor) AdjustBillingOnComplete(_ *model.Task, _ *relaycommon.TaskInfo) int {
	return 0
}

func (a *taskPollingFetchAdaptor) fetchCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.taskIDs)
}

func (a *taskPollingFetchAdaptor) fetchedTaskIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.taskIDs...)
}

func TestRedactVideoResponseBodyPreservesPollingPayloadShape(t *testing.T) {
	rawVideo := strings.Repeat("a", 300)
	body, err := common.Marshal(map[string]any{
		"done": true,
		"name": "operations/provider-task",
		"response": map[string]any{
			"bytesBase64Encoded": "secret-bytes",
			"video":              rawVideo,
			"videos": []any{
				map[string]any{
					"bytesBase64Encoded": "other-secret-bytes",
					"mimeType":           "video/mp4",
					"uri":                "https://media.example/video.mp4",
				},
			},
		},
	})
	require.NoError(t, err)

	var stored map[string]any
	require.NoError(t, common.Unmarshal(redactVideoResponseBody(body), &stored))
	assert.Equal(t, true, stored["done"])
	assert.Equal(t, "operations/provider-task", stored["name"])
	response, ok := stored["response"].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, response, "bytesBase64Encoded")
	assert.Equal(t, strings.Repeat("a", 256)+"...", response["video"])
	videos, ok := response["videos"].([]any)
	require.True(t, ok)
	require.Len(t, videos, 1)
	video, ok := videos[0].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, video, "bytesBase64Encoded")
	assert.Equal(t, "video/mp4", video["mimeType"])
	assert.Equal(t, "https://media.example/video.mp4", video["uri"])
}

func TestRedactVideoResponseBodyDropsInteractionInlineVideo(t *testing.T) {
	body := []byte(`{"id":"int_1","status":"completed","steps":[{"type":"model_output","content":[{"type":"video","mime_type":"video/mp4","data":"` + strings.Repeat("a", 1024) + `"},{"type":"text","text":"done"}]}]}`)

	var stored map[string]any
	require.NoError(t, common.Unmarshal(redactVideoResponseBody(body), &stored))
	content := stored["steps"].([]any)[0].(map[string]any)["content"].([]any)
	assert.Equal(t, map[string]any{"type": "video", "mime_type": "video/mp4"}, content[0])
	assert.Equal(t, "done", content[1].(map[string]any)["text"])
}

func TestRedactVideoResponseBodyKeepsUntouchedBytes(t *testing.T) {
	body := []byte(`{"name":"op","response":{"videos":[{"uri":"u","bytesBase64Encoded":"x"}],"durationSeconds":8.0,"bytesBase64Encoded":"y"},"done":true}`)

	assert.Equal(t, `{"name":"op","response":{"videos":[{"uri":"u"}],"durationSeconds":8.0},"done":true}`, string(redactVideoResponseBody(body)))
}

func seedTaskPollingChannel(t *testing.T, id int, disableSleep bool) {
	t.Helper()
	ch := &model.Provider{
		Id:     id,
		Type:   constant.ChannelTypeKling,
		Name:   "polling_channel",
		Key:    "sk-test",
		Status: common.ChannelStatusEnabled,
	}
	if disableSleep {
		ch.SetOtherSettings(dto.ProviderOtherSettings{DisableTaskPollingSleep: true})
	}
	require.NoError(t, model.DB.Create(ch).Error)
}

func seedPollingTask(t *testing.T, channelID int, publicID string, upstreamID string) *model.Task {
	t.Helper()
	task := &model.Task{
		TaskID:    publicID,
		Platform:  constant.TaskPlatform("kling"),
		UserId:    1,
		ChannelId: channelID,
		Action:    constant.TaskActionImageToVideo,
		Status:    model.TaskStatusInProgress,
		Progress:  "30%",
		CreatedAt: time.Now().Unix(),
		UpdatedAt: time.Now().Unix(),
		PrivateData: model.TaskPrivateData{
			UpstreamTaskID: upstreamID,
		},
	}
	require.NoError(t, model.DB.Create(task).Error)
	return task
}

func TestUpdateVideoTasksDefaultSleepWaitsBetweenTasks(t *testing.T) {
	truncate(t)

	const channelID = 101
	seedTaskPollingChannel(t, channelID, false)
	first := seedPollingTask(t, channelID, "task_public_1", "upstream_1")
	second := seedPollingTask(t, channelID, "task_public_2", "upstream_2")

	adaptor := &taskPollingFetchAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := UpdateVideoTasks(ctx, constant.TaskPlatform("kling"), map[int][]string{
		channelID: {
			first.GetUpstreamTaskID(),
			second.GetUpstreamTaskID(),
		},
	}, map[string]*model.Task{
		first.GetUpstreamTaskID():  first,
		second.GetUpstreamTaskID(): second,
	})

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, 1, adaptor.fetchCount())
}

func TestPollingPassesExecutingChannelTypeToAdaptor(t *testing.T) {
	truncate(t)
	const channelID = 112
	baseURL := "https://gateway.example"
	gateway := &model.Provider{Id: channelID, Type: constant.ChannelTypeNewAPI, Name: "gateway", Key: "sk-gateway", Status: common.ChannelStatusEnabled, BaseURL: &baseURL}
	gateway.HeaderOverride = common.GetPointer(`{"X-API-Key":"{api_key}"}`)
	gateway.SetOtherSettings(dto.ProviderOtherSettings{DisableTaskPollingSleep: true})
	require.NoError(t, model.DB.Create(gateway).Error)
	task := seedPollingTask(t, channelID, "task_gateway", "upstream_gateway")
	taskChannels := map[int][]string{channelID: {task.GetUpstreamTaskID()}}
	tasks := map[string]*model.Task{task.GetUpstreamTaskID(): task}

	perTask := &taskPollingFetchAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return perTask }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })
	require.NoError(t, UpdateVideoTasks(context.Background(), constant.TaskPlatform("kling"), taskChannels, tasks))
	meta := perTask.initChannelMeta()
	require.NotNil(t, meta, "per-task polling initializes the adaptor with channel metadata")
	assert.Equal(t, constant.ChannelTypeNewAPI, meta.ChannelType, "the adaptor derives the upstream kind from the channel type")
	assert.Equal(t, channelID, meta.ChannelId)
	assert.Equal(t, baseURL, meta.ChannelBaseUrl)
	assert.Equal(t, map[string]any{"X-API-Key": "{api_key}"}, meta.HeadersOverride)

	batch := &batchPollingAdaptor{}
	require.NoError(t, UpdateBatchTasks(context.Background(), batch, taskChannels, tasks))
	meta = batch.initChannelMeta()
	require.NotNil(t, meta, "batch polling initializes the adaptor with channel metadata")
	assert.Equal(t, constant.ChannelTypeNewAPI, meta.ChannelType)
	assert.Equal(t, channelID, meta.ChannelId)
	assert.Equal(t, baseURL, meta.ChannelBaseUrl)
	assert.Equal(t, map[string]any{"X-API-Key": "{api_key}"}, meta.HeadersOverride)
}

func TestDispatchPlatformUpdateUsesFetchMode(t *testing.T) {
	truncate(t)
	const channelID = 109
	seedTaskPollingChannel(t, channelID, true)
	task := seedPollingTask(t, channelID, "task_batch", "upstream_batch")
	taskChannels := map[int][]string{channelID: {task.GetUpstreamTaskID()}}
	tasks := map[string]*model.Task{task.GetUpstreamTaskID(): task}

	batch := &batchPollingAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return batch }
	DispatchPlatformUpdate(context.Background(), "batch-plugin", taskChannels, tasks)
	assert.Equal(t, 1, batch.batchCalls)
	assert.Equal(t, 0, batch.fetchCount())
	var persisted model.Task
	require.NoError(t, model.DB.First(&persisted, task.ID).Error)
	assert.Equal(t, "https://example.com/result", persisted.GetResultURL())

	perTask := &taskPollingFetchAdaptor{}
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return perTask }
	DispatchPlatformUpdate(context.Background(), "per-task-plugin", taskChannels, tasks)
	assert.Equal(t, 1, perTask.fetchCount())

	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return nil }
	assert.NotPanics(t, func() { DispatchPlatformUpdate(context.Background(), "missing-plugin", taskChannels, tasks) })
	GetTaskAdaptorFunc = previousFactory
}

func TestUpdateBatchTasksSettlesTieredUsageForTerminalStates(t *testing.T) {
	for _, tc := range []struct {
		name              string
		status            model.TaskStatus
		units, sale, cost int64
	}{
		{"success with usage", model.TaskStatusSuccess, 3, 3000, 1500},
		{"failure with usage", model.TaskStatusFailure, 3, 0, 1500},
		{"success with zero usage", model.TaskStatusSuccess, 0, 0, 0},
		{"failure with zero usage", model.TaskStatusFailure, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := seedCanonicalWalletTaskWithPricing(t, 5000, `tier("actual", u("units") * 0.000001)`, `u("units") * 0.0000005`, map[string]any{"units": 5})
			task.PrivateData.UpstreamTaskID = "batch-upstream"
			task.SetData(map[string]any{"provider_payload": "must-be-preserved"})
			require.NoError(t, model.DB.Save(task).Error)
			id := task.GetUpstreamTaskID()
			adaptor := &batchPollingAdaptor{results: map[string]*BatchTaskResult{id: {TaskInfo: relaycommon.TaskInfo{TaskID: id, Status: string(tc.status), UsageFacts: map[string]any{"units": tc.units}}}}}
			tasks := map[string]*model.Task{id: task}
			require.NoError(t, UpdateBatchTasks(t.Context(), adaptor, map[int][]string{77: {id}}, tasks))
			var persisted model.Task
			require.NoError(t, model.DB.First(&persisted, task.ID).Error)
			assert.Equal(t, tc.status, persisted.Status)
			var data map[string]any
			require.NoError(t, common.Unmarshal(persisted.Data, &data))
			assert.Equal(t, "must-be-preserved", data["provider_payload"])
			assertCanonicalWalletTaskAccounting(t, &persisted, 5000, tc.sale, tc.cost)
			var before []model.LedgerEntry
			require.NoError(t, model.DB.Order("id").Find(&before).Error)
			require.NoError(t, UpdateBatchTasks(t.Context(), adaptor, map[int][]string{77: {id}}, tasks))
			var after []model.LedgerEntry
			require.NoError(t, model.DB.Order("id").Find(&after).Error)
			assert.Equal(t, before, after)
			assert.Zero(t, countLogs(t), "canonical refund/reconciliation is not an old quota log")
		})
	}

}

func TestUpdateBatchTasksRefundsFailedTieredTask(t *testing.T) {
	task := seedCanonicalWalletTaskWithPricing(t, 5000, `tier("actual", u("units") * 0.000001)`, `u("units") * 0.0000005`, map[string]any{"units": 5})
	id := task.GetUpstreamTaskID()
	adaptor := &batchPollingAdaptor{results: map[string]*BatchTaskResult{id: {TaskInfo: relaycommon.TaskInfo{TaskID: id, Status: model.TaskStatusFailure, Reason: "upstream failed", UsageFacts: map[string]any{"units": float64(5)}}}}}
	require.NoError(t, UpdateBatchTasks(t.Context(), adaptor, map[int][]string{77: {id}}, map[string]*model.Task{id: task}))
	var persisted model.Task
	require.NoError(t, model.DB.First(&persisted, task.ID).Error)
	assert.EqualValues(t, model.TaskStatusFailure, persisted.Status)
	assertCanonicalWalletTaskAccounting(t, &persisted, 5000, 0, 2500)
	record, err := model.GetGatewayRequest(t.Context(), "wallet-task-settlement", 77)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"units": float64(5)}, record.Usage.TaskFacts)
	assert.Zero(t, countLogs(t))
}

func TestUpdateBatchTasksRefundsFailedTaskWithoutUsageSettlement(t *testing.T) {
	task := seedCanonicalWalletTaskWithPricing(t, 4000, `u("tokens") / 1000000000`, `0.000001`, nil)
	task.Properties.OriginModelName = "missing-batch-token-price"
	require.NoError(t, model.DB.Save(task).Error)
	id := task.GetUpstreamTaskID()
	adaptor := &batchPollingAdaptor{results: map[string]*BatchTaskResult{id: {TaskInfo: relaycommon.TaskInfo{TaskID: id, Status: model.TaskStatusFailure, Reason: "upstream failed", TotalTokens: 123, UsageFactsUnknown: true}}}}
	require.NoError(t, UpdateBatchTasks(t.Context(), adaptor, map[int][]string{77: {id}}, map[string]*model.Task{id: task}))
	assertCanonicalWalletTaskAccounting(t, task, 4000, 0, 1000)
	assert.Zero(t, countLogs(t))
}

func TestUpdateVideoTasksCanSkipPollingSleepPerChannel(t *testing.T) {
	truncate(t)

	const channelID = 102
	seedTaskPollingChannel(t, channelID, true)
	first := seedPollingTask(t, channelID, "task_public_3", "upstream_3")
	second := seedPollingTask(t, channelID, "task_public_4", "upstream_4")

	adaptor := &taskPollingFetchAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := UpdateVideoTasks(ctx, constant.TaskPlatform("kling"), map[int][]string{
		channelID: {
			first.GetUpstreamTaskID(),
			second.GetUpstreamTaskID(),
		},
	}, map[string]*model.Task{
		first.GetUpstreamTaskID():  first,
		second.GetUpstreamTaskID(): second,
	})

	require.NoError(t, err)
	assert.Equal(t, 2, adaptor.fetchCount())
}

func TestUpdateVideoTasksDefaultSleepDoesNotBlockOtherChannels(t *testing.T) {
	truncate(t)

	const firstChannelID = 201
	const secondChannelID = 202
	seedTaskPollingChannel(t, firstChannelID, false)
	seedTaskPollingChannel(t, secondChannelID, false)
	firstChannelFirst := seedPollingTask(t, firstChannelID, "task_public_5", "upstream_a_1")
	firstChannelSecond := seedPollingTask(t, firstChannelID, "task_public_6", "upstream_a_2")
	secondChannelFirst := seedPollingTask(t, secondChannelID, "task_public_7", "upstream_b_1")
	secondChannelSecond := seedPollingTask(t, secondChannelID, "task_public_8", "upstream_b_2")

	adaptor := &taskPollingFetchAdaptor{fetched: make(chan string, 4)}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errCh := make(chan error, 1)
	gopool.Go(func() {
		errCh <- UpdateVideoTasks(ctx, constant.TaskPlatform("kling"), map[int][]string{
			firstChannelID: {
				firstChannelFirst.GetUpstreamTaskID(),
				firstChannelSecond.GetUpstreamTaskID(),
			},
			secondChannelID: {
				secondChannelFirst.GetUpstreamTaskID(),
				secondChannelSecond.GetUpstreamTaskID(),
			},
		}, map[string]*model.Task{
			firstChannelFirst.GetUpstreamTaskID():   firstChannelFirst,
			firstChannelSecond.GetUpstreamTaskID():  firstChannelSecond,
			secondChannelFirst.GetUpstreamTaskID():  secondChannelFirst,
			secondChannelSecond.GetUpstreamTaskID(): secondChannelSecond,
		})
	})
	for range 2 {
		select {
		case <-adaptor.fetched:
		case err := <-errCh:
			require.FailNow(t, "polling ended before both channels made their first call", "%v", err)
		case <-t.Context().Done():
			require.FailNow(t, "both channels must make their first call")
		}
	}
	cancel()
	require.ErrorIs(t, <-errCh, context.Canceled)
	assert.ElementsMatch(t, []string{"upstream_a_1", "upstream_b_1"}, adaptor.fetchedTaskIDs())
}

func TestUpdateVideoTasksSlowChannelDoesNotBlockOtherChannels(t *testing.T) {
	truncate(t)

	const slowChannelID = 251
	const fastChannelID = 252
	seedTaskPollingChannel(t, slowChannelID, false)
	seedTaskPollingChannel(t, fastChannelID, true)
	slowTask := seedPollingTask(t, slowChannelID, "task_public_slow", "upstream_slow_1")
	fastFirst := seedPollingTask(t, fastChannelID, "task_public_fast_1", "upstream_fast_parallel_1")
	fastSecond := seedPollingTask(t, fastChannelID, "task_public_fast_2", "upstream_fast_parallel_2")

	// Polling and GORM updates own these mutable tasks after dispatch.
	slowUpstreamID := slowTask.GetUpstreamTaskID()
	fastFirstUpstreamID := fastFirst.GetUpstreamTaskID()
	fastSecondUpstreamID := fastSecond.GetUpstreamTaskID()
	adaptor := &taskPollingFetchAdaptor{
		fetched:      make(chan string, 4),
		blockTaskID:  slowUpstreamID,
		blockStarted: make(chan struct{}),
		releaseBlock: make(chan struct{}),
	}
	var releaseOnce sync.Once
	releaseBlockedTask := func() {
		releaseOnce.Do(func() {
			close(adaptor.releaseBlock)
		})
	}
	t.Cleanup(releaseBlockedTask)
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	errCh := make(chan error, 1)
	gopool.Go(func() {
		errCh <- UpdateVideoTasks(context.Background(), constant.TaskPlatform("kling"), map[int][]string{
			slowChannelID: {slowUpstreamID},
			fastChannelID: {fastFirstUpstreamID, fastSecondUpstreamID},
		}, map[string]*model.Task{
			slowUpstreamID:       slowTask,
			fastFirstUpstreamID:  fastFirst,
			fastSecondUpstreamID: fastSecond,
		})
	})

	select {
	case <-adaptor.blockStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("slow channel did not start blocking")
	}

	require.Eventually(t, func() bool {
		fetchedTaskIDs := adaptor.fetchedTaskIDs()
		return len(fetchedTaskIDs) == 2 &&
			fetchedTaskIDs[0] == fastFirstUpstreamID &&
			fetchedTaskIDs[1] == fastSecondUpstreamID
	}, 500*time.Millisecond, 10*time.Millisecond)

	releaseBlockedTask()
	require.NoError(t, <-errCh)
	assert.ElementsMatch(t, []string{
		slowTask.GetUpstreamTaskID(),
		fastFirst.GetUpstreamTaskID(),
		fastSecond.GetUpstreamTaskID(),
	}, adaptor.fetchedTaskIDs())
}

func TestUpdateVideoTasksMixedChannelSleepSettings(t *testing.T) {
	truncate(t)

	const sleepyChannelID = 301
	const fastChannelID = 302
	seedTaskPollingChannel(t, sleepyChannelID, false)
	seedTaskPollingChannel(t, fastChannelID, true)
	sleepyFirst := seedPollingTask(t, sleepyChannelID, "task_public_9", "upstream_sleepy_1")
	sleepySecond := seedPollingTask(t, sleepyChannelID, "task_public_10", "upstream_sleepy_2")
	fastFirst := seedPollingTask(t, fastChannelID, "task_public_11", "upstream_fast_1")
	fastSecond := seedPollingTask(t, fastChannelID, "task_public_12", "upstream_fast_2")

	adaptor := &taskPollingFetchAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := UpdateVideoTasks(ctx, constant.TaskPlatform("kling"), map[int][]string{
		sleepyChannelID: {
			sleepyFirst.GetUpstreamTaskID(),
			sleepySecond.GetUpstreamTaskID(),
		},
		fastChannelID: {
			fastFirst.GetUpstreamTaskID(),
			fastSecond.GetUpstreamTaskID(),
		},
	}, map[string]*model.Task{
		sleepyFirst.GetUpstreamTaskID():  sleepyFirst,
		sleepySecond.GetUpstreamTaskID(): sleepySecond,
		fastFirst.GetUpstreamTaskID():    fastFirst,
		fastSecond.GetUpstreamTaskID():   fastSecond,
	})

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.ElementsMatch(t, []string{"upstream_sleepy_1", "upstream_fast_1", "upstream_fast_2"}, adaptor.fetchedTaskIDs())
}

func TestUpdateSunoTasksStalePollsRefundExactlyOnce(t *testing.T) {
	task := seedCanonicalWalletTask(t, 2500)
	task.Platform = constant.TaskPlatformSuno
	task.PrivateData.UpstreamTaskID = "suno_upstream_refund_once"
	require.NoError(t, model.DB.Save(task).Error)
	var first, stale model.Task
	require.NoError(t, model.DB.First(&first, task.ID).Error)
	require.NoError(t, model.DB.First(&stale, task.ID).Error)
	id := task.GetUpstreamTaskID()
	adaptor := &batchPollingAdaptor{results: map[string]*BatchTaskResult{id: {TaskInfo: relaycommon.TaskInfo{TaskID: id, Status: model.TaskStatusFailure, Reason: "upstream failed", UsageFacts: map[string]any{"tokens": 0}}}}}
	require.NoError(t, updateBatchTasks(t.Context(), adaptor, 77, []string{id}, map[string]*model.Task{id: &first}))
	assertCanonicalWalletTaskAccounting(t, &first, 2500, 0, 0)
	var before []model.LedgerEntry
	require.NoError(t, model.DB.Order("id").Find(&before).Error)
	require.NoError(t, updateBatchTasks(t.Context(), adaptor, 77, []string{id}, map[string]*model.Task{id: &stale}))
	var after []model.LedgerEntry
	require.NoError(t, model.DB.Order("id").Find(&after).Error)
	assert.Equal(t, before, after)
	var persisted model.Task
	require.NoError(t, model.DB.First(&persisted, task.ID).Error)
	assert.EqualValues(t, model.TaskStatusFailure, persisted.Status)
	assert.Zero(t, countLogs(t))
}

func TestRunTaskPollingOnceDoesNotRefundHistoricalFailedTask(t *testing.T) {
	truncate(t)

	const userID, initialQuota, taskQuota = 402, 10_000, 1_200
	seedUser(t, userID, initialQuota)

	task := makeTask(userID, 0, taskQuota, 0, BillingSourceWallet, 0)
	task.TaskID = "historical_failed_already_refunded"
	task.Status = model.TaskStatusFailure
	task.Progress = "100%"
	task.SubmitTime = time.Now().Add(-90 * 24 * time.Hour).Unix()
	task.UpdatedAt = time.Now().Add(-time.Minute).Unix()
	require.NoError(t, model.DB.Create(task).Error)

	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor {
		return &taskPollingFetchAdaptor{}
	}
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	summary := RunTaskPollingOnce(context.Background(), nil)

	assert.Zero(t, summary.UnfinishedTasks)
	assert.Equal(t, initialQuota, getUserQuota(t, userID))
	assert.Equal(t, taskQuota, getTaskQuota(t, task.ID))
	assert.Equal(t, int64(0), countLogs(t))
}

func TestSweepTimedOutTasksPreservesArchivedSubscriptionObligations(t *testing.T) {
	model.UseTestDatabases(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Task{}, &model.User{}, &model.Token{}, &model.Provider{}, &model.Log{}, &model.LegacyTaskObligation{}))
	require.NoError(t, model.MigrateSubscriptionMoney(model.DB))

	const userID, tokenID, initialQuota, initialTokenQuota, taskQuota = 405, 405, 10_000, 8_000, 1_200
	seedUser(t, userID, initialQuota)
	seedToken(t, tokenID, userID, "archived-subscription-timeout", initialTokenQuota)
	parents := []model.UserSubscription{
		{UserId: userID, Status: "active", MoneyVersion: 1},
		{UserId: userID, Status: "active", MoneyVersion: 1},
	}
	require.NoError(t, model.DB.Create(&parents).Error)

	// Fill the timeout batch with archived evidence on both sides of the old
	// refund rollout. Half of the source subscriptions will no longer exist.
	protectedIDs := make([]int64, 0, 100)
	obligations := make([]model.LegacySubscriptionTaskObligation, 0, 100)
	for index := range 100 {
		task := makeTask(userID, 0, taskQuota, tokenID, BillingSourceSubscription, parents[index%2].Id)
		task.TaskID = fmt.Sprintf("archived_subscription_timeout_%d", index)
		task.Progress = "50%"
		task.SubmitTime = model.TaskRefundLegacyCutoff - 100 + int64(index)
		if index >= 50 {
			task.SubmitTime = model.TaskRefundLegacyCutoff + int64(index)
		}
		if index == 99 {
			task.Quota = 0 // Zero source quota is not proof that the obligation ended.
		}
		require.NoError(t, model.DB.Create(task).Error)
		protectedIDs = append(protectedIDs, task.ID)
		obligations = append(obligations, model.LegacySubscriptionTaskObligation{
			SourceTaskID: task.ID, UserID: userID, SubscriptionID: task.PrivateData.SubscriptionId,
			ProviderID: task.ChannelId, TaskID: task.TaskID, Platform: string(task.Platform),
			SourceQuota: int64(task.Quota), SourceStatus: string(task.Status),
			SourceCreatedAt: task.CreatedAt, SourceUpdatedAt: task.UpdatedAt,
			State: "unverified", CapturedAt: time.Now().Unix(),
		})
	}
	require.NoError(t, model.DB.Create(&obligations).Error)
	require.NoError(t, model.DB.Delete(&parents[1]).Error)
	var parentCount int64
	require.NoError(t, model.DB.Model(&model.UserSubscription{}).Where("id IN ?", []int{parents[0].Id, parents[1].Id}).Count(&parentCount).Error)
	require.EqualValues(t, 1, parentCount)
	ordinaryEvidence := make([]model.LegacyTaskObligation, 0, 100)
	for index := range 100 {
		task := makeTask(userID, 0, taskQuota, tokenID, BillingSourceWallet, 0)
		task.TaskID = fmt.Sprintf("archived_wallet_timeout_%d", index)
		task.Progress = "50%"
		task.SubmitTime = model.TaskRefundLegacyCutoff - 300 + int64(index)
		if index == 99 {
			task.Quota = 0
		}
		require.NoError(t, model.DB.Create(task).Error)
		protectedIDs = append(protectedIDs, task.ID)
		ordinaryEvidence = append(ordinaryEvidence, model.LegacyTaskObligation{SourceTaskID: task.ID, UserID: userID, TaskID: task.TaskID,
			Platform: string(task.Platform), BillingSource: BillingSourceWallet, TokenID: tokenID, SourceQuota: int64(task.Quota), SourceStatus: string(task.Status), State: "unverified"})
	}
	require.NoError(t, model.DB.Create(&ordinaryEvidence).Error)

	gatewayTask := makeTask(userID, 0, 0, 0, BillingSourceWallet, 0)
	gatewayTask.TaskID = "accepted_gateway_timeout_unchanged"
	gatewayTask.Progress = "50%"
	gatewayTask.SubmitTime = model.TaskRefundLegacyCutoff - 200
	gatewayTask.PrivateData.Gateway = &model.TaskGatewaySnapshot{Accepted: true}
	require.NoError(t, model.DB.Create(gatewayTask).Error)
	protectedIDs = append(protectedIDs, gatewayTask.ID)

	ordinaryTask := makeTask(userID, 0, taskQuota, 0, BillingSourceWallet, 0)
	ordinaryTask.TaskID = "ordinary_timeout_after_archived_obligations"
	ordinaryTask.Progress = "50%"
	ordinaryTask.SubmitTime = time.Now().Add(-2 * time.Hour).Unix()
	require.NoError(t, model.DB.Create(ordinaryTask).Error)

	var before []model.Task
	require.NoError(t, model.DB.Where("id IN ?", protectedIDs).Order("id").Find(&before).Error)
	previousTimeout := constant.TaskTimeoutMinutes
	constant.TaskTimeoutMinutes = 1
	t.Cleanup(func() { constant.TaskTimeoutMinutes = previousTimeout })

	candidates := model.GetTimedOutUnfinishedTasks(time.Now().Add(-time.Minute).Unix(), 1)
	candidateIDs := make([]int64, len(candidates))
	for index, task := range candidates {
		candidateIDs[index] = task.ID
	}
	assert.Equal(t, []int64{ordinaryTask.ID}, candidateIDs, "preserved obligations must be excluded before LIMIT")

	for range 2 {
		sweepTimedOutTasks(t.Context())
	}

	var after []model.Task
	require.NoError(t, model.DB.Where("id IN ?", protectedIDs).Order("id").Find(&after).Error)
	require.Len(t, after, len(before))
	var changed []int64
	for index := range before {
		if !reflect.DeepEqual(before[index], after[index]) {
			changed = append(changed, before[index].ID)
		}
	}
	assert.Empty(t, changed, "timeout must preserve status, source quota, timestamps and identity of archived/Gateway tasks")
	var remainingObligations []model.LegacySubscriptionTaskObligation
	require.NoError(t, model.DB.Order("source_task_id").Find(&remainingObligations).Error)
	assert.Equal(t, obligations, remainingObligations)
	var remainingOrdinary []model.LegacyTaskObligation
	require.NoError(t, model.DB.Order("source_task_id").Find(&remainingOrdinary).Error)
	assert.Equal(t, ordinaryEvidence, remainingOrdinary)
	var ordinary model.Task
	require.NoError(t, model.DB.First(&ordinary, ordinaryTask.ID).Error)
	assert.EqualValues(t, model.TaskStatusFailure, ordinary.Status)
	assert.Equal(t, taskQuota, ordinary.Quota)
	assert.Equal(t, initialQuota, getUserQuota(t, userID))
	assert.Equal(t, initialTokenQuota, getTokenRemainQuota(t, tokenID))
	assert.Zero(t, getTokenUsedQuota(t, tokenID))
	assert.Zero(t, countLogs(t), "clock-based status changes cannot prove a historical refund")
	for _, table := range []string{"subscription_periods", "wallets", "ledger_entries"} {
		var count int64
		require.NoError(t, model.DB.Table(table).Count(&count).Error)
		assert.Zero(t, count, "timeout must not invent subscription or money facts in %s", table)
	}
	pollable := model.GetAllUnFinishSyncTasks(len(protectedIDs))
	pollableIDs := make([]int64, len(pollable))
	for index, task := range pollable {
		pollableIDs[index] = task.ID
	}
	assert.ElementsMatch(t, protectedIDs, pollableIDs, "actual Provider polling, including accepted Gateway tasks, is unchanged")
}

func TestSweepTimedOutTasksHonorsRefundRolloutBoundary(t *testing.T) {
	truncate(t)

	const (
		userID          = 403
		initialQuota    = 10_000
		legacyTaskQuota = 1_800
		modernTaskQuota = 1_200
	)
	seedUser(t, userID, initialQuota)

	// The timeout batch must select historical work before LIMIT, so a full
	// batch of overdue accepted money tasks cannot starve ordinary status cleanup.
	gatewayTasks := make([]*model.Task, 100)
	for index := range gatewayTasks {
		gatewayTask := makeTask(userID, 0, 0, 0, BillingSourceWallet, 0)
		gatewayTask.TaskID = fmt.Sprintf("accepted_money_timeout_%d", index)
		gatewayTask.Progress = "50%"
		gatewayTask.SubmitTime = 1771718300
		gatewayTask.PrivateData.Gateway = &model.TaskGatewaySnapshot{Accepted: true}
		gatewayTasks[index] = gatewayTask
	}
	require.NoError(t, model.DB.Create(&gatewayTasks).Error)

	legacyTask := makeTask(userID, 0, legacyTaskQuota, 0, BillingSourceWallet, 0)
	legacyTask.TaskID = "legacy_timeout_without_refund"
	legacyTask.Progress = "50%"
	legacyTask.SubmitTime = 1771718399 // 2026-02-21 23:59:59 UTC
	require.NoError(t, model.DB.Create(legacyTask).Error)

	modernTask := makeTask(userID, 0, modernTaskQuota, 0, BillingSourceWallet, 0)
	modernTask.TaskID = "modern_timeout_with_refund"
	modernTask.Progress = "50%"
	modernTask.SubmitTime = 1771718400 // 2026-02-22 00:00:00 UTC
	require.NoError(t, model.DB.Create(modernTask).Error)

	previousTimeout := constant.TaskTimeoutMinutes
	constant.TaskTimeoutMinutes = 1
	t.Cleanup(func() { constant.TaskTimeoutMinutes = previousTimeout })

	sweepTimedOutTasks(context.Background())

	var reloadedLegacy model.Task
	var reloadedModern model.Task
	require.NoError(t, model.DB.First(&reloadedLegacy, legacyTask.ID).Error)
	require.NoError(t, model.DB.First(&reloadedModern, modernTask.ID).Error)
	assert.EqualValues(t, model.TaskStatusFailure, reloadedLegacy.Status)
	assert.EqualValues(t, model.TaskStatusFailure, reloadedModern.Status)
	assert.Equal(t, legacyTaskQuota, reloadedLegacy.Quota)
	assert.Equal(t, modernTaskQuota, reloadedModern.Quota)
	assert.Contains(t, reloadedLegacy.FailReason, "old system")
	assert.Equal(t, "Task timed out", reloadedModern.FailReason)
	var retained int64
	require.NoError(t, model.DB.Model(&model.Task{}).
		Where("user_id = ? AND status = ?", userID, model.TaskStatusInProgress).
		Where(`"private_data"->>'gateway' IS NOT NULL`).Count(&retained).Error)
	assert.EqualValues(t, len(gatewayTasks), retained, "accepted money tasks remain pending beyond the host deadline")
	assert.Equal(t, initialQuota, getUserQuota(t, userID))
	assert.Zero(t, countLogs(t))
}

// TASK_TIMEOUT_MINUTES defaults to 72 hours, the longest a vendor keeps a
// task running (Ark can be configured to 72h, Vidu off-peak runs 48h).
func TestSweepTimedOutTasksAtTheDefaultDeadline(t *testing.T) {
	truncate(t)

	const userID, initialQuota, taskQuota = 404, 10_000, 1_500
	seedUser(t, userID, initialQuota)
	running := makeTask(userID, 0, taskQuota, 0, BillingSourceWallet, 0)
	running.TaskID = "running_for_60_hours"
	running.Progress = "50%"
	running.SubmitTime = time.Now().Add(-60 * time.Hour).Unix()
	require.NoError(t, model.DB.Create(running).Error)
	stuck := makeTask(userID, 0, taskQuota, 0, BillingSourceWallet, 0)
	stuck.TaskID = "running_for_73_hours"
	stuck.Progress = "50%"
	stuck.SubmitTime = time.Now().Add(-73 * time.Hour).Unix()
	require.NoError(t, model.DB.Create(stuck).Error)

	previousTimeout := constant.TaskTimeoutMinutes
	constant.TaskTimeoutMinutes = 4320
	t.Cleanup(func() { constant.TaskTimeoutMinutes = previousTimeout })

	sweepTimedOutTasks(context.Background())

	var reloadedRunning, reloadedStuck model.Task
	require.NoError(t, model.DB.First(&reloadedRunning, running.ID).Error)
	require.NoError(t, model.DB.First(&reloadedStuck, stuck.ID).Error)
	assert.NotEqualValues(t, model.TaskStatusFailure, reloadedRunning.Status)
	assert.Equal(t, taskQuota, reloadedRunning.Quota)
	assert.EqualValues(t, model.TaskStatusFailure, reloadedStuck.Status)
	assert.Equal(t, "Task timed out", reloadedStuck.FailReason)
	assert.Equal(t, initialQuota, getUserQuota(t, userID))
}

type scriptedPollingAdaptor struct {
	statusCode int
	body       []byte
	fetchErr   error
	parse      *relaycommon.TaskInfo
	parseErr   error
}

func (a *scriptedPollingAdaptor) Init(*relaycommon.RelayInfo) {}
func (a *scriptedPollingAdaptor) FetchTask(string, string, *model.Task, string) (*http.Response, error) {
	if a.fetchErr != nil {
		return nil, a.fetchErr
	}
	code := a.statusCode
	if code == 0 {
		code = http.StatusOK
	}
	body := a.body
	if body == nil {
		body = []byte(`{}`)
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(bytes.NewReader(body))}, nil
}
func (a *scriptedPollingAdaptor) ParseTaskResult(*model.Task, *http.Response, []byte) (*relaycommon.TaskInfo, error) {
	if a.parseErr != nil {
		return nil, a.parseErr
	}
	if a.parse != nil {
		return a.parse, nil
	}
	return &relaycommon.TaskInfo{Status: model.TaskStatusInProgress}, nil
}
func (a *scriptedPollingAdaptor) AdjustBillingOnComplete(*model.Task, *relaycommon.TaskInfo) int {
	return 0
}

type scriptedBatchPollingAdaptor struct {
	scriptedPollingAdaptor
	results map[string]*BatchTaskResult
}

func (a *scriptedBatchPollingAdaptor) FetchMode() string { return "batch" }
func (a *scriptedBatchPollingAdaptor) FetchBatchTasks(string, string, []*model.Task, string) (*http.Response, error) {
	return a.FetchTask("", "", nil, "")
}
func (a *scriptedBatchPollingAdaptor) ParseBatchResult([]*model.Task, *http.Response, []byte) (map[string]*BatchTaskResult, error) {
	if a.parseErr != nil {
		return nil, a.parseErr
	}
	return a.results, nil
}

type legacyObligationPollingAdaptor struct {
	scriptedPollingAdaptor
	pluginKey, pluginVersion string
	fetchMode                string
	providerID               int
	observed                 func(int, string, *model.Task)
}

func (a *legacyObligationPollingAdaptor) TaskPluginIdentity() (string, string) {
	return a.pluginKey, a.pluginVersion
}
func (a *legacyObligationPollingAdaptor) Init(info *relaycommon.RelayInfo) {
	a.providerID = info.ChannelId
}
func (a *legacyObligationPollingAdaptor) FetchTask(baseURL, key string, task *model.Task, proxy string) (*http.Response, error) {
	if a.observed != nil {
		a.observed(a.providerID, key, task)
	}
	return a.scriptedPollingAdaptor.FetchTask(baseURL, key, task, proxy)
}

func (a *legacyObligationPollingAdaptor) FetchMode() string { return a.fetchMode }
func (a *legacyObligationPollingAdaptor) FetchBatchTasks(baseURL, key string, tasks []*model.Task, proxy string) (*http.Response, error) {
	if len(tasks) != 1 {
		return nil, fmt.Errorf("legacy batch must retain one original identity")
	}
	return a.FetchTask(baseURL, key, tasks[0], proxy)
}
func (a *legacyObligationPollingAdaptor) ParseBatchResult(tasks []*model.Task, _ *http.Response, _ []byte) (map[string]*BatchTaskResult, error) {
	if a.parseErr != nil {
		return nil, a.parseErr
	}
	if len(tasks) != 1 {
		return nil, fmt.Errorf("missing original task result")
	}
	result := a.parse
	if result == nil {
		result = &relaycommon.TaskInfo{Status: model.TaskStatusInProgress}
	}
	return map[string]*BatchTaskResult{tasks[0].GetUpstreamTaskID(): {TaskInfo: *result}}, nil
}

func TestLegacyTaskPollingKeepsProviderAndOwnerIdentity(t *testing.T) {
	for _, mode := range []string{"per_task", "batch"} {
		t.Run(mode, func(t *testing.T) {
			seedSubscriptionTaskMoney(t, 0)
			require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", 77).Updates(map[string]any{"type": constant.ChannelTypeKling, "key": "provider-77-original"}).Error)
			require.NoError(t, model.DB.Create(&model.User{Id: 78, Username: "other-task-owner", AffCode: "other-task-owner", Status: common.UserStatusEnabled}).Error)
			require.NoError(t, model.DB.Create(&model.Provider{Id: 78, Type: constant.ChannelTypeKling, Key: "provider-78-original", Status: common.ChannelStatusEnabled}).Error)
			var tasks []*model.Task
			for index, ids := range [][2]int{{77, 77}, {78, 78}, {78, 77}} {
				task := makeTask(ids[0], ids[1], 5000, 0, BillingSourceWallet, 0)
				task.TaskID = fmt.Sprintf("public-task-%d", index)
				task.Platform = "kling"
				task.Status = model.TaskStatusQueued
				task.PrivateData.UpstreamTaskID = "same-private-task-id"
				task.PrivateData.Key = fmt.Sprintf("provider-%d-original", ids[1])
				task.PrivateData.Execution = &model.TaskExecutionSnapshot{RequestID: fmt.Sprintf("original-request-%d", index), TaskPlugin: &model.TaskPluginSnapshot{Key: "kling", Version: "1.0.0", Generation: 7}}
				require.NoError(t, model.DB.Create(task).Error)
				archiveLegacyPollingTask(t, task)
				tasks = append(tasks, task)
			}
			registry := jsplugin.NewRegistry()
			_, err := registry.Register(channelSelectTaskPluginSource("kling", constant.ChannelTypeKling), jsplugin.Options{})
			require.NoError(t, err)
			oldRegistry, oldFactory, oldTimeout, oldLimit := jsplugin.DefaultRegistry, GetTaskAdaptorFunc, constant.TaskTimeoutMinutes, constant.TaskQueryLimit
			jsplugin.DefaultRegistry = registry
			constant.TaskTimeoutMinutes = 0
			constant.TaskQueryLimit = 100
			t.Cleanup(func() {
				jsplugin.DefaultRegistry = oldRegistry
				GetTaskAdaptorFunc = oldFactory
				constant.TaskTimeoutMinutes = oldTimeout
				constant.TaskQueryLimit = oldLimit
			})
			var mu sync.Mutex
			seen := make(map[int64]int)
			GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor {
				a := &legacyObligationPollingAdaptor{pluginKey: "kling", pluginVersion: "1.0.0", fetchMode: mode}
				a.observed = func(providerID int, key string, task *model.Task) {
					mu.Lock()
					seen[task.ID]++
					mu.Unlock()
					assert.Equal(t, task.ChannelId, providerID)
					assert.Equal(t, task.PrivateData.Key, key)
					status := model.TaskStatusSuccess
					if task.ID == tasks[1].ID {
						status = model.TaskStatusFailure
					}
					a.parse = &relaycommon.TaskInfo{TaskID: task.GetUpstreamTaskID(), Status: status}
				}
				return a
			}
			var before []model.LedgerEntry
			require.NoError(t, model.DB.Order("id").Find(&before).Error)
			RunTaskPollingOnce(t.Context(), nil)
			for index, source := range tasks {
				var got model.Task
				require.NoError(t, model.DB.First(&got, source.ID).Error)
				assert.Equal(t, 1, seen[source.ID])
				assert.Equal(t, source.UserId, got.UserId)
				assert.Equal(t, source.ChannelId, got.ChannelId)
				expectedPrivate := source.PrivateData
				if mode == "per_task" && index != 1 {
					expectedPrivate.ResultURL = taskcommon.BuildProxyURL(source.TaskID)
				}
				assert.Equal(t, expectedPrivate, got.PrivateData)
				assert.Equal(t, 5000, got.Quota)
				status := model.TaskStatus(model.TaskStatusSuccess)
				if index == 1 {
					status = model.TaskStatusFailure
				}
				assert.Equal(t, status, got.Status)
			}
			var after []model.LedgerEntry
			require.NoError(t, model.DB.Order("id").Find(&after).Error)
			assert.Equal(t, before, after)
			assert.Zero(t, countLogs(t))
		})
	}
}

func archiveLegacyPollingTask(t *testing.T, task *model.Task) {
	t.Helper()
	require.NoError(t, model.DB.AutoMigrate(&model.LegacyTaskObligation{}, &model.LegacySubscriptionTaskObligation{}))
	requestID := ""
	plugin := model.TaskPluginSnapshot{}
	if task.PrivateData.Execution != nil {
		requestID = task.PrivateData.Execution.RequestID
		if task.PrivateData.Execution.TaskPlugin != nil {
			plugin = *task.PrivateData.Execution.TaskPlugin
		}
	}
	if task.PrivateData.BillingSource == BillingSourceSubscription {
		require.NoError(t, model.DB.Create(&model.LegacySubscriptionTaskObligation{
			SourceTaskID: task.ID, UserID: task.UserId, SubscriptionID: task.PrivateData.SubscriptionId,
			ProviderID: task.ChannelId, TaskID: task.TaskID, Platform: string(task.Platform), RequestID: requestID,
			SourceQuota: int64(task.Quota), SourceStatus: string(task.Status), State: "unverified",
		}).Error)
		return
	}
	require.NoError(t, model.DB.Create(&model.LegacyTaskObligation{
		SourceTaskID: task.ID, UserID: task.UserId, ProviderID: task.ChannelId, TaskID: task.TaskID,
		UpstreamTaskID: task.PrivateData.UpstreamTaskID, Platform: string(task.Platform), BillingSource: task.PrivateData.BillingSource,
		TokenID: task.PrivateData.TokenId, SubscriptionID: task.PrivateData.SubscriptionId, RequestID: requestID,
		PluginKey: plugin.Key, PluginVersion: plugin.Version, PluginGeneration: plugin.Generation,
		SourceQuota: int64(task.Quota), SourceStatus: string(task.Status), State: "unverified",
	}).Error)
}

func TestLegacyTaskPollingPreservesUnverifiedObligations(t *testing.T) {
	for _, source := range []string{BillingSourceWallet, BillingSourceSubscription} {
		for _, scenarioName := range []string{"success", "failure", "not_found", "gone", "failure_threshold", "transport_error", "hook_error", "unrecognized", "nonterminal_reason", "gateway_envelope", "batch_success", "batch_failure", "batch_not_found", "batch_gone", "batch_transport_error", "batch_hook_error", "batch_unrecognized", "batch_nonterminal_reason", "missing_provider", "missing_upstream", "missing_key", "provider_key_changed", "plugin_missing", "plugin_mismatch", "adaptor_without_identity", "owner_rebound", "provider_rebound", "evidence_read_error", "in_flight_owner_rebind"} {
			scenario, mode := scenarioName, "per_task"
			if batchScenario, ok := strings.CutPrefix(scenarioName, "batch_"); ok {
				scenario, mode = batchScenario, "batch"
			}
			t.Run(source+"/"+scenarioName, func(t *testing.T) {
				seedSubscriptionTaskMoney(t, 0)
				require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", 77).Updates(map[string]any{"type": constant.ChannelTypeKling, "key": "original-task-key"}).Error)
				task := makeTask(77, 77, 5000, 77, source, 0)
				task.Platform = "kling"
				task.Status = model.TaskStatusQueued
				task.Progress = "0%"
				task.Properties.OriginModelName = "private-model-canary"
				task.PrivateData.UpstreamTaskID = "original-upstream"
				task.PrivateData.Key = "original-task-key"
				task.PrivateData.PluginState = []byte(`{"secret":"private-state-canary"}`)
				task.PrivateData.Execution = &model.TaskExecutionSnapshot{RequestID: "original-request", TaskPlugin: &model.TaskPluginSnapshot{Key: "kling", Version: "1.0.0", Generation: 7}}
				if scenario == "missing_key" {
					task.PrivateData.Key = ""
				}
				if scenario == "missing_provider" {
					task.ChannelId = 999
				}
				if scenario == "missing_upstream" {
					task.TaskID = ""
					task.PrivateData.UpstreamTaskID = ""
				}
				task.PrivateData.PollFailures = 100
				require.NoError(t, model.DB.Create(task).Error)
				archiveLegacyPollingTask(t, task)
				var ordinaryBefore []model.LegacyTaskObligation
				var subscriptionBefore []model.LegacySubscriptionTaskObligation
				require.NoError(t, model.DB.Find(&ordinaryBefore).Error)
				require.NoError(t, model.DB.Find(&subscriptionBefore).Error)
				switch scenario {
				case "provider_key_changed":
					require.NoError(t, model.DB.Model(&model.Provider{}).Where("id = ?", 77).Update("key", "replacement-not-original").Error)
				case "owner_rebound":
					require.NoError(t, model.DB.Model(task).Update("user_id", 78).Error)
				case "provider_rebound":
					require.NoError(t, model.DB.Model(task).Update("channel_id", 78).Error)
				case "evidence_read_error":
					require.NoError(t, model.DB.Exec(`ALTER TABLE "legacy_task_obligations" RENAME TO "temporarily_unavailable_evidence"`).Error)
				}
				registry := jsplugin.NewRegistry()
				_, err := registry.Register(channelSelectTaskPluginSource("kling", constant.ChannelTypeKling), jsplugin.Options{})
				require.NoError(t, err)
				oldRegistry, oldFactory, oldTimeout, oldLimit := jsplugin.DefaultRegistry, GetTaskAdaptorFunc, constant.TaskTimeoutMinutes, constant.TaskQueryLimit
				jsplugin.DefaultRegistry = registry
				if scenario == "plugin_missing" {
					jsplugin.DefaultRegistry = jsplugin.NewRegistry()
				}
				constant.TaskTimeoutMinutes = 0
				constant.TaskQueryLimit = 100
				t.Cleanup(func() {
					jsplugin.DefaultRegistry = oldRegistry
					GetTaskAdaptorFunc = oldFactory
					constant.TaskTimeoutMinutes = oldTimeout
					constant.TaskQueryLimit = oldLimit
				})
				calls := 0
				GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor {
					if scenario == "adaptor_without_identity" {
						return &scriptedPollingAdaptor{}
					}
					a := &legacyObligationPollingAdaptor{pluginKey: "kling", pluginVersion: "1.0.0", fetchMode: mode}
					a.body = []byte(`{"detail":"provider-response-canary","credential":"original-task-key"}`)
					a.observed = func(providerID int, key string, got *model.Task) {
						calls++
						assert.Equal(t, 77, providerID)
						assert.Equal(t, "original-task-key", key)
						assert.Equal(t, task.ID, got.ID)
						if scenario == "in_flight_owner_rebind" {
							require.NoError(t, model.DB.Model(&model.Task{}).Where("id = ?", got.ID).Update("user_id", 78).Error)
						}
					}
					if scenario == "plugin_mismatch" {
						a.pluginVersion = "2.0.0"
					}
					switch scenario {
					case "success":
						a.parse = &relaycommon.TaskInfo{Status: model.TaskStatusSuccess, Progress: "100%"}
					case "failure":
						a.parse = &relaycommon.TaskInfo{Status: model.TaskStatusFailure, Reason: "provider-reason-canary original-task-key"}
					case "transport_error":
						a.fetchErr = fmt.Errorf("transport-error-canary https://original-task-key@provider.invalid?credential=original-task-key")
					case "hook_error":
						a.parseErr = fmt.Errorf("hook-error-canary credential=original-task-key")
					case "unrecognized":
						a.parse = &relaycommon.TaskInfo{Status: model.TaskStatusUnknown, Reason: "provider-reason-canary original-task-key"}
					case "nonterminal_reason":
						a.parse = &relaycommon.TaskInfo{Status: model.TaskStatusInProgress, Reason: "provider-reason-canary original-task-key"}
					case "gateway_envelope":
						a.body = []byte(`{"code":"success","data":{"task_id":"original-upstream","status":"SUCCESS","progress":"100%","fail_reason":"provider-reason-canary original-task-key","data":{"detail":"provider-response-canary","credential":"original-task-key"}}}`)
					case "not_found":
						a.statusCode = http.StatusNotFound
					case "gone":
						a.statusCode = http.StatusGone
					case "failure_threshold":
						a.statusCode = http.StatusServiceUnavailable
					}
					if a.parse != nil {
						a.parse.Url = "https://provider.invalid/provider-result-canary?credential=original-task-key"
						a.parse.PluginState = []byte(`{"opaque":"provider-plugin-state-canary","credential":"original-task-key"}`)
					}
					return a
				}
				var captured bytes.Buffer
				oldDebug := common.DebugEnabled
				common.DebugEnabled = true
				common.LogWriterMu.Lock()
				oldWriter, oldErrorWriter := gin.DefaultWriter, gin.DefaultErrorWriter
				gin.DefaultWriter, gin.DefaultErrorWriter = &captured, &captured
				common.LogWriterMu.Unlock()
				RunTaskPollingOnce(t.Context(), nil)
				common.LogWriterMu.Lock()
				gin.DefaultWriter, gin.DefaultErrorWriter = oldWriter, oldErrorWriter
				common.LogWriterMu.Unlock()
				common.DebugEnabled = oldDebug
				assert.NotContains(t, captured.String(), "private-model-canary")
				assert.NotContains(t, captured.String(), "original-task-key")
				assert.NotContains(t, captured.String(), "private-state-canary")
				for _, secret := range []string{"provider-response-canary", "provider-reason-canary", "provider-result-canary", "provider-plugin-state-canary", "transport-error-canary", "hook-error-canary"} {
					assert.NotContains(t, captured.String(), secret)
				}
				assert.NotEmpty(t, captured.String(), "safe diagnostics must remain enabled")
				if scenario == "evidence_read_error" {
					require.NoError(t, model.DB.Exec(`ALTER TABLE "temporarily_unavailable_evidence" RENAME TO "legacy_task_obligations"`).Error)
				}
				var after model.Task
				require.NoError(t, model.DB.First(&after, task.ID).Error)
				wantStatus := model.TaskStatus(model.TaskStatusQueued)
				if scenario == "success" || scenario == "gateway_envelope" {
					wantStatus = model.TaskStatusSuccess
				}
				if scenario == "nonterminal_reason" {
					wantStatus = model.TaskStatusInProgress
				}
				if scenario == "failure" {
					wantStatus = model.TaskStatusFailure
				}
				assert.Equal(t, wantStatus, after.Status)
				assert.Equal(t, 5000, after.Quota)
				assert.Equal(t, task.PrivateData.Execution, after.PrivateData.Execution)
				assert.Equal(t, task.PrivateData.Key, after.PrivateData.Key)
				switch scenario {
				case "success", "failure", "nonterminal_reason":
					assert.Equal(t, 1, calls)
					assert.JSONEq(t, `{"opaque":"provider-plugin-state-canary","credential":"original-task-key"}`, string(after.PrivateData.PluginState))
					if scenario == "failure" || mode == "batch" && scenario == "nonterminal_reason" {
						assert.Equal(t, "provider-reason-canary original-task-key", after.FailReason)
					}
					if scenario == "success" {
						assert.Equal(t, "https://provider.invalid/provider-result-canary?credential=original-task-key", after.PrivateData.ResultURL)
					}
				case "not_found", "gone", "failure_threshold", "transport_error", "hook_error", "unrecognized", "gateway_envelope", "in_flight_owner_rebind":
					assert.Equal(t, 1, calls)
				default:
					assert.Zero(t, calls, "unproven identity or credential must not reach the Provider")
				}
				var ordinaryAfter []model.LegacyTaskObligation
				var subscriptionAfter []model.LegacySubscriptionTaskObligation
				require.NoError(t, model.DB.Find(&ordinaryAfter).Error)
				require.NoError(t, model.DB.Find(&subscriptionAfter).Error)
				assert.Equal(t, ordinaryBefore, ordinaryAfter)
				assert.Equal(t, subscriptionBefore, subscriptionAfter)
				assert.Equal(t, 0, getUserQuota(t, 77))
				assert.Equal(t, 8000, getTokenRemainQuota(t, 77))
				assert.Zero(t, countLogs(t))
			})
		}
	}
}

func TestUpdateVideoSingleTaskPollClassification(t *testing.T) {
	testCases := []struct {
		name          string
		statusCode    int
		fetchErr      error
		parse         *relaycommon.TaskInfo
		parseErr      error
		priorFailures int
		priorState    string
		maxFailures   int
		wantStatus    model.TaskStatus
		wantFailures  int
		wantReason    string
		wantState     string
		wantUnchanged bool
	}{
		{
			name:          "404 fails immediately and refunds",
			statusCode:    http.StatusNotFound,
			wantStatus:    model.TaskStatusFailure,
			wantReason:    "Upstream task not found",
			wantUnchanged: false,
		},
		{
			name:          "401 increments without changing status",
			statusCode:    http.StatusUnauthorized,
			wantStatus:    model.TaskStatusInProgress,
			wantFailures:  1,
			wantUnchanged: true,
		},
		{
			name:          "429 reaches threshold without refund",
			statusCode:    http.StatusTooManyRequests,
			priorFailures: 2,
			maxFailures:   3,
			wantStatus:    model.TaskStatusFailure,
			wantFailures:  3,
			wantReason:    "Failed to query the upstream task status",
		},
		{
			name:          "UNKNOWN increments",
			statusCode:    http.StatusOK,
			parse:         &relaycommon.TaskInfo{Status: model.TaskStatusUnknown, Reason: "weird"},
			wantStatus:    model.TaskStatusInProgress,
			wantFailures:  1,
			wantUnchanged: true,
		},
		{
			name:          "valid 2xx resets the failure counter",
			statusCode:    http.StatusOK,
			parse:         &relaycommon.TaskInfo{Status: model.TaskStatusInProgress},
			priorFailures: 5,
			wantStatus:    model.TaskStatusInProgress,
			wantFailures:  0,
		},
		{
			name:       "omit state preserves previous plugin state",
			statusCode: http.StatusOK,
			parse:      &relaycommon.TaskInfo{Status: model.TaskStatusInProgress},
			priorState: `{"req_key":"keep"}`,
			wantStatus: model.TaskStatusInProgress,
			wantState:  `{"req_key":"keep"}`,
		},
		{
			name:       "returned state replaces plugin state",
			statusCode: http.StatusOK,
			parse: &relaycommon.TaskInfo{
				Status:      model.TaskStatusInProgress,
				PluginState: []byte(`{"req_key":"new"}`),
			},
			priorState: `{"req_key":"old"}`,
			wantStatus: model.TaskStatusInProgress,
			wantState:  `{"req_key":"new"}`,
		},
		{
			name:          "other 4xx non-terminal is unrecognized",
			statusCode:    http.StatusBadRequest,
			parse:         &relaycommon.TaskInfo{Status: model.TaskStatusInProgress},
			wantStatus:    model.TaskStatusInProgress,
			wantFailures:  1,
			wantUnchanged: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			truncate(t)
			const userID, tokenID, channelID = 510, 510, 510
			const initialQuota, preConsumed, tokenRemain = 10_000, 4_000, 7_000
			seedUser(t, userID, initialQuota)
			seedToken(t, tokenID, userID, "sk-poll-class", tokenRemain)
			ch := &model.Provider{Id: channelID, Type: constant.ChannelTypeKling, Name: "poll", Key: "sk-test", Status: common.ChannelStatusEnabled}

			if testCase.maxFailures > 0 {
				previous := constant.TaskPollMaxFailures
				constant.TaskPollMaxFailures = testCase.maxFailures
				t.Cleanup(func() { constant.TaskPollMaxFailures = previous })
			}

			task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
			task.TaskID = "task_poll_class"
			task.PrivateData.UpstreamTaskID = "upstream_poll_class"
			task.PrivateData.PollFailures = testCase.priorFailures
			if testCase.priorState != "" {
				task.PrivateData.PluginState = []byte(testCase.priorState)
			}
			require.NoError(t, model.DB.Create(task).Error)

			adaptor := &scriptedPollingAdaptor{statusCode: testCase.statusCode, fetchErr: testCase.fetchErr, parse: testCase.parse, parseErr: testCase.parseErr}
			require.NoError(t, updateVideoSingleTask(context.Background(), adaptor, ch, task.GetUpstreamTaskID(), map[string]*model.Task{
				task.GetUpstreamTaskID(): task,
			}))

			var persisted model.Task
			require.NoError(t, model.DB.First(&persisted, task.ID).Error)
			assert.EqualValues(t, testCase.wantStatus, persisted.Status)
			assert.Equal(t, testCase.wantFailures, persisted.PrivateData.PollFailures)
			if testCase.wantUnchanged {
				assert.Empty(t, persisted.FailReason)
			}
			if testCase.wantReason != "" {
				assert.Equal(t, testCase.wantReason, persisted.FailReason)
			}
			if testCase.wantState != "" {
				assert.JSONEq(t, testCase.wantState, string(persisted.PrivateData.PluginState))
			}
			assert.Equal(t, initialQuota, getUserQuota(t, userID))
			assert.Equal(t, tokenRemain, getTokenRemainQuota(t, tokenID))
			assert.Equal(t, preConsumed, persisted.Quota)
			assert.Zero(t, countLogs(t), "HTTP classification cannot prove a historical refund")
		})
	}
}

func TestUpdateBatchTasksPollClassification(t *testing.T) {
	testCases := []struct {
		name         string
		statusCode   int
		resultStatus model.TaskStatus
		wantStatus   model.TaskStatus
		wantFailures int
		wantReason   string
	}{
		{
			name:       "404 classifies unarchived batch without refund",
			statusCode: http.StatusNotFound,
			wantStatus: model.TaskStatusFailure,
			wantReason: "Upstream task not found",
		},
		{
			name:         "401 increments every task",
			statusCode:   http.StatusUnauthorized,
			wantStatus:   model.TaskStatusInProgress,
			wantFailures: 1,
		},
		{
			name:         "UNKNOWN increments",
			statusCode:   http.StatusOK,
			resultStatus: model.TaskStatusUnknown,
			wantStatus:   model.TaskStatusInProgress,
			wantFailures: 1,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			truncate(t)
			const userID, tokenID, channelID = 610, 610, 610
			const initialQuota, preConsumed, tokenRemain = 10_000, 4_000, 7_000
			seedUser(t, userID, initialQuota)
			seedToken(t, tokenID, userID, "sk-batch-class", tokenRemain)
			seedTaskPollingChannel(t, channelID, true)

			task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
			task.TaskID = "task_batch_class"
			task.PrivateData.UpstreamTaskID = "upstream_batch_class"
			require.NoError(t, model.DB.Create(task).Error)
			upstreamID := task.GetUpstreamTaskID()

			adaptor := &scriptedBatchPollingAdaptor{
				scriptedPollingAdaptor: scriptedPollingAdaptor{statusCode: testCase.statusCode},
			}
			if testCase.resultStatus != "" {
				adaptor.results = map[string]*BatchTaskResult{
					upstreamID: {TaskInfo: relaycommon.TaskInfo{TaskID: upstreamID, Status: string(testCase.resultStatus), Reason: "weird"}},
				}
			}

			require.NoError(t, UpdateBatchTasks(context.Background(), adaptor, map[int][]string{channelID: {upstreamID}}, map[string]*model.Task{upstreamID: task}))

			var persisted model.Task
			require.NoError(t, model.DB.First(&persisted, task.ID).Error)
			assert.EqualValues(t, testCase.wantStatus, persisted.Status)
			assert.Equal(t, testCase.wantFailures, persisted.PrivateData.PollFailures)
			if testCase.wantReason != "" {
				assert.Equal(t, testCase.wantReason, persisted.FailReason)
			}
			assert.Equal(t, initialQuota, getUserQuota(t, userID))
			assert.Equal(t, tokenRemain, getTokenRemainQuota(t, tokenID))
			assert.Equal(t, preConsumed, persisted.Quota)
			assert.Zero(t, countLogs(t), "HTTP classification cannot prove a historical refund")
		})
	}
}
