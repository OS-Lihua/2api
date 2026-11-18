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
import { act, cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createInstance } from 'i18next'
import { I18nextProvider, initReactI18next } from 'react-i18next'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import { BillingHistoryDialog } from '@/features/wallet/components/dialogs/billing-history-dialog'
import { TransferDialog } from '@/features/wallet/components/dialogs/transfer-dialog'
import en from '@/i18n/locales/en.json'
import zh from '@/i18n/locales/zh.json'
import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { RechargeFormCard } from '../components/recharge-form-card'
import type { TopupInfo } from '../types'

const i18n = createInstance()
let client: QueryClient

beforeEach(async () => {
  client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  useAuthStore.getState().auth.setUser({ id: 1, username: 'buyer', role: 1 })
  await i18n.use(initReactI18next).init({
    lng: 'zh',
    fallbackLng: false,
    nsSeparator: false,
    resources: { en, zh },
    interpolation: { escapeValue: false },
  })
})

afterEach(() => {
  cleanup()
  client.clear()
  useAuthStore.getState().auth.reset()
})

it('translates the debt-repayment notice in the transfer dialog when the language changes', async () => {
  render(
    <I18nextProvider i18n={i18n}>
      <TransferDialog
        open
        onOpenChange={vi.fn()}
        onConfirm={vi.fn().mockResolvedValue(true)}
        available={{ nanos: '9', currency: 'USD' }}
        attempt={null}
        transferring={false}
      />
    </I18nextProvider>
  )
  expect(await screen.findByRole('dialog')).toHaveAccessibleDescription(
    '钱包入账金额会先偿还欠款。'
  )
  await act(() => i18n.changeLanguage('en'))
  expect(screen.getByRole('dialog')).toHaveAccessibleDescription(
    'Wallet credits first repay outstanding debt.'
  )
})

it('translates billing history statuses and updates them when the language changes', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        total: 3,
        items: ['success', 'pending', 'expired'].map((status, id) => ({
          id,
          status,
          contract_version: 0,
          purpose: 'legacy_topup',
          legacy_amount: '10',
          legacy_money: '10',
          trade_no: `order-${id}`,
          payment_method: 'stripe',
          create_time: 1,
        })),
      },
    },
  })
  render(
    <QueryClientProvider client={client}>
      <I18nextProvider i18n={i18n}>
        <BillingHistoryDialog open onOpenChange={vi.fn()} />
      </I18nextProvider>
    </QueryClientProvider>
  )
  await screen.findByText('order-0')
  for (const label of ['成功', '待确认', '已过期']) {
    expect(screen.getByText(label)).toBeVisible()
  }
  await act(() => i18n.changeLanguage('en'))
  for (const label of ['Success', 'Pending', 'Expired']) {
    expect(screen.getByText(label)).toBeVisible()
  }
})

it.each([
  { paid: '584', nanos: '584000000000' },
  { paid: '0.0098', nanos: '9800000' },
])(
  'translates recharge presets and the reviewed payment without changing its exact $paid amount',
  async ({ paid, nanos }) => {
    const info: TopupInfo = {
      enable_online_topup: true,
      enable_stripe_topup: false,
      pay_methods: [{ name: 'Bank', type: 'bank' }],
      checkout: {
        providers: [
          {
            provider: 'epay',
            configured: true,
            enabled: true,
            selection_kind: 'amount',
            minimum: { nanos: '1000000000', currency: 'USD' },
            presets: [
              { nanos: '100000000000', currency: 'USD' },
              { nanos: '20000000000', currency: 'USD' },
            ],
            products: [],
          },
        ],
      },
    }
    vi.spyOn(api, 'post').mockResolvedValue({
      data: {
        success: true,
        data: {
          quote_id: 'translated-quote',
          expires_at: 2_000_000_000,
          charge_amount: { nanos, currency: 'CNY' },
          charge_final: true,
          source_amount: { nanos: '100000000000', currency: 'USD' },
          credit_amount: { nanos: '100000000000', currency: 'USD' },
          fx: null,
        },
      },
    })
    const user = userEvent.setup()
    render(
      <QueryClientProvider client={client}>
        <I18nextProvider i18n={i18n}>
          <RechargeFormCard
            topupInfo={info}
            redemptionCode=''
            redeeming={false}
            onRedemptionCodeChange={vi.fn()}
            onRedeem={vi.fn()}
          />
        </I18nextProvider>
      </QueryClientProvider>
    )
    expect(screen.getByRole('button', { name: '兑换' })).toBeVisible()
    expect(screen.getByRole('button', { name: '20 USD' })).toBeVisible()
    expect(screen.getByPlaceholderText('最低 1 USD')).toBeVisible()
    await user.click(screen.getByRole('button', { name: '100 USD' }))
    expect(screen.getByLabelText(/USD/)).toHaveValue('100')
    await user.click(
      screen.getByRole('button', { name: i18n.t('Review payment') })
    )
    expect(await screen.findByText(`${paid} CNY`)).toBeVisible()
    await act(() => i18n.changeLanguage('en'))
    expect(screen.getByText(`${paid} CNY`)).toBeVisible()
    expect(
      screen.getByRole('alertdialog', { name: 'Confirm Payment' })
    ).toBeVisible()
    expect(screen.getByPlaceholderText('Minimum 1 USD')).toBeVisible()
  }
)
