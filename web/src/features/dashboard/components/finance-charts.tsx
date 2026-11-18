import { useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { Bar, BarChart, CartesianGrid, Sankey, XAxis, YAxis } from 'recharts'

import { StaticDataTable } from '@/components/data-table/static/static-data-table'
import { EmptyState } from '@/components/empty-state'
import { Button } from '@/components/ui/button'
import { ChartContainer, ChartTooltip } from '@/components/ui/chart'
import { toIntlLocale } from '@/i18n/languages'
import { formatMoneyAmount } from '@/lib/currency'

import {
  bucketLabel,
  exactQuantity,
  financeDimensionLabels,
  normalizedGeometry,
  type FinanceReport,
  type FinanceDimension,
} from '../lib/finance'

function FinanceChartTooltip({
  anchor,
  coordinate,
  children,
}: {
  anchor: HTMLDivElement | null
  coordinate?: { x: number; y: number }
  children: ReactNode
}) {
  const content = useRef<HTMLDivElement>(null)
  const [position, setPosition] = useState({ left: 16, top: 16 })
  useLayoutEffect(() => {
    const update = () => {
      const chart = anchor?.getBoundingClientRect()
      const tooltip = content.current?.getBoundingClientRect()
      if (!chart || !tooltip) return
      // Only screen coordinates use numbers. The displayed money stays exact.
      const next = {
        left: Math.max(
          16,
          Math.min(
            chart.left + (coordinate?.x ?? 0) + 12,
            window.innerWidth - tooltip.width - 16
          )
        ),
        top: Math.max(
          16,
          Math.min(
            chart.top + (coordinate?.y ?? 0) + 12,
            window.innerHeight - tooltip.height - 16
          )
        ),
      }
      setPosition((previous) =>
        previous.left === next.left && previous.top === next.top
          ? previous
          : next
      )
    }
    update()
    const observer = new ResizeObserver(update)
    if (content.current) observer.observe(content.current)
    window.addEventListener('scroll', update, true)
    window.addEventListener('resize', update)
    return () => {
      observer.disconnect()
      window.removeEventListener('scroll', update, true)
      window.removeEventListener('resize', update)
    }
  }, [anchor, coordinate?.x, coordinate?.y])
  return (
    <div
      ref={content}
      role='tooltip'
      className='bg-background fixed z-50 rounded border p-2 text-sm [overflow-wrap:anywhere]'
      style={{ ...position, width: 320, maxWidth: 'calc(100vw - 2rem)' }}
    >
      {children}
    </div>
  )
}

export function FinanceCharts({
  report,
  flow = false,
  onFilter,
}: {
  report: FinanceReport
  flow?: boolean
  onFilter?: (dimension: FinanceDimension, id: number) => void
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const [metric, setMetric] = useState<'requests' | 'revenue'>('requests')
  const [view, setView] = useState<'time' | 'distribution'>('time')
  const [hidden, setHidden] = useState(false)
  const chart = useRef<HTMLDivElement>(null)
  const currency = report.totals.revenue.known_amount.currency
  const amount = (value: bigint) =>
    metric === 'requests'
      ? exactQuantity(value.toString(), locale)
      : formatMoneyAmount({ nanos: value.toString(), currency }, locale)
  const source = new Map<
    string,
    { label: string; axisLabel: string; exact: bigint; pending: boolean }
  >()
  if (view === 'time') {
    for (const bucket of report.buckets) {
      source.set(String(bucket.start), {
        label: bucketLabel(bucket.start, report.timezone, locale),
        axisLabel: bucketLabel(bucket.start, report.timezone, locale),
        exact: 0n,
        pending: false,
      })
    }
  }
  for (const row of report.items) {
    const key =
      view === 'time'
        ? String(row.bucket_start)
        : report.group_by
            .map((dimension) => `${dimension}:${row.dimensions[dimension]}`)
            .join('|')
    const label =
      view === 'time'
        ? bucketLabel(row.bucket_start, report.timezone, locale)
        : report.group_by
            .map(
              (dimension) =>
                `${row.names[dimension] || t(financeDimensionLabels[dimension])} (#${row.dimensions[dimension]})`
            )
            .join(' → ') || t('All requests')
    const axisLabel =
      view === 'time'
        ? label
        : report.group_by
            .map((dimension) => `#${row.dimensions[dimension]}`)
            .join(' → ') || t('All requests')
    const item = source.get(key) ?? {
      label,
      axisLabel,
      exact: 0n,
      pending: false,
    }
    item.exact +=
      metric === 'requests'
        ? BigInt(row.request_count)
        : BigInt(row.revenue.known_amount.nanos)
    item.pending ||= metric === 'revenue' && row.revenue.total_amount === null
    source.set(key, item)
  }
  const values = [...source.values()]
  const maximum = values.reduce((max, item) => {
    const absolute = item.exact < 0n ? -item.exact : item.exact
    return absolute > max ? absolute : max
  }, 0n)
  const points = values.map((item) => ({
    ...item,
    geometry: normalizedGeometry(item.exact, maximum),
  }))
  const ticks = [
    ...new Set([-maximum, -maximum / 2n, 0n, maximum / 2n, maximum]),
  ].map((exact) => ({ exact, geometry: normalizedGeometry(exact, maximum) }))
  // Layout uses label length only; financial values remain exact strings/BigInts.
  const minimumChartWidth =
    320 + Math.max(...ticks.map((tick) => amount(tick.exact).length)) * 10
  const nodes: {
    name: string
    exact: bigint
    label: string
    dimension: FinanceDimension
    id: number
  }[] = []
  const nodeIndex = new Map<string, number>()
  const links = new Map<
    string,
    { source: number; target: number; exact: bigint }
  >()
  if (flow) {
    for (const row of report.items) {
      const count = BigInt(row.request_count)
      if (count === 0n) continue
      const path = report.group_by.map((dimension) => {
        const identity = `${dimension}:${row.dimensions[dimension]}`
        let index = nodeIndex.get(identity)
        if (index === undefined) {
          index = nodes.length
          nodeIndex.set(identity, index)
          nodes.push({
            name: identity,
            label: `${t(financeDimensionLabels[dimension])}: ${row.names[dimension] || t(financeDimensionLabels[dimension])} (#${row.dimensions[dimension]})`,
            exact: 0n,
            dimension,
            id: row.dimensions[dimension],
          })
        }
        nodes[index].exact += count
        return index
      })
      for (let index = 1; index < path.length; index++) {
        const key = `${path[index - 1]}:${path[index]}`
        const link = links.get(key) ?? {
          source: path[index - 1],
          target: path[index],
          exact: 0n,
        }
        link.exact += count
        links.set(key, link)
      }
    }
  }
  const flowMax = nodes.reduce(
    (max, node) => (node.exact > max ? node.exact : max),
    0n
  )
  return (
    <section
      className='min-w-0 space-y-3 rounded-lg border p-3'
      aria-label={t('Request analytics')}
    >
      <div className='flex flex-wrap gap-2'>
        <Button
          size='sm'
          variant={metric === 'requests' ? 'default' : 'outline'}
          aria-pressed={metric === 'requests'}
          onClick={() => setMetric('requests')}
        >
          {t('Requests')}
        </Button>
        <Button
          size='sm'
          variant={metric === 'revenue' ? 'default' : 'outline'}
          aria-pressed={metric === 'revenue'}
          onClick={() => setMetric('revenue')}
        >
          {t('Known revenue')}
        </Button>
        <Button
          size='sm'
          variant={view === 'time' ? 'default' : 'outline'}
          aria-pressed={view === 'time'}
          onClick={() => setView('time')}
        >
          {t('Trend')}
        </Button>
        <Button
          size='sm'
          variant={view === 'distribution' ? 'default' : 'outline'}
          aria-pressed={view === 'distribution'}
          onClick={() => setView('distribution')}
        >
          {t('Distribution')}
        </Button>
        <Button size='sm' variant='ghost' onClick={() => setHidden(!hidden)}>
          {hidden ? t('Show chart') : t('Hide chart')}
        </Button>
      </div>
      <p className='text-muted-foreground text-sm'>
        {t(
          'Chart positions are relative. Exact signed values and pending states are preserved in the details.'
        )}
      </p>
      {!hidden &&
        (report.items.length === 0 ? (
          <EmptyState title={t('No requests in this range')} />
        ) : (
          <>
            <p className='text-muted-foreground text-xs'>
              {t('Scroll horizontally to view the complete chart.')}
            </p>
            <div
              role='region'
              aria-label={t('Bar Chart')}
              tabIndex={0}
              className='focus-visible:ring-ring max-w-full overflow-x-auto rounded-md focus-visible:ring-2'
            >
              <ChartContainer
                ref={chart}
                className='h-72 w-full'
                style={{ minWidth: minimumChartWidth }}
                config={{
                  geometry: {
                    label:
                      metric === 'requests'
                        ? t('Requests')
                        : t('Known revenue'),
                    color: 'var(--chart-1)',
                  },
                }}
              >
                <BarChart
                  data={points}
                  accessibilityLayer
                  margin={{ left: 8, right: 8, top: 16, bottom: 35 }}
                >
                  <CartesianGrid vertical={false} />
                  <XAxis
                    dataKey='axisLabel'
                    tickLine={false}
                    tick={{ fontSize: 10 }}
                    interval='preserveStartEnd'
                  />
                  <YAxis
                    ticks={ticks.map((tick) => tick.geometry)}
                    tickFormatter={(value: number) => {
                      const tick = ticks.find((item) => item.geometry === value)
                      return tick ? amount(tick.exact) : ''
                    }}
                    width='auto'
                    tick={{ fontSize: 10 }}
                  />
                  <ChartTooltip
                    portal={
                      typeof document === 'undefined'
                        ? undefined
                        : document.body
                    }
                    wrapperStyle={{ pointerEvents: 'none' }}
                    isAnimationActive={false}
                    content={({ active, payload, coordinate }) => {
                      const point = points.find(
                        (item) => item.label === payload?.[0]?.payload?.label
                      )
                      return active && point ? (
                        <FinanceChartTooltip
                          anchor={chart.current}
                          coordinate={coordinate}
                        >
                          {point.label}
                          <br />
                          {amount(point.exact)}
                          {point.pending && (
                            <p>
                              {t('Known amount only')} ·{' '}
                              {t('Total unavailable')}
                            </p>
                          )}
                        </FinanceChartTooltip>
                      ) : null
                    }}
                  />
                  <Bar
                    dataKey='geometry'
                    fill='var(--color-geometry)'
                    isAnimationActive={false}
                  />
                </BarChart>
              </ChartContainer>
            </div>
            {flow && links.size > 0 && (
              <>
                <p className='text-sm'>
                  {t(
                    'Flow widths represent nonnegative request counts, not money.'
                  )}
                </p>
                <div className='max-w-full overflow-x-auto'>
                  <Sankey
                    width={Math.max(600, report.group_by.length * 180)}
                    height={320}
                    data={{
                      nodes: nodes.map((node) => ({
                        name: hidden ? node.name : node.label,
                      })),
                      links: [...links.values()].map((link) => ({
                        ...link,
                        // A positive flow must retain finite drawable geometry even
                        // when its exact count is far below the largest node.
                        value: Math.max(
                          Number.EPSILON,
                          normalizedGeometry(link.exact, flowMax)
                        ),
                      })),
                    }}
                    nodePadding={20}
                    margin={{ left: 10, right: 10, top: 10, bottom: 10 }}
                  />
                </div>
                <ul className='grid min-w-0 gap-1 text-sm sm:grid-cols-2'>
                  {nodes.map((node) => (
                    <li key={node.name} className='[overflow-wrap:anywhere]'>
                      <Button
                        variant='ghost'
                        size='sm'
                        className='h-auto max-w-full text-left [overflow-wrap:anywhere] whitespace-normal'
                        disabled={!onFilter || node.id === 0}
                        onClick={() => onFilter?.(node.dimension, node.id)}
                      >
                        {node.label}:{' '}
                        {exactQuantity(node.exact.toString(), locale)}
                      </Button>
                    </li>
                  ))}
                </ul>
                <StaticDataTable
                  tableProps={{ 'aria-label': t('Flow details') }}
                  data={[...links.values()]}
                  getRowKey={(link) => `${link.source}:${link.target}`}
                  columns={[
                    {
                      id: 'from',
                      header: t('From'),
                      cell: (link) => (
                        <span className='[overflow-wrap:anywhere] whitespace-normal'>
                          {nodes[link.source].label}
                        </span>
                      ),
                    },
                    {
                      id: 'to',
                      header: t('To'),
                      cell: (link) => (
                        <span className='[overflow-wrap:anywhere] whitespace-normal'>
                          {nodes[link.target].label}
                        </span>
                      ),
                    },
                    {
                      id: 'count',
                      header: t('Requests'),
                      cell: (link) => (
                        <span className='[overflow-wrap:anywhere] whitespace-normal'>
                          {exactQuantity(link.exact.toString(), locale)}
                        </span>
                      ),
                    },
                  ]}
                />
              </>
            )}
          </>
        ))}
    </section>
  )
}
