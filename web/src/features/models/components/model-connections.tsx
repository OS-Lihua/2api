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
import { useTranslation } from 'react-i18next'

import { StatusBadge } from '@/components/status-badge'
import { ProviderChannels } from '@/features/providers/components/provider-channels'

import type { Model } from '../types'
import { ModelSquareStatus } from './model-square-status'

export function ModelConnections(props: { model: Model }) {
  const { t } = useTranslation()
  return (
    <div className='min-h-0 flex-1 space-y-6 overflow-auto p-4'>
      <p className='text-muted-foreground text-sm'>
        {t(
          'Channel status does not establish a sale price, Offering access, or request availability.'
        )}
      </p>
      <ModelSquareStatus model={props.model} detail />
      {props.model.id > 0 && props.model.name_rule === 0 ? (
        <ProviderChannels modelId={props.model.id} readOnly />
      ) : (
        <p role='status'>
          {t(
            'Connections require a saved exact-name Model. Metadata rules do not bind Channels.'
          )}
        </p>
      )}
      <section className='space-y-3'>
        <h3 className='font-medium'>{t('Supported endpoints')}</h3>
        <p className='text-muted-foreground text-sm'>
          {t(
            'Native endpoints declared by channels and model metadata. Actual availability depends on the selected route and request.'
          )}
        </p>
        <div className='flex flex-wrap gap-2'>
          {props.model.supported_endpoints?.map((endpoint) => (
            <StatusBadge key={endpoint} variant='neutral' label={endpoint} />
          ))}
          {!props.model.supported_endpoints?.length && (
            <span className='text-muted-foreground text-sm'>
              {t('No endpoints inferred from channels')}
            </span>
          )}
        </div>
      </section>
    </div>
  )
}
