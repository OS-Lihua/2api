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
import { AlertTriangle } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import type { QuotaSaturation } from '../types'
import { DetailRow, DetailSection } from './dialogs/log-detail-layout'

export function QuotaSaturationFacts(props: { saturation: QuotaSaturation }) {
  const { t } = useTranslation()
  const kinds = {
    overflow: t('Overflow'),
    underflow: t('Underflow'),
    nan: t('Invalid (NaN)'),
  }
  return (
    <DetailSection
      icon={<AlertTriangle className='size-3.5' aria-hidden='true' />}
      label={t('Quota clamped')}
      variant='danger'
    >
      <p className='mb-1 text-xs wrap-break-word'>
        {t('Quota saturation protection triggered')}
      </p>
      <DetailRow
        label={t('Kind')}
        value={
          Object.hasOwn(kinds, props.saturation.kind)
            ? kinds[props.saturation.kind]
            : t('Invalid (NaN)')
        }
      />
      <DetailRow
        label={t('Original value')}
        value={String(props.saturation.original)}
        mono
      />
      <DetailRow
        label={t('Clamped to')}
        value={String(props.saturation.clamped)}
        mono
      />
      <DetailRow label={t('Operation')} value={props.saturation.op} mono />
    </DetailSection>
  )
}
