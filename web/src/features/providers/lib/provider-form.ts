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

import { channelFormSchema } from '@/features/channels/lib/channel-form'

import type {
  CreateProviderPayload,
  Provider,
  ProviderAddMode,
  UpdateProviderPayload,
} from '../types'
import { channelFormSchema as routeChannelFormSchema } from './channel-form'

export function getProviderFormSchema(t: TFunction, isUpdate: boolean) {
  return z
    .object({
      type: z.number().int().positive(),
      rpm: routeChannelFormSchema.shape.rpm.refine(
        (value) => Number(value) <= 1_000_000_000,
        t('Invalid parameters')
      ),
      tpm: routeChannelFormSchema.shape.tpm.refine(
        (value) => Number(value) <= 1_000_000_000_000,
        t('Invalid parameters')
      ),
      max_concurrency: routeChannelFormSchema.shape.max_concurrency.refine(
        (value) => Number(value) <= 1_000_000,
        t('Invalid parameters')
      ),
      name: z.string().trim().min(1, t('Name is required')),
      base_url: z.string().trim(),
      remark: z.string().trim().max(255),
      key: z.string(),
      mode: z.enum(['single', 'batch', 'multi_to_single']),
      multi_key_mode: z.enum(['random', 'polling']),
      key_mode: z.enum(['append', 'replace']),
      other: z.string(),
      settings: channelFormSchema.shape.settings,
      setting: channelFormSchema.shape.setting,
      header_override: channelFormSchema.shape.header_override,
      openai_organization: z.string(),
    })
    .refine((values) => isUpdate || values.key.trim() !== '', {
      path: ['key'],
      message: t('Key is required'),
    })
    .superRefine((values, ctx) => {
      if (
        !isUpdate &&
        [3, 18, 21, 39, 41, 49].includes(values.type) &&
        !values.other.trim()
      ) {
        ctx.addIssue({
          code: 'custom',
          path: ['other'],
          message: t('This channel type requires additional configuration'),
        })
      }
      if (values.type === 41 && values.other.trim().startsWith('{')) {
        try {
          const regions: unknown = JSON.parse(values.other)
          if (
            !regions ||
            typeof regions !== 'object' ||
            !('default' in regions)
          ) {
            throw new Error()
          }
        } catch {
          ctx.addIssue({
            code: 'custom',
            path: ['other'],
            message: t('Deployment region must include a default field'),
          })
        }
      }
    })
}

export type ProviderFormValues = z.infer<
  ReturnType<typeof getProviderFormSchema>
>

export const PROVIDER_FORM_DEFAULT_VALUES: ProviderFormValues = {
  type: 1,
  rpm: '0',
  tpm: '0',
  max_concurrency: '0',
  name: '',
  base_url: '',
  remark: '',
  key: '',
  mode: 'single' satisfies ProviderAddMode,
  multi_key_mode: 'random',
  key_mode: 'append',
  other: '',
  settings: '',
  setting: '',
  header_override: '',
  openai_organization: '',
}

export function providerToFormValues(provider: Provider): ProviderFormValues {
  return {
    ...PROVIDER_FORM_DEFAULT_VALUES,
    type: provider.type,
    rpm: String(provider.route_limits?.rpm ?? 0),
    tpm: String(provider.route_limits?.tpm ?? 0),
    max_concurrency: String(provider.route_limits?.max_concurrency ?? 0),
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
  if (
    Number(values.rpm) ||
    Number(values.tpm) ||
    Number(values.max_concurrency)
  ) {
    payload.provider.route_limits = {
      rpm: Number(values.rpm),
      tpm: Number(values.tpm),
      max_concurrency: Number(values.max_concurrency),
    }
  }
  if (values.remark) payload.provider.remark = values.remark
  Object.assign(payload.provider, toAdapterConfiguration(values))
  if (values.mode === 'multi_to_single') {
    payload.multi_key_mode = values.multi_key_mode
  }
  return payload
}

// Adapter fields are write-only. Blank inputs omit the field on edit so stored
// authentication headers and proxy credentials survive ordinary saves.
function toAdapterConfiguration(values: ProviderFormValues) {
  const fields: Record<string, string> = {}
  for (const name of [
    'other',
    'settings',
    'setting',
    'header_override',
    'openai_organization',
  ] as const) {
    const value = values[name]?.trim()
    if (value) fields[name] = value
  }
  if (values.type === 41 && fields.other && !fields.other.startsWith('{')) {
    fields.other = JSON.stringify({ default: fields.other })
  }
  return fields
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
    ...toAdapterConfiguration(values),
  }
  if (
    provider.route_limits ||
    Number(values.rpm) ||
    Number(values.tpm) ||
    Number(values.max_concurrency)
  ) {
    payload.route_limits = {
      rpm: Number(values.rpm),
      tpm: Number(values.tpm),
      max_concurrency: Number(values.max_concurrency),
    }
  }
  if (values.key.trim() === '') return payload
  payload.key = values.key
  if (provider.channel_info?.is_multi_key) payload.key_mode = values.key_mode
  return payload
}
