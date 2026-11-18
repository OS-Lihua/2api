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
import { useMutation } from '@tanstack/react-query'
import { useMemo, useState } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { z } from 'zod'

import { ErrorState } from '@/components/error-state'
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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { toIntlLocale } from '@/i18n/languages'
import {
  formatMoneyAmount,
  moneyAmountToDecimal,
  parseDecimalMoneyAmount,
} from '@/lib/currency'
import { getServerErrorMessage } from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

import { requestCheckoutQuote } from '../api'
import { usePayment } from '../hooks/use-payment'
import type {
  CheckoutProviderInfo,
  CheckoutQuote,
  CheckoutQuoteRequest,
  TopupInfo,
} from '../types'
import { PaymentConfirmDialog } from './dialogs/payment-confirm-dialog'

const providerNames = {
  epay: 'Epay',
  stripe: 'Stripe',
  creem: 'Creem',
  waffo: 'Waffo',
  waffo_pancake: 'Waffo Pancake',
}

export function CheckoutForm(props: { topupInfo: TopupInfo }) {
  const { t } = useTranslation()
  const userId = useAuthStore((state) => state.auth.user?.id)
  const providers = (props.topupInfo.checkout?.providers ?? []).filter(
    (provider) => provider.configured && provider.enabled
  )
  const [selectedProvider, setSelectedProvider] = useState('')
  const provider =
    providers.find((item) => item.provider === selectedProvider) ?? providers[0]
  if (!provider || !userId) {
    return (
      <p className='text-muted-foreground text-sm'>
        {t(
          'Online topup is not enabled. Please use redemption code or contact administrator.'
        )}
      </p>
    )
  }

  return (
    <div className='space-y-4'>
      <div className='flex flex-wrap gap-2' aria-label={t('Payment provider')}>
        {providers.map((item) => (
          <Button
            key={item.provider}
            type='button'
            variant={
              provider.provider === item.provider ? 'default' : 'outline'
            }
            aria-pressed={provider.provider === item.provider}
            onClick={() => setSelectedProvider(item.provider)}
          >
            {providerNames[item.provider]}
          </Button>
        ))}
      </div>
      <ProviderCheckoutForm
        key={`${userId}:${provider.provider}`}
        provider={provider}
        topupInfo={props.topupInfo}
        userId={userId}
      />
    </div>
  )
}

function ProviderCheckoutForm(props: {
  provider: CheckoutProviderInfo
  topupInfo: TopupInfo
  userId: number
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const provider = props.provider
  const [quote, setQuote] = useState<CheckoutQuote | null>(null)
  const [quotePaymentMethod, setQuotePaymentMethod] = useState('')
  const [creationAttempted, setCreationAttempted] = useState(false)
  const methods = useMemo(() => {
    if (provider.provider === 'epay') {
      return props.topupInfo.pay_methods
        .filter(
          (method) =>
            !['stripe', 'creem', 'waffo', 'waffo_pancake'].includes(method.type)
        )
        .map((method) => ({ value: method.type, name: method.name }))
    }
    if (provider.provider === 'waffo') {
      return (props.topupInfo.waffo_pay_methods ?? []).map((method, index) => ({
        value: String(index),
        name: method.name,
      }))
    }
    return []
  }, [
    provider.provider,
    props.topupInfo.pay_methods,
    props.topupInfo.waffo_pay_methods,
  ])
  const schema = useMemo(
    () =>
      z
        .object({
          amount: z.string(),
          productId: z.string(),
          quantity: z.string(),
          method: z.string(),
        })
        .superRefine((values, ctx) => {
          if (provider.selection_kind === 'amount') {
            const minimum = provider.minimum
            const amount =
              minimum &&
              parseDecimalMoneyAmount(values.amount, minimum.currency)
            if (
              !amount ||
              amount.nanos === '0' ||
              BigInt(amount.nanos) < BigInt(minimum.nanos)
            ) {
              ctx.addIssue({
                code: 'custom',
                path: ['amount'],
                message: 'Invalid amount',
              })
            }
          } else {
            if (
              !provider.products.some(
                (product) => product.product_id === values.productId
              )
            ) {
              ctx.addIssue({
                code: 'custom',
                path: ['productId'],
                message: 'Select a product',
              })
            }
            if (
              !/^\d+$/.test(values.quantity) ||
              BigInt(values.quantity) < 1n ||
              BigInt(values.quantity) >
                (provider.provider === 'stripe' ? 10000n : 1n)
            ) {
              ctx.addIssue({
                code: 'custom',
                path: ['quantity'],
                message: 'Invalid quantity',
              })
            }
          }
          if (
            (provider.provider === 'epay' || provider.provider === 'waffo') &&
            !methods.some((method) => method.value === values.method)
          ) {
            ctx.addIssue({
              code: 'custom',
              path: ['method'],
              message: 'Select a payment method',
            })
          }
        }),
    [provider, methods]
  )
  const form = useForm<z.infer<typeof schema>>({
    resolver: zodResolver(schema),
    defaultValues: {
      amount: provider.minimum ? moneyAmountToDecimal(provider.minimum) : '',
      productId: provider.products[0]?.product_id ?? '',
      quantity: '1',
      method: methods[0]?.value ?? '',
    },
  })
  const quoteMutation = useMutation({
    mutationFn: requestCheckoutQuote,
    meta: { errorToast: false },
  })
  const paymentMutation = usePayment(provider.provider, props.userId)
  const pending = quoteMutation.isPending || paymentMutation.isPending

  if (provider.selection_kind === 'amount' && !provider.minimum) {
    return (
      <ErrorState description={t('Payment configuration is unavailable')} />
    )
  }

  if (
    (provider.provider === 'epay' || provider.provider === 'waffo') &&
    methods.length === 0
  ) {
    return <ErrorState description={t('No payment methods available')} />
  }

  const onSubmit = async (values: z.infer<typeof schema>) => {
    form.clearErrors('root')
    const request: CheckoutQuoteRequest = {
      provider: provider.provider,
      selection: {
        kind: 'product',
        product_id: values.productId,
        quantity: values.quantity,
      },
    }
    if (provider.selection_kind === 'amount') {
      const source =
        provider.minimum &&
        parseDecimalMoneyAmount(values.amount, provider.minimum.currency)
      if (!source) return
      request.selection = { kind: 'amount', source_amount: source }
    }
    if (provider.provider === 'epay') request.payment_method = values.method
    if (provider.provider === 'waffo') {
      request.pay_method_index = Number(values.method)
    }
    try {
      const result = await quoteMutation.mutateAsync(request)
      if (useAuthStore.getState().auth.user?.id !== props.userId) return
      setCreationAttempted(false)
      const selectedMethod = methods.find(
        (method) => method.value === values.method
      )
      setQuotePaymentMethod(
        selectedMethod
          ? `${providerNames[provider.provider]} · ${selectedMethod.name}`
          : providerNames[provider.provider]
      )
      setQuote(result)
    } catch (error) {
      if (useAuthStore.getState().auth.user?.id === props.userId) {
        form.setError('root', { message: getServerErrorMessage(error) })
      }
    }
  }

  const confirmPayment = async () => {
    if (
      !quote ||
      paymentMutation.isPending ||
      useAuthStore.getState().auth.user?.id !== props.userId
    ) {
      return
    }
    if (!creationAttempted && quote.expires_at * 1000 <= Date.now()) return
    setCreationAttempted(true)
    try {
      const redirected = await paymentMutation.mutateAsync(quote.quote_id)
      if (
        redirected &&
        useAuthStore.getState().auth.user?.id === props.userId
      ) {
        setQuote(null)
      }
    } catch (error) {
      if (useAuthStore.getState().auth.user?.id === props.userId) {
        form.setError('root', { message: getServerErrorMessage(error) })
      }
    }
  }

  return (
    <>
      <Form {...form}>
        <form onSubmit={form.handleSubmit(onSubmit)} className='space-y-4'>
          {provider.selection_kind === 'amount' ? (
            <>
              {provider.presets.length > 0 && (
                <div className='grid grid-cols-1 gap-2 sm:grid-cols-2'>
                  {provider.presets.map((preset) => (
                    <Button
                      key={`${preset.currency}:${preset.nanos}`}
                      type='button'
                      variant='outline'
                      className='h-auto min-h-10 min-w-0 justify-start wrap-anywhere whitespace-normal'
                      disabled={pending || !!quote}
                      onClick={() =>
                        form.setValue('amount', moneyAmountToDecimal(preset), {
                          shouldDirty: true,
                          shouldValidate: true,
                        })
                      }
                    >
                      {formatMoneyAmount(preset, locale)}
                    </Button>
                  ))}
                </div>
              )}
              <FormField
                control={form.control}
                name='amount'
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>
                      {t('Custom Amount')} ({provider.minimum?.currency})
                    </FormLabel>
                    <FormControl>
                      <Input
                        {...field}
                        inputMode='decimal'
                        disabled={pending || !!quote}
                        placeholder={t('Minimum {{amount}}', {
                          amount: provider.minimum
                            ? formatMoneyAmount(provider.minimum, locale)
                            : '-',
                        })}
                      />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
            </>
          ) : (
            <>
              <FormField
                control={form.control}
                name='productId'
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>{t('Product')}</FormLabel>
                    <Select
                      items={provider.products.map((product) => ({
                        value: product.product_id,
                        label: product.name,
                      }))}
                      value={field.value}
                      onValueChange={field.onChange}
                      disabled={pending || !!quote}
                    >
                      <FormControl>
                        <SelectTrigger>
                          <SelectValue />
                        </SelectTrigger>
                      </FormControl>
                      <SelectContent>
                        {provider.products.map((product) => (
                          <SelectItem
                            key={product.product_id}
                            value={product.product_id}
                          >
                            {product.name} ·{' '}
                            {formatMoneyAmount(product.source_amount, locale)}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                    <FormMessage />
                  </FormItem>
                )}
              />
              {provider.provider === 'stripe' && (
                <FormField
                  control={form.control}
                  name='quantity'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('Quantity')}</FormLabel>
                      <FormControl>
                        <Input
                          {...field}
                          inputMode='numeric'
                          disabled={pending || !!quote}
                        />
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />
              )}
            </>
          )}
          {methods.length > 0 && (
            <FormField
              control={form.control}
              name='method'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Payment Method')}</FormLabel>
                  <Select
                    items={methods.map((method) => ({
                      value: method.value,
                      label: method.name,
                    }))}
                    value={field.value}
                    onValueChange={field.onChange}
                    disabled={pending || !!quote}
                  >
                    <FormControl>
                      <SelectTrigger>
                        <SelectValue />
                      </SelectTrigger>
                    </FormControl>
                    <SelectContent>
                      {methods.map((method) => (
                        <SelectItem key={method.value} value={method.value}>
                          {method.name}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  <FormMessage />
                </FormItem>
              )}
            />
          )}
          {!quote && form.formState.errors.root?.message && (
            <p role='alert' className='text-destructive text-sm'>
              {form.formState.errors.root.message}
            </p>
          )}
          <Button type='submit' disabled={pending || !!quote}>
            {quoteMutation.isPending ? t('Loading...') : t('Review payment')}
          </Button>
        </form>
      </Form>
      {quote && (
        <PaymentConfirmDialog
          quote={quote}
          paymentMethod={quotePaymentMethod}
          processing={paymentMutation.isPending}
          creationAttempted={creationAttempted}
          error={form.formState.errors.root?.message}
          onConfirm={() => void confirmPayment()}
          onOpenChange={(open) => {
            if (!open && !paymentMutation.isPending) {
              setQuote(null)
              form.clearErrors('root')
            }
          }}
        />
      )}
    </>
  )
}
