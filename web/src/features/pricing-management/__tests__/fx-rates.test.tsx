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
import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { FxRates } from '../fx-rates'

const rate = {
  from_currency: 'EUR',
  to_currency: 'CNY',
  rate: '7.123456789012345678',
  buffer_rate: '0.02',
  updated_by: 1,
  updated_at: 1700000000,
}
const clients: QueryClient[] = []
afterEach(() => {
  clients.forEach((client) => client.clear())
  clients.length = 0
  vi.restoreAllMocks()
  localStorage.clear()
  useAuthStore.getState().auth.reset()
})
function renderPage(write = true) {
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
        <FxRates />
      </QueryClientProvider>
    ),
  }
}
function mockRates() {
  return vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/admin/v1/fx-rates') {
      return {
        data: {
          success: true,
          data: { items: [rate], total: 1, page: 1, page_size: 20 },
        },
      }
    }
    if (url === '/api/admin/v1/fx-rates/quote') {
      return {
        data: {
          success: true,
          data: {
            from_currency: 'EUR',
            to_currency: 'CNY',
            raw_rate: rate.rate,
            accounting_rate: rate.rate,
            comparison_rate: '7.26592592479259259156',
            buffer_rate: '0.02',
          },
        },
      }
    }
    throw new Error(`Unexpected GET ${url}`)
  })
}
it('shows server accounting and comparison quotes without truncating precision for read-only administrators', async () => {
  const get = mockRates()
  renderPage(false)
  expect(await screen.findByText(rate.rate)).toBeVisible()
  expect(
    screen.queryByRole('button', { name: /Maintain/ })
  ).not.toBeInTheDocument()
  await userEvent
    .setup()
    .click(screen.getByRole('button', { name: 'Quote EUR to CNY' }))
  const dialog = await screen.findByRole('dialog', {
    name: 'Exchange rate quote',
  })
  expect(
    await within(dialog).findByText('7.26592592479259259156')
  ).toBeVisible()
  expect(within(dialog).getByText('Accounting rate')).toBeVisible()
  expect(within(dialog).getByText(rate.rate)).toBeVisible()
  expect(get).toHaveBeenCalledWith('/api/admin/v1/fx-rates/quote', {
    params: { from_currency: 'EUR', to_currency: 'CNY' },
  })
})

it('preserves an explicit zero buffer and keeps a rejected save editable', async () => {
  mockRates()
  const put = vi.spyOn(api, 'put').mockResolvedValue({
    data: { success: false, message: 'Invalid exchange rate or buffer' },
  })
  renderPage()
  const user = userEvent.setup()
  await user.click(
    await screen.findByRole('button', { name: 'Maintain EUR to CNY rate' })
  )
  const dialog = await screen.findByRole('dialog', {
    name: 'Maintain exchange rate',
  })
  await user.clear(within(dialog).getByLabelText('Comparison buffer rate'))
  await user.type(within(dialog).getByLabelText('Comparison buffer rate'), '0')
  await user.click(
    within(dialog).getByRole('button', { name: 'Save exchange rate' })
  )
  await waitFor(() =>
    expect(put).toHaveBeenCalledWith('/api/admin/v1/fx-rates', {
      from_currency: 'EUR',
      to_currency: 'CNY',
      rate: rate.rate,
      buffer_rate: '0',
    })
  )
  expect(await within(dialog).findByRole('alert')).toHaveTextContent(
    'Invalid exchange rate or buffer'
  )
  expect(within(dialog).getByLabelText('Raw exchange rate')).toHaveValue(
    rate.rate
  )
  expect(within(dialog).getByLabelText('Comparison buffer rate')).toHaveValue(
    '0'
  )
})

it.each([
  ['Raw exchange rate', '0'],
  ['Raw exchange rate', '1000000000.000000000000000001'],
  ['Raw exchange rate', '0.1234567890123456789'],
  ['Raw exchange rate', '1e3'],
  ['Comparison buffer rate', '1.000000000000000001'],
])('blocks invalid exact decimal %s=%s before saving', async (label, value) => {
  mockRates()
  const put = vi.spyOn(api, 'put')
  renderPage()
  const user = userEvent.setup()
  await user.click(
    await screen.findByRole('button', { name: 'Maintain EUR to CNY rate' })
  )
  const dialog = await screen.findByRole('dialog', {
    name: 'Maintain exchange rate',
  })
  const input = within(dialog).getByLabelText(label)
  await user.clear(input)
  await user.type(input, value)
  await user.click(
    within(dialog).getByRole('button', { name: 'Save exchange rate' })
  )
  await waitFor(() => expect(input).toHaveAttribute('aria-invalid', 'true'))
  expect(put).not.toHaveBeenCalled()
})

it('retries a failed quote without displaying an invented inverse rate', async () => {
  const get = mockRates()
  const original = get.getMockImplementation()
  let failed = true
  get.mockImplementation(async (url, config) => {
    if (url.endsWith('/quote') && failed) throw new Error('Quote unavailable')
    return original?.(url, config)
  })
  renderPage(false)
  const user = userEvent.setup()
  await user.click(
    await screen.findByRole('button', { name: 'Quote EUR to CNY' })
  )
  const dialog = await screen.findByRole('dialog', {
    name: 'Exchange rate quote',
  })
  expect(
    await within(dialog).findByRole('button', { name: 'Retry' })
  ).toBeVisible()
  failed = false
  await user.click(await within(dialog).findByRole('button', { name: 'Retry' }))
  expect(
    await within(dialog).findByText('7.26592592479259259156')
  ).toBeVisible()
})

it('protects unsaved exchange-rate input when closing and canceling discard', async () => {
  mockRates()
  renderPage()
  const user = userEvent.setup()
  await user.click(
    await screen.findByRole('button', { name: 'Maintain EUR to CNY rate' })
  )
  const dialog = await screen.findByRole('dialog', {
    name: 'Maintain exchange rate',
  })
  await user.clear(within(dialog).getByLabelText('Raw exchange rate'))
  await user.type(
    within(dialog).getByLabelText('Raw exchange rate'),
    '8.123456789012345678'
  )
  await user.click(within(dialog).getByRole('button', { name: 'Cancel' }))
  const confirm = await screen.findByRole('alertdialog', {
    name: 'Discard unsaved changes?',
  })
  await user.click(within(confirm).getByRole('button', { name: 'Cancel' }))
  expect(within(dialog).getByLabelText('Raw exchange rate')).toHaveValue(
    '8.123456789012345678'
  )
})

it('rejects same-currency pairs with an accessible currency-field error', async () => {
  mockRates()
  const put = vi.spyOn(api, 'put')
  renderPage()
  const user = userEvent.setup()
  await user.click(
    screen.getByRole('button', { name: 'Maintain exchange rate' })
  )
  const dialog = await screen.findByRole('dialog', {
    name: 'Maintain exchange rate',
  })
  for (const label of ['From currency', 'To currency']) {
    await user.type(
      within(dialog).getByRole('combobox', { name: label }),
      'USD'
    )
    await user.tab()
  }
  await user.type(within(dialog).getByLabelText('Raw exchange rate'), '1')
  await user.click(
    within(dialog).getByRole('button', { name: 'Save exchange rate' })
  )
  const currency = within(dialog).getByRole('combobox', { name: 'To currency' })
  await waitFor(() => expect(currency).toHaveAttribute('aria-invalid', 'true'))
  expect(currency).toHaveAccessibleDescription(
    'Choose two different currencies.'
  )
  expect(put).not.toHaveBeenCalled()
})

it('blocks closing and duplicate saves while pending, then reopens the refreshed rate with a production-fresh cache', async () => {
  let currentRate = rate
  vi.spyOn(api, 'get').mockImplementation(async () => ({
    data: {
      success: true,
      data: { items: [currentRate], total: 1, page: 1, page_size: 20 },
    },
  }))
  let complete: (value: unknown) => void = () => undefined
  const put = vi.spyOn(api, 'put').mockImplementation(
    () =>
      new Promise((resolve) => {
        complete = resolve
      })
  )
  renderPage()
  const user = userEvent.setup()
  await user.click(
    await screen.findByRole('button', { name: 'Maintain EUR to CNY rate' })
  )
  const dialog = await screen.findByRole('dialog', {
    name: 'Maintain exchange rate',
  })
  const input = within(dialog).getByLabelText('Raw exchange rate')
  await user.clear(input)
  await user.type(input, '8.123456789012345678')
  await user.click(
    within(dialog).getByRole('button', { name: 'Save exchange rate' })
  )
  await waitFor(() =>
    expect(
      within(dialog).getByRole('button', { name: 'Saving...' })
    ).toBeDisabled()
  )
  expect(input).toBeDisabled()
  await user.keyboard('{Escape}')
  expect(dialog).toBeVisible()
  expect(put).toHaveBeenCalledTimes(1)
  currentRate = { ...rate, rate: '8.123456789012345678' }
  await act(async () =>
    complete({ data: { success: true, data: currentRate } })
  )
  await waitFor(() =>
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  )
  await user.click(
    await screen.findByRole('button', { name: 'Maintain EUR to CNY rate' })
  )
  expect(
    within(
      await screen.findByRole('dialog', { name: 'Maintain exchange rate' })
    ).getByLabelText('Raw exchange rate')
  ).toHaveValue('8.123456789012345678')
})

it('saves a precise directional exchange rate with the default comparison buffer', async () => {
  mockRates()
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: true, data: rate } })
  renderPage()
  const user = userEvent.setup()
  await user.click(
    screen.getByRole('button', { name: 'Maintain exchange rate' })
  )
  const dialog = await screen.findByRole('dialog', {
    name: 'Maintain exchange rate',
  })
  for (const [label, value] of [
    ['From currency', 'EUR'],
    ['To currency', 'CNY'],
  ]) {
    await user.type(
      within(dialog).getByRole('combobox', { name: label }),
      value
    )
    await user.tab()
  }
  await user.type(within(dialog).getByLabelText('Raw exchange rate'), rate.rate)
  expect(within(dialog).getByLabelText('Comparison buffer rate')).toHaveValue(
    '0.02'
  )
  await user.click(
    within(dialog).getByRole('button', { name: 'Save exchange rate' })
  )
  await waitFor(() =>
    expect(put).toHaveBeenCalledWith('/api/admin/v1/fx-rates', {
      from_currency: 'EUR',
      to_currency: 'CNY',
      rate: rate.rate,
      buffer_rate: '0.02',
    })
  )
  await waitFor(() =>
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  )
})
