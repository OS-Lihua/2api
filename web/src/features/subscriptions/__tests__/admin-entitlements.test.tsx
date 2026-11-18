import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { UserSubscriptionsDialog } from '../components/dialogs/user-subscriptions-dialog'
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
function show() {
  useAuthStore.getState().auth.setUser({ id: 99, role: 100, username: 'admin' })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <UserSubscriptionsDialog
          open
          onOpenChange={vi.fn()}
          user={{ id: 1, username: 'buyer' }}
        />
      </QueryClientProvider>
    </I18nextProvider>
  )
}
it('shows the current immutable period instead of legacy quota totals', async () => {
  vi.spyOn(api, 'get').mockImplementation(async (url) => ({
    data: {
      success: true,
      data:
        url === '/api/subscription/admin/plans'
          ? [{ plan }]
          : [subscriptionRecord],
    },
  }))
  show()
  expect(await screen.findByText('9,007,199.254740993 EUR')).toBeVisible()
  expect(screen.getByText('9,214,364,837.600034813 EUR')).toBeVisible()
  expect(screen.getByText('0.000000001 EUR')).toBeVisible()
  expect(screen.queryByText('Total Quota')).not.toBeInTheDocument()
})
it('keeps a failed admin read retryable instead of showing an empty successful history', async () => {
  const get = vi.spyOn(api, 'get').mockRejectedValue(new Error('Unavailable'))
  show()
  const retry = await screen.findByRole('button', { name: 'Retry' })
  expect(screen.queryByText('No subscription records')).not.toBeInTheDocument()
  get.mockImplementation(async (url) => ({
    data: {
      success: true,
      data:
        url === '/api/subscription/admin/plans'
          ? [{ plan }]
          : [subscriptionRecord],
    },
  }))
  await userEvent.setup().click(retry)
  expect(await screen.findByText('9,007,199.254740993 EUR')).toBeVisible()
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
        data: url === '/api/subscription/admin/plans' ? [{ plan }] : [record],
      },
    }))
    show()
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
