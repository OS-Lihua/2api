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
import { useFieldArray, useFormContext } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import { MultiSelect, type Option } from '@/components/multi-select'
import { Button } from '@/components/ui/button'
import {
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'

import type { OfferingFormValues } from '../lib/offering-form'

export function MemberTierEditor(props: {
  models: Option[]
  disabled: boolean
}) {
  const { t } = useTranslation()
  const form = useFormContext<OfferingFormValues>()
  const tiers = useFieldArray({ control: form.control, name: 'tiers' })
  const values = form.watch('tiers')
  return (
    <section className='space-y-3' aria-labelledby='member-tiers-title'>
      <h3 id='member-tiers-title' className='font-semibold'>
        {t('Member tiers')}
      </h3>
      <p className='text-muted-foreground text-sm'>
        {t(
          'Tier 1 is highest. Models in the same tier are interchangeable; higher tier numbers are fallback targets.'
        )}
      </p>
      {tiers.fields.map((tier, index) => (
        <fieldset
          key={tier.id}
          disabled={props.disabled}
          className='space-y-3 rounded-lg border p-3'
        >
          <legend className='px-1 text-sm font-medium'>
            {t('Member tier {{tier}}', {
              tier: values[index]?.tier || index + 1,
            })}
          </legend>
          <div className='flex items-end justify-between gap-3'>
            <FormField
              control={form.control}
              name={`tiers.${index}.tier`}
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Tier number')}</FormLabel>
                  <FormControl>
                    <Input
                      type='number'
                      min={1}
                      step={1}
                      {...field}
                      value={Number.isNaN(field.value) ? '' : field.value}
                      onChange={(event) =>
                        field.onChange(event.target.valueAsNumber)
                      }
                    />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            <Button
              type='button'
              variant='outline'
              disabled={props.disabled}
              onClick={() => tiers.remove(index)}
              aria-label={t('Remove member tier {{tier}}', {
                tier: values[index]?.tier || index + 1,
              })}
            >
              {t('Remove tier')}
            </Button>
          </div>
          <FormField
            control={form.control}
            name={`tiers.${index}.model_ids`}
            render={({ field }) => (
              <FormItem>
                <FormLabel>
                  {t('Models in tier {{tier}}', {
                    tier: values[index]?.tier || index + 1,
                  })}
                </FormLabel>
                <FormControl>
                  <MultiSelect
                    placeholder={t('Models in tier {{tier}}', {
                      tier: values[index]?.tier || index + 1,
                    })}
                    options={props.models.filter(
                      (model) =>
                        !values.some(
                          (other, otherIndex) =>
                            otherIndex !== index &&
                            other.model_ids.includes(model.value)
                        )
                    )}
                    selected={field.value}
                    onChange={field.onChange}
                    disabled={props.disabled}
                  />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
        </fieldset>
      ))}
      {form.formState.errors.tiers?.root?.message && (
        <p role='alert' className='text-destructive text-sm'>
          {form.formState.errors.tiers.root.message}
        </p>
      )}
      <Button
        type='button'
        variant='outline'
        disabled={props.disabled}
        onClick={() =>
          tiers.append({
            tier:
              Math.max(
                0,
                ...values.map((tier) =>
                  Number.isFinite(tier.tier) ? tier.tier : 0
                )
              ) + 1,
            model_ids: [],
          })
        }
      >
        {t('Add member tier')}
      </Button>
    </section>
  )
}
