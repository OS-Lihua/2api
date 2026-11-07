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

import type {
  ApiResponse,
  CreateProviderPayload,
  ListProvidersParams,
  ListProvidersResponse,
  Provider,
  ProviderKeysResponse,
  UpdateProviderPayload,
} from './types'

const PROVIDERS_PATH = '/api/admin/v1/providers'

export async function listProviders(
  params: ListProvidersParams
): Promise<ListProvidersResponse> {
  const res = await api.get(PROVIDERS_PATH, { params })
  return res.data
}

export async function getProvider(id: number): Promise<ApiResponse<Provider>> {
  const res = await api.get(`${PROVIDERS_PATH}/${id}`)
  return res.data
}

export async function createProvider(
  payload: CreateProviderPayload
): Promise<ApiResponse<{ ids: number[] }>> {
  const res = await api.post(PROVIDERS_PATH, payload)
  return res.data
}

export async function updateProvider(
  id: number,
  payload: UpdateProviderPayload
): Promise<ApiResponse<Provider>> {
  const res = await api.patch(`${PROVIDERS_PATH}/${id}`, payload)
  return res.data
}

export async function deleteProvider(id: number): Promise<ApiResponse> {
  const res = await api.delete(`${PROVIDERS_PATH}/${id}`)
  return res.data
}

export async function setProviderEnabled(
  id: number,
  enabled: boolean
): Promise<ApiResponse<Provider>> {
  const action = enabled ? 'enable' : 'disable'
  const res = await api.post(`${PROVIDERS_PATH}/${id}/${action}`)
  return res.data
}

export async function refreshProviderBalance(
  id: number
): Promise<ApiResponse<Provider & { raw_response?: string }>> {
  const res = await api.post(`${PROVIDERS_PATH}/${id}/balance`)
  return res.data
}

export async function listProviderKeys(
  id: number,
  page: number,
  pageSize: number,
  status?: number
): Promise<ProviderKeysResponse> {
  const res = await api.get(`${PROVIDERS_PATH}/${id}/keys`, {
    params: { p: page, page_size: pageSize, status },
  })
  return res.data
}

async function postProviderKeyAction(
  path: string,
  method: 'post' | 'delete' = 'post'
): Promise<ApiResponse> {
  const res = method === 'post' ? await api.post(path) : await api.delete(path)
  return res.data
}

export const providerKeyApi = {
  getStatus: listProviderKeys,
  enable: (id: number, index: number) =>
    postProviderKeyAction(`${PROVIDERS_PATH}/${id}/keys/${index}/enable`),
  disable: (id: number, index: number) =>
    postProviderKeyAction(`${PROVIDERS_PATH}/${id}/keys/${index}/disable`),
  remove: (id: number, index: number) =>
    postProviderKeyAction(`${PROVIDERS_PATH}/${id}/keys/${index}`, 'delete'),
  enableAll: (id: number) =>
    postProviderKeyAction(`${PROVIDERS_PATH}/${id}/keys/enable`),
  disableAll: (id: number) =>
    postProviderKeyAction(`${PROVIDERS_PATH}/${id}/keys/disable`),
  deleteDisabled: (id: number) =>
    postProviderKeyAction(`${PROVIDERS_PATH}/${id}/keys/disabled`, 'delete'),
}

export const providersQueryKeys = {
  all: ['providers'] as const,
  lists: () => [...providersQueryKeys.all, 'list'] as const,
  list: (params: ListProvidersParams) =>
    [...providersQueryKeys.lists(), params] as const,
}
