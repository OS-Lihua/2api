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
import { z } from 'zod'

import {
  moneyAmountToDecimal,
  parseDecimalMoneyAmount,
  type MoneyAmount,
} from '@/lib/currency'

export const CHECKOUT_PROVIDERS = [
  'epay',
  'stripe',
  'creem',
  'waffo',
  'waffo_pancake',
] as const
export type CheckoutProviderKey = (typeof CHECKOUT_PROVIDERS)[number]
export const CHECKOUT_PROVIDER_NAMES: Record<CheckoutProviderKey, string> = {
  epay: 'Epay',
  stripe: 'Stripe',
  creem: 'Creem',
  waffo: 'Waffo',
  waffo_pancake: 'Waffo Pancake',
}
export function isProductCheckout(key: CheckoutProviderKey): boolean {
  return key === 'stripe' || key === 'creem'
}
export function checkoutTextByteLength(value: string): number {
  return new TextEncoder().encode(value).length
}
const positiveRatio = z.string().refine((value) => {
  if (
    value.length > 40 ||
    !/^(?:\d+(?:\.\d{0,18})?|\.\d{1,18})$/.test(value) ||
    !/[1-9]/.test(value)
  ) {
    return false
  }
  const [whole, fraction = ''] = value.split('.')
  const scaled = BigInt(whole) * 10n ** 18n + BigInt(fraction.padEnd(18, '0'))
  return scaled <= 1_000_000_000n * 10n ** 18n
}, 'Enter a positive decimal ratio up to 1,000,000,000 with at most 18 decimal places.')
const discountRatio = positiveRatio.refine((value) => {
  if (!/^(?:\d+(?:\.\d*)?|\.\d+)$/.test(value)) return false
  const [whole, fraction = ''] = value.split('.')
  return BigInt(whole) < 1n || (BigInt(whole) === 1n && !/[1-9]/.test(fraction))
}, 'Discount ratio must be greater than 0 and at most 1.')
const moneySchema = z
  .object({
    nanos: z
      .string()
      .refine(
        (value) =>
          /^\d+$/.test(value) &&
          BigInt(value) > 0n &&
          BigInt(value) <= 9_223_372_036_854_775_807n
      ),
    currency: z.string().regex(/^[A-Z]{3}$/),
  })
  .strict()
const providerSchema = z
  .object({
    enabled: z.boolean(),
    source_unit: moneySchema.optional(),
    charge_unit: moneySchema.optional(),
    minimum: moneySchema.optional(),
    presets: z.array(moneySchema).optional(),
    group_ratios: z
      .record(
        z
          .string()
          .min(1)
          .refine((value) => checkoutTextByteLength(value) <= 64)
          .refine((value) => value === value.trim()),
        positiveRatio
      )
      .optional(),
    discounts: z
      .array(
        z.object({ source_amount: moneySchema, ratio: discountRatio }).strict()
      )
      .optional(),
    products: z
      .array(
        z
          .object({
            product_id: z
              .string()
              .min(1)
              .refine((value) => checkoutTextByteLength(value) <= 255)
              .refine((value) => value === value.trim()),
            name: z
              .string()
              .min(1)
              .refine((value) => checkoutTextByteLength(value) <= 255)
              .refine((value) => !!value.trim()),
            source_amount: moneySchema,
          })
          .strict()
      )
      .optional(),
  })
  .strict()
export const checkoutConfigSchema = z
  .object({
    providers: z.partialRecord(z.enum(CHECKOUT_PROVIDERS), providerSchema),
  })
  .strict()
  .superRefine((config, context) => {
    for (const key of CHECKOUT_PROVIDERS) {
      const provider = config.providers[key]
      if (!provider) continue
      const invalid = (field: string) =>
        context.addIssue({
          code: 'custom',
          path: ['providers', key, field],
          message: 'Invalid checkout configuration',
        })
      if (isProductCheckout(key)) {
        if (!provider.products?.length) invalid('products')
        if (
          provider.source_unit ||
          provider.charge_unit ||
          provider.minimum ||
          provider.presets?.length ||
          provider.discounts?.length
        ) {
          invalid('source_unit')
        }
        if (
          key === 'creem' &&
          Object.keys(provider.group_ratios ?? {}).length
        ) {
          invalid('group_ratios')
        }
        const ids =
          provider.products?.map((product) => product.product_id) ?? []
        if (new Set(ids).size !== ids.length) invalid('products')
        continue
      }
      if (!provider.source_unit || !provider.charge_unit || !provider.minimum) {
        invalid('source_unit')
      }
      if (provider.products?.length) invalid('products')
      if (key === 'waffo_pancake' && provider.charge_unit?.currency !== 'USD') {
        invalid('charge_unit')
      }
      if (
        provider.presets?.some(
          (amount) =>
            moneySchema.safeParse(amount).success &&
            provider.minimum &&
            moneySchema.safeParse(provider.minimum).success &&
            BigInt(amount.nanos) < BigInt(provider.minimum.nanos)
        )
      ) {
        invalid('presets')
      }
      const sourceAmounts = [
        provider.minimum,
        ...(provider.presets ?? []),
        ...(provider.discounts?.map((discount) => discount.source_amount) ??
          []),
      ]
      if (
        sourceAmounts.some(
          (amount) =>
            amount && amount.currency !== provider.source_unit?.currency
        )
      ) {
        invalid('minimum')
      }
      const presets =
        provider.presets
          ?.filter((amount) => moneySchema.safeParse(amount).success)
          .map((amount) => BigInt(amount.nanos).toString()) ?? []
      const discounts =
        provider.discounts
          ?.filter(
            (discount) => moneySchema.safeParse(discount.source_amount).success
          )
          .map((discount) => BigInt(discount.source_amount.nanos).toString()) ??
        []
      if (new Set(presets).size !== presets.length) invalid('presets')
      if (new Set(discounts).size !== discounts.length) invalid('discounts')
    }
  })
export type CheckoutConfig = z.infer<typeof checkoutConfigSchema>

const providerDraftSchema = z
  .object({
    key: z.enum(CHECKOUT_PROVIDERS),
    configured: z.boolean(),
    enabled: z.boolean(),
    source_currency: z.string(),
    charge_currency: z.string(),
    source_unit: z.string(),
    charge_unit: z.string(),
    minimum: z.string(),
    presets: z.array(z.object({ amount: z.string() })),
    group_ratios: z.array(z.object({ group: z.string(), ratio: z.string() })),
    discounts: z.array(z.object({ amount: z.string(), ratio: z.string() })),
    products: z.array(
      z.object({
        product_id: z.string(),
        name: z.string(),
        amount: z.string(),
        currency: z.string(),
      })
    ),
  })
  .superRefine((provider, context) => {
    if (!provider.configured) return
    const issue = (path: (string | number)[], message: string) =>
      context.addIssue({ code: 'custom', path, message })
    const validateMoney = (
      amount: string,
      currency: string,
      path: (string | number)[]
    ) => {
      const money = parseDecimalMoneyAmount(amount, currency)
      if (!money || BigInt(money.nanos) <= 0n) {
        issue(
          path,
          'Enter a positive amount with at most 9 decimal places within the int64 range.'
        )
      }
      return money
    }
    const groups = new Set<string>()
    for (const [index, group] of provider.group_ratios.entries()) {
      if (
        !group.group.trim() ||
        checkoutTextByteLength(group.group.trim()) > 64 ||
        groups.has(group.group.trim())
      ) {
        issue(
          ['group_ratios', index, 'group'],
          'Enter a unique group name of at most 64 UTF-8 bytes.'
        )
      }
      groups.add(group.group.trim())
      if (!positiveRatio.safeParse(group.ratio).success) {
        issue(
          ['group_ratios', index, 'ratio'],
          'Enter a positive decimal ratio up to 1,000,000,000 with at most 18 decimal places.'
        )
      }
    }
    if (isProductCheckout(provider.key)) {
      if (!provider.products.length) {
        issue(['products'], 'Add at least one external product.')
      }
      const ids = new Set<string>()
      for (const [index, product] of provider.products.entries()) {
        if (
          !product.product_id.trim() ||
          checkoutTextByteLength(product.product_id.trim()) > 255 ||
          ids.has(product.product_id.trim())
        ) {
          issue(
            ['products', index, 'product_id'],
            'Enter a unique external product ID of at most 255 UTF-8 bytes.'
          )
        }
        ids.add(product.product_id.trim())
        if (!product.name.trim()) {
          issue(['products', index, 'name'], 'Product name is required.')
        }
        if (checkoutTextByteLength(product.name.trim()) > 255) {
          issue(['products', index, 'name'], 'Use at most 255 UTF-8 bytes.')
        }
        validateMoney(product.amount, product.currency, [
          'products',
          index,
          'amount',
        ])
        if (!/^[A-Z]{3}$/.test(product.currency)) {
          issue(['products', index, 'currency'], 'Invalid currency')
        }
      }
      return
    }
    if (!/^[A-Z]{3}$/.test(provider.source_currency)) {
      issue(['source_currency'], 'Invalid currency')
    }
    if (!/^[A-Z]{3}$/.test(provider.charge_currency)) {
      issue(['charge_currency'], 'Invalid currency')
    }
    validateMoney(provider.source_unit, provider.source_currency, [
      'source_unit',
    ])
    validateMoney(provider.charge_unit, provider.charge_currency, [
      'charge_unit',
    ])
    if (
      provider.key === 'waffo_pancake' &&
      provider.charge_currency !== 'USD'
    ) {
      issue(['charge_currency'], 'Waffo Pancake checkout charges must use USD.')
    }
    const minimum = validateMoney(provider.minimum, provider.source_currency, [
      'minimum',
    ])
    const presets = new Set<string>()
    for (const [index, preset] of provider.presets.entries()) {
      const money = validateMoney(preset.amount, provider.source_currency, [
        'presets',
        index,
        'amount',
      ])
      if (money && minimum && BigInt(money.nanos) < BigInt(minimum.nanos)) {
        issue(
          ['presets', index, 'amount'],
          'Preset source amount must be at least the minimum.'
        )
      }
      if (money && presets.has(money.nanos)) {
        issue(['presets', index, 'amount'], 'Enter a unique source amount.')
      }
      if (money) presets.add(money.nanos)
    }
    const discounts = new Set<string>()
    for (const [index, discount] of provider.discounts.entries()) {
      const money = validateMoney(discount.amount, provider.source_currency, [
        'discounts',
        index,
        'amount',
      ])
      if (money && discounts.has(money.nanos)) {
        issue(['discounts', index, 'amount'], 'Enter a unique source amount.')
      }
      if (money) discounts.add(money.nanos)
      if (!discountRatio.safeParse(discount.ratio).success) {
        issue(
          ['discounts', index, 'ratio'],
          'Discount ratio must be greater than 0 and at most 1.'
        )
      }
    }
  })
export const checkoutSettingsSchema = z.object({
  providers: z.array(providerDraftSchema),
})
export type CheckoutSettingsValues = z.infer<typeof checkoutSettingsSchema>

export function checkoutConfigToForm(
  config: CheckoutConfig
): CheckoutSettingsValues {
  return {
    providers: CHECKOUT_PROVIDERS.map((key) => {
      const provider = config.providers[key]
      return {
        key,
        configured: !!provider,
        enabled: provider?.enabled ?? false,
        source_currency: provider?.source_unit?.currency ?? '',
        charge_currency: provider?.charge_unit?.currency ?? '',
        source_unit: provider?.source_unit
          ? moneyAmountToDecimal(provider.source_unit)
          : '',
        charge_unit: provider?.charge_unit
          ? moneyAmountToDecimal(provider.charge_unit)
          : '',
        minimum: provider?.minimum
          ? moneyAmountToDecimal(provider.minimum)
          : '',
        presets:
          provider?.presets?.map((amount) => ({
            amount: moneyAmountToDecimal(amount),
          })) ?? [],
        group_ratios: Object.entries(provider?.group_ratios ?? {}).map(
          ([group, ratio]) => ({ group, ratio })
        ),
        discounts:
          provider?.discounts?.map((discount) => ({
            amount: moneyAmountToDecimal(discount.source_amount),
            ratio: discount.ratio,
          })) ?? [],
        products:
          provider?.products?.map((product) => ({
            product_id: product.product_id,
            name: product.name,
            amount: moneyAmountToDecimal(product.source_amount),
            currency: product.source_amount.currency,
          })) ?? [],
      }
    }),
  }
}

export function checkoutFormToConfig(
  values: CheckoutSettingsValues
): CheckoutConfig {
  const providers: CheckoutConfig['providers'] = {}
  for (const provider of values.providers) {
    if (!provider.configured) continue
    const groups = provider.group_ratios.length
      ? {
          group_ratios: Object.fromEntries(
            provider.group_ratios.map((group) => [
              group.group.trim(),
              group.ratio,
            ])
          ),
        }
      : {}
    if (isProductCheckout(provider.key)) {
      providers[provider.key] = {
        enabled: provider.enabled,
        ...groups,
        products: provider.products.map((product) => ({
          product_id: product.product_id.trim(),
          name: product.name.trim(),
          source_amount: parseDecimalMoneyAmount(
            product.amount,
            product.currency
          ) as MoneyAmount,
        })),
      }
      continue
    }
    providers[provider.key] = {
      enabled: provider.enabled,
      source_unit: parseDecimalMoneyAmount(
        provider.source_unit,
        provider.source_currency
      ) as MoneyAmount,
      charge_unit: parseDecimalMoneyAmount(
        provider.charge_unit,
        provider.charge_currency
      ) as MoneyAmount,
      minimum: parseDecimalMoneyAmount(
        provider.minimum,
        provider.source_currency
      ) as MoneyAmount,
      ...(provider.presets.length
        ? {
            presets: provider.presets.map(
              (preset) =>
                parseDecimalMoneyAmount(
                  preset.amount,
                  provider.source_currency
                ) as MoneyAmount
            ),
          }
        : {}),
      ...groups,
      ...(provider.discounts.length
        ? {
            discounts: provider.discounts.map((discount) => ({
              source_amount: parseDecimalMoneyAmount(
                discount.amount,
                provider.source_currency
              ) as MoneyAmount,
              ratio: discount.ratio,
            })),
          }
        : {}),
    }
  }
  return checkoutConfigSchema.parse({ providers })
}
