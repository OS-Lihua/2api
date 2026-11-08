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
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import { BulkImportDialog } from '../components/bulk-import-dialog'

let client: QueryClient

afterEach(() => {
  client?.clear()
  vi.restoreAllMocks()
})

it('previews a losing batch and requires explicit approval with the frozen revision before execution', async () => {
  const report = {
    revision: 'a'.repeat(64),
    effective_at: 1800000000,
    requires_confirmation: true,
    estimates: [
      {
        model_offering_id: 9,
        channel_id: 31,
        provider_id: 41,
        samples: 0,
        status: 'estimated',
        negative: true,
        revenue: { nanos: '1000000000', currency: 'USD' },
        cost: { nanos: '2000000000', currency: 'USD' },
        margin: { nanos: '-1000000000', currency: 'USD' },
      },
    ],
  }
  const preview = {
    dry_run: true,
    valid: true,
    committed: false,
    rows: [{ row: 1, resource: 'price-versions', action: 'create' }],
    margin_check: report,
  }
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValueOnce({ data: { success: true, data: preview } })
    .mockResolvedValue({
      data: {
        success: true,
        data: { ...preview, dry_run: false, committed: true },
      },
    })
  client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  render(
    <QueryClientProvider client={client}>
      <BulkImportDialog open onOpenChange={vi.fn()} />
    </QueryClientProvider>
  )
  const source =
    '{"rows":[{"resource":"price-versions","action":"create","data":{"owner_type":"channel","owner_id":31,"currency":"USD","expression":"fixed(2)"}}]}'
  fireEvent.change(screen.getByLabelText('Import content'), {
    target: { value: source },
  })
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'Preview import' }))
  await screen.findByText('Import preview')
  await user.click(screen.getByRole('button', { name: 'Execute import' }))
  const approval = await screen.findByRole('alertdialog', {
    name: 'Negative margin requires confirmation',
  })
  expect(post).toHaveBeenCalledTimes(1)
  await user.click(
    within(approval).getByRole('button', { name: 'Confirm loss' })
  )
  await waitFor(() =>
    expect(post).toHaveBeenLastCalledWith(
      '/api/admin/v1/bulk-import',
      source,
      expect.objectContaining({
        params: { dry_run: false },
        headers: expect.objectContaining({
          'X-Margin-Confirmation': report.revision,
          'X-Margin-Effective-At': '1800000000',
        }),
      })
    )
  )
  expect(
    await screen.findByText('Batch imported successfully')
  ).toBeInTheDocument()
})
