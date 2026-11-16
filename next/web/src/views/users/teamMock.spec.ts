// @vitest-environment node
// The users and account owners of the dev mock hold to what the server
// answers (CONTRACTS §103.3, §103.4): a supplier agent reaches its team, names
// no owner but itself, and an administrator moves users and accounts.
import { beforeAll, describe, expect, it } from 'vitest'
import { mockServer, type MockServer } from '@/testing/mockServer'

let call: MockServer['call'], signIn: MockServer['signIn'], fields: MockServer['fields']
let agent = ''
beforeAll(async () => {
  ;({ call, signIn, fields } = await mockServer('core', 'accounts', 'resources', 'billing'))
  agent = await signIn('agent@example.com')
})

const ids = (a: { body: { data: { data: Array<{ id: number }> } } }) => a.body.data.data.map((u) => u.id).sort((x, y) => x - y)

describe('the team in the mock', () => {
  it('lists an agent its team without itself, every level down, and narrows to an owner', async () => {
    const vendor = await signIn('vendor@example.com')
    const made = await call('POST', '/users', { email: 'below@example.com', password: 'longenough', role_keys: ['supplier'] }, agent)
    expect(made.body.data).toMatchObject({ owner_id: 11, owner_email: 'agent@example.com' })
    // The vendor (in the agent's team) has no user:create; an administrator puts a user under it.
    const deep = (await call('POST', '/users', { email: 'deep@example.com', password: 'longenough', owner_id: 6 })).body.data
    expect(ids(await call('GET', '/users?page_size=200', undefined, agent))).toEqual([6, 100, 101, made.body.data.id, deep.id].sort((x, y) => x - y))
    expect(ids(await call('GET', '/users?page_size=200&direct=true', undefined, agent))).not.toContain(deep.id)
    expect(ids(await call('GET', '/users?page_size=200&owner_id=6', undefined, agent))).toEqual([deep.id])
    expect((await call('GET', '/users/11', undefined, agent)).status).toBe(404)
    expect((await call('GET', '/users', undefined, vendor)).status).toBe(403)
  })

  it('lets a team holder name only itself as the owner, and refuses an owner below the user', async () => {
    expect((await call('POST', '/users', { email: 'other@example.com', password: 'longenough', owner_id: 2 }, agent)).status).toBe(403)
    expect((await call('PATCH', '/users/100', { owner_id: 2 }, agent)).status).toBe(403)
    expect((await call('PATCH', '/users/2', { display_name: 'x' }, agent)).status).toBe(404)
    const cycle = await call('PATCH', '/users/11', { owner_id: 6 })
    expect([cycle.status, fields(cycle), cycle.body.error.details.fields[0].code]).toEqual([400, ['owner_id'], 'cycle'])
  })

  it('moves an account to another owner for account:owner:update only, and its new owner reads it', async () => {
    expect((await call('PUT', '/accounts/12/owner', { owner_id: 6 }, agent)).status).toBe(403)
    const moved = await call('PUT', '/accounts/12/owner', { owner_id: 6 })
    expect(moved.body.data).toEqual({ id: 12, owner_id: 6, owner_email: 'vendor@example.com' })
    const listed = (await call('GET', '/accounts?page_size=200&subordinates=true', undefined, agent)).body.data.data.map((a: { id: number }) => a.id)
    expect(listed).toContain(12)
    expect(listed).not.toContain(17)
  })
})
