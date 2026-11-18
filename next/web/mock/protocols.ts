// Mock of the protocol binding (docs/DESIGN-2026-11-17-PROTOCOL-BINDING.md):
// what an account type serves (native, adapted within a platform, converted
// from another platform, count_tokens estimated locally), what an account and
// a group serve with their `protocols` / `model_protocols`, and the field
// errors of the two. The rules mirror the core's; the grades are made up.
import { activePlatforms } from './platforms'

interface TypeEndpoint {
  method: string
  path: string
  protocol: string
  platform: string
  native: boolean
  grade: string
  via?: string[]
}
interface Rule {
  models: string[]
  protocols: string[]
}
interface Served {
  protocol: string
  via: 'native' | 'adapted' | 'converted'
  grade: string
  hops?: string[]
  /** null: every model the rules do not bind to other protocols. */
  models: string[] | null
}

const REST = '*'
/** A count_tokens protocol follows its family's generation protocol. */
const COUNT_TOKENS_FAMILY: Record<string, string> = { 'anthropic.count_tokens': 'anthropic.messages', 'gemini.count_tokens': 'gemini.generate' }

/** Protocols the core converts between. */
const TEXT = ['anthropic.messages', 'openai.chat', 'openai.responses', 'gemini.generate', 'gemini.stream_generate']
const platformOf = (protocol: string) => protocol.split('.')[0]

/**
 * The endpoints of GET /account-types for a type supporting `supports`:
 * `subset` (protocols) narrows what it serves natively, the other text
 * endpoints of its platforms are adapted to the first one it has. Text
 * endpoints of other platforms are converted (Anthropic and Gemini reach each
 * other in two hops, through OpenAI chat), and their count_tokens estimated.
 */
export function typeEndpoints(supports: string[], subset?: string[]): TypeEndpoint[] {
  const all = activePlatforms().flatMap((p) => p.endpoints.map((e) => ({ method: e.method, path: e.path, protocol: e.protocol, platform: p.id })))
  const native = all.filter((e) => supports.includes(e.platform) && (!subset || subset.includes(e.protocol)))
  const target = native.find((e) => TEXT.includes(e.protocol))?.protocol
  const out: TypeEndpoint[] = native.map((e) => ({ ...e, native: true, grade: 'native' }))
  for (const e of all) {
    if (native.includes(e) || !target || !TEXT.includes(e.protocol)) continue
    const twoHop = !supports.includes(e.platform) && [platformOf(e.protocol), platformOf(target)].sort().join() === 'anthropic,gemini'
    out.push({ ...e, native: false, grade: supports.includes(e.platform) ? 'good' : twoHop ? 'discouraged' : 'fair', via: twoHop ? [e.protocol, 'openai.chat', target] : [e.protocol, target] })
  }
  const served = new Set(out.map((e) => e.protocol))
  for (const e of all) if (!served.has(e.protocol) && served.has(COUNT_TOKENS_FAMILY[e.protocol])) out.push({ ...e, native: false, grade: 'local' })
  return out.sort((a, b) => Number(b.native) - Number(a.native) || a.platform.localeCompare(b.platform) || a.path.localeCompare(b.path))
}

interface Binding {
  /** The account's model list; empty = every model. */
  models?: string[]
  protocols?: string[] | null
  model_protocols?: Rule[]
}
type TypeView = { platforms: Array<{ id: string }>; endpoints: TypeEndpoint[] }

/** The protocols of a type, each once, with how the type serves it. */
function typeProtocols(at: TypeView): Array<TypeEndpoint & { how: Served['via'] }> {
  const declared = new Set(at.platforms.map((p) => p.id))
  const seen = new Set<string>()
  return at.endpoints
    .filter((e) => !seen.has(e.protocol) && !!seen.add(e.protocol))
    .map((e) => ({ ...e, how: e.native ? 'native' : declared.has(e.platform) ? 'adapted' : 'converted' }))
}

/** The protocols an account can be bound to: no count_tokens, no local estimate. */
const bindable = (at: TypeView) => typeProtocols(at).filter((e) => !(e.protocol in COUNT_TOKENS_FAMILY) && e.grade !== 'local')

/**
 * The models an account serves on a protocol of its type: null for every
 * model no rule lists, else the models of the rules listing it; undefined
 * when it serves none. A count_tokens protocol follows its family.
 */
function modelsOn(a: Binding, protocol: string, own: boolean): string[] | null | undefined {
  const p = COUNT_TOKENS_FAMILY[protocol] ?? protocol
  if (own && a.protocols && !a.protocols.includes(p)) return undefined
  // A model no rule lists is served on the own protocols, never converted.
  let others = own
  const models: string[] = []
  for (const r of a.model_protocols ?? []) {
    const listed = r.protocols.includes(p)
    if (r.models.includes(REST)) others = listed
    else if (listed) models.push(...r.models.filter((m) => !a.models?.length || a.models.includes(m)))
  }
  if (others) return null
  return models.length ? models.sort() : undefined
}

/** served_protocols of GET /accounts/:id: the protocols of the type the account serves now, with the models of each. */
export function servedProtocols(a: Binding, at: TypeView): Served[] {
  const out: Served[] = []
  for (const e of typeProtocols(at)) {
    const models = modelsOn(a, e.protocol, e.how !== 'converted')
    if (models !== undefined) out.push({ protocol: e.protocol, via: e.how, grade: e.grade, hops: e.via, models })
  }
  return out
}

/** served_protocols of GET /groups/:id: each protocol with the number of member accounts serving it. */
export function groupServedProtocols(members: Served[][]): Array<{ protocol: string; accounts: number }> {
  const counts = new Map<string, number>()
  for (const served of members) for (const s of served) counts.set(s.protocol, (counts.get(s.protocol) ?? 0) + 1)
  return [...counts].map(([protocol, accounts]) => ({ protocol, accounts })).sort((a, b) => a.protocol.localeCompare(b.protocol))
}

type FieldError = { field: string; code: string; message: string }

/** Field errors of `protocols` / `model_protocols` (design §4), for the account the request would leave. */
export function protocolFieldErrors(b: Binding, at: TypeView): FieldError[] {
  const all = bindable(at)
  const own = new Set(all.filter((e) => e.how !== 'converted').map((e) => e.protocol))
  const cross = new Set(all.filter((e) => e.how === 'converted').map((e) => e.protocol))
  const out: FieldError[] = []
  const bad = (field: string, code: string, message: string) => out.push({ field, code, message })
  ;(b.protocols ?? []).forEach((p, i) => own.has(p) || bad(`protocols[${i}]`, 'invalid', 'not a protocol of the account type\'s own platforms'))
  const rules = b.model_protocols ?? []
  if (rules.length > 50) bad('model_protocols', 'too_many', 'at most 50 entries')
  const seen = new Set<string>()
  rules.forEach((r, i) => {
    if (!r.protocols?.length) bad(`model_protocols[${i}].protocols`, 'required', 'pick at least one protocol')
    if ((r.models?.length ?? 0) > 500) bad(`model_protocols[${i}].models`, 'too_many', 'at most 500 models')
    ;(r.models ?? []).forEach((m, j) => {
      if (m === REST && r.models.length > 1) bad(`model_protocols[${i}].models[${j}]`, 'invalid', '"*" must be an entry of its own')
      if (seen.has(m)) bad(`model_protocols[${i}].models[${j}]`, 'duplicate', 'model listed twice')
      seen.add(m)
    })
    ;(r.protocols ?? []).forEach((p, j) => own.has(p) || cross.has(p) || bad(`model_protocols[${i}].protocols[${j}]`, 'invalid', 'the account type cannot serve this protocol'))
  })
  if (b.protocols && !b.protocols.length && !rules.some((r) => r.protocols?.some((p) => cross.has(p)))) bad('protocols', 'no_protocol', 'the account opens no protocol')
  return out
}
