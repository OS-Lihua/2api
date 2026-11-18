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
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { toast } from 'sonner'
import { afterEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import { SettingsPageProvider } from '../../components/settings-page-context'
import { CheckinSettingsSection } from '../checkin-settings-section'

const clients: QueryClient[] = []
const rewardRange = JSON.stringify({
  min: { nanos: '2000000', currency: 'USD' },
  max: { nanos: '20000000', currency: 'USD' },
})

function Fixture(props: { rewardRange: string }) {
  const [container, setContainer] = useState<HTMLDivElement | null>(null)
  return (
    <>
      <div ref={setContainer} />
      <SettingsPageProvider actionsContainer={container}>
        <CheckinSettingsSection
          defaultValues={{ enabled: true, rewardRange: props.rewardRange }}
        />
      </SettingsPageProvider>
    </>
  )
}

function renderSettings(range = rewardRange) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  clients.push(client)
  return render(
    <QueryClientProvider client={client}>
      <Fixture rewardRange={range} />
    </QueryClientProvider>
  )
}

afterEach(() => {
  cleanup()
  clients.splice(0).forEach((client) => client.clear())
  vi.restoreAllMocks()
})

it('edits exact decimal rewards and saves both bounds in one atomic money option', async () => {
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: true } })
  renderSettings()
  const min = screen.getByRole('textbox', { name: 'Minimum check-in reward' })
  const max = screen.getByRole('textbox', { name: 'Maximum check-in reward' })
  expect(min).toHaveValue('0.002')
  expect(max).toHaveValue('0.02')
  await userEvent.clear(min)
  await userEvent.type(min, '0.000000001')
  await userEvent.clear(max)
  await userEvent.type(max, '9007199.254740993')
  await userEvent.click(
    screen.getByRole('button', { name: 'Save check-in settings' })
  )
  await waitFor(() =>
    expect(put).toHaveBeenCalledWith('/api/option/', {
      key: 'checkin_setting.reward_range',
      value: JSON.stringify({
        min: { nanos: '1', currency: 'USD' },
        max: { nanos: '9007199254740993', currency: 'USD' },
      }),
    })
  )
  expect(put).toHaveBeenCalledTimes(1)
  expect(
    screen.getByRole('button', { name: 'Save check-in settings' })
  ).toBeDisabled()
})

it.each([
  ['-0.000000001', '0.02', 'Minimum check-in reward'],
  ['1e3', '0.02', 'Minimum check-in reward'],
  ['0.0000000001', '0.02', 'Minimum check-in reward'],
  ['0', '9223372036.854775808', 'Maximum check-in reward'],
  ['0.020000001', '0.02', 'Maximum check-in reward'],
])(
  'rejects the invalid reward range %s to %s without writing options',
  async (min, max, invalidField) => {
    const put = vi.spyOn(api, 'put')
    renderSettings()
    fireEvent.change(
      screen.getByRole('textbox', { name: 'Minimum check-in reward' }),
      { target: { value: min } }
    )
    fireEvent.change(
      screen.getByRole('textbox', { name: 'Maximum check-in reward' }),
      { target: { value: max } }
    )
    await userEvent.click(
      screen.getByRole('button', { name: 'Save check-in settings' })
    )
    await waitFor(() =>
      expect(
        screen.getByRole('textbox', { name: invalidField })
      ).toHaveAttribute('aria-invalid', 'true')
    )
    expect(put).not.toHaveBeenCalled()
  }
)

it('allows a zero reward without coercing it to a positive minimum', async () => {
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: true } })
  renderSettings()
  for (const name of ['Minimum check-in reward', 'Maximum check-in reward']) {
    fireEvent.change(screen.getByRole('textbox', { name }), {
      target: { value: '0' },
    })
  }
  await userEvent.click(
    screen.getByRole('button', { name: 'Save check-in settings' })
  )
  await waitFor(() =>
    expect(put).toHaveBeenCalledWith('/api/option/', {
      key: 'checkin_setting.reward_range',
      value: JSON.stringify({
        min: { nanos: '0', currency: 'USD' },
        max: { nanos: '0', currency: 'USD' },
      }),
    })
  )
})

it('uses one selected currency for both reward amounts', async () => {
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: true } })
  renderSettings()
  const currency = screen.getByRole('combobox', { name: 'Reward currency' })
  await userEvent.clear(currency)
  await userEvent.type(currency, 'EUR')
  await userEvent.tab()
  await userEvent.click(
    screen.getByRole('button', { name: 'Save check-in settings' })
  )
  await waitFor(() =>
    expect(put).toHaveBeenCalledWith('/api/option/', {
      key: 'checkin_setting.reward_range',
      value: JSON.stringify({
        min: { nanos: '2000000', currency: 'EUR' },
        max: { nanos: '20000000', currency: 'EUR' },
      }),
    })
  )
})

it.each(['business', 'network'])(
  'retains the edited range after a %s save failure and permits retry',
  async (failure) => {
    const put = vi.spyOn(api, 'put')
    if (failure === 'business') {
      put.mockResolvedValueOnce({
        data: { success: false, message: 'Invalid amount' },
      })
    } else put.mockRejectedValueOnce(new Error('Network unavailable'))
    put.mockResolvedValue({ data: { success: true } })
    const success = vi.spyOn(toast, 'success')
    renderSettings()
    const min = screen.getByRole('textbox', { name: 'Minimum check-in reward' })
    fireEvent.change(min, { target: { value: '0.000000001' } })
    await userEvent.click(
      screen.getByRole('button', { name: 'Save check-in settings' })
    )
    expect(await screen.findByRole('alert')).toHaveTextContent(
      failure === 'business' ? 'Invalid amount' : 'Network unavailable'
    )
    expect(min).toHaveValue('0.000000001')
    expect(success).not.toHaveBeenCalled()
    await userEvent.click(
      screen.getByRole('button', { name: 'Save check-in settings' })
    )
    await waitFor(() => expect(success).toHaveBeenCalled())
    expect(put).toHaveBeenCalledTimes(2)
  }
)

it('disables reward inputs and saving while a range update is pending', async () => {
  let resolve!: (value: { data: { success: boolean } }) => void
  const put = vi.spyOn(api, 'put').mockImplementation(
    () =>
      new Promise((done) => {
        resolve = done
      })
  )
  renderSettings()
  const min = screen.getByRole('textbox', { name: 'Minimum check-in reward' })
  fireEvent.change(min, { target: { value: '0.000000001' } })
  await userEvent.click(
    screen.getByRole('button', { name: 'Save check-in settings' })
  )
  expect(min).toBeDisabled()
  expect(
    screen.getByRole('textbox', { name: 'Maximum check-in reward' })
  ).toBeDisabled()
  const save = screen.getByRole('button', { name: 'Saving...' })
  expect(save).toBeDisabled()
  await userEvent.click(save)
  expect(put).toHaveBeenCalledTimes(1)
  resolve({ data: { success: true } })
  await waitFor(() =>
    expect(
      screen.getByRole('button', { name: 'Save check-in settings' })
    ).toBeDisabled()
  )
})

it('does not offer saving when the persisted range is invalid or uses mismatched currencies', () => {
  renderSettings(
    JSON.stringify({
      min: { nanos: '1', currency: 'USD' },
      max: { nanos: '2', currency: 'EUR' },
    })
  )
  expect(screen.getByText('Invalid check-in reward range')).toBeInTheDocument()
  expect(screen.queryByRole('textbox')).not.toBeInTheDocument()
  expect(
    screen.queryByRole('button', { name: 'Save check-in settings' })
  ).not.toBeInTheDocument()
})
