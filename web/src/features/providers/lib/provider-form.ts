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
import type { TFunction } from 'i18next'
import { z } from 'zod'

import type {
  CreateProviderPayload,
  Provider,
  ProviderAddMode,
  UpdateProviderPayload,
} from '../types'

export function getProviderFormSchema(t: TFunction, isUpdate: boolean) {
  return z
    .object({
      type: z.number().int().positive(),
      name: z.string().trim().min(1, t('Name is required')),
      base_url: z.string().trim(),
      remark: z.string().trim().max(255),
      key: z.string(),
      mode: z.enum(['single', 'batch', 'multi_to_single']),
      multi_key_mode: z.enum(['random', 'polling']),
      key_mode: z.enum(['append', 'replace']),
    })
    .refine((values) => isUpdate || values.key.trim() !== '', {
      path: ['key'],
      message: t('Key is required'),
    })
}

export type ProviderFormValues = z.infer<
  ReturnType<typeof getProviderFormSchema>
>

export const PROVIDER_FORM_DEFAULT_VALUES: ProviderFormValues = {
  type: 1,
  name: '',
  base_url: '',
  remark: '',
  key: '',
  mode: 'single' satisfies ProviderAddMode,
  multi_key_mode: 'random',
  key_mode: 'append',
}

export function providerToFormValues(provider: Provider): ProviderFormValues {
  return {
    ...PROVIDER_FORM_DEFAULT_VALUES,
    type: provider.type,
    name: provider.name,
    base_url: provider.base_url ?? '',
    remark: provider.remark ?? '',
    multi_key_mode: provider.channel_info?.multi_key_mode ?? 'random',
  }
}

export function toCreateProviderPayload(
  values: ProviderFormValues
): CreateProviderPayload {
  const payload: CreateProviderPayload = {
    mode: values.mode,
    provider: {
      type: values.type,
      name: values.name.trim(),
      key: values.key,
    },
  }
  if (values.base_url) payload.provider.base_url = values.base_url
  if (values.remark) payload.provider.remark = values.remark
  if (values.mode === 'multi_to_single') {
    payload.multi_key_mode = values.multi_key_mode
  }
  return payload
}

// An empty key keeps the stored credential. A multi-key Provider either
// appends the new keys or replaces all of them.
export function toUpdateProviderPayload(
  values: ProviderFormValues,
  provider: Provider
): UpdateProviderPayload {
  const payload: UpdateProviderPayload = {
    name: values.name.trim(),
    base_url: values.base_url,
    remark: values.remark,
  }
  if (values.key.trim() === '') return payload
  payload.key = values.key
  if (provider.channel_info?.is_multi_key) payload.key_mode = values.key_mode
  return payload
}
