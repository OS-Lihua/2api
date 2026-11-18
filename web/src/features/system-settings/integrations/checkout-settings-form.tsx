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
import { useMemo } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import { ErrorState } from '@/components/error-state'
import { Form } from '@/components/ui/form'
import { getServerErrorMessage } from '@/lib/server-error-message'

import { SettingsForm } from '../components/settings-form-layout'
import { SettingsPageFormActions } from '../components/settings-page-context'
import { useUpdateOption } from '../hooks/use-update-option'
import { CheckoutProviderEditor } from './checkout-provider-editor'
import {
  checkoutConfigSchema,
  checkoutConfigToForm,
  checkoutFormToConfig,
  checkoutSettingsSchema,
  type CheckoutSettingsValues,
} from './checkout-settings-schema'

export function CheckoutSettingsSection(props: {
  config: string
  complianceConfirmed: boolean
}) {
  const { t } = useTranslation()
  const config = useMemo(() => {
    try {
      const parsed = checkoutConfigSchema.safeParse(JSON.parse(props.config))
      return parsed.success ? parsed.data : null
    } catch {
      return null
    }
  }, [props.config])
  return (
    <section
      className='mb-8 space-y-4'
      aria-label={t('Wallet checkout pricing')}
    >
      <h3 className='text-lg font-medium'>{t('Wallet checkout pricing')}</h3>
      <p className='text-muted-foreground text-sm'>
        {t(
          'Save all gateway pricing rules together. An unconfigured or disabled gateway cannot create new quotes; existing payment callbacks remain available.'
        )}
      </p>
      {config ? (
        <CheckoutSettingsForm
          key={props.config}
          defaultValues={checkoutConfigToForm(config)}
          disabled={!props.complianceConfirmed}
        />
      ) : (
        <ErrorState
          title={t('Invalid checkout configuration')}
          description={t(
            'The saved checkout configuration is invalid. It has not been replaced with defaults. Correct the stored configuration before editing pricing.'
          )}
        />
      )}
    </section>
  )
}
function CheckoutSettingsForm(props: {
  defaultValues: CheckoutSettingsValues
  disabled: boolean
}) {
  const { t } = useTranslation()
  const update = useUpdateOption()
  const form = useForm<CheckoutSettingsValues>({
    resolver: zodResolver(checkoutSettingsSchema),
    defaultValues: props.defaultValues,
    disabled: props.disabled || update.isPending,
  })
  const pending = update.isPending || form.formState.isSubmitting
  async function onSubmit(values: CheckoutSettingsValues) {
    if (props.disabled) return
    form.clearErrors('root')
    try {
      await update.mutateAsync({
        key: 'checkout_setting.config',
        value: JSON.stringify(checkoutFormToConfig(values)),
      })
      form.reset(values)
    } catch (error) {
      form.setError('root', { message: getServerErrorMessage(error) })
    }
  }
  return (
    <Form {...form}>
      <SettingsForm
        onSubmit={form.handleSubmit(onSubmit)}
        data-no-autosubmit='true'
      >
        <SettingsPageFormActions
          onSave={form.handleSubmit(onSubmit)}
          isSaving={pending}
          isSaveDisabled={props.disabled || !form.formState.isDirty}
          saveLabel='Save checkout settings'
        />
        <fieldset
          disabled={props.disabled || pending}
          className='min-w-0 space-y-4'
          aria-busy={pending}
        >
          {props.defaultValues.providers.map((provider, index) => (
            <CheckoutProviderEditor key={provider.key} index={index} />
          ))}
        </fieldset>
        {form.formState.errors.root?.message && (
          <p role='alert' className='text-destructive text-sm'>
            {t(form.formState.errors.root.message)}
          </p>
        )}
      </SettingsForm>
    </Form>
  )
}
