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
import { act, cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import i18next from 'i18next'
import { afterEach, expect, test, vi } from 'vitest'

import en from '@/i18n/locales/en.json'
import fr from '@/i18n/locales/fr.json'
import ja from '@/i18n/locales/ja.json'
import ru from '@/i18n/locales/ru.json'
import viLocale from '@/i18n/locales/vi.json'
import zhTW from '@/i18n/locales/zh-TW.json'
import zh from '@/i18n/locales/zh.json'
import { api } from '@/lib/api'

import { BillingSettings } from '../../billing'
import type { UpdateOptionRequest } from '../../types'

const clients: QueryClient[] = []
const retirementMessage = 'Legacy quota reservation settings have been retired.'
const savedLink = 'https://example.com/old-topup'
const nextLink = 'https://example.com/new-topup?plan=external'
const locales = { en, zh, 'zh-TW': zhTW, fr, ru, ja, vi: viLocale }

async function renderSettings(
  overrides: Record<string, string> = {},
  includeLegacy = true
) {
  const options: Record<string, string> = {
    TopUpLink: savedLink,
    ...(includeLegacy
      ? {
          'quota_setting.enable_free_model_pre_consume': 'true',
          'quota_setting.trust_quota_usd': '10',
          'quota_setting.pre_consume_multiplier': '1',
        }
      : {}),
    ...overrides,
  }
  vi.spyOn(api, 'get').mockImplementation(async (url) => {
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
  })
  const billing = createRoute({
    getParentRoute: () => settings,
    path: 'billing/$section',
    component: BillingSettings,
  })
  const router = createRouter({
    routeTree: root.addChildren([
      authenticated.addChildren([settings.addChildren([billing])]),
    ]),
    history: createMemoryHistory({
      initialEntries: ['/system-settings/billing/quota'],
    }),
  })
  render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
  const input = await screen.findByRole('textbox', {
    name: i18next.t('Top-Up Link'),
  })
  return { input, put, user: userEvent.setup() }
}

function saveButton() {
  return screen.getByRole('button', { name: i18next.t('Save Changes') })
}

function expectRetiredControlsAbsent() {
  expect(
    screen.queryByRole('spinbutton', {
      name: 'Wallet pre-consume bypass threshold (USD)',
    })
  ).not.toBeInTheDocument()
  expect(
    screen.queryByRole('spinbutton', { name: 'Input pre-consume multiplier' })
  ).not.toBeInTheDocument()
  expect(
    screen.queryByRole('switch', { name: 'Pre-Consume for Free Models' })
  ).not.toBeInTheDocument()
}

afterEach(async () => {
  cleanup()
  clients.splice(0).forEach((client) => client.clear())
  vi.restoreAllMocks()
  await i18next.changeLanguage('en')
  for (const language of Object.keys(locales)) {
    i18next.removeResourceBundle(language, 'translation')
  }
})

test.each(['0.5', '1.5', '2.5', '0.0001'])(
  'retires controls for historical multiplier %s and saves only the adjacent link',
  async (value) => {
    const { input, user, put } = await renderSettings({
      'quota_setting.pre_consume_multiplier': value,
    })
    expectRetiredControlsAbsent()
    expect(input).toHaveValue(savedLink)
    await user.clear(input)
    await user.type(input, nextLink)
    await user.click(saveButton())
    await waitFor(() =>
      expect(put).toHaveBeenCalledExactlyOnceWith('/api/option/', {
        key: 'TopUpLink',
        value: nextLink,
      })
    )
    expect(input).toHaveValue(nextLink)
    expectRetiredControlsAbsent()
  }
)

test.each(['0', '-0.5', ''])(
  'invalid historical multiplier "%s" cannot block saving the adjacent link',
  async (value) => {
    const { input, user, put } = await renderSettings({
      'quota_setting.pre_consume_multiplier': value,
    })
    expectRetiredControlsAbsent()
    await user.clear(input)
    await user.type(input, nextLink)
    await user.click(saveButton())
    await waitFor(() =>
      expect(put).toHaveBeenCalledExactlyOnceWith('/api/option/', {
        key: 'TopUpLink',
        value: nextLink,
      })
    )
    expect(input).not.toHaveAttribute('aria-invalid', 'true')
  }
)

test('zero historical threshold and disabled free-model reservation remain read-only source data', async () => {
  const { input, user, put } = await renderSettings({
    'quota_setting.trust_quota_usd': '0',
    'quota_setting.enable_free_model_pre_consume': 'false',
  })
  expectRetiredControlsAbsent()
  await user.clear(input)
  await user.click(saveButton())
  await waitFor(() =>
    expect(put).toHaveBeenCalledExactlyOnceWith('/api/option/', {
      key: 'TopUpLink',
      value: '',
    })
  )
  expect(input).toHaveValue('')
})

test.each(['', '-1'])(
  'invalid historical threshold "%s" cannot block saving the adjacent link',
  async (value) => {
    const { input, user, put } = await renderSettings({
      'quota_setting.trust_quota_usd': value,
    })
    expectRetiredControlsAbsent()
    await user.clear(input)
    await user.type(input, nextLink)
    await user.click(saveButton())
    await waitFor(() =>
      expect(put).toHaveBeenCalledExactlyOnceWith('/api/option/', {
        key: 'TopUpLink',
        value: nextLink,
      })
    )
    expect(input).not.toHaveAttribute('aria-invalid', 'true')
  }
)

test('missing retired options do not recreate default controls or send an unchanged link', async () => {
  const { input, user, put } = await renderSettings({}, false)
  expectRetiredControlsAbsent()
  expect(input).toHaveValue(savedLink)
  await user.click(saveButton())
  expect(put).not.toHaveBeenCalled()
  await user.clear(input)
  await user.type(input, nextLink)
  await user.click(saveButton())
  await waitFor(() =>
    expect(put).toHaveBeenCalledExactlyOnceWith('/api/option/', {
      key: 'TopUpLink',
      value: nextLink,
    })
  )
  expectRetiredControlsAbsent()
})

test.each(['server', 'network'])(
  '%s save failure retains the link draft and allows retry without retired writes',
  async (failure) => {
    const { input, user, put } = await renderSettings()
    if (failure === 'server') {
      put.mockResolvedValueOnce({
        data: { success: false, message: 'Link save rejected' },
      })
    } else {
      put.mockRejectedValueOnce(new Error('Link save rejected'))
    }
    await user.clear(input)
    await user.type(input, nextLink)
    await user.click(saveButton())
    expect(await screen.findByText('Link save rejected')).toBeVisible()
    expect(input).toHaveValue(nextLink)
    expect(input).toHaveAttribute('aria-invalid', 'true')
    await user.click(saveButton())
    await waitFor(() => expect(put).toHaveBeenCalledTimes(2))
    for (const call of put.mock.calls) {
      expect(call).toEqual([
        '/api/option/',
        { key: 'TopUpLink', value: nextLink },
      ])
    }
    await waitFor(() =>
      expect(screen.queryByText('Link save rejected')).not.toBeInTheDocument()
    )
    expect(input).toHaveValue(nextLink)
    expectRetiredControlsAbsent()
  }
)

test('pending link save disables duplicate submission and retains the draft', async () => {
  const { input, user, put } = await renderSettings()
  let complete: () => void = () => {
    throw new Error('Save not started')
  }
  put.mockImplementationOnce(
    () =>
      new Promise((resolve) => {
        complete = () => resolve({ data: { success: true } })
      })
  )
  await user.clear(input)
  await user.type(input, nextLink)
  await user.click(saveButton())
  const saving = await screen.findByRole('button', { name: 'Saving...' })
  expect(saving).toBeDisabled()
  expect(input).toHaveValue(nextLink)
  await user.click(saving)
  expect(put).toHaveBeenCalledExactlyOnceWith('/api/option/', {
    key: 'TopUpLink',
    value: nextLink,
  })
  await act(async () => complete())
  await waitFor(() => expect(saveButton()).toBeEnabled())
})

test.each([
  ['en', 'Legacy quota reservation settings have been retired.'],
  ['zh', '旧额度预扣设置已退役。'],
  ['zh-TW', '舊額度預扣設定已退役。'],
  ['fr', 'Les anciens paramètres de réservation de quota ont été retirés.'],
  ['ru', 'Устаревшие настройки резервирования квоты выведены из эксплуатации.'],
  ['ja', '旧クォータ事前確保設定は廃止されました。'],
  ['vi', 'Các cài đặt giữ trước hạn mức cũ đã ngừng hoạt động.'],
] as const)(
  'shows the retired API error in %s without losing the link draft',
  async (language, message) => {
    for (const [code, resource] of Object.entries(locales)) {
      i18next.addResourceBundle(code, 'translation', resource.translation)
    }
    await i18next.changeLanguage(language)
    const { input, user, put } = await renderSettings()
    put.mockRejectedValueOnce({
      response: {
        status: 410,
        data: {
          success: false,
          message: retirementMessage,
          message_key: retirementMessage,
          code: 'legacy_quota_settings_retired',
        },
      },
    })
    await user.clear(input)
    await user.type(input, nextLink)
    await user.click(saveButton())
    expect(await screen.findByText(message)).toBeVisible()
    expect(input).toHaveValue(nextLink)
    expect(put).toHaveBeenCalledExactlyOnceWith('/api/option/', {
      key: 'TopUpLink',
      value: nextLink,
    })
  }
)
