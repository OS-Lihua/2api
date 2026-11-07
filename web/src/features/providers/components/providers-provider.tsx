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
import React, { useState } from 'react'

import useDialogState from '@/hooks/use-dialog'

import type { Provider, ProvidersDialogType } from '../types'

type ProvidersContextType = {
  open: ProvidersDialogType | null
  setOpen: (dialog: ProvidersDialogType | null) => void
  currentRow: Provider | null
  setCurrentRow: React.Dispatch<React.SetStateAction<Provider | null>>
}

const ProvidersContext = React.createContext<ProvidersContextType | null>(null)

export function ProvidersProvider(props: { children: React.ReactNode }) {
  const [open, setOpen] = useDialogState<ProvidersDialogType>(null)
  const [currentRow, setCurrentRow] = useState<Provider | null>(null)

  return (
    <ProvidersContext value={{ open, setOpen, currentRow, setCurrentRow }}>
      {props.children}
    </ProvidersContext>
  )
}

// eslint-disable-next-line react-refresh/only-export-components
export const useProviders = () => {
  const context = React.useContext(ProvidersContext)
  if (!context) {
    throw new Error('useProviders has to be used within <ProvidersProvider>')
  }
  return context
}
