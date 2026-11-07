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
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import type { ColumnDef } from '@tanstack/react-table'
import { useMemo } from 'react'
import { useTranslation } from 'react-i18next'

import { DataTablePage, useDataTable } from '@/components/data-table'
import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber } from '@/lib/format'

import {
  channelKeys,
  listChannelProbes,
  runChannelProbe,
  type ChannelProbeResult,
  type SingleModelChannel,
} from '../channel-api'
import { getChannelProbeLabels } from '../lib/channel-probes'

type ProbeRow = { capability: string; result?: ChannelProbeResult }

export function ChannelProbeDialog(props: {
  channel: SingleModelChannel
  canOperate: boolean
  onClose: () => void
}) {
  const client = useQueryClient()
  const { t, i18n } = useTranslation()
  const query = useQuery({
    queryKey: ['channel-probes', props.channel.id],
    queryFn: () => listChannelProbes(props.channel.id),
    refetchInterval: (query) =>
      query.state.data?.results.some((result) => result.status === 'running')
        ? 1500
        : false,
  })
  const { mutate, isPending, variables } = useMutation({
    mutationFn: (capability: string) =>
      runChannelProbe(props.channel.id, capability),
    onSettled: async () => {
      await Promise.all([
        query.refetch(),
        client.invalidateQueries({ queryKey: channelKeys.all }),
      ])
    },
  })
  const rows: ProbeRow[] = useMemo(
    () =>
      (query.data?.capabilities ?? []).map((capability) => ({
        capability,
        result: query.data?.results.find(
          (result) => result.capability === capability
        ),
      })),
    [query.data]
  )
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const featureLabels = useMemo(() => getChannelProbeLabels(t), [t])
  const reasons = useMemo(
    () =>
      ({
        task_pending: t(
          'The Provider task did not finish within the probe budget'
        ),
        limit_sample_only: t(
          'Only a smaller sample was tested; the declared limit is not verified'
        ),
        probe_interrupted: t('The probe was interrupted'),
        verified: t('Capability behavior observed'),
        behavior_not_observed: t('Capability behavior was not observed'),
        protocol_unavailable: t('The Provider does not support this protocol'),
        request_not_representable: t(
          'The probe cannot be represented in this protocol'
        ),
        credential_unavailable: t(
          'No enabled Provider credential is available'
        ),
        request_invalid: t('The probe request is invalid'),
        request_timeout: t('The probe timed out'),
        transport_failed: t('The upstream connection failed'),
        response_invalid: t('The upstream response is invalid'),
        response_interrupted: t('The upstream response was interrupted'),
        response_too_large: t('The upstream response exceeded the probe limit'),
        upstream_rejected: t('The upstream rejected the probe'),
      }) as Record<string, string>,
    [t]
  )
  const columns = useMemo<ColumnDef<ProbeRow>[]>(
    () => [
      {
        accessorKey: 'capability',
        header: t('Capability'),
        meta: { mobileTitle: true },
        cell: ({ row }) =>
          featureLabels.get(row.original.capability) ?? row.original.capability,
      },
      {
        id: 'result',
        header: t('Result'),
        cell: ({ row }) => {
          const status = row.original.result?.status
          let label = t('Not tested')
          if (status === 'passed') label = t('Success')
          if (status === 'failed') label = t('Failed')
          if (status === 'inconclusive') label = t('Inconclusive')
          if (status === 'running') label = t('Testing...')
          return (
            <StatusBadge
              label={label}
              variant={status === 'passed' ? 'success' : 'neutral'}
              copyable={false}
            />
          )
        },
      },
      {
        id: 'time',
        header: t('Time'),
        cell: ({ row }) =>
          row.original.result?.finished_at
            ? new Date(row.original.result.finished_at * 1000).toLocaleString(
                locale
              )
            : '-',
      },
      {
        id: 'reason',
        header: t('Details'),
        cell: ({ row }) => reasons[row.original.result?.reason ?? ''] ?? '-',
      },
      {
        id: 'tested_value',
        header: t('Tested value'),
        cell: ({ row }) =>
          row.original.result?.tested_value
            ? formatNumber(row.original.result.tested_value, locale)
            : '-',
      },
      {
        id: 'actions',
        header: t('Actions'),
        cell: ({ row }) =>
          props.canOperate && (
            <Button
              size='sm'
              variant='outline'
              aria-label={t('Probe {{capability}}', {
                capability:
                  featureLabels.get(row.original.capability) ??
                  row.original.capability,
              })}
              disabled={isPending}
              onClick={() => mutate(row.original.capability)}
            >
              {isPending && variables === row.original.capability
                ? t('Testing...')
                : t('Test')}
            </Button>
          ),
      },
    ],
    [
      t,
      locale,
      featureLabels,
      reasons,
      props.canOperate,
      isPending,
      variables,
      mutate,
    ]
  )
  const { table } = useDataTable({
    data: rows,
    columns,
    getRowId: (row) => row.capability,
  })
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open && !isPending) props.onClose()
      }}
      title={t('Capability probes')}
      description={t(
        'Results do not change capability declarations. Test requests may incur Provider costs.'
      )}
      contentClassName='sm:max-w-4xl'
      footer={
        <Button variant='outline' disabled={isPending} onClick={props.onClose}>
          {t('Close')}
        </Button>
      }
    >
      <p className='mb-3 text-sm break-all'>
        {props.channel.upstream_model_name} · {props.channel.protocol}
      </p>
      {props.channel.protocol.startsWith('task:') && (
        <p className='text-muted-foreground mb-3 text-sm'>
          {t('An unfinished Provider task may continue after the probe ends.')}
        </p>
      )}
      {query.isError ? (
        <ErrorState onRetry={() => void query.refetch()} />
      ) : (
        <DataTablePage
          table={table}
          columns={columns}
          fixedHeight={false}
          paginationInFooter={false}
          isLoading={query.isLoading}
        />
      )}
    </Dialog>
  )
}
