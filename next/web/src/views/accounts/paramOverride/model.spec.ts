import { describe, expect, it } from 'vitest'
import {
  duplicateDraft,
  moveDraft,
  parseOverride,
  readOperation,
  readOverride,
  textToValue,
  validateOperation,
  valueToText,
  writeOverride
} from './model'

describe('param override model', () => {
  it('round-trips new-api operations, keeping keys it does not know', () => {
    const v = {
      operations: [
        { description: 'fix', mode: 'set', path: 'temperature', value: 0.7, keep_origin: true },
        { mode: 'set_header', path: 'anthropic-beta', value: { 'context-1m-2025-08-07': null, $append: ['x'] } },
        { mode: 'pass_headers', value: ['x-a', 'x-b'] },
        { mode: 'replace', path: 'model', from: 'a', to: '' },
        { mode: 'return_error', value: { message: 'no', status_code: 403, skip_retry: false } },
        {
          mode: 'delete',
          path: 'top_p',
          logic: 'AND',
          conditions: [{ path: 'model', mode: 'prefix', value: 'claude-', invert: true }],
          audit: 1
        }
      ]
    }
    expect(writeOverride(readOverride(v))).toEqual({
      operations: [
        v.operations[0],
        v.operations[1],
        v.operations[2],
        { mode: 'replace', path: 'model', from: 'a' },
        v.operations[4],
        v.operations[5]
      ]
    })
  })

  it('turns legacy keys into literal set operations that run first', () => {
    const drafts = readOverride({ 'a.b': 1, operations: [{ mode: 'delete', path: 'c' }] })
    expect(writeOverride(drafts)).toEqual({
      operations: [
        { mode: 'set', path: 'a\\.b', value: 1 },
        { mode: 'delete', path: 'c' }
      ]
    })
  })

  it('reads a value as JSON when it parses and quotes strings that would not read back', () => {
    expect(textToValue('0.7')).toBe(0.7)
    expect(textToValue('{"a":1}')).toEqual({ a: 1 })
    expect(textToValue('hello')).toBe('hello')
    expect(valueToText('123')).toBe('"123"')
    expect(valueToText('hello')).toBe('hello')
    expect(textToValue(valueToText('123'))).toBe('123')
  })

  it('accepts a string or comma list for pass_headers and a string return_error', () => {
    expect(readOperation({ mode: 'pass_headers', value: 'x-a, x-b' }).list).toEqual(['x-a', 'x-b'])
    expect(readOperation({ mode: 'pass_headers', value: '["x-a", "x-b"]' }).list).toEqual(['x-a', 'x-b'])
    expect(readOperation({ mode: 'pass_headers', value: { headers: ['x-a'], names: 'x-b, x-c', header: 'x-d' } }).list).toEqual([
      'x-a',
      'x-b',
      'x-c',
      'x-d'
    ])
    expect(readOperation({ mode: 'pass_headers', value: '{"names": ["x-a"]}' }).list).toEqual(['x-a'])
    expect(readOperation({ mode: 'return_error', value: 'blocked' }).error).toMatchObject({
      message: 'blocked',
      status_code: 400,
      skip_retry: true
    })
  })

  it('finds what the server would refuse', () => {
    const codes = (op: Record<string, unknown>) => validateOperation(readOperation(op)).map(p => `${p.field}:${p.code}`)
    expect(codes({ mode: 'set', path: 'a', value: 1 })).toEqual([])
    expect(codes({ mode: 'nope' })).toEqual(['mode:unsupported'])
    expect(codes({ mode: 'set', value: '{"a":' })).toEqual(['path:required', 'value:invalid_json'])
    expect(codes({ mode: 'copy', from: 'a' })).toEqual(['to:required'])
    expect(codes({ mode: 'regex_replace', path: 'a', from: '(' })).toEqual(['from:invalid_regex'])
    expect(codes({ mode: 'set_header', path: 'bad header', value: 'v' })).toEqual(['path:invalid_header'])
    expect(codes({ mode: 'pass_headers', value: [] })).toEqual(['value:required'])
    expect(codes({ mode: 'return_error', value: { message: '', status_code: 99 } })).toEqual([
      'error.message:required',
      'error.status_code:status'
    ])
    expect(codes({ mode: 'delete', path: 'a', conditions: [{ path: '', value: '{' }] })).toEqual([
      'conditions.0.path:required',
      'conditions.0.value:invalid_json'
    ])
  })

  it('round-trips sync_fields and prune_objects, leaving out an empty prune path', () => {
    const v = {
      operations: [
        { mode: 'sync_fields', from: 'header:session_id', to: 'json:prompt_cache_key' },
        { mode: 'prune_objects', path: 'tools', value: { type: 'web_search', recursive: false } },
        { mode: 'prune_objects', value: 'thinking' },
        { mode: 'set', path: 'a', value: 1, conditions: [{ path: 'using_group', mode: 'regex', value: '^vip-' }], logic: 'OR' }
      ]
    }
    expect(writeOverride(readOverride(v))).toEqual(v)
  })

  it('reads conditions written as an object of path → value as equals conditions, so saving keeps them', () => {
    const drafts = readOverride({
      operations: [{ mode: 'delete', path: 'top_p', conditions: { model: 'claude-x', stream: true } }]
    })
    expect(writeOverride(drafts)).toEqual({
      operations: [
        {
          mode: 'delete',
          path: 'top_p',
          logic: 'OR',
          conditions: [
            { path: 'model', mode: 'full', value: 'claude-x' },
            { path: 'stream', mode: 'full', value: true }
          ]
        }
      ]
    })
    expect(drafts[0].extra).toEqual({})
  })

  it('keeps a blank object-form condition key as a condition with an empty path, which the checks flag', () => {
    const [draft] = readOverride({
      operations: [{ mode: 'delete', path: 'top_p', conditions: { model: 'claude-x', ' ': 1, '': 2 } }]
    })
    expect(draft.conditions.map(c => [c.path, c.mode, c.valueText])).toEqual([
      ['model', 'full', 'claude-x'],
      ['', 'full', '1'],
      ['', 'full', '2']
    ])
    expect(validateOperation(draft).map(p => `${p.field}:${p.code}`)).toEqual([
      'conditions.1.path:required',
      'conditions.2.path:required'
    ])
  })

  it('checks sync_fields sides, prune_objects values and regex conditions', () => {
    const codes = (op: Record<string, unknown>) => validateOperation(readOperation(op)).map(p => `${p.field}:${p.code}`)
    expect(codes({ mode: 'sync_fields', from: 'json:a', to: 'header:x-a' })).toEqual([])
    expect(codes({ mode: 'sync_fields', from: 'a.b', to: 'BODY:c' })).toEqual([])
    expect(codes({ mode: 'sync_fields', from: 'query:a', to: '' })).toEqual(['to:required', 'from:invalid_prefix'])
    expect(codes({ mode: 'sync_fields', from: 'json:', to: 'header:bad name' })).toEqual(['from:required', 'to:invalid_header'])
    expect(codes({ mode: 'prune_objects', value: 'thinking' })).toEqual([])
    expect(codes({ mode: 'prune_objects', path: 'messages', value: { where: { type: 'x' } } })).toEqual([])
    expect(codes({ mode: 'prune_objects' })).toEqual(['value:required'])
    expect(codes({ mode: 'prune_objects', value: [1] })).toEqual(['value:invalid_prune'])
    const cond = (value: unknown) => codes({ mode: 'delete', path: 'a', conditions: [{ path: 'model', mode: 'regex', value }] })
    expect(cond('^claude-[a-z]+')).toEqual([])
    expect(cond('[a-z]+')).toEqual([])
    expect(cond('(')).toEqual(['conditions.0.value:invalid_regex'])
    expect(cond(1)).toEqual(['conditions.0.value:regex_string'])
  })

  it('parses JSON text of an override', () => {
    expect(parseOverride('')).toEqual({ value: null })
    expect(parseOverride('{}')).toEqual({ value: null })
    expect(parseOverride('[]')).toEqual({ error: 'object' })
    expect(parseOverride('{"operations": 1}')).toEqual({ error: 'operations' })
    expect(parseOverride('{"temperature": 1}')).toEqual({ value: { temperature: 1 } })
    expect(parseOverride('{')).toMatchObject({ error: 'syntax', detail: expect.any(String) })
    expect(parseOverride('{"operations": [{"mode": "set"}]}')).toEqual({ value: { operations: [{ mode: 'set' }] } })
  })

  it('moves and duplicates drafts', () => {
    const list = readOverride({
      operations: [
        { mode: 'delete', path: 'a' },
        { mode: 'delete', path: 'b' }
      ]
    })
    expect(moveDraft(list, 0, 1).map(d => d.path)).toEqual(['b', 'a'])
    expect(moveDraft(list, 0, -1)).toBe(list)
    const copied = duplicateDraft(list, 0)
    expect(copied.map(d => d.path)).toEqual(['a', 'a', 'b'])
    expect(copied[1].id).not.toBe(copied[0].id)
  })
})
