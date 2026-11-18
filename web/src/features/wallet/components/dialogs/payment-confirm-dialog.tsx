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
import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { toIntlLocale } from '@/i18n/languages'
import { formatMoneyAmount } from '@/lib/currency'

import type { CheckoutQuote } from '../../types'

export function PaymentConfirmDialog(props: {
  quote: CheckoutQuote
  paymentMethod: string
  onOpenChange: (open: boolean) => void
  onConfirm: () => void
  processing: boolean
  creationAttempted: boolean
  error?: string
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const [expired, setExpired] = useState(
    () => props.quote.expires_at * 1000 <= Date.now()
  )
  useEffect(() => {
    const remaining = props.quote.expires_at * 1000 - Date.now()
    if (remaining <= 0) {
      setExpired(true)
      return
    }
    const timer = setTimeout(
      () => setExpired(true),
      Math.min(remaining, 2_147_483_647)
    )
    return () => clearTimeout(timer)
  }, [props.quote.expires_at])

  return (
    <ConfirmDialog
      open
      onOpenChange={props.onOpenChange}
      title={t('Confirm Payment')}
      desc={t('Review your payment details')}
      confirmText={t('Confirm Payment')}
      handleConfirm={props.onConfirm}
      isLoading={props.processing}
      disabled={expired && !props.creationAttempted}
      className='max-sm:w-[calc(100vw-1.5rem)] sm:max-w-md'
    >
      <dl className='grid min-w-0 grid-cols-1 gap-2 text-sm sm:grid-cols-2'>
        <dt className='text-muted-foreground'>{t('Payment Method')}</dt>
        <dd className='min-w-0 wrap-anywhere'>{props.paymentMethod}</dd>
        <dt className='text-muted-foreground'>
          {t(props.quote.charge_final ? 'You Pay' : 'Payment base amount')}
        </dt>
        <dd className='min-w-0 font-medium wrap-anywhere'>
          {formatMoneyAmount(props.quote.charge_amount, locale)}
        </dd>
        <dt className='text-muted-foreground'>{t('Reward amount')}</dt>
        <dd className='min-w-0 wrap-anywhere'>
          {formatMoneyAmount(props.quote.source_amount, locale)}
        </dd>
        <dt className='text-muted-foreground'>{t('Wallet credit')}</dt>
        <dd className='min-w-0 font-medium wrap-anywhere'>
          {formatMoneyAmount(props.quote.credit_amount, locale)}
        </dd>
        {props.quote.fx && (
          <>
            <dt className='text-muted-foreground'>{t('Accounting rate')}</dt>
            <dd className='min-w-0 wrap-anywhere'>
              {props.quote.fx.from_currency} → {props.quote.fx.to_currency}:{' '}
              {props.quote.fx.rate}
            </dd>
          </>
        )}
      </dl>
      {!props.quote.charge_final && (
        <p className='text-muted-foreground text-sm'>
          {t('Final payment amount is confirmed on the payment page.')}
        </p>
      )}
      <p className='text-muted-foreground text-sm'>
        {t('Wallet credits first repay outstanding debt.')}
      </p>
      {expired && !props.creationAttempted && (
        <p role='alert' className='text-destructive text-sm'>
          {t('Quote expired. Cancel and review the payment again.')}
        </p>
      )}
      {props.error && (
        <p role='alert' className='text-destructive text-sm'>
          {props.error}
        </p>
      )}
    </ConfirmDialog>
  )
}
