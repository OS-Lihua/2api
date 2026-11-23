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
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import { act, cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'
import { useSystemConfigStore } from '@/stores/system-config-store'

import { metric, reportFixture } from '../../../__tests__/finance-fixtures'
import type { FinanceFilter } from '../../../lib/finance'
import { OverviewDashboard } from '../overview-dashboard'

const storageKey = 'dashboard_overview_setup_guide_expanded'
let client: QueryClient
let keyLookupError: Error | null
let requestCount = '1'

beforeEach(() => {
  window.localStorage.clear()
  useSystemConfigStore.setState(useSystemConfigStore.getInitialState(), true)
  const legacyUser = {
    id: 1,
    username: 'dashboard-user',
    role: 1,
    quota: 1000000,
    used_quota: 1000,
    request_count: 1,
    permissions: { admin_permissions: { wallet: { read: true } } },
  }
  useAuthStore.getState().auth.setUser(legacyUser)
  client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  keyLookupError = null
  requestCount = '1'
  vi.spyOn(api, 'get').mockImplementation(async (url, config) => {
    switch (url) {
      case '/api/token/?p=1&size=10':
        if (keyLookupError) throw keyLookupError
        return {
          data: {
            success: true,
            data: {
              items: [{ id: 1, name: 'App key', key: 'masked', status: 1 }],
            },
          },
        }
      case '/api/status':
        return {
          data: {
            data: {
              api_info_enabled: false,
              announcements_enabled: false,
              faq_enabled: false,
              uptime_kuma_enabled: false,
            },
          },
        }
      case '/api/user/models':
        return { data: { success: true, data: ['gpt-4o-mini'] } }
      case '/api/user/self/wallet':
        return {
          data: {
            success: true,
            data: {
              user_id: 1,
              currency: 'EUR',
              balance: '9007199.254740993',
              outstanding: '0.000000001',
              consumed: { nanos: '1', currency: 'EUR' },
              sale_pending: false,
              requests_paused: false,
              updated_at: 1,
            },
          },
        }
      case '/api/user/self/dashboard-finance': {
        const report = reportFixture(config?.params as FinanceFilter)
        report.totals.request_count = requestCount
        report.totals.collected = metric('2')
        return { data: { success: true, data: report } }
      }
      default:
        throw new Error(`Unexpected dashboard request: ${url}`)
    }
  })
})

afterEach(() => {
  cleanup()
  client.clear()
  useAuthStore.setState(useAuthStore.getInitialState(), true)
  useSystemConfigStore.setState(useSystemConfigStore.getInitialState(), true)
  window.localStorage.clear()
})

async function renderOverview() {
  const router = createRouter({
    routeTree: createRootRoute({ component: OverviewDashboard }),
    history: createMemoryHistory({ initialEntries: ['/'] }),
  })
  await router.load()
  return render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
}

describe('overview setup guide', () => {
  it('uses the authorized exact wallet rather than legacy user quota for the balance', async () => {
    await renderOverview()
    expect(await screen.findByText('9,007,199.254740993 EUR')).toBeVisible()
    expect(api.get).toHaveBeenCalledWith(
      '/api/user/self/wallet',
      expect.objectContaining({ signal: expect.any(AbortSignal) })
    )
    expect(
      vi
        .mocked(api.get)
        .mock.calls.some(([url]) => String(url).startsWith('/api/data'))
    ).toBe(false)
  })
  it('shows usage first and only a header entry when setup is complete', async () => {
    await renderOverview()

    const toggle = await screen.findByRole('button', { name: 'Setup guide' })
    expect(toggle).toHaveAttribute('aria-expanded', 'false')
    expect(
      screen.getAllByRole('heading').map((heading) => heading.textContent)
    ).toEqual(['Overview', 'Usage at a glance'])
    expect(screen.queryByText('Setup guide complete')).not.toBeInTheDocument()
    expect(screen.queryByText('Setup progress: 3/3')).not.toBeInTheDocument()
    for (const name of ['API Keys', 'Providers', 'Usage Logs', 'Pricing']) {
      expect(screen.queryByRole('button', { name })).not.toBeInTheDocument()
    }
    const panel = document.querySelector(
      `[id="${toggle.getAttribute('aria-controls')}"]`
    )
    expect(panel).toBeInTheDocument()
    expect(panel).not.toBeVisible()
  })

  it('toggles the completed guide with the keyboard and restores focus after hiding it', async () => {
    const user = userEvent.setup()
    await renderOverview()
    const toggle = await screen.findByRole('button', { name: 'Setup guide' })

    await user.tab()
    expect(toggle).toHaveFocus()
    await user.keyboard('{Enter}')
    expect(toggle).toHaveAttribute('aria-expanded', 'true')
    expect(
      document.querySelector(`[id="${toggle.getAttribute('aria-controls')}"]`)
    ).toBeVisible()
    expect(
      screen.getByRole('heading', {
        name: 'Build on your API gateway in minutes',
      })
    ).toBeVisible()
    await waitFor(() =>
      expect(screen.getByRole('button', { name: /^API Keys/ })).toBeVisible()
    )

    await user.keyboard(' ')
    expect(toggle).toHaveAttribute('aria-expanded', 'false')
    expect(toggle).toHaveFocus()
    await user.click(toggle)
    await user.click(screen.getByRole('button', { name: 'Hide setup guide' }))
    expect(toggle).toHaveAttribute('aria-expanded', 'false')
    expect(toggle).toHaveFocus()
  })

  it('restores the completed guide preference after remounting', async () => {
    const user = userEvent.setup()
    const first = await renderOverview()
    await user.click(await screen.findByRole('button', { name: 'Setup guide' }))
    first.unmount()

    const second = await renderOverview()
    expect(
      await screen.findByRole('button', { name: 'Setup guide' })
    ).toHaveAttribute('aria-expanded', 'true')
    await user.click(screen.getByRole('button', { name: 'Hide setup guide' }))
    second.unmount()

    await renderOverview()
    expect(
      await screen.findByRole('button', { name: 'Setup guide' })
    ).toHaveAttribute('aria-expanded', 'false')
    expect(screen.queryByText('Setup guide complete')).not.toBeInTheDocument()
  })

  it('keeps the existing progress banner when an incomplete guide is manually collapsed', async () => {
    const user = userEvent.setup()
    useAuthStore
      .getState()
      .auth.setUser({ id: 1, username: 'new-user', role: 1 })
    await renderOverview()

    await user.click(
      await screen.findByRole('button', { name: 'Hide setup guide' })
    )
    expect(screen.getByText('Setup progress: 1/3')).toBeVisible()
    expect(
      screen.getByText('Setup guide is collapsed. Expand it anytime.')
    ).toBeVisible()
    expect(screen.getByRole('button', { name: 'API Keys' })).toBeVisible()
    expect(
      screen.queryByRole('button', { name: 'Setup guide' })
    ).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Show setup guide' }))
    expect(
      screen.getByRole('button', { name: 'Hide setup guide' })
    ).toBeVisible()
  })

  it('removes the collapsed progress banner when the remaining setup step completes', async () => {
    requestCount = '0'
    useAuthStore.getState().auth.setUser({
      id: 1,
      username: 'dashboard-user',
      role: 1,
      permissions: { admin_permissions: { wallet: { read: true } } },
    })
    window.localStorage.setItem(storageKey, 'collapsed')
    await renderOverview()
    expect(await screen.findByText('Setup progress: 2/3')).toBeVisible()

    await act(async () => {
      requestCount = '1'
      await client.invalidateQueries({ queryKey: ['dashboard-finance'] })
    })
    expect(
      await screen.findByRole('button', { name: 'Setup guide' })
    ).toHaveAttribute('aria-expanded', 'false')
    expect(screen.queryByText(/Setup progress:/)).not.toBeInTheDocument()
  })

  it('does not show a completed setup entry when the key lookup fails', async () => {
    keyLookupError = new Error('Key lookup unavailable')
    await renderOverview()

    expect(
      await screen.findByRole('button', { name: 'Hide setup guide' })
    ).toBeVisible()
    expect(
      screen.queryByRole('button', { name: 'Setup guide' })
    ).not.toBeInTheDocument()
  })
})
