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
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useMemo, useState } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import { z } from 'zod'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { MultiSelect } from '@/components/multi-select'
import { Button } from '@/components/ui/button'
import {
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { handleServerError } from '@/lib/handle-server-error'
import {
  getServerErrorMessage,
  requireServerSuccess,
} from '@/lib/server-error-message'

import {
  getKeyOfferings,
  getKeyPermissions,
  updateKeyPermissions,
  type KeyOfferingChoice,
  type KeyPermissions,
} from '../api'

export function KeyPermissionsEditor(props: {
  tokenId: number
  onSaved: () => void
  onDirtyChange?: (dirty: boolean) => void
}) {
  const [reloadCount, setReloadCount] = useState(0)
  const query = useQuery({
    queryKey: ['key-permissions', props.tokenId],
    queryFn: async () => {
      const [permissions, choices] = await Promise.all([
        getKeyPermissions(props.tokenId),
        getKeyOfferings(),
      ])
      return {
        permissions: requireServerSuccess(permissions).data,
        choices: requireServerSuccess(choices).data,
      }
    },
    gcTime: 0,
    staleTime: 0,
    refetchOnWindowFocus: false,
  })
  const { t } = useTranslation()
  if (query.isPending) return <LoadingState />
  if (!query.data?.permissions || !query.data.choices) {
    return <ErrorState onRetry={() => void query.refetch()} />
  }
  const reloadPermissions = () => {
    void query.refetch().then((result) => {
      if (result.isSuccess) setReloadCount((count) => count + 1)
    })
  }
  return (
    <>
      {query.isError && (
        <ErrorState
          className='min-h-0 py-3'
          title={t('Failed to get token permissions')}
          description={getServerErrorMessage(query.error)}
          onRetry={reloadPermissions}
        />
      )}
      <KeyPermissionsForm
        key={`${props.tokenId}:${query.data.permissions.revision}:${reloadCount}`}
        tokenId={props.tokenId}
        permissions={query.data.permissions}
        choices={query.data.choices}
        onSaved={props.onSaved}
        onDirtyChange={props.onDirtyChange}
        onReload={reloadPermissions}
        reloading={query.isFetching}
        title={t('Key permissions')}
      />
    </>
  )
}

function KeyPermissionsForm(props: {
  tokenId: number
  permissions: KeyPermissions
  choices: KeyOfferingChoice[]
  onSaved: () => void
  onDirtyChange?: (dirty: boolean) => void
  onReload: () => void
  reloading: boolean
  title: string
}) {
  const { t } = useTranslation()
  const client = useQueryClient()
  const [confirm, setConfirm] = useState(false)
  const schema = useMemo(
    () =>
      z.object({
        default_allow: z.boolean(),
        exception_ids: z
          .array(z.number().int().positive())
          .max(
            1000,
            t('Select at most {{max}} Offering exceptions', { max: 1000 })
          ),
        revision: z.number().int().nonnegative(),
      }),
    [t]
  )
  const form = useForm<KeyPermissions>({
    resolver: zodResolver(schema),
    defaultValues: props.permissions,
  })
  const dirty = form.formState.isDirty
  const onDirtyChange = props.onDirtyChange
  useEffect(() => {
    onDirtyChange?.(dirty)
    return () => onDirtyChange?.(false)
  }, [dirty, onDirtyChange])
  const allow = form.watch('default_allow')
  const choices = props.choices.map((choice) => ({
    value: String(choice.id),
    label: choice.name,
  }))
  const visible = new Set(props.choices.map((choice) => choice.id))
  for (const id of props.permissions.exception_ids) {
    if (!visible.has(id)) {
      choices.push({
        value: String(id),
        label: t('Unavailable Offering #{{id}}', { id }),
      })
    }
  }
  const mutation = useMutation({
    mutationFn: async (values: KeyPermissions) =>
      requireServerSuccess(await updateKeyPermissions(props.tokenId, values)),
    onSuccess: () => {
      setConfirm(false)
      toast.success(t('Key permissions saved'))
      void client.invalidateQueries({
        queryKey: ['key-permissions', props.tokenId],
      })
      props.onSaved()
    },
    onError: (error) => {
      setConfirm(false)
      handleServerError(error)
    },
  })
  const busy = mutation.isPending || props.reloading
  return (
    <Form {...form}>
      <div
        className='space-y-4'
        role='group'
        aria-label={props.title}
        aria-busy={busy}
      >
        <FormField
          control={form.control}
          name='default_allow'
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('Default policy')}</FormLabel>
              <FormControl>
                <NativeSelect
                  value={field.value ? 'allow' : 'deny'}
                  onChange={(event) =>
                    field.onChange(event.target.value === 'allow')
                  }
                  disabled={busy}
                >
                  <NativeSelectOption value='allow'>
                    {t('Allow by default')}
                  </NativeSelectOption>
                  <NativeSelectOption value='deny'>
                    {t('Deny by default')}
                  </NativeSelectOption>
                </NativeSelect>
              </FormControl>
              <FormDescription>
                {allow
                  ? t(
                      'Newly listed visible Offerings are allowed. Exceptions are denied.'
                    )
                  : t(
                      'Newly listed Offerings are denied. Only visible exceptions are allowed.'
                    )}
              </FormDescription>
            </FormItem>
          )}
        />
        <FormField
          control={form.control}
          name='exception_ids'
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('Offering exceptions')}</FormLabel>
              <FormControl>
                <MultiSelect
                  options={choices}
                  selected={field.value.map(String)}
                  onChange={(ids) => field.onChange(ids.map(Number))}
                  disabled={busy}
                  placeholder={t('Select Offerings')}
                />
              </FormControl>
              <FormDescription>
                {t(
                  'Permissions always stay within your visible Offerings. Hidden exceptions are retained but do not grant access.'
                )}
              </FormDescription>
              <FormMessage />
            </FormItem>
          )}
        />
        <div className='flex flex-wrap gap-2'>
          <Button
            type='button'
            variant='outline'
            disabled={busy}
            onClick={() => {
              form.setValue('default_allow', true, { shouldDirty: true })
              form.setValue('exception_ids', [], { shouldDirty: true })
            }}
          >
            {t('Allow all')}
          </Button>
          <Button
            type='button'
            variant='outline'
            disabled={busy}
            onClick={() => {
              form.setValue('default_allow', false, { shouldDirty: true })
              form.setValue('exception_ids', [], { shouldDirty: true })
            }}
          >
            {t('Deny all')}
          </Button>
          <Button
            type='button'
            variant='outline'
            disabled={busy}
            onClick={props.onReload}
          >
            {t('Reload permissions')}
          </Button>
          <Button
            type='button'
            disabled={busy || !form.formState.isDirty}
            onClick={form.handleSubmit(() => setConfirm(true))}
          >
            {t('Save permissions')}
          </Button>
        </div>
      </div>
      <ConfirmDialog
        open={confirm}
        onOpenChange={setConfirm}
        title={t('Save Key permissions?')}
        desc={t('These permissions take effect immediately for this Key.')}
        confirmText={t('Save permissions')}
        isLoading={mutation.isPending}
        handleConfirm={form.handleSubmit((values) => mutation.mutate(values))}
      />
    </Form>
  )
}
