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
import { t } from 'i18next'
import { afterEach, describe, expect, it } from 'vitest'

import { ROLE } from '@/lib/roles'
import { Route as ModelsRoute } from '@/routes/_authenticated/system-settings/models/$section'
import { Route as SettingsRoute } from '@/routes/_authenticated/system-settings/route'
import { useAuthStore } from '@/stores/auth-store'
import { getModelsSectionNavItems } from '../section-registry'

type ModelsBeforeLoad = (context: { params: { section: string } }) => void

async function loadModelsRoute(section: string, role: number) {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'operator', role })
  const root = createRootRoute()
  const settings = createRoute({
    getParentRoute: () => root,
    path: '/system-settings',
    beforeLoad: SettingsRoute.options.beforeLoad as () => void,
  })
  const models = createRoute({
    getParentRoute: () => settings,
    path: 'models/$section',
    beforeLoad: ModelsRoute.options.beforeLoad as ModelsBeforeLoad,
  })
  const router = createRouter({
    routeTree: root.addChildren([
      settings.addChildren([models]),
      createRoute({ getParentRoute: () => root, path: '/403' }),
    ]),
    history: createMemoryHistory({
      initialEntries: [`/system-settings/models/${section}`],
    }),
  })
  await router.load()
  return router.state.location.pathname
}

afterEach(() => {
  useAuthStore.getState().auth.reset()
})

describe('model settings navigation', () => {
  it('hides the retired Grok fee page and redirects its bookmark', async () => {
    const links = getModelsSectionNavItems(t).map((item) => item.url)
    expect(links).not.toContain('/system-settings/models/grok')
    expect(await loadModelsRoute('grok', ROLE.SUPER_ADMIN)).toBe(
      '/system-settings/models/global'
    )
  })

  it('keeps the system settings role guard for the retired page', async () => {
    expect(await loadModelsRoute('grok', ROLE.ADMIN)).toBe('/403')
  })

  it('keeps other model settings reachable', async () => {
    expect(await loadModelsRoute('claude', ROLE.SUPER_ADMIN)).toBe(
      '/system-settings/models/claude'
    )
  })
})
