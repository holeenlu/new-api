import assert from 'node:assert/strict'
import { before, describe, test } from 'node:test'

import i18next from 'i18next'

import zhCN from '../i18n/locales/zh.json'
import {
  localizeErrorCode,
  localizeErrorMessage,
} from './localize-error-message'

// The localization layer is locale-aware: Chinese locales get the Simplified-
// Chinese prose rewrite, and classified error codes resolve through i18next.t
// against the loaded translations. Initialize the shared i18next instance as
// Chinese (the module imports the same singleton) so these assertions exercise
// that path instead of the verbatim/English fallback.
before(async () => {
  await i18next.init({
    lng: 'zhCN',
    fallbackLng: 'en',
    resources: { zhCN },
    nsSeparator: false,
    interpolation: { escapeValue: false },
  })
})

describe('localizeErrorMessage', () => {
  test('classifies an upstream EOF without losing the request URL', () => {
    assert.equal(
      localizeErrorMessage(
        'status_code=500, Post "https://chatgpt.com/backend-api/codex/responses": EOF'
      ),
      '状态码=500, 请求 "https://chatgpt.com/backend-api/codex/responses" 时，上游在返回响应头前断开连接（EOF）'
    )
  })

  test('localizes stream termination fields', () => {
    assert.equal(localizeErrorMessage('client_gone'), '客户端已断开')
    assert.equal(localizeErrorMessage('context canceled'), '请求上下文已取消')
    assert.equal(localizeErrorMessage('timeout'), '上游流等待超时')
  })

  test('localizes stream transport and credential state errors', () => {
    assert.equal(localizeErrorMessage('scanner_error'), '上游流读取错误')
    assert.equal(
      localizeErrorMessage(
        'upstream stream produced no semantic output within 30s'
      ),
      '上游流在 30s 内未产生有效输出'
    )
    assert.equal(
      localizeErrorMessage(
        'stream error: stream ID 99; INTERNAL_ERROR; received from peer'
      ),
      '上游 HTTP/2 流异常：流 ID 99，对端返回内部错误'
    )
    assert.equal(
      localizeErrorMessage(
        'subscription OAuth credential is busy; retry after 1 seconds: subscription OAuth credential is temporarily unavailable'
      ),
      '订阅 OAuth 凭证正忙，请在 1 秒后重试：订阅 OAuth 凭证暂时不可用'
    )
    assert.equal(
      localizeErrorMessage(
        'Selected model is at capacity. Please try a different model.'
      ),
      '所选模型当前容量不足，请稍后重试或选择其他模型。'
    )
  })

  test('keeps diagnostic codes while translating their labels', () => {
    assert.equal(
      localizeErrorMessage(
        'status_code=503, error_type=new_api_error, error_code=do_request_failed'
      ),
      '状态码=503, 错误类型=网关错误, 错误代码=请求上游失败'
    )
  })

  test('retains unknown provider text behind a Chinese diagnostic label', () => {
    assert.equal(
      localizeErrorMessage('provider-specific failure ABC-123'),
      '错误详情（上游原文）：provider-specific failure ABC-123'
    )
  })

  test('provides stable messages for classified OAuth errors', () => {
    assert.equal(
      localizeErrorCode('oauth_unauthorized'),
      'OAuth 凭证无效或已过期，请重新授权或刷新渠道凭证。'
    )
    assert.equal(
      localizeErrorCode('upstream_quota_exhausted'),
      '上游账号额度已耗尽，相关 OAuth 凭证已隔离，请联系管理员。'
    )
    assert.equal(localizeErrorCode('unclassified_error'), undefined)
  })
})
