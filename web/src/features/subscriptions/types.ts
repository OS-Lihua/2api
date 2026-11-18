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

import { moneyAmountSchema } from '@/lib/wallet-summary'

export const subscriptionAmountSchema = moneyAmountSchema.refine((value) =>
  /^\d+$/.test(value.nanos)
)

// ============================================================================
// Subscription Plan Schema & Types
// ============================================================================

export const subscriptionPlanSchema = z.object({
  id: z.number(),
  purchase_revision: z.string().min(1),
  title: z.string(),
  subtitle: z.string().optional(),
  price: subscriptionAmountSchema,
  allowance: subscriptionAmountSchema,
  unlimited: z.boolean(),
  duration_unit: z.enum(['year', 'month', 'day', 'hour', 'custom']),
  duration_value: z.number(),
  custom_seconds: z.number().optional(),
  quota_reset_period: z.enum(['never', 'daily', 'weekly', 'monthly', 'custom']),
  quota_reset_custom_seconds: z.number().optional(),
  enabled: z.boolean(),
  sort_order: z.number(),
  allow_balance_pay: z.boolean(),
  allow_wallet_overflow: z.boolean(),
  max_purchase_per_user: z.number(),
  upgrade_group: z.string().optional(),
  downgrade_group: z.string().optional(),
  stripe_price_id: z.string().optional(),
  creem_product_id: z.string().optional(),
  waffo_pancake_product_id: z.string().optional(),
})

export type SubscriptionPlan = z.infer<typeof subscriptionPlanSchema>

export interface PlanRecord {
  plan: SubscriptionPlan
}

// ============================================================================
// User Subscription Schema & Types
// ============================================================================

export const userSubscriptionSchema = z.object({
  id: z.number(),
  user_id: z.number(),
  plan_id: z.number(),
  status: z.string(),
  source: z.string().optional(),
  start_time: z.number(),
  end_time: z.number(),
  allowance: subscriptionAmountSchema,
  unlimited: z.boolean(),
  allow_wallet_overflow: z.boolean(),
  next_reset_time: z.number().optional(),
})

export const subscriptionPeriodSchema = z
  .object({
    period_id: z.number().int().positive(),
    sequence: z.number().int().nonnegative(),
    allowance: subscriptionAmountSchema,
    consumed: moneyAmountSchema,
    available: moneyAmountSchema,
    reserved: moneyAmountSchema,
    unlimited: z.boolean(),
  })
  .refine((period) =>
    [period.consumed, period.available, period.reserved].every(
      (amount) => amount.currency === period.allowance.currency
    )
  )

export type SubscriptionPeriod = z.infer<typeof subscriptionPeriodSchema>
export type UserSubscription = z.infer<typeof userSubscriptionSchema>

export const userSubscriptionRecordSchema = z.object({
  subscription: userSubscriptionSchema,
  period: subscriptionPeriodSchema,
  legacy_reconciliation_pending: z.boolean().optional(),
})
export type UserSubscriptionRecord = z.infer<
  typeof userSubscriptionRecordSchema
>

// ============================================================================
// API Request/Response Types
// ============================================================================

export interface ApiResponse<T = unknown> {
  success: boolean
  message?: string
  data?: T
}

export interface PlanPayload {
  plan: Partial<SubscriptionPlan>
}

export interface SubscriptionPayRequest {
  plan_id: number
  payment_method?: string
}

export interface SubscriptionPayResponse {
  success: boolean
  message?: string
  data?: {
    // Stripe-style hosted checkout link.
    pay_link?: string
    // Waffo Pancake / Creem hosted checkout URL.
    checkout_url?: string
    // Pancake-only: order metadata + self-service buyer session token,
    // surfaced for future flows (refund / cancel from new-api's own UI).
    session_id?: string
    expires_at?: number | string
    order_id?: string
    token?: string
    token_expires_at?: number | string
  }
  url?: string
}

export interface CreateUserSubscriptionRequest {
  plan_id: number
}

export interface ResetUserSubscriptionsRequest {
  plan_id: number
  advance_reset_time: boolean
}

export interface ResetPlanSubscriptionsRequest {
  advance_reset_time: boolean
}

export interface SubscriptionResetResult {
  plan_id: number
  matched_count: number
  reset_count: number
  user_count: number
  advance_reset_time: boolean
}

// ============================================================================
// Self Subscription Data (user-facing)
// ============================================================================

export const billingPreferenceSchema = z.enum([
  'subscription_first',
  'wallet_first',
  'subscription_only',
  'wallet_only',
])
export type BillingPreference = z.infer<typeof billingPreferenceSchema>
export const selfSubscriptionDataSchema = z.object({
  billing_preference: billingPreferenceSchema,
  subscriptions: z.array(userSubscriptionRecordSchema),
  all_subscriptions: z.array(userSubscriptionRecordSchema),
})
export type SelfSubscriptionData = z.infer<typeof selfSubscriptionDataSchema>

export const planRecordSchema = z.object({ plan: subscriptionPlanSchema })
export const planPayloadSchema = z
  .object({
    plan: subscriptionPlanSchema
      .omit({ id: true, purchase_revision: true })
      .strict(),
  })
  .strict()
export const balancePurchaseRequestSchema = z
  .object({
    plan_id: z.number().int().positive(),
    purchase_id: z.string().uuid(),
    expected_revision: z.string().min(1),
  })
  .strict()
export type BalancePurchaseRequest = z.infer<
  typeof balancePurchaseRequestSchema
>
export const balancePurchaseResultSchema = userSubscriptionRecordSchema
  .extend({
    order_id: z.string().min(1),
    purchase_revision: z.string().min(1),
    contract_price: subscriptionAmountSchema,
    wallet_charge: subscriptionAmountSchema,
    fx_rate: z.null(),
  })
  .refine(
    (result) =>
      result.contract_price.currency === result.wallet_charge.currency &&
      result.contract_price.nanos === result.wallet_charge.nanos
  )
export type BalancePurchaseResult = z.infer<typeof balancePurchaseResultSchema>

// ============================================================================
// Dialog Types
// ============================================================================

export type SubscriptionsDialogType =
  | 'create'
  | 'update'
  | 'toggle-status'
  | 'reset-subscriptions'
