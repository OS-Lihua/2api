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
import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { DataTablePage, useDataTable } from '@/components/data-table'
import { ErrorState } from '@/components/error-state'
import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber } from '@/lib/format'
import { useAuthStore } from '@/stores/auth-store'

import {
  assertChannelAccess,
  channelAccessScope,
  channelScopeKey,
  isChannelScopeCurrent,
  type ChannelAccessScope,
  channelAction,
  channelKeys,
  listSingleModelChannels,
  type SingleModelChannel,
} from '../channel-api'
import { getChannelProbeLabels } from '../lib/channel-probes'
import { ChannelMutateDrawer } from './channel-mutate-drawer'
import { ChannelProbeDialog } from './channel-probe-dialog'

type ProviderChannelsProps =
  | { providerId: number; modelId?: never; readOnly?: boolean }
  | { modelId: number; providerId?: never; readOnly: true }

export function ProviderChannels(props: ProviderChannelsProps) {
  useAuthStore((state) => state.auth.user)
  useAuthStore((state) => state.auth.session?.sid)
  const scope = channelAccessScope()
  return (
    <ScopedChannels
      key={JSON.stringify([
        channelScopeKey(scope),
        props.providerId,
        props.modelId,
        props.readOnly,
      ])}
      {...props}
      scope={scope}
    />
  )
}

function ScopedChannels(
  props: ProviderChannelsProps & { scope: ChannelAccessScope }
) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const featureLabels = getChannelProbeLabels(t)
  const canWrite = props.scope.read && props.scope.write && !props.readOnly
  const canOperate = props.scope.read && props.scope.operate && !props.readOnly
  const active = useRef(true)
  useEffect(() => {
    active.current = true
    return () => {
      active.current = false
    }
  }, [])
  const current = () => active.current && isChannelScopeCurrent(props.scope)
  const [probing, setProbing] = useState<SingleModelChannel | null>(null)
  const [pagination, setPagination] = useState({ pageIndex: 0, pageSize: 10 })
  const [editing, setEditing] = useState<SingleModelChannel | null | undefined>(
    undefined
  )
  const [operation, setOperation] = useState<{
    channel: SingleModelChannel
    action: string
  } | null>(null)
  const filter =
    props.modelId === undefined
      ? { provider_id: props.providerId }
      : { model_id: props.modelId }
  const validTarget =
    Number.isSafeInteger(props.modelId ?? props.providerId) &&
    (props.modelId ?? props.providerId) > 0
  const query = useQuery({
    queryKey: channelKeys.list(
      filter,
      pagination.pageIndex + 1,
      pagination.pageSize,
      props.scope
    ),
    queryFn: async ({ signal }) => {
      assertChannelAccess(props.scope, 'read', signal)
      const result = await listSingleModelChannels(
        filter,
        pagination.pageIndex + 1,
        signal,
        pagination.pageSize
      )
      assertChannelAccess(props.scope, 'read', signal)
      return result
    },
    enabled: props.scope.read && validTarget,
    retry: false,
    staleTime: 0,
    gcTime: 0,
    refetchInterval: 5_000,
    meta: { errorToast: false },
  })
  const mutation = useMutation({
    meta: { errorToast: false },
    mutationFn: async () => {
      if (!current() || props.readOnly || !operation) {
        throw new Error('Channel access changed')
      }
      assertChannelAccess(
        props.scope,
        operation.action === 'delete' ? 'write' : 'operate'
      )
      await channelAction(operation.channel.id, operation.action)
      return `${pagination.pageIndex}:${pagination.pageSize}`
    },
    onSuccess: async (completedPage) => {
      if (
        !current() ||
        completedPage !== `${pagination.pageIndex}:${pagination.pageSize}`
      ) {
        return
      }
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
    { accessorKey: 'provider_id', header: t('Provider ID') },
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
  const visibleColumns = props.readOnly
    ? columns.filter((column) => column.id !== 'actions')
    : columns
  const { table } = useDataTable({
    data: props.scope.read && !query.isError ? (query.data?.items ?? []) : [],
    columns: visibleColumns,
    getRowId: (row) => String(row.id),
    pagination,
    onPaginationChange: (next) => {
      setOperation(null)
      setEditing(undefined)
      setProbing(null)
      setPagination(next)
    },
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
      {!props.scope.read && (
        <p role='status'>
          {t('Channel read permission is required to view connections.')}
        </p>
      )}
      {props.scope.read && query.isError && (
        <ErrorState onRetry={() => void query.refetch()} />
      )}
      {props.scope.read && !query.isError && (
        <DataTablePage
          table={table}
          columns={visibleColumns}
          fixedHeight={false}
          paginationInFooter={false}
          compactPagination
          isLoading={query.isLoading}
          emptyTitle={t('No channels found')}
        />
      )}
      {canWrite && editing !== undefined && props.providerId !== undefined && (
        <ChannelMutateDrawer
          providerId={props.providerId}
          channel={editing ?? undefined}
          onClose={() => setEditing(undefined)}
          onSaved={() => {
            if (!current()) return
            setEditing(undefined)
            void query.refetch()
          }}
        />
      )}
      {props.scope.read && !props.readOnly && probing && (
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
        desc={
          <span className='wrap-anywhere'>
            {operation?.channel.upstream_model_name ?? ''}
          </span>
        }
        destructive={operation?.action === 'delete'}
        isLoading={mutation.isPending}
        handleConfirm={() => mutation.mutate()}
      >
        {mutation.isError && <p role='alert'>{mutation.error.message}</p>}
      </ConfirmDialog>
    </section>
  )
}
