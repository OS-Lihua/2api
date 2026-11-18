import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  act,
  cleanup,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { AxiosError, AxiosHeaders } from 'axios'
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { SubscriptionPurchaseDialog } from '../components/dialogs/subscription-purchase-dialog'
import { balancePurchaseRequestSchema } from '../types'
import { plan, subscriptionRecord } from './fixtures'

const i18n = createInstance()
await i18n.init({
  lng: 'en',
  resources: { en: { translation: {} } },
  initAsync: false,
})
const receipt = {
  success: true,
  data: {
    order_id: 'sub-order-1',
    purchase_revision: plan.purchase_revision,
    ...subscriptionRecord,
    contract_price: plan.price,
    wallet_charge: plan.price,
    fx_rate: null,
  },
}

function conflict(code: string, accepted?: boolean) {
  return new AxiosError('Conflict', 'ERR_BAD_REQUEST', undefined, undefined, {
    status: 409,
    statusText: 'Conflict',
    headers: {},
    config: { headers: new AxiosHeaders() },
    data: { success: false, code, accepted },
  })
}

function session(sid: string) {
  return {
    sid,
    current: true,
    login_method: 'password',
    ip: '',
    user_agent: '',
    created_at: 1,
    last_active_at: 1,
    expires_at: 2000000000,
  }
}

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
  useAuthStore.getState().auth.reset()
})

function setup(currency = 'USD', selectedPlan = plan) {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'buyer', role: 100 })
  useAuthStore.setState((state) => ({
    auth: { ...state.auth, session: session('subscription-session') },
  }))
  const get = vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/user/self/wallet') {
      return {
        data: {
          success: true,
          data: {
            user_id: 1,
            currency,
            balance: '9007199.254740993',
            outstanding: '0',
            consumed: { nanos: '0', currency },
            sale_pending: false,
            funding_pending: false,
            requests_paused: false,
            updated_at: 1,
          },
        },
      }
    }
    throw new Error(`Unexpected GET ${url}`)
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  client.setQueryData(['admin-user-wallets', 1], 'old wallet')
  client.setQueryData(['self-subscriptions', 1], 'old entitlement')
  const close = vi.fn()
  const success = vi.fn()
  const view = render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <SubscriptionPurchaseDialog
          open
          onOpenChange={close}
          onPurchaseSuccess={success}
          plan={{ plan: selectedPlan }}
          enableStripe
        />
      </QueryClientProvider>
    </I18nextProvider>
  )
  return { get, client, close, success, ...view }
}

it('confirms the exact wallet price, keeps one purchase identity after timeout, and refreshes wallet and entitlement on success', async () => {
  const post = vi
    .spyOn(api, 'post')
    .mockRejectedValueOnce(new Error('Network timeout'))
    .mockResolvedValueOnce({ data: receipt })
  const { client, close, success } = setup()
  const user = userEvent.setup()
  const pay = await screen.findByRole('button', { name: 'Pay with wallet' })
  await waitFor(() => expect(pay).toBeEnabled())
  expect(screen.getAllByText('9,007,199.254740993 USD').length).toBeGreaterThan(
    0
  )
  expect(screen.getByText('9,223,372,036.854775807 EUR')).toBeVisible()
  await user.click(pay)
  expect(post).not.toHaveBeenCalled()
  const confirm = screen.getByRole('alertdialog', {
    name: 'Confirm wallet purchase',
  })
  expect(within(confirm).getByText('9,007,199.254740993 USD')).toBeVisible()
  expect(within(confirm).getByText('9,223,372,036.854775807 EUR')).toBeVisible()
  expect(within(confirm).getByText('paid')).toBeVisible()
  expect(within(confirm).getByText('default')).toBeVisible()
  await user.click(
    within(confirm).getByRole('button', { name: 'Confirm purchase' })
  )
  await waitFor(() => expect(post).toHaveBeenCalledTimes(1))
  expect(close).not.toHaveBeenCalled()
  await waitFor(() =>
    expect(
      within(confirm).getByRole('button', { name: 'Confirm purchase' })
    ).toBeEnabled()
  )
  expect(post.mock.calls[0]).toEqual([
    '/api/subscription/balance/pay',
    {
      plan_id: 7,
      purchase_id: expect.stringMatching(/^[0-9a-f-]{36}$/),
      expected_revision: plan.purchase_revision,
    },
  ])
  await user.click(
    within(confirm).getByRole('button', { name: 'Confirm purchase' })
  )
  await waitFor(() => expect(success).toHaveBeenCalledOnce())
  expect(post.mock.calls[1]).toEqual(post.mock.calls[0])
  expect(client.getQueryState(['admin-user-wallets', 1])?.isInvalidated).toBe(
    true
  )
  expect(client.getQueryState(['self-subscriptions', 1])?.isInvalidated).toBe(
    true
  )
  expect(close).toHaveBeenCalledWith(false)
})

it('retains the unresolved purchase identity and displayed contract price after the dialog is reopened', async () => {
  const post = vi
    .spyOn(api, 'post')
    .mockRejectedValueOnce(new Error('Timeout'))
    .mockResolvedValue({ data: receipt })
  const { client, rerender, close, success } = setup()
  const user = userEvent.setup()
  const pay = await screen.findByRole('button', { name: 'Pay with wallet' })
  await waitFor(() => expect(pay).toBeEnabled())
  await user.click(pay)
  await user.click(screen.getByRole('button', { name: 'Confirm purchase' }))
  await waitFor(() => expect(post).toHaveBeenCalledOnce())
  await waitFor(() =>
    expect(
      screen.getByRole('button', { name: 'Confirm purchase' })
    ).toBeEnabled()
  )
  const show = (open: boolean) => (
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <SubscriptionPurchaseDialog
          open={open}
          onOpenChange={close}
          onPurchaseSuccess={success}
          plan={{
            plan: {
              ...plan,
              title: 'Changed terms',
              purchase_revision: `v1:${'b'.repeat(64)}`,
              price: { nanos: '1', currency: 'USD' },
              allowance: { nanos: '2', currency: 'JPY' },
              unlimited: true,
              duration_unit: 'year',
              upgrade_group: 'new-group',
              downgrade_group: 'other-group',
              allow_wallet_overflow: false,
            },
          }}
          enableStripe
        />
      </QueryClientProvider>
    </I18nextProvider>
  )
  rerender(show(false))
  rerender(show(true))
  expect(
    await screen.findByText(
      'An earlier wallet purchase has not been confirmed. Retry it before starting another payment.'
    )
  ).toBeVisible()
  expect(screen.getByRole('button', { name: 'Stripe' })).toBeDisabled()
  expect(screen.queryByText('0.000000001 USD')).not.toBeInTheDocument()
  expect(screen.queryByText('Changed terms')).not.toBeInTheDocument()
  expect(screen.queryByText('Unlimited')).not.toBeInTheDocument()
  expect(screen.getByText('9,223,372,036.854775807 EUR')).toBeVisible()
  expect(screen.getByText('paid')).toBeVisible()
  expect(screen.getByText('default')).toBeVisible()
  const confirm = screen.queryByRole('button', { name: 'Confirm purchase' })
  if (!confirm) {
    await user.click(screen.getByRole('button', { name: 'Pay with wallet' }))
  }
  await user.click(screen.getByRole('button', { name: 'Confirm purchase' }))
  await waitFor(() => expect(success).toHaveBeenCalledOnce())
  expect(post.mock.calls[1]).toEqual(post.mock.calls[0])
})

it('rejects cross-currency wallet purchase without disabling external payment methods or guessing FX', async () => {
  const post = vi.spyOn(api, 'post')
  setup('EUR')
  expect(
    await screen.findByText(
      'Wallet purchases require the plan price and wallet to use the same currency.'
    )
  ).toBeVisible()
  expect(screen.getByRole('button', { name: 'Pay with wallet' })).toBeDisabled()
  expect(screen.getByRole('button', { name: 'Stripe' })).toBeEnabled()
  expect(post).not.toHaveBeenCalled()
})

it('keeps finite zero distinct from an unlimited entitlement', async () => {
  setup('USD', {
    ...plan,
    allowance: { nanos: '0', currency: 'JPY' },
    unlimited: false,
  })
  expect(await screen.findByText('0 JPY')).toBeVisible()
  expect(screen.queryByText('Unlimited')).not.toBeInTheDocument()
})

it('displays unlimited explicitly without inventing an entitlement balance', async () => {
  setup('USD', {
    ...plan,
    allowance: { nanos: '0', currency: 'JPY' },
    unlimited: true,
  })
  expect(await screen.findByText('Unlimited')).toBeVisible()
  expect(screen.queryByText('0 JPY')).not.toBeInTheDocument()
})

it.each(['session', 'account'])(
  'ignores a successful purchase response from a superseded %s',
  async (change) => {
    let resolve: (value: { data: typeof receipt }) => void = () => undefined
    const pending = new Promise<{ data: typeof receipt }>((complete) => {
      resolve = complete
    })
    const post = vi.spyOn(api, 'post').mockReturnValue(pending)
    const { close, success, client } = setup()
    const user = userEvent.setup()
    const pay = await screen.findByRole('button', { name: 'Pay with wallet' })
    await waitFor(() => expect(pay).toBeEnabled())
    await user.click(pay)
    await user.click(screen.getByRole('button', { name: 'Confirm purchase' }))
    await waitFor(() => expect(post).toHaveBeenCalledOnce())
    act(() => {
      if (change === 'session') {
        useAuthStore.setState((state) => ({
          auth: { ...state.auth, session: session('new-session') },
        }))
      } else {
        useAuthStore
          .getState()
          .auth.setUser({ id: 2, username: 'next', role: 100 })
      }
    })
    await act(async () => resolve({ data: receipt }))
    expect(close).not.toHaveBeenCalled()
    expect(success).not.toHaveBeenCalled()
    expect(client.getQueryState(['admin-user-wallets', 1])?.isInvalidated).toBe(
      false
    )
  }
)

it('refreshes only an explicitly unaccepted terms conflict and requires a new confirmation with a new identity', async () => {
  const changed = {
    ...plan,
    title: 'Changed plan',
    purchase_revision: `v1:${'b'.repeat(64)}`,
    price: { nanos: '2', currency: 'USD' },
    allowance: { nanos: '3', currency: 'JPY' },
  }
  const post = vi
    .spyOn(api, 'post')
    .mockRejectedValueOnce(
      conflict('subscription_purchase_terms_changed', false)
    )
    .mockResolvedValueOnce({
      data: {
        ...receipt,
        data: {
          ...receipt.data,
          purchase_revision: changed.purchase_revision,
          contract_price: changed.price,
          wallet_charge: changed.price,
        },
      },
    })
  const { get, success } = setup()
  const user = userEvent.setup()
  const pay = await screen.findByRole('button', { name: 'Pay with wallet' })
  await waitFor(() => expect(pay).toBeEnabled())
  get.mockResolvedValueOnce({
    data: { success: true, data: [{ plan: changed }] },
  })
  await user.click(pay)
  await user.click(screen.getByRole('button', { name: 'Confirm purchase' }))
  expect(
    await screen.findByText(
      'Subscription purchase terms have changed. Review the plan and confirm again.'
    )
  ).toBeVisible()
  expect(await screen.findByText('Changed plan')).toBeVisible()
  expect(get).toHaveBeenCalledWith('/api/subscription/plans', {
    signal: expect.any(AbortSignal),
  })
  expect(post).toHaveBeenCalledOnce()
  expect(success).not.toHaveBeenCalled()
  expect(
    screen.queryByRole('alertdialog', { name: 'Confirm wallet purchase' })
  ).not.toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Pay with wallet' }))
  const confirm = screen.getByRole('alertdialog', {
    name: 'Confirm wallet purchase',
  })
  expect(within(confirm).getByText('0.000000003 JPY')).toBeVisible()
  expect(within(confirm).getByText('0.000000002 USD')).toBeVisible()
  expect(post).toHaveBeenCalledOnce()
  await user.click(
    within(confirm).getByRole('button', { name: 'Confirm purchase' })
  )
  await waitFor(() => expect(success).toHaveBeenCalledOnce())
  expect(post.mock.calls[0]?.[1]).toMatchObject({
    expected_revision: plan.purchase_revision,
  })
  expect(post.mock.calls[1]?.[1]).toMatchObject({
    expected_revision: changed.purchase_revision,
  })
  expect(
    balancePurchaseRequestSchema.parse(post.mock.calls[1]?.[1]).purchase_id
  ).not.toBe(
    balancePurchaseRequestSchema.parse(post.mock.calls[0]?.[1]).purchase_id
  )
})

it.each([
  'subscription_purchase_revision_conflict',
  'subscription_purchase_terms_changed',
])(
  'retains the same identity and full terms for %s without an explicit non-acceptance guarantee',
  async (code) => {
    const post = vi.spyOn(api, 'post').mockRejectedValue(conflict(code))
    const { get } = setup()
    const user = userEvent.setup()
    const pay = await screen.findByRole('button', { name: 'Pay with wallet' })
    await waitFor(() => expect(pay).toBeEnabled())
    await user.click(pay)
    const confirm = screen.getByRole('alertdialog', {
      name: 'Confirm wallet purchase',
    })
    await user.click(
      within(confirm).getByRole('button', { name: 'Confirm purchase' })
    )
    await waitFor(() => expect(post).toHaveBeenCalledOnce())
    await waitFor(() =>
      expect(
        within(confirm).getByRole('button', { name: 'Confirm purchase' })
      ).toBeEnabled()
    )
    expect(
      within(confirm).getByText('9,223,372,036.854775807 EUR')
    ).toBeVisible()
    await user.click(
      within(confirm).getByRole('button', { name: 'Confirm purchase' })
    )
    await waitFor(() => expect(post).toHaveBeenCalledTimes(2))
    expect(post.mock.calls[1]).toEqual(post.mock.calls[0])
    expect(
      get.mock.calls.some(([url]) => url === '/api/subscription/plans')
    ).toBe(false)
  }
)

it('keeps the first confirmation snapshot when the parent receives changed terms before any purchase request', async () => {
  const post = vi.spyOn(api, 'post').mockResolvedValue({ data: receipt })
  const { client, rerender, close, success } = setup()
  const user = userEvent.setup()
  const pay = await screen.findByRole('button', { name: 'Pay with wallet' })
  await waitFor(() => expect(pay).toBeEnabled())
  await user.click(pay)
  rerender(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <SubscriptionPurchaseDialog
          open
          onOpenChange={close}
          onPurchaseSuccess={success}
          plan={{
            plan: {
              ...plan,
              title: 'Changed before submission',
              purchase_revision: 'different-revision',
              allowance: { nanos: '0', currency: 'JPY' },
              price: { nanos: '0', currency: 'USD' },
            },
          }}
        />
      </QueryClientProvider>
    </I18nextProvider>
  )
  const confirm = screen.getByRole('alertdialog', {
    name: 'Confirm wallet purchase',
  })
  expect(within(confirm).getByText('9,007,199.254740993 USD')).toBeVisible()
  expect(within(confirm).getByText('9,223,372,036.854775807 EUR')).toBeVisible()
  expect(
    screen.queryByText('Changed before submission')
  ).not.toBeInTheDocument()
  expect(post).not.toHaveBeenCalled()
  await user.click(
    within(confirm).getByRole('button', { name: 'Confirm purchase' })
  )
  await waitFor(() => expect(success).toHaveBeenCalledOnce())
  expect(post.mock.calls[0]?.[1]).toMatchObject({
    expected_revision: plan.purchase_revision,
  })
})
