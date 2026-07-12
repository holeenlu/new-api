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
user-agent
x-client-request-id
x-codex-beta-features
x-codex-turn-metadata
x-codex-window-id
x-openai-internal-codex-responses-lite
```

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
Context string `json:"context,omitempty"`
```

### 1.6 渠道测试和模型同步

文件：

- `controller/channel-test.go`
- `controller/channel.go`
- `controller/channel_upstream_update.go`

Codex 渠道测试强制使用 stream 模式；Codex 模型列表和上游模型同步改为使用本地固定 `ModelList`，避免上游 `/v1/models` 返回不支持的模型。

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

## 3. 国际化

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

## 4. 测试

新增测试：

```text
relay/channel/claude/adaptor_test.go
relay/channel/codex/adaptor_test.go
relay/channel/codex/constants_test.go
```

覆盖内容：

- Claude Code OAuth 请求头生成；
- 删除旧 `x-api-key`；
- Codex 客户端请求头转发；
- Codex Bearer 和 account ID 设置；
- Codex 固定模型列表。

## 5. 部署和验证

本地 Docker 镜像已重新构建并启动，服务地址：

```text
http://127.0.0.1:3001
```

已验证：

- Docker 完整镜像构建通过；
- 后端编译通过；
- `/api/status` 可正常访问；
- 本地容器已重新创建并运行。

## 6. 已知限制和建议

### 6.1 Anthropic OAuth 组织权限

如果上游返回：

```text
OAuth authentication is currently not allowed for this organization.
```

表示 Token 已被上游识别，但所属 Anthropic 组织不允许 OAuth API 访问。该错误不是网关请求头错误，无法由本项目绕过。建议对该错误增加专门提示，而不是统一显示为 `unknown_error`。

### 6.2 建议作者审阅

1. Codex OAuth client ID、redirect URI 和 scope 是否应由配置项管理；
2. Codex 渠道测试使用的固定 session/turn 元数据是否应改为每次动态生成；
3. Codex 模型列表是否应由上游能力或管理员配置，而不是硬编码；
4. Claude Code OAuth 请求头生成逻辑是否应与模型抓取逻辑共用同一个 helper；
5. OAuth Token、refresh token 和 callback URL 在日志中必须避免明文输出；
6. 对 OAuth 403、401、模型不支持等上游错误增加明确的错误分类和前端提示。

