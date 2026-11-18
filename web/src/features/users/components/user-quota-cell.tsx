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
import { useTranslation } from 'react-i18next'

import { FundingStatus } from '@/components/funding-status'
import { LoadingState } from '@/components/loading-state'
import { QuotaDetailsPopover } from '@/components/quota-details-popover'
import { toIntlLocale } from '@/i18n/languages'
import { formatMoneyAmount } from '@/lib/currency'
import type { WalletSummary } from '@/lib/wallet-summary'

export function UserWalletCell(props: {
  wallet?: WalletSummary
  loading?: boolean
}) {
  const { t, i18n } = useTranslation()
  if (props.loading) {
    return <LoadingState inline size='sm' message={t('Loading...')} />
  }
  if (!props.wallet) {
    return (
      <span className='text-muted-foreground'>{t('Wallet unavailable')}</span>
    )
  }
  const wallet = props.wallet
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const available = formatMoneyAmount(wallet.available, locale)
  const consumed = formatMoneyAmount(wallet.consumed, locale)
  const details = [
    { label: t('Available Balance'), value: available },
    {
      label: t('Outstanding'),
      value: formatMoneyAmount(wallet.outstanding, locale),
    },
    { label: t('Posted consumption'), value: consumed },
  ]
  if (wallet.sale_pending) {
    details.push({ label: t('Sales awaiting accounting'), value: t('Yes') })
  }
  if (wallet.funding_pending === true) {
    details.push({ label: t('Funding allocation pending'), value: t('Yes') })
  }
  if (wallet.requests_paused) {
    details.push({ label: t('Requests paused'), value: t('Yes') })
  }
  return (
    <QuotaDetailsPopover
      title={t('Wallet')}
      triggerLabel={`${t('Available Balance')} ${available}; ${t('Posted consumption')} ${consumed}`}
      details={details}
      triggerClassName='whitespace-normal'
      description={[
        t(
          'Posted ledger consumption includes signed corrections. It excludes unsettled sales and legacy quota records. Debt repayments are not counted again.'
        ),
        wallet.funding_pending === true
          ? t(
              'Revenue is recorded, but funding allocation is incomplete. Zero outstanding debt does not mean fully paid.'
            )
          : '',
      ]
        .filter(Boolean)
        .join(' ')}
    >
      <span className='grid min-w-0 grid-cols-1 gap-y-1 text-sm tabular-nums'>
        <span className='wrap-anywhere'>{available}</span>
        <span
          data-table-text='secondary'
          className='text-muted-foreground flex min-w-0 flex-col gap-1 text-xs font-normal'
        >
          <span>{t('Posted consumption')}</span>
          <span className='wrap-anywhere'>{consumed}</span>
        </span>
        <FundingStatus pending={wallet.funding_pending} compact />
        {wallet.sale_pending && (
          <span className='text-muted-foreground text-xs'>
            {t('Sales awaiting accounting')}
          </span>
        )}
      </span>
    </QuotaDetailsPopover>
  )
}
