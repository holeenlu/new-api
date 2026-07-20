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
import { zodResolver } from '@hookform/resolvers/zod'
import { Refresh01Icon } from '@hugeicons/core-free-icons'
import { HugeiconsIcon } from '@hugeicons/react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import {
  MapPinIcon,
  NetworkIcon,
  ServerIcon,
  ShieldCheckIcon,
} from 'lucide-react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import * as z from 'zod'

import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Form,
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

import { refreshUpstreamLocationProfiles } from '../api'
import { SettingsPageFormActions } from '../components/settings-page-context'
import { SettingsSection } from '../components/settings-section'
import { useResetForm } from '../hooks/use-reset-form'
import { useUpdateOption } from '../hooks/use-update-option'
import type {
  OperationsSettings,
  UpstreamLocationMode,
  UpstreamLocationProfileResponse,
} from '../types'

type UpstreamPrivacySectionProps = {
  settings: OperationsSettings
}

type LocationProfile = {
  publicIp: string
  country: string
  region: string
  city: string
  timezone: string
  latitude: string
  longitude: string
}

const upstreamPrivacySchema = z.object({
  UpstreamLocationMode: z.enum(['strip', 'auto', 'host', 'egress', 'client']),
})

type UpstreamPrivacyFormValues = z.infer<typeof upstreamPrivacySchema>

const locationModeOptions: Array<{
  value: UpstreamLocationMode
  label: string
}> = [
  { value: 'strip', label: 'Strip client location' },
  { value: 'auto', label: 'Automatic route selection' },
  { value: 'host', label: 'Force host profile' },
  { value: 'egress', label: 'Force proxy egress profile' },
  { value: 'client', label: 'Allow client location' },
]

function locationSummary(profile: LocationProfile, fallback: string): string {
  return (
    [profile.city, profile.region, profile.country]
      .filter(Boolean)
      .join(', ') || fallback
  )
}

function coordinateSummary(profile: LocationProfile, fallback: string): string {
  if (!profile.latitude && !profile.longitude) return fallback
  return [profile.latitude || fallback, profile.longitude || fallback].join(
    ', '
  )
}

function locationProfileFromResponse(
  profile: UpstreamLocationProfileResponse
): LocationProfile {
  return {
    publicIp: profile.public_ip,
    country: profile.country,
    region: profile.region,
    city: profile.city,
    timezone: profile.timezone,
    latitude: profile.latitude,
    longitude: profile.longitude,
  }
}

function refreshErrorMessage(error: unknown): string | undefined {
  if (error && typeof error === 'object' && 'response' in error) {
    const response = error.response
    if (response && typeof response === 'object' && 'data' in response) {
      const data = response.data
      if (data && typeof data === 'object' && 'message' in data) {
        return typeof data.message === 'string' ? data.message : undefined
      }
    }
  }
  return error instanceof Error && error.message ? error.message : undefined
}

export function UpstreamPrivacySection(props: UpstreamPrivacySectionProps) {
  const { t } = useTranslation()
  const updateOption = useUpdateOption()
  const queryClient = useQueryClient()
  const defaultValues: UpstreamPrivacyFormValues = {
    UpstreamLocationMode: props.settings.UpstreamLocationMode,
  }
  const form = useForm<UpstreamPrivacyFormValues>({
    resolver: zodResolver(upstreamPrivacySchema),
    defaultValues,
  })

  useResetForm(form, defaultValues)

  const refreshProfiles = useMutation({
    mutationFn: refreshUpstreamLocationProfiles,
    onSuccess: async (response) => {
      await queryClient.invalidateQueries({ queryKey: ['system-options'] })
      const hasWarnings = Boolean(
        response.data.host.error || response.data.egress.error
      )
      if (hasWarnings) {
        toast.warning(t('Network profiles refreshed with warnings'))
      } else {
        toast.success(t('Network profiles refreshed'))
      }
    },
    onError: (error: unknown) => {
      toast.error(
        refreshErrorMessage(error) || t('Failed to refresh network profiles')
      )
    },
  })

  const onSubmit = async (data: UpstreamPrivacyFormValues) => {
    if (data.UpstreamLocationMode === props.settings.UpstreamLocationMode) {
      return
    }
    await updateOption.mutateAsync({
      key: 'UpstreamLocationMode',
      value: data.UpstreamLocationMode,
    })
  }

  const refreshedProfiles = refreshProfiles.data?.data
  const hostProfile: LocationProfile = refreshedProfiles
    ? locationProfileFromResponse(refreshedProfiles.host.profile)
    : {
        publicIp: props.settings.UpstreamHostPublicIP,
        country: props.settings.UpstreamHostLocationCountry,
        region: props.settings.UpstreamHostLocationRegion,
        city: props.settings.UpstreamHostLocationCity,
        timezone: props.settings.UpstreamHostLocationTimezone,
        latitude: props.settings.UpstreamHostLocationLatitude,
        longitude: props.settings.UpstreamHostLocationLongitude,
      }
  const egressProfile: LocationProfile = refreshedProfiles
    ? locationProfileFromResponse(refreshedProfiles.egress.profile)
    : {
        publicIp: props.settings.UpstreamEgressPublicIP,
        country: props.settings.UpstreamEgressLocationCountry,
        region: props.settings.UpstreamEgressLocationRegion,
        city: props.settings.UpstreamEgressLocationCity,
        timezone: props.settings.UpstreamEgressLocationTimezone,
        latitude: props.settings.UpstreamEgressLocationLatitude,
        longitude: props.settings.UpstreamEgressLocationLongitude,
      }
  const mode = form.watch('UpstreamLocationMode')
  let selectionText = t(
    'Proxied channels use the proxy egress profile; direct channels use the host profile'
  )
  if (mode === 'strip') {
    selectionText = t('Client location is removed')
  } else if (mode === 'client') {
    selectionText = t('Client location is allowed; client IP remains blocked')
  } else if (mode === 'host') {
    selectionText = t('All requests use the host location profile')
  } else if (mode === 'egress') {
    selectionText = t('All requests use the proxy egress profile')
  } else if (props.settings.UpstreamSystemProxyEnabled) {
    selectionText = t(
      'System VPN or TUN is active; all requests use the proxy egress profile'
    )
  }
  const notConfigured = t('Not configured')

  const profiles = [
    {
      key: 'host',
      title: t('Host network profile'),
      icon: <ServerIcon className='h-4 w-4' aria-hidden='true' />,
      value: hostProfile,
    },
    {
      key: 'egress',
      title: t('VPN or proxy egress profile'),
      icon: <NetworkIcon className='h-4 w-4' aria-hidden='true' />,
      value: egressProfile,
    },
  ]

  return (
    <SettingsSection title={t('Upstream Privacy')}>
      <Form {...form}>
        <form onSubmit={form.handleSubmit(onSubmit)}>
          <SettingsPageFormActions
            onSave={form.handleSubmit(onSubmit)}
            onReset={() => form.reset(defaultValues)}
            isSaving={updateOption.isPending}
            isSaveDisabled={!form.formState.isDirty}
            isResetDisabled={!form.formState.isDirty}
          />
          <div className='border-border divide-border divide-y border-y'>
            <FormField
              control={form.control}
              name='UpstreamLocationMode'
              render={({ field }) => (
                <FormItem className='flex flex-wrap items-center justify-between gap-3 px-4 py-3'>
                  <div className='flex items-center gap-2 text-sm font-medium'>
                    <ShieldCheckIcon
                      className='text-muted-foreground h-4 w-4'
                      aria-hidden='true'
                    />
                    <FormLabel>{t('Location forwarding mode')}</FormLabel>
                  </div>
                  <Select
                    items={locationModeOptions.map((option) => ({
                      value: option.value,
                      label: t(option.label),
                    }))}
                    value={field.value}
                    onValueChange={field.onChange}
                    disabled={updateOption.isPending}
                  >
                    <FormControl>
                      <SelectTrigger className='w-64 max-w-full'>
                        <SelectValue />
                      </SelectTrigger>
                    </FormControl>
                    <SelectContent alignItemWithTrigger={false}>
                      <SelectGroup>
                        {locationModeOptions.map((option) => (
                          <SelectItem key={option.value} value={option.value}>
                            {t(option.label)}
                          </SelectItem>
                        ))}
                      </SelectGroup>
                    </SelectContent>
                  </Select>
                  <FormMessage className='basis-full text-right' />
                </FormItem>
              )}
            />
            <div className='flex flex-wrap items-center justify-between gap-3 px-4 py-3'>
              <span className='text-sm font-medium'>
                {t('System-level VPN or TUN')}
              </span>
              <Badge
                variant={
                  props.settings.UpstreamSystemProxyEnabled
                    ? 'default'
                    : 'outline'
                }
              >
                {props.settings.UpstreamSystemProxyEnabled
                  ? t('Enabled')
                  : t('Disabled')}
              </Badge>
            </div>
            <div className='px-4 py-3'>
              <div className='text-sm font-medium'>
                {t('Effective selection rule')}
              </div>
              <div className='text-muted-foreground mt-1 text-sm'>
                {selectionText}
              </div>
            </div>
          </div>
        </form>
      </Form>

      <div className='flex justify-end'>
        <Button
          type='button'
          variant='outline'
          size='sm'
          disabled={refreshProfiles.isPending}
          onClick={() => refreshProfiles.mutate()}
        >
          <HugeiconsIcon
            icon={Refresh01Icon}
            data-icon='inline-start'
            className={refreshProfiles.isPending ? 'animate-spin' : undefined}
          />
          {refreshProfiles.isPending ? t('Refreshing...') : t('Refresh')}
        </Button>
      </div>

      <div
        className='grid gap-6 lg:grid-cols-2'
        aria-busy={refreshProfiles.isPending}
      >
        {profiles.map((profile) => (
          <section key={profile.key} className='min-w-0'>
            <h3 className='mb-2 flex items-center gap-2 text-sm font-semibold'>
              {profile.icon}
              {profile.title}
            </h3>
            <dl className='border-border divide-border divide-y border-y text-sm'>
              <div className='grid grid-cols-[minmax(7rem,0.4fr)_minmax(0,1fr)] gap-3 px-3 py-2.5'>
                <dt className='text-muted-foreground'>
                  {t('Expected public IP')}
                </dt>
                <dd className='min-w-0 break-words text-right font-mono'>
                  {profile.value.publicIp || notConfigured}
                </dd>
              </div>
              <div className='grid grid-cols-[minmax(7rem,0.4fr)_minmax(0,1fr)] gap-3 px-3 py-2.5'>
                <dt className='text-muted-foreground flex items-center gap-2'>
                  <MapPinIcon className='h-4 w-4 shrink-0' aria-hidden='true' />
                  {t('Location')}
                </dt>
                <dd className='min-w-0 break-words text-right'>
                  {locationSummary(profile.value, notConfigured)}
                </dd>
              </div>
              <div className='grid grid-cols-[minmax(7rem,0.4fr)_minmax(0,1fr)] gap-3 px-3 py-2.5'>
                <dt className='text-muted-foreground'>{t('Timezone')}</dt>
                <dd className='min-w-0 break-words text-right'>
                  {profile.value.timezone || notConfigured}
                </dd>
              </div>
              <div className='grid grid-cols-[minmax(7rem,0.4fr)_minmax(0,1fr)] gap-3 px-3 py-2.5'>
                <dt className='text-muted-foreground'>{t('Coordinates')}</dt>
                <dd className='min-w-0 break-words text-right tabular-nums'>
                  {coordinateSummary(profile.value, notConfigured)}
                </dd>
              </div>
            </dl>
          </section>
        ))}
      </div>
    </SettingsSection>
  )
}
