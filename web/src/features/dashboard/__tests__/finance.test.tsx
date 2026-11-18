import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import en from '@/i18n/locales/en.json'
import fr from '@/i18n/locales/fr.json'
import ja from '@/i18n/locales/ja.json'
import ru from '@/i18n/locales/ru.json'
import viLocale from '@/i18n/locales/vi.json'
import zhTW from '@/i18n/locales/zh-TW.json'
import zh from '@/i18n/locales/zh.json'
import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { FinanceDashboard } from '../components/finance-dashboard'
import {
  bucketLabel,
  exactQuantity,
  normalizedGeometry,
  validFinanceFilter,
  type FinanceFilter,
} from '../lib/finance'
import { metric, reportFixture } from './finance-fixtures'

let client: QueryClient
const actor = (id = 1, admin = false, allowed = true) => ({
  id,
  username: `user-${id}`,
  role: admin ? 10 : 1,
  quota: 999999999,
  used_quota: 999999999,
  permissions: {
    admin_permissions: { wallet: { read: allowed }, ledger: { read: allowed } },
  },
})
const session = (sid: string) => ({
  sid,
  current: true,
  login_method: 'password',
  ip: '',
  user_agent: '',
  created_at: 1,
  last_active_at: 1,
  expires_at: 9999999999,
})
function renderDashboard(
  admin = false,
  view: 'models' | 'users' | 'flow' = 'models'
) {
  useAuthStore.getState().auth.setUser(actor(1, admin))
  useAuthStore.setState((state) => ({
    auth: { ...state.auth, session: session('first') },
  }))
  return render(
    <QueryClientProvider client={client}>
      <FinanceDashboard view={view} />
    </QueryClientProvider>
  )
}
function serve(admin = false) {
  return vi.spyOn(api, 'get').mockImplementation(async (url, config) => {
    expect(url).toBe(
      admin
        ? '/api/admin/v1/reports/dashboard-finance'
        : '/api/user/self/dashboard-finance'
    )
    expect(config?.signal).toBeInstanceOf(AbortSignal)
    return {
      data: {
        success: true,
        data: reportFixture(config?.params as FinanceFilter, admin),
      },
    }
  })
}
beforeEach(() => {
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  useAuthStore.setState(useAuthStore.getInitialState(), true)
})
afterEach(() => {
  cleanup()
  client.clear()
  vi.restoreAllMocks()
  useAuthStore.setState(useAuthStore.getInitialState(), true)
})

it('renders exact unbounded money and usage without quota fallbacks or self procurement', async () => {
  const get = serve()
  renderDashboard()
  expect(
    await screen.findByText('922,337,203,685,477,580.800000001 EUR')
  ).toBeVisible()
  expect(screen.getByText('9,007,199.254740993 EUR')).toBeVisible()
  const usage = screen.getByRole('table', { name: 'Usage facts' })
  expect(within(usage).getByText('9,007,199,254,740,993')).toBeVisible()
  expect(within(usage).getByText('Total unavailable')).toBeVisible()
  expect(screen.queryByText('Procurement cost')).not.toBeInTheDocument()
  expect(screen.getByText(/Primary gateway requests only/)).toBeVisible()
  expect(
    get.mock.calls.every(([url]) => !String(url).startsWith('/api/data'))
  ).toBe(true)
})
it('separates purchase-only pending, known signed difference, original currencies and sales', async () => {
  serve(true)
  renderDashboard(true)
  expect(await screen.findByText('-0.000000001 EUR')).toBeVisible()
  expect(screen.getByText('123.000000001 JPY')).toBeVisible()
  expect(
    screen.getByText(/A known difference is not a minimum profit/)
  ).toBeVisible()
  expect(screen.getByText(/Purchase price pending/)).toHaveTextContent('1')
  expect(
    screen.getAllByText(/Total unavailable · Pending requests: 1/)
  ).toHaveLength(2)
  expect(
    screen.queryByText('Sales awaiting accounting')
  ).not.toBeInTheDocument()
})
it('keeps known revenue and margin when allocation is pending without treating zero known debt as settled', async () => {
  vi.spyOn(api, 'get').mockImplementation(async (_, config) => {
    const report = reportFixture(config?.params as FinanceFilter, true)
    const totals = {
      ...report.totals,
      funding_pending_count: '1',
      collected: metric('0', '1'),
      outstanding: metric('0', '1'),
      margin: metric('7'),
      procurement: {
        cost: metric('1'),
        original_costs: [],
        purchase_pending_count: '0',
        fx_pending_count: '0',
      },
    }
    return { data: { success: true, data: { ...report, totals } } }
  })
  renderDashboard(true)
  expect(await screen.findByText('Funding allocation pending')).toBeVisible()
  expect(screen.getByText(/Pending funding requests/)).toHaveTextContent('1')
  expect(
    screen.getByText('922,337,203,685,477,580.800000001 EUR')
  ).toBeVisible()
  expect(screen.getByText('0.000000007 EUR')).toBeVisible()
  expect(
    screen.getAllByText(/Total unavailable · Pending requests: 1/)
  ).toHaveLength(2)
})

it('filters duplicate display names by stable Offering ID and opens full details', async () => {
  const user = userEvent.setup()
  const get = serve()
  renderDashboard()
  const details = await screen.findByRole('table', {
    name: 'Financial report details',
  })
  expect(
    within(details).getByRole('button', { name: 'Same name (#7)' })
  ).toBeVisible()
  expect(
    within(details).getByRole('button', { name: 'Same name (#8)' })
  ).toBeVisible()
  await user.click(
    within(details).getAllByRole('button', { name: 'Details' })[0]
  )
  expect(await screen.findByRole('dialog')).toHaveTextContent(
    'Model Offering: Same name (#7)'
  )
  await user.keyboard('{Escape}')
  await user.click(
    within(details).getByRole('button', { name: 'Same name (#8)' })
  )
  await waitFor(() =>
    expect(get).toHaveBeenLastCalledWith(
      '/api/user/self/dashboard-finance',
      expect.objectContaining({
        params: expect.objectContaining({ model_offering_id: '8' }),
      })
    )
  )
})
it.each([false, true])(
  'does not fetch financial data without the required read scope (admin=%s)',
  async (admin) => {
    const get = serve(admin)
    renderDashboard(admin)
    await screen.findByText('922,337,203,685,477,580.800000001 EUR')
    act(() => useAuthStore.getState().auth.setUser(actor(1, admin, false)))
    await waitFor(() =>
      expect(screen.getByText('Financial access required')).toBeVisible()
    )
    // Reset the spy after the authorized initial mount; revoked access must not issue another request.
    get.mockClear()
    await act(async () => {
      await client.invalidateQueries({ queryKey: ['dashboard-finance'] })
    })
    expect(get).not.toHaveBeenCalled()
    expect(screen.queryByText('0 EUR')).not.toBeInTheDocument()
  }
)
it('starts unauthorized without even requesting an initial report', () => {
  const get = vi.spyOn(api, 'get')
  useAuthStore.getState().auth.setUser(actor(1, false, false))
  render(
    <QueryClientProvider client={client}>
      <FinanceDashboard />
    </QueryClientProvider>
  )
  expect(screen.getByText('Financial access required')).toBeVisible()
  expect(get).not.toHaveBeenCalled()
})
it('retries a failed report without displaying a fabricated zero', async () => {
  const user = userEvent.setup()
  const get = serve()
  get.mockRejectedValueOnce(new Error('unavailable'))
  renderDashboard()
  await user.click(await screen.findByRole('button', { name: 'Retry' }))
  expect(
    await screen.findByText('922,337,203,685,477,580.800000001 EUR')
  ).toBeVisible()
  expect(get).toHaveBeenCalledTimes(2)
})
it.each(['account', 'session'])(
  'ignores late reports after an %s switch',
  async (kind) => {
    let resolveFirst: (value: {
      data: { success: boolean; data: ReturnType<typeof reportFixture> }
    }) => void = () => {}
    const first = new Promise<{
      data: { success: boolean; data: ReturnType<typeof reportFixture> }
    }>((resolve) => {
      resolveFirst = resolve
    })
    let firstFilter: FinanceFilter | undefined
    let firstSignal: { readonly aborted: boolean } | undefined
    const get = vi
      .spyOn(api, 'get')
      .mockImplementation(async (_url, config) => {
        const filter = config?.params as FinanceFilter
        if (!firstFilter) {
          firstFilter = filter
          firstSignal = config?.signal
          return first
        }
        const result = reportFixture(filter)
        result.totals.revenue = {
          known_amount: { nanos: '3000000000', currency: 'EUR' },
          total_amount: { nanos: '3000000000', currency: 'EUR' },
          pending_count: '0',
        }
        return { data: { success: true, data: result } }
      })
    renderDashboard()
    await waitFor(() => expect(get).toHaveBeenCalledTimes(1))
    act(() => {
      if (kind === 'account') useAuthStore.getState().auth.setUser(actor(2))
      else {
        useAuthStore.setState((state) => ({
          auth: { ...state.auth, session: session('second') },
        }))
      }
    })
    expect(await screen.findByText('3 EUR')).toBeVisible()
    expect(firstSignal?.aborted).toBe(true)
    if (!firstFilter) throw new Error('Initial report was not requested')
    const initialFilter = firstFilter
    await act(async () =>
      resolveFirst({
        data: { success: true, data: reportFixture(initialFilter) },
      })
    )
    expect(
      screen.queryByText('922,337,203,685,477,580.800000001 EUR')
    ).not.toBeInTheDocument()
  }
)
it('rejects an invalid timezone before an API call and applies a valid calendar grain', async () => {
  const user = userEvent.setup()
  const get = serve()
  renderDashboard()
  await screen.findByText('922,337,203,685,477,580.800000001 EUR')
  const zone = screen.getByLabelText('Report timezone')
  await user.clear(zone)
  await user.type(zone, 'Invalid/Timezone')
  expect(screen.getByRole('button', { name: 'Apply filters' })).toBeDisabled()
  expect(get).toHaveBeenCalledTimes(1)
  await user.clear(zone)
  await user.type(zone, 'America/New_York')
  await user.click(screen.getByRole('button', { name: 'Apply filters' }))
  await waitFor(() =>
    expect(get).toHaveBeenLastCalledWith(
      '/api/user/self/dashboard-finance',
      expect.objectContaining({
        params: expect.objectContaining({ timezone: 'America/New_York' }),
      })
    )
  )
})
it('renders request-count flows and retains exact values beyond safe integer geometry', async () => {
  const user = userEvent.setup()
  const get = vi.spyOn(api, 'get').mockImplementation(async (_url, config) => {
    const report = reportFixture(config?.params as FinanceFilter, true)
    report.totals.request_count = '18014398509481986'
    report.totals.succeeded_count = '18014398509481986'
    report.items.forEach((item) => {
      item.request_count = '9007199254740993'
      item.succeeded_count = '9007199254740993'
      item.names.token = '<img src=x onerror=alert(1)>'
    })
    return { data: { success: true, data: report } }
  })
  renderDashboard(true, 'flow')
  expect(
    await screen.findByText(
      'Flow widths represent nonnegative request counts, not money.'
    )
  ).toBeVisible()
  expect(
    screen.getByRole('table', { name: 'Financial report details' })
  ).toHaveTextContent('Same name (#7)')
  const edges = screen.getByRole('table', { name: 'Flow details' })
  expect(within(edges).getAllByText('9,007,199,254,740,993')).toHaveLength(8)
  expect(
    within(edges).getAllByText(/<img src=x onerror=alert\(1\)>/).length
  ).toBeGreaterThan(0)
  expect(within(edges).queryByRole('img')).not.toBeInTheDocument()
  await user.click(
    screen.getByRole('button', {
      name: 'Model Offering: Same name (#7): 9,007,199,254,740,993',
    })
  )
  await waitFor(() =>
    expect(get).toHaveBeenLastCalledWith(
      '/api/admin/v1/reports/dashboard-finance',
      expect.objectContaining({
        params: expect.objectContaining({ model_offering_id: '7' }),
      })
    )
  )
  expect(normalizedGeometry(10n ** 400n, 10n ** 400n)).toBe(1)
  expect(normalizedGeometry(-(10n ** 400n), 10n ** 400n)).toBe(-1)
})
it.each([
  'numeric money',
  'negative requests',
  'mixed currency',
  'invalid pending',
  'private self costs',
  'missing procurement',
  'invalid bucket',
  'unsafe dimension',
])(
  'rejects %s responses and retries without quota or zero fallback',
  async (invalid) => {
    const user = userEvent.setup()
    const admin = invalid === 'missing procurement'
    let broken = true
    vi.spyOn(api, 'get').mockImplementation(async (_url, config) => {
      const report = reportFixture(config?.params as FinanceFilter, admin)
      let data: unknown = report
      if (broken) {
        if (invalid === 'numeric money') {
          data = {
            ...report,
            totals: {
              ...report.totals,
              revenue: {
                ...report.totals.revenue,
                known_amount: { nanos: 9007199254740992, currency: 'EUR' },
              },
            },
          }
        }
        if (invalid === 'mixed currency') {
          report.items[0].revenue = metric('1', '0', 'JPY')
        }
        if (invalid === 'invalid pending') {
          report.totals.revenue = { ...metric('1'), pending_count: '1' }
        }
        if (invalid === 'negative requests') {
          report.items[0].request_count = '-1'
        }
        if (invalid === 'private self costs') report.totals.margin = metric('1')
        if (invalid === 'missing procurement') delete report.totals.procurement
        if (invalid === 'invalid bucket') report.buckets[0].end = report.from
        if (invalid === 'unsafe dimension') {
          report.items[0].dimensions.model_offering = 9007199254740992
        }
      }
      return { data: { success: true, data } }
    })
    renderDashboard(admin)
    const retry = await screen.findByRole('button', { name: 'Retry' })
    expect(screen.queryByText('0 EUR')).not.toBeInTheDocument()
    expect(
      screen.queryByText('922,337,203,685,477,580.800000001 EUR')
    ).not.toBeInTheDocument()
    broken = false
    await user.click(retry)
    expect(
      await screen.findByText('922,337,203,685,477,580.800000001 EUR')
    ).toBeVisible()
  }
)
it('uses server calendar boundaries and separates empty data from pending and signed corrections', async () => {
  const user = userEvent.setup()
  vi.spyOn(api, 'get').mockImplementation(async (_url, config) => {
    const report = reportFixture(config?.params as FinanceFilter)
    report.buckets[0] = { start: report.from - 1000, end: report.until + 1000 }
    report.items.forEach((item) => {
      item.bucket_start = report.buckets[0].start
      item.bucket_end = report.buckets[0].end
    })
    report.items[0].revenue = metric('-9007199254740993', '1')
    return { data: { success: true, data: report } }
  })
  renderDashboard()
  const table = await screen.findByRole('table', {
    name: 'Financial report details',
  })
  expect(within(table).getByText('-9,007,199.254740993 EUR')).toBeVisible()
  expect(
    within(table).getByText(
      /Known amount only · Pending requests: 1 · Total unavailable/
    )
  ).toBeVisible()
  expect(
    screen.queryByText('No requests in this range')
  ).not.toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Known revenue' }))
  expect(screen.getByRole('button', { name: 'Known revenue' })).toHaveAttribute(
    'aria-pressed',
    'true'
  )
  expect(screen.getByRole('button', { name: 'Requests' })).toHaveAttribute(
    'aria-pressed',
    'false'
  )
  await user.click(screen.getByRole('button', { name: 'Distribution' }))
  expect(screen.getByRole('button', { name: 'Distribution' })).toHaveAttribute(
    'aria-pressed',
    'true'
  )
  expect(within(table).getByText('-9,007,199.254740993 EUR')).toBeVisible()
})
it('keeps complete signed chart labels in a keyboard-accessible local scroll viewport', async () => {
  serve()
  renderDashboard()
  const table = await screen.findByRole('table', {
    name: 'Financial report details',
  })
  await userEvent.click(screen.getByRole('button', { name: 'Known revenue' }))
  await userEvent.click(screen.getByRole('button', { name: 'Distribution' }))
  const chart = screen.getByRole('region', { name: 'Bar Chart' })
  expect(chart).toHaveClass('max-w-full', 'overflow-x-auto')
  expect(chart).toHaveAttribute('tabindex', '0')
  expect(
    screen.getByText('Scroll horizontally to view the complete chart.')
  ).toBeVisible()
  expect(chart.querySelector('[data-slot="chart"]')).not.toHaveClass(
    'overflow-hidden'
  )
  expect(within(table).getByText('922,337,203,685,477,580.8 EUR')).toBeVisible()
  expect(
    screen.getByText('922,337,203,685,477,580.800000001 EUR')
  ).toBeVisible()
})

it('shows keyboard chart details outside the horizontal clipping region with exact native money', async () => {
  // JSDOM has no layout; only the browser geometry boundary is supplied here.
  // Real viewport clipping and moving/settled scroll are covered in Chromium.
  vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockReturnValue(
    new DOMRect(0, 0, 640, 288)
  )
  serve()
  renderDashboard()
  await screen.findByRole('table', { name: 'Financial report details' })
  await userEvent.click(screen.getByRole('button', { name: 'Known revenue' }))
  await userEvent.click(screen.getByRole('button', { name: 'Distribution' }))
  const chart = screen.getByRole('region', { name: 'Bar Chart' })
  fireEvent.keyDown(within(chart).getByRole('application'), {
    key: 'ArrowRight',
  })
  const tooltip = await screen.findByRole('tooltip')
  expect(tooltip).toHaveTextContent('Same name (#7)')
  expect(tooltip).toHaveTextContent('922,337,203,685,477,580.8 EUR')
  expect(tooltip.closest('[role="region"]')).toBeNull()
  expect(tooltip).toHaveClass('fixed', '[overflow-wrap:anywhere]')
  fireEvent.keyDown(within(chart).getByRole('application'), {
    key: 'ArrowRight',
  })
  await waitFor(() => expect(tooltip).toHaveTextContent('Same name (#8)'))
  expect(tooltip).toHaveTextContent('0.000000001 EUR')
})

it('paginates stable IDs and keeps long names and exact money readable in row details', async () => {
  const user = userEvent.setup()
  const label = `Offering ${'long-name-'.repeat(40)}`
  vi.spyOn(api, 'get').mockImplementation(async (_url, config) => {
    const report = reportFixture(config?.params as FinanceFilter)
    report.items = Array.from({ length: 21 }, (_, index) => ({
      ...report.items[0],
      dimensions: { model_offering: index + 1 },
      names: { model_offering: index === 20 ? label : `Offering ${index + 1}` },
    }))
    report.totals.request_count = '21'
    report.totals.succeeded_count = '21'
    return { data: { success: true, data: report } }
  })
  renderDashboard()
  let table = await screen.findByRole('table', {
    name: 'Financial report details',
  })
  expect(
    within(table).getAllByRole('button', { name: 'Details' })
  ).toHaveLength(20)
  expect(screen.getByRole('button', { name: 'Previous' })).toBeDisabled()
  await user.click(screen.getByRole('button', { name: 'Next' }))
  table = screen.getByRole('table', { name: 'Financial report details' })
  expect(
    within(table).getByRole('button', { name: `${label} (#21)` })
  ).toHaveClass('whitespace-normal', '[overflow-wrap:anywhere]')
  expect(screen.getByRole('button', { name: 'Next' })).toBeDisabled()
  await user.click(within(table).getByRole('button', { name: 'Details' }))
  const dialog = await screen.findByRole('dialog', {
    name: 'Financial report details',
  })
  expect(
    within(dialog).getByText(`Model Offering: ${label} (#21)`)
  ).toHaveClass('[overflow-wrap:anywhere]')
  expect(
    within(dialog).getByText('922,337,203,685,477,580.8 EUR')
  ).toBeVisible()
  await user.keyboard('{Escape}')
  await user.click(screen.getByRole('button', { name: 'Previous' }))
  expect(
    within(
      screen.getByRole('table', { name: 'Financial report details' })
    ).getAllByRole('button', { name: 'Details' })
  ).toHaveLength(20)
})
it('renders an empty successful cohort without inventing pending requests or a balance history', async () => {
  vi.spyOn(api, 'get').mockImplementation(async (_url, config) => {
    const report = reportFixture(config?.params as FinanceFilter)
    report.items = []
    report.totals = {
      request_count: '0',
      in_flight_count: '0',
      succeeded_count: '0',
      failed_count: '0',
      other_terminal_count: '0',
      revenue: metric(),
      collected: metric(),
      outstanding: metric(),
      usage: {},
    }
    return { data: { success: true, data: report } }
  })
  renderDashboard()
  expect(
    (await screen.findAllByText('No requests in this range')).length
  ).toBeGreaterThan(0)
  expect(screen.getAllByText('0 EUR')).toHaveLength(3)
  expect(screen.queryByText('Total unavailable')).not.toBeInTheDocument()
  expect(screen.queryByText('Current wallet position')).not.toBeInTheDocument()
})
it('updates unbounded signed amounts and fractional usage across real locale resources', async () => {
  const translated = createInstance()
  await translated.init({
    lng: 'en',
    fallbackLng: 'en',
    initAsync: false,
    resources: { en, fr, ja, ru, vi: viLocale, zhCN: zh, zhTW },
  })
  vi.spyOn(api, 'get').mockImplementation(async (_url, config) => {
    const report = reportFixture(config?.params as FinanceFilter)
    report.totals.revenue = metric('-922337203685477580800000001')
    report.totals.usage.audio_input_seconds = {
      known_total: '9007199254740993.000000001',
      total: null,
      missing_count: '1',
      estimated_count: '1',
    }
    return { data: { success: true, data: report } }
  })
  useAuthStore.getState().auth.setUser(actor())
  render(
    <I18nextProvider i18n={translated}>
      <QueryClientProvider client={client}>
        <FinanceDashboard />
      </QueryClientProvider>
    </I18nextProvider>
  )
  await screen.findByText('-922,337,203,685,477,580.800000001 EUR')
  for (const [language, amount, quantity] of [
    [
      'en',
      '-922,337,203,685,477,580.800000001 EUR',
      '9,007,199,254,740,993.000000001',
    ],
    [
      'zhCN',
      '-922,337,203,685,477,580.800000001 EUR',
      '9,007,199,254,740,993.000000001',
    ],
    [
      'zhTW',
      '-922,337,203,685,477,580.800000001 EUR',
      '9,007,199,254,740,993.000000001',
    ],
    [
      'fr',
      '-922 337 203 685 477 580,800000001 EUR',
      '9 007 199 254 740 993,000000001',
    ],
    [
      'ru',
      '-922 337 203 685 477 580,800000001 EUR',
      '9 007 199 254 740 993,000000001',
    ],
    [
      'ja',
      '-922,337,203,685,477,580.800000001 EUR',
      '9,007,199,254,740,993.000000001',
    ],
    [
      'vi',
      '-922.337.203.685.477.580,800000001 EUR',
      '9.007.199.254.740.993,000000001',
    ],
    [
      'invalid_locale_!',
      '-922,337,203,685,477,580.800000001 EUR',
      '9,007,199,254,740,993.000000001',
    ],
  ]) {
    await act(async () => translated.changeLanguage(language))
    expect(
      screen.getByText(amount, { normalizer: (value) => value })
    ).toBeVisible()
    expect(
      screen.getByText(quantity, { normalizer: (value) => value })
    ).toBeVisible()
  }
})
it('keeps repeated DST hours distinct and validates exact 31-day limits', () => {
  const first = bucketLabel(
    Date.parse('2026-11-01T05:00:00Z'),
    'America/New_York',
    'en-US'
  )
  const second = bucketLabel(
    Date.parse('2026-11-01T06:00:00Z'),
    'America/New_York',
    'en-US'
  )
  expect(first).not.toBe(second)
  expect(first).toContain('GMT-4')
  expect(second).toContain('GMT-5')
  const filter: FinanceFilter = {
    from: '2026-01-01T00:00:00.000Z',
    until: '2026-02-01T00:00:00.000Z',
    bucket: 'day',
    timezone: 'UTC',
  }
  expect(validFinanceFilter(filter, false)).toBe(true)
  expect(
    validFinanceFilter({ ...filter, until: '2026-02-01T00:00:00.001Z' }, false)
  ).toBe(false)
  expect(validFinanceFilter({ ...filter, until: filter.from }, false)).toBe(
    false
  )
  expect(exactQuantity('9007199254740993.000000001', 'fr-FR')).toBe(
    '9 007 199 254 740 993,000000001'
  )
})
