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
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import en from '@/i18n/locales/en.json'
import fr from '@/i18n/locales/fr.json'
import ja from '@/i18n/locales/ja.json'
import ru from '@/i18n/locales/ru.json'
import viLocale from '@/i18n/locales/vi.json'
import zhTW from '@/i18n/locales/zh-TW.json'
import zh from '@/i18n/locales/zh.json'
import { api } from '@/lib/api'

import { UploadDialog } from '../components/upload-dialog'
import { MAX_PLUGIN_SOURCE_BYTES } from '../lib/plugin-url'
import type { TaskPluginPricingIssue, TaskPluginDetail } from '../types'

const uploadResponse = vi.fn()
const activationResponse = vi.fn()
beforeEach(() => {
  uploadResponse.mockReset()
  activationResponse.mockReset()
  vi.spyOn(api, 'post').mockImplementation(async (url, payload) => {
    const handler =
      url === '/api/plugin/task' ? uploadResponse : activationResponse
    if (
      url !== '/api/plugin/task' &&
      url !== '/api/plugin/task/demo/activate'
    ) {
      throw new Error(`Unexpected POST ${url}`)
    }
    return { data: { success: true, data: await handler(payload) } }
  })
})

const queryClients: QueryClient[] = []

function renderDialog(open = true, initialKey?: string) {
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
      mutations: { retry: false },
    },
  })
  queryClients.push(queryClient)
  const onOpenChange = vi.fn()
  const view = render(
    <QueryClientProvider client={queryClient}>
      <UploadDialog
        open={open}
        onOpenChange={onOpenChange}
        initialKey={initialKey}
      />
    </QueryClientProvider>
  )
  return { onOpenChange, queryClient, view }
}

function sourceEditor() {
  return screen.getByRole('textbox', { name: 'Plugin source' })
}

function fileInput() {
  return screen.getByLabelText('JavaScript file') as HTMLInputElement
}

/** The dialog chrome also renders an sr-only "Close", so scope to the footer. */
function footerButton(name: string | RegExp) {
  const footer = document.querySelector('[data-slot=dialog-footer]')
  return within(footer as HTMLElement).getByRole('button', { name })
}

function savedPlugin(active: boolean): TaskPluginDetail {
  return {
    meta: {
      key: 'demo',
      name: 'Demo',
      version: '2.0.0',
      apiVersion: 1,
      author: { name: 'Demo' },
      models: [],
      fetchMode: 'per_task',
    },
    pricing_diagnostics: { state: 'ready', issues: [] },
    source: 'const a = 1',
    layer: 'override',
    plugin: {
      id: 2,
      key: 'demo',
      api_version: 1,
      version: '2.0.0',
      source: 'const a = 1',
      source_hash: 'hash',
      enabled: true,
      active,
      created_at: 1,
      remark: '',
    },
  }
}

async function uploadSource(user: ReturnType<typeof userEvent.setup>) {
  await user.upload(
    fileInput(),
    new File(['const a = 1'], 'plugin.js', { type: 'text/javascript' })
  )
  await waitFor(() => expect(footerButton('Upload')).toBeEnabled())
  await user.click(footerButton('Upload'))
}

afterEach(() => {
  for (const queryClient of queryClients) queryClient.clear()
  queryClients.length = 0
  vi.unstubAllGlobals()
})

describe('UploadDialog layout', () => {
  test('renders the source editor and hides the native file input from view', () => {
    renderDialog()

    expect(sourceEditor()).toBeInTheDocument()
    // The unstyled native picker is the element that made this dialog look
    // foreign; it must stay in the DOM (labelled) but never be the visible
    // control.
    expect(fileInput()).toHaveClass('sr-only')
    expect(
      screen.getByRole('button', { name: 'Choose file' })
    ).toBeInTheDocument()
    expect(screen.getByText('0 bytes')).toBeInTheDocument()
  })

  test('scrolls its body instead of growing past the viewport', () => {
    renderDialog()

    const content = document.querySelector('[data-slot=dialog-content]')
    const body = content?.querySelector(':scope > div:nth-child(2)')
    expect(content).toHaveClass(
      'max-h-[var(--dialog-available-height,calc(100dvh-2rem))]'
    )
    expect(body).toHaveClass('overflow-y-auto')
  })

  test('does not steal focus into the mid-dialog source editor on open', () => {
    renderDialog()

    expect(document.querySelector('.cm-content')).not.toHaveFocus()
  })
})

describe('UploadDialog file selection', () => {
  test('fills the source editor and names the chosen file', async () => {
    const user = userEvent.setup()
    renderDialog()

    await user.upload(
      fileInput(),
      new File(['export const meta = {}'], 'plugin.js', {
        type: 'text/javascript',
      })
    )

    expect(await screen.findByText('plugin.js')).toBeInTheDocument()
    await waitFor(() =>
      expect(document.querySelector('.cm-content')?.textContent).toContain(
        'export const meta = {}'
      )
    )
    expect(
      screen.getByRole('button', { name: 'Choose another file' })
    ).toBeInTheDocument()
  })

  test('rejects a file over the 8 MiB limit without touching the source', async () => {
    const user = userEvent.setup()
    renderDialog()

    const oversized = new File(['x'], 'huge.js', { type: 'text/javascript' })
    Object.defineProperty(oversized, 'size', {
      value: MAX_PLUGIN_SOURCE_BYTES + 1,
    })
    await user.upload(fileInput(), oversized)

    expect(
      await screen.findByText('Plugin source exceeds the 8 MiB limit.')
    ).toBeInTheDocument()
    expect(screen.queryByText('huge.js')).toBeNull()
    expect(footerButton('Upload')).toBeDisabled()
  })
})

describe('UploadDialog declaration preview', () => {
  async function selectSource(
    user: ReturnType<typeof userEvent.setup>,
    source: string
  ) {
    await user.upload(
      fileInput(),
      new File([source], 'plugin.js', { type: 'text/javascript' })
    )
    return screen.findByRole('region', { name: 'Connection settings' })
  }

  test('lists the request domains the selected plugin declares', async () => {
    const user = userEvent.setup()
    renderDialog()

    const connections = await selectSource(
      user,
      'export const meta = {models: ["m"], allowedHosts: ["files.example.com", "cdn.example.com:8443"]};'
    )

    expect(
      within(connections).getByText('files.example.com, cdn.example.com:8443')
    ).toBeVisible()
    expect(
      within(connections).queryByText('From marketplace index')
    ).not.toBeInTheDocument()
  })

  test('says no extra domains are declared when the plugin lists none', async () => {
    const user = userEvent.setup()
    renderDialog()

    const connections = await selectSource(
      user,
      'export const meta = {models: ["m"]};'
    )

    expect(
      within(connections).getByText('No extra domains declared')
    ).toBeVisible()
  })
})

describe('UploadDialog URL import', () => {
  test('keeps Fetch disabled until a URL is typed, then fills the source', async () => {
    const user = userEvent.setup()
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => new Response('const fetched = 1', { status: 200 }))
    )
    renderDialog()

    expect(screen.getByRole('button', { name: 'Fetch' })).toBeDisabled()

    await user.type(
      screen.getByLabelText('Import from URL'),
      'https://example.com/plugin.js'
    )
    expect(screen.getByRole('button', { name: 'Fetch' })).toBeEnabled()
    await user.click(screen.getByRole('button', { name: 'Fetch' }))

    await waitFor(() =>
      expect(document.querySelector('.cm-content')?.textContent).toContain(
        'const fetched = 1'
      )
    )
    // Fetching must never upload on its own.
    expect(uploadResponse).not.toHaveBeenCalled()
  })

  test('fetches on Enter and marks the field invalid when the URL is not absolute', async () => {
    const user = userEvent.setup()
    renderDialog()

    const urlField = screen.getByLabelText('Import from URL')
    await user.type(urlField, 'plugin.js{Enter}')

    expect(
      await screen.findByText('Enter an absolute http(s) URL.')
    ).toBeInTheDocument()
    expect(urlField).toHaveAttribute('aria-invalid', 'true')
    expect(document.querySelector('[data-slot=field-error]')).toHaveAttribute(
      'role',
      'alert'
    )
  })
})

describe('UploadDialog upload lifecycle', () => {
  test.each([undefined, 'demo'])(
    'offers activation after saving an inactive version from %s',
    async (initialKey) => {
      const user = userEvent.setup()
      uploadResponse.mockResolvedValue(savedPlugin(false))
      let resolveActivation!: () => void
      activationResponse.mockImplementation(
        () =>
          new Promise<void>((resolve) => {
            resolveActivation = resolve
          })
      )
      const { queryClient, onOpenChange } = renderDialog(true, initialKey)
      queryClient.setQueryData(['task-plugin', 'demo'], { cached: true })
      queryClient.setQueryData(['task-plugin-versions', 'demo'], [])
      queryClient.setQueryData(['task-plugins'], [])

      await uploadSource(user)

      expect(
        await screen.findByText('Plugin saved, activation required')
      ).toBeVisible()
      expect(
        screen.getByText(
          'This version is saved but not active. Activate it to replace the current version, or activate it later from the Versions tab.'
        )
      ).toBeVisible()
      expect(
        screen.queryByRole('textbox', { name: 'Plugin source' })
      ).not.toBeInTheDocument()
      expect(activationResponse).not.toHaveBeenCalled()
      expect(footerButton('Activate now')).toHaveFocus()
      // Reset the cached queries so activation must invalidate them again.
      queryClient.setQueryData(['task-plugin', 'demo'], { cached: true })
      queryClient.setQueryData(['task-plugin-versions', 'demo'], [])
      queryClient.setQueryData(['task-plugins'], [])
      await user.keyboard('{Enter}')

      expect(api.post).toHaveBeenCalledWith(
        '/api/plugin/task/demo/activate',
        { version: '2.0.0' },
        expect.any(Object)
      )
      expect(footerButton('Activating...')).toBeDisabled()
      expect(footerButton('Later')).toBeDisabled()
      await user.keyboard('{Escape}')
      expect(onOpenChange).not.toHaveBeenCalled()
      resolveActivation()

      expect(await screen.findByText('Plugin version activated')).toBeVisible()
      expect(
        screen.queryByRole('button', { name: 'Activate now' })
      ).not.toBeInTheDocument()
      expect(footerButton('Close')).toHaveFocus()
      for (const queryKey of [
        ['task-plugins'],
        ['task-plugin', 'demo'],
        ['task-plugin-versions', 'demo'],
      ]) {
        expect(queryClient.getQueryState(queryKey)?.isInvalidated).toBe(true)
      }
    }
  )

  test('keeps the saved version available to retry when activation fails', async () => {
    const user = userEvent.setup()
    uploadResponse.mockResolvedValue(savedPlugin(false))
    activationResponse
      .mockRejectedValueOnce(new Error('activation failed'))
      .mockResolvedValueOnce(undefined)
    renderDialog()
    await uploadSource(user)
    await user.click(
      await screen.findByRole('button', { name: 'Activate now' })
    )

    expect(await screen.findByText('activation failed')).toBeVisible()
    expect(screen.getByText('Plugin saved, activation required')).toBeVisible()
    expect(footerButton('Activate now')).toBeEnabled()
    await user.click(footerButton('Activate now'))
    expect(await screen.findByText('Plugin version activated')).toBeVisible()
    expect(uploadResponse).toHaveBeenCalledTimes(1)
  })

  test('allows postponing activation without changing the active version', async () => {
    const user = userEvent.setup()
    uploadResponse.mockResolvedValue(savedPlugin(false))
    const { onOpenChange } = renderDialog()
    await uploadSource(user)
    await user.click(await screen.findByRole('button', { name: 'Later' }))
    expect(onOpenChange).toHaveBeenCalledWith(false)
    expect(activationResponse).not.toHaveBeenCalled()
  })

  test('shows an already active upload as activated without another activation request', async () => {
    const user = userEvent.setup()
    uploadResponse.mockResolvedValue(savedPlugin(true))
    renderDialog()
    await uploadSource(user)
    expect(await screen.findByText('Plugin version activated')).toBeVisible()
    expect(
      screen.queryByRole('button', { name: 'Activate now' })
    ).not.toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: /^Upload$/ })
    ).not.toBeInTheDocument()
    expect(activationResponse).not.toHaveBeenCalled()
  })

  test('lists current price owners requiring attention after upload', async () => {
    const user = userEvent.setup()
    const issue: TaskPluginPricingIssue = {
      owner_type: 'model_offering',
      owner_id: 7,
      owner_name: 'demo-video',
      price_version_id: null,
      plugin_key: 'demo',
      plugin_name: 'Demo',
      reason: 'unconfigured',
    }
    uploadResponse.mockResolvedValue({
      ...savedPlugin(true),
      pricing_diagnostics: { state: 'ready', issues: [issue] },
    })
    const { queryClient } = renderDialog()
    queryClient.setQueryData(['task-plugin', 'demo'], [])

    await uploadSource(user)

    expect(
      await screen.findByText('Prices of these owners need attention:')
    ).toBeVisible()
    expect(screen.getByText('Model offering #7 · demo-video')).toBeVisible()
    expect(
      queryClient.getQueryState(['task-plugin', 'demo'])?.isInvalidated
    ).toBe(true)
  })

  test('lists what the activation reports after saving an inactive version', async () => {
    const user = userEvent.setup()
    uploadResponse.mockResolvedValue(savedPlugin(false))
    activationResponse.mockResolvedValue({
      pricing_diagnostics: {
        state: 'ready',
        issues: [
          {
            owner_type: 'channel',
            owner_id: 8,
            owner_name: 'demo-old',
            price_version_id: 13,
            plugin_key: 'demo',
            plugin_name: 'Demo',
            reason: 'stale',
          },
        ],
      },
    })
    renderDialog()
    await uploadSource(user)
    await screen.findByText('Plugin saved, activation required')
    expect(
      screen.queryByText('Prices of these owners need attention:')
    ).not.toBeInTheDocument()

    await user.click(footerButton('Activate now'))

    expect(
      await screen.findByText(
        "The price owner's plugin or model binding is no longer valid."
      )
    ).toBeVisible()
  })

  test('disables Upload while the source is empty and shows a pending label', async () => {
    const user = userEvent.setup()
    let resolveUpload: (value: unknown) => void = () => undefined
    uploadResponse.mockImplementation(
      () => new Promise((resolve) => (resolveUpload = resolve))
    )
    renderDialog()

    expect(footerButton('Upload')).toBeDisabled()

    await user.upload(
      fileInput(),
      new File(['const a = 1'], 'plugin.js', { type: 'text/javascript' })
    )
    const uploadButton = await waitFor(() => {
      const button = footerButton('Upload')
      expect(button).toBeEnabled()
      return button
    })

    await user.click(uploadButton)
    await waitFor(() => expect(footerButton(/Uploading/)).toBeDisabled())

    resolveUpload(savedPlugin(true))
    expect(
      await screen.findByText('Plugin version activated')
    ).toBeInTheDocument()
  })

  test('surfaces an upload rejection verbatim', async () => {
    const user = userEvent.setup()
    uploadResponse.mockRejectedValue(new Error('key conflicts with `demo`'))
    renderDialog()

    await user.upload(
      fileInput(),
      new File(['const a = 1'], 'plugin.js', { type: 'text/javascript' })
    )
    await waitFor(() => expect(footerButton('Upload')).toBeEnabled())
    await user.click(footerButton('Upload'))

    expect(
      await screen.findByText('key conflicts with `demo`')
    ).toBeInTheDocument()
  })

  test('clears every field when the dialog is closed', async () => {
    const user = userEvent.setup()
    const { onOpenChange, queryClient, view } = renderDialog()

    await user.upload(
      fileInput(),
      new File(['const a = 1'], 'plugin.js', { type: 'text/javascript' })
    )
    await user.type(screen.getByLabelText('Import from URL'), 'plugin.js')
    await screen.findByText('plugin.js')

    await user.click(footerButton('Close'))
    expect(onOpenChange).toHaveBeenCalledWith(false)

    view.rerender(
      <QueryClientProvider client={queryClient}>
        <UploadDialog open onOpenChange={onOpenChange} />
      </QueryClientProvider>
    )

    expect(screen.getByLabelText('Import from URL')).toHaveValue('')
    expect(screen.getByText('0 bytes')).toBeInTheDocument()
    expect(
      screen.getByRole('button', { name: 'Choose file' })
    ).toBeInTheDocument()
  })
})

const diagnosticLocales = { en, zh, 'zh-TW': zhTW, fr, ru, ja, vi: viLocale }
test.each(Object.keys(diagnosticLocales))(
  '%s: saved upload diagnostics and read retry use real localized owner messages',
  async (language) => {
    const instance = createInstance()
    await instance.init({
      lng: language,
      fallbackLng: 'en',
      resources: diagnosticLocales,
      interpolation: { escapeValue: false },
    })
    const user = userEvent.setup()
    uploadResponse.mockResolvedValue({
      ...savedPlugin(true),
      pricing_diagnostics: { state: 'unavailable', issues: [] },
    })
    const queryClient = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    })
    queryClients.push(queryClient)
    const read = vi
      .spyOn(api, 'get')
      .mockRejectedValueOnce(new Error('temporarily unavailable'))
      .mockResolvedValueOnce({
        data: {
          success: true,
          data: {
            ...savedPlugin(true),
            pricing_diagnostics: {
              state: 'ready',
              issues: [
                {
                  owner_type: 'model_offering',
                  owner_id: 7,
                  owner_name: 'Same name',
                  price_version_id: null,
                  plugin_key: 'demo',
                  plugin_name: 'Demo',
                  reason: 'unconfigured',
                },
                {
                  owner_type: 'channel',
                  owner_id: 7,
                  owner_name: 'Same name',
                  price_version_id: 33,
                  plugin_key: 'demo',
                  plugin_name: 'Demo',
                  reason: 'incompatible',
                },
                {
                  owner_type: 'channel',
                  owner_id: 8,
                  owner_name: '长合法名称'.repeat(40),
                  price_version_id: 34,
                  plugin_key: 'demo',
                  plugin_name: 'Demo',
                  reason: 'stale',
                },
              ],
            },
          },
        },
      })
    render(
      <I18nextProvider i18n={instance}>
        <QueryClientProvider client={queryClient}>
          <UploadDialog open onOpenChange={() => undefined} />
        </QueryClientProvider>
      </I18nextProvider>
    )
    await user.upload(
      screen.getByLabelText(instance.t('JavaScript file')),
      new File(['const a = 1'], 'plugin.js', { type: 'text/javascript' })
    )
    await waitFor(() =>
      expect(footerButton(instance.t('Upload'))).toBeEnabled()
    )
    await user.click(footerButton(instance.t('Upload')))
    expect(
      await screen.findByText(instance.t('Plugin version activated'))
    ).toBeVisible()
    expect(
      screen.getByText(
        instance.t(
          'Pricing diagnostics are unavailable. The plugin change was saved.'
        )
      )
    ).toBeVisible()
    expect(
      screen.getByText(instance.t('Current PriceVersion diagnostics'))
    ).toBeVisible()
    const retryLabel = instance.t('Retry pricing diagnostics')
    await user.click(screen.getByRole('button', { name: retryLabel }))
    await waitFor(() =>
      expect(screen.getByRole('button', { name: retryLabel })).toBeEnabled()
    )
    expect(
      screen.getByText(instance.t('Plugin version activated'))
    ).toBeVisible()
    await user.click(screen.getByRole('button', { name: retryLabel }))
    expect(
      await screen.findByText(`${instance.t('Model offering')} #7 · Same name`)
    ).toBeVisible()
    expect(
      screen.getByText(`${instance.t('Channel')} #7 · Same name`)
    ).toBeVisible()
    expect(screen.getByText(instance.t('No price set'))).toBeVisible()
    expect(
      screen.getByText(
        instance.t(
          'The current PriceVersion is incompatible with the plugin usage schema.'
        )
      )
    ).toBeVisible()
    expect(
      screen.getByText(
        instance.t(
          "The price owner's plugin or model binding is no longer valid."
        )
      )
    ).toBeVisible()
    expect(
      screen.getByText(`Demo (demo) · ${instance.t('PriceVersion')} #33`)
    ).toBeVisible()
    expect(
      screen
        .getByText(`${instance.t('Channel')} #8 · ${'长合法名称'.repeat(40)}`)
        .closest('li')
    ).toHaveClass('wrap-anywhere')
    expect(uploadResponse).toHaveBeenCalledTimes(1)
    expect(api.post).toHaveBeenCalledTimes(1)
    expect(read).toHaveBeenCalledTimes(2)
    expect(read).toHaveBeenLastCalledWith(
      '/api/plugin/task/demo',
      expect.objectContaining({
        params: undefined,
        signal: expect.any(AbortSignal),
      })
    )
  }
)
