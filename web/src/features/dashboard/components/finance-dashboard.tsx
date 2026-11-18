import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { StaticDataTable } from '@/components/data-table/static/static-data-table'
import { DateTimePicker } from '@/components/datetime-picker'
import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'
import { toIntlLocale } from '@/i18n/languages'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

import { useFinanceReport } from '../hooks/use-finance-report'
import {
  bucketLabel,
  defaultFinanceFilter,
  exactQuantity,
  financeDimensionLabels,
  rowIdentity,
  validFinanceFilter,
  type FinanceDimension,
  type FinanceFilter,
  type FinanceRow,
} from '../lib/finance'
import { FinanceCharts } from './finance-charts'
import { FinanceAmount, FinanceTotals } from './finance-totals'

export type FinanceView = 'models' | 'users' | 'flow'
export function FinanceDashboard({ view = 'models' }: { view?: FinanceView }) {
  const user = useAuthStore((state) => state.auth.user)
  const sid = useAuthStore((state) => state.auth.session?.sid)
  const admin = (user?.role ?? 0) >= ROLE.ADMIN
  return (
    <FinanceContent
      key={`${user?.id}:${sid}:${admin}:${view}`}
      admin={admin}
      view={view}
    />
  )
}
function FinanceContent({
  admin,
  view,
}: {
  admin: boolean
  view: FinanceView
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const dimensions: FinanceDimension[] = admin
    ? ['user', 'token', 'model_offering', 'channel', 'provider']
    : ['token', 'model_offering']
  let groupBy = 'model_offering'
  if (view === 'flow') groupBy = dimensions.join(',')
  else if (view === 'users' && admin) groupBy = 'user'
  const [filter, setFilter] = useState<FinanceFilter>(() => ({
    ...defaultFinanceFilter(),
    group_by: groupBy,
  }))
  const [draft, setDraft] = useState(filter)
  const [page, setPage] = useState(0)
  const [selected, setSelected] = useState<FinanceRow | null>(null)
  const report = useFinanceReport(admin, filter)
  const valid = validFinanceFilter(draft, admin)
  if (!report.allowed) {
    return (
      <EmptyState
        title={t('Financial access required')}
        description={t(
          'Wallet or ledger read permission is required. No financial data has been requested.'
        )}
      />
    )
  }
  const rows = report.data?.items ?? []
  const pageSize = 20
  return (
    <div className='min-w-0 space-y-4'>
      <form
        className='grid min-w-0 grid-cols-1 items-end gap-3 rounded-lg border p-3 sm:grid-cols-2 xl:grid-cols-4'
        onSubmit={(event) => {
          event.preventDefault()
          if (valid) {
            setFilter(draft)
            setPage(0)
            setSelected(null)
          }
        }}
      >
        <div className='min-w-0 space-y-1'>
          <Label htmlFor='finance-from'>{t('From')}</Label>
          <DateTimePicker
            id='finance-from'
            value={draft.from ? new Date(draft.from) : undefined}
            onChange={(value) =>
              setDraft({ ...draft, from: value?.toISOString() ?? '' })
            }
          />
        </div>
        <div className='min-w-0 space-y-1'>
          <Label htmlFor='finance-until'>{t('Until (exclusive)')}</Label>
          <DateTimePicker
            id='finance-until'
            value={draft.until ? new Date(draft.until) : undefined}
            onChange={(value) =>
              setDraft({ ...draft, until: value?.toISOString() ?? '' })
            }
          />
        </div>
        <div className='min-w-0 space-y-1'>
          <Label htmlFor='finance-timezone'>{t('Report timezone')}</Label>
          <Input
            id='finance-timezone'
            value={draft.timezone}
            onChange={(event) =>
              setDraft({ ...draft, timezone: event.target.value })
            }
          />
        </div>
        <div className='min-w-0 space-y-1'>
          <Label htmlFor='finance-bucket'>{t('Granularity')}</Label>
          <Select
            value={draft.bucket}
            onValueChange={(value) => {
              if (value === 'hour' || value === 'day' || value === 'week') {
                setDraft({ ...draft, bucket: value })
              }
            }}
          >
            <SelectTrigger id='finance-bucket'>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {(['hour', 'day', 'week'] as const).map((value) => (
                <SelectItem key={value} value={value}>
                  {t({ hour: 'Hour', day: 'Day', week: 'Week' }[value])}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        {dimensions.map((dimension) => {
          const key = `${dimension}_id` as keyof Pick<
            FinanceFilter,
            | 'user_id'
            | 'token_id'
            | 'model_offering_id'
            | 'channel_id'
            | 'provider_id'
          >
          return (
            <div key={dimension} className='min-w-0 space-y-1'>
              <Label htmlFor={`finance-${dimension}`}>
                {t('Filter by stable ID')}:{' '}
                {t(financeDimensionLabels[dimension])}
              </Label>
              <Input
                id={`finance-${dimension}`}
                inputMode='numeric'
                value={draft[key] ?? ''}
                placeholder={t('All')}
                onChange={(event) =>
                  setDraft({ ...draft, [key]: event.target.value || undefined })
                }
              />
            </div>
          )
        })}
        <Button type='submit' disabled={!valid || report.isFetching}>
          {t('Apply filters')}
        </Button>
        <Button
          type='button'
          variant='outline'
          onClick={() => {
            const reset = { ...defaultFinanceFilter(), group_by: groupBy }
            setDraft(reset)
            setFilter(reset)
            setPage(0)
            setSelected(null)
          }}
        >
          {t('Reset')}
        </Button>
        <p className='text-muted-foreground text-sm sm:col-span-2 xl:col-span-4'>
          {t(
            'Choose at most 31 days. The start is inclusive and the end is exclusive. Date controls use your browser timezone; report buckets use the selected IANA timezone.'
          )}
        </p>
        {!valid && (
          <p
            role='alert'
            className='text-destructive sm:col-span-2 xl:col-span-4'
          >
            {t('Invalid parameters')}
          </p>
        )}
      </form>
      {report.isError && (
        <ErrorState
          description={t('Failed to load')}
          onRetry={() => void report.refetch()}
        />
      )}
      {report.isPending && <LoadingState />}
      {!report.isError && !report.isPending && report.data && (
        <>
          <p className='text-muted-foreground text-sm [overflow-wrap:anywhere]'>
            {t(
              'Primary gateway requests only, grouped by request start time. Legacy quota records and unattributed adjustments are excluded.'
            )}
            <br />
            {filter.from} ≤ {t('Request start')} &lt; {filter.until}
          </p>
          <FinanceTotals totals={report.data.totals} admin={admin} />
          <FinanceCharts
            report={report.data}
            flow={view === 'flow'}
            onFilter={(dimension, id) => {
              const next = { ...filter, [`${dimension}_id`]: String(id) }
              setDraft(next)
              setFilter(next)
              setPage(0)
              setSelected(null)
            }}
          />
          <StaticDataTable
            tableProps={{ 'aria-label': t('Financial report details') }}
            data={rows.slice(page * pageSize, (page + 1) * pageSize)}
            getRowKey={(row) => rowIdentity(row, report.data?.group_by ?? [])}
            emptyContent={<EmptyState title={t('No requests in this range')} />}
            columns={[
              {
                id: 'bucket',
                header: t('Bucket'),
                cell: (row) => (
                  <span className='whitespace-normal'>
                    {bucketLabel(row.bucket_start, filter.timezone, locale)}
                    <br />
                    {new Date(row.bucket_start).toISOString()} →{' '}
                    {new Date(row.bucket_end).toISOString()}
                  </span>
                ),
              },
              {
                id: 'dimensions',
                header: t('Dimensions'),
                cell: (row) => (
                  <div className='space-y-1 whitespace-normal'>
                    {report.data?.group_by.map((dimension) => (
                      <Button
                        key={dimension}
                        size='sm'
                        variant='ghost'
                        className='h-auto max-w-full text-left [overflow-wrap:anywhere] whitespace-normal'
                        disabled={!row.dimensions[dimension]}
                        onClick={() => {
                          const next = {
                            ...filter,
                            [`${dimension}_id`]: String(
                              row.dimensions[dimension]
                            ),
                          }
                          setDraft(next)
                          setFilter(next)
                          setPage(0)
                          setSelected(null)
                        }}
                      >
                        {row.names[dimension] ||
                          t(financeDimensionLabels[dimension])}{' '}
                        (#
                        {row.dimensions[dimension]})
                      </Button>
                    ))}
                  </div>
                ),
              },
              {
                id: 'count',
                header: t('Requests'),
                cell: (row) => (
                  <span className='[overflow-wrap:anywhere] whitespace-normal'>
                    {exactQuantity(row.request_count, locale)}
                  </span>
                ),
              },
              {
                id: 'revenue',
                header: t('Revenue'),
                cell: (row) => <FinanceAmount metric={row.revenue} />,
              },
              {
                id: 'detail',
                header: t('Details'),
                cell: (row) => (
                  <Button
                    variant='outline'
                    size='sm'
                    onClick={() => setSelected(row)}
                  >
                    {t('Details')}
                  </Button>
                ),
              },
            ]}
          />
          <div className='flex flex-wrap items-center justify-end gap-3'>
            <Button
              variant='outline'
              disabled={page === 0}
              onClick={() => setPage(page - 1)}
            >
              {t('Previous')}
            </Button>
            <span>
              {page + 1} / {Math.max(1, Math.ceil(rows.length / pageSize))}
            </span>
            <Button
              variant='outline'
              disabled={(page + 1) * pageSize >= rows.length}
              onClick={() => setPage(page + 1)}
            >
              {t('Next')}
            </Button>
          </div>
        </>
      )}
      <Sheet
        open={Boolean(selected && report.data)}
        onOpenChange={(open) => {
          if (!open) setSelected(null)
        }}
      >
        <SheetContent className='w-full overflow-y-auto sm:max-w-2xl'>
          <SheetHeader>
            <SheetTitle>{t('Financial report details')}</SheetTitle>
          </SheetHeader>
          {selected && report.data && (
            <div className='min-w-0 space-y-3 p-4'>
              <p className='[overflow-wrap:anywhere]'>
                {bucketLabel(selected.bucket_start, filter.timezone, locale)} →{' '}
                {bucketLabel(selected.bucket_end, filter.timezone, locale)}
              </p>
              {report.data.group_by.map((dimension) => (
                <p key={dimension} className='[overflow-wrap:anywhere]'>
                  {t(financeDimensionLabels[dimension])}:{' '}
                  {selected.names[dimension]} (#
                  {selected.dimensions[dimension]})
                </p>
              ))}
              <FinanceTotals totals={selected} admin={admin} />
            </div>
          )}
        </SheetContent>
      </Sheet>
    </div>
  )
}
