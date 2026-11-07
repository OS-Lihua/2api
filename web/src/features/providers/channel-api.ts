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
import type { ModelCapabilities } from '@/features/models/types'
import { api } from '@/lib/api'
import { requireServerSuccess } from '@/lib/server-error-message'

export interface SingleModelChannel {
  id: number
  provider_id: number
  model_id: number
  protocol: string
  upstream_model_name: string
  status: 'pending' | 'enabled' | 'disabled'
  mapping_confirmed: boolean
  capabilities: ModelCapabilities | null
  parameter_overrides: Record<string, unknown>
  rpm: number
  tpm: number
  max_concurrency: number
  weight: number
  latest_probe?: ChannelProbeResult
}
export interface ChannelModel {
  id: number
  name: string
  capabilities: ModelCapabilities
}
export const channelKeys = {
  all: ['single-model-channels'] as const,
  list: (providerId: number, page: number) =>
    ['single-model-channels', providerId, page] as const,
}
const path = '/api/admin/v1/channels'
export async function listSingleModelChannels(
  providerId: number,
  page: number
) {
  const response = await api.get(path, {
    params: { provider_id: providerId, p: page, page_size: 10 },
  })
  return requireServerSuccess<{
    success: boolean
    data: { items: SingleModelChannel[]; total: number }
  }>(response.data).data
}
export interface ChannelProbeResult {
  id: number
  capability: string
  protocol: string
  started_at: number
  finished_at: number
  status: 'running' | 'passed' | 'failed' | 'inconclusive'
  reason: string
  tested_value?: number
}
export async function listChannelProbes(id: number) {
  const response = await api.get(`${path}/${id}/probes`)
  return requireServerSuccess<{
    success: boolean
    data: { capabilities: string[]; results: ChannelProbeResult[] }
  }>(response.data).data
}
export async function runChannelProbe(id: number, capability: string) {
  const response = await api.post(
    `${path}/${id}/probes`,
    { capability },
    { timeout: 100_000 }
  )
  return requireServerSuccess<{ success: boolean; data: ChannelProbeResult }>(
    response.data
  ).data
}
export async function channelAction(id: number, action: string) {
  const response =
    action === 'delete'
      ? await api.delete(`${path}/${id}`)
      : await api.post(`${path}/${id}/${action}`)
  return requireServerSuccess(response.data)
}
export async function saveSingleModelChannel(
  id: number | undefined,
  body: unknown
) {
  const response =
    id === undefined
      ? await api.post(path, body)
      : await api.patch(`${path}/${id}`, body)
  return requireServerSuccess(response.data)
}
export async function findChannelModels(keyword: string) {
  const response = await api.get('/api/admin/v1/models', {
    params: { keyword, page_size: 100 },
  })
  return requireServerSuccess<{
    success: boolean
    data: { items: ChannelModel[] }
  }>(response.data).data
}
export async function suggestChannelModels(name: string) {
  const response = await api.get(`${path}/suggest-models`, {
    params: { upstream_model_name: name },
  })
  return requireServerSuccess<{
    success: boolean
    data: { items: ChannelModel[]; selected_model_id: number | null }
  }>(response.data).data
}
