<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRouter, type RouteLocationRaw } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { api, isApiError } from '@sub2api/host'
import { confirm, toast, SAlert, SButton, SHint, SModal, SStatusDot } from '@sub2api/ui'
import { useAuthStore } from '@/stores/auth'
import { lineStatus, UPDATE_LINES, useUpdatesStore, type UpdateIssue, type UpdateLine } from '@/stores/updates'
import { notifyError } from '@/utils/errors'
import { useIssueText } from '@/views/upgrades/upgrades'
import UpdateLinePanel from './updates/UpdateLinePanel.vue'

defineProps<{ compact?: boolean }>()
const { t } = useI18n()
const issueText = useIssueText()
const auth = useAuthStore(), updates = useUpdatesStore(), router = useRouter()
const open = ref(false), applying = ref<UpdateLine | ''>(''), refreshing = ref(false)
const basic = ref<{ version: string; managed: boolean } | null>(null)
const allowed = computed(() => auth.has('system:update:read') && basic.value?.managed === true)
const version = computed(() => (basic.value?.version || updates.result?.core.current_version)?.replace(/^v/, ''))
async function loadVersion() { try { basic.value = await api.get('/system/version') } catch { /* version unavailable remains explicitly unknown */ } }
onMounted(loadVersion)
/** The server may have been updated since the page loaded: refetch before showing or applying. */
async function refresh() {
  refreshing.value = true
  try { await Promise.all([loadVersion(), updates.check()]) } finally { refreshing.value = false }
}
function show() {
  open.value = true
  void refresh()
}
const status = (line: UpdateLine) => lineStatus(updates.result, line, !!updates.error)
const summary = computed(() => UPDATE_LINES.map(line => `${t(`coreUpdates.lines.${line}`)}: ${t(`coreUpdates.status.${status(line)}`)}`).join(' · '))
const supported = (line: UpdateLine) => line === 'core' || !!updates.result?.gateway.self_update
const versionText = computed(() => (version.value ? `v${version.value}` : '—'))
function go(to: RouteLocationRaw) {
  open.value = false
  void router.push(to)
}
watch(allowed, can => { if (can) void updates.check(); else { open.value = false; updates.invalidate() } }, { immediate: true })

async function apply(line: UpdateLine) {
  if (applying.value) return
  applying.value = line
  try {
    await refresh()
    const target = updates.result?.[line].latest_version
    if (status(line) !== 'available' || !target) return toast(`${t(`coreUpdates.lines.${line}`)}: ${t(`coreUpdates.status.${status(line)}`)}`)
    let result = await updates.apply(line, target)
    if (result.status === 'needs_confirmation') {
      const reasons = (result.reason_codes || []).map(r => `• ${issueText(r)}`).join('\n')
      const message = t('coreUpdates.applyConfirm', { line: t(`coreUpdates.lines.${line}`), version: target, reasons })
      if (!(await confirm({ title: t('coreUpdates.applyConfirmTitle'), message, confirmText: t('coreUpdates.applyAnyway'), danger: true }))) return
      result = await updates.apply(line, target, true)
    }
    if (result.status === 'started' && result.plan_id) go({ path: '/system/upgrades', query: { upgrade: result.plan_id } })
  } catch (e) { failed(e) } finally { applying.value = '' }
}

/** A refusal by the preflight lists its blockers, worded like the reasons. */
function failed(e: unknown) {
  if (!isApiError(e) || e.code !== 'preflight_blocked') return notifyError(e)
  const blockers: UpdateIssue[] = e.details.blocker_codes || []
  toast(t('coreUpdates.applyBlocked', { blockers: blockers.map(issueText).join(' · ') }), 'error')
}
</script>

<template>
  <template v-if="allowed">
    <button
      type="button"
      class="inline-flex max-w-full items-center gap-1 rounded-md px-1.5 py-0.5 text-xxs font-medium hover:bg-surface-2"
      :class="updates.available ? 'text-warning-600 dark:text-warning-400' : 'text-fg-muted'"
      :title="`${t('coreUpdates.currentCore')}: ${version || '—'} · ${summary}`"
      :aria-label="`${t('coreUpdates.title')} · ${summary}`"
      data-testid="version-badge"
      @click="show"
    >
      <span class="truncate">{{ version || compact ? versionText : t('coreUpdates.title') }}</span>
      <SStatusDot v-if="updates.available" tone="warning" data-testid="version-badge-dot" />
    </button>
    <SModal v-model:open="open" :title="t('coreUpdates.title')">
      <div class="space-y-4">
        <SAlert v-if="updates.error" tone="danger" dense><span class="break-words">{{ updates.error }}</span></SAlert>
        <SAlert v-else-if="updates.result && !updates.result.enabled" tone="neutral" dense>{{ t('coreUpdates.status.disabled') }}</SAlert>
        <UpdateLinePanel
          v-for="line in UPDATE_LINES"
          :key="line"
          :line="line"
          :check="updates.result?.[line]"
          :status="status(line)"
          :can-apply="auth.has('system:update:execute') && !updates.busy && !refreshing"
          :supported="supported(line)"
          :applying="applying === line"
          @apply="apply(line)"
        />
        <SHint v-if="auth.has('system:update:execute')" size="xs">{{ t('coreUpdates.applyHint') }}</SHint>
        <div class="flex flex-wrap justify-end gap-2">
          <SButton v-if="auth.has('settings:read')" size="sm" @click="go({ path: '/settings', query: { tab: 'updates' } })">{{ t('coreUpdates.configure') }}</SButton>
          <SButton size="sm" @click="go('/system/upgrades')">{{ t('coreUpdates.openUpgrades') }}</SButton>
          <SButton size="sm" :loading="updates.busy" :disabled="!!applying" data-testid="update-check" @click="updates.check(true)">{{ t('coreUpdates.check') }}</SButton>
        </div>
      </div>
    </SModal>
  </template>
  <span v-else class="inline-block max-w-full truncate px-1.5 text-xxs text-fg-muted" :title="t('coreUpdates.currentCore')">{{ versionText }}</span>
</template>
