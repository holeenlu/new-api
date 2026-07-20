/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.
*/
import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import { mergeFetchedModelsIntoMapping } from './model-mapping-validation'

describe('mergeFetchedModelsIntoMapping', () => {
  test('adds identity mappings for fetched models', () => {
    assert.deepEqual(
      JSON.parse(mergeFetchedModelsIntoMapping('', ['gpt-5', 'gpt-5'])),
      { 'gpt-5': 'gpt-5' }
    )
  })

  test('preserves custom mappings while adding missing identities', () => {
    assert.deepEqual(
      JSON.parse(
        mergeFetchedModelsIntoMapping('{"chat-model":"vendor-chat"}', [
          'vendor-chat',
          'gpt-5',
        ])
      ),
      {
        'chat-model': 'vendor-chat',
        'vendor-chat': 'vendor-chat',
        'gpt-5': 'gpt-5',
      }
    )
  })

  test('does not replace invalid user input during discovery', () => {
    const invalid = '{"broken"'
    assert.equal(mergeFetchedModelsIntoMapping(invalid, ['gpt-5']), invalid)
  })
})
