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
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { z } from 'zod'

import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'
import {
  Form,
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { CurrencyInput } from '@/features/pricing-management/components/currency-input'
import { parseDecimalMoneyAmount } from '@/lib/currency'

import {
  checkoutTextByteLength,
  type CheckoutSettingsValues,
} from './checkout-settings-schema'

export type CreemProductData =
  CheckoutSettingsValues['providers'][number]['products'][number]
const productSchema = z
  .object({
    product_id: z
      .string()
      .trim()
      .min(1, 'Enter a unique external product ID of at most 255 UTF-8 bytes.')
      .refine(
        (value) => checkoutTextByteLength(value) <= 255,
        'Use at most 255 UTF-8 bytes.'
      ),
    name: z
      .string()
      .trim()
      .min(1, 'Product name is required.')
      .refine(
        (value) => checkoutTextByteLength(value) <= 255,
        'Use at most 255 UTF-8 bytes.'
      ),
    amount: z.string(),
    currency: z.string().regex(/^[A-Z]{3}$/, 'Invalid currency'),
  })
  .superRefine((value, context) => {
    const money = parseDecimalMoneyAmount(value.amount, value.currency)
    if (!money || BigInt(money.nanos) <= 0n) {
      context.addIssue({
        code: 'custom',
        path: ['amount'],
        message:
          'Enter a positive amount with at most 9 decimal places within the int64 range.',
      })
    }
  })

export function CreemProductDialog(props: {
  provider: 'stripe' | 'creem'
  editData: CreemProductData | null
  onClose: () => void
  onSave: (value: CreemProductData) => void
  productIDs: string[]
}) {
  const { t } = useTranslation()
  const form = useForm<CreemProductData>({
    resolver: zodResolver(productSchema),
    defaultValues: props.editData ?? {
      product_id: '',
      name: '',
      amount: '',
      currency: '',
    },
  })
  return (
    <Dialog
      open
      onOpenChange={(open) => !open && props.onClose()}
      title={
        props.editData ? t('Edit external product') : t('Add external product')
      }
      description={t(
        'The server retrieves the actual payment price from the external product. Configure only the reward per item here.'
      )}
      contentHeight='auto'
      footer={
        <>
          <Button type='button' variant='outline' onClick={props.onClose}>
            {t('Cancel')}
          </Button>
          <Button type='submit' form='checkout-product-reward'>
            {t('Save product reward')}
          </Button>
        </>
      }
    >
      <Form {...form}>
        <form
          id='checkout-product-reward'
          className='space-y-4'
          onSubmit={(event) => {
            event.stopPropagation()
            void form.handleSubmit((values) => {
              if (props.productIDs.includes(values.product_id)) {
                form.setError('product_id', {
                  message:
                    'Enter a unique external product ID of at most 255 UTF-8 bytes.',
                })
                return
              }
              props.onSave(values)
              props.onClose()
            })(event)
          }}
        >
          <FormField
            control={form.control}
            name='product_id'
            render={({ field }) => (
              <FormItem>
                <FormLabel>
                  {props.provider === 'stripe'
                    ? t('Stripe Price ID')
                    : t('Creem Product ID')}
                </FormLabel>
                <FormControl>
                  <Input {...field} />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name='name'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Product name')}</FormLabel>
                <FormControl>
                  <Input {...field} />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name='currency'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Reward currency')}</FormLabel>
                <FormControl>
                  <CurrencyInput
                    value={field.value}
                    onValueChange={field.onChange}
                  />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name='amount'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Reward amount per item')}</FormLabel>
                <FormControl>
                  <Input {...field} type='text' inputMode='decimal' />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
        </form>
      </Form>
    </Dialog>
  )
}
