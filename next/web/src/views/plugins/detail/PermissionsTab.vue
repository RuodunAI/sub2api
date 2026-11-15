<script setup lang="ts">
// A plugin's two kinds of permissions: the access it requests (granted to the
// plugin itself: GrantsTab, revoke / narrow / grant again), and the
// permissions it provides to users × roles (the Roles page's data seen by
// plugin: GET/PUT /permissions/plugins/:plugin, the shared PermissionMatrix).
// A plugin declaring none has no module there (404): the matrix says so.
// Unsaved matrix changes are reported up (`dirty`) for the page to confirm
// before they are lost.
import { computed, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { api, isApiError } from '@sub2api/host'
import { SAlert, SCard, SEmpty, SHint, toast } from '@sub2api/ui'
import type { PermissionItem, PluginRoleMatrix } from '@/api/types'
import { lt } from '@/i18n'
import { useAuthStore } from '@/stores/auth'
import { notifyError } from '@/utils/errors'
import { missingText } from '@/utils/permErrors'
import type { RoleKeys } from '@/authz/pluginGrants'
import PermissionMatrix from '@/components/permission/PermissionMatrix.vue'
import { useGrantMatrix, type CellLock, type MatrixPerm, type MatrixRole } from '@/components/permission/useGrantMatrix'
import SettingsSaveBar from '@/components/SettingsSaveBar.vue'
import GrantsTab from './GrantsTab.vue'
import type { PluginDetail } from '../pluginUtil'

const props = defineProps<{ detail: PluginDetail }>()
const emit = defineEmits<{ (e: 'changed'): void; (e: 'dirty', v: boolean): void }>()
const { t } = useI18n()
const auth = useAuthStore()

const data = ref<PluginRoleMatrix | null>(null)
const loading = ref(false)
const saving = ref(false)
const saveMissing = ref('')

/** A catalog item as a matrix row: its key and defaults are full already. */
const toMatrixPerm = (p: PermissionItem): MatrixPerm => ({
  action: p.key,
  local: p.key.slice(p.key.indexOf(':') + 1),
  label: p.label,
  description: p.description,
  risk: p.risk,
  group: p.group,
  requires: p.requires,
  scope: p.scope,
  defaults: p.defaults,
  status: p.status
})
const perms = computed(() => (data.value?.module.permissions ?? []).map(toMatrixPerm))
const roles = computed(() => data.value?.roles ?? [])
const baseline = computed<RoleKeys>(() => Object.fromEntries(roles.value.map((r) => [r.key, r.permission_keys])))
const editable = computed(() => new Set(roles.value.filter((r) => r.editable).map((r) => r.key)))
const lockOf = (p: MatrixPerm, r: MatrixRole): CellLock => {
  if (r.superuser) return 'superuser'
  if (!editable.value.has(r.key)) return 'role_readonly'
  if (p.status === 'removed') return 'removed'
  return auth.hasAction(p.action) ? null : 'not_held'
}
const matrix = useGrantMatrix({ perms, roles, baseline, lockOf })
watch(matrix.dirty, (v) => emit('dirty', v))

/** The first unsaved change, "Operator · Edit flows", next to the count. */
const firstChange = computed(() => {
  const c = matrix.changes.value[0]
  const role = c && roles.value.find((r) => r.key === c.role)
  const perm = c && perms.value.find((p) => p.action === c.action)
  return role && perm ? `${lt(role.name) || role.key} · ${lt(perm.label) || perm.local}` : ''
})

async function load() {
  loading.value = true
  try {
    data.value = await api.get<PluginRoleMatrix>(`/permissions/plugins/${encodeURIComponent(props.detail.key)}`)
  } catch (e) {
    data.value = null
    if (!isApiError(e) || e.status !== 404) notifyError(e)
  } finally {
    loading.value = false
  }
}

/** Sends the changed roles only, each with all its keys of the plugin (the PUT replaces just the roles listed). */
async function save() {
  const changed = [...new Set(matrix.changes.value.map((c) => c.role))]
  saving.value = true
  saveMissing.value = ''
  try {
    const body = { roles: Object.fromEntries(changed.map((k) => [k, matrix.value.value[k] ?? []])) }
    data.value = await api.put<PluginRoleMatrix>(`/permissions/plugins/${encodeURIComponent(props.detail.key)}`, body)
    toast(t('plugins.matrix.saved'), 'success')
    if (changed.some((k) => auth.me?.roles.includes(k))) auth.refreshPermissions().catch(() => undefined)
  } catch (e) {
    saveMissing.value = missingText(e, t)
    if (!saveMissing.value) notifyError(e)
  } finally {
    saving.value = false
  }
}

function reset() {
  saveMissing.value = ''
  matrix.reset()
}

onMounted(() => {
  if (auth.has('role:read')) load()
})
</script>

<template>
  <div class="space-y-4">
    <SCard :title="t('plugins.permissions.hostTitle')" :subtitle="t('plugins.permissions.hostHint')" :padded="false" data-testid="plugin-host-permissions">
      <GrantsTab :detail="detail" @changed="emit('changed')" />
    </SCard>

    <SCard v-if="auth.has('role:read')" :title="t('plugins.permissions.userTitle')" :subtitle="t('plugins.permissions.userHint')" data-testid="plugin-user-permissions">
      <div class="space-y-3">
        <SAlert v-if="detail.status === 'disabled'" tone="info" dense>{{ t('plugins.permissions.disabledKept') }}</SAlert>
        <SEmpty v-if="!loading && !perms.length" size="sm" :text="t('plugins.matrix.none')" />
        <PermissionMatrix
          v-else-if="perms.length"
          :matrix="matrix"
          :perms="perms"
          :groups="data?.module.groups"
          :roles="roles"
          :plugin-key="detail.key"
          :lock-of="lockOf"
          :scope-limit="auth.scopes"
          :fold-after="5"
          :read-only="!auth.has('role:define')"
        />
        <SAlert v-if="saveMissing" tone="danger" dense data-testid="matrix-save-missing">{{ saveMissing }}</SAlert>
        <div
          v-if="matrix.dirty.value"
          class="sticky bottom-0 z-10 flex flex-wrap items-center justify-between gap-3 rounded-xl border border-line bg-surface/95 p-3 shadow-lg backdrop-blur"
          data-testid="matrix-save"
        >
          <div class="min-w-0 text-sm text-fg">
            {{ t('plugins.matrix.unsaved', { n: matrix.changes.value.length }) }}
            <SHint v-if="firstChange" inline size="xs" class="ml-1">{{ firstChange }}</SHint>
          </div>
          <SettingsSaveBar :dirty="true" :saving="saving" @save="save" @reset="reset" />
        </div>
      </div>
    </SCard>
  </div>
</template>
