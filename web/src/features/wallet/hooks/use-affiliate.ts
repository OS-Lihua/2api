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
import i18next from 'i18next'
import { useRef, useState } from 'react'
import { toast } from 'sonner'

import type { MoneyAmount } from '@/lib/currency'
import { requireServerSuccess } from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

import {
  getAffiliateCode,
  getReferralRewards,
  transferAffiliateQuota,
} from '../api'
import { generateAffiliateLink } from '../lib'
import type { AffiliateTransferRequest } from '../types'

export function useAffiliate(userId: number) {
  const client = useQueryClient()
  const attempt = useRef<AffiliateTransferRequest | null>(null)
  const [transferAttempt, setTransferAttempt] =
    useState<AffiliateTransferRequest | null>(null)
  const rewards = useQuery({
    queryKey: ['referral-rewards', userId],
    queryFn: ({ signal }) => getReferralRewards(signal),
    enabled: !!userId,
    meta: { errorToast: false },
  })
  const code = useQuery({
    queryKey: ['affiliate-code', userId],
    queryFn: async ({ signal }) => {
      const response = requireServerSuccess(await getAffiliateCode(signal))
      if (typeof response.data !== 'string') {
        throw new Error('Referral rewards unavailable')
      }
      return response.data
    },
    enabled: !!userId,
    meta: { errorToast: false },
  })
  const transfer = useMutation({
    meta: { errorToast: false },
    mutationFn: async (request: AffiliateTransferRequest) => {
      if (useAuthStore.getState().auth.user?.id !== userId) return false
      requireServerSuccess(await transferAffiliateQuota(request))
      if (useAuthStore.getState().auth.user?.id !== userId) return false
      toast.success(i18next.t('Transfer successful'))
      void client.invalidateQueries({ queryKey: ['referral-rewards', userId] })
      void client.invalidateQueries({ queryKey: ['self-wallet', userId] })
      return true
    },
  })

  async function transferRewards(amount: MoneyAmount): Promise<boolean> {
    if (useAuthStore.getState().auth.user?.id !== userId) return false
    if (transfer.isPending) return false
    if (!attempt.current) {
      attempt.current = { amount, transfer_id: crypto.randomUUID() }
      setTransferAttempt(attempt.current)
    }
    if (
      attempt.current.amount.nanos !== amount.nanos ||
      attempt.current.amount.currency !== amount.currency
    ) {
      throw new Error('Retry the pending transfer before changing the amount')
    }
    const success = await transfer.mutateAsync(attempt.current)
    if (success) {
      attempt.current = null
      setTransferAttempt(null)
    }
    return success
  }

  return {
    rewards: rewards.data ?? null,
    affiliateLink: code.data ? generateAffiliateLink(code.data) : '',
    loading: rewards.isPending || code.isPending,
    error: rewards.error ?? code.error,
    transferring: transfer.isPending,
    transferAttempt,
    transferRewards,
    refetch: () => Promise.all([rewards.refetch(), code.refetch()]),
  }
}
