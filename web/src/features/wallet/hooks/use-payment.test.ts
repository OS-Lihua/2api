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
import { act, cleanup, renderHook } from '@testing-library/react'
import { createElement, type ReactNode } from 'react'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { requestCheckoutQuote } from '../api'
import type { CheckoutProvider } from '../types'
import { usePayment } from './use-payment'

let client: QueryClient
beforeEach(() => {
  client = new QueryClient({ defaultOptions: { mutations: { retry: false } } })
  useAuthStore.getState().auth.setUser({ id: 1, username: 'buyer', role: 1 })
})
afterEach(() => {
  cleanup()
  client.clear()
  useAuthStore.getState().auth.reset()
})
function Wrapper(props: { children: ReactNode }) {
  return createElement(QueryClientProvider, { client }, props.children)
}

describe('payment amount routing', () => {
  test('quotes Waffo through the shared money API without losing the source amount', async () => {
    const quote = {
      quote_id: 'waffo-quote',
      expires_at: 2_000_000_000,
      charge_amount: { nanos: '18750000001', currency: 'EUR' },
      charge_final: true,
      source_amount: { nanos: '9007199254740993', currency: 'USD' },
      credit_amount: { nanos: '9007199254740993', currency: 'USD' },
      fx: null,
    }
    const post = vi
      .spyOn(api, 'post')
      .mockResolvedValue({ data: { success: true, data: quote } })
    expect(
      await requestCheckoutQuote({
        provider: 'waffo',
        pay_method_index: 0,
        selection: { kind: 'amount', source_amount: quote.source_amount },
      })
    ).toEqual(quote)
    expect(post).toHaveBeenCalledExactlyOnceWith(
      '/api/user/topup/quote',
      {
        provider: 'waffo',
        pay_method_index: 0,
        selection: { kind: 'amount', source_amount: quote.source_amount },
      },
      expect.anything()
    )
  })

  test.each<CheckoutProvider>([
    'epay',
    'stripe',
    'creem',
    'waffo',
    'waffo_pancake',
  ])('rejects a non-http redirect returned by %s', async (provider) => {
    const unsafe = 'javascript:alert(1)'
    vi.spyOn(api, 'post').mockResolvedValue({
      data: {
        success: true,
        url: unsafe,
        data: { pay_link: unsafe, checkout_url: unsafe, payment_url: unsafe },
      },
    })
    const submit = vi
      .spyOn(HTMLFormElement.prototype, 'submit')
      .mockImplementation(() => undefined)
    const { result } = renderHook(() => usePayment(provider, 1), {
      wrapper: Wrapper,
    })
    await act(async () => {
      await expect(
        result.current.mutateAsync('reviewed-quote')
      ).rejects.toThrow('Invalid payment redirect URL')
    })
    expect(submit).not.toHaveBeenCalled()
  })

  test('discards a late payment redirect after the current account changes', async () => {
    let finish: ((value: unknown) => void) | undefined
    vi.spyOn(api, 'post').mockImplementation(
      () =>
        new Promise((resolve) => {
          finish = resolve
        })
    )
    const submit = vi
      .spyOn(HTMLFormElement.prototype, 'submit')
      .mockImplementation(() => undefined)
    const { result } = renderHook(() => usePayment('epay', 1), {
      wrapper: Wrapper,
    })
    let pending: Promise<boolean> | undefined
    await act(async () => {
      pending = result.current.mutateAsync('accepted-quote')
    })
    await act(async () => {
      useAuthStore
        .getState()
        .auth.setUser({ id: 2, username: 'other-buyer', role: 1 })
    })
    await act(async () => {
      finish?.({
        data: {
          success: true,
          url: 'https://payments.example.test/checkout',
          data: { order: 'accepted-order' },
        },
      })
      expect(await pending).toBe(false)
    })
    expect(submit).not.toHaveBeenCalled()
  })

  test('does not create or redirect a payment after the current account changes', async () => {
    const post = vi.spyOn(api, 'post')
    const { result } = renderHook(() => usePayment('epay', 1), {
      wrapper: Wrapper,
    })
    useAuthStore
      .getState()
      .auth.setUser({ id: 2, username: 'other-buyer', role: 1 })
    await act(async () => {
      expect(await result.current.mutateAsync('old-account-quote')).toBe(false)
    })
    expect(post).not.toHaveBeenCalled()
  })
})
