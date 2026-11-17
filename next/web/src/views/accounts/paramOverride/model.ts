// The account's request parameter override (CONTRACTS §84), in new-api's
// param_override format: {"operations": [...]} plus optional legacy keys
// (each a literal top-level field to set). The editor works on drafts —
// one per operation, with the value as text where it is typed as JSON — and
// writes the operations format back; legacy keys become `set` operations.

import { isJsonObject as isObject, parseJsonObject, type JsonParse } from '../controls/jsonObject'

export type ModeGroup = 'body' | 'string' | 'header' | 'control'
/** How an operation's value is edited: JSON text, plain text, a header list, or the error form. */
export type ValueKind = 'json' | 'text' | 'header' | 'list' | 'error'

interface ModeMeta {
  group: ModeGroup
  /** path: a JSON path ('path'), a header name ('header'), or none; `pathOptional` when empty means the whole body. */
  path?: 'path' | 'header'
  pathOptional?: boolean
  value?: ValueKind
  valueRequired?: boolean
  /**
   * from/to: 'path', 'header', search / replacement 'text', or a 'sync' side
   * (`json:path`, `body:path`, `header:name` or a bare path); `toOptional`
   * for the replacement of replace / regex_replace.
   */
  fromTo?: 'path' | 'header' | 'text' | 'sync'
  toOptional?: boolean
  keepOrigin?: boolean
}

export const MODES: Record<string, ModeMeta> = {
  set: { group: 'body', path: 'path', value: 'json', keepOrigin: true },
  delete: { group: 'body', path: 'path' },
  append: { group: 'body', path: 'path', value: 'json', keepOrigin: true },
  prepend: { group: 'body', path: 'path', value: 'json', keepOrigin: true },
  copy: { group: 'body', fromTo: 'path' },
  move: { group: 'body', fromTo: 'path' },
  sync_fields: { group: 'body', fromTo: 'sync' },
  prune_objects: { group: 'body', path: 'path', pathOptional: true, value: 'json', valueRequired: true },
  replace: { group: 'string', path: 'path', fromTo: 'text', toOptional: true },
  regex_replace: { group: 'string', path: 'path', fromTo: 'text', toOptional: true },
  trim_prefix: { group: 'string', path: 'path', value: 'text', valueRequired: true },
  trim_suffix: { group: 'string', path: 'path', value: 'text', valueRequired: true },
  ensure_prefix: { group: 'string', path: 'path', value: 'text', valueRequired: true },
  ensure_suffix: { group: 'string', path: 'path', value: 'text', valueRequired: true },
  trim_space: { group: 'string', path: 'path' },
  to_lower: { group: 'string', path: 'path' },
  to_upper: { group: 'string', path: 'path' },
  set_header: { group: 'header', path: 'header', value: 'header', valueRequired: true, keepOrigin: true },
  delete_header: { group: 'header', path: 'header' },
  copy_header: { group: 'header', fromTo: 'header', keepOrigin: true },
  move_header: { group: 'header', fromTo: 'header', keepOrigin: true },
  pass_headers: { group: 'header', value: 'list', keepOrigin: true },
  return_error: { group: 'control', value: 'error' }
}
export const MODE_GROUPS: ModeGroup[] = ['body', 'string', 'header', 'control']
export const CONDITION_MODES = ['full', 'prefix', 'suffix', 'contains', 'regex', 'gt', 'gte', 'lt', 'lte'] as const
export type ConditionMode = (typeof CONDITION_MODES)[number]

/** Keys the request context offers to conditions besides the request body. */
export const CONTEXT_KEYS = [
  'model',
  'upstream_model',
  'original_model',
  'request_path',
  'request_headers.',
  'header_override.',
  'retry_index',
  'is_retry',
  'stream',
  'user_id',
  'group',
  'using_group',
  'account_id'
]

export const MAX_OPERATIONS = 100
export const MAX_DESCRIPTION = 200

export type Json = unknown
export type Override = Record<string, Json>

export interface ErrorValue {
  message: string
  status_code: number
  code: string
  type: string
  skip_retry: boolean
}

export interface ConditionDraft {
  id: number
  path: string
  mode: string
  valueText: string
  invert: boolean
  passMissing: boolean
}

export interface OperationDraft {
  id: number
  description: string
  mode: string
  path: string
  from: string
  to: string
  /** The value: JSON text ('json'), plain text ('text', a string 'header'), or a header mapping as JSON. */
  valueText: string
  list: string[]
  error: ErrorValue
  keepOrigin: boolean
  logic: 'OR' | 'AND'
  conditions: ConditionDraft[]
  /** Keys this editor does not know, written back unchanged. */
  extra: Record<string, Json>
}

let seq = 0
const nextId = () => ++seq

const str = (x: unknown) => (typeof x === 'string' ? x : '')

const DEFAULT_ERROR: ErrorValue = { message: '', status_code: 400, code: '', type: '', skip_retry: true }

// ---------------------------------------------------------------- values

/**
 * JSON text of a value as the editor shows it: strings as they are unless
 * they would read back as another type (then quoted), everything else as JSON.
 */
export function valueToText(v: Json): string {
  if (v === undefined) return ''
  if (typeof v === 'string') return parsesAsJson(v) ? JSON.stringify(v) : v
  return JSON.stringify(v, null, isObject(v) || Array.isArray(v) ? 2 : undefined)
}

function parsesAsJson(text: string): boolean {
  try {
    JSON.parse(text)
    return true
  } catch {
    return false
  }
}

/** The value a JSON text stands for: JSON when it parses, the text itself otherwise. */
export function textToValue(text: string): Json {
  const s = text.trim()
  if (!s) return ''
  try {
    return JSON.parse(s)
  } catch {
    return text
  }
}

/** Whether text that looks like JSON (an object or array) fails to parse. */
export function brokenJson(text: string): string {
  const s = text.trim()
  if (!/^[[{]/.test(s)) return ''
  try {
    JSON.parse(s)
    return ''
  } catch (e) {
    return (e as Error).message
  }
}

// ---------------------------------------------------------------- read / write

function readError(v: Json): ErrorValue {
  if (typeof v === 'string') return { ...DEFAULT_ERROR, message: v }
  if (!isObject(v)) return { ...DEFAULT_ERROR }
  const status = Number(v.status_code ?? v.status ?? DEFAULT_ERROR.status_code)
  return {
    message: str(v.message) || str(v.msg),
    status_code: Number.isInteger(status) ? status : DEFAULT_ERROR.status_code,
    code: v.code === undefined ? '' : String(v.code),
    type: str(v.type),
    skip_retry: v.skip_retry !== false
  }
}

function writeError(e: ErrorValue): Record<string, Json> {
  const out: Record<string, Json> = { message: e.message.trim(), status_code: e.status_code }
  if (e.code.trim()) out.code = e.code.trim()
  if (e.type.trim()) out.type = e.type.trim()
  if (!e.skip_retry) out.skip_retry = false
  return out
}

/** The names of a pass_headers value: an array, a comma list, either as JSON text, or an object of headers / names / header. */
function readList(v: Json): string[] {
  if (Array.isArray(v)) return v.map(String)
  if (isObject(v)) return ['headers', 'names', 'header'].flatMap(k => readList(v[k]))
  if (typeof v !== 'string') return []
  const s = v.trim()
  if (/^[[{]/.test(s) && parsesAsJson(s)) return readList(JSON.parse(s))
  return s
    .split(',')
    .map(x => x.trim())
    .filter(Boolean)
}

function readCondition(c: Json): ConditionDraft {
  const o = isObject(c) ? c : {}
  return {
    id: nextId(),
    path: str(o.path),
    mode: str(o.mode) || 'full',
    valueText: valueToText(o.value),
    invert: o.invert === true,
    passMissing: o.pass_missing_key === true
  }
}

/**
 * An operation's conditions: an array, or an object of path → value (each an `equals` condition), as the server reads both.
 * The server keeps an object's keys as written, a blank one included, so a blank key stays a condition here too, with an
 * empty path that the checks below flag rather than one dropped on the next save.
 */
function readConditions(v: Json): ConditionDraft[] {
  if (Array.isArray(v)) return v.map(readCondition)
  if (!isObject(v)) return []
  return Object.entries(v).map(([path, value]) => readCondition({ path: path.trim() ? path : '', mode: 'full', value }))
}

const KNOWN_KEYS = new Set(['description', 'mode', 'path', 'from', 'to', 'value', 'keep_origin', 'logic', 'conditions'])

export function newOperation(seed: Partial<Record<string, Json>> = { mode: 'set' }): OperationDraft {
  return readOperation(seed)
}

export function readOperation(raw: Json): OperationDraft {
  const o = isObject(raw) ? raw : {}
  const mode = str(o.mode)
  const kind = MODES[mode]?.value
  const extra = Object.fromEntries(Object.entries(o).filter(([k]) => !KNOWN_KEYS.has(k)))
  return {
    id: nextId(),
    description: str(o.description),
    mode,
    path: str(o.path),
    from: str(o.from),
    to: str(o.to),
    valueText: kind === 'header' && typeof o.value === 'string' ? o.value : kind === 'text' ? str(o.value) : valueToText(o.value),
    list: readList(o.value),
    error: readError(o.value),
    keepOrigin: o.keep_origin === true,
    logic: str(o.logic).toUpperCase() === 'AND' ? 'AND' : 'OR',
    conditions: readConditions(o.conditions),
    extra
  }
}

/** A literal top-level key as a path (sjson escapes). */
const literalPath = (key: string) => key.replace(/[.*?\\]/g, m => `\\${m}`)

/** The drafts of an override: legacy keys first (they apply first), then the operations. */
export function readOverride(v: Override | null | undefined): OperationDraft[] {
  if (!isObject(v)) return []
  const legacy = Object.entries(v)
    .filter(([k]) => k !== 'operations')
    .map(([k, value]) => readOperation({ mode: 'set', path: literalPath(k), value }))
  const ops = Array.isArray(v.operations) ? v.operations.map(readOperation) : []
  return [...legacy, ...ops]
}

function writeValue(d: OperationDraft): Json {
  switch (MODES[d.mode]?.value) {
    case 'json':
      return textToValue(d.valueText)
    case 'text':
      return d.valueText
    case 'header':
      return /^\s*\{/.test(d.valueText) ? textToValue(d.valueText) : d.valueText.trim()
    case 'list':
      return d.list.map(s => s.trim()).filter(Boolean)
    case 'error':
      return writeError(d.error)
  }
  return undefined
}

export function writeOperation(d: OperationDraft): Record<string, Json> {
  const meta = MODES[d.mode]
  const out: Record<string, Json> = {}
  if (d.description.trim()) out.description = d.description.trim()
  out.mode = d.mode
  if (meta?.path && (d.path.trim() || !meta.pathOptional)) out.path = d.path.trim()
  if (meta?.fromTo) {
    out.from = meta.fromTo === 'text' ? d.from : d.from.trim()
    if (d.to || !meta.toOptional) out.to = meta.fromTo === 'text' ? d.to : d.to.trim()
  }
  if (meta?.value) out.value = writeValue(d)
  if (meta?.keepOrigin && d.keepOrigin) out.keep_origin = true
  if (d.conditions.length) {
    out.logic = d.logic
    out.conditions = d.conditions.map(c => {
      const cond: Record<string, Json> = { path: c.path.trim(), mode: c.mode, value: textToValue(c.valueText) }
      if (c.invert) cond.invert = true
      if (c.passMissing) cond.pass_missing_key = true
      return cond
    })
  }
  // An unknown mode keeps what it came with.
  if (!meta) Object.assign(out, pick(d))
  return { ...out, ...d.extra }
}

function pick(d: OperationDraft): Record<string, Json> {
  const out: Record<string, Json> = {}
  if (d.path) out.path = d.path
  if (d.from) out.from = d.from
  if (d.to) out.to = d.to
  if (d.valueText) out.value = textToValue(d.valueText)
  return out
}

/** The override to save: null when there is nothing to do. */
export function writeOverride(drafts: OperationDraft[]): Override | null {
  return drafts.length ? { operations: drafts.map(writeOperation) } : null
}

/** Parses JSON text of an override: the value, or why it is not one (`object`, `operations`). */
export function parseOverride(text: string): JsonParse<Override> {
  const parsed = parseJsonObject(text)
  const ops = 'value' in parsed ? parsed.value?.operations : undefined
  return ops === undefined || (Array.isArray(ops) && ops.every(isObject)) ? parsed : { error: 'operations' }
}

export const operationCount = (v: Override | null | undefined) => readOverride(v).length

// ---------------------------------------------------------------- checks

export interface Problem {
  /** The draft field: mode, path, from, to, value, description, or `conditions.<i>.<field>`. */
  field: string
  code:
    | 'required'
    | 'unsupported'
    | 'invalid_json'
    | 'invalid_regex'
    | 'regex_string'
    | 'invalid_header'
    | 'invalid_prefix'
    | 'invalid_prune'
    | 'status'
    | 'too_long'
  detail?: string
}

/** An HTTP field name (RFC 9110 token). */
const HEADER_NAME = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/

function regexError(pattern: string): string {
  try {
    new RegExp(pattern)
    return ''
  } catch (e) {
    return (e as Error).message
  }
}

/** A regex must be a string that compiles (here JavaScript's syntax; the server compiles Go's). */
function regexProblems(field: string, pattern: Json): Problem[] {
  if (typeof pattern !== 'string') return [{ field, code: 'regex_string' }]
  const detail = regexError(pattern)
  return detail ? [{ field, code: 'invalid_regex', detail }] : []
}

/** A side of sync_fields: `json:` or `body:` and a path, `header:` and a name, or a bare path. */
function syncProblems(field: string, spec: string): Problem[] {
  const s = spec.trim()
  const at = s.indexOf(':')
  if (at < 0) return []
  const kind = s.slice(0, at).trim().toLowerCase()
  const key = s.slice(at + 1).trim()
  if (!['json', 'body', 'header'].includes(kind)) return [{ field, code: 'invalid_prefix', detail: kind }]
  if (!key) return [{ field, code: 'required' }]
  return kind === 'header' && !HEADER_NAME.test(key) ? [{ field, code: 'invalid_header', detail: key }] : []
}

function conditionProblems(c: ConditionDraft, at: string): Problem[] {
  const out: Problem[] = c.path.trim() ? [] : [{ field: `${at}.path`, code: 'required' }]
  if (c.mode === 'regex') return [...out, ...regexProblems(`${at}.value`, textToValue(c.valueText))]
  const detail = brokenJson(c.valueText)
  return detail ? [...out, { field: `${at}.value`, code: 'invalid_json', detail }] : out
}

/** What the server would refuse in a draft (the server's check stays the authority). */
export function validateOperation(d: OperationDraft): Problem[] {
  const meta = MODES[d.mode]
  if (!meta) return [{ field: 'mode', code: d.mode ? 'unsupported' : 'required', detail: d.mode }]
  const out: Problem[] = []
  const need = (field: keyof OperationDraft & string, value: string) => !value.trim() && out.push({ field, code: 'required' })
  const header = (field: string, value: string) =>
    value.trim() && !HEADER_NAME.test(value.trim()) && out.push({ field, code: 'invalid_header', detail: value })
  if (d.description.length > MAX_DESCRIPTION) out.push({ field: 'description', code: 'too_long' })
  if (meta.path && !meta.pathOptional) need('path', d.path)
  if (meta.path === 'header') header('path', d.path)
  if (meta.fromTo) {
    need('from', d.from)
    if (!meta.toOptional) need('to', d.to)
  }
  if (meta.fromTo === 'header') {
    header('from', d.from)
    header('to', d.to)
  }
  if (meta.fromTo === 'sync') out.push(...syncProblems('from', d.from), ...syncProblems('to', d.to))
  if (d.mode === 'regex_replace' && d.from) out.push(...regexProblems('from', d.from))
  out.push(...validateValue(d, meta))
  d.conditions.forEach((c, i) => out.push(...conditionProblems(c, `conditions.${i}`)))
  return out
}

function validateValue(d: OperationDraft, meta: ModeMeta): Problem[] {
  if (meta.valueRequired && !d.valueText.trim()) return [{ field: 'value', code: 'required' }]
  switch (meta.value) {
    case 'json':
    case 'header': {
      const detail = brokenJson(d.valueText)
      if (detail) return [{ field: 'value', code: 'invalid_json', detail }]
      // prune_objects takes a type or an object {logic, recursive, conditions, where, type}.
      const v = textToValue(d.valueText)
      return d.mode === 'prune_objects' && typeof v !== 'string' && !isObject(v) ? [{ field: 'value', code: 'invalid_prune' }] : []
    }
    case 'list':
      return d.list.length
        ? d.list.filter(h => !HEADER_NAME.test(h)).map(h => ({ field: 'value', code: 'invalid_header' as const, detail: h }))
        : [{ field: 'value', code: 'required' }]
    case 'error': {
      const out: Problem[] = []
      if (!d.error.message.trim()) out.push({ field: 'error.message', code: 'required' })
      if (!Number.isInteger(d.error.status_code) || d.error.status_code < 100 || d.error.status_code > 599)
        out.push({ field: 'error.status_code', code: 'status' })
      return out
    }
  }
  return []
}

// ---------------------------------------------------------------- list operations

export function moveDraft(list: OperationDraft[], i: number, by: -1 | 1): OperationDraft[] {
  const j = i + by
  if (j < 0 || j >= list.length) return list
  const next = [...list]
  ;[next[i], next[j]] = [next[j], next[i]]
  return next
}

export function duplicateDraft(list: OperationDraft[], i: number): OperationDraft[] {
  const copy = readOperation(writeOperation(list[i]))
  return [...list.slice(0, i + 1), copy, ...list.slice(i + 1)]
}

export const removeDraft = (list: OperationDraft[], i: number) => list.filter((_, n) => n !== i)

// ---------------------------------------------------------------- templates

export interface Template {
  key: string
  op: Record<string, Json>
}

/** Starting points the "add from template" menu offers. */
export const TEMPLATES: Template[] = [
  { key: 'setParam', op: { mode: 'set', path: 'temperature', value: 0.7 } },
  { key: 'deleteParam', op: { mode: 'delete', path: 'top_p' } },
  {
    key: 'byModel',
    op: { mode: 'set', path: 'max_tokens', value: 8192, logic: 'OR', conditions: [{ path: 'model', mode: 'prefix', value: 'claude-' }] }
  },
  { key: 'syncSession', op: { mode: 'sync_fields', from: 'header:session_id', to: 'json:prompt_cache_key' } },
  { key: 'pruneTools', op: { mode: 'prune_objects', path: 'tools', value: { type: 'web_search_20250305' } } },
  { key: 'betaToken', op: { mode: 'set_header', path: 'anthropic-beta', value: { 'context-1m-2025-08-07': null } } },
  { key: 'setHeader', op: { mode: 'set_header', path: 'x-custom-header', value: 'value' } },
  {
    key: 'blockModel',
    op: {
      mode: 'return_error',
      value: { message: 'this model is not available on this account', status_code: 400 },
      logic: 'OR',
      conditions: [{ path: 'model', mode: 'contains', value: 'opus' }]
    }
  }
]
