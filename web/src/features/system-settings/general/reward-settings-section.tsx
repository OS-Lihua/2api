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
import { Alert, AlertDescription } from '@/components/ui/alert'
import {
  Form,
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { CurrencyInput } from '@/features/pricing-management/components/currency-input'
import { getServerErrorMessage } from '@/lib/server-error-message'

import { FormDirtyIndicator } from '../components/form-dirty-indicator'
import { FormNavigationGuard } from '../components/form-navigation-guard'
import {
  SettingsForm,
  SettingsFormGrid,
} from '../components/settings-form-layout'
import { SettingsPageFormActions } from '../components/settings-page-context'
import { SettingsSection } from '../components/settings-section'
import { useUpdateOption } from '../hooks/use-update-option'
import {
  REWARD_FIELDS,
  parseRewardConfig,
  rewardConfigToForm,
  rewardFormToConfig,
  rewardSettingsSchema,
  type RewardSettingsValues,
} from '../lib/reward-settings'

const rewardLabels = {
  signup: 'Signup Reward',
  inviter: 'Inviter Reward',
  invitee: 'Invitee Reward',
} as const
const complianceMessage =
  'Non-zero invitation rewards require compliance confirmation in Payment Gateway settings.'

export function RewardSettingsSection(props: {
  config: string
  complianceConfirmed: boolean
}) {
  const { t } = useTranslation()
  const config = useMemo(() => parseRewardConfig(props.config), [props.config])

  return (
    <SettingsSection title={t('Reward Settings')}>
      {config ? (
        <RewardSettingsForm
          key={props.config}
          defaultValues={rewardConfigToForm(config)}
          complianceConfirmed={props.complianceConfirmed}
        />
      ) : (
        <ErrorState description={t('Invalid reward configuration')} />
      )}
    </SettingsSection>
  )
}

function RewardSettingsForm(props: {
  defaultValues: RewardSettingsValues
  complianceConfirmed: boolean
}) {
  const { t } = useTranslation()
  const update = useUpdateOption()
  const form = useForm<RewardSettingsValues>({
    resolver: zodResolver(rewardSettingsSchema),
    defaultValues: props.defaultValues,
  })
  const { isDirty, isSubmitting } = form.formState
  const pending = update.isPending || isSubmitting

  async function onSubmit(values: RewardSettingsValues) {
    form.clearErrors('root')
    try {
      const config = rewardFormToConfig(values)
      if (
        !props.complianceConfirmed &&
        (BigInt(config.inviter.nanos) > 0n || BigInt(config.invitee.nanos) > 0n)
      ) {
        form.setError('root', { message: t(complianceMessage) })
        return
      }
      await update.mutateAsync({
        key: 'reward_setting.config',
        value: JSON.stringify(config),
      })
      form.reset(values)
    } catch (error) {
      form.setError('root', { message: getServerErrorMessage(error) })
    }
  }

  return (
    <>
      <FormNavigationGuard when={isDirty} />
      {!props.complianceConfirmed && (
        <Alert variant='destructive'>
          <AlertDescription>{t(complianceMessage)}</AlertDescription>
        </Alert>
      )}
      <Form {...form}>
        <SettingsForm onSubmit={form.handleSubmit(onSubmit)} autoComplete='off'>
          <SettingsPageFormActions
            onSave={form.handleSubmit(onSubmit)}
            isSaving={pending}
            isSaveDisabled={!isDirty}
            saveLabel='Save reward settings'
          />
          <FormDirtyIndicator isDirty={isDirty} />
          <p className='text-muted-foreground text-sm'>
            {t('Each reward has its own currency. Zero disables that reward.')}
          </p>
          {REWARD_FIELDS.map((key) => (
            <fieldset
              key={key}
              disabled={pending}
              className='min-w-0 space-y-4 rounded-lg border p-4'
            >
              <legend className='px-1 text-sm font-medium'>
                {t(rewardLabels[key])}
              </legend>
              <SettingsFormGrid>
                <FormField
                  control={form.control}
                  name={`${key}.amount`}
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('Reward amount')}</FormLabel>
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
                  name={`${key}.currency`}
                  render={({ field }) => (
                    <FormItem>
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
              </SettingsFormGrid>
            </fieldset>
          ))}
          {form.formState.errors.root && (
            <p role='alert' className='text-destructive text-sm'>
              {form.formState.errors.root.message}
            </p>
          )}
        </SettingsForm>
      </Form>
    </>
  )
}
