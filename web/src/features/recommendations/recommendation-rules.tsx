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
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ConfirmDialog } from '@/components/confirm-dialog'
import {
  DataTablePage,
  StaticRowActions,
  useDataTable,
} from '@/components/data-table'
import {
  sideDrawerContentClassName,
  sideDrawerHeaderClassName,
} from '@/components/drawer-layout'
import { ErrorState } from '@/components/error-state'
import { Button } from '@/components/ui/button'
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
  SheetDescription,
} from '@/components/ui/sheet'
import { toIntlLocale } from '@/i18n/languages'
import { hasPermission } from '@/lib/admin-permissions'
import { formatNumber } from '@/lib/format'
import { useAuthStore } from '@/stores/auth-store'

import {
  deleteRecommendationRule,
  listRecommendationRules,
  type RecommendationKind,
  type RecommendationRule,
} from './api'
import { RecommendationRuleEditor } from './recommendation-rule-editor'

export function RecommendationRules(props: { kind: RecommendationKind }) {
  const { t, i18n } = useTranslation()
  const user = useAuthStore((state) => state.auth.user)
  const canRead = hasPermission(user, 'recommendation', 'read')
  const canWrite = hasPermission(user, 'recommendation', 'write')
  const [open, setOpen] = useState(false)
  const [editing, setEditing] = useState<RecommendationRule | null>(null)
  const [deleting, setDeleting] = useState<RecommendationRule | null>(null)
  const [pagination, setPagination] = useState({ pageIndex: 0, pageSize: 20 })
  const client = useQueryClient()
  const query = useQuery({
    queryKey: [
      'recommendation-rules',
      props.kind,
      pagination.pageIndex,
      pagination.pageSize,
    ],
    queryFn: () =>
      listRecommendationRules(
        props.kind,
        pagination.pageIndex + 1,
        pagination.pageSize
      ),
    enabled: open && canRead,
  })
  const remove = useMutation({
    mutationFn: deleteRecommendationRule,
    onSuccess: async () => {
      setDeleting(null)
      await Promise.all([
        client.invalidateQueries({ queryKey: ['recommendation-rules'] }),
        client.invalidateQueries({ queryKey: ['recommendations'] }),
      ])
    },
  })
  const columns: ColumnDef<RecommendationRule>[] = [
    { accessorKey: 'pattern', header: t('Match pattern') },
    {
      id: 'target',
      header: t(props.kind === 'provider' ? 'Name' : 'Publisher'),
      cell: ({ row }) =>
        row.original.provider_name ||
        row.original.publisher_name ||
        formatNumber(
          row.original.publisher_id,
          toIntlLocale(i18n.resolvedLanguage || i18n.language)
        ),
    },
    {
      accessorKey: 'priority',
      header: t('Priority'),
      cell: ({ row }) =>
        formatNumber(
          row.original.priority,
          toIntlLocale(i18n.resolvedLanguage || i18n.language)
        ),
    },
    {
      accessorKey: 'enabled',
      header: t('Status'),
      cell: ({ row }) => t(row.original.enabled ? 'Enabled' : 'Disabled'),
    },
    {
      id: 'actions',
      cell: ({ row }) => (
        <StaticRowActions
          editLabel={t('Edit')}
          deleteLabel={t('Delete')}
          menuLabel={t('Open menu')}
          editDisabled={!canWrite}
          deleteDisabled={!canWrite}
          onEdit={() => setEditing(row.original)}
          onDelete={() => setDeleting(row.original)}
        />
      ),
    },
  ]
  const { table } = useDataTable({
    data: query.data?.items ?? [],
    columns,
    pagination,
    onPaginationChange: setPagination,
    manualPagination: true,
    totalCount: query.data?.total ?? 0,
  })
  if (!canRead) return null
  return (
    <>
      <Button size='sm' variant='outline' onClick={() => setOpen(true)}>
        {t('Recommendation rules')}
      </Button>
      <Sheet open={open} onOpenChange={setOpen}>
        <SheetContent
          className={sideDrawerContentClassName('sm:max-w-[1000px]')}
        >
          <SheetHeader className={sideDrawerHeaderClassName()}>
            <SheetTitle>{t('Recommendation rules')}</SheetTitle>
            <SheetDescription>
              {t(
                'Built-in rules are editable. Changes and deletions survive restarts.'
              )}
            </SheetDescription>
          </SheetHeader>
          <div className='flex min-h-0 flex-1 flex-col gap-4 p-4'>
            <Button
              disabled={!canWrite}
              onClick={() =>
                setEditing({
                  id: 0,
                  kind: props.kind,
                  pattern: '',
                  priority: 0,
                  enabled: true,
                  provider_name: '',
                  provider_type: 1,
                  publisher_id: 0,
                  publisher_name: '',
                })
              }
            >
              {t('Add rule')}
            </Button>
            {query.isError ? (
              <ErrorState
                description={t('Failed to load')}
                onRetry={() => void query.refetch()}
              />
            ) : (
              <DataTablePage
                table={table}
                columns={columns}
                isLoading={query.isLoading}
                isFetching={query.isFetching}
                emptyTitle={t('No recommendation rules')}
                paginationInFooter={false}
                toolbar={false}
              />
            )}
          </div>
        </SheetContent>
      </Sheet>
      {editing && (
        <RecommendationRuleEditor
          key={`${editing.id}:${props.kind}`}
          rule={editing}
          onClose={() => setEditing(null)}
        />
      )}
      <ConfirmDialog
        open={deleting !== null}
        onOpenChange={(value) => {
          if (!value) setDeleting(null)
        }}
        title={t('Delete rule?')}
        desc={deleting?.pattern ?? ''}
        destructive
        isLoading={remove.isPending}
        confirmText={t('Delete')}
        handleConfirm={() => {
          if (deleting) remove.mutate(deleting.id)
        }}
      />
    </>
  )
}
