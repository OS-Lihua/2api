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
import { TriangleAlert } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { StatusBadge, type StatusVariant } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import {
  Popover,
  PopoverTrigger,
  PopoverContent,
  PopoverTitle,
  PopoverDescription,
} from '@/components/ui/popover'

import type { Model } from '../types'

export function ModelSquareStatus(props: { model: Model; detail?: boolean }) {
  const { t } = useTranslation()
  let label: string
  let description: string
  let variant: StatusVariant = 'neutral'
  if (props.model.status !== 1) {
    label = t('Metadata disabled')
    description = t(
      'Metadata status does not change Offering visibility or API authorization.'
    )
  } else if (props.model.name_rule !== 0) {
    label = t('Metadata rule')
    description = t(
      'Connections require a saved exact-name Model. Metadata rules do not bind Channels.'
    )
  } else if (
    props.model.enabled_channel_count === undefined ||
    props.model.configured_channel_count === undefined
  ) {
    return <span className='text-muted-foreground'>—</span>
  } else if (props.model.enabled_channel_count > 0) {
    label = t('Enabled Channels')
    description = t(
      'Enabled counts include only enabled Channels with confirmed mappings and enabled Providers. They do not establish sale prices or Offering access.'
    )
    variant = 'success'
  } else {
    label = t('No enabled Channels')
    description =
      props.model.configured_channel_count === 0
        ? t('No Channel is configured for this Model ID.')
        : t(
            'No Channel has an enabled Provider and a confirmed, enabled mapping for this Model ID.'
          )
    variant = 'warning'
  }
  const badge = (
    <StatusBadge
      variant={variant}
      label={label}
      icon={variant === 'warning' ? TriangleAlert : undefined}
      copyable={false}
      title={undefined}
    />
  )
  if (props.detail) {
    return (
      <div className='space-y-2'>
        {badge}
        <p className='text-muted-foreground text-sm'>{description}</p>
      </div>
    )
  }
  return (
    <Popover>
      <PopoverTrigger
        render={
          <Button
            variant='ghost'
            size='sm'
            title={description}
            aria-description={description}
            className='h-auto max-w-full min-w-0 cursor-pointer border-0 p-0'
          />
        }
      >
        {badge}
      </PopoverTrigger>
      <PopoverContent
        role='dialog'
        className='max-w-[calc(100vw-2rem)] break-words whitespace-normal'
        collisionPadding={16}
      >
        <PopoverTitle>{label}</PopoverTitle>
        <PopoverDescription>{description}</PopoverDescription>
      </PopoverContent>
    </Popover>
  )
}
