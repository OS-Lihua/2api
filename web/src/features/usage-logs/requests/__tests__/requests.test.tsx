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
import { QueryClientProvider, type QueryClient } from '@tanstack/react-query'
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { AxiosError, type AxiosAdapter } from 'axios'
import i18next from 'i18next'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import fr from '@/i18n/locales/fr.json'
import zhTW from '@/i18n/locales/zh-TW.json'
import { api } from '@/lib/api'
import { createAppQueryClient } from '@/lib/query-client'
import { useAuthStore } from '@/stores/auth-store'

import { RequestLogs } from '..'

const requestId =
  'req_0123456789abcdefghijklmnopqrstuvwxyz_0123456789abcdefghijklmnopqrstuvwxyz'
const request = {
  id: requestId,
  user_id: 7,
  token_id: 4,
  model_offering_id: 12,
  offering_name: 'chat-standard',
  requested_name: 'chat-alias',
  sale_price_version_id: 9,
  sale_currency: 'USD',
  sale_fx_rate: '1.000000001',
  usage: { prompt_tokens: 120, completion_tokens: 30, total_tokens: 150 },
  usage_source: 'estimated',
  group_ratio: '1',
  receivable: { nanos: '9007199254740993', currency: 'USD' },
  ledger_entry_ids: ['9007199254740995'],
  started_at: 1794000000123,
  ended_at: 1794000001456,
  status: 'success',
}
const attempt = {
  id: 'attempt-long-0123456789abcdefghijklmnopqrstuvwxyz',
  request_id: requestId,
  started_at: request.started_at,
  ended_at: request.ended_at,
  status: 'success',
  error_class: '',
  usage: request.usage,
  usage_source: 'estimated',
  conversion_losses: ['unsupported_parameter'],
}
const internalAttempt = {
  ...attempt,
  provider_id: 83,
  channel_id: 51,
  model_id: 62,
  member_tier: 0,
  purchase_price_version_id: 44,
  cost: { nanos: '1', currency: 'EUR' },
  converted_cost: { nanos: '2', currency: 'USD' },
  accounting_fx_rate: '1.23456789123456789',
  failover_decision: 'stop',
  failover_reason: 'admin-only-route',
  circuit_action: 'none',
  route_diagnostics: {
    method: 'single_channel',
    candidate_count: 3,
    eligible_count: 1,
    excluded_counts: { disabled: 2 },
    excluded: [{ channel_id: 61, reason: 'disabled' }],
    credentials: 'never-show',
    destination: 'https://internal.invalid',
    raw_prompt: 'never-show-prompt',
  },
}
const originalAdapter = api.defaults.adapter
let client: QueryClient
let urls: URL[]

beforeEach(() => {
  urls = []
  useAuthStore.getState().auth.setUser({ id: 7, username: 'reader', role: 1 })
  client = createAppQueryClient()
  client.setDefaultOptions({ queries: { retry: false } })
  const adapter: AxiosAdapter = async (config) => {
    const url = new URL(api.getUri(config), 'http://localhost')
    urls.push(url)
    const items = url.pathname.endsWith('/attempts') ? [attempt] : [request]
    return {
      config,
      status: 200,
      statusText: 'OK',
      headers: {},
      data: {
        success: true,
        data: { items, total: 1, page: 1, page_size: 20 },
      },
    }
  }
  api.defaults.adapter = adapter
})

afterEach(async () => {
  cleanup()
  await i18next.changeLanguage('en')
  i18next.removeResourceBundle('fr', 'translation')
  i18next.removeResourceBundle('zhTW', 'translation')
  client.clear()
  api.defaults.adapter = originalAdapter
  useAuthStore.getState().auth.reset()
})

function renderRequests() {
  return render(
    <QueryClientProvider client={client}>
      <RequestLogs />
    </QueryClientProvider>
  )
}

it('shows a precise charge and estimated usage, opens the real self attempt, and closes with Escape', async () => {
  const user = userEvent.setup()
  renderRequests()
  expect(await screen.findByText('chat-standard')).toBeVisible()
  expect(screen.getByText('chat-alias')).toBeVisible()
  expect(screen.getByText('9,007,199.254740993 USD')).toBeVisible()
  expect(screen.getByText('Estimated')).toBeVisible()
  expect(screen.getByText(requestId)).toHaveAttribute('title', requestId)
  await user.click(screen.getByRole('button', { name: 'Copy request ID' }))
  expect(await navigator.clipboard.readText()).toBe(requestId)
  const open = screen.getByRole('button', {
    name: `View attempts for request ${requestId}`,
  })
  await user.click(open)
  const drawer = await screen.findByRole('dialog', { name: 'Request attempts' })
  expect(await within(drawer).findByText(attempt.id)).toBeVisible()
  expect(within(drawer).getByText('9007199254740995')).toBeVisible()
  expect(within(drawer).getByText('1.000000001')).toBeVisible()
  expect(within(drawer).getByText('unsupported_parameter')).toBeVisible()
  expect(within(drawer).getByText('Estimated')).toBeVisible()
  expect(urls.map((url) => url.pathname)).toEqual([
    '/api/requests/self',
    `/api/requests/self/${requestId}/attempts`,
  ])
  expect(urls[0].searchParams.get('page')).toBe('1')
  expect(urls[0].searchParams.get('page_size')).toBe('20')
  await user.keyboard('{Escape}')
  expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  await waitFor(() => expect(open).toHaveFocus())
})

it('uses request:read permission for all requests and strips internal fields in the self view even if the DTO contains them', async () => {
  useAuthStore.getState().auth.setUser({
    id: 7,
    username: 'admin',
    role: 10,
    permissions: { admin_permissions: { request: { read: true } } },
  })
  api.defaults.adapter = async (config) => {
    const url = new URL(api.getUri(config), 'http://localhost')
    urls.push(url)
    return {
      config,
      status: 200,
      statusText: 'OK',
      headers: {},
      data: {
        success: true,
        data: {
          items: url.pathname.endsWith('/attempts')
            ? [internalAttempt]
            : [request],
          total: 1,
          page: 1,
          page_size: 20,
        },
      },
    }
  }
  const user = userEvent.setup()
  renderRequests()
  await user.click(
    await screen.findByRole('button', {
      name: `View attempts for request ${requestId}`,
    })
  )
  let drawer = await screen.findByRole('dialog', { name: 'Request attempts' })
  expect(await within(drawer).findByText('0.000000001 EUR')).toBeVisible()
  expect(within(drawer).getByText('1.23456789123456789')).toBeVisible()
  expect(within(drawer).getByText('admin-only-route')).toBeVisible()
  expect(within(drawer).getByText('Candidate Channels')).toBeVisible()
  expect(within(drawer).getByText('Eligible Channels')).toBeVisible()
  expect(within(drawer).getByText('Disabled: 2')).toBeVisible()
  expect(within(drawer).queryByText('never-show')).not.toBeInTheDocument()
  await user.keyboard('{Escape}')
  await user.click(screen.getByRole('tab', { name: 'Only Mine' }))
  await user.click(
    await screen.findByRole('button', {
      name: `View attempts for request ${requestId}`,
    })
  )
  drawer = await screen.findByRole('dialog', { name: 'Request attempts' })
  expect(await within(drawer).findByText(attempt.id)).toBeVisible()
  expect(within(drawer).queryByText('Purchase cost')).not.toBeInTheDocument()
  expect(within(drawer).queryByText('0.000000001 EUR')).not.toBeInTheDocument()
  expect(
    within(drawer).queryByText('1.23456789123456789')
  ).not.toBeInTheDocument()
  expect(within(drawer).queryByText('admin-only-route')).not.toBeInTheDocument()
  expect(within(drawer).queryByText('Provider ID')).not.toBeInTheDocument()
  expect(within(drawer).queryByText('Channel ID')).not.toBeInTheDocument()
  expect(within(drawer).queryByText('Member tier')).not.toBeInTheDocument()
  expect(urls.map((url) => url.pathname)).toEqual([
    '/api/admin/v1/requests',
    `/api/admin/v1/requests/${requestId}/attempts`,
    '/api/requests/self',
    `/api/requests/self/${requestId}/attempts`,
  ])
})

it('falls back to self endpoints when a legacy admin role lacks request:read permission', async () => {
  useAuthStore.getState().auth.setUser({
    id: 7,
    username: 'admin',
    role: 10,
    permissions: { admin_permissions: { request: { read: false } } },
  })
  renderRequests()
  expect(await screen.findByText('chat-standard')).toBeVisible()
  expect(screen.queryByRole('tab', { name: 'All' })).not.toBeInTheDocument()
  expect(urls[0].pathname).toBe('/api/requests/self')
})

it('clears the open admin drawer immediately when request permission is revoked', async () => {
  const admin = {
    id: 7,
    username: 'admin',
    role: 10,
    permissions: { admin_permissions: { request: { read: true } } },
  }
  useAuthStore.getState().auth.setUser(admin)
  const user = userEvent.setup()
  renderRequests()
  await user.click(
    await screen.findByRole('button', {
      name: `View attempts for request ${requestId}`,
    })
  )
  expect(
    await screen.findByRole('dialog', { name: 'Request attempts' })
  ).toBeVisible()
  await act(async () =>
    useAuthStore.getState().auth.setUser({
      ...admin,
      permissions: { admin_permissions: { request: { read: false } } },
    })
  )
  expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  expect(screen.queryByRole('tab', { name: 'All' })).not.toBeInTheDocument()
  await waitFor(() =>
    expect(urls.some((url) => url.pathname === '/api/requests/self')).toBe(true)
  )
})

it("does not reuse another authenticated user's rows or open drawer", async () => {
  const user = userEvent.setup()
  renderRequests()
  await user.click(
    await screen.findByRole('button', {
      name: `View attempts for request ${requestId}`,
    })
  )
  expect(
    await screen.findByRole('dialog', { name: 'Request attempts' })
  ).toBeVisible()
  api.defaults.adapter = async (config) => ({
    config,
    status: 200,
    statusText: 'OK',
    headers: {},
    data: {
      success: true,
      data: {
        items: [
          {
            ...request,
            offering_name: 'second-user-offering',
            id: 'second-user-request',
          },
        ],
        total: 1,
        page: 1,
        page_size: 20,
      },
    },
  })
  await act(async () =>
    useAuthStore.getState().auth.setUser({ id: 8, username: 'second', role: 1 })
  )
  expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  expect(screen.queryByText('chat-standard')).not.toBeInTheDocument()
  expect(await screen.findByText('second-user-offering')).toBeVisible()
})

it('preserves a request-list server error and retries into an explicit empty state', async () => {
  let failed = false
  api.defaults.adapter = async (config) => {
    const data = failed
      ? { success: true, data: { items: [], total: 0, page: 1, page_size: 20 } }
      : {
          success: false,
          message: 'Request history is temporarily unavailable',
        }
    failed = true
    return { config, data, status: 200, statusText: 'OK', headers: {} }
  }
  const user = userEvent.setup()
  renderRequests()
  expect(
    await screen.findByText('Request history is temporarily unavailable')
  ).toBeVisible()
  expect(screen.queryByText('No requests recorded')).not.toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Retry' }))
  expect(await screen.findByText('No requests recorded')).toBeVisible()
})

it('keeps an attempt network failure visible and retries into an empty attempt history', async () => {
  const baseAdapter = api.defaults.adapter as AxiosAdapter
  let attemptsFailed = false
  api.defaults.adapter = async (config) => {
    if (!config.url?.endsWith('/attempts')) return baseAdapter(config)
    if (!attemptsFailed) {
      attemptsFailed = true
      throw new AxiosError(
        'Service unavailable',
        'ERR_BAD_RESPONSE',
        config,
        undefined,
        {
          config,
          data: {
            success: false,
            message: 'Attempt history is temporarily unavailable',
          },
          status: 503,
          statusText: 'Service Unavailable',
          headers: {},
        }
      )
    }
    return {
      config,
      status: 200,
      statusText: 'OK',
      headers: {},
      data: {
        success: true,
        data: { items: [], total: 0, page: 1, page_size: 20 },
      },
    }
  }
  const user = userEvent.setup()
  renderRequests()
  await user.click(
    await screen.findByRole('button', {
      name: `View attempts for request ${requestId}`,
    })
  )
  const drawer = await screen.findByRole('dialog', { name: 'Request attempts' })
  expect(
    await within(drawer).findByText(
      'Attempt history is temporarily unavailable'
    )
  ).toBeVisible()
  expect(
    within(drawer).queryByText('No attempts recorded')
  ).not.toBeInTheDocument()
  await user.click(within(drawer).getByRole('button', { name: 'Retry' }))
  expect(await within(drawer).findByText('No attempts recorded')).toBeVisible()
})

it('applies supported identity and millisecond date filters and resets them', async () => {
  const user = userEvent.setup()
  renderRequests()
  await screen.findByText('chat-standard')
  await user.type(
    screen.getByRole('textbox', { name: 'Model Offering ID' }),
    '12'
  )
  await user.type(screen.getByRole('textbox', { name: 'Token ID' }), '4')
  expect(
    screen.queryByRole('textbox', { name: 'User ID' })
  ).not.toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Date Range' }))
  fireEvent.change(screen.getByLabelText('Start Time'), {
    target: { value: '2026-11-07T09:00:00' },
  })
  fireEvent.change(screen.getByLabelText('End Time'), {
    target: { value: '2026-11-07T10:00:00' },
  })
  await user.click(screen.getByRole('button', { name: 'Confirm' }))
  await user.click(screen.getByRole('button', { name: 'Search' }))
  await waitFor(() =>
    expect(urls.at(-1)?.searchParams.get('model_offering_id')).toBe('12')
  )
  const params = urls.at(-1)?.searchParams
  expect(params?.get('token_id')).toBe('4')
  expect(params?.get('started_after')).toBe(
    String(new Date('2026-11-07T09:00:00').getTime())
  )
  expect(params?.get('started_before')).toBe(
    String(new Date('2026-11-07T10:00:00').getTime())
  )
  expect(params?.has('user_id')).toBe(false)
  await user.click(screen.getByRole('button', { name: 'Reset' }))
  await waitFor(() =>
    expect(urls.at(-1)?.searchParams.has('started_after')).toBe(false)
  )
  expect(
    screen.getByRole('textbox', { name: 'Model Offering ID' })
  ).toHaveValue('')
})

it('paginates actual request pages and retains the active server-side filters', async () => {
  useAuthStore.getState().auth.setUser({
    id: 7,
    username: 'admin',
    role: 10,
    permissions: { admin_permissions: { request: { read: true } } },
  })
  api.defaults.adapter = async (config) => {
    const url = new URL(api.getUri(config), 'http://localhost')
    urls.push(url)
    const page = Number(url.searchParams.get('page'))
    return {
      config,
      status: 200,
      statusText: 'OK',
      headers: {},
      data: {
        success: true,
        data: {
          items: [
            {
              ...request,
              id: `page-${page}-request`,
              offering_name: `page-${page}-offering`,
            },
          ],
          total: 21,
          page,
          page_size: 20,
        },
      },
    }
  }
  const user = userEvent.setup()
  renderRequests()
  await screen.findByText('page-1-offering')
  await user.type(screen.getByRole('textbox', { name: 'User ID' }), '8')
  await user.click(screen.getByRole('button', { name: 'Search' }))
  await waitFor(() =>
    expect(urls.at(-1)?.searchParams.get('user_id')).toBe('8')
  )
  await user.click(screen.getByRole('button', { name: 'Go to next page' }))
  expect(await screen.findByText('page-2-offering')).toBeVisible()
  expect(urls.at(-1)?.searchParams.get('page')).toBe('2')
  expect(urls.at(-1)?.searchParams.get('user_id')).toBe('8')
  expect(screen.queryByText('page-1-offering')).not.toBeInTheDocument()
})

it('rejects invalid IDs and a reversed date range without sending those filters', async () => {
  const user = userEvent.setup()
  renderRequests()
  await screen.findByText('chat-standard')
  await user.type(screen.getByRole('textbox', { name: 'Token ID' }), '-1')
  await user.click(screen.getByRole('button', { name: 'Search' }))
  expect(screen.getByText('Enter positive whole-number IDs.')).toBeVisible()
  expect(screen.getByRole('textbox', { name: 'Token ID' })).toHaveAttribute(
    'aria-invalid',
    'true'
  )
  expect(urls).toHaveLength(1)
  await user.clear(screen.getByRole('textbox', { name: 'Token ID' }))
  await user.click(screen.getByRole('button', { name: 'Date Range' }))
  fireEvent.change(screen.getByLabelText('Start Time'), {
    target: { value: '2026-11-07T10:00:00' },
  })
  fireEvent.change(screen.getByLabelText('End Time'), {
    target: { value: '2026-11-07T09:00:00' },
  })
  await user.click(screen.getByRole('button', { name: 'Confirm' }))
  await user.click(screen.getByRole('button', { name: 'Search' }))
  expect(screen.getByText('End time must be after start time')).toBeVisible()
  expect(urls).toHaveLength(1)
})

it('paginates recorded attempts using the selected request endpoint', async () => {
  const baseAdapter = api.defaults.adapter as AxiosAdapter
  api.defaults.adapter = async (config) => {
    if (!config.url?.endsWith('/attempts')) return baseAdapter(config)
    const url = new URL(api.getUri(config), 'http://localhost')
    urls.push(url)
    const page = Number(url.searchParams.get('page'))
    return {
      config,
      status: 200,
      statusText: 'OK',
      headers: {},
      data: {
        success: true,
        data: {
          items: [{ ...attempt, id: `attempt-page-${page}` }],
          total: 21,
          page,
          page_size: 20,
        },
      },
    }
  }
  const user = userEvent.setup()
  renderRequests()
  await user.click(
    await screen.findByRole('button', {
      name: `View attempts for request ${requestId}`,
    })
  )
  const drawer = await screen.findByRole('dialog', { name: 'Request attempts' })
  expect(await within(drawer).findByText('attempt-page-1')).toBeVisible()
  await user.click(
    within(drawer).getByRole('button', { name: 'Go to next page' })
  )
  expect(await within(drawer).findByText('attempt-page-2')).toBeVisible()
  expect(within(drawer).queryByText('attempt-page-1')).not.toBeInTheDocument()
  expect(urls.at(-1)?.pathname).toBe(`/api/requests/self/${requestId}/attempts`)
  expect(urls.at(-1)?.searchParams.get('page')).toBe('2')
})

it('shows missing optional usage and charge fields without inventing zero usage or a retry', async () => {
  api.defaults.adapter = async (config) => ({
    config,
    status: 200,
    statusText: 'OK',
    headers: {},
    data: {
      success: true,
      data: {
        items: config.url?.endsWith('/attempts')
          ? [
              {
                id: 'minimal-attempt',
                request_id: 'minimal-request',
                status: 'failed',
                started_at: request.started_at,
                usage_source: 'none',
              },
            ]
          : [
              {
                id: 'minimal-request',
                user_id: 7,
                status: 'failed',
                started_at: request.started_at,
                usage_source: 'none',
                error_code: 'upstream_unavailable',
              },
            ],
        total: 1,
        page: 1,
        page_size: 20,
      },
    },
  })
  const user = userEvent.setup()
  renderRequests()
  expect(await screen.findByText('No usage available')).toBeVisible()
  expect(screen.getByText('upstream_unavailable')).toBeVisible()
  await user.click(
    screen.getByRole('button', {
      name: 'View attempts for request minimal-request',
    })
  )
  const drawer = await screen.findByRole('dialog', { name: 'Request attempts' })
  expect(await within(drawer).findByText('minimal-attempt')).toBeVisible()
  expect(
    within(
      within(drawer).getByRole('list', { name: 'Attempt chronology' })
    ).getAllByRole('listitem')
  ).toHaveLength(1)
  expect(within(drawer).queryByText('Estimated')).not.toBeInTheDocument()
  expect(within(drawer).queryByText('Purchase cost')).not.toBeInTheDocument()
})

it('shows actual media quantities including zero in request rows and attempts without inventing token usage', async () => {
  api.defaults.adapter = async (config) => ({
    config,
    status: 200,
    statusText: 'OK',
    headers: {},
    data: {
      success: true,
      data: {
        items: [
          {
            ...(config.url?.endsWith('/attempts') ? attempt : request),
            usage: {
              image_count: 0,
              audio_input_seconds: '1.25',
              audio_output_seconds: '0',
            },
            usage_source: 'none',
          },
        ],
        total: 1,
        page: 1,
        page_size: 20,
      },
    },
  })
  const user = userEvent.setup()
  renderRequests()
  expect(await screen.findByText('Image count: 0')).toBeVisible()
  expect(screen.getByText('Input audio duration: 1.25 seconds')).toBeVisible()
  expect(screen.getByText('Output audio duration: 0 seconds')).toBeVisible()
  expect(screen.queryByText('Total tokens: 0')).not.toBeInTheDocument()
  await user.click(
    screen.getByRole('button', {
      name: `View attempts for request ${requestId}`,
    })
  )
  const drawer = await screen.findByRole('dialog', { name: 'Request attempts' })
  expect(await within(drawer).findByText('Image count: 0')).toBeVisible()
  expect(
    within(drawer).getByText('Input audio duration: 1.25 seconds')
  ).toBeVisible()
})

it('updates the exact charge separators when the interface language changes', async () => {
  i18next.addResourceBundle('fr', 'translation', fr.translation)
  i18next.addResourceBundle('zhTW', 'translation', zhTW.translation)
  renderRequests()
  expect(await screen.findByText('9,007,199.254740993 USD')).toBeVisible()
  await act(async () => {
    await i18next.changeLanguage('fr')
  })
  expect(screen.getByText('9 007 199,254740993 USD')).toBeVisible()
  await act(async () => {
    await i18next.changeLanguage('zhTW')
  })
  expect(screen.getByText('9,007,199.254740993 USD')).toBeVisible()
})

it('keeps long IDs, exact charges, and the attempt action available in the mobile list', async () => {
  const matchMedia = window.matchMedia.bind(window)
  vi.spyOn(window, 'matchMedia').mockImplementation((query) => ({
    ...matchMedia(query),
    matches: query === '(max-width: 640px)' || matchMedia(query).matches,
  }))
  const user = userEvent.setup()
  renderRequests()
  expect(await screen.findByText(requestId)).toHaveAttribute('title', requestId)
  expect(screen.queryByRole('table')).not.toBeInTheDocument()
  expect(screen.getByText('9,007,199.254740993 USD')).toBeVisible()
  await user.click(
    screen.getByRole('button', {
      name: `View attempts for request ${requestId}`,
    })
  )
  const drawer = await screen.findByRole('dialog', { name: 'Request attempts' })
  expect(await within(drawer).findByText(attempt.id)).toBeVisible()
  await user.click(within(drawer).getByRole('button', { name: 'Close' }))
  expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
})

it('clears the previous request attempts while another request is loading', async () => {
  const baseAdapter = api.defaults.adapter as AxiosAdapter
  let releaseAttempt: (() => void) | undefined
  api.defaults.adapter = async (config) => {
    if (config.url?.includes('/second-request/attempts')) {
      await new Promise<void>((resolve) => {
        releaseAttempt = resolve
      })
      return {
        config,
        status: 200,
        statusText: 'OK',
        headers: {},
        data: {
          success: true,
          data: { items: [], total: 0, page: 1, page_size: 20 },
        },
      }
    }
    if (config.url?.endsWith('/attempts')) return baseAdapter(config)
    return {
      config,
      status: 200,
      statusText: 'OK',
      headers: {},
      data: {
        success: true,
        data: {
          items: [request, { ...request, id: 'second-request' }],
          total: 2,
          page: 1,
          page_size: 20,
        },
      },
    }
  }
  const user = userEvent.setup()
  renderRequests()
  await user.click(
    await screen.findByRole('button', {
      name: `View attempts for request ${requestId}`,
    })
  )
  let drawer = await screen.findByRole('dialog', { name: 'Request attempts' })
  expect(await within(drawer).findByText(attempt.id)).toBeVisible()
  await user.keyboard('{Escape}')
  await user.click(
    screen.getByRole('button', {
      name: 'View attempts for request second-request',
    })
  )
  drawer = await screen.findByRole('dialog', { name: 'Request attempts' })
  expect(within(drawer).queryByText(attempt.id)).not.toBeInTheDocument()
  expect(within(drawer).getByText('Loading attempts...')).toBeVisible()
  await act(async () => {
    releaseAttempt?.()
  })
  expect(await within(drawer).findByText('No attempts recorded')).toBeVisible()
})
