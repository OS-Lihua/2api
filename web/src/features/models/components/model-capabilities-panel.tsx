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
import { useEffect } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import {
  SideDrawerSection,
  sideDrawerFooterClassName,
  sideDrawerFormClassName,
} from '@/components/drawer-layout'
import { MultiSelect } from '@/components/multi-select'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import {
  Form,
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { SheetFooter } from '@/components/ui/sheet'
import {
  createServerError,
  getServerErrorMessage,
} from '@/lib/server-error-message'

import { updateModelCapabilities } from '../api'
import {
  getCapabilityFeatureOptions,
  getCapabilityLimitOptions,
  getModalityOptions,
} from '../constants'
import { modelsQueryKeys } from '../lib'
import {
  capabilityFormSchema,
  transformCapabilitiesToFormValues,
  transformFormValuesToCapabilities,
  type CapabilityFormValues,
} from '../lib/model-form'
import type { Model } from '../types'

/**
 * Edits the capabilities of a saved exact-name Model. It saves separately
 * from the metadata section.
 */
export function ModelCapabilitiesPanel(props: {
  model: Model
  onDirtyChange: (dirty: boolean) => void
  onClose: () => void
}) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const form = useForm<CapabilityFormValues>({
    resolver: zodResolver(capabilityFormSchema),
    defaultValues: transformCapabilitiesToFormValues(props.model.capabilities),
  })
  const dirty = form.formState.isDirty
  const onDirtyChange = props.onDirtyChange
  useEffect(() => onDirtyChange(dirty), [dirty, onDirtyChange])

  const save = useMutation({
    meta: { errorToast: false },
    onMutate: () => form.clearErrors('root.server'),
    mutationFn: async (values: CapabilityFormValues) => {
      const response = await updateModelCapabilities(
        props.model,
        transformFormValuesToCapabilities(values)
      )
      if (!response.success) {
        throw createServerError(response, t('Operation failed'))
      }
    },
    onSuccess: async (_data, values) => {
      form.reset(values)
      await queryClient.invalidateQueries({ queryKey: modelsQueryKeys.all })
      toast.success(t('Model capabilities saved'))
    },
    onError: (error) => {
      form.setError('root.server', {
        message: getServerErrorMessage(error, t('Operation failed')),
      })
    },
  })
  const modalityOptions = getModalityOptions(t)

  return (
    <>
      <Form {...form}>
        <form
          id='model-capabilities-form'
          onSubmit={form.handleSubmit((values) => save.mutate(values))}
          className={sideDrawerFormClassName()}
        >
          <SideDrawerSection>
            <h3 className='text-sm font-semibold'>{t('Capabilities')}</h3>
            <FormField
              control={form.control}
              name='features'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Supported features')}</FormLabel>
                  <FormControl>
                    <MultiSelect
                      options={getCapabilityFeatureOptions(t)}
                      selected={field.value}
                      onChange={field.onChange}
                      placeholder={t('Select features')}
                      disabled={save.isPending}
                    />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            <FormField
              control={form.control}
              name='input_modalities'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Input modalities')}</FormLabel>
                  <FormControl>
                    <MultiSelect
                      options={modalityOptions}
                      selected={field.value}
                      onChange={field.onChange}
                      placeholder={t('Select input modalities')}
                      disabled={save.isPending}
                    />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            <FormField
              control={form.control}
              name='output_modalities'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Output modalities')}</FormLabel>
                  <FormControl>
                    <MultiSelect
                      options={modalityOptions}
                      selected={field.value}
                      onChange={field.onChange}
                      placeholder={t('Select output modalities')}
                      disabled={save.isPending}
                    />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
          </SideDrawerSection>

          <SideDrawerSection>
            <h3 className='text-sm font-semibold'>{t('Limits')}</h3>
            <p className='text-muted-foreground text-sm'>
              {t('Leave a limit empty when it is not declared.')}
            </p>
            <div className='grid gap-4 sm:grid-cols-2'>
              {getCapabilityLimitOptions(t).map((limit) => (
                <FormField
                  key={limit.value}
                  control={form.control}
                  name={`limits.${limit.value}`}
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{limit.label}</FormLabel>
                      <FormControl>
                        <Input
                          inputMode='numeric'
                          {...field}
                          disabled={save.isPending}
                        />
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />
              ))}
            </div>
          </SideDrawerSection>
        </form>
      </Form>
      <SheetFooter className={sideDrawerFooterClassName('flex-wrap')}>
        {form.formState.errors.root?.server?.message && (
          <Alert variant='destructive' className='col-span-2 basis-full'>
            <AlertDescription className='break-words'>
              {form.formState.errors.root.server.message}
            </AlertDescription>
          </Alert>
        )}
        <Button
          variant='outline'
          onClick={props.onClose}
          disabled={save.isPending}
        >
          {t('Close')}
        </Button>
        <Button
          form='model-capabilities-form'
          type='submit'
          disabled={save.isPending}
        >
          {save.isPending ? t('Saving...') : t('Save capabilities')}
        </Button>
      </SheetFooter>
    </>
  )
}
