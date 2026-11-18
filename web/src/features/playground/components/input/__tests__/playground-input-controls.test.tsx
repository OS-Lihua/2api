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
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { expect, test, vi } from 'vitest'

import { getInputControlState } from '../../../lib/input/input-control-utils'
import { PlaygroundInputControls } from '../playground-input-controls'

const models = [{ label: 'Visible offering', value: 'visible-offering' }]

test.each([
  {
    name: 'available models',
    models,
    isModelLoading: false,
    disabled: false,
    available: true,
  },
  {
    name: 'empty models',
    models: [],
    isModelLoading: false,
    disabled: false,
    available: false,
  },
  {
    name: 'loading models',
    models,
    isModelLoading: true,
    disabled: false,
    available: false,
  },
  {
    name: 'disabled input',
    models,
    isModelLoading: false,
    disabled: true,
    available: false,
  },
])('$name determine input availability without any group data', (scenario) => {
  const state = getInputControlState({
    ...scenario,
    text: 'hello',
    hasStopHandler: true,
  })

  expect(state.canSubmit).toBe(scenario.available)
  expect(state.isSelectorDisabled).toBe(!scenario.available)
})

test('renders only the offering model selector and enables sending without groups', () => {
  render(
    <PlaygroundInputControls
      models={models}
      modelValue='visible-offering'
      onModelChange={vi.fn()}
      text='hello'
      tools={null}
    />
  )

  for (const trigger of screen.getAllByRole('combobox', {
    name: 'Visible offering',
  })) {
    expect(trigger).toBeEnabled()
    expect(trigger).not.toHaveTextContent('Group')
  }
  for (const send of screen.getAllByRole('button', { name: /Send/ })) {
    expect(send).toBeEnabled()
  }
})

test('keeps Stop available during generation even if the model catalog is empty', async () => {
  const user = userEvent.setup()
  const onStop = vi.fn()
  render(
    <PlaygroundInputControls
      disabled
      isGenerating
      models={[]}
      modelValue=''
      onModelChange={vi.fn()}
      onStop={onStop}
      text=''
      tools={null}
    />
  )

  const stop = screen.getAllByRole('button', { name: /Stop/ })[0]
  expect(stop).toBeEnabled()
  await user.click(stop)

  expect(onStop).toHaveBeenCalledOnce()
  expect(screen.queryByRole('button', { name: /Send/ })).not.toBeInTheDocument()
})
