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

import { createServerError } from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

import { getTopupInfo, isApiSuccess } from '../api'
import { checkoutInfoSchema } from '../lib/checkout'
import type { PaymentMethod, WaffoPayMethod } from '../types'

function parseMethods(value: unknown): Record<string, unknown>[] {
  let items = value
  if (typeof items === 'string') items = JSON.parse(items)
  if (items == null || items === '') return []
  if (
    !Array.isArray(items) ||
    items.some(
      (item) => !item || typeof item !== 'object' || Array.isArray(item)
    )
  ) {
    throw new Error('Payment configuration is unavailable')
  }
  return items as Record<string, unknown>[]
}

export function useTopupInfo() {
  const userId = useAuthStore((state) => state.auth.user?.id)
  const query = useQuery({
    queryKey: ['topup-info', userId],
    enabled: !!userId,
    meta: { errorToast: false },
    queryFn: async ({ signal }) => {
      const response = await getTopupInfo(signal)
      if (!isApiSuccess(response) || !response.data) {
        throw createServerError(
          response,
          'Payment configuration is unavailable'
        )
      }
      const data = response.data
      const checkout = checkoutInfoSchema.safeParse(data.checkout)
      if (!checkout.success) {
        throw new Error('Payment configuration is unavailable')
      }
      const methods: PaymentMethod[] = parseMethods(data.pay_methods)
        .filter(
          (method) =>
            typeof method.name === 'string' && typeof method.type === 'string'
        )
        .map((method) => ({
          name: method.name as string,
          type: method.type as string,
          icon: typeof method.icon === 'string' ? method.icon : undefined,
        }))
      const waffoMethods: WaffoPayMethod[] = parseMethods(
        data.waffo_pay_methods
      ).map((method) => {
        if (typeof method.name !== 'string' || !method.name.trim()) {
          throw new Error('Payment configuration is unavailable')
        }
        return {
          name: method.name,
          icon: typeof method.icon === 'string' ? method.icon : undefined,
          payMethodType:
            typeof method.payMethodType === 'string'
              ? method.payMethodType
              : undefined,
          payMethodName:
            typeof method.payMethodName === 'string'
              ? method.payMethodName
              : undefined,
        }
      })
      return {
        ...data,
        checkout: checkout.data,
        pay_methods: methods,
        waffo_pay_methods: waffoMethods,
      }
    },
  })
  return {
    topupInfo: query.isError ? null : (query.data ?? null),
    loading: query.isPending,
    error: query.error,
    refetch: query.refetch,
  }
}
