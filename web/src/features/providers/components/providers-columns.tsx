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
import { useTranslation } from 'react-i18next'

import { StatusBadge } from '@/components/status-badge'
import { TableId } from '@/components/table-id'
import { Button } from '@/components/ui/button'
import { ChannelTypeLogo } from '@/features/channels/components/channel-type-badge'
import {
  CHANNEL_STATUS_CONFIG,
  CHANNEL_TYPES,
} from '@/features/channels/constants'
import { formatCurrencyFromUSD } from '@/lib/currency'
import { formatTimestampToDate } from '@/lib/format'

import type { Provider } from '../types'
import { ProviderRowActions } from './provider-row-actions'

export function useProvidersColumns(): ColumnDef<Provider>[] {
  const { t } = useTranslation()
  return [
    {
      accessorKey: 'id',
      header: t('ID'),
      meta: { mobileHidden: true },
      cell: ({ row }) => (
        <TableId value={row.original.id} className='w-[60px]' />
      ),
      size: 80,
    },
    {
      accessorKey: 'name',
      header: t('Name'),
      meta: { mobileTitle: true },
      cell: ({ row }) => (
        <div className='space-y-2'>
          <Button
            variant='ghost'
            size='sm'
            aria-expanded={row.getIsExpanded()}
            aria-controls={`provider-channels-${row.original.id}`}
            onClick={() => row.toggleExpanded()}
          >
            {row.original.name}
          </Button>
          <span className='sr-only'>{t('Channels')}</span>
        </div>
      ),
      size: 200,
    },
    {
      accessorKey: 'type',
      header: t('Type'),
      cell: ({ row }) => {
        const type = row.original.type
        const label =
          CHANNEL_TYPES[type as keyof typeof CHANNEL_TYPES] ?? String(type)
        return (
          <div className='flex items-center gap-2'>
            <ChannelTypeLogo type={type} />
            <span className='text-sm'>{label}</span>
          </div>
        )
      },
      size: 180,
    },
    {
      accessorKey: 'status',
      header: t('Status'),
      meta: { mobileBadge: true },
      cell: ({ row }) => {
        const config =
          CHANNEL_STATUS_CONFIG[
            row.original.status as keyof typeof CHANNEL_STATUS_CONFIG
          ] ?? CHANNEL_STATUS_CONFIG[0]
        return (
          <StatusBadge
            label={t(config.label)}
            variant={config.variant}
            showDot
            copyable={false}
            className='-ml-1.5'
          />
        )
      },
      size: 130,
    },
    {
      id: 'keys',
      header: t('Keys'),
      cell: ({ row }) => {
        const info = row.original.channel_info
        if (!info?.is_multi_key) {
          return <span className='text-sm'>{t('Single key')}</span>
        }
        return (
          <span className='text-sm'>
            {t('{{count}} keys', { count: info.multi_key_size })}
          </span>
        )
      },
      size: 120,
    },
    {
      accessorKey: 'balance',
      header: t('Balance'),
      cell: ({ row }) => {
        const provider = row.original
        if (!provider.balance_updated_time) {
          return <span className='text-muted-foreground text-sm'>-</span>
        }
        return (
          <div className='flex flex-col'>
            <span className='font-mono text-sm'>
              {formatCurrencyFromUSD(provider.balance)}
            </span>
            <span className='text-muted-foreground text-xs'>
              {formatTimestampToDate(provider.balance_updated_time)}
            </span>
          </div>
        )
      },
      size: 180,
    },
    {
      id: 'actions',
      header: () => t('Actions'),
      cell: ({ row }) => <ProviderRowActions provider={row.original} />,
      meta: { pinned: 'right' as const },
    },
  ]
}
