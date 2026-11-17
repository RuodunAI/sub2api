// The JSON text of an override editor (CONTRACTS §84, §108): the object it
// stands for, or why it is not one. Free of Vue: the mock imports models
// that use it.

/** Why JSON text is not an override: `syntax` (with the parser's message as detail), `object`, or a model's own code. */
export interface JsonError {
  error: string
  detail?: string
  key?: string
}

export type JsonParse<T> = { value: T | null } | JsonError

export const isJsonObject = (x: unknown): x is Record<string, unknown> => !!x && typeof x === 'object' && !Array.isArray(x)

/** Parses JSON text that must be an object; empty text, null and {} are null (nothing set). */
export function parseJsonObject(text: string): JsonParse<Record<string, unknown>> {
  if (!text.trim()) return { value: null }
  let v: unknown
  try {
    v = JSON.parse(text)
  } catch (e) {
    return { error: 'syntax', detail: (e as Error).message }
  }
  if (v === null) return { value: null }
  if (!isJsonObject(v)) return { error: 'object' }
  return { value: Object.keys(v).length ? v : null }
}
