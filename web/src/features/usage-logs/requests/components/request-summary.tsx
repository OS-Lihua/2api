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

import type { RequestLog, RequestScope } from '../types'
import {
  RequestIdentifier,
  RequestMoney,
  RequestOutcome,
  RequestTime,
} from './request-facts'

export function RequestSummary(props: {
  request: RequestLog
  scope: RequestScope
}) {
  const { t } = useTranslation()
  const request = props.request
  const fields = [
    [t('Model Offering'), request.offering_name],
    [
      t('Alias'),
      request.requested_name !== request.offering_name
        ? request.requested_name
        : undefined,
    ],
    [t('Model Offering ID'), request.model_offering_id],
    [t('Token ID'), request.token_id],
    [t('Sale price version'), request.sale_price_version_id],
    [t('Sale currency'), request.sale_currency],
    [t('Sale exchange rate'), request.sale_fx_rate],
    [t('Group ratio'), request.group_ratio],
  ]
  if (props.scope === 'all') fields.push([t('User ID'), request.user_id])
  return (
    <section className='space-y-3 border-b pb-5'>
      <div className='flex flex-wrap items-center justify-between gap-2'>
        <h3 className='text-sm font-medium'>{t('Request summary')}</h3>
        <RequestOutcome status={request.status} />
      </div>
      <dl className='grid gap-3 text-xs sm:grid-cols-2'>
        <div>
          <dt className='text-muted-foreground'>{t('Charge')}</dt>
          <dd className='mt-1'>
            <RequestMoney
              amount={request.receivable}
              pending={
                request.status === 'pending' ||
                request.status === 'submission_unknown'
              }
            />
          </dd>
        </div>
        {fields.map(([label, value]) => (
          <div key={label}>
            <dt className='text-muted-foreground'>{label}</dt>
            <dd className='mt-1 break-words'>{value ?? '-'}</dd>
          </div>
        ))}
        <div>
          <dt className='text-muted-foreground'>{t('Started at')}</dt>
          <dd>
            <RequestTime timestamp={request.started_at} />
          </dd>
        </div>
        <div>
          <dt className='text-muted-foreground'>{t('Ended at')}</dt>
          <dd>
            <RequestTime timestamp={request.ended_at} />
          </dd>
        </div>
      </dl>
      {request.error_code && (
        <p className='text-destructive text-xs break-words'>
          {request.error_code}
        </p>
      )}
      {!!request.ledger_entry_ids?.length && (
        <div className='space-y-1'>
          <h4 className='text-muted-foreground text-xs'>
            {t('Ledger entries')}
          </h4>
          {request.ledger_entry_ids.map((id) => (
            <RequestIdentifier
              key={id}
              value={id}
              label={t('Copy ledger entry ID')}
            />
          ))}
        </div>
      )}
    </section>
  )
}
