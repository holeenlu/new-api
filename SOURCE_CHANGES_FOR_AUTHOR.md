# New API 源码修改需求与实现说明

本文档整理当前工作区中针对 **ChatGPT/Codex 订阅渠道** 与 **Claude Code OAuth 渠道** 的全部功能修改，供提交给项目作者审阅。

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
  `tool_search`。包含 `namespace`、托管 `tool_search` 或其他服务端工具时，
  保留原始工具结构并自动回退到普通 Responses 模式，不会静默删除或改写工具。

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
适配器仅在配置 `CODEX_MODEL_LIST` 时返回该显式限制列表；管理端手动抓取模型和上游模型巡检调用
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

## 7. HTTPS、日志和重试数据治理

- `104.128.92.169` 公网部署强制启用 `SESSION_COOKIE_SECURE=true`，可信入口默认为 `https://nextcode.buildtoconnect.com`；Caddy 将公网 IP 的 HTTP 请求永久重定向到该 HTTPS 域名并发送 HSTS；
- 本地和 `192.168.11.12` 内网部署不强制 Secure Cookie，继续支持 HTTP；
- 请求正文、上游响应正文、SSE 事件、OAuth code/token、Webhook 签名和正文不写入运行日志，诊断日志仅保留状态码、字节数和非内容元数据；
- 渠道编辑器新增“数据治理”，可填写真实供应商、数据区域、保留期、训练策略、重试隔离范围和策略组；
- 默认 `channel` 隔离不把提示词发送到其他渠道；当前渠道为多 Key 时才可在该渠道内换 Key；
- `provider` 隔离要求渠道类型、规范化上游端点、显式供应商、区域、保留期和训练策略全部一致；
- `policy_group` 隔离要求显式供应商、区域、保留期、训练策略和策略组全部一致；配置不完整或非法时运行期自动回退到 `channel`，失败关闭；
- 已尝试的单 Key 渠道不会再次入选；内存缓存和数据库直查路径使用同一候选过滤边界；找不到合规备用渠道时保留并返回原始上游错误；
- 响应通过 `X-Relay-Upstream-Provider`、`X-Relay-Attempt`、`X-Relay-Retry-Count`、`X-Relay-Retry-Isolation`、`X-Relay-Data-Region`、`X-Relay-Data-Retention` 和 `X-Relay-Data-Training` 披露实际处理策略，CORS 同步暴露这些 Header。

新增回归测试：`service/retry_data_policy_test.go`，覆盖默认隔离、多 Key 渠道内重试、配置不完整时失败关闭、相同供应商边界、策略组边界和响应头披露。

## 8. 上游位置与客户端 IP 隐私

- 所有 JSON 模型请求在最终出站边界执行位置隐私策略，参数覆盖和渠道正文直通不能绕过；
- `UPSTREAM_LOCATION_MODE=strip` 为默认值，删除 OpenAI、Claude、Gemini 协议中的用户位置、经纬度及 metadata/client_metadata 中的 IP 和位置字段；
- `UPSTREAM_LOCATION_MODE=auto` 根据真实网络路径选择画像：渠道配置代理或 `UPSTREAM_SYSTEM_PROXY_ENABLED=true` 时使用 VPN/代理出口画像，否则使用宿主网络出口画像；两套画像可分别由 `UPSTREAM_EGRESS_LOCATION_*` 和 `UPSTREAM_HOST_LOCATION_*` 显式配置；
- `UPSTREAM_LOCATION_DISCOVERY_ENABLED=true` 时，服务启动会分别通过直连路径和已启用的系统代理/VPN 路径检查 ChatGPT、Claude 连通性；两者均可建立 HTTP 连接后，通过 Cloudflare trace 获取该路径的实际公网 IP，并使用 `ipwho.is` 补充国家、地区、城市、时区和经纬度。显式环境变量始终优先，探测只补充空字段；探测失败不会放行客户端位置；
- `UPSTREAM_LOCATION_MODE=egress` 仅在客户端原本提供位置的协议位置上，改写为通过 `UPSTREAM_EGRESS_LOCATION_*` 配置的最终 VPN/代理出口位置，而不是宿主机物理位置；配置缺失时失败关闭并删除原字段；旧的 `relay` 和 `RELAY_LOCATION_*` 仍作为兼容别名；
- `UPSTREAM_LOCATION_MODE=client` 明确允许客户端正文位置透传，但客户端真实 IP 无论位于 Header 还是协议 metadata 都仍禁止发送；
- `Forwarded`、`X-Forwarded-For`、`X-Real-IP`、`CF-Connecting-IP` 以及常见 CDN/代理客户端 IP Header 被加入强制出站禁止名单，通配、正则和显式 Header Override 均不能绕过；
- 中转服务器不伪造或添加自己的 `X-Forwarded-For`，模型服务商通过连接来源自然看到中转服务器出口 IP；
- 国内宿主机通过 VPN、SOCKS5 或 HTTP 代理访问上游时，上游自然看到代理出口 IP；代理必须禁止自行追加客户端或宿主机的 `Forwarded`/`X-Forwarded-For`，否则该 Header 是在离开应用后由代理新增，应用无法再次清理；
- Root 控制面板的“系统设置 → 操作设置 → 上游隐私”可保存位置转发模式，并只读展示系统级 VPN/TUN 状态、自动选择规则、宿主画像和代理出口画像；模式保存到数据库并在运行时热更新、多节点按既有 Option 同步周期传播，`UPSTREAM_LOCATION_MODE` 仅作为数据库尚未配置时的启动默认值；画像数据来自当前进程加载的显式环境变量及启动探测结果，不暴露代理 URL 或凭证；
- `UPSTREAM_HOST_PUBLIC_IP` 和 `UPSTREAM_EGRESS_PUBLIC_IP` 仅作为 Root 控制面板中的出口 IP 展示值，不会写入模型请求；真实网络来源仍由 TCP/TLS 出口决定；
- 自动探测只发送无凭证的连通性请求，不携带客户端请求、OAuth Token 或模型数据；公网出口 IP 会被 Cloudflare 和 `ipwho.is` 看到。对第三方 IP 地理定位服务有合规限制时，可设置 `UPSTREAM_LOCATION_DISCOVERY_ENABLED=false` 并完全使用显式画像；
- 三套部署入口共用 `docker-compose.deploy.yml` 的隐私环境变量，部署脚本会为已有或新建 `.env` 补齐 `UPSTREAM_LOCATION_MODE=strip`、`UPSTREAM_LOCATION_DISCOVERY_ENABLED=true` 和 8 秒总探测超时。安全默认仍为删除客户端位置，探测画像不会自动改变转发模式。

## 9. 已知限制和建议

### 9.1 Anthropic OAuth 组织权限

如果上游返回：

```text
OAuth authentication is currently not allowed for this organization.
```

表示 Token 已被上游识别，但所属 Anthropic 组织不允许 OAuth API 访问。该错误不是网关请求头错误，无法由本项目绕过。建议对该错误增加专门提示，而不是统一显示为 `unknown_error`。

### 9.2 建议作者审阅

1. Codex OAuth client ID、redirect URI 和 scope 是否应由配置项管理；
2. Codex 渠道测试使用的固定 session/turn 元数据是否应改为每次动态生成；
3. Codex 模型列表是否应由上游能力或管理员配置，而不是硬编码；
4. Claude Code OAuth 请求头生成逻辑是否应与模型抓取逻辑共用同一个 helper；
5. OAuth Token、refresh token 和 callback URL 在日志中必须避免明文输出；
6. 对 OAuth 403、401、模型不支持等上游错误增加明确的错误分类和前端提示。
