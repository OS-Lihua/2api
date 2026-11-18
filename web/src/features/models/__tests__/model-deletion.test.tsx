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
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { ModelDeleteDialog } from '../components/dialogs/model-delete-dialog'

const models = [
  { id: 7, model_name: 'example-model', name_rule: 0 },
  { id: 8, model_name: 'another-model', name_rule: 0 },
]
function Fixture(props: { batch?: boolean; onSuccess?: () => void }) {
  const [open, setOpen] = useState(true)
  return (
    <>
      <button type='button' onClick={() => setOpen(true)}>
        Open deletion
      </button>
      {open && (
        <ModelDeleteDialog
          models={props.batch ? models : models.slice(0, 1)}
          onClose={() => setOpen(false)}
          onSuccess={props.onSuccess}
        />
      )}
    </>
  )
}
function mount(batch = false) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  const onSuccess = vi.fn()
  const invalidate = vi.spyOn(client, 'invalidateQueries')
  render(
    <QueryClientProvider client={client}>
      <Fixture batch={batch} onSuccess={onSuccess} />
    </QueryClientProvider>
  )
  return { onSuccess, invalidate }
}
afterEach(() => {
  cleanup()
  useAuthStore.getState().auth.reset()
})

describe('model deletion', () => {
  it('keeps the complete long Model name readable while confirming metadata-only deletion', async () => {
    const name = `legal-${'W'.repeat(250)}`
    const remove = vi.spyOn(api, 'delete').mockResolvedValue({
      data: { success: true, data: { deleted_count: 1 } },
    })
    const onClose = vi.fn()
    const client = new QueryClient({
      defaultOptions: { mutations: { retry: false } },
    })
    render(
      <QueryClientProvider client={client}>
        <ModelDeleteDialog
          models={[{ id: 7, model_name: name, name_rule: 0 }]}
          onClose={onClose}
        />
      </QueryClientProvider>
    )
    const description = screen.getByText(`Delete model "${name}"?`)
    expect(description).toBeVisible()
    expect(description).toHaveClass('wrap-anywhere')
    expect(description).not.toHaveClass('truncate', 'line-clamp-1')
    await userEvent.click(screen.getByRole('button', { name: 'Delete' }))
    await waitFor(() => expect(onClose).toHaveBeenCalledOnce())
    expect(remove).toHaveBeenCalledWith('/api/models/7')
  })

  it('keeps Channels independent without a cascade option after cancelling and reopening', async () => {
    const remove = vi.spyOn(api, 'delete').mockResolvedValue({
      data: {
        success: true,
        data: { deleted_count: 1, updated_channels: 0 },
      },
    })
    const { onSuccess, invalidate } = mount()
    const user = userEvent.setup()
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(remove).not.toHaveBeenCalled()
    await user.click(screen.getByRole('button', { name: 'Open deletion' }))
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Delete' }))
    await waitFor(() => expect(onSuccess).toHaveBeenCalledOnce())
    expect(remove).toHaveBeenCalledWith('/api/models/7')
    expect(invalidate).not.toHaveBeenCalledWith({ queryKey: ['channels'] })
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
  })

  it('preserves selected Model IDs after a reference rejection and retries without cascade flags', async () => {
    const post = vi
      .spyOn(api, 'post')
      .mockResolvedValueOnce({
        data: {
          success: false,
          message: 'Model is referenced by a Channel or Offering',
        },
      })
      .mockResolvedValue({
        data: {
          success: true,
          data: { deleted_count: 2, updated_channels: 3 },
        },
      })
    const { onSuccess, invalidate } = mount(true)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Delete' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Model is referenced by a Channel or Offering'
    )
    expect(onSuccess).not.toHaveBeenCalled()
    expect(screen.getByRole('alertdialog')).toHaveTextContent(
      'Delete 2 models?'
    )
    expect(post).toHaveBeenCalledWith('/api/models/delete', {
      model_ids: [7, 8],
    })
    await user.click(screen.getByRole('button', { name: 'Delete' }))
    await waitFor(() => expect(onSuccess).toHaveBeenCalledOnce())
    expect(post).toHaveBeenLastCalledWith('/api/models/delete', {
      model_ids: [7, 8],
    })
    expect(invalidate).not.toHaveBeenCalledWith({ queryKey: ['channels'] })
  })

  it('disables dismissal and repeated submission while a single removal is pending', async () => {
    let complete!: (response: unknown) => void
    const remove = vi.spyOn(api, 'delete').mockImplementation(
      () =>
        new Promise((resolve) => {
          complete = resolve
        })
    )
    const { onSuccess } = mount()
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Delete' }))
    expect(screen.getByRole('button', { name: 'Delete' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeDisabled()
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()
    await user.keyboard('{Escape}')
    expect(screen.getByRole('alertdialog')).toBeVisible()
    expect(remove).toHaveBeenCalledTimes(1)
    expect(remove).toHaveBeenCalledWith('/api/models/7')
    complete({
      data: { success: true, data: { deleted_count: 1, updated_channels: 2 } },
    })
    await waitFor(() => expect(onSuccess).toHaveBeenCalledOnce())
  })
})

it('keeps Offering prices independent when a super administrator deletes Model metadata', async () => {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'root', role: 100 })
  const post = vi.spyOn(api, 'post').mockResolvedValue({
    data: { success: true, data: { deleted_count: 2, updated_channels: 0 } },
  })
  const { onSuccess, invalidate } = mount(true)
  const user = userEvent.setup()
  expect(
    screen.queryByRole('checkbox', { name: 'Also remove pricing' })
  ).not.toBeInTheDocument()
  expect(
    screen.queryByText(/Built-in pricing may become effective again/)
  ).not.toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Delete' }))
  await waitFor(() => expect(onSuccess).toHaveBeenCalledOnce())
  expect(post).toHaveBeenCalledWith('/api/models/delete', {
    model_ids: [7, 8],
  })
  expect(invalidate).not.toHaveBeenCalledWith({
    queryKey: ['model-pricing-config'],
  })
  expect(invalidate).not.toHaveBeenCalledWith({ queryKey: ['channels'] })
})

it('does not offer pricing deletion to an ordinary administrator', () => {
  useAuthStore.getState().auth.setUser({ id: 2, username: 'admin', role: 10 })
  mount()
  expect(
    screen.queryByRole('checkbox', { name: 'Also remove pricing' })
  ).not.toBeInTheDocument()
  expect(screen.getByRole('button', { name: 'Delete' })).toBeEnabled()
})

it.each([1, 2, 3])(
  'deletes only stable metadata IDs when selection includes matching rule %i',
  async (rule) => {
    const post = vi.spyOn(api, 'post').mockResolvedValue({
      data: {
        success: true,
        data: { deleted_count: 2, updated_channels: 0 },
      },
    })
    const client = new QueryClient({
      defaultOptions: { mutations: { retry: false } },
    })
    render(
      <QueryClientProvider client={client}>
        <ModelDeleteDialog
          models={[models[1], { ...models[0], name_rule: rule }]}
          onClose={() => {}}
        />
      </QueryClientProvider>
    )
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()
    expect(
      screen.getByText(
        'Channels and Offerings are not deleted with Model metadata. Referenced Models cannot be deleted.'
      )
    ).toBeVisible()
    await userEvent
      .setup()
      .click(screen.getByRole('button', { name: 'Delete' }))
    await waitFor(() =>
      expect(post).toHaveBeenCalledWith('/api/models/delete', {
        model_ids: [8, 7],
      })
    )
  }
)
