<script setup lang="ts">
// The upgrade matrix's "needs confirmation" column for one permission: its
// grants (review new_role_grants) that the upgrade would not carry over on its
// own (a sensitive permission, or a role other than admin, operator and
// supplier), else that they are granted without confirmation.
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { SBadge, SHint } from '@sub2api/ui'
import type { PluginRoleGrant } from '@/api/types'

const props = defineProps<{ grants: PluginRoleGrant[] }>()
const { t } = useI18n()
const needs = computed(() => props.grants.filter((g) => g.needs_consent))
const sensitive = computed(() => needs.value.some((g) => g.risk !== 'low'))
</script>

<template>
  <SBadge v-if="needs.length" size="xs" tone="warning" data-testid="consent-need">
    {{ t(sensitive ? 'plugins.matrix.needConfirmSensitive' : 'plugins.matrix.needConfirmCustom') }}
  </SBadge>
  <SHint v-else-if="grants.length" inline size="xs">{{ t('plugins.matrix.autoGrant') }}</SHint>
</template>
