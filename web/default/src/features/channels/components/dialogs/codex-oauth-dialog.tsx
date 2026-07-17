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
import { Check, Copy, ExternalLink, Loader2 } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { Dialog } from '@/components/dialog'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { useCopyToClipboard } from '@/hooks/use-copy-to-clipboard'
import { tryPrettyJson } from '@/lib/utils'

import { completeCodexOAuth, startCodexOAuth } from '../../api'
import { getChannelErrorMessage } from '../../lib/channel-error-messages'

type CodexOAuthDialogProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  onKeyGenerated: (key: string) => void
}

export function CodexOAuthDialog(props: CodexOAuthDialogProps) {
  const { t } = useTranslation()
  const { copiedText, copyToClipboard } = useCopyToClipboard({ notify: false })
  const [authorizeUrl, setAuthorizeUrl] = useState('')
  const [callbackUrl, setCallbackUrl] = useState('')
  const [isStarting, setIsStarting] = useState(false)
  const [isCompleting, setIsCompleting] = useState(false)

  useEffect(() => {
    if (!props.open) {
      setAuthorizeUrl('')
      setCallbackUrl('')
      setIsStarting(false)
      setIsCompleting(false)
    }
  }, [props.open])

  const canComplete = useMemo(
    () => Boolean(callbackUrl.trim()) && !isCompleting,
    [callbackUrl, isCompleting]
  )

  const handleStart = async (): Promise<void> => {
    setIsStarting(true)
    try {
      const response = await startCodexOAuth()
      const url = response.data?.authorize_url
      if (!response.success || !url) {
        throw new Error(response.message || t('OAuth start failed'))
      }
      setAuthorizeUrl(url)
      window.open(url, '_blank', 'noopener,noreferrer')
    } catch (error) {
      toast.error(
        error instanceof Error ? error.message : t('OAuth start failed')
      )
    } finally {
      setIsStarting(false)
    }
  }

  const handleComplete = async (): Promise<void> => {
    setIsCompleting(true)
    try {
      const response = await completeCodexOAuth(callbackUrl.trim())
      const key = response.data?.key
      if (!response.success || !key) {
        throw new Error(
          getChannelErrorMessage(
            response.error_code,
            response.message || t('OAuth failed')
          )
        )
      }
      props.onKeyGenerated(tryPrettyJson(key))
      toast.success(t('Credential generated'))
      props.onOpenChange(false)
    } catch (error) {
      toast.error(error instanceof Error ? error.message : t('OAuth failed'))
    } finally {
      setIsCompleting(false)
    }
  }

  return (
    <Dialog
      open={props.open}
      onOpenChange={props.onOpenChange}
      title={t('Codex Authorization')}
      description={t(
        'Sign in to ChatGPT and generate a Codex OAuth credential.'
      )}
      contentClassName='sm:max-w-2xl'
      contentHeight='auto'
      bodyClassName='space-y-4'
      footer={
        <>
          <Button
            type='button'
            variant='outline'
            onClick={() => props.onOpenChange(false)}
            disabled={isStarting || isCompleting}
          >
            {t('Cancel')}
          </Button>
          <Button onClick={handleComplete} disabled={!canComplete}>
            {isCompleting && <Loader2 className='mr-2 h-4 w-4 animate-spin' />}
            {isCompleting ? t('Generating...') : t('Generate credential')}
          </Button>
        </>
      }
    >
      <Alert>
        <AlertDescription>
          {t(
            'After signing in, copy the full callback URL from the browser address bar and paste it here.'
          )}
        </AlertDescription>
      </Alert>
      <div className='flex flex-wrap gap-2'>
        <Button type='button' onClick={handleStart} disabled={isStarting}>
          {isStarting ? (
            <Loader2 className='mr-2 h-4 w-4 animate-spin' />
          ) : (
            <ExternalLink className='mr-2 h-4 w-4' />
          )}
          {t('Open authorization page')}
        </Button>
        <Button
          type='button'
          variant='outline'
          disabled={!authorizeUrl || isStarting}
          onClick={() => copyToClipboard(authorizeUrl)}
        >
          {copiedText === authorizeUrl ? (
            <Check className='mr-2 h-4 w-4' />
          ) : (
            <Copy className='mr-2 h-4 w-4' />
          )}
          {t('Copy authorization link')}
        </Button>
      </div>
      <div className='space-y-2'>
        <label className='text-sm font-medium' htmlFor='codex-oauth-callback'>
          {t('Callback URL')}
        </label>
        <Input
          id='codex-oauth-callback'
          value={callbackUrl}
          onChange={(event) => setCallbackUrl(event.target.value)}
          placeholder={t(
            'Paste the full callback URL (includes code and state)'
          )}
          autoComplete='off'
          spellCheck={false}
        />
      </div>
    </Dialog>
  )
}
