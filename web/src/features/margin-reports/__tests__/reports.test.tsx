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
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { MarginReports } from '../index'

let client: QueryClient
beforeEach(() =>
  useAuthStore.getState().auth.setUser({ id: 1, role: 100, username: 'root' })
)
afterEach(() => {
  useAuthStore.getState().auth.reset()
  client?.clear()
  vi.restoreAllMocks()
})
it('shows known amounts separately from unfinished margin and applies dimension and time filters', async () => {
  const get = vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        total: 1,
        items: [
          {
            model_offering_id: 17,
            channel_id: 31,
            provider_id: 41,
            date: '2025-01-02',
            revenue: { nanos: '1000000000', currency: 'USD' },
            cost: { nanos: '200000000', currency: 'USD' },
            collected: { nanos: '0', currency: 'USD' },
            outstanding: { nanos: '1000000000', currency: 'USD' },
            known_margin: { nanos: '800000000', currency: 'USD' },
            margin: null,
            sale_pending: 0,
            cost_pending: 1,
            converted_cost_pending: 1,
            original_costs: [{ nanos: '100000000', currency: 'EUR' }],
          },
        ],
      },
    },
  })
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(
    <QueryClientProvider client={client}>
      <MarginReports />
    </QueryClientProvider>
  )
  const result = await screen.findByText('Incomplete accounting')
  expect(result).toBeInTheDocument()
  const pageScroll = result.closest('main')?.querySelector('.overflow-auto')
  expect(pageScroll).toContainElement(result)
  expect(pageScroll).toContainElement(
    screen.getByRole('button', { name: 'Apply filters' })
  )
  expect(
    screen.getByRole('columnheader', { name: 'Known revenue' })
  ).toBeInTheDocument()
  const headers = screen
    .getAllByRole('columnheader')
    .map((header) => header.textContent)
  expect(headers.indexOf('Gross margin')).toBeLessThan(
    headers.indexOf('Known revenue')
  )
  const user = userEvent.setup()
  await user.type(screen.getByRole('textbox', { name: 'Provider ID' }), '41')
  await user.type(screen.getByLabelText('From date'), '2025-01-02')
  await user.click(screen.getByRole('button', { name: 'Apply filters' }))
  await waitFor(() =>
    expect(get).toHaveBeenLastCalledWith(
      '/api/admin/v1/reports/margin',
      expect.objectContaining({
        params: expect.objectContaining({
          provider_id: 41,
          from: '2025-01-02',
          timezone: 'UTC',
          page: 1,
        }),
      })
    )
  )
})

it.each([1, 0, undefined])(
  'preserves known gross margin while funding allocation is independent (%s)',
  async (pending) => {
    const amount = { nanos: '9007199254740993', currency: 'EUR' }
    vi.spyOn(api, 'get').mockResolvedValue({
      data: {
        success: true,
        data: {
          total: 1,
          items: [
            {
              model_offering_id: 17,
              revenue: amount,
              cost: { ...amount, nanos: '0' },
              collected: { ...amount, nanos: '0' },
              outstanding: { ...amount, nanos: '0' },
              known_margin: amount,
              margin: amount,
              original_costs: [],
              sale_pending: 0,
              cost_pending: 0,
              converted_cost_pending: 0,
              funding_pending: pending,
            },
          ],
        },
      },
    })
    client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    render(
      <QueryClientProvider client={client}>
        <MarginReports />
      </QueryClientProvider>
    )
    expect(
      (await screen.findAllByText('9,007,199.254740993 EUR')).length
    ).toBeGreaterThan(0)
    expect(screen.queryByText('Incomplete accounting')).not.toBeInTheDocument()
    if (pending === 1) {
      expect(screen.getByText('Funding allocation pending')).toBeVisible()
    } else {
      expect(
        screen.queryByText('Funding allocation pending')
      ).not.toBeInTheDocument()
    }
    if (pending === undefined) {
      expect(
        screen.getAllByText('Funding status unavailable').length
      ).toBeGreaterThan(0)
    }
  }
)
