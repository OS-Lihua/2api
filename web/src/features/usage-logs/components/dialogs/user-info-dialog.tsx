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
import { useQuery } from '@tanstack/react-query'
import { useTranslation } from 'react-i18next'

import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Label } from '@/components/ui/label'
import { getUserWalletSummaries } from '@/features/users/api'
import { UserWalletCell } from '@/features/users/components/user-quota-cell'
import { toIntlLocale } from '@/i18n/languages'
import { hasPermission } from '@/lib/admin-permissions'
import { formatMoneyAmount } from '@/lib/currency'
import { formatCompactNumber } from '@/lib/format'
import { referralRewardsSchema } from '@/lib/referral-rewards'
import { ROLE } from '@/lib/roles'
import { requireServerSuccess } from '@/lib/server-error-message'
import { isWalletScopeCurrent, walletReadScope } from '@/lib/wallet-summary'
import { useAuthStore } from '@/stores/auth-store'

import { getUserInfo } from '../../api'

interface UserInfoDialogProps {
  userId: number | null
  open: boolean
  onOpenChange: (open: boolean) => void
}

function InfoItem({ label, value }: { label: string; value: string | number }) {
  return (
    <div className='min-w-0 space-y-1.5'>
      <Label className='text-muted-foreground text-xs'>{label}</Label>
      <div className='text-sm font-semibold wrap-anywhere'>{value}</div>
    </div>
  )
}

function UserInfoWallet({ userId }: { userId: number }) {
  const { t } = useTranslation()
  const actorId = useAuthStore((state) => state.auth.user?.id)
  const sessionId = useAuthStore((state) => state.auth.session?.sid ?? null)
  const scope = walletReadScope('ledger')
  const query = useQuery({
    queryKey: [
      'admin-user-wallets',
      actorId,
      sessionId,
      scope.allowed,
      [userId],
      'user-info',
    ],
    queryFn: async ({ signal }) => {
      if (!isWalletScopeCurrent(scope, 'ledger')) {
        throw new Error('Permission denied')
      }
      const result = await getUserWalletSummaries([userId], signal)
      if (signal.aborted || !isWalletScopeCurrent(scope, 'ledger')) {
        throw new Error('Session changed')
      }
      return result
    },
    enabled: scope.allowed,
    staleTime: 0,
    gcTime: 0,
    retry: false,
    meta: { errorToast: false },
  })
  if (!scope.allowed) return null
  if (query.isFetching || query.isPending) {
    return <LoadingState inline size='sm' message={t('Loading...')} />
  }
  if (query.isError) {
    return (
      <ErrorState
        description={t('Wallet unavailable')}
        onRetry={() => void query.refetch()}
        className='min-h-0'
      />
    )
  }
  return (
    <div className='min-w-0 space-y-1.5'>
      <Label className='text-muted-foreground text-xs'>{t('Wallet')}</Label>
      <UserWalletCell wallet={query.data[0]} />
    </div>
  )
}

export function UserInfoDialog({
  userId,
  open,
  onOpenChange,
}: UserInfoDialogProps) {
  const { t, i18n } = useTranslation()
  const actor = useAuthStore((state) => state.auth.user)
  const sessionId = useAuthStore((state) => state.auth.session?.sid ?? null)
  const canReadUser =
    (actor?.role ?? 0) >= ROLE.ADMIN && hasPermission(actor, 'user', 'read')
  function isCurrentScope() {
    const current = useAuthStore.getState().auth
    return (
      current.user?.id === actor?.id &&
      (current.session?.sid ?? null) === sessionId &&
      (current.user?.role ?? 0) >= ROLE.ADMIN &&
      hasPermission(current.user, 'user', 'read')
    )
  }
  const query = useQuery({
    queryKey: [
      'log-user-info',
      actor?.id,
      sessionId,
      canReadUser,
      userId,
      open,
    ],
    queryFn: async ({ signal }) => {
      if (!userId || !isCurrentScope()) {
        throw new Error('Permission denied')
      }
      const response = requireServerSuccess(await getUserInfo(userId, signal))
      if (signal.aborted || !isCurrentScope()) {
        throw new Error('Session changed')
      }
      if (
        !response.data ||
        response.data.id !== userId ||
        (response.data.referral_rewards !== undefined &&
          !referralRewardsSchema.safeParse(response.data.referral_rewards)
            .success)
      ) {
        throw new Error('Invalid user response')
      }
      return response.data
    },
    enabled: open && !!userId && canReadUser,
    staleTime: 0,
    gcTime: 0,
    retry: false,
    meta: { errorToast: false },
  })
  const isLoading = query.isFetching
  const userInfo =
    open && canReadUser && !isLoading && !query.isError ? query.data : undefined

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title={t('User Information')}
      description={t(
        'View detailed information about this user including balance, usage statistics, and invitation details.'
      )}
      contentClassName='sm:max-w-lg'
      contentHeight='auto'
      bodyClassName='space-y-4'
    >
      {!canReadUser && (
        <ErrorState
          description={t('No permission to perform this action')}
          className='min-h-0'
        />
      )}
      {canReadUser && isLoading && <LoadingState message={t('Loading...')} />}
      {canReadUser && query.isError && !isLoading && (
        <ErrorState
          description={t('Failed to fetch user information')}
          onRetry={() => void query.refetch()}
          className='min-h-0'
        />
      )}
      {userInfo && (
        <div className='space-y-4 py-4'>
          {/* Basic Info */}
          <div className='grid grid-cols-2 gap-4'>
            <InfoItem label={t('Username')} value={userInfo.username} />
            {userInfo.display_name && (
              <InfoItem
                label={t('Display Name')}
                value={userInfo.display_name}
              />
            )}
          </div>

          <UserInfoWallet
            key={`${actor?.id}:${sessionId}:${userInfo.id}`}
            userId={userInfo.id}
          />

          {/* Statistics */}
          <div className='grid grid-cols-2 gap-4'>
            <InfoItem
              label={t('Request Count')}
              value={formatCompactNumber(userInfo.request_count)}
            />
            {userInfo.group && (
              <InfoItem label={t('User Group')} value={userInfo.group} />
            )}
          </div>

          {/* Invitation Info */}
          {(userInfo.aff_code ||
            userInfo.aff_count !== undefined ||
            userInfo.referral_rewards) && (
            <>
              <div className='grid grid-cols-2 gap-4'>
                {userInfo.aff_code && (
                  <InfoItem
                    label={t('Invitation Code')}
                    value={userInfo.aff_code}
                  />
                )}
                {userInfo.aff_count !== undefined && (
                  <InfoItem
                    label={t('Invited Users')}
                    value={formatCompactNumber(userInfo.aff_count)}
                  />
                )}
              </div>

              {userInfo.referral_rewards && (
                <InfoItem
                  label={t('Available referral rewards')}
                  value={formatMoneyAmount(
                    userInfo.referral_rewards.available,
                    toIntlLocale(i18n.resolvedLanguage || i18n.language)
                  )}
                />
              )}
            </>
          )}

          {/* Remark */}
          {userInfo.remark && (
            <div className='space-y-1.5'>
              <Label className='text-muted-foreground text-xs'>
                {t('Remark')}
              </Label>
              <div className='text-sm leading-relaxed font-semibold break-words'>
                {userInfo.remark}
              </div>
            </div>
          )}
        </div>
      )}
      {canReadUser && !isLoading && !query.isError && !userInfo && (
        <div className='text-muted-foreground py-8 text-center text-sm'>
          {t('No user information available')}
        </div>
      )}
    </Dialog>
  )
}
