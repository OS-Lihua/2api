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
import { toast } from 'sonner'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import en from '@/i18n/locales/en.json'
import fr from '@/i18n/locales/fr.json'
import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import type { User } from '../../types'
import { UsersMutateDrawer } from '../users-mutate-drawer'
import { UsersProvider } from '../users-provider'

const target: User = {
  id: 2,
  username: 'wallet-owner',
  display_name: 'Wallet owner',
  role: 1,
  status: 1,
  quota: 77,
  used_quota: 0,
  request_count: 0,
  group: 'default',
}
const wallet = {
  user_id: 2,
  currency: 'EUR',
  balance: '9007199.254740993',
  outstanding: '0.000000001',
  requests_paused: true,
  updated_at: 1,
}
const clients: QueryClient[] = []
const i18n = createInstance()
await i18n.init({
  lng: 'en',
  resources: { en, fr },
  initAsync: false,
})

function renderDrawer(role = 100, read = true, write = true) {
  useAuthStore.getState().auth.setUser({
    id: 1,
    username: 'operator',
    role,
    permissions: { admin_permissions: { ledger: { read, write } } },
  })
  const get = vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/group/') {
      return { data: { success: true, data: ['default'] } }
    }
    if (url === '/api/authz/catalog') {
      return { data: { success: true, data: { resources: [], roles: [] } } }
    }
    if (url === '/api/admin/v1/user-wallets/2') {
      return { data: { success: true, data: wallet } }
    }
    return { data: { success: true, data: target } }
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.push(client)
  const content = (user: User) => (
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <UsersProvider>
          <UsersMutateDrawer
            open
            onOpenChange={() => undefined}
            currentRow={user}
          />
        </UsersProvider>
      </QueryClientProvider>
    </I18nextProvider>
  )
  const view = render(content(target))
  return {
    get,
    client,
    showUser: (user: User) => view.rerender(content(user)),
    ...view,
  }
}

async function openWallet() {
  await screen.findByDisplayValue('Wallet owner')
  await userEvent.click(screen.getByRole('button', { name: 'Wallet' }))
  return screen.findByRole('dialog', { name: 'Wallet' })
}

async function reviewAdjustment(amount: string, remark = 'Balance correction') {
  await userEvent.type(
    await screen.findByRole('textbox', { name: /^Amount/ }),
    amount
  )
  if (remark) {
    await userEvent.type(
      screen.getByRole('textbox', { name: 'Remark' }),
      remark
    )
  }
  await userEvent.click(screen.getByRole('button', { name: 'Confirm' }))
}

beforeEach(async () => {
  await i18n.changeLanguage('en')
  vi.spyOn(window, 'scrollTo').mockImplementation(() => {})
})
afterEach(() => {
  cleanup()
  clients.splice(0).forEach((client) => client.clear())
  useAuthStore.getState().auth.reset()
  vi.restoreAllMocks()
})

it('loads exact wallet amounts and debt without exposing the retired quota editor', async () => {
  const { get } = renderDrawer()
  const dialog = await openWallet()
  expect(
    await within(dialog).findByText('9,007,199.254740993 EUR')
  ).toBeInTheDocument()
  expect(within(dialog).getByText('0.000000001 EUR')).toBeInTheDocument()
  expect(screen.queryByText('Adjust Quota')).not.toBeInTheDocument()
  expect(screen.queryByLabelText(/Remaining Quota/)).not.toBeInTheDocument()
  expect(get).toHaveBeenCalledWith(
    '/api/admin/v1/user-wallets/2',
    expect.objectContaining({ signal: expect.any(AbortSignal) })
  )
})

it.each(['9007199.254740993', '-0.000000001'])(
  'confirms a signed exact adjustment before posting amount %s to the ledger API',
  async (amount) => {
    const post = vi
      .spyOn(api, 'post')
      .mockResolvedValue({ data: { success: true, data: { id: 1 } } })
    const success = vi.spyOn(toast, 'success')
    renderDrawer()
    await openWallet()
    await reviewAdjustment(amount)
    expect(post).not.toHaveBeenCalled()
    const confirmation = await screen.findByRole('alertdialog', {
      name: 'Confirm balance adjustment',
    })
    expect(within(confirmation).getByText('wallet-owner')).toBeInTheDocument()
    expect(
      within(confirmation).getByText('Balance correction')
    ).toBeInTheDocument()
    expect(
      within(confirmation).getByText(
        amount === '-0.000000001'
          ? '-0.000000001 EUR'
          : '9,007,199.254740993 EUR'
      )
    ).toBeInTheDocument()
    await userEvent.click(
      within(confirmation).getByRole('button', { name: 'Confirm' })
    )
    await waitFor(() =>
      expect(post).toHaveBeenCalledWith(
        '/api/admin/v1/user-wallets/2/adjustments',
        { amount, remark: 'Balance correction' }
      )
    )
    await waitFor(() =>
      expect(success).toHaveBeenCalledWith('Balance updated successfully')
    )
    await waitFor(() =>
      expect(
        screen.queryByRole('dialog', { name: 'Wallet' })
      ).not.toBeInTheDocument()
    )
  }
)

it.each([
  '0',
  '1e3',
  '0.0000000001',
  '9223372036.854775808',
  '-9223372036.854775809',
])(
  'rejects invalid wallet amount %s before opening confirmation or sending a request',
  async (amount) => {
    const post = vi.spyOn(api, 'post')
    renderDrawer()
    await openWallet()
    await reviewAdjustment(amount)
    expect(await screen.findByText('Invalid amount')).toBeInTheDocument()
    expect(screen.getByRole('textbox', { name: /^Amount/ })).toHaveAttribute(
      'aria-invalid',
      'true'
    )
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    expect(post).not.toHaveBeenCalled()
  }
)

it('requires an explanatory remark for a valid wallet adjustment', async () => {
  const post = vi.spyOn(api, 'post')
  renderDrawer()
  await openWallet()
  await reviewAdjustment('0.000000001', '  ')
  expect(await screen.findByText('Remark is required')).toBeInTheDocument()
  expect(screen.getByRole('textbox', { name: 'Remark' })).toHaveAttribute(
    'aria-invalid',
    'true'
  )
  expect(post).not.toHaveBeenCalled()
})

it('lets a ledger reader inspect balances without showing adjustment controls', async () => {
  renderDrawer(10, true, false)
  const dialog = await openWallet()
  expect(
    await within(dialog).findByText('9,007,199.254740993 EUR')
  ).toBeInTheDocument()
  expect(within(dialog).queryByRole('textbox')).not.toBeInTheDocument()
  expect(
    within(dialog).queryByRole('button', { name: 'Confirm' })
  ).not.toBeInTheDocument()
})

it('hides wallet access when the administrator lacks ledger read permission', async () => {
  const { get } = renderDrawer(10, false, false)
  await screen.findByDisplayValue('Wallet owner')
  expect(
    screen.queryByRole('button', { name: 'Wallet' })
  ).not.toBeInTheDocument()
  expect(
    get.mock.calls.some(([url]) =>
      String(url).startsWith('/api/admin/v1/user-wallets/')
    )
  ).toBe(false)
})

it.each(['business', 'empty'])(
  'keeps a wallet unknown after a %s response and allows retry before adjustment',
  async (failure) => {
    const { get } = renderDrawer()
    let reads = 0
    get.mockImplementation(async (url) => {
      if (url === '/api/admin/v1/user-wallets/2') {
        reads++
        if (reads === 1) {
          return failure === 'business'
            ? { data: { success: false, message: 'Wallet unavailable' } }
            : { data: { success: true } }
        }
        return { data: { success: true, data: wallet } }
      }
      if (url === '/api/group/') {
        return { data: { success: true, data: ['default'] } }
      }
      if (url === '/api/authz/catalog') {
        return { data: { success: true, data: { resources: [], roles: [] } } }
      }
      return { data: { success: true, data: target } }
    })
    const dialog = await openWallet()
    expect(
      await within(dialog).findByText(
        failure === 'business' ? 'Wallet unavailable' : 'Failed to load'
      )
    ).toBeInTheDocument()
    expect(within(dialog).queryByText('0 EUR')).not.toBeInTheDocument()
    expect(within(dialog).queryByRole('textbox')).not.toBeInTheDocument()
    await userEvent.click(within(dialog).getByRole('button', { name: 'Retry' }))
    expect(
      await within(dialog).findByRole('textbox', { name: /^Amount/ })
    ).toBeEnabled()
  }
)

it.each(['business', 'network'])(
  'keeps the dialog and draft after a %s failure and allows a successful retry',
  async (failure) => {
    const post = vi.spyOn(api, 'post')
    if (failure === 'business') {
      post.mockResolvedValueOnce({
        data: { success: false, message: 'Insufficient balance' },
      })
    } else post.mockRejectedValueOnce(new Error('Network unavailable'))
    post.mockResolvedValue({ data: { success: true, data: { id: 1 } } })
    const success = vi.spyOn(toast, 'success')
    renderDrawer()
    await openWallet()
    await reviewAdjustment('-1')
    const confirmation = await screen.findByRole('alertdialog')
    await userEvent.click(
      within(confirmation).getByRole('button', { name: 'Confirm' })
    )
    expect(await screen.findByRole('alert')).toHaveTextContent(
      failure === 'business' ? 'Insufficient balance' : 'Network unavailable'
    )
    expect(success).not.toHaveBeenCalled()
    expect(screen.getByRole('textbox', { name: /^Amount/ })).toHaveValue('-1')
    await userEvent.click(screen.getByRole('button', { name: 'Confirm' }))
    const retry = await screen.findByRole('alertdialog')
    await userEvent.click(
      within(retry).getByRole('button', { name: 'Confirm' })
    )
    await waitFor(() =>
      expect(success).toHaveBeenCalledWith('Balance updated successfully')
    )
  }
)

it('disables confirmation while a posting is pending so it cannot be submitted twice', async () => {
  let resolve!: (value: {
    data: { success: boolean; data: { id: number } }
  }) => void
  const post = vi.spyOn(api, 'post').mockImplementation(
    () =>
      new Promise((done) => {
        resolve = done
      })
  )
  renderDrawer()
  await openWallet()
  await reviewAdjustment('1')
  const confirmation = await screen.findByRole('alertdialog')
  const confirm = within(confirmation).getByRole('button', {
    name: 'Confirm',
  })
  await userEvent.click(confirm)
  expect(confirm).toBeDisabled()
  await userEvent.click(confirm)
  expect(post).toHaveBeenCalledTimes(1)
  expect(
    within(confirmation).getByRole('button', { name: 'Cancel' })
  ).toBeDisabled()
  await act(async () => resolve({ data: { success: true, data: { id: 1 } } }))
})

it('does not offer adjustments to an administrator managing a peer account', async () => {
  const { showUser } = renderDrawer(10, true, true)
  showUser({ ...target, role: 10 })
  const dialog = await openWallet()
  expect(
    await within(dialog).findByText('9,007,199.254740993 EUR')
  ).toBeInTheDocument()
  expect(within(dialog).queryByRole('textbox')).not.toBeInTheDocument()
})

it('cancels confirmation with the keyboard and preserves the draft without posting', async () => {
  const post = vi.spyOn(api, 'post')
  renderDrawer()
  await openWallet()
  await reviewAdjustment('1')
  await screen.findByRole('alertdialog')
  await userEvent.keyboard('{Escape}')
  await waitFor(() =>
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
  )
  expect(screen.getByRole('textbox', { name: /^Amount/ })).toHaveValue('1')
  expect(screen.getByRole('textbox', { name: 'Remark' })).toHaveValue(
    'Balance correction'
  )
  expect(post).not.toHaveBeenCalled()
})

it('keeps unsaved profile changes when a wallet adjustment succeeds', async () => {
  vi.spyOn(api, 'post').mockResolvedValue({
    data: { success: true, data: { id: 1 } },
  })
  renderDrawer()
  const name = await screen.findByDisplayValue('Wallet owner')
  await userEvent.clear(name)
  await userEvent.type(name, 'Unsaved profile')
  await userEvent.click(screen.getByRole('button', { name: 'Wallet' }))
  await screen.findByRole('dialog', { name: 'Wallet' })
  await reviewAdjustment('1')
  const confirmation = await screen.findByRole('alertdialog')
  await userEvent.click(
    within(confirmation).getByRole('button', { name: 'Confirm' })
  )
  expect(await screen.findByDisplayValue('Unsaved profile')).toBeInTheDocument()
})

it('ignores a previous user wallet response after switching the target account', async () => {
  const { get, showUser } = renderDrawer()
  let resolve!: (value: {
    data: { success: boolean; data: typeof wallet }
  }) => void
  const nextUser = {
    ...target,
    id: 3,
    username: 'next-wallet-owner',
    display_name: 'Next wallet owner',
  }
  get.mockImplementation(async (url) => {
    if (url === '/api/admin/v1/user-wallets/2') {
      return new Promise((done) => {
        resolve = done
      })
    }
    if (url === '/api/admin/v1/user-wallets/3') {
      return {
        data: {
          success: true,
          data: {
            ...wallet,
            user_id: 3,
            currency: 'USD',
            balance: '3',
            outstanding: '0',
          },
        },
      }
    }
    if (url === '/api/user/3') {
      return { data: { success: true, data: nextUser } }
    }
    if (url === '/api/group/') {
      return { data: { success: true, data: ['default'] } }
    }
    if (url === '/api/authz/catalog') {
      return { data: { success: true, data: { resources: [], roles: [] } } }
    }
    return { data: { success: true, data: target } }
  })
  await openWallet()
  expect(screen.getByText('Loading...')).toBeInTheDocument()
  expect(
    screen.queryByRole('textbox', { name: /^Amount/ })
  ).not.toBeInTheDocument()
  showUser(nextUser)
  const dialog = await screen.findByRole('dialog', {
    name: 'Wallet',
  })
  expect(await within(dialog).findByText('3 USD')).toBeInTheDocument()
  await act(async () => resolve({ data: { success: true, data: wallet } }))
  expect(within(dialog).getByText('3 USD')).toBeInTheDocument()
  expect(
    within(dialog).queryByText('9,007,199.254740993 EUR')
  ).not.toBeInTheDocument()
  expect(within(dialog).getByRole('textbox', { name: /^Amount/ })).toHaveValue(
    ''
  )
})

it('invalidates the visible batch and self-wallet summaries after adjustment', async () => {
  vi.spyOn(api, 'post').mockResolvedValue({
    data: { success: true, data: { id: 1 } },
  })
  const { client } = renderDrawer()
  const batchKey = ['admin-user-wallets', 1, null, true, [2]]
  const selfKey = ['self-wallet', 2, 'session', 'summary', true]
  client.setQueryData(batchKey, ['old balance'])
  client.setQueryData(selfKey, ['old balance'])
  await openWallet()
  await reviewAdjustment('1')
  await userEvent.click(
    within(await screen.findByRole('alertdialog')).getByRole('button', {
      name: 'Confirm',
    })
  )
  await waitFor(() =>
    expect(client.getQueryState(batchKey)?.isInvalidated).toBe(true)
  )
  expect(client.getQueryState(selfKey)?.isInvalidated).toBe(true)
})

it('does not apply a late adjustment result to the next actor session', async () => {
  type Response = { data: { success: boolean; data: { id: number } } }
  let resolve!: (value: Response) => void
  const completion = new Promise<Response>((done) => {
    resolve = done
  })
  vi.spyOn(api, 'post').mockReturnValue(completion)
  const success = vi.spyOn(toast, 'success')
  renderDrawer()
  await openWallet()
  await reviewAdjustment('1')
  await userEvent.click(
    within(await screen.findByRole('alertdialog')).getByRole('button', {
      name: 'Confirm',
    })
  )
  act(() =>
    useAuthStore
      .getState()
      .auth.setUser({ id: 9, username: 'next-admin', role: 100 })
  )
  await act(async () => resolve({ data: { success: true, data: { id: 1 } } }))
  expect(success).not.toHaveBeenCalled()
})

it('updates exact wallet formatting when the interface language changes', async () => {
  renderDrawer()
  const dialog = await openWallet()
  expect(
    await within(dialog).findByText('9,007,199.254740993 EUR')
  ).toBeInTheDocument()
  await act(async () => {
    await i18n.changeLanguage('fr')
  })
  expect(
    within(dialog).getByText('9 007 199,254740993 EUR', {
      normalizer: (text) => text,
    })
  ).toBeInTheDocument()
})
