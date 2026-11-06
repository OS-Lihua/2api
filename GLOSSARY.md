# LLM 网关

统一接入多个模型服务供应商，对下游提供统一的模型 ID 和多协议兼容，并按上下游分别计价结算的 AI 网关。

## 主体

**Provider（供应商）**:
网关接入的一个上游账号，包含地址、凭据（可多把 Key 轮换）和账号级配额；同一家公司的多个独立账号是多个 Provider。
_Avoid_: Connection、渠道、账号、上游

**Publisher（模型发行方）**:
发布并维护某个模型的主体。同一家企业可以同时是某个 Provider 的所属公司和 Publisher，两种角色分开表达。
_Avoid_: Vendor、厂商、模型供应商

## 模型与调用通道

**Model（统一模型）**:
网关内部统一识别的模型身份，属于一个 Publisher。仅名称相似不构成同一 Model，版本或行为不同即为不同 Model。
_Avoid_: 模型名、上游模型

**ModelOffering（对外模型）**:
用户看到、授权、调用和付费的对象，由一个或多个分层级的成员 Model 提供服务；对外模型 ID 即其主名。
_Avoid_: 模型映射、模型别名、对外模型服务

**别名（Alias）**:
除主名外也能调用某个 ModelOffering 的名称，不出现在模型列表中。
_Avoid_: 映射名、旧名

**成员层级（Member tier）**:
ModelOffering 中成员 Model 的排序；同一层级的成员可互换，较低层级是降级目标。
_Avoid_: 优先级（指 Channel 调度时）

**Channel（通道）**:
通过某个 Provider、以某种协议调用某个上游模型、从而提供某个 Model 的具体路径；是选路和采购计价的最小单位。
_Avoid_: 线路、渠道

**协议（Protocol）**:
某个 Channel 与 Provider 通信所用的 API 格式，如 OpenAI Chat、Anthropic Messages。
_Avoid_: 渠道类型、接口格式

**能力（Capability）**:
Model 或 Channel 能处理的请求特性，如工具调用、图像输入、上下文长度上限；Channel 的能力只能是其 Model 能力的子集。
_Avoid_: 端点类型、支持项

**上游模型名（Upstream model name）**:
某个 Channel 向 Provider 发起调用时使用的模型名称。
_Avoid_: 映射名、模型别名

**尝试（Attempt）**:
为处理一次下游请求而向某个 Channel 发起的一次上游调用；一次请求可包含多次尝试。
_Avoid_: 重试（指单次调用时）

## 价格

**价格版本（Price version）**:
某个 ModelOffering 的销售价或某个 Channel 的采购价在一段生效期内的不可变定义，包含币种与计费表达式。
_Avoid_: 倍率、价格配置

**采购价（Purchase price）**:
网关调用某个 Channel 时向 Provider 支付的单位价格。
_Avoid_: 成本价、渠道价

**销售价（Sale price）**:
网关向用户收取的、按 ModelOffering 定价的单位价格；不随实际提供服务的 Model 或 Channel 变化。
_Avoid_: 模型倍率、售价倍率

**钱包（Wallet）**:
用户在网关的余额账户，使用站点结算币种。
_Avoid_: 额度、quota

**账目（Ledger entry）**:
一笔只追加、不可修改的金额记录，带币种，关联账户和请求或尝试；是金额的唯一依据。
_Avoid_: 消费日志、流水日志

## 路由

**Route（路由策略）**:
在对外保持同一个 ModelOffering 的前提下，决定一次请求实际由哪个 Channel 处理的规则。
_Avoid_: 选路逻辑、调度策略、Router

**通道容灾（Failover）**:
在 ModelOffering 同一成员层级内的 Channel 之间切换，可跨该层的不同 Model。
_Avoid_: 线路容灾、重试、降级

**模型降级（Model fallback）**:
请求改由 ModelOffering 较低成员层级的 Model 处理。
_Avoid_: 容灾、回退线路

**转换损失（Conversion loss）**:
下游请求转换为 Channel 的上游协议时，某个参数或能力被丢弃、改写或截断。
_Avoid_: 静默丢参、兼容降级
