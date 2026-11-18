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
import type { AxiosRequestConfig } from 'axios'

import type { WalletBalance } from '@/features/wallet/types'
import type { PermissionCatalog } from '@/lib/admin-permissions'
import { api } from '@/lib/api'
import type { CustomOAuthBinding } from '@/lib/oauth'
import {
  createServerError,
  requireServerSuccess,
} from '@/lib/server-error-message'
import { walletSummarySchema, type WalletSummary } from '@/lib/wallet-summary'

import type {
  User,
  GetUsersParams,
  GetUsersResponse,
  SearchUsersParams,
  UserFormData,
  ManageUserAction,
  ApiResponse,
} from './types'

// A step-up proof is single-use, so the request carrying it must never be
// replayed by the auth-refresh interceptor; it refreshes first instead.
function securityProofConfig(proofToken?: string): AxiosRequestConfig {
  if (!proofToken) return {}
  return {
    headers: { 'X-Security-Proof': proofToken },
    singleUseAuthorization: true,
  }
}

// ============================================================================
// User Management APIs
// ============================================================================

/**
 * Get paginated users list
 */
export async function getUsers(
  params: GetUsersParams = {},
  signal?: AbortSignal
): Promise<GetUsersResponse> {
  const { p = 1, page_size = 10, sort_by, sort_order } = params
  const res = await api.get('/api/user/', {
    params: {
      p,
      page_size,
      sort_by,
      sort_order,
    },
    signal,
  })
  return res.data
}

/**
 * Search users by keyword or group
 */
export async function searchUsers(
  params: SearchUsersParams,
  signal?: AbortSignal
): Promise<GetUsersResponse> {
  const {
    keyword = '',
    group = '',
    role = '',
    status = '',
    p = 1,
    page_size = 10,
    sort_by,
    sort_order,
  } = params
  const queryParams = new URLSearchParams()
  queryParams.set('keyword', keyword)
  queryParams.set('group', group)
  if (role) queryParams.set('role', role)
  if (status) queryParams.set('status', status)
  queryParams.set('p', String(p))
  queryParams.set('page_size', String(page_size))
  if (sort_by) queryParams.set('sort_by', sort_by)
  if (sort_order) queryParams.set('sort_order', sort_order)
  const res = await api.get(`/api/user/search?${queryParams.toString()}`, {
    signal,
  })
  return res.data
}

/**
 * Get single user by ID
 */
export async function getUser(id: number): Promise<ApiResponse<User>> {
  const res = await api.get(`/api/user/${id}`)
  return res.data
}

/**
 * Create a new user. Creating an administrator requires an
 * `admin.user.create` proof.
 */
export async function createUser(
  data: UserFormData,
  proofToken?: string
): Promise<ApiResponse<User>> {
  const res = await api.post(
    '/api/user/',
    data,
    securityProofConfig(proofToken)
  )
  return res.data
}

/**
 * Update an existing user. Changing the password or the admin permission
 * matrix requires an `admin.user.update` proof.
 */
export async function updateUser(
  data: UserFormData & { id: number },
  proofToken?: string
): Promise<ApiResponse<Partial<User>>> {
  const res = await api.put('/api/user/', data, securityProofConfig(proofToken))
  return res.data
}

/**
 * Delete a single user (hard delete); requires an `admin.user.delete` proof
 */
export async function deleteUser(
  id: number,
  proofToken: string
): Promise<ApiResponse> {
  const res = await api.delete(
    `/api/user/${id}/`,
    securityProofConfig(proofToken)
  )
  return res.data
}

/**
 * Manage user (promote, demote, enable, disable, delete); requires an
 * `admin.user.manage` proof (`admin.user.delete` for deletion)
 */
export async function manageUser(
  id: number,
  action: ManageUserAction,
  proofToken: string
): Promise<ApiResponse<Partial<User>>> {
  const res = await api.post(
    '/api/user/manage',
    { id, action },
    securityProofConfig(proofToken)
  )
  return res.data
}

export async function getUserWalletSummaries(
  userIds: number[],
  signal?: AbortSignal
): Promise<WalletSummary[]> {
  const ids = [...new Set(userIds)].sort((a, b) => a - b)
  if (
    ids.length === 0 ||
    ids.length > 100 ||
    ids.some((id) => !Number.isSafeInteger(id) || id <= 0)
  ) {
    throw new Error('Invalid user ID')
  }
  const res = await api.get('/api/admin/v1/user-wallets', {
    params: { user_ids: ids.join(',') },
    signal,
  })
  const response = requireServerSuccess(res.data)
  const items = walletSummarySchema.array().parse(response.data?.items)
  if (
    items.length !== ids.length ||
    new Set(items.map((item) => item.user_id)).size !== ids.length ||
    items.some((item) => !ids.includes(item.user_id))
  ) {
    throw new Error('Invalid wallet response')
  }
  return items
}

export async function getUserWallet(
  userId: number,
  signal?: AbortSignal
): Promise<ApiResponse<WalletBalance>> {
  const res = await api.get<ApiResponse<WalletBalance>>(
    `/api/admin/v1/user-wallets/${userId}`,
    { signal }
  )
  const response = requireServerSuccess(res.data)
  if (!response.data) throw createServerError(response, 'Failed to load')
  return response
}

export async function adjustUserWallet(
  userId: number,
  payload: { amount: string; remark: string }
): Promise<ApiResponse> {
  const res = await api.post(
    `/api/admin/v1/user-wallets/${userId}/adjustments`,
    payload
  )
  return requireServerSuccess(res.data)
}

/**
 * Reset user's Passkey registration; requires an `admin.user.passkey.reset` proof
 */
export async function resetUserPasskey(
  id: number,
  proofToken: string
): Promise<ApiResponse> {
  const res = await api.delete(
    `/api/user/${id}/reset_passkey`,
    securityProofConfig(proofToken)
  )
  return res.data
}

/**
 * Reset user's Two-Factor Authentication setup; requires an
 * `admin.user.2fa.disable` proof
 */
export async function resetUserTwoFA(
  id: number,
  proofToken: string
): Promise<ApiResponse> {
  const res = await api.delete(
    `/api/user/${id}/2fa`,
    securityProofConfig(proofToken)
  )
  return res.data
}

/**
 * Get all available groups
 */
export async function getGroups(): Promise<ApiResponse<string[]>> {
  const res = await api.get('/api/group/')
  return res.data
}

/**
 * Get the permission catalog (resources, actions, and role baselines).
 * Source of truth lives in the backend authz package.
 */
export async function getPermissionCatalog(): Promise<PermissionCatalog> {
  const res = await api.get('/api/authz/catalog')
  requireServerSuccess(res.data)
  return {
    resources: res.data?.data?.resources ?? [],
    roles: res.data?.data?.roles ?? [],
  }
}

// ============================================================================
// Admin Binding Management APIs
// ============================================================================

/**
 * Get user's custom OAuth bindings (admin)
 */
export async function getUserOAuthBindings(
  userId: number
): Promise<ApiResponse<CustomOAuthBinding[]>> {
  const res = await api.get(`/api/user/${userId}/oauth/bindings`)
  return res.data
}

/**
 * Clear a user's built-in binding (admin); requires an
 * `admin.user.binding.clear` proof bound to the binding type
 */
export async function adminClearUserBinding(
  userId: number,
  bindingType: string,
  proofToken: string
): Promise<ApiResponse> {
  const res = await api.delete(
    `/api/user/${userId}/bindings/${bindingType}`,
    securityProofConfig(proofToken)
  )
  return res.data
}

/**
 * Unbind custom OAuth for a user (admin); requires an
 * `admin.user.binding.clear` proof bound to the provider ID
 */
export async function adminUnbindCustomOAuth(
  userId: number,
  providerId: number,
  proofToken: string
): Promise<ApiResponse> {
  const res = await api.delete(
    `/api/user/${userId}/oauth/bindings/${providerId}`,
    securityProofConfig(proofToken)
  )
  return res.data
}
