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

import { ConfirmDialog } from '@/components/confirm-dialog'
import { StaticDataTable } from '@/components/data-table'
import { RequestMoney } from '@/features/usage-logs/requests/components/request-facts'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber } from '@/lib/format'

import type { MarginCheck, MarginEstimate } from '../types'

export function MarginEstimates(props: { estimates: MarginEstimate[] }) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  return (
    <div className='space-y-3'>
      <p className='text-muted-foreground text-sm'>
        {t(
          'Sale estimates use the lowest applicable group discount multiplier.'
        )}
      </p>
      <StaticDataTable
        data={props.estimates}
        getRowKey={(row, index) =>
          `${row.model_offering_id}:${row.channel_id}:${index}`
        }
        getRowClassName={(row) =>
          row.negative ? 'text-destructive' : undefined
        }
        tableProps={{ 'aria-label': t('Estimated channel margins') }}
        columns={[
          {
            id: 'offering',
            header: t('Model Offering ID'),
            cell: (row) => `#${row.model_offering_id}`,
          },
          {
            id: 'channel',
            header: t('Channel ID'),
            cell: (row) => `#${row.channel_id}`,
          },
          {
            id: 'margin',
            header: t('Gross margin'),
            cell: (row) => (
              <RequestMoney
                amount={row.margin ?? undefined}
                pending={!row.margin}
              />
            ),
          },
          {
            id: 'provider',
            header: t('Provider ID'),
            cell: (row) => `#${row.provider_id}`,
          },
          {
            id: 'samples',
            header: t('Usage samples'),
            cell: (row) => formatNumber(row.samples, locale),
          },
          {
            id: 'revenue',
            header: t('Revenue'),
            cell: (row) => (
              <RequestMoney
                amount={row.revenue ?? undefined}
                pending={!row.revenue}
              />
            ),
          },
          {
            id: 'cost',
            header: t('Purchase cost'),
            cell: (row) => (
              <RequestMoney
                amount={row.cost ?? undefined}
                pending={!row.cost}
              />
            ),
          },
          {
            id: 'status',
            header: t('Status'),
            cell: (row) => {
              if (row.negative) return t('Sale price below purchase price')
              if (row.status === 'estimated') return t('Estimated')
              if (row.status === 'missing_sale_price') {
                return t('Missing sale price')
              }
              if (row.status === 'missing_purchase_price') {
                return t('Missing purchase price')
              }
              if (row.status === 'missing_fx') return t('Missing exchange rate')
              if (row.status === 'no_usage_history') {
                return t('No usage in the last seven days')
              }
              return t('Insufficient pricing facts')
            },
          },
        ]}
      />
    </div>
  )
}

export function MarginConfirmation(props: {
  report: MarginCheck
  onCancel: () => void
  onConfirm: () => void
  isPending: boolean
}) {
  const { t } = useTranslation()
  return (
    <ConfirmDialog
      open
      onOpenChange={(open) => {
        if (!open) props.onCancel()
      }}
      title={t('Negative margin requires confirmation')}
      desc={t(
        'These combinations may lose money. Confirm the displayed price or exchange rate to continue.'
      )}
      destructive
      confirmText={t('Confirm loss')}
      handleConfirm={props.onConfirm}
      isLoading={props.isPending}
      className='max-h-[90dvh] overflow-y-auto sm:max-w-4xl'
    >
      <MarginEstimates estimates={props.report.estimates} />
    </ConfirmDialog>
  )
}
