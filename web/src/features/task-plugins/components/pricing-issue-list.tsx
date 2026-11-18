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
import { useQuery } from '@tanstack/react-query'
import { useTranslation } from 'react-i18next'

import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { hasPermission } from '@/lib/admin-permissions'
import { ROLE } from '@/lib/roles'
import { cn } from '@/lib/utils'
import { useAuthStore } from '@/stores/auth-store'

import { getTaskPlugin } from '../api'
import {
  PRICING_DIAGNOSTIC_REASONS,
  useTaskPluginScope,
} from '../lib/pricing-diagnostics'
import type { TaskPluginPricingDiagnostics } from '../types'

const VISIBLE_ISSUES = 5

type PricingIssueListProps = {
  diagnostics: TaskPluginPricingDiagnostics
  pluginKey: string
  scope: string
  saved?: boolean
  onDiagnosticsChange?: (diagnostics: TaskPluginPricingDiagnostics) => void
}

/** A checked subset of current price owners, never legacy model-name pricing. */
export function PricingIssueList(props: PricingIssueListProps) {
  const { t } = useTranslation()
  const currentScope = useTaskPluginScope()
  const user = useAuthStore((state) => state.auth.user)
  const query = useQuery({
    queryKey: [
      'task-plugin-pricing-diagnostics',
      props.pluginKey,
      props.scope,
      props.diagnostics,
    ],
    queryFn: async ({ signal }) =>
      (await getTaskPlugin(props.pluginKey, undefined, signal))
        .pricing_diagnostics,
    enabled: false,
    retry: false,
    gcTime: 0,
    meta: { errorToast: false },
  })
  if (currentScope !== props.scope) return null
  const diagnostics = query.data ?? props.diagnostics
  const hidden = diagnostics.issues.length - VISIBLE_ISSUES
  return (
    <Alert
      className={cn(
        'min-w-0 grid-cols-1',
        props.saved &&
          'max-h-[calc(100dvh-8rem)] overflow-y-auto overscroll-contain'
      )}
      role='status'
    >
      <AlertTitle>{t('Current PriceVersion diagnostics')}</AlertTitle>
      <AlertDescription className='min-w-0 space-y-2 wrap-anywhere'>
        <p>
          {t(
            'Checks the current running plugin and visible price owners, not the selected historical version.'
          )}
        </p>
        {diagnostics.state === 'unavailable' && (
          <>
            <p>
              {props.saved
                ? t(
                    'Pricing diagnostics are unavailable. The plugin change was saved.'
                  )
                : t('Pricing diagnostics are unavailable.')}
            </p>
            <Button
              type='button'
              variant='outline'
              size='sm'
              className='h-auto min-h-8 whitespace-normal'
              disabled={query.isFetching}
              onClick={async () => {
                const result = await query.refetch()
                if (result.isSuccess) {
                  props.onDiagnosticsChange?.(result.data)
                }
              }}
            >
              {t('Retry pricing diagnostics')}
            </Button>
          </>
        )}
        {diagnostics.state === 'not_authorized' && (
          <p>{t('You are not authorized to read pricing diagnostics.')}</p>
        )}
        {diagnostics.state === 'ready' && diagnostics.issues.length === 0 && (
          <p>{t('No pricing issues found for visible price owners.')}</p>
        )}
        {diagnostics.state === 'ready' && diagnostics.issues.length > 0 && (
          <>
            <p>{t('Prices of these owners need attention:')}</p>
            <ul className='min-w-0 space-y-2'>
              {diagnostics.issues.slice(0, VISIBLE_ISSUES).map((issue) => {
                const canManage = Boolean(
                  user &&
                  user.role >= ROLE.ADMIN &&
                  hasPermission(user, 'pricing', 'read') &&
                  hasPermission(
                    user,
                    issue.owner_type === 'channel' ? 'channel' : 'model',
                    'read'
                  )
                )
                return (
                  <li
                    key={`${issue.owner_type}:${issue.owner_id}:${issue.plugin_key}`}
                    className='min-w-0 wrap-anywhere'
                  >
                    <p>
                      {issue.owner_type === 'channel'
                        ? t('Channel')
                        : t('Model offering')}{' '}
                      #{issue.owner_id} · {issue.owner_name}
                    </p>
                    <p>{t(PRICING_DIAGNOSTIC_REASONS[issue.reason])}</p>
                    <p>
                      {issue.plugin_name} ({issue.plugin_key})
                      {issue.price_version_id !== null && (
                        <>
                          {' '}
                          · {t('PriceVersion')} #{issue.price_version_id}
                        </>
                      )}
                    </p>
                    {canManage && (
                      <a href='/price-versions'>{t('Manage PriceVersions')}</a>
                    )}
                  </li>
                )
              })}
            </ul>
            {hidden > 0 && (
              <p>{t('And {{count}} more price owners', { count: hidden })}</p>
            )}
          </>
        )}
      </AlertDescription>
    </Alert>
  )
}
