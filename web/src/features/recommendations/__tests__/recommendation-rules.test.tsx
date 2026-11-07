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
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { RecommendationRules } from '../recommendation-rules'

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
  useAuthStore.getState().auth.reset()
})

it('administrator can create and edit persisted Provider recommendation rules', async () => {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'root', role: 100 })
  const rule = {
    id: 1,
    kind: 'provider',
    pattern: 'api.example.com',
    provider_name: 'Example',
    provider_type: 1,
    enabled: true,
    priority: 0,
    publisher_id: 0,
    publisher_name: '',
  }
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: { items: [rule], total: 1 } },
  })
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: true } })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  render(
    <QueryClientProvider client={client}>
      <RecommendationRules kind='provider' />
    </QueryClientProvider>
  )
  await userEvent.click(
    screen.getByRole('button', { name: 'Recommendation rules' })
  )
  await userEvent.click(await screen.findByRole('button', { name: 'Edit' }))
  const pattern = await screen.findByRole('textbox', { name: 'Match pattern' })
  await userEvent.clear(pattern)
  await userEvent.type(pattern, '*.example.com')
  await userEvent.click(screen.getByRole('button', { name: 'Save changes' }))
  await waitFor(() =>
    expect(put).toHaveBeenCalledWith(
      '/api/admin/v1/recommendation-rules/1',
      expect.objectContaining({
        pattern: '*.example.com',
        provider_name: 'Example',
        provider_type: 1,
      })
    )
  )
  client.clear()
})
