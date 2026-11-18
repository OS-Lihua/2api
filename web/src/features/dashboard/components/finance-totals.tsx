import { Activity, CreditCard, Receipt, Wallet } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { StaticDataTable } from '@/components/data-table/static/static-data-table'
import { FundingStatus } from '@/components/funding-status'
import { toIntlLocale } from '@/i18n/languages'
import { formatMoneyAmount } from '@/lib/currency'

import {
  exactQuantity,
  type FinanceMetric,
  type FinanceTotals as Totals,
} from '../lib/finance'
import { StatCard } from './ui/stat-card'

const usageLabels: Record<string, string[]> = {
  prompt_tokens: ['Input Tokens'],
  completion_tokens: ['Output Tokens'],
  total_tokens: ['Total Tokens'],
  cached_tokens: ['Cache Read', 'Tokens'],
  cache_creation_tokens: ['Cache Creation', 'Tokens'],
  cache_creation_12h_tokens: ['Cache Creation (12h)', 'Tokens'],
  image_input_tokens: ['Image', 'Input', 'Tokens'],
  image_output_tokens: ['Image', 'Output', 'Tokens'],
  audio_input_tokens: ['Audio Input', 'Tokens'],
  audio_output_tokens: ['Audio Output', 'Tokens'],
  image_count: ['Image count'],
  audio_input_seconds: ['Audio Input', 'seconds'],
  audio_output_seconds: ['Audio Output', 'seconds'],
}

export function FinanceAmount({ metric }: { metric: FinanceMetric }) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  return (
    <div className='min-w-0 space-y-1 [overflow-wrap:anywhere] whitespace-normal'>
      <span>{formatMoneyAmount(metric.known_amount, locale)}</span>
      {metric.total_amount === null && (
        <div className='text-muted-foreground text-xs'>
          {t('Known amount only')} · {t('Pending requests')}:{' '}
          {exactQuantity(metric.pending_count, locale)} ·{' '}
          {t('Total unavailable')}
        </div>
      )}
    </div>
  )
}
export function FinanceTotals({
  totals,
  admin,
}: {
  totals: Totals
  admin: boolean
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const money = [
    { key: 'Revenue', metric: totals.revenue, icon: Receipt },
    { key: 'Collected', metric: totals.collected, icon: Wallet },
    { key: 'Outstanding Debt', metric: totals.outstanding, icon: CreditCard },
  ]
  if (admin && totals.procurement && totals.margin) {
    money.push(
      {
        key: 'Procurement cost',
        metric: totals.procurement.cost,
        icon: Receipt,
      },
      { key: 'Revenue minus cost', metric: totals.margin, icon: Receipt }
    )
  }
  return (
    <div className='space-y-4'>
      <FundingStatus
        pending={
          totals.funding_pending_count === undefined
            ? undefined
            : BigInt(totals.funding_pending_count) > 0n
        }
      />
      {totals.funding_pending_count !== undefined && (
        <p className='text-sm'>
          {t('Pending funding requests')}:{' '}
          {exactQuantity(totals.funding_pending_count, locale)}
        </p>
      )}
      <div className='grid min-w-0 grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-3'>
        {money.map(({ key, metric, icon }) => (
          <div key={key} className='min-w-0 rounded-lg border'>
            <StatCard
              title={t(key)}
              value={formatMoneyAmount(metric.known_amount, locale)}
              description={
                metric.total_amount === null
                  ? t('Known amount only')
                  : t('Total')
              }
              icon={icon}
            />
            {metric.total_amount === null && (
              <p className='px-4 pb-3 text-sm [overflow-wrap:anywhere]'>
                {t('Total unavailable')} · {t('Pending requests')}:{' '}
                {exactQuantity(metric.pending_count, locale)}
              </p>
            )}
          </div>
        ))}
        <StatCard
          title={t('Requests')}
          value={exactQuantity(totals.request_count, locale)}
          description={t('Request start cohort')}
          icon={Activity}
        />
      </div>
      <p className='text-muted-foreground text-sm'>
        {t(
          'Revenue includes confirmed debt. Collected payments and outstanding debt are separate; repayments do not recognize revenue again.'
        )}
      </p>
      {admin && (
        <p className='text-muted-foreground text-sm'>
          {t(
            'Revenue is attributed to the last attempt; procurement includes all attempts. A known difference is not a minimum profit.'
          )}
        </p>
      )}
      <div className='grid gap-2 text-sm sm:grid-cols-2 lg:grid-cols-4'>
        {(
          [
            ['In flight', totals.in_flight_count],
            ['Succeeded', totals.succeeded_count],
            ['Failed', totals.failed_count],
            ['Other terminal requests', totals.other_terminal_count],
          ] as const
        ).map(([label, value]) => (
          <div key={label} className='[overflow-wrap:anywhere]'>
            {t(label)}: {exactQuantity(value, locale)}
          </div>
        ))}
      </div>
      {admin && totals.procurement && (
        <div className='space-y-2 rounded-lg border p-3 text-sm'>
          <p>
            {t('Purchase price pending')}:{' '}
            {exactQuantity(totals.procurement.purchase_pending_count, locale)} ·{' '}
            {t('FX pending')}:{' '}
            {exactQuantity(totals.procurement.fx_pending_count, locale)}
          </p>
          <p>{t('Original procurement currencies')}</p>
          {totals.procurement.original_costs.map((metric) => (
            <div key={metric.known_amount.currency}>
              <FinanceAmount metric={metric} />
            </div>
          ))}
        </div>
      )}
      <StaticDataTable
        tableProps={{ 'aria-label': t('Usage facts') }}
        data={Object.entries(totals.usage)}
        getRowKey={([field]) => field}
        columns={[
          {
            id: 'field',
            header: t('Usage facts'),
            cell: ([field]) => (
              <span className='[overflow-wrap:anywhere] whitespace-normal'>
                {usageLabels[field]?.map((label) => t(label)).join(' · ') ??
                  field}
              </span>
            ),
          },
          {
            id: 'known',
            header: t('Known usage'),
            cell: ([, value]) => (
              <span className='[overflow-wrap:anywhere] whitespace-normal'>
                {exactQuantity(value.known_total, locale)}
              </span>
            ),
          },
          {
            id: 'total',
            header: t('Total'),
            cell: ([, value]) => (
              <span className='[overflow-wrap:anywhere] whitespace-normal'>
                {value.total === null
                  ? t('Total unavailable')
                  : exactQuantity(value.total, locale)}
              </span>
            ),
          },
          {
            id: 'missing',
            header: t('Missing usage requests'),
            cell: ([, value]) => exactQuantity(value.missing_count, locale),
          },
          {
            id: 'estimated',
            header: t('Estimated usage requests'),
            cell: ([, value]) => exactQuantity(value.estimated_count, locale),
          },
        ]}
      />
    </div>
  )
}
