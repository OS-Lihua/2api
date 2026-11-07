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
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { afterEach, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'

import { SettingsPageProvider } from '../../components/settings-page-context'
import {
  getOptionValue,
  useSystemOptions,
} from '../../hooks/use-system-options'
import { GatewayBudgetSettings } from '../gateway-budget-settings'

const defaults = {
  GatewayMaxAttempts: 3,
  GatewayTotalFirstByteSeconds: 90,
  GatewayAttemptFirstByteSeconds: 30,
}

function LiveSettings() {
  const options = useSystemOptions()
  return (
    <GatewayBudgetSettings
      defaultValues={getOptionValue(options.data?.data, defaults)}
    />
  )
}

function Fixture(props: { live?: boolean }) {
  const [container, setContainer] = useState<HTMLDivElement | null>(null)
  return (
    <>
      <div ref={setContainer} />
      <SettingsPageProvider actionsContainer={container}>
        {props.live ? (
          <LiveSettings />
        ) : (
          <GatewayBudgetSettings defaultValues={defaults} />
        )}
      </SettingsPageProvider>
    </>
  )
}

function renderSettings(live = false) {
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false } },
  })
  render(
    <QueryClientProvider client={client}>
      <Fixture live={live} />
    </QueryClientProvider>
  )
}

afterEach(() => vi.restoreAllMocks())

test('shows effective budgets and saves edited whole seconds through the settings API', async () => {
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: true } })
  const user = userEvent.setup()
  renderSettings()
  expect(
    screen.getByRole('spinbutton', { name: 'Maximum attempts' })
  ).toHaveValue(3)
  expect(
    screen.getByRole('spinbutton', {
      name: 'Total first-byte budget (seconds)',
    })
  ).toHaveValue(90)
  const attempt = screen.getByRole('spinbutton', {
    name: 'Attempt first-byte budget (seconds)',
  })
  expect(attempt).toHaveValue(30)
  fireEvent.change(attempt, { target: { value: '12' } })
  await user.click(screen.getByRole('button', { name: 'Save Changes' }))
  await waitFor(() =>
    expect(put).toHaveBeenCalledWith('/api/option/', {
      key: 'GatewayAttemptFirstByteSeconds',
      value: 12,
    })
  )
  expect(put).toHaveBeenCalledTimes(1)
})

test('rejects fractional or out-of-range budgets before saving', async () => {
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: true } })
  const user = userEvent.setup()
  renderSettings()
  const input = screen.getByRole('spinbutton', { name: 'Maximum attempts' })
  fireEvent.change(input, { target: { value: '1.5' } })
  await user.click(screen.getByRole('button', { name: 'Save Changes' }))
  expect(
    await screen.findByText(
      'Maximum attempts must be an integer between 1 and 5'
    )
  ).toBeVisible()
  expect(input).toHaveAttribute('aria-invalid', 'true')
  expect(put).not.toHaveBeenCalled()
})

test('preserves unsaved fields when a partial save refreshes options before the next write fails', async () => {
  const saved = { ...defaults }
  const get = vi
    .spyOn(api, 'get')
    .mockImplementation(async () => ({
      data: {
        success: true,
        data: Object.entries(saved).map(([key, value]) => ({
          key,
          value: String(value),
        })),
      },
    }))
  let failSecond: (() => void) | undefined
  const put = vi
    .spyOn(api, 'put')
    .mockImplementationOnce(async () => {
      saved.GatewayMaxAttempts = 5
      return { data: { success: true } }
    })
    .mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          failSecond = () =>
            resolve({ data: { success: false, message: 'Save failed' } })
        })
    )
    .mockResolvedValue({ data: { success: true } })
  const user = userEvent.setup()
  renderSettings(true)
  await waitFor(() => expect(get).toHaveBeenCalledTimes(1))
  fireEvent.change(
    screen.getByRole('spinbutton', { name: 'Maximum attempts' }),
    { target: { value: '5' } }
  )
  const total = screen.getByRole('spinbutton', {
    name: 'Total first-byte budget (seconds)',
  })
  fireEvent.change(total, { target: { value: '61' } })
  await user.click(screen.getByRole('button', { name: 'Save Changes' }))
  await waitFor(() => expect(get).toHaveBeenCalledTimes(2))
  await waitFor(() => expect(put).toHaveBeenCalledTimes(2))
  failSecond?.()
  await waitFor(() =>
    expect(screen.getByRole('button', { name: 'Save Changes' })).toBeEnabled()
  )
  expect(total).toHaveValue(61)
  await user.click(screen.getByRole('button', { name: 'Save Changes' }))
  await waitFor(() =>
    expect(put).toHaveBeenLastCalledWith('/api/option/', {
      key: 'GatewayTotalFirstByteSeconds',
      value: 61,
    })
  )
})

test('keeps edited values after a failed save so the administrator can retry', async () => {
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValueOnce({ data: { success: false, message: 'Save failed' } })
    .mockResolvedValue({ data: { success: true } })
  const user = userEvent.setup()
  renderSettings()
  const input = screen.getByRole('spinbutton', { name: 'Maximum attempts' })
  fireEvent.change(input, { target: { value: '5' } })
  await user.click(screen.getByRole('button', { name: 'Save Changes' }))
  await waitFor(() => expect(put).toHaveBeenCalledTimes(1))
  expect(input).toHaveValue(5)
  await waitFor(() =>
    expect(screen.getByRole('button', { name: 'Save Changes' })).toBeEnabled()
  )
  await user.click(screen.getByRole('button', { name: 'Save Changes' }))
  await waitFor(() => expect(put).toHaveBeenCalledTimes(2))
})
