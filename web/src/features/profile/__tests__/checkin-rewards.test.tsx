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
import { act, cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { toast } from 'sonner'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import en from '@/i18n/locales/en.json'
import fr from '@/i18n/locales/fr.json'
import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { CheckinCalendarCard } from '../components/checkin-calendar-card'

const status = {
  enabled: true,
  reward_range: {
    min: { nanos: '2000000', currency: 'USD' },
    max: { nanos: '20000000', currency: 'USD' },
  },
  stats: {
    checked_in_today: false,
    total_checkins: 5,
    checkin_count: 4,
    records: [
      {
        checkin_date: '2026-11-01',
        amount: { nanos: '9223372036854775807', currency: 'USD' },
      },
      {
        checkin_date: '2026-11-02',
        amount: { nanos: '9223372036854775807', currency: 'USD' },
      },
      { checkin_date: '2026-11-03', amount: { nanos: '1', currency: 'EUR' } },
      { checkin_date: '2026-11-04', amount: { nanos: '-1', currency: 'USD' } },
    ],
    total_awarded: [
      { nanos: '28446744073709551613', currency: 'USD' },
      { nanos: '2', currency: 'EUR' },
    ],
  },
}
const clients: QueryClient[] = []
const i18n = createInstance()
await i18n.init({ lng: 'en', resources: { en, fr }, initAsync: false })

function renderCheckin(
  options = {
    checkinEnabled: true,
    turnstileEnabled: false,
    turnstileSiteKey: '',
  }
) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.push(client)
  const view = render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <CheckinCalendarCard {...options} />
      </QueryClientProvider>
    </I18nextProvider>
  )
  return { client, ...view }
}

beforeEach(async () => {
  vi.useFakeTimers({ toFake: ['Date'] })
  vi.setSystemTime(new Date('2026-11-14T12:00:00Z'))
  await i18n.changeLanguage('en')
  useAuthStore
    .getState()
    .auth.setUser({ id: 1, username: 'checkin-user', role: 1 })
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: status },
  })
})
afterEach(() => {
  cleanup()
  clients.splice(0).forEach((client) => client.clear())
  useAuthStore.getState().auth.reset()
  vi.restoreAllMocks()
  vi.useRealTimers()
})

it('shows exact monthly and lifetime rewards separately by currency beyond the int64 total limit', async () => {
  renderCheckin()
  const monthly = await screen.findByText('18,446,744,073.709551613 USD')
  expect(monthly).toHaveClass('wrap-anywhere')
  expect(screen.getByText('Total check-ins').closest('.grid')).toHaveClass(
    'grid-cols-1',
    'sm:grid-cols-3'
  )
  expect(screen.getByText('28,446,744,073.709551613 USD')).toBeInTheDocument()
  expect(screen.getByText('0.000000001 EUR')).toBeInTheDocument()
  expect(screen.getByText('0.000000002 EUR')).toBeInTheDocument()
  expect(api.get).toHaveBeenCalledWith(
    '/api/user/checkin?month=2026-11',
    expect.objectContaining({ signal: expect.any(AbortSignal) })
  )
  expect(screen.queryByText(/random quota rewards/)).not.toBeInTheDocument()
})

it('keeps a zero reward checked in and supports keyboard expansion of the calendar', async () => {
  vi.mocked(api.get).mockResolvedValue({
    data: {
      success: true,
      data: {
        ...status,
        stats: {
          ...status.stats,
          checked_in_today: true,
          records: [
            {
              checkin_date: '2026-11-14',
              amount: { nanos: '0', currency: 'EUR' },
            },
          ],
          total_awarded: [{ nanos: '0', currency: 'EUR' }],
        },
      },
    },
  })
  renderCheckin()
  const header = await screen.findByRole('button', { name: /Daily Check-in/ })
  expect(header).toHaveAttribute('aria-expanded', 'false')
  expect(header).toHaveTextContent('Today 0 EUR')
  expect(screen.getByRole('button', { name: 'Checked in' })).toBeDisabled()
  header.focus()
  await userEvent.keyboard('{Enter}')
  expect(header).toHaveAttribute('aria-expanded', 'true')
  expect(
    screen.getByText('You can only check in once per day')
  ).toBeInTheDocument()
})

it('reports the actual credited amount and refreshes only the current user wallet after success', async () => {
  vi.spyOn(api, 'post').mockResolvedValue({
    data: {
      success: true,
      data: {
        amount: { nanos: '10000001', currency: 'EUR' },
        checkin_date: '2026-11-14',
      },
    },
  })
  const success = vi.spyOn(toast, 'success')
  const { client } = renderCheckin()
  client.setQueryData(['self-wallet', 1], { balance: '1', currency: 'EUR' })
  client.setQueryData(['self-wallet', 2], { balance: '2', currency: 'EUR' })
  await userEvent.click(
    await screen.findByRole('button', { name: 'Check in now' })
  )
  await waitFor(() =>
    expect(success).toHaveBeenCalledWith(
      'Check-in successful! Received 0.010000001 EUR'
    )
  )
  expect(api.post).toHaveBeenCalledWith('/api/user/checkin')
  expect(client.getQueryState(['self-wallet', 1])?.isInvalidated).toBe(true)
  expect(client.getQueryState(['self-wallet', 2])?.isInvalidated).toBe(false)
})

it.each(['business', 'network'])(
  'keeps check-in available after a %s failure without reporting a credit',
  async (failure) => {
    const post = vi.spyOn(api, 'post')
    if (failure === 'business') {
      post.mockResolvedValueOnce({
        data: { success: false, message: 'Check-in failed' },
      })
    } else post.mockRejectedValueOnce(new Error('Check-in failed'))
    post.mockResolvedValue({
      data: {
        success: true,
        data: {
          amount: { nanos: '1', currency: 'EUR' },
          checkin_date: '2026-11-14',
        },
      },
    })
    const success = vi.spyOn(toast, 'success')
    const error = vi.spyOn(toast, 'error')
    renderCheckin()
    const submit = await screen.findByRole('button', { name: 'Check in now' })
    await userEvent.click(submit)
    await waitFor(() => expect(error).toHaveBeenCalledWith('Check-in failed'))
    expect(success).not.toHaveBeenCalled()
    expect(submit).toBeEnabled()
    await userEvent.click(submit)
    await waitFor(() =>
      expect(success).toHaveBeenCalledWith(
        'Check-in successful! Received 0.000000001 EUR'
      )
    )
  }
)

it('shows an unavailable status instead of zero rewards and allows a retry', async () => {
  vi.mocked(api.get).mockResolvedValueOnce({
    data: { success: false, message: 'Check-in unavailable' },
  })
  renderCheckin()
  expect(await screen.findByText('Check-in unavailable')).toBeInTheDocument()
  expect(screen.queryByText('Total earned')).not.toBeInTheDocument()
  expect(
    screen.queryByRole('button', { name: 'Check in now' })
  ).not.toBeInTheDocument()
  await userEvent.click(screen.getByRole('button', { name: 'Retry' }))
  expect(
    await screen.findByText('28,446,744,073.709551613 USD')
  ).toBeInTheDocument()
})

it('does not display cached rewards or a late response from the previous signed-in user', async () => {
  let resolve!: (value: {
    data: { success: boolean; data: typeof status }
  }) => void
  vi.mocked(api.get).mockImplementationOnce(
    () =>
      new Promise((done) => {
        resolve = done
      })
  )
  const { client } = renderCheckin()
  client.setQueryData(['checkin-status', 1, '2026-11'], status)
  const nextStatus = {
    ...status,
    stats: {
      ...status.stats,
      records: [],
      total_awarded: [{ nanos: '3000000000', currency: 'GBP' }],
    },
  }
  vi.mocked(api.get).mockResolvedValue({
    data: { success: true, data: nextStatus },
  })
  await act(async () => {
    useAuthStore
      .getState()
      .auth.setUser({ id: 2, username: 'next-checkin-user', role: 1 })
  })
  expect(await screen.findByText('3 GBP')).toBeInTheDocument()
  await act(async () => resolve({ data: { success: true, data: status } }))
  expect(screen.getByText('3 GBP')).toBeInTheDocument()
  expect(
    screen.queryByText('28,446,744,073.709551613 USD')
  ).not.toBeInTheDocument()
})

it('updates exact reward formatting when the interface language changes', async () => {
  renderCheckin()
  await screen.findByText('28,446,744,073.709551613 USD')
  await act(async () => {
    await i18n.changeLanguage('fr')
  })
  expect(
    screen.getByText('28 446 744 073,709551613 USD', {
      normalizer: (text) => text,
    })
  ).toBeInTheDocument()
})

it('disables check-in while the credit is pending and prevents duplicate submissions', async () => {
  let resolve!: (value: {
    data: {
      success: boolean
      data: {
        amount: { nanos: string; currency: string }
        checkin_date: string
      }
    }
  }) => void
  const post = vi.spyOn(api, 'post').mockImplementation(
    () =>
      new Promise((done) => {
        resolve = done
      })
  )
  renderCheckin()
  await userEvent.click(
    await screen.findByRole('button', { name: 'Check in now' })
  )
  const pending = screen.getByRole('button', { name: 'Loading...' })
  expect(pending).toBeDisabled()
  await userEvent.click(pending)
  expect(post).toHaveBeenCalledTimes(1)
  await act(async () =>
    resolve({
      data: {
        success: true,
        data: {
          amount: { nanos: '1', currency: 'EUR' },
          checkin_date: '2026-11-14',
        },
      },
    })
  )
})

it('does not announce a previous user credit or invalidate the next user wallet after an account switch', async () => {
  let resolve!: (value: {
    data: {
      success: boolean
      data: {
        amount: { nanos: string; currency: string }
        checkin_date: string
      }
    }
  }) => void
  vi.spyOn(api, 'post').mockImplementation(
    () =>
      new Promise((done) => {
        resolve = done
      })
  )
  const success = vi.spyOn(toast, 'success')
  const { client } = renderCheckin()
  client.setQueryData(['self-wallet', 2], { balance: '2', currency: 'GBP' })
  await userEvent.click(
    await screen.findByRole('button', { name: 'Check in now' })
  )
  await act(async () => {
    useAuthStore
      .getState()
      .auth.setUser({ id: 2, username: 'next-checkin-user', role: 1 })
  })
  await screen.findByRole('button', { name: 'Check in now' })
  await act(async () =>
    resolve({
      data: {
        success: true,
        data: {
          amount: { nanos: '1', currency: 'EUR' },
          checkin_date: '2026-11-14',
        },
      },
    })
  )
  expect(success).not.toHaveBeenCalled()
  expect(client.getQueryState(['self-wallet', 2])?.isInvalidated).toBe(false)
})

it('does not load or offer check-in when the feature is disabled', () => {
  renderCheckin({
    checkinEnabled: false,
    turnstileEnabled: false,
    turnstileSiteKey: '',
  })
  expect(api.get).not.toHaveBeenCalled()
  expect(screen.queryByRole('button')).not.toBeInTheDocument()
})
