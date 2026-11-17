// The account's request header override (CONTRACTS §108), in new-api's
// channel header_override format: an object of header name -> value. A key
// `*`, `re:<pattern>` or `regex:<pattern>` passes client headers through; a
// value is literal text, `{client_header:name}`, `{client_header:name:suffix}`
// or `{unset}`. The editor works on rows; the server does the real checks
// (blocked headers, placeholders, limits), this file only the shape.

import { parseJsonObject, type JsonParse } from '../controls/jsonObject'

export type HeaderOverride = Record<string, string>

export interface HeaderRow {
  id: number
  key: string
  value: string
}

/** What a row does: passes headers through, sets a value, takes the client's, deletes, or nothing. */
export type RowKind = 'passAll' | 'passRegex' | 'ignored' | 'unset' | 'client' | 'clientAppend' | 'static'

export interface RowMeaning {
  kind: RowKind
  /** The client header of `{client_header:…}`, the pattern of a regex key. */
  name?: string
  /** What `{client_header:name:suffix}` appends. */
  suffix?: string
}

/** The most entries the server takes (paramoverride.MaxHeaderOverrideEntries). */
export const MAX_ENTRIES = 100

let lastId = 0
export const newRow = (key = '', value = ''): HeaderRow => ({ id: ++lastId, key, value })

export const readRows = (v: HeaderOverride | null | undefined): HeaderRow[] => Object.entries(v ?? {}).map(([k, val]) => newRow(k, val))

const blank = (r: HeaderRow) => !r.key.trim() && !r.value.trim()

/** The override to save: null when there is nothing in it. Blank rows are dropped, keys trimmed. */
export function writeRows(rows: HeaderRow[]): HeaderOverride | null {
  const kept = rows.filter(r => !blank(r))
  return kept.length ? Object.fromEntries(kept.map(r => [r.key.trim(), r.value])) : null
}

export const entryCount = (v: HeaderOverride | null | undefined) => Object.keys(v ?? {}).length

export const formatOverride = (v: HeaderOverride | null | undefined) => (entryCount(v) ? JSON.stringify(v, null, 2) : '')

/** Parses JSON text of an override: the value, or why it is not one (`object`, or `string` with the key whose value is not a string). */
export function parseOverride(text: string): JsonParse<HeaderOverride> {
  const parsed = parseJsonObject(text)
  if (!('value' in parsed)) return parsed
  const bad = Object.entries(parsed.value ?? {}).find(([, val]) => typeof val !== 'string')
  return bad ? { error: 'string', key: bad[0] } : { value: parsed.value as HeaderOverride | null }
}

const REGEX_KEY = /^(?:re|regex):/i
// The placeholder is the whole value and its first `}` the last character;
// the name ends at the first `:`.
const CLIENT = /^\{client_header:([^:}]*)(?::([^}]*))?\}$/

/** What a row does, as the gateway reads it (key and value trimmed). */
export function rowMeaning(key: string, value: string): RowMeaning {
  const k = key.trim()
  const v = value.trim()
  if (k === '*') return { kind: 'passAll' }
  if (REGEX_KEY.test(k)) return { kind: 'passRegex', name: k.replace(REGEX_KEY, '').trim() }
  if (v === '') return { kind: 'ignored' }
  if (v === '{unset}') return { kind: 'unset' }
  const m = CLIENT.exec(v)
  if (!m) return { kind: 'static' }
  const name = m[1].trim()
  return m[2] === undefined ? { kind: 'client', name } : { kind: 'clientAppend', name, suffix: m[2].trim() }
}

type T = (key: string, named?: Record<string, unknown>) => string
const P = 'accounts.headerOverride'

/** The row in one line, e.g. "x-trace-id ← client x-request-id". */
export function describeRow(r: Pick<HeaderRow, 'key' | 'value'>, t: T): string {
  const m = rowMeaning(r.key, r.value)
  return t(`${P}.say.${m.kind}`, { key: r.key.trim(), value: r.value, name: m.name ?? '', suffix: m.suffix ?? '' })
}

/** Quick inserts beside a row's value: what each does to the row. */
export const QUICK_INSERTS: ReadonlyArray<{ key: string; apply: (r: HeaderRow) => Partial<HeaderRow> }> = [
  { key: 'client', apply: r => ({ value: `{client_header:${r.key.trim()}}` }) },
  { key: 'clientAppend', apply: r => ({ value: `{client_header:${r.key.trim()}:}` }) },
  { key: 'unset', apply: () => ({ value: '{unset}' }) },
  { key: 'passAll', apply: () => ({ key: '*', value: '' }) }
]

/**
 * A row that cannot be saved as written: a value without a name, a name used
 * twice (names are case-insensitive), `{client_header:}` without a client
 * header (`noSource`), `{client_header:name:}` with nothing to append
 * (`emptySuffix`), or a placeholder that is not the whole value (`partialPlaceholder`).
 */
export type RowProblem = 'noKey' | 'duplicate' | 'noSource' | 'emptySuffix' | 'partialPlaceholder'

/** What the server refuses in a row's placeholder, whatever its name. */
export function placeholderProblem(key: string, value: string): RowProblem | null {
  const m = rowMeaning(key, value)
  if (m.kind === 'static') return /\{client_header:|\{unset\}/.test(value) ? 'partialPlaceholder' : null
  if (m.kind !== 'client' && m.kind !== 'clientAppend') return null
  if (!m.name) return 'noSource'
  return m.kind === 'clientAppend' && !m.suffix ? 'emptySuffix' : null
}

export function rowProblems(rows: HeaderRow[]): Array<RowProblem | null> {
  const seen = new Set<string>()
  return rows.map(r => {
    if (blank(r)) return null
    const k = r.key.trim().toLowerCase()
    if (!k) return 'noKey'
    if (seen.has(k)) return 'duplicate'
    seen.add(k)
    return placeholderProblem(r.key, r.value)
  })
}
