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
import {
  QueryClient,
  QueryClientProvider,
  useQuery,
} from '@tanstack/react-query'
import {
  act,
  cleanup,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import { getUserWallet, getUserWalletSummaries } from '@/features/users/api'
import { api } from '@/lib/api'
import { formatMoneyAmount } from '@/lib/currency'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

import { getSelfWallet } from '../api'
import { BillingHistoryDialog } from '../components/dialogs/billing-history-dialog'

let client: QueryClient
const checkout = {
  charge_amount: { nanos: '18750000001', currency: 'EUR' },
  charge_final: false,
  source_amount: { nanos: '9007199254740993', currency: 'USD' },
  credit_amount: { nanos: '63050394783186951', currency: 'CNY' },
  unit_charge_amount: { nanos: '18750000001', currency: 'EUR' },
  fx: { from_currency: 'USD', to_currency: 'CNY', rate: '7' },
  fx_frozen_at: 1,
  expires_at: 2_000_000_000,
  accepted_at: 1,
  creation_state: 'created',
  paid_amount: { nanos: '19750000001', currency: 'EUR' },
  paid_at: 2,
  fulfillment_pending: false,
}
const record = {
  id: 1,
  user_id: 1,
  trade_no: 'exact-order',
  payment_method: 'stripe',
  create_time: 1,
  status: 'success',
  contract_version: 1,
  purpose: 'wallet_topup',
  legacy_amount: null,
  legacy_money: null,
  checkout,
}

beforeEach(() => {
  client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  useAuthStore
    .getState()
    .auth.setUser({ id: 1, username: 'buyer', role: ROLE.USER })
})
afterEach(() => {
  cleanup()
  client.clear()
  useAuthStore.getState().auth.reset()
})
function WalletSummaryProbe() {
  const userId = useAuthStore((state) => state.auth.user?.id ?? 0)
  const sessionId = useAuthStore((state) => state.auth.session?.sid ?? null)
  const self = useQuery({
    queryKey: ['self-wallet', userId, sessionId, 'summary', true],
    queryFn: ({ signal }) => getSelfWallet(signal),
  })
  const batch = useQuery({
    queryKey: ['admin-user-wallets', userId, sessionId, true, [userId]],
    queryFn: ({ signal }) => getUserWalletSummaries([userId], signal),
  })
  const detail = useQuery({
    queryKey: ['admin-user-wallet', userId, sessionId, userId, true],
    queryFn: ({ signal }) => getUserWallet(userId, signal),
  })
  return (
    <>
      <output aria-label='Self wallet'>
        {self.data ? `${self.data.balance} ${self.data.currency}` : 'Loading'}
      </output>
      <output aria-label='Listed wallet'>
        {batch.data?.[0]
          ? formatMoneyAmount(batch.data[0].available, 'en-US')
          : 'Loading'}
      </output>
      <output aria-label='Wallet detail'>
        {detail.data?.data
          ? `${detail.data.data.balance} ${detail.data.data.currency}`
          : 'Loading'}
      </output>
    </>
  )
}

function renderHistory(withWallets = false) {
  return render(
    <QueryClientProvider client={client}>
      {withWallets && <WalletSummaryProbe />}
      <BillingHistoryDialog open onOpenChange={vi.fn()} />
    </QueryClientProvider>
  )
}

it('shows the recorded payment, reward and wallet currencies without losing nanos', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: { items: [record], total: 1 } },
  })
  renderHistory()
  await screen.findByText('exact-order')
  expect(screen.getByText('18.750000001 EUR')).toBeVisible()
  const order = within(screen.getByRole('article', { name: 'exact-order' }))
  expect(order.getByText('19.750000001 EUR')).toBeVisible()
  expect(order.getByText('9,007,199.254740993 USD')).toBeVisible()
  expect(order.getByText('63,050,394.783186951 CNY')).toBeVisible()
  expect(order.getByText('Payment base amount')).toBeVisible()
  expect(order.getByText('Actual payment')).toBeVisible()
})

it('keeps unconfirmed payment and paid-but-unfulfilled credit explicitly pending', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        items: [
          {
            ...record,
            id: 2,
            trade_no: 'unpaid-order',
            status: 'pending',
            checkout: { ...checkout, paid_amount: null, paid_at: 0 },
          },
          {
            ...record,
            id: 3,
            trade_no: 'paid-order',
            status: 'pending',
            checkout: { ...checkout, fulfillment_pending: true },
          },
        ],
        total: 2,
      },
    },
  })
  renderHistory()
  const unpaid = within(
    await screen.findByRole('article', { name: 'unpaid-order' })
  )
  expect(unpaid.getByText('Payment not confirmed')).toBeVisible()
  expect(unpaid.queryByText('0 EUR')).not.toBeInTheDocument()
  const paid = within(screen.getByRole('article', { name: 'paid-order' }))
  expect(paid.getByText('Wallet credit pending')).toBeVisible()
  expect(paid.getByText('19.750000001 EUR')).toBeVisible()
  expect(paid.queryByText('Success')).not.toBeInTheDocument()
})

it('shows legacy raw values and distinguishes subscription history without inventing currency', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        items: [
          {
            ...record,
            contract_version: 0,
            purpose: 'subscription_history',
            checkout: undefined,
            legacy_amount: '9007199254740993',
            legacy_money: '12.3400000001',
            amount: 0,
            money: 12.34,
          },
        ],
        total: 1,
      },
    },
  })
  renderHistory()
  const order = within(
    await screen.findByRole('article', { name: 'exact-order' })
  )
  expect(order.getByText('Subscription history')).toBeVisible()
  expect(order.getByText('9007199254740993')).toBeVisible()
  expect(order.getByText('12.3400000001')).toBeVisible()
  expect(
    order.getByText('Historical values have no recorded currency.')
  ).toBeVisible()
  expect(order.queryByText('Actual payment')).not.toBeInTheDocument()
  expect(order.queryByText(/USD|EUR|CNY/)).not.toBeInTheDocument()
})

it('only offers administrative completion for an accepted pending order', async () => {
  useAuthStore
    .getState()
    .auth.setUser({ id: 1, username: 'admin', role: ROLE.ADMIN })
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        items: [
          {
            ...record,
            id: 1,
            trade_no: 'quoted-order',
            status: 'quoted',
            checkout: { ...checkout, accepted_at: 0 },
          },
          { ...record, id: 2, trade_no: 'failed-order', status: 'failed' },
          { ...record, id: 3, trade_no: 'pending-order', status: 'pending' },
          {
            ...record,
            id: 4,
            trade_no: 'unpaid-order',
            status: 'pending',
            checkout: { ...checkout, paid_amount: null, paid_at: 0 },
          },
        ],
        total: 3,
      },
    },
  })
  renderHistory()
  const quoted = within(
    await screen.findByRole('article', { name: 'quoted-order' })
  )
  expect(quoted.getByText('Quoted')).toBeVisible()
  expect(
    quoted.queryByRole('button', { name: 'Complete Order' })
  ).not.toBeInTheDocument()
  const failed = within(screen.getByRole('article', { name: 'failed-order' }))
  expect(failed.getByText('Failed')).toBeVisible()
  expect(
    failed.queryByRole('button', { name: 'Complete Order' })
  ).not.toBeInTheDocument()
  expect(
    within(screen.getByRole('article', { name: 'unpaid-order' })).queryByRole(
      'button',
      { name: 'Complete Order' }
    )
  ).not.toBeInTheDocument()
  expect(
    within(screen.getByRole('article', { name: 'pending-order' })).getByRole(
      'button',
      { name: 'Complete Order' }
    )
  ).toBeVisible()
})

it('keeps a failed completion open for retry and refreshes the recorded status on success', async () => {
  useAuthStore
    .getState()
    .auth.setUser({ id: 1, username: 'admin', role: ROLE.ADMIN })
  vi.spyOn(api, 'get')
    .mockResolvedValueOnce({
      data: {
        success: true,
        data: { items: [{ ...record, status: 'pending' }], total: 1 },
      },
    })
    .mockResolvedValue({
      data: { success: true, data: { items: [record], total: 1 } },
    })
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValueOnce({
      data: { success: false, message: 'Completion unavailable' },
    })
    .mockResolvedValue({ data: { success: true } })
  const user = userEvent.setup()
  renderHistory()
  await user.click(
    await screen.findByRole('button', { name: 'Complete Order' })
  )
  const confirmation = await screen.findByRole('alertdialog', {
    name: 'Complete Order',
  })
  expect(confirmation).toHaveTextContent(
    'Complete the wallet credit for this confirmed payment. Outstanding debt is repaid first.'
  )
  await user.click(
    within(confirmation).getByRole('button', { name: 'Confirm' })
  )
  await waitFor(() =>
    expect(
      within(confirmation).getByRole('button', { name: 'Confirm' })
    ).toBeEnabled()
  )
  expect(confirmation).toBeVisible()
  expect(post).toHaveBeenLastCalledWith('/api/user/topup/complete', {
    trade_no: 'exact-order',
  })
  await user.click(
    within(confirmation).getByRole('button', { name: 'Confirm' })
  )
  await waitFor(() =>
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
  )
  expect(
    within(screen.getByRole('article', { name: 'exact-order' })).getByText(
      'Success'
    )
  ).toBeVisible()
})

function mockCompletionWalletReads(isCompleted: () => boolean) {
  return vi.spyOn(api, 'get').mockImplementation(async (path) => {
    const userId = useAuthStore.getState().auth.user?.id ?? 0
    let balance = '7'
    if (userId === 2) balance = '23'
    else if (isCompleted()) balance = '19'
    if (
      path === '/api/user/self/wallet' ||
      path === `/api/admin/v1/user-wallets/${userId}`
    ) {
      return {
        data: {
          success: true,
          data: {
            user_id: userId,
            balance,
            currency: 'USD',
            outstanding: '0',
            consumed: { nanos: '0', currency: 'USD' },
            sale_pending: false,
            requests_paused: false,
            updated_at: 1,
          },
        },
      }
    }
    if (path === '/api/admin/v1/user-wallets') {
      return {
        data: {
          success: true,
          data: {
            items: [
              {
                user_id: userId,
                available: { nanos: `${balance}000000000`, currency: 'USD' },
                outstanding: { nanos: '0', currency: 'USD' },
                consumed: { nanos: '0', currency: 'USD' },
                sale_pending: false,
                requests_paused: false,
              },
            ],
          },
        },
      }
    }
    return {
      data: {
        success: true,
        data: {
          items: [
            {
              ...record,
              user_id: userId,
              status: isCompleted() ? 'success' : 'pending',
              checkout: {
                ...checkout,
                source_amount: { nanos: '12000000000', currency: 'USD' },
                credit_amount: { nanos: '12000000000', currency: 'USD' },
                fx: null,
              },
            },
          ],
          total: 1,
        },
      },
    }
  })
}

it('refreshes mounted self and listed wallets only after order completion succeeds', async () => {
  useAuthStore
    .getState()
    .auth.setUser({ id: 1, username: 'admin', role: ROLE.ADMIN })
  let completed = false
  const get = mockCompletionWalletReads(() => completed)
  vi.spyOn(api, 'post')
    .mockResolvedValueOnce({
      data: { success: false, message: 'Completion unavailable' },
    })
    .mockImplementationOnce(async () => {
      completed = true
      return { data: { success: true } }
    })
  const user = userEvent.setup()
  renderHistory(true)
  await waitFor(() => {
    expect(screen.getByLabelText('Self wallet')).toHaveTextContent('7 USD')
    expect(screen.getByLabelText('Listed wallet')).toHaveTextContent('7 USD')
  })
  await user.click(
    await screen.findByRole('button', { name: 'Complete Order' })
  )
  const confirmation = await screen.findByRole('alertdialog', {
    name: 'Complete Order',
  })
  await user.click(
    within(confirmation).getByRole('button', { name: 'Confirm' })
  )
  await waitFor(() =>
    expect(
      within(confirmation).getByRole('button', { name: 'Confirm' })
    ).toBeEnabled()
  )
  expect(
    get.mock.calls.filter(([path]) => path === '/api/user/self/wallet')
  ).toHaveLength(1)
  expect(
    get.mock.calls.filter(([path]) => path === '/api/admin/v1/user-wallets')
  ).toHaveLength(1)
  expect(screen.getByLabelText('Self wallet')).toHaveTextContent('7 USD')
  expect(screen.getByLabelText('Listed wallet')).toHaveTextContent('7 USD')
  await user.click(
    within(confirmation).getByRole('button', { name: 'Confirm' })
  )
  await waitFor(() => {
    expect(screen.getByLabelText('Self wallet')).toHaveTextContent('19 USD')
    expect(screen.getByLabelText('Listed wallet')).toHaveTextContent('19 USD')
    expect(screen.getByLabelText('Wallet detail')).toHaveTextContent('19 USD')
  })
  expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
})

it('does not refresh a new account wallet when an old account completion arrives late', async () => {
  useAuthStore
    .getState()
    .auth.setUser({ id: 1, username: 'admin', role: ROLE.ADMIN })
  const get = mockCompletionWalletReads(() => false)
  let finish: ((value: unknown) => void) | undefined
  vi.spyOn(api, 'post').mockImplementationOnce(
    () =>
      new Promise((resolve) => {
        finish = resolve
      })
  )
  const user = userEvent.setup()
  renderHistory(true)
  await user.click(
    await screen.findByRole('button', { name: 'Complete Order' })
  )
  await user.click(
    within(
      await screen.findByRole('alertdialog', { name: 'Complete Order' })
    ).getByRole('button', { name: 'Confirm' })
  )
  await act(async () => {
    useAuthStore
      .getState()
      .auth.setUser({ id: 2, username: 'other-admin', role: ROLE.ADMIN })
  })
  await waitFor(() => {
    expect(screen.getByLabelText('Self wallet')).toHaveTextContent('23 USD')
    expect(screen.getByLabelText('Listed wallet')).toHaveTextContent('23 USD')
  })
  const requestCount = get.mock.calls.length
  await act(async () => {
    finish?.({ data: { success: true } })
  })
  expect(get).toHaveBeenCalledTimes(requestCount)
  expect(screen.getByLabelText('Self wallet')).toHaveTextContent('23 USD')
  expect(screen.getByLabelText('Listed wallet')).toHaveTextContent('23 USD')
})

it('clears an old confirmation and ignores completion after the same account signs in again', async () => {
  useAuthStore
    .getState()
    .auth.setUser({ id: 1, username: 'admin', role: ROLE.ADMIN })
  const get = mockCompletionWalletReads(() => false)
  let finish: ((value: unknown) => void) | undefined
  vi.spyOn(api, 'post').mockImplementationOnce(
    () =>
      new Promise((resolve) => {
        finish = resolve
      })
  )
  const user = userEvent.setup()
  renderHistory(true)
  await user.click(
    await screen.findByRole('button', { name: 'Complete Order' })
  )
  await user.click(
    within(
      await screen.findByRole('alertdialog', { name: 'Complete Order' })
    ).getByRole('button', { name: 'Confirm' })
  )
  await act(async () => {
    useAuthStore.getState().auth.setBundle({
      access_token: 'test-session-access',
      token_type: 'Bearer',
      access_expires_at: 2_000_000_000,
      user: { id: 1, username: 'admin', role: ROLE.ADMIN },
      session: {
        sid: 'replacement-session',
        current: true,
        login_method: 'password',
        ip: '',
        user_agent: '',
        created_at: 1,
        last_active_at: 1,
        expires_at: 2_000_000_000,
      },
    })
  })
  await waitFor(() => {
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    expect(screen.getByLabelText('Self wallet')).toHaveTextContent('7 USD')
    expect(screen.getByLabelText('Listed wallet')).toHaveTextContent('7 USD')
  })
  await screen.findByRole('article', { name: 'exact-order' })
  const requestCount = get.mock.calls.length
  await act(async () => {
    finish?.({ data: { success: true } })
  })
  expect(get).toHaveBeenCalledTimes(requestCount)
  expect(screen.getByLabelText('Self wallet')).toHaveTextContent('7 USD')
})

it('shows a retryable history error instead of an empty successful history', async () => {
  const get = vi
    .spyOn(api, 'get')
    .mockRejectedValueOnce(new Error('History unavailable'))
    .mockResolvedValue({
      data: { success: true, data: { items: [record], total: 1 } },
    })
  const user = userEvent.setup()
  renderHistory()
  expect(await screen.findByText('History unavailable')).toBeVisible()
  expect(screen.queryByText('No billing records found')).not.toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Retry' }))
  expect(
    await screen.findByRole('article', { name: 'exact-order' })
  ).toBeVisible()
  expect(get).toHaveBeenCalledTimes(2)
})

it('discards a late history response after the current account changes', async () => {
  let finish: ((value: unknown) => void) | undefined
  vi.spyOn(api, 'get')
    .mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          finish = resolve
        })
    )
    .mockResolvedValue({
      data: { success: true, data: { items: [], total: 0 } },
    })
  renderHistory()
  await act(async () => {
    useAuthStore
      .getState()
      .auth.setUser({ id: 2, username: 'other', role: ROLE.USER })
  })
  await screen.findByText('No billing records found')
  await act(async () => {
    finish?.({ data: { success: true, data: { items: [record], total: 1 } } })
  })
  expect(screen.queryByText('exact-order')).not.toBeInTheDocument()
})
