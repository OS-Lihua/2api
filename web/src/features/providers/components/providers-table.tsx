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
import { getRouteApi } from '@tanstack/react-router'
import { Fragment } from 'react'
import { useTranslation } from 'react-i18next'

import {
  DISABLED_ROW_DESKTOP,
  DISABLED_ROW_MOBILE,
  DataTablePage,
  DataTableRow,
  useDataTable,
} from '@/components/data-table'
import { TableRow, TableCell } from '@/components/ui/table'
import { CHANNEL_STATUS } from '@/features/channels/constants'
import { useTableUrlState } from '@/hooks/use-table-url-state'
import { createServerError } from '@/lib/server-error-message'

import { listProviders, providersQueryKeys } from '../api'
import { ERROR_MESSAGES, PROVIDER_STATUS_FILTER_OPTIONS } from '../constants'
import { ProviderChannels } from './provider-channels'
import { useProvidersColumns } from './providers-columns'

const route = getRouteApi('/_authenticated/providers/')

export function ProvidersTable() {
  const { t } = useTranslation()
  const columns = useProvidersColumns()
  const {
    globalFilter,
    onGlobalFilterChange,
    columnFilters,
    onColumnFiltersChange,
    pagination,
    onPaginationChange,
    ensurePageInRange,
  } = useTableUrlState({
    search: route.useSearch(),
    navigate: route.useNavigate(),
    pagination: { defaultPage: 1, defaultPageSize: 20 },
    globalFilter: { enabled: true, key: 'filter' },
    columnFilters: [{ columnId: 'status', searchKey: 'status', type: 'array' }],
  })
  const statusFilter =
    (columnFilters.find((filter) => filter.id === 'status')?.value as
      | string[]
      | undefined) ?? []

  const params = {
    p: pagination.pageIndex + 1,
    page_size: pagination.pageSize,
    keyword: globalFilter?.trim() || undefined,
    status: statusFilter[0] || undefined,
  }
  const { data, isLoading, isFetching } = useQuery({
    queryKey: providersQueryKeys.list(params),
    queryFn: async () => {
      const result = await listProviders(params)
      if (!result.success) {
        throw createServerError(result, t(ERROR_MESSAGES.LOAD_FAILED))
      }
      return { items: result.data?.items ?? [], total: result.data?.total ?? 0 }
    },
    placeholderData: keepPreviousData,
  })

  const { table } = useDataTable({
    data: data?.items ?? [],
    columns,
    getRowId: (row) => String(row.id),
    columnFilters,
    globalFilter,
    pagination,
    onPaginationChange,
    onGlobalFilterChange,
    onColumnFiltersChange,
    manualPagination: true,
    manualFiltering: true,
    totalCount: data?.total ?? 0,
    ensurePageInRange,
  })

  return (
    <DataTablePage
      table={table}
      hideMobile
      renderRow={(row, helpers) => (
        <Fragment key={row.id}>
          <DataTableRow
            row={row}
            cellRenderColumns={columns}
            getColumnClassName={(columnId) =>
              helpers.getCellClassName(columnId)
            }
          />
          {row.getIsExpanded() && (
            <TableRow>
              <TableCell colSpan={row.getVisibleCells().length} className='p-0'>
                <div id={`provider-channels-${row.original.id}`}>
                  <ProviderChannels providerId={row.original.id} />
                </div>
              </TableCell>
            </TableRow>
          )}
        </Fragment>
      )}
      columns={columns}
      isLoading={isLoading}
      isFetching={isFetching}
      emptyTitle={t('No providers found')}
      emptyDescription={t(
        'Add an upstream account as a provider to start routing requests to it.'
      )}
      skeletonKeyPrefix='providers-skeleton'
      applyHeaderSize
      toolbarProps={{
        searchPlaceholder: t('Filter by name, ID, or base URL...'),
        searchDebounceMs: 500,
        filters: [
          {
            columnId: 'status',
            title: t('Status'),
            options: PROVIDER_STATUS_FILTER_OPTIONS.map((option) => ({
              value: option.value,
              label: t(option.label),
            })),
            singleSelect: true,
          },
        ],
      }}
      getRowClassName={(row, { isMobile }) => {
        if (row.original.status === CHANNEL_STATUS.ENABLED) return undefined
        return isMobile ? DISABLED_ROW_MOBILE : DISABLED_ROW_DESKTOP
      }}
    />
  )
}
