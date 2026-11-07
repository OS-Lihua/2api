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
  createRootRoute,
  createRoute,
  createRouter,
  createMemoryHistory,
  RouterProvider,
} from '@tanstack/react-router'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { ChannelMutateDrawer } from '../channel-mutate-drawer'
import { ProviderChannels } from '../provider-channels'
import { ProvidersProvider } from '../providers-provider'
import { ProvidersTable } from '../providers-table'

const i18n = createInstance()
await i18n.init({
  lng: 'en',
  resources: { en: { translation: {} } },
  initAsync: false,
})
afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
  useAuthStore.getState().auth.reset()
})

it('keyboard users expand and collapse one provider to load its channels', async () => {
  localStorage.clear()
  const get = vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/admin/v1/providers') {
      return {
        data: {
          success: true,
          data: {
            items: [
              {
                id: 7,
                name: 'Account A',
                type: 1,
                status: 1,
                balance: 0,
                balance_updated_time: 0,
                created_time: 0,
                channel_info: { is_multi_key: false, multi_key_size: 0 },
              },
            ],
            total: 1,
          },
        },
      }
    }
    return { data: { success: true, data: { items: [], total: 0 } } }
  })
  const root = createRootRoute()
  const auth = createRoute({ getParentRoute: () => root, id: '_authenticated' })
  const providers = createRoute({
    getParentRoute: () => auth,
    path: 'providers/',
    component: () => (
      <ProvidersProvider>
        <ProvidersTable />
      </ProvidersProvider>
    ),
  })
  const router = createRouter({
    routeTree: root.addChildren([auth.addChildren([providers])]),
    history: createMemoryHistory({ initialEntries: ['/providers/'] }),
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  await router.load()
  render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <RouterProvider router={router} />
      </QueryClientProvider>
    </I18nextProvider>
  )
  await waitFor(() => expect(client.isFetching()).toBe(0))
  const toggle = await screen.findByRole('button', { name: 'Account A' })
  expect(toggle).toHaveAttribute('aria-expanded', 'false')
  toggle.focus()
  await userEvent.keyboard('{Enter}')
  await waitFor(() =>
    expect(screen.getByRole('button', { name: 'Account A' })).toHaveAttribute(
      'aria-expanded',
      'true'
    )
  )
  expect(await screen.findByText('No channels found')).toBeVisible()
  expect(get).toHaveBeenCalledWith('/api/admin/v1/channels', {
    params: { provider_id: 7, p: 1, page_size: 10 },
  })
  await userEvent.click(screen.getByRole('button', { name: 'Account A' }))
  await waitFor(() =>
    expect(screen.getByRole('button', { name: 'Account A' })).toHaveAttribute(
      'aria-expanded',
      'false'
    )
  )
  await waitFor(() =>
    expect(
      screen.queryByRole('region', { name: 'Channels' })
    ).not.toBeInTheDocument()
  )
  client.clear()
  localStorage.clear()
})

it('creating a channel suggests its Model, validates limits, and retains a rejected draft', async () => {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'root', role: 100 })
  vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/admin/v1/channels/protocols') {
      return { data: { success: true, data: ['openai_chat'] } }
    }
    if (url === '/api/admin/v1/channels/suggest-models') {
      return {
        data: {
          success: true,
          data: { items: [{ id: 2, name: 'gpt-4o' }], selected_model_id: 2 },
        },
      }
    }
    return {
      data: { success: true, data: { items: [{ id: 2, name: 'gpt-4o' }] } },
    }
  })
  let complete: (value: unknown) => void = () => {}
  const post = vi.spyOn(api, 'post').mockImplementation(
    () =>
      new Promise((resolve) => {
        complete = resolve
      })
  )
  const onSaved = vi.fn()
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <ChannelMutateDrawer
          providerId={7}
          onClose={() => {}}
          onSaved={onSaved}
        />
      </QueryClientProvider>
    </I18nextProvider>
  )
  await userEvent.type(
    screen.getByRole('textbox', { name: 'Upstream model name' }),
    'openai/GPT-4o'
  )
  await userEvent.click(screen.getByRole('button', { name: 'Suggest Model' }))
  await userEvent.click(screen.getByRole('combobox', { name: 'Protocol' }))
  await userEvent.click(
    await screen.findByRole('option', { name: 'openai_chat' })
  )
  const rpm = screen.getByRole('textbox', { name: 'RPM' })
  await userEvent.clear(rpm)
  await userEvent.type(rpm, '-1')
  await userEvent.click(screen.getByRole('button', { name: 'Save changes' }))
  expect(await screen.findByText('Enter a non-negative integer')).toBeVisible()
  expect(post).not.toHaveBeenCalled()
  await userEvent.clear(rpm)
  await userEvent.type(rpm, '60')
  await userEvent.click(screen.getByRole('button', { name: 'Save changes' }))
  await waitFor(() =>
    expect(post).toHaveBeenCalledWith('/api/admin/v1/channels', {
      provider_id: 7,
      model_id: 2,
      protocol: 'openai_chat',
      upstream_model_name: 'openai/GPT-4o',
      rpm: 60,
      tpm: 0,
      max_concurrency: 0,
      weight: 1,
      parameter_overrides: {},
      capabilities: null,
    })
  )
  expect(screen.getByRole('button', { name: 'Saving...' })).toBeDisabled()
  complete({ data: { success: false, message: 'Save rejected' } })
  expect(await screen.findByRole('alert')).toHaveTextContent('Save rejected')
  expect(onSaved).not.toHaveBeenCalled()
  expect(
    screen.getByRole('textbox', { name: 'Upstream model name' })
  ).toHaveValue('openai/GPT-4o')
  post.mockResolvedValue({ data: { success: true, data: { id: 3 } } })
  await userEvent.click(screen.getByRole('button', { name: 'Save changes' }))
  await waitFor(() => expect(onSaved).toHaveBeenCalled())
  client.clear()
})

it('editing a channel saves explicit zero limits and its capability narrowing', async () => {
  const channel = {
    id: 3,
    provider_id: 7,
    model_id: 2,
    protocol: 'openai_chat',
    upstream_model_name: 'gpt-4o',
    status: 'disabled' as const,
    mapping_confirmed: true,
    rpm: 60,
    tpm: 1000,
    max_concurrency: 2,
    weight: 1,
    capabilities: {
      features: ['streaming' as const],
      input_modalities: ['text' as const],
      output_modalities: ['text' as const],
      limits: { context_length: 16000 },
    },
    parameter_overrides: { temperature: 0 },
  }
  vi.spyOn(api, 'get').mockImplementation(async (url) => ({
    data: {
      success: true,
      data:
        url === '/api/admin/v1/channels/protocols'
          ? ['openai_chat']
          : { items: [{ id: 2, name: 'gpt-4o' }] },
    },
  }))
  const patch = vi
    .spyOn(api, 'patch')
    .mockResolvedValue({ data: { success: true, data: channel } })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  const onSaved = vi.fn()
  render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <ChannelMutateDrawer
          providerId={7}
          channel={channel}
          onClose={() => {}}
          onSaved={onSaved}
        />
      </QueryClientProvider>
    </I18nextProvider>
  )
  const tpm = screen.getByRole('textbox', { name: 'TPM' })
  await userEvent.clear(tpm)
  await userEvent.type(tpm, '0')
  await userEvent.click(screen.getByRole('button', { name: 'Save changes' }))
  await waitFor(() => expect(onSaved).toHaveBeenCalled())
  expect(patch).toHaveBeenCalledWith('/api/admin/v1/channels/3', {
    provider_id: 7,
    model_id: 2,
    protocol: 'openai_chat',
    upstream_model_name: 'gpt-4o',
    rpm: 60,
    tpm: 0,
    max_concurrency: 2,
    weight: 1,
    capabilities: channel.capabilities,
    parameter_overrides: { temperature: 0 },
  })
  client.clear()
})

it('shows channels for this provider and confirms a pending mapping', async () => {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'root', role: 100 })
  const channel = {
    id: 3,
    provider_id: 7,
    model_id: 2,
    protocol: 'openai_chat',
    upstream_model_name: 'gpt-4o',
    status: 'pending',
    mapping_confirmed: false,
    rpm: 60,
    tpm: 1000,
    max_concurrency: 2,
    weight: 1,
    capabilities: null,
    parameter_overrides: {},
  }
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: { items: [channel], total: 1 } },
  })
  const post = vi.spyOn(api, 'post').mockResolvedValue({
    data: {
      success: true,
      data: { ...channel, status: 'disabled', mapping_confirmed: true },
    },
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <ProviderChannels providerId={7} />
      </QueryClientProvider>
    </I18nextProvider>
  )
  expect(await screen.findByText('gpt-4o')).toBeVisible()
  await userEvent.click(screen.getByRole('button', { name: 'Confirm mapping' }))
  expect(await screen.findByRole('alertdialog')).toBeVisible()
  await userEvent.click(screen.getByRole('button', { name: 'Continue' }))
  expect(post).toHaveBeenCalledWith('/api/admin/v1/channels/3/confirm')
})
