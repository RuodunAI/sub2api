// @vitest-environment node
// The account usage endpoints of the dev mock hold to what the server answers
// (CONTRACTS §103.5): the records of the accounts in reach, whitelisted fields,
// and a 400 for any other parameter.
import { beforeAll, describe, expect, it } from 'vitest'
import { mockServer, type MockServer } from '@/testing/mockServer'

let call: MockServer['call'], signIn: MockServer['signIn'], fields: MockServer['fields']
let vendor = ''
let agent = ''
beforeAll(async () => {
  ;({ call, signIn, fields } = await mockServer('core', 'accounts', 'resources', 'billing'))
  vendor = await signIn('vendor@example.com')
  agent = await signIn('agent@example.com')
})

const accountIds = (rows: Array<{ account_id: number }>) => [...new Set(rows.map((r) => r.account_id))].sort()

describe('account usage in the mock', () => {
  it('lists the records of the accounts in reach: the owner its own, an agent its team’s too', async () => {
    const own = (await call('GET', '/account-usage?page_size=200', undefined, vendor)).body.data.data
    expect(accountIds(own)).toEqual([13, 16])
    const team = (await call('GET', '/account-usage?page_size=200', undefined, agent)).body.data.data
    expect(accountIds(team)).toEqual([13, 16])
    expect((await call('GET', '/account-usage', undefined, await signIn('pricer@example.com'))).status).toBe(403)
  })

  it('answers whitelisted fields, the list price as the cost, and out of reach accounts in a trail by account_ref only', async () => {
    const rows = (await call('GET', '/account-usage?page_size=200', undefined, vendor)).body.data.data
    for (const key of ['user_id', 'user_email', 'api_key_name', 'client_ip', 'group_name', 'total_cost', 'billing_status', 'error_message', 'conversion']) {
      expect(rows.some((r: object) => key in r)).toBe(false)
    }
    expect(rows.every((r: { standard_cost: string }) => /^\d+\.\d{8}$/.test(r.standard_cost))).toBe(true)
    const steps = rows.flatMap((r: { attempt_trail?: { steps: object[] } }) => r.attempt_trail?.steps ?? [])
    expect(steps.length).toBeGreaterThan(0)
    for (const s of steps as Array<Record<string, unknown>>) {
      expect('message' in s).toBe(false)
      if (s.account_id) expect([13, 16]).toContain(s.account_id)
      else expect(s.account_ref).toBeGreaterThan(0)
    }
    const detail = await call('GET', `/account-usage/${rows[0].id}`, undefined, vendor)
    expect(detail.body.data).toMatchObject({ id: rows[0].id, metrics: expect.any(Object) })
    const outside = (await call('GET', '/usage?page_size=200')).body.data.data.find((u: { account_id: number }) => u.account_id === 12)
    expect((await call('GET', `/account-usage/${outside.id}`, undefined, vendor)).status).toBe(404)
  })

  it('refuses a parameter the account view does not take, by name, after the permission; the detail reads none', async () => {
    const r = await call('GET', '/account-usage?user_id=1&client_ip=1.2.3.4', undefined, vendor)
    expect([r.status, fields(r)]).toEqual([400, ['client_ip', 'user_id']])
    expect(fields(await call('GET', '/account-usage?limit=5', undefined, vendor))).toEqual(['limit'])
    expect((await call('GET', '/account-usage?user_id=1', undefined, await signIn('pricer@example.com'))).status).toBe(403)
    const id = (await call('GET', '/account-usage', undefined, vendor)).body.data.data[0].id
    expect((await call('GET', `/account-usage/${id}?user_id=1`, undefined, vendor)).status).toBe(200)
    expect((await call('GET', '/account-usage/overview?model=x', undefined, vendor)).status).toBe(400)
    expect((await call('GET', '/account-usage/summary?group_by=user', undefined, vendor)).status).toBe(400)
    expect((await call('GET', '/account-usage/summary?group_by=model&order=cost', undefined, vendor)).status).toBe(400)
  })

  it('sums by account with its name and without the billed cost', async () => {
    const rows = (await call('GET', '/account-usage/summary?group_by=account&from=1970-01-01T00:00:00Z', undefined, vendor)).body.data
    expect(rows.map((r: { key: string }) => r.key).sort()).toEqual(['13', '16'])
    expect(rows[0]).toMatchObject({ account_name: expect.any(String), standard_cost: expect.any(String) })
    expect(rows.some((r: object) => 'total_cost' in r)).toBe(false)
    const overview = (await call('GET', '/account-usage/overview', undefined, vendor)).body.data
    expect('total_cost' in overview.today).toBe(false)
    expect(overview.accounts.total).toBe(3)
    expect(overview.users).toBeUndefined()
  })
})
