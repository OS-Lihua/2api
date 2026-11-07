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
import { useQuery } from '@tanstack/react-query'
import { useTranslation } from 'react-i18next'

import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { getServerErrorMessage } from '@/lib/server-error-message'

import { getFxQuote, pricingKeys } from '../api'
import type { FxRate } from '../types'

export function FxQuoteDialog(props: { rate: FxRate; onClose: () => void }) {
  const { t } = useTranslation()
  const query = useQuery({
    queryKey: [
      ...pricingKeys.fx,
      'quote',
      props.rate.from_currency,
      props.rate.to_currency,
    ],
    queryFn: () => getFxQuote(props.rate.from_currency, props.rate.to_currency),
    refetchOnMount: 'always',
  })
  return (
    <Dialog
      open
      title={t('Exchange rate quote')}
      onOpenChange={(open) => !open && props.onClose()}
      description={`${props.rate.from_currency} → ${props.rate.to_currency}`}
    >
      {query.isError && (
        <ErrorState
          description={getServerErrorMessage(query.error)}
          onRetry={() => void query.refetch()}
        />
      )}
      {query.isPending && <LoadingState />}
      {query.data && !query.isError && (
        <dl className='grid grid-cols-1 gap-3 sm:grid-cols-2'>
          <dt>{t('Accounting rate')}</dt>
          <dd className='font-mono break-all'>{query.data.accounting_rate}</dd>
          <dt>{t('Comparison rate')}</dt>
          <dd className='font-mono break-all'>{query.data.comparison_rate}</dd>
          <dt>{t('Comparison buffer rate')}</dt>
          <dd className='font-mono break-all'>{query.data.buffer_rate}</dd>
        </dl>
      )}
    </Dialog>
  )
}
