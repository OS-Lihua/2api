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

export type RequestScope = 'all' | 'self'
export type RequestStatus =
  | 'success'
  | 'failed'
  | 'cancelled'
  | 'partial'
  | 'settlement_pending'
export type UsageSource = 'upstream' | 'estimated' | 'none'
export interface RequestUsage {
  prompt_tokens?: number
  completion_tokens?: number
  total_tokens?: number
}
export interface RequestLog {
  id: string
  user_id: number
  token_id?: number
  model_offering_id?: number
  offering_name?: string
  requested_name?: string
  sale_price_version_id?: number
  sale_currency?: string
  sale_fx_rate?: string
  usage?: RequestUsage
  usage_source?: UsageSource
  group_ratio?: string
  receivable?: MoneyAmount
  sale_pending?: boolean
  ledger_entry_ids?: string[]
  started_at: number
  ended_at?: number
  status: RequestStatus
  error_code?: string
}
export interface RouteDiagnostics {
  method: 'single_channel'
  candidate_count: number
  eligible_count: number
  excluded_counts: Record<string, number>
  excluded: { channel_id: number; reason: string }[]
}
export interface RequestAttempt {
  id: string
  request_id: string
  started_at: number
  ended_at?: number
  status: string
  error_class?: string
  usage?: RequestUsage
  usage_source?: UsageSource
  conversion_losses?: string[]
  channel_id?: number
  provider_id?: number
  model_id?: number
  member_tier?: number
  purchase_price_version_id?: number
  cost?: MoneyAmount
  cost_pending?: boolean
  accounting_fx_rate?: string
  converted_cost?: MoneyAmount
  failover_decision?: string
  failover_reason?: string
  circuit_action?: string
  route_diagnostics?: RouteDiagnostics
}
export interface RequestFilters {
  model_offering_id?: number
  token_id?: number
  user_id?: number
  status?: RequestStatus
  started_after?: number
  started_before?: number
}
export interface RequestPage<T> {
  items: T[]
  total: number
  page: number
  page_size: number
}
export interface RequestResponse<T> {
  success: boolean
  message?: string
  data?: RequestPage<T>
}
