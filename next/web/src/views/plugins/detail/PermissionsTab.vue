<script setup lang="ts">
// A plugin's two kinds of permissions: the access it requests (granted to the
// plugin itself: GrantsTab, revoke / narrow / grant again), and the
// permissions it provides to users × roles (the Roles page's data seen by
// plugin: GET/PUT /permissions/plugins/:plugin, the shared PermissionMatrix).
// A plugin declaring none has no module there (404): the matrix says so.
//
// The matrix reloads with the detail (an upgrade or a confirmed review may
// have granted new keys), its unsaved cells kept on top of what it reads
// (useGrantMatrix), an edit a new lock refuses dropped and told. The save
// carries the version it read (C1): when the grants changed meanwhile (409
// version_conflict) it reads them again the same way; if the roles it saves
// are as they were (the version moved for something else) it sends again at
// once, else it asks to save once more, naming the cells edited on both
// sides. A failed read keeps the edits for another try. A save granting sensitive or critical
// permissions asks first; a critical one may also ask the server's step-up
// (handled by the API client). Leaving the tab, the plugin or the page with
// unsaved cells asks first.
import { computed, onMounted, ref, watch } from 'vue'
import { onBeforeRouteLeave, onBeforeRouteUpdate } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { api, isApiError } from '@sub2api/host'
import { SAlert, SButton, SCard, SDisclosure, SEmpty, SHint, toast } from '@sub2api/ui'
import type { PluginRoleMatrix } from '@/api/types'
import { lt } from '@/i18n'
import { useAuthStore } from '@/stores/auth'
import { notifyError } from '@/utils/errors'
import { isVersionConflict, missingText } from '@/utils/permErrors'
import { confirmDiscard } from '@/composables/confirmDiscard'
import { cellSummary, localOf, newRiskyGrants, sameKeys, type CellChange, type RoleKeys } from '@/authz/pluginGrants'
import PermissionMatrix from '@/components/permission/PermissionMatrix.vue'
import PermRiskBadge from '@/components/permission/PermRiskBadge.vue'
import { useGrantMatrix, type CellLock, type MatrixPerm, type MatrixRole } from '@/components/permission/useGrantMatrix'
import { useEditorGrants } from '@/components/permission/useEditorGrants'
import { confirmRiskyGrants } from '@/components/permission/confirmRiskyGrants'
import SettingsSaveBar from '@/components/SettingsSaveBar.vue'
import StickyActionBar from '@/components/StickyActionBar.vue'
import GrantsTab from './GrantsTab.vue'
import { tabOf, type PluginDetail } from '../pluginUtil'

const props = defineProps<{ detail: PluginDetail }>()
const emit = defineEmits<{ (e: 'changed'): void }>()
const { t } = useI18n()
const auth = useAuthStore()

const data = ref<PluginRoleMatrix | null>(null)
const loading = ref(false)
/** The matrix could not be read (not a 404): an error to retry, not "declares none". */
const loadFailed = ref(false)
const saving = ref(false)
const saveMissing = ref('')

/** The catalog's items as matrix rows: their keys and defaults are full already. */
const perms = computed<MatrixPerm[]>(() => (data.value?.module.permissions ?? []).map((p) => ({ ...p, action: p.key, local: localOf(p.key) })))
const roles = computed(() => data.value?.roles ?? [])
const baseline = computed<RoleKeys>(() => Object.fromEntries(roles.value.map((r) => [r.key, r.permission_keys])))
const editable = computed(() => new Set(roles.value.filter((r) => r.editable).map((r) => r.key)))
const removed = (action: string) => perms.value.some((p) => p.action === action && p.status === 'removed')
/** What the editor may grant: its keys in the grant view, a disabled plugin's included (the server's check). */
const editor = useEditorGrants(() => roles.value, removed)
const lockOf = (p: MatrixPerm, r: MatrixRole): CellLock => {
  if (r.superuser) return 'superuser'
  if (!editable.value.has(r.key)) return 'role_readonly'
  if (p.status === 'removed') return 'removed'
  return editor.holdsAction(p.action) ? null : 'not_held'
}
const matrix = useGrantMatrix({ perms, roles, baseline, lockOf, scopeLimit: editor.scopes, readOnly: computed(() => !auth.has('role:define')) })

const roleName = (key: string) => {
  const r = roles.value.find((x) => x.key === key)
  return r ? lt(r.name) || key : key
}
const permOf = (action: string) => perms.value.find((p) => p.action === action)

// ------------------------------------------------------------------ unsaved changes

const RISK_ORDER = { critical: 0, sensitive: 1, low: 2 } as const
/** The unsaved cells, the sensitive and critical ones first. */
const changes = computed(() =>
  matrix.changes.value
    .flatMap((c) => {
      const perm = permOf(c.action)
      return perm ? [{ ...c, perm }] : []
    })
    .sort((a, b) => RISK_ORDER[a.perm.risk] - RISK_ORDER[b.perm.risk])
)
/** One cell's grant in words: granted or not, every row, the own rows, N chosen. */
function cellText(p: MatrixPerm, keys: string[]): string {
  if (!p.scope) return t(keys.length ? 'plugins.matrix.granted' : 'plugins.matrix.notGranted')
  const s = cellSummary(keys, p.action)
  if (s.choice === 'custom') return t('plugins.matrix.scopeCount', { n: s.refs })
  return t({ none: 'plugins.matrix.notGranted', all: 'plugins.matrix.scopeAll', own: 'plugins.matrix.scopeOwn' }[s.choice])
}
/** "Operator · Edit flows: Not granted → 2 chosen". */
const changeText = (c: CellChange & { perm: MatrixPerm }) =>
  t('plugins.matrix.changeLine', { role: roleName(c.role), perm: lt(c.perm.label) || c.perm.local, from: cellText(c.perm, c.from), to: cellText(c.perm, c.to) })

// ------------------------------------------------------------------ load / save

/** Reads the matrix; false when that failed. A failed read again keeps what is shown, unsaved cells included. */
async function load(): Promise<boolean> {
  loading.value = true
  loadFailed.value = false
  try {
    data.value = await api.get<PluginRoleMatrix>(`/permissions/plugins/${encodeURIComponent(props.detail.key)}`)
    return true
  } catch (e) {
    const gone = isApiError(e) && e.status === 404
    if (gone || !data.value) data.value = null
    loadFailed.value = !gone && !data.value
    if (!gone) notifyError(e)
    return false
  } finally {
    loading.value = false
  }
}

/** "Operator · Edit flows, Admin · View rules": the cells of a merge to tell. */
const cellList = (cells: CellChange[]) => cells.map((c) => `${roleName(c.role)} · ${lt(permOf(c.action)?.label) || localOf(c.action)}`).join(t('roles.listSep'))
// A reload that a new lock refused some edits on says which.
watch(
  () => matrix.replayed.value,
  (r) => {
    if (r?.dropped.length) toast(t('plugins.matrix.editsDropped', { n: r.dropped.length, list: cellList(r.dropped) }, r.dropped.length), 'warning')
  }
)

/**
 * Sends the changed roles only, each with all its keys of the plugin (the
 * PUT replaces just the roles listed). resent: the same save again after a
 * 409 that left these roles as they were (asked about already, not again).
 */
async function save(resent = false) {
  const changed = [...new Set(matrix.changes.value.map((c) => c.role))]
  if (!changed.length) return
  if (!resent && !(await confirmRiskyGrants(newRiskyGrants(baseline.value, matrix.value.value, perms.value), (a) => permOf(a)?.label, roleName))) return
  saving.value = true
  saveMissing.value = ''
  try {
    const version = data.value?.version
    const body = { roles: Object.fromEntries(changed.map((k) => [k, matrix.value.value[k] ?? []])), ...(version !== undefined && { version }) }
    data.value = await api.put<PluginRoleMatrix>(`/permissions/plugins/${encodeURIComponent(props.detail.key)}`, body)
    toast(t('plugins.matrix.saved'), 'success')
    if (changed.some((k) => auth.me?.roles.includes(k))) auth.refreshPermissions().catch(() => undefined)
  } catch (e) {
    if (isVersionConflict(e)) {
      await conflict(changed, resent)
      return
    }
    saveMissing.value = missingText(e, t, roleName)
    if (!saveMissing.value) notifyError(e)
  } finally {
    saving.value = false
  }
}

/** A 409: reads again (the edits stay on top); sends again at once when the roles saved are as they were. */
async function conflict(changed: string[], resent: boolean) {
  const before = baseline.value
  if (!(await load())) return
  const merged = matrix.replayed.value
  const untouched = changed.every((k) => sameKeys(before[k] ?? [], baseline.value[k] ?? []))
  if (!resent && untouched && !merged?.dropped.length) return save(true)
  const clashes = merged?.clashes ?? []
  toast(clashes.length ? t('roles.conflictClashes', { list: cellList(clashes) }) : t('roles.conflictMerged'), 'info')
}

function reset() {
  saveMissing.value = ''
  matrix.reset()
}

onMounted(() => {
  if (auth.has('role:read')) load()
})
watch(
  () => props.detail,
  () => {
    if (auth.has('role:read')) load()
  }
)

// The tab and its edits go away when the page leaves it: another tab,
// another plugin (the same page), or another page; asked first.
onBeforeRouteUpdate((to, from) => to.params.key === from.params.key && tabOf(to.query.tab) === 'permissions' ? true : confirmDiscard(matrix.dirty.value))
onBeforeRouteLeave(() => confirmDiscard(matrix.dirty.value))
</script>

<template>
  <div class="space-y-4">
    <SCard :title="t('plugins.permissions.hostTitle')" :subtitle="t('plugins.permissions.hostHint')" :padded="false" data-testid="plugin-host-permissions">
      <GrantsTab :detail="detail" @changed="emit('changed')" />
    </SCard>

    <SCard v-if="auth.has('role:read')" :title="t('plugins.permissions.userTitle')" :subtitle="t('plugins.permissions.userHint')" data-testid="plugin-user-permissions">
      <div class="space-y-3">
        <SAlert v-if="detail.status === 'disabled'" tone="info" dense>{{ t('plugins.permissions.disabledKept') }}</SAlert>
        <SEmpty v-if="loadFailed" size="sm" icon="warning" :text="t('common.loadFailed')" data-testid="matrix-load-failed">
          <SButton size="sm" @click="load">{{ t('common.refresh') }}</SButton>
        </SEmpty>
        <SEmpty v-else-if="!loading && !perms.length" size="sm" :text="t('plugins.matrix.none')" />
        <PermissionMatrix v-else-if="perms.length" :matrix="matrix" :groups="data?.module.groups" :plugin-key="detail.key" :fold-after="5" />
        <SAlert v-if="saveMissing" tone="danger" dense data-testid="matrix-save-missing">{{ saveMissing }}</SAlert>
        <StickyActionBar v-if="changes.length" data-testid="matrix-save">
          <div class="flex flex-wrap items-center gap-x-2 gap-y-1">
            <span>{{ t('plugins.matrix.unsaved', { n: changes.length }, changes.length) }}</span>
            <PermRiskBadge v-if="changes[0].perm.risk !== 'low'" :risk="changes[0].perm.risk" />
            <SHint inline size="xs" data-testid="matrix-save-first">{{ changeText(changes[0]) }}</SHint>
          </div>
          <SDisclosure v-if="changes.length > 1" class="mt-1" :label="t('plugins.matrix.showChanges', { n: changes.length }, changes.length)" data-testid="matrix-save-all">
            <ul class="space-y-0.5 text-xs">
              <li v-for="c in changes" :key="`${c.role}/${c.action}`" class="flex flex-wrap items-center gap-1.5">
                <PermRiskBadge v-if="c.perm.risk !== 'low'" :risk="c.perm.risk" />
                <span>{{ changeText(c) }}</span>
              </li>
            </ul>
          </SDisclosure>
          <template #actions><SettingsSaveBar :dirty="true" :saving="saving" @save="save()" @reset="reset" /></template>
        </StickyActionBar>
      </div>
    </SCard>
  </div>
</template>
