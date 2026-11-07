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
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { CopyButton } from '@/components/copy-button'
import {
  StaticDataTable,
  type StaticDataTableColumn,
} from '@/components/data-table'
import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'
import { Field, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber } from '@/lib/format'
import { handleServerError } from '@/lib/handle-server-error'

import {
  importBatch,
  importExample,
  type ImportPreviewRow,
  type ImportResult,
  type ImportTarget,
} from '../bulk-api'

export function BulkImportDialog(props: {
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const { t, i18n } = useTranslation()
  const client = useQueryClient()
  const [source, setSource] = useState('')
  const [format, setFormat] = useState('json')
  const [target, setTarget] = useState<ImportTarget>('bulk-import')
  const [result, setResult] = useState<ImportResult | null>(null)
  const [confirm, setConfirm] = useState(false)
  const [fileError, setFileError] = useState('')
  const [readingFile, setReadingFile] = useState(false)
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const mutation = useMutation({
    mutationFn: (dryRun: boolean) =>
      importBatch(source, format, dryRun, target),
    onSuccess: (data) => {
      setResult(data)
      setConfirm(false)
      if (data.committed) {
        setSource('')
        void client.invalidateQueries({ queryKey: ['providers'] })
        void client.invalidateQueries({ queryKey: ['single-model-channels'] })
        void client.invalidateQueries({ queryKey: ['price-versions'] })
      }
    },
    onError: (error) => {
      setConfirm(false)
      handleServerError(error)
    },
  })
  const columns: StaticDataTableColumn<ImportPreviewRow>[] = [
    {
      id: 'row',
      header: t('Row'),
      cell: (row) => formatNumber(row.row, locale),
    },
    {
      id: 'resource',
      header: t('Resource'),
      cell: (row) => {
        if (row.resource === 'providers') return t('Providers')
        if (row.resource === 'channels') return t('Channels')
        if (row.resource === 'price-versions') return t('Price versions')
        return '—'
      },
    },
    {
      id: 'action',
      header: t('Action'),
      cell: (row) => {
        if (row.action === 'create') return t('Create')
        if (row.action === 'confirm') return t('Confirm mapping')
        if (row.action === 'enable') return t('Enable')
        return '—'
      },
    },
    {
      id: 'id',
      header: t('ID'),
      cell: (row) => (row.id ? formatNumber(row.id, locale) : '—'),
    },
    {
      id: 'details',
      header: t('Details'),
      cell: (row) => {
        const details: string[] = []
        if (row.model_id) {
          details.push(
            `${t('Model ID')}: ${formatNumber(row.model_id, locale)}`
          )
        }
        if (row.provider_id) {
          details.push(
            `${t('Provider ID')}: ${formatNumber(row.provider_id, locale)}`
          )
        }
        if (row.provider_row) {
          details.push(
            `${t('Providers')} · ${t('Row')}: ${formatNumber(row.provider_row, locale)}`
          )
        }
        if (row.protocol) details.push(row.protocol)
        if (row.currency) details.push(row.currency)
        if (row.owner_id) {
          details.push(
            `${t('Price owner ID')}: ${formatNumber(row.owner_id, locale)}`
          )
        }
        if (row.owner_row) {
          details.push(
            `${t('Channels')} · ${t('Row')}: ${formatNumber(row.owner_row, locale)}`
          )
        }
        if (row.effective_at) {
          details.push(new Date(row.effective_at * 1000).toLocaleString(locale))
        }
        return details.join(' · ') || '—'
      },
    },
    {
      id: 'result',
      header: t('Result'),
      cell: (row) => {
        if (row.error) return t(row.error)
        if (row.status) {
          return t(
            {
              pending: 'Pending confirmation',
              enabled: 'Enabled',
              disabled: 'Disabled',
            }[row.status] ?? row.status
          )
        }
        return t('Valid')
      },
    },
  ]
  const canExecute =
    result?.dry_run && result.valid && !mutation.isPending && !readingFile
  return (
    <>
      <Dialog
        open={props.open}
        onOpenChange={(open) => {
          if (!mutation.isPending) props.onOpenChange(open)
        }}
        title={t('Bulk import')}
        description={t(
          'Import Providers, Channels, and immutable price versions. Preview first, then commit the entire batch.'
        )}
        footer={
          <>
            <Button
              variant='outline'
              disabled={!source.trim() || mutation.isPending || readingFile}
              onClick={() => {
                setResult(null)
                mutation.mutate(true)
              }}
            >
              {t('Preview import')}
            </Button>
            <Button disabled={!canExecute} onClick={() => setConfirm(true)}>
              {t('Execute import')}
            </Button>
          </>
        }
      >
        <div className='space-y-4'>
          <p className='text-muted-foreground text-sm'>
            {t(
              'JSON or CSV, up to 2 MiB and 500 rows. Credentials are write-only. Nothing is saved during preview.'
            )}
          </p>
          <Field>
            <FieldLabel htmlFor='bulk-operation'>{t('Operation')}</FieldLabel>
            <NativeSelect
              id='bulk-operation'
              value={target}
              disabled={mutation.isPending || readingFile}
              onChange={(event) => {
                setTarget(event.target.value as ImportTarget)
                setResult(null)
              }}
            >
              <NativeSelectOption value='bulk-import'>
                {t('Bulk import')}
              </NativeSelectOption>
              <NativeSelectOption value='providers/import'>
                {t('Providers')}
              </NativeSelectOption>
              <NativeSelectOption value='channels/import'>
                {t('Channels')}
              </NativeSelectOption>
              <NativeSelectOption value='price-versions/import'>
                {t('Price versions')}
              </NativeSelectOption>
              <NativeSelectOption value='channels/bulk-confirm'>
                {t('Confirm mapping')}
              </NativeSelectOption>
              <NativeSelectOption value='channels/bulk-enable'>
                {t('Enable')}
              </NativeSelectOption>
            </NativeSelect>
          </Field>
          <Field>
            <FieldLabel htmlFor='bulk-format'>{t('Format')}</FieldLabel>
            <NativeSelect
              id='bulk-format'
              value={format}
              disabled={mutation.isPending || readingFile}
              onChange={(event) => {
                setFormat(event.target.value)
                setResult(null)
              }}
            >
              <NativeSelectOption value='json'>JSON</NativeSelectOption>
              <NativeSelectOption value='csv'>CSV</NativeSelectOption>
            </NativeSelect>
          </Field>
          <Field data-invalid={Boolean(fileError)}>
            <FieldLabel htmlFor='bulk-file'>{t('Import file')}</FieldLabel>
            <Input
              id='bulk-file'
              aria-invalid={Boolean(fileError)}
              aria-describedby={fileError ? 'bulk-file-error' : undefined}
              type='file'
              accept='.json,.csv,application/json,text/csv'
              disabled={mutation.isPending || readingFile}
              onChange={async (event) => {
                const file = event.target.files?.[0]
                event.target.value = ''
                setResult(null)
                if (!file) return
                if (file.size > 2 * 1024 * 1024) {
                  setFileError('Import exceeds the upload limit')
                  return
                }
                try {
                  setReadingFile(true)
                  const text = await file.text()
                  setSource(text)
                  setResult(null)
                  setFormat(file.name.endsWith('.csv') ? 'csv' : 'json')
                  setFileError('')
                } catch (error) {
                  handleServerError(error)
                } finally {
                  setReadingFile(false)
                }
              }}
            />
          </Field>
          {fileError && (
            <p
              id='bulk-file-error'
              role='alert'
              className='text-destructive text-sm'
            >
              {t(fileError)}
            </p>
          )}
          <Field>
            <FieldLabel htmlFor='bulk-content'>
              {t('Import content')}
            </FieldLabel>
            <Textarea
              id='bulk-content'
              value={source}
              disabled={mutation.isPending || readingFile}
              rows={8}
              spellCheck={false}
              autoComplete='off'
              onChange={(event) => {
                setSource(event.target.value)
                setResult(null)
                setFileError('')
              }}
            />
          </Field>
          <p className='text-muted-foreground text-sm'>
            {t(
              'Rows run in order. Use ref to name a new resource, then provider_ref, channel_ref, or owner_ref to refer to it. Use existing IDs for saved resources.'
            )}
          </p>
          <p className='text-muted-foreground text-sm'>
            {t(
              'CSV columns: resource, action, ref, data. The data cell contains a JSON object. Prices remain expression strings; no numeric conversion is applied.'
            )}
          </p>
          <div className='flex items-center justify-between'>
            <span className='text-sm'>{t('Example batch')}</span>
            <CopyButton value={importExample} size='sm' variant='outline'>
              {t('Copy example')}
            </CopyButton>
          </div>
          <pre className='bg-muted max-h-40 overflow-auto rounded p-3 text-xs'>
            {importExample}
          </pre>
          {result && (
            <section aria-live='polite' className='space-y-2'>
              <p role='status'>
                {result.committed
                  ? t('Batch imported successfully')
                  : t('Import preview')}
              </p>
              <StaticDataTable
                columns={columns}
                data={result.rows}
                getRowKey={(row) => row.row}
              />
            </section>
          )}
        </div>
      </Dialog>
      <ConfirmDialog
        open={confirm}
        onOpenChange={(open) => {
          if (!mutation.isPending) setConfirm(open)
        }}
        title={t('Execute import')}
        desc={t(
          'The server will validate every row again. Any failure rolls back the whole batch.'
        )}
        isLoading={mutation.isPending}
        handleConfirm={() => mutation.mutate(false)}
      />
    </>
  )
}

export function BulkImportButton() {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  return (
    <>
      <Button size='sm' variant='outline' onClick={() => setOpen(true)}>
        {t('Bulk import')}
      </Button>
      {open && <BulkImportDialog open onOpenChange={setOpen} />}
    </>
  )
}
