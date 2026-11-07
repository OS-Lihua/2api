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
import { useRef, useState } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { DateTimePicker } from '@/components/datetime-picker'
import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { Button } from '@/components/ui/button'
import { ComboboxInput } from '@/components/ui/combobox-input'
import {
  Form,
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { splitBillingExprAndRequestRules } from '@/features/pricing/lib/billing-expr'
import { TieredPricingEditor } from '@/features/system-settings/models/tiered-pricing-editor'
import { getServerErrorMessage } from '@/lib/server-error-message'

import { createPriceVersion, getPriceOwners, pricingKeys } from '../api'
import { priceFormSchema, type PriceFormValues } from '../lib/forms'
import { CurrencyInput } from './currency-input'

export function PriceDialog(props: { onClose: () => void }) {
  const { t } = useTranslation()
  const client = useQueryClient()
  const [discard, setDiscard] = useState(false)
  const formElement = useRef<HTMLFormElement>(null)
  const form = useForm<PriceFormValues>({
    resolver: zodResolver(priceFormSchema(t)),
    defaultValues: {
      owner_type: 'model_offering',
      owner_id: '',
      currency: 'USD',
      expression: 'tier("base", p * 0 + c * 0)',
      activation: 'immediate',
    },
  })
  const expressionParts = splitBillingExprAndRequestRules(
    form.watch('expression')
  )
  const isDirty = form.formState.isDirty
  const ownerType = form.watch('owner_type')
  const owners = useQuery({
    queryKey: ['price-owners', ownerType],
    queryFn: () => getPriceOwners(ownerType),
    refetchOnMount: 'always',
  })
  const mutation = useMutation({
    mutationFn: (values: PriceFormValues) =>
      createPriceVersion({
        owner_type: values.owner_type,
        owner_id: Number(values.owner_id),
        currency: values.currency,
        expression: values.expression,
        ...(values.activation === 'scheduled' && values.effective_at
          ? { effective_at: Math.floor(values.effective_at.getTime() / 1000) }
          : {}),
      }),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: pricingKeys.prices })
      toast.success(t('Price version created'))
      props.onClose()
    },
    onError: (error) =>
      form.setError('root', { message: getServerErrorMessage(error) }),
  })
  const requestClose = () => {
    if (mutation.isPending) return
    if (
      isDirty ||
      formElement.current?.querySelector('[data-billing-invalid="true"]')
    ) {
      setDiscard(true)
    } else props.onClose()
  }
  return (
    <>
      <Dialog
        open
        onOpenChange={(open) => !open && requestClose()}
        title={t('New price version')}
        description={t(
          'Price versions cannot be edited or deleted. The server validates the expression before saving.'
        )}
        contentClassName='sm:max-w-4xl'
        contentHeight='min(70dvh, 800px)'
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
              form='price-version-form'
              disabled={mutation.isPending}
            >
              {mutation.isPending ? t('Saving...') : t('Create price version')}
            </Button>
          </>
        }
      >
        <Form {...form}>
          <form
            id='price-version-form'
            ref={formElement}
            noValidate
            aria-busy={mutation.isPending}
            className='space-y-4'
            onSubmit={form.handleSubmit((values) => {
              if (
                formElement.current?.querySelector(
                  '[data-billing-invalid="true"]'
                )
              ) {
                form.setError('root', {
                  message: t(
                    'Complete the invalid expression fields before saving.'
                  ),
                })
                return
              }
              mutation.mutate(values)
            })}
          >
            <fieldset disabled={mutation.isPending} className='space-y-4'>
              <div className='grid gap-4 sm:grid-cols-2'>
                <FormField
                  control={form.control}
                  name='owner_type'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('Price owner type')}</FormLabel>
                      <FormControl>
                        <NativeSelect
                          {...field}
                          className='w-full'
                          onChange={(event) => {
                            field.onChange(event)
                            form.setValue('owner_id', '', { shouldDirty: true })
                          }}
                        >
                          <NativeSelectOption value='model_offering'>
                            {t('Model offering')}
                          </NativeSelectOption>
                          <NativeSelectOption value='channel'>
                            {t('Channel')}
                          </NativeSelectOption>
                        </NativeSelect>
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />
                <FormField
                  control={form.control}
                  name='owner_id'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('Price owner ID')}</FormLabel>
                      <FormControl>
                        <ComboboxInput
                          value={field.value}
                          onValueChange={field.onChange}
                          options={owners.data ?? []}
                          allowCustomValue
                          disabled={mutation.isPending}
                          placeholder={t('Select an owner or enter its ID')}
                        />
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />
              </div>
              {owners.data === null && (
                <p className='text-muted-foreground text-sm'>
                  {t(
                    'Owner names are unavailable with your permissions. Enter the owner ID to continue.'
                  )}
                </p>
              )}
              {owners.isError && (
                <ErrorState
                  className='min-h-0 py-2'
                  description={getServerErrorMessage(owners.error)}
                  onRetry={() => void owners.refetch()}
                />
              )}
              <FormField
                control={form.control}
                name='currency'
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>{t('Currency')}</FormLabel>
                    <FormControl>
                      <CurrencyInput
                        value={field.value}
                        onValueChange={field.onChange}
                        disabled={mutation.isPending}
                      />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
              <FormField
                control={form.control}
                name='activation'
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>{t('Activation time')}</FormLabel>
                    <FormControl>
                      <NativeSelect {...field} className='w-full'>
                        <NativeSelectOption value='immediate'>
                          {t('Immediately')}
                        </NativeSelectOption>
                        <NativeSelectOption value='scheduled'>
                          {t('Scheduled')}
                        </NativeSelectOption>
                      </NativeSelect>
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
              {form.watch('activation') === 'scheduled' && (
                <FormField
                  control={form.control}
                  name='effective_at'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('Effective at')}</FormLabel>
                      <FormControl>
                        <DateTimePicker
                          value={field.value}
                          onChange={field.onChange}
                          disabled={mutation.isPending}
                          placeholder={t('Select activation date')}
                          aria-label={t('Effective at')}
                        />
                      </FormControl>
                      <p className='text-muted-foreground text-xs'>
                        {t('Dates and times use your local time zone.')}
                      </p>
                      <FormMessage />
                    </FormItem>
                  )}
                />
              )}
              <TieredPricingEditor
                nativeCurrency={form.watch('currency') || 'USD'}
                billingExpr={expressionParts.billingExpr}
                requestRuleExpr={expressionParts.requestRuleExpr}
                onExpressionChange={(value) =>
                  form.setValue('expression', value, { shouldDirty: true })
                }
              />
              {form.formState.errors.expression?.message && (
                <p role='alert' className='text-destructive text-sm'>
                  {form.formState.errors.expression.message}
                </p>
              )}
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
