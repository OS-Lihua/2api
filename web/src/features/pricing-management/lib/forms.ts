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

export function priceFormSchema(t: TFunction) {
  return z
    .object({
      owner_type: z.enum(['model_offering', 'channel']),
      owner_id: z
        .string()
        .regex(/^[1-9]\d*$/, t('Enter a positive owner ID.'))
        .refine(
          (value) => Number.isSafeInteger(Number(value)),
          t('Enter a positive owner ID.')
        ),
      currency: z
        .string()
        .regex(
          /^[A-Z]{3}$/,
          t('Enter a three-letter uppercase currency code.')
        ),
      expression: z
        .string()
        .refine(
          (value) => value.trim().length > 0,
          t('A billing expression is required.')
        ),
      activation: z.enum(['immediate', 'scheduled']),
      effective_at: z.date().optional(),
    })
    .refine(
      (values) =>
        values.activation !== 'scheduled' || values.effective_at !== undefined,
      {
        message: t('Select an activation date and time.'),
        path: ['effective_at'],
      }
    )
}
export type PriceFormValues = z.infer<ReturnType<typeof priceFormSchema>>

function isBoundedDecimal(
  value: string,
  maximum: bigint,
  positive: boolean
): boolean {
  if (!/^\d+(?:\.\d{1,18})?$/.test(value)) return false
  const [whole, fraction = ''] = value.split('.')
  const scaled = BigInt(whole + fraction.padEnd(18, '0'))
  return scaled >= (positive ? 1n : 0n) && scaled <= maximum * 10n ** 18n
}

export function fxFormSchema(t: TFunction) {
  return z
    .object({
      from_currency: z
        .string()
        .regex(
          /^[A-Z]{3}$/,
          t('Enter a three-letter uppercase currency code.')
        ),
      to_currency: z
        .string()
        .regex(
          /^[A-Z]{3}$/,
          t('Enter a three-letter uppercase currency code.')
        ),
      rate: z
        .string()
        .refine(
          (value) => isBoundedDecimal(value, 1_000_000_000n, true),
          t(
            'Enter a positive decimal rate up to 1,000,000,000 with at most 18 decimal places.'
          )
        ),
      buffer_rate: z
        .string()
        .refine(
          (value) => isBoundedDecimal(value, 1n, false),
          t(
            'Enter a decimal buffer from 0 to 1 with at most 18 decimal places.'
          )
        ),
    })
    .refine((values) => values.from_currency !== values.to_currency, {
      message: t('Choose two different currencies.'),
      path: ['to_currency'],
    })
}
export type FxFormValues = z.infer<ReturnType<typeof fxFormSchema>>
