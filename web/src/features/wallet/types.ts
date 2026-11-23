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

export type CheckoutProvider =
  | 'epay'
  | 'stripe'
  | 'creem'
  | 'waffo'
  | 'waffo_pancake'

export interface CheckoutProduct {
  product_id: string
  name: string
  source_amount: MoneyAmount
}

export interface CheckoutProviderInfo {
  provider: CheckoutProvider
  configured: boolean
  enabled: boolean
  selection_kind: 'amount' | 'product'
  minimum?: MoneyAmount
  presets: MoneyAmount[]
  products: CheckoutProduct[]
}

export interface CheckoutQuoteRequest {
  provider: CheckoutProvider
  payment_method?: string
  pay_method_index?: number
  selection:
    | { kind: 'amount'; source_amount: MoneyAmount }
    | { kind: 'product'; product_id: string; quantity?: string }
}

export interface CheckoutQuote {
  quote_id: string
  expires_at: number
  charge_amount: MoneyAmount
  charge_final: boolean
  source_amount: MoneyAmount
  credit_amount: MoneyAmount
  fx: { from_currency: string; to_currency: string; rate: string } | null
}

export interface CheckoutPaymentResponse extends ApiResponse<unknown> {
  url?: string
}

export interface ApiResponse<T = unknown> {
  success?: boolean
  message?: string
  data?: T
}

export type TopupInfoResponse = ApiResponse<TopupInfo>
export type RedemptionResponse = ApiResponse<MoneyAmount>
export type AffiliateCodeResponse = ApiResponse<string>
export type AffiliateTransferResponse = ApiResponse

export interface PaymentMethod {
  name: string
  type: string
  color?: string
  icon?: string
}

export interface WaffoPayMethod {
  name: string
  icon?: string
  payMethodType?: string
  payMethodName?: string
}

export interface TopupInfo {
  checkout?: { providers: CheckoutProviderInfo[] }
  enable_online_topup: boolean
  enable_stripe_topup: boolean
  pay_methods: PaymentMethod[]
  topup_link?: string
  enable_creem_topup?: boolean
  enable_waffo_topup?: boolean
  waffo_pay_methods?: WaffoPayMethod[]
  enable_waffo_pancake_topup?: boolean
  enable_redemption?: boolean
  payment_compliance_confirmed?: boolean
  payment_compliance_terms_version?: string
}

export interface RedemptionRequest {
  key: string
}

export interface AffiliateTransferRequest {
  amount: MoneyAmount
  transfer_id: string
}

export type { ReferralRewards } from '@/lib/referral-rewards'

export interface WalletBalance {
  user_id: number
  currency: string
  balance: string
  updated_at: number
  outstanding?: string
  requests_paused?: boolean
  funding_pending?: boolean
}

export interface UserWalletData {
  id: number
  username: string
  request_count: number
  aff_count: number
  group: string
}

export type TopupStatus =
  | 'quoted'
  | 'pending'
  | 'success'
  | 'expired'
  | 'failed'

export interface CheckoutOrder extends Omit<CheckoutQuote, 'quote_id'> {
  unit_charge_amount: MoneyAmount
  fx_frozen_at: number
  accepted_at: number
  creation_state: 'quoted' | 'creating' | 'created' | 'uncertain'
  paid_amount: MoneyAmount | null
  paid_at: number
  fulfillment_pending: boolean
  product_id?: string
  quantity?: string
  provider_session_id?: string
}

interface TopupRecordBase {
  id: number
  user_id: number
  trade_no: string
  payment_method: string
  create_time: number
  complete_time?: number
  status: TopupStatus
}

export type TopupRecord = TopupRecordBase &
  (
    | {
        contract_version: 1
        purpose: 'wallet_topup'
        legacy_amount: null
        legacy_money: null
        checkout: CheckoutOrder
      }
    | {
        contract_version: 0
        purpose: 'legacy_topup' | 'subscription_history'
        legacy_amount: string | null
        legacy_money: string | null
        checkout?: never
      }
  )

export interface BillingHistoryResponse {
  items: TopupRecord[]
  total: number
}

export interface CompleteOrderRequest {
  trade_no: string
}
