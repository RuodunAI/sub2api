<script setup lang="ts">
// The risk of a user permission (low / sensitive / critical). Plugin host
// permissions rank risk on their own four levels (views/plugins/parts/RiskDot),
// so the two never share a badge or its texts.
import { useI18n } from 'vue-i18n'
import { SBadge } from '@sub2api/ui'
import type { PermRisk } from '@/api/types'

defineProps<{ risk: PermRisk }>()
const { t } = useI18n()
const TONE = { low: 'gray', sensitive: 'warning', critical: 'danger' } as const
</script>

<template>
  <SBadge size="xs" :tone="TONE[risk]" :data-risk="risk">{{ t(`roles.risk.${risk}`) }}</SBadge>
</template>
