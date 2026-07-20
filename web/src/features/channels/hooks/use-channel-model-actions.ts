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
import { useCallback, useState } from 'react'
import type { UseFormReturn } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { fetchModels, fetchUpstreamModels } from '../api'
import { MODEL_FETCHABLE_TYPES } from '../constants'
import {
  formatModelsArray,
  mergeFetchedModelsIntoMapping,
  parseModelsString,
  type ChannelFormValues,
} from '../lib'
import { getChannelErrorMessage } from '../lib/channel-error-messages'

type PrefillModelGroup = {
  id: number
  name: string
  items: string | string[]
}

type UseChannelModelActionsProps = {
  form: UseFormReturn<ChannelFormValues>
  currentModels: string[]
  availableModels: string[]
  channelId: number | null
  editing: boolean
  canEditSensitive: boolean
  copyToClipboard: (text: string) => Promise<boolean>
}

export function useChannelModelActions(props: UseChannelModelActionsProps) {
  const { t } = useTranslation()
  const [isFetchingModels, setIsFetchingModels] = useState(false)
  const {
    form,
    currentModels,
    availableModels,
    channelId,
    editing,
    canEditSensitive,
    copyToClipboard,
  } = props

  const updateModels = useCallback(
    (newModels: string[], merge = false) => {
      const finalModels = merge
        ? formatModelsArray([...currentModels, ...newModels])
        : formatModelsArray(newModels)
      form.setValue('models', finalModels)
      return newModels.length
    },
    [currentModels, form]
  )

  const fetchAndFillModels = useCallback(async () => {
    const type = form.getValues('type')
    const existingChannelId = editing ? channelId : null
    if (!MODEL_FETCHABLE_TYPES.has(type)) {
      toast.error(t('This channel type does not support fetching models'))
      return
    }
    if (!editing && !canEditSensitive) {
      toast.error(t("You don't have necessary permission"))
      return
    }
    if (!editing && !form.getValues('key')?.trim()) {
      toast.error(t('Please enter API key first'))
      return
    }
    if (editing && !existingChannelId) {
      toast.error(t('Channel ID is required'))
      return
    }

    setIsFetchingModels(true)
    try {
      const response = existingChannelId
        ? await fetchUpstreamModels(existingChannelId)
        : await fetchModels({
            type,
            key: form.getValues('key'),
            base_url: form.getValues('base_url') || '',
            advanced_custom: form.getValues('advanced_custom') || '',
            header_override: form.getValues('header_override') || '',
            proxy: form.getValues('proxy') || '',
          })
      if (!response.success) {
        throw new Error(
          getChannelErrorMessage(
            response.error_code,
            response.message || t('Failed to fetch models')
          )
        )
      }

      const models = formatModelsArray(
        Array.isArray(response.data) ? response.data : []
      )
      const modelList = parseModelsString(models)
      if (modelList.length === 0) {
        toast.info(t('No models fetched yet.'))
        return
      }
      form.setValue('models', models, {
        shouldDirty: true,
        shouldValidate: true,
      })
      const currentModelMapping = form.getValues('model_mapping') || ''
      const mergedModelMapping = mergeFetchedModelsIntoMapping(
        currentModelMapping,
        modelList
      )
      if (mergedModelMapping !== currentModelMapping) {
        form.setValue('model_mapping', mergedModelMapping, {
          shouldDirty: true,
          shouldValidate: true,
        })
      }
      toast.success(t('Fetched {{count}} models', { count: modelList.length }))
    } catch (error) {
      toast.error(
        error instanceof Error ? error.message : t('Failed to fetch models')
      )
    } finally {
      setIsFetchingModels(false)
    }
  }, [canEditSensitive, channelId, editing, form, t])

  const fillAllModels = useCallback(() => {
    if (!availableModels.length) {
      toast.info(t('No models available'))
      return
    }
    updateModels(availableModels)
    toast.success(
      t('Filled {{count}} model(s)', { count: availableModels.length })
    )
  }, [availableModels, t, updateModels])

  const clearModels = useCallback(() => {
    form.setValue('models', '')
    toast.success(t('Cleared all models'))
  }, [form, t])

  const copyModels = useCallback(async () => {
    const models = form.getValues('models')
    if (!models?.trim()) {
      toast.info(t('No models to copy'))
      return
    }
    await copyToClipboard(models)
  }, [copyToClipboard, form, t])

  const addPrefillGroup = useCallback(
    (group: PrefillModelGroup) => {
      try {
        const items = Array.isArray(group.items)
          ? group.items
          : JSON.parse(group.items)
        if (!Array.isArray(items)) {
          throw new Error('Invalid items format')
        }
        const count = updateModels(items, true)
        toast.success(
          t('Added {{count}} models from "{{name}}"', {
            count,
            name: group.name,
          })
        )
      } catch {
        toast.error(t('Failed to parse group items'))
      }
    },
    [t, updateModels]
  )

  const changeModels = useCallback(
    (selected: string[]) => form.setValue('models', selected.join(',')),
    [form]
  )

  return {
    addPrefillGroup,
    changeModels,
    clearModels,
    copyModels,
    fetchAndFillModels,
    fillAllModels,
    isFetchingModels,
    updateModels,
  }
}
