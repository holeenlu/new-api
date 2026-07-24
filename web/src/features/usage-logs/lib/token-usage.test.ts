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
import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import type { UsageLog } from '../data/schema'
import { getUsageTokenBreakdown } from './token-usage'

const baseLog = {
  prompt_tokens: 2,
  completion_tokens: 92,
} as UsageLog

describe('Anthropic usage token presentation', () => {
  test('prefers the authoritative total input over reconstructed fields', () => {
    const result = getUsageTokenBreakdown(baseLog, {
      usage_semantic: 'anthropic',
      input_tokens_total: 102_038,
      cache_tokens: 90_000,
      cache_write_tokens: 500,
    })

    assert.equal(result.isAnthropic, true)
    assert.equal(result.totalInputTokens, 102_038)
    assert.equal(result.promptTokens, 2)
    assert.equal(result.completionTokens, 92)
  })

  test('reconstructs old Anthropic logs from fresh and cache components', () => {
    const result = getUsageTokenBreakdown(baseLog, {
      usage_semantic: 'anthropic',
      cache_tokens: 100_999,
      cache_creation_tokens_5m: 1_037,
    })

    assert.equal(result.totalInputTokens, 102_038)
    assert.equal(result.cacheReadTokens, 100_999)
    assert.equal(result.cacheWriteTokens, 1_037)
  })

  test('does not apply Anthropic totals to other usage semantics', () => {
    const result = getUsageTokenBreakdown(baseLog, {
      usage_semantic: 'openai',
      input_tokens_total: 102_038,
      cache_tokens: 100_999,
    })

    assert.equal(result.isAnthropic, false)
    assert.equal(result.totalInputTokens, 2)
  })
})
