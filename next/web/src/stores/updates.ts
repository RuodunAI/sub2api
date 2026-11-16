import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { api } from '@sub2api/host'
import { errorMessage } from '@/utils/errors'

export type UpdateLine = 'core' | 'gateway'
export const UPDATE_LINES: UpdateLine[] = ['core', 'gateway']

/** The last check of one version line; error keeps the last successful fields. */
export interface LineCheck {
  latest_version?: string
  has_update: boolean
  compatible: boolean
  reason?: string
  tag?: string
  notes?: string
  page?: string
  published_at?: string
  checked_at?: string
  last_success_at?: string
  error?: string
}

export interface CoreCheck extends LineCheck {
  current_version: string
  strategy?: string
}

export interface GatewayCheck extends LineCheck {
  nodes: Array<{ node_id: string; version: string }>
  /** Every node runs a gateway that can update itself. */
  self_update: boolean
  /** Image of the latest gateway, pinned by digest: the manual path. */
  image?: string
}

/** GET /system/update-check (the gateway's stored checks of both lines). */
export interface UpdateCheck {
  enabled: boolean
  source: { kind: string; repository: string; url: string }
  core: CoreCheck
  gateway: GatewayCheck
}

/** POST /system/updates/apply. */
export interface ApplyResult {
  status: 'started' | 'needs_confirmation'
  plan_id?: string
  reasons?: string[]
}

export type LineStatus = 'unchecked' | 'disabled' | 'failed' | 'noRelease' | 'available' | 'blocked' | 'upToDate'

/**
 * What a line's check says. A failed or never successful check is "failed",
 * never "upToDate": an unknown latest version is not the current one. A
 * successful check that found no stable release says why in its reason.
 */
export function lineStatus(check: UpdateCheck | null, line: UpdateLine, requestFailed = false): LineStatus {
  if (requestFailed) return 'failed'
  if (!check) return 'unchecked'
  if (!check.enabled) return 'disabled'
  const l = check[line]
  if (!l.checked_at && !l.error) return 'unchecked'
  if (l.error) return 'failed'
  if (!l.latest_version) return l.reason ? 'noRelease' : 'failed'
  if (l.has_update) return 'available'
  return l.reason || !l.compatible ? 'blocked' : 'upToDate'
}

export const useUpdatesStore = defineStore('updates', () => {
  const result = ref<UpdateCheck | null>(null)
  const busy = ref(false)
  const error = ref('')
  /** Either line has a newer release (as of its last successful check). */
  const available = computed(() => !error.value && !!result.value?.enabled && UPDATE_LINES.some(line => result.value![line].has_update))
  let generation = 0
  async function check(force = false) {
    if (busy.value) return
    busy.value = true
    error.value = ''
    const request = ++generation
    try {
      const response = await api.get<UpdateCheck>(`/system/update-check${force ? '?force=true' : ''}`)
      if (request === generation) result.value = response
    } catch (e) {
      if (request === generation) error.value = errorMessage(e)
    } finally {
      if (request === generation) busy.value = false
    }
  }
  function invalidate() { generation++; result.value = null; error.value = ''; busy.value = false }
  const apply = (line: UpdateLine, version: string, confirm = false) => api.post<ApplyResult>('/system/updates/apply', { line, version, confirm })
  return { result, busy, error, available, check, invalidate, apply }
})
