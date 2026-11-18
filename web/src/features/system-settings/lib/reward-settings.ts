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

import { moneyAmountToDecimal, parseDecimalMoneyAmount } from '@/lib/currency'

export const REWARD_FIELDS = ['signup', 'inviter', 'invitee'] as const

const amountSchema = z
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

export const rewardConfigSchema = z
  .object({
    signup: amountSchema,
    inviter: amountSchema,
    invitee: amountSchema,
  })
  .strict()

const rewardDraftSchema = z
  .object({
    amount: z.string(),
    currency: z.string().regex(/^[A-Z]{3}$/, 'Invalid currency'),
  })
  .superRefine((value, context) => {
    if (!/^[A-Z]{3}$/.test(value.currency)) return
    const amount = parseDecimalMoneyAmount(value.amount, value.currency)
    if (!amount || BigInt(amount.nanos) < 0n) {
      context.addIssue({
        code: 'custom',
        path: ['amount'],
        message: 'Invalid amount',
      })
    }
  })

export const rewardSettingsSchema = z.object({
  signup: rewardDraftSchema,
  inviter: rewardDraftSchema,
  invitee: rewardDraftSchema,
})

export type RewardConfig = z.infer<typeof rewardConfigSchema>
export type RewardSettingsValues = z.infer<typeof rewardSettingsSchema>

export function parseRewardConfig(source: string): RewardConfig | null {
  try {
    // JSON.parse validates syntax and the schema validates the complete shape.
    // Scan validated JSON only to detect keys that JSON.parse would overwrite.
    const result = rewardConfigSchema.safeParse(JSON.parse(source))
    if (!result.success) return null
    const objects: Set<string>[] = []
    for (let index = 0; index < source.length; index += 1) {
      const character = source[index]
      if (character === '{') {
        objects.push(new Set())
      } else if (character === '}') {
        objects.pop()
      } else if (character === '"') {
        const start = index
        index += 1
        while (index < source.length && source[index] !== '"') {
          index += source[index] === '\\' ? 2 : 1
        }
        let next = index + 1
        while (/\s/.test(source[next] ?? '')) next += 1
        if (source[next] !== ':') continue
        const key: string = JSON.parse(source.slice(start, index + 1))
        const keys = objects.at(-1)
        if (!keys || keys.has(key)) return null
        keys.add(key)
      }
    }
    return result.data
  } catch {
    return null
  }
}

export function rewardConfigToForm(config: RewardConfig): RewardSettingsValues {
  return {
    signup: {
      amount: moneyAmountToDecimal(config.signup),
      currency: config.signup.currency,
    },
    inviter: {
      amount: moneyAmountToDecimal(config.inviter),
      currency: config.inviter.currency,
    },
    invitee: {
      amount: moneyAmountToDecimal(config.invitee),
      currency: config.invitee.currency,
    },
  }
}

export function rewardFormToConfig(values: RewardSettingsValues): RewardConfig {
  return rewardConfigSchema.parse({
    signup: parseDecimalMoneyAmount(
      values.signup.amount,
      values.signup.currency
    ),
    inviter: parseDecimalMoneyAmount(
      values.inviter.amount,
      values.inviter.currency
    ),
    invitee: parseDecimalMoneyAmount(
      values.invitee.amount,
      values.invitee.currency
    ),
  })
}
