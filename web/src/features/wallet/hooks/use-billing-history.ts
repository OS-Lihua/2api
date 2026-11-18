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
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { useIsAdmin } from '@/hooks/use-admin'
import { useDebounce } from '@/hooks/use-debounce'
import { handleServerError } from '@/lib/handle-server-error'
import { ROLE } from '@/lib/roles'
import {
  createServerError,
  requireServerSuccess,
} from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

import {
  completeOrder,
  getAllBillingHistory,
  getUserBillingHistory,
} from '../api'

export function useBillingHistory(
  options: {
    initialPage?: number
    initialPageSize?: number
    enabled?: boolean
  } = {}
) {
  const { t } = useTranslation()
  const userId = useAuthStore((state) => state.auth.user?.id)
  const sessionId = useAuthStore((state) => state.auth.session?.sid ?? null)
  const isAdmin = useIsAdmin()
  const queryClient = useQueryClient()
  const [page, setPage] = useState(options.initialPage ?? 1)
  const [pageSize, setPageSize] = useState(options.initialPageSize ?? 10)
  const [keyword, setKeyword] = useState('')
  const debouncedKeyword = useDebounce(keyword)
  const enabled = !!userId && options.enabled !== false
  const query = useQuery({
    queryKey: [
      'billing-history',
      userId,
      sessionId,
      isAdmin,
      page,
      pageSize,
      debouncedKeyword,
    ],
    enabled,
    meta: { errorToast: false },
    queryFn: async ({ signal }) => {
      const response = isAdmin
        ? await getAllBillingHistory(page, pageSize, debouncedKeyword, signal)
        : await getUserBillingHistory(page, pageSize, debouncedKeyword, signal)
      requireServerSuccess(response)
      if (!response.data) {
        throw createServerError(response, 'Failed to load billing history')
      }
      return response.data
    },
  })
  const isCurrentAdmin = () => {
    const auth = useAuthStore.getState().auth
    return (
      isAdmin &&
      auth.user?.id === userId &&
      (auth.session?.sid ?? null) === sessionId &&
      (auth.user?.role ?? 0) >= ROLE.ADMIN
    )
  }
  const completion = useMutation({
    meta: { errorToast: false },
    mutationFn: async (tradeNo: string) => {
      if (!isCurrentAdmin()) return false
      const response = await completeOrder({ trade_no: tradeNo })
      requireServerSuccess(response)
      if (!isCurrentAdmin()) return false
      toast.success(t('Order completed successfully'))
      await Promise.all([
        queryClient.invalidateQueries({
          queryKey: ['billing-history', userId, sessionId],
        }),
        queryClient.invalidateQueries({ queryKey: ['self-wallet'] }),
        queryClient.invalidateQueries({ queryKey: ['admin-user-wallets'] }),
        queryClient.invalidateQueries({ queryKey: ['admin-user-wallet'] }),
      ])
      return true
    },
    onError: (error) => {
      if (isCurrentAdmin()) {
        handleServerError(error, t('Failed to complete order'))
      }
    },
  })

  return {
    records: query.isError ? [] : (query.data?.items ?? []),
    total: query.isError ? 0 : (query.data?.total ?? 0),
    page,
    pageSize,
    keyword,
    loading: enabled && (query.isPending || keyword !== debouncedKeyword),
    error: query.error,
    completing: completion.isPending,
    isAdmin,
    handlePageChange: setPage,
    handlePageSizeChange: (value: number) => {
      setPageSize(value)
      setPage(1)
    },
    handleSearch: (value: string) => {
      setKeyword(value)
      setPage(1)
    },
    handleCompleteOrder: async (tradeNo: string) => {
      try {
        return await completion.mutateAsync(tradeNo)
      } catch {
        return false
      }
    },
    refresh: query.refetch,
  }
}
