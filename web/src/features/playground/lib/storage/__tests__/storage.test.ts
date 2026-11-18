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
import { afterEach, beforeEach, expect, test } from 'vitest'

import { useAuthStore } from '@/stores/auth-store'

import { DEFAULT_CONFIG, STORAGE_KEYS } from '../../../constants'
import type { Message } from '../../../types'
import { getInitialPlaygroundConfig } from '../../state/playground-state-utils'
import {
  clearPlaygroundData,
  loadConfig,
  loadMessages,
  loadParameterEnabled,
  saveConfig,
  saveMessages,
  saveParameterEnabled,
} from '../storage'

const messages: Message[] = [
  {
    key: 'a-chat',
    from: 'user',
    versions: [{ id: 'a-version', content: 'Account A private chat' }],
  },
]

beforeEach(() => {
  localStorage.clear()
  useAuthStore.getState().auth.reset()
})

afterEach(() => {
  localStorage.clear()
  useAuthStore.getState().auth.reset()
})

test('switching accounts isolates config, messages and parameter toggles and preserves returning account data', () => {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'A', role: 1 })
  saveConfig({ model: 'account-a-model' })
  saveMessages(messages)
  saveParameterEnabled({ temperature: true })

  useAuthStore.getState().auth.reset()
  useAuthStore.getState().auth.setUser({ id: 2, username: 'B', role: 1 })
  expect(loadConfig()).toEqual({})
  expect(loadMessages()).toBeNull()
  expect(loadParameterEnabled()).toEqual({})
  saveConfig({ model: 'account-b-model' })
  saveMessages([])
  saveParameterEnabled({ temperature: false })

  useAuthStore.getState().auth.setUser({ id: 1, username: 'A', role: 1 })
  expect(loadConfig()).toEqual({ model: 'account-a-model' })
  expect(loadMessages()).toEqual(messages)
  expect(loadParameterEnabled()).toEqual({ temperature: true })

  clearPlaygroundData()
  expect(loadMessages()).toBeNull()
  useAuthStore.getState().auth.setUser({ id: 2, username: 'B', role: 1 })
  expect(loadConfig()).toEqual({ model: 'account-b-model' })
  expect(loadMessages()).toEqual([])
  expect(loadParameterEnabled()).toEqual({ temperature: false })
})

test('legacy global data is removed instead of assigned to the first signed-in account', () => {
  localStorage.setItem(
    STORAGE_KEYS.CONFIG,
    JSON.stringify({ model: 'legacy-model' })
  )
  localStorage.setItem(STORAGE_KEYS.MESSAGES, JSON.stringify(messages))
  localStorage.setItem(
    STORAGE_KEYS.PARAMETER_ENABLED,
    JSON.stringify({ temperature: true })
  )
  useAuthStore.getState().auth.setUser({ id: 2, username: 'B', role: 1 })

  expect(loadConfig()).toEqual({})
  expect(loadMessages()).toBeNull()
  expect(loadParameterEnabled()).toEqual({})
  for (const key of Object.values(STORAGE_KEYS)) {
    expect(localStorage.getItem(key)).toBeNull()
  }
})

test.each([false, true])(
  'drops legacy group from versioned=%s account config while preserving messages, zero values and toggles',
  (versioned) => {
    useAuthStore.getState().auth.setUser({ id: 1, username: 'A', role: 1 })
    const config = {
      model: 'visible-offering',
      group: 'legacy-route',
      temperature: 0,
      max_tokens: 0,
      seed: 0,
      stream: false,
    }
    localStorage.setItem(
      `${STORAGE_KEYS.CONFIG}:1`,
      JSON.stringify(versioned ? { version: 1, data: config } : config)
    )
    saveMessages(messages)
    saveParameterEnabled({ temperature: false, seed: true })

    const loaded = getInitialPlaygroundConfig()

    expect(loaded).toEqual({
      ...DEFAULT_CONFIG,
      model: 'visible-offering',
      temperature: 0,
      max_tokens: 0,
      seed: 0,
      stream: false,
    })
    expect(loaded).not.toHaveProperty('group')
    expect(localStorage.getItem(`${STORAGE_KEYS.CONFIG}:1`)).not.toContain(
      'legacy-route'
    )
    expect(loadMessages()).toEqual(messages)
    expect(loadParameterEnabled()).toEqual({ temperature: false, seed: true })
  }
)

test('saving an old config object strips group before persisting it', () => {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'A', role: 1 })
  const config = {
    model: 'visible-offering',
    group: 'legacy-route',
    stream: false,
  }

  saveConfig(config)

  expect(loadConfig()).toEqual({ model: 'visible-offering', stream: false })
  expect(localStorage.getItem(`${STORAGE_KEYS.CONFIG}:1`)).not.toContain(
    'legacy-route'
  )
})

test('storage calls without a signed-in user do not persist shared data', () => {
  saveConfig({ model: 'signed-out-model' })
  saveMessages(messages)
  saveParameterEnabled({ temperature: true })
  expect(loadConfig()).toEqual({})
  expect(loadMessages()).toBeNull()
  expect(loadParameterEnabled()).toEqual({})
  expect(localStorage.length).toBe(0)
})
