import { zodResolver } from '@hookform/resolvers/zod'
import { useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ErrorState } from '@/components/error-state'
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
import { Switch } from '@/components/ui/switch'
import { toIntlLocale } from '@/i18n/languages'
import {
  balanceReminderDraftSchema,
  balanceReminderFromDraft,
  parseBalanceReminderConfig,
  type BalanceReminderDraft,
} from '@/lib/balance-reminder'
import {
  formatMoneyAmount,
  moneyAmountToDecimal,
  parseDecimalMoneyAmount,
} from '@/lib/currency'
import {
  getServerErrorMessage,
  requireServerSuccess,
} from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

import { updateSystemOption } from '../api'
import {
  SettingsForm,
  SettingsSwitchContent,
  SettingsSwitchItem,
} from '../components/settings-form-layout'

export function BalanceReminderSettings(props: { config: string }) {
  const { t } = useTranslation()
  const userID = useAuthStore((state) => state.auth.user?.id)
  const sessionID = useAuthStore((state) => state.auth.session?.sid)
  const config = parseBalanceReminderConfig(props.config)
  if (!config) {
    return (
      <ErrorState description={t('Invalid balance reminder configuration')} />
    )
  }
  return (
    <BalanceReminderForm
      key={`${userID}:${sessionID}:${props.config}`}
      defaultValues={{
        enabled: config.enabled,
        inherit: false,
        amount: moneyAmountToDecimal(config.threshold),
        currency: config.threshold.currency,
      }}
    />
  )
}

function BalanceReminderForm(props: { defaultValues: BalanceReminderDraft }) {
  const { t, i18n } = useTranslation()
  const queryClient = useQueryClient()
  const userID = useAuthStore((state) => state.auth.user?.id)
  const sessionID = useAuthStore((state) => state.auth.session?.sid)
  const active = useRef(true)
  const form = useForm<BalanceReminderDraft>({
    resolver: zodResolver(balanceReminderDraftSchema),
    defaultValues: props.defaultValues,
  })
  const { isDirty, isSubmitting } = form.formState
  useEffect(() => {
    active.current = true
    return () => {
      active.current = false
    }
  }, [])
  async function onSubmit(values: BalanceReminderDraft) {
    const isCurrent = () => {
      const auth = useAuthStore.getState().auth
      return (
        active.current &&
        auth.user?.id === userID &&
        auth.session?.sid === sessionID
      )
    }
    if (isSubmitting || !isCurrent()) return
    form.clearErrors('root')
    try {
      requireServerSuccess(
        await updateSystemOption({
          key: 'balance_reminder_setting.config',
          value: JSON.stringify(
            balanceReminderFromDraft({ ...values, inherit: false })
          ),
        })
      )
      if (!isCurrent()) return
      form.reset(values)
      void queryClient.invalidateQueries({ queryKey: ['system-options'] })
      toast.success(t('Setting updated successfully'))
    } catch (error) {
      if (isCurrent()) {
        form.setError('root', { message: getServerErrorMessage(error) })
      }
    }
  }
  const amount = parseDecimalMoneyAmount(
    form.formState.defaultValues?.amount ?? props.defaultValues.amount,
    props.defaultValues.currency
  )
  return (
    <section
      aria-label={t('Balance reminder settings')}
      className='min-w-0 space-y-4 border-t pt-6'
    >
      <h3 className='font-medium'>{t('Balance reminder settings')}</h3>
      <Form {...form}>
        <SettingsForm onSubmit={form.handleSubmit(onSubmit)} autoComplete='off'>
          <fieldset disabled={isSubmitting} className='min-w-0 space-y-4'>
            <FormField
              control={form.control}
              name='enabled'
              render={({ field }) => (
                <SettingsSwitchItem>
                  <SettingsSwitchContent>
                    <FormLabel>{t('Low balance reminder')}</FormLabel>
                  </SettingsSwitchContent>
                  <FormControl>
                    <Switch
                      checked={field.value}
                      onCheckedChange={field.onChange}
                      disabled={isSubmitting}
                    />
                  </FormControl>
                </SettingsSwitchItem>
              )}
            />
            <FormField
              control={form.control}
              name='amount'
              render={({ field }) => (
                <FormItem className='min-w-0 grid-cols-1'>
                  <FormLabel>
                    {t('Default reminder threshold')} (
                    {props.defaultValues.currency})
                  </FormLabel>
                  <FormControl>
                    <Input
                      {...field}
                      type='text'
                      inputMode='decimal'
                      disabled={isSubmitting}
                      className='min-w-0'
                    />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            <p className='text-muted-foreground text-xs wrap-anywhere'>
              {t('Amounts use the settlement currency without conversion.')}
            </p>
            {amount && (
              <p className='text-muted-foreground text-xs wrap-anywhere'>
                {t('Saved reminder threshold')}:{' '}
                {formatMoneyAmount(
                  amount,
                  toIntlLocale(i18n.resolvedLanguage || i18n.language)
                )}
              </p>
            )}
          </fieldset>
          {form.formState.errors.root && (
            <ErrorState description={form.formState.errors.root.message} />
          )}
          <Button
            type='submit'
            disabled={isSubmitting || !isDirty}
            className='h-auto min-h-8 min-w-0 whitespace-normal'
          >
            {t('Save balance reminder settings')}
          </Button>
        </SettingsForm>
      </Form>
    </section>
  )
}
