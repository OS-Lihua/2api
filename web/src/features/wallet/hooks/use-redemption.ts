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
import { useState, useCallback } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { toIntlLocale } from '@/i18n/languages'
import { formatMoneyAmount } from '@/lib/currency'
import { handleServerError } from '@/lib/handle-server-error'

import { redeemTopupCode } from '../api'

// ============================================================================
// Redemption Hook
// ============================================================================

export function useRedemption() {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const [redeeming, setRedeeming] = useState(false)

  const redeemCode = useCallback(
    async (code: string): Promise<boolean> => {
      if (!code || code.trim() === '') {
        toast.error(t('Please enter a redemption code'))
        return false
      }

      try {
        setRedeeming(true)
        const response = await redeemTopupCode({ key: code })

        if (response.success && response.data) {
          toast.success(
            t('Redemption successful! Added: {{amount}}', {
              amount: formatMoneyAmount(response.data, locale),
            })
          )
          return true
        }

        handleServerError(response, t('Redemption failed'))
        return false
      } catch (_error) {
        handleServerError(_error, t('Redemption failed'))
        return false
      } finally {
        setRedeeming(false)
      }
    },
    [locale, t]
  )

  return {
    redeeming,
    redeemCode,
  }
}
