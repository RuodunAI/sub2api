import { describe, expect, it } from 'vitest'
import { copyGrants, type CopyRules } from './copyGrants'

const rules = (over: Partial<CopyRules> = {}): CopyRules => ({ mayGrant: () => true, gone: () => false, critical: () => false, ...over })
const none = { notGrantable: [], critical: [], gone: [] }

describe('copyGrants union', () => {
  it('adds what the source grants and keeps the rest, canonical and sorted', () => {
    expect(copyGrants(['user:read', 'plugin.g:read'], ['user:delete', 'user:read', 'plugin.g:flow:read:*'], 'union', rules())).toEqual({
      keys: ['plugin.g:flow:read', 'plugin.g:read', 'user:delete', 'user:read'],
      ...none
    })
  })

  it('meets scopes as scopes: a wider scope replaces a narrower one, a narrower one adds nothing', () => {
    expect(copyGrants(['account:read:own'], ['account:read:team'], 'union', rules()).keys).toEqual(['account:read:team'])
    expect(copyGrants(['account:read'], ['account:read:own', 'account:read:team'], 'union', rules()).keys).toEqual(['account:read'])
  })

  it('leaves out what the editor may not grant, keeping the scope the role already has', () => {
    const mayGrant = (k: string) => k !== 'account:read' && k !== 'plugin.g:read'
    expect(copyGrants(['account:read:own'], ['account:read', 'plugin.g:read', 'user:read'], 'union', rules({ mayGrant }))).toEqual({
      keys: ['account:read:own', 'user:read'],
      notGrantable: ['account:read', 'plugin.g:read'],
      critical: [],
      gone: []
    })
  })

  it('leaves out critical and gone actions apart from what the editor may not grant', () => {
    const res = copyGrants([], ['plugin.g:bypass', 'plugin.g:old', 'user:delete'], 'union', rules({
      critical: (a) => a === 'plugin.g:bypass',
      gone: (a) => a === 'plugin.g:old',
      mayGrant: (k) => k !== 'user:delete'
    }))
    expect(res).toEqual({ keys: [], notGrantable: ['user:delete'], critical: ['plugin.g:bypass'], gone: ['plugin.g:old'] })
    // A critical key beyond the editor cannot be ticked by hand either: it is not grantable.
    expect(copyGrants([], ['plugin.g:bypass'], 'union', rules({ critical: () => true, mayGrant: () => false })).notGrantable).toEqual(['plugin.g:bypass'])
  })

  it('does not ask for what the role already implies', () => {
    expect(copyGrants(['account:read', 'plugin.g:bypass'], ['account:read:own', 'plugin.g:bypass'], 'union', rules({ mayGrant: () => false, critical: () => true }))).toEqual({
      keys: ['account:read', 'plugin.g:bypass'],
      ...none
    })
  })

  it('keeps a key of the role that does not parse and ignores one of the source', () => {
    expect(copyGrants(['plugin.g:read:Bad/!'], ['plugin.g:x:Bad/!', 'user:read'], 'union', rules()).keys).toEqual(['plugin.g:read:Bad/!', 'user:read'])
  })
})

describe('copyGrants intersect', () => {
  it('keeps per action the rows both sides cover, at the narrower scope, granting nothing', () => {
    expect(copyGrants(['user:read', 'user:delete', 'account:read', 'proxy:read:own', 'account:test:team'], ['user:read', 'account:read:own', 'proxy:read', 'gateway:use', 'account:test:own'], 'intersect', rules({ mayGrant: () => false }))).toEqual({
      keys: ['account:read:own', 'account:test:own', 'proxy:read:own', 'user:read'],
      ...none
    })
  })

  it('meets a group range and an account as that account within the group', () => {
    expect(copyGrants(['account:read:group/12'], ['account:read:account/5'], 'intersect', rules()).keys).toEqual(['account:read:group/12/account/5'])
    expect(copyGrants(['account:read:account/5'], ['account:read:group/*'], 'intersect', rules()).keys).toEqual(['account:read:group/*/account/5'])
    expect(copyGrants(['account:read:group/12'], ['account:read:group/*/account/5'], 'intersect', rules()).keys).toEqual(['account:read:group/12/account/5'])
    expect(copyGrants(['account:read:group/12'], ['account:read:group/13', 'account:read:own', 'account:read:group/13/account/5'], 'intersect', rules()).keys).toEqual([])
  })

  it('meets plugin scopes', () => {
    expect(copyGrants(['plugin.g:flow:read', 'plugin.g:flow:edit:flow/1'], ['plugin.g:flow:read:flow/3', 'plugin.g:flow:edit:flow/2'], 'intersect', rules()).keys).toEqual(['plugin.g:flow:read:flow/3'])
  })

  it('does not narrow a gone action: it keeps only its own scopes the source implies', () => {
    const gone = (a: string) => a === 'plugin.g:x'
    expect(copyGrants(['plugin.g:x'], ['plugin.g:x:own'], 'intersect', rules({ gone })).keys).toEqual([])
    expect(copyGrants(['plugin.g:x:own'], ['plugin.g:x'], 'intersect', rules({ gone })).keys).toEqual(['plugin.g:x:own'])
  })

  it('with a role granting nothing clears the role', () => {
    expect(copyGrants(['user:read', 'account:read:team'], [], 'intersect', rules()).keys).toEqual([])
  })
})
