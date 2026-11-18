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
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useId, useRef, useState } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Button } from '@/components/ui/button'
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
import { Textarea } from '@/components/ui/textarea'
import { toIntlLocale } from '@/i18n/languages'
import { hasPermission } from '@/lib/admin-permissions'
import { formatDecimalMoneyAmount } from '@/lib/currency'
import { ROLE } from '@/lib/roles'
import { getServerErrorMessage } from '@/lib/server-error-message'
import { isWalletScopeCurrent, walletReadScope } from '@/lib/wallet-summary'
import { useAuthStore } from '@/stores/auth-store'

import { adjustUserWallet, getUserWallet } from '../api'
import {
  walletAdjustmentFormSchema,
  type WalletAdjustmentFormValues,
} from '../lib/wallet-adjustment-form'
import type { User } from '../types'

export function UserWalletDialog(props: {
  user: Pick<User, 'id' | 'username' | 'role'>
  onClose: () => void
  onSuccess: () => void
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const currentUser = useAuthStore((state) => state.auth.user)
  const sessionId = useAuthStore((state) => state.auth.session?.sid ?? null)
  const scope = walletReadScope('ledger')
  const canRead = scope.allowed
  const active = useRef(true)
  useEffect(() => {
    active.current = true
    return () => {
      active.current = false
    }
  }, [])
  const canAdjust =
    hasPermission(currentUser, 'ledger', 'write') &&
    (currentUser?.role === ROLE.SUPER_ADMIN ||
      (currentUser?.role ?? 0) > props.user.role)
  const queryKey = [
    'admin-user-wallet',
    currentUser?.id,
    sessionId,
    props.user.id,
    canRead,
  ]
  const query = useQuery({
    queryKey,
    queryFn: async ({ signal }) => {
      if (!isWalletScopeCurrent(scope, 'ledger')) {
        throw new Error('Permission denied')
      }
      const result = await getUserWallet(props.user.id, signal)
      if (signal.aborted || !isWalletScopeCurrent(scope, 'ledger')) {
        throw new Error('Session changed')
      }
      if (result.data?.user_id !== props.user.id) {
        throw new Error('Invalid wallet response')
      }
      return result
    },
    enabled: canRead,
    staleTime: 0,
    refetchOnMount: 'always',
    meta: { errorToast: false },
  })
  const wallet =
    canRead && !query.isError && !query.isFetching
      ? query.data?.data
      : undefined
  const client = useQueryClient()
  const formId = useId()
  const form = useForm<WalletAdjustmentFormValues>({
    resolver: zodResolver(walletAdjustmentFormSchema(wallet?.currency ?? '')),
    defaultValues: { amount: '', remark: '' },
  })
  const [confirmation, setConfirmation] =
    useState<WalletAdjustmentFormValues | null>(null)
  const mutation = useMutation({
    mutationFn: async (values: WalletAdjustmentFormValues) => {
      const actor = useAuthStore.getState().auth.user
      if (
        !active.current ||
        !isWalletScopeCurrent(scope, 'ledger') ||
        !hasPermission(actor, 'ledger', 'write') ||
        !canAdjust
      ) {
        throw new Error('Permission denied')
      }
      return adjustUserWallet(props.user.id, values)
    },
    onSuccess: () => {
      if (!active.current || !isWalletScopeCurrent(scope, 'ledger')) return
      void client.invalidateQueries({ queryKey })
      void client.invalidateQueries({ queryKey: ['admin-user-wallets'] })
      void client.invalidateQueries({
        queryKey: ['self-wallet', props.user.id],
      })
      toast.success(t('Balance updated successfully'))
      props.onSuccess()
      props.onClose()
    },
    onError: (error) => {
      if (!active.current || !isWalletScopeCurrent(scope, 'ledger')) return
      form.setError('root', { message: getServerErrorMessage(error) })
      setConfirmation(null)
    },
  })
  if (!canRead) return null

  return (
    <>
      <Dialog
        open
        onOpenChange={(open) => {
          if (!open && !mutation.isPending) props.onClose()
        }}
        title={t('Wallet')}
        description={t('Wallet for {{username}}', {
          username: props.user.username,
        })}
        bodyClassName='space-y-4'
        showCloseButton={!mutation.isPending}
        footer={
          <>
            <Button
              variant='outline'
              disabled={mutation.isPending}
              onClick={props.onClose}
            >
              {t('Close')}
            </Button>
            {canAdjust && wallet && (
              <Button
                type='submit'
                form={formId}
                disabled={mutation.isPending || query.isFetching}
              >
                {t('Confirm')}
              </Button>
            )}
          </>
        }
      >
        {query.isError && (
          <ErrorState
            description={getServerErrorMessage(query.error)}
            onRetry={() => void query.refetch()}
          />
        )}
        {query.isPending && <LoadingState />}
        {wallet && (
          <>
            <dl className='grid grid-cols-1 gap-2 sm:grid-cols-2'>
              <dt>{t('Available Balance')}</dt>
              <dd className='font-mono wrap-anywhere'>
                {formatDecimalMoneyAmount(
                  { amount: wallet.balance, currency: wallet.currency },
                  locale
                )}
              </dd>
              <dt>{t('Outstanding')}</dt>
              <dd className='font-mono wrap-anywhere'>
                {formatDecimalMoneyAmount(
                  {
                    amount: wallet.outstanding ?? '0',
                    currency: wallet.currency,
                  },
                  locale
                )}
              </dd>
            </dl>
            {canAdjust && (
              <Form {...form}>
                <form
                  id={formId}
                  className='space-y-4'
                  onSubmit={form.handleSubmit((values) => {
                    form.clearErrors('root')
                    setConfirmation(values)
                  })}
                >
                  <h3 className='text-sm font-medium'>{t('Adjust balance')}</h3>
                  <FormField
                    control={form.control}
                    name='amount'
                    render={({ field }) => (
                      <FormItem>
                        <FormLabel>
                          {t('Amount')} ({wallet.currency})
                        </FormLabel>
                        <FormControl>
                          <Input
                            {...field}
                            type='text'
                            inputMode='decimal'
                            disabled={mutation.isPending || query.isFetching}
                          />
                        </FormControl>
                        <FormDescription>
                          {t(
                            'Use a positive amount to add funds or a negative amount to deduct funds. Credits repay outstanding debt first.'
                          )}
                        </FormDescription>
                        <FormMessage />
                      </FormItem>
                    )}
                  />
                  <FormField
                    control={form.control}
                    name='remark'
                    render={({ field }) => (
                      <FormItem>
                        <FormLabel>{t('Remark')}</FormLabel>
                        <FormControl>
                          <Textarea
                            {...field}
                            disabled={mutation.isPending || query.isFetching}
                          />
                        </FormControl>
                        <FormMessage />
                      </FormItem>
                    )}
                  />
                  {form.formState.errors.root && (
                    <p role='alert' className='text-destructive text-sm'>
                      {form.formState.errors.root.message}
                    </p>
                  )}
                </form>
              </Form>
            )}
          </>
        )}
      </Dialog>
      {confirmation && wallet && canAdjust && (
        <ConfirmDialog
          open
          onOpenChange={(open) => {
            if (!open && !mutation.isPending) setConfirmation(null)
          }}
          title={t('Confirm balance adjustment')}
          confirmText={t('Confirm')}
          isLoading={mutation.isPending}
          disabled={query.isFetching}
          desc={
            <dl className='grid grid-cols-1 gap-2 sm:grid-cols-2'>
              <dt>{t('User')}</dt>
              <dd className='wrap-anywhere'>{props.user.username}</dd>
              <dt>{t('Amount')}</dt>
              <dd className='font-mono wrap-anywhere'>
                {formatDecimalMoneyAmount(
                  { amount: confirmation.amount, currency: wallet.currency },
                  locale
                )}
              </dd>
              <dt>{t('Remark')}</dt>
              <dd className='wrap-anywhere whitespace-pre-wrap'>
                {confirmation.remark}
              </dd>
            </dl>
          }
          handleConfirm={() => {
            if (!mutation.isPending && !query.isFetching) {
              mutation.mutate(confirmation)
            }
          }}
        />
      )}
    </>
  )
}
