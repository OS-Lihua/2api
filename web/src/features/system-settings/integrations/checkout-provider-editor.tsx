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
import {
  useFieldArray,
  useFormContext,
  type FieldPathByValue,
} from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import {
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import { CurrencyInput } from '@/features/pricing-management/components/currency-input'

import { SettingsSwitchItem } from '../components/settings-form-layout'
import {
  CHECKOUT_PROVIDER_NAMES,
  isProductCheckout,
  type CheckoutSettingsValues,
} from './checkout-settings-schema'
import { CreemProductsVisualEditor } from './creem-products-visual-editor'

type TextPath = FieldPathByValue<CheckoutSettingsValues, string>
function CheckoutTextField(props: {
  name: TextPath
  label: string
  currency?: boolean
  decimal?: boolean
}) {
  const { t } = useTranslation()
  const form = useFormContext<CheckoutSettingsValues>()
  return (
    <FormField
      control={form.control}
      name={props.name}
      render={({ field }) => (
        <FormItem>
          <FormLabel>{t(props.label)}</FormLabel>
          <FormControl>
            {props.currency ? (
              <CurrencyInput
                value={field.value}
                onValueChange={field.onChange}
                disabled={field.disabled}
              />
            ) : (
              <Input
                {...field}
                type='text'
                inputMode={props.decimal ? 'decimal' : undefined}
              />
            )}
          </FormControl>
          <FormMessage />
        </FormItem>
      )}
    />
  )
}

type RuleKind = 'presets' | 'group_ratios' | 'discounts'
function CheckoutRuleList(props: {
  index: number
  kind: RuleKind
  title: string
  addLabel: string
}) {
  const { t } = useTranslation()
  const form = useFormContext<CheckoutSettingsValues>()
  const name = `providers.${props.index}.${props.kind}` as const
  const rows = useFieldArray<CheckoutSettingsValues, typeof name>({
    control: form.control,
    name,
  })
  const error = form.getFieldState(name, form.formState).error
  return (
    <fieldset className='min-w-0 space-y-3 rounded-lg border p-3'>
      <legend className='px-1 text-sm font-medium'>{t(props.title)}</legend>
      {rows.fields.length === 0 && (
        <p className='text-muted-foreground text-sm'>
          {t('No rules configured')}
        </p>
      )}
      {rows.fields.map((row, index) => (
        <div
          key={row.id}
          className='grid min-w-0 items-end gap-3 sm:grid-cols-2'
        >
          {props.kind === 'group_ratios' ? (
            <>
              <CheckoutTextField
                name={`providers.${props.index}.group_ratios.${index}.group`}
                label='Group name'
              />
              <CheckoutTextField
                name={`providers.${props.index}.group_ratios.${index}.ratio`}
                label='Group ratio'
                decimal
              />
            </>
          ) : (
            <>
              <CheckoutTextField
                name={`providers.${props.index}.${props.kind}.${index}.amount`}
                label='Source amount'
                decimal
              />
              {props.kind === 'discounts' && (
                <CheckoutTextField
                  name={`providers.${props.index}.discounts.${index}.ratio`}
                  label='Discount ratio'
                  decimal
                />
              )}
            </>
          )}
          <Button
            type='button'
            variant='outline'
            onClick={() => rows.remove(index)}
            aria-label={t('Remove rule {{index}}', { index: index + 1 })}
          >
            {t('Remove')}
          </Button>
        </div>
      ))}
      {error?.message && (
        <p role='alert' className='text-destructive text-sm'>
          {t(error.message)}
        </p>
      )}
      <Button
        type='button'
        variant='outline'
        onClick={() => {
          if (props.kind === 'group_ratios') {
            rows.append({ group: '', ratio: '' })
          } else if (props.kind === 'discounts') {
            rows.append({ amount: '', ratio: '' })
          } else {
            rows.append({ amount: '' })
          }
        }}
      >
        {t(props.addLabel)}
      </Button>
    </fieldset>
  )
}

export function CheckoutProviderEditor(props: { index: number }) {
  const { t } = useTranslation()
  const form = useFormContext<CheckoutSettingsValues>()
  const provider = form.watch(`providers.${props.index}`)
  const product = isProductCheckout(provider.key)
  return (
    <fieldset className='min-w-0 space-y-4 rounded-xl border p-4'>
      <legend className='px-1 font-medium'>
        {t('{{provider}} checkout', {
          provider: CHECKOUT_PROVIDER_NAMES[provider.key],
        })}
      </legend>
      <FormField
        control={form.control}
        name={`providers.${props.index}.configured`}
        render={({ field }) => (
          <SettingsSwitchItem>
            <FormLabel>{t('Configure wallet checkout')}</FormLabel>
            <FormControl>
              <Switch
                checked={field.value}
                onCheckedChange={field.onChange}
                disabled={field.disabled}
              />
            </FormControl>
          </SettingsSwitchItem>
        )}
      />
      {provider.configured && (
        <>
          <FormField
            control={form.control}
            name={`providers.${props.index}.enabled`}
            render={({ field }) => (
              <SettingsSwitchItem>
                <FormLabel>{t('Enable new checkout quotes')}</FormLabel>
                <FormControl>
                  <Switch
                    checked={field.value}
                    onCheckedChange={field.onChange}
                    disabled={field.disabled}
                  />
                </FormControl>
              </SettingsSwitchItem>
            )}
          />
          {product ? (
            <>
              <p className='text-muted-foreground text-sm'>
                {t(
                  'The server retrieves the actual payment price from the external product. Configure only the reward per item here.'
                )}
              </p>
              <p className='text-muted-foreground text-sm'>
                {provider.key === 'stripe'
                  ? t(
                      'Stripe uses a real external Price ID. Buyers may select a whole quantity from 1 to 10,000.'
                    )
                  : t(
                      'Creem uses a real external Product ID. Each checkout purchases one item.'
                    )}
              </p>
              <FormField
                control={form.control}
                name={`providers.${props.index}.products`}
                render={({ field }) => (
                  <FormItem className='min-w-0 grid-cols-1'>
                    <FormLabel>{t('External products')}</FormLabel>
                    <FormControl>
                      <CreemProductsVisualEditor
                        provider={
                          provider.key === 'stripe' ? 'stripe' : 'creem'
                        }
                        value={field.value}
                        onChange={field.onChange}
                        disabled={field.disabled}
                      />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
            </>
          ) : (
            <>
              <p className='text-muted-foreground text-sm'>
                {t(
                  'The charge unit is the commercial payment price for one source unit. The two currencies may differ. Accounting FX is configured separately and does not set this price.'
                )}
              </p>
              <div className='grid min-w-0 gap-4 sm:grid-cols-2'>
                <CheckoutTextField
                  name={`providers.${props.index}.source_currency`}
                  label='Source currency'
                  currency
                />
                <CheckoutTextField
                  name={`providers.${props.index}.source_unit`}
                  label='Source unit amount'
                  decimal
                />
                <CheckoutTextField
                  name={`providers.${props.index}.charge_currency`}
                  label='Charge currency'
                  currency
                />
                <CheckoutTextField
                  name={`providers.${props.index}.charge_unit`}
                  label='Charge unit amount'
                  decimal
                />
                <CheckoutTextField
                  name={`providers.${props.index}.minimum`}
                  label='Minimum source amount'
                  decimal
                />
              </div>
              <p className='text-muted-foreground text-sm'>
                {t(
                  'Minimum, presets, and discount amounts use the source currency.'
                )}
              </p>
              <CheckoutRuleList
                index={props.index}
                kind='presets'
                title='Preset source amounts'
                addLabel='Add preset'
              />
              <CheckoutRuleList
                index={props.index}
                kind='discounts'
                title='Payment discounts'
                addLabel='Add payment discount'
              />
            </>
          )}
          {provider.key !== 'creem' && (
            <>
              <p className='text-muted-foreground text-sm'>
                {product
                  ? t(
                      'Group ratios multiply the wallet reward. They do not change the external payment price.'
                    )
                  : t(
                      'Group ratios multiply the payment price. Discount ratios must be greater than 0 and at most 1.'
                    )}
              </p>
              <CheckoutRuleList
                index={props.index}
                kind='group_ratios'
                title={product ? 'Reward group ratios' : 'Payment group ratios'}
                addLabel='Add group ratio'
              />
            </>
          )}
        </>
      )}
    </fieldset>
  )
}
