import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import {
  localizeErrorCode,
  localizeErrorMessage,
} from './localize-error-message'

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
    assert.equal(localizeErrorCode('unclassified_error'), undefined)
  })
})
