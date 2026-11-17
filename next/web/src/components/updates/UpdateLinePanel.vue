<script setup lang="ts">
// One version line (core or gateway) of the update check in the version
// modal: versions, check state, release notes and the one-click update.
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { SAlert, SBadge, SButton, SDescItem, SDescriptions, SHint, SLink, type Tone } from '@sub2api/ui'
import { formatDateTime } from '@/utils/format'
import type { CoreCheck, GatewayCheck, LineStatus, UpdateLine } from '@/stores/updates'
import { useLineReason } from './lineReason'

const props = defineProps<{
  line: UpdateLine
  check?: CoreCheck | GatewayCheck
  status: LineStatus
  /** The caller may run the one-click update. */
  canApply: boolean
  /** The line updates itself; otherwise its image is the manual path. */
  supported: boolean
  applying: boolean
}>()
const emit = defineEmits<{ (e: 'apply'): void }>()
const { t } = useI18n()
const lineReason = useLineReason()

const STATUS_TONE: Record<LineStatus, Tone> = { unchecked: 'gray', disabled: 'gray', failed: 'danger', noRelease: 'warning', available: 'warning', blocked: 'warning', upToDate: 'success' }
const gateway = computed(() => (props.line === 'gateway' ? (props.check as GatewayCheck | undefined) : undefined))
const nodes = computed(() => gateway.value?.nodes || [])
/** The core's version, or the versions the gateways report, oldest first. */
const current = computed(() => {
  if (!gateway.value) return (props.check as CoreCheck | undefined)?.current_version || ''
  const versions = new Set(nodes.value.map(n => n.version).filter(Boolean))
  return [...versions].sort((a, b) => a.localeCompare(b, undefined, { numeric: true })).join(', ')
})
/** Gateways too old to report a version, counted apart from the versions. */
const unreported = computed(() => nodes.value.filter(n => !n.version).length)
const page = computed(() => {
  try {
    const url = new URL(props.check?.page || '')
    return url.protocol === 'https:' ? url.href : ''
  } catch { return '' }
})
const when = (v?: string) => (v ? formatDateTime(v) : '—')
/** Why the latest release cannot be applied, or why there is none. */
const blocker = computed(() => lineReason(props.check) || (props.check?.latest_version && !props.check.compatible ? t('coreUpdates.incompatible') : ''))
const ready = computed(() => props.canApply && props.supported && props.status === 'available' && !blocker.value)
</script>

<template>
  <section class="space-y-3 rounded-lg border border-line p-3 sm:p-4" :data-testid="`update-line-${line}`">
    <div class="flex flex-wrap items-center justify-between gap-2">
      <h3 class="text-sm font-semibold text-fg">{{ t(`coreUpdates.lines.${line}`) }}</h3>
      <SBadge :tone="STATUS_TONE[status]" data-testid="update-line-status">{{ t(`coreUpdates.status.${status}`) }}</SBadge>
    </div>
    <SDescriptions dense label-width="6rem">
      <SDescItem :label="t('coreUpdates.current')" :value="current || '—'" mono />
      <SDescItem v-if="unreported" :label="t('coreUpdates.unreported')" :value="t('coreUpdates.nodeCount', { count: unreported, total: nodes.length })" data-testid="update-unreported" />
      <SDescItem :label="t('coreUpdates.latest')" :value="check?.latest_version || '—'" mono />
      <SDescItem :label="t('coreUpdates.checked')" :value="when(check?.checked_at)" />
      <SDescItem v-if="check?.error" :label="t('coreUpdates.lastSuccess')" :value="when(check.last_success_at)" />
      <SDescItem v-if="gateway?.image" :label="t('coreUpdates.image')" :value="gateway.image" mono copyable />
    </SDescriptions>
    <SAlert v-if="check?.error" tone="danger" dense><span class="break-words">{{ check.error }}</span></SAlert>
    <SHint v-if="blocker" tone="warning" data-testid="update-blocker">{{ blocker }}</SHint>
    <div v-if="check?.notes" class="max-h-40 overflow-y-auto whitespace-pre-wrap break-words rounded-lg border border-line bg-surface-2 p-3 text-sm">{{ check.notes }}</div>
    <div class="flex flex-wrap items-center justify-between gap-2">
      <SLink v-if="page" :href="page" external class="text-sm">{{ t('coreUpdates.releasePage') }} ↗</SLink>
      <span v-else />
      <SButton v-if="ready" size="sm" variant="primary" :loading="applying" data-testid="update-apply" @click="emit('apply')">{{ t('coreUpdates.apply') }}</SButton>
    </div>
    <SHint v-if="!supported && status === 'available'" size="xs" data-testid="update-manual">{{ t('coreUpdates.gatewayManual') }}</SHint>
  </section>
</template>
