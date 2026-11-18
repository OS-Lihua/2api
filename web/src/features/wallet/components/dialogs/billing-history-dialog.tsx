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
import { ChevronLeft, ChevronRight, Search } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { getServerErrorMessage } from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

import { useBillingHistory } from '../../hooks/use-billing-history'
import type { TopupRecord } from '../../types'
import { BillingHistoryRecord } from '../billing-history-record'

interface BillingHistoryDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
}

export function BillingHistoryDialog(props: BillingHistoryDialogProps) {
  const userId = useAuthStore((state) => state.auth.user?.id)
  const sessionId = useAuthStore((state) => state.auth.session?.sid ?? null)
  const role = useAuthStore((state) => state.auth.user?.role)
  if (!userId) return null
  return (
    <BillingHistoryContent key={`${userId}:${sessionId}:${role}`} {...props} />
  )
}

function BillingHistoryContent(props: BillingHistoryDialogProps) {
  const { t } = useTranslation()
  const history = useBillingHistory({ enabled: props.open })
  const [confirmRecord, setConfirmRecord] = useState<TopupRecord | null>(null)
  const totalPages = Math.ceil(history.total / history.pageSize)

  return (
    <>
      <Dialog
        open={props.open}
        onOpenChange={props.onOpenChange}
        title={t('Billing History')}
        description={t(
          'View your topup transaction records and payment history'
        )}
        contentClassName='flex max-h-(--dialog-available-height) flex-col max-sm:w-screen max-sm:max-w-none max-sm:rounded-none max-sm:p-4 sm:max-w-4xl'
        contentHeight='auto'
        bodyClassName='space-y-3'
      >
        <div className='min-h-0 min-w-0 space-y-3'>
          <div className='flex items-center gap-2'>
            <div className='relative min-w-0 flex-1'>
              <Search
                aria-hidden='true'
                className='text-muted-foreground absolute top-1/2 left-3 size-4 -translate-y-1/2'
              />
              <Input
                aria-label={t('Search by order number...')}
                placeholder={t('Search by order number...')}
                value={history.keyword}
                onChange={(event) => history.handleSearch(event.target.value)}
                className='h-9 pl-10'
              />
            </div>
            <Select
              items={[10, 20, 50, 100].map((size) => ({
                value: String(size),
                label: t(`${size} / page`),
              }))}
              value={String(history.pageSize)}
              onValueChange={(value) => {
                if (value !== null) history.handlePageSizeChange(Number(value))
              }}
            >
              <SelectTrigger
                aria-label={t('Rows per page')}
                className='h-9 w-[92px] sm:w-32'
              >
                <SelectValue />
              </SelectTrigger>
              <SelectContent alignItemWithTrigger={false}>
                {[10, 20, 50, 100].map((size) => (
                  <SelectItem key={size} value={String(size)}>
                    {t(`${size} / page`)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className='max-h-[min(54vh,520px)] min-w-0 overflow-y-auto pr-1'>
            {history.loading && (
              <div className='space-y-3'>
                {['first', 'second', 'third'].map((placeholder) => (
                  <Skeleton key={placeholder} className='h-32 w-full' />
                ))}
              </div>
            )}
            {!history.loading && history.error && (
              <ErrorState
                description={getServerErrorMessage(history.error)}
                onRetry={() => void history.refresh()}
              />
            )}
            {!history.loading &&
              !history.error &&
              history.records.length === 0 && (
                <div className='text-muted-foreground flex min-h-40 flex-col items-center justify-center py-10 text-center'>
                  <p className='text-sm font-medium'>
                    {t('No billing records found')}
                  </p>
                  <p className='mt-1 text-xs'>
                    {history.keyword
                      ? t('Try adjusting your search')
                      : t('Your transaction history will appear here')}
                  </p>
                </div>
              )}
            {!history.loading &&
              !history.error &&
              history.records.length > 0 && (
                <div className='space-y-3'>
                  {history.records.map((record) => (
                    <BillingHistoryRecord
                      key={record.id}
                      record={record}
                      isAdmin={history.isAdmin}
                      completing={history.completing}
                      onComplete={setConfirmRecord}
                    />
                  ))}
                </div>
              )}
          </div>
          {!history.loading && !history.error && history.records.length > 0 && (
            <div className='flex flex-col items-center gap-3 border-t pt-4 sm:flex-row sm:items-center sm:justify-between'>
              <p className='text-muted-foreground text-xs sm:text-sm'>
                {t('Showing')} {(history.page - 1) * history.pageSize + 1}-
                {Math.min(history.page * history.pageSize, history.total)}{' '}
                {t('of')} {history.total}
              </p>
              <div className='flex items-center gap-2'>
                <Button
                  aria-label={t('Previous page')}
                  variant='outline'
                  size='sm'
                  onClick={() => history.handlePageChange(history.page - 1)}
                  disabled={history.page <= 1}
                  className='h-8 w-8 p-0'
                >
                  <ChevronLeft className='size-4' />
                </Button>
                <p className='text-muted-foreground text-sm'>
                  {history.page} / {totalPages}
                </p>
                <Button
                  aria-label={t('Next page')}
                  variant='outline'
                  size='sm'
                  onClick={() => history.handlePageChange(history.page + 1)}
                  disabled={history.page >= totalPages}
                  className='h-8 w-8 p-0'
                >
                  <ChevronRight className='size-4' />
                </Button>
              </div>
            </div>
          )}
        </div>
      </Dialog>
      <ConfirmDialog
        open={confirmRecord !== null}
        onOpenChange={(open) => {
          if (!open && !history.completing) setConfirmRecord(null)
        }}
        title={t('Complete Order')}
        desc={t(
          confirmRecord?.contract_version === 1
            ? 'Complete the wallet credit for this confirmed payment. Outstanding debt is repaid first.'
            : 'Complete this historical order using its stored payment and reward terms.'
        )}
        confirmText={t('Confirm')}
        isLoading={history.completing}
        handleConfirm={() => {
          if (!confirmRecord || history.completing) return
          void history
            .handleCompleteOrder(confirmRecord.trade_no)
            .then((success) => {
              if (success) setConfirmRecord(null)
            })
        }}
      />
    </>
  )
}
