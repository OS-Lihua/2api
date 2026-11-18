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
import { getRouteApi } from '@tanstack/react-router'
import type { OnChangeFn, SortingState } from '@tanstack/react-table'
import { useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'

import {
  DISABLED_ROW_DESKTOP,
  DISABLED_ROW_MOBILE,
  DataTablePage,
  useDataTable,
} from '@/components/data-table'
import { ErrorState } from '@/components/error-state'
import { useMediaQuery } from '@/hooks'
import { useTableUrlState } from '@/hooks/use-table-url-state'
import {
  createServerError,
  requireServerSuccess,
} from '@/lib/server-error-message'
import { isWalletScopeCurrent, walletReadScope } from '@/lib/wallet-summary'
import { useAuthStore } from '@/stores/auth-store'

import {
  getGroups,
  getUsers,
  getUserWalletSummaries,
  searchUsers,
} from '../api'
import {
  USER_STATUS,
  getUserStatusOptions,
  getUserRoleOptions,
  isUserDeleted,
} from '../constants'
import type { User, UserSortBy } from '../types'
import { DataTableBulkActions } from './data-table-bulk-actions'
import { useUsersColumns } from './users-columns'
import { useUsers } from './users-provider'

const route = getRouteApi('/_authenticated/users/')

const USER_SORTABLE_COLUMNS = new Set<UserSortBy>([
  'id',
  'username',
  'group',
  'created_at',
  'last_login_at',
])

function isDisabledUserRow(user: User) {
  return isUserDeleted(user) || user.status === USER_STATUS.DISABLED
}

export function UsersTable() {
  const { t } = useTranslation()
  const actor = useAuthStore((state) => state.auth.user)
  const sessionId = useAuthStore((state) => state.auth.session?.sid ?? null)
  const walletScope = walletReadScope('ledger')
  const { refreshTrigger } = useUsers()
  const isMobile = useMediaQuery('(max-width: 640px)')
  const [sorting, setSorting] = useState<SortingState>([])

  const {
    globalFilter,
    onGlobalFilterChange,
    columnFilters,
    onColumnFiltersChange,
    pagination: requestedPagination,
    onPaginationChange,
    ensurePageInRange,
  } = useTableUrlState({
    search: route.useSearch(),
    navigate: route.useNavigate(),
    pagination: { defaultPage: 1, defaultPageSize: isMobile ? 10 : 20 },
    globalFilter: { enabled: true, key: 'filter' },
    columnFilters: [
      { columnId: 'status', searchKey: 'status', type: 'array' },
      { columnId: 'role', searchKey: 'role', type: 'array' },
      { columnId: 'group', searchKey: 'group', type: 'array' },
    ],
  })
  const pagination = useMemo(
    () => ({
      pageIndex:
        Number.isSafeInteger(requestedPagination.pageIndex) &&
        requestedPagination.pageIndex >= 0
          ? requestedPagination.pageIndex
          : 0,
      pageSize: Number.isFinite(requestedPagination.pageSize)
        ? Math.max(1, Math.min(100, Math.floor(requestedPagination.pageSize)))
        : 20,
    }),
    [requestedPagination.pageIndex, requestedPagination.pageSize]
  )
  useEffect(() => {
    if (
      pagination.pageSize !== requestedPagination.pageSize ||
      pagination.pageIndex !== requestedPagination.pageIndex
    ) {
      onPaginationChange(pagination)
    }
  }, [
    pagination,
    requestedPagination.pageIndex,
    requestedPagination.pageSize,
    onPaginationChange,
  ])
  const statusFilter =
    (columnFilters.find((filter) => filter.id === 'status')?.value as
      | string[]
      | undefined) ?? []
  const roleFilter =
    (columnFilters.find((filter) => filter.id === 'role')?.value as
      | string[]
      | undefined) ?? []
  const groupFilter =
    (columnFilters.find((filter) => filter.id === 'group')?.value as
      | string[]
      | undefined) ?? []

  const { data: groupsData } = useQuery({
    queryKey: ['groups'],
    queryFn: async () => requireServerSuccess(await getGroups()),
    staleTime: 5 * 60 * 1000,
  })

  const sortParams = useMemo(() => {
    const activeSort = sorting[0]
    if (
      !activeSort ||
      !USER_SORTABLE_COLUMNS.has(activeSort.id as UserSortBy)
    ) {
      return {}
    }

    return {
      sort_by: activeSort.id as UserSortBy,
      sort_order: activeSort.desc ? 'desc' : 'asc',
    } as const
  }, [sorting])

  const handleSortingChange: OnChangeFn<SortingState> = (updater) => {
    setSorting(updater)
    if (pagination.pageIndex > 0) {
      onPaginationChange({ ...pagination, pageIndex: 0 })
    }
  }

  // Fetch data with React Query
  const { data, dataUpdatedAt, isLoading, isFetching } = useQuery({
    queryKey: [
      'users',
      actor?.id,
      sessionId,
      pagination.pageIndex + 1,
      pagination.pageSize,
      globalFilter,
      statusFilter,
      roleFilter,
      groupFilter,
      sortParams,
      refreshTrigger,
    ],
    queryFn: async ({ signal }) => {
      const hasFilter = globalFilter?.trim()
      const hasColumnFilter =
        statusFilter.length > 0 ||
        roleFilter.length > 0 ||
        groupFilter.length > 0
      const params = {
        p: pagination.pageIndex + 1,
        page_size: pagination.pageSize,
        ...sortParams,
      }

      const result =
        hasFilter || hasColumnFilter
          ? await searchUsers(
              {
                ...params,
                keyword: globalFilter,
                status: statusFilter[0] ?? '',
                role: roleFilter[0] ?? '',
                group: groupFilter[0] ?? '',
              },
              signal
            )
          : await getUsers(params, signal)
      const current = useAuthStore.getState().auth
      if (
        signal.aborted ||
        current.user?.id !== actor?.id ||
        (current.session?.sid ?? null) !== sessionId
      ) {
        throw new Error('Session changed')
      }

      if (!result.success) {
        throw createServerError(
          result,
          t(hasFilter ? 'Failed to search users' : 'Failed to load users')
        )
      }

      return {
        items: result.data?.items || [],
        total: result.data?.total || 0,
      }
    },
    placeholderData: (previousData, previousQuery) =>
      previousQuery?.queryKey[1] === actor?.id &&
      previousQuery?.queryKey[2] === sessionId
        ? previousData
        : undefined,
  })

  const users = data?.items || []
  const walletIDs = [
    ...new Set(
      users.filter((user) => !isUserDeleted(user)).map((user) => user.id)
    ),
  ].sort((a, b) => a - b)
  const walletQuery = useQuery({
    queryKey: [
      'admin-user-wallets',
      actor?.id,
      sessionId,
      walletScope.allowed,
      walletIDs,
      refreshTrigger,
      dataUpdatedAt,
    ],
    queryFn: async ({ signal }) => {
      if (!isWalletScopeCurrent(walletScope, 'ledger')) {
        throw new Error('Permission denied')
      }
      const result = await getUserWalletSummaries(walletIDs, signal)
      if (signal.aborted || !isWalletScopeCurrent(walletScope, 'ledger')) {
        throw new Error('Session changed')
      }
      return result
    },
    enabled: walletScope.allowed && walletIDs.length > 0 && !isFetching,
    staleTime: 0,
    gcTime: 0,
    retry: false,
    meta: { errorToast: false },
  })
  const walletState = useMemo(
    () => ({
      visible: walletScope.allowed,
      byId: new Map(
        (walletScope.allowed &&
        !walletQuery.isError &&
        !walletQuery.isFetching &&
        !isFetching
          ? (walletQuery.data ?? [])
          : []
        ).map((wallet) => [wallet.user_id, wallet])
      ),
      loading:
        isFetching ||
        walletQuery.isFetching ||
        (walletIDs.length > 0 && walletQuery.isPending),
    }),
    [
      walletScope.allowed,
      walletQuery.data,
      walletQuery.isError,
      walletQuery.isFetching,
      walletQuery.isPending,
      isFetching,
      walletIDs.length,
    ]
  )
  const columns = useUsersColumns(walletState)

  const { table } = useDataTable({
    data: users,
    columns,
    enableRowSelection: true,
    columnFilters,
    globalFilter,
    pagination,
    sorting,
    globalFilterFn: (row, _columnId, filterValue) => {
      const searchValue = String(filterValue).toLowerCase()
      const fields = [
        row.getValue('username'),
        row.original.display_name,
        row.original.email,
      ]
      return fields.some((field) =>
        String(field || '')
          .toLowerCase()
          .includes(searchValue)
      )
    },
    onPaginationChange,
    onGlobalFilterChange,
    onColumnFiltersChange,
    onSortingChange: handleSortingChange,
    manualPagination: true,
    manualFiltering: true,
    manualSorting: true,
    totalCount: data?.total || 0,
    ensurePageInRange,
  })

  return (
    <>
      {walletScope.allowed && walletQuery.isError && (
        <ErrorState
          description={t('Failed to load wallet balances')}
          onRetry={() => void walletQuery.refetch()}
          className='min-h-0'
        />
      )}
      <DataTablePage
        table={table}
        columns={columns}
        isLoading={isLoading}
        isFetching={isFetching}
        emptyTitle={t('No Users Found')}
        emptyDescription={t(
          'No users available. Try adjusting your search or filters.'
        )}
        skeletonKeyPrefix='users-skeleton'
        applyHeaderSize
        toolbarProps={{
          searchPlaceholder: t('Filter by username, name or email...'),
          searchDebounceMs: 500,
          filters: [
            {
              columnId: 'status',
              title: t('Status'),
              options: getUserStatusOptions(t),
              singleSelect: true,
            },
            {
              columnId: 'role',
              title: t('Role'),
              options: getUserRoleOptions(t),
              singleSelect: true,
            },
            {
              columnId: 'group',
              title: t('User Group'),
              options: (groupsData?.data ?? []).map((group) => ({
                label: group,
                value: group,
              })),
              singleSelect: true,
            },
          ],
        }}
        getRowClassName={(row, { isMobile }) => {
          if (!isDisabledUserRow(row.original)) return undefined
          return isMobile ? DISABLED_ROW_MOBILE : DISABLED_ROW_DESKTOP
        }}
        bulkActions={<DataTableBulkActions table={table} />}
      />
    </>
  )
}
