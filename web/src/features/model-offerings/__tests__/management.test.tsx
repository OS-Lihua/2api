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
import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { AxiosError } from 'axios'
import { afterEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { ModelOfferings } from '..'
import type { ModelOffering } from '../types'

const offering: ModelOffering = {
  id: 9,
  name: 'assistant',
  aliases: ['assistant-v1'],
  visibility: 'all',
  groups: [],
  members: [{ model_id: 7, tier: 1 }],
  channel_scope: {
    include_provider_ids: [],
    exclude_provider_ids: [],
    include_channel_ids: [],
    exclude_channel_ids: [],
  },
  route_method: 'cost',
  accepted_conversion_losses: [],
  minimum_margin_rate: '0',
  allow_loss_fallback: false,
  enabled: false,
  created_at: 1,
  updated_at: 1,
}

function mockManagementApi(items: ModelOffering[] = [offering]) {
  return vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/admin/v1/model-offerings/9') {
      return { data: { success: true, data: offering } }
    }
    if (url === '/api/admin/v1/model-offerings') {
      return {
        data: {
          success: true,
          data: { items, total: items.length, page: 1, page_size: 20 },
        },
      }
    }
    if (url === '/api/admin/v1/models') {
      return {
        data: {
          success: true,
          data: {
            items: [
              { id: 7, name: 'full-model' },
              { id: 8, name: 'fast-model' },
            ],
            total: 2,
          },
        },
      }
    }
    if (url === '/api/admin/v1/providers') {
      return {
        data: {
          success: true,
          data: {
            items: [
              { id: 3, name: 'Primary account' },
              { id: 4, name: 'Backup account' },
            ],
            total: 2,
          },
        },
      }
    }
    if (url === '/api/admin/v1/channels') {
      return {
        data: {
          success: true,
          data: {
            items: [
              {
                id: 11,
                provider_id: 3,
                protocol: 'openai',
                upstream_model_name: 'upstream-a',
              },
              {
                id: 12,
                provider_id: 4,
                protocol: 'openai',
                upstream_model_name: 'upstream-b',
              },
            ],
            total: 2,
          },
        },
      }
    }
    if (url === '/api/group/') {
      return { data: { success: true, data: ['premium'] } }
    }
    throw new Error(`Unexpected GET ${url}`)
  })
}

async function selectItem(
  user: ReturnType<typeof userEvent.setup>,
  label: string,
  option: string
) {
  await user.click(screen.getByRole('combobox', { name: label }))
  await user.click(await screen.findByRole('option', { name: option }))
  await user.keyboard('{Escape}')
}

const clients: QueryClient[] = []
afterEach(() => {
  clients.forEach((client) => client.clear())
  clients.length = 0
  vi.restoreAllMocks()
  localStorage.clear()
  useAuthStore.getState().auth.reset()
})

function renderPage(role = 100, canWrite = false, staleTime = 0) {
  useAuthStore.getState().auth.setUser({
    id: 1,
    username: 'admin',
    role,
    permissions: {
      admin_permissions: { model: { read: true, write: canWrite } },
    },
  })
  const client = new QueryClient({
    defaultOptions: {
      queries: { retry: false, staleTime },
      mutations: { retry: false },
    },
  })
  clients.push(client)
  return render(
    <QueryClientProvider client={client}>
      <ModelOfferings />
    </QueryClientProvider>
  )
}

it('lists primary names, aliases and status, and searches through the management API', async () => {
  const get = vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: { items: [offering], total: 1, page: 1, page_size: 20 },
    },
  })
  renderPage()
  expect(await screen.findByText('assistant')).toBeVisible()
  expect(screen.getByText('assistant-v1')).toBeVisible()
  expect(screen.getByText('Disabled')).toBeVisible()
  expect(get).toHaveBeenCalledWith('/api/admin/v1/model-offerings', {
    params: { p: 1, page_size: 20, keyword: '' },
  })
  const user = userEvent.setup()
  await user.type(
    screen.getByPlaceholderText('Search model offerings...'),
    'assistant'
  )
  await waitFor(() =>
    expect(get).toHaveBeenLastCalledWith('/api/admin/v1/model-offerings', {
      params: { p: 1, page_size: 20, keyword: 'assistant' },
    })
  )
})

it('creates a disabled offering with visual tiers, scope, visibility and route settings', async () => {
  mockManagementApi([])
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValue({ data: { success: true, data: offering } })
  renderPage()
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'New model offering' }))
  await user.type(await screen.findByLabelText('Primary name'), 'assistant')
  await user.type(
    screen.getByRole('combobox', { name: 'Aliases' }),
    'assistant-v1,'
  )
  await user.keyboard('{Escape}')
  await selectItem(user, 'Models in tier 1', 'full-model')
  await user.click(screen.getByRole('button', { name: 'Add member tier' }))
  await selectItem(user, 'Models in tier 2', 'fast-model')
  await user.selectOptions(screen.getByLabelText('Visibility'), 'groups')
  await selectItem(user, 'User groups', 'premium')
  await selectItem(user, 'Include providers', 'Primary account')
  await selectItem(user, 'Exclude providers', 'Backup account')
  await selectItem(
    user,
    'Include channels',
    '#11 · upstream-a · openai · Primary account'
  )
  await selectItem(
    user,
    'Exclude channels',
    '#12 · upstream-b · openai · Backup account'
  )
  await user.selectOptions(screen.getByLabelText('Route method'), 'latency')
  await user.type(
    screen.getByRole('combobox', { name: 'Accepted conversion losses' }),
    'tool_choice,'
  )
  await user.keyboard('{Escape}')
  await user.clear(screen.getByLabelText('Minimum margin rate'))
  await user.type(screen.getByLabelText('Minimum margin rate'), '0.125')
  await user.click(screen.getByRole('switch', { name: 'Allow loss fallback' }))
  await user.click(screen.getByRole('button', { name: 'Save model offering' }))
  await waitFor(() =>
    expect(post).toHaveBeenCalledWith('/api/admin/v1/model-offerings', {
      name: 'assistant',
      aliases: ['assistant-v1'],
      visibility: 'groups',
      groups: ['premium'],
      members: [
        { model_id: 7, tier: 1 },
        { model_id: 8, tier: 2 },
      ],
      channel_scope: {
        include_provider_ids: [3],
        exclude_provider_ids: [4],
        include_channel_ids: [11],
        exclude_channel_ids: [12],
      },
      route_method: 'latency',
      accepted_conversion_losses: ['tool_choice'],
      minimum_margin_rate: '0.125',
      allow_loss_fallback: true,
    })
  )
  await waitFor(() =>
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  )
})

it('loads the full offering before renaming, preserves all settings and shows the retained old-name alias', async () => {
  let saved = offering
  mockManagementApi().mockImplementation(async (url) => {
    if (url === '/api/admin/v1/model-offerings/9') {
      return { data: { success: true, data: saved } }
    }
    if (url === '/api/admin/v1/model-offerings') {
      return {
        data: {
          success: true,
          data: { items: [saved], total: 1, page: 1, page_size: 20 },
        },
      }
    }
    if (url === '/api/group/') return { data: { success: true, data: [] } }
    return {
      data: {
        success: true,
        data: { items: [{ id: 7, name: 'full-model' }], total: 1 },
      },
    }
  })
  const put = vi.spyOn(api, 'put').mockImplementation(async () => {
    saved = {
      ...offering,
      name: 'assistant-v2',
      aliases: ['assistant-v1', 'assistant'],
    }
    return { data: { success: true, data: saved } }
  })
  renderPage()
  const user = userEvent.setup()
  await user.click(await screen.findByRole('button', { name: 'Edit' }))
  const name = await screen.findByLabelText('Primary name')
  expect(name).toHaveValue('assistant')
  expect(
    screen.getByText('Renaming keeps the previous primary name as an alias.')
  ).toBeVisible()
  await user.clear(name)
  await user.type(name, 'assistant-v2')
  await user.click(screen.getByRole('button', { name: 'Save model offering' }))
  await waitFor(() =>
    expect(put).toHaveBeenCalledWith('/api/admin/v1/model-offerings/9', {
      name: 'assistant-v2',
      aliases: ['assistant-v1'],
      visibility: 'all',
      groups: [],
      members: [{ model_id: 7, tier: 1 }],
      channel_scope: {
        include_provider_ids: [],
        exclude_provider_ids: [],
        include_channel_ids: [],
        exclude_channel_ids: [],
      },
      route_method: 'cost',
      accepted_conversion_losses: [],
      minimum_margin_rate: '0',
      allow_loss_fallback: false,
    })
  )
  expect(await screen.findByText('assistant-v2')).toBeVisible()
  expect(await screen.findByText('assistant-v1, assistant')).toBeVisible()
})

it('keeps the offering disabled and shows the server reason when enabling without a sale price fails', async () => {
  mockManagementApi()
  const error = new AxiosError('Request failed with status code 400')
  error.response = {
    data: {
      success: false,
      message: 'A sale price is required to enable a model offering',
    },
    status: 400,
  } as typeof error.response
  const post = vi.spyOn(api, 'post').mockRejectedValue(error)
  renderPage()
  const user = userEvent.setup()
  await user.click(await screen.findByRole('button', { name: 'Enable' }))
  await user.click(
    within(screen.getByRole('alertdialog')).getByRole('button', {
      name: 'Enable',
    })
  )
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'A sale price is required to enable a model offering'
  )
  expect(post).toHaveBeenCalledWith('/api/admin/v1/model-offerings/9/enable')
  await user.click(screen.getByRole('button', { name: 'Cancel' }))
  expect(screen.getByText('Disabled')).toBeVisible()
})

it('hides write actions from an administrator with model read permission only', async () => {
  mockManagementApi()
  renderPage(10)
  expect(await screen.findByText('assistant')).toBeVisible()
  for (const name of ['New model offering', 'Edit', 'Enable', 'Delete']) {
    expect(screen.queryByRole('button', { name })).not.toBeInTheDocument()
  }
})

it.each([
  { tier: '2', message: 'Include a member tier numbered 1' },
  { tier: '0', message: 'Enter a positive integer' },
])(
  'rejects invalid member tier $tier and focuses the invalid tier',
  async ({ tier: value, message }) => {
    mockManagementApi()
    const put = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    renderPage()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Edit' }))
    const tier = await screen.findByRole('spinbutton', { name: 'Tier number' })
    await user.clear(tier)
    await user.type(tier, value)
    await user.click(
      screen.getByRole('button', { name: 'Save model offering' })
    )
    expect(await screen.findByText(message)).toBeVisible()
    expect(tier).toHaveAttribute('aria-invalid', 'true')
    await waitFor(() => expect(tier).toHaveFocus())
    expect(put).not.toHaveBeenCalled()
  }
)

it('rejects whitespace in primary names before submitting the full replacement', async () => {
  mockManagementApi()
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: true } })
  renderPage()
  const user = userEvent.setup()
  await user.click(await screen.findByRole('button', { name: 'Edit' }))
  const name = await screen.findByLabelText('Primary name')
  await user.clear(name)
  await user.type(name, 'invalid name')
  await user.click(screen.getByRole('button', { name: 'Save model offering' }))
  expect(
    await screen.findByText(
      'ModelOffering names must contain 1 to 128 characters without whitespace'
    )
  ).toBeVisible()
  expect(name).toHaveAttribute('aria-invalid', 'true')
  expect(put).not.toHaveBeenCalled()
})

it('confirms enable, disable and deletion and refreshes the displayed offering after each successful action', async () => {
  let items = [offering]
  mockManagementApi().mockImplementation(async () => ({
    data: {
      success: true,
      data: { items, total: items.length, page: 1, page_size: 20 },
    },
  }))
  const post = vi.spyOn(api, 'post').mockImplementation(async (url) => {
    items = [{ ...offering, enabled: url.endsWith('/enable') }]
    return { data: { success: true, data: items[0] } }
  })
  const remove = vi.spyOn(api, 'delete').mockImplementation(async () => {
    items = []
    return { data: { success: true } }
  })
  renderPage()
  const user = userEvent.setup()
  await user.click(await screen.findByRole('button', { name: 'Enable' }))
  expect(post).not.toHaveBeenCalled()
  await user.click(
    within(screen.getByRole('alertdialog')).getByRole('button', {
      name: 'Enable',
    })
  )
  expect(await screen.findByText('Enabled')).toBeVisible()
  await user.click(screen.getByRole('button', { name: 'Disable' }))
  await user.click(
    within(screen.getByRole('alertdialog')).getByRole('button', {
      name: 'Disable',
    })
  )
  expect(await screen.findByText('Disabled')).toBeVisible()
  expect(post).toHaveBeenLastCalledWith(
    '/api/admin/v1/model-offerings/9/disable'
  )
  await user.click(screen.getByRole('button', { name: 'Delete' }))
  await user.click(screen.getByRole('button', { name: 'Cancel' }))
  expect(remove).not.toHaveBeenCalled()
  await user.click(screen.getByRole('button', { name: 'Delete' }))
  await user.click(
    within(screen.getByRole('alertdialog')).getByRole('button', {
      name: 'Delete',
    })
  )
  expect(await screen.findByText('No model offerings found')).toBeVisible()
  expect(remove).toHaveBeenCalledWith('/api/admin/v1/model-offerings/9')
})

it('locks inputs while saving and keeps the draft after a server rejection so the user can retry', async () => {
  mockManagementApi()
  let finish!: (value: Awaited<ReturnType<typeof api.put>>) => void
  const pending = new Promise<Awaited<ReturnType<typeof api.put>>>(
    (resolve) => {
      finish = resolve
    }
  )
  const put = vi
    .spyOn(api, 'put')
    .mockReturnValueOnce(pending)
    .mockResolvedValue({ data: { success: true } })
  renderPage()
  const user = userEvent.setup()
  await user.click(await screen.findByRole('button', { name: 'Edit' }))
  const name = await screen.findByLabelText('Primary name')
  await user.clear(name)
  await user.type(name, 'assistant-new')
  await user.click(screen.getByRole('button', { name: 'Save model offering' }))
  await waitFor(() => expect(put).toHaveBeenCalled())
  expect(name).toBeDisabled()
  expect(screen.getByRole('combobox', { name: 'Aliases' })).toBeDisabled()
  expect(screen.getByRole('button', { name: 'Saving...' })).toBeDisabled()
  await act(async () =>
    finish({
      data: {
        success: false,
        message: 'ModelOffering name or alias already exists',
      },
    })
  )
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'ModelOffering name or alias already exists'
  )
  expect(name).toHaveValue('assistant-new')
  expect(name).toBeEnabled()
  await user.click(screen.getByRole('button', { name: 'Save model offering' }))
  await waitFor(() =>
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  )
})

it('marks an overlapping channel scope invalid and preserves the selection for correction', async () => {
  mockManagementApi()
  const put = vi.spyOn(api, 'put')
  renderPage()
  const user = userEvent.setup()
  await user.click(await screen.findByRole('button', { name: 'Edit' }))
  await screen.findByLabelText('Primary name')
  await selectItem(user, 'Include providers', 'Primary account')
  await selectItem(user, 'Exclude providers', 'Primary account')
  await user.click(screen.getByRole('button', { name: 'Save model offering' }))
  expect(
    await screen.findByText(
      'The same item cannot be both included and excluded'
    )
  ).toBeVisible()
  expect(
    screen.getByRole('combobox', { name: 'Exclude providers' })
  ).toHaveAttribute('aria-invalid', 'true')
  expect(put).not.toHaveBeenCalled()
})

it('warns before discarding a changed member tier and preserves the draft when the user cancels', async () => {
  mockManagementApi()
  renderPage()
  const user = userEvent.setup()
  await user.click(await screen.findByRole('button', { name: 'Edit' }))
  await screen.findByLabelText('Primary name')
  await user.click(screen.getByRole('button', { name: 'Add member tier' }))
  await user.click(screen.getByRole('button', { name: 'Cancel' }))
  expect(await screen.findByRole('alertdialog')).toHaveTextContent(
    'Discard unsaved changes?'
  )
  await user.click(
    within(screen.getByRole('alertdialog')).getByRole('button', {
      name: 'Cancel',
    })
  )
  expect(
    screen.getByRole('combobox', { name: 'Models in tier 2' })
  ).toBeVisible()
  await user.click(screen.getByRole('button', { name: 'Cancel' }))
  await user.click(screen.getByRole('button', { name: 'Discard' }))
  await waitFor(() =>
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  )
})

it('returns to the preceding page after deleting the last result on the final page', async () => {
  let deleted = false
  const get = mockManagementApi().mockImplementation(async (_url, config) => {
    const page = (config?.params as { p?: number } | undefined)?.p ?? 1
    const items =
      page === 1 ? [{ ...offering, id: 1, name: 'first-offering' }] : [offering]
    return {
      data: {
        success: true,
        data: {
          items: page === 2 && deleted ? [] : items,
          total: deleted ? 20 : 21,
          page,
          page_size: 20,
        },
      },
    }
  })
  vi.spyOn(api, 'delete').mockImplementation(async () => {
    deleted = true
    return { data: { success: true } }
  })
  renderPage()
  const user = userEvent.setup()
  await screen.findByText('first-offering')
  await user.click(screen.getByRole('button', { name: 'Go to next page' }))
  expect(await screen.findByText('assistant')).toBeVisible()
  await user.click(screen.getByRole('button', { name: 'Delete' }))
  await user.click(
    within(screen.getByRole('alertdialog')).getByRole('button', {
      name: 'Delete',
    })
  )
  await waitFor(() => expect(screen.getByText('first-offering')).toBeVisible())
  expect(get).toHaveBeenLastCalledWith('/api/admin/v1/model-offerings', {
    params: { p: 1, page_size: 20, keyword: '' },
  })
})

it('blocks incomplete members, empty group visibility and an out-of-range margin with accessible field errors', async () => {
  mockManagementApi([])
  const post = vi.spyOn(api, 'post')
  renderPage()
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'New model offering' }))
  await user.type(await screen.findByLabelText('Primary name'), 'assistant')
  await user.selectOptions(screen.getByLabelText('Visibility'), 'groups')
  await user.clear(screen.getByLabelText('Minimum margin rate'))
  await user.type(screen.getByLabelText('Minimum margin rate'), '1.01')
  await user.click(screen.getByRole('button', { name: 'Save model offering' }))
  for (const message of [
    'Select at least one model in each tier',
    'Select at least one user group',
    'Enter a decimal rate from 0 to 1',
  ]) {
    expect(await screen.findByText(message)).toBeVisible()
  }
  for (const label of [
    'Models in tier 1',
    'User groups',
    'Minimum margin rate',
  ]) {
    expect(screen.getByLabelText(label)).toHaveAttribute('aria-invalid', 'true')
  }
  expect(post).not.toHaveBeenCalled()
})

it('reopens a still-fresh cache with production freshness and loads the current offering', async () => {
  const get = mockManagementApi()
  const originalGet = get.getMockImplementation()
  if (!originalGet) throw new Error('Missing API fixture')
  renderPage(100, false, 10_000)
  const user = userEvent.setup()
  await user.click(await screen.findByRole('button', { name: 'Edit' }))
  await screen.findByLabelText('Primary name')
  await user.click(screen.getByRole('button', { name: 'Cancel' }))
  get.mockImplementation((url, config) =>
    url === '/api/admin/v1/model-offerings/9'
      ? Promise.resolve({
          data: {
            success: true,
            data: {
              ...offering,
              route_method: 'weight',
              minimum_margin_rate: '0.25',
            },
          },
        })
      : originalGet(url, config)
  )
  await user.click(screen.getByRole('button', { name: 'Edit' }))
  expect(await screen.findByLabelText('Route method')).toHaveValue('weight')
  expect(screen.getByLabelText('Minimum margin rate')).toHaveValue('0.25')
})

it('waits for fresh detail when reopening a cached offering before saving a full replacement', async () => {
  const get = mockManagementApi()
  const originalGet = get.getMockImplementation()
  if (!originalGet) throw new Error('Missing API fixture')
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: true } })
  renderPage()
  const user = userEvent.setup()
  await user.click(await screen.findByRole('button', { name: 'Edit' }))
  await screen.findByLabelText('Primary name')
  await user.click(screen.getByRole('button', { name: 'Cancel' }))

  let finish!: (value: Awaited<ReturnType<typeof api.get>>) => void
  get.mockImplementation((url, config) => {
    if (url === '/api/admin/v1/model-offerings/9') {
      return new Promise((resolve) => {
        finish = resolve
      })
    }
    return originalGet(url, config)
  })
  await user.click(screen.getByRole('button', { name: 'Edit' }))
  expect(screen.queryByLabelText('Primary name')).not.toBeInTheDocument()
  await act(async () =>
    finish({
      data: {
        success: true,
        data: {
          ...offering,
          route_method: 'weight',
          minimum_margin_rate: '0.25',
        },
      },
    })
  )
  expect(await screen.findByLabelText('Route method')).toHaveValue('weight')
  expect(screen.getByLabelText('Minimum margin rate')).toHaveValue('0.25')
  await user.click(screen.getByRole('button', { name: 'Save model offering' }))
  await waitFor(() =>
    expect(put).toHaveBeenCalledWith(
      '/api/admin/v1/model-offerings/9',
      expect.objectContaining({
        route_method: 'weight',
        minimum_margin_rate: '0.25',
      })
    )
  )
})

it('blocks cached replacement after a failed initial detail refresh and retries before allowing edits', async () => {
  const get = mockManagementApi()
  const originalGet = get.getMockImplementation()
  if (!originalGet) throw new Error('Missing API fixture')
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: true } })
  renderPage()
  const user = userEvent.setup()
  await user.click(await screen.findByRole('button', { name: 'Edit' }))
  await screen.findByLabelText('Primary name')
  await user.click(screen.getByRole('button', { name: 'Cancel' }))
  get.mockImplementation((url, config) =>
    url === '/api/admin/v1/model-offerings/9'
      ? Promise.reject(new Error('Failed to refresh'))
      : originalGet(url, config)
  )
  await user.click(screen.getByRole('button', { name: 'Edit' }))
  await screen.findByRole('button', { name: 'Retry' })
  expect(screen.queryByLabelText('Primary name')).not.toBeInTheDocument()
  expect(
    screen.queryByRole('button', { name: 'Save model offering' })
  ).not.toBeInTheDocument()
  expect(put).not.toHaveBeenCalled()
  get.mockImplementation((url, config) =>
    url === '/api/admin/v1/model-offerings/9'
      ? Promise.resolve({
          data: {
            success: true,
            data: {
              ...offering,
              route_method: 'weight',
              minimum_margin_rate: '0.25',
            },
          },
        })
      : originalGet(url, config)
  )
  await user.click(screen.getByRole('button', { name: 'Retry' }))
  const name = await screen.findByLabelText('Primary name')
  await user.clear(name)
  await user.type(name, 'renamed')
  await user.click(screen.getByRole('button', { name: 'Save model offering' }))
  await waitFor(() =>
    expect(put).toHaveBeenCalledWith(
      '/api/admin/v1/model-offerings/9',
      expect.objectContaining({
        name: 'renamed',
        route_method: 'weight',
        minimum_margin_rate: '0.25',
      })
    )
  )
})

it('applies refreshed detail while the editor is pristine and preserves fields after editing starts', async () => {
  const get = mockManagementApi()
  const originalGet = get.getMockImplementation()
  if (!originalGet) throw new Error('Missing API fixture')
  let current = offering
  get.mockImplementation((url, config) =>
    url === '/api/admin/v1/model-offerings/9'
      ? Promise.resolve({ data: { success: true, data: current } })
      : originalGet(url, config)
  )
  renderPage()
  const user = userEvent.setup()
  await user.click(await screen.findByRole('button', { name: 'Edit' }))
  await screen.findByLabelText('Primary name')
  current = { ...offering, name: 'server-name', minimum_margin_rate: '0.25' }
  await act(async () => {
    await clients[0].refetchQueries()
  })
  await waitFor(() =>
    expect(screen.getByLabelText('Primary name')).toHaveValue('server-name')
  )
  expect(screen.getByLabelText('Minimum margin rate')).toHaveValue('0.25')
  await user.clear(screen.getByLabelText('Primary name'))
  await user.type(screen.getByLabelText('Primary name'), 'draft-name')
  current = {
    ...current,
    name: 'another-server-name',
    minimum_margin_rate: '0.5',
  }
  await act(async () => {
    await clients[0].refetchQueries()
  })
  expect(screen.getByLabelText('Primary name')).toHaveValue('draft-name')
  expect(screen.getByLabelText('Minimum margin rate')).toHaveValue('0.25')
})

it.each(['/api/admin/v1/model-offerings/9', '/api/admin/v1/models'])(
  'preserves a dirty editor and allows retry when a background refresh of %s fails',
  async (failedPath) => {
    const get = mockManagementApi()
    const originalGet = get.getMockImplementation()
    if (!originalGet) throw new Error('Missing API fixture')
    renderPage()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Edit' }))
    const name = await screen.findByLabelText('Primary name')
    await user.clear(name)
    await user.type(name, 'draft-name')
    get.mockImplementation((url, config) =>
      url === failedPath
        ? Promise.reject(new Error('Failed to refresh'))
        : originalGet(url, config)
    )
    await act(async () => {
      await clients[0].refetchQueries()
    })
    expect(await screen.findByText('Failed to refresh')).toBeVisible()
    expect(screen.getByLabelText('Primary name')).toHaveValue('draft-name')
    get.mockImplementation(originalGet)
    await user.click(screen.getByRole('button', { name: 'Retry' }))
    await waitFor(() =>
      expect(screen.queryByText('Failed to refresh')).not.toBeInTheDocument()
    )
    expect(screen.getByLabelText('Primary name')).toHaveValue('draft-name')
  }
)

it.each([
  {
    label: 'Aliases',
    field: 'aliases',
    value: 'pending-alias',
    expected: ['assistant-v1', 'pending-alias'],
  },
  {
    label: 'Accepted conversion losses',
    field: 'accepted_conversion_losses',
    value: 'tool_choice',
    expected: ['tool_choice'],
  },
])(
  'includes a typed $label draft when Save is clicked without Enter',
  async ({ label, field, value, expected }) => {
    mockManagementApi()
    const put = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    renderPage()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Edit' }))
    await user.type(await screen.findByRole('combobox', { name: label }), value)
    await user.tab()
    expect(screen.getByRole('combobox', { name: label })).toHaveValue('')
    await user.click(
      screen.getByRole('button', { name: 'Save model offering' })
    )
    await waitFor(() =>
      expect(put).toHaveBeenCalledWith(
        '/api/admin/v1/model-offerings/9',
        expect.objectContaining({ [field]: expected })
      )
    )
  }
)

it.each([
  {
    label: 'Aliases',
    field: 'aliases',
    expected: ['assistant-v1', 'pending-tag'],
  },
  {
    label: 'Accepted conversion losses',
    field: 'accepted_conversion_losses',
    expected: ['pending-tag'],
  },
])(
  'warns before Escape closes an editor with only an uncommitted $label draft',
  async ({ label, field, expected }) => {
    mockManagementApi()
    const put = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    renderPage()
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Edit' }))
    await user.type(
      await screen.findByRole('combobox', { name: label }),
      'pending-tag'
    )
    await user.keyboard('{Escape}')
    await waitFor(() =>
      expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
    )
    expect(screen.getByRole('combobox', { name: label })).toHaveValue('')
    expect(screen.getByRole('combobox', { name: label })).toHaveFocus()
    await user.keyboard('{Escape}')
    const confirmation = await screen.findByRole('alertdialog')
    expect(confirmation).toHaveTextContent('Discard unsaved changes?')
    await user.click(
      within(confirmation).getByRole('button', { name: 'Cancel' })
    )
    await user.click(
      screen.getByRole('button', { name: 'Save model offering' })
    )
    await waitFor(() =>
      expect(put).toHaveBeenCalledWith(
        '/api/admin/v1/model-offerings/9',
        expect.objectContaining({ [field]: expected })
      )
    )
  }
)

it.each([
  {
    forbidden: ['providers'],
    disabled: ['Include providers', 'Exclude providers'],
    enabled: 'Include channels',
  },
  {
    forbidden: ['channels'],
    disabled: ['Include channels', 'Exclude channels'],
    enabled: 'Include providers',
  },
  {
    forbidden: ['providers', 'channels'],
    disabled: [
      'Include providers',
      'Exclude providers',
      'Include channels',
      'Exclude channels',
    ],
    enabled: 'Models in tier 1',
  },
])(
  'allows offering edits while preserving scope IDs when $forbidden choices return 403',
  async ({ forbidden, disabled, enabled }) => {
    const get = mockManagementApi()
    const originalGet = get.getMockImplementation()
    if (!originalGet) throw new Error('Missing API fixture')
    const scope = {
      ...offering.channel_scope,
      include_provider_ids: [3],
      include_channel_ids: [11],
    }
    const error = new AxiosError('Request failed with status code 403')
    error.response = {
      data: { success: false, message: 'Forbidden' },
      status: 403,
    } as typeof error.response
    get.mockImplementation((url, config) => {
      if (url === '/api/admin/v1/model-offerings/9') {
        return Promise.resolve({
          data: { success: true, data: { ...offering, channel_scope: scope } },
        })
      }
      if (forbidden.some((resource) => url === `/api/admin/v1/${resource}`)) {
        return Promise.reject(error)
      }
      return originalGet(url, config)
    })
    const put = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    renderPage(10, true)
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Edit' }))
    const name = await screen.findByLabelText('Primary name')
    for (const label of disabled) {
      expect(screen.getByRole('combobox', { name: label })).toBeDisabled()
    }
    expect(screen.getByRole('combobox', { name: enabled })).toBeEnabled()
    expect(
      screen.getByText(
        'Some channel scope choices are unavailable. Existing selections are preserved.'
      )
    ).toBeVisible()
    await user.clear(name)
    await user.type(name, 'renamed')
    await user.click(
      screen.getByRole('button', { name: 'Save model offering' })
    )
    await waitFor(() =>
      expect(put).toHaveBeenCalledWith(
        '/api/admin/v1/model-offerings/9',
        expect.objectContaining({ name: 'renamed', channel_scope: scope })
      )
    )
  }
)

it('offers a retry without a writable form when editor choices fail to load', async () => {
  const get = mockManagementApi()
  renderPage()
  const user = userEvent.setup()
  await screen.findByText('assistant')
  get.mockRejectedValueOnce(new Error('Failed to load'))
  await user.click(screen.getByRole('button', { name: 'New model offering' }))
  expect(await screen.findByRole('button', { name: 'Retry' })).toBeVisible()
  expect(
    screen.queryByRole('button', { name: 'Save model offering' })
  ).not.toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Retry' }))
  expect(await screen.findByLabelText('Primary name')).toBeEnabled()
})
