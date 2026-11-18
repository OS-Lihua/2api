import { z } from 'zod'

import { moneyTotalSchema } from '@/lib/wallet-summary'

const count = z.string().regex(/^(0|[1-9]\d*)$/)
const quantity = z.string().regex(/^(0|[1-9]\d*)(\.\d+)?$/)
export const financeMetricSchema = z
  .object({
    known_amount: moneyTotalSchema,
    total_amount: moneyTotalSchema.nullable(),
    pending_count: count,
  })
  .refine(
    (value) =>
      BigInt(value.pending_count) > 0n === (value.total_amount === null)
  )
  .refine(
    (value) =>
      value.total_amount === null ||
      (value.total_amount.currency === value.known_amount.currency &&
        value.total_amount.nanos === value.known_amount.nanos)
  )
const usage = z
  .object({
    known_total: quantity,
    total: quantity.nullable(),
    missing_count: count,
    estimated_count: count,
  })
  .refine(
    (value) => BigInt(value.missing_count) > 0n === (value.total === null)
  )
  .refine((value) => value.total === null || value.total === value.known_total)
const procurement = z.object({
  cost: financeMetricSchema,
  original_costs: z.array(financeMetricSchema),
  purchase_pending_count: count,
  fx_pending_count: count,
})
const totals = z.object({
  request_count: count,
  in_flight_count: count,
  succeeded_count: count,
  failed_count: count,
  other_terminal_count: count,
  funding_pending_count: count.optional(),
  revenue: financeMetricSchema,
  collected: financeMetricSchema,
  outstanding: financeMetricSchema,
  usage: z.record(z.string(), usage),
  procurement: procurement.optional(),
  margin: financeMetricSchema.optional(),
})
const time = z.number().int().safe()
export const dimensionSchema = z.enum([
  'user',
  'token',
  'model_offering',
  'channel',
  'provider',
])
const row = totals.extend({
  bucket_start: time,
  bucket_end: time,
  dimensions: z.record(z.string(), z.number().int().nonnegative().safe()),
  names: z.record(z.string(), z.string()),
})
export const financeReportSchema = z
  .object({
    from: time,
    until: time,
    bucket: z.enum(['hour', 'day', 'week']),
    timezone: z.string(),
    group_by: z.array(dimensionSchema),
    time_basis: z.literal('request_started_at'),
    coverage: z.literal('primary_gateway_only'),
    revenue_attribution: z.literal('last_attempt'),
    cost_attribution: z.literal('all_attempts').optional(),
    pending_count_basis: z.literal('distinct_requests'),
    unattributed_adjustments: z.literal('excluded'),
    buckets: z.array(z.object({ start: time, end: time })),
    totals,
    items: z.array(row),
  })
  .superRefine((report, context) => {
    const boundaries = new Map(
      report.buckets.map((bucket) => [bucket.start, bucket.end])
    )
    const first = report.buckets.at(0)
    const last = report.buckets.at(-1)
    const currency = report.totals.revenue.known_amount.currency
    const invalidCurrency = [report.totals, ...report.items].some((item) => {
      const amounts = [item.revenue, item.collected, item.outstanding]
      if (item.procurement) amounts.push(item.procurement.cost)
      if (item.margin) amounts.push(item.margin)
      return amounts.some((metric) => metric.known_amount.currency !== currency)
    })
    if (
      !first ||
      !last ||
      first.start > report.from ||
      first.end <= report.from ||
      last.start >= report.until ||
      last.end < report.until ||
      new Set(report.group_by).size !== report.group_by.length ||
      invalidCurrency ||
      boundaries.size !== report.buckets.length ||
      report.buckets.some(
        (bucket, index) =>
          bucket.start >= bucket.end ||
          (index > 0 && report.buckets[index - 1].end !== bucket.start)
      ) ||
      report.items.some(
        (item) =>
          boundaries.get(item.bucket_start) !== item.bucket_end ||
          report.group_by.some(
            (dimension) => item.dimensions[dimension] === undefined
          )
      )
    ) {
      context.addIssue({
        code: 'custom',
        message: 'Invalid report buckets or dimensions',
      })
    }
  })
export type FinanceReport = z.infer<typeof financeReportSchema>
export type FinanceMetric = z.infer<typeof financeMetricSchema>
export type FinanceTotals = z.infer<typeof totals>
export type FinanceRow = z.infer<typeof row>
export type FinanceDimension = z.infer<typeof dimensionSchema>
export const financeDimensionLabels: Record<FinanceDimension, string> = {
  user: 'User',
  token: 'API Key',
  model_offering: 'Model Offering',
  channel: 'Channel',
  provider: 'Provider',
}
export type FinanceFilter = {
  from: string
  until: string
  bucket: 'hour' | 'day' | 'week'
  timezone: string
  group_by?: string
  user_id?: string
  token_id?: string
  model_offering_id?: string
  channel_id?: string
  provider_id?: string
}
export function defaultFinanceFilter(): FinanceFilter {
  const until = new Date()
  return {
    from: new Date(until.getTime() - 7 * 86400000).toISOString(),
    until: until.toISOString(),
    bucket: 'day',
    timezone: 'UTC',
  }
}
export function validFinanceFilter(
  filter: FinanceFilter,
  admin: boolean
): boolean {
  const from = Date.parse(filter.from),
    until = Date.parse(filter.until)
  if (
    !Number.isFinite(from) ||
    !Number.isFinite(until) ||
    from >= until ||
    until - from > 31 * 86400000 ||
    !['hour', 'day', 'week'].includes(filter.bucket)
  ) {
    return false
  }
  try {
    new Intl.DateTimeFormat('en', { timeZone: filter.timezone }).format()
  } catch {
    return false
  }
  for (const key of [
    'user_id',
    'token_id',
    'model_offering_id',
    'channel_id',
    'provider_id',
  ] as const) {
    if (
      filter[key] !== undefined &&
      (!/^[1-9]\d*$/.test(filter[key]) ||
        (!admin && ['user_id', 'channel_id', 'provider_id'].includes(key)))
    ) {
      return false
    }
  }
  return true
}
// Only this bounded, non-authoritative chart geometry crosses into Number.
export function normalizedGeometry(value: bigint, maximum: bigint): number {
  return maximum === 0n ? 0 : Number((value * 1000000n) / maximum) / 1000000
}
export function exactQuantity(value: string, locale?: string): string {
  const [whole, fraction] = value.split('.')
  const formatted = new Intl.NumberFormat(locale).format(BigInt(whole))
  if (!fraction) return formatted
  const separator =
    new Intl.NumberFormat(locale)
      .formatToParts(1.1)
      .find((part) => part.type === 'decimal')?.value ?? '.'
  return `${formatted}${separator}${fraction}`
}
export function rowIdentity(
  row: FinanceRow,
  dimensions: FinanceDimension[]
): string {
  return `${row.bucket_start}:${dimensions.map((dimension) => `${dimension}:${row.dimensions[dimension]}`).join('|')}`
}
export function bucketLabel(
  start: number,
  timezone: string,
  locale?: string
): string {
  return new Intl.DateTimeFormat(locale, {
    timeZone: timezone,
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    timeZoneName: 'shortOffset',
    hourCycle: 'h23',
  }).format(start)
}
