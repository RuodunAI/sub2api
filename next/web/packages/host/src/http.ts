// Session-aware HTTP client for the console API (API_BASE, see ./routes).
//
// Contract (docs/CONTRACTS.md §3):
//   success  -> {"data": ...}            list -> {"data": [...], "page": {...}}
//   failure  -> {"error": {code, message, details}}
// 401 means the session ended (s2s_ sessions renew on the server, CONTRACTS
// §100): the session is cleared and onUnauthenticated sends the console to the
// login page, unless another tab stored a different token meanwhile, which the
// request is retried with once. A tab whose session another tab cleared (a
// logout there, or a 401) follows it to the login page. Any other status,
// 5xx included, leaves the session alone. fetchWithSession applies the same
// policy to a raw fetch (an upload, a stream, a file).
// 403 verification_required asks onVerificationRequired for a step-up proof
// and replays the request once with X-Verify-Proof (CONTRACTS §80.2).

import { API_BASE } from './routes'

export interface FieldError {
  field: string
  code?: string
  message: string
}

export interface ApiErrorBody {
  code: string
  message: string
  details?: Record<string, any> & { fields?: FieldError[] }
}

export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly details: Record<string, any>
  /** field path -> message, from details.fields of invalid_argument errors */
  readonly fields: Record<string, string>
  /**
   * field path -> machine code of the same field errors ("invalid" when the
   * server gave none). Pages translate known codes (fieldErrorText) instead of
   * showing the server's message, which may be English only.
   */
  readonly fieldCodes: Record<string, string>

  constructor(status: number, body: ApiErrorBody) {
    super(body.message || body.code)
    this.name = 'ApiError'
    this.status = status
    this.code = body.code || 'internal'
    this.details = body.details || {}
    const fields: Record<string, string> = {}
    const fieldCodes: Record<string, string> = {}
    for (const f of body.details?.fields || []) {
      if (!f || !f.field) continue
      fields[f.field] = localizedText(f.message) || f.code || 'invalid'
      fieldCodes[f.field] = f.code || 'invalid'
    }
    this.fields = fields
    this.fieldCodes = fieldCodes
  }
}

/** Field messages may be plain strings or {en, zh} objects (e.g. price expressions). */
function localizedText(v: unknown): string {
  if (typeof v === 'string') return v
  if (v && typeof v === 'object') {
    const m = v as Record<string, unknown>
    const loc = config.locale?.() || 'en'
    const s = m[loc] ?? m.en ?? Object.values(m)[0]
    return typeof s === 'string' ? s : ''
  }
  return ''
}

export function isApiError(e: unknown): e is ApiError {
  return e instanceof ApiError
}

export interface PageInfo {
  page: number
  page_size: number
  total: number
}

export interface ListResult<T> {
  items: T[]
  page: PageInfo
  /**
   * True when the response body carried its own `page` object, i.e. the route
   * really paginates and `page`/`page_size` mean something to it. False when
   * `page` below was synthesised from the returned array.
   *
   * Callers that need to know cannot recover this from `page` alone: a
   * synthesised page is indistinguishable from a real last-and-only page, and
   * comparing `page.total > items.length` answers "is there more than one
   * page", not "does the server paginate" — the two differ for every route
   * whose data happens to fit in one page today.
   */
  paged: boolean
}

export type Query = Record<string, string | number | boolean | null | undefined | Array<string | number>>

export interface RequestOptions {
  query?: Query
  body?: unknown
  headers?: Record<string, string>
  signal?: AbortSignal
  /**
   * Time limit in ms of each send (the first one, a resend with the token
   * another tab stored, the replay with a step-up proof). Unlike a timeout signal
   * it does not run while the user verifies, so a slow verification neither
   * closes the dialog nor shortens the replay.
   */
  timeoutMs?: number
  /** Do not attach the access token, and leave a 401 to the caller. */
  anonymous?: boolean
  /** Headers from the final successful response, after auth retries. */
  onSuccessHeaders?: (headers: Headers) => void
}

// ------------------------------------------------------------------ session

export interface Session {
  access_token: string
  /** @deprecated Never set: sessions renew on the server (CONTRACTS §100). Removed in host UI 4.0. */
  refresh_token?: string
  /** @deprecated Never set: sessions renew on the server (CONTRACTS §100). Removed in host UI 4.0. */
  expires_at?: number
}

const SESSION_KEY = 's2a.session'
const listeners = new Set<(s: Session | null) => void>()

function readSession(): Session | null {
  try {
    const raw = localStorage.getItem(SESSION_KEY)
    return raw ? (JSON.parse(raw) as Session) : null
  } catch {
    return null
  }
}

let current: Session | null = typeof localStorage !== 'undefined' ? readSession() : null

export const session = {
  get(): Session | null {
    return current
  },
  set(s: Session | null) {
    current = s
    try {
      if (s) localStorage.setItem(SESSION_KEY, JSON.stringify(s))
      else localStorage.removeItem(SESSION_KEY)
    } catch {
      /* storage unavailable */
    }
    listeners.forEach((fn) => fn(s))
  },
  /** Store a login or step-up response carrying a new access token. */
  fromTokenResponse(r: { access_token: string }) {
    session.set({ access_token: r.access_token })
  },
  clear() {
    session.set(null)
  },
  onChange(fn: (s: Session | null) => void): () => void {
    listeners.add(fn)
    return () => listeners.delete(fn)
  }
}

// ------------------------------------------------------------------ config

/**
 * Why the session ended: "expired" when the server no longer accepts the
 * signed-in session (it idled out, reached its maximum age or was revoked),
 * "unauthenticated" when there was none, or another tab signed out.
 */
export type UnauthenticatedReason = 'expired' | 'unauthenticated'

/** details of a 403 verification_required: what to verify for (CONTRACTS §80.2). */
export interface VerificationChallenge {
  scope: string
  /** The operation, "METHOD /api/v1/path"; sent back as is with the verification. */
  context: string
  password_required?: boolean
}

/** Error code of a request whose step-up verification the user dismissed. */
export const VERIFICATION_CANCELLED = 'verification_cancelled'

export interface HttpConfig {
  baseURL: string
  /** Called when the session is gone. */
  onUnauthenticated?: (reason: UnauthenticatedReason) => void
  /**
   * Called on 403 verification_required: resolves the proof the request is
   * replayed with, or null when the user cancels (the request then fails
   * with VERIFICATION_CANCELLED). `signal` is the request's: once it aborts
   * the verification is moot. Unset, the 403 is the request's error.
   */
  onVerificationRequired?: (challenge: VerificationChallenge, signal?: AbortSignal) => Promise<string | null>
  locale?: () => string
  /**
   * Called with X-Authz-Version of every authenticated response (CONTRACTS
   * §5.2): the authz version the answering node knew when it authenticated.
   */
  onAuthzVersion?: (version: number) => void
  /** Called with the error of every 403 response before it is thrown. */
  onForbidden?: (error: ApiError) => void
}

/** Response header carrying the node's authz version (CONTRACTS §5.2). */
export const AUTHZ_VERSION_HEADER = 'X-Authz-Version'

const config: HttpConfig = { baseURL: API_BASE }

export function configureHttp(c: Partial<HttpConfig>) {
  Object.assign(config, c)
}

// Keep tabs in sync: a tab follows another that signed out, or whose session
// ended, to the login page (both share the one session in localStorage).
if (typeof window !== 'undefined') {
  window.addEventListener('storage', (e) => {
    if (e.key !== SESSION_KEY) return
    const had = !!current
    current = readSession()
    listeners.forEach((fn) => fn(current))
    if (had && !current) config.onUnauthenticated?.('unauthenticated')
  })
}

/**
 * The console API base every request is built on. Use it instead of writing
 * "/api/v1" again: it is the one place that can change.
 */
export function apiBase(): string {
  return config.baseURL
}

function buildURL(path: string, query?: Query): string {
  const base = /^https?:\/\//.test(path) || path.startsWith(config.baseURL + '/') ? '' : config.baseURL
  let url = base + (path.startsWith('/') ? path : '/' + path)
  if (query) {
    const qs = new URLSearchParams()
    for (const [k, v] of Object.entries(query)) {
      if (v === undefined || v === null || v === '') continue
      if (Array.isArray(v)) v.forEach((x) => qs.append(k, String(x)))
      else qs.append(k, String(v))
    }
    const s = qs.toString()
    if (s) url += (url.includes('?') ? '&' : '?') + s
  }
  return url
}

/** Re-reads the session written by other tabs (the storage event may not have fired yet). */
function syncSession(): Session | null {
  if (typeof localStorage === 'undefined') return current
  let raw: string | null
  try {
    raw = localStorage.getItem(SESSION_KEY)
  } catch {
    return current // storage unavailable: keep the in-memory session
  }
  const stored = raw ? readSession() : null
  if (stored?.access_token !== current?.access_token) {
    current = stored
    listeners.forEach((fn) => fn(current))
  }
  return current
}

/**
 * Ends the session after a 401 of a request sent with token, unless the
 * session holds another token by now (a newer login): that one stays.
 */
function sessionEnded(token: string | undefined) {
  const s = session.get()
  if (s && s.access_token !== token) return
  if (s) session.clear()
  config.onUnauthenticated?.(s ? 'expired' : 'unauthenticated')
}

/** A fetch init whose headers are a plain record. */
export type SessionFetchInit = Omit<RequestInit, 'headers'> & { headers?: Record<string, string> }

/** fetch with the console's headers (token when given, locale); reports the authz version. */
async function fetchAs(url: string, init: SessionFetchInit, token: string | undefined): Promise<Response> {
  const headers = { ...init.headers }
  if (token) headers.Authorization = 'Bearer ' + token
  const loc = config.locale?.()
  if (loc) headers['Accept-Language'] = loc
  const res = await fetch(url, { ...init, headers })
  const version = Number(res.headers.get(AUTHZ_VERSION_HEADER) || NaN)
  if (Number.isFinite(version)) config.onAuthzVersion?.(version)
  return res
}

/**
 * Sends with the session's token. A 401 is sent once more when another tab
 * stored a different token meanwhile (its storage event may not have arrived
 * yet); a 401 of the session's token ends the session.
 */
async function sendWithSession(send: (token: string | undefined) => Promise<Response>): Promise<Response> {
  let token = session.get()?.access_token
  let res = await send(token)
  if (res.status !== 401) return res
  const s = syncSession()
  if (s && s.access_token !== token) {
    token = s.access_token
    res = await send(token)
  }
  if (res.status === 401) sessionEnded(token)
  return res
}

/**
 * fetch of a console API path (as request takes it) with the session token,
 * for what the JSON client cannot carry: a raw body, a stream, a file. A 401
 * follows the client's policy (resent once with a token another tab stored,
 * otherwise the session ends). The response, a failed one included, is the
 * caller's to read.
 */
export function fetchWithSession(path: string, init: SessionFetchInit = {}): Promise<Response> {
  const url = buildURL(path)
  return sendWithSession((token) => fetchAs(url, init, token))
}

/** Low-level request returning the parsed JSON envelope (or null for 204). */
export async function requestRaw(method: string, path: string, opts: RequestOptions = {}): Promise<any> {
  const headers: Record<string, string> = { Accept: 'application/json', ...opts.headers }
  let body: BodyInit | undefined
  if (typeof FormData !== 'undefined' && opts.body instanceof FormData) body = opts.body
  else if (opts.body !== undefined) {
    headers['Content-Type'] = 'application/json'
    body = JSON.stringify(opts.body)
  }
  const url = buildURL(path, opts.query)
  /** One attempt, with extra headers (a step-up proof); each has its own time limit. */
  const send = (extra?: Record<string, string>): Promise<Response> => {
    const init = () => ({ method, headers: { ...headers, ...extra }, body, signal: sendSignal(opts) })
    return opts.anonymous ? fetchAs(url, init(), undefined) : sendWithSession((token) => fetchAs(url, init(), token))
  }

  let res = await send()
  let json = await safeJSON(res)
  const challenge = verificationChallenge(res, json, opts)
  if (challenge) {
    const proof = await config.onVerificationRequired!(challenge, opts.signal)
    if (opts.signal?.aborted) throw new DOMException('request aborted', 'AbortError')
    if (!proof) throw new ApiError(403, { code: VERIFICATION_CANCELLED, message: '', details: json.error.details })
    res = await send({ 'X-Verify-Proof': proof })
    json = await safeJSON(res)
  }
  if (res.status === 204) { opts.onSuccessHeaders?.(new Headers(res.headers)); return null }
  if (!res.ok) {
    const body: ApiErrorBody = json?.error || { code: statusCode(res.status), message: res.statusText || 'request failed' }
    const err = new ApiError(res.status, body)
    if (res.status === 403) config.onForbidden?.(err)
    throw err
  }
  opts.onSuccessHeaders?.(new Headers(res.headers))
  return json
}

/** The signal of one send: the request's, bounded by its timeoutMs. */
function sendSignal(opts: RequestOptions): AbortSignal | undefined {
  if (!opts.timeoutMs) return opts.signal
  const timeout = AbortSignal.timeout(opts.timeoutMs)
  return opts.signal ? AbortSignal.any([opts.signal, timeout]) : timeout
}

/** The step-up a failed response asks for, when the console handles step-ups. */
function verificationChallenge(res: Response, json: any, opts: RequestOptions): VerificationChallenge | null {
  if (res.status !== 403 || json?.error?.code !== 'verification_required') return null
  if (opts.anonymous || !config.onVerificationRequired) return null
  const d = json.error.details || {}
  return typeof d.scope === 'string' && typeof d.context === 'string' ? { scope: d.scope, context: d.context, password_required: !!d.password_required } : null
}

function statusCode(status: number): string {
  switch (status) {
    case 400:
      return 'invalid_argument'
    case 401:
      return 'unauthenticated'
    case 403:
      return 'permission_denied'
    case 404:
      return 'not_found'
    case 409:
      return 'conflict'
    case 429:
      return 'rate_limited'
    case 503:
      return 'unavailable'
    default:
      return 'internal'
  }
}

async function safeJSON(res: Response): Promise<any> {
  const text = await res.text()
  if (!text) return null
  try {
    return JSON.parse(text)
  } catch {
    return null
  }
}

/** Request returning the `data` member of the envelope. */
export async function request<T = any>(method: string, path: string, opts: RequestOptions = {}): Promise<T> {
  const json = await requestRaw(method, path, opts)
  return (json && 'data' in json ? json.data : json) as T
}

/** Data and headers belong to one final response; authentication stays shared. */
export async function requestWithHeaders<T>(method: string, path: string, opts: RequestOptions = {}): Promise<{ data: T; headers: Headers }> {
  let headers = new Headers()
  const data = await request<T>(method, path, { ...opts, onSuccessHeaders: value => { headers = value; opts.onSuccessHeaders?.(value) } })
  return { data, headers }
}

/** List request returning items and page info. */
export async function requestList<T = any>(path: string, query?: Query, opts: RequestOptions = {}): Promise<ListResult<T>> {
  const json = await requestRaw('GET', path, { ...opts, query })
  const items: T[] = Array.isArray(json?.data) ? json.data : Array.isArray(json) ? json : []
  const envelope = json?.page
  const paged = !!envelope && typeof envelope === 'object' && !Array.isArray(envelope)
  const page: PageInfo = paged ? envelope : { page: 1, page_size: items.length, total: items.length }
  return { items, page, paged }
}

/**
 * Every item of a paginated list, fetched page by page at the core's largest
 * page size, up to `max` items (unbounded by default). A failed page rejects:
 * a partial list is never returned. A route that caps the page size lower
 * answers with its own `page.page_size`: a full page of that size is not the
 * last one, so the walk ends on a short (or empty) page or at `page.total`.
 */
export async function listAll<T = any>(client: ApiClient, path: string, opts: { query?: Query; max?: number } = {}): Promise<T[]> {
  const PAGE = 200
  const max = opts.max ?? Infinity
  const out: T[] = []
  for (let page = 1; out.length < max; page++) {
    const r = await client.list<T>(path, { ...opts.query, page, page_size: PAGE })
    out.push(...r.items)
    if (!r.paged || r.items.length === 0) break
    const size = Number(r.page.page_size) > 0 ? Math.min(Number(r.page.page_size), PAGE) : PAGE
    const total = Number(r.page.total)
    if (r.items.length < size || (total > 0 && out.length >= total)) break
  }
  return out.slice(0, max)
}

export interface ApiClient {
  get<T = any>(path: string, query?: Query, opts?: RequestOptions): Promise<T>
  list<T = any>(path: string, query?: Query, opts?: RequestOptions): Promise<ListResult<T>>
  post<T = any>(path: string, body?: unknown, opts?: RequestOptions): Promise<T>
  put<T = any>(path: string, body?: unknown, opts?: RequestOptions): Promise<T>
  patch<T = any>(path: string, body?: unknown, opts?: RequestOptions): Promise<T>
  del<T = any>(path: string, query?: Query, opts?: RequestOptions): Promise<T>
  upload<T = any>(path: string, form: FormData, opts?: RequestOptions): Promise<T>
  request<T = any>(method: string, path: string, opts?: RequestOptions): Promise<T>
}

/** Creates a client whose relative paths are joined to prefix (e.g. "/p/guard"). */
export function createClient(prefix = ''): ApiClient {
  const p = (path: string) => {
    if (!prefix) return path
    return prefix.replace(/\/$/, '') + (path.startsWith('/') ? path : '/' + path)
  }
  return {
    get: (path, query, opts) => request('GET', p(path), { ...opts, query }),
    list: (path, query, opts) => requestList(p(path), query, opts),
    post: (path, body, opts) => request('POST', p(path), { ...opts, body }),
    put: (path, body, opts) => request('PUT', p(path), { ...opts, body }),
    patch: (path, body, opts) => request('PATCH', p(path), { ...opts, body }),
    del: (path, query, opts) => request('DELETE', p(path), { ...opts, query }),
    upload: (path, form, opts) => request('POST', p(path), { ...opts, body: form }),
    request: (method, path, opts) => request(method, p(path), opts)
  }
}

/** Console API client rooted at /api/v1. */
export const api: ApiClient = createClient()

// ------------------------------------------------------------------ host UI 3.x compatibility
//
// The console renewed JWT sessions itself until sessions moved to the server
// (CONTRACTS §100). Plugins built against UI 3.x may still import these: they
// stay, harmless, until host UI 4.0.

/** @deprecated Sessions renew on the server; always 0. Removed in host UI 4.0. */
export const RENEW_BEFORE_MS = 0

/** @deprecated Sessions renew on the server; never due. Removed in host UI 4.0. */
export function renewalDue(_session: Session | null, _now: number): boolean {
  return false
}

/** @deprecated Sessions renew on the server; does nothing and returns a no-op stop. Removed in host UI 4.0. */
export function startKeepAlive(): () => void {
  return () => {}
}
