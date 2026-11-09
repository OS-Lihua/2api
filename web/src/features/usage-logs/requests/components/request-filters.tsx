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
import type { Table } from '@tanstack/react-table'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Alert, AlertDescription } from '@/components/ui/alert'
import { Combobox } from '@/components/ui/combobox'

import { CompactDateTimeRangePicker } from '../../components/compact-date-time-range-picker'
import {
  LogsFilterField,
  LogsFilterInput,
  LogsFilterToolbar,
} from '../../components/logs-filter-toolbar'
import type { RequestFilters, RequestLog, RequestStatus } from '../types'

interface FilterDraft {
  offering: string
  token: string
  user: string
  status: RequestStatus | ''
  start?: Date
  end?: Date
}
const EMPTY_FILTERS: FilterDraft = {
  offering: '',
  token: '',
  user: '',
  status: '',
}

export function RequestFilterBar(props: {
  table: Table<RequestLog>
  isAdmin: boolean
  isFetching: boolean
  onApply: (filters: RequestFilters) => void
}) {
  const { t } = useTranslation()
  const [draft, setDraft] = useState<FilterDraft>(EMPTY_FILTERS)
  const invalidIds = [
    draft.offering,
    draft.token,
    ...(props.isAdmin ? [draft.user] : []),
  ].filter(
    (value) =>
      value !== '' &&
      (!/^\d+$/.test(value) ||
        !Number.isSafeInteger(Number(value)) ||
        Number(value) <= 0)
  )
  const invalidId = invalidIds.length > 0
  const invalidRange = Boolean(
    draft.start && draft.end && draft.start > draft.end
  )
  const hasActiveFilters = Boolean(
    draft.offering ||
    draft.token ||
    draft.user ||
    draft.status ||
    draft.start ||
    draft.end
  )
  const fields = (
    <>
      <LogsFilterField>
        <LogsFilterInput
          aria-label={t('Model Offering ID')}
          aria-invalid={invalidIds.includes(draft.offering)}
          placeholder={t('Model Offering ID')}
          inputMode='numeric'
          value={draft.offering}
          onChange={(event) =>
            setDraft({ ...draft, offering: event.target.value })
          }
        />
      </LogsFilterField>
      <LogsFilterField>
        <LogsFilterInput
          aria-label={t('Token ID')}
          aria-invalid={invalidIds.includes(draft.token)}
          placeholder={t('Token ID')}
          inputMode='numeric'
          value={draft.token}
          onChange={(event) =>
            setDraft({ ...draft, token: event.target.value })
          }
        />
      </LogsFilterField>
      {props.isAdmin && (
        <LogsFilterField>
          <LogsFilterInput
            aria-label={t('User ID')}
            aria-invalid={invalidIds.includes(draft.user)}
            placeholder={t('User ID')}
            inputMode='numeric'
            value={draft.user}
            onChange={(event) =>
              setDraft({ ...draft, user: event.target.value })
            }
          />
        </LogsFilterField>
      )}
      <LogsFilterField>
        <Combobox
          aria-label={t('Request status')}
          value={draft.status || 'all'}
          options={[
            { value: 'all', label: t('All results') },
            { value: 'success', label: t('Success') },
            { value: 'failed', label: t('Failed') },
            { value: 'cancelled', label: t('Cancelled') },
            { value: 'partial', label: t('Partial') },
            { value: 'settlement_pending', label: t('Settlement pending') },
          ]}
          onValueChange={(value) =>
            setDraft({
              ...draft,
              status: value === 'all' ? '' : (value as RequestStatus),
            })
          }
        />
      </LogsFilterField>
      <LogsFilterField wide>
        <CompactDateTimeRangePicker
          start={draft.start}
          end={draft.end}
          onChange={({ start, end }) => setDraft({ ...draft, start, end })}
        />
      </LogsFilterField>
    </>
  )
  return (
    <div className='shrink-0 space-y-2'>
      <LogsFilterToolbar
        table={props.table}
        primaryFilters={fields}
        hasActiveFilters={hasActiveFilters}
        searchLoading={props.isFetching}
        onSearch={() => {
          if (invalidId || invalidRange) return
          props.onApply({
            model_offering_id: draft.offering
              ? Number(draft.offering)
              : undefined,
            token_id: draft.token ? Number(draft.token) : undefined,
            user_id:
              props.isAdmin && draft.user ? Number(draft.user) : undefined,
            status: draft.status || undefined,
            started_after: draft.start?.getTime(),
            started_before: draft.end?.getTime(),
          })
        }}
        onReset={() => {
          setDraft(EMPTY_FILTERS)
          props.onApply({})
        }}
      />
      {invalidId && (
        <Alert variant='destructive'>
          <AlertDescription>
            {t('Enter positive whole-number IDs.')}
          </AlertDescription>
        </Alert>
      )}
      {invalidRange && (
        <Alert variant='destructive'>
          <AlertDescription>
            {t('End time must be after start time')}
          </AlertDescription>
        </Alert>
      )}
    </div>
  )
}
