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
import type { ComponentProps } from 'react'
import { useTranslation } from 'react-i18next'

import { ComboboxInput } from '@/components/ui/combobox-input'

const currencies = [
  'USD',
  'CNY',
  'EUR',
  'GBP',
  'JPY',
  'HKD',
  'CAD',
  'AUD',
  'SGD',
  'CHF',
].map((code) => ({ value: code, label: code }))

export function CurrencyInput(
  props: Omit<
    ComponentProps<typeof ComboboxInput>,
    'options' | 'allowCustomValue'
  >
) {
  const { t } = useTranslation()
  return (
    <ComboboxInput
      {...props}
      options={currencies}
      allowCustomValue
      placeholder={t('Currency code')}
      onValueChange={(value) => props.onValueChange(value.toUpperCase())}
    />
  )
}
