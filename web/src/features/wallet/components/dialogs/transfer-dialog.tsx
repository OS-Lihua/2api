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
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import { Dialog } from '@/components/dialog'
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
import { toIntlLocale } from '@/i18n/languages'
import {
  formatMoneyAmount,
  moneyAmountToDecimal,
  parseDecimalMoneyAmount,
  type MoneyAmount,
} from '@/lib/currency'
import { getServerErrorMessage } from '@/lib/server-error-message'

import { referralTransferSchema } from '../../lib/referral'
import type { AffiliateTransferRequest } from '../../types'

interface TransferDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  onConfirm: (amount: MoneyAmount) => Promise<boolean>
  available: MoneyAmount
  attempt: AffiliateTransferRequest | null
  transferring: boolean
}

export function TransferDialog(props: TransferDialogProps) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const form = useForm<{ amount: string }>({
    resolver: zodResolver(
      referralTransferSchema(props.attempt?.amount ?? props.available)
    ),
    defaultValues: {
      amount: moneyAmountToDecimal(props.attempt?.amount ?? props.available),
    },
  })
  const pending = props.transferring || form.formState.isSubmitting
  async function onSubmit(values: { amount: string }) {
    form.clearErrors('root')
    const amount =
      props.attempt?.amount ??
      parseDecimalMoneyAmount(values.amount, props.available.currency)
    if (!amount) return
    try {
      if (await props.onConfirm(amount)) {
        props.onOpenChange(false)
        form.reset({ amount: '' })
      }
    } catch (error) {
      form.setError('root', { message: getServerErrorMessage(error) })
    }
  }
  return (
    <Dialog
      open={props.open}
      onOpenChange={(open) => {
        if (!pending) props.onOpenChange(open)
      }}
      title={t('Transfer Rewards')}
      description={t('Wallet credits first repay outstanding debt.')}
      contentClassName='max-sm:w-[calc(100vw-1.5rem)] sm:max-w-md'
      showCloseButton={!pending}
      footer={
        <>
          <Button
            variant='outline'
            onClick={() => props.onOpenChange(false)}
            disabled={pending}
          >
            {t('Cancel')}
          </Button>
          <Button onClick={form.handleSubmit(onSubmit)} disabled={pending}>
            {t('Transfer')}
          </Button>
        </>
      }
    >
      <div className='mb-4 min-w-0'>
        <p className='text-muted-foreground text-xs'>
          {t('Available Rewards')}
        </p>
        <p className='text-xl font-semibold wrap-anywhere tabular-nums'>
          {formatMoneyAmount(props.available, locale)}
        </p>
      </div>
      <Form {...form}>
        <form onSubmit={form.handleSubmit(onSubmit)} className='space-y-4'>
          <FormField
            control={form.control}
            name='amount'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Transfer Amount')}</FormLabel>
                <FormControl>
                  <Input
                    {...field}
                    type='text'
                    inputMode='decimal'
                    disabled={pending || props.attempt !== null}
                  />
                </FormControl>
                <FormMessage role='alert' />
              </FormItem>
            )}
          />
          {props.attempt && (
            <p className='text-muted-foreground text-xs'>
              {t('Retry this transfer to confirm its result.')}
            </p>
          )}
          {form.formState.errors.root && (
            <p role='alert' className='text-destructive text-sm'>
              {form.formState.errors.root.message}
            </p>
          )}
        </form>
      </Form>
    </Dialog>
  )
}
