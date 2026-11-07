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
import { zodResolver } from '@hookform/resolvers/zod'
import { useTranslation } from 'react-i18next'
import { z } from 'zod'

import {
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'

import { SettingsForm } from '../components/settings-form-layout'
import { SettingsPageFormActions } from '../components/settings-page-context'
import { SettingsSection } from '../components/settings-section'
import { useSettingsForm } from '../hooks/use-settings-form'
import { useUpdateOption } from '../hooks/use-update-option'
import { safeNumberFieldProps } from '../utils/numeric-field'

const fields = [
  {
    name: 'GatewayMaxAttempts',
    label: 'Maximum attempts',
    min: 1,
    max: 5,
    error: 'Maximum attempts must be an integer between 1 and 5',
  },
  {
    name: 'GatewayTotalFirstByteSeconds',
    label: 'Total first-byte budget (seconds)',
    min: 5,
    max: 600,
    error:
      'Total first-byte budget must be an integer between 5 and 600 seconds',
  },
  {
    name: 'GatewayAttemptFirstByteSeconds',
    label: 'Attempt first-byte budget (seconds)',
    min: 1,
    max: 600,
    error:
      'Attempt first-byte budget must be an integer between 1 and 600 seconds',
  },
] as const

const refreshResetOptions = { keepDirtyValues: true }

type BudgetValues = {
  GatewayMaxAttempts: number
  GatewayTotalFirstByteSeconds: number
  GatewayAttemptFirstByteSeconds: number
}

export function GatewayBudgetSettings(props: { defaultValues: BudgetValues }) {
  const { t } = useTranslation()
  const update = useUpdateOption()
  const schema = z.object({
    GatewayMaxAttempts: z
      .number()
      .int(t(fields[0].error))
      .min(1, t(fields[0].error))
      .max(5, t(fields[0].error)),
    GatewayTotalFirstByteSeconds: z
      .number()
      .int(t(fields[1].error))
      .min(5, t(fields[1].error))
      .max(600, t(fields[1].error)),
    GatewayAttemptFirstByteSeconds: z
      .number()
      .int(t(fields[2].error))
      .min(1, t(fields[2].error))
      .max(600, t(fields[2].error)),
  })
  const { form, handleSubmit, isSubmitting } = useSettingsForm<BudgetValues>({
    resolver: zodResolver(schema),
    defaultValues: props.defaultValues,
    resetOptions: refreshResetOptions,
    onSubmit: async (_values, changed) => {
      for (const [key, value] of Object.entries(changed)) {
        await update.mutateAsync({ key, value: Number(value) })
      }
    },
  })
  // useUpdateOption reports failures; keep dirty inputs available for retry.
  const save = () => handleSubmit().catch(() => {})

  return (
    <SettingsSection title={t('Failover budgets')}>
      <Form {...form}>
        <SettingsForm
          onSubmit={(event) => {
            event.preventDefault()
            void save()
          }}
        >
          <SettingsPageFormActions onSave={save} isSaving={isSubmitting} />
          <FormDescription>
            {t(
              'Defaults: 3 attempts, 90 seconds total, 30 seconds per attempt. Budgets are shared across attempts and member tiers. Each attempt is capped by the remaining total; no new attempt starts with less than 5 seconds remaining. Business output ends the first-byte timer.'
            )}
          </FormDescription>
          {fields.map((item) => (
            <FormField
              key={item.name}
              control={form.control}
              name={item.name}
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t(item.label)}</FormLabel>
                  <FormControl>
                    <Input
                      type='number'
                      min={item.min}
                      max={item.max}
                      step={1}
                      {...safeNumberFieldProps(field)}
                    />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
          ))}
        </SettingsForm>
      </Form>
    </SettingsSection>
  )
}
