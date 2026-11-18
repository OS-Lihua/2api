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
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import { ModelGroupSelector } from '../../model-group-selector'

beforeEach(() => {
  // jsdom omits pointer capture, which the mobile Drawer uses on pointer down.
  Object.defineProperty(HTMLElement.prototype, 'setPointerCapture', {
    configurable: true,
    value: vi.fn(),
  })
})

afterEach(() => {
  Reflect.deleteProperty(HTMLElement.prototype, 'setPointerCapture')
  vi.unstubAllGlobals()
})

const models = [
  { label: 'First offering', value: 'first' },
  { label: 'Second offering', value: 'second' },
]
const groups = [
  { label: 'Default group', value: 'default' },
  { label: 'Other group', value: 'other' },
]

test('keeps group selection visible and its callback working when showGroup is omitted', async () => {
  const user = userEvent.setup()
  const onGroupChange = vi.fn()
  render(
    <ModelGroupSelector
      selectedModel='first'
      models={models}
      onModelChange={vi.fn()}
      selectedGroup='default'
      groups={groups}
      onGroupChange={onGroupChange}
    />
  )

  await user.click(
    screen.getByRole('combobox', { name: 'First offering Default group' })
  )
  await user.click(await screen.findByRole('button', { name: 'Other group' }))

  expect(onGroupChange).toHaveBeenCalledWith('other')
  expect(screen.getByText('Model Group')).toBeVisible()
})

test.each([1024, 375])(
  'hides groups at width %s while preserving searchable keyboard model selection and focus',
  async (width) => {
    vi.stubGlobal('innerWidth', width)
    const user = userEvent.setup()
    const onModelChange = vi.fn()
    render(
      <ModelGroupSelector
        selectedModel='first'
        models={models}
        onModelChange={onModelChange}
        selectedGroup='default'
        groups={groups}
        onGroupChange={vi.fn()}
        showGroup={false}
      />
    )
    const trigger = screen.getByRole('combobox', { name: 'First offering' })
    expect(trigger).toHaveAttribute('aria-expanded', 'false')

    await user.click(trigger)

    expect(trigger).toHaveAttribute('aria-expanded', 'true')
    expect(screen.queryByText('Model Group')).not.toBeInTheDocument()
    expect(screen.queryByText('Default group')).not.toBeInTheDocument()
    const search = await screen.findByPlaceholderText('Search models...')
    await user.type(search, 'second')
    const option = await screen.findByRole('option', {
      name: 'Second offering',
    })
    expect(
      screen.queryByRole('option', { name: 'First offering' })
    ).not.toBeInTheDocument()
    await user.keyboard('{ArrowDown}')
    expect(option).toHaveAttribute('aria-selected', 'true')
    await user.keyboard('{Enter}')

    expect(onModelChange).toHaveBeenCalledWith('second')
    await waitFor(() =>
      expect(trigger).toHaveAttribute('aria-expanded', 'false')
    )
    await waitFor(() => expect(trigger).toHaveFocus())
  }
)

test('supports a disabled model-only selector without group props', async () => {
  const user = userEvent.setup()
  render(
    <ModelGroupSelector
      selectedModel='first'
      models={models}
      onModelChange={vi.fn()}
      showGroup={false}
      disabled
    />
  )
  const trigger = screen.getByRole('combobox', { name: 'First offering' })

  await user.click(trigger)

  expect(trigger).toBeDisabled()
  expect(trigger).toHaveAttribute('aria-expanded', 'false')
  expect(
    screen.queryByPlaceholderText('Search models...')
  ).not.toBeInTheDocument()
})

test('shows the existing empty state when model-only search has no matches', async () => {
  const user = userEvent.setup()
  render(
    <ModelGroupSelector
      selectedModel=''
      models={models}
      onModelChange={vi.fn()}
      showGroup={false}
    />
  )

  await user.click(screen.getByRole('combobox', { name: 'Model' }))
  await user.type(
    await screen.findByPlaceholderText('Search models...'),
    'unavailable'
  )

  expect(screen.getByText('No model found.')).toBeVisible()
  expect(screen.queryAllByRole('option')).toHaveLength(0)
})
