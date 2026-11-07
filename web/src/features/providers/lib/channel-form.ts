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
  capabilityFormSchema,
  transformCapabilitiesToFormValues,
} from '@/features/models/lib/model-form'

const objectJSON = z.string().refine((value) => {
  try {
    const parsed: unknown = JSON.parse(value)
    return (
      typeof parsed === 'object' && parsed !== null && !Array.isArray(parsed)
    )
  } catch {
    return false
  }
}, 'Parameter overrides must be a JSON object')
const nonnegative = z
  .string()
  .regex(/^\d+$/, 'Enter a non-negative integer')
  .refine(
    (value) => Number.isSafeInteger(Number(value)),
    'Enter an integer no greater than 9007199254740991'
  )
export const channelFormSchema = z.object({
  upstream_model_name: z
    .string()
    .trim()
    .min(1, 'Invalid upstream model name')
    .max(256),
  protocol: z.string().min(1, 'Invalid channel protocol'),
  model_id: z.string().regex(/^[1-9]\d*$/, 'Model is required'),
  rpm: nonnegative,
  tpm: nonnegative,
  max_concurrency: nonnegative,
  weight: nonnegative,
  parameter_overrides: objectJSON,
  capabilities: z.string().refine((value) => {
    if (value.trim() === '') return true
    try {
      const parsed = JSON.parse(value)
      return capabilityFormSchema.safeParse(
        transformCapabilitiesToFormValues(parsed)
      ).success
    } catch {
      return false
    }
  }, 'Invalid capability declaration'),
})
export type ChannelFormValues = z.infer<typeof channelFormSchema>
