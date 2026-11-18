import type {
  FinanceFilter,
  FinanceMetric,
  FinanceReport,
  FinanceTotals,
} from '../lib/finance'

export const metric = (
  nanos = '0',
  pending = '0',
  currency = 'EUR'
): FinanceMetric => ({
  known_amount: { nanos, currency },
  total_amount: pending === '0' ? { nanos, currency } : null,
  pending_count: pending,
})
export function reportFixture(
  filter: FinanceFilter,
  admin = false
): FinanceReport {
  const from = Date.parse(filter.from),
    until = Date.parse(filter.until)
  const group_by = (filter.group_by?.split(',') ??
    []) as FinanceReport['group_by']
  const totals: FinanceTotals = {
    request_count: '2',
    in_flight_count: '0',
    succeeded_count: '2',
    failed_count: '0',
    other_terminal_count: '0',
    revenue: metric('922337203685477580800000001'),
    collected: metric('9007199254740993'),
    outstanding: metric('1'),
    usage: {
      total_tokens: {
        known_total: '9007199254740993',
        total: null,
        missing_count: '1',
        estimated_count: '1',
      },
    },
  }
  if (admin) {
    totals.procurement = {
      cost: metric('922337203685477580800000002', '1'),
      original_costs: [metric('123000000001', '1', 'JPY')],
      purchase_pending_count: '1',
      fx_pending_count: '0',
    }
    totals.margin = metric('-1', '1')
  }
  return {
    from,
    until,
    bucket: filter.bucket,
    timezone: filter.timezone,
    group_by,
    time_basis: 'request_started_at',
    coverage: 'primary_gateway_only',
    revenue_attribution: 'last_attempt',
    ...(admin ? { cost_attribution: 'all_attempts' as const } : {}),
    pending_count_basis: 'distinct_requests',
    unattributed_adjustments: 'excluded',
    buckets: [{ start: from, end: until }],
    totals,
    items: [7, 8].map((id) => ({
      ...totals,
      request_count: '1',
      revenue: metric(id === 7 ? '922337203685477580800000000' : '1'),
      bucket_start: from,
      bucket_end: until,
      dimensions: Object.fromEntries(
        group_by.map((dimension) => [dimension, id])
      ),
      names: Object.fromEntries(
        group_by.map((dimension) => [dimension, 'Same name'])
      ),
    })),
  }
}
