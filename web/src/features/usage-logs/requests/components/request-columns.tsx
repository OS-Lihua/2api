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
import type { ColumnDef } from '@tanstack/react-table'
import { useMemo } from 'react'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'

import type { RequestLog } from '../types'
import {
  RequestIdentifier,
  RequestMoney,
  RequestOutcome,
  RequestTime,
  RequestUsageFacts,
} from './request-facts'

export function useRequestColumns(onSelect: (request: RequestLog) => void) {
  const { t } = useTranslation()
  return useMemo<ColumnDef<RequestLog>[]>(
    () => [
      {
        accessorKey: 'id',
        header: t('Request ID'),
        size: 240,
        cell: ({ row }) => (
          <div className='w-56 max-w-full'>
            <RequestIdentifier
              value={row.original.id}
              label={t('Copy request ID')}
            />
          </div>
        ),
      },
      {
        accessorKey: 'offering_name',
        header: t('Model Offering'),
        size: 200,
        cell: ({ row }) => (
          <div className='max-w-48 min-w-0 space-y-1'>
            <div
              className='truncate font-medium'
              title={row.original.offering_name}
            >
              {row.original.offering_name || '-'}
            </div>
            {row.original.requested_name &&
              row.original.requested_name !== row.original.offering_name && (
                <div
                  className='text-muted-foreground truncate text-xs'
                  title={row.original.requested_name}
                >
                  {row.original.requested_name}
                </div>
              )}
          </div>
        ),
      },
      {
        accessorKey: 'status',
        header: t('Status'),
        size: 150,
        cell: ({ row }) => (
          <div className='space-y-1'>
            <RequestOutcome status={row.original.status} />
            {row.original.error_code && (
              <div
                title={row.original.error_code}
                className='text-muted-foreground max-w-40 truncate font-mono text-xs'
              >
                {row.original.error_code}
              </div>
            )}
          </div>
        ),
      },
      {
        id: 'usage',
        header: t('Usage'),
        size: 210,
        cell: ({ row }) => (
          <RequestUsageFacts
            usage={row.original.usage}
            source={row.original.usage_source}
          />
        ),
      },
      {
        id: 'receivable',
        header: t('Charge'),
        size: 220,
        cell: ({ row }) => (
          <RequestMoney
            amount={row.original.receivable}
            pending={row.original.sale_pending}
          />
        ),
      },
      {
        accessorKey: 'sale_price_version_id',
        header: t('Sale price version'),
        size: 150,
        cell: ({ row }) => row.original.sale_price_version_id ?? '-',
      },
      {
        accessorKey: 'started_at',
        header: t('Started at'),
        size: 230,
        cell: ({ row }) => <RequestTime timestamp={row.original.started_at} />,
      },
      {
        id: 'attempts',
        header: t('Attempts'),
        size: 110,
        enableHiding: false,
        cell: ({ row }) => (
          <Button
            size='sm'
            variant='outline'
            aria-label={t('View attempts for request {{id}}', {
              id: row.original.id,
            })}
            onClick={() => onSelect(row.original)}
          >
            {t('Attempts')}
          </Button>
        ),
      },
    ],
    [onSelect, t]
  )
}
