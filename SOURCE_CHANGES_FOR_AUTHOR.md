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
串行执行，避免 refresh token 轮换竞争。用量查询只在上游真实返回 `401` 时刷新，`403` 作为账号或
组织权限问题直接返回，不再无效刷新。

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

新增客户端请求头透传：

```text
session-id
thread-id
x-client-request-id
x-codex-beta-features
x-codex-parent-thread-id
x-codex-turn-state
x-codex-turn-metadata
x-codex-window-id
x-openai-memgen-request
x-openai-subagent
x-responsesapi-include-timing-metrics
```

认证、账号、`originator`、`user-agent`、`content-type` 等身份与协议 Header
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
- 与客户端会话一致的 session/thread/turn/window 元数据；客户端未提供时由服务端生成。

Lite 请求还必须满足：

- `stream: true`；
- `store: false`；
- 工具仅允许 `function`、`custom`，以及 `execution: "client"` 的
  `tool_search`。HTTP、SSE 和 WebSocket 请求会在最终出站前递归过滤
  `namespace`、`web_search`、`image_generation`、托管 `tool_search` 等 Lite
  不支持的 hosted 工具，同时保留客户端函数、自定义工具和客户端工具搜索。

这些修改用于解决 Codex Lite 对请求头、流式响应和 reasoning context 的严格校验。

### 1.5 Responses Lite 搜索与 WebSocket

新增客户端入口：

```text
POST /v1/alpha/search
POST /backend-api/codex/alpha/search
GET /v1/responses  (WebSocket Upgrade)
```

- Alpha search 通过当前 API Key 的分组和模型规则选择 Codex OAuth 渠道，转发至
  `/backend-api/codex/alpha/search`，恢复 Responses Lite 的 `web.run` 搜索；
- 搜索请求移除仅用于本地路由的 `prompt_cache_key` 和
  `prompt_cache_retention`，保留会话及 actor authorization Header；
- Responses WebSocket 会话固定同一 OAuth 渠道和模型，使用上游
  `responses_websockets=2026-02-06` 协议；
- `/v1/models?client_version=...` 返回 Codex 客户端兼容目录，并仅为实际由 Codex
  渠道承载的模型声明 `prefer_websockets: true` 和搜索能力；
- `response.create` 和 `response.append` 均进入既有鉴权、模型限制、计费、隐私和
  错误处理链；同一会话固定渠道，避免 OAuth 身份在会话中途漂移；
- 上游 WebSocket 事件按客户端协议实时返回；上游明确返回 `426 Upgrade Required`
  时自动回退 HTTP/SSE，不会因上游暂未开放 WebSocket 而中断请求。
- WebSocket 帧和转换后的请求体统一使用 `MAX_REQUEST_BODY_MB` 限制，默认 `128 MB`，
  不再单独使用固定的 `16 MB` 限制；超限返回 `413 Request Entity Too Large`，并标记为
  不可重试，避免大请求被重复发送到备用渠道。
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

Codex 渠道测试强制使用 stream 模式，定时自动测试会跳过订阅 OAuth 渠道。
适配器仅在配置 `CODEX_MODEL_LIST` 时返回该显式限制列表；管理端手动抓取模型和上游模型巡检调用
`/backend-api/codex/models`，按当前 ChatGPT 账号返回实际可用模型。模型抓取继承请求或任务
Context，并受到订阅 OAuth 超时限制，不再使用不可取消的 `context.Background()`。
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
- 每个渠道默认最多 5 个并发请求，相邻请求启动间隔默认 750ms；
- 等待上游首个响应头默认最多 30 秒；Transport 超时返回 `504` 和 `Retry-After`，便于客户端与真正的应用内部 `500` 区分；
- 本地并发保护触发时返回可重试的 503，以便切换到备用订阅渠道；
- 上游 5xx、504、524 可进入重试判断，但默认只允许当前多 Key 渠道换 Key 重试；只有管理员显式配置相同供应商端点或相同数据策略组后，才允许跨渠道重试；
- 定时推理测试和上游价格同步跳过订阅 OAuth 渠道。

## 3. 管理与模型价格

### 3.1 特权用户令牌管理

- Root 可以列出、搜索和管理 Root/管理员所属令牌；
- 普通管理员只能管理自己的令牌，不能读取或操作 Root、同级管理员的令牌；
- 批量删除先完成整批权限校验，再在事务内统一删除，失败时不会部分删除；
- 列表与详情继续使用脱敏 Key，只有通过角色校验的显式取 Key 接口返回完整值。

### 3.2 模型价格编辑

- 可视化编辑器支持按输入单价反算输入、缓存、输出、图片和音频倍率；
- 保存时提交编辑器的最新价格快照，并继续通过 React Hook Form/Zod Schema 校验；
- 多项系统配置统一显示保存状态、成功或失败提示，并在完成后刷新系统配置缓存；
- GPT-5.4 及后续模型的完成倍率允许自定义，其他硬编码锁定模型继续使用内置倍率；
- 批量保存会在事务写入前拒绝负数、NaN、无穷值、未知计费模式、无效计费表达式，以及缺少表达式的
  分层计费配置；
- “从上游获取模型”只更新模型列表，不覆盖已有 `model_mapping`。

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
- 上游价格/模型巡检排除和任务启用条件；
- 管理员与 Root 的令牌权限边界及批量删除原子性；
- 硬编码完成倍率锁定与 GPT-5.4 可配置行为；
- Codex Responses Lite 元数据、工具兼容、普通模式回退和非流式拒绝；
- Alpha search 请求清理和 Responses WebSocket 帧校验、会话渠道固定、SSE 事件转换及
  `426` 回退；
- Codex 客户端模型能力目录及仅对 Codex 渠道声明 WebSocket/搜索能力；
- 全局/渠道请求透传对 Codex、Claude Code 的强制禁用；
- 客户端网络 Header 禁止透传和 OAuth 身份 Header 防覆盖；
- 宿主/代理出口画像发现、位置模式热更新和隐私过滤；
- 默认渠道隔离、供应商隔离、策略组隔离和响应披露 Header；
- 批量模型价格配置事务写入和位置模式运行时选项；
- OAuth refresh token 非轮换响应和授权错误分类；
- 多 Key 随机与轮询重试排除本请求已使用 Key；
- 上游错误正文不进入日志、数据库错误或客户端响应；
- 前端传输错误、供应商错误和流终止原因统一显示为简体中文。

## 6. 部署和验证

提供三套部署入口：

```text
bin/deploy-local.sh
bin/deploy-174.137.56.226.sh
bin/deploy-192.168.11.12.sh
```

部署行为：

- `docker-compose.deploy.yml` 统一维护应用、PostgreSQL、Redis 和 OAuth 保护参数，三套目标 Compose 只保留端口、Caddy 等目标差异；
- `bin/deploy-common.sh` 统一版本号、Buildx、镜像平台检查和 `.env` 初始化，远程脚本复用同一套环境初始化逻辑；
- `bin/deploy-remote.sh` 统一 174、192 的远程构建发布流程，两个服务器脚本只声明目标地址、目录、端口和额外服务；
- 公网服务器部署目标已从 `104.128.92.169` 迁移至 `174.137.56.226`：Caddy、Compose 文件、
  远程部署脚本与兼容入口均已改名并指向新地址；本地和 192 服务监听 3000，174 由 Caddy 对外
  提供 `nextcode.buildtoconnect.com`；
- 本地、174、192 服务时区统一为 UTC，与 Codex / Claude Code 上游时间戳和配额窗口保持一致；
- Compose 默认设置 `SUBSCRIPTION_OAUTH_RESPONSE_HEADER_TIMEOUT=30`；部署脚本仅将旧默认值 `120` 自动迁移为 `30`，保留管理员自定义值；
- Compose 与部署脚本默认补齐 `MAX_REQUEST_BODY_MB=128`；可在三套部署的 `.env` 中按 MB 调整，
  同时作用于普通 HTTP 请求和 Codex Responses WebSocket 请求；
- Docker 构建支持 GOPROXY 主备切换；
- 运行镜像只保留静态服务二进制、CA、时区数据库和健康检查所需工具，不携带 Go 编译器；
- `APP_VERSION` 按部署要求固定取最近 Git Release tag，源码补丁构建不会改变页面显示版本；镜像归档
  SHA-256 和镜像 ID 用于确认同一 Release 版本下的实际部署产物；
- 镜像传输前后强制比较压缩包 SHA-256，服务器内部再比较已加载镜像与运行容器的镜像 ID，不一致立即失败；
- 构建后默认将 Buildx 缓存控制在 20GB 内，并清理带有 new-api 镜像标签的无引用旧镜像；
- 部署脚本等待容器健康，并再次请求 `/api/status`。

部署缓存清理可通过 `DEPLOY_PRUNE_BUILD_CACHE`、`DEPLOY_BUILDX_CACHE_MAX_USED_SPACE`
和 `DEPLOY_PRUNE_PROJECT_IMAGES` 调整。上游模型巡检总开关为
`CHANNEL_UPSTREAM_MODEL_UPDATE_TASK_ENABLED`；部署脚本会在目标 `.env` 中补齐该配置，
设置为 `false` 后即使渠道自身启用了巡检也不会执行。

Codex OAuth 的应用参数通过 `CODEX_OAUTH_CLIENT_ID`、`CODEX_OAUTH_REDIRECT_URI`
和 `CODEX_OAUTH_SCOPE` 管理；`CODEX_MODEL_LIST` 可提供管理员预设模型，留空时应在
渠道编辑器中从上游账户动态获取模型。渠道测试会为每次请求生成独立的 session、turn
和 installation 元数据，OAuth 凭证及回调地址在日志输出前统一脱敏。

本地服务地址为 `http://127.0.0.1:3000`，192 服务地址为
`http://192.168.11.12:3000`；174 服务器保留宿主机本地诊断映射
`http://127.0.0.1:3001`，公网入口为 `https://nextcode.buildtoconnect.com`。

## 7. HTTPS、日志和重试数据治理

- `174.137.56.226` 公网部署强制启用 `SESSION_COOKIE_SECURE=true`，可信入口默认为 `https://nextcode.buildtoconnect.com`；Caddy 将公网 IP 的 HTTP 请求永久重定向到该 HTTPS 域名并发送 HSTS；
- 本地和 `192.168.11.12` 内网部署不强制 Secure Cookie，继续支持 HTTP；
- Redis、支付回调、任务轮询、常规成功响应和流处理日志只记录状态码、字节数与非内容元数据；OAuth
  code/token、Webhook 签名和正文经过专门省略或脱敏；通用上游错误最多读取 1 MB，只保留脱敏、
  限长后的结构化错误信息、Content-Type、响应字节数和上游 request ID，Ollama 异常 SSE 只记录行字节数；
- 渠道编辑器新增“数据治理”，可填写真实供应商、数据区域、保留期、训练策略、重试隔离范围和策略组；
- 默认 `channel` 隔离不把提示词发送到其他渠道；当前渠道为多 Key 时才可在该渠道内换 Key；
- `provider` 隔离要求渠道类型、规范化上游端点、显式供应商、区域、保留期和训练策略全部一致；
- `policy_group` 隔离要求显式供应商、区域、保留期、训练策略和策略组全部一致；配置不完整或非法时运行期自动回退到 `channel`，失败关闭；
- 已尝试的单 Key 渠道不会再次入选；多 Key 渠道按请求记录 `(channel_id, key_index)`，随机和轮询模式
  都会排除已经使用的 Key，所有启用 Key 用尽后停止选择该渠道；内存缓存和数据库直查路径使用同一
  候选过滤边界；找不到合规备用渠道时保留并返回经过治理的上游错误摘要；
- 响应通过 `X-Relay-Upstream-Provider`、`X-Relay-Attempt`、`X-Relay-Retry-Count`、`X-Relay-Retry-Isolation`、`X-Relay-Data-Region`、`X-Relay-Data-Retention` 和 `X-Relay-Data-Training` 披露实际处理策略，CORS 同步暴露这些 Header。

新增回归测试：`service/retry_data_policy_test.go`，覆盖默认隔离、多 Key 渠道内重试、配置不完整时失败关闭、相同供应商边界、策略组边界和响应头披露。

## 8. 上游位置与客户端 IP 隐私

- JSON 模型请求在最终出站边界执行位置隐私策略，常见协议位置字段、参数覆盖和渠道正文直通均纳入
  过滤；隐私过滤不再依赖大小写敏感 marker 快路径，每个 JSON 出站请求都会解析，并使用规范化键
  匹配大小写、连字符、点号、下划线及嵌套数组中的敏感网络字段；
- `UPSTREAM_LOCATION_MODE=strip` 为默认值，删除 OpenAI、Claude、Gemini 协议中的用户位置、经纬度及 metadata/client_metadata 中的 IP 和位置字段；
- `UPSTREAM_LOCATION_MODE=auto` 根据真实网络路径选择画像：渠道配置代理或 `UPSTREAM_SYSTEM_PROXY_ENABLED=true` 时使用 VPN/代理出口画像，否则使用宿主网络出口画像；两套画像可分别由 `UPSTREAM_EGRESS_LOCATION_*` 和 `UPSTREAM_HOST_LOCATION_*` 显式配置；
- `UPSTREAM_LOCATION_DISCOVERY_ENABLED=true` 时，服务启动会分别通过直连路径和已启用的系统代理/VPN 路径检查 ChatGPT、Claude 连通性；两者均可建立 HTTP 连接后，通过 Cloudflare trace 获取该路径的实际公网 IP，并使用 `ipwho.is` 补充国家、地区、城市、时区和经纬度。显式环境变量始终优先，探测只补充空字段；探测失败不会放行客户端位置；
- `UPSTREAM_LOCATION_MODE=egress` 仅在客户端原本提供位置的协议位置上，改写为通过 `UPSTREAM_EGRESS_LOCATION_*` 配置的最终 VPN/代理出口位置，而不是宿主机物理位置；配置缺失时失败关闭并删除原字段；旧的 `relay` 和 `RELAY_LOCATION_*` 仍作为兼容别名；
- `UPSTREAM_LOCATION_MODE=client` 明确允许客户端正文位置透传，但客户端真实 IP 无论位于 Header 还是协议 metadata 都仍禁止发送；
- `Forwarded`、`X-Forwarded-For`、`X-Real-IP`、`CF-Connecting-IP` 以及常见 CDN/代理客户端 IP Header 被加入强制出站禁止名单，通配、正则和显式 Header Override 均不能绕过；
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
- 三套部署入口共用 `docker-compose.deploy.yml` 的隐私环境变量，部署脚本会为已有或新建 `.env` 补齐 `UPSTREAM_LOCATION_MODE=strip`、`UPSTREAM_LOCATION_DISCOVERY_ENABLED=true` 和 8 秒总探测超时。安全默认仍为删除客户端位置，探测画像不会自动改变转发模式。

## 9. 审查结论与剩余风险

### 9.1 已解决：通用上游错误和 Ollama 异常流正文泄露

- `RelayErrorHandler` 已移除无效的 `showBodyWhenFail` 参数，并限制最多读取 1 MB；
- 原始响应正文不再写入运行日志、数据库错误或客户端错误；
- 保留脱敏和限长后的供应商结构化错误信息、状态码、Content-Type、响应字节数及上游 request ID；
- Ollama SSE 解码失败仅记录行字节数。

### 9.2 已解决：位置隐私 marker 快路径漏检

已删除大小写敏感的 marker 预扫描。所有 JSON 出站请求统一解析后过滤，测试覆盖顶层字段、大小写、
连字符、点号、下划线、嵌套对象和嵌套数组。

### 9.3 订阅 OAuth 兼容实现不能等同于官方公开 API 合规

Codex 路径使用 `chatgpt.com/backend-api/codex`、官方客户端 OAuth client ID、`originator` 和 `x-openai-internal-*` Header；Claude Code 路径使用 `claude-cli`、Claude Code 专用 beta Header 和固定 CLI identity system prompt。这些均属于客户端兼容或内部接口行为，不是代码库内能够证明已获供应商授权的公开 API 合同。

部署前仍需取得 OpenAI/Anthropic 对账号共享、中转、自动化、转售、内部端点和订阅 OAuth Token 使用方式的明确授权。否则即使技术请求成功，也仍存在 401/403、组织禁用、接口变更和账号限制风险。

### 9.4 已解决：单项系统配置忽略数据库写入错误

`UpdateOption` 统一复用事务型 `UpdateOptionsBulk`。`FirstOrCreate`、`Save` 或事务提交失败会直接返回，
只有数据库提交成功后才更新运行时 Option 状态。

### 9.5 已解决：多 Key 渠道重试重复选择同一个 Key

重试边界已按请求记录 `(channel_id, key_index)`。随机、轮询和默认模式都会排除本请求已经使用的
启用 Key；所有启用 Key 用尽后，该渠道不再进入候选列表。

### 9.6 有意行为：构建版本固定显示 Git Release

按部署需求，`APP_VERSION` 只使用最近 Git Release tag。修改源码并重新编译不会改变页面显示版本；
同一 Release 下的产物差异通过镜像归档 SHA-256、加载后的镜像 ID 和运行容器镜像 ID 校验，不向
`APP_VERSION` 添加 commit、dirty 标记或构建时间。

### 9.7 已解决：批量模型价格保存缺少语义校验

批量接口在事务写入前验证有限非负价格/倍率、合法计费模式、非空模型名和表达式，并对
`billing_expr` 执行 smoke test；分层计费缺少对应表达式时整批拒绝，不会部分写入。

### 9.8 Anthropic OAuth 组织权限

如果上游返回 `OAuth authentication is currently not allowed for this organization.`，表示 Token 已被识别，但所属 Anthropic 组织不允许 OAuth API 访问。该限制无法由网关 Header 或重试逻辑绕过。

### 9.9 已提交的数据库备份包含生产敏感数据

`backups/nextcode-20260715/local-before-nextcode.dump` 和
`backups/nextcode-20260715/remote-new-api.sql` 已被提交到 Git 历史。后者包含
`channels`、`tokens`、`options`、`users` 与 `custom_oauth_providers` 等表的导出，字段范围内包含
渠道密钥、API Key、OAuth client secret、访问令牌、用户认证和运行配置等敏感数据的可能载体。

这是高优先级源码安全问题：应立即停止在仓库中保留此类备份，将两个文件从当前分支和所有可访问 Git
历史中移除，加入忽略规则，并轮换可能已暴露的渠道 Key、API Key、OAuth client secret、用户访问
令牌及数据库凭据。仅删除工作树文件不足以消除已克隆、镜像或缓存中的历史泄露风险。
