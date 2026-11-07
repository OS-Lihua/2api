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

import { toIntlLocale } from '@/i18n/languages'
import { formatNumber } from '@/lib/format'

import type { RequestAttempt, RouteDiagnostics } from '../types'
import { RequestMoney } from './request-facts'

export function AttemptAdminFacts(props: { attempt: RequestAttempt }) {
  const { t } = useTranslation()
  const attempt = props.attempt
  const fields = [
    [t('Provider ID'), attempt.provider_id],
    [t('Channel ID'), attempt.channel_id],
    [t('Model ID'), attempt.model_id],
    [t('Member tier'), attempt.member_tier],
    [t('Purchase price version'), attempt.purchase_price_version_id],
    [t('Accounting exchange rate'), attempt.accounting_fx_rate],
    [t('Failover decision'), attempt.failover_decision],
    [t('Failover reason'), attempt.failover_reason],
    [t('Circuit action'), attempt.circuit_action],
  ]
  return (
    <section className='space-y-3 rounded-lg border p-3'>
      <h4 className='text-sm font-medium'>{t('Route and purchase cost')}</h4>
      <dl className='grid gap-3 text-xs sm:grid-cols-2'>
        {fields.map(([label, value]) => (
          <div key={label}>
            <dt className='text-muted-foreground'>{label}</dt>
            <dd className='mt-1 font-mono break-words'>{value ?? '-'}</dd>
          </div>
        ))}
        <div>
          <dt className='text-muted-foreground'>{t('Purchase cost')}</dt>
          <dd className='mt-1'>
            <RequestMoney amount={attempt.cost} />
          </dd>
        </div>
        <div>
          <dt className='text-muted-foreground'>{t('Converted cost')}</dt>
          <dd className='mt-1'>
            <RequestMoney amount={attempt.converted_cost} />
          </dd>
        </div>
      </dl>
      {attempt.route_diagnostics?.method === 'single_channel' && (
        <RouteDiagnosticsFacts diagnostics={attempt.route_diagnostics} />
      )}
    </section>
  )
}

function RouteDiagnosticsFacts(props: { diagnostics: RouteDiagnostics }) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const reasons: Record<string, string> = {
    disabled: t('Disabled'),
    scope: t('Outside scope'),
    capability: t('Capability mismatch'),
    protocol: t('Protocol mismatch'),
    price_unavailable: t('Purchase price unavailable'),
  }
  const diagnostics = props.diagnostics
  const counts = Object.entries(diagnostics.excluded_counts ?? {}).filter(
    ([reason, count]) =>
      Object.hasOwn(reasons, reason) &&
      typeof count === 'number' &&
      Number.isFinite(count) &&
      count > 0
  )
  const excluded = Array.isArray(diagnostics.excluded)
    ? diagnostics.excluded.slice(0, 10)
    : []
  return (
    <div className='space-y-2 border-t pt-3 text-xs'>
      <h5 className='font-medium'>{t('Route selection')}</h5>
      <p>{t('Single Channel')}</p>
      <dl className='grid grid-cols-2 gap-3'>
        <div>
          <dt className='text-muted-foreground'>{t('Candidate Channels')}</dt>
          <dd>{formatNumber(diagnostics.candidate_count, locale)}</dd>
        </div>
        <div>
          <dt className='text-muted-foreground'>{t('Eligible Channels')}</dt>
          <dd>{formatNumber(diagnostics.eligible_count, locale)}</dd>
        </div>
      </dl>
      {counts.map(([reason, count]) => (
        <p key={reason}>
          {reasons[reason]}: {formatNumber(count, locale)}
        </p>
      ))}
      {!!excluded.length && (
        <ul className='space-y-1'>
          {excluded.map((entry) => (
            <li
              key={`${entry.channel_id}-${entry.reason}`}
              className='break-words'
            >
              {t('Channel ID')}:{' '}
              {typeof entry.channel_id === 'number' ? entry.channel_id : '-'} ·{' '}
              {Object.hasOwn(reasons, entry.reason)
                ? reasons[entry.reason]
                : t('Other exclusion')}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
