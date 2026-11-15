<script setup lang="ts">
// A list of role grants of a plugin's permissions (CONTRACTS P4C), as the
// plugin page shows those awaiting a super admin's review: each with its
// key (scope included), role, risk and where it comes from (the manifest's
// suggestion, the holders of a renamed key, or the consent's own choice). A
// grant that needs confirmation (a sensitive permission, or a role other
// than admin, operator and supplier) is marked.
import { useI18n } from 'vue-i18n'
import { SBadge, SCard, SHint, SIcon } from '@sub2api/ui'
import type { PluginRoleGrant } from '@/api/types'
import { lt } from '@/i18n'
import { keysFromSuffixes } from '@/authz/pluginGrants'
import { useAssignableRoles } from '@/composables/useAssignableRoles'
import PermRiskBadge from '@/components/permission/PermRiskBadge.vue'

defineProps<{ grants: PluginRoleGrant[]; title: string; hint: string }>()
const { t } = useI18n()
const managed = useAssignableRoles()
const roleName = (key: string) => lt(managed.get(key)?.name) || key
const keyOf = (g: PluginRoleGrant) => keysFromSuffixes(g.permission, [g.scope ?? ''])[0]
const VIA = { default: 'plugins.consent.viaDefault', inherit: 'plugins.consent.viaInherit', override: 'plugins.consent.viaOverride' } as const
</script>

<template>
  <SCard v-if="grants.length" :title="title" data-testid="new-role-grants">
    <SHint size="xs" class="mb-2">{{ hint }}</SHint>
    <ul class="space-y-1 text-sm">
      <li
        v-for="g in grants"
        :key="`${g.version || ''}/${keyOf(g)}/${g.role}`"
        class="flex flex-wrap items-center gap-2 rounded-lg px-2 py-1"
        :class="g.needs_consent ? 'bg-orange-50 dark:bg-orange-900/20' : ''"
        :data-needs-consent="g.needs_consent || undefined"
      >
        <span class="font-mono text-xs">{{ keyOf(g) }}</span>
        <SIcon name="chevron-right" class="h-3 w-3 text-gray-400" />
        <SBadge tone="primary">{{ roleName(g.role) }}</SBadge>
        <PermRiskBadge v-if="g.risk !== 'low'" :risk="g.risk" />
        <SHint inline size="xs">{{ t(VIA[g.via]) }}</SHint>
        <SHint v-if="g.version" inline size="xs" class="font-mono">v{{ g.version }}</SHint>
        <SBadge v-if="g.needs_consent" tone="warning">{{ t('plugins.consent.needsConsent') }}</SBadge>
      </li>
    </ul>
    <slot name="actions" />
  </SCard>
</template>
