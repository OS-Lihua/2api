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
import type { TFunction } from 'i18next'
import { describe, expect, test } from 'vitest'

import { apiKeySchema, type ApiKey } from '../../types'
import {
  getApiKeyFormDefaultValues,
  getApiKeyFormSchema,
  transformApiKeyToFormDefaults,
  transformFormDataToPayload,
} from '../api-key-form'

const t = ((key: string) => key) as TFunction

const baseApiKey: ApiKey = {
  id: 1,
  name: 'test',
  key: 'test********1234',
  status: 1,
  expired_time: -1,
  created_time: 1,
  accessed_time: 0,
  allow_ips: '',
}

const legacyFields = {
  remain_quota: 500000,
  used_quota: 100000,
  unlimited_quota: false,
  allow_user_quota_query: true,
  model_limits_enabled: true,
  model_limits: 'legacy-model',
  group: 'auto',
  auto_groups: ['vip', 'default'],
  cross_group_retry: true,
}

describe('API key form payload', () => {
  test('accepts a Key response without legacy quota, model or group fields', () => {
    expect(apiKeySchema.parse(baseApiKey)).toEqual(baseApiKey)
  })

  test('discards legacy response fields before populating and saving an existing Key', () => {
    const apiKey = apiKeySchema.parse({ ...baseApiKey, ...legacyFields })
    expect(apiKey).toEqual(baseApiKey)
    const defaults = transformApiKeyToFormDefaults(apiKey)
    expect(defaults).toEqual({
      name: 'test',
      expired_time: null,
      allow_ips: '',
      tokenCount: 1,
    })
    expect(transformFormDataToPayload(defaults)).toEqual({
      name: 'test',
      expired_time: -1,
      allow_ips: '',
    })
  })

  test('submits only supported fields even if stale form values contain legacy settings', () => {
    const result = getApiKeyFormSchema(t).parse({
      ...getApiKeyFormDefaultValues(),
      ...legacyFields,
      remain_quota_dollars: 10,
      auto_groups_mode: 'custom',
      name: 'new-key',
      tokenCount: 2,
    })

    expect(transformFormDataToPayload(result)).toEqual({
      name: 'new-key',
      expired_time: -1,
      allow_ips: '',
    })
  })

  test('retains expiration to the second and IP restrictions when an existing Key is saved', () => {
    const defaults = transformApiKeyToFormDefaults({
      ...baseApiKey,
      expired_time: 2000000000,
      allow_ips: '192.0.2.1\n2001:db8::/32',
    })

    expect(defaults.expired_time).toEqual(new Date(2000000000000))
    expect(transformFormDataToPayload(defaults)).toEqual({
      name: 'test',
      expired_time: 2000000000,
      allow_ips: '192.0.2.1\n2001:db8::/32',
    })
  })

  test('rejects an empty name before submitting a Key', () => {
    const result = getApiKeyFormSchema(t).safeParse(
      getApiKeyFormDefaultValues()
    )

    expect(result.success).toBe(false)
    if (result.success) return
    expect(result.error.issues[0]?.path).toEqual(['name'])
    expect(result.error.issues[0]?.message).toBe('Please enter a name')
  })

  test('rejects a batch quantity below one', () => {
    const result = getApiKeyFormSchema(t).safeParse({
      ...getApiKeyFormDefaultValues(),
      name: 'batch',
      tokenCount: 0,
    })

    expect(result.success).toBe(false)
    if (result.success) return
    expect(result.error.issues[0]?.path).toEqual(['tokenCount'])
  })
})
