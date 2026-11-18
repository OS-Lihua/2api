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
import { ExternalLink, Gift, Loader2, Receipt, WalletCards } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { ErrorState } from '@/components/error-state'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { IconBadge } from '@/components/ui/icon-badge'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Skeleton } from '@/components/ui/skeleton'
import { TitledCard } from '@/components/ui/titled-card'
import { getServerErrorMessage } from '@/lib/server-error-message'

import type { TopupInfo } from '../types'
import { CheckoutForm } from './checkout-form'

interface RechargeFormCardProps {
  topupInfo: TopupInfo | null
  redemptionCode: string
  onRedemptionCodeChange: (code: string) => void
  onRedeem: () => void
  redeeming: boolean
  loading?: boolean
  error?: unknown
  onRetry?: () => void
  onOpenBilling?: () => void
}

export function RechargeFormCard(props: RechargeFormCardProps) {
  const { t } = useTranslation()
  return (
    <TitledCard
      title={t('Add Funds')}
      description={t('Choose an amount and payment method')}
      icon={<WalletCards className='h-4 w-4' />}
      iconTone='success'
      disableHoverEffect
      contentClassName='space-y-4 sm:space-y-6'
      action={
        props.onOpenBilling && (
          <Button
            variant='outline'
            size='sm'
            onClick={props.onOpenBilling}
            className='w-full gap-2 sm:w-auto'
          >
            <Receipt className='h-4 w-4' />
            {t('Order History')}
          </Button>
        )
      }
    >
      {props.loading && (
        <div className='space-y-3'>
          <Skeleton className='h-10 w-full' />
          <Skeleton className='h-10 w-full' />
        </div>
      )}
      {!props.loading && (props.error || !props.topupInfo) && (
        <ErrorState
          description={
            props.error
              ? getServerErrorMessage(props.error)
              : t('Payment configuration is unavailable')
          }
          onRetry={props.onRetry}
        />
      )}
      {!props.loading && !props.error && props.topupInfo && (
        <>
          <CheckoutForm topupInfo={props.topupInfo} />
          {props.topupInfo.enable_redemption !== false ? (
            <div className='space-y-2.5 border-t pt-4 sm:space-y-3 sm:pt-6'>
              <div className='flex items-center gap-2'>
                <IconBadge tone='warning' size='xs'>
                  <Gift />
                </IconBadge>
                <Label
                  htmlFor='redemption-code'
                  className='text-muted-foreground text-xs font-medium tracking-wider uppercase'
                >
                  {t('Have a Code?')}
                </Label>
              </div>
              <div className='grid grid-cols-[minmax(0,1fr)_auto] gap-2'>
                <Input
                  id='redemption-code'
                  value={props.redemptionCode}
                  onChange={(event) =>
                    props.onRedemptionCodeChange(event.target.value)
                  }
                  placeholder={t('Enter your redemption code')}
                  className='h-9 min-w-0'
                />
                <Button
                  onClick={props.onRedeem}
                  disabled={props.redeeming}
                  variant='outline'
                  className='h-9 px-4'
                >
                  {props.redeeming && (
                    <Loader2 className='mr-2 h-4 w-4 animate-spin' />
                  )}
                  {t('Redeem code')}
                </Button>
              </div>
              {props.topupInfo.topup_link && (
                <p className='text-muted-foreground text-xs'>
                  {t('Need a redemption code?')}{' '}
                  <a
                    href={props.topupInfo.topup_link}
                    target='_blank'
                    rel='noopener noreferrer'
                    className='inline-flex items-center gap-1 underline-offset-4 hover:underline'
                  >
                    {t('Get one here')}
                    <ExternalLink className='h-3 w-3' />
                  </a>
                </p>
              )}
            </div>
          ) : (
            <Alert className='border-t'>
              <AlertDescription>
                {t(
                  'Redemption codes are disabled until the administrator confirms compliance terms.'
                )}
              </AlertDescription>
            </Alert>
          )}
        </>
      )}
    </TitledCard>
  )
}
