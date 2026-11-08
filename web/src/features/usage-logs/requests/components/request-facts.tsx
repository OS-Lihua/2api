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
import { useTranslation } from 'react-i18next'

import { CopyButton } from '@/components/copy-button'
import { StatusBadge } from '@/components/status-badge'
import { Badge } from '@/components/ui/badge'
import { toIntlLocale } from '@/i18n/languages'
import { formatMoneyAmount, type MoneyAmount } from '@/lib/currency'
import { formatNumber } from '@/lib/format'

import type { RequestUsage, UsageSource } from '../types'

export function RequestIdentifier(props: { value: string; label: string }) {
  return (
    <div className='flex min-w-0 items-center gap-1'>
      <span title={props.value} className='min-w-0 truncate font-mono text-xs'>
        {props.value}
      </span>
      <CopyButton
        value={props.value}
        aria-label={props.label}
        className='size-7'
        iconClassName='size-3.5'
      />
    </div>
  )
}

export function RequestMoney(props: {
  amount?: MoneyAmount
  pending?: boolean
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  return (
    <span className='font-mono text-xs break-all whitespace-normal tabular-nums'>
      {props.pending ? t('Pending') : formatMoneyAmount(props.amount, locale)}
    </span>
  )
}

export function RequestTime(props: { timestamp?: number }) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  if (!props.timestamp || !Number.isFinite(props.timestamp)) {
    return <span>-</span>
  }
  const date = new Date(props.timestamp)
  if (Number.isNaN(date.getTime())) return <span>-</span>
  const iso = date.toISOString()
  return (
    <div className='flex min-w-0 items-center gap-1'>
      <time dateTime={iso} title={iso} className='min-w-0 text-xs tabular-nums'>
        {new Intl.DateTimeFormat(locale, {
          dateStyle: 'medium',
          timeStyle: 'medium',
        }).format(date)}
      </time>
      <CopyButton
        value={iso}
        aria-label={t('Copy timestamp')}
        className='size-7'
        iconClassName='size-3.5'
      />
    </div>
  )
}

export function RequestOutcome(props: { status: string }) {
  const { t } = useTranslation()
  const statuses = {
    success: t('Success'),
    failed: t('Failed'),
    cancelled: t('Cancelled'),
    partial: t('Partial'),
    settlement_pending: t('Settlement pending'),
    pending: t('Pending'),
    submission_unknown: t('Task submission outcome unknown'),
  }
  const label = statuses[props.status as keyof typeof statuses] ?? props.status
  let variant: 'success' | 'danger' | 'warning' | 'neutral' = 'neutral'
  if (props.status === 'success') variant = 'success'
  else if (props.status === 'failed') variant = 'danger'
  else if (
    props.status === 'partial' ||
    props.status === 'settlement_pending' ||
    props.status === 'submission_unknown'
  ) {
    variant = 'warning'
  }
  return (
    <StatusBadge label={label} variant={variant} copyable={false} size='sm' />
  )
}

export function RequestUsageFacts(props: {
  usage?: RequestUsage
  source?: UsageSource
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  if (!props.usage || props.source === 'none') {
    return (
      <span className='text-muted-foreground text-xs'>
        {t('No usage available')}
      </span>
    )
  }
  const taskFacts = Object.entries(props.usage?.task_facts ?? {})
  return (
    <div className='space-y-1 text-xs'>
      {taskFacts.length > 0 ? (
        <dl role='group' aria-label={t('Task usage')} className='space-y-1'>
          {taskFacts.map(([key, value]) => {
            let display: string
            if (typeof value === 'number') {
              display = formatNumber(value, locale)
            } else if (typeof value === 'boolean') {
              display = value ? t('Yes') : t('No')
            } else {
              display = value
            }
            return (
              <div key={key} className='flex flex-wrap gap-1'>
                <dt className='text-muted-foreground'>{key}</dt>
                <dd className='break-all tabular-nums' title={String(value)}>
                  {display}
                </dd>
              </div>
            )
          })}
        </dl>
      ) : (
        <div>
          <div className='tabular-nums'>
            {t('Input')}: {formatNumber(props.usage.prompt_tokens, locale)} ·{' '}
            {t('Output')}: {formatNumber(props.usage.completion_tokens, locale)}
          </div>
          {props.usage.total_tokens !== undefined && (
            <div className='text-muted-foreground'>
              {t('Total tokens')}:{' '}
              {formatNumber(props.usage.total_tokens, locale)}
            </div>
          )}
        </div>
      )}
      {props.source === 'estimated' && (
        <Badge variant='outline'>{t('Estimated')}</Badge>
      )}
      {props.source === 'upstream' && (
        <span className='text-muted-foreground'>{t('Upstream usage')}</span>
      )}
    </div>
  )
}
