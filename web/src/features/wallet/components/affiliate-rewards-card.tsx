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
import { Share2 } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { CopyButton } from '@/components/copy-button'
import { ErrorState } from '@/components/error-state'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { IconBadge } from '@/components/ui/icon-badge'
import { Input } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/skeleton'
import { toIntlLocale } from '@/i18n/languages'
import { formatMoneyAmount } from '@/lib/currency'
import { formatNumber } from '@/lib/format'
import { getServerErrorMessage } from '@/lib/server-error-message'

import type { ReferralRewards } from '../types'

interface AffiliateRewardsCardProps {
  rewards: ReferralRewards | null
  affiliateLink: string
  onTransfer: () => void
  complianceConfirmed?: boolean
  loading?: boolean
  hasPendingTransfer?: boolean
  error?: unknown
  onRetry?: () => void
}

export function AffiliateRewardsCard(props: AffiliateRewardsCardProps) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  if (props.error) {
    return (
      <ErrorState
        description={getServerErrorMessage(props.error)}
        onRetry={props.onRetry}
      />
    )
  }
  if (props.loading || !props.rewards) {
    return (
      <Card data-card-hover='false' className='bg-muted/20 py-0'>
        <CardContent className='grid gap-4 p-3 sm:p-4 lg:grid-cols-3'>
          <Skeleton className='h-14 rounded-lg' />
          <Skeleton className='h-14 rounded-lg' />
          <Skeleton className='h-14 rounded-lg' />
        </CardContent>
      </Card>
    )
  }
  const hasRewards =
    props.hasPendingTransfer || BigInt(props.rewards.available.nanos) > 0n
  return (
    <Card data-card-hover='false' className='bg-muted/20 py-0'>
      <CardContent className='grid min-w-0 gap-4 p-3 sm:p-4 xl:grid-cols-[minmax(0,1fr)_minmax(0,1fr)]'>
        <div className='flex min-w-0 items-center gap-2.5'>
          <IconBadge tone='chart-3'>
            <Share2 />
          </IconBadge>
          <div className='min-w-0'>
            <h3 className='text-sm font-semibold'>{t('Referral Program')}</h3>
            <p className='text-muted-foreground text-xs'>
              {t(
                'Earn rewards by inviting users. Transfer available rewards to your wallet.'
              )}
            </p>
          </div>
        </div>
        <dl className='grid min-w-0 grid-cols-2 gap-3 text-center sm:grid-cols-3'>
          {[
            [
              t('Available Rewards'),
              formatMoneyAmount(props.rewards.available, locale),
            ],
            [
              t('Total Earned'),
              formatMoneyAmount(props.rewards.earned, locale),
            ],
            [t('Invites'), formatNumber(props.rewards.invited, locale)],
          ].map(([label, value]) => (
            <div key={label} className='min-w-0'>
              <dt className='text-muted-foreground text-xs'>{label}</dt>
              <dd className='mt-1 text-sm font-semibold wrap-anywhere tabular-nums'>
                {value}
              </dd>
            </div>
          ))}
        </dl>
        <div className='flex min-w-0 flex-wrap items-center gap-2 xl:col-span-2'>
          <Input
            value={props.affiliateLink}
            readOnly
            aria-label={t('Referral link')}
            className='h-9 min-w-0 flex-1 font-mono text-xs'
          />
          <CopyButton
            value={props.affiliateLink}
            variant='outline'
            className='size-9 shrink-0'
            iconClassName='size-4'
            tooltip={t('Copy referral link')}
            aria-label={t('Copy referral link')}
          />
          {hasRewards && (
            <Button
              onClick={props.onTransfer}
              disabled={props.complianceConfirmed === false}
              size='sm'
            >
              {t('Transfer to wallet')}
            </Button>
          )}
        </div>
        {props.complianceConfirmed === false && (
          <p className='text-muted-foreground text-xs xl:col-span-2'>
            {t(
              'Referral reward transfer is disabled until the administrator confirms compliance terms.'
            )}
          </p>
        )}
      </CardContent>
    </Card>
  )
}
