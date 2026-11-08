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
import { afterEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import { MarginReports } from '../index'

let client: QueryClient
afterEach(() => {
  client?.clear()
  vi.restoreAllMocks()
})
it('shows known amounts separately from unfinished margin and applies dimension and time filters', async () => {
  const get = vi
    .spyOn(api, 'get')
    .mockResolvedValue({
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
  expect(await screen.findByText('Incomplete accounting')).toBeInTheDocument()
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
