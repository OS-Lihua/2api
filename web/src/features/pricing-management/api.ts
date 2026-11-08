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

import type { ComboboxInputOption } from '@/components/ui/combobox-input'
import { channelChoice, listScopeChoices } from '@/features/model-offerings/api'
import { api } from '@/lib/api'
import { requireServerSuccess } from '@/lib/server-error-message'

import type {
  MarginCheck,
  ApiResult,
  FxQuote,
  FxRate,
  FxRateInput,
  OwnerType,
  Page,
  PriceListParams,
  PriceVersion,
  PriceVersionInput,
} from './types'

export const priceVersionsPath = '/api/admin/v1/price-versions'
export const pricingKeys = {
  prices: ['price-versions'] as const,
  fx: ['fx-rates'] as const,
  list: (params: PriceListParams) =>
    ['price-versions', 'list', params] as const,
}

export const fxRatesPath = '/api/admin/v1/fx-rates'

export async function listFxRates(params: {
  p: number
  page_size: number
}): Promise<Page<FxRate>> {
  const response = await api.get(fxRatesPath, { params })
  return requireServerSuccess<ApiResult<Page<FxRate>>>(response.data).data
}

export async function getFxQuote(
  from_currency: string,
  to_currency: string
): Promise<FxQuote> {
  const response = await api.get(`${fxRatesPath}/quote`, {
    params: { from_currency, to_currency },
  })
  return requireServerSuccess<ApiResult<FxQuote>>(response.data).data
}

export class MarginConfirmationRequired extends Error {
  constructor(public readonly report: MarginCheck) {
    super('Negative margin requires confirmation')
  }
}

function throwMarginConfirmation(error: unknown): never {
  if (
    isAxiosError(error) &&
    error.response?.status === 409 &&
    error.response.data?.message_key === 'Negative margin requires confirmation'
  ) {
    throw new MarginConfirmationRequired(
      error.response.data.data as MarginCheck
    )
  }
  throw error
}

export async function listMarginEstimates(): Promise<MarginCheck> {
  const response = await api.get(`${priceVersionsPath}/margins`)
  return requireServerSuccess<ApiResult<MarginCheck>>(response.data).data
}

export async function saveFxRate(payload: FxRateInput): Promise<void> {
  try {
    const response = await api.put(fxRatesPath, payload)
    requireServerSuccess(response.data)
  } catch (error) {
    throwMarginConfirmation(error)
  }
}

export async function getCurrentPrice(
  owner_type: OwnerType,
  owner_id: number
): Promise<PriceVersion | null> {
  try {
    const response = await api.get(`${priceVersionsPath}/current`, {
      params: { owner_type, owner_id },
    })
    return requireServerSuccess<ApiResult<PriceVersion>>(response.data).data
  } catch (error) {
    if (isAxiosError(error) && error.response?.status === 404) return null
    throw error
  }
}

export async function createPriceVersion(
  payload: PriceVersionInput
): Promise<void> {
  try {
    const response = await api.post(priceVersionsPath, payload)
    requireServerSuccess(response.data)
  } catch (error) {
    throwMarginConfirmation(error)
  }
}

export async function getPriceOwners(
  type: OwnerType
): Promise<ComboboxInputOption[] | null> {
  const path =
    type === 'channel'
      ? '/api/admin/v1/channels'
      : '/api/admin/v1/model-offerings'
  const owners = await listScopeChoices(path)
  return (
    owners?.map((owner) =>
      type === 'channel'
        ? channelChoice(owner)
        : {
            value: String(owner.id),
            label: `#${owner.id} · ${owner.name}`,
          }
    ) ?? null
  )
}

export async function listPriceVersions(
  params: PriceListParams
): Promise<Page<PriceVersion>> {
  const response = await api.get(priceVersionsPath, { params })
  return requireServerSuccess<ApiResult<Page<PriceVersion>>>(response.data).data
}
