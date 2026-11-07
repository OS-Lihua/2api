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
import { expect, it } from 'vitest'

import { toIntlLocale } from '@/i18n/languages'

import { formatMoneyAmount } from '../currency'

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
