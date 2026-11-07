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
import { useAuthStore } from '@/stores/auth-store'

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
  useAuthStore.getState().auth.reset()
})

function renderDrawer(currentRow?: Provider) {
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
          currentRow={currentRow}
        />
      </QueryClientProvider>
    </I18nextProvider>
  )
  return onOpenChange
}

it('creating a Vertex Provider sends its deployment configuration', async () => {
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValue({ data: { success: true, data: { ids: [8] } } })
  const onOpenChange = renderDrawer(undefined)
  await userEvent.click(screen.getByRole('combobox', { name: 'Type' }))
  await userEvent.click(
    await screen.findByRole('option', { name: 'Vertex AI' })
  )
  await userEvent.type(
    screen.getByRole('textbox', { name: 'Name' }),
    'Vertex Provider'
  )
  await userEvent.click(screen.getByRole('textbox', { name: 'Key' }))
  await userEvent.paste('{"project_id":"test-project"}')
  await userEvent.click(screen.getByRole('button', { name: 'Save changes' }))
  expect(
    await screen.findByText(
      'This channel type requires additional configuration'
    )
  ).toBeVisible()
  expect(post).not.toHaveBeenCalled()
  await userEvent.type(
    screen.getByRole('textbox', { name: /Deployment Region/ }),
    'us-central1'
  )
  await userEvent.click(screen.getByRole('button', { name: 'Save changes' }))
  await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false))
  expect(post).toHaveBeenCalledWith('/api/admin/v1/providers', {
    mode: 'single',
    provider: {
      name: 'Vertex Provider',
      type: 41,
      key: '{"project_id":"test-project"}',
      other: '{"default":"us-central1"}',
    },
  })
})

it('recommendation requires confirmation and never overwrites a manual name when it arrives late', async () => {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'root', role: 100 })
  let resolveRecommendation!: (
    value: Awaited<ReturnType<typeof api.get>>
  ) => void
  const pending = new Promise<Awaited<ReturnType<typeof api.get>>>(
    (resolve) => {
      resolveRecommendation = resolve
    }
  )
  vi.spyOn(api, 'get').mockImplementation(() => pending)
  const post = vi.spyOn(api, 'post')
  renderDrawer()
  const name = screen.getByRole('textbox', { name: 'Name' })
  await userEvent.type(name, 'My account')
  resolveRecommendation({
    data: {
      success: true,
      data: {
        items: [
          {
            id: 1,
            kind: 'provider',
            pattern: 'api.openai.com',
            provider_name: 'OpenAI',
            provider_type: 1,
            priority: 0,
            enabled: true,
            publisher_id: 0,
            publisher_name: '',
          },
        ],
      },
    },
  })
  await userEvent.click(
    await screen.findByRole('button', { name: 'Use recommendation' })
  )
  expect(name).toHaveValue('My account')
  expect(post).not.toHaveBeenCalled()
  await userEvent.click(
    screen.getByRole('button', { name: 'Confirm recommendation' })
  )
  expect(name).toHaveValue('OpenAI')
  expect(post).not.toHaveBeenCalled()
  await userEvent.clear(name)
  await userEvent.type(name, 'Manually renamed')
  expect(name).toHaveValue('Manually renamed')
})

it('recommendations keep URL credentials out of requests', async () => {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'root', role: 100 })
  const get = vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: { items: [] } },
  })
  const secrets = [
    'private-user-fixture',
    'private-pass-fixture',
    'private-query-fixture',
    'private-fragment-fixture',
  ] as const
  const endpoint = new URL('https://api.deepseek.com:8443/v1')
  endpoint.username = secrets[0]
  endpoint.password = secrets[1]
  endpoint.searchParams.set('api_key', secrets[2])
  endpoint.hash = secrets[3]
  renderDrawer()
  await userEvent.click(screen.getByRole('textbox', { name: 'Base URL' }))
  await userEvent.paste(endpoint.href)

  await waitFor(() => {
    const lookups = get.mock.calls.filter(
      ([path]) => path === '/api/admin/v1/recommendations/provider'
    )
    expect(lookups.length).toBeGreaterThan(0)
    const [path, config] = lookups.at(-1) ?? []
    const requestURI = new URL(
      api.getUri({ url: path, ...config }),
      'https://console.example'
    )
    expect(requestURI.searchParams.get('url')).toBe(endpoint.origin)
    expect([...requestURI.searchParams.keys()].sort()).toEqual(['type', 'url'])
    for (const secret of secrets) {
      expect(decodeURIComponent(requestURI.href)).not.toContain(secret)
    }
  })
})

it('saving an edit with an empty key field keeps the stored key', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: stored },
  })
  const patch = vi
    .spyOn(api, 'patch')
    .mockResolvedValue({ data: { success: true, data: stored } })
  const onOpenChange = renderDrawer(stored)

  const name = await screen.findByDisplayValue('account one')
  expect(screen.getByRole('combobox', { name: 'Type' })).toBeDisabled()
  expect(
    screen.getByText('Create a new Provider to use a different type.')
  ).toBeVisible()
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
