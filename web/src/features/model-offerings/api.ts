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
import { isAxiosError } from 'axios'

import type { Option } from '@/components/multi-select'
import { getGroups } from '@/features/users/api'
import { api } from '@/lib/api'
import { requireServerSuccess } from '@/lib/server-error-message'

import type {
  ApiResult,
  ModelOffering,
  ModelOfferingInput,
  OfferingListParams,
  OfferingPage,
} from './types'

export const offeringsPath = '/api/admin/v1/model-offerings'
export const offeringKeys = {
  all: ['model-offerings'] as const,
  list: (params: OfferingListParams) =>
    ['model-offerings', 'list', params] as const,
}

export async function listOfferings(
  params: OfferingListParams
): Promise<OfferingPage> {
  const response = await api.get(offeringsPath, { params })
  return requireServerSuccess<ApiResult<OfferingPage>>(response.data).data
}

export async function getOffering(id: number): Promise<ModelOffering> {
  const response = await api.get(`${offeringsPath}/${id}`)
  return requireServerSuccess<ApiResult<ModelOffering>>(response.data).data
}

export async function saveOffering(
  id: number | null,
  payload: ModelOfferingInput
): Promise<void> {
  const response =
    id === null
      ? await api.post(offeringsPath, payload)
      : await api.put(`${offeringsPath}/${id}`, payload)
  requireServerSuccess(response.data)
}

export type OfferingAction = 'enable' | 'disable' | 'delete'

export async function offeringAction(
  id: number,
  action: OfferingAction
): Promise<void> {
  const response =
    action === 'delete'
      ? await api.delete(`${offeringsPath}/${id}`)
      : await api.post(`${offeringsPath}/${id}/${action}`)
  requireServerSuccess(response.data)
}

interface ChoiceRecord {
  id: number
  name?: string
  provider_id?: number
  protocol?: string
  upstream_model_name?: string
}

async function listAllChoices(path: string): Promise<ChoiceRecord[]> {
  const items: ChoiceRecord[] = []
  for (let page = 1; ; page++) {
    const response = await api.get(path, {
      params: { p: page, page_size: 100 },
    })
    const result = requireServerSuccess<
      ApiResult<{ items: ChoiceRecord[]; total: number }>
    >(response.data).data
    items.push(...result.items)
    if (result.items.length === 0 || items.length >= result.total) return items
  }
}

export async function listScopeChoices(
  path: string
): Promise<ChoiceRecord[] | null> {
  try {
    return await listAllChoices(path)
  } catch (error) {
    if (isAxiosError(error) && error.response?.status === 403) return null
    throw error
  }
}

export function channelChoice(
  channel: ChoiceRecord,
  providerNames?: ReadonlyMap<number, string | undefined>
): Option {
  return {
    value: String(channel.id),
    label: `#${channel.id} · ${channel.upstream_model_name} · ${channel.protocol} · ${providerNames?.get(channel.provider_id ?? 0) ?? `#${channel.provider_id}`}`,
  }
}

export interface OfferingChoices {
  models: Option[]
  providers: Option[] | null
  channels: Option[] | null
  groups: Option[]
}

export async function getOfferingChoices(): Promise<OfferingChoices> {
  const [models, providers, channels, groups] = await Promise.all([
    listAllChoices('/api/admin/v1/models'),
    listScopeChoices('/api/admin/v1/providers'),
    listScopeChoices('/api/admin/v1/channels'),
    getGroups().then(requireServerSuccess),
  ])
  const providerNames = new Map(
    (providers ?? []).map((provider) => [provider.id, provider.name])
  )
  return {
    models: models.map((item) => ({
      value: String(item.id),
      label: item.name ?? `#${item.id}`,
    })),
    providers:
      providers?.map((item) => ({
        value: String(item.id),
        label: item.name ?? `#${item.id}`,
      })) ?? null,
    channels:
      channels?.map((item) => channelChoice(item, providerNames)) ?? null,
    groups: (groups.data ?? []).map((group) => ({
      value: group,
      label: group,
    })),
  }
}
