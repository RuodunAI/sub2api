<script setup lang="ts">
// The host permissions granted to the plugin: revoke, narrow a list scope
// (NarrowGrantDialog), grant a revoked or denied one again. No card of its
// own: the Permissions tab wraps it in one.
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { api, isApiError } from '@sub2api/host'
import { SButton, SHint, STable, confirm, toast, type TableColumn } from '@sub2api/ui'
import PluginStatusBadge from '../parts/PluginStatusBadge.vue'
import { formatDateTime } from '@/utils/format'
import { notifyError } from '@/utils/errors'
import { useAuthStore } from '@/stores/auth'
import { hpKey, riskOf, type PluginDetail, type PluginGrant } from '../pluginUtil'
import RiskDot from '../parts/RiskDot.vue'
import ScopeChips from '../parts/ScopeChips.vue'
import NarrowGrantDialog from './NarrowGrantDialog.vue'
import { useConsentLabels } from '../consent/hostPermission'

const props = defineProps<{ detail: PluginDetail }>()
const emit = defineEmits<{ (e: 'changed'): void }>()
const { t, te } = useI18n()
const auth = useAuthStore()
const { hpLabel } = useConsentLabels()

const grants = ref<PluginGrant[]>(props.detail.grants || [])
const loading = ref(false)
const revoking = ref('')
/** The permission whose PUT is running (narrow or grant again). */
const setting = ref('')
const narrowing = ref<PluginGrant | null>(null)
const narrowOpen = ref(false)

const canManage = computed(() => auth.has('plugin:manage'))
const notGranted = (g: PluginGrant) => g.status === 'revoked' || g.status === 'denied'
/** Only a list with more than one item can lose one (each keeps at least one); other scope shapes are only revoked. */
const narrowable = (g: PluginGrant) => !notGranted(g) && Object.values(g.scope ?? {}).some((v) => Array.isArray(v) && v.length > 1)

const columns = computed<TableColumn[]>(() => [
  { key: 'permission', label: t('plugins.grants.permission') },
  { key: 'risk', label: t('plugins.grants.risk') },
  { key: 'scope', label: t('plugins.grants.scope') },
  { key: 'status', label: t('common.status') },
  { key: 'granted_by', label: t('plugins.grants.grantedBy') },
  { key: 'granted_at', label: t('plugins.grants.grantedAt') },
  { key: 'actions', label: '', align: 'right' }
])

async function load() {
  loading.value = true
  try {
    const r = await api.get<PluginGrant[] | { items: PluginGrant[] }>(`/plugins/${encodeURIComponent(props.detail.key)}/grants`)
    grants.value = Array.isArray(r) ? r : r?.items || []
  } catch (e) {
    notifyError(e)
  } finally {
    loading.value = false
  }
}

async function revoke(g: PluginGrant) {
  const ok = await confirm({
    title: t('plugins.grants.revokeTitle'),
    message: t('plugins.grants.revokeConfirm', { perm: hpLabel(g.permission) }),
    danger: true,
    confirmText: t('plugins.grants.revoke')
  })
  if (!ok) return
  revoking.value = g.permission
  try {
    await api.del(`/plugins/${encodeURIComponent(props.detail.key)}/grants/${encodeURIComponent(g.permission)}`)
    toast(t('plugins.grants.revoked'), 'success')
    await load()
    emit('changed')
  } catch (e) {
    notifyError(e)
  } finally {
    revoking.value = ''
  }
}

/**
 * PUT /plugins/:key/grants/:permission: {scope} narrows, {} grants what the
 * manifest requests. The step-up it may ask for is the HTTP client's.
 */
async function setGrant(g: PluginGrant, body: { scope?: Record<string, unknown> }, done: string): Promise<boolean> {
  setting.value = g.permission
  try {
    await api.put(`/plugins/${encodeURIComponent(props.detail.key)}/grants/${encodeURIComponent(g.permission)}`, body)
    toast(t(done), 'success')
    await load()
    emit('changed')
    return true
  } catch (e) {
    const refusal = grantRefusal(e)
    if (refusal) toast(refusal, 'error')
    else notifyError(e)
    return false
  } finally {
    setting.value = ''
  }
}

/** The refusals of a grant PUT worded here: a scope wider than requested, a missing plugin:grant:<risk>; '' for the rest. */
function grantRefusal(e: unknown): string {
  if (!isApiError(e)) return ''
  if (e.fieldCodes.scope === 'scope_too_wide') return t('plugins.grants.scopeTooWide')
  const permission = e.details.permission
  return e.code === 'permission_denied' && typeof permission === 'string' ? t('plugins.grants.needsPermission', { permission }) : ''
}

function openNarrow(g: PluginGrant) {
  narrowing.value = g
  narrowOpen.value = true
}

async function narrow(scope: Record<string, unknown>) {
  if (narrowing.value && (await setGrant(narrowing.value, { scope }, 'plugins.grants.narrowed'))) narrowOpen.value = false
}

async function regrant(g: PluginGrant) {
  const ok = await confirm({ title: t('plugins.grants.regrant'), message: t('plugins.grants.regrantConfirm', { perm: hpLabel(g.permission) }), confirmText: t('plugins.grants.regrant') })
  if (ok) await setGrant(g, {}, 'plugins.grants.regranted')
}

onMounted(load)
</script>

<template>
  <div>
    <STable :columns="columns" :rows="grants" :loading="loading" row-key="permission">
      <template #cell-permission="{ row }">
        <div class="font-medium">{{ hpLabel(row.permission) }}</div>
        <SHint size="xs" class="font-mono">{{ row.permission }}</SHint>
        <SHint v-if="te(`plugins.hpDesc.${hpKey(row.permission)}`)" size="xs" class="mt-0.5 max-w-xs">{{ t(`plugins.hpDesc.${hpKey(row.permission)}`) }}</SHint>
      </template>
      <template #cell-risk="{ row }">
        <RiskDot :risk="riskOf(row.permission)" label />
      </template>
      <template #cell-scope="{ row }">
        <ScopeChips v-if="row.scope" :scope="row.scope" />
        <SHint v-else inline>—</SHint>
      </template>
      <template #cell-status="{ row }">
        <PluginStatusBadge :status="row.status" />
      </template>
      <template #cell-granted_by="{ row }">
        {{ row.granted_by_email || row.granted_by || '—' }}
      </template>
      <template #cell-granted_at="{ row }">
        <span class="whitespace-nowrap text-xs">{{ formatDateTime(row.granted_at) }}</span>
      </template>
      <template #cell-actions="{ row }">
        <div v-if="canManage" class="flex justify-end gap-1">
          <SButton v-if="narrowable(row)" size="sm" variant="ghost" :disabled="setting === row.permission" data-testid="grant-narrow" @click="openNarrow(row)">{{
            t('plugins.grants.narrow')
          }}</SButton>
          <SButton v-if="notGranted(row)" size="sm" variant="ghost" :loading="setting === row.permission" data-testid="grant-regrant" @click="regrant(row)">{{
            t('plugins.grants.regrant')
          }}</SButton>
          <SButton v-if="row.status !== 'revoked'" size="sm" variant="ghost" tone="danger" :loading="revoking === row.permission" @click="revoke(row)">{{
            t('plugins.grants.revoke')
          }}</SButton>
        </div>
      </template>
    </STable>
    <NarrowGrantDialog
      v-model:open="narrowOpen"
      :grant="narrowing"
      :label="narrowing ? hpLabel(narrowing.permission) : ''"
      :loading="!!setting"
      @submit="narrow"
    />
  </div>
</template>
