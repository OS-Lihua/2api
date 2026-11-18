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
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import {
  act,
  cleanup,
  render,
  screen,
  within,
  waitFor,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { Toaster, toast } from 'sonner'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import zh from '@/i18n/locales/zh.json'
import { api } from '@/lib/api'

import { apiKeySchema, type ApiKey } from '../../types'
import { ApiKeysProvider } from '../api-keys-provider'
import { ApiKeysTable } from '../api-keys-table'

const key = apiKeySchema.parse({
  id: 7,
  name: 'production',
  key: 'demo********1234',
  status: 1,
  expired_time: -1,
  created_time: 0,
  accessed_time: 0,
})
const i18n = createInstance()
await i18n.init({
  lng: 'en',
  resources: { en: { translation: {} } },
  initAsync: false,
})
const clients: QueryClient[] = []

beforeEach(() => {
  vi.spyOn(window, 'scrollTo').mockImplementation(() => {})
  localStorage.clear()
  vi.spyOn(api, 'get').mockResolvedValue({ data: { success: true, data: {} } })
})
afterEach(() => {
  cleanup()
  toast.dismiss()
  localStorage.clear()
  clients.splice(0).forEach((client) => client.clear())
})

function KeysPage() {
  return (
    <ApiKeysProvider>
      <ApiKeysTable />
      <Toaster />
    </ApiKeysProvider>
  )
}

async function renderKeysPage(status = 1, overrides: Partial<ApiKey> = {}) {
  let currentKey = {
    ...key,
    status,
    remain_quota: 40_000_000,
    used_quota: 60_000_000,
    unlimited_quota: false,
    group: 'legacy-group',
    model_limits_enabled: true,
    model_limits: 'legacy-model',
    ...overrides,
  }
  vi.mocked(api.get).mockImplementation(async (url) => {
    if (url.startsWith('/api/token/')) {
      return {
        data: { success: true, data: { items: [currentKey], total: 1 } },
      }
    }
    throw new Error(`Unexpected GET ${url}`)
  })
  const post = vi.spyOn(api, 'post').mockResolvedValue({
    data: { success: true, data: { key: 'fake-key-for-test-only' } },
  })
  const put = vi.spyOn(api, 'put').mockImplementation(async (_url, data) => {
    const update = data as { id: number; status: number }
    currentKey = { ...currentKey, status: update.status }
    return { data: { success: true, data: currentKey } }
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  client.setQueryData(['status'], {})
  clients.push(client)
  const root = createRootRoute()
  const auth = createRoute({ getParentRoute: () => root, id: '_authenticated' })
  const keysRoute = createRoute({
    getParentRoute: () => auth,
    path: 'keys/',
    component: KeysPage,
  })
  const router = createRouter({
    routeTree: root.addChildren([auth.addChildren([keysRoute])]),
    history: createMemoryHistory({ initialEntries: ['/keys/'] }),
  })
  await router.load()
  render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <RouterProvider router={router} />
      </QueryClientProvider>
    </I18nextProvider>
  )
  await screen.findByText(currentKey.name)
  return { post, put }
}

it('shows Key identity, status, dates and IP restrictions without legacy columns or group requests', async () => {
  const { post } = await renderKeysPage()
  for (const name of [
    'Name',
    'Status',
    'API Key',
    'IP Restriction',
    'Time',
    'Expires',
  ]) {
    expect(screen.getByRole('columnheader', { name })).toBeInTheDocument()
  }
  for (const name of [/Quota/, 'Group', 'Models']) {
    expect(screen.queryByRole('columnheader', { name })).not.toBeInTheDocument()
  }
  const timeCell = screen.getByRole('cell', { name: /Created.*Last Used/ })
  expect(within(timeCell).getByText('Last Used')).toBeInTheDocument()
  expect(screen.getByText('sk-demo********1234')).toBeInTheDocument()
  expect(
    screen.queryByText('sk-fake-key-for-test-only')
  ).not.toBeInTheDocument()
  expect(post).not.toHaveBeenCalled()
  expect(
    vi
      .mocked(api.get)
      .mock.calls.every(([url]) => url.startsWith('/api/token/?'))
  ).toBe(true)
})

it('restores dates hidden by the old default and preserves unrelated column preferences', async () => {
  localStorage.setItem(
    'api-keys:column-visibility',
    JSON.stringify({
      created_time: false,
      accessed_time: false,
      expired_time: false,
      allow_ips: false,
      quota: true,
      group: true,
      model_limits: true,
    })
  )
  await renderKeysPage()
  expect(screen.getByRole('columnheader', { name: 'Time' })).toBeInTheDocument()
  expect(
    screen.getByRole('columnheader', { name: 'Expires' })
  ).toBeInTheDocument()
  expect(
    screen.queryByRole('columnheader', { name: 'IP Restriction' })
  ).not.toBeInTheDocument()
})

it.each([
  [1, 'Disable', 2, 'Disabled'],
  [2, 'Enable', 1, 'Enabled'],
])(
  'keeps status %s toggling at its original row button without fetching a full key',
  async (status, action, nextStatus, nextLabel) => {
    const { post, put } = await renderKeysPage(status)
    const user = userEvent.setup()
    const button = screen.getByRole('button', { name: action })
    act(() => button.focus())
    await user.keyboard('{Enter}')
    await waitFor(() =>
      expect(put).toHaveBeenCalledWith('/api/token/?status_only=true', {
        id: 7,
        status: nextStatus,
      })
    )
    await screen.findByText(nextLabel)
    expect(post).not.toHaveBeenCalled()
  }
)

it('keeps expired status when the server refuses reactivation', async () => {
  const { put, post } = await renderKeysPage(3)
  put.mockResolvedValue({ data: { success: false, message: 'Token expired' } })
  await userEvent.click(screen.getByRole('button', { name: 'Enable' }))
  await screen.findByText('Token expired')
  expect(screen.getByText('Expired')).toBeInTheDocument()
  expect(screen.queryByText('Enabled')).not.toBeInTheDocument()
  expect(post).not.toHaveBeenCalled()
})

it.each([true, false])(
  'fetches a full key only on explicit copy and honors permission success=%s',
  async (success) => {
    const user = userEvent.setup()
    const { post } = await renderKeysPage()
    post.mockResolvedValue(
      success
        ? { data: { success: true, data: { key: 'fake-key-for-test-only' } } }
        : { data: { success: false, message: 'Verification required' } }
    )
    const copy = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue()
    await user.click(screen.getByRole('button', { name: 'Open menu' }))
    expect(post).not.toHaveBeenCalled()
    await user.click(screen.getByRole('menuitem', { name: 'Copy Key' }))
    await waitFor(() => expect(post).toHaveBeenCalledWith('/api/token/7/key'))
    if (success) {
      await waitFor(() =>
        expect(copy).toHaveBeenCalledWith('sk-fake-key-for-test-only')
      )
    } else {
      await screen.findByText('Verification required')
      expect(copy).not.toHaveBeenCalled()
    }
  }
)

it('keeps mobile Key identity, dates and IP restrictions without legacy settings', async () => {
  const matchMedia = window.matchMedia
  vi.spyOn(window, 'matchMedia').mockImplementation((query) => ({
    ...matchMedia(query),
    matches: query.includes('max-width'),
  }))
  i18n.addResourceBundle('zh', 'translation', zh.translation)
  await i18n.changeLanguage('zh')
  try {
    const { post } = await renderKeysPage()
    expect(screen.queryByRole('table')).not.toBeInTheDocument()
    expect(screen.getByText(key.name)).toBeInTheDocument()
    expect(screen.getByText('sk-demo********1234')).toBeInTheDocument()
    for (const label of ['Created', 'Last Used', 'Expires', 'IP Restriction']) {
      expect(
        screen.getByText(zh.translation[label as keyof typeof zh.translation])
      ).toBeInTheDocument()
    }
    for (const label of [
      'Quota',
      'Group',
      'Models',
      'Used amount',
      'Remaining',
    ]) {
      expect(
        screen.queryByText(
          zh.translation[label as keyof typeof zh.translation],
          { exact: true }
        )
      ).not.toBeInTheDocument()
    }
    expect(screen.queryByText('legacy-group')).not.toBeInTheDocument()
    expect(screen.queryByText('legacy-model')).not.toBeInTheDocument()
    expect(post).not.toHaveBeenCalled()
  } finally {
    await i18n.changeLanguage('en')
  }
})

it('opens mobile IP restrictions by keyboard and restores focus after closing details', async () => {
  const matchMedia = window.matchMedia
  vi.spyOn(window, 'matchMedia').mockImplementation((query) => ({
    ...matchMedia(query),
    matches: query.includes('max-width'),
  }))
  await renderKeysPage(1, {
    name: 'production-with-a-long-key-name',
    allow_ips: '192.0.2.1\n2001:db8::/32',
  })
  const user = userEvent.setup()
  const button = screen.getByRole('button', { name: /IP Restriction: 2 IP/ })
  act(() => button.focus())
  await user.keyboard('{Enter}')
  const details = await screen.findByRole('dialog')
  expect(within(details).getByText('192.0.2.1')).toBeVisible()
  expect(within(details).getByText('2001:db8::/32')).toBeVisible()
  await user.keyboard('{Escape}')
  await waitFor(() =>
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  )
  expect(button).toHaveFocus()
})
