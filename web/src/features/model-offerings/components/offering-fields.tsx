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

import { MultiSelect, type Option } from '@/components/multi-select'
import {
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Switch } from '@/components/ui/switch'

import type { OfferingFormValues } from '../lib/offering-form'

export function OfferingIdentityFields(props: {
  groups: Option[]
  disabled: boolean
  isUpdate: boolean
  onAliasDraftChange: (value: string) => void
}) {
  const { t } = useTranslation()
  const form = useFormContext<OfferingFormValues>()
  return (
    <section className='space-y-4'>
      <FormField
        control={form.control}
        name='name'
        render={({ field }) => (
          <FormItem>
            <FormLabel>{t('Primary name')}</FormLabel>
            <FormControl>
              <Input {...field} />
            </FormControl>
            <FormDescription>
              {props.isUpdate
                ? t('Renaming keeps the previous primary name as an alias.')
                : t(
                    'New model offerings are disabled until you enable them. A sale price is required to enable an offering.'
                  )}
            </FormDescription>
            <FormMessage />
          </FormItem>
        )}
      />
      <FormField
        control={form.control}
        name='aliases'
        render={({ field }) => (
          <FormItem>
            <FormLabel>{t('Aliases')}</FormLabel>
            <FormControl>
              <MultiSelect
                options={[]}
                selected={field.value}
                onChange={field.onChange}
                allowCreate
                commitOnBlur
                onDraftChange={props.onAliasDraftChange}
                placeholder={t('Aliases')}
                disabled={props.disabled}
              />
            </FormControl>
            <FormDescription>
              {t(
                'Primary names and aliases must be globally unique. Aliases are callable but hidden from model lists.'
              )}
            </FormDescription>
            <FormMessage />
          </FormItem>
        )}
      />
      <FormField
        control={form.control}
        name='visibility'
        render={({ field }) => (
          <FormItem>
            <FormLabel>{t('Visibility')}</FormLabel>
            <FormControl>
              <NativeSelect {...field} className='w-full'>
                <NativeSelectOption value='all'>
                  {t('All users')}
                </NativeSelectOption>
                <NativeSelectOption value='groups'>
                  {t('Selected user groups')}
                </NativeSelectOption>
              </NativeSelect>
            </FormControl>
            <FormMessage />
          </FormItem>
        )}
      />
      {form.watch('visibility') === 'groups' && (
        <FormField
          control={form.control}
          name='groups'
          render={({ field }) => (
            <FormItem>
              <FormLabel>{t('User groups')}</FormLabel>
              <FormControl>
                <MultiSelect
                  options={props.groups}
                  selected={field.value}
                  onChange={field.onChange}
                  placeholder={t('User groups')}
                  disabled={props.disabled}
                />
              </FormControl>
              <FormMessage />
            </FormItem>
          )}
        />
      )}
    </section>
  )
}

export function OfferingRouteFields(props: {
  disabled: boolean
  onLossDraftChange: (value: string) => void
}) {
  const { t } = useTranslation()
  const form = useFormContext<OfferingFormValues>()
  return (
    <section className='space-y-4' aria-labelledby='offering-route-title'>
      <h3 id='offering-route-title' className='font-semibold'>
        {t('Route settings')}
      </h3>
      <FormField
        control={form.control}
        name='route_method'
        render={({ field }) => (
          <FormItem>
            <FormLabel>{t('Route method')}</FormLabel>
            <FormControl>
              <NativeSelect {...field} className='w-full'>
                <NativeSelectOption value='cost'>
                  {t('Cost')}
                </NativeSelectOption>
                <NativeSelectOption value='weight'>
                  {t('Weight')}
                </NativeSelectOption>
                <NativeSelectOption value='latency'>
                  {t('Latency')}
                </NativeSelectOption>
              </NativeSelect>
            </FormControl>
            <FormMessage />
          </FormItem>
        )}
      />
      <FormField
        control={form.control}
        name='accepted_conversion_losses'
        render={({ field }) => (
          <FormItem>
            <FormLabel>{t('Accepted conversion losses')}</FormLabel>
            <FormControl>
              <MultiSelect
                options={[]}
                selected={field.value}
                onChange={field.onChange}
                allowCreate
                commitOnBlur
                onDraftChange={props.onLossDraftChange}
                placeholder={t('Accepted conversion losses')}
                disabled={props.disabled}
              />
            </FormControl>
            <FormDescription>
              {t(
                'Enter conversion loss identifiers. Leave empty to reject all conversion losses.'
              )}
            </FormDescription>
            <FormMessage />
          </FormItem>
        )}
      />
      <FormField
        control={form.control}
        name='minimum_margin_rate'
        render={({ field }) => (
          <FormItem>
            <FormLabel>{t('Minimum margin rate')}</FormLabel>
            <FormControl>
              <Input {...field} inputMode='decimal' />
            </FormControl>
            <FormDescription>
              {t('Use a decimal from 0 to 1. For example, 0.1 means 10%.')}
            </FormDescription>
            <FormMessage />
          </FormItem>
        )}
      />
      <FormField
        control={form.control}
        name='allow_loss_fallback'
        render={({ field }) => (
          <FormItem className='flex items-center justify-between gap-3 rounded-lg border p-3'>
            <div className='space-y-1'>
              <FormLabel>{t('Allow loss fallback')}</FormLabel>
              <FormDescription>
                {t(
                  'Allow a channel below the minimum margin when no other channel is available.'
                )}
              </FormDescription>
            </div>
            <FormControl>
              <Switch
                checked={field.value}
                onCheckedChange={field.onChange}
                disabled={props.disabled}
              />
            </FormControl>
            <FormMessage />
          </FormItem>
        )}
      />
    </section>
  )
}
