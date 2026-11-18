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
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
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
import { useState } from 'react'
import { afterEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import { getBillingSectionContent } from '../../billing/section-registry'
import { SettingsPageProvider } from '../../components/settings-page-context'
import type { BillingSettings } from '../../types'

const initialRewards = {
  signup: { nanos: '1', currency: 'EUR' },
  inviter: { nanos: '9007199254740993', currency: 'JPY' },
  invitee: { nanos: '0', currency: 'USD' },
}
const zeroRewards = {
  signup: { nanos: '0', currency: 'EUR' },
  inviter: { nanos: '0', currency: 'JPY' },
  invitee: { nanos: '0', currency: 'USD' },
}
const clients: QueryClient[] = []

async function amountInput(label = 'Signup Reward') {
  return within(await screen.findByRole('group', { name: label })).getByRole(
    'textbox',
    { name: 'Reward amount' }
  )
}

function saveButton() {
  return screen.getByRole('button', { name: 'Save reward settings' })
}

function Fixture(props: {
  config: string
  confirmed: boolean
  termsVersion: string
}) {
  const [actions, setActions] = useState<HTMLDivElement | null>(null)
  const settings: BillingSettings = {
    'reward_setting.config': props.config,
    'checkout_setting.config': '{"providers":{}}',
    TopUpLink: '',
    PayAddress: '',
    EpayId: '',
    EpayKey: '',
    CustomCallbackAddress: '',
    PayMethods: '[]',
    StripeApiSecret: '',
    StripeWebhookSecret: '',
    StripePromotionCodesEnabled: false,
    CreemApiKey: '',
    CreemWebhookSecret: '',
    CreemTestMode: false,
    WaffoEnabled: false,
    WaffoApiKey: '',
    WaffoPrivateKey: '',
    WaffoPublicCert: '',
    WaffoSandboxPublicCert: '',
    WaffoSandboxApiKey: '',
    WaffoSandboxPrivateKey: '',
    WaffoSandbox: false,
    WaffoMerchantId: '',
    WaffoNotifyUrl: '',
    WaffoReturnUrl: '',
    WaffoPayMethods: '[]',
    WaffoPancakeMerchantID: '',
    WaffoPancakePrivateKey: '',
    WaffoPancakeReturnURL: '',
    WaffoPancakeStoreID: '',
    WaffoPancakeProductID: '',
    'payment_setting.compliance_confirmed': props.confirmed,
    'payment_setting.compliance_terms_version': props.termsVersion,
    'payment_setting.compliance_confirmed_at': 0,
    'payment_setting.compliance_confirmed_by': 0,
    'payment_setting.compliance_confirmed_ip': '',
    'checkin_setting.enabled': false,
    'checkin_setting.reward_range': '',
  }
  return (
    <SettingsPageProvider actionsContainer={actions}>
      <div ref={setActions} />
      {getBillingSectionContent('rewards', settings)}
    </SettingsPageProvider>
  )
}

async function renderSettings(
  config = JSON.stringify(initialRewards),
  confirmed = true,
  termsVersion = 'v1'
) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  clients.push(client)
  const router = createRouter({
    routeTree: createRootRoute({
      component: () => (
        <Fixture
          config={config}
          confirmed={confirmed}
          termsVersion={termsVersion}
        />
      ),
    }),
    history: createMemoryHistory({ initialEntries: ['/'] }),
  })
  render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
  return userEvent.setup()
}

afterEach(() => {
  cleanup()
  clients.splice(0).forEach((client) => client.clear())
  vi.restoreAllMocks()
})

it('edits three independently denominated rewards and atomically saves exact nanos beyond the safe integer range', async () => {
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: true } })
  const user = await renderSettings()
  const signup = await screen.findByRole('group', { name: 'Signup Reward' })
  const inviter = screen.getByRole('group', { name: 'Inviter Reward' })
  const invitee = screen.getByRole('group', { name: 'Invitee Reward' })
  expect(
    within(signup).getByRole('textbox', { name: 'Reward amount' })
  ).toHaveValue('0.000000001')
  expect(
    within(inviter).getByRole('textbox', { name: 'Reward amount' })
  ).toHaveValue('9007199.254740993')
  fireEvent.change(
    within(signup).getByRole('textbox', { name: 'Reward amount' }),
    { target: { value: '9223372036.854775807' } }
  )
  fireEvent.change(
    within(invitee).getByRole('textbox', { name: 'Reward amount' }),
    { target: { value: '0.000000009' } }
  )
  await user.click(screen.getByRole('button', { name: 'Save reward settings' }))
  await waitFor(() =>
    expect(put).toHaveBeenCalledWith('/api/option/', {
      key: 'reward_setting.config',
      value: JSON.stringify({
        signup: { nanos: '9223372036854775807', currency: 'EUR' },
        inviter: { nanos: '9007199254740993', currency: 'JPY' },
        invitee: { nanos: '9', currency: 'USD' },
      }),
    })
  )
  expect(put).toHaveBeenCalledTimes(1)
  await waitFor(() =>
    expect(
      screen.getByRole('button', { name: 'Save reward settings' })
    ).toBeDisabled()
  )
  expect(
    screen.queryByRole('spinbutton', { name: 'New User Quota' })
  ).not.toBeInTheDocument()
})

it('accepts zero for every reward without changing any currency', async () => {
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: true } })
  const user = await renderSettings()
  for (const label of ['Signup Reward', 'Inviter Reward', 'Invitee Reward']) {
    fireEvent.change(await amountInput(label), { target: { value: '0' } })
  }
  await user.click(saveButton())
  await waitFor(() =>
    expect(put).toHaveBeenCalledWith('/api/option/', {
      key: 'reward_setting.config',
      value: JSON.stringify(zeroRewards),
    })
  )
})

it.each([
  ['', 'missing config'],
  ['{', 'malformed JSON'],
  ['null', 'null config'],
  ['{}', 'missing rewards'],
  [
    JSON.stringify({
      ...initialRewards,
      signup: { nanos: 1, currency: 'EUR' },
    }),
    'numeric nanos',
  ],
  [
    JSON.stringify({
      ...initialRewards,
      signup: { nanos: '-1', currency: 'EUR' },
    }),
    'negative nanos',
  ],
  [
    JSON.stringify({
      ...initialRewards,
      signup: { nanos: '9223372036854775808', currency: 'EUR' },
    }),
    'overflow nanos',
  ],
  [
    JSON.stringify({
      ...initialRewards,
      signup: { nanos: '1.5', currency: 'EUR' },
    }),
    'fractional nanos',
  ],
  [
    JSON.stringify({
      ...initialRewards,
      signup: { nanos: '1', currency: 'eur' },
    }),
    'lowercase stored currency',
  ],
  [
    JSON.stringify({ ...initialRewards, signup: { nanos: '1', currency: '' } }),
    'missing currency',
  ],
  [
    JSON.stringify({
      ...initialRewards,
      signup: { nanos: '1', currency: 'EURO' },
    }),
    'long currency',
  ],
  [
    JSON.stringify({
      ...initialRewards,
      signup: { nanos: '1', currency: 'EUR', quota: 1 },
    }),
    'unknown amount field',
  ],
  [JSON.stringify({ ...initialRewards, old_quota: 1 }), 'unknown reward field'],
  [
    '{"signup":{"nanos":"0","nanos":"1","currency":"EUR"},"inviter":{"nanos":"0","currency":"JPY"},"invitee":{"nanos":"0","currency":"USD"}}',
    'duplicate nanos',
  ],
  [
    '{"signup":{"nanos":"0","currency":"EUR"},"signup":{"nanos":"1","currency":"EUR"},"inviter":{"nanos":"0","currency":"JPY"},"invitee":{"nanos":"0","currency":"USD"}}',
    'duplicate reward',
  ],
  [
    '{"signup":{"nanos":"0","\\u006eanos":"1","currency":"EUR"},"inviter":{"nanos":"0","currency":"JPY"},"invitee":{"nanos":"0","currency":"USD"}}',
    'escaped duplicate nanos',
  ],
])(
  'does not replace invalid stored configuration (%s: %s) with defaults',
  async (config) => {
    const put = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    await renderSettings(config)
    expect(
      await screen.findByText('Invalid reward configuration')
    ).toBeVisible()
    expect(
      screen.queryByRole('button', { name: 'Save reward settings' })
    ).not.toBeInTheDocument()
    expect(
      screen.queryByRole('textbox', { name: 'Reward amount' })
    ).not.toBeInTheDocument()
    expect(put).not.toHaveBeenCalled()
  }
)

it.each([
  '-0.000000001',
  '0.0000000001',
  '9223372036.854775808',
  '1e3',
  '',
  'NaN',
  '1,25',
])(
  'rejects invalid decimal draft %s without sending a request',
  async (draft) => {
    const put = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    const user = await renderSettings()
    const input = await amountInput()
    fireEvent.change(input, { target: { value: draft } })
    await user.click(saveButton())
    await waitFor(() => expect(input).toHaveAttribute('aria-invalid', 'true'))
    expect(input).toHaveValue(draft)
    expect(put).not.toHaveBeenCalled()
  }
)

it.each([
  [false, 'v1', 'Inviter Reward'],
  [false, 'v1', 'Invitee Reward'],
  [true, 'v0', 'Inviter Reward'],
  [true, 'v0', 'Invitee Reward'],
])(
  'blocks nonzero invitation reward without current compliance (%s/%s/%s)',
  async (confirmed, version, label) => {
    const put = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    const user = await renderSettings(
      JSON.stringify(zeroRewards),
      confirmed,
      version
    )
    const input = await amountInput(label)
    fireEvent.change(input, { target: { value: '0.000000001' } })
    await user.click(saveButton())
    await waitFor(() =>
      expect(
        screen.getAllByText(
          'Non-zero invitation rewards require compliance confirmation in Payment Gateway settings.'
        )
      ).toHaveLength(2)
    )
    expect(input).toHaveValue('0.000000001')
    expect(put).not.toHaveBeenCalled()
    fireEvent.change(input, { target: { value: '0' } })
    fireEvent.change(await amountInput(), { target: { value: '0.000000009' } })
    await user.click(saveButton())
    await waitFor(() =>
      expect(put).toHaveBeenCalledWith('/api/option/', {
        key: 'reward_setting.config',
        value: JSON.stringify({
          ...zeroRewards,
          signup: { nanos: '9', currency: 'EUR' },
        }),
      })
    )
  }
)

it.each(['rejection', 'failure response'])(
  'retains the exact draft after %s and retries one complete configuration',
  async (failure) => {
    const put = vi.spyOn(api, 'put')
    if (failure === 'rejection') {
      put.mockRejectedValueOnce(new Error('Reward save unavailable'))
    } else {
      put.mockResolvedValueOnce({
        data: { success: false, message: 'Reward save unavailable' },
      })
    }
    put.mockResolvedValueOnce({ data: { success: true } })
    const user = await renderSettings()
    const input = await amountInput()
    fireEvent.change(input, { target: { value: '9007199.254740995' } })
    await user.click(saveButton())
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Reward save unavailable'
    )
    expect(input).toHaveValue('9007199.254740995')
    expect(saveButton()).toBeEnabled()
    await user.click(saveButton())
    await waitFor(() => expect(put).toHaveBeenCalledTimes(2))
    const request = {
      key: 'reward_setting.config',
      value: JSON.stringify({
        ...initialRewards,
        signup: { nanos: '9007199254740995', currency: 'EUR' },
      }),
    }
    expect(put).toHaveBeenNthCalledWith(1, '/api/option/', request)
    expect(put).toHaveBeenNthCalledWith(2, '/api/option/', request)
    await waitFor(() => expect(saveButton()).toBeDisabled())
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  }
)

it('accepts valid JSON whitespace, reordered properties and escaped names without changing precise amounts', async () => {
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: true } })
  const config =
    '{\n "invitee": {"currency":"USD", "nanos":"0"}, "\\u0073ignup": {"currency":"E\\u0055R", "nanos":"1"}, "inviter": {"currency":"JPY", "nanos":"9007199254740993"}\n}'
  const user = await renderSettings(config)
  fireEvent.change(await amountInput(), { target: { value: '0.000000002' } })
  await user.click(saveButton())
  await waitFor(() =>
    expect(put).toHaveBeenCalledWith('/api/option/', {
      key: 'reward_setting.config',
      value: JSON.stringify({
        ...initialRewards,
        signup: { nanos: '2', currency: 'EUR' },
      }),
    })
  )
})

it.each(['', 'EU', 'EURO', 'E1R'])(
  'rejects invalid currency %s without defaulting to another currency',
  async (currency) => {
    const put = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    const user = await renderSettings()
    const group = await screen.findByRole('group', { name: 'Signup Reward' })
    const input = within(group).getByRole('combobox', {
      name: 'Reward currency',
    })
    await user.clear(input)
    if (currency) await user.type(input, currency)
    await user.click(saveButton())
    await waitFor(() => expect(input).toHaveAttribute('aria-invalid', 'true'))
    expect(input).toHaveValue(currency)
    expect(put).not.toHaveBeenCalled()
  }
)

it('edits one custom currency without converting the reward amount or changing other currencies', async () => {
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: true } })
  const user = await renderSettings()
  const group = await screen.findByRole('group', { name: 'Inviter Reward' })
  const input = within(group).getByRole('combobox', { name: 'Reward currency' })
  await user.clear(input)
  await user.type(input, 'nzd')
  await user.click(saveButton())
  await waitFor(() =>
    expect(put).toHaveBeenCalledWith('/api/option/', {
      key: 'reward_setting.config',
      value: JSON.stringify({
        ...initialRewards,
        inviter: { nanos: '9007199254740993', currency: 'NZD' },
      }),
    })
  )
  expect(input).toHaveValue('NZD')
})

it('cannot bypass missing compliance by changing only signup when existing invitation rewards are nonzero', async () => {
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: true } })
  const user = await renderSettings(JSON.stringify(initialRewards), false)
  fireEvent.change(await amountInput(), { target: { value: '1' } })
  await user.click(saveButton())
  await waitFor(() =>
    expect(
      screen.getAllByText(
        'Non-zero invitation rewards require compliance confirmation in Payment Gateway settings.'
      )
    ).toHaveLength(2)
  )
  expect(put).not.toHaveBeenCalled()
  fireEvent.change(await amountInput('Inviter Reward'), {
    target: { value: '0' },
  })
  await user.click(saveButton())
  await waitFor(() =>
    expect(put).toHaveBeenCalledWith('/api/option/', {
      key: 'reward_setting.config',
      value: JSON.stringify({
        ...zeroRewards,
        signup: { nanos: '1000000000', currency: 'EUR' },
      }),
    })
  )
})

it('locks amount, currency and save controls during a pending request', async () => {
  let complete: () => void = () => {
    throw new Error('Request not started')
  }
  const put = vi.spyOn(api, 'put').mockImplementationOnce(
    () =>
      new Promise((resolve) => {
        complete = () => resolve({ data: { success: true } })
      })
  )
  const user = await renderSettings()
  fireEvent.change(await amountInput(), { target: { value: '2' } })
  await user.dblClick(saveButton())
  await waitFor(() => expect(put).toHaveBeenCalledTimes(1))
  const saving = screen.getByRole('button', { name: 'Saving...' })
  expect(saving).toBeDisabled()
  for (const label of ['Signup Reward', 'Inviter Reward', 'Invitee Reward']) {
    const group = screen.getByRole('group', { name: label })
    expect(
      within(group).getByRole('textbox', { name: 'Reward amount' })
    ).toBeDisabled()
    expect(
      within(group).getByRole('combobox', { name: 'Reward currency' })
    ).toBeDisabled()
  }
  await user.click(saving)
  expect(put).toHaveBeenCalledTimes(1)
  await act(async () => complete())
  await waitFor(() => expect(saveButton()).toBeDisabled())
  expect(await amountInput()).toBeEnabled()
})
