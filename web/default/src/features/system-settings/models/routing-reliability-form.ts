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
import * as z from 'zod'

import {
  excludeHttpStatusCodes,
  parseHttpStatusCodeRules,
} from '@/lib/http-status-code-rules'

const numericString = z.string().refine((value) => {
  const trimmed = value.trim()
  if (!trimmed) return true
  return !Number.isNaN(Number(trimmed)) && Number(trimmed) >= 0
}, 'Enter a non-negative number or leave empty')

const channelTestModes = ['scheduled_all', 'passive_recovery'] as const
const alwaysSkippedRetryStatusCodes = new Set([504, 524])

export const parseAutomaticRetryStatusCodes = (value: unknown) =>
  excludeHttpStatusCodes(
    parseHttpStatusCodeRules(value),
    alwaysSkippedRetryStatusCodes
  )

type ChannelTestMode = (typeof channelTestModes)[number]

export const routingReliabilitySchema = z
  .object({
    RetryTimes: z.coerce.number().min(0).max(10),
    SubscriptionOAuthUpstreamRetryTimes: z.coerce.number().int().min(0).max(10),
    SubscriptionOAuthCapacityCycleTimes: z.coerce.number().int().min(0).max(10),
    SubscriptionOAuthCapacityWaitSeconds: z.coerce
      .number()
      .int()
      .min(0)
      .max(30),
    SubscriptionOAuthRetry429: z.boolean(),
    ChannelDisableThreshold: numericString,
    AutomaticDisableChannelEnabled: z.boolean(),
    AutomaticEnableChannelEnabled: z.boolean(),
    AutomaticDisableKeywords: z.string(),
    AutomaticDisableStatusCodes: z.string(),
    AutomaticRetryStatusCodes: z.string(),
    monitor_setting: z.object({
      auto_test_channel_enabled: z.boolean(),
      auto_test_channel_minutes: z.coerce
        .number()
        .int()
        .min(1, 'Interval must be at least 1 minute'),
      channel_test_mode: z.enum(channelTestModes),
    }),
  })
  .superRefine((values, ctx) => {
    const disableParsed = parseHttpStatusCodeRules(
      values.AutomaticDisableStatusCodes
    )
    if (!disableParsed.ok) {
      ctx.addIssue({
        code: 'custom',
        path: ['AutomaticDisableStatusCodes'],
        message: `Invalid status code rules: ${disableParsed.invalidTokens.join(
          ', '
        )}`,
      })
    }

    const retryParsed = parseAutomaticRetryStatusCodes(
      values.AutomaticRetryStatusCodes
    )
    if (!retryParsed.ok) {
      ctx.addIssue({
        code: 'custom',
        path: ['AutomaticRetryStatusCodes'],
        message: `Invalid status code rules: ${retryParsed.invalidTokens.join(
          ', '
        )}`,
      })
    }
  })

export type RoutingReliabilityFormValues = z.output<
  typeof routingReliabilitySchema
>
export type RoutingReliabilityFormInput = z.input<typeof routingReliabilitySchema>

export type RoutingReliabilitySectionProps = {
  defaultValues: {
    RetryTimes: number
    SubscriptionOAuthUpstreamRetryTimes: number
    SubscriptionOAuthCapacityCycleTimes: number
    SubscriptionOAuthCapacityWaitSeconds: number
    SubscriptionOAuthRetry429: boolean
    ChannelDisableThreshold: string
    AutomaticDisableChannelEnabled: boolean
    AutomaticEnableChannelEnabled: boolean
    AutomaticDisableKeywords: string
    AutomaticDisableStatusCodes: string
    AutomaticRetryStatusCodes: string
    'monitor_setting.auto_test_channel_enabled': boolean
    'monitor_setting.auto_test_channel_minutes': number
    'monitor_setting.channel_test_mode': ChannelTestMode
  }
}

export type NormalizedRoutingReliabilityValues = {
  RetryTimes: number
  SubscriptionOAuthUpstreamRetryTimes: number
  SubscriptionOAuthCapacityCycleTimes: number
  SubscriptionOAuthCapacityWaitSeconds: number
  SubscriptionOAuthRetry429: boolean
  ChannelDisableThreshold: string
  AutomaticDisableChannelEnabled: boolean
  AutomaticEnableChannelEnabled: boolean
  AutomaticDisableKeywords: string
  AutomaticDisableStatusCodes: string
  AutomaticRetryStatusCodes: string
  'monitor_setting.auto_test_channel_enabled': boolean
  'monitor_setting.auto_test_channel_minutes': number
  'monitor_setting.channel_test_mode': ChannelTestMode
}

function normalizeLineEndings(value: string) {
  return value.replaceAll('\r\n', '\n')
}

function normalizeChannelTestMode(value?: string): ChannelTestMode {
  return value === 'passive_recovery' ? 'passive_recovery' : 'scheduled_all'
}

export const buildFormDefaults = (
  defaults: RoutingReliabilitySectionProps['defaultValues']
): RoutingReliabilityFormInput => ({
  RetryTimes: defaults.RetryTimes ?? 0,
  SubscriptionOAuthUpstreamRetryTimes:
    defaults.SubscriptionOAuthUpstreamRetryTimes ?? 5,
  SubscriptionOAuthCapacityCycleTimes:
    defaults.SubscriptionOAuthCapacityCycleTimes ?? 5,
  SubscriptionOAuthCapacityWaitSeconds:
    defaults.SubscriptionOAuthCapacityWaitSeconds ?? 5,
  SubscriptionOAuthRetry429: defaults.SubscriptionOAuthRetry429 ?? false,
  ChannelDisableThreshold: defaults.ChannelDisableThreshold ?? '',
  AutomaticDisableChannelEnabled: defaults.AutomaticDisableChannelEnabled,
  AutomaticEnableChannelEnabled: defaults.AutomaticEnableChannelEnabled,
  AutomaticDisableKeywords: normalizeLineEndings(
    defaults.AutomaticDisableKeywords ?? ''
  ),
  AutomaticDisableStatusCodes: defaults.AutomaticDisableStatusCodes ?? '',
  AutomaticRetryStatusCodes: defaults.AutomaticRetryStatusCodes ?? '',
  monitor_setting: {
    auto_test_channel_enabled:
      defaults['monitor_setting.auto_test_channel_enabled'],
    auto_test_channel_minutes:
      defaults['monitor_setting.auto_test_channel_minutes'],
    channel_test_mode: normalizeChannelTestMode(
      defaults['monitor_setting.channel_test_mode']
    ),
  },
})

export const normalizeDefaults = (
  defaults: RoutingReliabilitySectionProps['defaultValues']
): NormalizedRoutingReliabilityValues => ({
  RetryTimes: defaults.RetryTimes ?? 0,
  SubscriptionOAuthUpstreamRetryTimes:
    defaults.SubscriptionOAuthUpstreamRetryTimes ?? 5,
  SubscriptionOAuthCapacityCycleTimes:
    defaults.SubscriptionOAuthCapacityCycleTimes ?? 5,
  SubscriptionOAuthCapacityWaitSeconds:
    defaults.SubscriptionOAuthCapacityWaitSeconds ?? 5,
  SubscriptionOAuthRetry429: defaults.SubscriptionOAuthRetry429 ?? false,
  ChannelDisableThreshold: (defaults.ChannelDisableThreshold ?? '').trim(),
  AutomaticDisableChannelEnabled: defaults.AutomaticDisableChannelEnabled,
  AutomaticEnableChannelEnabled: defaults.AutomaticEnableChannelEnabled,
  AutomaticDisableKeywords: normalizeLineEndings(
    defaults.AutomaticDisableKeywords ?? ''
  ),
  AutomaticDisableStatusCodes: parseHttpStatusCodeRules(
    defaults.AutomaticDisableStatusCodes ?? ''
  ).normalized,
  AutomaticRetryStatusCodes: parseAutomaticRetryStatusCodes(
    defaults.AutomaticRetryStatusCodes ?? ''
  ).normalized,
  'monitor_setting.auto_test_channel_enabled':
    defaults['monitor_setting.auto_test_channel_enabled'],
  'monitor_setting.auto_test_channel_minutes':
    defaults['monitor_setting.auto_test_channel_minutes'],
  'monitor_setting.channel_test_mode': normalizeChannelTestMode(
    defaults['monitor_setting.channel_test_mode']
  ),
})

export const normalizeFormValues = (
  values: RoutingReliabilityFormValues
): NormalizedRoutingReliabilityValues => ({
  RetryTimes: values.RetryTimes,
  SubscriptionOAuthUpstreamRetryTimes:
    values.SubscriptionOAuthUpstreamRetryTimes,
  SubscriptionOAuthCapacityCycleTimes:
    values.SubscriptionOAuthCapacityCycleTimes,
  SubscriptionOAuthCapacityWaitSeconds:
    values.SubscriptionOAuthCapacityWaitSeconds,
  SubscriptionOAuthRetry429: values.SubscriptionOAuthRetry429,
  ChannelDisableThreshold: values.ChannelDisableThreshold.trim(),
  AutomaticDisableChannelEnabled: values.AutomaticDisableChannelEnabled,
  AutomaticEnableChannelEnabled: values.AutomaticEnableChannelEnabled,
  AutomaticDisableKeywords: normalizeLineEndings(
    values.AutomaticDisableKeywords
  ),
  AutomaticDisableStatusCodes: parseHttpStatusCodeRules(
    values.AutomaticDisableStatusCodes
  ).normalized,
  AutomaticRetryStatusCodes: parseAutomaticRetryStatusCodes(
    values.AutomaticRetryStatusCodes
  ).normalized,
  'monitor_setting.auto_test_channel_enabled':
    values.monitor_setting.auto_test_channel_enabled,
  'monitor_setting.auto_test_channel_minutes':
    values.monitor_setting.auto_test_channel_minutes,
  'monitor_setting.channel_test_mode': values.monitor_setting.channel_test_mode,
})
