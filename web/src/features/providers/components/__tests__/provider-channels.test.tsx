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
import {
  act,
  cleanup,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, expect, it, vi } from 'vitest'

import { ModelConnections } from '@/features/models/components/model-connections'
import type { Model } from '@/features/models/types'
import en from '@/i18n/locales/en.json'
import fr from '@/i18n/locales/fr.json'
import zh from '@/i18n/locales/zh.json'
import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { BulkImportDialog } from '../bulk-import-dialog'
import { ChannelMutateDrawer } from '../channel-mutate-drawer'
import { ChannelProbeDialog } from '../channel-probe-dialog'
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
  vi.useRealTimers()
  vi.restoreAllMocks()
  useAuthStore.getState().auth.reset()
})

const savedModel: Model = {
  id: 2,
  model_name: 'same-name',
  name_rule: 0,
  status: 1,
  sync_official: 0,
  created_time: 1,
  updated_time: 1,
  supported_endpoints: ['openai_chat'],
}

it('loads Model connections by stable ID through the shared read-only Channel table', async () => {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'root', role: 100 })
  const get = vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        items: [
          {
            id: 31,
            provider_id: 7,
            model_id: 2,
            protocol: 'openai_chat',
            upstream_model_name: 'actual-upstream',
            status: 'pending',
            mapping_confirmed: false,
          },
        ],
        total: 1,
      },
    },
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <ModelConnections model={savedModel} />
      </QueryClientProvider>
    </I18nextProvider>
  )
  expect(await screen.findByText('actual-upstream')).toBeVisible()
  expect(get).toHaveBeenCalledWith('/api/admin/v1/channels', {
    params: { model_id: 2, p: 1, page_size: 10 },
    signal: expect.any(AbortSignal),
  })
  expect(
    screen.getByRole('columnheader', { name: 'Provider ID' })
  ).toBeVisible()
  expect(screen.getByText('Pending confirmation')).toBeVisible()
  for (const name of [
    'New Channel',
    'Edit',
    'Delete',
    'Confirm mapping',
    'Capability probes',
  ]) {
    expect(screen.queryByRole('button', { name })).not.toBeInTheDocument()
  }
  expect(screen.queryByText('Enable Groups')).not.toBeInTheDocument()
  expect(
    screen.getByText(
      'Native endpoints declared by channels and model metadata. Actual availability depends on the selected route and request.'
    )
  ).toBeVisible()
  expect(
    get.mock.calls.every(([url]) => url === '/api/admin/v1/channels')
  ).toBe(true)
  client.clear()
})

function channelResponse(name: string, modelId = 2, total = 1) {
  return {
    data: {
      success: true,
      data: {
        items: name
          ? [
              {
                id: modelId * 10,
                provider_id: 7,
                model_id: modelId,
                protocol: 'openai_chat',
                upstream_model_name: name,
                status: 'enabled',
                mapping_confirmed: true,
              },
            ]
          : [],
        total,
      },
    },
  }
}
function channelSession(sid: string) {
  useAuthStore.setState((state) => ({
    auth: {
      ...state.auth,
      session: {
        sid,
        current: true,
        login_method: 'password',
        ip: '',
        user_agent: '',
        created_at: 1,
        last_active_at: 1,
        expires_at: 9999999999,
      },
    },
  }))
}
function mountConnections(initial: Model = savedModel) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  const element = (model: Model) => (
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <ModelConnections model={model} />
      </QueryClientProvider>
    </I18nextProvider>
  )
  const result = render(element(initial))
  return { client, rerender: (model: Model) => result.rerender(element(model)) }
}

it('requires channel:read separately from model:read and never requests Provider names', async () => {
  useAuthStore.getState().auth.setUser({
    id: 8,
    username: 'metadata-reader',
    role: 10,
    permissions: {
      admin_permissions: {
        model: { read: true },
        channel: { write: true, operate: true },
      },
    },
  })
  const get = vi
    .spyOn(api, 'get')
    .mockResolvedValue(channelResponse('forbidden'))
  const { client } = mountConnections()
  expect(screen.getByRole('status')).toHaveTextContent(
    'Channel read permission is required to view connections.'
  )
  expect(get).not.toHaveBeenCalled()
  expect(screen.queryByRole('table')).not.toBeInTheDocument()
  await act(() =>
    useAuthStore.getState().auth.setUser({
      id: 8,
      username: 'metadata-reader',
      role: 10,
      permissions: {
        admin_permissions: { model: { read: true }, channel: { read: true } },
      },
    })
  )
  expect(await screen.findByText('forbidden')).toBeVisible()
  expect(
    get.mock.calls.every(([url]) => url === '/api/admin/v1/channels')
  ).toBe(true)
  expect(
    screen.queryByRole('button', { name: 'New Channel' })
  ).not.toBeInTheDocument()
  client.clear()
})

it.each([
  { id: 0, name_rule: 0 },
  { id: 2, name_rule: 1 },
])(
  'does not infer Channels from unsaved or rule metadata $id/$name_rule',
  (identity) => {
    useAuthStore.getState().auth.setUser({ id: 1, username: 'root', role: 100 })
    const get = vi.spyOn(api, 'get')
    const { client } = mountConnections({ ...savedModel, ...identity })
    expect(screen.getByRole('status')).toHaveTextContent(
      'Connections require a saved exact-name Model. Metadata rules do not bind Channels.'
    )
    expect(get).not.toHaveBeenCalled()
    client.clear()
  }
)

it('isolates same-name Model IDs, keeps a renamed ID, and cancels late target responses', async () => {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'root', role: 100 })
  let finish: (value: ReturnType<typeof channelResponse>) => void = () => {}
  const pending = new Promise<ReturnType<typeof channelResponse>>((resolve) => {
    finish = resolve
  })
  const get = vi
    .spyOn(api, 'get')
    .mockImplementation((_url, config) =>
      (config?.params as { model_id?: number } | undefined)?.model_id === 2
        ? pending
        : Promise.resolve(channelResponse('second-ID', 3))
    )
  const { client, rerender } = mountConnections()
  await waitFor(() => expect(get).toHaveBeenCalledOnce())
  const firstSignal = get.mock.calls[0][1]?.signal
  rerender({ ...savedModel, id: 3 })
  expect(await screen.findByText('second-ID')).toBeVisible()
  expect(firstSignal?.aborted).toBe(true)
  await act(async () => finish(channelResponse('late-first-ID')))
  expect(screen.queryByText('late-first-ID')).not.toBeInTheDocument()
  rerender({ ...savedModel, id: 3, model_name: 'renamed' })
  expect(screen.getByText('second-ID')).toBeVisible()
  expect(get).toHaveBeenCalledTimes(2)
  expect(get.mock.calls.map(([, config]) => config?.params)).toEqual([
    { model_id: 2, p: 1, page_size: 10 },
    { model_id: 3, p: 1, page_size: 10 },
  ])
  client.clear()
})

it.each(['actor', 'session', 'permission'] as const)(
  'discards late Channels after changing %s',
  async (change) => {
    useAuthStore.getState().auth.setUser({ id: 1, username: 'root', role: 100 })
    channelSession('before')
    let finish: (value: ReturnType<typeof channelResponse>) => void = () => {}
    const pending = new Promise<ReturnType<typeof channelResponse>>(
      (resolve) => {
        finish = resolve
      }
    )
    const get = vi
      .spyOn(api, 'get')
      .mockReturnValueOnce(pending)
      .mockResolvedValue(channelResponse('current-scope'))
    const { client } = mountConnections()
    await waitFor(() => expect(get).toHaveBeenCalledOnce())
    const signal = get.mock.calls[0][1]?.signal
    await act(() => {
      if (change === 'session') channelSession('after')
      else if (change === 'actor') {
        useAuthStore
          .getState()
          .auth.setUser({ id: 9, username: 'other', role: 100 })
      } else {
        useAuthStore.getState().auth.setUser({
          id: 1,
          username: 'root',
          role: 10,
          permissions: { admin_permissions: { model: { read: true } } },
        })
      }
    })
    expect(signal?.aborted).toBe(true)
    if (change === 'permission') {
      expect(screen.getByRole('status')).toHaveTextContent(
        'Channel read permission is required'
      )
    } else expect(await screen.findByText('current-scope')).toBeVisible()
    await act(async () => finish(channelResponse('old-private-channel')))
    expect(screen.queryByText('old-private-channel')).not.toBeInTheDocument()
    expect(get).toHaveBeenCalledTimes(change === 'permission' ? 1 : 2)
    client.clear()
  }
)

it('keeps Channel load errors distinct from an empty result and retries the current ID', async () => {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'root', role: 100 })
  const get = vi
    .spyOn(api, 'get')
    .mockRejectedValueOnce(new Error('Load rejected'))
    .mockResolvedValue(channelResponse('', 2, 0))
  const { client } = mountConnections()
  expect(await screen.findByRole('button', { name: 'Retry' })).toBeVisible()
  expect(screen.queryByText('No channels found')).not.toBeInTheDocument()
  await userEvent.click(screen.getByRole('button', { name: 'Retry' }))
  expect(await screen.findByText('No channels found')).toBeVisible()
  expect(get).toHaveBeenCalledTimes(2)
  expect(get).toHaveBeenLastCalledWith('/api/admin/v1/channels', {
    params: { model_id: 2, p: 1, page_size: 10 },
    signal: expect.any(AbortSignal),
  })
  client.clear()
})

it.each([
  { write: false, operate: false },
  { write: true, operate: false },
  { write: false, operate: true },
])(
  'preserves separate Provider Channel write=$write and operate=$operate permissions',
  async ({ write, operate }) => {
    useAuthStore.getState().auth.setUser({
      id: 8,
      username: 'channel-reader',
      role: 10,
      permissions: {
        admin_permissions: { channel: { read: true, write, operate } },
      },
    })
    const get = vi
      .spyOn(api, 'get')
      .mockResolvedValue(channelResponse('provider-upstream'))
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    render(
      <I18nextProvider i18n={i18n}>
        <QueryClientProvider client={client}>
          <ProviderChannels providerId={7} />
        </QueryClientProvider>
      </I18nextProvider>
    )
    expect(await screen.findByText('provider-upstream')).toBeVisible()
    expect(screen.queryByRole('button', { name: 'New Channel' }) !== null).toBe(
      write
    )
    expect(screen.queryByRole('button', { name: 'Edit' }) !== null).toBe(write)
    expect(screen.queryByRole('button', { name: 'Delete' }) !== null).toBe(
      write
    )
    expect(screen.queryByRole('button', { name: 'Disable' }) !== null).toBe(
      operate
    )
    expect(
      screen.getByRole('button', { name: 'Capability probes' })
    ).toBeVisible()
    expect(get).toHaveBeenCalledWith('/api/admin/v1/channels', {
      params: { provider_id: 7, p: 1, page_size: 10 },
      signal: expect.any(AbortSignal),
    })
    client.clear()
  }
)

it('does not refresh or finish an old Provider action after a session switch', async () => {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'root', role: 100 })
  channelSession('action-before')
  const get = vi
    .spyOn(api, 'get')
    .mockResolvedValue(channelResponse('provider-upstream'))
  let finish: (value: { data: { success: boolean } }) => void = () => {}
  const pending = new Promise<{ data: { success: boolean } }>((resolve) => {
    finish = resolve
  })
  const post = vi.spyOn(api, 'post').mockReturnValue(pending)
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
  await screen.findByText('provider-upstream')
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'Disable' }))
  await user.click(
    within(screen.getByRole('alertdialog')).getByRole('button', {
      name: 'Continue',
    })
  )
  await waitFor(() => expect(post).toHaveBeenCalledOnce())
  await act(() => channelSession('action-after'))
  await screen.findByText('provider-upstream')
  const calls = get.mock.calls.length
  await act(async () => finish({ data: { success: true } }))
  expect(get).toHaveBeenCalledTimes(calls)
  expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
  client.clear()
})

it('loads the next Channel page for the same Model ID without retaining prior rows', async () => {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'root', role: 100 })
  const get = vi
    .spyOn(api, 'get')
    .mockResolvedValueOnce(channelResponse('first-page', 2, 11))
    .mockResolvedValue(channelResponse('second-page', 2, 11))
  const { client } = mountConnections()
  await screen.findByText('first-page')
  await userEvent.click(screen.getByRole('button', { name: 'Go to next page' }))
  expect(await screen.findByText('second-page')).toBeVisible()
  expect(screen.queryByText('first-page')).not.toBeInTheDocument()
  expect(get).toHaveBeenLastCalledWith('/api/admin/v1/channels', {
    params: { model_id: 2, p: 2, page_size: 10 },
    signal: expect.any(AbortSignal),
  })
  client.clear()
})

it('keeps long read-only Channel names accessible on mobile while translated connection guidance changes', async () => {
  const originalMatchMedia = window.matchMedia
  vi.spyOn(window, 'matchMedia').mockImplementation((query) => ({
    ...originalMatchMedia(query),
    matches: query === '(max-width: 640px)',
  }))
  useAuthStore.getState().auth.setUser({ id: 1, username: 'root', role: 100 })
  const name = `upstream-${'long-name-'.repeat(30)}`
  const get = vi.spyOn(api, 'get').mockResolvedValue(channelResponse(name))
  const translated = createInstance()
  await translated.init({
    lng: 'en',
    resources: { en, fr, zh },
    initAsync: false,
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  render(
    <I18nextProvider i18n={translated}>
      <QueryClientProvider client={client}>
        <ModelConnections model={savedModel} />
      </QueryClientProvider>
    </I18nextProvider>
  )
  expect(await screen.findByText(name)).toBeVisible()
  const guidance =
    'Channel status does not establish a sale price, Offering access, or request availability.'
  expect(screen.getByText(guidance)).toBeVisible()
  await act(async () => translated.changeLanguage('fr'))
  expect(screen.getByText(fr.translation[guidance])).toBeVisible()
  expect(screen.getByText(name)).toBeVisible()
  await act(async () => translated.changeLanguage('zh'))
  expect(screen.getByText(zh.translation[guidance])).toBeVisible()
  expect(screen.getByText(name)).toBeVisible()
  expect(get).toHaveBeenCalledOnce()
  expect(
    screen.queryByRole('button', { name: zh.translation['New Channel'] })
  ).not.toBeInTheDocument()
  client.clear()
})

it('previews a batch, invalidates edited previews, and confirms before committing', async () => {
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false } },
  })
  const preview = {
    dry_run: true,
    valid: true,
    committed: false,
    rows: [{ row: 1, resource: 'providers', action: 'create' }],
  }
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValue({ data: { success: true, data: preview } })
  render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <BulkImportDialog open onOpenChange={() => {}} />
      </QueryClientProvider>
    </I18nextProvider>
  )
  const source = screen.getByRole('textbox', { name: 'Import content' })
  const execute = screen.getByRole('button', { name: 'Execute import' })
  expect(execute).toBeDisabled()
  await userEvent.type(source, 'test batch')
  await userEvent.click(screen.getByRole('button', { name: 'Preview import' }))
  expect(await screen.findByRole('table')).toBeVisible()
  await waitFor(() => expect(execute).toBeEnabled())
  await userEvent.type(source, ' changed')
  expect(execute).toBeDisabled()
  await userEvent.click(screen.getByRole('button', { name: 'Preview import' }))
  await waitFor(() => expect(execute).toBeEnabled())
  await userEvent.click(execute)
  expect(await screen.findByRole('alertdialog')).toBeVisible()
  post.mockRejectedValueOnce(new Error('Commit rejected'))
  await userEvent.click(screen.getByRole('button', { name: 'Continue' }))
  await waitFor(() =>
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
  )
  expect(source).toHaveValue('test batch changed')
  expect(
    screen.queryByText('Batch imported successfully')
  ).not.toBeInTheDocument()
  await userEvent.click(execute)
  expect(await screen.findByRole('alertdialog')).toBeVisible()
  post.mockResolvedValue({
    data: {
      success: true,
      data: { ...preview, dry_run: false, committed: true },
    },
  })
  await userEvent.click(screen.getByRole('button', { name: 'Continue' }))
  expect(await screen.findByText('Batch imported successfully')).toBeVisible()
  expect(source).toHaveValue('')
  client.clear()
})

it('uses resource permissions through the selected import operation', async () => {
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false } },
  })
  const post = vi.spyOn(api, 'post').mockResolvedValue({
    data: {
      success: true,
      data: { dry_run: true, valid: true, committed: false, rows: [] },
    },
  })
  render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <BulkImportDialog open onOpenChange={() => {}} />
      </QueryClientProvider>
    </I18nextProvider>
  )
  await userEvent.type(
    screen.getByRole('textbox', { name: 'Import content' }),
    'batch'
  )
  for (const target of [
    'price-versions/import',
    'channels/bulk-confirm',
    'channels/bulk-enable',
  ]) {
    await userEvent.selectOptions(
      screen.getByRole('combobox', { name: 'Operation' }),
      target
    )
    expect(
      screen.getByRole('button', { name: 'Execute import' })
    ).toBeDisabled()
    await userEvent.click(
      screen.getByRole('button', { name: 'Preview import' })
    )
    await waitFor(() =>
      expect(post).toHaveBeenLastCalledWith(
        `/api/admin/v1/${target}`,
        'batch',
        expect.objectContaining({ params: { dry_run: true } })
      )
    )
    await waitFor(() =>
      expect(
        screen.getByRole('button', { name: 'Preview import' })
      ).toBeEnabled()
    )
  }
  client.clear()
})

it('blocks execution after invalid or rejected previews and while an uploaded file is being read', async () => {
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false } },
  })
  const post = vi.spyOn(api, 'post').mockResolvedValue({
    data: {
      success: false,
      data: {
        dry_run: true,
        valid: false,
        committed: false,
        rows: [
          {
            row: 2,
            resource: 'channels',
            action: 'enable',
            error: 'Configure a purchase price before enabling the channel',
          },
        ],
      },
    },
  })
  render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <BulkImportDialog open onOpenChange={() => {}} />
      </QueryClientProvider>
    </I18nextProvider>
  )
  const source = screen.getByRole('textbox', { name: 'Import content' })
  await userEvent.type(source, 'draft')
  let complete: (text: string) => void = () => {}
  const file = new File(['batch'], 'batch.json', { type: 'application/json' })
  Object.defineProperty(file, 'text', {
    value: () =>
      new Promise<string>((resolve) => {
        complete = resolve
      }),
  })
  await userEvent.upload(screen.getByLabelText('Import file'), file)
  expect(screen.getByRole('button', { name: 'Preview import' })).toBeDisabled()
  expect(source).toBeDisabled()
  await act(async () => complete('uploaded draft'))
  await waitFor(() => expect(source).toHaveValue('uploaded draft'))
  await userEvent.click(screen.getByRole('button', { name: 'Preview import' }))
  expect(
    await screen.findByText(
      'Configure a purchase price before enabling the channel'
    )
  ).toBeVisible()
  expect(screen.getByRole('button', { name: 'Execute import' })).toBeDisabled()
  post.mockRejectedValue(new Error('Request failed'))
  await userEvent.click(screen.getByRole('button', { name: 'Preview import' }))
  await waitFor(() =>
    expect(screen.getByRole('button', { name: 'Preview import' })).toBeEnabled()
  )
  expect(screen.getByRole('button', { name: 'Execute import' })).toBeDisabled()
  expect(source).toHaveValue('uploaded draft')
  expect(
    screen.queryByText('Batch imported successfully')
  ).not.toBeInTheDocument()
  client.clear()
})

it('associates oversized upload errors with the file input and updates their language', async () => {
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false } },
  })
  render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <BulkImportDialog open onOpenChange={() => {}} />
      </QueryClientProvider>
    </I18nextProvider>
  )
  const input = screen.getByLabelText('Import file')
  await userEvent.upload(
    input,
    new File([new Uint8Array((2 << 20) + 1)], 'large.csv', { type: 'text/csv' })
  )
  expect(input).toHaveAttribute('aria-invalid', 'true')
  expect(input).toHaveAccessibleDescription('Import exceeds the upload limit')
  i18n.addResource(
    'zh',
    'translation',
    'Import exceeds the upload limit',
    '超过上传上限'
  )
  await act(async () => {
    await i18n.changeLanguage('zh')
  })
  expect(screen.getByRole('alert')).toHaveTextContent('超过上传上限')
  await act(async () => {
    await i18n.changeLanguage('en')
  })
  client.clear()
})

it('keyboard users expand and collapse one provider to load its channels', async () => {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'root', role: 100 })
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
    signal: expect.any(AbortSignal),
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

it('allows authorized Channel creation by stable Model ID without bypassing model:read for names', async () => {
  useAuthStore.getState().auth.setUser({
    id: 8,
    username: 'channel-writer',
    role: 10,
    permissions: {
      admin_permissions: { channel: { read: true, write: true } },
    },
  })
  const get = vi
    .spyOn(api, 'get')
    .mockResolvedValue({ data: { success: true, data: ['openai_chat'] } })
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValue({ data: { success: true, data: { id: 3 } } })
  const saved = vi.fn()
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <ChannelMutateDrawer
          providerId={7}
          onClose={() => {}}
          onSaved={saved}
        />
      </QueryClientProvider>
    </I18nextProvider>
  )
  const user = userEvent.setup()
  await user.type(
    screen.getByRole('textbox', { name: 'Upstream model name' }),
    'upstream-exact'
  )
  await user.type(screen.getByRole('textbox', { name: 'Model ID' }), '2')
  expect(screen.getByRole('button', { name: 'Suggest Model' })).toBeDisabled()
  await user.click(screen.getByRole('combobox', { name: 'Protocol' }))
  await user.click(await screen.findByRole('option', { name: 'openai_chat' }))
  await user.click(screen.getByRole('button', { name: 'Save changes' }))
  await waitFor(() => expect(saved).toHaveBeenCalledOnce())
  expect(post).toHaveBeenCalledWith(
    '/api/admin/v1/channels',
    expect.objectContaining({
      provider_id: 7,
      model_id: 2,
      upstream_model_name: 'upstream-exact',
    })
  )
  expect(
    get.mock.calls.every(([url]) => url === '/api/admin/v1/channels/protocols')
  ).toBe(true)
  client.clear()
})

it('creating a channel suggests its Model, validates limits, and retains a rejected draft', async () => {
  const longModelName = `legal-${'W'.repeat(250)}`
  useAuthStore.getState().auth.setUser({ id: 1, username: 'root', role: 100 })
  vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/admin/v1/channels/protocols') {
      return { data: { success: true, data: ['openai_chat'] } }
    }
    if (url === '/api/admin/v1/channels/suggest-models') {
      return {
        data: {
          success: true,
          data: {
            items: [{ id: 2, name: longModelName }],
            selected_model_id: 2,
          },
        },
      }
    }
    return {
      data: {
        success: true,
        data: { items: [{ id: 2, name: longModelName }] },
      },
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
  const modelChoice = screen.getByRole('combobox', { name: 'Model' })
  await waitFor(() => expect(modelChoice).toHaveTextContent(longModelName))
  expect(modelChoice.closest('[data-slot="form-item"]')).toHaveClass(
    'min-w-0',
    'grid-cols-1'
  )
  expect(modelChoice).toHaveAttribute('title', longModelName)
  await userEvent.click(modelChoice)
  const option = await screen.findByRole('option', { name: longModelName })
  expect(within(option).getByText(longModelName)).toHaveClass('wrap-anywhere')
  await userEvent.click(option)
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

it('editing a channel clears overrides while saving zero limits and capability narrowing', async () => {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'root', role: 100 })
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
  await userEvent.click(
    within(
      screen.getByRole('group', { name: 'Parameter overrides' })
    ).getByRole('button', { name: 'Delete row' })
  )
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
    parameter_overrides: {},
  })
  client.clear()
})

it('opens capability probes and shows persisted results before activation', async () => {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'root', role: 100 })
  vi.spyOn(api, 'get').mockImplementation(async (url) => ({
    data: {
      success: true,
      data: url.endsWith('/probes')
        ? {
            capabilities: ['streaming', 'tool_calling'],
            results: [
              {
                id: 1,
                capability: 'streaming',
                status: 'passed',
                reason: 'verified',
                finished_at: 1794000000,
              },
            ],
          }
        : {
            items: [
              {
                id: 3,
                provider_id: 7,
                model_id: 2,
                protocol: 'openai_chat',
                upstream_model_name: 'probe-model',
                status: 'pending',
                mapping_confirmed: false,
              },
            ],
            total: 1,
          },
    },
  }))
  const post = vi.spyOn(api, 'post').mockResolvedValue({
    data: {
      success: true,
      data: {
        capability: 'tool_calling',
        status: 'inconclusive',
        reason: 'behavior_not_observed',
        finished_at: 1794000001,
      },
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
  await userEvent.click(
    await screen.findByRole('button', { name: 'Capability probes' })
  )
  await waitFor(() =>
    expect(
      within(
        screen.getByRole('dialog', { name: 'Capability probes' })
      ).getByText('Success')
    ).toBeVisible()
  )
  const dialog = screen.getByRole('dialog', { name: 'Capability probes' })
  expect(within(dialog).getByText('Not tested')).toBeVisible()
  await userEvent.click(
    within(dialog).getByRole('button', { name: 'Probe Tool calling' })
  )
  await waitFor(() =>
    expect(post).toHaveBeenCalledWith(
      '/api/admin/v1/channels/3/probes',
      {
        capability: 'tool_calling',
      },
      { timeout: 100_000 }
    )
  )
  expect(
    within(dialog).getByText(
      'Results do not change capability declarations. Test requests may incur Provider costs.'
    )
  ).toBeVisible()
  client.clear()
})

it('refreshes another administrator’s running probe and reconciles a failed submission', async () => {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'root', role: 100 })
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
  let status = 'running'
  vi.spyOn(api, 'get').mockImplementation(async () => ({
    data: {
      success: true,
      data: {
        capabilities: ['streaming'],
        results: [
          {
            id: 1,
            capability: 'streaming',
            status,
            reason: status === 'passed' ? 'verified' : 'behavior_not_observed',
            finished_at: 1794000001,
          },
        ],
      },
    },
  }))
  vi.spyOn(api, 'post').mockImplementation(async () => {
    status = 'inconclusive'
    throw new Error('synthetic network failure')
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <ChannelProbeDialog
          channel={{
            id: 3,
            provider_id: 7,
            model_id: 2,
            protocol: 'openai_chat',
            upstream_model_name: 'probe-model',
            status: 'pending',
            mapping_confirmed: false,
            capabilities: null,
            parameter_overrides: {},
            rpm: 0,
            tpm: 0,
            max_concurrency: 0,
            weight: 1,
          }}
          canOperate
          onClose={() => {}}
        />
      </QueryClientProvider>
    </I18nextProvider>
  )
  expect(await screen.findByText('Testing...')).toBeVisible()
  status = 'passed'
  await act(async () => {
    await vi.advanceTimersByTimeAsync(1500)
  })
  expect(await screen.findByText('Capability behavior observed')).toBeVisible()
  await userEvent.click(screen.getByRole('button', { name: 'Probe Streaming' }))
  expect(
    await screen.findByText('Capability behavior was not observed')
  ).toBeVisible()
  client.clear()
})

it('shows runtime circuit health separately from persistent Channel status', async () => {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'root', role: 100 })
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        items: [
          {
            id: 3,
            provider_id: 7,
            model_id: 2,
            protocol: 'openai_chat',
            upstream_model_name: 'runtime-model',
            status: 'enabled',
            mapping_confirmed: true,
            health: {
              circuit: 'open',
              retry_at: 1794000060000,
              failures: 5,
              samples: 10,
              success_rate: 0.5,
              first_byte_ms: 120,
              first_byte_samples: 5,
            },
          },
        ],
        total: 1,
      },
    },
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <ProviderChannels providerId={7} />
      </QueryClientProvider>
    </I18nextProvider>
  )
  expect(await screen.findByText('Circuit open')).toBeVisible()
  expect(screen.getByRole('region', { name: 'Channels' })).toHaveClass(
    'max-w-[calc(100vw-3rem)]',
    'md:max-w-none',
    'whitespace-normal'
  )
  expect(screen.getByText('Enabled')).toBeVisible()
  expect(screen.getByText('Recent success rate')).toBeVisible()
  expect(screen.getByText('50%')).toBeVisible()
  expect(screen.getByText('120 ms')).toBeVisible()
  expect(screen.getByText('10')).toBeVisible()
  expect(screen.getByText(/Probe after:/)).toBeVisible()
  client.clear()
})

it('shows channels for this provider and confirms a pending mapping', async () => {
  const longName = `legal-${'W'.repeat(250)}`
  useAuthStore.getState().auth.setUser({ id: 1, username: 'root', role: 100 })
  const channel = {
    id: 3,
    provider_id: 7,
    model_id: 2,
    protocol: 'openai_chat',
    upstream_model_name: longName,
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
  expect(await screen.findByText(longName)).toBeVisible()
  await userEvent.click(screen.getByRole('button', { name: 'Confirm mapping' }))
  const dialog = await screen.findByRole('alertdialog')
  expect(dialog).toBeVisible()
  expect(within(dialog).getByText(longName)).toHaveClass('wrap-anywhere')
  await userEvent.click(screen.getByRole('button', { name: 'Continue' }))
  expect(post).toHaveBeenCalledWith('/api/admin/v1/channels/3/confirm')
})
