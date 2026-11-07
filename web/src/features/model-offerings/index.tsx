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
import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import {
  ADMIN_PERMISSION_ACTIONS,
  ADMIN_PERMISSION_RESOURCES,
  hasPermission,
} from '@/lib/admin-permissions'
import { useAuthStore } from '@/stores/auth-store'

import { listOfferings, offeringKeys } from './api'
import { OfferingDialog } from './components/offering-dialog'
import { OfferingRowActions } from './components/offering-row-actions'
import type { ModelOffering } from './types'

export function ModelOfferings() {
  const { t } = useTranslation()
  const user = useAuthStore((state) => state.auth.user)
  const canWrite = hasPermission(
    user,
    ADMIN_PERMISSION_RESOURCES.MODEL,
    ADMIN_PERMISSION_ACTIONS.WRITE
  )
  const [pagination, setPagination] = useState({ pageIndex: 0, pageSize: 20 })
  const [keyword, setKeyword] = useState('')
  const [editing, setEditing] = useState<number | null | undefined>()
  const params = {
    p: pagination.pageIndex + 1,
    page_size: pagination.pageSize,
    keyword,
  }
  const query = useQuery({
    queryKey: offeringKeys.list(params),
    queryFn: () => listOfferings(params),
    placeholderData: keepPreviousData,
  })
  const columns: ColumnDef<ModelOffering>[] = [
    {
      accessorKey: 'name',
      header: t('Primary name'),
      meta: { mobileTitle: true },
      cell: ({ row }) => (
        <span className='break-all whitespace-normal'>{row.original.name}</span>
      ),
    },
    {
      accessorKey: 'aliases',
      header: t('Aliases'),
      cell: ({ row }) => (
        <span className='break-all whitespace-normal'>
          {row.original.aliases.join(', ') || '—'}
        </span>
      ),
    },
    {
      accessorKey: 'visibility',
      header: t('Visibility'),
      cell: ({ row }) => (
        <span className='break-all whitespace-normal'>
          {row.original.visibility === 'all'
            ? t('All users')
            : row.original.groups.join(', ')}
        </span>
      ),
    },
    {
      accessorKey: 'route_method',
      header: t('Route method'),
      cell: ({ row }) =>
        ({ cost: t('Cost'), weight: t('Weight'), latency: t('Latency') })[
          row.original.route_method
        ],
    },
    {
      accessorKey: 'enabled',
      header: t('Status'),
      cell: ({ row }) => (
        <StatusBadge
          label={row.original.enabled ? t('Enabled') : t('Disabled')}
          variant={row.original.enabled ? 'success' : 'neutral'}
          copyable={false}
        />
      ),
    },
  ]
  if (canWrite) {
    columns.push({
      id: 'actions',
      header: t('Actions'),
      cell: ({ row }) => (
        <OfferingRowActions
          offering={row.original}
          onEdit={() => setEditing(row.original.id)}
        />
      ),
    })
  }
  const { table } = useDataTable({
    data: query.data?.items ?? [],
    columns,
    getRowId: (row) => String(row.id),
    pagination,
    onPaginationChange: setPagination,
    globalFilter: keyword,
    onGlobalFilterChange: (value) => {
      setKeyword(typeof value === 'function' ? value(keyword) : value)
      setPagination((current) => ({ ...current, pageIndex: 0 }))
    },
    columnFilters: [],
    manualFiltering: true,
    manualPagination: true,
    totalCount: query.data?.total ?? 0,
    ensurePageInRange: (pageCount) =>
      setPagination((current) =>
        current.pageIndex < Math.max(1, pageCount)
          ? current
          : { ...current, pageIndex: Math.max(0, pageCount - 1) }
      ),
  })
  return (
    <>
      <SectionPageLayout fixedContent>
        <SectionPageLayout.Title>
          {t('Model offerings')}
        </SectionPageLayout.Title>
        <SectionPageLayout.Actions>
          {canWrite && (
            <Button size='sm' onClick={() => setEditing(null)}>
              {t('New model offering')}
            </Button>
          )}
        </SectionPageLayout.Actions>
        <SectionPageLayout.Content>
          {query.isError ? (
            <ErrorState onRetry={() => void query.refetch()} />
          ) : (
            <DataTablePage
              table={table}
              columns={columns}
              isLoading={query.isLoading}
              isFetching={query.isFetching}
              emptyTitle={t('No model offerings found')}
              toolbarProps={{
                searchPlaceholder: t('Search model offerings...'),
                searchDebounceMs: 300,
              }}
            />
          )}
        </SectionPageLayout.Content>
      </SectionPageLayout>
      {editing !== undefined && (
        <OfferingDialog id={editing} onClose={() => setEditing(undefined)} />
      )}
    </>
  )
}
