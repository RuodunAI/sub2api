import { describe, expect, it } from 'vitest'
import { copyGrants, type CopyRules } from './copyGrants'

const rules = (over: Partial<CopyRules> = {}): CopyRules => ({ mayGrant: () => true, gone: () => false, critical: () => false, ...over })
const none = { notGrantable: [], critical: [], gone: [] }

describe('copyGrants merge', () => {
  it('adds what the source grants and keeps the rest, canonical and sorted', () => {
    expect(copyGrants(['user:read', 'plugin.g:read'], ['user:delete', 'user:read', 'plugin.g:flow:read:*'], 'merge', rules())).toEqual({
      keys: ['plugin.g:flow:read', 'plugin.g:read', 'user:delete', 'user:read'],
      ...none
    })
  })

  it('meets scopes as scopes: a wider scope replaces a narrower one, a narrower one adds nothing', () => {
    expect(copyGrants(['account:read:own'], ['account:read:team'], 'merge', rules()).keys).toEqual(['account:read:team'])
    expect(copyGrants(['account:read'], ['account:read:own', 'account:read:team'], 'merge', rules()).keys).toEqual(['account:read'])
  })

  it('leaves out what the editor may not grant, keeping the scope the role already has', () => {
    const mayGrant = (k: string) => k !== 'account:read' && k !== 'plugin.g:read'
    expect(copyGrants(['account:read:own'], ['account:read', 'plugin.g:read', 'user:read'], 'merge', rules({ mayGrant }))).toEqual({
      keys: ['account:read:own', 'user:read'],
      notGrantable: ['account:read', 'plugin.g:read'],
      critical: [],
      gone: []
    })
  })

  it('leaves out critical and gone actions apart from what the editor may not grant', () => {
    const res = copyGrants([], ['plugin.g:bypass', 'plugin.g:old', 'user:delete'], 'merge', rules({
      critical: (a) => a === 'plugin.g:bypass',
      gone: (a) => a === 'plugin.g:old',
      mayGrant: (k) => k !== 'user:delete'
    }))
    expect(res).toEqual({ keys: [], notGrantable: ['user:delete'], critical: ['plugin.g:bypass'], gone: ['plugin.g:old'] })
    // A critical key beyond the editor cannot be ticked by hand either: it is not grantable.
    expect(copyGrants([], ['plugin.g:bypass'], 'merge', rules({ critical: () => true, mayGrant: () => false })).notGrantable).toEqual(['plugin.g:bypass'])
  })

  it('does not ask for what the role already implies', () => {
    expect(copyGrants(['account:read', 'plugin.g:bypass'], ['account:read:own', 'plugin.g:bypass'], 'merge', rules({ mayGrant: () => false, critical: () => true }))).toEqual({
      keys: ['account:read', 'plugin.g:bypass'],
      ...none
    })
  })

  it('keeps a key of the role that does not parse and ignores one of the source', () => {
    expect(copyGrants(['plugin.g:read:Bad/!'], ['plugin.g:x:Bad/!', 'user:read'], 'merge', rules()).keys).toEqual(['plugin.g:read:Bad/!', 'user:read'])
  })
})

describe('copyGrants overwrite', () => {
  it('ticks exactly what the source has', () => {
    expect(copyGrants(['user:read', 'user:delete', 'plugin.g:read'], ['user:read', 'gateway:use'], 'overwrite', rules())).toEqual({
      keys: ['gateway:use', 'user:read'],
      ...none
    })
  })

  it('takes a scope the role implies without a grant, so it may narrow', () => {
    expect(copyGrants(['account:read', 'proxy:read:own'], ['account:read:own', 'proxy:read:team'], 'overwrite', rules({ mayGrant: () => false }))).toEqual({
      keys: ['account:read:own'],
      notGrantable: ['proxy:read:team'],
      critical: [],
      gone: []
    })
  })

  it('leaves out critical and gone permissions of the source, and keeps the role\'s own gone keys', () => {
    const res = copyGrants(['plugin.g:old:own', 'plugin.g:bypass'], ['plugin.g:old', 'plugin.g:bypass', 'plugin.g:root'], 'overwrite', rules({
      gone: (a) => a === 'plugin.g:old',
      critical: (a) => a === 'plugin.g:bypass' || a === 'plugin.g:root'
    }))
    expect(res).toEqual({ keys: ['plugin.g:bypass', 'plugin.g:old:own'], notGrantable: [], critical: ['plugin.g:root'], gone: ['plugin.g:old'] })
  })

  it('with a role granting nothing clears the role', () => {
    expect(copyGrants(['user:read', 'account:read:team'], [], 'overwrite', rules()).keys).toEqual([])
  })
})
