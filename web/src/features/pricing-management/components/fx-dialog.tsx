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
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { Dialog } from '@/components/dialog'
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
import { Input } from '@/components/ui/input'
import { getServerErrorMessage } from '@/lib/server-error-message'

import { pricingKeys, saveFxRate } from '../api'
import { fxFormSchema, type FxFormValues } from '../lib/forms'
import type { FxRate } from '../types'
import { CurrencyInput } from './currency-input'

export function FxDialog(props: { initial?: FxRate; onClose: () => void }) {
  const { t } = useTranslation()
  const client = useQueryClient()
  const [discard, setDiscard] = useState(false)
  const form = useForm<FxFormValues>({
    resolver: zodResolver(fxFormSchema(t)),
    defaultValues: {
      from_currency: props.initial?.from_currency ?? '',
      to_currency: props.initial?.to_currency ?? '',
      rate: props.initial?.rate ?? '',
      buffer_rate: props.initial?.buffer_rate ?? '0.02',
    },
  })
  const isDirty = form.formState.isDirty
  const mutation = useMutation({
    mutationFn: saveFxRate,
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: pricingKeys.fx })
      toast.success(t('Exchange rate saved'))
      props.onClose()
    },
    onError: (error) =>
      form.setError('root', { message: getServerErrorMessage(error) }),
  })
  const requestClose = () => {
    if (mutation.isPending) return
    if (isDirty) setDiscard(true)
    else props.onClose()
  }
  return (
    <>
      <Dialog
        open
        onOpenChange={(open) => !open && requestClose()}
        title={t('Maintain exchange rate')}
        description={t(
          'Rates are directional. Accounting uses the raw rate; comparison uses the raw rate multiplied by one plus the buffer. The default buffer is 2%.'
        )}
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
              type='submit'
              form='fx-rate-form'
              disabled={mutation.isPending}
            >
              {mutation.isPending ? t('Saving...') : t('Save exchange rate')}
            </Button>
          </>
        }
      >
        <Form {...form}>
          <form
            id='fx-rate-form'
            noValidate
            className='space-y-4'
            aria-busy={mutation.isPending}
            onSubmit={form.handleSubmit((values) => mutation.mutate(values))}
          >
            <fieldset className='space-y-4' disabled={mutation.isPending}>
              <div className='grid gap-4 sm:grid-cols-2'>
                <FormField
                  control={form.control}
                  name='from_currency'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('From currency')}</FormLabel>
                      <FormControl>
                        <CurrencyInput
                          value={field.value}
                          onValueChange={field.onChange}
                          disabled={mutation.isPending || !!props.initial}
                        />
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />
                <FormField
                  control={form.control}
                  name='to_currency'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('To currency')}</FormLabel>
                      <FormControl>
                        <CurrencyInput
                          value={field.value}
                          onValueChange={field.onChange}
                          disabled={mutation.isPending || !!props.initial}
                        />
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />
              </div>
              <FormField
                control={form.control}
                name='rate'
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>{t('Raw exchange rate')}</FormLabel>
                    <FormControl>
                      <Input {...field} inputMode='decimal' />
                    </FormControl>
                    <FormDescription>
                      {t(
                        'Amount of the destination currency for one unit of the source currency. No inverse rates are inferred.'
                      )}
                    </FormDescription>
                    <FormMessage />
                  </FormItem>
                )}
              />
              <FormField
                control={form.control}
                name='buffer_rate'
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>{t('Comparison buffer rate')}</FormLabel>
                    <FormControl>
                      <Input {...field} inputMode='decimal' />
                    </FormControl>
                    <FormDescription>
                      {t(
                        'Use 0.02 for 2%. Use 0 to disable the comparison buffer.'
                      )}
                    </FormDescription>
                    <FormMessage />
                  </FormItem>
                )}
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
