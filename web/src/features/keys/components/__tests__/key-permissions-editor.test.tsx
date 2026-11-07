import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import { createInstance } from 'i18next'
import { I18nextProvider, initReactI18next } from 'react-i18next'
import { afterEach, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'

import { KeyPermissionsEditor } from '../key-permissions-editor'

const i18n = createInstance()
await i18n
  .use(initReactI18next)
  .init({ lng: 'en', resources: { en: { translation: {} } } })
let client: QueryClient

afterEach(() => {
  client?.clear()
  vi.restoreAllMocks()
})

function renderEditor() {
  client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  return render(
    <QueryClientProvider client={client}>
      <I18nextProvider i18n={i18n}>
        <KeyPermissionsEditor tokenId={7} onSaved={() => undefined} />
      </I18nextProvider>
    </QueryClientProvider>
  )
}

test('allows all only after confirmation and clears exceptions while retaining the loaded revision', async () => {
  vi.spyOn(api, 'get').mockImplementation(async (url) => ({
    data: {
      success: true,
      data: url.endsWith('/permissions')
        ? { default_allow: false, exception_ids: [42], revision: 3 }
        : [{ id: 42, name: 'public-offering' }],
    },
  }))
  const put = vi.spyOn(api, 'put').mockResolvedValue({
    data: {
      success: true,
      data: { default_allow: true, exception_ids: [], revision: 4 },
    },
  })
  renderEditor()
  const policy = await screen.findByRole('combobox', { name: 'Default policy' })
  expect(policy).toHaveValue('deny')
  expect(
    screen.getByText(
      'Newly listed Offerings are denied. Only visible exceptions are allowed.'
    )
  ).toBeVisible()
  fireEvent.click(screen.getByRole('button', { name: 'Allow all' }))
  expect(policy).toHaveValue('allow')
  fireEvent.click(screen.getByRole('button', { name: 'Save permissions' }))
  const dialog = await screen.findByRole('alertdialog')
  expect(put).not.toHaveBeenCalled()
  fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }))
  expect(put).not.toHaveBeenCalled()
  fireEvent.click(screen.getByRole('button', { name: 'Save permissions' }))
  fireEvent.click(
    within(await screen.findByRole('alertdialog')).getByRole('button', {
      name: 'Save permissions',
    })
  )
  await waitFor(() =>
    expect(put).toHaveBeenCalledWith('/api/token/7/permissions', {
      default_allow: true,
      exception_ids: [],
      revision: 3,
    })
  )
})

test('retains hidden exception IDs while changing the default strategy', async () => {
  vi.spyOn(api, 'get').mockImplementation(async (url) => ({
    data: {
      success: true,
      data: url.endsWith('/permissions')
        ? { default_allow: true, exception_ids: [99], revision: 8 }
        : [],
    },
  }))
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: true } })
  renderEditor()
  const policy = await screen.findByRole('combobox', { name: 'Default policy' })
  expect(screen.getByText('Unavailable Offering #99')).toBeVisible()
  fireEvent.change(policy, { target: { value: 'deny' } })
  fireEvent.click(screen.getByRole('button', { name: 'Save permissions' }))
  fireEvent.click(
    within(await screen.findByRole('alertdialog')).getByRole('button', {
      name: 'Save permissions',
    })
  )
  await waitFor(() =>
    expect(put).toHaveBeenCalledWith('/api/token/7/permissions', {
      default_allow: false,
      exception_ids: [99],
      revision: 8,
    })
  )
})

test('reload discards unsaved edits even when the stored revision is unchanged', async () => {
  vi.spyOn(api, 'get').mockImplementation(async (url) => ({
    data: {
      success: true,
      data: url.endsWith('/permissions')
        ? { default_allow: true, exception_ids: [], revision: 1 }
        : [],
    },
  }))
  renderEditor()
  const policy = await screen.findByRole('combobox', { name: 'Default policy' })
  fireEvent.change(policy, { target: { value: 'deny' } })
  expect(policy).toHaveValue('deny')
  fireEvent.click(screen.getByRole('button', { name: 'Reload permissions' }))
  await waitFor(() =>
    expect(
      screen.getByRole('combobox', { name: 'Default policy' })
    ).toHaveValue('allow')
  )
  expect(
    screen.getByRole('button', { name: 'Save permissions' })
  ).toBeDisabled()
})

test('failed reads show retry and never expose a save action', async () => {
  vi.spyOn(api, 'get').mockRejectedValue(new Error('network unavailable'))
  renderEditor()
  expect(await screen.findByRole('button', { name: 'Retry' })).toBeVisible()
  expect(
    screen.queryByRole('button', { name: 'Save permissions' })
  ).not.toBeInTheDocument()
})
