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
import { X } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { DataTablePagination, useDataTable } from '@/components/data-table'
import {
  sideDrawerContentClassName,
  sideDrawerFormClassName,
  sideDrawerHeaderClassName,
} from '@/components/drawer-layout'
import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Button } from '@/components/ui/button'
import {
  Sheet,
  SheetClose,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'
import { getServerErrorMessage } from '@/lib/server-error-message'

import { getRequestAttempts } from '../api'
import type { RequestLog, RequestScope } from '../types'
import { AttemptAdminFacts } from './attempt-admin-facts'
import {
  RequestIdentifier,
  RequestOutcome,
  RequestTime,
  RequestUsageFacts,
} from './request-facts'
import { RequestSummary } from './request-summary'

export function AttemptsDrawer(props: {
  request: RequestLog
  scope: RequestScope
  userId: number
  onClose: () => void
}) {
  const { t } = useTranslation()
  const [page, setPage] = useState(1)
  const query = useQuery({
    queryKey: [
      'requests',
      props.userId,
      props.scope,
      props.request.id,
      'attempts',
      page,
    ],
    queryFn: ({ signal }) =>
      getRequestAttempts(props.scope, props.request.id, page, signal),
  })
  const items = query.data?.items ?? []
  const { table } = useDataTable({
    data: items,
    columns: [],
    totalCount: query.data?.total ?? 0,
    pagination: { pageIndex: page - 1, pageSize: 20 },
    onPaginationChange: (updater) => {
      if (query.isFetching || query.isError) return
      const next =
        typeof updater === 'function'
          ? updater({ pageIndex: page - 1, pageSize: 20 })
          : updater
      setPage(next.pageIndex + 1)
    },
    manualPagination: true,
    enableRowSelection: false,
    enableSorting: false,
  })
  let content
  if (query.isPending) {
    content = <LoadingState message={t('Loading attempts...')} />
  } else if (query.isError) {
    content = (
      <ErrorState
        title={t('Failed to load attempts')}
        description={getServerErrorMessage(query.error)}
        onRetry={() => void query.refetch()}
      />
    )
  } else if (!items.length) {
    content = <EmptyState title={t('No attempts recorded')} />
  } else {
    content = (
      <>
        <ol aria-label={t('Attempt chronology')} className='space-y-5'>
          {items.map((attempt) => (
            <li
              key={attempt.id}
              className='space-y-3 border-b pb-5 last:border-0'
            >
              <div className='flex min-w-0 flex-wrap items-center justify-between gap-2'>
                <div className='min-w-0 flex-1'>
                  <RequestIdentifier
                    value={attempt.id}
                    label={t('Copy attempt ID')}
                  />
                </div>
                <RequestOutcome status={attempt.status} />
              </div>
              <dl className='grid gap-3 text-sm sm:grid-cols-2'>
                <div>
                  <dt className='text-muted-foreground text-xs'>
                    {t('Started at')}
                  </dt>
                  <dd>
                    <RequestTime timestamp={attempt.started_at} />
                  </dd>
                </div>
                <div>
                  <dt className='text-muted-foreground text-xs'>
                    {t('Ended at')}
                  </dt>
                  <dd>
                    <RequestTime timestamp={attempt.ended_at} />
                  </dd>
                </div>
              </dl>
              <RequestUsageFacts
                usage={attempt.usage}
                source={attempt.usage_source}
              />
              {props.scope === 'all' && <AttemptAdminFacts attempt={attempt} />}
              {attempt.error_class && (
                <p className='text-destructive text-xs break-words'>
                  {t('Error class')}: {attempt.error_class}
                </p>
              )}
              {!!attempt.conversion_losses?.length && (
                <div className='space-y-1 text-xs'>
                  <h4 className='font-medium'>{t('Conversion losses')}</h4>
                  <ul className='list-inside list-disc space-y-1 break-words'>
                    {attempt.conversion_losses.map((loss) => (
                      <li key={loss}>{loss}</li>
                    ))}
                  </ul>
                </div>
              )}
            </li>
          ))}
        </ol>
        {(query.data?.total ?? 0) > 20 && (
          <DataTablePagination table={table} compact />
        )}
      </>
    )
  }
  return (
    <Sheet
      open
      onOpenChange={(open) => {
        if (!open) props.onClose()
      }}
    >
      <SheetContent
        showCloseButton={false}
        className={sideDrawerContentClassName('sm:max-w-2xl')}
      >
        <SheetClose
          render={
            <Button
              variant='ghost'
              size='icon-sm'
              className='absolute top-3 right-3'
              aria-label={t('Close')}
            />
          }
        >
          <X aria-hidden='true' />
        </SheetClose>
        <SheetHeader className={sideDrawerHeaderClassName('pr-12 sm:pr-14')}>
          <SheetTitle>{t('Request attempts')}</SheetTitle>
          <SheetDescription>
            {t('Upstream calls recorded for this request.')}
          </SheetDescription>
          <RequestIdentifier
            value={props.request.id}
            label={t('Copy request ID')}
          />
        </SheetHeader>
        <div className={sideDrawerFormClassName()}>
          <RequestSummary request={props.request} scope={props.scope} />
          {content}
        </div>
      </SheetContent>
    </Sheet>
  )
}
