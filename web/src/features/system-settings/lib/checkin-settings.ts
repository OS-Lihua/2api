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
import { z } from 'zod'

import { parseDecimalMoneyAmount } from '@/lib/currency'

const rewardAmountSchema = z
  .object({
    nanos: z
      .string()
      .refine(
        (value) =>
          /^\d+$/.test(value) && BigInt(value) <= 9_223_372_036_854_775_807n
      ),
    currency: z.string().regex(/^[A-Z]{3}$/),
  })
  .strict()

export const checkinRewardRangeSchema = z
  .object({
    min: rewardAmountSchema,
    max: rewardAmountSchema,
  })
  .strict()
  .refine(
    (range) =>
      range.min.currency === range.max.currency &&
      /^\d+$/.test(range.min.nanos) &&
      /^\d+$/.test(range.max.nanos) &&
      BigInt(range.min.nanos) <= BigInt(range.max.nanos)
  )

export const checkinSettingsSchema = z
  .object({
    enabled: z.boolean(),
    currency: z.string().regex(/^[A-Z]{3}$/, 'Invalid currency'),
    min: z.string(),
    max: z.string(),
  })
  .superRefine((values, context) => {
    if (!/^[A-Z]{3}$/.test(values.currency)) return
    const min = parseDecimalMoneyAmount(values.min, values.currency)
    const max = parseDecimalMoneyAmount(values.max, values.currency)
    if (!min || BigInt(min.nanos) < 0n) {
      context.addIssue({
        code: 'custom',
        path: ['min'],
        message: 'Invalid amount',
      })
    }
    if (!max || BigInt(max.nanos) < 0n) {
      context.addIssue({
        code: 'custom',
        path: ['max'],
        message: 'Invalid amount',
      })
    }
    if (min && max && BigInt(min.nanos) > BigInt(max.nanos)) {
      context.addIssue({
        code: 'custom',
        path: ['max'],
        message: 'Maximum reward must be at least the minimum reward',
      })
    }
  })

export type CheckinSettingsValues = z.infer<typeof checkinSettingsSchema>
