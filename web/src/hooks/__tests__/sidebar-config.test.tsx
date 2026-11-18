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
import {
  act,
  cleanup,
  render,
  renderHook,
  screen,
} from '@testing-library/react'
import type { ReactNode } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { checkIsActive } from '@/components/layout/lib/url-utils'
import { invalidateModelPricing } from '@/features/model-pricing/api'
import {
  parseSidebarModulesAdmin,
  serializeSidebarModulesAdmin,
} from '@/features/system-settings/maintenance/config'
import {
  MODEL_PRICING_ISSUES_QUERY_KEY,
  type ModelPricingIssue,
} from '@/features/system-settings/models/pricing-issues'
import { api } from '@/lib/api'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

import { useSidebarConfig } from '../use-sidebar-config'
import { useSidebarData } from '../use-sidebar-data'
import { useSidebarView } from '../use-sidebar-view'

beforeEach(() => {
  vi.stubGlobal('localStorage', {
    getItem: () => null,
    setItem: () => undefined,
    removeItem: () => undefined,
  })
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  useAuthStore.getState().auth.reset()
})

function sidebarFor(admin?: object, user?: object, canConfigure = true) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  client.setQueryData(['status'], {
    SidebarModulesAdmin: admin ? JSON.stringify(admin) : '',
  })
  useAuthStore.getState().auth.setUser({
    id: 1,
    username: 'alice',
    role: 1,
    permissions: { sidebar_settings: canConfigure },
    sidebar_modules: user ? JSON.stringify(user) : '',
  })
  function Wrapper(props: { children: ReactNode }) {
    return (
      <QueryClientProvider client={client}>
        {props.children}
      </QueryClientProvider>
    )
  }
  const result = renderHook(
    () => useSidebarConfig(useSidebarData().navGroups),
    { wrapper: Wrapper }
  )
  return result
}

it('offers Providers and its channels without retaining the legacy Channels page in navigation', () => {
  useAuthStore
    .getState()
    .auth.setUser({ id: 1, username: 'operator', role: 100 })
  const { result } = renderHook(() => useSidebarData())
  const items = result.current.navGroups.flatMap((group) => group.items)
  expect(items).toContainEqual(
    expect.objectContaining({ title: 'Providers', url: '/providers' })
  )
  expect(items.some((item) => item.url === '/channels')).toBe(false)
})

describe('security sidebar visibility', () => {
  it('old configurations show Security & Access immediately after Profile and keep API Keys', () => {
    const { result } = sidebarFor(
      { personal: { enabled: true, personal: true, topup: true } },
      { personal: { enabled: true, personal: true } }
    )
    expect(
      result.current
        .find((group) => group.id === 'personal')
        ?.items.map((item) => item.title)
    ).toEqual(['Wallet', 'Profile', 'Security & Access'])
    expect(
      result.current
        .flatMap((group) => group.items)
        .some((item) => item.title === 'API Keys')
    ).toBe(true)
  })
  it.each([
    [{ personal: { enabled: true, security: false } }, undefined],
    [{ personal: { enabled: false } }, { personal: { security: true } }],
    [undefined, { personal: { enabled: true, security: false } }],
    [undefined, { personal: { enabled: false } }],
  ])(
    'admin or user disablement hides Security & Access (%j, %j)',
    (admin, user) => {
      const { result } = sidebarFor(admin, user)
      expect(
        result.current
          .flatMap((group) => group.items)
          .some((item) => item.title === 'Security & Access')
      ).toBe(false)
    }
  )
  it('users without sidebar configuration permission retain the admin view', () => {
    const { result } = sidebarFor(
      undefined,
      { personal: { security: false } },
      false
    )
    expect(
      result.current
        .flatMap((group) => group.items)
        .some((item) => item.title === 'Security & Access')
    ).toBe(true)
  })
})

describe('audit log sidebar entry', () => {
  it('admin settings default Audit Logs to visible and preserve its independent toggle when saved', () => {
    const config = parseSidebarModulesAdmin(
      '{"console":{"enabled":true,"log":true}}'
    )
    expect(config.console.audit).toBe(true)
    config.console.audit = false
    const saved = parseSidebarModulesAdmin(serializeSidebarModulesAdmin(config))
    const { result } = sidebarFor(saved)
    const titles = result.current
      .flatMap((group) => group.items)
      .map((item) => item.title)
    expect(titles).not.toContain('Audit Logs')
    expect(titles).toContain('Usage Logs')
  })

  it('legacy configurations show a separate Audit Logs link immediately after Usage Logs', () => {
    const { result } = sidebarFor(
      { console: { enabled: true, log: true } },
      { console: { enabled: true, log: true } }
    )
    const items =
      result.current.find((group) => group.id === 'general')?.items ?? []
    const usageIndex = items.findIndex((item) => item.title === 'Usage Logs')
    expect(items[usageIndex + 1]).toMatchObject({
      title: 'Audit Logs',
      url: '/usage-logs/audit',
    })
    const selected = items.filter((item) =>
      checkIsActive('/usage-logs/audit', item)
    )
    expect(selected.map((item) => item.title)).toEqual(['Audit Logs'])
  })

  it.each([
    [{ console: { enabled: true, audit: false } }, undefined],
    [{ console: { enabled: false } }, { console: { audit: true } }],
    [undefined, { console: { enabled: true, audit: false } }],
    [undefined, { console: { enabled: false } }],
  ])(
    'admin and personal visibility rules can hide Audit Logs (%j, %j)',
    (admin, user) => {
      const { result } = sidebarFor(admin, user)
      expect(
        result.current
          .flatMap((group) => group.items)
          .some((item) => item.title === 'Audit Logs')
      ).toBe(false)
    }
  )

  it('hiding Usage Logs does not hide the independently configured Audit Logs entry', () => {
    const { result } = sidebarFor({ console: { enabled: true, log: false } })
    const titles = result.current
      .flatMap((group) => group.items)
      .map((item) => item.title)
    expect(titles).not.toContain('Usage Logs')
    expect(titles).toContain('Audit Logs')
  })
})

it('shows Request logs alongside legacy logs and respects the usage-log visibility setting', () => {
  const visible = sidebarFor({ console: { enabled: true, log: true } })
  const items = visible.result.current.flatMap((group) => group.items)
  expect(items).toEqual(
    expect.arrayContaining([
      expect.objectContaining({
        title: 'Request logs',
        url: '/usage-logs/requests',
      }),
      expect.objectContaining({
        title: 'Usage Logs',
        url: '/usage-logs/common',
      }),
      expect.objectContaining({ title: 'Task Logs', url: '/usage-logs/task' }),
    ])
  )
  const hidden = sidebarFor({ console: { enabled: true, log: false } })
  expect(
    hidden.result.current
      .flatMap((group) => group.items)
      .map((item) => item.title)
  ).not.toContain('Request logs')
})

describe('retired model pricing navigation', () => {
  function issue(modelName: string, pluginKey: string): ModelPricingIssue {
    return {
      model_name: modelName,
      plugin_key: pluginKey,
      plugin_name: pluginKey,
      reason: 'unconfigured',
    }
  }

  function NavigationEntries() {
    const { navGroups } = useSidebarView()
    const entries = navGroups
      .flatMap((group) => group.items)
      .flatMap((item) => [item, ...(item.items ?? [])])
    return (
      <ul aria-label='Navigation entries'>
        {entries.map((item) => (
          <li key={item.title}>
            {item.url ? <a href={item.url}>{item.title}</a> : item.title}
            {item.badge && <span>{`${item.title}: ${item.badge}`}</span>}
          </li>
        ))}
      </ul>
    )
  }

  async function renderSidebar(
    path: string,
    role: number,
    issues: () => ModelPricingIssue[]
  ) {
    const get = vi.spyOn(api, 'get').mockImplementation(async (url) => ({
      data: {
        success: true,
        data: url === '/api/option/model_pricing/issues' ? issues() : {},
      },
    }))
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    client.setQueryData(['status'], { SidebarModulesAdmin: '' })
    client.setQueryData(MODEL_PRICING_ISSUES_QUERY_KEY, issues())
    useAuthStore.getState().auth.setUser({ id: 1, username: 'root', role })
    const router = createRouter({
      routeTree: createRootRoute({ component: NavigationEntries }),
      history: createMemoryHistory({ initialEntries: [path] }),
    })
    await router.load()
    render(
      <QueryClientProvider client={client}>
        <RouterProvider router={router} />
      </QueryClientProvider>
    )
    return { client, get }
  }

  it('shows price versions and offerings without loading or displaying cached model-name pricing warnings', async () => {
    const { get, client } = await renderSidebar(
      '/dashboard/overview',
      ROLE.SUPER_ADMIN,
      () => [
        issue('kling-v3', 'kling'),
        issue('shared-video', 'alpha'),
        issue('shared-video', 'beta'),
      ]
    )

    expect(
      await screen.findByRole('link', { name: 'Price versions' })
    ).toHaveAttribute('href', '/price-versions')
    expect(
      screen.getByRole('link', { name: 'Model offerings' })
    ).toHaveAttribute('href', '/model-offerings')
    expect(
      screen.getByRole('link', { name: 'System Settings' })
    ).toBeInTheDocument()
    expect(screen.queryByText(/^System Settings:/)).not.toBeInTheDocument()
    expect(get).not.toHaveBeenCalledWith('/api/option/model_pricing/issues')
    client.clear()
  })

  it('keeps payment and check-in settings while removing model, group and quota-display pricing pages', async () => {
    const { get, client } = await renderSidebar(
      '/system-settings/site',
      ROLE.SUPER_ADMIN,
      () => [issue('kling-v3', 'kling')]
    )

    expect(
      await screen.findByRole('link', { name: 'Payment Gateway' })
    ).toHaveAttribute('href', '/system-settings/billing/payment')
    expect(
      screen.getByRole('link', { name: 'Check-in Rewards' })
    ).toHaveAttribute('href', '/system-settings/billing/checkin')
    for (const name of [
      'Model Pricing',
      'Group Pricing',
      'Currency & Display',
    ]) {
      expect(screen.queryByRole('link', { name })).not.toBeInTheDocument()
    }
    expect(screen.queryByText(/^Billing & Payment:/)).not.toBeInTheDocument()
    expect(get).not.toHaveBeenCalledWith('/api/option/model_pricing/issues')
    client.clear()
  })

  it('does not revive the retired pricing subscription after plugin or saved-price cache invalidation', async () => {
    let issues = [issue('kling-v3', 'kling')]
    const { client, get } = await renderSidebar(
      '/dashboard/overview',
      ROLE.SUPER_ADMIN,
      () => issues
    )
    await screen.findByRole('link', { name: 'Price versions' })

    issues = [issue('kling-v3', 'kling'), issue('vidu-q3', 'vidu')]
    await act(() =>
      client.invalidateQueries({ queryKey: MODEL_PRICING_ISSUES_QUERY_KEY })
    )
    issues = []
    await act(() => invalidateModelPricing(client))

    expect(get).not.toHaveBeenCalledWith('/api/option/model_pricing/issues')
    expect(screen.queryByText(/^System Settings:/)).not.toBeInTheDocument()
    expect(
      screen.getByRole('link', { name: 'Price versions' })
    ).toHaveAttribute('href', '/price-versions')
    client.clear()
  })

  it('never asks for the retired count below the super administrator role', async () => {
    const { get, client } = await renderSidebar(
      '/dashboard/overview',
      ROLE.ADMIN,
      () => [issue('kling-v3', 'kling')]
    )

    expect(
      await screen.findByRole('list', { name: 'Navigation entries' })
    ).toBeInTheDocument()
    expect(get).not.toHaveBeenCalledWith('/api/option/model_pricing/issues')
    expect(
      screen.queryByRole('link', { name: 'System Settings' })
    ).not.toBeInTheDocument()
    client.clear()
  })
})
