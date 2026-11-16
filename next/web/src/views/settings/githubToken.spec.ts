import { describe, expect, it } from 'vitest'
import { githubTokenURL, repositoryOwner } from './githubToken'

describe('GitHub token template URL', () => {
  it('pre-fills a read-only token for the repository owner', () => {
    const url = new URL(githubTokenURL('https://github.com/Owner-1/private-repo.git'))
    expect(url.origin + url.pathname).toBe('https://github.com/settings/personal-access-tokens/new')
    expect(Object.fromEntries(url.searchParams)).toEqual({
      name: 'sub2api updates', description: 'Read releases for sub2api update checks', target_name: 'Owner-1', expires_in: 'none', contents: 'read'
    })
    expect(url.searchParams.get('name')!.length).toBeLessThanOrEqual(40)
    expect(githubTokenURL('owner/repo')).toContain('name=sub2api%20updates&')
  })

  it('leaves the owner out unless the repository is valid', () => {
    for (const repository of ['', 'owner', 'owner/repo/extra', '-bad/repo', 'https://evil.test/owner/repo', 'own&er/repo']) {
      expect(repositoryOwner(repository)).toBe('')
      expect(githubTokenURL(repository)).not.toContain('target_name')
    }
    expect(repositoryOwner(' owner/repo ')).toBe('owner')
  })
})
