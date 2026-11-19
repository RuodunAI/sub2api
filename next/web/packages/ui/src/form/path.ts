// Dotted paths ('a.b.0.c') into a form value. Rule keys may hold '*' for
// every item of an array (or key of an object): 'items.*.url'.
import { getPath } from '../value'

export { getPath }
export { toFieldPath as toPath } from '@sub2api/host'

const isIndex = (k: string) => /^\d+$/.test(k)

/**
 * Writes `value` at `path` without touching `obj`: every container on the
 * way is copied, everything else is shared. `undefined` removes an object
 * key (the key is not sent). Missing containers are created (an array when
 * the next step is an index).
 */
export function setPath<T>(obj: T, path: string, value: unknown): T {
  return write(obj, path.split('.'), value) as T
}

function write(node: unknown, [key, ...rest]: string[], value: unknown): unknown {
  const copy: any = Array.isArray(node) ? [...node] : node && typeof node === 'object' ? { ...node } : isIndex(key) ? [] : {}
  const next = rest.length ? write(copy[key], rest, value) : value
  if (next === undefined && !Array.isArray(copy)) delete copy[key]
  else copy[key] = next
  return copy
}

/** The concrete paths `pattern` names in `model` ('*' expanded over the current items). */
export function expandPath(pattern: string, model: unknown): string[] {
  const star = pattern.indexOf('*')
  if (star < 0) return [pattern]
  const head = pattern.slice(0, star).replace(/\.$/, '')
  const tail = pattern.slice(star + 1)
  const list = head ? getPath(model, head) : model
  if (!list || typeof list !== 'object') return []
  return Object.keys(list).flatMap((k) => expandPath(`${head ? head + '.' : ''}${k}${tail}`, model))
}

/** `pattern` (with '*' steps) names `path`. */
export function matchPath(pattern: string, path: string): boolean {
  const a = pattern.split('.')
  const b = path.split('.')
  return a.length === b.length && a.every((k, i) => k === '*' || k === b[i])
}

/** One path holds the other (equal, ancestor or descendant): a write to one changes the other. */
export function overlaps(a: string, b: string): boolean {
  return a === b || a.startsWith(b + '.') || b.startsWith(a + '.')
}
