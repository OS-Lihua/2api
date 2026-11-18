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
import { isAxiosError } from 'axios'
import { z } from 'zod'

import { api } from '@/lib/api'
import { requireServerSuccess } from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

import {
  balancePurchaseRequestSchema,
  balancePurchaseResultSchema,
  billingPreferenceSchema,
  planPayloadSchema,
  planRecordSchema,
  selfSubscriptionDataSchema,
  userSubscriptionRecordSchema,
  type BalancePurchaseRequest,
  type BalancePurchaseResult,
  type ApiResponse,
  type PlanRecord,
  type PlanPayload,
  type UserSubscriptionRecord,
  type CreateUserSubscriptionRequest,
  type ResetUserSubscriptionsRequest,
  type ResetPlanSubscriptionsRequest,
  type SubscriptionResetResult,
  type SubscriptionPayResponse,
  type SubscriptionPayRequest,
  type SelfSubscriptionData,
} from './types'

export function subscriptionScope() {
  const { user, session } = useAuthStore.getState().auth
  return { userId: user?.id, sessionId: session?.sid ?? null }
}
export function isSubscriptionScopeCurrent(
  scope: ReturnType<typeof subscriptionScope>
) {
  const current = subscriptionScope()
  return (
    current.userId === scope.userId && current.sessionId === scope.sessionId
  )
}
const responseSchema = <T extends z.ZodType>(data: T) =>
  z.object({
    success: z.literal(true),
    message: z.string().optional(),
    data,
  })
async function readSubscription<T extends z.ZodType>(
  path: string,
  schema: T,
  signal?: AbortSignal
) {
  const scope = subscriptionScope()
  const response = await api.get(path, { signal })
  if (signal?.aborted || !isSubscriptionScopeCurrent(scope)) {
    throw new Error('Session changed')
  }
  return responseSchema(schema).parse(requireServerSuccess(response.data))
}

// ============================================================================
// Admin Plan Management
// ============================================================================

export async function getAdminPlans(
  signal?: AbortSignal
): Promise<ApiResponse<PlanRecord[]>> {
  return readSubscription(
    '/api/subscription/admin/plans',
    z.array(planRecordSchema),
    signal
  )
}

export async function createPlan(
  data: PlanPayload
): Promise<ApiResponse<PlanRecord>> {
  const res = await api.post(
    '/api/subscription/admin/plans',
    planPayloadSchema.parse(data)
  )
  return res.data
}

export async function updatePlan(
  id: number,
  data: PlanPayload
): Promise<ApiResponse<PlanRecord>> {
  const res = await api.put(
    `/api/subscription/admin/plans/${id}`,
    planPayloadSchema.parse(data)
  )
  return res.data
}

export async function patchPlanStatus(
  id: number,
  enabled: boolean
): Promise<ApiResponse> {
  const res = await api.patch(`/api/subscription/admin/plans/${id}`, {
    enabled,
  })
  return res.data
}

// ============================================================================
// Admin User Subscription Management
// ============================================================================

export async function getUserSubscriptions(
  userId: number,
  signal?: AbortSignal
): Promise<ApiResponse<UserSubscriptionRecord[]>> {
  return readSubscription(
    `/api/subscription/admin/users/${userId}/subscriptions`,
    z.array(userSubscriptionRecordSchema),
    signal
  )
}

export async function createUserSubscription(
  userId: number,
  data: CreateUserSubscriptionRequest
): Promise<ApiResponse<{ message?: string }>> {
  const res = await api.post(
    `/api/subscription/admin/users/${userId}/subscriptions`,
    data
  )
  return res.data
}

export async function invalidateUserSubscription(
  subId: number
): Promise<ApiResponse<{ message?: string }>> {
  const res = await api.post(
    `/api/subscription/admin/user_subscriptions/${subId}/invalidate`
  )
  return res.data
}

export async function deleteUserSubscription(
  subId: number
): Promise<ApiResponse> {
  const res = await api.delete(
    `/api/subscription/admin/user_subscriptions/${subId}`
  )
  return res.data
}

export async function resetUserSubscriptionsByPlan(
  userId: number,
  data: ResetUserSubscriptionsRequest
): Promise<ApiResponse<SubscriptionResetResult>> {
  const res = await api.post(
    `/api/subscription/admin/users/${userId}/subscriptions/reset`,
    data
  )
  return res.data
}

export async function resetPlanSubscriptions(
  planId: number,
  data: ResetPlanSubscriptionsRequest
): Promise<ApiResponse<SubscriptionResetResult>> {
  const res = await api.post(
    `/api/subscription/admin/plans/${planId}/subscriptions/reset`,
    data
  )
  return res.data
}

// ============================================================================
// User-facing Subscription Payment
// ============================================================================

export async function paySubscriptionStripe(
  data: SubscriptionPayRequest
): Promise<SubscriptionPayResponse> {
  const res = await api.post('/api/subscription/stripe/pay', data)
  return res.data
}

export async function paySubscriptionCreem(
  data: SubscriptionPayRequest
): Promise<SubscriptionPayResponse> {
  const res = await api.post('/api/subscription/creem/pay', data)
  return res.data
}

export async function paySubscriptionWaffoPancake(
  data: SubscriptionPayRequest
): Promise<SubscriptionPayResponse> {
  const res = await api.post('/api/subscription/waffo-pancake/pay', data)
  return res.data
}

export class SubscriptionPurchaseTermsChanged extends Error {
  constructor() {
    super(
      'Subscription purchase terms have changed. Review the plan and confirm again.'
    )
    this.name = 'SubscriptionPurchaseTermsChanged'
  }
}

export async function paySubscriptionBalance(
  data: BalancePurchaseRequest
): Promise<ApiResponse<BalancePurchaseResult>> {
  const scope = subscriptionScope()
  const request = balancePurchaseRequestSchema.parse(data)
  try {
    const res = await api.post('/api/subscription/balance/pay', request)
    if (!isSubscriptionScopeCurrent(scope)) throw new Error('Session changed')
    const result = responseSchema(balancePurchaseResultSchema).parse(
      requireServerSuccess(res.data)
    )
    if (
      result.data.purchase_revision !== request.expected_revision ||
      result.data.subscription.plan_id !== request.plan_id
    ) {
      throw new Error('Payment request failed')
    }
    return result
  } catch (error) {
    if (!isSubscriptionScopeCurrent(scope)) throw new Error('Session changed')
    if (
      isAxiosError(error) &&
      error.response?.status === 409 &&
      error.response.data?.success === false &&
      error.response.data?.code === 'subscription_purchase_terms_changed' &&
      error.response.data?.accepted === false
    ) {
      throw new SubscriptionPurchaseTermsChanged()
    }
    throw error
  }
}

// Mints a Pancake OnetimeProduct (see controller for the OnetimeProduct vs
// SubscriptionProduct rationale) using persisted creds + StoreID.
export async function createWaffoPancakeSubscriptionProduct(data: {
  name: string
  amount: string
}): Promise<
  ApiResponse<{ product_id: string; product_name: string; store_id: string }>
> {
  const res = await api.post(
    '/api/option/waffo-pancake/subscription-product',
    data
  )
  return res.data
}

// Returns the OnetimeProducts in the saved Pancake store; empty when the
// gateway isn't fully configured.
export async function listWaffoPancakeSubscriptionProductOptions(): Promise<
  ApiResponse<{
    store_id: string
    products: { id: string; name: string; status: string }[]
  }>
> {
  const res = await api.get(
    '/api/option/waffo-pancake/subscription-product-options'
  )
  return res.data
}

export async function paySubscriptionEpay(
  data: SubscriptionPayRequest & { payment_method: string }
): Promise<SubscriptionPayResponse & { url?: string }> {
  const res = await api.post('/api/subscription/epay/pay', data)
  return {
    ...res.data,
    url: res.data.url || (res as unknown as { url?: string }).url,
  }
}

// ============================================================================
// User Self Subscriptions
// ============================================================================

export async function getSelfSubscriptions(
  signal?: AbortSignal
): Promise<ApiResponse<SelfSubscriptionData>> {
  return getSelfSubscriptionFull(signal)
}

export async function getSelfSubscriptionFull(
  signal?: AbortSignal
): Promise<ApiResponse<SelfSubscriptionData>> {
  return readSubscription(
    '/api/subscription/self',
    selfSubscriptionDataSchema,
    signal
  )
}

export async function getPublicPlans(
  signal?: AbortSignal
): Promise<ApiResponse<PlanRecord[]>> {
  return readSubscription(
    '/api/subscription/plans',
    z.array(planRecordSchema),
    signal
  )
}

export async function updateBillingPreference(
  preference: string
): Promise<ApiResponse<{ billing_preference?: string }>> {
  const res = await api.put('/api/subscription/self/preference', {
    billing_preference: billingPreferenceSchema.parse(preference),
  })
  return res.data
}

export async function getGroups(): Promise<ApiResponse<string[]>> {
  const res = await api.get('/api/group')
  return res.data
}
