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
import { describe, expect, it } from 'vitest'

import {
  PROVIDER_FORM_DEFAULT_VALUES,
  toCreateProviderPayload,
  toUpdateProviderPayload,
} from '../lib/provider-form'
import type { Provider } from '../types'

function provider(isMultiKey: boolean): Provider {
  return {
    id: 7,
    type: 1,
    name: 'account',
    status: 1,
    base_url: 'https://upstream.example',
    balance: 0,
    balance_updated_time: 0,
    created_time: 0,
    remark: '',
    channel_info: {
      is_multi_key: isMultiKey,
      multi_key_size: isMultiKey ? 2 : 0,
    },
  }
}

describe('toUpdateProviderPayload', () => {
  it('omits the key when the key field is left empty', () => {
    const payload = toUpdateProviderPayload(
      { ...PROVIDER_FORM_DEFAULT_VALUES, name: ' renamed ', key: '  ' },
      provider(false)
    )

    expect(payload).toEqual({ name: 'renamed', base_url: '', remark: '' })
  })

  it('sends the key update mode only for a multi-key provider', () => {
    const values = {
      ...PROVIDER_FORM_DEFAULT_VALUES,
      name: 'account',
      key: 'sk-new',
      key_mode: 'replace' as const,
    }

    expect(toUpdateProviderPayload(values, provider(true))).toMatchObject({
      key: 'sk-new',
      key_mode: 'replace',
    })
    expect(toUpdateProviderPayload(values, provider(false))).not.toHaveProperty(
      'key_mode'
    )
  })
})

describe('toCreateProviderPayload', () => {
  it.each([
    [3, '2025-04-01-preview', '{"azure_responses_version":"preview"}'],
    [18, 'v2.1', ''],
    [21, 'knowledge-base', ''],
    [39, 'cloudflare-account', ''],
    [
      41,
      '{"default":"us-east1"}',
      '{"vertex_key_type":"api_key","vertex_project_id":"project"}',
    ],
    [49, 'coze-agent', ''],
    [33, '', '{"aws_key_type":"api_key","aws_api_type":"mantle"}'],
  ])('preserves adapter configuration for type %s', (type, other, settings) => {
    const payload = toCreateProviderPayload({
      ...PROVIDER_FORM_DEFAULT_VALUES,
      type: Number(type),
      name: 'configured',
      key: 'new-key',
      other: String(other),
      settings: String(settings),
    })
    expect(payload.provider).toMatchObject({
      type,
      name: 'configured',
      key: 'new-key',
    })
    if (other) expect(payload.provider.other).toBe(other)
    if (settings) expect(payload.provider.settings).toBe(settings)
  })

  it('sends replacement headers only when entered and preserves omitted configuration on edit', () => {
    const values = {
      ...PROVIDER_FORM_DEFAULT_VALUES,
      name: 'configured',
      header_override: '{"Authorization":"Bearer new-secret"}',
    }
    expect(toCreateProviderPayload(values).provider.header_override).toBe(
      values.header_override
    )
    expect(toUpdateProviderPayload(values, provider(false))).toEqual({
      name: 'configured',
      base_url: '',
      remark: '',
      header_override: values.header_override,
    })
    expect(
      toUpdateProviderPayload(
        { ...values, header_override: '' },
        provider(false)
      )
    ).not.toHaveProperty('header_override')
  })
  it('sends the rotation mode only when several keys form one provider', () => {
    const values = {
      ...PROVIDER_FORM_DEFAULT_VALUES,
      name: 'account',
      key: 'sk-a\nsk-b',
      multi_key_mode: 'polling' as const,
    }

    expect(
      toCreateProviderPayload({ ...values, mode: 'multi_to_single' })
    ).toEqual({
      mode: 'multi_to_single',
      multi_key_mode: 'polling',
      provider: { type: 1, name: 'account', key: 'sk-a\nsk-b' },
    })
    expect(toCreateProviderPayload({ ...values, mode: 'batch' })).toEqual({
      mode: 'batch',
      provider: { type: 1, name: 'account', key: 'sk-a\nsk-b' },
    })
  })
})
