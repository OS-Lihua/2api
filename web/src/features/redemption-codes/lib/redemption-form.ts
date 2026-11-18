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
import type { TFunction } from 'i18next'
import { z } from 'zod'

import { moneyAmountToDecimal, parseDecimalMoneyAmount } from '@/lib/currency'

import {
  REDEMPTION_VALIDATION,
  getRedemptionFormErrorMessages,
} from '../constants'
import type { RedemptionFormData, Redemption } from '../types'

// ============================================================================
// Form Schema (use getRedemptionFormSchema(t) in components for i18n messages)
// ============================================================================

export function getRedemptionFormSchema(t: TFunction) {
  const msg = getRedemptionFormErrorMessages(t)
  return z.object({
    name: z
      .string()
      .min(REDEMPTION_VALIDATION.NAME_MIN_LENGTH, msg.NAME_LENGTH_INVALID)
      .max(REDEMPTION_VALIDATION.NAME_MAX_LENGTH, msg.NAME_LENGTH_INVALID),
    amount: z.string().refine((value) => {
      const amount = parseDecimalMoneyAmount(value, 'USD')
      return amount !== null && BigInt(amount.nanos) > 0n
    }, t('Invalid redemption amount')),
    currency: z
      .string()
      .regex(/^[A-Z]{3}$/, t('Enter a three-letter uppercase currency code.')),
    expired_time: z.date().nullable(),
    count: z
      .number()
      .min(REDEMPTION_VALIDATION.COUNT_MIN, msg.COUNT_INVALID)
      .max(REDEMPTION_VALIDATION.COUNT_MAX, msg.COUNT_INVALID)
      .optional(),
  })
}

export type RedemptionFormValues = {
  name: string
  amount: string
  currency: string
  expired_time: Date | null
  count?: number
}

// ============================================================================
// Form Defaults
// ============================================================================

export const REDEMPTION_FORM_DEFAULT_VALUES: RedemptionFormValues = {
  name: '',
  amount: '10',
  currency: 'USD',
  expired_time: null,
  count: 1,
}

// ============================================================================
// Form Data Transformation
// ============================================================================

/**
 * Transform form data to API payload
 */
export function transformFormDataToPayload(
  data: RedemptionFormValues
): RedemptionFormData {
  const amount = parseDecimalMoneyAmount(data.amount, data.currency)
  if (!amount || BigInt(amount.nanos) <= 0n) {
    throw new Error('Invalid redemption amount')
  }
  return {
    name: data.name,
    amount,
    expired_time: data.expired_time
      ? Math.floor(data.expired_time.getTime() / 1000)
      : 0,
    count: data.count || 1,
  }
}

/**
 * Transform redemption data to form defaults
 */
export function transformRedemptionToFormDefaults(
  redemption: Redemption
): RedemptionFormValues {
  return {
    name: redemption.name,
    amount: moneyAmountToDecimal(redemption.amount),
    currency: redemption.amount.currency,
    expired_time:
      redemption.expired_time > 0
        ? new Date(redemption.expired_time * 1000)
        : null,
    count: 1,
  }
}
