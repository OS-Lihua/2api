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
import { cleanup, renderHook, waitFor } from '@testing-library/react'
import { createElement, type ReactNode } from 'react'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { useTopupInfo } from '../use-topup-info'

let client: QueryClient
const info = {
  enable_online_topup: false,
  enable_stripe_topup: false,
  pay_methods: [],
  waffo_pay_methods: [{ name: 'Card A' }, { name: 'Card B' }],
  checkout: {
    providers: [
      {
        provider: 'waffo',
        configured: true,
        enabled: true,
        selection_kind: 'amount',
        minimum: { nanos: '1', currency: 'USD' },
        presets: [{ nanos: '9007199254740993', currency: 'USD' }],
        products: [],
      },
    ],
  },
}
beforeEach(() => {
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
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

it('keeps exact presets and the original Waffo payment method ordering', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: info },
  })
  const { result } = renderHook(() => useTopupInfo(), { wrapper: Wrapper })
  await waitFor(() => expect(result.current.loading).toBe(false))
  expect(result.current.topupInfo?.checkout?.providers[0].presets).toEqual([
    { nanos: '9007199254740993', currency: 'USD' },
  ])
  expect(
    result.current.topupInfo?.waffo_pay_methods?.map((method) => method.name)
  ).toEqual(['Card A', 'Card B'])
})

it.each([{}, null])(
  'rejects malformed Waffo methods rather than shifting the selected gateway index (%j)',
  async (malformed) => {
    vi.spyOn(api, 'get').mockResolvedValue({
      data: {
        success: true,
        data: { ...info, waffo_pay_methods: [malformed, { name: 'Card B' }] },
      },
    })
    const { result } = renderHook(() => useTopupInfo(), { wrapper: Wrapper })
    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current.topupInfo).toBeNull()
    expect(result.current.error?.message).toBe(
      'Payment configuration is unavailable'
    )
  }
)
