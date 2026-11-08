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
import type { MoneyAmount } from '@/lib/currency'

export interface MarginEstimate {
  model_offering_id: number
  channel_id: number
  provider_id: number
  samples: number
  revenue: MoneyAmount | null
  cost: MoneyAmount | null
  margin: MoneyAmount | null
  negative: boolean
  status: string
}
export interface MarginCheck {
  revision: string
  effective_at?: number
  requires_confirmation: boolean
  estimates: MarginEstimate[]
}

export type OwnerType = 'model_offering' | 'channel'
export interface PriceVersionInput {
  margin_confirmation?: string
  owner_type: OwnerType
  owner_id: number
  currency: string
  expression: string
  effective_at?: number
}
export interface PriceVersion extends PriceVersionInput {
  effective_at: number
  id: number
  created_by: number
  created_at: number
}
export interface Page<T> {
  items: T[]
  total: number
  page: number
  page_size: number
}
export interface PriceListParams {
  p: number
  page_size: number
  owner_type?: OwnerType
  owner_id?: number
}
export interface FxRateInput {
  margin_confirmation?: string
  from_currency: string
  to_currency: string
  rate: string
  buffer_rate: string
}
export interface FxRate extends FxRateInput {
  updated_by: number
  updated_at: number
}
export interface FxQuote {
  from_currency: string
  to_currency: string
  raw_rate: string
  accounting_rate: string
  comparison_rate: string
  buffer_rate: string
}
export interface ApiResult<T> {
  success: boolean
  message?: string
  data: T
}
