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
import { useId } from 'react'
import { useTranslation } from 'react-i18next'

import { CopyButton } from '@/components/copy-button'
import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import { toIntlLocale } from '@/i18n/languages'
import { formatMoneyAmount } from '@/lib/currency'

import {
  formatTimestamp,
  getPaymentMethodName,
  getStatusConfig,
} from '../lib/billing'
import type { TopupRecord } from '../types'

export function BillingHistoryRecord(props: {
  record: TopupRecord
  isAdmin: boolean
  completing: boolean
  onComplete: (record: TopupRecord) => void
}) {
  const { t, i18n } = useTranslation()
  const titleId = useId()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const record = props.record
  const checkout = record.contract_version === 1 ? record.checkout : undefined
  const status = checkout?.fulfillment_pending
    ? { label: 'Wallet credit pending', variant: 'warning' as const }
    : getStatusConfig(record.status)
  const details = [
    {
      label: 'Payment Method',
      value: getPaymentMethodName(record.payment_method, t),
    },
  ]
  if (checkout) {
    details.push(
      {
        label: checkout.charge_final ? 'You Pay' : 'Payment base amount',
        value: formatMoneyAmount(checkout.charge_amount, locale),
      },
      {
        label: 'Actual payment',
        value:
          checkout.paid_amount === null
            ? t('Payment not confirmed')
            : formatMoneyAmount(checkout.paid_amount, locale),
      },
      {
        label: 'Reward amount',
        value: formatMoneyAmount(checkout.source_amount, locale),
      },
      {
        label: 'Wallet credit',
        value: formatMoneyAmount(checkout.credit_amount, locale),
      }
    )
    if (checkout.fx) {
      details.push({
        label: 'Accounting rate',
        value: `${checkout.fx.from_currency} → ${checkout.fx.to_currency}: ${checkout.fx.rate}`,
      })
    }
  } else {
    details.push(
      {
        label: 'Historical amount',
        value: record.legacy_amount ?? t('Not recorded'),
      },
      {
        label: 'Historical payment value',
        value: record.legacy_money ?? t('Not recorded'),
      }
    )
  }
  const canComplete =
    props.isAdmin &&
    record.status === 'pending' &&
    (record.contract_version === 0 ||
      (checkout !== undefined &&
        checkout.accepted_at > 0 &&
        checkout.paid_amount !== null))

  return (
    <article
      aria-labelledby={titleId}
      className='min-w-0 rounded-lg border p-3 sm:p-4'
    >
      <div className='flex flex-wrap items-start justify-between gap-2'>
        <div className='min-w-0 flex-1 space-y-1'>
          <div className='flex min-w-0 items-start gap-2'>
            <h3
              id={titleId}
              className='min-w-0 font-mono text-sm wrap-anywhere'
            >
              {record.trade_no}
            </h3>
            <CopyButton
              value={record.trade_no}
              size='sm'
              className='h-5 w-5 p-0'
              iconClassName='size-3'
            />
          </div>
          <p className='text-muted-foreground text-xs'>
            {formatTimestamp(record.create_time)}
          </p>
          {props.isAdmin && record.user_id != null && (
            <StatusBadge
              label={`${t('User ID')}: ${record.user_id}`}
              variant='neutral'
              size='sm'
              copyText={String(record.user_id)}
            />
          )}
        </div>
        <StatusBadge
          label={t(status.label)}
          variant={status.variant}
          showDot
          copyable={false}
        />
      </div>
      {!checkout && (
        <p className='mt-3 text-sm font-medium'>
          {t(
            record.purpose === 'subscription_history'
              ? 'Subscription history'
              : 'Historical topup'
          )}
        </p>
      )}
      <dl className='mt-3 grid min-w-0 grid-cols-1 gap-3 sm:mt-4 sm:grid-cols-2 sm:gap-4'>
        {details.map((detail) => (
          <div key={detail.label} className='min-w-0 space-y-1'>
            <dt className='text-muted-foreground text-xs'>{t(detail.label)}</dt>
            <dd className='text-sm font-medium wrap-anywhere'>
              {detail.value}
            </dd>
          </div>
        ))}
      </dl>
      {!checkout && (
        <p className='text-muted-foreground mt-3 text-xs'>
          {t('Historical values have no recorded currency.')}
        </p>
      )}
      {checkout && (
        <p className='text-muted-foreground mt-3 text-xs'>
          {t('Wallet credits first repay outstanding debt.')}
        </p>
      )}
      {checkout?.creation_state === 'uncertain' && (
        <p className='text-muted-foreground mt-3 text-xs'>
          {t('Payment session creation is being confirmed.')}
        </p>
      )}
      {canComplete && (
        <div className='mt-4 flex justify-end'>
          <Button
            size='sm'
            variant='outline'
            onClick={() => props.onComplete(record)}
            disabled={props.completing}
          >
            {t('Complete Order')}
          </Button>
        </div>
      )}
    </article>
  )
}
