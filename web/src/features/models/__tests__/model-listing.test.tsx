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
  render,
  screen,
  within,
  cleanup,
  waitFor,
  act,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import i18n from 'i18next'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import { OfferingCatalog, OfferingPrice } from '@/features/pricing'
import type { CatalogOffering } from '@/features/pricing/catalog'
import en from '@/i18n/locales/en.json'
import fr from '@/i18n/locales/fr.json'
import ja from '@/i18n/locales/ja.json'
import ru from '@/i18n/locales/ru.json'
import viLocale from '@/i18n/locales/vi.json'
import zhTW from '@/i18n/locales/zh-TW.json'
import zhCN from '@/i18n/locales/zh.json'
import { api } from '@/lib/api'
import { getServerErrorMessage } from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'
import {
  DEFAULT_CURRENCY_CONFIG,
  useSystemConfigStore,
} from '@/stores/system-config-store'

import { ModelsDialogs } from '../components/models-dialogs'
import { ModelsPrimaryButtons } from '../components/models-primary-buttons'
import { ModelsProvider } from '../components/models-provider'
import { ModelsTable } from '../components/models-table'
import type { Model, PrefillGroup } from '../types'

const metadata: Model = {
  id: 7,
  model_name: 'catalog-only',
  square_state: 'unavailable',
  has_metadata: true,
  configured_channel_count: 0,
  enabled_channel_count: 0,
  name_rule: 0,
  status: 1,
  sync_official: 1,
  created_time: 1,
  updated_time: 1,
}
const channel: Model = {
  ...metadata,
  id: 8,
  model_name: 'channel-only',
  has_metadata: true,
  configured_channel_count: 1,
  status: 1,
  sync_official: 0,
}
const clients: QueryClient[] = []

function Page() {
  return (
    <ModelsProvider>
      <ModelsPrimaryButtons />
      <ModelsTable />
      <ModelsDialogs />
    </ModelsProvider>
  )
}

function saleOffering(expression: string, currency = 'EUR'): CatalogOffering {
  return {
    id: 11,
    name: 'sale-offering',
    aliases: ['sale-alias'],
    members: [
      {
        model_id: 7,
        name: 'channel-only',
        tier: 1,
        description: '',
        icon: '',
        tags: '',
        publisher_id: 0,
        publisher_name: '',
        capabilities: null,
      },
    ],
    price: { id: 23, expression, currency, effective_at: 1 },
  }
}

async function renderSaleCatalog(
  offering: CatalogOffering | Promise<CatalogOffering>
) {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'user', role: 1 })
  const get = vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url !== '/api/pricing') throw new Error(`Unexpected request: ${url}`)
    return { data: { success: true, data: [await offering], as_of: 100 } }
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.push(client)
  render(
    <QueryClientProvider client={client}>
      <OfferingCatalog />
    </QueryClientProvider>
  )
  return get
}

async function renderList(
  items: Model[] = [
    metadata,
    channel,
    { ...channel, id: 9, model_name: 'other-channel' },
  ],
  options: {
    initialUrl?: string
    total?: number
    prefillGroups?: PrefillGroup[]
  } = {}
) {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'admin', role: 100 })
  const get = vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/models/' || url === '/api/models/search') {
      return {
        data: {
          success: true,
          data: { items, total: options.total ?? items.length },
        },
      }
    }
    if (url === '/api/prefill_group') {
      return { data: { success: true, data: options.prefillGroups ?? [] } }
    }
    const detail = items.find((item) => url === `/api/models/${item.id}`)
    if (detail) return { data: { success: true, data: detail } }
    return { data: { success: true, data: { items: [] } } }
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
  const models = createRoute({
    getParentRoute: () => authenticated,
    path: 'models/$section',
    component: Page,
  })
  const router = createRouter({
    routeTree: root.addChildren([authenticated.addChildren([models])]),
    history: createMemoryHistory({
      initialEntries: [options.initialUrl ?? '/models/metadata'],
    }),
  })
  await router.load()
  const result = render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
  if (items.length) {
    await screen.findByRole('button', { name: items[0].model_name })
  } else await screen.findByText('No Models Found')
  await waitFor(() => expect(client.isFetching()).toBe(0))
  expect(
    get.mock.calls.some(([url]) =>
      String(url).startsWith('/api/option/model_pricing')
    )
  ).toBe(false)
  return { ...result, get, router }
}

beforeEach(() => {
  useSystemConfigStore.getState().setConfig({
    currency: { ...DEFAULT_CURRENCY_CONFIG, quotaDisplayType: 'USD' },
  })
  vi.spyOn(window, 'scrollTo').mockImplementation(() => {})
  i18n.addResourceBundle('fr', 'translation', fr.translation, true, true)
  i18n.addResourceBundle('zhCN', 'translation', zhCN.translation, true, true)
})

afterEach(async () => {
  cleanup()
  clients.splice(0).forEach((client) => client.clear())
  useAuthStore.getState().auth.reset()
  useSystemConfigStore
    .getState()
    .setConfig({ currency: { ...DEFAULT_CURRENCY_CONFIG } })
  await i18n.changeLanguage('en')
})

it('localizes model drawer guidance and rename boundaries when the language changes', async () => {
  await renderList([metadata])
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'catalog-only' }))
  const input = await screen.findByRole('textbox', { name: /Model Name/ })
  await user.clear(input)
  await user.type(input, 'renamed-catalog')
  await act(() => i18n.changeLanguage('zhCN'))
  const drawer = screen.getByRole('dialog', { name: 'catalog-only' })
  expect
    .soft(drawer)
    .toHaveTextContent('管理元信息、能力和通道关联。各部分分别保存。')
  expect
    .soft(drawer)
    .toHaveTextContent('重命名元信息不会更改对外模型名称或通道模型名。')
  expect.soft(drawer).toHaveTextContent('匹配规则仅用于元信息。')
  await act(() => i18n.changeLanguage('fr'))
  expect
    .soft(drawer)
    .toHaveTextContent(
      'Gérez les métadonnées, les capacités et les connexions aux canaux. Chaque section est enregistrée séparément.'
    )
  expect
    .soft(drawer)
    .toHaveTextContent(
      'Renommer les métadonnées ne modifie ni les noms des Offerings ni les modèles des canaux.'
    )
  expect
    .soft(drawer)
    .toHaveTextContent(
      'Les règles de correspondance concernent uniquement les métadonnées.'
    )
})

it('requests saved Models and explains exact-ID Channel counts without Offering claims', async () => {
  const { get } = await renderList()
  expect(get).toHaveBeenCalledWith('/api/models/', {
    params: expect.objectContaining({ include_channel_models: true }),
  })
  expect(screen.getAllByText('No enabled Channels')).toHaveLength(3)
  const warning = screen
    .getAllByText('No enabled Channels')[0]
    .closest('[data-slot="status-badge"]')
  expect(warning).toHaveClass('text-warning')
  expect(warning?.querySelector('svg')).toBeInTheDocument()
  expect(warning).not.toHaveClass('text-success')
  expect(screen.queryByText('Missing metadata')).not.toBeInTheDocument()
  expect(screen.getByText('Configured 0 · Enabled 0')).toBeVisible()
  expect(screen.getAllByText('Configured 1 · Enabled 0')).toHaveLength(2)
  expect(
    screen.queryByText('No Channel is configured for this Model ID.')
  ).not.toBeInTheDocument()
  const user = userEvent.setup()
  const trigger = screen.getAllByRole('button', {
    name: 'No enabled Channels',
  })[0]
  expect(trigger).toHaveAttribute(
    'title',
    'No Channel is configured for this Model ID.'
  )
  await user.click(trigger)
  expect(
    await screen.findByRole('dialog', { name: 'No enabled Channels' })
  ).toHaveTextContent('No Channel is configured for this Model ID.')
  expect(trigger).toHaveAttribute('aria-expanded', 'true')
  await user.keyboard('{Escape}')
  await waitFor(() =>
    expect(
      screen.queryByRole('dialog', { name: 'No enabled Channels' })
    ).not.toBeInTheDocument()
  )
  expect(trigger).toHaveFocus()
  await user.click(screen.getByRole('button', { name: 'catalog-only' }))
  await user.click(screen.getByRole('tab', { name: 'Channels' }))
  expect(
    screen.getByText('No Channel is configured for this Model ID.')
  ).toBeVisible()
})

it('keeps stable Model IDs individually selectable and preserves metadata-only bulk mutations', async () => {
  await renderList()
  const user = userEvent.setup()
  await user.click(
    screen.getByRole('checkbox', { name: 'Select channel-only' })
  )
  expect(
    screen.getByRole('checkbox', { name: 'Select channel-only' })
  ).toBeChecked()
  expect(
    screen.getByRole('checkbox', { name: 'Select other-channel' })
  ).not.toBeChecked()
  await user.click(
    screen.getByRole('checkbox', { name: 'Select catalog-only' })
  )
  expect(
    screen.getByRole('checkbox', { name: 'Select channel-only' })
  ).toBeChecked()
  const toolbar = screen.getByRole('toolbar', { name: /Bulk actions/ })
  for (const name of [
    'Change vendor',
    'Clear vendor',
    'Enable selected Model metadata',
    'Disable selected Model metadata',
    'Delete selected models',
  ]) {
    expect(within(toolbar).getByRole('button', { name })).toBeEnabled()
  }
  expect(
    within(toolbar).getByRole('button', { name: 'Copy model names' })
  ).toBeEnabled()
  await user.click(
    screen.getByRole('checkbox', { name: 'Select channel-only' })
  )
  expect(
    within(toolbar).getByRole('button', { name: 'Delete selected models' })
  ).toBeEnabled()
})

it('keeps long model names and translated channel labels truncated inside their existing cells', async () => {
  const longName =
    'provider/very-long-channel-model-with-detailed-version-and-context-window'
  await renderList([
    { ...channel, model_name: longName },
    { ...metadata, status: 0, square_state: 'hidden' },
  ])
  expect(screen.getByText(longName)).toHaveClass('truncate')
  expect(screen.queryByText('Add metadata')).not.toBeInTheDocument()
  await act(async () => {
    await i18n.changeLanguage('fr')
  })
  const label = screen.getAllByText(
    i18n.t('Configured {{configured}} · Enabled {{enabled}}', {
      configured: 0,
      enabled: 0,
    })
  )[0]
  expect(label).toHaveClass('whitespace-normal', 'sm:truncate')
  expect(label).toHaveAttribute('tabindex', '0')
  await act(async () => {
    await i18n.changeLanguage('zhCN')
  })
  expect(screen.getByText(i18n.t('Metadata disabled'))).toBeVisible()
  expect(
    screen.getByText(
      i18n.t('Configured {{configured}} · Enabled {{enabled}}', {
        configured: 1,
        enabled: 0,
      })
    )
  ).toBeVisible()
})

const missingModelsRetiredMessage =
  'Legacy missing-model discovery has been retired. Use Models and metadata sync.'

it.each([
  ['en', en, missingModelsRetiredMessage],
  ['zh', zhCN, '旧版缺失模型发现已退役。请使用模型管理和元数据同步。'],
  ['zh-TW', zhTW, '舊版缺失模型探索已退役。請使用模型管理與中繼資料同步。'],
  [
    'fr',
    fr,
    'L’ancienne détection des modèles manquants a été retirée. Utilisez la gestion des modèles et la synchronisation des métadonnées.',
  ],
  [
    'ru',
    ru,
    'Устаревший поиск отсутствующих моделей выведен из эксплуатации. Используйте управление моделями и синхронизацию метаданных.',
  ],
  [
    'ja',
    ja,
    '旧式の不足モデル検出は廃止されました。モデル管理とメタデータ同期を使用してください。',
  ],
  [
    'vi',
    viLocale,
    'Cơ chế phát hiện mô hình còn thiếu cũ đã ngừng hoạt động. Hãy sử dụng quản lý mô hình và đồng bộ siêu dữ liệu.',
  ],
] as const)(
  'localizes the retired discovery HTTP response in %s',
  async (language, resource, expected) => {
    i18n.addResourceBundle(
      language,
      'translation',
      resource.translation,
      true,
      true
    )
    await i18n.changeLanguage(language)
    expect(
      getServerErrorMessage({
        response: {
          status: 410,
          headers: { 'cache-control': 'no-store' },
          data: {
            success: false,
            code: 'legacy_missing_models_retired',
            message: missingModelsRetiredMessage,
            message_key: missingModelsRetiredMessage,
          },
        },
      })
    ).toBe(expected)
  }
)

it('removes missing-model discovery from the menu while preserving model, tag and endpoint presets', async () => {
  const { get } = await renderList([channel], {
    prefillGroups: [
      { id: 1, name: 'Model preset', type: 'model', items: ['channel-only'] },
      { id: 2, name: 'Tag preset', type: 'tag', items: ['tag-value'] },
      { id: 3, name: 'Endpoint preset', type: 'endpoint', items: ['openai'] },
    ],
  })
  const user = userEvent.setup()
  await user.click(screen.getAllByRole('button', { name: 'Open menu' })[0])
  expect(
    screen.queryByRole('menuitem', { name: 'Missing Models' })
  ).not.toBeInTheDocument()
  await user.click(screen.getByRole('menuitem', { name: 'Prefill Groups' }))
  const dialog = await screen.findByRole('dialog', {
    name: 'Prefill Group Management',
  })
  for (const name of ['Model preset', 'Tag preset', 'Endpoint preset']) {
    expect(await within(dialog).findByText(name)).toBeVisible()
  }
  expect(within(dialog).getByText('channel-only')).toBeVisible()
  expect(within(dialog).getByText('tag-value')).toBeVisible()
  expect(within(dialog).getByText('openai')).toBeVisible()
  expect(get).toHaveBeenCalledWith('/api/prefill_group', {
    params: undefined,
  })
  expect(get.mock.calls.some(([url]) => url === '/api/models/missing')).toBe(
    false
  )
})

it('creates metadata only after an explicit Add Model and Save action', async () => {
  await renderList([channel])
  const post = vi.spyOn(api, 'post').mockResolvedValue({
    data: { success: true, data: { ...channel, id: 9, has_metadata: true } },
  })
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'Add Model' }))
  const name = screen.getByRole('textbox', { name: 'Model Name *' })
  expect(name).toHaveValue('')
  await user.type(name, 'new-metadata')
  expect(post).not.toHaveBeenCalled()
  await user.click(screen.getByRole('button', { name: 'Save metadata' }))
  await waitFor(() =>
    expect(post).toHaveBeenCalledWith(
      '/api/models/',
      expect.objectContaining({ model_name: 'new-metadata' }),
      expect.anything()
    )
  )
})

it('keeps native Offering prices and channel explanations readable on mobile without model-name pricing', async () => {
  const originalMatchMedia = window.matchMedia
  vi.spyOn(window, 'matchMedia').mockImplementation((query) => ({
    ...originalMatchMedia(query),
    matches: query === '(max-width: 640px)',
  }))
  await renderList([metadata])
  expect(
    screen.queryByRole('columnheader', { name: 'Pricing' })
  ).not.toBeInTheDocument()
  const nativePrice = saleOffering('tier("base", p * 1.25 + c * 10)')
  render(<OfferingPrice offering={nativePrice} tokenUnit='M' />)
  expect(screen.getByText('p: 1.25 EUR / 1M tokens')).toHaveClass('break-all')
  expect(screen.getByLabelText('Complete price expression')).toHaveClass(
    'min-w-0',
    'overflow-x-auto',
    'whitespace-pre-wrap',
    'break-all'
  )
  expect(screen.getByText('Configured 0 · Enabled 0')).toHaveClass(
    'whitespace-normal'
  )
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'catalog-only' }))
  await user.click(screen.getByRole('tab', { name: 'Channels' }))
  expect(
    screen.getByText('No Channel is configured for this Model ID.')
  ).toBeVisible()
})

it('explains real Channel counts and metadata status without turning them into Offering visibility', async () => {
  await renderList([
    metadata,
    {
      ...metadata,
      id: 8,
      model_name: 'enabled-model',
      configured_channel_count: 1,
      enabled_channel_count: 1,
    },
    {
      ...metadata,
      id: 9,
      model_name: 'disabled-model',
      status: 0,
      configured_channel_count: 1,
      enabled_channel_count: 1,
    },
    { ...metadata, id: 10, model_name: 'metadata-rule', name_rule: 1 },
    {
      ...metadata,
      id: 11,
      model_name: 'pending-model',
      configured_channel_count: 2,
      enabled_channel_count: 0,
    },
    {
      ...metadata,
      id: 12,
      model_name: 'unknown-counts',
      configured_channel_count: undefined,
      enabled_channel_count: undefined,
    },
  ])
  expect(screen.getByRole('button', { name: 'Metadata status' })).toBeVisible()
  for (const label of screen.getAllByText('No enabled Channels')) {
    const badge = label.closest('[data-slot="status-badge"]')
    expect(badge).toHaveClass('text-warning')
    expect(badge?.querySelector('svg')).toBeInTheDocument()
    expect(badge).not.toHaveClass('text-success')
  }
  expect(
    screen.getByText('Enabled Channels').closest('[data-slot="status-badge"]')
  ).toHaveClass('text-success')
  expect(
    screen.getByText('Metadata disabled').closest('[data-slot="status-badge"]')
  ).toHaveClass('text-muted-foreground')
  expect(screen.getByText('Metadata rule')).toBeVisible()
  expect(screen.queryByText('Partly shown')).not.toBeInTheDocument()
  const unknown = screen
    .getByRole('button', { name: 'unknown-counts' })
    .closest('tr')
  expect(unknown).not.toHaveTextContent('Configured 0')
  expect(unknown).not.toHaveTextContent('No enabled Channels')
})

it('opens the warning icon’s own model reason and closes it on an outside click without selecting the row', async () => {
  await renderList([metadata, channel])
  const user = userEvent.setup()
  const triggers = screen.getAllByRole('button', {
    name: 'No enabled Channels',
  })
  const icon = triggers[1].querySelector('svg')
  expect(icon).not.toBeNull()
  await user.click(icon as SVGElement)
  const popup = await screen.findByRole('dialog', {
    name: 'No enabled Channels',
  })
  expect(popup).toHaveTextContent(
    'No Channel has an enabled Provider and a confirmed, enabled mapping for this Model ID.'
  )
  expect(popup).not.toHaveTextContent(
    'No Channel is configured for this Model ID.'
  )
  expect(
    screen.getByRole('checkbox', { name: 'Select channel-only' })
  ).not.toBeChecked()
  await user.click(screen.getByPlaceholderText('Filter by model name...'))
  await waitFor(() =>
    expect(
      screen.queryByRole('dialog', { name: 'No enabled Channels' })
    ).not.toBeInTheDocument()
  )
  expect(triggers[1]).toHaveAttribute('aria-expanded', 'false')
  await user.click(triggers[0])
  expect(
    await screen.findByRole('dialog', { name: 'No enabled Channels' })
  ).toHaveTextContent('No Channel is configured for this Model ID.')
})

it('opens a long translated reason by touch in the mobile card without requiring the detail drawer', async () => {
  const originalMatchMedia = window.matchMedia
  vi.spyOn(window, 'matchMedia').mockImplementation((query) => ({
    ...originalMatchMedia(query),
    matches: query === '(max-width: 640px)',
  }))
  await renderList([metadata])
  await act(async () => {
    await i18n.changeLanguage('fr')
  })
  const user = userEvent.setup()
  const trigger = screen.getByRole('button', {
    name: i18n.t('No enabled Channels'),
  })
  await user.pointer([
    { keys: '[TouchA>]', target: trigger },
    { keys: '[/TouchA]' },
  ])
  const popup = await screen.findByRole('dialog', {
    name: i18n.t('No enabled Channels'),
  })
  expect(popup).toHaveTextContent(
    i18n.t('No Channel is configured for this Model ID.')
  )
  expect(popup).toHaveClass('whitespace-normal', 'break-words')
  expect(popup).toHaveClass('max-w-[calc(100vw-2rem)]')
  expect(
    screen.queryByRole('dialog', { name: metadata.model_name })
  ).not.toBeInTheDocument()
})

it.each([
  {
    name: 'migrated-token-price',
    expression: 'tier("base", p * 3 + c * 15)',
    summary: 'p: 3 EUR / 1M tokens',
  },
  {
    name: 'single-expression',
    expression: 'tier("base", p * 3 + c * 15 + cr * 0.3)',
    summary: 'cr: 0.3 EUR / 1M tokens',
  },
  {
    name: 'tier-expression',
    expression:
      'len <= 200000 ? tier("standard", p * 3 + c * 15) : tier("long", p * 6 + c * 22.5)',
    summary: null,
  },
  {
    name: 'free-request',
    expression: 'tier("free", fixed(0))',
    summary: '0 EUR / request',
  },
  {
    name: 'free-tokens',
    expression: 'tier("base", p * 0 + c * 0)',
    summary: 'p: 0 EUR / 1M tokens',
  },
  {
    name: 'free-expression',
    expression: 'tier("free", p * 0 + c * 0)',
    summary: 'c: 0 EUR / 1M tokens',
  },
])(
  'shows $name as an immutable native-currency Offering price without model metadata or legacy ratios',
  async ({ expression, summary }) => {
    const get = await renderSaleCatalog(saleOffering(expression))
    expect(
      await screen.findByLabelText('Complete price expression')
    ).toHaveTextContent(expression)
    if (summary) expect(screen.getByText(summary)).toBeVisible()
    expect(screen.queryByText('No current sale price')).not.toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'sale-offering' }))
    const detail = within(
      await screen.findByRole('dialog', { name: 'sale-offering' })
    )
    expect(
      detail.getByLabelText('Complete price expression')
    ).toHaveTextContent(expression)
    if (summary) expect(detail.getByText(summary)).toBeVisible()
    expect(get.mock.calls.every(([url]) => url === '/api/pricing')).toBe(true)
  }
)

it('preserves task tiers and usage variables in their native currency without site FX or per-million conversion', async () => {
  useSystemConfigStore.getState().setConfig({
    currency: {
      ...DEFAULT_CURRENCY_CONFIG,
      quotaDisplayType: 'CNY',
      usdExchangeRate: 7,
    },
  })
  const expression =
    'u("mode") == "pro" ? tier("pro", u("seconds") * 0.8) : tier("std", u("seconds") * 0.4)'
  await renderSaleCatalog(saleOffering(expression))
  expect(
    await screen.findByLabelText('Complete price expression')
  ).toHaveTextContent(expression)
  expect(screen.getByText('EUR', { exact: true })).toBeVisible()
  expect(screen.queryByText(/2\.8.*5\.6/)).not.toBeInTheDocument()
  expect(screen.queryByText('CNY')).not.toBeInTheDocument()
  expect(
    screen.getByText(
      'Token coefficients are per 1M tokens; fixed() is per request; expressions using u() return amounts in the expression currency.'
    )
  ).toBeVisible()
})

it('opens the complete conditional and request expression without creating model metadata', async () => {
  const expression =
    '(len <= 200000 ? tier("standard", p * 3 + c * 15 + cr * 0.3) : tier("long", p * 6 + c * 22.5 + cr * 0.6)) * (header("x-priority") == "high" ? 2 : 1)'
  await renderSaleCatalog(saleOffering(expression))
  const write = vi.spyOn(api, 'post')
  await userEvent.click(
    await screen.findByRole('button', { name: 'sale-offering' })
  )
  const detail = within(
    await screen.findByRole('dialog', { name: 'sale-offering' })
  )
  expect(detail.getByLabelText('Complete price expression')).toHaveTextContent(
    expression
  )
  for (const fragment of ['standard', 'long', '0.3', '0.6', 'x-priority']) {
    expect(
      detail.getByLabelText('Complete price expression')
    ).toHaveTextContent(fragment)
  }
  expect(write).not.toHaveBeenCalled()
})

it('keeps an unrecognized expression complete rather than extracting a partial sale price', async () => {
  const expression = 'tier("custom", max(p * 2 + c * 8, 100))'
  await renderSaleCatalog(saleOffering(expression))
  expect(
    await screen.findByLabelText('Complete price expression')
  ).toHaveTextContent(expression)
  expect(screen.queryByText('p: 2 EUR / 1M tokens')).not.toBeInTheDocument()
  await userEvent.click(screen.getByRole('button', { name: 'sale-offering' }))
  expect(
    within(
      await screen.findByRole('dialog', { name: 'sale-offering' })
    ).getByLabelText('Complete price expression')
  ).toHaveTextContent(expression)
})

it('distinguishes a pending or failed catalog from a successfully loaded missing price and allows retry', async () => {
  let rejectPricing: (reason: Error) => void = () => {}
  const pending = new Promise<CatalogOffering>((_resolve, reject) => {
    rejectPricing = reject
  })
  const get = await renderSaleCatalog(pending)
  expect(screen.getByRole('status', { name: 'Loading...' })).toBeVisible()
  expect(screen.queryByText('No current sale price')).not.toBeInTheDocument()
  await act(async () => rejectPricing(new Error('pricing unavailable')))
  expect(
    await screen.findByText('Unable to load Offering prices')
  ).toBeVisible()
  expect(screen.queryByText('No current sale price')).not.toBeInTheDocument()
  get.mockResolvedValue({
    data: {
      success: true,
      data: [{ ...saleOffering('fixed(0)'), price: null }],
      as_of: 101,
    },
  })
  await userEvent.click(screen.getByRole('button', { name: 'Retry' }))
  expect(await screen.findByText('No current sale price')).toBeVisible()
  expect(screen.queryByText('0 EUR / request')).not.toBeInTheDocument()
})

it('filters Channel status independently of metadata status and restores filters through browser history', async () => {
  const { get, router } = await renderList([channel], {
    initialUrl: '/models/metadata?page=3&status=%5B%22enabled%22%5D',
    total: 100,
  })
  const user = userEvent.setup()
  await user.click(
    screen.getByRole('button', { name: 'Metadata and Channel status' })
  )
  await user.click(screen.getByRole('option', { name: 'Enabled Channels' }))
  await waitFor(() =>
    expect(get).toHaveBeenCalledWith('/api/models/search', {
      params: expect.objectContaining({
        status: 'enabled',
        square_state: 'visible',
        p: 1,
      }),
    })
  )
  expect(router.state.location.search).toMatchObject({
    status: ['enabled'],
    square_state: ['visible'],
  })
  await user.keyboard('{Escape}')
  await act(async () => {
    router.history.back()
  })
  await waitFor(() =>
    expect(router.state.location.search).not.toHaveProperty('square_state')
  )
  await act(async () => {
    router.history.forward()
  })
  await waitFor(() =>
    expect(router.state.location.search).toMatchObject({
      square_state: ['visible'],
    })
  )
  await user.click(
    screen.getByRole('button', { name: /Metadata and Channel status/ })
  )
  await user.click(screen.getByRole('option', { name: 'Clear filters' }))
  await waitFor(() =>
    expect(router.state.location.search).not.toHaveProperty('square_state')
  )
  expect(router.state.location.search).toMatchObject({ status: ['enabled'] })
})

it('keeps all columns while collapsing tags and connection counts', async () => {
  await renderList([
    {
      ...channel,
      tags: 'Tools,Files,Vision',
      configured_channel_count: 2,
      enabled_channel_count: 1,
    },
  ])
  expect(screen.getByText('Configured 2 · Enabled 1')).toBeVisible()
  expect(
    screen.getByRole('columnheader', { name: 'Sync policy' })
  ).toBeVisible()
  expect(screen.getByRole('columnheader', { name: 'Tags' })).toBeVisible()
  expect(screen.getByText('Tools')).toBeVisible()
  expect(screen.queryByText('Files')).not.toBeInTheDocument()
  await userEvent.click(screen.getByRole('button', { name: 'Show all tags' }))
  expect(await screen.findByText('Files')).toBeVisible()
  expect(screen.getByText('Vision')).toBeVisible()
})

it('keeps an active visibility filter when its server result is empty', async () => {
  const { get } = await renderList([], {
    initialUrl: '/models/metadata?square_state=%5B%22unavailable%22%5D',
  })
  expect(screen.getByText('No Models Found')).toBeVisible()
  expect(screen.getByText('Try adjusting your search')).toBeVisible()
  expect(
    screen.getByRole('button', {
      name: /Metadata and Channel status.*No enabled Channels/,
    })
  ).toBeVisible()
  expect(get).toHaveBeenCalledWith('/api/models/search', {
    params: expect.objectContaining({ square_state: 'unavailable', p: 1 }),
  })
})

it.each(['CUSTOM', 'TOKENS'] as const)(
  'keeps the PriceVersion currency and exact token prices in %s site display mode',
  async (type) => {
    useSystemConfigStore.getState().setConfig({
      currency: {
        ...DEFAULT_CURRENCY_CONFIG,
        quotaDisplayType: type,
        customCurrencySymbol: 'CAT',
        customCurrencyExchangeRate: 7,
      },
    })
    const expression = 'tier("base", p * 0.100000000000000001 + c * 1)'
    await renderSaleCatalog(saleOffering(expression))
    expect(
      await screen.findByText('p: 0.100000000000000001 EUR / 1M tokens')
    ).toBeVisible()
    expect(screen.getByText('c: 1 EUR / 1M tokens')).toBeVisible()
    expect(
      screen.getByLabelText('Complete price expression')
    ).toHaveTextContent(expression)
    expect(screen.queryByText('CAT')).not.toBeInTheDocument()
    expect(screen.queryByText('USD')).not.toBeInTheDocument()
  }
)
