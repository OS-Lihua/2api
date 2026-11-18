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
import { Toaster } from 'sonner'
import { afterEach, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { PluginsTable } from '../components/plugins-table'
import type {
  TaskPluginPricingIssue,
  TaskPluginPricingDiagnostics,
  TaskPluginListItem,
} from '../types'

const clients: QueryClient[] = []
afterEach(() => {
  for (const client of clients) client.clear()
  clients.length = 0
  localStorage.removeItem('task-plugins-view-mode')
  act(() => useAuthStore.getState().auth.reset())
})

function renderPlugins(
  enabled: boolean,
  view: 'card' | 'table' = 'card',
  source: TaskPluginListItem['source'] = 'override'
) {
  localStorage.setItem('task-plugins-view-mode', view)
  const item: TaskPluginListItem = {
    meta: {
      key: 'example',
      name: 'Example',
      version: '1.0.0',
      apiVersion: 1,
      author: { name: 'Example' },
      models: [],
      fetchMode: 'per_task',
    },
    source,
    enabled,
    active: true,
    source_hash: '',
    remark: '',
    runtime_status: enabled ? 'registered' : 'disabled',
    channel_count: 0,
    in_flight_count: 0,
  }
  if (source === 'override_over_factory') {
    item.factory_meta = { ...item.meta, version: '0.9.0' }
  }
  const client = new QueryClient({
    defaultOptions: {
      queries: { retry: false, staleTime: Infinity },
      mutations: { retry: false },
    },
  })
  clients.push(client)
  client.setQueryData(['task-plugins'], [item])
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: [{ ...item, enabled: !enabled }] },
  })
  render(
    <QueryClientProvider client={client}>
      <PluginsTable onDetails={() => undefined} onUpload={() => undefined} />
    </QueryClientProvider>
  )
  return screen.getAllByRole('switch', { name: 'Enable plugin example' })[0]
}

test.each([
  ['card', true],
  ['card', false],
  ['table', true],
  ['table', false],
] as const)(
  '%s view: changing enabled=%s requires confirmation and cancellation preserves status',
  async (view, enabled) => {
    const user = userEvent.setup()
    const post = vi
      .spyOn(api, 'post')
      .mockResolvedValue({ data: { success: true, data: null } })
    const toggle = renderPlugins(enabled, view)
    await user.click(toggle)
    expect(post).not.toHaveBeenCalled()
    expect(toggle).toHaveAttribute('aria-checked', String(enabled))
    const title = enabled ? 'Disable plugin?' : 'Enable plugin?'
    const dialog = await screen.findByRole('alertdialog', { name: title })
    expect(dialog).toHaveTextContent('Example')
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }))
    await waitFor(() =>
      expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    )
    expect(post).not.toHaveBeenCalled()
    expect(toggle).toHaveAttribute('aria-checked', String(enabled))
    await user.click(
      screen.getAllByRole('switch', { name: 'Enable plugin example' })[0]
    )
    await user.click(
      within(await screen.findByRole('alertdialog', { name: title })).getByRole(
        'button',
        { name: enabled ? 'Disable' : 'Enable' }
      )
    )
    await waitFor(() =>
      expect(post).toHaveBeenCalledExactlyOnceWith(
        '/api/plugin/task/example/status',
        { enabled: !enabled },
        expect.any(Object)
      )
    )
    await waitFor(() =>
      expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    )
    expect(
      screen.getAllByRole('switch', { name: 'Enable plugin example' })[0]
    ).toHaveAttribute('aria-checked', String(!enabled))
  }
)

test('keyboard toggle opens confirmation and Escape cancels without a request', async () => {
  const user = userEvent.setup()
  const post = vi.spyOn(api, 'post')
  const toggle = renderPlugins(true)
  act(() => toggle.focus())
  await user.keyboard(' ')
  await screen.findByRole('alertdialog', { name: 'Disable plugin?' })
  await user.keyboard('{Escape}')
  await waitFor(() =>
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
  )
  expect(post).not.toHaveBeenCalled()
  expect(toggle).toHaveAttribute('aria-checked', 'true')
})

test('failed status request leaves confirmation available to retry and preserves the switch', async () => {
  const user = userEvent.setup()
  let finish!: (value: { data: { success: boolean; message: string } }) => void
  const post = vi.spyOn(api, 'post').mockImplementationOnce(
    () =>
      new Promise((resolve) => {
        finish = resolve
      })
  )
  const toggle = renderPlugins(true)
  await user.click(toggle)
  const dialog = await screen.findByRole('alertdialog', {
    name: 'Disable plugin?',
  })
  const confirm = within(dialog).getByRole('button', { name: 'Disable' })
  await user.click(confirm)
  expect(confirm).toBeDisabled()
  expect(within(dialog).getByRole('button', { name: 'Cancel' })).toBeDisabled()
  expect(screen.getAllByRole('switch', { hidden: true })[0]).toHaveAttribute(
    'aria-checked',
    'true'
  )
  await user.click(confirm)
  expect(post).toHaveBeenCalledTimes(1)
  await act(async () =>
    finish({ data: { success: false, message: 'Status update failed' } })
  )
  await waitFor(() => expect(confirm).toBeEnabled())
  expect(toggle).toHaveAttribute('aria-checked', 'true')
  expect(dialog).toBeInTheDocument()
})

test('confirmed disable blocked by usage opens the existing cascade confirmation', async () => {
  const user = userEvent.setup()
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValueOnce({
      data: {
        success: false,
        message: 'Plugin in use',
        data: {
          channels: [{ id: 7, name: 'Video channel' }],
          in_flight_count: 1,
        },
      },
    })
    .mockResolvedValueOnce({ data: { success: true, data: null } })
  await user.click(renderPlugins(true))
  await user.click(
    within(
      await screen.findByRole('alertdialog', { name: 'Disable plugin?' })
    ).getByRole('button', { name: 'Disable' })
  )
  const usage = await screen.findByRole('alertdialog', {
    name: 'Plugin is still in use',
  })
  expect(
    screen.queryByRole('alertdialog', { name: 'Disable plugin?' })
  ).not.toBeInTheDocument()
  expect(usage).toHaveTextContent('Video channel')
  await user.click(
    within(usage).getByRole('button', { name: 'Cascade disable channels' })
  )
  await waitFor(() =>
    expect(post).toHaveBeenLastCalledWith(
      '/api/plugin/task/example/status',
      { enabled: false },
      expect.objectContaining({ params: { cascade: true } })
    )
  )
  await waitFor(() =>
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
  )
})

test('usage dialog offers one cancel button and force operation posts force params', async () => {
  const user = userEvent.setup()
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValueOnce({
      data: {
        success: false,
        message: 'Plugin in use',
        data: {
          channels: [{ id: 7, name: 'Video channel' }],
          in_flight_count: 0,
        },
      },
    })
    .mockResolvedValueOnce({ data: { success: true, data: null } })
  await user.click(renderPlugins(true))
  await user.click(
    within(
      await screen.findByRole('alertdialog', { name: 'Disable plugin?' })
    ).getByRole('button', { name: 'Disable' })
  )
  const usage = await screen.findByRole('alertdialog', {
    name: 'Plugin is still in use',
  })
  expect(within(usage).getAllByRole('button', { name: 'Cancel' })).toHaveLength(
    1
  )
  await user.click(
    within(usage).getByRole('button', { name: 'Force operation' })
  )
  await waitFor(() =>
    expect(post).toHaveBeenLastCalledWith(
      '/api/plugin/task/example/status',
      { enabled: false },
      expect.objectContaining({ params: { cascade: true, force: true } })
    )
  )
})

test.each(['factory', 'override_over_factory'] as const)(
  '%s deletion protects the factory plugin while allowing custom version removal',
  async (source) => {
    const user = userEvent.setup()
    renderPlugins(true, 'table', source)
    await user.click(screen.getAllByRole('button', { name: 'Open menu' })[0])
    const remove = screen.getByRole('menuitem', {
      name: 'Delete active custom version',
    })
    if (source === 'factory') {
      expect(remove).toHaveAttribute('aria-disabled', 'true')
    } else {
      expect(remove).not.toHaveAttribute('aria-disabled', 'true')
      await user.click(remove)
      expect(
        await screen.findByRole('alertdialog', {
          name: 'Delete plugin version?',
        })
      ).toBeVisible()
    }
  }
)

test('a saved enable reports unavailable diagnostics without pretending the save failed', async () => {
  const user = userEvent.setup()
  const post = vi.spyOn(api, 'post').mockResolvedValue({
    data: {
      success: true,
      data: { pricing_diagnostics: { state: 'unavailable', issues: [] } },
    },
  })
  const toggle = renderPlugins(false, 'table')
  renderToaster()
  await user.click(toggle)
  await user.click(
    within(
      await screen.findByRole('alertdialog', {
        name: 'Enable plugin?',
      })
    ).getByRole('button', { name: 'Enable' })
  )
  expect(await screen.findByText('Plugin enabled')).toBeVisible()
  expect(
    screen.getByText(
      'Pricing diagnostics are unavailable. The plugin change was saved.'
    )
  ).toBeVisible()
  expect(
    screen.getByRole('button', { name: 'Retry pricing diagnostics' })
  ).toBeEnabled()
  expect(post).toHaveBeenCalledTimes(1)
  expect(
    screen.queryByText('No pricing issues found for visible price owners.')
  ).not.toBeInTheDocument()
})

function renderToaster() {
  const client = clients.at(-1)
  if (!client) {
    throw new Error('Render the plugin table before its notifications')
  }
  return render(
    <QueryClientProvider client={client}>
      <Toaster closeButton />
    </QueryClientProvider>
  )
}

function pricingIssue(
  ownerName: string,
  reason: TaskPluginPricingIssue['reason'] = 'unconfigured'
): TaskPluginPricingIssue {
  return {
    owner_type: 'model_offering',
    owner_id: Number(ownerName.match(/m(\d+)$/)?.[1] ?? 7),
    owner_name: ownerName,
    price_version_id: null,
    plugin_key: 'example',
    plugin_name: 'Example',
    reason,
  }
}

test('switching a plugin on lists current price owners requiring attention and refreshes plugin details', async () => {
  const user = userEvent.setup()
  vi.spyOn(api, 'post').mockResolvedValue({
    data: {
      success: true,
      data: {
        plugin_enabled: true,
        pricing_diagnostics: {
          state: 'ready',
          issues: [pricingIssue('example-v3')],
        },
      },
    },
  })
  const toggle = renderPlugins(false, 'table')
  renderToaster()
  const [client] = clients
  client.setQueryData(['task-plugin', 'example'], [])

  await user.click(toggle)
  await user.click(
    within(
      await screen.findByRole('alertdialog', { name: 'Enable plugin?' })
    ).getByRole('button', { name: 'Enable' })
  )

  expect(await screen.findByText('Plugin enabled')).toBeVisible()
  expect(screen.getByText('Model offering #7 · example-v3')).toBeVisible()
  expect(client.getQueryState(['task-plugin', 'example'])?.isInvalidated).toBe(
    true
  )
})

test('deleting the active custom version lists what the version taking over cannot charge', async () => {
  const user = userEvent.setup()
  const remove = vi.spyOn(api, 'delete').mockResolvedValue({
    data: {
      success: true,
      data: {
        pricing_diagnostics: {
          state: 'ready',
          issues: [pricingIssue('example-old', 'stale')],
        },
      },
    },
  })
  renderPlugins(true, 'table', 'override_over_factory')
  renderToaster()

  await user.click(screen.getAllByRole('button', { name: 'Open menu' })[0])
  await user.click(
    screen.getByRole('menuitem', { name: 'Delete active custom version' })
  )
  await user.click(
    within(
      await screen.findByRole('alertdialog', { name: 'Delete plugin version?' })
    ).getByRole('button', { name: 'Delete' })
  )

  expect(remove).toHaveBeenCalledWith(
    '/api/plugin/task/example/versions/1.0.0',
    expect.any(Object)
  )
  expect(await screen.findByText('Plugin version deleted')).toBeVisible()
  expect(
    screen.getByText(
      "The price owner's plugin or model binding is no longer valid."
    )
  ).toBeVisible()
})

test('a long pricing notice names five owners and counts the rest', async () => {
  const user = userEvent.setup()
  const issues = Array.from({ length: 7 }, (_, index) =>
    pricingIssue(`example-m${index + 1}`)
  )
  vi.spyOn(api, 'post').mockResolvedValue({
    data: {
      success: true,
      data: { pricing_diagnostics: { state: 'ready', issues } },
    },
  })
  const toggle = renderPlugins(false, 'table')
  renderToaster()

  await user.click(toggle)
  await user.click(
    within(
      await screen.findByRole('alertdialog', { name: 'Enable plugin?' })
    ).getByRole('button', { name: 'Enable' })
  )

  expect(await screen.findByText('And 2 more price owners')).toBeVisible()
  expect(screen.getByText('Model offering #5 · example-m5')).toBeVisible()
  expect(
    screen.queryByText('Model offering #6 · example-m6')
  ).not.toBeInTheDocument()
})

function setDiagnosticsActor(
  id = 1,
  sid = 'diagnostics-session',
  modelRead = true
) {
  useAuthStore.setState((state) => ({
    auth: {
      ...state.auth,
      user: {
        id,
        username: `admin-${id}`,
        role: 10,
        status: 1,
        permissions: {
          admin_permissions: {
            pricing: { read: true },
            model: { read: modelRead },
            channel: { read: true },
          },
        },
      },
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

async function enableForDiagnostics(diagnostics: unknown) {
  const user = userEvent.setup()
  const post = vi.spyOn(api, 'post').mockResolvedValue({
    data: { success: true, data: { pricing_diagnostics: diagnostics } },
  })
  const toggle = renderPlugins(false, 'table')
  renderToaster()
  await user.click(toggle)
  await user.click(
    within(
      await screen.findByRole('alertdialog', { name: 'Enable plugin?' })
    ).getByRole('button', { name: 'Enable' })
  )
  await screen.findByText('Plugin enabled')
  return { user, post }
}

test.each([
  ['ready', 'No pricing issues found for visible price owners.'],
  ['not_authorized', 'You are not authorized to read pricing diagnostics.'],
  [
    'unavailable',
    'Pricing diagnostics are unavailable. The plugin change was saved.',
  ],
] as const)(
  'saved plugin distinguishes %s from an unchecked empty issue list',
  async (state, message) => {
    await enableForDiagnostics({ state, issues: [] })
    expect(screen.getByText(message)).toBeVisible()
    expect(
      screen.queryByRole('button', { name: 'Retry pricing diagnostics' }) !==
        null
    ).toBe(state === 'unavailable')
    expect(
      screen.getByText(
        'Checks the current running plugin and visible price owners, not the selected historical version.'
      )
    ).toBeVisible()
    expect(
      vi.mocked(api.get).mock.calls.every(([url]) => url === '/api/plugin/task')
    ).toBe(true)
  }
)

test('diagnostic retry only reads current details and survives read failures without saving twice', async () => {
  const { user, post } = await enableForDiagnostics({
    state: 'unavailable',
    issues: [],
  })
  const get = vi.mocked(api.get)
  get
    .mockRejectedValueOnce(new Error('read unavailable'))
    .mockResolvedValueOnce({
      data: {
        success: true,
        data: {
          pricing_diagnostics: {
            state: 'ready',
            issues: [pricingIssue('Recovered owner')],
          },
        },
      },
    })
  screen.getByRole('button', { name: 'Retry pricing diagnostics' }).focus()
  await user.keyboard('{Enter}')
  await waitFor(() =>
    expect(
      screen.getByRole('button', { name: 'Retry pricing diagnostics' })
    ).toBeEnabled()
  )
  expect(screen.getByText('Plugin enabled')).toBeVisible()
  expect(
    screen.queryByText('No pricing issues found for visible price owners.')
  ).not.toBeInTheDocument()
  screen.getByRole('button', { name: 'Retry pricing diagnostics' }).focus()
  await user.keyboard('{Enter}')
  expect(
    await screen.findByText('Model offering #7 · Recovered owner')
  ).toBeVisible()
  expect(post).toHaveBeenCalledTimes(1)
  const reads = get.mock.calls.filter(
    ([url]) => url === '/api/plugin/task/example'
  )
  expect(reads).toHaveLength(2)
  for (const [, config] of reads) {
    expect(config).toMatchObject({
      signal: expect.any(AbortSignal),
      params: undefined,
    })
  }
  expect(get.mock.calls.some(([url]) => url.includes('model_pricing'))).toBe(
    false
  )
})

test('retrying unavailable diagnostics remeasures one toast for three full price owners', async () => {
  const longName = `owner-${'W'.repeat(122)}`
  const originalRect = HTMLElement.prototype.getBoundingClientRect
  vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(
    function (this: HTMLElement) {
      if (this.hasAttribute('data-sonner-toast')) {
        const height = this.textContent?.includes(longName) ? 860 : 300
        return new DOMRect(16, 16, 343, height)
      }
      return originalRect.call(this)
    }
  )
  const { user, post } = await enableForDiagnostics({
    state: 'unavailable',
    issues: [],
  })
  const notification = screen.getByText('Plugin enabled').closest('li')
  if (!(notification instanceof HTMLElement)) {
    throw new Error('Plugin notification must be rendered by Sonner')
  }
  await waitFor(() =>
    expect(notification.style.getPropertyValue('--initial-height')).toBe(
      '300px'
    )
  )
  const issues: TaskPluginPricingIssue[] = [
    { ...pricingIssue('same-owner'), owner_id: 7 },
    {
      ...pricingIssue('same-owner', 'incompatible'),
      owner_type: 'channel',
      owner_id: 7,
      price_version_id: 41,
    },
    { ...pricingIssue(longName, 'stale'), owner_id: 8, price_version_id: 42 },
  ]
  vi.mocked(api.get).mockResolvedValueOnce({
    data: {
      success: true,
      data: { pricing_diagnostics: { state: 'ready', issues } },
    },
  })
  screen.getByRole('button', { name: 'Retry pricing diagnostics' }).focus()
  await user.keyboard('{Enter}')
  expect(
    await screen.findByText(`Model offering #8 · ${longName}`)
  ).toBeVisible()
  expect(screen.getByText('Model offering #7 · same-owner')).toBeVisible()
  expect(screen.getByText('Channel #7 · same-owner')).toBeVisible()
  await waitFor(() =>
    expect(notification.style.getPropertyValue('--initial-height')).toBe(
      '860px'
    )
  )
  expect(screen.getByText('Plugin enabled').closest('li')).toBe(notification)
  expect(document.querySelectorAll('[data-sonner-toast]')).toHaveLength(1)
  expect(within(notification).getByRole('status')).toHaveClass(
    'max-h-[calc(100dvh-8rem)]',
    'overflow-y-auto',
    'overscroll-contain'
  )
  expect(post).toHaveBeenCalledTimes(1)
  expect(
    vi
      .mocked(api.get)
      .mock.calls.filter(([url]) => url === '/api/plugin/task/example')
  ).toHaveLength(1)
})

test('closing a notification during a diagnostic read never recreates it on completion', async () => {
  const { user, post } = await enableForDiagnostics({
    state: 'unavailable',
    issues: [],
  })
  let finish: (value: unknown) => void = () => undefined
  vi.mocked(api.get).mockImplementationOnce(
    () =>
      new Promise((resolve) => {
        finish = resolve
      })
  )
  screen.getByRole('button', { name: 'Retry pricing diagnostics' }).focus()
  await user.keyboard('{Enter}')
  expect(
    screen.getByRole('button', { name: 'Retry pricing diagnostics' })
  ).toBeDisabled()
  screen.getByRole('button', { name: 'Close toast' }).focus()
  await user.keyboard('{Enter}')
  await waitFor(() =>
    expect(screen.queryByText('Plugin enabled')).not.toBeInTheDocument()
  )
  await act(async () =>
    finish({
      data: {
        success: true,
        data: {
          pricing_diagnostics: {
            state: 'ready',
            issues: [pricingIssue('Dismissed owner')],
          },
        },
      },
    })
  )
  expect(screen.queryByText('Plugin enabled')).not.toBeInTheDocument()
  expect(screen.queryByText(/Dismissed owner/)).not.toBeInTheDocument()
  expect(post).toHaveBeenCalledTimes(1)
})

test.each(['account', 'session', 'permission'] as const)(
  'a pending diagnostic retry cannot update the toast after a %s change',
  async (change) => {
    setDiagnosticsActor()
    const { user, post } = await enableForDiagnostics({
      state: 'unavailable',
      issues: [],
    })
    let finish: (value: unknown) => void = () => undefined
    vi.mocked(api.get).mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          finish = resolve
        })
    )
    const retry = screen.getByRole('button', {
      name: 'Retry pricing diagnostics',
    })
    retry.focus()
    await user.keyboard('{Enter}{Enter}')
    expect(retry).toBeDisabled()
    expect(
      vi
        .mocked(api.get)
        .mock.calls.filter(([url]) => url === '/api/plugin/task/example')
    ).toHaveLength(1)
    act(() =>
      setDiagnosticsActor(
        change === 'account' ? 2 : 1,
        change === 'session' ? 'new-session' : 'diagnostics-session',
        change !== 'permission'
      )
    )
    await act(async () =>
      finish({
        data: {
          success: true,
          data: {
            pricing_diagnostics: {
              state: 'ready',
              issues: [pricingIssue('Previous retry owner')],
            },
          },
        },
      })
    )
    expect(screen.queryByText(/Previous retry owner/)).not.toBeInTheDocument()
    expect(
      screen.queryByRole('link', { name: 'Manage PriceVersions' })
    ).not.toBeInTheDocument()
    expect(post).toHaveBeenCalledTimes(1)
    expect(document.querySelectorAll('[data-sonner-toast]')).toHaveLength(1)
  }
)

test('same-name owners remain separate and management links respect owner permissions', async () => {
  setDiagnosticsActor(1, 'owner-permissions', false)
  const longName = `long-${'名'.repeat(250)}`
  const issues: TaskPluginPricingIssue[] = [
    { ...pricingIssue('Same name'), owner_id: 7 },
    {
      ...pricingIssue('Same name', 'incompatible'),
      owner_type: 'channel',
      owner_id: 7,
      price_version_id: 91,
    },
    { ...pricingIssue('Same name'), owner_id: 8 },
    {
      ...pricingIssue(longName, 'stale'),
      owner_type: 'channel',
      owner_id: 9,
      price_version_id: 92,
    },
  ]
  await enableForDiagnostics({ state: 'ready', issues })
  expect(screen.getByText('Model offering #7 · Same name')).toBeVisible()
  expect(screen.getByText('Model offering #8 · Same name')).toBeVisible()
  expect(screen.getByText('Channel #7 · Same name')).toBeVisible()
  expect(
    screen.getByText(`Channel #9 · ${longName}`).closest('li')
  ).toHaveClass('wrap-anywhere')
  const links = screen.getAllByRole('link', { name: 'Manage PriceVersions' })
  expect(links).toHaveLength(2)
  for (const link of links) {
    expect(link).toHaveAttribute('href', '/price-versions')
  }
  expect(
    screen.queryByRole('link', { name: /model pricing/i })
  ).not.toBeInTheDocument()
  expect(screen.getByText('Example (example) · PriceVersion #91')).toBeVisible()
})

test.each([
  undefined,
  {
    state: 'not_authorized',
    issues: [{ ...pricingIssue('Hidden private owner') }],
  },
  {
    state: 'unavailable',
    issues: [{ ...pricingIssue('Hidden private owner') }],
  },
  {
    state: 'ready',
    issues: [
      {
        model_name: 'Hidden private owner',
        plugin_key: 'example',
        plugin_name: 'Example',
        reason: 'unconfigured',
      },
    ],
  },
])(
  'missing or malformed diagnostics never imply success or disclose legacy owner fields',
  async (value) => {
    await enableForDiagnostics(value)
    expect(
      screen.getByText(
        'Pricing diagnostics are unavailable. The plugin change was saved.'
      )
    ).toBeVisible()
    expect(screen.queryByText(/Hidden private owner/)).not.toBeInTheDocument()
    expect(
      screen.queryByText('No pricing issues found for visible price owners.')
    ).not.toBeInTheDocument()
  }
)

test.each(['account', 'session', 'permission'] as const)(
  'late mutation diagnostics do not survive a %s change',
  async (change) => {
    setDiagnosticsActor()
    const user = userEvent.setup()
    let finish: (value: {
      data: {
        success: boolean
        data: { pricing_diagnostics: TaskPluginPricingDiagnostics }
      }
    }) => void = () => undefined
    const post = vi.spyOn(api, 'post').mockImplementation(
      () =>
        new Promise((resolve) => {
          finish = resolve
        })
    )
    const toggle = renderPlugins(false, 'table')
    renderToaster()
    await user.click(toggle)
    await user.click(
      within(
        await screen.findByRole('alertdialog', { name: 'Enable plugin?' })
      ).getByRole('button', { name: 'Enable' })
    )
    expect(post).toHaveBeenCalledTimes(1)
    act(() =>
      setDiagnosticsActor(
        change === 'account' ? 2 : 1,
        change === 'session' ? 'new-session' : 'diagnostics-session',
        change !== 'permission'
      )
    )
    await act(async () =>
      finish({
        data: {
          success: true,
          data: {
            pricing_diagnostics: {
              state: 'ready',
              issues: [pricingIssue('Previous private owner')],
            },
          },
        },
      })
    )
    expect(screen.queryByText(/Previous private owner/)).not.toBeInTheDocument()
    expect(screen.queryByText('Plugin enabled')).not.toBeInTheDocument()
  }
)

test('an already visible diagnostic toast hides private owners immediately after account change', async () => {
  setDiagnosticsActor()
  await enableForDiagnostics({
    state: 'ready',
    issues: [pricingIssue('Private owner')],
  })
  expect(screen.getByText('Model offering #7 · Private owner')).toBeVisible()
  act(() => setDiagnosticsActor(2, 'new-session'))
  expect(screen.queryByText(/Private owner/)).not.toBeInTheDocument()
})
