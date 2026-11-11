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
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { AxiosError } from 'axios'
import { afterEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { PriceVersions } from '../price-versions'

const version = {
  id: 3,
  owner_type: 'model_offering',
  owner_id: 9,
  currency: 'CNY',
  expression: 'v1:tier("base", p * 2.5 + c * 10)',
  effective_at: 1800000000,
  created_by: 1,
  created_at: 1700000000,
}
const clients: QueryClient[] = []
afterEach(() => {
  clients.forEach((client) => client.clear())
  clients.length = 0
  vi.restoreAllMocks()
  vi.useRealTimers()
  localStorage.clear()
  useAuthStore.getState().auth.reset()
})

function renderPage(write = false) {
  useAuthStore.getState().auth.setUser({
    id: 1,
    username: 'admin',
    role: 10,
    permissions: { admin_permissions: { pricing: { read: true, write } } },
  })
  const client = new QueryClient({
    defaultOptions: {
      queries: { retry: false, staleTime: 10_000 },
      mutations: { retry: false },
    },
  })
  clients.push(client)
  return {
    client,
    ...render(
      <QueryClientProvider client={client}>
        <PriceVersions />
      </QueryClientProvider>
    ),
  }
}

function mockPrices(forbiddenPath?: string) {
  return vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === forbiddenPath) {
      throw Object.assign(new AxiosError('Forbidden'), {
        response: { status: 403 },
      })
    }
    if (url === '/api/admin/v1/price-versions/current') {
      return { data: { success: true, data: version } }
    }
    if (url === '/api/admin/v1/price-versions') {
      return {
        data: {
          success: true,
          data: { items: [version], total: 1, page: 1, page_size: 20 },
        },
      }
    }
    if (url === '/api/admin/v1/model-offerings') {
      return {
        data: {
          success: true,
          data: { items: [{ id: 9, name: 'assistant' }], total: 1 },
        },
      }
    }
    if (url === '/api/admin/v1/channels') {
      return {
        data: {
          success: true,
          data: {
            items: [
              {
                id: 12,
                upstream_model_name: 'upstream-a',
                protocol: 'openai',
                provider_id: 3,
              },
            ],
            total: 1,
          },
        },
      }
    }
    throw new Error(`Unexpected GET ${url}`)
  })
}

it('keeps all thirty margin combinations and price history reachable through the page scroll region', async () => {
  const amount = { nanos: '1', currency: 'USD' }
  vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/admin/v1/price-versions/margins') {
      return {
        data: {
          success: true,
          data: {
            estimates: Array.from({ length: 30 }, (_, index) => ({
              model_offering_id: 9,
              channel_id: 100 + index,
              provider_id: 41,
              samples: 2,
              revenue: amount,
              cost: amount,
              margin: amount,
              negative: index === 29,
              status: 'estimated',
            })),
          },
        },
      }
    }
    return { data: { success: true, data: { items: [version], total: 1 } } }
  })
  renderPage()
  const toggle = screen.getByRole('button', {
    name: 'Estimated channel margins',
  })
  await userEvent.click(toggle)
  expect(toggle).toHaveAttribute('aria-expanded', 'true')
  const region = screen.getByRole('region', {
    name: 'Estimated channel margins',
  })
  const table = await within(region).findByRole('table')
  expect(within(table).getAllByRole('row')).toHaveLength(31)
  expect(within(table).getByText('#129')).toBeVisible()
  region.focus()
  expect(region).toHaveFocus()
  const filter = screen.getByRole('button', { name: 'Apply owner filter' })
  const pageScroll = region.closest('main')?.querySelector('.overflow-auto')
  expect(pageScroll).toContainElement(region)
  expect(pageScroll).toContainElement(filter)
  filter.focus()
  expect(filter).toHaveFocus()
  expect(screen.getByText(version.expression)).toBeVisible()
})

it('requires an explicit loss confirmation and resubmits the exact rejected draft and server revision', async () => {
  mockPrices()
  const report = {
    revision: 'frozen-margin',
    effective_at: 1800000000,
    requires_confirmation: true,
    estimates: [
      {
        model_offering_id: 9,
        channel_id: 12,
        provider_id: 3,
        samples: 7,
        status: 'estimated',
        negative: true,
        revenue: { nanos: '1000000000', currency: 'USD' },
        cost: { nanos: '2000000000', currency: 'USD' },
        margin: { nanos: '-1000000000', currency: 'USD' },
      },
    ],
  }
  const post = vi
    .spyOn(api, 'post')
    .mockRejectedValueOnce(
      Object.assign(new AxiosError('Conflict'), {
        response: {
          status: 409,
          data: {
            success: false,
            message_key: 'Negative margin requires confirmation',
            data: report,
          },
        },
      })
    )
    .mockResolvedValue({ data: { success: true, data: version } })
  renderPage(true)
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'New price version' }))
  const dialog = await screen.findByRole('dialog', {
    name: 'New price version',
  })
  await user.type(
    within(dialog).getByRole('combobox', { name: 'Price owner ID' }),
    '9'
  )
  await user.tab()
  await user.click(
    within(dialog).getByRole('button', { name: 'Create price version' })
  )
  const approval = await screen.findByRole('alertdialog', {
    name: 'Negative margin requires confirmation',
  })
  expect(post).toHaveBeenCalledTimes(1)
  expect(
    within(approval)
      .getAllByRole('columnheader')
      .slice(0, 3)
      .map((header) => header.textContent)
  ).toContain('Gross margin')
  await user.click(
    within(approval).getByRole('button', { name: 'Confirm loss' })
  )
  await waitFor(() =>
    expect(post).toHaveBeenLastCalledWith(
      '/api/admin/v1/price-versions',
      expect.objectContaining({
        owner_id: 9,
        effective_at: 1800000000,
        margin_confirmation: 'frozen-margin',
      })
    )
  )
})

it('creates the explicitly displayed zero price without requiring an unrelated input edit', async () => {
  mockPrices()
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValue({ data: { success: true, data: version } })
  renderPage(true)
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'New price version' }))
  const dialog = await screen.findByRole('dialog', {
    name: 'New price version',
  })
  await user.type(
    within(dialog).getByRole('combobox', { name: 'Price owner ID' }),
    '9'
  )
  await user.tab()
  expect(
    within(dialog).getByRole('textbox', { name: 'Input price' })
  ).toHaveValue('0')
  await user.click(
    within(dialog).getByRole('button', { name: 'Create price version' })
  )
  await waitFor(() =>
    expect(post).toHaveBeenCalledWith(
      '/api/admin/v1/price-versions',
      expect.objectContaining({ expression: 'tier("base", p * 0 + c * 0)' })
    )
  )
})

it('creates a native-currency version with server time despite a skewed browser clock and preserves exact source', async () => {
  vi.useFakeTimers({ toFake: ['Date'] })
  vi.setSystemTime(new Date(2099, 0, 1))
  mockPrices()
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValue({ data: { success: true, data: version } })
  renderPage(true)
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'New price version' }))
  const dialog = await screen.findByRole('dialog', {
    name: 'New price version',
  })
  await user.type(
    within(dialog).getByRole('combobox', { name: 'Price owner ID' }),
    '9'
  )
  await user.tab()
  await user.clear(within(dialog).getByRole('combobox', { name: 'Currency' }))
  await user.type(
    within(dialog).getByRole('combobox', { name: 'Currency' }),
    'EUR'
  )
  await user.tab()
  await user.click(
    within(dialog).getByRole('combobox', { name: 'Editor mode' })
  )
  await user.click(
    await screen.findByRole('option', { name: 'Expression editor' })
  )
  const expression = '  v1:tier("precise", p * 0.123456789012345678 + c * 0)  '
  await user.clear(within(dialog).getByLabelText('Billing expression'))
  await user.type(
    within(dialog).getByLabelText('Billing expression'),
    expression
  )
  await user.clear(within(dialog).getByRole('combobox', { name: 'Currency' }))
  await user.type(
    within(dialog).getByRole('combobox', { name: 'Currency' }),
    'CNY'
  )
  await user.tab()
  expect(within(dialog).getByLabelText('Billing expression')).toHaveValue(
    expression
  )
  expect(
    within(dialog).queryByText(/Raw expressions and presets use USD/)
  ).not.toBeInTheDocument()
  await user.click(
    within(dialog).getByRole('button', { name: 'Create price version' })
  )
  await waitFor(() =>
    expect(post).toHaveBeenCalledWith('/api/admin/v1/price-versions', {
      owner_type: 'model_offering',
      owner_id: 9,
      currency: 'CNY',
      expression,
    })
  )
  await waitFor(() =>
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  )
})

it('preserves precise native-currency coefficients entered through the visual editor', async () => {
  mockPrices()
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValue({ data: { success: true, data: version } })
  renderPage(true)
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'New price version' }))
  const dialog = await screen.findByRole('dialog', {
    name: 'New price version',
  })
  await user.type(
    within(dialog).getByRole('combobox', { name: 'Price owner ID' }),
    '9'
  )
  await user.tab()
  const input = within(dialog).getByRole('textbox', { name: 'Input price' })
  await user.clear(input)
  await user.type(input, '0.123456789012345678')
  await user.click(
    within(dialog).getByRole('button', { name: 'Create price version' })
  )
  await waitFor(() =>
    expect(post).toHaveBeenCalledWith(
      '/api/admin/v1/price-versions',
      expect.objectContaining({
        expression: expect.stringContaining('p * 0.123456789012345678'),
      })
    )
  )
})

it('submits a scheduled activation in Unix seconds and retains the draft after server rejection', async () => {
  vi.useFakeTimers({ toFake: ['Date'] })
  vi.setSystemTime(new Date(2026, 5, 15, 10))
  mockPrices()
  const post = vi.spyOn(api, 'post').mockResolvedValue({
    data: {
      success: false,
      message: 'A price version already exists at this effective time',
    },
  })
  renderPage(true)
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'New price version' }))
  const dialog = await screen.findByRole('dialog', {
    name: 'New price version',
  })
  await user.type(
    within(dialog).getByRole('combobox', { name: 'Price owner ID' }),
    '9'
  )
  await user.tab()
  await user.selectOptions(
    within(dialog).getByLabelText('Activation time'),
    'scheduled'
  )
  await user.click(within(dialog).getByLabelText('Effective at'))
  await user.click(screen.getByRole('button', { name: 'Go to the Next Month' }))
  await user.click(screen.getByRole('button', { name: /July 16th, 2026/ }))
  fireEvent.change(within(dialog).getByLabelText('Time'), {
    target: { value: '08:30:45' },
  })
  const input = within(dialog).getByRole('textbox', { name: 'Input price' })
  await user.clear(input)
  await user.type(input, '0')
  await user.click(
    within(dialog).getByRole('button', { name: 'Create price version' })
  )
  await waitFor(() =>
    expect(post).toHaveBeenCalledWith(
      '/api/admin/v1/price-versions',
      expect.objectContaining({
        effective_at: Math.floor(
          new Date(2026, 6, 16, 8, 30, 45).getTime() / 1000
        ),
      })
    )
  )
  expect(await within(dialog).findByRole('alert')).toHaveTextContent(
    'A price version already exists at this effective time'
  )
  expect(input).toHaveValue('0')
  expect(within(dialog).getByLabelText('Time')).toHaveValue('08:30:45')
})

it('loads the server current version for one owner independently of scheduled history', async () => {
  const get = mockPrices()
  renderPage()
  const user = userEvent.setup()
  await user.selectOptions(
    screen.getByLabelText('Price owner type'),
    'model_offering'
  )
  await user.type(screen.getByLabelText('Price owner ID'), '9')
  await user.click(screen.getByRole('button', { name: 'Apply owner filter' }))
  expect(await screen.findByText('Current price version')).toBeVisible()
  await waitFor(() =>
    expect(get).toHaveBeenCalledWith('/api/admin/v1/price-versions/current', {
      params: { owner_type: 'model_offering', owner_id: 9 },
    })
  )
  expect(await screen.findByText('#3 · CNY')).toBeVisible()
})

it('retains all exact coefficients when switching a native expression into visual editing', async () => {
  mockPrices()
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValue({ data: { success: true, data: version } })
  renderPage(true)
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'New price version' }))
  const dialog = await screen.findByRole('dialog', {
    name: 'New price version',
  })
  await user.click(
    within(dialog).getByRole('combobox', { name: 'Editor mode' })
  )
  await user.click(
    await screen.findByRole('option', { name: 'Expression editor' })
  )
  await user.clear(within(dialog).getByLabelText('Billing expression'))
  await user.type(
    within(dialog).getByLabelText('Billing expression'),
    'v1:tier("base", p * 0.123456789012345678 + c * 0)'
  )
  await user.click(
    within(dialog).getByRole('combobox', { name: 'Editor mode' })
  )
  await user.click(await screen.findByRole('option', { name: 'Visual editor' }))
  const price = within(dialog).getByRole('textbox', { name: 'Input price' })
  expect(price).toHaveValue('0.123456789012345678')
  await user.clear(price)
  await user.type(price, '0.123456789012345679')
  await user.type(
    within(dialog).getByRole('combobox', { name: 'Price owner ID' }),
    '9'
  )
  await user.tab()
  await user.clear(within(dialog).getByRole('combobox', { name: 'Currency' }))
  await user.type(
    within(dialog).getByRole('combobox', { name: 'Currency' }),
    'EUR'
  )
  await user.tab()
  expect(price).toHaveValue('0.123456789012345679')
  await user.click(
    within(dialog).getByRole('button', { name: 'Create price version' })
  )
  await waitFor(() =>
    expect(post).toHaveBeenCalledWith(
      '/api/admin/v1/price-versions',
      expect.objectContaining({
        currency: 'EUR',
        expression: 'v1:tier("base", p * 0.123456789012345679 + c * 0)',
      })
    )
  )
})

it.each(['/api/admin/v1/model-offerings', '/api/admin/v1/channels'])(
  'keeps price creation usable when %s owner names are forbidden',
  async (path) => {
    mockPrices(path)
    const post = vi
      .spyOn(api, 'post')
      .mockResolvedValue({ data: { success: true, data: version } })
    renderPage(true)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'New price version' }))
    const dialog = await screen.findByRole('dialog', {
      name: 'New price version',
    })
    if (path.endsWith('channels')) {
      await user.selectOptions(
        within(dialog).getByLabelText('Price owner type'),
        'channel'
      )
    }
    expect(
      await within(dialog).findByText(
        'Owner names are unavailable with your permissions. Enter the owner ID to continue.'
      )
    ).toBeVisible()
    await user.type(
      within(dialog).getByRole('combobox', { name: 'Price owner ID' }),
      '12'
    )
    await user.tab()
    await user.type(
      within(dialog).getByRole('textbox', { name: 'Input price' }),
      '1'
    )
    await user.click(
      within(dialog).getByRole('button', { name: 'Create price version' })
    )
    await waitFor(() =>
      expect(post).toHaveBeenCalledWith(
        '/api/admin/v1/price-versions',
        expect.objectContaining({
          owner_id: 12,
          owner_type: path.endsWith('channels') ? 'channel' : 'model_offering',
        })
      )
    )
  }
)

it('reopens a fresh-cached owner form and retains dirty inputs across background owner failures', async () => {
  const get = mockPrices()
  const { client } = renderPage(true)
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'New price version' }))
  await user.click(
    within(
      await screen.findByRole('dialog', { name: 'New price version' })
    ).getByRole('button', { name: 'Cancel' })
  )
  await waitFor(() =>
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  )
  await user.click(screen.getByRole('button', { name: 'New price version' }))
  const dialog = await screen.findByRole('dialog', {
    name: 'New price version',
  })
  await user.type(
    within(dialog).getByRole('combobox', { name: 'Price owner ID' }),
    '9'
  )
  await user.tab()
  get.mockRejectedValue(new Error('Owner service unavailable'))
  await client.invalidateQueries({ queryKey: ['price-owners'] })
  expect(
    await within(dialog).findByRole('button', { name: 'Retry' })
  ).toBeVisible()
  expect(
    within(dialog).getByRole('combobox', { name: 'Price owner ID' })
  ).toHaveValue('#9 · assistant')
  await user.click(within(dialog).getByRole('button', { name: 'Cancel' }))
  const confirm = await screen.findByRole('alertdialog', {
    name: 'Discard unsaved changes?',
  })
  await user.click(within(confirm).getByRole('button', { name: 'Cancel' }))
  expect(
    within(dialog).getByRole('combobox', { name: 'Price owner ID' })
  ).toHaveValue('#9 · assistant')
})

it('shows no currently effective price when the current endpoint returns 404', async () => {
  const get = mockPrices()
  const original = get.getMockImplementation()
  get.mockImplementation(async (url, config) => {
    if (url.endsWith('/current')) {
      throw Object.assign(new AxiosError('Not found'), {
        response: { status: 404 },
      })
    }
    return original?.(url, config)
  })
  renderPage()
  const user = userEvent.setup()
  await user.selectOptions(
    screen.getByLabelText('Price owner type'),
    'model_offering'
  )
  await user.type(screen.getByLabelText('Price owner ID'), '9')
  await user.click(screen.getByRole('button', { name: 'Apply owner filter' }))
  expect(
    await screen.findByText('No price is currently effective for this owner.')
  ).toBeVisible()
})

it('refreshes an unfiltered scheduled status when its activation boundary passes', async () => {
  vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval'] })
  vi.setSystemTime(new Date((version.effective_at - 1) * 1000))
  mockPrices()
  renderPage()
  expect(await screen.findByText('Scheduled')).toBeVisible()
  await act(async () => {
    await vi.advanceTimersByTimeAsync(1100)
  })
  expect(screen.queryByText('Scheduled')).not.toBeInTheDocument()
  expect(screen.getByText('Effective')).toBeVisible()
})

it('blocks saving a request multiplier with an incomplete numeric restriction and protects the draft on close', async () => {
  mockPrices()
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValue({ data: { success: true, data: version } })
  renderPage(true)
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'New price version' }))
  const dialog = await screen.findByRole('dialog', {
    name: 'New price version',
  })
  await user.type(
    within(dialog).getByRole('combobox', { name: 'Price owner ID' }),
    '9'
  )
  await user.tab()
  await user.click(
    within(dialog).getByRole('button', { name: 'Add rule group' })
  )
  await user.type(
    within(dialog).getByPlaceholderText('service_tier'),
    'service_tier'
  )
  await user.type(within(dialog).getByPlaceholderText('Value'), 'fast')
  await user.click(
    within(dialog).getByRole('button', { name: 'Add param/header' })
  )
  await user.type(
    within(dialog).getAllByPlaceholderText('service_tier')[1],
    'context_length'
  )
  await user.click(within(dialog).getAllByText('Equals')[1])
  await user.click(await screen.findByRole('option', { name: 'Greater than' }))
  await user.type(
    within(dialog).getAllByPlaceholderText('Value')[1],
    'unfinished'
  )
  await user.clear(within(dialog).getByPlaceholderText('1.0'))
  await user.type(within(dialog).getByPlaceholderText('1.0'), '2')
  await user.click(
    within(dialog).getByRole('button', { name: 'Create price version' })
  )
  expect(
    await within(dialog).findByText(
      'Complete the invalid expression fields before saving.'
    )
  ).toBeVisible()
  expect(post).not.toHaveBeenCalled()
  await user.click(within(dialog).getByRole('button', { name: 'Cancel' }))
  const confirm = await screen.findByRole('alertdialog', {
    name: 'Discard unsaved changes?',
  })
  await user.click(within(confirm).getByRole('button', { name: 'Cancel' }))
  expect(within(dialog).getAllByPlaceholderText('Value')[1]).toHaveValue(
    'unfinished'
  )
})

it('keeps the version prefix at the start when adding visual request rules', async () => {
  mockPrices()
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValue({ data: { success: true, data: version } })
  renderPage(true)
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'New price version' }))
  const dialog = await screen.findByRole('dialog', {
    name: 'New price version',
  })
  await user.type(
    within(dialog).getByRole('combobox', { name: 'Price owner ID' }),
    '9'
  )
  await user.tab()
  await user.click(
    within(dialog).getByRole('combobox', { name: 'Editor mode' })
  )
  await user.click(
    await screen.findByRole('option', { name: 'Expression editor' })
  )
  await user.clear(within(dialog).getByLabelText('Billing expression'))
  await user.type(
    within(dialog).getByLabelText('Billing expression'),
    'v1:tier("base", p * 2 + c * 0)'
  )
  await user.click(
    within(dialog).getByRole('combobox', { name: 'Editor mode' })
  )
  await user.click(await screen.findByRole('option', { name: 'Visual editor' }))
  await user.click(
    within(dialog).getByRole('button', { name: 'Add rule group' })
  )
  await user.type(
    within(dialog).getByPlaceholderText('service_tier'),
    'service_tier'
  )
  await user.type(within(dialog).getByPlaceholderText('Value'), 'fast')
  await user.type(within(dialog).getByPlaceholderText('1.0'), '2')
  await user.click(
    within(dialog).getByRole('button', { name: 'Create price version' })
  )
  await waitFor(() =>
    expect(post).toHaveBeenCalledWith(
      '/api/admin/v1/price-versions',
      expect.objectContaining({
        expression:
          'v1:(tier("base", p * 2 + c * 0)) * (param("service_tier") == "fast" ? 2 : 1)',
      })
    )
  )
})

it.each([
  ['an empty', ''],
  ['an exact decimal', '1.000000000000000001'],
])(
  'preserves %s multiplier draft instead of silently changing its value',
  async (_label, draft) => {
    mockPrices()
    const post = vi
      .spyOn(api, 'post')
      .mockResolvedValue({ data: { success: true, data: version } })
    renderPage(true)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'New price version' }))
    const dialog = await screen.findByRole('dialog', {
      name: 'New price version',
    })
    await user.type(
      within(dialog).getByRole('combobox', { name: 'Price owner ID' }),
      '9'
    )
    await user.tab()
    await user.click(
      within(dialog).getByRole('button', { name: 'Add rule group' })
    )
    await user.type(
      within(dialog).getByPlaceholderText('service_tier'),
      'service_tier'
    )
    await user.type(within(dialog).getByPlaceholderText('Value'), 'fast')
    const multiplier = within(dialog).getByRole('textbox', {
      name: 'Multiplier',
    })
    await user.type(multiplier, '2')
    await user.clear(multiplier)
    if (draft) await user.type(multiplier, draft)
    await user.click(
      within(dialog).getByRole('button', { name: 'Create price version' })
    )
    if (!draft) {
      expect(
        await within(dialog).findByText(
          'Complete the invalid expression fields before saving.'
        )
      ).toBeVisible()
      expect(post).not.toHaveBeenCalled()
      expect(multiplier).toHaveValue('')
      await user.type(multiplier, '0')
      await user.click(
        within(dialog).getByRole('button', { name: 'Create price version' })
      )
    }
    await waitFor(() =>
      expect(post).toHaveBeenCalledWith(
        '/api/admin/v1/price-versions',
        expect.objectContaining({
          expression: expect.stringContaining(
            `param("service_tier") == "fast" ? ${draft || '0'} : 1`
          ),
        })
      )
    )
  }
)

it.each([
  ['param', '"1"'],
  ['param', '"true"'],
  ['param', '1'],
  ['param', 'true'],
  ['param', '1.000000000000000001'],
  ['header', '"1"'],
])(
  'preserves %s condition literal %s when editing its multiplier and saving',
  async (sourceKind, literal) => {
    mockPrices()
    const post = vi
      .spyOn(api, 'post')
      .mockResolvedValue({ data: { success: true, data: version } })
    renderPage(true)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'New price version' }))
    const dialog = await screen.findByRole('dialog', {
      name: 'New price version',
    })
    await user.type(
      within(dialog).getByRole('combobox', { name: 'Price owner ID' }),
      '9'
    )
    await user.tab()
    await user.click(
      within(dialog).getByRole('combobox', { name: 'Editor mode' })
    )
    await user.click(
      await screen.findByRole('option', { name: 'Expression editor' })
    )
    await user.clear(within(dialog).getByLabelText('Billing expression'))
    await user.type(
      within(dialog).getByLabelText('Billing expression'),
      `v1:(tier("base", p * 1 + c * 0)) * (${sourceKind}("tier") == ${literal} ? 2 : 1)`
    )
    await user.click(
      within(dialog).getByRole('combobox', { name: 'Editor mode' })
    )
    await user.click(
      await screen.findByRole('option', { name: 'Visual editor' })
    )
    await user.clear(within(dialog).getByPlaceholderText('1.0'))
    await user.type(within(dialog).getByPlaceholderText('1.0'), '3')
    await user.click(
      within(dialog).getByRole('button', { name: 'Create price version' })
    )
    await waitFor(() =>
      expect(post).toHaveBeenCalledWith(
        '/api/admin/v1/price-versions',
        expect.objectContaining({
          expression: expect.stringContaining(
            `${sourceKind}("tier") == ${literal} ? 3 : 1`
          ),
        })
      )
    )
  }
)

it('keeps native request-rule expressions visually editable while retaining the exact raw source', async () => {
  mockPrices()
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValue({ data: { success: true, data: version } })
  renderPage(true)
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'New price version' }))
  const dialog = await screen.findByRole('dialog', {
    name: 'New price version',
  })
  await user.type(
    within(dialog).getByRole('combobox', { name: 'Price owner ID' }),
    '9'
  )
  await user.tab()
  await user.click(
    within(dialog).getByRole('combobox', { name: 'Editor mode' })
  )
  await user.click(
    await screen.findByRole('option', { name: 'Expression editor' })
  )
  const source =
    '  v1:(tier("base", p * 0.123456789012345678 + c * 0)) * (param("service_tier") == "fast" ? 2 : 1)  '
  await user.clear(within(dialog).getByLabelText('Billing expression'))
  await user.paste(source)
  await user.click(
    within(dialog).getByRole('combobox', { name: 'Editor mode' })
  )
  await user.click(await screen.findByRole('option', { name: 'Visual editor' }))
  expect(
    within(dialog).getByRole('textbox', { name: 'Input price' })
  ).toHaveValue('0.123456789012345678')
  expect(within(dialog).getByPlaceholderText('Value')).toHaveValue('fast')
  expect(within(dialog).getByPlaceholderText('1.0')).toHaveValue('2')
  await user.click(
    within(dialog).getByRole('combobox', { name: 'Editor mode' })
  )
  await user.click(
    await screen.findByRole('option', { name: 'Expression editor' })
  )
  expect(within(dialog).getByLabelText('Billing expression')).toHaveValue(
    source
  )
  await user.click(
    within(dialog).getByRole('button', { name: 'Create price version' })
  )
  await waitFor(() =>
    expect(post).toHaveBeenCalledWith(
      '/api/admin/v1/price-versions',
      expect.objectContaining({ expression: source })
    )
  )
})

it.each([
  ['v1:tier("base", p * (0.123456789012345678) + c * 0)', 'Input price'],
  ['v1:tier("base", fixed((0.123456789012345678)))', 'Price per request'],
])(
  'edits grouped numeric literals without losing their exact digits: %s',
  async (source, label) => {
    mockPrices()
    const post = vi
      .spyOn(api, 'post')
      .mockResolvedValue({ data: { success: true, data: version } })
    renderPage(true)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'New price version' }))
    const dialog = await screen.findByRole('dialog', {
      name: 'New price version',
    })
    await user.type(
      within(dialog).getByRole('combobox', { name: 'Price owner ID' }),
      '9'
    )
    await user.tab()
    await user.click(
      within(dialog).getByRole('combobox', { name: 'Editor mode' })
    )
    await user.click(
      await screen.findByRole('option', { name: 'Expression editor' })
    )
    await user.clear(within(dialog).getByLabelText('Billing expression'))
    await user.type(within(dialog).getByLabelText('Billing expression'), source)
    await user.click(
      within(dialog).getByRole('combobox', { name: 'Editor mode' })
    )
    await user.click(
      await screen.findByRole('option', { name: 'Visual editor' })
    )
    const input = within(dialog).getByRole('textbox', { name: label })
    expect(input).toHaveValue('0.123456789012345678')
    await user.clear(input)
    await user.type(input, '0.123456789012345679')
    await user.click(
      within(dialog).getByRole('button', { name: 'Create price version' })
    )
    await waitFor(() =>
      expect(post).toHaveBeenCalledWith(
        '/api/admin/v1/price-versions',
        expect.objectContaining({
          expression: source.replace(
            '0.123456789012345678',
            '0.123456789012345679'
          ),
        })
      )
    )
  }
)

it('lists immutable price history and filters by owner through the pricing API', async () => {
  const get = vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: { items: [version], total: 1, page: 1, page_size: 20 },
    },
  })
  renderPage()
  await waitFor(() =>
    expect(screen.getByText(version.expression)).toBeVisible()
  )
  expect(screen.getByText('CNY')).toBeVisible()
  expect(
    screen.queryByRole('button', { name: 'New price version' })
  ).not.toBeInTheDocument()
  expect(
    screen.queryByRole('button', { name: /delete|edit/i })
  ).not.toBeInTheDocument()
  const user = userEvent.setup()
  await user.selectOptions(screen.getByLabelText('Price owner type'), 'channel')
  await user.type(screen.getByLabelText('Price owner ID'), '12')
  await user.click(screen.getByRole('button', { name: 'Apply owner filter' }))
  await waitFor(() =>
    expect(get).toHaveBeenCalledWith('/api/admin/v1/price-versions', {
      params: { p: 1, page_size: 20, owner_type: 'channel', owner_id: 12 },
    })
  )
})
