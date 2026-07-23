# New API 源码功能修改说明

本文档以项目官方源码为基础，完整说明本次新增、调整和加固的功能。内容覆盖 Codex 与 Claude Code OAuth 渠道、令牌权限、模型管理与计费、上游请求隐私、重试数据治理、日志脱敏，以及本地和服务器部署体系。以下描述以当前源码实际行为为准。

## 1. ChatGPT/Codex 订阅渠道

### 1.1 上游动态模型发现与可选限制

文件：`relay/channel/codex/constants.go`

Codex 渠道不再在代码中固定模型列表。模型列表由环境变量
`CODEX_MODEL_LIST` 控制：

- 未设置或为空：不返回本地预设模型，渠道编辑器通过 OAuth 授权后的上游模型接口获取当前账号实际可用的模型；
- 设置为逗号分隔列表：仅将该列表作为管理员显式限制，自动去除空项和重复项。

因此，不再维护 `gpt-5.4`、`gpt-5.4 mini`、`gpt-5.6-*` 或 compact 后缀模型的代码内白名单，避免本地目录与上游账号实际权限脱节。

### 1.2 Codex OAuth 浏览器授权流程

新增后端接口：

```text
POST /api/channel/codex/oauth/start
POST /api/channel/codex/oauth/complete
```

实现文件：

- `service/codex_oauth.go`
- `controller/codex_oauth.go`
- `router/channel-router.go`

实现内容：

- 生成 OAuth `state` 和 PKCE `code_verifier`；
- 生成 OpenAI 授权地址；
- 使用 session 保存 state/verifier；
- 接收浏览器回调 URL；
- 校验 `code`、`state` 和 PKCE verifier；
- 请求 `https://auth.openai.com/oauth/token` 换取 access token 和 refresh token；
- 从 JWT 提取 ChatGPT account ID 和 email；
- 生成现有 Codex 渠道兼容的 JSON credential；
- 授权完成后清理 session 中的 OAuth 状态。

授权码换取 Token 的失败现在按原因返回安全提示：授权码或 redirect URI 被拒绝（`400/422`）、
凭证无效（`401`）、账号无权访问（`403`）、上游限流（`429`）、上游临时不可用（`5xx`）、
网络不可达、超时和异常成功响应不再统一显示为“OAuth authorization failed”。服务日志保留底层
网络原因，但会统一脱敏 callback URL、授权码和 Token。

授权、刷新、模型发现、用量查询和请求转发共用同一份 Codex credential DTO。刷新响应没有返回新的
`refresh_token` 时保留原 refresh token；同一渠道的自动刷新、手动刷新和用量查询刷新按渠道 ID
串行执行，避免 refresh token 轮换竞争；等待锁期间如果其他请求已经完成刷新，则直接复用新的凭证，
不再连续旋转 refresh token。刷新结果使用渠道 ID 和刷新前 Key 作为更新条件，只有凭证仍是本次读取的
版本时才写回；管理员并发重新授权后，后台刷新不会覆盖新凭证。用量查询只在上游真实返回 `401` 时
刷新，`403` 作为账号或组织权限问题直接返回，不再无效刷新。

生成的 credential 结构包含：

```json
{
  "access_token": "...",
  "refresh_token": "...",
  "account_id": "...",
  "email": "...",
  "type": "codex",
  "last_refresh": "...",
  "expired": "..."
}
```

### 1.3 Codex 前端授权界面

新增文件：

```text
web/default/src/features/channels/components/dialogs/codex-oauth-dialog.tsx
```

后台 Codex 渠道编辑界面新增“Authorize”按钮，支持：

1. 打开 OpenAI 授权页面；
2. 复制授权链接；
3. 粘贴 localhost 回调 URL；
4. 调用后端完成授权；
5. 自动把生成的 JSON credential 写入 Key 字段。

前端 API 封装位于：

```text
web/default/src/features/channels/api.ts
```

新增 `startCodexOAuth()` 和 `completeCodexOAuth()`。

### 1.4 Codex 请求兼容性

文件：`relay/channel/codex/adaptor.go`

客户端身份类请求头不再透传：

```text
session-id
thread-id
x-client-request-id
x-codex-parent-thread-id
x-codex-turn-state
x-codex-turn-metadata
x-codex-window-id
X-Session-ID
X-Codex-Installation-ID
```

这些值即使通过 Header 通配、正则透传或 `{client_header:...}` 配置也会被 OAuth
保护规则拦截。仅保留不承载客户端身份的能力标志：

```text
x-openai-memgen-request
x-openai-subagent
x-responsesapi-include-timing-metrics
```

认证、账号、`originator`、`user-agent`、`content-type`、
`x-codex-beta-features` 等身份、能力与协议 Header
由服务端生成，客户端和渠道静态配置均不能覆盖。内部
`x-openai-internal-codex-responses-lite` 不接受客户端透传，也不能通过渠道
Header Override 强制开启；服务端仅在映射后的上游模型属于 `gpt-5.6-*`
且端点为 `/v1/responses` 时受控补充。

对于 `gpt-5.6-*` 模型的渠道测试和正常 OAuth API 请求，自动补充：

```text
originator: Codex Desktop
x-openai-internal-codex-responses-lite: true
x-codex-beta-features: remote_compaction_v2
```

同时在 Responses 请求中补充测试所需的：

- `client_metadata`；
- `include: ["reasoning.encrypted_content"]`；
- `parallel_tool_calls: false`；
- `tool_choice: "auto"`；
- `text: {"verbosity":"low"}`；
- `reasoning.context: "all_turns"`；
- 由网关为每个请求动态生成的 session/thread/turn/window/installation 元数据。

客户端传入的 `client_metadata` 不会进入上游：普通 Codex 请求直接删除该可选字段，
Lite 请求使用网关生成的最小 metadata 完整替换，避免设备 UUID、安装 ID、位置和客户端
会话标识随订阅 OAuth 身份发送。Claude Code OAuth 同样删除客户端 `metadata.user_id`。

Lite 请求还必须满足：

- `stream: true`；
- `store: false`；
- 工具仅允许 `function`、`custom`，以及 `execution: "client"` 的
  `tool_search`。HTTP、SSE 和 WebSocket 请求会在最终出站前递归过滤
  `namespace`、`web_search`、`image_generation`、托管 `tool_search` 等 Lite
  不支持的 hosted 工具，同时保留客户端函数、自定义工具和客户端工具搜索。

这些修改用于解决 Codex Lite 对请求头、流式响应和 reasoning context 的严格校验。

Codex 订阅渠道只支持 Responses 上游。客户端使用 `/v1/chat/completions` 或
`/v1/messages` 时，无论全局 Chat Completions 转 Responses 策略是否启用，都会
强制执行协议转换并把响应转换回客户端原协议，避免系统设置关闭后直接落入 Codex
适配器的“不支持端点”分支。

### 1.5 Responses Lite 搜索与 WebSocket

新增客户端入口：

```text
POST /v1/alpha/search
POST /backend-api/codex/alpha/search
GET /v1/responses  (WebSocket Upgrade)
```

- Alpha search 通过当前 API Key 的分组和模型规则选择 Codex OAuth 渠道，转发至
  `/backend-api/codex/alpha/search`，恢复 Responses Lite 的 `web.run` 搜索；
- 搜索请求移除仅用于本地路由的 `prompt_cache_key`、
  `prompt_cache_retention` 以及客户端 session/thread/turn/device/client metadata；客户端
  `id` 会替换为网关随机 ID，并作为该请求的 `X-Session-ID` 使用。客户端提供的会话 Header
  和 `X-OpenAI-Actor-Authorization` 均不会转发；
- Responses WebSocket 会话按连接代际固定同一 OAuth 渠道和凭证，使用上游
  `responses_websockets=2026-02-06` 协议；自包含 turn 可切换模型并建立新连接，
  continuation 仍固定到生成其 `previous_response_id` 的原连接和模型；
- `/v1/models?client_version=...` 返回 Codex 客户端兼容目录，并仅为实际由 Codex
  渠道承载的模型声明 `prefer_websockets: true` 和搜索能力；各启用 OAuth 凭证从上游模型
  目录取得 context window，按渠道和可路由分组取共同最小值。Gemini 和 OpenAI 兼容上游目录若
  返回 `inputTokenLimit`、`context_window` 或 `context_length`，也会以相同方式缓存和聚合。能力目录
  采用三层优先级：全部可路由渠道均确认的上游实测值、维护在 `model/official_model_metadata.go` 中的
  官方公开规格、保守默认值。Claude Code 等未提供窗口元数据的渠道因此使用官方规格，而不会套用
  Codex 的默认值。任一渠道无法确认或数据越界时不使用部分实测结果，回退官方规格或默认值。
  未收录的普通模型默认按 128K 返回；未收录的 Codex 模型保留原有 272K / 1M 客户端兼容回退。
  已验证目录缓存到渠道 `settings`；OAuth 凭证、渠道类型、上游地址、模型映射或多 Key 启用集合变化时
  立即清除缓存，等待下一次手动抓取或渠道级巡检重新确认；
- `response.create` 和 `response.append` 均进入既有鉴权、模型限制、计费、隐私和
  错误处理链；同一会话固定渠道，避免 OAuth 身份在会话中途漂移；
- 上游 WebSocket 事件按客户端协议实时返回；上游明确返回 `426 Upgrade Required`
  时自动回退 HTTP/SSE，不会因上游暂未开放 WebSocket 而中断请求。
- 自包含 turn 的上游连接空闲超过 30 秒时，会在写入 `response.create` 前主动重连；
  带 `previous_response_id` 的 continuation 始终保留原连接，不能迁移到替代连接。
  每条上游连接由唯一常驻 reader 处理业务帧和控制帧，空闲期仍会回复上游 Ping；terminal
  写回完成前不会预读下一 turn；空闲期的 `response.*`/`error` 仍按 turn 事件失败关闭，
  其他合法非空类型作为连接扩展仅记录类型和连接代际后丢弃，不使用容易过期的固定白名单。
  continuation 最近活动超过 30 秒时，会在写入前使用一次带唯一关联值的 Ping/Pong 做 3 秒
  存活确认；失败时关闭原连接并快速失败，
  不写入、不迁移、不重放。被动保活依赖当前观测到的上游约 20 秒 Ping 周期；即使该行为变化，
  写前探测仍保证不会向未经确认的连接写 continuation。连接在 55 分钟主动回收，早于上游
  60 分钟硬上限；并发空闲连接数作为生产观察项，只有数据证明需要时才增加统一 LRU/总量上限。
  首个 turn 业务事件等待限制为 30 秒，连接元数据不会延长该期限；超时后不重放已写入的请求，
  避免重复执行或重复扣费。临时协议诊断只记录握手、事件类型、事件间隔、终止状态及错误帧写回结果，
  不记录请求正文、响应正文、SSE 内容或 WebSocket 控制帧载荷；生产行为确认后移除该诊断。
- WebSocket 帧和转换后的请求体统一使用 `MAX_REQUEST_BODY_MB` 限制，默认 `128 MB`，
  不再单独使用固定的 `16 MB` 限制；超限返回 `413 Request Entity Too Large`，并标记为
  不可重试，避免大请求被重复发送到备用渠道。每轮结束只保留后续 append 所需的模型和配置字段，
  不保留上一轮 `input`、`previous_response_id` 等大请求状态；
- Codex/Claude Code OAuth 遇到 EOF、连接重置等响应头前断连时返回 `502`，
  交由 Retry Times 决定是否故障转移；不关闭共享 HTTP 连接池，且这类临时 `5xx`
  不会禁用渠道或标记 OAuth 凭证失效。客户端已取消则返回 `499` 且不重试。

该能力沿用 API Key 鉴权；WebSocket 客户端可通过既有
`Sec-WebSocket-Protocol` 机制携带 API Key。WebSocket 的首个请求必须是
`response.create` 且包含模型；后续 `response.append` 只能在同一连接内追加输入，
缺失的通用字段从上一请求继承，并强制转换为流式请求。

### 1.6 Responses DTO 扩展

文件：`dto/openai_request.go`

新增字段：

```go
ClientMetadata json.RawMessage `json:"client_metadata,omitempty"`
```

并为 `Reasoning` 增加：

```go
Context json.RawMessage `json:"context,omitempty"`
```

### 1.7 渠道测试和模型同步

文件：

- `controller/channel-test.go`
- `controller/channel.go`
- `controller/channel_upstream_update.go`
- `controller/model.go`
- `dto/channel_settings.go`
- `model/channel_model_metadata.go`
- `model/official_model_metadata.go`
- `relay/channel/codex/models.go`
- `relay/channel/gemini/relay-gemini.go`

Codex 渠道测试强制使用 stream 模式。定时自动测试是否执行完全由后台渠道健康检查中的
“定时渠道测试”设置控制，不在 Compose、环境变量或渠道类型判断中额外硬编码排除。
适配器仅在配置 `CODEX_MODEL_LIST` 时返回该显式限制列表；管理端手动抓取模型和上游模型巡检调用
`/backend-api/codex/models`，逐一检查渠道内所有启用且去重后的 ChatGPT OAuth 凭证，聚合实际
可用模型及 context window；任一凭证失败时整次目录抓取失败，不返回可能导致误删模型的部分目录。
模型抓取继承请求或任务 Context，并受到 `CHANNEL_MANAGEMENT_REQUEST_TIMEOUT`（默认 30 秒）的
整次请求总超时约束，不再使用不可取消的 `context.Background()`，也不会因多 Key 串行检查把
总等待时间放大为凭证数乘以 30 秒。
上游价格同步明确排除 Codex 与 Claude Code OAuth 渠道。

### 1.8 Codex 使用量界面

文件：`web/default/src/features/channels/components/dialogs/codex-usage-dialog.tsx`

用量卡片从显示“Used”改为显示“Remaining”，进度条和百分比改为剩余百分比：

```text
remaining = 100 - used_percent
```

并对百分比进行边界限制，避免出现负数或超过 100 的显示。
用量、重置次数和用量重置请求不再使用固定的 15 秒总超时，而是复用
`SUBSCRIPTION_OAUTH_RESPONSE_HEADER_TIMEOUT`（默认 30 秒），并额外保留 5 秒用于读取和处理响应体。
这避免网络已完成 TLS 握手、但 ChatGPT 在 15 秒后才返回响应头时被本地提前取消。
三个 Wham 接口共用同一请求构造、认证 Header 和响应读取逻辑，响应体最大限制为 1 MB。渠道 Base URL
会先规范化为 HTTP/HTTPS 源站或代理前缀，自动剥离已有的 `/backend-api`、`/backend-api/codex`、
`/backend-api/wham` 及其资源路径，避免生成重复路径；带 URL userinfo 的地址会被拒绝。

## 2. Claude Code 订阅渠道

### 2.1 新增渠道类型

文件：`constant/channel.go`

新增：

```go
ChannelTypeClaudeCode = 59
```

渠道名称：

```text
Claude Code (OAuth)
```

默认地址：

```text
https://api.anthropic.com
```

### 2.2 复用 Anthropic 适配器

文件：`common/api_type.go`

将 `ChannelTypeClaudeCode` 映射为 `APITypeAnthropic`，复用现有 Claude 请求转换逻辑。

### 2.3 OAuth 请求认证

文件：`relay/channel/claude/adaptor.go`

支持以下 Key 格式：

```text
CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-...
```

也支持直接填写 Token。

请求不再使用 `x-api-key`，而是发送：

```http
Authorization: Bearer <token>
anthropic-beta: oauth-2025-04-20
anthropic-version: 2023-06-01
user-agent: claude-cli
x-app: cli
```

空 Token 会在本地直接返回错误。

### 2.4 模型获取和流式支持

文件：

- `controller/channel.go`
- `controller/channel-billing.go`
- `relay/common/relay_info.go`

后台获取 Claude Code 模型列表时使用 OAuth 请求头；同时将 Claude Code 加入支持流式选项的渠道列表。

### 2.5 Claude Code 前端配置

文件：

- `web/default/src/features/channels/constants.ts`
- `web/default/src/features/channels/lib/channel-utils.ts`
- `web/default/src/features/channels/components/drawers/channel-mutate-drawer.tsx`

前端新增：

- 渠道类型选项；
- Claude 图标；
- 模型抓取支持；
- Key 格式提示；
- Anthropic 高级配置和字段透传配置复用。

### 2.6 订阅 OAuth 共通保护与故障转移

- Codex 和 Claude Code 禁止请求体直通与请求体参数覆盖；
- 允许渠道亲和生成的安全 Header 透传，但过滤认证和身份 Header；
- 调试日志只记录请求体大小，不输出订阅 OAuth 请求正文；
- 并发与 50ms 最小启动间隔按 Codex `account_id` 或 Claude Code 凭证指纹共享，而非按渠道 ID；默认单服务实例账号并发为 5，同一凭证配置到多个渠道不会叠加额度；不同服务器按各自实例独立限流；
- Claude Code 可通过 `CLAUDE_CODE_OAUTH_LOCAL_LIMITS_ENABLED=false` 临时暂停本机并发、最小请求间隔和凭证冷却门禁；该开关不关闭上游错误分类、同分组故障转移、已输出后禁止重放及明确失效凭证隔离。默认开启，修改后需重启；当前本机及正式部署配置均默认开启；
- “账户信息”“刷新凭据”“从上游获取”、手动模型发现、定时上游模型检查、Codex 用量查询/重置和管理员手动最小化渠道测试等管理请求完全独立于推理容量状态，不占用 OAuth 推理并发槽、不等待最小请求间隔，并可绕过用量窗口冷却；这些请求不会清除或延长推理冷却，也不会抢占半开放恢复探针；定时和管理员触发的批量渠道测试均排除全部订阅 OAuth 渠道，只保留管理员单渠道最小化测试；
- 等待上游首个响应头默认最多 30 秒；Transport 超时返回 `504` 和 `Retry-After`，便于客户端与真正的应用内部 `500` 区分；
- 本地并发保护在 HTTP/SSE、Responses WebSocket 和 Alpha Search 路径统一返回专用、可重试的
  `oauth_channel_concurrency_limit` 503。并发槽饱和时使用 `Retry-After: 1`；普通冷却返回向上取整后的真实剩余秒数；套餐用量窗口冷却会保留 `upstream_usage_limit` 原因，并同时返回预计 UTC 重置时间和剩余秒数，不再降级显示为通用“凭证正忙”；该错误只写运行日志，不进入持久化错误日志；
- 达到并发上限后排除的是 OAuth 凭证指纹，而非整个渠道；相同凭证配置在其他渠道或其他分组时共享同一并发、节奏与冷却状态，多 Key 渠道仍可继续使用其他未饱和账号；订阅 OAuth 的自动安全默认值固定在本次请求首次选中的实际分组，并限定相同渠道类型与兼容数据处理策略，渠道标签仅作为管理元数据；
- 订阅 OAuth 独立配置上游重试次数、容量循环次数、容量累计等待上限和普通 429 跨账号重试。默认分别为 5、5、5 秒和关闭；并发槽申请失败会归还本次凭证尝试计数，不消耗上游失败预算；普通突发限流 429 按上游 `Retry-After`（最长 15 分钟，缺失时 30 秒）冷却当前凭证，关闭开关时将错误返回客户端，打开时立即切换已冻结实际分组内的备用账号，不在原账号上重复放大 429；
- 每个 OAuth 凭证拥有独立的 5 次可重试上游失败预算，并在实际 Relay 入口另设同值的凭证选择硬上限；整个客户端请求另有 10 次 Relay 尝试硬上限，避免候选账号数量乘以单凭证预算造成请求放大。耗尽后停止并返回最后一个真实上游错误；同一凭证耗尽时会在当前请求中排除、短暂冷却并切换已冻结实际分组内的下一凭证；恢复状态使用代际校验，旧请求的迟到成功不能清除新一轮冷却；恢复探测的结果确认与并发槽释放在同一临界区完成；下游已经输出或客户端取消后禁止重试；
- 订阅 OAuth 上游错误按语义统一为 `oauth_unauthorized`（401 凭证无效）、`oauth_forbidden`（403 权限拒绝）、`upstream_account_disabled`（账号或组织停用）、`upstream_quota_exhausted`（余额或永久额度耗尽）、`upstream_rate_limited`（临时突发限流）、`upstream_usage_limit`（可自动恢复的套餐用量窗口耗尽）和 `model_not_supported`（模型不可用）；额度识别覆盖上游 error code 及中英文常见错误文本，不再把所有 429 混为同一种故障；
- Claude Code 不再调用或暴露 `https://api.anthropic.com/api/oauth/usage`；该接口可能独立返回 `403/429`，也无法稳定提供准确用量窗口和重置时间，不能作为推理失败原因的可靠证据。推理重试只使用实际模型响应的 HTTP 状态、结构化错误代码/类型、错误信息和 `Retry-After`：通用 `429 rate_limit_error` 及 `This request would exceed your account's rate limit` 归类为 `upstream_rate_limited`；只有明确包含 `usage_limit`、`usage limit`、`weekly limit` 等套餐窗口语义时才归类为 `upstream_usage_limit`；
- Codex 的订阅窗口耗尽有时只返回 `exceeded retry limit, last status: 429 Too Many Requests`；中转会用同一账号查询 Wham 用量快照，仅当 `limit_reached`、`allowed=false` 或主/次窗口达到 100% 时升级为 `upstream_usage_limit` 并采用快照重置时间。快照不可用、过期或仍有余量时保留普通临时 429；Codex Wham 关联查询同样使用 30 秒成功快照和 5 秒失败结果的有界短缓存，管理端用量与重置操作仍直接访问上游；
- OAuth `401/403`、账号或组织停用、余额或永久额度耗尽会立即在当前请求中排除凭证并切换合规备用账号；原始 HTTP `401/403` 本身不作为持久禁用证据。Codex 的 `401` 或 Access Token 过期会先对当前凭证执行一次带旧 Access Token 条件的串行刷新，刷新成功且尚未输出时重试同一账号；只有 Token 端明确拒绝 Refresh Token、Refresh Token 被撤销或要求重新授权，或者推理端明确返回账号/组织停用、永久余额不足时，才在一次数据库事务中隔离该凭证的全部渠道引用。普通 `oauth_forbidden`、模糊的“调用次数/额度达到上限”等文案不会持久禁用。套餐用量窗口耗尽不会禁用数据库渠道，而是按实际推理错误中的重置字段冷却对应凭证，缺失时默认一小时、最长八天，并在安全时切换同组备用凭证；Responses WebSocket 使用同一状态机；模型不支持和模型容量不足均只在当前模型请求中排除该凭证，不会误伤该账号的其他模型；
- 恢复探测若在最小请求间隔等待或其他上游发送前阶段被取消，会以“未发送”结果释放并发槽，不会被误判为上游失败或延长冷却；
- 管理员手动渠道测试保留适配器返回的结构化状态码、错误代码、上游状态和重试信息，并在测试结束时确认恢复探针结果，避免将真实 400/429/503 包装成通用 500 或让半开放并发槽长期占用；
- 每个服务实例运行独立的凭证状态条件清理器，每 10 分钟从数据库读取当前 Codex/Claude Code 渠道仍引用的凭证指纹；仍被任一渠道引用的有效指纹即使长期没有请求也保留状态。只有凭证轮换、渠道删除或渠道类型变更后不再被数据库引用的旧指纹，且至少空闲 1 小时、没有活动租约、恢复探测、未结束冷却或节流预约时才回收；回收只删除进程内并发/冷却状态，不修改数据库渠道、OAuth 凭证或标签组；数据库查询失败时整轮清理失败关闭；回收先在状态锁内标记 retired，再按指针匹配从共享 Map 删除，并发请求发现 retired 后重新加载，避免同一凭证被拆成两套并发计数；
- 容量池全部饱和时执行包含首次扫描在内的 5 轮凭证检查，采用约 250ms、500ms、1s、2s 的带抖动退避，累计等待不超过 5 秒；第一轮按优先级和权重确定凭证顺序，后续轮次稳定复用该顺序，实现 A→B→C→A；上游失败路径由有限凭证集合及每凭证预算自然终止，不再使用会截断后续凭证预算的隐藏总尝试数和 45 秒软限制；
- Alpha Search 使用与普通 Relay 相同的订阅 OAuth 重试预算、RetryBoundary、模型映射和渠道数据策略，并在重试前按单次调用完成一次预扣、成功后一次结算和消费日志记录；搜索正文不进入日志；
  WebSocket 只在首次上游 WebSocket 握手成功或 HTTP 回退响应完整成功后固定渠道，使内部 Relay 能在会话固定前选择合规备用凭证；成功重试前
  清除前一失败尝试的 `Retry-After`，避免 `200` 响应携带过期重试指令；
- 重试候选已排除本请求尝试过的渠道时，先尝试当前最高优先级中仍可用的渠道，再降到下一优先级，
  不会因为重试次数直接跳过同优先级备用渠道；
- 普通渠道仅在请求尚未写入上游且下游尚未输出时自动重试，避免连接重置造成重复执行；订阅 OAuth 的明确失败响应仍受独立凭证预算保护。原始 504/524 始终禁止自动重试，只有管理员明确映射为其他可重试状态码并完成高风险确认后才按映射策略处理；
- Codex Responses 流会在首个实际输出前暂存 `response.created`、`response.in_progress` 等生命周期事件，暂存上限为 64 KB，首个有效输出等待上限为 30 秒。预检完成前不向客户端发送 SSE 保活、失败账号的生命周期事件或 `X-Codex-Turn-State`；超时时终止当前上游尝试，以可重试的 `502/do_request_failed` 交由现有安全策略切换候选凭证，不触发真实 504 的全局禁止重试规则。若此时收到 `Selected model is at capacity` / `model_at_capacity`，网关会记录结构化 `model_at_capacity` 错误并在已冻结实际分组内切换备用凭证。该专用切换不依赖“429 跨 OAuth 账号重试”开关，也不会禁用或冷却凭证；如果上游在此阶段以 HTTP/2 `INTERNAL_ERROR`、连接重置等方式中断流，网关会返回可重试的 `502/do_request_failed` 并切换候选凭证。一旦正文、工具调用等实际输出已经提交，或预检字节达到上限，则禁止重试，只转发错误并把流异常写入用量日志，避免重复输出和重复计费；
- 定时推理测试由后台渠道健康检查开关统一控制普通渠道；Codex 和 Claude Code 始终不进入定时或手动批量推理测试，只允许管理员单渠道最小化测试。上游价格同步跳过订阅 OAuth 渠道。

## 3. 管理与模型价格

### 3.1 特权用户令牌管理

- 管理员和 Root 都可以列出、搜索和管理所有管理员及 Root 所属令牌；
- 管理员和 Root 均可在权限校验后查看完整 Key、复制、编辑和删除彼此创建的令牌；
- 普通用户令牌不进入特权令牌列表，管理员和 Root 也不能通过该能力越权管理普通用户令牌；
- 批量删除先完成整批权限校验，再在事务内统一删除，失败时不会部分删除；
- 列表与详情继续使用脱敏 Key，只有通过角色校验的显式取 Key 接口返回完整值。

### 3.2 模型价格编辑

- 可视化编辑器支持按输入单价反算输入、缓存、输出、图片和音频倍率；
- 保存时提交编辑器的最新价格快照，并继续通过 React Hook Form/Zod Schema 校验；
- 多项系统配置统一显示保存状态、成功或失败提示，并在完成后刷新系统配置缓存；
- 内置完成倍率只作为未配置时的默认值，不再锁定任何模型；人工保存和上游价格同步写入的
  `CompletionRatio` 始终优先生效；
- 批量保存会在事务写入前拒绝负数、NaN、无穷值、未知计费模式、无效计费表达式，以及缺少表达式的
  分层计费配置；
- “从上游获取模型”只更新模型列表，不覆盖已有 `model_mapping`。
- `/models/metadata` 的每 Token 输入价格编辑方式会持久化：选择“价格模式（美元/100 万 Token）”后，
  刷新或重新打开模型仍保持该编辑方式。该偏好与 `ModelRatio` 同一模型定价配置原子保存，只影响编辑器
  展示，不改变实际计费公式。
- “上游价格同步”的“选择同步渠道”新增 OpenAI 官方 API 价格和 Claude Code（Anthropic 官方 API）价格两个只读来源。
  它们使用内置、可审查的官方公开 API 每百万 Token 价格目录，按现有模型倍率换算后进入差异选择和
  管理员确认保存流程；不访问 Codex 或 Claude Code OAuth 凭证，也不把订阅月费换算为 Token 价格。
  未有公开 API 定价的模型不会猜测或写入价格。

## 4. 国际化

新增或同步以下语言文件：

```text
web/default/src/i18n/locales/en.json
web/default/src/i18n/locales/zh.json
web/default/src/i18n/locales/zh-TW.json
web/default/src/i18n/locales/fr.json
web/default/src/i18n/locales/ja.json
web/default/src/i18n/locales/ru.json
web/default/src/i18n/locales/vi.json
```

涉及 Codex OAuth 授权、回调 URL、凭据生成、剩余用量、Claude Code 渠道等文案。

## 5. 测试

新增测试：

```text
relay/channel/claude/adaptor_test.go
relay/channel/codex/adaptor_test.go
relay/channel/codex/constants_test.go
relay/channel/codex/models_test.go
relay/channel/api_request_test.go
relay/common/location_privacy_test.go
relay/common/outbound_body_test.go
relay/common/request_passthrough_test.go
controller/channel_upstream_update_test.go
controller/codex_responses_websocket_test.go
controller/option_batch_test.go
controller/option_location_test.go
controller/ratio_sync_test.go
controller/relay_retry_test.go
controller/token_test.go
controller/model_list_test.go
common/credential_redaction_test.go
common/upstream_location_test.go
model/option_location_test.go
service/channel_oauth_policy_test.go
service/http_client_test.go
service/retry_data_policy_test.go
service/codex_oauth_test.go
service/codex_credential_refresh_test.go
service/codex_wham_usage_test.go
service/error_test.go
setting/ratio_setting/model_ratio_test.go
relay/channel/codex/alpha_search_test.go
relay/channel/codex/responses_websocket_test.go
model/channel_multi_key_test.go
web/default/src/lib/localize-error-message.test.ts
```

覆盖内容：

- Claude Code OAuth 请求头生成；
- 删除旧 `x-api-key`；
- Codex 客户端请求头转发；
- Codex Bearer 和 account ID 设置；
- Codex 显式模型限制和账号动态模型获取；
- 订阅 OAuth 安全字段、Header、超时与重试分类；
- 上游价格排除和逐渠道模型巡检调度条件；
- 管理员与 Root 的令牌权限边界及批量删除原子性；
- 内置完成倍率默认值与全部模型可覆盖行为；
- Codex Responses Lite 元数据、工具兼容、普通模式回退和非流式拒绝；
- Alpha search 请求清理和 Responses WebSocket 帧校验、会话渠道固定、SSE 事件转换及
  `426` 回退；
- Codex 客户端模型能力目录及仅对 Codex 渠道声明 WebSocket/搜索能力；
- 全局/渠道请求透传对 Codex、Claude Code 的强制禁用；
- 客户端网络 Header 禁止透传和 OAuth 身份 Header 防覆盖；
- 宿主/代理出口画像发现、位置模式热更新和隐私过滤；
- 自动标签隔离、渠道隔离、供应商隔离、策略组隔离和响应披露 Header；
- 批量模型价格配置事务写入和位置模式运行时选项；
- OAuth refresh token 非轮换响应、并发刷新复用和授权错误分类；
- Codex Wham 用量请求路径、认证 Header、重置请求体及响应大小限制；
- 多 Key 随机与轮询重试排除本请求已使用 Key；
- Codex/Claude Code 凭证级单实例并发租约、HTTP/WebSocket/Alpha Search 统一 503 错误契约、凭证级排除、成功重试 Header 清理和非持久化容量日志策略；
- 上游请求正文、响应正文和 SSE 内容不进入运行日志或数据库错误；客户端仅接收结构化上游错误摘要，不接收网络地址、代理拓扑或 WebSocket 握手正文；
- 前端传输错误、供应商错误、流终止原因和本地 OAuth 凭证容量状态统一显示为简体中文，本地容量错误不再误标为“上游原文”。

## 6. 部署和验证

部署采用“规范一致、配置独立、脚本独立”的方式。每个目标只有一份完整 Compose 和一支明确入口，
不再通过公共 Compose 叠加目标覆盖，也不再通过参数化远程总脚本统一编排：

```text
本机测试  docker-compose.local.yml                       bin/deploy-local.sh
174 正式  docker-compose.server-174.137.56.226.yml       bin/deploy-174.137.56.226.sh
192.168.11.12  docker-compose.server-192.168.11.12.yml   bin/deploy-192.168.11.12.sh
192.168.172.80 docker-compose.server-192.168.172.80.yml  bin/deploy-192.168.172.80.sh
```

四份 Compose 都完整声明 new-api、PostgreSQL、Redis、网络、卷、健康检查和该目标需要的代理，不依赖
`docker-compose.yml` 或已删除的 `docker-compose.deploy.yml`。174 独立包含 HTTPS Caddy，两台 192 独立包含
3000 端口代理，本机只监听 `127.0.0.1:3000`。三份文件采用相同字段顺序和命名规范，但修改其中一份
不会改变其他目标。首次使用新脚本成功校验独立 Compose 后，会删除该服务器部署目录中不再使用的
基础 Compose、覆盖 Compose 和旧公共远程脚本，保留 `.env`、数据、日志、备份和目标配置。173 与 174
均由 Caddy 监听 80/443，公网 IP 的 HTTP 请求重定向到对应 HTTPS 入口，应用容器只保留本机诊断端口。

`bin/deploy-local.sh` 负责本机 `linux/amd64` 构建、镜像版本冒烟检查、本机数据库备份、本机持久测试
环境更新和三个 Relay 路由检查；它只维护 `http://127.0.0.1:3000` 的测试环境，不生成正式发布制品。
每次成功构建后，脚本会清理 Buildx 的普通构建层，仅保留最多 10GB，避免连续部署把 Docker Desktop
磁盘镜像填满。可通过 `DEPLOY_BUILD_CACHE_MAX_USED_SPACE=15GB` 调整上限（旧变量
`DEPLOY_BUILD_CACHE_RESERVED_SPACE` 仍兼容），或设置
`DEPLOY_PRUNE_BUILD_CACHE=false` 暂时关闭清理；`DEPLOY_PRUNE_BUILD_CACHE=all` 会删除所有 Buildx 缓存，
用于一次性紧急释放空间。脚本在构建前后均输出 Docker 存储汇总。默认不删除命名卷；仅在明确设置
`DEPLOY_PRUNE_AUDIT_VOLUMES=true` 时，才会移除未被容器引用的本地 Go 审计/构建缓存卷，不会触碰
PostgreSQL、Redis 或业务数据卷。

三支正式服务器脚本分别配置自己的 SSH、目录、Compose、Caddy、镜像标签、回滚标签和公网验收，
只保留目标参数，并复用 `bin/deploy-common.sh` 中的构建、传输、校验和验收编排。公共脚本会上传受
版本控制的 `bin/deploy-remote.sh`，由它在目标服务器执行镜像装载、数据库备份、切换、回滚和本机
健康检查；该远端激活脚本仍在使用，并未删除。每个正式入口都会在本机以当前源码独立构建自己的 `linux/amd64` 镜像，
构建在服务器外完成，随后只传输该服务器 Compose、Caddyfile 和本次临时压缩镜像；不依赖
`.deploy-artifacts`，不上传源码，也不整体覆盖服务器 `.env`。服务器 `.env` 缺失时直接停止，避免使用
其他服务器默认值。唯一例外是部署策略字段：每次部署都会在原地、原子性地把服务器 `.env` 的显示
时区 `TZ` 强制对齐为统一值（`Asia/Taipei`），并在缺失时补齐 `DEFAULT_LANGUAGE`（默认 `zh-CN`，
已存在则保留），其余键一律原样保留。这样已初始化的服务器不会因为最初写入的旧 `TZ`（例如 `UTC`）
长期覆盖 Compose 的 `${TZ:-...}` 默认值，导致客户端错误信息里的重置时间按 UTC 而非本地时区渲染。

Docker 构建上下文明确排除 `.env`、数据库导出、`data/`、`logs/`、部署制品和本地测试目录，避免凭证
或用户数据进入 BuildKit 缓存与构建上下文。

正式部署按固定轻量步骤执行：构建当前源码并校验 Release 版本；校验本次临时镜像压缩包 SHA-256；确认
已有 PostgreSQL、Redis 健康且不重建它们；保留最近三份数据库备份；校验 Caddy；保存固定 rollback
镜像；只重建 new-api；应用健康
后热加载 Caddy 配置；精确比较远端目标镜像 ID 与运行容器镜像 ID；校验内部和外部启动时间；最后检查 `/v1/responses`、
`/v1/chat/completions`、`/v1/messages` 均返回未授权 401。任一步失败恢复该服务器自己的 rollback 镜像。
成功后会删除仅用于传输的构建镜像标签和无标签旧镜像；当前运行镜像、回滚镜像、数据库、Redis、日志和
最近三份数据库备份均会保留。
部署过程不执行蓝绿切换、候选容器、全工作区同步或自动镜像清理。

三个正式入口互不调用、互不连续部署，必须单独执行：

```bash
./bin/deploy-174.137.56.226.sh
./bin/deploy-192.168.11.12.sh
./bin/deploy-192.168.172.80.sh
```

正式环境参数由各自 `.env` 管理，包括 `HTTP_PROXY`、`HTTPS_PROXY`、`NO_PROXY`、
OAuth 并发和请求间隔。`UPSTREAM_SYSTEM_PROXY_ENABLED`
只影响位置画像选择，真正的上游网络代理仍由 `HTTP_PROXY/HTTPS_PROXY` 控制。

Codex OAuth 的应用参数通过 `CODEX_OAUTH_CLIENT_ID`、`CODEX_OAUTH_REDIRECT_URI`
和 `CODEX_OAUTH_SCOPE` 管理；`CODEX_MODEL_LIST` 可提供管理员预设模型，留空时应在
渠道编辑器中从上游账户动态获取模型。渠道测试会为每次请求生成独立的 session、turn
和 installation 元数据，OAuth 凭证及回调地址在日志输出前统一脱敏。

持久本地测试环境地址为 `http://127.0.0.1:3000`。192 正式服务地址为
`http://192.168.11.12:3000`；174 公网入口为 `https://nextcode.buildtoconnect.com`，并将
`http://174.137.56.226`、`https://174.137.56.226` 永久重定向到该域名。

## 7. HTTPS、日志和重试数据治理

- `174.137.56.226` 公网部署强制启用 `SESSION_COOKIE_SECURE=true`，可信入口默认为 `https://nextcode.buildtoconnect.com`；
- 本地和 `192.168.11.12` 内网部署不强制 Secure Cookie，继续支持 HTTP；
- Redis、支付回调、任务轮询、常规成功响应和流处理日志只记录状态码、字节数与非内容元数据；OAuth
  code/token、Webhook 签名和正文经过专门省略或脱敏；通用上游错误最多读取 1 MB，只保留脱敏、
  限长后的结构化错误信息、Content-Type、响应字节数和上游 request ID，Ollama 异常 SSE 只记录行字节数；
- 渠道编辑器新增“数据治理”，可填写真实供应商、数据区域、保留期、训练策略、重试隔离范围和策略组；
- 订阅 OAuth 自动隔离只允许本次请求首次选中的实际分组内、相同渠道类型且数据策略兼容的候选；`auto` 分组会冻结首次实际命中的分组，后续重试不得跨组；渠道标签不参与自动路由。当前渠道为多 Key 时可在该渠道内换 Key；非订阅渠道仍保留显式标签隔离能力；
- 单请求跨全部凭证最多执行 10 次真实上游尝试；单凭证仍最多 5 次。容量循环只用于当前确实处于本机并发饱和的凭证，已处于限流或用量冷却的凭证直接排除，不进入容量循环；
- `provider` 隔离要求渠道类型、规范化上游端点、显式供应商、区域、保留期和训练策略全部一致；
- `policy_group` 隔离要求显式供应商、区域、保留期、训练策略和策略组全部一致；配置不完整或非法时运行期自动回退到 `channel`，失败关闭；
- 已尝试的单 Key 渠道不会再次入选；多 Key 渠道按请求记录 `(channel_id, key_index)`，随机和轮询模式
  都会排除已经使用的 Key，所有启用 Key 用尽后停止选择该渠道；内存缓存和数据库直查路径使用同一
  候选过滤边界；找不到合规备用渠道时保留并返回经过治理的上游错误摘要；
- 响应通过 `X-Relay-Upstream-Provider`、`X-Relay-Attempt`、`X-Relay-Retry-Count`、`X-Relay-Retry-Isolation`、`X-Relay-Data-Region`、`X-Relay-Data-Retention` 和 `X-Relay-Data-Training` 披露实际处理策略，CORS 同步暴露这些 Header。

新增回归测试：`service/retry_data_policy_test.go`，覆盖默认隔离、多 Key 渠道内重试、配置不完整时失败关闭、相同供应商边界、策略组边界和响应头披露。

## 8. 上游位置与客户端 IP 隐私

- JSON 模型请求在最终出站边界执行位置隐私策略，常见协议位置字段、参数覆盖和渠道正文直通均纳入
  过滤；转换请求和渠道正文直通均先以固定内存流式扫描 JSON 对象键，仅在发现位置或网络候选键时才
  整体解析过滤。扫描支持大小写、连字符、点号、下划线、跨缓冲区和 `\uXXXX` 转义键；无候选键的
  大请求直接复用原 `BodyStorage`，避免磁盘缓存正文重新读入堆内存并放大；
- `UPSTREAM_LOCATION_MODE=strip` 为默认值，删除 OpenAI、Claude、Gemini 协议中的用户位置、经纬度及 metadata/client_metadata 中的 IP 和位置字段；
- `UPSTREAM_LOCATION_MODE=auto` 根据真实网络路径选择画像：渠道配置代理或 `UPSTREAM_SYSTEM_PROXY_ENABLED=true` 时使用 VPN/代理出口画像，否则使用宿主网络出口画像；两套画像可分别由 `UPSTREAM_EGRESS_LOCATION_*` 和 `UPSTREAM_HOST_LOCATION_*` 显式配置；
- `UPSTREAM_LOCATION_DISCOVERY_ENABLED=true` 时，服务启动会分别通过直连路径、系统代理/VPN 以及每个渠道独立代理检查 ChatGPT、Claude 连通性；每个不同渠道代理使用独立的内存出口画像，不再共享系统代理画像。连通后通过 Cloudflare trace 获取实际公网 IP，并使用 `ipwho.is` 补充国家、地区、城市、时区和经纬度；探测失败不会放行客户端位置；
- `UPSTREAM_LOCATION_MODE=egress` 仅在客户端原本提供位置的协议位置上，改写为通过 `UPSTREAM_EGRESS_LOCATION_*` 配置的最终 VPN/代理出口位置，而不是宿主机物理位置；配置缺失时失败关闭并删除原字段；旧的 `relay` 和 `RELAY_LOCATION_*` 仍作为兼容别名；
- `UPSTREAM_LOCATION_MODE=client` 明确允许客户端正文位置透传，但客户端真实 IP 无论位于 Header 还是协议 metadata 都仍禁止发送；
- `Forwarded`、`X-Forwarded-For`、`X-Real-IP`、`CF-Connecting-IP`、`CF-IPCountry`、CloudFront Viewer 和 Vercel IP 位置 Header 被加入强制出站禁止名单，通配、正则和显式 Header Override 均不能绕过；
- 中转服务器不伪造或添加自己的 `X-Forwarded-For`，模型服务商通过连接来源自然看到中转服务器出口 IP；
- 国内宿主机通过 VPN、SOCKS5 或 HTTP 代理访问上游时，上游自然看到代理出口 IP；代理必须禁止自行追加客户端或宿主机的 `Forwarded`/`X-Forwarded-For`，否则该 Header 是在离开应用后由代理新增，应用无法再次清理；
- Root 控制面板的“系统设置 → 操作设置 → 上游隐私”可保存位置转发模式，并展示系统级 VPN/TUN 状态、自动选择规则、宿主画像和代理出口画像；模式保存到数据库并在运行时热更新、多节点按既有 Option 同步周期传播，`UPSTREAM_LOCATION_MODE` 仅作为数据库尚未配置时的启动默认值；画像数据来自当前进程加载的显式环境变量及启动探测结果，不暴露代理 URL 或凭证；
- 同一页面新增“刷新”操作，对应仅限 Root 调用的
  `POST /api/option/upstream-location/refresh`。它即使自动启动探测被关闭也会执行，只使用
  固定的 ChatGPT、Claude、Cloudflare trace 和 IP 地理定位地址，调用方不能指定探测 URL；
  直连路径始终探测，系统代理/VPN 已启用时额外探测代理出口路径；
- 刷新过程使用互斥锁，已有刷新时返回 `409 Conflict` 和 `Retry-After: 2`；只要任一路径更新
  成功即返回最新画像及另一路径的警告，全部失败返回 `502` 并保留上一次成功画像。刷新结果仅保存在
  当前进程内存，重启后仍以显式环境变量和启动探测重建；
- `UPSTREAM_HOST_PUBLIC_IP` 和 `UPSTREAM_EGRESS_PUBLIC_IP` 仅作为 Root 控制面板中的出口 IP 展示值，不会写入模型请求；真实网络来源仍由 TCP/TLS 出口决定；
- 自动探测只发送无凭证的连通性请求，不携带客户端请求、OAuth Token 或模型数据；公网出口 IP 会被 Cloudflare 和 `ipwho.is` 看到。对第三方 IP 地理定位服务有合规限制时，可设置 `UPSTREAM_LOCATION_DISCOVERY_ENABLED=false` 并完全使用显式画像；
- 三套独立 Compose 均按同一规范声明隐私环境变量，默认使用 `UPSTREAM_LOCATION_MODE=strip`、
  `UPSTREAM_LOCATION_DISCOVERY_ENABLED=true` 和 8 秒总探测超时。正式部署脚本不创建或改写服务器
  `.env`；每台服务器可独立覆盖这些值。安全默认仍为删除客户端位置，探测画像不会自动改变转发模式。

## 9. 审查结论与剩余风险

### 9.1 已解决：通用上游错误和 Ollama 异常流正文泄露

- `RelayErrorHandler` 已移除无效的 `showBodyWhenFail` 参数，并限制最多读取 1 MB；
- 原始响应正文不再写入运行日志、数据库错误或客户端错误；
- 保留脱敏和限长后的供应商结构化错误信息、状态码、Content-Type、响应字节数及上游 request ID；
- Ollama SSE 解码失败仅记录行字节数。

### 9.2 已解决：位置隐私 marker 快路径漏检和大请求内存放大

原大小写敏感 marker 预扫描已替换为常量内存 JSON 对象键扫描器。它会解码转义键并使用与完整过滤器
相同的规范化候选键；转换请求同样先扫描，正文直通只有命中候选键才调用 `BodyStorage.Bytes()` 和完整
JSON 解析。测试覆盖顶层字段、大小写、连字符、点号、下划线、跨 32KB 缓冲区、转义键、读取位置恢复
及仅含普通 metadata 的大正文零复制路径。

### 9.3 订阅 OAuth 兼容实现不能等同于官方公开 API 合规

Codex 路径使用 `chatgpt.com/backend-api/codex`、官方客户端 OAuth client ID、`originator` 和 `x-openai-internal-*` Header；Claude Code 路径使用 `claude-cli`、Claude Code 专用 beta Header 和固定 CLI identity system prompt。这些均属于客户端兼容或内部接口行为，不是代码库内能够证明已获供应商授权的公开 API 合同。

部署前仍需取得 OpenAI/Anthropic 对账号共享、中转、自动化、转售、内部端点和订阅 OAuth Token 使用方式的明确授权。否则即使技术请求成功，也仍存在 401/403、组织禁用、接口变更和账号限制风险。

### 9.4 已解决：单项系统配置忽略数据库写入错误

`UpdateOption` 统一复用事务型 `UpdateOptionsBulk`。`FirstOrCreate`、`Save` 或事务提交失败会直接返回，
只有数据库提交成功后才更新运行时 Option 状态。

### 9.5 已解决：多 Key 渠道重试重复选择同一个 Key

重试边界已按请求记录 `(channel_id, key_index)`。随机、轮询和默认模式都会排除本请求已经使用的
启用 Key；所有启用 Key 用尽后，该渠道不再进入候选列表。

### 9.6 部署版本包含源码标识与编译时间

部署脚本将 `APP_VERSION` 构建为 `git describe --tags --always --dirty --abbrev=12` 加
`+build.YYYYMMDDTHHMMSSZ`。`/system-settings/operations/update-checker` 的“当前版本”因此可同时
显示本地源码提交/dirty 状态与 UTC 编译时间；镜像、运行服务和远端部署状态使用同一版本字符串进行校验。
更新检查会忽略 `+build.*` 后缀，仅在源码标签恰好等于 GitHub Release tag 时显示“已是最新版本”。

### 9.7 已解决：批量模型价格保存缺少语义校验

批量接口在事务写入前验证有限非负价格/倍率、合法计费模式、非空模型名和表达式，并对
`billing_expr` 执行 smoke test；分层计费缺少对应表达式时整批拒绝，不会部分写入。

### 9.8 Anthropic OAuth 组织权限

如果上游返回 `OAuth authentication is currently not allowed for this organization.`，表示 Token 已被识别，但所属 Anthropic 组织不允许 OAuth API 访问。该限制无法由网关 Header 或重试逻辑绕过。

### 9.9 已确认的审计排除项

以下两项已经由项目负责人确认，不再作为后续代码审计、发布阻断或待修复问题重复报告：

1. Git 历史中的生产数据库备份属于已知历史风险。当前工作树已删除相关文件，但不再要求本项目通过
   重写既有 Git 历史解决，也不在后续审计中重复提出凭据轮换或历史清理要求；
2. 普通管理员与 Root 可以互相列出、查看完整 Key、编辑和删除对方创建的特权 API Key，这是明确的
   业务授权。现有 `getManageableToken` 行为及对应回归测试按产品策略保留，不再按角色层级越权报告。

上述排除仅针对这两个已确认事项，不放宽其他凭据日志脱敏、普通用户令牌隔离、数据库备份进入工作树、
认证授权或角色权限问题的审计标准。

历史背景：

`backups/nextcode-20260715/local-before-nextcode.dump` 和
`backups/nextcode-20260715/remote-new-api.sql` 已被提交到 Git 历史。后者包含
`channels`、`tokens`、`options`、`users` 与 `custom_oauth_providers` 等表的导出，字段范围内包含
渠道密钥、API Key、OAuth client secret、访问令牌、用户认证和运行配置等敏感数据的可能载体。

## 10. 架构维护记录

本说明记录本次功能实际行为；当前模块边界、不可破坏约束和后续治理项另行维护在：

```text
docs/architecture/overview.md
docs/architecture/invariants.md
docs/architecture/health.md
docs/adr/README.md
```

这些记录定义后续需求的处理方式：局部修复直接验证；跨模块功能先确认既有能力、单一状态所有者和
失败/回滚行为；新增工作流、持久化状态、认证、计费或重试策略前先建立 ADR。此次梳理确认的下一批
结构治理重点已落实 OAuth 尝试状态单一所有者与路由可靠性配置的事务批量保存；后续重点为部署脚本职责
分层、`RelayInfo` 按稳定概念拆分，以及路由可靠性表单的 Schema 单一来源。
