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
import type { QueryClient } from '@tanstack/react-query'
import { useCallback, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import type { StartVerificationOptions } from '@/features/auth/secure-verification'

import {
  getChannelKey,
  refreshCodexCredential as refreshCodexCredentialRequest,
} from '../api'
import { channelsQueryKeys } from '../lib'
import { getChannelErrorMessage } from '../lib/channel-error-messages'

type UseChannelCredentialActionsProps = {
  channelId: number | null
  queryClient: QueryClient
  withVerification: (
    apiCall: (proofToken?: string) => Promise<unknown>,
    config: StartVerificationOptions
  ) => Promise<unknown>
}

export function useChannelCredentialActions(
  props: UseChannelCredentialActionsProps
) {
  const { t } = useTranslation()
  const { channelId, queryClient, withVerification } = props
  const [channelKey, setChannelKey] = useState<string | null>(null)
  const [isChannelKeyLoading, setIsChannelKeyLoading] = useState(false)
  const [isCodexCredentialRefreshing, setIsCodexCredentialRefreshing] =
    useState(false)

  const resetChannelKey = useCallback(() => {
    setChannelKey(null)
    setIsChannelKeyLoading(false)
  }, [])

  const fetchChannelKey = useCallback(async () => {
    if (!channelId) {
      throw new Error('Channel is not selected')
    }

    setIsChannelKeyLoading(true)
    try {
      const res = await getChannelKey(channelId)
      if (!res.success) {
        throw new Error(res.message || t('Failed to fetch channel key'))
      }

      setChannelKey(res.data?.key ?? '')
      toast.success(t('Channel key unlocked'))
      return res
    } finally {
      setIsChannelKeyLoading(false)
    }
  }, [channelId, t])

  const revealChannelKey = useCallback(async () => {
    if (!channelId) return

    try {
      await withVerification(fetchChannelKey, {
        scope: 'channel.key.read',
        preferredMethod: 'passkey',
        title: t('Verify to view channel key'),
        description: t(
          'Use Passkey or 2FA to confirm your identity before revealing this channel key.'
        ),
      })
    } catch (error) {
      if (error instanceof Error) {
        toast.error(error.message)
      }
    }
  }, [channelId, fetchChannelKey, withVerification])

  const refreshCodexCredential = useCallback(async () => {
    if (!channelId) return

    setIsCodexCredentialRefreshing(true)
    try {
      const res = await refreshCodexCredentialRequest(channelId)
      if (!res.success) {
        throw new Error(
          getChannelErrorMessage(
            res.error_code,
            res.message || t('Failed to refresh credential')
          )
        )
      }
      toast.success(t('Credential refreshed'))
      queryClient.invalidateQueries({
        queryKey: channelsQueryKeys.detail(channelId),
      })
    } catch (error) {
      toast.error(error instanceof Error ? error.message : t('Refresh failed'))
    } finally {
      setIsCodexCredentialRefreshing(false)
    }
  }, [channelId, queryClient, t])

  return {
    channelKey,
    fetchChannelKey,
    isChannelKeyLoading,
    isCodexCredentialRefreshing,
    refreshCodexCredential,
    resetChannelKey,
    revealChannelKey,
  }
}
