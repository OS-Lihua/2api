# 现有路由与重试流程的扩展点

**结论**：本仓库的选路是“缓存/数据库里的候选渠道过滤 → 分组 → 优先级分层 → 权重随机”，重试是“控制器外层循环按错误分类决定是否换渠道或换分组”。要插入“先过滤、再评分”的智能选路，最小侵入点是把 `GetRandomSatisfiedChannel` 的“随机加权”替换为可扩展的评分器，并让 `SelectChannelForRequest` 继续负责 pin、亲和性、过滤与上下文装配。流式请求只能在**首包前**换渠道；首包后只能作为同一连接上的错误终止，不能安全重放请求。

术语：本报告按 issue #1 的约定使用 **Channel** 表示“凭据 + 地址”的现有渠道对象，**Route** 表示“Channel × 协议 × 单个上游模型”的路径；当前代码中的 `Ability`/`group2model2channels` 本质上是 Route 的启用索引。若后续引入 Provider/Publisher，本报告的结论仍然适用于 Route 层。

---

## 1. 代码地图

| 能力 | 主要入口 | 说明 |
|---|---|---|
| HTTP / WebSocket 请求进入 relay | `controller/relay.go:72` `Relay` | 由 `router/relay-router.go:84-160` 挂载，先经过 `middleware.Distribute()` |
| 请求前置选路 | `middleware/distributor.go:34` `Distribute` | 解析模型、做令牌模型权限过滤，然后调用 `service.SelectChannelForRequest` |
| 单次尝试统一选路 | `service/channel_select.go:303` `SelectChannelForRequest` | 顺序：pin → 首次亲和性 → 随机合格渠道 → 请求过滤器 |
| 随机/优先级/权重选择 | `service/channel_select.go:114` `CacheGetRandomSatisfiedChannel`；`model/channel_cache.go:117` `GetRandomSatisfiedChannel` | auto 分组跨组重试也在这里 |
| 重试决策 | `service/relay_error.go:20` `DecideRelayRetry` | 单一决策函数，reason 写入日志 |
| 渠道亲和性 | `service/channel_affinity.go:550,683,729` | 匹配规则、写缓存、记录日志 |
| 流式读取 | `relay/helper/stream_scanner.go:120` `StreamScannerHandler` | 记录首响应时间、流结束原因 |
| Responses WebSocket | `relay/responses_websocket.go:298,300` | 每个逻辑请求有独立重试循环 |

许可证与来源：当前 worktree 的 `go.mod:3` 模块名是 `github.com/QuantumNous/new-api`；`NOTICE` 标明项目 Copyright (c) QuantumNous and contributors，采用 **GNU Affero General Public License v3.0**。本报告引用的都是本仓库当前分支源码。

---

## 2. 选路如何实现

### 2.1 候选渠道的来源与过滤

有内存缓存时，`GetRandomSatisfiedChannel` 使用 `group2model2channels` 这个“分组 → 模型 → 渠道 ID”的索引：

- `model/channel_cache.go:31-99` 在 `InitChannelCache` 中从数据库加载全部渠道和 `Ability`，跳过禁用渠道，按渠道的多个分组和多个模型建立索引，并按优先级降序排序。
- `model/channel_cache.go:117-208` 是核心选择函数：
  1. 先按 `(group, model)` 取候选；
  2. 没有结果时用 `ratio_setting.RoutingMatchModelName` 归一化模型名再取一次；
  3. 通过 `filterCandidateIDs` 应用请求过滤器；
  4. 收集去重后的优先级并降序排序；
  5. 用 `retry` 作为优先级层索引，`retry >= len(uniquePriorities)` 时取最低可用优先级；
  6. 在同一优先级内按权重随机。

过滤器实现见 `model/channel_constraint.go:20-106`：

- 当前过滤器类型只有四种：`request_path`、`task_plugin_endpoint`、`task_plugin_identity`、`responses_websocket`，定义在 `dto/channel_constraints.go:20-54`。
- `filterCandidateIDs` 按 `filterEvalOrder` 顺序过滤，并返回第一个导致候选为空的过滤器类型，用于错误归因。
- `ChannelSatisfiesFilters` 用于 pin、亲和性渠道和最终渠道复核；这是“先过滤”的现成入口。

没有内存缓存时，`model/ability.go:108-185` 的 `GetChannel` 直接查询 `Ability` 表，逻辑与缓存路径一致，也支持同样的过滤器。

### 2.2 优先级、权重和重试层

`GetRandomSatisfiedChannel` 的 `retry` 参数不是“重试次数”本身，而是“优先级层数”：

- 第一次尝试用 `retry=0`，即最高优先级。
- 同优先级内失败后，外层循环递增 retry，下一轮会进入次高优先级。
- 如果 retry 超过优先级层数，会停留在最后一层，继续在该层内随机。
- 权重为 0 时有平滑逻辑：所有渠道权重为 0 时等效权重 100；平均权重小于 10 时放大 100 倍，避免小权重被完全吞掉。见 `model/channel_cache.go:174-207`。

因此，“先过滤、再评分”可以自然映射到这里的结构：**过滤决定候选集，优先级/权重决定从候选集中怎么选**。最直接的评分插入点是保留过滤后的 `channels` 列表，把原来的“同优先级随机加权”改成按评分排序或加权抽样。

### 2.3 pin 与亲和性

`SelectChannelForRequest`（`service/channel_select.go:303-406`）是所有路径共享的入口，优先级如下：

1. **Pinned channel 最高**：`dto.ChannelConstraints.ResolvedPin()` 解析出获胜 pin；token 管理员指定的渠道 `Rank 0`，origin task 绑定渠道 `Rank 10`（`dto/channel_constraints.go:6-9`）。同一个渠道上的多个 pin 会合并成更严格的 retry mode；不同渠道按 rank 竞争，输掉的 pin 会记录警告（`service/channel_select.go:305-331`）。
2. **仅首次尝试使用亲和性**：`retry.GetRetry() == 0` 时调用 `GetPreferredChannelByAffinity`；命中后仍要满足过滤器、启用状态和分组模型能力。strict session 模式下亲和性不可用会直接返回 503（`service/channel_select.go:332-369`）。
3. **随机合格渠道**：前两者都未命中时进入 `CacheGetRandomSatisfiedChannel`（`service/channel_select.go:371-386`）。
4. **最终复核**：随机结果也要再过一次 `ChannelSatisfiesFilters`，防止缓存和实时过滤不一致（`service/channel_select.go:387-394`）。

亲和性缓存是 HybridCache，内存 LRU + 可选 Redis，命名空间 `new-api:channel_affinity:v1`（`service/channel_affinity.go:77-101`）。规则匹配支持模型正则、路径正则、User-Agent、请求头、gjson 请求体字段等 key 来源（`service/channel_affinity.go:550-632`）。命中亲和性会设置 `ginKeyChannelAffinityRetryOffset=1`，让后续重试从正常优先级层继续，而不是把亲和性当作一层优先级（`service/channel_affinity.go:683-728`）。

成功后的亲和性记录有两个时机：

- Distribute 中间件在响应状态码小于 400 时记录（`middleware/distributor.go:135-138`）；
- Responses WebSocket 在上游发出第一个 `response.*` 事件、即连接被接受后记录（`relay/responses_websocket.go:462-468`）。

### 2.4 auto 分组与跨分组重试

`CacheGetRandomSatisfiedChannel` 对 `tokenGroup == "auto"` 有专门逻辑（`service/channel_select.go:114-208`）：

- 先解析有序 auto 分组列表 `GetRequestAutoGroups`，没有则报“auto groups is not enabled”；
- 用 `ContextKeyAutoGroupIndex` 记录当前分组；
- 每个分组内部先耗尽优先级；
- 开启 token 级 `CrossGroupRetry` 时，当前分组重试次数耗尽后把 index 移到下一个分组，并重置重试计数；
- 未开启跨组重试时，auto 通常只在第一个可用分组内尝试。

相关测试在 `service/channel_select_auto_groups_test.go` 和 `service/group_auto_groups_test.go`，覆盖了跨组开启、分组过滤和权限变化后的行为。

---

## 3. 重试如何实现

### 3.1 HTTP/WS relay 主循环

`controller/relay.go:72-240` 的 `Relay` 是文本、Claude、Gemini、图像、音频、Responses 等大多数请求的主循环：

```go
for ; retryParam.GetRetry() <= common.RetryTimes; retryParam.IncreaseRetry() {
    channel, channelErr := getChannel(c, relayInfo, retryParam)
    ...
    service.AppendUsedChannel(c, channel.Id)
    ...
    newAPIError = relayHandler(c, relayInfo) // 或对应协议 helper
    ...
    decision := service.DecideRelayRetry(c, newAPIError, common.RetryTimes-retryParam.GetRetry())
    service.RecordPolicyFailure(...)
    processChannelError(...)
    if decision.Action != "retry" { break }
}
```

关键点：

- `common.RetryTimes` 默认为 0，`common/constants.go:143`；可通过配置调整。
- 每轮都会重置 `relayInfo.StreamStatus`、`PerformanceBusinessRejection`、`PerformanceOutputTokens`，并更新 `RetryIndex`（`controller/relay.go:177-181`）。
- `relayInfo.LastError` 保存上一轮错误，供参数覆盖模板使用（`controller/relay.go:174-224`；模板上下文见 `relay/common/override.go:2213-2234`）。
- `AppendUsedChannel` 把每次尝试的渠道 ID 追加到 `use_channel`，最终写入 admin log（`service/channel_select.go:408-412`；`service/log_info_generate.go:73`）。
- 请求体使用 `common.GetBodyStorage(c)` 的可重放存储，每轮重新包成 `io.NopCloser`，这是重试能重新读 body 的基础（`controller/relay.go:191-201`）。

### 3.2 首轮是否复用 Distribute 已选渠道

不会复用。`GenRelayInfo`/`genBaseRelayInfo` 不填充 `ChannelMeta`，而 `getChannel` 只有在 `info.ChannelMeta != nil` 时才走“已有渠道”分支；首轮 `ChannelMeta == nil`，所以会再次调用 `CacheGetRandomSatisfiedChannel`（`controller/relay.go:298-331`）。这意味着 Distribute 的选路结果主要提供上下文、亲和性和日志；真正的每次尝试选择都在 Relay 循环内发生。

### 3.3 重试决策

`DecideRelayRetry`（`service/relay_error.go:20-58`）的顺序是：

1. 错误为空 → stop；
2. strict session 亲和性失败 → stop，reason `strict_session`；
3. pin 的 retry mode 是 `PinRetrySingleAttempt` → stop，reason `pinned_channel`；
4. `types.IsChannelError` → retry，reason `channel_error`；
5. `types.IsSkipRetryError` → stop；
6. 重试预算耗尽 → stop，reason `attempt_budget_exhausted`；
7. 2xx → stop；
8. 状态码不在 100–599 → retry；
9. 全局强制跳过状态码/错误码 → stop；
10. 命中可重试状态码 → retry；
11. 否则 stop。

这个函数是单一出口，`ShouldRetryRelayError` 只是包装（`service/relay_error.go:60-62`）。相关策略事件会通过 `RequestPolicyState` 记录，并作为 `admin_info.request_policy` 写入日志（`service/request_policy.go:20-189`；`service/log_info_generate.go:94-96`）。

### 3.4 失败后的渠道处理

失败会调用 `ProcessChannelError`（`service/relay_error.go:64-107`）：

- 记录渠道错误日志；
- `ShouldDisableChannel` 判定是否需要自动禁用；
- 若渠道启用 `AutoBan`，异步调用 `DisableChannel`；
- 符合条件时写入独立的错误日志。

自动禁用开关是 `common.AutomaticDisableChannelEnabled`，渠道错误或命中禁用状态码/关键词时触发（`service/channel.go:60-83`）。

### 3.5 任务提交的重试

异步任务有独立但相似的循环：`controller/relay.go:489-622` 的 `executeTaskSubmissionWith`。区别是：

- 它支持 `relayInfo.LockedChannel`，例如 origin task remix 场景强制回到原渠道；
- 重试决策在 `decideTaskRetry`（`controller/relay.go:796-845`）；
- 只有任务真正持久化成功后才认为不可退款，之后才结算（`controller/relay.go:522-531,703-721`）。

---

## 4. 流式请求的首包边界

### 4.1 HTTP SSE

流式响应的写入从 `SetEventStreamHeaders` 开始：它设置 `Content-Type: text/event-stream`、`Transfer-Encoding: chunked` 等响应头，并替换 writer（`relay/helper/common.go:179-201`）。随后 `StreamScannerHandler` 启动扫描器，收到每个 `data:` 帧时调用 `info.SetFirstResponseTime()`，收到 `[DONE]` 时记录正常结束（`relay/helper/stream_scanner.go:300-327`）。

因此“首包”可以理解为：**第一个从上游解析出来的 SSE data 帧**。在这之前，连接尚未向客户端提交流式响应体；在这之后，HTTP 响应已经进入 chunked streaming，不能再换上游重放。

需要注意一个边界：如果启用了 stream keep-alive，`StartStreamKeepAlive` 可能在真正业务首包之前发送 `: PING\n\n` 注释帧（`relay/helper/common.go:88-135,251-260`）。这会向客户端写出字节，但不代表业务响应已开始；对“能否切换上游”的判断仍应以是否收到/写出第一个业务 data 帧为准。

### 4.2 HTTP 流式切换结论

- **首包前可以切换**：上游请求失败、非 200、连接失败等错误仍在 `adaptor.DoRequest` 或 `DoResponse` 早期返回；只要 `c.Writer` 尚未写出业务流，外层 `Relay` 循环就可以重试另一个渠道。
- **首包后不能安全切换**：一旦 `DoResponse` 已把上游 SSE 传给客户端，`Relay` 的错误处理会走 `writeRelayStreamError`，把错误作为同一流中的 error event 发出（`controller/relay.go:96-107,243-262`）。当前实现不会在首包后重新选渠道重放请求，因为客户端已经收到了第一个响应的字节，重放会造成重复输出或不一致流。
- **当前实现没有首包后自动故障转移**。`TextHelper` 在 `adaptor.DoResponse` 返回错误后才回到外层循环；如果错误发生在流中，writer 已写出，外层即使想 retry 也只能把错误写入同一个流。

### 4.3 Responses WebSocket

WebSocket 路径是显式例外，它的“首包”是上游第一个 `response.*` 事件：

- 选路循环在 `relay/responses_websocket.go:298-360`；
- 上游 WebSocket dial 成功、发送 create payload 后，会锁定模型、渠道、key、group 和连接上下文；
- 收到第一个事件时 `info.SetFirstResponseTime()`；
- 只有在上游发出 `response.*` 事件、即请求被接受后，才调用 `RecordChannelAffinity`（`relay/responses_websocket.go:375-468`）。

它的切换语义比 HTTP 更强：

- **首事件前可切换**：`DoWssRequest` dial 失败时会走 `DecideRelayRetry`，可以继续尝试下一个渠道。
- **首事件后连接锁定**：当前 `response.create` 的后续事件必须继续来自同一个上游连接；后续同连接上的新请求在 `restoreConnectionContext` 校验通过后仍可复用锁定连接。
- **控制事件可缓冲**：`response.cancel` 在上游接受前会先缓冲，接受后再发给上游（`relay/responses_websocket.go:484-504`）。

### 4.4 流式可观测性

- `RelayInfo.FirstResponseTime` 记录首响应时间，`HasSendResponse` 判断是否已经收到首响应（`relay/common/relay_info.go:1000-1008`）。
- `StreamStatus` 记录结束原因：done、timeout、client_gone、scanner_error、handler_stop、eof、panic、ping_fail（`relay/common/stream_status.go:8-19`）。
- 这些信息会写入日志的 `admin_info.stream_info`（`service/log_info_generate.go:136-164`）。

---

## 5. “先过滤、再评分”的插入点

### 5.1 推荐方案：在候选过滤后引入评分器

最佳插入点是 `model/channel_cache.go:117` 的 `GetRandomSatisfiedChannel`，以及数据库路径 `model/ability.go:108` 的 `GetChannel`。两者都已经具备：

- 候选渠道集合；
- 请求过滤器；
- 分组、模型、优先级、权重；
- retry 层概念。

建议的扩展形态：

```go
type ChannelScoreContext struct {
    Group       string
    Model       string
    Retry       int
    RequestPath string
    Filters     []dto.ChannelFilter
    Candidates  []*Channel
}

type ChannelScorer interface {
    Name() string
    Score(ctx *ChannelScoreContext, ch *Channel) (float64, error)
}
```

然后保留现有“优先级分层”，只在同一优先级层内用评分替代或增强随机加权：

- **保守模式**：仍按优先级选层，但层内按评分排序，分数相同再随机；
- **概率模式**：把评分转换成 Softmax 或加权随机，保留探索性；
- **分级模式**：先按健康度、成功率、延迟、成本等硬指标过滤，再按评分排序。

### 5.2 为什么不建议直接改 `SelectChannelForRequest` 的对外行为

`SelectChannelForRequest` 已经承担了 pin、亲和性、auto 分组、过滤器、错误归因和上下文装配。它适合作为评分结果的应用入口，但不适合把评分逻辑全部塞进去。更清晰的做法是：

1. `GetRandomSatisfiedChannel` 继续负责候选生成；
2. 新增评分器对候选排序或加权；
3. `SelectChannelForRequest` 只在最终选择后继续做过滤复核、pin、亲和性和日志。

这样可以保持测试兼容，也让“过滤”和“评分”分层清楚。

### 5.3 需要同时支持的路径

如果要全面替换随机选路，不能只改内存缓存路径，还要覆盖：

- 内存缓存路径：`model/channel_cache.go:117`；
- 数据库直查路径：`model/ability.go:108`；
- auto 分组路径：`service/channel_select.go:114`；
- Responses WebSocket：`relay/responses_websocket.go:938`，它复用了 `SelectChannelForRequest`，所以只要底层评分生效，这里也会受益。

### 5.4 过滤器扩展点

现有 `dto.ChannelFilterKind` 只有四种。若要做智能路由，建议扩展新的 filter kind，例如：

- `provider`：按上游 Publisher/Provider 过滤；
- `capability`：按工具调用、视觉、音频、长上下文等能力过滤；
- `latency_tier` / `quality_tier`：按实测或配置档位过滤；
- `cost_ceiling`：按渠道成本上限过滤；
- `health`：按最近错误率、熔断状态过滤。

实现时应同步扩展：

- `dto/channel_constraints.go` 的 `ChannelFilterKind` 与 `ChannelFilter`；
- `model/channel_constraint.go` 的 `filterEvalOrder` 与 `channelMatchesFilter`；
- 数据库路径 `model/ability.go` 的 `filterAbilitiesByConstraints`；
- 管理端 API 和前端展示。

### 5.5 评分所需的上下文

评分器不应只看渠道静态配置，最好能拿到请求级和运行级上下文：

- 请求级：模型、分组、路径、是否流式、期望能力、用户 token 限制；
- 渠道级：优先级、权重、成本、协议类型、多 key 状态、禁用状态；
- 运行级：最近错误率、平均延迟、成功率、限流状态、健康检查结果；
- 会话级：亲和性命中、上一轮失败原因、当前 retry index。

现有代码已经把这些信息放在 `gin.Context`、`RelayInfo`、`ChannelConstraints` 和 `RequestPolicyState` 中，评分器可以优先从这些对象读取，避免新增全局状态。

---

## 6. 流式切换的工程约束

如果未来要实现“流式请求在首包前后切换”，建议分成两个层次：

### 6.1 首包前切换

这是安全且推荐支持的场景。做法是确保所有可能失败的操作都发生在向客户端写出第一个业务帧之前：

- 上游连接建立；
- 请求体发送；
- 上游 HTTP 状态码确认；
- 上游第一个事件或第一个 SSE data 帧到达。

只要这些步骤失败，就可以回到外层重试循环换渠道。当前 HTTP SSE 已经基本符合这个模型；WebSocket 也明确支持 dial 失败重试。

### 6.2 首包后切换

这个场景风险很高，除非引入协议级缓冲或重放机制，否则不应默认启用：

- 客户端已经收到第一个业务帧，重放会产生重复内容；
- 不同上游的响应 ID、工具调用格式、usage 统计可能不一致；
- 计费可能重复；
- 亲和性、pin、预算控制都会被复杂化。

如果确实要做，建议采用“短暂缓冲再转发”的策略：上游首帧到达后不立即写给客户端，先完成健康判定；一旦开始转发，就不再切换。这个策略等价于把“首包前”的定义从“收到上游帧”前移到“向客户端写出第一帧”前。对 WebSocket 这类已接受请求，继续沿用当前“连接锁定”模型更稳妥。

---

## 7. 实施建议

1. **先做可插拔评分器**：不要一开始替换所有随机逻辑。新增 `ChannelScorer` 接口，默认实现保持现有优先级 + 权重随机行为。
2. **以 `GetRandomSatisfiedChannel` 为主入口**：先覆盖内存缓存路径，再同步数据库路径。
3. **保留优先级语义**：评分先作用于同优先级层内；跨优先级仍按 retry 层推进，避免破坏现有重试预期。
4. **保留过滤复核**：无论评分怎么算，最终渠道仍必须通过 `ChannelSatisfiesFilters`。
5. **记录决策日志**：把评分、候选列表摘要、最终选择原因写入 `RequestPolicyState`，继续进入 `admin_info.request_policy`。
6. **流式只承诺首包前切换**：明确“首包”为第一个上游业务帧或第一个客户端业务帧，并在文档中区分 keep-alive ping。
7. **补齐测试**：覆盖评分排序、评分并列随机、过滤器优先级、auto 分组跨组、WebSocket 锁定连接、首包前后错误路径。

---

## 8. 一句话结论

现有系统已经有清晰的“过滤 → 分组 → 优先级 → 权重随机 → 重试”链路；智能路由最好作为可插拔评分器插在候选过滤之后、最终选择之前，而流式切换只能可靠支持在第一个上游业务帧写给客户端之前发生。
