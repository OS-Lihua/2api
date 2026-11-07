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
import type { Control, FieldPath, FieldValues } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import {
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Textarea } from '@/components/ui/textarea'

// These are adapter deployment parameters, not a general Provider region.
const deploymentFields: Record<
  number,
  { label: string; placeholder: string; description: string }
> = {
  3: {
    label: 'Default API Version',
    placeholder: 'e.g., 2025-04-01-preview',
    description: 'Default API version for this channel',
  },
  18: {
    label: 'Model Version',
    placeholder: 'e.g., v2.1',
    description: 'Spark model version, e.g., v2.1 (version number in API URL)',
  },
  21: {
    label: 'Knowledge Base ID',
    placeholder: 'e.g., 123456',
    description: 'Enter the knowledge base ID',
  },
  39: {
    label: 'Account ID',
    placeholder: 'e.g., d6b5da8hk1awo8nap34ube6gh',
    description: 'Your Cloudflare Account ID',
  },
  41: {
    label: 'Deployment Region',
    placeholder: 'e.g., us-central1 or JSON format for model-specific regions',
    description: 'Enter deployment region or JSON mapping:',
  },
  49: {
    label: 'Agent ID',
    placeholder: 'e.g., 7342866812345',
    description: 'Enter the Coze agent ID',
  },
}

export function AdapterDeploymentField<T extends FieldValues>(props: {
  control: Control<T>
  name: FieldPath<T>
  type: number
  required?: boolean
}) {
  const { t } = useTranslation()
  const config = deploymentFields[props.type]
  if (!config) return null
  return (
    <FormField
      control={props.control}
      name={props.name}
      render={({ field }) => (
        <FormItem>
          <FormLabel required={props.required ?? true}>
            {t(config.label)}
          </FormLabel>
          <FormControl>
            {props.type === 41 ? (
              <Textarea
                {...field}
                value={String(field.value ?? '')}
                rows={3}
                placeholder={t(config.placeholder)}
              />
            ) : (
              <Input
                {...field}
                value={String(field.value ?? '')}
                placeholder={t(config.placeholder)}
              />
            )}
          </FormControl>
          <FormDescription>
            {t(config.description)}
            {props.type === 41 && (
              <>
                {' '}
                {'{'}
                {t(
                  '"default": "us-central1", "claude-3-5-sonnet-20240620": "europe-west1"'
                )}
                {'}'}
              </>
            )}
          </FormDescription>
          <FormMessage />
        </FormItem>
      )}
    />
  )
}
