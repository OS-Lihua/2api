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
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { DataTablePage, useDataTable } from '@/components/data-table'
import { ErrorState } from '@/components/error-state'
import { SectionPageLayout } from '@/components/layout'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { getServerErrorMessage } from '@/lib/server-error-message'

import { getRequests } from './api'
import { AttemptsDrawer } from './components/attempts-drawer'
import { useRequestColumns } from './components/request-columns'
import { RequestFilterBar } from './components/request-filters'
import type { RequestFilters, RequestLog, RequestScope } from './types'
import { useRequestScope } from './use-request-scope'

const EMPTY_REQUESTS: RequestLog[] = []

export function RequestLogs() {
  const { t } = useTranslation()
  const access = useRequestScope()
  return (
    <SectionPageLayout fixedContent>
      <SectionPageLayout.Title>{t('Request logs')}</SectionPageLayout.Title>
      <SectionPageLayout.Actions>
        {access.canReadAll && (
          <Tabs
            value={access.scope}
            onValueChange={(scope) => {
              if (scope === 'all' || scope === 'self') access.setScope(scope)
            }}
          >
            <TabsList aria-label={t('Request scope')}>
              <TabsTrigger value='all'>{t('All')}</TabsTrigger>
              <TabsTrigger value='self'>{t('Only Mine')}</TabsTrigger>
            </TabsList>
          </Tabs>
        )}
      </SectionPageLayout.Actions>
      <SectionPageLayout.Content>
        {access.userId !== undefined && (
          <RequestLogViewer
            key={`${access.userId}:${access.scope}`}
            userId={access.userId}
            scope={access.scope}
          />
        )}
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}

function RequestLogViewer(props: { userId: number; scope: RequestScope }) {
  const { t } = useTranslation()
  const userId = props.userId
  const scope = props.scope
  const [selected, setSelected] = useState<RequestLog | null>(null)
  const [pagination, setPagination] = useState({ pageIndex: 0, pageSize: 20 })
  const [filters, setFilters] = useState<RequestFilters>({})
  const query = useQuery({
    queryKey: ['requests', userId, scope, filters, pagination],
    queryFn: ({ signal }) =>
      getRequests(
        scope,
        {
          ...filters,
          page: pagination.pageIndex + 1,
          page_size: pagination.pageSize,
        },
        signal
      ),
    staleTime: 0,
  })
  const columns = useRequestColumns(setSelected)
  const { table } = useDataTable({
    columns,
    data: query.isError
      ? EMPTY_REQUESTS
      : (query.data?.items ?? EMPTY_REQUESTS),
    getRowId: (request) => request.id,
    totalCount: query.data?.total ?? 0,
    pagination,
    onPaginationChange: (updater) => {
      if (query.isFetching || query.isError) return
      setSelected(null)
      setPagination((previous) => {
        const next = typeof updater === 'function' ? updater(previous) : updater
        return next.pageSize === previous.pageSize
          ? next
          : { ...next, pageIndex: 0 }
      })
    },
    manualPagination: true,
    manualFiltering: true,
    enableRowSelection: false,
    enableSorting: false,
  })
  return (
    <div className='flex h-full min-h-0 flex-col gap-3'>
      <RequestFilterBar
        table={table}
        isAdmin={scope === 'all'}
        isFetching={query.isFetching}
        onApply={(next) => {
          setSelected(null)
          if (
            JSON.stringify(next) === JSON.stringify(filters) &&
            pagination.pageIndex === 0
          ) {
            void query.refetch()
          }
          setFilters(next)
          setPagination((previous) => ({ ...previous, pageIndex: 0 }))
        }}
      />
      {query.isError ? (
        <ErrorState
          title={t('Failed to load requests')}
          description={getServerErrorMessage(query.error)}
          onRetry={() => void query.refetch()}
        />
      ) : (
        <DataTablePage
          table={table}
          columns={columns}
          isLoading={query.isPending}
          isFetching={query.isFetching}
          emptyTitle={t('No requests recorded')}
          toolbarProps={null}
          applyHeaderSize
          className='h-auto min-h-0 flex-1'
        />
      )}
      {selected && (
        <AttemptsDrawer
          key={selected.id}
          request={selected}
          scope={scope}
          userId={userId}
          onClose={() => setSelected(null)}
        />
      )}
    </div>
  )
}
