import { z } from 'zod'

import { hasPermission } from '@/lib/admin-permissions'
import { parseDecimalMoneyAmount } from '@/lib/currency'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

const currency = z.string().regex(/^[A-Z]{3}$/)
const storedAmount = z.object({
  nanos: z
    .string()
    .regex(/^(0|[1-9]\d*)$/)
    .refine((value) => BigInt(value) <= 9223372036854775807n),
  currency,
})
// A read-only ledger aggregate is signed and is not bounded by entry int64 storage.
export const moneyTotalSchema = z.object({
  nanos: z.string().regex(/^-?(0|[1-9]\d*)$/),
  currency,
})
export const moneyAmountSchema = moneyTotalSchema.refine(
  (value) =>
    /^-?(0|[1-9]\d*)$/.test(value.nanos) &&
    BigInt(value.nanos) >= -9223372036854775808n &&
    BigInt(value.nanos) <= 9223372036854775807n
)

export const walletSummarySchema = z.object({
  user_id: z.number().int().positive(),
  available: storedAmount,
  outstanding: storedAmount,
  consumed: moneyTotalSchema,
  sale_pending: z.boolean(),
  funding_pending: z.boolean().optional(),
  requests_paused: z.boolean(),
})
export type WalletSummary = z.infer<typeof walletSummarySchema>

export const selfWalletSummarySchema = z
  .object({
    user_id: z.number().int().positive(),
    currency,
    balance: z.string(),
    outstanding: z.string().optional(),
    consumed: moneyTotalSchema,
    sale_pending: z.boolean(),
    funding_pending: z.boolean().optional(),
    requests_paused: z.boolean(),
  })
  .transform((value, context) => {
    const available = parseDecimalMoneyAmount(value.balance, value.currency)
    const outstanding = parseDecimalMoneyAmount(
      value.outstanding ?? '0',
      value.currency
    )
    if (
      !available ||
      !outstanding ||
      BigInt(available.nanos) < 0n ||
      BigInt(outstanding.nanos) < 0n
    ) {
      context.addIssue({ code: 'custom', message: 'Invalid wallet balance' })
      return z.NEVER
    }
    return { ...value, available, outstanding }
  })

export function walletReadScope(resource: 'ledger' | 'wallet') {
  const { user, session } = useAuthStore.getState().auth
  return {
    userId: user?.id,
    sessionId: session?.sid ?? null,
    allowed:
      hasPermission(user, resource, 'read') &&
      (resource === 'wallet' || (user?.role ?? 0) >= ROLE.ADMIN),
  }
}
export type WalletReadScope = ReturnType<typeof walletReadScope>

export function isWalletScopeCurrent(
  scope: WalletReadScope,
  resource: 'ledger' | 'wallet'
) {
  const current = walletReadScope(resource)
  return (
    scope.allowed &&
    current.allowed &&
    scope.userId === current.userId &&
    scope.sessionId === current.sessionId
  )
}
