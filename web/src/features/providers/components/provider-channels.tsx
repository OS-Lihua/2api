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
import { useMutation, useQuery } from '@tanstack/react-query'
import type { ColumnDef } from '@tanstack/react-table'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { DataTablePage, useDataTable } from '@/components/data-table'
import { ErrorState } from '@/components/error-state'
import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import { toIntlLocale } from '@/i18n/languages'
import {
  hasPermission,
  ADMIN_PERMISSION_RESOURCES,
  ADMIN_PERMISSION_ACTIONS,
} from '@/lib/admin-permissions'
import { formatNumber } from '@/lib/format'
import { useAuthStore } from '@/stores/auth-store'

import {
  channelAction,
  channelKeys,
  listSingleModelChannels,
  type SingleModelChannel,
} from '../channel-api'
import { getChannelProbeLabels } from '../lib/channel-probes'
import { ChannelMutateDrawer } from './channel-mutate-drawer'
import { ChannelProbeDialog } from './channel-probe-dialog'

export function ProviderChannels(props: { providerId: number }) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const featureLabels = getChannelProbeLabels(t)
  const user = useAuthStore((state) => state.auth.user)
  const canWrite = hasPermission(
    user,
    ADMIN_PERMISSION_RESOURCES.CHANNEL,
    ADMIN_PERMISSION_ACTIONS.WRITE
  )
  const canOperate = hasPermission(
    user,
    ADMIN_PERMISSION_RESOURCES.CHANNEL,
    ADMIN_PERMISSION_ACTIONS.OPERATE
  )
  const [probing, setProbing] = useState<SingleModelChannel | null>(null)
  const [pagination, setPagination] = useState({ pageIndex: 0, pageSize: 10 })
  const [editing, setEditing] = useState<SingleModelChannel | null | undefined>(
    undefined
  )
  const [operation, setOperation] = useState<{
    channel: SingleModelChannel
    action: string
  } | null>(null)
  const query = useQuery({
    queryKey: channelKeys.list(props.providerId, pagination.pageIndex + 1),
    queryFn: () =>
      listSingleModelChannels(props.providerId, pagination.pageIndex + 1),
    refetchInterval: 5_000,
  })
  const mutation = useMutation({
    mutationFn: async () => {
      if (operation) await channelAction(operation.channel.id, operation.action)
    },
    onSuccess: async () => {
      setOperation(null)
      await query.refetch()
    },
  })
  const columns: ColumnDef<SingleModelChannel>[] = [
    {
      accessorKey: 'upstream_model_name',
      header: t('Upstream model name'),
      meta: { mobileTitle: true },
    },
    { accessorKey: 'protocol', header: t('Protocol') },
    { accessorKey: 'model_id', header: t('Model ID') },
    {
      accessorKey: 'status',
      header: t('Status'),
      cell: ({ row }) => (
        <StatusBadge
          label={t(
            {
              pending: 'Pending confirmation',
              enabled: 'Enabled',
              disabled: 'Disabled',
            }[row.original.status]
          )}
          variant={row.original.status === 'enabled' ? 'success' : 'neutral'}
          copyable={false}
        />
      ),
    },
    {
      id: 'health',
      header: t('Runtime health'),
      cell: ({ row }) => {
        const health = row.original.health
        if (!health) return t('Unavailable')
        const labels = {
          closed: t('Circuit closed'),
          open: t('Circuit open'),
          half_open: t('Half-open probe'),
        }
        return (
          <div className='space-y-1 text-xs'>
            <StatusBadge
              label={labels[health.circuit]}
              variant={health.circuit === 'closed' ? 'success' : 'neutral'}
              copyable={false}
            />
            {health.samples > 0 ? (
              <dl>
                <div>
                  <dt>{t('Recent success rate')}</dt>
                  <dd>{formatNumber(health.success_rate * 100, locale)}%</dd>
                </div>
                <div>
                  <dt>{t('First business byte latency')}</dt>
                  <dd>
                    {health.first_byte_samples > 0
                      ? t('{{value}} ms', {
                          value: formatNumber(health.first_byte_ms, locale),
                        })
                      : '-'}
                  </dd>
                </div>
                <div>
                  <dt>{t('Health samples')}</dt>
                  <dd>{formatNumber(health.samples, locale)}</dd>
                </div>
              </dl>
            ) : (
              <p>{t('No recent samples')}</p>
            )}
            {health.retry_at > 0 && (
              <p>
                {t('Probe after')}:{' '}
                <time dateTime={new Date(health.retry_at).toISOString()}>
                  {new Date(health.retry_at).toLocaleString(locale)}
                </time>
              </p>
            )}
          </div>
        )
      },
    },
    {
      id: 'probe',
      header: t('Capability probes'),
      cell: ({ row }) => {
        const result = row.original.latest_probe
        if (!result) return t('Not tested')
        let status = t('Inconclusive')
        if (result.status === 'passed') status = t('Success')
        if (result.status === 'failed') status = t('Failed')
        if (result.status === 'running') status = t('Testing...')
        return (
          <div className='space-y-1 text-xs'>
            <div>
              {featureLabels.get(result.capability) ?? result.capability} ·{' '}
              {status}
            </div>
            {result.finished_at > 0 && (
              <time
                dateTime={new Date(result.finished_at * 1000).toISOString()}
              >
                {new Date(result.finished_at * 1000).toLocaleString(locale)}
              </time>
            )}
          </div>
        )
      },
    },
    {
      id: 'actions',
      header: t('Actions'),
      cell: ({ row }) => (
        <div className='flex flex-wrap gap-2'>
          <Button
            size='sm'
            variant='outline'
            onClick={() => setProbing(row.original)}
          >
            {t('Capability probes')}
          </Button>
          {canWrite && (
            <Button
              size='sm'
              variant='outline'
              onClick={() => setEditing(row.original)}
            >
              {t('Edit')}
            </Button>
          )}
          {canOperate && !row.original.mapping_confirmed && (
            <Button
              size='sm'
              variant='outline'
              onClick={() =>
                setOperation({ channel: row.original, action: 'confirm' })
              }
            >
              {t('Confirm mapping')}
            </Button>
          )}
          {canOperate && row.original.mapping_confirmed && (
            <Button
              size='sm'
              variant='outline'
              onClick={() =>
                setOperation({
                  channel: row.original,
                  action:
                    row.original.status === 'enabled' ? 'disable' : 'enable',
                })
              }
            >
              {row.original.status === 'enabled' ? t('Disable') : t('Enable')}
            </Button>
          )}
          {canWrite && (
            <Button
              size='sm'
              variant='destructive'
              onClick={() =>
                setOperation({ channel: row.original, action: 'delete' })
              }
            >
              {t('Delete')}
            </Button>
          )}
        </div>
      ),
    },
  ]
  const { table } = useDataTable({
    data: query.data?.items ?? [],
    columns,
    getRowId: (row) => String(row.id),
    pagination,
    onPaginationChange: setPagination,
    manualPagination: true,
    totalCount: query.data?.total ?? 0,
  })
  return (
    <section
      className='w-full max-w-[calc(100vw-3rem)] space-y-3 p-4 whitespace-normal md:max-w-none'
      aria-label={t('Channels')}
    >
      <div className='flex items-center justify-between'>
        <h3 className='font-semibold'>{t('Channels')}</h3>
        {canWrite && (
          <Button size='sm' onClick={() => setEditing(null)}>
            {t('New Channel')}
          </Button>
        )}
      </div>
      {query.isError ? (
        <ErrorState onRetry={() => void query.refetch()} />
      ) : (
        <DataTablePage
          table={table}
          columns={columns}
          fixedHeight={false}
          paginationInFooter={false}
          compactPagination
          isLoading={query.isLoading}
          emptyTitle={t('No channels found')}
        />
      )}
      {editing !== undefined && (
        <ChannelMutateDrawer
          providerId={props.providerId}
          channel={editing ?? undefined}
          onClose={() => setEditing(undefined)}
          onSaved={() => {
            setEditing(undefined)
            void query.refetch()
          }}
        />
      )}
      {probing && (
        <ChannelProbeDialog
          channel={probing}
          canOperate={canOperate}
          onClose={() => setProbing(null)}
        />
      )}
      <ConfirmDialog
        open={operation !== null}
        onOpenChange={(open) => {
          if (!open && !mutation.isPending) setOperation(null)
        }}
        title={t('Confirm channel change')}
        desc={operation?.channel.upstream_model_name ?? ''}
        destructive={operation?.action === 'delete'}
        isLoading={mutation.isPending}
        handleConfirm={() => mutation.mutate()}
      />
    </section>
  )
}
