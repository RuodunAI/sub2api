import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

// http.ts keeps module state (session, config): every test
// gets a fresh copy. Migrated from scripts/http-headers-test.mjs and extended
// with session / error-envelope cases.
type Http = typeof import('./http')
let http: Http

const json = (data: unknown, status = 200, headers: Record<string, string> = {}) =>
  new Response(JSON.stringify(status < 400 ? { data } : { error: data }), { status, headers })

beforeEach(async () => {
  vi.resetModules()
  localStorage.clear()
  http = await import('./http')
})

afterEach(() => {
  http.session.clear()
  vi.unstubAllGlobals()
})

function stubFetch(fn: (url: string, init: RequestInit & { headers: Record<string, string> }) => Response | Promise<Response>) {
  const spy = vi.fn(fn)
  vi.stubGlobal('fetch', spy)
  return spy
}

describe('envelope', () => {
  it('unwraps data and lists, synthesising page for bare arrays', async () => {
    stubFetch((url) => {
      if (url.includes('/paged')) return new Response(JSON.stringify({ data: [1, 2], page: { page: 1, page_size: 2, total: 9 } }))
      if (url.includes('/bare')) return new Response(JSON.stringify({ data: [1, 2, 3] }))
      return json({ ok: true })
    })
    expect(await http.api.get('/x')).toEqual({ ok: true })
    const paged = await http.api.list<number>('/paged')
    expect(paged).toMatchObject({ items: [1, 2], paged: true, page: { total: 9 } })
    const bare = await http.api.list<number>('/bare')
    expect(bare).toMatchObject({ items: [1, 2, 3], paged: false, page: { page: 1, page_size: 3, total: 3 } })
  })

  it('listAll walks the pages up to max, keeping the query', async () => {
    const f = stubFetch((url) => {
      const page = Number(new URL(url, 'http://x').searchParams.get('page'))
      const items = Array.from({ length: page < 3 ? 200 : 5 }, (_, i) => (page - 1) * 200 + i)
      return new Response(JSON.stringify({ data: items, page: { page, page_size: 200, total: 405 } }))
    })
    expect(await http.listAll<number>(http.api, '/all', { query: { q: 'a' } })).toHaveLength(405)
    expect(f).toHaveBeenCalledTimes(3)
    expect(f.mock.calls[0][0]).toBe('/api/v1/all?q=a&page=1&page_size=200')
    expect(await http.listAll<number>(http.api, '/all', { max: 250 })).toHaveLength(250)
    expect(f).toHaveBeenCalledTimes(5)
  })

  it('listAll follows a route that caps the page size lower than asked', async () => {
    const f = stubFetch((url) => {
      const page = Number(new URL(url, 'http://x').searchParams.get('page'))
      const items = Array.from({ length: page < 3 ? 50 : 7 }, (_, i) => (page - 1) * 50 + i)
      return new Response(JSON.stringify({ data: items, page: { page, page_size: 50, total: 107 } }))
    })
    const all = await http.listAll<number>(http.api, '/capped')
    expect(all).toHaveLength(107)
    expect(all.at(-1)).toBe(106)
    expect(f).toHaveBeenCalledTimes(3)
  })

  it('listAll stops on a short page when the total is unknown, and on an empty page', async () => {
    let f = stubFetch((url) => {
      const page = Number(new URL(url, 'http://x').searchParams.get('page'))
      return new Response(JSON.stringify({ data: Array.from({ length: page < 2 ? 100 : 30 }, () => page), page: { page, page_size: 100, total: 0 } }))
    })
    expect(await http.listAll<number>(http.api, '/nototal')).toHaveLength(130)
    expect(f).toHaveBeenCalledTimes(2)
    f = stubFetch((url) => {
      const page = Number(new URL(url, 'http://x').searchParams.get('page'))
      return new Response(JSON.stringify({ data: page < 2 ? Array.from({ length: 100 }, () => 1) : [], page: { page, page_size: 100, total: 0 } }))
    })
    expect(await http.listAll<number>(http.api, '/exact')).toHaveLength(100)
    expect(f).toHaveBeenCalledTimes(2)
  })

  it('builds query strings: drops empty values, repeats arrays', async () => {
    const f = stubFetch(() => json(null))
    await http.api.get('/q', { a: 1, b: '', c: null, d: undefined, e: ['x', 'y'], f: false })
    expect(f.mock.calls[0][0]).toBe('/api/v1/q?a=1&e=x&e=y&f=false')
  })

  it('throws ApiError with flattened localized field errors', async () => {
    http.configureHttp({ locale: () => 'zh' })
    stubFetch(() =>
      json(
        {
          code: 'invalid_argument',
          message: 'bad',
          details: { fields: [{ field: 'name', message: 'required' }, { field: 'expr', code: 'syntax', message: { en: 'syntax', zh: '语法' } }] }
        },
        400
      )
    )
    const err = await http.api.post('/x', {}).catch((e) => e)
    expect(http.isApiError(err)).toBe(true)
    expect(err.code).toBe('invalid_argument')
    expect(err.fields).toEqual({ name: 'required', expr: '语法' })
    expect(err.fieldCodes).toEqual({ name: 'invalid', expr: 'syntax' })
  })

  it('maps HTTP status when the body has no error code', async () => {
    stubFetch(() => new Response('oops', { status: 409 }))
    const err = await http.api.get('/x').catch((e) => e)
    expect(err.code).toBe('conflict')
  })

  it('returns null for 204', async () => {
    stubFetch(() => new Response(null, { status: 204 }))
    expect(await http.api.del('/x')).toBeNull()
  })

  it('surfaces 403 as-is without retrying', async () => {
    const f = stubFetch(() => json({ code: 'permission_denied', message: 'no' }, 403))
    await expect(http.api.post('/danger')).rejects.toMatchObject({ status: 403, code: 'permission_denied' })
    expect(f).toHaveBeenCalledTimes(1)
  })
})

describe('response headers', () => {
  it('reports headers of the final response only', async () => {
    stubFetch(() => json({ core: 'b' }, 200, { 'X-Sub2api-Entry-Node': 'gateway-a' }))
    const direct = await http.requestWithHeaders<{ core: string }>('GET', '/system/version')
    expect(direct.headers.get('x-sub2api-entry-node')).toBe('gateway-a')
    expect(direct.data.core).toBe('b')

    const cb = vi.fn()
    stubFetch(() => json({ code: 'unavailable', message: 'down' }, 503, { 'X-Sub2api-Entry-Node': 'untrusted' }))
    await expect(http.requestWithHeaders('GET', '/system/version', { onSuccessHeaders: cb })).rejects.toBeInstanceOf(http.ApiError)
    expect(cb).not.toHaveBeenCalled()
  })
})

describe('session', () => {
  it('signs out on 401 at once: the server renews a session, nothing is left to try', async () => {
    http.session.fromTokenResponse({ access_token: 's2s_a' })
    const onUnauthenticated = vi.fn()
    http.configureHttp({ onUnauthenticated })
    const f = stubFetch(() => json({ code: 'unauthenticated' }, 401))
    await expect(http.api.get('/x')).rejects.toBeInstanceOf(http.ApiError)
    expect(f).toHaveBeenCalledTimes(1)
    expect(http.session.get()).toBeNull()
    expect(localStorage.getItem('s2a.session')).toBeNull()
    expect(onUnauthenticated).toHaveBeenCalledWith('expired')
  })

  it('reports a 401 without a session as unauthenticated', async () => {
    const onUnauthenticated = vi.fn()
    http.configureHttp({ onUnauthenticated })
    stubFetch(() => json({ code: 'unauthenticated' }, 401))
    await expect(http.api.get('/x')).rejects.toBeInstanceOf(http.ApiError)
    expect(onUnauthenticated).toHaveBeenCalledWith('unauthenticated')
  })

  it('leaves a 401 of an anonymous request to the caller', async () => {
    http.session.fromTokenResponse({ access_token: 's2s_a' })
    const onUnauthenticated = vi.fn()
    http.configureHttp({ onUnauthenticated })
    stubFetch(() => json({ code: 'unauthenticated' }, 401))
    await expect(http.api.post('/auth/login', {}, { anonymous: true })).rejects.toMatchObject({ status: 401 })
    expect(http.session.get()?.access_token).toBe('s2s_a')
    expect(onUnauthenticated).not.toHaveBeenCalled()
  })

  it('retries a 401 when the token changed meanwhile (step-up rotation)', async () => {
    http.session.fromTokenResponse({ access_token: 's2s_old' })
    const f = stubFetch((_url, init) => {
      if (init.headers.Authorization === 'Bearer s2s_old') {
        http.session.fromTokenResponse({ access_token: 's2s_new' })
        return json({ code: 'unauthenticated' }, 401)
      }
      return json({ ok: 1 }, 200, { 'X-Sub2api-Entry-Node': 'final' })
    })
    const r = await http.requestWithHeaders('GET', '/x')
    expect(r.data).toEqual({ ok: 1 })
    expect(r.headers.get('x-sub2api-entry-node')).toBe('final')
    expect(f).toHaveBeenCalledTimes(2)
  })

  it('reads what another tab stored before ending the session on 401', async () => {
    http.session.fromTokenResponse({ access_token: 's2s_old' })
    // Another tab signed in again; this one, just woken, missed the storage event.
    localStorage.setItem('s2a.session', JSON.stringify({ access_token: 's2s_new' }))
    const onUnauthenticated = vi.fn()
    http.configureHttp({ onUnauthenticated })
    const f = stubFetch((_url, init) => (init.headers.Authorization === 'Bearer s2s_new' ? json({ ok: 1 }) : json({ code: 'unauthenticated' }, 401)))
    expect(await http.api.get('/x')).toEqual({ ok: 1 })
    expect(f).toHaveBeenCalledTimes(2)
    expect(http.session.get()?.access_token).toBe('s2s_new')
    expect(onUnauthenticated).not.toHaveBeenCalled()
  })

  it('keeps a newer login stored while a request of the ended session was out', async () => {
    http.session.fromTokenResponse({ access_token: 's2s_old' })
    const onUnauthenticated = vi.fn()
    http.configureHttp({ onUnauthenticated })
    let calls = 0
    stubFetch(() => {
      // Another login lands while each send is out: the retry's too.
      http.session.fromTokenResponse({ access_token: `s2s_${++calls}` })
      return json({ code: 'unauthenticated' }, 401)
    })
    await expect(http.api.get('/x')).rejects.toMatchObject({ status: 401 })
    expect(calls).toBe(2)
    expect(http.session.get()?.access_token).toBe('s2s_2')
    expect(onUnauthenticated).not.toHaveBeenCalled()
  })

  it('keeps the session through a server error: an outage is not a sign-out', async () => {
    http.session.fromTokenResponse({ access_token: 's2s_a' })
    const onUnauthenticated = vi.fn()
    http.configureHttp({ onUnauthenticated })
    for (const status of [500, 503]) {
      stubFetch(() => json({ code: 'internal' }, status))
      await expect(http.api.get('/x')).rejects.toMatchObject({ status })
    }
    expect(http.session.get()?.access_token).toBe('s2s_a')
    expect(onUnauthenticated).not.toHaveBeenCalled()
  })

  it('fetchWithSession sends a raw request with the token and the 401 policy of the client', async () => {
    http.session.fromTokenResponse({ access_token: 's2s_old' })
    const onUnauthenticated = vi.fn()
    http.configureHttp({ onUnauthenticated })
    const body = new Blob(['raw'])
    const f = stubFetch((_url, init) => {
      if (init.headers.Authorization !== 'Bearer s2s_old') return new Response('file')
      http.session.fromTokenResponse({ access_token: 's2s_new' })
      return json({ code: 'unauthenticated' }, 401)
    })
    const res = await http.fetchWithSession('/files/a?offset=1', { method: 'PUT', headers: { 'X-A': '1' }, body })
    expect(await res.text()).toBe('file')
    expect(f).toHaveBeenCalledTimes(2)
    const [url, init] = f.mock.calls[1]
    expect(url).toBe('/api/v1/files/a?offset=1')
    expect(init).toMatchObject({ method: 'PUT', body, headers: { 'X-A': '1', Authorization: 'Bearer s2s_new' } })

    stubFetch(() => json({ code: 'unauthenticated' }, 401))
    expect((await http.fetchWithSession('/files/a')).status).toBe(401)
    expect(http.session.get()).toBeNull()
    expect(onUnauthenticated).toHaveBeenCalledWith('expired')
  })

  it('follows another tab that signs out, or whose session ended, to the login page', () => {
    http.session.fromTokenResponse({ access_token: 's2s_a' })
    const onUnauthenticated = vi.fn()
    http.configureHttp({ onUnauthenticated })
    localStorage.removeItem('s2a.session')
    window.dispatchEvent(new StorageEvent('storage', { key: 's2a.session', newValue: null }))
    expect(http.session.get()).toBeNull()
    expect(onUnauthenticated).toHaveBeenCalledWith('unauthenticated')
  })

  it('adopts a token another tab stored, signed out of nothing', () => {
    const onUnauthenticated = vi.fn()
    http.configureHttp({ onUnauthenticated })
    const seen = vi.fn()
    http.session.onChange(seen)
    for (const token of ['s2s_a', 's2s_b']) {
      localStorage.setItem('s2a.session', JSON.stringify({ access_token: token }))
      window.dispatchEvent(new StorageEvent('storage', { key: 's2a.session' }))
      expect(http.session.get()?.access_token).toBe(token)
    }
    expect(seen).toHaveBeenCalledTimes(2)
    expect(onUnauthenticated).not.toHaveBeenCalled()
  })
})

describe('session.adoptRotated', () => {
  it('takes up a rotated token in place of the one the request was sent with', () => {
    http.session.fromTokenResponse({ access_token: 's2s_a' })
    const seen = vi.fn()
    http.session.onChange(seen)
    expect(http.session.adoptRotated('s2s_a', 's2s_b')).toBe(true)
    expect(http.session.get()?.access_token).toBe('s2s_b')
    expect(JSON.parse(localStorage.getItem('s2a.session')!)).toEqual({ access_token: 's2s_b' })
    expect(seen).toHaveBeenCalledTimes(1)
  })

  it('keeps a newer token another tab stored, its storage event not in yet', () => {
    http.session.fromTokenResponse({ access_token: 's2s_a' })
    localStorage.setItem('s2a.session', JSON.stringify({ access_token: 's2s_other' }))
    expect(http.session.adoptRotated('s2s_a', 's2s_late')).toBe(false)
    expect(http.session.get()?.access_token).toBe('s2s_other')
    expect(JSON.parse(localStorage.getItem('s2a.session')!)).toEqual({ access_token: 's2s_other' })
  })

  it('takes up a rotation of a request resent with the token another tab stored (onSentWith)', async () => {
    http.session.fromTokenResponse({ access_token: 's2s_old' })
    // Another tab rotated the session; this one, its storage event not in yet, sends the old token.
    localStorage.setItem('s2a.session', JSON.stringify({ access_token: 's2s_other' }))
    stubFetch((_url, init) => (init.headers.Authorization === 'Bearer s2s_other' ? json({ access_token: 's2s_rotated' }) : json({ code: 'unauthenticated' }, 401)))
    let sentWith: string | undefined
    const r = await http.api.post<{ access_token: string }>('/me/verify', {}, { onSentWith: (token) => (sentWith = token) })
    expect(sentWith).toBe('s2s_other')
    expect(http.session.adoptRotated(sentWith, r.access_token)).toBe(true)
    expect(http.session.get()?.access_token).toBe('s2s_rotated')
  })

  it('does not sign a signed-out or newly signed-in console back into the old session', () => {
    expect(http.session.adoptRotated('s2s_a', 's2s_b')).toBe(false)
    expect(http.session.get()).toBeNull()
    http.session.fromTokenResponse({ access_token: 's2s_new_login' })
    expect(http.session.adoptRotated('s2s_a', 's2s_b')).toBe(false)
    expect(http.session.adoptRotated(undefined, 's2s_b')).toBe(false)
    expect(http.session.get()?.access_token).toBe('s2s_new_login')
  })

  it('tells listeners of a token another tab stored once, by the event or by a read first', () => {
    http.session.fromTokenResponse({ access_token: 's2s_a' })
    const seen = vi.fn()
    http.session.onChange(seen)
    localStorage.setItem('s2a.session', JSON.stringify({ access_token: 's2s_b' }))
    http.session.adoptRotated('s2s_a', 's2s_c') // reads s2s_b first
    window.dispatchEvent(new StorageEvent('storage', { key: 's2a.session' }))
    expect(seen).toHaveBeenCalledTimes(1)
    expect(http.session.get()?.access_token).toBe('s2s_b')
  })
})

describe('authz hooks', () => {
  afterEach(() => http.configureHttp({ onAuthzVersion: undefined, onForbidden: undefined }))

  it('reports X-Authz-Version of success and error responses, and 403 errors', async () => {
    const onAuthzVersion = vi.fn()
    const onForbidden = vi.fn()
    http.configureHttp({ onAuthzVersion, onForbidden })
    stubFetch((url) => {
      if (url.endsWith('/ok')) return json(1, 200, { 'X-Authz-Version': '7' })
      if (url.endsWith('/denied')) return json({ code: 'permission_denied', message: 'no' }, 403, { 'X-Authz-Version': '8' })
      return json(1)
    })
    await http.api.get('/ok')
    await expect(http.api.get('/denied')).rejects.toMatchObject({ status: 403, code: 'permission_denied' })
    await http.api.get('/plain')
    expect(onAuthzVersion.mock.calls).toEqual([[7], [8]])
    expect(onForbidden).toHaveBeenCalledTimes(1)
    expect(onForbidden.mock.calls[0][0]).toBeInstanceOf(http.ApiError)
  })

  it('does not report a verification_required answered by a step-up as forbidden', async () => {
    const onForbidden = vi.fn()
    const onAuthzVersion = vi.fn()
    http.configureHttp({ onForbidden, onAuthzVersion, onVerificationRequired: async () => 'p' })
    stubFetch((_url, init) =>
      init.headers['X-Verify-Proof']
        ? json(1, 200, { 'X-Authz-Version': '4' })
        : json({ code: 'verification_required', message: 'verify', details: { scope: 'user.delete', context: 'DELETE /api/v1/users/5' } }, 403, { 'X-Authz-Version': '3' })
    )
    expect(await http.api.del('/users/5')).toBe(1)
    expect(onForbidden).not.toHaveBeenCalled()
    expect(onAuthzVersion.mock.calls).toEqual([[3], [4]])
    http.configureHttp({ onVerificationRequired: undefined })
  })
})

describe('step-up verification', () => {
  const required = (details: Record<string, unknown> = {}) =>
    json({ code: 'verification_required', message: 'verify', details: { scope: 'user.delete', context: 'DELETE /api/v1/users/5', methods: [], password_required: false, ...details } }, 403)

  it('asks for a proof and replays the request once with X-Verify-Proof', async () => {
    const ask = vi.fn(async (_c: unknown, _s?: AbortSignal) => 'proof-1')
    http.configureHttp({ onVerificationRequired: ask })
    const spy = stubFetch((_url, init) => (init.headers['X-Verify-Proof'] ? new Response(null, { status: 204 }) : required()))
    const form = new FormData()
    form.append('f', 'x')
    expect(await http.api.upload('/users/5', form)).toBeNull()
    expect(ask.mock.calls[0][0]).toEqual({ scope: 'user.delete', context: 'DELETE /api/v1/users/5', password_required: false })
    expect(spy).toHaveBeenCalledTimes(2)
    expect(spy.mock.calls[1][1].headers['X-Verify-Proof']).toBe('proof-1')
    expect(spy.mock.calls[1][1].body).toBe(form)
  })

  it('replays a JSON body unchanged and returns the replayed response', async () => {
    http.configureHttp({ onVerificationRequired: async () => 'p' })
    const spy = stubFetch((_url, init) => (init.headers['X-Verify-Proof'] ? json({ ok: 1 }) : required({ password_required: true })))
    expect(await http.api.put('/me/password', { new_password: 'n' })).toEqual({ ok: 1 })
    expect(spy.mock.calls.map((c) => c[1].body)).toEqual(['{"new_password":"n"}', '{"new_password":"n"}'])
  })

  it('fails with verification_cancelled when the user dismisses it', async () => {
    http.configureHttp({ onVerificationRequired: async () => null })
    const spy = stubFetch(() => required())
    await expect(http.api.del('/users/5')).rejects.toMatchObject({ status: 403, code: http.VERIFICATION_CANCELLED })
    expect(spy).toHaveBeenCalledTimes(1)
  })

  it('does not ask twice: a replay still refused is the error', async () => {
    const ask = vi.fn(async () => 'p')
    http.configureHttp({ onVerificationRequired: ask })
    stubFetch(() => required())
    await expect(http.api.del('/users/5')).rejects.toMatchObject({ code: 'verification_required' })
    expect(ask).toHaveBeenCalledTimes(1)
  })

  it('replays with the token the verification rotated (StepUpDialog stores it)', async () => {
    http.session.fromTokenResponse({ access_token: 's2s_old' })
    http.configureHttp({
      onVerificationRequired: async () => {
        http.session.fromTokenResponse({ access_token: 's2s_new' })
        return 'p'
      }
    })
    const spy = stubFetch((_url, init) => (init.headers['X-Verify-Proof'] ? json({ ok: 1 }) : required()))
    expect(await http.api.del('/users/5')).toEqual({ ok: 1 })
    expect(spy.mock.calls.at(-1)![1].headers).toMatchObject({ Authorization: 'Bearer s2s_new', 'X-Verify-Proof': 'p' })
  })

  it('gives the hook the request signal and fails as aborted once it aborts', async () => {
    const ctrl = new AbortController()
    let seen: AbortSignal | undefined
    http.configureHttp({
      onVerificationRequired: async (_c, signal) => {
        seen = signal
        ctrl.abort()
        return null
      }
    })
    const spy = stubFetch(() => required())
    await expect(http.api.del('/users/5', undefined, { signal: ctrl.signal })).rejects.toMatchObject({ name: 'AbortError' })
    expect(seen).toBe(ctrl.signal)
    expect(spy).toHaveBeenCalledTimes(1)
  })

  it('bounds each send by timeoutMs, not the verification in between', async () => {
    let seen: AbortSignal | undefined = new AbortController().signal
    http.configureHttp({
      onVerificationRequired: async (_c, signal) => {
        seen = signal
        // The user takes longer than the limit: the dialog is not closed under them.
        await new Promise((resolve) => setTimeout(resolve, 60))
        return signal?.aborted ? null : 'p'
      }
    })
    const spy = stubFetch((_url, init) => (init.headers['X-Verify-Proof'] ? json({ ok: 1 }) : required()))
    expect(await http.api.put('/system/settings', {}, { timeoutMs: 20 })).toEqual({ ok: 1 })
    expect(seen).toBeUndefined()
    // The replay gets a limit of its own.
    const signals = spy.mock.calls.map((c) => c[1].signal as AbortSignal)
    expect(signals).toHaveLength(2)
    expect(signals[1]).not.toBe(signals[0])
  })

  it('aborts a send that outlives timeoutMs', async () => {
    stubFetch((_url, init) => new Promise((_resolve, reject) => init.signal!.addEventListener('abort', () => reject(init.signal!.reason))))
    await expect(http.api.get('/slow', undefined, { timeoutMs: 20 })).rejects.toMatchObject({ name: 'TimeoutError' })
  })

  it('leaves other 403s alone', async () => {
    const ask = vi.fn(async () => 'p')
    http.configureHttp({ onVerificationRequired: ask })
    stubFetch(() => json({ code: 'permission_denied', message: 'no' }, 403))
    await expect(http.api.del('/users/5')).rejects.toMatchObject({ code: 'permission_denied' })
    expect(ask).not.toHaveBeenCalled()
  })
})
