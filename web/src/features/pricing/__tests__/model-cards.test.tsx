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
  act,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createJSONStorage } from 'zustand/middleware'

import fr from '@/i18n/locales/fr.json'
import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'
import {
  DEFAULT_CURRENCY_CONFIG,
  useSystemConfigStore,
} from '@/stores/system-config-store'

import type { CatalogOffering } from '../catalog'
import { CachedPriceCell } from '../components/cached-price-cell'
import { ModelCard } from '../components/model-card'
import { ModelCardGrid } from '../components/model-card-grid'
import { OfferingCatalog } from '../index'
import type { PricingModel } from '../types'

function pricingModel(overrides: Partial<PricingModel> = {}): PricingModel {
  return {
    id: 1,
    model_name: 'example-model',
    quota_type: 0,
    model_ratio: 1,
    completion_ratio: 3,
    enable_groups: ['default', 'premium'],
    group_ratio: { default: 1, premium: 3 },
    ...overrides,
  }
}

let queryClient: QueryClient
const originalStorage = useSystemConfigStore.persist.getOptions().storage
beforeEach(() => {
  const storage = new Map<string, string>()
  vi.stubGlobal('localStorage', {
    getItem: (key: string) => storage.get(key) ?? null,
    setItem: (key: string, value: string) => storage.set(key, value),
    removeItem: (key: string) => storage.delete(key),
    clear: () => storage.clear(),
    key: (index: number) => [...storage.keys()][index] ?? null,
    get length() {
      return storage.size
    },
  })
  useSystemConfigStore.persist.setOptions({
    storage: createJSONStorage(() => localStorage),
  })
  useSystemConfigStore.getState().setConfig({
    currency: { ...DEFAULT_CURRENCY_CONFIG, quotaDisplayType: 'USD' },
  })
  queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  })
})
afterEach(() => {
  queryClient.clear()
  useSystemConfigStore
    .getState()
    .setConfig({ currency: { ...DEFAULT_CURRENCY_CONFIG } })
  vi.useRealTimers()
  vi.unstubAllGlobals()
  useSystemConfigStore.persist.setOptions({ storage: originalStorage })
})

describe('model cards', () => {
  it('shows separate generic and image cache prices including a free image cache', () => {
    render(
      <CachedPriceCell
        model={pricingModel({
          billing_mode: 'tiered_expr',
          billing_expr:
            'tier("standard", p * 5 + cr * 1.25 + img * 8 + img_cr * 0 + c * 30)',
        })}
        options={{ tokenUnit: 'M' }}
      />
    )
    expect(screen.getByText('Cache Read').parentElement).toHaveTextContent(
      '$1.25'
    )
    expect(screen.getByText('Image Cache').parentElement).toHaveTextContent(
      '$0'
    )
  })
  it('shows fixed prices per request in both token display units', () => {
    const model = pricingModel({
      billing_mode: 'tiered_expr',
      billing_expr: 'tier("request", fixed(0.01))',
    })
    const { rerender } = render(
      <ModelCard model={model} onClick={vi.fn()} tokenUnit='K' />
    )
    expect(screen.getByText('$0.01')).toBeVisible()
    expect(screen.getByText('/ request')).toBeVisible()
    rerender(<ModelCard model={model} onClick={vi.fn()} tokenUnit='M' />)
    expect(screen.getByText('$0.01')).toBeVisible()
    expect(screen.queryByText('/ 1M')).not.toBeInTheDocument()
  })
  it('updates the current time tier at a minute boundary and after returning to the page', () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-09-07T08:59:59+08:00'))
    const model = pricingModel({
      billing_mode: 'tiered_expr',
      billing_expr:
        'hour("Asia/Shanghai") >= 9 && hour("Asia/Shanghai") < 12 ? tier("peak", p * 3 + c * 9) : tier("off_peak", p * 1.5 + c * 4.5)',
    })
    render(<ModelCard model={model} onClick={vi.fn()} tokenUnit='M' />)
    expect(screen.getByText('Current period price')).toBeVisible()
    expect(screen.getByText('$1.5')).toBeVisible()
    act(() => vi.advanceTimersByTime(1000))
    expect(screen.getByText('$3')).toBeVisible()
    act(() => {
      vi.setSystemTime(new Date('2026-09-07T12:00:00+08:00'))
      fireEvent(document, new Event('visibilitychange'))
    })
    expect(screen.getByText('$1.5')).toBeVisible()
  })

  it('copies the complete long model name without opening details', async () => {
    const user = userEvent.setup()
    const onClick = vi.fn()
    const name = 'provider/model-with-a-long-name-and-a-version-suffix-20260906'
    render(
      <ModelCard model={pricingModel({ model_name: name })} onClick={onClick} />
    )
    expect(screen.getByRole('heading', { name })).toHaveAttribute('title', name)
    await user.click(screen.getByRole('button', { name: 'Copy model name' }))
    expect(await navigator.clipboard.readText()).toBe(name)
    expect(onClick).not.toHaveBeenCalled()
    await user.click(screen.getByRole('button', { name: 'Details' }))
    expect(onClick).toHaveBeenCalledOnce()
    expect(onClick).toHaveBeenCalledWith(name)
  })

  it('shows missing values without placeholder bars when metrics are unavailable', () => {
    render(<ModelCard model={pricingModel()} onClick={vi.fn()} />)
    const metrics = screen.getByLabelText(
      'Performance metrics for the last 24 hours'
    )
    expect(within(metrics).getAllByText('—')).toHaveLength(2)
    expect(within(metrics).getByText('—s')).toBeVisible()
    expect(within(metrics).getByText('—t/s')).toBeVisible()
    expect(within(metrics).queryByText(/100/)).not.toBeInTheDocument()
    const statusStrip = within(metrics).getByRole('img', {
      name: 'No performance data available',
    })
    expect(statusStrip).toHaveTextContent('—')
    expect(statusStrip.querySelectorAll('[aria-hidden]')).toHaveLength(0)
    expect(screen.getByText('No description available.')).toBeVisible()
    expect(screen.getByRole('button', { name: 'Details' })).toBeEnabled()
  })

  it('uses fixed spacing between available status samples', () => {
    render(
      <ModelCard
        model={pricingModel()}
        onClick={vi.fn()}
        perf={{
          avg_latency_ms: 1200,
          avg_tps: 42,
          success_rate: 50,
          recent_success_series: [
            { ts: 3600, success_rate: 100 },
            { ts: 18000, success_rate: 0 },
          ],
        }}
      />
    )
    const statusStrip = screen.getByRole('img', {
      name: 'Success rate',
    })
    expect(statusStrip).toHaveClass('gap-px')
    expect(statusStrip).not.toHaveClass('justify-between')
  })

  it('shows server history for idle models while preserving the current window metrics', async () => {
    const request = vi.spyOn(api, 'get').mockResolvedValue({
      data: {
        success: true,
        data: {
          models: [
            {
              model_name: 'active-model',
              avg_latency_ms: 1200,
              avg_tps: 42,
              success_rate: 99.8,
              recent_success_series: [{ ts: 7200, success_rate: 0 }],
            },
          ],
          recent_success_series: {
            'active-model': [{ ts: 3600, success_rate: 100 }],
            'idle-model': [
              { ts: 3600, success_rate: 0 },
              { ts: 7200, success_rate: 100 },
            ],
          },
        },
      },
    })
    render(
      <QueryClientProvider client={queryClient}>
        <ModelCardGrid
          models={[
            pricingModel({ model_name: 'active-model' }),
            pricingModel({ id: 2, model_name: 'idle-model' }),
          ]}
          onModelClick={vi.fn()}
        />
      </QueryClientProvider>
    )
    await screen.findByText('99.80%')
    expect(request).toHaveBeenCalledWith('/api/perf-metrics/summary', {
      params: { hours: 24, recent_model: ['active-model', 'idle-model'] },
      paramsSerializer: { indexes: null },
    })
    const [active, idle] = screen.getAllByLabelText(
      'Performance metrics for the last 24 hours'
    )
    expect(within(active).getByText('1.20s')).toBeVisible()
    expect(
      within(active).getByRole('img', { name: 'Success rate' }).children[0]
    ).toHaveClass('bg-emerald-500')
    const samples = within(idle).getByRole('img', {
      name: 'Success rate',
    }).children
    expect(samples).toHaveLength(2)
    expect(samples[0]).toHaveClass('bg-red-500')
    expect(samples[1]).toHaveClass('bg-emerald-500')
    expect(within(idle).getByText('—')).toBeVisible()
    expect(within(idle).getByText('—s')).toBeVisible()
    expect(within(idle).getByText('—t/s')).toBeVisible()
  })

  it('keeps group, endpoint and tag overflow counts with their own metadata', () => {
    const groups = ['default-with-a-long-group-name', 'premium', 'internal']
    const endpoints = ['openai-response', 'openai', 'claude', 'gemini', 'jina']
    const tags = [
      'video-generation',
      'high-resolution',
      'fast',
      'batch',
      'hd',
      'pro',
    ]
    render(
      <ModelCard
        model={pricingModel({
          enable_groups: groups,
          supported_endpoint_types: endpoints,
          tags: tags.join(','),
        })}
        onClick={vi.fn()}
      />
    )

    const groupField = screen.getByText('Groups').parentElement
    const endpointField = screen.getByText('Endpoints').parentElement
    if (!groupField || !endpointField) {
      throw new Error('Expected labeled group and endpoint fields')
    }
    const tagField = screen.getByRole('group', { name: 'Tags' })
    expect(within(groupField).getByText(groups[0])).toBeVisible()
    expect(within(groupField).getByText('+2')).toHaveAttribute(
      'title',
      groups.slice(1).join(', ')
    )
    expect(
      within(endpointField).getByText('openai-response, openai')
    ).toHaveAttribute('title', endpoints.join(', '))
    expect(within(endpointField).getByText('+3')).toBeVisible()
    expect(
      within(tagField).getByText('video-generation, high-resolution')
    ).toHaveAttribute('title', tags.join(', '))
    expect(within(tagField).getByText('+4')).toBeVisible()
    expect(
      within(screen.getByRole('group', { name: 'Pricing' })).getByText(
        'Token-based'
      )
    ).toBeVisible()
  })

  it('omits metadata fields when the model has no groups, endpoints or tags', () => {
    render(
      <ModelCard
        model={pricingModel({ enable_groups: [] })}
        onClick={vi.fn()}
      />
    )
    expect(screen.queryByText('Groups')).not.toBeInTheDocument()
    expect(screen.queryByText('Endpoints')).not.toBeInTheDocument()
    expect(
      screen.queryByRole('group', { name: 'Tags' })
    ).not.toBeInTheDocument()
  })

  it.each([
    { success_rate: 0, expected: '0.00%' },
    { success_rate: 99.8, expected: '99.80%' },
    { success_rate: Number.NaN, expected: '—' },
  ])(
    'shows $expected for the reported request success rate $success_rate',
    ({ success_rate, expected }) => {
      render(
        <ModelCard
          model={pricingModel()}
          onClick={vi.fn()}
          perf={{ avg_latency_ms: 1200, avg_tps: 42, success_rate }}
        />
      )
      const metrics = screen.getByLabelText(
        'Performance metrics for the last 24 hours'
      )
      expect(within(metrics).getAllByText(expected)[0]).toBeVisible()
      expect(within(metrics).getByText('Status')).toBeVisible()
      expect(within(metrics).getByText('1.20s')).toBeVisible()
      expect(within(metrics).getByText('42.0t/s')).toBeVisible()
    }
  )

  it('keeps group and recharge pricing correct when changing the token unit, including a free cache price', () => {
    const props = {
      model: pricingModel({ cache_ratio: 0 }),
      onClick: vi.fn(),
      selectedGroup: 'premium',
      showRechargePrice: true,
      priceRate: 3,
      usdExchangeRate: 6,
    }
    const { rerender } = render(<ModelCard {...props} tokenUnit='M' />)
    expect(screen.getByText('Input').parentElement).toHaveTextContent(
      /\$3\s*\/\s*1M/
    )
    expect(screen.getByText('Output').parentElement).toHaveTextContent(
      /\$9\s*\/\s*1M/
    )
    expect(screen.getByText('Cached').parentElement).toHaveTextContent(
      /\$0\s*\/\s*1M/
    )
    rerender(<ModelCard {...props} tokenUnit='K' />)
    expect(screen.getByText('Input').parentElement).toHaveTextContent(
      /\$0.003\s*\/\s*1K/
    )
    expect(screen.getByText('Output').parentElement).toHaveTextContent(
      /\$0.009\s*\/\s*1K/
    )
    expect(screen.getByText('Cached').parentElement).toHaveTextContent(
      /\$0\s*\/\s*1K/
    )
  })

  it('shows a per-request price with the selected group and recharge multiplier without a token unit', () => {
    render(
      <ModelCard
        model={pricingModel({ quota_type: 1, model_price: 0.4 })}
        onClick={vi.fn()}
        selectedGroup='premium'
        showRechargePrice
        priceRate={3}
        usdExchangeRate={6}
        tokenUnit='K'
      />
    )
    expect(screen.getByText(/\$0.6/)).toHaveTextContent(/\$0.6\s*\/\s*request/)
    expect(screen.queryByText(/1K|1M/)).not.toBeInTheDocument()
    expect(screen.getAllByText('Per Request')).toHaveLength(1)
    expect(screen.queryByText('Per-request')).not.toBeInTheDocument()
  })

  it('preserves expression prices and makes the selected token unit explicit', () => {
    render(
      <ModelCard
        model={pricingModel({
          billing_mode: 'tiered_expr',
          billing_expr: 'tier("base", p * 3 + c * 15)',
        })}
        onClick={vi.fn()}
        tokenUnit='K'
        selectedGroup='premium'
        showRechargePrice
        priceRate={3}
        usdExchangeRate={6}
      />
    )
    expect(screen.getByText('Input').parentElement).toHaveTextContent(
      /\$0.0045\s*\/\s*1K/
    )
    expect(screen.getByText('Output').parentElement).toHaveTextContent(
      /\$0.0225\s*\/\s*1K/
    )
  })

  it('shows the cache read price of an expression model after its input and output prices', () => {
    render(
      <ModelCard
        model={pricingModel({
          billing_mode: 'tiered_expr',
          billing_expr: 'tier("base", p * 0.15 + c * 0.6 + cr * 0.003)',
        })}
        onClick={vi.fn()}
        tokenUnit='M'
        selectedGroup='default'
      />
    )
    expect(screen.getByText('Input').parentElement).toHaveTextContent(
      /\$0.15\s*\/\s*1M/
    )
    expect(screen.getByText('Output').parentElement).toHaveTextContent(
      /\$0.6\s*\/\s*1M/
    )
    expect(screen.getByText('Cache Read').parentElement).toHaveTextContent(
      /\$0.003\s*\/\s*1M/
    )
  })

  it('shows the cache read price once when a plugin provider already lists it as a main price', () => {
    render(
      <ModelCard
        model={pricingModel({
          billing_plugin_variants: [
            {
              plugin_key: 'alpha',
              plugin_name: 'Alpha',
              billing_mode: 'tiered_expr',
              billing_expr: 'tier("base", p * 1 + c * 2 + cr * 0.5)',
              billing_usage_schema: {},
            },
          ],
        })}
        onClick={vi.fn()}
        tokenUnit='M'
        selectedGroup='default'
      />
    )
    expect(screen.getAllByText('Cache Read')).toHaveLength(1)
  })

  it('shows the cache read price of an expression model that prices nothing else', () => {
    render(
      <ModelCard
        model={pricingModel({
          billing_mode: 'tiered_expr',
          billing_expr: 'tier("base", cr * 0.5)',
        })}
        onClick={vi.fn()}
        tokenUnit='M'
        selectedGroup='default'
      />
    )
    expect(screen.getByText('Cache Read').parentElement).toHaveTextContent(
      /\$0.5\s*\/\s*1M/
    )
  })

  it('keeps task price ranges in their actual usage unit instead of the selected token unit', () => {
    render(
      <ModelCard
        model={pricingModel({
          billing_mode: 'tiered_expr',
          billing_expr:
            'u("mode") == "pro" ? tier("pro", u("seconds") * 0.8) : tier("std", u("seconds") * 0.4)',
          billing_usage_schema: {
            seconds: { type: 'number', unit: 'second' },
            mode: { enum: ['std', 'pro'] },
          },
        })}
        onClick={vi.fn()}
        tokenUnit='K'
      />
    )
    expect(screen.getByText(/0.4.*0.8/)).toHaveTextContent(/0.4 – \$0.8/)
    expect(screen.getByText(/^\/\s*s$/)).toBeVisible()
    expect(screen.queryByText(/1K|1M/)).not.toBeInTheDocument()
  })

  it('hides a task usage price left at zero while another usage price is set', () => {
    render(
      <ModelCard
        model={pricingModel({
          billing_mode: 'tiered_expr',
          billing_expr: 'tier("base", u("count") * 0 + u("seconds") * 0.7)',
          billing_usage_schema: {
            count: {
              type: 'number',
              unit: 'count',
              description: { en: 'Per-task price' },
            },
            seconds: {
              type: 'number',
              unit: 'second',
              description: { en: 'Video generation unit price' },
            },
          },
        })}
        onClick={vi.fn()}
      />
    )
    expect(screen.getByText('Video generation unit price')).toBeVisible()
    expect(screen.queryByText('Per-task price')).not.toBeInTheDocument()
  })

  it('does not show the tier name of a single-tier task price', () => {
    render(
      <ModelCard
        model={pricingModel({
          billing_mode: 'tiered_expr',
          billing_expr: 'tier("base", u("seconds") * 0.4)',
          billing_usage_schema: { seconds: { type: 'number', unit: 'second' } },
        })}
        onClick={vi.fn()}
      />
    )
    expect(screen.getByText(/^\$0\.4$/)).toBeVisible()
    expect(screen.queryByText('(base)')).not.toBeInTheDocument()
  })

  it('shows the unconfigured usage message without inventing a token price', () => {
    render(
      <ModelCard
        model={pricingModel({
          billing_usage_schema: { seconds: { type: 'number', unit: 'second' } },
        })}
        onClick={vi.fn()}
      />
    )
    expect(
      screen.getByText('Usage-based billing · price not configured')
    ).toBeVisible()
    expect(screen.queryByText('Input')).not.toBeInTheDocument()
  })

  it('shows a spaced task token range with its unit when an example price is present', () => {
    render(
      <ModelCard
        model={pricingModel({
          billing_mode: 'tiered_expr',
          billing_expr:
            'u("mode") == "pro" ? tier("pro", u("tokens") * 70 / 1000000) : tier("std", u("tokens") * 42 / 1000000)',
          billing_usage_schema: {
            tokens: { type: 'number', unit: 'token' },
            mode: { enum: ['std', 'pro'] },
          },
          billing_usage_examples: [
            { label: '480p · 5s', facts: { tokens: 48000, mode: 'std' } },
          ],
        })}
        onClick={vi.fn()}
        tokenUnit='K'
      />
    )
    expect(screen.getByText('$42 – $70').parentElement).toHaveTextContent(
      '$42 – $70 / 1M token'
    )
    expect(screen.getByText(/480p · 5s ≈/)).toBeVisible()
  })

  it('keeps an unrecognized expression visible with the special billing message', () => {
    const expression =
      'u("seconds") > 30 ? tier("long", u("seconds") * 0.3) : tier("short", u("seconds") * 0.4)'
    render(
      <ModelCard
        model={pricingModel({
          billing_mode: 'tiered_expr',
          billing_expr: expression,
          billing_usage_schema: { seconds: { type: 'number', unit: 'second' } },
        })}
        onClick={vi.fn()}
      />
    )
    expect(screen.getByText('Special billing expression')).toBeVisible()
    expect(screen.getByText(expression)).toBeVisible()
  })

  it('keeps browsing and missing metric values available after the metrics request fails', async () => {
    const request = vi
      .spyOn(api, 'get')
      .mockRejectedValue(new Error('metrics unavailable'))
    const onModelClick = vi.fn()
    render(
      <QueryClientProvider client={queryClient}>
        <ModelCardGrid models={[pricingModel()]} onModelClick={onModelClick} />
      </QueryClientProvider>
    )
    await waitFor(() =>
      expect(
        queryClient.getQueryState([
          'perf-metrics-summary',
          24,
          ['example-model'],
        ])?.status
      ).toBe('error')
    )
    expect(request).toHaveBeenCalledWith('/api/perf-metrics/summary', {
      params: { hours: 24, recent_model: ['example-model'] },
      paramsSerializer: { indexes: null },
    })
    expect(
      within(
        screen.getByLabelText('Performance metrics for the last 24 hours')
      ).getAllByText(/^—/)
    ).toHaveLength(4)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Details' }))
    expect(onModelClick).toHaveBeenCalledWith('example-model')
  })

  it('paginates the model cards and disables navigation at both boundaries', async () => {
    const request = vi.spyOn(api, 'get').mockResolvedValue({
      data: { success: true, data: { models: [] } },
    })
    const models = Array.from({ length: 21 }, (_, index) =>
      pricingModel({ id: index + 1, model_name: `model-${index + 1}` })
    )
    const user = userEvent.setup()
    render(
      <QueryClientProvider client={queryClient}>
        <ModelCardGrid models={models} onModelClick={vi.fn()} />
      </QueryClientProvider>
    )
    expect(screen.getByRole('button', { name: 'Previous page' })).toBeDisabled()
    expect(screen.getAllByRole('heading', { level: 3 })).toHaveLength(20)
    expect(request).toHaveBeenCalledWith('/api/perf-metrics/summary', {
      params: {
        hours: 24,
        recent_model: models.slice(0, 20).map((model) => model.model_name),
      },
      paramsSerializer: { indexes: null },
    })
    await user.click(screen.getByRole('button', { name: 'Next page' }))
    expect(screen.getAllByRole('heading', { level: 3 })).toHaveLength(1)
    expect(screen.getByRole('heading', { name: 'model-21' })).toBeVisible()
    expect(request).toHaveBeenLastCalledWith('/api/perf-metrics/summary', {
      params: { hours: 24, recent_model: ['model-21'] },
      paramsSerializer: { indexes: null },
    })
    expect(screen.getByRole('button', { name: 'Next page' })).toBeDisabled()
    await user.click(screen.getByRole('button', { name: 'Previous page' }))
    expect(screen.getByRole('heading', { name: 'model-1' })).toBeVisible()
  })

  it('switches the card grid to three columns at the xl breakpoint instead of 2xl', () => {
    queryClient.setQueryData(['perf-metrics-summary', 24, ['example-model']], {
      success: true,
      data: { models: [] },
    })
    render(
      <QueryClientProvider client={queryClient}>
        <ModelCardGrid models={[pricingModel()]} onModelClick={vi.fn()} />
      </QueryClientProvider>
    )
    const grid = screen
      .getByRole('heading', { name: 'example-model' })
      .closest('.grid')
    expect(grid).toHaveClass('xl:grid-cols-3')
    expect(grid).not.toHaveClass('2xl:grid-cols-3')
    expect(grid).not.toHaveClass('min-[1440px]:grid-cols-3')
  })

  it('shows only sparse valid samples in chronological order without placeholder bars', () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-09-07T12:00:00.000Z'))
    const currentHourStart = Math.floor(Date.now() / 1000 / 3600) * 3600

    render(
      <ModelCard
        model={pricingModel()}
        onClick={vi.fn()}
        perf={{
          avg_latency_ms: 1200,
          avg_tps: 42,
          success_rate: 100,
          window_start: currentHourStart - 23 * 3600,
          recent_success_series: [
            { ts: currentHourStart, success_rate: 100 },
            { ts: currentHourStart - 5 * 3600, success_rate: 80 },
            { ts: currentHourStart - 3 * 3600, success_rate: 0 },
          ],
        }}
      />
    )

    const spans = [
      ...screen.getByRole('img', {
        name: 'Success rate',
      }).children,
    ]
    expect(spans).toHaveLength(3)
    expect(spans[0]).toHaveClass('bg-amber-500')
    expect(spans[1]).toHaveClass('bg-red-500')
    expect(spans[2]).toHaveClass('bg-emerald-500')
    vi.useRealTimers()
  })

  it('skips invalid samples without inserting gaps or dropping a zero success rate', () => {
    const windowStart = Date.parse('2026-09-07T00:00:00.000Z') / 1000
    render(
      <ModelCard
        model={pricingModel()}
        onClick={vi.fn()}
        perf={{
          avg_latency_ms: 1200,
          avg_tps: 42,
          success_rate: 50,
          window_start: windowStart,
          recent_success_series: [
            { ts: windowStart, success_rate: 100 },
            { ts: windowStart + 3600, success_rate: Number.NaN },
            { ts: windowStart + 2 * 3600, success_rate: -1 },
            { ts: windowStart + 3 * 3600, success_rate: 101 },
            {
              ts: windowStart + 4 * 3600,
              success_rate: Number.POSITIVE_INFINITY,
            },
            { ts: windowStart + 5 * 3600, success_rate: 0 },
            { ts: Number.NaN, success_rate: 100 },
          ],
        }}
      />
    )

    const spans = [
      ...screen.getByRole('img', {
        name: 'Success rate',
      }).children,
    ]
    expect(spans).toHaveLength(2)
    expect(spans[0]).toHaveClass('bg-emerald-500')
    expect(spans[1]).toHaveClass('bg-red-500')
  })

  it('shows the latest 24 valid samples without requiring hourly window metadata or mutating the series', () => {
    const recentSeries = [
      { ts: 0, success_rate: 0 },
      ...Array.from({ length: 24 }, (_, index) => ({
        ts: (index + 1) * 7200,
        success_rate: 100,
      })),
      { ts: 25 * 7200, success_rate: Number.NaN },
    ].reverse()
    const originalSeries = [...recentSeries]
    render(
      <ModelCard
        model={pricingModel()}
        onClick={vi.fn()}
        perf={{
          avg_latency_ms: 1200,
          avg_tps: 42,
          success_rate: 100,
          recent_success_series: recentSeries,
        }}
      />
    )

    const spans = [
      ...screen.getByRole('img', {
        name: 'Success rate',
      }).children,
    ]
    expect(spans).toHaveLength(24)
    spans.forEach((sample) => {
      expect(sample).toHaveClass('bg-emerald-500')
    })
    expect(recentSeries).toEqual(originalSeries)
  })

  it.each([undefined, [], [{ ts: 3600, success_rate: Number.NaN }]])(
    'shows an empty state without bars when the series has no valid samples: %j',
    (series) => {
      render(
        <ModelCard
          model={pricingModel()}
          onClick={vi.fn()}
          perf={{
            avg_latency_ms: 1200,
            avg_tps: 42,
            success_rate: 100,
            recent_success_series: series,
          }}
        />
      )

      const statusStrip = screen.getByRole('img', {
        name: 'No performance data available',
      })
      expect(statusStrip).toHaveTextContent('—')
      expect(statusStrip.querySelectorAll('[aria-hidden]')).toHaveLength(0)
    }
  )

  it('shows a single server sample without padding when the browser clock is a day ahead', () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-09-07T12:37:00.000Z'))
    const currentHourStart = Math.floor(Date.now() / 1000 / 3600) * 3600

    render(
      <ModelCard
        model={pricingModel()}
        onClick={vi.fn()}
        perf={{
          avg_latency_ms: 1200,
          avg_tps: 42,
          success_rate: 80,
          window_start: currentHourStart - 47 * 3600,
          recent_success_series: [
            { ts: currentHourStart - 29 * 3600, success_rate: 80 },
          ],
        }}
      />
    )

    const spans = [
      ...screen.getByRole('img', {
        name: 'Success rate',
      }).children,
    ]
    expect(spans).toHaveLength(1)
    expect(spans[0]).toHaveClass('bg-amber-500')
    vi.useRealTimers()
  })
})

function catalogOffering(
  overrides: Partial<CatalogOffering> = {}
): CatalogOffering {
  return {
    id: 1,
    name: 'sale-offering',
    aliases: ['sale-alias'],
    members: [
      {
        model_id: 7,
        name: 'shared-member',
        tier: 1,
        description: 'Member description',
        icon: '',
        tags: 'vision',
        publisher_id: 3,
        publisher_name: 'Publisher',
        capabilities: {},
      },
    ],
    price: {
      id: 1,
      currency: 'EUR',
      expression: 'tier("sale", p * 0.100000000000000001 + cr * 0 + c * 2)',
      effective_at: 1,
    },
    ...overrides,
  }
}

function renderCatalog(props: Parameters<typeof OfferingCatalog>[0] = {}) {
  return render(
    <QueryClientProvider client={queryClient}>
      <OfferingCatalog {...props} />
    </QueryClientProvider>
  )
}

describe('Offering sale catalog', () => {
  beforeEach(() => useAuthStore.getState().auth.reset())
  afterEach(() => useAuthStore.getState().auth.reset())
  const signIn = (id = 1) =>
    useAuthStore.getState().auth.setUser({
      id,
      username: `account-${id}`,
      role: 1,
      group: 'default',
    })
  const response = (data: CatalogOffering[]) => ({
    data: { success: true, data, as_of: 1 },
  })

  it('shows a sign-in prompt without fetching an anonymous catalog', () => {
    const get = vi.spyOn(api, 'get')
    renderCatalog()
    expect(screen.getByText('Sign in to view Offering prices')).toBeVisible()
    expect(screen.getByRole('link', { name: 'Sign in' })).toHaveAttribute(
      'href',
      expect.stringContaining('/sign-in?redirect=')
    )
    expect(get).not.toHaveBeenCalled()
  })

  it('shrinks and wraps a maximum-length Offering name without pushing away its copy action', async () => {
    signIn()
    const name = `sale_${'x'.repeat(123)}`
    vi.spyOn(api, 'get').mockResolvedValue(
      response([catalogOffering({ name })])
    )
    const user = userEvent.setup()
    const writeText = vi
      .spyOn(navigator.clipboard, 'writeText')
      .mockResolvedValue()
    renderCatalog()
    const card = within(await screen.findByRole('article'))
    const nameButton = card.getByRole('button', { name })
    expect(nameButton).toHaveClass(
      'min-w-0',
      'flex-1',
      'shrink',
      'break-all',
      'whitespace-normal'
    )
    expect(nameButton).not.toHaveClass('shrink-0')
    const copy = card.getByRole('button', { name: 'Copy Offering name' })
    expect(copy).toBeEnabled()
    await user.click(copy)
    expect(writeText).toHaveBeenCalledWith(name)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    await user.click(nameButton)
    expect(await screen.findByRole('dialog')).toHaveTextContent(name)
  })

  it('shows translated filter labels before opening a popup and updates selected labels on language changes', async () => {
    signIn()
    vi.spyOn(api, 'get').mockResolvedValue(response([catalogOffering()]))
    const i18n = createInstance()
    await i18n.init({
      lng: 'en',
      fallbackLng: false,
      resources: { en: { translation: {} }, fr },
    })
    const view = render(
      <I18nextProvider i18n={i18n}>
        <QueryClientProvider client={queryClient}>
          <OfferingCatalog />
        </QueryClientProvider>
      </I18nextProvider>
    )
    await screen.findByRole('article')
    expect(
      screen.getByRole('combobox', { name: 'Publisher' })
    ).toHaveTextContent('All publishers')
    expect(screen.getByRole('combobox', { name: 'Tags' })).toHaveTextContent(
      'All tags'
    )
    expect(screen.getByRole('combobox', { name: 'Sort' })).toHaveTextContent(
      'Name ascending'
    )
    await act(() => i18n.changeLanguage('fr'))
    expect(
      screen.getByRole('combobox', { name: i18n.t('Publisher') })
    ).toHaveTextContent(i18n.t('All publishers'))
    expect(
      screen.getByRole('combobox', { name: i18n.t('Tags') })
    ).toHaveTextContent(i18n.t('All tags'))
    expect(
      screen.getByRole('combobox', { name: i18n.t('Sort') })
    ).toHaveTextContent(i18n.t('Name ascending'))
    view.rerender(
      <I18nextProvider i18n={i18n}>
        <QueryClientProvider client={queryClient}>
          <OfferingCatalog
            search={{
              search: '',
              vendor: '3',
              tag: 'vision',
              sort: 'name-desc',
              tokenUnit: 'M',
              view: 'card',
            }}
          />
        </QueryClientProvider>
      </I18nextProvider>
    )
    expect(
      screen.getByRole('combobox', { name: i18n.t('Publisher') })
    ).toHaveTextContent('Publisher')
    expect(
      screen.getByRole('combobox', { name: i18n.t('Tags') })
    ).toHaveTextContent('vision')
    expect(
      screen.getByRole('combobox', { name: i18n.t('Sort') })
    ).toHaveTextContent(i18n.t('Name descending'))
  })

  it.each(['Publisher', 'Tags'] as const)(
    'keeps a long %s filter value readable when selecting and clearing it',
    async (label) => {
      signIn()
      const name = `${label}_${'x'.repeat(128 - label.length - 1)}`
      const offering = catalogOffering()
      vi.spyOn(api, 'get').mockResolvedValue(
        response([
          {
            ...offering,
            members: offering.members.map((member) => ({
              ...member,
              publisher_name: name,
              tags: name,
            })),
          },
        ])
      )
      const user = userEvent.setup()
      renderCatalog()
      await screen.findByRole('article')
      const filter = screen.getByRole('combobox', { name: label })
      await user.click(filter)
      const option = await screen.findByRole('option', { name })
      expect(within(option).getByText(name)).toHaveClass(
        'wrap-anywhere',
        'whitespace-normal'
      )
      await user.click(option)
      expect(filter).toHaveTextContent(name)
      expect(filter).toHaveClass('min-w-0', 'max-w-full', 'h-auto')
      expect(within(filter).getByText(name)).toHaveClass(
        'wrap-anywhere',
        'whitespace-normal'
      )
      await user.click(filter)
      await user.click(
        screen.getByRole('option', {
          name: label === 'Publisher' ? 'All publishers' : 'All tags',
        })
      )
      expect(filter).toHaveTextContent(
        label === 'Publisher' ? 'All publishers' : 'All tags'
      )
      expect(screen.getByRole('article')).toBeVisible()
    }
  )

  it('wraps a maximum-length member name in the expandable capability summary', async () => {
    signIn()
    const name = `model_${'x'.repeat(122)}`
    const offering = catalogOffering()
    vi.spyOn(api, 'get').mockResolvedValue(
      response([
        {
          ...offering,
          members: offering.members.map((member) => ({
            ...member,
            name,
            capabilities: { vision: true },
          })),
        },
      ])
    )
    const user = userEvent.setup()
    renderCatalog({ detailName: offering.name })
    const summary = await screen.findByText(`${name} · Capabilities`)
    expect(summary.tagName).toBe('SUMMARY')
    expect(summary).toHaveClass('wrap-anywhere')
    await user.click(summary)
    expect(summary.closest('details')).toHaveAttribute('open')
    expect(screen.getByText(/"vision": true/)).toBeVisible()
  })

  it('identifies the current version as a base price, not an account charge, in cards, tables and details', async () => {
    signIn()
    const offering = catalogOffering()
    vi.spyOn(api, 'get').mockResolvedValue(response([offering]))
    const view = renderCatalog()
    const explanation =
      'Base prices come from the current price version. Your group discount, request usage and pricing conditions determine the final charge.'
    const card = within(await screen.findByRole('article'))
    expect(card.getByText('Base sale price')).toBeVisible()
    expect(card.getByText(explanation)).toBeVisible()
    expect(card.getByLabelText('Complete price expression')).toHaveTextContent(
      offering.price?.expression ?? ''
    )
    await userEvent.click(screen.getByRole('button', { name: 'Table view' }))
    const table = within(screen.getByRole('table'))
    expect(
      table.getByRole('columnheader', { name: 'Base sale price' })
    ).toBeVisible()
    expect(table.getByText(explanation)).toBeVisible()
    await userEvent.click(table.getByRole('button', { name: offering.name }))
    const dialog = within(await screen.findByRole('dialog'))
    expect(dialog.getByText('Base sale price')).toBeVisible()
    expect(dialog.getByText(explanation)).toBeVisible()
    view.rerender(
      <QueryClientProvider client={queryClient}>
        <OfferingCatalog detailName={offering.name} />
      </QueryClientProvider>
    )
    expect(screen.getByRole('heading', { name: offering.name })).toBeVisible()
    expect(screen.getByText('Base sale price')).toBeVisible()
    expect(screen.getByText(explanation)).toBeVisible()
    expect(
      screen.getByLabelText('Complete price expression')
    ).toHaveTextContent(offering.price?.expression ?? '')
  })

  it('preserves native 18-decimal coefficients, zero and the full expression when switching M/K', async () => {
    signIn()
    const offering = catalogOffering()
    vi.spyOn(api, 'get').mockResolvedValue(
      response([
        offering,
        catalogOffering({ id: 2, name: 'no-current-price', price: null }),
      ])
    )
    renderCatalog()
    expect(await screen.findByText(/p: 0.100000000000000001 EUR/)).toBeVisible()
    expect(screen.getByText(/cr: 0 EUR/)).toBeVisible()
    expect(
      screen.getByLabelText('Complete price expression')
    ).toHaveTextContent(offering.price?.expression ?? '')
    expect(screen.getByText('No current sale price')).toBeVisible()
    await userEvent.click(screen.getByRole('button', { name: '1M tokens' }))
    expect(screen.getByText(/p: 0.000100000000000000001 EUR/)).toBeVisible()
    expect(screen.getByText(/cr: 0 EUR/)).toBeVisible()
    expect(
      screen.getByLabelText('Complete price expression')
    ).toHaveTextContent(offering.price?.expression ?? '')
    expect(screen.queryByText(/USD/)).not.toBeInTheDocument()
  })

  it('keeps fixed and task prices in native units and exposes the entire conditional expression', async () => {
    signIn()
    const fixed = catalogOffering({
      price: {
        id: 2,
        currency: 'JPY',
        expression: 'tier("fixed", fixed(0))',
        effective_at: 1,
      },
    })
    const expression =
      'u("mode") == "pro" ? tier("pro", u("seconds") * 0.123456789123456789) : tier("standard", u("seconds") * 0.1)'
    vi.spyOn(api, 'get').mockResolvedValue(
      response([
        fixed,
        catalogOffering({
          id: 2,
          name: 'task-sale',
          price: { id: 3, currency: 'EUR', expression, effective_at: 1 },
        }),
      ])
    )
    renderCatalog()
    expect(await screen.findByText('0 JPY / request')).toBeVisible()
    expect(
      screen.getAllByLabelText('Complete price expression')[1]
    ).toHaveTextContent(expression)
    await userEvent.click(screen.getByRole('button', { name: '1M tokens' }))
    expect(screen.getByText('0 JPY / request')).toBeVisible()
    expect(
      screen.getAllByLabelText('Complete price expression')[1]
    ).toHaveTextContent(expression)
  })

  it('searches aliases, opens member tiers, and links with the external Offering name', async () => {
    signIn()
    const onOpenPage = vi.fn()
    vi.spyOn(api, 'get').mockResolvedValue(
      response([
        catalogOffering(),
        catalogOffering({
          id: 2,
          name: 'second-sale',
          aliases: ['second-alias'],
        }),
      ])
    )
    renderCatalog({ onOpenPage })
    await screen.findByRole('button', { name: 'sale-offering' })
    await userEvent.type(screen.getByRole('textbox'), 'second-alias')
    expect(
      screen.queryByRole('button', { name: 'sale-offering' })
    ).not.toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'second-sale' }))
    const dialog = within(await screen.findByRole('dialog'))
    expect(dialog.getByText('shared-member')).toBeVisible()
    expect(dialog.getByText('Member description')).toBeVisible()
    expect(dialog.getByText('1')).toBeVisible()
    expect(dialog.getByRole('cell', { name: 'Publisher' })).toBeVisible()
    expect(dialog.getByText(/"model": "second-sale"/)).toBeVisible()
    await userEvent.click(
      dialog.getByRole('button', { name: 'Open details page' })
    )
    expect(onOpenPage).toHaveBeenCalledWith('second-sale')
  })

  it('preserves card/table views, pagination, and keyboard opening', async () => {
    signIn()
    vi.spyOn(api, 'get').mockResolvedValue(
      response(
        Array.from({ length: 13 }, (_, index) =>
          catalogOffering({
            id: index + 1,
            name: `sale-${String(index).padStart(2, '0')}`,
          })
        )
      )
    )
    renderCatalog()
    const first = await screen.findByRole('button', { name: 'sale-00' })
    first.focus()
    await userEvent.keyboard('{Enter}')
    expect(await screen.findByRole('dialog')).toBeVisible()
    await userEvent.keyboard('{Escape}')
    await waitFor(() =>
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    )
    await userEvent.click(screen.getByRole('button', { name: 'Next' }))
    expect(screen.getByRole('button', { name: 'sale-12' })).toBeVisible()
    expect(
      screen.queryByRole('button', { name: 'sale-00' })
    ).not.toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Table view' }))
    expect(screen.getByRole('table')).toHaveTextContent('sale-12')
    await userEvent.click(screen.getByRole('button', { name: 'Previous' }))
    expect(screen.getByRole('table')).toHaveTextContent('sale-00')
  })

  it('shows a retryable error for malformed catalog data instead of a zero or empty success', async () => {
    signIn()
    const get = vi
      .spyOn(api, 'get')
      .mockResolvedValueOnce({
        data: {
          success: true,
          data: [
            {
              ...catalogOffering(),
              price: {
                id: 1,
                currency: 'usd',
                expression: 'fixed(0)',
                effective_at: 1,
              },
            },
          ],
          as_of: 1,
        },
      })
      .mockResolvedValue(response([catalogOffering()]))
    renderCatalog()
    expect(
      await screen.findByText('Unable to load Offering prices')
    ).toBeVisible()
    expect(screen.queryByText('No visible Offerings')).not.toBeInTheDocument()
    await userEvent.click(
      screen.getByRole('button', { name: /Retry|Try again/ })
    )
    expect(
      await screen.findByRole('button', { name: 'sale-offering' })
    ).toBeVisible()
    expect(get).toHaveBeenCalledTimes(2)
  })

  it('clears the previous account immediately and ignores its late catalog response', async () => {
    signIn()
    let resolveFirst!: (value: ReturnType<typeof response>) => void
    const first = new Promise<ReturnType<typeof response>>((resolve) => {
      resolveFirst = resolve
    })
    const get = vi
      .spyOn(api, 'get')
      .mockReturnValueOnce(first)
      .mockResolvedValue(
        response([catalogOffering({ id: 2, name: 'account-two' })])
      )
    renderCatalog()
    await waitFor(() => expect(get).toHaveBeenCalledTimes(1))
    act(() => signIn(2))
    expect(
      await screen.findByRole('button', { name: 'account-two' })
    ).toBeVisible()
    await act(async () =>
      resolveFirst(response([catalogOffering({ name: 'account-one-private' })]))
    )
    expect(screen.queryByText('account-one-private')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'account-two' })).toBeVisible()
  })

  it('resolves a detail alias and preserves the unavailable state for a hidden Offering', async () => {
    signIn()
    vi.spyOn(api, 'get').mockResolvedValue(response([catalogOffering()]))
    const view = renderCatalog({ detailName: 'sale-alias' })
    expect(
      await screen.findByRole('heading', { name: 'sale-offering' })
    ).toBeVisible()
    view.rerender(
      <QueryClientProvider client={queryClient}>
        <OfferingCatalog detailName='hidden-sale' />
      </QueryClientProvider>
    )
    expect(screen.getByText('Offering unavailable')).toBeVisible()
    expect(screen.queryByText('shared-member')).not.toBeInTheDocument()
  })
})
