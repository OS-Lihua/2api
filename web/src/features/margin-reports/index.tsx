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
import type { ColumnDef } from '@tanstack/react-table'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { DataTablePage, useDataTable } from '@/components/data-table'
import { ErrorState } from '@/components/error-state'
import { SectionPageLayout } from '@/components/layout'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { RequestMoney } from '@/features/usage-logs/requests/components/request-facts'
import { toIntlLocale } from '@/i18n/languages'
import { api } from '@/lib/api'
import type { MoneyAmount } from '@/lib/currency'
import { formatNumber } from '@/lib/format'
import {
  getServerErrorMessage,
  requireServerSuccess,
} from '@/lib/server-error-message'

interface MarginRow {
  model_offering_id?: number
  channel_id?: number
  provider_id?: number
  date?: string
  revenue: MoneyAmount
  collected: MoneyAmount
  outstanding: MoneyAmount
  cost: MoneyAmount
  original_costs: MoneyAmount[]
  margin: MoneyAmount | null
  known_margin: MoneyAmount
  sale_pending: number
  cost_pending: number
  converted_cost_pending: number
}
interface MarginFilters {
  model_offering_id: string
  channel_id: string
  provider_id: string
  from: string
  until: string
  timezone: string
  group_by: string
}
const initial: MarginFilters = {
  model_offering_id: '',
  channel_id: '',
  provider_id: '',
  from: '',
  until: '',
  timezone: 'UTC',
  group_by: 'model_offering,channel,provider,day',
}
const empty: MarginRow[] = []

export function MarginReports() {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const [draft, setDraft] = useState(initial)
  const [filters, setFilters] = useState(initial)
  const [pagination, setPagination] = useState({ pageIndex: 0, pageSize: 20 })
  const valid = [
    draft.model_offering_id,
    draft.channel_id,
    draft.provider_id,
  ].every(
    (value) =>
      value === '' ||
      (/^[1-9]\d*$/.test(value) && Number.isSafeInteger(Number(value)))
  )
  const query = useQuery({
    queryKey: ['margin-report', filters, pagination],
    queryFn: async ({ signal }) => {
      const params: Record<string, string | number> = {
        page: pagination.pageIndex + 1,
        page_size: pagination.pageSize,
      }
      for (const [key, value] of Object.entries(filters)) {
        if (value !== '') {
          params[key] = key.endsWith('_id') ? Number(value) : value
        }
      }
      const response = await api.get('/api/admin/v1/reports/margin', {
        params,
        signal,
      })
      return requireServerSuccess<{
        success: boolean
        data: { items: MarginRow[]; total: number }
      }>(response.data).data
    },
  })
  const columns: ColumnDef<MarginRow>[] = [
    {
      accessorKey: 'model_offering_id',
      header: t('Model Offering ID'),
      meta: { mobileTitle: true },
    },
    { accessorKey: 'channel_id', header: t('Channel ID') },
    { accessorKey: 'provider_id', header: t('Provider ID') },
    { accessorKey: 'date', header: t('Date') },
    {
      id: 'margin',
      header: t('Gross margin'),
      cell: ({ row }) => (
        <div>
          <RequestMoney
            amount={row.original.margin ?? undefined}
            pending={!row.original.margin}
          />
          {!row.original.margin && (
            <span className='block text-xs'>{t('Incomplete accounting')}</span>
          )}
        </div>
      ),
    },
    {
      id: 'revenue',
      header: t('Known revenue'),
      cell: ({ row }) => (
        <div>
          <RequestMoney amount={row.original.revenue} />
          {row.original.sale_pending > 0 && (
            <span className='block text-xs'>
              {t('Pending sales')}:{' '}
              {formatNumber(row.original.sale_pending, locale)}
            </span>
          )}
        </div>
      ),
    },
    {
      id: 'collected',
      header: t('Collected'),
      cell: ({ row }) => <RequestMoney amount={row.original.collected} />,
    },
    {
      id: 'outstanding',
      header: t('Known outstanding'),
      cell: ({ row }) => <RequestMoney amount={row.original.outstanding} />,
    },
    {
      id: 'cost',
      header: t('Known purchase cost'),
      cell: ({ row }) => <RequestMoney amount={row.original.cost} />,
    },
    {
      id: 'original',
      header: t('Original-currency purchase costs'),
      cell: ({ row }) => (
        <div className='space-y-1'>
          {row.original.original_costs.map((amount) => (
            <RequestMoney key={amount.currency} amount={amount} />
          ))}
        </div>
      ),
    },
    {
      id: 'known_margin',
      header: t('Known revenue less known costs'),
      cell: ({ row }) => <RequestMoney amount={row.original.known_margin} />,
    },
    {
      id: 'pending',
      header: t('Pending accounting'),
      cell: ({ row }) => (
        <dl className='text-xs'>
          <dt>{t('Pending sales')}</dt>
          <dd>{formatNumber(row.original.sale_pending, locale)}</dd>
          <dt>{t('Pending purchase costs')}</dt>
          <dd>{formatNumber(row.original.cost_pending, locale)}</dd>
          <dt>{t('Pending cost conversions')}</dt>
          <dd>{formatNumber(row.original.converted_cost_pending, locale)}</dd>
        </dl>
      ),
    },
  ]
  const { table } = useDataTable({
    columns,
    data: query.isError ? empty : (query.data?.items ?? empty),
    pagination,
    onPaginationChange: setPagination,
    totalCount: query.data?.total ?? 0,
    manualPagination: true,
    manualFiltering: true,
    enableSorting: false,
    enableRowSelection: false,
    getRowId: (row) =>
      `${row.model_offering_id}:${row.channel_id}:${row.provider_id}:${row.date}`,
  })
  return (
    <SectionPageLayout>
      <SectionPageLayout.Title>{t('Margin reports')}</SectionPageLayout.Title>
      <SectionPageLayout.Content>
        <div className='flex flex-col gap-3'>
          <p className='text-muted-foreground text-sm'>
            {t(
              'Revenue includes unpaid receivables once. Each attempt contributes its cost. Revenue belongs to the final attempt. Dates use request start time; the end date is exclusive.'
            )}
          </p>
          <form
            className='flex flex-wrap items-end gap-2'
            onSubmit={(event) => {
              event.preventDefault()
              if (!valid) return
              setFilters(draft)
              setPagination((value) => ({ ...value, pageIndex: 0 }))
            }}
          >
            {(
              [
                ['model_offering_id', t('Model Offering ID')],
                ['channel_id', t('Channel ID')],
                ['provider_id', t('Provider ID')],
                ['from', t('From date')],
                ['until', t('Until date')],
                ['timezone', t('Time zone')],
              ] as const
            ).map(([key, label]) => (
              <label key={key} className='grid gap-1 text-xs'>
                {label}
                <Input
                  className='w-40'
                  type={key === 'from' || key === 'until' ? 'date' : 'text'}
                  value={draft[key]}
                  onChange={(event) =>
                    setDraft((value) => ({
                      ...value,
                      [key]: event.target.value,
                    }))
                  }
                />
              </label>
            ))}
            <label className='grid gap-1 text-xs'>
              {t('Group by')}
              <NativeSelect
                value={draft.group_by}
                onChange={(event) =>
                  setDraft((value) => ({
                    ...value,
                    group_by: event.target.value,
                  }))
                }
              >
                <NativeSelectOption value='model_offering,channel,provider,day'>
                  {t('All dimensions')}
                </NativeSelectOption>
                <NativeSelectOption value='model_offering'>
                  {t('Model Offering')}
                </NativeSelectOption>
                <NativeSelectOption value='channel'>
                  {t('Channel')}
                </NativeSelectOption>
                <NativeSelectOption value='provider'>
                  {t('Provider')}
                </NativeSelectOption>
                <NativeSelectOption value='day'>{t('Date')}</NativeSelectOption>
                <NativeSelectOption value='model_offering,day'>
                  {t('Model Offering')} / {t('Date')}
                </NativeSelectOption>
                <NativeSelectOption value='channel,day'>
                  {t('Channel')} / {t('Date')}
                </NativeSelectOption>
                <NativeSelectOption value='provider,day'>
                  {t('Provider')} / {t('Date')}
                </NativeSelectOption>
              </NativeSelect>
            </label>
            <Button
              type='submit'
              variant='outline'
              disabled={!valid || query.isFetching}
            >
              {t('Apply filters')}
            </Button>
          </form>
          {query.isError ? (
            <ErrorState
              description={getServerErrorMessage(query.error)}
              onRetry={() => void query.refetch()}
            />
          ) : (
            <DataTablePage
              fixedHeight={false}
              table={table}
              columns={columns}
              isLoading={query.isPending}
              isFetching={query.isFetching}
              toolbarProps={null}
              emptyTitle={t('No margin records')}
            />
          )}
        </div>
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}
