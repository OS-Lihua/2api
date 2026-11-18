import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'

import { api } from '@/lib/api'
import { requireServerSuccess } from '@/lib/server-error-message'
import {
  isWalletScopeCurrent,
  selfWalletSummarySchema,
  walletReadScope,
} from '@/lib/wallet-summary'
import { useAuthStore } from '@/stores/auth-store'

import {
  defaultFinanceFilter,
  financeReportSchema,
  validFinanceFilter,
  type FinanceFilter,
} from '../lib/finance'

export const dashboardFinanceKey = ['dashboard-finance'] as const
export function useFinanceReport(admin: boolean, filter: FinanceFilter) {
  const user = useAuthStore((state) => state.auth.user)
  const sid = useAuthStore((state) => state.auth.session?.sid)
  const resource = admin ? 'ledger' : 'wallet'
  const scope = walletReadScope(resource)
  const valid = validFinanceFilter(filter, admin)
  const query = useQuery({
    queryKey: [
      ...dashboardFinanceKey,
      user?.id,
      sid,
      admin,
      scope.allowed,
      filter,
    ],
    enabled: scope.allowed && valid,
    staleTime: 0,
    gcTime: 0,
    retry: false,
    meta: { errorToast: false },
    queryFn: async ({ signal }) => {
      if (!isWalletScopeCurrent(scope, resource)) {
        throw new Error('Financial access changed')
      }
      const response = await api.get(
        admin
          ? '/api/admin/v1/reports/dashboard-finance'
          : '/api/user/self/dashboard-finance',
        { params: filter, signal }
      )
      if (signal.aborted || !isWalletScopeCurrent(scope, resource)) {
        throw new Error('Financial access changed')
      }
      const report = financeReportSchema.parse(
        requireServerSuccess(response.data).data
      )
      if (
        report.from !== Date.parse(filter.from) ||
        report.until !== Date.parse(filter.until) ||
        report.bucket !== filter.bucket ||
        report.timezone !== filter.timezone ||
        report.group_by.join(',') !== (filter.group_by ?? '') ||
        (admin &&
          (!report.cost_attribution ||
            !report.totals.procurement ||
            !report.totals.margin ||
            report.items.some((item) => !item.procurement || !item.margin))) ||
        (!admin &&
          (report.cost_attribution ||
            report.totals.procurement ||
            report.totals.margin ||
            report.items.some((item) => item.procurement || item.margin)))
      ) {
        throw new Error('Invalid financial report')
      }
      return report
    },
  })
  return {
    ...query,
    data: scope.allowed && valid && !query.isError ? query.data : undefined,
    allowed: scope.allowed,
    valid,
  }
}
export function useDashboardWallet() {
  const user = useAuthStore((state) => state.auth.user)
  const sid = useAuthStore((state) => state.auth.session?.sid)
  const scope = walletReadScope('wallet')
  const query = useQuery({
    queryKey: ['self-wallet', user?.id, sid, 'dashboard', scope.allowed],
    enabled: scope.allowed,
    staleTime: 0,
    gcTime: 0,
    retry: false,
    meta: { errorToast: false },
    queryFn: async ({ signal }) => {
      if (!isWalletScopeCurrent(scope, 'wallet')) {
        throw new Error('Financial access changed')
      }
      const response = await api.get('/api/user/self/wallet', { signal })
      if (signal.aborted || !isWalletScopeCurrent(scope, 'wallet')) {
        throw new Error('Financial access changed')
      }
      const wallet = selfWalletSummarySchema.parse(
        requireServerSuccess(response.data).data
      )
      if (wallet.user_id !== scope.userId) {
        throw new Error('Invalid wallet owner')
      }
      return wallet
    },
  })
  return {
    ...query,
    data: scope.allowed && !query.isError ? query.data : undefined,
    allowed: scope.allowed,
  }
}
export function useOverviewFinance() {
  const [filter] = useState(defaultFinanceFilter)
  const wallet = useDashboardWallet()
  const report = useFinanceReport(false, filter)
  return { wallet, report }
}
