import { describe, expect, it } from 'vitest'
import { i18n } from '@/i18n'
import {
  describeRow,
  entryCount,
  formatOverride,
  newRow,
  parseOverride,
  QUICK_INSERTS,
  readRows,
  rowMeaning,
  rowProblems,
  writeRows
} from './model'

const sample = {
  '*': '',
  're:^x-claude-code-': '',
  'User-Agent': 'my-client/1.0',
  'anthropic-beta': '{client_header:anthropic-beta:context-1m-2025-08-07}',
  'x-trace-id': '{client_header:x-request-id}',
  'x-unwanted': '{unset}'
}

describe('header override model', () => {
  it('round-trips new-api header_override through rows, keeping key order and empty values', () => {
    const rows = readRows(sample)
    expect(rows.map(r => r.key)).toEqual(Object.keys(sample))
    expect(writeRows(rows)).toEqual(sample)
    expect(Object.keys(writeRows(rows)!)).toEqual(Object.keys(sample))
    expect(entryCount(sample)).toBe(6)
  })

  it('drops blank rows, trims keys and saves nothing as null', () => {
    expect(writeRows([newRow(' x-a ', '1'), newRow('', ''), newRow('  ', '')])).toEqual({ 'x-a': '1' })
    expect(writeRows([newRow()])).toBeNull()
    expect(writeRows([])).toBeNull()
    expect(readRows(null)).toEqual([])
    expect(entryCount(undefined)).toBe(0)
  })

  it('parses JSON text: an object of strings, empty as null', () => {
    expect(parseOverride(JSON.stringify(sample))).toEqual({ value: sample })
    expect(parseOverride('  ')).toEqual({ value: null })
    expect(parseOverride('null')).toEqual({ value: null })
    expect(parseOverride('{}')).toEqual({ value: null })
    expect(parseOverride('[]')).toEqual({ error: 'object' })
    expect(parseOverride('"x"')).toEqual({ error: 'object' })
    expect(parseOverride('{"x-a": 1}')).toEqual({ error: 'string', key: 'x-a' })
    expect(parseOverride('{')).toMatchObject({ error: 'syntax', detail: expect.any(String) })
    expect(formatOverride(null)).toBe('')
    expect(JSON.parse(formatOverride(sample))).toEqual(sample)
  })

  it('reads what each row does', () => {
    expect(rowMeaning('*', 'ignored')).toEqual({ kind: 'passAll' })
    expect(rowMeaning('re:^x-', '')).toEqual({ kind: 'passRegex', name: '^x-' })
    expect(rowMeaning('REGEX:^x-', '')).toEqual({ kind: 'passRegex', name: '^x-' })
    expect(rowMeaning('x-a', '')).toEqual({ kind: 'ignored' })
    expect(rowMeaning('x-a', '{unset}')).toEqual({ kind: 'unset' })
    expect(rowMeaning('x-a', '{client_header:x-b}')).toEqual({ kind: 'client', name: 'x-b' })
    expect(rowMeaning('x-a', '{client_header:x-b:v1,v2}')).toEqual({ kind: 'clientAppend', name: 'x-b', suffix: 'v1,v2' })
    expect(rowMeaning('x-a', '{client_header:x-b:}')).toEqual({ kind: 'clientAppend', name: 'x-b', suffix: '' })
    expect(rowMeaning('x-a', 'pre {client_header:x-b}')).toEqual({ kind: 'static' })
    expect(rowMeaning('x-a', '{api_key}')).toEqual({ kind: 'static' })
  })

  it('reads rows as the server does: trimmed, the first } ending the placeholder', () => {
    expect(rowMeaning(' x-a ', '  ')).toEqual({ kind: 'ignored' })
    expect(rowMeaning('x-a', ' {unset} ')).toEqual({ kind: 'unset' })
    expect(rowMeaning('x-a', ' {client_header: x-b } ')).toEqual({ kind: 'client', name: 'x-b' })
    expect(rowMeaning('x-a', '{client_header:x-b: v1 }')).toEqual({ kind: 'clientAppend', name: 'x-b', suffix: 'v1' })
    expect(rowMeaning('x-a', '{client_header:x-b:a}b}')).toEqual({ kind: 'static' })
    expect(rowMeaning('x-a', '{client_header:x-b}}')).toEqual({ kind: 'static' })
    expect(rowMeaning('re: ^x- ', '')).toEqual({ kind: 'passRegex', name: '^x-' })
  })

  it('describes rows in one line', () => {
    i18n.global.locale.value = 'zh'
    const t = i18n.global.t
    expect(readRows(sample).map(r => describeRow(r, t))).toEqual([
      '透传全部客户端请求头',
      '透传名称匹配 ^x-claude-code- 的客户端请求头',
      'User-Agent 设为 my-client/1.0',
      'anthropic-beta 取客户端 anthropic-beta 的值，再追加 context-1m-2025-08-07',
      'x-trace-id 取客户端 x-request-id 的值',
      '删除 x-unwanted'
    ])
  })

  it('inserts placeholders for the row', () => {
    const r = newRow(' x-a ', 'old')
    const apply = (key: string) => QUICK_INSERTS.find(q => q.key === key)!.apply(r)
    expect(apply('client')).toEqual({ value: '{client_header:x-a}' })
    expect(apply('clientAppend')).toEqual({ value: '{client_header:x-a:}' })
    expect(apply('unset')).toEqual({ value: '{unset}' })
    expect(apply('passAll')).toEqual({ key: '*', value: '' })
  })

  it('flags rows that cannot be saved as written', () => {
    expect(
      rowProblems([
        newRow('x-a', '1'),
        newRow('', 'v'),
        newRow('', '  '),
        newRow(' x-a', '2'),
        newRow('X-A', '3'),
        newRow('User-Agent', 'a')
      ])
    ).toEqual([null, 'noKey', null, 'duplicate', 'duplicate', null])
    expect(rowProblems([newRow('user-agent ', 'b'), newRow('User-Agent', 'a')])).toEqual([null, 'duplicate'])
  })

  it('flags placeholders the server refuses, such as quick inserts on a row without a name', () => {
    const inserted = (key: string) => QUICK_INSERTS.find(q => q.key === key)!.apply(newRow())
    expect(rowProblems([newRow('x-a', inserted('client').value)])).toEqual(['noSource'])
    expect(rowProblems([newRow('x-a', '{client_header:x-b:}')])).toEqual(['emptySuffix'])
    expect(rowProblems([newRow('x-a', '{client_header:x-b: }')])).toEqual(['emptySuffix'])
    expect(rowProblems([newRow('x-a', '{client_header::v}')])).toEqual(['noSource'])
    expect(rowProblems([newRow('x-a', '{client_header:x-b:v}'), newRow('x-c', '{client_header:x-b}')])).toEqual([null, null])
    expect(
      rowProblems([
        newRow('x-a', '{client_header:x-b:a}b}'),
        newRow('x-b', '{client_header:x-b}}'),
        newRow('x-c', 'pre {client_header:x-b}'),
        newRow('x-d', '{unset} x'),
        newRow('*', '{unset} x'),
        newRow('x-e', '{api_key}')
      ])
    ).toEqual(['partialPlaceholder', 'partialPlaceholder', 'partialPlaceholder', 'partialPlaceholder', null, null])
  })
})
