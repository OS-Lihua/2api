# 调研：现有计费链路能否按每次上游尝试记录 Route、实际用量与采购价版本

> 结论先行：**现有链路不能按“每次上游尝试”记录完整的 Route、实际用量和采购价版本。**
> 现状是“一次请求一份消费账”：预扣费和定价快照在重试循环开始前只建立一次，最终只落一条消费日志（记录最终成功渠道的用量与销售侧计费）。每次上游尝试只有管理端诊断事件（`use_channel` 链路、`request_policy` 事件、失败 error log），这些诊断不承载计费数量，也没有“Route”与“采购价版本”字段。

---

## 一、现有计费链路：一次请求的生命周期

### 1. 预扣费与定价快照是“请求级”，不是“尝试级”

- 文本/多模态请求在进入重试循环**之前**执行一次 `relay.PrepareRequestBilling`，完成用量预估与预扣费（`controller/relay.go:160`）。
- 重试循环在其后才开始：`for ; retryParam.GetRetry() <= common.RetryTimes; ...`（`controller/relay.go:177`）。
- `relay/request_billing.go:23-26` 的注释明确说明：*“channel retries retain the resulting billing session and pricing snapshot”*——即渠道重试会**保留**同一个计费会话与定价快照，不会为每次尝试重新定价或重新预扣。

因此从计费会话的角度看，一次客户端请求自始至终只有**一个** BillingSession、**一份**定价快照。

### 2. 每次上游尝试记录了什么

每次尝试会经历：选渠道 → 记录渠道链路 → 准备分组计费 → 发起上游请求 → 失败则记录策略事件并决定是否重试。

- `service.AppendUsedChannel(c, channel.Id)` 把每次选中的渠道追加进 `use_channel` 轨迹（`controller/relay.go:185` 附近调用）。
- 失败时 `service.RecordPolicyFailure` 写入一条 `PolicyEvent`（`controller/relay.go:226`，`service/request_policy.go:100-127`），字段包括：
  - `attempt`（第几次尝试）
  - `channel_id`（渠道 ID）
  - `group`（分组）
  - `rule`（命中的路由规则名）
  - `status`（HTTP 状态码）
  - `error_code` / `error_source`（错误码与来源：upstream/channel/transport/local）
  - `elapsed_ms`（该次尝试耗时）
  - `decision`（重试决策：action/reason/source）
  - `health`（渠道健康动作，如 `channel_disable_requested`）
  （结构定义见 `service/request_policy.go:25-36`。）

**关键点**：这些事件记录的是“这次尝试是否失败、要不要重试、渠道健康度”，**不包含**本次尝试的 token 用量，也不包含价格或成本。

### 3. 结算与落账：最终只有一条消费日志

- 成功后按**最终**实际 usage 结算：`BillingSession.Settle(actualQuota)` 与预扣额做差额多退少补（`service/billing_session.go:27-60`，`Settle` 在 44 行起）。
- 落一条 `LogTypeConsume` 消费日志（`model/log.go:324-375` `RecordConsumeLog`），核心列见 `model/log.go:61-91` 的 `Log` 结构：

| 字段 | 含义 |
|---|---|
| `user_id` / `username` | 用户 |
| `token_name` / `token_id` | 令牌 |
| `model_name` | 模型名 |
| `quota` | 本次扣减的内部额度 |
| `prompt_tokens` / `completion_tokens` | 输入/输出 token |
| `use_time` | 耗时 |
| `is_stream` | 是否流式 |
| `channel_id`（json: `channel`） | **最终成功**的渠道 ID |
| `group` | 分组 |
| `request_id` | 网关请求 ID |
| `upstream_request_id` | 上游请求 ID |
| `other` | JSON 扩展信息（详见第四节） |

**结论**：日志粒度是“请求”；“尝试”粒度只存在于诊断事件里，二者没有用量的关联落账。

---

## 二、“Route”概念现状：没有一等 Route 对象

当前代码里没有“Route（Channel × 上游协议 × 上游模型 × 定价版本）”这样的稳定实体。最接近的几个日志字段：

| 字段 | 内容 | 来源 |
|---|---|---|
| `admin_info.use_channel` | 重试链路上的渠道 ID 列表，前端以 `id → id` 展示 | `web/src/features/usage-logs/types.ts:122`；`common-logs-columns.tsx:411-417` |
| `admin_info.request_policy[]` | 每次尝试的 `PolicyEvent`（见上） | `service/request_policy.go:25-36` |
| `request_conversion` | 请求协议转换链，如 `OpenAI Compatible → Claude Messages` | `service/log_info_generate.go:236-262` |
| `admin_info.channel_affinity` | 会话粘性命中渠道/密钥的指纹 | `web/src/features/usage-logs/types.ts:86-97`；`service/channel_affinity.go:711-726` |
| `admin_info.usage_billing_path` | 本次计费用量取自哪条路径（本地估算 / 上游 / 按协议） | `service/billing_usage.go:14-29, 56-62` |

这些字段合起来能“还原”一次请求走了哪些渠道、换了哪些协议，但**不是一个可寻址的 Route 标识**，也没有把“该 Route 对应的采购价”挂上去。

---

## 三、实际用量记录现状

### 1. 最终用量有，按尝试用量没有

消费日志的 `other` JSON 里保存了较完整的**最终**用量明细（前端类型见 `web/src/features/usage-logs/types.ts:171-201`）：

- `text_input` / `text_output`：文本输入/输出 token
- `cache_tokens`：缓存读取 token
- `cache_creation_tokens_5m` / `_1h` / `_12h`：缓存写入 token（按时长分档）
- `billing_tokens`：按计费类目拆分的 token
- `image_cache_tokens`、`audio_input` / `audio_output`、`image_output` 等：多模态用量
- `usage_facts`：任务插件计费的实际用量事实（`types.ts:229`）

### 2. 用量来源路径被显式记录

`admin_info.usage_billing_path` 标明本次计费用量来自哪里（`service/billing_usage.go:14-29`）：

- `local`：本地 tokenizer 估算
- `upstream`：上游通用返回
- `billing-usage-openai` / `billing-usage-openai-estimated`
- `billing-usage-anthropic` / `billing-usage-anthropic-estimated`
- `billing-usage-gemini` / `billing-usage-gemini-estimated`

这说明网关已经区分“计费用量”与“响应展示用量”，并记录了取数路径——这是按尝试记账的有利基础，但目前只在最终日志里落一份。

### 3. 失败尝试的用量去向

失败尝试会写 error log（`LogType=5`），但 error log 不承载 token 用量与金额。也就是说：**如果某次尝试产生了上游用量但最终失败重试，这部分用量目前不会作为独立成本记录下来。**

---

## 四、模型倍率 / 补全倍率 / 分组倍率 / 计费表达式 与“币种 + 计费项 + 单位 + 单价”的对应关系

### 1. 传统倍率模型

`Pricing` 结构（`model/pricing.go:27-52`）集中了销售侧定价字段：

| 字段 | 含义 | 对应“计费项+单位+单价” |
|---|---|---|
| `model_ratio` | 模型倍率（输入侧基准倍率） | 计费项=输入 token，单位=1M token，单价=倍率 × 基准价 |
| `completion_ratio` | 补全倍率 | 计费项=输出 token，单位=1M token |
| `model_price` | 按次价格 | 计费项=次数，单位=次 |
| `cache_ratio` / `create_cache_ratio` | 缓存读/写倍率 | 计费项=缓存 token |
| `image_ratio` / `audio_ratio` / `audio_completion_ratio` | 图片/音频倍率 | 计费项=图片/音频 token |
| `billing_mode` / `billing_expr` | 计费模式 / 计费表达式 | 见下 |
| `pricing_version` | 价格版本号（见第五节） | — |

### 2. 计费表达式（新式计费）

当 `billing_mode = tiered_expr` 时，使用计费表达式定价。表达式变量来自 `service/tiered_settle.go:32-120` `BuildTieredTokenParams`：

- `p`：输入 token
- `c`：输出 token
- `cr`：缓存读取
- `cc` / `cc1h` / `cc12h`：缓存写入（5 分钟 / 1 小时 / 12 小时档）
- `img` / `img_cr` / `img_o`：图片输入 / 图片缓存读 / 图片输出
- `ai` / `ao`：音频输入 / 音频输出
- `len`：参与分档判定的上下文长度（Claude 语义下 = p+cr+cc+cc1h）

### 3. 币种、单位与换算

- 表达式系数的语义是 **USD / 1M token**（v1 版表达式）。
- 结算时通过 `quotaConversion` 换算成内部额度：`exprOutput / 1_000_000 * QuotaPerUnit`（`pkg/billingexpr/settle.go:11-21`）。
- `QuotaPerUnit = 500000`，即 **$0.002 / 1K tokens**（`common/constants.go:22`）。
- 也就是说：**“币种”在计费内核里固定为 USD 表达式 → 内部额度**；展示层再按站点配置把额度格式化为 USD/CNY/TOKENS（`web/src/lib/currency.ts:500-537` `formatQuotaWithCurrency`）。
- 分组倍率在表达式结果之上再乘：`afterGroup = quotaBeforeGroup * GroupRatio`（`pkg/billingexpr/settle.go:34-36`）。

### 4. 任务插件的“计费项+单位”显式化

任务类插件的 `usageSchema` 字段带有 `type / unit / unitLabel / description`（`pkg/jsplugin/registry.go:207-214`），是当前最接近“计费项 + 单位”显式声明的地方；但**单价**仍在价格/倍率侧，插件只声明用量维度。

---

## 五、采购价版本现状

**当前没有采购价（进价）与 Route 级成本版本字段。**

- `Pricing` 的字段全部是销售侧（倍率/按次价/表达式），没有 cost/purchase 类字段。
- `Channel` 结构（`model/channel.go:24-55`）也没有渠道进价、成本价字段。
- 现有的几个“版本”都不是采购价版本：
  - `Pricing.PricingVersion`（`model/pricing.go:50`）：价格接口的缓存版本串，刷新时被赋成固定值（`model/pricing.go:433`）。
  - `controller/pricing.go:75` 的 `pricing_version`：接口级常量。
  - `model.ModelPricingVersion`（`model/model_pricing_config.go:78`）：对配置值做 SHA-256 哈希，用于变更检测。
- 任务计费有“提交时价格快照”：`TaskBillingContext` 保存 `model_price / group_ratio / model_ratio / other_ratios / origin_model_name / tiered_snapshot`（`model/task.go:161-172`），任务轮询阶段用它重算并多退少补（`service/task_polling.go:686-734`）。但这份快照仍是“模型 + 分组”维度的**销售价**快照，不是 Route 采购成本。

---

## 六、若要支持“按每次上游尝试记录 Route + 实际用量 + 采购价版本”，差距清单

1. **尝试级记录**：为每次上游尝试建立独立记录（`request_id` + `attempt` 序号），含 `channel_id`、上游协议/端点、上游模型名、开始/结束时间、状态。
2. **尝试级用量**：从每次响应/流中提取 usage 并落到该尝试记录；失败但有部分用量的尝试也要能记。
3. **Route 标识**：定义 `channel × protocol × upstream_model × pricing_version` 的稳定 route key，并在选路时写入。
4. **采购价与版本**：新增 cost 侧价格模型（进价）与版本管理，独立于现有销售倍率。
5. **结算口径**：明确失败尝试是否计费、重试总成本如何归属；当前“一次请求一条账”的模型需要扩展为“一次请求 + N 条尝试明细”。

---

## 结论（一句话）

现有计费链路是“一次请求一份消费账”：预扣费与定价快照在重试前只建立一次（`controller/relay.go:160,177`；`relay/request_billing.go:23-26`），最终只按成功渠道落一条消费日志（`model/log.go:61-91,324-375`）；每次上游尝试仅有 `use_channel` / `request_policy` 诊断事件（`service/request_policy.go:25-36`），**没有 Route 实体、没有按尝试的用量账、也没有采购价与采购价版本字段**；现有模型倍率、补全倍率、分组倍率与计费表达式共同构成销售侧“计费项 + 单位 + 单价”，以 USD/1M token 表达并按 `QuotaPerUnit=500000` 换算为内部额度（`pkg/billingexpr/settle.go:11-21`；`common/constants.go:22`）。
