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
      {props.pending
        ? t('Settlement pending')
        : formatMoneyAmount(props.amount, locale)}
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
  }
  const label = statuses[props.status as keyof typeof statuses] ?? props.status
  let variant: 'success' | 'danger' | 'warning' | 'neutral' = 'neutral'
  if (props.status === 'success') variant = 'success'
  else if (props.status === 'failed') variant = 'danger'
  else if (
    props.status === 'partial' ||
    props.status === 'settlement_pending'
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
  const unmeasured = props.usage?.unmeasured_tokens ?? []
  const hasMediaQuantities =
    unmeasured.length > 0 ||
    props.usage?.image_input_tokens !== undefined ||
    props.usage?.image_output_tokens !== undefined ||
    props.usage?.audio_input_tokens !== undefined ||
    props.usage?.audio_output_tokens !== undefined ||
    props.usage?.image_count !== undefined ||
    props.usage?.audio_input_seconds !== undefined ||
    props.usage?.audio_output_seconds !== undefined
  if (!props.usage || (props.source === 'none' && !hasMediaQuantities)) {
    return (
      <span className='text-muted-foreground text-xs'>
        {t('No usage available')}
      </span>
    )
  }
  const durationFormatter = new Intl.NumberFormat(locale, {
    maximumSignificantDigits: 9,
  })
  return (
    <div className='space-y-1 text-xs'>
      {props.source !== 'none' && (
        <div className='tabular-nums'>
          {t('Input')}:{' '}
          {unmeasured.includes('p')
            ? t('Unknown')
            : formatNumber(props.usage.prompt_tokens, locale)}{' '}
          · {t('Output')}:{' '}
          {unmeasured.includes('c')
            ? t('Unknown')
            : formatNumber(props.usage.completion_tokens, locale)}
        </div>
      )}
      {(
        [
          ['img', t('Image input tokens'), props.usage.image_input_tokens],
          ['img_o', t('Image output tokens'), props.usage.image_output_tokens],
          ['ai', t('Audio input tokens'), props.usage.audio_input_tokens],
          ['ao', t('Audio output tokens'), props.usage.audio_output_tokens],
        ] as const
      )
        .filter(
          ([quantity, , value]) =>
            value !== undefined || unmeasured.includes(quantity)
        )
        .map(([quantity, label, value]) => (
          <div key={quantity} className='tabular-nums'>
            {label}:{' '}
            {unmeasured.includes(quantity)
              ? t('Unknown')
              : formatNumber(value, locale)}
          </div>
        ))}
      {props.usage.image_count !== undefined && (
        <div className='tabular-nums'>
          {t('Image count')}: {formatNumber(props.usage.image_count, locale)}
        </div>
      )}
      {props.usage.audio_input_seconds !== undefined && (
        <div className='tabular-nums'>
          {t('Input audio duration')}:{' '}
          <span title={props.usage.audio_input_seconds}>
            {durationFormatter.format(Number(props.usage.audio_input_seconds))}
          </span>{' '}
          {t('seconds')}
        </div>
      )}
      {props.usage.audio_output_seconds !== undefined && (
        <div className='tabular-nums'>
          {t('Output audio duration')}:{' '}
          <span title={props.usage.audio_output_seconds}>
            {durationFormatter.format(Number(props.usage.audio_output_seconds))}
          </span>{' '}
          {t('seconds')}
        </div>
      )}
      {props.source !== 'none' && props.usage.total_tokens !== undefined && (
        <div className='text-muted-foreground'>
          {t('Total tokens')}:{' '}
          {unmeasured.length > 0
            ? t('Unknown')
            : formatNumber(props.usage.total_tokens, locale)}
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
