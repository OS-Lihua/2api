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
export interface ChannelScope {
  include_provider_ids: number[]
  exclude_provider_ids: number[]
  include_channel_ids: number[]
  exclude_channel_ids: number[]
}

export interface ModelOfferingInput {
  name: string
  aliases: string[]
  visibility: 'all' | 'groups'
  groups: string[]
  members: { model_id: number; tier: number }[]
  channel_scope: ChannelScope
  route_method: 'cost' | 'weight' | 'latency'
  accepted_conversion_losses: string[]
  minimum_margin_rate: string
  allow_loss_fallback: boolean
}

export interface ModelOffering extends ModelOfferingInput {
  id: number
  enabled: boolean
  created_at: number
  updated_at: number
}

export interface OfferingListParams {
  p: number
  page_size: number
  keyword: string
}

export interface ApiResult<T> {
  success: boolean
  message?: string
  data: T
}

export interface OfferingPage {
  items: ModelOffering[]
  total: number
  page: number
  page_size: number
}
