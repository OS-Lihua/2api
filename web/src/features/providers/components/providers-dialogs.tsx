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
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { MultiKeyManageDialog } from '@/features/channels/components/dialogs/multi-key-manage-dialog'
import { handleServerError } from '@/lib/handle-server-error'

import { deleteProvider, providerKeyApi, providersQueryKeys } from '../api'
import { SUCCESS_MESSAGES } from '../constants'
import { ProviderMutateDrawer } from './provider-mutate-drawer'
import { useProviders } from './providers-provider'

export function ProvidersDialogs() {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const { open, setOpen, currentRow } = useProviders()
  const [isDeleting, setIsDeleting] = useState(false)
  const close = (isOpen: boolean) => !isOpen && setOpen(null)
  const refreshList = () =>
    queryClient.invalidateQueries({ queryKey: providersQueryKeys.lists() })

  const handleDelete = async () => {
    if (!currentRow) return
    setIsDeleting(true)
    try {
      const result = await deleteProvider(currentRow.id)
      if (!result.success) {
        handleServerError(result)
        return
      }
      toast.success(t(SUCCESS_MESSAGES.DELETED))
      setOpen(null)
      void refreshList()
    } catch (error) {
      handleServerError(error)
    } finally {
      setIsDeleting(false)
    }
  }

  return (
    <>
      <ProviderMutateDrawer
        open={open === 'create' || open === 'update'}
        onOpenChange={close}
        currentRow={open === 'update' ? (currentRow ?? undefined) : undefined}
      />
      <ConfirmDialog
        destructive
        open={open === 'delete'}
        onOpenChange={close}
        title={t('Delete provider')}
        desc={t(
          'Delete provider "{{name}}"? Requests can no longer use this account.',
          { name: currentRow?.name ?? '' }
        )}
        confirmText={t('Delete')}
        isLoading={isDeleting}
        handleConfirm={handleDelete}
      />
      <MultiKeyManageDialog
        open={open === 'keys'}
        onOpenChange={close}
        target={currentRow}
        keyApi={providerKeyApi}
        description={t('Manage the keys of this provider')}
        onKeysChanged={() => void refreshList()}
      />
    </>
  )
}
