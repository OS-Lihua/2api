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

const amountSchema = z.object({
  nanos: z
    .string()
    .refine(
      (value) =>
        /^\d+$/.test(value) &&
        BigInt(value) > 0n &&
        BigInt(value) <= 9_223_372_036_854_775_807n
    ),
  currency: z.string().regex(/^[A-Z]{3}$/),
})

export const checkoutInfoSchema = z.object({
  providers: z.array(
    z
      .object({
        provider: z.enum(['epay', 'stripe', 'creem', 'waffo', 'waffo_pancake']),
        configured: z.boolean(),
        enabled: z.boolean(),
        selection_kind: z.enum(['amount', 'product']),
        minimum: amountSchema.optional(),
        presets: z.array(amountSchema),
        products: z.array(
          z.object({
            product_id: z.string().min(1),
            name: z.string().min(1),
            source_amount: amountSchema,
          })
        ),
      })
      .superRefine((provider, ctx) => {
        if (!provider.configured) return
        if (provider.selection_kind === 'amount') {
          if (
            !provider.minimum ||
            provider.presets.some(
              (amount) => amount.currency !== provider.minimum?.currency
            )
          ) {
            ctx.addIssue({
              code: 'custom',
              message: 'Payment configuration is unavailable',
            })
          }
        } else if (provider.products.length === 0) {
          ctx.addIssue({
            code: 'custom',
            message: 'Payment configuration is unavailable',
          })
        }
      })
  ),
})

export const checkoutQuoteSchema = z
  .object({
    quote_id: z.string().min(1),
    expires_at: z.number().int().positive().max(Number.MAX_SAFE_INTEGER),
    charge_amount: amountSchema,
    charge_final: z.boolean(),
    source_amount: amountSchema,
    credit_amount: amountSchema,
    fx: z
      .object({
        from_currency: z.string().regex(/^[A-Z]{3}$/),
        to_currency: z.string().regex(/^[A-Z]{3}$/),
        rate: z
          .string()
          .refine(
            (value) => /^\d+(?:\.\d+)?$/.test(value) && /[1-9]/.test(value)
          ),
      })
      .nullable(),
  })
  .superRefine((quote, ctx) => {
    if (
      !/^\d+$/.test(quote.source_amount.nanos) ||
      !/^\d+$/.test(quote.credit_amount.nanos)
    ) {
      return
    }
    if (quote.source_amount.currency === quote.credit_amount.currency) {
      if (
        quote.fx !== null ||
        BigInt(quote.source_amount.nanos) !== BigInt(quote.credit_amount.nanos)
      ) {
        ctx.addIssue({ code: 'custom', message: 'Quote unavailable' })
      }
    } else if (
      !quote.fx ||
      quote.fx.from_currency !== quote.source_amount.currency ||
      quote.fx.to_currency !== quote.credit_amount.currency
    ) {
      ctx.addIssue({ code: 'custom', message: 'Quote unavailable' })
    }
  })
