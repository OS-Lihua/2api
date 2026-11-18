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
import { describe, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'

import { requestCheckoutPayment, requestCheckoutQuote } from '../api'
import type { CheckoutProvider, CheckoutQuote } from '../types'

const quote: CheckoutQuote = {
  quote_id: 'method-quote',
  expires_at: 2_000_000_000,
  charge_amount: { nanos: '18750000000', currency: 'USD' },
  charge_final: true,
  source_amount: { nanos: '120000000000', currency: 'USD' },
  credit_amount: { nanos: '120000000000', currency: 'USD' },
  fx: null,
}

describe('payment type classification', () => {
  test.each<[CheckoutProvider, string]>([
    ['epay', '/api/user/pay'],
    ['stripe', '/api/user/stripe/pay'],
    ['creem', '/api/user/creem/pay'],
    ['waffo', '/api/user/waffo/pay'],
    ['waffo_pancake', '/api/user/waffo-pancake/pay'],
  ])('keeps %s on its dedicated order endpoint', async (provider, path) => {
    const post = vi
      .spyOn(api, 'post')
      .mockResolvedValue({ data: { success: true } })
    await requestCheckoutPayment(provider, 'reviewed-quote')
    expect(post).toHaveBeenCalledExactlyOnceWith(
      path,
      { quote_id: 'reviewed-quote' },
      expect.anything()
    )
  })
})

describe('payment dispatch', () => {
  test.each([
    { ...quote, source_amount: { ...quote.source_amount, nanos: 'invalid' } },
    { ...quote, charge_amount: { ...quote.charge_amount, nanos: '-1' } },
    {
      ...quote,
      credit_amount: { ...quote.credit_amount, currency: 'EUR' },
      fx: { from_currency: 'EUR', to_currency: 'USD', rate: '1.1' },
    },
  ])('rejects a quote with invalid recorded money or FX', async (invalid) => {
    vi.spyOn(api, 'post').mockResolvedValue({
      data: { success: true, data: invalid },
    })
    await expect(
      requestCheckoutQuote({
        provider: 'waffo',
        pay_method_index: 0,
        selection: { kind: 'amount', source_amount: quote.source_amount },
      })
    ).rejects.toThrow('Quote unavailable')
  })

  test('keeps the selected Waffo method index in its quote through confirmation', async () => {
    const post = vi
      .spyOn(api, 'post')
      .mockResolvedValueOnce({ data: { success: true, data: quote } })
      .mockResolvedValueOnce({ data: { success: true } })
    const reviewed = await requestCheckoutQuote({
      provider: 'waffo',
      pay_method_index: 3,
      selection: {
        kind: 'amount',
        source_amount: { nanos: '120000000000', currency: 'USD' },
      },
    })
    await requestCheckoutPayment('waffo', reviewed.quote_id)
    expect(post).toHaveBeenNthCalledWith(
      1,
      '/api/user/topup/quote',
      {
        provider: 'waffo',
        pay_method_index: 3,
        selection: {
          kind: 'amount',
          source_amount: { nanos: '120000000000', currency: 'USD' },
        },
      },
      expect.anything()
    )
    expect(post).toHaveBeenNthCalledWith(
      2,
      '/api/user/waffo/pay',
      { quote_id: 'method-quote' },
      expect.anything()
    )
  })

  test('does not create a Waffo quote without a selected method index', async () => {
    const post = vi.spyOn(api, 'post')
    await expect(
      requestCheckoutQuote({
        provider: 'waffo',
        selection: {
          kind: 'amount',
          source_amount: { nanos: '120000000000', currency: 'USD' },
        },
      })
    ).rejects.toThrow('Select a payment method')
    expect(post).not.toHaveBeenCalled()
  })
})
