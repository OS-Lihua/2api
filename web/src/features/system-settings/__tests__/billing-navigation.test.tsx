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
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
} from '@tanstack/react-router'
import { afterEach, describe, expect, it } from 'vitest'

import { ROLE } from '@/lib/roles'
import { Route as BillingRoute } from '@/routes/_authenticated/system-settings/billing/$section'
import { Route as SettingsRoute } from '@/routes/_authenticated/system-settings/route'
import { useAuthStore } from '@/stores/auth-store'

type BillingBeforeLoad = (context: { params: { section: string } }) => void

async function loadBillingRoute(section: string, role: number) {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'operator', role })
  const root = createRootRoute()
  const settings = createRoute({
    getParentRoute: () => root,
    path: '/system-settings',
    beforeLoad: SettingsRoute.options.beforeLoad as () => void,
  })
  const billing = createRoute({
    getParentRoute: () => settings,
    path: 'billing/$section',
    beforeLoad: BillingRoute.options.beforeLoad as BillingBeforeLoad,
  })
  const router = createRouter({
    routeTree: root.addChildren([
      settings.addChildren([billing]),
      createRoute({ getParentRoute: () => root, path: '/price-versions' }),
      createRoute({ getParentRoute: () => root, path: '/fx-rates' }),
      createRoute({ getParentRoute: () => root, path: '/403' }),
    ]),
    history: createMemoryHistory({
      initialEntries: [`/system-settings/billing/${section}`],
    }),
  })
  await router.load()
  return router.state.location.pathname
}

afterEach(() => {
  useAuthStore.getState().auth.reset()
})

describe('billing page retirement', () => {
  it.each([
    ['model-pricing', '/price-versions'],
    ['group-pricing', '/price-versions'],
    ['currency', '/fx-rates'],
  ])('redirects the retired %s bookmark to %s', async (section, target) => {
    expect(await loadBillingRoute(section, ROLE.SUPER_ADMIN)).toBe(target)
  })

  it.each(['model-pricing', 'group-pricing', 'currency'])(
    'keeps the system-settings role guard on the retired %s bookmark',
    async (section) => {
      expect(await loadBillingRoute(section, ROLE.ADMIN)).toBe('/403')
    }
  )

  it.each(['payment', 'checkin'])(
    'keeps the active %s page reachable',
    async (section) => {
      expect(await loadBillingRoute(section, ROLE.SUPER_ADMIN)).toBe(
        `/system-settings/billing/${section}`
      )
    }
  )

  it('keeps unknown sections on the supported default page', async () => {
    expect(await loadBillingRoute('removed', ROLE.SUPER_ADMIN)).toBe(
      '/system-settings/billing/quota'
    )
  })
})
