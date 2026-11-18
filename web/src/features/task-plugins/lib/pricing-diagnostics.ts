import { z } from 'zod'

import { useAuthStore } from '@/stores/auth-store'

import type {
  TaskPluginPricingDiagnostics,
  TaskPluginPricingIssue,
} from '../types'

const issueSchema = z
  .object({
    owner_type: z.enum(['model_offering', 'channel']),
    owner_id: z.number().int().positive().safe(),
    owner_name: z.string(),
    price_version_id: z.number().int().positive().safe().nullable(),
    plugin_key: z.string(),
    plugin_name: z.string(),
    reason: z.enum(['unconfigured', 'incompatible', 'stale']),
  })
  .strict()

const diagnosticsSchema = z
  .object({
    state: z.enum(['ready', 'unavailable', 'not_authorized']),
    issues: z.array(issueSchema),
  })
  .strict()
  .refine((value) => value.state === 'ready' || value.issues.length === 0)

export const PRICING_DIAGNOSTIC_REASONS: Record<
  TaskPluginPricingIssue['reason'],
  string
> = {
  unconfigured: 'No price set',
  incompatible:
    'The current PriceVersion is incompatible with the plugin usage schema.',
  stale: "The price owner's plugin or model binding is no longer valid.",
}

/** A missing or malformed diagnostic never reverses an already committed mutation. */
export function readPricingDiagnostics(
  value: unknown
): TaskPluginPricingDiagnostics {
  const parsed = diagnosticsSchema.safeParse(value)
  return parsed.success ? parsed.data : { state: 'unavailable', issues: [] }
}

function scopeOf(
  auth: ReturnType<typeof useAuthStore.getState>['auth']
): string {
  return JSON.stringify([
    auth.user?.id ?? null,
    auth.session?.sid ?? null,
    auth.user?.role ?? null,
    auth.user?.status ?? null,
    auth.user?.permissions?.admin_permissions ?? {},
  ])
}

export function taskPluginScope(): string {
  return scopeOf(useAuthStore.getState().auth)
}

export function useTaskPluginScope(): string {
  return useAuthStore((state) => scopeOf(state.auth))
}

export function isTaskPluginScopeCurrent(scope: string): boolean {
  return taskPluginScope() === scope
}

export function assertTaskPluginScope(scope: string, signal?: AbortSignal) {
  if (signal?.aborted || !isTaskPluginScopeCurrent(scope)) {
    throw new DOMException('The active account changed.', 'AbortError')
  }
}

export function taskPluginDetailKey(key: string, version?: string) {
  return ['task-plugin', key, taskPluginScope(), version ?? 'current'] as const
}

export function taskPluginVersionsKey(key: string) {
  return ['task-plugin-versions', key, taskPluginScope()] as const
}
