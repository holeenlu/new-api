# New API 源码修改需求与实现说明

本文档整理当前工作区中针对 **ChatGPT/Codex 订阅渠道** 与 **Claude Code OAuth 渠道** 的全部功能修改，供提交给项目作者审阅。

## 1. ChatGPT/Codex 订阅渠道

### 1.1 固定可用模型

文件：`relay/channel/codex/constants.go`

将 Codex 渠道模型列表限制为：

```text
gpt-5.6-sol
gpt-5.6-terra
gpt-5.6-luna
gpt-5.5
```

原先的 `gpt-5.4`、`gpt-5.4 mini` 以及其他自动生成的模型和 compact 后缀模型不再出现在 Codex 模型列表中。

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
`x-openai-internal-codex-responses-lite` 不接受客户端透传，只在 Lite 渠道测试中由服务端补充。

对于 `gpt-5.6-*` 模型的渠道测试请求，自动补充：

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
- 固定的 session/thread/turn/window 元数据。

这些修改用于解决 Codex Lite 对请求头、流式响应和 reasoning context 的严格校验。

### 1.5 Responses DTO 扩展

文件：`dto/openai_request.go`

新增字段：

```go
ClientMetadata json.RawMessage `json:"client_metadata,omitempty"`
```

并为 `Reasoning` 增加：

```go
Context json.RawMessage `json:"context,omitempty"`
```

### 1.6 渠道测试和模型同步

文件：

- `controller/channel-test.go`
- `controller/channel.go`
- `controller/channel_upstream_update.go`

Codex 渠道测试强制使用 stream 模式，定时自动测试会跳过订阅 OAuth 渠道。
适配器的默认模型列表保持为本地固定 `ModelList`；管理端手动抓取模型和上游模型巡检则调用
`/backend-api/codex/models`，按当前 ChatGPT 账号返回实际可用模型。模型抓取继承请求或任务
Context，并受到订阅 OAuth 超时限制，不再使用不可取消的 `context.Background()`。
上游价格同步明确排除 Codex 与 Claude Code OAuth 渠道。

### 1.7 Codex 使用量界面

文件：`web/default/src/features/channels/components/dialogs/codex-usage-dialog.tsx`

用量卡片从显示“Used”改为显示“Remaining”，进度条和百分比改为剩余百分比：

```text
remaining = 100 - used_percent
```

并对百分比进行边界限制，避免出现负数或超过 100 的显示。

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
- 等待上游首个响应头默认最多 30 秒；
- 本地并发保护触发时返回可重试的 503，以便切换到备用订阅渠道；
- 上游 5xx、504、524 可进入渠道重试和故障转移，确定性的客户端错误不重试；
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
controller/channel_upstream_update_test.go
controller/ratio_sync_test.go
controller/relay_retry_test.go
controller/token_test.go
service/channel_oauth_policy_test.go
service/http_client_test.go
setting/ratio_setting/model_ratio_test.go
```

覆盖内容：

- Claude Code OAuth 请求头生成；
- 删除旧 `x-api-key`；
- Codex 客户端请求头转发；
- Codex Bearer 和 account ID 设置；
- Codex 固定模型列表和账号模型获取；
- 订阅 OAuth 安全字段、Header、超时与重试分类；
- 上游价格/模型巡检排除和任务启用条件；
- 管理员与 Root 的令牌权限边界及批量删除原子性；
- 硬编码完成倍率锁定与 GPT-5.4 可配置行为。

## 6. 部署和验证

提供三套部署入口：

```text
bin/deploy-local.sh
bin/deploy-104.128.92.169.sh
bin/deploy-192.168.11.12.sh
```

部署行为：

- `docker-compose.deploy.yml` 统一维护应用、PostgreSQL、Redis 和 OAuth 保护参数，三套目标 Compose 只保留端口、Caddy 等目标差异；
- `bin/deploy-common.sh` 统一版本号、Buildx、镜像平台检查和 `.env` 初始化，远程脚本复用同一套环境初始化逻辑；
- `bin/deploy-remote.sh` 统一 104、192 的远程构建发布流程，两个服务器脚本只声明目标地址、目录、端口和额外服务；
- 本地和 192 服务监听 3000，104 由 Caddy 对外提供 `nextcode.buildtoconnect.com`；
- 本地、104、192 服务时区统一为 UTC，与 Codex / Claude Code 上游时间戳和配额窗口保持一致；
- Compose 默认设置 `SUBSCRIPTION_OAUTH_RESPONSE_HEADER_TIMEOUT=30`；
- Docker 构建支持 GOPROXY 主备切换；
- 运行镜像只保留静态服务二进制、CA、时区数据库和健康检查所需工具，不携带 Go 编译器；
- 每次构建版本包含 Git describe 结果和 UTC 构建时间，可区分同一 tag 上的不同补丁；
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
`http://192.168.11.12:3000`；104 服务器保留宿主机本地诊断映射
`http://127.0.0.1:3001`，公网入口为 `https://nextcode.buildtoconnect.com`。

## 7. 已知限制和建议

### 7.1 Anthropic OAuth 组织权限

如果上游返回：

```text
OAuth authentication is currently not allowed for this organization.
```

表示 Token 已被上游识别，但所属 Anthropic 组织不允许 OAuth API 访问。该错误不是网关请求头错误，无法由本项目绕过。建议对该错误增加专门提示，而不是统一显示为 `unknown_error`。

### 7.2 建议作者审阅

1. Codex OAuth client ID、redirect URI 和 scope 是否应由配置项管理；
2. Codex 渠道测试使用的固定 session/turn 元数据是否应改为每次动态生成；
3. Codex 模型列表是否应由上游能力或管理员配置，而不是硬编码；
4. Claude Code OAuth 请求头生成逻辑是否应与模型抓取逻辑共用同一个 helper；
5. OAuth Token、refresh token 和 callback URL 在日志中必须避免明文输出；
6. 对 OAuth 403、401、模型不支持等上游错误增加明确的错误分类和前端提示。
