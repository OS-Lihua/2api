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
import { expect, test } from 'vitest'

import { DEFAULT_CONFIG, DEFAULT_PARAMETER_ENABLED } from '../../../constants'
import type { Message } from '../../../types'
import { buildChatCompletionPayload } from '../payload-builder'

const messages: Message[] = [
  {
    key: 'prompt',
    from: 'user',
    versions: [{ id: 'edited', content: 'Edited prompt' }],
  },
]

test.each([true, false])(
  'builds a stream=%s request with only the offering model and enabled explicit zero parameters',
  (stream) => {
    const config = {
      ...DEFAULT_CONFIG,
      group: 'legacy-route',
      model: 'visible-offering',
      stream,
      temperature: 0,
      top_p: 0,
      max_tokens: 0,
      frequency_penalty: 0,
      presence_penalty: 0,
      seed: 0,
    }
    const payload = buildChatCompletionPayload(messages, config, {
      ...DEFAULT_PARAMETER_ENABLED,
      max_tokens: true,
      seed: true,
    })

    expect(payload).toEqual({
      model: 'visible-offering',
      messages: [{ role: 'user', content: 'Edited prompt' }],
      stream,
      temperature: 0,
      top_p: 0,
      max_tokens: 0,
      frequency_penalty: 0,
      presence_penalty: 0,
      seed: 0,
    })
  }
)

test('omits disabled parameters while retaining an explicit false stream flag', () => {
  const payload = buildChatCompletionPayload(
    messages,
    { ...DEFAULT_CONFIG, stream: false },
    {
      temperature: false,
      top_p: false,
      max_tokens: false,
      frequency_penalty: false,
      presence_penalty: false,
      seed: false,
    }
  )

  expect(payload).toEqual({
    model: DEFAULT_CONFIG.model,
    messages: [{ role: 'user', content: 'Edited prompt' }],
    stream: false,
  })
})
