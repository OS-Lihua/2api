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
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import i18next from 'i18next'
import { Toaster, toast } from 'sonner'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { PrivacyCard } from '@/features/security/components/privacy-card'
import en from '@/i18n/locales/en.json'
import fr from '@/i18n/locales/fr.json'
import ja from '@/i18n/locales/ja.json'
import ru from '@/i18n/locales/ru.json'
import viLocale from '@/i18n/locales/vi.json'
import zhTW from '@/i18n/locales/zh-TW.json'
import zh from '@/i18n/locales/zh.json'
import { api } from '@/lib/api'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

import { updateUserSettings } from '../api'
import { NotificationTab } from '../components/tabs/notification-tab'
import type { UserProfile } from '../types'

const profile: UserProfile = {
  id: 1,
  username: 'alice',
  display_name: 'Alice',
  role: 1,
  group: 'default',
  request_count: 0,
  status: 1,
  aff_count: 0,
  created_time: 0,
  balance_reminder: {
    enabled: true,
    threshold: { nanos: '1200000', currency: 'USD' },
    effective_enabled: true,
    effective_threshold: { nanos: '1200000', currency: 'USD' },
  },
}
const settings = {
  notify_type: 'webhook',
  quota_warning_threshold: 1200,
  notification_email: '',
  webhook_url: 'https://example.com/notify',
  webhook_secret: 'webhook-secret',
  bark_url: '',
  gotify_url: '',
  gotify_token: '',
  gotify_priority: 5,
  record_ip_log: true,
  upstream_model_update_notify_enabled: true,
}

const { quota_warning_threshold: _legacyThreshold, ...transportSettings } =
  settings
const reminderPreference = {
  enabled: true,
  threshold: { nanos: '1200000', currency: 'USD' },
}

const locales = { en, zh, 'zh-TW': zhTW, fr, ru, ja, vi: viLocale }
const clients: QueryClient[] = []
const retirementMessage = 'The setting for unpriced models has been retired.'

afterEach(async () => {
  toast.dismiss()
  useAuthStore.getState().auth.reset()
  for (const client of clients.splice(0)) client.clear()
  await i18next.changeLanguage('en')
  for (const language of Object.keys(locales)) {
    i18next.removeResourceBundle(language, 'translation')
  }
  vi.restoreAllMocks()
})

/** The body a PUT sends over the wire: undefined fields are left out. */
function sentBody(put: { mock: { calls: unknown[][] } }): unknown {
  const encoded = JSON.stringify(put.mock.calls[0][1])
  return JSON.parse(encoded)
}

describe('exact balance reminders', () => {
  it('saves an exact native threshold beyond the safe integer range without legacy quota fields', async () => {
    const current = {
      ...profile,
      setting: JSON.stringify({ ...settings, quota_warning_enabled: true }),
      balance_reminder: {
        enabled: true,
        threshold: { nanos: '9007199254740993', currency: 'EUR' },
        effective_enabled: true,
        effective_threshold: { nanos: '9007199254740993', currency: 'EUR' },
      },
    }
    vi.spyOn(api, 'get').mockResolvedValue({
      data: { success: true, data: current },
    })
    const put = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    const user = userEvent.setup()
    render(<NotificationTab profile={current} onUpdate={vi.fn()} />)
    const threshold = screen.getByRole('textbox', {
      name: 'Reminder threshold (EUR)',
    })
    expect(threshold).toHaveValue('9007199.254740993')
    await user.clear(threshold)
    await user.type(threshold, '9223372036.854775807')
    await user.click(screen.getByRole('button', { name: 'Save Settings' }))
    await waitFor(() => expect(put).toHaveBeenCalledTimes(1))
    expect(put.mock.calls[0][0]).toBe('/api/user/setting')
    expect(put.mock.calls[0][1]).toMatchObject({
      balance_reminder: {
        enabled: true,
        threshold: { nanos: '9223372036854775807', currency: 'EUR' },
      },
    })
    expect(put.mock.calls[0][1]).not.toHaveProperty('quota_warning_threshold')
    expect(put.mock.calls[0][1]).not.toHaveProperty('quota_warning_enabled')
  })
})

describe('user settings saves across profile and security', () => {
  it.each([true, false])(
    'disabling IP recording preserves notification settings without forwarding legacy unpriced=%s',
    async (legacy) => {
      const get = vi.spyOn(api, 'get').mockResolvedValue({
        data: {
          success: true,
          data: {
            ...profile,
            setting: JSON.stringify({
              ...settings,
              accept_unset_model_ratio_model: legacy,
            }),
          },
        },
      })
      const put = vi
        .spyOn(api, 'put')
        .mockResolvedValue({ data: { success: true } })
      await updateUserSettings({ record_ip_log: false })
      expect(get).toHaveBeenCalledWith('/api/user/self')
      expect(put).toHaveBeenCalledWith('/api/user/setting', {
        ...transportSettings,
        record_ip_log: false,
      })
    }
  )

  it('saving IP recording for a user with no settings supplies the defaults and leaves unchosen switches unset', async () => {
    vi.spyOn(api, 'get').mockResolvedValue({
      data: { success: true, data: { ...profile, role: ROLE.SUPER_ADMIN } },
    })
    const put = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    await updateUserSettings({ record_ip_log: true })
    expect(put.mock.calls[0][0]).toBe('/api/user/setting')
    expect(sentBody(put)).toEqual({
      notify_type: 'email',
      notification_email: '',
      webhook_url: '',
      webhook_secret: '',
      bark_url: '',
      gotify_url: '',
      gotify_token: '',
      gotify_priority: 5,
      record_ip_log: true,
      upstream_model_update_notify_enabled: false,
    })
  })

  it('alternating notification and IP saves retains the latest values from each page', async () => {
    let saved = { ...settings }
    vi.spyOn(api, 'get').mockImplementation(async () => ({
      data: {
        success: true,
        data: { ...profile, setting: JSON.stringify(saved) },
      },
    }))
    vi.spyOn(api, 'put').mockImplementation(async (_url, body) => {
      saved = body as typeof settings
      return { data: { success: true } }
    })
    await updateUserSettings({ record_ip_log: false })
    await updateUserSettings({
      notification_email: 'latest@example.com',
      balance_reminder: {
        enabled: true,
        threshold: { nanos: '2500', currency: 'USD' },
      },
    })
    await updateUserSettings({ record_ip_log: true })
    expect(saved).toEqual({
      ...transportSettings,
      notification_email: 'latest@example.com',
      record_ip_log: true,
    })
  })

  it('a rejected profile read prevents the settings write', async () => {
    vi.spyOn(api, 'get').mockRejectedValue(new Error('offline'))
    const put = vi.spyOn(api, 'put')
    await expect(updateUserSettings({ record_ip_log: false })).rejects.toThrow(
      'offline'
    )
    expect(put).not.toHaveBeenCalled()
  })

  it('an unsuccessful profile response prevents the settings write', async () => {
    vi.spyOn(api, 'get').mockResolvedValue({
      data: { success: false, message: 'Unavailable' },
    })
    const put = vi.spyOn(api, 'put')
    expect(await updateUserSettings({ record_ip_log: false })).toEqual({
      success: false,
      message: 'Unavailable',
    })
    expect(put).not.toHaveBeenCalled()
  })

  it('an unsuccessful settings write is returned to the caller', async () => {
    vi.spyOn(api, 'get').mockResolvedValue({
      data: { success: true, data: profile },
    })
    vi.spyOn(api, 'put').mockResolvedValue({
      data: { success: false, message: 'Save failed' },
    })
    expect(await updateUserSettings({ record_ip_log: true })).toEqual({
      success: false,
      message: 'Save failed',
    })
  })

  it.each([true, false])(
    'saving notifications with legacy unpriced=%s keeps current IP settings without the retired preference',
    async (legacy) => {
      const onUpdate = vi.fn()
      vi.spyOn(api, 'get').mockResolvedValue({
        data: {
          success: true,
          data: {
            ...profile,
            setting: JSON.stringify({
              ...settings,
              record_ip_log: false,
              accept_unset_model_ratio_model: legacy,
            }),
          },
        },
      })
      const put = vi
        .spyOn(api, 'put')
        .mockResolvedValue({ data: { success: true } })
      render(
        <NotificationTab
          profile={{
            ...profile,
            setting: JSON.stringify({
              ...settings,
              accept_unset_model_ratio_model: legacy,
            }),
          }}
          onUpdate={onUpdate}
        />
      )
      expect(
        screen.queryByRole('switch', { name: 'Record IP Address' })
      ).not.toBeInTheDocument()
      expect(
        screen.queryByRole('switch', { name: 'Accept Unpriced Models' })
      ).not.toBeInTheDocument()
      expect(
        screen.queryByRole('heading', { name: 'Preferences' })
      ).not.toBeInTheDocument()
      expect(
        screen.queryByText('Configure your account behavior preferences')
      ).not.toBeInTheDocument()
      fireEvent.change(
        screen.getByRole('textbox', { name: 'Reminder threshold (USD)' }),
        { target: { value: '0.0054' } }
      )
      fireEvent.click(screen.getByRole('button', { name: 'Save Settings' }))
      await waitFor(() => expect(onUpdate).toHaveBeenCalled())
      expect(put).toHaveBeenCalledWith(
        '/api/user/setting',
        {
          ...transportSettings,
          balance_reminder: {
            enabled: true,
            threshold: { nanos: '5400000', currency: 'USD' },
          },
          record_ip_log: false,
        },
        { signal: expect.any(AbortSignal) }
      )
    }
  )
})

type SettingsEditor = 'notifications' | 'privacy'

function renderSettingsEditor(editor: SettingsEditor, legacy = true) {
  const currentProfile = {
    ...profile,
    setting: JSON.stringify({
      ...settings,
      accept_unset_model_ratio_model: legacy,
    }),
  }
  vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url !== '/api/user/self') throw new Error(`Unexpected GET ${url}`)
    return { data: { success: true, data: currentProfile } }
  })
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: true } })
  const onUpdate = vi.fn()
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  clients.push(client)
  render(
    <QueryClientProvider client={client}>
      {editor === 'notifications' ? (
        <NotificationTab profile={currentProfile} onUpdate={onUpdate} />
      ) : (
        <PrivacyCard profile={currentProfile} onUpdate={onUpdate} />
      )}
      <Toaster />
    </QueryClientProvider>
  )
  return { put, onUpdate, user: userEvent.setup() }
}

async function editSettingsDraft(editor: SettingsEditor) {
  const user = userEvent.setup()
  if (editor === 'privacy') {
    const toggle = screen.getByRole('switch', {
      name: i18next.t('Record IP Address'),
    })
    toggle.focus()
    await user.keyboard(' ')
    return
  }
  const url = screen.getByRole('textbox', { name: i18next.t('Webhook URL') })
  await user.clear(url)
  await user.type(url, 'https://example.com/edited-notify')
  const secret = screen.getByLabelText(i18next.t('Webhook Secret'))
  await user.clear(secret)
  await user.type(secret, 'edited-webhook-secret')
}

function expectSettingsDraft(editor: SettingsEditor) {
  if (editor === 'privacy') {
    expect(
      screen.getByRole('switch', { name: i18next.t('Record IP Address') })
    ).not.toBeChecked()
    return
  }
  expect(
    screen.getByRole('textbox', { name: i18next.t('Webhook URL') })
  ).toHaveValue('https://example.com/edited-notify')
  expect(screen.getByLabelText(i18next.t('Webhook Secret'))).toHaveValue(
    'edited-webhook-secret'
  )
}

function expectedSettingsBody(editor: SettingsEditor) {
  return editor === 'privacy'
    ? { ...transportSettings, record_ip_log: false }
    : {
        ...transportSettings,
        balance_reminder: reminderPreference,
        webhook_url: 'https://example.com/edited-notify',
        webhook_secret: 'edited-webhook-secret',
      }
}

function expectSettingsWrites(
  put: { mock: { calls: unknown[][] } },
  editor: SettingsEditor
) {
  for (const [url, body] of put.mock.calls) {
    expect(url).toBe('/api/user/setting')
    const encoded = JSON.stringify(body)
    expect(JSON.parse(encoded)).toEqual(expectedSettingsBody(editor))
  }
}

describe('retired unpriced-model setting at the real settings API boundary', () => {
  it.each([
    { accept_unset_model_ratio_model: null },
    { Accept_Unset_Model_Ratio_Model: true },
    { ACCEPT_UNSET_MODEL_RATIO_MODEL: false },
  ])(
    'does not forward legacy null or case aliases from saved settings: %j',
    async (legacy) => {
      vi.spyOn(api, 'get').mockResolvedValue({
        data: {
          success: true,
          data: {
            ...profile,
            setting: JSON.stringify({ ...settings, ...legacy }),
          },
        },
      })
      const put = vi
        .spyOn(api, 'put')
        .mockResolvedValue({ data: { success: true } })
      await updateUserSettings({ record_ip_log: false })
      expect(sentBody(put)).toEqual({
        ...transportSettings,
        record_ip_log: false,
      })
    }
  )

  it.each([true, false])(
    'the Security IP switch saves without forwarding stored legacy=%s',
    async (legacy) => {
      const { put, onUpdate, user } = renderSettingsEditor('privacy', legacy)
      await editSettingsDraft('privacy')
      await user.click(screen.getByRole('button', { name: 'Save Settings' }))
      await waitFor(() => expect(onUpdate).toHaveBeenCalledExactlyOnceWith())
      expect(put).toHaveBeenCalledTimes(1)
      expectSettingsWrites(put, 'privacy')
      expectSettingsDraft('privacy')
    }
  )

  it.each([
    ['notifications', 'network'],
    ['notifications', 'server'],
    ['privacy', 'network'],
    ['privacy', 'server'],
  ] as const)(
    '%s keeps the draft after a %s failure and retries without the retired field',
    async (editor, failure) => {
      const { put, onUpdate, user } = renderSettingsEditor(editor)
      if (failure === 'network') {
        put.mockRejectedValueOnce(new Error('Settings save rejected'))
      } else {
        put.mockResolvedValueOnce({
          data: { success: false, message: 'Settings save rejected' },
        })
      }
      await editSettingsDraft(editor)
      const save = screen.getByRole('button', { name: 'Save Settings' })
      await user.click(save)
      expect(await screen.findByText('Settings save rejected')).toBeVisible()
      expect(onUpdate).not.toHaveBeenCalled()
      expectSettingsDraft(editor)
      expect(save).toBeEnabled()
      await user.click(save)
      await waitFor(() => expect(onUpdate).toHaveBeenCalledExactlyOnceWith())
      expect(put).toHaveBeenCalledTimes(2)
      expectSettingsWrites(put, editor)
    }
  )

  it.each(['notifications', 'privacy'] as const)(
    '%s disables duplicate submission while the settings write is pending',
    async (editor) => {
      const { put, onUpdate, user } = renderSettingsEditor(editor)
      let complete: () => void = () => {
        throw new Error('Save not started')
      }
      put.mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            complete = () => resolve({ data: { success: true } })
          })
      )
      await editSettingsDraft(editor)
      await user.click(screen.getByRole('button', { name: 'Save Settings' }))
      const saving = await screen.findByRole('button', { name: 'Saving...' })
      expect(saving).toBeDisabled()
      await user.click(saving)
      expect(put).toHaveBeenCalledTimes(1)
      expect(onUpdate).not.toHaveBeenCalled()
      expectSettingsDraft(editor)
      await act(async () => complete())
      await waitFor(() => expect(onUpdate).toHaveBeenCalledExactlyOnceWith())
      expectSettingsWrites(put, editor)
    }
  )

  it.each([
    ['en', 'The setting for unpriced models has been retired.'],
    ['zh', '无定价模型设置已退役。'],
    ['zh-TW', '未定價模型設定已退役。'],
    ['fr', 'Le paramètre des modèles sans tarification a été retiré.'],
    [
      'ru',
      'Настройка для моделей без указанной цены выведена из эксплуатации.',
    ],
    ['ja', '価格未設定のモデルに関する設定は廃止されました。'],
    ['vi', 'Cài đặt cho mô hình chưa có giá đã ngừng hoạt động.'],
  ] as const)(
    'shows the retired API error in %s and retains notification credentials for retry',
    async (language, message) => {
      for (const [code, resource] of Object.entries(locales)) {
        i18next.addResourceBundle(code, 'translation', resource.translation)
      }
      await i18next.changeLanguage(language)
      const { put, onUpdate, user } = renderSettingsEditor('notifications')
      put.mockRejectedValueOnce({
        response: {
          status: 410,
          headers: { 'cache-control': 'no-store' },
          data: {
            success: false,
            code: 'legacy_unpriced_model_setting_retired',
            message: retirementMessage,
            message_key: retirementMessage,
          },
        },
      })
      await editSettingsDraft('notifications')
      const save = screen.getByRole('button', {
        name: i18next.t('Save Settings'),
      })
      await user.click(save)
      expect(await screen.findByText(message)).toBeVisible()
      expect(onUpdate).not.toHaveBeenCalled()
      expectSettingsDraft('notifications')
      await user.click(save)
      await waitFor(() => expect(onUpdate).toHaveBeenCalledExactlyOnceWith())
      expect(put).toHaveBeenCalledTimes(2)
      expectSettingsWrites(put, 'notifications')
    }
  )
})

it.each([
  [
    'en',
    'Legacy quota reminders have been retired. Use balance reminder settings.',
  ],
  ['zh', '旧版额度提醒已退役。请使用余额提醒设置。'],
  ['zh-TW', '舊版額度提醒已退役。請使用餘額提醒設定。'],
  [
    'fr',
    'Les anciens rappels de quota ont été retirés. Utilisez les paramètres de rappel de solde.',
  ],
  [
    'ru',
    'Устаревшие уведомления о квоте выведены из эксплуатации. Используйте настройки уведомления о балансе.',
  ],
  [
    'ja',
    '旧式のクォータ通知は廃止されました。残高通知の設定を使用してください。',
  ],
  [
    'vi',
    'Nhắc nhở hạn ngạch cũ đã ngừng hoạt động. Hãy sử dụng cài đặt nhắc nhở số dư.',
  ],
] as const)(
  'shows the quota reminder retirement API message in %s without losing the notification draft',
  async (language, message) => {
    for (const [code, resource] of Object.entries(locales)) {
      i18next.addResourceBundle(code, 'translation', resource.translation)
    }
    await i18next.changeLanguage(language)
    const { put, onUpdate, user } = renderSettingsEditor('notifications')
    const key =
      'Legacy quota reminders have been retired. Use balance reminder settings.'
    put.mockRejectedValueOnce({
      response: {
        status: 410,
        headers: { 'cache-control': 'no-store' },
        data: {
          success: false,
          code: 'legacy_quota_reminder_settings_retired',
          message: key,
          message_key: key,
        },
      },
    })
    await editSettingsDraft('notifications')
    const save = screen.getByRole('button', {
      name: i18next.t('Save Settings'),
    })
    await user.click(save)
    expect(await screen.findByText(message)).toBeVisible()
    expect(onUpdate).not.toHaveBeenCalled()
    expectSettingsDraft('notifications')
    await user.click(save)
    await waitFor(() => expect(onUpdate).toHaveBeenCalledExactlyOnceWith())
    expect(put).toHaveBeenCalledTimes(2)
    expectSettingsWrites(put, 'notifications')
  }
)

describe('notifications a user receives', () => {
  function renderNotificationTab(
    role: number,
    setting: Record<string, unknown>,
    effectiveEnabled = true,
    threshold: { nanos: string; currency: string } | null = null
  ) {
    const onUpdate = vi.fn()
    render(
      <NotificationTab
        profile={{
          ...profile,
          role,
          setting: JSON.stringify(setting),
          balance_reminder: {
            enabled: true,
            threshold,
            effective_enabled: effectiveEnabled,
            effective_threshold: threshold ?? {
              nanos: '2000000',
              currency: 'USD',
            },
          },
        }}
        onUpdate={onUpdate}
      />
    )
    return onUpdate
  }

  function mockSave(role: number, setting: Record<string, unknown>) {
    vi.spyOn(api, 'get').mockResolvedValue({
      data: {
        success: true,
        data: { ...profile, role, setting: JSON.stringify(setting) },
      },
    })
    return vi.spyOn(api, 'put').mockResolvedValue({ data: { success: true } })
  }

  async function save(onUpdate: () => void) {
    fireEvent.click(screen.getByRole('button', { name: 'Save Settings' }))
    await waitFor(() => expect(onUpdate).toHaveBeenCalled())
  }

  it.each([
    { name: 'user', role: ROLE.USER, channel: undefined },
    { name: 'admin', role: ROLE.ADMIN, channel: false },
    { name: 'root', role: ROLE.SUPER_ADMIN, channel: true },
  ])(
    'a $name who never chose sees the reminder on and the channel switch as $channel',
    (row) => {
      renderNotificationTab(row.role, { notify_type: 'email' })
      expect(
        screen.getByRole('switch', { name: 'Low balance reminder' })
      ).toBeChecked()
      const channel = screen.queryByRole('switch', {
        name: 'Channel notifications',
      })
      const upstream = screen.queryByRole('switch', {
        name: 'Upstream Model Updates',
      })
      if (row.channel === undefined) {
        expect(channel).not.toBeInTheDocument()
        expect(upstream).not.toBeInTheDocument()
        return
      }
      expect(channel?.getAttribute('aria-checked')).toBe(String(row.channel))
      expect(upstream).not.toBeChecked()
    }
  )

  it('a user can enable reminders even when the saved effective status is off', () => {
    renderNotificationTab(ROLE.USER, { notify_type: 'email' }, false)
    expect(screen.getByText('Notification Method')).toBeInTheDocument()
    expect(
      screen.getByRole('switch', { name: 'Low balance reminder' })
    ).toBeEnabled()
    expect(
      screen.getByText('Saved reminder status', { exact: false })
    ).toHaveTextContent('Disabled')
  })

  it('an admin with effective reminders off retains both personal and administrator controls', () => {
    renderNotificationTab(ROLE.ADMIN, { notify_type: 'email' }, false)
    expect(
      screen.getByRole('switch', { name: 'Low balance reminder' })
    ).toBeEnabled()
    expect(
      screen.getByRole('switch', { name: 'Channel notifications' })
    ).toBeInTheDocument()
    expect(screen.getByText('Notification Method')).toBeInTheDocument()
  })

  it('an inherited threshold displays the effective amount and saves explicit null', async () => {
    const put = mockSave(ROLE.USER, { notify_type: 'email' })
    const onUpdate = renderNotificationTab(ROLE.USER, { notify_type: 'email' })
    expect(
      screen.getByRole('switch', { name: 'Use site reminder threshold' })
    ).toBeChecked()
    expect(
      screen.queryByRole('textbox', { name: 'Reminder threshold (USD)' })
    ).not.toBeInTheDocument()
    expect(
      screen.getByText('Saved effective threshold', { exact: false })
    ).toHaveTextContent('0.002 USD')
    await save(onUpdate)
    expect(sentBody(put)).toMatchObject({
      balance_reminder: { enabled: true, threshold: null },
    })
  })

  it('an untouched threshold is saved as exact native nanos despite legacy quota settings', async () => {
    const setting = { notify_type: 'email', quota_warning_threshold: 1234 }
    const put = mockSave(ROLE.USER, setting)
    const onUpdate = renderNotificationTab(ROLE.USER, setting, true, {
      nanos: '1234',
      currency: 'USD',
    })
    expect(
      screen.getByRole('textbox', { name: 'Reminder threshold (USD)' })
    ).toHaveValue('0.000001234')
    await save(onUpdate)
    expect(sentBody(put)).toMatchObject({
      balance_reminder: {
        enabled: true,
        threshold: { nanos: '1234', currency: 'USD' },
      },
    })
    expect(sentBody(put)).not.toHaveProperty('quota_warning_threshold')
  })

  it('turning the reminder off preserves the explicit threshold in the complete preference', async () => {
    const put = mockSave(ROLE.USER, { notify_type: 'email' })
    const onUpdate = renderNotificationTab(
      ROLE.USER,
      { notify_type: 'email' },
      true,
      { nanos: '1', currency: 'USD' }
    )
    fireEvent.click(
      screen.getByRole('switch', { name: 'Low balance reminder' })
    )
    expect(
      screen.getByRole('textbox', { name: 'Reminder threshold (USD)' })
    ).toHaveValue('0.000000001')
    await save(onUpdate)
    expect(sentBody(put)).toMatchObject({
      balance_reminder: {
        enabled: false,
        threshold: { nanos: '1', currency: 'USD' },
      },
    })
  })

  it('switches root never touched stay unset when it saves', async () => {
    const put = mockSave(ROLE.SUPER_ADMIN, { notify_type: 'email' })
    const onUpdate = renderNotificationTab(ROLE.SUPER_ADMIN, {
      notify_type: 'email',
    })
    await save(onUpdate)
    expect(sentBody(put)).not.toHaveProperty('channel_notify_enabled')
    expect(sentBody(put)).not.toHaveProperty('quota_warning_enabled')
  })

  it('an admin turning channel notifications on saves the switch', async () => {
    const put = mockSave(ROLE.ADMIN, { notify_type: 'email' })
    const onUpdate = renderNotificationTab(ROLE.ADMIN, { notify_type: 'email' })
    fireEvent.click(
      screen.getByRole('switch', { name: 'Channel notifications' })
    )
    await save(onUpdate)
    expect(sentBody(put)).toMatchObject({ channel_notify_enabled: true })
  })
})

function reminderEditor(...input: [] | [unknown]) {
  const reminder = input.length ? input[0] : profile.balance_reminder
  // Preserve the actual JSON response boundary, including omitted properties.
  const encoded = JSON.stringify({
    ...profile,
    setting: JSON.stringify({
      ...settings,
      quota_warning_enabled: true,
      balance_reminder: { enabled: false, threshold: null },
    }),
    balance_reminder: reminder,
  })
  const current: UserProfile = JSON.parse(encoded)
  const get = vi
    .spyOn(api, 'get')
    .mockResolvedValue({ data: { success: true, data: current } })
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: true } })
  const onUpdate = vi.fn()
  const view = render(
    <>
      <NotificationTab profile={current} onUpdate={onUpdate} />
      <Toaster />
    </>
  )
  return { get, put, onUpdate, current, view, user: userEvent.setup() }
}

function changeReminderIdentity(change: 'account' | 'session') {
  const auth = useAuthStore.getState().auth
  useAuthStore.setState({
    auth: {
      ...auth,
      user: { id: change === 'account' ? 2 : 1, username: 'next', role: 1 },
      session: {
        sid: 'next-session',
        current: true,
        login_method: 'password',
        ip: '',
        user_agent: '',
        created_at: 1,
        last_active_at: 1,
        expires_at: 9999999999,
      },
    },
  })
}

describe('balance reminder precision, inheritance and identity boundaries', () => {
  it('uses keyboard controls to distinguish inherited null, explicit zero, and inheritance again', async () => {
    const { put, user } = reminderEditor({
      enabled: false,
      threshold: null,
      effective_enabled: false,
      effective_threshold: { nanos: '1', currency: 'USD' },
    })
    const inherit = screen.getByRole('switch', {
      name: 'Use site reminder threshold',
    })
    inherit.focus()
    await user.keyboard(' ')
    const input = screen.getByRole('textbox', {
      name: 'Reminder threshold (USD)',
    })
    expect(input).toHaveValue('')
    await user.click(screen.getByRole('button', { name: 'Save Settings' }))
    expect(await screen.findByText('Invalid amount')).toBeVisible()
    expect(put).not.toHaveBeenCalled()
    await user.type(input, '0')
    await user.keyboard('{Enter}')
    await waitFor(() => expect(put).toHaveBeenCalledTimes(1))
    expect(put.mock.calls[0][1]).toMatchObject({
      balance_reminder: {
        enabled: false,
        threshold: { nanos: '0', currency: 'USD' },
      },
    })
    inherit.focus()
    await user.keyboard(' ')
    await user.click(screen.getByRole('button', { name: 'Save Settings' }))
    await waitFor(() => expect(put).toHaveBeenCalledTimes(2))
    expect(put.mock.calls[1][1]).toMatchObject({
      balance_reminder: { enabled: false, threshold: null },
    })
  })

  it.each([
    '-0.000000001',
    '9223372036.854775808',
    '0.0000000001',
    '1e2',
    'NaN',
  ])(
    'rejects the invalid exact draft %s without a settings write',
    async (amount) => {
      const { put, user } = reminderEditor()
      const input = screen.getByRole('textbox', {
        name: 'Reminder threshold (USD)',
      })
      await user.clear(input)
      await user.type(input, amount)
      await user.click(screen.getByRole('button', { name: 'Save Settings' }))
      expect(await screen.findByText('Invalid amount')).toBeVisible()
      expect(input).toHaveValue(amount)
      expect(put).not.toHaveBeenCalled()
    }
  )

  it.each([
    undefined,
    null,
    {
      enabled: true,
      threshold: null,
      effective_enabled: true,
      effective_threshold: { nanos: 1, currency: 'USD' },
    },
    {
      enabled: true,
      threshold: null,
      effective_enabled: true,
      effective_threshold: { nanos: '-1', currency: 'USD' },
    },
    {
      enabled: true,
      threshold: null,
      effective_enabled: true,
      effective_threshold: { nanos: '9223372036854775808', currency: 'USD' },
    },
    {
      enabled: true,
      threshold: { nanos: '0', currency: 'EUR' },
      effective_enabled: true,
      effective_threshold: { nanos: '1', currency: 'USD' },
    },
    {
      enabled: true,
      threshold: null,
      effective_enabled: true,
      effective_threshold: { nanos: '0', currency: 'usd' },
    },
  ])(
    'does not infer a reminder from missing or malformed root data: %j',
    (reminder) => {
      const { put } = reminderEditor(reminder)
      expect(
        screen.getByText('Invalid balance reminder configuration')
      ).toBeVisible()
      expect(
        screen.queryByRole('button', { name: 'Save Settings' })
      ).not.toBeInTheDocument()
      expect(put).not.toHaveBeenCalled()
    }
  )

  it.each(['network', 'business'] as const)(
    'keeps the exact threshold and enabled draft after a %s failure and retries',
    async (failure) => {
      const { put, user, onUpdate } = reminderEditor()
      if (failure === 'network') {
        put.mockRejectedValueOnce(new Error('Reminder write rejected'))
      } else {
        put.mockResolvedValueOnce({
          data: { success: false, message: 'Reminder write rejected' },
        })
      }
      const input = screen.getByRole('textbox', {
        name: 'Reminder threshold (USD)',
      })
      await user.clear(input)
      await user.type(input, '9007199.254740993')
      await user.click(
        screen.getByRole('switch', { name: 'Low balance reminder' })
      )
      const save = screen.getByRole('button', { name: 'Save Settings' })
      await user.click(save)
      expect(await screen.findByText('Reminder write rejected')).toBeVisible()
      expect(input).toHaveValue('9007199.254740993')
      expect(onUpdate).not.toHaveBeenCalled()
      await user.click(save)
      await waitFor(() => expect(onUpdate).toHaveBeenCalledTimes(1))
      expect(put.mock.calls[0][1]).toEqual(put.mock.calls[1][1])
      expect(put.mock.calls[1][1]).toMatchObject({
        balance_reminder: {
          enabled: false,
          threshold: { nanos: '9007199254740993', currency: 'USD' },
        },
      })
    }
  )

  it.each([
    ['account', 'read'],
    ['session', 'read'],
    ['account', 'write'],
    ['session', 'write'],
  ] as const)(
    'ignores a previous %s after an in-flight %s and never emits stale success',
    async (change, phase) => {
      useAuthStore
        .getState()
        .auth.setUser({ id: 1, username: 'alice', role: 1 })
      const { get, put, user, onUpdate, current } = reminderEditor()
      let finish: () => void = () => {
        throw new Error('Request not started')
      }
      const pending = new Promise<{
        data: { success: boolean; data: UserProfile }
      }>((resolve) => {
        finish = () => resolve({ data: { success: true, data: current } })
      })
      if (phase === 'read') get.mockReturnValueOnce(pending)
      else put.mockReturnValueOnce(pending)
      const success = vi.spyOn(toast, 'success')
      await user.click(screen.getByRole('button', { name: 'Save Settings' }))
      await waitFor(() =>
        expect(phase === 'read' ? get : put).toHaveBeenCalledTimes(1)
      )
      const signal = get.mock.calls[0][1]?.signal
      act(() => changeReminderIdentity(change))
      expect(signal?.aborted).toBe(true)
      await act(async () => finish())
      expect(onUpdate).not.toHaveBeenCalled()
      expect(success).not.toHaveBeenCalled()
      expect(put).toHaveBeenCalledTimes(phase === 'read' ? 0 : 1)
    }
  )

  it('never submits the separate root reminder when saving Security IP settings even if setting JSON contains one', async () => {
    vi.spyOn(api, 'get').mockResolvedValue({
      data: {
        success: true,
        data: {
          ...profile,
          setting: JSON.stringify({
            ...settings,
            quota_warning_enabled: true,
            balance_reminder: { enabled: false, threshold: null },
          }),
        },
      },
    })
    const put = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    await updateUserSettings({ record_ip_log: false })
    expect(put.mock.calls[0][1]).not.toHaveProperty('balance_reminder')
    expect(put.mock.calls[0][1]).not.toHaveProperty('quota_warning_threshold')
    expect(put.mock.calls[0][1]).not.toHaveProperty('quota_warning_enabled')
  })

  it('retains all nanos and native currency through seven live language changes', async () => {
    for (const [language, resource] of Object.entries(locales)) {
      i18next.addResourceBundle(
        language,
        'translation',
        resource.translation,
        true,
        true
      )
    }
    const { put } = reminderEditor({
      enabled: true,
      threshold: { nanos: '9007199254740993', currency: 'EUR' },
      effective_enabled: true,
      effective_threshold: { nanos: '9007199254740993', currency: 'EUR' },
    })
    for (const [language, formatted] of [
      ['en', '9,007,199.254740993 EUR'],
      ['zh', '9,007,199.254740993 EUR'],
      ['zh-TW', '9,007,199.254740993 EUR'],
      ['fr', '9 007 199,254740993 EUR'],
      ['ru', '9 007 199,254740993 EUR'],
      ['ja', '9,007,199.254740993 EUR'],
      ['vi', '9.007.199,254740993 EUR'],
    ]) {
      await act(async () => {
        await i18next.changeLanguage(language)
      })
      expect(
        screen.getByRole('textbox', {
          name: `${i18next.t('Reminder threshold')} (EUR)`,
        })
      ).toHaveValue('9007199.254740993')
      expect(
        screen.getByText(
          (_, element) =>
            element?.tagName === 'P' &&
            !!element.textContent?.endsWith(formatted)
        )
      ).toBeVisible()
    }
    expect(put).not.toHaveBeenCalled()
  })
})
