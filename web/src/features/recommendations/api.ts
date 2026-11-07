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
import { requireServerSuccess } from '@/lib/server-error-message'

export type RecommendationKind = 'provider' | 'publisher'
export interface RecommendationRule {
  id: number
  kind: RecommendationKind
  pattern: string
  priority: number
  enabled: boolean
  provider_name: string
  provider_type: number
  publisher_id: number
  publisher_name: string
}
export type RecommendationInput = {
  url?: string
  type?: number
  model_name?: string
}
const rulesPath = '/api/admin/v1/recommendation-rules'

export async function queryRecommendations(
  kind: RecommendationKind,
  input: RecommendationInput
): Promise<RecommendationRule[]> {
  const response = await api.get(`/api/admin/v1/recommendations/${kind}`, {
    params: input,
  })
  return requireServerSuccess(response.data).data.items
}
export async function listRecommendationRules(
  kind: RecommendationKind,
  page: number
): Promise<{ items: RecommendationRule[]; total: number }> {
  const response = await api.get(rulesPath, {
    params: { kind, p: page, page_size: 20 },
  })
  return requireServerSuccess(response.data).data
}
export async function saveRecommendationRule(
  rule: RecommendationRule
): Promise<void> {
  const response = rule.id
    ? await api.put(`${rulesPath}/${rule.id}`, rule)
    : await api.post(rulesPath, rule)
  requireServerSuccess(response.data)
}
export async function deleteRecommendationRule(id: number): Promise<void> {
  const response = await api.delete(`${rulesPath}/${id}`)
  requireServerSuccess(response.data)
}
