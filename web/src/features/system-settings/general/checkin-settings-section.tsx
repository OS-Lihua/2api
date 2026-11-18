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
import { useMemo, useRef } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import { ErrorState } from '@/components/error-state'
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
import { Switch } from '@/components/ui/switch'
import { CurrencyInput } from '@/features/pricing-management/components/currency-input'
import { moneyAmountToDecimal, parseDecimalMoneyAmount } from '@/lib/currency'
import { getServerErrorMessage } from '@/lib/server-error-message'

import {
  SettingsForm,
  SettingsSwitchContent,
  SettingsSwitchItem,
} from '../components/settings-form-layout'
import { SettingsPageFormActions } from '../components/settings-page-context'
import { SettingsSection } from '../components/settings-section'
import { useUpdateOption } from '../hooks/use-update-option'
import {
  checkinRewardRangeSchema,
  checkinSettingsSchema,
  type CheckinSettingsValues,
} from '../lib/checkin-settings'

export function CheckinSettingsSection(props: {
  defaultValues: { enabled: boolean; rewardRange: string }
}) {
  const { t } = useTranslation()
  const range = useMemo(() => {
    try {
      const parsed = checkinRewardRangeSchema.safeParse(
        JSON.parse(props.defaultValues.rewardRange)
      )
      return parsed.success ? parsed.data : null
    } catch {
      return null
    }
  }, [props.defaultValues.rewardRange])

  return (
    <SettingsSection title={t('Check-in Settings')}>
      {range ? (
        <CheckinSettingsForm
          defaultValues={{
            enabled: props.defaultValues.enabled,
            currency: range.min.currency,
            min: moneyAmountToDecimal(range.min),
            max: moneyAmountToDecimal(range.max),
          }}
        />
      ) : (
        <ErrorState description={t('Invalid check-in reward range')} />
      )}
    </SettingsSection>
  )
}

function CheckinSettingsForm(props: { defaultValues: CheckinSettingsValues }) {
  const { t } = useTranslation()
  const updateOption = useUpdateOption()
  const saved = useRef(props.defaultValues)
  const form = useForm<CheckinSettingsValues>({
    resolver: zodResolver(checkinSettingsSchema),
    defaultValues: props.defaultValues,
  })
  const { isDirty, isSubmitting } = form.formState
  const enabled = form.watch('enabled')
  const pending = updateOption.isPending || isSubmitting

  async function onSubmit(values: CheckinSettingsValues) {
    form.clearErrors('root')
    try {
      if (
        values.min !== saved.current.min ||
        values.max !== saved.current.max ||
        values.currency !== saved.current.currency
      ) {
        await updateOption.mutateAsync({
          key: 'checkin_setting.reward_range',
          value: JSON.stringify({
            min: parseDecimalMoneyAmount(values.min, values.currency),
            max: parseDecimalMoneyAmount(values.max, values.currency),
          }),
        })
      }
      if (values.enabled !== saved.current.enabled) {
        await updateOption.mutateAsync({
          key: 'checkin_setting.enabled',
          value: String(values.enabled),
        })
      }
      saved.current = values
      form.reset(values)
    } catch (error) {
      form.setError('root', { message: getServerErrorMessage(error) })
    }
  }

  return (
    <Form {...form}>
      <SettingsForm onSubmit={form.handleSubmit(onSubmit)} autoComplete='off'>
        <SettingsPageFormActions
          onSave={form.handleSubmit(onSubmit)}
          isSaving={pending}
          isSaveDisabled={!isDirty}
          saveLabel='Save check-in settings'
        />
        <FormField
          control={form.control}
          name='enabled'
          render={({ field }) => (
            <SettingsSwitchItem>
              <SettingsSwitchContent>
                <FormLabel>{t('Enable check-in feature')}</FormLabel>
                <FormDescription>
                  {t('Allow users to check in daily for random rewards')}
                </FormDescription>
              </SettingsSwitchContent>
              <FormControl>
                <Switch
                  checked={field.value}
                  onCheckedChange={field.onChange}
                  disabled={pending}
                />
              </FormControl>
            </SettingsSwitchItem>
          )}
        />
        {enabled && (
          <div className='grid gap-6 sm:grid-cols-2'>
            <FormField
              control={form.control}
              name='currency'
              render={({ field }) => (
                <FormItem className='sm:col-span-2'>
                  <FormLabel>{t('Reward currency')}</FormLabel>
                  <FormControl>
                    <CurrencyInput
                      value={field.value}
                      onValueChange={field.onChange}
                      disabled={pending}
                    />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            <FormField
              control={form.control}
              name='min'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Minimum check-in reward')}</FormLabel>
                  <FormControl>
                    <Input
                      {...field}
                      type='text'
                      inputMode='decimal'
                      disabled={pending}
                    />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            <FormField
              control={form.control}
              name='max'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Maximum check-in reward')}</FormLabel>
                  <FormControl>
                    <Input
                      {...field}
                      type='text'
                      inputMode='decimal'
                      disabled={pending}
                    />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
          </div>
        )}
        {form.formState.errors.root && (
          <p role='alert' className='text-destructive text-sm'>
            {form.formState.errors.root.message}
          </p>
        )}
      </SettingsForm>
    </Form>
  )
}
