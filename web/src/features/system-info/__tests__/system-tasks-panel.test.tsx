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
  cleanup,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { Toaster } from 'sonner'
import { afterEach, expect, it, vi } from 'vitest'

import en from '@/i18n/locales/en.json'
import fr from '@/i18n/locales/fr.json'
import ja from '@/i18n/locales/ja.json'
import ru from '@/i18n/locales/ru.json'
import viLocale from '@/i18n/locales/vi.json'
import zhTW from '@/i18n/locales/zh-TW.json'
import zh from '@/i18n/locales/zh.json'
import { api, type ApiRequestConfig } from '@/lib/api'

import { SystemTasksPanel } from '../components/system-tasks-panel'

const task = {
  id: 1,
  task_id: 'history-task',
  type: 'model_update',
  status: 'succeeded',
  created_at: 100,
  updated_at: 200,
  locked_by: 'history-runner',
}

function renderPanel() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  render(
    <QueryClientProvider client={client}>
      <SystemTasksPanel />
      <Toaster />
    </QueryClientProvider>
  )
  return client
}

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

it.each([
  {
    kind: 'Channel',
    type: 'model_update',
    retirement:
      'Legacy Channel management has been retired. Use the Provider and Channel APIs instead.',
    message: '旧通道管理已退役，请改用 Provider 和 Channel API。',
  },
  {
    kind: 'Midjourney',
    type: 'midjourney_poll',
    retirement:
      'Legacy Midjourney polling has been retired. Migrate historical tasks before starting the server.',
    message: '旧版 Midjourney 轮询已退役。请先迁移历史任务，再启动服务器。',
  },
  {
    kind: 'balance reminder',
    type: 'balance_reminder',
    retirement: 'Balance reminder delivery could not complete.',
    message: '余额提醒发送未能完成。',
  },
])(
  'translates the fixed $kind retirement error in the cell and title after language changes but preserves unknown historical errors',
  async ({ type, retirement, message }) => {
    const historical = `${retirement} Previous task {{id}} <unknown>`
    const translated = createInstance()
    await translated.init({
      lng: 'en',
      resources: { en, zh },
      initAsync: false,
    })
    vi.spyOn(api, 'get').mockImplementation(
      async (_url, config?: ApiRequestConfig) => ({
        data: {
          success: true,
          data:
            config?.params?.scope === 'active'
              ? []
              : [
                  { ...task, type, status: 'failed', error: retirement },
                  {
                    ...task,
                    task_id: 'unknown',
                    status: 'failed',
                    error: historical,
                  },
                ],
          total: config?.params?.scope === 'active' ? 0 : 2,
        },
      })
    )
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    render(
      <I18nextProvider i18n={translated}>
        <QueryClientProvider client={client}>
          <SystemTasksPanel />
        </QueryClientProvider>
      </I18nextProvider>
    )
    expect(await screen.findByTitle(retirement)).toHaveTextContent(retirement)
    expect(screen.getByRole('cell', { name: retirement })).toHaveAttribute(
      'title',
      retirement
    )
    expect(screen.getByTitle(historical)).toHaveTextContent(historical)
    await act(async () => translated.changeLanguage('zh'))
    expect(screen.getByTitle(message)).toHaveTextContent(message)
    expect(screen.getByRole('cell', { name: message })).toHaveAttribute(
      'title',
      message
    )
    expect(screen.getByTitle(historical)).toHaveTextContent(historical)
    await act(async () => translated.changeLanguage('en'))
    expect(screen.getByRole('cell', { name: retirement })).toHaveAttribute(
      'title',
      retirement
    )
    expect(screen.getByTitle(historical)).toHaveTextContent(historical)
    client.clear()
  }
)

it('localizes balance reminder task labels and fixed errors in seven languages without translating unknown text or inventing delivery progress', async () => {
  const error = 'Balance reminder delivery could not complete.'
  const unknown = `${error} Gateway {{id}} <unknown>`
  const translated = createInstance()
  await translated.init({
    lng: 'en',
    resources: { en, zh, 'zh-TW': zhTW, fr, ru, ja, vi: viLocale },
    initAsync: false,
  })
  const expected = [
    ['en', 'Balance reminders', error],
    ['zh', '余额提醒', '余额提醒发送未能完成。'],
    ['zh-TW', '餘額提醒', '餘額提醒傳送未能完成。'],
    [
      'fr',
      'Rappels de solde',
      'L’envoi du rappel de solde n’a pas pu être terminé.',
    ],
    [
      'ru',
      'Уведомления о балансе',
      'Не удалось завершить отправку уведомления о балансе.',
    ],
    ['ja', '残高通知', '残高通知の送信を完了できませんでした。'],
    ['vi', 'Nhắc nhở số dư', 'Không thể hoàn tất việc gửi nhắc nhở số dư.'],
  ] as const
  for (const [language] of expected) {
    translated.addResource(
      language,
      'translation',
      unknown,
      'This unknown message must remain raw'
    )
  }
  vi.spyOn(api, 'get').mockImplementation(
    async (_url, config?: ApiRequestConfig) => ({
      data: {
        success: true,
        data:
          config?.params?.scope === 'active'
            ? []
            : [
                {
                  ...task,
                  type: 'balance_reminder',
                  status: 'failed',
                  error,
                  state: { claimed: 4, accepted: 1, retry: 2, skipped: 1 },
                },
                {
                  ...task,
                  task_id: 'unknown',
                  type: 'future_task',
                  status: 'failed',
                  error: unknown,
                },
              ],
        total: config?.params?.scope === 'active' ? 0 : 2,
      },
    })
  )
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  render(
    <I18nextProvider i18n={translated}>
      <QueryClientProvider client={client}>
        <SystemTasksPanel />
      </QueryClientProvider>
    </I18nextProvider>
  )
  await screen.findByTitle(error)
  for (const [language, label, message] of expected) {
    await act(async () => translated.changeLanguage(language))
    expect(screen.getByRole('option', { name: label })).toHaveValue(
      'balance_reminder'
    )
    const cell = screen.getByRole('cell', { name: `${label} balance_reminder` })
    expect(cell).toBeVisible()
    const row = cell.closest('tr')
    if (!row) throw new Error('Missing reminder task row')
    expect(within(row).getByRole('cell', { name: message })).toHaveAttribute(
      'title',
      message
    )
    expect(within(row).getByText('-')).toBeVisible()
    expect(within(row).queryByText('25%')).not.toBeInTheDocument()
    expect(within(row).queryByText('100%')).not.toBeInTheDocument()
    expect(screen.getByTitle(unknown)).toHaveTextContent(unknown)
    expect(
      screen.queryByText('This unknown message must remain raw')
    ).not.toBeInTheDocument()
  }
  client.clear()
})

it('filters balance reminder history by the stable task type rather than the translated label', async () => {
  const get = vi
    .spyOn(api, 'get')
    .mockImplementation(async (_url, config?: ApiRequestConfig) => ({
      data: {
        success: true,
        data:
          config?.params?.scope === 'active'
            ? []
            : [{ ...task, type: 'balance_reminder' }],
        total: config?.params?.scope === 'active' ? 0 : 1,
      },
    }))
  const client = renderPanel()
  await userEvent.selectOptions(
    await screen.findByRole('combobox', { name: 'Type' }),
    'balance_reminder'
  )
  await waitFor(() =>
    expect(get).toHaveBeenCalledWith(
      '/api/system-task/list',
      expect.objectContaining({
        params: expect.objectContaining({
          scope: 'history',
          type: 'balance_reminder',
          offset: 0,
        }),
      })
    )
  )
  expect(
    screen.getByRole('cell', { name: 'Balance reminders balance_reminder' })
  ).toBeVisible()
  client.clear()
})

it('filters history on the server and resets pagination without hiding active tasks', async () => {
  const get = vi
    .spyOn(api, 'get')
    .mockImplementation(async (_url, config?: ApiRequestConfig) => {
      if (config?.params?.scope === 'active') {
        return {
          data: {
            success: true,
            data: [
              {
                ...task,
                task_id: 'active',
                status: 'running',
                locked_by: 'active-runner',
              },
            ],
            total: 1,
          },
        }
      }
      return { data: { success: true, data: [task], total: 21 } }
    })
  const client = renderPanel()
  await screen.findByRole('combobox', { name: 'Type' })
  await userEvent.click(
    await screen.findByRole('button', { name: 'Go to next page' })
  )
  await waitFor(() =>
    expect(get).toHaveBeenCalledWith(
      '/api/system-task/list',
      expect.objectContaining({
        params: expect.objectContaining({ scope: 'history', offset: 20 }),
      })
    )
  )
  await userEvent.selectOptions(
    screen.getByRole('combobox', { name: 'Type' }),
    'model_update'
  )
  await userEvent.selectOptions(
    screen.getByRole('combobox', { name: 'Status' }),
    'failed'
  )
  await waitFor(() =>
    expect(get).toHaveBeenLastCalledWith(
      '/api/system-task/list',
      expect.objectContaining({
        params: expect.objectContaining({
          scope: 'history',
          type: 'model_update',
          status: 'failed',
          offset: 0,
        }),
      })
    )
  )
  expect(screen.getByText('active-runner')).toBeVisible()
  client.clear()
})

it('keeps filters available and disables cleanup when history is empty', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: [], total: 0 },
  })
  const client = renderPanel()
  expect(await screen.findByText('No historical system tasks.')).toBeVisible()
  expect(screen.getByRole('combobox', { name: 'Type' })).toBeEnabled()
  expect(
    screen.getByRole('button', { name: 'Clean task history' })
  ).toBeDisabled()
  client.clear()
})

it('requires confirmation and cleans all matching history pages using the selected filters', async () => {
  vi.spyOn(api, 'get').mockImplementation(
    async (_url, config?: ApiRequestConfig) => ({
      data: {
        success: true,
        data: config?.params?.scope === 'active' ? [] : [task],
        total: 21,
      },
    })
  )
  const remove = vi
    .spyOn(api, 'delete')
    .mockResolvedValue({ data: { success: true, data: { deleted_count: 20 } } })
  const client = renderPanel()
  await userEvent.selectOptions(
    await screen.findByRole('combobox', { name: 'Type' }),
    'model_update'
  )
  await waitFor(() =>
    expect(
      screen.getByRole('button', { name: 'Clean task history' })
    ).toBeEnabled()
  )
  await userEvent.click(
    screen.getByRole('button', { name: 'Clean task history' })
  )
  const dialog = screen.getByRole('alertdialog')
  expect(within(dialog).getByText(/latest run of each task type/)).toBeVisible()
  expect(remove).not.toHaveBeenCalled()
  await userEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }))
  expect(remove).not.toHaveBeenCalled()
  await userEvent.click(
    screen.getByRole('button', { name: 'Clean task history' })
  )
  await userEvent.click(
    within(screen.getByRole('alertdialog')).getByRole('button', {
      name: 'Delete',
    })
  )
  await waitFor(() =>
    expect(remove).toHaveBeenCalledWith('/api/system-task/history', {
      params: { type: 'model_update', status: '' },
    })
  )
  await waitFor(() =>
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
  )
  client.clear()
})

it('retains the confirmation and shows the server error when cleanup fails', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: [task], total: 1 },
  })
  let finishCleanup!: (response: {
    data: { success: boolean; message: string }
  }) => void
  const response = new Promise<{ data: { success: boolean; message: string } }>(
    (resolve) => {
      finishCleanup = resolve
    }
  )
  vi.spyOn(api, 'delete').mockReturnValue(response)
  const client = renderPanel()
  await waitFor(() =>
    expect(
      screen.getByRole('button', { name: 'Clean task history' })
    ).toBeEnabled()
  )
  await userEvent.click(
    screen.getByRole('button', { name: 'Clean task history' })
  )
  const dialog = screen.getByRole('alertdialog')
  await userEvent.click(within(dialog).getByRole('button', { name: 'Delete' }))
  expect(
    within(dialog).getByRole('button', { name: 'Deleting...' })
  ).toBeDisabled()
  expect(within(dialog).getByRole('button', { name: 'Cancel' })).toBeDisabled()
  finishCleanup({
    data: { success: false, message: 'History cleanup unavailable' },
  })
  expect(await screen.findByText('History cleanup unavailable')).toBeVisible()
  expect(within(dialog).getByRole('button', { name: 'Delete' })).toBeEnabled()
  client.clear()
})

it('shows history query failures without hiding active tasks and allows retry', async () => {
  let historyFailed = true
  vi.spyOn(api, 'get').mockImplementation(
    async (_url, config?: ApiRequestConfig) => {
      if (config?.params?.scope === 'history' && historyFailed) {
        return { data: { success: false, message: 'History unavailable' } }
      }
      return {
        data: {
          success: true,
          data: [
            {
              ...task,
              locked_by:
                config?.params?.scope === 'active'
                  ? 'active-runner'
                  : 'history-runner',
            },
          ],
          total: 1,
        },
      }
    }
  )
  const client = renderPanel()
  expect(await screen.findByText('History unavailable')).toBeVisible()
  expect(screen.getByText('active-runner')).toBeVisible()
  expect(
    screen.getByRole('button', { name: 'Clean task history' })
  ).toBeDisabled()
  historyFailed = false
  await userEvent.click(
    screen.getByRole('button', { name: /retry|try again/i })
  )
  expect(await screen.findByText('history-runner')).toBeVisible()
  client.clear()
})

it('refreshes history when the last running task finishes', async () => {
  let finished = false
  vi.spyOn(api, 'get').mockImplementation(
    async (_url, config?: ApiRequestConfig) => {
      const active = config?.params?.scope === 'active'
      const data =
        active === finished
          ? []
          : [{ ...task, status: finished ? 'succeeded' : 'running' }]
      return { data: { success: true, data, total: data.length } }
    }
  )
  const client = renderPanel()
  await screen.findByText('No historical system tasks.')
  finished = true
  await userEvent.click(screen.getByRole('button', { name: 'Refresh' }))
  expect(await screen.findByText('No active system tasks.')).toBeVisible()
  await waitFor(() =>
    expect(
      screen.queryByText('No historical system tasks.')
    ).not.toBeInTheDocument()
  )
  expect(screen.getByText('history-runner')).toBeVisible()
  client.clear()
})
