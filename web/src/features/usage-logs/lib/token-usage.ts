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
import type { UsageLog } from '../data/schema'
import type { LogOtherData } from '../types'

export interface UsageTokenBreakdown {
  isAnthropic: boolean
  promptTokens: number
  completionTokens: number
  totalInputTokens: number
  cacheReadTokens: number
  cacheWriteTokens: number
}

function finiteNonNegative(value: unknown): number | null {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0
    ? value
    : null
}

export function getUsageTokenBreakdown(
  log: UsageLog,
  other: LogOtherData | null | undefined
): UsageTokenBreakdown {
  const promptTokens = log.prompt_tokens || 0
  const completionTokens = log.completion_tokens || 0
  const cacheReadTokens = other?.cache_tokens || 0
  const cacheWrite5m = other?.cache_creation_tokens_5m || 0
  const cacheWrite1h = other?.cache_creation_tokens_1h || 0
  const normalizedCacheWrite = finiteNonNegative(other?.cache_write_tokens)
  const hasSplitCacheWrite = cacheWrite5m > 0 || cacheWrite1h > 0
  const cacheWriteTokens =
    normalizedCacheWrite ??
    (hasSplitCacheWrite
      ? cacheWrite5m + cacheWrite1h
      : other?.cache_creation_tokens || 0)
  const isAnthropic = other?.usage_semantic === 'anthropic'
  const authoritativeTotal = finiteNonNegative(other?.input_tokens_total)
  const totalInputTokens = isAnthropic
    ? (authoritativeTotal ?? promptTokens + cacheReadTokens + cacheWriteTokens)
    : promptTokens

  return {
    isAnthropic,
    promptTokens,
    completionTokens,
    totalInputTokens,
    cacheReadTokens,
    cacheWriteTokens,
  }
}
