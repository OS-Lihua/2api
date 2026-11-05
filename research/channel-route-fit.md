# 现有 Channel/Ability 能否承载 Route（渠道 × 协议 × 单模型）

## 结论（先行）

**能承载"按单模型的路由候选与调度"，不能承载"按单模型的协议与采购价"。** 现有 `Channel` + `abilities` 表已经把"某个渠道在某个分组下能服务哪些模型、以什么优先级和权重参与调度"拆到了 `(group, model, channel_id)` 粒度，这部分可以直接复用为 Route 的骨架；但协议（协议转换器）绑定在渠道级 `Channel.Type` 上，采购价/倍率是全局按模型名定价的，二者都没有下沉到单个模型。若要承载 Route，需要把 `abilities` 升级（或另建 Route 表）为 `(channel_id, model, protocol)` 粒度，并为其增加协议、能力、采购价、模型映射与健康状态字段。

---

## 1. 现状：abilities 已经是"按单模型"的路由索引，但只覆盖调度属性

`Ability` 表以 `(group, model, channel_id)` 为复合主键，附带 `Enabled / Priority / Weight / Tag` 四个调度字段：

> model/ability.go:18-27

```go
type Ability struct {
    Group     string  `json:"group" gorm:"type:varchar(64);primaryKey;autoIncrement:false"`
    Model     string  `json:"model" gorm:"type:varchar(255);primaryKey;autoIncrement:false"`
    ChannelId int     `json:"channel_id" gorm:"primaryKey;autoIncrement:false;index"`
    Enabled   bool    `json:"enabled"`
    Priority  *int64  `json:"priority" gorm:"bigint;default:0;index"`
    Weight    uint    `json:"weight" gorm:"default:0;index"`
    Tag       *string `json:"tag" gorm:"index"`
}
```

它的生成方式是 `Channel.Models × Channel.Group` 的笛卡尔积，并把**渠道级**的 `Status / Priority / Weight / Tag` 复制到每一行（model/ability.go:225-270 `AddAbilities`、model/ability.go:272-339 `UpdateAbilities`）。

**可复用**：`(group, model, channel_id)` 这个粒度正是 Route 需要的"单渠道 × 单上游模型 × 下游分组"候选集；`Enabled / Priority / Weight / Tag` 已按此粒度存在，可直接沿用。

**缺口**：abilities 里没有协议、能力、价格、上游模型身份、健康状态；这些要么在渠道级，要么在全局定价里。

## 2. 协议绑定在 `Channel.Type`，无法按单模型表达

协议由 `Channel.Type`（整数渠道类型）决定，选路后用同一类型查找协议转换器：

- 渠道类型常量：constant/channel.go:5-40（如 `ChannelTypeOpenAI=1`、`ChannelTypeAnthropic=14`、`ChannelTypeGemini=24`）；
- 转换器按 APIType 分发：relay/relay_adaptor.go:50-60 `GetAdaptor`；
- 转换器接口（ConvertOpenAIRequest / ConvertClaudeRequest / ConvertGeminiRequest 等）定义在 relay/channel/adapter.go:18-35。

也就是说：**一个渠道上的所有模型共享同一个协议栈**。一个 OpenAI 兼容渠道里挂了一个只有 Gemini 原生协议可用的模型，现有模型无法表达"这个模型走另一种协议"。能力端点也是按"渠道类型 → 端点类型"推断的（model/pricing.go:122-127 `getPricingEndpointTypesForAbility` → `common.GetEndpointTypesByChannelType`），不是按模型声明。

**Route 语义缺口**：Route 的"协议"半边目前没有落点；abilities 或新 Route 表需要增加 `protocol`（或可复用 `endpoint_type`）维度。

## 3. 采购价是全局按模型名定价，没有按渠道/线路的价格

定价数据与渠道无关，按**模型名**存放在全局映射中：

- `Pricing` 结构按 `ModelName` 聚合 `ModelRatio / ModelPrice / CompletionRatio / CacheRatio / ImageRatio / AudioRatio / ... / BillingMode / BillingExpr`：model/pricing.go:38-67；
- 取值入口 `ratio_setting.GetModelRatio`（setting/ratio_setting/model_ratio.go:385-393）、`GetModelPrice`（360-370）、`GetCompletionRatio`（436-445）；
- 计费时组装为 `PriceData`（types/price_data.go:33-51），分组倍率并入 `GroupRatioInfo`；
- 计费入口 `ModelPriceHelper`（relay/helper/price.go:65-75）按 `BillingModelName` 查价。

渠道侧只有汇总量：`Balance`（美元余额，channel.go:37）、`UsedQuota`（channel.go:42），都是渠道级累计，不是"这条线路每个模型多少钱"。

**结论**：现有体系能表达"某个模型对下游卖多少钱"，不能表达"某个渠道采购这个模型的成本价"。Route 若要承载采购价，需要新增按 `(channel_id, model)`（或 Route ID）的价格字段/价格表，并保持与现有全局定价的兼容与优先级关系。

## 4. 五个字段在 Route 语义下的含义

### 4.1 `ModelMapping` —— 模型身份转换表（下游名 → 上游名）

`ModelMapping *string` 是渠道级 JSON 映射（model/channel.go:43，读取于 channel.go:542-548）。请求期由 `ModelMappedHelper` 消费（relay/helper/model_mapped.go:14-66）：支持链式映射与环检测，把 `OriginModelName` 翻译成 `UpstreamModelName`；计费名 `BillingModelName` 与路由名分离（relay/common/relay_info.go:103-106）。

**Route 语义**：这是 Route 的"模型身份"半边——同一条物理渠道里，下游叫什么名字、上游真实叫什么。它目前挂在渠道上、按请求生效；Route 化时应下沉为 Route 级字段（每条线路自己的身份映射），并保留链式映射/环检测语义。

### 4.2 `Models` —— 渠道可服务的上游模型清单（Route 候选生成器）

`Models string` 是逗号分隔的模型名列表（channel.go:40，解析于 channel.go:302-308 `GetModels`）。它与 `Group` 叉乘生成 abilities（model/ability.go:225-270）。

**Route 语义**：`Models` 决定"这条渠道能提供哪些上游模型"，即 Route 的上游模型枚举。Route 化后它应从"渠道的模型列表"变成"每条 Route 的上游模型标识"（或直接由 Route 行本身携带 model）。

### 4.3 `Group` —— 下游授权/可见域 + 定价倍率维度 + 路由键

`Group string` 是逗号分隔分组（channel.go:41，解析于 channel.go:309-318）。它同时承担三重身份：

1. **授权与可见性**：令牌分组校验（middleware/auth.go:622-637）；
2. **定价倍率**：`defaultGroupRatio`、`GroupGroupRatio`（setting/ratio_setting/group_ratio.go:13-31），并入 `PriceData.GroupRatioInfo`；
3. **路由键**：abilities 的主键成分（model/ability.go:19-20）。

**Route 语义**：`Group` 表示"这条线路对哪些下游分组开放"。注意它已经把"访问域"和"价格域"耦合在一起；Route 设计若要求两者独立，需要拆分或显式建模。

### 4.4 `Priority` —— 同一 (group, model) 下的容灾/优选层级

`Priority *int64` 在渠道与 abilities 上各有一份（channel.go:46、ability.go:24），数值越大越优先。选路时按优先级分层，重试 `retry` 次数作为"优先级层索引"逐层下降：

- DB 路径：model/ability.go:63-91 `getPriority`、model/ability.go:108-159 `GetChannel`；
- 内存缓存路径：model/channel_cache.go:117-205 `GetRandomSatisfiedChannel`。

**Route 语义**：`Priority` 是线路的 failover 层级（也天然承载"贵/快线路优先、便宜/慢线路兜底"的业务分层）。它已经是按 `(group, model)` 粒度生效的，可直接复用为 Route 的容灾顺序。

### 4.5 `Weight` —— 同一优先级层内的负载均衡权重

`Weight *uint` 同样双份存在（channel.go:32、ability.go:25）。同一优先级层内按权重随机选择，DB 路径使用 `weight + 10` 平滑（ability.go:147-159），缓存路径使用平滑因子（channel_cache.go:189-205）。

**Route 语义**：`Weight` 是"同一层级内多条等价线路的流量切分比例"。可直接复用为 Route 的层内权重。

## 5. 其余可复用项与缺口清单

### 5.1 可复用

| 现有设施 | 位置 | 对 Route 的价值 |
| --- | --- | --- |
| `(group, model, channel_id)` 主键 | model/ability.go:19-22 | 就是"单渠道 × 单上游模型 × 下游分组"的候选集粒度 |
| `Enabled / Priority / Weight / Tag`（abilities 级） | model/ability.go:23-26 | 线路开关、容灾层级、层内权重、标签 |
| `ModelMapping` 链式映射与环检测 | relay/helper/model_mapped.go:14-66 | Route 的模型身份翻译，语义已完备，需下沉粒度 |
| 约束过滤器框架 `ChannelSatisfiesFilters` | model/channel_constraint.go:20-40 | 已支持 request_path / task_plugin_endpoint / task_plugin_identity / responses_websocket，可扩展为 Route 约束 |
| 双路径选路（DB + 内存缓存） | model/ability.go:108-159、model/channel_cache.go:117-205 | Route 查询可沿用同一模式 |
| 多 Key 状态 | channel.go:73-82 `ChannelInfo`、channel.go:188-297 | 线路内的凭据池与逐 Key 熔断 |
| 模型元数据 / Vendor | model/model_meta.go:35-63、model/vendor_meta.go:15-32 | 模型目录与 Publisher 概念已有落点（注意：这是元数据，不是采购价） |

### 5.2 缺口

1. **协议粒度**：协议在 `Channel.Type`，所有模型共享；Route 需要 `(channel, model, protocol)` 粒度。见第 2 节。
2. **采购价**：定价全局按模型名；无渠道级/线路级成本价。见第 3 节。
3. **能力声明**：端点能力由渠道类型推断（model/pricing.go:122-127），没有"这个模型在这条线路上支持哪些端点/能力"的显式声明。
4. **模型身份**：`ModelMapping` 挂在渠道上；Route 需要线路级上游模型名。
5. **健康与状态**：`Status` 是渠道级（含多 Key 状态）；没有按线路的独立熔断/健康度。
6. **访问域与价格域耦合**：`Group` 同时是路由键和定价倍率维度，若 Route 要求两者独立需拆分。
7. **迁移与兼容**：abilities 已是事实上的路由表；Route 化应优先扩展该表或建立由 `(channel_id, model, protocol)` 键控的新表，并同时兼容 SQLite / MySQL / PostgreSQL。

## 6. 总体判断

现有结构的"路由半边"（谁、在哪个分组、以什么优先级/权重、服务哪个模型）已经具备单模型粒度，可以直接演化为 Route；"协议半边"和"价格半边"仍停留在渠道级与全局模型级。因此最短路径不是推翻 Channel/Ability，而是：

1. 把 `abilities` 明确升级为 Route 表（或并列新增 Route 表），主键扩展为 `(channel_id, model, protocol)`；
2. 为 Route 增加协议、能力、上游模型身份、采购价与健康状态；
3. 保留 `Priority / Weight / Enabled / Tag` 的现有语义作为调度属性；
4. 让 `ModelMapping`、定价查询、约束过滤在新粒度上工作，并保持与现有渠道配置的双轨兼容与平滑迁移。
