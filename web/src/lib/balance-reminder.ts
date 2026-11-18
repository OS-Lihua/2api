import { z } from 'zod'

import { parseDecimalMoneyAmount } from '@/lib/currency'

const thresholdSchema = z
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

export const balanceReminderConfigSchema = z
  .object({
    enabled: z.boolean(),
    threshold: thresholdSchema,
  })
  .strict()

export const balanceReminderPreferenceSchema = z
  .object({
    enabled: z.boolean(),
    threshold: thresholdSchema.nullable(),
  })
  .strict()

export const userBalanceReminderSchema = z
  .object({
    enabled: z.boolean(),
    threshold: thresholdSchema.nullable(),
    effective_enabled: z.boolean(),
    effective_threshold: thresholdSchema,
  })
  .strict()
  .refine(
    (value) =>
      value.threshold === null ||
      value.threshold.currency === value.effective_threshold.currency
  )

export type BalanceReminderPreference = z.infer<
  typeof balanceReminderPreferenceSchema
>
export type UserBalanceReminder = z.infer<typeof userBalanceReminderSchema>

export const balanceReminderDraftSchema = z
  .object({
    enabled: z.boolean(),
    inherit: z.boolean(),
    amount: z.string(),
    currency: z.string().regex(/^[A-Z]{3}$/, 'Invalid currency'),
  })
  .superRefine((value, context) => {
    if (value.inherit) return
    const amount = parseDecimalMoneyAmount(value.amount, value.currency)
    if (!amount || BigInt(amount.nanos) < 0n) {
      context.addIssue({
        code: 'custom',
        path: ['amount'],
        message: 'Invalid amount',
      })
    }
  })

export type BalanceReminderDraft = z.infer<typeof balanceReminderDraftSchema>

export function balanceReminderFromDraft(
  value: BalanceReminderDraft
): BalanceReminderPreference {
  balanceReminderDraftSchema.parse(value)
  return balanceReminderPreferenceSchema.parse({
    enabled: value.enabled,
    threshold: value.inherit
      ? null
      : parseDecimalMoneyAmount(value.amount, value.currency),
  })
}

export function parseBalanceReminderConfig(source: string) {
  try {
    const result = balanceReminderConfigSchema.safeParse(JSON.parse(source))
    if (!result.success) return null
    // Reject duplicate JSON members instead of silently taking the last value.
    const objects: Set<string>[] = []
    for (let index = 0; index < source.length; index += 1) {
      const character = source[index]
      if (character === '{') objects.push(new Set())
      else if (character === '}') objects.pop()
      else if (character === '"') {
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
