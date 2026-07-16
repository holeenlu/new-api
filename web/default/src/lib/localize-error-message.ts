/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/

const DEFAULT_ERROR_MESSAGE = '请求失败'

const ERROR_CODE_MESSAGES: Readonly<Record<string, string>> = {
  model_not_supported:
    'OAuth 账号无法使用此模型，请获取上游模型列表或选择其他模型。',
  oauth_forbidden: 'OAuth 账号无权访问此资源，请检查订阅状态和账号权限。',
  oauth_unauthorized: 'OAuth 凭证无效或已过期，请重新授权或刷新渠道凭证。',
}

const TOKEN_REPLACEMENTS: ReadonlyArray<readonly [RegExp, string]> = [
  [/\bstatus_code\b/gi, '状态码'],
  [/\berror_type\b/gi, '错误类型'],
  [/\berror_code\b/gi, '错误代码'],
  [/\brequest_id\b/gi, '请求 ID'],
  [/\bdo_request_failed\b/gi, '请求上游失败'],
  [/\bbad_response_status_code\b/gi, '上游返回异常状态码'],
  [/\binvalid_request_error\b/gi, '请求参数错误'],
  [/\brate_limit_error\b/gi, '请求频率限制错误'],
  [/\bpermission_error\b/gi, '权限错误'],
  [/\bauthentication_error\b/gi, '身份验证错误'],
  [/\bnew_api_error\b/gi, '网关错误'],
  [/\bopenai_error\b/gi, 'OpenAI 兼容接口错误'],
  [/\bunknown_error\b/gi, '未知错误'],
  [/\bhttpStatus(?=\s*[:=])/g, 'HTTP 状态码'],
  [/\brequestUrl(?=\s*[:=])/g, '请求地址'],
  [/\bprobedModel(?=\s*[:=])/g, '测试模型'],
  [/\bresponseBody(?=\s*[:=])/g, '响应体'],
  [/\bcheckedAt(?=\s*[:=])/g, '检查时间'],
  [/\bendpoint(?=\s*[:=])/gi, '上游地址'],
  [/\bmessage(?=\s*[:=])/gi, '信息'],
  [/\bbody(?=\s*[:=])/gi, '响应体'],
  [/\btype(?=\s*[:=])/gi, '类型'],
  [/\bparam(?=\s*[:=])/gi, '参数'],
  [/\bcode(?=\s*[:=])/gi, '代码'],
]

const EXACT_MESSAGES: Readonly<Record<string, string>> = {
  eof: '上游在返回响应头前断开连接（EOF）',
  error: '错误',
  failed: '操作失败',
  forbidden: '禁止访问',
  unauthorized: '未授权',
  unknown: '未知',
  'request failed': '请求失败',
  'request error occurred': '请求发生错误',
  'an unknown error occurred': '发生未知错误',
  'something went wrong!': '发生错误',
  'content not found.': '未找到内容',
  'session expired!': '会话已过期，请重新登录',
  'connection closed': '连接已关闭',
  'network connection failed or server not responding':
    '网络连接失败或服务器未响应',
  'error parsing response data': '解析响应数据失败',
  'error establishing connection': '建立连接失败',
  'generation was interrupted': '生成已中断',
}

function extractErrorText(value: unknown): string {
  if (typeof value === 'string') return value
  if (value instanceof Error) return value.message
  if (!value || typeof value !== 'object') return ''

  const record = value as Record<string, unknown>
  for (const key of ['message', 'detail', 'title', 'error']) {
    const candidate = record[key]
    if (typeof candidate === 'string' && candidate.trim()) return candidate
    if (candidate && typeof candidate === 'object') {
      const nested = extractErrorText(candidate)
      if (nested) return nested
    }
  }

  return ''
}

function replaceKnownErrorText(message: string): string {
  let localized = message

  localized = localized.replaceAll(
    /\b(Post|Get|Put|Patch|Delete)\s+"([^"]+)":\s*EOF\b/gi,
    '请求 "$2" 时，上游在返回响应头前断开连接（EOF）'
  )
  localized = localized.replaceAll(
    /\b(?:http2:\s*)?timeout awaiting response headers\b/gi,
    '等待上游响应头超时'
  )
  localized = localized.replaceAll(
    /\bcontext deadline exceeded\b/gi,
    '请求超过截止时间'
  )
  localized = localized.replaceAll(/\bcontext canceled\b/gi, '请求上下文已取消')
  localized = localized.replaceAll(/\bclient_gone\b/gi, '客户端已断开')
  localized = localized.replaceAll(
    /\bconnection reset by peer\b/gi,
    '上游重置了连接'
  )
  localized = localized.replaceAll(/\bbroken pipe\b/gi, '连接已断开')
  localized = localized.replaceAll(/\bconnection refused\b/gi, '连接被拒绝')
  localized = localized.replaceAll(/\bno such host\b/gi, '无法解析上游主机')
  localized = localized.replaceAll(
    /\bTLS handshake timeout\b/gi,
    'TLS 握手超时'
  )

  localized = localized.replaceAll(
    /\bcodex responses websocket request is too large\b/gi,
    'Codex Responses WebSocket 请求体过大'
  )
  localized = localized.replaceAll(
    /\brequest (?:body )?(?:is too large|exceeds[^,\n}]*)/gi,
    '请求体过大'
  )
  localized = localized.replaceAll(
    /\bbad response status code\s*(\d+)?/gi,
    (_match, status: string | undefined) =>
      status ? `上游返回异常状态码 ${status}` : '上游返回异常状态码'
  )
  localized = localized.replaceAll(
    /\bGateway returned HTTP\s*(\d+)/gi,
    '网关返回 HTTP $1'
  )
  localized = localized.replaceAll(
    /\bStream must be set to true\b/gi,
    'Stream 必须设置为 true'
  )
  localized = localized.replaceAll(
    /\bStore must be set to false\b/gi,
    'Store 必须设置为 false'
  )
  localized = localized.replaceAll(
    /\bModel not found\s+([^,\n}]+)/gi,
    '找不到模型 $1'
  )
  localized = localized.replaceAll(
    /\bThis model is not supported([^,\n}]*)/gi,
    '当前调用方式不支持此模型$1'
  )
  localized = localized.replaceAll(
    /\bOAuth authentication is currently not allowed for this organization\.?/gi,
    '当前组织不允许使用 OAuth 身份验证'
  )
  localized = localized.replaceAll(
    /\bconcurrency limit reached;?\s*retry later\.?/gi,
    '已达到并发限制，请稍后重试'
  )
  localized = localized.replaceAll(
    /\brate limit(?:ed| error)?\b/gi,
    '请求频率受限'
  )
  localized = localized.replaceAll(
    /\bItem with id\s+('[^']+'|"[^"]+")\s+not found\.?/gi,
    '找不到 ID 为 $1 的项目。'
  )
  localized = localized.replaceAll(
    /\bItems are not persisted when `store` is set to false\.?/gi,
    '`store` 设为 false 时不会持久化项目。'
  )
  localized = localized.replaceAll(
    /\bTry again with `store` set to true, or remove this item from your input\.?/gi,
    '请将 `store` 设为 true 后重试，或从输入中删除该项目。'
  )
  localized = localized.replaceAll(
    /\bonly supports function tools, custom tools, and client-executed tool search\.?/gi,
    '仅支持 function 工具、custom 工具和由客户端执行的 tool search。'
  )

  for (const [pattern, replacement] of TOKEN_REPLACEMENTS) {
    localized = localized.replaceAll(pattern, replacement)
  }

  const exact = localized.trim().toLowerCase()
  return EXACT_MESSAGES[exact] ?? localized
}

export function localizeErrorCode(
  errorCode: string | undefined
): string | undefined {
  if (!errorCode) return undefined
  return ERROR_CODE_MESSAGES[errorCode]
}

/**
 * Converts provider, transport, and API errors to Simplified Chinese for UI
 * display while retaining URLs, model names, request IDs, and protocol codes.
 */
export function localizeErrorMessage(
  value: unknown,
  fallback = DEFAULT_ERROR_MESSAGE
): string {
  const source = extractErrorText(value).trim()
  if (!source) return fallback

  const localized = replaceKnownErrorText(source)
  if (localized !== source || /[\u3400-\u9fff]/.test(localized)) {
    return localized
  }

  return `错误详情（上游原文）：${source}`
}
