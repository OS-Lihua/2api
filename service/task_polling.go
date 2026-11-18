package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	perfmetrics "github.com/QuantumNous/new-api/pkg/perf_metrics"
	"github.com/QuantumNous/new-api/relay/channel/task/taskcommon"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/MoeForge/moego/ojson"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/samber/lo"
)

// TaskPollingAdaptor 定义轮询所需的最小适配器接口，避免 service -> relay 的循环依赖
type TaskPollingAdaptor interface {
	Init(info *relaycommon.RelayInfo)
	FetchTask(baseURL string, key string, task *model.Task, proxy string) (*http.Response, error)
	ParseTaskResult(task *model.Task, resp *http.Response, body []byte) (*relaycommon.TaskInfo, error)
	// AdjustBillingOnComplete 在任务到达终态（成功/失败）时由轮询循环调用。
	// 返回正数触发差额结算（补扣/退还），返回 0 保持预扣费金额不变。
	AdjustBillingOnComplete(task *model.Task, taskResult *relaycommon.TaskInfo) int
}

type contextTaskPollingAdaptor interface {
	FetchTaskWithContext(context.Context, string, string, *model.Task, string) (*http.Response, error)
	ParseTaskResultWithContext(context.Context, *model.Task, *http.Response, []byte) (*relaycommon.TaskInfo, error)
}

type BatchTaskPollingAdaptor interface {
	TaskPollingAdaptor
	FetchMode() string
	FetchBatchTasks(baseURL, key string, tasks []*model.Task, proxy string) (*http.Response, error)
	ParseBatchResult(tasks []*model.Task, resp *http.Response, body []byte) (map[string]*BatchTaskResult, error)
}

type contextBatchTaskPollingAdaptor interface {
	FetchBatchTasksWithContext(context.Context, string, string, []*model.Task, string) (*http.Response, error)
	ParseBatchResultWithContext(context.Context, []*model.Task, *http.Response, []byte) (map[string]*BatchTaskResult, error)
}

const (
	pollClassOK           = "ok"
	pollClassOtherClient  = "other_client"
	pollClassNotFound     = "not_found"
	pollClassAuth         = "auth"
	pollClassTransient    = "transient"
	pollClassUnrecognized = "unrecognized"
	pollClassHookError    = "hook_error"
	pollClassTransport    = "transport_error"
)

type BatchTaskResult struct {
	TaskInfo   relaycommon.TaskInfo
	Action     string
	SubmitTime int64
	StartTime  int64
	FinishTime int64
	Data       any
}

// GetTaskAdaptorFunc 由 main 包注入，用于获取指定平台的任务适配器。
// 打破 service -> relay -> relay/channel -> service 的循环依赖。
var GetTaskAdaptorFunc func(platform constant.TaskPlatform) TaskPollingAdaptor

// sweepTimedOutTasks 在主轮询之前独立清理超时任务。
// 每次最多处理 100 条，剩余的下个周期继续处理。
// 使用 per-task CAS (UpdateWithStatus) 防止覆盖被正常轮询已推进的任务。
func sweepTimedOutTasks(ctx context.Context) {
	if constant.TaskTimeoutMinutes <= 0 {
		return
	}
	cutoff := time.Now().Unix() - int64(constant.TaskTimeoutMinutes)*60
	tasks := model.GetTimedOutUnfinishedTasks(cutoff, 100)
	if len(tasks) == 0 {
		return
	}

	reason := "Task timed out"
	legacyReason := "Task timed out (a task left by the old system is not refunded; contact the administrator)"
	now := time.Now().Unix()
	timedOutCount := 0

	for _, task := range tasks {
		isLegacy := task.SubmitTime > 0 && task.SubmitTime < model.TaskRefundLegacyCutoff

		oldStatus := task.Status
		task.Status = model.TaskStatusFailure
		task.Progress = "100%"
		task.FinishTime = now
		if isLegacy {
			task.FailReason = legacyReason
		} else {
			task.FailReason = reason
		}

		won, err := task.UpdateWithStatus(oldStatus)
		if err != nil {
			logger.LogError(ctx, common.LogText("Task timeout update failed: task_id=%d provider_id=%d", task.ID, task.ChannelId))
			continue
		}
		if !won {
			logger.LogInfo(ctx, common.LogText("Task timeout update skipped after concurrent transition: task_id=%d provider_id=%d", task.ID, task.ChannelId))
			continue
		}
		timedOutCount++
	}

	if timedOutCount > 0 {
		logger.LogInfo(ctx, common.LogText("sweepTimedOutTasks: timed out %d tasks", timedOutCount))
	}
}

// TaskPollSummary is the result recorded on an async_task_poll system task row,
// summarizing one polling pass.
type TaskPollSummary struct {
	UnfinishedTasks  int `json:"unfinished_tasks"`
	PlatformsScanned int `json:"platforms_scanned"`
	NullTasksFailed  int `json:"null_tasks_failed"`
}

// RunTaskPollingOnce performs one async-task (Suno/video) polling pass
// synchronously. It honors ctx cancellation (the system-task runner cancels it
// when the lease is lost) and, when report is non-nil, reports progress as
// (processedPlatforms, totalPlatforms). It returns immediately if the task
// adaptor factory has not been wired yet, to avoid a nil call during startup.
func RunTaskPollingOnce(ctx context.Context, report func(processed, total int)) TaskPollSummary {
	summary := TaskPollSummary{}
	if GetTaskAdaptorFunc == nil {
		return summary
	}
	if ctx == nil {
		ctx = context.Background()
	}

	common.SysLog(common.LogText("task progress polling started"))
	sweepTimedOutTasks(ctx)
	allTasks := model.GetAllUnFinishSyncTasks(constant.TaskQueryLimit)
	summary.UnfinishedTasks = len(allTasks)
	platformTask := make(map[constant.TaskPlatform][]*model.Task)
	gatewayTasks := make(map[int][]*model.Task)
	for _, t := range allTasks {
		if legacy := t.PrivateData.LegacyObligation; legacy != nil {
			gatewayTasks[legacy.ProviderID] = append(gatewayTasks[legacy.ProviderID], t)
			continue
		}
		if t.PrivateData.Gateway != nil {
			if t.PrivateData.Gateway.Accepted {
				providerID := t.PrivateData.Gateway.ProviderID
				gatewayTasks[providerID] = append(gatewayTasks[providerID], t)
			}
			continue
		}
		archived, err := model.ValidateLegacyTaskPolling(ctx, t)
		if err != nil {
			logger.LogWarn(ctx, common.LogText("Legacy task polling remains pending: task_id=%d provider_id=%d", t.ID, t.ChannelId))
			continue
		}
		if archived {
			gatewayTasks[t.ChannelId] = append(gatewayTasks[t.ChannelId], t)
			continue
		}
		platformTask[t.Platform] = append(platformTask[t.Platform], t)
	}
	var gatewayPolls sync.WaitGroup
	for _, tasks := range gatewayTasks {
		gatewayPolls.Add(1)
		gopool.Go(func() {
			defer gatewayPolls.Done()
			for _, task := range tasks {
				if ctx.Err() != nil {
					return
				}
				var err error
				if task.PrivateData.Gateway == nil && task.PrivateData.LegacyObligation == nil {
					err = UpdateLegacyTask(ctx, task)
				} else {
					err = UpdateGatewayTask(ctx, task)
				}
				if err != nil {
					logger.LogError(ctx, common.LogText("Task polling remains pending: task_id=%d provider_id=%d", task.ID, task.ChannelId))
				}
			}
		})
	}

	totalPlatforms := len(platformTask)
	processedPlatforms := 0
	for platform, tasks := range platformTask {
		if ctx.Err() != nil {
			break
		}
		if report != nil {
			report(processedPlatforms, totalPlatforms)
		}
		processedPlatforms++
		if len(tasks) == 0 {
			continue
		}
		summary.PlatformsScanned++
		taskChannelM := make(map[int][]string)
		taskM := make(map[string]*model.Task)
		nullTaskIds := make([]int64, 0)
		for _, task := range tasks {
			if task.PrivateData.Gateway != nil && !task.PrivateData.Gateway.Accepted {
				continue
			}
			upstreamID := task.GetUpstreamTaskID()
			if upstreamID == "" {
				// 统计失败的未完成任务
				nullTaskIds = append(nullTaskIds, task.ID)
				continue
			}
			taskM[upstreamID] = task
			taskChannelM[task.ChannelId] = append(taskChannelM[task.ChannelId], upstreamID)
		}
		if len(nullTaskIds) > 0 {
			summary.NullTasksFailed += len(nullTaskIds)
			err := model.TaskBulkUpdateByID(nullTaskIds, map[string]any{
				"status":   "FAILURE",
				"progress": "100%",
			})
			if err != nil {
				logger.LogError(ctx, common.LogText("Missing task identity update failed: count=%d", len(nullTaskIds)))
			} else {
				logger.LogInfo(ctx, common.LogText("Missing task identity update completed: count=%d", len(nullTaskIds)))
			}
		}
		if len(taskChannelM) == 0 {
			continue
		}

		DispatchPlatformUpdate(ctx, platform, taskChannelM, taskM)
	}
	gatewayPolls.Wait()
	if report != nil && ctx.Err() == nil {
		report(totalPlatforms, totalPlatforms)
	}
	common.SysLog(common.LogText("task progress polling finished"))
	return summary
}

// DispatchPlatformUpdate 按平台分发轮询更新
func DispatchPlatformUpdate(ctx context.Context, platform constant.TaskPlatform, taskChannelM map[int][]string, taskM map[string]*model.Task) {
	if ctx == nil {
		ctx = context.Background()
	}
	if platform == constant.TaskPlatformMidjourney {
		// MJ 轮询由其自身处理，这里预留入口
		return
	}
	adaptor := GetTaskAdaptorFunc(platform)
	if batchAdaptor, ok := adaptor.(BatchTaskPollingAdaptor); ok && batchAdaptor.FetchMode() == "batch" {
		if err := UpdateBatchTasks(ctx, batchAdaptor, taskChannelM, taskM); err != nil {
			common.SysLog(common.LogText("Batch task polling interrupted"))
		}
		return
	}
	if err := UpdateVideoTasks(ctx, platform, taskChannelM, taskM); err != nil {
		common.SysLog(common.LogText("Single task polling interrupted"))
	}
}

func UpdateBatchTasks(ctx context.Context, adaptor BatchTaskPollingAdaptor, taskChannelM map[int][]string, taskM map[string]*model.Task) error {
	for channelId, taskIds := range taskChannelM {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := updateBatchTasks(ctx, adaptor, channelId, taskIds, taskM)
		if err != nil {
			logger.LogError(ctx, common.LogText("Batch task polling failed: provider_id=%d", channelId))
		}
	}
	return nil
}

type gatewayTaskProviderSelection struct {
	Provider *model.Provider
	KeyIndex int
}

func updateBatchTasks(ctx context.Context, adaptor BatchTaskPollingAdaptor, channelId int, taskIds []string, taskM map[string]*model.Task, pinned ...gatewayTaskProviderSelection) error {
	logger.LogInfo(ctx, common.LogText("channel #%d has %d unfinished tasks", channelId, len(taskIds)))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if len(taskIds) == 0 {
		return nil
	}
	var ch *model.Provider
	var err error
	if len(pinned) > 0 {
		ch = pinned[0].Provider
	} else {
		ch, err = model.CacheGetProvider(channelId)
	}
	if err != nil {
		logger.LogError(ctx, common.LogText("Task polling Provider lookup failed: provider_id=%d", channelId))
		// Collect DB primary key IDs for bulk update (taskIds are upstream IDs, not task_id column values)
		var failedIDs []int64
		for _, upstreamID := range taskIds {
			if t, ok := taskM[upstreamID]; ok {
				failedIDs = append(failedIDs, t.ID)
			}
		}
		err = model.TaskBulkUpdateByID(failedIDs, map[string]any{
			"fail_reason": "Failed to get the channel information. Please contact the administrator",
			"status":      "FAILURE",
			"progress":    "100%",
		})
		if err != nil {
			logger.LogError(ctx, common.LogText("Batch task status update failed: provider_id=%d", channelId))
		}
		return err
	}
	proxy := ch.GetSetting().Proxy
	baseURL := ch.GetBaseURL()
	if baseURL == "" {
		baseURL = constant.GetChannelBaseURL(ch.Type)
	}
	tasks := make([]*model.Task, 0, len(taskIds))
	for _, upstreamID := range taskIds {
		if task := taskM[upstreamID]; task != nil {
			tasks = append(tasks, task)
		}
	}
	// The channel type tells plugin adaptors whether the upstream is another
	// New API gateway, the same signal submission derives from the request.
	info := &relaycommon.RelayInfo{}
	info.ProviderMeta = &relaycommon.ProviderMeta{
		ChannelType:     ch.Type,
		ChannelId:       ch.Id,
		ChannelBaseUrl:  baseURL,
		ChannelSetting:  ch.GetSetting(),
		HeadersOverride: ch.GetHeaderOverride(),
	}
	if len(pinned) > 0 {
		info.ChannelMultiKeyIndex = pinned[0].KeyIndex
	}
	info.ApiKey = ch.Key
	adaptor.Init(info)
	contextual, hasContext := adaptor.(contextBatchTaskPollingAdaptor)
	var resp *http.Response
	if hasContext {
		resp, err = contextual.FetchBatchTasksWithContext(ctx, baseURL, ch.Key, tasks, proxy)
	} else {
		resp, err = adaptor.FetchBatchTasks(baseURL, ch.Key, tasks, proxy)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		logger.LogWarn(ctx, common.LogText("Batch task poll failed: provider_id=%d class=%s http=%d", channelId, pollClassTransport, 0))
		return recordPollFailureForTasks(ctx, adaptor, tasks, pollClassTransport, 0)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		logger.LogWarn(ctx, common.LogText("Batch task poll failed: provider_id=%d class=%s http=%d", channelId, pollClassTransport, resp.StatusCode))
		return recordPollFailureForTasks(ctx, adaptor, tasks, pollClassTransport, resp.StatusCode)
	}
	switch classifyPollHTTP(resp.StatusCode) {
	case pollClassNotFound:
		for _, task := range tasks {
			if task != nil {
				logger.LogWarn(ctx, common.LogText("Task poll failed: task_id=%d provider_id=%d class=%s failures=%d http=%d", task.ID, task.ChannelId, pollClassNotFound, task.PrivateData.PollFailures, resp.StatusCode))
			}
		}
		return failTasksFromPoll(ctx, adaptor, tasks, "Upstream task not found")
	case pollClassAuth:
		logger.LogWarn(ctx, common.LogText("task poll auth failure channel_id=%d http=%d", channelId, resp.StatusCode))
		return recordPollFailureForTasks(ctx, adaptor, tasks, pollClassAuth, resp.StatusCode)
	case pollClassTransient:
		return recordPollFailureForTasks(ctx, adaptor, tasks, pollClassTransient, resp.StatusCode)
	}
	var responseItems map[string]*BatchTaskResult
	if hasContext {
		responseItems, err = contextual.ParseBatchResultWithContext(ctx, tasks, resp, responseBody)
	} else {
		responseItems, err = adaptor.ParseBatchResult(tasks, resp, responseBody)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return recordPollFailureForTasks(ctx, adaptor, tasks, pollClassHookError, resp.StatusCode)
	}
	for upstreamID, responseItem := range responseItems {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		task := taskM[upstreamID]
		if task == nil {
			logger.LogWarn(ctx, common.LogText("Batch task response ignored: provider_id=%d class=unmatched_identity", channelId))
			continue
		}
		snap := task.Snapshot()
		httpClass := classifyPollHTTP(resp.StatusCode)
		parsedStatus := model.TaskStatus(responseItem.TaskInfo.Status)
		if parsedStatus == model.TaskStatusUnknown || parsedStatus == "" || !knownPollStatus(parsedStatus) {
			if err := recordPollFailure(ctx, adaptor, task, snap.Status, pollClassUnrecognized, resp.StatusCode); err != nil {
				logger.LogError(ctx, common.LogText("Task poll status update failed: task_id=%d provider_id=%d", task.ID, task.ChannelId))
			}
			continue
		}
		if httpClass == pollClassOtherClient && isNonTerminalPollStatus(parsedStatus) {
			if err := recordPollFailure(ctx, adaptor, task, snap.Status, pollClassUnrecognized, resp.StatusCode); err != nil {
				logger.LogError(ctx, common.LogText("Task poll status update failed: task_id=%d provider_id=%d", task.ID, task.ChannelId))
			}
			continue
		}
		if isNonTerminalPollStatus(parsedStatus) {
			task.PrivateData.PollFailures = 0
		}
		if len(responseItem.TaskInfo.PluginState) > 0 {
			task.PrivateData.PluginState = responseItem.TaskInfo.PluginState
		}
		task.Status = lo.If(parsedStatus != "", parsedStatus).Else(task.Status)
		task.FailReason = lo.If(responseItem.TaskInfo.Reason != "", responseItem.TaskInfo.Reason).Else(task.FailReason)
		task.SubmitTime = lo.If(responseItem.SubmitTime != 0, responseItem.SubmitTime).Else(task.SubmitTime)
		task.StartTime = lo.If(responseItem.StartTime != 0, responseItem.StartTime).Else(task.StartTime)
		task.FinishTime = lo.If(responseItem.FinishTime != 0, responseItem.FinishTime).Else(task.FinishTime)
		if responseItem.TaskInfo.Progress != "" {
			task.Progress = responseItem.TaskInfo.Progress
		}
		archived := false
		if task.PrivateData.Gateway == nil && task.PrivateData.LegacyObligation == nil {
			var err error
			archived, err = model.ValidateLegacyTaskPolling(ctx, task)
			if err != nil {
				return err
			}
		}
		if responseItem.TaskInfo.Reason != "" && !archived || task.Status == model.TaskStatusFailure {
			logger.LogInfo(ctx, common.LogText("Task failed: task_id=%d provider_id=%d", task.ID, task.ChannelId))
			task.Status = model.TaskStatusFailure
			task.Progress = "100%"
		}
		if responseItem.TaskInfo.Status == model.TaskStatusSuccess {
			task.Progress = "100%"
		}
		if responseItem.Data != nil {
			task.SetData(responseItem.Data)
		} else if task.Status == model.TaskStatusSuccess || task.Status == model.TaskStatusFailure {
			logger.LogWarn(ctx, common.LogText(
				"Batch task terminal response omitted data: task_id=%d provider_id=%d",
				task.ID, task.ChannelId,
			))
		}
		if responseItem.TaskInfo.Url != "" {
			task.PrivateData.ResultURL = responseItem.TaskInfo.Url
		}

		isDone := task.Status == model.TaskStatusSuccess || task.Status == model.TaskStatusFailure
		terminalTransition := isDone && snap.Status != task.Status
		if terminalTransition && task.PrivateData.Gateway != nil {
			task.PrivateData.Gateway.CompletionFacts = responseItem.TaskInfo.UsageFacts
			task.PrivateData.Gateway.CompletionUnknown = responseItem.TaskInfo.UsageFactsUnknown
		}
		won, updateErr := task.UpdateWithStatus(snap.Status)
		if updateErr != nil {
			logger.LogError(ctx, common.LogText("Task poll status update failed: task_id=%d provider_id=%d", task.ID, task.ChannelId))
			continue
		}
		if !won {
			logger.LogWarn(ctx, common.LogText("Batch task update skipped after concurrent transition: task_id=%d provider_id=%d", task.ID, task.ChannelId))
			continue
		}
		if terminalTransition {
			finalizeTerminalTask(ctx, adaptor, task, &responseItem.TaskInfo)
		}
	}
	return nil
}

// UpdateVideoTasks 按渠道更新所有视频任务
func UpdateVideoTasks(ctx context.Context, platform constant.TaskPlatform, taskChannelM map[int][]string, taskM map[string]*model.Task) error {
	channelIDs := make([]int, 0, len(taskChannelM))
	for channelID := range taskChannelM {
		channelIDs = append(channelIDs, channelID)
	}
	sort.Ints(channelIDs)

	var wg sync.WaitGroup
	for _, channelId := range channelIDs {
		taskIds := taskChannelM[channelId]
		if len(taskIds) == 0 {
			continue
		}
		taskIds = append([]string(nil), taskIds...)

		wg.Add(1)
		gopool.Go(func() {
			defer wg.Done()
			if err := updateVideoTasks(ctx, platform, channelId, taskIds, taskM); err != nil {
				logger.LogError(ctx, common.LogText("Single task polling failed: provider_id=%d", channelId))
			}
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func updateVideoTasks(ctx context.Context, platform constant.TaskPlatform, channelId int, taskIds []string, taskM map[string]*model.Task) error {
	logger.LogInfo(ctx, common.LogText("Channel #%d pending video tasks: %d", channelId, len(taskIds)))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if len(taskIds) == 0 {
		return nil
	}
	cacheGetChannel, err := model.CacheGetProvider(channelId)
	if err != nil {
		// Collect DB primary key IDs for bulk update (taskIds are upstream IDs, not task_id column values)
		var failedIDs []int64
		for _, upstreamID := range taskIds {
			if t, ok := taskM[upstreamID]; ok {
				failedIDs = append(failedIDs, t.ID)
			}
		}
		errUpdate := model.TaskBulkUpdateByID(failedIDs, map[string]any{
			"fail_reason": "Failed to get the channel information. Please contact the administrator",
			"status":      "FAILURE",
			"progress":    "100%",
		})
		if errUpdate != nil {
			logger.LogError(ctx, common.LogText("Single task status update failed: provider_id=%d", channelId))
		}
		return fmt.Errorf("CacheGetProvider failed: %w", err)
	}
	adaptor := GetTaskAdaptorFunc(platform)
	if adaptor == nil {
		return fmt.Errorf("video adaptor not found")
	}
	info := &relaycommon.RelayInfo{}
	info.ProviderMeta = &relaycommon.ProviderMeta{
		ChannelType:     cacheGetChannel.Type,
		ChannelId:       cacheGetChannel.Id,
		ChannelBaseUrl:  cacheGetChannel.GetBaseURL(),
		ChannelSetting:  cacheGetChannel.GetSetting(),
		HeadersOverride: cacheGetChannel.GetHeaderOverride(),
	}
	info.ApiKey = cacheGetChannel.Key
	adaptor.Init(info)
	disablePollingSleep := cacheGetChannel.GetOtherSettings().DisableTaskPollingSleep
	for i, taskId := range taskIds {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := updateVideoSingleTask(ctx, adaptor, cacheGetChannel, taskId, taskM); err != nil {
			logger.LogError(ctx, common.LogText("Single task poll update failed: provider_id=%d", channelId))
		}
		if disablePollingSleep || i == len(taskIds)-1 {
			continue
		}

		// sleep 1 second between tasks for this channel only.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
	return nil
}

func updateVideoSingleTask(ctx context.Context, adaptor TaskPollingAdaptor, ch *model.Provider, taskId string, taskM map[string]*model.Task) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	baseURL := constant.GetChannelBaseURL(ch.Type)
	if ch.GetBaseURL() != "" {
		baseURL = ch.GetBaseURL()
	}
	proxy := ch.GetSetting().Proxy

	task := taskM[taskId]
	if task == nil {
		logger.LogError(ctx, common.LogText("Single task poll identity is unavailable: provider_id=%d", ch.Id))
		return fmt.Errorf("task polling identity not found")
	}
	key := ch.Key

	privateData := task.PrivateData
	if privateData.Key != "" {
		key = privateData.Key
	}
	snap := task.Snapshot()
	contextual, hasContext := adaptor.(contextTaskPollingAdaptor)
	var resp *http.Response
	var err error
	if hasContext {
		resp, err = contextual.FetchTaskWithContext(ctx, baseURL, key, task, proxy)
	} else {
		resp, err = adaptor.FetchTask(baseURL, key, task, proxy)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return recordPollFailure(ctx, adaptor, task, snap.Status, pollClassTransport, 0)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return recordPollFailure(ctx, adaptor, task, snap.Status, pollClassTransport, resp.StatusCode)
	}

	logger.LogDebug(ctx, "%s", common.LogText("Task poll response received: task_id=%d provider_id=%d http=%d", task.ID, task.ChannelId, resp.StatusCode))

	switch classifyPollHTTP(resp.StatusCode) {
	case pollClassNotFound:
		logger.LogWarn(ctx, common.LogText("Task poll failed: task_id=%d provider_id=%d class=%s failures=%d http=%d", task.ID, task.ChannelId, pollClassNotFound, task.PrivateData.PollFailures, resp.StatusCode))
		return failTaskFromPoll(ctx, adaptor, task, snap.Status, "Upstream task not found")
	case pollClassAuth:
		logger.LogWarn(ctx, common.LogText("Task poll failed: task_id=%d provider_id=%d class=%s http=%d", task.ID, ch.Id, pollClassAuth, resp.StatusCode))
		return recordPollFailure(ctx, adaptor, task, snap.Status, pollClassAuth, resp.StatusCode)
	case pollClassTransient:
		return recordPollFailure(ctx, adaptor, task, snap.Status, pollClassTransient, resp.StatusCode)
	}

	taskResult := &relaycommon.TaskInfo{}
	// try parse as New API response format
	var responseItems taskdto.TaskResponse[model.Task]
	if err = common.Unmarshal(responseBody, &responseItems); err == nil && responseItems.IsSuccess() {
		logger.LogDebug(ctx, "%s", common.LogText("Task poll gateway envelope parsed: task_id=%d provider_id=%d http=%d", task.ID, task.ChannelId, resp.StatusCode))
		t := responseItems.Data
		taskResult.TaskID = t.TaskID
		taskResult.Status = string(t.Status)
		taskResult.Url = t.GetResultURL()
		taskResult.Progress = t.Progress
		taskResult.Reason = t.FailReason
		task.Data = t.Data
	} else {
		if hasContext {
			taskResult, err = contextual.ParseTaskResultWithContext(ctx, task, resp, responseBody)
		} else {
			taskResult, err = adaptor.ParseTaskResult(task, resp, responseBody)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return recordPollFailure(ctx, adaptor, task, snap.Status, pollClassHookError, resp.StatusCode)
		}
	}

	logger.LogDebug(ctx, "%s", common.LogText("Task poll result parsed: task_id=%d provider_id=%d http=%d", task.ID, task.ChannelId, resp.StatusCode))

	parsedStatus := model.TaskStatus(taskResult.Status)
	if parsedStatus == model.TaskStatusUnknown || parsedStatus == "" || !knownPollStatus(parsedStatus) {
		return recordPollFailure(ctx, adaptor, task, snap.Status, pollClassUnrecognized, resp.StatusCode)
	}
	if classifyPollHTTP(resp.StatusCode) == pollClassOtherClient && isNonTerminalPollStatus(parsedStatus) {
		return recordPollFailure(ctx, adaptor, task, snap.Status, pollClassUnrecognized, resp.StatusCode)
	}

	task.Data = redactVideoResponseBody(responseBody)
	if len(taskResult.PluginState) > 0 {
		task.PrivateData.PluginState = taskResult.PluginState
	}
	if isNonTerminalPollStatus(parsedStatus) {
		task.PrivateData.PollFailures = 0
	}

	now := time.Now().Unix()
	shouldFinalizeBilling := false

	task.Status = parsedStatus
	switch parsedStatus {
	case model.TaskStatusSubmitted:
		task.Progress = taskcommon.ProgressSubmitted
	case model.TaskStatusQueued:
		task.Progress = taskcommon.ProgressQueued
	case model.TaskStatusInProgress:
		task.Progress = taskcommon.ProgressInProgress
		if task.StartTime == 0 {
			task.StartTime = now
		}
	case model.TaskStatusSuccess:
		task.Progress = taskcommon.ProgressComplete
		if task.FinishTime == 0 {
			task.FinishTime = now
		}
		if strings.HasPrefix(taskResult.Url, "data:") {
			// data: URI (e.g. Vertex base64 encoded video) — keep in Data, not in ResultURL
			task.PrivateData.ResultURL = taskcommon.BuildProxyURL(task.TaskID)
		} else if taskResult.Url != "" {
			// Direct upstream URL (e.g. Kling, Ali, Doubao, etc.)
			task.PrivateData.ResultURL = taskResult.Url
		} else {
			// No URL from adaptor — construct proxy URL using public task ID
			task.PrivateData.ResultURL = taskcommon.BuildProxyURL(task.TaskID)
		}
		shouldFinalizeBilling = true
	case model.TaskStatusFailure:
		logger.LogInfo(ctx, common.LogText("Task failed: task_id=%d provider_id=%d", task.ID, task.ChannelId))
		task.Status = model.TaskStatusFailure
		task.Progress = taskcommon.ProgressComplete
		if task.FinishTime == 0 {
			task.FinishTime = now
		}
		task.FailReason = taskResult.Reason
		taskResult.Progress = taskcommon.ProgressComplete
		shouldFinalizeBilling = true
	}
	if taskResult.Progress != "" {
		task.Progress = taskResult.Progress
	}

	if task.PrivateData.Gateway == nil && task.PrivateData.LegacyObligation == nil {
		if _, err := model.ValidateLegacyTaskPolling(ctx, task); err != nil {
			return err
		}
	}
	isDone := task.Status == model.TaskStatusSuccess || task.Status == model.TaskStatusFailure
	if isDone && snap.Status != task.Status {
		if task.PrivateData.Gateway != nil {
			task.PrivateData.Gateway.CompletionFacts = taskResult.UsageFacts
			task.PrivateData.Gateway.CompletionUnknown = taskResult.UsageFactsUnknown
		}
		if legacy := task.PrivateData.LegacyObligation; legacy != nil {
			legacy.CompletionFacts = taskResult.UsageFacts
			legacy.CompletionUnknown = taskResult.UsageFactsUnknown
		}
		won, err := task.UpdateWithStatus(snap.Status)
		if err != nil {
			logger.LogError(ctx, common.LogText("Task poll status update failed: task_id=%d provider_id=%d", task.ID, task.ChannelId))
			shouldFinalizeBilling = false
		} else if !won {
			logger.LogWarn(ctx, common.LogText("Task update skipped after concurrent transition: task_id=%d provider_id=%d", task.ID, task.ChannelId))
			shouldFinalizeBilling = false
		}
	} else if !snap.Equal(task.Snapshot()) {
		if _, err := task.UpdateWithStatus(snap.Status); err != nil {
			logger.LogError(ctx, common.LogText("Task poll status update failed: task_id=%d provider_id=%d", task.ID, task.ChannelId))
		}
	} else {
		// No changes, skip update
		logger.LogDebug(ctx, "%s", common.LogText("Task poll unchanged: task_id=%d provider_id=%d", task.ID, task.ChannelId))
	}

	if shouldFinalizeBilling {
		finalizeTerminalTask(ctx, adaptor, task, taskResult)
	}

	return nil
}

// finalizeTerminalTask 终态统一收尾（状态 CAS 赢家调用，恰好一次）：采样 + 结算 + 失败兜底退款。
func finalizeTerminalTask(ctx context.Context, adaptor TaskPollingAdaptor, task *model.Task, taskResult *relaycommon.TaskInfo) {
	perfmetrics.RecordTaskResult(task, taskResult)
	if task.PrivateData.LegacyObligation != nil {
		return
	}
	if task.PrivateData.Gateway != nil && taskResult != nil {
		task.PrivateData.Gateway.CompletionUnknown = taskResult.UsageFactsUnknown
	}
	if task.PrivateData.Gateway != nil {
		if err := SettleGatewayTask(ctx, task); err != nil {
			logger.LogError(ctx, common.LogText("Gateway task settlement remains pending: task_id=%d provider_id=%d", task.ID, task.ChannelId))
		}
		return
	}
	// Historical terminal business facts do not verify a debit, refund or price.
	// Keep both the source counters and independent evidence untouched.
}

func redactVideoResponseBody(body []byte) []byte {
	// Only the redacted members change; the rest keeps the upstream bytes.
	if !ojson.ValidBytes(body) || !ojson.GetBytesNoCopy(body, "@this").IsObject() {
		return body
	}
	doc := ojson.NewDoc(body)
	if resp := ojson.GetBytesNoCopy(body, "response"); resp.IsObject() {
		_ = doc.Delete("response.bytesBase64Encoded")
		if video := resp.Get("video"); video.Type == ojson.String {
			_ = doc.Set("response.video", truncateBase64(video.Str))
		}
		if videos := resp.Get("videos"); videos.IsArray() {
			for i, video := range videos.Array() {
				if video.IsObject() {
					_ = doc.Delete(fmt.Sprintf("response.videos.%d.bytesBase64Encoded", i))
				}
			}
		}
	}
	if steps := ojson.GetBytesNoCopy(body, "steps"); steps.IsArray() {
		for i, step := range steps.Array() {
			parts := step.Get("content")
			if !parts.IsArray() {
				continue
			}
			for j, part := range parts.Array() {
				if part.Get("type").String() == "video" {
					_ = doc.Delete(fmt.Sprintf("steps.%d.content.%d.data", i, j))
				}
			}
		}
	}
	return doc.Bytes()
}

func truncateBase64(s string) string {
	const maxKeep = 256
	if len(s) <= maxKeep {
		return s
	}
	return s[:maxKeep] + "..."
}

// settleTaskBillingOnComplete is retained for legacy callers only. Historical
// integer expressions and adaptor hints cannot finalize a monetary obligation.
func settleTaskBillingOnComplete(ctx context.Context, adaptor TaskPollingAdaptor, task *model.Task, taskResult *relaycommon.TaskInfo) bool {
	return false
}

func classifyPollHTTP(statusCode int) string {
	switch {
	case statusCode >= 200 && statusCode < 300:
		return pollClassOK
	case statusCode == http.StatusNotFound || statusCode == http.StatusGone:
		return pollClassNotFound
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return pollClassAuth
	case statusCode == http.StatusTooManyRequests || statusCode >= 500:
		return pollClassTransient
	case statusCode >= 400 && statusCode < 500:
		return pollClassOtherClient
	default:
		return pollClassTransient
	}
}

func knownPollStatus(status model.TaskStatus) bool {
	switch status {
	case model.TaskStatusNotStart, model.TaskStatusSubmitted, model.TaskStatusQueued, model.TaskStatusInProgress, model.TaskStatusSuccess, model.TaskStatusFailure:
		return true
	default:
		return false
	}
}

func isNonTerminalPollStatus(status model.TaskStatus) bool {
	switch status {
	case model.TaskStatusNotStart, model.TaskStatusSubmitted, model.TaskStatusQueued, model.TaskStatusInProgress:
		return true
	default:
		return false
	}
}

func recordPollFailure(ctx context.Context, adaptor TaskPollingAdaptor, task *model.Task, fromStatus model.TaskStatus, class string, statusCode int) error {
	task.PrivateData.PollFailures++
	// TASK_POLL_MAX_FAILURES <= 0 disables the consecutive-failure cutoff, matching
	// TASK_TIMEOUT_MINUTES semantics; the 72h sweep (TASK_TIMEOUT_MINUTES default
	// 4320) remains the only backstop.
	giveUp := constant.TaskPollMaxFailures > 0 && task.PrivateData.PollFailures >= constant.TaskPollMaxFailures
	if giveUp || class == pollClassUnrecognized || class == pollClassHookError {
		// Provider payloads, reasons and transport errors may contain usable keys.
		// Keep diagnostics to local identities and host-controlled classifications.
		logger.LogWarn(ctx, common.LogText("Task poll failed: task_id=%d provider_id=%d class=%s failures=%d http=%d", task.ID, task.ChannelId, class, task.PrivateData.PollFailures, statusCode))
	}
	if giveUp {
		return failTaskFromPoll(ctx, adaptor, task, fromStatus, "Failed to query the upstream task status")
	}
	if _, err := task.UpdateWithStatus(fromStatus); err != nil {
		return err
	}
	return nil
}

func recordPollFailureForTasks(ctx context.Context, adaptor TaskPollingAdaptor, tasks []*model.Task, class string, statusCode int) error {
	var firstErr error
	for _, task := range tasks {
		if task == nil {
			continue
		}
		if err := recordPollFailure(ctx, adaptor, task, task.Status, class, statusCode); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func failTaskFromPoll(ctx context.Context, adaptor TaskPollingAdaptor, task *model.Task, fromStatus model.TaskStatus, reason string) error {
	if task.PrivateData.Gateway == nil && task.PrivateData.LegacyObligation == nil {
		archived, err := model.ValidateLegacyTaskPolling(ctx, task)
		if err != nil {
			return err
		}
		if archived {
			_, err := task.UpdateWithStatus(fromStatus)
			return err
		}
	}
	if task.PrivateData.LegacyObligation != nil || task.Platform == "midjourney" && task.PrivateData.Gateway != nil && task.PrivateData.Gateway.Accepted {
		// A missing response, HTTP error or retry cutoff is not an actual MJ
		// terminal fact. Preserve its accepted identity and unresolved obligation.
		_, err := task.UpdateWithStatus(fromStatus)
		return err
	}
	now := time.Now().Unix()
	task.Status = model.TaskStatusFailure
	task.Progress = taskcommon.ProgressComplete
	if task.FinishTime == 0 {
		task.FinishTime = now
	}
	task.FailReason = reason
	won, err := task.UpdateWithStatus(fromStatus)
	if err != nil {
		return err
	}
	if !won {
		return nil
	}
	finalizeTerminalTask(ctx, adaptor, task, relaycommon.FailTaskInfo(reason))
	return nil
}

func failTasksFromPoll(ctx context.Context, adaptor TaskPollingAdaptor, tasks []*model.Task, reason string) error {
	var firstErr error
	for _, task := range tasks {
		if task == nil {
			continue
		}
		if err := failTaskFromPoll(ctx, adaptor, task, task.Status, reason); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
