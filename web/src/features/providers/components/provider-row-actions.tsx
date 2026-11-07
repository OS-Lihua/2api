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
import { useQueryClient } from '@tanstack/react-query'
import {
  Edit,
  KeyRound,
  Power,
  PowerOff,
  RefreshCw,
  Trash2,
} from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { DataTableRowActionMenu } from '@/components/data-table/core/row-action-menu'
import { Button } from '@/components/ui/button'
import {
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuShortcut,
} from '@/components/ui/dropdown-menu'
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from '@/components/ui/tooltip'
import { CHANNEL_STATUS } from '@/features/channels/constants'
import { formatCurrencyFromUSD } from '@/lib/currency'
import { handleServerError } from '@/lib/handle-server-error'

import {
  providersQueryKeys,
  refreshProviderBalance,
  setProviderEnabled,
} from '../api'
import { ERROR_MESSAGES, SUCCESS_MESSAGES } from '../constants'
import type { Provider } from '../types'
import { useProviders } from './providers-provider'

export function ProviderRowActions(props: { provider: Provider }) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const { setOpen, setCurrentRow } = useProviders()
  const provider = props.provider
  const isEnabled = provider.status === CHANNEL_STATUS.ENABLED
  const isMultiKey = provider.channel_info?.is_multi_key === true

  const openDialog = (dialog: 'update' | 'delete' | 'keys') => {
    setCurrentRow(provider)
    setOpen(dialog)
  }

  const refreshList = () =>
    queryClient.invalidateQueries({ queryKey: providersQueryKeys.lists() })

  const handleToggleStatus = async () => {
    try {
      const result = await setProviderEnabled(provider.id, !isEnabled)
      if (!result.success) {
        handleServerError(result)
        return
      }
      toast.success(
        t(isEnabled ? SUCCESS_MESSAGES.DISABLED : SUCCESS_MESSAGES.ENABLED)
      )
      void refreshList()
    } catch (error) {
      handleServerError(error)
    }
  }

  const handleRefreshBalance = async () => {
    try {
      const result = await refreshProviderBalance(provider.id)
      if (!result.success || !result.data) {
        handleServerError(result, t(ERROR_MESSAGES.BALANCE_FAILED))
        return
      }
      if (result.data.raw_response) {
        toast.info(result.data.raw_response)
      } else {
        toast.success(
          t('Balance updated: {{balance}}', {
            balance: formatCurrencyFromUSD(result.data.balance),
          })
        )
      }
      void refreshList()
    } catch (error) {
      handleServerError(error, t(ERROR_MESSAGES.BALANCE_FAILED))
    }
  }

  return (
    <div className='-ml-1.5 flex items-center gap-1'>
      <Tooltip>
        <TooltipTrigger
          render={
            <Button
              variant='ghost'
              size='icon-sm'
              onClick={() => openDialog('update')}
              aria-label={t('Edit')}
            />
          }
        >
          <Edit />
        </TooltipTrigger>
        <TooltipContent>{t('Edit')}</TooltipContent>
      </Tooltip>

      <DataTableRowActionMenu ariaLabel={t('Open menu')} modal={false}>
        <DropdownMenuItem onClick={handleToggleStatus}>
          {isEnabled ? t('Disable') : t('Enable')}
          <DropdownMenuShortcut>
            {isEnabled ? <PowerOff size={16} /> : <Power size={16} />}
          </DropdownMenuShortcut>
        </DropdownMenuItem>
        {isMultiKey ? (
          <DropdownMenuItem onClick={() => openDialog('keys')}>
            {t('Manage Keys')}
            <DropdownMenuShortcut>
              <KeyRound size={16} />
            </DropdownMenuShortcut>
          </DropdownMenuItem>
        ) : (
          <DropdownMenuItem onClick={handleRefreshBalance}>
            {t('Update balance')}
            <DropdownMenuShortcut>
              <RefreshCw size={16} />
            </DropdownMenuShortcut>
          </DropdownMenuItem>
        )}
        <DropdownMenuSeparator />
        <DropdownMenuItem
          onClick={() => openDialog('delete')}
          className='text-destructive focus:text-destructive'
        >
          {t('Delete')}
          <DropdownMenuShortcut>
            <Trash2 size={16} />
          </DropdownMenuShortcut>
        </DropdownMenuItem>
      </DataTableRowActionMenu>
    </div>
  )
}
