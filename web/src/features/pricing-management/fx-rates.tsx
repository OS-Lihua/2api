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
import { keepPreviousData, useQuery } from '@tanstack/react-query'
import type { ColumnDef } from '@tanstack/react-table'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { DataTablePage, useDataTable } from '@/components/data-table'
import { ErrorState } from '@/components/error-state'
import { SectionPageLayout } from '@/components/layout'
import { Button } from '@/components/ui/button'
import { toIntlLocale } from '@/i18n/languages'
import { hasPermission } from '@/lib/admin-permissions'
import { getServerErrorMessage } from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

import { listFxRates, pricingKeys } from './api'
import { FxDialog } from './components/fx-dialog'
import { FxQuoteDialog } from './components/fx-quote-dialog'
import type { FxRate } from './types'

export function FxRates() {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const user = useAuthStore((state) => state.auth.user)
  const canWrite = hasPermission(user, 'pricing', 'write')
  const [quote, setQuote] = useState<FxRate>()
  const [editing, setEditing] = useState<FxRate | null | undefined>()
  const [pagination, setPagination] = useState({ pageIndex: 0, pageSize: 20 })
  const params = { p: pagination.pageIndex + 1, page_size: pagination.pageSize }
  const query = useQuery({
    queryKey: [...pricingKeys.fx, 'list', params],
    queryFn: () => listFxRates(params),
    placeholderData: keepPreviousData,
    refetchOnMount: 'always',
  })
  const columns: ColumnDef<FxRate>[] = [
    {
      id: 'pair',
      header: t('Currency pair'),
      meta: { mobileTitle: true },
      cell: ({ row }) =>
        `${row.original.from_currency} → ${row.original.to_currency}`,
    },
    {
      accessorKey: 'rate',
      header: t('Raw exchange rate'),
      cell: ({ row }) => (
        <code className='break-all whitespace-normal'>{row.original.rate}</code>
      ),
    },
    { accessorKey: 'buffer_rate', header: t('Comparison buffer rate') },
    { accessorKey: 'updated_by', header: t('Updated by') },
    {
      accessorKey: 'updated_at',
      header: t('Updated at'),
      cell: ({ row }) =>
        new Date(row.original.updated_at * 1000).toLocaleString(locale),
    },
  ]
  columns.push({
    id: 'quote',
    header: t('Quote'),
    cell: ({ row }) => (
      <Button
        size='sm'
        variant='ghost'
        aria-label={t('Quote {{from}} to {{to}}', {
          from: row.original.from_currency,
          to: row.original.to_currency,
        })}
        onClick={() => setQuote(row.original)}
      >
        {t('Quote')}
      </Button>
    ),
  })
  if (canWrite) {
    columns.push({
      id: 'actions',
      header: t('Actions'),
      cell: ({ row }) => (
        <Button
          size='sm'
          variant='ghost'
          onClick={() => setEditing(row.original)}
          aria-label={t('Maintain {{from}} to {{to}} rate', {
            from: row.original.from_currency,
            to: row.original.to_currency,
          })}
        >
          {t('Maintain exchange rate')}
        </Button>
      ),
    })
  }
  const { table } = useDataTable({
    data: query.data?.items ?? [],
    columns,
    getRowId: (row) => `${row.from_currency}:${row.to_currency}`,
    pagination,
    onPaginationChange: setPagination,
    columnFilters: [],
    manualPagination: true,
    totalCount: query.data?.total ?? 0,
  })
  return (
    <SectionPageLayout fixedContent>
      <SectionPageLayout.Title>{t('Exchange rates')}</SectionPageLayout.Title>
      <SectionPageLayout.Actions>
        {canWrite && (
          <Button size='sm' onClick={() => setEditing(null)}>
            {t('Maintain exchange rate')}
          </Button>
        )}
      </SectionPageLayout.Actions>
      <SectionPageLayout.Content>
        {query.isError && (
          <ErrorState
            description={getServerErrorMessage(query.error)}
            onRetry={() => void query.refetch()}
          />
        )}
        <DataTablePage
          table={table}
          columns={columns}
          isLoading={query.isLoading}
          isFetching={query.isFetching}
          emptyTitle={t('No exchange rates found')}
          toolbarProps={null}
        />
        {quote && (
          <FxQuoteDialog rate={quote} onClose={() => setQuote(undefined)} />
        )}
        {editing !== undefined && (
          <FxDialog
            initial={editing ?? undefined}
            onClose={() => setEditing(undefined)}
          />
        )}
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}
