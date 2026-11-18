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
import { Crown, RefreshCw, Sparkles, Check } from 'lucide-react'
import { useState, useEffect, useMemo, useRef } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ErrorState } from '@/components/error-state'
import {
  StatusBadge,
  dotColorMap,
  textColorMap,
} from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader } from '@/components/ui/card'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Separator } from '@/components/ui/separator'
import { Skeleton } from '@/components/ui/skeleton'
import { TitledCard } from '@/components/ui/titled-card'
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from '@/components/ui/tooltip'
import {
  getPublicPlans,
  getSelfSubscriptionFull,
  updateBillingPreference,
  subscriptionScope,
  isSubscriptionScopeCurrent,
} from '@/features/subscriptions/api'
import { SubscriptionPurchaseDialog } from '@/features/subscriptions/components/dialogs/subscription-purchase-dialog'
import { SubscriptionPeriodDetails } from '@/features/subscriptions/components/subscription-period-details'
import { formatDuration, formatResetPeriod } from '@/features/subscriptions/lib'
import {
  billingPreferenceSchema,
  type BillingPreference,
  type PlanRecord,
  type UserSubscriptionRecord,
} from '@/features/subscriptions/types'
import { toIntlLocale } from '@/i18n/languages'
import { formatMoneyAmount } from '@/lib/currency'
import { handleServerError } from '@/lib/handle-server-error'
import { requireServerSuccess } from '@/lib/server-error-message'
import { cn } from '@/lib/utils'
import { useAuthStore } from '@/stores/auth-store'

import type { PaymentMethod, TopupInfo } from '../types'

interface SubscriptionPlansCardProps {
  topupInfo: TopupInfo | null
  onAvailabilityChange?: (available: boolean) => void
  onPurchaseSuccess?: () => void | Promise<void>
}

function getEpayMethods(payMethods: PaymentMethod[] = []): PaymentMethod[] {
  return payMethods.filter(
    (m) => m?.type && m.type !== 'stripe' && m.type !== 'creem'
  )
}

function getBillingPreferenceLabel(
  preference: string,
  t: (key: string) => string
): string {
  switch (preference) {
    case 'subscription_first':
      return t('Subscription First')
    case 'wallet_first':
      return t('Wallet First')
    case 'subscription_only':
      return t('Subscription Only')
    case 'wallet_only':
      return t('Wallet Only')
    default:
      return preference
  }
}

export function SubscriptionPlansCard(props: SubscriptionPlansCardProps) {
  const actor = useAuthStore((state) => state.auth.user?.id)
  const sid = useAuthStore((state) => state.auth.session?.sid)
  return <SubscriptionPlansContent key={`${actor}:${sid}`} {...props} />
}

function SubscriptionPlansContent({
  topupInfo,
  onAvailabilityChange,
  onPurchaseSuccess,
}: SubscriptionPlansCardProps) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const scope = subscriptionScope()
  const client = useQueryClient()
  const active = useRef(true)
  const preferenceBusy = useRef(false)
  useEffect(() => {
    active.current = true
    return () => {
      active.current = false
    }
  }, [])
  const current = () => active.current && isSubscriptionScopeCurrent(scope)
  const plansQuery = useQuery({
    queryKey: ['subscription-plans', scope.userId, scope.sessionId],
    enabled: !!scope.userId,
    queryFn: ({ signal }) => getPublicPlans(signal),
    staleTime: 0,
    retry: false,
  })
  const selfKey = ['self-subscriptions', scope.userId, scope.sessionId]
  const selfQuery = useQuery({
    queryKey: selfKey,
    enabled: !!scope.userId,
    queryFn: ({ signal }) => getSelfSubscriptionFull(signal),
    staleTime: 0,
    retry: false,
  })
  const plans = useMemo(() => plansQuery.data?.data ?? [], [plansQuery.data])
  const activeSubscriptions = selfQuery.data?.data?.subscriptions ?? []
  const allSubscriptions = useMemo(
    () => selfQuery.data?.data?.all_subscriptions ?? [],
    [selfQuery.data]
  )
  const [draftPreference, setDraftPreference] = useState<BillingPreference>()
  const billingPreference =
    draftPreference ?? selfQuery.data?.data?.billing_preference ?? 'wallet_only'
  const [preferencePending, setPreferencePending] = useState(false)
  const loading = plansQuery.isPending || selfQuery.isPending
  const refreshing = plansQuery.isFetching || selfQuery.isFetching
  const failed = plansQuery.isError || selfQuery.isError

  const [purchaseOpen, setPurchaseOpen] = useState(false)
  const [selectedPlan, setSelectedPlan] = useState<PlanRecord | null>(null)

  const enableStripe = !!topupInfo?.enable_stripe_topup
  const enableCreem = !!topupInfo?.enable_creem_topup
  const enableWaffoPancake = !!topupInfo?.enable_waffo_pancake_topup
  const enableOnlineTopUp = !!topupInfo?.enable_online_topup
  const epayMethods = useMemo(
    () => getEpayMethods(topupInfo?.pay_methods),
    [topupInfo?.pay_methods]
  )

  const fetchSelfSubscription = () => selfQuery.refetch()
  const handleRefresh = () =>
    Promise.all([plansQuery.refetch(), selfQuery.refetch()])
  const handlePreferenceChange = async (value: string) => {
    if (!current() || preferenceBusy.current || !selfQuery.data?.data) return
    const pref = billingPreferenceSchema.parse(value)
    preferenceBusy.current = true
    setPreferencePending(true)
    setDraftPreference(pref)
    try {
      const res = requireServerSuccess(await updateBillingPreference(pref))
      if (!current()) return
      const normalized = billingPreferenceSchema.parse(
        res.data?.billing_preference ?? pref
      )
      client.setQueryData(selfKey, {
        ...selfQuery.data,
        data: { ...selfQuery.data.data, billing_preference: normalized },
      })
      toast.success(t('Updated successfully'))
    } catch (error) {
      if (current()) handleServerError(error, t('Update failed'))
    } finally {
      preferenceBusy.current = false
      if (current()) {
        setDraftPreference(undefined)
        setPreferencePending(false)
      }
    }
  }

  const hasActive = activeSubscriptions.length > 0
  const hasAny = allSubscriptions.length > 0
  const isAvailable = loading || failed || plans.length > 0 || hasAny
  const disablePref = !hasActive
  const isSubPref =
    billingPreference === 'subscription_first' ||
    billingPreference === 'subscription_only'

  const planPurchaseCountMap = useMemo(() => {
    const map = new Map<number, number>()
    for (const sub of allSubscriptions) {
      const planId = sub?.subscription?.plan_id
      if (!planId) continue
      map.set(planId, (map.get(planId) || 0) + 1)
    }
    return map
  }, [allSubscriptions])

  useEffect(() => {
    onAvailabilityChange?.(isAvailable)
  }, [isAvailable, onAvailabilityChange])

  const planTitleMap = useMemo(() => {
    const map = new Map<number, string>()
    for (const p of plans) {
      if (p?.plan?.id) {
        map.set(p.plan.id, p.plan.title || '')
      }
    }
    return map
  }, [plans])

  const getRemainingDays = (sub: UserSubscriptionRecord) => {
    const endTime = sub?.subscription?.end_time || 0
    if (!endTime) return 0
    const now = Date.now() / 1000
    return Math.max(0, Math.ceil((endTime - now) / 86400))
  }

  if (loading) {
    return (
      <Card data-card-hover='false' className='gap-0 overflow-hidden py-0'>
        <CardHeader className='border-b p-3 !pb-3 sm:p-5 sm:!pb-5'>
          <Skeleton className='h-6 w-32' />
        </CardHeader>
        <CardContent className='space-y-4 p-3 sm:p-5'>
          <Skeleton className='h-20 w-full' />
          <div className='grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-3'>
            {['first', 'second', 'third'].map((key) => (
              <Skeleton key={key} className='h-48 w-full' />
            ))}
          </div>
        </CardContent>
      </Card>
    )
  }

  if (failed) {
    return (
      <ErrorState
        description={t('Failed to load subscriptions')}
        onRetry={() => void handleRefresh()}
      />
    )
  }

  if (plans.length === 0 && !hasAny) {
    return null
  }

  return (
    <>
      <TitledCard
        title={t('Subscription Plans')}
        description={t('Subscribe to a plan for model access')}
        icon={<Crown className='h-4 w-4' />}
        iconTone='warning'
        disableHoverEffect
        contentClassName='space-y-4 sm:space-y-5'
      >
        {/* My subscriptions & billing preference */}
        <div className='rounded-xl border p-3 sm:p-4'>
          <div className='flex flex-wrap items-center justify-between gap-2.5 sm:gap-3'>
            <div className='flex min-w-0 flex-wrap items-center gap-2'>
              <span className='text-sm font-medium'>
                {t('My Subscriptions')}
              </span>
              <span className='flex items-center gap-1.5 text-xs font-medium'>
                <span
                  className={cn(
                    'size-1.5 shrink-0 rounded-full',
                    hasActive ? dotColorMap.success : dotColorMap.neutral
                  )}
                  aria-hidden='true'
                />
                {hasActive ? (
                  <span className={cn(textColorMap.success)}>
                    {activeSubscriptions.length} {t('active')}
                  </span>
                ) : (
                  <span className='text-muted-foreground'>
                    {t('No Active')}
                  </span>
                )}
                {allSubscriptions.length > activeSubscriptions.length && (
                  <>
                    <span className='text-muted-foreground/30'>·</span>
                    <span className='text-muted-foreground'>
                      {allSubscriptions.length - activeSubscriptions.length}{' '}
                      {t('expired')}
                    </span>
                  </>
                )}
              </span>
            </div>
            <div className='flex w-full items-center gap-2 sm:w-auto'>
              <Select
                items={[
                  {
                    value: 'subscription_first',
                    label: (
                      <>
                        {getBillingPreferenceLabel('subscription_first', t)}
                        {disablePref ? ` (${t('No Active')})` : ''}
                      </>
                    ),
                  },
                  {
                    value: 'wallet_first',
                    label: getBillingPreferenceLabel('wallet_first', t),
                  },
                  {
                    value: 'subscription_only',
                    label: (
                      <>
                        {getBillingPreferenceLabel('subscription_only', t)}
                        {disablePref ? ` (${t('No Active')})` : ''}
                      </>
                    ),
                  },
                  {
                    value: 'wallet_only',
                    label: getBillingPreferenceLabel('wallet_only', t),
                  },
                ]}
                value={billingPreference}
                disabled={preferencePending}
                onValueChange={(v) => v !== null && handlePreferenceChange(v)}
              >
                <SelectTrigger
                  aria-label={t('Billing preference')}
                  className='h-8 flex-1 text-xs sm:w-[140px] sm:flex-none'
                >
                  <SelectValue>
                    {getBillingPreferenceLabel(billingPreference, t)}
                  </SelectValue>
                </SelectTrigger>
                <SelectContent alignItemWithTrigger={false}>
                  <SelectGroup>
                    <SelectItem
                      value='subscription_first'
                      disabled={disablePref}
                    >
                      {getBillingPreferenceLabel('subscription_first', t)}
                      {disablePref ? ` (${t('No Active')})` : ''}
                    </SelectItem>
                    <SelectItem value='wallet_first'>
                      {getBillingPreferenceLabel('wallet_first', t)}
                    </SelectItem>
                    <SelectItem
                      value='subscription_only'
                      disabled={disablePref}
                    >
                      {getBillingPreferenceLabel('subscription_only', t)}
                      {disablePref ? ` (${t('No Active')})` : ''}
                    </SelectItem>
                    <SelectItem value='wallet_only'>
                      {getBillingPreferenceLabel('wallet_only', t)}
                    </SelectItem>
                  </SelectGroup>
                </SelectContent>
              </Select>
              <Button
                variant='ghost'
                size='icon'
                className='h-8 w-8'
                aria-label={t('Refresh')}
                onClick={() => void handleRefresh()}
                disabled={refreshing}
              >
                <RefreshCw
                  className={`h-3.5 w-3.5 ${refreshing ? 'animate-spin' : ''}`}
                />
              </Button>
            </div>
          </div>

          <p className='text-muted-foreground mt-2 text-xs'>
            {billingPreference === 'wallet_only' &&
              t('Each request uses only the wallet.')}
            {billingPreference === 'subscription_only' &&
              t(
                'Each request uses one subscription period only. It never falls back to the wallet.'
              )}
            {billingPreference === 'wallet_first' &&
              t(
                'Each request tries the wallet first and uses one subscription period only when the wallet balance is insufficient.'
              )}
            {billingPreference === 'subscription_first' &&
              t(
                'Each request tries one subscription period first. Without an active subscription it uses the wallet; with insufficient entitlement it uses the wallet only if every active subscription allows overflow.'
              )}{' '}
            {t(
              'A request never combines subscription periods or switches funding after reservation.'
            )}
          </p>

          {disablePref && isSubPref && (
            <p className='text-muted-foreground mt-2 text-xs'>
              {billingPreference === 'subscription_only'
                ? t(
                    'Preference saved as {{pref}}, but no active subscription. Requests will be rejected.',
                    { pref: t('Subscription Only') }
                  )
                : t(
                    'Preference saved as {{pref}}, but no active subscription. Wallet will be used automatically.',
                    { pref: t('Subscription First') }
                  )}
            </p>
          )}

          {hasAny && (
            <>
              <Separator className='my-3' />
              <div className='max-h-64 space-y-3 overflow-y-auto pr-1'>
                {allSubscriptions.map((sub) => {
                  const subscription = sub.subscription
                  const planTitle =
                    planTitleMap.get(subscription?.plan_id) || ''
                  const remainDays = getRemainingDays(sub)
                  const now = Date.now() / 1000
                  const isExpired = (subscription?.end_time || 0) < now
                  const isCancelled = subscription?.status === 'cancelled'
                  const isActive =
                    subscription?.status === 'active' && !isExpired
                  const nextResetTime = subscription?.next_reset_time ?? 0
                  let statusBadge = (
                    <StatusBadge
                      label={t('Expired')}
                      variant='neutral'
                      copyable={false}
                    />
                  )
                  if (isActive) {
                    statusBadge = (
                      <StatusBadge
                        label={t('Active')}
                        variant='success'
                        copyable={false}
                      />
                    )
                  } else if (isCancelled) {
                    statusBadge = (
                      <StatusBadge
                        label={t('Cancelled')}
                        variant='neutral'
                        copyable={false}
                      />
                    )
                  }

                  let endTimeLabel = t('Expired at')
                  if (isActive) {
                    endTimeLabel = t('Until')
                  } else if (isCancelled) {
                    endTimeLabel = t('Cancelled at')
                  }

                  return (
                    <div
                      key={subscription?.id}
                      className='bg-background rounded-md border p-3 text-xs'
                    >
                      <div className='flex items-center justify-between'>
                        <div className='flex items-center gap-2'>
                          <span className='font-medium'>
                            {planTitle
                              ? `${planTitle} · ${t('Subscription')} #${subscription?.id}`
                              : `${t('Subscription')} #${subscription?.id}`}
                          </span>
                          {statusBadge}
                        </div>
                        {isActive && (
                          <span className='text-muted-foreground'>
                            {t('{{count}} days remaining', {
                              count: remainDays,
                            })}
                          </span>
                        )}
                      </div>
                      <div className='text-muted-foreground mt-1.5'>
                        {endTimeLabel}{' '}
                        {new Date(
                          (subscription?.end_time || 0) * 1000
                        ).toLocaleString(locale)}
                      </div>
                      {isActive && nextResetTime > 0 && (
                        <div className='text-muted-foreground mt-1'>
                          {t('Next reset')}:{' '}
                          {new Date(nextResetTime * 1000).toLocaleString(
                            locale
                          )}
                        </div>
                      )}
                      <div className='mt-2'>
                        <SubscriptionPeriodDetails
                          period={sub.period}
                          legacyReconciliationPending={
                            sub.legacy_reconciliation_pending
                          }
                        />
                      </div>
                    </div>
                  )
                })}
              </div>
            </>
          )}

          {!hasAny && (
            <p className='text-muted-foreground mt-2 text-xs'>
              {t('Subscribe to a plan for model access')}
            </p>
          )}
        </div>

        {/* Available plans grid */}
        {plans.length > 0 ? (
          <div className='grid grid-cols-1 gap-3 2xl:grid-cols-2 2xl:gap-4'>
            {plans.map((p, index) => {
              const plan = p?.plan
              if (!plan) return null
              const price = formatMoneyAmount(plan.price, locale)
              const isPopular = index === 0 && plans.length > 1
              const limit = Number(plan.max_purchase_per_user || 0)
              const count = planPurchaseCountMap.get(plan.id) || 0
              const reached = limit > 0 && count >= limit

              const benefits = [
                `${t('Validity Period')}: ${formatDuration(plan, t)}`,
                formatResetPeriod(plan, t) !== t('No Reset')
                  ? `${t('Entitlement reset')}: ${formatResetPeriod(plan, t)}`
                  : null,
                `${t('Entitlement limit')}: ${plan.unlimited ? t('Unlimited') : formatMoneyAmount(plan.allowance, locale)}`,
                limit > 0 ? `${t('Purchase Limit')}: ${limit}` : null,
                plan.upgrade_group
                  ? `${t('Upgrade Group')}: ${plan.upgrade_group}`
                  : null,
              ].filter(Boolean) as string[]

              return (
                <Card
                  key={plan.id}
                  data-card-hover='false'
                  className={cn(isPopular && 'border-primary/70 shadow-sm')}
                >
                  <CardContent className='flex h-full flex-col p-3.5 sm:p-4'>
                    <div className='mb-2 flex items-start justify-between gap-3'>
                      <div className='min-w-0'>
                        <h4 className='truncate font-semibold'>
                          {plan.title || t('Subscription Plans')}
                        </h4>
                        {plan.subtitle && (
                          <p className='text-muted-foreground truncate text-xs'>
                            {plan.subtitle}
                          </p>
                        )}
                      </div>
                      {isPopular && (
                        <StatusBadge
                          variant='info'
                          copyable={false}
                          className='shrink-0'
                        >
                          <Sparkles className='h-3 w-3' />
                          {t('Recommended')}
                        </StatusBadge>
                      )}
                    </div>

                    <div className='py-2'>
                      <span className='text-primary text-2xl font-bold wrap-anywhere'>
                        {price}
                      </span>
                    </div>

                    <div className='flex-1 space-y-1.5 pb-3'>
                      {benefits.map((label) => (
                        <div
                          key={label}
                          className='text-muted-foreground flex items-center gap-2 text-xs'
                        >
                          <Check className='text-primary h-3 w-3 shrink-0' />
                          <span>{label}</span>
                        </div>
                      ))}
                    </div>

                    <Separator className='mb-3' />

                    {reached ? (
                      <Tooltip>
                        <TooltipTrigger render={<div />}>
                          <Button variant='outline' className='w-full' disabled>
                            {t('Limit Reached')}
                          </Button>
                        </TooltipTrigger>
                        <TooltipContent>
                          {t('Purchase limit reached')} ({count}/{limit})
                        </TooltipContent>
                      </Tooltip>
                    ) : (
                      <Button
                        variant='outline'
                        className='w-full'
                        onClick={() => {
                          setSelectedPlan(p)
                          setPurchaseOpen(true)
                        }}
                      >
                        {t('Subscribe Now')}
                      </Button>
                    )}
                  </CardContent>
                </Card>
              )
            })}
          </div>
        ) : (
          <p className='text-muted-foreground py-4 text-center text-sm'>
            {t('No plans available')}
          </p>
        )}
      </TitledCard>

      <SubscriptionPurchaseDialog
        open={purchaseOpen}
        onOpenChange={(open) => {
          setPurchaseOpen(open)
          if (!open) {
            fetchSelfSubscription()
          }
        }}
        plan={selectedPlan}
        enableStripe={enableStripe}
        enableCreem={enableCreem}
        enableWaffoPancake={enableWaffoPancake}
        enableOnlineTopUp={enableOnlineTopUp}
        epayMethods={epayMethods}
        onPurchaseSuccess={onPurchaseSuccess}
        purchaseLimit={
          selectedPlan?.plan?.max_purchase_per_user
            ? Number(selectedPlan.plan.max_purchase_per_user)
            : undefined
        }
        purchaseCount={
          selectedPlan?.plan?.id
            ? planPurchaseCountMap.get(selectedPlan.plan.id)
            : undefined
        }
      />
    </>
  )
}
