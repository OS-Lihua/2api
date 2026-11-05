# Key 模型权限与模型列表调研（issue #6）

## 结论

1. **Key 的模型权限由 `Token.ModelLimitsEnabled + Token.ModelLimits` 实现，是纯“黑名单反转”式的附加限制，不是独立授权体系。** 字段定义在 `model/token.go:25-26`；`ModelLimits` 是逗号分隔的模型名列表，`GetModelLimitsMap()` 转成 `map[string]bool`（`model/token.go:341-355`）。开启后，只有出现在该列表里的模型可用；关闭时该 Key 继承所属用户/分组的全部可用模型。

2. **用户权限的真正载体是“用户分组 → abilities 表 → 渠道”。** `TokenAuth` 中先取 `userCache.Group`；若 Key 设置了 `Group`，必须先通过 `service.GetUserUsableGroups(userGroup)` 校验，且该分组必须存在于 `ratio_setting.GroupRatio`，否则返回 403（`middleware/auth.go:623-638`）。最终生效分组写入 `ContextKeyUsingGroup`。也就是说，Key 只能在“所属用户可用分组”的子集里选择，不能越过用户权限。

3. **`abilities` 是路由与模型可见性的最小单位：`(group, model, channel_id)` 三元组，附带 enabled/priority/weight/tag。** 定义见 `model/ability.go:18-26`。渠道保存时会按 `channel.Models × channel.Group` 生成笛卡尔积写入（`model/ability.go:225-265`）；渠道更新时先删后插（`model/ability.go:270-345`）。

4. **实际调用时，`Distribute` 中间件做两层检查：先查 Token 模型限制，再按 `(usingGroup, model)` 选渠道。** Token 限制检查在 `middleware/distributor.go:57-74`；匹配规则由 `TokenModelLimitAllows` 实现，精确名、格式化名、路由归一化名均可通过（`middleware/distributor.go:551-563`）。渠道选择最终落到 `service.SelectChannelForRequest` → `CacheGetRandomSatisfiedChannel` / `model.GetChannel`，查询条件都是 `group = ? AND model = ? AND enabled = true`（`service/channel_select.go:303-382`、`model/ability.go:92-176`、`model/channel_cache.go:126-135`）。

5. **`/v1/models` 是“用户/Key 可见模型列表”，不是全量模型表。** 它按 Key 生效分组取 `abilities` 中的 distinct model（`service/group.go:111-122`、`model/ability.go:44-50`），再依次过滤 Token 模型限制（`controller/model.go:240-257`）和计费配置 `HasModelBillingConfig`（`controller/model.go:258-261`，实现 `relay/helper/price.go:281-293`）。分组解析规则：Key 指定普通分组时用该分组；`auto` 时用用户分组 + Token AutoGroups 快照（`controller/model.go:188-216`）。

6. **`/v1/models/:model` 单模型查询目前只查内置静态表，不做 abilities / Token 限制校验。** `RetrieveModel` 仅查 `openAIModelsMap`，查不到返回 `model_not_found`（`controller/model.go:348-375`）；该静态表来自各 adaptor 的 `GetModelList()`，在 `controller/model.go:35-97` 初始化。因此“列出来的不一定可调，可调的也不一定列出”（例如自定义模型不在静态表时，单查接口会误报不存在）。

7. **新增模型后，现有 Key 是否可见取决于三个条件：Key 未启用模型限制、Key 生效分组的某渠道包含该模型、该模型有计费配置（除非自用模式/用户设置放行）。** 渠道模型列表变更会同步重写 abilities，并在管理端操作后立即 `InitChannelCache()` 重建内存路由缓存（`controller/channel.go:1263`）；后台还有 `SYNC_FREQUENCY`（默认 60 秒）的兜底同步（`main.go:102-105`、`common/init.go:112`）。所以新增模型对未启用限制的现有 Key 是自动生效的，无需逐 Key 授权。

8. **上游自动发现的新模型会合并进渠道 `Models` 并更新 abilities，但不会自动进入销售价/内置价体系。** 自动同步逻辑见 `controller/channel_upstream_update.go:537-569` 与 `controller/channel_upstream_update.go:1010-1025`。由于 `/v1/models` 默认过滤无计费配置模型，这些模型在配置价格前不会对普通用户展示；这是“上游有模型”和“网关正式售卖该模型”之间的 intentionally conservative 门禁。

## 细节与边界

### 1. Key 限制的存储与缓存

- `Token.ModelLimitsEnabled bool`、`Token.ModelLimits string \`gorm:"type:text"\``（`model/token.go:25-26`）。
- 创建/更新 Key 时由用户直接提交这两个字段，服务端未按用户可用模型做交集校验（`controller/token.go:359-360`、`controller/token.go:456-457`）。也就是说，若用户把无权限模型写进 Key 限制，调用时仍会被分组渠道选择挡住；该字段更像“进一步收窄”，不是提权入口。
- Redis 开启时，Token 快照会缓存 `ModelLimitsEnabled` 和 `ModelLimits`（`model/token_cache.go:75-88`）；更新 Token 前会先失效缓存并设置 mutation fence（`model/token.go:309-332`）。

### 2. 分组与 AutoGroups

- Key 分组为 `auto` 时，`Token.AutoGroups` 保存用户可选择的分组快照；写入前会用 `service.IsUserSelectableGroup(userGroup, group)` 校验（`controller/token.go:92-129`）。
- 请求时通过 `GetRequestAutoGroups` 解析：上下文无值则回退到用户可用 Auto 分组，有值也仍会再经过用户权限过滤（`service/group.go:87-109`）。
- 跨分组重试仅在 `auto` 分组且 `CrossGroupRetry` 开启时生效，按分组顺序耗尽优先级后切换（`service/channel_select.go:114-195`）。

### 3. 模型名匹配的宽松点

- Token 限制和渠道选择都支持“路由归一化名”兜底：`ratio_setting.RoutingMatchModelName`。这会剥掉 `@` 修饰符和旧别名后匹配（`middleware/distributor.go:551-563`、`model/ability.go:120-128`）。
- 这提高了兼容性，但也意味着：如果 Token 限制写了基础名，带修饰符的变体可能同样通过；反之，精确授权某个变体时需要理解归一化规则。

### 4. 已知不一致 / 可改进点

- **`/v1/models/:model` 与列表、调用路径不一致**：单模型查询只看静态 `openAIModelsMap`，不看 abilities、Token 限制或分组。自定义模型即使可调用，也会返回 `model_not_found`（`controller/model.go:348-375`）。
- **管理端 `/api/models` 与中继端模型接口语义不同**：`/api/models` 返回渠道类型到静态模型列表的映射（`router/api-router.go:30`、`controller/model.go:325-339`）；`/api/channel/models`、`/api/channel/models_enabled` 是管理员渠道视角接口（`router/channel-router.go:62-64`），均不代表某个用户 Key 的可用集合。
- **渠道创建后无显式缓存刷新**：`AddChannel` 路径没有立即调用 `InitChannelCache()`（`controller/channel.go:740-879`），新渠道模型通常要等 60 秒兜底同步或下次管理操作；若希望管理操作即时生效，这里是一个可补齐点。
- **Key 模型限制不与用户可用模型自动同步**：用户分组变化后，已存在的 Key 限制列表不会自动增删；权限最终仍以“分组 + abilities”为准，Key 列表只做收窄。

## 对二开的建议

- 若要做更清晰的“Key 模型权限”，建议把语义定为：**用户分组决定可选模型全集，Key 限制只能收窄，不能扩大**；管理端应展示交集，而不是裸列表。
- Route 化后，模型可见性建议从 `abilities(group, model, channel_id)` 演进为以 Route/Model 为中心的能力表，避免 `Channel.Models × Channel.Group` 笛卡尔积继续膨胀。
- `/v1/models` 与 `/v1/models/:model` 应统一到同一套可见性/可调用性判定，避免单模型查询误报。
- 上游自动新增模型建议保留“无定价不上架”的默认门禁，同时给管理员一个明确的待上架列表。
