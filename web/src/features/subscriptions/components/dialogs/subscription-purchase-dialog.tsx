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
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Crown } from 'lucide-react'
import { useState, useEffect, useRef } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { GroupBadge } from '@/components/group-badge'
import { LoadingState } from '@/components/loading-state'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Combobox } from '@/components/ui/combobox'
import { Separator } from '@/components/ui/separator'
import { getSelfWallet } from '@/features/wallet/api'
import { submitPaymentForm } from '@/features/wallet/lib/payment'
import { toIntlLocale } from '@/i18n/languages'
import { formatMoneyAmount } from '@/lib/currency'
import { handleServerError } from '@/lib/handle-server-error'
import {
  selfWalletSummarySchema,
  walletReadScope,
  isWalletScopeCurrent,
} from '@/lib/wallet-summary'
import { useAuthStore } from '@/stores/auth-store'

import {
  paySubscriptionStripe,
  paySubscriptionCreem,
  paySubscriptionEpay,
  paySubscriptionWaffoPancake,
  paySubscriptionBalance,
  getPublicPlans,
  SubscriptionPurchaseTermsChanged,
  subscriptionScope,
  isSubscriptionScopeCurrent,
} from '../../api'
import { formatDuration, formatResetPeriod } from '../../lib'
import {
  subscriptionPlanSchema,
  type PlanRecord,
  type BalancePurchaseRequest,
  type SubscriptionPlan,
} from '../../types'

interface PaymentMethod {
  type: string
  name?: string
}

interface Props {
  open: boolean
  onOpenChange: (open: boolean) => void
  plan: PlanRecord | null
  enableStripe?: boolean
  enableCreem?: boolean
  enableWaffoPancake?: boolean
  enableOnlineTopUp?: boolean
  epayMethods?: PaymentMethod[]
  purchaseLimit?: number
  purchaseCount?: number
  onPurchaseSuccess?: () => void | Promise<void>
}

type PendingPurchase = {
  request: BalancePurchaseRequest
  plan: SubscriptionPlan
}

function PurchaseTerms({ plan }: { plan: SubscriptionPlan }) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  return (
    <dl className='bg-muted/50 min-w-0 space-y-3 rounded-lg border p-3 text-sm wrap-anywhere'>
      <div className='flex min-w-0 flex-wrap justify-between gap-2'>
        <dt className='text-muted-foreground'>{t('Plan Name')}</dt>
        <dd>{plan.title}</dd>
      </div>
      <div className='flex min-w-0 flex-wrap justify-between gap-2'>
        <dt className='text-muted-foreground'>{t('Validity Period')}</dt>
        <dd>{formatDuration(plan, t)}</dd>
      </div>
      <div className='flex min-w-0 flex-wrap justify-between gap-2'>
        <dt className='text-muted-foreground'>{t('Entitlement reset')}</dt>
        <dd>{formatResetPeriod(plan, t)}</dd>
      </div>
      <div className='flex min-w-0 flex-wrap justify-between gap-2'>
        <dt className='text-muted-foreground'>{t('Entitlement limit')}</dt>
        <dd>
          {plan.unlimited
            ? t('Unlimited')
            : formatMoneyAmount(plan.allowance, locale)}
        </dd>
      </div>
      {plan.upgrade_group && (
        <div className='flex min-w-0 flex-wrap justify-between gap-2'>
          <dt className='text-muted-foreground'>{t('Upgrade Group')}</dt>
          <dd>
            <GroupBadge group={plan.upgrade_group} />
          </dd>
        </div>
      )}
      <div className='flex min-w-0 flex-wrap justify-between gap-2'>
        <dt className='text-muted-foreground'>{t('Downgrade Group')}</dt>
        <dd>
          {plan.downgrade_group ? (
            <GroupBadge group={plan.downgrade_group} />
          ) : (
            t('Downgrade to pre-purchase group')
          )}
        </dd>
      </div>
      <div className='flex min-w-0 flex-wrap justify-between gap-2'>
        <dt className='text-muted-foreground'>
          {t(
            'Allow whole-request wallet fallback when the entitlement is insufficient'
          )}
        </dt>
        <dd>{plan.allow_wallet_overflow ? t('Yes') : t('No')}</dd>
      </div>
      <Separator />
      <div className='flex min-w-0 flex-wrap justify-between gap-2 font-medium'>
        <dt>{t('Amount Due')}</dt>
        <dd className='text-primary'>
          {formatMoneyAmount(plan.price, locale)}
        </dd>
      </div>
    </dl>
  )
}

export function SubscriptionPurchaseDialog(props: Props) {
  const actor = useAuthStore((state) => state.auth.user?.id)
  const sid = useAuthStore((state) => state.auth.session?.sid)
  const { t } = useTranslation()
  if (!props.plan) return null
  const parsed = subscriptionPlanSchema.safeParse(props.plan.plan)
  if (!parsed.success) {
    return (
      <Dialog
        open={props.open}
        onOpenChange={props.onOpenChange}
        title={t('Purchase Subscription')}
      >
        <ErrorState
          description={t(
            'Invalid subscription money data. Reload the plan before editing.'
          )}
        />
      </Dialog>
    )
  }
  return (
    <SubscriptionPurchaseContent
      key={`${actor}:${sid}:${parsed.data.id}`}
      {...props}
      plan={{ plan: parsed.data }}
    />
  )
}

function SubscriptionPurchaseContent(props: Props & { plan: PlanRecord }) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const user = useAuthStore((state) => state.auth.user)
  const scope = subscriptionScope()
  const walletScope = walletReadScope('wallet')
  const client = useQueryClient()
  const active = useRef(true)
  const busy = useRef(false)
  useEffect(() => {
    active.current = true
    return () => {
      active.current = false
    }
  }, [])
  const isCurrent = () =>
    active.current && isSubscriptionScopeCurrent(scope) && !!user
  const [confirmOpen, setConfirmOpen] = useState(false)
  const [purchaseError, setPurchaseError] = useState(false)
  const [reviewedPlan, setReviewedPlan] = useState<SubscriptionPlan | null>(
    null
  )
  const [termsChanged, setTermsChanged] = useState(false)
  const refreshKey = [
    'subscription-purchase-plan',
    scope.userId,
    scope.sessionId,
    props.plan.plan.id,
  ]
  const planRefresh = useQuery({
    queryKey: refreshKey,
    enabled: props.open && termsChanged,
    queryFn: async ({ signal }) => {
      const result = await getPublicPlans(signal)
      const current = result.data?.find(
        (entry) => entry.plan.id === props.plan.plan.id
      )
      if (!current) throw new Error('Subscription plan not found')
      return current.plan
    },
    staleTime: 0,
    gcTime: 0,
    retry: false,
  })
  const attemptKey = [
    'subscription-purchase-attempt',
    scope.userId,
    scope.sessionId,
    props.plan.plan.id,
  ]
  const attempt = client.getQueryData<PendingPurchase>(attemptKey)
  const walletQuery = useQuery({
    queryKey: [
      'self-wallet',
      scope.userId,
      scope.sessionId,
      'subscription-purchase',
      walletScope.allowed,
    ],
    enabled: props.open && walletScope.allowed,
    queryFn: async ({ signal }) => {
      if (!isWalletScopeCurrent(walletScope, 'wallet')) {
        throw new Error('Session changed')
      }
      const result = selfWalletSummarySchema.parse(await getSelfWallet(signal))
      if (
        signal.aborted ||
        !isWalletScopeCurrent(walletScope, 'wallet') ||
        result.user_id !== scope.userId
      ) {
        throw new Error('Session changed')
      }
      return result
    },
    staleTime: 0,
    gcTime: 0,
    retry: false,
  })
  const [paying, setPaying] = useState(false)
  const [selectedEpayMethod, setSelectedEpayMethod] = useState('')

  useEffect(() => {
    if (props.open && props.epayMethods && props.epayMethods.length > 0) {
      setSelectedEpayMethod(props.epayMethods[0].type)
    } else if (!props.open) {
      setSelectedEpayMethod('')
      setConfirmOpen(false)
      setReviewedPlan(null)
    }
  }, [props.open, props.epayMethods])

  const plan =
    attempt?.plan ??
    reviewedPlan ??
    (termsChanged ? planRefresh.data : undefined) ??
    props.plan.plan
  const hasStripe = props.enableStripe && !!plan.stripe_price_id
  const hasCreem = props.enableCreem && !!plan.creem_product_id
  const hasWaffoPancake =
    props.enableWaffoPancake && !!plan.waffo_pancake_product_id
  const hasEpay =
    props.enableOnlineTopUp && (props.epayMethods || []).length > 0
  const hasAnyPayment = hasStripe || hasCreem || hasWaffoPancake || hasEpay
  const priceAmount = plan.price
  const price = formatMoneyAmount(priceAmount, locale)
  const wallet =
    walletScope.allowed && !walletQuery.isError && !walletQuery.isFetching
      ? walletQuery.data
      : undefined
  const differentCurrency = !!wallet && wallet.currency !== priceAmount.currency
  const insufficientBalance =
    !!wallet &&
    !differentCurrency &&
    BigInt(wallet.available.nanos) < BigInt(priceAmount.nanos)
  const limitReached =
    (props.purchaseLimit || 0) > 0 &&
    (props.purchaseCount || 0) >= (props.purchaseLimit || 0)
  const allowBalancePay = plan.allow_balance_pay !== false
  const refreshingTerms =
    termsChanged &&
    (!planRefresh.data || planRefresh.isFetching || planRefresh.isError)
  const balanceBlocked =
    !walletScope.allowed ||
    refreshingTerms ||
    (!attempt &&
      (!allowBalancePay ||
        !wallet ||
        differentCurrency ||
        insufficientBalance ||
        limitReached))
  const externalBlocked = paying || limitReached || !!attempt || refreshingTerms
  const safeURL = (value: string) => {
    const parsed = new URL(value)
    if (!['http:', 'https:'].includes(parsed.protocol)) {
      throw new Error('Invalid payment URL')
    }
    return parsed.href
  }

  const handlePayStripe = async () => {
    if (!isCurrent() || busy.current) return
    busy.current = true
    setPaying(true)
    try {
      const res = await paySubscriptionStripe({ plan_id: plan.id })
      if (!isCurrent()) return
      if (res.message === 'success' && res.data?.pay_link) {
        window.open(safeURL(res.data.pay_link), '_blank', 'noopener,noreferrer')
        toast.success(t('Payment page opened'))
        props.onOpenChange(false)
      } else {
        handleServerError(res, t('Payment request failed'))
      }
    } catch (error) {
      if (isCurrent()) handleServerError(error, t('Payment request failed'))
    } finally {
      busy.current = false
      if (isCurrent()) setPaying(false)
    }
  }

  const handlePayCreem = async () => {
    if (!isCurrent() || busy.current) return
    busy.current = true
    setPaying(true)
    try {
      const res = await paySubscriptionCreem({ plan_id: plan.id })
      if (!isCurrent()) return
      if (res.message === 'success' && res.data?.checkout_url) {
        window.open(
          safeURL(res.data.checkout_url),
          '_blank',
          'noopener,noreferrer'
        )
        toast.success(t('Payment page opened'))
        props.onOpenChange(false)
      } else {
        handleServerError(res, t('Payment request failed'))
      }
    } catch (error) {
      if (isCurrent()) handleServerError(error, t('Payment request failed'))
    } finally {
      busy.current = false
      if (isCurrent()) setPaying(false)
    }
  }

  // In-tab redirect (not window.open) — user-gesture context is lost
  // across the await, so a popup would be blocked. Same as the wallet hook.
  const handlePayWaffoPancake = async () => {
    if (!isCurrent() || busy.current) return
    busy.current = true
    setPaying(true)
    try {
      const res = await paySubscriptionWaffoPancake({ plan_id: plan.id })
      if (!isCurrent()) return
      if (res.message === 'success' && res.data?.checkout_url) {
        toast.success(t('Redirecting to payment page...'))
        window.location.href = safeURL(res.data.checkout_url)
      } else {
        handleServerError(res, t('Payment request failed'))
      }
    } catch (error) {
      if (isCurrent()) handleServerError(error, t('Payment request failed'))
    } finally {
      busy.current = false
      if (isCurrent()) setPaying(false)
    }
  }

  const handlePayEpay = async () => {
    if (!selectedEpayMethod) {
      toast.error(t('Please select a payment method'))
      return
    }
    if (!isCurrent() || busy.current) return
    busy.current = true
    setPaying(true)
    try {
      const res = await paySubscriptionEpay({
        plan_id: plan.id,
        payment_method: selectedEpayMethod,
      })
      if (!isCurrent()) return
      if (res.message === 'success' && res.url) {
        submitPaymentForm(safeURL(res.url), res.data || {})
        toast.success(t('Payment initiated'))
        props.onOpenChange(false)
      } else {
        handleServerError(res, t('Payment request failed'))
      }
    } catch (error) {
      if (isCurrent()) handleServerError(error, t('Payment request failed'))
    } finally {
      busy.current = false
      if (isCurrent()) setPaying(false)
    }
  }

  const handlePayBalance = async () => {
    if (
      !isCurrent() ||
      !isWalletScopeCurrent(walletScope, 'wallet') ||
      balanceBlocked ||
      busy.current
    ) {
      return
    }
    const pending = client.getQueryData<PendingPurchase>(attemptKey) ?? {
      request: {
        plan_id: plan.id,
        purchase_id: crypto.randomUUID(),
        expected_revision: plan.purchase_revision,
      },
      plan: subscriptionPlanSchema.parse(plan),
    }
    client.setQueryDefaults(['subscription-purchase-attempt'], {
      gcTime: Infinity,
    })
    client.setQueryData(attemptKey, pending)
    busy.current = true
    setPaying(true)
    setPurchaseError(false)
    try {
      await paySubscriptionBalance(pending.request)
      if (!isCurrent()) return
      client.removeQueries({ queryKey: attemptKey, exact: true })
      toast.success(t('Subscription purchased successfully'))
      for (const prefix of [
        'self-wallet',
        'admin-user-wallet',
        'admin-user-wallets',
        'self-subscriptions',
        'admin-user-subscriptions',
        'usage-log-billing-source',
        'dashboard-finance',
      ]) {
        void client.invalidateQueries({ queryKey: [prefix] })
      }
      setConfirmOpen(false)
      props.onOpenChange(false)
      void Promise.resolve(props.onPurchaseSuccess?.()).catch(
        (error: unknown) => {
          if (isCurrent()) handleServerError(error, t('Failed to load'))
        }
      )
    } catch (error) {
      if (isCurrent()) {
        if (error instanceof SubscriptionPurchaseTermsChanged) {
          client.removeQueries({ queryKey: attemptKey, exact: true })
          setReviewedPlan(null)
          setConfirmOpen(false)
          setTermsChanged(true)
          void client.resetQueries({ queryKey: refreshKey, exact: true })
        } else {
          setPurchaseError(true)
          handleServerError(error, t('Payment request failed'))
        }
      }
    } finally {
      busy.current = false
      if (isCurrent()) setPaying(false)
    }
  }

  return (
    <>
      <Dialog
        open={props.open}
        onOpenChange={props.onOpenChange}
        title={
          <>
            <Crown className='h-5 w-5' />
            {t('Purchase Subscription')}
          </>
        }
        contentClassName='max-sm:w-[calc(100vw-1.5rem)] sm:max-w-md'
        titleClassName='flex items-center gap-2'
        contentHeight='auto'
        bodyClassName='space-y-4'
      >
        <div className='space-y-3 sm:space-y-4'>
          <PurchaseTerms plan={plan} />
          {termsChanged && (
            <Alert>
              <AlertDescription>
                {t(
                  'Subscription purchase terms have changed. Review the plan and confirm again.'
                )}
              </AlertDescription>
            </Alert>
          )}
          {termsChanged && planRefresh.isFetching && <LoadingState />}
          {termsChanged && planRefresh.isError && (
            <ErrorState
              description={t('Failed to load')}
              onRetry={() => void planRefresh.refetch()}
            />
          )}

          {limitReached && (
            <Alert variant='destructive'>
              <AlertDescription>
                {t('Purchase limit reached')} ({props.purchaseCount}/
                {props.purchaseLimit})
              </AlertDescription>
            </Alert>
          )}

          <p className='text-muted-foreground text-xs'>
            {t(
              'The price buys the plan. Its separate entitlement is not a wallet balance and cannot be withdrawn.'
            )}
          </p>
          <div className='flex min-w-0 flex-col gap-2 rounded-md border p-3'>
            <div className='flex flex-wrap justify-between gap-2 text-xs'>
              <span className='text-muted-foreground'>
                {t('Wallet payment')}
              </span>
              <span className='wrap-anywhere'>{price}</span>
            </div>
            {wallet && (
              <div className='flex flex-wrap justify-between gap-2 text-xs'>
                <span className='text-muted-foreground'>{t('Available')}</span>
                <span className='wrap-anywhere'>
                  {formatMoneyAmount(wallet.available, locale)}
                </span>
              </div>
            )}
            {walletScope.allowed && walletQuery.isFetching && <LoadingState />}
            {walletScope.allowed && walletQuery.isError && (
              <ErrorState
                description={t('Failed to load wallet balances')}
                onRetry={() => void walletQuery.refetch()}
              />
            )}
            {!walletScope.allowed && <p>{t('Wallet unavailable')}</p>}
            {!allowBalancePay && (
              <p>{t('This plan does not allow wallet purchases.')}</p>
            )}
            {differentCurrency && (
              <p role='status'>
                {t(
                  'Wallet purchases require the plan price and wallet to use the same currency.'
                )}
              </p>
            )}
            {insufficientBalance && !attempt && (
              <p>{t('Insufficient balance')}</p>
            )}
            {attempt && (
              <p>
                {t(
                  'An earlier wallet purchase has not been confirmed. Retry it before starting another payment.'
                )}
              </p>
            )}
            <Button
              variant='outline'
              onClick={() => {
                setReviewedPlan(subscriptionPlanSchema.parse(plan))
                setConfirmOpen(true)
              }}
              disabled={paying || balanceBlocked}
            >
              {t('Pay with wallet')}
            </Button>
          </div>

          {hasAnyPayment && (
            <div className='space-y-3'>
              <p className='text-muted-foreground text-xs'>
                {t('Select payment method')}
              </p>
              {(hasStripe || hasCreem || hasWaffoPancake) && (
                <div className='grid grid-cols-2 gap-2 sm:flex'>
                  {hasStripe && (
                    <Button
                      variant='outline'
                      className='flex-1'
                      onClick={handlePayStripe}
                      disabled={externalBlocked}
                    >
                      Stripe
                    </Button>
                  )}
                  {hasCreem && (
                    <Button
                      variant='outline'
                      className='flex-1'
                      onClick={handlePayCreem}
                      disabled={externalBlocked}
                    >
                      Creem
                    </Button>
                  )}
                  {hasWaffoPancake && (
                    <Button
                      variant='outline'
                      className='flex-1'
                      onClick={handlePayWaffoPancake}
                      disabled={externalBlocked}
                    >
                      Waffo Pancake
                    </Button>
                  )}
                </div>
              )}
              {hasEpay && (
                <div className='grid grid-cols-[minmax(0,1fr)_auto] gap-2'>
                  <Combobox
                    options={(props.epayMethods || []).map((m) => ({
                      value: m.type,
                      label: m.name || m.type,
                    }))}
                    value={selectedEpayMethod}
                    onValueChange={(v) =>
                      v !== null && setSelectedEpayMethod(v)
                    }
                    disabled={externalBlocked}
                    className='flex-1'
                  />
                  <Button
                    onClick={handlePayEpay}
                    disabled={externalBlocked || !selectedEpayMethod}
                  >
                    {t('Pay')}
                  </Button>
                </div>
              )}
            </div>
          )}
        </div>
      </Dialog>
      <ConfirmDialog
        open={props.open && confirmOpen}
        onOpenChange={(open) => {
          if (!paying) {
            setConfirmOpen(open)
            if (!open) setReviewedPlan(null)
          }
        }}
        title={t('Confirm wallet purchase')}
        confirmText={t('Confirm purchase')}
        disabled={balanceBlocked}
        isLoading={paying}
        handleConfirm={() => void handlePayBalance()}
        desc={
          <div className='min-w-0 space-y-2 wrap-anywhere'>
            <PurchaseTerms plan={plan} />
            <p>
              {t(
                'This purchase debits your wallet and grants a separate, non-withdrawable subscription entitlement.'
              )}
            </p>
            {purchaseError && (
              <Alert variant='destructive'>
                <AlertDescription>
                  {t('Payment request failed')}
                </AlertDescription>
              </Alert>
            )}
          </div>
        }
      />
    </>
  )
}
