import { useTranslation } from 'react-i18next'

import { Progress } from '@/components/ui/progress'
import { toIntlLocale } from '@/i18n/languages'
import { formatMoneyAmount } from '@/lib/currency'

import type { SubscriptionPeriod } from '../types'

export function SubscriptionPeriodDetails({
  period,
  legacyReconciliationPending,
}: {
  period: SubscriptionPeriod
  legacyReconciliationPending?: boolean
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const allowance = BigInt(period.allowance.nanos)
  const consumed = BigInt(period.consumed.nanos)
  const ratio = allowance > 0n ? (consumed * 10000n) / allowance : 0n
  // Only bounded progress geometry is numeric; the authoritative values remain exact.
  let boundedRatio = ratio
  if (boundedRatio < 0n) boundedRatio = 0n
  if (boundedRatio > 10000n) boundedRatio = 10000n
  const progress = Number(boundedRatio) / 100
  const rows = [
    {
      label: t('Entitlement limit'),
      value: period.unlimited
        ? t('Unlimited')
        : formatMoneyAmount(period.allowance, locale),
    },
    {
      label: t('Consumed entitlement'),
      value: formatMoneyAmount(period.consumed, locale),
    },
    {
      label: t('Reserved entitlement'),
      value: formatMoneyAmount(period.reserved, locale),
    },
    {
      label: t('Available entitlement'),
      value: period.unlimited
        ? t('Unlimited')
        : formatMoneyAmount(period.available, locale),
    },
  ]
  return (
    <div className='min-w-0 space-y-2'>
      {legacyReconciliationPending === true && (
        <p className='text-xs wrap-anywhere text-amber-700 dark:text-amber-300'>
          {t(
            'Historical usage may include unreconciled precharges. Recorded amounts are unchanged; no refund or transfer into this period is implied.'
          )}
        </p>
      )}
      <dl className='grid min-w-0 grid-cols-1 gap-1 text-xs'>
        {rows.map((row) => (
          <div
            key={row.label}
            className='flex min-w-0 flex-wrap justify-between gap-x-3 gap-y-1'
          >
            <dt className='text-muted-foreground'>{row.label}</dt>
            <dd className='min-w-0 wrap-anywhere tabular-nums'>{row.value}</dd>
          </div>
        ))}
      </dl>
      {!period.unlimited && allowance > 0n && (
        <Progress
          value={progress}
          aria-label={t('Consumed entitlement')}
          aria-valuetext={`${formatMoneyAmount(period.consumed, locale)} / ${formatMoneyAmount(period.allowance, locale)}`}
          className='h-1.5'
        />
      )}
    </div>
  )
}
