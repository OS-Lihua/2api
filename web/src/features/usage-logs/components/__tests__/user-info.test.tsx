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
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import zh from '@/i18n/locales/zh.json'
import { api } from '@/lib/api'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

import { UserInfoDialog } from '../dialogs/user-info-dialog'

let client: QueryClient
const i18n = createInstance()
await i18n.init({
  lng: 'en',
  resources: { en: { translation: {} }, zh },
  initAsync: false,
})

function actor(id = 1, ledger = true, userRead = true) {
  return {
    id,
    username: `admin-${id}`,
    role: ROLE.ADMIN,
    permissions: {
      admin_permissions: { user: { read: userRead }, ledger: { read: ledger } },
    },
  }
}
function metadata(id = 2) {
  return {
    data: {
      success: true,
      data: {
        id,
        username: `customer-${id}`,
        display_name: 'Customer',
        quota: 99999999,
        used_quota: 88888888,
        request_count: 17,
        group: 'default',
        aff_count: 2,
        referral_rewards: {
          available: { nanos: '9', currency: 'EUR' },
          earned: { nanos: '10', currency: 'EUR' },
          invited: 2,
        },
      },
    },
  }
}
function wallet(id = 2, nanos = '9007199254740993') {
  return {
    data: {
      success: true,
      data: {
        items: [
          {
            user_id: id,
            available: { nanos, currency: 'EUR' },
            outstanding: { nanos: '1', currency: 'EUR' },
            consumed: {
              nanos: '-922337203685477580800000001',
              currency: 'EUR',
            },
            sale_pending: true,
            requests_paused: true,
          },
        ],
      },
    },
  }
}
function fixture() {
  return vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/user/2') return metadata()
    if (url === '/api/admin/v1/user-wallets') return wallet()
    throw new Error('Unexpected API request')
  })
}
function view(userId = 2, open = true) {
  return (
    <QueryClientProvider client={client}>
      <I18nextProvider i18n={i18n}>
        <UserInfoDialog userId={userId} open={open} onOpenChange={vi.fn()} />
      </I18nextProvider>
    </QueryClientProvider>
  )
}
function mount(userId = 2) {
  return render(view(userId))
}
function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((done) => {
    resolve = done
  })
  return { promise, resolve }
}
function replaceActor(kind: 'account' | 'session') {
  if (kind === 'account') {
    useAuthStore.getState().auth.setUser(actor(9))
    return
  }
  useAuthStore.getState().auth.setBundle({
    access_token: 'test-session-access',
    token_type: 'Bearer',
    access_expires_at: 2_000_000_000,
    user: actor(),
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
}

beforeEach(async () => {
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  useAuthStore.getState().auth.setUser(actor())
  await i18n.changeLanguage('en')
})
afterEach(() => {
  cleanup()
  client.clear()
  useAuthStore.getState().auth.reset()
  vi.restoreAllMocks()
})

it('reads exact authorized wallet facts rather than the metadata quota fields', async () => {
  const get = fixture()
  mount()
  await screen.findByText('customer-2')
  expect(await screen.findByText('9,007,199.254740993 EUR')).toBeVisible()
  expect(
    screen.getByText('-922,337,203,685,477,580.800000001 EUR')
  ).toBeVisible()
  expect(screen.queryByText('Used Quota')).not.toBeInTheDocument()
  expect(screen.getByText('Wallet')).toBeVisible()
  expect(get).toHaveBeenCalledWith('/api/admin/v1/user-wallets', {
    params: { user_ids: '2' },
    signal: expect.any(AbortSignal),
  })
  await userEvent.click(
    screen.getByRole('button', { name: /Available Balance 9,007,199/ })
  )
  const details = await screen.findByRole('dialog', { name: 'Wallet' })
  expect(within(details).getByText('0.000000001 EUR')).toBeVisible()
  expect(within(details).getByText('Sales awaiting accounting')).toBeVisible()
  expect(within(details).getByText('Requests paused')).toBeVisible()
  expect(
    within(details).getByText(/Debt repayments are not counted again/)
  ).toBeVisible()
})

it('does not treat user metadata permission as wallet permission', async () => {
  useAuthStore.getState().auth.setUser(actor(1, false))
  const get = fixture()
  mount()
  await screen.findByText('customer-2')
  expect(screen.queryByText('Balance')).not.toBeInTheDocument()
  expect(screen.queryByText('Used Quota')).not.toBeInTheDocument()
  expect(screen.queryByText('Posted consumption')).not.toBeInTheDocument()
  expect(get.mock.calls.map(([url]) => url)).toEqual(['/api/user/2'])
})

it('shows a retryable metadata failure instead of a successful empty result', async () => {
  const get = vi
    .spyOn(api, 'get')
    .mockResolvedValue({ data: { success: false, message: 'Not available' } })
  mount()
  expect(await screen.findByRole('button', { name: 'Retry' })).toBeVisible()
  expect(
    screen.queryByText('No user information available')
  ).not.toBeInTheDocument()
  get.mockImplementation(async (url) =>
    url === '/api/user/2' ? metadata() : wallet()
  )
  await userEvent.click(screen.getByRole('button', { name: 'Retry' }))
  expect(await screen.findByText('customer-2')).toBeVisible()
  expect(await screen.findByText('9,007,199.254740993 EUR')).toBeVisible()
})

it.each(['account', 'session'] as const)(
  'discards late metadata after an actor %s change',
  async (kind) => {
    const pending = deferred<ReturnType<typeof metadata>>()
    let metadataCalls = 0
    const get = vi.spyOn(api, 'get').mockImplementation(async (url) => {
      if (url === '/api/admin/v1/user-wallets') return wallet(2, '8000000000')
      if (++metadataCalls === 1) return pending.promise
      const response = metadata()
      response.data.data.username = 'replacement-customer'
      return response
    })
    mount()
    await waitFor(() => expect(get).toHaveBeenCalledTimes(1))
    const signal = get.mock.calls[0][1]?.signal
    act(() => replaceActor(kind))
    expect(await screen.findByText('replacement-customer')).toBeVisible()
    expect(await screen.findByText('8 EUR')).toBeVisible()
    expect(signal?.aborted).toBe(true)
    const old = metadata()
    old.data.data.username = 'stale-customer'
    await act(async () => pending.resolve(old))
    expect(screen.queryByText('stale-customer')).not.toBeInTheDocument()
    expect(screen.getByText('8 EUR')).toBeVisible()
    expect(
      get.mock.calls.filter(([url]) => url === '/api/admin/v1/user-wallets')
    ).toHaveLength(1)
  }
)

it.each(['account', 'session'] as const)(
  'discards late wallet facts after an actor %s change',
  async (kind) => {
    const pending = deferred<ReturnType<typeof wallet>>()
    let walletCalls = 0
    const get = vi.spyOn(api, 'get').mockImplementation(async (url) => {
      if (url === '/api/user/2') return metadata()
      if (++walletCalls === 1) return pending.promise
      return wallet(2, '8000000000')
    })
    mount()
    await screen.findByText('customer-2')
    await waitFor(() => expect(walletCalls).toBe(1))
    const signal = get.mock.calls.find(
      ([url]) => url === '/api/admin/v1/user-wallets'
    )?.[1]?.signal
    act(() => replaceActor(kind))
    expect(await screen.findByText('8 EUR')).toBeVisible()
    await act(async () => pending.resolve(wallet()))
    expect(signal?.aborted).toBe(true)
    expect(
      screen.queryByText('9,007,199.254740993 EUR')
    ).not.toBeInTheDocument()
    expect(screen.getByText('8 EUR')).toBeVisible()
  }
)

it.each(['ledger', 'user'] as const)(
  'hides cached financial facts when %s permission is revoked',
  async (resource) => {
    const get = fixture()
    mount()
    await screen.findByText('9,007,199.254740993 EUR')
    act(() =>
      useAuthStore
        .getState()
        .auth.setUser(actor(1, resource !== 'ledger', resource !== 'user'))
    )
    expect(
      screen.queryByText('9,007,199.254740993 EUR')
    ).not.toBeInTheDocument()
    expect(screen.queryByText('Posted consumption')).not.toBeInTheDocument()
    if (resource === 'user') {
      expect(screen.queryByText('customer-2')).not.toBeInTheDocument()
      expect(
        screen.getByText('No permission to perform this action')
      ).toBeVisible()
    } else {
      expect(screen.getByText('customer-2')).toBeVisible()
    }
    expect(get).toHaveBeenCalledTimes(2)
  }
)

it.each([
  {
    available: { nanos: 9007199254740992, currency: 'EUR' },
    earned: { nanos: '10', currency: 'EUR' },
    invited: 2,
  },
  {
    available: { nanos: '9', currency: 'EUR' },
    earned: { nanos: '10', currency: 'USD' },
    invited: 2,
  },
])(
  'rejects malformed referral money in user metadata instead of coercing it',
  async (rewards) => {
    const response = metadata()
    vi.spyOn(api, 'get').mockImplementation(async (url) =>
      url === '/api/user/2'
        ? {
            data: {
              ...response.data,
              data: { ...response.data.data, referral_rewards: rewards },
            },
          }
        : wallet()
    )
    mount()
    expect(await screen.findByRole('button', { name: 'Retry' })).toBeVisible()
    expect(screen.queryByText('customer-2')).not.toBeInTheDocument()
    expect(
      screen.queryByText('9,007,199.254740992 EUR')
    ).not.toBeInTheDocument()
  }
)

it('denies metadata requests without user-read permission even if ledger-read is present', async () => {
  useAuthStore.getState().auth.setUser(actor(1, true, false))
  const get = fixture()
  mount()
  expect(screen.getByText('No permission to perform this action')).toBeVisible()
  await act(async () => {
    await i18n.changeLanguage('zh')
  })
  expect(screen.getByText('无权进行此操作')).toBeVisible()
  expect(screen.queryByText('Permission denied')).not.toBeInTheDocument()
  expect(get).not.toHaveBeenCalled()
})

it.each([
  { label: 'wrong target', value: wallet(99) },
  {
    label: 'missing aggregate',
    value: { data: { success: true, data: { items: [] } } },
  },
  {
    label: 'unknown consumption',
    value: {
      data: {
        success: true,
        data: { items: [{ ...wallet().data.data.items[0], consumed: null }] },
      },
    },
  },
])(
  'retries $label wallet data without showing quota or a guessed zero',
  async ({ value }) => {
    let failed = true
    const get = vi.spyOn(api, 'get').mockImplementation(async (url) => {
      if (url === '/api/user/2') return metadata()
      return failed ? value : wallet()
    })
    mount()
    expect(await screen.findByText('Wallet unavailable')).toBeVisible()
    expect(screen.getByText('customer-2')).toBeVisible()
    expect(screen.queryByText('Used Quota')).not.toBeInTheDocument()
    expect(screen.queryByText('0 EUR')).not.toBeInTheDocument()
    failed = false
    await userEvent.click(screen.getByRole('button', { name: 'Retry' }))
    expect(await screen.findByText('9,007,199.254740993 EUR')).toBeVisible()
    expect(
      get.mock.calls.filter(([url]) => url === '/api/user/2')
    ).toHaveLength(1)
  }
)

it('isolates a newly selected target from a late response and closes normally', async () => {
  const pending = deferred<ReturnType<typeof metadata>>()
  const get = vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/user/2') return pending.promise
    if (url === '/api/user/3') return metadata(3)
    return wallet(3, '3000000000')
  })
  const rendered = mount()
  await waitFor(() => expect(get).toHaveBeenCalledTimes(1))
  const signal = get.mock.calls[0][1]?.signal
  rendered.rerender(view(3))
  expect(await screen.findByText('customer-3')).toBeVisible()
  expect(await screen.findByText('3 EUR')).toBeVisible()
  await act(async () => pending.resolve(metadata()))
  expect(signal?.aborted).toBe(true)
  expect(screen.queryByText('customer-2')).not.toBeInTheDocument()
  rendered.rerender(view(3, false))
  await waitFor(() =>
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  )
})

it('aborts pending metadata on close without fetching a hidden wallet', async () => {
  const pending = deferred<ReturnType<typeof metadata>>()
  const get = vi
    .spyOn(api, 'get')
    .mockImplementation(async () => pending.promise)
  const rendered = mount()
  await waitFor(() => expect(get).toHaveBeenCalledTimes(1))
  const signal = get.mock.calls[0][1]?.signal
  rendered.rerender(view(2, false))
  await waitFor(() => expect(signal?.aborted).toBe(true))
  await act(async () => pending.resolve(metadata()))
  expect(screen.queryByText('customer-2')).not.toBeInTheDocument()
  expect(get).toHaveBeenCalledTimes(1)
})

it('responds to language changes without refetching or losing fractional nanos', async () => {
  const get = fixture()
  mount()
  await screen.findByText('9,007,199.254740993 EUR')
  await act(async () => i18n.changeLanguage('zh'))
  expect(screen.getByRole('dialog', { name: '用户信息' })).toBeVisible()
  expect(screen.getByText('9,007,199.254740993 EUR')).toBeVisible()
  expect(screen.getByText(zh.translation['Posted consumption'])).toBeVisible()
  expect(get).toHaveBeenCalledTimes(2)
})
