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
import { act, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createInstance } from 'i18next'
import { useState } from 'react'
import { I18nextProvider } from 'react-i18next'
import { toast } from 'sonner'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import french from '@/i18n/locales/fr.json'
import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { SettingsPageProvider } from '../../components/settings-page-context'
import { EmailSettingsSection } from '../email-settings-section'

function Fixture(props: { config?: string }) {
  const [container, setContainer] = useState<HTMLDivElement | null>(null)
  return (
    <>
      <div ref={setContainer} />
      <SettingsPageProvider actionsContainer={container}>
        <EmailSettingsSection
          reminderConfig={
            props.config ??
            JSON.stringify({
              enabled: true,
              threshold: { nanos: '9007199254740993', currency: 'EUR' },
            })
          }
          defaultValues={{
            EmailProvider: 'smtp',
            ResendApiKey: '',
            ResendKeyConfigured: false,
            ResendFrom: '',
            SMTPServer: 'smtp.example.com',
            SMTPPort: '587',
            SMTPAccount: 'noreply@example.com',
            SMTPFrom: '',
            SMTPToken: '',
            SMTPSSLEnabled: false,
            SMTPStartTLSEnabled: true,
            SMTPInsecureSkipVerify: false,
            SMTPForceAuthLogin: false,
          }}
        />
      </SettingsPageProvider>
    </>
  )
}

function renderSettings(config?: string) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  const view = render(
    <QueryClientProvider client={client}>
      <Fixture config={config} />
    </QueryClientProvider>
  )
  return { ...view, client }
}

beforeEach(() => {
  vi.spyOn(api, 'put').mockResolvedValue({ data: { success: true } })
})

afterEach(() => {
  useAuthStore.getState().auth.reset()
  vi.restoreAllMocks()
})

test('saves the native reminder as one atomic JSON option without changing email credentials', async () => {
  const user = userEvent.setup()
  renderSettings()
  const input = screen.getByRole('textbox', {
    name: 'Default reminder threshold (EUR)',
  })
  expect(input).toHaveValue('9007199.254740993')
  await user.clear(input)
  await user.type(input, '9223372036.854775807')
  await user.click(
    screen.getByRole('button', { name: 'Save balance reminder settings' })
  )
  await waitFor(() => expect(api.put).toHaveBeenCalledTimes(1))
  expect(api.put).toHaveBeenCalledWith('/api/option/', {
    key: 'balance_reminder_setting.config',
    value: JSON.stringify({
      enabled: true,
      threshold: { nanos: '9223372036854775807', currency: 'EUR' },
    }),
  })
})

test('keeps the full French save label wrappable and retries the unchanged exact reminder', async () => {
  const language = createInstance()
  await language.init({
    lng: 'fr',
    fallbackLng: 'fr',
    resources: { fr: french },
    interpolation: { escapeValue: false },
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  render(
    <I18nextProvider i18n={language}>
      <QueryClientProvider client={client}>
        <Fixture />
      </QueryClientProvider>
    </I18nextProvider>
  )
  const user = userEvent.setup()
  const put = vi.mocked(api.put)
  put.mockRejectedValueOnce(new Error('Reminder save rejected'))
  const input = screen.getByRole('textbox', {
    name: `${language.t('Default reminder threshold')} (EUR)`,
  })
  await user.clear(input)
  await user.type(input, '9223372036.854775807')
  const label = 'Enregistrer les paramètres du rappel de solde'
  const save = screen.getByRole('button', { name: label })
  await user.click(save)
  expect(await screen.findByText('Reminder save rejected')).toBeVisible()
  expect(input).toHaveValue('9223372036.854775807')
  expect(save).toHaveTextContent(label)
  expect(save).toHaveClass('h-auto', 'min-h-8', 'min-w-0', 'whitespace-normal')
  expect(save).not.toHaveClass(
    'whitespace-nowrap',
    'truncate',
    'overflow-hidden'
  )
  await user.click(save)
  await waitFor(() => expect(put).toHaveBeenCalledTimes(2))
  expect(put.mock.calls[0]).toEqual(put.mock.calls[1])
  expect(put).toHaveBeenLastCalledWith('/api/option/', {
    key: 'balance_reminder_setting.config',
    value: JSON.stringify({
      enabled: true,
      threshold: { nanos: '9223372036854775807', currency: 'EUR' },
    }),
  })
  await waitFor(() => expect(save).toBeDisabled())
})

test('the default threshold shows as an amount and is not saved again when only SMTP changes', async () => {
  const user = userEvent.setup()
  renderSettings()
  expect(
    screen.getByRole('textbox', { name: 'Default reminder threshold (EUR)' })
  ).toHaveValue('9007199.254740993')
  await user.click(screen.getByRole('switch', { name: 'Force AUTH LOGIN' }))
  await user.click(screen.getByRole('button', { name: 'Save Changes' }))
  await waitFor(() =>
    expect(api.put).toHaveBeenCalledWith('/api/option/', {
      key: 'SMTPForceAuthLogin',
      value: true,
    })
  )
  expect(api.put).toHaveBeenCalledTimes(1)
})

test('an edited default threshold is saved as exact native money', async () => {
  const user = userEvent.setup()
  renderSettings()
  const input = screen.getByRole('textbox', {
    name: 'Default reminder threshold (EUR)',
  })
  await user.clear(input)
  await user.type(input, '1')
  await user.click(
    screen.getByRole('button', { name: 'Save balance reminder settings' })
  )
  await waitFor(() =>
    expect(api.put).toHaveBeenCalledWith('/api/option/', {
      key: 'balance_reminder_setting.config',
      value: JSON.stringify({
        enabled: true,
        threshold: { nanos: '1000000000', currency: 'EUR' },
      }),
    })
  )
  expect(api.put).toHaveBeenCalledTimes(1)
})

test('turning the reminder off preserves the threshold in one complete config', async () => {
  const user = userEvent.setup()
  renderSettings()
  await user.click(screen.getByRole('switch', { name: 'Low balance reminder' }))
  expect(
    screen.queryByRole('textbox', {
      name: 'Default reminder threshold (EUR)',
    })
  ).toHaveValue('9007199.254740993')
  await user.click(
    screen.getByRole('button', { name: 'Save balance reminder settings' })
  )
  await waitFor(() =>
    expect(api.put).toHaveBeenCalledWith('/api/option/', {
      key: 'balance_reminder_setting.config',
      value: JSON.stringify({
        enabled: false,
        threshold: { nanos: '9007199254740993', currency: 'EUR' },
      }),
    })
  )
  expect(api.put).toHaveBeenCalledTimes(1)
})

test.each([
  '',
  '{}',
  '{',
  JSON.stringify({ enabled: false }),
  JSON.stringify({ enabled: true, threshold: null }),
  JSON.stringify({ enabled: true, threshold: { nanos: 1, currency: 'USD' } }),
  JSON.stringify({
    enabled: true,
    threshold: { nanos: '-1', currency: 'USD' },
  }),
  JSON.stringify({
    enabled: true,
    threshold: { nanos: '9223372036854775808', currency: 'USD' },
  }),
  JSON.stringify({ enabled: true, threshold: { nanos: '0', currency: 'usd' } }),
  JSON.stringify({
    enabled: true,
    threshold: { nanos: '0', currency: 'USD' },
    extra: 1,
  }),
  '{"enabled":true,"enabled":false,"threshold":{"nanos":"0","currency":"USD"}}',
  '{"enabled":true,"threshold":{"nanos":"0","currency":"USD","currency":"EUR"}}',
])(
  'rejects absent or invalid persisted reminder config without blocking SMTP: %s',
  async (config) => {
    const user = userEvent.setup()
    renderSettings(config)
    expect(
      screen.getByText('Invalid balance reminder configuration')
    ).toBeVisible()
    expect(
      screen.queryByRole('button', { name: 'Save balance reminder settings' })
    ).not.toBeInTheDocument()
    expect(
      screen.queryByRole('textbox', { name: /Default reminder threshold/ })
    ).not.toBeInTheDocument()
    await user.click(screen.getByRole('switch', { name: 'Force AUTH LOGIN' }))
    await user.click(screen.getByRole('button', { name: 'Save Changes' }))
    await waitFor(() =>
      expect(api.put).toHaveBeenCalledExactlyOnceWith('/api/option/', {
        key: 'SMTPForceAuthLogin',
        value: true,
      })
    )
  }
)

test.each(['', '-0.000000001', '9223372036.854775808', '0.0000000001', '1e2'])(
  'rejects the invalid native site threshold %s and retains the draft',
  async (amount) => {
    const user = userEvent.setup()
    renderSettings()
    const input = screen.getByRole('textbox', {
      name: 'Default reminder threshold (EUR)',
    })
    await user.clear(input)
    if (amount) await user.type(input, amount)
    await user.click(
      screen.getByRole('button', { name: 'Save balance reminder settings' })
    )
    expect(await screen.findByText('Invalid amount')).toBeVisible()
    expect(input).toHaveValue(amount)
    expect(api.put).not.toHaveBeenCalled()
  }
)

test('accepts explicit zero and one nano through keyboard submission', async () => {
  const user = userEvent.setup()
  renderSettings()
  const input = screen.getByRole('textbox', {
    name: 'Default reminder threshold (EUR)',
  })
  for (const [amount, nanos] of [
    ['0', '0'],
    ['0.000000001', '1'],
  ]) {
    await user.clear(input)
    await user.type(input, amount)
    await user.keyboard('{Enter}')
    await waitFor(() =>
      expect(api.put).toHaveBeenLastCalledWith('/api/option/', {
        key: 'balance_reminder_setting.config',
        value: JSON.stringify({
          enabled: true,
          threshold: { nanos, currency: 'EUR' },
        }),
      })
    )
  }
  expect(api.put).toHaveBeenCalledTimes(2)
})

test.each(['network', 'business'] as const)(
  'preserves reminder and SMTP drafts after a %s failure, then retries only the atomic reminder',
  async (failure) => {
    const user = userEvent.setup()
    const put = vi.mocked(api.put)
    if (failure === 'network') {
      put.mockRejectedValueOnce(new Error('Reminder save rejected'))
    } else {
      put.mockResolvedValueOnce({
        data: { success: false, message: 'Reminder save rejected' },
      })
    }
    renderSettings()
    const host = screen.getByRole('textbox', { name: 'SMTP Host' })
    await user.clear(host)
    await user.type(host, 'draft.smtp.example.com')
    const input = screen.getByRole('textbox', {
      name: 'Default reminder threshold (EUR)',
    })
    await user.clear(input)
    await user.type(input, '0.000000001')
    await user.click(
      screen.getByRole('switch', { name: 'Low balance reminder' })
    )
    const save = screen.getByRole('button', {
      name: 'Save balance reminder settings',
    })
    await user.click(save)
    expect(await screen.findByText('Reminder save rejected')).toBeVisible()
    expect(input).toHaveValue('0.000000001')
    expect(host).toHaveValue('draft.smtp.example.com')
    expect(
      screen.getByRole('switch', { name: 'Low balance reminder' })
    ).not.toBeChecked()
    await user.click(save)
    await waitFor(() => expect(put).toHaveBeenCalledTimes(2))
    expect(put.mock.calls[0]).toEqual(put.mock.calls[1])
    expect(put.mock.calls[1][1]).toEqual({
      key: 'balance_reminder_setting.config',
      value: JSON.stringify({
        enabled: false,
        threshold: { nanos: '1', currency: 'EUR' },
      }),
    })
    expect(host).toHaveValue('draft.smtp.example.com')
  }
)

test('disables reminder edits and duplicate submissions until the pending save completes', async () => {
  const user = userEvent.setup()
  let finish: () => void = () => {
    throw new Error('Not started')
  }
  vi.mocked(api.put).mockImplementationOnce(
    () =>
      new Promise((resolve) => {
        finish = () => resolve({ data: { success: true } })
      })
  )
  renderSettings()
  const toggle = screen.getByRole('switch', { name: 'Low balance reminder' })
  await user.click(toggle)
  const save = screen.getByRole('button', {
    name: 'Save balance reminder settings',
  })
  await user.click(save)
  expect(save).toBeDisabled()
  expect(toggle).toHaveAttribute('aria-disabled', 'true')
  await user.click(toggle)
  expect(toggle).not.toBeChecked()
  expect(
    screen.getByRole('textbox', { name: 'Default reminder threshold (EUR)' })
  ).toBeDisabled()
  await user.click(save)
  expect(api.put).toHaveBeenCalledTimes(1)
  await act(async () => finish())
  expect(toggle).not.toHaveAttribute('aria-disabled', 'true')
  expect(save).toBeDisabled()
})

test.each(['account', 'session'] as const)(
  'does not refresh or announce a completed site save after the %s changes',
  async (change) => {
    useAuthStore.getState().auth.setUser({ id: 1, username: 'root', role: 100 })
    const user = userEvent.setup()
    let finish: () => void = () => {
      throw new Error('Not started')
    }
    vi.mocked(api.put).mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          finish = () => resolve({ data: { success: true } })
        })
    )
    const { client } = renderSettings()
    client.setQueryData(['system-options'], { untouched: true })
    const success = vi.spyOn(toast, 'success')
    await user.click(
      screen.getByRole('switch', { name: 'Low balance reminder' })
    )
    await user.click(
      screen.getByRole('button', { name: 'Save balance reminder settings' })
    )
    act(() => {
      const auth = useAuthStore.getState().auth
      useAuthStore.setState({
        auth: {
          ...auth,
          user: {
            id: change === 'account' ? 2 : 1,
            username: 'new',
            role: 100,
          },
          session: {
            sid: 'new-session',
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
    })
    await act(async () => finish())
    expect(success).not.toHaveBeenCalled()
    expect(client.getQueryState(['system-options'])?.isInvalidated).toBe(false)
    expect(api.put).toHaveBeenCalledTimes(1)
  }
)
