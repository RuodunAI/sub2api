// The dev mock (web/mock, vite --mode mock) answering requests as its server
// does, for the specs that hold it to what the core answers, and for a page
// under test that talks to it (`fetch`). The mock is node code outside the
// app's type program, so it is loaded by path and its answers are untyped.

export interface Answer {
  status: number
  body: any
}

export interface MockServer {
  /** Answers a request as the mock server does: a plain value is wrapped in {data}. */
  call(method: string, url: string, body?: unknown, token?: string): Promise<Answer>
  /** The access token of a mock identity (password admin). */
  signIn(email: string): Promise<string>
  /** The fields of a 400, in order. */
  fields(a: Answer): string[] | undefined
  /** A global fetch answering /api/v1 as the dev server's mock middleware does (mock/index.ts), for the console's api client. */
  fetch(input: string | URL | Request, init?: RequestInit): Promise<Response>
}

type Route = { method: string; re: RegExp; keys: string[]; handler: (req: unknown) => unknown }

const mockFile = (name: string) => new URL(`../../mock/${name}.ts`, import.meta.url).pathname

/** Loads the router and the mock areas named (each registers its routes). */
export async function mockServer(...areas: string[]): Promise<MockServer> {
  const routes: Route[] = (await import(/* @vite-ignore */ mockFile('router'))).routes
  for (const area of areas) await import(/* @vite-ignore */ mockFile(area))

  /** The handler's own result: a plain value, or {__status, body}. */
  async function handle(method: string, url: string, body?: unknown, token?: string): Promise<any> {
    const u = new URL(url, 'http://mock')
    for (const r of routes) {
      const m = r.method === method ? r.re.exec(u.pathname) : null
      if (!m) continue
      const params = Object.fromEntries(r.keys.map((k, i) => [k, decodeURIComponent(m[i + 1])]))
      return r.handler({
        method,
        path: u.pathname,
        params,
        query: Object.fromEntries(u.searchParams),
        body,
        headers: token ? { authorization: `Bearer ${token}` } : {}
      })
    }
    throw new Error(`no mock route for ${method} ${url}`)
  }
  const raw = (out: any) => out && typeof out === 'object' && '__status' in out

  async function call(method: string, url: string, body?: unknown, token?: string): Promise<Answer> {
    const out = await handle(method, url, body, token)
    return raw(out) ? { status: out.__status, body: out.body } : { status: 200, body: { data: out } }
  }
  async function fetch(input: string | URL | Request, init: RequestInit = {}): Promise<Response> {
    const u = new URL(String(input), 'http://mock')
    const token = new Headers(init.headers).get('authorization')?.replace(/^Bearer /, '')
    const out = await handle(init.method ?? 'GET', u.pathname.replace(/^\/api\/v1/, '') + u.search, typeof init.body === 'string' ? JSON.parse(init.body) : undefined, token)
    // As the middleware: a list ({data, page}) or an error goes as it is, any other value in {data}.
    const [status, body] = raw(out) ? [out.__status, out.body] : [200, out && typeof out === 'object' && ('data' in out || 'error' in out) ? out : { data: out }]
    return new Response(body === undefined ? null : JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
  }
  const signIn = async (email: string) => (await call('POST', '/auth/login', { email, password: 'admin' })).body.data.access_token as string
  const fields = (a: Answer) => a.body.error?.details?.fields?.map((f: { field: string }) => f.field)
  return { call, signIn, fields, fetch }
}
