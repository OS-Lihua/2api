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
import {
  skipToken,
  useMutation,
  useQuery,
  useQueryClient,
} from '@tanstack/react-query'
import { useEffect, useState } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Button } from '@/components/ui/button'
import { Form } from '@/components/ui/form'
import { getServerErrorMessage } from '@/lib/server-error-message'

import {
  getOffering,
  getOfferingChoices,
  offeringKeys,
  saveOffering,
  type OfferingChoices,
} from '../api'
import {
  offeringFormSchema,
  offeringFormValues,
  offeringPayload,
  type OfferingFormValues,
} from '../lib/offering-form'
import type { ModelOffering } from '../types'
import { ChannelScopeEditor } from './channel-scope-editor'
import { MemberTierEditor } from './member-tier-editor'
import { OfferingIdentityFields, OfferingRouteFields } from './offering-fields'

export function OfferingDialog(props: {
  id: number | null
  onClose: () => void
}) {
  const { t } = useTranslation()
  const id = props.id
  const choices = useQuery({
    queryKey: ['model-offering-choices'],
    queryFn: getOfferingChoices,
  })
  const detail = useQuery({
    queryKey: [...offeringKeys.all, 'detail', id],
    queryFn: id === null ? skipToken : () => getOffering(id),
    refetchOnMount: 'always',
  })
  const [loadedId, setLoadedId] = useState<number | null>(null)
  useEffect(() => {
    if (detail.isSuccess && detail.isFetchedAfterMount) setLoadedId(id)
  }, [id, detail.isSuccess, detail.isFetchedAfterMount])
  const detailReady = id === null || loadedId === id
  const retry = () => {
    void choices.refetch()
    if (props.id !== null) void detail.refetch()
  }
  if (
    (choices.isError && !choices.data) ||
    (detail.isError && (!detail.data || !detailReady))
  ) {
    return (
      <Dialog
        open
        onOpenChange={(open) => !open && props.onClose()}
        title={t('Edit model offering')}
      >
        <ErrorState
          description={getServerErrorMessage(choices.error ?? detail.error)}
          onRetry={retry}
        />
      </Dialog>
    )
  }
  if (!choices.data || (props.id !== null && (!detail.data || !detailReady))) {
    return (
      <Dialog
        open
        onOpenChange={(open) => !open && props.onClose()}
        title={
          props.id === null ? t('New model offering') : t('Edit model offering')
        }
      >
        <LoadingState />
      </Dialog>
    )
  }
  return (
    <OfferingForm
      id={props.id}
      offering={detail.data}
      choices={choices.data}
      refreshError={choices.error ?? detail.error}
      onRetry={retry}
      onClose={props.onClose}
    />
  )
}

function OfferingForm(props: {
  id: number | null
  offering?: ModelOffering
  choices: OfferingChoices
  refreshError: unknown
  onRetry: () => void
  onClose: () => void
}) {
  const { t } = useTranslation()
  const client = useQueryClient()
  const [discard, setDiscard] = useState(false)
  const [aliasDraft, setAliasDraft] = useState('')
  const [lossDraft, setLossDraft] = useState('')
  const hasPendingDraft = aliasDraft.trim() !== '' || lossDraft.trim() !== ''
  const form = useForm<OfferingFormValues>({
    resolver: zodResolver(offeringFormSchema(t)),
    defaultValues: offeringFormValues(props.offering),
  })
  const isDirty = form.formState.isDirty
  useEffect(() => {
    if (!isDirty && !hasPendingDraft) {
      form.reset(offeringFormValues(props.offering))
    }
  }, [form, props.offering, hasPendingDraft, isDirty])
  const mutation = useMutation({
    mutationFn: (values: OfferingFormValues) =>
      saveOffering(props.id, offeringPayload(values)),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: offeringKeys.all })
      toast.success(t('Model offering saved'))
      props.onClose()
    },
    onError: (error) =>
      form.setError('root', { message: getServerErrorMessage(error) }),
  })
  const requestClose = () => {
    if (mutation.isPending) return
    if (isDirty || hasPendingDraft) setDiscard(true)
    else props.onClose()
  }
  return (
    <>
      <Dialog
        open
        onOpenChange={(open) => !open && requestClose()}
        title={
          props.id === null ? t('New model offering') : t('Edit model offering')
        }
        description={t(
          'Configure the public model name, member tiers and route settings.'
        )}
        contentClassName='sm:max-w-3xl'
        contentHeight='min(70dvh, 760px)'
        footer={
          <>
            <Button
              variant='outline'
              disabled={mutation.isPending}
              onClick={requestClose}
            >
              {t('Cancel')}
            </Button>
            <Button
              form='model-offering-form'
              type='submit'
              disabled={mutation.isPending}
            >
              {mutation.isPending ? t('Saving...') : t('Save model offering')}
            </Button>
          </>
        }
      >
        {props.refreshError != null && (
          <ErrorState
            className='min-h-0 py-3'
            description={getServerErrorMessage(props.refreshError)}
            onRetry={props.onRetry}
          />
        )}
        <Form {...form}>
          <form
            id='model-offering-form'
            noValidate
            onSubmit={form.handleSubmit((values) => mutation.mutate(values))}
            className='space-y-6'
            aria-busy={mutation.isPending}
          >
            <fieldset disabled={mutation.isPending} className='space-y-6'>
              <OfferingIdentityFields
                groups={props.choices.groups}
                disabled={mutation.isPending}
                isUpdate={props.id !== null}
                onAliasDraftChange={setAliasDraft}
              />
              <MemberTierEditor
                models={props.choices.models}
                disabled={mutation.isPending}
              />
              <ChannelScopeEditor
                choices={props.choices}
                disabled={mutation.isPending}
              />
              <OfferingRouteFields
                disabled={mutation.isPending}
                onLossDraftChange={setLossDraft}
              />
            </fieldset>
            {form.formState.errors.root?.message && (
              <p role='alert' className='text-destructive text-sm'>
                {form.formState.errors.root.message}
              </p>
            )}
          </form>
        </Form>
      </Dialog>
      <ConfirmDialog
        open={discard}
        onOpenChange={setDiscard}
        title={t('Discard unsaved changes?')}
        desc={t('Your changes will be lost.')}
        confirmText={t('Discard')}
        destructive
        handleConfirm={props.onClose}
      />
    </>
  )
}
