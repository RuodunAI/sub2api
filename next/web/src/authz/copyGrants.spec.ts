import { describe, expect, it } from 'vitest'
import { copyGrants } from './copyGrants'

const all = () => true

describe('copyGrants', () => {
  it('union adds what the source grants and keeps the rest, canonical and sorted', () => {
    expect(copyGrants(['user:read', 'plugin.g:read'], ['user:delete', 'user:read', 'plugin.g:flow:read:*'], 'union', all)).toEqual({
      keys: ['plugin.g:flow:read', 'plugin.g:read', 'user:delete', 'user:read'],
      skipped: []
    })
  })

  it('union meets scopes as scopes: a wider scope replaces a narrower one, a narrower one adds nothing', () => {
    expect(copyGrants(['account:read:own'], ['account:read:team'], 'union', all).keys).toEqual(['account:read:team'])
    expect(copyGrants(['account:read'], ['account:read:own', 'account:read:team'], 'union', all).keys).toEqual(['account:read'])
  })

  it('union leaves out and names what the editor may not grant, keeping the scope the role already has', () => {
    const mayGrant = (k: string) => k !== 'account:read' && k !== 'plugin.g:read'
    expect(copyGrants(['account:read:own'], ['account:read', 'plugin.g:read', 'user:read'], 'union', mayGrant)).toEqual({
      keys: ['account:read:own', 'user:read'],
      skipped: ['account:read', 'plugin.g:read']
    })
  })

  it('union does not ask to grant what the role already implies', () => {
    expect(copyGrants(['account:read'], ['account:read:own'], 'union', () => false)).toEqual({ keys: ['account:read'], skipped: [] })
  })

  it('intersect keeps per action what both sides cover, at the narrower scope', () => {
    expect(copyGrants(['user:read', 'user:delete', 'account:read', 'proxy:read:own'], ['user:read', 'account:read:own', 'proxy:read', 'gateway:use'], 'intersect', () => false)).toEqual({
      keys: ['account:read:own', 'proxy:read:own', 'user:read'],
      skipped: []
    })
  })

  it('intersect with a role granting nothing clears the role', () => {
    expect(copyGrants(['user:read', 'account:read:team'], [], 'intersect', all).keys).toEqual([])
  })

  it('keeps a key of the role that does not parse and ignores one of the source', () => {
    expect(copyGrants(['plugin.g:read:Bad/!'], ['plugin.g:x:Bad/!', 'user:read'], 'union', all).keys).toEqual(['plugin.g:read:Bad/!', 'user:read'])
  })
})
