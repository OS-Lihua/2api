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
import { z } from 'zod'

import {
  MODEL_CAPABILITY_FEATURES,
  MODEL_CAPABILITY_LIMITS,
  MODEL_MODALITIES,
  type Model,
  type ModelCapabilities,
  type ModelCapabilityLimit,
} from '../types'
import { parseModelTags as parseTagsFromUtils } from './model-utils'

// ============================================================================
// Model Form Schema
// ============================================================================

/**
 * Model form validation schema
 */
export const modelFormSchema = z.object({
  id: z.number().optional(),
  model_name: z.string().min(1, 'Model name is required'),
  description: z.string().default(''),
  icon: z.string().default(''),
  tags: z.array(z.string()).default([]),
  vendor_id: z.number(),
  endpoints: z.string().default(''),
  name_rule: z.number().min(0).max(3).default(0),
  status: z.boolean().default(true),
  sync_official: z.boolean().default(true),
  enable_groups: z.array(z.string()).default([]),
  quota_types: z.array(z.number()).default([]),
})

export type ModelFormValues = z.infer<typeof modelFormSchema>

export const MODEL_FORM_DEFAULT_VALUES: ModelFormValues = {
  model_name: '',
  description: '',
  icon: '',
  tags: [],
  vendor_id: 0,
  endpoints: '',
  name_rule: 0,
  status: true,
  sync_official: true,
  enable_groups: [],
  quota_types: [],
}

// ============================================================================
// Capability Form Schema
// ============================================================================

const capabilityLimitSchema = z
  .string()
  .trim()
  .refine((value) => value === '' || /^[1-9]\d*$/.test(value), {
    message: 'Enter a positive integer',
  })
  .refine((value) => value === '' || Number(value) <= Number.MAX_SAFE_INTEGER, {
    message: 'Enter an integer no greater than 9007199254740991',
  })

/**
 * Capability form. Limits are text inputs; an empty limit is not declared.
 */
export const capabilityFormSchema = z.object({
  features: z.array(z.enum(MODEL_CAPABILITY_FEATURES)),
  input_modalities: z.array(z.enum(MODEL_MODALITIES)),
  output_modalities: z.array(z.enum(MODEL_MODALITIES)),
  limits: z.object(
    Object.fromEntries(
      MODEL_CAPABILITY_LIMITS.map((limit) => [limit, capabilityLimitSchema])
    ) as Record<ModelCapabilityLimit, typeof capabilityLimitSchema>
  ),
})

export type CapabilityFormValues = z.infer<typeof capabilityFormSchema>

export function transformCapabilitiesToFormValues(
  capabilities: ModelCapabilities | undefined
): CapabilityFormValues {
  const limits = Object.fromEntries(
    MODEL_CAPABILITY_LIMITS.map((limit) => {
      const value = capabilities?.limits?.[limit]
      return [limit, value === undefined ? '' : String(value)]
    })
  ) as Record<ModelCapabilityLimit, string>
  return {
    features: capabilities?.features ?? [],
    input_modalities: capabilities?.input_modalities ?? [],
    output_modalities: capabilities?.output_modalities ?? [],
    limits,
  }
}

export function transformFormValuesToCapabilities(
  values: CapabilityFormValues
): ModelCapabilities {
  const limits: ModelCapabilities['limits'] = {}
  for (const limit of MODEL_CAPABILITY_LIMITS) {
    const value = values.limits[limit].trim()
    if (value !== '') limits[limit] = Number(value)
  }
  return {
    features: values.features,
    input_modalities: values.input_modalities,
    output_modalities: values.output_modalities,
    limits,
  }
}

// ============================================================================
// Vendor Form Schema
// ============================================================================

/**
 * Vendor form validation schema
 */
export const vendorFormSchema = z.object({
  id: z.number().optional(),
  name: z.string().min(1, 'Vendor name is required'),
  description: z.string().default(''),
  icon: z.string().default(''),
  status: z.number().default(1),
})

export type VendorFormValues = z.infer<typeof vendorFormSchema>

// ============================================================================
// Form Data Transformation
// ============================================================================

/**
 * Transform model to form default values
 */
export function transformModelToFormDefaults(model: Model): ModelFormValues {
  return {
    id: model.id,
    model_name: model.model_name,
    description: model.description || '',
    icon: model.icon || '',
    tags: parseTagsFromUtils(model.tags),
    vendor_id: model.vendor_id ?? 0,
    endpoints: model.endpoints || '',
    name_rule: model.name_rule || 0,
    status: model.status === 1,
    sync_official: model.sync_official === 1,
    enable_groups: model.enable_groups || [],
    quota_types: model.quota_types || [],
  }
}

/**
 * Transform form data to model create/update payload
 */
export function transformFormDataToModelPayload(
  formData: ModelFormValues
): Partial<Model> {
  return {
    id: formData.id,
    model_name: formData.model_name,
    description: formData.description || '',
    icon: formData.icon || '',
    tags: formatTagsArray(formData.tags),
    vendor_id: formData.vendor_id,
    endpoints: formData.endpoints || '',
    name_rule: formData.name_rule,
    status: formData.status ? 1 : 0,
    sync_official: formData.sync_official ? 1 : 0,
    enable_groups: formData.enable_groups,
    quota_types: formData.quota_types,
  }
}

// ============================================================================
// Parsing and Formatting Helpers
// ============================================================================

/**
 * Format tags array to string
 */
export function formatTagsArray(tags: string[]): string {
  return tags.filter(Boolean).join(',')
}

/**
 * Validate JSON string
 */
export function validateJSON(value: string): boolean {
  if (!value || value.trim() === '') return true

  try {
    JSON.parse(value)
    return true
  } catch {
    return false
  }
}

/**
 * Validate endpoints JSON
 */
export function validateEndpoints(endpoints: string): boolean {
  return validateJSON(endpoints)
}
