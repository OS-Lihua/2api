import { afterEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import {
  getAdminPlans,
  getPublicPlans,
  getSelfSubscriptionFull,
  getUserSubscriptions,
  paySubscriptionBalance,
} from '../api'
import { plan, subscriptionRecord } from './fixtures'

afterEach(() => vi.restoreAllMocks())

it.each([getAdminPlans, getPublicPlans])(
  'rejects legacy or malformed monetary plan responses instead of treating them as free',
  async (read) => {
    vi.spyOn(api, 'get').mockResolvedValue({
      data: {
        success: true,
        data: [
          {
            plan: {
              ...plan,
              price: undefined,
              price_amount: 1.25,
              total_amount: 0,
              currency: 'USD',
            },
          },
        ],
      },
    })
    await expect(read()).rejects.toThrow()
  }
)

it.each([
  { ...plan.price, nanos: 9007199254740992 },
  { ...plan.price, nanos: '9223372036854775808' },
  { ...plan.price, currency: 'usd' },
  { ...plan.price, nanos: '-1' },
])('rejects an invalid exact price at the API boundary', async (price) => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: [{ plan: { ...plan, price } }] },
  })
  await expect(getPublicPlans()).rejects.toThrow()
})

it.each(['allow_balance_pay', 'allow_wallet_overflow', 'unlimited'])(
  'does not infer a missing financial permission or unlimited flag (%s)',
  async (field) => {
    vi.spyOn(api, 'get').mockResolvedValue({
      data: {
        success: true,
        data: [{ plan: { ...plan, [field]: undefined } }],
      },
    })
    await expect(getPublicPlans()).rejects.toThrow()
  }
)

it('retains every nanos digit and explicit finite zero when loading plans', async () => {
  const stored = {
    ...plan,
    allowance: { nanos: '0', currency: 'EUR' },
    unlimited: false,
  }
  const signal = new AbortController().signal
  const get = vi
    .spyOn(api, 'get')
    .mockResolvedValue({ data: { success: true, data: [{ plan: stored }] } })
  expect((await getPublicPlans(signal)).data).toEqual([{ plan: stored }])
  expect(get).toHaveBeenCalledWith('/api/subscription/plans', { signal })
})

it('loads the same period money snapshot through self and administrator subscription APIs', async () => {
  const signal = new AbortController().signal
  const get = vi.spyOn(api, 'get').mockImplementation(async (url) => ({
    data: {
      success: true,
      data:
        url === '/api/subscription/self'
          ? {
              billing_preference: 'subscription_only',
              subscriptions: [subscriptionRecord],
              all_subscriptions: [subscriptionRecord],
            }
          : [subscriptionRecord],
    },
  }))
  expect((await getSelfSubscriptionFull(signal)).data?.subscriptions).toEqual([
    subscriptionRecord,
  ])
  expect((await getUserSubscriptions(1, signal)).data).toEqual([
    subscriptionRecord,
  ])
  expect(get).toHaveBeenCalledWith(
    '/api/subscription/admin/users/1/subscriptions',
    { signal }
  )
})

it('rejects a missing or mismatched period instead of reading legacy used quota', async () => {
  const get = vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        billing_preference: 'wallet_only',
        subscriptions: [
          {
            subscription: {
              ...subscriptionRecord.subscription,
              amount_total: 100,
              amount_used: 0,
            },
          },
        ],
        all_subscriptions: [],
      },
    },
  })
  await expect(getSelfSubscriptionFull()).rejects.toThrow()
  get.mockResolvedValue({
    data: {
      success: true,
      data: {
        billing_preference: 'wallet_only',
        subscriptions: [
          {
            ...subscriptionRecord,
            period: {
              ...subscriptionRecord.period,
              available: { nanos: '1', currency: 'USD' },
            },
          },
        ],
        all_subscriptions: [],
      },
    },
  })
  await expect(getSelfSubscriptionFull()).rejects.toThrow()
})

it('validates balance purchase identity and preserves the server charge snapshot', async () => {
  const post = vi.spyOn(api, 'post').mockResolvedValue({
    data: {
      success: true,
      data: {
        order_id: 'subscription-order-1',
        purchase_revision: plan.purchase_revision,
        ...subscriptionRecord,
        contract_price: plan.price,
        wallet_charge: plan.price,
        fx_rate: null,
      },
    },
  })
  await expect(
    paySubscriptionBalance({
      plan_id: 7,
      purchase_id: 'not-a-uuid',
      expected_revision: plan.purchase_revision,
    })
  ).rejects.toThrow()
  expect(post).not.toHaveBeenCalled()
  const request = {
    plan_id: 7,
    purchase_id: 'e6ce38c9-2ba7-4698-9e89-4e891087ab1a',
    expected_revision: plan.purchase_revision,
  }
  const result = await paySubscriptionBalance(request)
  expect(post).toHaveBeenCalledExactlyOnceWith(
    '/api/subscription/balance/pay',
    request
  )
  expect(result.data?.purchase_revision).toBe(plan.purchase_revision)
  expect(result.data?.wallet_charge).toEqual(plan.price)
  expect(result.data?.period).toEqual(subscriptionRecord.period)
})
