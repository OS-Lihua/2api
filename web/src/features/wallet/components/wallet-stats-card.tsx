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
  CircleCheck,
  CirclePause,
  ReceiptText,
  WalletCards,
} from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { FundingStatus } from '@/components/funding-status'
import { IconBadge, type IconBadgeTone } from '@/components/ui/icon-badge'
import { Skeleton } from '@/components/ui/skeleton'
import { toIntlLocale } from '@/i18n/languages'
import { formatDecimalMoneyAmount } from '@/lib/currency'

import type { WalletBalance } from '../types'

interface WalletStatsCardProps {
  wallet: WalletBalance | null
  loading?: boolean
}

export function WalletStatsCard(props: WalletStatsCardProps) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  if (props.loading) {
    return (
      <div className='grid grid-cols-1 divide-y rounded-lg border sm:grid-cols-3 sm:divide-x sm:divide-y-0'>
        {['balance', 'outstanding', 'status'].map((key) => (
          <div key={key} className='min-w-0 px-2.5 py-2.5 sm:px-5 sm:py-4'>
            <Skeleton className='h-3.5 w-full' />
            <Skeleton className='mt-2 h-6 w-full sm:h-7' />
          </div>
        ))}
      </div>
    )
  }

  const wallet = props.wallet
  let requestStatus = '-'
  let requestTone: IconBadgeTone = 'neutral'
  if (wallet) {
    requestStatus = wallet.requests_paused ? t('Paused') : t('Allowed')
    requestTone = wallet.requests_paused ? 'warning' : 'success'
  }
  const stats: {
    label: string
    value: string
    icon: typeof WalletCards
    tone: IconBadgeTone
  }[] = [
    {
      label: t('Current Balance'),
      value: formatDecimalMoneyAmount(
        wallet && { amount: wallet.balance, currency: wallet.currency },
        locale
      ),
      icon: WalletCards,
      tone: 'neutral',
    },
    {
      label: t('Unpaid balance'),
      value: formatDecimalMoneyAmount(
        wallet && {
          amount: wallet.outstanding ?? '0',
          currency: wallet.currency,
        },
        locale
      ),
      icon: ReceiptText,
      tone: 'neutral',
    },
    {
      label: t('Request status'),
      value: requestStatus,
      icon: wallet?.requests_paused ? CirclePause : CircleCheck,
      tone: requestTone,
    },
  ]

  return (
    <div className='space-y-2'>
      <dl className='grid grid-cols-1 divide-y rounded-lg border sm:grid-cols-3 sm:divide-x sm:divide-y-0'>
        {stats.map((item) => (
          <div
            key={item.label}
            className='min-w-0 px-2.5 py-2.5 sm:px-5 sm:py-4'
          >
            <dt className='flex items-center gap-1.5 sm:gap-2.5'>
              <IconBadge tone={item.tone} size='stat'>
                <item.icon />
              </IconBadge>
              <div className='text-muted-foreground text-[11px] font-medium tracking-wider break-words uppercase sm:text-xs'>
                {item.label}
              </div>
            </dt>

            <dd className='text-foreground mt-1.5 text-sm font-bold tracking-tight wrap-anywhere sm:mt-2.5 sm:text-2xl'>
              {item.value}
            </dd>
          </div>
        ))}
      </dl>
      {wallet && <FundingStatus pending={wallet.funding_pending} />}
    </div>
  )
}
