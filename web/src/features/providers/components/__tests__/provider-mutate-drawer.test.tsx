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
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import type { Provider } from '../../types'
import { ProviderMutateDrawer } from '../provider-mutate-drawer'

const i18n = createInstance()
await i18n.init({
  lng: 'en',
  resources: { en: { translation: {} } },
  initAsync: false,
})

const stored: Provider = {
  id: 7,
  type: 1,
  name: 'account one',
  status: 1,
  base_url: 'https://upstream.example',
  balance: 0,
  balance_updated_time: 0,
  created_time: 0,
  remark: '',
  channel_info: { is_multi_key: false, multi_key_size: 0 },
}

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

function renderDrawer() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  const onOpenChange = vi.fn()
  render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <ProviderMutateDrawer
          open
          onOpenChange={onOpenChange}
          currentRow={stored}
        />
      </QueryClientProvider>
    </I18nextProvider>
  )
  return onOpenChange
}

it('saving an edit with an empty key field keeps the stored key', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: stored },
  })
  const patch = vi
    .spyOn(api, 'patch')
    .mockResolvedValue({ data: { success: true, data: stored } })
  const onOpenChange = renderDrawer()

  const name = await screen.findByDisplayValue('account one')
  const key = screen.getByRole('textbox', { name: 'Key' })
  expect(key).toHaveValue('')
  expect(key).toHaveAttribute(
    'placeholder',
    'Leave empty to keep the current key'
  )

  await userEvent.clear(name)
  await userEvent.type(name, 'account renamed')
  await userEvent.click(screen.getByRole('button', { name: 'Save changes' }))

  await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false))
  expect(patch).toHaveBeenCalledWith('/api/admin/v1/providers/7', {
    name: 'account renamed',
    base_url: 'https://upstream.example',
    remark: '',
  })
})
