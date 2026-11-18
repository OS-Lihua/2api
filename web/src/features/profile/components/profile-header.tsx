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
import { Activity, BarChart3, WalletCards } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { ErrorState } from '@/components/error-state'
import { FundingStatus } from '@/components/funding-status'
import { LoadingState } from '@/components/loading-state'
import { StatusBadge } from '@/components/status-badge'
import { Avatar, AvatarFallback } from '@/components/ui/avatar'
import { Card, CardContent } from '@/components/ui/card'
import { IconBadge, type IconBadgeTone } from '@/components/ui/icon-badge'
import { Skeleton } from '@/components/ui/skeleton'
import { getSelfWallet } from '@/features/wallet/api'
import { toIntlLocale } from '@/i18n/languages'
import { getUserAvatarFallback, getUserAvatarStyle } from '@/lib/avatar'
import { formatMoneyAmount } from '@/lib/currency'
import { formatCompactNumber } from '@/lib/format'
import { getRoleLabel } from '@/lib/roles'
import {
  isWalletScopeCurrent,
  selfWalletSummarySchema,
  walletReadScope,
} from '@/lib/wallet-summary'
import { useAuthStore } from '@/stores/auth-store'

import { getDisplayName } from '../lib'
import type { UserProfile } from '../types'

// ============================================================================
// Profile Header Component
// ============================================================================

interface ProfileHeaderProps {
  profile: UserProfile | null
  loading: boolean
}

export function ProfileHeader({ profile, loading }: ProfileHeaderProps) {
  const { t, i18n } = useTranslation()
  const actor = useAuthStore((state) => state.auth.user)
  const sessionId = useAuthStore((state) => state.auth.session?.sid ?? null)
  const scope = walletReadScope('wallet')
  const canReadWallet = scope.allowed && profile?.id === actor?.id
  const query = useQuery({
    queryKey: ['self-wallet', actor?.id, sessionId, 'summary', canReadWallet],
    queryFn: async ({ signal }) => {
      if (!isWalletScopeCurrent(scope, 'wallet')) {
        throw new Error('Permission denied')
      }
      const value = await getSelfWallet(signal)
      if (signal.aborted || !isWalletScopeCurrent(scope, 'wallet')) {
        throw new Error('Session changed')
      }
      const summary = selfWalletSummarySchema.parse(value)
      if (summary.user_id !== actor?.id) {
        throw new Error('Invalid wallet response')
      }
      return summary
    },
    enabled: canReadWallet,
    staleTime: 0,
    gcTime: 0,
    retry: false,
    meta: { errorToast: false },
  })
  const wallet =
    canReadWallet && !query.isError && !query.isFetching
      ? query.data
      : undefined
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)

  if (loading) {
    return (
      <Card data-card-hover='false' className='gap-0 overflow-hidden py-0'>
        <CardContent className='p-4 sm:p-5'>
          <div className='flex flex-col items-center gap-4 text-center sm:flex-row sm:text-left'>
            <Skeleton className='h-16 w-16 rounded-2xl' />
            <div className='space-y-3'>
              <div className='flex flex-col items-center gap-2 sm:flex-row sm:justify-start'>
                <Skeleton className='h-8 w-48' />
                <Skeleton className='h-5 w-16' />
              </div>
              <div className='flex flex-col items-center gap-1 sm:flex-row sm:justify-start sm:gap-4'>
                <Skeleton className='h-4 w-24' />
                <Skeleton className='h-4 w-40' />
                <Skeleton className='h-4 w-20' />
              </div>
            </div>
          </div>
        </CardContent>
        <div className='border-t'>
          <div className='divide-border/60 grid grid-cols-1 divide-y sm:grid-cols-3 sm:divide-x sm:divide-y-0'>
            {['balance', 'usage', 'requests'].map((key) => (
              <div key={key} className='px-4 py-3.5 sm:px-5 sm:py-4'>
                <Skeleton className='h-3.5 w-20' />
                <Skeleton className='mt-2 h-7 w-28' />
                <Skeleton className='mt-1.5 h-3.5 w-24' />
              </div>
            ))}
          </div>
        </div>
      </Card>
    )
  }

  if (!profile) return null

  const displayName = getDisplayName(profile)
  const avatarName = profile.username || displayName
  const avatarFallback = getUserAvatarFallback(avatarName)
  const avatarFallbackStyle = getUserAvatarStyle(avatarName)
  const roleLabel = getRoleLabel(profile.role)
  const stats: {
    label: string
    value: string
    description: string
    icon: typeof WalletCards
    tone: IconBadgeTone
  }[] = [
    {
      label: t('API Requests'),
      value: formatCompactNumber(profile.request_count),
      description: t('Total requests made'),
      icon: Activity,
      tone: 'chart-4',
    },
  ]
  if (wallet) {
    stats.unshift(
      {
        label: t('Available Balance'),
        value: formatMoneyAmount(wallet.available, locale),
        description: '',
        icon: WalletCards,
        tone: 'success',
      },
      {
        label: t('Outstanding'),
        value: formatMoneyAmount(wallet.outstanding, locale),
        description: '',
        icon: WalletCards,
        tone: 'warning',
      },
      {
        label: t('Posted consumption'),
        value: formatMoneyAmount(wallet.consumed, locale),
        description: '',
        icon: BarChart3,
        tone: 'info',
      }
    )
  }

  return (
    <Card data-card-hover='false' className='gap-0 overflow-hidden py-0'>
      <CardContent className='p-3 sm:p-5'>
        <div className='flex items-center gap-3 text-left sm:gap-4'>
          <Avatar className='ring-background h-12 w-12 rounded-xl text-sm ring-2 sm:h-16 sm:w-16 sm:rounded-2xl sm:text-lg sm:ring-4'>
            <AvatarFallback
              className='rounded-xl font-semibold text-white sm:rounded-2xl'
              style={avatarFallbackStyle}
            >
              {avatarFallback}
            </AvatarFallback>
          </Avatar>

          <div className='min-w-0 flex-1 space-y-1.5 sm:space-y-3'>
            <div className='flex min-w-0 items-center gap-2'>
              <h1 className='truncate text-xl font-semibold tracking-tight sm:text-2xl'>
                {displayName}
              </h1>
              <StatusBadge
                label={roleLabel}
                variant='neutral'
                copyable={false}
              />
              <StatusBadge
                label={`${t('User ID')} ${profile.id}`}
                variant='info'
                copyText={String(profile.id)}
              />
            </div>

            <div className='text-muted-foreground flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs sm:gap-x-4 sm:text-sm'>
              <span className='truncate'>@{profile.username}</span>
              {profile.email && (
                <>
                  <span>•</span>
                  <span className='truncate'>{profile.email}</span>
                </>
              )}
              {profile.group && (
                <>
                  <span>•</span>
                  <span className='truncate'>{profile.group}</span>
                </>
              )}
            </div>
          </div>
        </div>
      </CardContent>
      <div className='border-t'>
        {canReadWallet && query.isError && (
          <ErrorState
            description={t('Failed to load wallet balances')}
            onRetry={() => void query.refetch()}
            className='min-h-0'
          />
        )}
        {canReadWallet && (query.isPending || query.isFetching) && (
          <LoadingState className='min-h-20' />
        )}
        {wallet && (
          <div className='space-y-1 px-3 py-3 text-sm sm:px-5'>
            <p className='text-muted-foreground'>
              {t(
                'Posted ledger consumption includes signed corrections. It excludes unsettled sales and legacy quota records. Debt repayments are not counted again.'
              )}
            </p>
            {wallet.sale_pending && <p>{t('Sales awaiting accounting')}</p>}
            <FundingStatus pending={wallet.funding_pending} />
            {wallet.requests_paused && <p>{t('Requests paused')}</p>}
          </div>
        )}
        <div className='divide-border/60 grid grid-cols-1 divide-y sm:grid-cols-2 sm:divide-x xl:grid-cols-4'>
          {stats.map((item) => (
            <div key={item.label} className='min-w-0 px-3 py-3 sm:px-5 sm:py-4'>
              <div className='flex items-center gap-2'>
                <IconBadge tone={item.tone} size='stat'>
                  <item.icon />
                </IconBadge>
                <div className='text-muted-foreground truncate text-xs font-medium tracking-wider uppercase'>
                  {item.label}
                </div>
              </div>

              <div className='text-foreground mt-1.5 font-mono text-lg font-bold tracking-tight wrap-anywhere tabular-nums sm:mt-2 sm:text-2xl'>
                {item.value}
              </div>
              <div className='text-muted-foreground/60 mt-1 hidden text-xs md:block'>
                {item.description}
              </div>
            </div>
          ))}
        </div>
      </div>
    </Card>
  )
}
