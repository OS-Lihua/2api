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
import {
  createServerError,
  requireServerSuccess,
} from '@/lib/server-error-message'

import type {
  RequestAttempt,
  RequestFilters,
  RequestLog,
  RequestResponse,
  RequestScope,
} from './types'

const paths = { all: '/api/admin/v1/requests', self: '/api/requests/self' }

export async function getRequests(
  scope: RequestScope,
  params: RequestFilters & { page: number; page_size: number },
  signal: AbortSignal
) {
  const { user_id, ...common } = params
  const response = await api.get<RequestResponse<RequestLog>>(paths[scope], {
    params: scope === 'all' ? params : common,
    signal,
  })
  const payload = requireServerSuccess(response.data)
  if (!payload.data || !Array.isArray(payload.data.items)) {
    throw createServerError(payload)
  }
  return payload.data
}

export async function getRequestAttempts(
  scope: RequestScope,
  requestId: string,
  page: number,
  signal: AbortSignal
) {
  const response = await api.get<RequestResponse<RequestAttempt>>(
    `${paths[scope]}/${encodeURIComponent(requestId)}/attempts`,
    { params: { page, page_size: 20 }, signal }
  )
  const payload = requireServerSuccess(response.data)
  if (!payload.data || !Array.isArray(payload.data.items)) {
    throw createServerError(payload)
  }
  return payload.data
}
