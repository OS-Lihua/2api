import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { SubscriptionsMutateDrawer } from '../components/subscriptions-mutate-drawer'
import { SubscriptionsProvider } from '../components/subscriptions-provider'
import { plan } from './fixtures'

const i18n = createInstance()
await i18n.init({
  lng: 'en',
  resources: { en: { translation: {} } },
  initAsync: false,
})

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
  useAuthStore.getState().auth.reset()
})

function renderEditor() {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'root', role: 100 })
  vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/group') {
      return { data: { success: true, data: ['paid', 'default'] } }
    }
    if (url === '/api/option/waffo-pancake/subscription-product-options') {
      return {
        data: {
          success: true,
          message: 'success',
          data: { store_id: 'store', products: [] },
        },
      }
    }
    if (url === '/api/option/') return { data: { success: true, data: [] } }
    throw new Error(`Unexpected GET ${url}`)
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  const close = vi.fn()
  render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <SubscriptionsProvider>
          <SubscriptionsMutateDrawer
            open
            onOpenChange={close}
            currentRow={{ plan }}
          />
        </SubscriptionsProvider>
      </QueryClientProvider>
    </I18nextProvider>
  )
  return { close, client }
}

it('edits exact plan price and independent allowance without changing the commercial fields', async () => {
  const user = userEvent.setup()
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: true } })
  const { close } = renderEditor()
  const price = await screen.findByLabelText('Plan Price')
  expect(price).toHaveValue('9007199.254740993')
  expect(screen.getByLabelText('Entitlement limit')).toHaveValue(
    '9223372036.854775807'
  )
  await user.clear(price)
  await user.type(price, '0.000000009')
  await user.click(screen.getByRole('button', { name: 'Save changes' }))
  await waitFor(() =>
    expect(put).toHaveBeenCalledWith('/api/subscription/admin/plans/7', {
      plan: {
        title: plan.title,
        subtitle: plan.subtitle,
        price: { nanos: '9', currency: 'USD' },
        allowance: plan.allowance,
        unlimited: false,
        duration_unit: 'month',
        duration_value: 3,
        custom_seconds: 0,
        quota_reset_period: 'daily',
        quota_reset_custom_seconds: 0,
        enabled: true,
        sort_order: 4,
        allow_balance_pay: true,
        allow_wallet_overflow: false,
        max_purchase_per_user: 2,
        upgrade_group: 'paid',
        downgrade_group: 'default',
        stripe_price_id: 'price_existing',
        creem_product_id: 'prod_existing',
        waffo_pancake_product_id: 'PROD_existing',
      },
    })
  )
  expect(close).toHaveBeenCalledWith(false)
})

it.each(['-0.000000001', '0.0000000001', '9223372036.854775808', '1e3'])(
  'rejects invalid exact price %s without sending a rounded amount',
  async (value) => {
    const put = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    renderEditor()
    const user = userEvent.setup()
    const input = await screen.findByLabelText('Plan Price')
    await user.clear(input)
    await user.type(input, value)
    await user.click(screen.getByRole('button', { name: 'Save changes' }))
    expect(await screen.findByText('Invalid amount')).toBeVisible()
    expect(put).not.toHaveBeenCalled()
    expect(input).toHaveValue(value)
  }
)

it('preserves an exact rejected draft and retries without changing product IDs or currencies', async () => {
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValueOnce({ data: { success: false, message: 'Rejected' } })
    .mockResolvedValue({ data: { success: true } })
  const { close } = renderEditor()
  const user = userEvent.setup()
  const price = await screen.findByLabelText('Plan Price')
  await user.clear(price)
  await user.type(price, '0.000000001')
  await user.click(screen.getByRole('button', { name: 'Save changes' }))
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'Your changes are preserved'
  )
  expect(close).not.toHaveBeenCalled()
  expect(price).toHaveValue('0.000000001')
  await user.click(screen.getByRole('button', { name: 'Save changes' }))
  await waitFor(() => expect(put).toHaveBeenCalledTimes(2))
  expect(put.mock.calls[1]).toEqual(put.mock.calls[0])
  expect(close).toHaveBeenCalledWith(false)
})

it('keeps a zero finite entitlement distinct from explicitly unlimited', async () => {
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: true } })
  renderEditor()
  const user = userEvent.setup()
  const limit = await screen.findByLabelText('Entitlement limit')
  await user.clear(limit)
  await user.type(limit, '0')
  await user.click(screen.getByRole('button', { name: 'Save changes' }))
  await waitFor(() =>
    expect(put).toHaveBeenCalledWith(
      expect.any(String),
      expect.objectContaining({
        plan: expect.objectContaining({
          allowance: { nanos: '0', currency: 'EUR' },
          unlimited: false,
        }),
      })
    )
  )
  await user.click(
    screen.getByRole('switch', { name: 'Unlimited entitlement' })
  )
  await user.click(screen.getByRole('button', { name: 'Save changes' }))
  await waitFor(() =>
    expect(put).toHaveBeenLastCalledWith(
      expect.any(String),
      expect.objectContaining({
        plan: expect.objectContaining({
          allowance: { nanos: '0', currency: 'EUR' },
          unlimited: true,
        }),
      })
    )
  )
})
