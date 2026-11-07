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
import { useFormContext } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import { MultiSelect } from '@/components/multi-select'
import {
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'

import type { OfferingChoices } from '../api'
import type { OfferingFormValues } from '../lib/offering-form'

export function ChannelScopeEditor(props: {
  choices: OfferingChoices
  disabled: boolean
}) {
  const { t } = useTranslation()
  const form = useFormContext<OfferingFormValues>()
  const fields = [
    {
      name: 'include_provider_ids',
      label: t('Include providers'),
      options: props.choices.providers,
    },
    {
      name: 'exclude_provider_ids',
      label: t('Exclude providers'),
      options: props.choices.providers,
    },
    {
      name: 'include_channel_ids',
      label: t('Include channels'),
      options: props.choices.channels,
    },
    {
      name: 'exclude_channel_ids',
      label: t('Exclude channels'),
      options: props.choices.channels,
    },
  ] as const
  return (
    <section className='space-y-3' aria-labelledby='channel-scope-title'>
      <h3 id='channel-scope-title' className='font-semibold'>
        {t('Channel scope')}
      </h3>
      <p className='text-muted-foreground text-sm'>
        {t(
          'Leave include lists empty to allow all channels. Exclusions always take precedence.'
        )}
      </p>
      {(props.choices.providers === null ||
        props.choices.channels === null) && (
        <p role='status' className='text-muted-foreground text-sm'>
          {t(
            'Some channel scope choices are unavailable. Existing selections are preserved.'
          )}
        </p>
      )}
      <div className='grid gap-4 sm:grid-cols-2'>
        {fields.map((item) => (
          <FormField
            key={item.name}
            control={form.control}
            name={`channel_scope.${item.name}`}
            render={({ field }) => (
              <FormItem>
                <FormLabel>{item.label}</FormLabel>
                <FormControl>
                  <MultiSelect
                    options={item.options ?? []}
                    selected={field.value.map(String)}
                    onChange={(values) => field.onChange(values.map(Number))}
                    placeholder={item.label}
                    disabled={props.disabled || item.options === null}
                  />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
        ))}
      </div>
    </section>
  )
}
