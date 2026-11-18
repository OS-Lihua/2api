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
import { api } from '@/lib/api'
import { referralRewardsSchema } from '@/lib/referral-rewards'
import {
  createServerError,
  requireServerSuccess,
} from '@/lib/server-error-message'

import { checkoutQuoteSchema } from './lib/checkout'
import type {
  WalletBalance,
  ReferralRewards,
  CheckoutProvider,
  CheckoutQuoteRequest,
  CheckoutQuote,
  CheckoutPaymentResponse,
  RedemptionRequest,
  AffiliateTransferRequest,
  ApiResponse,
  TopupInfoResponse,
  RedemptionResponse,
  AffiliateCodeResponse,
  AffiliateTransferResponse,
  BillingHistoryResponse,
  CompleteOrderRequest,
} from './types'

export function isApiSuccess(response: ApiResponse): boolean {
  return response.success === true || response.message === 'success'
}

export async function getSelfWallet(
  signal?: AbortSignal
): Promise<WalletBalance> {
  const response = await api.get<ApiResponse<WalletBalance>>(
    '/api/user/self/wallet',
    { signal }
  )
  requireServerSuccess(response.data)
  if (!response.data.data) throw createServerError(response.data)
  return response.data.data
}

export async function getTopupInfo(
  signal?: AbortSignal
): Promise<TopupInfoResponse> {
  const response = await api.get('/api/user/topup/info', { signal })
  return response.data
}

export async function requestCheckoutQuote(
  request: CheckoutQuoteRequest
): Promise<CheckoutQuote> {
  if (
    request.provider === 'waffo' &&
    (request.pay_method_index === undefined ||
      !Number.isSafeInteger(request.pay_method_index) ||
      request.pay_method_index < 0)
  ) {
    throw new Error('Select a payment method')
  }
  const response = await api.post<ApiResponse<CheckoutQuote>>(
    '/api/user/topup/quote',
    request,
    { skipBusinessError: true } as Record<string, unknown>
  )
  requireServerSuccess(response.data)
  const result = checkoutQuoteSchema.safeParse(response.data.data)
  if (!result.success) throw new Error('Quote unavailable')
  return result.data
}

export async function requestCheckoutPayment(
  provider: CheckoutProvider,
  quoteId: string
): Promise<CheckoutPaymentResponse> {
  const endpoint =
    provider === 'epay'
      ? '/api/user/pay'
      : `/api/user/${provider.replace('_', '-')}/pay`
  const response = await api.post<CheckoutPaymentResponse>(
    endpoint,
    { quote_id: quoteId },
    { skipBusinessError: true } as Record<string, unknown>
  )
  if (!isApiSuccess(response.data)) {
    throw createServerError(response.data, 'Payment request failed')
  }
  return response.data
}

export async function redeemTopupCode(
  request: RedemptionRequest
): Promise<RedemptionResponse> {
  const response = await api.post('/api/user/topup', request)
  return response.data
}

export async function getReferralRewards(
  signal?: AbortSignal
): Promise<ReferralRewards> {
  const response = await api.get<ApiResponse<ReferralRewards>>(
    '/api/user/referral-rewards',
    { signal }
  )
  requireServerSuccess(response.data)
  const parsed = referralRewardsSchema.safeParse(response.data.data)
  if (!parsed.success) throw new Error('Referral rewards unavailable')
  return parsed.data
}

export async function getAffiliateCode(
  signal?: AbortSignal
): Promise<AffiliateCodeResponse> {
  const response = await api.get('/api/user/aff', { signal })
  return response.data
}

export async function transferAffiliateQuota(
  request: AffiliateTransferRequest
): Promise<AffiliateTransferResponse> {
  const response = await api.post('/api/user/aff_transfer', request, {
    skipBusinessError: true,
  } as Record<string, unknown>)
  return response.data
}

export async function getUserBillingHistory(
  page: number,
  pageSize: number,
  keyword?: string,
  signal?: AbortSignal
): Promise<ApiResponse<BillingHistoryResponse>> {
  const params = new URLSearchParams({
    p: page.toString(),
    page_size: pageSize.toString(),
  })
  if (keyword) params.append('keyword', keyword)
  const response = await api.get(`/api/user/topup/self?${params.toString()}`, {
    signal,
  })
  return response.data
}

export async function getAllBillingHistory(
  page: number,
  pageSize: number,
  keyword?: string,
  signal?: AbortSignal
): Promise<ApiResponse<BillingHistoryResponse>> {
  const params = new URLSearchParams({
    p: page.toString(),
    page_size: pageSize.toString(),
  })
  if (keyword) params.append('keyword', keyword)
  const response = await api.get(`/api/user/topup?${params.toString()}`, {
    signal,
  })
  return response.data
}

export async function completeOrder(
  request: CompleteOrderRequest
): Promise<ApiResponse> {
  const response = await api.post('/api/user/topup/complete', request)
  return response.data
}
