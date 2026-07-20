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
import { ShieldCheck } from 'lucide-react'
import type { UseFormReturn } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import { Alert, AlertDescription } from '@/components/ui/alert'
import {
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { IconBadge } from '@/components/ui/icon-badge'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

import type { ChannelFormValues } from '../../../lib'

type ChannelDataGovernanceFieldsProps = {
  form: UseFormReturn<ChannelFormValues>
  sectionId: string
  className: string
  sensitiveLocked: boolean
  subscriptionOAuth: boolean
  retryIsolation: ChannelFormValues['retry_isolation']
}

const textFields = [
  {
    name: 'data_provider',
    label: 'Provider',
    placeholder: 'Use the channel provider by default',
    description: 'Public name of the upstream data processor',
  },
  {
    name: 'data_region',
    label: 'Data region',
    placeholder: 'Provider or account policy',
    description: 'Region where prompts and outputs may be processed',
  },
  {
    name: 'data_retention',
    label: 'Data retention',
    placeholder: 'Provider or account policy',
    description: 'Retention period disclosed to API clients',
  },
] as const

export function ChannelDataGovernanceFields(
  props: ChannelDataGovernanceFieldsProps
) {
  const { t } = useTranslation()

  return (
    <div id={props.sectionId} className={props.className}>
      <div className='flex items-center gap-2'>
        <IconBadge tone='success' size='xs'>
          <ShieldCheck className='h-3.5 w-3.5' />
        </IconBadge>
        <h4 className='text-muted-foreground text-xs font-medium tracking-wide uppercase'>
          {t('Data governance')}
        </h4>
      </div>

      <Alert>
        <AlertDescription>
          {t(
            'These values are disclosed in response headers and define the boundary for automatic retries.'
          )}
        </AlertDescription>
      </Alert>

      <div className='grid gap-4 sm:grid-cols-2'>
        {textFields.map((config) => (
          <FormField
            key={config.name}
            control={props.form.control}
            name={config.name}
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t(config.label)}</FormLabel>
                <FormControl>
                  <Input
                    placeholder={t(config.placeholder)}
                    disabled={props.sensitiveLocked}
                    {...field}
                  />
                </FormControl>
                <FormDescription>{t(config.description)}</FormDescription>
                <FormMessage />
              </FormItem>
            )}
          />
        ))}

        <FormField
          control={props.form.control}
          name='data_training'
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('Training policy')}</FormLabel>
              <Select
                disabled={props.sensitiveLocked}
                items={[
                  {
                    value: 'provider_default',
                    label: t('Provider or account policy'),
                  },
                  { value: 'disabled', label: t('Disabled') },
                  { value: 'enabled', label: t('Enabled') },
                ]}
                onValueChange={field.onChange}
                value={field.value}
              >
                <FormControl>
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                </FormControl>
                <SelectContent alignItemWithTrigger={false}>
                  <SelectGroup>
                    <SelectItem value='provider_default'>
                      {t('Provider or account policy')}
                    </SelectItem>
                    <SelectItem value='disabled'>{t('Disabled')}</SelectItem>
                    <SelectItem value='enabled'>{t('Enabled')}</SelectItem>
                  </SelectGroup>
                </SelectContent>
              </Select>
              <FormDescription>
                {t('Whether upstream may use request data for model training')}
              </FormDescription>
              <FormMessage />
            </FormItem>
          )}
        />

        <FormField
          control={props.form.control}
          name='retry_isolation'
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('Retry isolation')}</FormLabel>
              <Select
                disabled={props.sensitiveLocked}
                items={[
                  { value: 'auto', label: t('Automatic safe default') },
                  { value: 'channel', label: t('Current channel only') },
                  { value: 'provider', label: t('Same provider endpoint') },
                  { value: 'policy_group', label: t('Explicit policy group') },
                ]}
                onValueChange={field.onChange}
                value={field.value}
              >
                <FormControl>
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                </FormControl>
                <SelectContent alignItemWithTrigger={false}>
                  <SelectGroup>
                    <SelectItem value='auto'>
                      {t('Automatic safe default')}
                    </SelectItem>
                    <SelectItem value='channel'>
                      {t('Current channel only')}
                    </SelectItem>
                    <SelectItem value='provider'>
                      {t('Same provider endpoint')}
                    </SelectItem>
                    <SelectItem value='policy_group'>
                      {t('Explicit policy group')}
                    </SelectItem>
                  </SelectGroup>
                </SelectContent>
              </Select>
              <FormDescription>
                {t(
                  props.subscriptionOAuth
                    ? 'Subscription OAuth automatic mode retries compatible channels in the selected group. Channel tags are metadata only.'
                    : 'Automatic mode retries channels with the same non-empty tag and matching data policy. Channels without a tag stay isolated.'
                )}
              </FormDescription>
              <FormMessage />
            </FormItem>
          )}
        />

        {props.retryIsolation === 'policy_group' && (
          <FormField
            control={props.form.control}
            name='retry_policy_group'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Policy group')}</FormLabel>
                <FormControl>
                  <Input
                    placeholder={t('Channels with identical data terms')}
                    disabled={props.sensitiveLocked}
                    {...field}
                  />
                </FormControl>
                <FormDescription>
                  {t(
                    'Only channels with the same provider, data policy, and group can receive retries.'
                  )}
                </FormDescription>
                <FormMessage />
              </FormItem>
            )}
          />
        )}
      </div>
    </div>
  )
}
