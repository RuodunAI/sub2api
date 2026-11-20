<script setup lang="ts">
// The tables outside its own a plugin asks for (manifest database.access):
// table, columns (all of them when none is listed) and privileges, one entry
// each, and a warning when any privilege but select is asked for. Shown by
// the consent page under db.core_read and by the plugin's database tab.
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { SAlert, SBadge, SHint } from '@sub2api/ui'
import type { DatabaseAccess } from '@/api/types'
import { useOptionalT } from '@/composables/optionalText'
import { asArray } from '../pluginUtil'
import { useConsentLabels } from '../consent/hostPermission'

const props = defineProps<{ access: DatabaseAccess[] }>()
const { t } = useI18n()
const tr = useOptionalT()
const { colon } = useConsentLabels()

const privilegeLabel = (p: string) => tr(`plugins.database.privilege.${p}`, undefined, p)
/** Any privilege but select: it writes or ties core rows to the plugin's. */
const writes = computed(() => props.access.some((a) => a.privileges.some((p) => p !== 'select')))
</script>

<template>
  <div data-testid="database-access">
    <ul class="divide-y divide-line rounded-lg border border-line">
      <li v-for="a in access" :key="a.table" class="px-3 py-2" :data-table="a.table">
        <div class="flex flex-wrap items-center gap-1.5">
          <code class="mr-1 break-all font-mono text-sm font-medium">{{ a.table }}</code>
          <SBadge v-for="p in a.privileges" :key="p" :tone="p === 'select' ? 'gray' : 'warning'">{{ privilegeLabel(p) }}</SBadge>
        </div>
        <SHint size="xs" class="mt-0.5 break-words">
          {{ t('plugins.database.columns') }}{{ colon }}<span v-if="asArray(a.columns).length" class="font-mono">{{ asArray(a.columns).join(', ') }}</span
          ><template v-else>{{ t('plugins.database.allColumns') }}</template>
        </SHint>
      </li>
    </ul>
    <SAlert v-if="writes" tone="warning" dense class="mt-2" data-testid="database-access-writes">{{ t('plugins.database.writeWarn') }}</SAlert>
  </div>
</template>
