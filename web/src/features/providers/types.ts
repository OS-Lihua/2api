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
import type { MultiKeyStatusResponse } from '@/features/channels/types'

// A Provider is one upstream account: address, credential, and account-level
// quota. The API never returns the credential, so Provider has no key field.
export interface ProviderRouteLimits {
  rpm: number
  tpm: number
  max_concurrency: number
}

export interface Provider {
  route_limits?: ProviderRouteLimits
  id: number
  type: number
  name: string
  status: number
  base_url?: string | null
  balance: number
  balance_updated_time: number
  created_time: number
  remark?: string | null
  channel_info: {
    is_multi_key: boolean
    multi_key_size: number
    multi_key_mode?: 'random' | 'polling'
  }
}

export interface ApiResponse<T = unknown> {
  success: boolean
  message?: string
  data?: T
}

export interface ListProvidersParams {
  p: number
  page_size: number
  keyword?: string
  status?: string
}

export type ListProvidersResponse = ApiResponse<{
  items: Provider[]
  total: number
}>

export type ProviderKeysResponse = MultiKeyStatusResponse

export type ProviderAddMode = 'single' | 'batch' | 'multi_to_single'

export interface CreateProviderPayload {
  mode: ProviderAddMode
  multi_key_mode?: 'random' | 'polling'
  provider: {
    route_limits?: ProviderRouteLimits
    type: number
    name: string
    key: string
    base_url?: string
    remark?: string
    other?: string
    settings?: string
    setting?: string
    header_override?: string
    openai_organization?: string
  }
}

export interface UpdateProviderPayload {
  route_limits?: ProviderRouteLimits
  name?: string
  base_url?: string
  remark?: string
  key?: string
  other?: string
  settings?: string
  setting?: string
  header_override?: string
  openai_organization?: string
  key_mode?: 'append' | 'replace'
}

export type ProvidersDialogType = 'create' | 'update' | 'delete' | 'keys'
