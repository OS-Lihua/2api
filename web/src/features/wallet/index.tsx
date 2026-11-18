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
import { useState, useEffect, useCallback } from 'react'
import { useTranslation } from 'react-i18next'

import { SectionPageLayout } from '@/components/layout'
import type { MoneyAmount } from '@/lib/currency'
import { useAuthStore } from '@/stores/auth-store'

import { getSelfWallet } from './api'
import { AffiliateRewardsCard } from './components/affiliate-rewards-card'
import { BillingHistoryDialog } from './components/dialogs/billing-history-dialog'
import { TransferDialog } from './components/dialogs/transfer-dialog'
import { RechargeFormCard } from './components/recharge-form-card'
import { SubscriptionPlansCard } from './components/subscription-plans-card'
import { WalletStatsCard } from './components/wallet-stats-card'
import { useTopupInfo, useAffiliate, useRedemption } from './hooks'

interface WalletProps {
  initialShowHistory?: boolean
}

export function Wallet(props: WalletProps) {
  const userId = useAuthStore((state) => state.auth.user?.id)
  if (!userId) return null
  return <WalletContent key={userId} {...props} userId={userId} />
}

function WalletContent(props: WalletProps & { userId: number }) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const walletQuery = useQuery({
    queryKey: ['self-wallet', props.userId],
    queryFn: ({ signal }) => getSelfWallet(signal),
  })
  const [transferDialogOpen, setTransferDialogOpen] = useState(false)
  const [billingDialogOpen, setBillingDialogOpen] = useState(false)
  const [redemptionCode, setRedemptionCode] = useState('')
  const [showSubscriptionPanel, setShowSubscriptionPanel] = useState(true)
  const {
    topupInfo,
    loading: topupLoading,
    error: topupError,
    refetch: refetchTopup,
  } = useTopupInfo()
  const {
    affiliateLink,
    loading: affiliateLoading,
    transferRewards,
    transferring,
    rewards,
    transferAttempt,
    error: affiliateError,
    refetch: refetchAffiliate,
  } = useAffiliate(props.userId)
  const { redeeming, redeemCode } = useRedemption()

  const refreshWallet = useCallback(async () => {
    await queryClient.invalidateQueries({
      queryKey: ['self-wallet', props.userId],
    })
  }, [queryClient, props.userId])

  useEffect(() => {
    if (props.initialShowHistory) {
      setBillingDialogOpen(true)
      window.history.replaceState({}, '', window.location.pathname)
    }
  }, [props.initialShowHistory])

  const handleRedeem = async () => {
    if (!redemptionCode) return
    if (await redeemCode(redemptionCode)) {
      setRedemptionCode('')
      await refreshWallet()
    }
  }
  const handleTransfer = async (amount: MoneyAmount) => {
    const success = await transferRewards(amount)
    if (success) await refreshWallet()
    return success
  }
  const handleSubscriptionAvailabilityChange = useCallback(
    (available: boolean) => setShowSubscriptionPanel(available),
    []
  )

  return (
    <>
      <SectionPageLayout>
        <SectionPageLayout.Title>{t('Wallet')}</SectionPageLayout.Title>
        <SectionPageLayout.Content>
          <div className='mx-auto flex w-full max-w-7xl flex-col gap-4 sm:gap-5'>
            <WalletStatsCard
              wallet={walletQuery.isError ? null : (walletQuery.data ?? null)}
              loading={walletQuery.isPending}
            />
            <div
              className={
                showSubscriptionPanel
                  ? 'grid gap-4 xl:grid-cols-[minmax(0,1.05fr)_minmax(360px,0.95fr)] xl:items-start'
                  : 'grid gap-4'
              }
            >
              <div id='wallet-add-funds' className='scroll-mt-4'>
                <RechargeFormCard
                  topupInfo={topupInfo}
                  redemptionCode={redemptionCode}
                  onRedemptionCodeChange={setRedemptionCode}
                  onRedeem={() => void handleRedeem()}
                  redeeming={redeeming}
                  loading={topupLoading}
                  error={topupError}
                  onRetry={() => void refetchTopup()}
                  onOpenBilling={() => setBillingDialogOpen(true)}
                />
              </div>
              <SubscriptionPlansCard
                topupInfo={topupInfo}
                onAvailabilityChange={handleSubscriptionAvailabilityChange}
                onPurchaseSuccess={refreshWallet}
              />
            </div>
            <AffiliateRewardsCard
              rewards={rewards}
              error={affiliateError}
              onRetry={() => void refetchAffiliate()}
              hasPendingTransfer={transferAttempt !== null}
              affiliateLink={affiliateLink}
              onTransfer={() => setTransferDialogOpen(true)}
              complianceConfirmed={
                topupInfo?.payment_compliance_confirmed !== false
              }
              loading={affiliateLoading}
            />
          </div>
        </SectionPageLayout.Content>
      </SectionPageLayout>
      {rewards && (
        <TransferDialog
          open={transferDialogOpen}
          onOpenChange={setTransferDialogOpen}
          onConfirm={handleTransfer}
          available={rewards.available}
          attempt={transferAttempt}
          transferring={transferring}
        />
      )}
      <BillingHistoryDialog
        open={billingDialogOpen}
        onOpenChange={setBillingDialogOpen}
      />
    </>
  )
}
