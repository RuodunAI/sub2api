// GitHub's page for a new fine-grained personal access token, pre-filled
// through its documented template parameters: a read-only (Contents: Read)
// token, without expiry, for the owner of the release repository.
const TOKEN_NAME = 'sub2api updates'
const TOKEN_DESCRIPTION = 'Read releases for sub2api update checks'
const OWNER = /^[A-Za-z0-9][A-Za-z0-9-]{0,38}$/

/** The owner of "owner/repository" or an https://github.com repository URL, or '' when it is not one. */
export function repositoryOwner(repository: string): string {
  const parts = repository.trim().replace(/^https:\/\/github\.com\//, '').replace(/\/$/, '').replace(/\.git$/, '').split('/')
  return parts.length === 2 && parts[1] && OWNER.test(parts[0]) ? parts[0] : ''
}

/** The token creation page for the source repository. */
export function githubTokenURL(repository: string): string {
  const params = { name: TOKEN_NAME, description: TOKEN_DESCRIPTION, target_name: repositoryOwner(repository), expires_in: 'none', contents: 'read' }
  const query = Object.entries(params).filter(([, v]) => v).map(([k, v]) => `${k}=${encodeURIComponent(v)}`)
  return `https://github.com/settings/personal-access-tokens/new?${query.join('&')}`
}
