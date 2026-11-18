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
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { RechargeFormCard } from '../components/recharge-form-card'
import type { TopupInfo } from '../types'

let client: QueryClient
const quote = {
  quote_id: 'quote-exact',
  expires_at: 1_795_000_000,
  charge_amount: { nanos: '18750000001', currency: 'EUR' },
  charge_final: false,
  source_amount: { nanos: '9007199254740993', currency: 'USD' },
  credit_amount: { nanos: '63050394783186951', currency: 'CNY' },
  fx: { from_currency: 'USD', to_currency: 'CNY', rate: '7' },
}
const topupInfo = {
  enable_online_topup: true,
  enable_stripe_topup: false,
  pay_methods: [{ name: 'Bank transfer', type: 'bank' }],
  checkout: {
    providers: [
      {
        provider: 'epay',
        configured: true,
        enabled: true,
        selection_kind: 'amount',
        minimum: { nanos: '1', currency: 'USD' },
        presets: [{ nanos: '9007199254740993', currency: 'USD' }],
        products: [],
      },
    ],
  },
} satisfies TopupInfo

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'] })
  vi.setSystemTime(new Date('2026-11-14T12:00:00Z'))
  useAuthStore.getState().auth.setUser({ id: 1, username: 'buyer', role: 1 })
  client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
})

afterEach(() => {
  cleanup()
  client.clear()
  useAuthStore.getState().auth.reset()
  vi.useRealTimers()
})

function renderCheckout(info: TopupInfo = topupInfo) {
  const props = {
    topupInfo: info,
    redemptionCode: '',
    onRedemptionCodeChange: vi.fn(),
    onRedeem: vi.fn(),
    redeeming: false,
  }
  return render(
    <QueryClientProvider client={client}>
      <RechargeFormCard {...props} />
    </QueryClientProvider>
  )
}

describe('exact checkout confirmation', () => {
  it('discards a late quote after switching accounts and keeps the new form usable', async () => {
    let finish: ((value: unknown) => void) | undefined
    const post = vi.spyOn(api, 'post').mockImplementation(
      () =>
        new Promise((resolve) => {
          finish = resolve
        })
    )
    const user = userEvent.setup()
    renderCheckout()
    await user.click(screen.getByRole('button', { name: 'Review payment' }))
    expect(screen.getByLabelText(/Custom Amount/)).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Loading...' })).toBeDisabled()
    await act(async () => {
      useAuthStore
        .getState()
        .auth.setUser({ id: 2, username: 'other', role: 1 })
    })
    await act(async () => {
      finish?.({ data: { success: true, data: quote } })
    })
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Review payment' })).toBeEnabled()
    expect(post).toHaveBeenCalledTimes(1)
  })

  it('shows an unavailable payment method state without offering an unusable quote action', () => {
    const post = vi.spyOn(api, 'post')
    renderCheckout({ ...topupInfo, pay_methods: [] })
    expect(screen.getByText('No payment methods available')).toBeVisible()
    expect(
      screen.queryByRole('button', { name: 'Review payment' })
    ).not.toBeInTheDocument()
    expect(post).not.toHaveBeenCalled()
  })

  it('quotes the exact source amount and separately shows payment, reward and wallet currencies', async () => {
    const post = vi
      .spyOn(api, 'post')
      .mockResolvedValue({ data: { success: true, data: quote } })
    const user = userEvent.setup()
    renderCheckout()
    await user.clear(screen.getByLabelText(/Custom Amount/))
    await user.type(screen.getByLabelText(/Custom Amount/), '9007199.254740993')
    await user.click(screen.getByRole('button', { name: 'Review payment' }))

    expect(
      await screen.findByRole('alertdialog', { name: 'Confirm Payment' })
    ).toBeVisible()
    expect(post).toHaveBeenCalledWith(
      '/api/user/topup/quote',
      {
        provider: 'epay',
        payment_method: 'bank',
        selection: {
          kind: 'amount',
          source_amount: { nanos: '9007199254740993', currency: 'USD' },
        },
      },
      expect.anything()
    )
    expect(screen.getByText('18.750000001 EUR')).toBeVisible()
    expect(
      screen.getAllByText('9,007,199.254740993 USD').length
    ).toBeGreaterThan(0)
    expect(screen.getByText('63,050,394.783186951 CNY')).toBeVisible()
    expect(
      screen.getByText('Final payment amount is confirmed on the payment page.')
    ).toBeVisible()
  })

  it('creates an order using only the reviewed quote and prevents a duplicate confirmation', async () => {
    let finish: ((value: unknown) => void) | undefined
    const post = vi.spyOn(api, 'post').mockImplementation(async (url) => {
      if (url === '/api/user/topup/quote') {
        return { data: { success: true, data: quote } }
      }
      return new Promise((resolve) => {
        finish = resolve
      })
    })
    vi.spyOn(HTMLFormElement.prototype, 'submit').mockImplementation(
      () => undefined
    )
    const user = userEvent.setup()
    renderCheckout()
    await user.click(screen.getByRole('button', { name: 'Review payment' }))
    await user.click(
      await screen.findByRole('button', { name: 'Confirm Payment' })
    )
    expect(
      screen.getByRole('button', { name: 'Confirm Payment' })
    ).toBeDisabled()
    expect(post).toHaveBeenLastCalledWith(
      '/api/user/pay',
      { quote_id: 'quote-exact' },
      expect.anything()
    )
    expect(
      post.mock.calls.filter(([url]) => url === '/api/user/pay')
    ).toHaveLength(1)
    finish?.({
      data: {
        success: true,
        url: 'https://payments.example.test/checkout',
        data: { order: 'local-order' },
      },
    })
    await waitFor(() =>
      expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    )
  })

  it.each(['business', 'network'])(
    'keeps the exact draft after a %s quote failure and lets the user retry',
    async (failure) => {
      const post = vi.spyOn(api, 'post')
      if (failure === 'network') {
        post.mockRejectedValueOnce(new Error('Quote unavailable'))
      } else {
        post.mockResolvedValueOnce({
          data: { success: false, message: 'Quote unavailable' },
        })
      }
      post.mockResolvedValue({ data: { success: true, data: quote } })
      const user = userEvent.setup()
      renderCheckout()
      const input = screen.getByLabelText(/Custom Amount/)
      await user.clear(input)
      await user.type(input, '0.000000001')
      await user.click(screen.getByRole('button', { name: 'Review payment' }))
      expect(await screen.findByRole('alert')).toHaveTextContent(
        'Quote unavailable'
      )
      expect(input).toHaveValue('0.000000001')
      expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
      await user.click(screen.getByRole('button', { name: 'Review payment' }))
      expect(await screen.findByRole('alertdialog')).toBeVisible()
    }
  )

  it('does not offer a legacy numeric payment path when the provider has no exact configuration', () => {
    const post = vi.spyOn(api, 'post')
    renderCheckout({
      ...topupInfo,
      checkout: {
        providers: [{ ...topupInfo.checkout.providers[0], configured: false }],
      },
    })
    expect(
      screen.queryByRole('button', { name: 'Review payment' })
    ).not.toBeInTheDocument()
    expect(
      screen.getByText(
        'Online topup is not enabled. Please use redemption code or contact administrator.'
      )
    ).toBeVisible()
    expect(post).not.toHaveBeenCalled()
  })

  it.each(['-1', '0', '1e3', '0.0000000001', '9223372036.854775808'])(
    'rejects the invalid amount %s before asking for a quote',
    async (value) => {
      const post = vi.spyOn(api, 'post')
      const user = userEvent.setup()
      renderCheckout()
      const input = screen.getByLabelText(/Custom Amount/)
      await user.clear(input)
      await user.type(input, value)
      await user.click(screen.getByRole('button', { name: 'Review payment' }))
      expect(await screen.findByText('Invalid amount')).toBeVisible()
      expect(input).toHaveAttribute('aria-invalid', 'true')
      expect(post).not.toHaveBeenCalled()
    }
  )

  it('blocks a quote that expires before its first confirmation', async () => {
    const post = vi.spyOn(api, 'post').mockResolvedValue({
      data: {
        success: true,
        data: { ...quote, expires_at: Math.floor(Date.now() / 1000) - 1 },
      },
    })
    const user = userEvent.setup()
    renderCheckout()
    await user.click(screen.getByRole('button', { name: 'Review payment' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Quote expired. Cancel and review the payment again.'
    )
    expect(
      screen.getByRole('button', { name: 'Confirm Payment' })
    ).toBeDisabled()
    expect(
      post.mock.calls.filter(([url]) => url === '/api/user/pay')
    ).toHaveLength(0)
  })

  it('preserves the reviewed quote after an order failure and retries the same id', async () => {
    const post = vi.spyOn(api, 'post').mockImplementation(async (url) => ({
      data:
        url === '/api/user/topup/quote'
          ? { success: true, data: quote }
          : { success: false, message: 'Order unavailable' },
    }))
    const user = userEvent.setup()
    renderCheckout()
    await user.click(screen.getByRole('button', { name: 'Review payment' }))
    await user.click(
      await screen.findByRole('button', { name: 'Confirm Payment' })
    )
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Order unavailable'
    )
    expect(screen.getByRole('alertdialog')).toBeVisible()
    vi.setSystemTime(new Date((quote.expires_at + 1) * 1000))
    await user.click(screen.getByRole('button', { name: 'Confirm Payment' }))
    await waitFor(() =>
      expect(
        post.mock.calls.filter(([url]) => url === '/api/user/pay')
      ).toHaveLength(2)
    )
    expect(
      post.mock.calls.filter(([url]) => url === '/api/user/topup/quote')
    ).toHaveLength(1)
    expect(
      post.mock.calls
        .filter(([url]) => url === '/api/user/pay')
        .map(([, data]) => data)
    ).toEqual([{ quote_id: 'quote-exact' }, { quote_id: 'quote-exact' }])
  })

  it('sends the selected Waffo method in the quote and never in an amount-based create request', async () => {
    const post = vi
      .spyOn(api, 'post')
      .mockResolvedValue({ data: { success: true, data: quote } })
    const user = userEvent.setup()
    renderCheckout({
      ...topupInfo,
      waffo_pay_methods: [{ name: 'Card A' }, { name: 'Card B' }],
      checkout: {
        providers: [{ ...topupInfo.checkout.providers[0], provider: 'waffo' }],
      },
    })
    await user.click(screen.getByRole('combobox', { name: 'Payment Method' }))
    await user.click(await screen.findByRole('option', { name: 'Card B' }))
    await user.click(screen.getByRole('button', { name: 'Review payment' }))
    expect(await screen.findByRole('alertdialog')).toHaveTextContent(
      'Waffo · Card B'
    )
    expect(post).toHaveBeenCalledWith(
      '/api/user/topup/quote',
      expect.objectContaining({ provider: 'waffo', pay_method_index: 1 }),
      expect.anything()
    )
  })

  it('quotes Stripe by its external product and exact integer quantity', async () => {
    const post = vi
      .spyOn(api, 'post')
      .mockResolvedValue({ data: { success: true, data: quote } })
    const user = userEvent.setup()
    renderCheckout({
      ...topupInfo,
      checkout: {
        providers: [
          {
            provider: 'stripe',
            configured: true,
            enabled: true,
            selection_kind: 'product',
            presets: [],
            products: [
              {
                product_id: 'price-example',
                name: 'Wallet credit',
                source_amount: { nanos: '1000000001', currency: 'USD' },
              },
            ],
          },
        ],
      },
    })
    const quantity = screen.getByRole('textbox', { name: 'Quantity' })
    await user.clear(quantity)
    await user.type(quantity, '3')
    await user.click(screen.getByRole('button', { name: 'Review payment' }))
    await screen.findByRole('alertdialog')
    expect(post).toHaveBeenCalledWith(
      '/api/user/topup/quote',
      {
        provider: 'stripe',
        selection: {
          kind: 'product',
          product_id: 'price-example',
          quantity: '3',
        },
      },
      expect.anything()
    )
  })
})
