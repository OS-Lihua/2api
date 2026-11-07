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
import { keepPreviousData, skipToken, useQuery } from '@tanstack/react-query'
import type { ColumnDef } from '@tanstack/react-table'
import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { DataTablePage, useDataTable } from '@/components/data-table'
import { ErrorState } from '@/components/error-state'
import { SectionPageLayout } from '@/components/layout'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { toIntlLocale } from '@/i18n/languages'
import { hasPermission } from '@/lib/admin-permissions'
import { getServerErrorMessage } from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

import { getCurrentPrice, listPriceVersions, pricingKeys } from './api'
import { PriceDialog } from './components/price-dialog'
import type { OwnerType, PriceListParams, PriceVersion } from './types'

export function PriceVersions() {
  const { t, i18n } = useTranslation()
  const user = useAuthStore((state) => state.auth.user)
  const canWrite = hasPermission(user, 'pricing', 'write')
  const [creating, setCreating] = useState(false)
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const [pagination, setPagination] = useState({ pageIndex: 0, pageSize: 20 })
  const [ownerType, setOwnerType] = useState<OwnerType | ''>('')
  const [ownerId, setOwnerId] = useState('')
  const [filter, setFilter] = useState<
    Pick<PriceListParams, 'owner_type' | 'owner_id'>
  >({})
  const validId =
    ownerId === '' ||
    (/^[1-9]\d*$/.test(ownerId) && Number.isSafeInteger(Number(ownerId)))
  const params = {
    p: pagination.pageIndex + 1,
    page_size: pagination.pageSize,
    ...filter,
  }
  const query = useQuery({
    queryKey: pricingKeys.list(params),
    queryFn: () => listPriceVersions(params),
    placeholderData: keepPreviousData,
  })
  const [now, setNow] = useState(() => Date.now())
  const hasScheduled = query.data?.items.some(
    (version) => version.effective_at > now / 1000
  )
  useEffect(() => {
    setNow(Date.now())
    if (!hasScheduled) return
    const timer = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(timer)
  }, [hasScheduled, query.data])
  const filteredType = filter.owner_type
  const filteredId = filter.owner_id
  const current = useQuery({
    queryKey: [...pricingKeys.prices, 'current', filteredType, filteredId],
    queryFn:
      filteredType && filteredId
        ? () => getCurrentPrice(filteredType, filteredId)
        : skipToken,
    refetchOnMount: 'always',
    refetchInterval: 30_000,
  })
  const columns: ColumnDef<PriceVersion>[] = [
    { accessorKey: 'id', header: t('Version'), meta: { mobileTitle: true } },
    {
      accessorKey: 'owner_type',
      header: t('Price owner'),
      cell: ({ row }) =>
        `${row.original.owner_type === 'channel' ? t('Channel') : t('Model offering')} #${row.original.owner_id}`,
    },
    { accessorKey: 'currency', header: t('Currency') },
    {
      accessorKey: 'expression',
      header: t('Billing expression'),
      cell: ({ row }) => (
        <code className='block max-w-xl text-xs break-all whitespace-pre-wrap'>
          {row.original.expression}
        </code>
      ),
    },
    {
      accessorKey: 'effective_at',
      header: t('Effective at'),
      cell: ({ row }) =>
        new Date(row.original.effective_at * 1000).toLocaleString(locale),
    },
    {
      id: 'status',
      header: t('Status'),
      cell: ({ row }) =>
        row.original.effective_at > now / 1000
          ? t('Scheduled')
          : t('Effective'),
    },
    { accessorKey: 'created_by', header: t('Created by') },
    {
      accessorKey: 'created_at',
      header: t('Created at'),
      cell: ({ row }) =>
        new Date(row.original.created_at * 1000).toLocaleString(locale),
    },
  ]
  const { table } = useDataTable({
    data: query.data?.items ?? [],
    columns,
    getRowId: (row) => String(row.id),
    pagination,
    onPaginationChange: setPagination,
    columnFilters: [],
    manualFiltering: true,
    manualPagination: true,
    totalCount: query.data?.total ?? 0,
  })
  return (
    <SectionPageLayout fixedContent>
      <SectionPageLayout.Title>{t('Price versions')}</SectionPageLayout.Title>
      <SectionPageLayout.Actions>
        {canWrite && (
          <Button size='sm' onClick={() => setCreating(true)}>
            {t('New price version')}
          </Button>
        )}
      </SectionPageLayout.Actions>
      <SectionPageLayout.Content>
        {creating && <PriceDialog onClose={() => setCreating(false)} />}
        <p className='text-muted-foreground mb-4 text-sm'>
          {t(
            'Price history is immutable. Create a new version to change a price.'
          )}
        </p>
        {filteredType && filteredId && (
          <section
            aria-label={t('Current price version')}
            className='mb-4 rounded-md border p-3'
            aria-busy={current.isFetching}
          >
            <h3 className='text-sm font-medium'>
              {t('Current price version')}
            </h3>
            {current.isPending && <p>{t('Loading...')}</p>}
            {current.isError && (
              <ErrorState
                description={getServerErrorMessage(current.error)}
                onRetry={() => void current.refetch()}
              />
            )}
            {current.isSuccess &&
              (current.data ? (
                <p>
                  #{current.data.id} · {current.data.currency}
                </p>
              ) : (
                <p>{t('No price is currently effective for this owner.')}</p>
              ))}
          </section>
        )}
        {query.isError && (
          <ErrorState
            description={getServerErrorMessage(query.error)}
            onRetry={() => void query.refetch()}
          />
        )}
        <DataTablePage
          table={table}
          columns={columns}
          isLoading={query.isLoading}
          isFetching={query.isFetching}
          emptyTitle={t('No price versions found')}
          toolbarProps={{
            customSearch: (
              <div className='flex flex-wrap items-center gap-2'>
                <NativeSelect
                  aria-label={t('Price owner type')}
                  value={ownerType}
                  onChange={(event) =>
                    setOwnerType(event.target.value as OwnerType | '')
                  }
                >
                  <NativeSelectOption value=''>
                    {t('All price owners')}
                  </NativeSelectOption>
                  <NativeSelectOption value='model_offering'>
                    {t('Model offering')}
                  </NativeSelectOption>
                  <NativeSelectOption value='channel'>
                    {t('Channel')}
                  </NativeSelectOption>
                </NativeSelect>
                <Input
                  className='w-40'
                  inputMode='numeric'
                  aria-label={t('Price owner ID')}
                  placeholder={t('Price owner ID')}
                  value={ownerId}
                  aria-invalid={!validId || undefined}
                  onChange={(event) => setOwnerId(event.target.value)}
                />
                <Button
                  variant='outline'
                  disabled={!validId || (ownerId !== '' && !ownerType)}
                  onClick={() => {
                    setFilter({
                      owner_type: ownerType || undefined,
                      owner_id: ownerId ? Number(ownerId) : undefined,
                    })
                    setPagination((current) => ({ ...current, pageIndex: 0 }))
                  }}
                >
                  {t('Apply owner filter')}
                </Button>
              </div>
            ),
          }}
        />
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}
