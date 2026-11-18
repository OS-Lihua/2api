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
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, cleanup, renderHook, waitFor } from '@testing-library/react'
import type { AxiosAdapter, InternalAxiosRequestConfig } from 'axios'
import type { ReactNode } from 'react'
import { toast } from 'sonner'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { usePlaygroundOptions } from '../use-playground-options'
import { usePlaygroundState } from '../use-playground-state'

const originalAdapter = api.defaults.adapter
let client: QueryClient
let requests: InternalAxiosRequestConfig[]
let modelNames: string[]

function signIn(id: number): void {
  useAuthStore.getState().auth.setBundle({
    access_token: `playground-test-access-${id}`,
    token_type: 'Bearer',
    access_expires_at: Date.now() / 1000 + 3600,
    user: { id, username: `user-${id}`, role: 1 },
    session: {
      sid: `playground-test-session-${id}`,
      current: true,
      login_method: 'password',
      ip: '',
      user_agent: '',
      created_at: 0,
      last_active_at: 0,
      expires_at: Date.now() / 1000 + 3600,
    },
  })
}

function wrapper(props: { children: ReactNode }) {
  return (
    <QueryClientProvider client={client}>{props.children}</QueryClientProvider>
  )
}

function useOptions() {
  const state = usePlaygroundState()
  const options = usePlaygroundOptions({
    ...state,
    currentModel: state.config.model,
  })
  return { ...state, ...options }
}

beforeEach(() => {
  localStorage.clear()
  signIn(1)
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  requests = []
  modelNames = ['visible-offering', 'another-offering']
  api.defaults.adapter = async (config) => {
    requests.push(config)
    return {
      data: {
        success: true,
        data: config.url?.endsWith('/groups') ? {} : [...modelNames],
      },
      status: 200,
      statusText: 'OK',
      headers: {},
      config,
    }
  }
})

afterEach(() => {
  cleanup()
  client.clear()
  api.defaults.adapter = originalAdapter
  useAuthStore.getState().auth.reset()
  localStorage.clear()
})

test('loads offerings for the real session without a group request or routing parameter', async () => {
  const { result } = renderHook(useOptions, { wrapper })

  await waitFor(() => expect(result.current.models).toHaveLength(2))

  expect(requests).toHaveLength(1)
  expect(requests[0].url).toBe('/api/user/self/models')
  expect(requests[0].params).toBeUndefined()
  expect(requests[0].withCredentials).toBe(true)
  expect(requests[0].headers.get('Authorization')).toBe(
    'Bearer playground-test-access-1'
  )
  expect(result.current.config.model).toBe('visible-offering')
  expect(localStorage.getItem('playground_config:1')).not.toContain(
    'playground-test-access'
  )
})

test('keeps a valid model selection and switches models without loading another group catalog', async () => {
  localStorage.setItem(
    'playground_config:1',
    JSON.stringify({ model: 'another-offering', group: 'legacy' })
  )
  const { result } = renderHook(useOptions, { wrapper })
  await waitFor(() => expect(result.current.models).toHaveLength(2))
  expect(result.current.config.model).toBe('another-offering')

  act(() => result.current.updateConfig('model', 'visible-offering'))

  expect(result.current.config.model).toBe('visible-offering')
  expect(requests).toHaveLength(1)
})

test('clears an obsolete model when the session has no visible offerings', async () => {
  modelNames = []
  const { result } = renderHook(useOptions, { wrapper })

  await waitFor(() => expect(result.current.config.model).toBe(''))
  expect(result.current.models).toEqual([])
  expect(result.current.isLoadingModels).toBe(false)
})

test('keeps controls unavailable while offerings are loading and fills them after the response', async () => {
  let resolveResponse:
    | ((value: Awaited<ReturnType<AxiosAdapter>>) => void)
    | undefined
  let pendingConfig: InternalAxiosRequestConfig | undefined
  api.defaults.adapter = (config) => {
    pendingConfig = config
    return new Promise((resolve) => {
      resolveResponse = resolve
    })
  }
  const { result } = renderHook(useOptions, { wrapper })
  expect(result.current.isLoadingModels).toBe(true)
  expect(result.current.models).toEqual([])
  await waitFor(() => expect(pendingConfig).toBeDefined())

  await act(async () =>
    resolveResponse?.({
      data: { success: true, data: ['visible-offering'] },
      status: 200,
      statusText: 'OK',
      headers: {},
      config: pendingConfig as InternalAxiosRequestConfig,
    })
  )

  await waitFor(() =>
    expect(result.current.config.model).toBe('visible-offering')
  )
  expect(result.current.isLoadingModels).toBe(false)
})

test.each(['business', 'network'])(
  'reports a %s catalog failure and removes previously usable models',
  async (failure) => {
    const notification = vi
      .spyOn(toast, 'error')
      .mockImplementation(() => 'test-toast')
    const { result } = renderHook(useOptions, { wrapper })
    await waitFor(() => expect(result.current.models).toHaveLength(2))
    api.defaults.adapter = async (config) => {
      if (failure === 'network') throw new Error('Catalog unavailable')
      return {
        data: { success: false, message: 'Catalog unavailable' },
        status: 200,
        statusText: 'OK',
        headers: {},
        config,
      }
    }

    await act(async () => {
      await client.invalidateQueries()
    })

    await waitFor(() =>
      expect(notification).toHaveBeenCalledWith('Catalog unavailable')
    )
    expect(result.current.models).toEqual([])
    expect(result.current.isLoadingModels).toBe(false)
  }
)

test('loads a new user catalog and clears choices on sign-out instead of reusing another account', async () => {
  client.setDefaultOptions({ queries: { retry: false, staleTime: Infinity } })
  const { result } = renderHook(useOptions, { wrapper })
  await waitFor(() => expect(result.current.models).toHaveLength(2))
  modelNames = ['second-user-offering']

  act(() => signIn(2))

  await waitFor(() =>
    expect(result.current.models).toEqual([
      { label: 'second-user-offering', value: 'second-user-offering' },
    ])
  )
  expect(result.current.config.model).toBe('second-user-offering')
  act(() => useAuthStore.getState().auth.reset())
  await waitFor(() => expect(result.current.models).toEqual([]))
  expect(requests).toHaveLength(2)
})
