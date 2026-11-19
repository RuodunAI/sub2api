// Form state over a v-model value: rules, local errors, server field
// errors, touched fields. The value stays the parent's (model / update):
// useForm only ever writes a patch of the current value, so keys neither
// the rules nor the page know (hidden credentials, kept secrets) go back
// unchanged.
//
// Triggers: a field is validated on blur (reported by its control through
// SField / SSettingRow `prop`), and on every change once touched (blurred,
// or after validate()). A rule with `trigger` runs only on those events.
// Changes are the writes through set(). validate() runs every rule.
//
// Errors: per path the local message wins over the server's; a write to a
// path drops the server error of that path (and the paths inside or around
// it). Errors no SField / SSettingRow claims are listed in `unclaimed`.
import { computed, reactive, toRaw, watch, type ComputedRef } from 'vue'
import { useUiT } from '../i18n'
import { expandPath, getPath, matchPath, overlaps, setPath, toPath } from './path'
import { checkValue, requires, toList, runsOn, type Rule, type Rules, type Trigger } from './rules'

type Model = Record<string, unknown>

export interface UseFormOptions<M extends Model> {
  /** The value (v-model of the form): read on every access. */
  model: () => M
  /** Writes a whole new value (emit('update:modelValue', v)). */
  update: (next: M) => void
  rules: Rules<M> | (() => Rules<M>)
  /** Server field errors (path -> message), e.g. the native form's `errors` prop. */
  serverErrors?: () => Record<string, string> | undefined
  /**
   * Filled into absent keys (nested objects too) of each value the parent
   * hands in (create mode, loaded settings); never overwrites a present key
   * and never refills what the user clears.
   */
  defaults?: Partial<M>
}

export interface FormApi<M extends Model = Model> {
  /** The whole current value (with this tick's writes). */
  value(): M
  /** Value at path. */
  get<T = any>(path: string): T
  /** Writes one path: a patch of the current value, every other key kept as it is (hidden keys too). */
  set(path: string, value: unknown): void
  /** Runs every rule; true when nothing failed. Marks every field touched. Never rejects. */
  validate(): Promise<boolean>
  validateField(paths: string | string[]): Promise<boolean>
  /** Drops the local and server messages (of these paths, else all) and their touched state. */
  clearValidate(paths?: string | string[]): void
  /** The message shown at path: the local error, else the server's. */
  error(path: string): string | undefined
  /** Errors whose path no SField / SSettingRow claims (shown above the form). */
  unclaimed: ComputedRef<string[]>
  /** Whether a rule makes path required (SField's star). */
  required(path: string): boolean
  readonly errors: Readonly<Record<string, string>>
  /** A control at path lost focus (SField / SSettingRow `prop`). */
  touch(path: string, kind: Trigger): void
  /** Shows path's errors at a frame labelled `label`; returns the release. */
  claim(path: string, label?: () => string | undefined): () => void
}

const isPlain = (v: unknown): v is Model => !!v && typeof v === 'object' && !Array.isArray(v)

/** `value` with the absent keys of `defaults` filled (the same object when nothing was missing). */
function fillMissing(value: Model, defaults: Model): Model {
  let out = value
  for (const [k, d] of Object.entries(defaults)) {
    const cur = value[k]
    const next = cur === undefined ? structuredClone(toRaw(d)) : isPlain(cur) && isPlain(d) ? fillMissing(cur, d) : cur
    if (next !== cur) out = { ...out, [k]: next }
  }
  return out
}

export function useForm<M extends Model>(opts: UseFormOptions<M>): FormApi<M> {
  const t = useUiT()
  const local = reactive<Record<string, string>>({})
  const touched = reactive(new Set<string>())
  const cleared = reactive(new Set<string>())
  const claims = reactive(new Map<string, { n: number; label?: () => string | undefined }>())
  const runs = new Map<string, { seq: number; abort: AbortController; done: Promise<boolean> }>()
  let seq = 0
  // The last value written and not yet handed back through `model`, so
  // writes in one tick build on each other.
  let draft: M | undefined
  let emitted: M | undefined

  const current = () => draft ?? opts.model() ?? ({} as M)
  const ruleTable = () => (typeof opts.rules === 'function' ? opts.rules() : opts.rules)

  function write(next: M) {
    draft = emitted = next
    opts.update(next)
    Promise.resolve().then(() => (draft = undefined))
  }

  // Defaults: once per value the parent hands in (not the ones we emitted).
  watch(
    () => toRaw(opts.model()),
    (v) => {
      if (!opts.defaults || (emitted && v === toRaw(emitted))) return
      const filled = fillMissing(v ?? {}, opts.defaults as Model) as M
      if (filled !== v) write(filled)
    },
    { immediate: true }
  )
  // A new set of server errors shows in full again.
  watch(
    () => opts.serverErrors?.(),
    () => cleared.clear()
  )

  /** Rules of a concrete path whose `when` holds. */
  function rulesOf(path: string): Rule<any, M>[] {
    const model = current()
    return Object.entries(ruleTable()).flatMap(([pattern, r]) => (matchPath(pattern, path) ? toList(r).filter((x) => !x.when || x.when(model)) : []))
  }

  /** Every concrete path the rules name in the current value. */
  function allPaths(): string[] {
    return [...new Set(Object.keys(ruleTable()).flatMap((p) => expandPath(p, current())))]
  }

  function run(path: string, trigger?: Trigger): Promise<boolean> {
    const rules = rulesOf(path)
    const picked = rules.filter((r) => runsOn(r, trigger, touched.has(path)))
    if (!rules.length) delete local[path]
    if (!picked.length) return Promise.resolve(!local[path])
    runs.get(path)?.abort.abort()
    const abort = new AbortController()
    const my = ++seq
    const model = current()
    const label = claims.get(path)?.label?.()
    const done = checkValue(picked, getPath(model, path), { path, model, label, signal: abort.signal }, t).then((msg) => {
      const latest = runs.get(path)
      // A newer run of the field decides, or the run was cleared: this result is stale.
      if (latest?.seq !== my) return latest ? latest.done : !local[path]
      runs.delete(path)
      if (msg) local[path] = msg
      else delete local[path]
      return !msg
    })
    runs.set(path, { seq: my, abort, done })
    return done
  }

    const serverErrors = computed(() => {
    const out: Record<string, string> = {}
    for (const [k, v] of Object.entries(opts.serverErrors?.() ?? {})) {
      const p = toPath(k)
      if (!cleared.has(p)) out[p] = v
    }
    return out
  })
  const errors = computed<Record<string, string>>(() => ({ ...serverErrors.value, ...local }))

  const form: FormApi<M> = {
    value: current,
    get: (path) => getPath(current(), path),
    set(path, value) {
      write(setPath(current(), path, value))
      for (const p of Object.keys(serverErrors.value)) if (overlaps(p, path)) cleared.add(p)
      for (const p of allPaths()) if (overlaps(p, path)) void run(p, 'change')
      // Errors elsewhere go once a `when` switched their rules off.
      for (const p of Object.keys(local)) if (!overlaps(p, path) && !rulesOf(p).length) delete local[p]
    },
    async validate() {
      const paths = allPaths()
      for (const p of Object.keys(local)) if (!paths.includes(p)) delete local[p]
      for (const p of paths) touched.add(p)
      const results = await Promise.all(paths.map((p) => run(p)))
      return results.every(Boolean)
    },
    async validateField(paths) {
      const results = await Promise.all(toList(paths).flatMap((p) => expandPath(p, current())).map((p) => run(p)))
      return results.every(Boolean)
    },
    clearValidate(paths) {
      const only = paths === undefined ? undefined : toList(paths)
      const hit = (p: string) => !only || only.some((o) => matchPath(o, p))
      for (const p of Object.keys(local)) if (hit(p)) delete local[p]
      for (const p of Object.keys(serverErrors.value)) if (hit(p)) cleared.add(p)
      for (const p of [...touched]) if (hit(p)) touched.delete(p)
      for (const [p, r] of runs) {
        if (!hit(p)) continue
        r.abort.abort()
        runs.delete(p)
      }
    },
    error: (path) => errors.value[path],
    unclaimed: computed(() => Object.entries(errors.value).flatMap(([p, msg]) => (claims.has(p) ? [] : [msg]))),
    required: (path) => requires(rulesOf(path)),
    get errors() {
      return errors.value
    },
    touch(path, kind) {
      if (kind === 'blur') touched.add(path)
      void run(path, kind)
    },
    claim(path, label) {
      const c = claims.get(path)
      claims.set(path, { n: (c?.n ?? 0) + 1, label: label ?? c?.label })
      return () => {
        const c = claims.get(path)
        if (!c || c.n <= 1) claims.delete(path)
        else claims.set(path, { ...c, n: c.n - 1 })
      }
    }
  }
  return form
}
