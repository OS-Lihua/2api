import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, expect, it, vi } from 'vitest'

import { SubscriptionPlansCard } from '@/features/wallet/components/subscription-plans-card'
import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { plan, subscriptionRecord } from './fixtures'

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

function renderCard() {
  useAuthStore
    .getState()
    .auth.setUser({ id: 1, username: 'subscriber', role: 100 })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <SubscriptionPlansCard topupInfo={null} />
      </QueryClientProvider>
    </I18nextProvider>
  )
  return client
}
function selfData() {
  return {
    success: true,
    data: {
      billing_preference: 'subscription_only',
      subscriptions: [subscriptionRecord],
      all_subscriptions: [subscriptionRecord],
    },
  }
}

it('shows exact period allowance, consumption, reservations and available entitlement without quota conversion', async () => {
  vi.spyOn(api, 'get').mockImplementation(async (url) => ({
    data:
      url === '/api/subscription/plans'
        ? { success: true, data: [{ plan }] }
        : selfData(),
  }))
  renderCard()
  expect(await screen.findByText('9,007,199.254740993 USD')).toBeVisible()
  expect(screen.getByText('9,007,199.254740993 EUR')).toBeVisible()
  expect(screen.getByText('9,214,364,837.600034813 EUR')).toBeVisible()
  expect(screen.getByText('0.000000001 EUR')).toBeVisible()
  expect(screen.getByText('Reserved entitlement')).toBeVisible()
  expect(screen.queryByText('Raw Quota')).not.toBeInTheDocument()
})

it('shows a failed subscription read as retryable, not no active subscriptions or a free plan', async () => {
  const get = vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/subscription/plans') {
      return { data: { success: true, data: [{ plan }] } }
    }
    throw new Error('Read unavailable')
  })
  renderCard()
  const retry = await screen.findByRole('button', { name: 'Retry' })
  expect(screen.queryByText('No Active')).not.toBeInTheDocument()
  get.mockImplementation(async (url) => ({
    data:
      url === '/api/subscription/plans'
        ? { success: true, data: [{ plan }] }
        : selfData(),
  }))
  await userEvent.setup().click(retry)
  expect(await screen.findByText('9,007,199.254740993 EUR')).toBeVisible()
})

it('restores the saved billing preference after a rejected update', async () => {
  vi.spyOn(api, 'get').mockImplementation(async (url) => ({
    data:
      url === '/api/subscription/plans'
        ? { success: true, data: [{ plan }] }
        : selfData(),
  }))
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: false, message: 'Not saved' } })
  renderCard()
  const user = userEvent.setup()
  const control = await screen.findByRole('combobox', {
    name: 'Billing preference',
  })
  await user.click(control)
  await user.click(screen.getByRole('option', { name: 'Wallet Only' }))
  await waitFor(() =>
    expect(put).toHaveBeenCalledWith('/api/subscription/self/preference', {
      billing_preference: 'wallet_only',
    })
  )
  await waitFor(() => expect(control).toHaveTextContent('Subscription Only'))
})

it.each([true, false])(
  'keeps legacy reconciliation separate from the current period balances (%s)',
  async (pending) => {
    const record = {
      ...subscriptionRecord,
      legacy_reconciliation_pending: pending,
    }
    vi.spyOn(api, 'get').mockImplementation(async (url) => ({
      data: {
        success: true,
        data:
          url === '/api/subscription/plans'
            ? [{ plan }]
            : {
                billing_preference: 'subscription_only',
                subscriptions: [record],
                all_subscriptions: [record],
              },
      },
    }))
    renderCard()
    expect(await screen.findByText('9,007,199.254740993 EUR')).toBeVisible()
    expect(screen.getByText('0.000000001 EUR')).toBeVisible()
    const message = screen.queryByText(
      'Historical usage may include unreconciled precharges. Recorded amounts are unchanged; no refund or transfer into this period is implied.'
    )
    if (pending) expect(message).toBeVisible()
    else expect(message).not.toBeInTheDocument()
    expect(
      screen.queryByText('Funding allocation pending')
    ).not.toBeInTheDocument()
  }
)
