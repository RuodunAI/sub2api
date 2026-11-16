import { fail, now, on } from './router'
import { stepUp } from './core'

const DEFAULT_REPOSITORY = 'Sub2API-Devs/sup2api'
const source = { enabled: true, kind: 'github', repository: DEFAULT_REPOSITORY, url: '', has_secret: false, revision: 1 }
const view = () => ({ ...source, default_repository: DEFAULT_REPOSITORY })
on('GET', '/system/version', () => ({ version: '0.1.290', managed: true }))
on('GET', '/system/update-source', view)
on('PUT', '/system/update-source', stepUp('system.update', req => {
  const b = req.body || {}
  const moved = b.kind !== source.kind || b.repository !== source.repository || b.url !== source.url
  Object.assign(source, { enabled: !!b.enabled, kind: b.kind, repository: String(b.repository || '').replace(/^https:\/\/github.com\//, '').replace(/\/$/, ''), url: b.url, revision: source.revision + 1 })
  if (b.secret !== undefined) source.has_secret = b.secret !== ''
  else if (moved) source.has_secret = false
  return view()
}))
const line = (version: string, latest: string, page: string) => ({
  latest_version: latest, has_update: latest !== version, compatible: true, reason: '', notes: '改进网关状态展示与在线更新检测。\nGateway state visibility and online update checks.',
  page, published_at: now(-3600), checked_at: now(-60), last_success_at: now(-60), error: ''
})
on('GET', '/system/update-check', () => ({
  enabled: source.enabled,
  source: { kind: source.kind, repository: source.repository, url: source.url },
  core: { ...line('0.1.290', '0.1.291', `https://github.com/${source.repository}/releases/tag/core-v0.1.291`), current_version: '0.1.290', strategy: 'primary-first', tag: 'core-v0.1.291' },
  gateway: {
    ...line('0.1.135', '0.1.136', `https://github.com/${source.repository}/releases/tag/gateway-v0.1.136`),
    nodes: [{ node_id: 'node-1', version: '0.1.135' }, { node_id: 'node-2', version: '0.1.135' }, { node_id: 'node-3', version: '' }],
    self_update: false, image: 'ghcr.io/sub2api-devs/sup2api-gateway@sha256:4f1c0e2b9a7d', tag: 'gateway-v0.1.136'
  }
}))
on('POST', '/system/updates/apply', stepUp('system.update', req => {
  if (req.body?.line === 'gateway') return fail(501, 'not_implemented', 'Gateway self-update is not available yet')
  if (!req.body?.confirm) return { status: 'needs_confirmation', reasons: ['此版本改动数据库，按主节点优先策略升级，期间服务会短暂停止'], preflight: {} }
  return { status: 'started', plan_id: 'plan-1' }
}))
on('GET', '/system/releases', () => ({ releases: ['old', 'new'].map((v, i) => ({ digest: `release-${v}`, manifest: { release_id: `0.1.${7 + i}`, build_id: v, source_commit: 'demo', created_at: now(-100) } })) }))
