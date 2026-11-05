# 下游→上游协议转换能力/损失矩阵

**研究对象**：本仓库 `relaykit/` 模块在 OpenAI Chat Completions、OpenAI Responses、Anthropic Messages、Gemini `generateContent` 四种协议之间的请求/响应转换能力，重点回答：哪些参数会被**静默丢弃或改写**（工具调用、推理、缓存、多模态、结构化输出）。

**方法**：以代码为唯一事实来源，逐条给出 `文件:行号` 引用；矩阵等级沿用 `relaykit/README.md:11-18` 的官方自评。

---

## 结论（先读这里）

1. **四种协议两两之间都能转换，但质量分三档**：Chat↔Responses 最好；Claude/Gemini 直连 Chat/Responses 次之；**Claude↔Gemini 两个方向都最差**，因为请求和响应都要经 OpenAI Chat 中转两跳（`relaykit/README.md:13-18`）。

2. **默认策略下，损失是"静默"的**：转换照常完成，损失细节只写进诊断列表；渠道不配置 `tool_loss_policy` 时，这些诊断只进管理端日志，不会拦截请求也不会返回给客户端。只有显式配置 `safe`/`strict` 才会拒绝（`relaykit/types/conversion.go:61-68`，`relaykit/dto/channel_settings.go:135-144`）。

3. **最容易被静默改写的三类参数**：① 托管工具与 `tool_choice` 的复杂策略（如 Claude 的 `blocked_domains`、`max_uses`、caller 约束，Gemini 的原生工具选择）；② 采样/推理控制（Claude 严格采样模型会剥离 `temperature/top_p/top_k`，Gemini 会把推理档位归一到目标模型支持档位）；③ 结构化输出约束（JSON Schema 会被清洗成 Gemini 支持的关键字白名单）。

4. **缓存能力跨协议基本不保**：只有 Claude `cache_control`→OpenAI Chat 多模态块、OpenAI `prompt_cache_key` 等少数"搭便车"路径；Claude→Gemini、Claude→Responses 等主流路径没有等价物，缓存提示会被丢弃。

5. **多模态受目标协议 MIME 能力限制**：Gemini→Chat 只接受 PNG/JPEG/WebP/GIF 图片和 WAV/MP3 音频，视频等多余类型报 `unsupported_media_type` 后跳过；Claude 文档到 Chat 只支持 base64 PDF 和纯文本。

6. **Responses 的有状态字段是硬错误不是静默丢失**：`conversation`、`previous_response_id`、`prompt`、`context_management` 在降级到 Chat/Claude/Gemini 时会直接报错，不会带着错误假设继续转发（`relaykit/relayconvert/internal/oai_responses/to_oai_chat_req.go:121-141`）。

---

## 一、总体能力矩阵

下表来自 `relaykit/README.md:11-18`，覆盖请求、非流式响应和流式响应同一张表。

| 源格式 \ 目标格式 | OpenAI Chat | OpenAI Responses | Claude Messages | Gemini |
|---|---:|---:|---:|---:|
| OpenAI Chat | — | `good` | `fair` | `fair` |
| OpenAI Responses | `good` | — | `fair` | `fair` |
| Claude Messages | `fair` | `fair` | — | `discouraged` |
| Gemini | `fair` | `fair` | `discouraged` | — |

**等级含义**（`relaykit/README.md:18`）：

- `good`：两种协议结构接近，字段基本一一对应。
- `fair`：主要内容能转，协议独有字段以诊断报告。
- `discouraged`：需要经 OpenAI Chat 中转两跳，丢字段机会更多。

**实际路由**（`relaykit/README.md:18`）：

- Gemini→Responses（请求和响应）、Claude→Responses（响应）、Responses→Gemini（响应）经 OpenAI Chat 中转。
- **Claude↔Gemini 请求和响应都经 OpenAI Chat 中转两跳**，因此是损失最大的路径。
- 实际路径记录在转换结果 `Steps`，等级记录在 `Quality`。

---

## 二、转换入口与"静默"机制

### 2.1 转换入口

- OpenAI 兼容入口：`relay/compatible_handler.go:112` 调 `adaptor.ConvertOpenAIRequest`。
- Claude 入口：`relay/claude_handler.go:96` 调 `adaptor.ConvertClaudeRequest`。
- Gemini 入口：`relay/gemini_handler.go:100` 调 `adaptor.ConvertGeminiRequest`。
- Responses 入口：`relay/responses_request.go:61` 与 `relay/chat_completions_via_responses.go:125` 调 `adaptor.ConvertOpenAIResponsesRequest`。
- 各适配器最终走 `service.ConvertRequest` / `ConvertRequestByID` / `ConvertRequestVia`（`service/request_converter.go:26-48`），底层在 `relaykit/relayconvert/request_registry.go`。

### 2.2 为什么默认是"静默丢失"

- 诊断严重级只有 `warning` 和 `error` 两类，没有 `info`（`relaykit/types/conversion.go:11-12`）。
- `RejectConversionLoss` 的行为（`relaykit/types/conversion.go:61-68`）：
  - `allow`：默认，永不因诊断拒绝；
  - `safe`：只拒绝带 `error` 级诊断的转换；
  - `strict`：有任何诊断都拒绝。
- 渠道级开关是 `tool_loss_policy`，合法值为空/`allow`/`safe`/`strict`（`relaykit/dto/channel_settings.go:135-144,148-160`），经 `relay/common/relay_info.go:993` 注入转换选项。
- 诊断会写入管理端日志字段 `conversion_diagnostics`，最多记录 32 条，超出后置 `conversion_diagnostics_truncated`（`relay/common/conversion_diagnostics.go:12,46-49`；`service/log_info_generate.go:78-82`）。

**含义**：默认部署下，客户端和普通用户看不到损失；管理员可以通过日志审计。若业务不能接受某些改写，应把渠道设为 `safe` 或 `strict`。

---

## 三、工具调用：能力与损失

### 3.1 基本能力

- function tool 的定义、调用和结果在四种协议之间可互转（`relaykit/README.md:26-28`；`relaykit/relayconvert/internal/toolconv/encode.go`、`decode.go`）。
- Claude 工具的 `strict` 标记可带入 Responses function 工具（`relaykit/relayconvert/internal/claude_messages/to_oai_responses_req.go:237-256`）。

### 3.2 已确认的静默改写/丢失

| 场景 | 行为 | 诊断码 | 证据 |
|---|---|---|---|
| Responses custom tool / namespaced tool → Chat/Claude/Gemini | 展平成普通 function tool；响应侧按 `convmeta.ResponsesToolState` 还原形状 | `tool_namespace_flattened`、`namespaced_tool_dropped` | `relaykit/relayconvert/internal/toolconv/decode.go:166-179` |
| Claude web search `allowed_domains`/`blocked_domains` → Chat `web_search_options` | 域名过滤约束不保留 | `unsupported_domain_filter` | `relaykit/relayconvert/internal/toolconv/encode.go:1414` |
| Claude web search `max_uses` → Chat | 次数上限不保留 | `unsupported_search_limit` | `relaykit/relayconvert/internal/toolconv/encode.go:1417` |
| Claude caller/外部访问控制 → Chat/Responses | 调用方/外部访问约束不保留 | `unsupported_search_controls` | `encode.go:1420`、`encode.go:1441` |
| Claude 响应包含/返回 token 调优 → Chat/Responses | 调优字段不保留 | `unsupported_search_tuning` | `encode.go:1423`、`encode.go:1444` |
| Claude `blocked_domains` → OpenAI Responses | Responses 只支持 allow 过滤，不支持 block 列表 | `unsupported_blocked_domains` | `encode.go:1438` |
| `tool_choice` 指向托管工具但目标不表达 | 视目标协议报错 | `unsupported_tool_choice`、`invalid_native_tool_choice`、`implicit_search_choice` | `encode.go:1572-1673` |
| 复杂 `tool_choice` 降级 | 强制选择被降级 | `forced_tool_choice_downgraded` | `encode.go:665` |
| 无工具时的 `tool_choice` | 转成诊断 | `tool_choice_without_tools` | `encode.go:224` |
| 历史中的托管工具调用 → 不支持目标 | 丢弃历史工具痕迹 | `hosted_tool_history_unsupported`、`unsupported_hosted_tool` | `encode.go:134,507,643,803,900,1258,1400` |

### 3.3 托管工具到 Responses 的特殊映射

Claude 托管工具流式桥接 `relaykit/relayconvert/internal/claude_messages/to_oai_responses_hosted_stream.go`：

- `server_tool_use` / `mcp_tool_use` 聚合 `input_json_delta`，到 `content_block_stop` 才生成 Responses 输出项（`:47-63,118-144`）。
- `web_search` → `web_search_call`；`mcp_tool_use` → `mcp_call`（`:146-156`）。
- **Claude `code_execution` 和 `web_fetch` 没有合法的 Responses 映射**，会直接报错（`:158-161`）。

Gemini 托管搜索桥接 `relaykit/relayconvert/internal/gemini_chat/to_oai_responses_hosted_stream.go`：

- Gemini 在答案之后才报 `webSearchQueries`，桥接器聚合去重后在流结束时补发一个完整 `web_search_call`（`:14-78`）。

### 3.4 工具调用小结

普通 function calling 跨四协议可靠；**托管工具、工具选择策略、搜索过滤条件是最容易静默变形的部分**。尤其 Claude→Chat 会丢掉域名黑白名单、次数上限和调用方约束；Claude↔Gemini 因两跳损失更多。

---

## 四、推理/思考：能力与损失

### 4.1 统一意图模型

推理设置先归一成 Intent（Mode/Effort/BudgetTokens/IncludeThoughts），再落到目标协议：

- `FromOpenAIChat`：`relaykit/relayconvert/reasoning/intent.go:361`
- `ApplyToOpenAIChat`：`:452`
- `ApplyToOpenAIResponses`：`:480`
- `FromOpenAIResponses`：`:513`
- `FromClaude`：`:561`
- `FromGemini`：`:626`

### 4.2 已确认的改写/丢失

| 源→目标 | 行为 | 诊断码 | 证据 |
|---|---|---|---|
| 任意→Claude， adaptive 思考模型 | 不能关闭思考；"disable"被解释成工具调用间隙思考 | `claude_thinking_disable_unsupported`、`claude_thinking_disable_as_between_tools` | `relaykit/relayconvert/reasoning/claude.go:171,184` |
| 任意→Claude | effort 被夹取/调整 | `claude_effort_adjusted`、`claude_effort_unsupported` | `claude.go:238,398,403` |
| 严格采样 Claude 模型 | **剥离 `temperature`/`top_p`/`top_k`** | `claude_sampling_removed` | `relaykit/relayconvert/internal/shared/claude/reasoning.go:165-183` |
| 任意→Claude | `max_tokens` 可能被抬高以满足思考预算 | `claude_max_tokens_raised`、`claude_budget_adjusted` | `claude.go:315,334` |
| 任意→Gemini | 思考档位按目标模型支持档归一 | `gemini_level_adjusted` | `relaykit/relayconvert/reasoning/gemini.go:203,384` |
| Gemini 思考签名 | 函数调用签名按渠道开关补 bypass 值 | — | `relaykit/relayconvert/internal/shared/gemini/request.go:64-96` |

**含义**：推理意图通常能跨协议传达，但**精确档位、采样参数、关闭思考的能力都可能在目标端被改写**。对 Claude 严格采样模型，上游会直接拿不到采样参数；这不是 bug，而是模型能力限制被显式建模。

---

## 五、缓存：能力与损失

| 路径 | 缓存能力 | 证据 |
|---|---|---|
| Claude `cache_control` → OpenAI Chat | 在 system/多模态块上保留 CacheControl | `relaykit/relayconvert/internal/claude_messages/to_oai_chat_req.go:140,188` |
| OpenAI Chat `prompt_cache_key` → Responses | 保留 PromptCacheKey | `relaykit/relayconvert/internal/oai_chat/to_oai_responses_req.go:404-428` |
| OpenAI Responses `prompt_cache_key`/`prompt_cache_retention` → Chat | 回传到 Chat 请求 | `relaykit/relayconvert/internal/oai_responses/to_oai_chat_req.go:81,112-115` |
| Claude → Gemini | **无缓存控制等价物**，本矩阵未发现保留路径 | 结构上无对应字段 |
| Claude → Responses 请求 | **无 `cache_control` 映射**；只转 system/messages/tools 等 | `relaykit/relayconvert/internal/claude_messages/to_oai_responses_req.go:33-70` |
| Chat/Responses → Claude | Claude 请求无 prompt cache 字段可承接 | Claude DTO 无对应字段 |

**结论**：缓存是跨协议损失最严重的维度之一。只有少数"源和目标都有近似字段"的路径能保留；Claude→Gemini、Claude→Responses、Chat→Claude 等路径没有可用的缓存控制承接字段。

---

## 六、多模态：能力与损失

### 6.1 已支持

- Claude 图片 → OpenAI Chat `image_url`（`relaykit/relayconvert/internal/claude_messages/to_oai_chat_req.go:205-211`）。
- Claude 图片/文档 → Responses `input_image`/`input_file`（`to_oai_responses_req.go:180-202`）。
- Gemini 图片/PDF/文本/WAV/MP3 → Chat 对应内容块（`relaykit/relayconvert/internal/gemini_chat/to_oai_chat_req.go:217-285`）。
- Gemini 支持的 MIME 清单见 `relaykit/relayconvert/internal/shared/gemini/request.go:22-55`，音频/视频按前缀接受。
- URL/data URL 解析依赖宿主注册的 `relayconvert.SetMediaResolver`，由 `service/request_converter.go:14-24` 注入。

### 6.2 已确认的丢失/报错

| 场景 | 行为 | 诊断码 | 证据 |
|---|---|---|---|
| Gemini 视频/不支持的 inline MIME → Chat | 报诊断并跳过该媒体 | `unsupported_media_type` | `gemini_chat/to_oai_chat_req.go:104,260-285` |
| Claude 文档非 base64 PDF/纯文本 → Chat | 报诊断并省略文档 | `unsupported_media_source` | `claude_messages/to_oai_chat_req.go:213-231` |
| Chat/Responses 图片 → Gemini | 仅接受 Gemini 支持的 MIME；不支持的报错 | — | `shared/gemini/request.go:50-55` |

**结论**：多模态不是"全类型通用"。音频、视频、文档在许多组合里会被降级或丢块；跨协议多模态应优先使用双方都支持的形式（图片、PDF、纯文本），避免依赖视频等单侧能力。

---

## 七、结构化输出：能力与损失

### 7.1 支持路径

- Chat `response_format.json_schema/json_object` → Gemini `responseMimeType: application/json` + schema（`relaykit/relayconvert/internal/oai_chat/to_gemini_chat_req.go:184-187`）。
- Responses `text.format` → Gemini 同上（`relaykit/relayconvert/internal/oai_responses/to_gemini_chat_req.go:185-201`）。
- Gemini API 版本差异：**v1 没有 `parametersJsonSchema`；v1beta/Vertex AI 支持**（`relay/common/relay_info.go:983-987`）。

### 7.2 已确认的清洗/丢失

- 进入 Gemini 前，JSON Schema 会被清洗成 Gemini 允许的关键字白名单，包括 `anyOf`、`default`、`description`、`enum`、`format`、`items`、`maxLength`、`minimum`、`pattern`、`required`、`type` 等（`relaykit/relayconvert/internal/shared/gemini/schema.go:17-40`）。
- 枚举只保留全字符串形式；数字/布尔枚举被删除，等于放宽了约束（`schema.go:145-158`）。
- 无法表达的关键字会记录 `gemini_schema_keyword_dropped`（`schema.go:294`）。

**结论**：结构化输出到 Gemini 是"尽力保留 + 白名单清洗"。复杂 JSON Schema（如 `allOf`、自定义关键字、非字符串枚举）会丢约束或报诊断；需要强约束时，优先选择 Responses/Chat 原生路径或支持 JSON Schema 的 Gemini 版本。

---

## 八、其他常见参数的改写

| 参数/场景 | 行为 | 证据 |
|---|---|---|
| Responses `conversation`、`previous_response_id`、`prompt`、`context_management` | 降级到 Chat 时**硬错误**；Claude/Gemini 路径同样拒绝有状态字段 | `relaykit/relayconvert/internal/oai_responses/to_oai_chat_req.go:121-141` |
| Chat `n > 1` → Responses 兼容模式 | 报错，不支持多候选 | `relaykit/relayconvert/internal/oai_chat/to_oai_responses_req.go:90-92` |
| 系统提示 | Claude system / Chat system/developer → Responses `instructions`；Claude system 文本块拼接 | `to_oai_responses_req.go:38,117-145` |
| 工具结果 | Claude `tool_result` → Responses `function_call_output` | `to_oai_responses_req.go:216-226` |
| 末尾 assistant 文本 | 为避免目标协议当作 prefill，删除末尾 assistant 文本，报 `trailing_assistant_omitted` | `relaykit/relayconvert/internal/oai_responses/req_helpers.go:75-84` |
| Claude `max_tokens` | Claude 必填；缺失时可用渠道默认值，推理场景可能自动抬高 | `shared/claude/reasoning.go:94-125` |
| Chat `stop` → Gemini | 截断为最多 5 个 stop sequences | `relaykit/relayconvert/internal/oai_chat/to_gemini_chat_req.go:52-56` |
| Chat `seed` → Gemini | 映射到 `generationConfig.seed` | `to_gemini_chat_req.go:37-39` |
| Chat/Responses 元数据、`store`、user 等 | 按字段在 DTO 中显式建模，能带则带 | `relaykit/dto/openai_request.go:82-87,1060-1135` |

---

## 九、按"静默丢失风险"排序的实践建议

1. **不要假设参数全量透传**。跨协议网关的语义是"转换到目标协议可表达的形式"，不是 JSON 透传。
2. **对工具能力敏感的业务**：避免依赖 Claude 的 `blocked_domains`、`max_uses`、caller 约束等高级搜索控制跨协议转发；必要时把渠道设为 `strict`。
3. **对采样和推理档位敏感的业务**：避免跨协议依赖精确 `temperature/top_p/top_k`；Claude 严格采样模型会剥离这些参数。
4. **缓存提示不要跨协议依赖**：除非确认走的是少数保留路径。
5. **多模态优先使用图片/PDF/文本**；视频、音频等能力在多数转换路径会被丢块或报 `unsupported_media_type`。
6. **结构化输出到 Gemini 时简化 schema**：避免复杂关键字和非字符串枚举；确认 Gemini API 版本支持 JSON Schema。
7. **需要审计静默损失时**：查看管理端日志的 `conversion_diagnostics` / `conversion_diagnostics_truncated` 字段，或把渠道 `tool_loss_policy` 设为 `safe`/`strict` 做拦截。
8. **最不稳的组合是 Claude↔Gemini**：两跳经 OpenAI Chat，工具、推理、缓存、结构化输出都更容易变形；生产上尽量安排直连或同协议兜底。

---

## 附：证据文件索引

- 矩阵与等级定义：`relaykit/README.md:7-18`
- 诊断模型与策略：`relaykit/types/conversion.go:8-68`
- 渠道策略：`relaykit/dto/channel_settings.go:135-160`
- 诊断落日志：`relay/common/conversion_diagnostics.go:12,46-49`；`service/log_info_generate.go:70-84`
- 请求入口：`relay/compatible_handler.go:112`、`relay/claude_handler.go:96`、`relay/gemini_handler.go:100`、`relay/responses_request.go:61`、`relay/chat_completions_via_responses.go:125`
- 转换服务封装：`service/request_converter.go:26-48`
- 工具损失：`relaykit/relayconvert/internal/toolconv/encode.go:134-1673`、`decode.go:166-179`
- 推理损失：`relaykit/relayconvert/reasoning/claude.go:171-403`、`gemini.go:203,384`、`internal/shared/claude/reasoning.go:165-183`
- 缓存：`internal/claude_messages/to_oai_chat_req.go:140,188`、`internal/oai_chat/to_oai_responses_req.go:404-428`、`internal/oai_responses/to_oai_chat_req.go:81-115`
- 多模态：`internal/gemini_chat/to_oai_chat_req.go:104,217-285`、`internal/shared/gemini/request.go:22-55`
- 结构化输出：`internal/shared/gemini/schema.go:17-40,145-158,294`、`relay/common/relay_info.go:983-987`
- 有状态字段：`internal/oai_responses/to_oai_chat_req.go:121-141`
