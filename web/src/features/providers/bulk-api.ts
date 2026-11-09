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
import { throwMarginConfirmation } from '@/features/pricing-management/api'
import type { MarginCheck } from '@/features/pricing-management/types'
import { api } from '@/lib/api'
import { requireServerSuccess } from '@/lib/server-error-message'

export type ImportPreviewRow = {
  row: number
  resource: string
  action: string
  id?: number
  status?: string
  error?: string
  provider_id?: number
  provider_row?: number
  model_id?: number
  protocol?: string
  provider_type?: number
  owner_type?: string
  owner_id?: number
  owner_row?: number
  currency?: string
  effective_at?: number
}

export type ImportResult = {
  margin_check?: MarginCheck
  dry_run: boolean
  valid: boolean
  committed: boolean
  rows: ImportPreviewRow[]
}

export type ImportTarget =
  | 'bulk-import'
  | 'providers/import'
  | 'channels/import'
  | 'price-versions/import'
  | 'channels/bulk-confirm'
  | 'channels/bulk-enable'

export async function importBatch(
  source: string,
  format: string,
  dryRun: boolean,
  target: ImportTarget,
  approval?: MarginCheck
): Promise<ImportResult> {
  try {
    const response = await api.post(`/api/admin/v1/${target}`, source, {
      params: { dry_run: dryRun },
      headers: {
        'Content-Type': format === 'csv' ? 'text/csv' : 'application/json',
        ...(approval
          ? {
              'X-Margin-Confirmation': approval.revision,
              'X-Margin-Effective-At': String(approval.effective_at),
            }
          : {}),
      },
      validateStatus: (status) => status === 200 || status === 400,
    })
    // Row validation failures are displayed in the preview table. Only an
    // explicit committed result represents a successful write.
    if (response.data.data?.rows) return response.data.data
    requireServerSuccess(response.data)
    return response.data.data
  } catch (error) {
    throwMarginConfirmation(error)
  }
}

export const importExample = JSON.stringify(
  {
    rows: [
      {
        resource: 'providers',
        action: 'create',
        ref: 'account',
        data: {
          name: 'Provider account',
          type: 1,
          key: 'REPLACE_KEY',
          base_url: 'https://api.example.com',
        },
      },
      {
        resource: 'channels',
        action: 'create',
        ref: 'channel',
        data: {
          provider_ref: 'account',
          model_id: 1,
          protocol: 'openai_chat',
          upstream_model_name: 'example-model',
        },
      },
      {
        resource: 'channels',
        action: 'confirm',
        data: { channel_ref: 'channel' },
      },
      {
        resource: 'price-versions',
        action: 'create',
        data: {
          owner_type: 'channel',
          owner_ref: 'channel',
          currency: 'USD',
          expression: 'tier("base", p * 2 + c * 8)',
        },
      },
      {
        resource: 'channels',
        action: 'enable',
        data: { channel_ref: 'channel' },
      },
    ],
  },
  null,
  2
)
