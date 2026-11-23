import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, expect, it, vi } from 'vitest'

import en from '@/i18n/locales/en.json'
import fr from '@/i18n/locales/fr.json'
import ja from '@/i18n/locales/ja.json'
import ru from '@/i18n/locales/ru.json'
import viLocale from '@/i18n/locales/vi.json'
import zhTW from '@/i18n/locales/zh-TW.json'
import zh from '@/i18n/locales/zh.json'
import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { ProfileHeader } from '../components/profile-header'
import type { UserProfile } from '../types'

const profile: UserProfile & { quota: number; used_quota: number } = {
  id: 1,
  username: 'one',
  display_name: 'One',
  role: 1,
  group: 'default',
  quota: 500000,
  used_quota: 500000,
  request_count: 3,
  status: 1,
  aff_count: 0,
  created_time: 0,
}
const wallet = {
  user_id: 1,
  balance: '9007199.254740993',
  currency: 'EUR',
  outstanding: '0.000000001',
  consumed: { nanos: '-922337203685477580800000001', currency: 'JPY' },
  sale_pending: true,
  requests_paused: true,
}
const clients: QueryClient[] = []
function renderHeader(allowed = true) {
  useAuthStore.getState().auth.setUser({
    id: 1,
    username: 'one',
    role: 1,
    permissions: { admin_permissions: { wallet: { read: allowed } } },
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.push(client)
  const result = render(
    <QueryClientProvider client={client}>
      <ProfileHeader profile={profile} loading={false} />
    </QueryClientProvider>
  )
  return { ...result, client }
}
afterEach(() => {
  cleanup()
  clients.splice(0).forEach((client) => client.clear())
  useAuthStore.getState().auth.reset()
  vi.restoreAllMocks()
})

it('reads exact self-wallet amounts independently from legacy profile quota', async () => {
  const get = vi
    .spyOn(api, 'get')
    .mockResolvedValue({ data: { success: true, data: wallet } })
  renderHeader()
  expect(await screen.findByText('9,007,199.254740993 EUR')).toBeVisible()
  expect(
    screen.getByText('-922,337,203,685,477,580.800000001 JPY')
  ).toBeVisible()
  expect(screen.getByText('0.000000001 EUR')).toBeVisible()
  expect(screen.getByText('Sales awaiting accounting')).toBeVisible()
  expect(screen.getByText('Requests paused')).toBeVisible()
  expect(screen.getByText('Posted consumption')).toBeVisible()
  expect(get).toHaveBeenCalledWith('/api/user/self/wallet', {
    signal: expect.any(AbortSignal),
  })
  expect(screen.queryByText('Total consumed quota')).not.toBeInTheDocument()
})

it.each([true, false, undefined])(
  'keeps funding allocation status distinct from sale pricing and zero debt (%s)',
  async (pending) => {
    vi.spyOn(api, 'get').mockResolvedValue({
      data: {
        success: true,
        data: {
          ...wallet,
          outstanding: '0',
          sale_pending: false,
          funding_pending: pending,
        },
      },
    })
    renderHeader()
    await screen.findByText('9,007,199.254740993 EUR')
    expect(screen.getByText('0 EUR')).toBeVisible()
    expect(
      screen.queryByText('Sales awaiting accounting')
    ).not.toBeInTheDocument()
    if (pending === true) {
      expect(screen.getByText('Funding allocation pending')).toBeVisible()
    } else {
      expect(
        screen.queryByText('Funding allocation pending')
      ).not.toBeInTheDocument()
    }
    if (pending === undefined) {
      expect(screen.getByText('Funding status unavailable')).toBeVisible()
    }
  }
)

it('preserves every amount digit through all seven interface languages', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: wallet },
  })
  const i18n = createInstance()
  await i18n.init({
    lng: 'en',
    fallbackLng: 'en',
    resources: { en, zh, 'zh-TW': zhTW, fr, ru, ja, vi: viLocale },
    interpolation: { escapeValue: false },
  })
  const view = renderHeader()
  view.rerender(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={view.client}>
        <ProfileHeader profile={profile} loading={false} />
      </QueryClientProvider>
    </I18nextProvider>
  )
  for (const [language, expected, label] of [
    ['en', '9,007,199.254740993 EUR', en.translation['Posted consumption']],
    ['zh', '9,007,199.254740993 EUR', zh.translation['Posted consumption']],
    [
      'zh-TW',
      '9,007,199.254740993 EUR',
      zhTW.translation['Posted consumption'],
    ],
    ['fr', '9 007 199,254740993 EUR', fr.translation['Posted consumption']],
    ['ru', '9 007 199,254740993 EUR', ru.translation['Posted consumption']],
    ['ja', '9,007,199.254740993 EUR', ja.translation['Posted consumption']],
    [
      'vi',
      '9.007.199,254740993 EUR',
      viLocale.translation['Posted consumption'],
    ],
  ]) {
    await act(async () => {
      await i18n.changeLanguage(language)
    })
    expect(
      await screen.findByText(expected, { normalizer: (text) => text })
    ).toBeVisible()
    expect(screen.getByText(label)).toBeVisible()
  }
})

it('never requests or displays financial data without wallet read permission', async () => {
  const get = vi.spyOn(api, 'get')
  renderHeader(false)
  expect(screen.getByText('API Requests')).toBeVisible()
  expect(screen.queryByText('Available Balance')).not.toBeInTheDocument()
  expect(screen.queryByText('Posted consumption')).not.toBeInTheDocument()
  expect(get).not.toHaveBeenCalled()
})

it('shows a retryable error instead of old quota or zero on a financial read failure', async () => {
  const get = vi
    .spyOn(api, 'get')
    .mockRejectedValueOnce(new Error('unavailable'))
    .mockResolvedValue({ data: { success: true, data: wallet } })
  renderHeader()
  await userEvent.click(await screen.findByRole('button', { name: 'Retry' }))
  expect(await screen.findByText('9,007,199.254740993 EUR')).toBeVisible()
  expect(get).toHaveBeenCalledTimes(2)
})

it.each(['account', 'session'] as const)(
  'ignores an old self-wallet response after the %s changes',
  async (change) => {
    type Response = { data: { success: boolean; data: typeof wallet } }
    let resolve!: (value: Response) => void
    const pending = new Promise<Response>((done) => {
      resolve = done
    })
    const get = vi
      .spyOn(api, 'get')
      .mockReturnValueOnce(pending)
      .mockResolvedValue({
        data: {
          success: true,
          data: {
            ...wallet,
            user_id: change === 'account' ? 2 : 1,
            balance: '3',
          },
        },
      })
    const view = renderHeader()
    await waitFor(() => expect(get).toHaveBeenCalledTimes(1))
    const signal = get.mock.calls[0][1]?.signal
    act(() => {
      const auth = useAuthStore.getState().auth
      useAuthStore.setState({
        auth: {
          ...auth,
          user: {
            id: change === 'account' ? 2 : 1,
            username: 'next',
            role: 1,
            permissions: { admin_permissions: { wallet: { read: true } } },
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
    view.rerender(
      <QueryClientProvider client={view.client}>
        <ProfileHeader
          profile={{ ...profile, id: change === 'account' ? 2 : 1 }}
          loading={false}
        />
      </QueryClientProvider>
    )
    expect(await screen.findByText('3 EUR')).toBeVisible()
    expect(signal?.aborted).toBe(true)
    await act(async () => resolve({ data: { success: true, data: wallet } }))
    expect(
      screen.queryByText('9,007,199.254740993 EUR')
    ).not.toBeInTheDocument()
    expect(screen.getByText('3 EUR')).toBeVisible()
  }
)

it.each(['number', 'currency', 'negative-balance'] as const)(
  'rejects a %s contract violation instead of fabricating a monetary value',
  async (invalid) => {
    const data = { ...wallet, consumed: { ...wallet.consumed } }
    let response: unknown = data
    if (invalid === 'number') {
      response = { ...data, consumed: { ...data.consumed, nanos: 1 } }
    }
    if (invalid === 'currency') data.consumed.currency = 'eur'
    if (invalid === 'negative-balance') data.balance = '-0.000000001'
    vi.spyOn(api, 'get').mockResolvedValue({
      data: { success: true, data: response },
    })
    renderHeader()
    expect(await screen.findByRole('button', { name: 'Retry' })).toBeVisible()
    expect(
      screen.queryByText('9,007,199.254740993 EUR')
    ).not.toBeInTheDocument()
    expect(screen.queryByText('0 EUR')).not.toBeInTheDocument()
  }
)

it('hides loaded amounts immediately when wallet permission is revoked', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: wallet },
  })
  renderHeader()
  await screen.findByText('9,007,199.254740993 EUR')
  act(() =>
    useAuthStore.getState().auth.setUser({
      id: 1,
      username: 'one',
      role: 1,
      permissions: { admin_permissions: { wallet: { read: false } } },
    })
  )
  await waitFor(() =>
    expect(
      screen.queryByText('9,007,199.254740993 EUR')
    ).not.toBeInTheDocument()
  )
})
