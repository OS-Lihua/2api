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
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { isAxiosError } from 'axios'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { createServerError, getServerMessage } from '@/lib/server-error-message'

import { deleteModel, deleteModels } from '../../api'
import type { Model } from '../../types'
import { invalidateVendorData } from '../../vendor-api'

interface ModelDeleteDialogProps {
  models: Pick<Model, 'id' | 'model_name' | 'name_rule'>[]
  onClose: () => void
  onSuccess?: () => void
}

export function ModelDeleteDialog(props: ModelDeleteDialogProps) {
  const { t } = useTranslation()
  const client = useQueryClient()
  const mutation = useMutation({
    mutationFn: async () => {
      const ids = props.models.map((model) => model.id)
      const response =
        ids.length === 1 ? await deleteModel(ids[0]) : await deleteModels(ids)
      if (!response.success) {
        throw createServerError(response, t('Failed to delete model'))
      }
      return response.data
    },
    onSuccess: async (result) => {
      await invalidateVendorData(client)
      toast.success(
        t('Successfully deleted {{count}} model(s)', {
          count: result.deleted_count,
        })
      )
      props.onSuccess?.()
      props.onClose()
    },
  })
  const description =
    props.models.length === 1
      ? t('Delete model "{{name}}"?', { name: props.models[0].model_name })
      : t('Delete {{count}} models?', { count: props.models.length })
  let errorMessage = mutation.error?.message
  if (isAxiosError(mutation.error)) {
    errorMessage =
      getServerMessage(mutation.error.response?.data) || errorMessage
  }
  return (
    <ConfirmDialog
      open
      onOpenChange={(open) => {
        if (!open && !mutation.isPending) props.onClose()
      }}
      title={t('Delete Models?')}
      desc={<span className='wrap-anywhere'>{description}</span>}
      confirmText={t('Delete')}
      destructive
      disabled={!props.models.length}
      isLoading={mutation.isPending}
      handleConfirm={() => mutation.mutate()}
    >
      <div className='space-y-3'>
        <p className='text-muted-foreground text-sm'>
          {t(
            'Channels and Offerings are not deleted with Model metadata. Referenced Models cannot be deleted.'
          )}
        </p>
        {mutation.isError && (
          <p role='alert' className='text-destructive text-sm'>
            {errorMessage || t('Failed to delete model')}
          </p>
        )}
      </div>
    </ConfirmDialog>
  )
}
