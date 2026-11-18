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
import { Ban, Plus, RotateCcw, Trash2 } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import {
  DataTableRowActionMenu,
  StaticDataTable,
} from '@/components/data-table'
import {
  sideDrawerContentClassName,
  sideDrawerFormClassName,
  sideDrawerHeaderClassName,
} from '@/components/drawer-layout'
import { ErrorState } from '@/components/error-state'
import { StatusBadge } from '@/components/status-badge'
import { TableId } from '@/components/table-id'
import { Button } from '@/components/ui/button'
import { Combobox } from '@/components/ui/combobox'
import {
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuShortcut,
} from '@/components/ui/dropdown-menu'
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
  SheetDescription,
} from '@/components/ui/sheet'
import { Switch } from '@/components/ui/switch'
import { toIntlLocale } from '@/i18n/languages'
import { formatMoneyAmount } from '@/lib/currency'
import { handleServerError } from '@/lib/handle-server-error'
import { ROLE } from '@/lib/roles'
import {
  getServerMessage,
  requireServerSuccess,
} from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

import {
  getAdminPlans,
  subscriptionScope,
  isSubscriptionScopeCurrent,
  getUserSubscriptions,
  createUserSubscription,
  invalidateUserSubscription,
  deleteUserSubscription,
  resetUserSubscriptionsByPlan,
} from '../../api'
import { formatTimestamp } from '../../lib'
import type { UserSubscriptionRecord } from '../../types'
import { SubscriptionPeriodDetails } from '../subscription-period-details'

interface Props {
  open: boolean
  onOpenChange: (open: boolean) => void
  user: { id: number; username?: string } | null
  onSuccess?: () => void
}

function SubscriptionStatusBadge(props: {
  sub: UserSubscriptionRecord['subscription']
  t: (key: string) => string
}) {
  // eslint-disable-next-line react-hooks/purity
  const now = Date.now() / 1000
  const isExpired = (props.sub.end_time || 0) > 0 && props.sub.end_time < now
  const isActive = props.sub.status === 'active' && !isExpired
  if (isActive) {
    return (
      <StatusBadge
        label={props.t('Active')}
        variant='success'
        copyable={false}
      />
    )
  }
  if (props.sub.status === 'cancelled') {
    return (
      <StatusBadge
        label={props.t('Invalidated')}
        variant='neutral'
        copyable={false}
      />
    )
  }
  return (
    <StatusBadge
      label={props.t('Expired')}
      variant='neutral'
      copyable={false}
    />
  )
}

export function UserSubscriptionsDialog(props: Props) {
  const actor = useAuthStore((state) => state.auth.user)
  const sid = useAuthStore((state) => state.auth.session?.sid)
  return (
    <UserSubscriptionsContent
      key={`${actor?.id}:${sid}:${props.user?.id}:${props.open}`}
      {...props}
    />
  )
}

function UserSubscriptionsContent(props: Props) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const scope = subscriptionScope()
  const client = useQueryClient()
  const active = useRef(true)
  const busy = useRef(false)
  useEffect(() => {
    active.current = true
    return () => {
      active.current = false
    }
  }, [])
  const current = () =>
    active.current &&
    isSubscriptionScopeCurrent(scope) &&
    (useAuthStore.getState().auth.user?.role ?? 0) >= ROLE.ADMIN
  const dataQuery = useQuery({
    queryKey: [
      'admin-user-subscriptions',
      scope.userId,
      scope.sessionId,
      props.user?.id,
    ],
    enabled: props.open && !!props.user?.id && current(),
    queryFn: async ({ signal }) => {
      if (!props.user?.id || !current()) {
        throw new Error('Subscription identity changed')
      }
      const [plans, subscriptions] = await Promise.all([
        getAdminPlans(signal),
        getUserSubscriptions(props.user.id, signal),
      ])
      return {
        plans: plans.data || [],
        subscriptions: subscriptions.data || [],
      }
    },
    retry: false,
    staleTime: 0,
    gcTime: 0,
  })
  const loading = dataQuery.isPending
  const plans = dataQuery.isError ? [] : dataQuery.data?.plans || []
  const subs = dataQuery.isError ? [] : dataQuery.data?.subscriptions || []
  const [creating, setCreating] = useState(false)
  const [actionPending, setActionPending] = useState(false)
  const [selectedPlanId, setSelectedPlanId] = useState<string>('')
  const [resetting, setResetting] = useState(false)
  const [advanceResetTime, setAdvanceResetTime] = useState(true)
  const [resetAction, setResetAction] = useState<{
    planId: number
    planTitle: string
  } | null>(null)
  const [confirmAction, setConfirmAction] = useState<{
    type: 'invalidate' | 'delete'
    subId: number
  } | null>(null)

  const planTitleMap = new Map<number, string>()
  for (const { plan } of plans) {
    if (plan.id) planTitleMap.set(plan.id, plan.title || `#${plan.id}`)
  }

  const refresh = async () => {
    await Promise.all([
      dataQuery.refetch(),
      client.invalidateQueries({ queryKey: ['self-subscriptions'] }),
      client.invalidateQueries({ queryKey: ['usage-log-billing-source'] }),
    ])
    if (current()) props.onSuccess?.()
  }

  const handleCreate = async () => {
    if (!props.user?.id || !selectedPlanId || busy.current || !current()) return
    busy.current = true
    setCreating(true)
    try {
      const res = requireServerSuccess(
        await createUserSubscription(props.user.id, {
          plan_id: Number(selectedPlanId),
        })
      )
      if (!current()) return
      toast.success(getServerMessage(res.data) || t('Added successfully'))
      setSelectedPlanId('')
      await refresh()
    } catch (error) {
      if (current()) handleServerError(error, t('Request failed'))
    } finally {
      busy.current = false
      if (current()) setCreating(false)
    }
  }

  const handleConfirmAction = async () => {
    if (!confirmAction || busy.current || !current()) return
    busy.current = true
    setActionPending(true)
    try {
      const response =
        confirmAction.type === 'invalidate'
          ? await invalidateUserSubscription(confirmAction.subId)
          : await deleteUserSubscription(confirmAction.subId)
      requireServerSuccess(response)
      if (!current()) return
      toast.success(
        confirmAction.type === 'invalidate'
          ? t('Has been invalidated')
          : t('Deleted')
      )
      setConfirmAction(null)
      await refresh()
    } catch (error) {
      if (current()) handleServerError(error, t('Operation failed'))
    } finally {
      busy.current = false
      if (current()) setActionPending(false)
    }
  }

  const handleResetConfirm = async () => {
    if (!props.user?.id || !resetAction || busy.current || !current()) return
    busy.current = true
    setResetting(true)
    try {
      const res = requireServerSuccess(
        await resetUserSubscriptionsByPlan(props.user.id, {
          plan_id: resetAction.planId,
          advance_reset_time: advanceResetTime,
        })
      )
      if (!current()) return
      toast.success(
        t('Reset {{count}} active subscriptions', {
          count: res.data?.reset_count || 0,
        })
      )
      setResetAction(null)
      await refresh()
    } catch (error) {
      if (current()) handleServerError(error, t('Operation failed'))
    } finally {
      busy.current = false
      if (current()) setResetting(false)
    }
  }

  return (
    <>
      <Sheet open={props.open} onOpenChange={props.onOpenChange}>
        <SheetContent className={sideDrawerContentClassName('sm:max-w-2xl')}>
          <SheetHeader className={sideDrawerHeaderClassName()}>
            <SheetTitle>{t('User Subscription Management')}</SheetTitle>
            <SheetDescription>
              {props.user?.username || '-'} (ID: {props.user?.id || '-'})
            </SheetDescription>
          </SheetHeader>

          <div className={sideDrawerFormClassName()}>
            <div className='flex min-w-0 flex-wrap gap-2'>
              <Combobox
                options={plans.map((p) => ({
                  value: String(p.plan.id),
                  label: `${p.plan.title} (${formatMoneyAmount(p.plan.price, locale)})`,
                }))}
                value={selectedPlanId}
                onValueChange={(v) => v !== null && setSelectedPlanId(v)}
                className='min-w-0 flex-1'
                placeholder={t('Select subscription plan')}
              />
              <Button
                onClick={handleCreate}
                disabled={
                  creating ||
                  actionPending ||
                  resetting ||
                  !selectedPlanId ||
                  dataQuery.isError ||
                  loading
                }
              >
                <Plus className='mr-1 h-4 w-4' />
                {t('Add subscription')}
              </Button>
            </div>

            {dataQuery.isError ? (
              <ErrorState
                description={t('Failed to load subscriptions')}
                onRetry={() => void dataQuery.refetch()}
              />
            ) : (
              <StaticDataTable
                data={loading ? [] : subs}
                getRowKey={(record) => record.subscription.id}
                emptyClassName={loading ? 'py-8' : 'text-muted-foreground py-8'}
                emptyContent={
                  loading ? t('Loading...') : t('No subscription records')
                }
                columns={[
                  {
                    id: 'id',
                    header: t('ID'),
                    cell: (record) => (
                      <TableId value={record.subscription.id} />
                    ),
                  },
                  {
                    id: 'plan',
                    header: t('Plan'),
                    cell: (record) => {
                      const sub = record.subscription

                      return (
                        <div>
                          <div className='font-medium'>
                            {planTitleMap.get(sub.plan_id) || `#${sub.plan_id}`}
                          </div>
                          <div className='text-muted-foreground text-sm'>
                            {t('Source')}: {sub.source || '-'}
                          </div>
                        </div>
                      )
                    },
                  },
                  {
                    id: 'status',
                    header: t('Status'),
                    cell: (record) => (
                      <SubscriptionStatusBadge
                        sub={record.subscription}
                        t={t}
                      />
                    ),
                  },
                  {
                    id: 'validity',
                    header: t('Validity'),
                    cell: (record) => {
                      const sub = record.subscription

                      return (
                        <div className='text-sm'>
                          <div>
                            {t('Start')}: {formatTimestamp(sub.start_time)}
                          </div>
                          <div>
                            {t('End')}: {formatTimestamp(sub.end_time)}
                          </div>
                        </div>
                      )
                    },
                  },
                  {
                    id: 'entitlement',
                    header: t('Subscription entitlement'),
                    cell: (record) => (
                      <SubscriptionPeriodDetails
                        period={record.period}
                        legacyReconciliationPending={
                          record.legacy_reconciliation_pending
                        }
                      />
                    ),
                  },
                  {
                    id: 'actions',
                    header: t('Actions'),
                    className: 'text-right',
                    cellClassName: 'text-right',
                    cell: (record) => {
                      const sub = record.subscription
                      const now = Date.now() / 1000
                      const isExpired =
                        (sub.end_time || 0) > 0 && sub.end_time < now
                      const isActive = sub.status === 'active' && !isExpired

                      return (
                        <DataTableRowActionMenu ariaLabel={t('Actions')}>
                          <DropdownMenuItem
                            disabled={!isActive}
                            onClick={() => {
                              setAdvanceResetTime(true)
                              setResetAction({
                                planId: sub.plan_id,
                                planTitle:
                                  planTitleMap.get(sub.plan_id) ||
                                  `#${sub.plan_id}`,
                              })
                            }}
                          >
                            {t('Reset entitlement period')}
                            <DropdownMenuShortcut>
                              <RotateCcw size={16} />
                            </DropdownMenuShortcut>
                          </DropdownMenuItem>
                          <DropdownMenuItem
                            disabled={!isActive}
                            onClick={() =>
                              setConfirmAction({
                                type: 'invalidate',
                                subId: sub.id,
                              })
                            }
                          >
                            {t('Invalidate')}
                            <DropdownMenuShortcut>
                              <Ban size={16} />
                            </DropdownMenuShortcut>
                          </DropdownMenuItem>
                          <DropdownMenuSeparator />
                          <DropdownMenuItem
                            variant='destructive'
                            onClick={() =>
                              setConfirmAction({
                                type: 'delete',
                                subId: sub.id,
                              })
                            }
                          >
                            {t('Delete')}
                            <DropdownMenuShortcut>
                              <Trash2 size={16} />
                            </DropdownMenuShortcut>
                          </DropdownMenuItem>
                        </DataTableRowActionMenu>
                      )
                    },
                  },
                ]}
              />
            )}
          </div>
        </SheetContent>
      </Sheet>

      {confirmAction && (
        <ConfirmDialog
          open
          onOpenChange={(v) => !v && !actionPending && setConfirmAction(null)}
          title={
            confirmAction.type === 'invalidate'
              ? t('Confirm invalidate')
              : t('Confirm delete')
          }
          desc={
            confirmAction.type === 'invalidate'
              ? t(
                  'After invalidating, this subscription will be immediately deactivated. Historical records are not affected. Continue?'
                )
              : t(
                  'Deleting will permanently remove this subscription record (including benefit details). Continue?'
                )
          }
          handleConfirm={handleConfirmAction}
          isLoading={actionPending}
          destructive={confirmAction.type === 'delete'}
        />
      )}

      {resetAction && (
        <ConfirmDialog
          open
          onOpenChange={(v) => !v && !resetting && setResetAction(null)}
          title={t('Reset entitlement period')}
          desc={t('Reset active {{plan}} subscriptions for this user?', {
            plan: resetAction.planTitle,
          })}
          confirmText={t('Reset entitlement period')}
          handleConfirm={handleResetConfirm}
          isLoading={resetting}
        >
          <label className='flex items-center justify-between gap-3 rounded-md border px-3 py-2 text-sm'>
            <span>{t('Advance next reset time')}</span>
            <Switch
              checked={advanceResetTime}
              onCheckedChange={(checked) => setAdvanceResetTime(!!checked)}
              aria-label={t('Advance next reset time')}
            />
          </label>
        </ConfirmDialog>
      )}
    </>
  )
}
