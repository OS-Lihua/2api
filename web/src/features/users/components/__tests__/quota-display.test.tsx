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
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import {
  flexRender,
  getCoreRowModel,
  useReactTable,
} from '@tanstack/react-table'
import {
  act,
  cleanup,
  render,
  screen,
  within,
  waitFor,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import { TooltipProvider } from '@/components/ui/tooltip'
import en from '@/i18n/locales/en.json'
import fr from '@/i18n/locales/fr.json'
import ja from '@/i18n/locales/ja.json'
import ru from '@/i18n/locales/ru.json'
import viLocale from '@/i18n/locales/vi.json'
import zhTW from '@/i18n/locales/zh-TW.json'
import zh from '@/i18n/locales/zh.json'
import { api } from '@/lib/api'
import { formatTimestampToDate } from '@/lib/format'
import type { WalletSummary } from '@/lib/wallet-summary'
import { useAuthStore } from '@/stores/auth-store'
import {
  DEFAULT_CURRENCY_CONFIG,
  useSystemConfigStore,
} from '@/stores/system-config-store'

import type { User } from '../../types'
import { UserWalletCell } from '../user-quota-cell'
import { useUsersColumns } from '../users-columns'
import { UsersProvider } from '../users-provider'
import { UsersTable } from '../users-table'

const clients: QueryClient[] = []
const i18n = createInstance()
await i18n.init({
  lng: 'en',
  resources: { en: { translation: {} } },
  initAsync: false,
})

function QuotaTable(props: {
  remaining: string
  used: string
  outstanding?: string
}) {
  const wallet: WalletSummary = {
    user_id: 1,
    available: { nanos: props.remaining, currency: 'EUR' },
    outstanding: { nanos: props.outstanding ?? '0', currency: 'EUR' },
    consumed: { nanos: props.used, currency: 'EUR' },
    sale_pending: false,
    requests_paused: false,
  }
  const columns = useUsersColumns({
    visible: true,
    byId: new Map([[1, wallet]]),
    loading: false,
  }).filter((column) => column.id === 'wallet')
  const table = useReactTable({
    columns,
    data: [
      {
        id: 1,
        username: 'test',
        display_name: '',
        role: 1,
        status: 1,
        quota: 999999,
        used_quota: 999999,
        request_count: 0,
        group: 'default',
      } as User,
    ],
    getCoreRowModel: getCoreRowModel(),
  })
  return (
    <table>
      <thead>
        {table.getHeaderGroups().map((group) => (
          <tr key={group.id}>
            {group.headers.map((header) => (
              <th key={header.id} data-sortable={header.column.getCanSort()}>
                {flexRender(
                  header.column.columnDef.header,
                  header.getContext()
                )}
              </th>
            ))}
          </tr>
        ))}
      </thead>
      <tbody>
        {table.getRowModel().rows.map((row) => (
          <tr key={row.id}>
            {row.getVisibleCells().map((cell) => (
              <td key={cell.id}>
                {flexRender(cell.column.columnDef.cell, cell.getContext())}
              </td>
            ))}
          </tr>
        ))}
      </tbody>
    </table>
  )
}

beforeEach(() => {
  vi.spyOn(window, 'scrollTo').mockImplementation(() => {})
  useSystemConfigStore
    .getState()
    .setConfig({ currency: { ...DEFAULT_CURRENCY_CONFIG } })
})
afterEach(() => {
  cleanup()
  clients.splice(0).forEach((client) => client.clear())
  useAuthStore.getState().auth.reset()
  useSystemConfigStore
    .getState()
    .setConfig({ currency: { ...DEFAULT_CURRENCY_CONFIG } })
})

it('keeps long native amounts, Chinese labels and the full explanation accessible in a viewport-bounded wallet popover', async () => {
  const translated = createInstance()
  await translated.init({ lng: 'zh', resources: { zh }, initAsync: false })
  render(
    <I18nextProvider i18n={translated}>
      <UserWalletCell
        wallet={{
          user_id: 1,
          available: { nanos: '9007199254740993', currency: 'EUR' },
          outstanding: { nanos: '1', currency: 'EUR' },
          consumed: { nanos: '-922337203685477580800000001', currency: 'EUR' },
          sale_pending: true,
          requests_paused: true,
        }}
      />
    </I18nextProvider>
  )
  await userEvent.click(screen.getByRole('button'))
  const dialog = screen.getByRole('dialog')
  expect(dialog).toHaveClass('max-h-(--available-height)', 'overflow-y-auto')
  const label = within(dialog).getByText(translated.t('Posted consumption'))
  expect(label.parentElement).toHaveClass('grid-cols-1', 'min-w-0')
  expect(label.nextElementSibling).toHaveTextContent(
    '-922,337,203,685,477,580.800000001 EUR'
  )
  expect(label.nextElementSibling).toHaveClass('wrap-anywhere')
  expect(
    within(dialog).getByText(
      translated.t(
        'Posted ledger consumption includes signed corrections. It excludes unsettled sales and legacy quota records. Debt repayments are not counted again.'
      )
    )
  ).toBeVisible()
  expect(
    within(dialog).getByText(translated.t('Requests paused'))
  ).toBeVisible()
})

it('translates paused wallet details with real resources when switching all seven languages', async () => {
  const translated = createInstance()
  await translated.init({
    lng: 'en',
    fallbackLng: 'en',
    initAsync: false,
    resources: { en, fr, ja, ru, vi: viLocale, zhCN: zh, zhTW },
  })
  render(
    <I18nextProvider i18n={translated}>
      <UserWalletCell
        wallet={{
          user_id: 1,
          available: { nanos: '9007199254740993', currency: 'EUR' },
          outstanding: { nanos: '1', currency: 'EUR' },
          consumed: { nanos: '-922337203685477580800000001', currency: 'JPY' },
          sale_pending: true,
          requests_paused: true,
        }}
      />
    </I18nextProvider>
  )
  await userEvent.click(screen.getByRole('button'))
  for (const [language, label, amount] of [
    ['en', 'Requests paused', '9,007,199.254740993 EUR'],
    ['zhCN', '请求已暂停', '9,007,199.254740993 EUR'],
    ['zhTW', '請求已暫停', '9,007,199.254740993 EUR'],
    ['fr', 'Requêtes suspendues', '9 007 199,254740993 EUR'],
    ['ru', 'Запросы приостановлены', '9 007 199,254740993 EUR'],
    ['ja', 'リクエスト停止中', '9,007,199.254740993 EUR'],
    ['vi', 'Yêu cầu đã tạm dừng', '9.007.199,254740993 EUR'],
  ]) {
    await act(async () => translated.changeLanguage(language))
    const dialog = screen.getByRole('dialog')
    expect(within(dialog).getByText(label)).toBeVisible()
    expect(
      within(dialog).getByText(amount, { normalizer: (value) => value })
    ).toBeVisible()
    if (language !== 'en') {
      expect(
        within(dialog).queryByText('Requests paused')
      ).not.toBeInTheDocument()
    }
  }
})

it('shows native balance above posted consumption and opens wallet details on click', async () => {
  render(
    <I18nextProvider i18n={i18n}>
      <QuotaTable remaining='1900000000' used='1100000000' />
    </I18nextProvider>
  )
  expect(
    screen.getByRole('columnheader', { name: 'Available Balance' })
  ).toHaveAttribute('data-sortable', 'false')
  expect(screen.getAllByRole('columnheader')).toHaveLength(1)
  const cells = screen.getAllByRole('cell')
  expect(cells).toHaveLength(1)
  expect(within(cells[0]).getByText('1.9 EUR')).toBeVisible()
  expect(within(cells[0]).getByText('1.1 EUR')).toBeVisible()
  expect(
    within(cells[0]).queryByText('Available Balance')
  ).not.toBeInTheDocument()
  expect(screen.getByText('1.9 EUR').parentElement).toHaveClass('grid-cols-1')
  expect(screen.getByText('Posted consumption').parentElement).toHaveAttribute(
    'data-table-text',
    'secondary'
  )
  expect(screen.queryByRole('progressbar')).not.toBeInTheDocument()
  const trigger = screen.getByRole('button', {
    name: 'Available Balance 1.9 EUR; Posted consumption 1.1 EUR',
  })
  await userEvent.click(trigger)
  const detail = await screen.findByRole('dialog', { name: 'Wallet' })
  for (const text of [
    'Available Balance',
    'Posted consumption',
    'Outstanding',
    '1.9 EUR',
    '1.1 EUR',
    '0 EUR',
  ]) {
    expect(within(detail).getByText(text)).toBeVisible()
  }
  expect(within(detail).getByText(/includes signed corrections/)).toBeVisible()
  expect(within(detail).queryByRole('progressbar')).not.toBeInTheDocument()
  await userEvent.keyboard('{Escape}')
  await waitFor(() =>
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  )
  expect(trigger).toHaveFocus()
})

it.each([
  ['0', '0 EUR'],
  ['1000000000', '1 EUR'],
])(
  'preserves a real zero wallet and posted consumption %s',
  async (used, expected) => {
    render(
      <I18nextProvider i18n={i18n}>
        <QuotaTable remaining='0' used={used} />
      </I18nextProvider>
    )
    expect(screen.queryByText('No Quota')).not.toBeInTheDocument()
    expect(screen.getByText('Posted consumption')).toBeVisible()
    await userEvent.click(
      screen.getByRole('button', {
        name: `Available Balance 0 EUR; Posted consumption ${expected}`,
      })
    )
    const detail = await screen.findByRole('dialog', { name: 'Wallet' })
    expect(within(detail).getAllByText('0 EUR')).toHaveLength(
      used === '0' ? 3 : 2
    )
    expect(within(detail).getAllByText(expected).length).toBeGreaterThan(0)
  }
)

it('preserves outstanding debt and signed corrections in details opened with the keyboard', async () => {
  render(
    <I18nextProvider i18n={i18n}>
      <QuotaTable remaining='0' used='-2000000000' outstanding='1000000000' />
    </I18nextProvider>
  )
  expect(screen.getByText('0 EUR')).toBeVisible()
  expect(screen.getByText('-2 EUR')).toBeVisible()
  await userEvent.tab()
  await userEvent.keyboard('{Enter}')
  const detail = await screen.findByRole('dialog')
  expect(within(detail).getByText('1 EUR')).toBeVisible()
  expect(within(detail).getByText('-2 EUR')).toBeVisible()
  expect(within(detail).getByText('Outstanding')).toBeVisible()
})

it('keeps the native money currency when a custom display symbol is configured', () => {
  useSystemConfigStore.getState().setConfig({
    currency: {
      ...DEFAULT_CURRENCY_CONFIG,
      quotaDisplayType: 'CUSTOM',
      customCurrencySymbol: '🐱',
      customCurrencyExchangeRate: 1,
    },
  })
  render(
    <I18nextProvider i18n={i18n}>
      <QuotaTable remaining='1900000000' used='1100000000' />
    </I18nextProvider>
  )
  expect(
    screen.getByRole('columnheader', { name: 'Available Balance' })
  ).toBeInTheDocument()
  for (const cell of screen.getAllByRole('cell')) {
    expect(cell).not.toHaveTextContent('🐱')
  }
  expect(
    within(screen.getAllByRole('cell')[0]).getByText('1.9 EUR')
  ).toBeInTheDocument()
  expect(
    within(screen.getAllByRole('cell')[0]).getByText('1.1 EUR')
  ).toBeInTheDocument()
})

function UsersPage() {
  return (
    <TooltipProvider>
      <UsersProvider>
        <UsersTable />
      </UsersProvider>
    </TooltipProvider>
  )
}

async function renderUsersList(
  emptyInvitation = false,
  options: {
    ledgerRead?: boolean
    deleted?: boolean
    initialEntry?: string
  } = {}
) {
  useAuthStore.getState().auth.setUser({
    id: 1,
    username: 'admin',
    role: 10,
    permissions: {
      admin_permissions: { ledger: { read: options.ledgerRead ?? true } },
    },
  })
  const usersResponse = {
    data: {
      success: true,
      data: {
        items: [
          {
            id: 2,
            username: 'long-user-name-for-table-layout',
            DeletedAt: options.deleted ? '2026-01-01T00:00:00Z' : null,
            display_name: 'A display name',
            role: 1,
            status: 1,
            quota: 1900,
            used_quota: 1100,
            request_count: 0,
            created_at: Math.floor(Date.now() / 1000) - 86400,
            last_login_at: Math.floor(Date.now() / 1000) - 20,
            group: 'default',
            aff_count: emptyInvitation ? 0 : 2,
            referral_rewards: {
              available: { nanos: '0', currency: 'CNY' },
              earned: {
                nanos: emptyInvitation ? '0' : '9007199254740993',
                currency: 'CNY',
              },
              invited: emptyInvitation ? 0 : 2,
            },
            inviter_id: emptyInvitation ? 0 : 42,
          },
        ],
        total: 1,
      },
    },
  }
  const get = vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/group/') {
      return { data: { success: true, data: ['default'] } }
    }
    if (url === '/api/admin/v1/user-wallets') {
      return {
        data: {
          success: true,
          data: {
            items: [
              {
                user_id: 2,
                available: { nanos: '9007199254740993', currency: 'EUR' },
                outstanding: { nanos: '1', currency: 'EUR' },
                consumed: {
                  nanos: '922337203685477580800000001',
                  currency: 'JPY',
                },
                sale_pending: true,
                requests_paused: true,
              },
            ],
          },
        },
      }
    }
    return usersResponse
  })
  const root = createRootRoute()
  const auth = createRoute({ getParentRoute: () => root, id: '_authenticated' })
  const users = createRoute({
    getParentRoute: () => auth,
    path: 'users/',
    component: UsersPage,
  })
  const router = createRouter({
    routeTree: root.addChildren([auth.addChildren([users])]),
    history: createMemoryHistory({
      initialEntries: [options.initialEntry ?? '/users/'],
    }),
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.push(client)
  await router.load()
  render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <RouterProvider router={router} />
      </QueryClientProvider>
    </I18nextProvider>
  )
  await screen.findByText('long-user-name-for-table-layout')
  return get
}

it('bounds an oversized URL page to the batch limit before sending either request', async () => {
  const get = await renderUsersList(false, {
    initialEntry: '/users/?pageSize=1000',
  })
  await screen.findByText('9,007,199.254740993 EUR')
  expect(get).toHaveBeenCalledWith('/api/user/', {
    params: { p: 1, page_size: 100 },
    signal: expect.any(AbortSignal),
  })
  for (const [, config] of get.mock.calls.filter(
    ([url]) => url === '/api/user/'
  )) {
    expect(config).toMatchObject({ params: { page_size: 100 } })
  }
  expect(get).toHaveBeenCalledWith('/api/admin/v1/user-wallets', {
    params: { user_ids: '2' },
    signal: expect.any(AbortSignal),
  })
})

it('does not request financial data for a user reader without ledger permission', async () => {
  const get = await renderUsersList(false, { ledgerRead: false })
  expect(
    screen.queryByRole('columnheader', { name: 'Available Balance' })
  ).not.toBeInTheDocument()
  expect(
    get.mock.calls.some(([url]) => url === '/api/admin/v1/user-wallets')
  ).toBe(false)
})

it('excludes deleted rows from batch reads and never presents them as a zero wallet', async () => {
  const get = await renderUsersList(false, { deleted: true })
  expect(screen.getByText('Wallet unavailable')).toBeVisible()
  expect(
    get.mock.calls.some(([url]) => url === '/api/admin/v1/user-wallets')
  ).toBe(false)
  expect(screen.queryByText('0 EUR')).not.toBeInTheDocument()
})

it('hides cached financial data immediately after ledger permission is revoked', async () => {
  await renderUsersList()
  await screen.findByText('9,007,199.254740993 EUR')
  act(() =>
    useAuthStore.getState().auth.setUser({
      id: 1,
      username: 'admin',
      role: 10,
      permissions: { admin_permissions: { ledger: { read: false } } },
    })
  )
  expect(screen.queryByText('9,007,199.254740993 EUR')).not.toBeInTheDocument()
  expect(screen.queryByText('Posted consumption')).not.toBeInTheDocument()
})

it('rejects malformed batch totals without falling back to quota and retries the read', async () => {
  const get = await renderUsersList()
  await screen.findByText('9,007,199.254740993 EUR')
  const original = get.getMockImplementation()
  if (!original) throw new Error('Expected API fixture')
  get.mockImplementation(async (url, config) => {
    if (url === '/api/admin/v1/user-wallets') {
      return { data: { success: true, data: { items: [] } } }
    }
    return original(url, config)
  })
  await act(async () => {
    await clients[0].invalidateQueries({ queryKey: ['admin-user-wallets'] })
  })
  const retry = await screen.findByRole('button', { name: 'Retry' })
  expect(screen.queryByText('9,007,199.254740993 EUR')).not.toBeInTheDocument()
  expect(screen.queryByText('0.0038')).not.toBeInTheDocument()
  get.mockImplementation(original)
  await userEvent.click(retry)
  expect(await screen.findByText('9,007,199.254740993 EUR')).toBeVisible()
})

it.each(['account', 'session'] as const)(
  'does not reuse a pending batch response after the %s changes',
  async (change) => {
    const get = await renderUsersList()
    await screen.findByText('9,007,199.254740993 EUR')
    const original = get.getMockImplementation()
    if (!original) throw new Error('Expected API fixture')
    type Response = Awaited<ReturnType<typeof original>>
    let resolve!: (value: Response) => void
    const pending = new Promise<Response>((done) => {
      resolve = done
    })
    let oldSignal: AbortSignal | undefined
    let waiting = true
    get.mockImplementation(async (url, config) => {
      if (url !== '/api/admin/v1/user-wallets') return original(url, config)
      if (waiting) {
        waiting = false
        oldSignal = config?.signal as AbortSignal
        return pending
      }
      return {
        data: {
          success: true,
          data: {
            items: [
              {
                user_id: 2,
                available: { nanos: '3000000000', currency: 'EUR' },
                outstanding: { nanos: '0', currency: 'EUR' },
                consumed: { nanos: '0', currency: 'EUR' },
                sale_pending: false,
                requests_paused: false,
              },
            ],
          },
        },
      }
    })
    act(() => {
      void clients[0].invalidateQueries({ queryKey: ['admin-user-wallets'] })
    })
    await waitFor(() => expect(oldSignal).toBeDefined())
    act(() => {
      const auth = useAuthStore.getState().auth
      useAuthStore.setState({
        auth: {
          ...auth,
          user: {
            ...auth.user,
            id: change === 'account' ? 9 : 1,
            username: 'next-admin',
            role: 10,
            permissions: { admin_permissions: { ledger: { read: true } } },
          },
          session: {
            sid: 'next-session',
            current: true,
            login_method: 'password',
            ip: '',
            user_agent: '',
            created_at: 1,
            last_active_at: 1,
            expires_at: 9999999999,
          },
        },
      })
    })
    expect(await screen.findByText('3 EUR')).toBeVisible()
    expect(oldSignal?.aborted).toBe(true)
    await act(async () => resolve(await original('/api/admin/v1/user-wallets')))
    expect(
      screen.queryByText('9,007,199.254740993 EUR')
    ).not.toBeInTheDocument()
    expect(screen.getByText('3 EUR')).toBeVisible()
  }
)

it('reads current-page wallet summaries and preserves unbounded posted consumption', async () => {
  const get = await renderUsersList()
  expect(await screen.findByText('9,007,199.254740993 EUR')).toBeVisible()
  expect(
    screen.getByText('922,337,203,685,477,580.800000001 JPY')
  ).toBeVisible()
  expect(get).toHaveBeenCalledWith('/api/admin/v1/user-wallets', {
    params: { user_ids: '2' },
    signal: expect.any(AbortSignal),
  })
  expect(screen.queryByText('0.0038')).not.toBeInTheDocument()
  await userEvent.click(
    screen.getByRole('button', { name: /Available Balance 9,007,199/ })
  )
  const detail = await screen.findByRole('dialog', { name: 'Wallet' })
  expect(within(detail).getByText('0.000000001 EUR')).toBeVisible()
  expect(within(detail).getByText('Posted consumption')).toBeVisible()
  expect(within(detail).getByText('Sales awaiting accounting')).toBeVisible()
  expect(within(detail).getByText('Requests paused')).toBeVisible()
})

it('keeps wallet unsortable, sends ID sorting to the server and preserves invitation details', async () => {
  const get = await renderUsersList()
  expect(
    screen.getByRole('columnheader', { name: 'User Group' })
  ).toBeInTheDocument()
  expect(
    screen.queryByRole('button', { name: 'Available Balance' })
  ).not.toBeInTheDocument()
  await userEvent.click(screen.getByRole('button', { name: 'ID' }))
  await userEvent.click(screen.getByRole('menuitem', { name: 'Asc' }))
  await waitFor(() =>
    expect(get).toHaveBeenCalledWith('/api/user/', {
      params: expect.objectContaining({ sort_by: 'id', sort_order: 'asc' }),
      signal: expect.any(AbortSignal),
    })
  )
  expect(
    screen.queryByRole('button', { name: 'Total Used' })
  ).not.toBeInTheDocument()
  expect(screen.getByText('Inviter ID: 42')).toBeInTheDocument()
  expect(screen.getByText(/Invited 2 users · Earnings:/)).toBeInTheDocument()
  expect(screen.getByText('9,007,199.254740993 CNY')).toBeVisible()
})

it('shows balance above usage on mobile cards in Chinese', async () => {
  const originalMatchMedia = window.matchMedia
  vi.spyOn(window, 'matchMedia').mockImplementation((query) => ({
    ...originalMatchMedia(query),
    matches: query.includes('max-width'),
  }))
  i18n.addResourceBundle('zh', 'translation', zh.translation)
  await i18n.changeLanguage('zh')
  try {
    await renderUsersList()
    expect(screen.getByText('可用余额')).toBeInTheDocument()
    expect(await screen.findByText('已入账消费')).toBeInTheDocument()
    expect(await screen.findByText('9,007,199.254740993 EUR')).toBeVisible()
    expect(
      screen.getByText('922,337,203,685,477,580.800000001 JPY')
    ).toBeVisible()
    expect(screen.queryByRole('table')).not.toBeInTheDocument()
    await userEvent.click(
      screen.getByRole('button', { name: /可用余额 9,007,199/ })
    )
    const detail = await screen.findByRole('dialog', { name: '钱包' })
    expect(within(detail).getByText('已入账消费')).toBeInTheDocument()
    expect(
      within(detail).getByText('922,337,203,685,477,580.800000001 JPY')
    ).toBeInTheDocument()
    await userEvent.keyboard('{Escape}')
  } finally {
    await i18n.changeLanguage('en')
  }
})

it('combines creation and last login into one column with full dates visible directly', async () => {
  vi.spyOn(Date, 'now').mockReturnValue(Date.UTC(2026, 8, 8, 12))
  await renderUsersList(true)
  expect(screen.getByRole('columnheader', { name: /Time/ })).toBeInTheDocument()
  expect(
    screen.queryByRole('columnheader', { name: /Last Login/ })
  ).not.toBeInTheDocument()
  const row = screen.getByRole('row', {
    name: /long-user-name-for-table-layout/,
  })
  expect(within(row).getByText('—')).toBeInTheDocument()
  const times = within(row).getAllByRole('time')
  expect(times).toHaveLength(2)
  expect(times[0]).toHaveTextContent(
    formatTimestampToDate(Math.floor(Date.now() / 1000) - 86400)
  )
  expect(times[1]).toHaveTextContent(
    formatTimestampToDate(Math.floor(Date.now() / 1000) - 20)
  )
  expect(within(row).queryByText('Just now')).not.toBeInTheDocument()
  expect(within(row).getByText('Created')).toBeInTheDocument()
  expect(within(row).getByText('Last Login')).toBeInTheDocument()
  expect(screen.queryByText('No Inviter')).not.toBeInTheDocument()
  await userEvent.click(screen.getByRole('button', { name: 'View' }))
  await userEvent.click(screen.getByRole('menuitemcheckbox', { name: 'Time' }))
  expect(
    screen.queryByRole('columnheader', { name: /Time/ })
  ).not.toBeInTheDocument()
})

it('keeps native values unchanged when legacy display FX settings change', () => {
  render(
    <I18nextProvider i18n={i18n}>
      <QuotaTable remaining='1000000000' used='2000000000' />
    </I18nextProvider>
  )
  act(() =>
    useSystemConfigStore.getState().setConfig({
      currency: {
        ...DEFAULT_CURRENCY_CONFIG,
        quotaDisplayType: 'CNY',
        usdExchangeRate: 7,
      },
    })
  )
  expect(
    screen.getByRole('columnheader', { name: 'Available Balance' })
  ).toBeInTheDocument()
  expect(
    within(screen.getAllByRole('cell')[0]).getByText('1 EUR')
  ).toBeInTheDocument()
  expect(
    within(screen.getAllByRole('cell')[0]).getByText('2 EUR')
  ).toBeInTheDocument()
  for (const cell of screen.getAllByRole('cell')) {
    expect(cell).not.toHaveTextContent('¥')
  }
})

it('does not relabel native wallet money as tokens in the legacy raw quota mode', () => {
  useSystemConfigStore.getState().setConfig({
    currency: { ...DEFAULT_CURRENCY_CONFIG, quotaDisplayType: 'TOKENS' },
  })
  render(
    <I18nextProvider i18n={i18n}>
      <QuotaTable remaining='100' used='200' />
    </I18nextProvider>
  )
  expect(
    screen.getByRole('columnheader', { name: 'Available Balance' })
  ).toBeInTheDocument()
  expect(
    within(screen.getAllByRole('cell')[0]).getByText('0.0000001 EUR')
  ).toBeInTheDocument()
  expect(
    within(screen.getAllByRole('cell')[0]).getByText('0.0000002 EUR')
  ).toBeInTheDocument()
})
