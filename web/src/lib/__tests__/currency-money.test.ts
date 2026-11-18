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
import { afterEach, expect, it } from 'vitest'

import { toIntlLocale } from '@/i18n/languages'
import { mapStatusDataToConfig } from '@/lib/status-query'
import { useSystemConfigStore } from '@/stores/system-config-store'

import {
  formatDecimalMoneyAmount,
  formatMoneyAmount,
  moneyAmountToDecimal,
  parseDecimalMoneyAmount,
} from '../currency'

afterEach(() => {
  useSystemConfigStore.setState(useSystemConfigStore.getInitialState(), true)
  localStorage.clear()
})

it.each(['USD', 'CNY', 'CUSTOM', 'TOKENS'])(
  'keeps native money exact when historical status uses %s display and a custom quota unit',
  (displayType) => {
    useSystemConfigStore.setState(useSystemConfigStore.getInitialState(), true)
    useSystemConfigStore.getState().setConfig(
      mapStatusDataToConfig({
        legacy_quota_per_unit: '1000000.000',
        quota_per_unit: 1,
        quota_display_type: displayType,
        usd_exchange_rate: 7,
        custom_currency_symbol: 'OLD',
        custom_currency_exchange_rate: 19,
      })
    )
    const amount = { nanos: '9007199254740993', currency: 'EUR' }
    expect(formatMoneyAmount(amount, 'en')).toBe('9,007,199.254740993 EUR')
    expect(moneyAmountToDecimal(amount)).toBe('9007199.254740993')
    expect(parseDecimalMoneyAmount('9007199.254740993', 'EUR')).toEqual(amount)
    expect(
      formatDecimalMoneyAmount(
        { amount: '9007199.254740993', currency: 'EUR' },
        'en'
      )
    ).toBe('9,007,199.254740993 EUR')
    expect(formatMoneyAmount({ nanos: '1', currency: 'JPY' }, 'en')).toBe(
      '0.000000001 JPY'
    )
  }
)

it.each([
  ['en', '9,007,199.254740993 USD'],
  ['zhCN', '9,007,199.254740993 USD'],
  ['zhTW', '9,007,199.254740993 USD'],
  ['fr', '9 007 199,254740993 USD'],
  ['ru', '9 007 199,254740993 USD'],
  ['ja', '9,007,199.254740993 USD'],
  ['vi', '9.007.199,254740993 USD'],
])(
  'preserves every nanos digit and currency in the normalized %s locale',
  (language, expected) => {
    expect(
      formatMoneyAmount(
        { nanos: '9007199254740993', currency: 'USD' },
        toIntlLocale(language)
      )
    ).toBe(expected)
    expect(
      formatDecimalMoneyAmount(
        { amount: '9007199.254740993', currency: 'USD' },
        toIntlLocale(language)
      )
    ).toBe(expected)
  }
)

it.each([
  ['0', '0 CNY'],
  ['-0', '0 CNY'],
  ['1', '0.000000001 CNY'],
  ['-1', '-0.000000001 CNY'],
  ['-1234567890123456789', '-1,234,567,890.123456789 CNY'],
  ['1000000000', '1 CNY'],
])(
  'formats signed nanos %s exactly without legacy currency conversion',
  (nanos, expected) => {
    expect(formatMoneyAmount({ nanos, currency: 'CNY' }, 'en')).toBe(expected)
  }
)

it.each([
  ['0', '0 USD'],
  ['-0.000000001', '-0.000000001 USD'],
  ['9223372036.854775807', '9,223,372,036.854775807 USD'],
  ['1.0000000001', '-'],
  ['1e3', '-'],
  ['NaN', '-'],
  ['', '-'],
])(
  'formats decimal money %s exactly or rejects invalid precision',
  (amount, expected) => {
    expect(formatDecimalMoneyAmount({ amount, currency: 'USD' }, 'en')).toBe(
      expected
    )
  }
)

it.each([
  ['0', '0'],
  ['0.000000001', '1'],
  ['9007199.254740993', '9007199254740993'],
  ['9223372036.854775807', '9223372036854775807'],
  ['-9223372036.854775808', '-9223372036854775808'],
])('round-trips editable decimal %s without losing nanos', (decimal, nanos) => {
  const amount = { nanos, currency: 'USD' }
  expect(parseDecimalMoneyAmount(decimal, 'USD')).toEqual(amount)
  expect(moneyAmountToDecimal(amount)).toBe(decimal)
})

it.each([
  '9223372036.854775808',
  '-9223372036.854775809',
  '0.0000000001',
  '1e3',
  'NaN',
  '',
])('rejects invalid or overflowing decimal input %s', (value) => {
  expect(parseDecimalMoneyAmount(value, 'USD')).toBeNull()
})

it('rejects invalid currency input without changing its denomination', () => {
  expect(parseDecimalMoneyAmount('1', 'usd')).toBeNull()
  expect(parseDecimalMoneyAmount('1', '')).toBeNull()
})

it('uses the runtime locale when an interface locale is invalid', () => {
  expect(
    formatMoneyAmount(
      { nanos: '1', currency: 'USD' },
      toIntlLocale('invalid_locale')
    )
  ).toBe('0.000000001 USD')
})

it.each([
  undefined,
  { nanos: '1.25', currency: 'USD' },
  { nanos: 'NaN', currency: 'USD' },
  { nanos: '1', currency: '<script>' },
  { nanos: '1', currency: '' },
])('shows a safe fallback for an absent or invalid amount %j', (amount) => {
  expect(formatMoneyAmount(amount, 'en')).toBe('-')
})
