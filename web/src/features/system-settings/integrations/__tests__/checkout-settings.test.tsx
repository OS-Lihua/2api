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
import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import i18next from 'i18next'
import { useState } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import { getBillingSectionContent } from '../../billing/section-registry'
import { SettingsPageProvider } from '../../components/settings-page-context'
import type { BillingSettings } from '../../types'

const amountProvider = {
  enabled: true,
  source_unit: { nanos: '1000000000', currency: 'USD' },
  charge_unit: { nanos: '7300000001', currency: 'CNY' },
  minimum: { nanos: '1', currency: 'USD' },
}
const productProvider = {
  enabled: true,
  products: [
    {
      product_id: 'price_external',
      name: 'Wallet',
      source_amount: { nanos: '9007199254740993', currency: 'EUR' },
    },
  ],
}

function PaymentFixture(props: {
  config: string
  confirmed: boolean
  settings?: Partial<BillingSettings>
}) {
  const [actions, setActions] = useState<HTMLDivElement | null>(null)
  const settings: BillingSettings = {
    'reward_setting.config': '',
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
    'payment_setting.compliance_terms_version': 'v1',
    'payment_setting.compliance_confirmed_at': 0,
    'payment_setting.compliance_confirmed_by': 0,
    'payment_setting.compliance_confirmed_ip': '',
    'checkin_setting.enabled': false,
    'checkin_setting.reward_range': '',
    'checkout_setting.config': props.config,
    ...props.settings,
  }
  return (
    <SettingsPageProvider actionsContainer={actions}>
      <div ref={setActions} />
      {getBillingSectionContent('payment', settings)}
    </SettingsPageProvider>
  )
}
function renderPayment(
  config: string,
  confirmed = true,
  client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  }),
  settings?: Partial<BillingSettings>
) {
  render(
    <QueryClientProvider client={client}>
      <PaymentFixture
        config={config}
        confirmed={confirmed}
        settings={settings}
      />
    </QueryClientProvider>
  )
  return userEvent.setup()
}
afterEach(async () => {
  vi.restoreAllMocks()
  await i18next.changeLanguage('en')
})

describe('administrator checkout settings', () => {
  it.each(['stripe', 'creem'] as const)(
    'keeps a maximum-length %s product inside a shrinkable form grid and the shared table scroll container',
    (provider) => {
      const productID = `external_${'x'.repeat(246)}`
      renderPayment(
        JSON.stringify({
          providers: {
            [provider]: {
              enabled: true,
              products: [
                {
                  product_id: productID,
                  name: 'A long external product name '.repeat(8),
                  source_amount: {
                    nanos: '9223372036854775807',
                    currency: 'EUR',
                  },
                },
              ],
            },
          },
        })
      )
      const gateway = screen.getByRole('group', {
        name: `${provider === 'stripe' ? 'Stripe' : 'Creem'} checkout`,
      })
      const table = within(gateway).getByRole('table')
      expect(within(table).getByText(productID)).toBeVisible()
      expect(table.closest('[data-slot="form-item"]')).toHaveClass(
        'min-w-0',
        'grid-cols-1'
      )
      expect(table.closest('[data-slot="table-container"]')).toHaveClass(
        'overflow-x-auto'
      )
    }
  )

  it('accepts a 64-byte Chinese group name and rejects a draft one byte longer', async () => {
    const request = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    const original = `${'组'.repeat(21)}a`
    const replacement = `${'组'.repeat(21)}b`
    const user = renderPayment(
      JSON.stringify({
        providers: {
          epay: { ...amountProvider, group_ratios: { [original]: '1' } },
        },
      })
    )
    const group = within(
      screen.getByRole('group', { name: 'Epay checkout' })
    ).getByRole('textbox', { name: 'Group name' })
    await user.clear(group)
    await user.type(group, `${original}x`)
    await user.click(
      screen.getByRole('button', { name: 'Save checkout settings' })
    )
    expect(
      await screen.findByText(
        'Enter a unique group name of at most 64 UTF-8 bytes.'
      )
    ).toBeVisible()
    expect(request).not.toHaveBeenCalled()
    await user.clear(group)
    await user.type(group, replacement)
    await user.click(
      screen.getByRole('button', { name: 'Save checkout settings' })
    )
    await waitFor(() =>
      expect(request).toHaveBeenCalledWith('/api/option/', {
        key: 'checkout_setting.config',
        value: JSON.stringify({
          providers: {
            epay: { ...amountProvider, group_ratios: { [replacement]: '1' } },
          },
        }),
      })
    )
  })

  it('accepts a 255-byte Chinese product name after rejecting a longer dialog draft', async () => {
    const request = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    const user = renderPayment(
      JSON.stringify({ providers: { stripe: productProvider } })
    )
    await user.click(
      within(screen.getByRole('group', { name: 'Stripe checkout' })).getByRole(
        'button',
        { name: 'Edit' }
      )
    )
    const dialog = within(
      screen.getByRole('dialog', { name: 'Edit external product' })
    )
    const name = dialog.getByRole('textbox', { name: 'Product name' })
    await user.clear(name)
    await user.type(name, '奖'.repeat(86))
    await user.click(
      dialog.getByRole('button', { name: 'Save product reward' })
    )
    expect(
      await dialog.findByText('Use at most 255 UTF-8 bytes.')
    ).toBeVisible()
    expect(request).not.toHaveBeenCalled()
    await user.clear(name)
    await user.type(name, '奖'.repeat(85))
    await user.click(
      dialog.getByRole('button', { name: 'Save product reward' })
    )
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    await user.click(
      screen.getByRole('button', { name: 'Save checkout settings' })
    )
    await waitFor(() =>
      expect(request).toHaveBeenCalledWith('/api/option/', {
        key: 'checkout_setting.config',
        value: JSON.stringify({
          providers: {
            stripe: {
              enabled: true,
              products: [
                { ...productProvider.products[0], name: '奖'.repeat(85) },
              ],
            },
          },
        }),
      })
    )
  })
  it.each([
    [
      'group name',
      JSON.stringify({
        providers: {
          epay: { ...amountProvider, group_ratios: { ['组'.repeat(22)]: '1' } },
        },
      }),
    ],
    [
      'product name',
      JSON.stringify({
        providers: {
          stripe: {
            enabled: true,
            products: [
              { ...productProvider.products[0], name: '奖'.repeat(86) },
            ],
          },
        },
      }),
    ],
    [
      'product ID',
      JSON.stringify({
        providers: {
          stripe: {
            enabled: true,
            products: [
              { ...productProvider.products[0], product_id: '品'.repeat(86) },
            ],
          },
        },
      }),
    ],
  ])(
    'rejects a saved Chinese %s that exceeds the backend UTF-8 byte limit',
    (_reason, config) => {
      renderPayment(config)
      expect(screen.getByText('Invalid checkout configuration')).toBeVisible()
      expect(
        screen.queryByRole('button', { name: 'Save checkout settings' })
      ).not.toBeInTheDocument()
    }
  )
  it('keeps Pancake catalog binding and saved credentials available independently of checkout pricing', async () => {
    const catalog = vi.spyOn(api, 'get').mockResolvedValue({
      data: {
        message: 'success',
        data: {
          stores: [
            {
              id: 'store_existing',
              name: 'Existing store',
              status: 'active',
              prodEnabled: true,
              onetimeProducts: [
                {
                  id: 'prod_existing',
                  name: 'Existing product',
                  status: 'active',
                },
                { id: 'prod_second', name: 'Second product', status: 'active' },
              ],
            },
          ],
        },
      },
    })
    const save = vi.spyOn(api, 'post').mockResolvedValue({
      data: {
        message: 'success',
        data: { store_id: 'store_existing', product_id: 'prod_second' },
      },
    })
    const put = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    const user = renderPayment('{"providers":{}}', true, undefined, {
      WaffoPancakeMerchantID: 'merchant_existing',
      WaffoPancakeStoreID: 'store_existing',
      WaffoPancakeProductID: 'prod_existing',
    })
    await user.click(screen.getByRole('tab', { name: 'Waffo Pancake' }))
    await waitFor(() =>
      expect(catalog).toHaveBeenCalledWith(
        '/api/option/waffo-pancake/catalog',
        { params: { merchant_id: '', private_key: '' } }
      )
    )
    const panel = within(screen.getByRole('tabpanel'))
    const product = (await panel.findAllByRole('combobox'))[1]
    await user.click(product)
    await user.click(
      screen.getByRole('option', { name: 'Second product (prod_second)' })
    )
    await user.click(
      screen.getByRole('button', { name: 'Save gateway connections' })
    )
    await waitFor(() =>
      expect(save).toHaveBeenCalledWith('/api/option/waffo-pancake/save', {
        merchant_id: 'merchant_existing',
        private_key: '',
        return_url: '',
        store_id: 'store_existing',
        product_id: 'prod_second',
      })
    )
    expect(put).not.toHaveBeenCalled()
  })
  it('edits payment method identity without offering the old per-method minimum amount', async () => {
    const user = renderPayment('{"providers":{}}')
    await user.click(screen.getByRole('button', { name: 'Add method' }))
    const dialog = within(
      screen.getByRole('dialog', { name: 'Add payment method' })
    )
    expect(
      dialog.queryByLabelText('Minimum top-up (optional)')
    ).not.toBeInTheDocument()
    expect(dialog.getByRole('textbox', { name: 'Name' })).toBeVisible()
    expect(
      dialog.getByRole('combobox', { name: 'Payment type key' })
    ).toBeVisible()
  })
  it('updates exact product rewards when the interface language changes and safely falls back for invalid languages', async () => {
    renderPayment(JSON.stringify({ providers: { stripe: productProvider } }))
    const stripe = within(
      screen.getByRole('group', { name: 'Stripe checkout' })
    )
    for (const [language, expected] of [
      ['en', '9,007,199.254740993 EUR'],
      ['zhCN', '9,007,199.254740993 EUR'],
      ['zhTW', '9,007,199.254740993 EUR'],
      ['fr', '9 007 199,254740993 EUR'],
      ['ru', '9 007 199,254740993 EUR'],
      ['ja', '9,007,199.254740993 EUR'],
      ['vi', '9.007.199,254740993 EUR'],
      ['invalid_locale', '9,007,199.254740993 EUR'],
    ]) {
      await act(() => i18next.changeLanguage(language))
      expect(
        stripe.getByText(expected, {
          exact: true,
          normalizer: (value) => value,
        })
      ).toBeVisible()
    }
  })

  it('edits a Stripe reward in the product dialog without submitting checkout before the explicit save', async () => {
    const request = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    const user = renderPayment(
      JSON.stringify({ providers: { stripe: productProvider } })
    )
    const stripe = within(
      screen.getByRole('group', { name: 'Stripe checkout' })
    )
    await user.click(stripe.getByRole('button', { name: 'Edit' }))
    const dialog = within(
      screen.getByRole('dialog', { name: 'Edit external product' })
    )
    const amount = dialog.getByRole('textbox', {
      name: 'Reward amount per item',
    })
    expect(amount).toHaveValue('9007199.254740993')
    await user.clear(amount)
    await user.type(amount, '0.000000001')
    await user.click(
      dialog.getByRole('button', { name: 'Save product reward' })
    )
    expect(await stripe.findByText('0.000000001 EUR')).toBeVisible()
    expect(request).not.toHaveBeenCalled()
    await user.click(
      screen.getByRole('button', { name: 'Save checkout settings' })
    )
    await waitFor(() =>
      expect(request).toHaveBeenCalledWith('/api/option/', {
        key: 'checkout_setting.config',
        value: JSON.stringify({
          providers: {
            stripe: {
              enabled: true,
              products: [
                {
                  product_id: 'price_external',
                  name: 'Wallet',
                  source_amount: { nanos: '1', currency: 'EUR' },
                },
              ],
            },
          },
        }),
      })
    )
    expect(request).toHaveBeenCalledTimes(1)
  })

  it('rejects a payment discount infinitesimally above one without rounding it to one', async () => {
    const request = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    const user = renderPayment(
      JSON.stringify({ providers: { epay: amountProvider } })
    )
    const discounts = within(
      within(screen.getByRole('group', { name: 'Epay checkout' })).getByRole(
        'group',
        { name: 'Payment discounts' }
      )
    )
    await user.click(
      discounts.getByRole('button', { name: 'Add payment discount' })
    )
    await user.type(
      discounts.getByRole('textbox', { name: 'Source amount' }),
      '1'
    )
    const ratio = discounts.getByRole('textbox', { name: 'Discount ratio' })
    await user.type(ratio, '1.000000000000000001')
    await user.click(
      screen.getByRole('button', { name: 'Save checkout settings' })
    )
    expect(
      await discounts.findByText(
        'Discount ratio must be greater than 0 and at most 1.'
      )
    ).toBeVisible()
    expect(ratio).toHaveAttribute('aria-invalid', 'true')
    expect(request).not.toHaveBeenCalled()
  })
  it('refreshes cached checkout choices after saving gateway pricing', async () => {
    vi.spyOn(api, 'put').mockResolvedValue({ data: { success: true } })
    const client = new QueryClient({
      defaultOptions: { mutations: { retry: false } },
    })
    client.setQueryData(['topup-info', 42], { checkout: { providers: [] } })
    client.setQueryData(['status'], {})
    const user = renderPayment(
      JSON.stringify({ providers: { epay: amountProvider } }),
      true,
      client
    )
    await user.click(
      within(screen.getByRole('group', { name: 'Epay checkout' })).getByRole(
        'switch',
        { name: 'Enable new checkout quotes' }
      )
    )
    await user.click(
      screen.getByRole('button', { name: 'Save checkout settings' })
    )
    await waitFor(() =>
      expect(client.getQueryState(['topup-info', 42])?.isInvalidated).toBe(true)
    )
    expect(client.getQueryState(['status'])?.isInvalidated).toBe(true)
  })
  it.each([
    ['malformed JSON', '{'],
    ['missing providers', '{}'],
    [
      'unknown gateway',
      JSON.stringify({ providers: { other: amountProvider } }),
    ],
    [
      'numeric nanos',
      JSON.stringify({
        providers: {
          epay: { ...amountProvider, minimum: { nanos: 1, currency: 'USD' } },
        },
      }),
    ],
    [
      'overflowing nanos',
      JSON.stringify({
        providers: {
          epay: {
            ...amountProvider,
            presets: [{ nanos: '9223372036854775808', currency: 'USD' }],
          },
        },
      }),
    ],
    [
      'mismatched source currency',
      JSON.stringify({
        providers: {
          epay: { ...amountProvider, minimum: { nanos: '1', currency: 'CNY' } },
        },
      }),
    ],
    [
      'incomplete disabled gateway',
      JSON.stringify({ providers: { epay: { enabled: false } } }),
    ],
    [
      'unsupported Creem reward ratio',
      JSON.stringify({
        providers: {
          creem: { ...productProvider, group_ratios: { vip: '2' } },
        },
      }),
    ],
    [
      'local fake payment price',
      JSON.stringify({
        providers: {
          stripe: {
            ...productProvider,
            products: [{ ...productProvider.products[0], price: 10 }],
          },
        },
      }),
    ],
    [
      'preset below the minimum',
      JSON.stringify({
        providers: {
          epay: {
            ...amountProvider,
            minimum: { nanos: '2', currency: 'USD' },
            presets: [{ nanos: '1', currency: 'USD' }],
          },
        },
      }),
    ],
    [
      'non-USD Pancake charge',
      JSON.stringify({ providers: { waffo_pancake: amountProvider } }),
    ],
  ])(
    'shows an error for saved %s and never offers a default pricing overwrite',
    (_reason, config) => {
      const request = vi
        .spyOn(api, 'put')
        .mockResolvedValue({ data: { success: true } })
      renderPayment(config)
      expect(screen.getByText('Invalid checkout configuration')).toBeVisible()
      expect(
        screen.queryByRole('button', { name: 'Save checkout settings' })
      ).not.toBeInTheDocument()
      expect(request).not.toHaveBeenCalled()
    }
  )

  it.each(['0', '-1', '9223372036.854775808', '0.0000000001', '1e-9', 'NaN'])(
    'rejects source amount %s without sending options and marks the input invalid',
    async (amount) => {
      const request = vi
        .spyOn(api, 'put')
        .mockResolvedValue({ data: { success: true } })
      const user = renderPayment(
        JSON.stringify({ providers: { epay: amountProvider } })
      )
      const minimum = within(
        screen.getByRole('group', { name: 'Epay checkout' })
      ).getByRole('textbox', { name: 'Minimum source amount' })
      await user.clear(minimum)
      await user.type(minimum, amount)
      await user.click(
        screen.getByRole('button', { name: 'Save checkout settings' })
      )
      expect(
        await screen.findByText(
          'Enter a positive amount with at most 9 decimal places within the int64 range.'
        )
      ).toBeVisible()
      expect(minimum).toHaveAttribute('aria-invalid', 'true')
      expect(request).not.toHaveBeenCalled()
    }
  )

  it('edits and removes preset rows while preserving precise discount and group ratios', async () => {
    const request = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    const user = renderPayment(
      JSON.stringify({ providers: { epay: amountProvider } })
    )
    const epay = within(screen.getByRole('group', { name: 'Epay checkout' }))
    const presets = within(
      epay.getByRole('group', { name: 'Preset source amounts' })
    )
    await user.click(presets.getByRole('button', { name: 'Add preset' }))
    await user.type(
      presets.getByRole('textbox', { name: 'Source amount' }),
      '0.000000001'
    )
    await user.click(presets.getByRole('button', { name: 'Add preset' }))
    await user.type(
      presets.getAllByRole('textbox', { name: 'Source amount' })[1],
      '9007199.254740993'
    )
    await user.click(presets.getByRole('button', { name: 'Remove rule 1' }))
    const discounts = within(
      epay.getByRole('group', { name: 'Payment discounts' })
    )
    await user.click(
      discounts.getByRole('button', { name: 'Add payment discount' })
    )
    await user.type(
      discounts.getByRole('textbox', { name: 'Source amount' }),
      '100.000000001'
    )
    await user.type(
      discounts.getByRole('textbox', { name: 'Discount ratio' }),
      '0.999999999999999999'
    )
    const groups = within(
      epay.getByRole('group', { name: 'Payment group ratios' })
    )
    await user.click(groups.getByRole('button', { name: 'Add group ratio' }))
    await user.type(groups.getByRole('textbox', { name: 'Group name' }), 'vip')
    await user.type(
      groups.getByRole('textbox', { name: 'Group ratio' }),
      '1.000000000000000001'
    )
    await user.click(
      screen.getByRole('button', { name: 'Save checkout settings' })
    )
    await waitFor(() => expect(request).toHaveBeenCalledTimes(1))
    expect(request).toHaveBeenCalledWith('/api/option/', {
      key: 'checkout_setting.config',
      value: JSON.stringify({
        providers: {
          epay: {
            ...amountProvider,
            presets: [{ nanos: '9007199254740993', currency: 'USD' }],
            group_ratios: { vip: '1.000000000000000001' },
            discounts: [
              {
                source_amount: { nanos: '100000000001', currency: 'USD' },
                ratio: '0.999999999999999999',
              },
            ],
          },
        },
      }),
    })
  })

  it('keeps invalid disabled gateway drafts visible and requires complete configuration', async () => {
    const request = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    const user = renderPayment('{"providers":{}}')
    const epay = within(screen.getByRole('group', { name: 'Epay checkout' }))
    await user.click(
      epay.getByRole('switch', { name: 'Configure wallet checkout' })
    )
    expect(epay.getByRole('combobox', { name: 'Charge currency' })).toHaveValue(
      ''
    )
    expect(
      epay.getByRole('textbox', { name: 'Charge unit amount' })
    ).toHaveValue('')
    expect(
      epay.getByRole('switch', { name: 'Enable new checkout quotes' })
    ).not.toBeChecked()
    await user.click(
      screen.getByRole('button', { name: 'Save checkout settings' })
    )
    await waitFor(() =>
      expect(
        epay.getByRole('textbox', { name: 'Charge unit amount' })
      ).toHaveAttribute('aria-invalid', 'true')
    )
    expect(request).not.toHaveBeenCalled()
    await user.click(
      epay.getByRole('switch', { name: 'Configure wallet checkout' })
    )
    expect(
      screen.getByRole('button', { name: 'Save checkout settings' })
    ).toBeDisabled()
    expect(request).not.toHaveBeenCalled()
  })

  it('removes a configured gateway while preserving other gateways in the same save', async () => {
    const request = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    const user = renderPayment(
      JSON.stringify({
        providers: { epay: amountProvider, stripe: productProvider },
      })
    )
    await user.click(
      within(screen.getByRole('group', { name: 'Epay checkout' })).getByRole(
        'switch',
        { name: 'Configure wallet checkout' }
      )
    )
    await user.click(
      screen.getByRole('button', { name: 'Save checkout settings' })
    )
    await waitFor(() =>
      expect(request).toHaveBeenCalledWith('/api/option/', {
        key: 'checkout_setting.config',
        value: JSON.stringify({ providers: { stripe: productProvider } }),
      })
    )
    expect(request).toHaveBeenCalledTimes(1)
  })

  it('keeps all payment controls locked until compliance is confirmed', () => {
    renderPayment(
      JSON.stringify({ providers: { epay: amountProvider } }),
      false
    )
    expect(
      screen.getByRole('button', { name: 'Save checkout settings' })
    ).toBeDisabled()
    expect(
      screen.getByRole('button', { name: 'Save gateway connections' })
    ).toBeDisabled()
    expect(
      within(screen.getByRole('group', { name: 'Epay checkout' })).getByRole(
        'textbox',
        { name: 'Minimum source amount' }
      )
    ).toBeDisabled()
    expect(
      within(screen.getByRole('group', { name: 'Epay checkout' })).getByRole(
        'switch',
        { name: 'Enable new checkout quotes' }
      )
    ).toHaveAttribute('aria-disabled', 'true')
    expect(
      screen.getByRole('button', { name: 'Confirm compliance' })
    ).toBeEnabled()
  })

  it('retains the precise pricing draft and shows a server rejection for retry', async () => {
    const request = vi.spyOn(api, 'put').mockResolvedValue({
      data: { success: false, message: 'Checkout save rejected' },
    })
    const user = renderPayment(
      JSON.stringify({ providers: { epay: amountProvider } })
    )
    const input = within(
      screen.getByRole('group', { name: 'Epay checkout' })
    ).getByRole('textbox', { name: 'Charge unit amount' })
    await user.clear(input)
    await user.type(input, '7.300000009')
    await user.click(
      screen.getByRole('button', { name: 'Save checkout settings' })
    )
    expect(
      await within(
        screen.getByRole('region', { name: 'Wallet checkout pricing' })
      ).findByRole('alert')
    ).toHaveTextContent('Checkout save rejected')
    expect(input).toHaveValue('7.300000009')
    expect(
      screen.getByRole('button', { name: 'Save checkout settings' })
    ).toBeEnabled()
    expect(request).toHaveBeenCalledTimes(1)
  })
  it('refuses a saved group ratio beyond the backend bound instead of replacing the configuration', () => {
    const request = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    renderPayment(
      JSON.stringify({
        providers: {
          epay: {
            ...amountProvider,
            group_ratios: { vip: '1000000000.000000001' },
          },
        },
      })
    )
    expect(screen.getByText('Invalid checkout configuration')).toBeVisible()
    expect(
      screen.queryByRole('button', { name: 'Save checkout settings' })
    ).not.toBeInTheDocument()
    expect(request).not.toHaveBeenCalled()
  })
  it('binds an external Creem product to an exact per-item reward without a local payment price', async () => {
    const request = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    const user = renderPayment('{"providers":{}}')
    const creem = within(screen.getByRole('group', { name: 'Creem checkout' }))
    await user.click(
      creem.getByRole('switch', { name: 'Configure wallet checkout' })
    )
    await user.click(
      creem.getByRole('switch', { name: 'Enable new checkout quotes' })
    )
    await user.click(
      creem.getByRole('button', { name: 'Add external product' })
    )
    const dialog = within(
      screen.getByRole('dialog', { name: 'Add external product' })
    )
    await user.type(
      dialog.getByRole('textbox', { name: 'Creem Product ID' }),
      'prod_external'
    )
    await user.type(
      dialog.getByRole('textbox', { name: 'Product name' }),
      'Credit pack'
    )
    await user.type(
      dialog.getByRole('textbox', { name: 'Reward amount per item' }),
      '9223372036.854775807'
    )
    await user.type(
      dialog.getByRole('combobox', { name: 'Reward currency' }),
      'cny'
    )
    await user.tab()
    expect(dialog.queryByRole('spinbutton')).not.toBeInTheDocument()
    expect(dialog.queryByLabelText('Price')).not.toBeInTheDocument()
    await user.click(
      dialog.getByRole('button', { name: 'Save product reward' })
    )
    expect(await creem.findByText('9,223,372,036.854775807 CNY')).toBeVisible()
    await user.click(
      screen.getByRole('button', { name: 'Save checkout settings' })
    )
    await waitFor(() => expect(request).toHaveBeenCalledTimes(1))
    expect(request).toHaveBeenCalledWith('/api/option/', {
      key: 'checkout_setting.config',
      value: JSON.stringify({
        providers: {
          creem: {
            enabled: true,
            products: [
              {
                product_id: 'prod_external',
                name: 'Credit pack',
                source_amount: {
                  nanos: '9223372036854775807',
                  currency: 'CNY',
                },
              },
            ],
          },
        },
      }),
    })
  })
  it('removes legacy wallet prices while retaining gateway credentials and callback settings', async () => {
    const request = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    const user = renderPayment('{"providers":{}}')
    expect(
      screen.queryByLabelText('Price (local currency / USD)')
    ).not.toBeInTheDocument()
    expect(
      screen.queryByLabelText('Minimum top-up (USD)')
    ).not.toBeInTheDocument()
    expect(screen.queryByText('Top-up amount options')).not.toBeInTheDocument()
    await user.click(screen.getByRole('tab', { name: 'Epay' }))
    await user.type(
      screen.getByRole('textbox', { name: 'Epay merchant ID' }),
      'merchant-example'
    )
    expect(
      screen.getByRole('textbox', { name: 'Callback address' })
    ).toBeVisible()
    await user.click(
      screen.getByRole('button', { name: 'Save gateway connections' })
    )
    await waitFor(() => expect(request).toHaveBeenCalledTimes(1))
    expect(request).toHaveBeenCalledWith('/api/option/', {
      key: 'EpayId',
      value: 'merchant-example',
    })
    await user.click(screen.getByRole('tab', { name: 'Stripe' }))
    expect(screen.getByLabelText('API secret')).toHaveAttribute(
      'type',
      'password'
    )
    expect(
      screen.queryByLabelText('Unit price (local currency / USD)')
    ).not.toBeInTheDocument()
    expect(
      screen.queryByLabelText('Minimum top-up (USD)')
    ).not.toBeInTheDocument()
    await user.click(screen.getByRole('tab', { name: 'Creem' }))
    expect(screen.getByLabelText('Webhook Secret')).toHaveAttribute(
      'type',
      'password'
    )
    expect(
      screen.queryByRole('button', { name: 'Add product' })
    ).not.toBeInTheDocument()
    await user.click(screen.getByRole('tab', { name: 'Waffo' }))
    expect(screen.getByText('RSA Private Key (Production)')).toBeVisible()
    expect(screen.queryByText('Unit price (USD)')).not.toBeInTheDocument()
    expect(
      screen.queryByText('Minimum top-up quantity')
    ).not.toBeInTheDocument()
    await user.click(screen.getByRole('tab', { name: 'Waffo Pancake' }))
    expect(screen.getByText('Bind a Pancake store + product')).toBeVisible()
  })
  it('saves an exact cross-currency unit price and all other gateways in one option', async () => {
    const request = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    const user = renderPayment(
      JSON.stringify({
        providers: { epay: amountProvider, stripe: productProvider },
      })
    )
    const epay = screen.getByRole('group', { name: 'Epay checkout' })
    const charge = within(epay).getByRole('textbox', {
      name: 'Charge unit amount',
    })
    expect(charge).toHaveValue('7.300000001')
    await user.clear(charge)
    await user.type(charge, '7.300000009')
    await user.click(
      screen.getByRole('button', { name: 'Save checkout settings' })
    )
    await waitFor(() => expect(request).toHaveBeenCalledTimes(1))
    expect(request).toHaveBeenCalledWith('/api/option/', {
      key: 'checkout_setting.config',
      value: JSON.stringify({
        providers: {
          epay: {
            ...amountProvider,
            charge_unit: { nanos: '7300000009', currency: 'CNY' },
          },
          stripe: productProvider,
        },
      }),
    })
    expect(
      screen.getByRole('button', { name: 'Save checkout settings' })
    ).toBeDisabled()
  })
})
