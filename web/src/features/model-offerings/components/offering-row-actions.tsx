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
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { Button } from '@/components/ui/button'
import { getServerErrorMessage } from '@/lib/server-error-message'

import { offeringAction, offeringKeys, type OfferingAction } from '../api'
import type { ModelOffering } from '../types'

export function OfferingRowActions(props: {
  offering: ModelOffering
  onEdit: () => void
}) {
  const { t } = useTranslation()
  const client = useQueryClient()
  const [action, setAction] = useState<OfferingAction | null>(null)
  const mutation = useMutation({
    mutationFn: (operation: OfferingAction) =>
      offeringAction(props.offering.id, operation),
    onSuccess: () => {
      setAction(null)
      void client.invalidateQueries({ queryKey: offeringKeys.all })
    },
    meta: { errorToast: false },
  })
  let title = t('Enable model offering?')
  let description = t('Enable {{name}}? A sale price is required.', {
    name: props.offering.name,
  })
  let confirmText = t('Enable')
  if (action === 'disable') {
    title = t('Disable model offering?')
    description = t(
      'Disable {{name}}? Users will no longer be able to call it.',
      { name: props.offering.name }
    )
    confirmText = t('Disable')
  }
  if (action === 'delete') {
    title = t('Delete model offering?')
    description = t('Delete {{name}} and its aliases? This cannot be undone.', {
      name: props.offering.name,
    })
    confirmText = t('Delete')
  }
  return (
    <>
      <div className='flex flex-wrap gap-2'>
        <Button size='sm' variant='outline' onClick={props.onEdit}>
          {t('Edit')}
        </Button>
        <Button
          size='sm'
          variant='outline'
          onClick={() => {
            mutation.reset()
            setAction(props.offering.enabled ? 'disable' : 'enable')
          }}
        >
          {props.offering.enabled ? t('Disable') : t('Enable')}
        </Button>
        <Button
          size='sm'
          variant='destructive'
          onClick={() => {
            mutation.reset()
            setAction('delete')
          }}
        >
          {t('Delete')}
        </Button>
      </div>
      <ConfirmDialog
        open={action !== null}
        onOpenChange={(open) => {
          if (!open && !mutation.isPending) setAction(null)
        }}
        title={title}
        desc={description}
        confirmText={confirmText}
        destructive={action === 'delete'}
        isLoading={mutation.isPending}
        handleConfirm={() => action && mutation.mutate(action)}
      >
        {mutation.isError && (
          <p role='alert' className='text-destructive text-sm'>
            {getServerErrorMessage(mutation.error)}
          </p>
        )}
      </ConfirmDialog>
    </>
  )
}
