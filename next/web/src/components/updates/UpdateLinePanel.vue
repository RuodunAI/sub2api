<script setup lang="ts">
// One version line (core or gateway) of the update check in the version
// modal: versions, check state, release notes and the one-click update.
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { SAlert, SBadge, SButton, SDescItem, SDescriptions, SHint, SLink, type Tone } from '@sub2api/ui'
import { formatDateTime } from '@/utils/format'
import type { CoreCheck, GatewayCheck, LineStatus, UpdateLine } from '@/stores/updates'

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

const STATUS_TONE: Record<LineStatus, Tone> = { unchecked: 'gray', disabled: 'gray', failed: 'danger', available: 'warning', blocked: 'warning', upToDate: 'success' }
const gateway = computed(() => (props.line === 'gateway' ? (props.check as GatewayCheck | undefined) : undefined))
const current = computed(() => {
  if (!gateway.value) return (props.check as CoreCheck | undefined)?.current_version || ''
  const versions = new Set((gateway.value.nodes || []).map(n => n.version || t('coreUpdates.unknown')))
  return [...versions].sort().join(', ')
})
const page = computed(() => {
  try {
    const url = new URL(props.check?.page || '')
    return url.protocol === 'https:' ? url.href : ''
  } catch { return '' }
})
const when = (v?: string) => (v ? formatDateTime(v) : '—')
const blocked = computed(() => !!props.check && (!!props.check.reason || !props.check.compatible))
const ready = computed(() => props.canApply && props.supported && props.status === 'available' && !blocked.value)
</script>

<template>
  <section class="space-y-3 rounded-lg border border-line p-3 sm:p-4" :data-testid="`update-line-${line}`">
    <div class="flex flex-wrap items-center justify-between gap-2">
      <h3 class="text-sm font-semibold text-fg">{{ t(`coreUpdates.lines.${line}`) }}</h3>
      <SBadge :tone="STATUS_TONE[status]" data-testid="update-line-status">{{ t(`coreUpdates.status.${status}`) }}</SBadge>
    </div>
    <SDescriptions dense label-width="6rem">
      <SDescItem :label="t('coreUpdates.current')" :value="current || '—'" mono />
      <SDescItem :label="t('coreUpdates.latest')" :value="check?.latest_version || '—'" mono />
      <SDescItem :label="t('coreUpdates.checked')" :value="when(check?.checked_at)" />
      <SDescItem v-if="check?.error" :label="t('coreUpdates.lastSuccess')" :value="when(check.last_success_at)" />
      <SDescItem v-if="gateway?.image" :label="t('coreUpdates.image')" :value="gateway.image" mono copyable />
    </SDescriptions>
    <SAlert v-if="check?.error" tone="danger" dense><span class="break-words">{{ check.error }}</span></SAlert>
    <SHint v-if="check?.latest_version && blocked" tone="warning">{{ check.reason || t('coreUpdates.incompatible') }}</SHint>
    <div v-if="check?.notes" class="max-h-40 overflow-y-auto whitespace-pre-wrap break-words rounded-lg bg-surface-2 p-3 text-sm">{{ check.notes }}</div>
    <div class="flex flex-wrap items-center justify-between gap-2">
      <SLink v-if="page" :href="page" external class="text-sm">{{ t('coreUpdates.releasePage') }} ↗</SLink>
      <span v-else />
      <SButton v-if="ready" size="sm" variant="primary" :loading="applying" data-testid="update-apply" @click="emit('apply')">{{ t('coreUpdates.apply') }}</SButton>
    </div>
    <SHint v-if="!supported && status === 'available'" size="xs" data-testid="update-manual">{{ t('coreUpdates.gatewayManual') }}</SHint>
  </section>
</template>
