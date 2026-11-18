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
import { useMutation } from '@tanstack/react-query'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { useAuthStore } from '@/stores/auth-store'

import { requestCheckoutPayment } from '../api'
import { submitPaymentForm } from '../lib/payment'
import type { CheckoutProvider } from '../types'

export function usePayment(provider: CheckoutProvider, userId: number) {
  const { t } = useTranslation()
  return useMutation({
    meta: { errorToast: false },
    mutationFn: async (quoteId: string) => {
      if (useAuthStore.getState().auth.user?.id !== userId) return false
      const response = await requestCheckoutPayment(provider, quoteId)
      if (useAuthStore.getState().auth.user?.id !== userId) return false
      const data = response.data
      let url = response.url
      if (provider !== 'epay') {
        const field = provider === 'stripe' ? 'pay_link' : 'checkout_url'
        if (typeof data === 'string') url = data
        else if (data && typeof data === 'object') {
          const payload = data as Record<string, unknown>
          const target = payload[provider === 'waffo' ? 'payment_url' : field]
          if (typeof target === 'string') url = target
        }
      }
      let validUrl = false
      try {
        validUrl = !!url && ['http:', 'https:'].includes(new URL(url).protocol)
      } catch {
        /* Invalid redirect values remain non-navigable. */
      }
      if (!url || !validUrl) throw new Error(t('Invalid payment redirect URL'))
      if (provider === 'epay') {
        if (!data || typeof data !== 'object' || Array.isArray(data)) {
          throw new Error(t('Payment request failed'))
        }
        submitPaymentForm(url, data as Record<string, unknown>)
      } else window.location.assign(url)
      toast.success(t('Redirecting to payment page...'))
      return true
    },
  })
}
