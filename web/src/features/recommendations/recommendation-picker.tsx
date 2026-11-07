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
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { ErrorState } from '@/components/error-state'
import { Button } from '@/components/ui/button'
import { hasPermission } from '@/lib/admin-permissions'
import { useAuthStore } from '@/stores/auth-store'

import {
  queryRecommendations,
  type RecommendationInput,
  type RecommendationKind,
  type RecommendationRule,
} from './api'

// Queries never change form values. Only a deliberate confirmation prefills
// fields, so a late response cannot replace the administrator's edits.
export function RecommendationPicker(props: {
  kind: RecommendationKind
  input: RecommendationInput
  onConfirm: (rule: RecommendationRule) => void
}) {
  const { t } = useTranslation()
  const user = useAuthStore((state) => state.auth.user)
  const [pending, setPending] = useState<{
    rule: RecommendationRule
    source: string
  } | null>(null)
  const source = JSON.stringify(props.input)
  const query = useQuery({
    queryKey: ['recommendations', props.kind, props.input],
    queryFn: () => queryRecommendations(props.kind, props.input),
    enabled:
      hasPermission(user, 'recommendation', 'read') &&
      (props.kind === 'provider' || Boolean(props.input.model_name)),
  })
  if (!hasPermission(user, 'recommendation', 'read')) return null
  if (query.isError) {
    return (
      <ErrorState
        description={t('Failed to load')}
        onRetry={() => void query.refetch()}
      />
    )
  }
  return (
    <div className='space-y-2'>
      <p className='text-muted-foreground text-sm'>
        {t('Recommendations only prefill fields. Review them before saving.')}
      </p>
      {query.isFetching && <p role='status'>{t('Loading...')}</p>}
      {query.data?.length === 0 && (
        <p role='status'>{t('No recommendation found')}</p>
      )}
      {query.data?.map((rule) => (
        <div key={rule.id} className='flex flex-wrap items-center gap-2'>
          <span className='text-sm'>
            {props.kind === 'provider'
              ? rule.provider_name
              : rule.publisher_name}
          </span>
          <Button
            type='button'
            variant='outline'
            size='sm'
            onClick={() => setPending({ rule, source })}
          >
            {t('Use recommendation')}
          </Button>
        </div>
      ))}
      <ConfirmDialog
        open={pending !== null}
        onOpenChange={(open) => {
          if (!open) setPending(null)
        }}
        title={t('Confirm recommendation')}
        desc={
          pending
            ? `${pending.rule.provider_name || pending.rule.publisher_name} · ${pending.rule.pattern}`
            : ''
        }
        confirmText={t('Confirm recommendation')}
        disabled={pending?.source !== source}
        handleConfirm={() => {
          if (pending && pending.source === source) {
            props.onConfirm(pending.rule)
          }
          setPending(null)
        }}
      />
    </div>
  )
}
