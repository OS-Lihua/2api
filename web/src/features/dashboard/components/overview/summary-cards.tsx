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
import { Link } from '@tanstack/react-router'
import { CreditCard, Wallet } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { FundingStatus } from '@/components/funding-status'
import { LoadingState } from '@/components/loading-state'
import { toIntlLocale } from '@/i18n/languages'
import { formatMoneyAmount } from '@/lib/currency'

import type { useOverviewFinance } from '../../hooks/use-finance-report'
import { FinanceTotals } from '../finance-totals'
import { StatCard } from '../ui/stat-card'

export function SummaryCards({
  finance,
}: {
  finance: ReturnType<typeof useOverviewFinance>
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const { wallet, report } = finance
  return (
    <section className='min-w-0 space-y-3'>
      <div className='flex items-center justify-between gap-2'>
        <h2 className='text-lg font-semibold'>{t('Usage at a glance')}</h2>
        <Link to='/wallet'>{t('Wallet')}</Link>
      </div>
      {!wallet.allowed && <EmptyState title={t('Financial access required')} />}
      {wallet.allowed && wallet.isError && (
        <ErrorState
          description={t('Failed to load wallet balances')}
          onRetry={() => void wallet.refetch()}
        />
      )}
      {wallet.allowed && !wallet.isError && !wallet.data && <LoadingState />}
      {wallet.data && (
        <>
          <div className='grid min-w-0 grid-cols-1 gap-3 sm:grid-cols-2'>
            <StatCard
              title={t('Available Balance')}
              value={formatMoneyAmount(wallet.data.available, locale)}
              description={t('Current wallet position')}
              icon={Wallet}
            />
            <StatCard
              title={t('Outstanding Debt')}
              value={formatMoneyAmount(wallet.data.outstanding, locale)}
              description={t('Current wallet position')}
              icon={CreditCard}
            />
          </div>
          {wallet.data.sale_pending && <p>{t('Sales awaiting accounting')}</p>}
          <FundingStatus pending={wallet.data.funding_pending} />
          {wallet.data.requests_paused && <p>{t('Requests paused')}</p>}
        </>
      )}
      {report.allowed && report.isError && (
        <ErrorState
          description={t('Failed to load')}
          onRetry={() => void report.refetch()}
        />
      )}
      {report.allowed && !report.isError && !report.data && <LoadingState />}
      {report.data && (
        <>
          <p className='text-muted-foreground text-sm'>
            {t(
              'Primary gateway requests in the last seven days. Wallet balances are current, not a reconstructed history.'
            )}
          </p>
          <FinanceTotals totals={report.data.totals} admin={false} />
        </>
      )}
    </section>
  )
}
