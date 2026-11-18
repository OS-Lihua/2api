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
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import i18next from 'i18next'
import { Toaster, toast } from 'sonner'
import { afterEach, expect, test, vi } from 'vitest'

import {
  getSavedGranularity,
  saveGranularity,
} from '@/features/dashboard/lib/filters'
import en from '@/i18n/locales/en.json'
import fr from '@/i18n/locales/fr.json'
import ja from '@/i18n/locales/ja.json'
import ru from '@/i18n/locales/ru.json'
import viLocale from '@/i18n/locales/vi.json'
import zhTW from '@/i18n/locales/zh-TW.json'
import zh from '@/i18n/locales/zh.json'
import { api } from '@/lib/api'
import { ROLE } from '@/lib/roles'
import { Route as ContentRoute } from '@/routes/_authenticated/system-settings/content/$section'
import { Route as SettingsRoute } from '@/routes/_authenticated/system-settings/route'
import { useAuthStore } from '@/stores/auth-store'

import type { UpdateOptionRequest } from '../../types'
import { ContentSettings } from '../index'
import { getContentSectionNavItems } from '../section-registry'

const clients: QueryClient[] = []
const retirementMessage =
  'Legacy statistics collection settings have been retired.'
const locales = { en, zh, 'zh-TW': zhTW, fr, ru, ja, vi: viLocale }

async function renderSettings(
  section = 'dashboard',
  overrides: Record<string, string> = {},
  role: number = ROLE.SUPER_ADMIN
) {
  const options: Record<string, string> = {
    DataExportEnabled: 'true',
    DataExportInterval: '60',
    DataExportDefaultTime: 'week',
    'console_setting.announcements': JSON.stringify([
      {
        id: 7,
        content: 'Active announcement',
        publishDate: '2026-01-01T00:00:00.000Z',
        type: 'default',
        extra: '',
      },
    ]),
    'console_setting.announcements_enabled': 'true',
    'console_setting.faq': '[]',
    'console_setting.faq_enabled': 'true',
    ...overrides,
  }
  const get = vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url !== '/api/option/') throw new Error(`Unexpected GET ${url}`)
    return {
      data: {
        success: true,
        data: Object.entries(options).map(([key, value]) => ({ key, value })),
      },
    }
  })
  const put = vi.spyOn(api, 'put').mockImplementation(async (url, request) => {
    if (url !== '/api/option/') throw new Error(`Unexpected PUT ${url}`)
    const option = request as UpdateOptionRequest
    options[option.key] = String(option.value)
    return { data: { success: true } }
  })
  vi.spyOn(window, 'scrollTo').mockImplementation(() => undefined)
  useAuthStore.getState().auth.setUser({
    id: 1,
    username: 'content-admin',
    role,
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  clients.push(client)
  const root = createRootRoute()
  const authenticated = createRoute({
    getParentRoute: () => root,
    id: '_authenticated',
  })
  const settings = createRoute({
    getParentRoute: () => authenticated,
    path: 'system-settings',
    beforeLoad: SettingsRoute.options.beforeLoad as () => void,
  })
  const content = createRoute({
    getParentRoute: () => settings,
    path: 'content/$section',
    beforeLoad: ContentRoute.options.beforeLoad as (context: {
      params: { section: string }
    }) => void,
    component: ContentSettings,
  })
  const router = createRouter({
    routeTree: root.addChildren([
      authenticated.addChildren([settings.addChildren([content])]),
      createRoute({
        getParentRoute: () => root,
        path: '/403',
        component: () => <h1>Forbidden</h1>,
      }),
    ]),
    history: createMemoryHistory({
      initialEntries: [`/system-settings/content/${section}`],
    }),
  })
  render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
      <Toaster />
    </QueryClientProvider>
  )
  if (role === ROLE.SUPER_ADMIN) {
    await waitFor(() =>
      expect(client.getQueryState(['system-options'])?.status).toBe('success')
    )
  } else {
    await screen.findByRole('heading', { name: 'Forbidden' })
  }
  await screen.findAllByRole('heading')
  return { get, put, options, router, user: userEvent.setup() }
}

async function addFAQ(user: ReturnType<typeof userEvent.setup>) {
  await user.click(
    await screen.findByRole('button', { name: i18next.t('Add FAQ') })
  )
  const dialog = screen.getByRole('dialog')
  await user.type(
    within(dialog).getByRole('textbox', { name: i18next.t('Question') }),
    'Where is my history?'
  )
  await user.type(
    within(dialog).getByRole('textbox', { name: i18next.t('Answer') }),
    'Open the usage logs and export the selected history.'
  )
  await user.click(
    within(dialog).getByRole('button', { name: i18next.t('Add') })
  )
  await waitFor(() =>
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  )
  return [
    {
      id: 1,
      question: 'Where is my history?',
      answer: 'Open the usage logs and export the selected history.',
    },
  ]
}

function saveButton() {
  return screen.getByRole('button', { name: i18next.t('Save Settings') })
}

afterEach(async () => {
  cleanup()
  toast.dismiss()
  clients.splice(0).forEach((client) => client.clear())
  useAuthStore.getState().auth.reset()
  vi.restoreAllMocks()
  localStorage.removeItem('data_export_default_time')
  localStorage.removeItem('dashboard_models_chart_preferences')
  await i18next.changeLanguage('en')
  for (const language of Object.keys(locales)) {
    i18next.removeResourceBundle(language, 'translation')
  }
})

test('redirects the retired dashboard form to actual content without old controls or writes', async () => {
  const { put, router } = await renderSettings()
  expect(
    screen.queryByRole('switch', { name: 'Enable Data Dashboard' })
  ).not.toBeInTheDocument()
  expect(
    screen.queryByRole('spinbutton', { name: 'Refresh interval (minutes)' })
  ).not.toBeInTheDocument()
  expect(screen.queryByText('Default time granularity')).not.toBeInTheDocument()
  expect(await screen.findByText('Active announcement')).toBeVisible()
  expect(router.state.location.pathname).toBe(
    '/system-settings/content/announcements'
  )
  expect(put).not.toHaveBeenCalled()
  expect(getContentSectionNavItems(i18next.t).map((item) => item.url)).toEqual([
    '/system-settings/content/announcements',
    '/system-settings/content/api-info',
    '/system-settings/content/faq',
    '/system-settings/content/uptime-kuma',
    '/system-settings/content/chat',
    '/system-settings/content/drawing',
  ])
})

test.each([
  ['announcements', 'Announcements'],
  ['api-info', 'API Addresses'],
  ['faq', 'FAQ'],
  ['uptime-kuma', 'Uptime Kuma'],
  ['chat', 'Chat Presets'],
  ['drawing', 'Drawing'],
])(
  'keeps the real %s content section reachable without retired controls',
  async (section, title) => {
    const { router, put } = await renderSettings(section)
    expect(
      await screen.findAllByRole('heading', { name: title })
    ).not.toHaveLength(0)
    expect(router.state.location.pathname).toBe(
      `/system-settings/content/${section}`
    )
    expect(screen.queryByText('Enable Data Dashboard')).not.toBeInTheDocument()
    expect(
      screen.queryByText('Refresh interval (minutes)')
    ).not.toBeInTheDocument()
    expect(
      screen.queryByText('Default time granularity')
    ).not.toBeInTheDocument()
    expect(put).not.toHaveBeenCalled()
  }
)

test('keeps the root-only settings guard on the retired bookmark before reading options', async () => {
  const { get, put, router } = await renderSettings('dashboard', {}, ROLE.ADMIN)
  expect(router.state.location.pathname).toBe('/403')
  expect(get).not.toHaveBeenCalled()
  expect(put).not.toHaveBeenCalled()
})

test.each([
  {
    DataExportEnabled: 'true',
    DataExportInterval: '60',
    DataExportDefaultTime: 'week',
  },
  {
    DataExportEnabled: 'false',
    DataExportInterval: '-1',
    DataExportDefaultTime: 'invalid',
  },
  {
    DataExportEnabled: '',
    DataExportInterval: 'not a number',
    DataExportDefaultTime: '',
  },
])(
  'ignores historical statistics payload %j while saving only FAQ content',
  async (legacy) => {
    const { user, put, options } = await renderSettings('faq', legacy)
    expect(saveButton()).toBeDisabled()
    const draft = await addFAQ(user)
    expect(put).not.toHaveBeenCalled()
    await user.click(saveButton())
    await waitFor(() =>
      expect(put).toHaveBeenCalledExactlyOnceWith('/api/option/', {
        key: 'console_setting.faq',
        value: JSON.stringify(draft),
      })
    )
    await waitFor(() => expect(saveButton()).toBeDisabled())
    for (const [key, value] of Object.entries(legacy)) {
      expect(options[key]).toBe(value)
    }
    expect(options['console_setting.announcements_enabled']).toBe('true')
    expect(
      JSON.parse(options['console_setting.announcements'])[0].content
    ).toBe('Active announcement')
  }
)

test.each([
  ['announcements', 'console_setting.announcements_enabled'],
  ['api-info', 'console_setting.api_info_enabled'],
  ['faq', 'console_setting.faq_enabled'],
  ['uptime-kuma', 'console_setting.uptime_kuma_enabled'],
])(
  'preserves the real %s visibility option without writing collection defaults',
  async (section, key) => {
    const { user, put } = await renderSettings(section, { [key]: 'false' })
    expect(await screen.findByText('Enabled')).toBeVisible()
    const toggle = screen.getByRole('switch')
    expect(toggle).not.toBeChecked()
    await user.click(toggle)
    await waitFor(() =>
      expect(put).toHaveBeenCalledExactlyOnceWith('/api/option/', {
        key,
        value: true,
      })
    )
    await waitFor(() => expect(toggle).toBeChecked())
  }
)

test('preserves failed content drafts and sends the same scoped value on retry', async () => {
  const { user, put } = await renderSettings('faq')
  const draft = await addFAQ(user)
  put.mockResolvedValueOnce({
    data: { success: false, message: 'Content save refused' },
  })
  await user.click(saveButton())
  expect(await screen.findByText('Content save refused')).toBeVisible()
  expect(screen.getByText('Where is my history?')).toBeVisible()
  expect(saveButton()).toBeEnabled()
  await user.click(saveButton())
  await waitFor(() => expect(saveButton()).toBeDisabled())
  expect(put.mock.calls).toEqual([
    [
      '/api/option/',
      { key: 'console_setting.faq', value: JSON.stringify(draft) },
    ],
    [
      '/api/option/',
      { key: 'console_setting.faq', value: JSON.stringify(draft) },
    ],
  ])
  await user.click(saveButton())
  expect(put).toHaveBeenCalledTimes(2)
})

test('blocks repeated saves while the real content request is pending', async () => {
  const { user, put } = await renderSettings('faq')
  const draft = await addFAQ(user)
  let resolveSave: (response: { data: { success: boolean } }) => void = () => {
    throw new Error('Save not started')
  }
  const pending = new Promise<{ data: { success: boolean } }>((resolve) => {
    resolveSave = resolve
  })
  put.mockImplementationOnce(async () => pending)
  await user.dblClick(saveButton())
  expect(screen.getByRole('button', { name: 'Saving...' })).toBeDisabled()
  expect(put).toHaveBeenCalledExactlyOnceWith('/api/option/', {
    key: 'console_setting.faq',
    value: JSON.stringify(draft),
  })
  await act(async () => resolveSave({ data: { success: true } }))
  await waitFor(() => expect(saveButton()).toBeDisabled())
  expect(put).toHaveBeenCalledTimes(1)
})

test('does not replace personal Dashboard granularity with the retired site value', async () => {
  localStorage.setItem('data_export_default_time', 'week')
  await renderSettings('dashboard', { DataExportDefaultTime: 'hour' })
  expect(getSavedGranularity()).toBe('week')
  expect(localStorage.getItem('data_export_default_time')).toBe('week')
  saveGranularity('day')
  expect(getSavedGranularity()).toBe('day')
  expect(localStorage.getItem('data_export_default_time')).toBe('day')
  expect(
    JSON.parse(
      localStorage.getItem('dashboard_models_chart_preferences') || '{}'
    ).defaultTimeGranularity
  ).toBe('day')
})

test.each(Object.entries(locales))(
  'translates the controlled retirement response in %s and retains the content draft',
  async (language, resource) => {
    i18next.addResourceBundle(
      language,
      'translation',
      resource.translation,
      true,
      true
    )
    await i18next.changeLanguage(language)
    const { user, put } = await renderSettings('faq')
    const draft = await addFAQ(user)
    put.mockRejectedValueOnce({
      isAxiosError: true,
      response: {
        status: 410,
        data: {
          success: false,
          code: 'legacy_statistics_settings_retired',
          message: retirementMessage,
          message_key: retirementMessage,
        },
      },
    })
    await user.click(saveButton())
    const translated = resource.translation[retirementMessage]
    expect(translated).toBeTruthy()
    expect(await screen.findByText(translated)).toBeVisible()
    expect(screen.getByText('Where is my history?')).toBeVisible()
    expect(saveButton()).toBeEnabled()
    await user.click(saveButton())
    await waitFor(() => expect(saveButton()).toBeDisabled())
    expect(put.mock.calls).toEqual([
      [
        '/api/option/',
        { key: 'console_setting.faq', value: JSON.stringify(draft) },
      ],
      [
        '/api/option/',
        { key: 'console_setting.faq', value: JSON.stringify(draft) },
      ],
    ])
  }
)
